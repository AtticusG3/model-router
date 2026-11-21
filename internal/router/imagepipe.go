package router

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"time"

	"model-router/internal/config"
)

// imageReject is a pipeline step refusing a client request. proxyImage answers
// it with 400 {"error": msg} before the backend sees the request.
type imageReject string

func (e imageReject) Error() string { return string(e) }

// checkImg2img validates an img2img body: sd-server drops a missing or
// unreadable init image and quietly runs txt2img instead. It also rewrites a
// data-URL first init to plain base64 and mirrors it into an empty
// extra_images, so vision-conditioned reference images get the hero image.
// Bodies that are not a JSON object, and other paths, pass through untouched.
func checkImg2img(req *http.Request, _ http.Header, notes *imageNotes) error {
	if req.Method != http.MethodPost || req.URL.Path != "/sdapi/v1/img2img" || req.Body == nil {
		return nil
	}
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		return err
	}
	req.Body = io.NopCloser(bytes.NewReader(raw))
	// RawMessage values keep untouched fields byte-exact.
	var obj map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &obj) != nil || obj == nil {
		return nil
	}

	var inits []json.RawMessage
	if json.Unmarshal(obj["init_images"], &inits) != nil || len(inits) == 0 {
		return imageReject("img2img requires non-empty init_images")
	}
	var first string
	_ = json.Unmarshal(inits[0], &first)
	img := decodeB64Image(first)
	if img == nil {
		return imageReject("init_images[0] is not a decodable PNG/JPEG/WEBP")
	}

	changed := false
	if strings.HasPrefix(first, "data:") {
		inits[0], _ = json.Marshal(base64.StdEncoding.EncodeToString(img))
		obj["init_images"], _ = json.Marshal(inits)
		notes.Add("img2img: data-URL init_images[0] sent as plain base64")
		changed = true
	}
	var extras []json.RawMessage
	if json.Unmarshal(obj["extra_images"], &extras) != nil || len(extras) == 0 {
		obj["extra_images"], _ = json.Marshal([]json.RawMessage{inits[0]})
		notes.Add("img2img: init_images[0] mirrored into extra_images")
		changed = true
	}
	if !changed {
		return nil
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	req.Body = io.NopCloser(bytes.NewReader(out))
	req.ContentLength = int64(len(out))
	return nil
}

// decodeB64Image decodes a base64 image, with or without a data-URL prefix,
// and returns nil unless it starts like a PNG, JPEG or WEBP file.
func decodeB64Image(v string) []byte {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, "data:") {
		if _, after, ok := strings.Cut(v, ","); ok {
			v = after
		}
	}
	raw, err := base64.StdEncoding.DecodeString(v)
	if err != nil || len(raw) < 24 {
		return nil
	}
	switch {
	case bytes.HasPrefix(raw, []byte("\x89PNG\r\n\x1a\n")),
		bytes.HasPrefix(raw, []byte("\xff\xd8\xff")),
		string(raw[:4]) == "RIFF" && string(raw[8:12]) == "WEBP":
		return raw
	}
	return nil
}

// captionSystemPrompt is the sd-server shim's CAPTION_SYSTEM_PROMPT, word for
// word, so both paths ask the enhancer the same question.
const captionSystemPrompt = `You turn an image request into a structured caption as one JSON object.
Return only the JSON object, with no markdown fence and no commentary.

Schema:
{
  "high_level_description": "one sentence describing the whole image",
  "compositional_deconstruction": {
    "background": "what fills the frame behind the subjects",
    "elements": [
      {"type": "obj", "bbox": [x_min, y_min, x_max, y_max], "desc": "detailed neutral description"}
    ]
  },
  "style_description": {
    "aesthetics": "...",
    "lighting": "...",
    "photo": "...",
    "medium": "..."
  }
}

Rules:
- bbox values are integers on a 0-1000 grid measured from the top-left, x_min < x_max and y_min < y_max.
- x runs left to right across the canvas width; y runs top to bottom down the canvas height.
- Place elements so they fit the canvas given in the request.
- Keep the subject, count, actions and intent of the request. Never add subjects that were not asked for.
- Give every element a type, a bbox and a detailed neutral desc.
- Include style_description only when the request asks for a style, medium, camera, lighting or aesthetic. Otherwise omit it.
`

