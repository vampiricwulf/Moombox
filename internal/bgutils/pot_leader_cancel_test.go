package bgutils

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// A caller that joined another's mint for the same binding used to get the
// leader's error verbatim — including the leader's own context ending, while
// its own context was live. A cancelled monitor probe thus handed "context
// canceled" to a job's mint for the same video, which went on without a token.
// The waiter now asks again.
//
// No network here, so the follower's own mint fails too — but with that
// failure, not the leader's cancellation.
//
// Mutant: the isContextErr retry removed — the follower returns
// context.Canceled.
func TestALeadersCancellationIsNotTheWaitersAnswer(t *testing.T) {
	prev := bgHTTPClient
	bgHTTPClient = &http.Client{Transport: refusingTransport{}}
	t.Cleanup(func() { bgHTTPClient = prev })

	pp := NewPotProvider(&BgConfig{}, bypassTestLogger{})
	release, err := pp.lockMinterCreation(context.Background()) // park the leader before its minter build
	if err != nil {
		t.Fatal(err)
	}

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() {
		_, err := pp.GeneratePoToken(leaderCtx, "SAMEVIDEO", false)
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

	followerDone := make(chan error, 1)
	go func() {
		_, err := pp.GeneratePoToken(context.Background(), "SAMEVIDEO", false)
		followerDone <- err
	}()
	time.Sleep(100 * time.Millisecond) // the follower joins the leader's entry
	cancelLeader()
	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader: %v, want its own cancellation", err)
	}
	release()

	select {
	case err := <-followerDone:
		if errors.Is(err, context.Canceled) {
			t.Errorf("the follower, whose context is live, got the leader's cancellation: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the follower never returned")
	}
}
