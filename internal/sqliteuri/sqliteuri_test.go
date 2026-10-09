package sqliteuri

import "testing"

// TestFileURIChangesOnlyWhatSQLiteWouldMisread pins both halves of the
// escaping: '%', '?', '#' and a leading "//" are escaped, and a path with none
// of them is passed through untouched, so the relative default and Windows
// paths (drive letter, backslashes) read exactly as they always have. The
// opens themselves are tested where the URIs are used: database.Open
// (TestOpenTakesTheDatabasePathLiterally) and the cookie readers.
//
// MUTANTS: drop any one of the three escapes, or the "//" authority (that row
// fails); url.PathEscape-style escaping of the whole path (the space, the
// colon or the backslashes change).
func TestFileURIChangesOnlyWhatSQLiteWouldMisread(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"./moombox.db":                   "file:./moombox.db",
		`C:\Users\me\Moombox\moombox.db`: `file:C:\Users\me\Moombox\moombox.db`,
		"/srv/My Moombox/moombox.db":     "file:/srv/My Moombox/moombox.db",
		"/srv/Moombox #2/moombox.db":     "file:/srv/Moombox %232/moombox.db",
		"/srv/a?b/100%/moombox.db":       "file:/srv/a%3Fb/100%25/moombox.db",
		"//srv/moombox.db":               "file:////srv/moombox.db",
	} {
		if got := FileURI(in); got != want {
			t.Errorf("FileURI(%q) = %q, want %q", in, got, want)
		}
	}
}
