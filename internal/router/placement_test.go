package router

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"model-router/internal/config"
)

// busterGPUs is buster's card layout: a 3090 for residents and two V100s.
func busterGPUs() []*GPUState {
	return []*GPUState{gpu(0, 24576), gpu(1, 32768), gpu(2, 32768)}
}

// spawnLog records the CUDA_VISIBLE_DEVICES each backend was spawned with,
// which is how a backend learns the card admission chose for it.
type spawnLog struct {
	mu   sync.Mutex
	cvds []string
}

func (l *spawnLog) spawn(_ string, env []string) (Proc, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cvd := ""
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "CUDA_VISIBLE_DEVICES="); ok {
			cvd = v
		}
	}
	l.cvds = append(l.cvds, cvd)
	return newFakeProc(), nil
}

func (l *spawnLog) visible() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.cvds...)
}

// placementRouter parses stanzas (PORT is a shared health server) on buster's
// GPUs with fake backends.
func placementRouter(t *testing.T, stanzas string) (*Router, *spawnLog) {
	t.Helper()
	port, _ := healthServer(t)
	cfg, err := config.Parse([]byte("start_port: 5900\nstanzas:\n" +
		strings.ReplaceAll(stanzas, "PORT", fmt.Sprint(port))))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	for i := range cfg.Stanzas {
		for n := range cfg.Stanzas[i].InstancePorts {
			cfg.Stanzas[i].InstancePorts[n] = port
		}
	}
	r := New(cfg, NewLogger(io.Discard, false), "test")
	log := &spawnLog{}
	r.spawn = log.spawn
	r.ledger.SetGPUs(busterGPUs())
	t.Cleanup(func() {
		for _, id := range cfg.StanzaIDs() {
			_ = r.Unload(id)
		}
	})
	return r, log
}

func stanzaYAML(id string, vram int, device string, extra ...string) string {
	return fmt.Sprintf(`  - model_id: %s
    command: "fake --port {port}"
    env: ["CUDA_VISIBLE_DEVICES={gpu}"]
    vram_mb: %d
    device: %q
    idle_ttl_seconds: 300
    health_check: /
    spin_up_seconds: 5
    port: PORT
%s`, id, vram, device, strings.Join(extra, ""))
}

func mustLoad(t *testing.T, r *Router, id string) int {
	t.Helper()
	g, err := r.Load(id)
	if err != nil {
		t.Fatalf("Load %s: %v", id, err)
	}
	return g
}

func TestTwoSingleCardModelsFillBothV100sInOrder(t *testing.T) {
	r, log := placementRouter(t,
		stanzaYAML("intent", 1500, "0")+
			stanzaYAML("agents-a1", 27000, "1|2")+
			stanzaYAML("qwopus", 30000, "1|2"))
	mustLoad(t, r, "intent")

	if g := mustLoad(t, r, "agents-a1"); g != 1 {
		t.Fatalf("agents-a1 on GPU %d, want the first V100 (1)", g)
	}
	if g := mustLoad(t, r, "qwopus"); g != 2 {
		t.Fatalf("qwopus on GPU %d, want the second V100 (2)", g)
	}
	for _, id := range []string{"intent", "agents-a1", "qwopus"} {
		if m := r.managed[id]; m == nil || m.State() != StateRunning {
			t.Fatalf("%s should still be running: nothing needed evicting", id)
		}
	}
	if got, want := strings.Join(log.visible(), ","), "0,1,2"; got != want {
		t.Fatalf("backends spawned with CUDA_VISIBLE_DEVICES %s, want %s", got, want)
	}
}

func TestSmallModelsPackSoAWholeCardStaysFree(t *testing.T) {
	r, _ := placementRouter(t,
		stanzaYAML("krea", 16000, "1|2")+
			stanzaYAML("z-anime", 12000, "1|2")+
			stanzaYAML("qwopus", 30000, "1|2"))

	if g := mustLoad(t, r, "krea"); g != 1 {
		t.Fatalf("krea on GPU %d, want 1", g)
	}
	if g := mustLoad(t, r, "z-anime"); g != 1 {
		t.Fatalf("z-anime on GPU %d, want 1 beside krea (tightest fit)", g)
	}
	if g := mustLoad(t, r, "qwopus"); g != 2 {
		t.Fatalf("qwopus on GPU %d, want the card left empty (2)", g)
	}
	if len(r.managed) != 3 {
		t.Fatalf("%d models running, want all 3 (no eviction)", len(r.managed))
	}
}