// defaultEnhancerBudget is the caption step's wall-clock budget when the
// stanza sets no enhancer.timeout_seconds.
const defaultEnhancerBudget = 20 * time.Second

// styleWords marks a prompt that asks for a style; only then is the caption's
// style_description kept. Same list as the shim.
var styleWords = regexp.MustCompile(`(?i)\b(photo|photograph|photoreal\w*|realistic|cinematic|anime|illustration|` +
	`painting|watercolor|oil\s+paint\w*|sketch|drawing|render|3d|cgi|lighting|` +
	`camera|lens|film|medium|style|aesthetic|pastel|pixel\s+art|graphic|` +
	`cartoon|comic)\b`)

var codeFence = regexp.MustCompile("(?i)^```(?:json)?\\s*|\\s*```$")

// ideoCaption is an ideogram4 caption. Field order is the spec's key order;
// keys the spec does not name are dropped.
type ideoCaption struct {
	HighLevel   string `json:"high_level_description"`
	Composition struct {
		Background json.RawMessage   `json:"background"`
		Elements   []json.RawMessage `json:"elements"`
	} `json:"compositional_deconstruction"`
	Style json.RawMessage `json:"style_description,omitempty"`
}

// ideoElement is one caption element in the spec's key order. text is only
// kept on type text elements.
type ideoElement struct {
	Type         *string         `json:"type"`
	BBox         []float64       `json:"bbox"`
	Text         json.RawMessage `json:"text,omitempty"`
	Desc         *string         `json:"desc"`
	ColorPalette json.RawMessage `json:"color_palette,omitempty"`
}

// valid is the shim's _valid_element: a type, a non-blank desc and an x-first
// bbox inside the 0-1000 grid with min < max.
func (e *ideoElement) valid() bool {
	b := e.BBox
	return e.Type != nil && e.Desc != nil && strings.TrimSpace(*e.Desc) != "" && len(b) == 4 &&
		0 <= b[0] && b[0] < b[2] && b[2] <= 1000 && 0 <= b[1] && b[1] < b[3] && b[3] <= 1000
}

// captionStep returns the Rewrite for caption: ideogram4. A plain txt2img
// prompt becomes an ideogram4 caption from the enhancer model, or the template
// caption holding the prompt verbatim when the enhancer fails for any reason.
// enhance_prompt: false, an empty prompt, a prompt that is already a JSON
// object, and other paths pass through untouched.
func (r *Router) captionStep(p *config.ImagePipeline) func(*http.Request, http.Header, *imageNotes) error {
	return func(req *http.Request, h http.Header, notes *imageNotes) error {
		if req.Method != http.MethodPost || req.URL.Path != "/sdapi/v1/txt2img" || req.Body == nil {
			return nil
		}
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			return err
		}
		req.Body = io.NopCloser(bytes.NewReader(raw))
		var obj map[string]json.RawMessage
		var prompt string
		if json.Unmarshal(raw, &obj) != nil || obj == nil || string(obj["enhance_prompt"]) == "false" ||
			json.Unmarshal(obj["prompt"], &prompt) != nil || strings.TrimSpace(prompt) == "" || isJSONObject(prompt) {
			return nil
		}

		source := "enhancer"
		enhanced, err := r.askEnhancer(req.Context(), p, prompt, captionDimension(obj["width"]), captionDimension(obj["height"]))
		if err != nil {
			r.logger.Warnf("image caption: enhancer skipped (%v); using template", err)
			enhanced, source = templateCaption(prompt), "template"
		}
		obj["prompt"], _ = json.Marshal(enhanced)
		out, err := json.Marshal(obj)
		if err != nil {
			return err
		}
		req.Body = io.NopCloser(bytes.NewReader(out))
		req.ContentLength = int64(len(out))
		// The sd-server shim's header names, so existing clients still match.
		h.Set("X-SD-Shim-Prompt-Original", shimQuote(prompt))
		h.Set("X-SD-Shim-Prompt-Enhanced", shimQuote(enhanced))
		h.Set("X-SD-Shim-Prompt-Source", source)
		notes.Add("prompt: " + source + " caption " + enhanced)
		return nil
	}
}

