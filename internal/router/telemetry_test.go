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

func TestTelemetrySnapshotFreshStaleAndReclaim(t *testing.T) {
	r := loadTestRouter(t)
	r.cfg.Stanza("a").Device = "0"
	r.cfg.Stanza("a").IdleTTLSeconds = 1
	if _, err := r.Load("a"); err != nil {
		t.Fatalf("Load a: %v", err)
	}
	r.ledger.SetGPUs([]*GPUState{{Index: 0, TotalMB: 10000, FreeMB: 4000}})

	snap := r.TelemetrySnapshot()
	if len(snap.LoadedModels) != 1 || snap.LoadedModels[0].ID != "a" || snap.LoadedModels[0].Freshness != "fresh" {
		t.Fatalf("fresh snapshot = %+v", snap.LoadedModels)
	}
	if snap.GPUs[0].FreeMB != 4000 || snap.GPUs[0].FreeIfStaleEvictedMB != 4000 {
		t.Fatalf("fresh gpu free=%d reclaim=%d", snap.GPUs[0].FreeMB, snap.GPUs[0].FreeIfStaleEvictedMB)
	}
	if snap.GPUs[0].FreeIfIdleEvictedMB != 10000 {
		t.Fatalf("idle reclaim = %d, want 10000", snap.GPUs[0].FreeIfIdleEvictedMB)
	}

	r.managed["a"].setLastUsed(time.Now().Add(-2 * time.Second))
	snap = r.TelemetrySnapshot()
	if snap.LoadedModels[0].Freshness != "stale" {
		t.Fatalf("want stale, got %+v", snap.LoadedModels[0])
	}
	if snap.GPUs[0].FreeIfStaleEvictedMB != 10000 {
		t.Fatalf("stale reclaim = %d, want 10000", snap.GPUs[0].FreeIfStaleEvictedMB)
	}
	if r.managed["a"] == nil {
		t.Fatal("stale must not unload until another load needs VRAM")
	}
}

func TestTelemetrySnapshotReportsSlotsAndInFlight(t *testing.T) {
	r := loadTestRouter(t)
	r.cfg.Stanza("a").Slots = 2
	if _, err := r.Load("a"); err != nil {
		t.Fatalf("Load a: %v", err)
	}
	r.holdOccupancy("a")
	if r.occupancyOf("a") != 1 {
		t.Fatalf("occupancy = %d, want 1", r.occupancyOf("a"))
	}
	snap := r.TelemetrySnapshot()
	if len(snap.LoadedModels) != 1 {
		t.Fatalf("loaded = %+v", snap.LoadedModels)
	}
	got := snap.LoadedModels[0]
	if got.Slots != 2 || got.InFlight != 1 {
		t.Fatalf("slots=%d in_flight=%d, want 2/1", got.Slots, got.InFlight)
	}
}

type errSentinel string

func (e errSentinel) Error() string { return string(e) }
