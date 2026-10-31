package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// failedProbeWarns counts the Warn lines probeAndClassify wrote for videoID's
// failed probes.
func failedProbeWarns(l *warnRecordingLogger, videoID string) int {
	n := 0
	for _, w := range l.warnings {
		if strings.HasPrefix(w, "[Monitor] Failed to check metadata for "+videoID+" ") {
			n++
		}
	}
	return n
}

// TestWalk_AProbeThatKeepsFailingWarnsOncePerSpell is the owner's W6
// follow-up (2026-10-31): the feed walk warns a row's failed probe once a
// spell, as DECAPI warns once per newest video
// (TestDecapi_AProbeThatKeepsFailingWarnsOncePerNewestVideo). An
// inconclusive status probe under a lasting bot wall stores nothing, so the
// row is probed again every cycle, and every one of those probes logged a
// Warn. Now the first failed probe since the row last answered warns, every
// one after it logs at Debug, and an answer ends the spell, so the next
// failure warns again.
//
// Two rows over four cycles: "walled" fails every cycle (one Warn, three
// Debug); "flaky" fails, fails, answers, fails (two Warns: the second spell's
// first failure warns too). Five cycles of "walled" also cross the failure
// tracker's give-up, whose line is a repeat's too.
//
// Mutants: probeRow passing Repeat false (walled warns every cycle);
// noteProbeOutcome never ending a spell on an answer (flaky's second spell
// logs at Debug), or never starting one on a failure (walled warns every
// cycle).
func TestWalk_AProbeThatKeepsFailingWarnsOncePerSpell(t *testing.T) {
	db := newTestDB(t)
	now := fixedNow()
	const chID = "UC1"
	seedRow(t, db, chID, "walled", now.Add(-1*time.Hour), "exact", "rss", "unknown")
	seedRow(t, db, chID, "flaky", now.Add(-2*time.Hour), "exact", "rss", "unknown")

	cycle := 0
	walledErr := errors.New("status probe inconclusive: android_vr refused with no broadcast details (login_required)")
	probe := func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		if videoID == "flaky" && cycle == 3 {
			return &VideoProbeResult{StreamStatus: "upcoming", Title: videoID}, nil
		}
		return nil, walledErr
	}
	logger := &warnRecordingLogger{}
	fm := newTestFeedMonitor(t, db, withProbe(probe), withNow(now))
	fm.logger = logger

	for cycle = 1; cycle <= 5; cycle++ {
		fm.walkForTest(t, chID)
		if cycle == 4 {
			if n := failedProbeWarns(logger, "flaky"); n != 2 {
				t.Errorf("flaky: %d Warn lines after fail, fail, answer, fail, want 2 — the answer ends the spell, and the next failure warns", n)
			}
		}
	}
	if n := failedProbeWarns(logger, "walled"); n != 1 {
		t.Errorf("walled: %d Warn lines over 5 cycles of failed probes, want 1, the spell's first", n)
	}
	debugs := 0
	for _, d := range logger.debugs {
		if strings.HasPrefix(d, "[Monitor] Failed to check metadata for walled ") {
			debugs++
		}
	}
	if debugs != 4 {
		t.Errorf("walled: %d Debug lines for its failed probes, want 4: every failure after the first is logged, below Warn", debugs)
	}
}

