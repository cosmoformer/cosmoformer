package awsaccess

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go/logging"
	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/clock"
	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/source"
)

const bootstrapARN = "arn:aws:iam::111122223333:role/workers/bootstrap"
const executionARN = "arn:aws:iam::444455556666:role/projects/execution"
const bootKey = "ASIABBBBBBBBBBBBBBBB"
const execKey = "ASIAEEEEEEEEEEEEEEEE"
const bootToken = "synthetic-bootstrap-token"
const execToken = "synthetic-execution-token"

type testClock struct {
	mu   sync.Mutex
	tick time.Duration
	err  error
}

func (c *testClock) Now() (time.Duration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tick, c.err
}
func (c *testClock) advance(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.tick += d }

type fixture struct {
	t                                                                      *testing.T
	server                                                                 *httptest.Server
	clock                                                                  *testClock
	wall                                                                   time.Time
	a                                                                      runs.Assignment
	worker                                                                 runs.Identity
	registry                                                               *evidence.Registry
	bootARN, bootAccount, roleList, region, owner                          string
	assumedARN, assumedAccount, assumedUserID, responseARN, responseUserID string
	key, secret, token                                                     string
	expiration                                                             *time.Time
	bootCredentials                                                        map[string]any
	denySTS, denyHead, noToken, duplicateRegion, lostAssume                bool
	stsStatus, headStatus                                                  int
	delayAssume, delayHead                                                 time.Duration
	assumeCount, headCount, imdsCount, callerCount                         atomic.Int32
	request                                                                url.Values
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	b, e := source.CanonicalBackend(map[string]any{"bucket": "synthetic-state", "key": "state", "region": "eu-west-1", "encrypt": true, "use_lockfile": true}, source.BackendContext{Engine: catalog.Engine{Name: "opentofu", Version: "1.13.8"}, Workspace: "default", ExpectedOwner: "777788889999"}, "")
	if e != nil {
		t.Fatal(e)
	}
	pool, repo, runtime := runs.ID(), runs.ID(), runs.ID()
	s := runs.ExecutionSpec{Version: 1, Phase: "execution", StackRevisionID: runs.ID(), Source: runs.Source{RepositoryID: repo, URL: "https://bitbucket.org/fictional/repo.git", Commit: strings.Repeat("a", 40), Root: ".", SubjectID: runs.ID(), FactsRevision: 1}, Engine: runs.Runtime{ID: runtime, Name: "opentofu", Version: "1.13.8", Arch: "amd64", ImageDigest: "sha256:" + strings.Repeat("a", 64)}, Workspace: "default", VarFiles: []string{}, BackendFiles: []string{}, Pool: runs.Pool{ID: pool, Tags: []string{}, GrantRevision: 1}, Backend: b, AWS: runs.ExecutionAWS{AWSRef: runs.AWSRef{ID: runs.ID(), Revision: 1, RoleARN: executionARN, Account: "444455556666"}, Bootstrap: bootstrapARN, SessionSeconds: 3600}, Secrets: []runs.SecretRef{}, SourceCredentials: []runs.SourceCredential{}, Grants: []runs.ResourceRef{{ID: runs.ID(), Kind: "pool", ResourceID: pool, Revision: 1}, {ID: runs.ID(), Kind: "repository", ResourceID: repo, Revision: 1}}, Review: runs.ReviewPolicy{Minimum: 1, Revision: 1}, RuleVersion: "synthetic-v1", ProfileID: "synthetic-v1"}
	for _, c := range b.Claims {
		s.Grants = append(s.Grants, runs.ResourceRef{ID: runs.ID(), Kind: "s3_object", Namespace: c.Namespace, Key: c.Key, Revision: 1})
	}
	a := runs.Assignment{AttemptID: runs.ID(), OperationID: runs.ID(), RunID: runs.ID(), StackID: runs.ID(), RuntimeID: runtime, Kind: "plan", Phase: "execution", Execution: &s, Digest: runs.Digest(s), CapSeconds: 1200, RemainingMS: 1200000, Platform: runs.ID(), NextSeq: 1}
	f := &fixture{t: t, clock: &testClock{tick: time.Second}, wall: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC), a: a, worker: runs.Identity{WorkerID: runs.ID(), Agent: runs.ID(), Generation: 1}, registry: &evidence.Registry{}, bootARN: "arn:aws:sts::111122223333:assumed-role/bootstrap/i-synthetic", bootAccount: "111122223333", roleList: "bootstrap\n", region: b.Region, owner: b.ExpectedOwner, key: execKey, secret: strings.Repeat("E", 40), token: execToken, assumedAccount: s.AWS.Account}
	f.assumedARN = "arn:aws:sts::444455556666:assumed-role/execution/cosmoformer-" + a.AttemptID
	f.assumedUserID = "AROAEEEEEEEEEEEEEEEEE:cosmoformer-" + a.AttemptID
	f.responseARN = f.assumedARN
	f.responseUserID = f.assumedUserID
	exp := f.wall.Add(time.Hour)
	f.expiration = &exp
	f.bootCredentials = map[string]any{"Code": "Success", "Type": "AWS-HMAC", "LastUpdated": f.wall, "AccessKeyId": bootKey, "SecretAccessKey": strings.Repeat("B", 40), "Token": bootToken, "Expiration": f.wall.Add(time.Hour)}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func xmlText(v string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(v))
	return b.String()
}
func (f *fixture) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == "PUT" && r.URL.Path == "/latest/api/token":
		if f.noToken {
			w.WriteHeader(403)
			return
		}
		w.Header().Set("X-aws-ec2-metadata-token-ttl-seconds", "21600")
		fmt.Fprint(w, "synthetic-imds-token")
	case strings.HasPrefix(r.URL.Path, "/latest/meta-data/"):
		f.imdsCount.Add(1)
		if r.Header.Get("X-aws-ec2-metadata-token") != "synthetic-imds-token" {
			f.t.Error("IMDSv1 request")
			w.WriteHeader(403)
			return
		}
		switch r.URL.Path {
		case "/latest/meta-data/iam/security-credentials/":
			fmt.Fprint(w, f.roleList)
		case "/latest/meta-data/iam/security-credentials/bootstrap":
			json.NewEncoder(w).Encode(f.bootCredentials)
		default:
			f.t.Error("unexpected metadata path")
			w.WriteHeader(404)
		}
	case r.Method == "POST":
		if e := r.ParseForm(); e != nil {
			f.t.Error(e)
			return
		}
		switch r.Form.Get("Action") {
		case "GetCallerIdentity":
			f.callerCount.Add(1)
			arn, account, user := f.bootARN, f.bootAccount, "AROABBBBBBBBBBBBBBBBB:i-synthetic"
			if strings.Contains(r.Header.Get("Authorization"), execKey+"/") {
				arn, account, user = f.assumedARN, f.assumedAccount, f.assumedUserID
			} else if !strings.Contains(r.Header.Get("Authorization"), bootKey+"/") {
				f.t.Error("ambient or unknown credential used")
			}
			fmt.Fprintf(w, `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult><Arn>%s</Arn><Account>%s</Account><UserId>%s</UserId></GetCallerIdentityResult></GetCallerIdentityResponse>`, xmlText(arn), xmlText(account), xmlText(user))
		case "AssumeRole":
			f.assumeCount.Add(1)
			f.request = r.Form
			if !strings.Contains(r.Header.Get("Authorization"), bootKey+"/") || r.Header.Get("X-Amz-Security-Token") != bootToken {
				f.t.Error("assume did not use fixed bootstrap credentials")
			}
			f.clock.advance(f.delayAssume)
			if f.lostAssume {
				conn, _, e := w.(http.Hijacker).Hijack()
				if e != nil {
					f.t.Error(e)
					return
				}
				conn.Close()
				return
			}
			if f.stsStatus != 0 {
				w.WriteHeader(f.stsStatus)
				fmt.Fprint(w, `<ErrorResponse><Error><Code>InternalFailure</Code><Message>synthetic-execution-token</Message></Error></ErrorResponse>`)
				return
			}
			if f.denySTS {
				w.WriteHeader(403)
				fmt.Fprintf(w, `<ErrorResponse><Error><Code>AccessDenied</Code><Message>%s</Message></Error></ErrorResponse>`, execToken)
				return
			}
			exp := ""
			if f.expiration != nil {
				exp = "<Expiration>" + f.expiration.Format(time.RFC3339) + "</Expiration>"
			}
			fmt.Fprintf(w, `<AssumeRoleResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><AssumeRoleResult><Credentials><AccessKeyId>%s</AccessKeyId><SecretAccessKey>%s</SecretAccessKey><SessionToken>%s</SessionToken>%s</Credentials><AssumedRoleUser><Arn>%s</Arn><AssumedRoleId>%s</AssumedRoleId></AssumedRoleUser></AssumeRoleResult></AssumeRoleResponse>`, xmlText(f.key), xmlText(f.secret), xmlText(f.token), exp, xmlText(f.responseARN), xmlText(f.responseUserID))
		default:
			f.t.Error("unexpected STS operation")
			w.WriteHeader(400)
		}
	case r.Method == "HEAD":
		f.headCount.Add(1)
		f.clock.advance(f.delayHead)
		if r.URL.Path != "/synthetic-state" {
			f.t.Error("unexpected bucket path", r.URL.Path)
		}
		if !strings.Contains(r.Header.Get("Authorization"), execKey+"/") || r.Header.Get("X-Amz-Security-Token") != execToken {
			f.t.Error("HeadBucket did not use execution credentials")
		}
		if r.Header.Get("X-Amz-Expected-Bucket-Owner") != f.owner || f.denyHead {
			w.WriteHeader(403)
			return
		}
		w.Header().Set("X-Amz-Bucket-Region", f.region)
		if f.duplicateRegion {
			w.Header().Add("X-Amz-Bucket-Region", "us-east-1")
		}
		if f.headStatus != 0 {
			w.WriteHeader(f.headStatus)
		}
	default:
		f.t.Error("unexpected fixture request")
		w.WriteHeader(400)
	}
}

