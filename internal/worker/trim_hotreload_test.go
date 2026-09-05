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
