package router

import (
	"crypto/sha256"
	"crypto/subtle"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"model-router/internal/config"
)

// keylessPrefixes are the client ranges that never need an API key:
// loopback, RFC1918, link-local, IPv6 ULA and Tailscale.
var keylessPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, c := range []string{
		"127.0.0.0/8", "::1/128",
		"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
		"169.254.0.0/16", "fe80::/10",
		"fc00::/7",
		"100.64.0.0/10", "fd7a:115c:a1e0::/48",
	} {
		out = append(out, netip.MustParsePrefix(c))
	}
	return out
}()

func inPrefixes(a netip.Addr, ps []netip.Prefix) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func parseClientAddr(s string) netip.Addr {
	a, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil {
		return netip.Addr{}
	}
	return a.WithZone("").Unmap()
}

// clientAddr is the request's client IP. It comes from the accepted socket;
// X-Forwarded-For / X-Real-IP count only when that peer is a trusted proxy.
// The zero Addr (unparseable) is treated as public by the caller.
func clientAddr(req *http.Request, trusted []netip.Prefix) netip.Addr {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		host = req.RemoteAddr
	}
	peer := parseClientAddr(host)
	if !peer.IsValid() || !inPrefixes(peer, trusted) {
		return peer
	}
	// Walk X-Forwarded-For from the right, skipping further trusted proxies;
	// the first other hop is the client.
	if xff := strings.Join(req.Header.Values("X-Forwarded-For"), ","); xff != "" {
		hops := strings.Split(xff, ",")
		for i := len(hops) - 1; i >= 0; i-- {
			a := parseClientAddr(hops[i])
			if !a.IsValid() || !inPrefixes(a, trusted) {
				return a
			}
		}
	}
	if xr := req.Header.Get("X-Real-IP"); xr != "" {
		return parseClientAddr(xr)
	}
	return peer
}

// presentedKeys lists the keys a request offers: Bearer token, x-api-key, and
// the Basic password (so a browser's key prompt works on /ui/).
func presentedKeys(req *http.Request) []string {
	var out []string
	if auth := req.Header.Get("Authorization"); len(auth) > 7 && strings.EqualFold(auth[:7], "Bearer ") {
		out = append(out, strings.TrimSpace(auth[7:]))
	}
	if k := req.Header.Get("X-Api-Key"); k != "" {
		out = append(out, k)
	}
	if _, pw, ok := req.BasicAuth(); ok && pw != "" {
		out = append(out, pw)
	}
	return out
}

// keyValid compares in constant time: fixed-length digests, no early exit.
func keyValid(presented, configured []string) bool {
	ok := 0
	for _, p := range presented {
		ph := sha256.Sum256([]byte(p))
		for _, c := range configured {
			ch := sha256.Sum256([]byte(c))
			ok |= subtle.ConstantTimeCompare(ph[:], ch[:])
		}
	}
	return ok == 1
}

// NewAccessHandler requires an API key from clients on public addresses and
// lets loopback, LAN and Tailscale clients through untouched. Everything but
// /health is gated, so a route added later is protected by default.
func NewAccessHandler(sec config.Security, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/health" {
			next.ServeHTTP(w, req)
			return
		}
		if a := clientAddr(req, sec.ProxyPrefixes); a.IsValid() && inPrefixes(a, keylessPrefixes) {
			next.ServeHTTP(w, req)
			return
		}
		if keyValid(presentedKeys(req), sec.APIKeys) {
			next.ServeHTTP(w, req)
			return
		}
		w.Header().Add("WWW-Authenticate", `Bearer realm="model-router"`)
		w.Header().Add("WWW-Authenticate", `Basic realm="model-router", charset="UTF-8"`)
		http.Error(w, "api key required", http.StatusUnauthorized)
	})
}
