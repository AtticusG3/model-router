package router

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"model-router/internal/config"
)

// filterRouter runs a stanza with the given filters YAML against a fake
// backend. send posts a body and returns the body the backend received.
func filterRouter(t *testing.T, filters string) (r *Router, send func(path, ct, body string) string) {
	t.Helper()
	var got string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		got = string(b)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(backend.Close)
	port := backend.Listener.Addr().(*net.TCPAddr).Port
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
stanzas:
  - model_id: chat-a
    command: "fake --port {port}"
    vram_mb: 0
    health_check: /
    spin_up_seconds: 5
    port: %d
    filters:
%s
`, port, filters)))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	r = New(cfg, NewLogger(io.Discard, false), "test")
	r.spawn = func(string, []string) (Proc, error) { return newFakeProc(), nil }
	r.ledger.SetGPUs([]*GPUState{gpu(0, 10000)})
	t.Cleanup(func() { _ = r.Unload("chat-a") })
	h := NewHandler(r, NewLogger(io.Discard, false))
	return r, func(path, ct, body string) string {
		req := httptest.NewRequest("POST", path, bytes.NewBufferString(body))
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		return got
	}
}

func TestFiltersStripAndSet(t *testing.T) {
	_, send := filterRouter(t, `
      stripParams: [temperature]
      setParams:
        top_p: 0.5
        seed?: 7
        top_k?: 40`)
	got := send("/v1/chat/completions", "application/json",
		`{"model":"chat-a","temperature":1.5,"top_p":0.9,"top_k":10,"big":12345678901234567890}`)
	if strings.Contains(got, "temperature") {
		t.Errorf("temperature not stripped: %s", got)
	}
	for _, want := range []string{`"top_p":0.5`, `"top_k":10`, `"seed":7`, `"model":"chat-a"`, `"big":12345678901234567890`} {
		if !strings.Contains(got, want) {
			t.Errorf("body %s missing %s", got, want)
		}
	}
}

func TestFiltersUrlencoded(t *testing.T) {
	_, send := filterRouter(t, `
      stripParams: [temperature]
      setParams: {top_p: 0.5, "language?": en}`)
	got := send("/v1/audio/transcriptions", "application/x-www-form-urlencoded", "model=chat-a&temperature=1&language=fr")
	if got != "language=fr&model=chat-a&top_p=0.5" {
		t.Errorf("form body = %q", got)
	}
	// A body that is neither JSON nor a form is forwarded as sent.
	req := httptest.NewRequest("POST", "/x", strings.NewReader("RIFF"))
	req.Header.Set("Content-Type", "audio/wav")
	if err := applyFilters(req, []string{"a"}, map[string]any{"b": 1}); err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(req.Body); string(b) != "RIFF" {
		t.Errorf("raw body became %q", b)
	}
}

// clipBytes is a fictional upload that contains CRLFs and dashes, so a part
// that was re-encoded rather than copied would show.
var clipBytes = bytes.Repeat([]byte("\r\n--RIFF\x00\xff fictional audio\r\n"), 8000)

// transcriptionBody is a multipart form: the given fields, then a file part.
func transcriptionBody(t *testing.T, fields ...[2]string) string {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.SetBoundary("fictionalboundary"); err != nil {
		t.Fatal(err)
	}
	for _, f := range fields {
		if err := w.WriteField(f[0], f[1]); err != nil {
			t.Fatal(err)
		}
	}
	fw, err := w.CreateFormFile("file", "clip.wav")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write(clipBytes)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

const transcriptionCT = "multipart/form-data; boundary=fictionalboundary"

// formFields parses a multipart body sent with transcriptionCT into its field
// values; it fails the test if the file part is not byte-identical.
func formFields(t *testing.T, body string) map[string]string {
	t.Helper()
	got := map[string]string{}
	mr := multipart.NewReader(strings.NewReader(body), "fictionalboundary")
	for {
		p, err := mr.NextRawPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("parse multipart: %v (body %.200q)", err, body)
		}
		b, _ := io.ReadAll(p)
		if p.FileName() != "" {
			if !bytes.Equal(b, clipBytes) {
				t.Errorf("file part changed: %d bytes, want %d", len(b), len(clipBytes))
			}
			continue
		}
		got[p.FormName()] = string(b)
	}
	return got
}

func TestFiltersMultipart(t *testing.T) {
	_, send := filterRouter(t, `
      stripParams: [prompt]
      setParams: {"response_format?": verbose_json, temperature: 0}`)
	const p = "/v1/audio/transcriptions"
	got := formFields(t, send(p, transcriptionCT, transcriptionBody(t, [2]string{"model", "chat-a"}, [2]string{"prompt", "x"})))
	want := map[string]string{"model": "chat-a", "response_format": "verbose_json", "temperature": "0"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("fields = %v, want %v", got, want)
	}
	got = formFields(t, send(p, transcriptionCT, transcriptionBody(t,
		[2]string{"model", "chat-a"}, [2]string{"response_format", "text"}, [2]string{"temperature", "1"})))
	want = map[string]string{"model": "chat-a", "response_format": "text", "temperature": "0"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("fields = %v, want %v", got, want)
	}
}

func TestFilterFormPrecedence(t *testing.T) {
	e, err := newFormEdit([]string{"a"}, map[string]any{"a?": "x", "b": true}, map[string]any{"b?": "y"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		key     string
		present bool
		want    string
		ok      bool
	}{
		{"a", false, "x", true},   // stripped, so the "?" value fills it
		{"b", true, "true", true}, // a forced value beats the client
		{"b", false, "y", true},   // a later "?" wins when the client omitted it
	} {
		if v, ok := e.set[tc.key].value(tc.present); v != tc.want || ok != tc.ok {
			t.Errorf("%s present=%v: got %q %v, want %q %v", tc.key, tc.present, v, ok, tc.want, tc.ok)
		}
	}
}

// The JSON path agrees with the form path: a later "?" set fills a field the
// client omitted even after an earlier bare set of the same key.
func TestFilterJSONPrecedenceMatchesForm(t *testing.T) {
	req := httptest.NewRequest("POST", "/x", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	if err := applyFilters(req, nil, map[string]any{"b": true}, map[string]any{"b?": "y"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(req.Body); string(got) != `{"b":"y"}` {
		t.Errorf("body = %s, want {\"b\":\"y\"}", got)
	}
}

// A body that is not declared as JSON or a form (here JSON with no
// Content-Type, or as text/plain) is forwarded byte for byte, not re-encoded.
func TestApplyFiltersLeavesUndeclaredBodyAlone(t *testing.T) {
	const body = `{"model":"chat-a","prompt":"fictional"}`
	for _, ct := range []string{"", "text/plain"} {
		req := httptest.NewRequest("POST", "/x", bytes.NewBufferString(body))
		req.Header.Del("Content-Type")
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		if err := applyFilters(req, []string{"prompt"}, map[string]any{"temperature": 0}); err != nil {
			t.Fatal(err)
		}
		if got, _ := io.ReadAll(req.Body); string(got) != body {
			t.Errorf("content type %q: body = %s, want %s", ct, got, body)
		}
	}
}

func TestApplyFiltersIgnoresNonObjectJSON(t *testing.T) {
	for _, body := range []string{`[1,2]`, `null`, `not json`} {
		req := httptest.NewRequest("POST", "/x", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		if err := applyFilters(req, []string{"a"}, map[string]any{"b": 1}); err != nil {
			t.Fatal(err)
		}
		if got, _ := io.ReadAll(req.Body); string(got) != body {
			t.Errorf("body %q became %q", body, got)
		}
	}
}

func TestFiltersAliasRoutesToSameStanza(t *testing.T) {
	r, send := filterRouter(t, `
      setParams: {temperature: 0.7}
      setParamsByID:
        "${MODEL_ID}:high":
          temperature: 0.1
          reasoning_effort: high`)
	const p = "/v1/chat/completions"
	got := send(p, "application/json", `{"model":"chat-a:high"}`)
	if !strings.Contains(got, `"temperature":0.1`) || !strings.Contains(got, `"reasoning_effort":"high"`) {
		t.Errorf("alias params not applied: %s", got)
	}
	got = send(p, "application/json", `{"model":"chat-a"}`)
	if !strings.Contains(got, `"temperature":0.7`) || strings.Contains(got, "reasoning_effort") {
		t.Errorf("plain id picked up alias params: %s", got)
	}
	var ids []string
	for _, ms := range r.AllModelStatuses() {
		ids = append(ids, ms.ID)
	}
	if strings.Join(ids, ",") != "chat-a,chat-a:high" {
		t.Errorf("catalog ids = %v, want chat-a and chat-a:high", ids)
	}
}

func TestFiltersPeer(t *testing.T) {
	var got string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		if req.URL.Path != "/_router/load" {
			got = string(b)
		}
		_, _ = w.Write([]byte(`{"status":"loaded"}`))
	}))
	defer peer.Close()
	cfg, err := config.Parse([]byte(`
peers:
  - name: far
    base_url: ` + peer.URL + `
    models: [remote-model]
    filters:
      stripParams: [temperature]
`))
	if err != nil {
		t.Fatal(err)
	}
	r := New(cfg, NewLogger(io.Discard, false), "test")
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(`{"model":"far/remote-model","temperature":1}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	NewHandler(r, NewLogger(io.Discard, false)).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || strings.Contains(got, "temperature") || !strings.Contains(got, `"model":"remote-model"`) {
		t.Errorf("status=%d peer body=%s", rec.Code, got)
	}
}

