package redact

import (
	"net/url"
	"regexp"
	"strings"
)

// A googlevideo media URL — a format's /videoplayback URL, a live DASH or HLS
// manifest under /api/manifest/, the HLS variant playlist and the segments it
// lists — is signed for the one client that asked for it, and says so in its
// parameters: the client's public IP address (ip), the signatures that
// authorise the fetch (sig, lsig), the GVS PO token (pot), the deciphered
// throttling parameter (n), the event and playback-session ids (ei, cpn), and
// a dozen more values the server stamped for that session. They sit in the
// query (…/videoplayback?…&ip=…&sig=…) or, on a live manifest and an HLS
// segment, as /key/value pairs in the path (…/ip/<addr>/…/sig/<sig>). A
// transport failure is a *url.Error that quotes the whole URL, and a job's
// error posts that text to the Job Failed embed — the operator's home address
// to everyone who reads the webhook channel.
//
// A Twitch live HLS URL carries its session the other way round: as the path,
// not as named parameters. The video-weaver variant playlist an usher master
// lists is …/v1/playlist/<session>.m3u8, and every segment that playlist names
// is …/v1/segment/<session>.ts, <session> an opaque blob the edge minted for
// this viewer's playback session — what the playback token granted, to whom,
// from where. The engine polls the one and fetches the other, so a transport
// failure quotes it just the same.

// mediaKeepKeys are the parameters whose values MediaURL keeps: they say which
// format a fetch was for, which part of it, and whether its URL had run out,
// and none of them names the client or authorises a fetch. Every other value
// is cut — a parameter googlevideo adds tomorrow is cut too.
var mediaKeepKeys = map[string]bool{
	"itag":     true, // the format
	"mime":     true,
	"clen":     true, // the format's size
	"dur":      true,
	"source":   true, // youtube, yt_live_broadcast
	"sq":       true, // the segment
	"range":    true, // the chunk
	"expire":   true, // whether the URL had run out
	"playlist": true, // index.m3u8, in an HLS URL's path form
	"file":     true, // seg.ts, likewise
}

// MediaURL cuts a googlevideo media URL's credentials out of rawURL: every
// parameter value except mediaKeepKeys' becomes <redacted>, in the query and
// in the path's /key/value pairs alike. The scheme, the host, the endpoint,
// every key and their order stay, so the text still says which fetch it was:
//
//	https://rr3---sn-abc.googlevideo.com/videoplayback?expire=1700000000&ei=<redacted>&ip=<redacted>&id=<redacted>&itag=140&…&sig=<redacted>&pot=<redacted>
//
// A media URL is one whose path is /videoplayback/… or /api/manifest/<kind>/…,
// whatever its host — a test server's included — or any URL on a
// googlevideo.com host. Its fragment, which googlevideo never uses, is cut
// whole.
//
// A Twitch media URL — one whose path is /v1/playlist/… or /v1/segment/…,
// whatever its host, or any URL on a ttvnw.net or live-video.net host — keeps
// its scheme, its host (which edge served it) and the endpoint's words, and
// loses the rest of its path (twitchPath): the session its file name spells
// goes, the file's extension stays, so the text still says a playlist or a
// segment, and from which edge:
//
//	https://video-edge-c2a0d4.fra05.abs.hls.ttvnw.net/v1/segment/<redacted>.ts
//
// Every query value of one is cut too, and its fragment.
//
// Every URL, a media URL or not, has its GVS PO token cut (PoTokenURL). The
// string is spliced rather than re-serialised through url.URL, so a URL
// net/url cannot parse, or one a log line truncated, is still handled, and a
// second call changes nothing.
func MediaURL(rawURL string) string {
	s := PoTokenURL(rawURL)
	prefix, rest := "", s
	host := ""
	if i := strings.Index(s, "://"); i >= 0 {
		end := len(s)
		if j := strings.IndexAny(s[i+3:], "/?#"); j >= 0 {
			end = i + 3 + j
		}
		prefix, rest = s[:end], s[end:]
		host = s[i+3 : end]
		if at := strings.LastIndexByte(host, '@'); at >= 0 {
			host = host[at+1:]
		}
		if c := strings.LastIndexByte(host, ':'); c >= 0 && !strings.Contains(host[c:], "]") {
			host = host[:c]
		}
	}
	pathEnd := strings.IndexAny(rest, "?#")
	if pathEnd < 0 {
		pathEnd = len(rest)
	}
	path, tail := rest[:pathEnd], rest[pathEnd:]

	segs := strings.Split(path, "/")
	if start, ok := mediaParamsStart(segs, isGooglevideoHost(host)); ok {
		for k := start; k+1 < len(segs); k += 2 {
			if !mediaKeepKeys[segs[k]] && segs[k+1] != "" {
				segs[k+1] = Marker
			}
		}
		return prefix + strings.Join(segs, "/") + cutQueryValues(tail, mediaKeepKeys)
	}
	if isTwitchMedia(segs, host) {
		twitchPath(segs)
		return prefix + strings.Join(segs, "/") + cutQueryValues(tail, nil)
	}
	return s
}

