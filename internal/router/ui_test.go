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
		"/_router/logs", "setInterval(logs",
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
	for _, v := range []string{"dashboard", "playground", "activity", "logs"} {
		if !strings.Contains(webUIHTML, `data-view="`+v+`" aria-label="`) {
			t.Errorf("nav button %q has no aria-label", v)
		}
	}
}

// Chat and Image share one Playground nav item with accessible tabs.
func TestEmbeddedUIPlaygroundTabs(t *testing.T) {
	for _, gone := range []string{"Chat lab", "Image lab", `data-view="chat"`, `data-view="image"`} {
		if strings.Contains(webUIHTML, gone) {
			t.Errorf("embedded UI still contains %q", gone)
		}
	}
	for _, want := range []string{
		`data-view="playground" aria-label="Playground"`, `role="tablist"`,
		`id="pg-tab-chat"`, `id="pg-tab-image"`, `id="pg-chat"`, `id="pg-image"`,
		`id="pg-tab-embeddings"`, `id="pg-embeddings"`, `id="embed-model"`, `id="embed-output"`,
		"window.sendEmbedding", "/v1/embeddings", "api_type==='embedding'",
		`id="pg-tab-rerank"`, `id="pg-rerank"`, `id="rerank-model"`, `id="rerank-output"`,
		"window.sendRerank", "/v1/rerank", "api_type==='rerank'",
		`id="pg-tab-speech"`, `id="pg-speech"`, `id="speech-model"`, `id="speech-output"`,
		"window.sendSpeech", "/v1/audio/speech", "api_type==='speech'",
		`id="pg-tab-transcription"`, `id="pg-transcription"`, `id="transcribe-model"`, `id="transcribe-output"`,
		"window.sendTranscription", "/v1/audio/transcriptions", "api_type==='transcription'",
		`id="pg-tab-load"`, `id="pg-load"`, `id="load-model"`, `id="load-output"`, `id="load-cancel"`,
		"window.startLoad", "window.cancelLoad", "AbortController",
		`id="chat-output"`, `id="image-output"`, "window.sendChat", "window.generateImage",
	} {
		if !strings.Contains(webUIHTML, want) {
			t.Errorf("embedded UI missing %q", want)
		}
	}
}
