package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// nPlayer is the player every row below solves under; nChal the n challenge
// its URLs carry (path form needs at least 10 characters), nSolved the answer
// a working solver gives.
const (
	nPlayer = "https://www.youtube.com/s/player/cafef00d/player_ias.vflset/en_US/base.js"
	nChal   = "abcdefghijkl"
	nSolved = "SOLVEDnvalue"
)

// nSolver is a routed solver answering every n challenge with answer(c), or
// failing with err, and counting what it is asked.
func nSolver(answer func(challenge string) string, err error) (*orderedSolver, *atomic.Int32) {
	var asked atomic.Int32
	return &orderedSolver{onN: func(_ context.Context, _, c string) (string, error) {
		asked.Add(1)
		if err != nil {
			return "", err
		}
		return answer(c), nil
	}}, &asked
}

// sentinelOf is what a player's n function returns from its catch block: a
// string ending with its own input (cipher.checkNResult).
func sentinelOf(c string) string { return "enhanced_except_AAAAAAAA_" + c }

func solvedAs(string) string { return nSolved }

// recordDownloaders swaps the engine constructor for one that records the
// options each downloader was built with.
func recordDownloaders(t *testing.T) *[]engine.DownloaderOptions {
	t.Helper()
	var built []engine.DownloaderOptions
	prev := newSegmentDownloader
	newSegmentDownloader = func(opts engine.DownloaderOptions) *engine.SegmentDownloader {
		built = append(built, opts)
		return engine.NewSegmentDownloader(opts)
	}
	t.Cleanup(func() { newSegmentDownloader = prev })
	return &built
}

// nMPD is a live DASH manifest whose segment URLs carry the n the manifest
// was fetched with, in the path, as live ones do.
const nMPD = `<?xml version="1.0"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011">
  <Period>
    <AdaptationSet mimeType="video/mp4">
      <Representation id="299" bandwidth="9000000" width="1920" height="1080">
        <BaseURL>https://r4---sn.googlevideo.com/videoplayback/id/abc.1/itag/299/n/` + nChal + `/sq/1</BaseURL>
      </Representation>
    </AdaptationSet>
    <AdaptationSet mimeType="audio/mp4">
      <Representation id="140" bandwidth="128000">
        <BaseURL>https://r4---sn.googlevideo.com/videoplayback/id/abc.1/itag/140/n/` + nChal + `/sq/1</BaseURL>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`

