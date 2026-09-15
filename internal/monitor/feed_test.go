package monitor

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// newTestDB opens a fresh temp-file database for a test, closed on cleanup.
// Every Plan 3 test starts from an empty store built by this helper.
func newTestDB(t *testing.T) *database.Database {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// feedMonitorOpt configures a FeedMonitor built by newTestFeedMonitor. Each
// withXxx helper below sets one field. Later Plan 3 tasks add withProbe,
// withWindowDays, withNow the same way as the corresponding FeedMonitor
// fields land (fm.probeAnon/probeAuth in Task 4, fm.now in Task 2) — no
// unused options ahead of the task that needs them.
type feedMonitorOpt func(*FeedMonitor)

// withRSS injects a fake RSS fetcher in place of the real HTTP GET
// (fm.fetchFeed), via the FetchRSS seam.
func withRSS(fn RSSFetchFunc) feedMonitorOpt {
	return func(fm *FeedMonitor) { fm.FetchRSS = fn }
}

// withMembership injects a fake membership-tab fetcher in place of the real
// youtube.Service.FetchMembershipVideos wiring.
func withMembership(fn MembershipFetchFunc) feedMonitorOpt {
	return func(fm *FeedMonitor) { fm.FetchMembership = fn }
}

// withClock pins fm.now to a caller-controlled instant — withNow's mutable
// twin. The pointed-at value is what fm.now() reports, so a test can advance
// the cycle clock between doCheck calls (the membership memo's horizon is the
// only thing in the package that needs more than one instant).
func withClock(clock *time.Time) feedMonitorOpt {
	return func(fm *FeedMonitor) { fm.now = func() time.Time { return *clock } }
}

// setChannels writes chans into the monitor's config store so doCheck's
// getYouTubeChannels sees them. Tests that drive checkChannel directly never
// needed this; the membership memo is armed per CYCLE, so its tests drive
// doCheck.
func setChannels(fm *FeedMonitor, chans ...config.ChannelConfig) {
	_ = fm.configStore.Update(func(c *config.MoomboxConfig) { c.Channels = chans })
}

// chYT is a minimal enabled YouTube channel with the given ID as both ID and
// display name.
func chYT(id string) config.ChannelConfig {
	return config.ChannelConfig{ID: id, Name: id}
}

// shrinkFeedStagger cuts the inter-channel pacing sleep for a test that drives
// several full cycles (3 channels x 3 cycles would otherwise sleep 3 s).
func shrinkFeedStagger(t *testing.T) {
	t.Helper()
	orig := feedStagger
	feedStagger = time.Millisecond
	t.Cleanup(func() { feedStagger = orig })
}

// withNow pins fm.now to a fixed instant. checkChannel reads it exactly once
// per cycle (the one-`now` rule — spec §7), so this is what makes the
// FETCH/STORE date math (and later, WALK/ARCHIVE) deterministic in tests.
func withNow(t time.Time) feedMonitorOpt {
	return func(fm *FeedMonitor) { fm.now = func() time.Time { return t } }
}

// withProbe wires fm.ProbeVideo to fn. probeRow (walk.go) falls back to
// ProbeVideo for every row whose source isn't "membership", or whenever
// ProbeVideoAuth is unset (as it is by default in tests) — so this one
// fixture serves both anonymous and membership rows unless a test also
// wires ProbeVideoAuth separately.
//
// A probe is NOT optional for a checkChannel cycle: the WALK step probes
// unconditionally and probeAndClassify panics on a nil ProbeVideo by
// design (fail-loud — a production wiring regression must panic visibly,
// not silently stop archiving). Tests that only assert FETCH/STORE
// outcomes wire withProbe(stubProbeErrored()).
func withProbe(fn VideoProbeFunc) feedMonitorOpt {
	return func(fm *FeedMonitor) { fm.ProbeVideo = fn }
}

// withProbeAuth wires fm.ProbeVideoAuth to fn — the AUTHENTICATED probe
// probeRow selects for source='membership' rows and for the same-cycle
// members_only escalation (walk.go). Tests that never touch members-only
// content leave it unset and everything falls back to withProbe's fixture.
func withProbeAuth(fn VideoProbeFunc) feedMonitorOpt {
	return func(fm *FeedMonitor) { fm.ProbeVideoAuth = fn }
}

// withProbeDate wires fm.ProbeDate — the date-completing half of the
// two-phase probe (§9): called when a vod-family probe returns no date and
// the row's own date is only an estimate (coarse/assumed). Tests whose
// probes always carry dates leave it nil.
func withProbeDate(fn func(ctx context.Context, videoID string) (string, string, error)) feedMonitorOpt {
	return func(fm *FeedMonitor) { fm.ProbeDate = fn }
}

// stubProbeErrored returns a VideoProbeFunc that always fails — the minimal
// probe for tests that assert only FETCH/STORE outcomes. An errored probe
// has no store effect (OutcomeErrored writes nothing, never exhausts), so
// wiring it satisfies the WALK step's required-probe contract without
// perturbing what those tests assert.
func stubProbeErrored() VideoProbeFunc {
	return func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		return nil, fmt.Errorf("stub probe: no probe behavior wired for this test")
	}
}

