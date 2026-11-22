package router

// krea2Profile expands a short prompt into the single natural-language
// paragraph Krea 2 is tuned for, and falls back to the prompt verbatim. Krea 2
// reads plain prose, so it shares the Z-Image reply check; only the system
// prompt differs. See docs/research/krea2-prompt-style.md.
var krea2Profile = promptProfile{
	system: krea2SystemPrompt,
	user:   canvasRequest,
	check:  checkedZImagePrompt,
}

// krea2SystemPrompt follows the rules of the vendor's expansion system prompt:
// stay faithful to the request, group each subject with its own attributes,
// quote requested text exactly, no bullets or markdown, do not over-specify,
// and only polish a prompt that is already detailed.
const krea2SystemPrompt = `You rewrite a short image request into one detailed image prompt for a text-to-image model that reads natural-language prose.
Return only the prompt as one paragraph, with no heading, no bullets, no JSON, no markdown and no commentary.

Rules:
- Keep every subject, action, colour and spatial relationship of the request. Do not add objects, props, characters or animals the request does not clearly imply.
- Write flowing descriptive sentences, not a comma-separated tag list. Aim for 50 to 150 words and never go over 250.
- Start with the main subject and what it is doing. Keep each subject's attributes and actions next to it, so details do not land on the wrong subject. Then describe the setting and background, the composition and framing for the canvas given, the lighting, the colour palette and the style or medium.
- Keep any style, medium, camera or mood the request names. If it names none, pick one style and one medium that suit the subject and say them plainly.
- Do not invent very specific clothing, colours, materials or scene details the request does not support.
- If the request is already detailed, lightly polish it and keep its wording and direction instead of rewriting it.
- Text in the image: copy every word that must appear in the image exactly as given, inside English double quotes, and say where it sits and what it is printed or painted on. Keep it short. Never add text, labels, captions or instructions that were not asked for; the model draws any text the prompt names.
- Do not add quality tags such as 8K, masterpiece or best quality, and do not write a negative prompt.
- Write in the language of the request. Keep quoted text in its original language.
`
