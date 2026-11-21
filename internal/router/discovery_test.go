package router

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"model-router/internal/config"
)

func discoveryConfig(t *testing.T, extra string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte("discovery:\n  subnets: [\"127.0.0.1/32\"]\n  ports: [1]\n" + extra))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return cfg
}

func healthRouter(t *testing.T, node string) *Router {
	t.Helper()
	r := New(discoveryConfig(t, ""), NewLogger(io.Discard, false), node)
	t.Cleanup(r.Shutdown)
	return r
}

func serverPort(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("url: %v", err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	return p
}

func TestDiscoveryFindsTwoRoutersInOneScan(t *testing.T) {
	logger := NewLogger(io.Discard, false)
	a, b := healthRouter(t, "alpha"), healthRouter(t, "beta")
	as := httptest.NewServer(NewHandler(a, logger))
	defer as.Close()
	bs := httptest.NewServer(NewHandler(b, logger))
	defer bs.Close()

	a.cfg().Discovery.Ports = []int{serverPort(t, as), serverPort(t, bs)}
	b.cfg().Discovery.Ports = []int{serverPort(t, as), serverPort(t, bs)}

	d := NewDiscoverer(a.Config, a, logger)
	added, _ := d.scan(context.Background())
	if added != 1 {
		t.Fatalf("added = %d, want 1", added)
	}
	p := a.peerByName("beta")
	if p == nil {
		t.Fatal("alpha did not discover beta")
	}
	if p.BaseURL != fmt.Sprintf("http://127.0.0.1:%d", serverPort(t, bs)) {
		t.Fatalf("beta base_url = %s", p.BaseURL)
	}
	if a.peerByName("alpha") != nil {
		t.Fatal("router discovered itself")
	}
	if a.Peers().Snapshot()["beta"] == nil {
		t.Fatal("discovered telemetry was not cached")
	}
}

func TestDiscoveryCollapsesOneNodeAndConfiguredWins(t *testing.T) {
	logger := NewLogger(io.Discard, false)
	node := healthRouter(t, "scan-node")
	h := NewHandler(node, logger)

	low := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Via", "1.1 test-proxy")
		h.ServeHTTP(w, req)
	}))
	defer low.Close()
	high := httptest.NewServer(h)
	defer high.Close()

	lowPort, highPort := serverPort(t, low), serverPort(t, high)
	cfg := discoveryConfig(t, "peers:\n  - name: scan-node\n    kind: router\n    base_url: http://127.0.0.1:9\n")
	cfg.Discovery.Ports = []int{lowPort, highPort}
	r := New(cfg, logger, "observer")
	d := NewDiscoverer(r.Config, r, logger)

	// The configured entry wins over both addresses reporting one node.
	if added, _ := d.scan(context.Background()); added != 0 {
		t.Fatalf("added = %d, want 0 for a configured node", added)
	}
	peers := r.PeerList()
	if len(peers) != 1 || peers[0].Name != "scan-node" {
		t.Fatalf("peers = %+v", peers)
	}
	if peers[0].BaseURL != "http://127.0.0.1:9" {
		t.Fatalf("configured peer was overwritten: %s", peers[0].BaseURL)
	}

	// Without a configured entry the direct address beats the proxied one,
	// even when the direct port is higher.
	plain := discoveryConfig(t, "")
	plain.Discovery.Ports = []int{lowPort, highPort}
	observer := New(plain, logger, "observer")
	d = NewDiscoverer(observer.Config, observer, logger)
	if added, _ := d.scan(context.Background()); added != 1 {
		t.Fatalf("added = %d, want 1", added)
	}
	got := observer.peerByName("scan-node")
	if got == nil || got.BaseURL != fmt.Sprintf("http://127.0.0.1:%d", highPort) {
		t.Fatalf("collapsed peer = %+v, want non-proxied port %d", got, highPort)
	}
}

