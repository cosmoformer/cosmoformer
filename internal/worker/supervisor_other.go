//go:build !linux

package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"github.com/cosmoformer/cosmoformer/internal/source"
)

func RunGit(context.Context, string, []string, []string) ([]byte, error) {
	return nil, source.ErrProcess
}
func runProgram(context.Context, string, string, []string, []string) ([]byte, error) {
	return nil, source.ErrProcess
}
func hash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
