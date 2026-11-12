package awsaccess

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/smithy-go/logging"
	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/source"
)

func TestProductionConstructorIsInertAndIgnoresAmbientAWS(t *testing.T) {
	for _, key := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_PROFILE", "AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE", "AWS_EC2_METADATA_SERVICE_ENDPOINT", "AWS_ENDPOINT_URL", "AWS_ENDPOINT_URL_STS", "AWS_ENDPOINT_URL_S3", "HTTP_PROXY", "HTTPS_PROXY"} {
		t.Setenv(key, "http://untrusted.invalid")
	}
	f := newFixture(t)
	a, e := New(bootstrapARN, f.a, f.worker, time.Second, f.registry)
	if e != nil {
		t.Fatal(e)
	}
	if a.used || f.imdsCount.Load() != 0 || f.assumeCount.Load() != 0 || f.headCount.Load() != 0 {
		t.Fatal("constructor performed I/O")
	}
	c := aws.Credentials{AccessKeyID: execKey, SecretAccessKey: strings.Repeat("E", 40), SessionToken: execToken}
	s := a.clients.sts(c, "eu-west-1").Options()
	b := a.clients.s3(c, "eu-west-1").Options()
	if s.BaseEndpoint != nil || b.BaseEndpoint != nil || b.UsePathStyle || b.UseAccelerate || !aws.ToBool(b.DisableS3ExpressSessionAuth) {
		t.Fatal("nonstandard endpoint/session feature enabled")
	}
	for _, p := range []aws.CredentialsProvider{s.Credentials, b.Credentials} {
		for i := 0; i < 2; i++ {
			v, e := p.Retrieve(context.Background())
			if e != nil || v.AccessKeyID != execKey || v.SecretAccessKey != c.SecretAccessKey || v.SessionToken != execToken {
				t.Fatal("ambient credentials selected")
			}
		}
	}
	for _, logger := range []logging.Logger{s.Logger, b.Logger} {
		if _, ok := logger.(logging.Nop); !ok {
			t.Fatal("credential adapter can emit SDK logs")
		}
	}
	h, ok := s.HTTPClient.(*http.Client)
	if !ok {
		t.Fatal("missing controlled HTTP client")
	}
	transport, ok := h.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil || h.CheckRedirect == nil || h.Timeout <= 0 {
		t.Fatal("ambient proxy/redirect/timeout settings used")
	}
	// Replace only the transport in this test. Exercise production endpoint
	// resolution without allowing a packet to leave the loopback fixture.
	base := f.server.Client().Transport
	h.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.Method {
		case "PUT", "GET":
			if r.URL.Scheme != "http" || r.URL.Host != "169.254.169.254" {
				t.Error("IMDS endpoint overridden")
			}
		case "POST":
			if r.URL.Scheme != "https" || r.URL.Host != "sts.eu-west-1.amazonaws.com" {
				t.Error("STS endpoint overridden")
			}
		case "HEAD":
			if r.URL.Scheme != "https" || r.URL.Host != "synthetic-state.s3.eu-west-1.amazonaws.com" {
				t.Error("S3 endpoint overridden")
			}
		default:
			t.Error("unexpected production request")
		}
		req := r.Clone(r.Context())
		u := *r.URL
		req.URL = &u
		req.URL.Scheme = "http"
		req.URL.Host = strings.TrimPrefix(f.server.URL, "http://")
		if r.Method == "HEAD" {
			req.URL.Path = "/synthetic-state"
		}
		return base.RoundTrip(req)
	})
	a.clock = f.clock
	a.wall = func() time.Time { return f.wall }
	prepare(t, a)
	if f.assumeCount.Load() != 1 || f.headCount.Load() != 1 {
		t.Fatal("production session checks missing")
	}
	if _, e := New("arn:aws:iam::111122223333:role/other", f.a, f.worker, time.Second, f.registry); e == nil {
		t.Fatal("local expected role ignored")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestInvalidAuthorityFailsBeforeAWS(t *testing.T) {
	cases := map[string]func(*fixture){
		"source only":                         func(f *fixture) { f.a.Phase = "preflight" },
		"apply lacks exact retained evidence": func(f *fixture) { f.a.Kind = "apply" },
		"no spec":                             func(f *fixture) { f.a.Execution = nil },
		"untrusted digest":                    func(f *fixture) { f.a.Digest = strings.Repeat("a", 64) },
		"missing platform":                    func(f *fixture) { f.a.Platform = "" },
		"missing agent":                       func(f *fixture) { f.worker.Agent = "" },
		"expired attempt":                     func(f *fixture) { f.a.RemainingMS = 0 },
		"incompatible cap":                    func(f *fixture) { f.a.CapSeconds = 3600 },
		"remaining exceeds cap":               func(f *fixture) { f.a.RemainingMS = f.a.CapSeconds*1000 + 1 },
		"missing registry":                    func(f *fixture) { f.registry = nil },
		"wrong runtime":                       func(f *fixture) { f.a.RuntimeID = runs.ID() },
		"wrong role account":                  func(f *fixture) { f.a.Execution.AWS.Account = "111122223333" },
		"wrong role partition": func(f *fixture) {
			f.a.Execution.AWS.RoleARN = strings.Replace(executionARN, "arn:aws:", "arn:aws-cn:", 1)
		},
		"assumed role as target": func(f *fixture) {
			f.a.Execution.AWS.RoleARN = "arn:aws:sts::444455556666:assumed-role/execution/session"
		},
		"wrong bootstrap metadata": func(f *fixture) { f.a.Execution.AWS.Bootstrap = "arn:aws:iam::111122223333:role/other" },
		"missing expected owner":   func(f *fixture) { f.a.Execution.Backend.ExpectedOwner = "" },
		"missing exact lock grant": func(f *fixture) { f.a.Execution.Grants = f.a.Execution.Grants[:len(f.a.Execution.Grants)-1] },
		"wrong key grant":          func(f *fixture) { f.a.Execution.Grants[len(f.a.Execution.Grants)-1].Key = "another.tflock" },
		"KMS still blocked": func(f *fixture) {
			f.a.Execution.Backend.Attributes["kms_key_id"] = "arn:aws:kms:eu-west-1:444455556666:key/synthetic-key"
			f.rebuildBackend(t)
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			change(f)
			if name != "untrusted digest" && f.a.Execution != nil {
				f.a.Digest = runs.Digest(f.a.Execution)
			}
			if a, e := f.access(); e == nil || a != nil {
				t.Fatal("invalid assignment accepted")
			}
			if f.imdsCount.Load() != 0 || f.assumeCount.Load() != 0 || f.headCount.Load() != 0 {
				t.Fatal("invalid authority contacted AWS")
			}
		})
	}
}