// TestWalk_AFailureSpellEndsWithTheRowsScope: a spell is kept for a row the
// walk can probe again, and dropped once the row has left the walk's scope —
// which a failing row does when its date leaves the window — or its channel
// has left the configuration (PruneHealth), so the spells are bounded by the
// scopes. A row that comes back starts a new spell.
//
// Mutants: pruneFailureSpells not called at the end of walk, or keeping rows
// outside scope (the out-of-scope row's spell survives); PruneHealth leaving
// failingProbes alone (the removed channel's spell survives).
func TestWalk_AFailureSpellEndsWithTheRowsScope(t *testing.T) {
	db := newTestDB(t)
	now := fixedNow()
	const chID = "UC1"
	seedRow(t, db, chID, "stays", now.Add(-1*time.Hour), "exact", "rss", "unknown")
	seedRow(t, db, chID, "leaves", now.Add(-2*time.Hour), "exact", "rss", "unknown")

	probe := func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		return nil, errors.New("status probe inconclusive")
	}
	fm := newTestFeedMonitor(t, db, withProbe(probe), withNow(now))
	fm.walkForTest(t, chID)
	if !fm.inFailureSpell(chID, "stays") || !fm.inFailureSpell(chID, "leaves") {
		t.Fatalf("precondition: both rows' failed probes start a spell (stays %v, leaves %v)",
			fm.inFailureSpell(chID, "stays"), fm.inFailureSpell(chID, "leaves"))
	}

	// "leaves" leaves the walk's scope: the next walk reads a scope without it.
	ch := &config.ChannelConfig{ID: chID, Name: chID, IncludeNonLiveContent: true}
	cutoff := now.Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	scope, err := db.FeedScope(chID, cutoff, now.UTC().Format(time.RFC3339), false)
	if err != nil {
		t.Fatal(err)
	}
	kept := scope[:0]
	for _, row := range scope {
		if row.VideoID != "leaves" {
			kept = append(kept, row)
		}
	}
	fm.walk(context.Background(), ch, chID, cutoff, now, kept)
	if !fm.inFailureSpell(chID, "stays") {
		t.Error("stays: its spell ended though it is still in scope and still failing")
	}
	if fm.inFailureSpell(chID, "leaves") {
		t.Error("leaves: its spell outlived the row's place in scope")
	}

	// The channel leaves the configuration: PruneHealth drops its spells.
	fm.PruneHealth()
	if fm.inFailureSpell(chID, "stays") {
		t.Error("stays: its spell outlived its channel's removal from the configuration")
	}
}

// TestWalk_ADetaillessMembersOnlyRefusalIsTheVerdict pins the members-only
// exception at the monitor level (owner decision, 2026-10-31: keep it). An
// ANDROID_VR members-only refusal with no videoDetails reaches the monitors
// as a probe result — classified not_a_stream, the one classification
// reached by absence, carrying the members_only verdict
// (TestAMembersOnlyStatusProbeRefusalReachesTheMonitorsAsTheVerdict, in
// cmd/moombox, pins the wiring that delivers it) — never as an inconclusive,
// errored probe. The walk acts on the verdict, not on the status it rides
// on: it relabels the row 'membership' and asks the authenticated probe the
// same cycle (§9), whose answer is the row's classification, and the row is
// in no failure spell.
//
// Mutants: probeRow keying its relabel on the denied outcome instead of the
// verdict (isDenied needs upcoming, so this shape is never relabelled or
// escalated); noteProbeOutcome counting an answered probe as a failure (the
// row is in a spell).
func TestWalk_ADetaillessMembersOnlyRefusalIsTheVerdict(t *testing.T) {
	db := newTestDB(t)
	now := fixedNow()
	const chID = "UC1"
	seedRow(t, db, chID, "m1", now.Add(-1*time.Hour), "exact", "rss", "unknown")

	var anonCalls, authCalls []string
	anon, auth := escalationProbes(
		&VideoProbeResult{StreamStatus: "not_a_stream", PlayabilityError: "members_only"},
		&VideoProbeResult{StreamStatus: "upcoming", Title: "m1"},
		&anonCalls, &authCalls)
	fm := newTestFeedMonitor(t, db, withProbe(anon), withProbeAuth(auth), withMembership(membWith()), withNow(now))

	fresh := fm.walkForTest(t, chID)

	if len(anonCalls) != 1 || len(authCalls) != 1 {
		t.Fatalf("probes anon=%v auth=%v, want one of each: the verdict escalates the same cycle", anonCalls, authCalls)
	}
	row := mustGetFeedItem(t, db, chID, "m1")
	if row.Source != "membership" {
		t.Errorf("source = %q, want membership: the refusal is the sighting", row.Source)
	}
	if res, ok := fresh["m1"]; !ok || res.Outcome != OutcomeProbed || res.StreamStatus != "upcoming" {
		t.Errorf("fresh[m1] = %+v (present %v), want the authenticated probe's upcoming", res, ok)
	}
	if row.Status != "upcoming" {
		t.Errorf("status = %q, want upcoming, the authenticated answer", row.Status)
	}
	if fm.inFailureSpell(chID, "m1") {
		t.Error("the refusal put the row in a failure spell: it is an answer, not a failed probe")
	}
}