// A multipart body to a peer gets the peer's id and the peer's filters in the
// same single re-stream.
func TestFiltersPeerMultipart(t *testing.T) {
	var got string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		if req.URL.Path != "/_router/load" {
			got = string(b)
		}
		_, _ = w.Write([]byte(`{"text":"fictional"}`))
	}))
	defer peer.Close()
	cfg, err := config.Parse([]byte(`
peers:
  - name: far
    base_url: ` + peer.URL + `
    models: [whisper-x]
    filters:
      stripParams: [prompt]
      setParams: {"response_format?": verbose_json}
`))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/v1/audio/transcriptions", strings.NewReader(transcriptionBody(t,
		[2]string{"model", "far/whisper-x"}, [2]string{"prompt", "x"})))
	req.Header.Set("Content-Type", transcriptionCT)
	rec := httptest.NewRecorder()
	NewHandler(New(cfg, NewLogger(io.Discard, false), "test"), NewLogger(io.Discard, false)).ServeHTTP(rec, req)
	want := map[string]string{"model": "whisper-x", "response_format": "verbose_json"}
	if f := formFields(t, got); rec.Code != http.StatusOK || fmt.Sprint(f) != fmt.Sprint(want) {
		t.Errorf("status=%d peer fields=%v, want %v", rec.Code, f, want)
	}
}
