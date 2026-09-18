package twitch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/constants"
)

// errUnexpectedStubHost marks a request the 429 stub was never meant to see —
// a misrouted request must fail the test, not pass it silently.
var errUnexpectedStubHost = errors.New("stub received an unexpected request host")

// install429Stub answers every GQL request with 429 and the given Retry-After
// header, and returns the request counter.
func install429Stub(t *testing.T, retryAfter string) *atomic.Int64 {
	t.Helper()
	var calls atomic.Int64
	prev := twitchHTTPClient
	t.Cleanup(func() { twitchHTTPClient = prev })
	twitchHTTPClient = &http.Client{Transport: probeRoundTripper(func(req *http.Request) (*http.Response, error) {
		if !strings.HasPrefix(req.URL.String(), constants.TwitchURLs.GQL) {
			return nil, errUnexpectedStubHost
		}
		calls.Add(1)
		h := make(http.Header)
		h.Set("Content-Type", "application/json")
		if retryAfter != "" {
			h.Set("Retry-After", retryAfter)
		}
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     h,
			Body:       io.NopCloser(strings.NewReader(`{}`)),
			Request:    req,
		}, nil
	})}
	return &calls
}

// TestGQL429WithALongRetryAfterReturnsWithoutRetrying is TWITCH-9 (report row
// #62). A 429 whose Retry-After exceeds gqlMaxRetryDelay was retried on the
// 1s/2s/4s schedule instead — three quick requests into a throttle Twitch had
// just asked us to respect, and the monitor cycle repeated them 15 s later.
//
// The planner's ruling of the row's two options: return at once and let the
// caller's cycle cadence be the backoff, rather than parking a monitor batch
// goroutine for the 30 s cap and THEN letting the caller's cadence stack on it.
//
// Mutants: the pre-fix `ra > 0 && ra <= gqlMaxRetryDelay` condition alone (the
// call makes gqlMaxRetries+1 requests); a fix that returns the "exhausted"
// wrapper instead of the 429 error (the message assertion fails, and
// worker.classifyProbeErr reads the status positionally out of that string).
func TestGQL429WithALongRetryAfterReturnsWithoutRetrying(t *testing.T) {
	prevDelay := gqlBaseRetryDelay
	gqlBaseRetryDelay = time.Millisecond
	t.Cleanup(func() { gqlBaseRetryDelay = prevDelay })

	calls := install429Stub(t, "120") // 2 minutes: four times the 30 s cap
	log := &renderingLogger{}
	a := NewAPI(log)

	start := time.Now()
	_, err := a.gqlRequest(context.Background(), "TestOp", map[string]any{"q": 1}, "")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a 429 must not succeed")
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("the stub answered %d times, want exactly 1 — a Retry-After past the cap must "+
			"end the attempt, not start a 1s/2s/4s schedule", n)
	}
	if elapsed > 5*time.Second {
		t.Errorf("gqlRequest took %v — it must return rather than sleep out the window", elapsed)
	}
	if !strings.Contains(err.Error(), "gql rate limited (429) (TestOp): ") {
		t.Errorf("err = %q, want the ordinary 429 error — worker.classifyProbeErr reads the "+
			"status positionally out of it", err)
	}
	if n := log.countLinesContaining("twitch gql retry"); n != 0 {
		t.Errorf("%d retry Debug line(s), want 0", n)
	}
}

// TestGQL429WithAShortRetryAfterStillRetries guards the other side: a
// Retry-After inside the cap is still honoured and still retried, which is the
// behaviour audit-finding twitch.md #11 put there.
//
// Mutant: a fix that returns on EVERY 429 carrying a Retry-After — the call
// then makes 1 request instead of gqlMaxRetries+1.
func TestGQL429WithAShortRetryAfterStillRetries(t *testing.T) {
	prevDelay := gqlBaseRetryDelay
	gqlBaseRetryDelay = time.Millisecond
	t.Cleanup(func() { gqlBaseRetryDelay = prevDelay })

	calls := install429Stub(t, "1") // 1 s: inside gqlMaxRetryDelay
	a := NewAPI(&testLogger{})

	if _, err := a.gqlRequest(context.Background(), "TestOp", map[string]any{"q": 1}, ""); err == nil {
		t.Fatal("a 429 must not succeed")
	}
	if n := calls.Load(); n != gqlMaxRetries+1 {
		t.Errorf("the stub answered %d times, want %d — a Retry-After inside the cap is a "+
			"retry hint, not a stop", n, gqlMaxRetries+1)
	}
}
