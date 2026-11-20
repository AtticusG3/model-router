package router

import (
	"strings"
	"testing"
)

func TestMetricsRenderCountersAndHistograms(t *testing.T) {
	m := NewMetrics()

	m.Observe(ActivityEntry{
		Model:      "ornith",
		Status:     200,
		DurationMs: 1200,
		TTFTMs:     180,
		Tokens:     TokenStats{PromptTokens: 40, CompletionTokens: 120, TokensPerSecond: 55.5},
	})
	m.Observe(ActivityEntry{
		Model:      "ornith",
		Status:     502,
		DurationMs: 250,
	})
	m.Observe(ActivityEntry{
		Model:      "qwen3-embedding-4b",
		Status:     200,
		DurationMs: 40,
		Tokens:     TokenStats{PromptTokens: 12},
	})

	out := m.Render("buster")

	for _, want := range []string{
		`model_router_build_info{node="buster"`,
		`model_router_uptime_seconds{node="buster"}`,
		`model_router_inflight_requests{node="buster"} 0`,
		`model_router_requests_total{model="ornith",status="2xx"} 1`,
		`model_router_requests_total{model="ornith",status="5xx"} 1`,
		`model_router_request_duration_seconds_count{model="ornith"} 2`,
		`model_router_request_ttft_seconds_count{model="ornith"} 1`,
		`model_router_tokens_total{model="ornith",kind="prompt"} 40`,
		`model_router_tokens_total{model="ornith",kind="completion"} 120`,
		`model_router_tokens_total{model="qwen3-embedding-4b",kind="prompt"} 12`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("render missing %q\n---\n%s", want, out)
		}
	}

	// A model with no completion tokens must not emit a zero-valued series.
	if strings.Contains(out, `kind="completion"} 0`) {
		t.Fatalf("zero token series should be omitted:\n%s", out)
	}

	// Histogram buckets must be cumulative: the 1s bucket for ornith sees the
	// 1.2s request excluded but the 250ms one included, so exactly one count.
	if !strings.Contains(out, `model_router_request_duration_seconds_bucket{model="ornith",le="1"} 1`) {
		t.Fatalf("cumulative bucket wrong:\n%s", out)
	}
	if !strings.Contains(out, `model_router_request_duration_seconds_bucket{model="ornith",le="+Inf"} 2`) {
		t.Fatalf("+Inf bucket wrong:\n%s", out)
	}
}

func TestMetricsInflight(t *testing.T) {
	m := NewMetrics()
	if got := m.Inflight(); got != 0 {
		t.Fatalf("initial inflight = %d, want 0", got)
	}
	m.IncInflight()
	m.IncInflight()
	if got := m.Inflight(); got != 2 {
		t.Fatalf("inflight = %d, want 2", got)
	}
	m.DecInflight()
	if got := m.Inflight(); got != 1 {
		t.Fatalf("inflight = %d, want 1", got)
	}
	if !strings.Contains(m.Render("n"), "model_router_inflight_requests{node=\"n\"} 1") {
		t.Fatal("render should reflect the live inflight gauge")
	}
}

func TestMetricsEmptyModelIsUnknown(t *testing.T) {
	m := NewMetrics()
	m.Observe(ActivityEntry{Status: 200, DurationMs: 10})
	if !strings.Contains(m.Render("n"), `model_router_requests_total{model="unknown",status="2xx"} 1`) {
		t.Fatal("empty model id should fold into unknown")
	}
}

// Media rows add a count and media seconds per api_type; a voice library
// change is not an api_type and is left out.
func TestMetricsMediaPerAPIType(t *testing.T) {
	m := NewMetrics()
	m.Observe(ActivityEntry{Model: "voice", Status: 200, Media: &MediaStats{Kind: "speech", Seconds: 1.5}})
	m.Observe(ActivityEntry{Model: "voice", Status: 200, Media: &MediaStats{Kind: "speech", Seconds: 2}})
	m.Observe(ActivityEntry{Model: "live", Status: 101, Media: &MediaStats{Kind: "realtime", Seconds: 30}})
	m.Observe(ActivityEntry{Model: "voice", Status: 200, Media: &MediaStats{Kind: voiceAdminKind}})
	out := m.Render("n")
	for _, want := range []string{
		`model_router_media_requests_total{api_type="speech"} 2`,
		`model_router_media_seconds_total{api_type="speech"} 3.500`,
		`model_router_media_requests_total{api_type="realtime"} 1`,
		`model_router_media_seconds_total{api_type="realtime"} 30.000`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("render missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, `api_type="admin"`) {
		t.Fatalf("voice admin row counted as an api_type:\n%s", out)
	}
}

func TestMetricsNilSafe(t *testing.T) {
	var m *Metrics
	m.IncInflight() // must not panic; callers guard with a nil check but be safe
	m.Observe(ActivityEntry{})
	m.DecInflight()
}

func TestSanitizeLogLine(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain line", "plain line"},
		{"\x1b[31mred\x1b[0m text", "red text"},
		{"progress\r100%", "progress100%"},
	}
	for _, c := range cases {
		if got := sanitize(c.in); got != c.want {
			t.Fatalf("sanitize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	long := strings.Repeat("x", maxRingLine+500)
	if got := sanitize(long); len([]rune(got)) > maxRingLine+1 {
		t.Fatalf("long line not capped: %d runes", len([]rune(got)))
	}
}

func TestLoggerRingHasTimestamp(t *testing.T) {
	var sink strings.Builder
	l := NewLogger(&sink, true)
	l.Infof("started %s", "model")

	recent := l.RecentLogs(10)
	if len(recent) != 1 {
		t.Fatalf("recent = %d, want 1", len(recent))
	}
	line := recent[0]
	if len(line) < len("15:04:05.000 ") || line[2] != ':' || line[5] != ':' {
		t.Fatalf("ring line missing time prefix: %q", line)
	}
	if !strings.Contains(line, "[router] started model") {
		t.Fatalf("ring line missing message: %q", line)
	}
	// stdout stays unstamped so journald does not get a duplicate timestamp.
	if strings.Contains(sink.String(), "15:04:05") {
		t.Fatalf("stdout should not carry the ring timestamp: %q", sink.String())
	}
}

func TestSummariseBackendMetrics(t *testing.T) {
	body := `# HELP llamacpp:prompt_tokens_seconds Average prompt throughput in tokens/s.
# TYPE llamacpp:prompt_tokens_seconds gauge
llamacpp:prompt_tokens_seconds 812.5
llamacpp:predicted_tokens_seconds 41.25
llamacpp:kv_cache_usage_ratio 0.375
llamacpp:requests_processing 2
llamacpp:tokens_predicted_total 9999
llamacpp:requests_deferred nope`
	got := summariseBackendMetrics(body)
	want := map[string]float64{"prompt_tps": 812.5, "gen_tps": 41.25, "kv_cache_ratio": 0.375, "processing": 2}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if got := summariseBackendMetrics("dial tcp 127.0.0.1:1: connection refused"); len(got) != 0 {
		t.Errorf("error text produced series: %v", got)
	}
}
