package youtube

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestIsConsentRedirect pins the EU consent-wall detection FetchWatchPage
// applies to the fetch's FINAL URL (after redirects). The consent
// interstitial answers 200 with no ytcfg/playerResponse, so without this
// check a cookie-less EU user silently loses visitorData, PlayerURL, STS,
// and PO tokens with nothing in the log. Mirrors the detection the chat
// API has had since audit chat.md C14.
func TestIsConsentRedirect(t *testing.T) {
	mk := func(host string) *http.Response {
		return &http.Response{Request: &http.Request{URL: &url.URL{Scheme: "https", Host: host}}}
	}

	if !isConsentRedirect(mk("consent.youtube.com")) {
		t.Error("consent.youtube.com must be detected as a consent redirect")
	}
	if !isConsentRedirect(mk("consent.google.com")) {
		t.Error("consent.google.com must be detected as a consent redirect")
	}
	if isConsentRedirect(mk("www.youtube.com")) {
		t.Error("www.youtube.com must NOT be detected as a consent redirect")
	}
	if isConsentRedirect(&http.Response{Request: &http.Request{}}) {
		t.Error("nil URL must be a safe non-match")
	}
	if isConsentRedirect(&http.Response{}) {
		t.Error("nil Request must be a safe non-match")
	}
}

func TestJSURLRegex_AcceptsScriptSrc(t *testing.T) {
	// Modern YouTube watch pages have sometimes shipped a "scriptSrc" key
	// pointing at the player JS. Make sure the regex still catches it.
	html := `{"scriptSrc":"/s/player/abc123/player_ias.vflset/en_US/base.js"}`
	m := jsURLRegex.FindStringSubmatch(html)
	if m == nil {
		t.Fatalf("expected jsURLRegex to match scriptSrc form")
	}
	if !strings.HasSuffix(m[1], "base.js") {
		t.Errorf("expected captured path to end at base.js, got %q", m[1])
	}
}

