package cookies

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	carryLoginRow  = ".youtube.com\tTRUE\t/\tTRUE\t0\tSAPISID\tfrom-login"
	carryManualRow = ".twitch.tv\tTRUE\t/\tTRUE\t0\tauth-token\tfrom-manual"
	carryStaleRow  = ".youtube.com\tTRUE\t/\tTRUE\t0\tSAPISID\tstale-manual"
)

// carryService is a service built, as the boot builds it, on bootPath — the
// cookie file a wizard's browser login writes to.
func carryService(t *testing.T, bootPath string) *AutoCookieService {
	t.Helper()
	return NewAutoCookieService(t.TempDir(), bootPath, NewCookieJar(), nopAutoCookieLogger{})
}

func writeCarryFile(t *testing.T, path string, rows ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "# Netscape HTTP Cookie File\n" + strings.Join(rows, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readCarryFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// TestCarryCookieFileToCarriesTheLoginIntoTheSavedCookieFile pins W26-06: a
// browser login on the wizard's cookie step writes to the cookie file the
// running service was built with, and the Advanced step lets the operator name
// another cookie file beside it — the restart then loaded an empty jar from
// the new path although the wizard had reported the login Done. The cookies
// are carried into the new file, its directory made, and the refresh sidecar
// with them.
//
// Mutants killed: not writing the new file (the restart's jar is empty);
// skipping the MkdirAll (a new directory is not created); not carrying the
// sidecar.
func TestCarryCookieFileToCarriesTheLoginIntoTheSavedCookieFile(t *testing.T) {
	dir := t.TempDir()
	boot := filepath.Join(dir, "cookies.txt")
	writeCarryFile(t, boot, carryLoginRow)
	refreshed := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if err := SaveMeta(boot, CookieMeta{LastRefresh: refreshed, Platforms: []string{"youtube"}}); err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(dir, "data", "my-cookies.txt")

	if err := carryService(t, boot).CarryCookieFileTo(custom); err != nil {
		t.Fatalf("CarryCookieFileTo: %v", err)
	}

	jar := NewCookieJar()
	if err := jar.Load(custom); err != nil {
		t.Fatalf("the restart's jar load: %v", err)
	}
	if got := readCarryFile(t, custom); !strings.Contains(got, "from-login") {
		t.Errorf("the saved cookie file does not hold the login's cookies:\n%s", got)
	}
	meta, err := LoadMeta(custom)
	if err != nil || meta == nil || !meta.LastRefresh.Equal(refreshed) {
		t.Errorf("sidecar at the new path: %+v, %v — want the login's refresh time", meta, err)
	}
	if got := readCarryFile(t, boot); !strings.Contains(got, "from-login") {
		t.Error("the boot-time cookie file was not left where it was")
	}
}

// TestWizardLoginSurvivesACustomCookieFile is W26-06 end to end, as the
// audit reproduced it: the first run's service is built on the boot-time
// cookie file and the wizard's login goes through the real FinishSetup, which
// writes there; the wizard saves another cookie file; the restart builds its
// jar from that one. Carried before the save, the login is in the restart's
// jar — without the carry the jar held no YouTube auth, and nothing repaired
// it while a browser resolved.
//
// Mutant killed: CarryCookieFileTo returning before it writes.
func TestWizardLoginSurvivesACustomCookieFile(t *testing.T) {
	root := t.TempDir()
	profileDir := writeWALCookieProfileAt(t, filepath.Join(root, "browser-profile"), youtubeAuthRows())
	bootPath := filepath.Join(root, "cookies.txt")
	customPath := filepath.Join(root, "data", "my-cookies.txt")

	// Boot 1 (first run): the wizard's login.
	jar1 := NewCookieJar()
	if err := jar1.Load(bootPath); err != nil {
		t.Fatalf("boot 1 jar: %v", err)
	}
	s1 := NewAutoCookieService(profileDir, bootPath, jar1, nopAutoCookieLogger{})
	s1.detectBrowser = func() *DetectedBrowser {
		return &DetectedBrowser{Type: "firefox", Path: "moombox-no-such-firefox", Name: "Firefox"}
	}
	s1.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
	s1.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }
	s1.setupProcess = &os.Process{Pid: -1}
	s1.setupBrowser = &DetectedBrowser{Type: "firefox", Path: "firefox", Name: "Firefox"}
	s1.browserExited = true
	res, err := s1.FinishSetupDetailed(context.Background())
	if err != nil || !res.YouTubeAccepted {
		t.Fatalf("the wizard's login: %+v, %v", res, err)
	}

	// Completing setup with the custom cookie file.
	if err := s1.CarryCookieFileTo(customPath); err != nil {
		t.Fatalf("CarryCookieFileTo: %v", err)
	}

	// Boot 2: the jar from the saved cookie file.
	jar2 := NewCookieJar()
	if err := jar2.Load(customPath); err != nil {
		t.Fatalf("boot 2 jar: %v", err)
	}
	if !jar2.HasAnyYouTubeAuthCookie() {
		t.Error("the restart's jar holds no YouTube auth although the wizard's login was accepted")
	}
}

// TestCarryCookieFileToMergesIntoAnExistingCookieFile: a cookie file already
// at the new path — one the operator exported by hand — keeps its other
// cookies, and the login's win a clash, the merge every cookie writer uses.
//
// Mutants killed: overwriting the existing file (its Twitch row is lost);
// merging the other way round (the stale SAPISID wins).
func TestCarryCookieFileToMergesIntoAnExistingCookieFile(t *testing.T) {
	dir := t.TempDir()
	boot := filepath.Join(dir, "cookies.txt")
	writeCarryFile(t, boot, carryLoginRow)
	custom := filepath.Join(dir, "manual.txt")
	writeCarryFile(t, custom, carryManualRow, carryStaleRow)

	if err := carryService(t, boot).CarryCookieFileTo(custom); err != nil {
		t.Fatalf("CarryCookieFileTo: %v", err)
	}
	got := readCarryFile(t, custom)
	if !strings.Contains(got, "from-manual") || !strings.Contains(got, "from-login") || strings.Contains(got, "stale-manual") {
		t.Errorf("merged file:\n%s\nwant the manual Twitch row kept and the login's SAPISID over the stale one", got)
	}
}

// TestCarryCookieFileToHasNothingToCarry: the same file under another
// spelling, no boot-time file, or an empty one carry nothing and are no error
// — the same file is not even rewritten.
//
// Mutants killed: dropping the same-file check (the file is rewritten with
// the merge's header); treating a missing boot-time file as an error.
func TestCarryCookieFileToHasNothingToCarry(t *testing.T) {
	dir := t.TempDir()
	boot := filepath.Join(dir, "cookies.txt")
	writeCarryFile(t, boot, carryLoginRow)
	before := readCarryFile(t, boot)
	if err := carryService(t, boot).CarryCookieFileTo(filepath.Join(dir, ".", "cookies.txt")); err != nil {
		t.Fatalf("the same file: %v", err)
	}
	if after := readCarryFile(t, boot); after != before {
		t.Errorf("the same file was rewritten:\n%s", after)
	}

	missing := filepath.Join(dir, "never-written.txt")
	target := filepath.Join(dir, "target.txt")
	if err := carryService(t, missing).CarryCookieFileTo(target); err != nil {
		t.Fatalf("no boot-time file: %v", err)
	}
	empty := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := carryService(t, empty).CarryCookieFileTo(target); err != nil {
		t.Fatalf("an empty boot-time file: %v", err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a cookie file was created with nothing to carry (stat %v)", err)
	}
}

// TestCarryCookieFileToRefusesAnUnreadableTarget: a cookie file at the new
// path that cannot be read is not overwritten — it may hold the only working
// credentials for the other platform — and the refusal is the one FinishSetup
// gives (ErrCookieFileUnreadable).
//
// Mutant killed: treating any read failure of the target as "no file yet"
// (the target is overwritten with the login's cookies alone).
func TestCarryCookieFileToRefusesAnUnreadableTarget(t *testing.T) {
	dir := t.TempDir()
	boot := filepath.Join(dir, "cookies.txt")
	writeCarryFile(t, boot, carryLoginRow)
	custom := filepath.Join(dir, "manual.txt")
	writeCarryFile(t, custom, carryManualRow)

	real := readCookieFile
	readCookieFile = func(name string) ([]byte, error) {
		if name == custom {
			return nil, errors.New("locked")
		}
		return real(name)
	}
	t.Cleanup(func() { readCookieFile = real })

	err := carryService(t, boot).CarryCookieFileTo(custom)
	if !errors.Is(err, ErrCookieFileUnreadable) {
		t.Errorf("err = %v, want ErrCookieFileUnreadable", err)
	}
	if got := readCarryFile(t, custom); strings.Contains(got, "from-login") || !strings.Contains(got, "from-manual") {
		t.Errorf("the unreadable target was overwritten:\n%s", got)
	}
}
