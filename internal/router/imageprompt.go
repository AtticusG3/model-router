package router

import "fmt"

// promptProfile is how one image_pipeline.caption family enhances a prompt:
// the enhancer's system prompt and user message, the check its reply must
// pass, and the prompt sent when the enhancer fails.
type promptProfile struct {
	system string
	user   func(prompt string, width, height int) string
	// check turns the enhancer's reply into the prompt sd-server gets, or
	// rejects it.
	check func(prompt, reply string) (string, error)
	// fallback is the prompt sent when the enhancer fails, with source
	// "template". nil sends the original prompt verbatim, with source
	// "original".
	fallback func(prompt string) string
}

// promptProfiles maps each image_pipeline.caption value to its profile. A new
// family adds a file and one line here, plus its name in
// config.ImageCaptionFormats.
var promptProfiles = map[string]*promptProfile{
	"ideogram4":   &ideogram4Profile,
	"z-image":     &zImageProfile,
	"z-anime":     &zAnimeProfile,
	"krea2":       &krea2Profile,
	"flux2-klein": &flux2KleinProfile,
}

// canvasRequest is the enhancer's user message: the canvas size, so the
// enhancer can lay the scene out for it, then the raw prompt.
func canvasRequest(prompt string, width, height int) string {
	return fmt.Sprintf("Canvas: %d pixels wide (x) by %d pixels tall (y).\n\nRequest: %s", width, height, prompt)
}
