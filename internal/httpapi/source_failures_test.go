//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"go.yaml.in/yaml/v3"
)

type fixtureTransport func(*http.Request) (*http.Response, error)

func (f fixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func (f *sourceFixture) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, e := f.DB.Owner.Exec(context.Background(), q, args...); e != nil {
		t.Fatal(e)
	}
}
func (f *sourceFixture) tracked(t *testing.T) (catalog.Stack, string) {
	t.Helper()
	m, e := catalog.Parse(f.Remote.Manifest)
	if e != nil {
		t.Fatal(e)
	}
	s := m.Stacks[0]
	team := runs.ID()
	f.exec(t, `INSERT INTO cosmoformer.teams(id,org_id,name) VALUES($1,$2,'Synthetic planners')`, team, f.Org)
	f.exec(t, `INSERT INTO cosmoformer.team_members(org_id,team_id,account_id) VALUES($1,$2,$3)`, f.Org, team, f.Account)
	f.exec(t, `INSERT INTO cosmoformer.grants(org_id,team_id,project_id,environment_id,role) VALUES($1,$2,$3,$4,'planner')`, f.Org, team, s.ProjectID, s.EnvironmentID)
	w := f.browser(t, "POST", "/api/v1/organizations/"+f.Org+"/repositories/"+f.Repo+"/sync", map[string]any{"expected_revision": 1}, 200)
	var out struct {
		IDs []string `json:"stack_ids"`
	}
	if e := json.Unmarshal(w.Body.Bytes(), &out); e != nil || len(out.IDs) != 1 {
		t.Fatal("missing tracked stack", e)
	}
	return s, out.IDs[0]
}
func TestSourceJobsClassifyFailuresAndReleaseRetryClaims(t *testing.T) {
	for _, tc := range []struct {
		name             string
		status, attempts int
		payload          string
		want             string
	}{
		{"rate limited", 429, 0, `{"source_kind":"branch","pr_number":0}`, "pending"},
		{"unavailable", 503, 0, `{"source_kind":"branch","pr_number":0}`, "pending"},
		{"forbidden", 403, 0, `{"source_kind":"branch","pr_number":0}`, "failed"},
		{"missing manifest", 404, 0, `{"source_kind":"branch","pr_number":0}`, "failed"},
		{"malformed payload", 0, 11, `{"unexpected":true}`, "failed"},
		{"retry limit", 503, 11, `{"source_kind":"branch","pr_number":0}`, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSourceFixture(t)
			ctx := context.Background()
			job := runs.ID()
			f.exec(t, `INSERT INTO cosmoformer.outbox(id,org_id,kind,subject_id,payload,status,attempt_count,next_attempt_at) VALUES($1,$2,'source_reconcile',$3,$4,'pending',$5,clock_timestamp())`, job, f.Org, f.Repo, []byte(tc.payload), tc.attempts)
			f.Remote.Status = tc.status
			if tc.name == "missing manifest" {
				f.Server.Bitbucket.HTTP.Transport = fixtureTransport(func(r *http.Request) (*http.Response, error) {
					if strings.Contains(r.URL.Path, "/src/") {
						return &http.Response{StatusCode: 404, Header: make(http.Header), Body: http.NoBody, Request: r}, nil
					}
					return f.Remote.RoundTrip(r)
				})
			}
			f.Server.sourceJobTick(ctx, f.Org, false)
			var status string
			var attempts int
			var claimed bool
			var next time.Time
			if e := f.DB.Owner.QueryRow(ctx, `SELECT status,attempt_count,claim_id IS NOT NULL,next_attempt_at FROM cosmoformer.outbox WHERE id=$1`, job).Scan(&status, &attempts, &claimed, &next); e != nil || status != tc.want || attempts != tc.attempts+1 || claimed {
				t.Fatal("job classification or claim release wrong", status, attempts, claimed, e)
			}
			if tc.status == 429 && time.Until(next) < 55*time.Second {
				t.Fatal("Retry-After was not honored", next)
			}
			if status == "failed" {
				base := "/api/v1/organizations/" + f.Org + "/source-jobs/" + job + "/retry"
				var rev int64
				if e := f.DB.Owner.QueryRow(ctx, `SELECT revision FROM cosmoformer.outbox WHERE id=$1`, job).Scan(&rev); e != nil {
					t.Fatal(e)
				}
				f.browser(t, "POST", base, map[string]any{"expected_revision": rev - 1}, 409)
				f.browser(t, "POST", base, map[string]any{"expected_revision": rev}, 200)
				if e := f.DB.Owner.QueryRow(ctx, `SELECT status,attempt_count FROM cosmoformer.outbox WHERE id=$1`, job).Scan(&status, &attempts); e != nil || status != "pending" || attempts != 0 {
					t.Fatal("operator retry did not reset job", status, attempts, e)
				}
			}
		})
	}
}

