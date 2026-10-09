package worker

import (
	"errors"
	"fmt"
	"strings"
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
