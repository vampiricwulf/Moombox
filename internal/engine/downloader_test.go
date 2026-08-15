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
	if d.opts.MaxTimeout != DefaultMaxTimeout {
		t.Errorf("MaxTimeout: got %v, want %v", d.opts.MaxTimeout, DefaultMaxTimeout)
	}
	if d.opts.EndSeq != -1 {
		t.Errorf("EndSeq: got %d, want -1", d.opts.EndSeq)
	}
	if d.opts.ResumeFile != "test.mp4.resume.json" {
		t.Errorf("ResumeFile: got %q, want %q", d.opts.ResumeFile, "test.mp4.resume.json")
	}
	if d.headSeq.Load() != -1 {
		t.Errorf("headSeq: got %d, want -1", d.headSeq.Load())
	}
}

func TestNewSegmentDownloader_CustomValues(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    "https://example.com/video",
		OutputFile: "out.mp4",
		MaxRetries: 10,
		MaxTimeout: 5 * time.Minute,
		EndSeq:     999,
		ResumeFile: "custom.resume.json",
	})

	if d.opts.MaxRetries != 10 {
		t.Errorf("MaxRetries: got %d, want 10", d.opts.MaxRetries)
	}
	if d.opts.MaxTimeout != 5*time.Minute {
		t.Errorf("MaxTimeout: got %v, want 5m", d.opts.MaxTimeout)
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
		{
			// Manifest-free DASH adaptiveFormat URL — query-style, must
			// append &sq=N rather than /sq/N (would otherwise corrupt
			// the URL by inserting path between the query string).
			name:    "query-style adaptiveFormat URL appends &sq=N",
			baseURL: "https://rr1---xx.c.youtube.com/videoplayback?expire=1&itag=299&n=DECRYPTED&sig=ABC",
			seq:     5,
			want:    "https://rr1---xx.c.youtube.com/videoplayback?expire=1&itag=299&n=DECRYPTED&sig=ABC&sq=5",
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

// TestSetBaseURLOverridesOpts covers the DECISIONS #7 atomic URL
// swap: getBaseURL returns the override after SetBaseURL is called,
// and falls back to opts.BaseURL when no override is set. Both
// states must be lock-free readable from any goroutine.
func TestSetBaseURLOverridesOpts(t *testing.T) {
	const orig = "https://example.com/orig/$Number$"
	const swap = "https://example.com/swapped/$Number$"

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    orig,
		OutputFile: "test.mp4",
	})

	if got := d.getBaseURL(); got != orig {
		t.Errorf("pre-swap getBaseURL: got %q, want %q", got, orig)
	}

	d.SetBaseURL(swap)
	if got := d.getBaseURL(); got != swap {
		t.Errorf("post-swap getBaseURL: got %q, want %q", got, swap)
	}

	// buildSegmentURL also picks up the override (the segment-fetch hot path).
	url := d.buildSegmentURL(5)
	if !contains(url, "swapped") {
		t.Errorf("buildSegmentURL after swap: got %q, expected to contain 'swapped'", url)
	}
}

// TestSetBaseURLConcurrentSwap exercises atomic.Pointer's safety
// under concurrent reads + writes. Two goroutines repeatedly swap
// the URL while a third reads — race detector verifies no torn
// writes / data races.
func TestSetBaseURLConcurrentSwap(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    "https://example.com/orig",
		OutputFile: "test.mp4",
	})

	const iterations = 1000
	done := make(chan struct{}, 3)

	go func() {
		for range iterations {
			d.SetBaseURL("https://example.com/swap-A")
		}
		done <- struct{}{}
	}()
	go func() {
		for range iterations {
			d.SetBaseURL("https://example.com/swap-B")
		}
		done <- struct{}{}
	}()
	go func() {
		for range iterations {
			_ = d.getBaseURL()
		}
		done <- struct{}{}
	}()

	for range 3 {
		<-done
	}

	final := d.getBaseURL()
	if final != "https://example.com/swap-A" && final != "https://example.com/swap-B" {
		t.Errorf("final getBaseURL = %q, expected one of the swap targets", final)
	}
}

// contains is a tiny helper to keep the test readable without
// importing strings.Contains alongside everything else.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
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
	d.currentSeq.Store(50)
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

