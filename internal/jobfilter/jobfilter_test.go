package jobfilter

import (
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// equalToken compares two tokens ignoring the unexported lower field, which
// is a derived cache and not part of a token's identity.
func equalToken(a, b Token) bool {
	if a.Kind != b.Kind || a.Value != b.Value || a.Negate != b.Negate {
		return false
	}
	if len(a.Terms) != len(b.Terms) {
		return false
	}
	for i := range a.Terms {
		if !equalToken(a.Terms[i], b.Terms[i]) {
			return false
		}
	}
	return true
}

func equalTokens(a, b []Token) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !equalToken(a[i], b[i]) {
			return false
		}
	}
	return true
}

// ---- filter-parser.test.mjs -----------------------------------------------

func TestParseEmptyString(t *testing.T) {
	// node test: "parseFilterQuery: empty string returns []"
	// (the node case also asserts parseFilterQuery(null) === []; Go's Parse
	// takes a typed string so there is no null to pass — the empty-string
	// and whitespace-only cases below are the applicable subset.)
	if got := Parse(""); len(got) != 0 {
		t.Errorf("Parse(\"\") = %+v, want empty", got)
	}
	if got := Parse("   "); len(got) != 0 {
		t.Errorf("Parse(\"   \") = %+v, want empty", got)
	}
}

func TestParseSingleTextTerm(t *testing.T) {
	// node test: "parseFilterQuery: single text term"
	got := Parse("hello")
	want := []Token{{Kind: KindText, Value: "hello", Negate: false}}
	if !equalTokens(got, want) {
		t.Errorf("Parse(\"hello\") = %+v, want %+v", got, want)
	}
}

func TestParseNegatedTextTerm(t *testing.T) {
	// node test: "parseFilterQuery: negated text term"
	got := Parse("-spam")
	want := []Token{{Kind: KindText, Value: "spam", Negate: true}}
	if !equalTokens(got, want) {
		t.Errorf("Parse(\"-spam\") = %+v, want %+v", got, want)
	}
}

func TestParseMultipleTermsAnd(t *testing.T) {
	// node test: "parseFilterQuery: multiple terms (AND)"
	got := Parse("foo bar -baz")
	want := []Token{
		{Kind: KindText, Value: "foo", Negate: false},
		{Kind: KindText, Value: "bar", Negate: false},
		{Kind: KindText, Value: "baz", Negate: true},
	}
	if !equalTokens(got, want) {
		t.Errorf("Parse(\"foo bar -baz\") = %+v, want %+v", got, want)
	}
}

func TestParseNamespacedTerm(t *testing.T) {
	// node test: "parseFilterQuery: namespaced term"
	got := Parse("status:active")
	want := []Token{{Kind: KindStatus, Value: "active", Negate: false}}
	if !equalTokens(got, want) {
		t.Errorf("Parse(\"status:active\") = %+v, want %+v", got, want)
	}
}

func TestParseQuotedNamespacedValue(t *testing.T) {
	// node test: "parseFilterQuery: quoted namespaced value"
	got := Parse(`channel:"shachi too"`)
	want := []Token{{Kind: KindChannel, Value: "shachi too", Negate: false}}
	if !equalTokens(got, want) {
		t.Errorf("Parse(channel:\"shachi too\") = %+v, want %+v", got, want)
	}
}

func TestParseOrViaPipe(t *testing.T) {
	// node test: "parseFilterQuery: OR via pipe"
	got := Parse("foo|bar")
	want := []Token{{
		Kind: KindOr,
		Terms: []Token{
			{Kind: KindText, Value: "foo", Negate: false},
			{Kind: KindText, Value: "bar", Negate: false},
		},
	}}
	if !equalTokens(got, want) {
		t.Errorf("Parse(\"foo|bar\") = %+v, want %+v", got, want)
	}
}

func TestParseUnknownNamespaceFallsBackToText(t *testing.T) {
	// node test: "parseFilterQuery: unknown namespace falls back to text type"
	got := Parse("foo:bar")
	want := []Token{{Kind: KindText, Value: "foo:bar", Negate: false}}
	if !equalTokens(got, want) {
		t.Errorf("Parse(\"foo:bar\") = %+v, want %+v", got, want)
	}
}

func TestParsePipeInsideQuotesIsLiteral(t *testing.T) {
	// node test: "parseFilterQuery: pipe inside quotes is literal, not OR"
	got := Parse(`channel:"a|b"`)
	want := []Token{{Kind: KindChannel, Value: "a|b", Negate: false}}
	if !equalTokens(got, want) {
		t.Errorf("Parse(channel:\"a|b\") = %+v, want %+v", got, want)
	}
}

