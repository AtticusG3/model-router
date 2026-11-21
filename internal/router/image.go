package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"sync"

	"model-router/internal/config"
)

// imageStep is one step of a stanza's image_pipeline. The serving node runs it
// for requests from local clients and for requests proxied in from peers.
type imageStep struct {
	// Rewrite runs before the request reaches the backend and may replace
	// req.Body, and may set reply headers on h, which reach the client whatever
	// the backend answers. An imageReject error answers the client 400; any
	// other error is a body read fault. nil is a no-op.
	Rewrite func(req *http.Request, h http.Header, notes *imageNotes) error
	// After inspects the backend reply. It may add headers to h and change req
	// headers or the prepared body for a retry; true asks for another attempt.
	// The step decides when to stop. Headers a step sets on an attempt are
	// carried to the final reply unless a later attempt sets the same name.
	After func(req *http.Request, status int, h http.Header, body []byte, notes *imageNotes) bool
}

// bodyRequestsLoRA reports whether a JSON request body names a non-empty LoRA
// list. It reads r to the end.
func bodyRequestsLoRA(r io.Reader) bool {
	var body struct {
		LoRA []json.RawMessage `json:"lora"`
	}
	if json.NewDecoder(r).Decode(&body) != nil {
		return false
	}
	return len(body.LoRA) > 0
}

// responseIsInvalidLoRA reports whether a reply is sd-server's 400 for a LoRA
// path it does not know.
func responseIsInvalidLoRA(status int, body []byte) bool {
	return status == http.StatusBadRequest && bytes.Contains(bytes.ToLower(body), []byte("invalid lora path"))
}

// imageNotes collects what pipeline steps changed, for the Activity capture.
type imageNotes struct {
	mu   sync.Mutex
	list []string
}

func (n *imageNotes) Add(s string) {
	if s == "" {
		return
	}
	n.mu.Lock()
	n.list = append(n.list, s)
	n.mu.Unlock()
}

func (n *imageNotes) all() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.list...)
}

type imageNotesKey struct{}

type preparedBodyKey struct{}

// preparedBodyFrom returns the replayable request body of a pipeline exchange,
// so a response step can rewrite it for a retry.
func preparedBodyFrom(req *http.Request) *preparedBody {
	b, _ := req.Context().Value(preparedBodyKey{}).(*preparedBody)
	return b
}

// imageNotesFrom returns the notes attached to a pipeline request, if any.
func imageNotesFrom(req *http.Request) []string {
	if n, ok := req.Context().Value(imageNotesKey{}).(*imageNotes); ok && n != nil {
		return n.all()
	}
	return nil
}

// originalImageBody is the client's request body as received, kept for the
// Activity capture when pipeline steps rewrite it. eof records whether the whole
// body was read; a backend that answers early leaves it false.
type originalImageBody struct {
	body      []byte
	bytes     int64
	truncated bool
	hasBody   bool
	eof       bool
}

type originalImageBodyKey struct{}

// captureOriginalImageBody keeps the first maxCaptureBody bytes of the client's
// body and leaves the whole body replayable on req.
func captureOriginalImageBody(req *http.Request) (*originalImageBody, error) {
	ob := &originalImageBody{}
	if req.Body == nil || req.Body == http.NoBody {
		ob.eof = true
		return ob, nil
	}
	ob.hasBody = true
	read, err := io.ReadAll(io.LimitReader(req.Body, maxCaptureBody+1))
	if err != nil {
		return nil, err
	}
	ob.body = read
	if len(read) > maxCaptureBody {
		ob.body = read[:maxCaptureBody]
		ob.truncated = true
	} else {
		// ReadAll stopped at the body's end, not at the capture cap.
		ob.eof = true
	}
	old := req.Body
	ob.bytes = int64(len(read))
	var rest io.Reader
	if req.ContentLength > 0 {
		ob.bytes = req.ContentLength
		rest = &countingReader{rc: old, eof: &ob.eof}
	} else {
		// A chunked upload has no total up front: count the rest as it passes.
		rest = &countingReader{rc: old, n: &ob.bytes, eof: &ob.eof}
	}
	req.Body = &prefixedBody{Reader: io.MultiReader(bytes.NewReader(read), rest), Closer: old}
	return ob, nil
}

// countingReader tallies the client body bytes as the request streams them and
// records that the body reached its end.
type countingReader struct {
	rc  io.ReadCloser
	n   *int64 // nil when the total came from Content-Length
	eof *bool
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	if c.n != nil {
		*c.n += int64(n)
	}
	if err == io.EOF {
		*c.eof = true
	}
	return n, err
}

// prefixedBody replays the prefix read for the Activity capture and then the
// rest of the client's body, keeping the original Close.
type prefixedBody struct {
	io.Reader
	io.Closer
}