// Endpoint overrides exist only in this test file; use the actual SDK signing,
// serialization and decoding stack against loopback, never real AWS endpoints.
func (f *fixture) access() (*Access, error) {
	h := f.server.Client()
	h.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c := clients{
		metadata: imds.New(imds.Options{Endpoint: f.server.URL, HTTPClient: h, ClientEnableState: imds.ClientEnabled, EnableFallback: aws.FalseTernary, Retryer: aws.NopRetryer{}, Logger: logging.Nop{}}),
		sts: func(c aws.Credentials, r string) *sts.Client {
			return sts.NewFromConfig(fixedConfig(c, r, h), func(o *sts.Options) { o.BaseEndpoint = aws.String(f.server.URL) })
		},
		s3: func(c aws.Credentials, r string) *s3.Client {
			return s3.NewFromConfig(fixedConfig(c, r, h), func(o *s3.Options) { o.BaseEndpoint = aws.String(f.server.URL); o.UsePathStyle = true })
		},
	}
	return newAccess(bootstrapARN, f.a, f.worker, time.Second, f.registry, f.clock, func() time.Time { n, _ := f.clock.Now(); return f.wall.Add(n - time.Second) }, c)
}
func (f *fixture) mustAccess() *Access {
	f.t.Helper()
	a, e := f.access()
	if e != nil {
		f.t.Fatal(e)
	}
	return a
}
func prepare(t *testing.T, a *Access) {
	t.Helper()
	if e := a.Prepare(context.Background()); e != nil {
		t.Fatal(e)
	}
}

