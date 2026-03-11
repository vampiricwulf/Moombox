package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewSegmentDownloader_Defaults(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    "https://example.com/sq/$Number$",
		OutputFile: "test.mp4",
	})

	if d.opts.MaxRetries != MaxSegmentRetries {
		t.Errorf("MaxRetries: got %d, want %d", d.opts.MaxRetries, MaxSegmentRetries)
	}
	if d.opts.RetryDelayCap != DefaultRetryDelayCap {
		t.Errorf("RetryDelayCap: got %d, want %d", d.opts.RetryDelayCap, DefaultRetryDelayCap)
	}
	if d.opts.LiveCheckRetries != 16 {
		t.Errorf("LiveCheckRetries: got %d, want 16", d.opts.LiveCheckRetries)
	}
	if d.opts.EndSeq != -1 {
		t.Errorf("EndSeq: got %d, want -1", d.opts.EndSeq)
	}
	if d.opts.ResumeFile != "test.mp4.resume.json" {
		t.Errorf("ResumeFile: got %q, want %q", d.opts.ResumeFile, "test.mp4.resume.json")
	}
	if d.headSeq != -1 {
		t.Errorf("headSeq: got %d, want -1", d.headSeq)
	}
}

func TestNewSegmentDownloader_CustomValues(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:          "https://example.com/video",
		OutputFile:       "out.mp4",
		MaxRetries:       10,
		RetryDelayCap:    120,
		LiveCheckRetries: 32,
		EndSeq:           999,
		ResumeFile:       "custom.resume.json",
	})

	if d.opts.MaxRetries != 10 {
		t.Errorf("MaxRetries: got %d, want 10", d.opts.MaxRetries)
	}
	if d.opts.RetryDelayCap != 120 {
		t.Errorf("RetryDelayCap: got %d, want 120", d.opts.RetryDelayCap)
	}
	if d.opts.EndSeq != 999 {
		t.Errorf("EndSeq: got %d, want 999", d.opts.EndSeq)
	}
	if d.opts.ResumeFile != "custom.resume.json" {
		t.Errorf("ResumeFile: got %q, want %q", d.opts.ResumeFile, "custom.resume.json")
	}
}

func TestNewSegmentDownloader_NilLogger(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    "https://example.com/sq/$Number$",
		OutputFile: "test.mp4",
	})
	// Should not panic when calling logger methods
	d.logger.Debug("test")
	d.logger.Info("test")
	d.logger.Warn("test")
	d.logger.Error("test")
}

func TestSegmentDownloader_Cancel(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    "https://example.com/sq/$Number$",
		OutputFile: "test.mp4",
	})

	if d.isCancelled() {
		t.Error("should not be cancelled initially")
	}

	d.Cancel()
	if !d.isCancelled() {
		t.Error("should be cancelled after Cancel()")
	}
}

func TestSegmentDownloader_CancelErr(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    "https://example.com/sq/$Number$",
		OutputFile: "test.mp4",
	})

	ctx := context.Background()

	// Neither cancelled nor context done
	if err := d.cancelErr(ctx); err != nil {
		t.Errorf("expected nil, got %v", err)
	}

	// User cancel flag set
	d.Cancel()
	if err := d.cancelErr(ctx); err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}

	// Context cancelled
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d2 := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    "https://example.com/sq/$Number$",
		OutputFile: "test.mp4",
	})
	if err := d2.cancelErr(ctx); err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestSegmentDownloader_BuildSegmentURL(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		seq     int
		want    string
	}{
		{
			name:    "template with $Number$",
			baseURL: "https://example.com/sq/$Number$?itag=137",
			seq:     42,
			want:    "https://example.com/sq/42?itag=137",
		},
		{
			name:    "YouTube style without template",
			baseURL: "https://example.com/videoplayback",
			seq:     100,
			want:    "https://example.com/videoplayback/sq/100",
		},
		{
			name:    "trailing slash stripped",
			baseURL: "https://example.com/videoplayback/",
			seq:     0,
			want:    "https://example.com/videoplayback/sq/0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := NewSegmentDownloader(DownloaderOptions{
				BaseURL:    tt.baseURL,
				OutputFile: "test.mp4",
			})
			got := d.buildSegmentURL(tt.seq)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSegmentDownloader_DoubleStart(t *testing.T) {
	tmp := t.TempDir()
	outFile := filepath.Join(tmp, "test.mp4")

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    "https://example.com/sq/$Number$",
		OutputFile: outFile,
		EndSeq:     -1,
	})

	// Manually set running to simulate an active download
	d.mu.Lock()
	d.running = true
	d.mu.Unlock()

	err := d.Start(context.Background())
	if err == nil || err.Error() != "already running" {
		t.Errorf("expected 'already running' error, got %v", err)
	}
}

func TestSegmentDownloader_BytesWritten(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    "https://example.com/sq/$Number$",
		OutputFile: "test.mp4",
	})

	if d.BytesWritten() != 0 {
		t.Errorf("expected 0 bytes initially, got %d", d.BytesWritten())
	}

	d.bytesWritten.Store(12345)
	if d.BytesWritten() != 12345 {
		t.Errorf("expected 12345, got %d", d.BytesWritten())
	}
}

