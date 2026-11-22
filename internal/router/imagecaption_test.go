package router

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"model-router/internal/config"
)

// testCaption is the enhancer's x-first reply from the shim tests.
const testCaption = `{"high_level_description":"a red sports car parked at a meet","compositional_deconstruction":{"background":"a row of parked cars under a clear sky","elements":[{"type":"obj","bbox":[120,240,880,820],"desc":"a red coupe"}]}}`

// fakeEnhancer is a chat backend that answers caption requests. Its fields set
// how it fails; it records each chat body it reads.
type fakeEnhancer struct {
	content string        // message content of a 200 reply
	reply   string        // raw reply body, overriding content
	status  int           // reply status, 200 when 0
	delay   time.Duration // wait before the headers
	trickle time.Duration // spread the body over this long
	drop    bool          // close the connection without a reply

	mu        sync.Mutex
	requests  []map[string]any
	cancelled chan struct{} // signalled when a delayed request is cancelled
}

func (f *fakeEnhancer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == "/health" {
		return
	}
	var body map[string]any
	_ = json.NewDecoder(req.Body).Decode(&body)
	f.mu.Lock()
	f.requests = append(f.requests, body)
	f.mu.Unlock()
	if f.drop {
		if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
			_ = conn.Close()
		}
		return
	}
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-req.Context().Done():
			select {
			case f.cancelled <- struct{}{}:
			default:
			}
			return
		}
	}
	out := f.reply
	if out == "" {
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": f.content}}}})
		out = string(b)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	if f.status != 0 {
		w.WriteHeader(f.status)
	}
	if f.trickle == 0 {
		_, _ = io.WriteString(w, out)
		return
	}
	for i := range len(out) {
		_, _ = w.Write([]byte{out[i]})
		_ = http.NewResponseController(w).Flush()
		select {
		case <-time.After(f.trickle / time.Duration(len(out))):
		case <-req.Context().Done():
			return
		}
	}
}

func (f *fakeEnhancer) chats() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.requests...)
}

// captionRig is an ideogram4 image stanza next to a chat stanza, enhancer-a,
// that fe serves. It records the prompt each image request reaches sd-server
// with.
type captionRig struct {
	r       *Router
	mu      sync.Mutex
	prompts []string
}

// newCaptionRig builds the rig. enhancer is the stanza's enhancer map, "" for
// the default model; load starts enhancer-a first.
func newCaptionRig(t *testing.T, fe *fakeEnhancer, enhancer string, load bool, peers ...string) *captionRig {
	t.Helper()
	return newCaptionRigFor(t, "ideogram4", fe, enhancer, load, peers...)
}

// newCaptionRigFor is newCaptionRig with the image stanza's caption set to
// caption.
func newCaptionRigFor(t *testing.T, caption string, fe *fakeEnhancer, enhancer string, load bool, peers ...string) *captionRig {
	t.Helper()
	c := &captionRig{}
	reply := func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Prompt string `json:"prompt"`
		}
		_ = json.NewDecoder(req.Body).Decode(&in)
		c.mu.Lock()
		c.prompts = append(c.prompts, in.Prompt)
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"images":["ok"]}`)
	}
	mux := imageBackend(t, reply).(*http.ServeMux)
	mux.HandleFunc("/sdapi/v1/img2img", reply)
	img := httptest.NewServer(mux)
	t.Cleanup(img.Close)
	if fe.cancelled == nil {
		fe.cancelled = make(chan struct{}, 1)
	}
	chat := httptest.NewServer(fe)
	t.Cleanup(chat.Close)
	if enhancer != "" {
		enhancer = "\n      enhancer: " + enhancer
	}
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
start_port: 5900
stanzas:
  - model_id: sd-ideo
    command: "sd-server --listen-port {port}"
    vram_mb: 0
    api_type: image
    health_check: /v1/models
    image_pipeline:
      caption: %s%s
    match: {path_prefix: /sdapi/v1, path_default: true}
    spin_up_seconds: 5
    port: %d
  - model_id: enhancer-a
    command: "llama-server --port {port}"
    vram_mb: 0
    spin_up_seconds: 5
    port: %d
%s
`, caption, enhancer, img.Listener.Addr().(*net.TCPAddr).Port, chat.Listener.Addr().(*net.TCPAddr).Port, strings.Join(peers, "\n"))))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	c.r = New(cfg, NewLogger(io.Discard, false), "node-a")
	c.r.spawn = func(string, []string) (Proc, error) { return newFakeProc(), nil }
	c.r.ledger.SetGPUs([]*GPUState{gpu(0, 10000)})
	t.Cleanup(func() {
		_ = c.r.Unload("sd-ideo")
		_ = c.r.Unload("enhancer-a")
	})
	// Start the image model up front, so timings cover the caption step only.
	if _, err := c.r.Load("sd-ideo"); err != nil {
		t.Fatalf("load image model: %v", err)
	}
	if load {
		if _, err := c.r.Load("enhancer-a"); err != nil {
			t.Fatalf("load enhancer: %v", err)
		}
	}
	return c
}

