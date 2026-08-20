// Package config loads and validates the model-router per-node configuration.
//
// The stanza schema is defined in SPEC.md. This file extends it with the fields
// the fleet actually needs (device pinning for buster's two GPUs, optional
// pools for legacy client ids, peers, preload).
package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Match describes how an incoming request is resolved to this stanza.
// body_field and path_prefix are mutually exclusive (discriminated like SPEC).
type Match struct {
	// BodyField is the JSON body field holding the model id (OpenAI-style).
	BodyField string `yaml:"body_field"`
	// PathPrefix is a URL path prefix used to match requests with no model in
	// the body (sd.cpp/A1111-style). When several stanzas share a prefix, the
	// one with PathDefault=true is the fallback.
	PathPrefix string `yaml:"path_prefix"`
	// PathDefault marks this stanza as the fallback for its path_prefix when
	// the request body carries no model field.
	PathDefault bool `yaml:"path_default"`
}

// Stanza is one model this node can serve. See SPEC.md.
type Stanza struct {
	ModelID string `yaml:"model_id"`
	Name    string `yaml:"name"`
	// Command is the backend command. {port} / ${PORT} are replaced with the
	// stanza's port; {model} / {model_path} with the stanza's ModelPath.
	Command string `yaml:"command"`
	// ModelPath is substituted into {model} / {model_path} in Command (optional;
	// fleet commands usually embed absolute paths already).
	ModelPath string `yaml:"model_path"`
	// VramMB is the VRAM reservation (worst case) used for admission control.
	VramMB int64 `yaml:"vram_mb"`
	// Device pins the stanza to a GPU (e.g. "CUDA0", "CUDA1", "0"). Empty means
	// "auto": pick the GPU with the most free VRAM at load time.
	Device string `yaml:"device"`
	// SpinUpSeconds is how long to wait for health before declaring failure.
	SpinUpSeconds int `yaml:"spin_up_seconds"`
	// APIType is one of chat | embedding | image | rerank.
	APIType string `yaml:"api_type"`
	Match   Match  `yaml:"match"`
	// HealthCheck is the URL path polled for readiness. Default "/health".
	HealthCheck string `yaml:"health_check"`
	// IdleTTLSeconds marks a running model stale after this many idle seconds.
	// Stale models stay loaded (reload is expensive); they are evicted only when
	// another load needs the VRAM. 0 = never stale (resident).
	IdleTTLSeconds int `yaml:"idle_ttl_seconds"`
	// Unlisted is accepted for llama-swap configs. Listing ignores it: the
	// mesh catalog shows every available model id once.
	Unlisted bool `yaml:"unlisted"`
	// Env are extra "KEY=value" environment variables for the backend process.
	Env []string `yaml:"env"`
	// Port is the fixed port the backend listens on. 0 = auto-allocate from
	// Config.StartPort in config order.
	Port int `yaml:"port"`
	// Proxy overrides the upstream URL; default "http://127.0.0.1:{port}".
	Proxy string `yaml:"proxy"`
	// Aliases are alternative model ids that resolve to this stanza (like
	// llama-swap's aliases). Needed so peer references like "digger/coding-model"
	// resolve to the node's actual coding stanza. Not listed in /v1/models.
	Aliases []string `yaml:"aliases"`
	// Slots is the local concurrency cap (llama.cpp --parallel). 0 in YAML
	// means derive from --parallel / -np in Command; missing flag → 1.
	Slots int `yaml:"slots"`
}

// SlotCount is the in-flight request cap for this stanza. Always >= 1.
func (s *Stanza) SlotCount() int {
	if s == nil || s.Slots <= 0 {
		return 1
	}
	return s.Slots
}

