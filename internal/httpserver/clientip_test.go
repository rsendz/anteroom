package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func resolverFor(t *testing.T, cidrs ...string) *clientIPResolver {
	t.Helper()
	r, err := newClientIPResolver(cidrs)
	if err != nil {
		t.Fatalf("newClientIPResolver: %v", err)
	}
	return r
}

// request builds a request from a given peer with an optional XFF chain.
func request(peer string, xff ...string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = peer
	for _, v := range xff {
		req.Header.Add("X-Forwarded-For", v)
	}
	return req
}

func TestClientIPIgnoresSpoofedHeaderFromUntrustedPeer(t *testing.T) {
	// The whole point of the trusted list: a visitor who simply sets the
	// header must not be able to choose which bucket they are counted under.
	r := resolverFor(t, "10.0.0.0/8")
	got, ok := r.clientIP(request("203.0.113.9:5555", "1.2.3.4, 5.6.7.8"))
	if !ok {
		t.Fatal("no client IP resolved")
	}
	if got.String() != "203.0.113.9" {
		t.Errorf("clientIP = %s, want the peer address 203.0.113.9", got)
	}
}

func TestClientIPWithNoTrustedProxies(t *testing.T) {
	r := resolverFor(t)
	got, _ := r.clientIP(request("203.0.113.9:5555", "1.2.3.4"))
	if got.String() != "203.0.113.9" {
		t.Errorf("clientIP = %s, want 203.0.113.9 (header ignored)", got)
	}
}

func TestClientIPThroughTrustedProxies(t *testing.T) {
	r := resolverFor(t, "10.0.0.0/8", "192.168.0.0/16")

	cases := []struct {
		name string
		peer string
		xff  []string
		want string
	}{
		{
			name: "one load balancer",
			peer: "10.0.0.7:443",
			xff:  []string{"198.51.100.4"},
			want: "198.51.100.4",
		},
		{
			name: "two trusted hops",
			peer: "10.0.0.7:443",
			xff:  []string{"198.51.100.4, 192.168.1.1"},
			want: "198.51.100.4",
		},
		{
			name: "spoofed entries before the real client are not reached",
			peer: "10.0.0.7:443",
			xff:  []string{"1.1.1.1, 198.51.100.4"},
			want: "198.51.100.4",
		},
		{
			name: "header split across repeated fields",
			peer: "10.0.0.7:443",
			xff:  []string{"198.51.100.4", "192.168.1.1"},
			want: "198.51.100.4",
		},
		{
			name: "every hop trusted falls back to the peer",
			peer: "10.0.0.7:443",
			xff:  []string{"10.1.1.1, 192.168.1.1"},
			want: "10.0.0.7",
		},
		{
			name: "no header at all",
			peer: "10.0.0.7:443",
			want: "10.0.0.7",
		},
		{
			name: "entry carrying a port",
			peer: "10.0.0.7:443",
			xff:  []string{"198.51.100.4:1234"},
			want: "198.51.100.4",
		},
		{
			name: "malformed entry stops the walk",
			peer: "10.0.0.7:443",
			xff:  []string{"not-an-ip, 192.168.1.1"},
			want: "10.0.0.7",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := r.clientIP(request(tc.peer, tc.xff...))
			if !ok {
				t.Fatal("no client IP resolved")
			}
			if got.String() != tc.want {
				t.Errorf("clientIP = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestClientIPv6(t *testing.T) {
	r := resolverFor(t, "fd00::/8")
	got, ok := r.clientIP(request("[fd00::1]:443", "2001:db8:1:2::5"))
	if !ok {
		t.Fatal("no client IP resolved")
	}
	if got.String() != "2001:db8:1:2::5" {
		t.Errorf("clientIP = %s", got)
	}
}

func TestClientIPUnparseablePeer(t *testing.T) {
	r := resolverFor(t)
	if _, ok := r.clientIP(request("@")); ok {
		t.Error("an unparseable peer should not resolve to an address")
	}
}

func TestBucketGroupsIPv6BySixtyFour(t *testing.T) {
	// Two addresses a customer could hold simultaneously must share a bucket,
	// or IPv6 makes the limit meaningless.
	a, _ := parseAddr("2001:db8:1:2::5")
	b, _ := parseAddr("2001:db8:1:2:ffff:ffff:ffff:ffff")
	if bucket(a) != bucket(b) {
		t.Errorf("addresses in one /64 landed in different buckets: %s vs %s", bucket(a), bucket(b))
	}

	// A different /64 is a different customer.
	c, _ := parseAddr("2001:db8:1:3::5")
	if bucket(a) == bucket(c) {
		t.Error("addresses in different /64s shared a bucket")
	}
}

func TestBucketKeepsIPv4PerAddress(t *testing.T) {
	a, _ := parseAddr("198.51.100.4")
	b, _ := parseAddr("198.51.100.5")
	if bucket(a) == bucket(b) {
		t.Error("distinct IPv4 addresses shared a bucket")
	}
	if got := bucket(a); got != "198.51.100.4" {
		t.Errorf("bucket = %q, want the bare address", got)
	}
}

func TestInvalidTrustedProxyIsReported(t *testing.T) {
	if _, err := newClientIPResolver([]string{"not-a-cidr"}); err == nil {
		t.Error("want an error for an invalid CIDR")
	}
	// A bare address is a common mistake and should be caught too.
	if _, err := newClientIPResolver([]string{"10.0.0.1"}); err == nil {
		t.Error("want an error for a bare address without a prefix length")
	}
}

func TestLooksProxiedButUntrusted(t *testing.T) {
	// The misconfiguration this catches is severe: with a load balancer in
	// front that nobody declared, every visitor resolves to the balancer and
	// they all share one rate-limit budget.
	untrusting := resolverFor(t)
	if !untrusting.looksProxiedButUntrusted(request("10.0.0.7:443", "198.51.100.4")) {
		t.Error("a forwarded header from an untrusted peer was not flagged")
	}
	if untrusting.looksProxiedButUntrusted(request("10.0.0.7:443")) {
		t.Error("a request with no forwarded header was flagged")
	}

	trusting := resolverFor(t, "10.0.0.0/8")
	if trusting.looksProxiedButUntrusted(request("10.0.0.7:443", "198.51.100.4")) {
		t.Error("a correctly configured proxy was flagged")
	}
}
