package youtube

import (
	"context"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// TestLivePublicExtraction runs the real anonymous extraction cascade against
// live YouTube. It exists to gate client-roster changes: yt-dlp retires and
// promotes Innertube clients faster than any fixture can track, and a dead
// client fails by returning a well-formed response with no usable formats —
// which no unit test can see.
//
// Enable with MOOMBOX_LIVE_YT_TEST=1. Skipped by default (network, and the
// video IDs are external state).
//
// Not asserted here: which CLIENT supplied the winning formats. The cascade
// deliberately pools several, and pinning provenance would make the test
// fail on a healthy reordering. It asserts what actually matters — the
// anonymous path still yields playable video+audio for a VOD and a live
// stream, and live still exposes a segment-addressable manifest.
func TestLivePublicExtraction(t *testing.T) {
	if os.Getenv("MOOMBOX_LIVE_YT_TEST") != "1" {
		t.Skip("set MOOMBOX_LIVE_YT_TEST=1 to run live YouTube extraction tests")
	}

	svc := NewService(cookies.NewCookieJar(), noopLogger{})
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	t.Run("vod", func(t *testing.T) {
		// Big Buck Bunny — a stable, unrestricted, non-age-gated upload.
		info, err := svc.PlayerAPI.GetVideoInfoPublic(ctx, "aqz-KE-bpKQ")
		if err != nil {
			t.Fatalf("GetVideoInfoPublic: %v", err)
		}
		var video, audio int
		for _, f := range info.Formats {
			if f.IsVideo() {
				video++
			}
			if f.IsAudio() {
				audio++
			}
		}
		t.Logf("status=%s formats=%d (video=%d audio=%d) title=%q",
			info.StreamStatus, len(info.Formats), video, audio, info.Title)
		for _, f := range info.Formats {
			if f.Source != "" {
				t.Logf("  itag=%d source=%s", f.Itag, f.Source)
			}
		}
		if video == 0 || audio == 0 {
			t.Errorf("anonymous VOD extraction yielded video=%d audio=%d; want both non-zero", video, audio)
		}
	})

	t.Run("live", func(t *testing.T) {
		videoID := resolveLiveVideoID(ctx, t)
		info, err := svc.PlayerAPI.GetVideoInfoPublic(ctx, videoID)
		if err != nil {
			t.Fatalf("GetVideoInfoPublic(%s): %v", videoID, err)
		}
		t.Logf("status=%s formats=%d dash=%t hls=%t title=%q",
			info.StreamStatus, len(info.Formats),
			info.DashManifestURL != "", info.HlsManifestURL != "", info.Title)
		if info.StreamStatus != StreamLive {
			t.Skipf("resolved video %s is not live (status=%s); skipping", videoID, info.StreamStatus)
		}
		// A live archive needs a manifest to walk.
		if info.DashManifestURL == "" && info.HlsManifestURL == "" {
			t.Error("live extraction produced neither a DASH nor an HLS manifest URL")
		}
		// DASH specifically is what makes a live stream segment-addressable
		// (--live-from-start). A cookieless client still serves one — verified
		// 2026-08-24 — so its absence means the fallback chain stopped at an
		// HLS-only client, not that YouTube withheld it. That exact regression
		// shipped and was caught here, so this is an assertion, not a note.
		if info.DashManifestURL == "" {
			t.Error("live extraction produced no DASH manifest URL; " +
				"the cookieless DASH fallback regressed (live-from-start addressability lost)")
		}
	})
}

// resolveLiveVideoID finds a currently-live video via a channel's /live page,
// so the test does not depend on a hard-coded stream still running.
func resolveLiveVideoID(ctx context.Context, t *testing.T) string {
	t.Helper()
	// Lofi Girl — a long-running 24/7 broadcast.
	const channelLive = "https://www.youtube.com/@LofiGirl/live"

	body, err := utils.FetchBody(ctx, channelLive, 30*time.Second, map[string]string{
		"User-Agent":      constants.UserAgents.Web,
		"Accept-Language": "en-US,en;q=0.5",
	})
	if err != nil {
		t.Skipf("could not fetch %s (%v); skipping live subtest", channelLive, err)
	}
	m := liveVideoIDRe.FindSubmatch(body)
	if m == nil {
		t.Skip("no videoId on the /live page (channel may be offline); skipping live subtest")
	}
	id := string(m[1])
	t.Logf("resolved live video ID: %s", id)
	return id
}

var liveVideoIDRe = regexp.MustCompile(`"videoId":"([\w-]{11})"`)
