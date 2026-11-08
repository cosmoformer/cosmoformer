package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/runs"
)

func TestCredentialHelperUsesOnlyExactPrivateCapability(t *testing.T) {
	dir, e := os.MkdirTemp("", "cf-helper-")
	if e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "git.sock")
	l, e := net.Listen("unix", socket)
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	os.Chmod(socket, 0600)
	ctx := helperContext{Socket: socket, AttemptID: runs.ID(), Identity: runs.Identity{WorkerID: runs.ID(), Generation: 1, Agent: runs.ID()}, Repositories: map[string]string{"https://bitbucket.org/fictional/repo.git": runs.ID()}}
	b, _ := json.Marshal(ctx)
	file := filepath.Join(dir, "context")
	os.WriteFile(file, b, 0600)
	received := make(chan helperRequest, 1)
	go func() {
		c, e := l.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		var r helperRequest
		json.NewDecoder(c).Decode(&r)
		received <- r
		json.NewEncoder(c).Encode(helperReply{Username: "x-token-auth", Token: "fictional-repository-token"})
	}()
	var out bytes.Buffer
	if e = CredentialHelper("get", file, strings.NewReader("protocol=https\nhost=bitbucket.org\npath=fictional/repo.git\n\n"), &out); e != nil {
		t.Fatal(e)
	}
	if out.String() != "username=x-token-auth\npassword=fictional-repository-token\n\n" {
		t.Fatal("credential protocol failed")
	}
	select {
	case r := <-received:
		if r.Identity != ctx.Identity || r.AttemptID != ctx.AttemptID {
			t.Fatal("helper lost attempt identity")
		}
	case <-time.After(time.Second):
		t.Fatal("missing local capability lookup")
	}
	if e = CredentialHelper("get", file, strings.NewReader("protocol=https\nhost=attacker.example\npath=fictional/repo.git\n\n"), io.Discard); e == nil {
		t.Fatal("foreign host accepted")
	}
	os.Chmod(file, 0644)
	if e = CredentialHelper("get", file, strings.NewReader(""), io.Discard); e == nil {
		t.Fatal("public context accepted")
	}
}
func TestKnownCredentialRegistrationBound(t *testing.T) {
	a := &Agent{}
	if !a.register("fictional-token") {
		t.Fatal("valid token refused")
	}
	if a.register("injected\ncredential") {
		t.Fatal("newline accepted")
	}
	if a.register(strings.Repeat("x", 262145)) {
		t.Fatal("capacity exceeded")
	}
}
func TestWorkerRequiresLinuxForProcesses(t *testing.T) {
	if runtime.GOOS == "linux" {
		return
	}
	if _, e := RunGit(context.Background(), "/", []string{"--version"}, nil); e == nil {
		t.Fatal("non-Linux execution enabled")
	}
}
