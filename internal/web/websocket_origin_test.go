package web

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// wsOriginFixture starts a server whose only handler is the upgrade, wired to
// the same Origin decision the middleware chain uses.
func wsOriginFixture(t *testing.T, networkAccess string, trusted []string) *httptest.Server {
	t.Helper()
	store := config.NewStore(&config.MoomboxConfig{
		Network: config.NetworkConfig{
			NetworkAccess:  networkAccess,
			TrustedProxies: trusted,
		},
	}, "")
	hub := NewWebSocketHub(testWSLogger{})
	hub.OriginCheck = func(r *http.Request) bool {
		ok, _ := originAllowed(store, r, r.Header.Get("Origin"))
		return ok
	}
	srv := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	t.Cleanup(func() {
		srv.Close()
		hub.Close()
	})
	return srv
}

// upgradeStatus performs one raw WebSocket handshake and returns the status the
// server answered with. Written by hand rather than with websocket.Dial because
// these rows must control the Host header, which net/http takes from req.Host
// and DialOptions does not expose.
func upgradeStatus(t *testing.T, srv *httptest.Server, host, origin string, hdr map[string]string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if host != "" {
		req.Host = host
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", base64.StdEncoding.EncodeToString(make([]byte, 16)))
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// TestWebSocketUpgradeSharesTheOriginDecision pins the four ways the upgrade
// used to differ from the CSRF check.
//
// THE MUTANTS: restoring OriginPatterns in place of InsecureSkipVerify lets
// the library's unconditional "Origin == Host" arm (accept.go
// authenticateOrigin) answer the rebinding row with 101; deriving the host
// from r.Host instead of effectiveRequestHost fails the forwarded row;
// restoring the old `hostname+":*"` pattern fails the port row; dropping the
// `origin != ""` guard fails the header-less row and breaks every non-browser
// client.
func TestWebSocketUpgradeSharesTheOriginDecision(t *testing.T) {
	t.Run("a rebinding page is refused even though Origin equals Host", func(t *testing.T) {
		srv := wsOriginFixture(t, "lan", nil)
		if got := upgradeStatus(t, srv, "attacker.dns", "http://attacker.dns", nil); got != http.StatusForbidden {
			t.Fatalf("status %d, want 403 — the library would accept this pair on its own", got)
		}
	})

	t.Run("a trusted proxy's forwarded host is the one compared", func(t *testing.T) {
		srv := wsOriginFixture(t, "external", []string{"127.0.0.1"})
		got := upgradeStatus(t, srv, "internal:774", "http://dash.example",
			map[string]string{"X-Forwarded-Host": "dash.example"})
		if got != http.StatusSwitchingProtocols {
			t.Fatalf("status %d, want 101 — the forwarded host matches the Origin", got)
		}
	})

	t.Run("ports are compared exactly", func(t *testing.T) {
		srv := wsOriginFixture(t, "external", nil)
		if got := upgradeStatus(t, srv, "dash.example:774", "http://dash.example:99", nil); got != http.StatusForbidden {
			t.Fatalf("status %d, want 403 — the ports differ", got)
		}
	})

	t.Run("a request with no Origin header is still accepted", func(t *testing.T) {
		srv := wsOriginFixture(t, "external", nil)
		if got := upgradeStatus(t, srv, "attacker.dns", "", nil); got != http.StatusSwitchingProtocols {
			t.Fatalf("status %d, want 101 — non-browser clients send no Origin", got)
		}
	})
}

// THE MUTANT: leave OriginCheck nil in NewServer — every test above still
// passes (they wire the hook themselves) while the real server accepts
// everything.
func TestNewServerWiresTheWebSocketOriginCheck(t *testing.T) {
	s := NewServer(config.NewStore(config.Defaults(), ""), testWSLogger{})
	if s.WebSocket().OriginCheck == nil {
		t.Fatal("NewServer left WebSocketHub.OriginCheck nil — the upgrade would accept any origin")
	}
}

// TestWebSocketUpgradeFailsClosedWithNoOriginCheck pins fix-round-1 item 3
// (Task 3 Concern 2): a hub whose OriginCheck was never wired must REFUSE an
// Origin-bearing upgrade, not accept it. NewServer always wires the real
// check (TestNewServerWiresTheWebSocketOriginCheck above); only a hub built
// outside it — today, only test harnesses — can reach this path.
//
// THE MUTANT: restore the pre-fix-round-1 condition
// (`hub.OriginCheck != nil && !hub.OriginCheck(r)`), which treats a nil check
// as "accept" — the first subtest starts answering 101 instead of 403.
func TestWebSocketUpgradeFailsClosedWithNoOriginCheck(t *testing.T) {
	newNilCheckFixture := func(t *testing.T) *httptest.Server {
		t.Helper()
		hub := NewWebSocketHub(testWSLogger{})
		// OriginCheck deliberately left nil — the point of the test.
		srv := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
		t.Cleanup(func() {
			srv.Close()
			hub.Close()
		})
		return srv
	}

	t.Run("an Origin-bearing upgrade is refused", func(t *testing.T) {
		srv := newNilCheckFixture(t)
		if got := upgradeStatus(t, srv, "dash.example", "http://attacker.example", nil); got != http.StatusForbidden {
			t.Fatalf("status %d, want 403 — a nil OriginCheck must fail CLOSED", got)
		}
	})

	t.Run("a request with no Origin header is unaffected", func(t *testing.T) {
		srv := newNilCheckFixture(t)
		if got := upgradeStatus(t, srv, "dash.example", "", nil); got != http.StatusSwitchingProtocols {
			t.Fatalf("status %d, want 101 — no Origin header means no Origin check at all", got)
		}
	})
}

// TestWebSocketUpgradeReachesTheCertificateSANWidening pins fix-round-1 item 4
// (Task 3 Concern 3): the localhost/lan certificate-SAN widening
// (isAllowedOrigin's identity argument, hostInSANs) is exercised on the
// upgrade path too, not only inside isAllowedOrigin's own unit tests.
//
// THE MUTANT: have wsOriginFixture's hook ignore identity — e.g. call
// isAllowedOrigin with a nil/empty identity instead of routing through
// originAllowed — and this row starts refusing the literal certificate name.
func TestWebSocketUpgradeReachesTheCertificateSANWidening(t *testing.T) {
	useIdentityCert(t, certWatcherFor(t, "dash.lan", []string{"dash.lan"}, nil))
	srv := wsOriginFixture(t, "lan", nil)
	if got := upgradeStatus(t, srv, "", "https://dash.lan", nil); got != http.StatusSwitchingProtocols {
		t.Fatalf("status %d, want 101 — a literal certificate-attested SAN must widen the lan upgrade too", got)
	}
}