// TestDashAndHlsSetupFailOnAnUnsolvedN is audit G9's manifest half. A DASH
// manifest whose segment URLs, or an HLS master URL, carry an n no solver
// answered used to be downloaded anyway — a Warn said "downloads may be
// throttled", and every segment was refused — and with no player URL or no
// solver the n was not even asked about. yt-dlp uses a manifest only `if
// solved_n or not n_challenge` (_video.py:3692-3713, 3730-3751), so the
// setup now fails, naming the n, before a downloader is built (and, for HLS,
// before the master is fetched, and for a DASH manifest URL's own query n,
// before the manifest is). The solved row pins the other side: each
// path-form n is replaced by its answer. A DASH manifest URL's path n is
// left for the segments' echo of it (DownloadDash's Step 1 says why): the
// manifest is fetched with it as sent, and an unsolved one fails the setup
// at the segments.
//
// Mutants: DownloadDash's segment loop keeping a URL it could not solve (the
// DASH failure rows set up), its manifest-URL step back to a Warn (the
// manifest-query rows fetch the manifest, the one with a player asks twice);
// DownloadHls keeping an unsolved master URL (the HLS failure rows set up);
// either strategy's old PlayerURL gate restored, in DownloadDash at either
// step (the no-player rows set up, the n sent as it came, or fetch the
// manifest); the path form left unreplaced (the solved rows carry the
// challenge); the manifest URL's path n solved at Step 1 (the solved
// path-n row fetches the answer).
func TestDashAndHlsSetupFailOnAnUnsolvedN(t *testing.T) {
	type row struct {
		name     string
		player   string
		answer   func(string) string
		err      error
		manifest string // DASH only: a manifest URL suffix carrying its own n
		wantErr  bool
		noFetch  bool  // DASH only: the setup fails before the manifest is fetched
		maxAsked int32 // the solver is asked at most this often
	}
	rows := []row{
		{name: "the solver fails", player: nPlayer, err: errors.New("sidecar down"), wantErr: true, maxAsked: 1},
		{name: "the solver answers with the exception sentinel", player: nPlayer, answer: sentinelOf, wantErr: true, maxAsked: 1},
		{name: "no player URL to solve with", answer: solvedAs, wantErr: true},
		{name: "solved", player: nPlayer, answer: solvedAs, maxAsked: 2},
	}
	for _, kind := range []string{"DASH", "HLS"} {
		cases := rows
		if kind == "DASH" {
			const pathN = "/n/" + nChal + "/source/yt_live_broadcast"
			cases = append(cases,
				row{name: "the manifest URL's own query n is unsolved", player: nPlayer, answer: sentinelOf,
					manifest: "?n=" + nChal, wantErr: true, noFetch: true, maxAsked: 1},
				row{name: "the manifest URL's own query n, no player URL", answer: solvedAs,
					manifest: "?n=" + nChal, wantErr: true, noFetch: true},
				row{name: "the manifest URL's path n, unsolved", player: nPlayer, answer: sentinelOf,
					manifest: pathN, wantErr: true, maxAsked: 1},
				row{name: "the manifest URL's path n, solved", player: nPlayer, answer: solvedAs,
					manifest: pathN, maxAsked: 2},
			)
		}
		for _, tc := range cases {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				built := recordDownloaders(t)
				routed, asked := nSolver(tc.answer, tc.err)
				job, _ := vodPotJob(t)
				info := &youtube.VideoInfo{StreamStatus: youtube.StreamLive, PlayerURL: tc.player}
				var srv *manifestServer
				var err error
				if kind == "DASH" {
					srv = newManifestServer(t, nMPD)
					info.DashManifestURL, info.DashManifestSource = srv.URL+"/api/manifest/dash/id/abc"+tc.manifest, "tv_auth"
					_, err = DownloadDash(context.Background(), job, info, routed, nil, nil, nil)
				} else {
					srv = newManifestServer(t, potTestMaster)
					info.HlsManifestURL, info.HlsManifestSource = srv.URL+"/api/manifest/hls_variant/id/abc/n/"+nChal+"/file/index.m3u8", "tv_auth"
					_, err = DownloadHls(context.Background(), job, info, routed, nil, nil, nil)
				}
				if n := asked.Load(); n > tc.maxAsked {
					t.Errorf("the solver was asked %d times, want at most %d", n, tc.maxAsked)
				}
				if tc.wantErr {
					if !errors.Is(err, cipher.ErrNUnsolved) || !strings.Contains(err.Error(), "n challenge unsolved") {
						t.Fatalf("Download%s = %v, want a setup error naming the unsolved n (cipher.ErrNUnsolved)", kind, err)
					}
					if len(*built) != 0 {
						t.Errorf("%d downloaders built for a manifest whose n is unsolved; want none", len(*built))
					}
					srv.mu.Lock()
					fetched := len(srv.paths)
					srv.mu.Unlock()
					if (kind == "HLS" || tc.noFetch) && fetched != 0 {
						t.Errorf("the %s manifest was fetched %d times with its n unsolved; want never", kind, fetched)
					}
					return
				}
				if err != nil {
					t.Fatalf("Download%s: %v", kind, err)
				}
				if kind == "HLS" {
					if path := srv.onlyPath(t); !strings.Contains(path, "/n/"+nSolved+"/") || strings.Contains(path, nChal) {
						t.Errorf("HLS master fetched at %q, want its n replaced by the answer", path)
					}
					return
				}
				if tc.manifest != "" {
					if path := srv.onlyPath(t); !strings.Contains(path, "/n/"+nChal+"/") {
						t.Errorf("DASH manifest fetched at %q, want its path n as sent, for the segments to echo", path)
					}
				}
				if len(*built) != 2 {
					t.Fatalf("built %d downloaders, want video and audio", len(*built))
				}
				for _, opts := range *built {
					if !strings.Contains(opts.BaseURL, "/n/"+nSolved+"/") || strings.Contains(opts.BaseURL, nChal) {
						t.Errorf("segment base URL %q, want its n replaced by the answer", opts.BaseURL)
					}
				}
			})
		}
	}
}

