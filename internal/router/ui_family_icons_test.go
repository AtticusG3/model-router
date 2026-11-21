package router

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A catalog row with a known family draws its bundled icon, a row without one
// keeps the generic model glyph, and a row can carry both an icon and peer
// chips. It runs the real embedded UI script under node, when node is installed.
func TestEmbeddedUIModelCatalogFamilyAndPeers(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; the family icon regression runs where node is available")
	}
	app := filepath.Join(t.TempDir(), "app.js")
	if err := os.WriteFile(app, []byte(uiAppJS), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "testdata/model_family_test.js", app).CombinedOutput()
	if err != nil {
		t.Fatalf("family icon regression: %v\n%s", err, out)
	}
}
