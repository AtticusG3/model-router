package config

import (
	"path/filepath"
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
	if !cfg.PeerAdvertises("coding-model") {
		t.Error("PeerAdvertises(coding-model) = false")
	}
	if cfg.PeerAdvertises("agents-a1") {
		t.Error("PeerAdvertises(agents-a1) = true, want false")
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

func TestValidCatalogFilesParse(t *testing.T) {
	files := []string{
		"../../test/router-a.yaml",
		"../../test/router-b.yaml",
		"../../configs/buster.yaml",
		"../../configs/digger.yaml",
		"../../configs/nomad.yaml",
		"../../configs/gareths-homelab.yaml",
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			if _, err := Load(f); err != nil {
				t.Fatalf("%s: %v", f, err)
			}
		})
	}
}

func TestParseRejectsInvalidCatalog(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "unknown pool target",
			raw: `
stanzas:
  - model_id: a
    command: "x"
pools:
  p:
    targets: [missing]
`,
			want: `unknown target "missing"`,
		},
		{
			name: "unknown pool peer target",
			raw: `
stanzas:
  - model_id: a
    command: "x"
pools:
  p:
    targets: [ghost/model]
`,
			want: `unknown target "ghost/model"`,
		},
		{
			name: "pool target not advertised by peer",
			raw: `
stanzas:
  - model_id: a
    command: "x"
peers:
  - name: digger
    base_url: http://127.0.0.1:9
    models: [coding-model]
pools:
  p:
    targets: [digger/nope]
`,
			want: `unknown target "digger/nope"`,
		},
		{
			name: "missing preload id",
			raw: `
stanzas:
  - model_id: a
    command: "x"
preload: [missing]
`,
			want: `preload: unknown model "missing"`,
		},
		{
			name: "empty peer name",
			raw: `
peers:
  - base_url: http://127.0.0.1:9
`,
			want: "name is required",
		},
		{
			name: "kind not in enum",
			raw: `
peers:
  - name: x
    kind: proxy
    base_url: http://127.0.0.1:9
`,
			want: `kind "proxy" is not router or openai`,
		},
		{
			name: "duplicate path_default on same prefix",
			raw: `
stanzas:
  - model_id: a
    command: "x"
    match:
      path_prefix: /sdapi/v1
      path_default: true
  - model_id: b
    command: "x"
    match:
      path_prefix: /sdapi/v1
      path_default: true
`,
			want: `duplicate path_default for prefix "/sdapi/v1"`,
		},
		{
			name: "alias pool collision",
			raw: `
stanzas:
  - model_id: a
    command: "x"
    aliases: [coding-pool]
pools:
  coding-pool:
    targets: [a]
`,
			want: `alias "coding-pool" collides with a pool`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.raw))
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q, want substring %q", err, tt.want)
			}
		})
	}
}

func TestParallelFromCommand(t *testing.T) {
	tests := []struct {
		cmd  string
		want int
	}{
		{cmd: "llama-server -m a.gguf --port 5900", want: 1},
		{cmd: "llama-server --parallel 2 --cont-batching", want: 2},
		{cmd: "llama-server --parallel=3", want: 3},
		{cmd: "llama-server -np 4 --n-predict 64", want: 4},
		{cmd: "llama-server -np=2 --n-predict 64", want: 2},
		{cmd: "llama-server --parallel 1 --parallel 3", want: 3},
		{cmd: "sd-server --listen-port 1", want: 1},
	}
	for _, tt := range tests {
		if got := parallelFromCommand(tt.cmd); got != tt.want {
			t.Errorf("parallelFromCommand(%q) = %d, want %d", tt.cmd, got, tt.want)
		}
	}
}

func TestParseDerivesSlotsFromParallel(t *testing.T) {
	cfg, err := Parse([]byte(`
stanzas:
  - model_id: chat
    command: "llama-server --port {port} --parallel 2"
  - model_id: embed
    command: "llama-server --port {port} --parallel=3"
  - model_id: override
    command: "llama-server --port {port} --parallel 1"
    slots: 4
  - model_id: image
    command: "sd-server --listen-port ${PORT}"
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Stanza("chat").Slots != 2 {
		t.Errorf("chat slots = %d, want 2", cfg.Stanza("chat").Slots)
	}
	if cfg.Stanza("embed").Slots != 3 {
		t.Errorf("embed slots = %d, want 3", cfg.Stanza("embed").Slots)
	}
	if cfg.Stanza("override").Slots != 4 {
		t.Errorf("explicit slots = %d, want 4 (must beat --parallel 1)", cfg.Stanza("override").Slots)
	}
	if cfg.Stanza("image").Slots != 1 {
		t.Errorf("image slots = %d, want 1", cfg.Stanza("image").Slots)
	}
}
