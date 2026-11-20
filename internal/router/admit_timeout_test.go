package router

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// pinnedHoldersYAML is a local-only model "shared-model" that may use card 0
// or 1, and two pinned models that each hold most of one 32 GB card.
const pinnedHoldersYAML = `
stanzas:
  - model_id: hold1
    command: "x --port {port}"
    vram_mb: 28000
    device: "0"
    pin: true
  - model_id: hold2
    command: "x --port {port}"
    vram_mb: 28000
    device: "1"
    pin: true
  - model_id: shared-model
    command: "x --port {port} --gpu {gpu}"
    vram_mb: 12000
    device: "0|1"
`

// holdBothCards reserves both pinned models on the router's two 32 GB cards.
func holdBothCards(t *testing.T, r *Router) {
	t.Helper()
	r.ledger.SetGPUs([]*GPUState{gpu(0, 32000), gpu(1, 32000)})
	for _, id := range []string{"hold1", "hold2"} {
		if _, ok := r.ledger.Reserve(id, r.cfg().Stanza(id)); !ok {
			t.Fatalf("reserve %s", id)
		}
	}
}

func TestCanEventuallyAdmitCountsPinnedOccupants(t *testing.T) {
	r := spilloverRouter(t, "start_port: 5900\n"+pinnedHoldersYAML)
	holdBothCards(t, r)
	s := r.cfg().Stanza("shared-model")
	if r.canEventuallyAdmit(s) {
		t.Fatal("pinned models hold both cards: the model can never be admitted")
	}
	if _, err := r.Load("shared-model"); err == nil {
		t.Fatal("Load must fail")
	} else if ae := new(admitError); !errors.As(err, &ae) {
		t.Fatalf("Load error = %T %v, want admitError", err, err)
	}

	// Control: with one holder unpinned, its card can be freed by eviction.
	r.cfg().Stanza("hold2").Pin = false
	if !r.canEventuallyAdmit(s) {
		t.Fatal("an unpinned holder can be evicted, so the model can be admitted")
	}
}

func TestPinnedHoldersFailLocalOnlyModelFast(t *testing.T) {
	r := spilloverRouter(t, "start_port: 5900\nadmit_timeout_seconds: 1\n"+pinnedHoldersYAML)
	holdBothCards(t, r)
	defer func(d time.Duration) { spillRetryInterval = d }(spillRetryInterval)
	spillRetryInterval = 20 * time.Millisecond

	start := time.Now()
	rec := chat(r, "shared-model")
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503; body %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("503 must carry Retry-After")
	}
	if !strings.Contains(rec.Body.String(), "shared-model") {
		t.Fatalf("body %q should name the model", rec.Body.String())
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %v, want about the 1s admit timeout", d)
	}
}

func TestSpillRetriesWhilePeerIsNotRankedYet(t *testing.T) {
	p := newWalkPeer(t, 200, "")
	r := walkRouter(t, pinnedHoldersYAML, p)
	holdBothCards(t, r)
	var logs bytes.Buffer
	r.logger = NewLogger(&logs, false)
	// No telemetry yet: the first walk finds no peer.
	r.peers = NewPeerCache(time.Minute)
	defer func(d time.Duration) { spillRetryInterval = d }(spillRetryInterval)
	spillRetryInterval = 20 * time.Millisecond
	go func() {
		time.Sleep(150 * time.Millisecond)
		r.peers.Set("alpha", &Telemetry{Node: "alpha"})
	}()

	rec := chat(r, "shared-model")
	if rec.Code != 200 || rec.Body.String() != "served" {
		t.Fatalf("got %d %q, want the peer to serve it", rec.Code, rec.Body.String())
	}
	if p.chats.Load() != 1 {
		t.Fatalf("peer chats = %d, want 1", p.chats.Load())
	}
	if !strings.Contains(logs.String(), "no peer chosen (alpha: telemetry stale)") {
		t.Fatalf("journal should say why no peer was chosen:\n%s", logs.String())
	}
}

// A request that spent its admit budget on peers must not get a fresh one for
// the local wait.
func TestLoadWaitUsesRemainingAdmitBudget(t *testing.T) {
	r := loadTestRouter(t)
	r.cfg().Stanza("a").Device = "0"
	r.cfg().Stanza("b").Device = "0"
	r.cfg().Stanza("a").VramMB = 8000
	r.cfg().Stanza("b").VramMB = 8000
	if _, err := r.Load("a"); err != nil {
		t.Fatalf("Load a: %v", err)
	}
	r.holdOccupancy("a")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, admitDeadlineKey{}, time.Now().Add(-time.Second))

	start := time.Now()
	_, err := r.load(ctx, "b", true, true)
	if ae := new(admitError); !errors.As(err, &ae) {
		t.Fatalf("error = %T %v, want admitError", err, err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("took %v, want an immediate give-up with the budget spent", d)
	}
}

