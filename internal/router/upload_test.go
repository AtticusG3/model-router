package router

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"model-router/internal/config"
)

// uploadRouter serves one local stanza, chat-a, through a fake process so no
// GPU is needed. extra is top-level YAML, e.g. a max_request_body_mb line.
func uploadRouter(t *testing.T, backendURL, extra string) (*Router, http.Handler) {
	t.Helper()
	u, err := url.Parse(backendURL)
	if err != nil {
		t.Fatalf("parse backend url: %v", err)
	}
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
%s
stanzas:
  - model_id: chat-a
    command: "fake --port {port}"
    vram_mb: 0
    health_check: /
    spin_up_seconds: 5
    port: %s
`, extra, u.Port())))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	r := New(cfg, NewLogger(io.Discard, false), "test")
	r.spawn = func(string, []string) (Proc, error) { return newFakeProc(), nil }
	r.ledger.SetGPUs([]*GPUState{gpu(0, 10000)})
	t.Cleanup(func() { _ = r.Unload("chat-a") })
	return r, NewHandler(r, NewLogger(io.Discard, false))
}

// fillReader yields a deterministic byte stream so a large body can be
// generated on the fly instead of held in memory.
type fillReader struct{ i int }

func (f *fillReader) Read(b []byte) (int, error) {
	for i := range b {
		b[i] = byte(f.i)
		f.i++
	}
	return len(b), nil
}

// writeMultipartBody writes a file part of fileBytes then the model field.
func writeMultipartBody(dst io.Writer, boundary string, fileBytes int64) {
	mw := multipart.NewWriter(dst)
	_ = mw.SetBoundary(boundary)
	fw, err := mw.CreateFormFile("file", "big.bin")
	if err != nil {
		return
	}
	_, _ = io.CopyN(fw, &fillReader{}, fileBytes)
	_ = mw.WriteField("model", "chat-a")
	_ = mw.Close()
}

// A body the client declares as larger than the cap is refused before it is
// read, so the backend never sees the upload.
func TestUploadOverCapReturns413(t *testing.T) {
	var hits atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	_, h := uploadRouter(t, backend.URL, "max_request_body_mb: 64")

	body := &fillReader{}
	req := httptest.NewRequest("POST", "/v1/chat/completions", io.NopCloser(body))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=xyz")
	req.ContentLength = 300 << 20
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "max_request_body_mb") {
		t.Fatalf("413 body %q does not name max_request_body_mb", rec.Body.String())
	}
	if hits.Load() != 0 {
		t.Fatalf("backend reached %d times, want 0", hits.Load())
	}
	// Only writeBodyError's bounded drain reads it, so the client can see the 413.
	if body.i > 1<<20 {
		t.Fatalf("body read %d bytes, want at most the 1 MB drain", body.i)
	}
}

// A client sending a chunked body a little over the cap reads the 413 over a
// real connection instead of a reset: the router drains the overshoot first.
func TestUploadOverCapClientReads413(t *testing.T) {
	var hits atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer backend.Close()
	_, h := uploadRouter(t, backend.URL, "max_request_body_mb: 1")
	srv := httptest.NewServer(h)
	defer srv.Close()

	// 1.5 MB with no Content-Length: read to the cap, then 413 with ~512 KB
	// still in flight, more than net/http discards on its own after the handler.
	body := io.LimitReader(&fillReader{}, 3<<19)
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", body)
	if err != nil {
		t.Fatalf("client did not get a response: %v", err)
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body %s", resp.StatusCode, msg)
	}
	if !strings.Contains(string(msg), "max_request_body_mb") {
		t.Fatalf("413 body %q does not name max_request_body_mb", msg)
	}
	if hits.Load() != 0 {
		t.Fatalf("backend reached %d times, want 0", hits.Load())
	}

	// The same body in-process: the handler itself must drain the overshoot.
	lr := &io.LimitedReader{R: &fillReader{}, N: 3 << 19}
	req := httptest.NewRequest("POST", "/v1/chat/completions", lr)
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("in-process status = %d, want 413", rec.Code)
	}
	if lr.N != 0 {
		t.Fatalf("%d bytes of the overshoot left unread, want 0", lr.N)
	}
}

// A stanza override lowers the cap for that stanza alone.
func TestBodyLimitPerStanzaOverride(t *testing.T) {
	cfg, err := config.Parse([]byte(`
stanzas:
  - model_id: a
    command: "x --port {port}"
  - model_id: b
    command: "x --port {port}"
    max_request_body_mb: 8
`))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if got := cfg.RequestBodyLimit("a"); got != config.DefaultMaxRequestBodyMB<<20 {
		t.Fatalf("stanza a limit = %d, want the default", got)
	}
	if got := cfg.RequestBodyLimit("b"); got != 8<<20 {
		t.Fatalf("stanza b limit = %d, want 8 MB", got)
	}
	if got := cfg.RequestBodyLimit(""); got != config.DefaultMaxRequestBodyMB<<20 {
		t.Fatalf("top-level limit = %d, want the default", got)
	}
}

// A multipart upload whose model field is last is spooled to a temp file and
// reaches the backend byte-identical, without the router buffering it in RAM.
func TestUploadSpoolModelLastIsByteIdentical(t *testing.T) {
	const fileBytes = 150 << 20
	const boundary = "routerboundary"

	type result struct {
		hash string
		size int64
	}
	got := make(chan result, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(http.StatusOK)
			return
		}
		h := sha256.New()
		n, _ := io.Copy(h, r.Body)
		got <- result{hex.EncodeToString(h.Sum(nil)), n}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()
	_, h := uploadRouter(t, backend.URL, "")

	// The expected hash, computed from the same generated bytes.
	wh := sha256.New()
	writeMultipartBody(wh, boundary, fileBytes)
	want := hex.EncodeToString(wh.Sum(nil))

	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		writeMultipartBody(pw, boundary, fileBytes)
	}()
	req := httptest.NewRequest("POST", "/v1/chat/completions", pr)
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	req.ContentLength = -1

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	runtime.ReadMemStats(&after)
	pr.Close()

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	res := <-got
	if res.hash != want {
		t.Fatalf("backend body hash %s, want %s (upload not byte-identical)", res.hash, want)
	}
	if res.size <= fileBytes {
		t.Fatalf("backend received %d bytes, want over the %d byte file part", res.size, fileBytes)
	}
	if growth := after.TotalAlloc - before.TotalAlloc; growth > 40<<20 {
		t.Fatalf("router allocated %d MB for a %d MB upload, want under 40 MB", growth>>20, fileBytes>>20)
	} else {
		t.Logf("total alloc growth %d bytes, heap %d -> %d", growth, before.HeapAlloc, after.HeapAlloc)
	}
}

// A chunked media response is flushed per chunk, so the client sees it before
// the backend finishes writing.
func TestStreamAudioFlushesChunks(t *testing.T) {
	acks := make(chan struct{})
	var backendDone atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/speech" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		fl, _ := w.(http.Flusher)
		chunk := make([]byte, 4096)
		for i := 0; i < 3; i++ {
			_, _ = w.Write(chunk)
			if fl != nil {
				fl.Flush()
			}
			<-acks
		}
		backendDone.Store(1)
	}))
	defer backend.Close()
	_, h := uploadRouter(t, backend.URL, "")
	srv := httptest.NewServer(h)
	defer srv.Close()

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Post(srv.URL+"/v1/audio/speech", "application/json", strings.NewReader(`{"model":"chat-a","input":"hi"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	buf := make([]byte, 8192)
	for i := 0; i < 3; i++ {
		n, err := resp.Body.Read(buf)
		if n == 0 {
			t.Fatalf("read %d returned no data (err %v); response was buffered", i+1, err)
		}
		if backendDone.Load() != 0 {
			t.Fatalf("client read %d after the backend had finished; response was buffered", i+1)
		}
		acks <- struct{}{}
	}
}

// A media response row records the content type, byte count and a body hash,
// and stores no response body; the request body (the TTS prompt) is still kept.
func TestBodyMediaResponseRowHasNoCapture(t *testing.T) {
	payload := make([]byte, 12*1024)
	for i := range payload {
		payload[i] = byte(i)
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/speech" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer backend.Close()
	r, h := uploadRouter(t, backend.URL, "")

	req := httptest.NewRequest("POST", "/v1/audio/speech", strings.NewReader(`{"model":"chat-a","input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	entries := r.Activity().List()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.ContentType != "audio/wav" {
		t.Fatalf("content type = %q, want audio/wav", e.ContentType)
	}
	if e.RespBytes != int64(len(payload)) {
		t.Fatalf("resp bytes = %d, want %d", e.RespBytes, len(payload))
	}
	if e.BodyHash == "" {
		t.Fatal("media row has no body hash")
	}
	c, ok := r.Activity().Capture(e.ID)
	if !ok || !e.HasCapture {
		t.Fatal("media row dropped its request capture")
	}
	if !strings.Contains(c.ReqBody, `"input":"hi"`) {
		t.Fatalf("request body = %q, want the TTS request", c.ReqBody)
	}
	if c.RespBody != "" {
		t.Fatalf("media response body captured: %d bytes", len(c.RespBody))
	}
}

// A multipart body with no model part routes by ?model=, both when the body
// fits the matcher's prefix and when it runs past it.
func TestUploadMultipartQueryModelFallback(t *testing.T) {
	for _, size := range []int64{1 << 10, 2 << 20} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			got := make(chan string, 1)
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/audio/transcriptions" {
					w.WriteHeader(http.StatusOK)
					return
				}
				h := sha256.New()
				_, _ = io.Copy(h, r.Body)
				got <- hex.EncodeToString(h.Sum(nil))
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"text":"ok"}`))
			}))
			defer backend.Close()
			_, h := uploadRouter(t, backend.URL, "")

			var body bytes.Buffer
			mw := multipart.NewWriter(&body)
			fw, err := mw.CreateFormFile("file", "clip.wav")
			if err != nil {
				t.Fatalf("form file: %v", err)
			}
			_, _ = io.CopyN(fw, &fillReader{}, size)
			_ = mw.Close()
			want := sha256.Sum256(body.Bytes())

			req := httptest.NewRequest("POST", "/v1/audio/transcriptions?model=chat-a", &body)
			req.Header.Set("Content-Type", mw.FormDataContentType())
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
			}
			if g := <-got; g != hex.EncodeToString(want[:]) {
				t.Fatalf("backend body hash %s, want %s", g, hex.EncodeToString(want[:]))
			}
		})
	}
}

