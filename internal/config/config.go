// Package config loads and validates the model-router per-node configuration.
//
// The file shape is the schema in SPEC.md: stanzas, pools, peers, preload,
// and telemetry.
package config

import (
	"fmt"
	"net/netip"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

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

// Filters rewrite a JSON request body before the backend (or peer) sees it,
// as in llama-swap. The model id field can never be stripped or set.
type Filters struct {
	// StripParams are top-level JSON keys (or form fields) removed from the request.
	StripParams []string `yaml:"stripParams"`
	// SetParams are keys set on the request, overwriting the client's value.
	// A key ending in "?" is set only when the client omitted it.
	SetParams map[string]any `yaml:"setParams"`
	// SetParamsByID mints aliases (stanzas only). Each key is an alias id
	// ("${MODEL_ID}" is the stanza id); its value is applied after SetParams
	// when a request names that alias.
	SetParamsByID map[string]map[string]any `yaml:"setParamsByID"`
	// RenameParams maps a client field name to the name the backend takes
	// (JSON keys, form fields, multipart parts including files). Renames run
	// before strip and set, so those use the backend's names.
	RenameParams map[string]string `yaml:"renameParams"`
}

// validate rejects empty keys and any attempt to touch a protected field.
func (f *Filters) validate(owner string, protected ...string) error {
	check := func(key string) error {
		bare := strings.TrimSuffix(key, "?")
		if bare == "" {
			return fmt.Errorf("%s: filters: empty parameter name", owner)
		}
		for _, p := range protected {
			if bare == p {
				return fmt.Errorf("%s: filters cannot strip or set %q", owner, p)
			}
		}
		return nil
	}
	for _, k := range f.StripParams {
		if err := check(k); err != nil {
			return err
		}
	}
	for k := range f.SetParams {
		if err := check(k); err != nil {
			return err
		}
	}
	for _, params := range f.SetParamsByID {
		for k := range params {
			if err := check(k); err != nil {
				return err
			}
		}
	}
	for from, to := range f.RenameParams {
		if err := check(from); err != nil {
			return err
		}
		if err := check(to); err != nil {
			return err
		}
	}
	return nil
}

// The api_type vocabulary a stanza may declare. An empty api_type is chat.
// The list is shared by config validation, /v1/models, the operator UI and the
// model-less route default, so a typo fails at load instead of silently
// hiding the model from the Playground.
const (
	APITypeChat          = "chat"
	APITypeEmbedding     = "embedding"
	APITypeRerank        = "rerank"
	APITypeImage         = "image"
	APITypeSpeech        = "speech"
	APITypeTranscription = "transcription"
	APITypeRealtime      = "realtime"
	// APITypeVideo is the async video job api_type.
	APITypeVideo    = "video"
	APITypeAudioGen = "audio_gen"

	// JobAdapterOpenAI proxies the /v1/videos job API unchanged. JobAdapterSDCpp
	// translates it onto sd-server's /sdcpp/v1 job API.
	JobAdapterOpenAI = "openai"
	JobAdapterSDCpp  = "sdcpp"

	// PeerSpillAuto spills a blocked or busy local target to a peer at once.
	// PeerSpillOnFailure keeps the request local until the model fails to
	// start hard or the admit wait runs out. PeerSpillNever is on_failure
	// except that a hard start failure is returned instead of spilled.
	PeerSpillAuto      = "auto"
	PeerSpillOnFailure = "on_failure"
	PeerSpillNever     = "never"
)

// PeerSpills is every value a stanza's peer_spill may take.
var PeerSpills = []string{PeerSpillAuto, PeerSpillOnFailure, PeerSpillNever}

// JobAdapters is every value a stanza's job.adapter may take.
var JobAdapters = []string{JobAdapterOpenAI, JobAdapterSDCpp}

// The field shape a speech backend takes a cloned reference in. ref_audio is
// vLLM-Omni/OpenAI (data URI plus ref_text); speaker_ref_b64 is llama-server
// (raw base64 plus lang).
const (
	CloneModeRefAudio      = "ref_audio"
	CloneModeSpeakerRefB64 = "speaker_ref_b64"
)

// CloneModes is every value a stanza's clone_mode may take.
var CloneModes = []string{CloneModeRefAudio, CloneModeSpeakerRefB64}

// Job selects the adapter in front of a stanza's own async job API. Empty
// adapter means openai (the backend already speaks /v1/videos).
type Job struct {
	Adapter string `yaml:"adapter"`
}

// APITypes is every value a stanza's api_type may take, in the order errors
// list them.
var APITypes = []string{
	APITypeChat, APITypeEmbedding, APITypeRerank, APITypeImage,
	APITypeSpeech, APITypeTranscription, APITypeRealtime, APITypeVideo, APITypeAudioGen,
}

// Capabilities are the optional capability flags a stanza may declare. The
// video flags describe the optional video playground fields the model accepts.
var Capabilities = []string{"vision", "audio_in", "audio_out", "tools", "reasoning", "duration", "resolution", "seed"}

// ImagePipeline is a stanza's image request pipeline. The YAML value is either
// a preset name (sd-server or ideogram4, see SPEC.md) or a map; a preset is
// shorthand for the map it names. Only api_type: image may set it.
type ImagePipeline struct {
	// Caption is the caption format a prompt is enhanced into, or "" for no
	// caption step.
	Caption string `yaml:"caption"`
	// Enhancer names the model and timeout used by a caption step.
	Enhancer *ImageEnhancer `yaml:"enhancer"`
	// RetryBlocked is how many times a blocked response may be retried, 0 for
	// no retry.
	RetryBlocked int `yaml:"retry_blocked"`

	// preset is the bare-string form, expanded by Parse so an unknown name is
	// reported with the stanza id.
	preset string
}

// ImageEnhancer is the model that turns a prompt into a caption.
type ImageEnhancer struct {
	Model          string `yaml:"model"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
}

// imagePipelinePresets are the names image_pipeline accepts as a bare string.
// sd-server runs the standard image checks; ideogram4 adds ideogram4-format
// caption enhancement.
var imagePipelinePresets = []string{"sd-server", "ideogram4"}

// imageCaptionFormats are the names image_pipeline.caption accepts, besides
// the empty string (no caption step).
var imageCaptionFormats = []string{"ideogram4"}

// UnmarshalYAML accepts a preset name or a map.
func (p *ImagePipeline) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode && value.Tag != "!!null" {
		return value.Decode(&p.preset)
	}
	type plain ImagePipeline
	return value.Decode((*plain)(p))
}

// expand replaces a preset with the fields it stands for.
func (p *ImagePipeline) expand() error {
	if p.preset == "" {
		return nil
	}
	switch p.preset {
	case "sd-server":
	case "ideogram4":
		if p.Caption == "" {
			p.Caption = "ideogram4"
		}
	default:
		return fmt.Errorf("unknown image_pipeline preset %q; must be one of %s",
			p.preset, strings.Join(imagePipelinePresets, ", "))
	}
	p.preset = ""
	return nil
}

// Stanza is one model this node can serve. See SPEC.md.
type Stanza struct {
	ModelID string `yaml:"model_id"`
	Name    string `yaml:"name"`
	// Family overrides the model family the catalog guesses from the id and
	// name (a family id such as qwen, mistral or flux). UI metadata only.
	Family string `yaml:"family"`
	// Params and Owner are the model's parameter count (e.g. "27B") and maker,
	// shown in the catalog. They win over catalog.metadata_url. UI metadata only.
	Params string `yaml:"params"`
	Owner  string `yaml:"owner"`
	// Command is the backend command. {port} / ${PORT} are replaced with the
	// stanza's port; {model} / {model_path} with the stanza's ModelPath.
	// {gpu} is left in place and filled with the GPU index chosen at spawn
	// time (see Render).
	Command string `yaml:"command"`
	// ModelPath is substituted into {model} / {model_path} in Command (optional;
	// fleet commands usually embed absolute paths already).
	ModelPath string `yaml:"model_path"`
	// VramMB is the VRAM reservation (worst case) used for admission control.
	VramMB int64 `yaml:"vram_mb"`
	// Device pins the stanza to a GPU (e.g. "CUDA0", "CUDA1", "0"). A comma
	// list ("0,1,2") reserves every listed GPU and splits vram_mb by free
	// VRAM, so a resident on one card keeps its bytes and emptier cards take
	// a larger share. A pipe list ("1|2") places the model on any one of the
	// listed GPUs, chosen at load time; Command or Env must then use {gpu}.
	// Empty means "auto": any one GPU on the node.
	Device string `yaml:"device"`
	// MaxInstances lets the router run up to this many copies of the model,
	// each on its own port and placed like a fresh load. Extra copies only
	// start when every running copy has its slots busy and a GPU fits the
	// copy without evicting anything. 0 or 1 = a single instance.
	MaxInstances int `yaml:"max_instances"`
	// SpinUpSeconds is how long to wait for health before declaring failure.
	SpinUpSeconds int `yaml:"spin_up_seconds"`
	// APIType is one of APITypes. Empty means chat. It is UI metadata, and it
	// gives a model-less request its default route (see SPEC.md); a request
	// that names a model, or an explicit match.path_prefix, still wins.
	APIType string `yaml:"api_type"`
	// Capabilities lists what the backend supports (vision, audio_in,
	// audio_out, tools, reasoning, and for video models duration, resolution,
	// seed). Published in /v1/models and /_router/status. A vision chat model
	// (started with an --mmproj) is still api_type: chat plus
	// capabilities: [vision].
	Capabilities []string `yaml:"capabilities"`
	Match        Match    `yaml:"match"`
	// HealthCheck is the URL path polled for readiness. Default "/health".
	HealthCheck string `yaml:"health_check"`
	// IdleTTLSeconds unloads a running instance after this many idle seconds
	// (see Router.SweepStale). 0 = resident: the sweep never touches it, but
	// a load that needs the VRAM still can, if it is not mid-request.
	IdleTTLSeconds int `yaml:"idle_ttl_seconds"`
	// HoldSeconds keeps a model resident for this long after it becomes healthy,
	// even if another request needs its VRAM. A cold model with a long warm-up
	// (e.g. a one-off calibration) would otherwise be evicted the instant it
	// reports healthy but before a client can reach it. 0 = no hold.
	HoldSeconds int `yaml:"hold_seconds"`
	// Pin keeps the stanza resident no matter what: it is never evicted, not
	// even as a last resort when another load needs its VRAM. A load that
	// cannot fit without disturbing it fails instead. Use for a model a
	// service depends on continuously (e.g. a review worker's backend).
	Pin bool `yaml:"pin"`
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
	// Filters rewrite the request body before the backend sees it. Its
	// SetParamsByID mints extra ids that resolve to this stanza.
	Filters Filters `yaml:"filters"`
	// ImagePipeline runs the image request pipeline for this stanza: request
	// rewriting, one response retry and response headers. Only api_type: image
	// may set it. See SPEC.md.
	ImagePipeline *ImagePipeline `yaml:"image_pipeline"`
	// Job selects the adapter in front of this stanza's async job API. Only
	// api_type: video may set it; openai (the default) proxies /v1/videos
	// unchanged, sdcpp translates it onto sd-server's /sdcpp/v1 job API.
	Job *Job `yaml:"job"`
	// MaxRequestBodyMB overrides the top-level max_request_body_mb for this
	// stanza. 0 = inherit.
	MaxRequestBodyMB int `yaml:"max_request_body_mb"`
	// PeerSpill controls whether a request for this local model may walk to a
	// peer when it cannot run here. auto (default) does so as soon as local
	// room is blocked or every slot is busy. on_failure waits for local
	// admission and the load, then walks only after a hard start failure or
	// an admit timeout. never is on_failure except that a hard start failure
	// is answered locally instead of walked.
	PeerSpill string `yaml:"peer_spill"`
	// Share advertises this model (and its filter-set ids) to peers. nil when
	// the key is absent (default true); false keeps it off the mesh and makes
	// this node refuse peer-proxied requests for it.
	Share *bool `yaml:"share"`
	// PathRewrite swaps a request path prefix for the backend's own before
	// the request is proxied to a local instance; the first matching from
	// wins. For a backend that serves an OpenAI route under another path.
	PathRewrite []PathRewrite `yaml:"path_rewrite"`
	// RequestTimeoutSeconds cuts one proxied request to a local instance off
	// after this many seconds (504). 0 = no limit: the router itself never
	// times out a request in flight, however long the audio.
	RequestTimeoutSeconds int `yaml:"request_timeout_seconds"`
	// CloneMode is the field shape a speech backend takes a library voice's
	// reference in: ref_audio (data URI plus ref_text) or speaker_ref_b64
	// (raw base64 plus lang). Empty means ref_audio.
	CloneMode string `yaml:"clone_mode"`

	// InstancePorts holds the backend port of each instance; [0] is Port.
	// Derived by Parse from MaxInstances.
	InstancePorts []int `yaml:"-"`
	// rawCommand is Command before {port} substitution, so extra instances
	// can be rendered on their own ports.
	rawCommand string
}

// PathRewrite is one path_rewrite entry: a path that starts with From is sent
// to the backend with From replaced by To.
type PathRewrite struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
}

// BackendPath is path as the backend serves it, after path_rewrite.
func (s *Stanza) BackendPath(path string) string {
	for _, rw := range s.PathRewrite {
		if rest, ok := strings.CutPrefix(path, rw.From); ok {
			return rw.To + rest
		}
	}
	return path
}

// SlotCount is the in-flight request cap for this stanza. Always >= 1.
func (s *Stanza) SlotCount() int {
	if s == nil || s.Slots <= 0 {
		return 1
	}
	return s.Slots
}

// Shared reports whether peers may see and request this model.
func (s *Stanza) Shared() bool { return s.Share == nil || *s.Share }

// CPUOnly reports whether the stanza runs with no GPU: `device: cpu` (a CPU
// speech or transcription backend, usually with vram_mb: 0). It never takes a
// GPU reservation and is never evicted to free VRAM it does not hold.
func (s *Stanza) CPUOnly() bool {
	return strings.EqualFold(strings.TrimSpace(s.Device), "cpu")
}

// LaunchEqual reports whether a backend started from s is the same process a
// backend started from o would be: same command, env and ports. Routing, TTL,
// hold, slots, filters and the rest are not part of the launch.
func (s *Stanza) LaunchEqual(o *Stanza) bool {
	return s.Command == o.Command && s.rawCommand == o.rawCommand && s.Port == o.Port &&
		slices.Equal(s.Env, o.Env) && slices.Equal(s.InstancePorts, o.InstancePorts)
}

// FilterIDs are the ids minted by Filters.SetParamsByID, sorted.
func (s *Stanza) FilterIDs() []string {
	ids := make([]string, 0, len(s.Filters.SetParamsByID))
	for id := range s.Filters.SetParamsByID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// InstanceCount is the maximum number of concurrent copies. Always >= 1.
func (s *Stanza) InstanceCount() int {
	if s == nil || s.MaxInstances <= 1 {
		return 1
	}
	return s.MaxInstances
}

// InstancePort is the backend port of instance n.
func (s *Stanza) InstancePort(n int) int {
	if n > 0 && n < len(s.InstancePorts) {
		return s.InstancePorts[n]
	}
	return s.Port
}

// Render returns the command and env for instance n placed on gpu. gpu < 0
// (no telemetry yet) falls back to the first GPU the device field lists.
func (s *Stanza) Render(n, gpu int) (string, []string) {
	if gpu < 0 {
		gpu = 0
		if idxs, _, _ := ParseDevice(s.Device); len(idxs) > 0 {
			gpu = idxs[0]
		}
	}
	port := fmt.Sprintf("%d", s.InstancePort(n))
	g := fmt.Sprintf("%d", gpu)
	cmd := s.Command
	if s.rawCommand != "" {
		cmd = portMacroRe.ReplaceAllString(s.rawCommand, port)
	}
	cmd = strings.ReplaceAll(cmd, "{gpu}", g)
	var env []string
	for _, e := range s.Env {
		e = strings.ReplaceAll(e, "{gpu}", g)
		env = append(env, portMacroRe.ReplaceAllString(e, port))
	}
	return cmd, env
}

// ParseDevice reads a stanza device field. split is true for a comma list
// (one model across every listed GPU); a pipe list or a single index is a
// set of candidates for single-GPU placement. Empty, or "cpu", returns no
// indexes (a GPU-less stanza).
func ParseDevice(device string) (idxs []int, split bool, err error) {
	device = strings.TrimSpace(device)
	if device == "" {
		return nil, false, nil
	}
	if strings.EqualFold(device, "cpu") {
		// A CPU-only backend: no GPU index, nothing to place.
		return nil, false, nil
	}
	sep := "|"
	if strings.Contains(device, ",") {
		if strings.Contains(device, "|") {
			return nil, false, fmt.Errorf("device %q mixes ',' and '|'", device)
		}
		sep = ","
		split = true
	}
	seen := map[int]bool{}
	for _, part := range strings.Split(device, sep) {
		d := strings.ToLower(strings.TrimSpace(part))
		d = strings.TrimPrefix(d, "cuda")
		d = strings.TrimPrefix(d, "gpu:")
		var idx int
		if _, err := fmt.Sscanf(d, "%d", &idx); err != nil || fmt.Sprintf("%d", idx) != d {
			return nil, false, fmt.Errorf("device %q: bad GPU %q", device, part)
		}
		if seen[idx] {
			continue
		}
		seen[idx] = true
		idxs = append(idxs, idx)
	}
	return idxs, split && len(idxs) > 1, nil
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
	// Filters rewrite the request body before it is sent to this peer.
	Filters Filters `yaml:"filters"`
}

// Telemetry tunes the GPU poll and peer sync loops.
type Telemetry struct {
	PollSeconds     int `yaml:"poll_seconds"`
	PeerSyncSeconds int `yaml:"peer_sync_seconds"`
	// PeerStaleSeconds: cached peer telemetry older than this is treated as
	// unknown and the peer is excluded from candidate selection.
	PeerStaleSeconds int `yaml:"peer_stale_seconds"`
}

// Discovery scans the local network for router peers at startup and on a
// backoff timer. Enabled is nil when the key is absent (default on).
type Discovery struct {
	Enabled *bool `yaml:"enabled"`
	// Ports are the router ports probed on every scanned address.
	Ports []int `yaml:"ports"`
	// Subnets are the CIDRs to scan. Empty scans each private IPv4 /24 of
	// this host's non-loopback interfaces.
	Subnets []string `yaml:"subnets"`
	// Exclude drops addresses (CIDR or bare IP) from the scan.
	Exclude []string `yaml:"exclude"`
	// IntervalSeconds is the first gap between scans; it doubles to
	// MaxIntervalSeconds while nothing changes. Default 30 / 600.
	IntervalSeconds    int `yaml:"interval_seconds"`
	MaxIntervalSeconds int `yaml:"max_interval_seconds"`

	// Networks and Excluded are Subnets and Exclude parsed by Parse.
	Networks []netip.Prefix `yaml:"-"`
	Excluded []netip.Prefix `yaml:"-"`
}

// On reports whether discovery is enabled. An absent enabled key means on.
func (d *Discovery) On() bool { return d.Enabled == nil || *d.Enabled }

// Security gates clients on public addresses. Loopback, private LAN and
// Tailscale clients never need a key.
type Security struct {
	// APIKeys are the keys a public client may present. The env var
	// MODEL_ROUTER_API_KEYS (comma separated) adds to this list. With no keys,
	// public requests are refused.
	APIKeys []string `yaml:"api_keys"`
	// TrustedProxies are CIDRs or exact IPs whose X-Forwarded-For / X-Real-IP
	// headers are believed. Any other peer's forwarded headers are ignored.
	TrustedProxies []string `yaml:"trusted_proxies"`

	// ProxyPrefixes is TrustedProxies parsed by Parse.
	ProxyPrefixes []netip.Prefix `yaml:"-"`
}

// Catalog points the Model catalog at an optional metadata file.
type Catalog struct {
	// MetadataURL is one JSON file of family and model metadata (see SPEC.md).
	// Empty means off: the catalog uses built-in data only.
	MetadataURL string `yaml:"metadata_url"`
	// RefreshSeconds is how often the file is fetched again. Default 3600.
	RefreshSeconds int `yaml:"refresh_seconds"`
}

// APIKeysEnv names the environment variable that adds API keys.
const APIKeysEnv = "MODEL_ROUTER_API_KEYS"

// Config is the whole per-node configuration file.
type Config struct {
	Security Security `yaml:"security"`
	// Listen is the router's own bind address, e.g. "0.0.0.0:18080".
	Listen string `yaml:"listen"`
	// StartPort is the base for auto-allocated stanza ports.
	StartPort int `yaml:"start_port"`
	// ReservedPorts are ports the router must never auto-allocate (sidecars).
	ReservedPorts []int           `yaml:"reserved_ports"`
	Stanzas       []Stanza        `yaml:"stanzas"`
	Pools         map[string]Pool `yaml:"pools"`
	Peers         []Peer          `yaml:"peers"`
	// HeadroomMB is memory left unbooked on every GPU. Admission and every
	// published free figure subtract it; vram_mb is unchanged. Default 0.
	HeadroomMB int64 `yaml:"headroom_mb"`
	// AdmitTimeoutSeconds caps how long a request waits for local GPU room
	// (re-trying peers meanwhile) before it gets 503 with Retry-After.
	// Default 60.
	AdmitTimeoutSeconds int `yaml:"admit_timeout_seconds"`
	// StreamLoadProgress writes loading updates into a streaming chat response
	// while the chosen local backend spins up. Default false.
	StreamLoadProgress bool `yaml:"stream_load_progress"`
	// MaxRequestBodyMB caps the request body the router will read for any
	// stanza (a stanza may override it). Over the cap the client gets 413
	// before any model is loaded. Default 256.
	MaxRequestBodyMB int `yaml:"max_request_body_mb"`
	// VoiceDir enables the router voice library: a cloned voice is one WAV
	// clip and one JSON sidecar in this directory. Empty disables the library
	// and leaves /v1/audio/voices to the speech backends.
	VoiceDir string `yaml:"voice_dir"`
	// VoiceMaxMB and VoiceMaxSeconds cap an uploaded clip. 0 = the defaults
	// (10 MB, 60 s).
	VoiceMaxMB      int64 `yaml:"voice_max_mb"`
	VoiceMaxSeconds int   `yaml:"voice_max_seconds"`
	// JobTableMax caps the async job table; a full table drops its oldest
	// finished job. Default 256.
	JobTableMax int `yaml:"job_table_max"`
	// JobTTLMinutes is the longest the router holds a model for one async job
	// that never reports an end state. Default 60.
	JobTTLMinutes int `yaml:"job_ttl_minutes"`
	// SessionMaxMinutes closes a realtime websocket session that stays open
	// longer, with close code 1001. 0 (default) means no cap.
	SessionMaxMinutes int `yaml:"session_max_minutes"`
	// Preload is a list of stanza ids to load at startup.
	Preload   []string  `yaml:"preload"`
	Telemetry Telemetry `yaml:"telemetry"`
	Catalog   Catalog   `yaml:"catalog"`
	Discovery Discovery `yaml:"discovery"`

	// Derived lookups (populated by Load).
	stanzaByID map[string]*Stanza
	aliasByID  map[string]*Stanza
	poolByName map[string]*Pool
	peerByName map[string]*Peer
	// maxBodyBytes is the largest cap any stanza accepts, so the matcher can
	// bound a read before it knows which stanza matched.
	maxBodyBytes int64
}

// Defaults applied by Load when fields are unset.
const (
	DefaultHealthCheck          = "/health"
	DefaultSpinUp               = 60
	DefaultPoll                 = 2
	DefaultPeerSync             = 5
	DefaultPeerStale            = 15
	DefaultDiscoveryInterval    = 30
	DefaultDiscoveryMaxInterval = 600
	DefaultStartPort            = 5800
	// DefaultMaxRequestBodyMB caps a request body when max_request_body_mb is unset.
	DefaultMaxRequestBodyMB = 256
	// DefaultVoiceMaxMB and DefaultVoiceMaxSeconds cap a voice clip when
	// voice_max_mb / voice_max_seconds are unset.
	DefaultVoiceMaxMB      int64 = 10
	DefaultVoiceMaxSeconds       = 60
	// MaxRequestBodyMBLimit (1 TiB) keeps the cap in bytes, plus one for the
	// over-cap probe read, well inside int64.
	MaxRequestBodyMBLimit = 1 << 20
	// MaxRetryBlocked caps image_pipeline retry_blocked so one client request
	// cannot hold a model through a long run of blocked generations.
	MaxRetryBlocked = 5
)

// DefaultDiscoveryPorts are the router ports the scan probes when the
// discovery ports key is absent.
var DefaultDiscoveryPorts = []int{18080, 8081, 8082}

// parseDiscovery applies the discovery defaults and parses subnets/exclude.
func (c *Config) parseDiscovery() error {
	d := &c.Discovery
	if len(d.Ports) == 0 {
		d.Ports = append([]int(nil), DefaultDiscoveryPorts...)
	}
	for _, port := range d.Ports {
		if port <= 0 || port > 65535 {
			return fmt.Errorf("discovery.ports: %d is not a valid port", port)
		}
	}
	if d.IntervalSeconds == 0 {
		d.IntervalSeconds = DefaultDiscoveryInterval
	}
	if d.MaxIntervalSeconds == 0 {
		// An explicit interval above the default ceiling raises the ceiling:
		// the user never set a max, so there is nothing to conflict with.
		d.MaxIntervalSeconds = max(DefaultDiscoveryMaxInterval, d.IntervalSeconds)
	}
	if d.IntervalSeconds < 0 || d.MaxIntervalSeconds < 0 {
		return fmt.Errorf("discovery intervals must not be negative")
	}
	if d.MaxIntervalSeconds < d.IntervalSeconds {
		return fmt.Errorf("discovery.max_interval_seconds must be >= interval_seconds")
	}
	var err error
	if d.Networks, err = parsePrefixes(d.Subnets); err != nil {
		return fmt.Errorf("discovery.subnets: %w", err)
	}
	if d.Excluded, err = parsePrefixes(d.Exclude); err != nil {
		return fmt.Errorf("discovery.exclude: %w", err)
	}
	return nil
}

// parsePrefixes accepts CIDRs and bare IPs (a bare IP is a /32 or /128).
func parsePrefixes(raw []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(raw))
	for _, v := range raw {
		if pfx, err := netip.ParsePrefix(v); err == nil {
			out = append(out, pfx.Masked())
			continue
		}
		addr, err := netip.ParseAddr(v)
		if err != nil {
			return nil, fmt.Errorf("%q is not a CIDR or IP", v)
		}
		out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return out, nil
}

var portMacroRe = regexp.MustCompile(`\{port\}|\$\{PORT\}`)

// Load reads, validates, and normalizes a config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	return Parse(raw)
}

// DefaultAdmitTimeout applies when admit_timeout_seconds is unset.
const DefaultAdmitTimeout = 60 * time.Second

// AdmitTimeout is AdmitTimeoutSeconds, or DefaultAdmitTimeout when unset.
func (c *Config) AdmitTimeout() time.Duration {
	if c.AdmitTimeoutSeconds <= 0 {
		return DefaultAdmitTimeout
	}
	return time.Duration(c.AdmitTimeoutSeconds) * time.Second
}

// JobTableCap is JobTableMax, or 256 when unset.
func (c *Config) JobTableCap() int {
	if c.JobTableMax <= 0 {
		return 256
	}
	return c.JobTableMax
}

// JobTTL is JobTTLMinutes, or an hour when unset.
func (c *Config) JobTTL() time.Duration {
	if c.JobTTLMinutes <= 0 {
		return time.Hour
	}
	return time.Duration(c.JobTTLMinutes) * time.Minute
}

// VoiceMaxBytes is the byte cap for one library voice clip, from
// voice_max_mb or the default.
func (c *Config) VoiceMaxBytes() int64 {
	mb := c.VoiceMaxMB
	if mb <= 0 {
		mb = DefaultVoiceMaxMB
	}
	return mb << 20
}

// VoiceMaxSecondsValue is the play-length cap for one library voice clip,
// from voice_max_seconds or the default.
func (c *Config) VoiceMaxSecondsValue() int {
	if c.VoiceMaxSeconds <= 0 {
		return DefaultVoiceMaxSeconds
	}
	return c.VoiceMaxSeconds
}

// RequestBodyLimit is the byte cap for a request resolved to stanza id (empty
// for a pool or peer): the stanza's max_request_body_mb override, else the
// top-level value.
func (c *Config) RequestBodyLimit(stanzaID string) int64 {
	mb := c.MaxRequestBodyMB
	if s := c.stanzaByID[stanzaID]; s != nil && s.MaxRequestBodyMB > 0 {
		mb = s.MaxRequestBodyMB
	}
	if mb <= 0 {
		mb = DefaultMaxRequestBodyMB
	}
	return int64(mb) << 20
}

// MaxRequestBodyBytes is the largest cap any stanza accepts, so the matcher can
// bound a read before it knows which stanza matched.
func (c *Config) MaxRequestBodyBytes() int64 { return c.maxBodyBytes }

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
	if cfg.Catalog.RefreshSeconds <= 0 {
		cfg.Catalog.RefreshSeconds = 3600
	}
	if err := cfg.parseDiscovery(); err != nil {
		return nil, err
	}

	for _, k := range strings.Split(os.Getenv(APIKeysEnv), ",") {
		if k = strings.TrimSpace(k); k != "" {
			cfg.Security.APIKeys = append(cfg.Security.APIKeys, k)
		}
	}
	for i, k := range cfg.Security.APIKeys {
		if k == "" {
			return nil, fmt.Errorf("security.api_keys[%d] is empty", i)
		}
	}
	for _, tp := range cfg.Security.TrustedProxies {
		pfx, err := netip.ParsePrefix(tp)
		if err != nil {
			addr, aerr := netip.ParseAddr(tp)
			if aerr != nil {
				return nil, fmt.Errorf("security.trusted_proxies: %q is not a CIDR or IP", tp)
			}
			pfx = netip.PrefixFrom(addr, addr.BitLen())
		}
		cfg.Security.ProxyPrefixes = append(cfg.Security.ProxyPrefixes, pfx.Masked())
	}

	if cfg.HeadroomMB < 0 {
		return nil, fmt.Errorf("headroom_mb must not be negative")
	}
	if cfg.MaxRequestBodyMB == 0 {
		cfg.MaxRequestBodyMB = DefaultMaxRequestBodyMB
	}
	if cfg.MaxRequestBodyMB < 0 || cfg.MaxRequestBodyMB > MaxRequestBodyMBLimit {
		return nil, fmt.Errorf("max_request_body_mb must be between 0 and %d", MaxRequestBodyMBLimit)
	}
	if cfg.VoiceMaxMB < 0 {
		return nil, fmt.Errorf("voice_max_mb must not be negative")
	}
	if cfg.VoiceMaxSeconds < 0 {
		return nil, fmt.Errorf("voice_max_seconds must not be negative")
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
		if strings.Contains(s.ModelID, "#") {
			return nil, fmt.Errorf("stanza %q: model_id must not contain '#' (reserved for instance ids)", s.ModelID)
		}
		if s.Match.BodyField != "" && s.Match.PathPrefix != "" {
			return nil, fmt.Errorf("stanza %q: match.body_field and match.path_prefix are mutually exclusive", s.ModelID)
		}
		if _, dup := cfg.stanzaByID[s.ModelID]; dup {
			return nil, fmt.Errorf("duplicate model_id %q", s.ModelID)
		}
		for _, a := range s.Aliases {
			if strings.Contains(a, "#") {
				return nil, fmt.Errorf("stanza %q: alias %q must not contain '#'", s.ModelID, a)
			}
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
		if err := s.Filters.validate(fmt.Sprintf("stanza %q", s.ModelID), "model", s.Match.BodyField); err != nil {
			return nil, err
		}
		// Expand ${MODEL_ID} now so the keys are the ids clients send.
		byID := map[string]map[string]any{}
		for id, params := range s.Filters.SetParamsByID {
			id = strings.ReplaceAll(id, "${MODEL_ID}", s.ModelID)
			if id == "" || strings.Contains(id, "#") {
				return nil, fmt.Errorf("stanza %q: setParamsByID id %q is empty or contains '#'", s.ModelID, id)
			}
			byID[id] = params
		}
		s.Filters.SetParamsByID = byID
		if s.APIType == "" {
			s.APIType = APITypeChat
		}
		if !slices.Contains(APITypes, s.APIType) {
			return nil, fmt.Errorf("stanza %q: api_type %q is not valid; must be one of %s",
				s.ModelID, s.APIType, strings.Join(APITypes, ", "))
		}
		if s.PeerSpill == "" {
			s.PeerSpill = PeerSpillAuto
		}
		if !slices.Contains(PeerSpills, s.PeerSpill) {
			return nil, fmt.Errorf("stanza %q: peer_spill %q is not valid; must be one of %s",
				s.ModelID, s.PeerSpill, strings.Join(PeerSpills, ", "))
		}
		for _, c := range s.Capabilities {
			if !slices.Contains(Capabilities, c) {
				return nil, fmt.Errorf("stanza %q: capability %q is not valid; must be one of %s",
					s.ModelID, c, strings.Join(Capabilities, ", "))
			}
			if (c == "duration" || c == "resolution" || c == "seed") && s.APIType != APITypeVideo {
				return nil, fmt.Errorf("stanza %q: capability %q is only valid with api_type: video", s.ModelID, c)
			}
		}
		if s.Job != nil {
			if s.Job.Adapter == "" {
				s.Job.Adapter = JobAdapterOpenAI
			}
			if !slices.Contains(JobAdapters, s.Job.Adapter) {
				return nil, fmt.Errorf("stanza %q: job.adapter %q is not valid; must be one of %s",
					s.ModelID, s.Job.Adapter, strings.Join(JobAdapters, ", "))
			}
			if s.APIType != APITypeVideo {
				return nil, fmt.Errorf("stanza %q: job is only valid with api_type: video", s.ModelID)
			}
		}
		if s.ImagePipeline != nil {
			if err := s.ImagePipeline.expand(); err != nil {
				return nil, fmt.Errorf("stanza %q: %w", s.ModelID, err)
			}
			if s.APIType != APITypeImage {
				return nil, fmt.Errorf("stanza %q: image_pipeline is only valid with api_type: image", s.ModelID)
			}
			p := s.ImagePipeline
			if p.Caption != "" && !slices.Contains(imageCaptionFormats, p.Caption) {
				return nil, fmt.Errorf("stanza %q: image_pipeline caption %q is not valid; use one of %s",
					s.ModelID, p.Caption, strings.Join(imageCaptionFormats, ", "))
			}
			if p.RetryBlocked < 0 {
				return nil, fmt.Errorf("stanza %q: image_pipeline retry_blocked must not be negative", s.ModelID)
			}
			if p.RetryBlocked > MaxRetryBlocked {
				return nil, fmt.Errorf("stanza %q: image_pipeline retry_blocked must be at most %d", s.ModelID, MaxRetryBlocked)
			}
			if p.Enhancer != nil && p.Enhancer.Model == "" {
				return nil, fmt.Errorf("stanza %q: image_pipeline enhancer needs a model", s.ModelID)
			}
		}
		if s.HealthCheck == "" {
			s.HealthCheck = DefaultHealthCheck
		}
		if s.SpinUpSeconds == 0 {
			s.SpinUpSeconds = DefaultSpinUp
		}
		if err := s.validatePlacement(); err != nil {
			return nil, err
		}
		if s.MaxRequestBodyMB < 0 || s.MaxRequestBodyMB > MaxRequestBodyMBLimit {
			return nil, fmt.Errorf("stanza %q: max_request_body_mb must be between 0 and %d", s.ModelID, MaxRequestBodyMBLimit)
		}
		if s.RequestTimeoutSeconds < 0 {
			return nil, fmt.Errorf("stanza %q: request_timeout_seconds must not be negative", s.ModelID)
		}
		if s.CloneMode != "" {
			if s.APIType != APITypeSpeech {
				return nil, fmt.Errorf("stanza %q: clone_mode is only valid with api_type: speech", s.ModelID)
			}
			if !slices.Contains(CloneModes, s.CloneMode) {
				return nil, fmt.Errorf("stanza %q: clone_mode %q is not valid; must be one of %s",
					s.ModelID, s.CloneMode, strings.Join(CloneModes, ", "))
			}
		} else if s.APIType == APITypeSpeech {
			s.CloneMode = CloneModeRefAudio
		}
		for _, rw := range s.PathRewrite {
			if !strings.HasPrefix(rw.From, "/") || !strings.HasPrefix(rw.To, "/") {
				return nil, fmt.Errorf("stanza %q: path_rewrite from and to must both start with /", s.ModelID)
			}
		}
		customProxy := s.Proxy != ""
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
		if s.ModelPath != "" {
			s.Command = strings.ReplaceAll(s.Command, "{model_path}", s.ModelPath)
			s.Command = strings.ReplaceAll(s.Command, "{model}", s.ModelPath)
		}
		s.rawCommand = s.Command
		s.Command = portMacroRe.ReplaceAllString(s.Command, fmt.Sprintf("%d", s.Port))
		if s.Slots <= 0 {
			s.Slots = parallelFromCommand(s.Command)
		}
		if s.InstanceCount() > 1 {
			if customProxy {
				return nil, fmt.Errorf("stanza %q: max_instances > 1 cannot use a custom proxy", s.ModelID)
			}
			if !s.usesMacro(portMacroRe.MatchString) {
				return nil, fmt.Errorf("stanza %q: max_instances > 1 needs {port} in command or env", s.ModelID)
			}
		}
		cfg.stanzaByID[s.ModelID] = s
	}
	cfg.allocateInstancePorts(reserved)

	// The largest cap any stanza accepts, so the matcher can bound a read
	// before it knows which stanza matched.
	cfg.maxBodyBytes = int64(cfg.MaxRequestBodyMB) << 20
	for i := range cfg.Stanzas {
		if b := int64(cfg.Stanzas[i].MaxRequestBodyMB) << 20; b > cfg.maxBodyBytes {
			cfg.maxBodyBytes = b
		}
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

	// Filter-set ids resolve like aliases but are also listed in the catalog.
	for i := range cfg.Stanzas {
		s := &cfg.Stanzas[i]
		for _, id := range s.FilterIDs() {
			if cfg.stanzaByID[id] != nil || cfg.aliasByID[id] != nil || cfg.poolByName[id] != nil {
				return nil, fmt.Errorf("stanza %q: setParamsByID id %q collides with another id", s.ModelID, id)
			}
			cfg.aliasByID[id] = s
		}
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
		if len(p.Filters.SetParamsByID) > 0 {
			return nil, fmt.Errorf("peer %q: setParamsByID is only valid on stanzas", p.Name)
		}
		if err := p.Filters.validate(fmt.Sprintf("peer %q", p.Name), "model", p.BodyField); err != nil {
			return nil, err
		}
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

// validatePlacement checks the device field against {gpu} use and replicas.
func (s *Stanza) validatePlacement() error {
	idxs, split, err := ParseDevice(s.Device)
	if err != nil {
		return fmt.Errorf("stanza %q: %w", s.ModelID, err)
	}
	if s.CPUOnly() {
		// No card to place, so no {gpu} or device-shape rules apply.
		return nil
	}
	hasGPU := s.usesMacro(func(v string) bool { return strings.Contains(v, "{gpu}") })
	if split && hasGPU {
		return fmt.Errorf("stanza %q: {gpu} names one GPU but device %q splits across several", s.ModelID, s.Device)
	}
	if split && s.InstanceCount() > 1 {
		return fmt.Errorf("stanza %q: max_instances > 1 needs single-GPU placement, not device %q", s.ModelID, s.Device)
	}
	// A choice of GPUs is meaningless unless the backend is told which one won.
	choice := !split && (len(idxs) > 1 || (len(idxs) == 0 && s.InstanceCount() > 1))
	if choice && !hasGPU {
		return fmt.Errorf("stanza %q: device %q picks a GPU at load time; use {gpu} in command or env (e.g. CUDA_VISIBLE_DEVICES={gpu})", s.ModelID, s.Device)
	}
	return nil
}

// usesMacro reports whether match is true for the command or any env entry.
func (s *Stanza) usesMacro(match func(string) bool) bool {
	cmd := s.rawCommand
	if cmd == "" {
		cmd = s.Command
	}
	if match(cmd) {
		return true
	}
	for _, e := range s.Env {
		if match(e) {
			return true
		}
	}
	return false
}

// allocateInstancePorts gives every extra instance a port above all stanza
// and reserved ports, in config order, so instance ports never collide.
func (c *Config) allocateInstancePorts(reserved map[int]bool) {
	used := map[int]bool{}
	next := c.StartPort
	for p := range reserved {
		used[p] = true
	}
	for i := range c.Stanzas {
		p := c.Stanzas[i].Port
		used[p] = true
		if p >= next {
			next = p + 1
		}
	}
	for i := range c.Stanzas {
		s := &c.Stanzas[i]
		s.InstancePorts = []int{s.Port}
		for n := 1; n < s.InstanceCount(); n++ {
			for used[next] {
				next++
			}
			s.InstancePorts = append(s.InstancePorts, next)
			used[next] = true
		}
	}
}

func (c *Config) validateCatalog() error {
	pathDefault := map[string]string{}
	typeDefault := map[string]string{}
	for i := range c.Stanzas {
		s := &c.Stanzas[i]
		if s.Match.PathDefault && s.Match.PathPrefix != "" {
			if prev, ok := pathDefault[s.Match.PathPrefix]; ok {
				return fmt.Errorf("duplicate path_default for prefix %q (stanzas %q and %q)", s.Match.PathPrefix, prev, s.ModelID)
			}
			pathDefault[s.Match.PathPrefix] = s.ModelID
		}
		if s.Match.PathDefault {
			// Model-less routing by api_type needs one default per type, whatever the prefix.
			if prev, ok := typeDefault[s.APIType]; ok {
				return fmt.Errorf("duplicate path_default for api_type %q (stanzas %q and %q)", s.APIType, prev, s.ModelID)
			}
			typeDefault[s.APIType] = s.ModelID
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

// DefaultStanza returns the stanza a model-less request for apiType should be
// served by: the one marked path_default, or the only stanza of that type.
// candidates lists every stanza of that type, so a caller can name them when
// there is no single default (none, or more than one path_default); id is
// empty then.
func (c *Config) DefaultStanza(apiType string) (id string, candidates []string) {
	defaults := 0
	for i := range c.Stanzas {
		s := &c.Stanzas[i]
		if s.APIType != apiType {
			continue
		}
		candidates = append(candidates, s.ModelID)
		if s.Match.PathDefault {
			id = s.ModelID
			defaults++
		}
	}
	if defaults > 1 {
		return "", candidates
	}
	if id == "" && len(candidates) == 1 {
		id = candidates[0]
	}
	return id, candidates
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
