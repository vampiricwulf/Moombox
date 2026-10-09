package worker

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// cookies.cookie_file is restart-required: saved without the restart, the
// setting names a file the running services never touch. The failed-refresh
// advice named the setting anyway, so the operator replaced the wrong file.
// It names the file in use (CookieFileInUse) now, the setting only as a
// fallback.
//
// Mutant: the advice reading the setting first again.
func TestFailedRefreshAdviceNamesTheCookieFileInUse(t *testing.T) {
	w, _ := testWorkerSetup(t)
	log := &fieldCaptureLogger{}
	w.logger = log
	cfg := config.Defaults()
	cfg.Cookies.CookieFile = "/new/cookies.txt" // saved, restart declined
	w.configStore = config.NewStore(cfg, "")
	w.OnCookieRefreshNeeded = func(string) CookieRefreshOutcome { return CookieRefreshNotRestored }
	w.CookieFileInUse = func() string { return "/boot/cookies.txt" }

	w.attemptCookieRefresh(&database.Job{ID: "j", VideoID: "v", Platform: "youtube"}, errors.New("sign in to confirm"))

	got, logged := log.field("auto cookie refresh failed — the cookie file has to be replaced by hand", "cookieFile")
	if !logged || got != "/boot/cookies.txt" {
		t.Errorf("advice named %v (logged %v), want the file in use, /boot/cookies.txt", got, logged)
	}
}

// A job-triggered refresh that found the browser profile held by another
// browser (cookies.ErrProfileInUse) is a skip, and the callback logs it as
// one. The worker could not tell that false from a failure, so one line after
// the skip it logged "auto cookie refresh failed — the cookie file has to be
// replaced by hand", replacement named as the only remedy for cookies nothing
// had rejected. CookieRefreshSkipped now leaves the job parked with a line
// that says so and none of the failed-refresh advice.
//
// Mutants (checked): the Skipped arm removed — the advice follows the skip
// and the parked line is missing; its return removed — the advice follows
// the parked line.
func TestHeldProfileSkipIsNotCalledAFailedRefresh(t *testing.T) {
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)
	log := &fieldCaptureLogger{}
	w.logger = log
	w.CookieFileInUse = func() string { return "/boot/cookies.txt" }
	job := parkedRun(t, w, db, "held_profile", false)
	var asked string
	w.OnCookieRefreshNeeded = func(platform string) CookieRefreshOutcome {
		asked = platform
		return CookieRefreshSkipped
	}

	w.setJobError(job, fmt.Errorf("%w: sign in to confirm", ErrCookiesRequired))
	w.queue.Complete(job.ID)
	w.wg.Wait()

	if asked != "youtube" {
		t.Fatalf("OnCookieRefreshNeeded asked for %q, want youtube", asked)
	}
	if got, logged := log.field("the job stays parked — the automatic cookie refresh was skipped, not failed", "jobID"); !logged || got != job.ID {
		t.Errorf("parked line: jobID %v (logged %v), want %s", got, logged, job.ID)
	}
	log.mu.Lock()
	for _, line := range log.lines {
		if msg, _ := line[0].(string); strings.HasPrefix(msg, "auto cookie refresh failed") {
			t.Errorf("a skipped refresh logged %q %v", msg, line[1:])
		}
	}
	log.mu.Unlock()
	if row, _ := db.GetJob(job.ID); row == nil || row.Status != database.StatusCookies {
		t.Errorf("row = %+v, want it still parked in COOKIES?", row)
	}
	if w.queue.isPending(job.ID) {
		t.Error("a skipped refresh handed the parked job on")
	}
}

// outcomeLine is one logged line with its level, for the refresh-outcome
// test below, which cares about both.
type outcomeLine struct {
	level, msg string
	args       []any
}

// attr returns the value logged under key on the line.
func (line outcomeLine) attr(key string) any {
	for i := 0; i+1 < len(line.args); i += 2 {
		if line.args[i] == key {
			return line.args[i+1]
		}
	}
	return nil
}

