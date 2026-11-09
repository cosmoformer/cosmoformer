//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/jackc/pgx/v5"
)

func TestAdminRejectedChangesPreserveMembershipsTeamsAndAudit(t *testing.T) {
	f := newSourceFixture(t)
	base := "/api/v1/organizations/" + f.Org
	create := f.browser(t, "POST", base+"/teams", map[string]any{"name": "Synthetic reviewers"}, 200)
	var team struct {
		ID string `json:"id"`
	}
	if e := json.Unmarshal(create.Body.Bytes(), &team); e != nil {
		t.Fatal(e)
	}
	var before int
	if e := f.DB.Owner.QueryRow(context.Background(), `SELECT count(*) FROM cosmoformer.audit_events`).Scan(&before); e != nil {
		t.Fatal(e)
	}
	for _, c := range []struct {
		method, path string
		body         any
		status       int
	}{
		{"POST", "/api/v1/admin/accounts", map[string]any{"username": "bad user", "display_name": "Synthetic"}, 422},
		{"POST", base + "/invitations", map[string]any{"username": "missing-account", "org_admin": false}, 404},
		{"POST", base + "/invitations", map[string]any{"username": "fixture", "org_admin": false}, 409},
		{"POST", base + "/invitations/" + runs.ID() + "/accept", map[string]any{}, 404},
		{"POST", base + "/teams", map[string]any{"name": "Synthetic reviewers"}, 409},
		{"PATCH", base + "/teams/" + team.ID, map[string]any{"name": "Changed", "revision": 99}, 409},
		{"POST", base + "/teams/" + team.ID + "/members", map[string]any{"account_id": runs.ID()}, 409},
		{"DELETE", base + "/teams/" + team.ID + "/members/" + runs.ID(), nil, 404},
		{"PATCH", base + "/members/" + f.Account, map[string]any{"revision": 1, "org_admin": false}, 409},
		{"PATCH", base + "/members/" + runs.ID(), map[string]any{"revision": 1, "org_admin": true}, 409},
		{"DELETE", base + "/members/" + f.Account, nil, 409},
		{"DELETE", base + "/members/" + runs.ID(), nil, 404},
		{"DELETE", base + "/grants/" + runs.ID(), nil, 404},
		{"POST", base + "/projects/" + runs.ID() + "/environments", map[string]any{"name": "Unowned project"}, 409},
	} {
		t.Run(c.method+" "+c.path, func(t *testing.T) { f.browser(t, c.method, c.path, c.body, c.status) })
	}
	var after int
	if e := f.DB.Owner.QueryRow(context.Background(), `SELECT count(*) FROM cosmoformer.audit_events`).Scan(&after); e != nil || after != before {
		t.Fatal("rejected changes produced committed audit events", before, after, e)
	}
}
func TestConfigurationAbsenceAndKeyFailureCannotCommitCredentials(t *testing.T) {
	f := newSourceFixture(t)
	base := "/api/v1/organizations/" + f.Org
	m, e := catalog.Parse(f.Remote.Manifest)
	if e != nil {
		t.Fatal(e)
	}
	s := m.Stacks[0]
	for _, resource := range []string{"worker-pools", "repositories", "aws-connections", "integrations", "resource-grants"} {
		f.browser(t, "PATCH", base+"/"+resource+"/"+runs.ID(), map[string]any{"expected_revision": 1, "enabled": false}, 409)
	}
	for _, action := range []string{"pause", "resume", "credentials"} {
		f.browser(t, "POST", base+"/workers/"+runs.ID()+"/"+action, map[string]any{"expected_revision": 1}, 409)
	}
	f.browser(t, "POST", base+"/workers", map[string]any{"pool_id": s.Worker.ID, "name": "invalid tag", "tags": []string{"BAD TAG"}}, 422)
	f.browser(t, "POST", base+"/source-jobs/"+runs.ID()+"/retry", map[string]any{"expected_revision": 1}, 409)
	created := f.browser(t, "POST", base+"/secrets", map[string]any{"project_id": s.ProjectID, "name": "Synthetic retained value", "value": "synthetic-test-value"}, 200)
	var secret struct {
		ID string `json:"id"`
	}
	if e := json.Unmarshal(created.Body.Bytes(), &secret); e != nil {
		t.Fatal(e)
	}
	originalHandler := f.Handler
	f.Handler, _ = NewWithOptions(f.DB.App, f.Server.Origin, Options{Clock: elapsedTest{}, Bitbucket: f.Server.Bitbucket})
	for _, c := range []struct {
		path string
		body any
	}{
		{base + "/secrets", map[string]any{"name": "missing key", "value": "synthetic-value"}},
		{base + "/repositories/" + f.Repo + "/source-credentials", map[string]any{"expected_revision": 0, "username": "fixture", "token": "synthetic-token"}},
	} {
		f.browser(t, "POST", c.path, c.body, 503)
	}
	f.browser(t, "PATCH", base+"/secrets/"+secret.ID, map[string]any{"expected_revision": 1, "enabled": false}, 409)
	f.Handler = originalHandler
	f.browser(t, "PATCH", base+"/secrets/"+secret.ID, map[string]any{"expected_revision": 99, "enabled": false}, 409)
	// A retained ciphertext that cannot be authenticated must not be re-encrypted.
	f.exec(t, `UPDATE cosmoformer.secrets SET encrypted_value=decode('00','hex') WHERE id=$1`, secret.ID)
	f.browser(t, "PATCH", base+"/secrets/"+secret.ID, map[string]any{"expected_revision": 1, "enabled": false}, 503)
	var revision int64
	if e := f.DB.Owner.QueryRow(context.Background(), `SELECT revision FROM cosmoformer.secrets WHERE id=$1`, secret.ID).Scan(&revision); e != nil || revision != 1 {
		t.Fatal("failed key operation changed revision", revision, e)
	}
}

