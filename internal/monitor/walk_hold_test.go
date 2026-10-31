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

// errWalled is the error a bot-walled status probe reaches the monitors
// with, marked inconclusive as cmd/moombox's monitorProbe marks
// *youtube.StatusProbeInconclusiveError
// (TestMonitorProbeMarksOnlyAnInconclusiveProbe).
func errWalled() error {
	return InconclusiveProbe(errors.New("status probe inconclusive: android_vr refused with no broadcast details (LOGIN_REQUIRED); web_embedded escalation: refused with no broadcast details"))
}

// TestWalk_AnInconclusiveRowIsHeldOnTheLadder is the owner's W6 follow-up 1
// (2026-10-31, revised): a feed row whose probe stays inconclusive backs off
// on the existing ladder, endRecheckBackoff — an hour after its first
// inconclusive probe, six after its second, then daily — until a probe
// answers. Before, the walk re-probed it every cycle, three requests a
// probe signed out, for as long as the refusal stood, which for a
// made-for-kids video is for good. Walked every ten minutes for 56 hours on
// the bubble's clock, the row is probed at exactly 0, 1 h, 7 h, 31 h and
// 55 h; and the spell's first failure is the only one at Warn.
//
// Mutants: the walk not checking probeHeld (probed every cycle);
// noteProbeOutcome not holding on an inconclusive probe (every cycle), or
// holding without advancing the step (0, 1 h, 2 h, …).
func TestWalk_AnInconclusiveRowIsHeldOnTheLadder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db := newTestDB(t)
		t0 := time.Now()
		const chID = "UC1"
		seedRow(t, db, chID, "walled", t0.Add(-time.Hour), "exact", "rss", "unknown")

		var at []time.Duration
		probe := func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
			at = append(at, time.Since(t0))
			return nil, errWalled()
		}
		logger := &warnRecordingLogger{}
		fm := newTestFeedMonitor(t, db, withProbe(probe), withWindowDays(30))
		fm.logger = logger

		for time.Since(t0) <= 56*time.Hour {
			fm.walkForTest(t, chID)
			time.Sleep(10 * time.Minute)
		}

		want := []time.Duration{0, time.Hour, 7 * time.Hour, 31 * time.Hour, 55 * time.Hour}
		if !slices.Equal(at, want) {
			t.Errorf("probed at %v, want %v: an hour after the first inconclusive probe, then six, then daily", at, want)
		}
		if n := failedProbeWarns(logger, "walled"); n != 1 {
			t.Errorf("%d Warn lines for the row's inconclusive probes, want 1, the spell's first", n)
		}
		if got := mustGetFeedItem(t, db, chID, "walled").Status; got != "unknown" {
			t.Errorf("status = %q, want unknown: an inconclusive probe stores nothing", got)
		}
	})
}

// TestWalk_AnAnswerResetsTheLadder: an answered probe ends the hold and
// resets the ladder, so the row is probed on its own rule again from the
// next cycle — an upcoming row every cycle — and its next inconclusive
// probe holds it for an hour, not the day the old step would have. Here the
// row is inconclusive at 0 and 1 h, answers upcoming at 7 h, is probed at
// once the next cycle (7 h 10 m), inconclusive again, and next at 8 h 10 m.
//
// Mutant: noteProbeOutcome keeping the spell on an answer (the 7 h 10 m
// probe holds the row for a day, so there is no 8 h 10 m probe).
func TestWalk_AnAnswerResetsTheLadder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db := newTestDB(t)
		t0 := time.Now()
		const chID = "UC1"
		seedRow(t, db, chID, "r", t0.Add(-time.Hour), "exact", "rss", "unknown")

		var at []time.Duration
		probe := func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
			since := time.Since(t0)
			at = append(at, since)
			if since == 7*time.Hour {
				return &VideoProbeResult{StreamStatus: "upcoming", Title: videoID}, nil
			}
			return nil, errWalled()
		}
		fm := newTestFeedMonitor(t, db, withProbe(probe), withWindowDays(30))

		for time.Since(t0) <= 9*time.Hour {
			fm.walkForTest(t, chID)
			time.Sleep(10 * time.Minute)
		}

		want := []time.Duration{0, time.Hour, 7 * time.Hour, 7*time.Hour + 10*time.Minute, 8*time.Hour + 10*time.Minute}
		if !slices.Equal(at, want) {
			t.Errorf("probed at %v, want %v: the answer at 7 h resets the ladder", at, want)
		}
		if got := mustGetFeedItem(t, db, chID, "r").Status; got != "upcoming" {
			t.Errorf("status = %q, want the answer's upcoming, which no inconclusive probe after it overwrote", got)
		}
	})
}

