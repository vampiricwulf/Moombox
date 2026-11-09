package jobstate

import (
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestQueuedHoldPrecedence is the hold-reason table (owner ruling R-D3): each
// gate on its own, every pair resolved by HoldPrecedence (the sweep's
// early-return order), Twitch rows through the YouTube rate-limit hold, a
// row's own backoff by its kind, and run-out holds ignored.
//
// Mutants: the precedence reordered (disk ahead of offline); the platform
// check on the rate limit dropped; an expired own hold still reported.
func TestQueuedHoldPrecedence(t *testing.T) {
	now := time.Date(2026, 11, 9, 14, 0, 0, 0, time.UTC)
	later, past := now.Add(30*time.Minute), now.Add(-time.Minute)
	yt := &database.Job{ID: "y", Platform: "youtube", Status: database.StatusQueued}
	tw := &database.Job{ID: "t", Platform: "twitch", Status: database.StatusQueued}
	retry := map[string]BacklogHold{"y": {Attempt: 2, Of: 3, Until: later}, "t": {Attempt: 1, Of: 3, Until: later}}
	cases := []struct {
		name string
		h    AdmissionHolds
		job  *database.Job
		want QueuedHold
	}{
		{"nothing", AdmissionHolds{}, yt, QueuedHold{}},
		{"offline", AdmissionHolds{Offline: true}, yt, QueuedHold{Reason: HoldOffline}},
		{"offline over everything", AdmissionHolds{Offline: true, DiskHeld: true, RateLimitedUntil: later, Jobs: retry}, yt, QueuedHold{Reason: HoldOffline}},
		{"disk over rate limit and retry", AdmissionHolds{DiskHeld: true, RateLimitedUntil: later, Jobs: retry}, yt, QueuedHold{Reason: HoldDisk}},
		{"disk holds Twitch too", AdmissionHolds{DiskHeld: true}, tw, QueuedHold{Reason: HoldDisk}},
		{"rate limit over retry", AdmissionHolds{RateLimitedUntil: later, Jobs: retry}, yt, QueuedHold{Reason: HoldRateLimit, Until: later}},
		{"rate limit does not hold Twitch", AdmissionHolds{RateLimitedUntil: later, Jobs: retry}, tw, QueuedHold{Reason: HoldRetry, Until: later, Attempt: 1, Of: 3}},
		{"rate limit run out", AdmissionHolds{RateLimitedUntil: past}, yt, QueuedHold{}},
		{"own retry", AdmissionHolds{Jobs: retry}, yt, QueuedHold{Reason: HoldRetry, Until: later, Attempt: 2, Of: 3}},
		{"own rate-limit requeue", AdmissionHolds{Jobs: map[string]BacklogHold{"y": {Until: later}}}, yt, QueuedHold{Reason: HoldRateLimit, Until: later}},
		{"own hold run out", AdmissionHolds{Jobs: map[string]BacklogHold{"y": {Attempt: 1, Of: 3, Until: past}}}, yt, QueuedHold{}},
		{"not Queued", AdmissionHolds{Offline: true}, &database.Job{ID: "y", Status: database.StatusUpcoming}, QueuedHold{}},
	}
	for _, c := range cases {
		if got := c.h.For(c.job, now); got != c.want {
			t.Errorf("%s: For = %+v, want %+v", c.name, got, c.want)
		}
	}
	at := time.Date(2026, 11, 9, 14, 5, 0, 0, time.Local)
	for _, c := range []struct {
		q    QueuedHold
		want string
	}{
		{QueuedHold{}, "Waiting in queue..."},
		{QueuedHold{Reason: HoldOffline}, "Held: offline"},
		{QueuedHold{Reason: HoldDisk}, "Held: disk full"},
		{QueuedHold{Reason: HoldRateLimit, Until: at}, "Held: YouTube rate limit until 14:05"},
		{QueuedHold{Reason: HoldRetry, Until: at, Attempt: 2, Of: 3}, "Retry 2 of 3 at 14:05"},
	} {
		if got := c.q.Text(); got != c.want {
			t.Errorf("Text(%+v) = %q, want %q", c.q, got, c.want)
		}
	}
}
