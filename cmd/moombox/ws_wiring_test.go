package main

import (
	"os"
	"strings"
	"testing"
)

// TestInitialStateStillSuppliesTheLogs pins the one consumer of the hub's
// deleted log ring.
//
// WEB-14 removed WebSocketHub's own logBuf and the `"logs"` fallback
// initialStateBytes used to add when the provider omitted the key
// (internal/web/websocket.go). That fallback was dead in production — this
// provider has always supplied the logger's own ring — but it did mean the key
// had a second source. Now this map literal is the ONLY one, and nothing else
// in the tree would notice it going away: the client reads
// `payload.logs` defensively, so the log panel would simply be empty on connect
// with every Go and node test still green.
//
// Structural, deliberately: wireWebSocket takes a fully built runState (a live
// database, a config store, three monitors, the connectivity monitor and the
// backfill map), so calling it is a different test at a different cost.
//
// THE MUTANT: delete the `"logs": s.log.GetRecentLines(),` line — the log panel
// is empty until the first broadcast line arrives after connect.
func TestInitialStateStillSuppliesTheLogs(t *testing.T) {
	src, err := os.ReadFile("ws_wiring.go")
	if err != nil {
		t.Fatalf("read ws_wiring.go: %v", err)
	}
	text := strings.ReplaceAll(string(src), "\r\n", "\n")

	if !strings.Contains(text, `"logs":`) {
		t.Error(`the InitialState provider no longer puts a "logs" key in the payload — the hub's own ` +
			"ring was deleted with WEB-14, so this map is the only source left and the log panel " +
			"comes up empty on connect")
	}
	if !strings.Contains(text, "s.log.GetRecentLines()") {
		t.Error("the InitialState payload does not read the logger's ring (GetRecentLines) — that ring " +
			"is what replaced the hub's copy")
	}
}
