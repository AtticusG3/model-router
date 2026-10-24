package router

import (
	"io"
	"testing"
	"time"

	"model-router/internal/config"
)

// A model that just became healthy stays resident for HoldSeconds even when
// another stanza needs its VRAM. Without the hold, a cold model with a long
// warm-up (one-off calibration) is evicted the instant it reports healthy,
// before a client can reach it.
func TestHoldProtectsRunningModelFromEviction(t *testing.T) {
	r := loadTestRouter(t)
	r.cfg.Stanza("a").HoldSeconds = 60
	r.cfg.Stanza("b").Device = "0"
	r.cfg.Stanza("a").Device = "0"

	if _, err := r.Load("a"); err != nil {
		t.Fatalf("Load a: %v", err)
	}
	if !r.protected("a") {
		t.Fatal("a running model inside its hold window must be protected")
	}

	// b needs the same GPU: admission would normally evict idle a.
	r.evictToFit("b", r.cfg.Stanza("b"))

	r.mu.Lock()
	_, stillLoaded := r.managed["a"]
	r.mu.Unlock()
	if !stillLoaded {
		t.Fatal("held model was evicted to admit another stanza")
	}
}

// Once the hold window closes the model becomes evictable again, so the
// protection cannot leak into normal admission.
func TestHoldExpires(t *testing.T) {
	r := loadTestRouter(t)
	r.cfg.Stanza("a").HoldSeconds = 1
	r.cfg.Stanza("a").Device = "0"
	r.cfg.Stanza("b").Device = "0"

	if _, err := r.Load("a"); err != nil {
		t.Fatalf("Load a: %v", err)
	}

	r.mu.Lock()
	m := r.managed["a"]
	r.mu.Unlock()
	m.mu.Lock()
	m.readyAt = time.Now().Add(-2 * time.Second)
	m.mu.Unlock()

	if r.protected("a") {
		t.Fatal("hold must expire once readyAt+HoldSeconds is in the past")
	}

	r.evictToFit("b", r.cfg.Stanza("b"))

	r.mu.Lock()
	_, stillLoaded := r.managed["a"]
	r.mu.Unlock()
	if stillLoaded {
		t.Fatal("expired hold must not protect the model any more")
	}
}

// HoldSeconds = 0 (the default) leaves admission unchanged: an idle model is
// evictable immediately.
func TestNoHoldByDefault(t *testing.T) {
	r := loadTestRouter(t)
	r.cfg.Stanza("a").Device = "0"
	r.cfg.Stanza("b").Device = "0"

	if _, err := r.Load("a"); err != nil {
		t.Fatalf("Load a: %v", err)
	}
	if r.protected("a") {
		t.Fatal("idle model with no hold must not be protected")
	}
}

// InHold is false for a stanza that never became healthy.
func TestInHoldRequiresRunning(t *testing.T) {
	m := newManaged(&config.Stanza{ModelID: "x", HoldSeconds: 10}, 0, NewLogger(io.Discard, false))
	if m.InHold(time.Now()) {
		t.Fatal("a never-started backend must not hold")
	}
}
