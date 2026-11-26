package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

// TestRestartOfAnotherRenditionSplitsIntoANewPart drives the live loop
// (runLiveStreamDownload) through owner ruling R4-1: a restarted capture
// whose downloaders refuse their staged files because those hold another
// rendition (engine.ErrRenditionChanged — the sidecar or the rendition marker
// named another itag) closes the staged part as it stands, muxed as its own
// part, and records on in a new one from the position the refusal names,
// with a fresh init (the sq=0 a manifest-free part that force-starts past it
// takes its ftyp+moov from). The job is not ended, the staged files are not
// written over, and every downloader the loop runs is armed to refuse
// another rendition.
//
// Mutants: drop the isRenditionChanged branch (the refusal falls to the
// stream-end verify, which finishes the job with no new part built); build
// the new part in the current dir (its downloaders write the staged files);
// drop the forced position (StartSeq is not 43); drop the background mux (no
// part mux runs); drop SplitOnRenditionChange from attachProgress (the new
// part's downloaders are not armed).
func TestRestartOfAnotherRenditionSplitsIntoANewPart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w, db := testWorkerSetup(t)
		pin := 299
		job := &database.Job{ID: "rend1", VideoID: "rend1", URL: "u", Platform: "youtube", Status: database.StatusDownloading,
			SelectedVideoItag: &pin} // a pin: no quality monitor
		if _, err := db.AddJob(job); err != nil {
			t.Fatal(err)
		}
		o := w.orchestrator
		gate := &gateSolver{}
		gate.open.Store(true)
		o.routedCipher, o.cipherSolver, o.potProvider = gate, nil, &bgutils.PotProvider{}
		o.SetFfmpegPath(filepath.Join(t.TempDir(), "no-ffmpeg"))
		muxLogs := &captureLogger{}
		o.logger = muxLogs
		root := t.TempDir()
		jobCtx := &JobContext{Job: job, DB: db, Config: &JobConfig{}, YT: youtube.NewService(nil, &discardLogger{}),
			StagingDir: root, OutputDir: t.TempDir(), Filename: "rend1", Logger: &captureLogger{},
			Interruption: &interruptionSignal{}, Run: &runRecord{}}

		// The part staged before the restart, in another rendition.
		staged := map[string]string{}
		for _, name := range []string{"video_stream", "audio_stream", "video_stream.rendition", "audio_stream.rendition"} {
			body := "the earlier session's " + name
			if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			staged[name] = body
		}
		// The restart's downloaders, refused at the next segment, 43.
		refused := func(name string) *engine.SegmentDownloader {
			return engine.NewSegmentDownloader(engine.DownloaderOptions{OutputFile: filepath.Join(root, name),
				BaseURL: "https://r1.googlevideo.com/videoplayback?itag=299", StartSeq: 43, ForceStartSeq: true})
		}
		result := &DownloadResult{VideoDownloader: refused("video_stream"), AudioDownloader: refused("audio_stream"),
			VideoPath: filepath.Join(root, "video_stream"), AudioPath: filepath.Join(root, "audio_stream"),
			HasVideo: true, HasAudio: true, VideoItag: 299, AudioItag: 140,
			// The quality the restart chose, the same as the fresh look's: no
			// quality change splits this capture.
			VideoWidth: 1920, VideoHeight: 1080, VideoFps: 30}

		var mu sync.Mutex
		runs := 0
		live := &youtube.VideoInfo{StreamStatus: youtube.StreamLive, PlayerURL: nPlayer, Formats: liveSwitchFormats("visionos")}
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
		var armed []bool
		runLiveDownloaders = func(_ *DownloadOrchestrator, _ context.Context, res *DownloadResult) error {
			mu.Lock()
			defer mu.Unlock()
			runs++
			for _, d := range []*engine.SegmentDownloader{res.VideoDownloader, res.AudioDownloader} {
				if d != nil {
					armed = append(armed, d.SplitOnRenditionChange)
				}
			}
			if runs == 1 {
				return fmt.Errorf("%w: %s", engine.ErrRenditionChanged, res.VideoPath)
			}
			return nil
		}
		built := recordDownloaders(t)

		tracker := NewProgressTracker(db, job.ID, discardLogger{}, 0)
		var lastSeg atomicTimeValue
		_, _, err := o.runLiveStreamDownload(context.Background(), jobCtx, jobCtx, 0, true, live, result, tracker,
			func() bool { return false }, &lastSeg)
		tracker.Close()
		time.Sleep(2 * activityUpdateInterval)
		if err != nil {
			t.Fatalf("runLiveStreamDownload = %v, want the capture carried on in a new part", err)
		}

		if runs != 2 {
			t.Fatalf("downloader runs = %d, want the refused one and the new part's", runs)
		}
		for i, a := range armed {
			if !a {
				t.Errorf("downloader %d the loop ran is not armed to refuse another rendition", i)
			}
		}
		// The refresh's look rebuilt the pair in the current part, discarded
		// unrun; the new part's are the last two.
		if len(*built) != 4 {
			t.Fatalf("built %d downloaders, want the look's pair and the new part's", len(*built))
		}
		newPart := filepath.Join(root, "seg_1")
		for _, opts := range (*built)[2:] {
			if filepath.Dir(opts.OutputFile) != newPart {
				t.Errorf("a new-part downloader writes %s, want into %s", opts.OutputFile, newPart)
			}
			if opts.StartSeq != 43 || !opts.ForceStartSeq {
				t.Errorf("a new-part downloader starts at %d (forced %v), want the refused position 43", opts.StartSeq, opts.ForceStartSeq)
			}
			if !strings.Contains(opts.InitURL, "sq=0") {
				t.Errorf("a new-part downloader's init is %q, want the stream's sq=0", opts.InitURL)
			}
		}
		if n := len(logLines(muxLogs, func(m string) bool { return m == "failed to mux quality segment" })); n != 1 {
			t.Errorf("part muxes run = %d, want the staged part's one", n)
		}
		for name, body := range staged {
			got, err := os.ReadFile(filepath.Join(root, name))
			if err != nil || string(got) != body {
				t.Errorf("%s = %q (err %v), want the staged part kept as it was", name, got, err)
			}
		}
	})
}
