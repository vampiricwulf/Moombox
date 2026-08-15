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

	got := extractAttestationChallenge(page)
	if got == "" {
		t.Fatal("expected challenge, got empty")
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
		if got := extractAttestationChallenge(html); got != "" {
			t.Errorf("%s: expected empty, got %q", name, got)
		}
	}
}