// send posts body to path, as a peer when peer is set, and wants a 200.
func (c *captionRig) send(t *testing.T, path, body, peer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if peer != "" {
		req.Header.Set("X-Model-Router-Peer", peer)
	}
	rec := httptest.NewRecorder()
	NewHandler(c.r, NewLogger(io.Discard, false)).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	return rec
}

// sent is the one prompt sd-server received.
func (c *captionRig) sent(t *testing.T) string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.prompts) != 1 {
		t.Fatalf("sd-server prompts = %q, want one", c.prompts)
	}
	return c.prompts[0]
}

func modelState(r *Router, id string) string {
	for _, ms := range r.catalogStatuses() {
		if ms.ID == id {
			return ms.State
		}
	}
	return ""
}

const txt2imgPath = "/sdapi/v1/txt2img"

func TestCaptionPlainPromptBecomesEnhancerCaption(t *testing.T) {
	want := `{"high_level_description":"a red sports car parked at a meet","compositional_deconstruction":{"background":"a row of parked cars under a clear sky","elements":[{"type":"obj","bbox":[240,120,820,880],"desc":"a red coupe"}]}}`
	for name, enhancer := range map[string]string{
		"named model":   "{model: enhancer-a, timeout_seconds: 5}",
		"default model": "",
	} {
		t.Run(name, func(t *testing.T) {
			fe := &fakeEnhancer{content: testCaption}
			c := newCaptionRig(t, fe, enhancer, true)
			rec := c.send(t, txt2imgPath, `{"prompt":"a red car","width":768,"height":512,"seed":7}`, "")

			if got := c.sent(t); got != want {
				t.Fatalf("sd-server prompt = %s\nwant %s", got, want)
			}
			h := rec.Header()
			if h.Get("X-SD-Shim-Prompt-Source") != "enhancer" || h.Get("X-SD-Shim-Prompt-Original") != "a%20red%20car" {
				t.Fatalf("headers = %v", h)
			}
			if got, _ := url.PathUnescape(h.Get("X-SD-Shim-Prompt-Enhanced")); got != want {
				t.Fatalf("enhanced header = %s", got)
			}
			chats := fe.chats()
			if len(chats) != 1 || chats[0]["model"] != "enhancer-a" {
				t.Fatalf("enhancer requests = %v", chats)
			}
			msgs, _ := chats[0]["messages"].([]any)
			sys, _ := msgs[0].(map[string]any)
			user, _ := msgs[1].(map[string]any)
			if sys["content"] != captionSystemPrompt {
				t.Fatalf("system prompt = %v", sys["content"])
			}
			if s, _ := user["content"].(string); !strings.Contains(s, "768 pixels wide (x) by 512 pixels tall (y)") || !strings.HasSuffix(s, "Request: a red car") {
				t.Fatalf("user message = %q", s)
			}

			var noted bool
			for _, e := range c.r.Activity().List() {
				if e.Path != txt2imgPath {
					continue
				}
				capt, _ := c.r.Activity().Capture(e.ID)
				noted = len(capt.Notes) == 1 && strings.Contains(capt.Notes[0], want)
			}
			if !noted {
				t.Fatal("Activity capture does not show the enhanced prompt")
			}
		})
	}
}

