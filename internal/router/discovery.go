package router

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"sync"
	"time"

	"model-router/internal/config"
)

const (
	discoveryProbeTimeout = 500 * time.Millisecond
	discoveryConcurrency  = 32
	// discoveryDropAfter is how many consecutive scans a discovered peer may
	// miss before it is dropped.
	discoveryDropAfter = 3
	discoveryMaxHosts  = 1 << 16
)

// discoveryClock is the only timing the scan loop needs, so a test can drive
// the backoff without waiting.
type discoveryClock interface {
	after(time.Duration) <-chan time.Time
}

type wallClock struct{}

func (wallClock) after(d time.Duration) <-chan time.Time { return time.After(d) }

// Discoverer scans the local network for router peers. It runs immediately at
// startup, then on a backoff timer that resets when the peer set changes.
type Discoverer struct {
	cfg    func() *config.Config
	router *Router
	logger *Logger
	clock  discoveryClock
	// dial and client are test seams: a test counts dials or answers probes
	// without a network.
	dial   func(context.Context, string, string) (net.Conn, error)
	client *http.Client
}

func NewDiscoverer(cfg func() *config.Config, r *Router, logger *Logger) *Discoverer {
	return &Discoverer{cfg: cfg, router: r, logger: logger, clock: wallClock{}}
}

// Run scans until ctx is cancelled. With discovery off it makes no
// connections but keeps checking the config, so a reload turns it back on.
func (d *Discoverer) Run(ctx context.Context) {
	wait := time.Duration(0)
	for {
		base := time.Duration(config.DefaultDiscoveryInterval) * time.Second
		ceiling := time.Duration(config.DefaultDiscoveryMaxInterval) * time.Second
		if cfg := d.cfg(); cfg != nil {
			base = time.Duration(cfg.Discovery.IntervalSeconds) * time.Second
			ceiling = time.Duration(cfg.Discovery.MaxIntervalSeconds) * time.Second
		}
		wait = min(max(wait, base), ceiling)
		added, dropped := d.scan(ctx)
		if added > 0 || dropped > 0 {
			wait = base
		}
		select {
		case <-ctx.Done():
			return
		case <-d.clock.after(wait):
		}
		wait = min(wait*2, ceiling)
	}
}

func (d *Discoverer) httpClient() *http.Client {
	if d.client != nil {
		return d.client
	}
	dial := d.dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	return &http.Client{Transport: &http.Transport{DialContext: dial}}
}

