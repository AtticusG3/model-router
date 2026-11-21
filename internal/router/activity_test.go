package router

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	_ "image/jpeg"
	"image/png"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"model-router/internal/config"
)

func TestParseTokenStatsJSON(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":26,"completion_tokens":128,"prompt_tokens_details":{"cached_tokens":10}},"timings":{"prompt_n":26,"predicted_n":128,"prompt_per_second":410.5,"predicted_per_second":123.4,"prompt_ms":18.2}}`)
	s := parseTokenStats(body, "application/json")
	if s.PromptTokens != 26 || s.CompletionTokens != 128 || s.CachedTokens != 10 {
		t.Fatalf("tokens = %+v", s)
	}
	if s.PromptPerSecond != 410.5 || s.TokensPerSecond != 123.4 || s.PromptMs != 18.2 {
		t.Fatalf("rates = %+v", s)
	}
}

func TestParseTokenStatsSSE(t *testing.T) {
	body := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"timings\":{\"prompt_n\":12,\"predicted_n\":8,\"predicted_per_second\":99.5,\"cache_n\":3},\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":8}}\n\n" +
		"data: [DONE]\n")
	s := parseTokenStats(body, "text/event-stream")
	if s.PromptTokens != 12 || s.CompletionTokens != 8 || s.CachedTokens != 3 {
		t.Fatalf("tokens = %+v", s)
	}
	if s.TokensPerSecond != 99.5 {
		t.Fatalf("tok/s = %v", s.TokensPerSecond)
	}
}

func TestCopyHeaderMapRedactsSecrets(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer secret")
	h.Set("X-Api-Key", "k")
	h.Set("Content-Type", "application/json")
	out := copyHeaderMap(h)
	if out["Authorization"][0] != "[redacted]" || out["X-Api-Key"][0] != "[redacted]" {
		t.Fatalf("secrets not redacted: %+v", out)
	}
	if out["Content-Type"][0] != "application/json" {
		t.Fatalf("content-type = %v", out["Content-Type"])
	}
}

