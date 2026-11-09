package worker

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/source"
)

func inputAssignment(t *testing.T, count int) runs.Assignment {
	t.Helper()
	pool := runs.ID()
	repository := runs.ID()
	backend, e := source.CanonicalBackend(map[string]any{"bucket": "synthetic-bucket", "key": "state", "region": "eu-west-1", "encrypt": true, "use_lockfile": true}, source.BackendContext{Engine: catalog.Engine{Name: "opentofu", Version: "1.13.8"}, Workspace: "default", ExpectedOwner: "111122223333"}, "")
	if e != nil {
		t.Fatal(e)
	}
	s := runs.ExecutionSpec{Version: 1, Phase: "execution", StackRevisionID: runs.ID(), Source: runs.Source{RepositoryID: repository, URL: "https://bitbucket.org/fictional/repo.git", Commit: strings.Repeat("a", 40), Root: ".", SubjectID: runs.ID(), FactsRevision: 1}, Engine: runs.Runtime{ID: runs.ID(), Name: "opentofu", Version: "1.13.8", Arch: "amd64", ImageDigest: "sha256:" + strings.Repeat("a", 64)}, Workspace: "default", VarFiles: []string{}, BackendFiles: []string{}, Pool: runs.Pool{ID: pool, Tags: []string{}, GrantRevision: 1}, Backend: backend, AWS: runs.ExecutionAWS{AWSRef: runs.AWSRef{ID: runs.ID(), Revision: 1, RoleARN: "arn:aws:iam::111122223333:role/fixture", Account: "111122223333"}, Bootstrap: "arn:aws:iam::111122223333:role/bootstrap", SessionSeconds: 3600}, Secrets: []runs.SecretRef{}, SourceCredentials: []runs.SourceCredential{}, Grants: []runs.ResourceRef{{ID: runs.ID(), Kind: "pool", ResourceID: pool, Revision: 1}}, Review: runs.ReviewPolicy{Minimum: 1, Revision: 1}, RuleVersion: "fixture-v1", ProfileID: "fixture-v1"}
	for i := 0; i < count; i++ {
		s.Secrets = append(s.Secrets, runs.SecretRef{ID: runs.ID(), Scope: "org", Revision: 1, Destination: fmt.Sprintf("TF_VAR_input%d", i)})
	}
	if e := s.Validate(); e != nil {
		t.Fatal(e)
	}
	return runs.Assignment{AttemptID: runs.ID(), Phase: "execution", Execution: &s, Digest: runs.Digest(s)}
}
func inputAgent(response runs.ExecutionInputValues) *Agent {
	return &Agent{Origin: "https://fixture.invalid", HTTP: &http.Client{Transport: logTransport(func(*http.Request) (*http.Response, error) { return reply(200, response), nil })}}
}
func TestExecutionInputValuesAreNotReleasedBeforeValidationAndRedactionRegistration(t *testing.T) {
	for _, scenario := range []string{"wrong digest", "missing secret", "wrong revision", "empty secret", "oversized secret", "NUL secret", "redactor full", "unbounded numeric scalar", "oversized environment"} {
		t.Run(scenario, func(t *testing.T) {
			count := 1
			if scenario == "oversized environment" {
				count = 9
			}
			a := inputAssignment(t, count)
			values := runs.ExecutionInputValues{SpecDigest: a.Digest, Secrets: []runs.EnvSecret{}}
			for _, r := range a.Execution.ExpectedSecrets() {
				values.Secrets = append(values.Secrets, runs.EnvSecret{ExpectedSecret: r, Value: "synthetic-input-value"})
			}
			registry := &evidence.Registry{}
			switch scenario {
			case "wrong digest":
				values.SpecDigest = strings.Repeat("b", 64)
			case "missing secret":
				values.Secrets = nil
			case "wrong revision":
				values.Secrets[0].Revision++
			case "empty secret":
				values.Secrets[0].Value = ""
			case "oversized secret":
				values.Secrets[0].Value = strings.Repeat("x", 16385)
			case "NUL secret":
				values.Secrets[0].Value = "private\x00value"
			case "redactor full":
				if e := registry.Register(strings.Repeat("z", 262144)); e != nil {
					t.Fatal(e)
				}
			case "unbounded numeric scalar":
				values.Secrets[0].Value = `{"value":1e99999999}`
			case "oversized environment":
				for i := range values.Secrets {
					values.Secrets[i].Value = strings.Repeat("x", 16384)
				}
			}
			got, e := inputAgent(values).FetchExecutionInputs(context.Background(), a, registry)
			if e == nil || got != nil {
				t.Fatal("unvalidated values were released", e)
			}
		})
	}
}
func TestStructuredVariableLeavesAreProtectedBeforeValuesAreReturned(t *testing.T) {
	a := inputAssignment(t, 1)
	value := `{"list":["nested-value","",false,true,{"number":9.1e1}],"nothing":null}`
	values := runs.ExecutionInputValues{SpecDigest: a.Digest, Secrets: []runs.EnvSecret{{ExpectedSecret: a.Execution.ExpectedSecrets()[0], Value: value}}}
	registry := &evidence.Registry{}
	got, e := inputAgent(values).FetchExecutionInputs(context.Background(), a, registry)
	if e != nil || len(got) != 1 || got[0].Value != value {
		t.Fatal(e)
	}
	for _, leaf := range []string{"nested-value", "false", "true", "91", "9.1e1"} {
		text := string(registry.Pipe().Feed([]byte(leaf), true))
		if text != strings.Repeat("*", len(leaf)) {
			t.Fatal("unprotected scalar", leaf, text)
		}
	}
}
