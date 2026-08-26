package cookies

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestBrowserLaunchActed pins the acted/not-acted decision.
//
// Every row is a way a refresh used to report success while doing nothing.
// The two halves of the predicate are not interchangeable: the error covers
// browsers that were killed or never started, the screenshot covers the
// failures that return no error at all — and it was those that made the
// Firefox-family refresh a permanent silent no-op.
func TestBrowserLaunchActed(t *testing.T) {
	cases := []struct {
		name      string
		launchErr error
		rendered  bool
		want      bool
	}{
		{"clean launch that rendered", nil, true, true},

		// The bug this whole arc exists for. runWithTimeout returns nil when
		// the job-count query fails (drainJob stops waiting) and when
		// job.assign failed (the browser is outside the job, so the drain sees
		// an empty job on lap zero). Both reap the launcher, kill or outrun the
		// browser, and hand back nil — and both used to log "refresh
		// completed" at Info.
		{"no error but nothing rendered", nil, false, false},

		// A drain timeout means the browser was alive and has just been killed
		// mid-load. A screenshot from earlier in that same launch does not
		// redeem it: the profile is half-written at best.
		{"drain timed out even though a page rendered", errBrowserDrainTimeout, true, false},
		{"drain timed out with nothing rendered", errBrowserDrainTimeout, false, false},

		{"browser could not start", errors.New("exec: no such file"), false, false},
		{"cancelled mid-launch", context.Canceled, false, false},
		{"launch error with a stale screenshot present", errors.New("boom"), true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := browserLaunchActed(tc.launchErr, tc.rendered); got != tc.want {
				t.Errorf("browserLaunchActed(%v, %v) = %v, want %v", tc.launchErr, tc.rendered, got, tc.want)
			}
		})
	}
}

// TestBrowserRenderProofIsPerLaunch is the trap the brief calls out: the
// screenshot lives at a FIXED path inside the profile and refreshFirefox's
// os.Remove is function-scoped, so without a per-launch clear the YouTube
// screenshot survives into the Twitch launch and every platform after the
// first reads as "acted" no matter what its browser did.
//
// The clear-then-check ordering is the contract; this pins it end to end.
func TestBrowserRenderProofIsPerLaunch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "refresh-screenshot.png")

	// A screenshot left by the previous platform's launch.
	if err := os.WriteFile(path, []byte("stale png bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !browserRendered(path) {
		t.Fatal("fixture is broken — the stale artifact should look like proof before it is cleared")
	}

	clearBrowserRenderProof(path)
	if browserRendered(path) {
		t.Fatal("a screenshot from the PREVIOUS launch was counted as this launch's proof")
	}

	// Clearing a path that is already gone is the normal case, not an error.
	clearBrowserRenderProof(path)

	if err := os.WriteFile(path, []byte("fresh png bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !browserRendered(path) {
		t.Error("a screenshot written by this launch is the proof the verdict is made of")
	}
}

// TestBrowserRenderedRejectsNonProof covers the artifacts that exist but prove
// nothing. A zero-length file is what a browser killed part-way through
// writing leaves behind; a directory at that path is not a screenshot at all.
func TestBrowserRenderedRejectsNonProof(t *testing.T) {
	dir := t.TempDir()

	missing := filepath.Join(dir, "never-written.png")
	if browserRendered(missing) {
		t.Error("a screenshot that does not exist is not proof")
	}

	empty := filepath.Join(dir, "empty.png")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if browserRendered(empty) {
		t.Error("a zero-length screenshot is what a killed browser leaves behind, not proof it rendered")
	}

	asDir := filepath.Join(dir, "adirectory.png")
	if err := os.Mkdir(asDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if browserRendered(asDir) {
		t.Error("a directory at the screenshot path is not a screenshot")
	}
}

// TestRefreshThatRenewedNothingDoesNotClaimCredit is F2 at the level that
// matters.
//
// A browser refresh whose browser never ran still finds the previous
// credentials in cookies.txt — the independent 30-minute RefreshService keeps
// them alive — so verification passes and the call still returns true, which is
// the honest answer to "will authenticated requests work?". What must NOT
// happen is this pass taking credit for it: stamping lastRefresh would suppress
// the next attempt (shouldSkipPeriodicRefresh skips inside interval/2) and tell
// the user in Settings that their cookies are fresher than they are, and the
// meta sidecar would make that durable across a restart.
func TestRefreshThatRenewedNothingDoesNotClaimCredit(t *testing.T) {
	// A profile with nothing relevant in it, so the read contributes nothing
	// and the existing cookies.txt is what verifies.
	profileDir := writeWALCookieProfile(t, []profileTestCookie{
		{name: "sessionid", value: "x", host: ".example.com", path: "/"},
	})
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(youtubeOnlyCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}

	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	// A browser that cannot launch is the simplest not-acted case: no
	// screenshot, and refreshFirefox degrades rather than failing the refresh.
	s.detectBrowser = func() *DetectedBrowser {
		return &DetectedBrowser{Type: "firefox", Path: "moombox-no-such-browser", Name: "Firefox"}
	}
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return true, nil }
	if err := s.jar.Load(cookiePath); err != nil {
		t.Fatal(err)
	}

	ok, err := s.RefreshCookies(context.Background())
	if err != nil {
		t.Fatalf("RefreshCookies: %v", err)
	}
	if !ok {
		t.Fatal("the cookies on disk still authenticate, so the caller's question is still answered yes")
	}

	if lr := s.GetStatus().LastRefresh; lr != nil {
		t.Errorf("lastRefresh was stamped for a refresh whose browser never ran: %q", *lr)
	}
	if _, statErr := os.Stat(MetaPath(cookiePath)); !os.IsNotExist(statErr) {
		t.Error("the meta sidecar records a refresh that never happened, making the claim survive a restart")
	}
}

// TestProfileImportStillClaimsCredit is the guard on the other side, and the
// reason the mtime-based version of this check was withdrawn.
//
// The browserless import (Docker: no browser installed, a mounted profile) has
// no browser that could have acted, so anything that demands proof of one makes
// every containerised import report a refresh that never renews — permanently,
// on every restart. Reading a profile IS renewal; this must stay indistinguishable
// from a successful browser refresh.
func TestProfileImportStillClaimsCredit(t *testing.T) {
	profileDir := writeWALCookieProfile(t, youtubeAuthRows())
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")

	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return nil }
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }

	ok, err := s.RefreshCookies(context.Background())
	if err != nil {
		t.Fatalf("RefreshCookies: %v", err)
	}
	if !ok {
		t.Fatal("the import read a signed-in profile and YouTube verified — that is a success")
	}

	if s.GetStatus().LastRefresh == nil {
		t.Error("a browserless import must still stamp lastRefresh; withholding it re-fires the import on every tick forever")
	}
	meta, err := LoadMeta(cookiePath)
	if err != nil {
		t.Fatalf("LoadMeta: %v", err)
	}
	if meta == nil || meta.LastRefresh.IsZero() {
		t.Fatal("a browserless import must still persist the meta sidecar")
	}
}
