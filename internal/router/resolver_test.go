package router

import (
	"io"
	"testing"

	"model-router/internal/config"
)

func TestResolveRefPeerQualified(t *testing.T) {
	cfg := testConfig(t)
	ref := resolveRef("digger/remote-model", cfg)
	if ref.Peer != "digger" {
		t.Errorf("peer = %q, want digger", ref.Peer)
	}
	if ref.PeerID != "remote-model" {
		t.Errorf("peerID = %q, want remote-model", ref.PeerID)
	}
}

func TestResolveRefLocal(t *testing.T) {
	cfg := testConfig(t)
	ref := resolveRef("agents-a1", cfg)
	if ref.Local != "agents-a1" {
		t.Errorf("local = %q", ref.Local)
	}
}

func TestResolveRefPool(t *testing.T) {
	cfg := testConfig(t)
	ref := resolveRef("coding-pool", cfg)
	if ref.Pool != "coding-pool" {
		t.Errorf("pool = %q", ref.Pool)
	}
}

func TestMatchPeerQualifiedFull(t *testing.T) {
	cfg := testConfig(t)
	req := newReq("POST", "http://x/v1/chat/completions", "application/json", `{"model":"digger/remote-model","messages":[]}`)
	ref, err := matchRequest(req, cfg)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if ref.Peer != "digger" || ref.PeerID != "remote-model" {
		t.Errorf("got %+v", ref)
	}
}

func spilloverRouter(t *testing.T, yaml string) *Router {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	r := New(cfg, NewLogger(io.Discard, false), "test")
	return r
}

func TestResolvePoolOccupancyIncrements(t *testing.T) {
	r := spilloverRouter(t, `
start_port: 5900
stanzas:
  - model_id: a
    command: "x --port {port}"
    vram_mb: 1000
  - model_id: b
    command: "x --port {port}"
    vram_mb: 1000
pools:
  p:
    targets: [a, b]
    spillover: 1
`)
	r.ledger.SetGPUs([]*GPUState{gpu(0, 10000)})

	t1, err := r.resolvePool("p")
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if t1.Local != "a" {
		t.Fatalf("first target = %+v, want local a", t1)
	}

	t2, err := r.resolvePool("p")
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if t2.Local != "b" {
		t.Fatalf("second target = %+v, want local b (a at spillover cap)", t2)
	}

	if _, err := r.resolvePool("p"); err == nil {
		t.Fatal("third resolve should fail: both targets at cap")
	}

	r.releaseOccupancy(t1)
	t3, err := r.resolvePool("p")
	if err != nil {
		t.Fatalf("resolve after release: %v", err)
	}
	if t3.Local != "a" {
		t.Fatalf("after release target = %+v, want local a", t3)
	}
}

func TestResolvePoolPeerUsesVramMB(t *testing.T) {
	r := spilloverRouter(t, `
start_port: 5900
stanzas:
  - model_id: qwopus-27b-coder
    command: "x --port {port}"
    vram_mb: 8000
    aliases: [coding-model]
pools:
  p:
    targets: [digger/coding-model]
    spillover: 1
peers:
  - name: digger
    kind: router
    base_url: http://192.168.1.36:8082
    models: [coding-model]
`)
	// FreeMB above the old 1024 magic but below vram_mb must not admit.
	r.peers.Set("digger", &Telemetry{
		Node: "digger",
		GPUs: []*GPUState{{Index: 0, TotalMB: 24000, FreeMB: 2000}},
	})
	if _, err := r.resolvePool("p"); err == nil {
		t.Fatal("peer with 2000 MB free must not fit a 8000 MB model")
	}

	r.peers.Set("digger", &Telemetry{
		Node: "digger",
		GPUs: []*GPUState{{Index: 0, TotalMB: 24000, FreeMB: 9000}},
	})
	tgt, err := r.resolvePool("p")
	if err != nil {
		t.Fatalf("peer with 9000 MB free should fit: %v", err)
	}
	if tgt.Peer != "digger" || tgt.PeerID != "coding-model" {
		t.Fatalf("target = %+v, want digger/coding-model", tgt)
	}
}

func TestResolvePoolPeerOnlyCatalogRejectsUnknownVram(t *testing.T) {
	r := spilloverRouter(t, `
start_port: 5900
pools:
  p:
    targets: [digger/coding-model]
    spillover: 1
peers:
  - name: digger
    kind: router
    base_url: http://192.168.1.36:8082
    models: [coding-model]
`)
	r.peers.Set("digger", &Telemetry{
		Node: "digger",
		GPUs: []*GPUState{{Index: 0, TotalMB: 24000, FreeMB: 2000}},
	})
	if _, err := r.resolvePool("p"); err == nil {
		t.Fatal("peer-only 8000 MB model must not fit a 2000 MB GPU via FreeMB >= 0")
	}

	r.peers.Set("digger", &Telemetry{
		Node: "digger",
		GPUs: []*GPUState{{Index: 0, TotalMB: 24000, FreeMB: 9000}},
	})
	if _, err := r.resolvePool("p"); err == nil {
		t.Fatal("unknown peer VRAM must fail closed even when the GPU has 9000 MB free")
	}
}

