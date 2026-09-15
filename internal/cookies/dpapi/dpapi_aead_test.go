package dpapi

import (
	"crypto/rand"
	"testing"
)

// TestDecryptV10CookieWithReusesOneAEAD pins the shape the per-profile reader
// depends on: one AEAD, many rows, identical results.
//
// Mutant: making decryptV10CookieWith build its own AEAD from a captured key
// (or reverting ReadChromeCookiesStats to call decryptV10Cookie per row) makes
// this fail to compile — which is the point: the shared-AEAD entry point has to
// exist for the reader to have something to hoist.
func TestDecryptV10CookieWithReusesOneAEAD(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	gcm, err := newCookieAEAD(key)
	if err != nil {
		t.Fatalf("newCookieAEAD: %v", err)
	}

	for _, want := range []string{"SID=one", "SID=two", "SID=three"} {
		encrypted := buildV10(t, key, "v10", []byte(want))
		got, err := decryptV10CookieWith(gcm, encrypted, false)
		if err != nil {
			t.Fatalf("decryptV10CookieWith(%q): %v", want, err)
		}
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

// TestNewCookieAEADRejectsABadKeyLength pins the one behaviour that moves: an
// unusable key is now a single failure the caller reports, not one skipped row
// per cookie.
//
// Mutant: dropping the aes.NewCipher error wrap leaves a nil AEAD and a nil
// error, and the first Open panics instead.
func TestNewCookieAEADRejectsABadKeyLength(t *testing.T) {
	if _, err := newCookieAEAD(make([]byte, 17)); err == nil {
		t.Fatal("a 17-byte key is not an AES key — want an error")
	}
}
