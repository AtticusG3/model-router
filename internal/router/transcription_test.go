package router

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"model-router/internal/config"
)

// transcriptionBackendTypes is what the documented backend sends for each
// response_format: whisper-server calls text html and srt x-subrip.
var transcriptionBackendTypes = map[string]string{
	"json":         "application/json",
	"verbose_json": "application/json",
	"text":         "text/html",
	"srt":          "application/x-subrip",
	"vtt":          "text/vtt",
}

// transcriptionTypes is the Content-Type the client must get for each
// response_format: a text transcript is text/plain, whatever the backend sent.
var transcriptionTypes = map[string]string{
	"json":         "application/json",
	"verbose_json": "application/json",
	"text":         "text/plain; charset=utf-8",
	"srt":          "text/plain; charset=utf-8",
	"vtt":          "text/plain; charset=utf-8",
}

// transcriptionRouter fronts backendURL with one local transcription stanza,
// whisper-x. extra is appended to the stanza (indent it four spaces).
func transcriptionRouter(t *testing.T, backendURL, extra string) (*Router, http.Handler) {
	t.Helper()
	u, err := url.Parse(backendURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
start_port: 5900
admit_timeout_seconds: 2
stanzas:
  - model_id: whisper-x
    command: "fake --port {port}"
    vram_mb: 0
    device: cpu
    api_type: transcription
    health_check: /health
    spin_up_seconds: 5
    port: %s
%s`, u.Port(), extra)))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	r := New(cfg, NewLogger(io.Discard, false), "test")
	r.spawn = func(string, []string) (Proc, error) { return newFakeProc(), nil }
	t.Cleanup(func() { _ = r.Unload("whisper-x") })
	return r, NewHandler(r, NewLogger(io.Discard, false))
}

func postTranscription(h http.Handler, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", transcriptionCT)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Every response_format comes back with the client-facing Content-Type (the
// three text formats as text/plain, whatever type the backend sent), the file
// and timestamp_granularities[] reach the backend as sent, and a request with
// no model is served by the only transcription stanza.
func TestTranscriptionFormatsAndModelLess(t *testing.T) {
	var mu sync.Mutex
	var got string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/health" {
			return
		}
		b, _ := io.ReadAll(req.Body)
		mu.Lock()
		got = string(b)
		mu.Unlock()
		req.Body = io.NopCloser(bytes.NewReader(b))
		format := req.FormValue("response_format")
		if format == "" {
			format = "json"
		}
		w.Header().Set("Content-Type", transcriptionBackendTypes[format])
		fmt.Fprintf(w, "fictional %s", format)
	}))
	defer backend.Close()
	_, h := transcriptionRouter(t, backend.URL, "")

	for format, ct := range transcriptionTypes {
		t.Run(format, func(t *testing.T) {
			rec := postTranscription(h, "/v1/audio/transcriptions", transcriptionBody(t,
				[2]string{"model", "whisper-x"}, [2]string{"response_format", format},
				[2]string{"timestamp_granularities[]", "word"}, [2]string{"timestamp_granularities[]", "segment"}))
			if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != ct || rec.Body.String() != "fictional "+format {
				t.Fatalf("status %d, type %q, body %q; want 200, %q", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String(), ct)
			}
			mu.Lock()
			body := got
			mu.Unlock()
			if f := formFields(t, body); f["response_format"] != format {
				t.Errorf("backend fields = %v", f)
			}
			if n := strings.Count(body, `name="timestamp_granularities[]"`); n != 2 {
				t.Errorf("backend got %d timestamp_granularities[] parts, want 2", n)
			}
		})
	}

	t.Run("model-less", func(t *testing.T) {
		rec := postTranscription(h, "/v1/audio/transcriptions", transcriptionBody(t, [2]string{"language", "ja"}))
		if rec.Code != http.StatusOK || rec.Body.String() != "fictional json" {
			t.Fatalf("status %d, body %q", rec.Code, rec.Body.String())
		}
		mu.Lock()
		body := got
		mu.Unlock()
		if f := formFields(t, body); f["language"] != "ja" || f["model"] != "" {
			t.Errorf("backend fields = %v", f)
		}
	})
}

// path_rewrite sends the backend its own path, keeps the client's path on the
// activity row, and leaves a path with no matching from alone.
func TestTranscriptionPathRewrite(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/health" {
			return
		}
		_, _ = io.Copy(io.Discard, req.Body)
		mu.Lock()
		paths = append(paths, req.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"fictional"}`))
	}))
	defer backend.Close()
	r, h := transcriptionRouter(t, backend.URL, `    path_rewrite:
      - {from: /v1/audio/transcriptions, to: /inference}
`)
	body := transcriptionBody(t, [2]string{"model", "whisper-x"})
	if rec := postTranscription(h, "/v1/audio/transcriptions", body); rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %q", rec.Code, rec.Body.String())
	}
	if e := onlyEntry(t, r); e.Path != "/v1/audio/transcriptions" {
		t.Errorf("activity path = %q, want the client's", e.Path)
	}
	if rec := postTranscription(h, "/v1/audio/translations", body); rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %q", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(paths) != "[/inference /v1/audio/translations]" {
		t.Errorf("backend paths = %v", paths)
	}
}

