package cookies

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// withFreshBrowserDetectCache snapshots every field of the package-global
// browserDetectCache, resets both slots to "never scanned", and restores the
// original values on cleanup. Required because browserDetectCache is shared
// by every test in this package — without save/restore, a test that forces a
// scan or an expiry would leak state into whichever test runs next.
func withFreshBrowserDetectCache(t *testing.T) {
	t.Helper()
	browserDetectCache.mu.Lock()
	prevBrowser := browserDetectCache.browser
	prevChecked := browserDetectCache.checked
	prevExpires := browserDetectCache.expires
	prevAvailable := browserDetectCache.available
	prevAvailChecked := browserDetectCache.availableChecked
	prevAvailExpires := browserDetectCache.availableExpires
	browserDetectCache.browser = nil
	browserDetectCache.checked = false
	browserDetectCache.expires = time.Time{}
	browserDetectCache.available = nil
	browserDetectCache.availableChecked = false
	browserDetectCache.availableExpires = time.Time{}
	browserDetectCache.mu.Unlock()

	t.Cleanup(func() {
		browserDetectCache.mu.Lock()
		browserDetectCache.browser = prevBrowser
		browserDetectCache.checked = prevChecked
		browserDetectCache.expires = prevExpires
		browserDetectCache.available = prevAvailable
		browserDetectCache.availableChecked = prevAvailChecked
		browserDetectCache.availableExpires = prevAvailExpires
		browserDetectCache.mu.Unlock()
	})
}

// stubDetectors swaps both uncached scanner seams for counting stubs that do
// no filesystem or registry I/O at all, and restores the real ones on
// cleanup. Returns a pointer to the shared invocation counter.
func stubDetectors(t *testing.T, browser *DetectedBrowser, available []DetectedBrowser) *int {
	t.Helper()
	calls := 0
	prevSingle, prevList := detectBrowserUncached, detectBrowsersUncached
	detectBrowserUncached = func() *DetectedBrowser {
		calls++
		return browser
	}
	detectBrowsersUncached = func() []DetectedBrowser {
		calls++
		return available
	}
	t.Cleanup(func() {
		detectBrowserUncached, detectBrowsersUncached = prevSingle, prevList
	})
	return &calls
}

// TestReloginStatusPerformsNoBrowserDetection is H5's core claim: the four
// highest-frequency GetStatus callers moved to ReloginStatus specifically so
// they stop paying for DetectBrowser/DetectBrowsers on every poll.
//
// Mutation check: put either detector call back into ReloginStatus and this
// fails — calls goes from 0 to non-zero across the loop below.
func TestReloginStatusPerformsNoBrowserDetection(t *testing.T) {
	withFreshBrowserDetectCache(t)
	calls := stubDetectors(t, nil, nil)

	s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})
	for i := 0; i < 5; i++ {
		s.ReloginStatus()
	}

	if *calls != 0 {
		t.Fatalf("ReloginStatus triggered %d browser-detection scans across 5 calls, want 0 — "+
			"GetStatus is the only method allowed to call DetectBrowser/DetectBrowsers", *calls)
	}
	browserDetectCache.mu.Lock()
	stillUnscanned := !browserDetectCache.checked && !browserDetectCache.availableChecked
	browserDetectCache.mu.Unlock()
	if !stillUnscanned {
		t.Error("the detection cache looks scanned after only ReloginStatus calls, even though the " +
			"counting seam saw zero invocations")
	}
}

// TestReloginStatusReapsAnAbandonedSetup is the trap the brief names
// explicitly: GetStatus's own doc says status polling is the most frequent
// visitor to s.mu, which is what makes it the reap that actually fires in
// production. Moving GetStatus's four highest-frequency callers to
// ReloginStatus makes THIS method that most-frequent visitor, so it must
// reap too — Arc 3's abandoned-setup fix (A1) depends on it.
//
// Isolated deliberately: only ReloginStatus is called before the assertion,
// which reads s.setupProcess directly rather than through a second GetStatus
// poll. A second poll reaping the setup would satisfy "the reap fired"
// without proving ReloginStatus did it — the junction defect the brief warns
// about by number.
//
// Mutation check: delete the reapAbandonedSetupLocked() call from
// ReloginStatus and this fails — s.setupProcess stays non-nil.
func TestReloginStatusReapsAnAbandonedSetup(t *testing.T) {
	captureKills(t)
	s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})
	abandonedSetup(t, s, setupAbandonGrace+time.Second)

	s.ReloginStatus()

	s.mu.Lock()
	stillRegistered := s.setupProcess != nil
	s.mu.Unlock()
	if stillRegistered {
		t.Fatal("ReloginStatus did not reap an abandoned setup. If the four highest-frequency " +
			"GetStatus callers move here without the reap, Arc 3's abandoned-setup fix silently " +
			"stops firing in production — no existing test would notice, because the reap still " +
			"exists on GetStatus, it would just never run there any more in practice")
	}
}

