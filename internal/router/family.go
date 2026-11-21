package router

import (
	"regexp"
	"strings"
)

// Family is a model's family and maker, shown as an icon in the Model catalog.
// ID names the icon in ui/families.
type Family struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Owner string `json:"owner"`
}

// families is matched in order against the lowercased model id, then the
// name, so a fine-tune (hermes, deepseek) wins over its base model's family.
var families = []struct {
	re *regexp.Regexp
	f  Family
}{
	{regexp.MustCompile(`hermes`), Family{"hermes", "Hermes", "Nous Research"}},
	{regexp.MustCompile(`deepseek`), Family{"deepseek", "DeepSeek", "DeepSeek"}},
	{regexp.MustCompile(`flux`), Family{"flux", "FLUX", "Black Forest Labs"}},
	{regexp.MustCompile(`qwen`), Family{"qwen", "Qwen", "Alibaba"}},
	{regexp.MustCompile(`mistral|mixtral|devstral|magistral|ministral`), Family{"mistral", "Mistral", "Mistral AI"}},
	{regexp.MustCompile(`llama`), Family{"llama", "Llama", "Meta"}},
	{regexp.MustCompile(`gemma`), Family{"gemma", "Gemma", "Google"}},
	{regexp.MustCompile(`\bphi`), Family{"phi", "Phi", "Microsoft"}}, // not "dolphin"
	{regexp.MustCompile(`ideogram`), Family{"ideogram", "Ideogram", "Ideogram"}},
	{regexp.MustCompile(`krea`), Family{"krea", "Krea", "Krea"}},
	{regexp.MustCompile(`fibo`), Family{"fibo", "FIBO", "Bria"}},
	{regexp.MustCompile(`whisper`), Family{"whisper", "Whisper", "OpenAI"}},
}

// modelFamily returns the family for a catalog row, or nil when unknown. A
// stanza's family: names a table id; any other value is shown as its own name.
func modelFamily(override, id, name string) *Family {
	if override != "" {
		for _, e := range families {
			if strings.EqualFold(e.f.ID, override) {
				f := e.f
				return &f
			}
		}
		return &Family{ID: override, Name: override}
	}
	for _, s := range []string{id, name} {
		s = strings.ToLower(s)
		for _, e := range families {
			if e.re.MatchString(s) {
				f := e.f
				return &f
			}
		}
	}
	return nil
}
