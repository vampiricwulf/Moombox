package worker

import (
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// The schedule-probe cadence tapers with the time left to the scheduled
// start — a twelfth of it, clamped to [2 min, 30 min] — and is off for a
// stream with no schedule or an unreadable one (owner decision, 2026-10-14).
//
// Mutants: the floor dropped (a past-due start probes continuously); the
// divisor changed; the cap dropped (a start a day out waits two hours); the
// no-schedule arm probing.
func TestScheduleProbeInterval(t *testing.T) {
	now := time.Date(2026, 10, 14, 20, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	for _, tc := range []struct {
		name  string
		start string
		want  time.Duration
	}{
		{"past due", at(-40 * time.Minute), scheduleProbeMin},
		{"in 10 min", at(10 * time.Minute), scheduleProbeMin},
		{"in 24 min", at(24 * time.Minute), scheduleProbeMin},
		{"in 36 min", at(36 * time.Minute), 3 * time.Minute},
		{"in 1 h", at(time.Hour), 5 * time.Minute},
		{"in 3 h", at(3 * time.Hour), 15 * time.Minute},
		{"in 5 h", at(5 * time.Hour), 25 * time.Minute},
		{"in 6 h", at(6 * time.Hour), scheduleProbeMax},
		{"in 24 h", at(24 * time.Hour), scheduleProbeMax},
		{"no schedule", "", 0},
		{"unreadable", "soon", 0},
	} {
		if got := scheduleProbeInterval(tc.start, now); got != tc.want {
			t.Errorf("%s: interval = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A schedule probe that says the stream has begun stands in for the status
// probe; one that says anything else does not — above all not_a_stream,
// which a refused WEB call reads as and which the loop's default arm would
// end the job on.
//
// Mutant: the allow-list widened to "anything but upcoming".
func TestScheduleProbeOvertakesOnlyABegunStream(t *testing.T) {
	for _, tc := range []struct {
		status youtube.StreamStatus
		want   bool
	}{
		{youtube.StreamLive, true},
		{youtube.StreamVOD, true},
		{youtube.StreamPostLive, true},
		{youtube.StreamUpcoming, false},
		{youtube.StreamNotAStream, false},
		{youtube.StreamStatus(""), false},
	} {
		if got := scheduleProbeOvertakes(&youtube.VideoInfo{StreamStatus: tc.status}); got != tc.want {
			t.Errorf("%q: overtakes = %v, want %v", tc.status, got, tc.want)
		}
	}
}

// A start that moves TWICE is written twice, each move with its own "Schedule
// Changed": the loop hands updateJobMetadata a schedule-only VideoInfo (the
// probe reads nothing else), and change detection compares against the row
// as last synced, not as first written. A probe that repeats the current
// start writes nothing and sends nothing — also when it spells the same
// moment with another offset, as the watch page ("+00:00") and the offline
// slate ("Z") do for one stream.
//
// Mutants: the loop syncing scheduledStartTime but not the row; a
// schedule-only VideoInfo read as live (no reschedule notice); the start
// compared as text (the respelt repeat announces a reschedule to the same
// moment).
func TestRescheduleIsWrittenOnEveryMove(t *testing.T) {
	sp, rec, job := foundTestJob(t)
	sp.applyJobMetadata(job, &youtube.VideoInfo{Title: "T", ScheduledStartTime: "2026-10-15T03:00:00Z", IsUpcoming: true}, false, false)

	for i, tc := range []struct{ probe, wantRow string }{
		{"2026-10-15T03:15:00Z", "2026-10-15T03:15:00Z"},
		{"2026-10-15T03:15:00Z", "2026-10-15T03:15:00Z"},      // repeat
		{"2026-10-15T03:15:00+00:00", "2026-10-15T03:15:00Z"}, // the same moment respelt
		{"2026-10-15T03:30:00Z", "2026-10-15T03:30:00Z"},
	} {
		sp.updateJobMetadata(job, &youtube.VideoInfo{ScheduledStartTime: tc.probe, IsUpcoming: true}, true)
		row, _ := sp.db.GetJob(job.ID)
		if row.StreamStartTime != tc.wantRow {
			t.Errorf("after probe %d (%s): row start = %q, want %q", i, tc.probe, row.StreamStartTime, tc.wantRow)
		}
	}
	got := rec.ByEvent("rescheduled")
	if len(got) != 2 {
		t.Fatalf("recorded %d \"rescheduled\" embeds for two moves (a repeat and a respelling between), want 2: %+v", len(got), got)
	}
	ts := func(s string) string { return notifications.Timestamp(parseRFC3339(s)) }
	if v, _ := got[1].Field("Old Time"); v != ts("2026-10-15T03:15:00Z") {
		t.Errorf("second notice Old Time = %q, want the first move's start", v)
	}
	if v, _ := got[1].Field("New Time"); v != ts("2026-10-15T03:30:00Z") {
		t.Errorf("second notice New Time = %q, want the second move's start", v)
	}
}
