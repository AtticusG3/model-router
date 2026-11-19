package router

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// walkPeer is a fake router peer. loadStatus/retryAfter shape the answer to
// /_router/load; a chat that gets past the load answers chatStatus.
type walkPeer struct {
	srv        *httptest.Server
	loads      atomic.Int32
	chats      atomic.Int32
	loadStatus int
	retryAfter string
	chatStatus int
	chatBody   atomic.Value // last chat request body, as a string
}

func newWalkPeer(t *testing.T, loadStatus int, retryAfter string) *walkPeer {
	t.Helper()
	p := &walkPeer{loadStatus: loadStatus, retryAfter: retryAfter, chatStatus: http.StatusOK}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		if req.URL.Path == "/_router/load" {
			p.loads.Add(1)
			if p.retryAfter != "" {
				w.Header().Set("Retry-After", p.retryAfter)
			}
			w.WriteHeader(p.loadStatus)
			return
		}
		p.chatBody.Store(string(raw))
		p.chats.Add(1)
		w.WriteHeader(p.chatStatus)
		_, _ = w.Write([]byte("served"))
	}))
	t.Cleanup(p.srv.Close)
	return p
}

// walkRouter builds a router with the given peers (name -> fake) all listing
// model "shared-model", in the order given, each with fresh telemetry.
func walkRouter(t *testing.T, extra string, peers ...*walkPeer) *Router {
	t.Helper()
	yaml := "start_port: 5900\n" + extra + "peers:\n"
	names := []string{"alpha", "bravo", "charlie"}
	for i := range peers {
		yaml += "  - name: " + names[i] + "\n    kind: router\n    base_url: http://127.0.0.1:9\n    models: [shared-model]\n"
	}
	r := spilloverRouter(t, yaml)
	for i, p := range peers {
		r.cfg().Peer(names[i]).BaseURL = p.srv.URL
		r.peers.Set(names[i], &Telemetry{Node: names[i]})
	}
	return r
}

func chat(r *Router, model string) *httptest.ResponseRecorder {
	h := NewHandler(r, NewLogger(io.Discard, false))
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPeerWalkRetryAfterIsRememberedAndSkipped(t *testing.T) {
	a := newWalkPeer(t, http.StatusServiceUnavailable, "60")
	b := newWalkPeer(t, http.StatusOK, "")
	r := walkRouter(t, "", a, b)

	for i := 0; i < 2; i++ {
		if rec := chat(r, "shared-model"); rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, body = %s", i, rec.Code, rec.Body.String())
		}
	}
	if a.loads.Load() != 1 || b.chats.Load() != 2 {
		t.Fatalf("alpha loads = %d (want 1), bravo chats = %d (want 2)", a.loads.Load(), b.chats.Load())
	}

	// Once the window has passed alpha is asked again.
	r.mu.Lock()
	r.peerUntil["alpha/shared-model"] = time.Now().Add(-time.Second)
	r.mu.Unlock()
	chat(r, "shared-model")
	if a.loads.Load() != 2 {
		t.Fatalf("alpha loads = %d after the window passed, want 2", a.loads.Load())
	}
}

func TestPeerWalkForgetsA503WithoutRetryAfter(t *testing.T) {
	a := newWalkPeer(t, http.StatusServiceUnavailable, "")
	b := newWalkPeer(t, http.StatusOK, "")
	r := walkRouter(t, "", a, b)

	for i := 0; i < 2; i++ {
		if rec := chat(r, "shared-model"); rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d", i, rec.Code)
		}
	}
	if a.loads.Load() != 2 {
		t.Fatalf("alpha loads = %d, want 2 (a bare 503 is not remembered)", a.loads.Load())
	}
}

