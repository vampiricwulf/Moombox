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

// TestTheBucketWaitOutlastsTheRoundingSkew is the pad.
//
// X-RateLimit-Reset-After is millisecond precision. A server that rounds it to
// NEAREST reports a window that closes up to half a millisecond LATER than the
// number says, so a sender waking exactly on the number arrives inside the
// window and is 429'd — by its own punctuality. Measured against a fake
// Discord doing exactly that: 1-3 such 429s per 400 requests. The ladder
// absorbs them (one wasted attempt plus the 429's >=1s Retry-After), which is
// why this is a pad and not an Important, but paying 50ms to not spend a
// second is the trade.
//
// THE MUTANT: dropping bucketSkewPad from noteBucket. The gap falls back to
// ~150ms and this fails while TestSleepsBeforeAnEmptyBucket above still
// passes — which is the whole point of measuring it separately.
func TestTheBucketWaitOutlastsTheRoundingSkew(t *testing.T) {
	rec := &arrivalRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rec.mark()
		rw.Header().Set("X-RateLimit-Remaining", "0")
		rw.Header().Set("X-RateLimit-Reset-After", "0.15")
		rw.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	d := &DiscordWebhook{URL: srv.URL}
	for range 2 {
		if err := d.Send("t", "", 0, nil, SendOptions{}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	// The deadline is armed AFTER the first response is read, so a correct
	// sender's gap strictly exceeds the window plus the pad.
	if want, got := 150*time.Millisecond+bucketSkewPad, rec.gap(); got < want {
		t.Errorf("the second request arrived %v after the first, want >= %v — a window reported as 0.150 may really close at 0.1505, "+
			"and a sender that wakes on the nose earns a 429 for it", got, want)
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

// TestSendOnceHonoursTheBucketOnlyWithinTheShutdownCap is the ruling on this
// task's review. SendOnce is the shutdown path, and the bucket it reads belongs
// to the target's persistent *DiscordWebhook — so an alert spike just before
// the user quits can leave it armed for seconds. The process force-exits 10s
// in: a window that closes soon is still worth waiting out, but one further
// away than shutdownBucketWaitCap is not, because the 429 that follows an
// immediate post drops the item exactly as the wait would have, without
// starving every item queued behind it in the meantime.
//
// THE MUTANT: calling the uncapped waitForBucket here (what this task shipped
// first). The far-bucket branch then blocks the full 3s and fails its bound.
func TestSendOnceHonoursTheBucketOnlyWithinTheShutdownCap(t *testing.T) {
	// hitCounter serves both branches: whatever the server answers, exactly
	// one request must reach it.
	newServer := func(t *testing.T, handle http.HandlerFunc) (*httptest.Server, func() int) {
		var hits int
		var mu sync.Mutex
		srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
			mu.Lock()
			hits++
			mu.Unlock()
			handle(rw, req)
		}))
		t.Cleanup(srv.Close)
		return srv, func() int {
			mu.Lock()
			defer mu.Unlock()
			return hits
		}
	}

	t.Run("a window inside the cap is waited out", func(t *testing.T) {
		srv, hits := newServer(t, func(rw http.ResponseWriter, _ *http.Request) {
			rw.WriteHeader(http.StatusNoContent)
		})
		d := &DiscordWebhook{URL: srv.URL}
		// Arm the bucket the way an earlier Send would have.
		d.noteBucket(discordResponse{status: http.StatusOK, rateRemain: "0", rateReset: "0.15"})

		start := time.Now()
		if err := d.SendOnce("t", "", 0, nil, SendOptions{}); err != nil {
			t.Fatalf("SendOnce: %v", err)
		}
		if elapsed := time.Since(start); elapsed < 140*time.Millisecond {
			t.Errorf("SendOnce returned after %v — a 150ms window is inside the %v cap and must be waited out", elapsed, shutdownBucketWaitCap)
		}
		if got := hits(); got != 1 {
			t.Errorf("server saw %d requests, want exactly 1", got)
		}
	})

	t.Run("a window past the cap is not", func(t *testing.T) {
		// A real empty bucket answers 429, which is what makes skipping the
		// wait free: the item is dropped either way, seconds earlier.
		srv, hits := newServer(t, func(rw http.ResponseWriter, _ *http.Request) {
			rw.Header().Set("Retry-After", "3")
			rw.WriteHeader(http.StatusTooManyRequests)
		})
		d := &DiscordWebhook{URL: srv.URL}
		d.noteBucket(discordResponse{status: http.StatusOK, rateRemain: "0", rateReset: "3"})

		start := time.Now()
		err := d.SendOnce("t", "", 0, nil, SendOptions{})
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("SendOnce against a 429: want an error, got nil")
		}
		if elapsed > 100*time.Millisecond {
			t.Errorf("SendOnce took %v — a 3s window is past the %v cap and must not be slept (the 10s force-exit would eat it)", elapsed, shutdownBucketWaitCap)
		}
		if got := hits(); got != 1 {
			t.Errorf("server saw %d requests, want exactly 1", got)
		}
	})
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
