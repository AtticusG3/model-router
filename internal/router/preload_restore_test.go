package router

import (
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"model-router/internal/config"
)

// preloadConfig is a two-stanza node: "keep" is the preload model, "img"
// takes its whole card and idles out after a second.
func preloadConfig(preload string, port int) string {
	return fmt.Sprintf(`
start_port: 5900
preload: %s
stanzas:
  - model_id: keep
    command: "fake --port {port}"
    vram_mb: 6000
    health_check: /
    spin_up_seconds: 5
    port: %d
  - model_id: img
    command: "fake --port {port}"
    vram_mb: 6000
    health_check: /
    spin_up_seconds: 5
    idle_ttl_seconds: 1
    port: %d
`, preload, port, port)
}

func preloadRouter(t *testing.T, preload string) *Router {
	t.Helper()
	port, _ := healthServer(t)
	cfg, err := config.Parse([]byte(preloadConfig(preload, port)))
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

// evictKeep stands in for Preload(): keep comes up, then img takes its card.
func evictKeep(t *testing.T, r *Router) {
	t.Helper()
	if _, err := r.Load("keep"); err != nil {
		t.Fatalf("Load keep: %v", err)
	}
	if _, err := r.Load("img"); err != nil {
		t.Fatalf("Load img: %v", err)
	}
	if managedState(r, "keep") != StateStopped {
		t.Fatal("img must evict the preload model to fit")
	}
}

func managedState(r *Router, key string) ProcessState {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m := r.managed[key]; m != nil {
		return m.State()
	}
	return StateStopped
}

func waitForState(t *testing.T, r *Router, key string, want ProcessState) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if managedState(r, key) == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s state = %s, want %s", key, managedState(r, key), want)
}

func TestPreloadRestoredAfterEvictorIdlesOut(t *testing.T) {
	r := preloadRouter(t, "[keep]")
	evictKeep(t, r)

	// img idles out on its own TTL; its card is then free for keep.
	r.managed["img"].setLastUsed(time.Now().Add(-2 * time.Second))
	r.SweepStale()

	waitForState(t, r, "keep", StateRunning)
	if _, ok := r.managed["img"]; ok {
		t.Fatal("img should have been swept")
	}
	r.mu.Lock()
	keys := make([]string, 0, len(r.managed))
	for k := range r.managed {
		keys = append(keys, k)
	}
	r.mu.Unlock()
	if strings.Join(keys, ",") != "keep" {
		t.Fatalf("managed = %v, want only the restored preload", keys)
	}
}

func TestManuallyUnloadedPreloadStaysDown(t *testing.T) {
	r := preloadRouter(t, "[keep]")
	if _, err := r.Load("keep"); err != nil {
		t.Fatalf("Load keep: %v", err)
	}
	if err := r.Unload("keep"); err != nil {
		t.Fatalf("Unload keep: %v", err)
	}
	// Give a restore pass a chance to run before asserting it did not.
	r.SweepStale()
	time.Sleep(200 * time.Millisecond)
	if st := managedState(r, "keep"); st != StateStopped {
		t.Fatalf("a preload the operator unloaded must stay down, state = %s", st)
	}
}

func TestReloadDroppingPreloadStopsRestore(t *testing.T) {
	r := preloadRouter(t, "[keep]")
	evictKeep(t, r)
	r.mu.Lock()
	pending := len(r.evictedPreloads)
	r.mu.Unlock()
	if pending != 1 {
		t.Fatalf("pending preload restores = %d, want 1", pending)
	}

	next, err := config.Parse([]byte(preloadConfig("[]", r.cfg().Stanza("keep").Port)))
	if err != nil {
		t.Fatalf("next config: %v", err)
	}
	if err := r.Apply(next); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// img idles out; keep is no longer in preload, so it must not come back.
	r.managed["img"].setLastUsed(time.Now().Add(-2 * time.Second))
	r.SweepStale()
	time.Sleep(200 * time.Millisecond)
	if st := managedState(r, "keep"); st != StateStopped {
		t.Fatalf("a model dropped from preload must not be restored, state = %s", st)
	}
}