func TestPeerWalkDirectTargetTriesOtherPeers(t *testing.T) {
	a := newWalkPeer(t, http.StatusServiceUnavailable, "30")
	b := newWalkPeer(t, http.StatusOK, "")
	r := walkRouter(t, "", a, b)

	if rec := chat(r, "alpha/shared-model"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if a.loads.Load() != 1 || b.chats.Load() != 1 {
		t.Fatalf("alpha loads = %d, bravo chats = %d, want 1 and 1", a.loads.Load(), b.chats.Load())
	}
}

func TestPeerWalkAllRefuseSendsLastFailureAndSmallestWindow(t *testing.T) {
	a := newWalkPeer(t, http.StatusServiceUnavailable, "30")
	b := newWalkPeer(t, http.StatusServiceUnavailable, "10")
	c := newWalkPeer(t, http.StatusServiceUnavailable, "")
	r := walkRouter(t, "", a, b, c)

	rec := chat(r, "shared-model")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "peer charlie rejected load") {
		t.Fatalf("body = %q, want the last failure (charlie)", rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "10" {
		t.Fatalf("Retry-After = %q, want 10", got)
	}
}

func TestPeerWalkAllRefuseWithoutWindowHasNoRetryAfter(t *testing.T) {
	a := newWalkPeer(t, http.StatusServiceUnavailable, "")
	b := newWalkPeer(t, http.StatusServiceUnavailable, "")
	r := walkRouter(t, "", a, b)

	rec := chat(r, "shared-model")
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "" {
		t.Fatalf("status = %d, Retry-After = %q, want 503 and none", rec.Code, rec.Header().Get("Retry-After"))
	}
}

func TestPeerWalkStopsOnceBytesStarted(t *testing.T) {
	a := newWalkPeer(t, http.StatusOK, "")
	a.chatStatus = http.StatusInternalServerError
	b := newWalkPeer(t, http.StatusOK, "")
	r := walkRouter(t, "", a, b)

	rec := chat(r, "shared-model")
	if rec.Code != http.StatusInternalServerError || rec.Body.String() != "served" {
		t.Fatalf("status = %d, body = %q, want alpha's error passed through", rec.Code, rec.Body.String())
	}
	if b.loads.Load() != 0 {
		t.Fatalf("bravo loads = %d, want 0 after alpha wrote a response", b.loads.Load())
	}
}

func TestPeerWalkMeshHopAfterLocalCouldNotStart(t *testing.T) {
	a := newWalkPeer(t, http.StatusServiceUnavailable, "30")
	b := newWalkPeer(t, http.StatusOK, "")
	r := walkRouter(t, "", a, b)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(`{"model":"shared-model"}`))
	if !r.serveSpill(rec, req, "shared-model") {
		t.Fatal("spill should reach bravo")
	}
	if a.loads.Load() != 1 || b.chats.Load() != 1 {
		t.Fatalf("alpha loads = %d, bravo chats = %d, want 1 and 1", a.loads.Load(), b.chats.Load())
	}
}

func TestPeerWalkSlotSpillWalksThenQueuesLocally(t *testing.T) {
	a := newWalkPeer(t, http.StatusServiceUnavailable, "30")
	b := newWalkPeer(t, http.StatusServiceUnavailable, "")
	r := walkRouter(t, "", a, b)
	newReq := func() *http.Request {
		return httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(`{"model":"shared-model"}`))
	}

	if r.serveSlotSpill(httptest.NewRecorder(), newReq(), "shared-model") {
		t.Fatal("slot spill should report false when every peer refuses, so the caller queues locally")
	}
	if a.loads.Load() != 1 || b.loads.Load() != 1 {
		t.Fatalf("loads = %d and %d, want both peers asked once", a.loads.Load(), b.loads.Load())
	}

	b.loadStatus = http.StatusOK
	rec := httptest.NewRecorder()
	if !r.serveSlotSpill(rec, newReq(), "shared-model") || b.chats.Load() != 1 {
		t.Fatal("slot spill should reach bravo once it accepts")
	}
	if a.loads.Load() != 1 {
		t.Fatalf("alpha loads = %d, want 1 (still inside its window)", a.loads.Load())
	}
}

func TestPeerWalkOpenAIPeerIsASingleHop(t *testing.T) {
	var hits atomic.Int32
	openai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer openai.Close()
	b := newWalkPeer(t, http.StatusOK, "")
	r := spilloverRouter(t, `
start_port: 5900
peers:
  - name: hosted
    kind: openai
    base_url: http://127.0.0.1:9
    models: [shared-model]
  - name: bravo
    kind: router
    base_url: http://127.0.0.1:9
    models: [shared-model]
`)
	r.cfg().Peer("hosted").BaseURL = openai.URL
	r.cfg().Peer("bravo").BaseURL = b.srv.URL
	r.peers.Set("bravo", &Telemetry{Node: "bravo"})

	if rec := chat(r, "shared-model"); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want the openai peer's failure", rec.Code)
	}
	if hits.Load() != 1 || b.loads.Load() != 0 {
		t.Fatalf("openai hits = %d, bravo loads = %d, want 1 and 0", hits.Load(), b.loads.Load())
	}
}

func TestPeerWalkPoolKeepsConfiguredOrderAndIgnoresWindows(t *testing.T) {
	a := newWalkPeer(t, http.StatusServiceUnavailable, "60")
	b := newWalkPeer(t, http.StatusOK, "")
	r := walkRouter(t, `
pools:
  shared-pool:
    targets: [alpha/shared-model, bravo/shared-model]
    spillover: 1
`, a, b)

	// A direct request leaves alpha with a remembered window.
	chat(r, "alpha/shared-model")
	if a.loads.Load() != 1 {
		t.Fatalf("alpha loads = %d, want 1", a.loads.Load())
	}
	for i := 0; i < 2; i++ {
		if rec := chat(r, "shared-pool"); rec.Code != http.StatusOK {
			t.Fatalf("pool request %d: status = %d, body = %s", i, rec.Code, rec.Body.String())
		}
	}
	if a.loads.Load() != 3 {
		t.Fatalf("alpha loads = %d, want 3 (a pool tries it first every time)", a.loads.Load())
	}
}
