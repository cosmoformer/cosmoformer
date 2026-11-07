//go:build integration

package acceptance

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type nativeArchive struct {
	URL    string `json:"url"`
	SHA    string `json:"sha256"`
	Member string `json:"member"`
}
type nativePackage struct {
	Sources []string `json:"source_addresses"`
	Version string   `json:"version"`
	Arch    string   `json:"arch"`
	URL     string   `json:"archive_url"`
	SHA     string   `json:"sha256"`
	Member  string   `json:"member"`
}
type nativeProfile struct {
	Engine   string          `json:"engine"`
	Version  string          `json:"version"`
	Arch     string          `json:"arch"`
	Base     string          `json:"base_image_ref"`
	Git      string          `json:"git_package_version"`
	Archive  nativeArchive   `json:"engine_archive"`
	Packages []nativePackage `json:"test_provider_packages"`
}
type nativeManifest struct {
	Schema   int             `json:"schema_version"`
	Arch     string          `json:"architecture"`
	Profiles []nativeProfile `json:"runtimes"`
}

// These are reviewed public fixture pins, independent of caller-selected inputs.
func nativePins(engine string) nativeProfile {
	p := nativeProfile{Engine: engine, Arch: "linux_amd64", Base: "debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2", Git: "1:2.47.3-0+deb13u1"}
	registry := "registry.terraform.io"
	switch engine {
	case "terraform":
		p.Version = "1.17.1"
		p.Archive = nativeArchive{"https://releases.hashicorp.com/terraform/1.17.1/terraform_1.17.1_linux_amd64.zip", "4638628058e71a55212b1c563712eac03b902128f40740f3dc8174df12135061", "terraform"}
	case "opentofu":
		registry = "registry.opentofu.org"
		p.Version = "1.13.8"
		p.Archive = nativeArchive{"https://github.com/opentofu/opentofu/releases/download/v1.13.8/tofu_1.13.8_linux_amd64.zip", "aa4382a5eefcf026309d07398ea803f97b9218a17d940999a938be22bfe9d73f", "tofu"}
	default:
		return nativeProfile{}
	}
	for _, x := range []struct{ name, version, sha string }{{"null", "3.2.4", "9d32ac3619cfc93eb3c4f423492a8e0f79db05fec58e449dee9b2d5873d5f69f"}, {"aws", "6.41.0", "4e8de5f3fcc3e3f41b6703ad4c0fa62175d0c29afcf56ac82eb16c604bb6dafa"}} {
		p.Packages = append(p.Packages, nativePackage{[]string{registry + "/hashicorp/" + x.name}, x.version, p.Arch, fmt.Sprintf("https://releases.hashicorp.com/terraform-provider-%s/%s/terraform-provider-%s_%s_linux_amd64.zip", x.name, x.version, x.name, x.version), x.sha, fmt.Sprintf("terraform-provider-%s_v%s_x5", x.name, x.version)})
	}
	return p
}
func loadNativeProfile(path, engine string) (nativeProfile, error) {
	raw, e := os.ReadFile(path)
	if e != nil {
		return nativeProfile{}, e
	}
	var m nativeManifest
	if e = json.Unmarshal(raw, &m); e != nil {
		return nativeProfile{}, e
	}
	if m.Schema != 1 || m.Arch != "linux_amd64" || len(m.Profiles) != 2 {
		return nativeProfile{}, fmt.Errorf("invalid native manifest")
	}
	seen := map[string]bool{}
	var selected nativeProfile
	for _, p := range m.Profiles {
		if seen[p.Engine] || p.Engine == "" || !reflect.DeepEqual(p, nativePins(p.Engine)) {
			return selected, fmt.Errorf("unapproved native fixture pin: %s", p.Engine)
		}
		seen[p.Engine] = true
		if p.Engine == engine {
			selected = p
		}
	}
	if selected.Engine == "" {
		return selected, fmt.Errorf("missing selected engine")
	}
	return selected, nil
}

