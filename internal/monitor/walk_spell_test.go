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
// follow-up 2 (2026-10-31): the feed walk warns a row's failed probe once a
// spell — Warn on the first in a spell, Debug after — as DECAPI demotes its
// repeats (TestDecapi_AProbeThatKeepsFailingWarnsOncePerNewestVideo). A
// row whose probe keeps failing stores nothing and is probed again, and
// every one of those probes logged a Warn. Now the first failed probe since
// the row last answered warns, every one after it logs at Debug, and an
// answer ends the spell, so the next failure warns again.
//
// The failures here are transient (a reset connection), which hold nothing:
// the row is probed every cycle, five probes in five cycles, where an
// inconclusive one is held on the ladder
// (TestWalk_AnInconclusiveRowIsHeldOnTheLadder, whose spell warns once too).
//
// Two rows over four cycles: "failing" fails every cycle (one Warn, three
// Debug); "flaky" fails, fails, answers, fails (two Warns: the second spell's
// first failure warns too). Five cycles of "failing" also cross the failure
// tracker's give-up, whose line is a repeat's too.
//
// Mutants: probeRow passing Repeat false (failing warns every cycle);
// noteProbeOutcome never ending a spell on an answer (flaky's second spell
// logs at Debug), never starting one on a failure (failing warns every
// cycle), or holding the row on a transient failure (failing is probed
// once).
func TestWalk_AProbeThatKeepsFailingWarnsOncePerSpell(t *testing.T) {
	db := newTestDB(t)
	now := fixedNow()
	const chID = "UC1"
	seedRow(t, db, chID, "failing", now.Add(-1*time.Hour), "exact", "rss", "unknown")
	seedRow(t, db, chID, "flaky", now.Add(-2*time.Hour), "exact", "rss", "unknown")

	cycle := 0
	transient := errors.New("dial tcp: connection reset by peer")
	probe := func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		if videoID == "flaky" && cycle == 3 {
			return &VideoProbeResult{StreamStatus: "upcoming", Title: videoID}, nil
		}
		return nil, transient
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
	if n := failedProbeWarns(logger, "failing"); n != 1 {
		t.Errorf("failing: %d Warn lines over 5 cycles of failed probes, want 1, the spell's first", n)
	}
	debugs := 0
	for _, d := range logger.debugs {
		if strings.HasPrefix(d, "[Monitor] Failed to check metadata for failing ") {
			debugs++
		}
	}
	if debugs != 4 {
		t.Errorf("failing: %d Debug lines for its failed probes, want 4: probed every cycle, and every failure after the first is logged, below Warn", debugs)
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

// TestWalk_ADetaillessMembersOnlyRefusalWithMembershipInactiveStoresNothing
// is owner decision 9 of wave FC. With membership inactive — no cookies, so
// no authenticated retry — the detail-less members-only refusal (classified
// not_a_stream, the one classification reached by absence) used to be
// stored as the row's not_a_stream, which no rule probes again: the
// members-only stream was lost for good, cookies or none. Now it stores
// nothing, as an upcoming members-only refusal (OutcomeDenied) does: the row
// stays unknown, the relabel to membership still lands, the cookie gate
// parks it while membership is inactive, and once cookies return the
// signed-in probe classifies it.
//
// Mutants: probeRow returning the refusal as probed (the row is stored
// not_a_stream, and the signed-in probe never runs once cookies return);
// storing nothing for a members-only answer whatever its status (the
// refusal with details, the vod row, stores nothing).
func TestWalk_ADetaillessMembersOnlyRefusalWithMembershipInactiveStoresNothing(t *testing.T) {
	db := newTestDB(t)
	now := fixedNow()
	const chID = "UC1"
	seedRow(t, db, chID, "m2", now.Add(-1*time.Hour), "exact", "rss", "unknown")

	var anonCalls, authCalls []string
	anon, auth := escalationProbes(
		&VideoProbeResult{StreamStatus: "not_a_stream", PlayabilityError: "members_only"},
		&VideoProbeResult{StreamStatus: "upcoming", Title: "m2"},
		&anonCalls, &authCalls)
	fm := newTestFeedMonitor(t, db, withProbe(anon), withProbeAuth(auth), withNow(now))

	fresh := fm.walkForTest(t, chID)
	if len(anonCalls) != 1 || len(authCalls) != 0 {
		t.Fatalf("probes anon=%v auth=%v, want the anonymous one alone: no cookies, no retry", anonCalls, authCalls)
	}
	row := mustGetFeedItem(t, db, chID, "m2")
	if row.Status != "unknown" || row.Source != "membership" {
		t.Errorf("row = %s/%s, want unknown/membership: nothing stored, the relabel kept", row.Status, row.Source)
	}
	if res, ok := fresh["m2"]; ok {
		t.Errorf("fresh[m2] = %+v, want none: nothing was classified", res)
	}

	// Cookies return: the signed-in probe answers the row.
	fm.FetchMembership = membWith()
	fm.walkForTest(t, chID)
	if len(authCalls) != 1 {
		t.Fatalf("authenticated probes = %v once cookies returned, want one", authCalls)
	}
	if got := mustGetFeedItem(t, db, chID, "m2").Status; got != "upcoming" {
		t.Errorf("status = %q once cookies returned, want the signed-in probe's upcoming", got)
	}
}

// A members-only answer that does state the broadcast (here a vod) is that
// classification, stored as it always was: only the detail-less shape is
// held back.
func TestWalk_AMembersOnlyAnswerWithDetailsIsStoredWithMembershipInactive(t *testing.T) {
	db := newTestDB(t)
	now := fixedNow()
	const chID = "UC1"
	seedRow(t, db, chID, "m3", now.Add(-1*time.Hour), "exact", "rss", "unknown")
	var anonCalls, authCalls []string
	anon, auth := escalationProbes(&VideoProbeResult{StreamStatus: "vod", PlayabilityError: "members_only"}, nil, &anonCalls, &authCalls)
	fm := newTestFeedMonitor(t, db, withProbe(anon), withProbeAuth(auth), withNow(now))
	fm.walkForTest(t, chID)
	if got := mustGetFeedItem(t, db, chID, "m3").Status; got != "vod" {
		t.Errorf("status = %q, want vod, the answer's own classification", got)
	}
}