// outcomeLogger records every line with its level and attributes.
type outcomeLogger struct {
	mu    sync.Mutex
	lines []outcomeLine
}

func (l *outcomeLogger) add(level, msg string, args []any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, outcomeLine{level, msg, args})
}
func (l *outcomeLogger) Debug(msg string, args ...any) { l.add("DEBUG", msg, args) }
func (l *outcomeLogger) Info(msg string, args ...any)  { l.add("INFO", msg, args) }
func (l *outcomeLogger) Warn(msg string, args ...any)  { l.add("WARN", msg, args) }
func (l *outcomeLogger) Error(msg string, args ...any) { l.add("ERROR", msg, args) }

// find returns the first line whose message is msg.
func (l *outcomeLogger) find(msg string) (outcomeLine, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		if line.msg == msg {
			return line, true
		}
	}
	return outcomeLine{}, false
}

// TestUnfailedRefreshOutcomesAreNotCalledFailures: after a job-triggered
// cookie refresh that did not restore access, the worker logged "auto cookie
// refresh failed — the cookie file has to be replaced by hand" for every
// outcome but the held profile's skip. Two of those outcomes were not
// failures. A refresh that could not establish whether the cookies work
// (cookies.RefreshUnknown) judged nothing — the cookies may be fine — and an
// automatic refresh that is turned off attempted nothing. Each now has its
// own line, at the level of the line it sits beside: the unconfirmed one at
// Info, like the skip's parked line, since a recheck answers it; the off one
// at Warn, like the failure's, since only the operator can act. A real
// failure keeps its line, naming the cookie file.
//
// Mutants (checked): either new arm removed (that outcome logs the failed
// line); the Unconfirmed arm's return removed (the failed line follows it);
// the Unconfirmed line at Warn, or the Off line at Info (the level check);
// the Off line without the cookie file (its attribute check).
func TestUnfailedRefreshOutcomesAreNotCalledFailures(t *testing.T) {
	const failed = "auto cookie refresh failed — the cookie file has to be replaced by hand"
	cases := []struct {
		name      string
		outcome   CookieRefreshOutcome
		wantMsg   string
		wantLevel string
	}{
		{"unconfirmed", CookieRefreshUnconfirmed,
			"auto cookie refresh could not confirm the cookies — the job stays parked; R C / Recheck will tell", "INFO"},
		{"off", CookieRefreshOff,
			"automatic cookie refresh is off — replace the cookie file or turn it on in Settings", "WARN"},
		{"failed", CookieRefreshNotRestored, failed, "WARN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			t.Cleanup(w.Stop)
			log := &outcomeLogger{}
			w.logger = log
			w.CookieFileInUse = func() string { return "/boot/cookies.txt" }
			job := parkedRun(t, w, db, "outcome_"+tc.name, false)
			w.OnCookieRefreshNeeded = func(string) CookieRefreshOutcome { return tc.outcome }

			w.setJobError(job, fmt.Errorf("%w: sign in to confirm", ErrCookiesRequired))
			w.queue.Complete(job.ID)
			w.wg.Wait()

			line, ok := log.find(tc.wantMsg)
			if !ok {
				t.Fatalf("no %q line; logged %v", tc.wantMsg, log.lines)
			}
			if line.level != tc.wantLevel {
				t.Errorf("logged at %s, want %s", line.level, tc.wantLevel)
			}
			if tc.outcome != CookieRefreshUnconfirmed {
				if got := line.attr("cookieFile"); got != "/boot/cookies.txt" {
					t.Errorf("cookieFile = %v, want the file in use, /boot/cookies.txt", got)
				}
			}
			if tc.outcome != CookieRefreshNotRestored {
				if _, said := log.find(failed); said {
					t.Errorf("a refresh that did not fail was still called a failure: %q", failed)
				}
			}
			if row, _ := db.GetJob(job.ID); row == nil || row.Status != database.StatusCookies {
				t.Errorf("row = %+v, want it still parked in COOKIES?", row)
			}
			if w.queue.isPending(job.ID) {
				t.Error("an unrestored refresh handed the parked job on")
			}
		})
	}
}
