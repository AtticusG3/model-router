package router

import (
	"strings"
	"testing"
)

// The Logs page is one streamed transcript. The old five-card page and its
// poll must not come back.
func TestEmbeddedUILogsPage(t *testing.T) {
	for _, gone := range []string{
		"Logs &amp; metrics", "Logs & metrics",
		"Router metrics", "Upstream metrics", "Router events", "Upstream logs", "Mesh logs",
		"/_router/logs", "setInterval(logs", "Raw metrics",
	} {
		if strings.Contains(webUIHTML, gone) {
			t.Errorf("embedded UI still contains %q", gone)
		}
	}
	for _, want := range []string{
		`data-view="logs" aria-label="Logs"`, `id="log-output"`, `id="log-model"`,
		`data-src="proxy"`, `data-src="upstream"`, `data-src="mesh"`, "/logs/stream",
	} {
		if !strings.Contains(webUIHTML, want) {
			t.Errorf("embedded UI missing %q", want)
		}
	}
	// Every nav button needs a name that survives the label being hidden.
	for _, v := range []string{"dashboard", "chat", "image", "activity", "performance", "logs"} {
		if !strings.Contains(webUIHTML, `data-view="`+v+`" aria-label="`) {
			t.Errorf("nav button %q has no aria-label", v)
		}
	}
}

// The Performance page shows gauges and charts. It must not render
// Prometheus text, and its poll must start and stop with the view.
func TestEmbeddedUIPerformancePage(t *testing.T) {
	for _, want := range []string{
		`data-view="performance"`, `id="performance"`, `id="perf-gpus"`, `id="perf-models"`,
		`id="perf-chart-rate"`, `id="perf-peer-list"`, `href="/metrics"`,
		"/_router/backend-metrics", "perfSet(view==='performance')", "document.hidden", "clearInterval(perfTimer)",
	} {
		if !strings.Contains(webUIHTML, want) {
			t.Errorf("embedded UI missing %q", want)
		}
	}
	for _, gone := range []string{"# TYPE", "# HELP", "renderMetricsText", "summarizeMetrics"} {
		if strings.Contains(webUIHTML, gone) {
			t.Errorf("embedded UI still contains %q", gone)
		}
	}
}
