//go:build integration

package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/bitbucket"
	"github.com/cosmoformer/cosmoformer/internal/cryptostore"
	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/httpapi"
	"github.com/cosmoformer/cosmoformer/internal/identity"
	"github.com/jackc/pgx/v5"
)

type approvalClock struct {
	start  time.Time
	mu     sync.Mutex
	offset time.Duration
	failed bool
	// A boundary test advances elapsed time just after the first freshness
	// check, while the remaining database references are still being checked.
	advanceAfter int
}

func (c *approvalClock) Now() (time.Duration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failed {
		return 0, errors.New("synthetic clock failure")
	}
	now := time.Second + time.Since(c.start) + c.offset
	if c.advanceAfter > 0 {
		c.advanceAfter--
		if c.advanceAfter == 0 {
			c.offset += 30 * time.Second
		}
	}
	return now, nil
}

type approvalProvider struct {
	base   http.RoundTripper
	mu     sync.Mutex
	calls  int
	outage bool
	after  func()
}

func (p *approvalProvider) RoundTrip(r *http.Request) (*http.Response, error) {
	p.mu.Lock()
	p.calls++
	outage := p.outage
	after := p.after
	p.mu.Unlock()
	if outage {
		return nil, errors.New("synthetic provider outage")
	}
	res, e := p.base.RoundTrip(r)
	if e == nil && after != nil && (strings.Contains(r.URL.Path, "/refs/") || strings.Contains(r.URL.Path, "/pullrequests/")) {
		after()
	}
	return res, e
}

type approvalFixture struct {
	*executionFixture
	voter    *client
	provider *approvalProvider
	clock    *approvalClock
	plan     map[string]any
	path     string
}