// scan probes every target once and merges the responses into the runtime
// peer set. added and dropped count changes to the discovered set.
func (d *Discoverer) scan(ctx context.Context) (added, dropped int) {
	cfg := d.cfg()
	if cfg == nil || !cfg.Discovery.On() {
		return 0, d.router.clearDiscovery()
	}
	targets := d.targets(cfg)
	client := d.httpClient()
	defer client.CloseIdleConnections()

	// Size the buffer to the work: probes send while still holding their sem
	// slot, so a buffer smaller than the target list deadlocks the dispatch
	// loop once every slot holds a blocked sender.
	results := make(chan probeResult, len(targets))
	sem := make(chan struct{}, discoveryConcurrency)
	var wg sync.WaitGroup
	for _, ap := range targets {
		select {
		case <-ctx.Done():
			break
		case sem <- struct{}{}:
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(ap netip.AddrPort) {
			defer wg.Done()
			defer func() { <-sem }()
			if r, ok := d.probe(ctx, client, ap); ok {
				results <- r
			}
		}(ap)
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	byNode := map[string][]probeResult{}
	for r := range results {
		byNode[r.node] = append(byNode[r.node], r)
	}
	found, configured := d.collapse(cfg, byNode)
	return d.router.applyDiscovery(found, configured)
}

// probeResult is one address that answered with this node's telemetry.
type probeResult struct {
	node      string
	addr      netip.AddrPort
	proxied   bool
	telemetry *Telemetry
}

func (d *Discoverer) probe(ctx context.Context, client *http.Client, ap netip.AddrPort) (probeResult, bool) {
	pctx, cancel := context.WithTimeout(ctx, discoveryProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, "http://"+ap.String()+"/_router/telemetry", nil)
	if err != nil {
		return probeResult{}, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return probeResult{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return probeResult{}, false
	}
	var t Telemetry
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t); err != nil {
		return probeResult{}, false
	}
	if t.Node == "" {
		return probeResult{}, false
	}
	// A reverse proxy in the path advertises itself with one of these.
	proxied := resp.Header.Get("Via") != "" ||
		resp.Header.Get("X-Forwarded-For") != "" ||
		resp.Header.Get("X-Real-IP") != ""
	return probeResult{node: t.Node, addr: ap, proxied: proxied, telemetry: &t}, true
}

// collapse turns probe hits into one peer per node: configured entries win,
// otherwise a direct (not proxied) address, then the earliest configured port.
func (d *Discoverer) collapse(cfg *config.Config, byNode map[string][]probeResult) (map[string]discoveredResult, map[string]bool) {
	configuredEndpoints := map[netip.AddrPort]*config.Peer{}
	for i := range cfg.Peers {
		if ap, ok := endpoint(cfg.Peers[i].BaseURL); ok {
			configuredEndpoints[ap] = &cfg.Peers[i]
		}
	}
	// A proxy usually adds no response header, so the proxied flag often
	// ties. The configured port order is then the evidence: discovery.ports
	// lists the direct router port first (18080 before the proxy ports 8081
	// and 8082). A plain lowest-port tie-break would pick the proxy.
	portRank := make(map[uint16]int, len(cfg.Discovery.Ports))
	for i, port := range cfg.Discovery.Ports {
		portRank[uint16(port)] = i
	}
	rank := func(port uint16) int {
		if i, ok := portRank[port]; ok {
			return i
		}
		return len(cfg.Discovery.Ports)
	}
	found := map[string]discoveredResult{}
	configured := map[string]bool{}
	for node, results := range byNode {
		if node == d.router.node {
			continue
		}
		sort.Slice(results, func(i, j int) bool {
			if results[i].proxied != results[j].proxied {
				return !results[i].proxied
			}
			if ri, rj := rank(results[i].addr.Port()), rank(results[j].addr.Port()); ri != rj {
				return ri < rj
			}
			if results[i].addr.Port() != results[j].addr.Port() {
				return results[i].addr.Port() < results[j].addr.Port()
			}
			return results[i].addr.Addr().Less(results[j].addr.Addr())
		})
		var configuredPeer *config.Peer
		for i := range cfg.Peers {
			if cfg.Peers[i].Name == node {
				configuredPeer = &cfg.Peers[i]
				break
			}
		}
		if configuredPeer == nil {
			for _, r := range results {
				if p := configuredEndpoints[r.addr]; p != nil {
					configuredPeer = p
					break
				}
			}
		}
		if configuredPeer == nil {
			// A hostname base_url does not parse as an address; the syncer's
			// cached telemetry names the machine the same way the probe does.
			for i := range cfg.Peers {
				if t, _, _ := d.router.peers.Entry(cfg.Peers[i].Name); t != nil && t.Node == node {
					configuredPeer = &cfg.Peers[i]
					break
				}
			}
		}
		if configuredPeer != nil {
			configured[node] = true
			configured[configuredPeer.Name] = true
			continue
		}
		best := results[0]
		found[node] = discoveredResult{
			peer: &config.Peer{
				Name:      node,
				Kind:      "router",
				BaseURL:   "http://" + best.addr.String(),
				BodyField: "model",
				Models:    modelsFrom(best.telemetry),
			},
			telemetry: best.telemetry,
		}
	}
	return found, configured
}

// modelsFrom is the peer's advertised catalog, or the model ids it reports
// loaded when an older node sends no catalog.
func modelsFrom(t *Telemetry) []string {
	if t == nil {
		return nil
	}
	if len(t.Catalog) > 0 {
		return append([]string(nil), t.Catalog...)
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range t.LoadedModels {
		if m.ID != "" && !seen[m.ID] {
			seen[m.ID] = true
			out = append(out, m.ID)
		}
	}
	return out
}

func endpoint(raw string) (netip.AddrPort, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return netip.AddrPort{}, false
	}
	ap, err := netip.ParseAddrPort(u.Host)
	if err != nil {
		return netip.AddrPort{}, false
	}
	return ap, true
}

// targets expands the configured subnets (or each private IPv4 /24 of the
// host's non-loopback interfaces) across the configured ports.
func (d *Discoverer) targets(cfg *config.Config) []netip.AddrPort {
	prefixes := cfg.Discovery.Networks
	if len(prefixes) == 0 {
		prefixes = interfacePrefixes()
	}
	seen := map[netip.AddrPort]bool{}
	var out []netip.AddrPort
	for _, p := range prefixes {
		hosts := prefixHosts(p)
		if len(hosts) == 0 && !p.Addr().Is4() {
			continue
		}
		if len(hosts) == 0 {
			// shortcut: prefixes wider than /16 are not enumerated, upgrade if a
			// site needs a whole /8 scanned.
			d.logger.Meshf("discovery: skipping %s (wider than /16)", p)
			continue
		}
		for _, addr := range hosts {
			if excluded(cfg.Discovery.Excluded, addr) {
				continue
			}
			for _, port := range cfg.Discovery.Ports {
				ap := netip.AddrPortFrom(addr, uint16(port))
				if seen[ap] {
					continue
				}
				seen[ap] = true
				out = append(out, ap)
			}
		}
	}
	return out
}

func excluded(prefixes []netip.Prefix, addr netip.Addr) bool {
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// interfacePrefixes is each private IPv4 /24 on an up, non-loopback interface.
func interfacePrefixes() []netip.Prefix {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	seen := map[netip.Prefix]bool{}
	var out []netip.Prefix
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil {
				continue
			}
			addr, ok := netip.AddrFromSlice(ip4)
			if !ok || !addr.IsPrivate() {
				continue
			}
			p := netip.PrefixFrom(addr, 24).Masked()
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// prefixHosts lists the usable addresses of an IPv4 prefix. /31 and /32 use
// every address; smaller prefixes skip the network and broadcast addresses.
func prefixHosts(p netip.Prefix) []netip.Addr {
	if !p.Addr().Is4() {
		return nil
	}
	p = p.Masked()
	base := p.Addr().As4()
	start := uint64(binary.BigEndian.Uint32(base[:]))
	size := uint64(1) << (32 - p.Bits())
	if size > discoveryMaxHosts {
		return nil
	}
	first, last := start, start+size-1
	if p.Bits() < 31 {
		first++
		last--
	}
	out := make([]netip.Addr, 0, last-first+1)
	for v := first; v <= last; v++ {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], uint32(v))
		out = append(out, netip.AddrFrom4(b))
	}
	return out
}
