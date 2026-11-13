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
