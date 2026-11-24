package config

import (
	"fmt"
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
    base_url: http://10.99.0.36:8082
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

// The matcher's ceiling is the largest cap any stanza accepts, whatever order
// the stanzas come in.
func TestMaxRequestBodyBytesIsLargestStanzaCap(t *testing.T) {
	cfg, err := Parse([]byte(`
max_request_body_mb: 256
stanzas:
  - model_id: big
    command: "x"
    max_request_body_mb: 1024
  - model_id: mid
    command: "y"
    max_request_body_mb: 512
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := cfg.MaxRequestBodyBytes(); got != 1024<<20 {
		t.Fatalf("MaxRequestBodyBytes = %d MB, want 1024 MB", got>>20)
	}
}

func TestStanzaShareDefaultsOn(t *testing.T) {
	cfg, err := Parse([]byte(`
stanzas:
  - model_id: open
    command: "x"
  - model_id: private
    command: "y"
    share: false
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !cfg.Stanza("open").Shared() || cfg.Stanza("private").Shared() {
		t.Fatalf("shared: open=%v private=%v, want true false", cfg.Stanza("open").Shared(), cfg.Stanza("private").Shared())
	}
}

// A cap so large its byte count (plus the one-byte over-cap probe) would wrap
// int64 is refused, so the matcher never reads a body with a negative limit.
func TestMaxRequestBodyMBOverLimitRejected(t *testing.T) {
	huge := "8796093022208" // 1<<43 MB: shifted to bytes it wraps int64 to negative
	for name, raw := range map[string]string{
		"top level": "max_request_body_mb: " + huge + "\nstanzas:\n  - model_id: a\n    command: \"x\"\n",
		"stanza":    "stanzas:\n  - model_id: a\n    command: \"x\"\n    max_request_body_mb: " + huge + "\n",
	} {
		if _, err := Parse([]byte(raw)); err == nil || !strings.Contains(err.Error(), "max_request_body_mb") {
			t.Fatalf("%s: err = %v, want a max_request_body_mb range error", name, err)
		}
	}
	if _, err := Parse([]byte("max_request_body_mb: 1048576\nstanzas:\n  - model_id: a\n    command: \"x\"\n")); err != nil {
		t.Fatalf("the 1 TiB limit itself should parse: %v", err)
	}
}

// path_rewrite swaps the first matching prefix and keeps the rest of the path;
// a relative from or to, or a negative request_timeout_seconds, is refused.
func TestPathRewriteAndRequestTimeout(t *testing.T) {
	cfg, err := Parse([]byte(`
stanzas:
  - model_id: a
    command: "x"
    request_timeout_seconds: 7200
    path_rewrite:
      - {from: /v1/audio/transcriptions, to: /inference}
      - {from: /v1/audio, to: /audio}
`))
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Stanza("a")
	for in, want := range map[string]string{
		"/v1/audio/transcriptions": "/inference",
		"/v1/audio/translations":   "/audio/translations",
		"/v1/chat/completions":     "/v1/chat/completions",
	} {
		if got := s.BackendPath(in); got != want {
			t.Errorf("BackendPath(%q) = %q, want %q", in, got, want)
		}
	}
	if s.RequestTimeoutSeconds != 7200 {
		t.Errorf("request_timeout_seconds = %d", s.RequestTimeoutSeconds)
	}
	for name, extra := range map[string]string{
		"relative from":    "    path_rewrite: [{from: v1/audio, to: /audio}]\n",
		"empty to":         "    path_rewrite: [{from: /v1/audio}]\n",
		"negative timeout": "    request_timeout_seconds: -1\n",
	} {
		if _, err := Parse([]byte("stanzas:\n  - model_id: a\n    command: \"x\"\n" + extra)); err == nil {
			t.Errorf("%s: parsed, want an error", name)
		}
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

// TestTrackedCaptionStanzasUseTheProfile pins the tracked stanzas each family
// ticket enabled to their caption with an enhancer, and the Krea 2 and Qwen
// edit variants the #207 and #208 research left off to no caption.
// flux2-klein (#209) and flux3-klein (#210) are each in the two tracked
// configs that carry them.
func TestTrackedCaptionStanzasUseTheProfile(t *testing.T) {
	files, _ := filepath.Glob("../../configs/*.yaml")
	cases := []struct {
		model, caption string
		want           int
	}{
		{"z-anime-turbo", "z-anime", 3},
		{"krea-2-turbo", "krea2", 3},
		{"fibo-2.0-edit-turbo", "fibo-2.0", 2},
		{"krea-character-creator", "", 0},
		{"krea2-ostris-edit", "", 0},
		{"flux2-klein", "flux2-klein", 2},
		{"flux3-klein", "flux3-klein", 2},
		{"qwen-image-2.1", "qwen-image-2.1", 1},
		{"qwen-image-edit-2511", "", 0},
	}
	for _, tc := range cases {
		enabled := 0
		for _, f := range files {
			cfg, err := Load(f)
			if err != nil {
				continue // overlays and partial files are covered elsewhere
			}
			s := cfg.Stanza(tc.model)
			if s == nil || s.ImagePipeline == nil || s.ImagePipeline.Caption == "" {
				continue
			}
			if s.ImagePipeline.Caption != tc.caption || s.ImagePipeline.Enhancer == nil {
				t.Errorf("%s: %s pipeline = %+v", f, tc.model, s.ImagePipeline)
			}
			enabled++
		}
		if enabled != tc.want {
			t.Errorf("%d tracked %s stanzas have a caption, want %d", enabled, tc.model, tc.want)
		}
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

func TestParseDevice(t *testing.T) {
	cases := []struct {
		in    string
		idxs  string
		split bool
		bad   bool
	}{
		{in: "", idxs: "[]"},
		{in: "1", idxs: "[1]"},
		{in: "CUDA2", idxs: "[2]"},
		{in: "1|2", idxs: "[1 2]"},
		{in: "0,1,2", idxs: "[0 1 2]", split: true},
		{in: "1|1", idxs: "[1]"},
		{in: "1,2|3", bad: true},
		{in: "auto", bad: true},
		{in: "1x", bad: true},
	}
	for _, c := range cases {
		idxs, split, err := ParseDevice(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("ParseDevice(%q) = %v, want an error", c.in, idxs)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseDevice(%q): %v", c.in, err)
			continue
		}
		if got := fmt.Sprint(idxs); got != c.idxs && !(c.idxs == "[]" && idxs == nil) {
			t.Errorf("ParseDevice(%q) = %s, want %s", c.in, got, c.idxs)
		}
		if split != c.split {
			t.Errorf("ParseDevice(%q) split = %v, want %v", c.in, split, c.split)
		}
	}
}

func TestGPUChoiceRequiresGPUMacro(t *testing.T) {
	_, err := Parse([]byte(`
stanzas:
  - model_id: chat
    command: "llama-server --port {port}"
    env: ["CUDA_VISIBLE_DEVICES=1"]
    device: "1|2"
`))
	if err == nil || !strings.Contains(err.Error(), "{gpu}") {
		t.Fatalf("err = %v, want a {gpu} error: the backend would ignore the chosen card", err)
	}
}

func TestSplitDeviceRejectsReplicas(t *testing.T) {
	_, err := Parse([]byte(`
stanzas:
  - model_id: flash
    command: "llama-server --port {port}"
    device: "0,1,2"
    max_instances: 2
`))
	if err == nil {
		t.Fatal("a model split across GPUs cannot also run extra copies")
	}
}

func TestReplicasNeedPortMacro(t *testing.T) {
	_, err := Parse([]byte(`
stanzas:
  - model_id: edit
    command: "python server.py"
    env: ["PORT=18100", "CUDA_VISIBLE_DEVICES={gpu}"]
    device: "1|2"
    max_instances: 2
    port: 18100
`))
	if err == nil || !strings.Contains(err.Error(), "{port}") {
		t.Fatalf("err = %v, want a {port} error: both copies would bind 18100", err)
	}
}

func TestInstancePortsAvoidStanzaAndReservedPorts(t *testing.T) {
	cfg, err := Parse([]byte(`
start_port: 5901
reserved_ports: [5906]
stanzas:
  - model_id: chat
    command: "llama-server --port {port}"
    env: ["CUDA_VISIBLE_DEVICES={gpu}"]
    device: "1|2"
    max_instances: 3
    port: 5901
  - model_id: image
    command: "sd-server --port {port}"
    port: 5905
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := fmt.Sprint(cfg.Stanza("chat").InstancePorts); got != "[5901 5907 5908]" {
		t.Errorf("chat instance ports = %s, want [5901 5907 5908] (above 5905, skipping reserved 5906)", got)
	}
	if got := fmt.Sprint(cfg.Stanza("image").InstancePorts); got != "[5905]" {
		t.Errorf("image instance ports = %s, want [5905]", got)
	}
}

func TestRenderFillsGPUAndInstancePort(t *testing.T) {
	cfg, err := Parse([]byte(`
start_port: 5901
stanzas:
  - model_id: chat
    command: "llama-server --port {port} --device CUDA0"
    env: ["CUDA_VISIBLE_DEVICES={gpu}", "TAG=chat-{port}"]
    device: "1|2"
    max_instances: 2
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	s := cfg.Stanza("chat")
	cmd, env := s.Render(1, 2)
	if cmd != "llama-server --port 5902 --device CUDA0" {
		t.Errorf("instance 1 command = %q", cmd)
	}
	if got := strings.Join(env, " "); got != "CUDA_VISIBLE_DEVICES=2 TAG=chat-5902" {
		t.Errorf("instance 1 env = %q", got)
	}
	cmd, env = s.Render(0, -1)
	if cmd != "llama-server --port 5901 --device CUDA0" || env[0] != "CUDA_VISIBLE_DEVICES=1" {
		t.Errorf("instance 0 without telemetry = %q %v, want port 5901 on the first listed GPU", cmd, env)
	}
}

func TestParseRejectsBadFilters(t *testing.T) {
	tests := []struct{ name, raw, want string }{
		{"strip model", "stanzas:\n  - {model_id: a, command: x, filters: {stripParams: [model]}}\n", `cannot strip or set "model"`},
		{"set model", "stanzas:\n  - {model_id: a, command: x, filters: {setParams: {model: b}}}\n", `cannot strip or set "model"`},
		{"set model if absent", "stanzas:\n  - {model_id: a, command: x, filters: {setParams: {\"model?\": b}}}\n", `cannot strip or set "model"`},
		{"set model by id", "stanzas:\n  - {model_id: a, command: x, filters: {setParamsByID: {\"a:hi\": {model: b}}}}\n", `cannot strip or set "model"`},
		{"strip custom body field", "stanzas:\n  - {model_id: a, command: x, match: {body_field: name}, filters: {stripParams: [name]}}\n", `cannot strip or set "name"`},
		{"peer strips model", "peers:\n  - {name: p, base_url: \"http://127.0.0.1:9\", filters: {stripParams: [model]}}\n", `cannot strip or set "model"`},
		{"peer by id", "peers:\n  - {name: p, base_url: \"http://127.0.0.1:9\", filters: {setParamsByID: {x: {a: 1}}}}\n", "only valid on stanzas"},
		{"empty key", "stanzas:\n  - {model_id: a, command: x, filters: {stripParams: [\"?\"]}}\n", "empty parameter name"},
		{"id collides", "stanzas:\n  - {model_id: a, command: x, filters: {setParamsByID: {b: {t: 1}}}}\n  - {model_id: b, command: x}\n", `id "b" collides`},
		{"id has hash", "stanzas:\n  - {model_id: a, command: x, filters: {setParamsByID: {\"a#2\": {t: 1}}}}\n", "contains '#'"},
		{"rename from model", "stanzas:\n  - {model_id: a, command: x, filters: {renameParams: {model: m}}}\n", `cannot strip or set "model"`},
		{"rename to model", "stanzas:\n  - {model_id: a, command: x, filters: {renameParams: {name: model}}}\n", `cannot strip or set "model"`},
		{"path_rewrite no slash", "stanzas:\n  - {model_id: a, command: x, path_rewrite: [{from: /v1/audio/speech, to: tts}]}\n", "must both start with /"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse([]byte(tt.raw)); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestLaunchEqualIgnoresEverythingButTheLaunch(t *testing.T) {
	stanza := func(fields string) *Stanza {
		cfg, err := Parse([]byte("stanzas:\n  - {model_id: a, port: 5901, command: \"srv --port {port}\", " + fields + "}\n"))
		if err != nil {
			t.Fatal(err)
		}
		return cfg.Stanza("a")
	}
	base := stanza("idle_ttl_seconds: 10, env: [K=1]")
	for _, same := range []string{"idle_ttl_seconds: 99, env: [K=1]", "env: [K=1], slots: 4, filters: {stripParams: [top_k]}", "env: [K=1], hold_seconds: 30, vram_mb: 8000"} {
		if !base.LaunchEqual(stanza(same)) {
			t.Errorf("%q: launch reported changed", same)
		}
	}
	if base.LaunchEqual(stanza("env: [K=2]")) {
		t.Error("env change not seen")
	}
	other := stanza("env: [K=1]")
	other.Command = "srv --port {port} --flag"
	if base.LaunchEqual(other) {
		t.Error("command change not seen")
	}
}

func TestFilterIDsExpandModelID(t *testing.T) {
	cfg, err := Parse([]byte("stanzas:\n  - {model_id: a, command: x, filters: {setParamsByID: {\"${MODEL_ID}:high\": {t: 1}}}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Alias("a:high") != cfg.Stanza("a") || cfg.Stanza("a").FilterIDs()[0] != "a:high" {
		t.Error("a:high does not resolve to stanza a")
	}
}

func TestAdmitTimeoutDefaultsAndOverrides(t *testing.T) {
	cfg, err := Parse([]byte("start_port: 5900\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AdmitTimeout() != DefaultAdmitTimeout {
		t.Fatalf("default = %v, want %v", cfg.AdmitTimeout(), DefaultAdmitTimeout)
	}
	cfg, err = Parse([]byte("start_port: 5900\nadmit_timeout_seconds: 7\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AdmitTimeout().Seconds() != 7 {
		t.Fatalf("override = %v, want 7s", cfg.AdmitTimeout())
	}
}

func TestAPITypeValidation(t *testing.T) {
	for _, at := range APITypes {
		if _, err := Parse([]byte("stanzas:\n  - {model_id: a, command: x, api_type: " + at + "}\n")); err != nil {
			t.Errorf("api_type %q rejected: %v", at, err)
		}
	}
	cfg, err := Parse([]byte("stanzas:\n  - {model_id: a, command: x}\n"))
	if err != nil {
		t.Fatalf("empty api_type: %v", err)
	}
	if cfg.Stanza("a").APIType != APITypeChat {
		t.Errorf("empty api_type = %q, want chat", cfg.Stanza("a").APIType)
	}
	_, err = Parse([]byte("stanzas:\n  - {model_id: a, command: x, api_type: transcribe}\n"))
	if err == nil || !strings.Contains(err.Error(), `api_type "transcribe" is not valid`) || !strings.Contains(err.Error(), APITypeAudioGen) {
		t.Fatalf("transcribe err = %v, want it named with the vocabulary listed", err)
	}
}

// A decision stanza declares the wire contract it speaks; -check names the
// stanza when that contract is unknown or set on a non-decision stanza.
func TestDecisionConfigValidation(t *testing.T) {
	for _, extra := range []string{"", ", decision_style: systemone"} {
		cfg, err := Parse([]byte("stanzas:\n  - {model_id: kev-9b, command: x, api_type: decision" + extra + "}\n"))
		if err != nil {
			t.Fatalf("decision stanza%s rejected: %v", extra, err)
		}
		if got := cfg.Stanza("kev-9b").APIType; got != "decision" {
			t.Fatalf("api_type = %q, want decision", got)
		}
	}
	for _, tc := range []struct{ yaml, want string }{
		{"api_type: decision, decision_style: openai", `stanza "kev-9b": decision_style "openai" is not valid; must be one of systemone`},
		{"api_type: chat, decision_style: systemone", `stanza "kev-9b": decision_style is only valid with api_type: decision`},
		{"api_type: decisions", `stanza "kev-9b": api_type "decisions" is not valid`},
	} {
		_, err := Parse([]byte("stanzas:\n  - {model_id: kev-9b, command: x, " + tc.yaml + "}\n"))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.yaml, err, tc.want)
		}
	}
	cfg, err := Load("../../configs/kev-9b.example.yaml")
	if err != nil {
		t.Fatalf("tracked decision example: %v", err)
	}
	if s := cfg.Stanza("kev-9b"); s == nil || s.APIType != "decision" {
		t.Fatalf("kev-9b.example.yaml: kev-9b stanza = %+v, want api_type decision", s)
	}
}

func TestPeerSpillValidation(t *testing.T) {
	cfg, err := Parse([]byte("stanzas:\n  - {model_id: a, command: x}\n"))
	if err != nil {
		t.Fatalf("empty peer_spill: %v", err)
	}
	if got := cfg.Stanza("a").PeerSpill; got != PeerSpillAuto {
		t.Errorf("empty peer_spill = %q, want %q", got, PeerSpillAuto)
	}
	for _, mode := range PeerSpills {
		if _, err := Parse([]byte("stanzas:\n  - {model_id: a, command: x, peer_spill: " + mode + "}\n")); err != nil {
			t.Errorf("peer_spill %q rejected: %v", mode, err)
		}
	}
	_, err = Parse([]byte("stanzas:\n  - {model_id: a, command: x, peer_spill: always}\n"))
	if err == nil || !strings.Contains(err.Error(), `peer_spill "always" is not valid`) || !strings.Contains(err.Error(), PeerSpillOnFailure) || !strings.Contains(err.Error(), PeerSpillNever) {
		t.Fatalf("always err = %v, want it named with the vocabulary listed", err)
	}
}

func TestCapabilityValidation(t *testing.T) {
	if _, err := Parse([]byte("stanzas:\n  - {model_id: a, command: x, api_type: video, capabilities: [vision, tools, duration, resolution, seed]}\n")); err != nil {
		t.Fatalf("known capabilities rejected: %v", err)
	}
	_, err := Parse([]byte("stanzas:\n  - {model_id: a, command: x, api_type: chat, capabilities: [seed]}\n"))
	if err == nil || !strings.Contains(err.Error(), `capability "seed" is only valid with api_type: video`) {
		t.Fatalf("chat seed capability err = %v, want it rejected", err)
	}
	_, err = Parse([]byte("stanzas:\n  - {model_id: a, command: x, capabilities: [telepathy]}\n"))
	if err == nil || !strings.Contains(err.Error(), `capability "telepathy" is not valid`) || !strings.Contains(err.Error(), "vision") {
		t.Fatalf("telepathy err = %v, want it named with the vocabulary listed", err)
	}
}

func TestJobAdapterValidation(t *testing.T) {
	cfg, err := Parse([]byte("stanzas:\n  - {model_id: v, command: x, api_type: video, job: {adapter: sdcpp}}\n"))
	if err != nil {
		t.Fatalf("sdcpp adapter rejected: %v", err)
	}
	if got := cfg.Stanza("v").Job.Adapter; got != JobAdapterSDCpp {
		t.Fatalf("adapter = %q, want %q", got, JobAdapterSDCpp)
	}
	cfg, err = Parse([]byte("stanzas:\n  - {model_id: v, command: x, api_type: video, job: {}}\n"))
	if err != nil {
		t.Fatalf("empty adapter rejected: %v", err)
	}
	if got := cfg.Stanza("v").Job.Adapter; got != JobAdapterOpenAI {
		t.Fatalf("empty adapter = %q, want %q", got, JobAdapterOpenAI)
	}
	_, err = Parse([]byte("stanzas:\n  - {model_id: v, command: x, api_type: video, job: {adapter: comfyui}}\n"))
	if err == nil || !strings.Contains(err.Error(), `job.adapter "comfyui" is not valid`) || !strings.Contains(err.Error(), JobAdapterSDCpp) {
		t.Fatalf("comfyui err = %v, want it named with the vocabulary listed", err)
	}
	_, err = Parse([]byte("stanzas:\n  - {model_id: i, command: x, api_type: image, job: {adapter: sdcpp}}\n"))
	if err == nil || !strings.Contains(err.Error(), "job is only valid with api_type: video") {
		t.Fatalf("job on image err = %v, want api_type restriction", err)
	}
}

func TestCPUDeviceParses(t *testing.T) {
	cfg, err := Parse([]byte(`
stanzas:
  - model_id: stt
    command: "stt-server --port {port}"
    vram_mb: 0
    device: cpu
    api_type: transcription
`))
	if err != nil {
		t.Fatalf("device: cpu rejected: %v", err)
	}
	if !cfg.Stanza("stt").CPUOnly() {
		t.Error("device: cpu must be CPUOnly")
	}
	if _, _, err := ParseDevice("CPU"); err != nil {
		t.Fatalf("ParseDevice(CPU): %v", err)
	}
	gpu, err := Parse([]byte("stanzas:\n  - {model_id: g, command: x, device: CUDA0, vram_mb: 9000}\n"))
	if err != nil {
		t.Fatalf("gpu stanza: %v", err)
	}
	if gpu.Stanza("g").CPUOnly() {
		t.Error("a GPU device must not read as CPUOnly")
	}
}

func TestDefaultStanzaForType(t *testing.T) {
	cfg, err := Parse([]byte(`
stanzas:
  - model_id: stt
    command: x
    api_type: transcription
  - model_id: speech-a
    command: x
    api_type: speech
  - model_id: speech-b
    command: x
    api_type: speech
    match: {path_default: true}
  - model_id: krea
    command: x
    api_type: image
    match: {path_prefix: /sdapi/v1, path_default: true}
  - model_id: sdxl
    command: x
    api_type: image
    match: {path_prefix: /sdapi/v1}
`))
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := cfg.DefaultStanza(APITypeTranscription); id != "stt" {
		t.Errorf("single transcription = %q, want stt", id)
	}
	if id, cands := cfg.DefaultStanza(APITypeSpeech); id != "speech-b" || len(cands) != 2 {
		t.Errorf("speech default = %q over %v, want speech-b of two", id, cands)
	}
	if id, _ := cfg.DefaultStanza(APITypeImage); id != "krea" {
		t.Errorf("image default = %q, want krea (its path_default)", id)
	}
	if id, cands := cfg.DefaultStanza(APITypeVideo); id != "" || cands != nil {
		t.Errorf("type with no stanza = %q %v, want empty", id, cands)
	}
}

// Two path_default stanzas of one api_type are ambiguous: Parse rejects them,
// and DefaultStanza (built past Parse) names both instead of picking the last.
func TestDefaultStanzaAmbiguousPathDefault(t *testing.T) {
	_, err := Parse([]byte(`
stanzas:
  - {model_id: speech-a, command: x, api_type: speech, match: {path_default: true}}
  - {model_id: speech-b, command: x, api_type: speech, match: {path_prefix: /tts, path_default: true}}
`))
	if err == nil || !strings.Contains(err.Error(), `duplicate path_default for api_type "speech"`) {
		t.Fatalf("Parse err = %v, want duplicate path_default for api_type", err)
	}
	cfg := &Config{Stanzas: []Stanza{
		{ModelID: "speech-a", APIType: APITypeSpeech, Match: Match{PathDefault: true}},
		{ModelID: "speech-b", APIType: APITypeSpeech, Match: Match{PathPrefix: "/tts", PathDefault: true}},
	}}
	id, cands := cfg.DefaultStanza(APITypeSpeech)
	if id != "" || len(cands) != 2 || cands[0] != "speech-a" || cands[1] != "speech-b" {
		t.Fatalf("DefaultStanza = %q %v, want empty id and both candidates", id, cands)
	}
}

func TestImagePipelineForms(t *testing.T) {
	cfg, err := Parse([]byte(`
stanzas:
  - model_id: sd-bare
    command: "sd-server --listen-ip 127.0.0.1 --listen-port {port}"
    api_type: image
    health_check: /v1/models
    image_pipeline: sd-server
  - model_id: sd-ideo
    command: "sd-server --listen-port {port}"
    api_type: image
    image_pipeline: ideogram4
  - model_id: sd-custom
    command: "sd-server --listen-port {port}"
    api_type: image
    image_pipeline:
      caption: ideogram4
      enhancer: {model: enhancer-a, timeout_seconds: 20}
      retry_blocked: 2
  - model_id: sd-zimage
    command: "sd-server --listen-port {port}"
    api_type: image
    image_pipeline:
      caption: z-image
      enhancer: {model: enhancer-a, timeout_seconds: 45}
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	sd := cfg.Stanza("sd-bare")
	if sd.ImagePipeline == nil || sd.ImagePipeline.Caption != "" || sd.HealthCheck != "/v1/models" {
		t.Fatalf("sd-server preset = %+v, health %q", sd.ImagePipeline, sd.HealthCheck)
	}
	ideo := cfg.Stanza("sd-ideo")
	if ideo.ImagePipeline == nil || ideo.ImagePipeline.Caption != "ideogram4" {
		t.Fatalf("ideogram4 preset = %+v, want caption ideogram4", ideo.ImagePipeline)
	}
	custom := cfg.Stanza("sd-custom").ImagePipeline
	if custom == nil || custom.Caption != "ideogram4" || custom.RetryBlocked != 2 ||
		custom.Enhancer == nil || custom.Enhancer.Model != "enhancer-a" || custom.Enhancer.TimeoutSeconds != 20 {
		t.Fatalf("map form = %+v", custom)
	}
	if z := cfg.Stanza("sd-zimage").ImagePipeline; z == nil || z.Caption != "z-image" || z.Enhancer == nil || z.Enhancer.TimeoutSeconds != 45 {
		t.Fatalf("z-image caption = %+v", z)
	}
}

func TestImagePipelineValidation(t *testing.T) {
	cases := []struct{ name, stanza, want string }{
		{"unknown preset", "api_type: image\n    image_pipeline: stable-diffusion", `unknown image_pipeline preset "stable-diffusion"`},
		{"unknown caption", "api_type: image\n    image_pipeline: {caption: sdxl}", `caption "sdxl" is not valid; use one of ideogram4, z-image, z-anime, krea2, flux2-klein, flux3-klein, qwen-image-2.1, fibo-2.0`},
		{"z-image is no preset", "api_type: image\n    image_pipeline: z-image", `unknown image_pipeline preset "z-image"`},
		{"non-image stanza", "image_pipeline: sd-server", "image_pipeline is only valid with api_type: image"},
		{"negative retry", "api_type: image\n    image_pipeline: {retry_blocked: -1}", "retry_blocked must not be negative"},
		{"high retry", "api_type: image\n    image_pipeline: {retry_blocked: 50}", "retry_blocked must be at most 5"},
		{"empty enhancer", "api_type: image\n    image_pipeline: {enhancer: {timeout_seconds: 5}}", "enhancer needs a model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			y := "stanzas:\n  - model_id: a\n    command: x\n    " + tc.stanza + "\n"
			_, err := Parse([]byte(y))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestDiscoveryDefaultsAndDisable(t *testing.T) {
	cfg, err := Parse([]byte("stanzas: []\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	d := cfg.Discovery
	if !d.On() {
		t.Fatal("discovery must default on")
	}
	if len(d.Ports) != 3 || d.Ports[0] != 18080 || d.Ports[1] != 8081 || d.Ports[2] != 8082 {
		t.Fatalf("ports = %v", d.Ports)
	}
	if d.IntervalSeconds != 30 || d.MaxIntervalSeconds != 600 {
		t.Fatalf("intervals = %d/%d", d.IntervalSeconds, d.MaxIntervalSeconds)
	}

	cfg, err = Parse([]byte("stanzas: []\ndiscovery:\n  enabled: false\n  subnets: [10.99.0.0/24]\n  exclude: [10.99.0.10]\n"))
	if err != nil {
		t.Fatalf("parse disabled: %v", err)
	}
	if cfg.Discovery.On() || len(cfg.Discovery.Networks) != 1 || len(cfg.Discovery.Excluded) != 1 {
		t.Fatalf("disabled discovery = %+v", cfg.Discovery)
	}

	if _, err := Parse([]byte("stanzas: []\ndiscovery: {interval_seconds: 900}\n")); err != nil {
		t.Fatalf("interval above the default max must parse: %v", err)
	}
	cfg, err = Parse([]byte("stanzas: []\ndiscovery: {interval_seconds: 900}\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Discovery.MaxIntervalSeconds != 900 {
		t.Fatalf("max = %d, want the interval 900", cfg.Discovery.MaxIntervalSeconds)
	}
	if _, err := Parse([]byte("stanzas: []\ndiscovery: {interval_seconds: 900, max_interval_seconds: 600}\n")); err == nil {
		t.Fatal("explicit max below interval must fail")
	}
	if _, err := Parse([]byte("stanzas: []\ndiscovery: {subnets: [not-a-cidr]}\n")); err == nil {
		t.Fatal("bad subnet must fail")
	}
	if _, err := Parse([]byte("stanzas: []\ndiscovery: {ports: [70000]}\n")); err == nil {
		t.Fatal("bad port must fail")
	}
}

// catalog.hub is off by default with the public hub bases; an unknown mode fails.
func TestCatalogHubDefaults(t *testing.T) {
	cfg, err := Parse([]byte("stanzas: []\ncatalog: {hf_base: \"http://hub.example.test/\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Catalog
	if c.Hub != HubOff || c.HFBase != "http://hub.example.test" || c.ModelScopeBase != "https://modelscope.cn" || c.HubRefreshSeconds != 86400 {
		t.Errorf("defaults: %+v", c)
	}
	if _, err := Parse([]byte("stanzas: []\ncatalog: {hub: github}\n")); err == nil {
		t.Error("unknown catalog.hub accepted")
	}
}
