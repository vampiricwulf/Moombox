package youtube

import "testing"

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
