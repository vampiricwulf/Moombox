package cookies

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// driveDead feeds one platform a dead session's verdicts: signed out every
// `step` from `from` for `span`, and returns when each one warranted recovery.
// A dead session is never quiet, which is what keeps livenessQuietReset from
// starting its schedule over between re-alarms.
func driveDead(rs *RefreshService, platform string, from time.Time, span, step time.Duration) (fires []time.Time) {
	for at := from; !at.After(from.Add(span)); at = at.Add(step) {
		if due, _ := rs.recordLiveness(platform, false, at); due {
			fires = append(fires, at)
		}
	}
	return fires
}

// TestRecordLivenessRefireWindowStopsAtTheCap is the CAP. Uncapped doubling
// reaches a fortnight in eleven alarms, and a session that recovered two weeks
// ago is still suppressed.
//
// Re-pinned for owner ruling KA-4 (2026-11-13): it used to space its verdicts a
// full day apart, which the 24 h quiet reset now reads as a new episode each
// time. A dead session's verdicts arrive every feed cycle, so that is what
// this drives.
//
// Mutation: delete the `if next > livenessRefireCap` clamp — the last two
// alarms are then 32 h and 64 h apart.
func TestRecordLivenessRefireWindowStopsAtTheCap(t *testing.T) {
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	t0 := time.Now()

	// Alarms at 0, +30m, then gaps of 1h, 2h, 4h, 8h, 16h, 24h, 24h, 24h.
	fires := driveDead(rs, "youtube", t0, 5*24*time.Hour, 10*time.Minute)
	if len(fires) != 10 {
		t.Fatalf("a session dead for five days raised %d alarms, want 10 (30m, then doubling to the cap): %v", len(fires), fires)
	}

	rs.mu.RLock()
	got := rs.livenessRefireBackoff["youtube"]
	rs.mu.RUnlock()
	if got != livenessRefireCap {
		t.Errorf("the back-off settled at %v, want the %v cap", got, livenessRefireCap)
	}
	// The cap is a real window, not just a stored number.
	for i := len(fires) - 2; i < len(fires); i++ {
		if gap := fires[i].Sub(fires[i-1]); gap != livenessRefireCap {
			t.Errorf("alarm %d came %v after the one before it, want the %v cap", i, gap, livenessRefireCap)
		}
	}
}

