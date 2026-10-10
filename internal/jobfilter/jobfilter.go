// Package jobfilter is the Go twin of web/public/modules/filter-parser.js
// and filter-engine.js — the dashboard's filter language, used by the TUI's
// / box. Changes to the language land in both places; the tests here mirror
// the node suites value for value.
package jobfilter

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// Kind is a token's namespace.
type Kind string

const (
	KindText     Kind = "text"
	KindStatus   Kind = "status"
	KindChannel  Kind = "channel"
	KindPlatform Kind = "platform"
	KindOr       Kind = "or"
)

// namespaces mirrors filter-parser.js's NAMESPACES set — the only prefixes
// recognized before the colon; everything else falls back to KindText.
var namespaces = map[string]Kind{
	"status":   KindStatus,
	"channel":  KindChannel,
	"platform": KindPlatform,
}

// Token is one parsed unit: a term (Kind text/status/channel/platform with
// Value and Negate) or an OR group (Kind "or" with Terms). Value is kept as
// typed (for Serialize); lower is its lower-cased form, computed once.
type Token struct {
	Kind   Kind
	Value  string
	Negate bool
	Terms  []Token // Kind == KindOr only
	lower  string
}

// BucketStatuses is the Web's STATUS_FILTER_MAP. It is keyed by the canonical
// bucket names only — every lookup goes through StatusBucket, which folds the
// "errors" alias into "issues" first.
var BucketStatuses = map[string][]database.JobStatus{
	"active":   {database.StatusDownloading, database.StatusLive, database.StatusUpcoming, database.StatusMuxing, database.StatusQueued},
	"issues":   {database.StatusError, database.StatusCancelled, database.StatusCookies},
	"finished": {database.StatusFinished},
}

// StatusBucket names the bucket a status: value selects ("active", "issues",
// "finished") or "" for a raw status name; "errors" is an alias of "issues".
func StatusBucket(value string) string {
	switch strings.ToLower(value) {
	case "active":
		return "active"
	case "issues":
		return "issues"
	case "errors":
		// The bucket's name before Cancelled joined it; keep hand-typed
		// queries working.
		return "issues"
	case "finished":
		return "finished"
	default:
		return ""
	}
}

// Term builds one term token programmatically, for callers that compose a
// query instead of typing it — the TUI's F cycle inserts its status token
// this way. Value's lower-cased form is private, so a Token{…} literal built
// outside this package would silently fail to match; go through Term.
//
// Not for KindOr: OR groups are only built by Parse; a KindOr token from Term
// has no Terms and matches nothing.
func Term(kind Kind, value string, negate bool) Token {
	return Token{Kind: kind, Value: value, Negate: negate, lower: strings.ToLower(value)}
}

