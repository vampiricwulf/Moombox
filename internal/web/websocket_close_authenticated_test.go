package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestCloseAuthenticatedClosesOnlySocketsThatAuthenticated: a socket's
// credential is checked once, at the upgrade, so revoking it has to close the
// socket. A local peer never presented one, and a loopback dashboard must not
// be disconnected because a remote device was revoked.
//
// Mutants: close every client: the local socket's read fails; never set
// wsClient.authenticated at the upgrade: nothing is closed and the count is 0.
func TestCloseAuthenticatedClosesOnlySocketsThatAuthenticated(t *testing.T) {
	hub := NewWebSocketHub(testWSLogger{})
	hub.InitialState = func() map[string]any { return map[string]any{} }
	hub.AuthCheck = func(*http.Request) bool { return true }
	var local atomic.Bool
	hub.LocalPeer = func(string) bool { return local.Load() }
	srv := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	t.Cleanup(func() { srv.Close(); hub.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dial := func(isLocal bool) *websocket.Conn {
		t.Helper()
		local.Store(isLocal)
		conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { conn.CloseNow() })
		if _, _, err := conn.Read(ctx); err != nil { // initial_state
			t.Fatalf("read initial_state: %v", err)
		}
		return conn
	}
	localConn := dial(true)
	remoteConn := dial(false)

	if got := hub.CloseAuthenticated(); got != 1 {
		t.Errorf("CloseAuthenticated closed %d sockets, want 1", got)
	}
	if got := hub.ClientCount(); got != 1 {
		t.Errorf("%d clients left in the hub, want the local one", got)
	}

	hub.Broadcast("connectivity", map[string]any{"online": true})
	if _, b, err := localConn.Read(ctx); err != nil || !strings.Contains(string(b), "connectivity") {
		t.Errorf("the local socket did not get the next broadcast: %q, %v", b, err)
	}
	if _, b, err := remoteConn.Read(ctx); err == nil {
		t.Errorf("the authenticated socket is still open and read %q", b)
	}
}

// TestAnUpgradeAuthenticatedBeforeARevokeIsNotRegistered: an upgrade passes
// AuthCheck and joins the hub some milliseconds later. A revoke that ran in
// between took its list of sockets without this one, so the socket stayed
// open on the revoked credential for as long as its tab did. Here the revoke
// lands inside AuthCheck, after it has decided to accept.
//
// Mutant: register without comparing the generation: the client joins the
// hub and reads its initial_state.
func TestAnUpgradeAuthenticatedBeforeARevokeIsNotRegistered(t *testing.T) {
	hub := NewWebSocketHub(testWSLogger{})
	hub.InitialState = func() map[string]any { return map[string]any{} }
	hub.LocalPeer = func(string) bool { return false }
	var revokeDuringCheck atomic.Bool
	revokeDuringCheck.Store(true)
	hub.AuthCheck = func(*http.Request) bool {
		if revokeDuringCheck.Load() {
			hub.CloseAuthenticated()
		}
		return true
	}
	srv := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	t.Cleanup(func() { srv.Close(); hub.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(srv.URL, "http")

	conn, _, err := websocket.Dial(ctx, url, nil)
	if err == nil {
		defer conn.CloseNow()
		if _, b, err := conn.Read(ctx); err == nil {
			t.Errorf("a socket authenticated before the revoke was served %q", b)
		} else if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
			t.Errorf("the socket ended with %v, want a policy-violation close", err)
		}
	}
	if got := hub.ClientCount(); got != 0 {
		t.Errorf("%d clients in the hub, want none", got)
	}

	// The browser reconnects; with no revoke in the way it is let in.
	revokeDuringCheck.Store(false)
	again, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("the reconnect was refused: %v", err)
	}
	defer again.CloseNow()
	if _, _, err := again.Read(ctx); err != nil {
		t.Errorf("the reconnect got no initial_state: %v", err)
	}
}
