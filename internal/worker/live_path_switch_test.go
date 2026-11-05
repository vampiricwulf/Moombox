package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// liveSwitchFormats is a live stream's split pair from client source: tv's
// carry an n, visionos's none.
func liveSwitchFormats(source string) []youtube.Format {
	w, h, fps := 1920, 1080, 30
	host, n, level := "r2", "", youtube.AuthLevelVisionOS
	if source != "visionos" {
		host, n, level = "r1", "&n="+nChal, youtube.AuthLevelTVPublic
	}
	return []youtube.Format{
		withLevel(youtube.Format{Itag: 299, URL: "https://" + host + ".googlevideo.com/videoplayback?itag=299" + n, MimeType: `video/mp4; codecs="avc1.64002a"`,
			Bitrate: 4_000_000, Width: &w, Height: &h, Fps: &fps, Source: source, TargetDurationSec: 2}, level),
		withLevel(youtube.Format{Itag: 140, URL: "https://" + host + ".googlevideo.com/videoplayback?itag=140" + n, MimeType: `audio/mp4; codecs="mp4a.40.2"`,
			Bitrate: 128_000, Source: source, TargetDurationSec: 2}, level),
	}
}

// TestLiveRefreshFailingOnTheChallengesSwitchesPathInANewPart drives the
// live loop (runLiveStreamDownload) through a real refresh: a manifest-free
// capture of tv's formats, recording into a part that already holds its
// capture so far, stops — on a 403 the engine reads as quality lost, or a
// clean stop whose status look says still live — and its refresh
// (refreshDownload → DownloadManifestlessDash over the fresh extraction's tv
// formats, whose n the solver cannot answer) fails on the player's JS
// challenges. The owner's decision on W8's second follow-up: the capture
// switches path in a NEW part — the no-JS from-start path (visionos's split
// formats from one cookieless re-extract), else the live HLS playlist — from
// the position the stopped downloaders reached, and the current part is kept
// as it stands, its files and resume state never written over, muxed as its
// own file. A current part with no media yet is reused. Before, the refresh
// failure was waited on and retried on the same tv formats until the loop
// gave up and finalized a stream YouTube still called live.
//
// Mutants: drop switchPart from the quality-loss branch (that row's refresh
// retries on tv, no downloaders built), or from the verify branch (its row);
// build into the current part (the old files are the new downloaders'
// output); drop the forced position (StartSeq is not the stopped seq); skip
// the HLS leg in switchLivePath (the no-pair row builds nothing); reuse the
// part even when it holds media (likewise, the old files are the output).
func TestLiveRefreshFailingOnTheChallengesSwitchesPathInANewPart(t *testing.T) {
	for _, tc := range []struct {
		name     string
		branch   string // "quality-lost" or "verify"
		chain    []youtube.Format
		staged   bool // the current part holds this session's capture so far
		wantPath string
	}{
		{"quality loss, no-JS DASH", "quality-lost", liveSwitchFormats("visionos"), true, "nojs"},
		{"still-live verify, no-JS DASH", "verify", liveSwitchFormats("visionos"), true, "nojs"},
		{"no no-JS pair, HLS", "quality-lost", nil, true, "hls"},
		{"an empty part is reused", "quality-lost", liveSwitchFormats("visionos"), false, "nojs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w, db := testWorkerSetup(t)
				pin := 299
				job := &database.Job{ID: "switch1", VideoID: "switch1", URL: "u", Platform: "youtube", Status: database.StatusDownloading,
					SelectedVideoItag: &pin} // a pin: no quality monitor
				if _, err := db.AddJob(job); err != nil {
					t.Fatal(err)
				}
				o := w.orchestrator
				o.routedCipher, o.cipherSolver, o.potProvider = failingSigSolver{}, nil, &bgutils.PotProvider{}
				o.SetFfmpegPath(filepath.Join(t.TempDir(), "no-ffmpeg"))
				logs := &captureLogger{}
				root := t.TempDir()
				jobCtx := &JobContext{Job: job, DB: db, Config: &JobConfig{}, YT: youtube.NewService(nil, &discardLogger{}),
					StagingDir: root, OutputDir: t.TempDir(), Filename: "switch1", Logger: logs,
					Interruption: &interruptionSignal{}, Run: &runRecord{}}

				// The capture so far: tv's pair, stopped at segment 42.
				staged := map[string]string{}
				if tc.staged {
					for _, name := range []string{"video_stream", "audio_stream", "video_stream.resume.json", "audio_stream.resume.json"} {
						body := "this session's " + name
						if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
							t.Fatal(err)
						}
						staged[name] = body
					}
				}
				stopped := func(name string) *engine.SegmentDownloader {
					return engine.NewSegmentDownloader(engine.DownloaderOptions{OutputFile: filepath.Join(root, name),
						BaseURL: "https://r1.googlevideo.com/videoplayback?itag=299&n=" + nChal, StartSeq: 42, ForceStartSeq: true})
				}
				result := &DownloadResult{VideoDownloader: stopped("video_stream"), AudioDownloader: stopped("audio_stream"),
					VideoPath: filepath.Join(root, "video_stream"), AudioPath: filepath.Join(root, "audio_stream"),
					HasVideo: true, HasAudio: true, VideoItag: 299, AudioItag: 140}

				// The loop's YouTube: live with tv's formats until the
				// switch has run, then over.
				var mu sync.Mutex
				runs, fetches := 0, 0
				live := &youtube.VideoInfo{StreamStatus: youtube.StreamLive, PlayerURL: nPlayer, Formats: liveSwitchFormats("tv_public"),
					HlsManifestURL: "https://manifest.googlevideo.com/api/manifest/hls_variant/id/abc/file/index.m3u8", HlsManifestSource: "tv_public"}
				origInfo, origRun := liveVideoInfo, runLiveDownloaders
				t.Cleanup(func() { liveVideoInfo, runLiveDownloaders = origInfo, origRun })
				liveVideoInfo = func(*youtube.Service, context.Context, string) (*youtube.VideoInfo, error) {
					mu.Lock()
					defer mu.Unlock()
					fetches++
					if runs > 1 {
						return &youtube.VideoInfo{StreamStatus: youtube.StreamPostLive}, nil
					}
					cp := *live
					return &cp, nil
				}
				runLiveDownloaders = func(*DownloadOrchestrator, context.Context, *DownloadResult) error {
					mu.Lock()
					defer mu.Unlock()
					runs++
					if runs == 1 && tc.branch == "quality-lost" {
						return fmt.Errorf("segment 43: HTTP 403: %w", engine.ErrQualityLost)
					}
					return nil
				}
				built := recordDownloaders(t)
				calls := fakeCookieless(t, tc.chain, nil)
				hls := &fakeStrategy{kind: "hls"}
				swapHls(t, hls)

				tracker := NewProgressTracker(db, job.ID, discardLogger{}, 0)
				var lastSeg atomicTimeValue
				_, _, err := o.runLiveStreamDownload(context.Background(), jobCtx, jobCtx, 0, tc.staged, live, result, tracker,
					func() bool { return false }, &lastSeg)
				tracker.Close()
				// The tracker's refresh loop sees the close on its next tick,
				// on the bubble's clock.
				time.Sleep(2 * activityUpdateInterval)
				if err != nil {
					t.Fatalf("runLiveStreamDownload = %v", err)
				}
				if n := calls.Load(); n != 1 {
					t.Errorf("cookieless re-extracts = %d, want the switch's one", n)
				}

				newPart := filepath.Join(root, "seg_1")
				if !tc.staged {
					newPart = root
				}
				switch tc.wantPath {
				case "nojs":
					if len(*built) != 2 || len(hls.seen) != 0 {
						t.Fatalf("built %d downloaders, HLS %d; want the no-JS pair alone", len(*built), len(hls.seen))
					}
					for _, opts := range *built {
						if filepath.Dir(opts.OutputFile) != newPart {
							t.Errorf("a switched downloader writes %s, want into %s", opts.OutputFile, newPart)
						}
						if !strings.Contains(opts.BaseURL, "r2.googlevideo.com") || cipher.HasNChallenge(opts.BaseURL) {
							t.Errorf("a switched downloader fetches %s, want visionos's copy, which needs no JS", opts.BaseURL)
						}
						if opts.StartSeq != 42 || !opts.ForceStartSeq {
							t.Errorf("a switched downloader starts at %d (forced %v), want the stopped position 42", opts.StartSeq, opts.ForceStartSeq)
						}
					}
					if !jobCtx.Run.noJS() {
						t.Error("the run is not on the no-JS path after the switch")
					}
					if logLine(logs, "[Cipher] live: a refresh failed on the player's JS challenges — switching, in a new part") == nil {
						t.Errorf("no Warn naming the switch: %v", logs.lines())
					}
				case "hls":
					if len(hls.seen) != 1 || len(*built) != 0 {
						t.Fatalf("HLS handed %d time(s), %d DASH downloaders built; want HLS once", len(hls.seen), len(*built))
					}
					if logLine(logs, "[Cipher] live: a refresh failed on the player's JS challenges and no split pair") == nil {
						t.Errorf("no Warn naming the HLS switch: %v", logs.lines())
					}
				}
				for name, body := range staged {
					got, err := os.ReadFile(filepath.Join(root, name))
					if err != nil || string(got) != body {
						t.Errorf("%s = %q (err %v), want the earlier capture kept as it was", name, got, err)
					}
				}
				if asides := stagedRestartAsides(root); len(asides) != 0 {
					t.Errorf("the current part was set aside in place (%v); it is its own part", asides)
				}
			})
		})
	}
}

