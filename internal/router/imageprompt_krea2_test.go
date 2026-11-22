package router

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// krea2Prose is an enhancer reply for the prompt in
// TestKrea2PlainPromptBecomesProse: the golden example in the research note.
const krea2Prose = `A small corner bakery at dawn, seen from across a quiet cobbled street. A wooden sign above the door reads "FRESH BREAD" in cream painted letters on a dark green board. Warm yellow light spills from the wide front window onto the wet stones, and loaves and pastries are stacked on shelves behind the glass. The shop fills the centre of a wide frame, with the empty street in the foreground and a pale blue sky above the rooftops. Soft early morning light, muted warm palette, natural documentary photograph.`

func TestKrea2PlainPromptBecomesProse(t *testing.T) {
	fe := &fakeEnhancer{content: krea2Prose}
	c := newCaptionRigFor(t, "krea2", fe, "{model: enhancer-a, timeout_seconds: 5}", true)
	const prompt = `a bakery at dawn with a sign that says "FRESH BREAD"`
	rec := c.send(t, txt2imgPath, `{"prompt":"a bakery at dawn with a sign that says \"FRESH BREAD\"","negative_prompt":"blurry","width":1344,"height":768}`, "")

	if got := c.sent(t); got != krea2Prose {
		t.Fatalf("sd-server prompt = %s\nwant %s", got, krea2Prose)
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
	if sys["content"] != krea2SystemPrompt {
		t.Fatalf("system prompt = %v", sys["content"])
	}
	if s, _ := user["content"].(string); !strings.Contains(s, "1344 pixels wide (x) by 768 pixels tall (y)") || !strings.HasSuffix(s, "Request: "+prompt) {
		t.Fatalf("user message = %q", s)
	}
}

func TestKrea2EnhancerFailureSendsPromptVerbatim(t *testing.T) {
	const prompt = `a shop window with a sign that says "CLOSED"`
	cases := []struct {
		name   string
		fe     *fakeEnhancer
		noLoad bool
	}{
		{name: "model not running", fe: &fakeEnhancer{content: krea2Prose}, noLoad: true},
		{name: "http 500", fe: &fakeEnhancer{status: http.StatusInternalServerError}},
		{name: "quoted text dropped", fe: &fakeEnhancer{content: krea2Prose}},
		{name: "json reply", fe: &fakeEnhancer{content: `{"prompt":"a shop window with a sign that says \"CLOSED\""}`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCaptionRigFor(t, "krea2", tc.fe, "{model: enhancer-a, timeout_seconds: 1}", !tc.noLoad)
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

func TestKrea2LeavesRequestsUntouched(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 16)))
	cases := []struct{ name, path, body string }{
		{"enhance_prompt false", txt2imgPath, `{"prompt":"a red car","enhance_prompt":false}`},
		{"img2img", "/sdapi/v1/img2img", `{"prompt":"a red car","init_images":["` + png + `"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fe := &fakeEnhancer{content: krea2Prose}
			c := newCaptionRigFor(t, "krea2", fe, "{model: enhancer-a, timeout_seconds: 5}", true)
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
