package monitor

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// fixedNow is the pinned "now" every Plan 3 Task 3 (WALK) test builds its
// fixtures around, via withNow.
func fixedNow() time.Time {
	return time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
}

// walkForTest builds the same channel/cutoff/scope checkChannel's WALK step
// would (spec §7/§8) and runs fm.walk() directly, for tests that exercise
// the WALK in isolation without a full checkChannel cycle (no RSS/membership
// fetch — fixtures are seeded straight into the store by the callers below).
func (fm *FeedMonitor) walkForTest(t *testing.T, channelID string) map[string]ProbeClassifyResult {
	t.Helper()
	ch := &config.ChannelConfig{ID: channelID, Name: channelID, IncludeNonLiveContent: true}
	cycleNow := fm.now().UTC()
	cutoff := cycleNow.Add(-time.Duration(fm.archiveWindowDays(ch)) * 24 * time.Hour).Format(time.RFC3339)
	scope, err := fm.db.FeedScope(channelID, cutoff, fm.membershipDiscoveryEnabled())
	if err != nil {
		t.Fatalf("FeedScope(%s): %v", channelID, err)
	}
	return fm.walk(context.Background(), ch, channelID, cutoff, scope)
}

// seedRow seeds a single feed_items row directly (bypassing FETCH/STORE),
// with an explicit status — UpsertFeedItem alone always forces status
// 'unknown' on insert, so a non-'unknown' status needs a second write via
// ApplyProbeToFeedItem, whose date-rank guard is a no-op here because the
// probe write reasserts the SAME published/precision the insert used.
func seedRow(t *testing.T, db *database.Database, chID, videoID string, published time.Time, precision, source, status string) {
	t.Helper()
	pubStr := published.UTC().Format(time.RFC3339)
	if _, err := db.UpsertFeedItem(database.FeedItem{
		ChannelID: chID, VideoID: videoID, Title: videoID,
		Published: pubStr, DatePrecision: precision,
		CatalogPos: 0, Source: source, FirstSeen: pubStr,
	}); err != nil {
		t.Fatalf("seedRow upsert %s: %v", videoID, err)
	}
	if status != "unknown" {
		if err := db.ApplyProbeToFeedItem(chID, videoID, status, videoID, pubStr, precision); err != nil {
			t.Fatalf("seedRow applyProbe %s: %v", videoID, err)
		}
	}
}

// seedCoarseLump seeds len(ids) feed_items rows for chID, all displaying the
// same "1 week ago" coarse bucket (stored published = now-7d, the skew-new
// value — spec §12) — mirroring what the STORE step writes for a run of
// membership items that all render the same relative-age text. catalog_pos
// runs 0..N-1 in the given id order, so FeedScope's Q1 (published DESC,
// catalog_pos ASC) walks them in exactly that order.
func seedCoarseLump(t *testing.T, db *database.Database, chID string, now time.Time, source string, ids ...string) {
	t.Helper()
	pubStr := now.Add(-7 * 24 * time.Hour).UTC().Format(time.RFC3339)
	for i, id := range ids {
		if _, err := db.UpsertFeedItem(database.FeedItem{
			ChannelID: chID, VideoID: id, Title: id,
			Published: pubStr, DatePrecision: "coarse",
			CatalogPos: i, Source: source, FirstSeen: pubStr,
		}); err != nil {
			t.Fatalf("seedCoarseLump upsert %s: %v", id, err)
		}
	}
}

