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
	"slices"
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
	// maxStatsTail is how much of a response past the capture clip is kept
	// for token parsing. The usage/timings chunk of a stream is last, so a
	// stream longer than maxCaptureBody (roughly 1k streamed tokens) lost its
	// token counts entirely before this tail existed.
	maxStatsTail    = 64 * 1024
	maxCaptureBytes = 8 * 1024 * 1024
	// maxImageBody is how much of an image-generation response is buffered, so
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
	// PromptEvalTokens is timings.prompt_n: prompt tokens the engine actually
	// evaluated, which excludes prompt cache hits (timings.cache_n).
	PromptEvalTokens int `json:"prompt_eval_tokens,omitempty"`
	// PredictedMs is timings.predicted_ms: time spent decoding.
	PredictedMs float64 `json:"predicted_ms,omitempty"`
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
	// JobID and JobState are set on an async job's row, written when the job
	// ends: DurationMs then runs from submit to that end state.
	JobID    string `json:"job_id,omitempty"`
	JobState string `json:"job_state,omitempty"`
	// FromPeer is the node that proxied the request here, from its
	// X-Model-Router-Peer header; empty for a client request.
	FromPeer string `json:"from_peer,omitempty"`
}

// maxInProgress caps the running requests listed in the Activity API and
// telemetry; maxRecent is how many finished rows telemetry summarises.
const (
	maxInProgress = 50
	maxRecent     = 20
)

// InProgressEntry is a logged request that has started and not yet finished.
// ID is the Activity id its finished row will get; with the telemetry's node
// it keys the request across the mesh.
type InProgressEntry struct {
	ID       int       `json:"id"`
	Model    string    `json:"model"`
	Target   string    `json:"target,omitempty"`
	Method   string    `json:"method"`
	Path     string    `json:"path"`
	Started  time.Time `json:"started"`
	FromPeer string    `json:"from_peer,omitempty"`
	TTFTMs   int       `json:"ttft_ms,omitempty"`
	// Tokens is the streamed chunks so far, about one per generated token.
	Tokens int `json:"tokens,omitempty"`
	// Prompt is the first 120 characters of the prompt or last message.
	Prompt string `json:"prompt,omitempty"`
}

// RecentEntry summarises a finished Activity row for peers, so a request they
// saw in progress can be shown as finished.
type RecentEntry struct {
	ID         int    `json:"id"`
	Model      string `json:"model"`
	Path       string `json:"path"`
	Status     int    `json:"status"`
	DurationMs int    `json:"duration_ms"`
	Tokens     int    `json:"tokens,omitempty"`
	Target     string `json:"target,omitempty"`
	FromPeer   string `json:"from_peer,omitempty"`
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
	// Truncated is set only when a body had to be cut as raw bytes.
	Truncated bool `json:"truncated"`
	// ClippedFields names the JSON fields shortened to fit the capture, such as
	// "request.messages[3].content" or "response.images[0]"; the bodies still parse.
	ClippedFields []string `json:"clipped_fields,omitempty"`
	// Notes are what an image_pipeline step changed, such as a rewritten
	// prompt. The request body above stays the client's original.
	Notes []string `json:"notes,omitempty"`
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
	running   map[int]*InProgressEntry
	subs      map[chan ActivityEntry]struct{}
}

func NewActivityLog() *ActivityLog {
	return &ActivityLog{captures: map[int]storedCapture{}, running: map[int]*InProgressEntry{}, subs: map[chan ActivityEntry]struct{}{}}
}

// Subscribe returns the rows List would return plus a channel of rows recorded
// after them. The channel is closed when cancel runs or when the subscriber
// falls behind; record never waits on a reader.
func (a *ActivityLog) Subscribe() (rows []ActivityEntry, live <-chan ActivityEntry, cancel func()) {
	ch := make(chan ActivityEntry, 32)
	a.mu.Lock()
	defer a.mu.Unlock()
	// Read the rows under the lock that registers ch, so no row lands in neither.
	rows = make([]ActivityEntry, 0, len(a.entries))
	for i := len(a.entries) - 1; i >= 0; i-- {
		rows = append(rows, a.entries[i])
	}
	a.subs[ch] = struct{}{}
	return rows, ch, func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.dropSub(ch)
	}
}

// dropSub closes ch once; the caller holds a.mu.
func (a *ActivityLog) dropSub(ch chan ActivityEntry) {
	if _, ok := a.subs[ch]; ok {
		delete(a.subs, ch)
		close(ch)
	}
}

