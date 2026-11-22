package router

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"model-router/internal/config"
)

// zImageProse is an enhancer reply for the prompt `a "OPEN" sign on a cafe`.
const zImageProse = `A small corner cafe at dusk, its front window glowing warm amber. Above the door hangs a hand-painted wooden sign reading "OPEN" in bold cream serif letters on a deep green board. Wide eye-level framing, soft light from the window, cobblestones in the foreground.`

func TestEveryCaptionFormatHasAProfile(t *testing.T) {
	for _, name := range config.ImageCaptionFormats {
		if promptProfiles[name] == nil {
			t.Errorf("caption %q passes config but has no prompt profile", name)
		}
	}
	if len(promptProfiles) != len(config.ImageCaptionFormats) {
		t.Errorf("%d profiles, %d allowed captions", len(promptProfiles), len(config.ImageCaptionFormats))
	}
}

func TestZImagePlainPromptBecomesProse(t *testing.T) {
	fe := &fakeEnhancer{content: "```\n" + zImageProse + "\n```"}
	c := newCaptionRigFor(t, "z-image", fe, "{model: enhancer-a, timeout_seconds: 5}", true)
	rec := c.send(t, txt2imgPath, `{"prompt":"a \"OPEN\" sign on a cafe","negative_prompt":"blurry","width":1280,"height":720}`, "")

	if got := c.sent(t); got != zImageProse {
		t.Fatalf("sd-server prompt = %s\nwant %s", got, zImageProse)
	}
	h := rec.Header()
	if h.Get("X-SD-Shim-Prompt-Source") != "enhancer" {
		t.Fatalf("headers = %v", h)
	}
	if got, _ := url.PathUnescape(h.Get("X-SD-Shim-Prompt-Enhanced")); got != zImageProse {
		t.Fatalf("enhanced header = %s", got)
	}
	chats := fe.chats()
	if len(chats) != 1 || chats[0]["model"] != "enhancer-a" || chats[0]["temperature"] != float64(0) {
		t.Fatalf("enhancer requests = %v", chats)
	}
	msgs, _ := chats[0]["messages"].([]any)
	sys, _ := msgs[0].(map[string]any)
	user, _ := msgs[1].(map[string]any)
	if sys["content"] != zImageSystemPrompt {
		t.Fatalf("system prompt = %v", sys["content"])
	}
	if s, _ := user["content"].(string); !strings.Contains(s, "1280 pixels wide (x) by 720 pixels tall (y)") || !strings.HasSuffix(s, `Request: a "OPEN" sign on a cafe`) {
		t.Fatalf("user message = %q", s)
	}
}

func TestZImageEnhancerFailureSendsPromptVerbatim(t *testing.T) {
	const prompt = `a "OPEN" sign on a cafe`
	cases := []struct {
		name   string
		fe     *fakeEnhancer
		noLoad bool
	}{
		{name: "model not running", fe: &fakeEnhancer{content: zImageProse}, noLoad: true},
		{name: "http 500", fe: &fakeEnhancer{status: http.StatusInternalServerError}},
		{name: "empty reply", fe: &fakeEnhancer{content: " \n "}},
		{name: "json reply", fe: &fakeEnhancer{content: testCaption}},
		{name: "quoted text dropped", fe: &fakeEnhancer{content: "A cafe at dusk with a sign that says closed."}},
		{name: "timeout", fe: &fakeEnhancer{content: zImageProse, delay: 3 * time.Second}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCaptionRigFor(t, "z-image", tc.fe, "{model: enhancer-a, timeout_seconds: 1}", !tc.noLoad)
			body, _ := json.Marshal(map[string]any{"prompt": prompt, "seed": 7})
			rec := c.send(t, txt2imgPath, string(body), "")
			if got := c.sent(t); got != prompt {
				t.Fatalf("sd-server prompt = %s, want the prompt verbatim", got)
			}
			h := rec.Header()
			if got, _ := url.PathUnescape(h.Get("X-SD-Shim-Prompt-Original")); got != prompt || h.Get("X-SD-Shim-Prompt-Source") != "original" {
				t.Fatalf("headers = %v", h)
			}
			if tc.noLoad && (len(tc.fe.chats()) != 0 || modelState(c.r, "enhancer-a") == string(StateRunning)) {
				t.Fatal("the caption step asked or started a stopped enhancer")
			}
		})
	}
}

func TestZImageLeavesRequestsUntouched(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 16)))
	cases := []struct{ name, path, body string }{
		{"enhance_prompt false", txt2imgPath, `{"prompt":"a red car","enhance_prompt":false}`},
		{"img2img", "/sdapi/v1/img2img", `{"prompt":"a red car","init_images":["` + png + `"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fe := &fakeEnhancer{content: zImageProse}
			c := newCaptionRigFor(t, "z-image", fe, "{model: enhancer-a, timeout_seconds: 5}", true)
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

func TestCheckedZImagePrompt(t *testing.T) {
	if got, err := checkedZImagePrompt(`a "OPEN" sign`, "  "+zImageProse+"\n"); err != nil || got != zImageProse {
		t.Fatalf("checked = %q, %v", got, err)
	}
	if _, err := checkedZImagePrompt(`a "OPEN" sign and "CAFE" above`, zImageProse); err == nil {
		t.Fatal("reply without the quoted CAFE accepted")
	}
}