func newApprovalFixture(t *testing.T, min int, expiry ...time.Duration) *approvalFixture {
	t.Helper()
	return approvalFixtureWithExecution(t, newExecutionFixtureForSourceProfile(t, map[string]any{"kind": "branch"}, true, min), expiry...)
}
func approvalFixtureWithExecution(t *testing.T, f *executionFixture, expiry ...time.Duration) *approvalFixture {
	t.Helper()
	if len(expiry) > 0 {
		// Bound the ready transition with a short fixture TTL. The production ready
		// identity trigger still enforces that this expiry can never be edited later.
		f.exec(t, `CREATE FUNCTION cosmoformer.fixture_review_expiry() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.status='ready' AND OLD.status='assembling' THEN NEW.expires_at:=clock_timestamp()+interval '1 millisecond'*`+intString(expiry[0].Milliseconds())+`; END IF; RETURN NEW; END $$`)
		f.exec(t, `CREATE TRIGGER zz_fixture_review_expiry BEFORE UPDATE ON cosmoformer.plans FOR EACH ROW EXECUTE FUNCTION cosmoformer.fixture_review_expiry()`)
	}
	body := finishReviewFixture(t, f, false)
	p := body["run"].(map[string]any)["plan"].(map[string]any)
	provider := &approvalProvider{base: f.remote}
	clk := &approvalClock{start: time.Now()}
	cipher, _ := cryptostore.New(bytes.Repeat([]byte{42}, 32))
	handler, _ := httpapi.NewWithOptions(f.db.App, origin, httpapi.Options{Evidence: &evidence.Custody{Storage: f.storage, Bucket: "synthetic"}, Clock: clk, Cipher: cipher, Bitbucket: bitbucket.New(&http.Client{Transport: provider})})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	voter := &client{t: t, base: server.URL, token: f.admin.token, csrf: f.admin.csrf}
	a := &approvalFixture{executionFixture: f, voter: voter, provider: provider, clock: clk, plan: p, path: "/organizations/" + f.org + "/plans/" + p["id"].(string) + "/approvals"}
	f.exec(t, `INSERT INTO cosmoformer.grants(org_id,team_id,project_id,environment_id,role) SELECT org_id,team_id,project_id,environment_id,'approver' FROM cosmoformer.grants WHERE role='planner' LIMIT 1`)
	return a
}
func intString(n int64) string { b, _ := json.Marshal(n); return string(b) }
func (f *approvalFixture) body() map[string]any {
	return map[string]any{"expected_plan_revision": f.plan["revision"], "manifest_digest": f.plan["manifest_digest"]}
}
func (f *approvalFixture) withdraw(c *client) map[string]any {
	return c.call("DELETE", f.path+"/me", map[string]any{"expected_plan_revision": f.currentRevision()}, 200)
}
func (f *approvalFixture) currentRevision() int64 {
	var rev int64
	if e := f.db.Owner.QueryRow(context.Background(), `SELECT revision FROM cosmoformer.plans WHERE id=$1`, f.plan["id"]).Scan(&rev); e != nil {
		f.voter.t.Fatal(e)
	}
	return rev
}
func (f *approvalFixture) summary(c *client) map[string]any { return c.call("GET", f.path, nil, 200) }
func (f *approvalFixture) second(t *testing.T, name string) *client {
	t.Helper()
	var account string
	if e := f.db.Owner.QueryRow(context.Background(), `INSERT INTO cosmoformer.accounts(username,display_name,password_phc) SELECT $1,$1,password_phc FROM cosmoformer.accounts WHERE username='admin' RETURNING id::text`, name).Scan(&account); e != nil {
		t.Fatal(e)
	}
	f.exec(t, `INSERT INTO cosmoformer.memberships(org_id,account_id) VALUES($1,$2)`, f.org, account)
	f.exec(t, `INSERT INTO cosmoformer.team_members(org_id,team_id,account_id) SELECT org_id,id,$2 FROM cosmoformer.teams WHERE org_id=$1`, f.org, account)
	c := &client{t: t, base: f.voter.base}
	c.login(name)
	return c
}
func assertCount(t *testing.T, m map[string]any, count, min int) {
	t.Helper()
	if m["counted_approvals"] != float64(count) || m["minimum"] != float64(min) {
		t.Fatalf("wrong distinct count: %v", m)
	}
}
func TestExactPlanApprovalSelfDistinctAndReplay(t *testing.T) {
	for _, minimum := range []int{1, 2} {
		t.Run(intString(int64(minimum)), func(t *testing.T) {
			f := newApprovalFixture(t, minimum)
			initial := f.summary(f.voter)
			assertCount(t, initial, 0, minimum)
			if initial["eligible"] != true || initial["can_approve"] != true {
				t.Fatal(initial)
			}
			key := identity.Token()
			first := f.voter.callKey("POST", f.path, f.body(), 200, key)
			assertCount(t, first, 1, minimum)
			again := f.voter.callKey("POST", f.path, f.body(), 200, key)
			if again["vote_event_id"] != first["vote_event_id"] {
				t.Fatal("replay appended event")
			}
			f.provider.mu.Lock()
			calls := f.provider.calls
			f.provider.mu.Unlock()
			for range 3 {
				f.summary(f.voter)
			}
			f.provider.mu.Lock()
			if calls != f.provider.calls {
				t.Error("polling called provider")
			}
			f.provider.mu.Unlock()
			anotherSession := &client{t: t, base: f.voter.base}
			anotherSession.login("admin")
			assertCount(t, anotherSession.call("POST", f.path, f.body(), 200), 1, minimum)
			other := f.second(t, "second.approver")
			assertCount(t, other.call("POST", f.path, f.body(), 200), 2, minimum)
			f.exec(t, `INSERT INTO cosmoformer.teams(org_id,name) VALUES($1,'Duplicate approver membership')`, f.org)
			f.exec(t, `INSERT INTO cosmoformer.team_members(org_id,team_id,account_id) SELECT $1,t.id,a.id FROM cosmoformer.teams t CROSS JOIN cosmoformer.accounts a WHERE t.name='Duplicate approver membership' AND a.username='admin'`, f.org)
			f.exec(t, `INSERT INTO cosmoformer.grants(org_id,team_id,project_id,role) SELECT s.org_id,t.id,s.project_id,'approver' FROM cosmoformer.stacks s JOIN cosmoformer.teams t ON t.org_id=s.org_id WHERE s.id=$1 AND t.name='Duplicate approver membership'`, f.stack)
			assertCount(t, f.summary(f.voter), 2, minimum)
			var events, projections int
			var rev int64
			var bound bool
			e := f.db.Owner.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM cosmoformer.approvals),(SELECT count(*) FROM cosmoformer.plan_votes),revision,EXISTS(SELECT FROM cosmoformer.approvals e JOIN cosmoformer.runs r ON r.source_commit=e.source_commit WHERE e.manifest_digest=p.manifest_digest AND e.subject_revision=p.revision) FROM cosmoformer.plans p WHERE id=$1`, f.plan["id"]).Scan(&events, &projections, &rev, &bound)
			if e != nil || events != 3 || projections != 2 || rev != 2 || !bound {
				t.Fatal("identity/history changed", events, projections, rev, bound, e)
			}
			if _, e = f.db.Owner.Exec(context.Background(), `UPDATE cosmoformer.approvals SET action='revoke'`); e == nil {
				t.Fatal("mutable event")
			}
			if _, e = f.db.Owner.Exec(context.Background(), `DELETE FROM cosmoformer.approvals`); e == nil {
				t.Fatal("deleted event")
			}
			assertCount(t, f.withdraw(anotherSession), 1, minimum)
		})
	}
}
func TestApprovalAuthorityLossRestorationAndOutageWithdrawal(t *testing.T) {
	for _, loss := range []string{"grant", "team", "membership", "account"} {
		t.Run(loss, func(t *testing.T) {
			f := newApprovalFixture(t, 1)
			other := f.second(t, "voter.two")
			key := identity.Token()
			other.callKey("POST", f.path, f.body(), 200, key)
			var actor string
			if e := f.db.Owner.QueryRow(context.Background(), `SELECT id::text FROM cosmoformer.accounts WHERE username='voter.two'`).Scan(&actor); e != nil {
				t.Fatal(e)
			}
			switch loss {
			case "grant":
				f.exec(t, `UPDATE cosmoformer.grants SET revoked_at=clock_timestamp() WHERE role='approver'`)
			case "team", "membership":
				f.exec(t, `DELETE FROM cosmoformer.team_members WHERE account_id=$1`, actor)
				if loss == "membership" {
					f.exec(t, `DELETE FROM cosmoformer.memberships WHERE account_id=$1`, actor)
				}
			case "account":
				f.exec(t, `UPDATE cosmoformer.accounts SET enabled=false WHERE id=$1`, actor)
			}
			assertCount(t, f.summary(f.voter), 0, 1)
			if loss == "grant" {
				other.callKey("POST", f.path, f.body(), 403, key)
				f.provider.mu.Lock()
				f.provider.outage = true
				f.provider.mu.Unlock()
				assertCount(t, f.withdraw(other), 0, 1) // no approval authority or provider required
				f.provider.mu.Lock()
				f.provider.outage = false
				f.provider.mu.Unlock()
			}
			switch loss {
			case "grant":
				f.exec(t, `UPDATE cosmoformer.grants SET revoked_at=NULL WHERE role='approver'`)
			case "membership":
				f.exec(t, `INSERT INTO cosmoformer.memberships(org_id,account_id) VALUES($1,$2)`, f.org, actor)
				fallthrough
			case "team":
				f.exec(t, `INSERT INTO cosmoformer.team_members(org_id,team_id,account_id) SELECT org_id,id,$2 FROM cosmoformer.teams WHERE org_id=$1`, f.org, actor)
			case "account":
				f.exec(t, `UPDATE cosmoformer.accounts SET enabled=true WHERE id=$1`, actor)
			}
			want := 1
			if loss == "grant" {
				want = 0
			}
			assertCount(t, f.summary(f.voter), want, 1)
			f.exec(t, `UPDATE cosmoformer.secrets SET semantic_revision=semantic_revision+1 WHERE id=$1`, f.assignment.Execution.Secrets[0].ID)
			assertCount(t, f.summary(f.voter), 0, 1)
		})
	}
}
func TestApprovalStaleIdentityInputsAndPolicy(t *testing.T) {
	for _, change := range []string{"secret", "credential", "policy", "runtime", "pool", "resource", "stack", "epoch", "recovery", "evidence", "ciphertext", "refresh", "label"} {
		t.Run(change, func(t *testing.T) {
			f := newApprovalFixture(t, 1)
			f.voter.call("POST", f.path, f.body(), 200)
			wrong := f.body()
			wrong["manifest_digest"] = strings.Repeat("f", 64)
			f.voter.call("POST", f.path, wrong, 409)
			wrong = f.body()
			wrong["expected_plan_revision"] = 1
			f.voter.call("POST", f.path, wrong, 409)
			switch change {
			case "secret":
				f.exec(t, `UPDATE cosmoformer.secrets SET semantic_revision=semantic_revision+1 WHERE id=$1`, f.assignment.Execution.Secrets[0].ID)
			case "credential":
				f.exec(t, `UPDATE cosmoformer.source_credentials SET semantic_revision=semantic_revision+1`)
			case "policy":
				f.exec(t, `UPDATE cosmoformer.projects SET minimum_approvals=2,revision=revision+1`)
				f.exec(t, `UPDATE cosmoformer.projects SET minimum_approvals=1,revision=revision+1`)
			case "runtime":
				f.exec(t, `UPDATE cosmoformer.runtime_catalog SET withdrawn=true`)
				f.exec(t, `UPDATE cosmoformer.runtime_catalog SET withdrawn=false`)
			case "pool":
				f.exec(t, `UPDATE cosmoformer.worker_pools SET enabled=false`)
				f.exec(t, `UPDATE cosmoformer.worker_pools SET enabled=true`)
			case "resource":
				f.exec(t, `UPDATE cosmoformer.resource_grants SET enabled=false WHERE kind='pool'`)
				f.exec(t, `UPDATE cosmoformer.resource_grants SET enabled=true WHERE kind='pool'`)
			case "stack":
				f.exec(t, `UPDATE cosmoformer.stacks SET archived=true WHERE id=$1`, f.stack)
				f.exec(t, `UPDATE cosmoformer.stacks SET archived=false WHERE id=$1`, f.stack)
			case "epoch":
				f.exec(t, `UPDATE cosmoformer.backend_targets SET state_epoch=state_epoch+1`)
			case "recovery":
				f.exec(t, `INSERT INTO cosmoformer.recovery_cases(org_id,stack_id,target_id,status,evidence,opened_at) SELECT org_id,stack_id,target_id,'open','{}',clock_timestamp() FROM cosmoformer.plans WHERE id=$1`, f.plan["id"])
			case "evidence":
				f.exec(t, `UPDATE cosmoformer.artifacts SET state='unavailable' WHERE id=(SELECT manifest_artifact_id FROM cosmoformer.plans WHERE id=$1)`, f.plan["id"])
			case "ciphertext":
				cipher, _ := cryptostore.New(bytes.Repeat([]byte{42}, 32))
				sealed, e := cipher.Seal([]byte(f.secret), cryptostore.AAD(f.org, f.assignment.Execution.Secrets[0].ID, "managed_secret", 1), func(fp [32]byte) error {
					_, e := f.db.Owner.Exec(context.Background(), `SELECT cosmoformer.reserve_encryptions($1,1)`, fp[:])
					return e
				})
				if e != nil {
					t.Fatal(e)
				}
				f.exec(t, `UPDATE cosmoformer.secrets SET encrypted_value=$1,revision=revision+1`, sealed)
				f.exec(t, `UPDATE cosmoformer.source_credentials SET encrypted_value=encrypted_value,revision=revision+1`)
			case "refresh":
				f.exec(t, `UPDATE cosmoformer.integrations SET encrypted_credentials=encrypted_credentials,revision=revision+1`)
			case "label":
				f.exec(t, `UPDATE cosmoformer.projects SET name='Renamed',revision=revision+1`)
			}
			operational := change == "ciphertext" || change == "refresh" || change == "label"
			summary := f.summary(f.voter)
			want := 0
			if operational {
				want = 1
			}
			assertCount(t, summary, want, 1)
			status := 409
			if operational {
				status = 200
			}
			f.voter.call("POST", f.path, map[string]any{"expected_plan_revision": f.currentRevision(), "manifest_digest": f.plan["manifest_digest"]}, status)
			f.withdraw(f.voter)
			var n int
			if e := f.db.Owner.QueryRow(context.Background(), `SELECT count(*) FROM cosmoformer.approvals`).Scan(&n); e != nil || n < 2 {
				t.Fatal("history lost", n, e)
			}
		})
	}
}

// rawVote returns errors to the parent test instead of terminating a goroutine.
func rawVote(c *client, path string, body any, key string) (int, map[string]any, error) {
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", c.base+"/api/v1"+path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", origin)
	req.Header.Set("X-Cosmoformer-Request", "browser")
	req.Header.Set("Idempotency-Key", key)
	req.Header.Set("X-CSRF-Token", c.csrf)
	req.AddCookie(&http.Cookie{Name: "__Host-cosmoformer_session", Value: c.token})
	res, e := http.DefaultClient.Do(req)
	if e != nil {
		return 0, nil, e
	}
	defer res.Body.Close()
	var m map[string]any
	e = json.NewDecoder(res.Body).Decode(&m)
	return res.StatusCode, m, e
}
func TestApprovalSourceOutagesChangesAndReverseArrivals(t *testing.T) {
	t.Run("outage", func(t *testing.T) {
		f := newApprovalFixture(t, 1)
		f.voter.call("POST", f.path, f.body(), 200)
		f.provider.mu.Lock()
		f.provider.outage = true
		f.provider.mu.Unlock()
		f.voter.call("POST", f.path, f.body(), 503)
		assertCount(t, f.withdraw(f.voter), 0, 1)
	})
	t.Run("ABA", func(t *testing.T) {
		f := newApprovalFixture(t, 1)
		f.voter.call("POST", f.path, f.body(), 200)
		f.remote.mu.Lock()
		f.remote.commit = strings.Repeat("b", 40)
		f.remote.mu.Unlock()
		f.voter.call("POST", f.path, f.body(), 409)
		// Confirm that the new authoritative facts committed despite the rejection.
		var commit string
		if e := f.db.Owner.QueryRow(context.Background(), `SELECT facts->>'commit' FROM cosmoformer.source_subjects WHERE id=$1`, f.assignment.Source.SubjectID).Scan(&commit); e != nil || commit != strings.Repeat("b", 40) {
			t.Fatal("changed facts rolled back", commit, e)
		}
		f.remote.mu.Lock()
		f.remote.commit = strings.Repeat("a", 40)
		f.remote.mu.Unlock()
		assertCount(t, f.summary(f.voter), 0, 1)
		f.voter.call("POST", f.path, map[string]any{"expected_plan_revision": f.currentRevision(), "manifest_digest": f.plan["manifest_digest"]}, 409)
	})
	t.Run("reverse", func(t *testing.T) {
		f := newApprovalFixture(t, 1)
		entered, release := make(chan struct{}), make(chan struct{})
		t.Cleanup(func() {
			select {
			case <-release:
			default:
				close(release)
			}
		})
		var once sync.Once
		f.provider.after = func() { once.Do(func() { close(entered); <-release }) }
		done := make(chan int, 1)
		go func() { status, _, _ := rawVote(f.voter, f.path, f.body(), identity.Token()); done <- status }()
		awaitApprovalSourceRead(t, entered, done)
		// A later ticket observes changed source, while the first response is held.
		f.provider.mu.Lock()
		f.provider.after = nil
		f.provider.mu.Unlock()
		f.remote.mu.Lock()
		f.remote.commit = strings.Repeat("b", 40)
		f.remote.mu.Unlock()
		f.voter.call("POST", f.path, f.body(), 409)
		close(release)
		if status := <-done; status != 409 {
			t.Fatal("reverse arrival accepted", status)
		}
		assertCount(t, f.summary(f.voter), 0, 1)
	})
}
func TestApprovalConcurrentVotesAndRevocations(t *testing.T) {
	f := newApprovalFixture(t, 2)
	other := f.second(t, "concurrent.voter")
	// Simultaneous reads may fence an older ticket. Every accepted vote must
	// serialize into one account projection; explicit new actions can retry.
	start := make(chan struct{})
	type result struct {
		status int
		err    error
	}
	done := make(chan result, 8)
	for i := 0; i < 8; i++ {
		go func(i int) {
			<-start
			c := f.voter
			if i%2 == 1 {
				c = other
			}
			s, _, e := rawVote(c, f.path, f.body(), identity.Token())
			done <- result{s, e}
		}(i)
	}
	close(start)
	for range 8 {
		r := <-done
		if r.err != nil || r.status != 200 && r.status != 409 {
			t.Fatal(r)
		}
	}
	f.voter.call("POST", f.path, f.body(), 200)
	other.call("POST", f.path, f.body(), 200)
	assertCount(t, f.summary(f.voter), 2, 2)
	// Own withdrawal and another approval commute with respect to distinct users.
	done = make(chan result, 1)
	go func() { s, _, e := rawVote(other, f.path, f.body(), identity.Token()); done <- result{s, e} }()
	f.withdraw(f.voter)
	if r := <-done; r.err != nil || r.status != 200 {
		t.Fatal(r)
	}
	assertCount(t, f.summary(f.voter), 1, 2)
}
func TestApprovalEvidenceUnavailableAndGateExpiry(t *testing.T) {
	t.Run("missing version", func(t *testing.T) {
		f := newApprovalFixture(t, 1)
		var key string
		if e := f.db.Owner.QueryRow(context.Background(), `SELECT object_key FROM cosmoformer.artifacts WHERE plan_id=$1 AND kind='binary_plan'`, f.plan["id"]).Scan(&key); e != nil {
			t.Fatal(e)
		}
		f.storage.mu.Lock()
		delete(f.storage.objects, key)
		f.storage.mu.Unlock()
		f.voter.call("POST", f.path, f.body(), 409)
		assertCount(t, f.summary(f.voter), 0, 1)
	})
	t.Run("expiry waiting for gate", func(t *testing.T) {
		f := newApprovalFixture(t, 1, 3*time.Second)
		entered, release := make(chan struct{}), make(chan struct{})
		t.Cleanup(func() {
			select {
			case <-release:
			default:
				close(release)
			}
		})
		f.provider.after = func() { close(entered); <-release }
		done := make(chan int, 1)
		go func() { s, _, _ := rawVote(f.voter, f.path, f.body(), identity.Token()); done <- s }()
		awaitApprovalSourceRead(t, entered, done)
		blocker, e := pgx.Connect(context.Background(), f.db.OwnerDSN)
		if e != nil {
			t.Fatal(e)
		}
		defer blocker.Close(context.Background())
		tx, e := blocker.Begin(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(context.Background())
		if _, e = tx.Exec(context.Background(), `SELECT set_config('app.org_id',$1,true)`, f.org); e != nil {
			t.Fatal(e)
		}
		if _, e = tx.Exec(context.Background(), `SELECT id FROM cosmoformer.coordination_gate FOR UPDATE`); e != nil {
			t.Fatal(e)
		}
		close(release)
		// Wait using the actual database expiry, not a fabricated transaction-start timestamp.
		var wait float64
		if e = tx.QueryRow(context.Background(), `SELECT greatest(0,extract(epoch FROM (expires_at-clock_timestamp())))+0.05 FROM cosmoformer.plans WHERE id=$1`, f.plan["id"]).Scan(&wait); e != nil {
			t.Fatal(e)
		}
		time.Sleep(time.Duration(wait * float64(time.Second)))
		tx.Commit(context.Background())
		if status := <-done; status != 409 {
			t.Fatal("expired vote accepted", status)
		}
		assertCount(t, f.summary(f.voter), 0, 1)
	})
}

func TestApprovalCurrentAuthorizationBeforeReplayAndFinalGate(t *testing.T) {
	for _, role := range []string{"administrator", "planner", "deployer", "raw_evidence_read"} {
		t.Run(role, func(t *testing.T) {
			f := newApprovalFixture(t, 1)
			key := identity.Token()
			f.voter.callKey("POST", f.path, f.body(), 200, key)
			f.exec(t, `UPDATE cosmoformer.grants SET revoked_at=clock_timestamp() WHERE role='approver'`)
			f.exec(t, `INSERT INTO cosmoformer.grants(org_id,team_id,project_id,environment_id,role) SELECT org_id,team_id,project_id,environment_id,$1 FROM cosmoformer.grants WHERE role='approver' ON CONFLICT DO NOTHING`, role)
			f.voter.callKey("POST", f.path, f.body(), 403, key)
			assertCount(t, f.summary(f.voter), 0, 1)
			f.withdraw(f.voter)
		})
	}
	for _, race := range []string{"session", "authority", "secret", "policy", "visibility"} {
		t.Run("during read "+race, func(t *testing.T) {
			f := newApprovalFixture(t, 1)
			entered, release := make(chan struct{}), make(chan struct{})
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
			})
			f.provider.after = func() { close(entered); <-release }
			done := make(chan int, 1)
			go func() { status, _, _ := rawVote(f.voter, f.path, f.body(), identity.Token()); done <- status }()
			awaitApprovalSourceRead(t, entered, done)
			switch race {
			case "session":
				f.exec(t, `UPDATE cosmoformer.sessions SET absolute_expires_at=clock_timestamp() WHERE verifier=$1`, identity.Digest(f.voter.token))
			case "authority":
				f.exec(t, `UPDATE cosmoformer.grants SET revoked_at=clock_timestamp() WHERE role='approver'`)
			case "visibility":
				f.exec(t, `UPDATE cosmoformer.grants SET revoked_at=clock_timestamp()`)
			case "secret":
				f.exec(t, `UPDATE cosmoformer.secrets SET semantic_revision=semantic_revision+1`)
			case "policy":
				f.exec(t, `UPDATE cosmoformer.projects SET minimum_approvals=2`)
			}
			close(release)
			status := <-done
			want := 409
			if race == "session" {
				want = 401
			}
			if race == "authority" || race == "visibility" {
				want = 403
			}
			if status != want {
				t.Fatal("stale authority accepted", race, status)
			}
			var events int
			if e := f.db.Owner.QueryRow(context.Background(), `SELECT count(*) FROM cosmoformer.approvals`).Scan(&events); e != nil || events != 0 {
				t.Fatal("failed gate left a vote", events, e)
			}
		})
	}
}
func TestApprovalClockAndReadinessBoundaries(t *testing.T) {
	for _, scenario := range []string{"clock failure", "clock discrepancy", "maintenance", "unsupported", "recovery only", "unready"} {
		t.Run(scenario, func(t *testing.T) {
			f := newApprovalFixture(t, 1)
			switch scenario {
			case "clock failure":
				f.clock.mu.Lock()
				f.clock.failed = true
				f.clock.mu.Unlock()
			case "clock discrepancy":
				f.voter.call("POST", f.path, f.body(), 200)
				f.clock.mu.Lock()
				f.clock.offset += 5 * time.Second
				f.clock.mu.Unlock()
			case "maintenance":
				f.exec(t, `UPDATE cosmoformer.installation SET maintenance=true`)
			case "unsupported":
				f.exec(t, `UPDATE cosmoformer.plans SET status='unsupported' WHERE id=$1`, f.plan["id"])
			case "recovery only":
				f.exec(t, `UPDATE cosmoformer.plans SET status='recovery_only' WHERE id=$1`, f.plan["id"])
			case "unready":
				f.exec(t, `UPDATE cosmoformer.plans SET status='assembling' WHERE id=$1`, f.plan["id"])
			}
			want := 409
			if strings.HasPrefix(scenario, "clock") {
				want = 503
			}
			f.voter.call("POST", f.path, map[string]any{"expected_plan_revision": f.currentRevision(), "manifest_digest": f.plan["manifest_digest"]}, want)
			if scenario == "clock discrepancy" {
				var latched bool
				if e := f.db.Owner.QueryRow(context.Background(), `SELECT clock_unreliable FROM cosmoformer.installation`).Scan(&latched); e != nil || !latched {
					t.Fatal("clock failure was rolled back", e)
				}
			}
		})
	}
}

func TestApprovalFreshnessAfterFinalReferenceChecks(t *testing.T) {
	f := newApprovalFixture(t, 1)
	f.provider.after = func() {
		f.clock.mu.Lock()
		// AcceptObservation, the gated clock observation, then the early
		// freshness check. Expire the read during the remaining gate work.
		f.clock.advanceAfter = 3
		f.clock.mu.Unlock()
	}
	f.voter.call("POST", f.path, f.body(), 409)
	assertCount(t, f.summary(f.voter), 0, 1)
	var events int
	if e := f.db.Owner.QueryRow(context.Background(), `SELECT count(*) FROM cosmoformer.approvals`).Scan(&events); e != nil || events != 0 {
		t.Fatal("expired source read appended a vote", events, e)
	}
}

func TestApprovalScopedConfigurationInvalidation(t *testing.T) {
	for _, change := range []string{"unrelated secret", "ciphertext only", "pool no-op", "selected secret value", "selected secret disabled"} {
		t.Run(change, func(t *testing.T) {
			f := newApprovalFixture(t, 1)
			f.voter.call("POST", f.path, f.body(), 200)
			base := "/organizations/" + f.org
			switch change {
			case "unrelated secret":
				project := id(f.admin.call("POST", base+"/projects", map[string]any{"name": "Unrelated project"}, 200))
				secret := id(f.admin.call("POST", base+"/secrets", map[string]any{"project_id": project, "name": "Unrelated secret", "value": "synthetic old value"}, 200))
				other := f.second(t, "unrelated.manager")
				var actor string
				if e := f.db.Owner.QueryRow(context.Background(), `SELECT id::text FROM cosmoformer.accounts WHERE username='unrelated.manager'`).Scan(&actor); e != nil {
					t.Fatal(e)
				}
				f.exec(t, `DELETE FROM cosmoformer.team_members WHERE account_id=$1`, actor)
				team := id(f.admin.call("POST", base+"/teams", map[string]any{"name": "Unrelated managers"}, 200))
				f.admin.call("POST", base+"/teams/"+team+"/members", map[string]any{"account_id": actor}, 200)
				f.admin.call("POST", base+"/grants", map[string]any{"team_id": team, "project_id": project, "role": "administrator"}, 200)
				other.call("GET", f.path, nil, 403)
				other.call("PATCH", base+"/secrets/"+secret, map[string]any{"expected_revision": 1, "enabled": true, "value": "synthetic new value"}, 200)
			case "ciphertext only":
				ref := f.assignment.Execution.Secrets[0]
				f.voter.call("PATCH", base+"/secrets/"+ref.ID, map[string]any{"expected_revision": 1, "enabled": true}, 200)
				var semantic int64
				if e := f.db.Owner.QueryRow(context.Background(), `SELECT semantic_revision FROM cosmoformer.secrets WHERE id=$1`, ref.ID).Scan(&semantic); e != nil || semantic != ref.Revision {
					t.Fatal("re-encryption changed the secret identity", semantic, e)
				}
			case "pool no-op":
				f.admin.call("PATCH", base+"/worker-pools/"+f.assignment.Execution.Pool.ID, map[string]any{"expected_revision": 1, "enabled": true}, 200)
			case "selected secret value", "selected secret disabled":
				body := map[string]any{"expected_revision": 1, "enabled": false}
				if change == "selected secret value" {
					body["enabled"], body["value"] = true, "synthetic changed input"
				}
				f.voter.call("PATCH", base+"/secrets/"+f.assignment.Execution.Secrets[0].ID, body, 200)
				assertCount(t, f.summary(f.voter), 0, 1)
				f.voter.call("POST", f.path, f.body(), 409)
				return
			}
			assertCount(t, f.summary(f.voter), 1, 1)
			if revision := f.currentRevision(); revision != 2 {
				t.Fatal("unaffected plan was invalidated", revision)
			}
		})
	}
}

func TestApprovalStricterGitPolicyAndPRContext(t *testing.T) {
	f := approvalFixtureWithExecution(t, newExecutionFixtureForSourceProfile(t, map[string]any{"kind": "pr", "pr_number": 7}, true, 1, 2))
	assertCount(t, f.summary(f.voter), 0, 2)
	vote := f.voter.call("POST", f.path, f.body(), 200)
	assertCount(t, vote, 1, 2)
	// Raising the admin floor is semantic even when Git already requested two.
	f.exec(t, `UPDATE cosmoformer.projects SET minimum_approvals=2,revision=revision+1`)
	assertCount(t, f.summary(f.voter), 0, 2)
	f.withdraw(f.voter)
	for _, change := range []string{"closed", "destination", "fork", "destination repository"} {
		t.Run(change, func(t *testing.T) {
			f := approvalFixtureWithExecution(t, newExecutionFixtureForSourceProfile(t, map[string]any{"kind": "pr", "pr_number": 7}, true, 1))
			f.remote.mu.Lock()
			switch change {
			case "closed":
				f.remote.prState = "MERGED"
			case "destination":
				f.remote.prDestination = "other-branch"
			case "fork":
				f.remote.prSourceRepository = "{unsupported-fork}"
			case "destination repository":
				f.remote.prDestinationRepository = "{replacement-repository}"
			}
			f.remote.mu.Unlock()
			f.voter.call("POST", f.path, f.body(), 409)
			if change == "fork" || change == "destination repository" {
				var sourceRepo, repo string
				if e := f.db.Owner.QueryRow(context.Background(), `SELECT facts->>'source_repository_id',facts->>'repository_id' FROM cosmoformer.source_subjects WHERE id=$1`, f.assignment.Source.SubjectID).Scan(&sourceRepo, &repo); e != nil {
					t.Fatal(e)
				}
				if change == "fork" && sourceRepo != "{unsupported-fork}" || change == "destination repository" && repo != "{replacement-repository}" {
					t.Fatal("changed unsupported source facts were rolled back", sourceRepo, repo)
				}
			}
			assertCount(t, f.summary(f.voter), 0, 1)
		})
	}
}
func TestApprovalReplaysDoNotReverseNewerVotes(t *testing.T) {
	f := newApprovalFixture(t, 1)
	approveKey, revokeKey := identity.Token(), identity.Token()
	vote := f.voter.callKey("POST", f.path, f.body(), 200, approveKey)
	revoked := f.voter.callKey("DELETE", f.path+"/me", map[string]any{"expected_plan_revision": 2}, 200, revokeKey)
	replay := f.voter.callKey("POST", f.path, f.body(), 200, approveKey)
	assertCount(t, replay, 0, 1)
	if replay["vote_event_id"] != vote["vote_event_id"] {
		t.Fatal("positive replay changed event")
	}
	f.voter.call("POST", f.path, f.body(), 200)
	replay = f.voter.callKey("DELETE", f.path+"/me", map[string]any{"expected_plan_revision": 2}, 200, revokeKey)
	assertCount(t, replay, 1, 1)
	if replay["vote_event_id"] != revoked["vote_event_id"] {
		t.Fatal("withdrawal replay changed event")
	}
	conflict := f.body()
	conflict["manifest_digest"] = strings.Repeat("f", 64)
	f.voter.callKey("POST", f.path, conflict, 409, approveKey)
	var events int
	if e := f.db.Owner.QueryRow(context.Background(), `SELECT count(*) FROM cosmoformer.approvals`).Scan(&events); e != nil || events != 3 {
		t.Fatal("replay changed history", events, e)
	}
}

func awaitApprovalSourceRead(t *testing.T, entered <-chan struct{}, done <-chan int) {
	t.Helper()
	select {
	case <-entered:
		return
	case status := <-done:
		t.Fatalf("approval ended before its source read: %d", status)
	case <-time.After(15 * time.Second):
		t.Fatal("approval did not reach its bounded source read")
	}
}