// TestFeed_ANewlySeenVideoIsProbedAtOnce: the hold is a row's, never the
// channel's. While the wall holds a row the channel already had, a video
// RSS lists for the first time is probed in the cycle that stores it, and
// is then held on its own ladder, from its own first probe. Full feed
// cycles, ten minutes apart: "old" at 0 and 1 h; "new", first listed at
// 10 m, at 10 m and 1 h 10 m.
//
// Mutant: probeHeld holding every row of a channel any of whose rows is held
// ("new" is not probed when first seen).
func TestFeed_ANewlySeenVideoIsProbedAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db := newTestDB(t)
		t0 := time.Now()
		const chID = "UC1"
		items := []rssItem{{ID: "old", Title: "old", Published: t0.Add(-2 * time.Hour).UTC().Format(time.RFC3339)}}
		rss := func(ctx context.Context, ch *config.ChannelConfig) ([]byte, error) { return rssXML(items...), nil }

		probes := map[string][]time.Duration{}
		probe := func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
			probes[videoID] = append(probes[videoID], time.Since(t0))
			return nil, errWalled()
		}
		fm := newTestFeedMonitor(t, db, withRSS(rss), withProbe(probe), withWindowDays(30))

		for time.Since(t0) <= 90*time.Minute {
			if time.Since(t0) == 10*time.Minute {
				items = append(items, rssItem{ID: "new", Title: "new", Published: t0.Add(5 * time.Minute).UTC().Format(time.RFC3339)})
			}
			fm.runCycleForTest(t, chID)
			time.Sleep(10 * time.Minute)
		}

		if want := []time.Duration{0, time.Hour}; !slices.Equal(probes["old"], want) {
			t.Errorf("old probed at %v, want %v", probes["old"], want)
		}
		if want := []time.Duration{10 * time.Minute, 70 * time.Minute}; !slices.Equal(probes["new"], want) {
			t.Errorf("new probed at %v, want %v: at once when first seen, then on its own ladder", probes["new"], want)
		}
	})
}

// TestArchive_AHeldRowIsNotRefreshProbed: the archive's refresh probe of a
// past-content row — a vod the walk does not probe, on a channel that
// archives VODs — keeps the hold too, so a vod whose refresh probe is
// inconclusive is asked again on the ladder, not every cycle, and gets no
// job until a probe answers. Full feed cycles ten minutes apart: probed at
// 0 and 1 h.
//
// Mutant: archive refresh-probing a held row (probed every cycle).
func TestArchive_AHeldRowIsNotRefreshProbed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db := newTestDB(t)
		t0 := time.Now()
		const chID = "UC1"
		seedRow(t, db, chID, "vod1", t0.Add(-time.Hour), "exact", "rss", "vod")

		var at []time.Duration
		probe := func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
			at = append(at, time.Since(t0))
			return nil, errWalled()
		}
		fm := newTestFeedMonitor(t, db, withRSS(rssWith()), withProbe(probe), withWindowDays(30))
		found := recordVideoFound(fm)

		for time.Since(t0) <= 110*time.Minute {
			fm.runCycleForTest(t, chID)
			time.Sleep(10 * time.Minute)
		}

		if want := []time.Duration{0, time.Hour}; !slices.Equal(at, want) {
			t.Errorf("refresh-probed at %v, want %v", at, want)
		}
		if len(*found) != 0 {
			t.Errorf("jobs %v, want none: no probe answered", *found)
		}
	})
}

// TestWalk_AFailureAsThePassEndsStartsNoSpell: a probe that failed because
// the walk's context ended — its pass budget spent, or shutdown — was never
// really asked, so it neither starts a failure spell nor holds the row.
// Before, it started one, and the next cycle's real failure, the line that
// names the cause, logged at Debug; the one Warn the operator saw said
// "context canceled".
//
// Mutant: noteProbeOutcome recording an errored probe made as ctx ended (the
// row is held, and its next probe never runs; or, transient, logs at Debug).
func TestWalk_AFailureAsThePassEndsStartsNoSpell(t *testing.T) {
	db := newTestDB(t)
	now := fixedNow()
	const chID = "UC1"
	seedRow(t, db, chID, "r", now.Add(-time.Hour), "exact", "rss", "unknown")

	ctx, cancel := context.WithCancel(context.Background())
	probe := func(pctx context.Context, videoID string) (*VideoProbeResult, error) {
		if pctx.Err() == nil && ctx.Err() == nil {
			cancel()
			return nil, InconclusiveProbe(context.Canceled)
		}
		return nil, errWalled()
	}
	logger := &warnRecordingLogger{}
	fm := newTestFeedMonitor(t, db, withProbe(probe), withNow(now))
	fm.logger = logger

	ch := &config.ChannelConfig{ID: chID, Name: chID, IncludeNonLiveContent: true}
	cutoff := now.Add(-time.Duration(fm.archiveWindowDays(ch)) * 24 * time.Hour).UTC().Format(time.RFC3339)
	scope, err := db.FeedScope(chID, cutoff, now.UTC().Format(time.RFC3339), false)
	if err != nil {
		t.Fatal(err)
	}
	fm.walk(ctx, ch, chID, cutoff, now, scope)
	if fm.inFailureSpell(chID, "r") || fm.probeHeld(chID, "r", now) {
		t.Fatalf("a probe that failed as the pass ended put the row in a spell (%v) or on hold (%v)",
			fm.inFailureSpell(chID, "r"), fm.probeHeld(chID, "r", now))
	}

	warns := failedProbeWarns(logger, "r")
	fm.walkForTest(t, chID)
	if n := failedProbeWarns(logger, "r") - warns; n != 1 {
		t.Errorf("the next cycle's real failure wrote %d Warn lines, want 1: it is the spell's first", n)
	}
}

