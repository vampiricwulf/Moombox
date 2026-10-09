package dpapi

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/sqliteuri"
)

// TestOpenCookieDBTakesThePathLiterally: a Chromium Cookies file under a
// profile path holding '#', '?' or %41 must be the file openCookieDB reads,
// and read-only. Pasted into the DSN raw, the '#' and '?' cut the path short
// (SQLite opened, or created, a file that was not the profile's) and "%41" was
// decoded, so the open failed.
//
// MUTANTS: build the DSN as "file:"+path again (every row reads no meta
// version); drop mode=ro (the write succeeds).
func TestOpenCookieDBTakesThePathLiterally(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, dir string
		unixOnly  bool
	}{
		{"hash", "Profile #2", false},
		{"question", "a?b", true},
		{"percent escape", "a%41b", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.unixOnly && runtime.GOOS == "windows" {
				t.Skip("not a legal Windows path")
			}
			p := filepath.Join(t.TempDir(), tc.dir, "Cookies")
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			fixture, err := sql.Open("sqlite", sqliteuri.FileURI(p))
			if err != nil {
				t.Fatal(err)
			}
			for _, stmt := range []string{chromeMetaTable, `INSERT INTO meta (key, value) VALUES ('version', '24')`} {
				if _, err := fixture.Exec(stmt); err != nil {
					t.Fatalf("exec %q: %v", stmt, err)
				}
			}
			fixture.Close()

			db, err := openCookieDB(p)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if v, ok := readChromeMetaVersion(db); !ok || v != 24 {
				t.Errorf("readChromeMetaVersion through openCookieDB(%q) = %d, %v; want 24 from the profile's own file", p, v, ok)
			}
			if _, err := db.Exec(`INSERT INTO meta (key, value) VALUES ('written', 'x')`); err == nil {
				t.Errorf("openCookieDB(%q) let a write through; the browser's database must be opened mode=ro", p)
			}
		})
	}
}