// TestReloginStatusMatchesGetStatusNeedsManualRelogin pins that the two
// methods compute the SAME field the same way — ReloginStatus is a cheaper
// accessor, not a second implementation of the relogin logic.
func TestReloginStatusMatchesGetStatusNeedsManualRelogin(t *testing.T) {
	withFreshBrowserDetectCache(t)
	stubDetectors(t, nil, []DetectedBrowser{})

	s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})
	// Written directly, under the lock the map's real writers hold. It used to
	// go through FlagManualRelogin, which was deleted in Arc 8 Task 12a as
	// exported surface with zero production callers — and a fixture is exactly
	// the wrong reason to keep a public method alive on a security-sensitive
	// service, because the next reader takes it for a wired feature.
	s.mu.Lock()
	s.needsRelogin["youtube"] = true
	s.mu.Unlock()

	relogin := s.ReloginStatus()
	full := s.GetStatus()

	if len(relogin) != len(full.NeedsManualRelogin) || relogin["youtube"] != full.NeedsManualRelogin["youtube"] ||
		relogin["twitch"] != full.NeedsManualRelogin["twitch"] {
		t.Errorf("ReloginStatus() = %v, GetStatus().NeedsManualRelogin = %v — must agree", relogin, full.NeedsManualRelogin)
	}
	if !relogin["youtube"] {
		t.Error("fixture is broken — the direct write to needsRelogin[\"youtube\"] should have set it")
	}
}

// TestGetStatusCachesAvailableBrowsersAndRescansAfterTTL is H5 part 2's core
// claim: DetectBrowsers used to rebuild the full list — filesystem I/O and a
// reg.exe spawn on Windows — on every single GetStatus call. It must now be
// cached exactly like DetectBrowser already was: same TTL, one scan per
// window, and a scan again once the window elapses.
//
// Mutation check: revert DetectBrowsers to call detectBrowsersUncached
// directly (no cache check) and this fails on the second-call assertion —
// calls goes to 2 instead of staying at 1.
func TestGetStatusCachesAvailableBrowsersAndRescansAfterTTL(t *testing.T) {
	withFreshBrowserDetectCache(t)
	stubbedList := []DetectedBrowser{{Type: "chrome", Path: "fake-chrome", Name: "Chrome"}}
	calls := stubDetectors(t, nil, stubbedList)

	s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})

	status1 := s.GetStatus()
	if *calls != 2 {
		t.Fatalf("first GetStatus(): %d detector calls, want 2 (DetectBrowser + DetectBrowsers, both cold)", *calls)
	}
	if len(status1.AvailableBrowsers) != 1 || status1.AvailableBrowsers[0].Path != "fake-chrome" {
		t.Fatalf("first GetStatus().AvailableBrowsers = %+v, want the stubbed list", status1.AvailableBrowsers)
	}

	status2 := s.GetStatus()
	if *calls != 2 {
		t.Fatalf("second GetStatus() within the TTL: %d detector calls, want still 2 — it must reuse "+
			"the cached list rather than re-scanning", *calls)
	}
	if len(status2.AvailableBrowsers) != 1 || status2.AvailableBrowsers[0].Path != "fake-chrome" {
		t.Fatalf("second GetStatus().AvailableBrowsers = %+v, want the same cached list", status2.AvailableBrowsers)
	}

	// Force both slots' TTL into the past and confirm a THIRD call re-scans
	// both — the whole point of this test (DetectBrowsers, previously
	// uncached) plus its sibling (DetectBrowser, already cached before this
	// arc) each contribute one more call.
	browserDetectCache.mu.Lock()
	browserDetectCache.expires = time.Now().Add(-time.Second)
	browserDetectCache.availableExpires = time.Now().Add(-time.Second)
	browserDetectCache.mu.Unlock()

	s.GetStatus()
	if *calls != 4 {
		t.Fatalf("GetStatus() after the TTL expired: %d detector calls, want 4 — a stale cache must "+
			"re-scan, not ride out an expired window forever", *calls)
	}
}

