package monitor

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

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

// TestFeedMonitorSmoke proves the constructor + opts scaffold works
// end-to-end, including the new FetchRSS seam: one checkChannel pass against
// a fresh, empty store, with both fetchers faked, completes without a panic
// or error and reaches OnVideoFound for the fixture's video.
func TestFeedMonitorSmoke(t *testing.T) {
	db := newTestDB(t)
	fm := newTestFeedMonitor(t, db,
		withRSS(rssWith(rssItem{ID: "vidSmoke01", Title: "hello world", Published: "2026-07-14T00:00:00Z"})),
		withMembership(membWith()),
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
