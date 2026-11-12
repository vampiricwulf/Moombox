package jobstate

import (
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestCookieRetryFor is the cookie-retry table (owner ruling R2-C1): the
// count shows on a row parked in COOKIES? on its credentials, and nowhere
// else; a count the 24 h window has outlived is gone.
//
// Mutants: the status check dropped; the membership check dropped; the
// window compared the wrong way round.
func TestCookieRetryFor(t *testing.T) {
	now := time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)
	recent, old := now.Add(-time.Hour), now.Add(-CookieRetryWindow)
	h := AdmissionHolds{CookieRetries: map[string]CookieRetry{
		"a": {Count: 2, Of: 3, Last: recent},
		"s": {Count: 3, Of: 3, Last: recent},
		"o": {Count: 2, Of: 3, Last: old},
		"z": {Count: 0, Of: 3, Last: recent},
	}}
	parked := func(id string) *database.Job {
		return &database.Job{ID: id, Status: database.StatusCookies, ParkReason: database.ParkReasonAuth}
	}
	cases := []struct {
		name string
		job  *database.Job
		want CookieRetry
	}{
		{"parked with retries used", parked("a"), CookieRetry{Count: 2, Of: 3, Last: recent}},
		{"parked with all used", parked("s"), CookieRetry{Count: 3, Of: 3, Last: recent}},
		{"window passed", parked("o"), CookieRetry{}},
		{"none used", parked("z"), CookieRetry{}},
		{"not counted", parked("x"), CookieRetry{}},
		{"unclassified park counts", &database.Job{ID: "a", Status: database.StatusCookies}, CookieRetry{Count: 2, Of: 3, Last: recent}},
		{"membership park", &database.Job{ID: "a", Status: database.StatusCookies, ParkReason: database.ParkReasonMembership}, CookieRetry{}},
		{"not parked", &database.Job{ID: "a", Status: database.StatusUpcoming}, CookieRetry{}},
		{"nil", nil, CookieRetry{}},
	}
	for _, c := range cases {
		if got := h.CookieRetryFor(c.job, now); got != c.want {
			t.Errorf("%s: CookieRetryFor = %+v, want %+v", c.name, got, c.want)
		}
	}
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
}
