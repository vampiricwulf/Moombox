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

// Write never reports a failure of the inner writer. Stdout is the
// best-effort sink: a hung-up SSH tty (EIO), a closed fd 1 (EBADF) or a
// console-less Windows child's invalid handle fails EVERY write for the rest
// of the run, and io.MultiWriter stops at the first writer's error — so
// passing it on kept every line out of moombox.log, the only persistent log,
// while the ring buffer looked normal (W24-10). Logger.Write swallows a
// failed reopen for the same reason.
func (sw *switchableWriter) Write(p []byte) (int, error) {
	if !sw.enabled.Load() {
		return len(p), nil
	}
	_, _ = sw.w.Write(p)
	return len(p), nil
}

// Logger wraps slog with file rotation, pub/sub, and ring buffer support.
//
// Lock hierarchy (acquire in this order to avoid deadlock; never invert):
//
//  1. fileMu     — protects file rotation (rotate() and Write of formatted line)
//  2. subMu      — protects the subscribers slice
//  3. ringMu     — protects the ringBuffer slice + ringIndex/ringCount.
//     Leaf lock: rotate→diagf ring-appends while holding fileMu, so
//     nothing may acquire another logger lock under ringMu. (broadcast's
//     slow-subscriber warning is appended only after it releases
//     subMu.RLock, so subMu and ringMu are never held together.)
//
// Most operations only take one lock. The Write path takes fileMu first,
// then publishes to ring/subscribers (each under its own mutex) without
// holding fileMu. Audit reports/small-packages.md.
//
// Per-job log routing lives in the DATABASE (db.TrackJobForLogs /
// db.RouteLogToJobs fed via SetLineRouter, served by db.GetJobLogs) — the
// logger once carried a parallel LogForJob/GetJobLogs buffer API, but
// nothing in production ever wired it and it was removed 2026-07.
type Logger struct {
	// handler renders the on-disk / stdout line. Its output shape is the
	// file format and is PROTECTED — see log() for why the record is built
	// here instead of through a *slog.Logger.
	handler slog.Handler
	level   *slog.LevelVar
	file    *os.File
	fileMu  sync.Mutex

	// Toggleable stdout writer (disabled during TUI)
	stdout *switchableWriter
	// stderrGate mirrors stdout suppression for the logger's own
	// diagnostics (rotation failures, slow-subscriber drops): while the
	// TUI owns the terminal, a raw stderr write scribbles over the
	// alternate screen, so diagf reroutes those lines into the ring
	// buffer + subscribers (the TUI log panel) instead.
	stderrGate *switchableWriter

	// File rotation
	filePath    string
	maxSize     int
	maxFiles    int
	currentSize int64

	// now samples the wall clock — once per log line, for BOTH formatted
	// shapes — and drives the rotation back-off window. WithClock replaces
	// it; see Option for why only New may write it.
	now func() time.Time
	// renameFile is os.Rename. WithRename replaces it so a test can simulate
	// the Windows failure CORE-3 is about (another process holding the
	// rotation target open makes both renames fail) without holding a real
	// handle.
	renameFile func(oldpath, newpath string) error

	// rotateFailing / rotateBackoffUntil back off after a rotation whose
	// renames failed. On Windows any process holding <log>.1 open (tail,
	// editor, AV) makes both renames fail, and rotate() was re-attempted
	// after EVERY write past the cap — so the failure repeated per log line:
	// a WARN diagnostic into the ring buffer, every WS subscriber and the
	// TUI log panel, per line, while the live file grew unbounded (CORE-3,
	// reproduced at 7.4x the cap). Guarded by fileMu like every other
	// rotation field.
	rotateFailing      bool
	rotateBackoffUntil time.Time
	// rotateStreakStart / rotateLastReport bound the reminder a persistent
	// failure emits. The one detail line the streak's first attempt writes is
	// easy to miss — diagf reaches stderr (never moombox.log itself) or, in
	// TUI mode, a 200-entry ring a busy instance recycles in minutes — while
	// a rename that fails for 48 h leaves the live file at thousands of times
	// the cap. So the streak says so again once an hour, with the size.
	rotateStreakStart time.Time
	rotateLastReport  time.Time

	// Ring buffer for recent log lines
	ringBuffer []string
	ringMu     sync.RWMutex
	ringSize   int
	ringIndex  int
	ringCount  int

	// Pub/sub for log lines
	subscribers []chan string
	subMu       sync.RWMutex

	// router receives every line log() emits, SYNCHRONOUSLY, on the goroutine
	// that logged it — see SetLineRouter. An atomic pointer rather than a
	// plain field because it is installed after New has published the logger
	// through slog.SetDefault, while other goroutines are already logging.
	router atomic.Pointer[func(line string)]

	// Rate-limiting for broadcast drop warnings (prevents stderr spam
	// when a subscriber is persistently slow)
	dropWarnLast atomic.Int64 // unix nanoseconds of last warning

	closed atomic.Bool
}

