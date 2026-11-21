package router

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"model-router/internal/config"
)

// noisePNG is base64 PNG of incompressible noise, so its size tracks w*h*4.
func noisePNG(t *testing.T, w, h int) string {
	t.Helper()
	src := image.NewNRGBA(image.Rect(0, 0, w, h))
	rnd := rand.New(rand.NewPCG(3, 4))
	for i := range src.Pix {
		src.Pix[i] = byte(rnd.IntN(256))
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// previewRouter is router-a with its digger peer answering every request with body.
func previewRouter(t *testing.T, body string) (*Router, http.Handler) {
	t.Helper()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/_router/load" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(peer.Close)
	cfg, err := config.Load("../../test/router-a.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cfg.Peer("digger").BaseURL = peer.URL
	logger := NewLogger(io.Discard, false)
	r := New(cfg, logger, "router-a")
	return r, NewHandler(r, logger)
}

// getPreview fetches an image preview and returns its status, type and decoded bounds.
func getPreview(t *testing.T, h http.Handler, path string) (int, string, image.Rectangle) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	if rec.Code != http.StatusOK {
		return rec.Code, "", image.Rectangle{}
	}
	img, _, err := image.Decode(rec.Body)
	if err != nil {
		t.Fatalf("%s: decode: %v", path, err)
	}
	return rec.Code, rec.Header().Get("Content-Type"), img.Bounds()
}

// An img2img request with a 1 MB init image and a 1.5 MB image in the response
// both get a JPEG preview, served at the number of their capture placeholder.
func TestActivityImagePreviews(t *testing.T) {
	in := noisePNG(t, 512, 512)
	out := noisePNG(t, 1600, 240)
	if len(in) < 1<<20 || len(out) < 3<<19 {
		t.Fatalf("test images %d and %d bytes, want 1 MB and 1.5 MB", len(in), len(out))
	}
	r, h := previewRouter(t, `{"images":["`+out+`"],"parameters":{}}`)

	req := httptest.NewRequest("POST", "/sdapi/v1/img2img", strings.NewReader(`{"model":"digger/remote-model","prompt":"a cat","init_images":["`+in+`"]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%.200s", rec.Code, rec.Body.String())
	}
	cap, ok := r.Activity().Capture(1)
	if !ok || !strings.Contains(cap.ReqBody, `"init_images":["[image 0: png `) || !strings.Contains(cap.RespBody, `"images":["[image 0: png `) {
		t.Fatalf("capture = %v, want both images clipped to [image 0] placeholders", ok)
	}

	code, ct, b := getPreview(t, h, "/_router/activity/1/image/req/0")
	if code != http.StatusOK || ct != "image/jpeg" || b.Dx() != 512 || b.Dy() != 512 {
		t.Fatalf("request preview = %d %q %v, want a 512x512 JPEG", code, ct, b)
	}
	code, ct, b = getPreview(t, h, "/_router/activity/1/image/resp/0")
	if code != http.StatusOK || ct != "image/jpeg" || b.Dx() != maxPreviewEdge || b.Dy() > maxPreviewEdge {
		t.Fatalf("response preview = %d %q %v, want a JPEG with a %d px long edge", code, ct, b, maxPreviewEdge)
	}
	for path, want := range map[string]int{
		"/_router/activity/1/image/resp/1": http.StatusNotFound,
		"/_router/activity/2/image/req/0":  http.StatusNotFound,
		"/_router/activity/1/image/both/0": http.StatusBadRequest,
		"/_router/activity/1/image/req/x":  http.StatusBadRequest,
	} {
		if code, _, _ := getPreview(t, h, path); code != want {
			t.Errorf("%s = %d, want %d", path, code, want)
		}
	}
}

// A chat request with an image_url data URL gets a request-side preview.
func TestActivityChatImagePreview(t *testing.T) {
	_, h := previewRouter(t, `{"choices":[{"message":{"content":"a cat"}}]}`)
	body := `{"model":"digger/remote-model","messages":[{"role":"user","content":[{"type":"text","text":"what is this?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,` + noisePNG(t, 32, 32) + `"}}]}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	code, ct, b := getPreview(t, h, "/_router/activity/1/image/req/0")
	if code != http.StatusOK || ct != "image/jpeg" || b.Dx() != 32 {
		t.Fatalf("chat preview = %d %q %v, want a 32 px JPEG", code, ct, b)
	}
}

// Previews are shed before any capture, oldest first, and the ring never
// holds more than maxCaptureBytes.
func TestActivityPreviewsShedBeforeCaptures(t *testing.T) {
	a := NewActivityLog()
	big := preview{make([]byte, 1<<20), "image/jpeg"}
	check := func() {
		t.Helper()
		total := 0
		for _, sc := range a.captures {
			total += sc.size
		}
		if total != a.captureSz || total > maxCaptureBytes {
			t.Fatalf("ring holds %d bytes, counted %d, budget %d", total, a.captureSz, maxCaptureBytes)
		}
	}
	for range 30 {
		e := a.record(ActivityEntry{Model: "demo"}, ActivityCapture{ReqBody: strings.Repeat("x", 100<<10)}, nil, "")
		a.addPreviews(e.ID, [2][]preview{{big}, {big, big}})
		check()
	}
	if len(a.captures) != 30 {
		t.Fatalf("captures = %d, want all 30 kept with previews shed instead", len(a.captures))
	}
	if _, _, ok := a.Preview(30, 1, 1); !ok {
		t.Fatal("newest row lost its previews")
	}
	if _, _, ok := a.Preview(1, 0, 0); ok {
		t.Fatal("oldest row kept its previews over the budget")
	}
	// A large body sheds the remaining previews rather than any capture.
	a.record(ActivityEntry{Model: "demo"}, ActivityCapture{ReqBody: strings.Repeat("x", 5<<20)}, nil, "")
	check()
	if _, _, ok := a.Preview(30, 1, 1); ok || len(a.captures) != 31 {
		t.Fatalf("captures = %d, previews kept %v; want 31 captures and no previews", len(a.captures), ok)
	}
}
