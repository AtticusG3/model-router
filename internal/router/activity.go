package router

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"image"
	"image/jpeg"
	_ "image/png"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"model-router/internal/config"
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
	// maxHashBytes is how much of an uncaptured media response is hashed for
	// the Activity row: an identity check, not a checksum of the whole body.
	maxHashBytes = 64 << 10
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
	// RespBytes is the response body size when the body was not captured: a
	// media response keeps only the content type, byte count and BodyHash.
	RespBytes int64 `json:"resp_bytes,omitempty"`
	// BodyHash is a hexadecimal prefix of the media response body hash, so an
	// operator can tell two uncaptured responses apart.
	BodyHash string `json:"body_hash,omitempty"`
	// ThumbIssue says why an image response has no thumbnail — a decode or
	// size failure. Empty when the row is not an image request or a thumbnail
	// was stored, so operators can tell a failure from "not an image".
	ThumbIssue string `json:"thumb_issue,omitempty"`
	// Media replaces the token stats on an audio, video, realtime or voice
	// library row; nil on every other row.
	Media *MediaStats `json:"media,omitempty"`
}

// voiceAdminKind is the MediaStats kind of a voice library write or delete.
const voiceAdminKind = "admin"

// MediaStats summarises a media row. Kind is the api_type the path serves
// (speech, transcription, video, audio_gen, realtime) or "admin" for a voice
// library change; only the fields of that kind are set.
type MediaStats struct {
	Kind string `json:"kind"`
	// Seconds is the media length: speech audio out, transcribed audio in
	// (when the backend reports it), the job's requested duration, or the
	// realtime session length.
	Seconds    float64 `json:"seconds,omitempty"`
	InputChars int     `json:"input_chars,omitempty"`
	Voice      string  `json:"voice,omitempty"`
	// Playable is true when the whole audio response fitted the capture cap
	// and is served from /_router/activity/{id}/audio.
	Playable    bool   `json:"playable,omitempty"`
	FileName    string `json:"file_name,omitempty"`
	InputBytes  int64  `json:"input_bytes,omitempty"`
	OutputChars int    `json:"output_chars,omitempty"`
	Language    string `json:"language,omitempty"`
	// Text is the first 200 characters of a transcript.
	Text      string `json:"text,omitempty"`
	JobID     string `json:"job_id,omitempty"`
	State     string `json:"state,omitempty"`
	FramesIn  int    `json:"frames_in,omitempty"`
	FramesOut int    `json:"frames_out,omitempty"`
	CloseCode int    `json:"close_code,omitempty"`
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

// storedCapture holds the capture and one optional blob served beside it: an
// image thumbnail (JPEG) or a whole speech response that fitted the cap.
type storedCapture struct {
	cap      ActivityCapture
	blob     []byte
	blobType string
	size     int
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

// Blob returns the thumbnail or audio kept for id, with its content type.
func (a *ActivityLog) Blob(id int) ([]byte, string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	sc, ok := a.captures[id]
	if !ok || len(sc.blob) == 0 {
		return nil, "", false
	}
	return sc.blob, sc.blobType, true
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

func (a *ActivityLog) record(entry ActivityEntry, cap ActivityCapture, blob []byte, blobType string) ActivityEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.nextID++
	entry.ID = a.nextID
	cap.ID = entry.ID
	size := len(cap.ReqBody) + len(cap.RespBody) + len(blob)
	for a.captureSz+size > maxCaptureBytes && len(a.captures) > 0 {
		a.dropOldestCapture()
	}
	if size > 0 && a.captureSz+size <= maxCaptureBytes {
		entry.HasCapture = true
		a.captures[entry.ID] = storedCapture{cap: cap, blob: blob, blobType: blobType, size: size}
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
	img          bytes.Buffer
	imgMax       int
	imgTruncated bool
	// A media response (audio, video, octet-stream) is never captured as
	// text: only its byte count and a hash prefix are kept. keepMedia also
	// keeps up to max bytes, so a short speech response can be replayed.
	media     bool
	keepMedia bool
	sniffed   string // body type sniffed when the head carried no Content-Type
	respBytes int64
	hash      hash.Hash
	hashed    int
	// ws is set once a websocket upgrade hijacks the connection.
	ws *wsConn
}

func (w *activityWriter) WriteHeader(code int) {
	// A forwarded interim response (100 Continue) is not the final head: pass
	// it through, but do not record it as the status or pick the body kind
	// from its empty headers.
	if code < 200 {
		w.ResponseWriter.WriteHeader(code)
		return
	}
	if w.wroteHead {
		return
	}
	w.wroteHead = true
	w.status = code
	w.media = isMediaResponse(w.Header().Get("Content-Type"))
	w.ResponseWriter.WriteHeader(code)
}

func (w *activityWriter) Write(p []byte) (int, error) {
	if !w.wroteHead {
		w.WriteHeader(http.StatusOK)
	}
	if w.sniffed == "" && len(p) > 0 && w.Header().Get("Content-Type") == "" {
		// No Content-Type on the head: net/http sniffs the first bytes for the
		// client, so pick the body kind from the same sniff.
		w.sniffed = http.DetectContentType(p)
		w.media = isMediaResponse(w.sniffed)
	}
	w.noteFirstToken(p)
	if w.media {
		w.captureMedia(p)
	}
	if !w.media || w.keepMedia {
		w.captureBody(p)
	}
	// No flush here: proxyTo's ReverseProxy (FlushInterval -1) already flushes
	// each chunk through Flush below.
	return w.ResponseWriter.Write(p)
}

// captureMedia counts the response bytes and hashes the first maxHashBytes,
// without holding the body.
func (w *activityWriter) captureMedia(p []byte) {
	w.respBytes += int64(len(p))
	if w.hashed >= maxHashBytes {
		return
	}
	n := maxHashBytes - w.hashed
	if n > len(p) {
		n = len(p)
	}
	if w.hash == nil {
		w.hash = sha256.New()
	}
	w.hash.Write(p[:n])
	w.hashed += n
}

// captureBody buffers the response for the activity capture, clipped to max,
// and fills the extra image buffer for a thumbnail.
func (w *activityWriter) captureBody(p []byte) {
	if w.imgMax > 0 {
		n := w.imgMax - w.img.Len()
		if n > len(p) {
			n = len(p)
		}
		if n > 0 {
			w.img.Write(p[:n])
		}
		if n < len(p) {
			w.imgTruncated = true
		}
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
}

// bodyHash is a hexadecimal prefix of the media response hash, "" when no media
// body was written.
func (w *activityWriter) bodyHash() string {
	if w.hash == nil {
		return ""
	}
	return hex.EncodeToString(w.hash.Sum(nil))[:16]
}

// isMediaResponse reports whether a response body is binary media the router
// never captures in memory: audio, video, or an opaque octet stream.
func isMediaResponse(contentType string) bool {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	return strings.HasPrefix(ct, "audio/") ||
		strings.HasPrefix(ct, "video/") ||
		ct == "application/octet-stream"
}

func (w *activityWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *activityWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// Hijack hands a websocket upgrade the client connection, wrapped so the
// session row can count frames each way. The 101 head still goes out through
// the server's own writer, so it is not read as a frame; ReverseProxy writes
// only that head to the ReadWriter and copies frames over the returned conn.
func (w *activityWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, brw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	// Bytes the server read ahead of the hijack come first, then the socket.
	src := io.Reader(conn)
	if n := brw.Reader.Buffered(); n > 0 {
		ahead, _ := brw.Reader.Peek(n)
		src = io.MultiReader(bytes.NewReader(ahead), conn)
	}
	w.ws = &wsConn{Conn: conn, r: src}
	w.status, w.wroteHead = http.StatusSwitchingProtocols, true
	return w.ws, bufio.NewReadWriter(bufio.NewReader(w.ws), brw.Writer), nil
}

// wsConn is the client side of a proxied websocket session. It counts frames
// each way for the Activity row and never holds a payload.
type wsConn struct {
	net.Conn
	r       io.Reader
	mu      sync.Mutex
	in, out wsFrames
	code    int
}

func (c *wsConn) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.note(&c.in, p[:n])
	return n, err
}

func (c *wsConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.note(&c.out, p[:n])
	return n, err
}

// CloseWrite keeps the half-close the proxy sends the client when the backend
// ends; the embedded net.Conn alone would hide it.
func (c *wsConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

func (c *wsConn) note(f *wsFrames, p []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if code := f.feed(p); code != 0 && c.code == 0 {
		c.code = code
	}
}

// counts returns the frames each way and the first close code either side sent.
func (c *wsConn) counts() (in, out, code int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.in.n, c.out.n, c.code
}

// wsFrames follows one direction of a websocket stream (RFC 6455 framing),
// counting frames and reading the status code of a close frame.
type wsFrames struct {
	n      int
	hdr    []byte
	left   uint64 // payload bytes of the current frame not yet seen
	close  bool   // the current frame is a close frame
	masked bool
	mask   [4]byte
	seen   int // close payload bytes read, up to the 2-byte code
	code   [2]byte
}

// feed consumes the next bytes of the stream. It returns the close code when
// this chunk completes one, else 0.
func (f *wsFrames) feed(p []byte) (closeCode int) {
	for len(p) > 0 {
		if f.left == 0 {
			f.hdr = append(f.hdr, p[0])
			p = p[1:]
			need := 2
			if len(f.hdr) >= 2 {
				switch f.hdr[1] & 0x7f {
				case 126:
					need += 2
				case 127:
					need += 8
				}
				if f.hdr[1]&0x80 != 0 {
					need += 4
				}
			}
			if len(f.hdr) < need {
				continue
			}
			size, rest := uint64(f.hdr[1]&0x7f), f.hdr[2:]
			switch size {
			case 126:
				size, rest = uint64(binary.BigEndian.Uint16(rest)), rest[2:]
			case 127:
				size, rest = binary.BigEndian.Uint64(rest), rest[8:]
			}
			f.masked = len(rest) == 4
			copy(f.mask[:], rest)
			f.n++
			f.close = f.hdr[0]&0x0f == 8
			f.seen = 0
			f.left = size
			f.hdr = f.hdr[:0]
			continue
		}
		n := len(p)
		if uint64(n) > f.left {
			n = int(f.left)
		}
		if f.close && f.seen < 2 {
			for _, b := range p[:n] {
				if f.seen == 2 {
					break
				}
				if f.masked {
					b ^= f.mask[f.seen]
				}
				f.code[f.seen] = b
				f.seen++
			}
			if f.seen == 2 {
				closeCode = int(binary.BigEndian.Uint16(f.code[:]))
			}
		}
		f.left -= uint64(n)
		p = p[n:]
	}
	return closeCode
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
// OpenAI data[].b64_json. The second return says why no thumbnail was made, so
// a decode failure is not mistaken for a body that never held an image; it is
// empty when a thumbnail was made or the body simply has no image.
func thumbFromImageResponse(body []byte) ([]byte, string) {
	if len(body) == 0 {
		return nil, ""
	}
	var obj struct {
		Images []string `json:"images"`
		Data   []struct {
			B64JSON string `json:"b64_json"`
			URL     string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, "response is not complete JSON: " + err.Error()
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
		return nil, ""
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		// An OpenAI response_format:"url" body carries a link, not bytes; the
		// UI reads that URL straight from the capture, so it is not a failure.
		if strings.HasPrefix(enc, "http") {
			return nil, ""
		}
		return nil, "image base64 decode failed: " + err.Error()
	}
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, "image decode failed: " + err.Error()
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, downscale(src, maxThumbEdge), &jpeg.Options{Quality: 80}); err != nil {
		return nil, "thumbnail encode failed: " + err.Error()
	}
	return buf.Bytes(), ""
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
	case string:
		// Some job APIs send a duration as a string, e.g. "seconds":"4".
		f, _ := strconv.ParseFloat(n, 64)
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
	"/audio/speech/batch", "/audio/generations", "/videos",
	"/txt2img", "/img2img",
}

// isVoiceAdmin reports whether a request writes to or deletes from the voice
// library; it gets an admin row rather than a generation row.
func isVoiceAdmin(method, path string) bool {
	return (method == http.MethodPost && strings.HasSuffix(path, "/audio/voices")) ||
		(method == http.MethodDelete && strings.Contains(path, "/audio/voices/"))
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

func (r *Router) recordProxy(req *http.Request, rec *activityWriter, started time.Time, reqBody []byte, reqBytes int64, reqTrunc bool, model, target string) {
	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}
	ct := rec.Header().Get("Content-Type")
	if ct == "" {
		ct = rec.sniffed
	}
	var tokens TokenStats
	if !rec.media {
		tokens = parseTokenStats(rec.body.Bytes(), ct)
	}
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
	// A realtime session is logged once, when its websocket closes.
	logged := rec.ws != nil || isGenerationRequest(req.Method, req.URL.Path) || isVoiceAdmin(req.Method, req.URL.Path)
	if logged {
		entry.Media = mediaStats(req, rec, reqBody, reqBytes, entry.DurationMs)
	}
	// Metrics see every proxied request, even when the activity ring is
	// disabled or filters the row out.
	if r.metrics != nil {
		r.metrics.Observe(entry)
	}
	if r.activity == nil || !logged {
		return
	}
	entry.RespBytes = rec.respBytes
	entry.BodyHash = rec.bodyHash()
	reqStr, clipped := clipBody(reqBody)
	var respStr string
	var blob []byte
	blobType := "image/jpeg"
	if rec.media {
		// Kept media bytes are served whole as the row's audio, never as text,
		// and only when the response fitted the cap.
		if !rec.truncated && rec.body.Len() > 0 {
			blob, blobType = rec.body.Bytes(), ct
			if entry.Media != nil {
				entry.Media.Playable = true
			}
		}
	} else {
		respStr = rec.body.String()
		thumb, issue := thumbFromImageResponse(rec.img.Bytes())
		if rec.imgTruncated {
			thumb, issue = nil, fmt.Sprintf("response exceeds the %d MB image buffer", maxImageBody>>20)
		}
		entry.ThumbIssue = issue
		blob = thumb
	}
	r.activity.record(entry, ActivityCapture{
		ReqHeaders:  copyHeaderMap(req.Header),
		ReqBody:     reqStr,
		RespHeaders: copyHeaderMap(rec.Header()),
		RespBody:    respStr,
		Truncated:   reqTrunc || clipped || (rec.truncated && !rec.media),
	}, blob, blobType)
}

// pathAPIType is the api_type a request path serves, "" when none.
func pathAPIType(path string) string {
	for _, m := range pathAPITypes {
		if strings.HasPrefix(path, m.prefix) {
			return m.apiType
		}
	}
	return ""
}

// mediaStats summarises an audio, video, realtime or voice library row from
// the captured bodies. It is nil for any other row, which keeps token stats.
func mediaStats(req *http.Request, rec *activityWriter, reqBody []byte, reqBytes int64, durMs int) *MediaStats {
	kind := pathAPIType(req.URL.Path)
	if isVoiceAdmin(req.Method, req.URL.Path) {
		kind = voiceAdminKind
	}
	m := &MediaStats{Kind: kind}
	// resp stays nil for an audio or plain-text body; reading a nil map is safe.
	var resp map[string]any
	_ = json.Unmarshal(rec.body.Bytes(), &resp)
	switch kind {
	case config.APITypeSpeech:
		var in struct {
			Input string `json:"input"`
			Voice string `json:"voice"`
		}
		_ = json.Unmarshal(reqBody, &in)
		m.InputChars = utf8.RuneCountInString(in.Input)
		m.Voice = in.Voice
		m.Seconds = wavSeconds(rec.body.Bytes(), rec.respBytes)
	case config.APITypeTranscription:
		m.FileName = multipartFileName(reqBody)
		m.InputBytes = reqBytes
		var text string
		switch {
		case rec.status >= 400:
		case resp != nil:
			text, _ = resp["text"].(string)
			m.Language, _ = resp["language"].(string)
			m.Seconds = asFloat(resp["duration"])
		default: // response_format text, srt or vtt
			text = rec.body.String()
		}
		m.OutputChars = utf8.RuneCountInString(text)
		m.Text = clipRunes(text, 200)
	case config.APITypeVideo, config.APITypeAudioGen:
		// shortcut: the row holds the submit response only, update it from the job's polls once #69 tracks jobs.
		m.JobID, _ = resp["id"].(string)
		m.State, _ = resp["status"].(string)
		if m.Seconds = asFloat(resp["seconds"]); m.Seconds == 0 {
			m.Seconds = asFloat(resp["duration"])
		}
	case config.APITypeRealtime:
		if rec.ws == nil {
			return nil
		}
		m.FramesIn, m.FramesOut, m.CloseCode = rec.ws.counts()
		m.Seconds = float64(durMs) / 1000
	case voiceAdminKind:
	default:
		return nil
	}
	return m
}

// wavSeconds is the play length of a WAV body from its header, given the first
// bytes and the full byte count; 0 when the body is not a WAV.
func wavSeconds(b []byte, total int64) float64 {
	if len(b) < 12 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return 0
	}
	var byteRate uint32
	for i := 12; i+8 <= len(b); {
		size := binary.LittleEndian.Uint32(b[i+4:])
		body := i + 8
		switch string(b[i : i+4]) {
		case "fmt ":
			if body+12 <= len(b) {
				byteRate = binary.LittleEndian.Uint32(b[body+8:])
			}
		case "data":
			if byteRate == 0 {
				return 0
			}
			n, rest := int64(size), total-int64(body)
			// A streamed WAV leaves the data size at 0 or 0xFFFFFFFF.
			if size == 0 || size == math.MaxUint32 || n > rest {
				n = rest
			}
			return float64(n) / float64(byteRate)
		}
		i = body + int(size) + int(size&1)
	}
	return 0
}

// multipartFileName is the first file name in a multipart body prefix, or "".
func multipartFileName(body []byte) string {
	const key = `filename="`
	i := bytes.Index(body, []byte(key))
	if i < 0 {
		return ""
	}
	rest := body[i+len(key):]
	j := bytes.IndexByte(rest, '"')
	if j < 0 || j > 255 {
		return ""
	}
	return string(rest[:j])
}

// clipRunes returns the first n characters of s.
func clipRunes(s string, n int) string {
	for i := range s {
		if n == 0 {
			return s[:i]
		}
		n--
	}
	return s
}

func (r *Router) wrapProxy(w http.ResponseWriter, req *http.Request, model, target string) (http.ResponseWriter, func()) {
	if r.activity == nil {
		return w, func() {}
	}
	started := time.Now()
	// A logged request keeps its audio up to the capture cap for the player.
	rec := &activityWriter{ResponseWriter: w, max: maxCaptureBody, started: started, keepMedia: isGenerationRequest(req.Method, req.URL.Path)}
	if isImageRequest(req.URL.Path) {
		rec.imgMax = maxImageBody
	}
	// Tee the request body as the proxy streams it, instead of reading it all
	// into memory first, so a large upload is not buffered a second time.
	var cap *captureReader
	if req.Body != nil && req.Body != http.NoBody {
		cap = &captureReader{rc: req.Body, max: maxCaptureBody}
		req.Body = cap
	}
	return rec, func() {
		var reqBody []byte
		var reqBytes int64
		var reqTrunc bool
		if cap != nil {
			// A backend that answers before reading the upload leaves the body
			// undrained: flag the capture as partial rather than complete.
			reqBody, reqBytes, reqTrunc = cap.buf.Bytes(), cap.n, cap.truncated || (!cap.eof && req.ContentLength != 0)
		}
		r.recordProxy(req, rec, started, reqBody, reqBytes, reqTrunc, model, target)
	}
}

// captureReader passes the request body through to the backend while keeping
// the first maxCaptureBody bytes for the activity capture.
type captureReader struct {
	rc        io.ReadCloser
	buf       bytes.Buffer
	max       int
	truncated bool
	eof       bool  // the body was read to its end
	n         int64 // bytes read in total, captured or not
}

func (c *captureReader) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	c.n += int64(n)
	if n > 0 {
		remain := c.max - c.buf.Len()
		switch {
		case remain <= 0:
			c.truncated = true
		case n > remain:
			c.buf.Write(p[:remain])
			c.truncated = true
		default:
			c.buf.Write(p[:n])
		}
	}
	if err == io.EOF {
		c.eof = true
	} else if err != nil {
		// The client went away or the body hit the cap: the capture is partial.
		c.truncated = true
	}
	return n, err
}

func (c *captureReader) Close() error { return c.rc.Close() }