func TestNormalizePlayerJSURL(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{
			`\/s\/player\/abc\/player_ias.vflset\/en_US\/base.js`,
			"https://www.youtube.com/s/player/abc/player_ias.vflset/en_US/base.js",
		},
		{
			"/s/player/abc/base.js",
			"https://www.youtube.com/s/player/abc/base.js",
		},
		{
			"https://fonts.googleapis.com/player.js",
			"https://fonts.googleapis.com/player.js",
		},
	}
	for _, tt := range tests {
		if got := normalizePlayerJSURL(tt.in); got != tt.want {
			t.Errorf("normalizePlayerJSURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestExtractEncryptedHostFlags(t *testing.T) {
	html := `some html before "WEB_PLAYER_CONTEXT_CONFIGS":{"WEB_PLAYER_CONTEXT_CONFIG_ID_EMBEDDED_PLAYER":{"encryptedHostFlags":"abc123xyz=="}} some html after`

	flags := extractEncryptedHostFlags(html)
	if flags != "abc123xyz==" {
		t.Errorf("expected 'abc123xyz==', got %q", flags)
	}
}

func TestExtractEncryptedHostFlags_WithInterveningKeys(t *testing.T) {
	html := `"WEB_PLAYER_CONTEXT_CONFIGS":{"WEB_PLAYER_CONTEXT_CONFIG_ID_EMBEDDED_PLAYER":{"otherKey":"otherValue","encryptedHostFlags":"def456uvw=="}}`

	flags := extractEncryptedHostFlags(html)
	if flags != "def456uvw==" {
		t.Errorf("expected 'def456uvw==', got %q", flags)
	}
}

func TestExtractEncryptedHostFlags_Missing(t *testing.T) {
	html := `some html without the config`

	flags := extractEncryptedHostFlags(html)
	if flags != "" {
		t.Errorf("expected empty string, got %q", flags)
	}
}

func TestExtractAttestationChallenge(t *testing.T) {
	// Shape per moonarchive 96344fe: window.ytAtN({...}) whose R key is a
	// JSON *string* containing bgChallenge.
	challenge := `{"interpreterUrl":{"privateDoNotAccessOrElseTrustedResourceUrlWrappedValue":"//www.google.com/js/th/abc.js"},"interpreterHash":"h","program":"prog","globalName":"trayride"}`
	rPayload, _ := json.Marshal(map[string]any{"bgChallenge": json.RawMessage(challenge)})
	atn, _ := json.Marshal(string(rPayload))
	page := `<html><script>window.ytAtN({R: ` + string(atn) + `, other: 1});</script></html>`

	got, reason := extractAttestationChallenge([]byte(page))
	if got == "" {
		t.Fatalf("expected challenge, got empty (reason=%s)", reason)
	}
	if reason != atnOK {
		t.Errorf("reason = %q, want %q", reason, atnOK)
	}
	var back map[string]any
	if err := json.Unmarshal([]byte(got), &back); err != nil {
		t.Fatalf("result not JSON: %v", err)
	}
	if back["globalName"] != "trayride" || back["program"] != "prog" {
		t.Errorf("challenge content mangled: %s", got)
	}

	for name, html := range map[string]string{
		"absent":            `<html><script>var x = 1;</script></html>`,
		"malformed_js":      `<html><script>window.ytAtN({R: });</script></html>`,
		"missing_R":         `<html><script>window.ytAtN({Q: "{}"});</script></html>`,
		"R_not_json":        `<html><script>window.ytAtN({R: "not json"});</script></html>`,
		"missing_challenge": `<html><script>window.ytAtN({R: "{\"noChallenge\":1}"});</script></html>`,
	} {
		if got, _ := extractAttestationChallenge([]byte(html)); got != "" {
			t.Errorf("%s: expected empty, got %q", name, got)
		}
	}
}

// TestExtractAttestationChallengeRejectsHostileOrigin pins the security gate
// added after the 2026-08-15 review: watch-page HTML embeds attacker-authored
// video metadata verbatim (JSON escaping leaves braces, parens and single
// quotes intact), so a crafted description can present itself as a ytAtN
// challenge. The sidecar EXECUTES the interpreter body it fetches, so a
// challenge naming a non-Google interpreter host must never leave this
// process.
func TestExtractAttestationChallengeRejectsHostileOrigin(t *testing.T) {
	hostile := `window.ytAtN({R:'{\"bgChallenge\":{\"program\":\"P\",\"globalName\":\"g\",` +
		`\"interpreterUrl\":{\"privateDoNotAccessOrElseTrustedResourceUrlWrappedValue\":\"//evil.tld/p.js\"}}}'})`
	page := `<html><script>var ytInitialPlayerResponse = {"shortDescription":"` + hostile + `"};</script></html>`

	got, reason := extractAttestationChallenge([]byte(page))
	if got != "" {
		t.Fatalf("hostile challenge was accepted: %s", got)
	}
	if !strings.HasPrefix(reason, atnBadInterpHost) {
		t.Errorf("reason = %q, want prefix %q", reason, atnBadInterpHost)
	}
}

// TestAllowedInterpreterHosts pins the EXACT-host rule. Every rejected case
// below was a live bypass proven against the previous suffix/pattern gate on
// 2026-08-15: storage.googleapis.com serves anyone's uploaded bucket objects,
// sites/script.google.com host third-party content, and google.com.se /
// google.co.nl are registrable third-party domains that merely look Google-ish.
func TestAllowedInterpreterHosts(t *testing.T) {
	for _, h := range []string{
		"www.google.com", "google.com", "www.gstatic.com", "ssl.gstatic.com",
		"s.ytimg.com", "www.youtube.com",
	} {
		if !slices.Contains(allowedInterpreterHosts, strings.ToLower(h)) {
			t.Errorf("%s should be allowed", h)
		}
	}
	for _, h := range []string{
		"storage.googleapis.com",         // anyone's GCS bucket objects
		"firebasestorage.googleapis.com", // anyone's Firebase uploads
		"commondatastorage.googleapis.com",
		"www.googleapis.com",
		"sites.google.com",  // third-party site builder
		"script.google.com", // third-party Apps Script
		"drive.google.com",  // user files
		"google.com.se",     // live third-party domain
		"google.co.nl", "google.org.ru", "google.pp.ru", "google.com.de",
		"google.de", "www.google.co.uk", // regional support intentionally dropped
		"i.ytimg.com", // user-uploaded thumbnails
		"lh3.googleusercontent.com", "yt3.ggpht.com",
		"rr2---sn-x.googlevideo.com",
		"evil.tld", "evilgoogle.com", "google.com.evil.tld", "notgstatic.com",
		"",
	} {
		if slices.Contains(allowedInterpreterHosts, strings.ToLower(h)) {
			t.Errorf("%s must NOT be allowed", h)
		}
	}
}

// TestCanonicalizeChallengeDefeatsParserDifferential pins the rebuild-don't-
// forward rule. Go's encoding/json matches keys case-insensitively with the
// last match winning, so a decoy key placed after the real one made Go
// validate an allowlisted host while the sidecar's case-sensitive JSON.parse
// read a different one from the same bytes. Canonicalization removes the
// class: the sidecar only ever sees fields this function rebuilt.
func TestCanonicalizeChallengeDefeatsParserDifferential(t *testing.T) {
	raw := `{"program":"P","globalName":"g",` +
		`"interpreterUrl":{"privateDoNotAccessOrElseTrustedResourceUrlWrappedValue":"//www.google.com/js/th/a.js",` +
		`"PRIVATEDONOTACCESSORELSETRUSTEDRESOURCEURLWRAPPEDVALUE":"//evil.tld/p.js"},` +
		`"interpreterJavascript":{"privateDoNotAccessOrElseSafeScriptWrappedValue":"alert(1)"},` +
		`"unexpectedExtra":"dropped"}`

	got, reason := canonicalizeChallenge(json.RawMessage(raw))
	if reason != atnOK {
		t.Fatalf("expected canonicalization to succeed, got reason %q", reason)
	}
	if strings.Contains(got, "evil.tld") {
		t.Errorf("decoy host survived canonicalization: %s", got)
	}
	if strings.Contains(got, "interpreterJavascript") || strings.Contains(got, "alert(1)") {
		t.Errorf("inline interpreter survived canonicalization: %s", got)
	}
	if strings.Contains(got, "unexpectedExtra") {
		t.Errorf("unknown field survived canonicalization: %s", got)
	}
	if !strings.Contains(got, "//www.google.com/js/th/a.js") {
		t.Errorf("validated URL missing from canonical output: %s", got)
	}
}

// TestCanonicalizeChallengeRejectsHostileHosts walks the concrete hosts the
// adversarial review used to reach code execution.
func TestCanonicalizeChallengeRejectsHostileHosts(t *testing.T) {
	for _, host := range []string{
		"storage.googleapis.com", "google.com.se", "evil.tld",
		"sites.google.com", "i.ytimg.com",
	} {
		raw := `{"program":"P","globalName":"g","interpreterUrl":{` +
			`"privateDoNotAccessOrElseTrustedResourceUrlWrappedValue":"//` + host + `/p.js"}}`
		got, reason := canonicalizeChallenge(json.RawMessage(raw))
		if got != "" {
			t.Errorf("%s: challenge accepted", host)
		}
		if !strings.HasPrefix(reason, atnBadInterpHost) {
			t.Errorf("%s: reason = %q, want prefix %q", host, reason, atnBadInterpHost)
		}
	}

	// Userinfo must be refused rather than reasoned about: the authority's
	// real host is not the one a careless reader sees.
	raw := `{"program":"P","globalName":"g","interpreterUrl":{` +
		`"privateDoNotAccessOrElseTrustedResourceUrlWrappedValue":"//www.google.com@evil.tld/p.js"}}`
	if got, reason := canonicalizeChallenge(json.RawMessage(raw)); got != "" {
		t.Errorf("userinfo host accepted (reason %q)", reason)
	}
}

// TestExtractAttestationChallengeBalancedScan covers the payload shapes the
// old non-greedy regex mis-handled: a `})` sequence inside the opaque
// challenge truncated the capture into an unbalanced fragment, which then
// failed to parse and was indistinguishable from "page carried no challenge".
func TestExtractAttestationChallengeBalancedScan(t *testing.T) {
	inner := `{"bgChallenge":{"program":"AA})BB","globalName":"g",` +
		`"interpreterUrl":{"privateDoNotAccessOrElseTrustedResourceUrlWrappedValue":"//www.google.com/js/th/a.js"}}}`
	rJSON, _ := json.Marshal(inner)
	page := `<html><script>window.ytAtN({R: ` + string(rJSON) + `});</script></html>`

	got, reason := extractAttestationChallenge([]byte(page))
	if got == "" {
		t.Fatalf("balanced scan failed on `})` payload (reason=%s)", reason)
	}
	var back map[string]any
	if err := json.Unmarshal([]byte(got), &back); err != nil {
		t.Fatalf("result not JSON: %v", err)
	}
	if back["program"] != "AA})BB" {
		t.Errorf("program mangled: %v", back["program"])
	}

	if got, reason := extractAttestationChallenge([]byte(`<script>window.ytAtN({R: "unclosed`)); got != "" || reason != atnUnbalanced {
		t.Errorf("unclosed literal: got %q reason %q, want empty/%s", got, reason, atnUnbalanced)
	}
}

// TestExtractAttestationChallengeRefusesInlineInterpreter pins the deliberate
// asymmetry with bgutils-js: interpreterJavascript and interpreterUrl are
// interchangeable upstream, but inline script arriving from scraped HTML has
// no origin to check, so a page-sourced challenge carrying it is refused and
// the sidecar's /att/get flow (a real YouTube API response) runs instead.
func TestExtractAttestationChallengeRefusesInlineInterpreter(t *testing.T) {
	inner := `{"bgChallenge":{"program":"P","globalName":"g","interpreterJavascript":{"privateDoNotAccessOrElseSafeScriptWrappedValue":"alert(1)"}}}`
	rJSON, _ := json.Marshal(inner)
	page := `<html><script>window.ytAtN({R: ` + string(rJSON) + `});</script></html>`

	got, reason := extractAttestationChallenge([]byte(page))
	if got != "" {
		t.Fatalf("inline-interpreter challenge accepted: %s", got)
	}
	if reason != atnNoInterpURL {
		t.Errorf("reason = %q, want %q", reason, atnNoInterpURL)
	}
}

// TestCanonicalizeChallengeRejectsReflectionEndpoints pins the round-2
// adversarial finding: an allowlisted HOST is not the same as
// Google-AUTHORED bytes. www.google.com serves JSONP endpoints that reflect
// an attacker-supplied callback back at HTTP 200 — no redirect, no
// look-alike domain, a genuinely allowlisted host — and the sidecar executes
// whatever it fetches. The reviewer drove real code execution through
// /complete/search?client=firefox&jsonp=<payload>.
//
// The genuine interpreter is a static TrustedResourceUrl (bare .js path, no
// query, no fragment), and reflection requires a query to reflect, so the
// shape requirement removes the class rather than this one endpoint.
func TestCanonicalizeChallengeRejectsReflectionEndpoints(t *testing.T) {
	hostile := []struct{ name, value string }{
		{
			"jsonp reflection (proven RCE)",
			"//www.google.com/complete/search?client=firefox&q=z&jsonp=eval(String.fromCharCode(1,2));Object",
		},
		{"any query at all", "//www.google.com/js/th/a.js?cb=payload"},
		{"bare query marker", "//www.google.com/js/th/a.js?"},
		{"fragment", "//www.google.com/js/th/a.js#payload"},
		{"non-script path", "//www.google.com/complete/search"},
		{"html path", "//www.google.com/index.html"},
	}
	for _, tc := range hostile {
		raw := `{"program":"P","globalName":"g","interpreterUrl":{` +
			`"privateDoNotAccessOrElseTrustedResourceUrlWrappedValue":"` + tc.value + `"}}`
		got, reason := canonicalizeChallenge(json.RawMessage(raw))
		if got != "" {
			t.Errorf("%s: accepted (%s)", tc.name, tc.value)
		}
		if !strings.HasPrefix(reason, atnBadInterpPath) {
			t.Errorf("%s: reason = %q, want prefix %q", tc.name, reason, atnBadInterpPath)
		}
	}

	// The genuine shape must still pass, or the gate has eaten the feature.
	raw := `{"program":"P","globalName":"g","interpreterUrl":{` +
		`"privateDoNotAccessOrElseTrustedResourceUrlWrappedValue":"//www.google.com/js/th/qtyJVB4UpQW6ehm0.js"}}`
	if got, reason := canonicalizeChallenge(json.RawMessage(raw)); got == "" {
		t.Errorf("genuine interpreter URL rejected: %s", reason)
	}
}

// TestCanonicalizeChallengeRejectsEncodedPaths pins the round-3
// defense-in-depth rule: the interpreter path is validated in its ENCODED
// form against an unreserved alphabet. Go decodes %3F into url.Path while
// JS's URL keeps it encoded, so /complete/search%3Fjsonp=X.js looks like a
// query-less .js path to Go and a literal-percent path to the sidecar. That
// URL 404s at Google today, which is the only thing that made it harmless —
// excluding '%' means correctness no longer depends on the origin's decoding.
func TestCanonicalizeChallengeRejectsEncodedPaths(t *testing.T) {
	for _, value := range []string{
		"//www.google.com/complete/search%3Fclient=firefox&jsonp=payload.js",
		"//www.google.com/js/th/a%2Fb.js",
		"//www.google.com/js/th/a%00.js",
		"//www.google.com/js/th/%252Fx.js",
		"//www.google.com/js/th/a.js%20",
		"//www.google.com/js/th/a b.js",
	} {
		raw := `{"program":"P","globalName":"g","interpreterUrl":{` +
			`"privateDoNotAccessOrElseTrustedResourceUrlWrappedValue":"` + value + `"}}`
		if got, reason := canonicalizeChallenge(json.RawMessage(raw)); got != "" {
			t.Errorf("accepted encoded path %q (reason %q)", value, reason)
		}
	}

	// The genuine shape — and the real captured URL — must still pass.
	for _, value := range []string{
		"//www.google.com/js/th/qtyJVB4UpQW6ehm0Eb6anVy7Y_bU8GitWVbp9gjCikM.js",
		"//www.gstatic.com/js/a-b_c.JS",
	} {
		raw := `{"program":"P","globalName":"g","interpreterUrl":{` +
			`"privateDoNotAccessOrElseTrustedResourceUrlWrappedValue":"` + value + `"}}`
		if got, reason := canonicalizeChallenge(json.RawMessage(raw)); got == "" {
			t.Errorf("genuine URL %q rejected: %s", value, reason)
		}
	}
}

// synthPlayerPage renders a watch page whose ytInitialPlayerResponse carries
// desc as its shortDescription, JSON-encoded exactly as YouTube encodes it.
// The trailing `var meta=1;` matters: it is a second `;` for the old lazy
// `({.+?});` pattern to reach, so a fixture without it would not reproduce
// the truncation this task fixes.
func synthPlayerPage(t *testing.T, desc string) string {
	t.Helper()
	encoded, err := json.Marshal(desc)
	if err != nil {
		t.Fatalf("marshal description: %v", err)
	}
	return `<!DOCTYPE html><html><head><script nonce="q">var ytInitialPlayerResponse = ` +
		`{"videoDetails":{"videoId":"abc12345678","title":"T","author":"A","channelId":"UC1",` +
		`"shortDescription":` + string(encoded) + `}};var meta=1;</script></head><body></body></html>`
}

// TestPlayerResponseSurvivesBraceSemicolonInDescription is ledger item T1-5.
//
// Mutant named: the old lazy `({.+?});` patterns. They stop at the FIRST `};`
// in the page, which a description containing a code sample supplies, so the
// captured text is unbalanced, every pattern fails identically, and
// PlayerResponse is nil with nothing in the log.
func TestPlayerResponseSurvivesBraceSemicolonInDescription(t *testing.T) {
	const desc = "code sample: if (x) {y();};  thanks for watching"
	page := synthPlayerPage(t, desc)

	ytcfg, pr := extractYtcfgAndPlayerResponse([]byte(page))
	if pr == nil {
		t.Fatal("playerResponse is nil — a `};` inside shortDescription truncated the object")
	}
	vd, _ := pr["videoDetails"].(map[string]any)
	if got, _ := vd["shortDescription"].(string); got != desc {
		t.Errorf("shortDescription = %q, want %q", got, desc)
	}
	if ytcfg.Description != desc {
		t.Errorf("ytcfg.Description = %q, want %q", ytcfg.Description, desc)
	}
}

// TestPlayerResponseScannerHonoursEscapedQuotes pins the OTHER half of the
// fix: the balanced scan must track backslash escapes.
//
// Mutant named: a scanner that does not track escapes. On this fixture the
// description's `"` arrives as `\"`; an escape-blind scanner leaves the
// string there, reads the following `}` as a closing brace, and returns the
// object one level short — which then fails to parse, or worse, parses into
// a truncated videoDetails.
func TestPlayerResponseScannerHonoursEscapedQuotes(t *testing.T) {
	const desc = `a " quote then } brace`
	page := synthPlayerPage(t, desc)

	_, pr := extractYtcfgAndPlayerResponse([]byte(page))
	if pr == nil {
		t.Fatal("playerResponse is nil — the scan mis-handled the escaped quote")
	}
	vd, _ := pr["videoDetails"].(map[string]any)
	if got, _ := vd["shortDescription"].(string); got != desc {
		t.Errorf("shortDescription = %q, want %q", got, desc)
	}
}

// legacyPlayerResponsePatterns are the three lazy regexes this task replaces.
// They live HERE, in the test, purely as the before-image for the equivalence
// check below: on every page shape the lazy form parsed, the anchored brace
// scan must return the same decoded object. The repository carries no
// watch-page fixture corpus, so this table IS the fixture set.
var legacyPlayerResponsePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?s)var ytInitialPlayerResponse\s*=\s*({.+?});`),
	regexp.MustCompile(`(?s)window\["ytInitialPlayerResponse"\]\s*=\s*({.+?});`),
	regexp.MustCompile(`(?s)ytInitialPlayerResponse\s*=\s*({.+?});`),
}

// legacyExtractPlayerResponse is the pre-change control flow, verbatim:
// first pattern whose capture unmarshals wins.
func legacyExtractPlayerResponse(html string) (map[string]any, bool) {
	for _, re := range legacyPlayerResponsePatterns {
		m := re.FindStringSubmatch(html)
		if m == nil {
			continue
		}
		var pr map[string]any
		if json.Unmarshal([]byte(m[1]), &pr) == nil {
			return pr, true
		}
	}
	return nil, false
}

// TestPlayerResponseExtractionMatchesTheLegacyPatterns is the before/after
// equivalence check. Each fixture must ALSO parse under the legacy patterns —
// a fixture that does not is not a before-image and the subtest says so.
//
// Mutant named: an anchor that matches at the wrong offset (e.g. one that
// forgets `loc[1]-1` and starts the scan one byte past the `{`) returns a
// different object, or none, on every row here.
func TestPlayerResponseExtractionMatchesTheLegacyPatterns(t *testing.T) {
	fixtures := map[string]string{
		"var form":          synthPlayerPage(t, "a benign description"),
		"window form":       `<script>window["ytInitialPlayerResponse"] = {"videoDetails":{"shortDescription":"plain"}};</script>`,
		"bare form":         `<script>ytInitialPlayerResponse = {"videoDetails":{"shortDescription":"plain"}};var x=1;</script>`,
		"spaced assignment": `<script>var ytInitialPlayerResponse   =   {"videoDetails":{"shortDescription":"plain"}};</script>`,
		"newlines inside":   "<script>var ytInitialPlayerResponse = {\n\"videoDetails\":{\"shortDescription\":\"plain\"}\n};</script>",
		"unicode escapes":   `<script>var ytInitialPlayerResponse = {"videoDetails":{"shortDescription":"\u007d\u003b end"}};</script>`,
	}
	for name, page := range fixtures {
		t.Run(name, func(t *testing.T) {
			want, ok := legacyExtractPlayerResponse(page)
			if !ok {
				t.Fatalf("fixture is not a before-image: the legacy patterns did not parse it")
			}
			got, ok := extractPlayerResponse([]byte(page))
			if !ok {
				t.Fatalf("the anchored scan found no object where the legacy patterns did")
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("anchored extraction differs from the legacy extraction\n got: %#v\nwant: %#v", got, want)
			}
		})
	}
}

// synthChatPage renders a watch page whose ytInitialData carries `filler`
// video renderers ahead of the conversationBar, so the cost of walking the
// document is realistic rather than notional. 500 items is ~148 KB.
func synthChatPage(filler int) []byte {
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html><head><script nonce="q">var ytInitialData = {"filler":[`)
	for i := range filler {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"videoRenderer":{"videoId":"abcdefghij%d","title":{"runs":[{"text":"Some fairly long video title number %d that pads the payload"}]},"thumbnail":{"thumbnails":[{"url":"https://i.ytimg.com/vi/abcdefghij%d/hqdefault.jpg","width":480,"height":360}]},"ownerText":{"runs":[{"text":"Channel Name %d"}]}}}`, i, i, i, i)
	}
	b.WriteString(`],"contents":{"twoColumnWatchNextResults":{"conversationBar":{"liveChatRenderer":` +
		`{"isReplay":false,"continuations":[{"reloadContinuationData":{"continuation":"CONTINUATION_TOKEN_XYZ"}}]}}}}};</script></head><body></body></html>`)
	return []byte(b.String())
}

func BenchmarkExtractChatContinuation(b *testing.B) {
	page := synthChatPage(500)
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := extractChatContinuation(page); err != nil {
			b.Fatal(err)
		}
	}
}

// chatContinuationAllocCeiling bounds extractChatContinuation's allocations.
//
// Measured 2026-09-15 on this exact fixture (go1.27.1/amd64): the
// map[string]any decode of the whole ytInitialData blob costs 32,593
// allocs/op at 500 filler items (148 KB) and 325,477 at 5,000 (1.5 MB) — it
// is linear in page size, and a real watch page is several times this one.
// The typed json.RawMessage envelope costs 6 at both sizes, independent of
// page size. The ceiling sits at 16 so ordinary
// encoding/json or Go-version drift does not flap it, while still being
// three orders of magnitude below the shape it replaces.
//
// Mutant named: any re-introduction of a whole-document map[string]any (or a
// json.RawMessage captured at `contents` rather than at liveChatRenderer,
// which copies the document) blows straight through 16.
const chatContinuationAllocCeiling = 16

func TestExtractChatContinuationAllocationCeiling(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation budget is not meaningful under the race detector")
	}
	page := synthChatPage(500)
	if got := testing.AllocsPerRun(20, func() {
		if _, _, err := extractChatContinuation(page); err != nil {
			t.Fatal(err)
		}
	}); got > chatContinuationAllocCeiling {
		t.Errorf("extractChatContinuation allocated %.0f per call, ceiling %d", got, chatContinuationAllocCeiling)
	}
}

// TestExtractChatContinuationShapes pins the behaviour the envelope must
// preserve, including the shapes the old `var ytInitialData = (…);</script>`
// regex could not read and the partial-decode cases the map[string]any walk
// survived.
//
// Mutants named: an envelope that keys the continuation list off only
// reloadContinuationData loses the replay row; a locator that keeps the
// `;</script>` terminator fails the "trailing script" row; a locator that
// keeps the `var ` prefix fails the "window property" row; a locator that
// also accepts a BARE `ytInitialData = {` fails the "forged" row.
//
// The four partial-decode rows are the ones that cost a real capture if they
// regress. encoding/json records the first type error and KEEPS decoding, so
// each of them yields err != nil AND a usable token. Returning the error
// there — `if err := json.Unmarshal(rendererRaw, &renderer); err != nil`, or
// hoisting the envelope's error return above the rendererRaw guard — hands
// both consumers an empty token, and orchestrator_chat.go and
// stream_processor_youtube.go both skip chat archiving on an empty token. So
// a YouTube type drift the map walk shrugged off would silently stop chat
// capture. The "renderer absent AND a type error" row is the other side: with
// no token to keep, the parse error IS the answer, and deleting that inner
// block must not survive.
func TestExtractChatContinuationShapes(t *testing.T) {
	const head = `<script>var ytInitialData = `
	body := func(inner string) string {
		return `{"contents":{"twoColumnWatchNextResults":{"conversationBar":{"liveChatRenderer":` + inner + `}}}}`
	}
	for _, tc := range []struct {
		name       string
		page       string
		wantToken  string
		wantReplay bool
		wantErr    string
		// wantErrPrefix is for the rows whose error wraps encoding/json's
		// own message, which names generated struct types and is not worth
		// pinning verbatim.
		wantErrPrefix string
	}{
		{
			name:      "reload continuation",
			page:      head + body(`{"isReplay":false,"continuations":[{"reloadContinuationData":{"continuation":"TOK"}}]}`) + `;</script>`,
			wantToken: "TOK",
		},
		{
			name:       "replay continuation",
			page:       head + body(`{"isReplay":true,"continuations":[{"liveChatReplayContinuationData":{"continuation":"RTOK"}}]}`) + `;</script>`,
			wantToken:  "RTOK",
			wantReplay: true,
		},
		{
			name:      "trailing script content, no terminator",
			page:      head + body(`{"continuations":[{"invalidationContinuationData":{"continuation":"ITOK"}}]}`) + `;var other = 1;</script>`,
			wantToken: "ITOK",
		},
		{
			name:      "window property form",
			page:      `<script>window["ytInitialData"] = ` + body(`{"continuations":[{"timedContinuationData":{"continuation":"TTOK"}}]}`) + `;</script>`,
			wantToken: "TTOK",
		},
		{
			name:    "no ytInitialData at all",
			page:    `<html><body>nothing here</body></html>`,
			wantErr: "ytInitialData not found",
		},
		{
			name:    "no liveChatRenderer",
			page:    head + `{"contents":{"twoColumnWatchNextResults":{"conversationBar":{}}}}` + `;</script>`,
			wantErr: "no liveChatRenderer found",
		},
		{
			name:    "renderer with an empty continuation list",
			page:    head + body(`{"isReplay":false,"continuations":[]}`) + `;</script>`,
			wantErr: "no continuations found",
		},
		{
			name:    "continuations carrying no token",
			page:    head + body(`{"continuations":[{"someOtherData":{"x":1}}]}`) + `;</script>`,
			wantErr: "no continuation token found",
		},
		{
			// The envelope's OWN decode errors (a duplicate `contents` whose
			// first occurrence is a number) and the renderer still comes out
			// of it, because encoding/json keeps going past the type error.
			name: "type error in the envelope, renderer decoded anyway",
			page: head + `{"contents":5,"contents":{"twoColumnWatchNextResults":{"conversationBar":{"liveChatRenderer":` +
				`{"isReplay":false,"continuations":[{"reloadContinuationData":{"continuation":"ATOK"}}]}}}}}` + `;</script>`,
			wantToken: "ATOK",
		},
		{
			// isReplay drifts to a string. The flag is lost (as it was under
			// the map walk's failed type assertion) but the TOKEN is not.
			name:      "isReplay is a string, token still returned",
			page:      head + body(`{"isReplay":"true","continuations":[{"reloadContinuationData":{"continuation":"BTOK"}}]}`) + `;</script>`,
			wantToken: "BTOK",
		},
		{
			name:      "a non-object continuation element ahead of a good one",
			page:      head + body(`{"continuations":[5,{"reloadContinuationData":{"continuation":"CTOK"}}]}`) + `;</script>`,
			wantToken: "CTOK",
		},
		{
			name:      "a numeric continuation ahead of a good one",
			page:      head + body(`{"continuations":[{"reloadContinuationData":{"continuation":5}},{"reloadContinuationData":{"continuation":"DTOK"}}]}`) + `;</script>`,
			wantToken: "DTOK",
		},
		{
			// No token to keep, so here the decode error IS the answer.
			name:          "renderer absent and the envelope decode errored",
			page:          head + `{"contents":5}` + `;</script>`,
			wantErrPrefix: "parse ytInitialData: ",
		},
		{
			// A shortDescription spelling a bare `ytInitialData = {` ahead of
			// the real assignment must not be taken for the document. The
			// forged blob carries a token so a locator that fell for it would
			// return FTOK instead of failing visibly.
			name: "forged ytInitialData in a description loses to the real one",
			page: `<script>var ytInitialPlayerResponse = {"videoDetails":{"shortDescription":` +
				`"ytInitialData = {\"contents\":{\"twoColumnWatchNextResults\":{\"conversationBar\":{\"liveChatRenderer\":{\"continuations\":[{\"reloadContinuationData\":{\"continuation\":\"FTOK\"}}]}}}}}"` +
				`}};</script>` + head + body(`{"continuations":[{"reloadContinuationData":{"continuation":"REAL"}}]}`) + `;</script>`,
			wantToken: "REAL",
		},
		{
			// The twin of TestPlayerResponseSkipsAForgedCandidate for the
			// ytInitialData locator: a page-authored `var ytInitialData = {}`
			// ahead of the real assignment matches the anchor and scans
			// cleanly, but decodes to an empty object. Taking the first match
			// returned it, and the empty envelope then read as "no
			// liveChatRenderer found" — a silent denial of chat capture.
			//
			// Mutant: a first-match locator (the pre-change
			// `ytInitialDataStartRe.FindIndex(data)`) returns `{}` here and
			// this row fails with "no liveChatRenderer found".
			name:      "a forged empty ytInitialData ahead of the real one loses",
			page:      `<p>var ytInitialData = {} </p>` + head + body(`{"continuations":[{"reloadContinuationData":{"continuation":"STOK"}}]}`) + `;</script>`,
			wantToken: "STOK",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tok, replay, err := extractChatContinuation([]byte(tc.page))
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if tc.wantErrPrefix != "" {
				if err == nil || !strings.HasPrefix(err.Error(), tc.wantErrPrefix) {
					t.Fatalf("err = %v, want one prefixed %q", err, tc.wantErrPrefix)
				}
				if tok != "" {
					t.Errorf("token = %q, want empty alongside the error", tok)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tok != tc.wantToken || replay != tc.wantReplay {
				t.Errorf("got (%q, %v), want (%q, %v)", tok, replay, tc.wantToken, tc.wantReplay)
			}
		})
	}
}

// TestPlayerResponseSkipsAForgedCandidate pins that a candidate which does
// not yield a NON-EMPTY JSON object does not end the search.
//
// First-occurrence was never a safety property: on a real watch page the
// `name="description"` and `og:description` meta tags are emitted thousands
// of bytes BEFORE `var ytInitialPlayerResponse = `, so page-authored text
// genuinely does precede the assignment. What actually limits forgery is
// that such text cannot spell a valid non-empty object — HTML attribute
// escaping turns `"` into `&quot;`, and inside a JSON string `\"` breaks the
// scan — so a forged candidate can only fail the scan, fail the decode, or
// decode to `{}`. Taking only the first occurrence turned that harmless
// inability into a denial: the real player response was never reached.
//
// The fixture spells both failure shapes in one meta attribute: `{}` decodes
// fine but is empty, and `{ x` cannot be scanned into anything that decodes.
//
// Mutant named: taking only the first match per anchor (the pre-change
// control flow, `loc := re.FindIndex(page)` with no iteration). It stops on
// the `{}` candidate and returns an empty map with ok=true, so videoDetails
// is absent and this test fails.
func TestPlayerResponseSkipsAForgedCandidate(t *testing.T) {
	page := `<!DOCTYPE html><html><head>` +
		`<meta name="description" content="var ytInitialPlayerResponse = {} var ytInitialPlayerResponse = { x">` +
		`<script nonce="q">var ytInitialPlayerResponse = ` +
		`{"videoDetails":{"videoId":"abc12345678","title":"T","author":"A","channelId":"UC1"}};` +
		`var meta=1;</script></head><body></body></html>`

	pr, ok := extractPlayerResponse([]byte(page))
	if !ok {
		t.Fatal("extractPlayerResponse found nothing — a forged candidate ahead of the real assignment ended the search")
	}
	vd, _ := pr["videoDetails"].(map[string]any)
	if got, _ := vd["videoId"].(string); got != "abc12345678" {
		t.Errorf("videoId = %q, want %q — the forged candidate was returned instead of the real player response (pr has %d keys)",
			got, "abc12345678", len(pr))
	}
}

// TestWatchPageURLCarriesTheAgeGateBypass is row #59: without yt-dlp's
// bpctr / has_verified pair (_video.py:3809) an age-restricted video's watch
// page returns the age-gate shell, and with it the ScheduledStartTime source
// and the WatchPage format tier both vanish.
//
// The whole query string is pinned, not just the presence of the two
// parameters: they are APPENDED to the URL that already worked, so a rewrite
// that reorders or drops `v=` — the only parameter YouTube actually needs —
// must fail here too.
//
// Mutant this kills: the two parameters dropped → the URL check fails.
func TestWatchPageURLCarriesTheAgeGateBypass(t *testing.T) {
	got := watchPageURL("dQw4w9WgXcQ")
	for _, want := range []string{"v=dQw4w9WgXcQ", "bpctr=9999999999", "has_verified=1"} {
		if !strings.Contains(got, want) {
			t.Errorf("watchPageURL = %q, missing %q", got, want)
		}
	}

	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("watchPageURL produced an unparseable URL %q: %v", got, err)
	}
	if want := "https://www.youtube.com/watch"; u.Scheme+"://"+u.Host+u.Path != want {
		t.Errorf("watch page endpoint = %q, want %q", u.Scheme+"://"+u.Host+u.Path, want)
	}
	if want := "v=dQw4w9WgXcQ&bpctr=9999999999&has_verified=1"; u.RawQuery != want {
		t.Errorf("watchPageURL query = %q, want exactly %q", u.RawQuery, want)
	}
}
