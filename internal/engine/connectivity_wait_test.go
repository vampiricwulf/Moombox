package engine

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWaitForConnectivity_AlreadyOnline(t *testing.T) {
	start := time.Now()
	err := waitForConnectivity(t.Context(), func() bool { return true }, connectivityPollInterval)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("should return immediately when online")
	}
}

func TestWaitForConnectivity_WaitsAndReturns(t *testing.T) {
	// A millisecond poll instead of connectivityPollInterval: what this test
	// proves is that the ticker re-asks and returns once the probe flips, not
	// how long production's ticker happens to be (that is pinned by
	// TestDefaultDelaysMatchConstants).
	const poll = 50 * time.Millisecond
	var online atomic.Bool
	go func() {
		time.Sleep(20 * time.Millisecond) // flips before the first tick
		online.Store(true)
	}()

	start := time.Now()
	err := waitForConnectivity(t.Context(), func() bool { return online.Load() }, poll)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second { // safety net, ~20x one poll
		t.Fatalf("waitForConnectivity took %v after the probe flipped online, want one poll interval", elapsed)
	}
}

func TestWaitForConnectivity_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	err := waitForConnectivity(ctx, func() bool { return false }, connectivityPollInterval)
	if err != context.DeadlineExceeded {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
}

// TestCallIsOnlineSingleFlight pins ENGINE-15 (report #51): a hung IsOnline
// probe used to leak one goroutine per 5 s poll for the whole outage, and
// every live downloader polls independently. Concurrent callers now share the
// one in-flight probe.
//
// Mutant: restoring the per-call `go func()` — starts counts 5 instead of 1.
func TestCallIsOnlineSingleFlight(t *testing.T) {
	var starts atomic.Int32
	block := make(chan struct{})
	probe := func() bool {
		starts.Add(1)
		<-block
		return true
	}

	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() { defer wg.Done(); callIsOnline(probe) }()
	}
	wg.Wait() // every caller gives up at isOnlineProbeTimeout
	close(block)

	if n := starts.Load(); n != 1 {
		t.Fatalf("probe invocations = %d, want 1 — callIsOnline is not single-flighted", n)
	}

	// Do not return while this flight is still registered. The probe goroutine
	// clears isOnlineInFlight in its deferred func, and a test that started
	// inside that window would attach to the finished flight and get `true`
	// WITHOUT invoking its own probe — two engine tests count their own probe
	// invocations (downloader_dash_headseq_test.go,
	// downloader_hls_maxtimeout_test.go), so leaving it registered is a latent
	// order-dependent flake.
	for {
		isOnlineMu.Lock()
		retired := isOnlineInFlight == nil
		isOnlineMu.Unlock()
		if retired {
			return
		}
		runtime.Gosched()
	}
}