func TestCaptionLeavesRequestsUntouched(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 16)))
	caption, _ := json.Marshal(testCaption)
	cases := []struct {
		name, path, body, prompt string
	}{
		{"enhance_prompt false", txt2imgPath, `{"prompt":"a red car","enhance_prompt":false}`, "a red car"},
		{"JSON prompt", txt2imgPath, `{"prompt":` + string(caption) + `}`, testCaption},
		{"empty prompt", txt2imgPath, `{"prompt":"  "}`, "  "},
		{"img2img", "/sdapi/v1/img2img", `{"prompt":"a red car","init_images":["` + png + `"]}`, "a red car"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fe := &fakeEnhancer{content: testCaption}
			c := newCaptionRig(t, fe, "{model: enhancer-a, timeout_seconds: 5}", true)
			rec := c.send(t, tc.path, tc.body, "")
			if got := c.sent(t); got != tc.prompt {
				t.Fatalf("sd-server prompt = %q, want %q", got, tc.prompt)
			}
			if len(fe.chats()) != 0 || rec.Header().Get("X-SD-Shim-Prompt-Source") != "" {
				t.Fatalf("enhancer asked %d times, source %q", len(fe.chats()), rec.Header().Get("X-SD-Shim-Prompt-Source"))
			}
		})
	}
}

// The failure table from the shim tests. The catalogue and URL cases there
// have no in-process counterpart: the router reads its own model state.
func TestCaptionEveryEnhancerFailureSendsTemplate(t *testing.T) {
	const budget = time.Second
	cases := []struct {
		name   string
		fe     *fakeEnhancer // the failure mode; nil for a healthy enhancer
		model  string
		noLoad bool
	}{
		{name: "model not running", noLoad: true},
		{name: "model missing from catalogue", model: "other-model"},
		{name: "dropped connection", fe: &fakeEnhancer{drop: true}},
		{name: "timeout before headers", fe: &fakeEnhancer{delay: 3 * budget}},
		{name: "trickled body", fe: &fakeEnhancer{trickle: 3 * budget}},
		{name: "http 500", fe: &fakeEnhancer{status: http.StatusInternalServerError}},
		{name: "non-json content", fe: &fakeEnhancer{content: "this is not json"}},
		{name: "json without choices", fe: &fakeEnhancer{reply: `{"id":"x"}`}},
		{name: "invalid caption", fe: &fakeEnhancer{content: `{"high_level_description":"x"}`}},
	}
	prompts := []string{"a red car", "a \"quoted\" sign\\path\nsecond line 🚗 赤い車"}
	for _, tc := range cases {
		for _, prompt := range prompts {
			t.Run(tc.name, func(t *testing.T) {
				mode := tc.fe
				if mode == nil {
					mode = &fakeEnhancer{}
				}
				// A fresh backend per run, so request counts are per prompt.
				fe := &fakeEnhancer{content: testCaption, reply: mode.reply, status: mode.status,
					delay: mode.delay, trickle: mode.trickle, drop: mode.drop}
				if mode.content != "" {
					fe.content = mode.content
				}
				model := tc.model
				if model == "" {
					model = "enhancer-a"
				}
				c := newCaptionRig(t, fe, "{model: "+model+", timeout_seconds: 1}", !tc.noLoad)
				body, _ := json.Marshal(map[string]string{"prompt": prompt})

				started := time.Now()
				rec := c.send(t, txt2imgPath, string(body), "")
				if elapsed := time.Since(started); elapsed > budget+time.Second {
					t.Fatalf("took %v, want under budget + 1s", elapsed)
				}
				if got := c.sent(t); got != templateCaption(prompt) {
					t.Fatalf("sd-server prompt = %s, want the template", got)
				}
				var capt struct {
					HighLevel   string `json:"high_level_description"`
					Composition struct {
						Elements []struct {
							BBox []int  `json:"bbox"`
							Desc string `json:"desc"`
						} `json:"elements"`
					} `json:"compositional_deconstruction"`
				}
				if err := json.Unmarshal([]byte(c.sent(t)), &capt); err != nil {
					t.Fatalf("template is not JSON: %v", err)
				}
				els := capt.Composition.Elements
				if capt.HighLevel != prompt || len(els) != 1 || els[0].Desc != prompt || fmt.Sprint(els[0].BBox) != "[200 200 800 800]" {
					t.Fatalf("template = %+v, want the prompt verbatim", capt)
				}
				if _, err := checkedCaption(prompt, c.sent(t)); err != nil {
					t.Fatalf("template fails the caption checks: %v", err)
				}
				h := rec.Header()
				if got, _ := url.PathUnescape(h.Get("X-SD-Shim-Prompt-Original")); got != prompt || h.Get("X-SD-Shim-Prompt-Source") != "template" {
					t.Fatalf("headers = %v", h)
				}
				if tc.noLoad || tc.model != "" {
					if len(fe.chats()) != 0 {
						t.Fatalf("enhancer asked %d times", len(fe.chats()))
					}
				}
				if tc.noLoad && modelState(c.r, "enhancer-a") == string(StateRunning) {
					t.Fatal("the caption step started a stopped enhancer")
				}
			})
		}
	}
}

