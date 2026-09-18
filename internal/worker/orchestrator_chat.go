package worker

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/chat"
	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// setupChatDownloader creates a chat downloader for a YouTube job (A3).
// Fetches the watch page to extract the chat continuation token, visitor data,
// and determines whether chat is live or replay. Returns nil if chat is unavailable.
func (o *DownloadOrchestrator) setupChatDownloader(ctx context.Context, jobCtx *JobContext, videoInfo *youtube.VideoInfo) *chat.ChatDownloader {
	// Fetch watch page to get chat continuation and visitor data. This is a
	// ONE-SHOT call, so a snapshot of the header is the right thing here — the
	// long-lived chat downloader below gets a live getter instead.
	cookieHeader := ""
	if jobCtx.YT != nil && jobCtx.YT.Auth != nil {
		cookieHeader = jobCtx.YT.Auth.GetCookieHeader()
	}

	watchResult, err := youtube.FetchWatchPage(ctx, jobCtx.Job.VideoID, cookieHeader)
	if err != nil {
		o.logger.Warn("failed to fetch watch page for chat", "err", err, "videoID", jobCtx.Job.VideoID)
		o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{
			"chat_status": "unavailable",
		})
		return nil
	}

	// Chat continuation is extracted at watch-page parse time (see watch_page.go);
	// reading from the result avoids re-parsing the ~5 MB HTML. There is no body
	// STRING to collect: since the 2026-09-15 sweep (Arc 3) FetchWatchPage reads
	// the page as []byte and its extractors read it in place, and the token
	// json.Unmarshal produced does not alias the page — so the result retains
	// none of those bytes and the page is collectable by the time this runs.
	continuation := watchResult.ChatContinuation
	isReplay := watchResult.ChatIsReplay
	if continuation == "" {
		o.logger.Debug("no chat continuation available", "videoID", jobCtx.Job.VideoID, "err", watchResult.ChatErr)
		o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{
			"chat_status": "unavailable",
		})
		return nil
	}

	// Extract visitor data from ytcfg
	visitorData := ""
	if watchResult.Ytcfg != nil {
		visitorData = watchResult.Ytcfg.VisitorData
	}

	chatPath := filepath.Join(jobCtx.StagingDir, "chat.json")
	opts := chat.ChatDownloaderOptions{
		VideoID:             jobCtx.Job.VideoID,
		VideoTitle:          jobCtx.Job.Title,
		ChannelName:         jobCtx.Job.ChannelName,
		OutputFile:          chatPath,
		InitialContinuation: continuation,
		ApiKey:              constants.DefaultAPIKey,
		VisitorData:         visitorData,
		IsReplay:            isReplay,
		IsLiveOrUpcoming:    videoInfo.IsLive || videoInfo.IsUpcoming,
	}
	if jobCtx.YT != nil && jobCtx.YT.Auth != nil {
		opts.GenerateAuth = jobCtx.YT.Auth.GenerateAuthorizationHeader
		// Method value, exactly like GenerateAuth above: it re-reads the jar
		// on every chat poll, so the hours-long download follows the ~30-min
		// cookie rotation instead of presenting the header it started with.
		// A nil Auth leaves the field nil, which the API treats as "no
		// Cookie header" — no poll-time panic.
		opts.CookieHeader = jobCtx.YT.Auth.GetCookieHeader
	}

	if videoInfo.ScheduledStartTime != "" {
		opts.StreamStartTime = videoInfo.ScheduledStartTime
	}

	dl := chat.NewChatDownloader(opts)
	// The downloader's own diagnostics — the mode rule's sidecar refusal, the
	// file-epoch adoption, API drift — are dropped on the floor unless a
	// logger is assigned here (Arc J D6).
	dl.Logger = o.logger
	dl.OnError = func(err error) {
		o.logger.Warn("[Chat] Chat API error", "jobID", jobCtx.Job.ID, "err", err)
	}
	o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{
		"chat_status": "pending",
	})

	// Transition from "pending" -> "downloading" when chat actually starts (matches TS "start" event)
	dl.OnStart = func(messageCount int, resuming bool) {
		o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{
			"chat_status": "downloading",
		})
		if resuming {
			o.logger.Info("[Chat] Resuming chat download", "jobID", jobCtx.Job.ID, "messages", messageCount)
		} else {
			o.logger.Info("[Chat] Started downloading chat", "jobID", jobCtx.Job.ID)
		}
	}

	return dl
}