// TestRecordLivenessRefireResetsOnlyAfterAQuietDay is the RESET, re-pinned for
// owner ruling KA-4 (2026-11-13). It replaces
// TestRecordLivenessRefireResetsWhenAuthReturns, which pinned the old rule: any
// signed-in verdict put the platform back on the base. A session whose pages
// flicker produces a signed-in verdict every few minutes, so the old rule
// re-ran the base window for as long as the flicker lasted (55 browser
// launches in 60 hours in the field).
//
// Mutations: call resetLivenessRefire from the signed-in branch again (the
// "one base window" line fails); drop the quiet reset from foldLiveness (the
// "after a quiet day" lines fail); make the quiet reset shorter than a day
// (the "23 h" line fails); make resetLivenessRefire delete lastRecoveryDecided
// too (the stamp line fails).
func TestRecordLivenessRefireResetsOnlyAfterAQuietDay(t *testing.T) {
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	t0 := time.Now()
	backoff := func() (time.Duration, bool) {
		rs.mu.RLock()
		defer rs.mu.RUnlock()
		w, ok := rs.livenessRefireBackoff["youtube"]
		return w, ok
	}

	if due, _ := rs.recordLiveness("youtube", false, t0); !due {
		t.Fatal("premise broken: the first logged-out verdict must warrant recovery")
	}
	// Stamps at t0+base and escalates the window to 2x base.
	secondAlarm := t0.Add(livenessRefireWindow)
	if due, _ := rs.recordLiveness("youtube", false, secondAlarm); !due {
		t.Fatal("premise broken: the first re-alarm must land one base window later")
	}

	// A page renders signed in a minute later. That is the flicker, not a
	// repair.
	if due, _ := rs.recordLiveness("youtube", true, secondAlarm.Add(time.Minute)); due {
		t.Fatal("a logged-in observation warranted recovery")
	}
	// One BASE window after the last stamp: the base schedule would fire here
	// and the escalated one (2x base) must not.
	if due, _ := rs.recordLiveness("youtube", false, secondAlarm.Add(livenessRefireWindow)); due {
		t.Error("a signed-out page one base window after the last alarm warranted recovery — a signed-in verdict put the back-off on the base again, which is the relaunch storm")
	}
	lastOut := secondAlarm.Add(2 * livenessRefireWindow)
	if due, _ := rs.recordLiveness("youtube", false, lastOut); !due {
		t.Fatal("the escalated window never let the next alarm through")
	}
	escalated, _ := backoff()

	// Pages stay signed in. 23 hours of that is not enough.
	if due, _ := rs.recordLiveness("youtube", true, lastOut.Add(livenessQuietReset-time.Hour)); due {
		t.Fatal("a logged-in observation warranted recovery")
	}
	if got, _ := backoff(); got != escalated {
		t.Errorf("the back-off is %v after 23 h without a signed-out page, want the escalated %v — the reset needs the full %v", got, escalated, livenessQuietReset)
	}
	// A full quiet day is.
	if due, _ := rs.recordLiveness("youtube", true, lastOut.Add(livenessQuietReset)); due {
		t.Fatal("a logged-in observation warranted recovery")
	}
	if got, ok := backoff(); ok {
		t.Errorf("the back-off is still %v after a quiet day, want it back on the base", got)
	}
	// The reset clears the escalation only: the dedupe stamp stands.
	rs.mu.RLock()
	stamp, ok := rs.lastRecoveryDecided["youtube"]
	rs.mu.RUnlock()
	if !ok || !stamp.Equal(lastOut) {
		t.Errorf("lastRecoveryDecided[\"youtube\"] = %v (present=%v), want the untouched %v", stamp, ok, lastOut)
	}

	// A new loss is reported at once and re-alarms on the base schedule.
	fresh := lastOut.Add(livenessQuietReset + time.Hour)
	if due, _ := rs.recordLiveness("youtube", false, fresh); !due {
		t.Error("the first signed-out page after a quiet day did not warrant recovery")
	}
	if due, _ := rs.recordLiveness("youtube", false, fresh.Add(livenessRefireWindow)); !due {
		t.Error("the re-alarm after a quiet day did not land one base window later")
	}
}

// TestQuietDayIsFoundByASignedOutVerdictToo: an install with nothing observing
// for a day (no channels, the fallback probe refused) has no signed-in verdict
// to carry the reset. The first signed-out page after the quiet finds it.
//
// Mutation: move the quiet reset inside the signed-in branch.
func TestQuietDayIsFoundByASignedOutVerdictToo(t *testing.T) {
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	t0 := time.Now()
	fires := driveDead(rs, "youtube", t0, 4*time.Hour, 10*time.Minute)
	last := t0.Add(4 * time.Hour)

	fresh := last.Add(livenessQuietReset)
	out := rs.foldLiveness("youtube", false, fresh)
	if !out.due {
		t.Fatal("the first signed-out page after a quiet day did not warrant recovery")
	}
	if out.cause.Attempt != 1 || out.cause.NextWindow != livenessRefireWindow {
		t.Errorf("after %d alarms and a quiet day the next one is attempt %d with a %v window, want attempt 1 on the %v base",
			len(fires), out.cause.Attempt, out.cause.NextWindow, livenessRefireWindow)
	}
}

// escalatedThenSignedIn leaves "youtube" with two alarms behind it (window 1 h
// from the second, at t0+30m) and the run of signed-out verdicts ended by a
// signed-in page at t0+31m.
func escalatedThenSignedIn(t *testing.T) (rs *RefreshService, t0 time.Time) {
	t.Helper()
	rs = NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	t0 = time.Now()
	if due, _ := rs.recordLiveness("youtube", false, t0); !due {
		t.Fatal("premise broken: the first logged-out verdict must warrant recovery")
	}
	if due, _ := rs.recordLiveness("youtube", false, t0.Add(livenessRefireWindow)); !due {
		t.Fatal("premise broken: the first re-alarm must land one base window later")
	}
	rs.recordLiveness("youtube", true, t0.Add(livenessRefireWindow+time.Minute))
	return rs, t0
}