// slots: 1 lets one upload reach the backend at a time; the others wait in
// the router, unread, and are all served.
func TestTranscriptionSlotsSerialiseUploads(t *testing.T) {
	var inFlight, peak atomic.Int32
	arrived := make(chan struct{}, 3)
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/health" {
			return
		}
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		arrived <- struct{}{}
		<-release
		_, _ = io.Copy(io.Discard, req.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"fictional"}`))
	}))
	defer backend.Close()
	defer close(release) // runs first, so a failed test does not leave handlers blocked
	_, h := transcriptionRouter(t, backend.URL, "    slots: 1\n")
	body := transcriptionBody(t, [2]string{"model", "whisper-x"})

	codes := make(chan int, 3)
	send := func() { codes <- postTranscription(h, "/v1/audio/transcriptions", body).Code }
	go send()
	<-arrived // the first holds the only slot (and has loaded the model)
	go send()
	go send()
	time.Sleep(300 * time.Millisecond)
	if n := len(arrived); n != 0 {
		t.Fatalf("%d more upload(s) reached a backend whose only slot is busy", n)
	}
	for i := 0; i < 3; i++ {
		if i > 0 {
			select {
			case <-arrived:
			case <-time.After(5 * time.Second):
				t.Fatalf("upload %d never reached the backend", i+1)
			}
		}
		release <- struct{}{}
	}
	for i := 0; i < 3; i++ {
		if c := <-codes; c != http.StatusOK {
			t.Errorf("status = %d, want 200", c)
		}
	}
	if p := peak.Load(); p != 1 {
		t.Errorf("backend saw %d uploads at once, want 1", p)
	}
}

// An upload that waits for a busy slot past admit_timeout_seconds gets 503
// with Retry-After, not before.
func TestTranscriptionSlotWaitEndsAtAdmitTimeout(t *testing.T) {
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/health" {
			return
		}
		arrived <- struct{}{}
		<-release
		_, _ = io.Copy(io.Discard, req.Body)
	}))
	defer backend.Close()
	_, h := transcriptionRouter(t, backend.URL, "    slots: 1\n")
	body := transcriptionBody(t, [2]string{"model", "whisper-x"})

	done := make(chan int, 1)
	go func() { done <- postTranscription(h, "/v1/audio/transcriptions", body).Code }()
	<-arrived
	start := time.Now()
	rec := postTranscription(h, "/v1/audio/transcriptions", body)
	waited := time.Since(start)
	close(release)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("status %d, Retry-After %q; want 503 with Retry-After", rec.Code, rec.Header().Get("Retry-After"))
	}
	if waited < 1900*time.Millisecond {
		t.Errorf("gave up after %v, before admit_timeout_seconds", waited)
	}
	if c := <-done; c != http.StatusOK {
		t.Errorf("first upload status = %d, want 200", c)
	}
}

// request_timeout_seconds cuts a backend that runs past it with 504.
func TestTranscriptionRequestTimeout(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/health" {
			return
		}
		// Read the upload first: the server only notices the router hang up
		// once the body is drained.
		_, _ = io.Copy(io.Discard, req.Body)
		select {
		case <-req.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer backend.Close()
	_, h := transcriptionRouter(t, backend.URL, "    request_timeout_seconds: 1\n")
	start := time.Now()
	rec := postTranscription(h, "/v1/audio/transcriptions", transcriptionBody(t, [2]string{"model", "whisper-x"}))
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status %d, body %q; want 504", rec.Code, rec.Body.String())
	}
	if d := time.Since(start); d < 900*time.Millisecond || d > 5*time.Second {
		t.Errorf("cut after %v, want about 1 s", d)
	}
}