func TestParseNegatedNamespaced(t *testing.T) {
	// node test: "parseFilterQuery: negated namespaced"
	got := Parse("-platform:twitch")
	want := []Token{{Kind: KindPlatform, Value: "twitch", Negate: true}}
	if !equalTokens(got, want) {
		t.Errorf("Parse(\"-platform:twitch\") = %+v, want %+v", got, want)
	}
}

func TestSerializeRoundTripsPlainText(t *testing.T) {
	// node test: "serializeToken: round-trips plain text"
	got := Serialize([]Token{{Kind: KindText, Value: "hello", Negate: false}})
	if got != "hello" {
		t.Errorf("Serialize = %q, want %q", got, "hello")
	}
}

func TestSerializeRoundTripsNegatedText(t *testing.T) {
	// node test: "serializeToken: round-trips negated text"
	got := Serialize([]Token{{Kind: KindText, Value: "spam", Negate: true}})
	if got != "-spam" {
		t.Errorf("Serialize = %q, want %q", got, "-spam")
	}
}

func TestSerializeRoundTripsNamespacedQuoted(t *testing.T) {
	// node test: "serializeToken: round-trips namespaced with space -> quoted"
	got := Serialize([]Token{{Kind: KindChannel, Value: "shachi too", Negate: false}})
	want := `channel:"shachi too"`
	if got != want {
		t.Errorf("Serialize = %q, want %q", got, want)
	}
}

func TestSerializeRoundTripsOrGroup(t *testing.T) {
	// node test: "serializeToken: round-trips OR group"
	got := Serialize([]Token{{
		Kind: KindOr,
		Terms: []Token{
			{Kind: KindText, Value: "foo", Negate: false},
			{Kind: KindText, Value: "bar", Negate: true},
		},
	}})
	if got != "foo|-bar" {
		t.Errorf("Serialize = %q, want %q", got, "foo|-bar")
	}
}

func TestParseSerializeParseIdentity(t *testing.T) {
	// node test: "parse -> serialize -> parse is identity for common queries"
	queries := []string{
		"foo",
		"-spam",
		"status:active",
		`channel:"shachi too"`,
		"foo|bar",
		"foo bar -baz status:active",
	}
	for _, q := range queries {
		parsed := Parse(q)
		serialized := Serialize(parsed)
		reparsed := Parse(serialized)
		if !equalTokens(reparsed, parsed) {
			t.Errorf("identity failed for %q: parsed=%+v serialized=%q reparsed=%+v", q, parsed, serialized, reparsed)
		}
	}
}

// ---- filter-engine.test.mjs -------------------------------------------

var jobs = []*database.Job{
	{ID: "1", Title: "Hololive concert", ChannelName: "Shachi Too", Status: database.StatusLive, Platform: "youtube"},
	{ID: "2", Title: "Random clip", ChannelName: "Shachi Too", Status: database.StatusFinished, Platform: "youtube"},
	{ID: "3", Title: "Stream VOD", ChannelName: "Another Ch", Status: database.StatusError, Platform: "twitch"},
	{ID: "4", Title: "Upcoming debut", ChannelName: "Debut Channel", Status: database.StatusUpcoming, Platform: "youtube"},
	{ID: "5", Title: "Muxing now", ChannelName: "Debut Channel", Status: database.StatusMuxing, Platform: "youtube"},
}

// Bucket membership is checked on its own fixture so the shared jobs list
// (which has no Cancelled/COOKIES? rows) keeps every other expectation.
var bucketJobs = []*database.Job{
	{ID: "e", Status: database.StatusError, Platform: "youtube"},
	{ID: "c", Status: database.StatusCancelled, Platform: "youtube"},
	{ID: "k", Status: database.StatusCookies, Platform: "youtube"},
	{ID: "f", Status: database.StatusFinished, Platform: "youtube"},
}

func filterIDs(list []*database.Job, query string) []string {
	tokens := Parse(query)
	var ids []string
	for _, j := range list {
		if Match(tokens, j) {
			ids = append(ids, j.ID)
		}
	}
	return ids
}

func filterWith(query string) []string { return filterIDs(jobs, query) }
func bucketWith(query string) []string { return filterIDs(bucketJobs, query) }