// probeReturningTrueAges builds a VideoProbeFunc that records every probed
// videoID (in call order) into *order and reports a dated `vod`
// classification computed from trueAgeDays — simulating a probe that
// discovers a listing estimate's real age. The STORED `published` a row
// carries (Q1 admission/ordering) and the DATE THE PROBE RETURNS (exhaustion
// math) are deliberately independent: callers control the former via
// seedRow/seedCoarseLump and the latter via this map.
func probeReturningTrueAges(now time.Time, trueAgeDays map[string]int, order *[]string) VideoProbeFunc {
	return func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		*order = append(*order, videoID)
		age, ok := trueAgeDays[videoID]
		if !ok {
			return nil, fmt.Errorf("probeReturningTrueAges: no fixture age for %s", videoID)
		}
		return &VideoProbeResult{
			StreamStatus:       "vod",
			Title:              videoID,
			PublishedAt:        now.Add(-time.Duration(age) * 24 * time.Hour).UTC().Format(time.RFC3339),
			PublishedPrecision: "day",
		}, nil
	}
}

// recordingProbe builds a VideoProbeFunc that records every probed videoID
// (in call order) into *probed and always reports a stable vod+started
// classification — for tests that only care whether/how many times a probe
// ran, not what it classified.
func recordingProbe(probed *[]string) VideoProbeFunc {
	return func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		*probed = append(*probed, videoID)
		return &VideoProbeResult{
			StreamStatus:       "vod",
			Title:              videoID,
			PublishedAt:        "2026-01-01T00:00:00Z",
			PublishedPrecision: "started",
		}, nil
	}
}

// contains reports whether s appears in list.
func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// addFinishedJob inserts a minimal terminal (Finished) job row keyed by
// videoID — jobs.id == jobs.video_id at every production creation site
// (database_feed_items.go's HasAnyJob comment), so this is enough to make
// HasAnyJob(videoID) true without exercising the full job-creation path.
func addFinishedJob(t *testing.T, db *database.Database, videoID string) {
	t.Helper()
	if _, err := db.AddJob(&database.Job{
		ID: videoID, VideoID: videoID, URL: "https://example.com/" + videoID,
		Status: database.StatusFinished,
	}); err != nil {
		t.Fatalf("addFinishedJob(%s): %v", videoID, err)
	}
}

// warnRecordingLogger discards Debug/Info/Error but records every Warn
// call's message, for tests asserting a specific warning was (or was not)
// logged. Not safe for concurrent use — fine here since walk() is serial and
// every caller runs it synchronously in the test goroutine.
type warnRecordingLogger struct {
	warnings []string
	debugs   []string
}

func (l *warnRecordingLogger) Debug(msg string, args ...any) { l.debugs = append(l.debugs, msg) }
func (l *warnRecordingLogger) Info(msg string, args ...any)  {}
func (l *warnRecordingLogger) Warn(msg string, args ...any)  { l.warnings = append(l.warnings, msg) }
func (l *warnRecordingLogger) Error(msg string, args ...any) {}

// TestWalk_EarlyExitPerSource covers spec §17's headline WALK case: against
// a 10-day window, five membership rows all displaying the same "1 week
// ago" coarse bucket, with true ages 8/9/11/12/13 days, cost exactly THREE
// probes — the third (11d, outside the window) retires the source, so 12d
// and 13d are never probed. Assert ordering (a concurrent implementation
// could pass a count assertion while probing every row past the boundary).
func TestWalk_EarlyExitPerSource(t *testing.T) {
	db := newTestDB(t)
	now := fixedNow()
	seedCoarseLump(t, db, "UC1", now, "membership", "w1", "w2", "w3", "w4", "w5")
	var order []string
	probe := probeReturningTrueAges(now, map[string]int{"w1": 8, "w2": 9, "w3": 11, "w4": 12, "w5": 13}, &order)
	fm := newTestFeedMonitor(t, db, withProbe(probe), withMembership(membWith()), withWindowDays(10), withNow(now))
	fm.walkForTest(t, "UC1")
	if len(order) != 3 || order[0] != "w1" || order[1] != "w2" || order[2] != "w3" {
		t.Fatalf("probe order %v, want [w1 w2 w3] — serial, and exhaustion after the first out-of-window DATED probe", order)
	}
}

