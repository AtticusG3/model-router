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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBlockedCardFixtures(t *testing.T) {
	for _, tc := range []struct {
		dir   string
		want  bool
		count int
	}{
		{"testdata/blocked", true, 7},
		{"testdata/good", false, 3},
	} {
		entries, err := os.ReadDir(tc.dir)
		if err != nil {
			t.Fatalf("read %s: %v", tc.dir, err)
		}
		if len(entries) != tc.count {
			t.Fatalf("%s has %d fixtures, want %d", tc.dir, len(entries), tc.count)
		}
		for _, e := range entries {
			if filepath.Ext(e.Name()) != ".png" {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(tc.dir, e.Name()))
			if err != nil {
				t.Fatalf("read %s: %v", e.Name(), err)
			}
			if got := blockedCardPNG(raw); got != tc.want {
				t.Errorf("%s: blocked = %v, want %v", e.Name(), got, tc.want)
			}
		}
	}
}

func TestBlockedCardSynthetic(t *testing.T) {
	grey := pngPixels(t, 32, 32, func(_, _ int) (uint8, uint8, uint8) { return 128, 128, 128 })
	if !blockedCardPNG(grey) {
		t.Fatal("flat grey card not flagged")
	}
	// A low-saturation photo-like gradient spans the luminance range, so it
	// never concentrates in the top bins.
	gradient := pngPixels(t, 256, 256, func(_, y int) (uint8, uint8, uint8) {
		v := uint8(y)
		return v, v, v
	})
	if blockedCardPNG(gradient) {
		t.Fatal("photo-like gradient flagged as a blocked card")
	}
}

func TestBlockedCardLineDrawing(t *testing.T) {
	// Black ink on white paper is desaturated and mostly white, so the bin
	// share passes; the fullest bin is at the top edge, which rules it out.
	drawing := pngPixels(t, 256, 256, func(x, y int) (uint8, uint8, uint8) {
		if (x > 40 && x < 216 && y > 100 && y < 105) || (y > 40 && y < 160 && x > 100 && x < 105) {
			return 0, 0, 0
		}
		return 255, 255, 255
	})
	if blockedCardPNG(drawing) {
		t.Fatal("white-background line drawing flagged as a blocked card")
	}
}

// Full-size attachments measured with the same sampling and binning as
// blockedCardStats; the committed 256x256 copies track them within 0.04. The
// attachments themselves are not committed, so the full-size leg runs only when
// MODEL_ROUTER_FULL_FIXTURES names their directory.
func TestBlockedCardShares(t *testing.T) {
	fullDir := os.Getenv("MODEL_ROUTER_FULL_FIXTURES")
	if fullDir == "" {
		t.Log("MODEL_ROUTER_FULL_FIXTURES unset; skipping full-size leg")
	}
	fixtures := []struct {
		name    string
		blocked bool
		share   [2]float64 // committed 256x256, full size
	}{
		{"plain-1.png", true, [2]float64{0.979, 0.984}},
		{"plain-2.png", true, [2]float64{0.979, 0.986}},
		{"plain-3.png", true, [2]float64{0.979, 0.984}},
		{"plain-4.png", true, [2]float64{0.979, 0.984}},
		{"plain-5.png", true, [2]float64{0.979, 0.985}},
		{"enh-1.png", true, [2]float64{0.575, 0.599}},
		{"enh-3.png", true, [2]float64{0.979, 0.986}},
		{"enh-2.png", false, [2]float64{0.374, 0.394}},
		{"enh-4.png", false, [2]float64{0.263, 0.223}},
		{"enh-5.png", false, [2]float64{0.389, 0.399}},
	}
	for _, f := range fixtures {
		dir := "testdata/good"
		if f.blocked {
			dir = "testdata/blocked"
		}
		for _, src := range []struct {
			label string
			path  string
			share float64
		}{
			{"256x256", filepath.Join(dir, f.name), f.share[0]},
			{"full", filepath.Join(fullDir, f.name), f.share[1]},
		} {
			if src.label == "full" && fullDir == "" {
				continue
			}
			mean, fullest, share, ok := blockedCardStats(decodePNG(t, readFixture(t, src.path)))
			if !ok {
				t.Fatalf("%s %s: no samples", f.name, src.label)
			}
			t.Logf("%s %s: mean chroma %.1f, fullest bin %d, top-3 share %.3f", f.name, src.label, mean, fullest, share)
			if share < src.share-0.02 || share > src.share+0.02 {
				t.Errorf("%s %s: top-3 share %.3f, want %.3f", f.name, src.label, share, src.share)
			}
			midRange := fullest >= blockedEdgeBins && fullest < blockedBinCount-blockedEdgeBins
			if blocked := mean <= blockedMeanChroma && share >= blockedTopShare && midRange; blocked != f.blocked {
				t.Errorf("%s %s: blocked = %v, want %v", f.name, src.label, blocked, f.blocked)
			}
		}
	}
}

