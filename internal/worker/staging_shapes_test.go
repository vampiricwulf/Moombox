package worker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// serveWholeFile answers ranged GETs for body the way googlevideo answers a
// whole-file adaptive URL, and returns the server.
func serveWholeFile(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var s, e int64
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &s, &e)
		if e <= 0 || e >= int64(len(body)) {
			e = int64(len(body)) - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", s, e, len(body)))
		w.Header().Set("Content-Length", strconv.FormatInt(e-s+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(body[s : e+1])
	}))
	t.Cleanup(srv.Close)
	return srv
}

// wholeFileVodInfo is a finished VOD whose one video-only format is served by
// srv: plain URL, no cipher, a client that needs no GVS token.
func wholeFileVodInfo(srv *httptest.Server, size int) *youtube.VideoInfo {
	w, h, fps := 1280, 720, 30
	return &youtube.VideoInfo{
		PlayerURL:    "https://www.youtube.com/s/player/abcd1234/player_ias.vflset/en_US/base.js",
		StreamStatus: youtube.StreamVOD,
		Formats: []youtube.Format{
			{Itag: 136, URL: srv.URL + "/videoplayback?id=o-AKvod&itag=136", MimeType: `video/mp4; codecs="avc1.4d401f"`, Bitrate: 2_000_000, Width: &w, Height: &h, Fps: &fps, ContentLength: strconv.Itoa(size), Source: "android_vr"},
		},
	}
}

// TestWholeFileVodSetsAsideEarlierLiveCapture pins V4's source fix: the
// whole-file VOD download clears its staging dir of the live-shape capture an
// earlier run left there — set aside under ONE engine stamp (so the DASH
// halves stay one recording), resume sidecar travelling with it — so the
// restart discovery afterwards sees only the download it just made.
//
// Mutant: drop the setAsideLiveShapesForVod call from DownloadVod — the stale
// video_stream stays and discoverStagingMedia picks it over video.mp4.
func TestWholeFileVodSetsAsideEarlierLiveCapture(t *testing.T) {
	job, _ := vodPotJob(t)
	dir := job.StagingDir
	for _, name := range []string{"video_stream", "audio_stream"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("\x00\x00\x00\x18ftypdash earlier post-live capture"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "video_stream.resume.json"), []byte(`{"lastSeq":7}`), 0o644); err != nil {
		t.Fatal(err)
	}

	body := append([]byte("\x00\x00\x00\x18ftypdash"), make([]byte, 1000)...)
	srv := serveWholeFile(t, body)
	res, err := DownloadVod(context.Background(), job, wholeFileVodInfo(srv, len(body)), stubCipherSolver{}, nil, nil)
	if err != nil {
		t.Fatalf("DownloadVod: %v", err)
	}
	o := &DownloadOrchestrator{logger: discardLogger{}}
	if err := o.runDownloaders(context.Background(), res); err != nil {
		t.Fatalf("download: %v", err)
	}

	for _, name := range []string{"video_stream", "audio_stream", "video_stream.resume.json"} {
		if fileExists(filepath.Join(dir, name)) {
			t.Errorf("%s is still in staging beside the whole-file download", name)
		}
	}
	asides := stagedRestartAsides(dir)
	if len(asides) != 2 {
		t.Fatalf("asides = %v, want the earlier capture's video and audio halves", asides)
	}
	if groups := groupStagedAsides(asides); len(groups) != 1 || groups[0].video == "" || groups[0].audio == "" {
		t.Errorf("the two halves were stamped apart, so they no longer mux as one recording: %+v", groups)
	}
	if !fileExists(engine.StagedRestartSidecar(groups0Video(t, asides))) {
		t.Errorf("the resume sidecar did not travel with its recording")
	}
	if got := discoverStagingMedia(dir); got == nil || filepath.Base(got.VideoPath) != "video.mp4" {
		t.Errorf("restart discovery picks %+v, want the whole-file video.mp4", got)
	}
}

// groups0Video returns the video_stream aside among asides.
func groups0Video(t *testing.T, asides []string) string {
	t.Helper()
	for _, a := range asides {
		if strings.HasPrefix(filepath.Base(a), "video_stream"+engine.StagedRestartSuffix) {
			return a
		}
	}
	t.Fatalf("no video_stream aside in %v", asides)
	return ""
}

