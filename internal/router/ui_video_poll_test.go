package router

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// runUIScript runs a regression script from testdata against the real
// embedded UI script under node, when node is installed. args follow the
// app.js path.
func runUIScript(t *testing.T, script string, args ...string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; " + script + " runs where node is available")
	}
	app := filepath.Join(t.TempDir(), "app.js")
	if err := os.WriteFile(app, []byte(uiAppJS), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, append([]string{filepath.Join("testdata", script), app}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", script, err, out)
	}
}

// The video poll must survive a transient status error and a rejected DELETE.
func TestEmbeddedUIVideoJobPolling(t *testing.T) {
	runUIScript(t, "video_poll_test.js")
}

// Playground history restores entries, keeps its caps and revokes the blob
// URLs it drops.
func TestEmbeddedUIPlaygroundHistoryScript(t *testing.T) {
	runUIScript(t, "history_test.js")
}
