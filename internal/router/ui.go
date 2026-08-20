package router

import (
	_ "embed"
	"strings"
)

//go:embed ui/index.html
var uiIndexHTML string

//go:embed ui/style.css
var uiCSS string

//go:embed ui/app.js
var uiJS string

// webUIHTML is the operator shell served at /ui/. Assembled from go:embed
// files so the fleet needs only the model-router binary.
var webUIHTML = strings.NewReplacer(
	"__EMBED_CSS__", uiCSS,
	"__EMBED_JS__", uiJS,
).Replace(uiIndexHTML)
