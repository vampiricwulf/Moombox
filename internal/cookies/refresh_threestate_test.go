package cookies

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// jarWithBothPlatforms loads a jar configured for YouTube AND Twitch, so one
// doRefresh exercises both halves of AuthStatus at once.
//
// The values are synthetic placeholders; nothing here reads a real cookie
// store. What matters is only which NAMES are present, because that is all the
// has-any predicates look at.
func jarWithBothPlatforms(t *testing.T) *CookieJar {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	content := "# Netscape HTTP Cookie File\n" +
		".youtube.com\tTRUE\t/\tTRUE\t0\tSAPISID\tsapisid-value\n" +
		".youtube.com\tTRUE\t/\tTRUE\t0\tLOGIN_INFO\tlogin-info-value\n" +
		"#HttpOnly_.twitch.tv\tTRUE\t/\tTRUE\t0\tauth-token\tauth-token-value\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	return jar
}

// codedBodyServer serves one fixed status code with one fixed body. The
// sibling helpers each pin only half of that — bodyServer always answers 200,
// statusServer always answers empty — and the rows below need both at once,
// because a 503 and a 200-carrying-a-logged-out-marker are the two outcomes
// whose difference this file exists to assert.
func codedBodyServer(t *testing.T, code int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestAuthStatusSeparatesCouldNotCheckFromRejected is the whole point of the
// verdict fields, asserted where they are produced.
//
// Before them, the "rate limited" row and the "rejected" row below were the
// SAME AuthStatus — both booleans false, the reason parked in YouTubeError,
// which nothing reads. Every surface consequently rendered a 503 as a red
// "not authenticated" badge, which is a conclusion the check did not reach and
// which sends an operator off to re-export credentials that are fine.
//
// The authenticated booleans are asserted on every row, not only where they
// move, because their meaning must NOT change: they stay "can we do
// authenticated work right now", false on an inconclusive check, and they are
// what every pre-existing consumer reads.
func TestAuthStatusSeparatesCouldNotCheckFromRejected(t *testing.T) {
	for _, tc := range []struct {
		name       string
		guideBody  string
		guideCode  int
		twitchCode int
		wantYT     RefreshVerdict
		wantTW     RefreshVerdict
		wantYTAuth bool
		wantTWAuth bool
	}{
		{
			name:      "both sites answered and both sessions are alive",
			guideBody: loggedInGuideBody, guideCode: http.StatusOK,
			twitchCode: http.StatusOK,
			wantYT:     RefreshOK, wantTW: RefreshOK,
			wantYTAuth: true, wantTWAuth: true,
		},
		{
			// Conclusive. This is the row that earns the red badge.
			name:      "both sites answered and both sessions are dead",
			guideBody: loggedOutGuideBody, guideCode: http.StatusOK,
			twitchCode: http.StatusUnauthorized,
			wantYT:     RefreshFailed, wantTW: RefreshFailed,
		},
		{
			// THE ROW THE OLD SHAPE COULD NOT EXPRESS. Identical booleans to
			// the row above, and it means something completely different.
			name:      "neither site could be asked",
			guideBody: "", guideCode: http.StatusServiceUnavailable,
			twitchCode: http.StatusServiceUnavailable,
			wantYT:     RefreshUnknown, wantTW: RefreshUnknown,
		},
		{
			// The verdicts are per-platform, so a healthy sibling must not
			// colour the other one — the same conflation RefreshResult was
			// split up to stop.
			name:      "one alive, one unreachable",
			guideBody: loggedInGuideBody, guideCode: http.StatusOK,
			twitchCode: http.StatusTooManyRequests,
			wantYT:     RefreshOK, wantTW: RefreshUnknown,
			wantYTAuth: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			guide := codedBodyServer(t, tc.guideCode, tc.guideBody)
			pointYouTubeGuideAt(t, guide)
			pointTwitchValidateAt(t, statusServer(t, tc.twitchCode))

			rs := NewRefreshService(jarWithBothPlatforms(t), 0, nopLogger{})
			rs.doRefresh(context.Background())
			got := rs.GetStatus()

			if got.YouTubeVerification != tc.wantYT {
				t.Errorf("YouTubeVerification = %v, want %v — %q", got.YouTubeVerification, tc.wantYT, tc.name)
			}
			if got.TwitchVerification != tc.wantTW {
				t.Errorf("TwitchVerification = %v, want %v — %q", got.TwitchVerification, tc.wantTW, tc.name)
			}
			if got.YouTubeAuthenticated != tc.wantYTAuth {
				t.Errorf("YouTubeAuthenticated = %v, want %v — its meaning must not change",
					got.YouTubeAuthenticated, tc.wantYTAuth)
			}
			if got.TwitchAuthenticated != tc.wantTWAuth {
				t.Errorf("TwitchAuthenticated = %v, want %v — its meaning must not change",
					got.TwitchAuthenticated, tc.wantTWAuth)
			}
			// Both platforms are configured on every row, so the presence
			// flags never move. A verdict that "explained" itself by claiming
			// the cookies vanished would be a different lie.
			if !got.HasYouTubeCookies || !got.HasTwitchCookies {
				t.Errorf("presence flags = yt:%v tw:%v, want both true — the jar holds both platforms",
					got.HasYouTubeCookies, got.HasTwitchCookies)
			}
		})
	}
}