const defaultRingSize = 200

// minLogRotationSize is the smallest acceptable rotation threshold. Below
// this, a single formatted log line could exceed the cap and trigger
// rotation on every Write — a hot loop. 4 KiB is well under any sane
// log-file-size budget while being more than any realistic single
// structured log line.
const minLogRotationSize = 4096

// rotateBackoff is how long a failed rotation waits before the next attempt.
// While it runs, the live log file is allowed to grow past
// log_max_file_size: an oversize file for up to a minute is strictly better
// than a diagnostic per log line for as long as the holder keeps the handle
// (CORE-3).
const rotateBackoff = 60 * time.Second

// rotateReportInterval is how often a rotation failure that persists says so
// again. The streak's first attempt reports the error itself; every hour
// after that the reminder carries how long the streak has run and how large
// the un-rotated file has grown, because the single first line goes to stderr
// or into a ring buffer that recycles (CORE-3).
const rotateReportInterval = time.Hour

// Option configures a Logger at construction.
//
// Options exist so the seams the tests need are set BEFORE New publishes the
// logger through slog.SetDefault — after that call the logger is reachable
// from every goroutine that logs through slog.Default(), and assigning a
// plain field on it would be an unsynchronised write. The fields are written
// nowhere else.
type Option func(*Logger)

// WithClock replaces the wall clock the logger samples: once per log line for
// both formatted shapes, and for the rotation back-off window. Tests use it
// to pin a timestamp or to step a multi-hour window without sleeping.
func WithClock(now func() time.Time) Option {
	return func(l *Logger) {
		if now != nil {
			l.now = now
		}
	}
}

// WithRename replaces os.Rename in the rotation path, so a test can simulate
// a rotation target another process holds open — the Windows failure the
// back-off exists for — without holding a real handle.
func WithRename(rename func(oldpath, newpath string) error) Option {
	return func(l *Logger) {
		if rename != nil {
			l.renameFile = rename
		}
	}
}

// New creates a new Logger with file rotation support.
func New(filePath, level string, maxSize, maxFiles int, options ...Option) (*Logger, error) {
	if maxSize < minLogRotationSize {
		// config.Validate accepts down to 1024, so a 1024-4095 value is
		// legal config — surface the override instead of silently ignoring
		// the operator's setting.
		fmt.Fprintf(os.Stderr, "logger: log_max_file_size %d below the %d-byte rotation floor, using the floor\n", maxSize, minLogRotationSize)
		maxSize = minLogRotationSize
	}
	if maxFiles < 1 {
		maxFiles = 1
	}
	l := &Logger{
		filePath:   filePath,
		maxSize:    maxSize,
		maxFiles:   maxFiles,
		ringSize:   defaultRingSize,
		ringBuffer: make([]string, defaultRingSize),
		now:        time.Now,
		renameFile: os.Rename,
	}
	for _, opt := range options {
		if opt != nil {
			opt(l)
		}
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
	// io.MultiWriter stops at the first writer that errors, so the stdout
	// sink must never return one: the switchable writer swallows stdout's
	// failures — a dead stdout must never cost the file a line.
	l.stdout = &switchableWriter{w: os.Stdout}
	l.stdout.enabled.Store(true)
	l.stderrGate = &switchableWriter{w: os.Stderr}
	l.stderrGate.enabled.Store(true)
	var writers []io.Writer
	writers = append(writers, l.stdout)
	if l.file != nil {
		writers = append(writers, l)
	}
	multi := io.MultiWriter(writers...)

	// Custom handler with timestamp formatting. Use the attribute's own
	// time value rather than time.Now(): log() stamps the record with the
	// SINGLE instant it also hands formatLogLine, so file/stdout timestamps
	// are the same sample as the ring-buffer / subscriber ones rather than
	// a second reading of the clock (CORE-22).
	opts := &slog.HandlerOptions{
		Level: l.level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			// The Kind check is load-bearing: ReplaceAttr also runs for USER
			// attributes, and Value.Time() panics on non-KindTime values —
			// a caller writing log.Info("x", "time", "12:30") would
			// otherwise crash inside the handler.
			if a.Key == slog.TimeKey && len(groups) == 0 && a.Value.Kind() == slog.KindTime {
				t := a.Value.Time()
				if t.IsZero() {
					t = time.Now()
				}
				a.Value = slog.StringValue(t.Format("2006-01-02 15:04:05"))
			}
			return a
		},
	}
	l.handler = slog.NewTextHandler(multi, opts)
	// Route the process-global slog default through the FULL pipeline (level
	// gate, file, ring buffer, subscribers, TUI-safe stderr gating) via the
	// bridge — not through l.handler directly, which would feed the file but
	// skip the ring buffer and subscribers, leaving stray warnings (e.g.
	// config.Save's DACL warning) invisible in the TUI log panel and the web
	// log endpoints. Std `log` output (http.Server error noise) is rebridged
	// by SetDefault at Info level and lands in the same places.
	slog.SetDefault(slog.New(defaultSlogBridge{l: l}))

	return l, nil
}

