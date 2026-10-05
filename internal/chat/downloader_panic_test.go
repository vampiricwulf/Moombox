package chat

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestStartReportsARecoveredPanic pins a panic as an outcome. Start recovers
// it so the goroutine survives, and used to return nil afterwards — the zero
// value of an unnamed result — so the worker recorded chat_status "finished"
// over a capture that died. The handoff waiter (the orchestrator's Start on the
// instance tryStartEarlyChat already started) must read the same verdict, which
// needs it recorded before done closes.
func TestStartReportsARecoveredPanic(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID:             "vidPanic",
		InitialContinuation: "tok0",
		ApiKey:              "k",
		IsLiveOrUpcoming:    true,
	})
	entered := make(chan struct{})
	release := make(chan struct{})
	cd.testFetchOverride = func(context.Context) (*ChatApiResponse, error) {
		close(entered)
		<-release
		panic("forced chat panic")
	}

	first := make(chan error, 1)
	go func() { first <- cd.Start(context.Background()) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the run never reached its first fetch")
	}

	waiter := make(chan error, 1)
	go func() { waiter <- cd.Start(context.Background()) }()
	// The waiter must be parked in the already-running arm before the panic
	// lands; there is no hook for that, so give it a moment to get there.
	time.Sleep(50 * time.Millisecond)
	close(release)

	for name, ch := range map[string]chan error{"the run's own Start": first, "the handoff waiter": waiter} {
		select {
		case err := <-ch:
			if err == nil || !strings.Contains(err.Error(), "panic") {
				t.Errorf("%s returned %v, want an error naming the panic", name, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s never returned", name)
		}
	}
}
