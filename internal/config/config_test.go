package config

import (
	"strings"
	"testing"
)

const testConfig = `
listen: 0.0.0.0:18080
start_port: 5900
reserved_ports: [5999]
stanzas:
  - model_id: agents-a1
    command: "/bin/llama-server -m a.gguf --port {port} --device CUDA1"
    vram_mb: 22000
    device: CUDA1
    api_type: chat
    health_check: /health
    idle_ttl_seconds: 0
  - model_id: krea-2-turbo
    command: "/opt/ai/bin/sd-server -l 0.0.0.0 --listen-port ${PORT}"
    vram_mb: 9000
    api_type: image
    match:
      path_prefix: /sdapi/v1
      path_default: true
  - model_id: krea-2-redcraft
    command: "/opt/ai/bin/sd-server -l 0.0.0.0 --listen-port ${PORT}"
    vram_mb: 9000
    api_type: image
    match:
      path_prefix: /sdapi/v1
pools:
  coding-pool:
    strategy: spillover
    targets: [agents-a1]
    spillover: 1
peers:
  - name: digger
    kind: router
    base_url: http://192.168.1.36:8082
    models: [coding-model]
preload: [agents-a1]
telemetry:
  poll_seconds: 2
`

func TestParse(t *testing.T) {
	cfg, err := Parse([]byte(testConfig))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Listen != "0.0.0.0:18080" {
		t.Errorf("listen = %q", cfg.Listen)
	}
	if len(cfg.Stanzas) != 3 {
		t.Fatalf("stanzas = %d", len(cfg.Stanzas))
	}
	// agents-a1 gets 5900 (start_port), krea-2-turbo 5901, redcraft 5902.
	if cfg.Stanzas[0].Port != 5900 {
		t.Errorf("agents-a1 port = %d, want 5900", cfg.Stanzas[0].Port)
	}
	if cfg.Stanzas[1].Port != 5901 {
		t.Errorf("krea-2-turbo port = %d, want 5901", cfg.Stanzas[1].Port)
	}
	if cfg.Stanzas[2].Port != 5902 {
		t.Errorf("krea-2-redcraft port = %d, want 5902", cfg.Stanzas[2].Port)
	}
	// Macro substitution.
	if !strings.Contains(cfg.Stanzas[0].Command, "--port 5900") {
		t.Errorf("agents-a1 command not substituted: %q", cfg.Stanzas[0].Command)
	}
	if !strings.Contains(cfg.Stanzas[1].Command, "--listen-port 5901") {
		t.Errorf("krea-2-turbo command not substituted: %q", cfg.Stanzas[1].Command)
	}
	// Defaults.
	if cfg.Stanzas[1].HealthCheck != "/health" {
		t.Errorf("default health check = %q", cfg.Stanzas[1].HealthCheck)
	}
	if cfg.Telemetry.PeerStaleSeconds != 15 {
		t.Errorf("default peer stale = %d", cfg.Telemetry.PeerStaleSeconds)
	}
	// Lookups.
	if cfg.Stanza("agents-a1") == nil {
		t.Error("stanza lookup failed")
	}
	if cfg.Pool("coding-pool") == nil {
		t.Error("pool lookup failed")
	}
	if cfg.Peer("digger") == nil {
		t.Error("peer lookup failed")
	}
	if cfg.Peer("digger").Kind != "router" {
		t.Errorf("peer kind = %q", cfg.Peer("digger").Kind)
	}
	if cfg.Peer("digger").BodyField != "model" {
		t.Errorf("peer body_field = %q, want default \"model\"", cfg.Peer("digger").BodyField)
	}
}

func TestReservedPortSkipped(t *testing.T) {
	cfg := `
start_port: 5998
reserved_ports: [5999]
stanzas:
  - model_id: a
    command: "x --port {port}"
  - model_id: b
    command: "x --port {port}"
`
	c, err := Parse([]byte(cfg))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.Stanzas[0].Port != 5998 {
		t.Errorf("a port = %d", c.Stanzas[0].Port)
	}
	if c.Stanzas[1].Port != 6000 {
		t.Errorf("b port = %d (should skip reserved 5999)", c.Stanzas[1].Port)
	}
}

func TestMatchFieldsMutuallyExclusive(t *testing.T) {
	cfg := `
stanzas:
  - model_id: a
    command: "x"
    match:
      body_field: model
      path_prefix: /sdapi/v1
`
	if _, err := Parse([]byte(cfg)); err == nil {
		t.Fatal("expected error for stanza with both body_field and path_prefix")
	}
}

func TestDuplicateModelIDRejected(t *testing.T) {
	cfg := `
stanzas:
  - model_id: a
    command: "x"
  - model_id: a
    command: "y"
`
	if _, err := Parse([]byte(cfg)); err == nil {
		t.Fatal("expected duplicate model_id error")
	}
}

func TestPeerBodyFieldExplicit(t *testing.T) {
	cfg := `
peers:
  - name: custom
    base_url: http://127.0.0.1:9999
    body_field: model_name
    models: [a]
`
	c, err := Parse([]byte(cfg))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.Peer("custom").BodyField != "model_name" {
		t.Errorf("body_field = %q, want model_name", c.Peer("custom").BodyField)
	}
}

func TestBodyFields(t *testing.T) {
	cfg := `
stanzas:
  - model_id: a
    command: "x"
    match:
      body_field: model_name
  - model_id: b
    command: "x"
    match:
      path_prefix: /sdapi/v1
  - model_id: c
    command: "x"
`
	c, err := Parse([]byte(cfg))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := c.BodyFields()
	want := []string{"model_name", "model"}
	if len(got) != len(want) {
		t.Fatalf("BodyFields() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("BodyFields() = %v, want %v", got, want)
		}
	}
}
