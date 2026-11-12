package jobstate

import (
	"fmt"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// MaxCookieAutoRetries is how many automatic retries one job gets out of an
// auth park in COOKIES? (owner ruling R2-C1): each time a cookie refresh, an
// import or a setup ends with the platform verified and the job is resumed
// by it, one is used. With none left the job stays parked and asks for no
// refresh of its own: the session verifies and the platform still refuses
// the video, so the cookies are not the cause.
const MaxCookieAutoRetries = 3

// CookieRetryWindow is how long after its last automatic cookie retry a
// job's count stands; past it the job has its full budget again.
const CookieRetryWindow = 24 * time.Hour

// CookieRetry is one job's automatic cookie retries: how many it has used,
// of how many, and when the last one was. The worker keeps it in memory
// (worker.DownloadWorker.NoteCookieRetry), so a restart forgets it.
type CookieRetry struct {
	Count int       `json:"count"`
	Of    int       `json:"of"`
	Last  time.Time `json:"last"`
}

// CookieRetryFor is job's automatic cookie retries at now, for a row parked
// in COOKIES? on its credentials: the zero value for any other row (a
// membership park is settled by an account change, not by retries), for a
// job that has used none, and once CookieRetryWindow has passed since the
// last one. The dashboard's twin is utils.js cookieRetry.
func (h AdmissionHolds) CookieRetryFor(job *database.Job, now time.Time) CookieRetry {
	if job == nil || job.Status != database.StatusCookies || job.ParkReason == database.ParkReasonMembership {
		return CookieRetry{}
	}
	r, ok := h.CookieRetries[job.ID]
	if !ok || r.Count <= 0 || !now.Before(r.Last.Add(CookieRetryWindow)) {
		return CookieRetry{}
	}
	return r
}

// Spent reports whether every automatic retry is used: nothing resumes the
// job by itself until the window passes or the operator restarts it.
func (c CookieRetry) Spent() bool {
	return c.Count > 0 && c.Count >= c.Of
}

// Text is the parked row's line in both UIs (the dashboard's twin is
// utils.js cookieRetryText); "" when the job has used no retry.
func (c CookieRetry) Text() string {
	switch {
	case c.Count <= 0:
		return ""
	case c.Spent():
		return fmt.Sprintf("Cookie retries used up (%d of %d): no more automatic retries", c.Count, c.Of)
	}
	return fmt.Sprintf("Cookie retry %d of %d used", c.Count, c.Of)
}
