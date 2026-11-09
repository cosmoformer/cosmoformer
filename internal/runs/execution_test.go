package runs

import (
	"encoding/json"
	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/source"
	"strings"
	"testing"
)

func executionSpecFixture(t *testing.T) ExecutionSpec {
	t.Helper()
	b, e := source.CanonicalBackend(map[string]any{"bucket": "synthetic-bucket", "key": "state", "region": "eu-west-1", "encrypt": true, "use_lockfile": true}, source.BackendContext{Engine: catalog.Engine{Name: "opentofu", Version: "1.13.8"}, Workspace: "default", ExpectedOwner: "111122223333"}, "")
	if e != nil {
		t.Fatal(e)
	}
	pool := ID()
	return ExecutionSpec{Version: 1, Phase: "execution", StackRevisionID: ID(), Source: Source{RepositoryID: ID(), URL: "https://bitbucket.org/fictional/repo.git", Commit: strings.Repeat("a", 40), Root: ".", SubjectID: ID(), FactsRevision: 1}, Engine: Runtime{ID: ID(), Name: "opentofu", Version: "1.13.8", Arch: "amd64", ImageDigest: "sha256:" + strings.Repeat("a", 64)}, Workspace: "default", VarFiles: []string{}, BackendFiles: []string{}, Pool: Pool{ID: pool, Tags: []string{}, GrantRevision: 1}, Backend: b, AWS: ExecutionAWS{AWSRef: AWSRef{ID: ID(), Revision: 1, RoleARN: "arn:aws:iam::111122223333:role/synthetic", Account: "111122223333"}, Bootstrap: "arn:aws:iam::111122223333:role/bootstrap", SessionSeconds: 3600}, Secrets: []SecretRef{}, SourceCredentials: []SourceCredential{}, Grants: []ResourceRef{{ID: ID(), Kind: "pool", ResourceID: pool, Revision: 1}}, Review: ReviewPolicy{Minimum: 1, Revision: 1}, RuleVersion: "synthetic-v1", ProfileID: "synthetic-v1"}
}
func TestExecutionAssignmentClosedAndDistinct(t *testing.T) {
	spec := executionSpecFixture(t)
	if e := spec.Validate(); e != nil {
		t.Fatal(e)
	}
	for _, field := range []string{`,"commands":["sh"]`, `,"env":{"PATH":"private"}`, `,"flags":["-lock=false"]`, `,"aws_connection_ref":{}`} {
		raw, _ := json.Marshal(spec)
		raw = append(raw[:len(raw)-1], []byte(field+"}")...)
		var changed ExecutionSpec
		if Decode(raw, &changed) == nil {
			t.Fatal("execution spec accepted extension", field)
		}
	}
	for _, raw := range []string{`{"ordinal":1,"ordinal":2}`, `{"ordinal":1,"nested":{"k":1,"k":2}}`, `{"ordinal":1} {}`, `{"ordinal":1,"env":{}}`} {
		var p CommandRequest
		if Decode([]byte(raw), &p) == nil {
			t.Fatal("ambiguous payload accepted")
		}
	}
	a := Assignment{Phase: "execution", Execution: &spec, Digest: Digest(spec)}
	b, e := json.Marshal(a)
	if e != nil {
		t.Fatal(e)
	}
	var roundtrip Assignment
	if e = Decode(b, &roundtrip); e != nil {
		t.Fatal(e)
	}
	if roundtrip.Execution == nil || Digest(roundtrip.Execution) != a.Digest {
		t.Fatal("execution spec downgraded to preflight")
	}
	var pre PreflightSpec
	if Decode(b, &pre) == nil {
		t.Fatal("different shape decoded as preflight")
	}
}
func TestFixedLogicalCommandSequence(t *testing.T) {
	s := executionSpecFixture(t)
	pre := strings.Repeat("b", 64)
	sealed := strings.Repeat("c", 64)
	for i, command := range []string{"init", "schema", "plan", "show"} {
		r := CommandRequest{Ordinal: int64(i + 1), Command: command, PreInitDigest: pre}
		if i > 0 {
			r.ManifestDigest = sealed
		}
		d, e := Command(s, "plan", r)
		if e != nil {
			t.Fatal(e)
		}
		if d.Root != "." || d.Runtime != s.Engine.ID || d.Ordinal != r.Ordinal || len(FixedFlags(command)) == 0 || !ValidDigest(Digest(d)) {
			t.Fatal("descriptor", d)
		}
		changed := d
		changed.Workspace = "other"
		if Digest(changed) == Digest(d) {
			t.Fatal("workspace not bound")
		}
		r.Command = "sh -c plan"
		if _, e = Command(s, "plan", r); e == nil {
			t.Fatal("shell fragment accepted")
		}
	}
	if _, e := Command(s, "plan", CommandRequest{Ordinal: 1, Command: "init", PreInitDigest: pre, ManifestDigest: sealed}); e == nil {
		t.Fatal("init accepted sealed digest")
	}
	if _, e := Command(s, "plan", CommandRequest{Ordinal: 3, Command: "plan", PreInitDigest: pre}); e == nil {
		t.Fatal("unsealed plan")
	}
}