// TestHasTwitchCookiesUsesTheLoosePredicate is V5's trap, asserted rather than
// asserted-about.
//
// The jar here holds twilight-user and NO auth-token: a Twitch session that was
// plainly configured and now has no credential (the jar ignores expiry,
// mergeCookieFiles prunes on it — see twitchAuthCookieNames). The strict
// predicate, HasTwitchAuthCookies, is FALSE for it. Wiring HasTwitchCookies to
// that one would report this install as never-configured, which is the exact
// V2-class conflation the YouTube side was re-pointed away from in Arc 1 — and
// the badge would say "Anonymous" about a broken sign-in.
//
// The assertion below therefore pins both halves: the loose predicate says yes,
// the strict one says no, and the field follows the loose one.
func TestHasTwitchCookiesUsesTheLoosePredicate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	content := "# Netscape HTTP Cookie File\n" +
		".twitch.tv\tTRUE\t/\tTRUE\t0\ttwilight-user\t%7B%22id%22%3A%221%22%7D\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}

	if jar.HasTwitchAuthCookies() {
		t.Fatal("premise lost: this jar is supposed to hold no bearer token, so the STRICT " +
			"predicate must be false — otherwise the two predicates agree and this test proves nothing")
	}
	if !jar.HasAnyTwitchAuthCookie() {
		t.Fatal("premise lost: the loose predicate must see twilight-user")
	}

	// No bearer token means checkTwitchAuth returns without a request, but pin
	// both seams anyway: an unpinned seam is one refactor from a live call.
	pointYouTubeGuideAt(t, codedBodyServer(t, http.StatusOK, loggedOutGuideBody))
	pointTwitchValidateAt(t, statusServer(t, http.StatusUnauthorized))

	rs := NewRefreshService(jar, 0, nopLogger{})
	rs.doRefresh(context.Background())
	got := rs.GetStatus()

	if !got.HasTwitchCookies {
		t.Error("HasTwitchCookies = false for a Twitch session that holds twilight-user. " +
			"Fed the strict predicate, a session whose auth-token was pruned on expiry reports " +
			"as never-configured, and the badge says \"Anonymous\" about a broken sign-in")
	}
	// The pair that makes the un-dead-coded CookiesOnly badge reachable:
	// configured, and conclusively not authenticated.
	if got.TwitchVerification != RefreshFailed {
		t.Errorf("TwitchVerification = %v, want failed — no bearer token is a CONCLUSION, "+
			"not an unreachable site", got.TwitchVerification)
	}
	if got.HasYouTubeCookies {
		t.Error("HasYouTubeCookies = true for a jar holding only Twitch cookies")
	}
}

