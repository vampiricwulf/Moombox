// Package logger provides structured logging with file rotation and pub/sub.
package logger

import (
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

	closed bool
}

type jobLogBuffer struct {
	lines []string
	mu    sync.RWMutex
}

const (
	defaultRingSize = 200
	maxJobLogLines  = 500
)

// New creates a new Logger with file rotation support.
func New(filePath, level string, maxSize, maxFiles int) (*Logger, error) {
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

	// Open log file
	if filePath != "" {
		if err := l.openFile(); err != nil {
			return nil, fmt.Errorf("failed to open log file: %w", err)
		}
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

	// Custom handler with timestamp formatting
	opts := &slog.HandlerOptions{
		Level: l.level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				a.Value = slog.StringValue(time.Now().Format("2006-01-02 15:04:05"))
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
		return 0, nil
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
	if l.closed {
		return
	}

	// Check level before doing any work
	if !l.slog.Enabled(nil, level) {
		return
	}

	l.slog.Log(nil, level, msg, args...)

	// Format for ring buffer and subscribers
	line := formatLogLine(level, msg, args...)
	l.addToRingBuffer(line)
	l.broadcast(line)
}

func formatLogLine(level slog.Level, msg string, args ...any) string {
	ts := time.Now().Format("2006-01-02 15:04:05")
	levelStr := level.String()

	var sb strings.Builder
	sb.WriteString(ts)
	sb.WriteString(" ")
	sb.WriteString(levelStr)
	sb.WriteString(" ")
	sb.WriteString(msg)

	for i := 0; i < len(args)-1; i += 2 {
		sb.WriteString(" ")
		sb.WriteString(fmt.Sprintf("%v=%v", args[i], args[i+1]))
	}

	return sb.String()
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
	if l.closed {
		return
	}

	l.subMu.RLock()
	defer l.subMu.RUnlock()

	for _, ch := range l.subscribers {
		select {
		case ch <- line:
		default:
			// Drop if subscriber is slow
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
		// Remove oldest entries
		copy(buf.lines, buf.lines[100:])
		buf.lines = buf.lines[:len(buf.lines)-100]
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
		for i := 0; i < l.ringCount; i++ {
			result = append(result, l.ringBuffer[i])
		}
	} else {
		// Buffer is full, start from ringIndex (oldest)
		for i := 0; i < l.ringSize; i++ {
			idx := (l.ringIndex + i) % l.ringSize
			result = append(result, l.ringBuffer[idx])
		}
	}
	return result
}

// Subscribe creates a new subscription channel for log lines.
func (l *Logger) Subscribe() chan string {
	ch := make(chan string, 100)
	l.subMu.Lock()
	l.subscribers = append(l.subscribers, ch)
	l.subMu.Unlock()
	return ch
}

// Unsubscribe removes a subscription channel.
// The channel is not closed here to avoid a race with broadcast() which may
// be concurrently sending to it. Instead, the channel is simply removed from
// the list and left for GC.
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

// SetLevel sets the log level dynamically.
func (l *Logger) SetLevel(level string) {
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
	}
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
	l.closed = true
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
