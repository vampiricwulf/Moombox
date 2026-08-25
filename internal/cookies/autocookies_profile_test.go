package cookies

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// profileTestCookie is one row for the synthetic Firefox cookie DB.
type profileTestCookie struct {
	name     string
	value    string
	host     string
	path     string
	expiry   int64
	httpOnly bool
	secure   bool
}

// youtubeAuthRows returns a cookie set that satisfies
// CookieJar.HasYouTubeAuthCookies (SAPISID + LOGIN_INFO) plus a couple of
// rotating tokens, so an imported profile can actually flip auth on.
func youtubeAuthRows() []profileTestCookie {
	return []profileTestCookie{
		{name: "SAPISID", value: "sapisid-from-profile", host: ".youtube.com", path: "/", expiry: 0, secure: true},
		{name: "LOGIN_INFO", value: "login-info-from-profile", host: ".youtube.com", path: "/", expiry: 0, httpOnly: true, secure: true},
		{name: "__Secure-3PAPISID", value: "3papisid-from-profile", host: ".youtube.com", path: "/", expiry: 0, secure: true},
		{name: "__Secure-1PSIDTS", value: "1psidts-from-profile", host: ".youtube.com", path: "/", expiry: 0, httpOnly: true, secure: true},
		{name: "HSID", value: "hsid-from-profile", host: ".google.com", path: "/", expiry: 0, httpOnly: true, secure: false},
	}
}