// begin registers a running request and reserves the Activity id its row
// will be written under.
func (a *ActivityLog) begin(e InProgressEntry) *InProgressEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.nextID++
	e.ID = a.nextID
	a.running[e.ID] = &e
	return &e
}

// end drops a running request; it is safe to call after record already did.
func (a *ActivityLog) end(id int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.running, id)
}

// update changes a running entry under the log's lock.
func (a *ActivityLog) update(e *InProgressEntry, f func(*InProgressEntry)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f(e)
}

// InProgress returns newest-first copies of the running requests, capped at
// maxInProgress.
func (a *ActivityLog) InProgress() []InProgressEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]InProgressEntry, 0, len(a.running))
	for _, e := range a.running {
		out = append(out, *e)
	}
	slices.SortFunc(out, func(x, y InProgressEntry) int { return y.ID - x.ID })
	return out[:min(len(out), maxInProgress)]
}

// Recent summarises the newest maxRecent finished rows.
func (a *ActivityLog) Recent() []RecentEntry {
	rows := a.List()
	rows = rows[:min(len(rows), maxRecent)]
	out := make([]RecentEntry, 0, len(rows))
	for _, e := range rows {
		out = append(out, RecentEntry{
			ID: e.ID, Model: e.Model, Path: e.Path, Status: e.Status, DurationMs: e.DurationMs,
			Tokens: e.Tokens.CompletionTokens, Target: e.Target, FromPeer: e.FromPeer,
		})
	}
	return out
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
	// A proxied row reserved its id when the request began; a job row has none.
	if entry.ID == 0 {
		a.nextID++
		entry.ID = a.nextID
	}
	// The finished row replaces the running entry under the same lock.
	delete(a.running, entry.ID)
	cap.ID = entry.ID
	size := len(cap.ReqBody) + len(cap.RespBody) + len(blob)
	for a.captureSz+size > maxCaptureBytes && len(a.captures) > 0 {
		a.dropOldestCapture()
	}
	// A capture worth keeping holds bytes or flags a truncated request: an
	// undrained upload with an empty response must still be visible as partial,
	// not dropped as an empty capture.
	if (size > 0 || cap.Truncated) && a.captureSz+size <= maxCaptureBytes {
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
	for ch := range a.subs {
		select {
		case ch <- entry:
		default:
			// A full buffer means a stalled reader: close it so it reconnects
			// for a fresh snapshot instead of silently missing rows.
			a.dropSub(ch)
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
	// tail keeps the last maxStatsTail bytes written past max, so token
	// stats can still be parsed from a long stream's final chunk.
	tail bytes.Buffer
	// A media response (audio, video, octet-stream) is never captured as
	// text: only its byte count and a hash prefix are kept. keepMedia also
	// keeps up to maxCaptureBody bytes, so a short speech response can be replayed.
	media     bool
	keepMedia bool
	sniffed   string // body type sniffed when the head carried no Content-Type
	respBytes int64
	hash      hash.Hash
	hashed    int
	// ws is set once a websocket upgrade hijacks the connection.
	ws *wsConn
	// live is this request's in-progress entry, nil when it is not logged.
	live *InProgressEntry
	log  *ActivityLog
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
	w.noteProgress(p)
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

// captureBody buffers the response, up to max, for the activity capture and
// thumbnail; recordProxy clips it to fit the ring.
func (w *activityWriter) captureBody(p []byte) {
	limit := w.max
	if w.media {
		// Kept audio is served whole, so it must already fit the capture.
		limit = min(limit, maxCaptureBody)
	}
	remain := limit - w.body.Len()
	switch {
	case remain <= 0:
		w.truncated = true
		w.keepTail(p)
	case len(p) > remain:
		w.body.Write(p[:remain])
		w.truncated = true
		w.keepTail(p[remain:])
	default:
		w.body.Write(p)
	}
}

// keepTail appends bytes past the capture clip to the stats tail, keeping
// only the newest maxStatsTail bytes.
func (w *activityWriter) keepTail(p []byte) {
	w.tail.Write(p)
	if w.tail.Len() > 2*maxStatsTail {
		keep := append([]byte(nil), w.tail.Bytes()[w.tail.Len()-maxStatsTail:]...)
		w.tail.Reset()
		w.tail.Write(keep)
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

// Hijack records the 101 and keeps the session's wsConn (sessionWriter wraps
// every proxied websocket) so the row can count frames each way.
func (w *activityWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, brw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	w.ws, _ = conn.(*wsConn)
	w.status, w.wroteHead = http.StatusSwitchingProtocols, true
	return conn, brw, nil
}

// wsConn is the client side of a proxied websocket session. It counts frames
// each way for the Activity row and never holds a payload.
type wsConn struct {
	net.Conn
	r       io.Reader
	mu      sync.Mutex
	in, out wsFrames
	code    int
	wmu     sync.Mutex // orders proxy writes with goAway's close frame
	ending  bool       // goAway waits for the frame in flight to finish
	ended   bool       // the close frame is out; later writes are refused
}

func (c *wsConn) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.note(&c.in, p[:n])
	return n, err
}

func (c *wsConn) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.ended {
		return 0, net.ErrClosed
	}
	n, err := c.Conn.Write(p)
	c.note(&c.out, p[:n])
	if c.ending {
		c.sendGoingAway()
	}
	return n, err
}

// goAway ends the session with close code 1001 (going away) as soon as the
// client stream is between frames, so a close never lands inside a frame the
// backend is part way through.
func (c *wsConn) goAway() {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.ending = true
	c.sendGoingAway()
}

// sendGoingAway writes the 1001 close frame if no frame is part sent, then
// gives the client a second to answer before the proxy's read ends. The
// caller holds wmu.
func (c *wsConn) sendGoingAway() {
	c.mu.Lock()
	between := c.out.left == 0 && len(c.out.hdr) == 0
	c.mu.Unlock()
	if !between || c.ended {
		return
	}
	c.ended = true
	frame := []byte{0x88, 0x02, 0x03, 0xe9}
	if _, err := c.Conn.Write(frame); err == nil {
		c.note(&c.out, frame)
	}
	_ = c.Conn.SetReadDeadline(time.Now().Add(time.Second))
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

// noteProgress copies the first-token time and the streamed chunk count to the
// in-progress entry once a streamed token has been seen.
// shortcut: counts SSE "data:" events (the usage and [DONE] events too), parse the deltas if a backend batches tokens per event.
func (w *activityWriter) noteProgress(p []byte) {
	if w.live == nil || !w.ttftSet {
		return
	}
	n := bytes.Count(p, []byte("data:"))
	w.log.update(w.live, func(e *InProgressEntry) {
		e.TTFTMs = w.ttftMs
		e.Tokens += n
	})
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
	if src.PromptEvalTokens != 0 {
		dst.PromptEvalTokens = src.PromptEvalTokens
	}
	if src.PredictedMs != 0 {
		dst.PredictedMs = src.PredictedMs
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
			// usage.prompt_tokens counts cache hits; prompt_n does not, so
			// add cache_n back to keep one meaning for PromptTokens.
			s.PromptTokens = asInt(t["prompt_n"]) + asInt(t["cache_n"])
		}
		if s.CompletionTokens == 0 {
			s.CompletionTokens = asInt(t["predicted_n"])
		}
		s.PromptPerSecond = asFloat(t["prompt_per_second"])
		s.TokensPerSecond = asFloat(t["predicted_per_second"])
		s.PromptMs = asFloat(t["prompt_ms"])
		s.PromptEvalTokens = asInt(t["prompt_n"])
		s.PredictedMs = asFloat(t["predicted_ms"])
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
// the activity ring by default. A new entry also needs a group in
// ACT_PATH_GROUP (ui/app.js) for the Activity filter; ui_test.go checks it.
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
		// A clipped stream carries its usage/timings in the tail, after the
		// capture limit. Later chunks win, as they do within one body.
		if rec.truncated && rec.tail.Len() > 0 {
			tokens = mergeTokenStats(tokens, parseTokenStats(rec.tail.Bytes(), ct))
		}
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
		FromPeer:    r.fromPeer(req),
	}
	if rec.live != nil {
		entry.ID = rec.live.ID
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
	reqStr, fields, clipped := clipCapture(reqBody, "request.")
	var respStr string
	var respCut bool
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
		var respFields []string
		respStr, respFields, respCut = clipCapture(rec.body.Bytes(), "response.")
		fields = append(fields, respFields...)
		if isImageRequest(req.URL.Path) {
			thumb, issue := thumbFromImageResponse(rec.body.Bytes())
			if rec.truncated {
				thumb, issue = nil, fmt.Sprintf("response exceeds the %d MB image buffer", maxImageBody>>20)
			}
			entry.ThumbIssue = issue
			blob = thumb
		}
	}
	r.activity.record(entry, ActivityCapture{
		ReqHeaders:    copyHeaderMap(req.Header),
		ReqBody:       reqStr,
		RespHeaders:   copyHeaderMap(rec.Header()),
		RespBody:      respStr,
		Truncated:     reqTrunc || clipped || respCut || (rec.truncated && !rec.media),
		ClippedFields: fields,
		Notes:         imageNotesFrom(req),
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

// promptPreview is the first 120 characters of a JSON request's prompt: the
// last chat message, or the prompt or input field. "" for any other body.
func promptPreview(body []byte) string {
	var b struct {
		Prompt   any `json:"prompt"`
		Input    any `json:"input"`
		Messages []struct {
			Content any `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &b) != nil {
		return ""
	}
	s := firstText(b.Prompt)
	if n := len(b.Messages); n > 0 {
		s = firstText(b.Messages[n-1].Content)
	}
	if s == "" {
		s = firstText(b.Input)
	}
	return clipRunes(s, 120)
}

// firstText is the first non-empty string in a prompt, a list of prompts or
// content parts ({"type":"text","text":...}).
func firstText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		for _, e := range t {
			if s := firstText(e); s != "" {
				return s
			}
		}
	case map[string]any:
		if s := firstText(t["text"]); s != "" {
			return s
		}
		return firstText(t["content"])
	}
	return ""
}

// fromPeer is the node that proxied req here. The header this router stamps
// on its own forward to a peer does not count: a peer-proxied request is never
// forwarded again, so a forward always began here.
func (r *Router) fromPeer(req *http.Request) string {
	if p := req.Header.Get("X-Model-Router-Peer"); p != r.node {
		return p
	}
	return ""
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
	rec := &activityWriter{ResponseWriter: w, max: maxClipSource, started: started, keepMedia: isGenerationRequest(req.Method, req.URL.Path)}
	if isImageRequest(req.URL.Path) {
		rec.max = maxImageBody
	}
	// Tee the request body as the proxy streams it, instead of reading it all
	// into memory first, so a large upload is not buffered a second time. An
	// image pipeline that rewrites the body already kept the client's original
	// bytes, so the capture keeps those instead.
	original, isOriginal := req.Context().Value(originalImageBodyKey{}).(*originalImageBody)
	// Same rule as recordProxy's logged, known before the upgrade is answered.
	if isWebsocketUpgrade(req) || isGenerationRequest(req.Method, req.URL.Path) || isVoiceAdmin(req.Method, req.URL.Path) {
		e := InProgressEntry{
			Model: model, Target: target, Method: req.Method, Path: req.URL.Path,
			Started: started, FromPeer: r.fromPeer(req),
		}
		if isOriginal {
			e.Prompt = promptPreview(original.body)
		}
		rec.log, rec.live = r.activity, r.activity.begin(e)
	}
	var cap *captureReader
	if !isOriginal && req.Body != nil && req.Body != http.NoBody {
		cap = &captureReader{rc: req.Body, max: maxClipSource}
		if live := rec.live; live != nil {
			// The proxy reads the body as it starts sending, so the preview
			// shows within moments of the request starting.
			// shortcut: no preview for a body over maxClipSource (large vision or upload), parse a prefix if that matters.
			cap.onEOF = func(b []byte) {
				p := promptPreview(b)
				r.activity.update(live, func(e *InProgressEntry) { e.Prompt = p })
			}
		}
		req.Body = cap
	}
	return rec, func() {
		if rec.live != nil {
			// Whatever the outcome, panics and client disconnects included.
			defer r.activity.end(rec.live.ID)
		}
		if isOriginal {
			// A backend that answers before reading the upload leaves the rest
			// unread, the same as the captureReader path below.
			trunc := original.truncated || (!original.eof && original.hasBody)
			r.recordProxy(req, rec, started, original.body, original.bytes, trunc, model, target)
			return
		}
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
// the first bytes for the activity capture: up to max of a JSON body, up to
// maxCaptureBody of anything else.
type captureReader struct {
	rc        io.ReadCloser
	buf       bytes.Buffer
	max       int
	truncated bool
	eof       bool  // the body was read to its end
	n         int64 // bytes read in total, captured or not
	// onEOF, when set, gets the whole body once it is read and not clipped.
	onEOF func([]byte)
}

func (c *captureReader) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	c.n += int64(n)
	if n > 0 {
		head := c.buf.Bytes()
		if len(head) == 0 {
			head = p[:n]
		}
		remain := captureLimit(head, c.max) - c.buf.Len()
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
		if !c.eof && !c.truncated && c.onEOF != nil {
			c.onEOF(c.buf.Bytes())
		}
		c.eof = true
	} else if err != nil {
		// The client went away or the body hit the cap: the capture is partial.
		c.truncated = true
	}
	return n, err
}

func (c *captureReader) Close() error { return c.rc.Close() }