// TestInvalidateBrowserDetectionForcesImmediateRescan is the invalidation
// hook's own guarantee: after InvalidateBrowserDetection, the very next
// DetectBrowsers() (and DetectBrowser()) call re-scans even though the TTL
// has not elapsed.
//
// Mutation check: make InvalidateBrowserDetection a no-op and this fails —
// calls stays at 1 instead of advancing to 2.
func TestInvalidateBrowserDetectionForcesImmediateRescan(t *testing.T) {
	withFreshBrowserDetectCache(t)
	calls := stubDetectors(t, &DetectedBrowser{Type: "firefox", Path: "fake-firefox", Name: "Firefox"},
		[]DetectedBrowser{{Type: "firefox", Path: "fake-firefox", Name: "Firefox"}})

	if got := DetectBrowsers(); len(got) != 1 {
		t.Fatalf("priming DetectBrowsers() = %+v, want the stubbed one-entry list", got)
	}
	if got := DetectBrowser(); got == nil {
		t.Fatal("priming DetectBrowser() = nil, want the stubbed browser")
	}
	if *calls != 2 {
		t.Fatalf("priming calls: %d, want 2 (one DetectBrowser + one DetectBrowsers)", *calls)
	}

	// Still well within the TTL — a second call must NOT re-scan yet.
	DetectBrowsers()
	DetectBrowser()
	if *calls != 2 {
		t.Fatalf("calls after a same-TTL re-poll: %d, want still 2 before invalidating", *calls)
	}

	InvalidateBrowserDetection()

	DetectBrowsers()
	DetectBrowser()
	if *calls != 4 {
		t.Fatalf("calls after InvalidateBrowserDetection + one poll of each: %d, want 4 — invalidate "+
			"must force both slots to re-scan immediately rather than riding out the remaining TTL", *calls)
	}
}

// --- Rider 1: the (network?) hedge split ---------------------------------

// newRollbackHedgeService builds the fixture for the "restoredPlatforms +
// rollbackWasInconclusive" arm of RefreshCookiesDetailed's error switch: a
// healthy cookies.txt already on disk for YouTube, a fresh browser-profile
// import for YouTube that the post-import check cannot evaluate, and no
// browser at all (forcing the browser-free import path so no real process is
// ever launched). attempted controls which half of verifyUnknown the check
// reports.
func newRollbackHedgeService(t *testing.T, attempted bool) *AutoCookieService {
	t.Helper()
	profileDir := writeWALCookieProfile(t, youtubeAuthRows())
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(youtubeOnlyCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return nil }
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) {
		if attempted {
			return false, errors.New("dial tcp: no such host")
		}
		return false, ErrAuthCheckNotAttempted
	}
	return s
}

// TestRollbackHedgeSaysTheCheckDidNotCompleteWhenAttempted pins the exact
// rendered string for the "a request went out and came back unusable"
// half — no question mark, because this is no longer a guess.
func TestRollbackHedgeSaysTheCheckDidNotCompleteWhenAttempted(t *testing.T) {
	s := newRollbackHedgeService(t, true)
	if _, err := s.RefreshCookies(context.Background()); err != nil {
		t.Fatalf("RefreshCookies: %v", err)
	}
	status := s.GetStatus()
	if status.LastError == nil {
		t.Fatal("an import that was not committed must leave an explanation")
	}
	const want = "kept the previous cookies for youtube — the auth check did not complete, " +
		"so the imported profile was not accepted"
	if *status.LastError != want {
		t.Errorf("LastError = %q, want %q", *status.LastError, want)
	}
}

// TestRollbackHedgeSaysNeverAttemptedWhenNoRequestWasMade pins the other
// half, wording it reuses verbatim from the Warn checkPlatformAuth's callers
// already emit for the same fact ("the extracted cookies cannot form an
// authenticated request") so the two surfaces agree.
func TestRollbackHedgeSaysNeverAttemptedWhenNoRequestWasMade(t *testing.T) {
	s := newRollbackHedgeService(t, false)
	if _, err := s.RefreshCookies(context.Background()); err != nil {
		t.Fatalf("RefreshCookies: %v", err)
	}
	status := s.GetStatus()
	if status.LastError == nil {
		t.Fatal("an import that was not committed must leave an explanation")
	}
	const want = "kept the previous cookies for youtube — the auth check was never attempted — " +
		"the extracted cookies cannot form an authenticated request, so the imported profile was not accepted"
	if *status.LastError != want {
		t.Errorf("LastError = %q, want %q", *status.LastError, want)
	}
}

// newInconclusiveNoRollbackService builds the fixture for the "inconclusive,
// nothing to restore" arm: no pre-existing cookies.txt at all (so
// platformsToRestore never runs), a fresh browser-profile import for YouTube
// only, and a post-import check that cannot complete.
func newInconclusiveNoRollbackService(t *testing.T, attempted bool) *AutoCookieService {
	t.Helper()
	profileDir := writeWALCookieProfile(t, youtubeAuthRows())
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return nil }
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) {
		if attempted {
			return false, errors.New("dial tcp: no such host")
		}
		return false, ErrAuthCheckNotAttempted
	}
	return s
}

