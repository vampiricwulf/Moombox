package worker

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestStreamParamsEqual(t *testing.T) {
	base := &streamParams{
		VCodec:     "h264",
		Width:      1920,
		Height:     1080,
		FrameRate:  "30/1",
		ACodec:     "aac",
		SampleRate: "44100",
		Channels:   2,
	}

	t.Run("identical", func(t *testing.T) {
		other := *base
		if !base.equal(&other) {
			t.Error("identical streamParams should be equal")
		}
	})

	t.Run("nil receiver", func(t *testing.T) {
		var p *streamParams
		if p.equal(base) {
			t.Error("nil receiver should never equal a non-nil streamParams")
		}
	})

	t.Run("nil argument", func(t *testing.T) {
		if base.equal(nil) {
			t.Error("non-nil streamParams should never equal nil")
		}
	})

	t.Run("both nil", func(t *testing.T) {
		var p *streamParams
		if p.equal(nil) {
			t.Error("nil.equal(nil) should be false, not a vacuous true")
		}
	})

	fieldCases := []struct {
		name   string
		mutate func(*streamParams)
	}{
		{"VCodec differs", func(p *streamParams) { p.VCodec = "vp9" }},
		{"Width differs", func(p *streamParams) { p.Width = 1280 }},
		{"Height differs", func(p *streamParams) { p.Height = 720 }},
		{"FrameRate differs", func(p *streamParams) { p.FrameRate = "60/1" }},
		{"ACodec differs", func(p *streamParams) { p.ACodec = "opus" }},
		{"SampleRate differs", func(p *streamParams) { p.SampleRate = "48000" }},
		{"Channels differs", func(p *streamParams) { p.Channels = 1 }},
	}

	for _, tc := range fieldCases {
		t.Run(tc.name, func(t *testing.T) {
			other := *base
			tc.mutate(&other)
			if base.equal(&other) {
				t.Errorf("streamParams should differ: %s", tc.name)
			}
		})
	}
}

// TestProbeStreamParams_MissingBinary confirms a probe FAILURE (ffprobe not
// found / non-zero exit) returns an error rather than a zero-value default.
// Task 8's merge logic must abort on unknowns, not fake success — this does
// not require a live ffprobe binary since the failure is at process-launch.
func TestProbeStreamParams_MissingBinary(t *testing.T) {
	_, err := probeStreamParams(context.Background(), "moombox-nonexistent-ffprobe-binary", "irrelevant.mp4")
	if err == nil {
		t.Fatal("expected an error when ffprobe binary cannot be found, got nil")
	}
}

// requireFFmpegTools skips the test unless both ffmpeg and ffprobe are
// resolvable on PATH.
func requireFFmpegTools(t *testing.T) (ffmpegPath, ffprobePath string) {
	t.Helper()
	fp, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not found on PATH, skipping live-tool test")
	}
	pp, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not found on PATH, skipping live-tool test")
	}
	return fp, pp
}

// TestProbeStreamParams_Fixture generates a tiny real video+audio file with
// ffmpeg and asserts probeStreamParams decodes non-empty codec fields from
// both the video and audio streams.
func TestProbeStreamParams_Fixture(t *testing.T) {
	ffmpegPath, ffprobePath := requireFFmpegTools(t)

	dir := t.TempDir()
	fixture := filepath.Join(dir, "fixture.mp4")

	cmd := exec.Command(ffmpegPath,
		"-y",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=128x72:rate=30",
		"-f", "lavfi", "-i", "sine=duration=1",
		"-c:v", "libx264",
		"-c:a", "aac",
		fixture,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to generate fixture: %v\n%s", err, out)
	}

	params, err := probeStreamParams(context.Background(), ffprobePath, fixture)
	if err != nil {
		t.Fatalf("probeStreamParams: %v", err)
	}
	if params.VCodec == "" {
		t.Error("expected non-empty VCodec")
	}
	if params.Width == 0 || params.Height == 0 {
		t.Errorf("expected non-zero dimensions, got %dx%d", params.Width, params.Height)
	}
	if params.FrameRate == "" {
		t.Error("expected non-empty FrameRate")
	}
	if params.ACodec == "" {
		t.Error("expected non-empty ACodec")
	}
	if params.SampleRate == "" {
		t.Error("expected non-empty SampleRate")
	}
	if params.Channels == 0 {
		t.Error("expected non-zero Channels")
	}
}

// TestProbeStreamParams_NoVideoStream confirms an audio-only file (no video
// stream present) is a probe ERROR, never a default streamParams — Task 8's
// merge must abort on unknowns.
func TestProbeStreamParams_NoVideoStream(t *testing.T) {
	ffmpegPath, ffprobePath := requireFFmpegTools(t)

	dir := t.TempDir()
	fixture := filepath.Join(dir, "audio_only.m4a")

	cmd := exec.Command(ffmpegPath,
		"-y",
		"-f", "lavfi", "-i", "sine=duration=1",
		"-c:a", "aac",
		fixture,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to generate audio-only fixture: %v\n%s", err, out)
	}

	_, err := probeStreamParams(context.Background(), ffprobePath, fixture)
	if err == nil {
		t.Fatal("expected an error when the file has no video stream, got nil")
	}
}
