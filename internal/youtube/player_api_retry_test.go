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

func newRetryTestAPI() *PlayerAPI {
	return NewPlayerAPI(NewAuth(cookies.NewCookieJar(), noopLogger{}), noopLogger{})
}

// TestDoRetryRequestStopsWhenTheBackoffWouldOutlastTheDeadline is ledger item
// T3-27. The recovery budget is min(45 s, MaxTimeout/3) — 10 s at the floor —
// and the retry ladder alone is 7 s.
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

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	_, err := newRetryTestAPI().doRetryRequest(ctx, srv.URL, []byte(`{}`), nil, nil, "Innertube")
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

	info, err := newRetryTestAPI().doRetryRequest(ctx, srv.URL, []byte(`{}`), nil, nil, "Innertube")
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

	_, err := newRetryTestAPI().doRetryRequest(context.Background(), srv.URL, []byte(`{}`), nil, nil, "Innertube")
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
