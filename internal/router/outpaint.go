package router

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/jpeg"
	"image/png"
	"io"
	"net/http"
	"strconv"
	"strings"

	_ "golang.org/x/image/webp"
)

// Outpaint is the request field that asks the router to expand a canvas and
// fill the new area. The client sends either explicit per-side margins in
// pixels or a preset; the router builds the padded image and the mask, so a
// curl client gets exactly what the Playground gets.
//
//	{"outpaint": {"top": 128, "bottom": 128}}
//	{"outpaint": {"preset": "2x"}}
//	{"outpaint": {"preset": "16:9"}}
type Outpaint struct {
	Top    int    `json:"top"`
	Bottom int    `json:"bottom"`
	Left   int    `json:"left"`
	Right  int    `json:"right"`
	Preset string `json:"preset"`
}

const (
	// outpaintMaxSide caps the padded canvas; sd-server tiles the VAE anyway,
	// but a 16k request on a 24 GB card is a mistake worth refusing early.
	outpaintMaxSide = 8192
	// outpaintMaxPad caps one side, so a typo cannot allocate gigabytes.
	outpaintMaxPad = 4096
	// outpaintFill is the padding colour: mid grey, which inpaint models treat
	// as "unknown" rather than as a subject or a background.
	outpaintFill = 128
)

// Margins resolves the request to per-side pixel margins for a w x h source.
// Explicit margins win; otherwise the preset is resolved. Everything is
// validated against the caps above.
func (o Outpaint) Margins(w, h int) (top, bottom, left, right int, err error) {
	if w <= 0 || h <= 0 {
		return 0, 0, 0, 0, imageReject("outpaint needs a decodable source image")
	}
	if o.Top == 0 && o.Bottom == 0 && o.Left == 0 && o.Right == 0 && o.Preset == "" {
		return 0, 0, 0, 0, imageReject("outpaint needs per-side margins or a preset")
	}
	if o.Preset != "" && (o.Top != 0 || o.Bottom != 0 || o.Left != 0 || o.Right != 0) {
		return 0, 0, 0, 0, imageReject("outpaint takes either margins or a preset, not both")
	}
	if o.Top < 0 || o.Bottom < 0 || o.Left < 0 || o.Right < 0 {
		return 0, 0, 0, 0, imageReject("outpaint margins must not be negative")
	}
	top, bottom, left, right = o.Top, o.Bottom, o.Left, o.Right
	if o.Preset != "" {
		top, bottom, left, right, err = presetMargins(w, h, o.Preset)
		if err != nil {
			return 0, 0, 0, 0, err
		}
	}
	if left+right == 0 && top+bottom == 0 {
		return 0, 0, 0, 0, imageReject("outpaint margins add up to zero")
	}
	for side, v := range map[string]int{"top": top, "bottom": bottom, "left": left, "right": right} {
		if v > outpaintMaxPad {
			return 0, 0, 0, 0, imageReject(fmt.Sprintf("outpaint %s %d exceeds the %d px cap", side, v, outpaintMaxPad))
		}
	}
	if w+left+right > outpaintMaxSide || h+top+bottom > outpaintMaxSide {
		return 0, 0, 0, 0, imageReject(fmt.Sprintf("outpaint result %dx%d exceeds the %d px side cap",
			w+left+right, h+top+bottom, outpaintMaxSide))
	}
	return top, bottom, left, right, nil
}

// presetMargins resolves "1.5x", "2x", "1:1", "16:9" and "9:16" (also
// "3:2"/"2:3" style targets) into centred margins. A scale multiplies both
// sides; an aspect target grows the canvas in the direction that reaches that
// ratio without cropping, so the source keeps its pixels.
func presetMargins(w, h int, preset string) (top, bottom, left, right int, err error) {
	p := strings.ToLower(strings.TrimSpace(preset))
	if s, ok := strings.CutSuffix(p, "x"); ok {
		scale, convErr := strconv.ParseFloat(s, 64)
		if convErr != nil || scale <= 1.0 || scale > 8.0 {
			return 0, 0, 0, 0, imageReject(fmt.Sprintf("outpaint preset %q: scale must be above 1 and at most 8", preset))
		}
		nw, nh := int(float64(w)*scale+0.5), int(float64(h)*scale+0.5)
		t, b := splitMarginPaired(nh - h)
		l, r := splitMarginPaired(nw - w)
		return t, b, l, r, nil
	}
	if aw, ah, ok := splitRatio(p); ok && aw > 0 && ah > 0 {
		// Grow to the exact ratio: reduce aw:ah, then take the smallest whole
		// multiple that still covers the source on both axes. Rounding the
		// canvas independently would land near the ratio, not on it.
		g := gcd(aw, ah)
		uw, uh := aw/g, ah/g
		k := max((w+uw-1)/uw, (h+uh-1)/uh)
		if k < 1 {
			k = 1
		}
		nw, nh := k*uw, k*uh
		l, r := splitMarginPaired(nw - w)
		t, b := splitMarginPaired(nh - h)
		return t, b, l, r, nil
	}
	return 0, 0, 0, 0, imageReject(fmt.Sprintf("outpaint preset %q: use 1.5x, 2x or a ratio like 16:9", preset))
}

