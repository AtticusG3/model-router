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

// The Speech tab lists the model's voices from /v1/audio/voices (only for a
// running model, so the tab never starts one), offers every response_format,
// and plays through MediaSource when the browser can, else from a blob.
func TestEmbeddedUISpeechVoicesAndFormats(t *testing.T) {
	if strings.Contains(webUIHTML, `<input id="speech-voice"`) {
		t.Error("the voice is still a free-text input, not the voice list")
	}
	for _, want := range []string{
		`<select id="speech-voice" aria-label="Voice">`, `onchange="loadVoices()"`,
		"window.loadVoices", "'/v1/audio/voices?model='+encodeURIComponent(model)",
		"m.state==='running'", "d.data||d.voices", "if(t.dataset.tab==='speech')loadVoices()",
		`<select id="speech-format" aria-label="Audio format">`, "response_format:format",
		"MediaSource.isTypeSupported(ct)", "function streamInto", "sb.appendBuffer(value)", "await r.blob()",
	} {
		if !strings.Contains(webUIHTML, want) {
			t.Errorf("embedded UI missing %q", want)
		}
	}
	for _, f := range []string{"mp3", "opus", "aac", "flac", "wav", "pcm"} {
		if !strings.Contains(webUIHTML, `<option value="`+f+`"`) {
			t.Errorf("format picker has no %s", f)
		}
	}
}

// The Transcription tab sends the chosen response format and language, and
// offers Copy and Download for subtitle results only.
func TestEmbeddedUITranscriptionOptions(t *testing.T) {
	for _, want := range []string{
		`<select id="transcribe-format">`, `<option value="json">`, `<option value="verbose_json">`,
		`<option value="text">`, `<option value="srt">`, `<option value="vtt">`,
		`<select id="transcribe-language">`, `<option value="">Backend default</option>`, `<option value="ja">`,
		"format=$('transcribe-format').value", "language=$('transcribe-language').value",
		"fd.append('response_format',format)", "if(language)fd.append('language',language)",
		`id="transcribe-actions" hidden`, "format==='srt'||format==='vtt'",
		"window.copyTranscript", "navigator.clipboard.writeText", "window.downloadTranscript", "a.download='transcript.'+",
	} {
		if !strings.Contains(webUIHTML, want) {
			t.Errorf("embedded UI missing %q", want)
		}
	}
}

// The Music & SFX tab round-trips an audio job: submit prompt, lyrics,
// duration and seed, poll the job until it ends, then fetch the content once
// for an audio player and a download link.
func TestEmbeddedUIAudioGenTab(t *testing.T) {
	for _, want := range []string{
		`id="pg-tab-audiogen"`, `aria-controls="pg-audiogen"`, `id="pg-audiogen" role="tabpanel" aria-labelledby="pg-tab-audiogen"`,
		`<select id="audiogen-model" aria-label="Audio generation model"><option value="">Path default</option>`, "api_type==='audio_gen'",
		`id="audiogen-prompt"`, `id="audiogen-lyrics"`, `id="audiogen-seconds"`, `id="audiogen-seed"`, `id="audiogen-output" class="output muted" aria-live="polite"`,
		"window.sendAudioGen", "fetch('/v1/audio/generations',{method:'POST'", "body.lyrics=lyrics", "body.seconds=seconds", "body.seed=Number(seed)",
		"'/v1/audio/generations/'+encodeURIComponent(job.id)", "while(job.status!=='completed')", "fetch(path+'/content')",
		`<audio controls aria-label="Generated audio"`, `download="'+esc(job.id)+'.'+esc(ext)+'">Download audio</a>`,
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
		// Image rows fetch the server-rendered thumbnail, with the capture as fallback.
		"'/_router/activity/'+id+'/thumb'", "function thumbSrc", "function captureSrc",
		// The row state commits only once the image decodes; a failed GET retries the
		// captured body. A decode failure on the server is surfaced as thumb_issue.
		"function showThumb", "im.onload=", "im.onerror=", "e.thumb_issue",
		// Media rows swap token stats for a per-kind summary, and the dialog has a
		// renderer per kind: player or "not captured", transcript, job link, session.
		"function mediaCells", "function capMedia", "e.media?mediaCells(e.media,e)",
		"case 'speech':", "case 'transcription':", "case 'video':case 'audio_gen':", "case 'realtime':",
		"m.playable?'<audio controls", "/_router/activity/'+esc(e.id)+'/audio", ", not captured.",
		"m.input_chars", "m.voice", "m.file_name", "m.text", "m.job_id", "/content", "m.state==='completed'",
		"m.frames_in", "m.frames_out", "m.close_code", "Voice library",
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
