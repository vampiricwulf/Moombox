package cipher

import "strings"

// HasNChallenge reports whether rawURL carries an n challenge in either form
// a googlevideo URL carries one, the forms RoutedDecryptNInURL solves: the
// path's /n/<value>/ (at least 10 characters, that function's floor) and the
// query's n=. A URL that carries none needs no player to be fetched, as
// yt-dlp's REQUIRE_JS_PLAYER False clients' URLs need none — visionos's
// direct format URLs and its live HLS master (_base.py:295-310).
//
// It is how the worker's challenge-free fallback tells a format, or a
// manifest, it can still fetch while no challenge can be solved (audit G10)
// from one it cannot: yt-dlp skips a format whose n has no answer and a
// manifest `if solved_n or not n_challenge` fails (_video.py:3570-3580,
// 3692-3713), so the next client's copy of the stream — the one with no
// challenge — is the one it keeps.
func HasNChallenge(rawURL string) bool {
	const pathPrefix = "/n/"
	if idx := strings.Index(rawURL, pathPrefix); idx >= 0 {
		rest := rawURL[idx+len(pathPrefix):]
		if value, _, ok := strings.Cut(rest, "/"); ok && len(value) >= 10 {
			return true
		}
	}
	rawQuery := ""
	if q := strings.Index(rawURL, "?"); q >= 0 {
		rawQuery = rawURL[q+1:]
	}
	raw, _ := RawQueryParam(rawQuery, "n")
	return raw != ""
}
