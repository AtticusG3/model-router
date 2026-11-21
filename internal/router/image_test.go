package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"model-router/internal/config"
)

// imageTestRouter starts backend and routes a bare sd-server stanza at it: no
// shim, sd-server listening directly, health checked on /v1/models.
func imageTestRouter(t *testing.T, backend http.Handler) *Router {
	return imageTestRouterPipeline(t, backend, "sd-server")
}

func imageTestRouterPipeline(t *testing.T, backend http.Handler, pipeline string) *Router {
	t.Helper()
	srv := httptest.NewServer(backend)
	t.Cleanup(srv.Close)
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
start_port: 5900
stanzas:
  - model_id: sd-bare
    command: "sd-server --listen-ip 127.0.0.1 --listen-port {port}"
    vram_mb: 0
    api_type: image
    health_check: /v1/models
    image_pipeline: %s
    match: {path_prefix: /sdapi/v1, path_default: true}
    spin_up_seconds: 5
    port: %d
`, pipeline, port)))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	r := New(cfg, NewLogger(io.Discard, false), "node-a")
	r.spawn = func(string, []string) (Proc, error) { return newFakeProc(), nil }
	r.ledger.SetGPUs([]*GPUState{gpu(0, 10000)})
	t.Cleanup(func() { _ = r.Unload("sd-bare") })
	return r
}

func imageBackend(t *testing.T, txt2img http.HandlerFunc) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"list","data":[]}`)
	})
	mux.HandleFunc("/sdapi/v1/txt2img", txt2img)
	return mux
}

// imageBackendWithLoras is imageBackend plus the sd-server LoRA endpoint. warm
// runs on every GET /sdapi/v1/loras.
func imageBackendWithLoras(t *testing.T, warm func(), txt2img http.HandlerFunc) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"list","data":[]}`)
	})
	mux.HandleFunc("/sdapi/v1/loras", func(w http.ResponseWriter, req *http.Request) {
		warm()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[]`)
	})
	mux.HandleFunc("/sdapi/v1/txt2img", txt2img)
	return mux
}