// errChatWaitTimedOut is the outcome resolveChatOutcome returns once its first
// wait has timed out and the downloader recorded no error of its own: a
// downloader that was still running when the job finalized has not completed,
// whatever its own eventual terminal error would have been — so this must
// never read as "finished"/"unavailable".
var errChatWaitTimedOut = errors.New("chat downloader was still running when the job finalized")

// resolveChatOutcome waits for the chat goroutine's completion signal (done)
// before reading rec's recorded outcome, and returns errChatWaitTimedOut
// instead of a possibly-stale nil if that signal never arrives.
//
// Fix round 1, Important 3: both orchestrators' chat goroutines run
// `rec.record(dl.Start(ctx))` and only then `defer close(done)` (LIFO defers:
// the recover-and-record statement executes first, the deferred close(done)
// last), so rec.record always happens-before done closes — which is exactly
// what makes "wait for done, THEN read rec" a safe read. Reading rec first,
// or gating the wait on dl.IsRunning(), both race a flag that a downloader's
// OWN shutdown defer clears strictly BEFORE the wrapper goroutine's record()
// runs (twitch.ChatDownloader/twitch.VodChatDownloader/chat.ChatDownloader all
// clear "running" on their way out of Start, ahead of returning to the
// wrapper) — exactly the window a stalled Twitch VOD chat could land in,
// writing "finished" over a stall and losing the real outcome for good, since
// nothing re-reads rec after this call returns.
//
// timeout bounds the first wait; on expiry dl is Stop()'d (if still running)
// and grace gives its goroutine a last chance to record and close done. Once
// that first wait has expired the result is NEVER nil, whether done closes
// inside the grace or not: the chat was still running when the job finalized,
// which is by definition not "finished". The goroutine's own error is
// reported when it recorded one (it is the more specific diagnosis);
// otherwise errChatWaitTimedOut stands in. Returning a recorded nil here
// instead would undo the whole rule, because every real downloader's
// Stop()-exit returns nil — a VOD chat still paging when its video finished
// would read "finished" on a short archive, silently.
func (o *DownloadOrchestrator) resolveChatOutcome(dl ChatSource, rec *chatOutcome, done chan struct{}, timeout, grace time.Duration) error {
	if done == nil {
		return rec.verdict()
	}

	timer := time.NewTimer(timeout)
	select {
	case <-done:
		timer.Stop()
		return rec.verdict()
	case <-timer.C:
	}

	if dl != nil && dl.IsRunning() {
		dl.Stop()
	}
	cleanupTimer := time.NewTimer(grace)
	select {
	case <-done:
		cleanupTimer.Stop()
		if v := rec.verdict(); v != nil {
			return v
		}
		return errChatWaitTimedOut
	case <-cleanupTimer.C:
		return errChatWaitTimedOut
	}
}

// cleanup handles cancellation cleanup.
func (o *DownloadOrchestrator) cleanup(chatDl *chat.ChatDownloader, chatDone chan struct{}) {
	if chatDl != nil {
		chatDl.Stop()
		if chatDone != nil {
			cleanupTimer := time.NewTimer(2 * time.Second)
			select {
			case <-chatDone:
				cleanupTimer.Stop()
			case <-cleanupTimer.C:
			}
		}
	}
}

