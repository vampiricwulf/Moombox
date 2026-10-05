package bgutils

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"
)

type bypassTestLogger struct{}

func (bypassTestLogger) Debug(string, ...any) {}
func (bypassTestLogger) Info(string, ...any)  {}
func (bypassTestLogger) Warn(string, ...any)  {}
func (bypassTestLogger) Error(string, ...any) {}

type refusingTransport struct{}

func (refusingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("no network in this test")
}

// A bypassCache mint — the 403 credential refresh, which must not be handed
// the cached minter's token — joined an ordinary mint in flight for the same
// binding and got exactly that token back, so the refresh changed nothing
// and the next segment 403'd again. It now mints on its own. And the ordinary
// mint, which began before it, no longer caches its stale session over
// whatever the refresh brings.
//
// The bypass mint has no sidecar here and no network, so it fails; what the
// test pins is that it never returns the old minter's token.
//
// Mutants: the bypass key removed (B joins A and returns the old token); the
// cacheGen check removed (A's old-minter session is cached after B began).
func TestABypassMintNeverJoinsAnOrdinaryOne(t *testing.T) {
	prev := bgHTTPClient
	bgHTTPClient = &http.Client{Transport: refusingTransport{}}
	t.Cleanup(func() { bgHTTPClient = prev })

	pp := NewPotProvider(&BgConfig{}, bypassTestLogger{})
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	pp.minterCache[defaultMinterKey] = &TokenMinter{
		MintFunc: func(string) (string, error) {
			once.Do(func() { close(entered) })
			<-release
			return "token-from-OLD-minter", nil
		},
		ExpiresAt: time.Now().Add(time.Hour),
	}

	type res struct {
		s   *SessionData
		err error
	}
	aDone := make(chan res, 1)
	go func() {
		s, err := pp.GeneratePoToken(context.Background(), "VIDEOID", false)
		aDone <- res{s, err}
	}()
	<-entered // A is minting with the cached minter

	b := make(chan res, 1)
	go func() {
		s, err := pp.GeneratePoToken(context.Background(), "VIDEOID", true)
		b <- res{s, err}
	}()
	var bRes res
	select {
	case bRes = <-b:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("the bypass mint waited on the ordinary one")
	}
	close(release)
	aRes := <-aDone

	if bRes.err == nil && bRes.s != nil && bRes.s.PoToken == "token-from-OLD-minter" {
		t.Error("the bypass mint returned the cached minter's token")
	}
	if aRes.err != nil || aRes.s.PoToken != "token-from-OLD-minter" {
		t.Errorf("the ordinary mint: %+v, %v", aRes.s, aRes.err)
	}
	pp.mu.Lock()
	cached, ok := pp.sessionCache["VIDEOID"]
	pp.mu.Unlock()
	if ok && cached.PoToken == "token-from-OLD-minter" {
		t.Error("the ordinary mint cached its old-minter session after a bypass mint began")
	}
}

// An ordinary caller arriving while a bypass mint is in flight joins it: its
// token is the fresher one.
func TestAnOrdinaryMintJoinsABypassOneInFlight(t *testing.T) {
	pp := NewPotProvider(&BgConfig{}, bypassTestLogger{})
	entry := &inflightEntry{done: make(chan struct{})}
	pp.inflight["VIDEOID"+bypassInflightSuffix] = entry
	got := make(chan *SessionData, 1)
	go func() {
		s, _ := pp.GeneratePoToken(context.Background(), "VIDEOID", false)
		got <- s
	}()
	time.Sleep(50 * time.Millisecond)
	entry.session = &SessionData{PoToken: "fresh"}
	close(entry.done)
	select {
	case s := <-got:
		if s == nil || s.PoToken != "fresh" {
			t.Errorf("got %+v, want the bypass mint's token", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the ordinary caller never returned")
	}
}
