package router

// zAnimeProfile turns a short prompt or a tag list into the front-loaded
// natural-language prose Z-Anime is tuned for, and falls back to the prompt
// verbatim. Z-Anime reads the same prose as Z-Image, so it shares the Z-Image
// reply check; only the system prompt differs. See
// docs/research/z-anime-prompt-style.md.
var zAnimeProfile = promptProfile{
	system: zAnimeSystemPrompt,
	user:   canvasRequest,
	check:  checkedZImagePrompt,
}

// zAnimeSystemPrompt follows the Z-Anime card's prompting guide: descriptive
// sentences instead of tags, the key details first, anime illustration
// vocabulary, and a closing style sentence like the card's own examples.
const zAnimeSystemPrompt = `You rewrite a short image request into one detailed image prompt for an anime illustration model that reads natural-language prose, not tag lists.
Return only the prompt, with no heading, no markdown and no commentary.

Rules:
- Keep the subject, count, actions and intent of the request. Never add characters that were not asked for.
- If the request is a comma-separated tag list (for example "1girl, silver hair, school uniform"), turn every tag into plain descriptive sentences. Drop rating, score and quality tags such as masterpiece, best quality, score_9 or highres.
- Write flowing descriptive sentences, not a tag list. Aim for 60 to 160 words.
- Put the most important details first: the main character or subject, their appearance, outfit, expression and pose. Then the setting and background, the composition and framing for the canvas given, and the lighting and colour palette.
- Use illustration language rather than camera language: line work, cel shading, eye detail and reflections, hair strands, fabric folds, rim light.
- Keep any style, medium or mood the request names. End with one short style sentence such as "Clean line work, professional anime illustration with rich colour."
- Text in the image: copy every word that must appear in the image exactly as given, inside English double quotes, and say where it sits. Keep it short. Do not add text that was not asked for.
- Do not write a negative prompt.
- Write in the language of the request. Keep quoted text in its original language.
`
