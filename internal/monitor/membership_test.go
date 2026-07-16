package monitor

import (
	"context"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// TestMembershipActive checks the gate: needs a fetcher, and honors the enabled
// callback.
func TestMembershipActive(t *testing.T) {
	fm := newTestFeedMonitor(t, newTestDB(t))
	if fm.membershipActive() {
		t.Error("no fetcher wired -> inactive")
	}
	fm.FetchMembership = func(ctx context.Context, id string) ([]MembershipVideo, error) { return nil, nil }
	if !fm.membershipActive() {
		t.Error("fetcher wired, no gate -> active")
	}
	fm.MembershipEnabled = func() bool { return false }
	if fm.membershipActive() {
		t.Error("gate false -> inactive")
	}
	fm.MembershipEnabled = func() bool { return true }
	if !fm.membershipActive() {
		t.Error("gate true -> active")
	}
}

// TestParseFeedCandidates covers the RSS-only path: videoId/title/url/published
// extraction, a missing <published> → zero time, and a malformed feed → error.
func TestParseFeedCandidates(t *testing.T) {
	fm := newTestFeedMonitor(t, newTestDB(t))
	feed := `<?xml version="1.0"?>
<feed xmlns:yt="http://www.youtube.com/xml/schemas/2015" xmlns:media="http://search.yahoo.com/mrss/" xmlns="http://www.w3.org/2005/Atom">
  <entry><yt:videoId>vidRecent01</yt:videoId><title>recent</title>
    <link rel="alternate" href="https://youtu.be/vidRecent01"/>
    <published>2026-07-13T04:00:00+00:00</published>
    <media:group><media:description>hello</media:description></media:group></entry>
  <entry><yt:videoId>vidNoDate02</yt:videoId><title>no date</title>
    <link rel="alternate" href="https://youtu.be/vidNoDate02"/>
    <media:group><media:description>world</media:description></media:group></entry>
</feed>`
	ch := &config.ChannelConfig{ID: "UCtest", Name: "Test"}
	cands, err := fm.parseFeedCandidates(ch, []byte(feed))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("got %d candidates", len(cands))
	}
	if cands[0].videoID != "vidRecent01" || cands[0].url != "https://youtu.be/vidRecent01" {
		t.Errorf("entry 0 extraction wrong: %+v", cands[0])
	}
	if cands[0].published.IsZero() {
		t.Error("entry 0 should have a parsed <published>")
	}
	if !cands[1].published.IsZero() {
		t.Error("entry 1 (no <published>) should parse to zero time")
	}
	if cands[0].source != "rss" {
		t.Errorf("RSS candidates must be source=rss: %+v", cands[0])
	}
	if _, err := fm.parseFeedCandidates(ch, []byte("<not-xml")); err == nil {
		t.Error("a malformed feed must return an error (health signal)")
	}
}
