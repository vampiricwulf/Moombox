package youtube

import (
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
