package clock

import (
	"testing"
	"time"
)

func TestBackwardsClockLatchesFailure(t *testing.T) {
	tick := time.Second
	c := &Checked{Read: func() (time.Duration, error) { return tick, nil }}
	if _, e := c.Now(); e != nil {
		t.Fatal(e)
	}
	tick = 0
	if _, e := c.Now(); e == nil {
		t.Fatal("backwards elapsed clock accepted")
	}
	tick = 2 * time.Second
	if _, e := c.Now(); e == nil {
		t.Fatal("failed clock revived")
	}
	if Conservative(10000) >= 10*time.Second || Conservative(100) != 0 {
		t.Fatal("duration extended")
	}
}
