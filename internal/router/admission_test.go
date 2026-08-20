package router

import (
	"fmt"
	"io"
	"testing"

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

func TestAdmitFromTotalsNotPolledFree(t *testing.T) {
	l := NewLedger()
	// nvidia-smi shows almost no free VRAM; admission still uses TotalMB.
	l.SetGPUs([]*GPUState{{Index: 0, TotalMB: 10000, FreeMB: 500}})
	a := &config.Stanza{ModelID: "a", VramMB: 6000, Device: "CUDA0"}
	if _, ok := l.Reserve(a); !ok {
		t.Fatal("should admit from TotalMB even when polled FreeMB is 500")
	}
	// Poll now reflects the running model; reservation must not double-count.
	l.SetGPUs([]*GPUState{{Index: 0, TotalMB: 10000, FreeMB: 4000}})
	b := &config.Stanza{ModelID: "b", VramMB: 3000, Device: "CUDA0"}
	if _, ok := l.Reserve(b); !ok {
		t.Fatal("3000 should fit in remaining 4000 (total minus reservations)")
	}
	c := &config.Stanza{ModelID: "c", VramMB: 2000, Device: "CUDA0"}
	if _, ok := l.Reserve(c); ok {
		t.Fatal("2000 should not fit in remaining 1000")
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
