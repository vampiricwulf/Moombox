package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// runAudioOnlyClip runs an audio_only clip job end to end through processJob,
// every rendition of testClip served as media, and returns the finished row,
// and the paths the CDN was asked for.
func runAudioOnlyClip(t *testing.T, media []byte) (*database.Job, []string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/preview.jpg" {
			http.Error(rw, "no thumbnail here", http.StatusNotFound)
			return
		}
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		http.ServeContent(rw, r, "clip.mp4", time.Time{}, strings.NewReader(string(media)))
	}))
	t.Cleanup(srv.Close)

	w, db := testWorkerSetup(t)
	w.streamProc.clipFn = func(context.Context, string) (*twitch.TwitchClipInfo, error) {
		return testClip(srv.URL), nil
	}
	job := addClipJob(t, db, t.TempDir())
	db.UpdateJobFields(job.ID, map[string]any{"quality_preference": "audio_only"})

	w.processJob(context.Background(), job.ID)

	got, err := db.GetJob(job.ID)
	if err != nil || got == nil {
		t.Fatalf("GetJob: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	return got, append([]string(nil), paths...)
}

// clipFixture renders a one-second MP4 with FFmpeg: video, and audio unless
// silent.
func clipFixture(t *testing.T, ffmpegPath string, silent bool) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "clip.mp4")
	args := []string{"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=128x72:rate=30"}
	if !silent {
		args = append(args, "-f", "lavfi", "-i", "sine=duration=1", "-c:a", "aac", "-shortest")
	}
	args = append(args, "-c:v", "libx264", "-pix_fmt", "yuv420p", path)
	if out, err := exec.Command(ffmpegPath, args...).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v\n%s", err, out)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestTwitchClipAudioOnlyKeepsOnlyTheAudio is owner decision FQ-4 end to end:
// a clip job whose quality_preference is audio_only downloads the clip's
// smallest rendition (720p60 here, not the 1080p30 an ordinary clip takes
// nor the portrait crop), keeps only its audio by stream copy, and finishes
// with an archive in the container every audio_only job's archive takes —
// the template's .mp4 — holding one audio stream and no video.
//
// Mutants: ExecuteTwitchClip not calling twitchClipExtractAudio (the archive
// keeps the video); SelectClipQuality's audio_only arm back to the best
// rendition (/clip-1080.mp4 fetched); the extraction's result not handed to
// the finalize (result.VideoPath left on the source: the video is archived).
func TestTwitchClipAudioOnlyKeepsOnlyTheAudio(t *testing.T) {
	ffmpegPath, ffprobePath := requireFFmpegTools(t)
	got, paths := runAudioOnlyClip(t, clipFixture(t, ffmpegPath, false))

	if got.Status != database.StatusFinished {
		t.Fatalf("status = %s (error %q), want Finished", got.Status, got.Error)
	}
	if len(paths) == 0 {
		t.Fatal("the clip was never fetched")
	}
	for _, p := range paths {
		if p != "/clip-720.mp4" {
			t.Errorf("fetched %s, want only the smallest landscape rendition /clip-720.mp4", p)
		}
	}
	if !strings.HasSuffix(got.OutputFile, "[tw_clip_FaintLightGullWholeWheat].mp4") {
		t.Errorf("output_file = %q, want the template's .mp4", got.OutputFile)
	}
	out, err := exec.Command(ffprobePath, "-v", "error", "-show_entries", "stream=codec_type", "-of", "csv=p=0", got.OutputFile).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", got.OutputFile, err)
	}
	if streams := strings.Fields(string(out)); len(streams) != 1 || streams[0] != "audio" {
		t.Errorf("archive streams = %v, want one audio stream", streams)
	}
}

// TestTwitchClipAudioOnlyWithoutAudioFails: an audio_only clip whose
// rendition carries no audio track ends in Error saying so, and archives
// nothing.
//
// Mutants: the ErrNoAudioTrack arm dropped (the error is FFmpeg's own
// "matches no streams" text); ExecuteTwitchClip carrying on to the finalize
// after a failed extraction (the video is archived and the row Finished).
func TestTwitchClipAudioOnlyWithoutAudioFails(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	got, _ := runAudioOnlyClip(t, clipFixture(t, ffmpegPath, true))

	if got.Status != database.StatusError {
		t.Fatalf("status = %s, want Error", got.Status)
	}
	const want = "twitch clip error: the clip has no audio track to keep for audio_only"
	if !strings.Contains(got.Error, want) {
		t.Errorf("error = %q, want it to say %q", got.Error, want)
	}
	if got.OutputFile != "" {
		t.Errorf("output_file = %q, want none", got.OutputFile)
	}
}

// TestTwitchClipAudioOnlyStagesItsSourceOutOfTheMuxesSight: an audio_only
// clip's rendition is staged as clip-source.mp4, a name discoverStagingMedia
// does not read, so a boot re-mux or a Mux action of a row stopped before
// its audio was extracted finds no media and archives no video; once
// extracted, audio.m4a is found as the job's audio alone.
//
// Mutant: twitchClipDownloadResult writing video.mp4 for an audio_only clip
// (the staged source reads as the archive's video).
func TestTwitchClipAudioOnlyStagesItsSourceOutOfTheMuxesSight(t *testing.T) {
	w, db := testWorkerSetup(t)
	job := addClipJob(t, db, t.TempDir())
	job.QualityPreference = "audio_only"
	jobCtx := w.buildJobContext(job)
	clip := &TwitchClipDownload{Slug: "FaintLightGullWholeWheat", Quality: testClip("https://cdn.example").Qualities[0]}
	result := w.orchestrator.twitchClipDownloadResult(context.Background(), jobCtx, clip)
	if want := filepath.Join(jobCtx.StagingDir, twitchClipSourceFile); result.VideoPath != want {
		t.Fatalf("staged as %s, want %s", result.VideoPath, want)
	}
	if err := os.MkdirAll(jobCtx.StagingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(result.VideoPath, []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
	if found := discoverStagingMedia(jobCtx.StagingDir); found != nil {
		t.Errorf("the staged source was found as media: %+v", found)
	}
	if err := os.WriteFile(filepath.Join(jobCtx.StagingDir, twitchClipAudioFile), []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	if found := discoverStagingMedia(jobCtx.StagingDir); found == nil || found.HasVideo || !found.HasAudio {
		t.Errorf("staged media with the audio extracted = %+v, want the audio alone", found)
	}
}
