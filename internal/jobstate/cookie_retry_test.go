package jobstate

import (
	"testing"
	"time"
)

// TestCookieRetryTextAndSpent: the line both UIs show for a job's automatic
// cookie retries (owner ruling R2-C1), and when the budget counts as spent.
// The rule for WHICH row shows a count is CookieRetryFor's, checked on both
// the Go and the JS side by TestCookieRetryParity (cmd/moombox) over
// job-state-parity.json; only the nil job, which a fixture row cannot hold,
// is checked here.
//
// Mutants: Spent true below the budget; Text naming the count for none used;
// the nil guard dropped from CookieRetryFor (a panic).
func TestCookieRetryTextAndSpent(t *testing.T) {
	for _, c := range []struct {
		r     CookieRetry
		spent bool
		want  string
	}{
		{CookieRetry{}, false, ""},
		{CookieRetry{Count: 1, Of: 3}, false, "Cookie retry 1 of 3 used"},
		{CookieRetry{Count: 3, Of: 3}, true, "Cookie retries used up (3 of 3): no more automatic retries"},
	} {
		if got := c.r.Text(); got != c.want || c.r.Spent() != c.spent {
			t.Errorf("%+v: Text = %q, Spent = %v; want %q, %v", c.r, got, c.r.Spent(), c.want, c.spent)
		}
	}
	h := AdmissionHolds{CookieRetries: map[string]CookieRetry{"": {Count: 2, Of: 3, Last: time.Now()}}}
	if got := h.CookieRetryFor(nil, time.Now()); got != (CookieRetry{}) {
		t.Errorf("CookieRetryFor(nil) = %+v, want none", got)
	}
}