// A ?model= query wins over a model part past the matcher's prefix, and the
// upload streams to the backend without being spooled to find that part.
func TestUploadQueryModelBeatsLateModelPartWithoutSpool(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	got := make(chan string, 1)
	spools := make(chan int, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" {
			w.WriteHeader(http.StatusOK)
			return
		}
		files, _ := filepath.Glob(filepath.Join(dir, "model-router-body-*"))
		spools <- len(files)
		h := sha256.New()
		_, _ = io.Copy(h, r.Body)
		got <- hex.EncodeToString(h.Sum(nil))
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	_, h := uploadRouter(t, backend.URL, "")

	// The late model part names no stanza: if it won, the request would 404.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "clip.wav")
	if err != nil {
		t.Fatalf("form file: %v", err)
	}
	_, _ = io.CopyN(fw, &fillReader{}, 2<<20)
	_ = mw.WriteField("model", "no-such-model")
	_ = mw.Close()
	want := sha256.Sum256(body.Bytes())

	req := httptest.NewRequest("POST", "/v1/audio/transcriptions?model=chat-a", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (query model wins); body %s", rec.Code, rec.Body.String())
	}
	if n := <-spools; n != 0 {
		t.Fatalf("spool files while the backend read the body = %d, want 0", n)
	}
	if g := <-got; g != hex.EncodeToString(want[:]) {
		t.Fatalf("backend body hash %s, want %s", g, hex.EncodeToString(want[:]))
	}
}

