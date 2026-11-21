package router

import "testing"

// Catalog rows rendered by the real embedded UI script under node: family
// icons, the one-line details, the title, Origin, the Load/Unload toggle and
// the Open UI state for each process state and origin.
func TestEmbeddedUIModelCatalogRows(t *testing.T) {
	runUIScript(t, "model_family_test.js")
}