// stripQuotePair strips one surrounding matching quote pair, if present —
// filter-parser.js's stripQuotePair.
func stripQuotePair(value string) string {
	if len(value) >= 2 {
		if (strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`)) ||
			(strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'")) {
			return value[1 : len(value)-1]
		}
	}
	return value
}

// tokenize walks the query rune by rune, splitting on unquoted spaces —
// filter-parser.js:59-85. A quote (" or ') opens quoting only at token
// start, right after a leading "-", or right after a namespace colon; the
// matching closing quote ends quoting. Elsewhere a quote character is
// literal, so an apostrophe in mori's does not swallow the rest of the
// query.
func tokenize(query string) []string {
	var tokens []string
	var current strings.Builder
	var inQuote rune
	for _, ch := range query {
		switch {
		case inQuote != 0:
			current.WriteRune(ch)
			if ch == inQuote {
				inQuote = 0
			}
		case (ch == '"' || ch == '\'') && (current.Len() == 0 || current.String() == "-" || strings.HasSuffix(current.String(), ":")):
			inQuote = ch
			current.WriteRune(ch)
		case ch == ' ':
			if current.Len() > 0 {
				tokens = append(tokens, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(ch)
		}
	}
	if current.Len() > 0 {
		tokens = append(tokens, current.String())
	}
	return tokens
}

// parseTerm parses a single term (no pipes) into a Token — filter-parser.js
// parseTerm. e.g. "status:active", "-jelly", `channel:"shachi too"`.
func parseTerm(raw string) Token {
	negate := false
	s := raw
	if strings.HasPrefix(s, "-") {
		negate = true
		s = s[1:]
	}
	if colonIdx := strings.IndexByte(s, ':'); colonIdx > 0 {
		ns := strings.ToLower(s[:colonIdx])
		if kind, ok := namespaces[ns]; ok {
			value := stripQuotePair(s[colonIdx+1:])
			return Token{Kind: kind, Value: value, Negate: negate, lower: strings.ToLower(value)}
		}
	}
	// Bare quoted phrases ("jelly fin") arrive as one token WITH the quotes
	// — strip them here too, or the engine would substring-match the
	// literal quote characters and never find anything.
	value := stripQuotePair(s)
	return Token{Kind: KindText, Value: value, Negate: negate, lower: strings.ToLower(value)}
}

// isJSTrimSpace reports whether String.prototype.trim() strips r: ECMAScript's
// WhiteSpace and LineTerminator sets. Go's unicode.IsSpace differs from them
// in exactly two code points — it counts U+0085 (NEL), which JS does not,
// and not U+FEFF (the BOM), which JS does — so a BOM-prefixed query pasted
// from a file matched in the dashboard and nothing in the TUI.
func isJSTrimSpace(r rune) bool {
	return r == '\uFEFF' || (unicode.IsSpace(r) && r != '\u0085')
}

// TrimQuery trims a query the way Parse does (JavaScript's String.trim), for
// callers that keep the query text beside its tokens.
func TrimQuery(query string) string {
	return strings.TrimFunc(query, isJSTrimSpace)
}

// Parse tokenises a query the way web/public/modules/filter-parser.js does.
func Parse(query string) []Token {
	trimmed := TrimQuery(query)
	if trimmed == "" {
		return nil
	}
	rawTokens := tokenize(trimmed)
	tokens := make([]Token, 0, len(rawTokens))
	for _, raw := range rawTokens {
		// Check for pipe (OR) — but not inside quotes.
		if strings.Contains(raw, "|") && !strings.ContainsAny(raw, `"'`) {
			parts := nonEmptyParts(strings.Split(raw, "|"))
			if len(parts) > 1 {
				terms := make([]Token, len(parts))
				for i, p := range parts {
					terms[i] = parseTerm(p)
				}
				tokens = append(tokens, Token{Kind: KindOr, Terms: terms})
				continue
			}
		}
		tokens = append(tokens, parseTerm(raw))
	}
	return tokens
}

// nonEmptyParts filters out empty strings — JS's Array.filter(Boolean).
func nonEmptyParts(parts []string) []string {
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// serializeToken renders one token back to query text —
// filter-parser.js serializeToken.
func serializeToken(t Token) string {
	if t.Kind == KindOr {
		parts := make([]string, len(t.Terms))
		for i, term := range t.Terms {
			parts[i] = serializeToken(term)
		}
		return strings.Join(parts, "|")
	}
	prefix := ""
	if t.Negate {
		prefix = "-"
	}
	val := quoteValue(t.Value, t.Kind == KindText)
	if t.Kind == KindText {
		return prefix + val
	}
	return prefix + string(t.Kind) + ":" + val
}

// Serialize renders tokens back to query text (filter-parser.js serializeToken).
// quoteValue quotes a value so it parses back to itself — filter-parser.js
// quoteValue, which carries the full rule set. In short: a space or a pipe, a
// leading quote character, and for a text term a leading "-" or a filter-key
// prefix need quotes; the quote is `"` unless the value holds one, then `'`
// (a value holding both kinds cannot be quoted losslessly and keeps `"`).
func quoteValue(value string, isText bool) string {
	lower := strings.ToLower(value)
	needs := strings.ContainsAny(value, " |") ||
		strings.HasPrefix(value, `"`) || strings.HasPrefix(value, "'") ||
		(isText && (strings.HasPrefix(value, "-") ||
			strings.HasPrefix(lower, "status:") || strings.HasPrefix(lower, "channel:") || strings.HasPrefix(lower, "platform:")))
	if !needs {
		return value
	}
	q := `"`
	if strings.Contains(value, `"`) && !strings.Contains(value, "'") {
		q = "'"
	}
	return q + value + q
}

func Serialize(tokens []Token) string {
	parts := make([]string, len(tokens))
	for i, t := range tokens {
		parts[i] = serializeToken(t)
	}
	return strings.Join(parts, " ")
}

// matchTerm tests whether a single term matches a job — filter-engine.js
// matchTerm; both twins match a text term against title, channel and video ID.
func matchTerm(t Token, job *database.Job) bool {
	var result bool
	switch t.Kind {
	case KindText:
		result = containsFoldLower(job.Title, t.lower) ||
			containsFoldLower(job.ChannelName, t.lower) ||
			containsFoldLower(job.VideoID, t.lower)
	case KindStatus:
		if statuses, ok := BucketStatuses[StatusBucket(t.Value)]; ok {
			result = slices.Contains(statuses, job.Status)
		} else {
			result = equalFoldLower(string(job.Status), t.lower)
		}
	case KindChannel:
		result = equalFoldLower(job.ChannelName, t.lower)
	case KindPlatform:
		result = equalFoldLower(job.Platform, t.lower)
	default:
		result = true
	}
	if t.Negate {
		return !result
	}
	return result
}

// containsFoldLower reports whether sub occurs in the lower-cased form of s.
// sub must ALREADY be lower-cased (and therefore valid UTF-8 — strings.ToLower
// never emits an invalid byte, and the scan relies on it: an invalid needle
// byte would decode as U+FFFD and match the haystack's replacement rune where
// the baseline does not). Token.lower is such a string, and it is the only
// thing this is called with.
//
// Exactly equivalent to strings.Contains(strings.ToLower(s), sub), which is
// what it replaces, but it lowers one rune at a time as it scans instead of
// building a lower-cased copy of every job field. The TUI runs Match over
// every job on every keystroke and on every list rebuild, so those copies were
// one allocation per job per match.
//
// unicode.ToLower, NOT unicode.SimpleFold: the JS twin
// (web/public/modules/filter-engine.js matchTerm) compares
// String.prototype.toLowerCase values, and the two relations differ —
// SimpleFold puts U+017F LATIN SMALL LETTER LONG S in the same orbit as 's',
// so a fold-based search would match "ſun" for the query "sun" where neither
// twin matches anything today. ToLower is a per-rune mapping in Go, so
// scanning rune by rune reproduces strings.ToLower exactly, invalid UTF-8
// included: a bad byte decodes as U+FFFD, which is the rune strings.ToLower
// writes for it.
func containsFoldLower(s, sub string) bool {
	if sub == "" {
		return true
	}
	first, _ := utf8.DecodeRuneInString(sub)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if lowerRune(r) == first && hasPrefixFoldLower(s[i:], sub) {
			return true
		}
		i += size
	}
	return false
}

