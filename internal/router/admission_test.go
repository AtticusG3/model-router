package router

import (
	"fmt"
	"io"
	"testing"
	"time"

	"model-router/internal/config"
)

func gpu(idx int, capMB int64) *GPUState {
	return &GPUState{Index: idx, FreeMB: capMB, TotalMB: capMB}
}

func TestReservePinnedDevice(t *testing.T) {
	l := NewLedger()
	l.SetGPUs([]*GPUState{gpu(0, 8000), gpu(1, 30000)})
	s := &config.Stanza{ModelID: "big", VramMB: 22000, Device: "CUDA1"}
	idx, ok := l.Reserve(s)
	if !ok {
		t.Fatal("expected admission")
	}
	if idx != 1 {
		t.Errorf("gpu = %d, want 1", idx)
	}
	l.Release("big")
}

func TestReservePinnedInsufficient(t *testing.T) {
	l := NewLedger()
	l.SetGPUs([]*GPUState{gpu(0, 8000), gpu(1, 30000)})
	s := &config.Stanza{ModelID: "big", VramMB: 22000, Device: "CUDA0"}
	if _, ok := l.Reserve(s); ok {
		t.Fatal("expected rejection on pinned GPU with insufficient VRAM")
	}
}

func TestReserveAutoPicksLargest(t *testing.T) {
	l := NewLedger()
	l.SetGPUs([]*GPUState{gpu(0, 8000), gpu(1, 30000)})
	s := &config.Stanza{ModelID: "auto", VramMB: 9000}
	idx, ok := l.Reserve(s)
	if !ok {
		t.Fatal("expected admission")
	}
	if idx != 1 {
		t.Errorf("gpu = %d, want 1 (largest free)", idx)
	}
}

func TestNoDoubleBooking(t *testing.T) {
	l := NewLedger()
	l.SetGPUs([]*GPUState{gpu(0, 10000)})
	a := &config.Stanza{ModelID: "a", VramMB: 6000, Device: "CUDA0"}
	b := &config.Stanza{ModelID: "b", VramMB: 6000, Device: "CUDA0"}
	if _, ok := l.Reserve(a); !ok {
		t.Fatal("a should admit")
	}
	// Second reservation on the same GPU must fail: free(10000) - 6000 < 6000.
	if _, ok := l.Reserve(b); ok {
		t.Fatal("b should be rejected while a holds 6000")
	}
	l.Release("a")
	if _, ok := l.Reserve(b); !ok {
		t.Fatal("b should admit after a released")
	}
}

func TestCanAdmitEvictingCreditsIdleReservation(t *testing.T) {
	l := NewLedger()
	l.SetGPUs([]*GPUState{gpu(1, 32768)})
	krea := &config.Stanza{ModelID: "krea2turbo", VramMB: 16000, Device: "1"}
	agents := &config.Stanza{ModelID: "agents-a1", VramMB: 22000, Device: "1"}
	if _, ok := l.Reserve(krea); !ok {
		t.Fatal("krea should admit")
	}
	if l.CanAdmit(agents) {
		t.Fatal("agents must not fit beside krea")
	}
	if !l.CanAdmitEvicting(agents, []string{"krea2turbo"}) {
		t.Fatal("agents should fit after idle-evicting krea")
	}
	if l.CanAdmitEvicting(agents, nil) {
		t.Fatal("agents must not fit while keeping krea")
	}
}

func TestReserveSameModelIdempotent(t *testing.T) {
	l := NewLedger()
	l.SetGPUs([]*GPUState{gpu(0, 10000)})
	s := &config.Stanza{ModelID: "a", VramMB: 6000, Device: "CUDA0"}
	if _, ok := l.Reserve(s); !ok {
		t.Fatal("first reserve failed")
	}
	if _, ok := l.Reserve(s); !ok {
		t.Fatal("second reserve of same model should be idempotent")
	}
}