// A server-side failure preparing the body (here the spool file cannot be
// created) is a generic 500 that does not echo the error or the temp path.
func TestUploadSpoolFailureIs500(t *testing.T) {
	var hits atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	_, h := uploadRouter(t, backend.URL, "")
	missing := t.TempDir() + "/missing-spool-dir"
	t.Setenv("TMPDIR", missing)

	var body bytes.Buffer
	writeMultipartBody(&body, "spoolfail", 2<<20)
	req := httptest.NewRequest("POST", "/v1/chat/completions", &body)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=spoolfail")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body %s", rec.Code, rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "could not read request body" {
		t.Fatalf("body = %q, want the generic message", got)
	}
	if hits.Load() != 0 {
		t.Fatalf("backend reached %d times, want 0", hits.Load())
	}
}

func TestUploadCutShortIs400NotLogged(t *testing.T) {
	var hits atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer backend.Close()
	r, h := uploadRouter(t, backend.URL, "")
	var logs strings.Builder
	r.logger = NewLogger(&logs, false)

	// Model part last, so the matcher spools; the client goes away halfway.
	var full bytes.Buffer
	writeMultipartBody(&full, "cutshort", 2<<20)
	body := io.MultiReader(bytes.NewReader(full.Bytes()[:full.Len()/2]), iotest.ErrReader(io.ErrUnexpectedEOF))
	req := httptest.NewRequest("POST", "/v1/chat/completions", body)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=cutshort")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(logs.String(), "could not read request body") {
		t.Fatalf("a cancelled upload was logged as a server fault: %s", logs.String())
	}
	if hits.Load() != 0 {
		t.Fatalf("backend reached %d times, want 0", hits.Load())
	}
}

