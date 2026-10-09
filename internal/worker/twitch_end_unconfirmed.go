package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// ErrTwitchEndUnconfirmed marks the error ExecuteTwitch returns from its
// unconfirmed-end latch on a LIVE capture: the download failed while nothing
// said the broadcast was over, so the job stops in Error with its staging and
// resume sidecar kept. setJobError records it as
// database.ParkReasonTwitchEndUnconfirmed (parkReasonForError), and that row is
// what the Twitch monitor later muxes automatically once it confirms the
// broadcast is over (AutoMuxEndedBroadcast, owner decision D-T4).
//
// A marker, not a message: the error's text is the download failure's own
// (endUnconfirmedError keeps it), and nothing routes on the prose. Only the
// broadcast-end latch carries it — a VOD's latch (it has no live end to
// confirm) and a failed part advance (a local I/O failure, which says nothing
// about the broadcast) do not.
var ErrTwitchEndUnconfirmed = errors.New("twitch broadcast end unconfirmed")

// endUnconfirmedError carries ErrTwitchEndUnconfirmed beside the latch's cause
// without changing what the error says: the job row, the failure embed and the
// log all show the download failure exactly as before.
type endUnconfirmedError struct{ cause error }

func (e *endUnconfirmedError) Error() string   { return e.cause.Error() }
func (e *endUnconfirmedError) Unwrap() []error { return []error{e.cause, ErrTwitchEndUnconfirmed} }

// markEndUnconfirmed wraps a live latch's cause with ErrTwitchEndUnconfirmed.
func markEndUnconfirmed(cause error) error { return &endUnconfirmedError{cause: cause} }

// TwitchJobLogin is the channel login a Twitch job records, as the worker
// reads it everywhere else (the job's URL first, then its channel name or ID
// shape). "" for a VOD, whose URL names no channel.
func TwitchJobLogin(job *database.Job) string {
	if strings.Contains(job.URL, "twitch.tv/videos/") {
		return ""
	}
	return extractTwitchLoginFromJob(job)
}

// TwitchBroadcastOver reports whether info — the channel's current state —
// says the broadcast job was recording is over: the channel is offline (nil or
// not live), or it is live with a DIFFERENT broadcast. A job whose ID carries
// the stream ID ("tw_<digits>", the monitor's rows and a Web add of a live
// channel) is compared by stream ID; any other row by stream_start_time, the
// worker's cross-restart broadcast identity (sameBroadcastStart). A row that
// knows neither cannot be told from the live broadcast, and reads as NOT over —
// an automatic mux of a capture whose broadcast may still be running is the
// mistake this must never make.
func TwitchBroadcastOver(job *database.Job, info *twitch.TwitchStreamInfo) bool {
	if info == nil || !info.IsLive {
		return true
	}
	if id, ok := strings.CutPrefix(job.ID, "tw_"); ok && id != "" && strings.Trim(id, "0123456789") == "" {
		return info.StreamID != "" && info.StreamID != id
	}
	if job.StreamStartTime == "" || info.StartedAt == "" {
		return false
	}
	return !sameBroadcastStart(job.StreamStartTime, info.StartedAt)
}