// enhancerContext is the in-process chat request's context: a fresh one, so
// the image request's values (Activity capture, pipeline notes) do not leak
// into it, ending with the budget or the client and marked no-load so a
// stopped enhancer is never started.
func enhancerContext(parent context.Context, budget time.Duration) (context.Context, func()) {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	stop := context.AfterFunc(parent, cancel)
	return context.WithValue(ctx, noLoadKey{}, true), func() {
		stop()
		cancel()
	}
}

// askEnhancer asks the enhancer chat model for a caption of prompt on a width
// by height canvas and returns the checked caption. The model must already be
// running here or offered by a fresh peer: a cold load could evict the image
// model. The call goes through ServeHTTP, so it queues and spills like any
// chat request, and the whole step shares one budget; when it runs out the
// chat request is cancelled so it frees the enhancer's slot.
func (r *Router) askEnhancer(parent context.Context, p *config.ImagePipeline, prompt string, width, height int) (string, error) {
	want, budget := "", defaultEnhancerBudget
	if e := p.Enhancer; e != nil {
		want = e.Model
		if e.TimeoutSeconds > 0 {
			budget = time.Duration(e.TimeoutSeconds) * time.Second
		}
	}
	// A fresh context: the image request's values (Activity capture, pipeline
	// notes) must not leak into the chat request. It still ends with the client.
	ctx, finish := enhancerContext(parent, budget)
	defer finish()

	model := r.runningEnhancer(want)
	if model == "" {
		return "", errors.New("enhancer model not running")
	}
	payload, err := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": captionSystemPrompt},
			{"role": "user", "content": fmt.Sprintf("Canvas: %d pixels wide (x) by %d pixels tall (y).\n\nRequest: %s", width, height, prompt)},
		},
		"temperature": 0,
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	// A goroutine holds the budget even on a path that ignores ctx; recover
	// keeps a fault in the chat path from taking the process down with it.
	done := make(chan any, 1)
	go func() {
		defer func() { done <- recover() }()
		r.ServeHTTP(rec, req)
	}()
	select {
	case <-ctx.Done():
		return "", fmt.Errorf("enhancer %s: %w", model, ctx.Err())
	case fault := <-done:
		if fault != nil {
			return "", fmt.Errorf("enhancer %s: %v", model, fault)
		}
	}
	if rec.Code != http.StatusOK {
		return "", fmt.Errorf("enhancer %s answered %d", model, rec.Code)
	}
	var reply struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil || len(reply.Choices) == 0 {
		return "", fmt.Errorf("enhancer %s reply has no choices", model)
	}
	return checkedCaption(prompt, reply.Choices[0].Message.Content)
}

// runningEnhancer returns want when the catalogue lists it as running locally
// or loaded on a fresh router peer. With no want it is the first such chat
// model that is not an embedding or rerank model. "" means none.
func (r *Router) runningEnhancer(want string) string {
	for _, ms := range r.catalogStatuses() {
		switch ms.Origin {
		case "local":
			if ms.State != string(StateRunning) {
				continue
			}
		case "remote":
			// A peer row only means the peer lists the model, not that it is
			// loaded: the chat call would then ask the peer to load it and could
			// evict the peer's own image model.
			// shortcut: a peer evicting after this check can still cold-load, upgrade when peers accept a serve-only request
			if !r.peerModelLoaded(ms.ID) {
				continue
			}
		default:
			continue
		}
		if want != "" {
			if ms.ID == want {
				return want
			}
			continue
		}
		id := strings.ToLower(ms.ID)
		if (ms.APIType == "" || ms.APIType == config.APITypeChat) && !strings.Contains(id, "embed") && !strings.Contains(id, "rerank") {
			return ms.ID
		}
	}
	return ""
}

