//go:build integration

package acceptance

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/clock"
	"github.com/cosmoformer/cosmoformer/internal/source"
	"github.com/cosmoformer/cosmoformer/internal/worker"
)

type nativeHarness struct {
	t                 *testing.T
	profile           nativeProfile
	binary, home, cli string
}
type cappedNativeOutput struct {
	bytes.Buffer
	remaining int64
}

func (b *cappedNativeOutput) Write(p []byte) (int, error) {
	if int64(len(p)) > b.remaining {
		return 0, fmt.Errorf("native fixture output limit exceeded")
	}
	b.remaining -= int64(len(p))
	return b.Buffer.Write(p)
}
func (h nativeHarness) env(workspace string) []string {
	env := []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + h.home, "LANG=C.UTF-8", "TF_IN_AUTOMATION=1", "CHECKPOINT_DISABLE=1", "TF_CLI_CONFIG_FILE=" + h.cli, "AWS_ACCESS_KEY_ID=fixture", "AWS_SECRET_ACCESS_KEY=fixture-secret", "AWS_EC2_METADATA_DISABLED=true", "AWS_CONFIG_FILE=/dev/null", "AWS_SHARED_CREDENTIALS_FILE=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}
	if workspace != "" {
		env = append(env, "TF_WORKSPACE="+workspace)
	}
	return env
}
func (h nativeHarness) command(dir, workspace, program string, args ...string) ([]byte, []byte, int) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Dir = dir
	cmd.Env = h.env(workspace)
	// Schema output is large. Both streams are bounded and never inherited by the
	// terminal. These fixtures contain synthetic data only.
	out := &cappedNativeOutput{remaining: 256 << 20}
	errout := &cappedNativeOutput{remaining: 1 << 20}
	cmd.Stdout = out
	cmd.Stderr = errout
	e := cmd.Run()
	code := 0
	if e != nil {
		if x, ok := e.(*exec.ExitError); ok {
			code = x.ExitCode()
		} else {
			h.t.Fatalf("native child failed: %v", e)
		}
	}
	if ctx.Err() != nil {
		h.t.Fatal("native child exceeded fixture deadline")
	}
	return out.Bytes(), errout.Bytes(), code
}
func nativeExcerpt(raw []byte) string {
	if len(raw) > 4096 {
		raw = raw[:4096]
	}
	return string(raw)
}
func (h nativeHarness) run(dir, workspace string, want int, args ...string) []byte {
	h.t.Helper()
	out, stderr, code := h.command(dir, workspace, h.binary, args...)
	if code != want {
		h.t.Fatalf("%s: exit %d, want %d\n%s\n%s", args[0], code, want, nativeExcerpt(out), nativeExcerpt(stderr))
	}
	return out
}
func (h nativeHarness) reject(dir, workspace, reason string, args ...string) {
	h.t.Helper()
	out, stderr, code := h.command(dir, workspace, h.binary, args...)
	if code == 0 || !strings.Contains(strings.ToLower(string(append(out, stderr...))), strings.ToLower(reason)) {
		h.t.Fatalf("%s did not reject %q: exit %d\n%s\n%s", args[0], reason, code, nativeExcerpt(out), nativeExcerpt(stderr))
	}
}
func writeNative(t *testing.T, path string, raw []byte) {
	t.Helper()
	if e := os.WriteFile(path, raw, 0600); e != nil {
		t.Fatal(e)
	}
}
func (h nativeHarness) git(dir string, args ...string) []byte {
	h.t.Helper()
	out, stderr, code := h.command(dir, "", "/usr/bin/git", args...)
	if code != 0 {
		h.t.Fatalf("Git %s: %s %s", args[0], nativeExcerpt(out), nativeExcerpt(stderr))
	}
	return out
}

