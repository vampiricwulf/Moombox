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
// monitor.MembershipVideo).
func membWith(videos ...youtube.MembershipVideo) MembershipFetchFunc {
	return func(ctx context.Context, channelID string) ([]MembershipVideo, error) {
		out := make([]MembershipVideo, len(videos))
		for i, v := range videos {
			out[i] = MembershipVideo{VideoID: v.VideoID, Title: v.Title, Age: v.Age}
		}
		return out, nil
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
	fm.OnVideoFound = func(videoID, title, url string, ch *config.ChannelConfig) {
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
