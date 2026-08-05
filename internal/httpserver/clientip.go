package httpserver

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ipv6BucketBits groups IPv6 addresses by /64 for rate limiting.
//
// A single customer is routinely handed a whole /64 (often more), so limiting
// per address would let one connection rotate through billions of them and
// never hit a limit. IPv4 has no such problem and is counted per address.
const ipv6BucketBits = 64

// clientIPResolver turns a request into the address a rate limit should be
// counted against.
//
// X-Forwarded-For is attacker-controlled: anyone can put whatever they like in
// it. It is therefore honoured only when the request actually arrived from a
// proxy the operator told us to trust, and even then only back to the first
// hop we did not put there ourselves. With no trusted proxies configured, the
// header is ignored entirely.
type clientIPResolver struct {
	trusted []netip.Prefix
}

func newClientIPResolver(cidrs []string) (*clientIPResolver, error) {
	r := &clientIPResolver{}
	for _, cidr := range cidrs {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(cidr))
		if err != nil {
			return nil, err
		}
		r.trusted = append(r.trusted, prefix.Masked())
	}
	return r, nil
}

// clientIP returns the address to attribute this request to. The boolean is
// false when no usable address could be determined at all, in which case the
// caller should not apply a per-address limit rather than lumping every such
// request into one bucket.
func (r *clientIPResolver) clientIP(req *http.Request) (netip.Addr, bool) {
	peer, ok := parseAddr(remoteHost(req.RemoteAddr))
	if !ok {
		return netip.Addr{}, false
	}
	if !r.isTrusted(peer) {
		// The request did not come from a proxy we trust, so anything it
		// claims about its own origin is unverifiable.
		return peer, true
	}

	// Walk right to left: the rightmost entries were appended by infrastructure
	// closest to us, and the first one outside our trusted set is the earliest
	// hop we can still vouch for.
	forwarded := forwardedFor(req)
	for i := len(forwarded) - 1; i >= 0; i-- {
		addr, ok := parseAddr(forwarded[i])
		if !ok {
			// A malformed entry means the chain below it is not trustworthy.
			break
		}
		if !r.isTrusted(addr) {
			return addr, true
		}
	}
	// Every hop is trusted infrastructure; the peer is the best we have.
	return peer, true
}

// looksProxiedButUntrusted reports a request that carries forwarding headers
// from a peer that is not in the trusted list.
//
// This is worth surfacing because the consequence is severe and silent: if
// anteroom is behind a load balancer nobody declared, every visitor resolves
// to that one balancer's address, they all share a single rate-limit budget,
// and the join limit throttles the entire site at once.
func (r *clientIPResolver) looksProxiedButUntrusted(req *http.Request) bool {
	if len(req.Header.Values("X-Forwarded-For")) == 0 {
		return false
	}
	peer, ok := parseAddr(remoteHost(req.RemoteAddr))
	return ok && !r.isTrusted(peer)
}

// bucket is the key a limit is counted under: the address for IPv4, the
// containing /64 for IPv6.
func bucket(addr netip.Addr) string {
	if addr.Is4() || addr.Is4In6() {
		return addr.Unmap().String()
	}
	prefix, err := addr.Prefix(ipv6BucketBits)
	if err != nil {
		return addr.String()
	}
	return prefix.String()
}

func (r *clientIPResolver) isTrusted(addr netip.Addr) bool {
	for _, prefix := range r.trusted {
		if prefix.Contains(addr.Unmap()) {
			return true
		}
	}
	return false
}

func forwardedFor(req *http.Request) []string {
	var out []string
	for _, header := range req.Header.Values("X-Forwarded-For") {
		for _, part := range strings.Split(header, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

func remoteHost(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}

func parseAddr(s string) (netip.Addr, bool) {
	// Some proxies write "[::1]" or append a port even inside XFF.
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimSuffix(s, "]"), "[")
	if addr, err := netip.ParseAddr(s); err == nil {
		return addr.Unmap(), true
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		if addr, err := netip.ParseAddr(host); err == nil {
			return addr.Unmap(), true
		}
	}
	return netip.Addr{}, false
}
