package cookies

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestShouldKeepWaiting pins the drain loop's exit conditions.
//
// The predicate itself was never the bug — the defect lived in the syscall
// wiring and in WHERE the wait happened — but it is the one piece of the
// drain that can be checked without a Job Object, and getting either half of
// it backwards resurrects the failure it exists to prevent: drop the
// `active > 0` half and the browser is killed mid-load again; drop the
// budget half and a hung browser pins a refresh forever.
func TestShouldKeepWaiting(t *testing.T) {
	cases := []struct {
		name    string
		active  int
		elapsed time.Duration
		budget  time.Duration
		want    bool
	}{
		{"browser still running, budget left", 2, time.Second, 30 * time.Second, true},
		{"job empty", 0, time.Second, 30 * time.Second, false},
		{"budget blown with browser alive", 2, 31 * time.Second, 30 * time.Second, false},
		{"budget blown and job empty", 0, 31 * time.Second, 30 * time.Second, false},
		{"exactly at the budget", 2, 30 * time.Second, 30 * time.Second, false},
		{"one lap short of the budget", 1, 29950 * time.Millisecond, 30 * time.Second, true},
		// A negative count is not something QueryInformationJobObject can
		// produce (the field is a DWORD), but a future caller passing a
		// sentinel must not be read as "keep waiting forever".
		{"nonsense negative count", -1, time.Second, 30 * time.Second, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldKeepWaiting(tc.active, tc.elapsed, tc.budget); got != tc.want {
				t.Errorf("shouldKeepWaiting(%d, %s, %s) = %v, want %v",
					tc.active, tc.elapsed, tc.budget, got, tc.want)
			}
		})
	}
}

// TestDrainJobReturnsImmediatelyWithoutAJob covers the two ways there is
// nothing to drain: newProcessJob failed (runWithTimeout carries on with a
// nil job) and a job with no handle — which is also what every non-Windows
// build looks like, since their activeProcesses always reports zero. Both
// must return nil instantly rather than erroring or burning the budget.
func TestDrainJobReturnsImmediatelyWithoutAJob(t *testing.T) {
	for _, tc := range []struct {
		name string
		job  *processJob
	}{
		{"nil job", nil},
		{"zero-value job", &processJob{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			if err := drainJob(context.Background(), tc.job, start, 30*time.Second, nopLogger{}); err != nil {
				t.Fatalf("drainJob = %v, want nil", err)
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Errorf("drainJob took %s; it should not have waited at all", elapsed)
			}
		})
	}
}

// capturingLogger records every message written through it. Carries all four
// methods so it satisfies the AutoCookieService logger field as well as the
// narrower one drainJob takes — one capturing logger for the package.
//
// msgs is every message regardless of level; infos and debugs are the same
// messages split out, for the tests whose subject IS the level — a line the
// operator sees by default versus one they have to go looking for.
type capturingLogger struct {
	msgs   []string
	infos  []string
	debugs []string
}

func (l *capturingLogger) Debug(msg string, args ...any) {
	l.msgs = append(l.msgs, msg)
	l.debugs = append(l.debugs, msg)
}

func (l *capturingLogger) Info(msg string, args ...any) {
	l.msgs = append(l.msgs, msg)
	l.infos = append(l.infos, msg)
}
func (l *capturingLogger) Warn(msg string, args ...any)  { l.msgs = append(l.msgs, msg) }
func (l *capturingLogger) Error(msg string, args ...any) { l.msgs = append(l.msgs, msg) }

// contains reports whether any recorded message contains sub.
func (l *capturingLogger) contains(sub string) bool {
	return countContaining(l.msgs, sub) > 0
}

// countContaining reports how many of `lines` contain sub.
func countContaining(lines []string, sub string) int {
	n := 0
	for _, m := range lines {
		if strings.Contains(m, sub) {
			n++
		}
	}
	return n
}

// TestDrainJobWithNothingToWaitOnDoesNotClaimTheBrowserFinished is a wording
// fix with a platform behind it.
//
// The drain exits as soon as the job reports zero active processes, and used
// to log "browser finished; job drained" when it did. On Linux and darwin the
// processJob stubs return zero unconditionally — there is no Job Object — so
// that line fired on lap ZERO of every single launch, announcing a completion
// nobody had observed and nothing had waited for. It is the one place in the
// drain that broke the "could not confirm is not confirmed" line the rest of
// the refresh path holds.
//
// A zero-handle job reports zero on Windows too, so this reproduces the exact
// shape on every platform.
func TestDrainJobWithNothingToWaitOnDoesNotClaimTheBrowserFinished(t *testing.T) {
	log := &capturingLogger{}
	if err := drainJob(context.Background(), &processJob{}, time.Now(), 30*time.Second, log); err != nil {
		t.Fatalf("drainJob = %v, want nil", err)
	}

	if len(log.msgs) != 1 {
		t.Fatalf("expected exactly one log line, got %v", log.msgs)
	}
	got := log.msgs[0]
	if strings.Contains(got, "browser finished") {
		t.Errorf("nothing was ever seen alive in the job, so nothing finished as far as this can tell: %q", got)
	}
	if !strings.Contains(got, "not waited on") {
		t.Errorf("the line must say what actually happened — that there was nothing to wait for: %q", got)
	}
}
