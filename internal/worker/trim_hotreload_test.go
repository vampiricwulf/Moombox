package worker

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/engine"
)

// TestSetFfmpegPathRebuildsMuxer: SetFfmpegPath rebuilds the muxer of each
// owner, so its ffprobe follows the new path, and a blank path goes back to
// the PATH lookup.
func TestSetFfmpegPathRebuildsMuxer(t *testing.T) {
	for _, tc := range []struct {
		name  string
		owner interface {
			SetFfmpegPath(string)
			mux() *engine.Muxer
		}
	}{
		{"TrimService", NewTrimService(nil, "", discardLogger{})},
		// paths.ffmpeg_path is hot-reloaded into the DOWNLOAD path too, not
		// only trims. The orchestrator's muxer handles mux/probe/concat for
		// every download and must follow a save. (A post-download trim runs
		// through the shared trim service, which the same save re-points.)
		{"Orchestrator", NewDownloadOrchestrator(nil, nil, "", discardLogger{}, nil, nil, nil, nil, nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.owner.mux().FFprobePath(); got != "ffprobe" {
				t.Fatalf("default ffprobe = %q", got)
			}

			tc.owner.SetFfmpegPath("C:/tools/ffmpeg.exe")
			if got := tc.owner.mux().FFprobePath(); got != "C:/tools/ffprobe.exe" && got != `C:\tools\ffprobe.exe` {
				t.Errorf("after SetFfmpegPath, ffprobe = %q", got)
			}

			tc.owner.SetFfmpegPath("")
			if got := tc.owner.mux().FFprobePath(); got != "ffprobe" {
				t.Errorf("blank path must restore PATH lookup, got %q", got)
			}
		})
	}
}

// TestWorkerSetFfmpegPathForwards: the worker forwards to its orchestrator, and
// tolerates a nil one (early wiring).
func TestWorkerSetFfmpegPathForwards(t *testing.T) {
	(&DownloadWorker{}).SetFfmpegPath("C:/tools/ffmpeg.exe") // must not panic

	w := &DownloadWorker{orchestrator: NewDownloadOrchestrator(nil, nil, "", discardLogger{}, nil, nil, nil, nil, nil)}
	w.SetFfmpegPath("C:/tools/ffmpeg.exe")
	if got := w.orchestrator.mux().FFprobePath(); got != "C:/tools/ffprobe.exe" && got != `C:\tools\ffprobe.exe` {
		t.Errorf("orchestrator ffprobe = %q after the worker forwarded", got)
	}
}
