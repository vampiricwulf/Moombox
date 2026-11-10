package chat

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestAStopBeforeStartStopsThatRun is Arc E's open lost-Stop item (K52): a
// Stop() that lands before the run's Start takes the lock — stopEarlyChat
// racing runEarlyChat's goroutine `dl.Start(ctx)` — used to be undone by
// Start resetting cancelFlag at its top, so the run went on, and its OnStart
// wrote chat_status "downloading" over the settle the Stop's caller had
// written. The Stop now stops that run: Start returns at once, fetches
// nothing and fires no OnStart.
//
// Mutant: Start without the stop-before-run check — OnStart fires and the
// loop fetches.
func TestAStopBeforeStartStopsThatRun(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID:             "vidEarlyStop",
		OutputFile:          filepath.Join(t.TempDir(), "chat.json"),
		InitialContinuation: "tok0",
		ApiKey:              "k",
		IsLiveOrUpcoming:    true,
	})
	var started, fetched atomic.Int32
	cd.OnStart = func(int, bool) { started.Add(1) }
	cd.testFetchOverride = func(ctx context.Context) (*ChatApiResponse, error) {
		fetched.Add(1)
		<-ctx.Done()
		return nil, ctx.Err()
	}

	cd.Stop() // lands before the goroutine's Start runs

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := cd.Start(ctx); err != nil {
		t.Fatalf("Start after a Stop = %v, want nil (a stopped run)", err)
	}
	if ctx.Err() != nil {
		t.Fatal("Start ran until the test's deadline — the Stop was lost")
	}
	if n := started.Load(); n != 0 {
		t.Errorf("OnStart fired %d time(s) — it writes chat_status \"downloading\" over the Stop's settle", n)
	}
	if n := fetched.Load(); n != 0 {
		t.Errorf("the stopped run fetched %d time(s)", n)
	}
	if cd.IsRunning() {
		t.Error("IsRunning() = true after a Start the Stop refused")
	}
}

// TestAStopIsConsumedByTheRunItStopped: the stop-before-run latch is spent by
// the one Start it stops. A Stop that landed while a run was in flight stops
// that run and leaves nothing behind, so the next Start on the instance (the
// early-chat handoff re-Starts a finished instance) runs.
func TestAStopIsConsumedByTheRunItStopped(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID:             "vidStopConsumed",
		OutputFile:          filepath.Join(t.TempDir(), "chat.json"),
		InitialContinuation: "tok0",
		ApiKey:              "k",
		IsLiveOrUpcoming:    true,
	})
	var started atomic.Int32
	cd.OnStart = func(int, bool) { started.Add(1) }
	inFetch := make(chan struct{}, 4)
	cd.testFetchOverride = func(ctx context.Context) (*ChatApiResponse, error) {
		inFetch <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// A Stop before any Start is spent by the first Start.
	cd.Stop()
	_ = cd.Start(ctx)

	// A Stop during a run stops that run only.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = cd.Start(ctx)
	}()
	<-inFetch
	cd.Stop()
	<-done

	// And the instance runs again afterwards.
	done2 := make(chan struct{})
	go func() {
		defer close(done2)
		_ = cd.Start(ctx)
	}()
	select {
	case <-inFetch:
	case <-ctx.Done():
		t.Fatal("a Start after a stopped run never ran — a Stop was left latched")
	}
	cd.Stop()
	<-done2
	if n := started.Load(); n != 2 {
		t.Errorf("OnStart fired %d time(s), want 2 (the two runs no Stop preceded)", n)
	}
}
