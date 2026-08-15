package youtube

import (
	"encoding/json"
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

// TestIsGoogleOwnedHost pins the allowlist semantics, including the
// lookalike-domain cases the dot anchoring exists to defeat and the
// user-content hosts deliberately excluded (their bytes are third-party
// controlled, and the fetched body is executed).
func TestIsGoogleOwnedHost(t *testing.T) {
	for _, h := range []string{
		"www.google.com", "google.com", "www.gstatic.com", "s.ytimg.com",
		"www.youtube.com", "google.de", "www.google.co.uk", "google.com.au",
		"jnn-pa.googleapis.com",
	} {
		if !isGoogleOwnedHost(h) {
			t.Errorf("%s should be allowed", h)
		}
	}
	for _, h := range []string{
		"evil.tld", "evilgoogle.com", "google.com.evil.tld", "notgstatic.com",
		"i.ytimg.com",                // user-uploaded thumbnails
		"lh3.googleusercontent.com",  // user content
		"yt3.ggpht.com",              // user avatars
		"rr2---sn-x.googlevideo.com", // user media
		"", "google.co.uk.evil.tld",
	} {
		if isGoogleOwnedHost(h) {
			t.Errorf("%s should be rejected", h)
		}
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
