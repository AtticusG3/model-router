package router

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// zAnimeProse is an enhancer reply for the tag-list prompt in
// TestZAnimeTagListBecomesProse, in the shape of the research note's golden
// example.
const zAnimeProse = `A young anime girl with long silver hair tied in a side ponytail with a blue scrunchie stands on a quiet school rooftop, smiling softly. She wears a white shirt, a red necktie and a navy pleated skirt that moves in the breeze. A chain-link fence and a pale evening sky fill the background, with the girl framed from the knees up in the centre of a tall canvas. Warm sunset rim light catches her hair. Clean line work, professional anime illustration with rich colour.`

func TestZAnimeTagListBecomesProse(t *testing.T) {
	fe := &fakeEnhancer{content: zAnimeProse}
	c := newCaptionRigFor(t, "z-anime", fe, "{model: enhancer-a, timeout_seconds: 5}", true)
	const prompt = "1girl, silver hair, side ponytail, blue scrunchie, school uniform, rooftop, sunset, masterpiece"
	rec := c.send(t, txt2imgPath, `{"prompt":"`+prompt+`","negative_prompt":"blurry","width":768,"height":1024}`, "")

	if got := c.sent(t); got != zAnimeProse {
		t.Fatalf("sd-server prompt = %s\nwant %s", got, zAnimeProse)
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
	if sys["content"] != zAnimeSystemPrompt || sys["content"] == zImageSystemPrompt {
		t.Fatalf("system prompt = %v", sys["content"])
	}
	if s, _ := user["content"].(string); !strings.Contains(s, "768 pixels wide (x) by 1024 pixels tall (y)") || !strings.HasSuffix(s, "Request: "+prompt) {
		t.Fatalf("user message = %q", s)
	}
}

func TestZAnimeEnhancerFailureSendsPromptVerbatim(t *testing.T) {
	const prompt = `a shrine maiden holding a sign that says "OPEN"`
	cases := []struct {
		name   string
		fe     *fakeEnhancer
		noLoad bool
	}{
		{name: "model not running", fe: &fakeEnhancer{content: zAnimeProse}, noLoad: true},
		{name: "http 500", fe: &fakeEnhancer{status: http.StatusInternalServerError}},
		{name: "quoted text dropped", fe: &fakeEnhancer{content: zAnimeProse}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCaptionRigFor(t, "z-anime", tc.fe, "{model: enhancer-a, timeout_seconds: 1}", !tc.noLoad)
			rec := c.send(t, txt2imgPath, `{"prompt":"a shrine maiden holding a sign that says \"OPEN\""}`, "")
			if got := c.sent(t); got != prompt {
				t.Fatalf("sd-server prompt = %s, want the prompt verbatim", got)
			}
			if src := rec.Header().Get("X-SD-Shim-Prompt-Source"); src != "original" {
				t.Fatalf("source = %q", src)
			}
		})
	}
}

func TestZAnimeLeavesRequestsUntouched(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 16)))
	cases := []struct{ name, path, body string }{
		{"enhance_prompt false", txt2imgPath, `{"prompt":"a red car","enhance_prompt":false}`},
		{"img2img", "/sdapi/v1/img2img", `{"prompt":"a red car","init_images":["` + png + `"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fe := &fakeEnhancer{content: zAnimeProse}
			c := newCaptionRigFor(t, "z-anime", fe, "{model: enhancer-a, timeout_seconds: 5}", true)
			rec := c.send(t, tc.path, tc.body, "")
			if got := c.sent(t); got != "a red car" {
				t.Fatalf("sd-server prompt = %q", got)
			}
			if len(fe.chats()) != 0 || rec.Header().Get("X-SD-Shim-Prompt-Source") != "" {
				t.Fatalf("enhancer asked %d times, source %q", len(fe.chats()), rec.Header().Get("X-SD-Shim-Prompt-Source"))
			}
		})
	}
}
