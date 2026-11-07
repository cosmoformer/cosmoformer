//go:build linux

package clock

import (
	"golang.org/x/sys/unix"
	"time"
)

func New() Source {
	return &Checked{Read: func() (time.Duration, error) {
		var t unix.Timespec
		e := unix.ClockGettime(unix.CLOCK_BOOTTIME, &t)
		return time.Duration(t.Nano()), e
	}}
}
