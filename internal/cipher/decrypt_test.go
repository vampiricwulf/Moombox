package cipher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// unreachablePlayerURL makes the goja fallback fail fast and offline: its
// player fetch goes to a loopback port nothing listens on, which the
// transport refuses at once (loopback is never proxied).
const unreachablePlayerURL = "http://127.0.0.1:1/s/player/cb017549/player_ias.vflset/en_US/base.js"

// newFailingGojaResolver is a real GojaResolver whose every solve fails at the
// player fetch — the shape of a goja fallback that cannot help.
func newFailingGojaResolver(t *testing.T) *GojaResolver {
	t.Helper()
	g, err := NewGojaResolver(t.TempDir(), &testLogger{})
	if err != nil {
		t.Fatalf("NewGojaResolver: %v", err)
	}
	return g
}

// captureSlog routes slog.Default through a buffer for the test. The routed
// n-param helpers report a degrade only through slog (they return a URL,
// never an error), so the log line is the one place the cause can show.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestRoutedResolveURL_SigFailureKeepsTheRoutedCauseWhenGojaFailsToo: the
// routed sig error used to be dropped on the floor before the goja fallback
// ran, so the error a caller saw was goja's alone — "no sig solver available
// for player X" — whether the sidecar was dead, the player was stale or ejs
// had found no solution. Both causes now travel, and the routed one stays the
// one errors.Is matches.
//
// Mutants this kills:
//   - returning gojaResolver.ResolveURL's error as before → errors.Is fails,
//     "no solutions" missing
//   - %w on the goja error instead                        → errors.Is fails
func TestRoutedResolveURL_SigFailureKeepsTheRoutedCauseWhenGojaFailsToo(t *testing.T) {
	routed := &staticSolver{sigErr: fmt.Errorf("%w: ejs solve sig: no solutions", ErrPlayerJSStale)}
	_, err := RoutedResolveURL(context.Background(), routed, newFailingGojaResolver(t), ResolveURLRequest{
		PlayerURL:          unreachablePlayerURL,
		StreamURL:          "https://example.com/stream",
		EncryptedSignature: "ABCDEF",
		SignatureKey:       "sig",
	})
	if err == nil {
		t.Fatal("expected an error when the routed sig and the goja fallback both fail")
	}
	if !errors.Is(err, ErrPlayerJSStale) {
		t.Errorf("errors.Is(err, ErrPlayerJSStale) is false; the routed cause must stay the wrapped one: %v", err)
	}
	for _, want := range []string{"no solutions", "goja fallback: "} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestRoutedDecryptNInURL_WarnNamesEveryFailedPath: decryptFn swallowed the
// routed N error entirely, so the Warn its callers log read "n decrypt
// unavailable for player X" for every failure alike. The routed cause is now
// logged at Debug as it happens and carried into the error the Warn shows,
// with the goja outcome (or its absence) beside it.
//
// Mutants this kills:
//   - the routed error dropped again   → "routed n: " / "no solutions" missing
//   - the goja outcome not carried     → "goja fallback: " missing
//   - the nil-goja shape misreported   → "(no goja fallback)" missing
func TestRoutedDecryptNInURL_WarnNamesEveryFailedPath(t *testing.T) {
	logs := captureSlog(t)
	routed := &staticSolver{nErr: fmt.Errorf("%w: ejs solve n: no solutions", ErrPlayerJSStale)}
	const raw = "https://example.com/videoplayback?n=abc123"

	if out := RoutedDecryptNInURL(context.Background(), routed, newFailingGojaResolver(t), unreachablePlayerURL, raw); out != raw {
		t.Errorf("URL changed although every solver failed: %q", out)
	}
	for _, want := range []string{
		"routed n-param decryption failed",              // the Debug line, as it happens
		"routed n: ", "no solutions", "goja fallback: ", // the Warn's error
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log output does not mention %q:\n%s", want, logs.String())
		}
	}

	logs.Reset()
	if out := RoutedDecryptNInURL(context.Background(), routed, nil, unreachablePlayerURL, raw); out != raw {
		t.Errorf("URL changed although the routed solver failed and there is no goja: %q", out)
	}
	for _, want := range []string{"routed n: ", "no solutions", "(no goja fallback)"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log output without goja does not mention %q:\n%s", want, logs.String())
		}
	}
}