// slowMultipart writes the model field, enough file bytes to fill the matcher's
// prefix, then a slow tail the client abandons mid-upload.
func slowMultipart(pw *io.PipeWriter, boundary string) {
	defer pw.Close()
	mw := multipart.NewWriter(pw)
	_ = mw.SetBoundary(boundary)
	_ = mw.WriteField("model", "chat-a")
	fw, err := mw.CreateFormFile("file", "big.bin")
	if err != nil {
		return
	}
	lazy := &fillReader{}
	if _, err := io.CopyN(fw, lazy, bodyPrefixBytes+4096); err != nil {
		return
	}
	for {
		if _, err := io.CopyN(fw, lazy, 1024); err != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A client that goes away mid-upload closes the backend request and frees the
// model slot at once, so the next request is not blocked.
func TestUploadClientGoneFreesSlot(t *testing.T) {
	started := make(chan struct{}, 4)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(http.StatusOK)
			return
		}
		select {
		case started <- struct{}{}:
		default:
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()
	_, h := uploadRouter(t, backend.URL, "")

	const boundary = "gone"
	pr, pw := io.Pipe()
	defer pr.Close()
	go slowMultipart(pw, boundary)

	ctx, cancel := context.WithCancel(context.Background())
	req1, err := http.NewRequestWithContext(ctx, "POST", "/v1/chat/completions", pr)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req1.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	req1.ContentLength = -1
	done1 := make(chan struct{})
	go func() {
		defer close(done1)
		h.ServeHTTP(httptest.NewRecorder(), req1)
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("upload never reached the backend")
	}
	cancel()
	pw.CloseWithError(errors.New("client gone"))

	select {
	case <-done1:
	case <-time.After(5 * time.Second):
		t.Fatal("first request did not end after the client went away")
	}

	start := time.Now()
	req2 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"chat-a"}`))
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second request status = %d, want 200; body %s", rec2.Code, rec2.Body.String())
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("second request took %v; the slot was not freed", d)
	}
}

// A multipart upload whose model field is first is streamed to the backend
// without being buffered, and still arrives byte-identical.
func TestUploadStreamsModelFirst(t *testing.T) {
	const fileBytes = 4 << 20
	const boundary = "streamboundary"

	got := make(chan [2]any, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(http.StatusOK)
			return
		}
		h := sha256.New()
		n, _ := io.Copy(h, r.Body)
		got <- [2]any{hex.EncodeToString(h.Sum(nil)), n}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()
	_, h := uploadRouter(t, backend.URL, "max_request_body_mb: 64")

	wh := sha256.New()
	writeModelFirstBody(wh, boundary, fileBytes)
	want := hex.EncodeToString(wh.Sum(nil))

	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		writeModelFirstBody(pw, boundary, fileBytes)
	}()
	req := httptest.NewRequest("POST", "/v1/chat/completions", pr)
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	pr.Close()

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	res := <-got
	if res[0] != want {
		t.Fatalf("backend body hash %v, want %s (upload not byte-identical)", res[0], want)
	}
	if res[1].(int64) <= fileBytes {
		t.Fatalf("backend received %v bytes, want over the %d byte file part", res[1], fileBytes)
	}
}

// writeModelFirstBody writes the model field then a file part, so the matcher
// finds the model in its prefix and streams the rest.
func writeModelFirstBody(dst io.Writer, boundary string, fileBytes int64) {
	mw := multipart.NewWriter(dst)
	_ = mw.SetBoundary(boundary)
	_ = mw.WriteField("model", "chat-a")
	fw, err := mw.CreateFormFile("file", "big.bin")
	if err != nil {
		return
	}
	_, _ = io.CopyN(fw, &fillReader{}, fileBytes)
	_ = mw.Close()
}

// speechRouter serves one local stanza, speech, routed by path_prefix /v1/audio
// so a raw body needs no model field, with a capMB body cap.
func speechRouter(t *testing.T, backendURL string, capMB int) http.Handler {
	t.Helper()
	u, err := url.Parse(backendURL)
	if err != nil {
		t.Fatalf("backend url: %v", err)
	}
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
max_request_body_mb: %d
stanzas:
  - model_id: speech
    command: "fake --port {port}"
    vram_mb: 0
    health_check: /
    spin_up_seconds: 5
    port: %s
    match:
      path_prefix: /v1/audio
      path_default: true
`, capMB, u.Port())))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	r := New(cfg, NewLogger(io.Discard, false), "test")
	r.spawn = func(string, []string) (Proc, error) { return newFakeProc(), nil }
	r.ledger.SetGPUs([]*GPUState{gpu(0, 10000)})
	t.Cleanup(func() { _ = r.Unload("speech") })
	return NewHandler(r, NewLogger(io.Discard, false))
}

