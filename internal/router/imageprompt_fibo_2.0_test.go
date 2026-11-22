package router

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// fibo2Reply is an enhancer reply for the prompt in
// TestFibo2PlainPromptBecomesCaption, the golden example in the research note:
// indented, with the two score keys the training captions dropped.
const fibo2Reply = `{
  "short_description": "A small bakery storefront at dawn with a painted sign above its front window.",
  "objects": [
    {"description": "a small bakery storefront with a wide front window full of loaves and pastries", "location": "center", "relative_size": "large within frame", "shape_and_color": "rectangular shopfront painted dark green with cream trim", "texture": "weathered painted wood and clean glass", "appearance_details": "warm yellow light glows from inside", "relationship": "the sign hangs above the window", "orientation": "facing the viewer"}
  ],
  "background_setting": "a quiet cobbled street at dawn with terrace houses on either side",
  "lighting": {"conditions": "early morning light mixed with warm light from the shop", "direction": "from inside the shop and from the left", "shadows": "long soft shadows across the cobbles"},
  "aesthetics": {"composition": "centred, eye level, symmetrical", "color_scheme": "dark green, cream and warm yellow against pale blue", "mood_atmosphere": "calm and welcoming", "aesthetic_score": "very high", "preference_score": "very high"},
  "photographic_characteristics": {"depth_of_field": "deep", "focus": "sharp on the storefront and sign", "camera_angle": "eye level", "lens_focal_length": "standard"},
  "style_medium": "photography",
  "text_render": [{"text": "FRESH BREAD", "location": "top center, above the window", "size": "large", "color": "cream", "font": "hand-painted serif", "appearance_details": "painted on a dark green board"}],
  "context": "a shopfront photograph for a local bakery",
  "artistic_style": "warm documentary photography"
}`

// fibo2Golden is fibo2Reply as sd-server gets it: compact, scores dropped.
const fibo2Golden = `{"short_description":"A small bakery storefront at dawn with a painted sign above its front window.","objects":[{"description":"a small bakery storefront with a wide front window full of loaves and pastries","location":"center","relative_size":"large within frame","shape_and_color":"rectangular shopfront painted dark green with cream trim","texture":"weathered painted wood and clean glass","appearance_details":"warm yellow light glows from inside","relationship":"the sign hangs above the window","orientation":"facing the viewer"}],"background_setting":"a quiet cobbled street at dawn with terrace houses on either side","lighting":{"conditions":"early morning light mixed with warm light from the shop","direction":"from inside the shop and from the left","shadows":"long soft shadows across the cobbles"},"aesthetics":{"composition":"centred, eye level, symmetrical","color_scheme":"dark green, cream and warm yellow against pale blue","mood_atmosphere":"calm and welcoming"},"photographic_characteristics":{"depth_of_field":"deep","focus":"sharp on the storefront and sign","camera_angle":"eye level","lens_focal_length":"standard"},"style_medium":"photography","text_render":[{"text":"FRESH BREAD","location":"top center, above the window","size":"large","color":"cream","font":"hand-painted serif","appearance_details":"painted on a dark green board"}],"context":"a shopfront photograph for a local bakery","artistic_style":"warm documentary photography"}`

func TestFibo2PlainPromptBecomesCaption(t *testing.T) {
	fe := &fakeEnhancer{content: fibo2Reply}
	c := newCaptionRigFor(t, "fibo-2.0", fe, "{model: enhancer-a, timeout_seconds: 5}", true)
	const prompt = `a bakery at dawn with a sign that says "FRESH BREAD"`
	rec := c.send(t, txt2imgPath, `{"prompt":"a bakery at dawn with a sign that says \"FRESH BREAD\"","width":1344,"height":768}`, "")

	if got := c.sent(t); got != fibo2Golden {
		t.Fatalf("sd-server prompt = %s\nwant %s", got, fibo2Golden)
	}
	h := rec.Header()
	if got, _ := url.PathUnescape(h.Get("X-SD-Shim-Prompt-Original")); got != prompt || h.Get("X-SD-Shim-Prompt-Source") != "enhancer" {
		t.Fatalf("headers = %v", h)
	}
	chats := fe.chats()
	if len(chats) != 1 {
		t.Fatalf("enhancer asked %d times", len(chats))
	}
	msgs, _ := chats[0]["messages"].([]any)
	sys, _ := msgs[0].(map[string]any)
	user, _ := msgs[1].(map[string]any)
	if sys["content"] != fibo2SystemPrompt {
		t.Fatalf("system prompt = %v", sys["content"])
	}
	if s, _ := user["content"].(string); !strings.Contains(s, "1344 pixels wide (x) by 768 pixels tall (y)") || !strings.HasSuffix(s, "Request: "+prompt) {
		t.Fatalf("user message = %q", s)
	}
}

