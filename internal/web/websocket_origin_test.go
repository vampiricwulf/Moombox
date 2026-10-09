package web

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
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
	hub.OriginCheck = func(r *http.Request) (bool, string) {
		return originAllowed(store, r, r.Header.Get("Origin"))
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

// TestWebSocketUpgradeHoldsALoopbackOriginToItsPort pins D-S7 on the socket:
// on lan (and localhost) a loopback or private origin is held to the port the
// upgrade was addressed to, so a page another local service serves cannot
// open the live stream.
//
// THE MUTANTS: drop `&& originPortServed(...)` from isAllowedOrigin's lan arm —
// the first row answers 101; apply samePort's two-portless leniency whatever
// browserSchemeUnknown says — the portless row answers 101 (a page another
// local service serves on https:443 opening the socket of a dashboard
// addressed on plain 80; loopback is exempt from mixed-content blocking).
func TestWebSocketUpgradeHoldsALoopbackOriginToItsPort(t *testing.T) {
	srv := wsOriginFixture(t, "lan", nil)
	host := strings.TrimPrefix(srv.URL, "http://")
	_, port, _ := strings.Cut(host, ":")

	other := "1"
	if port == other {
		other = "2"
	}
	if got := upgradeStatus(t, srv, "", "http://127.0.0.1:"+other, nil); got != http.StatusForbidden {
		t.Fatalf("status %d, want 403 — a loopback page on another port is another program", got)
	}
	if got := upgradeStatus(t, srv, "", "http://localhost:"+port, nil); got != http.StatusSwitchingProtocols {
		t.Fatalf("status %d, want 101 — the dashboard's own port, by either loopback spelling", got)
	}
	if got := upgradeStatus(t, srv, "127.0.0.1", "https://127.0.0.1", nil); got != http.StatusForbidden {
		t.Fatalf("status %d, want 403 — a portless Host over plain HTTP is port 80, and an https page is 443", got)
	}
	if got := upgradeStatus(t, srv, "127.0.0.1", "http://localhost", nil); got != http.StatusSwitchingProtocols {
		t.Fatalf("status %d, want 101 — a portless Host over plain HTTP is port 80, as is an http page", got)
	}
}

// THE MUTANT: leave OriginCheck nil in NewServer — every test above still
// passes (they wire the hook themselves) while the real server refuses every
// browser upgrade.
func TestNewServerWiresTheWebSocketOriginCheck(t *testing.T) {
	s := NewServer(config.NewStore(config.Defaults(), ""), testWSLogger{})
	if s.WebSocket().OriginCheck == nil {
		t.Fatal("NewServer left WebSocketHub.OriginCheck nil — the upgrade would refuse every browser origin")
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

// TestWebSocketUpgradeRefusalLogsTheEffectiveHost pins fix-round-1 item 7
// (Task 3 review Finding 1): the refusal line must name the host the
// decision actually compared — the effective host (X-Forwarded-Host from a
// trusted proxy) — not the raw r.Host. On exactly the trusted-proxy
// deployment this task exists to fix, r.Host is a red herring for an
// operator diagnosing a 403; the CSRF twin's refusal line already names
// comparedHost, not r.Host (middleware.go).
//
// THE MUTANT: log r.Host instead of OriginCheck's returned comparedHost — the
// logged host reverts to "internal:774" and the assertion fails.
func TestWebSocketUpgradeRefusalLogsTheEffectiveHost(t *testing.T) {
	store := config.NewStore(&config.MoomboxConfig{
		Network: config.NetworkConfig{
			NetworkAccess:  "external",
			TrustedProxies: []string{"127.0.0.1"},
		},
	}, "")
	logger := &recordingLogger{}
	hub := NewWebSocketHub(logger)
	hub.OriginCheck = func(r *http.Request) (bool, string) {
		return originAllowed(store, r, r.Header.Get("Origin"))
	}
	srv := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	t.Cleanup(func() {
		srv.Close()
		hub.Close()
	})

	got := upgradeStatus(t, srv, "internal:774", "http://attacker.example",
		map[string]string{"X-Forwarded-Host": "dash.example"})
	if got != http.StatusForbidden {
		t.Fatalf("status %d, want 403 — attacker.example does not match the forwarded host", got)
	}

	if len(logger.warns) != 1 {
		t.Fatalf("logged %v, want exactly one refusal line", logger.warns)
	}
	if !strings.Contains(logger.warns[0], "host=dash.example") {
		t.Fatalf("logged %q, want it to name the forwarded host dash.example, not r.Host (internal:774)", logger.warns[0])
	}
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
	// The test client is the listed proxy: a TLS-terminating one on :443
	// forwards the name portless while the hop to Moombox is plain HTTP
	// (browserSchemeUnknown), so the port rule holds and the name alone
	// decides.
	srv := wsOriginFixture(t, "lan", []string{"127.0.0.1"})
	if got := upgradeStatus(t, srv, "dash.lan", "https://dash.lan", nil); got != http.StatusSwitchingProtocols {
		t.Fatalf("status %d, want 101 — a literal certificate-attested SAN must widen the lan upgrade too", got)
	}
}
