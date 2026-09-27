package notifications

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// arrivalRecorder timestamps each request so a test can measure the gap
// between two deliveries. Wall-clock, deliberately: the thing under test is a
// real time.Sleep, and a fake clock would pin the arithmetic while leaving the
// sleep itself unexercised.
type arrivalRecorder struct {
	mu   sync.Mutex
	when []time.Time
}

func (a *arrivalRecorder) mark() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.when = append(a.when, time.Now())
}

func (a *arrivalRecorder) gap() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.when) < 2 {
		return 0
	}
	return a.when[1].Sub(a.when[0])
}

// TestSleepsBeforeAnEmptyBucket is R1. Sixteen concurrent goroutines hammered
// one webhook's bucket and the X-RateLimit-* headers were never read, so
// Moombox produced its own 429s and each one cost a delivery attempt. A
// response saying "Remaining: 0, Reset-After: N" is Discord telling us exactly
// when the next request may go; honouring it pre-emptively costs one sleep and
// saves an attempt.
//
// THE MUTANT: dropping the waitForBucket call at the top of the attempt loop.
// The second request then arrives immediately and the gap assertion fails.
func TestSleepsBeforeAnEmptyBucket(t *testing.T) {
	rec := &arrivalRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rec.mark()
		rw.Header().Set("X-RateLimit-Remaining", "0")
		rw.Header().Set("X-RateLimit-Reset-After", "0.15")
		rw.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	d := &DiscordWebhook{URL: srv.URL}
	if err := d.Send("first", "", 0, nil, SendOptions{}); err != nil {
		t.Fatalf("first Send: %v", err)
	}
	if err := d.Send("second", "", 0, nil, SendOptions{}); err != nil {
		t.Fatalf("second Send: %v", err)
	}
	if got := rec.gap(); got < 140*time.Millisecond {
		t.Errorf("the second request arrived %v after the first — the empty-bucket sleep did not happen (want >= ~150ms)", got)
	}
}

// TestDoesNotSleepWhenTheBucketHasRoom is the premise: a sleep on every send
// would throttle a healthy webhook to one embed per window for nothing.
func TestDoesNotSleepWhenTheBucketHasRoom(t *testing.T) {
	rec := &arrivalRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rec.mark()
		rw.Header().Set("X-RateLimit-Remaining", "4")
		rw.Header().Set("X-RateLimit-Reset-After", "5")
		rw.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	d := &DiscordWebhook{URL: srv.URL}
	for range 2 {
		if err := d.Send("t", "", 0, nil, SendOptions{}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	if got := rec.gap(); got > 500*time.Millisecond {
		t.Errorf("the second request waited %v although the bucket reported 4 remaining", got)
	}
}

// TestBucketClearsWhenTheWindowRefills: a stale "Remaining: 0" must not keep
// sleeping forever once Discord reports room again.
func TestBucketClearsWhenTheWindowRefills(t *testing.T) {
	var n int
	var mu sync.Mutex
	rec := &arrivalRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		n++
		first := n == 1
		mu.Unlock()
		if first {
			rw.Header().Set("X-RateLimit-Remaining", "0")
			rw.Header().Set("X-RateLimit-Reset-After", "0.05")
		} else {
			rec.mark()
			rw.Header().Set("X-RateLimit-Remaining", "9")
			rw.Header().Set("X-RateLimit-Reset-After", "5")
		}
		rw.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	d := &DiscordWebhook{URL: srv.URL}
	for range 3 {
		if err := d.Send("t", "", 0, nil, SendOptions{}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	if got := rec.gap(); got > 500*time.Millisecond {
		t.Errorf("the third request waited %v — the bucket was not cleared by a Remaining > 0 response", got)
	}
}

// TestA429DoesNotAlsoArmTheBucketSleep: Discord sets Remaining: 0 ON a 429, and
// the ladder already honours its Retry-After. Arming the pre-emptive sleep from
// the same response would wait the window twice and burn the cumulative-sleep
// budget on the double.
func TestA429DoesNotAlsoArmTheBucketSleep(t *testing.T) {
	shrinkBackoff(t)
	var n int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		n++
		first := n == 1
		mu.Unlock()
		if first {
			rw.Header().Set("Retry-After", "0.05")
			rw.Header().Set("X-RateLimit-Remaining", "0")
			rw.Header().Set("X-RateLimit-Reset-After", "20")
			rw.WriteHeader(http.StatusTooManyRequests)
			return
		}
		rw.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	d := &DiscordWebhook{URL: srv.URL}
	start := time.Now()
	if err := d.Send("t", "", 0, nil, SendOptions{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Send took %v — the 429's Retry-After and its X-RateLimit-Reset-After were both slept", elapsed)
	}
}

// TestSendOnceMakesExactlyOneAttempt is what BeginShutdown depends on.
func TestSendOnceMakesExactlyOneAttempt(t *testing.T) {
	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		rw.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	d := &DiscordWebhook{URL: srv.URL}
	if err := d.SendOnce("t", "", 0, nil, SendOptions{}); err == nil {
		t.Fatal("SendOnce against a 500: want an error, got nil")
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 1 {
		t.Errorf("SendOnce made %d attempts, want exactly 1 — a shutdown send must not run the ladder", hits)
	}
}
