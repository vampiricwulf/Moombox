package cookies

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// failCookieRead makes readCookieFile return err instead of touching disk,
// for the lifetime of the test. Mirrors failCookieWriteAfter
// (autocookies_emptyresult_test.go) for the read side of the same seam
// pattern — writeCookieFile.
func failCookieRead(t *testing.T, err error) {
	t.Helper()
	real := readCookieFile
	t.Cleanup(func() { readCookieFile = real })
	readCookieFile = func(string) ([]byte, error) {
		return nil, err
	}
}

// TestFinishSetupAbortsOnUnreadableExistingCookieFile is S9 at the
// FinishSetup site (autocookies.go, formerly :511).
//
// The old code treated "could not read the existing cookies.txt" and "the
// file does not exist" identically: on a transient read failure (permission
// blip, locked file, I/O error, a bind-mount hiccup in Docker) the merge was
// skipped silently and the write below replaced cookies.txt with ONLY the
// cookies this setup call just extracted — destroying whatever the OTHER
// platform had. This pins the fix: a real file with known content sits on
// disk, the read is made to fail without touching it, and the assertion is
// that the bytes on disk are unchanged and an error came back.
func TestFinishSetupAbortsOnUnreadableExistingCookieFile(t *testing.T) {
	s := finishSetupService(t, youtubeAuthRows(), nopAutoCookieLogger{})
	if err := os.WriteFile(s.cookiePath, []byte(previousCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}

	readErr := errors.New("permission denied (simulated)")
	failCookieRead(t, readErr)

	ytAuth, twAuth, err := s.FinishSetup(context.Background())

	if err == nil {
		t.Fatal("FinishSetup must return an error when the existing cookie file could not be read")
	}
	if !errors.Is(err, ErrCookieFileUnreadable) {
		t.Errorf("returned error does not wrap ErrCookieFileUnreadable, which is what every consumer discriminates on: %v", err)
	}
	if !errors.Is(err, readErr) {
		t.Errorf("returned error does not wrap the underlying read failure: %v", err)
	}
	if ytAuth || twAuth {
		t.Errorf("an aborted setup must not report either platform authenticated, got yt=%v tw=%v", ytAuth, twAuth)
	}

	data, readBackErr := os.ReadFile(s.cookiePath)
	if readBackErr != nil {
		t.Fatalf("cookies.txt must still exist after the abort: %v", readBackErr)
	}
	if string(data) != previousCookieFile {
		t.Errorf("existing cookies.txt was modified by an aborted setup:\ngot:  %q\nwant: %q", data, previousCookieFile)
	}

	if status := s.GetStatus(); status.LastError == nil {
		t.Error("an aborted setup must leave a visible error on status")
	}
}

// TestFinishSetupProceedsWhenNoExistingCookieFile is the load-bearing
// first-run counterpart to the abort test above. Breaking this path would be
// worse than the bug the abort exists to fix: a fresh install would fail to
// write its very first cookie file. No cookies.txt exists yet, so FinishSetup
// must proceed exactly as before — write the freshly extracted cookies with
// no merge attempted and no error.
func TestFinishSetupProceedsWhenNoExistingCookieFile(t *testing.T) {
	s := finishSetupService(t, youtubeAuthRows(), nopAutoCookieLogger{})
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }

	if _, statErr := os.Stat(s.cookiePath); !os.IsNotExist(statErr) {
		t.Fatalf("fixture is broken — cookies.txt must not exist yet, stat err = %v", statErr)
	}

	ytAuth, _, err := s.FinishSetup(context.Background())
	if err != nil {
		t.Fatalf("FinishSetup on a first run must not error: %v", err)
	}
	if !ytAuth {
		t.Error("expected YouTube accepted on a fresh setup")
	}

	data, err := os.ReadFile(s.cookiePath)
	if err != nil {
		t.Fatalf("FinishSetup must have written cookies.txt: %v", err)
	}
	if !strings.Contains(string(data), "login-info-from-profile") {
		t.Errorf("cookies.txt does not contain the extracted cookies:\n%s", data)
	}
}

