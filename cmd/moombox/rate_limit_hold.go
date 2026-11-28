package main

import "time"

// restoreRateLimitHold starts the backlog scheduler's YouTube rate-limit
// hold where the previous process left it, and has the hold record itself in
// st from then on (worker.Scheduler.RecordRateLimitHold). A rate-limit
// verdict holds every backlog admission for the hour yt-dlp says the limit
// lasts, and the hold lived in memory: a restart inside that hour — an
// update, a crash, a settings save that restarts — reopened admission at once,
// and the backlog went back to finding the limit one VOD at a time, the
// drain the hold exists to stop. Saved beside the disk gate's hold
// (openAlertsDoc.RateLimitHeldUntil), it holds the new process until the
// same instant, and runs out on its own then. One that has already run out
// restores nothing, and is cleared from the file so it is not read again.
// Called before the worker starts, recorder first, as restoreDiskGate is.
func restoreRateLimitHold(st *openAlerts, gate interface {
	RestoreRateLimitHold(until time.Time) bool
	RecordRateLimitHold(record func(until time.Time))
}) {
	gate.RecordRateLimitHold(func(until time.Time) {
		st.update(func(d *openAlertsDoc) { d.RateLimitHeldUntil = until.UTC() })
	})
	until := st.snapshot().RateLimitHeldUntil
	if until.IsZero() || gate.RestoreRateLimitHold(until) {
		return
	}
	st.update(func(d *openAlertsDoc) { d.RateLimitHeldUntil = time.Time{} })
}

// restoreLatencyBackfillHold is restoreRateLimitHold for the latency-mode
// backfill's own rate-limit hold (owner ruling RV-19): the walk holds its
// requests for the hour after YouTube rate-limits one, and without this a
// restart inside that hour sent its next request into the same limit five
// minutes after start. Saved in st (openAlertsDoc.LatencyBackfillHeldUntil),
// restored before the worker starts, recorder first; one that has already
// run out restores nothing and is cleared from the file.
func restoreLatencyBackfillHold(st *openAlerts, walk interface {
	RestoreLatencyBackfillHold(until time.Time) bool
	RecordLatencyBackfillHold(record func(until time.Time))
}) {
	walk.RecordLatencyBackfillHold(func(until time.Time) {
		st.update(func(d *openAlertsDoc) { d.LatencyBackfillHeldUntil = until.UTC() })
	})
	until := st.snapshot().LatencyBackfillHeldUntil
	if until.IsZero() || walk.RestoreLatencyBackfillHold(until) {
		return
	}
	st.update(func(d *openAlertsDoc) { d.LatencyBackfillHeldUntil = time.Time{} })
}