func TestTrackedInventoryAndPlansRejectChangedSource(t *testing.T) {
	f := newSourceFixture(t)
	s, stack := f.tracked(t)
	base := "/api/v1/organizations/" + f.Org
	plan := base + "/stacks/" + stack + "/plans"
	listed := f.browser(t, "GET", base+"/stacks", nil, 200)
	if !strings.Contains(listed.Body.String(), stack) || !strings.Contains(listed.Body.String(), `"can_plan":true`) {
		t.Fatal("planner cannot see tracked stack")
	}
	f.browser(t, "POST", plan, map[string]any{"source": map[string]any{"kind": "branch", "pr_number": 3}, "priority": "medium"}, 422)
	f.browser(t, "POST", plan, map[string]any{"source": map[string]any{"kind": "pr"}, "priority": "medium"}, 422)
	original := append([]byte{}, f.Remote.Manifest...)
	for _, tc := range []struct {
		name   string
		status int
	}{{"malformed", 422}, {"absent", 409}, {"identity changed", 409}, {"inventory advanced", 409}, {"provider unavailable", 503}} {
		t.Run(tc.name, func(t *testing.T) {
			f.Remote.Manifest = append([]byte{}, original...)
			f.Remote.Status = 0
			f.Remote.Commit = strings.Repeat("a", 40)
			switch tc.name {
			case "malformed":
				f.Remote.Manifest = []byte("version: unsupported")
			case "absent":
				copy := s
				copy.ID = runs.ID()
				f.Remote.Manifest, _ = yaml.Marshal(catalog.Manifest{Version: 1, Stacks: []catalog.Stack{copy}})
			case "identity changed":
				copy := s
				copy.Workspace = "other"
				f.Remote.Manifest, _ = yaml.Marshal(catalog.Manifest{Version: 1, Stacks: []catalog.Stack{copy}})
			case "inventory advanced":
				f.Remote.Commit = strings.Repeat("b", 40)
			case "provider unavailable":
				f.Remote.Status = 503
			}
			f.browser(t, "POST", plan, map[string]any{"source": map[string]any{"kind": "branch"}, "priority": "medium"}, tc.status)
		})
	}
	f.Remote.Manifest = original
	f.Remote.Status = 0
	f.Remote.Commit = strings.Repeat("a", 40)
	f.browser(t, "POST", plan, map[string]any{"source": map[string]any{"kind": "pr", "pr_number": 3}, "priority": "high"}, 200)
	f.browser(t, "POST", base+"/repositories/"+f.Repo+"/sync", map[string]any{"expected_revision": 1}, 409)
	// A repository update racing a source read must invalidate that read's result.
	f.Server.Bitbucket.HTTP.Transport = fixtureTransport(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/src/") {
			f.exec(t, `UPDATE cosmoformer.repositories SET revision=revision+1 WHERE id=$1`, f.Repo)
		}
		return f.Remote.RoundTrip(r)
	})
	f.browser(t, "POST", plan, map[string]any{"source": map[string]any{"kind": "branch"}, "priority": "medium"}, 409)
	f.Server.Bitbucket.HTTP.Transport = f.Remote
	f.exec(t, `DELETE FROM cosmoformer.team_members WHERE org_id=$1`, f.Org)
	f.browser(t, "POST", plan, map[string]any{"source": map[string]any{"kind": "branch"}, "priority": "medium"}, 403)
	hidden := f.browser(t, "GET", base+"/stacks", nil, 200)
	if strings.Contains(hidden.Body.String(), stack) {
		t.Fatal("revoked planner retained stack visibility")
	}
}

func TestSourceReconciliationStopsAfterPRClosureAndRejectsIdentityChanges(t *testing.T) {
	f := newSourceFixture(t)
	s, stack := f.tracked(t)
	ctx := context.Background()
	if e := f.Server.reconcileSource(ctx, f.Org, f.Repo, "pr", 3); e != nil {
		t.Fatal(e)
	}
	f.Server.Bitbucket.HTTP.Transport = fixtureTransport(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/pullrequests/") {
			raw := `{"state":"DECLINED","source":{"repository":{"uuid":"{fictional-repository}"},"commit":{"hash":"` + f.Remote.Commit + `"},"branch":{"name":"change"}},"destination":{"repository":{"uuid":"{fictional-repository}"},"branch":{"name":"main"}}}`
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(raw)), Request: r}, nil
		}
		if strings.Contains(r.URL.Path, "/src/") {
			t.Error("closed PR fetched source")
		}
		return f.Remote.RoundTrip(r)
	})
	if e := f.Server.reconcileSource(ctx, f.Org, f.Repo, "pr", 3); e != nil {
		t.Fatal(e)
	}
	var facts []byte
	if e := f.DB.Owner.QueryRow(ctx, `SELECT facts FROM cosmoformer.source_subjects WHERE repository_id=$1 AND kind='pr'`, f.Repo).Scan(&facts); e != nil || !strings.Contains(string(facts), "DECLINED") {
		t.Fatal("closed PR facts not accepted", e)
	}
	f.Server.Bitbucket.HTTP.Transport = f.Remote
	s.Workspace = "other"
	f.Remote.Manifest, _ = yaml.Marshal(catalog.Manifest{Version: 1, Stacks: []catalog.Stack{s}})
	if e := f.Server.reconcileSource(ctx, f.Org, f.Repo, "pr", 3); e != nil {
		t.Fatal(e)
	}
	var count int
	if e := f.DB.Owner.QueryRow(ctx, `SELECT count(*) FROM cosmoformer.runs WHERE stack_id=$1`, stack).Scan(&count); e != nil || count != 1 {
		t.Fatal("untracked PR identity queued work", count, e)
	}
	for _, v := range []struct {
		kind string
		pr   int
	}{{"apply", 0}, {"pr", 0}, {"branch", 3}} {
		if e := f.Server.reconcileSource(ctx, f.Org, f.Repo, v.kind, v.pr); e == nil {
			t.Fatal("unsupported action entered reconciliation")
		}
	}
	s.Workspace = "default"
	f.Remote.Manifest, _ = yaml.Marshal(catalog.Manifest{Version: 1, Stacks: []catalog.Stack{s}})
	job := f.nextPRFeedback(t)
	f.Server.Bitbucket.HTTP.Transport = fixtureTransport(func(*http.Request) (*http.Response, error) { return nil, errors.New("synthetic disconnected provider") })
	if e := f.Server.deliverFeedback(ctx, f.Org, job); e == nil {
		t.Fatal("feedback delivery failure ignored")
	}
}

