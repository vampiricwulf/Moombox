package worker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHasStagingFiles(t *testing.T) {
	dir := t.TempDir()
	// No staging dir
	if HasStagingFiles(dir, "job1") {
		t.Fatal("expected false for missing dir")
	}
	// Empty staging dir
	os.MkdirAll(filepath.Join(dir, "job1"), 0o755)
	if HasStagingFiles(dir, "job1") {
		t.Fatal("expected false for empty dir")
	}
	// With file
	os.WriteFile(filepath.Join(dir, "job1", "chat.json"), []byte("{}"), 0o644)
	if !HasStagingFiles(dir, "job1") {
		t.Fatal("expected true")
	}
}

func TestHasSegmentFiles(t *testing.T) {
	dir := t.TempDir()
	jobDir := filepath.Join(dir, "job1")
	os.MkdirAll(jobDir, 0o755)
	// No segments — only a non-segment file
	os.WriteFile(filepath.Join(jobDir, "chat.json"), []byte("{}"), 0o644)
	if HasSegmentFiles(dir, "job1") {
		t.Fatal("expected false for non-segment files")
	}
	// DASH video segment
	os.WriteFile(filepath.Join(jobDir, "video_stream"), []byte("data"), 0o644)
	if !HasSegmentFiles(dir, "job1") {
		t.Fatal("expected true for video_stream")
	}
	// Remove video_stream, test audio_stream
	os.Remove(filepath.Join(jobDir, "video_stream"))
	os.WriteFile(filepath.Join(jobDir, "audio_stream"), []byte("data"), 0o644)
	if !HasSegmentFiles(dir, "job1") {
		t.Fatal("expected true for audio_stream")
	}
	// Remove audio_stream, test video.ts (HLS)
	os.Remove(filepath.Join(jobDir, "audio_stream"))
	os.WriteFile(filepath.Join(jobDir, "video.ts"), []byte("data"), 0o644)
	if !HasSegmentFiles(dir, "job1") {
		t.Fatal("expected true for video.ts")
	}
	// Remove video.ts, test video.mp4 (VOD)
	os.Remove(filepath.Join(jobDir, "video.ts"))
	os.WriteFile(filepath.Join(jobDir, "video.mp4"), []byte("data"), 0o644)
	if !HasSegmentFiles(dir, "job1") {
		t.Fatal("expected true for video.mp4")
	}
	// Remove video.mp4, test audio.m4a (VOD)
	os.Remove(filepath.Join(jobDir, "video.mp4"))
	os.WriteFile(filepath.Join(jobDir, "audio.m4a"), []byte("data"), 0o644)
	if !HasSegmentFiles(dir, "job1") {
		t.Fatal("expected true for audio.m4a")
	}
}
