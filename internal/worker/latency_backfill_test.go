package worker

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// TestClassifyLatencyAnswer is the backfill's final-vs-transient table
// (owner ruling RV-19): an answer with videoDetails is stored (its class, or
// unstated without one), a refusal every client asked calls private, gone
// or blocked is stored as unstated, a rate limit in any of its shapes holds
// the walk, and the rest is asked again later.
//
// Mutants: the class parsed only for a live answer (the VOD cases fail);
// a lone terminal refusal from ANDROID_VR taken as final while its
// escalation failed (the "escalation failed" case); a members-only refusal
// taken as final; the escalation's rate limit read as transient.
func TestClassifyLatencyAnswer(t *testing.T) {
	t.Parallel()
	const normal = "MDE_STREAM_OPTIMIZATIONS_RENDERER_LATENCY_NORMAL"
	inconclusive := func(p, esc youtube.PlayabilityError, escErr error) error {
		return &youtube.StatusProbeInconclusiveError{Playability: p, Escalation: youtube.StatusProbeEscalationWebEmbedded,
			EscalationErr: escErr, EscalationPlayability: esc}
	}
	refused := errors.New("refused too")
	cases := []struct {
		name    string
		info    *youtube.VideoInfo
		err     error
		mode    database.LatencyMode
		verdict latencyVerdict
	}{
		{"an ended stream with its class", &youtube.VideoInfo{StreamStatus: youtube.StreamVOD, LatencyClass: normal, LatencyClassAsked: true},
			nil, database.LatencyModeNormal, latencyAnswered},
		{"ultra low", &youtube.VideoInfo{LatencyClass: "MDE_STREAM_OPTIMIZATIONS_RENDERER_LATENCY_ULTRA_LOW", LatencyClassAsked: true},
			nil, database.LatencyModeUltraLow, latencyAnswered},
		{"an upload: videoDetails, no class", &youtube.VideoInfo{StreamStatus: youtube.StreamVOD, LatencyClassAsked: true},
			nil, database.LatencyModeUnstated, latencyAnswered},
		{"private on the answer itself", &youtube.VideoInfo{PlayabilityError: youtube.PlayabilityPrivate},
			nil, database.LatencyModeUnstated, latencyAnswered},
		{"members only, no details", &youtube.VideoInfo{PlayabilityError: youtube.PlayabilityMembersOnly},
			nil, "", latencyTransient},
		{"login required (bot wall)", &youtube.VideoInfo{PlayabilityError: youtube.PlayabilityLoginRequired},
			nil, "", latencyTransient},
		{"a rate-limited answer", &youtube.VideoInfo{PlayabilityError: youtube.PlayabilityRateLimited},
			nil, "", latencyRateLimited},
		{"no answer and no error", nil, nil, "", latencyTransient},
		{"deleted: both clients say unavailable", nil,
			inconclusive(youtube.PlayabilityUnavailable, youtube.PlayabilityUnavailable, refused), database.LatencyModeUnstated, latencyAnswered},
		{"private on both", nil,
			inconclusive(youtube.PlayabilityPrivate, youtube.PlayabilityPrivate, refused), database.LatencyModeUnstated, latencyAnswered},
		{"unavailable, escalation failed", nil,
			inconclusive(youtube.PlayabilityUnavailable, "", errors.New("HTTP 400")), "", latencyTransient},
		{"unavailable, escalation a bot wall", nil,
			inconclusive(youtube.PlayabilityUnavailable, youtube.PlayabilityLoginRequired, refused), "", latencyTransient},
		{"bot wall, escalation unavailable", nil,
			inconclusive(youtube.PlayabilityLoginRequired, youtube.PlayabilityUnavailable, refused), "", latencyTransient},
		{"probe rate-limited", nil,
			inconclusive(youtube.PlayabilityRateLimited, "", nil), "", latencyRateLimited},
		{"escalation rate-limited", nil,
			inconclusive(youtube.PlayabilityUnavailable, youtube.PlayabilityRateLimited, refused), "", latencyRateLimited},
		{"a 429", nil, fmt.Errorf("probe: %w", &youtube.HTTPRateLimitError{Err: errors.New("429")}), "", latencyRateLimited},
		{"a network error", nil, errors.New("dial tcp: timeout"), "", latencyTransient},
	}
	for _, c := range cases {
		mode, verdict := classifyLatencyAnswer(c.info, c.err)
		if mode != c.mode || verdict != c.verdict {
			t.Errorf("%s: (%q, %d), want (%q, %d)", c.name, mode, verdict, c.mode, c.verdict)
		}
	}
}

