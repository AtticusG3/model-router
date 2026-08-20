package router

import (
	"testing"
	"time"
)

func TestPeerCacheFreshSetErrAndStale(t *testing.T) {
	c := NewPeerCache(time.Hour)
	if c.Fresh("missing") {
		t.Fatal("unknown peer must not be fresh")
	}

	c.SetErr("broken", errSentinel("boom"))
	if c.Fresh("broken") {
		t.Fatal("error-only entry must not count as fresh telemetry")
	}

	c.Set("ok", &Telemetry{})
	if !c.Fresh("ok") {
		t.Fatal("just-set peer should be fresh")
	}

	stale := NewPeerCache(time.Millisecond)
	stale.Set("old", &Telemetry{})
	time.Sleep(5 * time.Millisecond)
	if stale.Fresh("old") {
		t.Fatal("telemetry older than the stale window must be ignored")
	}
}

type errSentinel string

func (e errSentinel) Error() string { return string(e) }
