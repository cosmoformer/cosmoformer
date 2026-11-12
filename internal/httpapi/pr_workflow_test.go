//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/cosmoformer/cosmoformer/internal/bitbucket"
	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/source"
	"github.com/jackc/pgx/v5"
	"go.yaml.in/yaml/v3"
)

// Only fictional fixtures acquire a compatible assessment. Inventory import
// itself always retains not_assessed, including in production reconciliation.
func (f *sourceFixture) enableNative(t *testing.T) {
	t.Helper()
	if e := f.Server.reconcileSource(context.Background(), f.Org, f.Repo, "branch", 0); e != nil {
		t.Fatal(e)
	}
	f.exec(t, `UPDATE cosmoformer.stacks SET assessment='compatible'`)
	f.exec(t, `UPDATE cosmoformer.aws_connections SET expected_bootstrap_principal='arn:aws:iam::111122223333:role/fixture-bootstrap',session_seconds=3600`)
	f.Server.Evidence = &evidence.Custody{}
}
func (f *sourceFixture) reconcile(t *testing.T, kind string, pr int) {
	t.Helper()
	if e := f.Server.reconcileSource(context.Background(), f.Org, f.Repo, kind, pr); e != nil {
		t.Fatal(e)
	}
}
func (f *sourceFixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if e := f.DB.Owner.QueryRow(context.Background(), query, args...).Scan(&n); e != nil {
		t.Fatal(e)
	}
	return n
}

func TestAutomationRequiresNativeGatesAndKeepsSourceChecksImmutable(t *testing.T) {
	for _, gate := range []string{"compatible", "evidence", "session", "runtime", "pool", "integration", "repository", "grant"} {
		t.Run(gate, func(t *testing.T) {
			f := newSourceFixture(t)
			_, stack := f.tracked(t)
			f.enableNative(t)
			base := "/api/v1/organizations/" + f.Org
			f.browser(t, "POST", base+"/stacks/"+stack+"/plans", map[string]any{"source": map[string]any{"kind": "pr", "pr_number": 3}, "priority": "medium", "intent": "source_check"}, 200)
			f.reconcile(t, "pr", 3)
			if n := f.count(t, `SELECT count(*) FROM cosmoformer.operations WHERE native_planning`); n != 1 {
				t.Fatal("source-only request suppressed native intent", n)
			}
			if n := f.count(t, `SELECT count(*) FROM cosmoformer.audit_events WHERE action='plan.automated' AND actor_id IS NULL AND facts->>'repository_id'=$1`, f.Repo); n != 1 {
				t.Fatal("automation context was not audited", n)
			}
			if n := f.count(t, `SELECT count(*) FROM cosmoformer.operations WHERE NOT native_planning AND status='queued'`); n != 1 {
				t.Fatal("source check changed", n)
			}
			f.reconcile(t, "pr", 3)
			if n := f.count(t, `SELECT count(*) FROM cosmoformer.operations WHERE native_planning`); n != 1 {
				t.Fatal("unchanged sweep duplicated native intent", n)
			}
			switch gate {
			case "compatible":
				f.exec(t, `UPDATE cosmoformer.stacks SET assessment='not_assessed'`)
			case "evidence":
				f.Server.Evidence = nil
			case "session":
				f.exec(t, `UPDATE cosmoformer.aws_connections SET expected_bootstrap_principal=NULL`)
			case "runtime":
				f.exec(t, `UPDATE cosmoformer.runtime_catalog SET withdrawn=true`)
			case "pool":
				f.exec(t, `UPDATE cosmoformer.worker_pools SET enabled=false`)
			case "integration":
				f.exec(t, `UPDATE cosmoformer.integrations SET enabled=false`)
			case "repository":
				f.exec(t, `UPDATE cosmoformer.repositories SET enabled=false`)
			case "grant":
				f.exec(t, `UPDATE cosmoformer.resource_grants SET enabled=false WHERE kind='repository'`)
			}
			f.Remote.PRCommit = strings.Repeat("b", 40)
			_ = f.Server.reconcileSource(context.Background(), f.Org, f.Repo, "pr", 3)
			if n := f.count(t, `SELECT count(*) FROM cosmoformer.operations WHERE native_planning`); n != 1 {
				t.Fatal("revoked gate accepted native intent", n)
			}
			if n := f.count(t, `SELECT count(*) FROM cosmoformer.operations WHERE kind='apply'`); n != 0 {
				t.Fatal("automation applied", n)
			}
		})
	}
}

