package cipher

import "regexp"

// mainVariantPath is the player JS path below /s/player/<id>/ of yt-dlp's
// 'main' variant, _PLAYER_JS_VARIANT_MAP['main'] in
// extractor/youtube/_video.py. Upstream's default variant became 'main' with
// df0553153 (#14079; b7b7910d9, #14430, only named the constant
// _DEFAULT_PLAYER_JS_VARIANT), was 'tv' from 1a895c18a (#15818) and 'main'
// again from 04d6974f5 (#16269), which is what b77c9304e runs. fa9a87158
// (#18124) made it 'tv' once more, because the ejs release yt-dlp ships
// (0.8.0) throws on the main variant of the players from 05a4ffc0 on; the ejs
// vendored here since 2026-11-26 (c47fb1b, with 40b7a7d's prototype helpers)
// solves it, so Moombox stays on main (owner ruling RV-11). A page may itself
// run another variant, one of the `tcl` ones, whose STS and answers differ:
// youtube.mainVariantSTS. The main variant is the shape every Moombox player
// fetch takes but one (tvVariantPath) — the sidecar's first
// (playerURLForID), and since the watch page's URL is rewritten to it
// (CanonicalPlayerURL), STS's and the goja fallback's too.
const mainVariantPath = "player_ias.vflset/en_US/base.js"

// tvVariantPath is yt-dlp's 'tv' variant of the same player
// (_PLAYER_JS_VARIANT_MAP['tv']): the body the sidecar solver has ejs solve
// with when ejs cannot solve with the main one (sidecarSolver.solve, owner
// ruling RV-11). The regular variants of one player id state one STS and give
// one answer to a challenge, so a solver can use either; what differs is the
// code ejs has to parse, and each ejs breakage so far took one of the two and
// left the other. The path has no locale. Only fetchTVPlayerJS fetches it,
// and never through PlayerCache, whose entries are keyed by player id alone.
const tvVariantPath = "tv-player-ias.vflset/tv-player-ias.js"

// playerVariantURL is the URL of one variant of a player on YouTube's host.
func playerVariantURL(playerID, variantPath string) string {
	return "https://www.youtube.com/s/player/" + playerID + "/" + variantPath
}

// playerPathIDRe is yt-dlp's _extract_player_info, `/s/player/(?P<id>[a-fA-F0-9]{8,})/`:
// the player id, read only out of a /s/player/<hex>/ path segment. It is
// stricter than extractPlayerIDFromURL's split on purpose: a URL this cannot
// read is not rewritten at all, rather than rewritten around an id that may
// not be one.
var playerPathIDRe = regexp.MustCompile(`/s/player/([a-fA-F0-9]{8,})/`)

// PlayerIDOf is yt-dlp's _extract_player_info: the id of the player a URL
// names, read only out of a /s/player/<hex>/ segment, or "" when it names
// none. Unlike PlayerIDFromURL it has no fallback and logs nothing, so a
// caller can ask whether a URL names a player at all.
func PlayerIDOf(playerURL string) string {
	m := playerPathIDRe.FindStringSubmatch(playerURL)
	if m == nil {
		return ""
	}
	return m[1]
}

// PlayerURLForID is the main-variant player JS URL for a player id — the URL
// yt-dlp's _construct_player_url(player_id=...) builds, and the one a
// fallback source that names only the id (the iframe API) is turned into.
func PlayerURLForID(playerID string) string {
	return playerURLForID(playerID)
}

// CanonicalPlayerURL rewrites a player JS URL to the main variant of the same
// player, keeping its scheme and host: .../s/player/<id>/<anything> becomes
// .../s/player/<id>/player_ias.vflset/en_US/base.js. A URL with no
// /s/player/<id>/ segment comes back unchanged.
//
// yt-dlp rewrites every player URL it reads off a page to its default
// variant the same way (_construct_player_url, b7b7910d9 / #14430; 'main'
// since 04d6974f5 / #16269, unless the player_js_variant extractor-arg asks
// otherwise): es6, tce, tv and the rest become player_ias. Moombox did not:
// the page's variant was fetched as given by STS and the goja fallback, while
// the sidecar fetched the main one — and the disk cache keys both by player
// id alone (CacheKey). The two therefore overwrote each other's ~3 MB file in
// turn, each body-length change read as a rotation that dropped the
// preprocessed copy and the compiled solver, and a main fetch replayed the
// other variant's If-Modified-Since / ETag, so a 304 handed the sidecar the
// other variant's body (audit G50). One variant means one body under the one
// key.
//
// The locale goes BEYOND upstream: yt-dlp reads player_ias.vflset/de_DE as
// the main variant already (its variant match takes any locale in place of
// en_US, _get_player_id_variant_and_path) and fetches it as the page gave
// it. Here it becomes en_US too, because the id-keyed entry must hold the
// body the sidecar fetches, and playerURLForID asks for en_US.
//
// The host is kept, not forced to www.youtube.com as upstream's urljoin
// does, because every real player URL is already on it and the test servers
// that stand in for it are not.
func CanonicalPlayerURL(playerURL string) string {
	loc := playerPathIDRe.FindStringIndex(playerURL)
	if loc == nil {
		return playerURL
	}
	return playerURL[:loc[1]] + mainVariantPath
}