// defaultSlogBridge adapts Logger.log — the full pipeline — to slog.Handler
// for the process-global default. Group qualifiers are flattened (stray
// diagnostics don't use them); attrs from WithAttrs are prepended to each
// record's args.
type defaultSlogBridge struct {
	l     *Logger
	attrs []slog.Attr
}

func (b defaultSlogBridge) Enabled(_ context.Context, level slog.Level) bool {
	return level >= b.l.level.Level()
}

func (b defaultSlogBridge) Handle(_ context.Context, r slog.Record) error {
	args := make([]any, 0, (len(b.attrs)+r.NumAttrs())*2)
	for _, a := range b.attrs {
		args = append(args, a.Key, a.Value.Any())
	}
	r.Attrs(func(a slog.Attr) bool {
		args = append(args, a.Key, a.Value.Any())
		return true
	})
	b.l.log(r.Level, r.Message, args...)
	return nil
}

func (b defaultSlogBridge) WithAttrs(attrs []slog.Attr) slog.Handler {
	nb := b
	nb.attrs = append(append([]slog.Attr{}, b.attrs...), attrs...)
	return nb
}

func (b defaultSlogBridge) WithGroup(string) slog.Handler { return b }

// Write implements io.Writer for the log file with rotation.
func (l *Logger) Write(p []byte) (n int, err error) {
	l.fileMu.Lock()
	defer l.fileMu.Unlock()

	if l.file == nil {
		// A goroutine that passed log()'s closed-check but lost the fileMu
		// race against Close must not REOPEN the file after close — that
		// would leave an fd nobody owns.
		if l.closed.Load() {
			return len(p), nil
		}
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
			// failure once when it first hit. Return (len(p), nil) to
			// satisfy the io.Writer contract ("Write must return a non-nil
			// error if it returns n < len(p)") and to keep slog's
			// MultiWriter from short-circuiting on the failed sink.
			return len(p), nil
		}
	}

	n, err = l.file.Write(p)
	l.currentSize += int64(n)

	// Check if rotation is needed
	if l.currentSize >= int64(l.maxSize) {
		l.rotateIfDue()
	}

	return n, err
}