// assertSecondRowStillProbed seeds two "unknown" membership rows in the same
// source and probes row1 with an outcome that must NOT retire the source —
// asserting row2 is still probed proves it. row1's DatePrecision is
// 'assumed' for mode=="assumed" (a dated-but-unresolved row) and 'coarse'
// otherwise (a dated listing row whose PROBE outcome itself doesn't qualify
// to exhaust: errored/denied/cooldown/dateless).
func assertSecondRowStillProbed(t *testing.T, mode string) {
	t.Helper()
	db := newTestDB(t)
	now := fixedNow()
	const chID = "UC1"
	const src = "membership"

	row1Precision := "coarse"
	if mode == "assumed" {
		row1Precision = "assumed"
	}
	// Both rows sit comfortably inside the default window (their STORED
	// published is a couple hours old); only row1's PROBE outcome varies by
	// mode, and it's what must fail to retire the source.
	seedRow(t, db, chID, "row1", now.Add(-1*time.Hour), row1Precision, src, "unknown")
	seedRow(t, db, chID, "row2", now.Add(-2*time.Hour), "coarse", src, "unknown")

	var order []string
	probe := func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		order = append(order, videoID)
		if videoID == "row1" {
			switch mode {
			case "errored":
				return nil, fmt.Errorf("boom")
			case "denied":
				return &VideoProbeResult{StreamStatus: "upcoming", PlayabilityError: "members_only"}, nil
			case "dateless":
				return &VideoProbeResult{StreamStatus: "not_a_stream"}, nil // no PublishedAt
			case "assumed":
				// A normal DATED probe result that WOULD retire a coarse row
				// — must not retire the source because row1 is 'assumed'.
				return &VideoProbeResult{StreamStatus: "vod", PublishedAt: now.AddDate(0, 0, -30).UTC().Format(time.RFC3339), PublishedPrecision: "day"}, nil
			case "cooldown":
				t.Fatal("row1 must not reach the probe function within its cooldown window")
			}
		}
		return &VideoProbeResult{StreamStatus: "vod", Title: videoID, PublishedAt: now.UTC().Format(time.RFC3339), PublishedPrecision: "day"}, nil
	}

	fm := newTestFeedMonitor(t, db, withProbe(probe), withMembership(membWith()), withNow(now))
	if mode == "cooldown" {
		fm.ProbeCooldown.SetDuration(time.Hour)
		fm.ProbeCooldown.Record("row1") // pre-seed so row1's probe is cooldown-suppressed
	}
	fm.walkForTest(t, chID)

	if !contains(order, "row2") {
		t.Fatalf("mode=%s: row2 not probed, order=%v", mode, order)
	}
}

// TestWalk_OnlyDatedCoarseProbesRetire covers spec §17: errored / denied /
// cooldown / probed-with-no-date leave the source live (none of them
// "learned" the window boundary), and an assumed row's date is not a
// listing coordinate so it never retires the source either.
func TestWalk_OnlyDatedCoarseProbesRetire(t *testing.T) {
	for _, mode := range []string{"errored", "denied", "cooldown", "dateless", "assumed"} {
		t.Run(mode, func(t *testing.T) { assertSecondRowStillProbed(t, mode) })
	}
}