func TestBlockedCardDecodeTime(t *testing.T) {
	// A real card is 1024x1024; the stride cap must keep decode plus sampling
	// well inside the 100 ms budget.
	if got := blockedSampleStride(1024, 1024); got != 4 {
		t.Fatalf("1024x1024 stride = %d, want 4", got)
	}
	card := pngFromImage(t, scaledPNG(decodePNG(t, readFixture(t, "testdata/blocked/plain-1.png")), 1024))
	start := time.Now()
	if !blockedCardPNG(card) {
		t.Fatal("1024x1024 card not flagged")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("1024x1024 detection took %v, want under 100 ms", elapsed)
	} else {
		t.Logf("1024x1024 decode and sampling took %v", elapsed)
	}
}

func BenchmarkBlockedCardPNG(b *testing.B) {
	card := pngFromImage(b, scaledPNG(decodePNG(b, readFixture(b, "testdata/blocked/plain-1.png")), 1024))
	b.ResetTimer()
	for range b.N {
		if !blockedCardPNG(card) {
			b.Fatal("1024x1024 card not flagged")
		}
	}
}

func TestBlockedCardResponse(t *testing.T) {
	card := pngPixels(t, 32, 32, func(_, _ int) (uint8, uint8, uint8) { return 128, 128, 128 })
	body := fmt.Sprintf(`{"images":["%s"]}`, base64.StdEncoding.EncodeToString(card))
	if !blockedCardResponse([]byte(body)) {
		t.Fatal("blocked-card reply not flagged")
	}
	if blockedCardResponse([]byte(`not json`)) {
		t.Fatal("malformed reply flagged")
	}
}

func TestImageBlockedRetry(t *testing.T) {
	card := readFixture(t, "testdata/blocked/plain-1.png")
	good := readFixture(t, "testdata/good/enh-2.png")
	for _, tc := range []struct {
		name       string
		pipeline   string
		blocked    int
		wantTries  int
		wantHeader string
	}{
		{"one retry", `{retry_blocked: 1}`, 1, 2, "1"},
		{"two retries", `{retry_blocked: 2}`, 2, 3, "2"},
		{"exhausted", `{retry_blocked: 1}`, 5, 2, "1"},
		{"disabled", `sd-server`, 1, 1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seeds []int64
			attempts := 0
			r := imageTestRouterPipeline(t, imageBackend(t, func(w http.ResponseWriter, req *http.Request) {
				attempts++
				var in struct {
					Seed int64 `json:"seed"`
				}
				_ = json.NewDecoder(req.Body).Decode(&in)
				seeds = append(seeds, in.Seed)
				w.Header().Set("Content-Type", "application/json")
				img := good
				if attempts <= tc.blocked {
					img = card
				}
				fmt.Fprintf(w, `{"images":["%s"]}`, base64.StdEncoding.EncodeToString(img))
			}), tc.pipeline)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/sdapi/v1/txt2img", strings.NewReader(`{"prompt":"cat"}`))
			req.Header.Set("Content-Type", "application/json")
			NewHandler(r, NewLogger(io.Discard, false)).ServeHTTP(rec, req)

			if attempts != tc.wantTries {
				t.Fatalf("backend attempts = %d, want %d", attempts, tc.wantTries)
			}
			if got := rec.Header().Get("X-SD-Shim-Blocked-Retries"); got != tc.wantHeader {
				t.Fatalf("retry header = %q, want %q", got, tc.wantHeader)
			}
			want := base64.StdEncoding.EncodeToString(good)
			if tc.blocked >= tc.wantTries && !strings.Contains(rec.Body.String(), base64.StdEncoding.EncodeToString(card)) {
				t.Fatal("client did not get the final card image")
			}
			if tc.blocked < tc.wantTries && !strings.Contains(rec.Body.String(), want) {
				t.Fatal("client did not get the retried image")
			}
			for i := 1; i < len(seeds); i++ {
				if seeds[i] == seeds[i-1] || seeds[i] <= 0 {
					t.Fatalf("retry seeds = %v, want a fresh seed each retry", seeds)
				}
			}
		})
	}
}

func readFixture(t testing.TB, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return raw
}

func decodePNG(t testing.TB, raw []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode png: %v", err)
	}
	return img
}

func pngFromImage(t testing.TB, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func scaledPNG(img image.Image, size int) image.Image {
	src := img.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := range size {
		for x := range size {
			dst.Set(x, y, img.At(src.Min.X+x*src.Dx()/size, src.Min.Y+y*src.Dy()/size))
		}
	}
	return dst
}

func pngPixels(t testing.TB, w, h int, fill func(x, y int) (uint8, uint8, uint8)) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			r, g, b := fill(x, y)
			i := img.PixOffset(x, y)
			img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = r, g, b, 255
		}
	}
	return pngFromImage(t, img)
}
