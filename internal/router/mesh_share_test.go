package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"model-router/internal/config"
)

// meshCaller builds router "bravo" whose only peer is node A at url, then
// pulls A's telemetry once.
func meshCaller(t *testing.T, url, models string) *Router {
	t.Helper()
	cfg, err := config.Parse([]byte("start_port: 5900\npeers:\n  - name: alpha\n    kind: router\n    base_url: " + url + "\n" + models))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	r := New(cfg, NewLogger(io.Discard, false), "bravo")
	NewPeerSyncer(r.PeerList, r.peers, time.Minute, nil).syncOnce()
	return r
}

func peerChat(h http.Handler, model, fromPeer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(`{"model":"`+model+`","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	if fromPeer != "" {
		req.Header.Set("X-Model-Router-Peer", fromPeer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Node A has stanzas a and b (share: false). A caller that names A with no
// models list sees and reaches a, never b, and A refuses b from any peer.
func TestMeshAdvertisesSharedStanzasOnly(t *testing.T) {
	a := loadTestRouter(t)
	off := false
	a.cfg().Stanza("b").Share = &off
	a.ledger.SetGPUs([]*GPUState{gpu(0, 20000)}) // room for a and b together, so no eviction
	ha := NewHandler(a, NewLogger(io.Discard, false))
	srv := httptest.NewServer(ha)
	t.Cleanup(srv.Close)

	if _, err := a.Load("b"); err != nil {
		t.Fatalf("Load b: %v", err)
	}
	tel := a.TelemetrySnapshot()
	if !slices.Equal(tel.Catalog, []string{"a"}) || len(tel.LoadedModels) != 0 {
		t.Fatalf("catalog = %v, loaded = %+v, want only a and nothing loaded", tel.Catalog, tel.LoadedModels)
	}

	b := meshCaller(t, srv.URL, "")
	hb := NewHandler(b, NewLogger(io.Discard, false))
	rec := httptest.NewRecorder()
	hb.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("/v1/models: %v", err)
	}
	var ids []string
	for _, m := range list.Data {
		ids = append(ids, m.ID)
	}
	if !slices.Equal(ids, []string{"a"}) {
		t.Fatalf("/v1/models ids = %v, want [a]", ids)
	}
	if rec := peerChat(hb, "a", ""); rec.Code != http.StatusOK {
		t.Fatalf("a via peer: %d %s", rec.Code, rec.Body.String())
	}
	if rec := peerChat(hb, "b", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("b via caller: %d %s, want 404", rec.Code, rec.Body.String())
	}

	// A caller whose config still names b is refused at the load.
	c := meshCaller(t, srv.URL, "    models: [b]\n")
	if rec := peerChat(NewHandler(c, NewLogger(io.Discard, false)), "b", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("b via stale config: %d %s, want 503", rec.Code, rec.Body.String())
	}
	if rec := peerChat(ha, "b", "bravo"); rec.Code != http.StatusNotFound || !bytes.Contains(rec.Body.Bytes(), []byte("model not shared")) {
		t.Fatalf("peer-proxied b: %d %s, want 404 model not shared", rec.Code, rec.Body.String())
	}
	if rec := peerChat(ha, "b", ""); rec.Code != http.StatusOK {
		t.Fatalf("local client b: %d %s", rec.Code, rec.Body.String())
	}
}

// Node A keeps y (share: false) off the mesh on every path a peer can name it
// by: a pool of y alone, a pool that also holds x, y's filter id, y's alias
// and /upstream/y. None of them may reach y's backend; a local client still can.
func TestUnsharedStanzaRefusedOnEveryPeerPath(t *testing.T) {
	backend := func(hits *atomic.Int32) int {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.URL.Path != "/" { // health checks are not requests
				hits.Add(1)
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		t.Cleanup(srv.Close)
		return srv.Listener.Addr().(*net.TCPAddr).Port
	}
	var xHits, yHits atomic.Int32
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
start_port: 5900
stanzas:
  - model_id: x
    command: "fake --port {port}"
    vram_mb: 1000
    health_check: /
    spin_up_seconds: 5
    port: %d
  - model_id: y
    command: "fake --port {port}"
    vram_mb: 1000
    health_check: /
    spin_up_seconds: 5
    port: %d
    share: false
    aliases: [y-alias]
    filters:
      setParamsByID:
        "${MODEL_ID}:high":
          temperature: 0.1
pools:
  only-y:
    targets: [y]
  mixed:
    targets: [y, x]
`, backend(&xHits), backend(&yHits))))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	a := New(cfg, NewLogger(io.Discard, false), "alpha")
	a.spawn = func(string, []string) (Proc, error) { return newFakeProc(), nil }
	a.ledger.SetGPUs([]*GPUState{gpu(0, 10000)})
	t.Cleanup(func() { _ = a.Unload("x"); _ = a.Unload("y") })
	h := NewHandler(a, NewLogger(io.Discard, false))

	for _, model := range []string{"only-y", "y:high", "y-alias"} {
		if rec := peerChat(h, model, "bravo"); rec.Code != http.StatusNotFound || !bytes.Contains(rec.Body.Bytes(), []byte("model not shared")) {
			t.Fatalf("peer asks for %s: %d %s, want 404 model not shared", model, rec.Code, rec.Body.String())
		}
	}
	req := httptest.NewRequest("GET", "/upstream/y/health", nil)
	req.Header.Set("X-Model-Router-Peer", "bravo")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound || !bytes.Contains(rec.Body.Bytes(), []byte("model not shared")) {
		t.Fatalf("peer upstream y: %d %s, want 404 model not shared", rec.Code, rec.Body.String())
	}
	if rec := peerChat(h, "mixed", "bravo"); rec.Code != http.StatusOK || xHits.Load() != 1 {
		t.Fatalf("peer asks for mixed: %d %s, x hits %d, want 200 from x", rec.Code, rec.Body.String(), xHits.Load())
	}
	if n := yHits.Load(); n != 0 {
		t.Fatalf("y backend got %d peer requests, want 0", n)
	}

	// A caller on a release before X-Model-Router-Peer existed looks like a
	// local client, so its load of y goes through. Pinned on purpose: the
	// README "share" section tells operators to upgrade callers first.
	load := httptest.NewRequest("POST", "/_router/load", bytes.NewBufferString(`{"model_id":"y"}`))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, load)
	if rec.Code != http.StatusOK {
		t.Fatalf("headerless load y: %d %s, want 200 (local load)", rec.Code, rec.Body.String())
	}

	if rec := peerChat(h, "only-y", ""); rec.Code != http.StatusOK || yHits.Load() != 1 {
		t.Fatalf("local client only-y: %d %s, y hits %d, want 200 from y", rec.Code, rec.Body.String(), yHits.Load())
	}
	if rec := peerChat(h, "y-alias", ""); rec.Code != http.StatusOK || yHits.Load() != 2 {
		t.Fatalf("local client y-alias: %d %s, y hits %d, want 200 from y", rec.Code, rec.Body.String(), yHits.Load())
	}

	// Peers polling telemetry never see a request to y, by id, alias or
	// filter id; the local Activity data still lists them.
	for _, m := range []string{"y", "y-alias", "y:high", "x"} {
		a.activity.begin(InProgressEntry{Model: m, Prompt: "fictional prompt for " + m})
	}
	tel := a.TelemetrySnapshot()
	var running, recent []string
	for _, e := range tel.InProgress {
		running = append(running, e.Model)
	}
	for _, e := range tel.Recent {
		recent = append(recent, e.Model)
	}
	if !slices.Equal(running, []string{"x"}) || !slices.Contains(recent, "x") || slices.ContainsFunc(recent, func(m string) bool { return m != "x" }) {
		t.Fatalf("telemetry in progress = %v, recent = %v, want x only", running, recent)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/_router/activity", nil))
	var local struct {
		Entries    []ActivityEntry   `json:"entries"`
		InProgress []InProgressEntry `json:"in_progress"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &local); err != nil {
		t.Fatalf("/_router/activity: %v", err)
	}
	if len(local.InProgress) != 4 || !slices.ContainsFunc(local.Entries, func(e ActivityEntry) bool { return e.Model != "x" }) {
		t.Fatalf("local activity in progress = %+v, entries = %+v, want y rows kept", local.InProgress, local.Entries)
	}
}

// Static models still count when telemetry has no catalog (an older peer),
// and the catalog adds to them when it does.
func TestConfiguredPeerModelsPlusCatalog(t *testing.T) {
	r := spilloverRouter(t, "start_port: 5900\npeers:\n  - name: alpha\n    kind: router\n    base_url: http://127.0.0.1:9\n    models: [listed]\n")
	p := r.peerByName("alpha")
	r.peers.Set("alpha", &Telemetry{Node: "alpha", LoadedModels: []LoadedModel{{ID: "running"}}})
	if got := r.peerModels(p); !slices.Equal(got, []string{"listed"}) {
		t.Fatalf("older peer models = %v, want [listed]", got)
	}
	r.peers.Set("alpha", &Telemetry{Node: "alpha", Catalog: []string{"listed", "advertised"}})
	if got := r.peerModels(p); !slices.Equal(got, []string{"listed", "advertised"}) {
		t.Fatalf("models = %v, want [listed advertised]", got)
	}
}
