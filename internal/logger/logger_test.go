package logger

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewLogger(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")

	l, err := New(logPath, "DEBUG", 1024*1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	l.Info("test message")
	l.Debug("debug message", "key", "value")

	// Verify file contains the actual messages we logged
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "test message") {
		t.Errorf("expected log file to contain 'test message', got:\n%s", content)
	}
	if !strings.Contains(content, "debug message") {
		t.Errorf("expected log file to contain 'debug message', got:\n%s", content)
	}
	if !strings.Contains(content, "key=value") {
		t.Errorf("expected log file to contain 'key=value', got:\n%s", content)
	}
}

func TestRingBuffer(t *testing.T) {
	l, err := New("", "DEBUG", 1024*1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// Fill ring buffer past capacity
	for i := range 250 {
		l.Info("message", "i", i)
	}

	lines := l.GetRecentLines()
	if len(lines) != defaultRingSize {
		t.Errorf("expected %d lines, got %d", defaultRingSize, len(lines))
	}
}

func TestRingBufferPartialFill(t *testing.T) {
	l, err := New("", "DEBUG", 1024*1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// Log fewer messages than ring size
	for i := range 5 {
		l.Info("partial", "i", i)
	}

	lines := l.GetRecentLines()
	if len(lines) != 5 {
		t.Errorf("expected 5 lines, got %d", len(lines))
	}
}

func TestRingBufferOrder(t *testing.T) {
	l, err := New("", "DEBUG", 1024*1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// Overfill and check that we get the most recent entries in order
	for i := range 210 {
		l.Info(fmt.Sprintf("msg-%d", i))
	}

	lines := l.GetRecentLines()
	if len(lines) != defaultRingSize {
		t.Errorf("expected %d lines, got %d", defaultRingSize, len(lines))
	}

	// First line should be msg-10 (oldest retained), last should be msg-209
	if !strings.Contains(lines[0], "msg-10") {
		t.Errorf("expected first line to contain 'msg-10', got %q", lines[0])
	}
	if !strings.Contains(lines[len(lines)-1], "msg-209") {
		t.Errorf("expected last line to contain 'msg-209', got %q", lines[len(lines)-1])
	}
}

func TestSubscribe(t *testing.T) {
	l, err := New("", "DEBUG", 1024*1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	ch := l.Subscribe()

	go func() {
		time.Sleep(10 * time.Millisecond)
		l.Info("hello subscriber")
	}()

	select {
	case line := <-ch:
		if !strings.Contains(line, "hello subscriber") {
			t.Errorf("expected message to contain 'hello subscriber', got %s", line)
		}
	case <-time.After(time.Second):
		t.Error("timeout waiting for log message")
	}

	l.Unsubscribe(ch)
}

func TestUnsubscribeNonExistent(t *testing.T) {
	l, err := New("", "DEBUG", 1024*1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// Should not panic when unsubscribing a channel that was never subscribed
	ch := make(chan string, 1)
	l.Unsubscribe(ch) // no-op, should not panic
}

func TestSetLevel(t *testing.T) {
	l, err := New("", "DEBUG", 1024*1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// Log a debug message at DEBUG level — should reach the ring buffer
	l.Debug("visible debug message")
	lines := l.GetRecentLines()
	foundVisible := false
	for _, line := range lines {
		if strings.Contains(line, "visible debug message") {
			foundVisible = true
			break
		}
	}
	if !foundVisible {
		t.Error("expected 'visible debug message' in ring buffer at DEBUG level")
	}

	// Raise level to ERROR — debug messages should be filtered before ring buffer
	l.SetLevel("ERROR")
	countBefore := len(l.GetRecentLines())
	l.Debug("should not appear")
	l.Info("also filtered")
	l.Warn("also filtered")
	countAfter := len(l.GetRecentLines())
	if countAfter != countBefore {
		t.Errorf("expected ring buffer count unchanged after filtered messages, got %d -> %d",
			countBefore, countAfter)
	}

	// ERROR messages should still reach the ring buffer
	l.Error("visible error message")
	lines = l.GetRecentLines()
	foundError := false
	for _, line := range lines {
		if strings.Contains(line, "visible error message") {
			foundError = true
			break
		}
	}
	if !foundError {
		t.Error("expected 'visible error message' in ring buffer at ERROR level")
	}
}

func TestSetLevelValidLevels(t *testing.T) {
	l, err := New("", "INFO", 1024*1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	tests := []struct {
		level string
		valid bool
	}{
		{"DEBUG", true},
		{"INFO", true},
		{"WARN", true},
		{"WARNING", true},
		{"ERROR", true},
		{"debug", true}, // case insensitive
		{"Info", true},  // case insensitive
		{"INVALID", false},
		{"", false},
		{"TRACE", false},
	}

	for _, tt := range tests {
		ok := l.SetLevel(tt.level)
		if ok != tt.valid {
			t.Errorf("SetLevel(%q) returned %v, expected %v", tt.level, ok, tt.valid)
		}
	}
}

func TestLogRotation(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "rotate.log")

	// Use the minimum allowed rotation size so rotation triggers after
	// just a few log lines without bypassing New's clamp.
	l, err := New(logPath, "DEBUG", minLogRotationSize, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// Each log line is ~100 bytes, so ~50 lines is enough to exceed the 4 KiB
	// threshold and force rotation.
	for i := range 60 {
		l.Info("rotation test message that is long enough to exceed the limit", "i", i)
	}

	// Check that rotated files exist
	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		t.Error("expected current log file to exist")
	}
	if _, err := os.Stat(logPath + ".1"); os.IsNotExist(err) {
		t.Error("expected rotated log file .1 to exist")
	}
}

// TestNewClampsSmallMaxSize verifies New clamps unreasonably small maxSize
// values up to the documented minimum. Regression for audit
// reports/small-packages.md logger.go:130-147.
func TestNewClampsSmallMaxSize(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "clamp.log")

	l, err := New(logPath, "DEBUG", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	if l.maxSize < minLogRotationSize {
		t.Errorf("maxSize=%d, want >= %d", l.maxSize, minLogRotationSize)
	}
	if l.maxFiles < 1 {
		t.Errorf("maxFiles=%d, want >= 1", l.maxFiles)
	}
}

func TestSuppressRestoreStdout(t *testing.T) {
	l, err := New("", "DEBUG", 1024*1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	if !l.stdout.enabled.Load() {
		t.Error("expected stdout enabled by default")
	}

	l.SuppressStdout()
	if l.stdout.enabled.Load() {
		t.Error("expected stdout disabled after SuppressStdout")
	}

	l.RestoreStdout()
	if !l.stdout.enabled.Load() {
		t.Error("expected stdout enabled after RestoreStdout")
	}
}

func TestCloseClosesSubscribers(t *testing.T) {
	l, err := New("", "DEBUG", 1024*1024, 3)
	if err != nil {
		t.Fatal(err)
	}

	ch := l.Subscribe()
	l.Close()

	// Channel should be closed after Close
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("expected channel to be closed")
		}
	case <-time.After(time.Second):
		t.Error("timeout waiting for channel close")
	}
}

func TestFormatLogLineMissingValue(t *testing.T) {
	// Odd number of args should produce "key=!MISSING"
	line := formatLogLine(time.Now(), slog.LevelInfo, "test", "orphan_key")
	if !strings.Contains(line, "orphan_key=!MISSING") {
		t.Errorf("expected '!MISSING' marker for unpaired key, got %q", line)
	}
}

func TestFormatLogLineSlogAttr(t *testing.T) {
	// slog.Attr values should be handled as self-contained key=value pairs
	line := formatLogLine(time.Now(), slog.LevelInfo, "test", slog.String("key1", "val1"), slog.Int("key2", 42))
	if !strings.Contains(line, "key1=val1") {
		t.Errorf("expected 'key1=val1', got %q", line)
	}
	if !strings.Contains(line, "key2=42") {
		t.Errorf("expected 'key2=42', got %q", line)
	}
	if strings.Contains(line, "!MISSING") {
		t.Errorf("unexpected '!MISSING' marker for slog.Attr args, got %q", line)
	}

	// Mixed: slog.Attr + plain key-value pairs
	line2 := formatLogLine(time.Now(), slog.LevelWarn, "mixed", slog.String("attr", "val"), "plain_key", "plain_val")
	if !strings.Contains(line2, "attr=val") {
		t.Errorf("expected 'attr=val', got %q", line2)
	}
	if !strings.Contains(line2, "plain_key=plain_val") {
		t.Errorf("expected 'plain_key=plain_val', got %q", line2)
	}
	if strings.Contains(line2, "!MISSING") {
		t.Errorf("unexpected '!MISSING' in mixed args, got %q", line2)
	}
}

// TestWriteReopensAfterFileNil locks the rotate-reopen-failure
// recovery: if rotate previously failed to reopen the log file
// (transient ENOSPC, AV briefly holding the file, etc.), the next
// Write must retry openFile so logging recovers automatically rather
// than silently dropping every line until restart.
//
// Simulates the failure mode by manually setting l.file = nil after
// a successful start, then asserting the next Write reopens and
// produces a non-zero byte count. Audit reports/small-packages.md.
func TestWriteReopensAfterFileNil(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")

	l, err := New(logPath, "DEBUG", 1024*1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// Confirm initial state — file is open.
	l.fileMu.Lock()
	hadFile := l.file != nil
	l.fileMu.Unlock()
	if !hadFile {
		t.Fatal("setup: l.file should be non-nil after New()")
	}

	// Simulate a rotate-failed-to-reopen state by closing + nilling
	// the file directly. Next Write should retry openFile.
	l.fileMu.Lock()
	l.file.Close()
	l.file = nil
	l.fileMu.Unlock()

	n, err := l.Write([]byte("recovered after reopen\n"))
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if n == 0 {
		t.Error("Write returned n=0; expected reopen-and-write recovery")
	}

	// Verify the file is now open again.
	l.fileMu.Lock()
	hasFile := l.file != nil
	l.fileMu.Unlock()
	if !hasFile {
		t.Error("l.file should be non-nil after reopen-on-write")
	}
}

// TestFormatLogLinePoolDoesNotCorruptStrings locks the alias-break in
// formatLogLine: strings.Builder.String() shares the buffer with the
// builder. Pooling without strings.Clone would let two concurrent
// callers see each other's bytes if their builders happened to be the
// same pooled instance across resets. Run many concurrent formats and
// confirm each result matches its inputs.
func TestFormatLogLinePoolDoesNotCorruptStrings(t *testing.T) {
	const n = 200
	results := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			// Distinct message + key=value per goroutine — no two
			// callers should see the same output.
			results[idx] = formatLogLine(time.Now(), slog.LevelInfo,
				fmt.Sprintf("msg_%d", idx),
				fmt.Sprintf("k%d", idx), fmt.Sprintf("v%d", idx))
		}(i)
	}
	wg.Wait()

	for i, line := range results {
		expectMsg := fmt.Sprintf("msg_%d", i)
		expectKV := fmt.Sprintf("k%d=v%d", i, i)
		if !strings.Contains(line, expectMsg) {
			t.Errorf("[%d] line missing %q (corruption?): %q", i, expectMsg, line)
		}
		if !strings.Contains(line, expectKV) {
			t.Errorf("[%d] line missing %q (corruption?): %q", i, expectKV, line)
		}
	}
}

func TestLogAfterClose(t *testing.T) {
	l, err := New("", "DEBUG", 1024*1024, 3)
	if err != nil {
		t.Fatal(err)
	}

	l.Close()

	// Should not panic when logging after close
	l.Info("after close")
	l.Debug("after close debug")
	l.Warn("after close warn")
	l.Error("after close error")
}

func TestNewLoggerNoFile(t *testing.T) {
	// Empty path means no file output
	l, err := New("", "INFO", 1024*1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	if l.file != nil {
		t.Error("expected nil file when path is empty")
	}

	// Should still work for ring buffer and subscribers
	l.Info("no file test")
	lines := l.GetRecentLines()
	if len(lines) != 1 {
		t.Errorf("expected 1 line in ring buffer, got %d", len(lines))
	}
}

// --- CORE-3 / CORE-22 (sweep-2 Arc C Task 10) ---

// goldenNow is the instant the format tests pin the clock to. It renders as
// "2026-09-17 12:00:00" in both wire shapes.
var goldenNow = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

// A rotation that cannot rename must back off. On Windows any process
// holding <log>.1 open makes both renames fail, and because rotate() was
// re-attempted after EVERY write past the cap, the failure repeated per
// line: ~1 WARN diagnostic per log line into the ring buffer, every WS
// subscriber and the TUI log panel, while the live file grew unbounded
// (CORE-3; reproduced at 94 diagnostics per 50 writes).
//
// The fixture is portable: renaming a file onto a NON-EMPTY directory fails
// on Windows and POSIX alike, and maxFiles=1 keeps the shift loop out of it.
// SuppressStdout is what the TUI does, and is what routes diagf into the
// ring buffer and the subscribers instead of stderr — the storm's path.
//
// Mutant: calling rotate() unconditionally from Write again — dozens of
// diagnostics instead of one.
func TestRotationBacksOffAfterAFailure(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "backoff.log")

	blocker := logPath + ".1"
	if err := os.MkdirAll(blocker, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocker, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	l, err := New(logPath, "DEBUG", minLogRotationSize, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	l.SuppressStdout()

	sub := l.Subscribe()

	done := make(chan int, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("subscriber goroutine panicked: %v", r)
			}
		}()
		n := 0
		for line := range sub {
			if strings.Contains(line, "logger: rotation") {
				n++
			}
		}
		done <- n
	}()

	for i := range 60 {
		l.Info("a line long enough to push the file past the rotation floor quickly", "i", i, "pad", strings.Repeat("p", 200))
	}
	// Close (not Unsubscribe) ends the range: Unsubscribe deliberately does
	// not close the channel — see its godoc.
	l.Close()
	diagnostics := <-done

	if diagnostics > 1 {
		t.Errorf("%d rotation diagnostics for 60 writes; the back-off must emit one per streak", diagnostics)
	}
	if diagnostics == 0 {
		t.Error("the first failed rotation must still report once — silence is not the fix")
	}
}

// The back-off attempts the rotation at most once per window, reports at most
// once per failure STREAK, never drops a line, and resets when a rotation
// finally succeeds (CORE-3). The Windows shape — another process holding the
// rotation target open so both renames fail — is simulated through the
// renameFile seam, so no real handle is held and nothing can leak.
//
// Mutants: rotating unconditionally from Write (renames climb with the write
// count); dropping the firstOfStreak guard (a diagnostic per attempt);
// returning early from Write while backing off (lines go missing); never
// clearing rotateFailing (no diagnostic after the success).
func TestRotationBackoffRetriesOncePerWindowAndResetsOnSuccess(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "seam.log")

	clock := goldenNow
	renames := 0
	renameFails := true
	// The seams go in through New, before the logger is published to
	// slog.SetDefault; only this goroutine drives the closures.
	l, err := New(logPath, "DEBUG", minLogRotationSize, 1,
		WithClock(func() time.Time { return clock }),
		WithRename(func(oldpath, newpath string) error {
			renames++
			if renameFails {
				return errors.New("simulated: another process holds the file open")
			}
			return os.Rename(oldpath, newpath)
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	l.SuppressStdout()

	write := func(tag string) {
		for i := range 30 {
			l.Info("a line long enough to push the file past the floor quickly", "tag", tag, "i", i, "pad", strings.Repeat("p", 200))
		}
	}
	diagnostics := func() int {
		n := 0
		for _, line := range l.GetRecentLines() {
			if strings.Contains(line, "logger: rotation") {
				n++
			}
		}
		return n
	}

	write("w1")
	if renames != 1 {
		t.Errorf("the first window attempted %d renames, want 1 — the back-off must not retry per write", renames)
	}
	if got := diagnostics(); got != 1 {
		t.Errorf("the first window reported %d diagnostics, want 1", got)
	}

	// Still inside the same window: no new attempt, no new diagnostic.
	clock = clock.Add(rotateBackoff / 2)
	write("w2")
	if renames != 1 {
		t.Errorf("%d renames after a second write burst inside the window, want 1", renames)
	}
	if got := diagnostics(); got != 1 {
		t.Errorf("%d diagnostics inside the window, want 1", got)
	}

	// The window expires: exactly one more attempt, and the streak stays
	// quiet — the holder, the handle and the message have not changed.
	clock = clock.Add(rotateBackoff + time.Second)
	write("w3")
	if renames != 2 {
		t.Errorf("%d renames after the window expired, want 2 (one attempt per window)", renames)
	}
	if got := diagnostics(); got != 1 {
		t.Errorf("%d diagnostics across two windows, want 1 (one per streak)", got)
	}

	// Not one line was dropped while backing off: all 90 are in the live
	// file, which is exactly why the file grows past the cap for a while.
	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"w1", "w2", "w3"} {
		if got := strings.Count(string(body), "tag="+tag+" "); got != 30 {
			t.Errorf("the live file holds %d of 30 %s lines — backing off must never drop a line", got, tag)
		}
	}

	// The holder lets go: the rotation succeeds and the streak resets.
	renameFails = false
	clock = clock.Add(rotateBackoff + time.Second)
	write("w4")
	if _, err := os.Stat(logPath + ".1"); err != nil {
		t.Errorf("the rotation must succeed once the rename does: %v", err)
	}
	if got := diagnostics(); got != 1 {
		t.Errorf("%d diagnostics after a successful rotation, want the same 1", got)
	}

	// A fresh failure is a fresh streak, so it reports again.
	renameFails = true
	before := renames
	clock = clock.Add(rotateBackoff + time.Second)
	write("w5")
	if got := renames - before; got != 1 {
		t.Errorf("the new streak attempted %d renames, want 1", got)
	}
	if got := diagnostics(); got != 2 {
		t.Errorf("%d diagnostics after the streak reset, want 2 — a success must re-arm the report", got)
	}
}

// The file line and the UI line are formatted from ONE clock sample. Before
// this, slog's TextHandler formatted its own copy from the record's time
// while formatLogLine sampled time.Now() again, so the two could disagree
// about the second (CORE-22). The clock here advances a full second per read,
// which turns any second sample into a visible disagreement.
//
// Mutant: reinstating a second sample (formatLogLine(l.now(), …) at the call
// site, or slog.Logger.Log, which stamps the record with time.Now()) — the
// file and the ring differ.
func TestFileAndRingLinesShareOneClockSample(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "once.log")
	ticks := 0
	l, err := New(logPath, "DEBUG", 1024*1024, 3, WithClock(func() time.Time {
		ticks++
		return goldenNow.Add(time.Duration(ticks) * time.Second)
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	l.SuppressStdout()

	l.Info("single format", "key", "value")

	lines := l.GetRecentLines()
	if len(lines) != 1 {
		t.Fatalf("ring holds %d lines, want 1", len(lines))
	}
	if ticks != 1 {
		t.Errorf("one log call sampled the clock %d times, want 1", ticks)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`time="([^"]+)"`).FindStringSubmatch(string(data))
	if m == nil {
		t.Fatalf("no time attr in the file line: %q", data)
	}
	if ring := lines[0][:len(m[1])]; ring != m[1] {
		t.Errorf("the file line and the UI line must carry the same instant.\nfile: %q\nring: %q", m[1], ring)
	}
}

// goldenFixture is the record set the byte-identity differential runs: every
// level, every attr kind slog can carry (including a group, an error, a nil
// and an odd trailing key), a multi-line message with a tab, non-ASCII, an
// empty message, and an attr whose key collides with slog's own time key —
// the case ReplaceAttr's Kind check exists for.
func goldenFixture(l *Logger) {
	l.Debug("debug with every scalar kind",
		"str", "plain",
		"quoted", "needs quoting",
		"int", 42,
		"int64", int64(-9000000000),
		"uint64", uint64(18446744073709551615),
		"float", 3.5,
		"bool", true,
		"dur", 1500*time.Millisecond,
		"ts", time.Date(2021, 3, 4, 5, 6, 7, 800000000, time.UTC),
		"err", errors.New("boom: it failed"),
		"nothing", nil,
	)
	l.Info("info with slog.Attr args", slog.String("a", "b"), slog.Int("n", 7),
		slog.Group("g", slog.String("inner", "v"), slog.Bool("ok", false)))
	l.Warn("warn with a multi-line message\nsecond line\tand a tab", "k", "v")
	l.Error("error with non-ASCII: café ☕ 日本語 — em dash", "emoji", "🎬")
	l.Info("unpaired trailing key", "orphan")
	l.Info("an attr key that collides with slog's own time key", "time", "12:30")
	l.Info("")
}

// goldenFileBytes is moombox.log's on-disk shape, captured from the
// pre-change logger (the same fixture, its timestamps pinned). The file
// format is PROTECTED: CORE-22 removes the second clock sample and slog's
// throwaway caller lookup, and NOTHING about the bytes.
const goldenFileBytes = `time="2026-09-17 12:00:00" level=DEBUG msg="debug with every scalar kind" str=plain quoted="needs quoting" int=42 int64=-9000000000 uint64=18446744073709551615 float=3.5 bool=true dur=1.5s ts=2021-03-04T05:06:07.800Z err="boom: it failed" nothing=<nil>
time="2026-09-17 12:00:00" level=INFO msg="info with slog.Attr args" a=b n=7 g.inner=v g.ok=false
time="2026-09-17 12:00:00" level=WARN msg="warn with a multi-line message\nsecond line\tand a tab" k=v
time="2026-09-17 12:00:00" level=ERROR msg="error with non-ASCII: café ☕ 日本語 — em dash" emoji=🎬
time="2026-09-17 12:00:00" level=INFO msg="unpaired trailing key" !BADKEY=orphan
time="2026-09-17 12:00:00" level=INFO msg="an attr key that collides with slog's own time key" time=12:30
time="2026-09-17 12:00:00" level=INFO msg=""
`

// goldenRingLines is the ring-buffer / subscriber shape — the TUI log panel's
// and the dashboard's line — for the SAME fixture, entries joined by "---".
// It is deliberately NOT the file's shape and never was: no time=/level=/msg=
// keys, no quoting, a real newline in the multi-line message, "!MISSING"
// where the file says "!BADKEY". Both shapes are pinned byte for byte; what
// CORE-22 changes is that they now carry the same instant.
const goldenRingLines = `2026-09-17 12:00:00 DEBUG debug with every scalar kind str=plain quoted=needs quoting int=42 int64=-9000000000 uint64=18446744073709551615 float=3.5 bool=true dur=1.5s ts=2021-03-04 05:06:07.8 +0000 UTC err=boom: it failed nothing=<nil>
---
2026-09-17 12:00:00 INFO info with slog.Attr args a=b n=7 g=[inner=v ok=false]
---
2026-09-17 12:00:00 WARN warn with a multi-line message
second line	and a tab k=v
---
2026-09-17 12:00:00 ERROR error with non-ASCII: café ☕ 日本語 — em dash emoji=🎬
---
2026-09-17 12:00:00 INFO unpaired trailing key orphan=!MISSING
---
2026-09-17 12:00:00 INFO an attr key that collides with slog's own time key time=12:30
---
2026-09-17 12:00:00 INFO `

// TestLogWireFormatsAreByteIdentical is the differential C10 requires: both
// formatted shapes, over the whole fixture, with the clock pinned. The
// goldens were captured from the pre-change logger, so any byte that moves —
// in the file OR in the line the TUI and the dashboard render — fails here.
//
// Mutant: writing formatLogLine's output to the file (the one-pass shortcut)
// — every file byte changes.
func TestLogWireFormatsAreByteIdentical(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "golden.log")
	l, err := New(logPath, "DEBUG", 1024*1024, 3, WithClock(func() time.Time { return goldenNow }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	l.SuppressStdout()

	goldenFixture(l)

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != goldenFileBytes {
		t.Errorf("the on-disk log format changed.\n got: %q\nwant: %q", data, goldenFileBytes)
	}
	if got := strings.Join(l.GetRecentLines(), "\n---\n"); got != goldenRingLines {
		t.Errorf("the ring-buffer/subscriber format changed.\n got: %q\nwant: %q", got, goldenRingLines)
	}
}

// A rotation failure that persists must keep saying so — once an hour, never
// once per attempt and never once per write. Measured on the previous commit,
// a rename failing for 48 h produced exactly ONE diagnostic across 2,879
// attempts while the live file reached 2,135× the cap; that line goes to
// stderr (so it is never in moombox.log) or, in TUI mode, into a 200-entry
// ring a busy instance recycles within minutes, so an operator who misses it
// sees only a file that will not stop growing (B-2).
//
// The payload is written through Write — the io.Writer the handler uses for
// the file — so the ring buffer holds the rotation diagnostics and nothing
// else and they can be counted exactly.
//
// Mutants: no reminder at all (1 diagnostic in 48 h); a reminder per attempt
// (2,879); the streak's report clock never advanced (a reminder per attempt
// again); the reminder emitted after a successful rotation too (the second
// phase's count rises).
func TestRotationFailureRemindsHourly(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "remind.log")

	clock := goldenNow
	renameFails := true
	l, err := New(logPath, "DEBUG", minLogRotationSize, 1,
		WithClock(func() time.Time { return clock }),
		WithRename(func(oldpath, newpath string) error {
			if renameFails {
				return errors.New("simulated: another process holds the file open")
			}
			return os.Rename(oldpath, newpath)
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	l.SuppressStdout() // what the TUI does — diagf then lands in the ring

	payload := []byte(strings.Repeat("p", 300) + "\n")
	minute := func() {
		t.Helper()
		clock = clock.Add(time.Minute)
		if _, err := l.Write(payload); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	count := func() (details, reminders int, last string) {
		for _, line := range l.GetRecentLines() {
			switch {
			case strings.Contains(line, "logger: rotation still blocked"):
				reminders++
				last = line
			case strings.Contains(line, "logger: rotation"):
				details++
			}
		}
		return details, reminders, last
	}

	const hours = 48
	for range hours * 60 {
		minute()
	}

	details, reminders, last := count()
	if details != 1 {
		t.Errorf("%d detail diagnostics in %d h, want 1 (one per streak)", details, hours)
	}
	if reminders < hours-3 || reminders > hours {
		t.Errorf("%d reminders in %d h; want roughly one an hour — never one per attempt (2,879) and never zero", reminders, hours)
	}
	// The reminder carries what the first line cannot: how long the streak
	// has run and how far past the cap the live file is.
	for _, want := range []string{"is now", "bytes (cap"} {
		if !strings.Contains(last, want) {
			t.Errorf("the reminder must name the file's size; %q has no %q", last, want)
		}
	}

	// Not one line was dropped while backing off — the file holds all of them.
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(len(payload)) * hours * 60; info.Size() != want {
		t.Errorf("the live file holds %d bytes, want %d — backing off must never drop a write", info.Size(), want)
	}

	// The holder lets go: rotations succeed again and the reminders stop.
	renameFails = false
	for range 3 * 60 {
		minute()
	}
	if _, after, _ := count(); after != reminders {
		t.Errorf("%d reminders after the rotation started succeeding, want the same %d", after, reminders)
	}
}

// TestFailingStdoutNeverCostsTheFileALine is W24-10: once stdout stops taking
// writes — a hung-up SSH tty after `moombox --headless & disown` (EIO), a
// process started with fd 1 closed (EBADF), a console-less Windows child —
// moombox.log must keep receiving every line. The stdout sink sat FIRST in an
// io.MultiWriter, which returns at the first writer's error, and the
// switchable writer passed os.Stdout's error on unchanged: the file stopped
// getting lines for the rest of the run while the ring buffer looked normal.
// A pipe whose reader went away is the Unix shape that kills the process
// instead (brokenpipe_unix_test.go).
//
// New captures os.Stdout at construction, as production does, so the test
// swaps it first. A closed *os.File fails every write with os.ErrClosed on
// every platform. The first phase is the mid-run shape (stdout works, then
// dies); the second a stdout that was dead from the start.
//
// Mutant: io.MultiWriter(l.stdout, l) in place of lineSinks again — both
// phases lose their lines from the file. lineSinks guards it twice over (file
// first, no early return), so each half has its own test below.
func TestFailingStdoutNeverCostsTheFileALine(t *testing.T) {
	swapStdout := func(f *os.File) {
		t.Helper()
		orig := os.Stdout
		os.Stdout = f
		t.Cleanup(func() { os.Stdout = orig })
	}
	dir := t.TempDir()

	t.Run("stdout dies mid-run", func(t *testing.T) {
		stdout, err := os.Create(filepath.Join(dir, "stdout-mid"))
		if err != nil {
			t.Fatal(err)
		}
		swapStdout(stdout)
		logPath := filepath.Join(dir, "mid.log")
		l, err := New(logPath, "INFO", 1<<20, 3)
		if err != nil {
			t.Fatal(err)
		}
		l.Info("while stdout works")
		stdout.Close() // the hang-up
		l.Info("after stdout died", "n", 1)
		l.Warn("after stdout died", "n", 2)
		l.Close()

		data, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"while stdout works", "n=1", "n=2"} {
			if !strings.Contains(string(data), want) {
				t.Errorf("moombox.log is missing %q after stdout failed:\n%s", want, data)
			}
		}
		// Control: stdout itself did take the line written while it worked.
		echoed, err := os.ReadFile(stdout.Name())
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(echoed), "while stdout works") {
			t.Errorf("stdout never took the line written while it worked: %q", echoed)
		}
	})

	t.Run("stdout dead from the start", func(t *testing.T) {
		stdout, err := os.Create(filepath.Join(dir, "stdout-dead"))
		if err != nil {
			t.Fatal(err)
		}
		stdout.Close()
		swapStdout(stdout)
		logPath := filepath.Join(dir, "dead.log")
		l, err := New(logPath, "INFO", 1<<20, 3)
		if err != nil {
			t.Fatal(err)
		}
		l.Info("first line")
		l.Error("last line")
		l.Close()

		data, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"first line", "last line"} {
			if !strings.Contains(string(data), want) {
				t.Errorf("moombox.log is missing %q with stdout closed:\n%s", want, data)
			}
		}
	})
}

// sinkProbe is an io.Writer standing in for a sink, for the lineSinks tests.
type sinkProbe func(p []byte) (int, error)

func (f sinkProbe) Write(p []byte) (int, error) { return f(p) }

// TestTheFileHasTheLineBeforeStdoutIsTouched: lineSinks writes moombox.log
// first, so a stdout write that never returns — a pipe whose reader stopped
// reading, a console paused with Ctrl+S — or that takes the process down with
// it cannot take the line it was handed along.
//
// Mutant: lineSinks.Write writing stdout ahead of the file — the probe finds
// the line not yet on disk.
func TestTheFileHasTheLineBeforeStdoutIsTouched(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "moombox.log")
	l, err := New(logPath, "INFO", 1<<20, 3)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk []bool
	l.stdout.w = sinkProbe(func(p []byte) (int, error) {
		data, err := os.ReadFile(logPath)
		onDisk = append(onDisk, err == nil && strings.Contains(string(data), string(p)))
		return len(p), nil
	})
	l.Info("first line")
	l.Warn("second line", "n", 2)
	l.Close()

	if len(onDisk) != 2 {
		t.Fatalf("stdout was written %d times, want once per line (2)", len(onDisk))
	}
	for i, ok := range onDisk {
		if !ok {
			t.Errorf("line %d reached stdout before it was in moombox.log", i+1)
		}
	}
}

// TestAFailingFileWriteNeverCostsStdoutALine is the other half of lineSinks:
// with the file written first, a failed write to it — a full disk, a handle
// the OS took away — must not keep the line off the console, where it is the
// one place the operator still sees it.
//
// Mutant: lineSinks.Write returning at the file's error — stdout gets nothing.
func TestAFailingFileWriteNeverCostsStdoutALine(t *testing.T) {
	l, err := New(filepath.Join(t.TempDir(), "moombox.log"), "INFO", 1<<20, 3)
	if err != nil {
		t.Fatal(err)
	}
	var echoed strings.Builder
	l.stdout.w = &echoed
	l.fileMu.Lock()
	l.file.Close() // every write to it now fails with os.ErrClosed
	l.fileMu.Unlock()

	l.Info("while the file fails", "n", 1)
	l.Error("while the file fails", "n", 2)
	l.Close()

	for _, want := range []string{"n=1", "n=2"} {
		if !strings.Contains(echoed.String(), want) {
			t.Errorf("stdout is missing the line %q logged while moombox.log failed:\n%s", want, echoed.String())
		}
	}
}

// TestLineRouterRunsInsideTheLogCall pins SetLineRouter's contract, which
// per-job log routing depends on (W24-11): the router has seen the line by the
// time Info returns — no channel, no goroutine — so a status write the caller
// makes next can never untrack the job ahead of its own line. It sees the
// ring-buffer shape, the one db.RouteLogToJobs matches job IDs in.
//
// Mutants this kills:
//   - the route call dropped from log(): the router never sees the line.
//   - SetLineRouter(nil) storing a pointer to the nil func: every later line
//     panics inside route and leaves a diagnostic.
//   - the recover dropped from route: the panicking router crashes the test.
func TestLineRouterRunsInsideTheLogCall(t *testing.T) {
	l, err := New("", "INFO", 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	l.SuppressStdout() // what the TUI does — diagf then lands in the ring

	var routed []string // appended on this goroutine only: that is the contract
	l.SetLineRouter(func(line string) { routed = append(routed, line) })

	l.Info("job error", "jobID", "vid0000001")
	if len(routed) != 1 {
		t.Fatalf("the router saw %d lines by the time Info returned, want 1", len(routed))
	}
	if ring := l.GetRecentLines(); routed[0] != ring[len(ring)-1] {
		t.Errorf("the router saw %q, the ring holds %q — it must see the ring-buffer shape", routed[0], ring[len(ring)-1])
	}
	l.Debug("below the level")
	if len(routed) != 1 {
		t.Errorf("a line below the level reached the router: %q", routed[len(routed)-1])
	}

	l.SetLineRouter(nil)
	l.Info("after the router was removed")
	if len(routed) != 1 {
		t.Errorf("a removed router still saw %q", routed[len(routed)-1])
	}
	for _, line := range l.GetRecentLines() {
		if strings.Contains(line, "line router panicked") {
			t.Fatalf("removing the router left one that panics: %q", line)
		}
	}

	l.SetLineRouter(func(string) { panic("router exploded") })
	l.Info("logged through a broken router")
	ring := strings.Join(l.GetRecentLines(), "\n")
	if !strings.Contains(ring, "logged through a broken router") {
		t.Errorf("a panicking router cost the ring its line:\n%s", ring)
	}
	if !strings.Contains(ring, "line router panicked: router exploded") {
		t.Errorf("a panicking router left no diagnostic:\n%s", ring)
	}
}

// TestRingSequenceNumbersPairASnapshotWithItsFeed pins what lets a reader
// that seeds from RecentLines and then follows SubscribeLines show each line
// once (W24-14): the snapshot names the number of its newest line, every line
// fed is numbered the way the ring numbered it, and a line emitted after the
// snapshot always numbers above it. A reader subscribes FIRST, so a line
// logged between the two reads is in both — and is the one the number lets
// it skip.
//
// Mutants this kills:
//   - addToRingBuffer not advancing ringSeq: the snapshot says 0 and every
//     line is numbered 0, so nothing tells the replayed line from a new one.
//   - broadcast handing SubscribeLines 0 instead of the ring's number: the fed
//     numbers stop matching. (A number read again after the fact matches it
//     whenever one goroutine logs; TestAFedLineKeepsTheNumberTheRingGaveIt
//     interleaves two.)
//   - RecentLines returning the line count instead of the newest number once
//     the ring wraps: the snapshot claims fewer lines than it has seen.
//   - Close leaving SubscribeLines channels open: the range below never ends.
func TestRingSequenceNumbersPairASnapshotWithItsFeed(t *testing.T) {
	l, err := New("", "INFO", 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	l.SuppressStdout()

	if lines, seq := l.RecentLines(); len(lines) != 0 || seq != 0 {
		t.Fatalf("an empty ring reported %d lines up to %d, want none up to 0", len(lines), seq)
	}

	// Past the ring's size, so the numbers have to keep counting after the
	// ring starts overwriting.
	const before = defaultRingSize + 50
	for i := range before {
		l.Info("before the reader", "i", i)
	}

	// The reader's order: the feed first, then the snapshot.
	sub := l.SubscribeLines()
	l.Info("between the subscription and the snapshot")
	lines, snapSeq := l.RecentLines()
	l.Info("after the snapshot")
	l.Close()

	if snapSeq != before+1 {
		t.Errorf("the snapshot's newest line is numbered %d, want %d — one per line the ring took", snapSeq, before+1)
	}
	if len(lines) != defaultRingSize || !strings.HasSuffix(lines[len(lines)-1], "between the subscription and the snapshot") {
		t.Fatalf("the snapshot holds %d lines ending %q", len(lines), lines[len(lines)-1])
	}

	var fed []Line
	for line := range sub {
		fed = append(fed, line)
	}
	if len(fed) != 2 {
		t.Fatalf("SubscribeLines delivered %d lines, want the 2 logged after it: %v", len(fed), fed)
	}
	if between := fed[0]; !strings.HasSuffix(between.Text, "between the subscription and the snapshot") || between.Seq != snapSeq {
		t.Errorf("the line both reads carry was fed as %q #%d; it is the snapshot's newest, #%d, and a reader must be able to tell",
			between.Text, between.Seq, snapSeq)
	}
	if after := fed[1]; !strings.HasSuffix(after.Text, "after the snapshot") || after.Seq != snapSeq+1 {
		t.Errorf("the line logged after the snapshot was fed as %q #%d, want #%d — above the snapshot's", after.Text, after.Seq, snapSeq+1)
	}
}

// TestAFedLineKeepsTheNumberTheRingGaveIt: SubscribeLines feeds each line
// with the number addToRingBuffer handed back for THAT line, even when another
// goroutine's line takes the next number before the first is fed. A reader
// skips what its snapshot already holds by that number (W24-14), so a line
// fed under a later line's number sits above the snapshot's newest and shows
// twice, while the later line's own number no longer says where it is.
//
// Deterministic, not a race to win: log() hands the line to the router
// between the ring append and the feed, holding no lock, so the router parks
// the first goroutine there while this one logs a second line end to end.
//
// Mutant: broadcast ignoring its seq and reading l.ringSeq again under ringMu
// — the parked line is fed under the second line's number.
func TestAFedLineKeepsTheNumberTheRingGaveIt(t *testing.T) {
	l, err := New("", "INFO", 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	l.SuppressStdout()
	sub := l.SubscribeLines()

	parked, release := make(chan struct{}), make(chan struct{})
	l.SetLineRouter(func(line string) {
		if strings.HasSuffix(line, "parked between the ring and the feed") {
			close(parked)
			<-release
		}
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("panic logging the parked line: %v", r)
			}
		}()
		l.Info("parked between the ring and the feed")
	}()
	<-parked
	l.Info("logged while the first waits")
	close(release)
	<-done

	lines, newest := l.RecentLines()
	l.Close()
	ringNumber := map[string]uint64{}
	for i, text := range lines {
		ringNumber[text] = newest - uint64(len(lines)-1-i)
	}
	var fed []Line
	for line := range sub {
		fed = append(fed, line)
	}
	if len(fed) != 2 {
		t.Fatalf("SubscribeLines delivered %d lines, want 2: %v", len(fed), fed)
	}
	for _, line := range fed {
		if want, ok := ringNumber[line.Text]; !ok || line.Seq != want {
			t.Errorf("%q was fed as #%d; the ring numbered it #%d", line.Text, line.Seq, want)
		}
	}
}
