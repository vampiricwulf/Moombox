package tui

import (
	"testing"
	"time"
)

// TestChordStateExpired pins the two windows: a prefix waits chordWindow
// from prefixTime, a confirm waits chordWindow from actionTime (not from the
// prefix), and an idle chord never expires.
//
// Mutants: measure the confirm from prefixTime (the "confirm fresh" row
// expires), or swap After for Before (every row flips).
func TestChordStateExpired(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	const eps = 10 * time.Millisecond
	cases := []struct {
		name  string
		c     chordState
		wantX bool
	}{
		{"idle", chordState{}, false},
		{"idle with stale times", chordState{prefixTime: now.Add(-time.Hour), actionTime: now.Add(-time.Hour)}, false},
		{"prefix just inside", chordState{prefix: "a", prefixTime: now.Add(-chordWindow + eps)}, false},
		{"prefix just past", chordState{prefix: "a", prefixTime: now.Add(-chordWindow - eps)}, true},
		{"confirm fresh, old prefix", chordState{prefix: "a", prefixTime: now.Add(-2 * chordWindow), action: "d", actionTime: now.Add(-chordWindow + eps)}, false},
		{"confirm just past", chordState{prefix: "a", prefixTime: now.Add(-2 * chordWindow), action: "d", actionTime: now.Add(-chordWindow - eps)}, true},
	}
	for _, tc := range cases {
		if got := tc.c.expired(now); got != tc.wantX {
			t.Errorf("%s: expired = %v, want %v", tc.name, got, tc.wantX)
		}
	}
}

// TestTickDisarmsAnExpiredConfirm: the tick retires a confirm whose window
// ran out, so a D that follows does not dispatch.
func TestTickDisarmsAnExpiredConfirm(t *testing.T) {
	a, deleted := chordDisarmApp(t)
	armDelete(t, a)
	a.chord.actionTime = time.Now().Add(-chordWindow - time.Second)
	a.Update(tickMsg{})
	if a.chord.prefix != "" {
		t.Fatalf("expired confirm survived the tick: %+v", a.chord)
	}
	pressRun(a, keyMsg("d"))
	if len(*deleted) != 0 {
		t.Errorf("D after an expired confirm deleted %v", *deleted)
	}
}
