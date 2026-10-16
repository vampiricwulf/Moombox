package worker

import (
	"context"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// networkWaitProgress is the progress line a job waiting out a connectivity
// outage shows for the length of the wait (waitForNetwork), as a VOD queueing
// for its slot shows vodSlotWaitProgress.
const networkWaitProgress = "Waiting for network connectivity..."

// outageConfirmWindow is how long a transport failure met while the
// connectivity monitor still reports online waits for the monitor to call
// the outage (outageCalled). The monitor needs two failed polls, five seconds
// apart, each up to its probe race long, so a fetch that met the first
// seconds of an outage fails before the monitor has said a word. A var so
// tests need not wait it out.
var outageConfirmWindow = 30 * time.Second

// isTransportFailure reports whether err never reached the service at all —
// a dial, DNS or TLS failure, a deadline with no answer, a connection cut
// mid-response — as opposed to an answer from it (an HTTP status, a GQL
// error, a substitution) or an error nobody can place. probeVote's "down"
// verdict, the one the connectivity oracle is told.
func isTransportFailure(err error) bool {
	return err != nil && probeVote(err) == reportFailure
}

// offline reports whether the connectivity monitor says the device has no
// network. No monitor wired: never.
func (w *DownloadWorker) offline() bool {
	return w.conn != nil && !w.conn.IsOnline()
}

// outageCalled reports whether a stream-processing failure was the device
// being offline, and so must be waited out rather than recorded: a transport
// failure (isTransportFailure) met while the monitor reports offline, or one
// the monitor goes on to call an outage within outageConfirmWindow. An answer
// from the service is a verdict whatever the monitor says, and a transport
// failure the monitor never calls — a platform's DNS failing on a network
// that is up — is a failure of that platform. False when ctx ends first; the
// caller reads ctx before it records anything.
func (w *DownloadWorker) outageCalled(ctx context.Context, err error) bool {
	if w.conn == nil || !isTransportFailure(err) {
		return false
	}
	called := make(chan struct{}, 1)
	unregister := w.conn.OnStateChange(func(online bool) {
		if !online {
			select {
			case called <- struct{}{}:
			default:
			}
		}
	})
	defer unregister()
	// Read AFTER subscribing, so a transition between the failure and the
	// subscription is in the read; every later one is in the subscription.
	if !w.conn.IsOnline() {
		return true
	}
	t := time.NewTimer(outageConfirmWindow)
	defer t.Stop()
	select {
	case <-called:
		return true
	case <-t.C:
		return false
	case <-ctx.Done():
		return false
	}
}

// waitForNetwork blocks until the connectivity monitor reports online, and
// reports false when ctx ends first. The row keeps its status throughout —
// an Upcoming broadcast stays Upcoming, an interrupted capture Downloading —
// and its progress line says what it is waiting for, cleared however the wait
// ends. Returns at once when the monitor already reports online.
func (w *DownloadWorker) waitForNetwork(ctx context.Context, jobID string) bool {
	if w.conn == nil {
		return true
	}
	online := make(chan struct{}, 1)
	unregister := w.conn.OnStateChange(func(isOnline bool) {
		if isOnline {
			select {
			case online <- struct{}{}:
			default:
			}
		}
	})
	defer unregister()
	// Read AFTER subscribing, as orchestrator.waitForOnline does: the
	// network may have come back between the caller's read and this one.
	if w.conn.IsOnline() {
		return true
	}
	w.db.UpdateJobFields(jobID, map[string]any{"progress": networkWaitProgress})
	defer w.db.UpdateJobFields(jobID, map[string]any{"progress": ""})
	for {
		select {
		case <-online:
		case <-ctx.Done():
			return false
		}
		// A flap can deliver "online" and take it back before this reads it.
		if w.conn.IsOnline() {
			return true
		}
	}
}

// waitOutOutage waits for the network on behalf of job's run, then reports
// whether the run should go on: false when ctx ended first, or when the row
// was deleted or left the states the queue processes while it waited — the
// re-read Process itself makes after its fetch, for a run that waited minutes
// or hours between the two.
func (w *DownloadWorker) waitOutOutage(ctx context.Context, job *database.Job) bool {
	if !w.waitForNetwork(ctx, job.ID) {
		return false
	}
	cur, err := w.db.GetJob(job.ID)
	if err != nil {
		// A database error must not end a job that is waiting for nothing
		// else: go on, as Process's own re-read does.
		return true
	}
	return cur != nil && ShouldProcess(cur)
}

// processStreamThroughOutages is stream processing that waits a connectivity
// outage out instead of failing on it. Stream processing's first act is a
// fetch — the full player fetch, Twitch's stream info — and a fetch made
// during an outage failed with a dial error that processJob recorded as the
// job's Error: an offline boot (the PC back before the router) re-enqueued
// every Upcoming, Live and Downloading row and failed every one of them for
// good. Nothing brought them back — a YouTube Error has no automatic way out,
// and the Twitch recovery wants TwitchOfflineErrMsg — so a scheduled stream
// went unrecorded and the rest of an interrupted Twitch capture (no DVR) was
// lost.
//
// So, as the scheduler holds admission and the downloaders wait in place:
// while the monitor reports offline, stream processing does not start; and a
// fetch that fails on the outage (outageCalled) waits for the network and
// runs stream processing again from the top, the row keeping its status
// throughout. Anything else — an answer from the service, a transport failure
// the monitor does not call an outage — is returned as before.
//
// A wait that ctx ends, or after which the row is gone or no longer
// processable, returns cancelledResult(), which processJob routes to
// handleCancellation: a shutdown leaves the row as it is, to resume on the
// next start.
func (w *DownloadWorker) processStreamThroughOutages(ctx context.Context, job *database.Job) (*StreamProcessResult, error) {
	for {
		if w.offline() {
			w.logger.Info("offline; stream processing waits for connectivity",
				"jobID", job.ID, "status", job.Status)
			if !w.waitOutOutage(ctx, job) {
				return cancelledResult(), nil
			}
			w.logger.Info("connectivity restored; processing the job", "jobID", job.ID)
		}
		result, err := w.processStream(ctx, job)
		if err == nil || ctx.Err() != nil || !w.outageCalled(ctx, err) {
			return result, w.markTwitchUnreachable(job, err)
		}
		w.logger.Warn("stream processing failed on a connectivity outage; waiting for the network, then processing the job again",
			"jobID", job.ID, "status", job.Status, "err", err)
	}
}

// refreshStaleVodInfoThroughOutages is refreshStaleVodInfo with the same
// outage rule: a re-extraction that fails on an outage waits for the network
// and is made again, rather than ending the VOD in Error (or, for backlog,
// sending it back to Queued) over a fetch the outage failed. The slots are
// held through the wait: the download they are for comes straight after.
// Returns an error wrapping ErrCancelled when the wait ended without the run.
func (w *DownloadWorker) refreshStaleVodInfoThroughOutages(ctx context.Context, job *database.Job, result *StreamProcessResult, extractedAt time.Time) error {
	for {
		err := w.refreshStaleVodInfo(ctx, job, result, extractedAt)
		if err == nil || ctx.Err() != nil || !w.outageCalled(ctx, err) {
			return err
		}
		w.logger.Warn("VOD re-extraction failed on a connectivity outage; waiting for the network, then re-extracting",
			"jobID", job.ID, "err", err)
		if !w.waitOutOutage(ctx, job) {
			return ErrCancelled
		}
	}
}

// twitchUnreachableError marks a Twitch live job's stream-processing failure
// that never reached Twitch (isTransportFailure) on a network the monitor
// says is up. It reads as the error it wraps; what it adds is the park
// reason setJobError records for it (parkReasonForError →
// ParkReasonTwitchUnreachable), the marker the Twitch monitor's recovery
// keys on alongside TwitchOfflineErrMsg — when the monitor next finds the
// same broadcast live it re-initialises the job, where an Error carrying the
// transport failure's own text stayed in Error for good and the rest of the
// broadcast (Twitch has no DVR) was never captured.
type twitchUnreachableError struct{ err error }

func (e *twitchUnreachableError) Error() string { return e.err.Error() }
func (e *twitchUnreachableError) Unwrap() error { return e.err }

// markTwitchUnreachable wraps err in twitchUnreachableError when it is a
// transport failure of a Twitch live job's stream processing. A VOD
// (tw_v…) is left as it is: the monitor's recovery is for live broadcasts.
func (w *DownloadWorker) markTwitchUnreachable(job *database.Job, err error) error {
	if job.Platform != "twitch" || strings.HasPrefix(job.VideoID, "tw_v") || !isTransportFailure(err) {
		return err
	}
	return &twitchUnreachableError{err: err}
}
