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
// fakeApplyUserOnlyDACL.
//
// It is not the ONLY way to reach the reload failure — a counting
// writeCookieFile stub that lets the merged write through and turns
// cookies.txt into a directory on the RESTORE write gets there on either
// caller, which is the directory trick cookie_import_rollback_test.go uses.
// It is the right way: OS-independent, and it fails the reload WITHOUT
// co-opting the write seam, so a test of "the file is correct and the process
// is not" cannot quietly become a test of a failed write instead.
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
// other caller. What the fixture actually drives is the MOUNTED-PROFILE arm of
// RefreshCookiesDetailed — detectBrowser returns nil, so no browser is
// launched and the profile's own cookies are the ones that fail to verify.
// That arm is where the refresh path words its rollback sentence, and the seam
// is what makes its reload fail without disturbing the write in front of it.
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

// TestRollbackIncompleteBodyIsBoundedByConstruction is the close review's
// M-CW5 (M-1). The import's rollback-incomplete failure is answered as an HTTP
// 500 whose body is the error's own sentence (internal/web/routes/cookies.go),
// and that body has to stay under internal/web's 1024-byte gzipMinSize: above
// it CompressionMiddleware's startGzip DELETES the Content-Length O-L added and
// re-chunks the answer, so the client waits out the whole blocking re-check for
// a body it has already been sent the headers for.
//
// Every other exit of that handler is bounded by the phrase it carries. This
// one was bounded by nothing: the wrapped write failure is an *os.LinkError
// from the rename inside writeFileAtomic, whose Error() prints BOTH absolute
// paths — and one of those is cookies.cookie_file, an operator config value
// with no length limit. A path of a few hundred characters (a deep Windows
// profile, a long bind-mount prefix) put the body over the ceiling on its own.
//
// So the cause is rendered "op: errno" rather than "op path path: errno". The
// error CHAIN is untouched — errors.Is still finds both the sentinel and the
// cause — and the log line beside it still carries the raw error with its
// paths, which is where an operator who needs them looks.
//
// Mutants:
//   - restore the plain %w on the cause -> the body carries both absolute
//     paths and goes over 1024 bytes.
//   - render only the errno and drop the Op -> "the operator is told what
//     failed" fails; "permission denied" with no verb names nothing.
//   - unwrap the chain instead of wrapping a bounded renderer -> the
//     errors.Is assertions fail.
func TestRollbackIncompleteBodyIsBoundedByConstruction(t *testing.T) {
	// A long cookies.txt path — a legal config value, and the input that used
	// to blow the ceiling on its own.
	longDir := filepath.Join(t.TempDir(), strings.Repeat("d", 200), strings.Repeat("e", 150))
	cookiePath := filepath.Join(longDir, "cookies.txt")

	s := NewAutoCookieService(t.TempDir(), cookiePath, NewCookieJar(), nopAutoCookieLogger{})

	// What writeFileAtomic's rename really returns: an *os.LinkError carrying
	// both absolute paths.
	renameErr := &os.LinkError{
		Op:  "rename",
		Old: cookiePath + ".3141592653.tmp",
		New: cookiePath,
		Err: errors.New("Access is denied."),
	}
	real := writeCookieFile
	t.Cleanup(func() { writeCookieFile = real })
	writeCookieFile = func(string, []byte, os.FileMode) error { return renameErr }

	fail := s.restorePreviousCookies("restored", []string{"twitch"}, rollbackMessages{
		sentinel:   ErrImportRollbackIncomplete,
		writeHead:  "Twitch did not verify and the previous cookies could not be restored",
		reloadHead: "Twitch's previous cookies were restored but could not be reloaded",
		writeLog:   "could not restore the previous cookies.txt after a rejected import",
	})
	if fail == nil {
		t.Fatal("restorePreviousCookies reported success although the write failed")
	}

	body := fail.err.Error()
	const ceiling = 512
	if len(body) >= ceiling {
		t.Errorf("the rollback-incomplete sentence is %d bytes for a %d-character cookie path, at or "+
			"over the %d-byte bound. The route answers it as a 500 body, and above internal/web's "+
			"1024-byte gzipMinSize startGzip deletes the Content-Length O-L added and re-chunks the "+
			"answer — which puts the client back behind the blocking re-check:\n%s",
			len(body), len(cookiePath), ceiling, body)
	}
	if n := strings.Count(body, cookiePath); n != 0 {
		t.Errorf("the sentence carries the cookie path %d times; it is an unbounded operator config "+
			"value and the message is bounded by construction or not at all:\n%s", n, body)
	}
	if !strings.Contains(body, "rename") || !strings.Contains(body, "Access is denied.") {
		t.Errorf("the sentence no longer says WHAT failed and why (want the op and the errno): %s", body)
	}
	if !errors.Is(fail.err, ErrImportRollbackIncomplete) {
		t.Error("the sentinel no longer survives — the route's 500 arm matches on it")
	}
	if !errors.Is(fail.err, renameErr) {
		t.Error("the cause no longer survives in the chain — bounding the TEXT must not unwrap the error")
	}
}
