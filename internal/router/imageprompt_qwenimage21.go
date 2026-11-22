package router

import (
	"fmt"
	"regexp"
	"strings"
)

// qwenImage21Profile expands a short prompt into the long English description
// Qwen-Image 2.1's official prompt rewriter produces, and falls back to the
// prompt verbatim. The model reads plain prose, so its reply check builds on
// the Z-Image one. See docs/research/qwen-image-2.1-prompt-style.md.
var qwenImage21Profile = promptProfile{
	system: qwenImage21SystemPrompt,
	user:   canvasRequest,
	check:  checkedQwenImage21Prompt,
}

// cjkQuotedText is on-image text in curly or corner-bracket quotes, the way
// Chinese and Japanese requests usually mark it.
var cjkQuotedText = regexp.MustCompile(`“([^”]+)”|「([^」]+)」`)

// The vendor's transparency trigger. A request may use the first sentence
// alone or both.
const (
	rgbaTriggerFirst = "This is an RGBA image with transparency."
	rgbaTrigger      = rgbaTriggerFirst + " The image has alpha channel and the background is transparent."
)

// checkedQwenImage21Prompt is the Z-Image check plus two rules: every
// CJK-quoted span of the prompt stays word for word, and an RGBA trigger the
// prompt opens with still opens the reply.
func checkedQwenImage21Prompt(prompt, content string) (string, error) {
	out, err := checkedZImagePrompt(prompt, content)
	if err != nil {
		return "", err
	}
	for _, m := range cjkQuotedText.FindAllStringSubmatch(prompt, -1) {
		if text := m[1] + m[2]; !strings.Contains(out, text) {
			return "", fmt.Errorf("enhancer prompt dropped the quoted text %q", text)
		}
	}
	prompt = strings.TrimSpace(prompt)
	for _, t := range []string{rgbaTrigger, rgbaTriggerFirst} {
		if strings.HasPrefix(prompt, t) {
			if !strings.HasPrefix(out, t) {
				return "", fmt.Errorf("enhancer prompt does not open with %q", t)
			}
			break
		}
	}
	return out, nil
}

// qwenImage21SystemPrompt follows the vendor's text-to-image rewriter rules:
// one long observational English paragraph opening with a fixed sentence,
// legible text in reading order and in its own script, no quality boosters.
// The vendor's answer is JSON with an aspect ratio; here the canvas is fixed by
// the request, so the enhancer returns the prose alone.
const qwenImage21SystemPrompt = `You rewrite a short image request into one long image prompt for a text-to-image model. The prompt describes the finished image as if you were looking at it.
Return only the prompt as one paragraph of plain prose, with no heading, no bullets, no JSON, no markdown and no commentary.

Rules:
- Write in English whatever the language of the request. Text that appears inside the image stays in its original language and script.
- Observe, do not instruct: present tense, third person, declarative sentences. Never write "you", "create", "make sure" or "the image should".
- Open with: The image is a <vertical, wide or square, matching the canvas given> <style> <photograph, poster, illustration, scene, portrait, infographic, close-up, graphic, page, card or logo> of <subject>, <the background and its palette>.
- Then describe the subjects and what they are doing, their positions, materials and colours, the lighting and the camera or rendering style. If the frame is divided into regions, such as a poster, page, interface or busy wide scene, walk the regions in order.
- Keep every subject, count, action, colour and spatial relationship of the request. Do not add subjects the request does not imply. Keep any style, medium or mood the request names.
- Keep it physically consistent: shadows fall away from the light, reflections match what is in front of the surface, and neighbouring objects are in scale.
- Text in the image: list every piece of legible text in reading order. Copy each word exactly as given, character for character, inside English double quotes, and say where it sits and its typeface, size and colour. Never add text, labels or captions that were not asked for.
- If the request starts with "This is an RGBA image with transparency.", start your reply with that sentence word for word. If the request follows it with "The image has alpha channel and the background is transparent.", copy that sentence word for word next. Only then open the description as above.
- Never write an aspect ratio, resolution or pixel count. Do not add quality boosters such as masterpiece, 8K, highly detailed or award-winning, and do not write a negative prompt.
- Aim for about twenty sentences, roughly 300 to 500 words. A short request still gets a full description.
`
