package router

import (
	"path/filepath"
	"testing"

	"model-router/internal/config"
)

func TestModelFamilyOverFleetCatalogs(t *testing.T) {
	files, _ := filepath.Glob("../../configs/*.yaml")
	stanzas := map[string]*config.Stanza{}
	for _, file := range files {
		cfg, err := config.Load(file)
		if err != nil {
			continue // not every fleet file is a full catalog; the ids below must still be found
		}
		for _, id := range cfg.StanzaIDs() {
			stanzas[id] = cfg.Stanza(id)
		}
	}
	want := map[string]string{ // model id -> family name ("" = none)
		"qwen38-flash-next":  "Qwen",
		"flux3-klein":        "FLUX",
		"ideogram4":          "Ideogram",
		"krea-2-turbo":       "Krea",
		"z-anime-turbo":      "",
		"jina-reranker-v3.5": "",
	}
	for id, name := range want {
		s := stanzas[id]
		if s == nil {
			t.Fatalf("%s is not in any configs/*.yaml catalog", id)
		}
		f := modelFamily(s.Family, id, s.Name)
		got := ""
		if f != nil {
			got = f.Name
		}
		if got != name {
			t.Errorf("%s: family %q, want %q", id, got, name)
		}
	}
	if f := modelFamily("", "flux3-klein", ""); f.Owner != "Black Forest Labs" {
		t.Errorf("flux owner %q", f.Owner)
	}
	if f := modelFamily("", "dolphin-7b", ""); f != nil {
		t.Errorf("dolphin matched %q", f.Name)
	}
}

func TestModelFamilyStanzaOverride(t *testing.T) {
	r := spilloverRouter(t, `
start_port: 5900
stanzas:
  - model_id: qwen-tune-27b
    name: "Qwen tune 27B"
    command: "x --port {port}"
    family: Mistral
  - model_id: house-blend
    command: "x --port {port}"
    family: Example Labs
  - model_id: plain-model
    command: "x --port {port}"
`)
	got := map[string]*Family{}
	for _, ms := range r.AllModelStatuses() {
		got[ms.ID] = ms.Family
	}
	if f := got["qwen-tune-27b"]; f == nil || f.ID != "mistral" || f.Owner != "Mistral AI" {
		t.Errorf("family: did not win over the id match: %+v", f)
	}
	if f := got["house-blend"]; f == nil || f.Name != "Example Labs" {
		t.Errorf("unknown family: value not kept: %+v", f)
	}
	if f := got["plain-model"]; f != nil {
		t.Errorf("plain-model got a family: %+v", f)
	}
}
