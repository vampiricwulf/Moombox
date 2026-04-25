// Package logger provides structured logging with file rotation and pub/sub.
package logger

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// switchableWriter wraps an io.Writer with an enable/disable toggle.
// Used to suppress stdout output when the TUI is running (BubbleTea
// owns the alternate screen, so raw log writes corrupt the display).
type switchableWriter struct {
	w       io.Writer
	enabled atomic.Bool
}

func (sw *switchableWriter) Write(p []byte) (int, error) {
	if !sw.enabled.Load() {
		return len(p), nil
	}
	return sw.w.Write(p)
}

// Logger wraps slog with file rotation, pub/sub, and ring buffer support.
//
// Lock hierarchy (acquire in this order to avoid deadlock; never invert):
//
//  1. fileMu     — protects file rotation (rotate() and Write of formatted line)
//  2. ringMu     — protects the ringBuffer slice + ringIndex/ringCount
//  3. jobLogsMu  — protects the jobLogs map + per-buffer fan-out
//  4. subMu      — protects the subscribers slice
//
// Most operations only take one lock. The Write path takes fileMu first,
// then publishes to ring/jobs/subscribers (each under its own mutex) without
// holding fileMu. Audit reports/small-packages.md.
type Logger struct {
	slog    *slog.Logger
	level   *slog.LevelVar
	file    *os.File
	fileMu  sync.Mutex

	// Toggleable stdout writer (disabled during TUI)
	stdout *switchableWriter

	// File rotation
	filePath   string
	maxSize    int
	maxFiles   int
	currentSize int64

	// Ring buffer for recent log lines
	ringBuffer []string
	ringMu     sync.RWMutex
	ringSize   int
	ringIndex  int
	ringCount  int

	// Per-job log buffers
	jobLogs   map[string]*jobLogBuffer
	jobLogsMu sync.RWMutex

	// Pub/sub for log lines
	subscribers []chan string
	subMu       sync.RWMutex

	// Rate-limiting for broadcast drop warnings (prevents stderr spam
	// when a subscriber is persistently slow)
	dropWarnLast atomic.Int64 // unix nanoseconds of last warning

	closed atomic.Bool
}

type jobLogBuffer struct {
	lines []string
	mu    sync.RWMutex
}

const (
	defaultRingSize = 200
	maxJobLogLines  = 500
)

// minLogRotationSize is the smallest acceptable rotation threshold. Below
// this, a single formatted log line could exceed the cap and trigger
// rotation on every Write — a hot loop. 4 KiB is well under any sane
// log-file-size budget while being more than any realistic single
// structured log line.
const minLogRotationSize = 4096

// New creates a new Logger with file rotation support.
func New(filePath, level string, maxSize, maxFiles int) (*Logger, error) {
	if maxSize < minLogRotationSize {
		maxSize = minLogRotationSize
	}
	if maxFiles < 1 {
		maxFiles = 1
	}
	l := &Logger{
		filePath: filePath,
		maxSize:  maxSize,
		maxFiles: maxFiles,
		ringSize: defaultRingSize,
		ringBuffer: make([]string, defaultRingSize),
		jobLogs:  make(map[string]*jobLogBuffer),
	}

	// Set up log level
	l.level = new(slog.LevelVar)
	l.SetLevel(level)

	// Open log file. Empty filePath is a deliberate "stdout + ring-buffer
	// only" mode used by tests and by the launcher's pre-init phase; we
	// surface it on stderr so callers that mis-construct the logger don't
	// silently lose all file output. Audit reports/small-packages.md.
	if filePath != "" {
		if err := l.openFile(); err != nil {
			return nil, fmt.Errorf("failed to open log file: %w", err)
		}
	} else {
		fmt.Fprintln(os.Stderr, "logger: no file path configured — log output goes to stdout + ring buffer only")
	}

	// Create multi-writer (stdout + file)
	// Stdout goes through a switchable writer so it can be suppressed
	// when the TUI is running (the TUI log panel uses Subscribe() instead).
	l.stdout = &switchableWriter{w: os.Stdout}
	l.stdout.enabled.Store(true)
	var writers []io.Writer
	writers = append(writers, l.stdout)
	if l.file != nil {
		writers = append(writers, l)
	}
	multi := io.MultiWriter(writers...)

	// Custom handler with timestamp formatting. Use the attribute's own
	// time value (captured by slog at the log call site) rather than
	// time.Now() so formatted timestamps reflect the exact call moment
	// even when slog buffers. This also keeps file/stdout timestamps in
	// lock-step with the ring-buffer / subscriber timestamps emitted
	// through formatLogLine.
	opts := &slog.HandlerOptions{
		Level: l.level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				t := a.Value.Time()
				if t.IsZero() {
					t = time.Now()
				}
				a.Value = slog.StringValue(t.Format("2006-01-02 15:04:05"))
			}
			return a
		},
	}
	l.slog = slog.New(slog.NewTextHandler(multi, opts))

	return l, nil
}

