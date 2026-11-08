package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/clock"
	"github.com/cosmoformer/cosmoformer/internal/config"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/source"
	"github.com/cosmoformer/cosmoformer/internal/worker"
)

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) == 3 && os.Args[1] == "credential-helper" {
		return worker.CredentialHelper(os.Args[2], os.Getenv("COSMOFORMER_GIT_CONTEXT"), os.Stdin, os.Stdout)
	}
	if len(os.Args) != 1 {
		return errors.New("worker accepts only credential-helper get/store/erase")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ticks := clock.New()
	if _, e := ticks.Now(); e != nil {
		return e
	}
	raw, e := config.ReadSecret(os.Getenv("COSMOFORMER_TOKEN_FILE"))
	if e != nil {
		return e
	}
	var credential struct {
		WorkerID string `json:"worker_id"`
		Token    string `json:"token"`
	}
	if runs.Decode([]byte(raw), &credential) != nil || !catalog.UUID(credential.WorkerID) {
		return errors.New("invalid protected worker identity file")
	}
	m, e := worker.VerifyRuntime(ctx, "/usr/local/share/cosmoformer/runtime.json")
	if e != nil {
		return errors.New("worker image/runtime identity did not verify")
	}
	runtimeID := os.Getenv("COSMOFORMER_RUNTIME_ID")
	image := os.Getenv("COSMOFORMER_IMAGE_DIGEST")
	if !catalog.UUID(runtimeID) || len(image) != 71 || image[:7] != "sha256:" {
		return errors.New("approved runtime ID and operator-verified image digest are required")
	}
	dir := os.Getenv("COSMOFORMER_ATTEMPT_DIR")
	if dir == "" {
		dir = "/var/lib/cosmoformer/attempts"
	}
	executable, e := os.Executable()
	if e != nil {
		return e
	}
	a := worker.Agent{HTTP: &http.Client{}, Origin: os.Getenv("COSMOFORMER_PLATFORM_ORIGIN"), Token: credential.Token, AttemptDir: dir, Clock: ticks, Git: source.Git{Run: worker.RunGit, Helper: executable}, Admission: runs.Admission{Protocol: 1, WorkerID: credential.WorkerID, Agent: runs.ID(), RuntimeID: runtimeID, Engine: m.Engine, Version: m.Version, Arch: m.Arch, ImageDigest: image, GitPackage: m.GitPackage, GitVersion: m.GitVersion, Phases: []string{"preflight"}, ClockSource: "boottime", ClockOK: true}}
	return a.Run(ctx)
}