func TestDiscoveryPrefersDirectPortWhenProxyAddsNoHeader(t *testing.T) {
	logger := NewLogger(io.Discard, false)
	r := New(discoveryConfig(t, ""), logger, "observer")
	d := NewDiscoverer(r.Config, r, logger)
	cfg := d.cfg()
	// The direct router port is listed first; neither address carries a
	// proxy header, so the port order decides. Lowest port would pick the
	// proxy on 8081.
	cfg.Discovery.Ports = []int{18080, 8081}
	byNode := map[string][]probeResult{"scan-node": {
		{node: "scan-node", addr: netip.MustParseAddrPort("127.0.0.1:8081")},
		{node: "scan-node", addr: netip.MustParseAddrPort("127.0.0.1:18080")},
	}}

	found, _ := d.collapse(cfg, byNode)
	got := found["scan-node"].peer
	if got == nil || got.BaseURL != "http://127.0.0.1:18080" {
		t.Fatalf("collapsed peer = %+v, want direct port 18080", got)
	}
}

func TestDiscoveryScanReturnsWithManyProbeResults(t *testing.T) {
	logger := NewLogger(io.Discard, false)
	cfg := discoveryConfig(t, "")
	// More targets than the concurrency limit in several waves: before the
	// result buffer covered the whole target list this deadlocked the scan.
	ports := make([]int, 100)
	for i := range ports {
		ports[i] = i + 1
	}
	cfg.Discovery.Ports = ports
	r := New(cfg, logger, "alpha")
	d := NewDiscoverer(r.Config, r, logger)
	d.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		body := `{"node":"bulk","gpus":[],"loaded_models":[],"timestamp":1}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}

	done := make(chan int, 1)
	go func() {
		added, _ := d.scan(context.Background())
		done <- added
	}()
	select {
	case added := <-done:
		if added != 1 {
			t.Fatalf("added = %d, want 1", added)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("scan did not return")
	}
}

func TestDiscoveryBackoffDoublesAndResets(t *testing.T) {
	logger := NewLogger(io.Discard, false)
	cfg := discoveryConfig(t, "")
	r := New(cfg, logger, "alpha")

	var up atomic.Bool
	d := NewDiscoverer(r.Config, r, logger)
	fc := newFakeClock()
	d.clock = fc
	d.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if !up.Load() {
			return nil, errors.New("down")
		}
		body := `{"node":"delta","gpus":[],"loaded_models":[],"timestamp":1}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	want := []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second, 240 * time.Second, 480 * time.Second, 600 * time.Second}
	for i, w := range want {
		waitForWaits(t, fc, i+1)
		if got := fc.lastWait(); got != w {
			t.Fatalf("wait = %v, want %v", got, w)
		}
		if w != want[len(want)-1] {
			fc.tick()
		}
	}

	// A new peer resets the backoff to the base interval.
	up.Store(true)
	fc.tick()
	waitForWaits(t, fc, len(want)+1)
	if got := fc.lastWait(); got != 30*time.Second {
		t.Fatalf("reset wait = %v, want 30s", got)
	}
}

func TestDiscoveryDropsStalePeerButKeepsConfigured(t *testing.T) {
	logger := NewLogger(io.Discard, false)
	beta := healthRouter(t, "beta")
	bs := httptest.NewServer(NewHandler(beta, logger))
	port := serverPort(t, bs)

	cfg := discoveryConfig(t, "peers:\n  - name: keeper\n    kind: router\n    base_url: http://127.0.0.1:9\n")
	cfg.Discovery.Ports = []int{port}
	a := New(cfg, logger, "alpha")
	d := NewDiscoverer(a.Config, a, logger)

	if added, _ := d.scan(context.Background()); added != 1 {
		t.Fatalf("added = %d, want 1", added)
	}
	bs.Close()

	d.scan(context.Background())
	d.scan(context.Background())
	if a.peerByName("beta") == nil {
		t.Fatal("peer dropped before 3 misses")
	}
	if _, dropped := d.scan(context.Background()); dropped != 1 {
		t.Fatalf("dropped = %d, want 1", dropped)
	}
	if a.peerByName("beta") != nil {
		t.Fatal("stale discovered peer was kept")
	}
	if a.peerByName("keeper") == nil {
		t.Fatal("configured peer was dropped")
	}
}

