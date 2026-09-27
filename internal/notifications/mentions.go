package notifications

import (
	"net/url"
	"strings"
)

// JobDeepLink returns the dashboard URL that opens a job's details, or "" when
// no public_url is configured.
//
// The shape is the SPA's own: `#job=<id>` is what the dashboard parses on
// load, so the link opens the job's details panel rather than the job list.
// config.ValidatePublicURL already trims a trailing slash from the stored
// value; the trim here is defensive, for a value that reached the manager
// through a path that skipped normalisation and would otherwise produce
// "https://host//#job=…".
func JobDeepLink(publicURL, jobID string) string {
	if publicURL == "" || jobID == "" {
		return ""
	}
	return strings.TrimSuffix(publicURL, "/") + "/#job=" + url.PathEscape(jobID)
}
