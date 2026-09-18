package youtube

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// scaleRetryBackoff shrinks the 1/2/4 s schedule so a deadline test costs
// milliseconds. internal/youtube runs no parallel tests, so the swap is safe.
func scaleRetryBackoff(t *testing.T, base time.Duration) {
	t.Helper()
	previous := playerRetryBackoffBase
	playerRetryBackoffBase = base
	t.Cleanup(func() { playerRetryBackoffBase = previous })
}

// newRetryTestAPI builds a PlayerAPI with a real (empty, in-memory) cookie
// jar. The retry path and the cascade entry points both reach
// Auth.GenerateAPIHeaders, which dereferences the jar, so a nil Auth would
// panic before the first request is ever made.
func newRetryTestAPI() *PlayerAPI {
	return NewPlayerAPI(NewAuth(cookies.NewCookieJar(), noopLogger{}), noopLogger{})
}

// TestDoRetryRequestStopsWhenTheBackoffWouldOutlastTheDeadline is ledger item
// T3-27. The recovery budget is min(45 s, MaxTimeout/3) — 10 s at the floor —
// and the retry ladder alone is 7 s.
//
// Arithmetic (scaled base B = 40 ms, deadline D = 120 ms), after fix round 1
// added a one-base reservation for the request that follows each sleep: the
// first retry's delay is B, and its guard threshold is delay+B = 80 ms — the
// ~120 ms remaining clears it with a 40 ms margin, so the sleep happens. That
// leaves ~80 ms for the second retry, whose delay is 2B = 80 ms and whose
// threshold is delay+B = 120 ms; ~80 ms remaining is 40 ms under that
// threshold, so the third attempt is skipped and the second 503 comes back
// immediately. Both checks carry a 40 ms margin — wide enough that a loaded
// CI runner cannot misfire the guard by one iteration.
//
// Mutant named: an unconditional utils.Sleep. It burns the rest of the budget
// inside the sleep and returns context.DeadlineExceeded, discarding the 503
// that is the actual reason the caller is being told no.
func TestDoRetryRequestStopsWhenTheBackoffWouldOutlastTheDeadline(t *testing.T) {
	scaleRetryBackoff(t, 40*time.Millisecond)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()

	_, err := newRetryTestAPI().doRetryRequest(ctx, srv.URL, []byte(`{}`), nil, nil, "Innertube", "abc12345678")
	if err == nil {
		t.Fatal("a 503-forever server must produce an error")
	}
	if !strings.Contains(err.Error(), "HTTP 503") {
		t.Errorf("err = %v, want the last HTTP error (HTTP 503), not the deadline", err)
	}
	if n := hits.Load(); n != 2 {
		t.Errorf("server saw %d requests, want 2 (the immediate attempt plus one that fit its backoff)", n)
	}
}

// TestDoRetryRequestReservesOneBackoffBaseForTheRequest is fix round 1 on
// T3-27: the guard must leave room not just for the sleep but for the HTTP
// round trip that follows it. Without that reservation, a remaining budget of
// delay + a few milliseconds lets the sleep run and then has the *request*
// race the deadline — the very bug this task exists to fix, just moved one
// step later, and non-deterministically at that.
//
// Arithmetic (scaled base B = 40 ms): the first retry's delay is B. A
// deadline of delay + half a base = 60 ms leaves only 20 ms beyond the sleep
// — short of the one full base (40 ms) the fixed guard reserves for the
// request — so the sleep must never happen: the real 503 from the first
// attempt comes back immediately, deterministically, with no second request
// ever sent.
//
// Mutant named: the un-reserved guard `time.Until(deadline) <= delay` (this
// task's own fix, before this round). 60 ms > 40 ms (delay alone) is true, so
// it would sleep the full 40 ms and then send a second request into a
// ~20 ms remaining budget — one extra hit the reserved guard never makes.
func TestDoRetryRequestReservesOneBackoffBaseForTheRequest(t *testing.T) {
	scaleRetryBackoff(t, 40*time.Millisecond)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	_, err := newRetryTestAPI().doRetryRequest(ctx, srv.URL, []byte(`{}`), nil, nil, "Innertube", "abc12345678")
	if err == nil {
		t.Fatal("a 503-forever server must produce an error")
	}
	if !strings.Contains(err.Error(), "HTTP 503") {
		t.Errorf("err = %v, want the last HTTP error (HTTP 503), not the deadline", err)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("server saw %d requests, want 1 — the reserved guard must skip the sleep entirely", n)
	}
}

// TestDoRetryRequestKeepsRetryingInsideABudget pins that the new check fires
// only when the sleep genuinely would not fit.
//
// Mutant named: a check that returns early whenever ctx HAS a deadline. It
// gives up after the first attempt and never sees the 200.
func TestDoRetryRequestKeepsRetryingInsideABudget(t *testing.T) {
	scaleRetryBackoff(t, 5*time.Millisecond)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"videoDetails":{"videoId":"abc12345678","title":"T"}}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	info, err := newRetryTestAPI().doRetryRequest(ctx, srv.URL, []byte(`{}`), nil, nil, "Innertube", "abc12345678")
	if err != nil {
		t.Fatalf("doRetryRequest: %v", err)
	}
	if info == nil {
		t.Fatal("info is nil after a 200")
	}
	if n := hits.Load(); n != 3 {
		t.Errorf("server saw %d requests, want 3", n)
	}
}

// TestDoRetryRequestWithoutADeadlineUsesEveryAttempt pins the `ok` half of
// ctx.Deadline().
//
// Mutant named: `dl, _ := ctx.Deadline()` with the bool dropped. A
// deadline-less context yields the zero Time, time.Until(zero) is hugely
// negative, and the guard fires on the FIRST retry — collapsing four attempts
// to one for every caller that passes context.Background().
func TestDoRetryRequestWithoutADeadlineUsesEveryAttempt(t *testing.T) {
	scaleRetryBackoff(t, time.Millisecond)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	_, err := newRetryTestAPI().doRetryRequest(context.Background(), srv.URL, []byte(`{}`), nil, nil, "Innertube", "abc12345678")
	if err == nil {
		t.Fatal("a 503-forever server must produce an error")
	}
	if n := hits.Load(); n != 4 {
		t.Errorf("server saw %d requests, want 4 — every attempt must run when there is no deadline", n)
	}
}

// minimalPotProvider implements PotTokenProvider with exactly the method the
// player API calls. It is the compile-time pin for ruling R1: while the
// interface still declared GeneratePlayerPoToken, this type did not satisfy
// it, and the assignment below did not build.
//
// Mutant named: re-adding a method to PotTokenProvider that no call site in
// this package uses breaks this file rather than quietly widening what every
// provider must implement.
type minimalPotProvider struct{ binding string }

func (m *minimalPotProvider) GeneratePoTokenString(ctx context.Context, contentBinding string, bypassCache bool) (string, error) {
	m.binding = contentBinding
	return "pot-" + contentBinding, nil
}

func TestPotTokenProviderNeedsOnlyGeneratePoTokenString(t *testing.T) {
	var provider PotTokenProvider = &minimalPotProvider{}
	got, err := provider.GeneratePoTokenString(context.Background(), "dQw4w9WgXcQ", false)
	if err != nil {
		t.Fatalf("GeneratePoTokenString: %v", err)
	}
	if got != "pot-dQw4w9WgXcQ" {
		t.Errorf("token = %q, want %q", got, "pot-dQw4w9WgXcQ")
	}
}