func TestFixedSessionPolicyAndCrossAccountOwner(t *testing.T) {
	f := newFixture(t)
	a := f.mustAccess()
	if e := a.Prepare(context.Background()); e != nil {
		t.Fatalf("prepare: %v; IMDS=%d caller=%d assume=%d head=%d", e, f.imdsCount.Load(), f.callerCount.Load(), f.assumeCount.Load(), f.headCount.Load())
	}
	p, e := f.a.Execution.Backend.OwnerSessionPolicy()
	if e != nil {
		t.Fatal(e)
	}
	if f.request.Get("Policy") != string(p) || f.request.Get("DurationSeconds") != "3600" || f.request.Get("RoleArn") != executionARN || f.request.Get("RoleSessionName") != "cosmoformer-"+f.a.AttemptID || len(f.request) != 6 {
		t.Fatal("AssumeRole request differs from fixed contract")
	}
	if f.headCount.Load() != 1 || f.assumeCount.Load() != 1 || f.callerCount.Load() != 2 {
		t.Fatal("preparation checks missing")
	}
	for i := 0; i < 2; i++ {
		env, e := a.Environment(context.Background(), a.binding)
		if e != nil || len(env) != 5 || env[0] != "AWS_ACCESS_KEY_ID="+execKey || env[1] != "AWS_SECRET_ACCESS_KEY="+f.secret || env[2] != "AWS_SESSION_TOKEN="+execToken || env[3] != "AWS_REGION=eu-west-1" || env[4] != "AWS_DEFAULT_REGION=eu-west-1" {
			t.Fatal("fixed environment not delivered")
		}
	}
	if e := a.Prepare(context.Background()); e == nil || f.assumeCount.Load() != 1 {
		t.Fatal("session renewed")
	}
	if ms, e := a.RemainingMS(); e != nil || ms <= f.a.RemainingMS || ms >= 3300000 {
		t.Fatal("credential budget not conservative", ms, e)
	}
	// Both bootstrap and execution values have already entered the shared mask.
	for _, value := range []string{bootKey, strings.Repeat("B", 40), bootToken, execKey, f.secret, execToken} {
		for split := 0; split <= len(value); split++ {
			pipe := f.registry.Pipe()
			out := pipe.Feed([]byte("before "+value[:split]), false)
			out = append(out, pipe.Feed([]byte(value[split:]+" after"), true)...)
			if strings.Contains(string(out), value) {
				t.Fatal("credential crossed a read boundary unmasked")
			}
		}
	}
}