// rotateIfDue rotates unless a previous attempt failed recently. The line
// itself has already been written either way: backing off lets the live file
// grow past the cap, it never drops a line (CORE-3).
func (l *Logger) rotateIfDue() {
	if l.rotateFailing && l.now().Before(l.rotateBackoffUntil) {
		return
	}
	l.rotate()
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

	// firstOfStreak: only the FIRST failed attempt of a streak reports. The
	// rest are the same holder, the same handle, the same message.
	firstOfStreak := !l.rotateFailing
	rotated := true

	// Shift existing log files
	for i := l.maxFiles - 1; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", l.filePath, i)
		dst := fmt.Sprintf("%s.%d", l.filePath, i+1)
		if err := l.renameFile(src, dst); err != nil && !os.IsNotExist(err) {
			rotated = false
			if firstOfStreak {
				l.diagf("logger: rotation rename %s -> %s failed: %v (retrying no sooner than %s)", src, dst, err, rotateBackoff)
			}
		}
	}

	// Rename current to .1
	if err := l.renameFile(l.filePath, l.filePath+".1"); err != nil && !os.IsNotExist(err) {
		rotated = false
		if firstOfStreak {
			l.diagf("logger: rotation rename current log failed: %v (retrying no sooner than %s)", err, rotateBackoff)
		}
	}

	// Remove excess files. Loop upward until the first gap so a runtime
	// DECREASE of log_max_files cleans every stale higher-numbered file —
	// removing only .{maxFiles+1} left .{maxFiles+2}.. behind forever.
	for i := l.maxFiles + 1; ; i++ {
		excess := fmt.Sprintf("%s.%d", l.filePath, i)
		if err := os.Remove(excess); err != nil {
			if !os.IsNotExist(err) && firstOfStreak {
				l.diagf("logger: rotation remove excess file failed: %v", err)
			}
			break
		}
	}

	if rotated {
		l.rotateFailing = false
		l.rotateBackoffUntil = time.Time{}
		l.rotateStreakStart = time.Time{}
		l.rotateLastReport = time.Time{}
	} else {
		now := l.now()
		switch {
		case firstOfStreak:
			l.rotateStreakStart = now
			l.rotateLastReport = now
		case now.Sub(l.rotateLastReport) >= rotateReportInterval:
			// Once an hour for as long as the holder keeps the handle, and
			// never per write: the first line is easy to miss and says
			// nothing about how far past the cap the file has run.
			l.diagf("logger: rotation still blocked after %s; %s is now %d bytes (cap %d)",
				now.Sub(l.rotateStreakStart).Round(time.Minute), l.filePath, l.currentSize, l.maxSize)
			l.rotateLastReport = now
		}
		l.rotateFailing = true
		l.rotateBackoffUntil = now.Add(rotateBackoff)
	}

	// Open fresh file — if this fails, surface it so we don't silently lose
	// all logging. NOT streak-suppressed: a reopen failure is a different
	// fault from a rename failure and loses every subsequent line.
	if err := l.openFile(); err != nil {
		l.diagf("logger: rotation failed to open new log file: %v", err)
	}
}

func (l *Logger) log(level slog.Level, msg string, args ...any) {
	if l.closed.Load() {
		return
	}

	// Check the level before doing any work. Reading the LevelVar directly
	// is the same answer slog.Logger.Enabled gives, without the handler
	// round trip.
	if level < l.level.Level() {
		return
	}

	// ONE clock sample per line, for BOTH wire shapes. slog's TextHandler
	// used to format its own copy from a record slog.Logger stamped with
	// time.Now() while formatLogLine sampled the clock again, so the file
	// line and the UI line could disagree about the second (CORE-22).
	now := l.now()

	// The on-disk / stdout line. Building the record here rather than
	// calling slog.Logger.Log is what carries `now` into the file format —
	// and it skips the runtime.Callers slog.Logger.log does on every call
	// for a source location this handler is not configured to print. The
	// rendered bytes are unchanged: this is the same record Log would have
	// built, with the same handler.
	rec := slog.NewRecord(now, level, msg, 0)
	rec.Add(args...)
	_ = l.handler.Handle(context.Background(), rec)

	// The ring-buffer / subscriber line — the TUI log panel's and the
	// dashboard's shape, which is deliberately not the file's.
	line := formatLogLine(now, level, msg, args...)
	l.addToRingBuffer(line)
	l.route(line)
	l.broadcast(line)
}

