package httputil

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"

	"codefloe.com/pat-s/zendrite/setup/config"
)

var trustedProxies atomic.Pointer[[]netip.Prefix]

// defaultTrustedProxies is used until SetTrustedProxies is called.
var defaultTrustedProxies = sync.OnceValue(func() []netip.Prefix {
	prefixes := make([]netip.Prefix, 0, len(config.DefaultTrustedProxies))
	for _, s := range config.DefaultTrustedProxies {
		p, err := config.ParseTrustedProxy(s)
		if err != nil {
			panic(err)
		}
		prefixes = append(prefixes, p)
	}
	return prefixes
})

// SetTrustedProxies replaces the list of reverse proxies whose
// X-Forwarded-For and X-Real-IP headers ClientIP believes. It is called once
// at startup from global.trusted_proxies.
func SetTrustedProxies(prefixes []netip.Prefix) {
	p := append([]netip.Prefix(nil), prefixes...)
	trustedProxies.Store(&p)
}

// currentTrustedProxies returns the configured trusted proxies, or the
// defaults when none have been configured yet.
func currentTrustedProxies() []netip.Prefix {
	if p := trustedProxies.Load(); p != nil {
		return *p
	}
	return defaultTrustedProxies()
}

// ClientIP returns the IP address of the client that made req, without a
// port. Forwarding headers are only honored when the direct peer is a
// trusted proxy; otherwise anyone could pick their own address.
func ClientIP(req *http.Request) string {
	if addr := clientAddr(req, currentTrustedProxies()); addr.IsValid() {
		return addr.String()
	}
	// RemoteAddr is not an IP (e.g. a unix socket); keep it as it is.
	if host, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
		return host
	}
	return req.RemoteAddr
}

func clientAddr(req *http.Request, trusted []netip.Prefix) netip.Addr {
	peer := parseAddr(req.RemoteAddr)
	// A unix socket peer has no IP address. Only a local reverse proxy can
	// connect over one, so treat it as trusted.
	isUnix := !peer.IsValid() && (req.RemoteAddr == "" || req.RemoteAddr == "@" || strings.HasPrefix(req.RemoteAddr, "/"))
	if !isUnix && !isTrusted(peer, trusted) {
		return peer
	}

	// Walk X-Forwarded-For from the right: each trusted hop appended the
	// address it received the request from, so the first untrusted address
	// is the client. Anything further left is client-controlled.
	if hops := forwardedFor(req.Header); len(hops) > 0 {
		client := peer
		for i := len(hops) - 1; i >= 0; i-- {
			hop := parseAddr(hops[i])
			if !hop.IsValid() {
				break
			}
			client = hop
			if !isTrusted(hop, trusted) {
				break
			}
		}
		if client.IsValid() {
			return client
		}
	}

	if real := parseAddr(strings.TrimSpace(req.Header.Get("X-Real-IP"))); real.IsValid() {
		return real
	}
	return peer
}

func forwardedFor(h http.Header) []string {
	var hops []string
	for _, v := range h.Values("X-Forwarded-For") {
		for _, hop := range strings.Split(v, ",") {
			hops = append(hops, strings.TrimSpace(hop))
		}
	}
	return hops
}

// parseAddr parses an IP address with or without a port, unmapping
// IPv4-mapped IPv6 addresses and dropping zones.
func parseAddr(s string) netip.Addr {
	if s == "" {
		return netip.Addr{}
	}
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().Unmap().WithZone("")
	}
	if a, err := netip.ParseAddr(strings.Trim(s, "[]")); err == nil {
		return a.Unmap().WithZone("")
	}
	return netip.Addr{}
}

func isTrusted(addr netip.Addr, trusted []netip.Prefix) bool {
	if !addr.IsValid() {
		return false
	}
	for _, p := range trusted {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// rateLimitKey returns the bucket a client IP is rate limited under. IPv4
// clients are keyed by address; IPv6 clients by their /64, since a single
// host is usually given a whole /64 and could otherwise rotate addresses.
func rateLimitKey(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	addr = addr.Unmap().WithZone("")
	if addr.Is6() {
		return netip.PrefixFrom(addr, 64).Masked().String() //nolint:mnd // IPv6 subnet size.
	}
	return addr.String()
}