func TestEvictsOnTheCardWithAStaleModelNotTheIdleOne(t *testing.T) {
	r, _ := placementRouter(t,
		stanzaYAML("idle-x", 27000, "1")+
			stanzaYAML("stale-y", 27000, "2")+
			stanzaYAML("qwopus", 30000, "1|2"))
	r.cfg.Stanza("stale-y").IdleTTLSeconds = 1
	mustLoad(t, r, "idle-x")
	mustLoad(t, r, "stale-y")
	r.managed["stale-y"].setLastUsed(time.Now().Add(-time.Minute))

	if g := mustLoad(t, r, "qwopus"); g != 2 {
		t.Fatalf("qwopus on GPU %d, want 2 where the stale model was", g)
	}
	if _, ok := r.managed["stale-y"]; ok {
		t.Fatal("stale-y should have been evicted")
	}
	if m := r.managed["idle-x"]; m == nil || m.State() != StateRunning {
		t.Fatal("idle-x must stay: a stale model on the other card was enough")
	}
}

func TestEvictsTheSmallerIdleOccupantWhenNoneAreStale(t *testing.T) {
	r, _ := placementRouter(t,
		stanzaYAML("big", 28000, "1")+
			stanzaYAML("small", 20000, "2")+
			stanzaYAML("qwopus", 30000, "1|2"))
	mustLoad(t, r, "big")
	mustLoad(t, r, "small")

	if g := mustLoad(t, r, "qwopus"); g != 2 {
		t.Fatalf("qwopus on GPU %d, want 2 (evicting 20000 MB beats 28000 MB)", g)
	}
	if _, ok := r.managed["small"]; ok {
		t.Fatal("small should have been evicted")
	}
	if r.managed["big"] == nil {
		t.Fatal("big must stay loaded")
	}
}

func TestBusyModelGetsSecondInstanceOnFreeCard(t *testing.T) {
	r, log := placementRouter(t,
		stanzaYAML("agents-a1", 27000, "1|2", "    max_instances: 2\n"))
	s := r.cfg.Stanza("agents-a1")
	mustLoad(t, r, "agents-a1")
	r.holdOccupancy("agents-a1")

	key, ok := r.acquireInstance(s, false)
	if !ok || key != "agents-a1#1" {
		t.Fatalf("acquire = %q, %v; want a second instance agents-a1#1", key, ok)
	}
	if g, err := r.load(t.Context(), key, true); err != nil || g != 2 {
		t.Fatalf("second instance on GPU %d (err %v), want 2", g, err)
	}
	if got, want := strings.Join(log.visible(), ","), "1,2"; got != want {
		t.Fatalf("CUDA_VISIBLE_DEVICES per spawn = %s, want %s", got, want)
	}
}

func TestSecondInstanceNeverEvicts(t *testing.T) {
	r, _ := placementRouter(t,
		stanzaYAML("agents-a1", 27000, "1|2", "    max_instances: 2\n")+
			stanzaYAML("other", 27000, "2"))
	mustLoad(t, r, "agents-a1")
	mustLoad(t, r, "other")
	r.holdOccupancy("agents-a1")

	if key, ok := r.acquireInstance(r.cfg.Stanza("agents-a1"), false); ok {
		t.Fatalf("acquire without queue = %q; no card is free, want busy", key)
	}
	key, ok := r.acquireInstance(r.cfg.Stanza("agents-a1"), true)
	if !ok || key != "agents-a1" {
		t.Fatalf("queued acquire = %q, %v; want to queue on agents-a1", key, ok)
	}
	if r.managed["other"] == nil {
		t.Fatal("an extra copy must not evict another model")
	}
	if _, ok := r.managed["agents-a1#1"]; ok {
		t.Fatal("no second instance should exist")
	}
}

