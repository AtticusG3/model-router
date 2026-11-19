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
	for _, v := range []string{"dashboard", "playground", "activity", "performance", "logs"} {
		if !strings.Contains(webUIHTML, `data-view="`+v+`" aria-label="`) {
			t.Errorf("nav button %q has no aria-label", v)
		}
	}
}

// Chat and Image share one Playground nav item, with a tab and a panel for
// every route the playground can drive.
func TestEmbeddedUIPlaygroundTabs(t *testing.T) {
	for _, gone := range []string{"Chat lab", "Image lab", `data-view="chat"`, `data-view="image"`} {
		if strings.Contains(webUIHTML, gone) {
			t.Errorf("embedded UI still contains %q", gone)
		}
	}
	for _, want := range []string{
		`data-view="playground" aria-label="Playground"`, `role="tablist"`, `role="tabpanel"`,
		`id="pg-tab-chat"`, `id="pg-tab-image"`, `id="pg-tab-embeddings"`, `id="pg-tab-rerank"`,
		`id="pg-tab-speech"`, `id="pg-tab-transcription"`, `id="pg-tab-loadtest"`, `id="pg-tab-realtime"`,
		`id="pg-chat"`, `id="pg-image"`, `id="pg-embeddings"`, `id="pg-rerank"`,
		`id="pg-speech"`, `id="pg-transcription"`, `id="pg-loadtest"`, `id="pg-realtime"`,
		`id="chat-output"`, `id="image-output"`, "window.sendChat", "window.generateImage",
		"window.runEmbeddings", "window.runRerank", "window.runSpeech",
		"window.runTranscription", "window.runLoadTest", "window.toggleRealtime",
		"/v1/embeddings", "/v1/rerank", "/v1/audio/speech", "/v1/audio/transcriptions", "/v1/realtime",
		"api_type: embedding", "api_type: rerank",
	} {
		if !strings.Contains(webUIHTML, want) {
			t.Errorf("embedded UI missing %q", want)
		}
	}
}

// The last config reload shows in the top bar, fed by "reload" on /_router/status.
func TestEmbeddedUIReloadIndicator(t *testing.T) {
	for _, want := range []string{`id="reload" role="status"`, `id="reload-text"`, "status.reload"} {
		if !strings.Contains(webUIHTML, want) {
			t.Errorf("embedded UI missing %q", want)
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
