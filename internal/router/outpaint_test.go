package router

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOutpaintMarginsExplicit(t *testing.T) {
	cases := []struct {
		name                       string
		spec                       Outpaint
		wantT, wantB, wantL, wantR int
	}{
		{"one side", Outpaint{Top: 64}, 64, 0, 0, 0},
		{"four sides", Outpaint{Top: 8, Bottom: 16, Left: 32, Right: 64}, 8, 16, 32, 64},
		{"odd size", Outpaint{Left: 1, Right: 2}, 0, 0, 1, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			top, bottom, left, right, err := tc.spec.Margins(101, 77)
			if err != nil {
				t.Fatalf("Margins: %v", err)
			}
			if top != tc.wantT || bottom != tc.wantB || left != tc.wantL || right != tc.wantR {
				t.Fatalf("got %d/%d/%d/%d, want %d/%d/%d/%d", top, bottom, left, right,
					tc.wantT, tc.wantB, tc.wantL, tc.wantR)
			}
		})
	}
}

func TestOutpaintMarginsPresets(t *testing.T) {
	// 100x50 source: 2x is 200x100 (50 top, 50 bottom, 50 left, 50 right);
	// 1.5x is 150x75, which has an odd growth on both axes.
	cases := []struct {
		name                       string
		preset                     string
		wantT, wantB, wantL, wantR int
	}{
		{"2x", "2x", 25, 25, 50, 50},
		{"1.5x odd growth", "1.5x", 12, 13, 25, 25},
		{"1:1 from landscape", "1:1", 25, 25, 0, 0},
		{"9:16 from landscape", "9:16", 71, 71, 4, 4},
		{"16:9 from square", "16:9", 4, 4, 46, 46},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, h := 100, 50
			if tc.name == "16:9 from square" {
				w, h = 100, 100
			}
			top, bottom, left, right, err := Outpaint{Preset: tc.preset}.Margins(w, h)
			if err != nil {
				t.Fatalf("Margins(%s): %v", tc.preset, err)
			}
			if top != tc.wantT || bottom != tc.wantB || left != tc.wantL || right != tc.wantR {
				t.Fatalf("%s on %dx%d: got %d/%d/%d/%d, want %d/%d/%d/%d", tc.preset, w, h,
					top, bottom, left, right, tc.wantT, tc.wantB, tc.wantL, tc.wantR)
			}
		})
	}
}

func TestOutpaintPresetKeepsAspect(t *testing.T) {
	// A 16:9 target from 100x100 must land exactly on 16:9, not near it.
	top, bottom, left, right, err := Outpaint{Preset: "16:9"}.Margins(100, 100)
	if err != nil {
		t.Fatalf("Margins: %v", err)
	}
	nw, nh := 100+left+right, 100+top+bottom
	if nw*9 != nh*16 {
		t.Fatalf("%dx%d is not 16:9", nw, nh)
	}
}

func TestOutpaintMarginsRejects(t *testing.T) {
	cases := map[string]Outpaint{
		"empty":         {},
		"negative":      {Top: -1},
		"preset+px":     {Preset: "2x", Top: 4},
		"bad preset":    {Preset: "huge"},
		"scale too big": {Preset: "9x"},
	}
	for name, spec := range cases {
		if _, _, _, _, err := spec.Margins(64, 64); err == nil {
			t.Fatalf("%s: expected a rejection", name)
		}
	}
	// Caps: a single side over the pad cap, and a canvas over the side cap.
	if _, _, _, _, err := (Outpaint{Top: outpaintMaxPad + 1}).Margins(64, 64); err == nil {
		t.Fatal("expected the pad cap to reject")
	}
	if _, _, _, _, err := (Outpaint{Top: 4000, Bottom: 4000}).Margins(6000, 6000); err == nil {
		t.Fatal("expected the side cap to reject")
	}
	// No source size is a rejection, not a panic.
	if _, _, _, _, err := (Outpaint{Top: 8}).Margins(0, 0); err == nil {
		t.Fatal("expected a zero-size source to reject")
	}
}

func TestPadAndMaskGeometry(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 5, 3))
	for y := 0; y < 3; y++ {
		for x := 0; x < 5; x++ {
			src.Set(x, y, color.RGBA{10, 20, 30, 255})
		}
	}
	padded, mask := padAndMask(src, 2, 1, 4, 0)
	if got := padded.Bounds(); got.Dx() != 9 || got.Dy() != 6 {
		t.Fatalf("padded size %v, want 9x6", got)
	}
	// Every pixel outside the source rect is the grey fill, every pixel inside
	// is the source colour.
	for y := 0; y < 6; y++ {
		for x := 0; x < 9; x++ {
			inSrc := x >= 4 && x < 9 && y >= 2 && y < 5
			r, _, _, _ := padded.At(x, y).RGBA()
			r8 := uint8(r >> 8)
			if inSrc && r8 != 10 {
				t.Fatalf("source pixel (%d,%d) = %d, want 10", x, y, r8)
			}
			if !inSrc && r8 != outpaintFill {
				t.Fatalf("fill pixel (%d,%d) = %d, want %d", x, y, r8, outpaintFill)
			}
			_, isWhite := mask.At(x, y).(color.Gray)
			if !isWhite {
				t.Fatalf("mask pixel (%d,%d) is not gray", x, y)
			}
			wantMask := uint8(0)
			if !inSrc {
				wantMask = 255
			}
			if got := mask.GrayAt(x, y).Y; got != wantMask {
				t.Fatalf("mask (%d,%d) = %d, want %d", x, y, got, wantMask)
			}
		}
	}
}

