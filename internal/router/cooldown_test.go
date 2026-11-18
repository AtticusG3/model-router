package router

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"model-router/internal/config"
)

// coolRig is a router with two models: "bad" never becomes healthy (until
// badOK is set) and "good" always does. A peer that counts every call sits in
// the config so tests can show this node never forwards.
type coolRig struct {
	r         *Router
	h         http.Handler
	badSpawns atomic.Int32
	badOK     atomic.Bool
	peerHits  atomic.Int32
	goodMu    sync.Mutex
	goodProcs []*fakeProc
}

func newCoolRig(t *testing.T, goodHandler http.HandlerFunc, badExtra ...string) *coolRig {
	t.Helper()
	rig := &coolRig{}
	badPort, _ := healthServer(t)
	good := httptest.NewServer(goodHandler)
	t.Cleanup(good.Close)
	goodPort := good.Listener.Addr().(*net.TCPAddr).Port
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rig.peerHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(peer.Close)

	cfg, err := config.Parse([]byte(fmt.Sprintf(`
start_port: 5900
stanzas:
  - model_id: bad
    command: "bad --port {port}"
    env: ["CUDA_VISIBLE_DEVICES={gpu}"]
    vram_mb: 3000
    health_check: /
    spin_up_seconds: 5
    match: {body_field: model}
    port: %d
    %s
  - model_id: good
    command: "good --port {port}"
    env: ["CUDA_VISIBLE_DEVICES={gpu}"]
    vram_mb: 3000
    health_check: /
    spin_up_seconds: 5
    match: {body_field: model}
    port: %d
peers:
  - name: other
    kind: router
    base_url: %s
    models: [bad]
`, badPort, strings.Join(badExtra, "\n    "), goodPort, peer.URL)))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	r := New(cfg, NewLogger(io.Discard, false), "test")
	r.spawn = func(cmd string, _ []string) (Proc, error) {
		fp := newFakeProc()
		if strings.HasPrefix(cmd, "bad") {
			rig.badSpawns.Add(1)
			if !rig.badOK.Load() {
				fp.alive = false
			}
			return fp, nil
		}
		rig.goodMu.Lock()
		rig.goodProcs = append(rig.goodProcs, fp)
		rig.goodMu.Unlock()
		return fp, nil
	}
	r.ledger.SetGPUs([]*GPUState{gpu(0, 10000)})
	t.Cleanup(func() { _ = r.Unload("bad"); _ = r.Unload("good") })
	rig.r = r
	rig.h = NewHandler(r, NewLogger(io.Discard, false))
	return rig
}

func (rig *coolRig) chat(model string, hdr ...string) *httptest.ResponseRecorder {
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, model)
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	rig.h.ServeHTTP(rec, req)
	return rec
}

func (rig *coolRig) peerLoad(model string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/_router/load", strings.NewReader(fmt.Sprintf(`{"model_id":%q}`, model)))
	rec := httptest.NewRecorder()
	rig.h.ServeHTTP(rec, req)
	return rec
}

// expire pretends the open window has passed, so the next request may start.
func (rig *coolRig) expire(model string) {
	rig.r.coolMu.Lock()
	rig.r.cooldowns[model].until = time.Now().Add(-time.Second)
	rig.r.coolMu.Unlock()
}

func retryAfterOf(t *testing.T, rec *httptest.ResponseRecorder) int {
	t.Helper()
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body = %s", rec.Code, rec.Body.String())
	}
	n, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil {
		t.Fatalf("Retry-After = %q: %v", rec.Header().Get("Retry-After"), err)
	}
	return n
}

