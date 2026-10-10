// Package sqliteuri builds the "file:" URI every SQLite database Moombox
// opens goes through: its own job database and the browsers' cookie
// databases alike.
package sqliteuri

import "strings"

// pathEscaper escapes the three characters SQLite's URI parser reads in a
// "file:" path: '%' (a %HH escape), '?' (the query) and '#' (a fragment).
var pathEscaper = strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23")

// FileURI is the "file:" URI that names path literally, for a DSN to append
// its "?" query to. modernc opens every "file:" DSN with SQLITE_OPEN_URI, so
// SQLite parses the path as a URI: pasted in raw, a path through
// "/srv/Moombox #2/" opened whatever came before the '#', one with a '?' was
// cut there and lost the DSN's own query (mode=ro and busy_timeout among
// it), and "%41" was decoded to "A". Only those three characters are
// escaped, so a path without them — relative, with spaces, or a Windows one
// with a drive letter and backslashes — reads exactly as before. A path that
// starts with "//" is given an empty authority in front of it, or SQLite
// would read its first segment as a host name and refuse it.
func FileURI(path string) string {
	p := pathEscaper.Replace(path)
	if strings.HasPrefix(p, "//") {
		p = "//" + p
	}
	return "file:" + p
}
