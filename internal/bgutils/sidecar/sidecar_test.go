package sidecar

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	bgembed "github.com/vampiricwulf/Moombox/internal/bgutils/embed"
	"github.com/vampiricwulf/Moombox/internal/treeguard"
)

// testLogger routes Sidecar logger calls through testing.T.Logf so test
// output stays attached to the test that produced it.
type testLogger struct{ t *testing.T }

func (l *testLogger) Debug(msg string, args ...any) { l.t.Logf("[DEBUG] "+msg+" %v", args) }
func (l *testLogger) Info(msg string, args ...any)  { l.t.Logf("[INFO ] "+msg+" %v", args) }
func (l *testLogger) Warn(msg string, args ...any)  { l.t.Logf("[WARN ] "+msg+" %v", args) }
func (l *testLogger) Error(msg string, args ...any) { l.t.Logf("[ERROR] "+msg+" %v", args) }

// requireBlobs skips when the embedded blobs are missing (e.g., a fresh
// checkout where neither `go run ./tools/fetch-node` nor
// `node bgutil-sidecar/build.mjs` has been run yet). Without the blobs
// the package compiles but the sidecar can't start.
func requireBlobs(t *testing.T) {
	t.Helper()
	if len(bgembed.EmbeddedNode) == 0 || len(bgembed.SidecarTarGz) == 0 {
		t.Skip("sidecar embed blobs missing; run `go run ./tools/fetch-node` and `node bgutil-sidecar/build.mjs` first")
	}
}

// sharedSidecarCache is ONE extraction for the whole package. The payload is
// ~36 MB and every Start that has to extract it cost about ten seconds on
// the Windows CI runner (Defender scans each file as it lands), which made
// six otherwise-quick tests a 75-second package. Tests run one after another
// and each stops its own sidecar before the next starts, so the extracted
// tree is shared safely. Created on first use, removed by TestMain.
var sharedSidecarCache = sync.OnceValues(func() (string, error) {
	return os.MkdirTemp("", "moombox-sidecar-test-")
})

// TestMain removes the shared extraction once the run is over, and fails a
// run that left a file in the package directory (treeguard.Run).
func TestMain(m *testing.M) {
	code := treeguard.Run(m)
	if dir, err := sharedSidecarCache(); err == nil {
		os.RemoveAll(dir)
	}
	os.Exit(code)
}

// sharedCacheDir is the package's shared extraction directory, skipping when
// the embed blobs are missing.
func sharedCacheDir(t *testing.T) string {
	t.Helper()
	requireBlobs(t)
	cacheDir, err := sharedSidecarCache()
	if err != nil {
		t.Fatalf("shared sidecar cache dir: %v", err)
	}
	return cacheDir
}

// startSidecar spins up a Sidecar on the package's shared extraction.
// Returns the started sidecar; t.Cleanup handles shutdown so even a
// panicking test won't leak the child process.
func startSidecar(t *testing.T) *Sidecar {
	t.Helper()
	cacheDir := sharedCacheDir(t)

	s := New(Config{
		CacheDir:       cacheDir,
		StartupTimeout: 30 * time.Second, // first-launch extracts ~36 MB
		RequestTimeout: 30 * time.Second,
		Logger:         &testLogger{t: t},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })
	return s
}

// reuseMarker is a file the payload does not ship, put inside a directory it
// does: a re-extraction clears that directory first, taking the marker with
// it.
const reuseMarker = "src/.moombox-test-reuse-marker"

