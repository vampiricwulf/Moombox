package jobfilter

import (
	"math"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// ArchiveCutoff turns a hide_finished_age_days threshold into the instant a
// Finished job's updated_at must be strictly BEFORE to count as archived.
//
// The multiplication order is load-bearing. `time.Duration(days*24) *
// time.Hour` — what the three Go copies this replaces all did — converts the
// float to an INTEGER number of hours first, so every threshold below one
// hour collapses to zero and archives every Finished job immediately.
// config.FlexDuration accepts "0.02d" (≈29 minutes) and the JS twin,
// _evaluateArchiveBoundary in web/public/app.js, computes
// `ageDays * 86400 * 1000` ms exactly — so the server archived a job and
// stopped broadcasting it while the dashboard kept showing it for another 29
// minutes (WEB-8). Scaling the float by the whole day keeps every fraction.
//
// Callers with a negative threshold ("never archive") must not call this:
// the cutoff would land in the FUTURE and archive everything. IsArchivedAt
// carries that guard for single-job callers; loop callers check it once
// before computing the cutoff.
//
// A threshold that cannot be scaled into a time.Duration at all — NaN, ±Inf,
// or a magnitude beyond ~292 years — returns the ZERO time, which IsArchived
// reads as "nothing is archived". Converting such a float to time.Duration is
// implementation-defined (amd64 yields math.MinInt64), and the failure mode
// of a garbage cutoff is archiving every Finished job, so the fail-safe is to
// archive none.
func ArchiveCutoff(now time.Time, hideAgeDays float64) time.Time {
	ns := hideAgeDays * float64(24*time.Hour)
	if math.IsNaN(ns) || ns > float64(math.MaxInt64) || ns < float64(math.MinInt64) {
		return time.Time{}
	}
	return now.Add(-time.Duration(ns))
}

// IsArchived reports whether j is a Finished row old enough to be archived
// at the given cutoff. The boundary is EXCLUSIVE: updated_at must be strictly
// before the cutoff, so a job sitting exactly on it stays active — the same
// rule as the JS twin's `nowMs - t > cutoffMs` in
// _evaluateArchiveBoundary (web/public/app.js).
//
// Only Finished jobs archive — Cancelled and Error rows stay in the active
// list because they may still need attention — and a missing or unparseable
// updated_at is treated as active (never hide a job because its timestamp is
// malformed).
//
// FOUR callers, deliberately one predicate: the REST archived filter
// (internal/web/routes/jobs.go), the job_update broadcast gate and the
// full-list filter (cmd/moombox), and the TUI's archive bucket
// (internal/tui/task_list.go). An earlier hand-copied version of this logic
// suppressed broadcasts for jobs the list filter still showed.
func IsArchived(j *database.Job, cutoff time.Time) bool {
	if j.Status != database.StatusFinished || j.UpdatedAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, j.UpdatedAt)
	if err != nil {
		return false
	}
	return t.Before(cutoff)
}

// IsArchivedAt is the single-job form, carrying the "never archive" guard.
// Semantics, matching the JS twin exactly:
//
//   - hideAgeDays < 0 : never archive
//   - hideAgeDays == 0: archive every Finished job whose updated_at is
//     strictly in the past (the cutoff IS now)
//   - hideAgeDays > 0 : archive Finished jobs older than the threshold
//
// A NaN threshold falls through to ArchiveCutoff's zero time, which archives
// nothing — the same fail-safe as a negative one.
func IsArchivedAt(j *database.Job, hideAgeDays float64, now time.Time) bool {
	if hideAgeDays < 0 {
		return false
	}
	return IsArchived(j, ArchiveCutoff(now, hideAgeDays))
}
