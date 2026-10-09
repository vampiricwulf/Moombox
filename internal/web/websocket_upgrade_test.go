package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestBroadcastsDuringTheSnapshotFollowIt: the write loop used to start before
// the initial snapshot was built, so a broadcast issued meanwhile could reach
// the client first — and a job_deleted that overtook a snapshot taken just
// before the delete let the snapshot restore the deleted row. Broadcasts from
// registration on now wait until the snapshot is written.
//
// Mutant: start writePump before sendInitialState again — job_deleted arrives
// first.
func TestBroadcastsDuringTheSnapshotFollowIt(t *testing.T) {
	hub := NewWebSocketHub(testWSLogger{})
	hub.InitialState = func() map[string]any {
		// A delete commits while the snapshot is being built.
		hub.Broadcast("job_deleted", map[string]any{"id": "x"})
		time.Sleep(50 * time.Millisecond) // give a running write loop its chance
		return map[string]any{"jobs": []any{}}
	}
	srv := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	t.Cleanup(func() { srv.Close(); hub.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	var types []string
	for range 2 {
		_, b, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v (got %v so far)", err, types)
		}
		var m WSMessage
		json.Unmarshal(b, &m)
		types = append(types, m.Type)
	}
	if types[0] != "initial_state" || types[1] != "job_deleted" {
		t.Errorf("frames = %v, want [initial_state job_deleted]", types)
	}
}

// TestUpgradePanicIsLoggedAndTheClientRemoved: the upgrade runs outside the
// router's RecoveryMiddleware, so a panic building the snapshot vanished into
// net/http's discarded ErrorLog and left the client registered with no reader
// or pinger. It is logged and the client removed.
//
// Mutant: drop the recover in HandleUpgrade — no Error is logged and the hub
// still counts the client.
func TestUpgradePanicIsLoggedAndTheClientRemoved(t *testing.T) {
	log := &countingWSLogger{}
	hub := NewWebSocketHub(log)
	hub.InitialState = func() map[string]any { panic("provider exploded") }
	srv := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	t.Cleanup(func() { srv.Close(); hub.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err == nil {
		defer conn.CloseNow()
		conn.Read(ctx) // the server closes it
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		hub.mu.Lock()
		n := len(hub.clients)
		hub.mu.Unlock()
		log.mu.Lock()
		errs := log.errors
		log.mu.Unlock()
		if n == 0 && errs > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after an upgrade panic: %d client(s) registered, %d Error line(s); want 0 and at least 1", n, errs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// upgradeLineLogger is panicLineLogger with the rest of the hub's logger.
type upgradeLineLogger struct{ panicLineLogger }

func (*upgradeLineLogger) Debug(string, ...any) {}
func (*upgradeLineLogger) Info(string, ...any)  {}
func (*upgradeLineLogger) Warn(string, ...any)  {}

// upgradeAuthThatPanics is the bug an operator has to find from the line.
func upgradeAuthThatPanics(*http.Request) bool { panic("auth store exploded") }

// TestUpgradePanicLineSaysWhereItPanicked is W24-15 for the one request
// RecoveryMiddleware never sees: interceptUpgrades hands the dashboard's
// WebSocket upgrade to HandleUpgrade ahead of the router, so its own recover
// stands in, and it logged the panic value and the peer with nothing saying
// where — a panic in the DB-backed AuthCheck or the snapshot provider could
// not be located from its log. It carries the same one-line, bounded,
// argument-free stack (panicStack; TestRecoveryLogsTheStackThatPanicked pins
// its shape).
//
// Mutant: the "stack" field dropped from HandleUpgrade's Error — no stack.
func TestUpgradePanicLineSaysWhereItPanicked(t *testing.T) {
	log := &upgradeLineLogger{}
	hub := NewWebSocketHub(log)
	hub.AuthCheck = upgradeAuthThatPanics
	req := httptest.NewRequest(http.MethodGet, "/?token=QUERY-SECRET", nil)
	req.RemoteAddr = "203.0.113.9:4000" // a public peer, so AuthCheck runs
	req.Header.Set("Upgrade", "websocket")
	rr := httptest.NewRecorder()
	hub.HandleUpgrade(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status %d, want 500", rr.Code)
	}
	if log.fields == nil {
		t.Fatal("the upgrade panic was not logged")
	}
	stack, _ := log.fields["stack"].(string)
	if !strings.HasPrefix(stack, "github.com/vampiricwulf/Moombox/internal/web.upgradeAuthThatPanics (websocket_upgrade_test.go:") {
		t.Errorf("the panic line does not start its stack at the function that panicked:\n%s", log.line)
	}
	if !strings.Contains(stack, "HandleUpgrade") {
		t.Errorf("the stack does not name the upgrade that called it: %s", stack)
	}
	if strings.Contains(log.line, "QUERY-SECRET") {
		t.Errorf("the panic line carries the request's query string: %s", log.line)
	}
}