// backfillHarness is a walk over a real database with a scripted probe and
// a settable clock, online state, hold and starting capture.
type backfillHarness struct {
	t        *testing.T
	db       *database.Database
	b        *latencyBackfill
	now      time.Time
	online   bool
	held     bool
	starting bool
	answers  map[string]func() (*youtube.VideoInfo, error)
	asked    []string
}

func newBackfillHarness(t *testing.T, db *database.Database) *backfillHarness {
	h := &backfillHarness{t: t, db: db, now: time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC), online: true,
		answers: map[string]func() (*youtube.VideoInfo, error){}}
	h.b = h.newWalk()
	return h
}

// newWalk is a fresh walk over the same database: a restart.
func (h *backfillHarness) newWalk() *latencyBackfill {
	return &latencyBackfill{
		db: h.db, logger: discardLogger{},
		probe: func(_ context.Context, videoID string) (*youtube.VideoInfo, error) {
			h.asked = append(h.asked, videoID)
			if a, ok := h.answers[videoID]; ok {
				return a()
			}
			return nil, errors.New("no scripted answer")
		},
		online:          func() bool { return h.online },
		rateLimitHeld:   func(time.Time) bool { return h.held },
		captureStarting: func() bool { return h.starting },
		now:             func() time.Time { return h.now },
	}
}

func (h *backfillHarness) add(id string, status database.JobStatus) {
	h.t.Helper()
	if _, err := h.db.AddJob(&database.Job{ID: id, VideoID: id, URL: "u", Platform: "youtube", Status: status}); err != nil {
		h.t.Fatalf("AddJob %s: %v", id, err)
	}
}

func (h *backfillHarness) mode(id string) database.LatencyMode {
	h.t.Helper()
	j, err := h.db.GetJob(id)
	if err != nil || j == nil {
		h.t.Fatalf("GetJob %s: %v", id, err)
	}
	return j.LatencyMode
}

// step runs one step and checks the wait it asks for.
func (h *backfillHarness) step(want time.Duration) {
	h.t.Helper()
	if got := h.b.stepRecovered(context.Background()); got != want {
		h.t.Fatalf("step waits %v, want %v", got, want)
	}
}

func (h *backfillHarness) takeAsked() []string {
	a := h.asked
	h.asked = nil
	return a
}

func answered(class string) func() (*youtube.VideoInfo, error) {
	return func() (*youtube.VideoInfo, error) {
		return &youtube.VideoInfo{LatencyClass: class, LatencyClassAsked: true}, nil
	}
}

// TestLatencyBackfillStoresOneRowPerStep: one request per step, a pause of
// latencyBackfillPause after each, the answer stored, rows that are not
// Finished YouTube rows with no mode left alone, and the idle wait once
// nothing is left.
//
// Mutants: step asking more than one row (two asked in one step); the pause
// shortened or dropped; the write left out (the mode stays NULL).
func TestLatencyBackfillStoresOneRowPerStep(t *testing.T) {
	t.Parallel()
	h := newBackfillHarness(t, openTestDB(t))
	h.add("aaa", database.StatusFinished)
	h.add("bbb", database.StatusFinished)
	h.add("ccc", database.StatusError)
	h.answers["aaa"] = answered("MDE_STREAM_OPTIMIZATIONS_RENDERER_LATENCY_LOW")
	h.answers["bbb"] = answered("")

	h.step(latencyBackfillPause)
	if got := h.takeAsked(); len(got) != 1 || got[0] != "aaa" {
		t.Fatalf("first step asked %v, want [aaa]", got)
	}
	if h.mode("aaa") != database.LatencyModeLow {
		t.Errorf("aaa = %q, want low", h.mode("aaa"))
	}
	h.step(latencyBackfillPause)
	if h.mode("bbb") != database.LatencyModeUnstated {
		t.Errorf("bbb = %q, want unstated (an upload)", h.mode("bbb"))
	}
	h.step(latencyBackfillIdle)
	if got := h.takeAsked(); len(got) != 1 {
		t.Errorf("asked %v after the walk, want only bbb", got)
	}
	if h.mode("ccc") != database.LatencyModeNone {
		t.Error("an Error row was asked about")
	}
}

