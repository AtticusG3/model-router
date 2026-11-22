package router

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// zImageProfile turns a short prompt into the long natural-language prose
// Z-Image is tuned for, and falls back to the prompt verbatim: Z-Image reads
// plain prose, so no template is needed. See
// docs/research/z-image-prompt-style.md.
var zImageProfile = promptProfile{
	system: zImageSystemPrompt,
	user:   canvasRequest,
	check:  checkedZImagePrompt,
}

// zImageSystemPrompt follows the steps of the vendor's prompt enhancer
// template: keep the core request, add concrete visual detail, quote every
// piece of on-image text exactly, no quality tags, prompt only.
const zImageSystemPrompt = `You rewrite a short image request into one detailed image prompt for a text-to-image model that reads natural-language prose.
Return only the prompt, with no heading, no markdown and no commentary.

Rules:
- Keep the subject, count, actions and intent of the request. Never add subjects that were not asked for.
- Write flowing descriptive sentences, not a comma-separated tag list. Aim for 80 to 200 words.
- Describe, in this order: the main subject and what it is doing, the setting and background, the composition and camera framing for the canvas given, the lighting, the colours and materials, and the overall style or medium.
- Be concrete: name colours, materials, light sources and spatial positions instead of vague words like beautiful or stunning.
- Keep any style, medium, camera or mood the request names. If it names none, pick one that suits the subject and say it plainly.
- Text in the image: copy every word that must appear in the image exactly as given, inside English double quotes, and say where it sits and its font, size and colour. Do not add text that was not asked for.
- Do not add quality tags such as 8K, masterpiece or best quality, and do not write a negative prompt.
- Write in the language of the request. Keep quoted text in its original language.
`

// quotedText is a double-quoted span of a prompt: text the image must show.
var quotedText = regexp.MustCompile(`"([^"]+)"`)

// checkedZImagePrompt accepts a non-empty prose reply that keeps every quoted
// span of the prompt word for word, so requested on-image text is not lost.
func checkedZImagePrompt(prompt, content string) (string, error) {
	out := strings.TrimSpace(codeFence.ReplaceAllString(strings.TrimSpace(content), ""))
	if out == "" {
		return "", errors.New("enhancer prompt is empty")
	}
	if isJSONObject(out) {
		return "", errors.New("enhancer prompt is a JSON object, not prose")
	}
	for _, m := range quotedText.FindAllStringSubmatch(prompt, -1) {
		if !strings.Contains(out, m[1]) {
			return "", fmt.Errorf("enhancer prompt dropped the quoted text %q", m[1])
		}
	}
	return out, nil
}
