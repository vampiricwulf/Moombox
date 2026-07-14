package monitor

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

func newTestFeedMonitor(t *testing.T) *FeedMonitor {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &FeedMonitor{
		db:              db,
		configStore:     config.NewStore(config.Defaults(), ""),
		logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		MetadataTracker: NewMetadataFailureTracker(),
		ProbeCooldown:   NewProbeCooldown(0),
	}
}

// TestMembershipActive checks the gate: needs a fetcher, and honors the enabled
// callback.
func TestMembershipActive(t *testing.T) {
	fm := newTestFeedMonitor(t)
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

// TestMembershipCandidates checks Age -> published (recency) conversion and that
// members items always carry authProbe.
func TestMembershipCandidates(t *testing.T) {
	now := time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)
	vids := []MembershipVideo{
		{VideoID: "liveVid0001", Title: "live", Age: 0},
		{VideoID: "oldVid00002", Title: "old", Age: 2 * 365 * 24 * time.Hour},
	}
	cands := membershipCandidates(vids, now, true)
	if len(cands) != 2 {
		t.Fatalf("got %d", len(cands))
	}
	if !cands[0].published.Equal(now) {
		t.Errorf("live item should be published=now, got %v", cands[0].published)
	}
	if !cands[1].published.Equal(now.Add(-2 * 365 * 24 * time.Hour)) {
		t.Errorf("old item recency wrong: %v", cands[1].published)
	}
	for _, c := range cands {
		if !c.authProbe {
			t.Errorf("membership candidate %s must use auth probe", c.videoID)
		}
		if c.source != "membership" {
			t.Errorf("source = %q", c.source)
		}
	}
}

// TestMergeCandidatesRecencyCap is the core of the user's model: RSS + membership
// merged, ranked by recency, capped — a live members item (now) always makes the
// cut; an old members VOD is crowded out by more-recent public uploads.
func TestMergeCandidatesRecencyCap(t *testing.T) {
	now := time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)
	cands := []discoveredVideo{
		{videoID: "rssRecent01", published: now.Add(-1 * time.Hour), source: "feed"},       // recent public
		{videoID: "rssOlder002", published: now.Add(-48 * time.Hour), source: "feed"},       // older public
		{videoID: "memLive0003", published: now, source: "membership", authProbe: true},      // live members (now)
		{videoID: "memOldVod04", published: now.Add(-700 * 24 * time.Hour), source: "membership", authProbe: true}, // ~2y old members VOD
	}
	got := mergeCandidates(cands, 2)
	if len(got) != 2 {
		t.Fatalf("cap=2, got %d", len(got))
	}
	// Newest two: live members (now) then recent RSS (-1h).
	if got[0].videoID != "memLive0003" {
		t.Errorf("live members item should rank first, got %s", got[0].videoID)
	}
	if got[1].videoID != "rssRecent01" {
		t.Errorf("recent RSS should rank second, got %s", got[1].videoID)
	}
	// The 2-year-old members VOD must be crowded out.
	for _, c := range got {
		if c.videoID == "memOldVod04" {
			t.Error("old members VOD should have been crowded out of the cap")
		}
	}
}

// TestProcessCandidate_AuthProbeSelection is the root-cause fix: members-only
// candidates must be probed with the AUTHENTICATED probe (else members VODs
// misclassify as upcoming).
func TestProcessCandidate_AuthProbeSelection(t *testing.T) {
	fm := newTestFeedMonitor(t)
	usedAuth := map[string]bool{}
	fm.ProbeVideo = func(ctx context.Context, id string) (*VideoProbeResult, error) {
		usedAuth[id] = false
		return &VideoProbeResult{StreamStatus: "vod", Title: "t"}, nil
	}
	fm.ProbeVideoAuth = func(ctx context.Context, id string) (*VideoProbeResult, error) {
		usedAuth[id] = true
		return &VideoProbeResult{StreamStatus: "vod", Title: "t"}, nil
	}
	fm.OnVideoFound = func(videoID, title, url string, ch *config.ChannelConfig) {}
	ch := &config.ChannelConfig{ID: "UCtest", Name: "Test", IncludeNonLiveContent: true}

	fm.processCandidate(context.Background(), ch, discoveredVideo{videoID: "memberVid01", title: "m", source: "membership", authProbe: true})
	fm.processCandidate(context.Background(), ch, discoveredVideo{videoID: "publicVid02", title: "p", source: "feed"})

	if !usedAuth["memberVid01"] {
		t.Error("members-only candidate must use the authenticated probe")
	}
	if usedAuth["publicVid02"] {
		t.Error("public/RSS candidate must use the anonymous probe")
	}
}

