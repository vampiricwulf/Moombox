package youtube

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/constants"
)

// The player JS URL is what every signature and n solve, and the STS every
// player call carries, are keyed on. The watch page used to be its only
// source (jsURLRegex), so one failed or player-less page left the whole
// extraction without one: STS 0, the DASH and HLS strategies skipping n, and
// every VOD and manifest-free resolve failing (audit G2). yt-dlp reads it off
// whichever ytcfg names one first — the watch page's, then each client's own
// config page (_video.py:3066-3068) — and only when none does fetches the
// iframe API for it (_download_player_url, b6de707d1 / #860;
// _video.py:2157-2171, 3074-3076).
//
// Moombox fetches no per-client config pages, so the sources it has are, in
// resolvePlayerURL's order: the watch page; a player URL a page named
// recently (the homepage Service.Init and the account-session read fetch
// anyway, an embed page, an earlier watch page or iframe API read — within
// playerURLFreshFor); the iframe API; and the last player any of them named,
// whatever its age.

// iframeAPIPlayerIDRe is _download_player_url's pattern,
// `player\\?/([0-9a-fA-F]{8})\\?/` (_video.py:2169): the iframe API script
// names its player as player/<id>/, the slashes JS-escaped (player\/<id>\/)
// on the script as served.
var iframeAPIPlayerIDRe = regexp.MustCompile(`player\\?/([0-9a-fA-F]{8})\\?/`)

// iframeAPIMaxBytes bounds the iframe API read. The script is a few
// kilobytes; a megabyte is room for it to grow, not a page.
const iframeAPIMaxBytes = 1 << 20

// playerURLFreshFor is how long a player URL some page named stands in for
// a watch page that names none, before the iframe API is asked instead. The
// homepage behind it is read at startup and on an account change only, and a
// player rotates every day or two (player_cache.go: rotations observed at
// ~36 h), so past six hours a fresh read is worth its one small request. An
// older URL is not wrong — YouTube serves a superseded player, and answers
// its STS, for a good while after a rotation — so it still stands behind a
// failed iframe API read, as the last good one.
const playerURLFreshFor = 6 * time.Hour

// fetchIframePlayerURL is fetchPlayerURLFromIframeAPI behind a package var.
// The unit suite's TestMain points it at a stub that answers nothing, so a
// cascade test whose stubbed watch page names no player reaches no network;
// the tests of the fetch itself call the function. Production never writes
// it.
var fetchIframePlayerURL = fetchPlayerURLFromIframeAPI

// fetchPlayerURLFromIframeAPI is yt-dlp's _download_player_url: GET
// /iframe_api (through fetchYouTubePage's retries, as upstream's
// _download_webpage_with_retries), read the player id off it, and build that
// player's main-variant URL (cipher.PlayerURLForID, upstream's
// _construct_player_url(player_id=...)).
//
// cookieHeader is what the request carries: resolvePlayerURL hands it the
// consent and language pair (Auth.ConsentCookieHeader). Upstream's request
// goes through the jar _real_initialize gave SOCS=CAI and the English PREF
// (_base.py:675-704), so the script is read behind the same consent as every
// page; a consent wall in front of it would otherwise leave the extraction
// with no player at all, and its error would advise a cookie file this read
// never carried. The pair, not the session: the read was anonymous before
// and neither cookie is a credential.
func fetchPlayerURLFromIframeAPI(ctx context.Context, cookieHeader string) (string, error) {
	headers := map[string]string{"User-Agent": constants.UserAgents.Web}
	if cookieHeader != "" {
		headers["Cookie"] = cookieHeader
	}
	body, err := fetchYouTubePage(ctx, "iframe API", constants.YouTubeURLs.Base+"/iframe_api", 15*time.Second,
		headers, iframeAPIMaxBytes)
	if err != nil {
		return "", err
	}
	m := iframeAPIPlayerIDRe.FindSubmatch(body)
	if m == nil {
		return "", errors.New("iframe API names no player")
	}
	return cipher.PlayerURLForID(string(m[1])), nil
}