// writeWALCookieProfile builds a Firefox-shaped profile directory whose
// cookies.sqlite is in WAL mode with the rows still sitting UNCHECKPOINTED
// in the -wal sidecar. The returned db handle is deliberately left OPEN for
// the lifetime of the test (closing the last connection checkpoints the WAL
// and deletes the sidecar, which is exactly the state we must NOT test in).
//
// This is the empirically-verified failure shape the import path exists to
// survive: copying cookies.sqlite alone out of a profile in this state opens
// cleanly and returns ZERO rows with no error. yt-dlp has that bug
// (yt_dlp/cookies.py shutil.copy of the main file only); Moombox must not.
func writeWALCookieProfile(t *testing.T, rows []profileTestCookie) string {
	t.Helper()
	profileDir := t.TempDir()
	dbPath := filepath.Join(profileDir, "cookies.sqlite")

	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open synthetic cookies.sqlite: %v", err)
	}
	// Pin a single connection so journal_mode/wal_autocheckpoint apply to the
	// connection that then holds the WAL open.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() { db.Close() })

	var mode string
	if err := db.QueryRow("PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		t.Fatalf("set journal_mode=WAL: %v", err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
	if _, err := db.Exec(`CREATE TABLE moz_cookies (
		id INTEGER PRIMARY KEY,
		name TEXT, value TEXT, host TEXT, path TEXT,
		expiry INTEGER, isHttpOnly INTEGER, isSecure INTEGER)`); err != nil {
		t.Fatalf("create moz_cookies: %v", err)
	}
	// Flush the SCHEMA into the main database, then stop checkpointing. This
	// is the real-world shape: a long-lived Firefox profile has its tables
	// committed to cookies.sqlite while only recent cookie writes are still
	// sitting in the -wal. Without this step the main file would not even
	// carry moz_cookies, and the "main file alone" control below would fail
	// with a schema error instead of the silent empty result we care about.
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatalf("checkpoint schema: %v", err)
	}
	if _, err := db.Exec("PRAGMA wal_autocheckpoint=0"); err != nil {
		t.Fatalf("disable wal autocheckpoint: %v", err)
	}
	for _, c := range rows {
		if _, err := db.Exec(
			"INSERT INTO moz_cookies (name, value, host, path, expiry, isHttpOnly, isSecure) VALUES (?,?,?,?,?,?,?)",
			c.name, c.value, c.host, c.path, c.expiry, boolToInt(c.httpOnly), boolToInt(c.secure),
		); err != nil {
			t.Fatalf("insert cookie %s: %v", c.name, err)
		}
	}

	// Guard: if the -wal sidecar is missing or empty the rows already landed
	// in the main file and this test would prove nothing.
	walPath := dbPath + "-wal"
	info, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("expected uncheckpointed %s to exist: %v", filepath.Base(walPath), err)
	}
	if info.Size() == 0 {
		t.Fatalf("%s is empty — rows were checkpointed into the main DB", filepath.Base(walPath))
	}
	return profileDir
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// TestSnapshotCopiesWALSidecars is the core regression this package exists
// for. Two halves:
//
//	main file only          -> opens fine, returns 0 rows, no error (the trap)
//	main + -wal (+ -shm)    -> returns every row
func TestSnapshotCopiesWALSidecars(t *testing.T) {
	profileDir := writeWALCookieProfile(t, youtubeAuthRows())

	t.Run("main file alone is silently empty", func(t *testing.T) {
		decoy := t.TempDir()
		src := filepath.Join(profileDir, "cookies.sqlite")
		dst := filepath.Join(decoy, "cookies.sqlite")
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("read main db: %v", err)
		}
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			t.Fatalf("write decoy db: %v", err)
		}
		lines, err := queryFirefoxCookieDB(dst)
		if err != nil {
			t.Fatalf("main-file-only copy: unexpected error %v (the trap is that it SUCCEEDS with no rows)", err)
		}
		if len(lines) != 0 {
			t.Fatalf("main-file-only copy returned %d rows; the WAL sidecars are no longer load-bearing — re-check this test's premise", len(lines))
		}
	})

	t.Run("snapshot with sidecars returns every row", func(t *testing.T) {
		snapDir, cleanup, err := snapshotFirefoxCookieDB(profileDir)
		if err != nil {
			t.Fatalf("snapshotFirefoxCookieDB: %v", err)
		}
		defer cleanup()

		if _, err := os.Stat(filepath.Join(snapDir, "cookies.sqlite-wal")); err != nil {
			t.Fatalf("snapshot is missing the -wal sidecar: %v", err)
		}
		lines, err := queryFirefoxCookieDB(filepath.Join(snapDir, "cookies.sqlite"))
		if err != nil {
			t.Fatalf("query snapshot: %v", err)
		}
		if len(lines) != len(youtubeAuthRows()) {
			t.Fatalf("snapshot returned %d rows, want %d — WAL contents were dropped", len(lines), len(youtubeAuthRows()))
		}
	})

	// The measurement behind dropping -shm from the copy list. -shm is pure
	// derived state (the WAL index) that SQLite rebuilds from the -wal, and
	// copying it AFTER the -wal means the copied index can describe more
	// frames than the copied log actually contains — the one stale-WAL vector
	// a snapshot could introduce. This asserts the copy is complete without
	// it, which is what makes dropping it safe.
	t.Run("main plus wal without shm returns every row", func(t *testing.T) {
		bare := t.TempDir()
		src := filepath.Join(profileDir, "cookies.sqlite")
		for _, suffix := range []string{"", "-wal"} {
			data, err := os.ReadFile(src + suffix)
			if err != nil {
				t.Fatalf("read %s: %v", src+suffix, err)
			}
			if err := os.WriteFile(filepath.Join(bare, "cookies.sqlite"+suffix), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := os.Stat(filepath.Join(bare, "cookies.sqlite-shm")); err == nil {
			t.Fatal("fixture error: -shm should not have been copied")
		}
		lines, err := queryFirefoxCookieDB(filepath.Join(bare, "cookies.sqlite"))
		if err != nil {
			t.Fatalf("query main+wal copy: %v", err)
		}
		if len(lines) != len(youtubeAuthRows()) {
			t.Fatalf("main+wal returned %d rows, want %d — -shm would have to be copied after all",
				len(lines), len(youtubeAuthRows()))
		}
	})

	t.Run("cleanup removes the snapshot", func(t *testing.T) {
		snapDir, cleanup, err := snapshotFirefoxCookieDB(profileDir)
		if err != nil {
			t.Fatalf("snapshotFirefoxCookieDB: %v", err)
		}
		cleanup()
		if _, err := os.Stat(snapDir); !os.IsNotExist(err) {
			t.Fatalf("snapshot dir %s still present after cleanup (stat err = %v) — session cookies left on disk", snapDir, err)
		}
	})
}

// TestImportProfileCookiesReadsUncheckpointedWAL proves the browser-free
// import path returns rows that only exist in the -wal sidecar.
func TestImportProfileCookiesReadsUncheckpointedWAL(t *testing.T) {
	profileDir := writeWALCookieProfile(t, youtubeAuthRows())
	s := NewAutoCookieService(profileDir, filepath.Join(t.TempDir(), "cookies.txt"), NewCookieJar(), nopAutoCookieLogger{})

	netscape, err := s.importProfileCookies()
	if err != nil {
		t.Fatalf("importProfileCookies: %v", err)
	}
	for _, want := range []string{"SAPISID", "LOGIN_INFO", "sapisid-from-profile", "login-info-from-profile"} {
		if !strings.Contains(netscape, want) {
			t.Errorf("imported cookie file missing %q:\n%s", want, netscape)
		}
	}
	if got := countNetscapeCookieRows(netscape); got != len(youtubeAuthRows()) {
		t.Errorf("countNetscapeCookieRows = %d, want %d", got, len(youtubeAuthRows()))
	}
}

// TestImportProfileCookiesZeroRelevantIsLoudError pins requirement: an empty
// cookie set is never a silent success. This is the exact signature of a
// dropped WAL and of a profile copied out from under a live Firefox.
func TestImportProfileCookiesZeroRelevantIsLoudError(t *testing.T) {
	profileDir := writeWALCookieProfile(t, []profileTestCookie{
		{name: "sessionid", value: "x", host: ".example.com", path: "/"},
		{name: "csrftoken", value: "y", host: ".not-youtube.invalid", path: "/"},
	})
	s := NewAutoCookieService(profileDir, filepath.Join(t.TempDir(), "cookies.txt"), NewCookieJar(), nopAutoCookieLogger{})

	netscape, err := s.importProfileCookies()
	if err == nil {
		t.Fatalf("importProfileCookies with zero relevant cookies returned no error (got %q) — silent empty jars are the bug", netscape)
	}
	if !errors.Is(err, ErrNoCookiesInProfile) {
		t.Fatalf("want errors.Is(err, ErrNoCookiesInProfile), got %v", err)
	}
}

func TestImportProfileCookiesFailureModes(t *testing.T) {
	t.Run("profile dir missing", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "nope")
		s := NewAutoCookieService(missing, "", NewCookieJar(), nopAutoCookieLogger{})
		_, err := s.importProfileCookies()
		if !errors.Is(err, ErrProfileNotFound) {
			t.Fatalf("want ErrProfileNotFound, got %v", err)
		}
	})

	t.Run("profile path is a file", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "profile")
		if err := os.WriteFile(f, []byte("not a dir"), 0o600); err != nil {
			t.Fatal(err)
		}
		s := NewAutoCookieService(f, "", NewCookieJar(), nopAutoCookieLogger{})
		_, err := s.importProfileCookies()
		if !errors.Is(err, ErrProfileNotADirectory) {
			t.Fatalf("want ErrProfileNotADirectory, got %v", err)
		}
	})

	t.Run("cookies.sqlite absent", func(t *testing.T) {
		dir := t.TempDir()
		s := NewAutoCookieService(dir, "", NewCookieJar(), nopAutoCookieLogger{})
		_, err := s.importProfileCookies()
		if !errors.Is(err, ErrCookieDBNotFound) {
			t.Fatalf("want ErrCookieDBNotFound, got %v", err)
		}
		if !strings.Contains(err.Error(), "Firefox") {
			t.Errorf("error should name the browser whose profile shape we expect: %v", err)
		}
	})

	t.Run("corrupt cookies.sqlite", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "cookies.sqlite"), []byte("this is not a database"), 0o600); err != nil {
			t.Fatal(err)
		}
		s := NewAutoCookieService(dir, "", NewCookieJar(), nopAutoCookieLogger{})
		_, err := s.importProfileCookies()
		if err == nil {
			t.Fatal("corrupt DB returned no error")
		}
		if errors.Is(err, ErrNoCookiesInProfile) {
			t.Fatalf("corrupt DB must not be reported as an empty profile: %v", err)
		}
		if !errors.Is(err, ErrCookieDBUnreadable) {
			t.Fatalf("want ErrCookieDBUnreadable, got %v", err)
		}
	})
}

