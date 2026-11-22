package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// The tracked config files in git carry placeholder addresses (#184/#195):
// the LAN /24 is written as 10.99.0.x and tailnet hosts as 203.0.113.x. The
// real values for a node live in an untracked overlay that is merged over the
// tracked file at load time, so deploying the repo copy can never point a
// node at placeholder peers.

// OverlayPath is the conventional overlay for a node: <config dir>/local/<node>.yaml.
// The directory `local/` is gitignored, so an overlay cannot be committed by
// accident. An empty node falls back to "node.yaml" using the config's own
// base name (e.g. /opt/ai/config/model-router.yaml -> local/model-router.yaml).
func OverlayPath(configPath, node string) string {
	if node == "" {
		node = trimYAMLExt(filepath.Base(configPath))
	}
	return filepath.Join(filepath.Dir(configPath), "local", node+".yaml")
}

// LoadWithOverlay loads path, then merges overlayPath over it: maps merge
// recursively and the overlay wins; lists and scalars in the overlay replace
// the base value. A missing overlay is not an error — the tracked file is used
// on its own, which is what a fresh checkout with placeholders does. An
// overlay that exists but does not parse or validate is an error, never a
// silent fallback.
func LoadWithOverlay(path, overlayPath string) (*Config, error) {
	if overlayPath == "" {
		return Load(path)
	}
	base, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	overlay, err := os.ReadFile(overlayPath)
	if err != nil {
		if os.IsNotExist(err) {
			return Parse(base)
		}
		return nil, fmt.Errorf("read overlay %s: %w", overlayPath, err)
	}
	merged, err := mergeYAML(base, overlay)
	if err != nil {
		return nil, fmt.Errorf("overlay %s: %w", overlayPath, err)
	}
	cfg, err := Parse(merged)
	if err != nil {
		return nil, fmt.Errorf("overlay %s: %w", overlayPath, err)
	}
	return cfg, nil
}

// mergeYAML deep-merges two YAML documents: maps merge key by key, everything
// else in the overlay replaces the base value.
func mergeYAML(base, overlay []byte) ([]byte, error) {
	var b, o map[string]any
	if err := yaml.Unmarshal(base, &b); err != nil {
		return nil, fmt.Errorf("parse base: %w", err)
	}
	if err := yaml.Unmarshal(overlay, &o); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	merged := mergeMaps(b, o)
	out, err := yaml.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("marshal merged config: %w", err)
	}
	return out, nil
}

func mergeMaps(base, overlay map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(overlay))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overlay {
		if bv, ok := out[k]; ok {
			if bm, ok := bv.(map[string]any); ok {
				if om, ok := v.(map[string]any); ok {
					out[k] = mergeMaps(bm, om)
					continue
				}
			}
		}
		out[k] = v
	}
	return out
}

func trimYAMLExt(name string) string {
	switch filepath.Ext(name) {
	case ".yaml", ".yml":
		return name[:len(name)-len(filepath.Ext(name))]
	}
	return name
}
