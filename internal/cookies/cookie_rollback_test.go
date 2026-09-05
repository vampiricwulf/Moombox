package cookies

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// errFakeJarReload is the failure a swapped loadCookieJar hands back. It is a
// sentinel rather than a fresh errors.New per call so each test can assert the
// CAUSE survived into the value its caller returns — the property that lets an
// operator's log show what actually went wrong underneath the sentence.
var errFakeJarReload = errors.New("fake reload failure: cookies.txt is not readable")

// fakeLoadCookieJar swaps the loadCookieJar seam for the duration of a test,
// restoring the real one via t.Cleanup. Same shape, and the same reason, as
// fakeApplyUserOnlyDACL: the reload that follows a restore is otherwise only
// reachable by breaking the filesystem underneath the jar (the directory trick
// in cookie_import_rollback_test.go), which the browser path's fixture cannot
// do without also breaking the write that precedes it.
func fakeLoadCookieJar(t *testing.T, fn func(s *AutoCookieService, path string) error) {
	t.Helper()
	real := loadCookieJar
	t.Cleanup(func() { loadCookieJar = real })
	loadCookieJar = fn
}

// TestImportRollbackReportsAJarReloadThatFails drives the import path's
// "the file is correct and the process is not" exit through the seam.
//
// The claim is unchanged by the extraction of restorePreviousCookies: the
// error the caller is handed wraps ErrImportRollbackIncomplete AND the reload
// failure underneath it, says which platform and which half failed, and is the
// SAME sentence that lands in lastError — so the dialog that caused the import
// and the Settings panel cannot describe different events.
func TestImportRollbackReportsAJarReloadThatFails(t *testing.T) {
	s, log := importServiceWithLog(t, importRollbackSeed)
	s.VerifyTwitchAuth = twitchLiveFromJar(s)
	fakeLoadCookieJar(t, func(*AutoCookieService, string) error { return errFakeJarReload })

	result, err := s.ImportCookies(context.Background(), importRollbackPaste)
	if !errors.Is(err, ErrImportRollbackIncomplete) {
		t.Fatalf("error = %v, want it to wrap ErrImportRollbackIncomplete", err)
	}
	if !errors.Is(err, errFakeJarReload) {
		t.Errorf("error = %v does not wrap the reload failure underneath it", err)
	}
	if !result.Wrote {
		t.Error("Wrote is false although cookies.txt was replaced — the caller's re-check is gated on it")
	}
	if !strings.Contains(err.Error(), "Twitch's previous cookies were restored but could not be "+
		"reloaded") || !strings.Contains(err.Error(), "this process is still using the rejected credentials") {
		t.Errorf("error = %q — this exit says the process is stale, not that the file is wrong", err)
	}
	if got := lastErrorSnapshot(s); got != err.Error() {
		t.Errorf("lastError = %q but the caller was handed %q — Settings and the dialog that "+
			"caused this would describe different events", got, err)
	}
	if !strings.Contains(log.all(), "could not reload cookie jar after restoring the previous cookies.txt") {
		t.Errorf("the log never names the failed reload:\n%s", log.all())
	}
}

// TestBrowserRefreshRollbackReportsAJarReloadThatFails is the same exit on the
// other caller, and the reason the seam exists: the browser path reaches its
// restore only after a successful write, so the filesystem trick the import
// tests use cannot break the reload without also breaking that write.
//
// The refresh path words its own sentence and returns its own short error —
// it does NOT carry ErrImportRollbackIncomplete — and both must survive the
// move into restorePreviousCookies.
func TestBrowserRefreshRollbackReportsAJarReloadThatFails(t *testing.T) {
	profileDir := writeWALCookieProfile(t, youtubeAndTwitchRows(staleTwitchToken))
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(previousCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}

	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return nil }
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) {
		return s.jar.GetTwitchAuthToken() == goodTwitchToken, nil
	}
	fakeLoadCookieJar(t, func(*AutoCookieService, string) error { return errFakeJarReload })

	out, err := s.RefreshCookiesDetailed(context.Background())
	if err == nil {
		t.Fatal("RefreshCookiesDetailed returned no error although the restore never reached the jar")
	}
	if !errors.Is(err, errFakeJarReload) {
		t.Errorf("error = %v does not wrap the reload failure underneath it", err)
	}
	if !strings.HasPrefix(err.Error(), "reload cookie jar after restore: ") {
		t.Errorf("error = %q — the refresh path's own short wrapper is what its callers match on", err)
	}
	if errors.Is(err, ErrImportRollbackIncomplete) {
		t.Errorf("error = %v carries the import path's sentinel; the browser path never did", err)
	}

	// refreshAborted(), i.e. Ran with no verdict and nothing renewed. Mechanism
	// is stamped by the function's own defer on every exit and is not part of
	// the aborted value.
	got := out
	got.Mechanism = ""
	if got != refreshAborted() {
		t.Errorf("result = %+v, want the aborted result %+v — a sibling platform that verified "+
			"would otherwise carry the whole call to success", out, refreshAborted())
	}

	last := lastErrorSnapshot(s)
	if !strings.Contains(last, "restored the previous cookies for twitch after the browser profile "+
		"did not verify, but reloading them failed") ||
		!strings.Contains(last, "this process is still using the rejected credentials") {
		t.Errorf("lastError = %q — it does not say which platform was restored and that the "+
			"process, not the file, is what is stale", last)
	}
}
