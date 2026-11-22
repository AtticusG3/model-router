package router

import (
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The capture helpers (WAV encoding, resample, frame picking, content parts)
// pass their node checks, and the WAV they write is one the router's own
// parser reads as the right length, so STT and the voice library accept it.
func TestEmbeddedUIMediaCaptureHelpers(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; mediacapture_test.js runs where node is available")
	}
	dir := t.TempDir()
	js := filepath.Join(dir, "mediacapture.js")
	if err := os.WriteFile(js, []byte(uiMediaCaptureJS), 0o600); err != nil {
		t.Fatal(err)
	}
	wav := filepath.Join(dir, "tone.wav")
	out, err := exec.Command(node, filepath.Join("testdata", "mediacapture_test.js"), js, wav).CombinedOutput()
	if err != nil {
		t.Fatalf("mediacapture_test.js: %v\n%s", err, out)
	}
	b, err := os.ReadFile(wav)
	if err != nil {
		t.Fatal(err)
	}
	if s := wavSeconds(b, int64(len(b))); math.Abs(s-1) > 0.001 {
		t.Fatalf("wavSeconds of the encoded 1 s tone = %v", s)
	}
}

// The capture script is embedded, loads before app.js, and every container it
// mounts into is in the page.
func TestEmbeddedUIMediaCaptureControls(t *testing.T) {
	ci, ai := strings.Index(webUIHTML, "root.MediaCapture="), strings.Index(webUIHTML, "window.sendChat=")
	if ci < 0 || ai < 0 || ci > ai {
		t.Fatalf("capture script must be embedded before app.js (capture at %d, app at %d)", ci, ai)
	}
	if strings.Contains(webUIHTML, "__EMBED_MEDIA_CAPTURE_JS__") {
		t.Fatal("capture placeholder left in the page")
	}
	for _, want := range []string{
		`id="chat-media"`, `id="chat-cam"`, `id="chat-mic"`, `id="chat-frames"`, `id="chat-image-file"`,
		`id="chat-attach"`, `id="chat-media-hint"`,
		`id="transcribe-mic"`,
		`id="clone-mic"`, `id="clone-file"`, `id="clone-text"`, `id="clone-language"`, `id="clone-use"`,
		`id="clone-name"`, `id="clone-consent"`, `id="clone-save"`, `id="clone-status"`,
		`id="video-cam"`, `id="video-cam-take"`,
		"window.routerModels=", "chatContent(", "transcriptionFile()", "videoReference()", "speechClone(model)",
		"getUserMedia", "isSecureContext", "MediaRecorder",
	} {
		if !strings.Contains(webUIHTML, want) {
			t.Errorf("embedded UI missing %q", want)
		}
	}
}
