package jobfilter

import "testing"

// roundTripValues are values Serialize used to emit in a form Parse read
// back as something else (web/tests/filter-parser.test.mjs runs the same list
// through the JS twin).
var roundTripValues = []string{
	`foo" bar`,    // the delimiter inside the quotes ended them early
	`"foo bar`,    // a leading quote opened quoting
	`"quoted"`,    // a matching pair was stripped off
	`'single'`,    // likewise
	`-dash`,       // came back negated (text)
	`status:live`, // came back as a status filter (text)
	`Channel:x`,   // likewise, any case
	`mori's set`,  // mid-token apostrophe beside a space
	`a|b`,         // came back as an OR group
	`plain`,
}

// Mutant: quoteValue reduced to the old space/pipe rule — most rows fail.
func TestSerializeRoundTrips(t *testing.T) {
	for _, kind := range []Kind{KindText, KindChannel} {
		for _, negate := range []bool{false, true} {
			for _, v := range roundTripValues {
				tok := Term(kind, v, negate)
				q := Serialize([]Token{tok, Term(KindStatus, "active", false)})
				got := Parse(q)
				if len(got) != 2 || got[0].Kind != kind || got[0].Value != v || got[0].Negate != negate ||
					got[1].Kind != KindStatus || got[1].Value != "active" {
					t.Errorf("%v %q (negate %v) serialized to %q, parsed back as %+v", kind, v, negate, q, got)
				}
			}
		}
	}
}

// TrimQuery is Parse's trim: it keeps U+0085, which strings.TrimSpace strips
// and JavaScript's trim does not, and strips a BOM, which TrimSpace keeps.
//
// Mutant: TrimQuery as strings.TrimSpace.
func TestTrimQueryIsJavaScriptsTrim(t *testing.T) {
	if got := TrimQuery("\uFEFF \u0085foo "); got != "\u0085foo" {
		t.Errorf("TrimQuery = %q", got)
	}
}