func TestDiscoveryDisabledMakesNoConnections(t *testing.T) {
	cfg, err := config.Parse([]byte("discovery:\n  enabled: false\n"))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	r := New(cfg, NewLogger(io.Discard, false), "alpha")
	var dials atomic.Int32
	d := NewDiscoverer(r.Config, r, NewLogger(io.Discard, false))
	d.dial = func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, errors.New("unexpected dial")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.scan(ctx)
	d.Run(ctx)
	if n := dials.Load(); n != 0 {
		t.Fatalf("dialer called %d times with discovery disabled", n)
	}
}

// A reload that turns discovery off drops the peers it had found, and makes
// no further connections; turning it back on resumes scanning.
func TestDiscoveryReloadStopsAndResumes(t *testing.T) {
	logger := NewLogger(io.Discard, false)
	r := New(reloadDiscoveryConfig(t, false, 1), logger, "alpha")
	d := NewDiscoverer(r.Config, r, logger)
	var calls atomic.Int32
	d.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		body := `{"node":"beta","gpus":[],"loaded_models":[],"catalog":["m"],"timestamp":1}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	fc := newFakeClock()
	d.clock = fc

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)
	waitForWaits(t, fc, 1)
	if n := calls.Load(); n != 0 {
		t.Fatalf("scanned %d times with discovery off", n)
	}

	// On: the next pass scans and finds beta.
	if err := r.Apply(reloadDiscoveryConfig(t, true, 1)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	fc.tick()
	waitForWaits(t, fc, 2)
	if r.peerByName("beta") == nil {
		t.Fatal("scanning did not resume after enabling discovery")
	}

	// Off again: the next pass drops beta before probing.
	if err := r.Apply(reloadDiscoveryConfig(t, false, 1)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	calls.Store(0)
	fc.tick()
	waitForWaits(t, fc, 3)
	if r.peerByName("beta") != nil {
		t.Fatal("discovered peer survived discovery off")
	}
	if _, ok := r.peers.Snapshot()["beta"]; ok {
		t.Fatal("discovered telemetry survived discovery off")
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("scanned %d times after discovery off", n)
	}
}

// A discovered peer's model list follows the syncer's fresh telemetry, not
// the copy frozen at scan time.
func TestDiscoveredPeerFollowsFreshCatalog(t *testing.T) {
	logger := NewLogger(io.Discard, false)
	var catalog atomic.Value
	catalog.Store([]string{"old"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ids := catalog.Load().([]string)
		io.WriteString(w, `{"node":"beta","gpus":[],"loaded_models":[],"catalog":["`+strings.Join(ids, `","`)+`"],"timestamp":1}`)
	}))
	defer srv.Close()

	cfg := discoveryConfig(t, "")
	cfg.Discovery.Ports = []int{serverPort(t, srv)}
	a := New(cfg, logger, "alpha")
	d := NewDiscoverer(a.Config, a, logger)
	if added, _ := d.scan(context.Background()); added != 1 {
		t.Fatalf("added = %d, want 1", added)
	}
	p := a.peerByName("beta")
	if p == nil || !a.peerHasModel(p, "old") || a.peerHasModel(p, "new") {
		t.Fatalf("scan-time models = %v", p)
	}
	has := func(ids []ModelStatus, id string) bool {
		for _, s := range ids {
			if s.ID == id {
				return true
			}
		}
		return false
	}
	if statuses := a.AllModelStatuses(); !has(statuses, "old") || has(statuses, "new") {
		t.Fatalf("scan-time catalog = %+v", statuses)
	}

	catalog.Store([]string{"new"})
	NewPeerSyncer(a.PeerList, a.peers, time.Minute, nil).syncOnce()
	if a.peerHasModel(p, "old") {
		t.Fatal("removed model is still routed to the peer")
	}
	if !a.peerHasModel(p, "new") {
		t.Fatal("added model is not routed to the peer")
	}
	if statuses := a.AllModelStatuses(); has(statuses, "old") || !has(statuses, "new") {
		t.Fatalf("fresh catalog = %+v, want only new", statuses)
	}
	if ranked, _ := a.rankPeers("new", false); len(ranked) != 1 || ranked[0].Peer != "beta" {
		t.Fatalf("ranked = %+v, want beta", ranked)
	}
}

func TestDiscoveryConfiguredPeerWithHostnameBaseURLWins(t *testing.T) {
	logger := NewLogger(io.Discard, false)
	cfg := discoveryConfig(t, "peers:\n  - name: gpu-box\n    kind: router\n    base_url: http://gpu-box.lan:18080\n")
	r := New(cfg, logger, "observer")
	// The syncer caches the configured peer's telemetry under its config name;
	// its node is the name the scan sees.
	r.peers.Set("gpu-box", &Telemetry{Node: "gpu-box-01"})
	d := NewDiscoverer(r.Config, r, logger)
	byNode := map[string][]probeResult{"gpu-box-01": {
		{node: "gpu-box-01", addr: netip.MustParseAddrPort("127.0.0.1:18080")},
	}}

	found, configured := d.collapse(cfg, byNode)
	if len(found) != 0 {
		t.Fatalf("found = %+v, want none for the configured machine", found)
	}
	if !configured["gpu-box"] || !configured["gpu-box-01"] {
		t.Fatalf("configured = %v, want both the node and the peer name", configured)
	}

	r.applyDiscovery(found, configured)
	if p := r.peerByName("gpu-box-01"); p != nil {
		t.Fatalf("configured machine discovered again as %+v", p)
	}
	if peers := r.PeerList(); len(peers) != 1 || peers[0].Name != "gpu-box" {
		t.Fatalf("peers = %+v, want only the configured gpu-box", peers)
	}
}

func TestDiscoveryConfigTakeoverFreesOldCache(t *testing.T) {
	logger := NewLogger(io.Discard, false)
	r := New(discoveryConfig(t, ""), logger, "observer")
	found := map[string]discoveredResult{"x": {
		peer:      &config.Peer{Name: "x", Kind: "router", BaseURL: "http://127.0.0.1:1"},
		telemetry: &Telemetry{Node: "x"},
	}}
	r.applyDiscovery(found, nil)
	if _, ok := r.peers.Snapshot()["x"]; !ok {
		t.Fatal("discovered telemetry was not cached")
	}

	// A configured peer y now owns node x, so the discovered name is gone.
	r.applyDiscovery(nil, map[string]bool{"x": true, "y": true})
	if _, ok := r.peers.Snapshot()["x"]; ok {
		t.Fatal("cache entry for the taken-over discovered name survived")
	}
}

func reloadDiscoveryConfig(t *testing.T, enabled bool, port int) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(fmt.Sprintf("discovery:\n  enabled: %t\n  subnets: [\"127.0.0.1/32\"]\n  ports: [%d]\n", enabled, port)))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return cfg
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// fakeClock records each requested wait and lets the test release it.
type fakeClock struct {
	mu    sync.Mutex
	waits []time.Duration
	tickc chan time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{tickc: make(chan time.Time, 1)} }

func (c *fakeClock) after(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.waits = append(c.waits, d)
	c.mu.Unlock()
	return c.tickc
}

func (c *fakeClock) lastWait() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.waits) == 0 {
		return 0
	}
	return c.waits[len(c.waits)-1]
}

func (c *fakeClock) tick() { c.tickc <- time.Time{} }

func waitForWaits(t *testing.T, c *fakeClock, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		c.mu.Lock()
		got := len(c.waits)
		c.mu.Unlock()
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("clock recorded %d waits, want %d", got, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// The catalog is the peer's model list; loaded models are only the fallback
// for a node that sends none.
func TestModelsFromPrefersCatalog(t *testing.T) {
	got := modelsFrom(&Telemetry{
		Catalog:      []string{"alpha", "beta"},
		Models:       map[string]*ImagePipelineInfo{"gamma": {}},
		LoadedModels: []LoadedModel{{ID: "delta"}},
	})
	if strings.Join(got, ",") != "alpha,beta" {
		t.Fatalf("catalog models = %v", got)
	}
	got = modelsFrom(&Telemetry{LoadedModels: []LoadedModel{{ID: "delta"}, {ID: "delta"}, {ID: "epsilon"}}})
	if strings.Join(got, ",") != "delta,epsilon" {
		t.Fatalf("loaded fallback = %v", got)
	}
}
