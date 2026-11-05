package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
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
// and fails as before only when there is none, naming both reasons. An
// extraction with no master at all asks the same chain: the challenge-free
// live fallback records that chain's master when the extraction it fell back
// from had none, and every refresh of the capture reaches DownloadHls with a
// fresh extraction that has none either (refreshDownload), which used to fail
// "no HLS manifest URL available" though the setup had not. This is what
// keeps the challenge-free live fallback recording, at setup and on every
// refresh, while the player cannot be solved (audit G10).
//
// Mutants: the rescue dropped (the first row fails to set up); the rescue
// keeping the extraction's master (the first row fetches the n path); the
// rescue swapping the URL but keeping the extraction's VideoInfo (the
// token-source row mints for the watch page's master and puts the token on
// visionos's); the rescue's reason dropped from the error (the second row);
// the empty-master arm dropped, or its reason dropped from the error (the
// refresh rows).
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

	t.Run("the rescued master is recorded as its own client's: no GVS token", func(t *testing.T) {
		// A web-family master needs a GVS token (youtube.GvsTokenRequired);
		// visionos's does not. The source, and the binding, that decide the
		// mint are the rescued master's, not the extraction's.
		srv := newManifestServer(t, potTestMaster)
		fakeCookielessHls(t, srv.URL+"/api/manifest/hls_variant/id/abc/source/visionos/file/index.m3u8", "visionos", nil)
		mints := fakeVodMint(t, "minted-for-the-watch-page", nil)
		built := recordDownloaders(t)
		routed, _ := nSolver(sentinelOf, nil)
		job, _ := reextractJob(t)
		info := &youtube.VideoInfo{StreamStatus: youtube.StreamLive, PlayerURL: nPlayer,
			HlsManifestURL: srv.URL + "/api/manifest/hls_variant/id/abc/n/" + nChal + "/file/index.m3u8", HlsManifestSource: "watch_page"}
		if _, err := DownloadHls(context.Background(), job, info, routed, nil, &bgutils.PotProvider{}, nil); err != nil {
			t.Fatalf("DownloadHls = %v, want the visionos master recorded", err)
		}
		if n := mints.Load(); n != 0 {
			t.Errorf("GVS mint ran %d time(s), want none for visionos's master", n)
		}
		if path := srv.onlyPath(t); strings.Contains(path, "/pot/") {
			t.Errorf("HLS master fetched at %q, want it bare", path)
		}
		if len(*built) != 1 || (*built)[0].PoToken != "" {
			t.Errorf("downloaders %+v, want one bare HLS downloader", *built)
		}
	})

	t.Run("a refresh whose extraction has no master records the cookieless master", func(t *testing.T) {
		srv := newManifestServer(t, potTestMaster)
		calls := fakeCookielessHls(t, srv.URL+"/api/manifest/hls_variant/id/abc/source/visionos/file/index.m3u8", "visionos", nil)
		built := recordDownloaders(t)
		routed, _ := nSolver(sentinelOf, nil)
		job, _ := reextractJob(t)
		info := &youtube.VideoInfo{StreamStatus: youtube.StreamLive, PlayerURL: nPlayer}
		res, err := DownloadHls(context.Background(), job, info, routed, nil, nil, nil)
		if err != nil {
			t.Fatalf("DownloadHls = %v, want the visionos master recorded", err)
		}
		if n := calls.Load(); n != 1 {
			t.Errorf("cookieless HLS fetch ran %d times, want 1", n)
		}
		if path := srv.onlyPath(t); !strings.Contains(path, "/source/visionos/") {
			t.Errorf("HLS master fetched at %q, want the visionos master", path)
		}
		if !res.IsHls || len(*built) != 1 {
			t.Errorf("result IsHls=%v, downloaders %+v; want one HLS downloader", res.IsHls, *built)
		}
		if info.HlsManifestURL != "" {
			t.Errorf("the caller's VideoInfo was rewritten (master %q)", info.HlsManifestURL)
		}
	})

	t.Run("a refresh with no master anywhere fails naming both", func(t *testing.T) {
		calls := fakeCookielessHls(t, "", "", errors.New("visionos: no HLS manifest"))
		job, _ := reextractJob(t)
		_, err := DownloadHls(context.Background(), job, &youtube.VideoInfo{StreamStatus: youtube.StreamLive, PlayerURL: nPlayer}, nil, nil, nil, nil)
		if msg := fmt.Sprint(err); !strings.Contains(msg, "no HLS manifest URL available") || !strings.Contains(msg, "visionos: no HLS manifest") {
			t.Errorf("DownloadHls = %v, want no master and the cookieless reason", err)
		}
		if n := calls.Load(); n != 1 {
			t.Errorf("cookieless HLS fetch ran %d times, want 1", n)
		}
	})
}

