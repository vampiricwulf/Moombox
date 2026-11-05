package monitor

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// errRateLimited is the error a status probe the session's rate limit
// refused reaches the monitors with, marked as cmd/moombox's monitorProbe
// marks it (TestMonitorProbeMarksARateLimitDistinctly).
func errRateLimited() error {
	return RateLimitedProbe(errors.New("status probe inconclusive: android_vr refused with no broadcast details (rate_limited); not escalated: the session is rate-limited"))
}

// TestWalk_ARateLimitedRowWaitsForTheHoldNotTheLadder is owner decision 8 of
// wave FC: a probe the session's rate limit refused does not climb the row's
// inconclusive ladder. While the backlog's rate-limit hold stands the walk
// skips the row's re-check; once the hold has run out it asks every cycle.
// Walked every ten minutes on the bubble's clock with the hold standing until
// 1 h, the row is probed at 0 (no spell yet), then at 1 h and every cycle
// after, where the ladder would have asked at 0, 1 h and 7 h.
//
// Mutants (each run): probeHeld not honouring the rate-limit hold (probed
// every cycle from 10 m); noteProbeOutcome holding a rate-limited probe on
// the ladder (probed at 0 and 1 h only); probeHeld holding a rate-limited
// row whatever the hold says (never probed after 0); probeAndClassify not
// reading ErrProbeRateLimited (the ladder's 0 and 1 h).
func TestWalk_ARateLimitedRowWaitsForTheHoldNotTheLadder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db := newTestDB(t)
		t0 := time.Now()
		const chID = "UC1"
		seedRow(t, db, chID, "limited", t0.Add(-time.Hour), "exact", "rss", "unknown")

		var at []time.Duration
		probe := func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
			at = append(at, time.Since(t0))
			return nil, errRateLimited()
		}
		fm := newTestFeedMonitor(t, db, withProbe(probe), withWindowDays(30))
		holdUntil := t0.Add(time.Hour)
		fm.RateLimitHeld = func(now time.Time) bool { return now.Before(holdUntil) }

		for time.Since(t0) <= 90*time.Minute {
			fm.walkForTest(t, chID)
			time.Sleep(10 * time.Minute)
		}

		want := []time.Duration{0, time.Hour, 70 * time.Minute, 80 * time.Minute, 90 * time.Minute}
		if !slices.Equal(at, want) {
			t.Errorf("probed at %v, want %v: skipped while the rate-limit hold stands, then every cycle", at, want)
		}
		if got := mustGetFeedItem(t, db, chID, "limited").Status; got != "unknown" {
			t.Errorf("status = %q, want unknown: a rate-limited probe stores nothing", got)
		}
	})
}

// TestWalk_ARateLimitDoesNotAdvanceTheLadder: rate-limited probes neither
// start nor advance a row's hold, so a bot wall met after them holds the row
// for the ladder's first step, an hour. No rate-limit hold stands (nil, as
// with no scheduler wired), so the rate-limited row is asked every cycle:
// rate-limited at 0, 10 m and 20 m, walled at 30 m, next at 1 h 30 m.
//
// Mutant: noteProbeOutcome holding a rate-limited probe on the ladder (the
// row is held from its first probe: 0 and 1 h only).
func TestWalk_ARateLimitDoesNotAdvanceTheLadder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db := newTestDB(t)
		t0 := time.Now()
		const chID = "UC1"
		seedRow(t, db, chID, "r", t0.Add(-time.Hour), "exact", "rss", "unknown")

		var at []time.Duration
		probe := func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
			since := time.Since(t0)
			at = append(at, since)
			if since < 30*time.Minute {
				return nil, errRateLimited()
			}
			return nil, errWalled()
		}
		fm := newTestFeedMonitor(t, db, withProbe(probe), withWindowDays(30))

		for time.Since(t0) <= 100*time.Minute {
			fm.walkForTest(t, chID)
			time.Sleep(10 * time.Minute)
		}

		want := []time.Duration{0, 10 * time.Minute, 20 * time.Minute, 30 * time.Minute, 90 * time.Minute}
		if !slices.Equal(at, want) {
			t.Errorf("probed at %v, want %v: the rate-limited probes leave the ladder at its first step", at, want)
		}
	})
}

// TestProbeAndClassifyMarksARateLimitedProbe: the classification carries the
// rate limit apart from the generic inconclusive case (it is both), and a
// bot wall is not rate-limited.
//
// Mutant: probeAndClassify not reading ErrProbeRateLimited (RateLimited
// false for the rate-limited row).
func TestProbeAndClassifyMarksARateLimitedProbe(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		err                      error
		wantInconclusive, wantRL bool
	}{
		{"a rate limit", errRateLimited(), true, true},
		{"a bot wall", errWalled(), true, false},
		{"a transport failure", errors.New("connection reset"), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := probeAndClassify(ProbeClassifyParams{
				VideoID:    "v",
				Channel:    &config.ChannelConfig{Name: "c"},
				ProbeVideo: func(context.Context, string) (*VideoProbeResult, error) { return nil, tc.err },
				Tracker:    NewMetadataFailureTracker(),
				Logger:     silentLogger{},
			})
			if res.Outcome != OutcomeErrored || res.Inconclusive != tc.wantInconclusive || res.RateLimited != tc.wantRL {
				t.Errorf("result = %+v, want errored, inconclusive %v, rate-limited %v", res, tc.wantInconclusive, tc.wantRL)
			}
		})
	}
}