// cutQueryValues returns tail — a URL's "?query#fragment", either part
// optional — with every query value but keep's replaced by Marker, and its
// fragment cut whole.
func cutQueryValues(tail string, keep map[string]bool) string {
	out := ""
	query, frag, hasFrag := strings.Cut(tail, "#")
	if strings.HasPrefix(query, "?") {
		parts := strings.Split(query[1:], "&")
		for i, p := range parts {
			if key, val, hasVal := strings.Cut(p, "="); hasVal && val != "" && !keep[key] {
				parts[i] = key + "=" + Marker
			}
		}
		out += "?" + strings.Join(parts, "&")
	}
	if hasFrag {
		if frag != "" {
			frag = Marker
		}
		out += "#" + frag
	}
	return out
}

// mediaParamsStart reports whether segs — a path split on '/', segs[0] the
// empty string before its leading slash — is a media endpoint's, and the index
// of its first path parameter's key: after /videoplayback, after
// /api/manifest/<kind>, or, on a googlevideo host, after the first segment.
func mediaParamsStart(segs []string, googlevideo bool) (int, bool) {
	switch {
	case len(segs) > 1 && segs[0] == "" && segs[1] == "videoplayback":
		return 2, true
	case len(segs) > 3 && segs[0] == "" && segs[1] == "api" && segs[2] == "manifest":
		return 4, true
	case googlevideo:
		return 2, true
	}
	return 0, false
}

// isGooglevideoHost reports whether host is googlevideo.com or under it.
func isGooglevideoHost(host string) bool {
	return hostUnder(host, "googlevideo.com")
}

// hostUnder reports whether host is domain or a name under it, in any case.
func hostUnder(host, domain string) bool {
	host = strings.ToLower(host)
	return host == domain || strings.HasSuffix(host, "."+domain)
}

// twitchKeepSegs are the path segments a Twitch media URL keeps: the words of
// its endpoints — a weaver's /v1/playlist/, an edge's /v1/segment/, an usher's
// /api/channel/hls/ and /vod/ — none of which names a session.
var twitchKeepSegs = map[string]bool{
	"v1":       true,
	"playlist": true,
	"segment":  true,
	"api":      true,
	"channel":  true,
	"hls":      true,
	"vod":      true,
}

// isTwitchMedia reports whether a URL — its path split on '/' as segs, and its
// host — is a Twitch media URL: a /v1/playlist/ or /v1/segment/ path on any
// host, or any path on a ttvnw.net or live-video.net host.
func isTwitchMedia(segs []string, host string) bool {
	if len(segs) > 2 && segs[0] == "" && segs[1] == "v1" && (segs[2] == "playlist" || segs[2] == "segment") {
		return true
	}
	return hostUnder(host, "ttvnw.net") || hostUnder(host, "live-video.net")
}