func TestFibo2EnhancerFailureSendsPromptVerbatim(t *testing.T) {
	const prompt = `a shop window with a sign that says "CLOSED"`
	cases := []struct {
		name   string
		fe     *fakeEnhancer
		noLoad bool
	}{
		{name: "model not running", fe: &fakeEnhancer{content: fibo2Reply}, noLoad: true},
		{name: "http 500", fe: &fakeEnhancer{status: http.StatusInternalServerError}},
		{name: "prose reply", fe: &fakeEnhancer{content: `A shop window with a sign that says "CLOSED".`}},
		{name: "quoted text dropped", fe: &fakeEnhancer{content: fibo2Reply}},
		{name: "ideogram4 caption", fe: &fakeEnhancer{content: `{"high_level_description":"a shop","compositional_deconstruction":{"background":"a street","elements":[{"type":"text","bbox":[100,100,900,300],"text":"CLOSED","desc":"a sign"}]}}`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCaptionRigFor(t, "fibo-2.0", tc.fe, "{model: enhancer-a, timeout_seconds: 1}", !tc.noLoad)
			rec := c.send(t, txt2imgPath, `{"prompt":"a shop window with a sign that says \"CLOSED\""}`, "")
			if got := c.sent(t); got != prompt {
				t.Fatalf("sd-server prompt = %s, want the prompt verbatim", got)
			}
			if src := rec.Header().Get("X-SD-Shim-Prompt-Source"); src != "original" {
				t.Fatalf("source = %q", src)
			}
		})
	}
}

func TestFibo2LeavesRequestsUntouched(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 16)))
	const vgl = `{"short_description":"a red car","text_render":[]}`
	cases := []struct{ name, path, body, want string }{
		{"enhance_prompt false", txt2imgPath, `{"prompt":"a red car","enhance_prompt":false}`, "a red car"},
		{"already a VGL caption", txt2imgPath, `{"prompt":"{\"short_description\":\"a red car\",\"text_render\":[]}"}`, vgl},
		{"img2img edit", "/sdapi/v1/img2img", `{"prompt":"make the car red","init_images":["` + png + `"]}`, "make the car red"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fe := &fakeEnhancer{content: fibo2Reply}
			c := newCaptionRigFor(t, "fibo-2.0", fe, "{model: enhancer-a, timeout_seconds: 5}", true)
			rec := c.send(t, tc.path, tc.body, "")
			if got := c.sent(t); got != tc.want {
				t.Fatalf("sd-server prompt = %q", got)
			}
			if len(fe.chats()) != 0 || rec.Header().Get("X-SD-Shim-Prompt-Source") != "" {
				t.Fatalf("enhancer asked %d times, source %q", len(fe.chats()), rec.Header().Get("X-SD-Shim-Prompt-Source"))
			}
		})
	}
}

