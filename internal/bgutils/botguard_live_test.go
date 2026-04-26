package bgutils

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestBotGuardLiveFingerprint runs the full PO-token generation flow against
// the real Google attestation endpoint. The test SKIPS unless
// MOOMBOX_LIVE_BG_TEST=1 is set in the environment, so it never runs by
// default (which would spam Google's WAA endpoint and require network).
//
// Owner enabled this knob during the BotGuard fingerprint investigation to
// validate test.49's prototype-chain fix end-to-end:
//
//	$env:MOOMBOX_LIVE_BG_TEST="1"; go test -v -run TestBotGuardLive ./internal/bgutils/...
//
// PASS = real integrity token (non-empty IntegrityToken, BotGuard accepted
// our environment). FAIL = websafe-only fallback (BotGuard declined).
func TestBotGuardLiveFingerprint(t *testing.T) {
	if os.Getenv("MOOMBOX_LIVE_BG_TEST") != "1" {
		t.Skip("set MOOMBOX_LIVE_BG_TEST=1 to run the live BotGuard fingerprint check")
	}

	logger := &testBGLogger{t: t}
	cfg := &BgConfig{
		RequestKey: DefaultRequestKey,
		// CacheDir intentionally empty -- the interpreter-hash cache was
		// disabled in test.41; we always fetch the full interpreter.
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	t.Log("Step 1: fetching BotGuard challenge from Google WAA...")
	challenge, err := FetchChallenge(ctx, cfg, logger)
	if err != nil {
		t.Fatalf("FetchChallenge: %v", err)
	}
	t.Logf("  globalName=%s programLen=%d interpreterScriptLen=%d",
		challenge.GlobalName, len(challenge.Program), len(challenge.InterpreterScript))

	t.Log("Step 2: creating BotGuard client (runs interpreter under our shim)...")
	client, err := NewBotGuardClient(ctx, challenge, logger)
	if err != nil {
		t.Fatalf("NewBotGuardClient: %v", err)
	}
	defer client.Shutdown()
	t.Log("  client created")

	t.Log("Step 3: taking snapshot...")
	response, webPoSignalOutput, err := client.Snapshot(ctx, SnapshotDefaultTimeout)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	wpoLen := webPoSignalOutput.Get("length")
	wpoLenN := int64(0)
	if wpoLen != nil {
		wpoLenN = wpoLen.ToInteger()
	}
	has0 := webPoSignalOutput.Get("0") != nil &&
		webPoSignalOutput.Get("0").String() != "undefined"

	t.Logf("  response=%d bytes; webPoSignalOutput.length=%d has[0]=%v",
		len(response), wpoLenN, has0)

	t.Log("Step 4: posting BotGuard response to GenerateIT...")
	itData, err := generateIntegrityToken(ctx, cfg, response)
	if err != nil {
		t.Fatalf("generateIntegrityToken: %v", err)
	}

	t.Logf("  IntegrityToken=%q (len=%d)", trunc(itData.IntegrityToken, 24), len(itData.IntegrityToken))
	t.Logf("  WebsafeFallbackToken=%q (len=%d)", trunc(itData.WebsafeFallbackToken, 24), len(itData.WebsafeFallbackToken))
	t.Logf("  EstimatedTTL=%v", itData.EstimatedTTL)

	// PASS/FAIL judgment
	if itData.IntegrityToken == "" {
		t.Errorf(
			"BotGuard declined our environment -- IntegrityToken empty, only websafe fallback returned. "+
				"webPoSignalOutput populated: %v. Fingerprint shim still doesn't pass Google's check. "+
				"Inspect the snapshot response prefix for clues: %q",
			has0, trunc(response, 120),
		)
		return
	}
	t.Logf("SUCCESS: BotGuard accepted our shim. webPoSignalOutput populated: %v", has0)
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// testBGLogger routes bgutils internal Debug/Info/Warn/Error through t.Logf
// so the live test surfaces every step in -v output.
type testBGLogger struct{ t *testing.T }

func (l *testBGLogger) Debug(msg string, args ...any) { l.t.Logf("[DEBUG] "+msg+" %v", args) }
func (l *testBGLogger) Info(msg string, args ...any)  { l.t.Logf("[INFO ] "+msg+" %v", args) }
func (l *testBGLogger) Warn(msg string, args ...any)  { l.t.Logf("[WARN ] "+msg+" %v", args) }
func (l *testBGLogger) Error(msg string, args ...any) { l.t.Logf("[ERROR] "+msg+" %v", args) }
