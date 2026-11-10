package engine

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// ladderDownloader is a downloader at the default MaxRetries (5) whose
// transient ladder counts in unit instead of 5 s, so a test can walk every
// rung of fetchSegmentWithRetry.
func ladderDownloader(t *testing.T, unit time.Duration) *SegmentDownloader {
	t.Helper()
	d := NewSegmentDownloader(DownloaderOptions{OutputFile: "x"})
	if d.opts.MaxRetries != MaxSegmentRetries {
		t.Fatalf("MaxRetries defaulted to %d, want %d", d.opts.MaxRetries, MaxSegmentRetries)
	}
	d.delays = fastDelays()
	d.delays.segmentRetryUnit = unit
	d.delays.connectivityPoll = 5 * time.Millisecond
	return d
}

// countingServer answers request n (1-based) with respond(n, w).
func countingServer(t *testing.T, respond func(n int32, w http.ResponseWriter)) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		respond(n.Add(1), w)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

// Four transient failures and then the segment: the fifth attempt is made and
// its body returned, after the linear ladder's 1+2+3+4 units of waiting.
//
// Mutant: the ladder's sleep written as a literal again — the test takes 50 s.
// Mutant: the sleep removed — the elapsed floor fails.
func TestFetchSegmentWithRetryClimbsTheWholeLadder(t *testing.T) {
	const unit = 10 * time.Millisecond
	srv, n := countingServer(t, func(n int32, w http.ResponseWriter) {
		if n <= 4 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, "segment")
	})
	d := ladderDownloader(t, unit)
	var fetched int64
	d.OnFetch = func(b int64) { fetched += b }

	start := time.Now()
	body, err := d.fetchSegmentWithRetry(t.Context(), srv.URL+"/seg", nil)
	elapsed := time.Since(start)

	if err != nil || string(body) != "segment" {
		t.Fatalf("fetchSegmentWithRetry = %q, %v; want the segment on the fifth attempt", body, err)
	}
	if got := n.Load(); got != 5 {
		t.Errorf("%d requests, want 5", got)
	}
	if elapsed < 10*unit {
		t.Errorf("returned after %v, want at least the ladder's 10 units (%v)", elapsed, 10*unit)
	}
	if fetched != int64(len("segment")) {
		t.Errorf("OnFetch saw %d bytes, want %d: only the successful body is an arrival", fetched, len("segment"))
	}
}

// Five failures end in ErrSegmentRetriesExhausted, and no wait follows the
// last attempt: the ladder sleeps 1+2+3+4 units, never the fifth rung's 5.
//
// The one interval measured is from the fifth request's arrival to the
// call's return: nothing but the answer without a trailing sleep, at least 5
// units with one. The ladder's own length is the test above's.
//
// Mutant: the `attempt < MaxRetries-1` guard removed — the call returns 5
// units after its last request and the ceiling fails.
func TestFetchSegmentWithRetryExhaustedWithoutATrailingSleep(t *testing.T) {
	const unit = 100 * time.Millisecond
	var lastRequest atomic.Int64 // UnixNano of the fifth request's arrival
	srv, n := countingServer(t, func(n int32, w http.ResponseWriter) {
		if n == 5 {
			lastRequest.Store(time.Now().UnixNano())
		}
		w.WriteHeader(http.StatusInternalServerError)
	})
	d := ladderDownloader(t, unit)
	var retrying int
	d.OnActivity = func(a DownloadActivity) {
		if a == ActivityRetrying {
			retrying++
		}
	}

	_, err := d.fetchSegmentWithRetry(t.Context(), srv.URL+"/seg", nil)
	returned := time.Now()

	if !errors.Is(err, ErrSegmentRetriesExhausted) {
		t.Fatalf("err = %v, want ErrSegmentRetriesExhausted", err)
	}
	if got := n.Load(); got != 5 {
		t.Errorf("%d requests, want 5", got)
	}
	if retrying != 5 {
		t.Errorf("ActivityRetrying emitted %d times, want once per failed attempt (5)", retrying)
	}
	if after := returned.Sub(time.Unix(0, lastRequest.Load())); after >= 4*unit {
		t.Errorf("returned %v after the final attempt's request: a sleep followed it (%v with one)", after, 5*unit)
	}
}

// The attempt counter is shared between error kinds: after two transient
// failures a headless 403 (no head known) is already the third attempt, so
// it is permanent at once instead of being asked headlessRefusalAttempts
// times of its own.
//
// Mutant: the headless budget counted on a counter of its own — the 403 is
// asked three times (five requests).
func TestFetchSegmentWithRetryHeadlessRefusalSharesTheAttemptCounter(t *testing.T) {
	for _, tc := range []struct {
		status int
		gone   bool
	}{{http.StatusForbidden, false}, {http.StatusGone, true}} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			srv, n := countingServer(t, func(n int32, w http.ResponseWriter) {
				switch n {
				case 1:
					w.WriteHeader(http.StatusBadGateway)
				case 2:
					// A 200 with no body is a failed fetch too.
				default:
					w.WriteHeader(tc.status)
				}
			})
			d := ladderDownloader(t, 10*time.Millisecond)
			if d.headSeq.Load() >= 0 {
				t.Fatal("a new downloader knows a head")
			}

			_, err := d.fetchSegmentWithRetry(t.Context(), srv.URL+"/seg", nil)

			if !errors.Is(err, ErrSegmentPermanent) {
				t.Fatalf("err = %v, want ErrSegmentPermanent", err)
			}
			if errors.Is(err, errSegmentGone) != tc.gone {
				t.Errorf("errors.Is(err, errSegmentGone) = %v, want %v", !tc.gone, tc.gone)
			}
			if got := n.Load(); got != 3 {
				t.Errorf("%d requests, want 3: the refusal was the third attempt", got)
			}
		})
	}
}

// A failure while the device is offline is not charged: with an outage on
// the third request, the segment still gets its five charged attempts, so
// the sixth request — the fifth charged one — is made and succeeds.
//
// Mutant: `attempt--` removed — the ladder is exhausted after five requests.
func TestFetchSegmentWithRetryOutageMidLadderIsNotCharged(t *testing.T) {
	var o outage
	srv, n := countingServer(t, func(n int32, w http.ResponseWriter) {
		if n == 3 {
			o.open()
		}
		if n <= 5 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, "segment")
	})
	d := ladderDownloader(t, 10*time.Millisecond)
	d.opts.IsOnline = o.isOnline
	var reconnecting int
	d.OnActivity = func(a DownloadActivity) {
		if a == ActivityReconnecting {
			reconnecting++
		}
		o.onActivity(a)
	}

	body, err := d.fetchSegmentWithRetry(t.Context(), srv.URL+"/seg", nil)

	if err != nil || string(body) != "segment" {
		t.Fatalf("fetchSegmentWithRetry = %q, %v; want the segment: the outage's failure must not be charged", body, err)
	}
	if got := n.Load(); got != 6 {
		t.Errorf("%d requests, want 6 (five charged attempts and the outage's one)", got)
	}
	if reconnecting != 1 {
		t.Errorf("ActivityReconnecting emitted %d times, want 1", reconnecting)
	}
	if o.down() {
		t.Error("the outage was never waited out")
	}
}