// Write implements io.Writer for the log file with rotation.
func (l *Logger) Write(p []byte) (n int, err error) {
	l.fileMu.Lock()
	defer l.fileMu.Unlock()

	if l.file == nil {
		// Retry-on-write: if rotate previously failed to reopen the log
		// file (transient ENOSPC, file permissions reset, antivirus
		// holding the file briefly, etc.), try again here so the next
		// Write recovers automatically instead of silently dropping
		// every log line until restart. Per-write os.OpenFile is one
		// syscall — cheap when the failure is transient, harmless when
		// it persists. Audit reports/small-packages.md (logger.go
		// rotate reopen-failure sentinel).
		if err := l.openFile(); err != nil {
			// Underlying failure persists. Don't log to stderr on every
			// write (would spam) — rotate already logged the original
			// failure once when it first hit.
			return 0, nil
		}
	}

	n, err = l.file.Write(p)
	l.currentSize += int64(n)

	// Check if rotation is needed
	if l.currentSize >= int64(l.maxSize) {
		l.rotate()
	}

	return n, err
}

func (l *Logger) openFile() error {
	dir := filepath.Dir(l.filePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	f, err := os.OpenFile(l.filePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}

	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}

	l.file = f
	l.currentSize = info.Size()
	return nil
}

func (l *Logger) rotate() {
	oldFile := l.file
	l.file = nil

	if oldFile != nil {
		oldFile.Close()
	}

	// Shift existing log files
	for i := l.maxFiles - 1; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", l.filePath, i)
		dst := fmt.Sprintf("%s.%d", l.filePath, i+1)
		if err := os.Rename(src, dst); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "logger: rotation rename %s -> %s failed: %v\n", src, dst, err)
		}
	}

	// Rename current to .1
	if err := os.Rename(l.filePath, l.filePath+".1"); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "logger: rotation rename current log failed: %v\n", err)
	}

	// Remove excess files
	excess := fmt.Sprintf("%s.%d", l.filePath, l.maxFiles+1)
	if err := os.Remove(excess); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "logger: rotation remove excess file failed: %v\n", err)
	}

	// Open fresh file — if this fails, log to stderr so we don't silently lose all logging
	if err := l.openFile(); err != nil {
		fmt.Fprintf(os.Stderr, "logger: rotation failed to open new log file: %v\n", err)
	}
}

func (l *Logger) log(level slog.Level, msg string, args ...any) {
	if l.closed.Load() {
		return
	}

	// Check level before doing any work
	if !l.slog.Enabled(context.Background(), level) {
		return
	}

	l.slog.Log(context.Background(), level, msg, args...)

	// Format for ring buffer and subscribers
	line := formatLogLine(level, msg, args...)
	l.addToRingBuffer(line)
	l.broadcast(line)
}

// logLineBuilderPool reuses strings.Builder instances across formatLogLine
// calls to avoid allocating a fresh buffer on every log line. Hot path:
// active downloads emit O(10) lines/second per concurrent job, so cutting
// the per-line allocation matters at sustained rates.
//
// Pre-grown to 256 bytes — typical log lines are 100-200 bytes, so most
// invocations don't need to grow the underlying slice.
//
// Audit reports/small-packages.md (formatLogLine sync.Pool).
var logLineBuilderPool = sync.Pool{
	New: func() any {
		sb := &strings.Builder{}
		sb.Grow(256)
		return sb
	},
}

