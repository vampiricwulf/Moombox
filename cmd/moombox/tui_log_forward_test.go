package main

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/logger"
)

// TestTUILogForwarderSkipsWhatTheBackfillHolds is W24-14's TUI half. The TUI
// subscribes to the logger before it reads the ring for the log panel's
// backfill, so no line falls between the two — and a line logged in between,
// which at startup is any line another goroutine logs while the TUI is being
// wired, is in the backfill AND comes through the subscription. The panel
// showed it twice. The real logger and the production forwarder here, in the
// order tui_wiring.go takes them.
//
// Mutant this kills: dropping forwardTUILogs' `line.Seq <= backfillSeq` skip —
// the line logged between the two reads is on the panel twice.
func TestTUILogForwarderSkipsWhatTheBackfillHolds(t *testing.T) {
	log, err := logger.New("", "info", 1<<20, 1)
	if err != nil {
		t.Fatalf("logger.New: %v", err)
	}
	log.SuppressStdout()

	log.Info("before the TUI")
	sub := log.SubscribeLines()
	log.Info("between the subscription and the backfill")
	backfill, backfillSeq := log.RecentLines()
	log.Info("after the backfill")
	log.Close() // closes sub, which ends the forwarder

	logCh := make(chan string, 10)
	var dropped atomic.Int64
	forwardTUILogs(context.Background(), sub, backfillSeq, logCh, &dropped)
	close(logCh)

	panel := append([]string(nil), backfill...)
	for line := range logCh {
		panel = append(panel, line)
	}
	for _, want := range []string{"before the TUI", "between the subscription and the backfill", "after the backfill"} {
		n := 0
		for _, line := range panel {
			if strings.HasSuffix(line, want) {
				n++
			}
		}
		if n != 1 {
			t.Errorf("the log panel shows %q %d times, want once: %q", want, n, panel)
		}
	}
	if d := dropped.Load(); d != 0 {
		t.Errorf("%d lines counted as dropped with room in the channel", d)
	}
}

// A full TUI channel drops the line and counts it, as the inline forwarder
// did; the skip does not change that.
func TestTUILogForwarderCountsWhatAFullChannelDrops(t *testing.T) {
	sub := make(chan logger.Line, 3)
	sub <- logger.Line{Seq: 10, Text: "first"}
	sub <- logger.Line{Seq: 11, Text: "second"}
	sub <- logger.Line{Seq: 12, Text: "third"}
	close(sub)

	logCh := make(chan string, 1)
	var dropped atomic.Int64
	forwardTUILogs(context.Background(), sub, 0, logCh, &dropped)

	if got := <-logCh; got != "first" {
		t.Errorf("the channel holds %q, want the first line", got)
	}
	if d := dropped.Load(); d != 2 {
		t.Errorf("%d lines counted as dropped, want 2", d)
	}
}
