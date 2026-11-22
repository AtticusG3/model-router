package router

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestOutpaintOnStanzaWithoutPipeline is the end-to-end contract: a stanza with
// no image_pipeline still gets the outpaint rewrite, because the Playground must
// work on every image model, not only the ones carrying a pipeline (#200).
func TestOutpaintOnStanzaWithoutPipeline(t *testing.T) {
	var got map[string]json.RawMessage
	backend := imageBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"images":[]}`)
	})
	mux, ok := backend.(*http.ServeMux)
	if !ok {
		t.Fatal("imageBackend is not a mux")
	}
	mux.HandleFunc("/sdapi/v1/img2img", func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		got = nil
		_ = json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"images":[]}`)
	})

	r := imageTestRouterPipeline(t, mux, "")
	handler := NewHandler(r, NewLogger(io.Discard, false))

	src := image.NewRGBA(image.Rect(0, 0, 6, 4))
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatalf("encode: %v", err)
	}
	body := fmt.Sprintf(`{"prompt":"a beach","init_images":[%q],"denoising_strength":0.95,"outpaint":{"top":4,"bottom":4,"left":2,"right":2}}`,
		base64.StdEncoding.EncodeToString(buf.Bytes()))
	req := httptest.NewRequest(http.MethodPost, "/sdapi/v1/img2img", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if got == nil {
		t.Fatal("the backend never received the request")
	}
	if _, still := got["outpaint"]; still {
		t.Fatal("the backend saw the outpaint field; the router must strip it")
	}
	var maskB64 string
	if err := json.Unmarshal(got["mask"], &maskB64); err != nil || maskB64 == "" {
		t.Fatalf("the backend saw no mask: %v", err)
	}
	mask, err := png.Decode(base64.NewDecoder(base64.StdEncoding, strings.NewReader(maskB64)))
	if err != nil {
		t.Fatalf("mask is not a PNG: %v", err)
	}
	if mask.Bounds().Dx() != 10 || mask.Bounds().Dy() != 12 {
		t.Fatalf("mask %v, want 10x12", mask.Bounds())
	}
	var w, h int
	_ = json.Unmarshal(got["width"], &w)
	_ = json.Unmarshal(got["height"], &h)
	if w != 10 || h != 12 {
		t.Fatalf("backend width/height = %dx%d, want 10x12", w, h)
	}
	// The padding is the router's, so the init the backend sees is the canvas.
	var inits []string
	_ = json.Unmarshal(got["init_images"], &inits)
	padded, err := png.Decode(base64.NewDecoder(base64.StdEncoding, strings.NewReader(inits[0])))
	if err != nil {
		t.Fatalf("init is not a PNG: %v", err)
	}
	if padded.Bounds().Dx() != 10 || padded.Bounds().Dy() != 12 {
		t.Fatalf("init %v, want the 10x12 canvas", padded.Bounds())
	}
}

// A plain img2img body with no outpaint still reaches the backend, still
// through the pipeline checks, so the rewrite is additive.
func TestImg2imgWithoutOutpaintStillProxies(t *testing.T) {
	var seen string
	backend := imageBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"images":[]}`)
	})
	mux := backend.(*http.ServeMux)
	mux.HandleFunc("/sdapi/v1/img2img", func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		seen = string(body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"images":[]}`)
	})
	r := imageTestRouterPipeline(t, mux, "")
	handler := NewHandler(r, NewLogger(io.Discard, false))

	src := image.NewRGBA(image.Rect(0, 0, 4, 4))
	var buf bytes.Buffer
	_ = png.Encode(&buf, src)
	body := fmt.Sprintf(`{"prompt":"a cat","init_images":[%q],"denoising_strength":0.5}`,
		base64.StdEncoding.EncodeToString(buf.Bytes()))
	req := httptest.NewRequest(http.MethodPost, "/sdapi/v1/img2img", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(seen, `"denoising_strength":0.5`) {
		t.Fatalf("client field lost on the way through: %s", seen)
	}
	if strings.Contains(seen, `"mask"`) {
		t.Fatalf("a body without outpaint must not gain a mask: %s", seen)
	}
}
