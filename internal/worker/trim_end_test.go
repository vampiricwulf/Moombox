package worker

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestTrimToTheFileEndIsNotRefused: a trim that runs to the end of a
// recording whose length is not a whole number of seconds is a trim to the
// end, not past it. muxAndFinalize stores length_seconds as the probed
// duration with the fraction dropped, and the dashboard's end marker is the
// player's own fractional duration — so moving only the start marker of a
// 3.6 s archive posted 3.6 against a row reading 3, and the service refused
// it with "end time (4s) exceeds video duration (3s)". An end from the next
// whole second on is still past the file, and the refusal now says by how
// much without rounding the two bounds into each other.
//
// Mutants: put back `endTime > maxDuration` (the trim to the end is refused);
// put back "%.0fs" in the refusal (it reads "end time (4s)" for 4.25).
func TestTrimToTheFileEndIsNotRefused(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "chan", "archive.mp4")
	writeFractionalFixture(t, ffmpegPath, src, "3.6")

	db, err := database.Open(filepath.Join(dir, "trim.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	length := 3 // int(3.6), as muxAndFinalize stores it
	job := &database.Job{
		ID: "yt_trimend", VideoID: "trimend", Platform: "youtube", URL: "u",
		Status: database.StatusFinished, OutputFile: src, Filename: filepath.Join("chan", "archive.mp4"),
		LengthSeconds: &length,
	}
	if _, err := db.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	ts := NewTrimService(db, ffmpegPath, discardLogger{})

	rec, err := ts.CreateTrim(t.Context(), job, 1, 3.6, nil)
	if err != nil {
		t.Fatalf("trim [1s, 3.6s] of a 3.6 s archive (length_seconds 3): %v", err)
	}
	if rec.EndTime != 3.6 {
		t.Errorf("trim end = %v, want 3.6", rec.EndTime)
	}

	for _, tc := range []struct {
		end  float64
		want string
	}{
		{4, "end time (4s) exceeds video duration (3s)"},
		{4.25, "end time (4.25s) exceeds video duration (3s)"},
	} {
		_, err := ts.CreateTrim(t.Context(), job, 0, tc.end, nil)
		var refused *TrimRefusedError
		if !errors.As(err, &refused) {
			t.Errorf("trim [0, %v] of a 3.6 s archive: err = %v, want a refusal", tc.end, err)
			continue
		}
		if refused.Reason != tc.want {
			t.Errorf("trim [0, %v]: refusal %q, want %q", tc.end, refused.Reason, tc.want)
		}
	}
}

// TestSplitTrimRefusalSaysByHowMuch: a quality-split job's bound is the sum
// of its segments' probed durations, fraction and all, so the check itself was
// exact — but its refusal rounded both sides to whole seconds, and an end
// 0.05 s past a 3.6 s recording read "end time (4s) exceeds total duration
// (4s)".
//
// Mutant: put back "%.0fs" in the refusal.
func TestSplitTrimRefusalSaysByHowMuch(t *testing.T) {
	ts := NewTrimService(nil, "ffmpeg-not-reached", discardLogger{})
	job := &database.Job{
		ID: "yt_split", VideoID: "split", Status: database.StatusFinished,
		Segments: []database.Segment{{SegmentIndex: 0, DurationSeconds: 1.8}, {SegmentIndex: 1, DurationSeconds: 1.8}},
	}
	_, err := ts.CreateTrim(t.Context(), job, 0, 3.65, nil)
	var refused *TrimRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("trim [0, 3.65] of a 3.6 s split recording: err = %v, want a refusal", err)
	}
	if want := "end time (3.65s) exceeds total duration (3.6s)"; refused.Reason != want {
		t.Errorf("refusal %q, want %q", refused.Reason, want)
	}
}

// writeFractionalFixture renders a video whose duration is not a whole number
// of seconds — writeMuxFixture takes an int.
func writeFractionalFixture(t *testing.T, ffmpegPath, path, seconds string) {
	t.Helper()
	cmd := exec.Command(ffmpegPath, "-nostdin", "-y",
		"-f", "lavfi", "-i", "testsrc=size=64x64:rate=5:duration="+seconds,
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-movflags", "+faststart", path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate fixture %s: %v\n%s", path, err, out)
	}
}
