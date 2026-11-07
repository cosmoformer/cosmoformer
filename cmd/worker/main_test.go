package main

import (
	"os"
	"testing"
)

func TestWorkerCommandBoundary(t *testing.T) {
	old := os.Args
	defer func() { os.Args = old }()
	os.Args = []string{"worker", "unexpected"}
	if run() == nil {
		t.Fatal("accepted an arbitrary worker command")
	}
	os.Args = []string{"worker", "credential-helper", "store"}
	if e := run(); e != nil {
		t.Fatal("credential store should discard", e)
	}
	os.Args = []string{"worker", "credential-helper", "erase"}
	if e := run(); e != nil {
		t.Fatal("credential erase should discard", e)
	}
	os.Args = []string{"worker", "credential-helper", "get"}
	t.Setenv("COSMOFORMER_GIT_CONTEXT", "/nonexistent-cosmoformer-test-context")
	if run() == nil {
		t.Fatal("helper retrieved credentials without an attempt context")
	}
}