// The deadline spillWhileBlocked records must be visible on the very request
// serveLocalTarget goes on to admit, so the local wait cannot start a new budget.
func TestSpillWhileBlockedLeavesDeadlineOnRequest(t *testing.T) {
	r := spilloverRouter(t, "start_port: 5900\n"+pinnedHoldersYAML)
	r.ledger.SetGPUs([]*GPUState{gpu(0, 32000), gpu(1, 32000)})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	before := time.Now()

	if r.spillWhileBlocked(httptest.NewRecorder(), req, r.cfg().Stanza("shared-model")) {
		t.Fatal("room is free, so the request should carry on locally")
	}
	d, ok := req.Context().Value(admitDeadlineKey{}).(time.Time)
	if !ok || d.Before(before) {
		t.Fatalf("request context deadline = %v (set %v), want one in the future", d, ok)
	}
}

// Local room that opens while a request is blocked locally must be taken at
// once, not on the next peer poll: the wait wakes on the occupancy poke.
func TestSpillWhileBlockedWakesOnLocalRoom(t *testing.T) {
	r := spilloverRouter(t, "start_port: 5900\n"+pinnedHoldersYAML)
	holdBothCards(t, r)
	// Both holders are mid-request, so neither can be evicted yet. One is
	// released below: then its card can be freed and shared-model fits.
	r.cfg().Stanza("hold1").Pin = false
	r.cfg().Stanza("hold2").Pin = false
	r.holdOccupancy("hold1")
	r.holdOccupancy("hold2")
	defer func(d time.Duration) { spillRetryInterval = d }(spillRetryInterval)
	spillRetryInterval = 5 * time.Second

	s := r.cfg().Stanza("shared-model")
	if r.localCanServe(s) {
		t.Fatal("both cards are busy with mid-request holders, so the model cannot start yet")
	}
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	go func() {
		time.Sleep(50 * time.Millisecond)
		r.releaseOccupancy(Target{Local: "hold1"})
	}()

	start := time.Now()
	if r.spillWhileBlocked(httptest.NewRecorder(), req, s) {
		t.Fatal("room opens, so the request must carry on locally")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("waited %v for room that opened at 50ms, want a wake-up well under the %v poll", d, spillRetryInterval)
	}
}

// A request that spends its admit budget in the peer walk must not get a
// second budget for the local admit that follows.
func TestSpillAndLocalAdmitShareOneBudget(t *testing.T) {
	// A peer that advertises shared-model but hangs on load. The request's
	// admit budget must end the hang, not the peer's own timer.
	p := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/_router/load" {
			select {
			case <-req.Context().Done():
			case <-time.After(2 * time.Second):
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer p.Close()

	r := spilloverRouter(t, "start_port: 5900\nadmit_timeout_seconds: 1\n"+pinnedHoldersYAML+
		"peers:\n  - name: alpha\n    kind: router\n    base_url: http://127.0.0.1:9\n    models: [shared-model]\n")
	holdBothCards(t, r)
	r.peers = NewPeerCache(time.Minute)
	r.cfg().Peer("alpha").BaseURL = p.URL
	r.peers.Set("alpha", &Telemetry{Node: "alpha"})
	// Free one card while the peer load hangs, so the local admit path is
	// reached with the budget spent.
	go func() {
		time.Sleep(900 * time.Millisecond)
		r.ledger.Release("hold2")
	}()

	start := time.Now()
	rec := chat(r, "shared-model")
	elapsed := time.Since(start)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("503 must carry Retry-After")
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("took %v, want the 1s admit budget shared by the peer walk and the local wait", elapsed)
	}
}

// A peer that refuses the first walk must not cost the next peer the request
// body: each retry sends the whole payload.
func TestSpillRetryKeepsRequestBodyForLaterPeer(t *testing.T) {
	a := newWalkPeer(t, 503, "")
	b := newWalkPeer(t, 200, "")
	r := walkRouter(t, pinnedHoldersYAML, a, b)
	holdBothCards(t, r)
	// Only alpha is ranked on the first walk; bravo joins on a later one.
	r.peers = NewPeerCache(time.Minute)
	r.peers.Set("alpha", &Telemetry{Node: "alpha"})
	defer func(d time.Duration) { spillRetryInterval = d }(spillRetryInterval)
	spillRetryInterval = 20 * time.Millisecond
	go func() {
		time.Sleep(100 * time.Millisecond)
		r.peers.Set("bravo", &Telemetry{Node: "bravo"})
	}()

	rec := chat(r, "shared-model")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want bravo to serve it", rec.Code)
	}
	if a.loads.Load() == 0 || a.chats.Load() != 0 {
		t.Fatalf("alpha loads = %d chats = %d, want a refused load and no chat", a.loads.Load(), a.chats.Load())
	}
	got, _ := b.chatBody.Load().(string)
	if !strings.Contains(got, `"content":"hi"`) || !strings.Contains(got, `"model":"shared-model"`) {
		t.Fatalf("bravo got body %q, want the full chat payload", got)
	}
}