// TestDeadRunFiresInsideAnEscalatedWindow: with the reset a day away, an
// escalated window must not hide a session that dies outright (owner ruling
// KA-4). livenessDeadRun signed-out verdicts in a row, no signed-in one between
// them, fire whatever the window says, once.
//
// Mutations: drop `deadRun` from the window check (the third verdict stays
// suppressed); stop setting deadFired (the run fires again three verdicts
// later, and every three after that).
func TestDeadRunFiresInsideAnEscalatedWindow(t *testing.T) {
	rs, t0 := escalatedThenSignedIn(t)
	at := t0.Add(livenessRefireWindow + 10*time.Minute) // 50 minutes of window left

	for i := 1; i < livenessDeadRun; i++ {
		if due, _ := rs.recordLiveness("youtube", false, at); due {
			t.Fatalf("signed-out verdict %d of the run warranted recovery inside the escalated window", i)
		}
		at = at.Add(livenessRunSpacing)
	}
	out := rs.foldLiveness("youtube", false, at)
	if !out.due {
		t.Fatalf("%d signed-out verdicts in a row did not warrant recovery — an escalated window is hiding a dead session", livenessDeadRun)
	}
	if out.cause.Trigger != RecoveryTriggerPage || out.cause.Attempt != 3 {
		t.Errorf("cause = %+v, want the page trigger and attempt 3 of this episode", out.cause)
	}
	for i := 1; i <= 2*livenessDeadRun; i++ {
		if due, _ := rs.recordLiveness("youtube", false, at.Add(time.Duration(i)*livenessRunSpacing)); due {
			t.Fatalf("the run fired a second time, %d verdicts on — it is once per run, then the window", i)
		}
	}
}

// TestDeadRunCountsFromTheVerdictThatFired: the session dies on the very
// verdict that fires an escalated-window alarm. That verdict starts the
// stretch of signed-out pages and stamps the decision at the same instant, and
// the rule used to compare the two and never fire: the loss then waited out
// the whole next window, up to a day, unless the API check failed too.
//
// Mutations: require the decision to predate the run again (nothing fires);
// stop setting deadFired on the dead-run fire (it fires every three verdicts).
func TestDeadRunCountsFromTheVerdictThatFired(t *testing.T) {
	rs, t0 := escalatedThenSignedIn(t)
	// The 1 h window from the second alarm has passed: this page fires, and
	// the window becomes 2 h.
	at := t0.Add(livenessRefireWindow + time.Hour)
	if due, _ := rs.recordLiveness("youtube", false, at); !due {
		t.Fatal("premise broken: a signed-out page after the window must warrant recovery")
	}
	for i := 1; i < livenessDeadRun; i++ {
		if due, _ := rs.recordLiveness("youtube", false, at.Add(time.Duration(i)*livenessRunSpacing)); due {
			t.Fatalf("signed-out verdict %d after the alarm warranted recovery", i)
		}
	}
	dead := at.Add(livenessDeadRun * livenessRunSpacing)
	if due, _ := rs.recordLiveness("youtube", false, dead); !due {
		t.Fatalf("%d signed-out verdicts after an alarm, none signed in, did not warrant recovery — the 2 h window is hiding a dead session", livenessDeadRun)
	}
	for i := 1; i <= 2*livenessDeadRun; i++ {
		if due, _ := rs.recordLiveness("youtube", false, dead.Add(time.Duration(i)*livenessRunSpacing)); due {
			t.Fatalf("the dead run fired again %d verdicts later, inside the window", i)
		}
	}
}

// TestDeadRunStandsDownOnTheBaseWindow: a first loss re-alarms one base window
// later by itself. The dead-session rule must not add an alarm inside it.
//
// Mutation: drop `escalated` from deadRun.
func TestDeadRunStandsDownOnTheBaseWindow(t *testing.T) {
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	t0 := time.Now()
	fires := driveDead(rs, "youtube", t0, livenessRefireWindow-time.Minute, livenessRunSpacing)
	if len(fires) != 1 {
		t.Fatalf("a first loss raised %d alarms inside the base window, want 1: %v", len(fires), fires)
	}
}

