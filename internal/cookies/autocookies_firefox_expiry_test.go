package cookies

import (
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// firefoxRow is one moz_cookies row for the fixture builder. expiry is a
// *int64 so a NULL expiry (which Firefox does write for some rows) can be
// expressed.
type firefoxRow struct {
	name   string
	value  string
	host   string
	path   string
	expiry *int64
}

// makeFirefoxCookieDB writes a minimal cookies.sqlite that matches the
// columns queryFirefoxCookieDB selects, stamped with the given
// `PRAGMA user_version` schema version. Returns the DB path.
//
// setVersion=false leaves user_version at SQLite's default of 0, which is
// what a corrupt / truncated / non-Firefox database looks like to the
// version probe.
func makeFirefoxCookieDB(t *testing.T, schemaVersion int, setVersion bool, rows []firefoxRow) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "cookies.sqlite")

	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE moz_cookies (
		id INTEGER PRIMARY KEY,
		name TEXT,
		value TEXT,
		host TEXT,
		path TEXT,
		expiry INTEGER,
		isHttpOnly INTEGER,
		isSecure INTEGER
	)`); err != nil {
		t.Fatalf("create moz_cookies: %v", err)
	}
	if setVersion {
		if _, err := db.Exec("PRAGMA user_version = " + strconv.Itoa(schemaVersion)); err != nil {
			t.Fatalf("set user_version: %v", err)
		}
	}
	for _, r := range rows {
		var exp any
		if r.expiry != nil {
			exp = *r.expiry
		}
		if _, err := db.Exec(
			`INSERT INTO moz_cookies (name, value, host, path, expiry, isHttpOnly, isSecure) VALUES (?, ?, ?, ?, ?, 1, 1)`,
			r.name, r.value, r.host, r.path, exp,
		); err != nil {
			t.Fatalf("insert row %q: %v", r.name, err)
		}
	}
	return dbPath
}

// netscapeExpiry pulls the expiry field (5th tab-separated column) out of
// the Netscape row that queryFirefoxCookieDB emits for cookie `name`.
func netscapeExpiry(t *testing.T, lines []string, name string) int64 {
	t.Helper()
	for _, line := range lines {
		fields := strings.Split(strings.TrimPrefix(line, "#HttpOnly_"), "\t")
		if len(fields) < 7 || fields[5] != name {
			continue
		}
		exp, err := strconv.ParseInt(fields[4], 10, 64)
		if err != nil {
			t.Fatalf("expiry field %q is not an integer: %v", fields[4], err)
		}
		return exp
	}
	t.Fatalf("cookie %q not found in output: %#v", name, lines)
	return 0
}

func ptrInt64(v int64) *int64 { return &v }

// TestQueryFirefoxCookieDB_ExpiryUnitsBySchemaVersion is the gate test for
// the Firefox 142 change: schema version 16 (and up) stores moz_cookies.expiry
// in MILLISECONDS, everything before it stores SECONDS. Both sides of the gate
// are asserted — a test that only exercised the >= 16 path would pass even if
// the code divided unconditionally.
//
// Ref: mozilla-firefox commit 5869af852cd20425165837f6c2d9971f3efba83d,
// mirrored from yt-dlp `_extract_firefox_cookies` (yt_dlp/cookies.py:189-192).
func TestQueryFirefoxCookieDB_ExpiryUnitsBySchemaVersion(t *testing.T) {
	// 2035-01-01T00:00:00Z — comfortably in the future either way, so the
	// value is compared directly rather than through an "is it expired" proxy.
	const expirySeconds int64 = 2051222400
	const expiryMillis int64 = expirySeconds * 1000

	tests := []struct {
		name          string
		schemaVersion int
		setVersion    bool
		storedExpiry  int64
		wantExpiry    int64
	}{
		{"schema 0 (unversioned) keeps seconds", 0, true, expirySeconds, expirySeconds},
		{"schema 15 (pre-FF142) keeps seconds", 15, true, expirySeconds, expirySeconds},
		{"schema 16 (FF142) converts milliseconds", 16, true, expiryMillis, expirySeconds},
		{"schema 17 (future) converts milliseconds", 17, true, expiryMillis, expirySeconds},
		{"missing user_version falls back to seconds", 0, false, expirySeconds, expirySeconds},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dbPath := makeFirefoxCookieDB(t, tt.schemaVersion, tt.setVersion, []firefoxRow{
				{name: "SID", value: "abc123", host: ".youtube.com", path: "/", expiry: ptrInt64(tt.storedExpiry)},
			})

			lines, err := queryFirefoxCookieDB(dbPath)
			if err != nil {
				t.Fatalf("queryFirefoxCookieDB: %v", err)
			}
			if got := netscapeExpiry(t, lines, "SID"); got != tt.wantExpiry {
				t.Errorf("expiry = %d, want %d (schema %d, stored %d)",
					got, tt.wantExpiry, tt.schemaVersion, tt.storedExpiry)
			}
		})
	}
}

// TestQueryFirefoxCookieDB_NullExpiry covers the row shape upstream guards
// with `expiry is not None`: a NULL expiry must not drop the cookie, and must
// come out as 0 (the Netscape "session cookie" sentinel, which the merge
// pruner deliberately never treats as expired).
func TestQueryFirefoxCookieDB_NullExpiry(t *testing.T) {
	for _, schemaVersion := range []int{15, 16} {
		t.Run("schema "+strconv.Itoa(schemaVersion), func(t *testing.T) {
			dbPath := makeFirefoxCookieDB(t, schemaVersion, true, []firefoxRow{
				{name: "SID", value: "abc123", host: ".youtube.com", path: "/", expiry: nil},
			})

			lines, err := queryFirefoxCookieDB(dbPath)
			if err != nil {
				t.Fatalf("queryFirefoxCookieDB: %v", err)
			}
			if got := netscapeExpiry(t, lines, "SID"); got != 0 {
				t.Errorf("NULL expiry = %d, want 0", got)
			}
		})
	}
}

// TestQueryFirefoxCookieDB_ExpiredCookieStillExpiredAfterConversion is the
// user-visible consequence of the bug: mergeCookieFiles prunes rows whose
// expiry is in the past (rowExpired), so a millisecond expiry read raw is
// ~1000x too large and a genuinely-expired Firefox cookie can never be pruned.
func TestQueryFirefoxCookieDB_ExpiredCookieStillExpiredAfterConversion(t *testing.T) {
	// 2001-09-09T01:46:40Z — long past.
	const pastSeconds int64 = 1000000000

	dbPath := makeFirefoxCookieDB(t, 16, true, []firefoxRow{
		{name: "SID", value: "stale", host: ".youtube.com", path: "/", expiry: ptrInt64(pastSeconds * 1000)},
	})

	lines, err := queryFirefoxCookieDB(dbPath)
	if err != nil {
		t.Fatalf("queryFirefoxCookieDB: %v", err)
	}
	// now = 2020-01-01T00:00:00Z, after pastSeconds either way.
	const now int64 = 1577836800
	if !rowExpired(lines[0], now) {
		t.Errorf("row %q should read as expired at %d after ms->s conversion", lines[0], now)
	}
}