// chatStatusIncomplete is the chat_status of a capture that ENDED WITHOUT
// COMPLETING: the downloader returned an error — a Twitch VOD paging stall, an
// IRC session that exhausted its reconnect budget, a panic — instead of running
// out of chat to fetch.
//
// One machine value, and the value IS the label: both UIs render chat_status
// verbatim (the Web details badge maps it to `warning`, the TUI's
// chatStatusColor to ColorWarning), exactly as they do for "finished",
// "downloading", "pending" and "unavailable". No display-string field restates
// it. "error" was not reused: nothing writes it, and it reads as a hard failure
// rather than an archive that stopped short.
const chatStatusIncomplete = "incomplete"

const (
	// vodChatWaitFloor and vodChatWaitCeiling bracket how long a finished VOD
	// download waits for its chat source to finish paging (owner decision
	// O-A). Live jobs keep chatWaitTimeout: a live chat stops when the
	// broadcast does, so two minutes of drain is the right shape there.
	//
	// A VOD's chat is different. Twitch pages VOD comments a screenful per
	// GQL round trip while the video downloads at link speed, so a chat-heavy
	// VOD on a fast link predictably still has minutes of paging left when
	// the video completes — and the two-minute cut then Stop()'d it, recorded
	// "incomplete", and deleted the preserved sidecar with staging
	// (sweep-2 TWITCH-3). The allowance scales with the video's own length
	// because the comment count does; the floor covers a short VOD with dense
	// chat and the ceiling stops a stalled pager holding a job open forever.
	vodChatWaitFloor   = 30 * time.Minute
	vodChatWaitCeiling = 6 * time.Hour
)

// vodChatWaitTimeout is the first-wait bound resolveChatOutcome is given for a
// VOD job. The download slot is released before this wait begins — the job is
// no longer downloading, and holding a slot through it would starve the pool.
func vodChatWaitTimeout(job *database.Job) time.Duration {
	wait := vodChatWaitFloor
	if job != nil && job.LengthSeconds != nil && *job.LengthSeconds > 0 {
		if length := time.Duration(*job.LengthSeconds) * time.Second; length > wait {
			wait = length
		}
	}
	return min(wait, vodChatWaitCeiling)
}

// resolveVodChatOutcome is the finalize-path chat wait for a VOD job: the
// whole of owner decision O-A in one place, so the two orchestrators cannot
// drift and so the ORDER — release, then wait — is a property of this
// function rather than of two call sites.
//
// The slot is given up FIRST, exactly once. The video is finished; a wait
// that can run for hours must not keep the next VOD queued behind a download
// that has stopped downloading. ReleaseDownloadSlot is keyed by the queue's
// holdingDlSlot map and so is idempotent, which is what lets the release
// below the mux stay where it is (it still covers the live path, and every
// path that reaches the mux without coming through here).
//
// The first wait is context-aware, because the bound is now long enough to
// matter: a Stop() (or a user cancel) mid-wait collapses it to zero, so
// resolveChatOutcome goes straight to its Stop()+grace path — and that path's
// rule, "never nil once the first wait expired", records a chat that was
// still paging at shutdown as incomplete rather than letting a Stop()-exit's
// nil verdict read as "finished". An ALREADY-cancelled context is left alone:
// ExecuteTwitch's outage-finalize path arrives here with ctx.Err() != nil by
// design, and collapsing its wait would change a verdict this decision is not
// about.
func (o *DownloadOrchestrator) resolveVodChatOutcome(ctx context.Context, dl ChatSource, rec *chatOutcome, done chan struct{}, job *database.Job) error {
	wait := vodChatWaitTimeout(job)

	jobID := ""
	if job != nil {
		jobID = job.ID
	}
	if o.queue != nil && jobID != "" {
		o.queue.ReleaseDownloadSlot(jobID)
	}
	o.logger.Debug("waiting for VOD chat to finish paging", "jobID", jobID, "bound", wait)

	if ctx != nil && done != nil && ctx.Err() == nil {
		timer := time.NewTimer(wait)
		select {
		case <-done:
			// resolveChatOutcome's own select takes the closed channel
			// immediately below; the bound is left intact for it.
			timer.Stop()
		case <-ctx.Done():
			wait = 0
			timer.Stop()
		case <-timer.C:
			wait = 0
		}
	}
	return o.resolveChatOutcome(dl, rec, done, wait, 2*time.Second)
}