func TestIdleSecondInstanceIsSweptWhileBusyOneStays(t *testing.T) {
	r, _ := placementRouter(t,
		stanzaYAML("agents-a1", 27000, "1|2", "    max_instances: 2\n"))
	r.cfg.Stanza("agents-a1").IdleTTLSeconds = 1
	mustLoad(t, r, "agents-a1")
	mustLoad(t, r, "agents-a1#1")
	r.managed["agents-a1"].setLastUsed(time.Now().Add(-time.Minute))
	r.managed["agents-a1#1"].setLastUsed(time.Now().Add(-time.Minute))
	r.holdOccupancy("agents-a1")

	r.SweepStale()
	if _, ok := r.managed["agents-a1#1"]; ok {
		t.Fatal("idle stale second instance should be swept")
	}
	if r.managed["agents-a1"] == nil {
		t.Fatal("in-flight instance must stay")
	}
	if _, ok := r.ledger.Reserve("probe", &config.Stanza{ModelID: "probe", VramMB: 30000, Device: "2"}); !ok {
		t.Fatal("GPU 2 should be free after the sweep")
	}
}

func TestUnloadModelStopsEveryInstance(t *testing.T) {
	r, _ := placementRouter(t,
		stanzaYAML("agents-a1", 27000, "1|2", "    max_instances: 2\n"))
	mustLoad(t, r, "agents-a1")
	mustLoad(t, r, "agents-a1#1")
	if err := r.Unload("agents-a1"); err != nil {
		t.Fatalf("Unload: %v", err)
	}
	if len(r.managed) != 0 {
		t.Fatalf("%d instances left after Unload", len(r.managed))
	}
	for _, g := range []string{"1", "2"} {
		probe := &config.Stanza{ModelID: "probe" + g, VramMB: 32000, Device: g}
		if _, ok := r.ledger.Reserve(probe.ModelID, probe); !ok {
			t.Fatalf("GPU %s should be free after Unload", g)
		}
	}
}

// Two concurrent chats to a one-slot model are served by two backends on
// two cards instead of queueing behind each other.
func TestHTTPConcurrentChatsSpreadAcrossInstances(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var hits [2]atomic.Int32
	backend := func(i int) int {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.URL.Path == "/health" {
				w.WriteHeader(http.StatusOK)
				return
			}
			hits[i].Add(1)
			<-release
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(fmt.Sprintf(`{"backend":%d}`, i)))
		}))
		t.Cleanup(srv.Close)
		t.Cleanup(unblock)
		return srv.Listener.Addr().(*net.TCPAddr).Port
	}
	p0, p1 := backend(0), backend(1)

	cfg, err := config.Parse([]byte(fmt.Sprintf(`
start_port: 5900
stanzas:
  - model_id: agents-a1
    command: "fake --port {port} --parallel 1"
    env: ["CUDA_VISIBLE_DEVICES={gpu}"]
    vram_mb: 27000
    device: "1|2"
    max_instances: 2
    spin_up_seconds: 5
    port: %d
`, p0)))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	cfg.Stanza("agents-a1").InstancePorts[1] = p1
	r := New(cfg, NewLogger(io.Discard, false), "test")
	log := &spawnLog{}
	r.spawn = log.spawn
	r.ledger.SetGPUs(busterGPUs())
	t.Cleanup(func() { _ = r.Unload("agents-a1") })
	h := NewHandler(r, NewLogger(io.Discard, false))

	var wg sync.WaitGroup
	bodies := make([]string, 2)
	send := func(i int) {
		defer wg.Done()
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			bytes.NewBufferString(`{"model":"agents-a1","messages":[]}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		bodies[i] = fmt.Sprintf("%d %s", rec.Code, rec.Body.String())
	}
	waitHit := func(i int) {
		deadline := time.Now().Add(5 * time.Second)
		for hits[i].Load() == 0 {
			if time.Now().After(deadline) {
				t.Fatalf("backend %d never received a chat (hits %d/%d)", i, hits[0].Load(), hits[1].Load())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	wg.Add(2)
	go send(0)
	waitHit(0)
	go send(1)
	waitHit(1)
	unblock()
	wg.Wait()

	if hits[0].Load() != 1 || hits[1].Load() != 1 {
		t.Fatalf("hits = %d/%d, want one chat per instance", hits[0].Load(), hits[1].Load())
	}
	if !strings.HasPrefix(bodies[0], "200 ") || !strings.HasPrefix(bodies[1], "200 ") {
		t.Fatalf("responses = %q", bodies)
	}
	if got, want := strings.Join(log.visible(), ","), "1,2"; got != want {
		t.Fatalf("CUDA_VISIBLE_DEVICES per spawn = %s, want %s", got, want)
	}
}
