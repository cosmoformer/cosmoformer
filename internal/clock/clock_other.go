//go:build !linux

package clock

import (
	"errors"
	"time"
)

func New() Source {
	return &Checked{Read: func() (time.Duration, error) { return 0, errors.New("worker execution requires Linux CLOCK_BOOTTIME") }}
}
