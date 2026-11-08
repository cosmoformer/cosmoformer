//go:build integration

package acceptance

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Exercise the shipped seed client against real authentication, permissions,
// HTTP validators and PostgreSQL, rather than duplicating its API in a mock.
func TestLocalDevelopmentSeed(t *testing.T) {
	_, admin := fixture(t)
	// Keep the real API/authentication and database, but expose them through the
	// same owner-only Unix transport as the development commands.
	private, err := os.MkdirTemp("", "cf-seed-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(private) })
	socket := filepath.Join(private, "api.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(socket, 0600); err != nil {
		t.Fatal(err)
	}
	backend := admin.base
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.URL.Scheme = "http"
		r.URL.Host = backend[len("http://"):]
		r.RequestURI = ""
		response, e := http.DefaultClient.Do(r)
		if e != nil {
			http.Error(w, "fixture", 502)
			return
		}
		defer response.Body.Close()
		for key, values := range response.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	module, err := filepath.Abs("../../scripts/dev-seed.mjs")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	script := `
import {pathToFileURL} from 'node:url';
import assert from 'node:assert/strict';
const {LocalClient,seed} = await import(pathToFileURL(process.argv[1]));
assert.throws(() => new LocalClient('http://127.0.0.1:18080'));
const client = new LocalClient(process.argv[2]);
const session = await client.login('admin', 'a fictional testing password 2026');
try {
 const first = await seed(client, session.actor);
 assert.deepEqual(await seed(client, session.actor), first);
 const directory = await client.call('GET','/organizations');
 assert.equal(directory.organizations.length,1);
 const base = '/organizations/'+first.organization;
 const projects = (await client.call('GET',base+'/projects')).projects;
 assert.equal(projects.length,1); assert.equal(projects[0].environments.length,1);
 const access = await client.call('GET',base+'/access');
 assert.equal(access.teams.length,1); assert.equal(access.teams[0].members.length,1);
 assert.equal(access.grants.length,1); assert.equal(access.grants[0].role,'planner');
 assert.equal(access.grants[0].environment_id,first.environment);
 const oldGrant = access.grants[0].id;
 await client.call('DELETE',base+'/grants/'+oldGrant,{});
 await seed(client,session.actor);
 const restored = (await client.call('GET',base+'/access')).grants;
 assert.equal(restored.length,1); assert.notEqual(restored[0].id,oldGrant);
} finally { await client.logout(); }
await assert.rejects(() => client.call('GET','/session'), /HTTP 401/);
`
	cmd := exec.CommandContext(ctx, "node", "--input-type=module", "-e", script, module, socket)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("local seed client: %v\n%s", err, output)
	}
}
