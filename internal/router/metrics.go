package router

import (
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Prometheus histogram buckets. Duration covers everything from a fast
// embedding call to a long non-streaming generation; TTFT only makes sense
// for streaming responses so it gets a tighter ladder.
var (
	durationBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 30, 60, 120, 300}
	ttftBuckets     = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 30}
)

// histogram is a fixed-bucket histogram with cumulative counts, which is the
// shape Prometheus expects for _bucket series. Counts are bumped at observe
// time so rendering stays a straight walk over the slices.
type histogram struct {
	counts []uint64
	sum    float64
	count  uint64
}

func newHistogram(n int) *histogram { return &histogram{counts: make([]uint64, n)} }

func (h *histogram) observe(bounds []float64, v float64) {
	for i := range bounds {
		if v <= bounds[i] {
			h.counts[i]++
		}
	}
	h.sum += v
	h.count++
}

// modelMetrics is the per-model slice of the collector. One struct per model
// keeps label lookups cheap and cardinality bounded by the catalog size.
type modelMetrics struct {
	reqs       map[string]uint64 // status class (2xx, 4xx, …) -> count
	dur        *histogram
	ttft       *histogram
	prompt     uint64
	completion uint64
}

// Metrics collects request counters, latency histograms, and token totals for
// the Prometheus exposition on /metrics. It is deliberately dependency-free:
// the router ships as a single static binary and this is a few hundred lines
// of text formatting instead of a client library and its transitive deps.
//
// Everything is safe for concurrent use. Observe is called from the request
// completion path; Render is called by the /metrics handler.
type Metrics struct {
	start    time.Time
	mu       sync.Mutex
	models   map[string]*modelMetrics
	inflight int64 // atomic: requests currently being proxied
}

// NewMetrics returns an empty collector stamped with the process start time.
func NewMetrics() *Metrics {
	return &Metrics{start: time.Now(), models: map[string]*modelMetrics{}}
}

// IncInflight and DecInflight track requests currently in flight. They are
// atomic because they run outside the model lock on the hot path. A nil
// receiver is a no-op so a partially-built Router in a test cannot panic.
func (m *Metrics) IncInflight() {
	if m == nil {
		return
	}
	atomic.AddInt64(&m.inflight, 1)
}

func (m *Metrics) DecInflight() {
	if m == nil {
		return
	}
	atomic.AddInt64(&m.inflight, -1)
}

// Inflight reports the current in-flight request count.
func (m *Metrics) Inflight() int64 {
	if m == nil {
		return 0
	}
	return atomic.LoadInt64(&m.inflight)
}

// statusClass buckets an HTTP status into the small label set used on the
// requests counter. Keeping this coarse avoids cardinality blow-ups while
// still letting an operator separate client errors from server errors.
func statusClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 300:
		return "3xx"
	case code >= 200:
		return "2xx"
	default:
		return "other"
	}
}