// Pool is a virtual model id resolved to concrete targets per request
// (llama-swap "selector"). Kept so existing clients can still POST
// coding-pool etc.; pools are not listed in /v1/models or the UI.
type Pool struct {
	Name     string   `yaml:"name"`
	Strategy string   `yaml:"strategy"`
	Targets  []string `yaml:"targets"`
	// Spillover is the in-flight cap per target. 0 = use the local target's
	// slots (or 1 for a peer with no local stanza).
	Spillover int `yaml:"spillover"`
}

// Peer is another node (or plain OpenAI-compatible upstream) that can serve
// models we don't have locally.
type Peer struct {
	Name string `yaml:"name"`
	// Kind is "router" (has a /_router control API) or "openai" (proxy-only,
	// e.g. openrouter). Default "router".
	Kind    string   `yaml:"kind"`
	BaseURL string   `yaml:"base_url"`
	Models  []string `yaml:"models"`
	// BodyField is the JSON body field holding the model id on this peer's
	// stanzas. The router rewrites this field to the peer-local model id
	// before proxying. Default "model".
	BodyField string `yaml:"body_field"`
}

// Telemetry tunes the GPU poll and peer sync loops.
type Telemetry struct {
	PollSeconds     int `yaml:"poll_seconds"`
	PeerSyncSeconds int `yaml:"peer_sync_seconds"`
	// PeerStaleSeconds: cached peer telemetry older than this is treated as
	// unknown and the peer is excluded from candidate selection.
	PeerStaleSeconds int `yaml:"peer_stale_seconds"`
}

// Config is the whole per-node configuration file.
type Config struct {
	// Listen is the router's own bind address, e.g. "0.0.0.0:18080".
	Listen string `yaml:"listen"`
	// StartPort is the base for auto-allocated stanza ports.
	StartPort int `yaml:"start_port"`
	// ReservedPorts are ports the router must never auto-allocate (sidecars).
	ReservedPorts []int           `yaml:"reserved_ports"`
	Stanzas       []Stanza        `yaml:"stanzas"`
	Pools         map[string]Pool `yaml:"pools"`
	Peers         []Peer          `yaml:"peers"`
	// Preload is a list of stanza ids to load at startup.
	Preload   []string  `yaml:"preload"`
	Telemetry Telemetry `yaml:"telemetry"`

	// Derived lookups (populated by Load).
	stanzaByID map[string]*Stanza
	aliasByID  map[string]*Stanza
	poolByName map[string]*Pool
	peerByName map[string]*Peer
}

// Defaults applied by Load when fields are unset.
const (
	DefaultHealthCheck = "/health"
	DefaultSpinUp      = 60
	DefaultPoll        = 2
	DefaultPeerSync    = 5
	DefaultPeerStale   = 15
	DefaultStartPort   = 5800
)

var portMacroRe = regexp.MustCompile(`\{port\}|\$\{PORT\}`)

// Load reads, validates, and normalizes a config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	return Parse(raw)
}

