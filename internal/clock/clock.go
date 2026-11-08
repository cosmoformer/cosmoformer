// Package clock supplies process-local elapsed deadlines. Ticks are never shared
// across process instances or reconstructed from wall-clock timestamps.
package clock

import (
	"errors"
	"sync"
	"time"
)

type Source interface{ Now() (time.Duration, error) }
type Checked struct {
	mu     sync.Mutex
	last   time.Duration
	failed bool
	Read   func() (time.Duration, error)
}

func (c *Checked) Now() (time.Duration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, e := c.Read()
	if e != nil || n < c.last || c.failed {
		c.failed = true
		return 0, errors.New("elapsed clock unavailable")
	}
	c.last = n
	return n, nil
}
func Conservative(b int64) time.Duration {
	if b <= 100 || b > 3600000 {
		return 0
	}
	return time.Duration(max(int64(0), b*9979/10000-100)) * time.Millisecond
}
