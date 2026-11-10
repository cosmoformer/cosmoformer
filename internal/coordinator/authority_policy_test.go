package coordinator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/source"
)

func authorityFixture(t *testing.T) (runs.Assignment, []policyRow) {
	t.Helper()
	pool, repo, project, env := runs.ID(), runs.ID(), runs.ID(), runs.ID()
	b, e := source.CanonicalBackend(map[string]any{"bucket": "synthetic-bucket", "key": "state", "region": "eu-west-1", "encrypt": true, "use_lockfile": true}, source.BackendContext{Engine: catalog.Engine{Name: "opentofu", Version: "1.13.8"}, Workspace: "default", ExpectedOwner: "111122223333"}, "")
	if e != nil {
		t.Fatal(e)
	}
	s := runs.ExecutionSpec{Version: 1, Phase: "execution", StackRevisionID: runs.ID(), Source: runs.Source{RepositoryID: repo, URL: "https://bitbucket.org/fictional/repo.git", Commit: strings.Repeat("a", 40), Root: ".", SubjectID: runs.ID(), FactsRevision: 1}, Engine: runs.Runtime{ID: runs.ID(), Name: "opentofu", Version: "1.13.8", Arch: "amd64", ImageDigest: "sha256:" + strings.Repeat("a", 64)}, Workspace: "default", VarFiles: []string{}, BackendFiles: []string{}, Pool: runs.Pool{ID: pool, Tags: []string{}, GrantRevision: 1}, Backend: b, AWS: runs.ExecutionAWS{AWSRef: runs.AWSRef{ID: runs.ID(), Revision: 1, RoleARN: "arn:aws:iam::111122223333:role/fixture", Account: "111122223333"}, Bootstrap: "arn:aws:iam::111122223333:role/bootstrap", SessionSeconds: 3600}, Secrets: []runs.SecretRef{{ID: runs.ID(), Revision: 1, Scope: "project", Destination: "TF_VAR_value"}}, SourceCredentials: []runs.SourceCredential{{ID: runs.ID(), RepositoryID: repo, Revision: 1}}, Grants: []runs.ResourceRef{{ID: runs.ID(), Kind: "pool", ResourceID: pool, Revision: 1}, {ID: runs.ID(), Kind: "repository", ResourceID: repo, Revision: 1}, {ID: runs.ID(), Kind: "s3_object", Namespace: "aws/synthetic-bucket", Key: "state", Revision: 1}, {ID: runs.ID(), Kind: "s3_object", Namespace: "aws/synthetic-bucket", Key: "state.tflock", Revision: 1}}, Review: runs.ReviewPolicy{Minimum: 1, Revision: 1}, RuleVersion: "fixture-v1", ProfileID: "fixture-v1"}
	s.Grants = append(s.Grants, runs.ResourceRef{ID: runs.ID(), Kind: "secret", ResourceID: s.Secrets[0].ID, Revision: 1})
	if e := s.Validate(); e != nil {
		t.Fatal(e)
	}
	// Storage response shapes include the ordinary preflight authority, exact
	// execution metadata, current automation scope, credentials and binding.
	rows := []policyRow{row(true), row("", project, env, repo, true), row(true), row(true), row(true), row(int64(1), s.Source.Commit)}
	for range s.Grants {
		rows = append(rows, row(true))
	}
	rows = append(rows, row(true))
	rows = append(rows, row(project, env, "", repo, "plan", 1, int64(1)), row(true))
	for range s.Grants {
		rows = append(rows, row(true))
	}
	rows = append(rows, row(true), row(true), row(true), row(true))
	return runs.Assignment{Kind: "plan", Execution: &s}, rows
}
func TestAuthorityCannotBeGrantedWithAnyUnreadableCurrentFact(t *testing.T) {
	a, rows := authorityFixture(t)
	c := policyCoordinator()
	tx := &policyTx{rows: append([]policyRow{}, rows...)}
	if e := c.executionAuthority(context.Background(), tx, a); e != nil || len(tx.rows) != 0 {
		t.Fatal("valid current authority fixture rejected", e, len(tx.rows))
	}
	// Inject an outage at every storage boundary. The invariant is independent of
	// the SQL spelling: unreadable current facts never yield positive authority.
	for i := range rows {
		t.Run(string(rune('A'+i)), func(t *testing.T) {
			failure := errors.New("current authority store unavailable")
			answers := append([]policyRow{}, rows...)
			answers[i] = policyRow{err: failure}
			tx := &policyTx{rows: answers}
			if e := c.executionAuthority(context.Background(), tx, a); e == nil {
				t.Fatal("unreadable current authority granted execution")
			}
			if tx.writes != 0 {
				t.Fatal("authority verification mutated permanent claims")
			}
		})
	}
}
func TestAuthorityRejectsChangedGrantMetadataWorkerAndBindingFacts(t *testing.T) {
	a, rows := authorityFixture(t)
	for i, answer := range rows {
		if len(answer.values) != 1 {
			continue
		}
		if _, ok := answer.values[0].(bool); !ok {
			continue
		}
		t.Run(string(rune('A'+i)), func(t *testing.T) {
			answers := append([]policyRow{}, rows...)
			answers[i] = row(false)
			tx := &policyTx{rows: answers}
			if e := policyCoordinator().executionAuthority(context.Background(), tx, a); e == nil {
				t.Fatal("changed current fact retained launch authority")
			}
			if tx.writes != 0 {
				t.Fatal("invalid authority wrote claims")
			}
		})
	}
}
func TestUnimplementedExecutionModesFailBeforeReadingStorage(t *testing.T) {
	a, _ := authorityFixture(t)
	c := policyCoordinator()
	codeIs(t, c.executionAuthority(context.Background(), &policyTx{}, runs.Assignment{}), "INVALID_SPEC")
	a.Kind = "apply"
	codeIs(t, c.executionAuthority(context.Background(), &policyTx{}, a), "APPLY_PREREQUISITES_UNAVAILABLE")
	a.Kind = "plan"
	attributes := map[string]any{}
	for k, v := range a.Execution.Backend.Attributes {
		attributes[k] = v
	}
	attributes["kms_key_id"] = "arn:aws:kms:eu-west-1:111122223333:key/11111111-1111-4111-8111-111111111111"
	b, e := source.CanonicalBackend(attributes, source.BackendContext{Engine: catalog.Engine{Name: "opentofu", Version: "1.13.8"}, Workspace: "default", ExpectedOwner: "111122223333"}, "")
	if e != nil {
		t.Fatal(e)
	}
	a.Execution.Backend = b
	codeIs(t, c.executionAuthority(context.Background(), &policyTx{}, a), "KMS_GRANT_PREREQUISITES_UNAVAILABLE")
}