// A raw (non-JSON, non-multipart) body routed by path_prefix is streamed
// through untouched.
func TestUploadRawStreamWithPathPrefix(t *testing.T) {
	got := make(chan string, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" {
			w.WriteHeader(http.StatusOK)
			return
		}
		h := sha256.New()
		_, _ = io.Copy(h, r.Body)
		got <- hex.EncodeToString(h.Sum(nil))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"text":"ok"}`))
	}))
	defer backend.Close()
	h := speechRouter(t, backend.URL, 64)

	payload := make([]byte, 1<<20)
	for i := range payload {
		payload[i] = byte(i)
	}
	want := sha256.Sum256(payload)

	req := httptest.NewRequest("POST", "/v1/audio/transcriptions", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/octet-stream")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if gotHash := <-got; gotHash != hex.EncodeToString(want[:]) {
		t.Fatalf("backend body hash %s, want %s", gotHash, hex.EncodeToString(want[:]))
	}
}

// A forwarded 100 Continue is not the final head: it must not set the recorded
// status or decide the body kind (its headers are empty).
func TestStreamInterimContinueIsNotTheHead(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &activityWriter{ResponseWriter: rec, max: maxCaptureBody, started: time.Now()}
	w.Header().Set("Content-Type", "audio/wav")
	w.WriteHeader(http.StatusContinue)
	if w.media || w.status != 0 || w.wroteHead {
		t.Fatalf("100 recorded as the head: media=%v status=%d wroteHead=%v", w.media, w.status, w.wroteHead)
	}
	w.WriteHeader(http.StatusOK)
	if !w.media || w.status != http.StatusOK {
		t.Fatalf("final head not recorded: media=%v status=%d", w.media, w.status)
	}
	_, _ = w.Write([]byte("abc"))
	if w.respBytes != 3 || w.body.Len() != 0 {
		t.Fatalf("media body captured: respBytes=%d bodyLen=%d", w.respBytes, w.body.Len())
	}
}

// A body small enough to stay in memory is still held to a cap below the
// in-memory size.
func TestBodyReadPreparedSmallBodyOverSmallCap(t *testing.T) {
	_, err := readPrepared(bytes.NewReader(make([]byte, 512<<10)), 256<<10)
	if !errors.Is(err, errBodyTooLarge) {
		t.Fatalf("512 KB body under a 256 KB cap: err = %v, want errBodyTooLarge", err)
	}
	p, err := readPrepared(bytes.NewReader(make([]byte, 256<<10)), 256<<10)
	if err != nil || p.size != 256<<10 || p.path != "" {
		t.Fatalf("body at the cap: err = %v, prepared = %+v, want it kept in memory", err, p)
	}
}

// Cleanup runs from both the matcher's error path and the request hook; the
// second call must not remove a file that has since taken the same path.
func TestBodyCleanupIsIdempotent(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	p, err := readPrepared(bytes.NewReader(make([]byte, 2<<20)), 4<<20)
	if err != nil || p.path == "" {
		t.Fatalf("readPrepared: err = %v, prepared = %+v, want a spooled body", err, p)
	}
	path := p.path
	p.cleanup()
	if err := os.WriteFile(path, []byte("someone else's"), 0o600); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	p.cleanup()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("second cleanup removed a file it no longer owns: %v", err)
	}
}

// overCapBackend reports, for each proxied request on path, whether the
// backend read a complete body.
func overCapBackend(t *testing.T, path string) (*httptest.Server, chan bool) {
	t.Helper()
	complete := make(chan bool, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, err := io.Copy(io.Discard, r.Body)
		select {
		case complete <- err == nil:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)
	return backend, complete
}

// assertRefusedOverCap checks a chunked upload over the cap got 413 and the
// backend never read a whole (short) body.
func assertRefusedOverCap(t *testing.T, rec *httptest.ResponseRecorder, complete chan bool) {
	t.Helper()
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "max_request_body_mb") {
		t.Fatalf("413 body %q does not name max_request_body_mb", rec.Body.String())
	}
	select {
	case ok := <-complete:
		if ok {
			t.Fatal("backend read a complete short body; the upload was forwarded truncated")
		}
	case <-time.After(2 * time.Second):
		// The backend never saw the request end cleanly: refused, not short.
	}
}

// A chunked multipart upload whose model is in the prefix, but whose file part
// runs past the cap, is refused with 413 rather than streamed short.
func TestUploadChunkedMultipartOverCapRefused(t *testing.T) {
	backend, complete := overCapBackend(t, "/v1/chat/completions")
	_, h := uploadRouter(t, backend.URL, "max_request_body_mb: 1")

	pr, pw := io.Pipe()
	defer pr.Close()
	go func() {
		defer pw.Close()
		writeModelFirstBody(pw, "chunkedcap", 4<<20)
	}()
	req := httptest.NewRequest("POST", "/v1/chat/completions", pr)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=chunkedcap")
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assertRefusedOverCap(t, rec, complete)
}

// A chunked raw (octet-stream) upload over the cap is refused with 413 rather
// than streamed short.
func TestUploadChunkedRawOverCapRefused(t *testing.T) {
	backend, complete := overCapBackend(t, "/v1/audio/transcriptions")
	h := speechRouter(t, backend.URL, 1)

	req := httptest.NewRequest("POST", "/v1/audio/transcriptions", bytes.NewReader(make([]byte, 2<<20)))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assertRefusedOverCap(t, rec, complete)
}

// When spillWhileBlocked hands a spooled body back to the local path, the
// spool must still be there for the local serve, and go when the request ends.
func TestSpillWhileBlockedKeepsSpoolForLocalServe(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	r := spilloverRouter(t, "start_port: 5900\n"+pinnedHoldersYAML)
	r.ledger.SetGPUs([]*GPUState{gpu(0, 32000), gpu(1, 32000)})
	payload := make([]byte, 2<<20) // over bodyMemoryBytes, so it is spooled
	for i := range payload {
		payload[i] = byte(i)
	}
	cs, req := withCleanups(httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(payload)))

	if r.spillWhileBlocked(httptest.NewRecorder(), req, r.cfg().Stanza("shared-model")) {
		t.Fatal("room is free, so the request should carry on locally")
	}
	spools, _ := filepath.Glob(filepath.Join(dir, "model-router-body-*"))
	if len(spools) != 1 {
		t.Fatalf("spool files after handing back to the local path = %d, want 1", len(spools))
	}
	got, err := io.ReadAll(req.Body)
	req.Body.Close()
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("local path read %d bytes (err %v), want the %d byte body", len(got), err, len(payload))
	}
	cs.run()
	if spools, _ := filepath.Glob(filepath.Join(dir, "model-router-body-*")); len(spools) != 0 {
		t.Fatalf("spool files after the request ended = %d, want 0", len(spools))
	}
}

// A response written with no Content-Type (the auto-200 path, or an upstream
// that sends none) is classed by the bytes net/http sniffs, so binary audio is
// not captured.
func TestStreamSniffsMediaWithoutContentType(t *testing.T) {
	wav := append([]byte("RIFF\x24\x00\x00\x00WAVEfmt "), make([]byte, 64)...)
	for _, explicitHead := range []bool{false, true} {
		rec := httptest.NewRecorder()
		w := &activityWriter{ResponseWriter: rec, max: maxCaptureBody, started: time.Now()}
		if explicitHead {
			w.WriteHeader(http.StatusOK)
		}
		_, _ = w.Write(wav)
		if !w.media || w.body.Len() != 0 || w.respBytes != int64(len(wav)) {
			t.Fatalf("head=%v: media=%v bodyLen=%d respBytes=%d, want an uncaptured media body",
				explicitHead, w.media, w.body.Len(), w.respBytes)
		}
		if !isMediaResponse(w.sniffed) {
			t.Fatalf("head=%v: sniffed type %q is not media", explicitHead, w.sniffed)
		}
	}

	// Text with no Content-Type is still captured.
	w := &activityWriter{ResponseWriter: httptest.NewRecorder(), max: maxCaptureBody, started: time.Now()}
	_, _ = w.Write([]byte(`{"ok":true}`))
	if w.media || w.body.Len() == 0 {
		t.Fatalf("JSON body: media=%v bodyLen=%d, want it captured", w.media, w.body.Len())
	}
}

// The activity writer does not flush a non-streamed body itself; the proxy's
// own flush policy decides.
func TestStreamActivityWriterDoesNotFlushJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &activityWriter{ResponseWriter: rec, max: maxCaptureBody, started: time.Now()}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":`))
	_, _ = w.Write([]byte(`true}`))
	if rec.Flushed {
		t.Fatal("a chunked JSON body was flushed by the activity writer on every Write")
	}
}

