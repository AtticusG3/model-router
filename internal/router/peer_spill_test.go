package router

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"model-router/internal/config"
)

// peerSpillRig is a local image stanza (shared-model) next to one peer that
// advertises the same id. embed occupies the only GPU in the blocked test.
type peerSpillRig struct {
	r    *Router
	peer *walkPeer
}

func newPeerSpillRig(t *testing.T, health http.Handler, spawn func(string, []string) (Proc, error), stanzaExtra string) *peerSpillRig {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/health" {
			health.ServeHTTP(w, req)
			return
		}
		_, _ = io.WriteString(w, "local")
	}))
	t.Cleanup(backend.Close)
	port := backend.Listener.Addr().(*net.TCPAddr).Port
	peer := newWalkPeer(t, http.StatusOK, "")
	extraLine := ""
	if stanzaExtra != "" {
		extraLine = "    " + stanzaExtra + "\n"
	}
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
start_port: 5900
stanzas:
  - model_id: embed
    command: "embed --port {port}"
    vram_mb: 6000
    device: "0"
    health_check: /health
    spin_up_seconds: 5
    port: %d
  - model_id: shared-model
    command: "local --port {port}"
    vram_mb: 6000
    device: "0"
    api_type: image
    match: {path_prefix: /sdapi/v1}
    health_check: /health
    spin_up_seconds: 5
    port: %d
%s
peers:
  - name: alpha
    kind: router
    base_url: %s
    models: [shared-model]
`, port, port, extraLine, peer.srv.URL)))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	r := New(cfg, NewLogger(io.Discard, false), "test")
	r.spawn = spawn
	r.ledger.SetGPUs([]*GPUState{gpu(0, 10000)})
	r.peers.Set("alpha", &Telemetry{Node: "alpha"})
	t.Cleanup(func() {
		_ = r.Unload("shared-model")
		_ = r.Unload("embed")
	})
	return &peerSpillRig{r: r, peer: peer}
}

func peerSpillImage(r *Router) *httptest.ResponseRecorder {
	body := `{"model":"shared-model","prompt":"cat"}`
	req := httptest.NewRequest(http.MethodPost, "/sdapi/v1/txt2img", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	NewHandler(r, NewLogger(io.Discard, false)).ServeHTTP(rec, req)
	return rec
}

func deadProc() *fakeProc {
	fp := newFakeProc()
	fp.mu.Lock()
	fp.alive = false
	fp.mu.Unlock()
	return fp
}

func okHealth(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }

func TestPeerSpillOnFailureWaitsForLocalStart(t *testing.T) {
	rig := newPeerSpillRig(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}), func(string, []string) (Proc, error) { return newFakeProc(), nil }, "peer_spill: on_failure")

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- peerSpillImage(rig.r) }()

	deadline := time.Now().Add(3 * time.Second)
	for {
		rig.r.mu.Lock()
		m := rig.r.managed["shared-model"]
		rig.r.mu.Unlock()
		if m != nil && m.State() == StateStarting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("local model never entered starting")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rig.peer.loads.Load() != 0 || rig.peer.chats.Load() != 0 {
		t.Fatalf("peer asked during the local start: loads=%d chats=%d", rig.peer.loads.Load(), rig.peer.chats.Load())
	}

	select {
	case rec := <-done:
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "local") {
			t.Fatalf("status = %d, body = %q, want the local answer", rec.Code, rec.Body.String())
		}
	case <-time.After(6 * time.Second):
		t.Fatal("request did not finish after the local start")
	}
	if rig.peer.loads.Load() != 0 || rig.peer.chats.Load() != 0 {
		t.Fatalf("peer asked for the served request: loads=%d chats=%d", rig.peer.loads.Load(), rig.peer.chats.Load())
	}
}

func TestPeerSpillOnFailureWaitsForProtectedOccupant(t *testing.T) {
	rig := newPeerSpillRig(t, http.HandlerFunc(okHealth), func(string, []string) (Proc, error) { return newFakeProc(), nil }, "peer_spill: on_failure")
	if _, err := rig.r.Load("embed"); err != nil {
		t.Fatalf("Load embed: %v", err)
	}
	rig.r.holdOccupancy("embed")
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { rig.r.releaseOccupancy(Target{Local: "embed"}) }) }
	defer release()

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- peerSpillImage(rig.r) }()

	deadline := time.Now().Add(2 * time.Second)
	for rig.r.occupancyOf("shared-model") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("request never took a local slot")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case rec := <-done:
		t.Fatalf("request finished while the GPU was blocked: %d %s", rec.Code, rec.Body.String())
	case <-time.After(150 * time.Millisecond):
	}
	if rig.peer.loads.Load() != 0 || rig.peer.chats.Load() != 0 {
		t.Fatalf("peer asked while the GPU was blocked: loads=%d chats=%d", rig.peer.loads.Load(), rig.peer.chats.Load())
	}

	release()
	select {
	case rec := <-done:
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "local") {
			t.Fatalf("status = %d, body = %q, want the local answer", rec.Code, rec.Body.String())
		}
	case <-time.After(6 * time.Second):
		t.Fatal("request did not finish after the occupant released the GPU")
	}
	if rig.peer.loads.Load() != 0 || rig.peer.chats.Load() != 0 {
		t.Fatalf("peer asked for the served request: loads=%d chats=%d", rig.peer.loads.Load(), rig.peer.chats.Load())
	}
}

func TestPeerSpillOnFailureFallsBackAfterHardStartFailure(t *testing.T) {
	rig := newPeerSpillRig(t, http.HandlerFunc(okHealth), func(string, []string) (Proc, error) { return deadProc(), nil }, "peer_spill: on_failure")

	rec := peerSpillImage(rig.r)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "served") {
		t.Fatalf("status = %d, body = %q, want the peer's answer", rec.Code, rec.Body.String())
	}
	if rig.peer.chats.Load() != 1 {
		t.Fatalf("peer chats = %d, want 1 after the hard start failure", rig.peer.chats.Load())
	}
}

func TestPeerSpillNeverReturns503AfterHardStartFailure(t *testing.T) {
	rig := newPeerSpillRig(t, http.HandlerFunc(okHealth), func(string, []string) (Proc, error) { return deadProc(), nil }, "peer_spill: never")

	rec := peerSpillImage(rig.r)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %q, want 503", rec.Code, rec.Body.String())
	}
	if rig.peer.loads.Load() != 0 || rig.peer.chats.Load() != 0 {
		t.Fatalf("peer asked despite peer_spill: never: loads=%d chats=%d", rig.peer.loads.Load(), rig.peer.chats.Load())
	}
}