// TestInconclusiveHedgeSaysTheCheckDidNotCompleteWhenAttempted is the
// no-rollback arm's attempted half.
func TestInconclusiveHedgeSaysTheCheckDidNotCompleteWhenAttempted(t *testing.T) {
	s := newInconclusiveNoRollbackService(t, true)
	if _, err := s.RefreshCookies(context.Background()); err != nil {
		t.Fatalf("RefreshCookies: %v", err)
	}
	status := s.GetStatus()
	if status.LastError == nil {
		t.Fatal("an inconclusive verification must leave an explanation")
	}
	const want = "YouTube auth could not be verified — the auth check did not complete"
	if *status.LastError != want {
		t.Errorf("LastError = %q, want %q", *status.LastError, want)
	}
}

// TestInconclusiveHedgeSaysNeverAttemptedWhenNoRequestWasMade is the
// no-rollback arm's never-attempted half.
func TestInconclusiveHedgeSaysNeverAttemptedWhenNoRequestWasMade(t *testing.T) {
	s := newInconclusiveNoRollbackService(t, false)
	if _, err := s.RefreshCookies(context.Background()); err != nil {
		t.Fatalf("RefreshCookies: %v", err)
	}
	status := s.GetStatus()
	if status.LastError == nil {
		t.Fatal("an inconclusive verification must leave an explanation")
	}
	const want = "YouTube auth could not be verified — the auth check was never attempted — " +
		"the extracted cookies cannot form an authenticated request"
	if *status.LastError != want {
		t.Errorf("LastError = %q, want %q", *status.LastError, want)
	}
}

// TestInconclusiveHedgeNamesEachPlatformWhenTheyDisagree is reviewer round 1,
// finding 1: YouTube's cookies never formed a request at all
// (ErrAuthCheckNotAttempted) while Twitch's check went out and ate a 429
// (attempted). Both land on verifyUnknown, but for different reasons — a
// single collapsed hedge would assert one platform's cause about the other.
// No previous cookies.txt, so nothing is restored and this is the
// no-rollback arm.
//
// Mutation check: collapsing to AND semantics (require every platform to
// have attempted before saying "did not complete") renders "YouTube +
// Twitch auth could not be verified — the auth check was never attempted —
// ..." — a false claim about Twitch, which DID attempt. Collapsing to OR
// (any platform attempted is enough) renders "...the auth check did not
// complete" — a false claim about YouTube, which never attempted at all.
// Both fail this test; only the per-platform breakdown passes.
func TestInconclusiveHedgeNamesEachPlatformWhenTheyDisagree(t *testing.T) {
	profileDir := writeWALCookieProfile(t, youtubeAndTwitchRows(goodTwitchToken))
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return nil }
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return false, ErrAuthCheckNotAttempted }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) {
		return false, errors.New("twitch auth check: unexpected status 429")
	}

	if _, err := s.RefreshCookies(context.Background()); err != nil {
		t.Fatalf("RefreshCookies: %v", err)
	}
	status := s.GetStatus()
	if status.LastError == nil {
		t.Fatal("an inconclusive verification must leave an explanation")
	}
	const want = "YouTube: the auth check was never attempted — the extracted cookies cannot form " +
		"an authenticated request; Twitch: the auth check did not complete"
	if *status.LastError != want {
		t.Errorf("LastError = %q, want %q", *status.LastError, want)
	}
}

// TestRollbackHedgeNamesEachPlatformWhenTheyDisagree is
// TestInconclusiveHedgeNamesEachPlatformWhenTheyDisagree's twin for the
// restored-platforms arm: a healthy cookies.txt for BOTH platforms already
// on disk, a fresh import that neither check can evaluate conclusively, and
// the same attempted split. Both platforms are restored (their pre-import
// checks had cookies and the post-import checks are inconclusive), so this
// exercises rollbackHedge rather than inconclusiveHedge.
func TestRollbackHedgeNamesEachPlatformWhenTheyDisagree(t *testing.T) {
	profileDir := writeWALCookieProfile(t, youtubeAndTwitchRows(staleTwitchToken))
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(previousCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return nil }
	// Ignores jar state on purpose, unlike the regression fixtures elsewhere
	// in this package: the point here is an inconclusive check on BOTH sides
	// of the import, not a verified-then-broken regression.
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return false, ErrAuthCheckNotAttempted }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) {
		return false, errors.New("dial tcp: no such host")
	}

	if _, err := s.RefreshCookies(context.Background()); err != nil {
		t.Fatalf("RefreshCookies: %v", err)
	}
	status := s.GetStatus()
	if status.LastError == nil {
		t.Fatal("an import that was not committed must leave an explanation")
	}
	const want = "kept the previous cookies for youtube + twitch — YouTube: the auth check was never " +
		"attempted — the extracted cookies cannot form an authenticated request; Twitch: the auth check " +
		"did not complete, so the imported profile was not accepted"
	if *status.LastError != want {
		t.Errorf("LastError = %q, want %q", *status.LastError, want)
	}
}