// TestVerdictFromCheckTreatsAnErrorAsAnAbsenceOfFindings pins the projection
// every surface's copy is derived from.
//
// The `false, err` row is the one that matters: err means "this check learned
// nothing" — a non-200, a redirected answer, an unreadable body — and mapping
// it to RefreshFailed would put the conclusive word back on a check that never
// reached one, undoing the whole change one function upstream of every badge.
func TestVerdictFromCheckTreatsAnErrorAsAnAbsenceOfFindings(t *testing.T) {
	boom := errors.New("dial tcp: lookup youtube.com: no such host")
	for _, tc := range []struct {
		name string
		auth bool
		err  error
		want RefreshVerdict
	}{
		{"reached the site, session alive", true, nil, RefreshOK},
		{"reached the site, session dead", false, nil, RefreshFailed},
		{"could not reach the site", false, boom, RefreshUnknown},
		{
			// Defensive: an error outranks the bool, so a check that set both
			// cannot be read as a healthy session.
			"errored while claiming success", true, boom, RefreshUnknown,
		},
		{
			// THE SENTINEL, and it is a FINDING rather than an absence of one.
			// The jar is configured and still cannot sign a request; no
			// waiting fixes that, only a re-export. Classified as unknown it
			// rendered as "could not establish" and then dropped off the
			// narrow TUI bar, where it used to be an always-visible alarm —
			// this arc's own defect class running backwards.
			"configured but can never sign a request",
			false, ErrAuthCheckNotAttempted, RefreshFailed,
		},
		{
			// Both production sites raise it through fmt.Errorf("...: %w"),
			// so a bare == comparison would classify every real occurrence as
			// unknown while this table stayed green on the unwrapped one.
			"the sentinel as production actually wraps it",
			false,
			fmt.Errorf("youtube auth check: no SAPISIDHASH could be generated: %w", ErrAuthCheckNotAttempted),
			RefreshFailed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := verdictFromCheck(tc.auth, tc.err); got != tc.want {
				t.Errorf("verdictFromCheck(%v, %v) = %v, want %v", tc.auth, tc.err, got, tc.want)
			}
		})
	}
}

