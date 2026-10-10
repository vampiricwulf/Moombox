package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// D-S10: 100.64.0.0/10 — the shared address space Tailscale numbers its
// nodes from — is private on lan, and only on lan. These pin the classifier
// and every decision that consults it: the IP gate, the Host and Origin
// checks, and the auth waivers (AuthMiddleware, IsLocalOrPrivateRequest, the
// WebSocket upgrade).

const tailnetPeer = "100.101.102.103"

func sharedSpaceStore(networkAccess string) *config.Store {
	return config.NewStore(&config.MoomboxConfig{
		Network: config.NetworkConfig{
			NetworkAccess: networkAccess,
			PasswordHash:  "scrypt:0011:2233", // any non-empty hash: IsAuthRequired tests emptiness
		},
	}, "")
}

// TestIsPrivateIPForSharedAddressSpace pins the classifier.
//
// THE MUTANTS: drop the sharedAddressSpace test (return false after the mode
// check) — every lan `true` row in the range fails; drop the
// `networkAccess != "lan"` guard — the external, public, localhost and unset
// rows fail, which is a password waived on external for a stranger behind
// the same ISP NAT; widen the range to 100.0.0.0/8 — the two just-outside
// rows fail; narrow it to 100.64.0.0/16 — the 100.101 and 100.127 rows fail.
func TestIsPrivateIPForSharedAddressSpace(t *testing.T) {
	for _, tc := range []struct {
		ip, mode string
		want     bool
	}{
		{"100.64.0.1", "lan", true},
		{tailnetPeer, "lan", true},
		{"100.127.255.255", "lan", true},
		{"::ffff:100.64.0.1", "lan", true}, // IPv4-mapped spelling
		{"100.63.255.255", "lan", false},   // just below the range
		{"100.128.0.0", "lan", false},      // just above it
		{"100.64.0.1", "external", false},
		{"100.64.0.1", "public", false},
		{"100.64.0.1", "localhost", false},
		{"100.64.0.1", "", false},
		{"192.168.1.5", "external", true},       // RFC 1918 is private on every mode, as before
		{"fd7a:115c:a1e0::1", "external", true}, // Tailscale's IPv6 is ULA, private already
		{"not-an-ip", "lan", false},
	} {
		if got := isPrivateIPFor(tc.ip, tc.mode); got != tc.want {
			t.Errorf("isPrivateIPFor(%q, %q) = %v, want %v", tc.ip, tc.mode, got, tc.want)
		}
	}
}

// TestTailnetPeerReachesALANDashboard drives a tailnet browser through the
// gates in chain order: the IP gate and the Host gate on a GET, and CSRF on a
// POST whose Origin is the tailnet address the browser typed.
//
// THE MUTANTS: the lan arm of ipAllowedByNetworkAccess back to
// `isLoopback(ip) || isPrivateIP(ip)` — every lan row is 403 at the IP gate;
// the lan arm of isAllowedOrigin back to isPrivateIP(hostname) — the GET is
// 403 at the Host gate and the POST at CSRF.
func TestTailnetPeerReachesALANDashboard(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	chain := func(store *config.Store) http.Handler {
		return IPGateMiddleware(store)(HostGateMiddleware(store)(CSRFMiddleware(store, "tok", &recordingLogger{})(ok)))
	}
	send := func(store *config.Store, method string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/jobs", strings.NewReader(""))
		req.RemoteAddr = tailnetPeer + ":50000"
		req.Host = tailnetPeer + ":774"
		if method == http.MethodPost {
			req.Header.Set("Origin", "http://"+tailnetPeer+":774")
		}
		rr := httptest.NewRecorder()
		chain(store).ServeHTTP(rr, req)
		return rr
	}

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		if rr := send(sharedSpaceStore("lan"), method); rr.Code != http.StatusNoContent {
			t.Errorf("lan %s from a tailnet peer: status %d, want 204: %s", method, rr.Code, rr.Body.String())
		}
		if rr := send(sharedSpaceStore("localhost"), method); rr.Code != http.StatusForbidden {
			t.Errorf("localhost %s from a tailnet peer: status %d, want 403", method, rr.Code)
		}
	}
}

// TestSharedAddressSpaceAuthWaiverFollowsTheMode pins the waivers: on lan a
// tailnet peer is a local one to IsLocalOrPrivateRequest (the set-password
// and remove-password gates); on external it is a public one, and
// AuthMiddleware asks it for the password.
//
// THE MUTANTS: IsLocalOrPrivateRequest back to the mode-free
// `isLoopback(ip) || isPrivateIP(ip)` — the lan row fails; drop the lan guard
// in isPrivateIPFor — the external IsLocalOrPrivateRequest row fails and
// AuthMiddleware lets the external tailnet peer through without a session.
// (AuthMiddleware's own switch to isLocalIPFor is equivalent today: lan never
// requires a password, IsAuthRequired, so the waiver cannot be observed
// there. So is externalHostRefused's, which returns before the peer test off
// external/public. Both are mode-aware so that every waiver reads one rule.)
func TestSharedAddressSpaceAuthWaiverFollowsTheMode(t *testing.T) {
	req := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
		r.RemoteAddr = tailnetPeer + ":50000"
		return r
	}
	if !IsLocalOrPrivateRequest(sharedSpaceStore("lan"), req()) {
		t.Error("IsLocalOrPrivateRequest on lan = false for a tailnet peer, want true — it is a LAN client there")
	}
	if IsLocalOrPrivateRequest(sharedSpaceStore("external"), req()) {
		t.Error("IsLocalOrPrivateRequest on external = true for a tailnet peer, want false — 100.64.0.0/10 is public there")
	}

	s := NewServer(sharedSpaceStore("external"), testWSLogger{})
	reached := false
	h := s.AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req())
	if reached || rr.Code != http.StatusUnauthorized {
		t.Errorf("external + password, tailnet peer with no session: reached=%v status %d, want the 401", reached, rr.Code)
	}
}

// TestWebSocketUpgradeClassesThePeerByMode pins the upgrade's own auth-skip
// decision to the mode-aware rule AuthMiddleware uses.
//
// THE MUTANTS: leave LocalPeer unset in NewServer — the wiring rows fail;
// have HandleUpgrade ignore LocalPeer (the mode-free test alone) — the lan
// upgrade goes to AuthCheck and is refused 401.
func TestWebSocketUpgradeClassesThePeerByMode(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want bool
	}{{"lan", true}, {"external", false}} {
		lp := NewServer(sharedSpaceStore(tc.mode), testWSLogger{}).WebSocket().LocalPeer
		if lp == nil {
			t.Fatal("NewServer left WebSocketHub.LocalPeer nil — the upgrade would class a tailnet peer mode-free")
		}
		if got := lp(tailnetPeer); got != tc.want {
			t.Errorf("%s: LocalPeer(%s) = %v, want %v", tc.mode, tailnetPeer, got, tc.want)
		}
	}

	store := sharedSpaceStore("lan")
	hub := NewWebSocketHub(testWSLogger{})
	hub.ClientIP = func(*http.Request) string { return tailnetPeer }
	hub.LocalPeer = func(ip string) bool { return isLocalPeer(store, ip) }
	hub.AuthCheck = func(*http.Request) bool { return false } // a peer that reaches it is refused
	srv := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	t.Cleanup(func() {
		srv.Close()
		hub.Close()
	})
	if got := upgradeStatus(t, srv, "", "", nil); got != http.StatusSwitchingProtocols {
		t.Fatalf("status %d, want 101 — a tailnet peer on lan is local and never reaches AuthCheck", got)
	}
}