// A model value cut off by the end of the multipart prefix is not used: the
// matcher scans the whole body and finds the full id.
func TestUploadModelStraddlingPrefixIsScannedWhole(t *testing.T) {
	cfg := testConfig(t)
	build := func(fill int) []byte {
		return multipartReqBody(t, [2]string{"prompt", strings.Repeat("a", fill)}, [2]string{"model", "agents-a1"})
	}
	body := build(bodyPrefixBytes)
	// Shift the filler so the prefix ends after "agents", mid-value.
	fill := bodyPrefixBytes - (bytes.Index(body, []byte("agents-a1")) + len("agents") - bodyPrefixBytes)
	body = build(fill)
	if i := bytes.Index(body, []byte("agents-a1")); i+len("agents") != bodyPrefixBytes {
		t.Fatalf("model value at %d does not straddle the %d-byte prefix", i, bodyPrefixBytes)
	}
	req, _ := http.NewRequest("POST", "http://x/v1/audio/transcriptions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=fictionalboundary")
	ref, err := matchRequest(req, cfg, meshOf(cfg))
	if err != nil || ref.Local != "agents-a1" {
		t.Fatalf("match = %+v, %v; want agents-a1", ref, err)
	}
	got, _ := io.ReadAll(req.Body)
	if !bytes.Equal(got, body) {
		t.Fatal("backend body differs from the client body")
	}
}