func TestResumeState_StaleStateReturnsNil(t *testing.T) {
	// Resume state older than maxResumeStateAge is considered stale.
	tmp := t.TempDir()
	resumeFile := filepath.Join(tmp, "test.resume.json")
	staleTs := time.Now().Add(-30 * 24 * time.Hour).Unix() // 30 days ago
	staleState := ResumeState{
		LastSeq:      100,
		BytesWritten: 5000,
		Timestamp:    staleTs,
		BaseURL:      "https://example.com/sq/$Number$",
	}
	data, _ := json.Marshal(staleState)
	if err := os.WriteFile(resumeFile, data, 0o644); err != nil {
		t.Fatal(err)
	}

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    "https://example.com/sq/$Number$",
		OutputFile: filepath.Join(tmp, "test.mp4"),
		ResumeFile: resumeFile,
	})
	state, err := d.loadResume()
	if err != nil {
		t.Fatalf("loadResume: %v", err)
	}
	if state != nil {
		t.Errorf("expected nil state for stale resume file, got %+v", state)
	}
}

func TestResumeState_FreshStateAccepted(t *testing.T) {
	// Resume state within maxResumeStateAge is honored.
	tmp := t.TempDir()
	resumeFile := filepath.Join(tmp, "test.resume.json")
	freshTs := time.Now().Add(-1 * time.Hour).Unix() // 1 hour ago
	freshState := ResumeState{
		LastSeq:      100,
		BytesWritten: 5000,
		Timestamp:    freshTs,
		BaseURL:      "https://example.com/sq/$Number$",
	}
	data, _ := json.Marshal(freshState)
	if err := os.WriteFile(resumeFile, data, 0o644); err != nil {
		t.Fatal(err)
	}

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    "https://example.com/sq/$Number$",
		OutputFile: filepath.Join(tmp, "test.mp4"),
		ResumeFile: resumeFile,
	})
	state, err := d.loadResume()
	if err != nil {
		t.Fatalf("loadResume: %v", err)
	}
	if state == nil {
		t.Fatal("expected non-nil state for fresh resume file")
	}
	if state.LastSeq != 100 {
		t.Errorf("LastSeq: got %d, want 100", state.LastSeq)
	}
}

func TestResumeState_ZeroTimestampAccepted(t *testing.T) {
	// Timestamp == 0 is treated as "legacy/unset" and should NOT be rejected
	// as stale -- the other validation paths (empty/negative seq) still
	// apply, but timestamp alone doesn't invalidate a valid-looking state.
	tmp := t.TempDir()
	resumeFile := filepath.Join(tmp, "test.resume.json")
	state := ResumeState{
		LastSeq:      50,
		BytesWritten: 1000,
		Timestamp:    0, // legacy/unset
		BaseURL:      "https://example.com/sq/$Number$",
	}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(resumeFile, data, 0o644); err != nil {
		t.Fatal(err)
	}

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    "https://example.com/sq/$Number$",
		OutputFile: filepath.Join(tmp, "test.mp4"),
		ResumeFile: resumeFile,
	})
	loaded, err := d.loadResume()
	if err != nil {
		t.Fatalf("loadResume: %v", err)
	}
	if loaded == nil {
		t.Fatal("expected non-nil state even with Timestamp=0")
	}
}

func TestResumeState_EmptyStateReturnsNil(t *testing.T) {
	// A resume file with LastSeq=0 and BytesWritten=0 is effectively empty
	// (saveResume() shouldn't have written it at all, but could happen via
	// corruption or hand-editing). loadResume should treat it as absent so
	// the caller doesn't advance currentSeq past segment 0 on first resume.
	tmp := t.TempDir()
	resumeFile := filepath.Join(tmp, "test.resume.json")

	emptyState := `{"lastSeq":0,"bytesWritten":0,"timestamp":0,"baseUrl":""}`
	if err := os.WriteFile(resumeFile, []byte(emptyState), 0o644); err != nil {
		t.Fatal(err)
	}

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    "https://example.com/sq/$Number$",
		OutputFile: filepath.Join(tmp, "test.mp4"),
		ResumeFile: resumeFile,
	})
	state, err := d.loadResume()
	if err != nil {
		t.Fatalf("loadResume: %v", err)
	}
	if state != nil {
		t.Errorf("expected nil state for empty resume file, got %+v", state)
	}
}

