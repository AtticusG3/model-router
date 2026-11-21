package router

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The video poll must survive a transient status error and a rejected DELETE.
// It runs the real embedded UI script under node, when node is installed.
func TestEmbeddedUIVideoJobPolling(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; the video polling regression runs where node is available")
	}
	app := filepath.Join(t.TempDir(), "app.js")
	if err := os.WriteFile(app, []byte(uiJS), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "testdata/video_poll_test.js", app).CombinedOutput()
	if err != nil {
		t.Fatalf("video polling regression: %v\n%s", err, out)
	}
}