func TestPermissionResponseRoundtrip(t *testing.T) {
	in := Transition{Accepted: true, NextSeq: 4, AttemptState: "permitted", OperationStatus: "executing", Permission: &Permission{ID: ID(), Ordinal: 1, Digest: strings.Repeat("a", 64), Platform: ID(), RemainingMS: 9900}}
	b, e := json.Marshal(in)
	if e != nil {
		t.Fatal(e)
	}
	var out Transition
	if e = Decode(b, &out); e != nil {
		t.Fatal(e)
	}
	if out.Permission == nil || *out.Permission != *in.Permission {
		t.Fatalf("permission lost in replay: %s", b)
	}
}

func TestExecutionSpecRejectsUntrustedInputMetadata(t *testing.T) {
	cases := map[string]struct {
		change func(*ExecutionSpec)
		code   string
	}{
		"unsupported runtime":        {func(s *ExecutionSpec) { s.Engine.Version = "latest" }, "RUNTIME_UNSUPPORTED"},
		"wrong phase":                {func(s *ExecutionSpec) { s.Phase = "preflight" }, "INVALID_SPEC"},
		"untrusted source URL":       {func(s *ExecutionSpec) { s.Source.URL = "https://attacker.example/repo.git" }, "INVALID_SPEC"},
		"missing secrets collection": {func(s *ExecutionSpec) { s.Secrets = nil }, "INVALID_SPEC"},
		"too many secrets":           {func(s *ExecutionSpec) { s.Secrets = make([]SecretRef, 65) }, "INVALID_SPEC"},
		"too many files":             {func(s *ExecutionSpec) { s.VarFiles = make([]string, 33) }, "INVALID_SPEC"},
		"duplicate file":             {func(s *ExecutionSpec) { s.VarFiles = []string{"vars.tfvars", "vars.tfvars"} }, "INVALID_SPEC"},
		"escaped file":               {func(s *ExecutionSpec) { s.BackendFiles = []string{"../outside"} }, "INVALID_SPEC"},
		"unversioned secret": {func(s *ExecutionSpec) {
			s.Secrets = []SecretRef{{ID: ID(), Revision: 0, Destination: "TF_VAR_value", Scope: "org"}}
		}, "INVALID_SPEC"},
		"reserved secret destination": {func(s *ExecutionSpec) {
			s.Secrets = []SecretRef{{ID: ID(), Revision: 1, Destination: "PATH", Scope: "org"}}
		}, "INVALID_SPEC"},
		"duplicate destination": {func(s *ExecutionSpec) {
			r := SecretRef{ID: ID(), Revision: 1, Destination: "TF_VAR_value", Scope: "org"}
			s.Secrets = []SecretRef{r, r}
		}, "INVALID_SPEC"},
		"duplicate grant":  {func(s *ExecutionSpec) { s.Grants = append(s.Grants, s.Grants[0]) }, "INVALID_SPEC"},
		"pool not granted": {func(s *ExecutionSpec) { s.Grants = []ResourceRef{} }, "INVALID_SPEC"},
		"unversioned credential": {func(s *ExecutionSpec) {
			s.SourceCredentials = []SourceCredential{{ID: ID(), RepositoryID: s.Source.RepositoryID, Revision: 0}}
		}, "INVALID_SPEC"},
		"backend substitution": {func(s *ExecutionSpec) { s.Backend.Target.Key = "other" }, "BACKEND_MISMATCH"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s := executionSpecFixture(t)
			c.change(&s)
			requireRunCode(t, s.Validate(), c.code)
		})
	}
	s := executionSpecFixture(t)
	s.VarFiles = []string{"vars.tfvars"}
	s.BackendFiles = []string{"backend.hcl"}
	s.Secrets = []SecretRef{{ID: ID(), Revision: 3, Destination: "TF_VAR_value", Scope: "project"}}
	s.SourceCredentials = []SourceCredential{{ID: ID(), RepositoryID: s.Source.RepositoryID, Revision: 2}}
	if e := s.Validate(); e != nil {
		t.Fatal(e)
	}
	got := s.ExpectedSecrets()
	if len(got) != 1 || got[0].Revision != 3 || got[0].Destination != "TF_VAR_value" {
		t.Fatal(got)
	}
	if Digest(make(chan int)) != "" {
		t.Fatal("uncanonical input was assigned a digest")
	}
}