// TestRestartMuxAfterWholeFileVodArchivesTheVod is V4 end to end: a VOD job
// whose earlier post-live attempt left a 2 s capture in staging downloads the
// complete 10 s recording as a whole file, and the process restarts in Muxing
// (the restart mux, muxOnRestart → muxFromStaging). The archive must be the
// complete download, and the earlier capture must survive as its own sibling
// rather than be deleted with staging.
//
// Mutant: drop the setAsideLiveShapesForVod call from DownloadVod — the
// archive is the 2 s capture and the 10 s download is deleted.
func TestRestartMuxAfterWholeFileVodArchivesTheVod(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	w, db := testWorkerSetup(t)
	staging, outputDir := muxFixtureJob(t, w, db, "j-vodshape")

	writeAsideFixture(t, ffmpegPath, filepath.Join(staging, "video_stream"), 2)
	full := filepath.Join(t.TempDir(), "full.mp4")
	writeMuxFixture(t, ffmpegPath, full, 10)
	body, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	srv := serveWholeFile(t, body)

	job, _ := db.GetJob("j-vodshape")
	jobCtx := w.buildJobContext(job)
	res, err := DownloadVod(context.Background(), jobCtx, wholeFileVodInfo(srv, len(body)), stubCipherSolver{}, nil, nil)
	if err != nil {
		t.Fatalf("DownloadVod: %v", err)
	}
	if err := w.orchestrator.runDownloaders(context.Background(), res); err != nil {
		t.Fatalf("download: %v", err)
	}

	w.enqueueExistingJobs() // the restart: a Muxing row re-muxes from staging
	w.Stop()

	fresh, _ := db.GetJob("j-vodshape")
	if fresh.Status != database.StatusFinished {
		t.Fatalf("status = %s (%q), want Finished", fresh.Status, fresh.Error)
	}
	archive := w.orchestrator.runFFprobe(context.Background(), fresh.OutputFile)
	if archive == nil || archive.DurationSec < 9 {
		t.Fatalf("archive %s probes %+v, want the complete 10 s download", fresh.OutputFile, archive)
	}
	var sibling string
	for _, n := range mp4sIn(t, outputDir) {
		if _, ok := engine.RestartSiblingStem(n); ok {
			sibling = filepath.Join(outputDir, n)
		}
	}
	if sibling == "" {
		t.Fatalf("outputs = %v, want the earlier capture kept as a .restart- sibling", mp4sIn(t, outputDir))
	}
	if p := w.orchestrator.runFFprobe(context.Background(), sibling); p == nil || p.DurationSec > 3 {
		t.Errorf("sibling %s probes %+v, want the 2 s earlier capture", sibling, p)
	}
}

// TestSetAsideStagedMediaNeverRenamesOntoAnAside pins the stamp search: a
// second set-aside inside the same second must not rename onto the first,
// which on POSIX silently replaces it.
//
// Mutant: drop the stamp search (always use time.Now().Unix()) — the second
// call's aside lands on the first's name and the first recording is gone.
func TestSetAsideStagedMediaNeverRenamesOntoAnAside(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().Unix()
	// Asides for the next few seconds already exist, so whatever second the
	// call reads, its first choice is taken.
	for s := now; s < now+5; s++ {
		p := filepath.Join(dir, "video_stream"+engine.StagedRestartSuffix+strconv.FormatInt(s, 10))
		if err := os.WriteFile(p, []byte("first"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "video_stream"), []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	asides, err := setAsideStagedMedia(dir, liveShapeStagingNames)
	if err != nil || len(asides) != 1 {
		t.Fatalf("setAsideStagedMedia = %v, %v", asides, err)
	}
	for s := now; s < now+5; s++ {
		p := filepath.Join(dir, "video_stream"+engine.StagedRestartSuffix+strconv.FormatInt(s, 10))
		if b, _ := os.ReadFile(p); string(b) != "first" {
			t.Errorf("%s was overwritten: %q", filepath.Base(p), b)
		}
	}
	if b, _ := os.ReadFile(asides[0]); string(b) != "second" {
		t.Errorf("the new aside %s holds %q", asides[0], b)
	}
}

// TestSetAsideStagedMediaRemovesEmptyFiles: an empty capture has nothing to
// recover, and an aside FFmpeg cannot open is shielded in staging for good.
//
// Mutant: drop the Size()==0 branch — the empty file becomes an aside.
func TestSetAsideStagedMediaRemovesEmptyFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "video.ts"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	asides, err := setAsideStagedMedia(dir, liveShapeStagingNames)
	if err != nil || len(asides) != 0 {
		t.Fatalf("setAsideStagedMedia = %v, %v; want nothing set aside", asides, err)
	}
	if fileExists(filepath.Join(dir, "video.ts")) || len(stagedRestartAsides(dir)) != 0 {
		t.Errorf("the empty capture is still in staging")
	}
}