// Simulate unavailable storage relations only inside this fixture's disposable
// database. Restore each relation before inspecting durable state or cleanup.
func TestAdminStorageFailuresCannotCommitPartialChanges(t *testing.T) {
	f := newSourceFixture(t)
	base := "/api/v1/organizations/" + f.Org
	m, e := catalog.Parse(f.Remote.Manifest)
	if e != nil {
		t.Fatal(e)
	}
	project := m.Stacks[0].ProjectID
	created := f.browser(t, "POST", base+"/teams", map[string]any{"name": "Original team"}, 200)
	var team struct {
		ID string `json:"id"`
	}
	if e := json.Unmarshal(created.Body.Bytes(), &team); e != nil {
		t.Fatal(e)
	}
	f.browser(t, "POST", base+"/teams/"+team.ID+"/members", map[string]any{"account_id": f.Account}, 200)
	var before int
	if e := f.DB.Owner.QueryRow(context.Background(), `SELECT count(*) FROM cosmoformer.audit_events`).Scan(&before); e != nil {
		t.Fatal(e)
	}
	for _, c := range []struct {
		table, method, path string
		body                any
	}{
		{"teams", "POST", base + "/teams", map[string]any{"name": "Must roll back"}},
		{"teams", "PATCH", base + "/teams/" + team.ID, map[string]any{"name": "Must roll back", "revision": 1}},
		{"team_members", "POST", base + "/teams/" + team.ID + "/members", map[string]any{"account_id": f.Account}},
		{"team_members", "DELETE", base + "/teams/" + team.ID + "/members/" + f.Account, nil},
		{"grants", "POST", base + "/grants", map[string]any{"team_id": team.ID, "project_id": project, "role": "viewer"}},
		{"grants", "DELETE", base + "/grants/" + runs.ID(), nil},
		{"projects", "POST", base + "/projects", map[string]any{"name": "Must roll back"}},
		{"environments", "POST", base + "/projects/" + project + "/environments", map[string]any{"name": "Must roll back"}},
		{"worker_pools", "GET", base + "/configuration", nil},
		{"outbox", "GET", base + "/configuration", nil},
		{"outbox", "POST", base + "/source-jobs/" + runs.ID() + "/retry", map[string]any{"expected_revision": 1}},
		{"worker_pools", "PATCH", base + "/worker-pools/" + runs.ID(), map[string]any{"expected_revision": 1, "enabled": false}},
		{"workers", "POST", base + "/workers/" + runs.ID() + "/pause", map[string]any{"expected_revision": 1}},
	} {
		t.Run(c.table+" "+c.method+" "+c.path, func(t *testing.T) {
			table := pgx.Identifier{"cosmoformer", c.table}.Sanitize()
			offline := pgx.Identifier{"cosmoformer", c.table + "_unavailable"}.Sanitize()
			f.exec(t, "ALTER TABLE "+table+" RENAME TO "+pgx.Identifier{c.table + "_unavailable"}.Sanitize())
			defer f.exec(t, "ALTER TABLE "+offline+" RENAME TO "+pgx.Identifier{c.table}.Sanitize())
			f.browser(t, c.method, c.path, c.body, 503)
		})
	}
	var name string
	var after int
	if e := f.DB.Owner.QueryRow(context.Background(), `SELECT name,(SELECT count(*) FROM cosmoformer.audit_events) FROM cosmoformer.teams WHERE id=$1`, team.ID).Scan(&name, &after); e != nil || name != "Original team" || before != after {
		t.Fatal("storage failure committed a partial administrative mutation", name, before, after, e)
	}
}