// withWindowDays sets monitors.archive_window_days on the test monitor's
// config store, which fm.archiveWindowDays(ch) reads for an unconfigured
// channel (no per-channel ArchiveWindowDays override).
func withWindowDays(days int) feedMonitorOpt {
	return func(fm *FeedMonitor) {
		_ = fm.configStore.Update(func(c *config.MoomboxConfig) { c.Monitors.ArchiveWindowDays = days })
	}
}

// newTestFeedMonitor builds a FeedMonitor over a real (temp-file) db and an
// in-memory config store, ready for a test to drive checkChannel/a cycle.
// Every Plan 3 test builds its monitor through this constructor plus the
// feedMonitorOpt helpers.
func newTestFeedMonitor(t *testing.T, db *database.Database, opts ...feedMonitorOpt) *FeedMonitor {
	t.Helper()
	fm := NewFeedMonitor(
		config.NewStore(config.Defaults(), ""),
		db,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	for _, opt := range opts {
		opt(fm)
	}
	return fm
}

// rssItem is one inline RSS fixture entry for rssWith/rssXML. Published is an
// RFC3339 string; empty omits the <published> element entirely, mirroring a
// real feed entry missing the field (parseFeedCandidates then reads a zero
// time — see TestParseFeedCandidates in membership_test.go).
type rssItem struct {
	ID        string
	Title     string
	Published string
}

// rssXML renders items into a minimal Atom feed matching YouTube's channel
// RSS shape (same element set parseFeedCandidates' atomFeed/atomEntry
// structs expect — copied from membership_test.go's inline fixture).
func rssXML(items ...rssItem) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?>` + "\n")
	b.WriteString(`<feed xmlns:yt="http://www.youtube.com/xml/schemas/2015" xmlns:media="http://search.yahoo.com/mrss/" xmlns="http://www.w3.org/2005/Atom">` + "\n")
	for _, it := range items {
		fmt.Fprintf(&b, "  <entry><yt:videoId>%s</yt:videoId><title>%s</title>\n", it.ID, it.Title)
		fmt.Fprintf(&b, "    <link rel=\"alternate\" href=\"https://youtu.be/%s\"/>\n", it.ID)
		if it.Published != "" {
			fmt.Fprintf(&b, "    <published>%s</published>\n", it.Published)
		}
		b.WriteString("    <media:group><media:description></media:description></media:group></entry>\n")
	}
	b.WriteString("</feed>")
	return []byte(b.String())
}

// rssWith returns an RSSFetchFunc serving items as a 200 response.
func rssWith(items ...rssItem) RSSFetchFunc {
	return func(ctx context.Context, ch *config.ChannelConfig) ([]byte, error) {
		return rssXML(items...), nil
	}
}

// rss404 returns an RSSFetchFunc that fails exactly like fetchFeed's non-200
// path (see feed.go's fetchFeed: `fmt.Errorf("feed http %d", resp.StatusCode)`),
// so tests can exercise "an RSS failure never establishes" without a real
// HTTP round trip.
func rss404() RSSFetchFunc {
	return func(ctx context.Context, ch *config.ChannelConfig) ([]byte, error) {
		return nil, fmt.Errorf("feed http %d", http.StatusNotFound)
	}
}

// membWith adapts youtube.MembershipVideo fixtures — the real fetcher's
// return type — into a MembershipFetchFunc, mirroring the production adapter
// closure in cmd/moombox/monitor_callbacks.go (youtube.MembershipVideo ->
// monitor.MembershipVideo). It answers confirmedNonMember=false: a fixture of
// a successful MEMBER fetch, empty list or not, which is what keeps every
// pre-existing test fetching on every cycle.
func membWith(videos ...youtube.MembershipVideo) MembershipFetchFunc {
	return func(ctx context.Context, channelID string) ([]MembershipVideo, bool, error) {
		out := make([]MembershipVideo, len(videos))
		for i, v := range videos {
			out[i] = MembershipVideo{VideoID: v.VideoID, Title: v.Title, Age: v.Age}
		}
		return out, false, nil
	}
}

// testRelativeAgeRe/testParseAge parse YouTube's membership-tab relative-age
// text ("3 weeks ago") the same way internal/youtube/channel_membership.go's
// (unexported) relativeAgeRe/itemAge do — duplicated here because monitor
// tests build fixtures from the same vocabulary but can't reach that
// package's unexported regex.
var testRelativeAgeRe = regexp.MustCompile(`(\d+)\s+(second|minute|hour|day|week|month|year)s?\s+ago`)

func testParseAge(s string) time.Duration {
	m := testRelativeAgeRe.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	var unit time.Duration
	switch m[2] {
	case "second":
		unit = time.Second
	case "minute":
		unit = time.Minute
	case "hour":
		unit = time.Hour
	case "day":
		unit = 24 * time.Hour
	case "week":
		unit = 7 * 24 * time.Hour
	case "month":
		unit = 30 * 24 * time.Hour
	case "year":
		unit = 365 * 24 * time.Hour
	}
	return time.Duration(n) * unit
}

// memberVideo builds a youtube.MembershipVideo fixture from a relative-age
// string as rendered on a channel's /membership tab — e.g. "3 weeks ago", or
// "" for an undatable item (Age 0, same treatment as live/upcoming — see
// itemAge's doc comment in channel_membership.go).
func memberVideo(id, ageText string) youtube.MembershipVideo {
	return youtube.MembershipVideo{VideoID: id, Title: id, Age: testParseAge(ageText)}
}

// runCycleForTest drives one checkChannel cycle for a synthesized channel —
// the way doCheck would for a configured one. The returned health error is
// swallowed (logged, not asserted): these tests check store/channel_state
// outcomes, matching doCheck's own handling of a per-channel error.
func (fm *FeedMonitor) runCycleForTest(t *testing.T, channelID string) {
	t.Helper()
	ch := &config.ChannelConfig{ID: channelID, Name: channelID, IncludeNonLiveContent: true}
	if err := fm.checkChannel(context.Background(), ch); err != nil {
		t.Logf("checkChannel(%s): %v", channelID, err)
	}
}

// establishedForTest reports whether channel_state.last_rss_ok_at has been
// written for channelID — the FETCH step's success signal (spec §7/§11).
func establishedForTest(t *testing.T, db *database.Database, channelID string) bool {
	t.Helper()
	ts, err := db.GetChannelRSSOK(channelID)
	if err != nil {
		t.Fatalf("GetChannelRSSOK(%s): %v", channelID, err)
	}
	return ts != ""
}

// mustGetFeedItem fetches a feed_items row via the database package's
// exported GetFeedItem, failing the test if the row is missing or the query
// errors. Package `database` has its own unexported mustGetFeedItem for its
// own tests; this is the monitor-package equivalent, going through the
// public API only.
func mustGetFeedItem(t *testing.T, db *database.Database, channelID, videoID string) *database.FeedItem {
	t.Helper()
	it, err := db.GetFeedItem(channelID, videoID)
	if err != nil {
		t.Fatalf("GetFeedItem(%s, %s): %v", channelID, videoID, err)
	}
	if it == nil {
		t.Fatalf("GetFeedItem(%s, %s): no row", channelID, videoID)
	}
	return it
}

// TestFeedMonitorSmoke proves the constructor + opts scaffold works
// end-to-end, including the new FetchRSS seam: one checkChannel pass against
// a fresh, empty store, with both fetchers faked, completes without a panic
// or error and reaches OnVideoFound for the fixture's video. The probe stub
// reports "live" — a successful classification — because this test's
// assertion is the job pipeline's endpoint (OnVideoFound), which an errored
// probe would legitimately never reach.
func TestFeedMonitorSmoke(t *testing.T) {
	db := newTestDB(t)
	fm := newTestFeedMonitor(t, db,
		withRSS(rssWith(rssItem{ID: "vidSmoke01", Title: "hello world", Published: "2026-07-14T00:00:00Z"})),
		withMembership(membWith()),
		withProbe(func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
			return &VideoProbeResult{StreamStatus: "live", Title: "hello world"}, nil
		}),
		withNow(time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)), // pins the fixture's <published> inside the window
	)

	var found []string
	fm.OnVideoFound = func(videoID, title, url string, ch *config.ChannelConfig, d JobDisposition) {
		found = append(found, videoID)
	}

	ch := &config.ChannelConfig{ID: "UCsmoke", Name: "Smoke Channel"}
	if err := fm.checkChannel(context.Background(), ch); err != nil {
		t.Fatalf("checkChannel: %v", err)
	}
	if len(found) != 1 || found[0] != "vidSmoke01" {
		t.Errorf("OnVideoFound = %v, want [vidSmoke01]", found)
	}
}

// TestStoreStep_UpsertsAndClassifiesDates covers the STORE step's date/source
// classification (spec §7): RSS items land exact/rss; membership items land
// coarse (dated) or assumed (undatable), always source=membership; every row
// lands status='unknown' regardless of source (the upsert enforces it).
func TestStoreStep_UpsertsAndClassifiesDates(t *testing.T) {
	db := newTestDB(t)
	fm := newTestFeedMonitor(t, db,
		withRSS(rssWith(rssItem{ID: "pub1", Published: "2026-07-15T10:00:00Z", Title: "A"})),
		withMembership(membWith(
			memberVideo("m1", "3 weeks ago"), // coarse: now - 21d
			memberVideo("m2", ""),            // undatable: assumed, published = cycle now
		)),
		// This test asserts STORE outcomes only; an errored probe writes
		// nothing to the store, so the WALK leaves these rows exactly as
		// the STORE step wrote them.
		withProbe(stubProbeErrored()))
	fm.runCycleForTest(t, "UC1")

	pub1 := mustGetFeedItem(t, db, "UC1", "pub1")
	if pub1.DatePrecision != "exact" || pub1.Source != "rss" || pub1.Status != "unknown" {
		t.Fatalf("rss row: %+v", pub1)
	}
	m1 := mustGetFeedItem(t, db, "UC1", "m1")
	if m1.DatePrecision != "coarse" || m1.Source != "membership" {
		t.Fatalf("coarse row: %+v", m1)
	}
	m2 := mustGetFeedItem(t, db, "UC1", "m2")
	if m2.DatePrecision != "assumed" {
		t.Fatalf("assumed row: %+v", m2)
	}
}

// TestStoreStep_MissingPublishedStoresAssumedNow covers the STORE step's
// zero-date arm (spec §12): an RSS entry with a missing (or unparseable)
// <published> parses to the zero time, and the store must record it as
// precision 'assumed' with published = the cycle's now — a claim of
// ignorance that Q2's unresolved arm keeps in scope until a probe supplies
// a real date. Writing it as 'exact'/0001-01-01T00:00:00Z instead would be
// a permanent unhealable sink: outside Q1 forever, in neither Q2 arm, and
// rank-4 'exact' blocks every later correction — violating §12's "nothing
// is excluded on a date we have not verified".
func TestStoreStep_MissingPublishedStoresAssumedNow(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	fm := newTestFeedMonitor(t, db,
		withRSS(rssWith(rssItem{ID: "nodate1", Title: "no date"})), // Published "" omits <published>
		withMembership(membWith()),
		withProbe(stubProbeErrored()),
		withNow(now))
	fm.runCycleForTest(t, "UC1")

	got := mustGetFeedItem(t, db, "UC1", "nodate1")
	if got.DatePrecision != "assumed" || got.Published != now.Format(time.RFC3339) {
		t.Fatalf("dateless RSS row = %+v, want precision=assumed published=%s (the cycle's now)",
			got, now.Format(time.RFC3339))
	}
}

// TestFetchStep_RSSSuccessEstablishes_404DoesNot covers the FETCH step's
// established-gate write (spec §7/§11): last_rss_ok_at is written on a
// transport SUCCESS, immediately in FETCH — a 404 must not establish, and an
// empty-but-200 response must (the §11 residual: a fetch, not a parse, is the
// gate).
func TestFetchStep_RSSSuccessEstablishes_404DoesNot(t *testing.T) {
	db := newTestDB(t)
	fm := newTestFeedMonitor(t, db, withRSS(rss404()), withMembership(membWith()), withProbe(stubProbeErrored()))
	fm.runCycleForTest(t, "UC1")
	if establishedForTest(t, db, "UC1") {
		t.Fatal("404 must not establish")
	}
	fm2 := newTestFeedMonitor(t, db, withRSS(rssWith()), withMembership(membWith()), withProbe(stubProbeErrored()))
	fm2.runCycleForTest(t, "UC1") // zero entries but 200 — still establishes (§11 residual)
	if !establishedForTest(t, db, "UC1") {
		t.Fatal("empty-but-200 RSS must establish")
	}
}

// TestFeed_MembershipMemoSkipsNonMembersButNotMembers pins two of T2-13's
// three cases: a "not a member" answer suppresses the authenticated ~1 MB
// fetch for membershipMemoTTL, and a MEMBER is fetched every cycle.
//
// Mutants this fails on:
//   - no memo: UC1/UC2 are fetched on every cycle (the 7,200 loads/day bug).
//   - memoizing on an empty video list: UC3 (a member) stops being fetched,
//     and a members-only live stream goes undiscovered for up to 6 h.
//   - a horizon that never expires: the third cycle does not re-check UC1.
func TestFeed_MembershipMemoSkipsNonMembersButNotMembers(t *testing.T) {
	shrinkFeedStagger(t)
	db := newTestDB(t)
	clock := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	fetches := map[string]int{}
	fm := newTestFeedMonitor(t, db,
		withRSS(rssWith()),
		withProbe(stubProbeErrored()),
		withClock(&clock),
		withMembership(func(ctx context.Context, channelID string) ([]MembershipVideo, bool, error) {
			fetches[channelID]++
			if channelID == "UC3" {
				// A member: nothing to memoize, so this channel is fetched
				// every cycle for as long as it stays one.
				return []MembershipVideo{{VideoID: "memberVid01", Title: "members only"}}, false, nil
			}
			return nil, true, nil // recognised session, confirmed non-member
		}),
	)
	setChannels(fm, chYT("UC1"), chYT("UC2"), chYT("UC3"))

	fm.doCheck(context.Background()) // cycle 1: nothing memoized yet
	assertFetches(t, "cycle 1", fetches, map[string]int{"UC1": 1, "UC2": 1, "UC3": 1})

	clock = clock.Add(10 * time.Minute)
	fm.doCheck(context.Background()) // cycle 2: only the member
	assertFetches(t, "cycle 2", fetches, map[string]int{"UC1": 1, "UC2": 1, "UC3": 2})

	clock = clock.Add(membershipMemoTTL)
	fm.doCheck(context.Background()) // cycle 3: the horizon expired
	assertFetches(t, "cycle 3", fetches, map[string]int{"UC1": 2, "UC2": 2, "UC3": 3})
}

// TestFeed_MembershipMemoAlwaysFetchesOneForLiveness pins T2-13's third case
// and the reason the memo is safe at all. The authenticated membership fetch
// is the system's preferred YouTube liveness probe: cmd/moombox's
// FetchMembership adapter routes its SessionAuthState to ObserveLiveness, and
// only when the fetch actually runs. A cycle where EVERY channel is memoized
// must still fetch exactly one, and must rotate — otherwise a dead session is
// never observed. armMembershipLiveness' doc comment has the exact bound (a
// fetch that RETURNS, not a verdict) and names the tier-2 backstop.
//
// Mutants this fails on:
//   - skipping every memoized channel: cycle 2 makes zero fetches and the
//     liveness signal goes dark.
//   - fetching every memoized channel "for liveness": cycle 2 makes three.
//   - a fixed nominee: cycle 3 re-fetches UC1 instead of rotating to UC2.
func TestFeed_MembershipMemoAlwaysFetchesOneForLiveness(t *testing.T) {
	shrinkFeedStagger(t)
	db := newTestDB(t)
	clock := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	var order []string
	fm := newTestFeedMonitor(t, db,
		withRSS(rssWith()),
		withProbe(stubProbeErrored()),
		withClock(&clock),
		withMembership(func(ctx context.Context, channelID string) ([]MembershipVideo, bool, error) {
			order = append(order, channelID)
			return nil, true, nil
		}),
	)
	setChannels(fm, chYT("UC1"), chYT("UC2"), chYT("UC3"))

	fm.doCheck(context.Background())
	if got := len(order); got != 3 {
		t.Fatalf("cycle 1 fetches = %d (%v), want 3", got, order)
	}

	clock = clock.Add(10 * time.Minute)
	order = nil
	fm.doCheck(context.Background())
	if len(order) != 1 || order[0] != "UC1" {
		t.Fatalf("cycle 2 fetches = %v, want exactly [UC1] — one nominated fetch keeps the liveness signal alive", order)
	}

	clock = clock.Add(10 * time.Minute)
	order = nil
	fm.doCheck(context.Background())
	if len(order) != 1 || order[0] != "UC2" {
		t.Fatalf("cycle 3 fetches = %v, want exactly [UC2] — the nomination must rotate to the earliest horizon", order)
	}
}

// assertFetches compares a per-channel fetch tally against want, naming the
// channel that diverged.
func assertFetches(t *testing.T, label string, got, want map[string]int) {
	t.Helper()
	for id, n := range want {
		if got[id] != n {
			t.Fatalf("%s: %s fetched %d times, want %d (all: %v)", label, id, got[id], n, got)
		}
	}
}

// TestFeed_MembershipFailedFetchWritesNoMemo is the guard on the memo's entry
// condition. MembershipFetchFunc says an error leaves both questions
// unanswered; the monitor must ENFORCE that rather than trust the fetcher, so
// this fixture deliberately fills the bit in alongside its error — the shape a
// careless adapter would produce.
//
// Mutant: moving recordMembershipSuccess out of the success branch (or having
// the error branch pass the fetcher's bit through) memoizes UC1 on a fetch
// that never answered, and members-only discovery for it goes dark for 6 h on
// the strength of one HTTP 503.
func TestFeed_MembershipFailedFetchWritesNoMemo(t *testing.T) {
	shrinkFeedStagger(t)
	db := newTestDB(t)
	clock := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	var order []string
	fm := newTestFeedMonitor(t, db,
		withRSS(rssWith()),
		withProbe(stubProbeErrored()),
		withClock(&clock),
		withMembership(func(ctx context.Context, channelID string) ([]MembershipVideo, bool, error) {
			order = append(order, channelID)
			return nil, true, fmt.Errorf("membership tab http 503")
		}),
	)
	setChannels(fm, chYT("UC1"))

	fm.doCheck(context.Background())

	fm.mu.Lock()
	_, memoized := fm.nonMemberUntil["UC1"]
	fm.mu.Unlock()
	if memoized {
		t.Fatal("a failed fetch memoized UC1 as a non-member — an error answers neither question, whatever the fetcher put in the bit")
	}

	clock = clock.Add(10 * time.Minute)
	order = nil
	fm.doCheck(context.Background())
	if len(order) != 1 || order[0] != "UC1" {
		t.Fatalf("cycle 2 fetches = %v, want [UC1] — an un-memoized channel is fetched every cycle", order)
	}
}

// TestFeed_MembershipMemoNominatesDespiteAPersistentlyFailingChannel pins the
// half of the liveness floor that a fetch ATTEMPT cannot carry.
//
// UC0's /membership page always errors. It therefore never gets a memo, so it
// is fetched every cycle — but that fetch answers nothing, neither for
// discovery nor for the session. If the arm treated it as "a channel is being
// fetched on its own account, no nomination needed", every memoized channel
// would skip and the cycle's only membership fetch would be the one that
// cannot produce a verdict: one broken channel switching the tier-1 liveness
// signal off for the whole install, indefinitely.
//
// UC0 is deliberately LAST in the channel list. That is what makes the arm's
// decision load-bearing rather than decorative: the re-arm inside
// recordMembershipFetchError can hand the duty to a memoized channel that
// comes AFTER the failure, so a failing channel in front of others is rescued
// either way. With nothing behind it, only a nomination made up front — by an
// arm that refuses to count it — saves the cycle.
//
// Mutant: dropping the membershipFetchErrored check from armMembershipLiveness
// — cycles 2+ then fetch UC0 alone and observe nothing.
func TestFeed_MembershipMemoNominatesDespiteAPersistentlyFailingChannel(t *testing.T) {
	shrinkFeedStagger(t)
	db := newTestDB(t)
	clock := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	var order []string
	fm := newTestFeedMonitor(t, db,
		withRSS(rssWith()),
		withProbe(stubProbeErrored()),
		withClock(&clock),
		withMembership(func(ctx context.Context, channelID string) ([]MembershipVideo, bool, error) {
			order = append(order, channelID)
			if channelID == "UC0" {
				return nil, false, fmt.Errorf("membership tab http 500")
			}
			return nil, true, nil // recognised session, confirmed non-member
		}),
	)
	setChannels(fm, chYT("UC1"), chYT("UC2"), chYT("UC0"))

	fm.doCheck(context.Background()) // cycle 1: nothing memoized; all three fetched

	// Three cycles, because the stand-in must also ROTATE: UC1, then UC2 (its
	// horizon is now the earlier one), then UC1 again.
	wantStandIn := []string{"UC1", "UC2", "UC1"}
	for i, want := range wantStandIn {
		cycle := i + 2
		clock = clock.Add(10 * time.Minute)
		order = nil
		fm.doCheck(context.Background())

		if len(order) != 2 {
			t.Fatalf("cycle %d fetches = %v, want exactly 2 — one memoized stand-in that can actually answer, plus the failing channel", cycle, order)
		}
		if order[0] != want {
			t.Fatalf("cycle %d fetches = %v, want %s first — the nomination goes to the earliest horizon and rotates", cycle, order, want)
		}
		if order[1] != "UC0" {
			t.Fatalf("cycle %d fetches = %v, want UC0 last — it is never memoized, so it is always fetched", cycle, order)
		}
	}
}

// TestFeed_MembershipLivenessFailsOverWhenTheNomineeErrors pins the other
// half: the nomination is satisfied by a fetch that RETURNS, not by one that
// is merely attempted. The nominee here fails, so the duty has to pass to
// another memoized channel in the same cycle.
//
// Mutants this fails on:
//   - retiring the obligation on an ATTEMPT rather than on a fetch that came
//     back (recordMembershipFetchError clearing membershipLivenessNeeded
//     instead of re-arming it): UC2 is skipped and the cycle ends with no
//     answer at all.
//   - keeping the nomination pinned to the channel that just failed: same
//     result, and across cycles the failing nominee keeps winning the
//     earliest-horizon pick — its horizon never advances, because a failed
//     fetch writes no memo — so the floor never recovers.
//   - dropping the membershipLivenessMaxTries ceiling: the last cycle walks
//     all three channels instead of stopping at two.
//
// NOT a mutant here: making membershipFetchAllowed consume the nomination
// itself. Given the re-arm this is behaviourally identical, which is why the
// predicate is left pure — the state transitions all live in the two record
// helpers, where the fetch's outcome is actually known.
func TestFeed_MembershipLivenessFailsOverWhenTheNomineeErrors(t *testing.T) {
	shrinkFeedStagger(t)
	db := newTestDB(t)
	clock := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	var order []string
	failing := ""
	fm := newTestFeedMonitor(t, db,
		withRSS(rssWith()),
		withProbe(stubProbeErrored()),
		withClock(&clock),
		withMembership(func(ctx context.Context, channelID string) ([]MembershipVideo, bool, error) {
			order = append(order, channelID)
			if failing == "*" || channelID == failing {
				return nil, false, fmt.Errorf("membership tab http 500")
			}
			return nil, true, nil
		}),
	)
	setChannels(fm, chYT("UC1"), chYT("UC2"), chYT("UC3"))

	fm.doCheck(context.Background()) // cycle 1: all three fetched and memoized
	if len(order) != 3 {
		t.Fatalf("cycle 1 fetches = %v, want all three", order)
	}

	// Cycle 2 nominates UC1 (every horizon is equal, so the first wins). Make
	// that fetch fail.
	failing = "UC1"
	clock = clock.Add(10 * time.Minute)
	order = nil
	fm.doCheck(context.Background())
	if len(order) != 2 || order[0] != "UC1" || order[1] != "UC2" {
		t.Fatalf("cycle 2 fetches = %v, want [UC1 UC2] — UC1's fetch never returned, so the floor is still owed and UC2 carries it", order)
	}

	// And the budget is a ceiling, not a licence to walk the list: with every
	// fetch failing, the cycle stops after membershipLivenessMaxTries. The
	// errored set is cleared first so only the budget is under test.
	failing = "*"
	fm.mu.Lock()
	fm.membershipFetchErrored = nil
	fm.mu.Unlock()
	clock = clock.Add(10 * time.Minute)
	order = nil
	fm.doCheck(context.Background())
	if len(order) != membershipLivenessMaxTries {
		t.Fatalf("cycle 3 fetches = %v, want %d — a cycle where every fetch fails must not walk the whole channel list", order, membershipLivenessMaxTries)
	}
}

// TestFeed_ResetMembershipMemoRefetchesEveryChannel pins the repair path. The
// memo suppresses the only discovery source there is for members-only content,
// so an operator who re-imports cookies must not wait out membershipMemoTTL —
// cmd/moombox calls this from OnAuthRecovered.
//
// Mutant: an empty ResetMembershipMemo body — the cycle after the repair still
// fetches only the nominated channel.
func TestFeed_ResetMembershipMemoRefetchesEveryChannel(t *testing.T) {
	shrinkFeedStagger(t)
	db := newTestDB(t)
	clock := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	var order []string
	fm := newTestFeedMonitor(t, db,
		withRSS(rssWith()),
		withProbe(stubProbeErrored()),
		withClock(&clock),
		withMembership(func(ctx context.Context, channelID string) ([]MembershipVideo, bool, error) {
			order = append(order, channelID)
			return nil, true, nil
		}),
	)
	setChannels(fm, chYT("UC1"), chYT("UC2"), chYT("UC3"))

	fm.doCheck(context.Background()) // memoizes all three

	clock = clock.Add(10 * time.Minute)
	order = nil
	fm.doCheck(context.Background())
	if len(order) != 1 {
		t.Fatalf("cycle 2 fetches = %v, want exactly 1 — the memo is in force", order)
	}

	if n := fm.ResetMembershipMemo(); n != 3 {
		t.Fatalf("ResetMembershipMemo() = %d, want 3 — the count is what the repair log reports", n)
	}

	clock = clock.Add(10 * time.Minute)
	order = nil
	fm.doCheck(context.Background())
	if len(order) != 3 {
		t.Fatalf("cycle 3 fetches = %v, want all three — a credential repair must not have to wait out membershipMemoTTL", order)
	}
}

// countingReader serves n bytes and reports how many were actually read.
type countingReader struct{ remaining, read int }

func (c *countingReader) Read(p []byte) (int, error) {
	if c.remaining <= 0 {
		return 0, io.EOF
	}
	n := min(len(p), c.remaining)
	c.remaining -= n
	c.read += n
	return n, nil
}

// TestDrainBoundedStopsAtTheLimit pins T4-35's feed half: a non-200 body is
// drained only far enough to keep the connection reusable, never in full.
//
// Mutant: the bare io.Copy(io.Discard, resp.Body) the feed fetcher used reads
// the whole 1 MB error page to throw it away.
func TestDrainBoundedStopsAtTheLimit(t *testing.T) {
	big := &countingReader{remaining: 1 << 20}
	drainBounded(big)
	if big.read != monitorDrainLimit {
		t.Fatalf("drained %d bytes of a 1MB body, want exactly %d", big.read, monitorDrainLimit)
	}

	// A body shorter than the limit still drains completely and returns.
	small := &countingReader{remaining: 17}
	drainBounded(small)
	if small.read != 17 {
		t.Fatalf("drained %d bytes of a 17-byte body, want 17", small.read)
	}
}