func TestClosedExecutionPayloadAndCommandStages(t *testing.T) {
	for _, route := range []string{"ack", "prepare-result", "manifest-seal", "start-permission", "command-events", "result", "controls/ack"} {
		if _, e := ExecutionPayload(route, json.RawMessage(`{}`)); e != nil {
			t.Fatal(route, e)
		}
		if _, e := ExecutionPayload(route, json.RawMessage(`{"private":"not allowed"}`)); e == nil {
			t.Fatal("unknown member accepted", route)
		}
	}
	if _, e := ExecutionPayload("exec", json.RawMessage(`{}`)); e == nil {
		t.Fatal("arbitrary route accepted")
	}
	s := executionSpecFixture(t)
	pre := strings.Repeat("a", 64)
	sealed := strings.Repeat("b", 64)
	d, e := Command(s, "apply", CommandRequest{Ordinal: 2, Command: "apply", PreInitDigest: pre, ManifestDigest: sealed})
	if e != nil || d.Output != "console" {
		t.Fatal(d, e)
	}
	for _, c := range []struct {
		kind string
		r    CommandRequest
		code string
	}{
		{"destroy", CommandRequest{Ordinal: 1, Command: "init"}, "COMMAND_UNSUPPORTED"},
		{"plan", CommandRequest{Ordinal: 0, Command: "init"}, "COMMAND_ORDER"},
		{"plan", CommandRequest{Ordinal: 5, Command: "plan"}, "COMMAND_ORDER"},
		{"apply", CommandRequest{Ordinal: 2, Command: "plan"}, "COMMAND_ORDER"},
		{"plan", CommandRequest{Ordinal: 1, Command: "init", PreInitDigest: "short"}, "MANIFEST_STAGE"},
	} {
		_, e := Command(s, c.kind, c.r)
		requireRunCode(t, e, c.code)
	}
}

func TestPermissionResponseRejectsPartialAndUnknownWireFields(t *testing.T) {
	for _, raw := range []string{`{`, `{"unknown":true}`, `{"accepted":true,"permission_id":"partial"}`, `{"accepted":true,"permission_id":"id","ordinal":"bad","command_spec_digest":"d","platform_instance":"p","remaining_ms":1,"display_expires_at":"2026-01-01T00:00:00Z"}`} {
		var out Transition
		if json.Unmarshal([]byte(raw), &out) == nil {
			t.Fatal("invalid permission accepted", raw)
		}
	}
	out := Transition{Permission: &Permission{ID: ID()}}
	if e := json.Unmarshal([]byte(`{"accepted":true}`), &out); e != nil || out.Permission != nil {
		t.Fatal("stale permission persisted", out, e)
	}
}
