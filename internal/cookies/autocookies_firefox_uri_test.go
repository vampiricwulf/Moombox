package cookies

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestFirefoxCookieDBPathTakenLiterally: a cookies.sqlite under a directory
// holding '#', '?' or %41 must be read, both directly and through the temp
// directory snapshot readFirefoxCookies opens — where the '#' sits in TMPDIR
// (on Windows %TEMP%, under a user name that may hold one). Pasted into the
// DSN raw, SQLite opened a truncated or decoded path, and the read failed as
// "no such table: moz_cookies" or "unable to open database file", which
// classifyCookieDBError then reported as a corrupt cookie database.
//
// MUTANT: build queryFirefoxCookieDB's DSN as "file:"+path again (every row
// fails).
func TestFirefoxCookieDBPathTakenLiterally(t *testing.T) {
	rows := []nullableFirefoxRow{{name: "SAPISID", value: "v", host: ".youtube.com", path: "/", expiry: int64(0), httpOnly: int64(0), secure: int64(1)}}
	cases := []struct {
		name, dir string
		unixOnly  bool
	}{
		{"hash", "a#b", false},
		{"question", "a?b", true},
		{"percent escape", "a%41b", false},
	}
	for _, tc := range cases {
		t.Run("direct "+tc.name, func(t *testing.T) {
			if tc.unixOnly && runtime.GOOS == "windows" {
				t.Skip("not a legal Windows path")
			}
			data, err := os.ReadFile(makeNullableFirefoxCookieDB(t, rows))
			if err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(t.TempDir(), tc.dir, "cookies.sqlite")
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, data, 0o644); err != nil {
				t.Fatal(err)
			}
			lines, _, err := queryFirefoxCookieDB(p)
			if err != nil || len(lines) != 1 {
				t.Errorf("queryFirefoxCookieDB(%q) = %d lines, err %v; want the 1 cookie", p, len(lines), err)
			}
		})
	}
	t.Run("snapshot under a temp directory holding a hash", func(t *testing.T) {
		src := makeNullableFirefoxCookieDB(t, rows)
		tmp := filepath.Join(t.TempDir(), "tmp #1")
		if err := os.MkdirAll(tmp, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, env := range []string{"TMPDIR", "TMP", "TEMP"} {
			t.Setenv(env, tmp)
		}
		if got := os.TempDir(); got != tmp {
			t.Fatalf("os.TempDir() = %q, want %q -- the snapshot would not land under the hash", got, tmp)
		}
		out, _, err := readFirefoxCookies(filepath.Dir(src))
		if err != nil || out == "" {
			t.Errorf("readFirefoxCookies with the temp directory %q: out %q, err %v; want the cookie", tmp, out, err)
		}
	})
}
