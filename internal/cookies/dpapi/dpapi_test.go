package dpapi

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"strings"
	"testing"
)

// TestDecryptV10Cookie_RoundTrip generates a fresh AES-GCM ciphertext
// in the layout Chrome writes — "v10" || nonce(12) || ct || tag(16) —
// and asserts decryptV10Cookie recovers the plaintext.
func TestDecryptV10Cookie_RoundTrip(t *testing.T) {
	masterKey := make([]byte, 32)
	if _, err := rand.Read(masterKey); err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("MyCookieValue=hello%20world")
	encrypted := buildV10(t, masterKey, "v10", plaintext)

	got, err := decryptV10Cookie(masterKey, encrypted)
	if err != nil {
		t.Fatalf("decryptV10Cookie: %v", err)
	}
	if got != string(plaintext) {
		t.Errorf("got %q, want %q", got, plaintext)
	}
}

// TestDecryptV10Cookie_V11Variant covers Chrome's v11 prefix (used on
// Linux desktop-keystore configurations and occasionally on Windows
// Edge). Same crypto, different prefix tag.
func TestDecryptV10Cookie_V11Variant(t *testing.T) {
	masterKey := make([]byte, 32)
	if _, err := rand.Read(masterKey); err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("session=abc")
	encrypted := buildV10(t, masterKey, "v11", plaintext)

	got, err := decryptV10Cookie(masterKey, encrypted)
	if err != nil {
		t.Fatalf("decryptV10Cookie v11: %v", err)
	}
	if got != string(plaintext) {
		t.Errorf("got %q, want %q", got, plaintext)
	}
}

// TestDecryptV10Cookie_Empty returns empty string + nil for an empty
// input — matches Chrome's session-cookie sentinel for "no value yet".
func TestDecryptV10Cookie_Empty(t *testing.T) {
	got, err := decryptV10Cookie(make([]byte, 32), nil)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

// TestDecryptV10Cookie_LegacyPrefix confirms the function rejects
// pre-v10 DPAPI blobs with a clear error rather than mis-decrypting.
func TestDecryptV10Cookie_LegacyPrefix(t *testing.T) {
	masterKey := make([]byte, 32)
	// 16 bytes that happen not to start with "v10" or "v11"
	encrypted := []byte("\x00\x01legacy_dpapi_blob_data_here")
	_, err := decryptV10Cookie(masterKey, encrypted)
	if err == nil {
		t.Fatal("expected error for legacy DPAPI prefix")
	}
	if !strings.Contains(err.Error(), "v10/v11 prefix") {
		t.Errorf("error message should mention v10/v11 prefix, got: %v", err)
	}
}

// TestDecryptV10Cookie_TooShort guards the length check.
func TestDecryptV10Cookie_TooShort(t *testing.T) {
	masterKey := make([]byte, 32)
	// "v10" + only 5 bytes — too short to even contain a nonce.
	encrypted := []byte("v10short")
	_, err := decryptV10Cookie(masterKey, encrypted)
	if err == nil {
		t.Fatal("expected error for too-short ciphertext")
	}
	if !strings.Contains(err.Error(), "too short") {
		t.Errorf("error message should mention 'too short', got: %v", err)
	}
}

// TestDecryptV10Cookie_BadMasterKey confirms a key mismatch surfaces
// as an AES-GCM open error (auth tag check fails). This is the row
// the live ReadChromeCookies path silently skips.
func TestDecryptV10Cookie_BadMasterKey(t *testing.T) {
	rightKey := make([]byte, 32)
	wrongKey := make([]byte, 32)
	if _, err := rand.Read(rightKey); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(wrongKey); err != nil {
		t.Fatal(err)
	}
	encrypted := buildV10(t, rightKey, "v10", []byte("secret"))
	if _, err := decryptV10Cookie(wrongKey, encrypted); err == nil {
		t.Fatal("expected error for wrong master key")
	}
}

// TestChromeEpochToUnix covers the boundary cases.
func TestChromeEpochToUnix(t *testing.T) {
	tests := []struct {
		name string
		in   int64
		want int64
	}{
		{"session cookie sentinel", 0, 0},
		// 1970-01-01 in chrome epoch microseconds = 11644473600 * 1e6
		{"unix epoch boundary", 11644473600 * 1_000_000, 0},
		// 2025-01-01 00:00:00 UTC = 1735689600 unix
		{"2025-01-01", (11644473600 + 1735689600) * 1_000_000, 1735689600},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := chromeEpochToUnix(tt.in); got != tt.want {
				t.Errorf("chromeEpochToUnix(%d) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

// TestChromeSameSiteString covers all four Chrome enum values plus an
// out-of-range default to guard against future enum extensions.
func TestChromeSameSiteString(t *testing.T) {
	tests := []struct {
		in   int
		want string
	}{
		{-1, "Unspecified"},
		{0, "None"},
		{1, "Lax"},
		{2, "Strict"},
		{3, "Unspecified"},
		{99, "Unspecified"},
	}
	for _, tt := range tests {
		if got := chromeSameSiteString(tt.in); got != tt.want {
			t.Errorf("chromeSameSiteString(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// buildV10 constructs a ciphertext in the Chrome v10/v11 cookie
// layout: prefix + 12-byte nonce + AES-GCM ciphertext-with-tag.
func buildV10(t *testing.T, masterKey []byte, prefix string, plaintext []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	ct := gcm.Seal(nil, nonce, plaintext, nil)
	out := make([]byte, 0, len(prefix)+len(nonce)+len(ct))
	out = append(out, prefix...)
	out = append(out, nonce...)
	out = append(out, ct...)
	return out
}