// TestLatencyBackfillHolds: nothing is asked while offline, while the
// scheduler's rate-limit hold stands, or while a capture is starting; each
// waits one pause and looks again. Its own rate-limit verdict holds the walk
// for rateLimitBackoff without counting against the row.
//
// Mutants: any of the three gates dropped (a request goes out); the walk's
// own hold not placed (the second step asks again at once); a rate limit
// counted as a failure (the row is given up on after three).
func TestLatencyBackfillHolds(t *testing.T) {
	t.Parallel()
	h := newBackfillHarness(t, openTestDB(t))
	h.add("aaa", database.StatusFinished)
	h.answers["aaa"] = answered("MDE_STREAM_OPTIMIZATIONS_RENDERER_LATENCY_NORMAL")

	for _, gate := range []struct {
		name string
		set  func(bool)
	}{
		{"offline", func(on bool) { h.online = !on }},
		{"the scheduler's rate-limit hold", func(on bool) { h.held = on }},
		{"a capture starting", func(on bool) { h.starting = on }},
	} {
		gate.set(true)
		h.step(latencyBackfillPause)
		if got := h.takeAsked(); len(got) != 0 {
			t.Errorf("%s: asked %v, want nothing", gate.name, got)
		}
		gate.set(false)
	}

	rateLimited := 0
	h.answers["aaa"] = func() (*youtube.VideoInfo, error) {
		rateLimited++
		return nil, &youtube.StatusProbeInconclusiveError{Playability: youtube.PlayabilityRateLimited}
	}
	for i := range latencyBackfillMaxTries + 1 {
		h.step(latencyBackfillPause)
		if rateLimited != i+1 {
			t.Fatalf("round %d: %d requests, want %d", i, rateLimited, i+1)
		}
		// Inside the hold: nothing is asked.
		h.now = h.now.Add(rateLimitBackoff - time.Second)
		h.step(latencyBackfillPause)
		if rateLimited != i+1 {
			t.Fatalf("round %d: asked inside the walk's own hold", i)
		}
		h.now = h.now.Add(time.Second)
	}
	h.answers["aaa"] = answered("MDE_STREAM_OPTIMIZATIONS_RENDERER_LATENCY_NORMAL")
	h.step(latencyBackfillPause)
	if h.mode("aaa") != database.LatencyModeNormal {
		t.Errorf("after the holds: %q, want normal (rate limits never count against the row)", h.mode("aaa"))
	}
}

// TestLatencyBackfillTransientIsBounded: a failed request leaves the row
// NULL, the walk moves on to the next row rather than standing on it, asks
// it again no sooner than latencyBackfillRetryAfter, and stops after
// latencyBackfillMaxTries in this process; a failure met as the device went
// offline is not counted. A restart (a new walk over the same database)
// asks again from the NULL rows themselves, and never about a stored one.
//
// Mutants: the retry wait ignored (asked again on the next step); the try
// cap dropped (asked a fourth time); the offline check after a failure
// dropped (given up one try early); the cursor not advanced past a failing
// row (bbb never asked).
func TestLatencyBackfillTransientIsBounded(t *testing.T) {
	t.Parallel()
	h := newBackfillHarness(t, openTestDB(t))
	h.add("aaa", database.StatusFinished)
	h.add("bbb", database.StatusFinished)
	h.answers["aaa"] = func() (*youtube.VideoInfo, error) { return nil, errors.New("dial tcp: timeout") }
	h.answers["bbb"] = answered("MDE_STREAM_OPTIMIZATIONS_RENDERER_LATENCY_LOW")

	h.step(latencyBackfillPause) // aaa fails (1)
	h.step(latencyBackfillPause) // bbb answers
	if got := h.takeAsked(); len(got) != 2 || got[1] != "bbb" {
		t.Fatalf("asked %v, want aaa then bbb", got)
	}
	if h.mode("aaa") != database.LatencyModeNone || h.mode("bbb") != database.LatencyModeLow {
		t.Fatalf("modes aaa=%q bbb=%q, want NULL and low", h.mode("aaa"), h.mode("bbb"))
	}
	h.step(latencyBackfillIdle) // aaa waits its retry
	if got := h.takeAsked(); len(got) != 0 {
		t.Fatalf("asked %v inside the retry wait", got)
	}

	// A failure as the device goes offline is not counted.
	h.now = h.now.Add(latencyBackfillRetryAfter)
	h.answers["aaa"] = func() (*youtube.VideoInfo, error) { h.online = false; return nil, errors.New("offline") }
	h.step(latencyBackfillPause)
	h.online = true
	h.answers["aaa"] = func() (*youtube.VideoInfo, error) { return nil, errors.New("dial tcp: timeout") }

	for i := 2; i <= latencyBackfillMaxTries; i++ {
		h.step(latencyBackfillPause) // aaa fails (i)
		h.now = h.now.Add(latencyBackfillRetryAfter)
	}
	if got := h.takeAsked(); len(got) != latencyBackfillMaxTries {
		t.Fatalf("asked %v, want aaa %d more times", got, latencyBackfillMaxTries)
	}
	h.step(latencyBackfillIdle)
	if got := h.takeAsked(); len(got) != 0 {
		t.Fatalf("asked %v after %d failures, want the row given up on", got, latencyBackfillMaxTries)
	}

	// A restart asks again, from the database alone.
	h.b = h.newWalk()
	h.answers["aaa"] = answered("MDE_STREAM_OPTIMIZATIONS_RENDERER_LATENCY_ULTRA_LOW")
	h.step(latencyBackfillPause)
	if got := h.takeAsked(); len(got) != 1 || got[0] != "aaa" {
		t.Fatalf("after a restart asked %v, want [aaa] (bbb is stored)", got)
	}
	if h.mode("aaa") != database.LatencyModeUltraLow {
		t.Errorf("aaa = %q, want ultra_low", h.mode("aaa"))
	}
}

