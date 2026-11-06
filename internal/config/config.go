package config

import (
	"errors"
	"net"
	"net/url"
	"os"
	"strings"
)

type Config struct{ Listen, Origin, DSNFile string }

func Load() (Config, error) {
	c := Config{Listen: os.Getenv("COSMOFORMER_LISTEN"), Origin: os.Getenv("COSMOFORMER_ORIGIN"), DSNFile: os.Getenv("COSMOFORMER_DATABASE_DSN_FILE")}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:18080"
	}
	return c, c.Validate()
}
func (c Config) Validate() error {
	u, e := url.Parse(c.Origin)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return errors.New("COSMOFORMER_ORIGIN must be an exact HTTPS origin without a path")
	}
	if _, _, e = net.SplitHostPort(c.Listen); e != nil {
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
