package router

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync"

	"model-router/internal/config"
)

// imageStep is one step of a stanza's image_pipeline. The serving node runs it
// for requests from local clients and for requests proxied in from peers.
type imageStep struct {
	// Rewrite runs before the request reaches the backend and may replace
	// req.Body. nil is a no-op.
	Rewrite func(req *http.Request, notes *imageNotes) error
	// After inspects the backend reply. It may add headers to h and change req
	// headers for a retry; true sends the same rewritten body once more. A
	// second reply is not retried.
	After func(req *http.Request, status int, h http.Header, body []byte, notes *imageNotes) bool
}

// defaultImageSteps builds the steps a stanza's image_pipeline runs. It is
// empty, so declaring the option changes nothing until a build adds the steps.
func defaultImageSteps(*config.Stanza) []imageStep { return nil }

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

// imageNotesFrom returns the notes attached to a pipeline request, if any.
func imageNotesFrom(req *http.Request) []string {
	if n, ok := req.Context().Value(imageNotesKey{}).(*imageNotes); ok && n != nil {
		return n.all()
	}
	return nil
}

// originalImageBody is the client's request body as received, kept for the
// Activity capture when pipeline steps rewrite it.
type originalImageBody struct {
	body      []byte
	bytes     int64
	truncated bool
}

type originalImageBodyKey struct{}

// captureOriginalImageBody keeps the first maxCaptureBody bytes of the client's
// body and leaves the whole body replayable on req.
func captureOriginalImageBody(req *http.Request) (*originalImageBody, error) {
	ob := &originalImageBody{}
	if req.Body == nil || req.Body == http.NoBody {
		return ob, nil
	}
	read, err := io.ReadAll(io.LimitReader(req.Body, maxCaptureBody+1))
	if err != nil {
		return nil, err
	}
	ob.body = read
	if len(read) > maxCaptureBody {
		ob.body = read[:maxCaptureBody]
		ob.truncated = true
	}
	ob.bytes = int64(len(read))
	if req.ContentLength > 0 {
		ob.bytes = req.ContentLength
	}
	old := req.Body
	req.Body = &prefixedBody{Reader: io.MultiReader(bytes.NewReader(read), old), Closer: old}
	return ob, nil
}

// prefixedBody replays the prefix read for the Activity capture and then the
// rest of the client's body, keeping the original Close.
type prefixedBody struct {
	io.Reader
	io.Closer
}

// proxyImage serves a local stanza that declares an image_pipeline. It keeps
// the client's original body for Activity, runs the request steps, then proxies
// the request; response steps can add headers and ask for one retry.
func (r *Router) proxyImage(w http.ResponseWriter, req *http.Request, t Target, baseURL, model, target string, s *config.Stanza) {
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
		if err := st.Rewrite(req, notes); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	inspect := false
	for _, st := range steps {
		if st.After != nil {
			inspect = true
			break
		}
	}
	if !inspect {
		r.proxyJobAware(w, req, t, baseURL, model, target)
		return
	}

	// A retry needs the rewritten body again. Remember it before the first send.
	snap, err := r.snapshotBody(req, r.cfg().RequestBodyLimit(s.ModelID))
	if err != nil {
		r.writeBodyError(w, req, err)
		return
	}
	send := func(dst http.ResponseWriter) {
		if err := snap.install(req); err != nil {
			http.Error(dst, "could not replay request body", http.StatusInternalServerError)
			return
		}
		r.proxyJobAware(dst, req, t, baseURL, model, target)
	}

	held := newImageReply(w, maxImageBody)
	send(held)
	if held.committed {
		return
	}
	retry := false
	for _, st := range steps {
		if st.After != nil && st.After(req, held.status(), held.Header(), held.buf.Bytes(), notes) {
			retry = true
		}
	}
	if retry {
		held.reset()
		send(held)
		if held.committed {
			return
		}
		for _, st := range steps {
			if st.After != nil {
				st.After(req, held.status(), held.Header(), held.buf.Bytes(), notes)
			}
		}
	}
	held.commit()
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