// TestLatencyBackfillStepPanicCountsAsFailure: a step that panics is
// recovered, counted against its row, and the walk goes on.
func TestLatencyBackfillStepPanicCountsAsFailure(t *testing.T) {
	t.Parallel()
	h := newBackfillHarness(t, openTestDB(t))
	h.add("aaa", database.StatusFinished)
	h.answers["aaa"] = func() (*youtube.VideoInfo, error) { panic("boom") }
	h.step(latencyBackfillPause)
	if h.b.tries["aaa"].n != 1 {
		t.Errorf("tries after a panic = %d, want 1", h.b.tries["aaa"].n)
	}
	h.step(latencyBackfillIdle)
}

// TestNewLatencyBackfillReadsTheSchedulersHold: the worker's walk is gated
// on the backlog scheduler's rate-limit hold, and on a Live row.
//
// Mutants: rateLimitHeld or captureStarting left unwired.
func TestNewLatencyBackfillReadsTheSchedulersHold(t *testing.T) {
	t.Parallel()
	w, db := testWorkerSetup(t)
	b := w.newLatencyBackfill()
	if b.probe != nil {
		t.Error("a walk with no YouTube service has a probe")
	}
	now := time.Now()
	if b.rateLimitHeld(now) {
		t.Fatal("held with no verdict")
	}
	w.scheduler.holdForRateLimit(now, rateLimitBackoff)
	if !b.rateLimitHeld(now) {
		t.Error("the scheduler's hold is not seen")
	}
	if b.captureStarting() {
		t.Fatal("a capture starting with no rows")
	}
	if _, err := db.AddJob(&database.Job{ID: "live", VideoID: "live", URL: "u", Platform: "youtube", Status: database.StatusLive}); err != nil {
		t.Fatal(err)
	}
	if !b.captureStarting() {
		t.Error("a Live row is not a capture starting")
	}
}

// TestLatencyProbeAsksTheMemberSessionForAMembersOnlyRefusal: the walk's
// request is the status probe, and for a members-only refusal with no
// videoDetails the authenticated TV probe, but only while signed in; any
// other answer is returned as it is.
//
// Mutants: the follow-up dropped (members-only signed in returns the
// refusal); the signed-in check dropped (a signed-out jar asks TV).
func TestLatencyProbeAsksTheMemberSessionForAMembersOnlyRefusal(t *testing.T) {
	t.Parallel()
	member := &youtube.VideoInfo{LatencyClass: "MDE_STREAM_OPTIMIZATIONS_RENDERER_LATENCY_LOW", LatencyClassAsked: true}
	for _, c := range []struct {
		name     string
		first    *youtube.VideoInfo
		signedIn bool
		wantAuth bool
	}{
		{"members only, signed in", &youtube.VideoInfo{PlayabilityError: youtube.PlayabilityMembersOnly}, true, true},
		{"members only, signed out", &youtube.VideoInfo{PlayabilityError: youtube.PlayabilityMembersOnly}, false, false},
		{"members only with details", &youtube.VideoInfo{PlayabilityError: youtube.PlayabilityMembersOnly, LatencyClassAsked: true}, true, false},
		{"an answer", &youtube.VideoInfo{LatencyClassAsked: true}, true, false},
		{"private", &youtube.VideoInfo{PlayabilityError: youtube.PlayabilityPrivate}, true, false},
	} {
		authAsked := false
		got, err := latencyProbeVia(context.Background(), "vid",
			func(context.Context, string) (*youtube.VideoInfo, error) { return c.first, nil },
			func(context.Context, string) (*youtube.VideoInfo, error) { authAsked = true; return member, nil },
			func() bool { return c.signedIn })
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if authAsked != c.wantAuth {
			t.Errorf("%s: authenticated probe asked = %v, want %v", c.name, authAsked, c.wantAuth)
		}
		if want := c.first; c.wantAuth {
			if got != member {
				t.Errorf("%s: returned the refusal, want the member's answer", c.name)
			}
		} else if got != want {
			t.Errorf("%s: answer replaced", c.name)
		}
	}
}