// TestWalk_ACooldownLeavesTheSpellAsItStands: a probe the cooldown
// suppressed was not asked, so it neither ends a spell nor starts one. With
// probe_cooldown set, a row whose probe keeps failing gives up at the third
// failure, which records the cooldown; the next cycle is suppressed, and
// the failure after the cooldown is still the spell's, at Debug.
//
// Mutant: noteProbeOutcome ending the spell on OutcomeCooldown (the failure
// after the cooldown warns again).
func TestWalk_ACooldownLeavesTheSpellAsItStands(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db := newTestDB(t)
		t0 := time.Now()
		const chID = "UC1"
		seedRow(t, db, chID, "r", t0.Add(-time.Hour), "exact", "rss", "unknown")

		probes := 0
		probe := func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
			probes++
			return nil, errors.New("dial tcp: connection reset by peer")
		}
		logger := &warnRecordingLogger{}
		fm := newTestFeedMonitor(t, db, withProbe(probe), withWindowDays(30))
		fm.logger = logger
		fm.ProbeCooldown.SetDuration(time.Hour)

		for range 4 { // three failures, the third giving up; then the cooldown
			fm.walkForTest(t, chID)
			time.Sleep(10 * time.Minute)
		}
		if probes != 3 {
			t.Fatalf("precondition: %d probes in four cycles, want 3, the fourth suppressed by the cooldown", probes)
		}
		time.Sleep(time.Hour)
		fm.walkForTest(t, chID)

		if probes != 4 {
			t.Fatalf("precondition: %d probes, want 4 once the cooldown ran out", probes)
		}
		if n := failedProbeWarns(logger, "r"); n != 1 {
			t.Errorf("%d Warn lines, want 1: the cooldown-suppressed cycle did not end the spell", n)
		}
	})
}

// TestWalk_TheEscalationsFailureIsNoted: the members_only escalation's own
// probe is recorded against the row's spell like any probe. The anonymous
// probe's members_only refusal is an answer and ends any spell; the
// authenticated escalation that follows it the same cycle fails, and that
// starts a spell, so the next cycle's authenticated probe of the relabelled
// row, failing too, is its repeat, at Debug. An inconclusive escalation
// holds the row, as any inconclusive probe does.
//
// Mutant: probeRow not noting the escalation's outcome (the next cycle's
// failure warns again; the inconclusive row is probed the next cycle).
func TestWalk_TheEscalationsFailureIsNoted(t *testing.T) {
	db := newTestDB(t)
	now := fixedNow()
	const chID = "UC1"
	seedRow(t, db, chID, "flaky", now.Add(-1*time.Hour), "exact", "rss", "unknown")
	seedRow(t, db, chID, "walled", now.Add(-2*time.Hour), "exact", "rss", "unknown")

	var authCalls []string
	anon := func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		return &VideoProbeResult{StreamStatus: "not_a_stream", PlayabilityError: "members_only"}, nil
	}
	auth := func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		authCalls = append(authCalls, videoID)
		if videoID == "walled" {
			return nil, errWalled()
		}
		return nil, errors.New("dial tcp: connection reset by peer")
	}
	logger := &warnRecordingLogger{}
	fm := newTestFeedMonitor(t, db, withProbe(anon), withProbeAuth(auth), withMembership(membWith()), withNow(now))
	fm.logger = logger

	fm.walkForTest(t, chID)
	fm.walkForTest(t, chID)

	if !slices.Equal(authCalls, []string{"flaky", "walled", "flaky"}) {
		t.Errorf("authenticated probes %v, want [flaky walled flaky]: the walled row's inconclusive escalation holds it", authCalls)
	}
	if n := failedProbeWarns(logger, "flaky"); n != 1 {
		t.Errorf("flaky: %d Warn lines, want 1: the escalation's failure started the spell", n)
	}
}