// TestLockedDBErrorIsClassified covers the "Firefox is running and holds an
// exclusive lock" mode. Recent Firefox defaults
// storage.sqlite.exclusiveLock.enabled=true, which blocks external readers —
// the message must tell the user to stop Firefox rather than read as a
// generic query failure.
func TestLockedDBErrorIsClassified(t *testing.T) {
	for _, raw := range []string{
		"database is locked (5) (SQLITE_BUSY)",
		"SQLITE_BUSY: database is locked",
		"The process cannot access the file because it is being used by another process.",
	} {
		if !isLockedDBError(errors.New(raw)) {
			t.Errorf("isLockedDBError(%q) = false, want true", raw)
		}
	}
	for _, raw := range []string{
		"file is not a database",
		"no such table: moz_cookies",
	} {
		if isLockedDBError(errors.New(raw)) {
			t.Errorf("isLockedDBError(%q) = true, want false", raw)
		}
	}

	wrapped := classifyCookieDBError(fmt.Errorf("query cookies: %w", errors.New("database is locked (5)")))
	if !errors.Is(wrapped, ErrCookieDBLocked) {
		t.Fatalf("classifyCookieDBError(locked) = %v, want ErrCookieDBLocked", wrapped)
	}
	if !strings.Contains(wrapped.Error(), "Firefox") {
		t.Errorf("locked-DB message should tell the user to close Firefox: %v", wrapped)
	}
}

