package worker

import (
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestFilenamesUseTheHostsLocalDate: both places the worker names a
// recording — buildJobContext when the job starts and resolveFreshFilename at
// mux time — resolve ${start_date}/${start_time} in the host's zone
// (time.Local; TZ in Docker), not in the UTC every stored time is written in.
// With TZ=America/Los_Angeles a Twitch stream that went live at 20:30 on Oct
// 16 ("2026-10-17T03:30:00Z") was filed under Oct 17, 03:30; so was a
// YouTube one ("+00:00"); and a row with no start time fell back to its
// created_at in UTC.
//
// Mutant: drop `now = now.In(time.Local)` in config.ResolveTemplate (every
// row resolves in UTC).
//
// Do not add t.Parallel(): it sets the process-wide time.Local.
func TestFilenamesUseTheHostsLocalDate(t *testing.T) {
	prev := time.Local
	time.Local = time.FixedZone("PDT", -7*3600)
	t.Cleanup(func() { time.Local = prev })

	w, db := testWorkerSetup(t)
	w.cfg.Downloader.OutputTemplate = "${start_date} ${start_time}"
	o := &DownloadOrchestrator{db: db, logger: discardLogger{}}

	for _, tc := range []struct {
		job  *database.Job
		want string
	}{
		{&database.Job{ID: "tw_local", VideoID: "tw_x", URL: "https://twitch.tv/x", Platform: "twitch",
			Status: database.StatusUpcoming, StreamStartTime: "2026-10-17T03:30:00Z"}, "20261016 2030"},
		{&database.Job{ID: "ytlocal0001", VideoID: "ytlocal0001", URL: "https://youtube.com/watch?v=ytlocal0001", Platform: "youtube",
			Status: database.StatusUpcoming, StreamStartTime: "2026-10-17T03:30:00+00:00"}, "20261016 2030"},
		{&database.Job{ID: "ytlocal0002", VideoID: "ytlocal0002", URL: "https://youtube.com/watch?v=ytlocal0002", Platform: "youtube",
			Status: database.StatusUpcoming, CreatedAt: "2026-10-16T02:15:00Z"}, "20261015 1915"},
	} {
		if _, err := db.AddJob(tc.job); err != nil {
			t.Fatalf("AddJob %s: %v", tc.job.ID, err)
		}
		job, err := db.GetJob(tc.job.ID)
		if err != nil {
			t.Fatalf("GetJob %s: %v", tc.job.ID, err)
		}
		jobCtx := w.buildJobContext(job)
		if jobCtx.Filename != tc.want {
			t.Errorf("%s: buildJobContext named it %q, want the local %q", job.ID, jobCtx.Filename, tc.want)
		}
		if fresh, _ := o.resolveFreshFilename(jobCtx); fresh != tc.want {
			t.Errorf("%s: resolveFreshFilename named it %q, want the local %q", job.ID, fresh, tc.want)
		}
	}
}