// TestResolveFreshDashURLHandsBackOnlyASolvedURL: the DASH 403 hook's
// re-solve returns a URL only when it solved the n in it. It used to log
// "fresh n-param decryption succeeded" and hand the engine whatever came
// back — the URL unchanged when the n could not be solved or there was none,
// or the player's exception sentinel installed as if it were the answer —
// after a legacy goja pass that re-ran the solve RoutedDecryptNInURL's own
// fallback had just run.
//
// Mutants: the unchanged-URL case returning the URL (the no-n row hands it
// back); cipher's sentinel check dropped (the sentinel row hands back the
// sentinel URL); the old function restored (the sentinel row again).
func TestResolveFreshDashURLHandsBackOnlyASolvedURL(t *testing.T) {
	// A goja fallback that cannot help: its player fetch is refused at once
	// (loopback, never proxied), so every row is decided by the routed answer.
	const unfetchable = "http://127.0.0.1:1/s/player/cafef00d/player_ias.vflset/en_US/base.js"
	goja, err := cipher.NewGojaResolver(t.TempDir(), &discardLogger{})
	if err != nil {
		t.Fatalf("NewGojaResolver: %v", err)
	}
	const withN = "https://r4---sn.googlevideo.com/videoplayback/id/abc.1/itag/299/n/" + nChal + "/sq/1"
	for _, tc := range []struct {
		name   string
		orig   string
		answer func(string) string
		err    error
		want   string
	}{
		{"solved", withN, solvedAs, nil, strings.Replace(withN, nChal, nSolved, 1)},
		{"the solver answers with the exception sentinel", withN, sentinelOf, nil, ""},
		{"the solver fails", withN, nil, errors.New("sidecar down"), ""},
		{"the URL carries no n", "https://r4---sn.googlevideo.com/videoplayback/id/abc.1/itag/299/sq/1", solvedAs, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			routed, _ := nSolver(tc.answer, tc.err)
			logs := &captureLogger{}
			got := resolveFreshDashURL(context.Background(), "job1", routed, goja, unfetchable, tc.orig, logs)
			if got != tc.want {
				t.Errorf("resolveFreshDashURL = %q, want %q", got, tc.want)
			}
			_, said := logs.field("[Cipher] retry: fresh n-param decryption succeeded; engine will swap BaseURL", "jobID")
			if said != (tc.want != "") {
				t.Errorf("the success line logged = %v, want %v (URL %q)", said, tc.want != "", got)
			}
		})
	}
}

// TestVodAndManifestFreeTreatAnUnsolvedNAsAResolveFailure: the strategies
// that resolve a chosen format's URL treat an n no solver answered as that
// format's resolve failure, cipher.ErrNUnsolved kept in the error (the
// systemic-failure fallback, audit W8, classifies on it). They used to be
// handed the URL with its n as YouTube sent it, and no error.
//
// resolveManifestlessStream gives up two ways once the broken itag is
// excluded — no candidate left at all, or candidates left of which the
// re-selection picks none (here an audio rendition, for a video stream) — and
// each keeps the resolve error.
//
// Mutants: cipher's routed n failure back to a Warn and the URL as it came
// (every row resolves); either of resolveManifestlessStream's give-ups
// dropping the resolve error (that give-up's row's errors.Is fails).
func TestVodAndManifestFreeTreatAnUnsolvedNAsAResolveFailure(t *testing.T) {
	routed, _ := nSolver(sentinelOf, nil)
	w, h, fps := 1280, 720, 30
	video := youtube.Format{Itag: 302, URL: "https://r1.googlevideo.com/videoplayback?itag=302&n=" + nChal,
		MimeType: `video/webm; codecs="vp9"`, Bitrate: 2_000_000, Width: &w, Height: &h, Fps: &fps, Source: "tv_auth"}

	job, _ := vodPotJob(t)
	result := &DownloadResult{HasVideo: true, VideoFormat: &video}
	if v, a, err := resolveVodURLs(context.Background(), job, result, []youtube.Format{video}, routed, nil, nPlayer); !errors.Is(err, cipher.ErrNUnsolved) {
		t.Errorf("resolveVodURLs = %q, %q, %v; want a resolve failure carrying cipher.ErrNUnsolved", v, a, err)
	}

	cand := DashStreamInfo{Itag: 302, MimeType: video.MimeType, Width: w, Height: h, FPS: fps, Bandwidth: video.Bitrate}
	audio := DashStreamInfo{Itag: 140, MimeType: `audio/mp4; codecs="mp4a.40.2"`, Bandwidth: 128_000}
	for _, tc := range []struct {
		name       string
		candidates []DashStreamInfo
		giveUp     string // the give-up the row must reach
	}{
		{"no candidate left", []DashStreamInfo{cand}, "no remaining candidates"},
		{"none the re-selection takes", []DashStreamInfo{cand, audio}, "re-selection produced no candidate"},
	} {
		u, _, err := resolveManifestlessStream(context.Background(), []youtube.Format{video}, tc.candidates, &cand,
			0, 0, true, "", false, routed, nil, nPlayer, map[int]bool{}, &discardLogger{})
		if !errors.Is(err, cipher.ErrNUnsolved) || !strings.Contains(fmt.Sprint(err), tc.giveUp) {
			t.Errorf("resolveManifestlessStream, %s = %q, %v; want the %q give-up carrying cipher.ErrNUnsolved", tc.name, u, err, tc.giveUp)
		}
	}
}

