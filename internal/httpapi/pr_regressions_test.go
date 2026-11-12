//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/store"
	"go.yaml.in/yaml/v3"
)

func (f *sourceFixture) feedbackJob(t *testing.T) sourceJob {
	t.Helper()
	ctx := context.Background()
	if e := f.Server.reconcileSource(ctx, f.Org, f.Repo, "branch", 0); e != nil {
		t.Fatal(e)
	}
	return f.nextPRFeedback(t)
}
func (f *sourceFixture) nextPRFeedback(t *testing.T) sourceJob {
	t.Helper()
	ctx := context.Background()
	if e := f.Server.reconcileSource(ctx, f.Org, f.Repo, "pr", 3); e != nil {
		t.Fatal(e)
	}
	var job sourceJob
	job.Repo = f.Repo
	if e := f.DB.Owner.QueryRow(ctx, `SELECT id::text,payload FROM cosmoformer.outbox WHERE kind='pr_feedback' ORDER BY created_at DESC,id DESC LIMIT 1`).Scan(&job.ID, &job.Payload); e != nil {
		t.Fatal(e)
	}
	return job
}

func TestFeedbackDiscardsSupersededRetries(t *testing.T) {
	f := newSourceFixture(t)
	old := f.feedbackJob(t)
	ctx := context.Background()
	f.Remote.Commit = strings.Repeat("b", 40)
	latest := f.nextPRFeedback(t)
	writes := []string{}
	f.Server.Bitbucket.HTTP.Transport = fixtureTransport(func(r *http.Request) (*http.Response, error) {
		raw := `{"values":[]}`
		if r.Method != "GET" {
			body, e := io.ReadAll(r.Body)
			if e != nil {
				t.Error(e)
			}
			writes = append(writes, string(body))
			raw = `{"id":17}`
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(raw)), Request: r}, nil
	})
	if e := f.Server.deliverFeedback(ctx, f.Org, latest); e != nil {
		t.Fatal(e)
	}
	if e := f.Server.deliverFeedback(ctx, f.Org, old); e != nil {
		t.Fatal(e)
	}
	if len(writes) != 1 || !strings.Contains(writes[0], f.Remote.Commit) {
		t.Fatal("superseded retry overwrote newer feedback", len(writes))
	}
}

func TestFeedbackSerializesConcurrentWritersAndRechecksAfterLock(t *testing.T) {
	f := newSourceFixture(t)
	old := f.feedbackJob(t)
	ctx := context.Background()
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	f.Server.Bitbucket.HTTP.Transport = fixtureTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == "POST" {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":17}`)), Request: r}, nil
		}
		return f.Remote.RoundTrip(r)
	})
	go func() { done <- f.Server.deliverFeedback(ctx, f.Org, old) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("feedback did not reach barrier")
	}
	// Source changes while the earlier write is in flight; another platform
	// instance cannot write newer feedback until that request has completed.
	f.Remote.mu.Lock()
	f.Remote.Commit = strings.Repeat("b", 40)
	f.Remote.mu.Unlock()
	latest := f.nextPRFeedback(t)
	if e := f.Server.deliverFeedback(ctx, f.Org, latest); !errors.Is(e, store.ErrConflict) {
		t.Error("concurrent writer was not deferred", e)
	}
	close(release)
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("feedback did not release its lock")
	}
	// The old write is now complete. New work reuses the comment identity, and
	// another delayed old retry is discarded after obtaining the released lock.
	f.Server.Bitbucket.HTTP.Transport = f.Remote
	if e := f.Server.deliverFeedback(ctx, f.Org, latest); e != nil {
		t.Fatal(e)
	}
	if e := f.Server.deliverFeedback(ctx, f.Org, old); e != nil {
		t.Fatal(e)
	}
	if f.Remote.Updates != 1 {
		t.Fatal("new feedback was not the final writer", f.Remote.Updates)
	}
}