func TestSourceJobLoopReconcilesAndStops(t *testing.T) {
	f := newSourceFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); f.Server.sourceJobs(ctx) }()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatal("background reconciliation did not produce tracked work")
		case <-ticker.C:
			var count int
			if e := f.DB.Owner.QueryRow(context.Background(), `SELECT count(*) FROM cosmoformer.runs`).Scan(&count); e != nil {
				t.Fatal(e)
			}
			if count == 1 {
				cancel()
				select {
				case <-done:
					return
				case <-time.After(time.Second):
					t.Fatal("source loop did not stop")
				}
			}
		}
	}
}

func TestFeedbackAdoptsCommentAfterAmbiguousPriorDelivery(t *testing.T) {
	f := newSourceFixture(t)
	job := f.feedbackJob(t)
	marker := "<!-- cosmoformer:" + f.Repo + ":3 -->"
	writes := 0
	f.Server.Bitbucket.HTTP.Transport = fixtureTransport(func(r *http.Request) (*http.Response, error) {
		raw := `{"values":[{"id":51,"content":{"raw":"` + marker + `"}}]}`
		if r.Method != "GET" {
			writes++
			if r.Method != "PUT" || !strings.HasSuffix(r.URL.Path, "/comments/51") {
				t.Error("ambiguous delivery created duplicate feedback")
			}
			raw = `{"id":51}`
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(raw)), Request: r}, nil
	})
	if e := f.Server.deliverFeedback(context.Background(), f.Org, job); e != nil {
		t.Fatal(e)
	}
	var id string
	if e := f.DB.Owner.QueryRow(context.Background(), `SELECT comment_id FROM cosmoformer.feedback_comments WHERE repository_id=$1`, f.Repo).Scan(&id); e != nil || id != "51" || writes != 1 {
		t.Fatal("feedback identity not persisted", id, writes, e)
	}
	if e := f.Server.deliverFeedback(context.Background(), f.Org, sourceJob{Repo: f.Repo, Payload: []byte(`{"pr_number":0}`)}); e == nil {
		t.Fatal("invalid feedback job accepted")
	}
}

func TestTrackedInventoryReusesRevisionsAndArchivesRemovedDeclarations(t *testing.T) {
	f := newSourceFixture(t)
	s, old := f.tracked(t)
	ctx := context.Background()
	base := "/api/v1/organizations/" + f.Org + "/repositories/" + f.Repo + "/sync"
	var before string
	if e := f.DB.Owner.QueryRow(ctx, `SELECT effective_revision_id::text FROM cosmoformer.stacks WHERE id=$1`, old).Scan(&before); e != nil {
		t.Fatal(e)
	}
	f.browser(t, "POST", base, map[string]any{"expected_revision": 2}, 200)
	var after string
	if e := f.DB.Owner.QueryRow(ctx, `SELECT effective_revision_id::text FROM cosmoformer.stacks WHERE id=$1`, old).Scan(&after); e != nil || after != before {
		t.Fatal("unchanged inventory manufactured a new source revision", e)
	}
	s.ID = runs.ID()
	s.Name = "Replacement declaration"
	f.Remote.Commit = strings.Repeat("b", 40)
	f.Remote.Manifest, _ = yaml.Marshal(catalog.Manifest{Version: 1, Stacks: []catalog.Stack{s}})
	f.browser(t, "POST", base, map[string]any{"expected_revision": 2}, 200)
	var archived bool
	if e := f.DB.Owner.QueryRow(ctx, `SELECT archived FROM cosmoformer.stacks WHERE id=$1`, old).Scan(&archived); e != nil || !archived {
		t.Fatal("removed declaration remained eligible", e)
	}
	// Git cannot move a declaration into a different project or tracked branch.
	s.ProjectID = runs.ID()
	f.Remote.Manifest, _ = yaml.Marshal(catalog.Manifest{Version: 1, Stacks: []catalog.Stack{s}})
	f.browser(t, "POST", base, map[string]any{"expected_revision": 3}, 403)
}