// twitchPath cuts a Twitch media URL's session out of its path, segs in
// place: every segment but twitchKeepSegs' becomes Marker. The last one — the
// file — keeps its extension, and its whole name when that is a number (a
// sequence number names no session); any other name goes, whatever it spells.
func twitchPath(segs []string) {
	for i, seg := range segs {
		if seg == "" || twitchKeepSegs[seg] {
			continue
		}
		if i < len(segs)-1 {
			segs[i] = Marker
			continue
		}
		stem, ext := seg, ""
		if dot := strings.LastIndexByte(seg, '.'); dot > 0 && isFileExt(seg[dot+1:]) {
			stem, ext = seg[:dot], seg[dot:]
		}
		if !isDigits(stem) {
			segs[i] = Marker + ext
		}
	}
}

// isFileExt reports whether ext — what follows a name's last '.' — reads as a
// media file's extension (ts, m3u8, mp4, m4s, aac): one to five ASCII letters
// and digits. Anything else is the session's own text and goes with it.
func isFileExt(ext string) bool {
	if ext == "" || len(ext) > 5 {
		return false
	}
	for _, c := range ext {
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9') {
			return false
		}
	}
	return true
}

// isDigits reports whether s is one or more ASCII digits.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// urlInTextRe matches a URL inside flattened text: a scheme, "://", and
// everything up to whitespace or the closing quote %q put around it. It stops
// at nothing else — not '<', which the <redacted> markers carry, nor '&' or
// '/' — so a parameter after an already redacted one is still inside the
// match.
var urlInTextRe = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s"]+`)

// MediaText applies MediaURL to every URL in s, then cuts any GVS PO token
// left outside one (PoTokenText). It is the rule for text that has already
// been flattened — an error's message, a job's error column — where nothing
// says which part of it is a URL. Text with no URL and no token is returned
// as is.
func MediaText(s string) string {
	if strings.Contains(s, "://") {
		s = urlInTextRe.ReplaceAllStringFunc(s, MediaURL)
	}
	return PoTokenText(s)
}

// MediaError keeps a media URL's credentials out of an error's text. A
// transport failure from http.Client.Do — and a request net/url refused to
// build — is a *url.Error whose Error() quotes the whole request URL, and that
// string reaches the "job error" log line, the job's stored error and the Job
// Failed embed.
//
// Contract: every *url.Error in err's tree whose URL MediaURL changes has its
// URL rewritten IN PLACE (Op and Err are untouched, so errors.Is / errors.As
// on the cause still hold). When err is such a *url.Error itself it is
// returned as is; when one sits under a wrapper whose message was precomputed
// (fmt.Errorf), the result is a thin wrapper carrying that message through
// MediaText, whose Unwrap is err. Every other error — nil, no *url.Error,
// nothing to cut — is returned unchanged, and a second call is a no-op.
func MediaError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if !rewriteURLErrors(err, MediaURL) {
		return err
	}
	if _, ok := err.(*url.Error); ok {
		return err
	}
	return &redactedError{msg: MediaText(msg), err: err}
}

// rewriteURLErrors applies rule to the URL of every *url.Error in err's tree
// and reports whether any of them changed.
func rewriteURLErrors(err error, rule func(string) string) bool {
	changed := false
	var walk func(error)
	walk = func(e error) {
		for e != nil {
			if ue, ok := e.(*url.Error); ok {
				if r := rule(ue.URL); r != ue.URL {
					ue.URL = r
					changed = true
				}
			}
			switch u := e.(type) {
			case interface{ Unwrap() []error }:
				for _, inner := range u.Unwrap() {
					walk(inner)
				}
				return
			case interface{ Unwrap() error }:
				e = u.Unwrap()
			default:
				return
			}
		}
	}
	walk(err)
	return changed
}

// redactedError carries a wrapper's message with its secrets cut out; Unwrap
// keeps the original chain for errors.Is / errors.As.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }
