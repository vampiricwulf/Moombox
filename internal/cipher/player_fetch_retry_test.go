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
// Mutants: Fetch calling fetchInternal once (the blip that clears on the
// second GET fails it); the bound raised or dropped (the dead CDN is asked
// more than three times).
func TestAColdPlayerFetchIsTriedThreeTimes(t *testing.T) {
	cases := []struct {
		name     string
		failures int32 // GETs answered 503 before one is served
		wantGets int32
		wantErr  bool
	}{
		{"a blip that clears on the second GET", 1, 2, false},
		{"one that clears on the third", 2, 3, false},
		{"a CDN that stays down", 99, 3, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var gets atomic.Int32
				orig := playerHTTPClient
				playerHTTPClient = httpxtest.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if gets.Add(1) <= tc.failures {
						w.WriteHeader(http.StatusServiceUnavailable)
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