// Observe records one completed proxied request. A zero or empty model id is
// folded into "unknown" so the series still exists rather than vanishing.
func (m *Metrics) Observe(e ActivityEntry) {
	if m == nil {
		return
	}
	key := e.Model
	if key == "" {
		key = "unknown"
	}
	durSec := float64(e.DurationMs) / 1000
	if durSec < 0 {
		durSec = 0
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	mm := m.models[key]
	if mm == nil {
		mm = &modelMetrics{
			reqs: map[string]uint64{},
			dur:  newHistogram(len(durationBuckets)),
			ttft: newHistogram(len(ttftBuckets)),
		}
		m.models[key] = mm
	}
	mm.reqs[statusClass(e.Status)]++
	mm.dur.observe(durationBuckets, durSec)
	if e.Tokens.PromptTokens > 0 {
		mm.prompt += uint64(e.Tokens.PromptTokens)
	}
	if e.Tokens.CompletionTokens > 0 {
		mm.completion += uint64(e.Tokens.CompletionTokens)
	}
	if e.TTFTMs > 0 {
		mm.ttft.observe(ttftBuckets, float64(e.TTFTMs)/1000)
	}
}

// Render writes the collector in Prometheus text exposition format (0.0.4).
// Output is sorted so repeated scrapes diff cleanly.
func (m *Metrics) Render(node string) string {
	var sb strings.Builder
	if m == nil {
		return ""
	}

	fmt.Fprintf(&sb, "# HELP model_router_build_info Build metadata for this router process.\n")
	fmt.Fprintf(&sb, "# TYPE model_router_build_info gauge\n")
	fmt.Fprintf(&sb, "model_router_build_info{node=%q,go_version=%q} 1\n", node, runtime.Version())

	fmt.Fprintf(&sb, "# HELP model_router_uptime_seconds Seconds since this process started.\n")
	fmt.Fprintf(&sb, "# TYPE model_router_uptime_seconds gauge\n")
	fmt.Fprintf(&sb, "model_router_uptime_seconds{node=%q} %.1f\n", node, time.Since(m.start).Seconds())

	fmt.Fprintf(&sb, "# HELP model_router_inflight_requests Requests currently being proxied.\n")
	fmt.Fprintf(&sb, "# TYPE model_router_inflight_requests gauge\n")
	fmt.Fprintf(&sb, "model_router_inflight_requests{node=%q} %d\n", node, atomic.LoadInt64(&m.inflight))

	m.mu.Lock()
	defer m.mu.Unlock()

	keys := make([]string, 0, len(m.models))
	for k := range m.models {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	if len(keys) > 0 {
		fmt.Fprintf(&sb, "# HELP model_router_requests_total Proxied requests by model and status class.\n")
		fmt.Fprintf(&sb, "# TYPE model_router_requests_total counter\n")
		for _, k := range keys {
			mm := m.models[k]
			classes := make([]string, 0, len(mm.reqs))
			for c := range mm.reqs {
				classes = append(classes, c)
			}
			sort.Strings(classes)
			for _, c := range classes {
				fmt.Fprintf(&sb, "model_router_requests_total{model=%q,status=%q} %d\n", k, c, mm.reqs[c])
			}
		}

		fmt.Fprintf(&sb, "# HELP model_router_request_duration_seconds End-to-end proxied request duration.\n")
		fmt.Fprintf(&sb, "# TYPE model_router_request_duration_seconds histogram\n")
		for _, k := range keys {
			mm := m.models[k]
			if mm.dur.count == 0 {
				continue
			}
			for i, b := range durationBuckets {
				fmt.Fprintf(&sb, "model_router_request_duration_seconds_bucket{model=%q,le=%q} %d\n", k, formatBound(b), mm.dur.counts[i])
			}
			fmt.Fprintf(&sb, "model_router_request_duration_seconds_bucket{model=%q,le=\"+Inf\"} %d\n", k, mm.dur.count)
			fmt.Fprintf(&sb, "model_router_request_duration_seconds_sum{model=%q} %.3f\n", k, mm.dur.sum)
			fmt.Fprintf(&sb, "model_router_request_duration_seconds_count{model=%q} %d\n", k, mm.dur.count)
		}

		fmt.Fprintf(&sb, "# HELP model_router_request_ttft_seconds Time to first generated token on streaming responses.\n")
		fmt.Fprintf(&sb, "# TYPE model_router_request_ttft_seconds histogram\n")
		for _, k := range keys {
			mm := m.models[k]
			if mm.ttft.count == 0 {
				continue
			}
			for i, b := range ttftBuckets {
				fmt.Fprintf(&sb, "model_router_request_ttft_seconds_bucket{model=%q,le=%q} %d\n", k, formatBound(b), mm.ttft.counts[i])
			}
			fmt.Fprintf(&sb, "model_router_request_ttft_seconds_bucket{model=%q,le=\"+Inf\"} %d\n", k, mm.ttft.count)
			fmt.Fprintf(&sb, "model_router_request_ttft_seconds_sum{model=%q} %.3f\n", k, mm.ttft.sum)
			fmt.Fprintf(&sb, "model_router_request_ttft_seconds_count{model=%q} %d\n", k, mm.ttft.count)
		}

		fmt.Fprintf(&sb, "# HELP model_router_tokens_total Tokens reported by the upstream, by kind.\n")
		fmt.Fprintf(&sb, "# TYPE model_router_tokens_total counter\n")
		for _, k := range keys {
			mm := m.models[k]
			if mm.prompt > 0 {
				fmt.Fprintf(&sb, "model_router_tokens_total{model=%q,kind=\"prompt\"} %d\n", k, mm.prompt)
			}
			if mm.completion > 0 {
				fmt.Fprintf(&sb, "model_router_tokens_total{model=%q,kind=\"completion\"} %d\n", k, mm.completion)
			}
		}
	}

	return sb.String()
}

// formatBound renders a bucket boundary the way Prometheus expects: no
// trailing zeros, no exponent for the ranges used here.
func formatBound(b float64) string {
	return strconv.FormatFloat(b, 'g', -1, 64)
}
