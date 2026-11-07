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
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/notifications"
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

// gateSolver answers every challenge while open, and fails each one once
// closed: a solver that stops working mid-stream.
type gateSolver struct{ open atomic.Bool }

func (s *gateSolver) Sig(context.Context, string, string) (string, error) {
	if s.open.Load() {
		return "SIGNED", nil
	}
	return "", errors.New("sig solve failed")
}

func (s *gateSolver) N(context.Context, string, string) (string, error) {
	if s.open.Load() {
		return nSolved, nil
	}
	return "", errors.New("n solve failed")
}

func (s *gateSolver) Batch(context.Context, string, []string, []string) (map[string]string, map[string]string, error) {
	return nil, nil, errors.New("gateSolver: Batch not used")
}

// splitSender is a notification sender that runs onSplit when a quality
// split is announced, ahead of the new part's refresh.
type splitSender struct{ onSplit func() }

func (s splitSender) Send(title, _ string, _ notifications.NotificationType, _ []notifications.Field, _ notifications.SendOptions) {
	if strings.HasSuffix(title, "Quality Split") {
		s.onSplit()
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
// own file (the background part mux runs for it: here, with no ffmpeg, its
// failure is logged once). A current part with no media yet is reused.
// Before, the refresh failure was waited on and retried on the same tv
// formats until the loop gave up and finalized a stream YouTube still called
// live.
//
// The switch answers the challenges wherever a refresh meets them: a
// still-live retry (refreshWhileLive) of a first refresh that failed for
// another reason — here an interruption's empty extraction — and a quality
// split's new-part refresh (splitPart), whose first look at the new quality
// still solved. And only them: a capture already on the no-JS path whose
// refresh found no pair because the extraction is the interruption
// signature (live, zero formats), or because the cookieless re-extract
// failed, is waited on and retried as any failed refresh, its one HLS switch
// unspent.
//
// Mutants: drop switchPart from the quality-loss branch (that row's refresh
// retries on tv, no downloaders built), or from the verify branch (its row);
// build into the current part (the old files are the new downloaders'
// output); drop the forced position (StartSeq is not the stopped seq); skip
// the HLS leg in switchLivePath (the no-pair row builds nothing); reuse the
// part even when it holds media (likewise, the old files are the output);
// drop switchPart's background mux (no part mux runs); drop the switch from
// the retry closure (the later-retry row builds nothing), or from splitPart
// (the split row's new part gets no downloaders); drop the zero-formats or
// the failed-re-extract refusal from liveSwitchWanted (the no-JS rows switch
// to HLS).
func TestLiveRefreshFailingOnTheChallengesSwitchesPathInANewPart(t *testing.T) {
	for _, tc := range []struct {
		name       string
		branch     string // "quality-lost" or "verify"
		chain      []youtube.Format
		chainErr   error
		staged     bool // the current part holds this session's capture so far
		noJS       bool // the run is on the no-JS path already
		firstEmpty bool // the first player read is the interruption signature
		allEmpty   bool // every live player read is
		split      bool // the solver answers until a quality split is announced
		wantPath   string
	}{
		{name: "quality loss, no-JS DASH", branch: "quality-lost", chain: liveSwitchFormats("visionos"), staged: true, wantPath: "nojs"},
		{name: "still-live verify, no-JS DASH", branch: "verify", chain: liveSwitchFormats("visionos"), staged: true, wantPath: "nojs"},
		{name: "no no-JS pair, HLS", branch: "quality-lost", staged: true, wantPath: "hls"},
		{name: "an empty part is reused", branch: "quality-lost", chain: liveSwitchFormats("visionos"), wantPath: "nojs"},
		{name: "a later retry meets the challenges", branch: "quality-lost", chain: liveSwitchFormats("visionos"), staged: true, firstEmpty: true, wantPath: "nojs"},
		{name: "a quality split's new-part refresh", branch: "quality-lost", chain: liveSwitchFormats("visionos"), staged: true, split: true, wantPath: "nojs"},
		{name: "a no-JS capture's interruption is no switch", branch: "quality-lost", staged: true, noJS: true, allEmpty: true, wantPath: "none"},
		{name: "a no-JS capture's failed re-extract is no switch", branch: "quality-lost", chainErr: errors.New("dial tcp: i/o timeout"), staged: true, noJS: true, wantPath: "none"},
		// A re-extract whose failure reads like the challenges' own (it
		// could not fetch the player) is the re-extract's failure still.
		{name: "a no-JS capture's re-extract failing on a player fetch is no switch", branch: "quality-lost", chainErr: fmt.Errorf("visionos: %w", cipher.ErrPlayerJSFetch), staged: true, noJS: true, wantPath: "none"},
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
				if tc.split {
					gate := &gateSolver{}
					gate.open.Store(true)
					o.routedCipher = gate
					o.notifier = splitSender{onSplit: func() { gate.open.Store(false) }}
				}
				o.SetFfmpegPath(filepath.Join(t.TempDir(), "no-ffmpeg"))
				muxLogs := &captureLogger{}
				o.logger = muxLogs
				logs := &captureLogger{}
				root := t.TempDir()
				jobCtx := &JobContext{Job: job, DB: db, Config: &JobConfig{}, YT: youtube.NewService(nil, &discardLogger{}),
					StagingDir: root, OutputDir: t.TempDir(), Filename: "switch1", Logger: logs,
					Interruption: &interruptionSignal{}, Run: &runRecord{}}
				if tc.noJS {
					jobCtx.Run.markNoJS()
				}

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
					if tc.allEmpty || (tc.firstEmpty && fetches == 1) {
						return &youtube.VideoInfo{StreamStatus: youtube.StreamLive, PlayerURL: nPlayer}, nil
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
				calls := fakeCookieless(t, tc.chain, tc.chainErr)
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
				if n := calls.Load(); tc.wantPath != "none" && n != 1 {
					t.Errorf("cookieless re-extracts = %d, want the switch's one", n)
				}

				newPart := filepath.Join(root, "seg_1")
				if !tc.staged {
					newPart = root
				}
				// A split row's first look at the new quality built tv's
				// pair in the current part, discarded unrun; the switch's
				// are the last two.
				switched := *built
				if tc.split && len(switched) >= 2 {
					switched = switched[len(switched)-2:]
				}
				switch tc.wantPath {
				case "nojs":
					if want := map[bool]int{true: 4, false: 2}[tc.split]; len(*built) != want || len(hls.seen) != 0 {
						t.Fatalf("built %d downloaders, HLS %d; want %d and no HLS", len(*built), len(hls.seen), want)
					}
					for _, opts := range switched {
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
				case "none":
					if len(hls.seen) != 0 || len(*built) != 0 {
						t.Fatalf("HLS handed %d time(s), %d DASH downloaders built; want no switch", len(hls.seen), len(*built))
					}
					if !jobCtx.Run.first("live-switch:hls") {
						t.Error("the run's one HLS switch was spent on a failure a switch does not answer")
					}
				}
				// The part the capture stopped in is muxed as its own part
				// whenever the new downloaders record into another.
				wantMux := 0
				if tc.staged && tc.wantPath != "none" {
					wantMux = 1
				}
				if n := len(logLines(muxLogs, func(m string) bool { return m == "failed to mux quality segment" })); n != wantMux {
					t.Errorf("part muxes run = %d, want %d", n, wantMux)
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
// Never a loop: only the run's one switch back gives a spent switch back
// (TestASwitchBackReArmsTheFallbackItLeaves).
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

// TestASwitchBackReArmsTheFallbackItLeaves is owner decision PE-3: the run's
// one switch back to the from-start path (runRecord.markSwitchedBack) gives
// back the mid-stream switch that put the capture on the fallback it leaves,
// so a second systemic failure later in the run falls back again, in a new
// part, where it found that switch spent and the capture stopped on the
// failure. One use, the switch being left: a capture that went no-JS, then
// HLS, gets HLS back and not the no-JS attempt; one a setup fallback put on
// a path gets nothing back, having spent nothing; and the switch back stays
// spent, so after the second fallback none is offered.
//
// Mutants (each run): markSwitchedBack not giving fallbackKey back (the
// no-JS row's second failure goes to HLS, the HLS row's finds no path
// left); switchLivePath not recording the no-JS switch (markSwitchedTo
// dropped from that arm: the no-JS row goes to HLS) or the HLS one (the HLS
// row finds no path left); markSwitchedBack giving back every spent switch
// (the HLS row's second failure goes no-JS).
func TestASwitchBackReArmsTheFallbackItLeaves(t *testing.T) {
	cause := fmt.Errorf("manifestless DASH: resolve video URL: %w", cipher.ErrNUnsolved)
	info := &youtube.VideoInfo{StreamStatus: youtube.StreamLive, PlayerURL: nPlayer, Formats: liveSwitchFormats("tv_public")}
	for _, tc := range []struct {
		name      string
		setupNoJS bool // a setup fallback put the capture on the no-JS path
		before    int  // mid-stream switches before the switch back
		want      []string
	}{
		// A mid-stream no-JS switch, a switch back, then the no-JS path
		// again, then HLS, then nothing: each still once after the re-arm.
		{"a no-JS switch is given back", false, 1, []string{"no-JS DASH", "HLS", "none"}},
		// no-JS then HLS, a switch back: HLS again, never no-JS again.
		{"the HLS switch is given back, not the no-JS one", false, 2, []string{"HLS", "none"}},
		// A setup fallback onto no-JS spent no switch: no-JS is still
		// unspent after the switch back, HLS as before.
		{"a setup fallback spent nothing to give back", true, 0, []string{"no-JS DASH", "HLS", "none"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hls := &fakeStrategy{kind: "hls"}
			swapHls(t, hls)
			noJS := &fakeStrategy{kind: "manifestless_dash"}
			orig := ManifestlessDashStrategy
			ManifestlessDashStrategy = noJS
			t.Cleanup(func() { ManifestlessDashStrategy = orig })
			fakeCookieless(t, liveSwitchFormats("visionos"), nil)
			job, _ := reextractJob(t)
			job.Run = &runRecord{}
			if tc.setupNoJS {
				job.Run.markNoJS()
			}
			for range tc.before {
				if _, _, err := switchLivePath(context.Background(), job, info, &StrategyDeps{}, cause); err != nil {
					t.Fatalf("a switch before the switch back failed: %v", err)
				}
			}
			if !job.Run.canSwitchBack() {
				t.Fatal("the capture is not offered its switch back")
			}
			job.Run.markSwitchedBack()
			var paths []string
			for range len(tc.want) {
				_, path, err := switchLivePath(context.Background(), job, info, &StrategyDeps{}, cause)
				if err != nil {
					if !errors.Is(err, errNoLivePathLeft) {
						t.Fatalf("switch = %v, want a path or errNoLivePathLeft", err)
					}
					path = "none"
				}
				paths = append(paths, path)
			}
			if !reflect.DeepEqual(paths, tc.want) {
				t.Errorf("switches after the switch back = %v, want %v", paths, tc.want)
			}
			if job.Run.canSwitchBack() {
				t.Error("a second switch back is offered")
			}
		})
	}
}
