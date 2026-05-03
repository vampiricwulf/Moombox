package bgutils

import (
	"context"
	"os"
	"testing"
	"time"

	bgembed "github.com/vampiricwulf/Moombox/internal/bgutils/embed"
	"github.com/vampiricwulf/Moombox/internal/bgutils/sidecar"
)

// silentLogger drops all log calls. Used by fallback tests where the
// sidecar warning noise during graceful-degradation isn't useful output.
type silentLogger struct{ t *testing.T }

func (l silentLogger) Debug(msg string, args ...any) { l.t.Logf("[DEBUG] "+msg+" %v", args) }
func (l silentLogger) Info(msg string, args ...any)  { l.t.Logf("[INFO ] "+msg+" %v", args) }
func (l silentLogger) Warn(msg string, args ...any)  { l.t.Logf("[WARN ] "+msg+" %v", args) }
func (l silentLogger) Error(msg string, args ...any) { l.t.Logf("[ERROR] "+msg+" %v", args) }

// TestSidecarFallsBackOnDeath verifies that when the sidecar process dies
// mid-flight, PotProvider's generateAndMint detects the unhealthy state
// and proceeds to its goja fallback path rather than hanging or
// surfacing a stale "sidecar unhealthy" error to the caller forever.
//
// Gated on MOOMBOX_LIVE_BG_TEST=1 because:
//   - Sidecar startup needs the real embed blobs (built by Phase 1+2).
//   - The first mint hits Google's WAA endpoint to validate that the
//     sidecar actually serves successful mints before we kill it.
//
// The post-kill mint is allowed to error (the goja path probably can't
// produce a real integrity token in this test rig); what we assert is
// that the call returns within a reasonable timeout, AND that the
// sidecar pointer is still attached but reports unhealthy.
func TestSidecarFallsBackOnDeath(t *testing.T) {
	if os.Getenv("MOOMBOX_LIVE_BG_TEST") != "1" {
		t.Skip("set MOOMBOX_LIVE_BG_TEST=1 to run the sidecar fallback live test")
	}
	if len(bgembed.EmbeddedNode) == 0 || len(bgembed.SidecarTarGz) == 0 {
		t.Skip("sidecar embed blobs missing")
	}

	log := silentLogger{t: t}
	pp := NewPotProvider(&BgConfig{}, log)

	// Start a real sidecar pointed at this test's TempDir so we don't
	// touch the real %LOCALAPPDATA% extraction.
	sc := sidecar.New(sidecar.Config{
		CacheDir:       t.TempDir(),
		StartupTimeout: 30 * time.Second,
		RequestTimeout: 60 * time.Second,
		Logger:         log,
	})
	startCtx, startCancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer startCancel()
	if err := sc.Start(startCtx); err != nil {
		t.Fatalf("sidecar Start: %v", err)
	}
	t.Cleanup(func() { _ = sc.Stop() })

	pp.SetSidecar(sc)

	// First mint: should succeed via the sidecar (real BotGuard flow).
	binding1 := "moombox-fallback-test-A-" + time.Now().Format("150405")
	mintCtx, mintCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer mintCancel()
	t1 := time.Now()
	session, err := pp.GeneratePoToken(mintCtx, binding1, false)
	if err != nil {
		t.Fatalf("first GeneratePoToken (via sidecar): %v", err)
	}
	t.Logf("first mint (via sidecar): poToken=%q elapsed=%v",
		trunc(session.PoToken, 24), time.Since(t1))
	if session.PoToken == "" {
		t.Fatalf("expected non-empty PO token from sidecar")
	}

	// Hard-kill the sidecar process to simulate a crash. The readPump
	// goroutine should observe stdout EOF within a beat and flip
	// IsHealthy() to false.
	if sc.IsHealthy() == false {
		t.Fatalf("sidecar already unhealthy before kill")
	}
	t.Log("hard-killing sidecar to force fallback")
	if err := killSidecarForTest(sc); err != nil {
		t.Fatalf("kill: %v", err)
	}

	// Wait for unhealthy to propagate.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && sc.IsHealthy() {
		time.Sleep(50 * time.Millisecond)
	}
	if sc.IsHealthy() {
		t.Fatalf("expected sidecar unhealthy after kill")
	}

	// Second mint with bypassCache so we don't hit the session cache and
	// can observe the fallback path. The goja path may or may not
	// succeed depending on test rig connectivity; what we assert is:
	//   1. The call returns within the timeout (no hang waiting on the
	//      dead sidecar).
	//   2. The sidecar pointer is still attached (we don't auto-detach).
	binding2 := "moombox-fallback-test-B-" + time.Now().Format("150405")
	mintCtx2, mintCancel2 := context.WithTimeout(context.Background(), 90*time.Second)
	defer mintCancel2()
	t2 := time.Now()
	_, err = pp.GeneratePoToken(mintCtx2, binding2, true)
	dur2 := time.Since(t2)
	if dur2 > 90*time.Second {
		t.Errorf("second mint took longer than expected: %v", dur2)
	}
	t.Logf("second mint (post-kill): err=%v elapsed=%v", err, dur2)

	// Sanity: the sidecar field should still be set; we just route around it.
	pp.mu.Lock()
	stillAttached := pp.sidecar != nil
	pp.mu.Unlock()
	if !stillAttached {
		t.Errorf("expected sidecar pointer to remain attached after fallback (we don't auto-detach)")
	}
}

// killSidecarForTest reaches into the sidecar to terminate the underlying
// node.exe process directly, bypassing the graceful shutdown path. Used
// only by tests in this package; not part of the public API.
func killSidecarForTest(s *sidecar.Sidecar) error {
	return s.KillForTest()
}