// TestLivePathSwitchesAreBounded: a run switches to each path at most once
// — the no-JS from-start path, then the live HLS playlist — and a third
// switch finds no path left and says so, the cause wrapped; a capture
// already on the no-JS path (the setup fallback put it there) goes to HLS.
// Never a loop: nothing gives a spent switch back.
//
// Mutants: drop the "live-switch:nojs" record — a run whose no-JS attempt
// found no pair tries it again; drop the "live-switch:hls" one — the third call switches to
// HLS again; drop the noJS check — the no-JS run switches to no-JS.
func TestLivePathSwitchesAreBounded(t *testing.T) {
	cause := fmt.Errorf("manifestless DASH: resolve video URL: %w", cipher.ErrNUnsolved)
	setup := func(t *testing.T) (*JobContext, *fakeStrategy, *fakeStrategy) {
		hls := &fakeStrategy{kind: "hls"}
		swapHls(t, hls)
		noJS := &fakeStrategy{kind: "manifestless_dash"}
		orig := ManifestlessDashStrategy
		ManifestlessDashStrategy = noJS
		t.Cleanup(func() { ManifestlessDashStrategy = orig })
		fakeCookieless(t, liveSwitchFormats("visionos"), nil)
		job, _ := reextractJob(t)
		job.Run = &runRecord{}
		return job, noJS, hls
	}
	info := &youtube.VideoInfo{StreamStatus: youtube.StreamLive, PlayerURL: nPlayer, Formats: liveSwitchFormats("tv_public")}

	job, noJS, hls := setup(t)
	var paths []string
	for range 3 {
		_, path, err := switchLivePath(context.Background(), job, info, &StrategyDeps{}, cause)
		if err != nil {
			if !errors.Is(err, errNoLivePathLeft) || !errors.Is(err, cipher.ErrNUnsolved) {
				t.Errorf("the spent switch = %v, want errNoLivePathLeft wrapping the cause", err)
			}
			path = "none"
		}
		paths = append(paths, path)
	}
	if want := []string{"no-JS DASH", "HLS", "none"}; !reflect.DeepEqual(paths, want) {
		t.Errorf("switches = %v, want %v", paths, want)
	}
	if len(noJS.seen) != 1 || len(hls.seen) != 1 {
		t.Errorf("no-JS set up %d time(s), HLS %d; want once each", len(noJS.seen), len(hls.seen))
	}

	// A no-JS attempt that found no pair is spent too: the next refresh
	// failure does not re-extract for it again, though the chain would now
	// serve one.
	job, noJS, _ = setup(t)
	fakeCookieless(t, nil, nil)
	if _, path, err := switchLivePath(context.Background(), job, info, &StrategyDeps{}, cause); err != nil || path != "HLS" {
		t.Fatalf("a switch with no no-JS pair = %q, %v; want HLS", path, err)
	}
	fakeCookieless(t, liveSwitchFormats("visionos"), nil)
	if _, path, err := switchLivePath(context.Background(), job, info, &StrategyDeps{}, cause); !errors.Is(err, errNoLivePathLeft) || len(noJS.seen) != 0 {
		t.Errorf("the next switch = %q, %v (no-JS set up %d time(s)); want no path left", path, err, len(noJS.seen))
	}

	job, noJS, hls = setup(t)
	job.Run.markNoJS()
	if _, path, err := switchLivePath(context.Background(), job, info, &StrategyDeps{}, cause); err != nil || path != "HLS" || len(noJS.seen) != 0 || len(hls.seen) != 1 {
		t.Errorf("a no-JS run's switch = %q, %v (no-JS set up %d time(s), HLS %d); want HLS", path, err, len(noJS.seen), len(hls.seen))
	}
}