func TestActivityRecordsProxiedRequest(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/_router/load" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"loaded"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":4,"completion_tokens":7},"timings":{"predicted_per_second":42.0,"prompt_ms":33.6}}`))
	}))
	defer peer.Close()

	cfg, err := config.Load("../../test/router-a.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p := cfg.Peer("digger")
	if p == nil {
		t.Fatal("peer digger missing")
	}
	p.BaseURL = peer.URL

	logger := NewLogger(io.Discard, false)
	r := New(cfg, logger, "router-a")
	h := NewHandler(r, logger)

	body := `{"model":"digger/remote-model","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer secret-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	listReq := httptest.NewRequest("GET", "/_router/activity", nil)
	listRec := httptest.NewRecorder()
	h.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("activity list status=%d", listRec.Code)
	}
	var payload struct {
		Entries []ActivityEntry `json:"entries"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("list json: %v", err)
	}
	if len(payload.Entries) != 1 {
		t.Fatalf("entries = %d, want 1: %+v", len(payload.Entries), payload.Entries)
	}
	e := payload.Entries[0]
	if e.Model != "remote-model" || e.Path != "/v1/chat/completions" || e.Status != 200 {
		t.Fatalf("entry = %+v", e)
	}
	if e.Tokens.PromptTokens != 4 || e.Tokens.CompletionTokens != 7 || e.Tokens.TokensPerSecond != 42 {
		t.Fatalf("tokens = %+v", e.Tokens)
	}
	if e.TTFTMs != 34 {
		t.Fatalf("ttft = %d, want prompt_ms 34", e.TTFTMs)
	}
	if !e.HasCapture {
		t.Fatal("expected capture")
	}

	capReq := httptest.NewRequest("GET", "/_router/activity/1", nil)
	capRec := httptest.NewRecorder()
	h.ServeHTTP(capRec, capReq)
	if capRec.Code != http.StatusOK {
		t.Fatalf("capture status=%d body=%s", capRec.Code, capRec.Body.String())
	}
	var cap ActivityCapture
	if err := json.Unmarshal(capRec.Body.Bytes(), &cap); err != nil {
		t.Fatalf("capture json: %v", err)
	}
	if !strings.Contains(cap.ReqBody, `"hi"`) {
		t.Fatalf("req body missing prompt: %s", cap.ReqBody)
	}
	if !strings.Contains(cap.RespBody, `"completion_tokens":7`) {
		t.Fatalf("resp body missing usage: %s", cap.RespBody)
	}
	if got := cap.ReqHeaders["Authorization"]; len(got) != 1 || got[0] != "[redacted]" {
		t.Fatalf("authorization = %v", cap.ReqHeaders["Authorization"])
	}

	// A chat response carries no image, so the thumbnail endpoint is a 404.
	thumbReq := httptest.NewRequest("GET", "/_router/activity/1/thumb", nil)
	thumbRec := httptest.NewRecorder()
	h.ServeHTTP(thumbRec, thumbReq)
	if thumbRec.Code != http.StatusNotFound {
		t.Fatalf("non-image thumb status = %d, want 404", thumbRec.Code)
	}
}

func TestActivityRecordsSSETimings(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/_router/load" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		if fl != nil {
			fl.Flush()
		}
		_, _ = w.Write([]byte("data: {\"timings\":{\"prompt_n\":9,\"predicted_n\":3,\"predicted_per_second\":55.5,\"prompt_per_second\":400,\"prompt_ms\":9000}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer peer.Close()

	cfg, err := config.Load("../../test/router-a.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p := cfg.Peer("digger")
	if p == nil {
		t.Fatal("peer digger missing")
	}
	p.BaseURL = peer.URL

	logger := NewLogger(io.Discard, false)
	r := New(cfg, logger, "router-a")
	h := NewHandler(r, logger)

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(`{"model":"digger/remote-model","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	entries := r.Activity().List()
	if len(entries) != 1 {
		t.Fatalf("entries = %d", len(entries))
	}
	tok := entries[0].Tokens
	if tok.PromptTokens != 9 || tok.CompletionTokens != 3 || tok.TokensPerSecond != 55.5 {
		t.Fatalf("sse tokens = %+v", tok)
	}
	if entries[0].TTFTMs <= 0 || entries[0].TTFTMs >= 9000 {
		t.Fatalf("ttft = %d, want wall-clock first token, not prompt_ms", entries[0].TTFTMs)
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("content-type = %q", rec.Header().Get("Content-Type"))
	}
}

func TestActivityWriterStampsStreamingTTFT(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "text/event-stream")
	w := &activityWriter{
		ResponseWriter: rec,
		max:            maxCaptureBody,
		started:        time.Now().Add(-120 * time.Millisecond),
	}
	_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":null}}]}\n\n"))
	if w.ttftSet {
		t.Fatal("role chunk set ttft")
	}
	_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"\"}}]}\n\n"))
	if w.ttftSet {
		t.Fatal("empty content set ttft")
	}
	_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n"))
	if !w.ttftSet || w.ttftMs < 100 {
		t.Fatalf("ttft=%d set=%v", w.ttftMs, w.ttftSet)
	}
	stamped := w.ttftMs
	_, _ = w.Write([]byte("data: {\"timings\":{\"prompt_ms\":3,\"predicted_per_second\":10}}\n\n"))
	if w.ttftMs != stamped {
		t.Fatalf("ttft overwritten: %d -> %d", stamped, w.ttftMs)
	}
}

func TestIsGenerationRequest(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		want         bool
	}{
		{"POST", "/v1/chat/completions", true},
		{"POST", "/v1/embeddings", true},
		{"POST", "/sdapi/v1/txt2img", true},
		{"POST", "/completion", true},
		{"GET", "/metrics", false},
		{"GET", "/web/sprite.svg", false},
		{"GET", "/mcp", false},
		{"POST", "/mcp", false},
		{"GET", "/v1/chat/completions", false},
		{"POST", "/tokenize", false},
	} {
		if got := isGenerationRequest(tc.method, tc.path); got != tc.want {
			t.Errorf("isGenerationRequest(%s %s) = %v, want %v", tc.method, tc.path, got, tc.want)
		}
	}
}

