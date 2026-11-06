package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// TestAFallbackCaptureSwitchesBackOnce drives the live loop
// (runLiveStreamDownload) on a capture a fallback put on the formats that
// need no player JS — visionos's pair, recording into a part that already
// holds its capture so far — through a refresh at which the challenges can
// be solved again: the capture switches back to the from-start path that
// needs the JS (tv's pair), in a NEW part from the position the stopped
// downloaders reached, the no-JS part kept as it stands and muxed as its own
// file, and the run is off the no-JS path (owner decision PR-R3). Before, a
// switch was one-way for the run: a sidecar restart or a player CDN blip
// cost the rest of the broadcast its from-start path. While the challenges
// still fail, nothing switches: the capture stays on the no-JS path, its own
// refresh runs, and the run's one switch back is not spent.
//
// Mutants: switchBack not called at the quality-loss refresh, or at the
// still-live verify's (that row's run stays no-JS); building into the current part (the old files
// are the new downloaders' output); markSwitchedBack not clearing the no-JS
// mark (the run still reads no-JS); switchBack spending the switch on a
// failed setup (the failing row's canSwitchBack is false).
//
// The refused row stops the no-JS pair at a segment YouTube kept refusing
// (engine.SegmentRefusedError at 42): the switch back is also the split at
// it (owner decision D8), so the new part starts both streams past the hole,
// at 43, and each stopped stream records its gap row. The HLS row switches
// back from the live HLS fallback, whose result has a video downloader only:
// the new pair's audio starts, forced, where the playlist stopped, as its
// video does. Mutants: switchBack handed the stopped positions unmoved (the
// refused row's pair starts at 42, no gap row); the audio position not
// seeded from the HLS video's (the HLS row's audio starts at 0, unforced).
func TestAFallbackCaptureSwitchesBackOnce(t *testing.T) {
	for _, tc := range []struct {
		name     string
		branch   string // "quality-lost" or "verify"
		solvable bool
		refused  bool // the first run stops at a refused segment, 42
		hls      bool // the capture is on the live HLS fallback
	}{
		{"quality loss, the challenges solvable again", "quality-lost", true, false, false},
		{"still-live verify, the challenges solvable again", "verify", true, false, false},
		{"quality loss, the challenges still fail", "quality-lost", false, false, false},
		{"refused segment, the challenges solvable again", "quality-lost", true, true, false},
		{"HLS fallback, quality loss", "quality-lost", true, false, true},
		{"HLS fallback, still-live verify", "verify", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w, db := testWorkerSetup(t)
				pin := 299
				job := &database.Job{ID: "back1", VideoID: "back1", URL: "u", Platform: "youtube", Status: database.StatusDownloading,
					SelectedVideoItag: &pin}
				if _, err := db.AddJob(job); err != nil {
					t.Fatal(err)
				}
				o := w.orchestrator
				gate := &gateSolver{}
				gate.open.Store(tc.solvable)
				o.routedCipher, o.cipherSolver, o.potProvider = gate, nil, &bgutils.PotProvider{}
				o.SetFfmpegPath(filepath.Join(t.TempDir(), "no-ffmpeg"))
				muxLogs := &captureLogger{}
				o.logger = muxLogs
				logs := &captureLogger{}
				root := t.TempDir()
				jobCtx := &JobContext{Job: job, DB: db, Config: &JobConfig{}, YT: youtube.NewService(nil, &discardLogger{}),
					StagingDir: root, OutputDir: t.TempDir(), Filename: "back1", Logger: logs,
					Interruption: &interruptionSignal{}, Run: &runRecord{}}
				names := []string{"video_stream", "audio_stream", "video_stream.resume.json", "audio_stream.resume.json"}
				if tc.hls {
					jobCtx.Run.markHlsFallback()
					names = []string{"video.ts"}
				} else {
					jobCtx.Run.markNoJS()
				}

				staged := map[string]string{}
				for _, name := range names {
					body := "the no-JS capture's " + name
					if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
						t.Fatal(err)
					}
					staged[name] = body
				}
				stopped := func(name string) *engine.SegmentDownloader {
					return engine.NewSegmentDownloader(engine.DownloaderOptions{OutputFile: filepath.Join(root, name),
						BaseURL: "https://r2.googlevideo.com/videoplayback?itag=299", StartSeq: 42, ForceStartSeq: true})
				}
				result := &DownloadResult{VideoDownloader: stopped("video_stream"), AudioDownloader: stopped("audio_stream"),
					VideoPath: filepath.Join(root, "video_stream"), AudioPath: filepath.Join(root, "audio_stream"),
					HasVideo: true, HasAudio: true, VideoItag: 299, AudioItag: 140}
				if tc.hls {
					result = &DownloadResult{VideoDownloader: stopped("video.ts"), VideoPath: filepath.Join(root, "video.ts"),
						HasVideo: true, HasAudio: true, IsHls: true}
				}
				var mu sync.Mutex
				runs := 0
				live := &youtube.VideoInfo{StreamStatus: youtube.StreamLive, PlayerURL: nPlayer, Formats: liveSwitchFormats("tv_public")}
				origInfo, origRun := liveVideoInfo, runLiveDownloaders
				t.Cleanup(func() { liveVideoInfo, runLiveDownloaders = origInfo, origRun })
				liveVideoInfo = func(*youtube.Service, context.Context, string) (*youtube.VideoInfo, error) {
					mu.Lock()
					defer mu.Unlock()
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
					if runs == 1 && tc.refused {
						return &engine.SegmentRefusedError{Seq: 42}
					}
					if runs == 1 && tc.branch == "quality-lost" {
						return fmt.Errorf("segment 43: HTTP 403: %w", engine.ErrQualityLost)
					}
					return nil
				}
				built := recordDownloaders(t)
				fakeCookieless(t, liveSwitchFormats("visionos"), nil)

				tracker := NewProgressTracker(db, job.ID, discardLogger{}, 0)
				var lastSeg atomicTimeValue
				_, _, err := o.runLiveStreamDownload(context.Background(), jobCtx, jobCtx, 0, true, live, result, tracker,
					func() bool { return false }, &lastSeg)
				tracker.Close()
				time.Sleep(2 * activityUpdateInterval)
				if err != nil {
					t.Fatalf("runLiveStreamDownload = %v", err)
				}

				back := logLine(logs, "[Cipher] live: the player's JS challenges can be solved again")
				if back == nil {
					back = logLine(muxLogs, "[Cipher] live: the player's JS challenges can be solved again")
				}
				if !tc.solvable {
					if back != nil {
						t.Error("switched back while the challenges still fail")
					}
					if !jobCtx.Run.noJS() || !jobCtx.Run.canSwitchBack() {
						t.Errorf("noJS = %v, canSwitchBack = %v; want the capture still on the no-JS path, its switch back unspent",
							jobCtx.Run.noJS(), jobCtx.Run.canSwitchBack())
					}
					return
				}
				if back == nil {
					t.Fatalf("no Warn naming the switch back: %v %v", logs.lines(), muxLogs.lines())
				}
				if jobCtx.Run.noJS() || jobCtx.Run.canSwitchBack() {
					t.Errorf("noJS = %v, canSwitchBack = %v; want the run back on the JS path, its one switch back spent",
						jobCtx.Run.noJS(), jobCtx.Run.canSwitchBack())
				}
				newPart := filepath.Join(root, "seg_1")
				if len(*built) != 2 {
					t.Fatalf("built %d downloaders, want the switch back's pair", len(*built))
				}
				for _, opts := range *built {
					if filepath.Dir(opts.OutputFile) != newPart {
						t.Errorf("a switched-back downloader writes %s, want into %s", opts.OutputFile, newPart)
					}
					if !strings.Contains(opts.BaseURL, "r1.googlevideo.com") || !strings.Contains(opts.BaseURL, "n="+nSolved) {
						t.Errorf("a switched-back downloader fetches %s, want tv's copy, its n solved", opts.BaseURL)
					}
					want := 42
					if tc.refused {
						want = 43 // past the refused segment
					}
					if opts.StartSeq != want || !opts.ForceStartSeq {
						t.Errorf("a switched-back downloader %s starts at %d (forced %v), want %d, forced", opts.OutputFile, opts.StartSeq, opts.ForceStartSeq, want)
					}
				}
				// The gap rows the stopped streams' tracker callbacks
				// recorded (noteRefusedHole), as the job's gap list shows them.
				tracker.mu.Lock()
				gaps := slices.Clone(tracker.gaps)
				tracker.mu.Unlock()
				var wantGaps []database.Gap
				if tc.refused {
					wantGaps = []database.Gap{{JobID: job.ID, From: 42, To: 42, Stream: "video"}, {JobID: job.ID, From: 42, To: 42, Stream: "audio"}}
				}
				if !slices.Equal(gaps, wantGaps) {
					t.Errorf("gap rows = %+v, want %+v", gaps, wantGaps)
				}
				if n := len(logLines(muxLogs, func(m string) bool { return m == "failed to mux quality segment" })); n != 1 {
					t.Errorf("part muxes run = %d, want the no-JS part's one", n)
				}
				for name, body := range staged {
					got, err := os.ReadFile(filepath.Join(root, name))
					if err != nil || string(got) != body {
						t.Errorf("%s = %q (err %v), want the no-JS capture kept as it was", name, got, err)
					}
				}
			})
		})
	}
}