// assertIDs compares got against want by length and content, treating a nil
// slice (no matches) the same as an explicitly empty want list.
func assertIDs(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestEmptyQueryReturnsAllJobs(t *testing.T) {
	// node test: "empty query returns all jobs"
	assertIDs(t, filterWith(""), "1", "2", "3", "4", "5")
}

func TestTextFilterMatchesTitle(t *testing.T) {
	// node test: "text filter matches title"
	assertIDs(t, filterWith("clip"), "2")
}

func TestTextFilterMatchesChannel(t *testing.T) {
	// node test: "text filter matches channel"
	assertIDs(t, filterWith("debut"), "4", "5")
}

func TestTextFilterCaseInsensitive(t *testing.T) {
	// node test: "text filter is case-insensitive"
	assertIDs(t, filterWith("SHACHI"), "1", "2")
}

func TestNegatedTextFilterExcludes(t *testing.T) {
	// node test: "negated text filter excludes"
	assertIDs(t, filterWith("-shachi"), "3", "4", "5")
}

func TestMultipleAndTextFilters(t *testing.T) {
	// node test: "multiple AND text filters"
	assertIDs(t, filterWith("debut -muxing"), "4")
}

func TestStatusActiveGroupsLiveUpcomingDownloadingMuxing(t *testing.T) {
	// node test: "status: active groups live/upcoming/downloading/muxing"
	ids := filterWith("status:active")
	sort.Strings(ids)
	assertIDs(t, ids, "1", "4", "5")
}

func TestStatusIssuesGroupsErrorCancelledCookies(t *testing.T) {
	// node test: "status: issues groups Error + Cancelled + COOKIES? (Q4 — the TUI's Issues bucket)"
	assertIDs(t, bucketWith("status:issues"), "e", "c", "k")
}

func TestStatusFinishedIsFinishedOnly(t *testing.T) {
	// node test: "status: finished is Finished only — Cancelled moved to issues"
	assertIDs(t, bucketWith("status:finished"), "f")
}

func TestStatusErrorsAliasOfIssues(t *testing.T) {
	// node test: "status: errors stays an alias of issues for hand-typed queries"
	got := bucketWith("status:errors")
	want := bucketWith("status:issues")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bucketWith(\"status:errors\") = %v, want same as status:issues = %v", got, want)
	}
}

func TestPlatformTwitchMatchesPlatformOnly(t *testing.T) {
	// node test: "platform: twitch matches platform only"
	assertIDs(t, filterWith("platform:twitch"), "3")
}

func TestPlatformStatusCombined(t *testing.T) {
	// node test: "platform + status combined"
	ids := filterWith("platform:youtube status:active")
	sort.Strings(ids)
	assertIDs(t, ids, "1", "4", "5")
}

func TestChannelExactMatchCaseInsensitive(t *testing.T) {
	// node test: "channel: exact match (case-insensitive)"
	ids := filterWith(`channel:"shachi too"`)
	sort.Strings(ids)
	assertIDs(t, ids, "1", "2")
}

func TestOrPipeMatchesAnyBranch(t *testing.T) {
	// node test: "OR pipe: matches any branch"
	ids := filterWith("status:active|status:issues")
	sort.Strings(ids)
	assertIDs(t, ids, "1", "3", "4", "5")
}

func TestNegatedNamespacedFilter(t *testing.T) {
	// node test: "negated namespaced filter"
	ids := filterWith("-platform:twitch")
	sort.Strings(ids)
	assertIDs(t, ids, "1", "2", "4", "5")
}

func TestUnknownStatusValueYieldsZeroMatches(t *testing.T) {
	// node test: "unknown status value yields zero matches"
	assertIDs(t, filterWith("status:unknown"))
}

// ---- TUI superset (ruling H2) -------------------------------------------

// TUI superset (ruling H2): a text term also matches the video ID by
// case-insensitive substring; fuzzy subsequence matching is gone.
func TestTextMatchesVideoIDBySubstring(t *testing.T) {
	j := &database.Job{ID: "j", Title: "Something", ChannelName: "Chan", VideoID: "dQw4w9WgXcQ", Status: database.StatusFinished, Platform: "youtube"}
	if !Match(Parse("w9wg"), j) {
		t.Fatal("substring of the video ID must match")
	}
	if Match(Parse("dQwWgX"), j) { // subsequence, not substring
		t.Fatal("fuzzy subsequence must NOT match — semantics are the Web's substring match")
	}
}

func TestStatusBucket(t *testing.T) {
	for in, want := range map[string]string{"active": "active", "ISSUES": "issues", "errors": "issues", "finished": "finished", "live": "", "": ""} {
		if got := StatusBucket(in); got != want {
			t.Errorf("StatusBucket(%q) = %q, want %q", in, got, want)
		}
	}
}

func BenchmarkMatch1000(b *testing.B) {
	jobs := make([]*database.Job, 1000)
	for i := range jobs {
		jobs[i] = &database.Job{ID: fmt.Sprint(i), Title: fmt.Sprintf("Stream %d karaoke night", i), ChannelName: "Channel " + fmt.Sprint(i%20), VideoID: fmt.Sprintf("vid%08d", i), Status: database.StatusFinished, Platform: "youtube"}
	}
	tokens := Parse(`karaoke status:finished -platform:twitch`)
	b.ResetTimer()
	for b.Loop() {
		for _, j := range jobs {
			Match(tokens, j)
		}
	}
}