func TestActivitySkipsNonGenerationRequests(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer peer.Close()

	cfg, err := config.Load("../../test/router-a.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cfg.Peer("digger").BaseURL = peer.URL

	logger := NewLogger(io.Discard, false)
	r := New(cfg, logger, "router-a")
	h := NewHandler(r, logger)

	for _, path := range []string{"/props", "/web/sprite.svg", "/mcp"} {
		req := httptest.NewRequest("GET", path+"?model=digger/remote-model", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d", path, rec.Code)
		}
	}
	if n := len(r.Activity().List()); n != 0 {
		t.Fatalf("activity rows = %d, want 0", n)
	}
}

// An image response above the capture limit still yields a thumbnail: the router
// buffers the whole body, decodes and downscales the first image before clipping
// the capture, and serves it from /_router/activity/{id}/thumb.
func TestActivityImageThumbnail(t *testing.T) {
	// Incompressible noise, so the base64 JSON body stays above the capture limit.
	src := image.NewNRGBA(image.Rect(0, 0, 512, 512))
	rnd := rand.New(rand.NewPCG(1, 2))
	for i := range src.Pix {
		src.Pix[i] = byte(rnd.IntN(256))
	}
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, src); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	body := `{"images":["` + base64.StdEncoding.EncodeToString(pngBuf.Bytes()) + `"],"parameters":{}}`
	if len(body) <= maxCaptureBody {
		t.Fatalf("test body %d bytes, want over the %d capture limit", len(body), maxCaptureBody)
	}

	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/_router/load" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer peer.Close()

	cfg, err := config.Load("../../test/router-a.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cfg.Peer("digger").BaseURL = peer.URL

	logger := NewLogger(io.Discard, false)
	r := New(cfg, logger, "router-a")
	h := NewHandler(r, logger)

	req := httptest.NewRequest("POST", "/sdapi/v1/txt2img", bytes.NewBufferString(`{"model":"digger/remote-model","prompt":"a cat","width":512,"height":512}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	entries := r.Activity().List()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if !entries[0].HasCapture {
		t.Fatal("expected capture")
	}

	// The stored capture is still clipped to the limit even though the whole
	// body was buffered for the thumbnail.
	cap, ok := r.Activity().Capture(entries[0].ID)
	if !ok {
		t.Fatal("capture missing")
	}
	if !cap.Truncated || len(cap.RespBody) != maxCaptureBody {
		t.Fatalf("capture = truncated %v, %d bytes, want truncated at %d", cap.Truncated, len(cap.RespBody), maxCaptureBody)
	}

	thumbReq := httptest.NewRequest("GET", "/_router/activity/1/thumb", nil)
	thumbRec := httptest.NewRecorder()
	h.ServeHTTP(thumbRec, thumbReq)
	if thumbRec.Code != http.StatusOK {
		t.Fatalf("thumb status = %d body=%s", thumbRec.Code, thumbRec.Body.String())
	}
	if ct := thumbRec.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("thumb content-type = %q", ct)
	}
	thumb, format, err := image.Decode(bytes.NewReader(thumbRec.Body.Bytes()))
	if err != nil {
		t.Fatalf("decode thumb: %v", err)
	}
	if format != "jpeg" {
		t.Fatalf("thumb format = %q, want jpeg", format)
	}
	if b := thumb.Bounds(); b.Dx() > maxThumbEdge || b.Dy() > maxThumbEdge {
		t.Fatalf("thumb bounds = %v, want long edge at most %d", b, maxThumbEdge)
	}
}

// An image the server cannot decode (an unregistered format such as WebP) records
// why on the entry, so the Activity UI can tell a decode failure apart from a row
// that never held an image, and serves no thumbnail.
func TestActivityThumbnailDecodeFailureSurfaced(t *testing.T) {
	// WebP magic with no decoder registered, so image.Decode fails on it.
	fake := append([]byte("RIFF\x24\x00\x00\x00WEBPVP8 "), make([]byte, 24)...)
	body := `{"images":["` + base64.StdEncoding.EncodeToString(fake) + `"]}`

	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/_router/load" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer peer.Close()

	cfg, err := config.Load("../../test/router-a.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cfg.Peer("digger").BaseURL = peer.URL

	logger := NewLogger(io.Discard, false)
	r := New(cfg, logger, "router-a")
	h := NewHandler(r, logger)

	req := httptest.NewRequest("POST", "/sdapi/v1/txt2img", bytes.NewBufferString(`{"model":"digger/remote-model","prompt":"a cat"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	entries := r.Activity().List()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if !strings.Contains(entries[0].ThumbIssue, "decode") {
		t.Fatalf("thumb issue = %q, want a decode failure", entries[0].ThumbIssue)
	}

	thumbRec := httptest.NewRecorder()
	h.ServeHTTP(thumbRec, httptest.NewRequest("GET", "/_router/activity/1/thumb", nil))
	if thumbRec.Code != http.StatusNotFound {
		t.Fatalf("thumb status = %d, want 404", thumbRec.Code)
	}
}

// The extra image buffer stops at imgMax and flags the cut, so recordProxy can
// report an oversized response instead of a decode failure.
func TestActivityWriterFlagsImageBufferTruncation(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &activityWriter{ResponseWriter: rec, max: maxCaptureBody, imgMax: 4, started: time.Now()}
	_, _ = w.Write([]byte("abcdef"))
	if !w.imgTruncated || w.img.String() != "abcd" {
		t.Fatalf("img = %q, truncated = %v", w.img.String(), w.imgTruncated)
	}
}

func TestParseTokenStatsTimingsPhases(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":23,"completion_tokens":40,"prompt_tokens_details":{"cached_tokens":5}},"timings":{"cache_n":5,"prompt_n":18,"prompt_ms":1068.6,"predicted_n":40,"predicted_ms":604.6,"predicted_per_second":66.2}}`)
	s := parseTokenStats(body, "application/json")
	if s.PromptTokens != 23 || s.CachedTokens != 5 || s.PromptEvalTokens != 18 || s.PredictedMs != 604.6 || s.CompletionTokens != 40 {
		t.Fatalf("stats = %+v", s)
	}
}

// A stream longer than the capture clip must still yield its final
// usage/timings chunk, which arrives after the clip.
func TestActivityWriterKeepsStatsPastCaptureClip(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "text/event-stream")
	w := &activityWriter{ResponseWriter: rec, max: maxCaptureBody, started: time.Now()}
	chunk := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"" + strings.Repeat("x", 200) + "\"}}]}\n\n")
	for written := 0; written < maxCaptureBody+3*maxStatsTail; written += len(chunk) {
		_, _ = w.Write(chunk)
	}
	_, _ = w.Write([]byte("data: {\"usage\":{\"prompt_tokens\":900,\"completion_tokens\":2500,\"prompt_tokens_details\":{\"cached_tokens\":800}},\"timings\":{\"prompt_n\":100,\"prompt_ms\":250,\"predicted_n\":2500,\"predicted_ms\":40000}}\n\ndata: [DONE]\n\n"))
	if !w.truncated {
		t.Fatal("capture should be truncated")
	}
	if w.tail.Len() > 2*maxStatsTail {
		t.Fatalf("tail grew to %d bytes", w.tail.Len())
	}
	head := parseTokenStats(w.body.Bytes(), "text/event-stream")
	if head.CompletionTokens != 0 {
		t.Fatalf("clipped head unexpectedly carries stats: %+v", head)
	}
	got := mergeTokenStats(head, parseTokenStats(w.tail.Bytes(), "text/event-stream"))
	if got.CompletionTokens != 2500 || got.PromptTokens != 900 || got.CachedTokens != 800 || got.PromptEvalTokens != 100 || got.PredictedMs != 40000 {
		t.Fatalf("tail stats = %+v", got)
	}
}

func TestActivityRecordsTokensForLongStream(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/_router/load" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"" + strings.Repeat("y", 300) + "\"}}]}\n\n")
		for n := 0; n < maxCaptureBody+maxStatsTail; n += len(chunk) {
			_, _ = w.Write(chunk)
		}
		_, _ = w.Write([]byte("data: {\"timings\":{\"cache_n\":40,\"prompt_n\":60,\"prompt_ms\":120,\"predicted_n\":1500,\"predicted_ms\":25000,\"predicted_per_second\":60}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer peer.Close()

	cfg, err := config.Load("../../test/router-a.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cfg.Peer("digger").BaseURL = peer.URL
	logger := NewLogger(io.Discard, false)
	r := New(cfg, logger, "router-a")
	h := NewHandler(r, logger)

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(`{"model":"digger/remote-model","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	entries := r.Activity().List()
	if len(entries) != 1 {
		t.Fatalf("entries = %d", len(entries))
	}
	tok := entries[0].Tokens
	if tok.PromptTokens != 100 || tok.CompletionTokens != 1500 || tok.PromptEvalTokens != 60 || tok.CachedTokens != 40 || tok.PredictedMs != 25000 {
		t.Fatalf("long stream tokens = %+v", tok)
	}
	out := r.metrics.Render("router-a")
	for _, want := range []string{
		`model_router_phase_tokens_total{model="remote-model",phase="decode"} 1500`,
		`model_router_phase_seconds_total{model="remote-model",phase="decode"} 25.000`,
		`model_router_phase_tokens_total{model="remote-model",phase="prefill"} 60`,
		`model_router_tokens_total{model="remote-model",kind="cached"} 40`,
		`model_router_tokens_total{model="remote-model",kind="prompt"} 100`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("metrics missing %q\n%s", want, out)
		}
	}
}