func TestCaptionSlowEnhancerIsCancelledAtBudget(t *testing.T) {
	fe := &fakeEnhancer{content: testCaption, delay: time.Minute}
	c := newCaptionRig(t, fe, "{model: enhancer-a, timeout_seconds: 1}", true)
	started := time.Now()
	rec := c.send(t, txt2imgPath, `{"prompt":"a red car"}`, "")
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("image request took %v, want under budget + 1s", elapsed)
	}
	if rec.Header().Get("X-SD-Shim-Prompt-Source") != "template" || c.sent(t) != templateCaption("a red car") {
		t.Fatalf("source %q, prompt %s", rec.Header().Get("X-SD-Shim-Prompt-Source"), c.sent(t))
	}
	select {
	case <-fe.cancelled:
	case <-time.After(time.Second):
		t.Fatal("the enhancer's chat request was not cancelled")
	}
}

func TestCaptionPeerRequestIsEnhancedHere(t *testing.T) {
	fe := &fakeEnhancer{content: testCaption}
	c := newCaptionRig(t, fe, "{model: enhancer-a, timeout_seconds: 5}", true)
	rec := c.send(t, txt2imgPath, `{"prompt":"a red car"}`, "node-b")
	if rec.Header().Get("X-SD-Shim-Prompt-Source") != "enhancer" || len(fe.chats()) != 1 {
		t.Fatalf("source %q after %d enhancer requests", rec.Header().Get("X-SD-Shim-Prompt-Source"), len(fe.chats()))
	}
	if !strings.Contains(c.sent(t), `"bbox":[240,120,820,880]`) {
		t.Fatalf("sd-server prompt = %s", c.sent(t))
	}
}

