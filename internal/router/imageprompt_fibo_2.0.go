package router

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// fibo2Profile turns a plain txt2img prompt into the structured VGL caption
// Fibo 2.0 is trained on, and falls back to the prompt verbatim: Fibo 2.0
// still reads plain text, so no template is needed. Edit requests are left
// alone, because an edit caption describes the source image the enhancer
// cannot see. See docs/research/fibo-2.0-prompt-style.md.
var fibo2Profile = promptProfile{
	system: fibo2SystemPrompt,
	user:   canvasRequest,
	check:  checkedFibo2Caption,
}

// fibo2SystemPrompt asks for a VGL caption with the vendor's top-level keys in
// the vendor's order, the required keys from the vendor's VGL schema, and the
// object, lighting, aesthetics, camera and text sub-keys the vendor documents.
const fibo2SystemPrompt = `You turn a short image request into one structured caption for a text-to-image model that reads JSON captions.
Return only the JSON object, with no markdown fence and no commentary.

Schema, keys in this order. Leave out an optional key you have nothing for:
{
  "short_description": "one or two sentences describing the whole image",
  "objects": [
    {"description": "what the subject is", "location": "where it sits in the frame, for example center or bottom-left", "relative_size": "small, medium or large within the frame", "shape_and_color": "...", "texture": "...", "appearance_details": "...", "relationship": "how it relates to the other objects", "orientation": "..."}
  ],
  "background_setting": "the environment behind the subjects",
  "lighting": {"conditions": "...", "direction": "...", "shadows": "..."},
  "aesthetics": {"composition": "...", "color_scheme": "...", "mood_atmosphere": "..."},
  "photographic_characteristics": {"depth_of_field": "...", "focus": "...", "camera_angle": "...", "lens_focal_length": "..."},
  "style_medium": "one of photography, digital illustration, 3D render, vector art, watercolor, oil painting, pencil sketch, anime, pixel art",
  "text_render": [{"text": "the exact words", "location": "...", "size": "...", "color": "...", "font": "...", "appearance_details": "..."}],
  "context": "what kind of image this is",
  "artistic_style": "at most three words"
}
An object that is a person may also have "pose", "expression", "clothing", "action", "gender" and "skin_tone_and_texture". A group of alike objects may have "number_of_objects".

Rules:
- short_description, background_setting, lighting, aesthetics and style_medium are required. All values are strings except objects and text_render, which are arrays of objects.
- Keep the subject, count, actions and intent of the request. Never add subjects that were not asked for.
- Give every distinct subject its own entry in objects with a non-empty description, and keep its details in that entry.
- Lay the scene out for the canvas given in the request.
- Keep any style, medium, camera or mood the request names. If it names none, pick one style_medium that suits the subject.
- text_render: one entry for each piece of text the request asks to show, with the words copied exactly as given. If the request asks for no text, leave text_render out. Never add text that was not asked for.
- Keep every double-quoted span of the request word for word somewhere in the caption. Put it in text_render only when the request asks for it to be shown in the image; a quoted name, title or style reference goes in the matching description, style or context value instead.
- Do not write aesthetic_score, preference_score or a negative prompt.
- Write the values in English. Keep text_render text in its original language.
`

// fibo2Caption is a VGL caption with the top-level keys in the vendor's order.
// Keys the schema does not name are dropped, and so are empty values, as the
// vendor pipeline does before encoding.
type fibo2Caption struct {
	ShortDescription  string           `json:"short_description"`
	Objects           json.RawMessage  `json:"objects,omitempty"`
	BackgroundSetting string           `json:"background_setting"`
	Lighting          json.RawMessage  `json:"lighting,omitempty"`
	Aesthetics        *fibo2Aesthetics `json:"aesthetics,omitempty"`
	Photographic      json.RawMessage  `json:"photographic_characteristics,omitempty"`
	StyleMedium       string           `json:"style_medium"`
	TextRender        json.RawMessage  `json:"text_render,omitempty"`
	Context           string           `json:"context,omitempty"`
	ArtisticStyle     string           `json:"artistic_style,omitempty"`
}

// fibo2Aesthetics keeps the documented aesthetics keys; the training captions
// dropped aesthetic_score and preference_score, so they never reach sd-server.
type fibo2Aesthetics struct {
	Composition    string `json:"composition,omitempty"`
	ColorScheme    string `json:"color_scheme,omitempty"`
	MoodAtmosphere string `json:"mood_atmosphere,omitempty"`
}