func TestBootstrapAndAssumedIdentityFailures(t *testing.T) {
	cases := map[string]func(*fixture){
		"bootstrap role":           func(f *fixture) { f.bootARN = "arn:aws:sts::111122223333:assumed-role/other/i-synthetic" },
		"bootstrap account":        func(f *fixture) { f.bootAccount = "999900001111" },
		"bootstrap ARN account":    func(f *fixture) { f.bootARN = "arn:aws:sts::999900001111:assumed-role/bootstrap/i-synthetic" },
		"bootstrap partition":      func(f *fixture) { f.bootARN = "arn:aws-cn:sts::111122223333:assumed-role/bootstrap/i-synthetic" },
		"bootstrap missing":        func(f *fixture) { f.bootARN = "" },
		"bootstrap user":           func(f *fixture) { f.bootARN = "arn:aws:iam::111122223333:user/bootstrap" },
		"bootstrap path ambiguity": func(f *fixture) { f.bootARN = "arn:aws:sts::111122223333:assumed-role/workers/bootstrap/i-synthetic" },
		"wrong assumed role":       func(f *fixture) { f.assumedARN = "arn:aws:sts::444455556666:assumed-role/other/session" },
		"wrong assumed account":    func(f *fixture) { f.assumedAccount = "111122223333" },
		"wrong assumed partition":  func(f *fixture) { f.assumedARN = strings.Replace(f.assumedARN, "arn:aws:", "arn:aws-cn:", 1) },
		"wrong assumed session":    func(f *fixture) { f.assumedARN += "extra" },
		"missing assumed identity": func(f *fixture) { f.responseARN = "" },
		"mismatched role ID":       func(f *fixture) { f.assumedUserID = "different" },
		"missing role ID":          func(f *fixture) { f.responseUserID = "" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			change(f)
			a := f.mustAccess()
			if a.Prepare(context.Background()) == nil {
				t.Fatal("unexpected identity accepted")
			}
			if env, e := a.Environment(context.Background(), a.binding); e == nil || env != nil {
				t.Fatal("credentials escaped failed preparation")
			}
			if f.headCount.Load() != 0 {
				t.Fatal("ownership checked with unverified identity")
			}
			if strings.HasPrefix(name, "bootstrap") && f.assumeCount.Load() != 0 {
				t.Fatal("unverified bootstrap assumed role")
			}
		})
	}
}