func formatLogLine(level slog.Level, msg string, args ...any) string {
	sb := logLineBuilderPool.Get().(*strings.Builder)
	defer func() {
		sb.Reset()
		logLineBuilderPool.Put(sb)
	}()

	ts := time.Now().Format("2006-01-02 15:04:05")
	levelStr := level.String()

	sb.WriteString(ts)
	sb.WriteString(" ")
	sb.WriteString(levelStr)
	sb.WriteString(" ")
	sb.WriteString(msg)

	// Process args as key=value pairs. slog.Attr values (from slog.String,
	// slog.Any, etc.) are self-contained key=value pairs and count as one arg
	// but should not trigger the "!MISSING" marker.
	i := 0
	for i < len(args) {
		if attr, ok := args[i].(slog.Attr); ok {
			fmt.Fprintf(sb, " %s=%v", attr.Key, attr.Value)
			i++
		} else if i+1 < len(args) {
			fmt.Fprintf(sb, " %v=%v", args[i], args[i+1])
			i += 2
		} else {
			fmt.Fprintf(sb, " %v=!MISSING", args[i])
			i++
		}
	}

	// Copy the bytes into a fresh string. strings.Builder.String() returns
	// a string that ALIASES the internal buffer via unsafe; if we Put the
	// builder back to the pool while a caller holds that string, the next
	// Get+Reset+WriteString would overwrite the returned string's data.
	// strings.Clone copies once, breaking the alias.
	return strings.Clone(sb.String())
}

func (l *Logger) addToRingBuffer(line string) {
	l.ringMu.Lock()
	defer l.ringMu.Unlock()

	l.ringBuffer[l.ringIndex] = line
	l.ringIndex = (l.ringIndex + 1) % l.ringSize
	if l.ringCount < l.ringSize {
		l.ringCount++
	}
}

func (l *Logger) broadcast(line string) {
	if l.closed.Load() {
		return
	}

	l.subMu.RLock()
	defer l.subMu.RUnlock()

	for _, ch := range l.subscribers {
		select {
		case ch <- line:
		default:
			// Drop if subscriber is slow — rate-limit the warning to at most
			// once per second to avoid flooding stderr under sustained load
			now := time.Now().UnixNano()
			last := l.dropWarnLast.Load()
			if now-last >= int64(time.Second) {
				if l.dropWarnLast.CompareAndSwap(last, now) {
					fmt.Fprintf(os.Stderr, "logger: dropped log line for slow subscriber\n")
				}
			}
		}
	}
}

// Debug logs a debug message.
func (l *Logger) Debug(msg string, args ...any) {
	l.log(slog.LevelDebug, msg, args...)
}

// Info logs an info message.
func (l *Logger) Info(msg string, args ...any) {
	l.log(slog.LevelInfo, msg, args...)
}

// Warn logs a warning message.
func (l *Logger) Warn(msg string, args ...any) {
	l.log(slog.LevelWarn, msg, args...)
}

// Error logs an error message.
func (l *Logger) Error(msg string, args ...any) {
	l.log(slog.LevelError, msg, args...)
}

// LogForJob logs a message and also stores it in the per-job buffer.
func (l *Logger) LogForJob(jobID string, level slog.Level, msg string, args ...any) {
	l.log(level, msg, args...)

	line := formatLogLine(level, msg, args...)

	l.jobLogsMu.RLock()
	buf, ok := l.jobLogs[jobID]
	l.jobLogsMu.RUnlock()

	if !ok {
		l.jobLogsMu.Lock()
		// Double-check under write lock to avoid overwriting concurrent creation
		if buf, ok = l.jobLogs[jobID]; !ok {
			buf = &jobLogBuffer{lines: make([]string, 0, 100)}
			l.jobLogs[jobID] = buf
		}
		l.jobLogsMu.Unlock()
	}

	buf.mu.Lock()
	if len(buf.lines) >= maxJobLogLines {
		// Remove oldest 20% of entries to amortize pruning cost
		pruneCount := maxJobLogLines / 5
		copy(buf.lines, buf.lines[pruneCount:])
		buf.lines = buf.lines[:len(buf.lines)-pruneCount]
	}
	buf.lines = append(buf.lines, line)
	buf.mu.Unlock()
}

// GetJobLogs returns log lines for a specific job.
func (l *Logger) GetJobLogs(jobID string) []string {
	l.jobLogsMu.RLock()
	buf, ok := l.jobLogs[jobID]
	l.jobLogsMu.RUnlock()

	if !ok {
		return nil
	}

	buf.mu.RLock()
	defer buf.mu.RUnlock()

	result := make([]string, len(buf.lines))
	copy(result, buf.lines)
	return result
}

// ClearJobLogs removes the log buffer for a specific job.
func (l *Logger) ClearJobLogs(jobID string) {
	l.jobLogsMu.Lock()
	delete(l.jobLogs, jobID)
	l.jobLogsMu.Unlock()
}