// TestSidecarOnTheEmbeddedPayload starts ONE real Node child on the shared
// extraction and runs, as subtests, every check that needs no more than a
// running sidecar — each of them used to start a child of its own, about a
// second apiece here and several on the Windows runner — then stops it,
// which is the graceful-shutdown check. None of them mints, so the stats
// still read as a fresh child's.
func TestSidecarOnTheEmbeddedPayload(t *testing.T) {
	cacheDir := sharedCacheDir(t)
	// The extraction the Start below must reuse — an earlier test's, or this
	// one's on a cold directory, the way a first launch extracts.
	if err := extractIfNeeded(cacheDir); err != nil {
		t.Fatalf("extractIfNeeded: %v", err)
	}
	nodePath := filepath.Join(cacheDir, nodeBinaryName())
	nodeBefore, err := os.Stat(nodePath)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(cacheDir, filepath.FromSlash(reuseMarker))
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(marker) })

	s := startSidecar(t)

	// A Start against a cache dir that already holds this payload reuses it:
	// it rewrites neither the tree nor the Node binary. This was a timing
	// comparison of a cold start with a warm one, which passed against a
	// Start that re-extracted every time whenever the two took about as long.
	//
	// Mutant: cacheLooksGood always false — the marker is cleared with src/
	// and the binary is rewritten.
	t.Run("a start reuses the extraction", func(t *testing.T) {
		if _, err := os.Stat(marker); err != nil {
			t.Errorf("Start re-extracted the tree it was given (the marker in src/ is gone: %v)", err)
		}
		nodeAfter, err := os.Stat(nodePath)
		if err != nil {
			t.Fatal(err)
		}
		if !nodeAfter.ModTime().Equal(nodeBefore.ModTime()) || nodeAfter.Size() != nodeBefore.Size() {
			t.Errorf("Start rewrote the Node binary: %v/%d before, %v/%d after",
				nodeBefore.ModTime(), nodeBefore.Size(), nodeAfter.ModTime(), nodeAfter.Size())
		}
	})

	// The basic JSON-RPC round-trip. Start gates the handshake on the
	// sidecar's `ready` notification, so by now the JSON-RPC layer is known
	// to work; the explicit ping is a regression check on the round-trip
	// plumbing itself, not the startup handshake. Does not hit network.
	t.Run("ping pong", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.ping(ctx); err != nil {
			t.Fatalf("ping: %v", err)
		}
		if !s.IsHealthy() {
			t.Fatalf("expected sidecar to be healthy after ping")
		}
	})

	// The JSON-RPC decode path with a non-trivial response object (numbers
	// nested under named fields).
	t.Run("get stats", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stats, err := s.GetStats(ctx)
		if err != nil {
			t.Fatalf("GetStats: %v", err)
		}
		// Fresh sidecar: no mints yet.
		if stats.MintsTotal != 0 {
			t.Errorf("expected MintsTotal=0, got %d", stats.MintsTotal)
		}
		if stats.CachedMinters != 0 {
			t.Errorf("expected CachedMinters=0, got %d", stats.CachedMinters)
		}
	})

	// Multiple pings in parallel: the reqID multiplexer must route each
	// response to its caller without stalling. Failure mode: a writeMu
	// deadlock or a pending-map race would hang the test.
	t.Run("concurrent requests", func(t *testing.T) {
		const N = 10
		var wg sync.WaitGroup
		errCh := make(chan error, N)
		wg.Add(N)
		for range N {
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := s.ping(ctx); err != nil {
					errCh <- err
				}
			}()
		}
		wg.Wait()
		close(errCh)
		for err := range errCh {
			t.Errorf("concurrent ping: %v", err)
		}
	})

	// The JSON-RPC round-trip for sig+n cipher solving. Uses the cb017549
	// player fixture which is structurally representative of YouTube's
	// current obfuscation pattern (no static helper-object sig algorithm;
	// URL passthrough required).
	t.Run("solve cipher", func(t *testing.T) {
		playerJS, err := os.ReadFile(filepath.Join("..", "..", "cipher", "testdata", "player_cb017549.js"))
		if err != nil {
			t.Skipf("cipher fixture missing: %v (run task 6.1 to download)", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Distinguishable challenges so we can assert on transformation.
		sigA := "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ-_"
		nA := "abcdefghij_12345"

		res, err := s.SolveCipher(ctx, SolveCipherRequest{
			PlayerID:      "cb017549",
			PlayerJS:      string(playerJS),
			SigChallenges: []string{sigA},
			NChallenges:   []string{nA},
		})
		if err != nil {
			t.Fatalf("SolveCipher: %v", err)
		}
		gotSig := res.SigResults[sigA]
		gotN := res.NResults[nA]

		if gotSig == "" || gotSig == sigA {
			t.Errorf("sig: expected non-empty transformation; got %q", gotSig)
		}
		if gotN == "" || gotN == nA {
			t.Errorf("n: expected non-empty transformation; got %q", gotN)
		}
		t.Logf("sig: %q -> %q", sigA, gotSig)
		t.Logf("n:   %q -> %q", nA, gotN)

		// Second call without playerJS should hit the player cache.
		res2, err := s.SolveCipher(ctx, SolveCipherRequest{
			PlayerID:      "cb017549",
			SigChallenges: []string{sigA},
		})
		if err != nil {
			t.Fatalf("SolveCipher (cache hit): %v", err)
		}
		if res2.SigResults[sigA] != gotSig {
			t.Errorf("cache-hit sig differs: %q vs %q", res2.SigResults[sigA], gotSig)
		}
	})

	// Last: Stop returns promptly when the sidecar has nothing inflight.
	// Stop's worst-case is ~3s (1s shutdown-RPC + 2s wait + Kill on
	// timeout); 5s ceiling here accounts for Windows process-reap latency
	// without crossing into "we're silently leaking the shutdown budget"
	// territory.
	t.Run("graceful shutdown", func(t *testing.T) {
		start := time.Now()
		if err := s.Stop(); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		elapsed := time.Since(start)
		if elapsed > 5*time.Second {
			t.Errorf("Stop took too long (must stay well under shutdown.go force-exit budget): %v", elapsed)
		}
		if s.IsHealthy() {
			t.Errorf("expected unhealthy after Stop")
		}
	})
}

// TestSidecarHardKill simulates a crashed child: hard-kill the process,
// then verify the next call returns an error rather than hanging on a
// reqID that will never come back.
func TestSidecarHardKill(t *testing.T) {
	s := startSidecar(t)
	if s.cmd == nil || s.cmd.Process == nil {
		t.Fatalf("sidecar process unexpectedly nil")
	}
	t.Logf("killing sidecar pid=%d to simulate crash", s.cmd.Process.Pid)

	if err := s.KillForTest(); err != nil {
		t.Fatalf("KillForTest: %v", err)
	}

	// readPump should observe EOF on stdout shortly and call markUnhealthy.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !s.IsHealthy() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if s.IsHealthy() {
		t.Errorf("expected sidecar unhealthy after hard kill")
	}

	// Subsequent calls should fail fast, not hang.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := s.ping(ctx)
	if err == nil {
		t.Errorf("expected ping after kill to error, got nil")
	}
}
