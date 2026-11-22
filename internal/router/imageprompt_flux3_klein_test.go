package router

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// flux3KleinCaption is an enhancer reply for the prompt in
// TestFlux3KleinPlainPromptBecomesCaption: the golden example in the research
// note.
const flux3KleinCaption = `A square, eye-level photograph of a small red fox curled up asleep inside an open vintage brown leather suitcase, which rests on a worn green fabric seat of an old passenger train carriage. The fox lies nose to tail in the centre of the frame, its rust-orange fur soft and slightly ruffled, white chest fur showing and its black-tipped ears folded back, eyes closed. The suitcase lid stands open behind it, lined with faded cream cloth, and a paper luggage tag tied to the handle reads "PLATFORM 9" in neat black handwritten capitals. Beside the seat, a large window shows a blurred green countryside sliding past. Warm late-afternoon sunlight falls through the glass from the left, laying long soft shadows across the fox and the seat. Muted warm palette of rust, brown, cream and olive green. Natural documentary photograph with a shallow depth of field, the background gently out of focus.`

func TestFlux3KleinPlainPromptBecomesCaption(t *testing.T) {
	fe := &fakeEnhancer{content: flux3KleinCaption}
	c := newCaptionRigFor(t, "flux3-klein", fe, "{model: enhancer-a, timeout_seconds: 5}", true)
	const prompt = `a red fox asleep in a suitcase on a train, luggage tag says "PLATFORM 9"`
	rec := c.send(t, txt2imgPath, `{"prompt":"a red fox asleep in a suitcase on a train, luggage tag says \"PLATFORM 9\"","width":1024,"height":1024}`, "")

	if got := c.sent(t); got != flux3KleinCaption {
		t.Fatalf("sd-server prompt = %s\nwant %s", got, flux3KleinCaption)
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
	if sys["content"] != flux3KleinSystemPrompt {
		t.Fatalf("system prompt = %v", sys["content"])
	}
	if s, _ := user["content"].(string); !strings.Contains(s, "1024 pixels wide (x) by 1024 pixels tall (y)") || !strings.HasSuffix(s, "Request: "+prompt) {
		t.Fatalf("user message = %q", s)
	}
}

func TestFlux3KleinEnhancerFailureSendsPromptVerbatim(t *testing.T) {
	const prompt = `a tin of paint labelled "SEA GREEN"`
	cases := []struct {
		name   string
		fe     *fakeEnhancer
		noLoad bool
	}{
		{name: "model not running", fe: &fakeEnhancer{content: flux3KleinCaption}, noLoad: true},
		{name: "http 500", fe: &fakeEnhancer{status: http.StatusInternalServerError}},
		{name: "quoted text dropped", fe: &fakeEnhancer{content: flux3KleinCaption}},
		{name: "empty reply", fe: &fakeEnhancer{content: "  "}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCaptionRigFor(t, "flux3-klein", tc.fe, "{model: enhancer-a, timeout_seconds: 1}", !tc.noLoad)
			rec := c.send(t, txt2imgPath, `{"prompt":"a tin of paint labelled \"SEA GREEN\""}`, "")
			if got := c.sent(t); got != prompt {
				t.Fatalf("sd-server prompt = %s, want the prompt verbatim", got)
			}
			if src := rec.Header().Get("X-SD-Shim-Prompt-Source"); src != "original" {
				t.Fatalf("source = %q", src)
			}
		})
	}
}

func TestFlux3KleinLeavesRequestsUntouched(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 16)))
	cases := []struct{ name, path, body string }{
		{"enhance_prompt false", txt2imgPath, `{"prompt":"a red car","enhance_prompt":false}`},
		{"img2img", "/sdapi/v1/img2img", `{"prompt":"a red car","init_images":["` + png + `"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fe := &fakeEnhancer{content: flux3KleinCaption}
			c := newCaptionRigFor(t, "flux3-klein", fe, "{model: enhancer-a, timeout_seconds: 5}", true)
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
