// Package jobfilter is the Go twin of web/public/modules/filter-parser.js
// and filter-engine.js — the dashboard's filter language, used by the TUI's
// / box. Changes to the language land in both places; the tests here mirror
// the node suites value for value.
package jobfilter

import (
	"slices"
	"strings"

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

// BucketStatuses is the Web's STATUS_FILTER_MAP.
var BucketStatuses = map[string][]database.JobStatus{
	"active": {database.StatusDownloading, database.StatusLive, database.StatusUpcoming, database.StatusMuxing, database.StatusQueued},
	"issues": {database.StatusError, database.StatusCancelled, database.StatusCookies},
	// status:errors was the bucket's name before Cancelled joined it; keep
	// hand-typed queries working.
	"errors":   {database.StatusError, database.StatusCancelled, database.StatusCookies},
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

// Parse tokenises a query the way web/public/modules/filter-parser.js does.
func Parse(query string) []Token {
	trimmed := strings.TrimSpace(query)
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
	// Re-quote spaced phrases or the round-trip corrupts them: an unquoted
	// "-jelly fin" re-tokenizes as TWO tokens, flipping half the phrase
	// from negated to required.
	val := t.Value
	if strings.Contains(val, " ") {
		val = `"` + val + `"`
	}
	if t.Kind == KindText {
		return prefix + val
	}
	return prefix + string(t.Kind) + ":" + val
}

// Serialize renders tokens back to query text (filter-parser.js serializeToken).
func Serialize(tokens []Token) string {
	parts := make([]string, len(tokens))
	for i, t := range tokens {
		parts[i] = serializeToken(t)
	}
	return strings.Join(parts, " ")
}

// matchTerm tests whether a single term matches a job —
// filter-engine.js matchTerm, plus the TUI's VideoID field in the text
// match.
func matchTerm(t Token, job *database.Job) bool {
	var result bool
	switch t.Kind {
	case KindText:
		result = strings.Contains(strings.ToLower(job.Title), t.lower) ||
			strings.Contains(strings.ToLower(job.ChannelName), t.lower) ||
			strings.Contains(strings.ToLower(job.VideoID), t.lower)
	case KindStatus:
		if statuses, ok := BucketStatuses[StatusBucket(t.lower)]; ok {
			result = slices.Contains(statuses, job.Status)
		} else {
			result = strings.EqualFold(string(job.Status), t.Value)
		}
	case KindChannel:
		result = strings.EqualFold(job.ChannelName, t.Value)
	case KindPlatform:
		result = strings.EqualFold(job.Platform, t.Value)
	default:
		result = true
	}
	if t.Negate {
		return !result
	}
	return result
}

// Match reports whether job passes every token (AND across tokens, OR within
// a group, negation per term) — web/public/modules/filter-engine.js, plus
// the TUI's VideoID field in the text match.
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
