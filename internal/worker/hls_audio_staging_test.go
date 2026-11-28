package worker

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/engine"
)

func touchStaged(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("media"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestDiscoverStagingMediaFindsTheHlsAudioRendition: a demuxed HLS capture
// stages video.ts and audio.ts, and every reader built on the discovery —
// the restart mux, the Mux action, HasSegmentFiles, the part recovery —
// sees both; an audio-only recording of such a master stages audio.ts
// alone. An audio.ts never joins a DASH video_stream.
//
// Mutant: the audio.ts arm removed (the pair reads as video.ts alone, the
// audio-only one as no media at all).
func TestDiscoverStagingMediaFindsTheHlsAudioRendition(t *testing.T) {
	for _, tc := range []struct {
		name         string
		files        []string
		video, audio string
		hls          bool
	}{
		{"pair", []string{"video.ts", "audio.ts"}, "video.ts", "audio.ts", true},
		{"muxed master", []string{"video.ts"}, "video.ts", "", true},
		{"audio only", []string{"audio.ts"}, "", "audio.ts", true},
		{"dash wins and keeps its own audio", []string{"video_stream", "audio_stream", "audio.ts"}, "video_stream", "audio_stream", false},
		{"dash video never takes audio.ts", []string{"video_stream", "audio.ts"}, "video_stream", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			touchStaged(t, dir, tc.files...)
			r := discoverStagingMedia(dir)
			if r == nil {
				t.Fatal("no media discovered")
			}
			base := func(p string) string {
				if p == "" {
					return ""
				}
				return filepath.Base(p)
			}
			if base(r.VideoPath) != tc.video || base(r.AudioPath) != tc.audio || r.IsHls != tc.hls {
				t.Errorf("discovered video %q audio %q hls %v, want %q %q %v", base(r.VideoPath), base(r.AudioPath), r.IsHls, tc.video, tc.audio, tc.hls)
			}
			if r.HasVideo != (tc.video != "") || r.HasAudio != (tc.audio != "") {
				t.Errorf("HasVideo %v HasAudio %v", r.HasVideo, r.HasAudio)
			}
		})
	}
}

// TestGroupStagedAsidesJoinsTheHlsPair: the two halves of a demuxed HLS
// capture set aside together are one recording — muxed into one sibling,
// listed as one aside — including when their two downloaders stamped them a
// second apart; a muxed master's video.ts beside a DASH pair stays its own.
//
// Mutant: audio.ts not classified as an audio half (two groups).
func TestGroupStagedAsidesJoinsTheHlsPair(t *testing.T) {
	dir := t.TempDir()
	p := func(n string) string { return filepath.Join(dir, n) }
	groups := groupStagedAsides([]string{p("video.ts.restart-100"), p("audio.ts.restart-101"), p("video_stream.restart-200"), p("audio_stream.restart-200"), p("video.ts.restart-200")})
	if len(groups) != 3 {
		t.Fatalf("groups = %+v, want three", groups)
	}
	if g := groups[0]; g.video != p("video.ts.restart-100") || g.audio != p("audio.ts.restart-101") {
		t.Errorf("HLS pair group = %+v", g)
	}
	if g := groups[2]; g.video != p("video.ts.restart-200") || g.audio != "" {
		t.Errorf("lone video.ts group = %+v", g)
	}
}

// TestSetAsideOtherLiveShapeCoversTheHlsAudio: audio.ts is HLS media. A DASH
// setup sets it aside with video.ts; an HLS setup sets aside the HLS half it
// does not write — an audio.ts beside a muxed master's capture, a video.ts
// beside an audio-only one — and keeps the half it resumes.
func TestSetAsideOtherLiveShapeCoversTheHlsAudio(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result *DownloadResult
		gone   []string
		kept   []string
	}{
		{"dash", &DownloadResult{HasVideo: true, HasAudio: true}, []string{"video.ts", "audio.ts"}, nil},
		{"hls pair", &DownloadResult{IsHls: true, HasVideo: true, HasAudio: true}, nil, []string{"video.ts", "audio.ts"}},
		{"hls muxed", &DownloadResult{IsHls: true, HasVideo: true}, []string{"audio.ts"}, []string{"video.ts"}},
		{"hls audio only", &DownloadResult{IsHls: true, HasAudio: true}, []string{"video.ts"}, []string{"audio.ts"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job, _ := vodPotJob(t)
			touchStaged(t, job.StagingDir, "video.ts", "audio.ts")
			if err := setAsideOtherLiveShape(job, tc.result); err != nil {
				t.Fatal(err)
			}
			for _, n := range tc.gone {
				if fileExists(filepath.Join(job.StagingDir, n)) {
					t.Errorf("%s left in place", n)
				}
			}
			for _, n := range tc.kept {
				if !fileExists(filepath.Join(job.StagingDir, n)) {
					t.Errorf("%s set aside", n)
				}
			}
		})
	}
}