func TestMalformedCredentialsAndNoFallback(t *testing.T) {
	cases := map[string]func(*fixture){
		"empty metadata":           func(f *fixture) { f.roleList = "" },
		"ambiguous metadata":       func(f *fixture) { f.roleList = "bootstrap\nother\n" },
		"wrong metadata role":      func(f *fixture) { f.roleList = "other" },
		"IMDSv2 required":          func(f *fixture) { f.noToken = true },
		"missing bootstrap key":    func(f *fixture) { delete(f.bootCredentials, "AccessKeyId") },
		"malformed bootstrap key":  func(f *fixture) { f.bootCredentials["AccessKeyId"] = "AKIASTATICSTATICSTATIC" },
		"expired bootstrap":        func(f *fixture) { f.bootCredentials["Expiration"] = f.wall },
		"missing bootstrap expiry": func(f *fixture) { delete(f.bootCredentials, "Expiration") },
		"bootstrap error":          func(f *fixture) { f.bootCredentials["Code"] = "Failure" },
		"missing execution key":    func(f *fixture) { f.key = "" },
		"malformed execution key":  func(f *fixture) { f.key = "ASIAinvalid" },
		"missing secret":           func(f *fixture) { f.secret = "" },
		"malformed secret":         func(f *fixture) { f.secret = strings.Repeat("\n", 40) },
		"missing token":            func(f *fixture) { f.token = "" },
		"invalid token":            func(f *fixture) { f.token = "line\nvalue" },
		"missing expiry":           func(f *fixture) { f.expiration = nil },
		"expired session":          func(f *fixture) { v := f.wall; f.expiration = &v },
		"policy refused":           func(f *fixture) { f.denySTS = true },
		"transient STS failure":    func(f *fixture) { f.stsStatus = 500 },
		"uncertain STS response":   func(f *fixture) { f.lostAssume = true },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			change(f)
			a := f.mustAccess()
			e := a.Prepare(context.Background())
			if e != ErrAccess {
				t.Fatal("malformed credentials did not fail safely", e)
			}
			if a.Prepare(context.Background()) == nil || f.assumeCount.Load() > 1 {
				t.Fatal("failed request retried")
			}
			if env, e := a.Environment(context.Background(), a.binding); e == nil || env != nil {
				t.Fatal("failed preparation delivered environment")
			}
			if f.headCount.Load() != 0 {
				t.Fatal("backend checked with invalid session")
			}
			if strings.Contains(e.Error(), execToken) {
				t.Fatal("remote error leaked credentials")
			}
		})
	}
}

func TestOwnershipRegionAndOwnerChange(t *testing.T) {
	for _, scenario := range []string{"wrong owner", "denied", "missing region", "wrong region", "changed owner", "ambiguous region", "ambiguous status"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			a := f.mustAccess()
			if scenario == "changed owner" {
				prepare(t, a)
			}
			switch scenario {
			case "wrong owner", "changed owner":
				f.owner = "000000000000"
			case "denied":
				f.denyHead = true
			case "missing region":
				f.region = ""
			case "wrong region":
				f.region = "us-east-1"
			case "ambiguous region":
				f.duplicateRegion = true
			case "ambiguous status":
				f.headStatus = 204
			}
			if scenario == "changed owner" {
				if env, e := a.Environment(context.Background(), a.binding); e == nil || env != nil {
					t.Fatal("ownership change accepted")
				}
			} else if a.Prepare(context.Background()) == nil {
				t.Fatal("unresolved backend accepted")
			}
			if f.assumeCount.Load() != 1 {
				t.Fatal("ownership failure caused renewal")
			}
			// An initial success never replaces the exact attached per-request policy.
			p, _ := f.a.Execution.Backend.OwnerSessionPolicy()
			if f.request.Get("Policy") != string(p) {
				t.Fatal("owner constraint lost")
			}
		})
	}
}

