package router

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"sync"
)

// bodyPrefixBytes is how much of a multipart body the matcher reads to find the
// model field before it decides to stream or spool the rest.
const bodyPrefixBytes = 64 << 10

// bodyMemoryBytes is the largest request body a retry loop keeps in memory;
// anything larger is spooled to a temp file.
const bodyMemoryBytes = 1 << 20

// errBodyTooLarge is returned when a request body is over the configured cap.
var errBodyTooLarge = errors.New("request body too large")

// tooLargeError is the 413 body: it names the cap so a client can size the next
// upload.
func tooLargeError(limit int64) error {
	return fmt.Errorf("%w: max_request_body_mb is %d MB", errBodyTooLarge, limit>>20)
}

// cleanups collects the temp files one request made, run when its handler ends.
type cleanups struct {
	mu  sync.Mutex
	fns []func()
}

type cleanupsKey struct{}

// withCleanups installs the per-request temp-file list on req.
func withCleanups(req *http.Request) (*cleanups, *http.Request) {
	c := &cleanups{}
	return c, req.WithContext(context.WithValue(req.Context(), cleanupsKey{}, c))
}

// trackCleanup registers fn to run when the request handler that owns req ends.
func trackCleanup(req *http.Request, fn func()) {
	if c, ok := req.Context().Value(cleanupsKey{}).(*cleanups); ok && c != nil {
		c.mu.Lock()
		c.fns = append(c.fns, fn)
		c.mu.Unlock()
	}
}

func (c *cleanups) run() {
	c.mu.Lock()
	fns := c.fns
	c.fns = nil
	c.mu.Unlock()
	for i := len(fns) - 1; i >= 0; i-- {
		fns[i]()
	}
}

// preparedBody is a request body the router can send more than once without
// holding a large payload in memory: small bodies stay in memory, larger ones
// live in a temp file deleted when the request ends.
type preparedBody struct {
	mem  []byte
	path string
	size int64
}

// readPrepared drains r into memory (at most bodyMemoryBytes) or a temp file.
// Over limit it returns errBodyTooLarge.
func readPrepared(r io.Reader, limit int64) (*preparedBody, error) {
	pref, err := io.ReadAll(io.LimitReader(r, bodyMemoryBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(pref)) > limit {
		return nil, tooLargeError(limit)
	}
	if int64(len(pref)) <= bodyMemoryBytes {
		return &preparedBody{mem: pref, size: int64(len(pref))}, nil
	}
	f, err := os.CreateTemp("", "model-router-body-*")
	if err != nil {
		return nil, err
	}
	n, err := f.Write(pref)
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, err
	}
	// Read one byte past the cap so an undeclared oversized body is caught.
	rest := io.LimitReader(r, limit-int64(n)+1)
	m, err := io.Copy(f, rest)
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, err
	}
	total := int64(n) + m
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return nil, err
	}
	if total > limit {
		os.Remove(f.Name())
		return nil, tooLargeError(limit)
	}
	return &preparedBody{path: f.Name(), size: total}, nil
}

// open returns a fresh reader over the body for one backend attempt.
func (b *preparedBody) open() (io.ReadCloser, error) {
	if b.path != "" {
		return os.Open(b.path)
	}
	return io.NopCloser(bytes.NewReader(b.mem)), nil
}

// cleanup deletes the temp file, if any. It is safe to call more than once: an
// error path and the request's cleanup hook may both run it.
func (b *preparedBody) cleanup() {
	if b.path != "" {
		os.Remove(b.path)
		b.path = ""
	}
}

// requestsLoRA reports whether the snapshotted body names a non-empty LoRA
// list. It streams the spooled copy instead of loading it whole.
func (b *preparedBody) requestsLoRA() bool {
	rc, err := b.open()
	if err != nil {
		return false
	}
	defer rc.Close()
	return bodyRequestsLoRA(rc)
}

// install puts a fresh copy of the body on req for one backend attempt.
func (b *preparedBody) install(req *http.Request) error {
	rc, err := b.open()
	if err != nil {
		return err
	}
	req.Body = rc
	req.ContentLength = b.size
	return nil
}

// snapshotBody drains req.Body once into a replayable body so a retry loop can
// send it again, and installs the first copy on req. A body over the cap gets
// 413; a large one is spooled to a temp file deleted when the handler returns.
func (r *Router) snapshotBody(req *http.Request, limit int64) (*preparedBody, error) {
	if req.Body == nil || req.Body == http.NoBody {
		p := &preparedBody{size: 0}
		return p, p.install(req)
	}
	p, err := readPrepared(req.Body, limit)
	if err != nil {
		return nil, err
	}
	if p.path != "" {
		trackCleanup(req, p.cleanup)
	}
	if err := p.install(req); err != nil {
		p.cleanup()
		return nil, err
	}
	return p, nil
}

// limitBody fails a chunked request body that grows past n bytes with err (a
// tooLargeError), so the backend request is cut off and proxyTo answers 413
// instead of forwarding a short body.
// shortcut: a chunked body over the cap is caught mid-stream after the model loads, spool chunked uploads first if that load cost starts to matter.
type limitBody struct {
	rc   io.ReadCloser
	n    int64
	read int64
	err  error
}

func (l *limitBody) Read(p []byte) (int, error) {
	if l.read > l.n {
		// Past the cap: keep failing without touching the client body, where
		// even a zero-length read can block on the next chunk header.
		return 0, l.err
	}
	if max := l.n + 1 - l.read; int64(len(p)) > max {
		p = p[:max]
	}
	n, err := l.rc.Read(p)
	l.read += int64(n)
	if l.read > l.n {
		// Not even the bytes under the cap from this read go on.
		return 0, l.err
	}
	return n, err
}

func (l *limitBody) Close() error { return l.rc.Close() }

// writeBodyError answers a request whose body could not be prepared: 413 over
// the cap, otherwise a generic 500 (the cause, such as a full temp dir, is a
// server fault and its message may name local paths, so it goes to the log).
// A body cut short because the client went away is answered 400 and not logged.
func (r *Router) writeBodyError(w http.ResponseWriter, req *http.Request, err error) {
	if errors.Is(err, errBodyTooLarge) {
		// Drain a bounded remainder so a client a little over the cap is not
		// reset mid-write and can read the 413. Far bigger uploads may still
		// see a reset when net/http closes the connection; that is inherent.
		_, _ = io.Copy(io.Discard, io.LimitReader(req.Body, 1<<20))
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	// A temp-file fault is the server's even if the request was cancelled too.
	var pathErr *fs.PathError
	if !errors.As(err, &pathErr) && (req.Context().Err() != nil || errors.Is(err, io.ErrUnexpectedEOF)) {
		http.Error(w, "request body ended early", http.StatusBadRequest)
		return
	}
	r.logger.Warnf("could not read request body: %v", err)
	http.Error(w, "could not read request body", http.StatusInternalServerError)
}