func TestImagePipelineWarmsLoRABeforeFirstRequest(t *testing.T) {
	var mu sync.Mutex
	warmed := false
	warmCalls, rejected, generated := 0, 0, 0
	r := imageTestRouter(t, imageBackendWithLoras(t, func() {
		mu.Lock()
		warmed = true
		warmCalls++
		mu.Unlock()
	}, func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			LoRA []string `json:"lora"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		mu.Lock()
		isWarm := warmed
		namesLora := len(body.LoRA) > 0
		if namesLora && !isWarm {
			rejected++
		} else {
			generated++
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if namesLora && !isWarm {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"invalid lora path"}`)
			return
		}
		fmt.Fprint(w, `{"images":["ok"]}`)
	}))

	if _, err := r.Load("sd-bare"); err != nil {
		t.Fatalf("Load: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/sdapi/v1/txt2img", strings.NewReader(`{"prompt":"cat","lora":["QuadView_identity.safetensors"]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	NewHandler(r, NewLogger(io.Discard, false)).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if warmCalls != 1 {
		t.Fatalf("warm calls = %d, want 1", warmCalls)
	}
	if rejected != 0 {
		t.Fatalf("LoRA requests rejected = %d, want 0", rejected)
	}
	if generated != 1 {
		t.Fatalf("generated = %d, want 1", generated)
	}
}

func TestImagePipelineRetriesInvalidLoRAOnce(t *testing.T) {
	var mu sync.Mutex
	warmCalls, attempts := 0, 0
	r := imageTestRouter(t, imageBackendWithLoras(t, func() {
		mu.Lock()
		warmCalls++
		mu.Unlock()
	}, func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			LoRA []string `json:"lora"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if len(body.LoRA) > 0 && n == 1 {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"invalid lora path"}`)
			return
		}
		fmt.Fprint(w, `{"images":["ok"]}`)
	}))

	if _, err := r.Load("sd-bare"); err != nil {
		t.Fatalf("Load: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/sdapi/v1/txt2img", strings.NewReader(`{"prompt":"cat","lora":["QuadView_identity.safetensors"]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	NewHandler(r, NewLogger(io.Discard, false)).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"images"`) {
		t.Fatalf("response = %d %s, want the retried success", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != 2 {
		t.Fatalf("attempts = %d, want one retry", attempts)
	}
	if warmCalls != 2 {
		t.Fatalf("warm calls = %d, want the ready warm and the forced re-warm", warmCalls)
	}
}

func TestImagePipelineInvalidLoRASecondFailureIsReturned(t *testing.T) {
	var mu sync.Mutex
	warmCalls, attempts := 0, 0
	r := imageTestRouter(t, imageBackendWithLoras(t, func() {
		mu.Lock()
		warmCalls++
		mu.Unlock()
	}, func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			LoRA []string `json:"lora"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		mu.Lock()
		attempts++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if len(body.LoRA) > 0 {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"invalid lora path"}`)
			return
		}
		fmt.Fprint(w, `{"images":["ok"]}`)
	}))

	if _, err := r.Load("sd-bare"); err != nil {
		t.Fatalf("Load: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/sdapi/v1/txt2img", strings.NewReader(`{"prompt":"cat","lora":["QuadView_identity.safetensors"]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	NewHandler(r, NewLogger(io.Discard, false)).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid lora path") {
		t.Fatalf("response = %d %s, want the second failure as is", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != 2 {
		t.Fatalf("attempts = %d, want two and no loop", attempts)
	}
	if warmCalls != 2 {
		t.Fatalf("warm calls = %d, want the ready warm and the forced re-warm", warmCalls)
	}
}

func TestImagePipelineNoLoraDoesNotWarmAfterReady(t *testing.T) {
	warmCh := make(chan struct{}, 8)
	r := imageTestRouter(t, imageBackendWithLoras(t, func() {
		warmCh <- struct{}{}
	}, func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"images":["ok"]}`)
	}))

	if _, err := r.Load("sd-bare"); err != nil {
		t.Fatalf("Load: %v", err)
	}
	select {
	case <-warmCh:
	case <-time.After(5 * time.Second):
		t.Fatal("ready warm not called")
	}
	for i := range 2 {
		req := httptest.NewRequest(http.MethodPost, "/sdapi/v1/txt2img", strings.NewReader(`{"prompt":"cat"}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		NewHandler(r, NewLogger(io.Discard, false)).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d status = %d", i, rec.Code)
		}
	}
	select {
	case <-warmCh:
		t.Fatal("a request without lora triggered another warm")
	default:
	}
}

func TestImagePipelineWarmResetsOnRestart(t *testing.T) {
	warmCh := make(chan struct{}, 8)
	r := imageTestRouter(t, imageBackendWithLoras(t, func() {
		warmCh <- struct{}{}
	}, func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"images":["ok"]}`)
	}))

	waitWarm := func() {
		select {
		case <-warmCh:
		case <-time.After(5 * time.Second):
			t.Fatal("ready warm not called")
		}
	}
	if _, err := r.Load("sd-bare"); err != nil {
		t.Fatalf("Load: %v", err)
	}
	waitWarm()
	if err := r.Unload("sd-bare"); err != nil {
		t.Fatalf("Unload: %v", err)
	}
	if _, err := r.Load("sd-bare"); err != nil {
		t.Fatalf("Load again: %v", err)
	}
	waitWarm()
}

func TestImagePipelineBareSdServer(t *testing.T) {
	var gotBody []byte
	var gotPath string
	r := imageTestRouter(t, imageBackend(t, func(w http.ResponseWriter, req *http.Request) {
		gotPath = req.URL.Path
		gotBody, _ = io.ReadAll(req.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"images":["iVBORw0KGgo="]}`)
	}))

	if _, err := r.Load("sd-bare"); err != nil {
		t.Fatalf("Load with health_check /v1/models: %v", err)
	}

	body := `{"prompt":"cat"}`
	req := httptest.NewRequest(http.MethodPost, "/sdapi/v1/txt2img", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	NewHandler(r, NewLogger(io.Discard, false)).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/sdapi/v1/txt2img" {
		t.Fatalf("backend path = %q", gotPath)
	}
	if string(gotBody) != body {
		t.Fatalf("backend body = %q, want the client's body unchanged", gotBody)
	}
	if !strings.Contains(rec.Body.String(), `"images"`) {
		t.Fatalf("response = %s", rec.Body.String())
	}
}

func TestImagePipelinePeerRequestAndNotes(t *testing.T) {
	var seenPeer, gotPrompt string
	r := imageTestRouter(t, imageBackend(t, func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Prompt string `json:"prompt"`
		}
		_ = json.NewDecoder(req.Body).Decode(&in)
		gotPrompt = in.Prompt
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"images":["ok"]}`)
	}))
	r.imageSteps = func(*config.Stanza) []imageStep {
		return []imageStep{{
			Rewrite: func(req *http.Request, _ http.Header, notes *imageNotes) error {
				seenPeer = req.Header.Get("X-Model-Router-Peer")
				var body map[string]any
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					return err
				}
				body["prompt"] = "rewritten"
				out, _ := json.Marshal(body)
				req.Body = io.NopCloser(bytes.NewReader(out))
				req.ContentLength = int64(len(out))
				notes.Add("prompt rewritten")
				return nil
			},
		}}
	}

	body := `{"prompt":"cat"}`
	req := httptest.NewRequest(http.MethodPost, "/sdapi/v1/txt2img", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Model-Router-Peer", "node-b")
	rec := httptest.NewRecorder()
	NewHandler(r, NewLogger(io.Discard, false)).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if seenPeer != "node-b" {
		t.Fatalf("step saw peer %q, want node-b", seenPeer)
	}
	if gotPrompt != "rewritten" {
		t.Fatalf("backend prompt = %q, want the step's rewrite", gotPrompt)
	}

	entries := r.Activity().List()
	if len(entries) == 0 {
		t.Fatal("no activity entry")
	}
	cap, ok := r.Activity().Capture(entries[len(entries)-1].ID)
	if !ok {
		t.Fatal("no activity capture")
	}
	if !strings.Contains(cap.ReqBody, `"prompt":"cat"`) {
		t.Fatalf("capture body = %q, want the client's original", cap.ReqBody)
	}
	if len(cap.Notes) != 1 || cap.Notes[0] != "prompt rewritten" {
		t.Fatalf("capture notes = %v", cap.Notes)
	}
}

func TestImagePipelineResponseStepRetriesOnce(t *testing.T) {
	attempts := 0
	r := imageTestRouter(t, imageBackend(t, func(w http.ResponseWriter, req *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "application/json")
		if attempts == 1 {
			fmt.Fprint(w, `{"blocked":true}`)
			return
		}
		fmt.Fprint(w, `{"images":["ok"]}`)
	}))
	r.imageSteps = func(*config.Stanza) []imageStep {
		return []imageStep{{
			After: func(req *http.Request, status int, h http.Header, body []byte, notes *imageNotes) bool {
				blocked := bytes.Contains(body, []byte(`"blocked":true`))
				if blocked {
					h.Set("X-Image-Retried", "1")
				}
				h.Set("X-Image-Pipeline", "checked")
				return blocked
			},
		}}
	}

	req := httptest.NewRequest(http.MethodPost, "/sdapi/v1/txt2img", strings.NewReader(`{"prompt":"cat"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	NewHandler(r, NewLogger(io.Discard, false)).ServeHTTP(rec, req)

	if attempts != 2 {
		t.Fatalf("backend attempts = %d, want one retry", attempts)
	}
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"images"`) {
		t.Fatalf("response = %d %s, want the retried body", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Image-Pipeline"); got != "checked" {
		t.Fatalf("response header = %q, want checked", got)
	}
	if got := rec.Header().Get("X-Image-Retried"); got != "1" {
		t.Fatalf("first-pass header = %q, want it carried past the retry", got)
	}
	if entries := r.Activity().List(); len(entries) != 1 {
		t.Fatalf("activity entries = %d, want one for the client request", len(entries))
	}
	entries := r.Activity().List()
	cap, ok := r.Activity().Capture(entries[0].ID)
	if !ok {
		t.Fatal("no activity capture")
	}
	if strings.Contains(cap.RespBody, `"blocked"`) || !strings.Contains(cap.RespBody, `"images"`) {
		t.Fatalf("captured reply = %q, want the retried reply", cap.RespBody)
	}
	if got := r.metrics.Inflight(); got != 0 {
		t.Fatalf("inflight after the exchange = %d, want 0", got)
	}
}

func TestImagePipelineCountsChunkedUploadBytes(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), maxCaptureBody+4096)
	req := httptest.NewRequest(http.MethodPost, "/sdapi/v1/img2img", strings.NewReader(string(payload)))
	req.ContentLength = -1
	ob, err := captureOriginalImageBody(req)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if _, err := io.Copy(io.Discard, req.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	if ob.bytes != int64(len(payload)) {
		t.Fatalf("bytes = %d, want all %d chunked upload bytes", ob.bytes, len(payload))
	}
	if !ob.truncated || len(ob.body) != maxCaptureBody {
		t.Fatalf("capture = truncated %v, %d bytes; want truncated at %d", ob.truncated, len(ob.body), maxCaptureBody)
	}
}

func TestImagePipelineEarlyReplyMarksCapturePartial(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 8*maxCaptureBody)
	r := imageTestRouter(t, imageBackend(t, func(w http.ResponseWriter, req *http.Request) {
		// Answer without reading the upload, so the body never reaches its end.
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"images":["ok"]}`)
	}))

	req := httptest.NewRequest(http.MethodPost, "/sdapi/v1/img2img", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(payload))
	rec := httptest.NewRecorder()
	NewHandler(r, NewLogger(io.Discard, false)).ServeHTTP(rec, req)

	entries := r.Activity().List()
	if len(entries) != 1 {
		t.Fatalf("activity entries = %d, want one", len(entries))
	}
	cap, ok := r.Activity().Capture(entries[0].ID)
	if !ok {
		t.Fatal("no activity capture")
	}
	if !cap.Truncated {
		t.Fatal("capture not marked partial after an early backend reply")
	}
}