// TestWalk_OrderingCheck covers spec §17's three ordering-check assertions
// over a seeded two-source scope: (1) an 8d-then-7d (newer) probe disables
// early exit for that source this cycle, so every row is probed and a
// warning is logged; (2) the check does not fire on the FIRST probe of a
// source (the IS-SET guard — a zero-value time is older than any real
// date); (3) per-source: a mis-ordered "membership" must not disable early
// exit for another, correctly-ordered source.
func TestWalk_OrderingCheck(t *testing.T) {
	db := newTestDB(t)
	now := fixedNow()
	const chID = "UC1"

	// "membership": listing order m1..m4 (stored published a few seconds
	// apart, purely for Q1 ordering). Probed true ages 8d, 7d (NEWER — a
	// violation), 20d, 21d.
	seedRow(t, db, chID, "m1", now.Add(-1*time.Second), "coarse", "membership", "unknown")
	seedRow(t, db, chID, "m2", now.Add(-2*time.Second), "coarse", "membership", "unknown")
	seedRow(t, db, chID, "m3", now.Add(-3*time.Second), "coarse", "membership", "unknown")
	seedRow(t, db, chID, "m4", now.Add(-4*time.Second), "coarse", "membership", "unknown")

	// A second, independently-tracked date-ordered source. Production only
	// ever writes rss/membership to feed_items.source, but walk()'s
	// per-source bookkeeping is keyed generically by row.Source and must not
	// leak between whatever values it sees — "membership2" is a test-only
	// stand-in exercising that genericness. Correctly ordered (true ages
	// increase monotonically: 8d, 9d, then 12d outside the 10-day window),
	// so it must retire normally regardless of membership's disabled early
	// exit.
	seedRow(t, db, chID, "b1", now.Add(-11*time.Second), "coarse", "membership2", "unknown")
	seedRow(t, db, chID, "b2", now.Add(-12*time.Second), "coarse", "membership2", "unknown")
	seedRow(t, db, chID, "b3", now.Add(-13*time.Second), "coarse", "membership2", "unknown")
	seedRow(t, db, chID, "b4", now.Add(-14*time.Second), "coarse", "membership2", "unknown")

	trueAgeDays := map[string]int{
		"m1": 8, "m2": 7, "m3": 20, "m4": 21,
		"b1": 8, "b2": 9, "b3": 12, "b4": 13,
	}
	var order []string
	probe := probeReturningTrueAges(now, trueAgeDays, &order)
	fm := newTestFeedMonitor(t, db, withProbe(probe), withMembership(membWith()), withWindowDays(10), withNow(now))
	rec := &warnRecordingLogger{}
	fm.logger = rec
	fm.walkForTest(t, chID)

	// Assertion 1: the m2 violation disables early exit for "membership"
	// this cycle, so m3's out-of-window date does not retire it — m4 is
	// still probed too.
	for _, id := range []string{"m1", "m2", "m3", "m4"} {
		if !contains(order, id) {
			t.Fatalf("membership row %s not probed; order=%v", id, order)
		}
	}

	// Assertion 2 (IS-SET guard): exactly ONE violation logged, at m2 —
	// never at m1, whose lastDate[src] was unset.
	violations := 0
	for _, w := range rec.warnings {
		if w == "listing order violated; early exit disabled" {
			violations++
		}
	}
	if violations != 1 {
		t.Fatalf("got %d ordering-violation warnings, want exactly 1 (must not fire on the first probe of a source): %v", violations, rec.warnings)
	}

	// Assertion 3 (per-source): "membership2" is unaffected by membership's
	// disabled early exit — it retires normally at b3, so b4 (behind the
	// boundary) is never probed.
	for _, id := range []string{"b1", "b2", "b3"} {
		if !contains(order, id) {
			t.Fatalf("membership2 row %s not probed; order=%v", id, order)
		}
	}
	if contains(order, "b4") {
		t.Fatalf("membership2 must still retire normally despite membership's disabled early exit; order=%v", order)
	}
}