// Every scenario starts from an exact commit obtained over verified loopback
// HTTPS. No file:// fallback, branch fallback or production TLS bypass exists.
func (h nativeHarness) repository(config string) func() string {
	t := h.t
	root := t.TempDir()
	work := filepath.Join(root, "work")
	if e := os.Mkdir(work, 0700); e != nil {
		t.Fatal(e)
	}
	writeNative(t, filepath.Join(work, "main.tf"), []byte(config))
	h.git(work, "init", "--quiet")
	h.git(work, "add", "main.tf")
	h.git(work, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--quiet", "-m", "Synthetic native fixture")
	commit := strings.TrimSpace(string(h.git(work, "rev-parse", "HEAD")))
	h.git(root, "clone", "--quiet", "--bare", work, filepath.Join(root, "fixture.git"))
	server := httptest.NewTLSServer(&cgi.Handler{Path: "/usr/lib/git-core/git-http-backend", Dir: root, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}})
	t.Cleanup(server.Close)
	ca := filepath.Join(root, "ca.pem")
	writeNative(t, ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	return func() string {
		dir := filepath.Join(t.TempDir(), "checkout")
		h.git(root, "-c", "http.sslCAInfo="+ca, "clone", "--quiet", "--no-checkout", server.URL+"/fixture.git", dir)
		h.git(dir, "checkout", "--quiet", "--detach", commit)
		if got := strings.TrimSpace(string(h.git(dir, "rev-parse", "HEAD"))); got != commit {
			t.Fatal("checkout did not retain exact commit")
		}
		return dir
	}
}

func TestNativeEngineCompatibility(t *testing.T) {
	if os.Getenv("COSMOFORMER_TEST_NATIVE") != "1" {
		t.Skip("requires isolated make test-native image; no native/DB fallback")
	}
	for k, want := range map[string]string{"COSMOFORMER_TEST_NATIVE_PROFILE_FILE": "/fixtures/runtimes.json", "COSMOFORMER_TEST_NATIVE_PACKAGE_DIR": "/fixtures/provider-mirror"} {
		if os.Getenv(k) != want {
			t.Fatalf("missing/mismatched %s", k)
		}
	}
	p, e := loadNativeProfile(os.Getenv("COSMOFORMER_TEST_NATIVE_PROFILE_FILE"), os.Getenv("COSMOFORMER_TEST_NATIVE_ENGINE"))
	if e != nil {
		t.Fatal(e)
	}
	binary := "/usr/local/bin/" + p.Archive.Member
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" || os.Getenv("COSMOFORMER_TEST_NATIVE_VERSION") != p.Version || os.Getenv("COSMOFORMER_TEST_NATIVE_BINARY") != binary {
		t.Fatal("native selection differs from reviewed Linux amd64 runtime")
	}
	for _, name := range []string{"COSMOFORMER_TEST_OWNER_DSN", "COSMOFORMER_DATABASE_DSN_FILE", "COSMOFORMER_MIGRATION_DSN_FILE", "COSMOFORMER_TEST_NATIVE_SUPPLY"} {
		if os.Getenv(name) != "" {
			t.Fatal("native container received forbidden input", name)
		}
	}
	if _, e := clock.New().Now(); e != nil {
		t.Fatal("required Linux elapsed clock unavailable", e)
	}
	meta, e := worker.VerifyRuntime(context.Background(), "/usr/local/share/cosmoformer/runtime.json")
	if e != nil || meta.Engine != p.Engine || meta.Version != p.Version {
		t.Fatal("baked runtime identity mismatch", e)
	}
	home := t.TempDir()
	cli := filepath.Join(home, "native.tfrc")
	writeNative(t, cli, []byte("disable_checkpoint = true\nprovider_installation {\n filesystem_mirror { path = \"/fixtures/provider-mirror\" }\n}\n"))
	h := nativeHarness{t, p, binary, home, cli}
	var version struct {
		Version  string `json:"terraform_version"`
		Platform string `json:"platform"`
	}
	if e = json.Unmarshal(h.run(home, "", 0, "version", "-json"), &version); e != nil || version.Version != p.Version || version.Platform != "linux_amd64" {
		t.Fatal("version -json identity mismatch", e)
	}
	t.Run("AWS_schema_offline", func(t *testing.T) {
		h := h
		h.t = t
		registry := strings.Split(p.Packages[1].Sources[0], "/hashicorp/")[0]
		checkout := h.repository(fmt.Sprintf("terraform {\n required_providers {\n aws = { source = %q, version = \"6.41.0\" }\n }\n}\n", registry+"/hashicorp/aws"))()
		h.run(checkout, "", 0, "init", "-input=false", "-no-color")
		var schema struct {
			Format    string `json:"format_version"`
			Providers map[string]struct {
				Provider struct {
					Block struct {
						Attributes map[string]struct {
							Sensitive bool `json:"sensitive"`
						} `json:"attributes"`
					} `json:"block"`
				} `json:"provider"`
				Resources map[string]json.RawMessage `json:"resource_schemas"`
			} `json:"provider_schemas"`
		}
		if e := json.Unmarshal(h.run(checkout, "", 0, "providers", "schema", "-json"), &schema); e != nil {
			t.Fatal(e)
		}
		aws := schema.Providers[registry+"/hashicorp/aws"]
		if schema.Format != "1.0" || len(aws.Resources) < 1000 || len(aws.Provider.Block.Attributes) == 0 {
			t.Fatal("AWS schema shape/profile differs")
		}
		for resource, attribute := range map[string]string{"aws_db_instance": "password", "aws_iam_access_key": "secret"} {
			var item struct {
				Block struct {
					Attributes map[string]struct {
						Sensitive bool `json:"sensitive"`
					} `json:"attributes"`
				} `json:"block"`
			}
			if e := json.Unmarshal(aws.Resources[resource], &item); e != nil || !item.Block.Attributes[attribute].Sensitive {
				t.Fatalf("AWS schema lost required sensitivity metadata for %s.%s: %v", resource, attribute, e)
			}
		}
		t.Logf("AWS 6.41.0 schema format %s: %d resource schemas", schema.Format, len(aws.Resources))
	})
	t.Run("required_owner_attribute_rejected", func(t *testing.T) {
		h := h
		h.t = t
		b := newNativeBackend(t)
		dir := h.repository(fmt.Sprintf("terraform {\n backend \"s3\" {\n bucket = \"fictional-state-bucket\"\n key = \"state.tfstate\"\n region = \"eu-west-1\"\n expected_bucket_owner = \"111122223333\"\n encrypt = true\n use_lockfile = true\n%s }\n}\n", nativeEndpointConfig(b)))()
		h.reject(dir, "", "expected_bucket_owner", "init", "-input=false", "-no-color")
		t.Log("unsupported native owner attribute rejected; owner intent is separate Cosmoformer metadata")
	})
	for _, profile := range []string{"s3", "dynamodb", "dual"} {
		for _, workspace := range []string{"default", "review", "custom"} {
			t.Run(profile+"/"+workspace, func(t *testing.T) {
				h := h
				h.t = t
				h.backendScenario(profile, workspace, profile == "s3", false)
			})
		}
	}
	if p.Engine == "opentofu" {
		for _, workspace := range []string{"default", "review", "custom"} {
			t.Run("path_segments/"+workspace, func(t *testing.T) {
				h := h
				h.t = t
				h.backendScenario("s3", workspace, true, true)
			})
		}
	} else {
		t.Run("repeated_slash_key_rejected", func(t *testing.T) {
			h := h
			h.t = t
			b := newNativeBackend(t)
			attrs := map[string]any{"bucket": nativeBucket, "key": "paths//state", "region": "eu-west-1", "encrypt": true, "use_lockfile": true}
			if _, e := source.CanonicalBackend(attrs, h.backendContext("review"), ""); e == nil {
				t.Fatal("preflight accepted a key rejected by the selected Terraform runtime")
			}
			dir := h.repository(fmt.Sprintf("terraform {\n backend \"s3\" {\n bucket = %q\n key = \"paths//state\"\n region = \"eu-west-1\"\n encrypt = true\n use_lockfile = true\n%s }\n}\n", nativeBucket, nativeEndpointConfig(b)))()
			h.reject(dir, "review", `must not contain "//"`, "init", "-input=false", "-no-color")
		})
	}
}

