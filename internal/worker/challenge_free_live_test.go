package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// fakeCookielessHls swaps fetchCookielessHls for one answering manifestURL,
// source, err and counting its calls.
func fakeCookielessHls(t *testing.T, manifestURL, source string, err error) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	orig := fetchCookielessHls
	fetchCookielessHls = func(_ *youtube.Service, ctx context.Context, _ string) (string, string, error) {
		calls.Add(1)
		if _, ok := ctx.Deadline(); !ok {
			t.Error("the cookieless HLS fetch's ctx carries no deadline — it must run under credentialRefreshTimeoutFor")
		}
		return manifestURL, source, err
	}
	t.Cleanup(func() { fetchCookielessHls = orig })
	return &calls
}

// TestDownloadHlsTakesTheChallengeFreeMaster: a live HLS master whose path n
// no solver answers used to fail the setup outright (cipher.ErrNUnsolved,
// audit G9). yt-dlp passes such a master over (`if solved_n or not
// n_challenge`, _video.py:3692-3713) and records the next client's —
// visionos's, which carries no n — so DownloadHls now asks the cookieless
// chain for that master and records it, bare (visionos needs no GVS token),
// and fails as before only when there is none, naming both reasons. This is
// what keeps the challenge-free live fallback recording, at setup and on
// every refresh, while the player cannot be solved (audit G10).
//
// Mutants: the rescue dropped (the first row fails to set up); the rescue
// keeping the extraction's master (the first row fetches the n path); the
// rescue's reason dropped from the error (the second row).
func TestDownloadHlsTakesTheChallengeFreeMaster(t *testing.T) {
	t.Run("the cookieless master is recorded", func(t *testing.T) {
		srv := newManifestServer(t, potTestMaster)
		calls := fakeCookielessHls(t, srv.URL+"/api/manifest/hls_variant/id/abc/source/visionos/file/index.m3u8", "visionos", nil)
		built := recordDownloaders(t)
		routed, _ := nSolver(sentinelOf, nil)
		job, logs := reextractJob(t)
		info := &youtube.VideoInfo{StreamStatus: youtube.StreamLive, PlayerURL: nPlayer,
			HlsManifestURL: srv.URL + "/api/manifest/hls_variant/id/abc/n/" + nChal + "/file/index.m3u8", HlsManifestSource: "tv_public"}
		res, err := DownloadHls(context.Background(), job, info, routed, nil, nil, nil)
		if err != nil {
			t.Fatalf("DownloadHls = %v, want the visionos master recorded", err)
		}
		if n := calls.Load(); n != 1 {
			t.Errorf("cookieless HLS fetch ran %d times, want 1", n)
		}
		if path := srv.onlyPath(t); !strings.Contains(path, "/source/visionos/") || strings.Contains(path, nChal) {
			t.Errorf("HLS master fetched at %q, want the visionos master", path)
		}
		if !res.IsHls || len(*built) != 1 || (*built)[0].PoToken != "" {
			t.Errorf("result IsHls=%v, downloaders %+v; want one bare HLS downloader", res.IsHls, *built)
		}
		if info.HlsManifestSource != "tv_public" {
			t.Errorf("the caller's VideoInfo was rewritten (source %q)", info.HlsManifestSource)
		}
		if line := logLine(logs, "[Cipher] HLS: using the master playlist that needs no player JS"); line == nil || line["source"] != "visionos" {
			t.Errorf("rescue line = %v, want source=visionos", line)
		}
	})

	t.Run("no cookieless master: the setup fails naming both", func(t *testing.T) {
		srv := newManifestServer(t, potTestMaster)
		fakeCookielessHls(t, "", "", errors.New("visionos: no HLS manifest"))
		routed, _ := nSolver(sentinelOf, nil)
		job, _ := reextractJob(t)
		info := &youtube.VideoInfo{StreamStatus: youtube.StreamLive, PlayerURL: nPlayer,
			HlsManifestURL: srv.URL + "/api/manifest/hls_variant/id/abc/n/" + nChal + "/file/index.m3u8", HlsManifestSource: "tv_public"}
		_, err := DownloadHls(context.Background(), job, info, routed, nil, nil, nil)
		if !errors.Is(err, cipher.ErrNUnsolved) || !strings.Contains(fmt.Sprint(err), "visionos: no HLS manifest") {
			t.Errorf("DownloadHls = %v, want the unsolved n and the cookieless reason", err)
		}
	})
}

