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
	Listen, Origin, DSNFile, UnixSocket         string
	Artifact                                    evidence.Settings
	PlanSeconds, PreflightSeconds, PlanTTLHours int64
}

func Load() (Config, error) {
	c := Config{Listen: os.Getenv("COSMOFORMER_LISTEN"), Origin: os.Getenv("COSMOFORMER_ORIGIN"), DSNFile: os.Getenv("COSMOFORMER_DATABASE_DSN_FILE")}
	c.Artifact = evidence.Settings{Bucket: os.Getenv("COSMOFORMER_ARTIFACT_BUCKET"), Region: os.Getenv("COSMOFORMER_ARTIFACT_REGION"), Owner: os.Getenv("COSMOFORMER_ARTIFACT_EXPECTED_OWNER")}
	c.UnixSocket = os.Getenv("COSMOFORMER_UNIX_SOCKET")
	readBound := func(key string, fallback, lo, hi int64) (int64, error) {
		v := os.Getenv("COSMOFORMER_" + key)
		if v == "" {
			return fallback, nil
		}
		n, e := strconv.ParseInt(v, 10, 64)
		if e != nil || n < lo || n > hi {
			return 0, errors.New("invalid planning budget or TTL setting")
		}
		return n, nil
	}
	var budgetErr error
	if c.PlanSeconds, budgetErr = readBound("PLAN_SECONDS", 1200, 30, 3300); budgetErr != nil {
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
	if c.PlanSeconds != 0 && (c.PlanSeconds < 30 || c.PlanSeconds > 3300) || c.PreflightSeconds != 0 && (c.PreflightSeconds < 30 || c.PreflightSeconds > 3300) || c.PlanTTLHours != 0 && (c.PlanTTLHours < 1 || c.PlanTTLHours > 720) {
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
