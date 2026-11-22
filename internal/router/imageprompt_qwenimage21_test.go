package router

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// qwenImage21Prose is an enhancer reply for the prompt in
// TestQwenImage21PlainPromptBecomesProse: the golden example in the research
// note.
const qwenImage21Prose = `The image is a wide photograph of a narrow night-market food stall, its glowing signboard set against a dark street washed in red and amber light. Above the stall hangs a long rectangular lightbox sign whose upper line reads "夜市面馆" in large bold white brush-style characters, and whose lower line reads "NIGHT NOODLES" in smaller condensed red capitals. Under the sign, a cook in a white apron and dark cap stands behind a steel counter, lifting a tangle of noodles from a steaming pot with long wooden chopsticks. Steam rises past the hanging bulbs and catches their warm light. Stacked ceramic bowls, a bottle of chilli oil and a row of soy sauce jars line the counter in the foreground. To the left, red paper lanterns sway on a wire, and to the right a second stall fades into soft shadow. The wet asphalt in front of the stall reflects the sign and the lanterns as long smeared streaks of colour. The light comes from the sign and the bulbs above the counter, so the cook's face is lit from above and the shadows of the bowls fall toward the street. The scene appears to be shot at eye level with a 35mm lens, with a shallow depth of field that keeps the cook and the sign sharp. The palette is deep red, amber and warm white against near-black, and the style is a candid documentary street photograph.`

func TestQwenImage21PlainPromptBecomesProse(t *testing.T) {
	fe := &fakeEnhancer{content: qwenImage21Prose}
	c := newCaptionRigFor(t, "qwen-image-2.1", fe, "{model: enhancer-a, timeout_seconds: 5}", true)
	const prompt = `a night market noodle stall with a sign that says "夜市面馆" and "NIGHT NOODLES"`
	rec := c.send(t, txt2imgPath, `{"prompt":"a night market noodle stall with a sign that says \"夜市面馆\" and \"NIGHT NOODLES\"","negative_prompt":"blurry","width":1664,"height":928}`, "")

	if got := c.sent(t); got != qwenImage21Prose {
		t.Fatalf("sd-server prompt = %s\nwant %s", got, qwenImage21Prose)
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
	if sys["content"] != qwenImage21SystemPrompt {
		t.Fatalf("system prompt = %v", sys["content"])
	}
	if s, _ := user["content"].(string); !strings.Contains(s, "1664 pixels wide (x) by 928 pixels tall (y)") || !strings.HasSuffix(s, "Request: "+prompt) {
		t.Fatalf("user message = %q", s)
	}
}

func TestQwenImage21EnhancerFailureSendsPromptVerbatim(t *testing.T) {
	const prompt = `a shop window with a sign that says "OPEN 24 HOURS"`
	cases := []struct {
		name   string
		fe     *fakeEnhancer
		noLoad bool
	}{
		{name: "model not running", fe: &fakeEnhancer{content: qwenImage21Prose}, noLoad: true},
		{name: "http 500", fe: &fakeEnhancer{status: http.StatusInternalServerError}},
		{name: "quoted text dropped", fe: &fakeEnhancer{content: qwenImage21Prose}},
		{name: "vendor json reply", fe: &fakeEnhancer{content: `{"rewritten_prompt":"a shop window with a sign that says \"OPEN 24 HOURS\"","wh_ratio":"1:1"}`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCaptionRigFor(t, "qwen-image-2.1", tc.fe, "{model: enhancer-a, timeout_seconds: 1}", !tc.noLoad)
			rec := c.send(t, txt2imgPath, `{"prompt":"a shop window with a sign that says \"OPEN 24 HOURS\""}`, "")
			if got := c.sent(t); got != prompt {
				t.Fatalf("sd-server prompt = %s, want the prompt verbatim", got)
			}
			if src := rec.Header().Get("X-SD-Shim-Prompt-Source"); src != "original" {
				t.Fatalf("source = %q", src)
			}
		})
	}
}

func TestQwenImage21LeavesRequestsUntouched(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 16)))
	cases := []struct{ name, path, body string }{
		{"enhance_prompt false", txt2imgPath, `{"prompt":"a red car","enhance_prompt":false}`},
		{"img2img edit", "/sdapi/v1/img2img", `{"prompt":"a red car","init_images":["` + png + `"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fe := &fakeEnhancer{content: qwenImage21Prose}
			c := newCaptionRigFor(t, "qwen-image-2.1", fe, "{model: enhancer-a, timeout_seconds: 5}", true)
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

func TestQwenImage21CheckKeepsCJKQuotesAndRGBATrigger(t *testing.T) {
	const full = rgbaTrigger + " A red paper lantern."
	cases := []struct {
		name, prompt, reply string
		ok                  bool
	}{
		{"curly quotes kept", `一家面馆，招牌写着“夜市面馆”`, qwenImage21Prose, true},
		{"curly quotes translated", `一家面馆，招牌写着“夜市面馆”`, `The image is a wide photograph of a stall whose sign reads "NIGHT NOODLE HOUSE".`, false},
		{"corner quotes changed", `一家书店，招牌写着「旧书屋」`, `The image is a wide photograph of a shop whose sign reads "旧书店".`, false},
		{"full trigger kept", full, rgbaTrigger + " The image is a square illustration of a red paper lantern.", true},
		{"second sentence dropped", full, rgbaTriggerFirst + " The image is a square illustration of a red paper lantern.", false},
		{"first sentence alone kept", rgbaTriggerFirst + " A red paper lantern.", rgbaTriggerFirst + " The image is a square illustration of a red paper lantern.", true},
		{"trigger moved", full, "The image is a square illustration of a red paper lantern. " + rgbaTrigger, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := qwenImage21Profile.check(tc.prompt, tc.reply)
			if (err == nil) != tc.ok {
				t.Fatalf("check error = %v, want ok %v", err, tc.ok)
			}
		})
	}
}
