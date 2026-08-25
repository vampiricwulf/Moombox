package cookies

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// nullableFirefoxRow is a moz_cookies row where every column can be NULL.
// `nil` means the column is written as SQL NULL — which is what Firefox
// actually does: none of name, value, host, path, isHttpOnly or isSecure is
// declared NOT NULL in moz_cookies.
type nullableFirefoxRow struct {
	name     any
	value    any
	host     any
	path     any
	expiry   any
	httpOnly any
	secure   any
}

func makeNullableFirefoxCookieDB(t *testing.T, rows []nullableFirefoxRow) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "cookies.sqlite")

	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE moz_cookies (
		id INTEGER PRIMARY KEY,
		name TEXT, value TEXT, host TEXT, path TEXT,
		expiry INTEGER, isHttpOnly INTEGER, isSecure INTEGER)`); err != nil {
		t.Fatalf("create moz_cookies: %v", err)
	}
	for i, r := range rows {
		if _, err := db.Exec(
			`INSERT INTO moz_cookies (name, value, host, path, expiry, isHttpOnly, isSecure) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			r.name, r.value, r.host, r.path, r.expiry, r.httpOnly, r.secure,
		); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}
	return dbPath
}

// netscapeRowFor returns the emitted Netscape row for cookie `name`, or "".
func netscapeRowFor(lines []string, name string) string {
	for _, line := range lines {
		fields := strings.Split(strings.TrimPrefix(line, "#HttpOnly_"), "\t")
		if len(fields) >= 7 && fields[5] == name {
			return line
		}
	}
	return ""
}

// TestQueryFirefoxCookieDB_NullColumnsAreNotSilentlyDropped is the same bug
// class the NULL-expiry fix closed, on the columns it did not touch. name,
// value, host and path are all nullable in moz_cookies and scanned into bare
// Go types, so ONE NULL anywhere in a row failed rows.Scan and the bare
// `continue` dropped the whole cookie — including rows that are perfectly
// usable, like a session cookie with no value or a cookie with no path.
//
// The user-visible shape of that: a profile whose LOGIN_INFO row happens to
// carry a NULL alongside it imports "successfully" without the credential
// that makes the import worth doing.
func TestQueryFirefoxCookieDB_NullColumnsAreNotSilentlyDropped(t *testing.T) {
	dbPath := makeNullableFirefoxCookieDB(t, []nullableFirefoxRow{
		// Control: nothing NULL.
		{name: "SAPISID", value: "sapisid-value", host: ".youtube.com", path: "/", expiry: int64(0), httpOnly: int64(0), secure: int64(1)},
		// Usable rows that a NULL currently destroys.
		{name: "LOGIN_INFO", value: nil, host: ".youtube.com", path: "/", expiry: int64(0), httpOnly: int64(1), secure: int64(1)},
		{name: "HSID", value: "hsid-value", host: ".google.com", path: nil, expiry: int64(0), httpOnly: int64(1), secure: int64(0)},
		{name: "YSC", value: "ysc-value", host: ".youtube.com", path: "/", expiry: int64(0), httpOnly: nil, secure: nil},
		{name: "SID", value: "sid-value", host: ".google.com", path: "/", expiry: nil, httpOnly: int64(1), secure: int64(1)},
		// Rows that cannot be used at all: no name to send, no host to match.
		{name: nil, value: "orphan-value", host: ".youtube.com", path: "/", expiry: int64(0), httpOnly: int64(0), secure: int64(1)},
		{name: "APISID", value: "apisid-value", host: nil, path: "/", expiry: int64(0), httpOnly: int64(0), secure: int64(1)},
	})

	lines, stats, err := queryFirefoxCookieDB(dbPath)
	if err != nil {
		t.Fatalf("queryFirefoxCookieDB: %v", err)
	}

	if got := netscapeRowFor(lines, "SAPISID"); got == "" {
		t.Fatalf("fixture is broken — the control row is missing:\n%v", lines)
	}

	// A NULL value is an empty value, not a missing cookie.
	row := netscapeRowFor(lines, "LOGIN_INFO")
	if row == "" {
		t.Errorf("a NULL value dropped the whole cookie:\n%v", lines)
	} else if fields := strings.Split(strings.TrimPrefix(row, "#HttpOnly_"), "\t"); fields[6] != "" {
		t.Errorf("NULL value became %q, want the empty string", fields[6])
	}

	// A NULL path is "no path restriction", i.e. "/".
	row = netscapeRowFor(lines, "HSID")
	if row == "" {
		t.Errorf("a NULL path dropped the whole cookie:\n%v", lines)
	} else if fields := strings.Split(strings.TrimPrefix(row, "#HttpOnly_"), "\t"); fields[2] != "/" {
		t.Errorf("NULL path became %q, want \"/\"", fields[2])
	}

	// NULL flags mean "not set", not "drop the cookie". isSecure defaults
	// the conservative way: the two guesses are not symmetric, since
	// "insecure" would send a session credential over plaintext while
	// "secure" merely withholds it.
	row = netscapeRowFor(lines, "YSC")
	if row == "" {
		t.Errorf("NULL isHttpOnly/isSecure dropped the whole cookie:\n%v", lines)
	} else {
		if strings.HasPrefix(row, "#HttpOnly_") {
			t.Errorf("NULL isHttpOnly became httpOnly=true: %q", row)
		}
		if fields := strings.Split(row, "\t"); fields[3] != "TRUE" {
			t.Errorf("NULL isSecure became %q, want TRUE — an unknown secure flag must not downgrade the cookie", fields[3])
		}
	}

	// The two unusable rows stay out of the file — but they are COUNTED, so
	// the drop can be explained instead of merely happening.
	if got := netscapeRowFor(lines, "APISID"); got != "" {
		t.Errorf("a cookie with no host was emitted with an empty domain: %q", got)
	}
	if stats.droppedNoName != 1 {
		t.Errorf("droppedNoName = %d, want 1 — a nameless row must be counted, not silently skipped", stats.droppedNoName)
	}
	if stats.droppedNoHost != 1 {
		t.Errorf("droppedNoHost = %d, want 1 — a hostless row must be counted, not silently skipped", stats.droppedNoHost)
	}
	if stats.scanErrors != 0 {
		t.Errorf("scanErrors = %d, want 0 — NULLs are expected, not scan failures", stats.scanErrors)
	}
	// A NULL expiry maps to the session-cookie sentinel, and that mapping is
	// counted like every other one — it is the column whose NULL handling
	// landed first, and it must not be the one that stays invisible.
	row = netscapeRowFor(lines, "SID")
	if row == "" {
		t.Errorf("a NULL expiry dropped the whole cookie:\n%v", lines)
	} else if fields := strings.Split(strings.TrimPrefix(row, "#HttpOnly_"), "\t"); fields[4] != "0" {
		t.Errorf("NULL expiry became %q, want the session sentinel 0", fields[4])
	}

	if stats.rows != 7 {
		t.Errorf("rows = %d, want 7", stats.rows)
	}
	if stats.defaulted != 4 {
		t.Errorf("defaulted = %d, want 4 (NULL value, path, flags, expiry)", stats.defaulted)
	}
}

