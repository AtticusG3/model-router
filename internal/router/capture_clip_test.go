package router

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"testing"
)

// A 2 MB chat request with one long message keeps its JSON: only that message
// is clipped, with a marker, and every other field is kept as sent.
func TestClipCaptureChatRequest(t *testing.T) {
	// Accents, quotes, HTML and newlines, so the clip lands among escapes.
	long := strings.Repeat("Línea de prueba \"citada\" <b>\n", (2<<20)/43)
	body, _ := json.Marshal(map[string]any{
		"model":    "chat-a",
		"messages": []map[string]string{{"role": "system", "content": "Be brief."}, {"role": "user", "content": long}},
		"stream":   false,
	})
	if len(body) < 2_000_000 {
		t.Fatalf("test body %d bytes, want about 2 MB", len(body))
	}
	got, fields, truncated := clipCapture(body, "request.")
	if truncated || len(got) > maxCaptureBody {
		t.Fatalf("truncated %v, %d bytes; want clipped inside the %d budget", truncated, len(got), maxCaptureBody)
	}
	if want := []string{"request.messages[1].content"}; !slices.Equal(fields, want) {
		t.Fatalf("fields = %q, want %q", fields, want)
	}
	var req struct {
		Model    string
		Stream   *bool
		Messages []struct{ Role, Content string }
	}
	if err := json.Unmarshal([]byte(got), &req); err != nil {
		t.Fatalf("clipped body is not JSON: %v", err)
	}
	if req.Model != "chat-a" || req.Stream == nil || len(req.Messages) != 2 || req.Messages[0].Content != "Be brief." || req.Messages[1].Role != "user" {
		t.Fatalf("clipped body lost fields: model %q stream %v messages %d", req.Model, req.Stream, len(req.Messages))
	}
	kept, _, ok := strings.Cut(req.Messages[1].Content, "…[clipped ")
	if !ok || !strings.HasPrefix(long, kept) || len(kept) < maxCaptureBody/2 {
		t.Fatalf("content keeps %d bytes, marker %v; want a long prefix and a clip marker", len(kept), ok)
	}
}

// A streamed reasoning trace over the budget keeps its first and last events
// (finish reason and usage) and summarises the middle as one SSE comment.
func TestClipCaptureReasoningStream(t *testing.T) {
	first := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"
	last := "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":7000}}\n\ndata: [DONE]\n\n"
	var sse bytes.Buffer
	sse.WriteString(first)
	for sse.Len() < 600<<10 {
		sse.WriteString("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"weighing the pump options \"}}]}\n\n")
	}
	sse.WriteString(last)
	got, fields, truncated := clipCapture(sse.Bytes(), "response.")
	if truncated || len(got) > maxCaptureBody {
		t.Fatalf("truncated %v, %d bytes; want clipped inside the %d budget", truncated, len(got), maxCaptureBody)
	}
	if !strings.HasPrefix(got, first) || !strings.HasSuffix(got, last) || !strings.Contains(got, "\n\n: [clipped ") {
		t.Fatalf("stream lost its ends or the gap marker: %.120q … %.120q", got, got[len(got)-120:])
	}
	if len(fields) != 1 || !strings.HasPrefix(fields[0], "response.events[") {
		t.Fatalf("fields = %q, want one dropped event range", fields)
	}
	for _, l := range strings.Split(got, "\n") {
		if d, ok := strings.CutPrefix(l, "data: "); ok && d != "[DONE]" && !json.Valid([]byte(d)) {
			t.Fatalf("event %.80q is not JSON", d)
		}
	}
	if s := parseSSEStats([]byte(got)); s.CompletionTokens != 7000 {
		t.Fatalf("usage lost from the clipped stream: %+v", s)
	}
}

// One long event, such as a streamed image, is clipped inside its JSON data.
func TestClipCaptureStreamedImageEvent(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString(append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 1<<20)...))
	sse := "event: image_generation.partial_image\ndata: {\"type\":\"image_generation.partial_image\",\"b64_json\":\"" + b64 + "\"}\n\ndata: [DONE]\n\n"
	got, fields, truncated := clipCapture([]byte(sse), "response.")
	if truncated || len(got) > maxCaptureBody {
		t.Fatalf("truncated %v, %d bytes", truncated, len(got))
	}
	if want := []string{"response.events[0].b64_json"}; !slices.Equal(fields, want) {
		t.Fatalf("fields = %q, want %q", fields, want)
	}
	want := "event: image_generation.partial_image\ndata: {\"type\":\"image_generation.partial_image\",\"b64_json\":\"[image 0: png 1.0 MB]\"}\n\ndata: [DONE]\n\n"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

// A body under the budget is stored byte for byte, whatever it holds.
func TestClipCaptureKeepsShortBodies(t *testing.T) {
	for _, b := range []string{"", "{ \"model\" : \"chat-a\",\n  \"n\":1e3 }", "data: {\"x\":1}\n\n", "not json"} {
		got, fields, truncated := clipCapture([]byte(b), "request.")
		if got != b || fields != nil || truncated {
			t.Errorf("%q: got %q, fields %q, truncated %v; want it unchanged", b, got, fields, truncated)
		}
	}
}

// A body that is not JSON or SSE, JSON cut short, or JSON too dense to fit by
// clipping strings keeps the raw clip and is flagged truncated.
func TestClipCaptureCutsOtherBodies(t *testing.T) {
	for name, b := range map[string][]byte{
		"text":     bytes.Repeat([]byte("x"), maxCaptureBody+10),
		"cut JSON": append([]byte(`{"prompt":"`), bytes.Repeat([]byte("y"), maxCaptureBody)...),
		"numbers":  []byte("[" + strings.Repeat("1,", maxCaptureBody/2) + "1]"),
	} {
		got, fields, truncated := clipCapture(b, "response.")
		if !truncated || fields != nil || got != string(b[:maxCaptureBody]) {
			t.Errorf("%s: %d bytes, fields %q, truncated %v; want the raw clip", name, len(got), fields, truncated)
		}
	}
}

// A JSON request body is buffered past the capture clip so it can be clipped by
// field; an upload stops at the clip as before.
func TestCaptureReaderKeepsJSONForClipping(t *testing.T) {
	for _, tc := range []struct {
		body  string
		whole bool
	}{
		{" {\"prompt\":\"" + strings.Repeat("z", 1<<20) + "\"}", true},
		{strings.Repeat("z", 1<<20), false},
	} {
		c := &captureReader{rc: io.NopCloser(strings.NewReader(tc.body)), max: maxClipSource}
		if _, err := io.Copy(io.Discard, c); err != nil {
			t.Fatal(err)
		}
		want := maxCaptureBody
		if tc.whole {
			want = len(tc.body)
		}
		if c.buf.Len() != want || c.truncated == tc.whole {
			t.Errorf("kept %d bytes, truncated %v; want %d bytes, truncated %v", c.buf.Len(), c.truncated, want, !tc.whole)
		}
	}
}
