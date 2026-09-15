package worker

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/chat"
	"github.com/vampiricwulf/Moombox/internal/constants"
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

// errChatWaitTimedOut is the outcome resolveChatOutcome returns when the chat
// goroutine's completion was never confirmed within the wait window plus its
// cleanup grace: a downloader that is (or might still be) running when the
// job finalizes has not completed, whatever its own eventual terminal error
// would have been — so this must never read as "finished"/"unavailable".
var errChatWaitTimedOut = errors.New("chat downloader did not confirm completion before the job finalized")

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
// and grace gives its goroutine a last chance to record and close done. If
// done still hasn't closed after that, the capture's completion is
// unconfirmed — by definition not "finished" — so this returns
// errChatWaitTimedOut rather than whatever rec happens to hold (typically
// nil, since the goroutine that would record it is presumably still stuck).
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
		return rec.verdict()
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
		// The only line that names WHY the archive is short. pagingStalled's
		// own Warn says where it stopped; this one says that the job row now
		// carries that fact.
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
// history it inherited. A nil context is the standalone Mux action, which never
// started chat.
func chatFileStatus(jobCtx *JobContext) string {
	if jobCtx != nil && jobCtx.ChatStatus == chatStatusIncomplete {
		return chatStatusIncomplete
	}
	return "finished"
}
