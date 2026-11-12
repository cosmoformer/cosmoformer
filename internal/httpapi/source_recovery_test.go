//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cosmoformer/cosmoformer/internal/runs"
)

func TestRetargetedPRRevokesAuthorityBeforeDecliningWork(t *testing.T) {
	for _, path := range []string{"background", "manual"} {
		t.Run(path, func(t *testing.T) {
			f := newSourceFixture(t)
			_, stack := f.tracked(t)
			f.enableNative(t)
			ctx := context.Background()
			if err := f.Server.reconcileSource(ctx, f.Org, f.Repo, "pr", 3); err != nil {
				t.Fatal(err)
			}
			var revision int64
			if err := f.DB.Owner.QueryRow(ctx, `SELECT facts_revision FROM cosmoformer.source_subjects WHERE kind='pr'`).Scan(&revision); err != nil {
				t.Fatal(err)
			}
			f.Remote.PRDestination = "release"
			f.Server.Bitbucket.HTTP.Transport = fixtureTransport(func(r *http.Request) (*http.Response, error) {
				if strings.Contains(r.URL.Path, "/src/") {
					t.Error("ineligible PR fetched its manifest")
				}
				return f.Remote.RoundTrip(r)
			})
			if path == "background" {
				if err := f.Server.reconcileSource(ctx, f.Org, f.Repo, "pr", 3); err != nil {
					t.Fatal(err)
				}
			} else {
				f.browser(t, "POST", "/api/v1/organizations/"+f.Org+"/stacks/"+stack+"/plans", map[string]any{"source": map[string]any{"kind": "pr", "pr_number": 3}, "priority": "medium"}, 409)
			}
			var current int64
			var destination string
			if err := f.DB.Owner.QueryRow(ctx, `SELECT facts_revision,facts->>'destination' FROM cosmoformer.source_subjects WHERE kind='pr'`).Scan(&current, &destination); err != nil || current <= revision || destination != "release" {
				t.Fatal("retargeted facts did not revoke prior authority", current, destination, err)
			}
			var count, eligible int
			if err := f.DB.Owner.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE r.source_facts_revision=s.facts_revision) FROM cosmoformer.runs r JOIN cosmoformer.source_subjects s ON s.id=r.source_subject_id WHERE s.kind='pr'`).Scan(&count, &eligible); err != nil || count != 1 || eligible != 0 {
				t.Fatal("old authority or replacement work survived retarget", count, eligible, err)
			}
		})
	}
}

func TestPeriodicSweepRepairsMissedPRChanges(t *testing.T) {
	for _, change := range []string{"updated", "closed"} {
		t.Run(change, func(t *testing.T) {
			f := newSourceFixture(t)
			f.tracked(t)
			f.enableNative(t)
			ctx := context.Background()
			for _, pr := range []int{3, 4} {
				if err := f.Server.reconcileSource(ctx, f.Org, f.Repo, "pr", pr); err != nil {
					t.Fatal(err)
				}
			}
			// Nothing enqueues a webhook job. The tracked branch stays unchanged.
			f.Remote.PRCommit = strings.Repeat("b", 40)
			if change == "closed" {
				f.Remote.PRState = "DECLINED"
			}
			f.Server.sourceJobTick(ctx, f.Org, true)
			for i := 0; i < 8; i++ {
				f.Server.sourceJobTick(ctx, f.Org, false)
			}
			var observed, stale, count, sweeps int
			if err := f.DB.Owner.QueryRow(ctx, `SELECT count(*) FROM cosmoformer.source_subjects WHERE kind='pr' AND facts->>'commit'=$1 AND facts->>'state'=$2`, f.Remote.PRCommit, map[string]string{"updated": "OPEN", "closed": "DECLINED"}[change]).Scan(&observed); err != nil || observed != 2 {
				t.Fatal("sweep did not recover all known PRs", observed, err)
			}
			if err := f.DB.Owner.QueryRow(ctx, `SELECT count(*) FROM cosmoformer.runs r JOIN cosmoformer.source_subjects s ON s.id=r.source_subject_id WHERE s.kind='pr' AND r.source_commit=$1 AND r.source_facts_revision=s.facts_revision`, strings.Repeat("a", 40)).Scan(&stale); err != nil || stale != 0 {
				t.Fatal("missed PR event left stale authority", stale, err)
			}
			want := 4
			if change == "closed" {
				want = 2
			}
			if err := f.DB.Owner.QueryRow(ctx, `SELECT count(*) FROM cosmoformer.runs WHERE purpose='pr_preview'`).Scan(&count); err != nil || count != want {
				t.Fatal("sweep created incorrect replacement work", count, err)
			}
			if err := f.DB.Owner.QueryRow(ctx, `SELECT count(*) FROM cosmoformer.outbox WHERE kind='source_reconcile' AND payload->>'source_kind'='pr'`).Scan(&sweeps); err != nil || sweeps != 2 {
				t.Fatal("PR sweep jobs collided", sweeps, err)
			}
			// Repeated unchanged reads must not produce another preview.
			for _, pr := range []int{3, 4} {
				if err := f.Server.reconcileSource(ctx, f.Org, f.Repo, "pr", pr); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.DB.Owner.QueryRow(ctx, `SELECT count(*) FROM cosmoformer.runs WHERE purpose='pr_preview'`).Scan(&count); err != nil || count != want {
				t.Fatal("unchanged recovery duplicated work", count, err)
			}
		})
	}
}

func TestSourceActionReplayPrecedesSourceReadsAndRevisionChecks(t *testing.T) {
	for _, action := range []string{"sync", "branch plan", "PR plan"} {
		t.Run(action, func(t *testing.T) {
			f := newSourceFixture(t)
			_, stack := f.tracked(t)
			f.enableNative(t)
			ctx := context.Background()
			base := "/api/v1/organizations/" + f.Org
			path := base + "/stacks/" + stack + "/plans"
			body := map[string]any{"source": map[string]any{"kind": "branch"}, "priority": "medium"}
			if action == "sync" {
				f.Remote.Commit = strings.Repeat("b", 40)
				path = base + "/repositories/" + f.Repo + "/sync"
				body = map[string]any{"expected_revision": 2}
			} else if action == "PR plan" {
				body["source"] = map[string]any{"kind": "pr", "pr_number": 3}
			}
			key := runs.ID()
			original := f.browserKey(t, "POST", path, body, key, 200)
			var before []byte
			const snapshot = `SELECT jsonb_build_object('subjects',(SELECT jsonb_agg(to_jsonb(s)) FROM cosmoformer.source_subjects s),'observations',(SELECT count(*) FROM cosmoformer.source_observations),'runs',(SELECT count(*) FROM cosmoformer.runs),'operations',(SELECT count(*) FROM cosmoformer.operations),'audit',(SELECT count(*) FROM cosmoformer.audit_events))`
			if err := f.DB.Owner.QueryRow(ctx, snapshot).Scan(&before); err != nil {
				t.Fatal(err)
			}
			calls := f.Remote.Calls
			for _, unavailable := range []bool{true, false} {
				f.Remote.Commit = strings.Repeat("c", 40)
				f.Remote.PRState = "DECLINED"
				f.Remote.Manifest = []byte("version: unsupported")
				f.Remote.Status = 0
				if unavailable {
					f.Remote.Status = 503
				}
				replayed := f.browserKey(t, "POST", path, body, key, 200)
				var first, second any
				if json.Unmarshal(original.Body.Bytes(), &first) != nil || json.Unmarshal(replayed.Body.Bytes(), &second) != nil {
					t.Fatal("invalid response")
				}
				a, _ := json.Marshal(first)
				b, _ := json.Marshal(second)
				if string(a) != string(b) {
					t.Fatal("replay did not recover the original response")
				}
			}
			var after []byte
			if err := f.DB.Owner.QueryRow(ctx, snapshot).Scan(&after); err != nil || string(after) != string(before) || f.Remote.Calls != calls {
				t.Fatal("replay performed provider I/O or changed source/work records", err)
			}
			// Matching a key never excuses a different request or revoked access.
			changed := map[string]any{"expected_revision": 3}
			if action != "sync" {
				changed = map[string]any{"source": body["source"], "priority": "high"}
			}
			f.browserKey(t, "POST", path, changed, key, 409)
			denied := 403
			if action == "sync" {
				f.exec(t, `UPDATE cosmoformer.accounts SET installation_admin=false WHERE id=$1`, f.Account)
				f.exec(t, `UPDATE cosmoformer.memberships SET org_admin=false WHERE account_id=$1`, f.Account)
				denied = 404
			} else {
				f.exec(t, `DELETE FROM cosmoformer.team_members WHERE account_id=$1`, f.Account)
			}
			f.browserKey(t, "POST", path, body, key, denied)
			if f.Remote.Calls != calls {
				t.Fatal("conflicting or unauthorized replay contacted provider")
			}
		})
	}
}