// splitMarginPaired splits one dimension's growth, putting the odd pixel on
// the far side so the source stays centred to within a pixel.
func splitMarginPaired(d int) (int, int) {
	if d < 0 {
		d = 0
	}
	return d / 2, d - d/2
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func splitRatio(p string) (int, int, bool) {
	aw, ah, ok := strings.Cut(p, ":")
	if !ok {
		return 0, 0, false
	}
	w, err1 := strconv.Atoi(strings.TrimSpace(aw))
	h, err2 := strconv.Atoi(strings.TrimSpace(ah))
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return w, h, true
}

// padAndMask returns the padded image (grey border) and its mask, where white
// marks the area the model must fill and black the pixels it keeps. That is
// sd-server's convention on the sdapi inpaint path.
func padAndMask(src image.Image, top, bottom, left, right int) (*image.RGBA, *image.Gray) {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	nw, nh := w+left+right, h+top+bottom
	padded := image.NewRGBA(image.Rect(0, 0, nw, nh))
	draw.Draw(padded, padded.Bounds(), &image.Uniform{C: color.RGBA{outpaintFill, outpaintFill, outpaintFill, 255}}, image.Point{}, draw.Src)
	draw.Draw(padded, image.Rect(left, top, left+w, top+h), src, b.Min, draw.Src)

	mask := image.NewGray(image.Rect(0, 0, nw, nh))
	white := color.Gray{Y: 255}
	for y := 0; y < nh; y++ {
		for x := 0; x < nw; x++ {
			if y < top || y >= top+h || x < left || x >= left+w {
				mask.SetGray(x, y, white)
			}
		}
	}
	return padded, mask
}

// outpaintRewrite is the image_pipeline step: when the body carries an
// `outpaint` field it pads init_images[0], builds the mask, sets width and
// height to the padded size and removes the field, so the backend only ever
// sees fields it knows.
func outpaintRewrite(req *http.Request, _ http.Header, notes *imageNotes) error {
	if req.Method != http.MethodPost || req.URL.Path != "/sdapi/v1/img2img" || req.Body == nil {
		return nil
	}
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		return err
	}
	req.Body = io.NopCloser(bytes.NewReader(raw))
	var obj map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &obj) != nil || obj == nil {
		return nil
	}
	field, ok := obj["outpaint"]
	if !ok {
		return nil
	}
	var spec Outpaint
	if err := json.Unmarshal(field, &spec); err != nil {
		return imageReject("outpaint must be an object with top/bottom/left/right or preset")
	}
	var inits []string
	if json.Unmarshal(obj["init_images"], &inits) != nil || len(inits) == 0 {
		return imageReject("outpaint needs init_images[0]")
	}
	srcBytes := decodeB64Image(inits[0])
	if srcBytes == nil {
		return imageReject("outpaint needs a decodable PNG/JPEG/WEBP source")
	}
	src, _, err := image.Decode(bytes.NewReader(srcBytes))
	if err != nil {
		return imageReject("outpaint could not decode the source image")
	}
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	top, bottom, left, right, err := spec.Margins(w, h)
	if err != nil {
		return err
	}
	padded, mask := padAndMask(src, top, bottom, left, right)

	var paddedB64, maskB64 string
	if paddedB64, err = encodePNGB64(padded); err != nil {
		return err
	}
	if maskB64, err = encodePNGB64(mask); err != nil {
		return err
	}
	inits[0] = paddedB64
	if obj["init_images"], err = json.Marshal(inits); err != nil {
		return err
	}
	if obj["mask"], err = json.Marshal(maskB64); err != nil {
		return err
	}
	delete(obj, "outpaint")
	// The canvas, not the source, is the generation size.
	if obj["width"], err = json.Marshal(padded.Bounds().Dx()); err != nil {
		return err
	}
	if obj["height"], err = json.Marshal(padded.Bounds().Dy()); err != nil {
		return err
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	req.Body = io.NopCloser(bytes.NewReader(out))
	req.ContentLength = int64(len(out))
	notes.Add(fmt.Sprintf("outpaint: %dx%d -> %dx%d (top %d, bottom %d, left %d, right %d), mask built",
		w, h, padded.Bounds().Dx(), padded.Bounds().Dy(), top, bottom, left, right))
	return nil
}

func encodePNGB64(img image.Image) (string, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}
