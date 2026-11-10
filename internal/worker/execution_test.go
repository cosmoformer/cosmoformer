package worker

import (
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPermissionRequestAnchorAndSingleUse(t *testing.T) {
	id := runs.Identity{WorkerID: runs.ID(), Generation: 1, Agent: runs.ID()}
	permit := runs.Permission{ID: runs.ID(), Ordinal: 1, Digest: strings.Repeat("a", 64), Platform: runs.ID(), RemainingMS: 10000, Expires: time.Unix(100, 0).UTC()}
	p, e := NewLaunchPermission(id, "attempt", permit, 10*time.Second, 30*time.Second)
	if e != nil {
		t.Fatal(e)
	}
	// A delayed response/replay cannot start a new duration at receipt.
	if e = p.Replay(permit, 18*time.Second); e != nil {
		t.Fatal(e)
	}
	if e = p.Claim(id, "attempt", permit.Digest, 20*time.Second); e == nil {
		t.Fatal("replay extended deadline")
	}
	proof, e := p.DiscardExpired(id, "attempt", 20*time.Second)
	if e != nil || proof.Permission != permit.ID || proof.ProofReason != "local_deadline_expired_discarded" {
		t.Fatal("never-started proof", e)
	}
	if e = p.Claim(id, "attempt", permit.Digest, 11*time.Second); e == nil {
		t.Fatal("discarded permission reused")
	}
	p, e = NewLaunchPermission(id, "attempt", permit, time.Second, 30*time.Second)
	if e != nil {
		t.Fatal(e)
	}
	other := permit
	other.Platform = runs.ID()
	if p.Replay(other, 2*time.Second) == nil {
		t.Fatal("cross-instance replay accepted")
	}
	if p.Claim(runs.Identity{WorkerID: id.WorkerID, Generation: 2, Agent: id.Agent}, "attempt", permit.Digest, 2*time.Second) == nil {
		t.Fatal("wrong generation")
	}
	if p.Claim(id, "attempt", strings.Repeat("b", 64), 2*time.Second) == nil {
		t.Fatal("wrong descriptor")
	}
	if e = p.Claim(id, "attempt", permit.Digest, 2*time.Second); e != nil {
		t.Fatal(e)
	}
	if p.Claim(id, "attempt", permit.Digest, 2*time.Second) == nil {
		t.Fatal("permission used twice")
	}
	if _, e = p.DiscardExpired(id, "attempt", 20*time.Second); e == nil {
		t.Fatal("spawn claimant fabricated never-started proof")
	}
}
func TestPermissionSpawnDiscardRace(t *testing.T) {
	for i := 0; i < 100; i++ {
		id := runs.Identity{WorkerID: runs.ID(), Generation: 1, Agent: runs.ID()}
		permit := runs.Permission{ID: runs.ID(), Ordinal: 1, Digest: strings.Repeat("a", 64), Platform: runs.ID(), RemainingMS: 1000}
		p, e := NewLaunchPermission(id, "attempt", permit, 0, 10*time.Second)
		if e != nil {
			t.Fatal(e)
		}
		var claim, discard error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); claim = p.Claim(id, "attempt", permit.Digest, p.deadline-time.Nanosecond) }()
		go func() { defer wg.Done(); _, discard = p.DiscardExpired(id, "attempt", p.deadline) }()
		wg.Wait()
		if (claim == nil) == (discard == nil) {
			t.Fatal("launch/discard did not serialize", claim, discard)
		}
	}
}

func TestCanonicalSecretNumbers(t *testing.T) {
	for input, want := range map[string]string{"92713.0": "92713", "9.2713e4": "92713", "-0.000": "0", "-1.2300e-2": "-0.0123", "1e-4": "0.0001", "0.10e2": "10"} {
		got, e := canonicalSecretNumber(input)
		if e != nil || got != want {
			t.Fatalf("canonical numeric protection: %q -> %q, %v", input, got, e)
		}
	}
	if _, e := canonicalSecretNumber("1e99999999"); e == nil {
		t.Fatal("unbounded scalar expansion accepted")
	}
}

func TestPermissionFinalSpawnBoundaryRechecksElapsedTime(t *testing.T) {
	id := runs.Identity{WorkerID: runs.ID(), Generation: 1, Agent: runs.ID()}
	permission := runs.Permission{ID: runs.ID(), Ordinal: 1, Digest: strings.Repeat("a", 64), Platform: runs.ID(), RemainingMS: 1000}
	p, e := NewLaunchPermission(id, "attempt", permission, 0, time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	if p.Check(id, "attempt", permission.Digest, 100*time.Millisecond) != nil {
		t.Fatal("early check failed")
	}
	called := false
	if p.Spawn(id, "attempt", permission.Digest, agentClock{tick: 2 * time.Second}, func() error { called = true; return nil }) == nil || called {
		t.Fatal("delayed exec crossed expired launch boundary")
	}
	if _, e = p.DiscardExpired(id, "attempt", 2*time.Second); e != nil {
		t.Fatal("never-started expired permission could not be discarded", e)
	}
	if p.Spawn(id, "attempt", permission.Digest, agentClock{tick: 100 * time.Millisecond}, func() error { called = true; return nil }) == nil || called {
		t.Fatal("discarded permission spawned")
	}
}
