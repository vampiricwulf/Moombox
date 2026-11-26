package notifications

import (
	"io"
	"net/http"
	"slices"
	"testing"
	"time"
)

// A 429 the delivery ENDS on — Retry-After past the cap, missing, or the
// ladder's last attempt — used to leave the bucket unarmed: noteBucket skips
// every 429, so the queue's next item POSTed straight into the same limit,
// and the one after it, and a long Retry-After dropped every queued alert
// within milliseconds. The give-up now arms the bucket, so the next item
// waits. A 429 the ladder honours and gets past arms nothing.
//
// Mutant: noteRateLimitGiveUp returning at once — no bucket wait after the
// give-up.
func TestA429TheDeliveryGivesUpOnArmsTheBucket(t *testing.T) {
	for _, tc := range []struct {
		name       string
		retryAfter string
		resetAfter string
		atLeast    time.Duration
	}{
		{"Retry-After past the wait cap", "90", "", discordRetryAfterCap - time.Second},
		{"no Retry-After", "", "", discordRetryAfterCap - time.Second},
		{"Reset-After only", "", "20", 19 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDiscord(t, func(_ int, _ recordedReq, rw http.ResponseWriter) {
				if tc.retryAfter != "" {
					rw.Header().Set("Retry-After", tc.retryAfter)
				}
				if tc.resetAfter != "" {
					rw.Header().Set("X-RateLimit-Reset-After", tc.resetAfter)
				}
				rw.WriteHeader(http.StatusTooManyRequests)
				io.WriteString(rw, `{"message":"You are being rate limited."}`)
			})
			d := &DiscordWebhook{URL: f.URL()}
			if err := d.Send(One("t", "d", 0, nil, SendOptions{})); err == nil {
				t.Fatal("a refused send reported success")
			}
			if w := d.bucketWait(); w < tc.atLeast || w > discordRetryAfterCap+time.Second {
				t.Errorf("bucket wait after the give-up = %s, want %s..%s", w, tc.atLeast, discordRetryAfterCap)
			}
		})
	}
}

// The ladder honoured a short Retry-After and the retry went through: the
// limit is over, and arming the bucket would hold the next item for nothing.
func TestA429TheLadderGetsPastArmsNothing(t *testing.T) {
	f := newFakeDiscord(t, func(n int, _ recordedReq, rw http.ResponseWriter) {
		if n == 0 {
			rw.Header().Set("Retry-After", "0.05")
			rw.WriteHeader(http.StatusTooManyRequests)
			return
		}
		rw.WriteHeader(http.StatusNoContent)
	})
	d := &DiscordWebhook{URL: f.URL()}
	if err := d.Send(One("t", "d", 0, nil, SendOptions{})); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if w := d.bucketWait(); w > 0 {
		t.Errorf("bucket wait = %s after a delivered retry, want none", w)
	}
}

// R4-9: a 429 asking for more than discordRetryAfterCap but no more than
// discordRetryAfterWaitCap is waited out once, so the notification at the
// head of the queue is delivered instead of dropped, wherever in the ladder it
// lands (after a 5xx's 2 s backoff too). A second long wait in the same ladder
// is past the budget and gives up.
//
// Mutants: the 429 check back on discordRetryAfterCap (no wait, delivered
// fails); the budget left at discordMaxSleepTotal (the 45 s wait gives up);
// the long wait allowed only as the ladder's first sleep (the 5xx row drops).
func TestA429RetryAfterUpToTheWaitCapIsWaitedOutOnce(t *testing.T) {
	for _, tc := range []struct {
		name      string
		first5xx  bool // the first request is answered 503
		limited   int  // how many requests are then answered 429 before a 204
		delivered bool
		hits      int
		sleeps    []time.Duration
	}{
		{"one 45 s wait, then delivered", false, 1, true, 2, []time.Duration{45 * time.Second}},
		{"a second 45 s wait gives up", false, 2, false, 2, []time.Duration{45 * time.Second}},
		{"a 45 s wait after a 5xx backoff, then delivered", true, 1, true, 3,
			[]time.Duration{discordRetryBackoff[0], 45 * time.Second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sleeps := recordDiscordSleeps(t)
			hits := 0
			f := newFakeDiscord(t, func(n int, _ recordedReq, rw http.ResponseWriter) {
				hits = n + 1
				if tc.first5xx {
					if n == 0 {
						rw.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					n--
				}
				if n < tc.limited {
					rw.Header().Set("Retry-After", "45")
					rw.WriteHeader(http.StatusTooManyRequests)
					return
				}
				rw.WriteHeader(http.StatusNoContent)
			})
			d := &DiscordWebhook{URL: f.URL()}
			err := d.Send(One("t", "d", 0, nil, SendOptions{}))
			if (err == nil) != tc.delivered {
				t.Fatalf("Send err = %v, want delivered=%v", err, tc.delivered)
			}
			if hits != tc.hits {
				t.Errorf("requests = %d, want %d", hits, tc.hits)
			}
			if got := sleeps(); !slices.Equal(got, tc.sleeps) {
				t.Errorf("sleeps = %v, want %v", got, tc.sleeps)
			}
		})
	}
}