// TestWalk_DatelessProbeHonorsRowDate is the hg9j1c9-Z8A production
// reproduction: the ANDROID_VR status probe carries no microformat, so every
// probe result is dateless — and the terminal invariant as first shipped
// demoted EVERY dateless vod write back to 'unknown', even on a row already
// holding a rank-4 'exact' RSS date. The store never advanced: a silent
// infinite no-op loop. §12's invariant is about the ROW ending up dated — a
// coarse-or-better stored date satisfies it without any probe date.
func TestWalk_DatelessProbeHonorsRowDate(t *testing.T) {
	db := newTestDB(t)
	now := fixedNow()
	pub := now.Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	seedRow(t, db, "UC1", "d1", now.Add(-2*time.Hour), "exact", "rss", "unknown")

	fm := newTestFeedMonitor(t, db, withProbe(func(ctx context.Context, id string) (*VideoProbeResult, error) {
		return &VideoProbeResult{StreamStatus: "vod", Title: "d1"}, nil // dateless, like production
	}), withNow(now))
	rec := &warnRecordingLogger{}
	fm.logger = rec
	fm.walkForTest(t, "UC1")

	row := mustGetFeedItem(t, db, "UC1", "d1")
	if row.Status != "vod" {
		t.Fatalf("status = %q, want vod — a dateless probe must not demote a row that already holds an exact date", row.Status)
	}
	if row.Published != pub || row.DatePrecision != "exact" {
		t.Fatalf("row date must be untouched: got %s/%s, want %s/exact", row.Published, row.DatePrecision, pub)
	}
	if !contains(rec.debugs, "walk done") {
		t.Fatalf("walk must emit its per-channel summary; debugs=%v", rec.debugs)
	}
}

// TestWalk_TwoPhaseDateFetch covers §9's date-completing fetch: a vod-family
// probe with no date on a row whose own date is an ESTIMATE (coarse/assumed)
// triggers exactly one ProbeDate call, and the fetched date upgrades the row
// through the ladder. Rows already holding day/exact/started dates never
// fetch — their date is authoritative.
func TestWalk_TwoPhaseDateFetch(t *testing.T) {
	db := newTestDB(t)
	now := fixedNow()
	const chID = "UC1"
	seedRow(t, db, chID, "t1", now.Add(-1*time.Hour), "coarse", "videos", "unknown")
	seedRow(t, db, chID, "t2", now.Add(-2*time.Hour), "exact", "rss", "unknown")

	trueDate := now.Add(-30 * time.Hour).UTC().Format(time.RFC3339)
	var fetched []string
	fm := newTestFeedMonitor(t, db,
		withProbe(func(ctx context.Context, id string) (*VideoProbeResult, error) {
			return &VideoProbeResult{StreamStatus: "vod", Title: id}, nil // dateless
		}),
		withProbeDate(func(ctx context.Context, id string) (string, string, error) {
			fetched = append(fetched, id)
			return trueDate, "day", nil
		}),
		withNow(now))
	fm.walkForTest(t, chID)

	if len(fetched) != 1 || fetched[0] != "t1" {
		t.Fatalf("date-fetch calls = %v, want exactly [t1] — day/exact/started rows never fetch", fetched)
	}
	r1 := mustGetFeedItem(t, db, chID, "t1")
	if r1.Status != "vod" || r1.Published != trueDate || r1.DatePrecision != "day" {
		t.Fatalf("t1 = %s %s/%s, want vod %s/day — the fetched date upgrades the row", r1.Status, r1.Published, r1.DatePrecision, trueDate)
	}
	if got := mustGetFeedItem(t, db, chID, "t2").Status; got != "vod" {
		t.Fatalf("t2 status = %q, want vod (its exact date already satisfies §12)", got)
	}
}

// TestWalk_DateFetchErrorRetries: a FAILED date fetch is a transient fault —
// treated like an errored probe: nothing written, not in FRESH, the source
// not exhausted; the row retries next cycle.
func TestWalk_DateFetchErrorRetries(t *testing.T) {
	db := newTestDB(t)
	now := fixedNow()
	const chID = "UC1"
	seedRow(t, db, chID, "e1", now.Add(-1*time.Hour), "coarse", "videos", "unknown")
	seedRow(t, db, chID, "e2", now.Add(-2*time.Hour), "coarse", "videos", "unknown")

	var order []string
	fm := newTestFeedMonitor(t, db,
		withProbe(func(ctx context.Context, id string) (*VideoProbeResult, error) {
			order = append(order, id)
			return &VideoProbeResult{StreamStatus: "vod", Title: id}, nil
		}),
		withProbeDate(func(ctx context.Context, id string) (string, string, error) {
			return "", "", fmt.Errorf("boom")
		}),
		withNow(now))
	fresh := fm.walkForTest(t, chID)

	if got := mustGetFeedItem(t, db, chID, "e1").Status; got != "unknown" {
		t.Fatalf("e1 status = %q, want unknown — a failed date fetch must not write", got)
	}
	if _, ok := fresh["e1"]; ok {
		t.Fatal("a date-fetch-failed row must not enter FRESH")
	}
	if !contains(order, "e2") {
		t.Fatalf("e2 not probed — a date-fetch failure must not exhaust the source; order=%v", order)
	}
}

