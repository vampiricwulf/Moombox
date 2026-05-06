package sidecar

import (
	"context"
	"sync"
	"testing"
	"time"

	bgembed "github.com/vampiricwulf/Moombox/internal/bgutils/embed"
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

// startSidecar spins up a Sidecar pointed at t.TempDir() so each test gets
// its own isolated extraction. Returns the started sidecar; t.Cleanup
// handles shutdown so even a panicking test won't leak the child process.
func startSidecar(t *testing.T) *Sidecar {
	t.Helper()
	requireBlobs(t)

	s := New(Config{
		CacheDir:       t.TempDir(),
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

// TestSidecarPingPong verifies the basic JSON-RPC round-trip: extract,
// launch, ready handshake, then a one-shot ping/pong. Start gates the
// handshake on the sidecar's `ready` notification, so by the time we get
// here the JSON-RPC layer is already known to work; the explicit ping
// here is a regression check on the round-trip plumbing itself, not the
// startup handshake. Does not hit network.
func TestSidecarPingPong(t *testing.T) {
	s := startSidecar(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if !s.IsHealthy() {
		t.Fatalf("expected sidecar to be healthy after ping")
	}
}

// TestSidecarGetStats exercises the JSON-RPC decode path with a non-trivial
// response object (numbers nested under named fields).
func TestSidecarGetStats(t *testing.T) {
	s := startSidecar(t)

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
}

// TestSidecarConcurrentRequests fires multiple pings in parallel; the
// reqID multiplexer must route each response to its caller without
// stalling. Failure mode: a writeMu deadlock or a pending-map race would
// hang the test.
func TestSidecarConcurrentRequests(t *testing.T) {
	s := startSidecar(t)

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
}

// TestSidecarGracefulShutdown verifies Stop returns promptly when the
// sidecar has nothing inflight. Stop's worst-case is now ~3s (1s
// shutdown-RPC + 2s wait + Kill on timeout); 5s ceiling here accounts
// for Windows process-reap latency without crossing into "we're
// silently leaking the shutdown budget" territory.
func TestSidecarGracefulShutdown(t *testing.T) {
	s := startSidecar(t)

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

// TestSidecarExtractIdempotent ensures a second Start against the same
// cache dir reuses the prior extraction. The old sidecar is stopped first
// (a single Sidecar instance can't be started twice).
func TestSidecarExtractIdempotent(t *testing.T) {
	requireBlobs(t)

	cacheDir := t.TempDir()
	cfg := Config{
		CacheDir:       cacheDir,
		StartupTimeout: 30 * time.Second,
		RequestTimeout: 30 * time.Second,
		Logger:         &testLogger{t: t},
	}

	// First launch: extracts the payload.
	s1 := New(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	t1Start := time.Now()
	if err := s1.Start(ctx); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	first := time.Since(t1Start)
	if err := s1.Stop(); err != nil {
		t.Fatalf("first Stop: %v", err)
	}

	// Second launch: should skip extraction (cache hit).
	s2 := New(cfg)
	t2Start := time.Now()
	if err := s2.Start(ctx); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	second := time.Since(t2Start)
	if err := s2.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}

	t.Logf("first start: %v   second start: %v", first, second)
	// Hard to assert "second was faster" reliably across machines, but the
	// extraction takes 1-3s and ping/pong is sub-100ms, so the second
	// should be at least notably shorter. Use a generous 50% threshold.
	if second > first+time.Second && second > first*9/10 {
		t.Errorf("expected second Start to be faster (cache reuse); first=%v second=%v", first, second)
	}
}
