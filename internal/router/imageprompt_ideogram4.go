package router

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ideogram4Profile turns a plain prompt into an ideogram4 JSON caption, and
// falls back to the template caption holding the prompt verbatim.
var ideogram4Profile = promptProfile{
	system:   captionSystemPrompt,
	user:     canvasRequest,
	check:    checkedCaption,
	fallback: templateCaption,
}

// captionSystemPrompt is the sd-server shim's CAPTION_SYSTEM_PROMPT, word for
// word, so both paths ask the enhancer the same question.
const captionSystemPrompt = `You turn an image request into a structured caption as one JSON object.
Return only the JSON object, with no markdown fence and no commentary.

Schema:
{
  "high_level_description": "one sentence describing the whole image",
  "compositional_deconstruction": {
    "background": "what fills the frame behind the subjects",
    "elements": [
      {"type": "obj", "bbox": [x_min, y_min, x_max, y_max], "desc": "detailed neutral description"}
    ]
  },
  "style_description": {
    "aesthetics": "...",
    "lighting": "...",
    "photo": "...",
    "medium": "..."
  }
}

Rules:
- bbox values are integers on a 0-1000 grid measured from the top-left, x_min < x_max and y_min < y_max.
- x runs left to right across the canvas width; y runs top to bottom down the canvas height.
- Place elements so they fit the canvas given in the request.
- Keep the subject, count, actions and intent of the request. Never add subjects that were not asked for.
- Give every element a type, a bbox and a detailed neutral desc.
- Include style_description only when the request asks for a style, medium, camera, lighting or aesthetic. Otherwise omit it.
`

// styleWords marks a prompt that asks for a style; only then is the caption's
// style_description kept. Same list as the shim.
var styleWords = regexp.MustCompile(`(?i)\b(photo|photograph|photoreal\w*|realistic|cinematic|anime|illustration|` +
	`painting|watercolor|oil\s+paint\w*|sketch|drawing|render|3d|cgi|lighting|` +
	`camera|lens|film|medium|style|aesthetic|pastel|pixel\s+art|graphic|` +
	`cartoon|comic)\b`)

// ideoCaption is an ideogram4 caption. Field order is the spec's key order;
// keys the spec does not name are dropped.
type ideoCaption struct {
	HighLevel   string `json:"high_level_description"`
	Composition struct {
		Background json.RawMessage   `json:"background"`
		Elements   []json.RawMessage `json:"elements"`
	} `json:"compositional_deconstruction"`
	Style json.RawMessage `json:"style_description,omitempty"`
}

// ideoElement is one caption element in the spec's key order. text is only
// kept on type text elements.
type ideoElement struct {
	Type         *string         `json:"type"`
	BBox         []float64       `json:"bbox"`
	Text         json.RawMessage `json:"text,omitempty"`
	Desc         *string         `json:"desc"`
	ColorPalette json.RawMessage `json:"color_palette,omitempty"`
}

// valid is the shim's _valid_element: a type, a non-blank desc and an x-first
// bbox inside the 0-1000 grid with min < max.
func (e *ideoElement) valid() bool {
	b := e.BBox
	return e.Type != nil && e.Desc != nil && strings.TrimSpace(*e.Desc) != "" && len(b) == 4 &&
		0 <= b[0] && b[0] < b[2] && b[2] <= 1000 && 0 <= b[1] && b[1] < b[3] && b[3] <= 1000
}

// checkedCaption parses the enhancer's reply as a caption and applies the
// shim's rules: at least one valid element, bboxes turned y-first, element
// keys in spec order, style_description only when the prompt asks for a style.
func checkedCaption(prompt, content string) (string, error) {
	var c ideoCaption
	if err := json.Unmarshal([]byte(codeFence.ReplaceAllString(strings.TrimSpace(content), "")), &c); err != nil {
		return "", fmt.Errorf("enhancer reply is not a caption: %w", err)
	}
	var elements []json.RawMessage
	for _, raw := range c.Composition.Elements {
		var e ideoElement
		if json.Unmarshal(raw, &e) != nil || !e.valid() {
			continue
		}
		// The model answers x-first; ideogram4 reads bboxes y-first.
		e.BBox = []float64{e.BBox[1], e.BBox[0], e.BBox[3], e.BBox[2]}
		if *e.Type != "text" {
			e.Text = nil
		}
		out, err := json.Marshal(e)
		if err != nil {
			return "", err
		}
		elements = append(elements, out)
	}
	if strings.TrimSpace(c.HighLevel) == "" || !nonEmptyBackground(c.Composition.Background) || len(elements) == 0 {
		return "", errors.New("enhancer caption is invalid")
	}
	c.Composition.Elements = elements
	if !styleWords.MatchString(prompt) {
		c.Style = nil
	}
	out, err := json.Marshal(c)
	return string(out), err
}

// templateCaption is the fallback caption: the prompt verbatim as the
// description and as one centred element.
func templateCaption(prompt string) string {
	kind, desc := "obj", prompt
	el, _ := json.Marshal(ideoElement{Type: &kind, BBox: []float64{200, 200, 800, 800}, Desc: &desc})
	var c ideoCaption
	c.HighLevel = prompt
	c.Composition.Background = json.RawMessage(`"a plain, evenly lit background"`)
	c.Composition.Elements = []json.RawMessage{el}
	out, _ := json.Marshal(c)
	return string(out)
}

// nonEmptyBackground reports whether raw is a non-empty string or object.
func nonEmptyBackground(raw json.RawMessage) bool {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s != ""
	}
	var m map[string]json.RawMessage
	return json.Unmarshal(raw, &m) == nil && len(m) > 0
}