// TestProcessCandidate_ActiveJobDedup: a candidate with an active job is skipped
// (shared dedup, so a video seen by two sources can't create two jobs).
func TestProcessCandidate_ActiveJobDedup(t *testing.T) {
	fm := newTestFeedMonitor(t)
	probed := map[string]bool{}
	fm.ProbeVideo = func(ctx context.Context, id string) (*VideoProbeResult, error) {
		probed[id] = true
		return &VideoProbeResult{StreamStatus: "live", Title: "t"}, nil
	}
	fm.OnVideoFound = func(videoID, title, url string, ch *config.ChannelConfig) {}
	if _, err := fm.db.AddJob(&database.Job{
		ID: "activeVid01", VideoID: "activeVid01", Platform: "youtube",
		Status: database.StatusDownloading, CreatedAt: "2026-07-13T00:00:00Z", UpdatedAt: "2026-07-13T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	ch := &config.ChannelConfig{ID: "UCtest", Name: "Test"}
	fm.processCandidate(context.Background(), ch, discoveredVideo{videoID: "activeVid01", title: "x", source: "feed"})
	if probed["activeVid01"] {
		t.Error("candidate with an active job should be skipped before probing")
	}
}

// TestProcessCandidate_MembersVodHonorsIncludeNonLive locks in the exact 2.7.1
// bug: a members VOD must not flood the queue. Because it's probed with the
// AUTHENTICATED probe it classifies as "vod" (not the anonymous probe's
// "upcoming" misfire), so include_non_live_content is honored — skipped when off,
// queued once when on. If the anon probe were used, it would return "upcoming"
// and be queued even when off (the regression).
func TestProcessCandidate_MembersVodHonorsIncludeNonLive(t *testing.T) {
	run := func(includeNonLive bool) bool {
		fm := newTestFeedMonitor(t)
		fm.ProbeVideoAuth = func(ctx context.Context, id string) (*VideoProbeResult, error) {
			return &VideoProbeResult{StreamStatus: "vod", Title: "t"}, nil
		}
		fm.ProbeVideo = func(ctx context.Context, id string) (*VideoProbeResult, error) {
			return &VideoProbeResult{StreamStatus: "upcoming", Title: "t"}, nil // the bug's misclassification
		}
		found := false
		fm.OnVideoFound = func(videoID, title, url string, ch *config.ChannelConfig) { found = true }
		ch := &config.ChannelConfig{ID: "UCtest", Name: "Test", IncludeNonLiveContent: includeNonLive}
		fm.processCandidate(context.Background(), ch, discoveredVideo{videoID: "memberVod1", title: "m", source: "membership", authProbe: true})
		return found
	}
	if run(false) {
		t.Error("members VOD must NOT be queued when include_non_live_content is off")
	}
	if !run(true) {
		t.Error("members VOD SHOULD be queued once when include_non_live_content is on")
	}
}

// TestMembershipCandidates_DropsVodsWhenNonLiveOff: un-jobbable members VODs
// don't take shared cap slots from public videos when the channel doesn't
// archive uploads; live/upcoming (Age 0) always survive.
func TestMembershipCandidates_DropsVodsWhenNonLiveOff(t *testing.T) {
	now := time.Now()
	vids := []MembershipVideo{{VideoID: "liveVid0001", Age: 0}, {VideoID: "oldVod00002", Age: 100 * time.Hour}}
	off := membershipCandidates(vids, now, false)
	if len(off) != 1 || off[0].videoID != "liveVid0001" {
		t.Errorf("non-live off should drop VODs and keep live, got %+v", off)
	}
	if on := membershipCandidates(vids, now, true); len(on) != 2 {
		t.Errorf("non-live on should keep all, got %d", len(on))
	}
}

// TestParseFeedCandidates covers the RSS-only path: videoId/title/url/published
// extraction, a missing <published> → zero time, and a malformed feed → error.
func TestParseFeedCandidates(t *testing.T) {
	fm := newTestFeedMonitor(t)
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
	if cands[0].source != "feed" || cands[0].authProbe {
		t.Errorf("feed candidates must be source=feed, authProbe=false: %+v", cands[0])
	}
	if _, err := fm.parseFeedCandidates(ch, []byte("<not-xml")); err == nil {
		t.Error("a malformed feed must return an error (health signal)")
	}
}