func TestFeedbackQueueHonorsWriteFailureClassification(t *testing.T) {
	for _, status := range []int{403, 429, 503} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			f := newSourceFixture(t)
			job := f.feedbackJob(t)
			ctx := context.Background()
			f.Server.Bitbucket.HTTP.Transport = fixtureTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method == "GET" {
					return f.Remote.RoundTrip(r)
				}
				h := make(http.Header)
				h.Set("Retry-After", "60")
				return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(`{"error":"synthetic"}`)), Request: r}, nil
			})
			f.exec(t, `UPDATE cosmoformer.outbox SET status='delivered' WHERE id<>$1`, job.ID)
			f.Server.sourceJobTick(ctx, f.Org, false)
			var state string
			var next time.Time
			if e := f.DB.Owner.QueryRow(ctx, `SELECT status,next_attempt_at FROM cosmoformer.outbox WHERE id=$1`, job.ID).Scan(&state, &next); e != nil {
				t.Fatal(e)
			}
			want := "pending"
			if status == 403 {
				want = "failed"
			}
			if state != want {
				t.Fatal("write failure misclassified", state)
			}
			if status == 429 && time.Until(next) < 55*time.Second {
				t.Fatal("feedback ignored Retry-After")
			}
		})
	}
}

func TestVisiblePreflightsAreLimitedAfterAuthorization(t *testing.T) {
	f := newSourceFixture(t)
	_, stack := f.tracked(t)
	ctx := context.Background()
	base := "/api/v1/organizations/" + f.Org
	f.browser(t, "POST", base+"/stacks/"+stack+"/plans", map[string]any{"source": map[string]any{"kind": "branch"}, "priority": "medium"}, 200)
	project, env, repo, other, subject, run := runs.ID(), runs.ID(), runs.ID(), runs.ID(), runs.ID(), runs.ID()
	f.exec(t, `INSERT INTO cosmoformer.projects(id,org_id,name)VALUES($1,$2,'Other project')`, project, f.Org)
	f.exec(t, `INSERT INTO cosmoformer.environments(id,org_id,project_id,name)VALUES($1,$2,$3,'Other environment')`, env, f.Org, project)
	f.exec(t, `INSERT INTO cosmoformer.repositories(id,org_id,project_id,integration_id,external_id,url,tracked_branch)VALUES($1,$2,$3,$4,'{other-repository}','https://bitbucket.org/fictional/other.git','main')`, repo, f.Org, project, f.Integration)
	f.exec(t, `INSERT INTO cosmoformer.stacks(id,org_id,project_id,environment_id,repository_id,declaration_id,display_name)VALUES($1,$2,$3,$4,$5,$6,'Hidden stack')`, other, f.Org, project, env, repo, runs.ID())
	f.exec(t, `INSERT INTO cosmoformer.source_subjects(id,org_id,repository_id,kind,external_key,facts)VALUES($1,$2,$3,'branch','main','{}')`, subject, f.Org, repo)
	f.exec(t, `INSERT INTO cosmoformer.runs(id,org_id,stack_id,source_subject_id,source_facts_revision,source_commit,purpose,automation_repository_id)VALUES($1,$2,$3,$4,1,$5,'tracked',$6)`, run, f.Org, other, subject, strings.Repeat("b", 40), repo)
	f.exec(t, `INSERT INTO cosmoformer.operations(org_id,run_id,stack_id,kind,status,accepted_at,ordering_at,created_at,automation_repository_id) SELECT $1,$2,$3,'plan','queued',clock_timestamp(),clock_timestamp(),clock_timestamp()+interval '1 second',$4 FROM generate_series(1,101)`, f.Org, run, other, repo)
	response := f.browser(t, "GET", base+"/preflights", nil, 200)
	var out struct {
		Preflights []PreflightPublic `json:"preflights"`
	}
	if e := json.Unmarshal(response.Body.Bytes(), &out); e != nil {
		t.Fatal(e)
	}
	if len(out.Preflights) != 1 || out.Preflights[0].Stack != "Synthetic stack" || !out.Preflights[0].CanPrioritize {
		t.Fatal("other projects hid visible queued work", out)
	}
	var count int
	if e := f.DB.Owner.QueryRow(ctx, `SELECT count(*) FROM cosmoformer.operations`).Scan(&count); e != nil || count != 102 {
		t.Fatal("fixture did not exceed the history limit", count, e)
	}
}