// multipartReqBody encodes ordered fields with a fixed boundary.
func multipartReqBody(t *testing.T, fields ...[2]string) []byte {
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
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A request body that fails mid-read (client gone) is captured as truncated,
// not as a complete body.
func TestBodyCaptureReadErrorMarksTruncated(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"clean end", io.EOF, false},
		{"client gone", io.ErrUnexpectedEOF, true},
	} {
		c := &captureReader{rc: io.NopCloser(io.MultiReader(strings.NewReader("partial"), iotest.ErrReader(tc.err))), max: maxCaptureBody}
		_, _ = io.ReadAll(c)
		if c.truncated != tc.want || c.buf.String() != "partial" {
			t.Errorf("%s: truncated = %v, captured %q; want %v, \"partial\"", tc.name, c.truncated, c.buf.String(), tc.want)
		}
	}
}

// A spool that fails on the temp dir is a server fault even when the request
// was cancelled at the same time: 500 and logged, not a quiet 400.
func TestUploadSpoolFailureWithCancelledContextIs500(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()
	r, h := uploadRouter(t, backend.URL, "")
	var logs strings.Builder
	r.logger = NewLogger(&logs, false)
	t.Setenv("TMPDIR", t.TempDir()+"/missing-spool-dir")

	var body bytes.Buffer
	writeMultipartBody(&body, "spoolcancel", 2<<20)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("POST", "/v1/chat/completions", &body).WithContext(ctx)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=spoolcancel")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(logs.String(), "could not read request body") {
		t.Fatalf("spool fault not logged: %q", logs.String())
	}
}