// TestRunRecordSwitchBack pins the run's one switch back: offered on either
// fallback, spent once, and spent for good — a capture that falls back again
// after it is not offered another.
func TestRunRecordSwitchBack(t *testing.T) {
	var none *runRecord
	if none.canSwitchBack() {
		t.Error("a nil record offers a switch back")
	}
	r := &runRecord{}
	if r.canSwitchBack() {
		t.Error("a capture on its from-start path is offered a switch back")
	}
	r.markHlsFallback()
	if !r.canSwitchBack() {
		t.Error("a capture an HLS fallback put on the playlist is not offered a switch back")
	}
	r.markSwitchedBack()
	if r.canSwitchBack() || r.noJS() {
		t.Error("the switch back left the run on a fallback")
	}
	r.markNoJS()
	if r.canSwitchBack() {
		t.Error("a second switch back is offered")
	}
}

// A fallback onto the live HLS playlist marks the run (liveHlsInstead), so
// the loop's switch back is offered to it too; a fallback that could not set
// HLS up either marks nothing.
//
// Mutant: liveHlsInstead not calling markHlsFallback.
func TestLiveHlsFallbackMarksTheRun(t *testing.T) {
	job := &JobContext{Job: &database.Job{ID: "hlsfb"}, Run: &runRecord{}}
	failing := &fakeStrategy{kind: "hls", answers: []error{fmt.Errorf("no master")}}
	if _, err := liveHlsInstead(context.Background(), job, failing, &youtube.VideoInfo{}, nil, fmt.Errorf("setup")); err == nil {
		t.Fatal("a failed HLS setup was not reported")
	}
	if job.Run.canSwitchBack() {
		t.Error("a failed HLS fallback marked the run")
	}
	if _, err := liveHlsInstead(context.Background(), job, &fakeStrategy{kind: "hls"}, &youtube.VideoInfo{}, nil, fmt.Errorf("setup")); err != nil {
		t.Fatal(err)
	}
	if !job.Run.canSwitchBack() {
		t.Error("the HLS fallback did not mark the run, so the loop never offers it a switch back")
	}
}
