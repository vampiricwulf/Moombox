package notifications

import (
	"io"
	"net/http"
	"testing"
	"time"
)

// A 429 the delivery ENDS on — Retry-After past the cap, missing, or the
// ladder's last attempt — used to leave the bucket unarmed: noteBucket skips
// every 429, so the queue's next item POSTed straight into the same limit,
// and the one after it, and a Retry-After of 45 s dropped every queued alert
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
		{"Retry-After past the cap", "45", "", discordRetryAfterCap - time.Second},
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