// hasPrefixFoldLower reports whether the lower-cased form of s starts with sub
// (already lower-cased).
func hasPrefixFoldLower(s, sub string) bool {
	for len(sub) > 0 {
		if len(s) == 0 {
			return false
		}
		sr, ssize := utf8.DecodeRuneInString(s)
		br, bsize := utf8.DecodeRuneInString(sub)
		if lowerRune(sr) != br {
			return false
		}
		s, sub = s[ssize:], sub[bsize:]
	}
	return true
}

// equalFoldLower reports whether the lower-cased form of s equals sub (already
// lower-cased, and therefore valid UTF-8, for the reason containsFoldLower
// spells out). Exactly equivalent to strings.ToLower(s) == sub, without
// building the lower-cased copy.
//
// NOT strings.EqualFold, which is what the status, channel and platform arms
// used to call. EqualFold is the SimpleFold ORBIT relation, and the orbit is
// wider than case: U+017F LATIN SMALL LETTER LONG S shares 's's orbit, so
// EqualFold("ſun", "sun") is true while the dashboard twin
// (web/public/modules/filter-engine.js, `toLowerCase() === toLowerCase()`)
// says false. ToLower equality is the relation the text arm already uses, so
// after this every arm of matchTerm folds case the one way both UIs agree on.
//
// The twins still differ on the points where Go's ToLower and JS's
// toLowerCase themselves disagree — U+0130 LATIN CAPITAL LETTER I WITH DOT
// ABOVE (Go maps it to 'i', JS to "i̇") and the Greek final sigma — in this
// arm and in the text arm alike. Pre-existing, identical in both, out of
// scope here.
func equalFoldLower(s, sub string) bool {
	for len(sub) > 0 {
		if len(s) == 0 {
			return false
		}
		sr, ssize := utf8.DecodeRuneInString(s)
		br, bsize := utf8.DecodeRuneInString(sub)
		if lowerRune(sr) != br {
			return false
		}
		s, sub = s[ssize:], sub[bsize:]
	}
	return len(s) == 0
}

// lowerRune is unicode.ToLower with the ASCII fast path strings.ToLower also
// takes — the case of every Latin title and every query typed into the / box.
func lowerRune(r rune) rune {
	if r < utf8.RuneSelf {
		if 'A' <= r && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}
	return unicode.ToLower(r)
}

// Match reports whether job passes every token (AND across tokens, OR within
// a group, negation per term) — web/public/modules/filter-engine.js.
func Match(tokens []Token, job *database.Job) bool {
	for _, t := range tokens {
		if t.Kind == KindOr {
			if !slices.ContainsFunc(t.Terms, func(term Token) bool { return matchTerm(term, job) }) {
				return false
			}
			continue
		}
		if !matchTerm(t, job) {
			return false
		}
	}
	return true
}