// peerModelLoaded reports whether any fresh router peer's telemetry lists id
// as loaded. kind: openai peers have no telemetry and stay out, so a paid
// upstream is never chosen to receive the prompt.
func (r *Router) peerModelLoaded(id string) bool {
	cfg := r.cfg()
	for i := range cfg.Peers {
		p := &cfg.Peers[i]
		if p.Kind == "openai" {
			continue
		}
		if r.peerLoads(p.Name, id) {
			return true
		}
	}
	return false
}

// peerLoads reports whether a fresh router peer's telemetry lists id as
// loaded.
func (r *Router) peerLoads(name, id string) bool {
	if !r.peers.Fresh(name) {
		return false
	}
	t := r.peers.Snapshot()[name]
	if t == nil {
		return false
	}
	for _, lm := range t.LoadedModels {
		if lm.ID == id || lm.Instance == id {
			return true
		}
	}
	return false
}

// checkedCaption parses the enhancer's reply as a caption and applies the
// shim's rules: at least one valid element, bboxes turned y-first, element
// keys in spec order, style_description only when the prompt asks for a style.
func checkedCaption(prompt, content string) (string, error) {
	var c ideoCaption
	if err := json.Unmarshal([]byte(codeFence.ReplaceAllString(strings.TrimSpace(content), "")), &c); err != nil {
		return "", fmt.Errorf("enhancer reply is not a caption: %w", err)
	}
	var elements []json.RawMessage
	for _, raw := range c.Composition.Elements {
		var e ideoElement
		if json.Unmarshal(raw, &e) != nil || !e.valid() {
			continue
		}
		// The model answers x-first; ideogram4 reads bboxes y-first.
		e.BBox = []float64{e.BBox[1], e.BBox[0], e.BBox[3], e.BBox[2]}
		if *e.Type != "text" {
			e.Text = nil
		}
		out, err := json.Marshal(e)
		if err != nil {
			return "", err
		}
		elements = append(elements, out)
	}
	if strings.TrimSpace(c.HighLevel) == "" || !nonEmptyBackground(c.Composition.Background) || len(elements) == 0 {
		return "", errors.New("enhancer caption is invalid")
	}
	c.Composition.Elements = elements
	if !styleWords.MatchString(prompt) {
		c.Style = nil
	}
	out, err := json.Marshal(c)
	return string(out), err
}

// templateCaption is the fallback caption: the prompt verbatim as the
// description and as one centred element.
func templateCaption(prompt string) string {
	kind, desc := "obj", prompt
	el, _ := json.Marshal(ideoElement{Type: &kind, BBox: []float64{200, 200, 800, 800}, Desc: &desc})
	var c ideoCaption
	c.HighLevel = prompt
	c.Composition.Background = json.RawMessage(`"a plain, evenly lit background"`)
	c.Composition.Elements = []json.RawMessage{el}
	out, _ := json.Marshal(c)
	return string(out)
}

// nonEmptyBackground reports whether raw is a non-empty string or object.
func nonEmptyBackground(raw json.RawMessage) bool {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s != ""
	}
	var m map[string]json.RawMessage
	return json.Unmarshal(raw, &m) == nil && len(m) > 0
}

// isJSONObject reports whether s parses as a JSON object.
func isJSONObject(s string) bool {
	var m map[string]json.RawMessage
	return json.Unmarshal([]byte(s), &m) == nil && m != nil
}

// captionDimension reads a body width or height, 1024 when absent or not a
// positive number.
func captionDimension(raw json.RawMessage) int {
	var f float64
	if json.Unmarshal(raw, &f) != nil || f < 1 {
		return 1024
	}
	return int(f)
}

// shimQuote percent-encodes s like Python's quote(s, safe=""), as the shim
// does for its prompt headers.
func shimQuote(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}