// TestQueryFirefoxCookieDB_ReportsSchemaProbeOutcome covers the other half of
// the invisibility: firefoxSchemaVersion degrades to 0 on ANY failure, which
// is the safe direction but was completely unobservable. A database with no
// readable user_version must say so rather than pass for a genuine schema 0.
func TestQueryFirefoxCookieDB_ReportsSchemaProbeOutcome(t *testing.T) {
	rows := []firefoxRow{{name: "SAPISID", value: "v", host: ".youtube.com", path: "/", expiry: ptrInt64(0)}}

	if _, stats, err := queryFirefoxCookieDB(makeFirefoxCookieDB(t, 16, true, rows)); err != nil {
		t.Fatalf("queryFirefoxCookieDB: %v", err)
	} else {
		if !stats.schemaKnown {
			t.Error("a stamped user_version must be reported as known")
		}
		if stats.schemaVersion != 16 {
			t.Errorf("schemaVersion = %d, want 16", stats.schemaVersion)
		}
	}

	// PRAGMA user_version on a database that was never stamped returns 0,
	// which is indistinguishable from "pre-Firefox-142" — and that is
	// exactly the point: the read is reported, and the caller decides how
	// loudly to say it.
	if _, stats, err := queryFirefoxCookieDB(makeFirefoxCookieDB(t, 0, false, rows)); err != nil {
		t.Fatalf("queryFirefoxCookieDB: %v", err)
	} else if stats.schemaVersion != 0 {
		t.Errorf("schemaVersion = %d, want 0 for an unstamped database", stats.schemaVersion)
	}
}