// escalationProbes builds the paired anon/auth recorders the escalation
// tests (spec §9/§17) share: each records every probed videoID in call order
// and replies with its fixed result (or an error when err is non-nil).
func escalationProbes(anonRes, authRes *VideoProbeResult, anonCalls, authCalls *[]string) (anon, auth VideoProbeFunc) {
	anon = func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		*anonCalls = append(*anonCalls, videoID)
		return anonRes, nil
	}
	auth = func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		*authCalls = append(*authCalls, videoID)
		return authRes, nil
	}
	return anon, auth
}

// membersOnlyRefusal is the probe result of an anonymous probe hitting
// members-only content: YouTube returns no formats and the playability
// error names why. isDenied (upcoming + members_only) classifies it
// OutcomeDenied — but PlayabilityError is carried either way, and the
// escalation keys on it, not on the outcome.
func membersOnlyRefusal() *VideoProbeResult {
	return &VideoProbeResult{StreamStatus: "upcoming", PlayabilityError: "members_only"}
}

// TestEscalation_MembersOnlyFlipsAndEscalatesOnce covers spec §17's headline
// escalation case: an anonymous probe's members_only refusal flips the row's
// source to 'membership' in the store AND retries with the authenticated
// probe the SAME cycle — with the probe cooldown ACTIVE, proving the retry
// bypasses it (the refusal itself just recorded the cooldown; a retry gated
// on it would suppress escalation entirely for any probe_cooldown > 0). The
// auth probe classifies vod+started+date ⇒ OutcomeProbed ⇒ in the FRESH map
// (jobbed later by the ARCHIVE step). Next cycle the flipped row costs
// exactly ONE probe, authenticated.
func TestEscalation_MembersOnlyFlipsAndEscalatesOnce(t *testing.T) {
	db := newTestDB(t)
	now := fixedNow()
	const chID = "UC1"
	seedRow(t, db, chID, "e1", now.Add(-1*time.Hour), "exact", "rss", "unknown")

	var anonCalls, authCalls []string
	anon, auth := escalationProbes(membersOnlyRefusal(), &VideoProbeResult{
		StreamStatus: "vod", Title: "e1",
		PublishedAt:        now.Add(-2 * time.Hour).UTC().Format(time.RFC3339),
		PublishedPrecision: "started",
	}, &anonCalls, &authCalls)

	fm := newTestFeedMonitor(t, db, withProbe(anon), withProbeAuth(auth), withMembership(membWith()), withNow(now))
	fm.ProbeCooldown.SetDuration(time.Hour) // make the same-cycle bypass observable

	fresh := fm.walkForTest(t, chID)

	if len(anonCalls) != 1 || anonCalls[0] != "e1" {
		t.Fatalf("cycle 1 anonymous probes = %v, want [e1]", anonCalls)
	}
	if len(authCalls) != 1 || authCalls[0] != "e1" {
		t.Fatalf("cycle 1 authenticated probes = %v, want [e1] — the refusal must escalate the SAME cycle, bypassing the just-recorded cooldown", authCalls)
	}
	if got := mustGetFeedItem(t, db, chID, "e1").Source; got != "membership" {
		t.Fatalf("source after refusal = %q, want membership", got)
	}
	if res, ok := fresh["e1"]; !ok || res.Outcome != OutcomeProbed {
		t.Fatalf("fresh[e1] = %+v (present=%v), want OutcomeProbed — the escalated result is the row's classification", res, ok)
	}
	if got := mustGetFeedItem(t, db, chID, "e1").Status; got != "vod" {
		t.Fatalf("status after escalated probe = %q, want vod", got)
	}

	// Next cycle: the flipped row routes straight to the authenticated
	// probe — exactly one probe, zero anonymous. (Cooldown back to the
	// default 0 = disabled; leaving the 1h window on would suppress the
	// probe outright and assert nothing about the choice.)
	fm.ProbeCooldown.SetDuration(0)
	anonCalls, authCalls = nil, nil
	fm.walkForTest(t, chID)
	if len(anonCalls) != 0 {
		t.Fatalf("cycle 2 anonymous probes = %v, want none — membership rows probe authenticated", anonCalls)
	}
	if len(authCalls) != 1 || authCalls[0] != "e1" {
		t.Fatalf("cycle 2 authenticated probes = %v, want exactly [e1]", authCalls)
	}
}

