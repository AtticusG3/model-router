package router

import (
	"testing"
	"time"
)

func TestPinnedStanzaIsNeverEvictable(t *testing.T) {
	r := loadTestRouter(t)
	r.cfg().Stanza("a").IdleTTLSeconds = 1
	r.cfg().Stanza("a").Pin = true
	if _, err := r.Load("a"); err != nil {
		t.Fatalf("Load a: %v", err)
	}
	for _, kind := range evictKinds {
		if r.evictable("a", kind) {
			t.Fatalf("pinned stanza must not be evictable (%s)", kind)
		}
	}
}

func TestUnpinnedStanzaStaysEvictable(t *testing.T) {
	r := loadTestRouter(t)
	r.cfg().Stanza("a").IdleTTLSeconds = 1
	if _, err := r.Load("a"); err != nil {
		t.Fatalf("Load a: %v", err)
	}
	r.managed["a"].setLastUsed(time.Now().Add(-2 * time.Second))
	if !r.evictable("a", evictStale) {
		t.Fatal("control: an unpinned stale stanza must be evictable")
	}
}

func TestPinnedStanzaSurvivesStaleSweep(t *testing.T) {
	r := loadTestRouter(t)
	r.cfg().Stanza("a").IdleTTLSeconds = 1
	r.cfg().Stanza("a").Pin = true
	if _, err := r.Load("a"); err != nil {
		t.Fatalf("Load a: %v", err)
	}
	r.managed["a"].setLastUsed(time.Now().Add(-2 * time.Hour))
	r.SweepStale()
	if _, ok := r.managed["a"]; !ok {
		t.Fatal("pinned model must not be swept even when stale")
	}
}