// Only the Docker builder selects this test. Runtime compatibility never fetches
// packages, and ordinary host integration explicitly skips fixture supply.
func TestNativeFixtureSupply(t *testing.T) {
	if os.Getenv("COSMOFORMER_TEST_NATIVE_SUPPLY") != "1" {
		t.Skip("build-only verified provider fixture supply")
	}
	p, e := loadNativeProfile(os.Getenv("COSMOFORMER_TEST_NATIVE_PROFILE_FILE"), os.Getenv("COSMOFORMER_TEST_NATIVE_ENGINE"))
	if e != nil {
		t.Fatal(e)
	}
	root := os.Getenv("COSMOFORMER_TEST_NATIVE_PACKAGE_DIR")
	if root != "/out/provider-mirror" {
		t.Fatal("unexpected build mirror path")
	}
	client := &http.Client{Timeout: 5 * time.Minute, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if r.URL.Scheme != "https" || len(via) > 5 {
			return fmt.Errorf("unsafe fixture redirect")
		}
		return nil
	}}
	for _, pkg := range p.Packages {
		t.Run(pkg.Member, func(t *testing.T) {
			response, e := client.Get(pkg.URL)
			if e != nil {
				t.Fatal(e)
			}
			defer response.Body.Close()
			if response.StatusCode != 200 {
				t.Fatalf("archive HTTP %d", response.StatusCode)
			}
			tmp, e := os.CreateTemp(t.TempDir(), "archive-*.zip")
			if e != nil {
				t.Fatal(e)
			}
			defer tmp.Close()
			h := sha256.New()
			n, e := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(response.Body, 350<<20+1))
			if e != nil || n > 350<<20 {
				t.Fatal("archive size/read failure", e)
			}
			if hex.EncodeToString(h.Sum(nil)) != pkg.SHA {
				t.Fatal("fixture archive checksum mismatch")
			}
			z, e := zip.NewReader(tmp, n)
			if e != nil {
				t.Fatal(e)
			}
			for _, address := range pkg.Sources {
				dir := filepath.Join(root, address, pkg.Version, pkg.Arch)
				if e = os.MkdirAll(dir, 0755); e != nil {
					t.Fatal(e)
				}
				for _, member := range []string{pkg.Member, "LICENSE.txt"} {
					var entry *zip.File
					for _, f := range z.File {
						if f.Name == member {
							if entry != nil {
								t.Fatal("duplicate archive member")
							}
							entry = f
						}
					}
					if entry == nil || entry.UncompressedSize64 > 1<<30 || !entry.Mode().IsRegular() {
						t.Fatal("missing/invalid fixture member", member)
					}
					in, e := entry.Open()
					if e != nil {
						t.Fatal(e)
					}
					mode := os.FileMode(0644)
					if member == pkg.Member {
						mode = 0755
					}
					out, e := os.OpenFile(filepath.Join(dir, member), os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
					if e != nil {
						t.Fatal(e)
					}
					_, copyErr := io.Copy(out, in)
					closeErr := out.Close()
					in.Close()
					if copyErr != nil || closeErr != nil {
						t.Fatal("fixture extraction failed", copyErr, closeErr)
					}
				}
				metadata, _ := json.MarshalIndent(pkg, "", "  ")
				if e = os.WriteFile(filepath.Join(dir, "cosmoformer-package.json"), metadata, 0644); e != nil {
					t.Fatal(e)
				}
			}
		})
	}
}

func TestNativeManifestRejectsUnreviewedPins(t *testing.T) {
	good := nativeManifest{1, "linux_amd64", []nativeProfile{nativePins("opentofu"), nativePins("terraform")}}
	for _, name := range []string{"valid", "engine", "arch", "base", "hash", "member", "provider URL", "provider address", "missing", "duplicate"} {
		t.Run(name, func(t *testing.T) {
			raw, _ := json.Marshal(good)
			var m nativeManifest
			json.Unmarshal(raw, &m)
			switch name {
			case "engine":
				m.Profiles[0].Engine = "unknown"
			case "arch":
				m.Profiles[0].Arch = "linux_arm64"
			case "base":
				m.Profiles[0].Base = "debian:latest"
			case "hash":
				m.Profiles[0].Archive.SHA = strings.Repeat("0", 64)
			case "member":
				m.Profiles[0].Archive.Member = "../tofu"
			case "provider URL":
				m.Profiles[0].Packages[0].URL = "https://example.invalid/package.zip"
			case "provider address":
				m.Profiles[0].Packages[0].Sources = []string{"registry.example.invalid/hashicorp/null"}
			case "missing":
				m.Profiles = m.Profiles[:1]
			case "duplicate":
				m.Profiles[1] = m.Profiles[0]
			}
			path := filepath.Join(t.TempDir(), "runtimes.json")
			raw, _ = json.Marshal(m)
			if e := os.WriteFile(path, raw, 0600); e != nil {
				t.Fatal(e)
			}
			_, e := loadNativeProfile(path, "opentofu")
			if (e == nil) != (name == "valid") {
				t.Fatalf("validation result: %v", e)
			}
		})
	}
}

// The selected Go builder already contains GNU Make 4.x. Its shell argument
// must match the host Make invocation even though this stage needs no Node.
func TestNativeMakeRecipe(t *testing.T) {
	if os.Getenv("COSMOFORMER_TEST_NATIVE_SUPPLY") != "1" {
		t.Skip("build-only GNU Make argument regression")
	}
	dir := t.TempDir()
	capture := filepath.Join(dir, "program.js")
	stub := `#!/bin/sh
set -eu
test "$#" -eq 3
test "$1" = --input-type=module
test "$2" = -e
printf '%s' "$3" > "$NATIVE_CAPTURE"
`
	if e := os.WriteFile(filepath.Join(dir, "node"), []byte(stub), 0700); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	version, e := exec.CommandContext(ctx, "/usr/bin/make", "--version").Output()
	if e != nil || !bytes.Contains(version, []byte("GNU Make 4.")) {
		t.Fatal("GNU Make 4.x builder prerequisite missing", e)
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/make", "--no-print-directory", "-f", "/fixtures/Makefile", "test-native")
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin", "HOME=" + dir, "NATIVE_CAPTURE=" + capture}
	if output, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("GNU Make invocation failed: %v: %s", e, output)
	}
	raw, e := os.ReadFile(capture)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.HasPrefix(raw, []byte("import fs from")) || bytes.Contains(raw, []byte("\\\n")) || bytes.Contains(raw, []byte("$${")) || !bytes.Contains(raw, []byte("${p[0]}")) {
		t.Fatal("Make passed escaped recipe syntax instead of the JavaScript program")
	}
	t.Log(strings.Split(string(version), "\n")[0])
}