// chatOutcome carries a chat downloader's terminal error from the goroutine it
// ran on to the orchestrator that derives chat_status from it.
//
// A mutex rather than a bare field because the Twitch orchestrator RELAUNCHES
// chat after a connectivity outage: a second goroutine can be recording while
// the first is still unwinding, and the reader is a third. The LAST run's
// outcome wins — a job whose chat recovered after an outage is not incomplete.
type chatOutcome struct {
	mu  sync.Mutex
	err error
}

// record stores one run's terminal error (nil for a clean exit).
func (c *chatOutcome) record(err error) {
	c.mu.Lock()
	c.err = err
	c.mu.Unlock()
}

// verdict returns the last recorded outcome, or nil if no run has ended.
func (c *chatOutcome) verdict() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// chatStatusForOutcome derives a job's terminal chat_status from what the chat
// downloader DID, not from its message count alone.
//
// The outcome comes FIRST, and that ordering is the whole fix. A Twitch VOD
// whose cursor paging stalls returns an error from pagingStalled
// (internal/twitch/vod_chat.go) with a SHORT archive and a preserved resume
// sidecar on disk; ranked by count that reads "finished", so both UIs showed a
// truncated archive as complete and nothing prompted the operator to act. An
// error means the capture stopped, not that it ran out of chat — including when
// it stopped before the first message, which is why "unavailable" (a genuinely
// empty chat) is only reached on a clean exit.
func chatStatusForOutcome(messageCount int, outcome error) string {
	switch {
	case outcome != nil:
		return chatStatusIncomplete
	case messageCount == 0:
		return "unavailable"
	default:
		return "finished"
	}
}

// recordChatOutcome writes the job's terminal chat_status and message count and
// remembers the status on the JobContext, where chatFileStatus reads it so the
// mux path's chat-file copy cannot overwrite the verdict.
func (o *DownloadOrchestrator) recordChatOutcome(jobCtx *JobContext, messageCount int, outcome error) {
	status := chatStatusForOutcome(messageCount, outcome)
	jobCtx.ChatStatus = status
	if outcome != nil {
		// The line that ties the verdict to the job row: pagingStalled's own
		// Warn (internal/twitch/vod_chat.go) already says where and why it
		// stopped, and this error's string carries that same offset/cursor/
		// reason here; what this line adds is that the row now carries the
		// fact. For a wait that timed out (errChatWaitTimedOut) it is the
		// only Warn there is.
		o.logger.Warn("chat capture did not complete; recording it as incomplete",
			"jobID", jobCtx.Job.ID, "err", outcome, "messages", messageCount)
	}
	o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{
		"chat_status":         status,
		"total_chat_messages": messageCount,
	})
}

// chatFileStatus is the chat_status the mux path records when it copies a chat
// file beside the video.
//
// Archiving a file means "finished" — that was the whole rule before — UNLESS
// the downloader already reported an INCOMPLETE capture, in which case the file
// being copied is the short one and writing "finished" over that verdict is
// exactly the bug. Every other verdict keeps the old behaviour, "unavailable"
// included: a resumed job whose session added no messages still archived the
// history it inherited. A nil context is never passed today — the standalone
// Mux action builds a real JobContext (buildJobContext, worker.go) with no
// chat verdict on it — so the nil guard is purely defensive.
func chatFileStatus(jobCtx *JobContext) string {
	if jobCtx != nil && jobCtx.ChatStatus == chatStatusIncomplete {
		return chatStatusIncomplete
	}
	return "finished"
}