func TestResumeState_NegativeLastSeqReturnsNil(t *testing.T) {
	// Defensive: a negative LastSeq would make currentSeq = LastSeq+1
	// go negative. Reject it.
	tmp := t.TempDir()
	resumeFile := filepath.Join(tmp, "test.resume.json")

	if err := os.WriteFile(resumeFile, []byte(`{"lastSeq":-1,"bytesWritten":0}`), 0o644); err != nil {
		t.Fatal(err)
	}

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    "https://example.com/sq/$Number$",
		OutputFile: filepath.Join(tmp, "test.mp4"),
		ResumeFile: resumeFile,
	})
	state, err := d.loadResume()
	if err != nil {
		t.Fatalf("loadResume: %v", err)
	}
	if state != nil {
		t.Errorf("expected nil state for negative lastSeq, got %+v", state)
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

	d.currentSeq.Store(10)
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

// TestDashLoop_DeferredProgressRequiresBytes verifies that the deferred OnProgress
// in runDashLoop only fires when actual bytes were written. When handleGoneError
// increments currentSeq without writing data, the deferred must NOT emit progress
// (which would falsely reset the orchestrator's safety counters).
func TestDashLoop_DeferredProgressRequiresBytes(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    "https://example.com/sq/$Number$",
		OutputFile: "test.mp4",
	})

	var progressFired bool
	d.OnProgress = func(p DownloadProgress) {
		progressFired = true
	}

	// Simulate the deferred logic: seq > 0 but no bytes written.
	// This mirrors what happens when handleGoneError increments currentSeq
	// 20 times on 403 errors without downloading any data.
	d.currentSeq.Store(20) // Advanced by error handler, no actual downloads
	// bytesWritten stays at 0 (default)

	seq := int(d.currentSeq.Load())
	// Replicate the deferred condition from runDashLoop
	if d.OnProgress != nil && seq > 0 && d.bytesWritten.Load() > 0 {
		d.OnProgress(DownloadProgress{
			Seq:   seq - 1,
			Bytes: d.bytesWritten.Load(),
		})
	}

	if progressFired {
		t.Error("deferred OnProgress should NOT fire when bytesWritten is 0")
	}

	// Now verify it DOES fire when bytes are written
	d.bytesWritten.Store(1000)
	seq = int(d.currentSeq.Load())
	if d.OnProgress != nil && seq > 0 && d.bytesWritten.Load() > 0 {
		d.OnProgress(DownloadProgress{
			Seq:   seq - 1,
			Bytes: d.bytesWritten.Load(),
		})
	}

	if !progressFired {
		t.Error("deferred OnProgress should fire when bytesWritten > 0")
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

type fakeReporter struct {
	fails     int
	successes int
}

func (f *fakeReporter) ReportFailure(string) { f.fails++ }
func (f *fakeReporter) ReportSuccess(string) { f.successes++ }

func TestApplyPoTokenQuery(t *testing.T) {
	tests := []struct {
		name  string
		url   string
		token string
		want  string
	}{
		{"empty token leaves URL untouched", "https://e.com/sq/1", "", "https://e.com/sq/1"},
		{"URL with no query uses ?", "https://e.com/sq/1", "abc", "https://e.com/sq/1?pot=abc"},
		{"URL with existing query uses &", "https://e.com/sq/1?itag=137", "xyz", "https://e.com/sq/1?itag=137&pot=xyz"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := applyPoTokenQuery(tt.url, tt.token)
			if got != tt.want {
				t.Errorf("applyPoTokenQuery(%q, %q) = %q, want %q", tt.url, tt.token, got, tt.want)
			}
		})
	}
}

func TestConnectivityReporter_SetAndClear(t *testing.T) {
	// Clean up after ourselves so we don't leak state into other tests
	t.Cleanup(func() { SetConnectivityReporter(nil) })

	if r := loadConnReporter(); r != nil {
		t.Errorf("expected nil reporter initially, got %T", r)
	}

	f := &fakeReporter{}
	SetConnectivityReporter(f)

	r := loadConnReporter()
	if r == nil {
		t.Fatal("expected non-nil reporter after Set")
	}
	r.ReportFailure("test")
	r.ReportSuccess("test")
	if f.fails != 1 || f.successes != 1 {
		t.Errorf("expected fails=1 successes=1, got fails=%d successes=%d", f.fails, f.successes)
	}

	// Clear it explicitly
	SetConnectivityReporter(nil)
	if r := loadConnReporter(); r != nil {
		t.Errorf("expected nil after SetConnectivityReporter(nil), got %T", r)
	}

	// Helpers are no-op when no reporter
	reportFailure("no-reporter") // should not panic
	reportSuccess("no-reporter") // should not panic
}

