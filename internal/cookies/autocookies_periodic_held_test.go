package cookies

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// warnArgsLogger keeps every Warn line with its key=value args, under a lock:
// the periodic tick logs from its own goroutine while the test reads. The args
// it sees here are a hostname and a lock path, never a cookie value.
type warnArgsLogger struct {
	mu    sync.Mutex
	warns []string
}

func (l *warnArgsLogger) Debug(msg string, args ...any) {}
func (l *warnArgsLogger) Info(msg string, args ...any)  {}
func (l *warnArgsLogger) Error(msg string, args ...any) {}

func (l *warnArgsLogger) Warn(msg string, args ...any) {
	var b strings.Builder
	b.WriteString(msg)
	for i := 0; i+1 < len(args); i += 2 {
		fmt.Fprintf(&b, " %v=%v", args[i], args[i+1])
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, b.String())
}

// lines returns the Warn lines that start with msg.
func (l *warnArgsLogger) lines(msg string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, w := range l.warns {
		if strings.HasPrefix(w, msg) {
			out = append(out, w)
		}
	}
	return out
}

// TestPeriodicTickReportsAHeldProfileAsASkip: a profile another machine's
// browser holds is a skip that says why, not a failure — the pass declined and
// launched nothing. The 30-minute tick logged it as "periodic auto-cookie
// refresh failed", on every tick for as long as the lock stood, while the
// status line both UIs draw from the same pass says why it was skipped. The
// tick now says "skipped" and carries the sentence, host and lock path
// included, as the reason.
//
// Mutants (checked): the tick's ErrProfileInUse arm removed — it logs
// "failed" again; the arm logging without the reason — the line names no lock.
func TestPeriodicTickReportsAHeldProfileAsASkip(t *testing.T) {
	profileDir := t.TempDir()
	lock := filepath.Join(profileDir, "SingletonLock")
	symlinkLock(t, foreignLockHost+"-4242", lock)
	cookiePath := ytAuthCookieFile(t)
	jar := NewCookieJar()
	if err := jar.Load(cookiePath); err != nil {
		t.Fatalf("load the fixture cookie file: %v", err)
	}
	log := &warnArgsLogger{}
	s := NewAutoCookieService(profileDir, cookiePath, jar, log)
	// A path that does not exist: a regression past the lock fails at exec
	// rather than opening a window.
	unlaunchable := filepath.Join(t.TempDir(), "not-a-browser")
	s.detectBrowser = func() *DetectedBrowser {
		return &DetectedBrowser{Type: "chrome", Path: unlaunchable, Name: "unlaunchable test browser"}
	}

	s.StartPeriodicRefresh(t.Context(), 20*time.Millisecond)

	const skipped = "periodic auto-cookie refresh skipped — a browser holds the profile"
	if !waitFor(func() bool { return len(log.lines(skipped)) > 0 }, 10*time.Second) {
		t.Fatalf("no tick logged %q (LastError=%q, %d \"failed\" lines)",
			skipped, lastErrorSnapshot(s), len(log.lines("periodic auto-cookie refresh failed")))
	}
	line := log.lines(skipped)[0]
	for _, want := range []string{"in use by " + foreignLockHost, "delete " + strconv.Quote(lock)} {
		if !strings.Contains(line, want) {
			t.Errorf("the skip line %q does not say %q", line, want)
		}
	}
	if failed := log.lines("periodic auto-cookie refresh failed"); len(failed) > 0 {
		t.Errorf("a held profile was logged as a failure %d times: %q", len(failed), failed[0])
	}
}
