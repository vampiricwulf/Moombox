package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// testURLExpiry reads expire= as the worker's formatURLExpiry does, for the
// engine's own tests.
func testURLExpiry(raw string) (time.Time, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return time.Time{}, false
	}
	secs, err := strconv.ParseInt(u.Query().Get("expire"), 10, 64)
	if err != nil || secs <= 0 {
		return time.Time{}, false
	}
	return time.Unix(secs, 0), true
}

// expiringURL is a segment URL of the given version that expires at exp.
func expiringURL(version int, exp time.Time) string {
	return fmt.Sprintf("%s/videoplayback?id=pr.1&itag=299&v=%d&expire=%d", inMemoryOrigin, version, exp.Unix())
}

// TestProactiveRefreshDue pins the timer's arithmetic, audit G48:
// min(expire − 30 min, last renewal + 5 h), yt-dlp's url_feed cadence
// (18000 s) when the URL names no expiry or a later one.
//
// Mutants: the lead dropped (the 2 h row's due moves to its expiry); the 5 h
// ceiling dropped (the 6 h row's due moves to 5 h 30 min, the no-expiry
// row's to never).
func TestProactiveRefreshDue(t *testing.T) {
	if proactiveRefreshLead != 30*time.Minute || proactiveRefreshEvery != 5*time.Hour || proactiveRefreshRetry != 5*time.Minute {
		t.Fatalf("lead %v, every %v, retry %v; want 30m, 5h, 5m", proactiveRefreshLead, proactiveRefreshEvery, proactiveRefreshRetry)
	}
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		url  string
		want time.Duration
	}{
		{"no expiry", inMemoryOrigin + "/videoplayback?id=x", 5 * time.Hour},
		{"expires in 6 h", expiringURL(1, now.Add(6*time.Hour)), 5 * time.Hour},
		{"expires in 2 h", expiringURL(1, now.Add(2*time.Hour)), 90 * time.Minute},
		{"expired", expiringURL(1, now.Add(-time.Hour)), -90 * time.Minute},
	} {
		if got := proactiveRefreshDue(now, tc.url, testURLExpiry).Sub(now); got != tc.want {
			t.Errorf("%s: due in %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestProactiveRefreshRenewsBeforeTheURLExpires is audit G48 under a fake
// clock: a live downloader's timer calls OnProactiveRefresh 30 minutes before
// its URL's expire= — not before, and without a single refused segment — and
// installs the URL and token it returns; the next renewal is 5 h after that
// one, the URL then living longer. A renewal that installs no URL is asked
// again 5 minutes later; a URL the 403 recovery swapped in meanwhile is not
// renewed on top of the recovery — the timer renews it 30 minutes before its
// own expiry instead; the timer ends with its ctx.
//
// Mutants: the timer firing at the expiry itself (the first check sees no
// renewal at 1 h 31 min); the URL left uninstalled (installCredentials
// skipped — the URL stays v1); the retry dropped (a failed renewal waits the
// full cadence — no second call at +5 min); the swap check dropped (the swap
// row renews at once on top of the recovery); the ctx not watched (the
// bubble never ends).
func TestProactiveRefreshRenewsBeforeTheURLExpires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		var calls atomic.Int32
		var mu sync.Mutex
		answers := []bool{true, false, true, true} // installs, installs nothing, installs, installs
		d := NewSegmentDownloader(DownloaderOptions{
			BaseURL:   expiringURL(1, start.Add(2*time.Hour)),
			URLExpiry: testURLExpiry,
			OnProactiveRefresh: func() (string, string) {
				n := int(calls.Add(1))
				mu.Lock()
				defer mu.Unlock()
				if !answers[n-1] {
					return "", ""
				}
				return expiringURL(n+1, time.Now().Add(6*time.Hour)), fmt.Sprintf("tok-%d", n)
			},
		})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			d.runProactiveRefresh(ctx)
		}()

		step := func(dur time.Duration, wantCalls int32, wantVersion int) {
			t.Helper()
			time.Sleep(dur)
			synctest.Wait()
			if n := calls.Load(); n != wantCalls {
				t.Fatalf("at +%v: %d renewal(s), want %d", time.Since(start), n, wantCalls)
			}
			if v, _ := strconv.Atoi(mustQuery(t, d.getBaseURL(), "v")); v != wantVersion {
				t.Fatalf("at +%v: URL version %d, want %d", time.Since(start), v, wantVersion)
			}
		}
		step(89*time.Minute, 0, 1) // +1h29m: the URL expires at +2h, renewal due at +1h30m
		step(2*time.Minute, 1, 2)  // +1h31m: renewed
		if got := d.getPoToken(); got != "tok-1" {
			t.Errorf("token = %q, want the renewal's", got)
		}
		step(298*time.Minute, 1, 2) // +6h29m: the next is due 5 h after the renewal, at +6h30m
		step(2*time.Minute, 2, 2)   // +6h31m: asked, installed nothing
		step(5*time.Minute, 3, 4)   // +6h36m: asked again 5 min later (+6h35m), installed

		// A recovery swap is not renewed again when the old due comes: the swapped
		// URL's own expiry sets the next.
		time.Sleep(time.Hour) // +7h36m
		d.SetBaseURL(expiringURL(9, time.Now().Add(6*time.Hour)))
		step(4*time.Hour, 3, 9)    // +11h36m: the old due (+11h35m) passed, no renewal
		step(89*time.Minute, 3, 9) // +13h05m: the swapped URL expires at +13h36m
		step(2*time.Minute, 4, 5)  // +13h07m: renewed 30 min before it (+13h06m)

		cancel()
		<-done
	})
}