func TestRequestAnchoredBudgetAndExpiry(t *testing.T) {
	for _, scenario := range []string{"short sufficient", "short insufficient", "delayed response", "delayed beyond attempt", "delayed ownership", "expiry equality", "attempt equality", "clock backward", "clock unavailable", "wall forward", "wall backward"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			switch scenario {
			case "short sufficient":
				v := f.wall.Add(30 * time.Minute)
				f.expiration = &v
			case "short insufficient":
				v := f.wall.Add(24 * time.Minute)
				f.expiration = &v
			case "delayed response":
				f.delayAssume = 3 * time.Minute
			case "delayed beyond attempt":
				f.delayAssume = 21 * time.Minute
			case "delayed ownership":
				f.delayHead = 21 * time.Minute
			}
			a := f.mustAccess()
			e := a.Prepare(context.Background())
			if scenario == "short insufficient" || scenario == "delayed beyond attempt" || scenario == "delayed ownership" {
				if e == nil {
					t.Fatal("exhausted budget accepted")
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			expected := time.Second + clock.Conservative(3600000) - stopMargin
			if scenario == "short sufficient" {
				expected = time.Second + clock.Conservative(1800000) - stopMargin
			}
			if a.deadline != expected {
				t.Fatal("response arrival extended credential budget", a.deadline, expected)
			}
			switch scenario {
			case "expiry equality":
				a.binding.Deadline = a.deadline
				n, _ := f.clock.Now()
				f.clock.advance(a.deadline - n)
			case "attempt equality":
				n, _ := f.clock.Now()
				f.clock.advance(a.binding.Deadline - n)
			case "clock backward":
				f.clock.advance(-time.Second)
			case "clock unavailable":
				f.clock.mu.Lock()
				f.clock.err = ErrAccess
				f.clock.mu.Unlock()
			case "wall forward":
				f.wall = f.wall.Add(2 * time.Hour)
			case "wall backward":
				f.wall = f.wall.Add(-2 * time.Hour)
			default:
				return
			}
			env, e := a.Environment(context.Background(), a.binding)
			if scenario == "wall backward" {
				if e != nil || len(env) != 5 || a.deadline != expected {
					t.Fatal("wall rollback changed fixed authority")
				}
				return
			}
			if e == nil || env != nil {
				t.Fatal("expired or unreliable budget delivered credentials")
			}
			if f.assumeCount.Load() != 1 {
				t.Fatal("budget exhaustion renewed session")
			}
		})
	}
}

func TestBindingReplayCannotExtendOrChangeInstance(t *testing.T) {
	for _, scenario := range []string{"platform", "agent", "worker", "generation", "attempt", "digest", "deadline extension", "shorter then extension"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			a := f.mustAccess()
			prepare(t, a)
			current := a.binding
			switch scenario {
			case "platform":
				current.Platform = runs.ID()
			case "agent":
				current.Agent = runs.ID()
			case "worker":
				current.WorkerID = runs.ID()
			case "generation":
				current.Generation++
			case "attempt":
				current.AttemptID = runs.ID()
			case "digest":
				current.SpecDigest = strings.Repeat("b", 64)
			case "deadline extension":
				current.Deadline++
			case "shorter then extension":
				shorter := current
				shorter.Deadline -= time.Minute
				if _, e := a.Environment(context.Background(), shorter); e != nil {
					t.Fatal(e)
				}
			}
			if env, e := a.Environment(context.Background(), current); e == nil || env != nil {
				t.Fatal("changed binding accepted")
			}
			if env, e := a.Environment(context.Background(), a.binding); e == nil || env != nil {
				t.Fatal("invalidated binding revived")
			}
			if f.assumeCount.Load() != 1 {
				t.Fatal("binding change renewed session")
			}
		})
	}
}

