package runs

import (
	"testing"
	"time"
)

func TestReverseArrivalAndABA(t *testing.T) {
	if CanObserve("one", "one", 0, time.Second, 1, 2, 10, 11) {
		t.Fatal("old baseline accepted")
	}
	if CanObserve("one", "one", 0, time.Second, 1, 1, 10, 11) {
		t.Fatal("old ticket accepted")
	}
	if !CanObserve("one", "one", 0, time.Second, 1, 1, 11, 10) {
		t.Fatal("new unchanged observation refused")
	}
	if CanObserve("old", "new", 0, time.Second, 1, 1, 11, 10) || CanObserve("one", "one", 0, 30*time.Second, 1, 1, 11, 10) {
		t.Fatal("expired/process-local proof revived")
	}
}
