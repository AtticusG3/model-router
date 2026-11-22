package router

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"

	"model-router/internal/config"
)

// A safety card is a near-uniform, desaturated image: the block detector
// samples the decoded first image and asks that the mean channel spread be
// small, that the fullest luminance bin be mid-range, that most samples sit in
// a few narrow luminance bins, and that the middle 60% of rows be near-uniform
// too. A plain white or black background, such as line art on white paper,
// fills an extreme bin and fails the mid-range check. A photo with one large
// flat area at the edge, such as an overcast sky over the top half, fails the
// middle-band check. Text and the checkerboard bands on some cards move a
// minority of samples out of those bins, so the bin share thresholds stay below
// one.
const (
	blockedSampleTarget = 1 << 16
	blockedBinCount     = 32
	blockedTopBins      = 3
	blockedEdgeBins     = 3
	blockedMeanChroma   = 16
	blockedTopShare     = 0.5
	blockedMidShare     = 0.9
	blockedSeedMax      = 1<<31 - 1
)

// blockedRetryStep returns the image_pipeline step for retry_blocked: when the
// first image of a txt2img reply is a safety card, resend the request with a
// fresh random seed, at most max times. It sets X-SD-Shim-Blocked-Retries to the
// number of retries actually sent.
func blockedRetryStep(max int) imageStep {
	retries := 0
	return imageStep{
		After: func(req *http.Request, status int, h http.Header, body []byte, _ *imageNotes) bool {
			if retries >= max || req.Method != http.MethodPost || req.URL.Path != "/sdapi/v1/txt2img" {
				return false
			}
			if status != http.StatusOK || !blockedCardResponse(body) {
				return false
			}
			snap := preparedBodyFrom(req)
			if snap == nil || snap.path != "" {
				// shortcut: a txt2img body spooled past bodyMemoryBytes keeps its original body, add a temp-file rewrite if large prompts must retry.
				return false
			}
			next, ok := reseededBody(snap.mem)
			if !ok {
				return false
			}
			snap.replace(next)
			retries++
			h.Set("X-SD-Shim-Blocked-Retries", strconv.Itoa(retries))
			return true
		},
	}
}

// defaultImageSteps builds the steps a stanza's image_pipeline runs: the
// img2img body checks for every preset, the caption step when the stanza's
// caption names a prompt profile, and the blocked-card retry when the stanza
// asks for one.
func (r *Router) defaultImageSteps(s *config.Stanza) []imageStep {
	// Outpaint runs first: it pads init_images[0] and attaches the mask, then
	// checkImg2img validates the padded body and mirrors it into extra_images.
	steps := []imageStep{{Rewrite: outpaintRewrite}, {Rewrite: checkImg2img}}
	if s == nil || s.ImagePipeline == nil {
		return steps
	}
	if prof := promptProfiles[s.ImagePipeline.Caption]; prof != nil {
		steps = append(steps, imageStep{Rewrite: r.captionStep(s.ImagePipeline, prof)})
	}
	if s.ImagePipeline.RetryBlocked > 0 {
		steps = append(steps, blockedRetryStep(s.ImagePipeline.RetryBlocked))
	}
	return steps
}

// blockedCardResponse reports whether the reply's first image is a safety card.
func blockedCardResponse(body []byte) bool {
	var reply struct {
		Images []string `json:"images"`
	}
	if json.Unmarshal(body, &reply) != nil || len(reply.Images) == 0 {
		return false
	}
	data := reply.Images[0]
	if i := strings.IndexByte(data, ','); i >= 0 {
		data = data[i+1:]
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return false
	}
	return blockedCardPNG(raw)
}

// blockedCardPNG reports whether raw is a desaturated PNG with most samples in
// a few mid-luminance bins.
func blockedCardPNG(raw []byte) bool {
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return false
	}
	meanChroma, fullest, topShare, midShare, ok := blockedCardStats(img)
	if !ok || meanChroma > blockedMeanChroma || topShare < blockedTopShare || midShare < blockedMidShare {
		return false
	}
	return fullest >= blockedEdgeBins && fullest < blockedBinCount-blockedEdgeBins
}

// blockedSampleStride returns the pixel stride that keeps the sample count near
// blockedSampleTarget for a w by h image.
func blockedSampleStride(w, h int) int {
	if total := w * h; total > blockedSampleTarget {
		return int(math.Sqrt(float64(total) / blockedSampleTarget))
	}
	return 1
}

// blockedCardStats samples img, returning the mean channel spread, the fullest
// luminance bin, the share of samples in the three fullest bins, and that
// share over the middle 60% of rows.
func blockedCardStats(img image.Image) (meanChroma float64, fullest int, topShare, midShare float64, ok bool) {
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w <= 0 || h <= 0 {
		return 0, 0, 0, 0, false
	}
	step := blockedSampleStride(w, h)
	midMin, midMax := bounds.Min.Y+h/5, bounds.Min.Y+h*4/5

	var bins, midBins [blockedBinCount]int
	samples, midSamples, chromaSum := 0, 0, 0
	for y := bounds.Min.Y; y < bounds.Max.Y; y += step {
		midRow := y >= midMin && y < midMax
		for x := bounds.Min.X; x < bounds.Max.X; x += step {
			r, g, b, _ := img.At(x, y).RGBA()
			r, g, b = r>>8, g>>8, b>>8
			chromaSum += int(max(r, g, b) - min(r, g, b))
			bin := int((r+g+b)/3) / (256 / blockedBinCount)
			bins[bin]++
			if midRow {
				midBins[bin]++
				midSamples++
			}
			samples++
		}
	}
	if samples == 0 || midSamples == 0 {
		return 0, 0, 0, 0, false
	}

	for i := range bins {
		if bins[i] > bins[fullest] {
			fullest = i
		}
	}
	return float64(chromaSum) / float64(samples), fullest,
		blockedBinShare(bins, samples), blockedBinShare(midBins, midSamples), true
}

// blockedBinShare returns the share of samples in the three fullest bins.
func blockedBinShare(bins [blockedBinCount]int, samples int) float64 {
	top := 0
	for range blockedTopBins {
		best := 0
		for i := range bins {
			if bins[i] > bins[best] {
				best = i
			}
		}
		top += bins[best]
		bins[best] = 0
	}
	return float64(top) / float64(samples)
}

// reseededBody replaces the seed field of a txt2img JSON body with a fresh
// random seed, keeping the other fields as raw JSON.
func reseededBody(data []byte) ([]byte, bool) {
	var body map[string]json.RawMessage
	if json.Unmarshal(data, &body) != nil {
		return nil, false
	}
	body["seed"] = json.RawMessage(strconv.FormatInt(rand.Int64N(blockedSeedMax)+1, 10))
	out, err := json.Marshal(body)
	return out, err == nil
}
