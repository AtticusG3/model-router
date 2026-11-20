package router

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"model-router/internal/config"
)

// A model-less POST to an /v1 audio path is served by the only stanza of
// that api_type, and the backend receives the original request.
func TestModelLessAudioRouteServedBySingleStanza(t *testing.T) {
	var gotPath string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		gotPath = req.URL.Path
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("audio-bytes"))
	}))
	defer backend.Close()
	port := backend.Listener.Addr().(*net.TCPAddr).Port

	cfg, err := config.Parse([]byte(fmt.Sprintf(`
start_port: 5900
stanzas:
  - model_id: speech
    command: "fake --port {port}"
    vram_mb: 0
    device: cpu
    api_type: speech
    health_check: /health
    spin_up_seconds: 5
    port: %d
`, port)))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	r := New(cfg, NewLogger(io.Discard, false), "test")
	r.spawn = func(string, []string) (Proc, error) { return newFakeProc(), nil }
	t.Cleanup(func() { _ = r.Unload("speech") })
	h := NewHandler(r, NewLogger(io.Discard, false))

	req := httptest.NewRequest("POST", "/v1/audio/speech", strings.NewReader(`{"input":"hi","voice":"alloy"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "audio-bytes" {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/audio/speech" {
		t.Fatalf("backend path = %q", gotPath)
	}
}

// Two speech stanzas and no path_default: the client must name one, so the
// router answers 400 naming both candidates instead of guessing.
func TestModelLessAudioRouteAmbiguousIs400(t *testing.T) {
	r := spilloverRouter(t, `
start_port: 5900
stanzas:
  - model_id: speech-a
    command: "x --port {port}"
    vram_mb: 0
    device: cpu
    api_type: speech
  - model_id: speech-b
    command: "x --port {port}"
    vram_mb: 0
    device: cpu
    api_type: speech
`)
	h := NewHandler(r, NewLogger(io.Discard, false))
	req := httptest.NewRequest("POST", "/v1/audio/speech", strings.NewReader(`{"input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "speech-a") || !strings.Contains(body, "speech-b") {
		t.Fatalf("400 must name the candidates: %q", body)
	}
}

// With a path_default among the candidates, the model-less request resolves to
// it (and an explicit path_prefix still wins over the type default).
func TestModelLessRoutePrefersPathDefault(t *testing.T) {
	cfg, err := config.Parse([]byte(`
start_port: 5900
stanzas:
  - model_id: speech-a
    command: "x --port {port}"
    vram_mb: 0
    device: cpu
    api_type: speech
  - model_id: speech-b
    command: "x --port {port}"
    vram_mb: 0
    device: cpu
    api_type: speech
    match: {path_default: true}
`))
	if err != nil {
		t.Fatal(err)
	}
	ref, err := matchRequest(newReq("POST", "http://x/v1/audio/speech", "application/json", `{"input":"hi"}`), cfg)
	if err != nil || ref.Local != "speech-b" {
		t.Fatalf("ref = %+v err = %v, want speech-b", ref, err)
	}
}

// A path that maps to no api_type, or to a type this node has no stanza for,
// still answers the usual 404.
func TestModelLessRouteUnknownStays404(t *testing.T) {
	cfg, err := config.Parse([]byte(`
start_port: 5900
stanzas:
  - model_id: speech
    command: "x --port {port}"
    vram_mb: 0
    device: cpu
    api_type: speech
`))
	if err != nil {
		t.Fatal(err)
	}
	// No video stanza here.
	if _, err := matchRequest(newReq("POST", "http://x/v1/videos", "application/json", `{}`), cfg); err == nil {
		t.Fatal("a path with no stanza of its type must fail")
	}
	// chat is not in the path table.
	if _, err := matchRequest(newReq("POST", "http://x/v1/chat/completions", "application/json", `{}`), cfg); err == nil {
		t.Fatal("chat must stay model-routed")
	}
}
