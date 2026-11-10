package worker

import (
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/jobstate"
)

// muxReadout writes a Muxing job's progress line while its archive is copied
// (owner ruling R-D1 of 2026-11-10): "Muxing 42%" as FFmpeg's output grows, then
// "Muxing: finalizing" for the faststart pass and the checks after it
// (jobstate.MuxProgressLine, MuxFinalizingLine). Until it existed the row sat
// on the download's last counters for the whole copy — minutes, for a long
// capture.
//
// It is not the download's ProgressTracker, which is closed by the time a
// finalize runs and renders nothing more. It writes the progress line and
// nothing else: never the percent, which is the download's (the bar), and
// never a status. Every line is written only while the row is still Muxing
// (UpdateJobFieldsIf), so one can never land on a Cancel or on the Finished
// write; the first refusal ends the readout.
//
// The download's counters are not lost: newMuxReadout logs the line the
// readout replaces, and the details panes keep their Segments and Chat rows.
//
// All of it runs on the goroutine that runs the copy (the engine calls
// report there), so it needs no lock.
type muxReadout struct {
	db    *database.Database
	jobID string
	// interval is the least time between two writes: the job's progress
	// interval (downloader.progress_interval_ms), the gate every progress
	// line is written through. FFmpeg reports about twice a second and a
	// line is written only when its text changes, so the gate is rarely the
	// limit.
	interval time.Duration
	now      func() time.Time

	saved     string // the row's progress line before the first write
	last      string // the line last written
	lastWrite time.Time
	wrote     bool
	stopped   bool
}

// newMuxReadout is the readout for one finalize's archive copy, or nil when
// the job gets none: a recording known to be missing its tail keeps the
// honest line its download wrote, all the way into the Finished row
// (keepIncompleteTailProgress), so nothing may write over it here. The flag
// is read fresh, as that function reads it. A row that cannot be read gets
// none either.
func (o *DownloadOrchestrator) newMuxReadout(jobCtx *JobContext) *muxReadout {
	fresh, err := o.db.GetJob(jobCtx.Job.ID)
	if err != nil || fresh == nil || fresh.IncompleteTail {
		return nil
	}
	interval := progressUpdateInterval
	if jobCtx.Config != nil && jobCtx.Config.ProgressInterval > 0 {
		interval = jobCtx.Config.ProgressInterval
	}
	if fresh.Progress != "" {
		o.logger.Info("muxing the archive: the progress line follows the copy from here", "jobID", fresh.ID, "downloadProgress", fresh.Progress)
	}
	return &muxReadout{db: o.db, jobID: fresh.ID, interval: interval, now: time.Now, saved: fresh.Progress}
}

// callback is the engine's progress callback, nil for a job with no readout:
// the copy then runs exactly as it did before there was one.
func (r *muxReadout) callback() func(engine.CopyProgress) {
	if r == nil {
		return nil
	}
	return r.report
}

// report writes the line for one of FFmpeg's reports, when it differs from
// the one on the row and the gate allows.
func (r *muxReadout) report(p engine.CopyProgress) {
	if r.stopped {
		return
	}
	line := jobstate.MuxProgressLine(p.Percent())
	if p.Finalizing {
		line = jobstate.MuxFinalizingLine
	}
	if line == r.last {
		return
	}
	now := r.now()
	if r.wrote && now.Sub(r.lastWrite) < r.interval {
		// The engine repeats a finalizing report while FFmpeg stays quiet,
		// and a percent is followed by the next, so a gated line is not lost.
		return
	}
	if !r.db.UpdateJobFieldsIf(r.jobID, database.StatusMuxing, map[string]any{"progress": line}) {
		// The row is no longer Muxing: a Cancel, a reset, a delete.
		r.stopped = true
		return
	}
	r.last, r.lastWrite, r.wrote = line, now, true
}

// restore puts back the line the row had, for a copy that did not make an
// archive: the row is about to read Error, or already reads Cancelled, and a
// row with either outcome showed the download's last counters before the
// readout existed. A row that has moved anywhere else (a reset, a resume) is
// left alone.
func (r *muxReadout) restore() {
	if r == nil || !r.wrote {
		return
	}
	r.stopped = true
	r.db.UpdateJobFieldsIfIn(r.jobID, []database.JobStatus{database.StatusMuxing, database.StatusCancelled},
		map[string]any{"progress": r.saved})
}
