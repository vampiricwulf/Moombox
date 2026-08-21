package main

import (
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/monitor"
)

// TestResumeOnRedetect pins the guard matrix for auto-resuming a preserved
// job on live re-detection (field case 2026-08-20, job BJUz0SzJP_c: a
// restarted broadcast was re-detected live every ~15s for hours and silently
// dropped every time). Exactly one combination resumes: Finished, flagged
// incomplete_tail, staging files still present on disk, a Broadcast
// disposition, and outside the 5-minute cooldown. Every other axis —
// still-active status, a human Cancel, a Finished job with no preserved
// tail, staging that vanished (mirrors the human /resume route's own gate),
// VOD re-detection, or a hot cooldown — must keep today's silent drop.
func TestResumeOnRedetect(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	longAgo := now.Add(-time.Hour)
	justNow := now.Add(-time.Minute)

	finishedFlagged := &database.Job{Status: database.StatusFinished, IncompleteTail: true}
	finishedUnflagged := &database.Job{Status: database.StatusFinished, IncompleteTail: false}
	cancelledFlagged := &database.Job{Status: database.StatusCancelled, IncompleteTail: true}
	downloadingFlagged := &database.Job{Status: database.StatusDownloading, IncompleteTail: true}
	errorFlagged := &database.Job{Status: database.StatusError, IncompleteTail: true}
	liveFlagged := &database.Job{Status: database.StatusLive, IncompleteTail: true}

	cases := []struct {
		name           string
		existing       *database.Job
		disposition    monitor.JobDisposition
		stagingExists  bool
		lastAutoResume time.Time
		want           bool
	}{
		{
			name:           "Finished + flagged + staging present + broadcast + cooldown clear resumes",
			existing:       finishedFlagged,
			disposition:    monitor.DispositionBroadcast,
			stagingExists:  true,
			lastAutoResume: longAgo,
			want:           true,
		},
		{
			name:           "Finished + flagged + staging present + broadcast + never resumed before resumes",
			existing:       finishedFlagged,
			disposition:    monitor.DispositionBroadcast,
			stagingExists:  true,
			lastAutoResume: time.Time{},
			want:           true,
		},
		{
			name:           "nil existing job never resumes",
			existing:       nil,
			disposition:    monitor.DispositionBroadcast,
			stagingExists:  true,
			lastAutoResume: longAgo,
			want:           false,
		},
		{
			name:           "Cancelled is a human decision — not overridden",
			existing:       cancelledFlagged,
			disposition:    monitor.DispositionBroadcast,
			stagingExists:  true,
			lastAutoResume: longAgo,
			want:           false,
		},
		{
			name:           "still Downloading never resumes",
			existing:       downloadingFlagged,
			disposition:    monitor.DispositionBroadcast,
			stagingExists:  true,
			lastAutoResume: longAgo,
			want:           false,
		},
		{
			name:           "Error status never resumes",
			existing:       errorFlagged,
			disposition:    monitor.DispositionBroadcast,
			stagingExists:  true,
			lastAutoResume: longAgo,
			want:           false,
		},
		{
			name:           "Live status never resumes",
			existing:       liveFlagged,
			disposition:    monitor.DispositionBroadcast,
			stagingExists:  true,
			lastAutoResume: longAgo,
			want:           false,
		},
		{
			name:           "Finished but no preserved tail never resumes",
			existing:       finishedUnflagged,
			disposition:    monitor.DispositionBroadcast,
			stagingExists:  true,
			lastAutoResume: longAgo,
			want:           false,
		},
		{
			name:           "Finished + flagged but VOD disposition never resumes",
			existing:       finishedFlagged,
			disposition:    monitor.DispositionNewVOD,
			stagingExists:  true,
			lastAutoResume: longAgo,
			want:           false,
		},
		{
			name:           "Finished + flagged but BacklogVOD disposition never resumes",
			existing:       finishedFlagged,
			disposition:    monitor.DispositionBacklogVOD,
			stagingExists:  true,
			lastAutoResume: longAgo,
			want:           false,
		},
		{
			name:           "cooldown hot (re-detected 1 minute after a prior auto-resume) suppresses",
			existing:       finishedFlagged,
			disposition:    monitor.DispositionBroadcast,
			stagingExists:  true,
			lastAutoResume: justNow,
			want:           false,
		},
		{
			name:           "cooldown boundary exactly 5 minutes resumes (>=)",
			existing:       finishedFlagged,
			disposition:    monitor.DispositionBroadcast,
			stagingExists:  true,
			lastAutoResume: now.Add(-5 * time.Minute),
			want:           true,
		},
		{
			// The staging-existence guard: otherwise-eligible job, but the
			// preserved staging directory is gone (manual deletion, or the
			// staging_dir setting was reconfigured since the Finished
			// write). Must fall through to the silent drop — same as a
			// flagless Finished job — not masquerade a fresh empty-staging
			// restart as a resume. Mirrors the human /resume route's own
			// worker.HasStagingFiles gate (internal/web/routes/jobs.go).
			name:           "Finished + flagged but staging vanished never resumes",
			existing:       finishedFlagged,
			disposition:    monitor.DispositionBroadcast,
			stagingExists:  false,
			lastAutoResume: longAgo,
			want:           false,
		},
		{
			name:           "staging vanished + cooldown clear + never resumed before still never resumes",
			existing:       finishedFlagged,
			disposition:    monitor.DispositionBroadcast,
			stagingExists:  false,
			lastAutoResume: time.Time{},
			want:           false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resumeOnRedetect(tc.existing, tc.disposition, tc.stagingExists, tc.lastAutoResume, now); got != tc.want {
				t.Errorf("resumeOnRedetect = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestResumeOnRedetectGuardsAreLoadBearing spot-checks that each guard in
// resumeOnRedetect is actually enforced — mutating away any single guard
// would flip a false case to true. Regression protection against a future
// edit accidentally dropping a condition (e.g. "&&" -> "||", or a check
// silently deleted) while every case above still independently reads green
// against a coincidentally-permissive implementation.
func TestResumeOnRedetectGuardsAreLoadBearing(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	longAgo := now.Add(-time.Hour)

	base := &database.Job{Status: database.StatusFinished, IncompleteTail: true}
	if !resumeOnRedetect(base, monitor.DispositionBroadcast, true, longAgo, now) {
		t.Fatal("baseline true case must resume — test setup is wrong")
	}

	// Drop the status guard: a Cancelled job with the flag must NOT resume.
	cancelled := &database.Job{Status: database.StatusCancelled, IncompleteTail: true}
	if resumeOnRedetect(cancelled, monitor.DispositionBroadcast, true, longAgo, now) {
		t.Error("status guard not enforced: Cancelled job resumed")
	}

	// Drop the IncompleteTail guard: a Finished job without the flag must
	// NOT resume (this is what distinguishes an ordinary completed archive
	// from one that preserved staging for resume).
	unflagged := &database.Job{Status: database.StatusFinished, IncompleteTail: false}
	if resumeOnRedetect(unflagged, monitor.DispositionBroadcast, true, longAgo, now) {
		t.Error("IncompleteTail guard not enforced: unflagged Finished job resumed")
	}

	// Drop the disposition guard: a VOD re-detection of the same preserved
	// job must NOT auto-resume (only a live broadcast re-detection should).
	if resumeOnRedetect(base, monitor.DispositionNewVOD, true, longAgo, now) {
		t.Error("disposition guard not enforced: NewVOD resumed")
	}

	// Drop the staging-existence guard: an otherwise-eligible job whose
	// preserved staging is gone must NOT resume — this is the guard that
	// mirrors the human /resume route's own HasStagingFiles gate.
	if resumeOnRedetect(base, monitor.DispositionBroadcast, false, longAgo, now) {
		t.Error("staging-existence guard not enforced: vanished staging resumed")
	}

	// Drop the cooldown guard: a re-detection seconds after a prior
	// auto-resume must NOT fire again.
	if resumeOnRedetect(base, monitor.DispositionBroadcast, true, now.Add(-time.Second), now) {
		t.Error("cooldown guard not enforced: hot cooldown resumed")
	}

	// Drop the nil guard: a nil existing job must NOT resume.
	if resumeOnRedetect(nil, monitor.DispositionBroadcast, true, longAgo, now) {
		t.Error("nil guard not enforced: nil existing job resumed")
	}
}
