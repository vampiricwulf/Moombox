package cipher

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/vampiricwulf/Moombox/internal/httpx/httpxtest"
)

// Two extractions ask for the same new player's STS at once, and the one
// that asked first is cancelled while the player JS is still downloading.
// The cancelled one leaves with its own context's error; the other, whose
// context is live, gets the STS from the same single download. It used to be
// handed the first caller's context.Canceled, which extractSTS turns into
// "no STS": that extraction's player requests went out without a
// signatureTimestamp.
//
// Mutants: the fetch run on the first caller's context (the second caller
// gets context.Canceled); a waiter that starts a fetch of its own instead of
// sharing (GETs 2).
func TestStsACancelledFirstCallerDoesNotPoisonTheOthers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const playerURL = "https://www.youtube.com/s/player/abcd1234/player_ias.vflset/en_US/base.js"
		var gets atomic.Int32
		serve := make(chan struct{})
		orig := playerHTTPClient
		playerHTTPClient = httpxtest.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gets.Add(1)
			<-serve
			_, _ = w.Write([]byte(`var cfg={signatureTimestamp:19834,other:1};`))
		}))
		defer func() { playerHTTPClient = orig }()
		pc, err := NewPlayerCache(t.TempDir(), nopPlayerCacheLogger{})
		if err != nil {
			t.Fatal(err)
		}
		sc := NewStsCache()

		type answer struct {
			sts string
			err error
		}
		firstCtx, cancelFirst := context.WithCancel(context.Background())
		defer cancelFirst()
		first := make(chan answer, 1)
		go func() {
			sts, err := sc.GetSts(firstCtx, pc, playerURL)
			first <- answer{sts, err}
		}()
		synctest.Wait() // the first caller's download is held by the server
		second := make(chan answer, 1)
		go func() {
			sts, err := sc.GetSts(context.Background(), pc, playerURL)
			second <- answer{sts, err}
		}()
		synctest.Wait()

		cancelFirst()
		if got := <-first; !errors.Is(got.err, context.Canceled) {
			t.Errorf("cancelled caller = %q, %v; want its own context.Canceled", got.sts, got.err)
		}
		close(serve)
		if got := <-second; got.err != nil || got.sts != "19834" {
			t.Errorf("caller with a live context = %q, %v; want the STS 19834", got.sts, got.err)
		}
		if n := gets.Load(); n != 1 {
			t.Errorf("player GETs = %d, want the one shared download", n)
		}
	})
}

// heldTransport holds every request until release is closed, then answers
// 404: a player CDN slow to say the player is gone. entered gets one value
// per request that arrives.
type heldTransport struct {
	entered chan struct{}
	release chan struct{}
}

func (ht *heldTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ht.entered <- struct{}{}
	<-ht.release
	return &http.Response{
		Status:     "404 Not Found",
		StatusCode: http.StatusNotFound,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     http.Header{},
		Body:       http.NoBody,
		Request:    req,
	}, nil
}

// A GetSolvers caller queued behind another caller's compile leaves when its
// own context ends, with that context's error. The queue was a plain mutex:
// a cancelled job, or a worker shutting down, stayed in it until every
// caller ahead had finished its own fetch of a player the CDN was not
// serving.
//
// Mutant: the compile slot taken without watching ctx (the queued caller is
// still waiting after 5 s, with the first caller's fetch held).
func TestGetSolversAQueuedCallerLeavesOnItsOwnContext(t *testing.T) {
	const playerURL = "https://www.youtube.com/s/player/abcd1234/player_ias.vflset/en_US/base.js"
	ht := &heldTransport{entered: make(chan struct{}, 4), release: make(chan struct{})}
	orig := playerHTTPClient
	playerHTTPClient = &http.Client{Transport: ht}
	defer func() { playerHTTPClient = orig }()
	s, err := NewGojaResolver(t.TempDir(), &testLogger{})
	if err != nil {
		t.Fatal(err)
	}

	holder := make(chan error, 1)
	go func() {
		_, err := s.GetSolvers(context.Background(), playerURL)
		holder <- err
	}()
	<-ht.entered // the first caller holds the compile slot, its fetch held

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	queued := make(chan error, 1)
	go func() {
		_, err := s.GetSolvers(ctx, playerURL)
		queued <- err
	}()
	select {
	case err := <-queued:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("queued caller = %v, want its own context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("queued caller with an ended context is still waiting for the compile slot")
	}

	close(ht.release)
	if err := <-holder; !errors.Is(err, ErrPlayerJSFetch) {
		t.Errorf("first caller = %v, want its own fetch's ErrPlayerJSFetch", err)
	}
}
