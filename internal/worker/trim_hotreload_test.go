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
// mux/probe/concat for every download, and the trim service it spawns per job
// is built from the orchestrator's remembered path — both must follow a save.
func TestOrchestratorSetFfmpegPathRebuildsMuxer(t *testing.T) {
	o := NewDownloadOrchestrator(nil, nil, "", nopWorkerLogger{}, nil, nil, nil, nil, nil)
	if got := o.mux().FFprobePath(); got != "ffprobe" {
		t.Fatalf("default ffprobe = %q", got)
	}
	if got := o.ffmpegPathValue(); got != "" {
		t.Fatalf("default ffmpeg path = %q, want empty", got)
	}

	o.SetFfmpegPath("C:/tools/ffmpeg.exe")
	if got := o.mux().FFprobePath(); got != "C:/tools/ffprobe.exe" && got != `C:\tools\ffprobe.exe` {
		t.Errorf("after SetFfmpegPath, ffprobe = %q", got)
	}
	if got := o.ffmpegPathValue(); got != "C:/tools/ffmpeg.exe" {
		t.Errorf("remembered ffmpeg path = %q, want C:/tools/ffmpeg.exe (the per-job TrimService is built from it)", got)
	}

	o.SetFfmpegPath("")
	if got := o.mux().FFprobePath(); got != "ffprobe" {
		t.Errorf("blank path must restore PATH lookup, got %q", got)
	}
	if got := o.ffmpegPathValue(); got != "" {
		t.Errorf("remembered ffmpeg path after blank = %q, want empty", got)
	}
}

// TestWorkerSetFfmpegPathForwards: the worker forwards to its orchestrator, and
// tolerates a nil one (early wiring).
func TestWorkerSetFfmpegPathForwards(t *testing.T) {
	(&DownloadWorker{}).SetFfmpegPath("C:/tools/ffmpeg.exe") // must not panic

	w := &DownloadWorker{orchestrator: NewDownloadOrchestrator(nil, nil, "", nopWorkerLogger{}, nil, nil, nil, nil, nil)}
	w.SetFfmpegPath("C:/tools/ffmpeg.exe")
	if got := w.orchestrator.ffmpegPathValue(); got != "C:/tools/ffmpeg.exe" {
		t.Errorf("orchestrator ffmpeg path = %q after the worker forwarded", got)
	}
}
