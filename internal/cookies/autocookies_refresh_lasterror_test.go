package cookies

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newProfileRefreshService builds the profile-import refresh the three exits
// below live on: no browser (detectBrowser returns nil), a mounted profile with
// working credentials, and a cookies.txt already on disk.
func newProfileRefreshService(t *testing.T, cookiePath string) *AutoCookieService {
	t.Helper()
	profileDir := writeWALCookieProfile(t, youtubeAndTwitchRows(goodTwitchToken))
	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return nil }
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return true, nil }
	return s
}

// TestRefreshWriteFailureIsRecordedForTheOperator: the browser-refresh pass's
// write exit returned an error and left lastError empty, so a write failing
// every 30 minutes (disk full, AV lock, Docker single-file bind mount) left
// both dashboards looking clean while credential renewal had stopped.
//
// Mutant: deleting the s.setError call at the writeCookieFile exit leaves
// LastError nil and fails this.
func TestRefreshWriteFailureIsRecordedForTheOperator(t *testing.T) {
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(previousCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newProfileRefreshService(t, cookiePath)
	failCookieWriteAfter(t, 1, errors.New("no space left on device"))

	if _, err := s.RefreshCookies(context.Background()); err == nil {
		t.Fatal("a refresh whose write failed must return an error")
	}
	status := s.GetStatus()
	if status.LastError == nil {
		t.Fatal("a failed write must be recorded — the status line is where an operator looks afterwards")
	}
	if !strings.Contains(*status.LastError, "could not write cookies.txt") {
		t.Errorf("LastError = %q, want the FinishSetup wording", *status.LastError)
	}
	if !strings.Contains(*status.LastError, "mount the data directory rather than cookies.txt itself") {
		t.Errorf("LastError = %q, want the Docker bind-mount hint its twin carries", *status.LastError)
	}
}

// TestRefreshMkdirFailureIsRecordedForTheOperator: the same silence on the
// exit that fires when cookies.txt's directory cannot be created.
//
// Mutant: deleting the s.setError call at the MkdirAll exit fails this.
func TestRefreshMkdirFailureIsRecordedForTheOperator(t *testing.T) {
	dir := t.TempDir()
	// A FILE where the cookie file's parent directory belongs: MkdirAll fails
	// with ENOTDIR on Linux and "directory name is invalid" on Windows.
	blocker := filepath.Join(dir, "notadir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newProfileRefreshService(t, filepath.Join(blocker, "cookies.txt"))

	if _, err := s.RefreshCookies(context.Background()); err == nil {
		t.Fatal("a refresh that cannot create the cookie directory must return an error")
	}
	status := s.GetStatus()
	if status.LastError == nil {
		t.Fatal("a failed mkdir must be recorded")
	}
	if !strings.Contains(*status.LastError, "could not create the directory for cookies.txt") {
		t.Errorf("LastError = %q, want the FinishSetup wording", *status.LastError)
	}
}

// TestRefreshJarLoadFailureIsRecordedForTheOperator: the worst of the three to
// leave silent — the cookies were fetched AND written, so nothing about the
// file on disk looks wrong and the pass simply reported nothing.
//
// Mutant: deleting the s.setError call at the jar.Load exit fails this.
func TestRefreshJarLoadFailureIsRecordedForTheOperator(t *testing.T) {
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(previousCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newProfileRefreshService(t, cookiePath)

	// The write "succeeds" but leaves a DIRECTORY at the path, so the reload
	// underneath fails — the package's existing way of driving this branch
	// (cookie_import_rollback_test.go).
	realWrite := writeCookieFile
	t.Cleanup(func() { writeCookieFile = realWrite })
	writeCookieFile = func(path string, data []byte, perm os.FileMode) error {
		if err := os.RemoveAll(path); err != nil {
			return err
		}
		return os.Mkdir(path, 0o755)
	}

	if _, err := s.RefreshCookies(context.Background()); err == nil {
		t.Fatal("a refresh whose jar reload failed must return an error")
	}
	status := s.GetStatus()
	if status.LastError == nil {
		t.Fatal("a write that could not be loaded back must be recorded")
	}
	if !strings.Contains(*status.LastError, "cookies.txt was written but could not be loaded") {
		t.Errorf("LastError = %q, want the FinishSetup wording", *status.LastError)
	}
}