// pagePlayerURL reads the player URL off a page — the homepage, an embed
// page, and the watch page through watchPagePlayerURL — as yt-dlp's
// _extract_player_url reads one off any client's ytcfg: the first jsUrl /
// PLAYER_JS_URL / scriptSrc that names a player (cipher.PlayerIDOf), on the
// main variant (normalizePlayerJSURL). "" when none does. A URL from any of
// these is remembered and handed to extractions that never saw the page, so
// one that names no player — a leading desktop_polymer scriptSrc — must not
// get that far; nor can it serve the page's own extraction, whose solver
// would fetch it as a player. Upstream never takes one either: it reads only
// PLAYER_JS_URL and jsUrl, and _extract_player_info raises on a URL with no
// /s/player/<id>/ (_video.py:2149-2155, 2194-2198).
func pagePlayerURL(page string) string {
	for _, m := range jsURLRegex.FindAllStringSubmatch(page, -1) {
		if u := normalizePlayerJSURL(m[1]); cipher.PlayerIDOf(u) != "" {
			return u
		}
	}
	return ""
}

// watchPagePlayerURL is pagePlayerURL over the watch page's bytes. It walks
// the matches one at a time rather than collecting them, and never turns the
// ~5 MB page into a string: on a page whose first match names the player it
// costs what the first-match read it replaced did. That read took the first
// match whatever it named, so a page
// leading with a non-player scriptSrc ran its extraction on that script and
// had it remembered for six hours as the fallback (resolvePlayerURL), over
// the homepage's real player.
func watchPagePlayerURL(page []byte) string {
	for rest := page; ; {
		loc := jsURLRegex.FindSubmatchIndex(rest)
		if loc == nil {
			return ""
		}
		if u := normalizePlayerJSURL(string(rest[loc[2]:loc[3]])); cipher.PlayerIDOf(u) != "" {
			return u
		}
		rest = rest[loc[1]:]
	}
}

// rememberPlayerURL records u as the player the latest source named — source
// says which, for the line that adopts it. A no-op for "".
func (p *PlayerAPI) rememberPlayerURL(u, source string) {
	if u == "" {
		return
	}
	p.playerURLMu.Lock()
	p.playerURL, p.playerURLAt, p.playerURLSource = u, time.Now(), source
	p.playerURLMu.Unlock()
}

// resolvePlayerURL is the player URL an extraction runs on: fromPage, the
// watch page's own (remembered for later extractions), or — when the page
// named none or could not be had — the fallbacks the file comment lists, in
// order: a URL remembered within playerURLFreshFor, the iframe API (its
// answer remembered too), the remembered URL whatever its age. "" only when
// no source has ever named a player and the iframe API cannot be read.
//
// Called before extractSTS, so STS, the watch page's own parse and every
// client call of the extraction carry the URL it finds, as upstream's
// player_url does from the first client on.
func (p *PlayerAPI) resolvePlayerURL(ctx context.Context, videoID, fromPage string) string {
	if fromPage != "" {
		p.rememberPlayerURL(fromPage, "watch page")
		return fromPage
	}
	p.playerURLMu.Lock()
	known, source, at := p.playerURL, p.playerURLSource, p.playerURLAt
	p.playerURLMu.Unlock()
	if known != "" && time.Since(at) < playerURLFreshFor {
		p.logger.Debug("[PlayerApi] watch page named no player; using the one a recent page named",
			"videoID", videoID, "source", source, "age", time.Since(at).Round(time.Second), "playerURL", known)
		return known
	}
	fresh, err := fetchIframePlayerURL(ctx, p.auth.ConsentCookieHeader())
	if err == nil {
		p.rememberPlayerURL(fresh, "iframe API")
		p.logger.Info("[PlayerApi] watch page named no player; using the iframe API's",
			"videoID", videoID, "playerURL", fresh)
		return fresh
	}
	logMiss := p.logger.Warn
	if ctx.Err() != nil {
		// The job's cancel or the caller's deadline, not a missing player.
		logMiss = p.logger.Debug
	}
	if known != "" {
		logMiss("[PlayerApi] watch page named no player and the iframe API could not be read; using the last good one",
			"videoID", videoID, "source", source, "age", time.Since(at).Round(time.Second), "playerURL", known, "error", err.Error())
		return known
	}
	logMiss("[PlayerApi] no player URL: the watch page named none and the iframe API could not be read — signature and n solving are off for this extraction",
		"videoID", videoID, "error", err.Error())
	return ""
}
