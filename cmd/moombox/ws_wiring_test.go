package main

import (
	"os"
	"regexp"
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
// The ring's read also yields the number of its newest line, which the payload
// must carry as logSeq: the client joined the hub before this read, so a line
// logged in between is in the snapshot AND arrives as a log frame, and logSeq
// is what the dashboard skips those frames by (W24-14). The read and the
// number have to be one call — two reads could disagree.
//
// THE MUTANTS: delete the `"logs": logs,` line — the log panel is empty until
// the first broadcast line arrives after connect; delete `"logSeq": logSeq,`
// — every line logged during the connect shows twice; read the lines through
// GetRecentLines and the number separately — the snapshot and its number
// stop being one read.
func TestInitialStateStillSuppliesTheLogs(t *testing.T) {
	src, err := os.ReadFile("ws_wiring.go")
	if err != nil {
		t.Fatalf("read ws_wiring.go: %v", err)
	}
	text := strings.ReplaceAll(string(src), "\r\n", "\n")

	if !regexp.MustCompile(`"logs":\s+logs,`).MatchString(text) {
		t.Error(`the InitialState provider no longer puts the ring's lines under a "logs" key — the ` +
			"hub's own ring was deleted with WEB-14, so this map is the only source left and the log " +
			"panel comes up empty on connect")
	}
	if !regexp.MustCompile(`"logSeq":\s+logSeq,`).MatchString(text) {
		t.Error(`the InitialState payload no longer carries "logSeq" — without it the dashboard cannot ` +
			"tell a log frame for a line the snapshot holds from a new one, and shows it twice")
	}
	if !strings.Contains(text, "logs, logSeq := s.log.RecentLines()") {
		t.Error("the InitialState payload does not take its lines and their number from ONE read of the " +
			"logger's ring (RecentLines) — that ring is what replaced the hub's copy, and a second read " +
			"could number a different set of lines")
	}
}

// TestInitialStateCarriesTheRunningTrims: a dashboard trim runs detached from
// the page that started it, so a page reloaded mid-trim learns that it is
// still running — and draws its progress bar — only from initial_state's
// runningTrims, seeded from the trim service. Structural, for the reason
// TestInitialStateStillSuppliesTheLogs gives.
//
// THE MUTANTS: put the list under another key (`"trims":`), or leave it
// empty instead of reading s.trimSvc.RunningTrims() — a reload mid-trim shows
// no trim running until its outcome arrives.
func TestInitialStateCarriesTheRunningTrims(t *testing.T) {
	src, err := os.ReadFile("ws_wiring.go")
	if err != nil {
		t.Fatalf("read ws_wiring.go: %v", err)
	}
	text := strings.ReplaceAll(string(src), "\r\n", "\n")
	if !strings.Contains(text, `"runningTrims":`) {
		t.Error(`the InitialState provider no longer puts a "runningTrims" key in the payload — a ` +
			"dashboard reloaded mid-trim no longer shows the trim it started running")
	}
	if !strings.Contains(text, "s.trimSvc.RunningTrims()") {
		t.Error("the InitialState payload does not read the trim service's RunningTrims")
	}
}
