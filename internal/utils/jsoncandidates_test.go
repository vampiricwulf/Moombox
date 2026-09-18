package utils

import (
	"encoding/json"
	"regexp"
	"testing"
)

// TestScanBalancedJSONObjectIgnoresBracesInsideQuotes pins the string-state
// tracking that makes this a scanner rather than a brace counter. It moved
// here from internal/youtube (where it was scanBalancedObject); the three
// quote kinds are JS string state, not JSON, because the literal is embedded
// in a <script> body and the surrounding page is JavaScript.
//
// Mutant this kills: dropping the single-quote and backtick arms of the quote
// switch — those two rows then truncate at the brace inside the string and
// return the wrong literal.
func TestScanBalancedJSONObjectIgnoresBracesInsideQuotes(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"plain", `{"a":1} tail`, `{"a":1}`, true},
		{"brace inside a double-quoted string", `{"a":"}"} tail`, `{"a":"}"}`, true},
		{"escaped quote inside a string", `{"a":"\""} tail`, `{"a":"\""}`, true},
		{"brace inside a single-quoted string", `{'a':'}'} tail`, `{'a':'}'}`, true},
		{"brace inside a backtick string", "{`a`:`}`} tail", "{`a`:`}`}", true},
		{"nested", `{"a":{"b":2}} tail`, `{"a":{"b":2}}`, true},
		{"never closes", `{"a":1`, "", false},
		{"does not start on a brace", `x{"a":1}`, "", false},
		{"empty", ``, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ScanBalancedJSONObject([]byte(tc.in))
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (got %q)", ok, tc.ok, got)
			}
			if ok && string(got) != tc.want {
				t.Errorf("literal = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFindJSONObjectCandidateReturnsTheFirstAcceptedLiteral pins the search
// itself: a candidate the accept function rejects must not end it.
//
// Mutant this kills: returning on the first match (the pre-change control
// flow of extractYtInitialDataInto and chat.ExtractChatContinuation — one
// FindIndex, one scan). It returns the forged `{}` instead of the real
// object, which is how a page-authored `var X = {}` denied the real document.
func TestFindJSONObjectCandidateReturnsTheFirstAcceptedLiteral(t *testing.T) {
	page := []byte(`<p>var X = {} </p><script>var X = {"real":true};</script>`)
	anchors := []*regexp.Regexp{regexp.MustCompile(`var X\s*=\s*\{`)}

	var seen []string
	obj, ok := FindJSONObjectCandidate(page, anchors, func(o []byte) bool {
		seen = append(seen, string(o))
		// A stand-in for a real consumer's accept: something between the
		// braces AND a successful decode. (IsNonEmptyJSONObject used to be
		// spelled here; it had no production caller and was deleted —
		// close-review Finding 15.)
		var cand map[string]any
		return IsNonEmptyJSONBody(o) && json.Unmarshal(o, &cand) == nil
	})
	if !ok {
		t.Fatal("FindJSONObjectCandidate found nothing — a rejected candidate ended the search")
	}
	if string(obj) != `{"real":true}` {
		t.Errorf("literal = %q, want the real object", obj)
	}
	if len(seen) != 2 || seen[0] != `{}` || seen[1] != `{"real":true}` {
		t.Errorf("accept saw %q, want the forged {} then the real object", seen)
	}
}

// TestFindJSONObjectCandidateDoesNotRescanRejectedOffsets pins the
// rejected-offset set. The real anchor lists overlap by construction: the
// player-response list's third anchor is a BARE `ytInitialPlayerResponse\s*=\s*\{`,
// which matches INSIDE every `var ytInitialPlayerResponse = {` occurrence the
// first anchor already tried. Without the set, a page where every candidate
// fails pays for each one twice — scan and accept — on a payload that is
// megabytes on a real watch page.
//
// Mutant this kills: dropping the rejected map (or recording the match start
// instead of the brace offset, which never collides) — accept is then called
// 4 times instead of 2, twice per distinct `{`.
func TestFindJSONObjectCandidateDoesNotRescanRejectedOffsets(t *testing.T) {
	page := []byte(`<p>var X = {} and var X = {}</p>`)
	anchors := []*regexp.Regexp{
		regexp.MustCompile(`var X\s*=\s*\{`),
		regexp.MustCompile(`X\s*=\s*\{`), // the bare twin: matches inside every `var X = {`
	}

	calls := 0
	obj, ok := FindJSONObjectCandidate(page, anchors, func([]byte) bool {
		calls++
		return false
	})
	if ok || obj != nil {
		t.Fatalf("FindJSONObjectCandidate = (%q, %v), want (nil, false) — every candidate was rejected", obj, ok)
	}
	if calls != 2 {
		t.Errorf("accept was called %d times, want 2 — one per distinct `{`; a later anchor must not re-offer an offset an earlier one already rejected", calls)
	}
}

// TestIsNonEmptyJSONBodyDoesNotValidate pins the split behind row #60. The
// emptiness half is load-bearing on its own — `{}` scans and decodes
// perfectly well, so without it a forged empty object wins the search exactly
// as a forged non-object cannot — but the json.Valid scan is a SECOND full
// pass over a multi-megabyte literal whose caller is about to decode it
// anyway.
//
// Mutants this kills:
//   - IsNonEmptyJSONBody still calling json.Valid → the malformed case fails
//   - the emptiness check dropped                  → the "{}" case fails
func TestIsNonEmptyJSONBodyDoesNotValidate(t *testing.T) {
	if IsNonEmptyJSONBody([]byte(`{}`)) {
		t.Error("an empty object was accepted")
	}
	if IsNonEmptyJSONBody([]byte(`{`)) {
		t.Error("a one-byte fragment was accepted")
	}
	if !IsNonEmptyJSONBody([]byte(`{"a":1}`)) {
		t.Error("a real object was rejected")
	}
	// Deliberately malformed: the caller's typed decode is what rejects this
	// now, so this predicate must NOT.
	if !IsNonEmptyJSONBody([]byte(`{"a":}`)) {
		t.Error("IsNonEmptyJSONBody validated the body — that is the decode's job now")
	}
}
