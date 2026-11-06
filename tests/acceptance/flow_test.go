//go:build integration

package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/httpapi"
	"github.com/cosmoformer/cosmoformer/internal/identity"
	"github.com/cosmoformer/cosmoformer/internal/testsupport"
)

const origin = "https://localhost:5173"
const password = "a fictional testing password 2026"

type client struct {
	t                 *testing.T
	base, token, csrf string
}

func (c *client) call(method, path string, body any, status int) map[string]any {
	return c.callKey(method, path, body, status, identity.Token())
}
func (c *client) callKey(method, path string, body any, status int, key string) map[string]any {
	c.t.Helper()
	raw, _ := json.Marshal(body)
	r, _ := http.NewRequest(method, c.base+"/api/v1"+path, bytes.NewReader(raw))
	if method != "GET" {
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", origin)
		r.Header.Set("X-Cosmoformer-Request", "browser")
		r.Header.Set("Idempotency-Key", key)
		r.Header.Set("X-CSRF-Token", c.csrf)
	}
	if c.token != "" {
		r.AddCookie(&http.Cookie{Name: "__Host-cosmoformer_session", Value: c.token})
	}
	response, e := http.DefaultClient.Do(r)
	if e != nil {
		c.t.Fatal(e)
	}
	defer response.Body.Close()
	b, _ := io.ReadAll(response.Body)
	if response.StatusCode != status {
		c.t.Fatalf("%s %s: got %d, want %d; %s", method, path, response.StatusCode, status, b)
	}
	for _, cookie := range response.Cookies() {
		if cookie.Name == "__Host-cosmoformer_session" {
			c.token = cookie.Value
			if !cookie.Secure || !cookie.HttpOnly || cookie.Path != "/" || cookie.Domain != "" || cookie.SameSite != http.SameSiteLaxMode {
				c.t.Fatal("unsafe session cookie")
			}
		}
	}
	var out map[string]any
	if e = json.Unmarshal(b, &out); e != nil {
		c.t.Fatal(e, string(b))
	}
	return out
}
func (c *client) login(name string) {
	c.call("POST", "/session/login", map[string]any{"username": name, "password": password}, 200)
	s := c.call("GET", "/session", nil, 200)
	c.csrf = s["csrf_token"].(string)
}
func fixture(t *testing.T) (*testsupport.Database, *client) {
	t.Helper()
	db := testsupport.NewDatabase(t)
	ctx := context.Background()
	h := identity.NewHasher()
	phc, e := h.Hash(ctx, password)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Owner.Exec(ctx, `INSERT INTO cosmoformer.accounts(username,display_name,installation_admin,password_phc) VALUES('admin','Administrator',true,$1)`, phc); e != nil {
		t.Fatal(e)
	}
	handler, _ := httpapi.New(db.App, origin)
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	c := &client{t: t, base: s.URL}
	c.login("admin")
	return db, c
}
func id(m map[string]any) string { return m["id"].(string) }
func TestAccountsAndCurrentScopedAccess(t *testing.T) {
	db, admin := fixture(t)
	ctx := context.Background()
	employeeID := id(admin.call("POST", "/admin/accounts", map[string]any{"username": "engineer", "display_name": "Engineer"}, 200))
	key := identity.Token()
	code := admin.callKey("POST", "/admin/accounts/"+employeeID+"/setup-code", map[string]any{}, 200, key)["code"].(string)
	admin.callKey("POST", "/admin/accounts/"+employeeID+"/setup-code", map[string]any{}, 409, key)
	var retained bool
	if e := db.Owner.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.browser_requests WHERE result::text LIKE '%'||$1||'%')`, code).Scan(&retained); e != nil || retained {
		t.Fatal("setup code retained in idempotency result", e)
	}
	employee := &client{t: t, base: admin.base}
	employee.call("POST", "/account/setup", map[string]any{"code": code, "password": password}, 200)
	employee.login("engineer")
	s := employee.call("GET", "/session", nil, 200)
	if len(s["organizations"].([]any)) != 0 {
		t.Fatal("new account has organization access")
	}
	employee.call("GET", "/admin/accounts", nil, 403)
	org := id(admin.call("POST", "/organizations", map[string]any{"name": "Example engineering"}, 200))
	base := "/organizations/" + org
	invite := id(admin.call("POST", base+"/invitations", map[string]any{"username": "engineer", "org_admin": false}, 200))
	admin.call("POST", base+"/invitations/"+invite+"/accept", map[string]any{}, 404)
	employee.call("POST", base+"/invitations/"+invite+"/accept", map[string]any{}, 200)
	key = identity.Token()
	project := id(admin.callKey("POST", base+"/projects", map[string]any{"name": "Payments"}, 200, key))
	if got := id(admin.callKey("POST", base+"/projects", map[string]any{"name": "Payments"}, 200, key)); got != project {
		t.Fatal("idempotent create returned another project")
	}
	admin.callKey("POST", base+"/projects", map[string]any{"name": "Changed"}, 409, key)
	dev := id(admin.call("POST", base+"/projects/"+project+"/environments", map[string]any{"name": "Development"}, 200))
	prod := id(admin.call("POST", base+"/projects/"+project+"/environments", map[string]any{"name": "Production"}, 200))
	if len(employee.call("GET", base+"/projects", nil, 200)["projects"].([]any)) != 0 {
		t.Fatal("membership granted project discovery")
	}
	team := id(admin.call("POST", base+"/teams", map[string]any{"name": "Engineers"}, 200))
	admin.call("POST", base+"/teams/"+team+"/members", map[string]any{"account_id": employeeID}, 200)
	grant := id(admin.call("POST", base+"/grants", map[string]any{"team_id": team, "project_id": project, "environment_id": dev, "role": "viewer"}, 200))
	p := employee.call("GET", base+"/projects", nil, 200)["projects"].([]any)
	if len(p) != 1 {
		t.Fatal("missing scoped project")
	}
	envs := p[0].(map[string]any)["environments"].([]any)
	if len(envs) != 1 || id(envs[0].(map[string]any)) != dev {
		t.Fatal("production leaked through development grant")
	}
	employee.call("GET", base+"/access", nil, 404)
	admin.call("DELETE", base+"/grants/"+grant, map[string]any{}, 200)
	if len(employee.call("GET", base+"/projects", nil, 200)["projects"].([]any)) != 0 {
		t.Fatal("grant revocation was not immediate")
	}
	// An environment administrator cannot grant project-wide or sibling authority.
	authority := id(admin.call("POST", base+"/grants", map[string]any{"team_id": team, "project_id": project, "environment_id": dev, "role": "administrator"}, 200))
	employee.call("POST", base+"/grants", map[string]any{"team_id": team, "project_id": project, "role": "deployer"}, 404)
	employee.call("POST", base+"/grants", map[string]any{"team_id": team, "project_id": project, "environment_id": prod, "role": "deployer"}, 404)
	employee.call("POST", base+"/teams", map[string]any{"name": "Escalation"}, 404)
	// Cached mutation responses still require current scoped authority.
	replayKey := identity.Token()
	replayBody := map[string]any{"team_id": team, "project_id": project, "environment_id": dev, "role": "viewer"}
	employee.callKey("POST", base+"/grants", replayBody, 200, replayKey)
	admin.call("DELETE", base+"/grants/"+authority, map[string]any{}, 200)
	employee.callKey("POST", base+"/grants", replayBody, 404, replayKey)
	admin.call("PATCH", base+"/members/"+employeeID, map[string]any{"revision": 1, "org_admin": true}, 200)
	employee.call("GET", base+"/access", nil, 200)
	admin.call("PATCH", base+"/members/"+employeeID, map[string]any{"revision": 2, "org_admin": false}, 200)
	employee.call("GET", base+"/access", nil, 404)
	admin.call("DELETE", base+"/members/"+employeeID, map[string]any{}, 200)
	employee.call("GET", base+"/projects", nil, 404)
	// A guessed organization cannot reveal directory/project data.
	employee.call("GET", "/organizations/99999999-9999-4999-8999-999999999999/projects", nil, 404)
	accounts := admin.call("GET", "/admin/accounts", nil, 200)["accounts"].([]any)
	var revision float64
	for _, v := range accounts {
		a := v.(map[string]any)
		if a["id"] == employeeID {
			revision = a["revision"].(float64)
		}
	}
	admin.call("PATCH", "/admin/accounts/"+employeeID, map[string]any{"revision": revision, "display_name": "Engineer", "enabled": false, "installation_admin": false}, 200)
	employee.call("GET", "/session", nil, 401)
}
func TestResetOneUseAndSessionExpiry(t *testing.T) {
	db, admin := fixture(t)
	ctx := context.Background()
	s := admin.call("GET", "/session", nil, 200)
	account := s["actor"].(map[string]any)["id"].(string)
	code := admin.call("POST", "/admin/accounts/"+account+"/reset-code", map[string]any{}, 200)["code"].(string)
	// A successful reset also clears the account's temporary login throttle.
	if _, e := db.Owner.Exec(ctx, `INSERT INTO cosmoformer.auth_throttles(subject,failures,window_at,blocked_until) VALUES($1,5,clock_timestamp(),clock_timestamp()+interval '15 minutes')`, identity.Digest("login/admin")); e != nil {
		t.Fatal(e)
	}
	visitor := &client{t: t, base: admin.base}
	visitor.call("POST", "/account/reset", map[string]any{"code": code, "password": password}, 200)
	visitor.call("POST", "/account/reset", map[string]any{"code": code, "password": password}, 422)
	admin.call("GET", "/session", nil, 401)
	visitor.login("admin")
	// Reads never renew activity; intentional activity cannot extend the absolute cap.
	var before, after string
	if e := db.Owner.QueryRow(ctx, `SELECT last_activity_at::text FROM cosmoformer.sessions LIMIT 1`).Scan(&before); e != nil {
		t.Fatal(e)
	}
	visitor.call("GET", "/session", nil, 200)
	if e := db.Owner.QueryRow(ctx, `SELECT last_activity_at::text FROM cosmoformer.sessions LIMIT 1`).Scan(&after); e != nil || before != after {
		t.Fatal("background read renewed session", e)
	}
	if _, e := db.Owner.Exec(ctx, `UPDATE cosmoformer.sessions SET last_activity_at=clock_timestamp()-interval '61 minutes'`); e != nil {
		t.Fatal(e)
	}
	visitor.call("POST", "/session/activity", map[string]any{}, 401)
	visitor.login("admin")
	if _, e := db.Owner.Exec(ctx, `UPDATE cosmoformer.sessions SET absolute_expires_at=clock_timestamp()-interval '1 second'`); e != nil {
		t.Fatal(e)
	}
	visitor.call("GET", "/session", nil, 401)
}
func TestCSRFAndSafeErrors(t *testing.T) {
	_, c := fixture(t)
	for _, originValue := range []string{"", "https://attacker.example"} {
		r, _ := http.NewRequest("POST", c.base+"/api/v1/admin/accounts", strings.NewReader(`{"username":"engineer","display_name":"Engineer"}`))
		r.Header.Set("Origin", originValue)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Cosmoformer-Request", "browser")
		r.AddCookie(&http.Cookie{Name: "__Host-cosmoformer_session", Value: c.token})
		response, e := http.DefaultClient.Do(r)
		if e != nil {
			t.Fatal(e)
		}
		response.Body.Close()
		if response.StatusCode != 403 {
			t.Fatal("foreign/missing origin accepted")
		}
	}
	old := c.csrf
	c.csrf = "wrong"
	c.call("POST", "/organizations", map[string]any{"name": "Blocked"}, 403)
	c.csrf = old
	// Huma parser/validation failures must not echo credential fields or values.
	response := c.call("POST", "/account/setup", map[string]any{"code": "SECRET-SHOULD-NOT-APPEAR", "password": "PRIVATE-PASSWORD"}, 422)
	b, _ := json.Marshal(response)
	if strings.Contains(string(b), "SECRET-SHOULD") || strings.Contains(string(b), "PRIVATE-PASSWORD") {
		t.Fatal("validation leaked credentials")
	}
}
func TestConcurrentProofConsumption(t *testing.T) {
	_, admin := fixture(t)
	account := id(admin.call("POST", "/admin/accounts", map[string]any{"username": "engineer", "display_name": "Engineer"}, 200))
	code := admin.call("POST", "/admin/accounts/"+account+"/setup-code", map[string]any{}, 200)["code"].(string)
	statuses := make(chan int, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			body, _ := json.Marshal(map[string]any{"code": code, "password": password})
			r, _ := http.NewRequest("POST", admin.base+"/api/v1/account/setup", bytes.NewReader(body))
			r.Header.Set("Origin", origin)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-Cosmoformer-Request", "browser")
			response, e := http.DefaultClient.Do(r)
			if e != nil {
				statuses <- 0
				return
			}
			response.Body.Close()
			statuses <- response.StatusCode
		})
	}
	wg.Wait()
	close(statuses)
	winners := 0
	for status := range statuses {
		if status == 200 {
			winners++
		} else if status != 422 {
			t.Errorf("unexpected status %d", status)
		}
	}
	if winners != 1 {
		t.Fatalf("one-use proof had %d consumers", winners)
	}
}

// TestBrowserServer is selected only by the Playwright lifecycle. Its account
// data lives in a generated per-suite DB, never the developer's application DB.
func TestBrowserServer(t *testing.T) {
	if os.Getenv("COSMOFORMER_TEST_BROWSER") != "1" {
		t.Skip("browser fixture server is explicitly selected by make browser-server")
	}
	db := testsupport.NewDatabase(t)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	h := identity.NewHasher()
	pass := identity.Token() + " browser"
	phc, e := h.Hash(ctx, pass)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Owner.Exec(ctx, `INSERT INTO cosmoformer.accounts(username,display_name,installation_admin,password_phc) VALUES('browseradmin','Browser administrator',true,$1)`, phc); e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(map[string]string{"username": "browseradmin", "password": pass})
	file := "../../.build/testdb/browser.json"
	if e = os.WriteFile(file, b, 0600); e != nil {
		t.Fatal(e)
	}
	defer os.Remove(file)
	handler, _ := httpapi.New(db.App, "https://localhost:5174")
	server := &http.Server{Addr: "127.0.0.1:18081", Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	fmt.Println("Isolated browser account server ready.")
	select {
	case e := <-done:
		t.Fatal(e)
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdown)
	}
}

func TestLoginThrottleAndLastAdministrator(t *testing.T) {
	_, admin := fixture(t)
	accounts := admin.call("GET", "/admin/accounts", nil, 200)["accounts"].([]any)
	a := accounts[0].(map[string]any)
	admin.call("PATCH", "/admin/accounts/"+a["id"].(string), map[string]any{"revision": a["revision"], "display_name": "Administrator", "enabled": false, "installation_admin": true}, 409)
	visitor := &client{t: t, base: admin.base}
	for range 5 {
		visitor.call("POST", "/session/login", map[string]any{"username": "unknown.engineer", "password": "wrong but bounded"}, 401)
	}
	visitor.call("POST", "/session/login", map[string]any{"username": "unknown.engineer", "password": "wrong but bounded"}, 429)
}

func TestHostBootstrapAndRecovery(t *testing.T) {
	db := testsupport.NewDatabase(t)
	dir := t.TempDir()
	dsnFile := filepath.Join(dir, "owner.dsn")
	if e := os.WriteFile(dsnFile, []byte(db.OwnerDSN), 0600); e != nil {
		t.Fatal(e)
	}
	binary := testsupport.BuildCommand(t, "cosmoformer")
	invoke := func(command, output string) ([]byte, error) {
		cmd := testsupport.Command(t, binary, command, "--username", "admin", "--setup-code-file", output)
		cmd.Env = append(cmd.Env, "COSMOFORMER_MIGRATION_DSN_FILE="+dsnFile)
		return cmd.CombinedOutput()
	}
	codeFile := filepath.Join(dir, "setup")
	output, e := invoke("bootstrap-admin", codeFile)
	if e != nil {
		t.Fatal("bootstrap command failed", string(output))
	}
	code, e := os.ReadFile(codeFile)
	if e != nil {
		t.Fatal(e)
	}
	info, e := os.Stat(codeFile)
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatal("bootstrap proof is not owner-readable only")
	}
	if bytes.Contains(output, bytes.TrimSpace(code)) {
		t.Fatal("bootstrap printed its code")
	}
	if _, e = invoke("bootstrap-admin", filepath.Join(dir, "second")); e == nil {
		t.Fatal("bootstrap reopened")
	}
	handler, _ := httpapi.New(db.App, origin)
	server := httptest.NewServer(handler)
	defer server.Close()
	c := &client{t: t, base: server.URL}
	c.call("POST", "/account/setup", map[string]any{"code": strings.TrimSpace(string(code)), "password": password}, 200)
	c.login("admin")
	recovery := filepath.Join(dir, "recovery")
	output, e = invoke("recover-admin", recovery)
	if e != nil {
		t.Fatal("recovery command failed", string(output))
	}
	c.call("GET", "/session", nil, 401)
	code, e = os.ReadFile(recovery)
	if e != nil {
		t.Fatal(e)
	}
	info, e = os.Stat(recovery)
	if e != nil || info.Mode().Perm() != 0600 || bytes.Contains(output, bytes.TrimSpace(code)) {
		t.Fatal("unsafe recovery proof output")
	}
	c.call("POST", "/account/reset", map[string]any{"code": strings.TrimSpace(string(code)), "password": password}, 200)
	c.login("admin")
}

func TestAdministrationConflictsAndTeamRevocation(t *testing.T) {
	_, admin := fixture(t)
	account := id(admin.call("POST", "/admin/accounts", map[string]any{"username": "engineer", "display_name": "Engineer"}, 200))
	admin.call("POST", "/admin/accounts", map[string]any{"username": "engineer", "display_name": "Duplicate"}, 409)
	admin.call("POST", "/admin/accounts", map[string]any{"username": "bad name", "display_name": "Invalid"}, 422)
	code := admin.call("POST", "/admin/accounts/"+account+"/setup-code", map[string]any{}, 200)["code"].(string)
	employee := &client{t: t, base: admin.base}
	employee.call("POST", "/account/setup", map[string]any{"code": code, "password": password}, 200)
	employee.login("engineer")
	admin.call("POST", "/admin/accounts/"+account+"/setup-code", map[string]any{}, 409)
	admin.call("PATCH", "/admin/accounts/"+account, map[string]any{"revision": 99, "display_name": "Stale", "enabled": true, "installation_admin": false}, 409)
	employee.call("POST", "/organizations", map[string]any{"name": "Forbidden"}, 403)
	org := id(admin.call("POST", "/organizations", map[string]any{"name": "Testing"}, 200))
	base := "/organizations/" + org
	admin.call("GET", "/organizations", nil, 200)
	admin.call("POST", base+"/invitations", map[string]any{"username": "unknown", "org_admin": false}, 404)
	admin.call("POST", base+"/invitations", map[string]any{"username": "bad name", "org_admin": false}, 409)
	invitation := id(admin.call("POST", base+"/invitations", map[string]any{"username": "engineer", "org_admin": false}, 200))
	employee.call("POST", base+"/invitations/"+invitation+"/accept", map[string]any{}, 200)
	admin.call("POST", base+"/invitations", map[string]any{"username": "engineer", "org_admin": false}, 409)
	team := id(admin.call("POST", base+"/teams", map[string]any{"name": "Team"}, 200))
	admin.call("PATCH", base+"/teams/"+team, map[string]any{"name": "Renamed", "revision": 1}, 200)
	admin.call("PATCH", base+"/teams/"+team, map[string]any{"name": "Stale", "revision": 1}, 409)
	state := admin.call("GET", base+"/access", nil, 200)
	if state["teams"].([]any)[0].(map[string]any)["name"] != "Renamed" {
		t.Fatal("team rename missing")
	}
	admin.call("POST", base+"/teams/"+team+"/members", map[string]any{"account_id": account}, 200)
	project := id(admin.call("POST", base+"/projects", map[string]any{"name": "Project"}, 200))
	admin.call("POST", base+"/grants", map[string]any{"team_id": team, "project_id": project, "role": "viewer"}, 200)
	if len(employee.call("GET", base+"/projects", nil, 200)["projects"].([]any)) != 1 {
		t.Fatal("project grant missing")
	}
	admin.call("DELETE", base+"/teams/"+team+"/members/"+account, map[string]any{}, 200)
	if len(employee.call("GET", base+"/projects", nil, 200)["projects"].([]any)) != 0 {
		t.Fatal("team removal retained access")
	}
	admin.call("DELETE", base+"/teams/"+team+"/members/"+account, map[string]any{}, 404)
	actor := admin.call("GET", "/session", nil, 200)["actor"].(map[string]any)["id"].(string)
	admin.call("PATCH", base+"/members/"+actor, map[string]any{"revision": 1, "org_admin": false}, 409)
	admin.call("DELETE", base+"/members/"+actor, map[string]any{}, 409)
	employee.call("POST", "/session/activity", map[string]any{}, 200)
	oldToken := employee.token
	employee.call("POST", "/session/logout", map[string]any{}, 200)
	employee.token = oldToken
	employee.call("GET", "/session", nil, 401)
}

func TestMutationIdempotencyAndBodyLimits(t *testing.T) {
	_, admin := fixture(t)
	admin.callKey("POST", "/organizations", map[string]any{"name": "Missing key"}, 422, "")
	admin.callKey("POST", "/organizations", map[string]any{"name": "Bad key"}, 422, "invalid key with spaces")
	admin.call("POST", "/organizations", map[string]any{"name": strings.Repeat("x", 65536)}, 422)
	// A valid token paired with a weak password is rejected before consuming proof.
	admin.call("POST", "/account/setup", map[string]any{"code": identity.Token(), "password": "short"}, 422)
	admin.call("POST", "/account/reset", map[string]any{"code": strings.Repeat("!", 43), "password": password}, 422)
}
