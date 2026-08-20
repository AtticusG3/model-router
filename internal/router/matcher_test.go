package router

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"testing"

	"model-router/internal/config"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(`
start_port: 5900
stanzas:
  - model_id: agents-a1
    command: "x --port {port}"
    match:
      body_field: model
  - model_id: krea-2-turbo
    command: "x --port {port}"
    match:
      path_prefix: /sdapi/v1
      path_default: true
  - model_id: krea-2-redcraft
    command: "x --port {port}"
    match:
      path_prefix: /sdapi/v1
  - model_id: embed
    command: "x --port {port}"
pools:
  coding-pool:
    strategy: spillover
    targets: [agents-a1]
peers:
  - name: digger
    kind: router
    base_url: http://192.168.1.36:8082
    models: [coding-model]
`))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return cfg
}

func newReq(method, target, ct, body string) *http.Request {
	req, _ := http.NewRequest(method, target, bytes.NewBufferString(body))
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	return req
}

// multipartReq builds a multipart/form-data request from ordered field pairs.
func multipartReq(t *testing.T, fields ...[2]string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, f := range fields {
		if err := w.WriteField(f[0], f[1]); err != nil {
			t.Fatalf("write field %s: %v", f[0], err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	req, _ := http.NewRequest("POST", "http://x/v1/chat/completions", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	return req
}

func TestMatchBodyField(t *testing.T) {
	cfg := testConfig(t)
	req := newReq("POST", "http://x/v1/chat/completions", "application/json", `{"model":"agents-a1","messages":[]}`)
	ref, err := matchRequest(req, cfg)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if ref.Local != "agents-a1" {
		t.Errorf("local = %q, want agents-a1", ref.Local)
	}
}

func TestMatchPool(t *testing.T) {
	cfg := testConfig(t)
	req := newReq("POST", "http://x/v1/chat/completions", "application/json", `{"model":"coding-pool","messages":[]}`)
	ref, err := matchRequest(req, cfg)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if ref.Pool != "coding-pool" {
		t.Errorf("pool = %q", ref.Pool)
	}
}

func TestMatchPeerQualified(t *testing.T) {
	cfg := testConfig(t)
	req := newReq("POST", "http://x/v1/chat/completions", "application/json", `{"model":"digger/coding-model","messages":[]}`)
	ref, err := matchRequest(req, cfg)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if ref.Peer != "digger" || ref.PeerID != "coding-model" {
		t.Errorf("peer = %q/%q", ref.Peer, ref.PeerID)
	}
}

func TestMatchPeerAdvertisedUnqualified(t *testing.T) {
	cfg := testConfig(t)
	req := newReq("POST", "http://x/v1/chat/completions", "application/json", `{"model":"coding-model","messages":[]}`)
	ref, err := matchRequest(req, cfg)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if ref.Local != "" || ref.Pool != "" || ref.Peer != "" {
		t.Fatalf("want mesh raw-only ref, got %+v", ref)
	}
	if ref.Raw != "coding-model" {
		t.Fatalf("raw = %q", ref.Raw)
	}
}

func TestMatchUnknownModelStillFails(t *testing.T) {
	cfg := testConfig(t)
	req := newReq("POST", "http://x/v1/chat/completions", "application/json", `{"model":"nope","messages":[]}`)
	if _, err := matchRequest(req, cfg); err == nil {
		t.Fatal("unknown model should fail")
	}
}

func TestMatchPathDefault(t *testing.T) {
	cfg := testConfig(t)
	// No model in body -> falls back to path_default stanza.
	req := newReq("POST", "http://x/sdapi/v1/txt2img", "application/json", `{"prompt":"cat"}`)
	ref, err := matchRequest(req, cfg)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if ref.Local != "krea-2-turbo" {
		t.Errorf("path default = %q, want krea-2-turbo", ref.Local)
	}
}

func TestMatchFirstPrefixWins(t *testing.T) {
	cfg, err := config.Parse([]byte(`
stanzas:
  - model_id: sd-first
    command: "x --port {port}"
    match:
      path_prefix: /sdapi/v1
  - model_id: sd-second
    command: "x --port {port}"
    match:
      path_prefix: /sdapi/v1
`))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	req := newReq("POST", "http://x/sdapi/v1/txt2img", "application/json", `{"prompt":"cat"}`)
	ref, err := matchRequest(req, cfg)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if ref.Local != "sd-first" {
		t.Errorf("first prefix = %q, want sd-first", ref.Local)
	}
}

func TestMatchPathDefaultNotFirst(t *testing.T) {
	cfg, err := config.Parse([]byte(`
stanzas:
  - model_id: sd-plain
    command: "x --port {port}"
    match:
      path_prefix: /sdapi/v1
  - model_id: sd-default
    command: "x --port {port}"
    match:
      path_prefix: /sdapi/v1
      path_default: true
`))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	req := newReq("POST", "http://x/sdapi/v1/txt2img", "application/json", `{"prompt":"cat"}`)
	ref, err := matchRequest(req, cfg)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if ref.Local != "sd-default" {
		t.Errorf("path default = %q, want sd-default", ref.Local)
	}
}

func TestMatchPathWithBodyModelWins(t *testing.T) {
	cfg := testConfig(t)
	// Body model wins over path default.
	req := newReq("POST", "http://x/sdapi/v1/txt2img", "application/json", `{"model":"krea-2-redcraft","prompt":"cat"}`)
	ref, err := matchRequest(req, cfg)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if ref.Local != "krea-2-redcraft" {
		t.Errorf("body model = %q, want krea-2-redcraft", ref.Local)
	}
}

func TestMatchNoModel404(t *testing.T) {
	cfg := testConfig(t)
	req := newReq("POST", "http://x/v1/chat/completions", "application/json", `{"messages":[]}`)
	if _, err := matchRequest(req, cfg); err == nil {
		t.Fatal("expected ErrNoModel")
	}
}

func TestMatchAlias(t *testing.T) {
	cfg, err := config.Parse([]byte(`
stanzas:
  - model_id: qwopus-27b-coder
    command: "x --port {port}"
    aliases: [coding-model]
`))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	req := newReq("POST", "http://x/v1/chat/completions", "application/json", `{"model":"coding-model","messages":[]}`)
	ref, err := matchRequest(req, cfg)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if ref.Local != "qwopus-27b-coder" {
		t.Errorf("alias resolved to %q, want qwopus-27b-coder", ref.Local)
	}
}

func TestMatchQuery(t *testing.T) {
	cfg := testConfig(t)
	req := newReq("GET", "http://x/props?model=agents-a1", "", "")
	ref, err := matchRequest(req, cfg)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if ref.Local != "agents-a1" {
		t.Errorf("query model = %q", ref.Local)
	}
}

func TestMatchCustomBodyField(t *testing.T) {
	cfg, err := config.Parse([]byte(`
stanzas:
  - model_id: sdxl-turbo
    command: "x --port {port}"
    match:
      body_field: model_name
`))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	req := newReq("POST", "http://x/v1/chat/completions", "application/json", `{"model_name":"sdxl-turbo","prompt":"cat"}`)
	ref, err := matchRequest(req, cfg)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if ref.Local != "sdxl-turbo" {
		t.Errorf("local = %q, want sdxl-turbo", ref.Local)
	}
}

func TestMatchURLEncodedForm(t *testing.T) {
	cfg := testConfig(t)
	req := newReq("POST", "http://x/v1/chat/completions", "application/x-www-form-urlencoded", "model=agents-a1&n=1")
	ref, err := matchRequest(req, cfg)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if ref.Local != "agents-a1" {
		t.Errorf("local = %q, want agents-a1", ref.Local)
	}
}

func TestMatchMultipartForm(t *testing.T) {
	cfg := testConfig(t)
	req := multipartReq(t, [2]string{"model", "agents-a1"}, [2]string{"prompt", "cat"})
	ref, err := matchRequest(req, cfg)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if ref.Local != "agents-a1" {
		t.Errorf("local = %q, want agents-a1", ref.Local)
	}
}

func TestMatchMultipartFormSkipsOtherParts(t *testing.T) {
	cfg := testConfig(t)
	// The model part comes after a non-model part; the parser must drain and
	// skip it to reach "model".
	req := multipartReq(t, [2]string{"image", "iVBORw0KGgo="}, [2]string{"model", "agents-a1"})
	ref, err := matchRequest(req, cfg)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if ref.Local != "agents-a1" {
		t.Errorf("local = %q, want agents-a1", ref.Local)
	}
}

func TestMatchFormFallsBackToQuery(t *testing.T) {
	cfg := testConfig(t)
	// Form body has no "model" field; the query string supplies it.
	req := newReq("POST", "http://x/v1/chat/completions?model=agents-a1", "application/x-www-form-urlencoded", "prompt=cat")
	ref, err := matchRequest(req, cfg)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if ref.Local != "agents-a1" {
		t.Errorf("local = %q, want agents-a1", ref.Local)
	}
}
