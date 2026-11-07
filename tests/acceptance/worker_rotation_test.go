//go:build integration

package acceptance

import (
	"context"
	"testing"

	"github.com/cosmoformer/cosmoformer/internal/coordinator"
	"github.com/cosmoformer/cosmoformer/internal/runs"
)

func TestWorkerCredentialRotationRevokesOldTokenAndPausesWorker(t *testing.T) {
	f := newPreflightFixture(t)
	ctx := context.Background()
	if e := f.agent.Admit(ctx); e != nil {
		t.Fatal(e)
	}
	var revision int64
	if e := f.db.Owner.QueryRow(ctx, `SELECT revision FROM cosmoformer.workers WHERE id=$1`, f.workerID).Scan(&revision); e != nil {
		t.Fatal(e)
	}
	path := "/organizations/" + f.org + "/workers/" + f.workerID
	rotated := f.admin.call("POST", path+"/credentials", map[string]any{"expected_revision": revision}, 200)
	replacement := rotated["token"].(string)
	if replacement == f.token || rotated["paused"] != true {
		t.Fatal("replacement reused token or remained active")
	}
	c := coordinator.New(f.db.App, f.ticks)
	if e := c.Heartbeat(ctx, f.token, f.agent.Identity); e == nil {
		t.Fatal("old worker token retained authentication")
	}
	if _, e := c.Admit(ctx, replacement, f.agent.Admission); runs.Code(e) != "WORKER_PAUSED" {
		t.Fatal("rotated worker bypassed pause", e)
	}
	f.admin.call("POST", path+"/credentials", map[string]any{"expected_revision": revision}, 409)
	f.admin.call("POST", path+"/resume", map[string]any{"expected_revision": revision + 1}, 200)
	if _, e := c.Admit(ctx, replacement, f.agent.Admission); e != nil {
		t.Fatal("current credential could not re-admit", e)
	}
	var now int64
	if e := f.db.Owner.QueryRow(ctx, `SELECT revision FROM cosmoformer.workers WHERE id=$1`, f.workerID).Scan(&now); e != nil {
		t.Fatal(e)
	}
	f.admin.call("POST", path+"/pause", map[string]any{"expected_revision": now}, 200)
	f.admin.call("POST", path+"/resume", map[string]any{"expected_revision": now}, 409)
}