// TestMediaStartTimeOfAnHlsPairIsTheEarlierHalf: the mux places the two
// halves of a demuxed HLS capture against each other (alignHlsPair), so the
// archive's 0:00 is the earlier one's first segment; a DASH pair keeps the
// video's (RV-19).
func TestMediaStartTimeOfAnHlsPairIsTheEarlierHalf(t *testing.T) {
	dir := t.TempDir()
	v0 := time.Date(2026, 11, 27, 10, 0, 5, 0, time.UTC)
	a0 := v0.Add(-5 * time.Second)
	for _, pair := range [][2]string{{"video.ts", "audio.ts"}, {"video_stream", "audio_stream"}} {
		noteMediaStart(filepath.Join(dir, pair[0]), v0)
		noteMediaStart(filepath.Join(dir, pair[1]), a0)
	}
	if got, ok := mediaStartTime(filepath.Join(dir, "video.ts"), filepath.Join(dir, "audio.ts")); !ok || got != a0.Format(time.RFC3339Nano) {
		t.Errorf("HLS pair: %q %v, want the audio's %s", got, ok, a0.Format(time.RFC3339Nano))
	}
	if got, _ := mediaStartTime(filepath.Join(dir, "video_stream"), filepath.Join(dir, "audio_stream")); got != v0.Format(time.RFC3339Nano) {
		t.Errorf("DASH pair: %q, want the video's", got)
	}
	if got, _ := mediaStartTime("", filepath.Join(dir, "audio.ts")); got != a0.Format(time.RFC3339Nano) {
		t.Errorf("audio-only HLS: %q, want the rendition's", got)
	}
}

// writeOffsetAudioTSFixture renders seconds of AAC tone as MPEG-TS starting
// at offsetSec — a staged audio.ts.
func writeOffsetAudioTSFixture(t *testing.T, ffmpegPath, path string, seconds, offsetSec int) {
	t.Helper()
	cmd := exec.Command(ffmpegPath, "-nostdin", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:duration=%d", seconds),
		"-c:a", "aac", "-output_ts_offset", fmt.Sprint(offsetSec), "-f", "mpegts", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate audio mpegts fixture %s: %v\n%s", path, err, out)
	}
}

// TestCopyStagedPlacesTheHlsPairOnOneTimeline: two downloaders record the
// halves of a demuxed HLS capture, each from where its own playlist's window
// began, so one can start a segment after the other. FFmpeg starts each
// input at its own first timestamp and played such a pair 5 s out of step;
// the copy delays the later half by the difference instead, read from the
// two files' MPEG-TS start times.
//
// Mutant: alignHlsPair not called (both tracks start at 0 in the copy).
func TestCopyStagedPlacesTheHlsPairOnOneTimeline(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	for _, tc := range []struct {
		name                 string
		videoAt, audioAt     int
		wantVideo, wantAudio float64
	}{
		{"audio a segment late", 100, 105, 0, 5},
		{"video a segment late", 105, 100, 5, 0},
		{"level", 100, 100, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			video, audio := filepath.Join(dir, "video.ts"), filepath.Join(dir, "audio.ts")
			writeOffsetTSFixture(t, ffmpegPath, video, 20, tc.videoAt)
			writeOffsetAudioTSFixture(t, ffmpegPath, audio, 20, tc.audioAt)
			o := NewDownloadOrchestrator(nil, nil, ffmpegPath, discardLogger{}, nil, nil, nil, nil, nil)
			out := filepath.Join(dir, "out.mp4")
			if _, err := o.copyStaged(context.Background(), "j1", video, audio, out, "", nil); err != nil {
				t.Fatalf("copyStaged: %v", err)
			}
			p := o.runFFprobe(context.Background(), out)
			vs, vok := p.streamStart("video")
			as, aok := p.streamStart("audio")
			if !vok || !aok {
				t.Fatalf("output streams = %+v, want video and audio", p.Streams)
			}
			if math.Abs(vs-tc.wantVideo) > 0.3 || math.Abs(as-tc.wantAudio) > 0.3 {
				t.Errorf("output starts video %.2f audio %.2f, want %.0f and %.0f", vs, as, tc.wantVideo, tc.wantAudio)
			}
		})
	}
}

// TestSwitchBackAudioSeqKeepsTheRenditionsPosition: switching back from the
// live HLS playlist to the from-start pair starts its audio where the HLS
// capture's audio stopped: a muxed master's video position (it has no audio
// downloader), a demuxed master's own audio rendition position.
//
// Mutant: the AudioDownloader check dropped (the rendition's position is
// replaced by the video's).
func TestSwitchBackAudioSeqKeepsTheRenditionsPosition(t *testing.T) {
	muxed := &DownloadResult{IsHls: true, VideoDownloader: nilSafeDownloader(t)}
	if got := switchBackAudioSeq(muxed, 500, 0); got != 500 {
		t.Errorf("muxed master: %d, want the video's 500", got)
	}
	demuxed := &DownloadResult{IsHls: true, VideoDownloader: nilSafeDownloader(t), AudioDownloader: nilSafeDownloader(t)}
	if got := switchBackAudioSeq(demuxed, 500, 498); got != 498 {
		t.Errorf("demuxed master: %d, want the rendition's 498", got)
	}
	if got := switchBackAudioSeq(&DownloadResult{AudioDownloader: nilSafeDownloader(t)}, 500, 498); got != 498 {
		t.Errorf("DASH: %d, want 498", got)
	}
}

func nilSafeDownloader(t *testing.T) *engine.SegmentDownloader {
	t.Helper()
	return engine.NewSegmentDownloader(engine.DownloaderOptions{OutputFile: filepath.Join(t.TempDir(), "x")})
}
