package engine

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// TestMuxCopyArchiveCarriesAspectAndChapters runs the real FFmpeg: the
// archive's one stream copy given both a yt:stretch aspect and a chapter list
// comes out with both — the -aspect output option and the ffmetadata input
// must not crowd each other out. Skipped where FFmpeg is not installed.
//
// Mutants: MuxCopyArchive passing "" for the aspect (DAR stays 4:3); passing
// "" for the chapters (no chapters).
func TestMuxCopyArchiveCarriesAspectAndChapters(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not found on PATH, skipping live-tool test")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not found on PATH, skipping live-tool test")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mp4")
	if b, err := exec.Command(ffmpegPath, "-nostdin", "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc=duration=4:size=320x240:rate=5", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", src).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v\n%s", err, b)
	}
	meta := filepath.Join(dir, "chapters.ffmeta")
	chapters := utils.SanitizeChapters([]utils.Chapter{{StartMs: 0, Title: "One"}, {StartMs: 2000, Title: "Two"}}, 4000)
	if err := os.WriteFile(meta, []byte(utils.FFMetadataChapters(chapters)), 0o644); err != nil {
		t.Fatal(err)
	}
	m := NewMuxer(ffmpegPath, &testLogger{})
	out := filepath.Join(dir, "out.mp4")
	if err := m.MuxCopyArchive(t.Context(), src, "", out, InputOffsets{}, "1.777778", meta); err != nil {
		t.Fatalf("MuxCopyArchive: %v", err)
	}
	b, err := exec.Command(m.FFprobePath(), "-v", "quiet", "-print_format", "json", "-show_chapters", "-show_streams", out).Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	var got struct {
		Chapters []struct {
			Tags map[string]string `json:"tags"`
		} `json:"chapters"`
		Streams []struct {
			CodecType string `json:"codec_type"`
			DAR       string `json:"display_aspect_ratio"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Chapters) != 2 || got.Chapters[1].Tags["title"] != "Two" {
		t.Errorf("chapters = %+v, want One and Two", got.Chapters)
	}
	dar := ""
	for _, s := range got.Streams {
		if s.CodecType == "video" {
			dar = s.DAR
		}
	}
	if dar != "16:9" {
		t.Errorf("display aspect = %q, want 16:9", dar)
	}
}
