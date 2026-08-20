package router

import (
	"testing"

	"model-router/internal/config"
)

func gpu(idx int, free int64) *GPUState {
	return &GPUState{Index: idx, FreeMB: free, TotalMB: 32768}
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
