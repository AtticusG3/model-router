package router

import (
	"embed"
	"encoding/json"
	"io/fs"
	"path"
	"strings"
)

//go:embed ui/index.html
var uiIndexHTML string

//go:embed ui/style.css
var uiCSS string

//go:embed ui/app.js
var uiJS string

// uiMediaCaptureJS is the Playground mic and camera capture script. It loads
// before app.js.
//
//go:embed ui/mediacapture.js
var uiMediaCaptureJS string

// uiAppJS is app.js as served, with the bundled family icons filled in.
var uiAppJS = strings.Replace(uiJS, "__EMBED_FAMILY_ICONS__", familyIconsJSON(), 1)

// webUIHTML is the operator shell served at /ui/. Assembled from go:embed
// files so the fleet needs only the model-router binary.
var webUIHTML = strings.NewReplacer(
	"__EMBED_CSS__", uiCSS,
	"__EMBED_MEDIA_CAPTURE_JS__", uiMediaCaptureJS,
	"__EMBED_JS__", uiAppJS,
).Replace(uiIndexHTML)

//go:embed ui/families/*.svg
var familyIconFS embed.FS

// familyIconsJSON maps each family id (the file name) to its SVG markup.
func familyIconsJSON() string {
	icons := map[string]string{}
	files, _ := fs.Glob(familyIconFS, "ui/families/*.svg")
	for _, f := range files {
		b, _ := familyIconFS.ReadFile(f)
		icons[strings.TrimSuffix(path.Base(f), ".svg")] = strings.TrimSpace(string(b))
	}
	j, _ := json.Marshal(icons)
	return string(j)
}