// TestUnsignableJarIsReportedAsAFailureNotAnUnknown carries the sentinel
// through the real pipeline, from the jar to the field a badge reads.
//
// The jar below is the state the sentinel exists for and it is not exotic:
// LOGIN_INFO survives while the whole SAPISID family is gone, which
// HasAnyYouTubeAuthCookie counts as CONFIGURED (LOGIN_INFO is in
// youtubeAuthCookieNames) and which GenerateAuthorizationHeader cannot sign
// with. yt-dlp documents YouTube doing exactly that on rotation.
//
// Two halves, and the second is the one that makes the first safe:
//
//   - PRESENTATION moved: the verdict is now RefreshFailed, so every surface
//     words it conclusively and the TUI badge is the red always-visible one
//     again.
//   - DECISION did not: the sentinel is still an error to shouldFireRecovery,
//     which is why no recovery fires here. It must not — a browser refresh
//     cannot repair a jar that has lost its SAPISID family, so firing would
//     spend a two-minute timeout being told no, and would report an auth loss
//     for a structural fault. `ytEverConcluded` staying false is the same
//     property one level down: an inconclusive check must not license the
//     next one to treat itself as a subsequent check.
//
// The premise assertions at the top are not decoration. If this jar stopped
// producing the sentinel — a broadened predicate, a changed header builder —
// every assertion below would still pass while testing an ordinary
// conclusive-negative, which is the failure mode this whole file exists to
// prevent.
func TestUnsignableJarIsReportedAsAFailureNotAnUnknown(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	content := "# Netscape HTTP Cookie File\n" +
		".youtube.com\tTRUE\t/\tTRUE\t0\tLOGIN_INFO\tlogin-info-value\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}

	if !jar.HasAnyYouTubeAuthCookie() {
		t.Fatal("premise lost: this jar must read as CONFIGURED, or the state under test " +
			"(configured and unsignable) does not exist")
	}
	if jar.GenerateAuthorizationHeader("https://www.youtube.com") != "" {
		t.Fatal("premise lost: this jar can sign a request, so no sentinel is raised and the " +
			"rows below would be testing an ordinary conclusive negative")
	}

	// No request can leave the process on this path, but pin both seams: an
	// unpinned seam is one refactor from a live call to youtube.com.
	pointYouTubeGuideAt(t, codedBodyServer(t, http.StatusOK, loggedInGuideBody))
	pointTwitchValidateAt(t, statusServer(t, http.StatusUnauthorized))

	rs := NewRefreshService(jar, 0, nopLogger{})
	var fired []string
	rs.OnRecoveryNeeded = func(platform string) { fired = append(fired, platform) }
	rs.doRefresh(context.Background())
	got := rs.GetStatus()

	// The premise for the whole split: the check really did fail structurally
	// rather than conclude anything, so the error is still recorded.
	if got.YouTubeError == "" {
		t.Fatal("premise lost: no error was recorded, so this check concluded normally and " +
			"the sentinel is not what produced the verdict below")
	}

	if got.YouTubeVerification != RefreshFailed {
		t.Errorf("YouTubeVerification = %v, want failed. A jar that is configured and can "+
			"NEVER sign a request is a permanent, actionable failure — reporting it as "+
			"\"could not establish\" hedges about the one state where hedging leaves the "+
			"user unwarned", got.YouTubeVerification)
	}
	if got.YouTubeAuthenticated {
		t.Error("YouTubeAuthenticated = true for a jar that cannot sign a request")
	}
	if !got.HasYouTubeCookies {
		t.Error("HasYouTubeCookies = false — without it the badge reads \"no cookies\" and the " +
			"verdict never reaches a surface at all")
	}

	// THE HALF THAT MUST NOT HAVE MOVED. The verdict is presentation; the
	// error is what drives behaviour, and it is still inconclusive there.
	if len(fired) != 0 {
		t.Errorf("OnRecoveryNeeded fired %v. The verdict split is presentational — the sentinel "+
			"must stay an ERROR to shouldFireRecovery, or a jar no browser refresh can repair "+
			"starts triggering recovery passes", fired)
	}
	rs.mu.RLock()
	concluded, prevAuth := rs.ytEverConcluded, rs.prevYouTubeAuth
	rs.mu.RUnlock()
	if concluded {
		t.Error("ytEverConcluded = true after a check that never formed a request — the " +
			"prev-state machinery is reading the verdict instead of the error")
	}
	if prevAuth {
		t.Error("prevYouTubeAuth moved on a check that never formed a request")
	}
}

// TestRecheckReportOnlySaysFailedWhenItIs owns the sentence BOTH UIs render
// for a manual recheck. The TUI's R C chord and the Web dashboard's refresh
// button are the same gesture, and before this they answered it differently —
// which is the drift RefreshDeclinedCauses was exported to stop, one surface
// over.
//
// The invariant at the bottom is the real guard, asserted on every row rather
// than on the two that expect it: the conclusive wording appears if and only
// if the verdict was conclusive. A fourth verdict added later cannot inherit
// it silently.
func TestRecheckReportOnlySaysFailedWhenItIs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		platforms []RecheckedPlatform
		want      string
	}{
		{
			name: "nothing is configured", want: "Cookies: no platforms configured",
		},
		{
			name:      "one platform, alive",
			platforms: []RecheckedPlatform{{"YouTube", RefreshOK}},
			want:      "Cookies: YouTube OK",
		},
		{
			name:      "one platform, conclusively rejected",
			platforms: []RecheckedPlatform{{"YouTube", RefreshFailed}},
			want:      "Cookies: YouTube not authenticated",
		},
		{
			// The row the two-arm copy could not express on either surface.
			name:      "one platform, nothing concluded",
			platforms: []RecheckedPlatform{{"YouTube", RefreshUnknown}},
			want:      "Cookies: YouTube — could not establish",
		},
		{
			// Per-platform, so a healthy sibling cannot colour the other one.
			name:      "both, disagreeing",
			platforms: []RecheckedPlatform{{"YouTube", RefreshOK}, {"Twitch", RefreshUnknown}},
			want:      "Cookies: YouTube OK, Twitch — could not establish",
		},
		{
			name:      "both, both dead",
			platforms: []RecheckedPlatform{{"YouTube", RefreshFailed}, {"Twitch", RefreshFailed}},
			want:      "Cookies: YouTube not authenticated, Twitch not authenticated",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := RecheckReport(tc.platforms...)
			if got != tc.want {
				t.Errorf("RecheckReport = %q, want %q", got, tc.want)
			}

			// THE INVARIANT. "not authenticated" is a claim about the
			// credentials and only a conclusive check may make it.
			conclusive := false
			for _, p := range tc.platforms {
				if p.Verdict == RefreshFailed {
					conclusive = true
				}
			}
			if said := strings.Contains(got, "not authenticated"); said != conclusive {
				t.Errorf("says \"not authenticated\" = %v, want %v: %q — that wording is a "+
					"finding about the credentials, and a check that reached no conclusion "+
					"has not made one", said, conclusive, got)
			}
		})
	}
}

