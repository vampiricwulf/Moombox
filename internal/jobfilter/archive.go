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
// The function is TOTAL: every threshold that is not a real, finite,
// non-negative number of days returns the ZERO time, which IsArchived reads
// as "nothing is archived".
//
//   - A NEGATIVE threshold means "never archive". No config produces one
//     today (Normalize and both settings UIs reject it), but scaling it
//     would put the cutoff in the FUTURE and archive every Finished job, so
//     it returns the zero time here rather than relying on each caller to
//     check first (every one of them does today; a fourth that forgot would
//     be CORE-8's failure again).
//   - NaN, ±Inf, or a magnitude beyond ~292 years cannot be scaled into a
//     time.Duration at all, and the conversion is implementation-defined:
//     amd64 yields math.MinInt64, whose negation overflows back to
//     math.MinInt64 and lands the cutoff in the year 1734 — archiving
//     nothing; arm64's saturating conversion turns NaN into 0, putting the
//     cutoff at `now` — archiving EVERYTHING. The zero time is what makes
//     all three targets agree, on the safe answer.
func ArchiveCutoff(now time.Time, hideAgeDays float64) time.Time {
	if hideAgeDays < 0 {
		return time.Time{}
	}
	ns := hideAgeDays * float64(24*time.Hour)
	// >=, not >: float64(math.MaxInt64) rounds UP to 2^63, which is itself
	// out of int64 range, so a value exactly there must be refused too.
	if math.IsNaN(ns) || ns >= float64(math.MaxInt64) || ns < float64(math.MinInt64) {
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
// nothing — the same fail-safe as a negative one. The negative branch below
// is documentation rather than protection now that ArchiveCutoff is total: it
// states the knob at the place callers read it.
func IsArchivedAt(j *database.Job, hideAgeDays float64, now time.Time) bool {
	if hideAgeDays < 0 {
		return false
	}
	return IsArchived(j, ArchiveCutoff(now, hideAgeDays))
}
