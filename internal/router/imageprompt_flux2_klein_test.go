package router

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// flux2KleinProse is an enhancer reply for the prompt in
// TestFlux2KleinPlainPromptBecomesProse: the golden example in the research
// note.
const flux2KleinProse = `A white stone lighthouse stands on a rocky headland above a choppy grey-green sea, waves breaking white against the dark rocks below. A long canvas banner strung between two poles at the base of the tower reads "HARBOUR DAYS" in bold navy capitals on a cream background. The lighthouse sits slightly left of centre in a square frame, with the rugged coastline running away to the right and a few gulls in the sky. Low late-afternoon sun from the left rakes across the stone and casts long warm shadows over the rocks. Style: crisp documentary photograph. Mood: bright and festive.`

func TestFlux2KleinPlainPromptBecomesProse(t *testing.T) {
	fe := &fakeEnhancer{content: flux2KleinProse}
	c := newCaptionRigFor(t, "flux2-klein", fe, "{model: enhancer-a, timeout_seconds: 5}", true)
	const prompt = `a lighthouse on a rocky coast with a banner that says "HARBOUR DAYS"`
	rec := c.send(t, txt2imgPath, `{"prompt":"a lighthouse on a rocky coast with a banner that says \"HARBOUR DAYS\"","negative_prompt":"blurry","width":1024,"height":1024}`, "")

	if got := c.sent(t); got != flux2KleinProse {
		t.Fatalf("sd-server prompt = %s\nwant %s", got, flux2KleinProse)
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
	if sys["content"] != flux2KleinSystemPrompt {
		t.Fatalf("system prompt = %v", sys["content"])
	}
	if s, _ := user["content"].(string); !strings.Contains(s, "1024 pixels wide (x) by 1024 pixels tall (y)") || !strings.HasSuffix(s, "Request: "+prompt) {
		t.Fatalf("user message = %q", s)
	}
}

func TestFlux2KleinEnhancerFailureSendsPromptVerbatim(t *testing.T) {
	const prompt = `a market stall with a sign that says "OPEN LATE"`
	cases := []struct {
		name   string
		fe     *fakeEnhancer
		noLoad bool
	}{
		{name: "model not running", fe: &fakeEnhancer{content: flux2KleinProse}, noLoad: true},
		{name: "http 500", fe: &fakeEnhancer{status: http.StatusInternalServerError}},
		{name: "quoted text dropped", fe: &fakeEnhancer{content: flux2KleinProse}},
		{name: "json reply", fe: &fakeEnhancer{content: `{"scene":"a market stall with a sign that says \"OPEN LATE\""}`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCaptionRigFor(t, "flux2-klein", tc.fe, "{model: enhancer-a, timeout_seconds: 1}", !tc.noLoad)
			rec := c.send(t, txt2imgPath, `{"prompt":"a market stall with a sign that says \"OPEN LATE\""}`, "")
			if got := c.sent(t); got != prompt {
				t.Fatalf("sd-server prompt = %s, want the prompt verbatim", got)
			}
			if src := rec.Header().Get("X-SD-Shim-Prompt-Source"); src != "original" {
				t.Fatalf("source = %q", src)
			}
		})
	}
}

func TestFlux2KleinLeavesRequestsUntouched(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 16)))
	cases := []struct{ name, path, body string }{
		{"enhance_prompt false", txt2imgPath, `{"prompt":"a red car","enhance_prompt":false}`},
		{"img2img", "/sdapi/v1/img2img", `{"prompt":"a red car","init_images":["` + png + `"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fe := &fakeEnhancer{content: flux2KleinProse}
			c := newCaptionRigFor(t, "flux2-klein", fe, "{model: enhancer-a, timeout_seconds: 5}", true)
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
