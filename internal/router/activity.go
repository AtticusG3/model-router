package router

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/jpeg"
	_ "image/png"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxActivityEntries = 200
	maxCaptureBody     = 256 * 1024
	maxCaptureBytes    = 8 * 1024 * 1024
	// maxImageBody is the extra buffer kept for an image-generation response so
	// the thumbnail is decoded from the whole body, not the clipped capture.
	// Longer responses are captured truncated with no thumbnail.
	maxImageBody = 32 * 1024 * 1024
	// maxThumbEdge is the long edge of a stored thumbnail, in pixels.
	maxThumbEdge = 160
)

var sensitiveHeaders = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"cookie":              true,
	"set-cookie":          true,
	"x-api-key":           true,
	"x-auth-token":        true,
	"api-key":             true,
}

// TokenStats is prompt/generation throughput parsed from an upstream body.
type TokenStats struct {
	PromptTokens     int     `json:"prompt_tokens,omitempty"`
	CachedTokens     int     `json:"cached_tokens,omitempty"`
	CompletionTokens int     `json:"completion_tokens,omitempty"`
	PromptPerSecond  float64 `json:"prompt_per_second,omitempty"`
	TokensPerSecond  float64 `json:"tokens_per_second,omitempty"`
	PromptMs         float64 `json:"prompt_ms,omitempty"`
}

// ActivityEntry is one proxied generation (or other model) request.
type ActivityEntry struct {
	ID          int       `json:"id"`
	Timestamp   time.Time `json:"timestamp"`
	Model       string    `json:"model"`
	Target      string    `json:"target,omitempty"`
	Method      string    `json:"method"`
	Path        string    `json:"path"`
	Status      int       `json:"status"`
	ContentType string    `json:"content_type,omitempty"`
	DurationMs  int       `json:"duration_ms"`
	// TTFTMs is milliseconds from request start until the first generated
	// token is written downstream. A finished non-streaming body has no
	// earlier byte to stamp, so it falls back to timings.prompt_ms.
	TTFTMs     int        `json:"ttft_ms,omitempty"`
	Tokens     TokenStats `json:"tokens"`
	HasCapture bool       `json:"has_capture"`
}

// ActivityCapture is the request/response pair for one activity id.
type ActivityCapture struct {
	ID          int                 `json:"id"`
	ReqHeaders  map[string][]string `json:"req_headers"`
	ReqBody     string              `json:"req_body"`
	RespHeaders map[string][]string `json:"resp_headers"`
	RespBody    string              `json:"resp_body"`
	Truncated   bool                `json:"truncated"`
}

type storedCapture struct {
	cap   ActivityCapture
	thumb []byte
	size  int
}

// ActivityLog is an in-memory ring of recent proxied requests plus capped
// request/response captures for the operator Activity page.
type ActivityLog struct {
	mu        sync.Mutex
	nextID    int
	entries   []ActivityEntry
	captures  map[int]storedCapture
	captureSz int
}

func NewActivityLog() *ActivityLog {
	return &ActivityLog{captures: map[int]storedCapture{}}
}

// List returns newest-first copies of recent activity rows.
func (a *ActivityLog) List() []ActivityEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]ActivityEntry, len(a.entries))
	for i := range a.entries {
		out[len(a.entries)-1-i] = a.entries[i]
	}
	return out
}

// Capture returns a copy of the stored request/response for id.
func (a *ActivityLog) Capture(id int) (ActivityCapture, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	sc, ok := a.captures[id]
	if !ok {
		return ActivityCapture{}, false
	}
	return sc.cap, true
}

// Thumb returns the JPEG thumbnail decoded from an image response body, if one
// was captured for id.
func (a *ActivityLog) Thumb(id int) ([]byte, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	sc, ok := a.captures[id]
	if !ok || len(sc.thumb) == 0 {
		return nil, false
	}
	return sc.thumb, true
}

func (a *ActivityLog) dropOldestCapture() {
	if len(a.captures) == 0 {
		return
	}
	for _, e := range a.entries {
		if sc, ok := a.captures[e.ID]; ok {
			a.captureSz -= sc.size
			delete(a.captures, e.ID)
			return
		}
	}
	for id, sc := range a.captures {
		a.captureSz -= sc.size
		delete(a.captures, id)
		return
	}
}