func TestSegmentDownloader_LastSeq(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    "https://example.com/sq/$Number$",
		OutputFile: "test.mp4",
		StartSeq:   10,
	})

	// currentSeq starts at 10, LastSeq = currentSeq - 1 = 9
	if d.LastSeq() != 9 {
		t.Errorf("expected LastSeq=9, got %d", d.LastSeq())
	}
}

func TestTruncateURL(t *testing.T) {
	short := "https://example.com"
	if got := truncateURL(short, 100); got != short {
		t.Errorf("short URL: got %q, want %q", got, short)
	}

	long := "https://example.com/" + string(make([]byte, 200))
	got := truncateURL(long, 50)
	if len(got) != 53 { // 50 + "..."
		t.Errorf("truncated URL length: got %d, want 53", len(got))
	}
}

func TestSleepCtx_ContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	start := time.Now()
	cancel() // Cancel immediately
	sleepCtx(ctx, 10*time.Second)
	elapsed := time.Since(start)
	if elapsed > 1*time.Second {
		t.Errorf("sleepCtx should return quickly on cancelled context, took %v", elapsed)
	}
}

func TestSleepCtx_NormalSleep(t *testing.T) {
	start := time.Now()
	sleepCtx(context.Background(), 50*time.Millisecond)
	elapsed := time.Since(start)
	if elapsed < 40*time.Millisecond {
		t.Errorf("sleepCtx returned too quickly: %v", elapsed)
	}
}

func TestResumeState_LoadSave(t *testing.T) {
	tmp := t.TempDir()
	resumeFile := filepath.Join(tmp, "test.resume.json")
	outFile := filepath.Join(tmp, "test.mp4")

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    "https://example.com/sq/$Number$",
		OutputFile: outFile,
		ResumeFile: resumeFile,
	})

	// Initially no resume file
	state, err := d.loadResume()
	if err == nil {
		t.Error("expected error loading non-existent resume file")
	}
	if state != nil {
		t.Error("expected nil state")
	}

	// Save a state
	d.mu.Lock()
	d.currentSeq = 50
	d.mu.Unlock()
	d.bytesWritten.Store(1000)
	d.saveResume()

	// Load it back
	state, err = d.loadResume()
	if err != nil {
		t.Fatalf("loadResume: %v", err)
	}
	if state.LastSeq != 49 { // currentSeq - 1
		t.Errorf("LastSeq: got %d, want 49", state.LastSeq)
	}
	if state.BytesWritten != 1000 {
		t.Errorf("BytesWritten: got %d, want 1000", state.BytesWritten)
	}
	if state.BaseURL != "https://example.com/sq/$Number$" {
		t.Errorf("BaseURL: got %q", state.BaseURL)
	}

	// Clear resume
	d.ClearResume()
	if _, err := os.Stat(resumeFile); !os.IsNotExist(err) {
		t.Error("resume file should be removed after ClearResume")
	}
}

func TestResumeState_SkipSaveWhenEmpty(t *testing.T) {
	tmp := t.TempDir()
	resumeFile := filepath.Join(tmp, "test.resume.json")

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    "https://example.com/sq/$Number$",
		OutputFile: filepath.Join(tmp, "test.mp4"),
		ResumeFile: resumeFile,
	})

	// currentSeq = 0, should not save
	d.saveResume()

	if _, err := os.Stat(resumeFile); !os.IsNotExist(err) {
		t.Error("resume file should not be created when nothing downloaded")
	}
}

func TestResumeState_CorruptedFile(t *testing.T) {
	tmp := t.TempDir()
	resumeFile := filepath.Join(tmp, "test.resume.json")

	// Write invalid JSON
	if err := os.WriteFile(resumeFile, []byte("not-json"), 0o644); err != nil {
		t.Fatal(err)
	}

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    "https://example.com/sq/$Number$",
		OutputFile: filepath.Join(tmp, "test.mp4"),
		ResumeFile: resumeFile,
	})

	state, err := d.loadResume()
	if err == nil {
		t.Error("expected error for corrupted resume file")
	}
	if state != nil {
		t.Error("expected nil state for corrupted resume file")
	}
}

func TestResumeState_TempFileCleanup(t *testing.T) {
	tmp := t.TempDir()
	resumeFile := filepath.Join(tmp, "test.resume.json")

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    "https://example.com/sq/$Number$",
		OutputFile: filepath.Join(tmp, "test.mp4"),
		ResumeFile: resumeFile,
	})

	d.mu.Lock()
	d.currentSeq = 10
	d.mu.Unlock()
	d.bytesWritten.Store(500)
	d.saveResume()

	// Verify no .tmp file left behind
	tmpFile := resumeFile + ".tmp"
	if _, err := os.Stat(tmpFile); !os.IsNotExist(err) {
		t.Error("temp file should not persist after successful save")
	}

	// Verify resume file exists and is valid JSON
	data, err := os.ReadFile(resumeFile)
	if err != nil {
		t.Fatalf("read resume file: %v", err)
	}
	var state ResumeState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("parse resume file: %v", err)
	}
}

func TestStreamEnded_AtomicAccess(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    "https://example.com/sq/$Number$",
		OutputFile: "test.mp4",
	})

	if d.streamEnded.Load() {
		t.Error("should not be ended initially")
	}

	d.streamEnded.Store(true)
	if !d.streamEnded.Load() {
		t.Error("should be ended after Store(true)")
	}
}
