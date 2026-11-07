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

// TestPlayerFetchRetryBounds pins the production values (owner decision
// PR-R3: a cold player GET is tried 3 times before it counts as systemic).
func TestPlayerFetchRetryBounds(t *testing.T) {
	if playerFetchAttempts != 3 {
		t.Errorf("playerFetchAttempts = %d, want 3", playerFetchAttempts)
	}
	if playerFetchRetryDelay != 2*time.Second {
		t.Errorf("playerFetchRetryDelay = %v, want 2s", playerFetchRetryDelay)
	}
}

// A cold player GET — nothing cached to serve instead — that fails is tried
// again, up to three times, before Fetch fails with ErrPlayerJSFetch (owner
// decision PR-R3). One GET decided it: a CDN blip on the first fetch of a
// newly rotated player, the moment several streams going live at once meet
// it, sent every one of them to the fallback path for the whole run.
//
// A definite answer about the player itself — a 404 for a player YouTube
// pulled, a 403 — is not retried: it says the same every time, and the
// retries only held setup (and a live capture's switch back) for the pauses.
// A 429 says the CDN is struggling, and is.
//
// Mutants: Fetch calling fetchInternal once (the blip that clears on the
// second GET fails it); the bound raised or dropped (the dead CDN is asked
// more than three times); every ErrPlayerJSFetch retried (the 404 and 403
// rows are asked three times).
func TestAColdPlayerFetchIsTriedThreeTimes(t *testing.T) {
	cases := []struct {
		name     string
		failures int32 // GETs answered status before one is served
		status   int
		wantGets int32
		wantErr  bool
	}{
		{"a blip that clears on the second GET", 1, http.StatusServiceUnavailable, 2, false},
		{"one that clears on the third", 2, http.StatusServiceUnavailable, 3, false},
		{"a CDN that stays down", 99, http.StatusServiceUnavailable, 3, true},
		{"a CDN that rate-limits, then serves", 1, http.StatusTooManyRequests, 2, false},
		{"a player the CDN says is gone", 99, http.StatusNotFound, 1, true},
		{"a player the CDN refuses", 99, http.StatusForbidden, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var gets atomic.Int32
				orig := playerHTTPClient
				playerHTTPClient = httpxtest.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if gets.Add(1) <= tc.failures {
						w.WriteHeader(tc.status)
						return
					}
					_, _ = w.Write([]byte("// player JS"))
				}))
				defer func() { playerHTTPClient = orig }()
				pc, err := NewPlayerCache(t.TempDir(), nopPlayerCacheLogger{})
				if err != nil {
					t.Fatal(err)
				}
				js, _, err := pc.Fetch(context.Background(), "https://www.youtube.com/s/player/abcd1234/player_ias.vflset/en_US/base.js")
				if tc.wantErr {
					if !errors.Is(err, ErrPlayerJSFetch) {
						t.Errorf("Fetch = %v, want ErrPlayerJSFetch once every attempt failed", err)
					}
				} else if err != nil || js != "// player JS" {
					t.Errorf("Fetch = %q, %v; want the player once a GET is served", js, err)
				}
				if got := gets.Load(); got != tc.wantGets {
					t.Errorf("GETs = %d, want %d", got, tc.wantGets)
				}
			})
		})
	}
}

// A caller whose own context ends while the leader is still retrying a
// player CDN that is down leaves then, with its context's error: the leader
// runs detached (coalesced waiters from other jobs must not be poisoned by
// one caller's cancel), and with singleflight's blocking Do a cancelled job,
// or a worker shutting down, waited out every attempt — three 30s
// round-trips and the pauses between. The leader still finishes on its own.
//
// Mutant: Fetch waiting on the singleflight result alone (Do, or DoChan
// without the ctx.Done arm): it returns ErrPlayerJSFetch after the leader's
// two pauses, 4s, not the caller's 1s.
func TestAColdPlayerFetchCallerLeavesOnItsOwnContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var gets atomic.Int32
		orig := playerHTTPClient
		playerHTTPClient = httpxtest.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gets.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer func() { playerHTTPClient = orig }()
		pc, err := NewPlayerCache(t.TempDir(), nopPlayerCacheLogger{})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		start := time.Now()
		_, _, err = pc.Fetch(ctx, "https://www.youtube.com/s/player/abcd1234/player_ias.vflset/en_US/base.js")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Fetch = %v, want the caller's own deadline", err)
		}
		if got := time.Since(start); got != time.Second {
			t.Errorf("Fetch returned after %v, want the caller's 1s", got)
		}
		synctest.Wait()
		time.Sleep(2 * playerFetchRetryDelay)
		synctest.Wait()
		if got := gets.Load(); got != playerFetchAttempts {
			t.Errorf("GETs = %d, want the detached leader's %d", got, playerFetchAttempts)
		}
	})
}