// SetLineRouter installs fn to receive every line the logger emits, in the
// ring-buffer / subscriber shape, SYNCHRONOUSLY: on the goroutine that logged
// it, before Debug/Info/Warn/Error return. nil removes it. One router at a
// time; a second call replaces the first.
//
// Per-job log routing (db.RouteLogToJobs) runs here and not behind
// Subscribe, because whether a line belongs to a job is decided against the
// routed set as it stands when the line is ROUTED, and the set changes
// synchronously: a status write untracks a job that goes terminal inside
// UpdateJobFields. A subscriber routes later, on its own goroutine — so
// setJobError's "job error" line, logged just before its status=Error write,
// lost the race to the untrack for 8 of 100 failed jobs and never reached the
// log an operator opens after a failure (W24-11). Routed here, a line logged
// before a status write is always routed before it.
//
// fn must be cheap and must not log: it runs inside every log call, and a log
// line from inside it would recurse. It holds none of the logger's locks.
func (l *Logger) SetLineRouter(fn func(line string)) {
	if fn == nil {
		l.router.Store(nil)
		return
	}
	l.router.Store(&fn)
}

// route hands one line to the router, if one is installed. A panicking router
// is reported on the diagnostic path and the log call returns normally: it
// runs on whatever goroutine logged, recover handlers among them, and a
// broken router must never turn a log line into a crash.
func (l *Logger) route(line string) {
	fn := l.router.Load()
	if fn == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			l.diagf("logger: line router panicked: %v", r)
		}
	}()
	(*fn)(line)
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

func formatLogLine(now time.Time, level slog.Level, msg string, args ...any) string {
	sb := logLineBuilderPool.Get().(*strings.Builder)
	defer func() {
		sb.Reset()
		logLineBuilderPool.Put(sb)
	}()

	ts := now.Format("2006-01-02 15:04:05")
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

	// ringWarn, if set, is appended to the ring buffer AFTER subMu is released.
	// ringMu is the leaf lock (acquired after subMu — see the hierarchy above),
	// so doing the append here under subMu.RLock would be correct ordering; we
	// still defer it past RUnlock so subMu and ringMu are never held at once and
	// the hot fan-out loop stays free of a ringMu acquisition.
	var ringWarn string
	l.subMu.RLock()
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
					if l.stderrGate == nil || l.stderrGate.enabled.Load() {
						fmt.Fprintf(os.Stderr, "logger: dropped log line for slow subscriber\n")
					} else {
						// Defer the ring-append (see ringWarn above); the CAS
						// rate-limit guarantees this is set at most once here.
						ringWarn = time.Now().Format("2006-01-02 15:04:05") +
							" WARN logger: dropped log line for slow subscriber"
					}
				}
			}
		}
	}
	l.subMu.RUnlock()

	if ringWarn != "" {
		l.addToRingBuffer(ringWarn)
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
		l.diagf("logger: unrecognized log level %q, falling back to INFO", level)
		return false
	}
	return true
}

// SuppressStdout disables stdout logging (and reroutes the logger's own
// stderr diagnostics into the ring buffer). Call this when the TUI starts
// so raw log writes don't corrupt BubbleTea's alternate screen.
func (l *Logger) SuppressStdout() {
	l.stdout.enabled.Store(false)
	if l.stderrGate != nil {
		l.stderrGate.enabled.Store(false)
	}
}

// RestoreStdout re-enables stdout logging. Call this after the TUI exits.
func (l *Logger) RestoreStdout() {
	l.stdout.enabled.Store(true)
	if l.stderrGate != nil {
		l.stderrGate.enabled.Store(true)
	}
}

// diagf emits a logger-internal diagnostic. On a normal console it goes to
// stderr; while the TUI owns the terminal (SuppressStdout) it is rerouted
// into the ring buffer + subscribers so it surfaces in the TUI log panel
// instead of scribbling over the alternate screen. Deliberately does NOT go
// through slog/l.Write: rotate() calls this while holding fileMu, and the
// multi-writer path would re-enter Write and deadlock. MUST NOT be called
// from inside broadcast (it re-enters broadcast on the suppressed path, and
// a recursive subMu.RLock deadlocks against a queued writer) — the
// broadcast-drop warning ring-appends directly instead.
func (l *Logger) diagf(format string, args ...any) {
	msg := strings.TrimRight(fmt.Sprintf(format, args...), "\n")
	if l.stderrGate == nil || l.stderrGate.enabled.Load() {
		fmt.Fprintln(os.Stderr, msg)
		return
	}
	line := time.Now().Format("2006-01-02 15:04:05") + " WARN " + msg
	l.addToRingBuffer(line)
	l.broadcast(line)
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