func (a *ActivityLog) record(entry ActivityEntry, cap ActivityCapture, thumb []byte) ActivityEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.nextID++
	entry.ID = a.nextID
	cap.ID = entry.ID
	size := len(cap.ReqBody) + len(cap.RespBody) + len(thumb)
	for a.captureSz+size > maxCaptureBytes && len(a.captures) > 0 {
		a.dropOldestCapture()
	}
	if size > 0 && a.captureSz+size <= maxCaptureBytes {
		entry.HasCapture = true
		a.captures[entry.ID] = storedCapture{cap: cap, thumb: thumb, size: size}
		a.captureSz += size
	}
	a.entries = append(a.entries, entry)
	if len(a.entries) > maxActivityEntries {
		evicted := a.entries[0]
		a.entries = a.entries[1:]
		if sc, ok := a.captures[evicted.ID]; ok {
			a.captureSz -= sc.size
			delete(a.captures, evicted.ID)
		}
	}
	return entry
}

type activityWriter struct {
	http.ResponseWriter
	status    int
	wroteHead bool
	body      bytes.Buffer
	max       int
	truncated bool
	started   time.Time
	ttftMs    int
	ttftSet   bool
	scan      bytes.Buffer
	// img keeps a full copy of an image response (up to imgMax) so the
	// thumbnail can be decoded before the capture body is clipped.
	img    bytes.Buffer
	imgMax int
}

func (w *activityWriter) WriteHeader(code int) {
	if w.wroteHead {
		return
	}
	w.wroteHead = true
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *activityWriter) Write(p []byte) (int, error) {
	if !w.wroteHead {
		w.WriteHeader(http.StatusOK)
	}
	w.noteFirstToken(p)
	if w.imgMax > w.img.Len() {
		n := w.imgMax - w.img.Len()
		if n > len(p) {
			n = len(p)
		}
		w.img.Write(p[:n])
	}
	remain := w.max - w.body.Len()
	switch {
	case remain <= 0:
		w.truncated = true
	case len(p) > remain:
		w.body.Write(p[:remain])
		w.truncated = true
	default:
		w.body.Write(p)
	}
	return w.ResponseWriter.Write(p)
}

func (w *activityWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *activityWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *activityWriter) noteFirstToken(p []byte) {
	if w.ttftSet || len(p) == 0 || w.started.IsZero() {
		return
	}
	w.scan.Write(p)
	buf := w.scan.Bytes()
	if !streamingBody(w.Header().Get("Content-Type"), buf) || !hasGeneratedToken(buf) {
		trimScan(&w.scan)
		return
	}
	w.ttftMs = roundMs(float64(time.Since(w.started).Milliseconds()))
	if w.ttftMs < 1 {
		w.ttftMs = 1
	}
	w.ttftSet = true
	w.scan.Reset()
}

func trimScan(buf *bytes.Buffer) {
	const maxScan = 8192
	const keep = 512
	if buf.Len() <= maxScan {
		return
	}
	tail := append([]byte(nil), buf.Bytes()[buf.Len()-keep:]...)
	buf.Reset()
	buf.Write(tail)
}

func streamingBody(contentType string, buf []byte) bool {
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		return true
	}
	return bytes.HasPrefix(bytes.TrimSpace(buf), []byte("data:"))
}

func hasGeneratedToken(buf []byte) bool {
	for _, key := range []string{"content", "text", "reasoning_content", "reasoning"} {
		if jsonStringFieldNonEmpty(buf, key) {
			return true
		}
	}
	return false
}

func jsonStringFieldNonEmpty(buf []byte, key string) bool {
	pat := []byte(`"` + key + `"`)
	from := 0
	for from < len(buf) {
		rel := bytes.Index(buf[from:], pat)
		if rel < 0 {
			return false
		}
		i := from + rel + len(pat)
		i = skipSpace(buf, i)
		if i >= len(buf) || buf[i] != ':' {
			from = i
			continue
		}
		i = skipSpace(buf, i+1)
		if i >= len(buf) || buf[i] != '"' {
			if i < len(buf) {
				from = i + 1
			} else {
				from = i
			}
			continue
		}
		i++
		if i >= len(buf) {
			return false
		}
		if buf[i] != '"' {
			return true
		}
		from = i + 1
	}
	return false
}