func TestOutpaintRewritePadsAndMasks(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 8, 6))
	body := img2imgBody(t, src, `{"outpaint":{"top":4,"bottom":4,"left":2,"right":2}}`)
	req := httptest.NewRequest(http.MethodPost, "/sdapi/v1/img2img", bytes.NewReader(body))
	notes := &imageNotes{}
	if err := outpaintRewrite(req, req.Header, notes); err != nil {
		t.Fatalf("outpaintRewrite: %v", err)
	}
	out := readBody(t, req)
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("reply is not JSON: %v", err)
	}
	if _, still := obj["outpaint"]; still {
		t.Fatal("outpaint field must be removed before the backend sees the body")
	}
	var w, h int
	_ = json.Unmarshal(obj["width"], &w)
	_ = json.Unmarshal(obj["height"], &h)
	if w != 12 || h != 14 {
		t.Fatalf("width/height = %dx%d, want 12x14", w, h)
	}
	var maskB64 string
	if err := json.Unmarshal(obj["mask"], &maskB64); err != nil || maskB64 == "" {
		t.Fatalf("mask missing: %v", err)
	}
	mask, err := png.Decode(base64.NewDecoder(base64.StdEncoding, strings.NewReader(maskB64)))
	if err != nil {
		t.Fatalf("mask is not a PNG: %v", err)
	}
	if mask.Bounds().Dx() != 12 || mask.Bounds().Dy() != 14 {
		t.Fatalf("mask size %v, want 12x14", mask.Bounds())
	}
	grayMask, ok := mask.(*image.Gray)
	if !ok {
		t.Fatal("mask must decode to image.Gray")
	}
	if grayMask.GrayAt(0, 0).Y != 255 {
		t.Fatal("corner must be white (fill area)")
	}
	if grayMask.GrayAt(2, 4).Y != 0 {
		t.Fatal("source centre must be black (kept area)")
	}
	if len(notes.all()) == 0 {
		t.Fatal("the rewrite must note what it did")
	}
	// The padded init must be the padded canvas, not the source.
	var inits []string
	_ = json.Unmarshal(obj["init_images"], &inits)
	if len(inits) != 1 {
		t.Fatalf("init_images len = %d", len(inits))
	}
	padded, err := png.Decode(base64.NewDecoder(base64.StdEncoding, strings.NewReader(inits[0])))
	if err != nil {
		t.Fatalf("padded init is not a PNG: %v", err)
	}
	if padded.Bounds().Dx() != 12 || padded.Bounds().Dy() != 14 {
		t.Fatalf("padded init size %v", padded.Bounds())
	}
}

func TestOutpaintRewriteIgnoresOtherBodies(t *testing.T) {
	// No outpaint field: the body is left byte-identical.
	src := image.NewRGBA(image.Rect(0, 0, 4, 4))
	body := img2imgBody(t, src, "")
	req := httptest.NewRequest(http.MethodPost, "/sdapi/v1/img2img", bytes.NewReader(body))
	if err := outpaintRewrite(req, req.Header, &imageNotes{}); err != nil {
		t.Fatalf("outpaintRewrite: %v", err)
	}
	if got := readBody(t, req); !bytes.Equal(got, body) {
		t.Fatal("a body without outpaint must pass through untouched")
	}
	// txt2img never carries outpaint.
	body2 := img2imgBody(t, src, `{"outpaint":{"top":4}}`)
	req2 := httptest.NewRequest(http.MethodPost, "/sdapi/v1/txt2img", bytes.NewReader(body2))
	if err := outpaintRewrite(req2, req2.Header, &imageNotes{}); err != nil {
		t.Fatalf("outpaintRewrite(txt2img): %v", err)
	}
	if got := readBody(t, req2); !bytes.Equal(got, body2) {
		t.Fatal("txt2img must not be rewritten")
	}
}

func TestOutpaintRewriteRejects(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 4, 4))
	cases := map[string]string{
		"no init":      `{"outpaint":{"top":4}}`,
		"empty source": `{"outpaint":{"top":4},"init_images":[""]}`,
		"bad margins":  `{"outpaint":{"top":-2}}`,
	}
	for name, extra := range cases {
		body := img2imgBody(t, src, extra)
		if name == "no init" {
			var obj map[string]json.RawMessage
			_ = json.Unmarshal(body, &obj)
			delete(obj, "init_images")
			body, _ = json.Marshal(obj)
		}
		req := httptest.NewRequest(http.MethodPost, "/sdapi/v1/img2img", bytes.NewReader(body))
		err := outpaintRewrite(req, req.Header, &imageNotes{})
		if err == nil {
			t.Fatalf("%s: expected a rejection", name)
		}
		if _, ok := err.(imageReject); !ok {
			t.Fatalf("%s: want an imageReject (400), got %T", name, err)
		}
	}
}

// img2imgBody builds an sdapi img2img body with a PNG source of the given size
// and merges the extra JSON object on top.
func img2imgBody(t *testing.T, src image.Image, extra string) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatalf("encode: %v", err)
	}
	obj := map[string]any{
		"init_images": []string{base64.StdEncoding.EncodeToString(buf.Bytes())},
		"prompt":      "a cat",
		"width":       src.Bounds().Dx(),
		"height":      src.Bounds().Dy(),
	}
	out, _ := json.Marshal(obj)
	if extra == "" {
		return out
	}
	var add map[string]json.RawMessage
	if err := json.Unmarshal([]byte(extra), &add); err != nil {
		t.Fatalf("extra JSON: %v", err)
	}
	var base map[string]json.RawMessage
	_ = json.Unmarshal(out, &base)
	for k, v := range add {
		base[k] = v
	}
	merged, _ := json.Marshal(base)
	return merged
}

func readBody(t *testing.T, req *http.Request) []byte {
	t.Helper()
	b, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return b
}