func okChat(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"chatcmpl-good"}`))
}

func TestStartCooldownWindowsDoubleAndCap(t *testing.T) {
	rig := newCoolRig(t, okChat)

	// First failed start: retried once inside the start, then 503 + 10s.
	rec := rig.chat("bad")
	if got := retryAfterOf(t, rec); got < 9 || got > 10 {
		t.Fatalf("first Retry-After = %d, want 10", got)
	}
	if got := rig.badSpawns.Load(); got != 2 {
		t.Fatalf("spawns in one failed start = %d, want 2 (one retry)", got)
	}

	// Each consecutive failed start doubles the window, up to 5 minutes.
	for _, want := range []int{20, 40, 80, 160, 300, 300} {
		rig.expire("bad")
		if got := retryAfterOf(t, rig.chat("bad")); got < want-1 || got > want {
			t.Fatalf("Retry-After = %d, want %d", got, want)
		}
	}
}

func TestStartCooldownRefusesAtOnceWithoutSpawningOrEvicting(t *testing.T) {
	rig := newCoolRig(t, okChat)
	if rec := rig.chat("good"); rec.Code != http.StatusOK {
		t.Fatalf("good: %d %s", rec.Code, rec.Body.String())
	}
	retryAfterOf(t, rig.chat("bad"))
	spawns := rig.badSpawns.Load()

	start := time.Now()
	rec := rig.chat("bad")
	if got := retryAfterOf(t, rec); got < 1 || got > 10 {
		t.Fatalf("Retry-After = %d, want 1..10", got)
	}
	if time.Since(start) > 150*time.Millisecond {
		t.Fatal("a request inside the window must fail at once")
	}
	if got := rig.badSpawns.Load(); got != spawns {
		t.Fatalf("spawned %d more times inside the window", got-spawns)
	}
	if rig.r.managed["good"] == nil || rig.r.managed["good"].State() != StateRunning {
		t.Fatal("a refused request must not evict a running model")
	}
}

func TestStartCooldownOtherModelStillStarts(t *testing.T) {
	rig := newCoolRig(t, okChat)
	retryAfterOf(t, rig.chat("bad"))
	if rec := rig.chat("good"); rec.Code != http.StatusOK {
		t.Fatalf("good during bad's window: %d %s", rec.Code, rec.Body.String())
	}
}

func TestStartCooldownHealthyStartResetsWindow(t *testing.T) {
	rig := newCoolRig(t, okChat)
	retryAfterOf(t, rig.chat("bad"))
	rig.expire("bad")
	retryAfterOf(t, rig.chat("bad")) // second failure: 20s

	rig.expire("bad")
	rig.badOK.Store(true)
	if rec := rig.chat("bad"); rec.Code != http.StatusOK && rec.Code != http.StatusBadGateway {
		t.Fatalf("healthy start: %d %s", rec.Code, rec.Body.String())
	}
	if rig.r.managed["bad"].State() != StateRunning {
		t.Fatal("bad should be running once it reports healthy")
	}

	// Stop it, break it again: the next window is back to 10s, not 40s.
	_ = rig.r.Unload("bad")
	rig.badOK.Store(false)
	if got := retryAfterOf(t, rig.chat("bad")); got < 9 || got > 10 {
		t.Fatalf("Retry-After after a healthy start = %d, want 10", got)
	}
}

func TestStartCooldownCoversEveryCopy(t *testing.T) {
	rig := newCoolRig(t, okChat, "max_instances: 2")
	retryAfterOf(t, rig.chat("bad"))
	spawns := rig.badSpawns.Load()
	if _, err := rig.r.Load("bad#1"); err == nil || rig.badSpawns.Load() != spawns {
		t.Fatalf("copy 1 must share the window and not spawn: err=%v", err)
	}
}

func TestStartCooldownPeerLoadGetsSameRefusalAndNothingForwards(t *testing.T) {
	rig := newCoolRig(t, okChat)
	retryAfterOf(t, rig.peerLoad("bad"))
	spawns := rig.badSpawns.Load()

	if got := retryAfterOf(t, rig.peerLoad("bad")); got < 1 || got > 10 {
		t.Fatalf("peer Retry-After = %d, want 1..10", got)
	}
	// A proxied request from a peer is refused the same way, not bounced.
	retryAfterOf(t, rig.chat("bad", "X-Model-Router-Peer", "other"))
	if rig.badSpawns.Load() != spawns {
		t.Fatal("a peer load inside the window spawned the model")
	}
	if got := rig.peerHits.Load(); got != 0 {
		t.Fatalf("this node called another node %d times", got)
	}
}

func TestCrashRestartIsNotStartCooldown(t *testing.T) {
	rig := newCoolRig(t, okChat)
	if rec := rig.chat("good"); rec.Code != http.StatusOK {
		t.Fatalf("good: %d", rec.Code)
	}
	rig.goodMu.Lock()
	first := rig.goodProcs[0]
	rig.goodMu.Unlock()
	first.exit(fmt.Errorf("crashed"))

	// The supervisor restarts it after 1 second, with no cooldown in the way.
	deadline := time.Now().Add(5 * time.Second)
	for {
		rig.goodMu.Lock()
		n := len(rig.goodProcs)
		rig.goodMu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("crashed model was not restarted")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := rig.r.startRefused("good"); err != nil {
		t.Fatalf("a crash must not open a start cooldown: %v", err)
	}
	if rec := rig.chat("good"); rec.Code != http.StatusOK {
		t.Fatalf("response after restart: %d %s", rec.Code, rec.Body.String())
	}
}

func TestStartCooldownLeavesInFlightGenerationRunning(t *testing.T) {
	release := make(chan struct{})
	inFlight := make(chan struct{})
	rig := newCoolRig(t, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v1/chat/completions" {
			return // health check
		}
		close(inFlight)
		<-release
		okChat(w, req)
	})

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- rig.chat("good") }()
	<-inFlight

	retryAfterOf(t, rig.chat("bad"))
	retryAfterOf(t, rig.chat("bad"))

	close(release)
	if rec := <-done; rec.Code != http.StatusOK {
		t.Fatalf("in-flight generation: %d %s", rec.Code, rec.Body.String())
	}
}