func skipSpace(buf []byte, i int) int {
	for i < len(buf) {
		switch buf[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

func roundMs(v float64) int {
	if v <= 0 {
		return 0
	}
	ms := int(math.Round(v))
	if ms < 1 {
		return 1
	}
	return ms
}

func copyHeaderMap(h http.Header) map[string][]string {
	out := make(map[string][]string, len(h))
	for k, vals := range h {
		if sensitiveHeaders[strings.ToLower(k)] {
			out[k] = []string{"[redacted]"}
			continue
		}
		cp := make([]string, len(vals))
		copy(cp, vals)
		out[k] = cp
	}
	return out
}

func clipBody(b []byte) (string, bool) {
	if len(b) <= maxCaptureBody {
		return string(b), false
	}
	return string(b[:maxCaptureBody]), true
}

// thumbFromImageResponse returns a downscaled JPEG thumbnail decoded from the
// first image in an image-generation response — sd.cpp's images[] or the
// OpenAI data[].b64_json. It returns nil when the body holds no image.
func thumbFromImageResponse(body []byte) []byte {
	if len(body) == 0 {
		return nil
	}
	var obj struct {
		Images []string `json:"images"`
		Data   []struct {
			B64JSON string `json:"b64_json"`
			URL     string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil
	}
	var enc string
	for _, s := range obj.Images {
		if s != "" {
			enc = s
			break
		}
	}
	if enc == "" {
		for _, d := range obj.Data {
			if d.B64JSON != "" {
				enc = d.B64JSON
				break
			}
			if d.URL != "" {
				enc = d.URL
				break
			}
		}
	}
	if i := strings.Index(enc, ";base64,"); i >= 0 {
		enc = enc[i+len(";base64,"):]
	}
	if enc == "" {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return nil
	}
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, downscale(src, maxThumbEdge), &jpeg.Options{Quality: 80}); err != nil {
		return nil
	}
	return buf.Bytes()
}

// downscale returns src scaled so its long edge is at most max pixels, sampling
// the nearest source pixel.
// shortcut: nearest-neighbour, swap in a box filter if thumbnails look aliased.
func downscale(src image.Image, max int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= max && h <= max {
		return src
	}
	nw, nh := max, max
	if w >= h {
		nh = h * max / w
	} else {
		nw = w * max / h
	}
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y := range nh {
		for x := range nw {
			dst.Set(x, y, src.At(b.Min.X+x*w/nw, b.Min.Y+y*h/nh))
		}
	}
	return dst
}

func asInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	case string:
		i, _ := strconv.Atoi(n)
		return i
	default:
		return 0
	}
}

func asFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	default:
		return 0
	}
}

func mergeTokenStats(dst, src TokenStats) TokenStats {
	if src.PromptTokens != 0 {
		dst.PromptTokens = src.PromptTokens
	}
	if src.CachedTokens != 0 {
		dst.CachedTokens = src.CachedTokens
	}
	if src.CompletionTokens != 0 {
		dst.CompletionTokens = src.CompletionTokens
	}
	if src.PromptPerSecond != 0 {
		dst.PromptPerSecond = src.PromptPerSecond
	}
	if src.TokensPerSecond != 0 {
		dst.TokensPerSecond = src.TokensPerSecond
	}
	if src.PromptMs != 0 {
		dst.PromptMs = src.PromptMs
	}
	return dst
}

func statsFromMap(obj map[string]any) TokenStats {
	var s TokenStats
	if u, ok := obj["usage"].(map[string]any); ok {
		s.PromptTokens = asInt(u["prompt_tokens"])
		s.CompletionTokens = asInt(u["completion_tokens"])
		if d, ok := u["prompt_tokens_details"].(map[string]any); ok {
			s.CachedTokens = asInt(d["cached_tokens"])
		}
	}
	if t, ok := obj["timings"].(map[string]any); ok {
		if s.PromptTokens == 0 {
			s.PromptTokens = asInt(t["prompt_n"])
		}
		if s.CompletionTokens == 0 {
			s.CompletionTokens = asInt(t["predicted_n"])
		}
		s.PromptPerSecond = asFloat(t["prompt_per_second"])
		s.TokensPerSecond = asFloat(t["predicted_per_second"])
		s.PromptMs = asFloat(t["prompt_ms"])
		if cached := asInt(t["cache_n"]); cached != 0 {
			s.CachedTokens = cached
		}
	}
	return s
}

