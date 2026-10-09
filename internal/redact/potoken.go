package redact

import (
	"net/url"
	"regexp"
	"strings"
)

// A googlevideo URL carries the GVS PO token in one of two places: as the pot
// query value engine.applyPoTokenQuery appends to every segment, chunk and
// probe URL, and as a /pot/<token> path segment, which is how the live DASH
// and HLS strategies attach it to a manifest URL and to the HLS variant
// playlist the downloader then polls. A rule that knows only one of the two
// leaves the other in every transport error that quotes the URL.
const (
	potQueryKey     = "pot"
	potPathSegment  = "pot"
	redactedPotPair = potQueryKey + "=" + Marker
)

// potQueryRe and potPathRe match the token in free text: a wrapper's
// precomputed message, or a URL net/url cannot parse. The query value runs to
// the next '&', the closing quote %q put around the URL, or whitespace; the
// path segment to the next '/', the query, the fragment, that quote or
// whitespace.
var (
	potQueryRe = regexp.MustCompile(`pot=[^&#"\s]+`)
	potPathRe  = regexp.MustCompile(`/pot/[^/?#&"\s]+`)
)

// PoTokenText cuts every GVS PO token out of s, in both forms. It is the
// token's rule for text that has already been flattened, where nothing says
// which part of it is a URL; MediaText applies it, after MediaURL, to an
// error's message and a job's error column. A string with no token is
// returned as is.
func PoTokenText(s string) string {
	if !strings.Contains(s, "pot") {
		return s
	}
	s = potQueryRe.ReplaceAllString(s, redactedPotPair)
	return potPathRe.ReplaceAllString(s, "/"+potPathSegment+"/"+Marker)
}

// PoTokenURL rewrites every GVS PO token rawURL carries — each pot query
// value and the segment after each /pot/ in the path — to <redacted>, leaving
// every other byte of the URL as it was: the scheme, the host, the rest of
// the path, every other parameter and their order. A URL net/url cannot parse
// falls back to PoTokenText.
func PoTokenURL(rawURL string) string {
	if _, err := url.Parse(rawURL); err != nil {
		return PoTokenText(rawURL)
	}
	// Splice the raw string rather than re-serialise through url.URL, so
	// nothing but a token can change.
	end := strings.IndexAny(rawURL, "?#")
	if end < 0 {
		end = len(rawURL)
	}
	head := redactPotPath(rawURL[:end])
	tail := rawURL[end:]
	if !strings.HasPrefix(tail, "?") {
		return head + tail
	}
	query, frag, hasFrag := strings.Cut(tail[1:], "#")
	parts := strings.Split(query, "&")
	for i, p := range parts {
		if key, _, _ := strings.Cut(p, "="); key == potQueryKey {
			parts[i] = redactedPotPair
		}
	}
	out := head + "?" + strings.Join(parts, "&")
	if hasFrag {
		out += "#" + frag
	}
	return out
}

// redactPotPath rewrites the segment after every "pot" segment of the path in
// head, a URL cut before its query and fragment. The authority is skipped, so
// a host that happens to be named pot is not read as the marker.
func redactPotPath(head string) string {
	prefix, path := "", head
	if i := strings.Index(head, "://"); i >= 0 {
		j := strings.IndexByte(head[i+3:], '/')
		if j < 0 {
			return head
		}
		prefix, path = head[:i+3+j], head[i+3+j:]
	}
	segs := strings.Split(path, "/")
	for k := 0; k+1 < len(segs); k++ {
		if segs[k] == potPathSegment && segs[k+1] != "" {
			segs[k+1] = Marker
			k++
		}
	}
	return prefix + strings.Join(segs, "/")
}
