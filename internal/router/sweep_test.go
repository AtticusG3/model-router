package router

import (
	"testing"
	"time"
)

func TestSweepStaleEvictsExpiredTTL(t *testing.T) {
	r := loadTestRouter(t)
	r.cfg.Stanza("a").IdleTTLSeconds = 1
	if _, err := r.Load("a"); err != nil {
		t.Fatalf("Load a: %v", err)
	}
	if r.managed["a"] == nil || r.managed["a"].State() != StateRunning {
		t.Fatal("a should be running")
	}
	// fresh: no eviction
	r.SweepStale()
	if _, ok := r.managed["a"]; !ok {
		t.Fatal("fresh model must not be swept")
	}
	// stale: swept
	r.managed["a"].setLastUsed(time.Now().Add(-2 * time.Second))
	r.SweepStale()
	if _, ok := r.managed["a"]; ok {
		t.Fatal("stale model must be swept (actively evicted)")
	}
}

func TestSweepStaleSkipsResidentAndProtected(t *testing.T) {
	r := loadTestRouter(t)
	r.cfg.Stanza("a").IdleTTLSeconds = 0 // resident: never swept
	if _, err := r.Load("a"); err != nil {
		t.Fatalf("Load a: %v", err)
	}
	r.managed["a"].setLastUsed(time.Now().Add(-2 * time.Hour))
	r.SweepStale()
	if _, ok := r.managed["a"]; !ok {
		t.Fatal("ttl=0 resident must never be swept")
	}
	// protected: mid-request occupancy — mark a stale, hold occupancy, sweep
	r.occupancy["a"] = 1
	r.SweepStale()
	if _, ok := r.managed["a"]; !ok {
		t.Fatal("in-flight model must not be swept")
	}
	r.occupancy["a"] = 0
	r.SweepStale()
	// a is ttl=0 so still not swept even without occupancy
	if _, ok := r.managed["a"]; !ok {
		t.Fatal("ttl=0 resident must never be swept (2)")
	}
}
