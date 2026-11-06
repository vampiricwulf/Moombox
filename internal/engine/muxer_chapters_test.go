package engine

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// TestBuildArgs_ChaptersInput pins where the chapter list goes in the argv
// (owner decision D23): one more input after the media, `-f ffmetadata -i`,
// mapped by its own index, and nothing at all without one.
//
// Mutants: -map_chapters given 0 (the video's own, empty, chapter list); the
// ffmetadata input placed ahead of the video (it would become input 0 and
// the index would name the video); the input written when no file is given.
func TestBuildArgs_ChaptersInput(t *testing.T) {
	m := NewMuxer("ffmpeg", &testLogger{})
	args := m.buildArgs("video.mp4", "audio.mp4", "output.mp4", nil, false, InputOffsets{}, "chapters.ffmeta")
	want := []string{"-y", "-i", "video.mp4", "-i", "audio.mp4", "-f", "ffmetadata", "-i", "chapters.ffmeta", "-map_chapters", "2"}
	if !slices.Equal(args[:len(want)], want) {
		t.Errorf("args = %v, want them to begin %v", args, want)
	}
	args = m.buildArgs("video.mp4", "", "output.mp4", nil, false, InputOffsets{}, "chapters.ffmeta")
	if i := slices.Index(args, "-map_chapters"); i < 0 || args[i+1] != "1" {
		t.Errorf("video-only args = %v, want -map_chapters 1", args)
	}
	if args := m.buildArgs("video.mp4", "audio.mp4", "output.mp4", nil, false, InputOffsets{}, ""); slices.Contains(args, "ffmetadata") || slices.Contains(args, "-map_chapters") {
		t.Errorf("args = %v, want no chapter input without a chapter file", args)
	}
}

// TestMuxCopyAtChaptersEmbedsThem runs the real FFmpeg: a copy given an
// FFMETADATA chapter list (utils.FFMetadataChapters, a title carrying every
// character the format reserves) comes out with those chapters, times and
// titles intact, and with its video stream — the extra input must not change
// the stream selection.
//
// Mutant: buildArgs dropping the chapter input — the output has no chapters.
func TestMuxCopyAtChaptersEmbedsThem(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not found on PATH, skipping live-tool test")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mp4")
	cmd := exec.Command(ffmpegPath, "-nostdin", "-y",
		"-f", "lavfi", "-i", "testsrc=size=64x64:rate=5:duration=6",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate the fixture: %v\n%s", err, out)
	}
	chapters := utils.SanitizeChapters([]utils.Chapter{
		{StartMs: 0, Title: "Just Chatting"},
		{StartMs: 2500, Title: `a=b; #1 \ two`},
	}, 6000)
	meta := filepath.Join(dir, "chapters.ffmeta")
	if err := os.WriteFile(meta, []byte(utils.FFMetadataChapters(chapters)), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.mp4")
	m := NewMuxer(ffmpegPath, &testLogger{})
	if err := m.MuxCopyAtChapters(context.Background(), src, "", out, InputOffsets{}, meta); err != nil {
		t.Fatalf("MuxCopyAtChapters: %v", err)
	}

	probe, err := exec.Command(m.FFprobePath(), "-v", "quiet", "-print_format", "json", "-show_chapters", "-show_streams", out).Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	var got struct {
		Chapters []struct {
			StartTime string            `json:"start_time"`
			EndTime   string            `json:"end_time"`
			Tags      map[string]string `json:"tags"`
		} `json:"chapters"`
		Streams []struct {
			CodecType string `json:"codec_type"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(probe, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Chapters) != 2 {
		t.Fatalf("output has %d chapters, want 2: %+v", len(got.Chapters), got.Chapters)
	}
	if got.Chapters[0].Tags["title"] != "Just Chatting" || got.Chapters[1].Tags["title"] != `a=b; #1 \ two` {
		t.Errorf("chapter titles = %q, %q", got.Chapters[0].Tags["title"], got.Chapters[1].Tags["title"])
	}
	if got.Chapters[1].StartTime != "2.500000" || got.Chapters[1].EndTime != "6.000000" {
		t.Errorf("second chapter = %s-%s, want 2.500000-6.000000", got.Chapters[1].StartTime, got.Chapters[1].EndTime)
	}
	video := 0
	for _, s := range got.Streams {
		if s.CodecType == "video" {
			video++
		}
	}
	if video != 1 {
		t.Errorf("output has %d video streams, want 1: %+v", video, got.Streams)
	}
}
