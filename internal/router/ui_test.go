package router

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The Logs page is one streamed transcript. The old five-card page and its
// interval poll must not come back. The JSON snapshot endpoint stays allowed:
// when a proxy holds the stream open, the page polls it as a fallback.
func TestEmbeddedUILogsPage(t *testing.T) {
	for _, gone := range []string{
		"Logs &amp; metrics", "Logs & metrics",
		"Router metrics", "Upstream metrics", "Router events", "Upstream logs", "Mesh logs",
		"setInterval(logs",
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
		`id="pg-tab-realtime"`, `id="pg-realtime"`, `id="rt-model"`, `id="rt-output"`, `id="rt-close"`,
		"window.openRealtime", "window.sendRealtime", "/v1/realtime", "api_type==='realtime'",
		`id="chat-output"`, `id="image-output"`, "window.sendChat", "window.generateImage",
		`id="image-cancel"`, "window.cancelImage", "Retry-After",
	} {
		if !strings.Contains(webUIHTML, want) {
			t.Errorf("embedded UI missing %q", want)
		}
	}
}

// Brand tokens, motion and the live activity table are part of the embedded shell.
func TestEmbeddedUIBrandAndLiveActivity(t *testing.T) {
	for _, want := range []string{
		`<link rel="icon" type="image/svg+xml" href="data:image/svg+xml,`, `<meta name="theme-color" content="#58d6e6">`,
		"--accent:#58d6e6", "--elev-1:", "--elev-2:", "--elev-3:", "--surface-1:",
		"@media(prefers-reduced-motion:reduce)", "animation:none!important", "transition:none!important",
		".spark-line{transition:d", ".thumb{", "width:44px",
		// Activity rows are reconciled by id, not rebuilt.
		"function syncRows", "tr.classList.add('leaving')", "row-in", "IntersectionObserver", "IMG_PATH",
		// The dialog has a parsed view, a raw fallback and collapsible headers.
		`id="capture-parsed"`, `id="capture-raw"`, `id="capture-toggle"`, `<details class="capture-headers" open>`,
		"function capParsed", "function chatReply", "function imgSrcs",
		// A model shows on every GPU it occupies.
		"m.gpus&&m.gpus.length?m.gpus:[m.gpu]",
	} {
		if !strings.Contains(webUIHTML, want) {
			t.Errorf("embedded UI missing %q", want)
		}
	}
	if strings.Contains(webUIHTML, "$('activity-rows').innerHTML=") {
		t.Error("activity rows are rebuilt with innerHTML again")
	}
}

// Some proxies hold a text/plain stream until it ends. The Logs page must stop
// waiting for headers and poll the JSON snapshot instead, and say so. A model
// source has no lines in the combined snapshot, so it polls its own.
func TestEmbeddedUILogsPollFallback(t *testing.T) {
	for _, want := range []string{
		"LOG_STREAM_WAIT", "slow=true;ac.abort()", "logPoll(next)",
		"/_router/logs/'+model", "j.lines", "Polling every 2 s",
	} {
		if !strings.Contains(webUIHTML, want) {
			t.Errorf("embedded UI missing %q", want)
		}
	}
	if strings.Contains(webUIHTML, "Stream blocked") {
		t.Error("the poll fallback still gives up on model sources")
	}
}

// The fallback reads these keys, so the snapshot must keep them.
func TestRouterLogsSnapshotFeedsUIFallback(t *testing.T) {
	srv, logger := logTestServer(t)
	seedLogs(logger)
	resp, err := http.Get(srv.URL + "/_router/logs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, k := range []string{"entries", "upstream", "mesh"} {
		var v []string
		if err := json.Unmarshal(raw[k], &v); err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		got[k] = v
	}
	for k, want := range map[string]string{"entries": "proxy-one", "upstream": "up-a1", "mesh": "mesh-one"} {
		if len(got[k]) == 0 || !strings.Contains(strings.Join(got[k], "\n"), want) {
			t.Errorf("%s missing %q: %v", k, want, got[k])
		}
	}
}