func (h nativeHarness) backendScenario(profile, workspace string, empty, edge bool) {
	t := h.t
	b := newNativeBackend(t)
	key := "paths/state.tfstate"
	if edge {
		key = "paths//segment/../state.tfstate"
	}
	attrs := map[string]any{"bucket": nativeBucket, "key": key, "region": "eu-west-1", "encrypt": true, "use_lockfile": profile != "dynamodb"}
	if workspace == "custom" {
		attrs["workspace_key_prefix"] = "workspaces//fixtures/.."
	}
	tableARN := ""
	if profile != "s3" {
		attrs["dynamodb_table"] = nativeTable
		tableARN = "arn:aws:dynamodb:eu-west-1:111122223333:table/" + nativeTable
	}
	canonical, e := source.CanonicalBackend(attrs, h.backendContext(workspace), tableARN)
	if e != nil {
		t.Fatal(e)
	}
	b.ownerPolicy(canonical)
	effective := canonical.Target.Key
	config := "terraform {\n backend \"s3\" {}\n"
	if !empty {
		config += fmt.Sprintf(" required_providers { null = { source = %q, version = \"3.2.4\" } }\n", h.profile.Packages[0].Sources[0])
	}
	config += "}\n"
	if !empty {
		config += "resource \"null_resource\" \"fixture\" { triggers = { value = \"synthetic\" } }\n"
	}
	config += "output \"fixture\" { value = \"local only\" }\n"
	config += "output \"sensitive_fixture\" {\n value = \"fictional-secret-only\"\n sensitive = true\n}\n"
	checkout := h.repository(config)
	dir := checkout()
	backend := fmt.Sprintf("bucket = %q\nkey = %q\nregion = \"eu-west-1\"\nencrypt = true\nuse_lockfile = %t\n%s", nativeBucket, key, profile != "dynamodb", nativeEndpointConfig(b))
	if profile != "s3" {
		backend += fmt.Sprintf("dynamodb_table = %q\n", nativeTable)
	}
	if workspace == "custom" {
		backend += fmt.Sprintf("workspace_key_prefix = %q\n", attrs["workspace_key_prefix"])
	}
	// All endpoint/credential controls live exclusively in this synthetic fixture.
	init := func(dir string, readonly bool) {
		path := filepath.Join(dir, "fixture.backend.hcl")
		writeNative(t, path, []byte(backend))
		args := []string{"init", "-input=false", "-no-color", "-reconfigure", "-lock-timeout=60s", "-backend-config=" + path}
		if readonly {
			args = append(args, "-lockfile=readonly")
		}
		h.run(dir, workspace, 0, args...)
	}
	init(dir, false)
	lock, lockErr := os.ReadFile(filepath.Join(dir, ".terraform.lock.hcl"))
	if empty {
		if lockErr != nil && !errors.Is(lockErr, os.ErrNotExist) {
			t.Fatal("could not establish provider-lock absence", lockErr)
		}
		if lockErr == nil && strings.TrimSpace(string(lock)) != "" {
			t.Fatal("empty provider set gained dependencies")
		}
	} else if lockErr != nil || !bytes.Contains(lock, []byte("3.2.4")) {
		t.Fatal("null provider lock missing", lockErr)
	}
	schema := h.run(dir, workspace, 0, "providers", "schema", "-json")
	var schemas struct {
		Format    string                     `json:"format_version"`
		Providers map[string]json.RawMessage `json:"provider_schemas"`
	}
	if e := json.Unmarshal(schema, &schemas); e != nil || schemas.Format != "1.0" {
		t.Fatal("provider schema format mismatch", e)
	}
	if empty && len(schemas.Providers) != 0 {
		t.Fatal("empty root loaded provider")
	}
	if !empty && len(schemas.Providers) != 1 {
		t.Fatal("external provider schema absent")
	}
	plan := filepath.Join(dir, "retained.plan")
	h.run(dir, workspace, 2, "plan", "-input=false", "-no-color", "-lock=true", "-lock-timeout=60s", "-detailed-exitcode", "-out="+plan)
	var shown struct {
		Format  string                     `json:"format_version"`
		Version string                     `json:"terraform_version"`
		Changes []json.RawMessage          `json:"resource_changes"`
		Outputs map[string]json.RawMessage `json:"output_changes"`
	}
	if e := json.Unmarshal(h.run(dir, workspace, 0, "show", "-json", plan), &shown); e != nil || shown.Format != "1.2" || shown.Version != h.profile.Version || len(shown.Outputs) != 2 {
		t.Fatal("plan JSON profile mismatch", e, shown.Format, shown.Version)
	}
	var sensitive struct {
		After          string `json:"after"`
		AfterSensitive bool   `json:"after_sensitive"`
	}
	if e := json.Unmarshal(shown.Outputs["sensitive_fixture"], &sensitive); e != nil || sensitive.After != "fictional-secret-only" || !sensitive.AfterSensitive {
		t.Fatal("saved-plan JSON lost synthetic sensitive-output metadata", e)
	}
	if (!empty && len(shown.Changes) != 1) || (empty && len(shown.Changes) != 0) {
		t.Fatal("unexpected synthetic resource changes")
	}
	fresh := checkout()
	if lockErr == nil {
		writeNative(t, filepath.Join(fresh, ".terraform.lock.hcl"), lock)
	}
	init(fresh, !empty)
	if !empty {
		got, e := os.ReadFile(filepath.Join(fresh, ".terraform.lock.hcl"))
		if e != nil || !bytes.Equal(got, lock) {
			t.Fatal("readonly init altered retained provider lock")
		}
	} else if errors.Is(lockErr, os.ErrNotExist) {
		if _, e := os.Stat(filepath.Join(fresh, ".terraform.lock.hcl")); !errors.Is(e, os.ErrNotExist) {
			t.Fatal("empty-provider replay did not preserve lockfile absence")
		}
	}
	// Only a synthetic retained plan is applied, with no automatic replan path.
	h.run(fresh, workspace, 0, "apply", "-input=false", "-no-color", "-lock=true", "-lock-timeout=60s", plan)
	objects, _ := b.snapshot()
	var state struct {
		Lineage string `json:"lineage"`
		Serial  int    `json:"serial"`
		Version int    `json:"version"`
	}
	if e := json.Unmarshal(objects[effective], &state); e != nil || state.Lineage == "" || state.Serial < 1 || state.Version != 4 {
		t.Fatal("native state missing exact canonical key/metadata", e, effective)
	}
	if profile != "s3" {
		sum := md5.Sum(objects[effective])
		b.mu.Lock()
		digest := b.items[nativeBucket+"/"+effective+"-md5"]["Digest"]["S"]
		b.mu.Unlock()
		if digest != hex.EncodeToString(sum[:]) {
			t.Fatal("native DynamoDB checksum differs from exact stored state")
		}
	}
	stable := filepath.Join(fresh, "stable.plan")
	h.run(fresh, workspace, 0, "plan", "-input=false", "-no-color", "-lock=true", "-lock-timeout=60s", "-detailed-exitcode", "-out="+stable)
	t.Run("owner_changed_after_init", func(t *testing.T) {
		h := h
		h.t = t
		before, _ := b.snapshot()
		b.mu.Lock()
		b.owner = "999900001111"
		denials := b.ownerDenials
		b.mu.Unlock()
		defer func() { b.mu.Lock(); b.owner = "111122223333"; b.mu.Unlock() }()
		h.reject(fresh, workspace, "403", "plan", "-input=false", "-no-color", "-lock=true", "-lock-timeout=1s", "-detailed-exitcode", "-out="+filepath.Join(fresh, "wrong-owner.plan"))
		after, _ := b.snapshot()
		if !bytes.Equal(before[effective], after[effective]) {
			t.Error("wrong-owner attempt changed fixture state")
		}
		b.mu.Lock()
		if b.ownerDenials <= denials {
			t.Error("owner constraint not exercised")
		}
		b.mu.Unlock()
		if _, e := os.Stat(filepath.Join(fresh, "wrong-owner.plan")); !errors.Is(e, os.ErrNotExist) {
			t.Error("wrong-owner attempt retained a plan")
		}
	})
	for _, service := range []string{"s3", "ddb"} {
		if service == "s3" && profile == "dynamodb" || service == "ddb" && profile == "s3" {
			continue
		}
		t.Run("contention_"+service, func(t *testing.T) {
			h := h
			h.t = t
			release := b.contend(effective, service)
			defer release()
			// Workspace state loading and plan locking use different diagnostic
			// headings. Both must identify the exact competing lock, not just fail.
			h.reject(fresh, workspace, "11111111-2222-3333-4444-555555555555", "plan", "-input=false", "-no-color", "-lock=true", "-lock-timeout=1s", "-detailed-exitcode", "-out="+filepath.Join(fresh, "blocked.plan"))
			b.mu.Lock()
			defer b.mu.Unlock()
			_, fileHeld := b.objects[effective+".tflock"]
			_, ddbHeld := b.items[nativeBucket+"/"+effective]
			if fileHeld != (service == "s3") || ddbHeld != (service == "ddb") {
				t.Error("contention removed a foreign lock or retained a partial dual lock")
			}
			if _, e := os.Stat(filepath.Join(fresh, "blocked.plan")); !errors.Is(e, os.ErrNotExist) {
				t.Error("contended command created a retained plan")
			}
		})
	}
	b.replaceState(effective, func(s map[string]any) { s["serial"] = s["serial"].(float64) + 1 })
	rejectSaved := func(reason string) {
		t.Helper()
		before, _ := b.snapshot()
		h.reject(fresh, workspace, reason, "apply", "-input=false", "-no-color", "-lock-timeout=60s", stable)
		after, _ := b.snapshot()
		if !bytes.Equal(before[effective], after[effective]) {
			t.Fatal("rejected saved plan changed remote fixture state")
		}
	}
	rejectSaved("Saved plan is stale")
	h.run(fresh, workspace, 0, "plan", "-input=false", "-no-color", "-lock=true", "-lock-timeout=60s", "-detailed-exitcode", "-out="+stable)
	b.replaceState(effective, func(s map[string]any) { s["lineage"] = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" })
	rejectSaved("different state lineage")
	h.assertNativeEvents(b, effective, profile)
	t.Logf("state v%d serial %d; plan JSON %s; exact effective key %q", state.Version, state.Serial, shown.Format, effective)
}
func (h nativeHarness) assertNativeEvents(b *nativeBackend, key, profile string) {
	h.t.Helper()
	objects, events := b.snapshot()
	counts := map[string]int{}
	for _, ev := range events {
		if ev.Service == "s3" && ev.Method != "LIST" && ev.Key != key && ev.Key != key+".tflock" {
			h.t.Fatalf("native key drift: %q, expected %q", ev.Key, key)
		}
		if ev.Service == "ddb" && ev.Key != nativeBucket+"/"+key && ev.Key != nativeBucket+"/"+key+"-md5" {
			h.t.Fatal("native LockID drift", ev.Key)
		}
		suffix := ""
		if strings.HasSuffix(ev.Key, ".tflock") {
			suffix = " lock"
		}
		if strings.HasSuffix(ev.Key, "-md5") {
			suffix = " checksum"
		}
		counts[ev.Service+" "+ev.Method+suffix]++
		if ev.Conflict {
			counts[ev.Service+" conflict"]++
		}
	}
	for _, event := range []string{"s3 HEAD", "s3 GET", "s3 PUT", "s3 LIST"} {
		if counts[event] == 0 {
			h.t.Error("unexercised state operation", event)
		}
	}
	if _, exists := objects[key+".tflock"]; exists {
		h.t.Error("native lockfile not released")
	}
	if profile != "dynamodb" {
		for _, event := range []string{"s3 PUT lock", "s3 GET lock", "s3 DELETE lock", "s3 conflict"} {
			if counts[event] == 0 {
				h.t.Error("unexercised lock operation", event)
			}
		}
	}
	if profile != "s3" {
		for _, event := range []string{"ddb PutItem", "ddb GetItem", "ddb DeleteItem", "ddb PutItem checksum", "ddb GetItem checksum", "ddb conflict"} {
			if counts[event] == 0 {
				h.t.Error("unexercised DynamoDB operation", event)
			}
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, exists := b.items[nativeBucket+"/"+key]; exists {
			h.t.Error("DynamoDB lock not released")
		}
	}
}

// Compile-time guard: the capture writer deliberately reports overflow.
var _ io.Writer = (*cappedNativeOutput)(nil)

// Even expected validation failures receive only loopback backend endpoints.
func nativeEndpointConfig(b *nativeBackend) string {
	return fmt.Sprintf("endpoints = { s3 = %q, dynamodb = %q }\nuse_path_style = true\nskip_credentials_validation = true\nskip_requesting_account_id = true\nskip_metadata_api_check = true\nskip_region_validation = true\n", b.server.URL, b.server.URL)
}

func (h nativeHarness) backendContext(workspace string) source.BackendContext {
	return source.BackendContext{Engine: catalog.Engine{Name: h.profile.Engine, Version: h.profile.Version}, Workspace: workspace, ExpectedOwner: "111122223333"}
}