func TestRejectedSourceProcessingStillCommitsNewFacts(t *testing.T) {
	for _, path := range []string{"background", "sync", "plan"} {
		for _, failure := range []string{"missing manifest", "invalid manifest", "ungranted secret"} {
			t.Run(path+"/"+failure, func(t *testing.T) {
				f := newSourceFixture(t)
				s, stack := f.tracked(t)
				ctx := context.Background()
				base := "/api/v1/organizations/" + f.Org
				f.browser(t, "POST", base+"/stacks/"+stack+"/plans", map[string]any{"source": map[string]any{"kind": "branch"}, "priority": "medium"}, 200)
				var prior int64
				var inventory string
				if e := f.DB.Owner.QueryRow(ctx, `SELECT facts_revision FROM cosmoformer.source_subjects WHERE repository_id=$1 AND kind='branch'`, f.Repo).Scan(&prior); e != nil {
					t.Fatal(e)
				}
				if e := f.DB.Owner.QueryRow(ctx, `SELECT inventory_commit FROM cosmoformer.repositories WHERE id=$1`, f.Repo).Scan(&inventory); e != nil {
					t.Fatal(e)
				}
				f.Remote.Commit = strings.Repeat("b", 40)
				switch failure {
				case "invalid manifest":
					f.Remote.Manifest = []byte("version: unsupported")
				case "missing manifest":
					f.Server.Bitbucket.HTTP.Transport = fixtureTransport(func(r *http.Request) (*http.Response, error) {
						if strings.Contains(r.URL.Path, "/src/") {
							return &http.Response{StatusCode: 404, Header: make(http.Header), Body: http.NoBody, Request: r}, nil
						}
						return f.Remote.RoundTrip(r)
					})
				case "ungranted secret":
					s.Secrets = []catalog.Secret{{Env: "TF_VAR_password", Source: catalog.SecretSource{Scope: "project", ID: runs.ID()}}}
					f.Remote.Manifest, _ = yaml.Marshal(catalog.Manifest{Version: 1, Stacks: []catalog.Stack{s}})
				}
				switch path {
				case "background":
					if e := f.Server.reconcileSource(ctx, f.Org, f.Repo, "branch", 0); e == nil {
						t.Fatal("invalid source was imported")
					}
				case "sync":
					status := 403
					if failure == "invalid manifest" {
						status = 422
					}
					if failure == "missing manifest" {
						status = 503
					}
					f.browser(t, "POST", base+"/repositories/"+f.Repo+"/sync", map[string]any{"expected_revision": 2}, status)
				case "plan":
					f.browser(t, "POST", base+"/stacks/"+stack+"/plans", map[string]any{"source": map[string]any{"kind": "branch"}, "priority": "medium"}, 409)
				}
				var current int64
				var commit, after string
				if e := f.DB.Owner.QueryRow(ctx, `SELECT facts_revision,facts->>'commit' FROM cosmoformer.source_subjects WHERE repository_id=$1 AND kind='branch'`, f.Repo).Scan(&current, &commit); e != nil {
					t.Fatal(e)
				}
				if current <= prior || commit != f.Remote.Commit {
					t.Fatal("new authenticated head was rolled back", current, prior, commit)
				}
				if e := f.DB.Owner.QueryRow(ctx, `SELECT inventory_commit FROM cosmoformer.repositories WHERE id=$1`, f.Repo).Scan(&after); e != nil || after != inventory {
					t.Fatal("failed inventory processing changed the accepted inventory", after, e)
				}
				var stale bool
				if e := f.DB.Owner.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.work_items w JOIN cosmoformer.runs r ON r.id=(SELECT run_id FROM cosmoformer.operations WHERE id=w.operation_id) JOIN cosmoformer.source_subjects s ON s.id=r.source_subject_id WHERE r.source_facts_revision=s.facts_revision AND r.source_commit=s.facts->>'commit')`).Scan(&stale); e != nil || stale {
					t.Fatal("old queued work retained source authority", stale, e)
				}
			})
		}
	}
}

func TestCommittedSourceObservationStillFencesDelayedWorkCreation(t *testing.T) {
	f := newSourceFixture(t)
	f.tracked(t)
	f.enableNative(t)
	ctx := context.Background()
	old := f.Remote.Commit
	newer := strings.Repeat("b", 40)
	interleaved := false
	f.Server.Bitbucket.HTTP.Transport = fixtureTransport(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/src/"+old+"/") && !interleaved {
			interleaved = true
			f.Remote.Commit = newer
			if e := f.Server.reconcileSource(ctx, f.Org, f.Repo, "pr", 3); e != nil {
				t.Fatal("newer observation failed", e)
			}
		}
		return f.Remote.RoundTrip(r)
	})
	if e := f.Server.reconcileSource(ctx, f.Org, f.Repo, "pr", 3); runs.Code(e) != "SOURCE_CHANGED" {
		t.Fatal("delayed source read created work after a newer observation", e)
	}
	var count int
	var commit string
	if e := f.DB.Owner.QueryRow(ctx, `SELECT count(*),min(source_commit) FROM cosmoformer.runs`).Scan(&count, &commit); e != nil || count != 1 || commit != newer {
		t.Fatal("older work survived the source fence", count, commit, e)
	}
}
