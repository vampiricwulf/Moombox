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