func mustQuery(t *testing.T, raw, key string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Query().Get(key)
}

// TestStartRunsTheProactiveRefresh pins the timer's wiring into Start: a
// live download whose URL expires 31 minutes in renews it a minute into the
// capture, and every segment after the renewal is fetched from the renewed
// URL — without one refused segment. The timer ends with the download,
// though the ctx Start was given does not.
//
// Mutants: Start not starting the timer (no request carries v=2); the
// timer not stopped when Start returns (a second renewal 5 h later).
func TestStartRunsTheProactiveRefresh(t *testing.T) {
	var mu sync.Mutex
	byVersion := map[string]int{}
	var firstV2 time.Duration = -1
	var t0 time.Time
	serveEngineInMemory(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		seq, err := strconv.Atoi(q.Get("sq"))
		if err != nil {
			w.WriteHeader(http.StatusOK)
			return
		}
		mu.Lock()
		byVersion[q.Get("v")]++
		if q.Get("v") == "2" && firstV2 < 0 {
			firstV2 = time.Since(t0)
		}
		mu.Unlock()
		time.Sleep(time.Second) // one segment a second, as a live edge serves them
		fmt.Fprintf(w, "seg-%05d;", seq)
	}))
	synctest.Test(t, func(t *testing.T) {
		var renewals atomic.Int32
		const endSeq = 179
		out := filepath.Join(t.TempDir(), "video_stream")
		mu.Lock()
		t0 = time.Now()
		mu.Unlock()
		d := NewSegmentDownloader(DownloaderOptions{
			BaseURL:    expiringURL(1, time.Now().Add(31*time.Minute)),
			OutputFile: out,
			EndSeq:     endSeq,
			MaxTimeout: time.Minute,
			URLExpiry:  testURLExpiry,
			OnProactiveRefresh: func() (string, string) {
				renewals.Add(1)
				return expiringURL(2, time.Now().Add(6*time.Hour)), ""
			},
		})
		// A ctx that outlives Start, so only Start can stop the timer.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if err := d.Start(ctx); err != nil {
			t.Fatalf("Start = %v", err)
		}
		// Past the next renewal's due: a timer still running would renew.
		time.Sleep(6 * time.Hour)
		synctest.Wait()
		wantSegments(t, out, 0, endSeq)
		mu.Lock()
		defer mu.Unlock()
		if renewals.Load() != 1 || byVersion["2"] == 0 {
			t.Fatalf("renewals %d, requests by URL version %v; want one renewal and the rest on v=2", renewals.Load(), byVersion)
		}
		// The renewal is due a minute in (expiry 31 min − 30 min); the first
		// request on it is the next segment after that.
		if firstV2 < time.Minute || firstV2 > time.Minute+5*time.Second {
			t.Errorf("first request on the renewed URL at +%v, want just past +1m", firstV2)
		}
	})
}
