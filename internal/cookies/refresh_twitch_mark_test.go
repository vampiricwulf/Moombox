package cookies

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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

// TestValidate200DoesNotClearAStandingTwitchMark is R3, and the whole point of
// the arc.
//
// The mutation: deleting the mark consult from refresh's status block, so the
// block writes verdictFromCheck(twAuth=true, nil) = RefreshOK straight over the
// mark. Under it the operator sees green within one tick while the capture is
// still dropping every subscriber-only message.
func TestValidate200DoesNotClearAStandingTwitchMark(t *testing.T) {
	rs, _ := twitchMarkFixture(t, "test-token-aaaa", "", http.StatusOK)

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
// Two mutations: dropping `rs.prevTwitchAuth = false` after the fire (every
// later downgrade on the same dead pair fires again, and with auto_enabled off
// that is a TypeError notification per refusal), and dropping the
// shouldFireRecovery gate entirely (recovery fires for a platform nobody
// configured).
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
	rs.NoteTwitchAuthLoss(twitchLossLoginRefused)
	if len(fired) != 1 || fired[0] != "twitch" {
		t.Fatalf("recovery fired %v, want exactly one [twitch] for one loss", fired)
	}

	// A repair, then a second loss: a NEW loss must be reported.
	writeTwitchPair(t, path, "test-token-bbbb", "archiveraccount")
	rs.doRefresh(context.Background())
	rs.NoteTwitchAuthLoss(twitchLossLoginRefused)
	if len(fired) != 2 {
		t.Errorf("recovery fired %v, want a second fire after the credentials were repaired and lost again", fired)
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
// The mutation: calling OnAuthChange unconditionally from NoteTwitchAuthLoss.
// Not merely noisy — it repaints two dashboards for an event neither displays.
func TestTwitchMarkFiresAuthChangeOnAVerdictTransitionOnly(t *testing.T) {
	rs, _ := twitchMarkFixture(t, "test-token-aaaa", "archiveraccount", http.StatusOK)
	var pushes int
	rs.OnAuthChange = func(AuthStatus) { pushes++ }

	rs.doRefresh(context.Background()) // twitch reads authenticated
	pushes = 0

	rs.NoteTwitchAuthLoss(twitchLossLoginRefused)
	if pushes != 1 {
		t.Fatalf("OnAuthChange fired %d times for the authenticated -> marked transition, want 1", pushes)
	}
	rs.NoteTwitchAuthLoss(twitchLossNoLoginCookie)
	if pushes != 1 {
		t.Errorf("OnAuthChange fired %d times total; a reason-only change must fire no push (authStatusChanged excludes the strings)", pushes)
	}
}
