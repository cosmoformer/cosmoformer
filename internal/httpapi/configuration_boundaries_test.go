//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/cryptostore"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"go.yaml.in/yaml/v3"
)

func TestConfigurationValidationAndRevisionFences(t *testing.T) {
	f := newSourceFixture(t)
	m, e := catalog.Parse(f.Remote.Manifest)
	if e != nil {
		t.Fatal(e)
	}
	s := m.Stacks[0]
	base := "/api/v1/organizations/" + f.Org
	for _, body := range []map[string]any{
		{"project_id": s.ProjectID, "kind": "s3_object", "namespace": "aws/fictional-bucket"},
		{"project_id": s.ProjectID, "kind": "ddb_table", "namespace": "arn:aws:dynamodb:eu-west-1:111122223333:table/fictional", "resource_id": runs.ID()},
		{"project_id": s.ProjectID, "kind": "repository", "resource_id": f.Repo, "namespace": "unexpected"},
		{"project_id": s.ProjectID, "kind": "pool"},
	} {
		f.browser(t, "POST", base+"/resource-grants", body, 422)
	}
	f.browser(t, "POST", base+"/resource-grants", map[string]any{"project_id": s.ProjectID, "kind": "repository", "resource_id": runs.ID()}, 404)
	f.browser(t, "POST", base+"/resource-grants", map[string]any{"project_id": s.ProjectID, "kind": "ddb_table", "namespace": "arn:aws:dynamodb:eu-west-1:111122223333:table/fictional"}, 200)
	f.browser(t, "POST", base+"/aws-connections", map[string]any{"project_id": s.ProjectID, "role_arn": "arn:aws:iam::999900001111:role/fictional", "expected_account": "111122223333"}, 422)
	f.browser(t, "POST", base+"/repositories", map[string]any{"project_id": s.ProjectID, "integration_id": f.Integration, "external_id": "fixture", "url": "https://attacker.example/repo.git", "tracked_branch": "main", "enabled": true}, 422)
	f.browser(t, "POST", base+"/workers", map[string]any{"name": "Synthetic", "pool_id": s.Worker.ID, "tags": []string{"Invalid tag"}}, 422)
	f.browser(t, "POST", base+"/secrets", map[string]any{"name": "Synthetic", "value": "invalid\x00secret"}, 422)
	f.browser(t, "POST", base+"/repositories/"+f.Repo+"/source-credentials", map[string]any{"expected_revision": 0, "username": "fixture", "token": "injected\ncredential"}, 422)
	for _, resource := range []struct{ path, id string }{{"worker-pools", s.Worker.ID}, {"repositories", f.Repo}, {"integrations", f.Integration}} {
		path := base + "/" + resource.path + "/" + resource.id
		f.browser(t, "PATCH", path, map[string]any{"expected_revision": 1, "enabled": false}, 200)
		f.browser(t, "PATCH", path, map[string]any{"expected_revision": 1, "enabled": true}, 409)
		f.browser(t, "PATCH", path, map[string]any{"expected_revision": 2, "enabled": true}, 200)
	}
	f.exec(t, `UPDATE cosmoformer.memberships SET org_admin=false WHERE org_id=$1 AND account_id=$2`, f.Org, f.Account)
	f.exec(t, `UPDATE cosmoformer.accounts SET installation_admin=false WHERE id=$1`, f.Account)
	f.browser(t, "GET", base+"/configuration", nil, 404)
	f.browser(t, "PATCH", base+"/worker-pools/"+s.Worker.ID, map[string]any{"expected_revision": 3, "enabled": false}, 404)
}