func TestChangedPathSelectionAndABAHistory(t *testing.T) {
	f := newSourceFixture(t)
	m, _ := catalog.Parse(f.Remote.Manifest)
	m.Stacks[0].Root = "apps/one"
	s := m.Stacks[0]
	s.ID = runs.ID()
	s.Name = "Second stack"
	s.Root = "apps/two"
	m.Stacks = append(m.Stacks, s)
	f.Remote.Manifest, _ = yaml.Marshal(m)
	f.enableNative(t)
	// Retained, successful tracked source inspections prove the complete local
	// closure used for exclusions. A missing closure must select conservatively.
	f.exec(t, `INSERT INTO cosmoformer.source_subjects(org_id,repository_id,kind,external_key,facts) VALUES($1,$2,'branch','fixture','{}')`, f.Org, f.Repo)
	var subject string
	f.DB.Owner.QueryRow(context.Background(), `SELECT id::text FROM cosmoformer.source_subjects WHERE external_key='fixture'`).Scan(&subject)
	f.exec(t, `INSERT INTO cosmoformer.runs(org_id,stack_id,source_subject_id,source_facts_revision,source_commit,purpose,automation_repository_id) SELECT org_id,id,$1,1,$2,'tracked',repository_id FROM cosmoformer.stacks`, subject, f.Remote.Commit)
	f.exec(t, `INSERT INTO cosmoformer.operations(org_id,run_id,stack_id,kind,status,accepted_at,ordering_at,automation_repository_id) SELECT org_id,id,stack_id,'plan','succeeded',clock_timestamp(),clock_timestamp(),automation_repository_id FROM cosmoformer.runs`)
	prep := runs.Prepare{Modules: nil}
	f.exec(t, `INSERT INTO cosmoformer.work_items(org_id,operation_id,phase,status,resolved_preflight) SELECT org_id,id,'preflight','done',$1 FROM cosmoformer.operations`, jsonBytes(prep))
	changed := "apps/one/main.tf"
	f.Server.Bitbucket.HTTP.Transport = fixtureTransport(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/diffstat/") {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"values":[{"new":{"path":"` + changed + `"}}]}`)), Request: r}, nil
		}
		return f.Remote.RoundTrip(r)
	})
	f.Remote.PRCommit = strings.Repeat("b", 40)
	f.reconcile(t, "pr", 3)
	if n := f.count(t, `SELECT count(*) FROM cosmoformer.operations WHERE native_planning`); n != 1 {
		t.Fatal("path did not select one stack", n)
	}
	for i := 0; i < 3; i++ {
		f.reconcile(t, "pr", 3)
	}
	if n := f.count(t, `SELECT count(*) FROM cosmoformer.operations WHERE native_planning`); n != 1 {
		t.Fatal("unchanged path selection drifted", n)
	}
	changed = "docs/unrelated.md"
	f.Remote.PRCommit = strings.Repeat("c", 40)
	f.reconcile(t, "pr", 3)
	if n := f.count(t, `SELECT count(*) FROM cosmoformer.operations WHERE native_planning AND status='queued'`); n != 0 {
		t.Fatal("path skip kept obsolete queued work", n)
	}
	changed = "cosmoformer.yaml"
	f.Remote.PRCommit = strings.Repeat("b", 40)
	f.reconcile(t, "pr", 3)
	if n := f.count(t, `SELECT count(*) FROM cosmoformer.operations WHERE native_planning AND status='queued'`); n != 2 {
		t.Fatal("manifest/reverse observation failed", n)
	}
	if n := f.count(t, `SELECT count(DISTINCT source_facts_revision) FROM cosmoformer.runs WHERE source_commit=$1 AND purpose='pr_preview'`, f.Remote.PRCommit); n != 2 {
		t.Fatal("ABA revived an older source fact", n)
	}
	// Nested local modules outside either root are part of selection even when
	// change_paths does not mention them.
	closure := runs.Prepare{Modules: []source.Module{{Path: "apps/one/shared", Source: "../../modules/shared"}, {Path: "modules/shared/nested", Source: "../nested"}}}
	f.exec(t, `UPDATE cosmoformer.work_items w SET resolved_preflight=$1 FROM cosmoformer.operations o JOIN cosmoformer.stacks s ON s.id=o.stack_id WHERE w.operation_id=o.id AND s.declaration_id=$2`, jsonBytes(closure), m.Stacks[0].ID)
	changed = "modules/nested/main.tf"
	f.Remote.PRCommit = strings.Repeat("d", 40)
	f.reconcile(t, "pr", 3)
	if n := f.count(t, `SELECT count(*) FROM cosmoformer.operations WHERE native_planning AND status='queued'`); n != 1 {
		t.Fatal("nested local module did not select its stack", n)
	}
}