// TestRefreshResolvesTheFreshNEachTime is audit G59. A credential refresh
// re-resolves its URL from a fresh player response, and an n no solver
// answered used to come back as the URL with its n as sent, err nil — which
// the refresh installed, swapping a working URL for one that is refused. Now
// such a refresh keeps the current URL, and the next one solves its own fresh
// n again rather than reusing anything: a solver that recovered (the sidecar
// back, its exception-sentinel answer no longer cached) is installed then.
// Both refresh paths: the segmented one (refreshGvsCredentials) and the
// whole-file one (refreshVodURL).
//
// Mutants: cipher's sentinel check dropped (the first refresh installs the
// sentinel URL); RoutedResolveURL's routed failure back to a Warn (the first
// refresh installs the unsolved URL — the error row).
func TestRefreshResolvesTheFreshNEachTime(t *testing.T) {
	for _, first := range []struct {
		name   string
		answer func(string) string
		err    error
	}{
		{"the exception sentinel", sentinelOf, nil},
		{"a solver error", nil, errors.New("sidecar restarting")},
	} {
		for _, path := range []string{"segmented", "whole-file"} {
			t.Run(path+"/"+first.name, func(t *testing.T) {
				// Each fresh response carries its own n; a segmented stream's
				// format has no byte length (formatBecameWholeFile), a
				// whole-file one has the served file's.
				clen := "1000"
				if path == "segmented" {
					clen = ""
				}
				var calls atomic.Int32
				orig := refreshVideoInfo
				refreshVideoInfo = func(*youtube.Service, context.Context, string) (*youtube.VideoInfo, error) {
					challenge := []string{"firstchallenge", "secondchallenge"}[min(calls.Add(1), 2)-1]
					return &youtube.VideoInfo{PlayerURL: nPlayer, Formats: []youtube.Format{{Itag: 137, ContentLength: clen, Source: "tv_auth",
						URL: "https://r1.googlevideo.com/videoplayback?expire=1&itag=137&clen=1000&n=" + challenge}}}, nil
				}
				t.Cleanup(func() { refreshVideoInfo = orig })
				var solves atomic.Int32
				routed := &orderedSolver{onN: func(_ context.Context, _, c string) (string, error) {
					if solves.Add(1) == 1 {
						if first.err != nil {
							return "", first.err
						}
						return first.answer(c), nil
					}
					return "solved-" + c[:6], nil
				}}

				const current = "https://r1.googlevideo.com/videoplayback?expire=1&itag=137&clen=1000&n=SETUPsolved"
				job, _ := vodPotJob(t)
				job.YT = youtube.NewService(nil, &discardLogger{})
				var refresh func() string
				if path == "segmented" {
					info := &youtube.VideoInfo{PlayerURL: nPlayer, Formats: []youtube.Format{{Itag: 137, URL: current, Source: "tv_auth"}}}
					refresh = func() string {
						u, _ := refreshGvsCredentials(context.Background(), job, info, 137, routed, nil, nil, "vd", "test")
						return u
					}
				} else {
					fakeCookielessCtx(t, nil, nil)
					st := newVodRefreshState(nil, youtube.Format{Itag: 137, URL: current, ContentLength: "1000", Source: "tv_auth"}, current)
					refresh = func() string {
						u, _ := refreshVodURL(context.Background(), job, nPlayer, st, routed, nil, nil, "vd", "VOD video")
						return u
					}
				}

				if got := refresh(); got != "" {
					t.Errorf("first refresh = %q, want \"\" — its fresh n has no answer, and the current URL stays", got)
				}
				want := "https://r1.googlevideo.com/videoplayback?expire=1&itag=137&clen=1000&n=solved-second"
				if got := refresh(); got != want {
					t.Errorf("second refresh = %q, want its own fresh n solved: %q", got, want)
				}
				if n := solves.Load(); n != 2 {
					t.Errorf("the solver was asked %d times, want once per refresh", n)
				}
			})
		}
	}
}
