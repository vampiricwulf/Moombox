package cipher

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeRoutedSolver is a minimal cipher.Solver for ResolveFormatURL tests.
// SigFn / NFn return canned values; nil functions surface an error.
type fakeRoutedSolver struct {
	SigFn func(string) (string, error)
	NFn   func(string) (string, error)
}

func (f *fakeRoutedSolver) Sig(_ context.Context, _, encrypted string) (string, error) {
	if f.SigFn == nil {
		return "", errors.New("sig not configured")
	}
	return f.SigFn(encrypted)
}
func (f *fakeRoutedSolver) N(_ context.Context, _, encrypted string) (string, error) {
	if f.NFn == nil {
		return "", errors.New("n not configured")
	}
	return f.NFn(encrypted)
}
func (f *fakeRoutedSolver) Batch(_ context.Context, _ string, sigs, ns []string) (map[string]string, map[string]string, error) {
	sigOut := map[string]string{}
	for _, s := range sigs {
		out, err := f.SigFn(s)
		if err != nil {
			return nil, nil, err
		}
		sigOut[s] = out
	}
	nOut := map[string]string{}
	for _, n := range ns {
		out, err := f.NFn(n)
		if err != nil {
			return nil, nil, err
		}
		nOut[n] = out
	}
	return sigOut, nOut, nil
}

const testPlayerURLForFormat = "https://www.youtube.com/s/player/abcdef12/player_ias.vflset/en_US/base.js"

// TestResolveFormatURL_SigCipherEntry — sig solver fires, decrypted sig
// is appended as &signature=, n is decrypted in place.
func TestResolveFormatURL_SigCipherEntry(t *testing.T) {
	routed := &fakeRoutedSolver{
		SigFn: func(in string) (string, error) {
			if in != "ENC_SIG" {
				return "", errors.New("unexpected sig input")
			}
			return "DEC_SIG", nil
		},
		NFn: func(in string) (string, error) {
			if in != "ENC_N" {
				return "", errors.New("unexpected n input")
			}
			return "DEC_N", nil
		},
	}

	got, err := ResolveFormatURL(context.Background(), routed, nil, testPlayerURLForFormat, ResolveFormatRequest{
		URL:          "https://r1---sn-test.googlevideo.com/videoplayback?expire=123&n=ENC_N&itag=137",
		EncryptedSig: "ENC_SIG",
		SigKey:       "signature",
	})
	if err != nil {
		t.Fatalf("ResolveFormatURL: %v", err)
	}

	if !strings.Contains(got, "signature=DEC_SIG") {
		t.Errorf("missing decrypted signature in URL: %q", got)
	}
	if !strings.Contains(got, "n=DEC_N") {
		t.Errorf("missing decrypted n in URL: %q", got)
	}
	if strings.Contains(got, "ENC_SIG") {
		t.Errorf("URL still contains encrypted sig: %q", got)
	}
}

// TestResolveFormatURL_DirectURL_OnlyDecryptsN — direct URL (no
// EncryptedSig) skips sig solving and only runs n decryption.
func TestResolveFormatURL_DirectURL_OnlyDecryptsN(t *testing.T) {
	sigCalled := false
	routed := &fakeRoutedSolver{
		SigFn: func(in string) (string, error) {
			sigCalled = true
			return "should-not-fire", nil
		},
		NFn: func(in string) (string, error) {
			return "DEC_" + in, nil
		},
	}

	got, err := ResolveFormatURL(context.Background(), routed, nil, testPlayerURLForFormat, ResolveFormatRequest{
		URL: "https://r1---sn-test.googlevideo.com/videoplayback?expire=123&n=ENC&itag=137",
	})
	if err != nil {
		t.Fatalf("ResolveFormatURL: %v", err)
	}

	if sigCalled {
		t.Errorf("sig solver should NOT have been called for direct URL")
	}
	if !strings.Contains(got, "n=DEC_ENC") {
		t.Errorf("expected decrypted n in URL: %q", got)
	}
}

// TestResolveFormatURL_DefaultsSigKey — when SigKey is empty, falls
// back to "signature" (matches the legacy decryptSignatureCipher
// default).
func TestResolveFormatURL_DefaultsSigKey(t *testing.T) {
	routed := &fakeRoutedSolver{
		SigFn: func(in string) (string, error) { return "DEC", nil },
		NFn:   func(in string) (string, error) { return in, nil }, // pass-through
	}

	got, err := ResolveFormatURL(context.Background(), routed, nil, testPlayerURLForFormat, ResolveFormatRequest{
		URL:          "https://r1---sn-test.googlevideo.com/videoplayback?expire=123",
		EncryptedSig: "ENC",
		// SigKey omitted
	})
	if err != nil {
		t.Fatalf("ResolveFormatURL: %v", err)
	}

	if !strings.Contains(got, "signature=DEC") {
		t.Errorf("default SigKey should be 'signature'; got URL %q", got)
	}
}

// TestResolveFormatURL_EmptyURL — programmer-error guard.
func TestResolveFormatURL_EmptyURL(t *testing.T) {
	_, err := ResolveFormatURL(context.Background(), nil, nil, testPlayerURLForFormat, ResolveFormatRequest{})
	if err == nil {
		t.Fatal("expected error on empty URL")
	}
	if !strings.Contains(err.Error(), "empty URL") {
		t.Errorf("error should mention empty URL; got: %v", err)
	}
}