func TestCanAdmitDoesNotReserve(t *testing.T) {
	l := NewLedger()
	l.SetGPUs([]*GPUState{gpu(0, 10000)})
	s := &config.Stanza{ModelID: "b", VramMB: 6000, Device: "CUDA0"}
	if !l.CanAdmit(s) {
		t.Fatal("b should be admissible")
	}
	// CanAdmit is a pure probe: it must not consume capacity. A separate 5000MB
	// model on the same GPU still fits only if b's 6000MB was NOT recorded.
	c := &config.Stanza{ModelID: "c", VramMB: 5000, Device: "CUDA0"}
	if _, ok := l.Reserve(c); !ok {
		t.Fatal("CanAdmit must not reserve: c should still fit after the probe")
	}
}

func TestCanAdmitKeepsExistingReservation(t *testing.T) {
	l := NewLedger()
	l.SetGPUs([]*GPUState{gpu(0, 10000)})
	a := &config.Stanza{ModelID: "a", VramMB: 6000, Device: "CUDA0"}
	if _, ok := l.Reserve(a); !ok {
		t.Fatal("a should admit")
	}
	if !l.CanAdmit(a) {
		t.Fatal("already-reserved model should report admissible")
	}
	// The probe must not have dropped a's reservation: a second 6000MB model
	// on the same GPU still can't fit (10000 - 6000 < 6000).
	b := &config.Stanza{ModelID: "b", VramMB: 6000, Device: "CUDA0"}
	if _, ok := l.Reserve(b); ok {
		t.Fatal("b should still be rejected while a holds 6000 (probe dropped the reservation)")
	}
}

func TestCanAdmitInsufficient(t *testing.T) {
	l := NewLedger()
	l.SetGPUs([]*GPUState{gpu(0, 10000)})
	s := &config.Stanza{ModelID: "big", VramMB: 22000, Device: "CUDA0"}
	if l.CanAdmit(s) {
		t.Fatal("big should not be admissible on a 10000MB GPU")
	}
}

func TestAdmitUsesLiveFreeAndLedger(t *testing.T) {
	l := NewLedger()
	// Other process occupying the card: live probe must block admission.
	l.SetGPUs([]*GPUState{{Index: 0, TotalMB: 10000, FreeMB: 500}})
	a := &config.Stanza{ModelID: "a", VramMB: 6000, Device: "CUDA0"}
	if _, ok := l.Reserve(a); ok {
		t.Fatal("must not admit 6000 when nvidia-smi free is 500")
	}

	l.SetGPUs([]*GPUState{{Index: 0, TotalMB: 10000, FreeMB: 8000}})
	if _, ok := l.Reserve(a); !ok {
		t.Fatal("should admit when live free is 8000")
	}
	// Spin-up lag: smi still shows 8000 free; ledger must stop a second 6000.
	b := &config.Stanza{ModelID: "b", VramMB: 6000, Device: "CUDA0"}
	if _, ok := l.Reserve(b); ok {
		t.Fatal("ledger must block double-book while smi has not dropped yet")
	}
	c := &config.Stanza{ModelID: "c", VramMB: 3000, Device: "CUDA0"}
	if _, ok := l.Reserve(c); !ok {
		t.Fatal("3000 should fit in remaining 4000 ledger / 8000 live")
	}
}