// TestFlickerNeverCompletesTheDeadRun: the two things that keep a flicker from
// buying the dead-run fire. A signed-in page between the signed-out ones starts
// the run over, and one feed cycle's burst (one verdict per channel, seconds
// apart) counts once.
//
// Mutations: stop clearing the run in the signed-in branch (the first half
// fires); count every verdict regardless of livenessRunSpacing (the second
// half fires).
func TestFlickerNeverCompletesTheDeadRun(t *testing.T) {
	rs, t0 := escalatedThenSignedIn(t)
	at := t0.Add(livenessRefireWindow + 5*time.Minute)
	for i := range 4 {
		if due, _ := rs.recordLiveness("youtube", false, at); due {
			t.Fatalf("signed-out page %d of a flicker warranted recovery inside the escalated window", i)
		}
		rs.recordLiveness("youtube", true, at.Add(time.Minute))
		at = at.Add(livenessRunSpacing + time.Minute)
	}

	burst, t1 := escalatedThenSignedIn(t)
	for i := range 2 * livenessDeadRun {
		at := t1.Add(livenessRefireWindow + 5*time.Minute + time.Duration(i)*500*time.Millisecond)
		if due, _ := burst.recordLiveness("youtube", false, at); due {
			t.Fatalf("verdict %d of one feed cycle's burst warranted recovery — one cycle must count once toward the run", i)
		}
	}
}

// TestDeadRunStandsDownForATierOneRecovery: a recovery decided inside the run,
// by the API check, is already working on the loss.
//
// Mutation: compare the run against tier-2 fires only.
func TestDeadRunStandsDownForATierOneRecovery(t *testing.T) {
	rs, t0 := escalatedThenSignedIn(t)
	at := t0.Add(livenessRefireWindow + 10*time.Minute)
	rs.recordLiveness("youtube", false, at)
	rs.noteRecoveryDecided("youtube", at.Add(time.Minute))
	// The tier-1 stamp restarts the escalated window (1 h); the run completes
	// well inside it.
	for i := 1; i <= livenessDeadRun; i++ {
		if due, _ := rs.recordLiveness("youtube", false, at.Add(time.Duration(i)*livenessRunSpacing)); due {
			t.Fatalf("verdict %d fired the dead run although tier 1 decided a recovery inside it", i+1)
		}
	}
}

// TestDeadRunStandsDownForATierOneRecoveryDecidedFirst: the API check fails
// before any page of the stretch renders signed out (the last page verdict was
// signed in, so no run is counting). The signed-out pages that follow are the
// same loss and must not fire a second recovery inside the window.
//
// Mutation: stand down only when a run is already counting (`ep.run > 0`).
func TestDeadRunStandsDownForATierOneRecoveryDecidedFirst(t *testing.T) {
	rs, t0 := escalatedThenSignedIn(t)
	at := t0.Add(livenessRefireWindow + 10*time.Minute)
	rs.noteRecoveryDecided("youtube", at)
	for i := 1; i <= livenessDeadRun+1; i++ {
		if due, _ := rs.recordLiveness("youtube", false, at.Add(time.Duration(i)*livenessRunSpacing)); due {
			t.Fatalf("signed-out verdict %d fired the dead run for a loss tier 1 had already raised", i)
		}
	}
	// A signed-in page ends the stretch and re-arms the rule.
	next := at.Add(time.Duration(livenessDeadRun+2) * livenessRunSpacing)
	rs.recordLiveness("youtube", true, next)
	fired := false
	for i := 1; i <= livenessDeadRun; i++ {
		due, _ := rs.recordLiveness("youtube", false, next.Add(time.Duration(i)*livenessRunSpacing))
		fired = fired || due
	}
	if !fired {
		t.Fatal("the dead-session rule stayed off after a signed-in page ended the stretch tier 1 had claimed")
	}
}

// TestRecoveryCauseSaysWhatAskedForIt pins the widened callback at both
// producers that can be reached without a network seam: a page verdict carries
// the page trigger with its attempt and next window.
func TestRecoveryCauseSaysWhatAskedForIt(t *testing.T) {
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	var got []RecoveryCause
	rs.OnRecoveryNeeded = func(_ string, cause RecoveryCause) { got = append(got, cause) }
	rs.ObserveLiveness("youtube", false)
	want := RecoveryCause{Trigger: RecoveryTriggerPage, Attempt: 1, NextWindow: livenessRefireWindow}
	if len(got) != 1 || got[0] != want {
		t.Errorf("causes = %+v, want exactly %+v", got, want)
	}
}