func (f *fixture) rebuildBackend(t *testing.T) {
	t.Helper()
	s := f.a.Execution
	b, e := source.CanonicalBackend(s.Backend.Attributes, source.BackendContext{Engine: catalog.Engine{Name: s.Engine.Name, Version: s.Engine.Version}, Workspace: s.Workspace, ExpectedOwner: s.Backend.ExpectedOwner}, s.Backend.TableARN)
	if e != nil {
		t.Fatal(e)
	}
	s.Backend = b
	f.a.Digest = runs.Digest(s)
}

func TestExactDynamoDBGrantPreserved(t *testing.T) {
	f := newFixture(t)
	s := f.a.Execution
	s.Backend.Attributes["dynamodb_table"] = "synthetic-locks"
	s.Backend.TableARN = "arn:aws:dynamodb:eu-west-1:444455556666:table/synthetic-locks"
	f.rebuildBackend(t)
	if _, e := f.access(); e == nil {
		t.Fatal("missing table grant accepted")
	}
	s.Grants = append(s.Grants, runs.ResourceRef{ID: runs.ID(), Revision: 1, Kind: "ddb_table", Namespace: s.Backend.TableARN})
	f.a.Digest = runs.Digest(s)
	a := f.mustAccess()
	prepare(t, a)
	// No DynamoDB calls or changed grant namespaces accompany session creation.
	if f.assumeCount.Load() != 1 || f.headCount.Load() != 1 {
		t.Fatal("unexpected backend operations")
	}
}