// TestEscalation_FailedEscalationStillFlips covers spec §17: when the
// authenticated retry ALSO returns members_only (cookies stale, or not that
// channel's member), the row classifies OutcomeDenied — but the source flip
// persists, so the next cycle costs one authenticated probe, not an
// anon-then-auth pair every cycle forever.
func TestEscalation_FailedEscalationStillFlips(t *testing.T) {
	db := newTestDB(t)
	now := fixedNow()
	const chID = "UC1"
	seedRow(t, db, chID, "e1", now.Add(-1*time.Hour), "exact", "rss", "unknown")

	var anonCalls, authCalls []string
	anon, auth := escalationProbes(membersOnlyRefusal(), membersOnlyRefusal(), &anonCalls, &authCalls)
	fm := newTestFeedMonitor(t, db, withProbe(anon), withProbeAuth(auth), withMembership(membWith()), withNow(now))

	fresh := fm.walkForTest(t, chID)

	if len(anonCalls) != 1 || len(authCalls) != 1 {
		t.Fatalf("cycle 1 probes anon=%v auth=%v, want one of each", anonCalls, authCalls)
	}
	if _, ok := fresh["e1"]; ok {
		t.Fatal("a denied escalation must NOT enter the FRESH map — there is nothing to disposition")
	}
	row := mustGetFeedItem(t, db, chID, "e1")
	if row.Source != "membership" {
		t.Fatalf("source after failed escalation = %q, want membership — the flip is unconditional", row.Source)
	}
	if row.Status != "unknown" {
		t.Fatalf("status after failed escalation = %q, want unknown — denied writes nothing", row.Status)
	}

	// Next cycle: already-membership row, denied again — ONE auth probe,
	// no second (row.Source is already membership ⇒ no re-escalation).
	anonCalls, authCalls = nil, nil
	fm.walkForTest(t, chID)
	if len(anonCalls) != 0 || len(authCalls) != 1 {
		t.Fatalf("cycle 2 probes anon=%v auth=%v, want zero anon and exactly one auth", anonCalls, authCalls)
	}
}

// TestEscalation_LoginRequiredNeverRelabels covers spec §9's other refusal:
// login_required on a public video is anti-bot pushback, not a membership
// signal — it must neither relabel the row nor burn an authenticated probe.
func TestEscalation_LoginRequiredNeverRelabels(t *testing.T) {
	db := newTestDB(t)
	now := fixedNow()
	const chID = "UC1"
	seedRow(t, db, chID, "e2", now.Add(-1*time.Hour), "exact", "rss", "unknown")

	var anonCalls, authCalls []string
	anon, auth := escalationProbes(&VideoProbeResult{StreamStatus: "upcoming", PlayabilityError: "login_required"}, nil, &anonCalls, &authCalls)
	// Membership fully active — the ONLY thing stopping escalation here must
	// be the login_required rule itself.
	fm := newTestFeedMonitor(t, db, withProbe(anon), withProbeAuth(auth), withMembership(membWith()), withNow(now))

	fm.walkForTest(t, chID)

	if len(anonCalls) != 1 {
		t.Fatalf("anonymous probes = %v, want [e2]", anonCalls)
	}
	if len(authCalls) != 0 {
		t.Fatalf("authenticated probes = %v, want none — login_required never escalates", authCalls)
	}
	if got := mustGetFeedItem(t, db, chID, "e2").Source; got != "rss" {
		t.Fatalf("source after login_required = %q, want rss — never relabeled", got)
	}
}

