package utils

import (
	"bytes"
	"encoding/json"
	"regexp"
)

// ScanBalancedJSONObject returns the complete `{...}` literal starting at
// s[0], tracking JS string state so braces inside quoted payloads never affect
// the depth count. Returns ok=false when the literal never closes, when s is
// empty, or when s does not start on a brace. The result is a sub-slice of s:
// no copy, so a caller holding it holds the page's backing array.
//
// All three quote kinds are tracked because the literal is embedded in a
// <script> body: the surrounding text is JavaScript, and a single-quoted or
// template string there can carry a brace the JSON grammar alone would not
// admit.
//
// []byte rather than string because every caller holds the raw response body:
// the watch-page extractors stopped copying the ~1-5 MB page into a string for
// the sake of the handful of extractors that only read it.
//
// Moved here from internal/youtube (where it was scanBalancedObject) at the
// 2026-09-15 chain close, so internal/chat's ytInitialData extractor can share
// it: chat must not import youtube (nor youtube chat), and both already import
// this package.
func ScanBalancedJSONObject(s []byte) ([]byte, bool) {
	if len(s) == 0 || s[0] != '{' {
		return nil, false
	}
	depth := 0
	var quote byte
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == quote:
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			quote = c
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[:i+1], true
			}
		}
	}
	return nil, false
}

// FindJSONObjectCandidate finds the JSON object literal a page assigns, given
// assignment-PREFIX anchors that each end ON the opening brace. Per anchor in
// order, and per occurrence of that anchor in page order, it brace-scans from
// that `{` and offers the literal to accept; the first accepted literal wins
// and is returned as a sub-slice of page.
//
// Every occurrence is tried, not just the first. First-occurrence was never a
// safety property: on a real watch page the `name="description"` and
// `og:description` meta tags are emitted thousands of bytes BEFORE the real
// assignment, so page-authored text genuinely does come first. What limits
// forgery is that such text cannot spell a valid non-empty JSON object — HTML
// attribute escaping turns `"` into `&quot;`, and inside a JSON string `\"`
// breaks the scan — so a forged candidate can only fail the scan or fail
// accept. Stopping at the first match turned that harmless inability into a
// DENIAL of the real document; skipping the failed candidate and searching on
// turns it back into nothing at all.
//
// Rejected `{` offsets are remembered, because real anchor lists overlap by
// construction: a bare `X\s*=\s*\{` anchor matches inside every
// `var X\s*=\s*\{` occurrence an earlier anchor already tried, so without the
// set a page where every candidate fails pays for each one twice — on a
// multi-megabyte payload. The map is allocated lazily, so the ordinary page
// (first candidate accepted) allocates nothing here.
//
// Each anchor must match at least the opening brace (a zero-width match is a
// contract violation) and `accept` is required (nil panics).
func FindJSONObjectCandidate(page []byte, anchors []*regexp.Regexp, accept func(obj []byte) bool) ([]byte, bool) {
	var rejected map[int]struct{}
	for _, re := range anchors {
		for start := 0; start < len(page); {
			loc := re.FindIndex(page[start:])
			if loc == nil {
				break
			}
			// The match ends ON the opening brace, so the literal starts one
			// byte back from the match end.
			brace := start + loc[1] - 1
			// Resume one byte past THIS match's start, so a rejected candidate
			// cannot be re-found and the scan above is free to run off the end
			// of a forged literal.
			start += loc[0] + 1
			if _, seen := rejected[brace]; seen {
				continue
			}
			if obj, ok := ScanBalancedJSONObject(page[brace:]); ok && accept(obj) {
				return obj, true
			}
			if rejected == nil {
				rejected = make(map[int]struct{})
			}
			rejected[brace] = struct{}{}
		}
	}
	return nil, false
}

// IsNonEmptyJSONObject is the accept predicate the two raw-literal consumers
// pass to FindJSONObjectCandidate — there is no default: the literal must be
// real JSON and must carry something between its braces.
//
// json.Valid is a scan, not a decode — it allocates nothing and does not build
// the map or envelope the caller is about to build anyway — so a caller that
// unmarshals afterwards pays one extra pass, not one extra decode. The
// emptiness half is load-bearing on its own: `{}` scans and decodes perfectly
// well, so without it a forged empty object would win the search exactly as a
// forged non-object cannot.
func IsNonEmptyJSONObject(obj []byte) bool {
	if len(obj) < 2 {
		return false
	}
	// obj always comes from ScanBalancedJSONObject, so it is at least `{}`.
	return len(bytes.TrimSpace(obj[1:len(obj)-1])) > 0 && json.Valid(obj)
}