// TestRefreshCookiesImportsProfileWhenNoBrowser is the end-to-end shape a
// container hits: DetectBrowser finds nothing, but the mounted profile has a
// readable cookies.sqlite, so the refresh imports instead of bailing.
func TestRefreshCookiesImportsProfileWhenNoBrowser(t *testing.T) {
	profileDir := writeWALCookieProfile(t, youtubeAuthRows())
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")

	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return nil } // container: no browser
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }

	ok, err := s.RefreshCookies(context.Background())
	if err != nil {
		t.Fatalf("RefreshCookies (browserless import): %v", err)
	}
	if !ok {
		t.Fatal("RefreshCookies (browserless import) = false, want true")
	}
	data, err := os.ReadFile(cookiePath)
	if err != nil {
		t.Fatalf("read written cookies.txt: %v", err)
	}
	if !strings.Contains(string(data), "login-info-from-profile") {
		t.Errorf("cookies.txt does not contain the imported profile cookies:\n%s", data)
	}
	if !s.jar.HasYouTubeAuthCookies() {
		t.Error("jar was not reloaded with the imported YouTube auth cookies")
	}
}

// TestRefreshCookiesNoBrowserNoProfileStillReportsNoBrowser keeps the
// historical answer for an install that has neither a browser nor a profile:
// the actionable advice there really is "install a browser / run setup".
func TestRefreshCookiesNoBrowserNoProfileStillReportsNoBrowser(t *testing.T) {
	s := NewAutoCookieService(filepath.Join(t.TempDir(), "absent"), "", NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return nil }

	ok, err := s.RefreshCookies(context.Background())
	if ok {
		t.Fatal("RefreshCookies with no browser and no profile = true, want false")
	}
	if !errors.Is(err, ErrNoBrowserFound) {
		t.Fatalf("want ErrNoBrowserFound, got %v", err)
	}
}

// TestRefreshCookiesRestoresCookiesWhenImportFailsVerification pins the
// "never clobber a good cookies.txt with a worse one" rule on a single
// -platform install: the only platform present verified before the import and
// does not after, so its previous credentials must come back — and, because
// they do, the refresh ends up healthy rather than failed.
func TestRefreshCookiesRestoresCookiesWhenImportFailsVerification(t *testing.T) {
	profileDir := writeWALCookieProfile(t, youtubeAuthRows())
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")

	original := "# Netscape HTTP Cookie File\n" +
		".youtube.com\tTRUE\t/\tTRUE\t0\tSAPISID\tgood-sapisid\n" +
		"#HttpOnly_.youtube.com\tTRUE\t/\tTRUE\t0\tLOGIN_INFO\tgood-login-info\n"
	if err := os.WriteFile(cookiePath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return nil }
	// Only the credential already on disk works; anything the profile brings
	// does not. That is a regression, not a flat failure.
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) {
		return s.jar.GetSapisid() == "good-sapisid", nil
	}
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }

	ok, err := s.RefreshCookies(context.Background())
	if err != nil {
		t.Fatalf("RefreshCookies: %v", err)
	}
	if !ok {
		t.Fatal("RefreshCookies = false; the working credentials were restored, so the end state is healthy")
	}
	data, err := os.ReadFile(cookiePath)
	if err != nil {
		t.Fatalf("read cookies.txt: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "good-sapisid") || !strings.Contains(got, "good-login-info") {
		t.Errorf("the working credentials were not restored:\n%s", got)
	}
	if strings.Contains(got, "sapisid-from-profile") {
		t.Errorf("the failing imported credentials survived the rollback:\n%s", got)
	}
}

