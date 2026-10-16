package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"unicode/utf8"

	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/redact"
)

// A crash the launcher's supervision respawns is almost always one the logger
// never saw: Moombox's own goroutines recover their panics into the log, so
// what ends the process is a fatal runtime error (concurrent map writes, out
// of memory, a stack overflow), a panic in a goroutine without a recover, or
// a fatal signal — and the Go runtime writes those only to stderr. In TUI mode
// that is the alternate screen the crashed TUI still held, which the respawned
// TUI clears; moombox.log, the TUI log panel and the dashboard got nothing but
// the exit code, and the Recovered From Crash notification sent the operator
// to "check the logs for the cause" they did not hold (W28-A04).
//
// So the child gives the runtime a second place to write it (armCrashOutput,
// debug.SetCrashOutput): a crash file beside the log, created empty at every
// start. The next start finds what a crash left there (takePreviousCrash),
// logs its beginning — the cause, and the trace of the goroutine that died —
// through the redaction every log line about a failure gets, and keeps the
// whole output one more crash cycle as <crash file>.1, cut to crashKeepBytes:
// at most two crashes' output on disk, however many crashes there were. One
// line never reaches the file: a fatal error's own "fatal error: …", which
// the runtime prints before it counts itself as crashing; crashCause names
// such a crash by the frame it struck in instead.

const (
	// crashExcerptBytes bounds the crash output moombox.log gets. The runtime
	// writes the cause first, then the goroutine that died, then — for a
	// fatal signal — every other goroutine, so the beginning is where the
	// diagnosis is; the rest stays in the kept file.
	crashExcerptBytes = 8 << 10
	// crashKeepBytes bounds the previous crash's output kept beside the log.
	crashKeepBytes = 1 << 20
	// crashCauseBytes bounds the cause line: the log line's cause= and the
	// notification's field.
	crashCauseBytes = 300
)

// crashOutputPath names the crash file beside the log at logPath: the log's
// name without its extension, then -crash.log ("moombox.log" →
// "moombox-crash.log").
func crashOutputPath(logPath string) string {
	base := filepath.Base(logPath)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	if stem == "" || stem == "." {
		stem = "moombox"
	}
	return filepath.Join(filepath.Dir(logPath), stem+"-crash.log")
}

// keptCrashPath is where takePreviousCrash keeps the previous crash's output.
func keptCrashPath(crashPath string) string { return crashPath + ".1" }

// previousCrash is what the previous process's crash left in the crash file.
// The zero value is "it left nothing": no crash, or one that ended the
// process without the runtime reporting it (SIGKILL, the OOM killer, an
// os.Exit with that code).
type previousCrash struct {
	cause   string // the output's first line, redacted and bounded
	excerpt string // its beginning, redacted, bounded by crashExcerptBytes
	size    int64  // the whole output's size
	kept    string // where the output is kept now; the crash file itself when moving it aside failed
}

func (c previousCrash) captured() bool { return c.size > 0 }

// takePreviousCrash reads what a crash left at crashPath, then moves it to
// keptCrashPath (replacing the crash before it) and cuts that to
// crashKeepBytes. A crash file that is absent or empty yields the zero
// value. The error reports what could not be read or moved; whatever was
// read is returned with it.
func takePreviousCrash(crashPath string) (previousCrash, error) {
	f, err := os.Open(crashPath)
	if errors.Is(err, fs.ErrNotExist) {
		return previousCrash{}, nil
	}
	if err != nil {
		return previousCrash{}, err
	}
	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		f.Close()
		return previousCrash{}, err
	}
	head := make([]byte, crashExcerptBytes)
	n, err := io.ReadFull(f, head)
	f.Close()
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return previousCrash{}, err
	}
	c := previousCrash{size: info.Size(), kept: crashPath}
	c.excerpt, c.cause = crashExcerpt(head[:n], info.Size())

	kept := keptCrashPath(crashPath)
	if err := os.Rename(crashPath, kept); err != nil {
		return c, fmt.Errorf("keep the crash output: %w", err)
	}
	c.kept = kept
	if c.size > crashKeepBytes {
		if err := os.Truncate(kept, crashKeepBytes); err != nil {
			return c, fmt.Errorf("bound the kept crash output: %w", err)
		}
	}
	return c, nil
}

// crashExcerpt turns the first bytes of a crash's output (of total bytes in
// all) into the redacted excerpt moombox.log gets and the cause line. A cut
// excerpt ends at a line boundary and says how much of the output it left
// out.
func crashExcerpt(head []byte, total int64) (excerpt, cause string) {
	text := string(head)
	if int64(len(head)) < total {
		if i := strings.LastIndexByte(text, '\n'); i > 0 {
			text = text[:i+1]
		}
		text += fmt.Sprintf("… %d more bytes", total-int64(len(text)))
	}
	excerpt = redactCrashText(strings.ToValidUTF8(strings.TrimRight(text, "\n"), "�"))
	cause = crashCause(strings.Split(excerpt, "\n"))
	if len(cause) > crashCauseBytes {
		cut := crashCauseBytes
		for cut > 0 && !utf8.RuneStart(cause[cut]) {
			cut--
		}
		cause = cause[:cut] + "…"
	}
	return excerpt, cause
}