// proxyImage serves a local stanza that declares an image_pipeline. It keeps
// the client's original body for Activity, runs the request steps, then proxies
// the request; a reply that rejects a LoRA path warms the cache and retries
// once, and response steps can add headers and ask for one retry.
func (r *Router) proxyImage(w http.ResponseWriter, req *http.Request, t Target, m *Managed, baseURL, model, target string, s *config.Stanza) {
	steps := r.imageSteps(s)
	notes := &imageNotes{}
	clients, err := captureOriginalImageBody(req)
	if err != nil {
		r.writeBodyError(w, req, err)
		return
	}
	req = req.WithContext(context.WithValue(context.WithValue(req.Context(), originalImageBodyKey{}, clients), imageNotesKey{}, notes))
	if err := filterLocal(req, s); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	for _, st := range steps {
		if st.Rewrite == nil {
			continue
		}
		if err := st.Rewrite(req, w.Header(), notes); err != nil {
			var rej imageReject
			if !errors.As(err, &rej) {
				r.writeBodyError(w, req, err)
				return
			}
			// The sd-server shim's header name, so existing clients and logs still match.
			w.Header().Set("X-SD-Shim", "reject")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": string(rej)})
			return
		}
	}

	// A retry needs the rewritten body again. Remember it before the first send.
	snap, err := r.snapshotBody(req, r.cfg().RequestBodyLimit(s.ModelID))
	if err != nil {
		r.writeBodyError(w, req, err)
		return
	}

	// Warm the LoRA cache before a request that names one. sd-server starts
	// with an empty cache, so the first such request would be rejected.
	lora := m != nil && snap.requestsLoRA()
	if lora {
		m.warmLoRAs(false)
	}

	inspect := false
	for _, st := range steps {
		if st.After != nil {
			inspect = true
			break
		}
	}
	if !inspect && !lora {
		r.proxyJobAware(w, req, t, baseURL, model, target)
		return
	}

	req = req.WithContext(context.WithValue(req.Context(), preparedBodyKey{}, snap))

	// One capture for the whole exchange: a retried attempt must not leave a
	// second Activity entry or inflight count for the discarded reply.
	wrapped, done := r.beginProxy(w, req, model, target)
	defer done()
	send := func(dst *imageReply) {
		if err := snap.install(req); err != nil {
			http.Error(dst, "could not replay request body", http.StatusInternalServerError)
			return
		}
		r.proxyJobAwareWriter(dst, req, t, baseURL, model, target)
	}

	held := newImageReply(wrapped, maxImageBody)
	send(held)
	if held.committed {
		return
	}
	carried := http.Header{}
	loraRetried := false
	for {
		before := held.Header().Clone()
		retry := false
		for _, st := range steps {
			if st.After != nil && st.After(req, held.status(), held.Header(), held.buf.Bytes(), notes) {
				retry = true
			}
		}
		// sd-server rejected a LoRA request before the cache was warm. Warm
		// again and retry once; a second failure goes to the client as is.
		if lora && !loraRetried && responseIsInvalidLoRA(held.status(), held.buf.Bytes()) {
			m.warmLoRAs(true)
			loraRetried = true
			retry = true
		}
		for k, v := range headerDelta(before, held.Header()) {
			carried[k] = v
		}
		if !retry {
			break
		}
		held.reset()
		send(held)
		if held.committed {
			return
		}
	}
	carryHeaders(held.Header(), carried)
	held.commit()
}

// headerDelta returns the headers in after that differ from before.
func headerDelta(before, after http.Header) http.Header {
	d := http.Header{}
	for k, v := range after {
		if !slices.Equal(before[k], v) {
			d[k] = v
		}
	}
	return d
}

// carryHeaders adds headers an earlier attempt set, keeping any value the
// final reply set for the same name.
func carryHeaders(h, added http.Header) {
	for k, v := range added {
		if _, ok := h[k]; !ok {
			h[k] = v
		}
	}
}

// imageReply holds a backend reply back from the client so pipeline steps can
// inspect it. Over max bytes it commits and streams the rest, so a large image
// response is not buffered whole; a committed reply is not inspected.
// shortcut: a reply larger than maxImageBody is passed through uninspected, add streaming inspection if a step needs it.
type imageReply struct {
	dst       http.ResponseWriter
	h         http.Header
	code      int
	wrote     bool
	committed bool
	buf       bytes.Buffer
	max       int
}

func newImageReply(dst http.ResponseWriter, max int) *imageReply {
	return &imageReply{dst: dst, h: http.Header{}, max: max}
}

func (h *imageReply) Header() http.Header { return h.h }

func (h *imageReply) WriteHeader(code int) {
	if code < 200 {
		// A 100 Continue is not the final head: pass it through now.
		h.dst.WriteHeader(code)
		return
	}
	if h.committed || h.wrote {
		return
	}
	h.code, h.wrote = code, true
}

func (h *imageReply) Write(p []byte) (int, error) {
	if h.committed {
		return h.dst.Write(p)
	}
	if !h.wrote {
		h.WriteHeader(http.StatusOK)
	}
	if h.max > 0 && h.buf.Len()+len(p) > h.max {
		h.commit()
		return h.dst.Write(p)
	}
	return h.buf.Write(p)
}

func (h *imageReply) Flush() {
	if h.committed {
		_ = http.NewResponseController(h.dst).Flush()
	}
}

func (h *imageReply) status() int {
	if h.code == 0 {
		return http.StatusOK
	}
	return h.code
}

func (h *imageReply) reset() {
	h.h = http.Header{}
	h.code, h.wrote, h.committed = 0, false, false
	h.buf.Reset()
}

func (h *imageReply) commit() {
	if h.committed {
		return
	}
	h.committed = true
	dst := h.dst.Header()
	for k, v := range h.h {
		dst[k] = v
	}
	h.dst.WriteHeader(h.status())
	if h.buf.Len() > 0 {
		_, _ = h.dst.Write(h.buf.Bytes())
	}
}
