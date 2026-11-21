package router

import (
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"model-router/internal/config"
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
		`id="pg-tab-video"`, `id="pg-video"`, `id="video-model"`, `id="video-output"`, `id="video-cancel"`,
		"window.sendVideo", "window.cancelVideo", "/v1/videos", "api_type==='video'",
		`id="chat-output"`, `id="image-output"`, "window.sendChat", "window.generateImage",
		`id="image-cancel"`, "window.cancelImage", "Retry-After",
	} {
		if !strings.Contains(webUIHTML, want) {
			t.Errorf("embedded UI missing %q", want)
		}
	}
}

// Every Playground tab has a history list and a Clear control, and history
// never reaches browser storage, so a reload clears it. sessionStorage only
// holds the running video job id.
func TestEmbeddedUIPlaygroundHistory(t *testing.T) {
	tabs := []string{"chat", "image", "embeddings", "rerank", "speech", "transcription", "video", "audiogen", "load", "realtime"}
	if n := strings.Count(webUIHTML, `role="tabpanel"`); n != len(tabs) {
		t.Errorf("found %d Playground tab panels, the history check covers %d", n, len(tabs))
	}
	for _, tab := range tabs {
		for _, want := range []string{
			`<ul id="hist-` + tab + `" class="history-list"`,
			`onclick="clearHistory('` + tab + `')">Clear history</button>`,
			tab + ":['",
		} {
			if !strings.Contains(webUIHTML, want) {
				t.Errorf("tab %s: embedded UI missing %q", tab, want)
			}
		}
	}
	for _, gone := range []string{"localStorage", "indexedDB"} {
		if strings.Contains(uiJS, gone) {
			t.Errorf("app.js uses %s", gone)
		}
	}
	video := strings.Count(uiJS, "sessionStorage.getItem(VIDEO_JOB_KEY)") +
		strings.Count(uiJS, "sessionStorage.setItem(VIDEO_JOB_KEY,") +
		strings.Count(uiJS, "sessionStorage.removeItem(VIDEO_JOB_KEY)")
	if n := strings.Count(uiJS, "sessionStorage."); n != video {
		t.Errorf("app.js makes %d sessionStorage calls, only %d are for the video job id", n, video)
	}
}

