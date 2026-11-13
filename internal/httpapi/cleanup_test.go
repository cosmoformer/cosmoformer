//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/runs"
)

type cleanupTestStorage struct{}

func (cleanupTestStorage) Check(context.Context) error { return nil }
func (cleanupTestStorage) Put(context.Context, string, io.Reader, int64) (string, error) {
	panic("unexpected upload")
}
func (cleanupTestStorage) Get(context.Context, string, string) (evidence.Object, error) {
	panic("unexpected read")
}
func (cleanupTestStorage) Delete(context.Context, string, string) error { panic("unexpected deletion") }

func TestCleanupAdministrationCurrentAuthorityReplayAndSettings(t *testing.T) {
	f := newSourceFixture(t)
	ctx := context.Background()
	settings := evidence.DefaultCleanupSettings()
	settings.Batch = 2
	f.Handler, _ = NewWithOptions(f.DB.App, f.Server.Origin, Options{Cleanup: settings, Evidence: &evidence.Custody{Storage: cleanupTestStorage{}, Bucket: "synthetic"}})
	response := f.browser(t, "GET", "/api/v1/admin/settings", nil, 200)
	var current OperationalSettings
	if e := json.Unmarshal(response.Body.Bytes(), &current); e != nil || current.Cleanup.Enabled || current.Cleanup.Batch != 2 || current.Cleanup.RetentionDays != 30 {
		t.Fatal(current, e)
	}
	for _, sensitive := range []string{"dsn", "bucket", "owner", "credential", "secret"} {
		if strings.Contains(response.Body.String(), sensitive) {
			t.Fatal("unsafe settings projection", sensitive)
		}
	}
	f.browser(t, "GET", "/api/v1/admin/cleanup-preview?org_id="+f.Org+"&outcome=eligible&limit=1", nil, 200)
	f.browser(t, "GET", "/api/v1/admin/cleanup-preview?org_id="+f.Org+"&cursor=invalid", nil, 422)
	f.browser(t, "GET", "/api/v1/admin/cleanup-preview?org_id="+f.Org+"&outcome=invalid", nil, 422)
	f.browser(t, "GET", "/api/v1/admin/cleanup-preview?org_id="+f.Org+"&limit=101", nil, 422)
	f.browser(t, "PATCH", "/api/v1/admin/settings", map[string]any{"cleanup_enabled": true}, 405)
	body := map[string]any{"org_id": f.Org, "limit": 2}
	key := runs.ID()
	first := f.browserKey(t, "POST", "/api/v1/admin/cleanup", body, key, 200)
	again := f.browserKey(t, "POST", "/api/v1/admin/cleanup", body, key, 200)
	if first.Body.String() != again.Body.String() {
		t.Fatal("cleanup replay changed claim set")
	}
	var events int
	if e := f.DB.Owner.QueryRow(ctx, `SELECT count(*) FROM cosmoformer.audit_events WHERE action='cleanup.requested'`).Scan(&events); e != nil || events != 1 {
		t.Fatal("not one audited action", events, e)
	}
	f.browserKey(t, "POST", "/api/v1/admin/cleanup", map[string]any{"org_id": f.Org, "limit": 1}, key, 409)
	f.browser(t, "POST", "/api/v1/admin/cleanup", map[string]any{"org_id": f.Org, "limit": 3}, 422)
	f.browser(t, "POST", "/api/v1/admin/cleanup", map[string]any{"org_id": f.Org, "limit": 0}, 422)
	if _, e := f.DB.Owner.Exec(ctx, `UPDATE cosmoformer.installation SET maintenance=true`); e != nil {
		t.Fatal(e)
	}
	f.browser(t, "POST", "/api/v1/admin/cleanup", body, 503)
	if _, e := f.DB.Owner.Exec(ctx, `INSERT INTO cosmoformer.accounts(username,display_name,installation_admin) VALUES('other-admin','Other',true); UPDATE cosmoformer.accounts SET installation_admin=false WHERE username='fixture'`); e != nil {
		t.Fatal(e)
	}
	// A previously accepted receipt cannot bypass current installation authority.
	f.browserKey(t, "POST", "/api/v1/admin/cleanup", body, key, 403)
	f.browser(t, "GET", "/api/v1/admin/settings", nil, 403)
	f.browser(t, "GET", "/api/v1/admin/cleanup-preview?org_id="+f.Org, nil, 403)
}
func TestCleanupSchedulingCancellationAndTenantWindow(t *testing.T) {
	f := newSourceFixture(t)
	ctx := context.Background()
	f.Server.Cleanup = evidence.DefaultCleanupSettings()
	f.Server.Evidence = &evidence.Custody{Storage: cleanupTestStorage{}, Bucket: "synthetic"}
	// Bounded discovery makes progress even for organizations without repositories.
	if _, e := f.DB.Owner.Exec(ctx, `INSERT INTO cosmoformer.organizations(name) SELECT 'Cleanup fixture '||g FROM generate_series(1,105) g`); e != nil {
		t.Fatal(e)
	}
	cursor := f.Server.cleanupTick(ctx, "")
	if cursor == "" {
		t.Fatal("tenant discovery was not paginated")
	}
	if next := f.Server.cleanupTick(ctx, cursor); next != "" {
		t.Fatal("tenant discovery did not wrap", next)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if next := f.Server.cleanupTick(cancelled, cursor); next != cursor {
		t.Fatal("cancellation advanced tenant cursor")
	}
	f.Server.cleanupJobs(cancelled)
}