func TestPullRequestPaginationFiltersBeforeGrouping(t *testing.T) {
	f := newSourceFixture(t)
	s, visibleStack := f.tracked(t)
	f.enableNative(t)
	for _, pr := range []int{3, 4, 5} {
		f.reconcile(t, "pr", pr)
	}
	path := "/api/v1/organizations/" + f.Org + "/pull-requests?project_id=" + s.ProjectID + "&limit=1"
	read := func(url string) PullRequestsOutput {
		w := f.browser(t, "GET", url, nil, 200)
		var out PullRequestsOutput
		if e := json.Unmarshal(w.Body.Bytes(), &out.Body); e != nil {
			t.Fatal(e)
		}
		return out
	}
	first := read(path)
	if len(first.Body.PullRequests) != 1 || first.Body.Next == "" {
		t.Fatal("missing head page", first)
	}
	second := read(path + "&before=" + first.Body.Next)
	if len(second.Body.PullRequests) != 1 || second.Body.PullRequests[0].ID == first.Body.PullRequests[0].ID {
		t.Fatal("cursor did not advance")
	}
	// A hundred newer hidden runs in another environment cannot shift the page
	// or supply a cursor even for an organization administrator.
	hiddenEnv, hiddenStack := runs.ID(), runs.ID()
	f.exec(t, `INSERT INTO cosmoformer.environments(id,org_id,project_id,name)VALUES($1,$2,$3,'Hidden environment')`, hiddenEnv, f.Org, s.ProjectID)
	f.exec(t, `INSERT INTO cosmoformer.stacks(id,org_id,project_id,environment_id,repository_id,declaration_id,display_name)VALUES($1,$2,$3,$4,$5,$6,'Hidden stack')`, hiddenStack, f.Org, s.ProjectID, hiddenEnv, f.Repo, runs.ID())
	var hidden string
	e := f.Server.serviceTx(context.Background(), f.Org, func(tx pgx.Tx) error {
		for i := 0; i < 101; i++ {
			hidden = runs.ID()
			if _, e := tx.Exec(context.Background(), `INSERT INTO cosmoformer.runs(id,org_id,stack_id,source_subject_id,source_facts_revision,source_commit,purpose,automation_repository_id) SELECT $1,org_id,$2,id,999,$3,'pr_preview',repository_id FROM cosmoformer.source_subjects WHERE kind='pr' AND external_key='3'`, hidden, hiddenStack, strings.Repeat("f", 40)); e != nil {
				return e
			}
			if _, e := tx.Exec(context.Background(), `INSERT INTO cosmoformer.operations(org_id,run_id,stack_id,kind,status,accepted_at,ordering_at,automation_repository_id)VALUES($1,$2,$3,'plan','succeeded',clock_timestamp(),clock_timestamp(),$4)`, f.Org, hidden, hiddenStack, f.Repo); e != nil {
				return e
			}
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	after := read(path)
	if after.Body.Next != first.Body.Next || len(after.Body.PullRequests[0].Runs) != 1 {
		t.Fatal("hidden runs affected aggregation or pagination")
	}
	if n := len(read(path + "&before=" + hidden).Body.PullRequests); n != 0 {
		t.Fatal("hidden cursor was accepted")
	}
	if n := len(read(path + "&head=" + hidden).Body.PullRequests); n != 0 {
		t.Fatal("hidden head was accepted")
	}

	f.exec(t, `INSERT INTO cosmoformer.runs(org_id,stack_id,source_subject_id,source_facts_revision,source_commit,purpose,automation_repository_id) SELECT ss.org_id,$1,ss.id,ss.facts_revision,ss.facts->>'commit','pr_preview',ss.repository_id FROM cosmoformer.source_subjects ss,generate_series(1,55) WHERE ss.kind='pr' AND ss.external_key='5'`, visibleStack)
	f.exec(t, `INSERT INTO cosmoformer.operations(org_id,run_id,stack_id,kind,status,accepted_at,ordering_at,automation_repository_id) SELECT r.org_id,r.id,r.stack_id,'plan','succeeded',clock_timestamp(),clock_timestamp(),r.automation_repository_id FROM cosmoformer.runs r WHERE r.stack_id=$1 AND NOT EXISTS(SELECT FROM cosmoformer.operations o WHERE o.run_id=r.id)`, visibleStack)
	head := read(path).Body.PullRequests[0]
	if len(head.Runs) != 50 || head.RunsNext == "" {
		t.Fatal("child page was not bounded")
	}
	children := read(path + "&head=" + head.ID + "&run_before=" + head.RunsNext).Body.PullRequests[0]
	if len(children.Runs) != 6 || children.RunsNext != "" {
		t.Fatal("child cursor lost or duplicated visible runs", len(children.Runs))
	}
	if children.Runs[0].Run.ID == head.Runs[0].Run.ID {
		t.Fatal("child cursor repeated its first page")
	}
	if len(read(path + "&head=" + head.ID + "&run_before=" + hidden).Body.PullRequests[0].Runs) != 0 {
		t.Fatal("hidden child cursor was accepted")
	}
	f.exec(t, `DELETE FROM cosmoformer.team_members WHERE account_id=$1`, f.Account)
	if n := len(read(path).Body.PullRequests); n != 0 {
		t.Fatal("revocation retained PR visibility")
	}
}

func TestFeedbackCreateAndUpdateResponseLossNeverDuplicatesCommentOrExecution(t *testing.T) {
	f := newSourceFixture(t)
	f.enableNative(t)
	f.reconcile(t, "pr", 3)
	var job sourceJob
	job.Repo = f.Repo
	if e := f.DB.Owner.QueryRow(context.Background(), `SELECT id::text,payload FROM cosmoformer.outbox WHERE kind='pr_feedback' AND status='pending' ORDER BY created_at DESC LIMIT 1`).Scan(&job.ID, &job.Payload); e != nil {
		t.Fatal(e)
	}
	stored := ""
	creates, updates := 0, 0
	lose := true
	f.Server.Bitbucket.HTTP.Transport = fixtureTransport(func(r *http.Request) (*http.Response, error) {
		raw := `{"values":[]}`
		if r.Method == "GET" && stored != "" {
			b, _ := json.Marshal(map[string]any{"values": []any{map[string]any{"id": 17, "content": map[string]string{"raw": stored}}}})
			raw = string(b)
		}
		if r.Method != "GET" {
			var body struct {
				Content struct {
					Raw string `json:"raw"`
				} `json:"content"`
			}
			if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
				t.Error(e)
			}
			stored = body.Content.Raw
			if r.Method == "POST" {
				creates++
			} else {
				updates++
			}
			if lose {
				lose = false
				return nil, io.ErrUnexpectedEOF
			}
			raw = `{"id":17}`
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(raw)), Request: r}, nil
	})
	before := f.count(t, `SELECT count(*) FROM cosmoformer.operations`)
	if e := f.Server.deliverFeedback(context.Background(), f.Org, job); e == nil {
		t.Fatal("missing create response loss")
	}
	if e := f.Server.deliverFeedback(context.Background(), f.Org, job); e != nil {
		t.Fatal(e)
	}
	lose = true
	if e := f.Server.deliverFeedback(context.Background(), f.Org, job); e == nil {
		t.Fatal("missing update response loss")
	}
	if e := f.Server.deliverFeedback(context.Background(), f.Org, job); e != nil {
		t.Fatal(e)
	}
	if creates != 1 || updates != 3 {
		t.Fatal("unstable comment identity", creates, updates)
	}
	if !strings.Contains(stored, "native workflow") || strings.Contains(stored, "source preflight") || strings.Contains(stored, "page=settings") || !strings.Contains(stored, "?env=") || !strings.Contains(stored, "&view=changes") {
		t.Fatal("untruthful or unscoped feedback", stored)
	}
	if len(stored) > 8192 || f.count(t, `SELECT count(*) FROM cosmoformer.operations`) != before || f.count(t, `SELECT count(*) FROM cosmoformer.approvals`) != 0 {
		t.Fatal("feedback altered infrastructure authority")
	}
}

func TestIncompletePathsCannotAuthorizePRInventoryExpansion(t *testing.T) {
	f := newSourceFixture(t)
	f.enableNative(t)
	m, _ := catalog.Parse(f.Remote.Manifest)
	proposed := m.Stacks[0]
	proposed.ID = runs.ID()
	proposed.Name = "Unauthorized new PR stack"
	m.Stacks = append(m.Stacks, proposed)
	f.Remote.Manifest, _ = yaml.Marshal(m)
	f.reconcile(t, "pr", 3)
	if n := f.count(t, `SELECT count(*) FROM cosmoformer.stacks`); n != 1 {
		t.Fatal("PR expanded tracked inventory", n)
	}
	if n := f.count(t, `SELECT count(*) FROM cosmoformer.operations WHERE native_planning`); n != 1 {
		t.Fatal("incomplete paths failed conservative authorized selection", n)
	}
}

func TestAmbiguousCreateCannotBlindlyPostAgainWhenMarkerIsNotVisible(t *testing.T) {
	f := newSourceFixture(t)
	job := f.feedbackJob(t)
	posts := 0
	f.Server.Bitbucket.HTTP.Transport = fixtureTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == "POST" {
			posts++
			return nil, io.ErrUnexpectedEOF
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"values":[]}`)), Request: r}, nil
	})
	for i := 0; i < 3; i++ {
		if e := f.Server.deliverFeedback(context.Background(), f.Org, job); e == nil {
			t.Fatal("ambiguous comment creation was treated as delivered")
		}
	}
	if posts != 1 {
		t.Fatal("invisible marker caused a duplicate create", posts)
	}
	if n := f.count(t, `SELECT count(*) FROM cosmoformer.feedback_comments WHERE creation_pending AND comment_id=''`); n != 1 {
		t.Fatal("creation ambiguity was not durable")
	}
}

func TestFeedbackRechecksSourceAfterMarkerLookup(t *testing.T) {
	f := newSourceFixture(t)
	job := f.feedbackJob(t)
	writes := 0
	advanced := false
	f.Server.Bitbucket.HTTP.Transport = fixtureTransport(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/comments") {
			if r.Method != "GET" {
				writes++
			}
			if !advanced {
				advanced = true
				f.Remote.PRCommit = strings.Repeat("b", 40)
				f.reconcile(t, "pr", 3)
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"values":[]}`)), Request: r}, nil
		}
		return f.Remote.RoundTrip(r)
	})
	if e := f.Server.deliverFeedback(context.Background(), f.Org, job); e != nil {
		t.Fatal(e)
	}
	if writes != 0 {
		t.Fatal("source changed during lookup but obsolete feedback was written")
	}
}