// The Speech tab lists the model's voices from /v1/audio/voices (only for a
// running model, so the tab never starts one) and unions that stanza's presets
// with the router's global voice library, offers every response_format, and
// plays through MediaSource when the browser can, else from a blob.
func TestEmbeddedUISpeechVoicesAndFormats(t *testing.T) {
	if strings.Contains(webUIHTML, `<input id="speech-voice"`) {
		t.Error("the voice is still a free-text input, not the voice list")
	}
	for _, want := range []string{
		`<select id="speech-voice" aria-label="Voice">`, `onchange="loadVoices()"`,
		"window.loadVoices", "'/v1/audio/voices?model='+encodeURIComponent(model)",
		"collect('/v1/audio/voices')", "ids=[...new Set(ids)]",
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

// The Video tab submits JSON or multipart to /v1/videos, resumes the job id
// from sessionStorage after a reload, polls with backoff, and ends it with
// DELETE. Optional fields depend on the model's capabilities.
func TestEmbeddedUIVideoTab(t *testing.T) {
	for _, want := range []string{
		`id="video-opt-duration" hidden`, `id="video-opt-resolution" hidden`, `id="video-opt-seed" hidden`,
		`id="video-seconds"`, `id="video-resolution"`, `id="video-seed"`,
		"['duration','resolution','seed']", `$('video-opt-'+k).hidden=!caps.includes(k)`,
		"body.seconds=", "body.size=", "body.seed=", "input_reference",
		"'Content-Type':'application/json'", "method:'DELETE'", "encodeURIComponent(id)",
		"VIDEO_JOB_KEY='router.video.job'", "sessionStorage.getItem(VIDEO_JOB_KEY)",
		"sessionStorage.setItem(VIDEO_JOB_KEY,d.id)", "sessionStorage.removeItem(VIDEO_JOB_KEY)",
		"videoDelay=Math.min(videoDelay*1.5,5000)", "videoFail(videoError(d))", "e.status===404",
		"Cancel failed: ",
		"d.status==='completed'", `'<video controls`, `download="video.'`, "Download video",
		"resumeVideo();refresh()", "Cancelled.",
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
		"function capParsed", "function chatReply", "function capImgs",
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

// The capture dialog names the clipped fields and opens a clipped body in the
// parsed view: an image placeholder shows the stored preview and a stream's
// dropped middle is marked. Only a raw cut is reported as such.
func TestEmbeddedUICaptureClippedFields(t *testing.T) {
	for _, want := range []string{
		"'Clipped to fit 256 KB: '+cap.clipped_fields.join(', ')", "Bodies cut at 256 KB as raw bytes. ",
		"setCapView(p.ok?'parsed':'raw')", "const CLIPPED_IMG=/^\\[image (\\d+): /",
		"else if(l.startsWith(': [clipped '))out[last]+=",
	} {
		if !strings.Contains(webUIHTML, want) {
			t.Errorf("embedded UI missing %q", want)
		}
	}
	if strings.Contains(webUIHTML, "Bodies truncated to 256 KB.") {
		t.Error("the dialog still reports every long body as truncated")
	}
}

// The parsed view shows request images (img2img inits and extras, chat
// image_url parts) and response images. A clipped placeholder loads the stored
// preview from the image endpoint; inline base64 is typed from its bytes, not
// guessed from its prefix. Each image opens full size in a new tab.
func TestEmbeddedUICaptureParsedImages(t *testing.T) {
	for _, want := range []string{
		"if(m)out.push('/_router/activity/'+id+'/image/'+side+'/'+m[1])",
		"h.startsWith('\\x89PNG')?'png':h.startsWith('\\xff\\xd8\\xff')?'jpeg'",
		"capImgs(req,e.id,'req').map(s=>capImg(s,'Input image'))",
		"capImgs(m&&m.content,e.id,'req').map(s=>capImg(s,'Message image'))",
		"const resultImgs=(d,id)=>capImgs(d&&(d.data||d.images),id,'resp')",
		"imgs.map(s=>capImg(s,'Generated image'))",
		`target="_blank" rel="noopener" title="Open full size in a new tab"`,
		"a.href=URL.createObjectURL(", "Image preview no longer stored",
		// A shed response preview falls back once to the row's thumbnail.
		"const thumb=im.src.replace(/\\/image\\/resp\\/0$/,'/thumb');",
		"if(thumb!==im.src){a.href=im.src=thumb;return}",
	} {
		if !strings.Contains(webUIHTML, want) {
			t.Errorf("embedded UI missing %q", want)
		}
	}
	if strings.Contains(webUIHTML, "v.startsWith('/9j/')") {
		t.Error("the parsed view still guesses the image type from the base64 prefix")
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

// The Activity view follows the live stream only while it is showing, and polls
// every 3 s when the stream never delivers its snapshot or the browser gives up.
func TestEmbeddedUIActivityStreamWithPollFallback(t *testing.T) {
	for _, want := range []string{
		"new EventSource('/_router/activity/stream')", "addEventListener('snapshot'", "addEventListener('done'",
		"actWait=setTimeout(actPoll,ACT_STREAM_WAIT)", "es.readyState===EventSource.CLOSED)return actPoll()",
		// A live stream that drops says so instead of still reading "Live".
		"$('activity-hint').textContent='Reconnecting to the live stream…'",
		"actPollTimer=setInterval(activity,3000)", "if(view!=='activity')actStop();else if(!wasAct)actStart()",
	} {
		if !strings.Contains(webUIHTML, want) {
			t.Errorf("embedded UI missing %q", want)
		}
	}
	if strings.Contains(webUIHTML, "refresh();activity();") {
		t.Error("Activity still polls from page load on every view")
	}
}

// The Activity view filters by path group above the table, Chat and SD API
// only by default, and the stat cards and sparklines follow the filter.
func TestEmbeddedUIActivityPathFilter(t *testing.T) {
	for _, want := range []string{
		`id="act-filter" role="group"`, `id="act-all" aria-pressed="false">All</button>`, `id="act-hidden"`,
		`data-group="chat" aria-pressed="true"`, `data-group="sdapi" aria-pressed="true"`,
		"ACT_DEFAULT='chat,sdapi'", "get('paths')", "history.replaceState(null,'','#paths='",
		"actRows.filter(e=>actShow.has(pathGroup(e.path))).filter(actMatch)", "hid+' hidden'",
	} {
		if !strings.Contains(webUIHTML, want) {
			t.Errorf("embedded UI missing %q", want)
		}
	}
	for _, g := range []string{"images", "embed", "completions", "audio", "video", "realtime", "other"} {
		if !strings.Contains(webUIHTML, `data-group="`+g+`" aria-pressed="false"`) {
			t.Errorf("group %s is missing or on by default", g)
		}
	}
}

// The Activity peer filter starts on All and Local, has an Origin column, and
// combines with the path-group filter: actRender applies both.
func TestEmbeddedUIActivityPeerFilter(t *testing.T) {
	for _, want := range []string{
		`<select class="chip" id="act-peer" aria-label="Filter activity by peer"><option value="">All</option><option value="local">Local</option></select>`,
		"<th>Model</th><th>Origin</th><th>Path</th>",
		"actRows.filter(e=>actShow.has(pathGroup(e.path))).filter(actMatch)", "$('act-peer').onchange=()=>actPaint(actRows)",
	} {
		if !strings.Contains(webUIHTML, want) {
			t.Errorf("embedded UI missing %q", want)
		}
	}
	// target is this node for every row served here, so it must not decide Local again.
	if strings.Contains(webUIHTML, "e.target===status.node)") {
		t.Error("the peer filter still reads target")
	}
}

// Rows the API builds carry origin, and the Activity view filters them by it
// (testdata/activity_origin_test.js): a client of this node, a request bravo
// forwarded here, one this node forwarded out to bravo, charlie's own requests
// from its telemetry, and a sender that is not a configured peer.
func TestEmbeddedUIActivityOriginFilter(t *testing.T) {
	cfg, err := config.Parse([]byte("start_port: 5900\npeers:\n  - name: bravo\n    kind: router\n    base_url: http://bravo.invalid\n    models: [chat]\n  - name: charlie\n    kind: router\n    base_url: http://charlie.invalid\n    models: [chat]\n"))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	r := New(cfg, NewLogger(io.Discard, false), "alpha")
	a := r.Activity()
	const chat, img = "/v1/chat/completions", "/sdapi/v1/txt2img"
	now := time.Now().UTC()
	for _, e := range []ActivityEntry{
		{Model: "client-chat", Target: "alpha", Path: chat},
		{Model: "client-embed", Target: "alpha", Path: "/v1/embeddings"},
		{Model: "forwarded-in", Target: "alpha", Path: chat, FromPeer: "bravo"},
		{Model: "unlisted-image", Target: "alpha", Path: img, FromPeer: "outsider"},
		{Model: "unlisted-image", Target: "alpha", Path: img, FromPeer: "outsider"},
		{Model: "forwarded-out", Target: "bravo", Path: chat},
	} {
		e.Timestamp, e.Method, e.Status = now, http.MethodPost, http.StatusOK
		a.record(e, ActivityCapture{}, nil, "", [2][]preview{})
	}
	a.begin(InProgressEntry{Model: "running-in", Target: "alpha", Method: http.MethodPost, Path: chat, Started: now, FromPeer: "outsider"})

	// charlie runs 4 and 5 for its own clients; 5 then finishes.
	peerRun := InProgressEntry{ID: 4, Model: "peer-run", Method: http.MethodPost, Path: chat, Started: now, Origin: originLocal}
	r.peers.Set("charlie", &Telemetry{Node: "charlie", InProgress: []InProgressEntry{peerRun, {ID: 5, Model: "peer-done", Method: http.MethodPost, Path: chat, Started: now, Origin: originLocal}}})
	seen := map[string]liveRow{}
	r.streamProgress(seen)
	r.peers.Set("charlie", &Telemetry{Node: "charlie", InProgress: []InProgressEntry{peerRun}, Recent: []RecentEntry{{ID: 5, Model: "peer-done", Path: chat, Status: http.StatusOK, Origin: originLocal}}})
	live, done := r.streamProgress(seen)

	snap := doJob(NewHandler(r, NewLogger(io.Discard, false)), http.MethodGet, "/_router/activity", "")
	var list struct {
		Entries    []ActivityEntry   `json:"entries"`
		InProgress []InProgressEntry `json:"in_progress"`
	}
	if err := json.Unmarshal(snap.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range list.Entries {
		got[e.Model] = e.Origin
	}
	for _, e := range list.InProgress {
		got[e.Model] = e.Origin
	}
	for _, e := range live {
		got[e.Model] = e.Origin
	}
	for _, e := range done {
		got[e.Model] = e.Origin
	}
	want := map[string]string{
		"client-chat": "local", "client-embed": "local", "forwarded-in": "bravo", "unlisted-image": "outsider",
		"forwarded-out": "local", "running-in": "outsider", "peer-run": "charlie", "peer-done": "charlie",
	}
	if !maps.Equal(got, want) {
		t.Fatalf("origins %v, want %v", got, want)
	}
	if rec := r.TelemetrySnapshot().Recent; len(rec) != 6 || rec[0].Origin != "local" || rec[2].Origin != "outsider" {
		t.Fatalf("telemetry recent = %+v", rec)
	}

	fx, err := json.Marshal(map[string]any{
		"status":   map[string]any{"node": "alpha", "peers": map[string]any{"bravo": map[string]any{}, "charlie": map[string]any{}}},
		"snapshot": json.RawMessage(snap.Body.Bytes()),
		"progress": map[string]any{"in_progress": live},
		"done":     done[0],
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "fixtures.json")
	if err := os.WriteFile(path, fx, 0o600); err != nil {
		t.Fatal(err)
	}
	runUIScript(t, "activity_origin_test.js", path)
}

// Every generationPathSuffixes entry has a group in the UI's ACT_PATH_GROUP
// table, every group has a chip, and no earlier suffix shadows a later one
// of another group (pathGroup takes the first match).
func TestUIPathGroupsCoverGenerationSuffixes(t *testing.T) {
	m := regexp.MustCompile(`const ACT_PATH_GROUP=\{([^}]*)\}`).FindStringSubmatch(uiAppJS)
	if m == nil {
		t.Fatal("ACT_PATH_GROUP not found in app.js")
	}
	var keys []string
	group := map[string]string{}
	for _, kv := range regexp.MustCompile(`'([^']+)':'([^']+)'`).FindAllStringSubmatch(m[1], -1) {
		keys = append(keys, kv[1])
		group[kv[1]] = kv[2]
		if !strings.Contains(webUIHTML, `data-group="`+kv[2]+`"`) {
			t.Errorf("group %q for %s has no filter chip", kv[2], kv[1])
		}
	}
	for _, s := range generationPathSuffixes {
		if group[s] == "" {
			t.Errorf("generation suffix %s has no group in ACT_PATH_GROUP", s)
		}
	}
	for i, a := range keys {
		for _, b := range keys[i+1:] {
			if strings.HasSuffix(b, a) && group[a] != group[b] {
				t.Errorf("%s (%s) comes before %s (%s) and shadows it", a, group[a], b, group[b])
			}
		}
	}
}

// Running requests, here and on peers, show as in-progress rows above the
// history. The finished row shares the running row's node and id key, so the
// same DOM row is repainted in place; a row that drops out without finishing is
// marked lost and fades a minute later.
func TestEmbeddedUIActivityInProgressRows(t *testing.T) {
	for _, want := range []string{
		// Markup: spinner, model, path, node, tokens so far, and a ticking elapsed time.
		"function liveCells", `'<span class="spinner" aria-hidden="true"></span>Running'`, "['status-bad','Lost']",
		"esc(actWhere(e))", "esc(fmtTok(e.tokens))", `'<span data-started="'+t+'">'`,
		"const elapsed=t=>fmtDur(Math.max(0,Date.now()-t))", "querySelectorAll('[data-started]').forEach(n=>{n.textContent=elapsed(n.dataset.started)})",
		".spinner{", "@keyframes spin", "tr.running{cursor:default}", "tr.lost{opacity:.55}",
		"(e.running?liveCells(e):actCells(e))", "tr.classList.toggle('running',!!e.running)", "tr.tabIndex=e.running?-1:0",
		// In-place swap: one key for both rows, the done event drops the running entry and
		// the reconcile reuses the DOM row with that key.
		"const actKey=e=>(e.node?e.node+'/':'')+e.id", "keep=new Set(entries.map(actKey))", "const id=actKey(e);",
		"addEventListener('progress',ev=>actPaint(actRows,JSON.parse(ev.data).in_progress||[]))",
		"actLive.delete(k);\n      actPaint([e,...actRows.filter(r=>actKey(r)!==k)].slice(0,ACT_MAX))",
		"rows=running.concat(rows)",
		// A snapshot or poll (local rows only) keeps the finished peer rows, merged by time.
		"rows.concat(actRows.filter(e=>e.node)).sort((a,b)=>Date.parse(b.timestamp)-Date.parse(a.timestamp))",
		"actLive.clear();\n      actPaint(actWithPeers(d.entries||[]),d.in_progress||[])",
		// A peer restart reuses ids: a running entry newer than the finished row with its key replaces it.
		"Date.parse(e.started)>Date.parse(old.get(actKey(e)).timestamp)", "actRows=actRows.filter(e=>!reused.has(actKey(e)))",
		// A poll has no peer running list, so peer running rows leave quietly instead of being marked lost.
		"actLive.forEach((e,k)=>{if(e.node)actLive.delete(k)});\n      actPaint(actWithPeers(d.entries||[]),d.in_progress||[])",
		// Lost rows.
		"ACT_LOST_MS=60000", "e.lost=true;", "if(actLive.get(k)===e){actLive.delete(k);actPaint(actRows)}",
		// A peer row's dialog shows the summary and links to the peer's UI.
		"if(e.running)return;", "'Summary from '+e.node+'. The capture stays on that peer.'",
		"esc(e.peer_url.replace(/\\/+$/,'')+'/ui/')", "Open the UI on '+esc(e.node)",
	} {
		if !strings.Contains(webUIHTML, want) {
			t.Errorf("embedded UI missing %q", want)
		}
	}
	if strings.Contains(uiJS, "addEventListener('entry'") {
		t.Error("the Activity view still listens for the old entry event")
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

// The model catalog has a Peers cell with one escaped chip per peer, and
// remote-only rows no longer say "Mesh routed".
func TestEmbeddedUIModelCatalogPeers(t *testing.T) {
	for _, want := range []string{
		"<th>State</th><th>Peers</th><th>VRAM</th>", "<td>'+peerChips(m.peers)+'</td>",
		"function peerChips", "esc(bits.join(' · '))", "'Last seen '+new Date(p.last_seen*1000)",
		".state.loaded{", `colspan="6"`,
	} {
		if !strings.Contains(webUIHTML, want) {
			t.Errorf("embedded UI missing %q", want)
		}
	}
	if strings.Contains(webUIHTML, "Mesh routed") {
		t.Error("remote rows still say Mesh routed instead of showing peer chips")
	}
}

// Catalog rows of a known family show its bundled icon, labelled with the
// family and maker; other rows keep the generic model or peer glyph.
func TestEmbeddedUIFamilyIcons(t *testing.T) {
	if strings.Contains(webUIHTML, "__EMBED_FAMILY_ICONS__") {
		t.Fatal("family icon placeholder was not filled")
	}
	var icons map[string]string
	if err := json.Unmarshal([]byte(familyIconsJSON()), &icons); err != nil {
		t.Fatal(err)
	}
	for _, e := range families {
		svg := icons[e.f.ID]
		if !strings.HasPrefix(svg, "<svg ") || !strings.Contains(svg, `aria-hidden="true"`) {
			t.Errorf("family %s has no usable icon: %q", e.f.ID, svg)
		}
	}
	for _, want := range []string{
		"const FAMILY_ICONS={", "\"qwen\":\"\\" + "u003csvg", // json.Marshal escapes the opening bracket
		// #132 makes a metadata icon_url win over the built-in family icon, and the
		// built-in icon stays as the fallback, so assert both halves rather than the
		// pre-#132 single expression. #177 lets a safe icon_url (a hub avatar) draw
		// even on a row with no family.
		"iu=m.info&&m.info.icon_url,fi=iu&&safeUrl(iu)?",
		":f&&Object.hasOwn(FAMILY_ICONS,f.id)&&FAMILY_ICONS[f.id],", `role="img" aria-label="'+esc(fl)+'"`,
		"(fi||icon(origin==='remote'?'peer':'model'))",
		"<td>'+peerChips(m.peers)+'</td>",
	} {
		if !strings.Contains(webUIHTML, want) {
			t.Errorf("embedded UI missing %q", want)
		}
	}
}