// A quoted name or style reference is not text to draw: the caption may keep it
// in any string value, so it is not forced into text_render, but dropping it
// altogether still fails the check.
func TestFibo2QuotedNameNeedNotBeRendered(t *testing.T) {
	if !strings.Contains(fibo2SystemPrompt, "Put it in text_render only when the request asks for it to be shown") {
		t.Fatal("system prompt does not limit text_render to text the request asks to show")
	}
	const prompt = `a dog named "Rex" in the style of "Glimmerfold Studio"`
	const tail = `"background_setting":"a meadow","lighting":{"conditions":"soft"},"aesthetics":{"composition":"centred"},"style_medium":"digital illustration"`
	cases := map[string]string{
		"in descriptions": `{"short_description":"a dog called Rex","objects":[{"description":"a scruffy terrier named Rex"}],` + tail + `,"artistic_style":"Glimmerfold Studio"}`,
		"in text_render":  `{"short_description":"a dog",` + tail + `,"text_render":[{"text":"Rex"},{"text":"Glimmerfold Studio"}]}`,
	}
	for name, reply := range cases {
		got, err := checkedFibo2Caption(prompt, reply)
		if err != nil {
			t.Fatalf("%s: rejected: %v", name, err)
		}
		if name == "in descriptions" && strings.Contains(got, "text_render") {
			t.Fatalf("%s: text_render added: %s", name, got)
		}
	}
	if _, err := checkedFibo2Caption(prompt, `{"short_description":"a dog",`+tail+`,"artistic_style":"Glimmerfold Studio"}`); err == nil {
		t.Fatal("reply without the quoted name accepted")
	}
	// A key name is not a value: a quoted span that only matches a key is dropped.
	if _, err := checkedFibo2Caption(`a "lighting" study`, `{"short_description":"a study",`+tail+`}`); err == nil {
		t.Fatal("quoted span matched only by a key name accepted")
	}
}

func TestFibo2CaptionCheck(t *testing.T) {
	const minimal = `"short_description":"a cafe","background_setting":"a street","lighting":{"conditions":"soft"},"aesthetics":{"composition":"centred"},"style_medium":"photography"`
	t.Run("fenced, empties dropped, no HTML escaping", func(t *testing.T) {
		reply := "```json\n{" + minimal + `,"objects":[],"photographic_characteristics":{},"text_render":[{"text":"Tea & <Cake>"}],"context":"","extra":"x"}` + "\n```"
		got, err := checkedFibo2Caption(`a cafe sign that says "Tea & <Cake>"`, reply)
		want := `{"short_description":"a cafe","background_setting":"a street","lighting":{"conditions":"soft"},"aesthetics":{"composition":"centred"},"style_medium":"photography","text_render":[{"text":"Tea & <Cake>"}]}`
		if err != nil || got != want {
			t.Fatalf("got %s, %v\nwant %s", got, err, want)
		}
	})
	bad := map[string]string{
		"no short_description": `{"background_setting":"a street","lighting":{},"aesthetics":{},"style_medium":"photography"}`,
		"no style_medium":      `{"short_description":"a cafe","background_setting":"a street","lighting":{},"aesthetics":{}}`,
		"no lighting":          `{"short_description":"a cafe","background_setting":"a street","aesthetics":{},"style_medium":"photography"}`,
		"no aesthetics":        `{"short_description":"a cafe","background_setting":"a street","lighting":{},"style_medium":"photography"}`,
		"lighting is a string": `{"short_description":"a cafe","background_setting":"a street","lighting":"soft","aesthetics":{},"style_medium":"photography"}`,
		"objects not objects":  `{` + minimal + `,"objects":["a cafe"]}`,
		"object no desc":       `{` + minimal + `,"objects":[{"location":"center"}]}`,
		"blank text entry":     `{` + minimal + `,"text_render":[{"text":" "}]}`,
		"empty lighting":       `{"short_description":"a cafe","background_setting":"a street","lighting":{},"aesthetics":{"composition":"centred"},"style_medium":"photography"}`,
		"empty aesthetics":     `{"short_description":"a cafe","background_setting":"a street","lighting":{"conditions":"soft"},"aesthetics":{"aesthetic_score":"high"},"style_medium":"photography"}`,
		"blank aesthetics":     `{"short_description":"a cafe","background_setting":"a street","lighting":{"conditions":"soft"},"aesthetics":{"composition":" ","mood_atmosphere":"\t"},"style_medium":"photography"}`,
	}
	for name, reply := range bad {
		if got, err := checkedFibo2Caption("a cafe", reply); err == nil {
			t.Errorf("%s: accepted as %s", name, got)
		}
	}
}
