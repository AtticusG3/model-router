package router

import (
	"bytes"
	"encoding/json"
	"io"
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
}

// ActivityEntry is one proxied generation (or other model) request.
type ActivityEntry struct {
	ID          int        `json:"id"`
	Timestamp   time.Time  `json:"timestamp"`
	Model       string     `json:"model"`
	Target      string     `json:"target,omitempty"`
	Method      string     `json:"method"`
	Path        string     `json:"path"`
	Status      int        `json:"status"`
	ContentType string     `json:"content_type,omitempty"`
	DurationMs  int        `json:"duration_ms"`
	Tokens      TokenStats `json:"tokens"`
	HasCapture  bool       `json:"has_capture"`
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
	cap  ActivityCapture
	size int
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

func (a *ActivityLog) record(entry ActivityEntry, cap ActivityCapture) ActivityEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.nextID++
	entry.ID = a.nextID
	cap.ID = entry.ID
	size := len(cap.ReqBody) + len(cap.RespBody)
	for a.captureSz+size > maxCaptureBytes && len(a.captures) > 0 {
		a.dropOldestCapture()
	}
	if size > 0 && a.captureSz+size <= maxCaptureBytes {
		entry.HasCapture = true
		a.captures[entry.ID] = storedCapture{cap: cap, size: size}
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

func (r *Router) recordProxy(req *http.Request, rec *activityWriter, started time.Time, reqBody []byte, reqTrunc bool, model, target string) {
	if r.activity == nil {
		return
	}
	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}
	ct := rec.Header().Get("Content-Type")
	entry := ActivityEntry{
		Timestamp:   started,
		Model:       model,
		Target:      target,
		Method:      req.Method,
		Path:        req.URL.Path,
		Status:      status,
		ContentType: ct,
		DurationMs:  int(time.Since(started).Milliseconds()),
		Tokens:      parseTokenStats(rec.body.Bytes(), ct),
	}
	reqStr, clipped := clipBody(reqBody)
	respStr := rec.body.String()
	r.activity.record(entry, ActivityCapture{
		ReqHeaders:  copyHeaderMap(req.Header),
		ReqBody:     reqStr,
		RespHeaders: copyHeaderMap(rec.Header()),
		RespBody:    respStr,
		Truncated:   reqTrunc || clipped || rec.truncated,
	})
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
	rec := &activityWriter{ResponseWriter: w, max: maxCaptureBody}
	started := time.Now()
	return rec, func() {
		r.recordProxy(req, rec, started, captureReq, reqTrunc, model, target)
	}
}