func TestPreparationIsSingleUseUnderConcurrency(t *testing.T) {
	f := newFixture(t)
	a := f.mustAccess()
	var wg sync.WaitGroup
	errors := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Go(func() { errors <- a.Prepare(context.Background()) })
	}
	wg.Wait()
	close(errors)
	success := 0
	for e := range errors {
		if e == nil {
			success++
		}
	}
	if success != 1 || f.assumeCount.Load() != 1 {
		t.Fatal("concurrent preparation renewed credentials")
	}
}

func TestAttemptDeadlineCannotBeRestartedAtResponseReceipt(t *testing.T) {
	f := newFixture(t)
	a := f.mustAccess()
	f.clock.advance(21 * time.Minute)
	if a.Prepare(context.Background()) == nil || f.imdsCount.Load() != 0 {
		t.Fatal("delayed poll reply created fresh attempt budget")
	}
	if env, e := a.Environment(context.Background(), a.binding); e == nil || env != nil {
		t.Fatal("expired attempt released credentials")
	}
	var zero Access
	if zero.Prepare(context.Background()) == nil {
		t.Fatal("zero handle accepted")
	}
	if _, e := newAccess(bootstrapARN, f.a, f.worker, time.Second, &evidence.Registry{}, nil, time.Now, clients{}); e == nil {
		t.Fatal("missing clock accepted")
	}
}

func TestAssignedAuthorityIsCopiedBeforePreparation(t *testing.T) {
	f := newFixture(t)
	a := f.mustAccess()
	binding := a.binding
	f.a.Execution.AWS.RoleARN = "arn:aws:iam::444455556666:role/retargeted"
	f.a.Execution.Backend.ExpectedOwner = "000000000000"
	f.a.Execution.Backend.Target.Bucket = "another-state-bucket"
	f.a.Execution.Backend.Attributes["bucket"] = "another-state-bucket"
	f.a.Platform = runs.ID()
	f.a.Digest = runs.Digest(f.a.Execution)
	prepare(t, a)
	if _, e := a.Environment(context.Background(), binding); e != nil {
		t.Fatal(e)
	}
	if f.request.Get("RoleArn") != executionARN || !strings.Contains(f.request.Get("Policy"), "777788889999") || strings.Contains(f.request.Get("Policy"), "another-state-bucket") {
		t.Fatal("assignment mutation retargeted credentials")
	}
}

func TestSavedPlanApplyUsesTheSameFixedOwnerSession(t *testing.T) {
	f := newFixture(t)
	p := &runs.RetainedPlan{ID: runs.ID(), Revision: 2, ManifestDigest: strings.Repeat("a", 64), Artifacts: []string{}, Records: []runs.RetainedArtifact{}}
	for _, kind := range []string{"pre_init_spec", "manifest", "binary_plan", "raw_plan_json", "provider_schema", "normalized_changes", "findings"} {
		id := runs.ID()
		p.Artifacts = append(p.Artifacts, id)
		p.Records = append(p.Records, runs.RetainedArtifact{ID: id, Kind: kind, Hash: p.ManifestDigest, Size: 1, Version: "synthetic-exact-version"})
	}
	f.a.Kind = "apply"
	f.a.Execution.RetainedPlan = p
	f.a.Digest = runs.Digest(f.a.Execution)
	f.a.CapSeconds = 2400
	f.a.RemainingMS = 2400000
	a, e := f.access()
	if e != nil {
		t.Fatal("complete Apply assignment rejected", e)
	}
	prepare(t, a)
	binding := a.binding
	env, e := a.Environment(context.Background(), binding)
	if e != nil || len(env) == 0 || f.assumeCount.Load() != 1 || f.headCount.Load() != 2 || f.request.Get("Policy") != a.policy || f.request.Get("DurationSeconds") != "3600" {
		t.Fatal("Apply did not use the fixed checked session", e)
	}
	if e = a.Prepare(context.Background()); e == nil || f.assumeCount.Load() != 1 {
		t.Fatal("Apply renewed its session")
	}
}
