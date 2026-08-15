package sidecar

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestSidecarLivePoToken runs the FULL BotGuard mint flow against the
// real Google attestation endpoint via the embedded Node sidecar.
// SKIPS unless MOOMBOX_LIVE_BG_TEST=1 is set so it never runs by default
// (would spam Google's WAA endpoint and require network).
//
// Counterpart to internal/bgutils/botguard_live_test.go's
// TestBotGuardLiveFingerprint, but covering the sidecar path that
// (Option 2 confirmed) the goja-only path can't satisfy because of
// BotGuard's timing fingerprint check.
//
// PASS = sidecar produced a real PO token (non-empty, plausibly
// websafe-base64 encoded). FAIL = sidecar declined or errored out.
//
// Usage:
//
//	$env:MOOMBOX_LIVE_BG_TEST="1"
//	go test -v -run TestSidecarLivePoToken ./internal/bgutils/sidecar/...
func TestSidecarLivePoToken(t *testing.T) {
	if os.Getenv("MOOMBOX_LIVE_BG_TEST") != "1" {
		t.Skip("set MOOMBOX_LIVE_BG_TEST=1 to run the live sidecar fingerprint check")
	}

	s := startSidecar(t)

	// Use a fixed-but-random-enough binding so cache hits are unlikely
	// across reruns. Caching is intentional within a single run though
	// (we test that with the second mint below).
	binding := "moombox-live-test-" + time.Now().Format("20060102-150405")

	t.Logf("Step 1: minting first PO token (binding=%s)", binding)
	t1 := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	token, err := s.GeneratePoToken(ctx, binding)
	if err != nil {
		t.Fatalf("GeneratePoToken: %v", err)
	}
	dur1 := time.Since(t1)
	t.Logf("  poToken=%s (len=%d) elapsed=%v", trimMid(token, 24), len(token), dur1)

	if token == "" {
		t.Fatalf("expected non-empty PO token; got empty string")
	}
	// Real PO tokens are websafe-base64: alphanumerics + - and _ for the
	// alphabet, plus = for padding. Reject only the std-base64-only
	// characters that the websafe variant remaps (+ -> -, / -> _).
	if strings.ContainsAny(token, " /+") {
		t.Errorf("token contains non-websafe-base64 characters: %q", token)
	}
	// Real tokens are typically 100-200 chars; sub-50 likely indicates a
	// degenerate response.
	if len(token) < 50 {
		t.Errorf("token suspiciously short (%d chars); expected ~100-200", len(token))
	}

	// Second mint should hit the sidecar's internal minter cache so it
	// returns much faster (no GenerateIT round-trip).
	t.Logf("Step 2: re-minting same binding (cache check)")
	t2 := time.Now()
	token2, err := s.GeneratePoToken(ctx, binding)
	if err != nil {
		t.Fatalf("second GeneratePoToken: %v", err)
	}
	dur2 := time.Since(t2)
	t.Logf("  poToken2=%s elapsed=%v", trimMid(token2, 24), dur2)

	if dur2 > dur1/2 {
		t.Logf("WARN: second mint not noticeably faster (%v vs %v) -- minter cache may not be hot", dur2, dur1)
	}

	stats, err := s.GetStats(ctx)
	if err != nil {
		t.Logf("GetStats: %v (non-fatal)", err)
	} else {
		t.Logf("  stats: cachedMinters=%d mintsTotal=%d mintsErrored=%d",
			stats.CachedMinters, stats.MintsTotal, stats.MintsErrored)
		if stats.CachedMinters != 1 {
			t.Errorf("expected CachedMinters=1 after first mint; got %d", stats.CachedMinters)
		}
		if stats.MintsTotal < 2 {
			t.Errorf("expected MintsTotal>=2 after two mints; got %d", stats.MintsTotal)
		}
	}

	t.Logf("SUCCESS: sidecar minted a real PO token via Google's WAA endpoint")
}

// TestSidecarLiveGvsMint exercises the freshMinter + provenance path against
// real YouTube endpoints. No page challenge is supplied (none is available in
// a test), so provenance must report the /att/get source and a fresh minter.
func TestSidecarLiveGvsMint(t *testing.T) {
	if os.Getenv("MOOMBOX_LIVE_BG_TEST") != "1" {
		t.Skip("set MOOMBOX_LIVE_BG_TEST=1 to run live BotGuard tests")
	}
	s := startSidecar(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	res, err := s.GenerateGvsPoToken(ctx, "dQw4w9WgXcQ", "")
	if err != nil {
		t.Fatalf("GenerateGvsPoToken: %v", err)
	}
	if res.PoToken == "" || res.MinterSource != "att_get" || !res.MinterFresh {
		t.Errorf("unexpected result: %+v", res)
	}
	// Second fresh mint must regenerate again (freshMinter honored).
	res2, err := s.GenerateGvsPoToken(ctx, "dQw4w9WgXcQ", "")
	if err != nil {
		t.Fatalf("second GenerateGvsPoToken: %v", err)
	}
	if !res2.MinterFresh {
		t.Error("second GVS mint reused the cached minter; freshMinter not honored")
	}
}

// trimMid abbreviates a long token to "<head>...<tail>" for log-friendly
// output without leaking the full credential.
func trimMid(s string, head int) string {
	if len(s) <= head*2+3 {
		return s
	}
	return s[:head] + "..." + s[len(s)-head:]
}
