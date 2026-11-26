package worker

import (
	"context"
	"errors"
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

// A staged part of another rendition that the switch back closes, not the
// split: the restart set up on the no-JS fallback formats (another itag than
// the staged part's), its first run is refused with
// engine.ErrRenditionChanged, and by the refresh the challenges solve again,
// so switchBack closes the staged part and opens the new one. The closed
// part's row carries the quality its file holds, as the split's does (owner
// ruling R4-1), not the one the restart chose.
//
// Mutant: currentQuality read from the file only in the split branch (the
// row reads 1080p, the fallback's).
func TestARenditionRefusedPartClosedByTheSwitchBackKeepsItsOwnQuality(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	synctest.Test(t, func(t *testing.T) {
		w, db := testWorkerSetup(t)
		pin := 299
		job := &database.Job{ID: "rend-back", VideoID: "rend-back", URL: "u", Platform: "youtube", Status: database.StatusDownloading,
			SelectedVideoItag: &pin}
		if _, err := db.AddJob(job); err != nil {
			t.Fatal(err)
		}
		o := w.orchestrator
		gate := &gateSolver{}
		gate.open.Store(true)
		o.routedCipher, o.cipherSolver, o.potProvider = gate, nil, &bgutils.PotProvider{}
		o.SetFfmpegPath(ffmpegPath)
		root := t.TempDir()
		jobCtx := &JobContext{Job: job, DB: db, Config: &JobConfig{}, YT: youtube.NewService(nil, &discardLogger{}),
			StagingDir: root, OutputDir: t.TempDir(), Filename: "rend-back", Logger: &captureLogger{},
			Interruption: &interruptionSignal{}, Run: &runRecord{}}
		jobCtx.Run.markNoJS()

		// The part staged before the restart: 64x64, whatever was chosen now.
		writeMuxFixture(t, ffmpegPath, filepath.Join(root, "video_stream"), 1)
		refused := engine.NewSegmentDownloader(engine.DownloaderOptions{OutputFile: filepath.Join(root, "video_stream"),
			BaseURL: "https://r2.googlevideo.com/videoplayback?itag=299", StartSeq: 43, ForceStartSeq: true})
		result := &DownloadResult{VideoDownloader: refused, VideoPath: filepath.Join(root, "video_stream"),
			HasVideo: true, VideoItag: 299, VideoWidth: 1920, VideoHeight: 1080, VideoFps: 30}

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
		runLiveDownloaders = func(_ *DownloadOrchestrator, _ context.Context, res *DownloadResult) error {
			mu.Lock()
			defer mu.Unlock()
			runs++
			if runs == 1 {
				return fmt.Errorf("%w: %s", engine.ErrRenditionChanged, res.VideoPath)
			}
			return nil
		}
		recordDownloaders(t)
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
		if jobCtx.Run.noJS() {
			t.Fatal("the run did not switch back: this test's part was closed by another exit")
		}
		segs, err := db.GetSegments(job.ID)
		if err != nil || len(segs) == 0 {
			t.Fatalf("segment rows = %v (err %v), want the closed staged part's", segs, err)
		}
		if segs[0].SegmentIndex != 0 || segs[0].Quality != "64p" {
			t.Errorf("the closed part's row is part %d at %q, want part 0 at the staged file's 64p", segs[0].SegmentIndex, segs[0].Quality)
		}
	})
}

// TestRestartPairStopsAtTheRefusedRenditionWithBothNextSegments runs the
// restart's real downloader pair through runDownloaders, the way the live
// loop's first run does, for the common trigger: a lowered resolution cap
// changes the video itag (299 -> 298) and leaves the audio's (140) alone, and
// the restart came before any resume sidecar (each file has only its
// rendition marker; the DB's last seq, 42, is the position). The video
// refuses its file, the audio resumes its own and is cancelled with the
// group, and the pair stops with engine.ErrRenditionChanged, not the audio's
// cancellation, both at segment 43 — the positions the live loop builds the
// new part from — with neither staged file written to.
//
// Mutants: drop the engine's rendition check (the video appends and the pair
// ends on the cancellation); leave the video's refused position at the DB's
// last seq (42, the new part re-fetches a segment the old one holds).
func TestRestartPairStopsAtTheRefusedRenditionWithBothNextSegments(t *testing.T) {
	w, _ := testWorkerSetup(t)
	root := t.TempDir()
	staged := func(name, marker string) string {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, make([]byte, 4096), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+".rendition", []byte(marker+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	videoPath := staged("video_stream", "abcdefghijk/299")
	audioPath := staged("audio_stream", "abcdefghijk/140")
	downloader := func(path, itag string) *engine.SegmentDownloader {
		d := engine.NewSegmentDownloader(engine.DownloaderOptions{OutputFile: path, StartSeq: 42, MaxRetries: 1,
			BaseURL: "http://127.0.0.1:1/videoplayback?id=abcdefghijk.1&itag=" + itag})
		d.SplitOnRenditionChange = true
		return d
	}
	result := &DownloadResult{VideoDownloader: downloader(videoPath, "298"), AudioDownloader: downloader(audioPath, "140"),
		VideoPath: videoPath, AudioPath: audioPath, HasVideo: true, HasAudio: true}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := w.orchestrator.runDownloaders(ctx, result)
	if !errors.Is(err, engine.ErrRenditionChanged) {
		t.Fatalf("runDownloaders = %v, want the video's refusal of another rendition", err)
	}
	if got := result.VideoDownloader.CurrentSeq(); got != 43 {
		t.Errorf("video CurrentSeq() = %d, want the next segment 43", got)
	}
	if got := result.AudioDownloader.CurrentSeq(); got != 43 {
		t.Errorf("audio CurrentSeq() = %d, want the next segment 43", got)
	}
	for _, p := range []string{videoPath, audioPath} {
		if info, statErr := os.Stat(p); statErr != nil || info.Size() != 4096 {
			t.Errorf("%s changed: %v, %v", p, info, statErr)
		}
	}
}

// TestDashManifestRestartSetsAsideAnotherRendition is owner ruling R4-1 on
// the DASH-manifest path (DashStrategy: an OTF or VOD download with a
// manifest, recorded as one file in the staging root, with no part to split
// into). A restart whose chosen itag (299) is not the one the staged file was
// recorded in (the rendition marker names 303), with the DB's last seq to
// fall back on, must not append 299's fragments under 303's init: the staged
// file is set aside as <file>.restart-<ts> with its bytes, and the download
// starts over in a fresh file marked with its own rendition.
//
// Mutant: drop RestartOnRenditionChange from DownloadDash's video downloader
// (the staged file is appended to on the DB seq and nothing is set aside).
func TestDashManifestRestartSetsAsideAnotherRendition(t *testing.T) {
	prev := newSegmentDownloader
	t.Cleanup(func() { newSegmentDownloader = prev })
	// The manifest's googlevideo host is sent to a dead local port; the path,
	// which carries the rendition, is kept.
	newSegmentDownloader = func(opts engine.DownloaderOptions) *engine.SegmentDownloader {
		opts.BaseURL = strings.Replace(opts.BaseURL, "https://r4---sn.googlevideo.com", "http://127.0.0.1:1", 1)
		opts.MaxRetries = 1
		d := engine.NewSegmentDownloader(opts)
		d.SetFastDelaysForTests(engineFastScale)
		return d
	}

	srv := newManifestServer(t, potTestMPD)
	job, _ := vodPotJob(t)
	job.Run = &runRecord{}
	last := 42
	job.Job.LastVideoSeq, job.Job.LastAudioSeq = &last, &last
	video := filepath.Join(job.StagingDir, "video_stream")
	headed := make([]byte, 4096)
	copy(headed[4:], "ftyp")
	if err := os.WriteFile(video, headed, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(video+".rendition", []byte("abc/303\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	info := &youtube.VideoInfo{
		StreamStatus:       youtube.StreamVOD,
		DashManifestURL:    srv.URL + "/api/manifest/dash/id/abc",
		DashManifestSource: "tv_auth",
		Formats: []youtube.Format{
			{Itag: 299, MimeType: `video/mp4; codecs="avc1.64002a"`},
			{Itag: 140, MimeType: `audio/mp4; codecs="mp4a.40.2"`},
		},
	}
	res, err := DownloadDash(context.Background(), job, info, nil, nil, &bgutils.PotProvider{}, nil)
	if err != nil {
		t.Fatalf("DownloadDash: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res.VideoDownloader.OnActivity = func(engine.DownloadActivity) { cancel() }
	if err := res.VideoDownloader.Start(ctx); errors.Is(err, engine.ErrStagedMediaPresent) || errors.Is(err, engine.ErrRenditionChanged) {
		t.Fatalf("Start = %v, want the download started over", err)
	}

	asides, _ := filepath.Glob(video + engine.StagedRestartSuffix + "*")
	if len(asides) != 1 {
		t.Fatalf("set-aside recordings = %v, want the staged file", asides)
	}
	if got, _ := os.ReadFile(asides[0]); len(got) != len(headed) {
		t.Errorf("the set-aside file holds %d bytes, want the %d the staged file held", len(got), len(headed))
	}
	if info, err := os.Stat(video); err == nil && info.Size() >= int64(len(headed)) {
		t.Errorf("video_stream is %d bytes, want a fresh file, not the staged one appended to", info.Size())
	}
	if got, _ := os.ReadFile(video + ".rendition"); strings.TrimSpace(string(got)) != "abc/299" {
		t.Errorf("rendition marker = %q, want the fresh file's abc/299", got)
	}
}