func TestManagedSecretReferencesRemainRevisionedAndScopeBound(t *testing.T) {
	f := newSourceFixture(t)
	m, e := catalog.Parse(f.Remote.Manifest)
	if e != nil {
		t.Fatal(e)
	}
	s := m.Stacks[0]
	base := "/api/v1/organizations/" + f.Org
	created := f.browser(t, "POST", base+"/secrets", map[string]any{"project_id": s.ProjectID, "name": "Synthetic project secret", "value": "fictional-private-value"}, 200)
	var secret struct {
		ID string `json:"id"`
	}
	if e := json.Unmarshal(created.Body.Bytes(), &secret); e != nil {
		t.Fatal(e)
	}
	s.Secrets = []catalog.Secret{{Env: "TF_VAR_password", Source: catalog.SecretSource{Scope: "project", ID: secret.ID}}}
	f.Remote.Manifest, _ = yaml.Marshal(catalog.Manifest{Version: 1, Stacks: []catalog.Stack{s}})
	sync := base + "/repositories/" + f.Repo + "/sync"
	f.browser(t, "POST", sync, map[string]any{"expected_revision": 1}, 403)
	f.browser(t, "POST", base+"/resource-grants", map[string]any{"project_id": s.ProjectID, "kind": "secret", "resource_id": secret.ID}, 200)
	// A project secret cannot be relabeled as an organization secret in Git.
	s.Secrets[0].Source.Scope = "org"
	f.Remote.Manifest, _ = yaml.Marshal(catalog.Manifest{Version: 1, Stacks: []catalog.Stack{s}})
	f.browser(t, "POST", sync, map[string]any{"expected_revision": 1}, 403)
	s.Secrets[0].Source.Scope = "project"
	f.Remote.Manifest, _ = yaml.Marshal(catalog.Manifest{Version: 1, Stacks: []catalog.Stack{s}})
	f.browser(t, "POST", sync, map[string]any{"expected_revision": 1}, 200)
	var frozen []byte
	if e := f.DB.Owner.QueryRow(context.Background(), `SELECT effective_config FROM cosmoformer.stack_revisions ORDER BY created_at DESC LIMIT 1`).Scan(&frozen); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(frozen), "fictional-private-value") || !strings.Contains(string(frozen), secret.ID) {
		t.Fatal("source snapshot lost reference isolation")
	}
	f.browser(t, "PATCH", base+"/secrets/"+secret.ID, map[string]any{"expected_revision": 1, "enabled": false}, 200)
	var semantic int64
	var sealed []byte
	if e := f.DB.Owner.QueryRow(context.Background(), `SELECT semantic_revision,encrypted_value FROM cosmoformer.secrets WHERE id=$1`, secret.ID).Scan(&semantic, &sealed); e != nil {
		t.Fatal(e)
	}
	raw, e := f.Server.Cipher.Open(sealed, cryptostore.AAD(f.Org, secret.ID, "managed_secret", uint64(semantic)))
	if e != nil || string(raw) != "fictional-private-value" || semantic != 2 {
		t.Fatal("disable did not reseal existing value at a new revision", e)
	}
	f.browser(t, "PATCH", base+"/secrets/"+secret.ID, map[string]any{"expected_revision": 2, "enabled": true, "value": "invalid\x00value"}, 422)
	f.browser(t, "PATCH", base+"/secrets/"+runs.ID(), map[string]any{"expected_revision": 1, "enabled": true, "value": "fictional"}, 404)
}

func TestEncryptedSettingsRejectNoncanonicalPathIDs(t *testing.T) {
	f := newSourceFixture(t)
	base := "/api/v1/organizations/" + f.Org
	f.configure(t)
	created := f.browser(t, "POST", base+"/secrets", map[string]any{"name": "Synthetic secret", "value": "fictional-private-value"}, 200)
	var secret struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &secret); err != nil || secret.ID == "" {
		t.Fatal("secret fixture missing", err)
	}
	ctx := context.Background()
	const snapshot = `SELECT jsonb_build_object('credentials',(SELECT jsonb_agg(to_jsonb(c)) FROM cosmoformer.source_credentials c),'secrets',(SELECT jsonb_agg(to_jsonb(s)) FROM cosmoformer.secrets s),'integrations',(SELECT jsonb_agg(to_jsonb(i)) FROM cosmoformer.integrations i),'audit',(SELECT count(*) FROM cosmoformer.audit_events))`
	var before []byte
	if err := f.DB.Owner.QueryRow(ctx, snapshot).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for _, form := range []struct {
		name string
		id   func(string) string
	}{
		{"uppercase", strings.ToUpper},
		{"braced", func(id string) string { return "{" + id + "}" }},
		{"compact", func(id string) string { return strings.ReplaceAll(id, "-", "") }},
	} {
		t.Run(form.name, func(t *testing.T) {
			org := "/api/v1/organizations/" + form.id(f.Org)
			if org == base || form.id(f.Integration) == f.Integration || form.id(secret.ID) == secret.ID {
				t.Skip("fixture identifier contains no letters")
			}
			f.browser(t, "POST", org+"/repositories/"+f.Repo+"/source-credentials", map[string]any{"expected_revision": 0, "username": "fixture", "token": "fictional-git-token"}, 422)
			f.browser(t, "POST", org+"/secrets", map[string]any{"name": "Invalid path write", "value": "fictional-private-value"}, 422)
			for _, path := range []string{org + "/integrations/" + f.Integration, base + "/integrations/" + form.id(f.Integration)} {
				f.browser(t, "POST", path+"/credentials", map[string]any{"expected_revision": 2, "client_id": "fictional-client", "client_secret": "fictional-secret", "webhook_secret": strings.Repeat("w", 32), "hook_uuid": f.Hook}, 422)
			}
			f.browser(t, "PATCH", base+"/secrets/"+form.id(secret.ID), map[string]any{"expected_revision": 1, "enabled": true, "value": "fictional-replacement"}, 422)
		})
	}
	var after []byte
	if err := f.DB.Owner.QueryRow(ctx, snapshot).Scan(&after); err != nil || string(before) != string(after) {
		t.Fatal("rejected identifier changed encrypted settings", err)
	}
	// Canonical readers can still decrypt the unchanged integration and secret.
	f.connect(t)
	f.browser(t, "PATCH", base+"/secrets/"+secret.ID, map[string]any{"expected_revision": 1, "enabled": false}, 200)
}