func TestResolveTargetReturnsLocalTarget(t *testing.T) {
	r := New(testConfig(t), NewLogger(io.Discard, false), "test")
	tgt, err := r.resolveTarget(ModelRef{Local: "agents-a1"})
	if err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}
	if tgt.Local != "agents-a1" || tgt.Peer != "" {
		t.Fatalf("target = %+v, want local agents-a1", tgt)
	}
}

func TestModelStatusCopiesAPIType(t *testing.T) {
	r := spilloverRouter(t, `
start_port: 5900
stanzas:
  - model_id: chat-a
    command: "x --port {port}"
    api_type: chat
  - model_id: img-a
    command: "x --port {port}"
    api_type: image
`)
	got := map[string]string{}
	for _, ms := range r.AllModelStatuses() {
		if ms.Type == "model" {
			got[ms.ID] = ms.APIType
		}
	}
	if got["chat-a"] != "chat" || got["img-a"] != "image" {
		t.Fatalf("api_type = %v, want chat-a=chat img-a=image", got)
	}
}

func TestCatalogUniqueMeshModels(t *testing.T) {
	r := spilloverRouter(t, `
start_port: 5900
stanzas:
  - model_id: qwen38-27b
    name: "Qwen3.8 27B"
    command: "x --port {port}"
    unlisted: true
    aliases: [coding-model]
  - model_id: local-only
    command: "x --port {port}"
pools:
  coding-pool:
    targets: [qwen38-27b, digger/coding-model]
peers:
  - name: digger
    kind: router
    base_url: http://192.168.1.36:8082
    models: [qwen38-27b, coding-model, remote-only]
  - name: nugget
    kind: router
    base_url: http://192.168.1.62:8081
    models: [qwen38-27b, nugget-only]
`)
	r.peers.Set("digger", &Telemetry{Node: "digger", GPUs: []*GPUState{{Index: 0, TotalMB: 24000, FreeMB: 9000}}})
	r.peers.Set("nugget", &Telemetry{Node: "nugget", GPUs: []*GPUState{{Index: 0, TotalMB: 16000, FreeMB: 8000}}})

	got := map[string]ModelStatus{}
	for _, ms := range r.AllModelStatuses() {
		if _, dup := got[ms.ID]; dup {
			t.Fatalf("duplicate catalog id %q", ms.ID)
		}
		got[ms.ID] = ms
	}
	if _, ok := got["coding-pool"]; ok {
		t.Fatal("catalog listed selector coding-pool")
	}
	if _, ok := got["digger/coding-model"]; ok {
		t.Fatal("catalog listed peer-qualified id")
	}
	if got["qwen38-27b"].Origin != "local" {
		t.Fatalf("qwen38-27b origin = %q, want local", got["qwen38-27b"].Origin)
	}
	if got["coding-model"].ID != "" {
		t.Fatal("alias coding-model listed as its own catalog row")
	}
	if got["remote-only"].Origin != "remote" || got["nugget-only"].Origin != "remote" {
		t.Fatalf("remote origins: remote-only=%q nugget-only=%q", got["remote-only"].Origin, got["nugget-only"].Origin)
	}
	if _, ok := got["local-only"]; !ok {
		t.Fatal("local-only missing from catalog")
	}
}

func TestResolveMeshPicksFreshPeer(t *testing.T) {
	r := spilloverRouter(t, `
start_port: 5900
stanzas:
  - model_id: local-chat
    command: "x --port {port}"
    vram_mb: 1000
peers:
  - name: nugget
    kind: router
    base_url: http://192.168.1.62:8081
    models: [qwen38-27b]
`)
	if _, err := r.resolveTarget(ModelRef{Raw: "qwen38-27b"}); err == nil {
		t.Fatal("stale peer must not be selected")
	}
	r.peers.Set("nugget", &Telemetry{
		Node: "nugget",
		GPUs: []*GPUState{{Index: 0, TotalMB: 16000, FreeMB: 8000}},
	})
	tgt, err := r.resolveTarget(ModelRef{Raw: "qwen38-27b"})
	if err != nil {
		t.Fatalf("resolveMesh: %v", err)
	}
	if tgt.Peer != "nugget" || tgt.PeerID != "qwen38-27b" {
		t.Fatalf("target = %+v, want nugget/qwen38-27b", tgt)
	}
}

func TestResolveMeshPrefersLocal(t *testing.T) {
	r := spilloverRouter(t, `
start_port: 5900
stanzas:
  - model_id: qwen38-27b
    command: "x --port {port}"
    vram_mb: 1000
peers:
  - name: nugget
    kind: router
    base_url: http://192.168.1.62:8081
    models: [qwen38-27b]
`)
	r.peers.Set("nugget", &Telemetry{
		Node: "nugget",
		GPUs: []*GPUState{{Index: 0, TotalMB: 16000, FreeMB: 8000}},
	})
	tgt, err := r.resolveTarget(ModelRef{Local: "qwen38-27b"})
	if err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}
	if tgt.Local != "qwen38-27b" || tgt.Peer != "" {
		t.Fatalf("target = %+v, want local qwen38-27b", tgt)
	}
}