// TestEscalation_NoCookiesNoEscalation covers spec §17: with membership
// inactive (no fetcher wired ⇒ no cookies), a members_only refusal still
// flips the source — the sighting is real and must not be forgotten — but
// no authenticated retry runs, and the next cycle the membership cookie
// gate parks the row entirely (zero probes) until cookies return.
func TestEscalation_NoCookiesNoEscalation(t *testing.T) {
	db := newTestDB(t)
	now := fixedNow()
	const chID = "UC1"
	seedRow(t, db, chID, "e3", now.Add(-1*time.Hour), "exact", "rss", "unknown")

	var anonCalls, authCalls []string
	anon, auth := escalationProbes(membersOnlyRefusal(), nil, &anonCalls, &authCalls)
	// NO withMembership: membershipActive() is false. ProbeVideoAuth is
	// still wired so a wrongly-issued escalation is caught, not nil-paniced.
	fm := newTestFeedMonitor(t, db, withProbe(anon), withProbeAuth(auth), withNow(now))

	fm.walkForTest(t, chID)

	if len(anonCalls) != 1 {
		t.Fatalf("cycle 1 anonymous probes = %v, want [e3]", anonCalls)
	}
	if len(authCalls) != 0 {
		t.Fatalf("cycle 1 authenticated probes = %v, want none — no cookies, no escalation", authCalls)
	}
	if got := mustGetFeedItem(t, db, chID, "e3").Source; got != "membership" {
		t.Fatalf("source after refusal = %q, want membership — the flip does not depend on cookies", got)
	}

	// Next cycle: the row is source='membership' and membership is inactive
	// — the cookie gate skips it before any probe.
	anonCalls, authCalls = nil, nil
	fm.walkForTest(t, chID)
	if len(anonCalls) != 0 || len(authCalls) != 0 {
		t.Fatalf("cycle 2 probes anon=%v auth=%v, want none — the membership gate parks the row until cookies return", anonCalls, authCalls)
	}
}

// TestWalk_RestartCarveOut covers spec §8's restart carve-out: a stream can
// restart on the same video ID, so a store-driven vod+started row with NO
// job row must still be probed (a stream that ended before we saw it live,
// on include_non_live_content=false, is otherwise never looked at again).
// The history row (§8's HasProcessed trap) is seeded via db.AddToHistory
// directly, matching a DECAPI give-up — the predicate is the jobs table,
// never HasProcessed.
func TestWalk_RestartCarveOut(t *testing.T) {
	db := newTestDB(t)
	now := fixedNow()
	seedRow(t, db, "UC1", "r1", now.AddDate(0, 0, -1), "started", "rss", "vod") // in-window ended broadcast
	// History row WITHOUT a job row (the HasProcessed trap, §8): via history only.
	if err := db.AddToHistory("r1"); err != nil {
		t.Fatalf("AddToHistory: %v", err)
	}

	var probed []string
	fm := newTestFeedMonitor(t, db, withProbe(recordingProbe(&probed)), withNow(now))
	fm.walkForTest(t, "UC1")
	if !contains(probed, "r1") {
		t.Fatal("vod+started with NO JOB ROW must be probed — the predicate is the jobs table, NOT HasProcessed")
	}

	// Now add a job row: never probed again.
	addFinishedJob(t, db, "r1")
	probed = nil
	fm.walkForTest(t, "UC1")
	if contains(probed, "r1") {
		t.Fatal("vod+started WITH a job row must not be probed — AddJob dedups re-jobs anyway")
	}
}