func TestRedactorCapacityAndNoCredentialSerialization(t *testing.T) {
	for _, scenario := range []string{"before bootstrap", "before execution", "bytes"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			if scenario == "before execution" {
				if e := f.registry.Register(bootKey, strings.Repeat("B", 40), bootToken); e != nil {
					t.Fatal(e)
				}
			}
			if scenario == "bytes" {
				if e := f.registry.Register(strings.Repeat("x", 262144)); e != nil {
					t.Fatal(e)
				}
			} else {
				terms := make([]string, 4096)
				for i := range terms {
					terms[i] = fmt.Sprintf("redaction-term-%d", i)
				}
				if scenario == "before execution" {
					terms = terms[:4093]
				}
				if e := f.registry.Register(terms...); e != nil {
					t.Fatal(e)
				}
			}
			a := f.mustAccess()
			if a.Prepare(context.Background()) != ErrAccess {
				t.Fatal("registry overflow accepted")
			}
			if env, e := a.Environment(context.Background(), a.binding); e == nil || env != nil {
				t.Fatal("unregistered credentials released")
			}
			if scenario != "before execution" && f.assumeCount.Load() != 0 {
				t.Fatal("unregistered bootstrap used")
			}
		})
	}
	f := newFixture(t)
	a := f.mustAccess()
	prepare(t, a)
	for _, format := range []string{"%v", "%+v", "%#v"} {
		text := fmt.Sprintf(format, a)
		for _, term := range []string{bootKey, execKey, execToken, f.secret} {
			if strings.Contains(text, term) {
				t.Fatal("format exposed credentials")
			}
		}
	}
	if b, e := json.Marshal(a); e == nil || len(b) != 0 {
		t.Fatal("credential-bearing handle serialized")
	}
	// The assignment remains credential-free and unchanged after preparation.
	b, e := json.Marshal(f.a)
	if e != nil {
		t.Fatal(e)
	}
	for _, term := range []string{bootKey, execKey, execToken, bootToken, f.secret} {
		if strings.Contains(string(b), term) {
			t.Fatal("credential persisted in assignment")
		}
	}
	if runs.Digest(f.a.Execution) != f.a.Digest {
		t.Fatal("assignment mutated")
	}
}

func TestNondefaultWorkspaceRequiresExistingExactState(t *testing.T) {
	for _, status := range []int{200, 403, 404} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newFixture(t)
			s := f.a.Execution
			s.Workspace = "review"
			b, e := source.CanonicalBackend(s.Backend.Attributes, source.BackendContext{Engine: catalog.Engine{Name: s.Engine.Name, Version: s.Engine.Version}, Workspace: s.Workspace, ExpectedOwner: s.Backend.ExpectedOwner}, s.Backend.TableARN)
			if e != nil {
				t.Fatal(e)
			}
			old := s.Backend
			s.Backend = b
			for i, g := range s.Grants {
				for j, claim := range old.Claims {
					if g.Kind == "s3_object" && g.Namespace == claim.Namespace && g.Key == claim.Key {
						s.Grants[i].Key = b.Claims[j].Key
					}
				}
			}
			f.a.Digest = runs.Digest(s)
			previous := f.server.Config.Handler
			calls := 0
			f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "HEAD" && r.URL.Path == "/"+b.Target.Bucket+"/"+b.Target.Key {
					calls++
					if r.Header.Get("X-Amz-Expected-Bucket-Owner") != b.ExpectedOwner {
						t.Error("workspace check omitted owner")
					}
					w.Header().Set("Content-Length", "128")
					w.WriteHeader(status)
					return
				}
				previous.ServeHTTP(w, r)
			})
			a := f.mustAccess()
			e = a.Prepare(context.Background())
			if status == 200 && e != nil || status != 200 && e == nil || calls != 1 {
				t.Fatal("workspace existence not enforced", e, calls)
			}
		})
	}
}
