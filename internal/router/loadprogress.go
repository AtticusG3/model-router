package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// loadProgressInterval is how often a held streaming request checks the
// backend's load progress. A var so tests can shorten it.
var loadProgressInterval = time.Second

// wantsStream reports whether req is a chat completion asking for a stream.
// Only a JSON body is buffered by the matcher (others stream), so only a JSON
// body is read here and restored.
func wantsStream(req *http.Request) bool {
	if req.Method != http.MethodPost || req.URL.Path != "/v1/chat/completions" || req.Body == nil ||
		!strings.Contains(req.Header.Get("Content-Type"), "application/json") {
		return false
	}
	body, err := io.ReadAll(req.Body)
	req.Body = io.NopCloser(bytes.NewReader(body))
	var v struct {
		Stream bool `json:"stream"`
	}
	return err == nil && json.Unmarshal(body, &v) == nil && v.Stream
}

// loadStream writes loading chunks ahead of the proxied response. Once it has
// started the stream, the proxy's own WriteHeader is dropped (the 200 is out).
type loadStream struct {
	http.ResponseWriter
	started bool
}

func (l *loadStream) Unwrap() http.ResponseWriter { return l.ResponseWriter }

func (l *loadStream) WriteHeader(code int) {
	if !l.started {
		l.ResponseWriter.WriteHeader(code)
	}
}

// event sends one SSE data line, opening the stream on the first call.
func (l *loadStream) event(payload any) {
	if !l.started {
		h := l.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Accel-Buffering", "no")
		l.ResponseWriter.WriteHeader(http.StatusOK)
		l.started = true
	}
	b, _ := json.Marshal(payload)
	_, _ = fmt.Fprintf(l.ResponseWriter, "data: %s\n\n", b)
	_ = http.NewResponseController(l.ResponseWriter).Flush()
}

// chunk puts text in the reasoning stream, which chat UIs show as "thinking".
func (l *loadStream) chunk(text string) {
	l.event(map[string]any{
		"object": "chat.completion.chunk",
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"role": "assistant", "reasoning_content": text},
		}},
	})
}

// fail ends the stream with an error event the client can show.
func (l *loadStream) fail(err error) {
	l.event(map[string]any{"error": map[string]any{"message": err.Error(), "type": "load_failed"}})
	_, _ = io.WriteString(l.ResponseWriter, "data: [DONE]\n\n")
	_ = http.NewResponseController(l.ResponseWriter).Flush()
}

// loadReportingProgress runs load and, while it is still going, writes a
// loading chunk each time the reported progress changes. A load that finishes
// before the first tick writes nothing.
func (r *Router) loadReportingProgress(lw *loadStream, req *http.Request, key string) error {
	done := make(chan error, 1)
	go func() {
		_, err := r.load(req.Context(), key, true, true)
		done <- err
	}()
	tick := time.NewTicker(loadProgressInterval)
	defer tick.Stop()
	last := ""
	for {
		select {
		case err := <-done:
			return err
		case <-tick.C:
			r.mu.Lock()
			m := r.managed[key]
			r.mu.Unlock()
			text := "loading model…"
			if m != nil {
				m.mu.Lock()
				// shortcut: assumes progress is a 0-1 fraction as llama.cpp reports it, upgrade if a backend reports percent.
				if m.hasProgress {
					text = fmt.Sprintf("loading model… %d%%", int(min(max(m.progress, 0), 1)*100))
				}
				m.mu.Unlock()
			}
			if text != last {
				last = text
				lw.chunk(text + "\n")
			}
		}
	}
}