// TestSignedOutPageAfterAPassIsWarnedOnce is ruling KA-4(b)'s "one Warn per pass,
// not per verdict".
//
// Mutations: drop `ep.passWarned = true` (three Warns); drop the Warn (none);
// stop NotePageRecoveryPass re-arming it (the second pass is never reported).
func TestSignedOutPageAfterAPassIsWarnedOnce(t *testing.T) {
	const line = "a page still renders signed out after the automatic recovery pass"
	rec := &capturingLogger{}
	rs := NewRefreshService(jarWithAuth(t), 0, rec)
	rs.ObserveLiveness("youtube", false)
	if rec.contains(line) {
		t.Fatal("the line was logged before any pass ran")
	}

	rs.NotePageRecoveryPass("youtube", time.Now().Add(-time.Second))
	for range 3 {
		rs.ObserveLiveness("youtube", false)
	}
	if got := countContaining(rec.warns, line); got != 1 {
		t.Fatalf("%d Warn lines for three signed-out pages after one pass, want 1: %q", got, rec.warns)
	}

	rs.NotePageRecoveryPass("youtube", time.Now().Add(-time.Second))
	rs.ObserveLiveness("youtube", false)
	if got := countContaining(rec.warns, line); got != 2 {
		t.Errorf("%d Warn lines after a second pass, want 2", got)
	}
}

// TestClearRecoveryEscalation: the operator's setup, import or manual refresh
// puts both platforms back on the base and forgets the signed-out pages, and
// keeps the dedupe stamp.
//
// Mutations: keep pageEpisodes (the health stays flickering); delete
// lastRecoveryDecided (the stamp line fails); skip the push (pushed stays 0).
func TestClearRecoveryEscalation(t *testing.T) {
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	now := time.Now()
	driveDead(rs, "youtube", now.Add(-2*time.Hour), 2*time.Hour, 10*time.Minute)
	rs.recordLiveness("twitch", false, now)
	if got := rs.GetStatus().SessionHealth("youtube", now); got != SessionFlickering {
		t.Fatalf("premise broken: health = %q after signed-out pages, want %q", got, SessionFlickering)
	}

	pushed := 0
	rs.OnAuthChange = func(s AuthStatus) {
		pushed++
		if s.YouTubePages != (PageHealth{}) || s.TwitchPages != (PageHealth{}) {
			t.Errorf("the pushed status still carries page facts: %+v %+v", s.YouTubePages, s.TwitchPages)
		}
	}
	rs.ClearRecoveryEscalation()

	if pushed != 1 {
		t.Errorf("OnAuthChange fired %d times, want 1 — the UIs draw this state", pushed)
	}
	status := rs.GetStatus()
	for _, platform := range []string{"youtube", "twitch"} {
		if got := status.SessionHealth(platform, now); got != SessionOK {
			t.Errorf("%s health = %q after the operator's gesture, want %q", platform, got, SessionOK)
		}
	}
	rs.mu.RLock()
	_, escalated := rs.livenessRefireBackoff["youtube"]
	_, stamped := rs.lastRecoveryDecided["youtube"]
	rs.mu.RUnlock()
	if escalated {
		t.Error("the back-off is still escalated")
	}
	if !stamped {
		t.Error("the dedupe stamp was cleared with the escalation")
	}

	var nilService *RefreshService
	nilService.ClearRecoveryEscalation() // must not panic: the routes may hold none
	nilService.NotePageRecoveryPass("youtube", now)
}

// TestPageFactsArePushedWhenTheUIsWouldDrawThem: the health is drawn from a
// status the UIs are pushed, and a dead session's burst must not push once per
// channel.
//
// Mutation: push on every signed-out verdict (LastSignedOut in
// pageHealthMoved).
func TestPageFactsArePushedWhenTheUIsWouldDrawThem(t *testing.T) {
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	var pushed []AuthStatus
	rs.OnAuthChange = func(s AuthStatus) { pushed = append(pushed, s) }

	rs.ObserveLiveness("youtube", true)
	if len(pushed) != 0 {
		t.Fatalf("a signed-in page on a healthy session pushed a status: %+v", pushed)
	}
	for range 5 {
		rs.ObserveLiveness("youtube", false)
	}
	if len(pushed) != 1 {
		t.Fatalf("%d pushes for one burst of signed-out pages, want 1", len(pushed))
	}
	got := pushed[0].YouTubePages
	if got.LastSignedOut.IsZero() || got.Attempts != 1 || !got.NextAttempt.After(got.LastSignedOut) {
		t.Errorf("pushed page facts = %+v, want the signed-out time, attempt 1 and a later next attempt", got)
	}
}

