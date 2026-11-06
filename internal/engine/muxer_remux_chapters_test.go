package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// TestRemuxChaptersArgs pins the chapter import's re-mux argv (owner decision
// CI-1): the archive as the one media input, the chapter list as the
// ffmetadata input mapped by its index, and `-map 0 -map_metadata 0 -c copy`
// so every stream and the global metadata ride along; with no chapter file,
// `-map_chapters -1` instead, so the archive's own chapters are dropped.
//
// Mutants: withAllStreamsKept omitting `-map 0` (FFmpeg's default selection
// keeps one video and one audio stream), omitting `-map -0:d` (each re-mux
// keeps the old chapter track as a data stream beside the new one), omitting
// `-map_metadata 0`, or
// omitting `-map_chapters -1` for a restore to none (the input's chapters
// would be copied by default).
func TestRemuxChaptersArgs(t *testing.T) {
	m := NewMuxer("ffmpeg", &testLogger{})
	args := withAllStreamsKept(m.buildArgs("archive.mp4", "", "tmp.mp4", nil, false, InputOffsets{}, "chapters.ffmeta"), false)
	want := []string{"-y", "-i", "archive.mp4", "-f", "ffmetadata", "-i", "chapters.ffmeta", "-map_chapters", "1",
		"-c", "copy", "-movflags", "faststart", "-map", "0", "-map", "-0:d", "-map_metadata", "0", "tmp.mp4"}
	if !slices.Equal(args, want) {
		t.Errorf("args = %v\nwant   %v", args, want)
	}
	args = withAllStreamsKept(m.buildArgs("archive.mp4", "", "tmp.mp4", nil, false, InputOffsets{}, ""), true)
	want = []string{"-y", "-i", "archive.mp4", "-c", "copy", "-movflags", "faststart",
		"-map", "0", "-map", "-0:d", "-map_metadata", "0", "-map_chapters", "-1", "tmp.mp4"}
	if !slices.Equal(args, want) {
		t.Errorf("no-chapters args = %v\nwant   %v", args, want)
	}
}

type remuxProbe struct {
	Chapters []struct {
		StartTime string            `json:"start_time"`
		Tags      map[string]string `json:"tags"`
	} `json:"chapters"`
	Streams []struct {
		CodecType          string `json:"codec_type"`
		DisplayAspectRatio string `json:"display_aspect_ratio"`
	} `json:"streams"`
	Format struct {
		Tags map[string]string `json:"tags"`
	} `json:"format"`
}

func (p remuxProbe) types() []string {
	var out []string
	for _, s := range p.Streams {
		out = append(out, s.CodecType)
	}
	return out
}

func probeRemux(t *testing.T, m *Muxer, path string) remuxProbe {
	t.Helper()
	out, err := exec.Command(m.FFprobePath(), "-v", "quiet", "-print_format", "json", "-show_chapters", "-show_streams", "-show_format", path).Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	var p remuxProbe
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestRemuxChaptersKeepsTheArchive runs the real FFmpeg over an archive the
// way the import meets one: video, audio, a yt:stretch display aspect, a
// global title tag and chapters of its own. The re-mux replaces the chapters
// and keeps everything else; a restore with no list leaves none; and a check
// that refuses the copy leaves the archive byte-identical.
//
// Mutants: as TestRemuxChaptersArgs' (the audio stream, the title or the
// aspect is lost, or the old chapters survive a restore to none); writeOutput
// skipping the checks (the refused copy replaces the archive).
func TestRemuxChaptersKeepsTheArchive(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not found on PATH, skipping live-tool test")
	}
	dir := t.TempDir()
	archive := filepath.Join(dir, "archive.mp4")
	oldMeta := filepath.Join(dir, "old.ffmeta")
	if err := os.WriteFile(oldMeta, []byte(utils.FFMetadataChapters(utils.SanitizeChapters([]utils.Chapter{{StartMs: 0, Title: "old"}}, 4000))), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(ffmpegPath, "-nostdin", "-y", "-v", "error",
		"-f", "lavfi", "-i", "testsrc=size=64x48:rate=5:duration=4",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=4",
		"-f", "ffmetadata", "-i", oldMeta, "-map", "0", "-map", "1", "-map_chapters", "2",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac",
		"-aspect", "16:9", "-metadata", "title=Kept Title", archive)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate the fixture: %v\n%s", err, out)
	}
	m := NewMuxer(ffmpegPath, &testLogger{})
	before := probeRemux(t, m, archive)
	if !slices.Equal(before.types(), []string{"video", "audio", "data"}) || before.Streams[0].DisplayAspectRatio != "16:9" {
		t.Fatalf("fixture streams = %+v (want video, audio and the chapter track)", before.Streams)
	}

	meta := filepath.Join(dir, "new.ffmeta")
	list := utils.SanitizeChapters([]utils.Chapter{{StartMs: 0, Title: "one"}, {StartMs: 2000, Title: "two"}}, 4000)
	if err := os.WriteFile(meta, []byte(utils.FFMetadataChapters(list)), 0o644); err != nil {
		t.Fatal(err)
	}
	// Twice: a second import must not stack a second chapter track.
	for range 2 {
		if err := m.RemuxChapters(context.Background(), archive, archive, meta); err != nil {
			t.Fatalf("RemuxChapters: %v", err)
		}
	}
	after := probeRemux(t, m, archive)
	if len(after.Chapters) != 2 || after.Chapters[0].Tags["title"] != "one" || after.Chapters[1].Tags["title"] != "two" || after.Chapters[1].StartTime != "2.000000" {
		t.Errorf("chapters = %+v, want one@0 and two@2", after.Chapters)
	}
	if types := after.types(); !slices.Equal(types, []string{"video", "audio", "data"}) {
		t.Errorf("streams = %v, want video, audio and one chapter track", types)
	}
	if after.Streams[0].DisplayAspectRatio != "16:9" {
		t.Errorf("display aspect = %q, want the archive's 16:9 kept", after.Streams[0].DisplayAspectRatio)
	}
	if after.Format.Tags["title"] != "Kept Title" {
		t.Errorf("global title = %q, want it kept", after.Format.Tags["title"])
	}

	// A restore to none.
	if err := m.RemuxChapters(context.Background(), archive, archive, ""); err != nil {
		t.Fatalf("RemuxChapters (none): %v", err)
	}
	if got := probeRemux(t, m, archive); len(got.Chapters) != 0 || !slices.Equal(got.types(), []string{"video", "audio"}) {
		t.Errorf("restore to none: chapters %+v, streams %v", got.Chapters, got.types())
	}

	// A refused copy leaves the archive as it was, and no temporary file.
	orig, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	refuse := errors.New("refused")
	if err := m.RemuxChapters(context.Background(), archive, archive, meta, func(context.Context, string) error { return refuse }); !errors.Is(err, refuse) {
		t.Fatalf("want the check's refusal, got %v", err)
	}
	if now, _ := os.ReadFile(archive); !bytes.Equal(now, orig) {
		t.Error("a refused re-mux changed the archive")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if IsMuxTempPath(e.Name()) {
			t.Errorf("temporary output %s left behind", e.Name())
		}
	}
}
