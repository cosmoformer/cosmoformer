package config

import (
	"errors"
	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	Listen, Origin, DSNFile, UnixSocket                       string
	Artifact                                                  evidence.Settings
	Cleanup                                                   evidence.CleanupSettings
	PlanSeconds, ApplySeconds, PreflightSeconds, PlanTTLHours int64
}

func Load() (Config, error) {
	c := Config{Listen: os.Getenv("COSMOFORMER_LISTEN"), Origin: os.Getenv("COSMOFORMER_ORIGIN"), DSNFile: os.Getenv("COSMOFORMER_DATABASE_DSN_FILE")}
	c.Artifact = evidence.Settings{Bucket: os.Getenv("COSMOFORMER_ARTIFACT_BUCKET"), Region: os.Getenv("COSMOFORMER_ARTIFACT_REGION"), Owner: os.Getenv("COSMOFORMER_ARTIFACT_EXPECTED_OWNER")}
	c.Cleanup = evidence.DefaultCleanupSettings()
	if enabled := os.Getenv("COSMOFORMER_CLEANUP_ENABLED"); enabled != "" {
		if enabled != "true" && enabled != "false" {
			return c, errors.New("invalid cleanup enabled setting")
		}
		c.Cleanup.Enabled = enabled == "true"
	}
	c.UnixSocket = os.Getenv("COSMOFORMER_UNIX_SOCKET")
	readBound := func(key string, fallback, lo, hi int64) (int64, error) {
		v := os.Getenv("COSMOFORMER_" + key)
		if v == "" {
			return fallback, nil
		}
		n, e := strconv.ParseInt(v, 10, 64)
		if e != nil || n < lo || n > hi {
			return 0, errors.New("invalid operational setting: " + key)
		}
		return n, nil
	}
	var budgetErr error
	if c.Cleanup.IntervalSeconds, budgetErr = readBound("CLEANUP_INTERVAL_SECONDS", 86400, 3600, 315360000); budgetErr != nil {
		return c, budgetErr
	}
	if c.Cleanup.RetentionDays, budgetErr = readBound("TERMINAL_RETENTION_DAYS", 30, 30, 365000); budgetErr != nil {
		return c, budgetErr
	}
	batch, cleanupErr := readBound("CLEANUP_BATCH", 100, 1, 100)
	if cleanupErr != nil {
		return c, cleanupErr
	}
	c.Cleanup.Batch = int(batch)
	if c.PlanSeconds, budgetErr = readBound("PLAN_SECONDS", 1200, 30, 3300); budgetErr != nil {
		return c, budgetErr
	}
	if c.ApplySeconds, budgetErr = readBound("APPLY_SECONDS", 2400, 30, 3300); budgetErr != nil {
		return c, budgetErr
	}
	if c.PreflightSeconds, budgetErr = readBound("PREFLIGHT_SECONDS", 300, 30, 3300); budgetErr != nil {
		return c, budgetErr
	}
	if c.PlanTTLHours, budgetErr = readBound("PLAN_TTL_HOURS", 168, 1, 720); budgetErr != nil {
		return c, budgetErr
	}
	if c.Listen == "" && c.UnixSocket == "" {
		c.Listen = "127.0.0.1:18080"
	}
	return c, c.Validate()
}
func (c Config) Validate() error {
	if c.Cleanup != (evidence.CleanupSettings{}) && c.Cleanup.Validate() != nil {
		return errors.New("invalid cleanup settings")
	}
	if c.Cleanup.Enabled && c.Artifact.Bucket == "" {
		return errors.New("cleanup requires configured artifact storage")
	}
	if c.ApplySeconds != 0 && (c.ApplySeconds < 30 || c.ApplySeconds > 3300) || c.PlanSeconds != 0 && (c.PlanSeconds < 30 || c.PlanSeconds > 3300) || c.PreflightSeconds != 0 && (c.PreflightSeconds < 30 || c.PreflightSeconds > 3300) || c.PlanTTLHours != 0 && (c.PlanTTLHours < 1 || c.PlanTTLHours > 720) {
		return errors.New("invalid planning budget or TTL setting")
	}

	if c.Artifact != (evidence.Settings{}) {
		if e := c.Artifact.Validate(); e != nil {
			return errors.New("invalid artifact storage settings")
		}
	}
	u, e := url.Parse(c.Origin)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return errors.New("COSMOFORMER_ORIGIN must be an exact HTTPS origin without a path")
	}
	if c.UnixSocket != "" {
		if c.Listen != "" || !filepath.IsAbs(c.UnixSocket) {
			return errors.New("Unix socket requires an absolute path and no TCP listen address")
		}
	} else if _, _, e = net.SplitHostPort(c.Listen); e != nil {
		return errors.New("invalid listen address")
	}
	if c.DSNFile == "" {
		return errors.New("COSMOFORMER_DATABASE_DSN_FILE is required")
	}
	return nil
}
func ReadSecret(path string) (string, error) {
	f, e := os.Open(path)
	if e != nil {
		return "", errors.New("cannot open protected input file")
	}
	defer f.Close()
	s, e := f.Stat()
	if e != nil || !s.Mode().IsRegular() || s.Mode().Perm()&0077 != 0 || s.Size() == 0 || s.Size() > 16384 {
		return "", errors.New("protected input must be a regular file, at most 16 KiB, readable only by its owner")
	}
	b := make([]byte, s.Size())
	_, e = f.ReadAt(b, 0)
	if e != nil {
		return "", errors.New("cannot read protected input")
	}
	return strings.TrimSuffix(string(b), "\n"), nil
}

// ReadKey reads raw key bytes without text trimming or implicit encodings.
func ReadKey(path string) ([]byte, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, errors.New("cannot open encryption key file")
	}
	defer f.Close()
	s, e := f.Stat()
	if e != nil || !s.Mode().IsRegular() || s.Mode().Perm()&0077 != 0 || s.Size() != 32 {
		return nil, errors.New("encryption key must be an owner-only regular file of exactly 32 raw bytes")
	}
	b := make([]byte, 32)
	if _, e = io.ReadFull(f, b); e != nil {
		return nil, errors.New("cannot read encryption key file")
	}
	return b, nil
}