// TestSessionHealthParity is the Go half of
// web/tests/fixtures/session-health-parity.json; the JS half is
// web/tests/cookie-badge.test.mjs. One predicate, two languages (CLAUDE.md
// State machines rule 2).
func TestSessionHealthParity(t *testing.T) {
	raw, err := os.ReadFile("../../web/tests/fixtures/session-health-parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name   string `json:"name"`
			Now    string `json:"now"`
			Status struct {
				Found           bool   `json:"found"`
				Verification    string `json:"verification"`
				PageSignedOutAt string `json:"pageSignedOutAt"`
			} `json:"status"`
			Health string `json:"health"`
			JSOnly bool   `json:"jsOnly"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	verdicts := map[string]RefreshVerdict{}
	for _, v := range []RefreshVerdict{RefreshOK, RefreshFailed, RefreshUnknown} {
		verdicts[v.String()] = v
	}
	seen := map[SessionHealth]bool{}
	for _, c := range fixture.Cases {
		if c.JSOnly {
			continue
		}
		now, err := time.Parse(time.RFC3339, c.Now)
		if err != nil {
			t.Fatalf("%s: now: %v", c.Name, err)
		}
		var last time.Time
		if c.Status.PageSignedOutAt != "" {
			if last, err = time.Parse(time.RFC3339, c.Status.PageSignedOutAt); err != nil {
				t.Fatalf("%s: pageSignedOutAt: %v", c.Name, err)
			}
		}
		verdict, ok := verdicts[c.Status.Verification]
		if !ok {
			t.Fatalf("%s: verification %q is not a RefreshVerdict", c.Name, c.Status.Verification)
		}
		got := SessionHealthOf(c.Status.Found, verdict, last, now)
		if string(got) != c.Health {
			t.Errorf("%s: SessionHealthOf = %q, fixture says %q", c.Name, got, c.Health)
		}
		seen[got] = true
	}
	for _, h := range []SessionHealth{SessionOK, SessionFlickering, SessionRelogin} {
		if !seen[h] {
			t.Errorf("no fixture case reaches %q", h)
		}
	}
}

// TestLivenessBackoffLinesCarryNoCookieValue is the leak test for the lines
// and the status facts ruling KA-4 added: the fire Warn's attempt and window,
// the signed-out-after-a-pass Warn, and the page facts on the status. Names,
// counts and times only.
func TestLivenessBackoffLinesCarryNoCookieValue(t *testing.T) {
	rec := &argRecordingLogger{}
	rs := NewRefreshService(jarWithAuth(t), 0, rec)
	var statuses []AuthStatus
	rs.OnAuthChange = func(s AuthStatus) { statuses = append(statuses, s) }
	rs.OnRecoveryNeeded = func(string, RecoveryCause) {}

	rs.ObserveLiveness("youtube", false)
	rs.NotePageRecoveryPass("youtube", time.Now().Add(-time.Second))
	rs.ObserveLiveness("youtube", false)
	rs.ObserveLiveness("youtube", true)
	rs.ClearRecoveryEscalation()
	statuses = append(statuses, rs.GetStatus())

	logged := rec.all()
	for _, line := range []string{"triggering recovery", "still renders signed out"} {
		if !strings.Contains(logged, line) {
			t.Fatalf("premise broken: the %q line was never logged, so this scans nothing: %s", line, logged)
		}
	}
	shown, err := json.Marshal(struct {
		Statuses []AuthStatus
		Pages    []PageHealth
	}{statuses, []PageHealth{statuses[0].YouTubePages, statuses[0].TwitchPages}})
	if err != nil {
		t.Fatal(err)
	}
	// jarWithAuth's two cookie values.
	for _, secret := range []string{"sapisid-value", "login-info-value"} {
		if strings.Contains(logged, secret) {
			t.Errorf("a log line carries the cookie value %q: %s", secret, logged)
		}
		if strings.Contains(string(shown), secret) {
			t.Errorf("the status carries the cookie value %q: %s", secret, shown)
		}
	}
}
