package worker

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// TestRefreshGvsCredentialsAdoptsTheFreshPlayerURL is audit G2's worker
// half. The 403 refresh's URL half was gated on the ORIGINAL extraction's
// PlayerURL, so a job whose watch page failed at setup — no player URL —
// skipped it on every refresh, even after a later extraction named a
// player. It is gated on a solver alone now, and resolves with the fresh
// response's player; a fresh URL that needs no player resolves with none.
//
// Mutant: the original-PlayerURL gate restored (both rows return no URL and
// the solver is never asked).
func TestRefreshGvsCredentialsAdoptsTheFreshPlayerURL(t *testing.T) {
	const freshPlayer = "https://www.youtube.com/s/player/cafef00d/player_ias.vflset/en_US/base.js"
	for _, tc := range []struct {
		name       string
		player     string // the fresh response's
		freshURL   string
		wantURL    string
		wantPlayer string // the player the solver is asked under; "" — not asked
	}{
		{name: "the fresh response names a player", player: freshPlayer,
			freshURL: "https://fresh.invalid/videoplayback?id=2&n=abc123", wantURL: "https://fresh.invalid/videoplayback?id=2&n=solved-n", wantPlayer: "cafef00d"},
		{name: "a fresh URL that needs no player",
			freshURL: "https://fresh.invalid/videoplayback?id=2&c=VISIONOS", wantURL: "https://fresh.invalid/videoplayback?id=2&c=VISIONOS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orig := refreshVideoInfo
			refreshVideoInfo = func(*youtube.Service, context.Context, string) (*youtube.VideoInfo, error) {
				return &youtube.VideoInfo{
					PlayerURL: tc.player,
					Formats:   []youtube.Format{{Itag: 140, URL: tc.freshURL, Source: "web_creator"}},
				}, nil
			}
			t.Cleanup(func() { refreshVideoInfo = orig })
			var askedUnder []string
			solver := &orderedSolver{onN: func(_ context.Context, playerID, _ string) (string, error) {
				askedUnder = append(askedUnder, playerID)
				return "solved-n", nil
			}}
			job := &JobContext{
				Job:    &database.Job{ID: "test-job", VideoID: "vid1"},
				YT:     youtube.NewService(nil, &discardLogger{}),
				Logger: &discardLogger{},
			}
			// The setup extraction had no player URL: its watch page failed.
			videoInfo := &youtube.VideoInfo{
				Formats: []youtube.Format{{Itag: 140, URL: "https://setup.invalid/videoplayback?id=1&n=abc123", Source: "web_creator"}},
			}

			baseURL, _ := refreshGvsCredentials(context.Background(), job, videoInfo, 140, solver, nil, nil, "vd-123", "test")

			if baseURL != tc.wantURL {
				t.Errorf("baseURL = %q, want %q", baseURL, tc.wantURL)
			}
			switch {
			case tc.wantPlayer == "" && len(askedUnder) != 0:
				t.Errorf("solver asked under %v, want not at all — the URL needs no player", askedUnder)
			case tc.wantPlayer != "" && (len(askedUnder) != 1 || askedUnder[0] != tc.wantPlayer):
				t.Errorf("solver asked under %v, want once under the fresh player %s", askedUnder, tc.wantPlayer)
			}
		})
	}
}