// AutoMuxEndedBroadcast muxes the staging a live Twitch capture kept when it
// failed with its broadcast's end unconfirmed, once the broadcast is confirmed
// over (owner decision D-T4). The Twitch monitor calls it when its poll shows
// such a job's channel offline or live with another broadcast; that is one
// sample, and a single offline sample is exactly the StreamMetadata flap owner
// decision O-C refuses to finalize on, so the worker confirms it first with
// the same two-sample check the download itself uses (confirmTwitchLiveness),
// off the monitor's goroutine. Anything short of a confirmed end — an error,
// the same broadcast still live — leaves the row as it is for the next poll.
//
// Then the mux itself runs exactly as the Mux action's does (muxJob), behind
// the job's previous run (afterJobExit), and ONCE: autoMuxNow clears the
// marker before it starts, so a mux that fails leaves the row in Error with an
// error saying so and nothing re-runs it. A second call while one is in flight
// for the job is dropped.
func (w *DownloadWorker) AutoMuxEndedBroadcast(jobID string) {
	if _, busy := w.autoMuxPending.LoadOrStore(jobID, struct{}{}); busy {
		return
	}
	w.wg.Go(func() {
		defer w.autoMuxPending.Delete(jobID)
		defer func() {
			if r := recover(); r != nil {
				w.logger.Error("panic in automatic Twitch mux", "jobID", jobID, "panic", fmt.Sprint(r))
			}
		}()
		job, err := w.db.GetJob(jobID)
		if err != nil || job == nil || !endUnconfirmedRow(job) {
			return
		}
		over, verdictErr := w.twitchBroadcastConfirmedOver(w.orchestrator.muxRoot(), job)
		if verdictErr != nil {
			w.logger.Debug("automatic Twitch mux: the broadcast's end could not be confirmed yet",
				"jobID", jobID, "err", verdictErr)
			return
		}
		if !over {
			w.logger.Debug("automatic Twitch mux: the monitor's offline sample was not confirmed; the broadcast is still live",
				"jobID", jobID)
			return
		}
		w.afterJobExit(jobID, "automatic mux", func() { w.autoMuxNow(jobID) })
	})
}

// twitchBroadcastConfirmedOver re-reads the job's channel with two samples and
// applies TwitchBroadcastOver. A worker with no Twitch service cannot confirm
// anything and answers an error.
func (w *DownloadWorker) twitchBroadcastConfirmedOver(ctx context.Context, job *database.Job) (bool, error) {
	login := TwitchJobLogin(job)
	if login == "" {
		return false, errors.New("the job names no channel login")
	}
	liveness := w.twitchLiveness
	if liveness == nil {
		if w.tw == nil {
			return false, errors.New("no Twitch service")
		}
		liveness = w.confirmTwitchStreamInfo
	}
	info, err := liveness(ctx, login)
	if err != nil {
		return false, err
	}
	return TwitchBroadcastOver(job, info), nil
}

// endUnconfirmedRow is the automatic mux's precondition: an Error row the
// unconfirmed-end latch marked.
func endUnconfirmedRow(job *database.Job) bool {
	return job.Platform == "twitch" && job.Status == database.StatusError &&
		job.ParkReason == database.ParkReasonTwitchEndUnconfirmed
}

// autoMuxNow claims the row and muxes it. Under autoMuxMu so the re-check and
// the claim are one step: the marker is cleared BEFORE the mux starts, which
// is what makes this run once per failure — a mux that fails leaves an Error
// row without the marker, so no later poll offers it again.
func (w *DownloadWorker) autoMuxNow(jobID string) {
	w.autoMuxMu.Lock()
	defer w.autoMuxMu.Unlock()
	job, err := w.db.GetJob(jobID)
	if err != nil || job == nil || !endUnconfirmedRow(job) {
		return // retried, reinitialized, muxed or deleted meanwhile
	}
	w.db.UpdateJobFields(jobID, map[string]any{"park_reason": database.ParkReasonNone})
	w.logger.Info("Twitch broadcast confirmed over — muxing the capture its failed download kept in staging",
		"jobID", jobID)
	if err := w.muxJob(jobID, autoMuxFailure); err != nil {
		w.logger.Warn("automatic Twitch mux could not start", "jobID", jobID, "err", err)
		w.db.UpdateJobFields(jobID, map[string]any{
			"status": database.StatusError,
			"error":  autoMuxFailure(err),
		})
	}
}

// autoMuxFailure is the error an automatic mux that failed leaves on the row:
// what was tried, that staging is still there, and the way to retry it.
func autoMuxFailure(err error) string {
	return fmt.Sprintf("automatic mux after the broadcast ended failed (staging is kept — use Mux to retry): %v", err)
}