// fakeStrategy is a DownloadStrategy of the given kind that answers each
// Download with the next of answers, recording the VideoInfo it was handed.
type fakeStrategy struct {
	kind    string
	answers []error
	seen    []*youtube.VideoInfo
}

func (f *fakeStrategy) Kind() string { return f.kind }

func (f *fakeStrategy) Download(_ context.Context, _ *JobContext, info *youtube.VideoInfo, _ *StrategyDeps) (*DownloadResult, error) {
	f.seen = append(f.seen, info)
	var err error
	if i := len(f.seen) - 1; i < len(f.answers) {
		err = f.answers[i]
	}
	if err != nil {
		return nil, err
	}
	return &DownloadResult{HasVideo: true}, nil
}

// swapHls replaces HlsStrategy with hls for the test.
func swapHls(t *testing.T, hls DownloadStrategy) {
	t.Helper()
	orig := HlsStrategy
	HlsStrategy = hls
	t.Cleanup(func() { HlsStrategy = orig })
}

// TestSetupFallsBackWhenChallengesCannotBeSolved is audit G10's live half and
// owner decision 7. A live capture whose from-start setup — manifest-free
// DASH, or a DASH manifest — failed on the player's JS challenges ended the
// job in Error; it now records the live HLS playlist instead, from the live
// edge, with a Warn saying why from-start was dropped, as yt-dlp records a
// live stream by default (HLS, the live adaptive formats skipped). With no
// master in the extraction it asks the cookieless chain for one first. A
// post-live manifest-free capture, which has no live edge, runs again on
// the formats that need no player JS (the drop, one cookieless re-extract,
// the merge), from the start still. Any other failure, a VOD's own (which
// DownloadVod handles itself), and a job already cancelled are returned as
// they came, and HLS is never tried.
//
// Mutants: the fallback dropped from the orchestrator (every fallback row
// fails); the DASH-manifest kind left out (its row); HLS tried for a VOD
// (the VOD row calls it); setupFailedOnChallenges matching any error (the
// unrelated row falls back); the cancelled check dropped (the cancelled row
// falls back); the missing-master lookup dropped (the no-master row hands
// HLS no master); the post-live re-run on the original formats (its row sees
// the tv copies); the Warn dropped (the live rows).
func TestSetupFallsBackWhenChallengesCannotBeSolved(t *testing.T) {
	systemic := fmt.Errorf("manifestless DASH: resolve video URL: itag 299: %w: %w", errChallengesUnsolvable, cipher.ErrNUnsolved)
	live := func() *youtube.VideoInfo {
		return &youtube.VideoInfo{StreamStatus: youtube.StreamLive, PlayerURL: nPlayer,
			HlsManifestURL: "https://manifest.googlevideo.com/api/manifest/hls_variant/id/abc/n/" + nChal + "/file/index.m3u8", HlsManifestSource: "tv_public"}
	}
	for _, tc := range []struct {
		name    string
		kind    string
		isVod   bool
		err     error
		cancel  bool
		wantHls bool
	}{
		{"live manifest-free, systemic", "manifestless_dash", false, systemic, false, true},
		{"live DASH manifest, its n unsolved", "dash", false, fmt.Errorf("DASH: segment URL: %w", cipher.ErrNUnsolved), false, true},
		{"live, the sidecar down", "manifestless_dash", false, fmt.Errorf("x: %w", cipher.ErrSidecarUnavailable), false, true},
		{"live, an unrelated failure", "manifestless_dash", false, errors.New("manifestless DASH: no suitable video adaptive format"), false, false},
		{"live, cancelled", "manifestless_dash", false, systemic, true, false},
		{"a VOD's own failure", "vod", true, systemic, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hls := &fakeStrategy{kind: "hls"}
			swapHls(t, hls)
			job, logs := reextractJob(t)
			rec := &warnRecorder{captureLogger: logs}
			job.Logger = rec
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			s := &fakeStrategy{kind: tc.kind, answers: []error{tc.err}}
			info := live()
			res, err := setupWithChallengeFallback(ctx, job, s, tc.isVod, info, &StrategyDeps{})
			warned := rec.warned("[Cipher] live: recording from the start cannot run")
			if !tc.wantHls {
				if !errors.Is(err, tc.err) || len(hls.seen) != 0 || warned {
					t.Errorf("setup = %v, HLS tried %d time(s), warned %v; want the strategy's own error and no HLS", err, len(hls.seen), warned)
				}
				return
			}
			if err != nil || res == nil {
				t.Fatalf("setup = %v, want the HLS fallback's result", err)
			}
			if len(hls.seen) != 1 || hls.seen[0] != info {
				t.Errorf("HLS handed %v, want the extraction's VideoInfo once", hls.seen)
			}
			if !warned {
				t.Errorf("no Warn saying why from-start was dropped: %v", logs.lines())
			}
			if line := logLine(logs, "[Cipher] live: recording from the start cannot run"); line == nil || !errors.Is(line["err"].(error), tc.err) {
				t.Errorf("fallback line = %v, want the setup failure as its err", line)
			}
		})
	}

	t.Run("live with no master asks the cookieless chain for one", func(t *testing.T) {
		hls := &fakeStrategy{kind: "hls"}
		swapHls(t, hls)
		calls := fakeCookielessHls(t, "https://manifest.googlevideo.com/api/manifest/hls_variant/id/abc/file/index.m3u8", "visionos", nil)
		job, _ := reextractJob(t)
		info := live()
		info.HlsManifestURL, info.HlsManifestSource = "", ""
		if _, err := setupWithChallengeFallback(context.Background(), job, &fakeStrategy{kind: "manifestless_dash", answers: []error{systemic}}, false, info, &StrategyDeps{}); err != nil {
			t.Fatalf("setup = %v, want the HLS fallback's result", err)
		}
		if n := calls.Load(); n != 1 || len(hls.seen) != 1 || hls.seen[0].HlsManifestSource != "visionos" || hls.seen[0].HlsManifestURL == "" {
			t.Errorf("cookieless fetch ran %d time(s), HLS handed %+v; want once, and the visionos master", n, hls.seen)
		}
	})

	t.Run("post-live manifest-free runs again on the challenge-free formats", func(t *testing.T) {
		hls := &fakeStrategy{kind: "hls"}
		swapHls(t, hls)
		w, h, fps := 1920, 1080, 30
		tvVideo := youtube.Format{Itag: 299, URL: "https://r1.googlevideo.com/videoplayback?itag=299&n=" + nChal, MimeType: `video/mp4; codecs="avc1.64002a"`,
			Bitrate: 4_000_000, Width: &w, Height: &h, Fps: &fps, Source: "tv_public", TargetDurationSec: 2}
		tvAudio := youtube.Format{Itag: 140, URL: "https://r1.googlevideo.com/videoplayback?itag=140&n=" + nChal, MimeType: `audio/mp4; codecs="mp4a.40.2"`,
			Bitrate: 128_000, Source: "tv_public", TargetDurationSec: 2}
		tvVideo, tvAudio = withLevel(tvVideo, youtube.AuthLevelTVPublic), withLevel(tvAudio, youtube.AuthLevelTVPublic)
		vo, au := tvVideo, tvAudio
		vo.Source, vo.URL, au.Source, au.URL = "visionos", "https://r2.googlevideo.com/videoplayback?itag=299", "visionos", "https://r2.googlevideo.com/videoplayback?itag=140"
		vo, au = withLevel(vo, youtube.AuthLevelVisionOS), withLevel(au, youtube.AuthLevelVisionOS)
		calls := fakeCookieless(t, []youtube.Format{vo, au}, nil)
		job, _ := reextractJob(t)
		info := &youtube.VideoInfo{StreamStatus: youtube.StreamPostLive, PlayerURL: nPlayer, Formats: []youtube.Format{tvVideo, tvAudio}}
		s := &fakeStrategy{kind: "manifestless_dash", answers: []error{systemic, nil}}
		if _, err := setupWithChallengeFallback(context.Background(), job, s, true, info, &StrategyDeps{}); err != nil {
			t.Fatalf("setup = %v, want the challenge-free re-run's result", err)
		}
		if n := calls.Load(); n != 1 || len(s.seen) != 2 || len(hls.seen) != 0 {
			t.Fatalf("cookieless re-extract ran %d time(s), strategy run %d time(s), HLS %d; want 1, 2, 0", n, len(s.seen), len(hls.seen))
		}
		second := s.seen[1]
		if second == info || len(second.Formats) != 2 {
			t.Fatalf("re-run handed %+v, want a copy holding the two visionos formats", second.Formats)
		}
		for _, f := range second.Formats {
			if f.Source != "visionos" || f.NeedsPlayerJS() {
				t.Errorf("re-run format %d from %s (needs JS %v), want visionos's copy", f.Itag, f.Source, f.NeedsPlayerJS())
			}
		}
		if info.Formats[0].Source != "tv_public" {
			t.Error("the caller's VideoInfo was rewritten")
		}
	})
}