// TestAuthStatusChangedGateCoversEverySurfaceInput is the OnAuthChange
// predicate, which is where a presentational change turns into a callback.
//
// The gate compared the two booleans only. That was sufficient while the
// booleans were the only thing rendered; it is not now, and the two silent
// transitions below are exactly the ones this arc adds:
//
//   - rejected → could-not-check leaves both booleans false. The badge has to
//     go from red to hedged and nothing would have told it.
//   - never-configured → configured-but-rejected leaves TwitchAuthenticated
//     false. That is the transition into the CookiesOnly arm this arc
//     un-dead-codes; gated on the booleans it would light only when some
//     unrelated flip happened to fire the callback.
//
// The two negative rows are the reason the gate is not simply `prev != next`.
// LastCheck moves on EVERY tick, so including it fires the callback
// unconditionally and the gate stops being a gate; the error strings vary in
// wording between two occurrences of one outcome, and nothing renders them.
// Both rows fail if either field is added to authStatusChanged.
func TestAuthStatusChangedGateCoversEverySurfaceInput(t *testing.T) {
	base := AuthStatus{
		YouTubeAuthenticated: false,
		TwitchAuthenticated:  false,
		HasYouTubeCookies:    true,
		HasTwitchCookies:     false,
		YouTubeVerification:  RefreshFailed,
		TwitchVerification:   RefreshFailed,
		LastCheck:            "2026-08-28T10:00:00Z",
	}
	with := func(mutate func(*AuthStatus)) AuthStatus {
		next := base
		mutate(&next)
		return next
	}

	for _, tc := range []struct {
		name string
		next AuthStatus
		want bool
	}{
		{"nothing moved", base, false},
		{
			"youtube signed back in",
			with(func(s *AuthStatus) { s.YouTubeAuthenticated = true; s.YouTubeVerification = RefreshOK }),
			true,
		},
		{
			"youtube went from rejected to could-not-check",
			with(func(s *AuthStatus) { s.YouTubeVerification = RefreshUnknown }),
			true,
		},
		{
			"twitch went from rejected to could-not-check",
			with(func(s *AuthStatus) { s.TwitchVerification = RefreshUnknown }),
			true,
		},
		{
			"twitch became configured",
			with(func(s *AuthStatus) { s.HasTwitchCookies = true }),
			true,
		},
		{
			"youtube cookies disappeared",
			with(func(s *AuthStatus) { s.HasYouTubeCookies = false }),
			true,
		},
		{
			"only the clock moved",
			with(func(s *AuthStatus) { s.LastCheck = "2026-08-28T10:30:00Z" }),
			false,
		},
		{
			"only the error wording changed",
			with(func(s *AuthStatus) { s.YouTubeError = "unexpected status 503"; s.TwitchError = "i/o timeout" }),
			false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := authStatusChanged(base, tc.next); got != tc.want {
				t.Errorf("authStatusChanged = %v, want %v for %q", got, tc.want, tc.name)
			}
		})
	}
}