// crashCause names a crash from the lines of its output: the first line — the
// runtime's "panic: …", "SIGABRT: abort", "fatal error: …" — unless the
// output starts at a goroutine's trace. That is a fatal error's (throw,
// fatal: concurrent map writes, out of memory, a misused sync.Mutex), whose
// own line the runtime prints before it counts itself as crashing, so that
// line reaches stderr only and the crash file starts after it. Its cause is
// then the first frame of the trace below the runtime's fatal and throw
// functions, which says where it struck.
func crashCause(lines []string) string {
	first := -1
	for i, line := range lines {
		if strings.TrimSpace(line) != "" {
			first = i
			break
		}
	}
	if first < 0 {
		return ""
	}
	head := strings.TrimSpace(lines[first])
	if !strings.HasPrefix(head, "goroutine ") {
		return head
	}
	for _, line := range lines[first+1:] {
		if line == "" || strings.HasPrefix(line, "\t") {
			continue // a frame's file:line, or the gap before the next goroutine
		}
		fn := line
		if i := strings.LastIndexByte(fn, '('); i > 0 {
			fn = fn[:i]
		}
		switch fn[strings.LastIndexByte(fn, '.')+1:] {
		case "throw", "fatal", "fatalthrow", "fatalpanic":
			continue
		}
		return "fatal error in " + fn + " (the runtime printed its message to stderr only)"
	}
	return head
}

// crashURLRe finds a URL in crash output, the same shape internal/redact
// matches in flattened error text.
var crashURLRe = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s"]+`)

// redactCrashText keeps credentials out of crash output on its way to the log
// and a notification. A crash's text is whatever the panic value said — an
// error quoting a request URL, a webhook URL, a media URL with its signed
// session — and nothing in a trace needs a URL's path, query or userinfo to
// be read, so every URL is cut to its origin (redact.URLOrigin), and a GVS PO
// token outside any URL goes too (redact.PoTokenText).
func redactCrashText(s string) string {
	if strings.Contains(s, "://") {
		s = crashURLRe.ReplaceAllStringFunc(s, redact.URLOrigin)
	}
	return redact.PoTokenText(s)
}

// armCrashOutput makes the runtime write a fatal error's output — an
// unrecovered panic, a fatal runtime error, a fatal signal's trace — to a
// fresh, empty crash file at crashPath as well as to stderr.
func armCrashOutput(crashPath string) error {
	f, err := os.OpenFile(crashPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close() // SetCrashOutput keeps its own duplicate of the descriptor
	return debug.SetCrashOutput(f, debug.CrashOptions{})
}

// captureCrashOutput logs what the previous process's crash left beside the
// log at logPath, then arms this process's own capture. Called as soon as the
// logger is open, so a crash anywhere after it is caught; the result is what
// the Recovered From Crash notice reports (crashRecoveredNotice). Logged on
// every start that finds a crash, not only a crash respawn: the launcher
// gives up after five in a row, and fails a fresh launch fast, telling the
// operator to check the logs — which the next start makes true.
func (s *runState) captureCrashOutput(logPath string) previousCrash {
	crashPath := crashOutputPath(logPath)
	if abs, err := filepath.Abs(crashPath); err == nil {
		crashPath = abs // the notification names it to someone reading it elsewhere
	}
	prev, err := takePreviousCrash(crashPath)
	if err != nil {
		s.log.Warn("could not take the previous run's crash output", slog.String("file", crashPath), slog.String("error", err.Error()))
	}
	if prev.captured() {
		s.log.Error("the previous run crashed — the beginning of its crash output",
			slog.String("cause", prev.cause),
			slog.String("crashOutput", prev.kept),
			slog.String("output", prev.excerpt))
	}
	if err := armCrashOutput(crashPath); err != nil {
		s.log.Warn("could not arm the crash output file — a crash's cause will reach stderr only",
			slog.String("file", crashPath), slog.String("error", err.Error()))
	}
	return prev
}

// crashRecoveredNotice is the Recovered From Crash notification for a crash
// respawn: the previous exit code, and what its crash output says — the cause
// and where the output is — or, when it left none, that the log has nothing
// more on it, rather than sending the operator to look. logName is the log
// file's name.
func crashRecoveredNotice(code string, prev previousCrash, logName string) (desc string, fields []notifications.Field) {
	if !prev.captured() {
		return fmt.Sprintf("Moombox exited abnormally (code %s) and was automatically restarted. It left no crash output: "+
			"a process killed outright (SIGKILL, the OOM killer) or one that exited with that code itself reports none, "+
			"so %s holds nothing more on the cause", code, logName), nil
	}
	return fmt.Sprintf("Moombox exited abnormally (code %s) and was automatically restarted — the beginning of its crash output "+
			"is in %s, and the whole of it in %s", code, logName, prev.kept),
		[]notifications.Field{{Name: "Cause", Value: prev.cause}}
}
