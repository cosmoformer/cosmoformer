//go:build !linux

package worker

import (
	"context"
	"github.com/cosmoformer/cosmoformer/internal/runs"
)

func runNative(context.Context, NativeCommand, func(runs.CommandEvent) error) (runs.CommandEvent, error) {
	return runs.CommandEvent{}, ErrAgent
}
