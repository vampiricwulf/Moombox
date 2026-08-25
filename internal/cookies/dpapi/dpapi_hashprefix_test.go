package dpapi

import (
	"crypto/rand"
	"crypto/sha256"
	"strings"
	"testing"
	"unicode/utf8"
)

// domainHash builds the 32-byte SHA-256-of-domain prefix Chrome 130+
// (Cookies DB meta.version >= 24) prepends to a cookie's plaintext inside
// the encrypted blob.
func domainHash(domain string) []byte {
	sum := sha256.Sum256([]byte(domain))
	return sum[:]
}

// newMasterKey returns a random 32-byte AES-GCM key.
func newMasterKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key
}

// TestDecryptV10Cookie_HashPrefixGate exercises BOTH sides of the
// meta.version gate. The gate-off case deliberately uses a plaintext longer
// than 32 bytes: with a short one, an implementation that stripped
// unconditionally would error out and look "correct", whereas here it would
// silently return the tail and fail the assertion.
func TestDecryptV10Cookie_HashPrefixGate(t *testing.T) {
	const longValue = "SID=g.a000abcdefghijklmnopqrstuvwxyz0123456789"
	if len(longValue) <= chromeDomainHashLen {
		t.Fatalf("fixture must exceed %d bytes to catch unconditional stripping", chromeDomainHashLen)
	}

	tests := []struct {
		name       string
		plaintext  []byte
		hashPrefix bool
		want       string
	}{
		{
			name:       "meta.version >= 24 strips the domain hash",
			plaintext:  append(domainHash(".youtube.com"), []byte(longValue)...),
			hashPrefix: true,
			want:       longValue,
		},
		{
			name:       "meta.version < 24 returns the value whole",
			plaintext:  []byte(longValue),
			hashPrefix: false,
			want:       longValue,
		},
		{
			name:       "meta.version < 24 short value untouched",
			plaintext:  []byte("YSC=abc"),
			hashPrefix: false,
			want:       "YSC=abc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			masterKey := newMasterKey(t)
			encrypted := buildV10(t, masterKey, "v10", tt.plaintext)

			got, err := decryptV10Cookie(masterKey, encrypted, tt.hashPrefix)
			if err != nil {
				t.Fatalf("decryptV10Cookie: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDecryptV10Cookie_HashPrefixExactly32Bytes covers the legitimate
// empty-value cookie: the plaintext is nothing but the hash prefix, so the
// value is the empty string and that is NOT an error.
func TestDecryptV10Cookie_HashPrefixExactly32Bytes(t *testing.T) {
	masterKey := newMasterKey(t)
	encrypted := buildV10(t, masterKey, "v10", domainHash(".youtube.com"))

	got, err := decryptV10Cookie(masterKey, encrypted, true)
	if err != nil {
		t.Fatalf("decryptV10Cookie: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

// TestDecryptV10Cookie_HashPrefixShorterThanPrefix guards the truncation
// hazard: a plaintext shorter than the 32-byte prefix cannot legitimately
// carry one, so it must error out (the caller skips the row) rather than
// slicing into nothing and reporting an empty cookie value as success.
func TestDecryptV10Cookie_HashPrefixShorterThanPrefix(t *testing.T) {
	masterKey := newMasterKey(t)
	encrypted := buildV10(t, masterKey, "v10", []byte("short"))

	_, err := decryptV10Cookie(masterKey, encrypted, true)
	if err == nil {
		t.Fatal("expected error for plaintext shorter than the 32-byte hash prefix")
	}
	if !strings.Contains(err.Error(), "hash prefix") {
		t.Errorf("error should mention the hash prefix, got: %v", err)
	}
}

// TestDecryptV10Cookie_RejectsNonUTF8 mirrors upstream's UnicodeDecodeError
// branch: a plaintext that is not valid UTF-8 is reported as a failure so
// the row is skipped, instead of being written into cookies.txt as binary
// garbage.
func TestDecryptV10Cookie_RejectsNonUTF8(t *testing.T) {
	masterKey := newMasterKey(t)
	encrypted := buildV10(t, masterKey, "v10", []byte{'a', 0xff, 0xfe, 'b'})

	_, err := decryptV10Cookie(masterKey, encrypted, false)
	if err == nil {
		t.Fatal("expected error for non-UTF-8 plaintext")
	}
	if !strings.Contains(err.Error(), "UTF-8") {
		t.Errorf("error should mention UTF-8, got: %v", err)
	}
}

// TestDecryptV10Cookie_UnstrippedHashFailsLoudly pins the safety net for a
// failed meta.version probe on a Chrome 130+ profile: the gate stays off,
// the SHA-256 bytes stay at the head of the plaintext, and the UTF-8 check
// rejects the row. The operator gets an empty extraction with a clear
// per-row error instead of a cookies.txt full of binary garbage.
func TestDecryptV10Cookie_UnstrippedHashFailsLoudly(t *testing.T) {
	plaintext := append(domainHash(".youtube.com"), []byte("SID=g.a000abcdef")...)
	if utf8.Valid(plaintext) {
		t.Skip("this domain's digest happens to be valid UTF-8; the loud-failure net does not apply")
	}

	masterKey := newMasterKey(t)
	encrypted := buildV10(t, masterKey, "v10", plaintext)

	_, err := decryptV10Cookie(masterKey, encrypted, false)
	if err == nil {
		t.Fatal("expected an un-stripped domain hash to be rejected, not returned as a cookie value")
	}
	if !strings.Contains(err.Error(), "UTF-8") {
		t.Errorf("error should mention UTF-8, got: %v", err)
	}
}