// countReader counts Read calls so a test can see a wrapper stop reading.
type countReader struct {
	io.Reader
	calls int
}

func (c *countReader) Read(p []byte) (int, error) {
	c.calls++
	return c.Reader.Read(p)
}

// Once a chunked body crosses the cap, every later Read returns the 413 error
// without reading the client body again.
func TestBodyLimitBodyLatchesOverCap(t *testing.T) {
	src := &countReader{Reader: strings.NewReader("0123456789")}
	l := &limitBody{rc: io.NopCloser(src), n: 4, err: tooLargeError(4)}
	buf := make([]byte, 16)
	if _, err := l.Read(buf); !errors.Is(err, errBodyTooLarge) {
		t.Fatalf("first read err = %v, want errBodyTooLarge", err)
	}
	calls := src.calls
	for i := 0; i < 3; i++ {
		if n, err := l.Read(buf); n != 0 || !errors.Is(err, errBodyTooLarge) {
			t.Fatalf("read %d after cap = %d, %v; want 0, errBodyTooLarge", i, n, err)
		}
	}
	if src.calls != calls {
		t.Fatalf("client body read %d more times after the cap", src.calls-calls)
	}
}

// A request body the backend never read (it answered first) is captured as
// truncated, not as a complete empty body; one read to the end is not.
func TestBodyCaptureUndrainedRequestMarksTruncated(t *testing.T) {
	r, _ := uploadRouter(t, "http://127.0.0.1:1", "")
	for _, drain := range []bool{false, true} {
		req := httptest.NewRequest("POST", "/v1/audio/transcriptions", strings.NewReader("fictional upload"))
		_, done := r.wrapProxy(httptest.NewRecorder(), req, "chat-a", "local")
		if drain {
			_, _ = io.ReadAll(req.Body)
		}
		done()
		c, ok := r.Activity().Capture(r.Activity().List()[0].ID) // newest first
		if !ok {
			t.Fatalf("drain=%v: no capture", drain)
		}
		if c.Truncated == drain {
			t.Fatalf("drain=%v: truncated = %v, want %v", drain, c.Truncated, !drain)
		}
	}
}
