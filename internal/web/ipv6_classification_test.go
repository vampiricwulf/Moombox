package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// The listener is dual-stack: net.Listen("tcp", "0.0.0.0:774") on lan,
// external and public opens an AF_INET6 socket with IPV6_V6ONLY off wherever
// the kernel has IPv6, so IPv6 clients connect and are judged by
// network_access on their real address, and an IPv4 client arrives through
// the same socket (Go spells it as plain IPv4; a forwarded or hand-built
// "::ffff:a.b.c.d" is the same address). These pin how every check that
// classifies an address reads an IPv6 one: the IP gate, the Host and Origin
// checks, and the auth waivers.

// ipv6Peers is every IPv6 shape a peer can arrive as, spelled the way
// net/http fills RemoteAddr, and how lan and localhost class it.
var ipv6Peers = []struct {
	name, remote    string
	local, loopback bool
}{
	{"IPv6 loopback", "[::1]:50000", true, true},
	{"IPv4 through the dual-stack socket", "192.168.1.5:50000", true, false},
	{"IPv4-mapped private", "[::ffff:192.168.1.5]:50000", true, false},
	{"IPv4-mapped loopback", "[::ffff:127.0.0.1]:50000", true, true},
	{"IPv4-mapped public", "[::ffff:8.8.8.8]:50000", false, false},
	{"ULA, the compose network's fd77:4d42::/64", "[fd77:4d42::5]:50000", true, false},
	{"ULA, fc00::/7's lower half", "[fc00::5]:50000", true, false},
	{"link-local, with the zone Go reports for an accepted peer", "[fe80::1%eth0]:50000", true, false},
	{"link-local, no zone", "[fe80::1]:50000", true, false},
	{"global IPv6", "[2001:db8::5]:50000", false, false},
	{"global IPv6, a public resolver's", "[2606:4700:4700::1111]:50000", false, false},
}

func ipv6Store(networkAccess, passwordHash string) *config.Store {
	return config.NewStore(&config.MoomboxConfig{
		Network: config.NetworkConfig{NetworkAccess: networkAccess, PasswordHash: passwordHash},
	}, "")
}

// TestIPv6PeersThroughTheGates drives each peer through the IP gate, the Host
// gate and CSRF on a GET and a POST that address the server as localhost —
// a name every mode admits — so the peer's address is the only thing judged:
// on lan, loopback and private (ULA fc00::/7, link-local fe80::/10, an
// IPv4-mapped private address) pass and a global IPv6 one is refused; on
// localhost only loopback passes.
//
// A link-local peer used to be refused on lan: Go reports it with its zone
// ("fe80::1%eth0"), which net.ParseIP does not parse, so every classifier
// read it as no address at all — the phones on the LAN the link-local rule
// exists for.
//
// THE MUTANTS: parseIPLiteral refusing a zoned address, net.ParseIP's rule
// (the zoned link-local row is 403 on lan); drop fc00::/7 from privateCIDRs
// (the ULA rows are 403); drop the IsLinkLocalUnicast arm (both link-local
// rows are 403); count IsGlobalUnicast as private (the global rows pass on
// lan).
func TestIPv6PeersThroughTheGates(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for _, mode := range []string{"lan", "localhost"} {
		store := ipv6Store(mode, "")
		chain := IPGateMiddleware(store)(HostGateMiddleware(store)(CSRFMiddleware(store, "tok", &recordingLogger{})(ok)))
		for _, p := range ipv6Peers {
			want := http.StatusForbidden
			if p.loopback || (mode == "lan" && p.local) {
				want = http.StatusNoContent
			}
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				req := httptest.NewRequest(method, "/api/jobs", strings.NewReader(""))
				req.RemoteAddr = p.remote
				req.Host = "localhost:774"
				if method == http.MethodPost {
					req.Header.Set("Origin", "http://localhost:774")
				}
				rr := httptest.NewRecorder()
				chain.ServeHTTP(rr, req)
				if rr.Code != want {
					t.Errorf("%s, %s %s from %s: status %d, want %d", mode, p.name, method, p.remote, rr.Code, want)
				}
			}
		}
	}
}

// TestIPv6PeersAuthWaivers pins the waivers to the same classes: on external
// with a password, AuthMiddleware lets a loopback or private peer through
// without a session and asks a global one for it; IsLocalOrPrivateRequest
// (the set- and remove-password gates) and the WebSocket upgrade's LocalPeer
// read the peer the same way.
//
// THE MUTANTS: parseIPLiteral refusing a zoned address (the zoned link-local
// peer is asked for the password, IsLocalOrPrivateRequest and LocalPeer
// refuse it); drop fc00::/7 (the ULA peers are asked for it).
func TestIPv6PeersAuthWaivers(t *testing.T) {
	store := ipv6Store("external", "scrypt:0011:2233")
	s := NewServer(store, testWSLogger{})
	lp := s.WebSocket().LocalPeer
	for _, p := range ipv6Peers {
		req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
		req.RemoteAddr = p.remote

		reached := false
		rr := httptest.NewRecorder()
		s.AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true })).ServeHTTP(rr, req)
		if reached != p.local {
			t.Errorf("external + password, %s (%s) with no session: reached=%v status %d, want reached=%v",
				p.name, p.remote, reached, rr.Code, p.local)
		}
		if got := IsLocalOrPrivateRequest(store, req); got != p.local {
			t.Errorf("IsLocalOrPrivateRequest, %s (%s) = %v, want %v", p.name, p.remote, got, p.local)
		}
		if got := lp(ExtractIP(req)); got != p.local {
			t.Errorf("WebSocket LocalPeer, %s (%s) = %v, want %v", p.name, p.remote, got, p.local)
		}
	}
}

// TestIPv6LiteralHostsAndOrigins: a browser that typed an IPv6 literal sends
// it as Host and Origin, and the Host gate and CSRF class it as they class a
// peer — on lan a ULA or loopback literal is the dashboard's own address and
// a global one is a name the mode does not admit; an IPv4-mapped literal is
// its IPv4 address.
//
// THE MUTANTS: drop fc00::/7 (the ULA host is 403); parseIPLiteral refusing
// an IPv4-mapped address, addr.Is4In6() (the mapped private host is 403).
func TestIPv6LiteralHostsAndOrigins(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for _, tc := range []struct {
		mode, host string
		want       int
	}{
		{"lan", "[fd77:4d42::1]:774", http.StatusNoContent},
		{"lan", "[::1]:774", http.StatusNoContent},
		{"lan", "[::ffff:192.168.1.10]:774", http.StatusNoContent},
		{"lan", "[2001:db8::1]:774", http.StatusForbidden},
		{"lan", "[::ffff:8.8.8.8]:774", http.StatusForbidden},
		{"localhost", "[::1]:774", http.StatusNoContent},
		{"localhost", "[fd77:4d42::1]:774", http.StatusForbidden},
	} {
		store := ipv6Store(tc.mode, "")
		chain := HostGateMiddleware(store)(CSRFMiddleware(store, "tok", &recordingLogger{})(ok))
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			req := httptest.NewRequest(method, "/api/jobs", strings.NewReader(""))
			req.RemoteAddr = "[::1]:50000"
			req.Host = tc.host
			if method == http.MethodPost {
				req.Header.Set("Origin", "http://"+tc.host)
			}
			rr := httptest.NewRecorder()
			chain.ServeHTTP(rr, req)
			if rr.Code != tc.want {
				t.Errorf("%s, %s with Host/Origin %s: status %d, want %d: %s", tc.mode, method, tc.host, rr.Code, tc.want, rr.Body.String())
			}
		}
	}
}