// TestFinishSetupTreatsEmptyProfileAsNoLogin guards the one caller that has a
// legitimate empty-profile state. readFirefoxCookies now errors on an empty
// read (a silently empty jar is the bug it exists to catch), but a user who
// opened the login browser and closed it without signing in must still get
// "no login detected" in the setup dialog rather than a hard failure — the
// Web UI throws on any non-2xx and would otherwise render "HTTP 422".
func TestFinishSetupTreatsEmptyProfileAsNoLogin(t *testing.T) {
	profileDir := writeWALCookieProfile(t, []profileTestCookie{
		{name: "sessionid", value: "x", host: ".example.com", path: "/"},
	})
	s := NewAutoCookieService(profileDir, filepath.Join(t.TempDir(), "cookies.txt"), NewCookieJar(), nopAutoCookieLogger{})
	s.setupProcess = &os.Process{Pid: -1}
	s.setupBrowser = &DetectedBrowser{Type: "firefox", Path: "firefox", Name: "Firefox"}
	s.browserExited = true // skip the graceful-close wait

	ytAuth, twAuth, err := s.FinishSetup(context.Background())
	if err != nil {
		t.Fatalf("FinishSetup on an empty profile should not be a hard error, got %v", err)
	}
	if ytAuth || twAuth {
		t.Fatalf("FinishSetup on an empty profile = (%v, %v), want (false, false)", ytAuth, twAuth)
	}
	status := s.GetStatus()
	if status.LastError == nil || !strings.Contains(*status.LastError, "no login detected") {
		t.Errorf("LastError should explain the empty profile, got %v", status.LastError)
	}
}

// TestShouldSeedFromProfileAtStartup covers the startup one-shot gate: it
// fires only when there is no browser to drive AND the profile actually
// holds a cookie DB to import.
func TestShouldSeedFromProfileAtStartup(t *testing.T) {
	profileDir := writeWALCookieProfile(t, youtubeAuthRows())

	t.Run("browserless with cookie db", func(t *testing.T) {
		s := NewAutoCookieService(profileDir, "", NewCookieJar(), nopAutoCookieLogger{})
		s.detectBrowser = func() *DetectedBrowser { return nil }
		if !s.shouldSeedFromProfileAtStartup() {
			t.Error("want true: no browser but a readable profile is the container case")
		}
	})

	t.Run("browser present", func(t *testing.T) {
		s := NewAutoCookieService(profileDir, "", NewCookieJar(), nopAutoCookieLogger{})
		s.detectBrowser = func() *DetectedBrowser {
			return &DetectedBrowser{Type: "firefox", Path: "/usr/bin/firefox", Name: "Firefox"}
		}
		if s.shouldSeedFromProfileAtStartup() {
			t.Error("want false: a real browser drives the normal refresh path")
		}
	})

	t.Run("no cookie db", func(t *testing.T) {
		s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})
		s.detectBrowser = func() *DetectedBrowser { return nil }
		if s.shouldSeedFromProfileAtStartup() {
			t.Error("want false: nothing to import")
		}
	})

	t.Run("unconfigured profile dir", func(t *testing.T) {
		s := NewAutoCookieService("", "", NewCookieJar(), nopAutoCookieLogger{})
		s.detectBrowser = func() *DetectedBrowser { return nil }
		if s.shouldSeedFromProfileAtStartup() {
			t.Error("want false: auto-cookies not configured")
		}
	})
}
