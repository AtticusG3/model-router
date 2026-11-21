package router

import (
	"encoding/json"
	"testing"
	"time"

	"model-router/internal/config"
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
	c.SetErr("ok", errSentinel("later"))
	if !c.Fresh("ok") {
		t.Fatal("a later error must keep the last good snapshot")
	}
	if c.Snapshot()["ok"] == nil {
		t.Fatal("snapshot must survive SetErr")
	}

	stale := NewPeerCache(time.Millisecond)
	stale.Set("old", &Telemetry{})
	time.Sleep(5 * time.Millisecond)
	stale.SetErr("old", errSentinel("later"))
	if stale.Fresh("old") {
		t.Fatal("an error must not refresh the stale window")
	}
}

func TestTelemetrySnapshotFreshStaleAndReclaim(t *testing.T) {
	r := loadTestRouter(t)
	r.cfg().Stanza("a").Device = "0"
	r.cfg().Stanza("a").IdleTTLSeconds = 1
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
	r.cfg().Stanza("a").Slots = 2
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

func TestTelemetrySnapshotIdleReclaimSkipsInFlight(t *testing.T) {
	r := loadTestRouter(t)
	if _, err := r.Load("a"); err != nil {
		t.Fatalf("Load a: %v", err)
	}
	r.ledger.SetGPUs([]*GPUState{{Index: 0, TotalMB: 10000, FreeMB: 4000}})
	r.holdOccupancy("a")
	snap := r.TelemetrySnapshot()
	if snap.GPUs[0].FreeIfIdleEvictedMB != 4000 {
		t.Fatalf("in-flight must not count as idle reclaim, got %d", snap.GPUs[0].FreeIfIdleEvictedMB)
	}
	r.releaseOccupancy(Target{Local: "a"})
	snap = r.TelemetrySnapshot()
	if snap.GPUs[0].FreeIfIdleEvictedMB != 10000 {
		t.Fatalf("idle reclaim after release = %d, want 10000", snap.GPUs[0].FreeIfIdleEvictedMB)
	}
}

func TestTelemetrySnapshotListsEveryGPUOfASplitModel(t *testing.T) {
	r := loadTestRouter(t)
	r.ledger.SetGPUs([]*GPUState{gpu(0, 10000), gpu(1, 10000), gpu(2, 10000)})
	r.cfg().Stanza("a").Device = "1,2"
	r.cfg().Stanza("b").Device = "0"
	r.cfg().Stanza("b").Port = r.cfg().Stanza("a").Port
	for _, id := range []string{"a", "b"} {
		if _, err := r.Load(id); err != nil {
			t.Fatalf("Load %s: %v", id, err)
		}
	}
	got := map[string][]int{}
	for _, lm := range r.TelemetrySnapshot().LoadedModels {
		got[lm.ID] = lm.GPUs
	}
	if len(got["a"]) != 2 || got["a"][0] != 1 || got["a"][1] != 2 {
		t.Fatalf("split model gpus = %v, want [1 2]", got["a"])
	}
	if len(got["b"]) != 1 || got["b"][0] != 0 {
		t.Fatalf("single-GPU model gpus = %v, want [0]", got["b"])
	}
}

func TestTelemetrySnapshotAdvertisesImagePipeline(t *testing.T) {
	r := loadTestRouter(t)
	r.cfg().Stanza("a").ImagePipeline = &config.ImagePipeline{Caption: "ideogram4"}
	got := r.TelemetrySnapshot().Models["a"]
	if got == nil || got.Caption != "ideogram4" {
		t.Fatalf("models = %+v, want a with caption ideogram4", got)
	}
	// Advertised before the model runs, so a peer that idle-evicted it still
	// ranks as matching.
	if len(r.TelemetrySnapshot().LoadedModels) != 0 {
		t.Fatal("model must not be reported loaded")
	}
}

func TestTelemetryParsesWithoutImagePipeline(t *testing.T) {
	var tel Telemetry
	raw := `{"node":"old","loaded_models":[{"id":"m","freshness":"fresh","gpu":0,"slots":1,"in_flight":0}]}`
	if err := json.Unmarshal([]byte(raw), &tel); err != nil {
		t.Fatalf("older telemetry must parse: %v", err)
	}
	if len(tel.LoadedModels) != 1 || tel.Models != nil {
		t.Fatalf("loaded_models = %+v, models = %+v, want no image pipeline", tel.LoadedModels, tel.Models)
	}
}

type errSentinel string

func (e errSentinel) Error() string { return string(e) }
