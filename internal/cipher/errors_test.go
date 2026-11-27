package cipher

import (
	"context"
	"errors"
	"testing"
)

// TestSentinelsAreDistinct sanity-checks that each sentinel only matches
// itself — guards against accidental aliasing if a future refactor
// collapses two sentinels into one.
func TestCipherSentinelsAreDistinct(t *testing.T) {
	all := []error{
		ErrExtractorMismatch,
		ErrPlayerJSFetch,
		ErrSigDecrypt,
		ErrNDecrypt,
		ErrInputRequired,
	}
	for i, a := range all {
		for j, b := range all {
			if i == j {
				continue
			}
			if errors.Is(a, b) {
				t.Errorf("sentinel aliasing: %v should not match %v", a, b)
			}
		}
	}
}

// TestDecryptSignatureMissingPlayerURL covers the empty-input branch
// at decrypt.go — must wrap ErrInputRequired so callers can match.
func TestDecryptSignatureMissingPlayerURL(t *testing.T) {
	s := &GojaResolver{}
	_, err := s.DecryptSignature(context.Background(), SignatureRequest{
		PlayerURL: "",
	})
	if err == nil {
		t.Fatal("DecryptSignature with empty URL: want error, got nil")
	}
	if !errors.Is(err, ErrInputRequired) {
		t.Errorf("want errors.Is(err, ErrInputRequired), got %v", err)
	}
}