// Parse parses config bytes and applies defaults/derivation.
func Parse(raw []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if cfg.Listen == "" {
		cfg.Listen = "0.0.0.0:18080"
	}
	if cfg.StartPort == 0 {
		cfg.StartPort = DefaultStartPort
	}
	if cfg.Telemetry.PollSeconds == 0 {
		cfg.Telemetry.PollSeconds = DefaultPoll
	}
	if cfg.Telemetry.PeerSyncSeconds == 0 {
		cfg.Telemetry.PeerSyncSeconds = DefaultPeerSync
	}
	if cfg.Telemetry.PeerStaleSeconds == 0 {
		cfg.Telemetry.PeerStaleSeconds = DefaultPeerStale
	}

	reserved := map[int]bool{}
	for _, p := range cfg.ReservedPorts {
		reserved[p] = true
	}

	cfg.stanzaByID = map[string]*Stanza{}
	cfg.aliasByID = map[string]*Stanza{}
	nextPort := cfg.StartPort
	for i := range cfg.Stanzas {
		s := &cfg.Stanzas[i]
		if s.ModelID == "" {
			return nil, fmt.Errorf("stanza %d: model_id is required", i)
		}
		if s.Match.BodyField != "" && s.Match.PathPrefix != "" {
			return nil, fmt.Errorf("stanza %q: match.body_field and match.path_prefix are mutually exclusive", s.ModelID)
		}
		if _, dup := cfg.stanzaByID[s.ModelID]; dup {
			return nil, fmt.Errorf("duplicate model_id %q", s.ModelID)
		}
		for _, a := range s.Aliases {
			if _, dup := cfg.stanzaByID[a]; dup {
				return nil, fmt.Errorf("alias %q collides with a stanza id", a)
			}
			if _, dup := cfg.aliasByID[a]; dup {
				return nil, fmt.Errorf("duplicate alias %q", a)
			}
			cfg.aliasByID[a] = s
		}
		if s.Command == "" {
			return nil, fmt.Errorf("stanza %q: command is required", s.ModelID)
		}
		if s.APIType == "" {
			s.APIType = "chat"
		}
		if s.HealthCheck == "" {
			s.HealthCheck = DefaultHealthCheck
		}
		if s.SpinUpSeconds == 0 {
			s.SpinUpSeconds = DefaultSpinUp
		}
		// Allocate a port.
		if s.Port == 0 {
			for reserved[nextPort] {
				nextPort++
			}
			s.Port = nextPort
			nextPort++
		}
		if s.Proxy == "" {
			s.Proxy = fmt.Sprintf("http://127.0.0.1:%d", s.Port)
		}
		s.Command = portMacroRe.ReplaceAllString(s.Command, fmt.Sprintf("%d", s.Port))
		if s.ModelPath != "" {
			s.Command = strings.ReplaceAll(s.Command, "{model_path}", s.ModelPath)
			s.Command = strings.ReplaceAll(s.Command, "{model}", s.ModelPath)
		}
		if s.Slots <= 0 {
			s.Slots = parallelFromCommand(s.Command)
		}
		cfg.stanzaByID[s.ModelID] = s
	}

	cfg.poolByName = map[string]*Pool{}
	for name, p := range cfg.Pools {
		if p.Strategy == "" {
			p.Strategy = "spillover"
		}
		// Spillover 0 means "use the target stanza's slots" at pick time.
		if _, dup := cfg.stanzaByID[name]; dup {
			return nil, fmt.Errorf("pool %q collides with a stanza id", name)
		}
		cp := p
		cp.Name = name
		cfg.poolByName[name] = &cp
		cfg.Pools[name] = cp
	}

	cfg.peerByName = map[string]*Peer{}
	for i := range cfg.Peers {
		p := &cfg.Peers[i]
		if p.Kind == "" {
			p.Kind = "router"
		}
		if p.BodyField == "" {
			p.BodyField = "model"
		}
		if p.BaseURL == "" {
			return nil, fmt.Errorf("peer %q: base_url is required", p.Name)
		}
		p.BaseURL = strings.TrimRight(p.BaseURL, "/")
		if _, dup := cfg.peerByName[p.Name]; dup {
			return nil, fmt.Errorf("duplicate peer %q", p.Name)
		}
		cfg.peerByName[p.Name] = p
	}

	if err := cfg.validateCatalog(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) validateCatalog() error {
	pathDefault := map[string]string{}
	for i := range c.Stanzas {
		s := &c.Stanzas[i]
		if s.Match.PathDefault && s.Match.PathPrefix != "" {
			if prev, ok := pathDefault[s.Match.PathPrefix]; ok {
				return fmt.Errorf("duplicate path_default for prefix %q (stanzas %q and %q)", s.Match.PathPrefix, prev, s.ModelID)
			}
			pathDefault[s.Match.PathPrefix] = s.ModelID
		}
		for _, a := range s.Aliases {
			if _, ok := c.poolByName[a]; ok {
				return fmt.Errorf("alias %q collides with a pool", a)
			}
		}
	}
	for i := range c.Peers {
		p := &c.Peers[i]
		if p.Name == "" {
			return fmt.Errorf("peer %d: name is required", i)
		}
		switch p.Kind {
		case "router", "openai":
		default:
			return fmt.Errorf("peer %q: kind %q is not router or openai", p.Name, p.Kind)
		}
	}
	for name, p := range c.poolByName {
		for _, t := range p.Targets {
			if !c.poolTargetResolves(t) {
				return fmt.Errorf("pool %q: unknown target %q", name, t)
			}
		}
	}
	for _, id := range c.Preload {
		if c.stanzaByID[id] == nil {
			return fmt.Errorf("preload: unknown model %q", id)
		}
	}
	return nil
}

// poolTargetResolves reports whether a pool target is a local stanza, an alias,
// or a peer-qualified id whose peer exists and advertises the model.
func (c *Config) poolTargetResolves(raw string) bool {
	if raw == "" {
		return false
	}
	peerName, model, ok := strings.Cut(raw, "/")
	if ok {
		p := c.peerByName[peerName]
		if p == nil || model == "" {
			return false
		}
		for _, m := range p.Models {
			if m == model {
				return true
			}
		}
		return false
	}
	return c.stanzaByID[raw] != nil || c.aliasByID[raw] != nil
}

// Stanza returns the local stanza with the given id, or nil.
func (c *Config) Stanza(id string) *Stanza { return c.stanzaByID[id] }

// Alias returns the stanza an alias resolves to, or nil.
func (c *Config) Alias(id string) *Stanza { return c.aliasByID[id] }

// Pool returns the pool with the given name, or nil.
func (c *Config) Pool(name string) *Pool { return c.poolByName[name] }

// Peer returns the peer with the given name, or nil.
func (c *Config) Peer(name string) *Peer { return c.peerByName[name] }

// PeerAdvertises reports whether any peer lists id in its models catalog.
func (c *Config) PeerAdvertises(id string) bool {
	if id == "" {
		return false
	}
	for i := range c.Peers {
		for _, m := range c.Peers[i].Models {
			if m == id {
				return true
			}
		}
	}
	return false
}

// StanzaIDs returns all local stanza ids in config order.
func (c *Config) StanzaIDs() []string {
	out := make([]string, 0, len(c.Stanzas))
	for _, s := range c.Stanzas {
		out = append(out, s.ModelID)
	}
	return out
}

// BodyFields returns the distinct JSON body field names the matcher should
// read, in config order. A stanza's field is its match.body_field; stanzas with
// neither body_field nor path_prefix default to "model" (OpenAI-style). "model"
// is always included so plain OpenAI request bodies keep resolving.
func (c *Config) BodyFields() []string {
	seen := map[string]bool{}
	var out []string
	add := func(f string) {
		if f == "" || seen[f] {
			return
		}
		seen[f] = true
		out = append(out, f)
	}
	for i := range c.Stanzas {
		s := &c.Stanzas[i]
		switch {
		case s.Match.BodyField != "":
			add(s.Match.BodyField)
		case s.Match.PathPrefix == "":
			add("model")
		}
	}
	add("model")
	return out
}

// parallelFromCommand reads llama.cpp --parallel / -np from a backend
// command. Last occurrence wins. Missing or unparsable → 1.
func parallelFromCommand(cmd string) int {
	fields := strings.Fields(cmd)
	n := 0
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		var rest string
		switch {
		case f == "--parallel" || f == "-np":
			if i+1 >= len(fields) {
				continue
			}
			rest = fields[i+1]
			i++
		case strings.HasPrefix(f, "--parallel="):
			rest = strings.TrimPrefix(f, "--parallel=")
		case strings.HasPrefix(f, "-np="):
			rest = strings.TrimPrefix(f, "-np=")
		default:
			continue
		}
		var v int
		if _, err := fmt.Sscanf(rest, "%d", &v); err == nil && v > 0 {
			n = v
		}
	}
	if n <= 0 {
		return 1
	}
	return n
}