func TestPredictableCommentMarkerCannotPoisonRecoveryIdentity(t *testing.T) {
	f := newSourceFixture(t)
	job := f.feedbackJob(t)
	oldMarker := "<!-- cosmoformer:" + f.Repo + ":3 -->"
	posts, puts := 0, 0
	posted := ""
	f.Server.Bitbucket.HTTP.Transport = fixtureTransport(func(r *http.Request) (*http.Response, error) {
		if !strings.Contains(r.URL.EscapedPath(), "/repositories/%7B%7D/%7Bfictional-repository%7D/") {
			t.Error("feedback was routed through a mutable slug", r.URL.EscapedPath())
		}
		raw := `{"values":[{"id":51,"content":{"raw":"` + oldMarker + `"}}]}`
		if r.Method == "POST" {
			posts++
			var body struct {
				Content struct {
					Raw string `json:"raw"`
				} `json:"content"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			posted = body.Content.Raw
			raw = `{"id":17}`
		}
		if r.Method == "PUT" {
			puts++
			raw = `{"id":51}`
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(raw)), Request: r}, nil
	})
	if e := f.Server.deliverFeedback(context.Background(), f.Org, job); e != nil {
		t.Fatal(e)
	}
	var nonce, id string
	if e := f.DB.Owner.QueryRow(context.Background(), `SELECT recovery_marker::text,comment_id FROM cosmoformer.feedback_comments WHERE repository_id=$1 AND pr_number=3`, f.Repo).Scan(&nonce, &id); e != nil {
		t.Fatal(e)
	}
	if posts != 1 || puts != 0 || id != "17" || !strings.Contains(posted, nonce) {
		t.Fatal("foreign comment was adopted", posts, puts, id)
	}
	if _, e := f.DB.Owner.Exec(context.Background(), `UPDATE cosmoformer.feedback_comments SET recovery_marker=gen_random_uuid() WHERE repository_id=$1`, f.Repo); e == nil {
		t.Fatal("recovery marker was mutable")
	}
	if _, e := f.DB.Owner.Exec(context.Background(), `UPDATE cosmoformer.feedback_comments SET comment_id='51' WHERE repository_id=$1`, f.Repo); e == nil {
		t.Fatal("known comment identity was mutable")
	}
}

func TestKnownRepositoryReplacementInvalidatesBeforeOldHeadRead(t *testing.T) {
	f := newSourceFixture(t)
	f.enableNative(t)
	f.reconcile(t, "pr", 3)
	f.Server.Bitbucket.HTTP.Transport = fixtureTransport(func(r *http.Request) (*http.Response, error) {
		raw := `{"uuid":"{replacement}"}`
		status := 200
		if strings.Contains(r.URL.Path, "/pullrequests/") {
			t.Error("read an old PR after known repository replacement")
			status = 404
			raw = `{}`
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(raw)), Request: r}, nil
	})
	if e := f.Server.reconcileSource(context.Background(), f.Org, f.Repo, "pr", 3); e != bitbucket.ErrContextChanged {
		t.Fatal("identity mismatch was not preserved", e)
	}
	if n := f.count(t, `SELECT count(*) FROM cosmoformer.operations WHERE native_planning AND status='queued'`); n != 0 {
		t.Fatal("known repository change preserved queued authority")
	}
	if n := f.count(t, `SELECT count(*) FROM cosmoformer.source_subjects WHERE kind='pr' AND facts->>'repository_id'='{replacement}' AND facts->>'commit'=''`); n != 1 {
		t.Fatal("invalidation-only identity facts were not committed")
	}
}