// TestRefreshCookiesAbortsOnUnreadableExistingCookieFile is S9 at the
// RefreshCookiesDetailed site (autocookies.go, formerly :1058).
//
// Reuses the exact regression fixture from
// TestRefreshCookiesRestoresOnlyTheRegressedPlatform (a healthy cookies.txt
// with BOTH platforms working, and a mounted profile whose Twitch token is
// stale) because that fixture is what makes the old bug concrete: with
// previousCookies wrongly treated as empty, the write below would replace
// cookies.txt with only the imported (YouTube-good, Twitch-stale) rows, and
// the per-platform rollback that exists to catch exactly that regression
// never even runs — it is gated on previousCookies != "".
//
// verifyCalls does NOT witness the rollback pre-check specifically — that
// gate (importedFromProfile && previousCookies != "") is skipped in the OLD,
// buggy code too, for the same fixture: a transient read failure was already
// treated as "file absent" there, so previousCookies is "" on both the
// fixed and unfixed paths, and the pre-check never distinguishes them. What
// does distinguish them is the UNCONDITIONAL post-write verification further
// down ("Verify auth via API callbacks"): the unfixed code reaches the write
// and then that check, calling both verify callbacks once each (confirmed
// against the unfixed baseline — 2 calls, not 0), while the fixed code
// returns before either ever runs. Zero calls therefore proves the abort
// precedes every checkPlatformAuth call in the function — including the one
// that would otherwise run right after a write this pass must not make.
func TestRefreshCookiesAbortsOnUnreadableExistingCookieFile(t *testing.T) {
	profileDir := writeWALCookieProfile(t, youtubeAndTwitchRows(staleTwitchToken))
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(previousCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}

	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return nil } // container: no browser, import path

	var verifyCalls int
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) {
		verifyCalls++
		return true, nil
	}
	s.VerifyTwitchAuth = func(context.Context) (bool, error) {
		verifyCalls++
		return s.jar.GetTwitchAuthToken() == goodTwitchToken, nil
	}

	readErr := errors.New("permission denied (simulated)")
	failCookieRead(t, readErr)

	result, err := s.RefreshCookiesDetailed(context.Background())

	if err == nil {
		t.Fatal("RefreshCookiesDetailed must return an error when the existing cookie file could not be read")
	}
	if !errors.Is(err, ErrCookieFileUnreadable) {
		t.Errorf("returned error does not wrap ErrCookieFileUnreadable, which is what every consumer discriminates on: %v", err)
	}
	if !errors.Is(err, readErr) {
		t.Errorf("returned error does not wrap the underlying read failure: %v", err)
	}
	if !result.Ran {
		t.Error("this pass already did real import work before the read failed; Ran must be true (refreshAborted, not refreshDeclined)")
	}
	if result.YouTube != RefreshUnknown || result.Twitch != RefreshUnknown {
		t.Errorf("an aborted pass must not carry a verdict for either platform, got YouTube=%v Twitch=%v", result.YouTube, result.Twitch)
	}
	if verifyCalls != 0 {
		t.Errorf("verify callbacks were called %d times; the abort must happen before ANY verification, including the rollback pre-check", verifyCalls)
	}

	data, readBackErr := os.ReadFile(cookiePath)
	if readBackErr != nil {
		t.Fatalf("cookies.txt must still exist after the abort: %v", readBackErr)
	}
	if string(data) != previousCookieFile {
		t.Errorf("existing cookies.txt was modified by an aborted refresh:\ngot:  %q\nwant: %q", data, previousCookieFile)
	}

	if status := s.GetStatus(); status.LastError == nil {
		t.Error("an aborted refresh must leave a visible error on status")
	}
}

// TestRefreshCookiesProceedsWhenNoExistingCookieFile is the load-bearing
// first-run counterpart at the RefreshCookiesDetailed site: the common
// container case where nothing has ever been written to cookiePath yet. The
// pass must proceed exactly as before — import, verify, write, no error.
func TestRefreshCookiesProceedsWhenNoExistingCookieFile(t *testing.T) {
	profileDir := writeWALCookieProfile(t, youtubeAuthRows())
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")

	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return nil }
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }

	if _, statErr := os.Stat(cookiePath); !os.IsNotExist(statErr) {
		t.Fatalf("fixture is broken — cookies.txt must not exist yet, stat err = %v", statErr)
	}

	result, err := s.RefreshCookiesDetailed(context.Background())
	if err != nil {
		t.Fatalf("a first-run import must not error: %v", err)
	}
	if result.YouTube != RefreshOK {
		t.Errorf("expected YouTube verified on a first-run import, got %v", result.YouTube)
	}

	data, readErr := os.ReadFile(cookiePath)
	if readErr != nil {
		t.Fatalf("first-run import must have written cookies.txt: %v", readErr)
	}
	if !strings.Contains(string(data), "login-info-from-profile") {
		t.Errorf("cookies.txt does not contain the imported cookies:\n%s", data)
	}
}

// TestRefreshCookiesWrapperPropagatesCookieFileUnreadable pins
// ErrCookieFileUnreadable one layer further out than the return site: through
// RefreshCookies, the thin bool-returning wrapper every whole-service caller
// (the startup seed, the periodic tick, the Settings "refresh now" button,
// runCookieRecovery) actually calls instead of RefreshCookiesDetailed. It
// passes err straight through today, but that is exactly the kind of
// assumption this plan has been burned by asserting instead of checking —
// see the coordinator's note that a discriminating error must survive to
// "the outermost caller, not just at the return site".
func TestRefreshCookiesWrapperPropagatesCookieFileUnreadable(t *testing.T) {
	profileDir := writeWALCookieProfile(t, youtubeAuthRows())
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(previousCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}

	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return nil }

	failCookieRead(t, errors.New("permission denied (simulated)"))

	ok, err := s.RefreshCookies(context.Background())
	if ok {
		t.Error("RefreshCookies = true on an aborted pass")
	}
	if !errors.Is(err, ErrCookieFileUnreadable) {
		t.Errorf("RefreshCookies's returned error lost ErrCookieFileUnreadable somewhere between it and RefreshCookiesDetailed: %v", err)
	}
}