// fakeStrategy is a DownloadStrategy of the given kind that answers each
// Download with the next of answers, recording the VideoInfo it was handed.
// Its result records HLS when its kind is "hls", as HlsStrategy's does, and
// it writes nothing.
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
	return &DownloadResult{HasVideo: true, IsHls: f.kind == "hls"}, nil
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
// job in Error; it now records from the start without JS when it can
// (TestLiveChallengeFailureRecordsFromStartWithoutJS), and, here, where the
// formats that need no player JS hold no split pair, the live HLS playlist
// instead, from the oldest segment its window holds, with a Warn saying why
// from-start was dropped, as yt-dlp records a live stream by default (HLS, the live adaptive
// formats skipped). With no master in the extraction the HLS setup asks the
// cookieless chain for one. A
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
// falls back); the empty-master arm dropped from DownloadHls (the no-master
// row fails to set up); the post-live re-run on the original formats (its row sees
// the tv copies); the post-live split-pair check dropped (the no-pair row
// runs the strategy again); the Warn dropped, or claiming the live edge (the
// live rows); the cancel guard confined to the live arms (the post-live
// cancelled row runs the challenge-free drop and wraps the error).
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
		{"live, the player unfetchable", "manifestless_dash", false, fmt.Errorf("resolve video URL: player JS for cafef00d: %w: status 503", cipher.ErrPlayerJSFetch), false, true},
		{"live, an unrelated failure", "manifestless_dash", false, errors.New("manifestless DASH: no suitable video adaptive format"), false, false},
		{"live, cancelled", "manifestless_dash", false, systemic, true, false},
		{"a VOD's own failure", "vod", true, systemic, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hls := &fakeStrategy{kind: "hls"}
			swapHls(t, hls)
			// The cookieless chain serves no split pair here, so the no-JS
			// from-start path finds nothing and HLS records
			// (TestLiveChallengeFailureRecordsFromStartWithoutJS covers the
			// chain that does).
			fakeCookieless(t, nil, nil)
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
			res, err := setupWithFallback(ctx, job, s, tc.isVod, info, &StrategyDeps{})
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
			line := logLine(logs, "[Cipher] live: recording from the start cannot run")
			if line == nil || !errors.Is(line["err"].(error), tc.err) {
				t.Errorf("fallback line = %v, want the setup failure as its err", line)
			}
			// It says where the capture starts: the oldest segment the
			// playlist's window holds (StartSeq -1), not the live edge.
			if msg, _ := line[""].(string); !strings.HasSuffix(msg, "from the oldest segment its window holds") {
				t.Errorf("fallback line %q does not say the capture starts at the window's oldest segment", msg)
			}
		})
	}

	t.Run("live with no master records the cookieless chain's", func(t *testing.T) {
		// The real HLS strategy: DownloadHls asks the cookieless chain for
		// the master the extraction lacks.
		srv := newManifestServer(t, potTestMaster)
		calls := fakeCookielessHls(t, srv.URL+"/api/manifest/hls_variant/id/abc/source/visionos/file/index.m3u8", "visionos", nil)
		fakeCookieless(t, nil, nil)
		recordDownloaders(t)
		job, _ := reextractJob(t)
		info := live()
		info.HlsManifestURL, info.HlsManifestSource = "", ""
		res, err := setupWithFallback(context.Background(), job, &fakeStrategy{kind: "manifestless_dash", answers: []error{systemic}}, false, info, &StrategyDeps{})
		if err != nil || res == nil || !res.IsHls {
			t.Fatalf("setup = %+v, %v; want the HLS fallback's result", res, err)
		}
		if n := calls.Load(); n != 1 {
			t.Errorf("cookieless fetch ran %d time(s), want once", n)
		}
		if path := srv.onlyPath(t); !strings.Contains(path, "/source/visionos/") {
			t.Errorf("HLS master fetched at %q, want the visionos master", path)
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
		if _, err := setupWithFallback(context.Background(), job, s, true, info, &StrategyDeps{}); err != nil {
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

	t.Run("post-live, cancelled, is returned as it came", func(t *testing.T) {
		// The cancel guard sits ahead of the post-live arm too: no
		// cookieless re-extract and no second run for a job already
		// cancelled.
		hls := &fakeStrategy{kind: "hls"}
		swapHls(t, hls)
		calls := fakeCookieless(t, nil, nil)
		job, logs := reextractJob(t)
		rec := &warnRecorder{captureLogger: logs}
		job.Logger = rec
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		info := &youtube.VideoInfo{StreamStatus: youtube.StreamPostLive, PlayerURL: nPlayer}
		s := &fakeStrategy{kind: "manifestless_dash", answers: []error{systemic, nil}}
		_, err := setupWithFallback(ctx, job, s, true, info, &StrategyDeps{})
		if err != systemic { // the very error, not one wrapping it
			t.Errorf("setup = %v, want the strategy's own error as it came", err)
		}
		if rec.warned("[Cipher] post-live manifest-free DASH: the player's JS challenges cannot be solved") {
			t.Errorf("a cancelled job ran the challenge-free drop: %v", logs.lines())
		}
		if n := calls.Load(); n != 0 || len(s.seen) != 1 || len(hls.seen) != 0 {
			t.Errorf("cookieless re-extract ran %d time(s), strategy run %d time(s), HLS %d; want 0, 1, 0", n, len(s.seen), len(hls.seen))
		}
	})

	t.Run("post-live with no challenge-free split pair left fails naming the cause", func(t *testing.T) {
		w, h, fps := 1920, 1080, 30
		tvVideo := withLevel(youtube.Format{Itag: 299, URL: "https://r1.googlevideo.com/videoplayback?itag=299&n=" + nChal, MimeType: `video/mp4; codecs="avc1.64002a"`,
			Bitrate: 4_000_000, Width: &w, Height: &h, Fps: &fps, Source: "tv_public", TargetDurationSec: 2}, youtube.AuthLevelTVPublic)
		tvAudio := withLevel(youtube.Format{Itag: 140, URL: "https://r1.googlevideo.com/videoplayback?itag=140&n=" + nChal, MimeType: `audio/mp4; codecs="mp4a.40.2"`,
			Bitrate: 128_000, Source: "tv_public", TargetDurationSec: 2}, youtube.AuthLevelTVPublic)
		// The cookieless chain has the audio alone.
		au := tvAudio
		au.Source, au.URL = "visionos", "https://r2.googlevideo.com/videoplayback?itag=140"
		calls := fakeCookieless(t, []youtube.Format{withLevel(au, youtube.AuthLevelVisionOS)}, nil)
		job, _ := reextractJob(t)
		info := &youtube.VideoInfo{StreamStatus: youtube.StreamPostLive, PlayerURL: nPlayer, Formats: []youtube.Format{tvVideo, tvAudio}}
		s := &fakeStrategy{kind: "manifestless_dash", answers: []error{systemic, nil}}
		_, err := setupWithFallback(context.Background(), job, s, true, info, &StrategyDeps{})
		if !errors.Is(err, errChallengesUnsolvable) || !strings.Contains(fmt.Sprint(err), "no split video and audio pair that needs no player JS is left") {
			t.Errorf("setup = %v, want the cause and no split pair left", err)
		}
		if n := calls.Load(); n != 1 || len(s.seen) != 1 {
			t.Errorf("cookieless re-extract ran %d time(s), strategy run %d time(s); want 1 and 1 (no re-run on a pool it cannot use)", n, len(s.seen))
		}
	})
}

// TestSetupSetsTheOtherShapeAside: a job resumed into the part it left
// unmuxed records there in whatever shape its setup now has, and the
// challenge-free fallback changes the shape inside the part — HLS into the
// part holding the earlier session's from-start DASH capture, and DASH again,
// once solving works, beside the fallback's video.ts. The finalize muxed the
// recording the setup named and the cleanup deleted staging with the other
// one unmuxed in it (decideStagingCleanup: removeStaging). The other shape is
// now set aside once the setup has built its downloaders, under one stamp (a
// DASH pair stays one recording), resume state travelling with it, so the
// finalize muxes it to its own file and the cleanup keeps staging for it. The
// shape a setup resumes is left where it is, and a setup that fails moves
// nothing.
//
// The missing_pot HLS fallback (missingPotFallback) is the same setup site,
// so it sets the DASH capture aside the same way.
//
// Mutants: the set-aside dropped (the first two rows keep both shapes, and
// the cleanup removes staging); HLS setting aside its own video.ts, or DASH
// its own pair (the resume rows); the set-aside run before the setup's own
// outcome is known (the failed rows move the pair); the missing_pot fallback
// left out of setupWithFallback (its rows fail to set up); the cancel guard
// moved below either arm (the cancelled rows run HLS and set the pair aside).
func TestSetupSetsTheOtherShapeAside(t *testing.T) {
	systemic := fmt.Errorf("manifestless DASH: resolve video URL: itag 299: %w: %w", errChallengesUnsolvable, cipher.ErrSidecarUnavailable)
	missingPot := fmt.Errorf("manifestless DASH: %w: no video adaptive format usable without a GVS PO token, and none could be minted: %w", errMissingPot, errors.New("sidecar down"))
	dash := []string{"video_stream", "audio_stream"}
	hlsName := []string{"video.ts"}
	for _, tc := range []struct {
		name      string
		staged    []string // the earlier session's capture in the part
		answers   []error  // the manifest-free setup's
		hlsErr    error
		cancel    bool
		wantAside bool
	}{
		{"the HLS fallback into a part holding a from-start DASH capture", dash, []error{systemic}, nil, false, true},
		{"DASH, solving back, into a part holding the fallback's video.ts", hlsName, []error{nil}, nil, false, true},
		{"the HLS fallback resuming its own video.ts", hlsName, []error{systemic}, nil, false, false},
		{"DASH resuming its own pair", dash, []error{nil}, nil, false, false},
		{"a setup that fails, the fallback's too", dash, []error{systemic}, errors.New("fake HLS: no variants"), false, false},
		{"the missing_pot HLS fallback into a part holding a from-start DASH capture", dash, []error{missingPot}, nil, false, true},
		{"a missing_pot fallback whose HLS setup fails too", dash, []error{missingPot}, errors.New("fake HLS: no variants"), false, false},
		{"a cancelled missing_pot setup", dash, []error{missingPot}, nil, true, false},
		{"a cancelled challenge setup", dash, []error{systemic}, nil, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hls := &fakeStrategy{kind: "hls", answers: []error{tc.hlsErr}}
			swapHls(t, hls)
			fakeCookieless(t, nil, nil) // no no-JS pair: the challenge rows fall to HLS
			job, logs := reextractJob(t)
			rec := &warnRecorder{captureLogger: logs}
			job.Logger = rec
			dir := job.StagingDir
			for _, name := range tc.staged {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("an earlier session's "+name), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			resume := filepath.Join(dir, tc.staged[0]+".resume.json")
			if err := os.WriteFile(resume, []byte(`{"lastSeq":7}`), 0o644); err != nil {
				t.Fatal(err)
			}
			info := &youtube.VideoInfo{StreamStatus: youtube.StreamLive, PlayerURL: nPlayer,
				HlsManifestURL: "https://manifest.googlevideo.com/api/manifest/hls_variant/id/abc/file/index.m3u8", HlsManifestSource: "visionos"}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			_, err := setupWithFallback(ctx, job, &fakeStrategy{kind: "manifestless_dash", answers: tc.answers}, false, info, &StrategyDeps{})
			if (err != nil) != (tc.hlsErr != nil || tc.cancel) {
				t.Fatalf("setup = %v", err)
			}
			if tc.cancel && len(hls.seen) != 0 {
				t.Errorf("a cancelled setup ran HLS %d time(s), want none", len(hls.seen))
			}
			asides := stagedRestartAsides(dir)
			warned := rec.warned("an earlier capture of this job in another shape was set aside")
			if !tc.wantAside {
				for _, name := range tc.staged {
					if !fileExists(filepath.Join(dir, name)) {
						t.Errorf("%s was moved, want it left for the setup to resume", name)
					}
				}
				if len(asides) != 0 || warned {
					t.Errorf("asides %v, warned %v; want none", asides, warned)
				}
				return
			}
			for _, name := range append(tc.staged, filepath.Base(resume)) {
				if fileExists(filepath.Join(dir, name)) {
					t.Errorf("%s is still in the part beside the new shape's capture", name)
				}
			}
			if len(asides) != len(tc.staged) {
				t.Fatalf("asides = %v, want the earlier capture's %v", asides, tc.staged)
			}
			if groups := groupStagedAsides(asides); len(groups) != 1 {
				t.Errorf("the earlier capture set aside as %d recordings, want one: %+v", len(groups), groups)
			}
			travelled := false
			for _, a := range asides {
				if strings.HasPrefix(filepath.Base(a), tc.staged[0]+engine.StagedRestartSuffix) {
					travelled = fileExists(engine.StagedRestartSidecar(a))
				}
			}
			if !travelled {
				t.Errorf("the resume state did not travel with %s's aside: %v", tc.staged[0], asides)
			}
			if !warned {
				t.Errorf("no Warn naming the set-aside: %v", logs.lines())
			}
			if v := decideStagingCleanup(job.DB, nil, job.Job.ID, dir); v.keep != keepStagingForAsides {
				t.Errorf("staging verdict %v, want it kept for the aside until the finalize muxes it", v.keep)
			}
		})
	}
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
	fakeCookieless(t, nil, nil)

	// Formats that need the player JS (an n challenge), and a cookieless
	// chain with none to offer: the no-JS from-start path has no pair, so
	// HLS records.
	v, a := adaptiveVideo(), adaptiveAudio()
	v.URL += "&n=" + nChal
	a.URL += "&n=" + nChal
	info := &youtube.VideoInfo{StreamStatus: youtube.StreamLive, PlayerURL: nPlayer,
		HlsManifestURL: "https://manifest.googlevideo.com/api/manifest/hls_variant/id/abc/file/index.m3u8", HlsManifestSource: "visionos",
		Formats: []youtube.Format{v, a}}
	err := w.orchestrator.ExecuteWithChat(context.Background(), w.buildJobContext(job), info, false, nil)
	if !errors.Is(err, errDownloadSetup) || !errors.Is(err, hlsErr) || !errors.Is(err, errChallengesUnsolvable) {
		t.Errorf("ExecuteWithChat = %v, want a setup failure naming the manifest-free and the HLS failures", err)
	}
	if len(ms.seen) != 1 || len(hls.seen) != 1 {
		t.Errorf("manifest-free tried %d time(s), HLS %d; want 1 and 1", len(ms.seen), len(hls.seen))
	}
}

// TestExecuteWithChatWarnsOfAJSLessExtraction: an extraction that ranked for
// a download without player JS (youtube.VideoInfo.JSLess — the cipher
// sidecar disabled or down) puts yt-dlp's warning in the job's log at Warn,
// once, that some formats may be missing (owner decision 4); any other
// extraction puts none there.
//
// Mutants: the warning dropped, or logged below Warn (the JS-less row);
// logged whatever the extraction (the other row).
func TestExecuteWithChatWarnsOfAJSLessExtraction(t *testing.T) {
	for _, jsless := range []bool{true, false} {
		t.Run(fmt.Sprintf("JSLess=%v", jsless), func(t *testing.T) {
			w, db := testWorkerSetup(t)
			job := &database.Job{ID: "j-warn", VideoID: "j-warn", URL: "u", Platform: "youtube", Status: database.StatusQueued}
			if _, err := db.AddJob(job); err != nil {
				t.Fatal(err)
			}
			stop := errors.New("fake VOD: stop here")
			origVod := VodStrategy
			VodStrategy = &fakeStrategy{kind: "vod", answers: []error{stop}}
			t.Cleanup(func() { VodStrategy = origVod })
			jobCtx := w.buildJobContext(job)
			rec := &warnRecorder{captureLogger: &captureLogger{}}
			jobCtx.Logger = rec
			info := &youtube.VideoInfo{StreamStatus: youtube.StreamVOD, JSLess: jsless,
				Formats: []youtube.Format{adaptiveVideo(), adaptiveAudio()}}
			if err := w.orchestrator.ExecuteWithChat(context.Background(), jobCtx, info, true, nil); !errors.Is(err, stop) {
				t.Fatalf("ExecuteWithChat = %v, want the fake's stop", err)
			}
			n := 0
			for _, m := range rec.warns {
				if strings.Contains(m, "some formats may be missing") {
					n++
				}
			}
			if want := map[bool]int{true: 1, false: 0}[jsless]; n != want {
				t.Errorf("%d Warn line(s) that formats may be missing, want %d", n, want)
			}
		})
	}
}

// TestChallengeFreeSeamsAreTheServiceMethods pins the two package seams'
// production values: the cookieless chain's formats and its HLS master, the
// youtube.Service methods themselves.
//
// Mutant: either seam pointing at another method (its check).
func TestChallengeFreeSeamsAreTheServiceMethods(t *testing.T) {
	if reflect.ValueOf(fetchCookielessHls).Pointer() != reflect.ValueOf((*youtube.Service).CookielessHlsManifest).Pointer() {
		t.Error("fetchCookielessHls is not (*youtube.Service).CookielessHlsManifest")
	}
	if reflect.ValueOf(fetchCookielessFormats).Pointer() != reflect.ValueOf((*youtube.Service).CookielessFormats).Pointer() {
		t.Error("fetchCookielessFormats is not (*youtube.Service).CookielessFormats")
	}
}

// TestLiveChallengeFailureRecordsFromStartWithoutJS is the owner's decision
// on W8's first follow-up: a live capture whose from-start setup —
// manifest-free DASH, or a DASH manifest — failed on the player's JS
// challenges re-extracts the cookieless chain and records from the start on
// visionos's split live formats, which need no signature and no n, on the
// manifest-free path, as yt-dlp's --live-from-start keeps the copy whose n
// needs no answer (_video.py:3573-3581). Its Warn says which path ran and
// why. HLS records only when that leaves no split video and audio pair, or
// when the no-JS setup fails too, and a Warn says that instead. A DASH
// capture already in the part dir is set aside before the new one records
// (one recording, its resume state with it), never written over, and an HLS
// video.ts beside it too. Before, every such capture dropped to the live
// HLS playlist, losing what the window no longer held.
//
// Mutants: go straight to HLS on a live challenge failure (the pair rows run
// HLS); run the no-JS setup on the extraction's own formats (its handed
// formats are tv's, needing JS); skip the pair check (the audio-only row runs
// the no-JS setup); return the no-JS setup's failure without falling to HLS
// (the failing row errors); drop the DASH set-aside (the staged row keeps
// video_stream in place); the Warn dropped or naming HLS (the pair rows).
func TestLiveChallengeFailureRecordsFromStartWithoutJS(t *testing.T) {
	systemic := fmt.Errorf("manifestless DASH: resolve video URL: itag 299: %w: %w", errChallengesUnsolvable, cipher.ErrNUnsolved)
	w, h, fps := 1920, 1080, 30
	tvVideo := withLevel(youtube.Format{Itag: 299, URL: "https://r1.googlevideo.com/videoplayback?itag=299&n=" + nChal, MimeType: `video/mp4; codecs="avc1.64002a"`,
		Bitrate: 4_000_000, Width: &w, Height: &h, Fps: &fps, Source: "tv_public", TargetDurationSec: 2}, youtube.AuthLevelTVPublic)
	tvAudio := withLevel(youtube.Format{Itag: 140, URL: "https://r1.googlevideo.com/videoplayback?itag=140&n=" + nChal, MimeType: `audio/mp4; codecs="mp4a.40.2"`,
		Bitrate: 128_000, Source: "tv_public", TargetDurationSec: 2}, youtube.AuthLevelTVPublic)
	visionos := func(f youtube.Format) youtube.Format {
		f.Source, f.URL = "visionos", strings.Replace(strings.Split(f.URL, "&n=")[0], "r1.", "r2.", 1)
		return withLevel(f, youtube.AuthLevelVisionOS)
	}
	pair := []youtube.Format{visionos(tvVideo), visionos(tvAudio)}
	for _, tc := range []struct {
		name      string
		kind      string
		chain     []youtube.Format
		noJSErr   error
		staged    []string
		wantNoJS  bool // the no-JS from-start capture records
		wantHls   bool
		wantAside []string
	}{
		{name: "manifest-free, visionos serves a pair", kind: "manifestless_dash", chain: pair, wantNoJS: true},
		{name: "DASH manifest, visionos serves a pair", kind: "dash", chain: pair, wantNoJS: true},
		{name: "visionos serves the audio alone", kind: "manifestless_dash", chain: pair[1:], wantHls: true},
		{name: "the no-JS setup fails too", kind: "manifestless_dash", chain: pair, noJSErr: errors.New("fake manifest-free: no fragments"), wantHls: true},
		{name: "a part holding a DASH capture and a video.ts", kind: "dash", chain: pair, staged: []string{"video_stream", "audio_stream", "video.ts"},
			wantNoJS: true, wantAside: []string{"video_stream", "audio_stream", "video.ts"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hls := &fakeStrategy{kind: "hls"}
			swapHls(t, hls)
			noJS := &fakeStrategy{kind: "manifestless_dash", answers: []error{tc.noJSErr}}
			orig := ManifestlessDashStrategy
			ManifestlessDashStrategy = noJS
			t.Cleanup(func() { ManifestlessDashStrategy = orig })
			calls := fakeCookieless(t, tc.chain, nil)
			job, logs := reextractJob(t)
			rec := &warnRecorder{captureLogger: logs}
			job.Logger = rec
			for _, name := range tc.staged {
				if err := os.WriteFile(filepath.Join(job.StagingDir, name), []byte("an earlier session's "+name), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if len(tc.staged) > 0 {
				if err := os.WriteFile(filepath.Join(job.StagingDir, "video_stream.resume.json"), []byte(`{"lastSeq":7}`), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			info := &youtube.VideoInfo{StreamStatus: youtube.StreamLive, PlayerURL: nPlayer, Formats: []youtube.Format{tvVideo, tvAudio},
				HlsManifestURL: "https://manifest.googlevideo.com/api/manifest/hls_variant/id/abc/file/index.m3u8", HlsManifestSource: "visionos"}
			first := &fakeStrategy{kind: tc.kind, answers: []error{systemic}}
			res, err := setupWithFallback(context.Background(), job, first, false, info, &StrategyDeps{})
			if err != nil || res == nil {
				t.Fatalf("setup = %v, want a capture", err)
			}
			if n := calls.Load(); n != 1 {
				t.Errorf("cookieless re-extract ran %d time(s), want once", n)
			}
			noJSWarned := rec.warned("[Cipher] live: the player's JS challenges cannot be solved — recording from the start on the split live formats that need no player JS")
			hlsWarned := rec.warned("[Cipher] live: recording from the start cannot run")
			if tc.wantNoJS {
				if len(noJS.seen) != 1 || len(hls.seen) != 0 || res.IsHls {
					t.Fatalf("no-JS setup run %d time(s), HLS %d; want the no-JS capture alone", len(noJS.seen), len(hls.seen))
				}
				handed := noJS.seen[0]
				if handed == info || len(handed.Formats) != 2 {
					t.Fatalf("no-JS setup handed %+v, want a copy holding visionos's pair", handed.Formats)
				}
				for _, f := range handed.Formats {
					if f.Source != "visionos" || f.NeedsPlayerJS() {
						t.Errorf("no-JS format %d from %s (needs JS %v), want visionos's copy", f.Itag, f.Source, f.NeedsPlayerJS())
					}
				}
				if !noJSWarned || hlsWarned {
					t.Errorf("warned no-JS %v, HLS %v; want the Warn naming the no-JS from-start path alone: %v", noJSWarned, hlsWarned, logs.lines())
				}
				if line := logLine(logs, "[Cipher] live: the player's JS challenges cannot be solved"); line == nil || !errors.Is(line["err"].(error), systemic) {
					t.Errorf("no-JS line = %v, want the setup failure as its err", line)
				}
			}
			if tc.wantHls {
				if len(hls.seen) != 1 || hls.seen[0] != info || !res.IsHls {
					t.Fatalf("HLS handed %v, want the extraction's VideoInfo once", hls.seen)
				}
				if !hlsWarned || noJSWarned {
					t.Errorf("warned HLS %v, no-JS %v; want the HLS Warn alone: %v", hlsWarned, noJSWarned, logs.lines())
				}
			}
			if info.Formats[0].Source != "tv_public" {
				t.Error("the caller's VideoInfo was rewritten")
			}
			asides := stagedRestartAsides(job.StagingDir)
			if len(asides) != len(tc.wantAside) {
				t.Fatalf("asides = %v, want the earlier %v", asides, tc.wantAside)
			}
			for _, name := range tc.wantAside {
				if fileExists(filepath.Join(job.StagingDir, name)) {
					t.Errorf("%s is still in the part beside the no-JS capture", name)
				}
			}
			if len(tc.wantAside) > 0 {
				if fileExists(filepath.Join(job.StagingDir, "video_stream.resume.json")) {
					t.Error("video_stream's resume state stayed beside the no-JS capture")
				}
				if groups := groupStagedAsides(asides); len(groups) != 2 {
					t.Errorf("the earlier captures set aside as %d recordings, want the DASH pair and the video.ts: %+v", len(groups), groups)
				}
			}
		})
	}
}