// checkedFibo2Caption parses the enhancer's reply as a VGL caption: the
// required keys present, objects and text_render arrays of non-empty entries,
// and every double-quoted span of the prompt kept word for word in some string
// value. The result is compact JSON, as the vendor serialises captions.
// shortcut: empty values nested inside lighting, camera, objects and text entries are kept, drop them too if Fibo output degrades on them
func checkedFibo2Caption(prompt, content string) (string, error) {
	var c fibo2Caption
	if err := json.Unmarshal([]byte(codeFence.ReplaceAllString(strings.TrimSpace(content), "")), &c); err != nil {
		return "", fmt.Errorf("enhancer reply is not a VGL caption: %w", err)
	}
	var ok bool
	if c.Lighting, ok = vglObject(c.Lighting, true); !ok {
		return "", errors.New("enhancer caption has no lighting object")
	}
	if c.Photographic, ok = vglObject(c.Photographic, false); !ok {
		return "", errors.New("enhancer caption photographic_characteristics is not an object")
	}
	if a := c.Aesthetics; a != nil {
		a.Composition, a.ColorScheme, a.MoodAtmosphere = strings.TrimSpace(a.Composition), strings.TrimSpace(a.ColorScheme), strings.TrimSpace(a.MoodAtmosphere)
	}
	if strings.TrimSpace(c.ShortDescription) == "" || strings.TrimSpace(c.BackgroundSetting) == "" ||
		strings.TrimSpace(c.StyleMedium) == "" || c.Aesthetics == nil || *c.Aesthetics == (fibo2Aesthetics{}) {
		return "", errors.New("enhancer caption is missing a required key")
	}

	var objects []struct {
		Description string `json:"description"`
	}
	if c.Objects, ok = vglEntries(c.Objects, &objects); !ok {
		return "", errors.New("enhancer caption objects is not an array of objects")
	}
	for _, o := range objects {
		if strings.TrimSpace(o.Description) == "" {
			return "", errors.New("enhancer caption has an object with no description")
		}
	}

	var texts []struct {
		Text string `json:"text"`
	}
	if c.TextRender, ok = vglEntries(c.TextRender, &texts); !ok {
		return "", errors.New("enhancer caption text_render is not an array of objects")
	}
	for _, t := range texts {
		if strings.TrimSpace(t.Text) == "" {
			return "", errors.New("enhancer caption has a text_render entry with no text")
		}
	}

	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false) // captions are text for the encoder, not HTML
	if err := enc.Encode(c); err != nil {
		return "", err
	}
	// A quoted span may be a name or style reference rather than text to draw,
	// so any string value of the sent caption may carry it.
	var tree any
	if err := json.Unmarshal(out.Bytes(), &tree); err != nil {
		return "", err
	}
	all := strings.Join(vglStrings(tree, nil), "\n")
	for _, m := range quotedText.FindAllStringSubmatch(prompt, -1) {
		if !strings.Contains(all, m[1]) {
			return "", fmt.Errorf("enhancer caption dropped the quoted text %q", m[1])
		}
	}
	return strings.TrimSpace(out.String()), nil
}

// vglStrings appends every string value in a decoded caption to s.
func vglStrings(v any, s []string) []string {
	switch v := v.(type) {
	case string:
		s = append(s, v)
	case []any:
		for _, e := range v {
			s = vglStrings(e, s)
		}
	case map[string]any:
		for _, e := range v {
			s = vglStrings(e, s)
		}
	}
	return s
}

// vglObject returns raw when it is a non-empty JSON object and nil when it is
// absent, null or empty. ok is false when raw is some other value, or absent,
// null or empty and required.
func vglObject(raw json.RawMessage, required bool) (json.RawMessage, bool) {
	if len(raw) == 0 {
		return nil, !required
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil, false
	}
	if len(m) == 0 {
		return nil, !required
	}
	return raw, true
}

// vglEntries decodes raw, an optional array of objects, into entries. It
// returns raw when the array has entries and nil when it is absent, null or
// empty; ok is false when raw is some other value.
func vglEntries[T any](raw json.RawMessage, entries *[]T) (json.RawMessage, bool) {
	if len(raw) == 0 {
		return nil, true
	}
	if json.Unmarshal(raw, entries) != nil {
		return nil, false
	}
	if len(*entries) == 0 {
		return nil, true
	}
	return raw, true
}