// PruneJobLogs removes log buffers for job IDs not in the provided set.
func (l *Logger) PruneJobLogs(activeIDs map[string]struct{}) {
	l.jobLogsMu.Lock()
	defer l.jobLogsMu.Unlock()
	for id := range l.jobLogs {
		if _, ok := activeIDs[id]; !ok {
			delete(l.jobLogs, id)
		}
	}
}

// GetRecentLines returns the most recent log lines from the ring buffer.
func (l *Logger) GetRecentLines() []string {
	l.ringMu.RLock()
	defer l.ringMu.RUnlock()

	result := make([]string, 0, l.ringCount)
	if l.ringCount < l.ringSize {
		// Buffer not full yet
		for i := range l.ringCount {
			result = append(result, l.ringBuffer[i])
		}
	} else {
		// Buffer is full, start from ringIndex (oldest)
		for i := range l.ringSize {
			idx := (l.ringIndex + i) % l.ringSize
			result = append(result, l.ringBuffer[idx])
		}
	}
	return result
}

// Subscribe creates a new subscription channel for log lines.
//
// The returned channel is buffered (capacity 100); if the subscriber's
// reader cannot keep up, broadcast drops new messages and emits a
// rate-limited warning rather than blocking.
//
// Lifecycle: callers must drive their own read loop with a select that
// also listens to a cancellation signal (context, stop chan, etc.).
// Unsubscribe does NOT close the returned channel (see Unsubscribe
// godoc for rationale). When the Logger is Close'd, all remaining
// subscribers (those that never Unsubscribed) do have their channels
// closed, so a read loop that blocks on <-ch will unblock at shutdown.
// A goroutine that Unsubscribes mid-run and continues to read from the
// channel will block forever — either stop reading once you Unsubscribe
// or never Unsubscribe (let Close drain you).
func (l *Logger) Subscribe() chan string {
	ch := make(chan string, 100)
	l.subMu.Lock()
	l.subscribers = append(l.subscribers, ch)
	l.subMu.Unlock()
	return ch
}

// Unsubscribe removes a subscription channel from the broadcast list.
//
// The channel is intentionally NOT closed here: broadcast may be
// concurrently sending to it under its own lock, and closing a channel
// that a goroutine may still write to is a panic. After Unsubscribe
// returns, no new messages will be sent to the channel, but any
// goroutine still blocked on <-ch will block forever unless it also
// exits on some external signal. See Subscribe's godoc for the
// required caller pattern.
func (l *Logger) Unsubscribe(ch chan string) {
	l.subMu.Lock()
	defer l.subMu.Unlock()

	for i, sub := range l.subscribers {
		if sub == ch {
			l.subscribers = append(l.subscribers[:i], l.subscribers[i+1:]...)
			return
		}
	}
}

// SetLevel sets the log level dynamically. Returns false if the level string
// was not recognized (falls back to Info).
func (l *Logger) SetLevel(level string) bool {
	switch strings.ToUpper(level) {
	case "DEBUG":
		l.level.Set(slog.LevelDebug)
	case "INFO":
		l.level.Set(slog.LevelInfo)
	case "WARN", "WARNING":
		l.level.Set(slog.LevelWarn)
	case "ERROR":
		l.level.Set(slog.LevelError)
	default:
		l.level.Set(slog.LevelInfo)
		fmt.Fprintf(os.Stderr, "logger: unrecognized log level %q, falling back to INFO\n", level)
		return false
	}
	return true
}

// SuppressStdout disables stdout logging. Call this when the TUI starts
// so raw log writes don't corrupt BubbleTea's alternate screen.
func (l *Logger) SuppressStdout() {
	l.stdout.enabled.Store(false)
}

// RestoreStdout re-enables stdout logging. Call this after the TUI exits.
func (l *Logger) RestoreStdout() {
	l.stdout.enabled.Store(true)
}

// Close flushes and closes the logger.
func (l *Logger) Close() {
	l.closed.Store(true)
	l.fileMu.Lock()
	defer l.fileMu.Unlock()

	if l.file != nil {
		l.file.Sync()
		l.file.Close()
		l.file = nil
	}

	// Nil out subscribers under lock first, then close channels.
	// This prevents broadcast() from sending to closed channels since
	// it holds RLock and will see the nil/empty slice.
	l.subMu.Lock()
	subs := l.subscribers
	l.subscribers = nil
	l.subMu.Unlock()

	for _, ch := range subs {
		close(ch)
	}
}
