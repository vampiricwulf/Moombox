package engine

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSurroundAudioStreamCopiesIntoMP4 is owner decision D15's gate on the
// audio ranking (wave W20): the 5.1 renditions YouTube lists — E-AC-3 (itag
// 328) and AC-3 (380), in MP4 — are ranked in through the channels rung only
// if the archive's mux carries them. It muxes each beside H.264, VP9 (WebM,
// as YouTube serves it) and AV1 video exactly as an archive mux does
// (MuxCopy: `-c copy -movflags faststart` into .mp4) and checks the output
// holds the surround track, six channels, unchanged. Skipped where FFmpeg is
// not installed, or (per video codec) where its build has no encoder to make
// the fixture with.
//
// Mutant: buildArgs writing `-c:v copy` only (no audio codec copy: FFmpeg
// re-encodes the audio to AAC, and the codec assertion fails).
func TestSurroundAudioStreamCopiesIntoMP4(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not found on PATH, skipping live-tool test")
	}
	ffprobePath, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not found on PATH, skipping live-tool test")
	}
	dir := t.TempDir()
	gen := func(name string, args ...string) bool {
		t.Helper()
		out := filepath.Join(dir, name)
		cmd := exec.Command(ffmpegPath, append(append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...), out)...)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Logf("fixture %s not generated: %v\n%s", name, err, b)
			return false
		}
		return true
	}
	const src = "testsrc=duration=1:size=128x72:rate=30"
	videos := map[string][]string{
		"h264.mp4": {"-f", "lavfi", "-i", src, "-c:v", "libx264", "-pix_fmt", "yuv420p"},
		"vp9.webm": {"-f", "lavfi", "-i", src, "-c:v", "libvpx-vp9", "-b:v", "100k"},
		"av1.mp4":  {"-f", "lavfi", "-i", src, "-c:v", "libsvtav1", "-pix_fmt", "yuv420p"},
	}
	wantVideo := map[string]string{"h264.mp4": "h264", "vp9.webm": "vp9", "av1.mp4": "av1"}
	const surround = "pan=5.1|c0=c0|c1=c0|c2=c0|c3=c0|c4=c0|c5=c0"
	audios := map[string]string{"ec3.m4a": "eac3", "ac3.m4a": "ac3"}
	for name, codec := range audios {
		if !gen(name, "-f", "lavfi", "-i", "sine=f=440:d=1", "-af", surround, "-c:a", codec, "-b:a", "384k", "-f", "mp4") {
			t.Skipf("this FFmpeg cannot encode %s", codec)
		}
	}
	m := NewMuxer(ffmpegPath, &testLogger{})
	for video, args := range videos {
		if !gen(video, args...) {
			t.Logf("skipping %s: this FFmpeg cannot encode it", video)
			continue
		}
		for audio, codec := range audios {
			t.Run(video+"+"+audio, func(t *testing.T) {
				out := filepath.Join(dir, strings.TrimSuffix(video, filepath.Ext(video))+"-"+codec+".mp4")
				if err := m.MuxCopy(t.Context(), filepath.Join(dir, video), filepath.Join(dir, audio), out); err != nil {
					t.Fatalf("MuxCopy: %v", err)
				}
				b, err := exec.Command(ffprobePath, "-v", "error", "-show_entries", "stream=codec_name,channels", "-of", "csv=p=0", out).Output()
				if err != nil {
					t.Fatalf("ffprobe: %v", err)
				}
				var got []string
				for line := range strings.FieldsSeq(string(b)) {
					got = append(got, strings.TrimSuffix(line, ",")) // a video stream has no channels
				}
				want := []string{wantVideo[video], codec + ",6"}
				if strings.Join(got, " ") != strings.Join(want, " ") {
					t.Errorf("streams = %v, want %v", got, want)
				}
			})
		}
	}
}
