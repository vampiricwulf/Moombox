package youtube

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

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

	got, reason := extractAttestationChallenge(page)
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
		if got, _ := extractAttestationChallenge(html); got != "" {
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

	got, reason := extractAttestationChallenge(page)
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

	got, reason := extractAttestationChallenge(page)
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

	if got, reason := extractAttestationChallenge(`<script>window.ytAtN({R: "unclosed`); got != "" || reason != atnUnbalanced {
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

	got, reason := extractAttestationChallenge(page)
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
