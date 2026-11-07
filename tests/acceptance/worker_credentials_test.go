//go:build integration

package acceptance

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cosmoformer/cosmoformer/internal/runs"
)

func TestWorkerCredentialCapabilityIsBoundToAssignmentAndRevision(t *testing.T) {
	f := newPreflightFixture(t)
	if e := f.agent.Admit(context.Background()); e != nil {
		t.Fatal(e)
	}
	var polled struct {
		Assignment *runs.Assignment `json:"assignment"`
	}
	raw := f.workerCall(t, "/poll", runs.Poll{Identity: f.agent.Identity, RequestID: runs.ID(), Wait: 20}, 200)
	if e := json.Unmarshal(raw, &polled); e != nil || polled.Assignment == nil {
		t.Fatal("no assignment", e)
	}
	a := polled.Assignment
	body := map[string]any{"worker_id": f.workerID, "generation": f.agent.Identity.Generation, "agent_instance": f.agent.Identity.Agent, "request_id": runs.ID(), "repository_id": f.repo, "canonical_https_url": "https://bitbucket.org/fictional/repo.git"}
	path := "/attempts/" + a.AttemptID + "/source-credential"
	raw = f.workerCall(t, path, body, 200)
	var got struct {
		Username, Token string
		Revision        int64 `json:"credential_revision"`
	}
	if e := json.Unmarshal(raw, &got); e != nil || got.Username != "x-token-auth" || got.Token != "synthetic-repository-only" || got.Revision != 1 {
		t.Fatal("assignment credential unavailable", e)
	}
	body["repository_id"] = runs.ID()
	f.workerCall(t, path, body, 403)
	body["repository_id"] = f.repo
	body["canonical_https_url"] = "https://bitbucket.org/fictional/other.git"
	f.workerCall(t, path, body, 403)
	body["canonical_https_url"] = "https://bitbucket.org/fictional/repo.git"
	body["generation"] = f.agent.Identity.Generation + 1
	f.workerCall(t, path, body, 409)
	body["generation"] = f.agent.Identity.Generation
	f.admin.call("POST", "/organizations/"+f.org+"/repositories/"+f.repo+"/source-credentials", map[string]any{"expected_revision": 1, "username": "x-token-auth", "token": "synthetic-replacement-only"}, 200)
	f.workerCall(t, path, body, 409)
	metadata := f.admin.call("GET", "/organizations/"+f.org+"/configuration", nil, 200)
	encoded, e := json.Marshal(metadata)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(encoded), "synthetic-repository-only") || strings.Contains(string(encoded), "synthetic-replacement-only") {
		t.Fatal("credential exposed in metadata")
	}
}

func TestNoncanonicalCredentialWriteLeavesWorkerCredentialUsable(t *testing.T) {
	f := newPreflightFixture(t)
	org := strings.ToUpper(f.org)
	if org == f.org {
		t.Skip("fixture identifier contains no letters")
	}
	f.admin.call("POST", "/organizations/"+org+"/repositories/"+f.repo+"/source-credentials", map[string]any{"expected_revision": 1, "username": "x-token-auth", "token": "fictional-replacement"}, 422)
	if err := f.agent.Admit(context.Background()); err != nil {
		t.Fatal(err)
	}
	var polled struct {
		Assignment *runs.Assignment `json:"assignment"`
	}
	raw := f.workerCall(t, "/poll", runs.Poll{Identity: f.agent.Identity, RequestID: runs.ID(), Wait: 20}, 200)
	if err := json.Unmarshal(raw, &polled); err != nil || polled.Assignment == nil {
		t.Fatal("no assignment", err)
	}
	raw = f.workerCall(t, "/attempts/"+polled.Assignment.AttemptID+"/source-credential", map[string]any{"worker_id": f.workerID, "generation": f.agent.Identity.Generation, "agent_instance": f.agent.Identity.Agent, "request_id": runs.ID(), "repository_id": f.repo, "canonical_https_url": polled.Assignment.Source.URL}, 200)
	var got struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &got); err != nil || got.Token != "synthetic-repository-only" {
		t.Fatal("canonical worker lost its original credential", err)
	}
}