func TestCaptionEnhancerThatStopsAfterTheCheckIsNotStarted(t *testing.T) {
	fe := &fakeEnhancer{content: testCaption}
	// A router peer that lists the enhancer without having it loaded, and a
	// paid upstream offering the same id: a spill must reach neither.
	alpha := newWalkPeer(t, http.StatusOK, "")
	paid := newWalkPeer(t, http.StatusOK, "")
	c := newCaptionRig(t, fe, "", true, `peers:
  - name: alpha
    kind: router
    base_url: http://127.0.0.1:9
    models: [enhancer-a]
  - name: paid
    kind: openai
    base_url: http://127.0.0.1:9
    models: [enhancer-a]`)
	c.r.cfg().Peer("alpha").BaseURL = alpha.srv.URL
	c.r.cfg().Peer("paid").BaseURL = paid.srv.URL
	c.r.peers.Set("alpha", &Telemetry{Node: "alpha"})
	if got := c.r.runningEnhancer("enhancer-a"); got != "enhancer-a" {
		t.Fatalf("enhancer check = %q, want enhancer-a", got)
	}
	// The model stops, as an eviction or unload leaves it, before the chat
	// request reaches the serving path.
	if err := c.r.Unload("enhancer-a"); err != nil {
		t.Fatalf("unload: %v", err)
	}
	ctx, done := enhancerContext(context.Background(), time.Second)
	defer done()
	if ctx.Value(noLoadKey{}) != true {
		t.Fatal("enhancer context is not marked no-load")
	}
	chat := func(ctx context.Context) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"enhancer-a","messages":[{"role":"user","content":"x"}]}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		c.r.ServeHTTP(rec, req)
		return rec
	}
	if rec := chat(ctx); rec.Code == http.StatusOK || modelState(c.r, "enhancer-a") == string(StateRunning) {
		t.Fatalf("stopped enhancer served: code %d, state %q", rec.Code, modelState(c.r, "enhancer-a"))
	}
	// The local failure spilled: the walk ran and left both peers out.
	if logs := strings.Join(c.r.logger.RecentMesh(0), "\n"); !strings.Contains(logs, "peer walk enhancer-a: no peer chosen") {
		t.Fatalf("mesh log = %q, want the spill to skip both peers", logs)
	}
	if alpha.loads.Load() != 0 || alpha.chats.Load() != 0 || paid.loads.Load() != 0 || paid.chats.Load() != 0 {
		t.Fatalf("no-load request reached peers: alpha loads %d chats %d, paid loads %d chats %d",
			alpha.loads.Load(), alpha.chats.Load(), paid.loads.Load(), paid.chats.Load())
	}
	// The same request without the flag loads it, so the flag is what held it.
	if rec := chat(context.Background()); rec.Code != http.StatusOK {
		t.Fatalf("plain chat request = %d, want 200", rec.Code)
	}
}

func TestCaptionBBoxesYFirstInSpecKeyOrder(t *testing.T) {
	reply := `{"high_level_description":"a car parked under string lights","compositional_deconstruction":{"background":"a night car park","elements":[` +
		`{"desc":"asphalt ground","bbox":[0,760,1000,1000],"type":"obj","extra":1,"text":"dropped"},` +
		`{"type":"obj","bbox":[100,120,900,220],"desc":"string lights","color_palette":["#FFD27A"]},` +
		`{"desc":"a sign","text":"OPEN","bbox":[700,300,900,400],"type":"text"},` +
		`{"type":"obj","bbox":[500,0,400,100],"desc":"broken box"},` +
		`{"type":"obj","bbox":[0,0,true,100],"desc":"bool bbox"},` +
		`{"type":"obj","bbox":[0,0,100,100],"desc":"  "}]}}`
	got, err := checkedCaption("a car at night", "```json\n"+reply+"\n```")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"high_level_description":"a car parked under string lights","compositional_deconstruction":{"background":"a night car park","elements":[` +
		`{"type":"obj","bbox":[760,0,1000,1000],"desc":"asphalt ground"},` +
		`{"type":"obj","bbox":[120,100,220,900],"desc":"string lights","color_palette":["#FFD27A"]},` +
		`{"type":"text","bbox":[300,700,400,900],"text":"OPEN","desc":"a sign"}]}}`
	if got != want {
		t.Fatalf("caption = %s\nwant %s", got, want)
	}
}

func TestCaptionStyleKeptOnlyWhenAsked(t *testing.T) {
	styled := strings.TrimSuffix(testCaption, "}") + `,"style_description":{"medium":"photo"}}`
	plain, err := checkedCaption("a red car", styled)
	if err != nil || strings.Contains(plain, "style_description") {
		t.Fatalf("plain prompt caption = %s, %v", plain, err)
	}
	asked, err := checkedCaption("a cinematic photo of a red car", styled)
	if err != nil || !strings.HasSuffix(asked, `,"style_description":{"medium":"photo"}}`) {
		t.Fatalf("styled prompt caption = %s, %v", asked, err)
	}
}

func TestCaptionInvalidReplies(t *testing.T) {
	for _, reply := range []string{
		`not json`,
		`[1,2]`,
		`{"high_level_description":"  ","compositional_deconstruction":{"background":"x","elements":[{"type":"obj","bbox":[0,0,10,10],"desc":"d"}]}}`,
		`{"high_level_description":"x","compositional_deconstruction":{"background":"","elements":[{"type":"obj","bbox":[0,0,10,10],"desc":"d"}]}}`,
		`{"high_level_description":"x","compositional_deconstruction":{"background":{},"elements":[{"type":"obj","bbox":[0,0,10,10],"desc":"d"}]}}`,
		`{"high_level_description":"x","compositional_deconstruction":{"background":"x","elements":[{"bbox":[0,0,10,10],"desc":"d"}]}}`,
		`{"high_level_description":"x","compositional_deconstruction":{"background":"x","elements":"none"}}`,
	} {
		if got, err := checkedCaption("a red car", reply); err == nil {
			t.Errorf("checkedCaption(%s) = %s, want an error", reply, got)
		}
	}
}

func TestRunningEnhancerUsesOnlyRunningOrLoadedModels(t *testing.T) {
	cfg, err := config.Parse([]byte(`
