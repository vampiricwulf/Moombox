package monitor

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

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

// TestCheckMembership_GatingAndRouting exercises the discovery-source gating
// (nil fetcher, disabled flag) and the routing of discovered members videos
// through the shared processCandidate pipeline to OnVideoFound.
func TestCheckMembership_GatingAndRouting(t *testing.T) {
	fm := newTestFeedMonitor(t)

	// Probe classifies every candidate as live so ProcessYouTubeVideo signals
	// ShouldProcess=true and OnVideoFound fires.
	fm.ProbeVideo = func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		return &VideoProbeResult{StreamStatus: "live", Title: "t"}, nil
	}
	var mu sync.Mutex
	var found []string
	fm.OnVideoFound = func(videoID, title, url string, ch *config.ChannelConfig) {
		mu.Lock()
		found = append(found, videoID)
		mu.Unlock()
	}
	ch := &config.ChannelConfig{ID: "UCtest", Name: "Test"}
	ctx := context.Background()

	// 1. nil FetchMembership → safe no-op.
	fm.checkMembership(ctx, ch)
	if len(found) != 0 {
		t.Fatalf("nil fetcher should route nothing, got %v", found)
	}

	fetchCalls := 0
	fm.FetchMembership = func(ctx context.Context, channelID string) ([]MembershipVideo, error) {
		fetchCalls++
		if channelID != "UCtest" {
			t.Errorf("fetch got channelID %q, want UCtest", channelID)
		}
		return []MembershipVideo{{VideoID: "vidMember01", Title: "a"}, {VideoID: "vidMember02", Title: "b"}}, nil
	}

	// 2. MembershipEnabled=false → fetch not even called.
	fm.MembershipEnabled = func() bool { return false }
	fm.checkMembership(ctx, ch)
	if fetchCalls != 0 {
		t.Fatalf("disabled gate should skip fetch, calls=%d", fetchCalls)
	}
	if len(found) != 0 {
		t.Fatalf("disabled gate should route nothing, got %v", found)
	}

	// 3. Enabled → fetch called once, both members videos routed.
	fm.MembershipEnabled = func() bool { return true }
	fm.checkMembership(ctx, ch)
	if fetchCalls != 1 {
		t.Fatalf("expected 1 fetch call, got %d", fetchCalls)
	}
	if len(found) != 2 || found[0] != "vidMember01" || found[1] != "vidMember02" {
		t.Fatalf("expected both members videos routed, got %v", found)
	}
}

// TestCheckMembership_ActiveJobDedup verifies a members video with an active
// job in flight is skipped — the same dedup the RSS path relies on, so a video
// seen by both sources can never create two jobs.
func TestCheckMembership_ActiveJobDedup(t *testing.T) {
	fm := newTestFeedMonitor(t)
	fm.ProbeVideo = func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		return &VideoProbeResult{StreamStatus: "live", Title: "t"}, nil
	}
	var found []string
	fm.OnVideoFound = func(videoID, title, url string, ch *config.ChannelConfig) {
		found = append(found, videoID)
	}
	fm.MembershipEnabled = func() bool { return true }
	fm.FetchMembership = func(ctx context.Context, channelID string) ([]MembershipVideo, error) {
		return []MembershipVideo{{VideoID: "activeVid01", Title: "a"}, {VideoID: "freshVid002", Title: "b"}}, nil
	}

	// Seed an active (downloading) job for the first video.
	if _, err := fm.db.AddJob(&database.Job{
		ID:        "activeVid01",
		VideoID:   "activeVid01",
		Platform:  "youtube",
		Status:    database.StatusDownloading,
		CreatedAt: "2026-07-13T00:00:00Z",
		UpdatedAt: "2026-07-13T00:00:00Z",
	}); err != nil {
		t.Fatalf("seed job: %v", err)
	}

	fm.checkMembership(context.Background(), &config.ChannelConfig{ID: "UCtest", Name: "Test"})

	if len(found) != 1 || found[0] != "freshVid002" {
		t.Fatalf("expected only the non-active video routed, got %v", found)
	}
}

// TestCheckMembership_ReprobeWindow verifies the re-probe bounding: within the
// recent window everything is probed, but beyond it an already-handled (in
// history) video is skipped while a brand-new video is still processed.
func TestCheckMembership_ReprobeWindow(t *testing.T) {
	fm := newTestFeedMonitor(t)
	probed := map[string]int{}
	fm.ProbeVideo = func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		probed[videoID]++
		return &VideoProbeResult{StreamStatus: "vod", Title: "t"}, nil
	}
	fm.OnVideoFound = func(videoID, title, url string, ch *config.ChannelConfig) {}
	fm.MembershipEnabled = func() bool { return true }

	// window=1 so index 0 is "recent" and indexes 1,2 are "beyond window".
	win := 1
	ch := &config.ChannelConfig{ID: "UCtest", Name: "Test", MaxFeedItems: &win}

	// oldHandled02 (beyond window) is already in history → must be skipped.
	if err := fm.db.AddToHistory("oldHandled02"); err != nil {
		t.Fatalf("seed history: %v", err)
	}

	fm.FetchMembership = func(ctx context.Context, channelID string) ([]MembershipVideo, error) {
		return []MembershipVideo{
			{VideoID: "recentVid01", Title: "recent"},  // i=0, within window → probed
			{VideoID: "oldHandled02", Title: "old"},    // i=1, beyond window + in history → skipped
			{VideoID: "newFarDown03", Title: "newfar"}, // i=2, beyond window but new → probed
		}, nil
	}

	fm.checkMembership(context.Background(), ch)

	if probed["recentVid01"] != 1 {
		t.Errorf("within-window video should be probed once, got %d", probed["recentVid01"])
	}
	if probed["oldHandled02"] != 0 {
		t.Errorf("beyond-window already-handled video should be skipped, got %d probes", probed["oldHandled02"])
	}
	if probed["newFarDown03"] != 1 {
		t.Errorf("beyond-window NEW video should still be probed, got %d", probed["newFarDown03"])
	}
}