// TestCipherFailureHooksNeedNoPlayerURL: the DASH and HLS strategies' 403
// hook is installed whenever a solver is, not only for an extraction that
// named a player (G2). For one that named none it does the half that needs
// no player — the POT and visitor data dropped, so the re-extraction the
// engine's refresh path leads to starts from fresh ones — asks the solver
// nothing, drops no solver for the empty player URL, and returns "".
//
// Mutants: the PlayerURL gate restored on the DASH video, DASH audio or HLS
// hook (that hook is nil); invalidate403Caches dropping a solver for "" (the
// solver's "invalidated" line). The hook's own empty-player return is belt
// and braces since audit W7: without it resolveFreshDashURL asks no solver
// either (RoutedDecryptNInURL refuses an n it has no player for), and returns
// "" for a URL it did not change.
func TestCipherFailureHooksNeedNoPlayerURL(t *testing.T) {
	// The MPD's segments carry no n. Live ones carry one in the path, and
	// without a player URL to solve it with the setup fails before any hook
	// is built (cipher.ErrNUnsolved, TestDashAndHlsSetupFailOnAnUnsolvedN).
	const mpd = `<?xml version="1.0"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011">
  <Period>
    <AdaptationSet mimeType="video/mp4">
      <Representation id="299" bandwidth="9000000" width="1920" height="1080">
        <BaseURL>https://r4---sn.googlevideo.com/videoplayback/id/abc.1/itag/299/sq/1</BaseURL>
      </Representation>
    </AdaptationSet>
    <AdaptationSet mimeType="audio/mp4">
      <Representation id="140" bandwidth="128000">
        <BaseURL>https://r4---sn.googlevideo.com/videoplayback/id/abc.1/itag/140/sq/1</BaseURL>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`
	for _, kind := range []string{"DASH", "HLS"} {
		t.Run(kind, func(t *testing.T) {
			job, _ := vodPotJob(t)
			yt := youtube.NewService(cookies.NewCookieJar(), &discardLogger{})
			yt.SetVisitorData("vd-setup")
			job.YT = yt
			var nCalls atomic.Int32
			routed := &orderedSolver{onN: func(context.Context, string, string) (string, error) {
				nCalls.Add(1)
				return "solved", nil
			}}
			gojaLogs := &captureLogger{}
			goja, err := cipher.NewGojaResolver(t.TempDir(), gojaLogs)
			if err != nil {
				t.Fatalf("NewGojaResolver: %v", err)
			}
			info := &youtube.VideoInfo{StreamStatus: youtube.StreamLive, PlaybackWaitExempt: true} // no PlayerURL
			h := &manifestHandler{}
			var res *DownloadResult
			if kind == "DASH" {
				h.body = mpd
				info.DashManifestURL, info.DashManifestSource = "http://m.invalid/api/manifest/dash/id/abc", "tv_auth"
				serveInBubble(t, h)
				res, err = DownloadDash(context.Background(), job, info, routed, goja, nil, nil)
			} else {
				h.body = potTestMaster
				info.HlsManifestURL, info.HlsManifestSource = "http://m.invalid/api/manifest/hls_variant/id/abc/file/index.m3u8", "tv_auth"
				serveInBubble(t, h)
				res, err = DownloadHls(context.Background(), job, info, routed, goja, nil, nil)
			}
			if err != nil {
				t.Fatalf("Download%s: %v", kind, err)
			}
			hooks := map[string]func() string{"video": res.VideoDownloader.OnCipherFailure}
			if kind == "DASH" {
				hooks["audio"] = res.AudioDownloader.OnCipherFailure
			}
			for stream, hook := range hooks {
				if hook == nil {
					t.Fatalf("%s %s: no OnCipherFailure hook for an extraction without a player URL", kind, stream)
				}
				yt.SetVisitorData("vd-setup")
				if got := hook(); got != "" {
					t.Errorf("%s %s hook = %q, want \"\" — there is no player to re-solve with", kind, stream, got)
				}
				if vd := yt.GetVisitorData(); vd != "" {
					t.Errorf("%s %s hook left visitor data %q, want it dropped", kind, stream, vd)
				}
			}
			if n := nCalls.Load(); n != 0 {
				t.Errorf("the solver was asked %d times; with no player there is nothing to ask it", n)
			}
			for _, line := range gojaLogs.lines() {
				if line[0] == "cipher: invalidated solver" {
					t.Errorf("a solver was invalidated for the empty player URL: %v", line)
				}
			}
		})
	}
}