start_port: 5900
stanzas:
  - model_id: local-chat
    command: "llama-server --port {port}"
    vram_mb: 0
peers:
  - name: alpha
    kind: router
    base_url: http://127.0.0.1:1
    models: [peer-embed, peer-chat]
  - name: paid
    kind: openai
    base_url: http://127.0.0.1:2
    models: [paid-chat]
`))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	r := New(cfg, NewLogger(io.Discard, false), "node-a")
	if got := r.runningEnhancer("local-chat"); got != "" {
		t.Fatalf("stopped local model picked: %q", got)
	}
	if got := r.runningEnhancer("peer-chat"); got != "" {
		t.Fatalf("model of a peer with no fresh telemetry picked: %q", got)
	}
	// The peer lists the model, but its telemetry says it is not loaded.
	r.peers.Set("alpha", &Telemetry{Node: "alpha"})
	if got := r.runningEnhancer("peer-chat"); got != "" {
		t.Fatalf("listed but unloaded peer model picked: %q", got)
	}
	if got := r.runningEnhancer(""); got != "" {
		t.Fatalf("default enhancer = %q, want none", got)
	}
	// Another node has only an embedding model loaded, and the paid upstream
	// has no loaded-model telemetry to trust.
	r.peers.Set("alpha", &Telemetry{Node: "alpha", LoadedModels: []LoadedModel{{ID: "peer-embed"}}})
	r.peers.Set("paid", &Telemetry{Node: "paid", LoadedModels: []LoadedModel{{ID: "paid-chat"}}})
	if got := r.runningEnhancer(""); got != "" {
		t.Fatalf("default enhancer = %q, want none", got)
	}
	r.peers.Set("alpha", &Telemetry{Node: "alpha", LoadedModels: []LoadedModel{{ID: "peer-chat"}}})
	if got := r.runningEnhancer("peer-chat"); got != "peer-chat" {
		t.Fatalf("loaded peer model = %q, want peer-chat", got)
	}
	if got := r.runningEnhancer(""); got != "peer-chat" {
		t.Fatalf("default enhancer = %q, want the first loaded non-embedding chat model", got)
	}
}

func TestShimQuoteMatchesPython(t *testing.T) {
	// Python: urllib.parse.quote('a b+c/é~_.-"', safe="")
	if got := shimQuote(`a b+c/é~_.-"`); got != "a%20b%2Bc%2F%C3%A9~_.-%22" {
		t.Fatalf("shimQuote = %s", got)
	}
}
