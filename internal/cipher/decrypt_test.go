package cipher

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// These tests exercise the routing/nil-handling logic in RoutedResolveURL and
// RoutedDecryptNInURL. They rely on staticSolver (defined in
// solver_composite_test.go) which is package-scoped.

func TestRoutedResolveURL_NilRoutedSolverFallsThroughWhenGojaNil(t *testing.T) {
	// Both nil — no way to resolve. Should error cleanly.
	_, err := RoutedResolveURL(context.Background(), nil, nil, ResolveURLRequest{
		PlayerURL:          "https://www.youtube.com/s/player/cb017549/player_ias.vflset/en_US/base.js",
		StreamURL:          "https://example.com/stream",
		EncryptedSignature: "ABCDEF",
		SignatureKey:       "sig",
	})
	if err == nil {
		t.Fatal("expected error when both routedSolver and gojaResolver are nil")
	}
}

func TestRoutedResolveURL_RoutedSigFailReturnsErrorWhenGojaNil(t *testing.T) {
	// Routed solver fails; goja is nil. Should error cleanly, not panic.
	routed := &staticSolver{sigErr: errors.New("sidecar boom")}
	_, err := RoutedResolveURL(context.Background(), routed, nil, ResolveURLRequest{
		PlayerURL:          "https://www.youtube.com/s/player/cb017549/player_ias.vflset/en_US/base.js",
		StreamURL:          "https://example.com/stream",
		EncryptedSignature: "ABCDEF",
		SignatureKey:       "sig",
	})
	if err == nil {
		t.Fatal("expected error when routed sig fails and goja fallback is nil")
	}
	if !strings.Contains(err.Error(), "no goja fallback") {
		t.Errorf("expected error to mention 'no goja fallback'; got: %v", err)
	}
}

func TestRoutedResolveURL_NoSigUsesRoutedNOnly(t *testing.T) {
	// No EncryptedSignature; URL has only an n-param. Routed solver
	// handles n; goja stays unused.
	routed := &staticSolver{n: map[string]string{"raw-n": "decoded-n"}}
	res, err := RoutedResolveURL(context.Background(), routed, nil, ResolveURLRequest{
		PlayerURL: "https://www.youtube.com/s/player/cb017549/player_ias.vflset/en_US/base.js",
		StreamURL: "https://example.com/videoplayback?n=raw-n",
	})
	if err != nil {
		t.Fatalf("RoutedResolveURL: %v", err)
	}
	if !strings.Contains(res.URL, "n=decoded-n") {
		t.Errorf("expected n-param replaced; got %q", res.URL)
	}
	if strings.Contains(res.URL, "n=raw-n") {
		t.Errorf("raw n still present: %q", res.URL)
	}
}

func TestRoutedDecryptNInURL_RoutedSolverHandlesN(t *testing.T) {
	routed := &staticSolver{n: map[string]string{"abc123": "def456"}}
	out := RoutedDecryptNInURL(context.Background(), routed, nil,
		"https://www.youtube.com/s/player/cb017549/player_ias.vflset/en_US/base.js",
		"https://example.com/videoplayback?n=abc123&other=foo")
	if !strings.Contains(out, "n=def456") {
		t.Errorf("expected routed n decryption; got %q", out)
	}
}

func TestRoutedDecryptNInURL_NoNParamPassesThrough(t *testing.T) {
	routed := &staticSolver{}
	raw := "https://example.com/videoplayback?other=foo"
	out := RoutedDecryptNInURL(context.Background(), routed, nil,
		"https://www.youtube.com/s/player/cb017549/player_ias.vflset/en_US/base.js",
		raw)
	if out != raw {
		t.Errorf("expected URL unchanged when no n-param; got %q vs %q", out, raw)
	}
}

func TestRoutedDecryptNInURL_BothNilReturnsRawURL(t *testing.T) {
	raw := "https://example.com/videoplayback?n=abc123"
	out := RoutedDecryptNInURL(context.Background(), nil, nil,
		"https://www.youtube.com/s/player/cb017549/player_ias.vflset/en_US/base.js",
		raw)
	// No solver means no decryption — URL is returned unchanged.
	if out != raw {
		t.Errorf("expected raw URL when both solvers nil; got %q", out)
	}
}