func parseJSONStats(body []byte) TokenStats {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return TokenStats{}
	}
	return statsFromMap(obj)
}

func parseSSEStats(body []byte) TokenStats {
	var s TokenStats
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[5:])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal(payload, &obj); err != nil {
			continue
		}
		s = mergeTokenStats(s, statsFromMap(obj))
	}
	return s
}

func parseTokenStats(body []byte, contentType string) TokenStats {
	ct := strings.ToLower(contentType)
	if strings.Contains(ct, "text/event-stream") || bytes.Contains(body, []byte("data:")) {
		if s := parseSSEStats(body); s != (TokenStats{}) {
			return s
		}
	}
	return parseJSONStats(body)
}

// generationPathSuffixes is the allow-list of POST endpoints that run a model.
// Anything else (metrics, assets, /mcp, /props, /slots, tokenize) stays out of
// the activity ring by default.
var generationPathSuffixes = []string{
	"/chat/completions", "/completions", "/completion", "/infill",
	"/embeddings", "/embedding", "/responses", "/messages",
	"/rerank", "/reranking",
	"/images/generations", "/images/edits", "/images/variations",
	"/audio/speech", "/audio/transcriptions", "/audio/translations",
	"/txt2img", "/img2img",
}

// isGenerationRequest reports whether a proxied request is a generation or
// inference call that belongs in the activity log.
func isGenerationRequest(method, path string) bool {
	if method != http.MethodPost {
		return false
	}
	for _, suffix := range generationPathSuffixes {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}

// imagePathSuffixes are the generation endpoints whose response body carries a
// rendered image; their responses get the extra buffer for a thumbnail.
var imagePathSuffixes = []string{
	"/images/generations", "/images/edits", "/images/variations",
	"/txt2img", "/img2img",
}

// isImageRequest reports whether a proxied request renders an image.
func isImageRequest(path string) bool {
	for _, suffix := range imagePathSuffixes {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}

func (r *Router) recordProxy(req *http.Request, rec *activityWriter, started time.Time, reqBody []byte, reqTrunc bool, model, target string) {
	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}
	ct := rec.Header().Get("Content-Type")
	tokens := parseTokenStats(rec.body.Bytes(), ct)
	ttft := rec.ttftMs
	if !rec.ttftSet {
		ttft = roundMs(tokens.PromptMs)
	}
	entry := ActivityEntry{
		Timestamp:   started,
		Model:       model,
		Target:      target,
		Method:      req.Method,
		Path:        req.URL.Path,
		Status:      status,
		ContentType: ct,
		DurationMs:  int(time.Since(started).Milliseconds()),
		TTFTMs:      ttft,
		Tokens:      tokens,
	}
	// Metrics see every proxied request, even when the activity ring is
	// disabled or filters the row out.
	if r.metrics != nil {
		r.metrics.Observe(entry)
	}
	if r.activity == nil || !isGenerationRequest(req.Method, req.URL.Path) {
		return
	}
	reqStr, clipped := clipBody(reqBody)
	respStr := rec.body.String()
	r.activity.record(entry, ActivityCapture{
		ReqHeaders:  copyHeaderMap(req.Header),
		ReqBody:     reqStr,
		RespHeaders: copyHeaderMap(rec.Header()),
		RespBody:    respStr,
		Truncated:   reqTrunc || clipped || rec.truncated,
	}, thumbFromImageResponse(rec.img.Bytes()))
}

func (r *Router) wrapProxy(w http.ResponseWriter, req *http.Request, model, target string) (http.ResponseWriter, func()) {
	if r.activity == nil {
		return w, func() {}
	}
	reqBody, err := io.ReadAll(req.Body)
	if err != nil {
		reqBody = nil
	}
	req.Body = io.NopCloser(bytes.NewReader(reqBody))
	reqTrunc := false
	captureReq := reqBody
	if len(captureReq) > maxCaptureBody {
		captureReq = captureReq[:maxCaptureBody]
		reqTrunc = true
	}
	started := time.Now()
	rec := &activityWriter{ResponseWriter: w, max: maxCaptureBody, started: started}
	if isImageRequest(req.URL.Path) {
		rec.imgMax = maxImageBody
	}
	return rec, func() {
		r.recordProxy(req, rec, started, captureReq, reqTrunc, model, target)
	}
}
