package bgutils

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// deadlineBody fails its read with context.DeadlineExceeded: what an HTTP
// client timeout firing mid-body looks like, and, by shape, what the
// sidecar's own RequestTimeout returns — a deadline of a context the caller
// never owned.
type deadlineBody struct{}

func (deadlineBody) Read([]byte) (int, error) { return 0, context.DeadlineExceeded }
func (deadlineBody) Close() error              { return nil }

// deadlineTransport answers every request with a deadlineBody and counts the
// requests, one per mint that reached the network.
type deadlineTransport struct{ calls atomic.Int32 }

func (t *deadlineTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.calls.Add(1)
	return &http.Response{StatusCode: http.StatusOK, Body: deadlineBody{}, Header: http.Header{}, Request: r}, nil
}

// A timeout INSIDE the leader's mint is the answer for its waiters, not the
// leader leaving. The waiter retry keyed on the error's type alone, so every
// waiter re-ran the doomed mint in turn — against a wedged sidecar, one more
// 90 s request per waiter, the last of them held for all of them.
//
// Mutant: retry on `isContextErr(entry.err) && ctx.Err() == nil` again,
// without leaderGone — the followers' re-mints reach the network.
func TestAMintTimeoutIsTheWaitersAnswer(t *testing.T) {
	prev := bgHTTPClient
	tr := &deadlineTransport{}
	bgHTTPClient = &http.Client{Transport: tr}
	t.Cleanup(func() { bgHTTPClient = prev })

	pp := NewPotProvider(&BgConfig{}, bypassTestLogger{})
	release, err := pp.lockMinterCreation(context.Background()) // park the leader before its minter build
	if err != nil {
		t.Fatal(err)
	}

	leaderDone := make(chan error, 1)
	go func() {
		_, err := pp.GeneratePoToken(context.Background(), "SAMEVIDEO", false)
		leaderDone <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		pp.mu.Lock()
		_, ok := pp.inflight["SAMEVIDEO"]
		pp.mu.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the leader never registered its mint")
		}
		time.Sleep(5 * time.Millisecond)
	}

	const followers = 3
	followerDone := make(chan error, followers)
	for range followers {
		go func() {
			_, err := pp.GeneratePoToken(context.Background(), "SAMEVIDEO", false)
			followerDone <- err
		}()
	}
	for pp.inflightWaits.Load() < followers {
		if time.Now().After(deadline) {
			t.Fatal("the followers never joined the leader's mint")
		}
		time.Sleep(5 * time.Millisecond)
	}
	release()

	if err := <-leaderDone; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("leader: %v, want the mint's own timeout", err)
	}
	for range followers {
		select {
		case err := <-followerDone:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("follower: %v, want the leader's answer", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a follower never returned")
		}
	}
	if n := tr.calls.Load(); n != 1 {
		t.Errorf("%d mints reached the network, want the leader's 1", n)
	}
}
