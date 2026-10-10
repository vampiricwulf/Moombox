package worker

import "testing"

func TestTrimServiceSetFfmpegPathRebuildsMuxer(t *testing.T) {
	ts := NewTrimService(nil, "", nopWorkerLogger{})
	if got := ts.mux().FFprobePath(); got != "ffprobe" {
		t.Fatalf("default ffprobe = %q", got)
	}
	ts.SetFfmpegPath("C:/tools/ffmpeg.exe")
	if got := ts.mux().FFprobePath(); got != "C:/tools/ffprobe.exe" && got != `C:\tools\ffprobe.exe` {
		t.Errorf("after SetFfmpegPath, ffprobe = %q", got)
	}
	ts.SetFfmpegPath("")
	if got := ts.mux().FFprobePath(); got != "ffprobe" {
		t.Errorf("blank path must restore PATH lookup, got %q", got)
	}
}

// TestOrchestratorSetFfmpegPathRebuildsMuxer: paths.ffmpeg_path is hot-reloaded
// into the DOWNLOAD path too, not only trims. The orchestrator's muxer handles
// mux/probe/concat for every download and must follow a save. (A post-download
// trim runs through the shared trim service, which the same save re-points.)
func TestOrchestratorSetFfmpegPathRebuildsMuxer(t *testing.T) {
	o := NewDownloadOrchestrator(nil, nil, "", nopWorkerLogger{}, nil, nil, nil, nil, nil)
	if got := o.mux().FFprobePath(); got != "ffprobe" {
		t.Fatalf("default ffprobe = %q", got)
	}

	o.SetFfmpegPath("C:/tools/ffmpeg.exe")
	if got := o.mux().FFprobePath(); got != "C:/tools/ffprobe.exe" && got != `C:\tools\ffprobe.exe` {
		t.Errorf("after SetFfmpegPath, ffprobe = %q", got)
	}

	o.SetFfmpegPath("")
	if got := o.mux().FFprobePath(); got != "ffprobe" {
		t.Errorf("blank path must restore PATH lookup, got %q", got)
	}
}

// TestWorkerSetFfmpegPathForwards: the worker forwards to its orchestrator, and
// tolerates a nil one (early wiring).
func TestWorkerSetFfmpegPathForwards(t *testing.T) {
	(&DownloadWorker{}).SetFfmpegPath("C:/tools/ffmpeg.exe") // must not panic

	w := &DownloadWorker{orchestrator: NewDownloadOrchestrator(nil, nil, "", nopWorkerLogger{}, nil, nil, nil, nil, nil)}
	w.SetFfmpegPath("C:/tools/ffmpeg.exe")
	if got := w.orchestrator.mux().FFprobePath(); got != "C:/tools/ffprobe.exe" && got != `C:\tools\ffprobe.exe` {
		t.Errorf("orchestrator ffprobe = %q after the worker forwarded", got)
	}
}
