package cookies

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Arc 10 R1-R3. Every credential in this file is synthetic and none is logged.
//
// The defect these pin: oauth2/validate answers 200 for a valid auth-token
// whether or not a `login` cookie sits beside it, so two of the four ways a
// Twitch capture goes anonymous were invisible to the only thing that writes
// AuthStatus.TwitchAuthenticated. A mark that validate could clear would be no
// mark at all — it would be erased within one 30-minute tick with nothing
// fixed.

// twitchMarkFixture writes a Twitch cookie file, loads it, and returns the
// service pointed at a validate server that answers `code`. The YouTube guide
// seam is pinned too: an unpinned seam is one refactor away from youtube.com.
func twitchMarkFixture(t *testing.T, token, login string, code int) (*RefreshService, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cookies.txt")
	writeTwitchPair(t, path, token, login)
	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	pointTwitchValidateAt(t, statusServer(t, code))
	ytSrv, _ := countingGuide(t, loggedOutGuideBody)
	pointYouTubeGuideAt(t, ytSrv)
	return NewRefreshService(jar, 0, nopLogger{}), path
}

// writeTwitchPair writes exactly the rows the given pair implies. An empty
// half is an ABSENT ROW — the state mergeCookieFiles' expiry pruning leaves.
func writeTwitchPair(t *testing.T, path, token, login string) {
	t.Helper()
	rows := []string{"# Netscape HTTP Cookie File"}
	if token != "" {
		rows = append(rows, strings.Join([]string{"#HttpOnly_.twitch.tv", "TRUE", "/", "TRUE", "0", "auth-token", token}, "\t"))
	}
	if login != "" {
		rows = append(rows, strings.Join([]string{".twitch.tv", "TRUE", "/", "TRUE", "0", "login", login}, "\t"))
	}
	if err := os.WriteFile(path, []byte(strings.Join(rows, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// markWarnRecorder records Warn lines WITH their args, flattened into one
// string per call.
//
// Deliberately not warnRecordingLogger (refresh_lifecycle_test.go), which
// keeps `msg` and drops the args: NoteTwitchAuthLoss's Warn message is a
// constant, so the only thing a leak could ride out on is the "reason" VALUE
// beside it. A msg-only recorder cannot tell the mapped sentence from the
// caller's raw token, which is exactly why a Warn logging the raw argument
// passed the whole suite in the first round.
type markWarnRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (l *markWarnRecorder) Debug(msg string, args ...any) {}
func (l *markWarnRecorder) Info(msg string, args ...any)  {}
func (l *markWarnRecorder) Error(msg string, args ...any) {}
func (l *markWarnRecorder) Warn(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprint(append([]any{msg}, args...)...))
}

func (l *markWarnRecorder) warnLines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// TestValidate200DoesNotClearAStandingTwitchMark is R3, and the whole point of
// the arc.
//
// The mutation: deleting the mark consult from refresh's status block, so the
// block writes verdictFromCheck(twAuth=true, nil) = RefreshOK straight over the
// mark. Under it the operator sees green within one tick while the capture is
// still dropping every subscriber-only message.
//
// The OnAuthRecovered assertion pins the HIGHEST-STAKES of the five sites the
// single twEffective value feeds, and it needs its own mutation: `!prevTW &&
// twAuth` at the recovered-transition site (refresh's tail). The status
// assertions above it all survive that one, because the status literal is a
// different read of the same value — and under it Moombox announces a Twitch
// recovery that never happened and resumes every parked Twitch job straight
// back into the failure they parked on.
func TestValidate200DoesNotClearAStandingTwitchMark(t *testing.T) {
	rs, _ := twitchMarkFixture(t, "test-token-aaaa", "", http.StatusOK)
	var recovered []string
	rs.OnAuthRecovered = func(platform string) { recovered = append(recovered, platform) }

	// A healthy first pass. It arms OnAuthRecovered at all (the transition is
	// gated on hasCheckedOnce) and it leaves the baseline "authenticated", so
	// the mark below is a witnessed fall rather than the startup case.
	rs.doRefresh(context.Background())
	if len(recovered) != 0 {
		t.Fatalf("the fixture is wrong: OnAuthRecovered fired %v on the first pass", recovered)
	}

	rs.NoteTwitchAuthLoss(twitchLossNoLoginCookie)
	rs.doRefresh(context.Background())

	got := rs.GetStatus()
	if got.TwitchAuthenticated {
		t.Error("a validate 200 cleared a standing mark — the two routes validate cannot see are green again with nothing fixed")
	}
	if got.TwitchVerification != RefreshFailed {
		t.Errorf("TwitchVerification = %v, want RefreshFailed — the mark is conclusive", got.TwitchVerification)
	}
	if want := twitchAuthLossMessage(twitchLossNoLoginCookie); got.TwitchError != want {
		t.Errorf("TwitchError = %q, want %q — the mark owns the reason while it stands", got.TwitchError, want)
	}
	if len(recovered) != 0 {
		t.Errorf("OnAuthRecovered fired %v while the mark stood — every parked Twitch job would resume into the same anonymous chat", recovered)
	}
}

// TestATwitchCredentialChangeClearsTheMark is R4's clearing half.
//
// Two mutations. Never clearing: the mark outlives every repair and the
// platform reads dead forever. And re-sampling `rs.twitchMark.identity` on
// each pass instead of holding the value the mark was TAKEN under: it then
// always equals `twIdentity`, so the comparison can never be unequal and the
// mark never clears — the same visible failure, from the opposite mistake.
func TestATwitchCredentialChangeClearsTheMark(t *testing.T) {
	rs, path := twitchMarkFixture(t, "test-token-aaaa", "", http.StatusOK)

	rs.NoteTwitchAuthLoss(twitchLossNoLoginCookie)
	rs.doRefresh(context.Background())
	if rs.GetStatus().TwitchAuthenticated {
		t.Fatal("the fixture is wrong: the mark did not stand through the first pass")
	}

	// The operator's fix: the same token with its login row restored.
	writeTwitchPair(t, path, "test-token-aaaa", "archiveraccount")
	rs.doRefresh(context.Background())

	got := rs.GetStatus()
	if !got.TwitchAuthenticated {
		t.Error("a changed credential pair did not clear the mark — validate never gets to decide again")
	}
	if got.TwitchVerification != RefreshOK {
		t.Errorf("TwitchVerification = %v, want RefreshOK", got.TwitchVerification)
	}
	if got.TwitchError != "" {
		t.Errorf("TwitchError = %q, want \"\" — the mark's reason must not survive the credential that caused it", got.TwitchError)
	}
}

// TestAChangeToDeadCredentialsClearsTheMarkAndReportsTheTruth: clearing is
// keyed on the FINGERPRINT alone, with no authenticated gate.
//
// The mutation: gating the clear on nowAuth. Under it, replacing a
// no-login-cookie pair with a pair whose token is revoked leaves the stale
// sentence about a missing login row in front of an operator whose actual
// problem is a 401.
func TestAChangeToDeadCredentialsClearsTheMarkAndReportsTheTruth(t *testing.T) {
	rs, path := twitchMarkFixture(t, "test-token-aaaa", "", http.StatusUnauthorized)

	rs.NoteTwitchAuthLoss(twitchLossNoLoginCookie)
	writeTwitchPair(t, path, "test-token-bbbb", "archiveraccount")
	rs.doRefresh(context.Background())

	got := rs.GetStatus()
	if got.TwitchAuthenticated {
		t.Error("a 401 reported as authenticated")
	}
	if got.TwitchError == twitchAuthLossMessage(twitchLossNoLoginCookie) {
		t.Error("the mark's stale reason survived a credential change — the operator is told to fix a login row while the real answer is a rejected token")
	}
}

// TestTwitchMarkFiresRecoveryOncePerLoss is R2.
//
// Three mutations. Dropping `rs.prevTwitchAuth = false` after the fire (every
// later downgrade on the same dead pair fires again, and with auto_enabled off
// that is a TypeError notification per refusal). Dropping the
// shouldFireRecovery gate entirely (recovery fires for a platform nobody
// configured). And `rs.prevTwitchAuth = twAuth` at refresh's Twitch baseline
// advance, which the doRefresh BETWEEN the two marks below exists for: under
// it a routine tick re-arms the baseline to validate's 200, so the very next
// downgrade on the same unrepaired pair reads as a fresh witnessed fall and
// alarms again — an alarm every half hour, forever, for one loss.
func TestTwitchMarkFiresRecoveryOncePerLoss(t *testing.T) {
	rs, path := twitchMarkFixture(t, "test-token-aaaa", "", http.StatusOK)
	var fired []string
	rs.OnRecoveryNeeded = func(platform string) { fired = append(fired, platform) }

	// A first conclusive check, so the baseline is "authenticated" and the next
	// loss is a WITNESSED transition rather than the startup case.
	rs.doRefresh(context.Background())
	if len(fired) != 0 {
		t.Fatalf("recovery fired %v on a healthy first pass", fired)
	}

	rs.NoteTwitchAuthLoss(twitchLossNoLoginCookie)
	// A periodic tick lands while the mark stands, and validate answers 200
	// for this pair because it cannot see the missing login row. Nothing was
	// repaired, so this pass must leave the baseline exactly where the mark
	// put it.
	rs.doRefresh(context.Background())
	rs.NoteTwitchAuthLoss(twitchLossLoginRefused)
	if len(fired) != 1 || fired[0] != "twitch" {
		t.Fatalf("recovery fired %v, want exactly one [twitch] for one loss", fired)
	}

	// A repair, then a second loss: a NEW loss must be reported.
	writeTwitchPair(t, path, "test-token-bbbb", "archiveraccount")
	rs.doRefresh(context.Background())
	rs.NoteTwitchAuthLoss(twitchLossLoginRefused)
	if len(fired) != 2 || fired[1] != "twitch" {
		t.Errorf("recovery fired %v, want a second [twitch] after the credentials were repaired and lost again", fired)
	}
}

// TestATwitchMarkBeforeAnyPassIsThatPlatformsFirstConclusion is R2's startup
// half, and it is the one shouldFireRecovery cannot get right on its own.
//
// A downgrade can land before the first refresh pass ever concludes — the IRC
// handshake happens the moment a job starts, and the service's opening check
// may still be in flight. That mark IS Twitch's first conclusive answer, so it
// must both fire once and count as the platform's conclusion; the pass that
// follows is then a "subsequent" check with an unchanged, still-false baseline
// and has nothing new to report.
//
// The mutation: dropping `rs.twEverConcluded = true` from the mark. The mark
// still fires (startup case, cookies present), but the platform stays
// "never concluded", so the next pass takes the startup branch too and alarms
// a second time for the same loss. Nothing in the first round touched that
// line: the other recovery tests all run a healthy pass FIRST, which sets the
// flag by the other route and hides it.
func TestATwitchMarkBeforeAnyPassIsThatPlatformsFirstConclusion(t *testing.T) {
	rs, _ := twitchMarkFixture(t, "test-token-aaaa", "", http.StatusUnauthorized)
	var fired []string
	rs.OnRecoveryNeeded = func(platform string) { fired = append(fired, platform) }

	rs.NoteTwitchAuthLoss(twitchLossNoLoginCookie)
	if len(fired) != 1 || fired[0] != "twitch" {
		t.Fatalf("recovery fired %v for a downgrade that beat the first pass, want exactly one [twitch]", fired)
	}

	// The first pass now lands and reaches its own conclusive negative — a 401
	// this time, so the answer is genuinely dead credentials and not the mark
	// speaking. Same loss, no second alarm.
	rs.doRefresh(context.Background())
	if len(fired) != 1 {
		t.Errorf("recovery fired %v, want one — the mark was this platform's first conclusion and the pass that follows it is not a second loss", fired)
	}
}

// TestTwitchMarkNeverFiresRecoveryForAnUnconfiguredPlatform: the false-alarm
// guard. A mark that ignored cookiesPresent would send someone to re-export
// credentials they never had — and in a container the remedy it names may not
// even be reachable.
//
// The mutation: passing `true` for cookiesPresent, or dropping the argument.
func TestTwitchMarkNeverFiresRecoveryForAnUnconfiguredPlatform(t *testing.T) {
	pointTwitchValidateAt(t, statusServer(t, http.StatusUnauthorized))
	ytSrv, _ := countingGuide(t, loggedOutGuideBody)
	pointYouTubeGuideAt(t, ytSrv)

	rs := NewRefreshService(NewCookieJar(), 0, nopLogger{})
	var fired []string
	rs.OnRecoveryNeeded = func(platform string) { fired = append(fired, platform) }

	rs.NoteTwitchAuthLoss(twitchLossLoginRefused)

	if len(fired) != 0 {
		t.Errorf("recovery fired %v on an empty jar", fired)
	}
}

// TestTwitchAuthLossReasonIsTheVocabularyOnly is the leak barrier.
//
// AuthStatus.TwitchError reaches two per-request operator surfaces
// (routes.TwitchAuthStatusPayload's `twitchError` and the TUI's R C result
// line). Because every arm of twitchAuthLossMessage returns a string LITERAL,
// the set of strings that field can hold is fixed at compile time and no
// caller can widen it.
//
// The mutation: `return reason`, or any fmt.Sprintf that interpolates it. Both
// pass a "the status says something" assertion, and both put caller-controlled
// text — one upstream change away from a value read off the wire — in front of
// an operator.
func TestTwitchAuthLossReasonIsTheVocabularyOnly(t *testing.T) {
	known := []string{
		twitchLossLoginRefused,
		twitchLossLoginUnacknowledged,
		twitchLossNoLoginCookie,
		twitchLossUnusableLoginCookie,
		twitchLossPlaybackTokenAnonymous,
	}
	seen := map[string]string{}
	generic := twitchAuthLossMessage("a-token-no-arm-was-ever-written-for")
	for _, reason := range known {
		msg := twitchAuthLossMessage(reason)
		if msg == generic {
			t.Errorf("%q renders the fallback sentence — it has no arm of its own", reason)
		}
		if prev, dup := seen[msg]; dup {
			t.Errorf("%q and %q render the same sentence %q", prev, reason, msg)
		}
		seen[msg] = reason
		if strings.Contains(msg, reason) {
			t.Errorf("the sentence for %q contains the raw token: %q", reason, msg)
		}
	}

	// An off-vocabulary input, shaped like the thing that must never reach this
	// function, must render the fallback and carry none of its input.
	leaky := "auth-token=test-token-aaaa; login=archiveraccount"
	if msg := twitchAuthLossMessage(leaky); msg != generic || strings.Contains(msg, "test-token-aaaa") {
		t.Errorf("an off-vocabulary reason rendered %q — the switch is not the barrier it claims to be", msg)
	}

	rs, _ := twitchMarkFixture(t, "test-token-aaaa", "archiveraccount", http.StatusOK)
	rs.NoteTwitchAuthLoss(leaky)
	if got := rs.GetStatus().TwitchError; got != generic {
		t.Errorf("AuthStatus.TwitchError = %q after an off-vocabulary mark, want the fallback sentence", got)
	}
}

// TestTwitchMarkLeavesYouTubeAlone: the mark writes THREE fields, not a whole
// AuthStatus.
//
// The mutation: `rs.status = AuthStatus{TwitchAuthenticated: false, ...}`,
// which silently zeroes YouTube's verdict, its reason and its cookies-present
// flag — so a Twitch chat downgrade repaints the YouTube badge.
func TestTwitchMarkLeavesYouTubeAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.txt")
	content := "# Netscape HTTP Cookie File\n" +
		".youtube.com\tTRUE\t/\tTRUE\t0\tSAPISID\tsapisid-value\n" +
		".youtube.com\tTRUE\t/\tTRUE\t0\tLOGIN_INFO\tlogin-info-value\n" +
		"#HttpOnly_.twitch.tv\tTRUE\t/\tTRUE\t0\tauth-token\ttest-token-aaaa\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	pointTwitchValidateAt(t, statusServer(t, http.StatusOK))
	ytSrv, _ := countingGuide(t, loggedInGuideBody)
	pointYouTubeGuideAt(t, ytSrv)

	rs := NewRefreshService(jar, 0, nopLogger{})
	rs.doRefresh(context.Background())
	before := rs.GetStatus()
	if !before.YouTubeAuthenticated {
		t.Fatal("the fixture is wrong: YouTube did not authenticate")
	}

	rs.NoteTwitchAuthLoss(twitchLossNoLoginCookie)

	after := rs.GetStatus()
	if after.YouTubeAuthenticated != before.YouTubeAuthenticated ||
		after.YouTubeVerification != before.YouTubeVerification ||
		after.YouTubeError != before.YouTubeError ||
		after.HasYouTubeCookies != before.HasYouTubeCookies {
		t.Errorf("a Twitch mark moved the YouTube half of AuthStatus: before %+v, after %+v", before, after)
	}
}

// TestTwitchMarkFiresAuthChangeOnAVerdictTransitionOnly pins the mark to
// authStatusChanged's existing CONTRACT rather than to a second rule.
//
// authStatusChanged deliberately excludes the two reason strings, because no
// OnAuthChange-driven surface may render them. A second mark that changes only
// the REASON must therefore fire no push; the per-request surfaces read the
// string fresh anyway.
//
// Two mutations. Calling OnAuthChange unconditionally from
// NoteTwitchAuthLoss — not merely noisy, it repaints two dashboards for an
// event neither displays. And `statusCopy = prev` instead of the post-mark
// status: the count is unchanged, so a push-counting test passes while every
// subscriber is handed the status the mark just replaced. That is worse than
// no push at all — the dashboard repaints itself green on the strength of the
// event that says it went red.
func TestTwitchMarkFiresAuthChangeOnAVerdictTransitionOnly(t *testing.T) {
	rs, _ := twitchMarkFixture(t, "test-token-aaaa", "archiveraccount", http.StatusOK)
	var pushes int
	var pushed AuthStatus
	rs.OnAuthChange = func(s AuthStatus) { pushes++; pushed = s }

	rs.doRefresh(context.Background()) // twitch reads authenticated
	before := rs.GetStatus()
	pushes = 0

	rs.NoteTwitchAuthLoss(twitchLossLoginRefused)
	if pushes != 1 {
		t.Fatalf("OnAuthChange fired %d times for the authenticated -> marked transition, want 1", pushes)
	}

	// The PAYLOAD, not just the count. Every subscriber renders this value and
	// none of them re-reads GetStatus first.
	if pushed.TwitchAuthenticated {
		t.Error("the pushed status says Twitch is authenticated — subscribers were handed the pre-mark status")
	}
	if pushed.TwitchVerification != RefreshFailed {
		t.Errorf("pushed TwitchVerification = %v, want RefreshFailed", pushed.TwitchVerification)
	}
	if want := twitchAuthLossMessage(twitchLossLoginRefused); pushed.TwitchError != want {
		t.Errorf("pushed TwitchError = %q, want %q", pushed.TwitchError, want)
	}
	if pushed.HasTwitchCookies != before.HasTwitchCookies {
		t.Errorf("pushed HasTwitchCookies = %v, want %v — a downgrade does not unconfigure the platform", pushed.HasTwitchCookies, before.HasTwitchCookies)
	}

	rs.NoteTwitchAuthLoss(twitchLossNoLoginCookie)
	if pushes != 1 {
		t.Errorf("OnAuthChange fired %d times total; a reason-only change must fire no push (authStatusChanged excludes the strings)", pushes)
	}
}

// TestTwitchAuthLossWarnCarriesTheMappedSentenceOnly closes the hole the
// comment above that Warn line used to claim was already closed.
//
// TestTwitchAuthLossReasonIsTheVocabularyOnly drives the STATUS field and its
// fixture logger is nopLogger, so the log line was observed by nothing: a Warn
// that printed the caller's raw argument passed all eight tests. The status
// field is not the only place a reason escapes to — the log file is read by
// the same operator and travels in the same bug report.
//
// The mutation: `"reason", reason` in place of
// `"reason", twitchAuthLossMessage(reason)`.
func TestTwitchAuthLossWarnCarriesTheMappedSentenceOnly(t *testing.T) {
	rs, _ := twitchMarkFixture(t, "test-token-aaaa", "archiveraccount", http.StatusOK)
	rec := &markWarnRecorder{}
	// Assigned rather than threaded through twitchMarkFixture: `logger` is
	// this package's own unexported field, the fixture is shared by nine
	// tests, and swapping it here changes nothing for any of them.
	rs.logger = rec
	rs.OnRecoveryNeeded = func(string) {}

	// Off-vocabulary AND credential-shaped, which is the only input that can
	// separate the two implementations: for a KNOWN reason the mapped sentence
	// and the raw token differ, but neither of them is a secret, so a leaky
	// Warn would look identical to a correct one.
	leaky := "auth-token=test-token-aaaa; login=archiveraccount"
	rs.NoteTwitchAuthLoss(leaky)

	lines := rec.warnLines()
	if len(lines) != 1 {
		t.Fatalf("Warn lines = %d, want exactly 1 for one recovery fire", len(lines))
	}
	if !strings.Contains(lines[0], twitchAuthLossMessage(leaky)) {
		t.Error("the Warn line does not carry the mapped sentence")
	}
	if strings.Contains(lines[0], leaky) || strings.Contains(lines[0], "test-token-aaaa") {
		t.Error("the Warn line carried the caller's raw reason — the switch is the barrier for AuthStatus.TwitchError only, and this line goes to the same operator")
	}
}
