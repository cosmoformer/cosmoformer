package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cosmoformer/cosmoformer/internal/config"
)

func TestPrivateAPIListener(t *testing.T) {
	dir, err := os.MkdirTemp("", "cf-api-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "api.sock")
	listener, err := listenAPI(config.Config{UnixSocket: socket})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(socket)
	if err != nil || info.Mode().Perm() != 0600 || info.Mode()&os.ModeSocket == 0 {
		t.Fatal("API socket permissions are unsafe", err)
	}
	if _, err = listenAPI(config.Config{UnixSocket: socket}); err == nil {
		t.Fatal("existing listener replaced")
	}
	listener.Close()
	if _, err = os.Stat(socket); !os.IsNotExist(err) {
		t.Fatal("private socket survived normal shutdown", err)
	}
	os.Chmod(dir, 0755)
	if _, err = listenAPI(config.Config{UnixSocket: socket}); err == nil {
		t.Fatal("public parent directory accepted")
	}
}