// TestNoJSCaptureRefreshesOnTheNoJSFormats: once the run records on the
// formats that need no player JS, every refresh of the capture
// (refreshDownload) sets up on them — the fresh extraction's no-JS copies and
// one cookieless re-extract's — not on the whole fresh pool, whose tv copies
// need the JS and failed the refresh into a path switch it did not need. A
// run that is not on that path refreshes on the whole pool, as always.
//
// Mutants: drop the noJS branch from refreshDownload — the no-JS run's
// refresh is handed tv's formats; mark nothing in noJSFromStart — the run
// after a no-JS setup refreshes on tv's; mark nothing in the post-live arm —
// its row's refresh is handed tv's.
func TestNoJSCaptureRefreshesOnTheNoJSFormats(t *testing.T) {
	systemic := fmt.Errorf("manifestless DASH: resolve video URL: itag 299: %w: %w", errChallengesUnsolvable, cipher.ErrNUnsolved)
	for _, tc := range []struct {
		name   string
		isVod  bool
		noJS   bool // the setup fell back
		wantVO bool // the refresh is handed visionos's copies alone
	}{
		{"live, after the no-JS setup fallback", false, true, true},
		{"post-live, after its challenge-free re-run", true, true, true},
		{"a run on the JS path", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ms := &fakeStrategy{kind: "manifestless_dash"}
			orig := ManifestlessDashStrategy
			ManifestlessDashStrategy = ms
			t.Cleanup(func() { ManifestlessDashStrategy = orig })
			fakeCookieless(t, liveSwitchFormats("visionos"), nil)
			swapHls(t, &fakeStrategy{kind: "hls"})
			job, _ := reextractJob(t)
			job.Run = &runRecord{}
			status := youtube.StreamLive
			if tc.isVod {
				status = youtube.StreamPostLive
			}
			info := &youtube.VideoInfo{StreamStatus: status, PlayerURL: nPlayer, Formats: liveSwitchFormats("tv_public")}
			if tc.noJS {
				first := ms
				if tc.isVod {
					first = &fakeStrategy{kind: "manifestless_dash", answers: []error{systemic}}
					ManifestlessDashStrategy = first
				}
				s := &fakeStrategy{kind: "manifestless_dash", answers: []error{systemic}}
				if tc.isVod {
					s = first
				}
				if _, err := setupWithFallback(context.Background(), job, s, tc.isVod, info, &StrategyDeps{}); err != nil {
					t.Fatalf("setup = %v", err)
				}
				ManifestlessDashStrategy = ms
			}
			before := len(ms.seen)
			o := &DownloadOrchestrator{logger: discardLogger{}, routedCipher: failingSigSolver{}}
			if _, err := o.refreshDownload(context.Background(), job, info, false); err != nil && tc.wantVO {
				t.Fatalf("refreshDownload = %v", err)
			}
			if !tc.wantVO {
				if len(ms.seen) != before {
					t.Errorf("a JS-path run's refresh went through the no-JS setup")
				}
				return
			}
			if len(ms.seen) != before+1 {
				t.Fatalf("refresh set up the no-JS strategy %d time(s), want once", len(ms.seen)-before)
			}
			for _, f := range ms.seen[len(ms.seen)-1].Formats {
				if f.Source != "visionos" || f.NeedsPlayerJS() {
					t.Errorf("the refresh was handed %d from %s (needs JS %v), want visionos's copies alone", f.Itag, f.Source, f.NeedsPlayerJS())
				}
			}
		})
	}
}

// TestLiveLoopSeamsAreTheProductionMethods pins the live loop's two package
// seams to the methods production runs.
func TestLiveLoopSeamsAreTheProductionMethods(t *testing.T) {
	if reflect.ValueOf(liveVideoInfo).Pointer() != reflect.ValueOf((*youtube.Service).GetVideoInfo).Pointer() {
		t.Error("liveVideoInfo is not (*youtube.Service).GetVideoInfo")
	}
	if reflect.ValueOf(runLiveDownloaders).Pointer() != reflect.ValueOf((*DownloadOrchestrator).runDownloaders).Pointer() {
		t.Error("runLiveDownloaders is not (*DownloadOrchestrator).runDownloaders")
	}
}
