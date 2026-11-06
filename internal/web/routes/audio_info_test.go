package routes

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestProbeAudioStreamIsFFprobe pins the seam's production value.
func TestProbeAudioStreamIsFFprobe(t *testing.T) {
	if reflect.ValueOf(probeAudioStream).Pointer() != reflect.ValueOf(ffprobeAudioStream).Pointer() {
		t.Error("probeAudioStream is not ffprobeAudioStream")
	}
}

// TestAudioInfoRoute pins GET /api/jobs/{id}/audio-info (owner decision
// PR-R7): the archive the player plays is probed — the first part of a split
// job, else the single file — and its first audio stream's codec and
// channels answered; a job with no archive answers {} without a probe.
//
// Mutants: the route not registered (404); the split job's single
// output_file probed instead of its first part, or its last part instead of
// its first; the probe's answer dropped ({} for the single file); the
// containment guard dropped (the part outside the output directory is
// probed).
func TestAudioInfoRoute(t *testing.T) {
	f := newChapterImportFixture(t, &fakeChapterImporter{})
	var probed []string
	orig := probeAudioStream
	probeAudioStream = func(_ context.Context, _, file string) (AudioInfo, error) {
		probed = append(probed, filepath.Base(file))
		return AudioInfo{Codec: "eac3", Channels: 6}, nil
	}
	t.Cleanup(func() { probeAudioStream = orig })

	single := filepath.Join(f.outputDir, "single.mp4")
	part1 := filepath.Join(f.outputDir, "split - part1.mp4")
	part2 := filepath.Join(f.outputDir, "split - part2.mp4")
	outside := filepath.Join(t.TempDir(), "elsewhere - part1.mp4")
	for _, p := range []string{single, part1, part2, outside} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.addJob(t, "one", func(j *database.Job) {
		j.Status = database.StatusFinished
		j.OutputFile = single
		j.Filename = "single.mp4"
	})
	f.addJob(t, "split", func(j *database.Job) {
		j.Status = database.StatusFinished
		j.OutputFile = single
		j.Segments = []database.Segment{
			{SegmentIndex: 0, Filename: filepath.Base(part1), FilePath: part1, DurationSeconds: 10},
			{SegmentIndex: 1, Filename: filepath.Base(part2), FilePath: part2, DurationSeconds: 10},
		}
	})
	f.addJob(t, "outside", func(j *database.Job) {
		j.Status = database.StatusFinished
		j.Segments = []database.Segment{{SegmentIndex: 0, Filename: filepath.Base(outside), FilePath: outside, DurationSeconds: 10}}
	})
	f.addJob(t, "none", func(j *database.Job) { j.Status = database.StatusError })

	var got AudioInfo
	f.getJSON(t, "/api/jobs/one/audio-info", &got)
	if got != (AudioInfo{Codec: "eac3", Channels: 6}) {
		t.Errorf("single file: %+v", got)
	}
	got = AudioInfo{}
	f.getJSON(t, "/api/jobs/split/audio-info", &got)
	if len(probed) != 2 || probed[1] != filepath.Base(part1) {
		t.Errorf("probed %v, want the single file then the split job's first part", probed)
	}
	got = AudioInfo{Codec: "x"}
	if body := f.getJSON(t, "/api/jobs/none/audio-info", &got); len(probed) != 2 || body != "{}\n" && body != "{}" {
		t.Errorf("a job with no archive: body %q, probed %v", body, probed)
	}
	got = AudioInfo{Codec: "x"}
	if body := f.getJSON(t, "/api/jobs/outside/audio-info", &got); len(probed) != 2 || body != "{}\n" && body != "{}" {
		t.Errorf("a part outside the output directory: body %q, probed %v; want {} and no probe", body, probed)
	}
}

// TestFFprobeAudioStreamReadsTheCodec runs the real probe over a generated
// file with stereo AAC audio, and over one with none. Skipped without FFmpeg.
//
// Mutants: -select_streams not a:0 (the video stream's codec, h264, comes
// back); channels not decoded (0).
func TestFFprobeAudioStreamReadsTheCodec(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not on PATH")
	}
	dir := t.TempDir()
	withAudio := filepath.Join(dir, "a.mp4")
	if out, err := exec.Command(ffmpeg, "-nostdin", "-y", "-v", "error",
		"-f", "lavfi", "-i", "testsrc=size=32x32:rate=5:duration=1",
		"-f", "lavfi", "-i", "sine=duration=1", "-ac", "2",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac", withAudio).CombinedOutput(); err != nil {
		t.Fatalf("generate: %v\n%s", err, out)
	}
	got, err := ffprobeAudioStream(context.Background(), ffprobe, withAudio)
	if err != nil || got != (AudioInfo{Codec: "aac", Channels: 2}) {
		t.Errorf("probe = %+v, %v; want aac, 2 channels", got, err)
	}
	silent := filepath.Join(dir, "v.mp4")
	if out, err := exec.Command(ffmpeg, "-nostdin", "-y", "-v", "error",
		"-f", "lavfi", "-i", "testsrc=size=32x32:rate=5:duration=1",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", silent).CombinedOutput(); err != nil {
		t.Fatalf("generate: %v\n%s", err, out)
	}
	if got, err := ffprobeAudioStream(context.Background(), ffprobe, silent); err != nil || got != (AudioInfo{}) {
		t.Errorf("probe of a file with no audio = %+v, %v; want none", got, err)
	}
}