// TestExecuteWithChatRecordsLiveHlsWhenChallengesCannotBeSolved pins the
// fallback's place in the orchestrator: a live job whose strategy selection
// picked manifest-free DASH and whose setup failed on the player's JS
// challenges reaches the HLS strategy through ExecuteWithChat itself, and an
// HLS failure comes back as the setup failure (errDownloadSetup) naming both.
//
// Mutant: ExecuteWithChat calling strategy.Download directly again (HLS is
// never tried, and the error carries the manifest-free failure alone).
func TestExecuteWithChatRecordsLiveHlsWhenChallengesCannotBeSolved(t *testing.T) {
	w, db := testWorkerSetup(t)
	job := &database.Job{ID: "j-jsless", VideoID: "j-jsless", URL: "u", Platform: "youtube", Status: database.StatusLive}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	ms := &fakeStrategy{kind: "manifestless_dash", answers: []error{fmt.Errorf("manifestless DASH: %w: %w", errChallengesUnsolvable, cipher.ErrSidecarUnavailable)}}
	hlsErr := errors.New("fake HLS: stop here")
	hls := &fakeStrategy{kind: "hls", answers: []error{hlsErr}}
	origMs := ManifestlessDashStrategy
	ManifestlessDashStrategy = ms
	t.Cleanup(func() { ManifestlessDashStrategy = origMs })
	swapHls(t, hls)

	info := &youtube.VideoInfo{StreamStatus: youtube.StreamLive, PlayerURL: nPlayer,
		HlsManifestURL: "https://manifest.googlevideo.com/api/manifest/hls_variant/id/abc/file/index.m3u8", HlsManifestSource: "visionos",
		Formats: []youtube.Format{adaptiveVideo(), adaptiveAudio()}}
	err := w.orchestrator.ExecuteWithChat(context.Background(), w.buildJobContext(job), info, false, nil)
	if !errors.Is(err, errDownloadSetup) || !errors.Is(err, hlsErr) || !errors.Is(err, errChallengesUnsolvable) {
		t.Errorf("ExecuteWithChat = %v, want a setup failure naming the manifest-free and the HLS failures", err)
	}
	if len(ms.seen) != 1 || len(hls.seen) != 1 {
		t.Errorf("manifest-free tried %d time(s), HLS %d; want 1 and 1", len(ms.seen), len(hls.seen))
	}
}