func loadTestRouter(t *testing.T) *Router {
	t.Helper()
	port, _ := healthServer(t)
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
start_port: 5900
stanzas:
  - model_id: a
    command: "fake --port {port}"
    vram_mb: 6000
    health_check: /
    spin_up_seconds: 5
    port: %d
  - model_id: b
    command: "fake --port {port}"
    vram_mb: 6000
    health_check: /
    spin_up_seconds: 5
`, port)))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	r := New(cfg, NewLogger(io.Discard, false), "test")
	r.spawn = func(string, []string) (Proc, error) { return newFakeProc(), nil }
	r.ledger.SetGPUs([]*GPUState{gpu(0, 10000)})
	t.Cleanup(func() {
		r.mu.Lock()
		ids := make([]string, 0, len(r.managed))
		for id := range r.managed {
			ids = append(ids, id)
		}
		r.mu.Unlock()
		for _, id := range ids {
			_ = r.Unload(id)
		}
	})
	return r
}

func TestLoadKeepsReservationUntilUnload(t *testing.T) {
	r := loadTestRouter(t)
	if _, err := r.Load("a"); err != nil {
		t.Fatalf("Load a: %v", err)
	}
	b := r.cfg.Stanza("b")
	if _, ok := r.ledger.Reserve(b); ok {
		t.Fatal("healthy Load must keep the reservation; b should not fit")
	}
}

func TestUnloadDeletesManagedAndReleases(t *testing.T) {
	r := loadTestRouter(t)
	if _, err := r.Load("a"); err != nil {
		t.Fatalf("Load a: %v", err)
	}
	first := r.managed["a"]
	if err := r.Unload("a"); err != nil {
		t.Fatalf("Unload a: %v", err)
	}
	if _, ok := r.managed["a"]; ok {
		t.Fatal("Unload must delete managed[id]")
	}
	if _, err := r.Load("a"); err != nil {
		t.Fatalf("second Load a: %v", err)
	}
	second := r.managed["a"]
	if second == nil || second == first {
		t.Fatal("second Load must create a new Managed")
	}
	r.Unload("a")
	if _, ok := r.ledger.Reserve(r.cfg.Stanza("b")); !ok {
		t.Fatal("b should admit after Unload released a")
	}
}

func TestFailedStartReleasesReservation(t *testing.T) {
	r := loadTestRouter(t)
	r.spawn = func(string, []string) (Proc, error) {
		return nil, fmt.Errorf("boom")
	}
	if _, err := r.Load("a"); err == nil {
		t.Fatal("expected Load to fail")
	}
	if _, ok := r.ledger.Reserve(r.cfg.Stanza("a")); !ok {
		t.Fatal("failed Start must release so the model can be reserved again")
	}
}

func TestLoadDoesNotEvictFreshNeighbor(t *testing.T) {
	r := loadTestRouter(t)
	r.cfg.Stanza("a").Device = "0"
	r.cfg.Stanza("b").Device = "0"
	r.cfg.Stanza("a").VramMB = 8000
	r.cfg.Stanza("b").VramMB = 8000
	r.cfg.Stanza("b").Port = r.cfg.Stanza("a").Port
	if _, err := r.Load("a"); err != nil {
		t.Fatalf("Load a: %v", err)
	}
	if _, err := r.Load("b"); err == nil {
		t.Fatal("Load b must not evict fresh a")
	}
	if r.managed["a"] == nil {
		t.Fatal("a must still be loaded")
	}
}

func TestLoadEvictsStaleNeighbor(t *testing.T) {
	r := loadTestRouter(t)
	r.cfg.Stanza("a").Device = "0"
	r.cfg.Stanza("b").Device = "0"
	r.cfg.Stanza("a").VramMB = 8000
	r.cfg.Stanza("b").VramMB = 8000
	r.cfg.Stanza("a").IdleTTLSeconds = 1
	r.cfg.Stanza("b").Port = r.cfg.Stanza("a").Port
	if _, err := r.Load("a"); err != nil {
		t.Fatalf("Load a: %v", err)
	}
	r.managed["a"].setLastUsed(time.Now().Add(-2 * time.Second))
	if _, err := r.Load("b"); err != nil {
		t.Fatalf("Load b should evict stale a: %v", err)
	}
	if _, ok := r.managed["a"]; ok {
		t.Fatal("stale a should have been evicted")
	}
	if r.managed["b"] == nil || r.managed["b"].State() != StateRunning {
		t.Fatal("b should be running")
	}
}

func TestLoadDoesNotEvictInFlight(t *testing.T) {
	r := loadTestRouter(t)
	r.cfg.Stanza("a").Device = "0"
	r.cfg.Stanza("b").Device = "0"
	r.cfg.Stanza("a").VramMB = 8000
	r.cfg.Stanza("b").VramMB = 8000
	r.cfg.Stanza("b").Port = r.cfg.Stanza("a").Port
	if _, err := r.Load("a"); err != nil {
		t.Fatalf("Load a: %v", err)
	}
	r.holdOccupancy("a")
	if _, err := r.Load("b"); err == nil {
		t.Fatal("Load b must not evict in-flight a")
	}
	if r.managed["a"] == nil {
		t.Fatal("a must still be loaded")
	}
}
