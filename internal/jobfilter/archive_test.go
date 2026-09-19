package jobfilter

import (
	"math"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// The threshold is a float number of DAYS and every fraction of it counts.
// The three Go copies this replaces wrote time.Duration(days*24)*time.Hour,
// which truncates to whole HOURS before multiplying — so 0.02d (≈29 min, a
// value FlexDuration accepts) became a zero cutoff and archived every
// Finished job immediately, while the JS twin
// (_evaluateArchiveBoundary in web/public/app.js) computed
// ageDays*86400*1000 ms exactly (WEB-8).
//
// Mutant: time.Duration(hideAgeDays*24) * time.Hour — the 0.02 and 0.5 rows
// return a zero-length window.
func TestArchiveCutoffKeepsSubHourFractions(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		days float64
		want time.Duration
	}{
		{0, 0},
		{0.02, 28*time.Minute + 48*time.Second}, // 0.02 * 86400 s
		{0.5, 12 * time.Hour},
		{1.5, 36 * time.Hour},
		{30, 720 * time.Hour},
	} {
		if got := now.Sub(ArchiveCutoff(now, tc.days)); got != tc.want {
			t.Errorf("ArchiveCutoff(%v): window = %v, want %v", tc.days, got, tc.want)
		}
	}
}

// A threshold that cannot be scaled into a time.Duration must not produce a
// garbage cutoff. Converting a NaN or overflowing float64 to time.Duration is
// implementation-defined (amd64 yields math.MinInt64), which would put the
// cutoff ~292 years in the FUTURE and archive every Finished job — the exact
// failure mode this row exists to prevent. The zero time is the fail-safe:
// IsArchived compares with Before, and no parsed timestamp precedes year 1.
//
// Mutant: dropping the finite check from ArchiveCutoff — the NaN, +Inf and
// 1e12 rows return a cutoff after now and archive the year-old job.
func TestArchiveCutoffGuardsNonFiniteThresholds(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	old := &database.Job{
		Status:    database.StatusFinished,
		UpdatedAt: now.Add(-365 * 24 * time.Hour).Format(time.RFC3339),
	}
	for _, days := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), 1e12, -1e12} {
		cutoff := ArchiveCutoff(now, days)
		if !cutoff.IsZero() {
			t.Errorf("ArchiveCutoff(%v) = %v, want the zero time", days, cutoff)
		}
		if IsArchived(old, cutoff) {
			t.Errorf("ArchiveCutoff(%v): a year-old Finished job must not archive", days)
		}
		if IsArchivedAt(old, days, now) {
			t.Errorf("IsArchivedAt(%v): a year-old Finished job must not archive", days)
		}
	}
}

// IsArchived is the one classification: Finished only, a parseable
// updated_at only, strictly before the cutoff.
//
// Mutant: !t.After(cutoff) instead of t.Before(cutoff) — the "exactly at the
// cutoff" row flips.
func TestIsArchivedRules(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	cutoff := ArchiveCutoff(now, 0.5) // 12 hours

	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }

	for _, tc := range []struct {
		name string
		job  *database.Job
		want bool
	}{
		{"finished 13h ago", &database.Job{Status: database.StatusFinished, UpdatedAt: at(13 * time.Hour)}, true},
		{"finished 11h ago", &database.Job{Status: database.StatusFinished, UpdatedAt: at(11 * time.Hour)}, false},
		{"finished exactly at the cutoff", &database.Job{Status: database.StatusFinished, UpdatedAt: at(12 * time.Hour)}, false},
		{"cancelled 13h ago", &database.Job{Status: database.StatusCancelled, UpdatedAt: at(13 * time.Hour)}, false},
		{"finished, no timestamp", &database.Job{Status: database.StatusFinished}, false},
		{"finished, unparseable timestamp", &database.Job{Status: database.StatusFinished, UpdatedAt: "yesterday"}, false},
	} {
		if got := IsArchived(tc.job, cutoff); got != tc.want {
			t.Errorf("%s: IsArchived = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A negative threshold means "never archive" — the single-job form must not
// turn it into a cutoff in the FUTURE, which would archive everything.
//
// Mutant: dropping the hideAgeDays < 0 guard from IsArchivedAt.
func TestIsArchivedAtNeverArchivesOnANegativeThreshold(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	j := &database.Job{Status: database.StatusFinished, UpdatedAt: now.Add(-365 * 24 * time.Hour).Format(time.RFC3339)}
	if IsArchivedAt(j, -1, now) {
		t.Error("a negative threshold must never archive")
	}
	if !IsArchivedAt(j, 0, now) {
		t.Error("a zero threshold archives every Finished job with a past updated_at")
	}
}
