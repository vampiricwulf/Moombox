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
// cookie file a wizard's browser login writes to — with supplied recorded as
// the platforms a login or an import in this run put there (what
// FinishSetupDetailed and ImportCookies note).
func carryService(t *testing.T, bootPath string, supplied ...string) *AutoCookieService {
	t.Helper()
	s := NewAutoCookieService(t.TempDir(), bootPath, NewCookieJar(), nopAutoCookieLogger{})
	s.mu.Lock()
	for _, platform := range supplied {
		s.noteSuppliedLocked(platform)
	}
	s.mu.Unlock()
	return s
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

	if err := carryService(t, boot, "youtube").CarryCookieFileTo(custom); err != nil {
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
// The boot-time file also holds another account's Twitch session, which the
// login merges with and the Twitch check rejects: only the platform the login
// was accepted for is carried (W27 review), so that row stays out of the
// saved file.
//
// Mutants killed: CarryCookieFileTo returning before it writes;
// FinishSetupDetailed not noting an accepted platform as supplied (nothing is
// carried); noting a platform it did not accept (the other account's Twitch
// row is carried).
func TestWizardLoginSurvivesACustomCookieFile(t *testing.T) {
	root := t.TempDir()
	profileDir := writeWALCookieProfileAt(t, filepath.Join(root, "browser-profile"), youtubeAuthRows())
	bootPath := filepath.Join(root, "cookies.txt")
	customPath := filepath.Join(root, "data", "my-cookies.txt")
	writeCarryFile(t, bootPath, ".twitch.tv\tTRUE\t/\tTRUE\t0\tauth-token\tother-account")

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
	if got := readCarryFile(t, customPath); strings.Contains(got, "other-account") {
		t.Errorf("the boot-time file's Twitch session, which the login was not accepted for, was carried:\n%s", got)
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

	if err := carryService(t, boot, "youtube").CarryCookieFileTo(custom); err != nil {
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
	if err := carryService(t, boot, "youtube").CarryCookieFileTo(filepath.Join(dir, ".", "cookies.txt")); err != nil {
		t.Fatalf("the same file: %v", err)
	}
	if after := readCarryFile(t, boot); after != before {
		t.Errorf("the same file was rewritten:\n%s", after)
	}

	missing := filepath.Join(dir, "never-written.txt")
	target := filepath.Join(dir, "target.txt")
	if err := carryService(t, missing, "youtube").CarryCookieFileTo(target); err != nil {
		t.Fatalf("no boot-time file: %v", err)
	}
	empty := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := carryService(t, empty, "youtube").CarryCookieFileTo(target); err != nil {
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

	err := carryService(t, boot, "youtube").CarryCookieFileTo(custom)
	if !errors.Is(err, ErrCookieFileUnreadable) {
		t.Errorf("err = %v, want ErrCookieFileUnreadable", err)
	}
	if got := readCarryFile(t, custom); strings.Contains(got, "from-login") || !strings.Contains(got, "from-manual") {
		t.Errorf("the unreadable target was overwritten:\n%s", got)
	}
}

// TestCarryCookieFileToWithoutALoginLeavesTheFileAlone pins the W27 review of
// W26-06: a first-run setup that ran no browser login and no import carries
// nothing. The boot-time ./cookies.txt can hold another session — an earlier
// install's, a second instance started with -config in the same folder — and
// the carry merged it into the cookie file the operator named, the carried
// rows winning every clash, so the operator's own fresh export was replaced
// by the old session. The named file is now left byte for byte, and one that
// does not exist is not created, and a boot-time file that cannot be read
// refuses nothing — there was nothing to carry from it.
//
// Mutants killed: the carry as it was — no early return for a run that
// supplied nothing, and every row of the boot-time file carried (the
// operator's SAPISID is replaced and the file rewritten); dropping only the
// early return (the unreadable boot-time file refuses the setup). The row
// filter alone is TestCarryCookieFileToCarriesOnlyTheSuppliedPlatforms's.
func TestCarryCookieFileToWithoutALoginLeavesTheFileAlone(t *testing.T) {
	dir := t.TempDir()
	boot := filepath.Join(dir, "cookies.txt")
	writeCarryFile(t, boot,
		".youtube.com\tTRUE\t/\tTRUE\t0\tSAPISID\told-install-session",
		".twitch.tv\tTRUE\t/\tTRUE\t0\tauth-token\told-install-session")
	chosen := filepath.Join(dir, "fresh-export.txt")
	writeCarryFile(t, chosen, ".youtube.com\tTRUE\t/\tTRUE\t0\tSAPISID\tfresh-export-session")
	before := readCarryFile(t, chosen)

	svc := carryService(t, boot)
	if err := svc.CarryCookieFileTo(chosen); err != nil {
		t.Fatalf("CarryCookieFileTo: %v", err)
	}
	if after := readCarryFile(t, chosen); after != before {
		t.Errorf("a setup with no login rewrote the operator's cookie file:\n%s", after)
	}
	absent := filepath.Join(dir, "not-yet.txt")
	if err := svc.CarryCookieFileTo(absent); err != nil {
		t.Fatalf("CarryCookieFileTo to a new file: %v", err)
	}
	if _, err := os.Stat(absent); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a setup with no login created a cookie file (stat %v)", err)
	}

	real := readCookieFile
	readCookieFile = func(name string) ([]byte, error) {
		if name == boot {
			return nil, errors.New("locked")
		}
		return real(name)
	}
	t.Cleanup(func() { readCookieFile = real })
	if err := svc.CarryCookieFileTo(chosen); err != nil {
		t.Errorf("an unreadable boot-time file refused a setup that carries nothing: %v", err)
	}
}

// TestCarryCookieFileToCarriesOnlyTheSuppliedPlatforms: a login on one
// platform carries that platform's rows and no other. The boot-time file's
// YouTube rows are another session's; the operator's file keeps its own, and
// gets the Twitch login. The sidecar carried with them names as verified only
// the carried platforms the boot-time sidecar verified, and the named file's
// own sidecar for the rest.
//
// Mutants killed: carrying every platform's rows (the old YouTube session
// wins); carrying the boot-time sidecar's platforms unfiltered (case
// "boot-time verified YouTube"); dropping the named file's own platforms
// (case "the named file verified YouTube"); keeping the named file's verdict
// for a carried platform (case "the named file verified Twitch").
func TestCarryCookieFileToCarriesOnlyTheSuppliedPlatforms(t *testing.T) {
	dir := t.TempDir()
	boot := filepath.Join(dir, "cookies.txt")
	writeCarryFile(t, boot,
		".youtube.com\tTRUE\t/\tTRUE\t0\tSAPISID\told-install-session",
		".twitch.tv\tTRUE\t/\tTRUE\t0\tauth-token\tfrom-login")
	chosen := filepath.Join(dir, "mine.txt")
	writeCarryFile(t, chosen, ".youtube.com\tTRUE\t/\tTRUE\t0\tSAPISID\tmine")

	if err := carryService(t, boot, "twitch").CarryCookieFileTo(chosen); err != nil {
		t.Fatalf("CarryCookieFileTo: %v", err)
	}
	got := readCarryFile(t, chosen)
	if !strings.Contains(got, "from-login") || !strings.Contains(got, "\tmine") || strings.Contains(got, "old-install-session") {
		t.Errorf("carried file:\n%s\nwant the Twitch login added and the file's own YouTube row kept", got)
	}

	older := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		bootMeta []string
		ownMeta  []string // nil: the named file has no sidecar
		want     string
	}{
		{"boot-time verified YouTube", []string{"youtube", "twitch"}, nil, "twitch"},
		{"the named file verified YouTube", []string{"twitch"}, []string{"youtube"}, "twitch,youtube"},
		{"the named file verified Twitch", []string{}, []string{"twitch"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			boot := filepath.Join(dir, "cookies.txt")
			writeCarryFile(t, boot, ".twitch.tv\tTRUE\t/\tTRUE\t0\tauth-token\tfrom-login")
			if err := SaveMeta(boot, CookieMeta{LastRefresh: newer, Platforms: tc.bootMeta}); err != nil {
				t.Fatal(err)
			}
			chosen := filepath.Join(dir, "mine.txt")
			writeCarryFile(t, chosen, ".youtube.com\tTRUE\t/\tTRUE\t0\tSAPISID\tmine")
			if tc.ownMeta != nil {
				if err := SaveMeta(chosen, CookieMeta{LastRefresh: older, Platforms: tc.ownMeta}); err != nil {
					t.Fatal(err)
				}
			}
			if err := carryService(t, boot, "twitch").CarryCookieFileTo(chosen); err != nil {
				t.Fatalf("CarryCookieFileTo: %v", err)
			}
			meta, err := LoadMeta(chosen)
			if err != nil || meta == nil || !meta.LastRefresh.Equal(newer) {
				t.Fatalf("carried sidecar: %+v, %v — want the login's refresh time", meta, err)
			}
			if got := strings.Join(meta.Platforms, ","); got != tc.want {
				t.Errorf("verified platforms %q, want %q", got, tc.want)
			}
		})
	}
}

// TestImportedCookiesAreCarried: an import in this run is supplied cookies as
// a login is — the platforms whose pasted rows ImportCookies installed are
// carried — and a paste the check rejected is not.
//
// Mutants killed: ImportCookies not noting an installed platform (the import
// is not carried); noting a platform whatever its outcome (the rejected paste
// is carried).
func TestImportedCookiesAreCarried(t *testing.T) {
	for _, tc := range []struct {
		name     string
		verified bool
		carried  bool
	}{
		{"installed", true, true},
		{"rejected", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := importService(t, "", tc.verified)
			if _, err := s.ImportCookies(context.Background(), netscapeHeader+fakeYouTubeRows); err != nil {
				t.Fatalf("ImportCookies: %v", err)
			}
			target := filepath.Join(t.TempDir(), "mine.txt")
			if err := s.CarryCookieFileTo(target); err != nil {
				t.Fatalf("CarryCookieFileTo: %v", err)
			}
			data, err := os.ReadFile(target)
			if carried := err == nil && strings.Contains(string(data), "fake-sapisid-aaaa"); carried != tc.carried {
				t.Errorf("the imported rows carried = %v, want %v (%q, %v)", carried, tc.carried, data, err)
			}
		})
	}
}