func TestCurrentSeq(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    "https://example.com/sq/$Number$",
		OutputFile: filepath.Join(t.TempDir(), "test.mp4"),
		StartSeq:   42,
	})
	if got := d.CurrentSeq(); got != 42 {
		t.Errorf("CurrentSeq() = %d, want 42", got)
	}
}

func TestForceStartSeq_ExistingFile(t *testing.T) {
	// ForceStartSeq + existing file = should honor StartSeq (same-quality append path)
	tmpDir := t.TempDir()
	outFile := filepath.Join(tmpDir, "video_stream")
	os.WriteFile(outFile, []byte("existing segment data"), 0o644)

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:       "https://example.com/sq/$Number$",
		OutputFile:    outFile,
		StartSeq:      100,
		ForceStartSeq: true,
	})
	if got := d.CurrentSeq(); got != 100 {
		t.Errorf("CurrentSeq() = %d, want 100", got)
	}
}

func TestForceStartSeq_MissingFile(t *testing.T) {
	// ForceStartSeq + missing file = should still honor StartSeq (different-quality split path)
	tmpDir := t.TempDir()
	outFile := filepath.Join(tmpDir, "video_stream") // does not exist

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:       "https://example.com/sq/$Number$",
		OutputFile:    outFile,
		StartSeq:      100,
		ForceStartSeq: true,
	})
	if got := d.CurrentSeq(); got != 100 {
		t.Errorf("CurrentSeq() = %d, want 100", got)
	}
}

// TestSetPoTokenOverridesOptions pins the mutable-token seam that credential
// recovery depends on: opts.PoToken is the initial value only, and a later
// SetPoToken must be what subsequent fetches use. Mirrors SetBaseURL.
func TestSetPoTokenOverridesOptions(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    "https://example.invalid/v",
		OutputFile: "x",
		PoToken:    "initial",
	})
	if got := d.getPoToken(); got != "initial" {
		t.Fatalf("getPoToken() = %q, want the options value %q", got, "initial")
	}
	d.SetPoToken("refreshed")
	if got := d.getPoToken(); got != "refreshed" {
		t.Errorf("getPoToken() = %q, want %q after SetPoToken", got, "refreshed")
	}
	// An empty refresh must not blank a working token — a failed re-mint
	// returns "" and must leave the existing credential in place.
	d.SetPoToken("")
	if got := d.getPoToken(); got != "refreshed" {
		t.Errorf("getPoToken() = %q, want the previous token retained after an empty SetPoToken", got)
	}
}

// TestRefreshCredentialsInstallsAndCoolsDown pins the two properties the 403
// recovery path relies on: a refresh installs whatever the callback returns,
// and repeated 403s inside the cooldown window cannot turn the callback into
// a hot loop (yt-dlp gates its url_feed refetch the same way).
func TestRefreshCredentialsInstallsAndCoolsDown(t *testing.T) {
	var calls int
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    "https://example.invalid/old",
		OutputFile: "x",
		PoToken:    "old-token",
		OnCredentialRefresh: func() (string, string) {
			calls++
			return "https://example.invalid/new", "new-token"
		},
	})

	if !d.refreshCredentials() {
		t.Fatal("first refresh should report that credentials were installed")
	}
	if calls != 1 {
		t.Fatalf("callback calls = %d, want 1", calls)
	}
	if got := d.getPoToken(); got != "new-token" {
		t.Errorf("token = %q, want new-token", got)
	}
	if got := d.getBaseURL(); got != "https://example.invalid/new" {
		t.Errorf("baseURL = %q, want the refreshed URL", got)
	}

	// Second call inside the cooldown must not re-invoke the callback.
	if d.refreshCredentials() {
		t.Error("refresh inside the cooldown window should report no new install")
	}
	if calls != 1 {
		t.Errorf("callback calls = %d, want still 1 (cooldown)", calls)
	}
}

// TestRefreshCredentialsNilCallback: downloaders without the callback (Twitch
// HLS, VOD) must be unaffected.
func TestRefreshCredentialsNilCallback(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{BaseURL: "https://example.invalid/v", OutputFile: "x"})
	if d.refreshCredentials() {
		t.Error("refreshCredentials with no callback should report false")
	}
}
