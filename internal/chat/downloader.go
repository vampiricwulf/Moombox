package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

const (
	dedupKeepSize       = 5000 // Keep last N IDs for dedup
	maxConsecErrorsLive = 20
	maxConsecErrorsVod  = 5
	// maxStaleContinuationAttempts bounds the fresh-continuation retry loop.
	// With exponential backoff 10s→5min cap, 12 attempts = ~50 min worst case.
	// The inner loop also exits early on cd.shouldStop() (which includes
	// MarkStreamEnded), so in the healthy path the orchestrator trims this
	// window further (audit chat.md R2 — was 30, tightened to 12).
	maxStaleContinuationAttempts = 12
	writeInterval                = 1 * time.Second // Flush batched messages to disk at most once per interval
)

// ChatDownloaderOptions configures a ChatDownloader.
type ChatDownloaderOptions struct {
	VideoID             string
	VideoTitle          string
	ChannelName         string
	OutputFile          string
	InitialContinuation string
	ApiKey              string
	VisitorData         string
	CookieHeader        string
	GenerateAuth        func() string // Returns SAPISIDHASH Authorization header for member-gated chat
	IsReplay            bool
	IsLiveOrUpcoming    bool
	StreamStartTime     string
	ResumeFile          string
}

// ChatDownloader downloads YouTube live chat messages.
type ChatDownloader struct {
	opts                 ChatDownloaderOptions
	api                  *ChatAPI
	mu                   sync.Mutex
	running              bool
	cancelFlag           bool
	streamEnded          bool
	liveContinuationOpen bool
	messages             []ChatMessage // Unwritten messages in memory
	messageCount         int
	dedup                *utils.OrderedDedup[string]
	continuation         string
	streamStartMs        int64
	flushedToDisk        bool
	// resumeFileAuto is true when ResumeFile was synthesized from OutputFile in
	// NewChatDownloader (caller did not pass an explicit ResumeFile). Used by
	// SetOutputFile to know whether it's safe to re-derive ResumeFile from the
	// new path — prevents clobbering a caller-provided literal ".resume.json"
	// (audit chat.md C7).
	resumeFileAuto bool
	// ioErrorOccurred is set whenever a disk-IO failure routes through OnError
	// inside writeFullChatFile / incrementalAppend / updateChatFileHeader.
	// Inspected by Start() before clearResume() so the resume file is
	// preserved if the final flush failed (audit chat.md C8).
	ioErrorOccurred bool
	cancelCtx       context.CancelFunc // for aborting sleep on stop/markStreamEnded
	done            chan struct{}      // closed when Start() completes; nil if never started

	// testRecoveryOverride allows tests to inject a recovery function instead of
	// calling recoverStaleContinuation. Only set in tests; nil in production.
	testRecoveryOverride func(ctx context.Context) bool

	// testBackoffOverride, when > 0, replaces the computed exponential-backoff
	// duration in handleFetchError so tests don't have to sleep for real
	// (5s-60s) intervals. Only set in tests; zero (disabled) in production.
	testBackoffOverride time.Duration

	OnStart  func(messageCount int, resuming bool)
	OnFinish func()
	OnError  func(err error)

	// onProgress is reassigned mid-flight by callers (orchestrator after the
	// chat goroutine has already started polling), so it can't be a plain
	// public field — concurrent reassignment + read from runChatLoop is a
	// data race per Go's memory model. Reads/writes go through callOnProgress
	// and SetOnProgress under onProgressMu. Separate from cd.mu so a slow
	// callback doesn't block other downloader state.
	onProgressMu sync.RWMutex
	onProgress   func(p ChatProgress)

	// Logger is an optional diagnostic sink for non-fatal, debug-level drift
	// signals (e.g. unexpected API field shapes). nil-safe — if not set,
	// debug diagnostics are silently dropped.
	Logger interface {
		Debug(msg string, args ...any)
	}
}

// SetOnProgress installs the progress callback. Safe to call before or after
// Start; the chat goroutine reads through callOnProgress under the same lock.
func (cd *ChatDownloader) SetOnProgress(fn func(p ChatProgress)) {
	cd.onProgressMu.Lock()
	cd.onProgress = fn
	cd.onProgressMu.Unlock()
}

// callOnProgress snapshots the current progress callback under the lock and
// invokes it outside the lock so a slow caller (e.g. a database update)
// does not block a concurrent SetOnProgress.
func (cd *ChatDownloader) callOnProgress(p ChatProgress) {
	cd.onProgressMu.RLock()
	fn := cd.onProgress
	cd.onProgressMu.RUnlock()
	if fn != nil {
		fn(p)
	}
}

// logDebug routes a debug-level diagnostic through the optional Logger.
// No-op when Logger is nil.
func (cd *ChatDownloader) logDebug(msg string, args ...any) {
	if cd.Logger != nil {
		cd.Logger.Debug(msg, args...)
	}
}

// reportIOError marks an IO failure and routes the error through OnError.
// The flag is inspected by Start() before clearResume() so the resume file
// is preserved when the final flush failed (audit chat.md C8).
func (cd *ChatDownloader) reportIOError(err error) {
	cd.mu.Lock()
	cd.ioErrorOccurred = true
	cd.mu.Unlock()
	if cd.OnError != nil {
		cd.OnError(err)
	}
}

// chatWarnAdapter adapts the Debug-only cd.Logger to the Warn-shaped
// utils.ChatFileLogger. Per-message marshal failures inside AppendChatMessages
// are non-fatal drift signals; Debug is the right severity.
type chatWarnAdapter struct{ cd *ChatDownloader }

func (a chatWarnAdapter) Warn(msg string, args ...any) { a.cd.logDebug(msg, args...) }

// NewChatDownloader creates a new chat downloader.
func NewChatDownloader(opts ChatDownloaderOptions) *ChatDownloader {
	api := NewChatAPI(opts.ApiKey, opts.VisitorData, opts.CookieHeader)
	api.generateAuth = opts.GenerateAuth

	resumeFileAuto := false
	if opts.ResumeFile == "" {
		opts.ResumeFile = opts.OutputFile + ".resume.json"
		resumeFileAuto = true
	}

	var streamStartMs int64
	if opts.StreamStartTime != "" {
		if t, err := time.Parse(time.RFC3339, opts.StreamStartTime); err == nil {
			streamStartMs = t.UnixMilli()
		}
	}

	return &ChatDownloader{
		opts:           opts,
		api:            api,
		dedup:          utils.NewOrderedDedup[string](),
		continuation:   opts.InitialContinuation,
		streamStartMs:  streamStartMs,
		resumeFileAuto: resumeFileAuto,
	}
}

// Start begins the chat download process.
// If the downloader is already running (e.g., early chat handed to the orchestrator),
// Start blocks until the existing run completes or ctx is cancelled.
func (cd *ChatDownloader) Start(ctx context.Context) error {
	cd.mu.Lock()
	if cd.running {
		done := cd.done
		cd.mu.Unlock()
		// Already running — wait for completion or context cancellation
		if done != nil {
			select {
			case <-done:
			case <-ctx.Done():
			}
		}
		return nil
	}
	cd.running = true
	cd.cancelFlag = false
	cd.streamEnded = false
	// A fresh run starts with no resume signal, not whatever a PRIOR run on
	// this same instance last left behind (e.g. a completed run that ended
	// with the signal open, then this same *ChatDownloader gets Start()
	// called again). Closed = no information by design (see
	// LiveContinuationOpen's doc comment) — a run that hasn't polled yet
	// has none, same as a run that just died.
	cd.liveContinuationOpen = false
	cd.done = make(chan struct{})
	// Propagate Logger to the API so parse-level drift diagnostics are captured.
	if cd.api != nil {
		cd.api.Logger = cd.Logger
	}
	cd.mu.Unlock()

	defer func() {
		if r := recover(); r != nil {
			// Capture the stack at the point of panic so production crashes
			// are diagnosable from the OnError sink alone.
			stack := debug.Stack()
			if cd.OnError != nil {
				// Guard against a panicking OnError handler — we're already
				// in recovery, a second panic here would escape the goroutine.
				func() {
					defer func() {
						_ = recover()
					}()
					cd.OnError(fmt.Errorf("chat downloader panic: %v\n%s", r, stack))
				}()
			}
			cd.mu.Lock()
			cd.running = false
			cd.mu.Unlock()
		}
	}()

	defer func() {
		cd.mu.Lock()
		cd.running = false
		cd.cancelCtx = nil
		done := cd.done
		cd.done = nil
		cd.mu.Unlock()
		if done != nil {
			close(done)
		}
	}()

	// Create a cancellable context for aborting sleep on stop/markStreamEnded
	ctx, cancel := context.WithCancel(ctx)
	cd.mu.Lock()
	cd.cancelCtx = cancel
	cd.mu.Unlock()
	defer cancel()

	// Load resume state
	resuming := false
	state, err := cd.loadResume()
	if err == nil && state != nil && state.VideoID == cd.opts.VideoID {
		cd.continuation = state.Continuation
		cd.messageCount = state.MessageCount
		cd.messages = nil // Start fresh — old messages already on disk
		if len(state.RecentIDs) > 0 {
			cd.dedup.Restore(state.RecentIDs)
		}
		// Cross-check that the chat file actually exists on disk — guards
		// against the case where the resume sidecar survived but the chat
		// file was deleted/moved out from under us. Without this, the next
		// write would take the incremental-append path and fail (audit
		// chat.md G5).
		cd.flushedToDisk = cd.messageCount > 0
		if cd.flushedToDisk && cd.opts.OutputFile != "" {
			if _, statErr := os.Stat(cd.opts.OutputFile); statErr != nil {
				cd.flushedToDisk = false
			}
		}
		resuming = true
	}

	if cd.OnStart != nil {
		cd.OnStart(cd.messageCount, resuming)
	}

	// Run chat loop
	cd.runChatLoop(ctx, resuming)

	// Write remaining messages
	if len(cd.messages) > 0 {
		cd.writeChatFile()
	}

	// Update header metadata if we flushed earlier
	if cd.flushedToDisk && cd.messageCount > 0 {
		cd.updateChatFileHeader()
	}

	// Clear resume state on clean completion (not cancelled), but only if the
	// final flush + header update succeeded — preserve the resume file when an
	// IO error fired so the next run can recover (audit chat.md C8).
	cd.mu.Lock()
	ioErr := cd.ioErrorOccurred
	cd.mu.Unlock()
	if !cd.wasCancelledOrShutdown(ctx) && !ioErr {
		cd.clearResume()
	}

	if cd.OnFinish != nil {
		cd.OnFinish()
	}

	return nil
}

// MarkStreamEnded signals that the stream has ended naturally.
// This exits the polling loop promptly, writes the final chat file,
// and clears resume state (successful completion).
// Distinct from Stop() which is for cancellation/shutdown.
func (cd *ChatDownloader) MarkStreamEnded() {
	cd.mu.Lock()
	cd.streamEnded = true
	if cd.cancelCtx != nil {
		cd.cancelCtx() // Wake up any sleeping poll
	}
	cd.mu.Unlock()
}

// Stop cancels the chat download (for shutdown/cancellation).
//
// This is a PERMANENT exit (I5 fix): closes the resume signal directly,
// rather than relying on the loop to notice the cancellation and close it
// on its way out — Stop() can be called while the loop is sleeping between
// polls, not just mid-fetch, and setting it here covers every case
// uniformly. Not an "ended" inference (we don't know whether the broadcast
// is still live) — a stopped downloader carries no information any more,
// and closed is what "no information" means by design (see
// LiveContinuationOpen's doc comment).
func (cd *ChatDownloader) Stop() {
	cd.mu.Lock()
	cd.running = false
	cd.cancelFlag = true
	cd.liveContinuationOpen = false
	if cd.cancelCtx != nil {
		cd.cancelCtx() // Wake up any sleeping poll
	}
	cd.mu.Unlock()
}

// MessageCount returns the total number of messages downloaded.
func (cd *ChatDownloader) MessageCount() int {
	cd.mu.Lock()
	defer cd.mu.Unlock()
	return cd.messageCount
}

// IsRunning returns whether the downloader is currently running.
func (cd *ChatDownloader) IsRunning() bool {
	cd.mu.Lock()
	defer cd.mu.Unlock()
	return cd.running
}

// isStreamActive returns whether the stream is still live (not ended).
func (cd *ChatDownloader) isStreamActive() bool {
	cd.mu.Lock()
	defer cd.mu.Unlock()
	return cd.opts.IsLiveOrUpcoming && !cd.streamEnded
}

func (cd *ChatDownloader) shouldStop() bool {
	cd.mu.Lock()
	defer cd.mu.Unlock()
	return !cd.running || cd.cancelFlag || cd.streamEnded
}

// LiveContinuationOpen reports whether the LIVE chat endpoint is still
// issuing continuations — the "chat is open" resume signal (interruption
// spec). Directional by design: true means the broadcast may resume; false
// means NOTHING (streamers disable chat independently, and a downloader
// that never started, or has permanently stopped, has no information).
// A TRANSIENT fetch error does not change it — only a definitive
// end-of-stream (handleEndOfStream) or a PERMANENT loop exit closes it:
// ErrAuthRequired, the consecutive-error budget exhausting (both I5 fix,
// handleFetchError), or Stop() (I5 fix) — a downloader that has stopped
// polling for good carries no information any more, and closed is what
// "no information" means here, by design, not an inference that the
// broadcast ended.
func (cd *ChatDownloader) LiveContinuationOpen() bool {
	cd.mu.Lock()
	defer cd.mu.Unlock()
	return cd.liveContinuationOpen
}

// setLiveContinuationOpen records the definitive open/closed state.
func (cd *ChatDownloader) setLiveContinuationOpen(open bool) {
	cd.mu.Lock()
	cd.liveContinuationOpen = open
	cd.mu.Unlock()
}

// SetLiveContinuationOpenForTesting force-sets the live-continuation signal
// without driving a real poll loop. Exported ONLY so cross-package tests
// (internal/worker's MayResume truth table, which consults the real
// LiveContinuationOpen() accessor against a real *ChatDownloader) can put
// one in a known open/closed state — liveContinuationOpen has no other
// exported setter, and production code must never call this; the real
// signal path is noteLivePollResult via the live poll loop.
func (cd *ChatDownloader) SetLiveContinuationOpenForTesting(open bool) {
	cd.setLiveContinuationOpen(open)
}

// noteLivePollResult is the runChatLoop hook: a successful LIVE poll with a
// continuation opens the signal; replay polls never do.
func (cd *ChatDownloader) noteLivePollResult(hasContinuation bool) {
	if cd.opts.IsReplay {
		return
	}
	if hasContinuation {
		cd.setLiveContinuationOpen(true)
	}
}

// wasCancelledOrShutdown returns true if the download was stopped by user
// cancellation or application shutdown. Context cancellation without Stop()
// is a shutdown race — the parent context was cancelled but Stop() hasn't
// been called yet. MarkStreamEnded sets streamEnded (clean completion) so
// we distinguish it from external context cancellation.
func (cd *ChatDownloader) wasCancelledOrShutdown(ctx context.Context) bool {
	cd.mu.Lock()
	cancelled := cd.cancelFlag
	ended := cd.streamEnded
	cd.mu.Unlock()
	if !cancelled && !ended && ctx.Err() != nil {
		return true
	}
	return cancelled
}

func (cd *ChatDownloader) runChatLoop(ctx context.Context, resuming bool) {
	consecutiveErrors := 0
	switchedToAllChat := resuming // Skip All Chat switch when resuming — continuation is already mid-stream
	// lastWriteAt is loop-local — only the loop reads/writes it for the
	// writeInterval throttle (audit chat.md U1).
	var lastWriteAt time.Time

	for !cd.shouldStop() {
		if cd.continuation == "" {
			return
		}

		resp, err := cd.fetchOne(ctx)
		if err != nil {
			if cd.handleFetchError(ctx, err, &consecutiveErrors) {
				break
			}
			continue
		}
		consecutiveErrors = 0

		// Switch from "Top Chat" (filtered) to "All Chat" (unfiltered) on the
		// first response. Only mark the switch complete when we actually
		// upgraded the continuation — if AllChatContinuation was absent
		// (early preroll), leave the flag false so a subsequent poll retries.
		if !switchedToAllChat && resp.AllChatContinuation != "" {
			cd.continuation = resp.AllChatContinuation
			switchedToAllChat = true
			continue
		}

		newInBatch, lastTs := cd.processBatch(resp)
		if newInBatch > 0 {
			cd.callOnProgress(ChatProgress{
				MessageCount:  cd.messageCount,
				LastTimestamp: lastTs,
			})
			cd.maybeFlush(&lastWriteAt, newInBatch)
		}

		// Bound dedup on every successful fetch — pinned announcements and
		// replayed IDs can grow seenIDs even when newInBatch == 0, so the
		// cull must live outside the write-interval gate (audit chat.md C6).
		if cd.dedup.Len() > dedupKeepSize {
			cd.dedup.Keep(dedupKeepSize)
		}

		// Handle end-of-stream / stale continuation
		if resp.IsComplete || resp.NextContinuation == "" {
			if !cd.isStreamActive() {
				break // VOD/replay complete
			}
			if !cd.handleEndOfStream(ctx) {
				break
			}
			switchedToAllChat = false // Fresh token defaults to Top Chat — re-trigger switch
			continue
		}

		cd.continuation = resp.NextContinuation
		cd.noteLivePollResult(true)
		if delay := cd.computePollDelay(resp); delay > 0 {
			cd.sleep(ctx, delay)
		}
	}

	// Save resume state when cancelled or context cancelled (shutdown race).
	// We save if there are unflushed messages OR any disk-flushed state exists
	// (dedup IDs, continuation token) so a resume can pick up where we left off.
	if cd.wasCancelledOrShutdown(ctx) && (len(cd.messages) > 0 || cd.flushedToDisk) {
		cd.saveResume()
	}
}

// fetchOne performs a single chat fetch, routing to the replay or live
// endpoint based on opts.IsReplay.
func (cd *ChatDownloader) fetchOne(ctx context.Context) (*ChatApiResponse, error) {
	if cd.opts.IsReplay {
		return cd.api.FetchChatReplay(ctx, cd.continuation)
	}
	return cd.api.FetchLiveChat(ctx, cd.continuation)
}

// handleFetchError reacts to an error returned by fetchOne. Returns true when
// the loop should break — context cancelled, auth failure (ErrAuthRequired),
// or consecutive-error budget exhausted. On a transient error it calls
// OnError, sleeps with exponential backoff, and returns false so the caller
// can `continue`.
func (cd *ChatDownloader) handleFetchError(ctx context.Context, err error, consecutiveErrors *int) bool {
	if ctx.Err() != nil {
		return true
	}
	// Auth failure — cookies are expired / never worked. Abort immediately
	// rather than burning the consecutive-error budget on a credential state
	// that will not recover without a refresh (audit chat.md T5).
	//
	// This is a PERMANENT loop exit (I5 fix): the downloader stops polling
	// for good here, so it must close the resume signal. This is NOT an
	// "ended" inference — we have no idea whether the broadcast is still
	// live — it's the opposite: a downloader that stopped polling carries
	// NO information any more, and closed is what "no information" means
	// by design (LiveContinuationOpen's doc comment). Leaving it latched
	// true would hand the engine permanent (wrong) MayResume evidence from
	// a downloader that will never observe anything again.
	if errors.Is(err, ErrAuthRequired) {
		cd.setLiveContinuationOpen(false)
		if cd.OnError != nil {
			cd.OnError(err)
		}
		return true
	}

	*consecutiveErrors++
	if cd.OnError != nil {
		cd.OnError(err)
	}

	// Higher tolerance for live streams (> not >=, matching TypeScript)
	maxErrors := maxConsecErrorsVod
	if cd.isStreamActive() {
		maxErrors = maxConsecErrorsLive
	}
	if *consecutiveErrors > maxErrors {
		// Same permanent-exit reasoning as the ErrAuthRequired branch above
		// — the consecutive-error budget is exhausted, this downloader is
		// done for good, and its resume signal must close with it.
		cd.setLiveContinuationOpen(false)
		if cd.OnError != nil {
			cd.OnError(fmt.Errorf("too many consecutive chat API errors"))
		}
		return true
	}

	// Exponential backoff (cap at 30s for VOD, 60s for live)
	maxBackoff := 30000
	if cd.isStreamActive() {
		maxBackoff = 60000
	}
	backoffMs := min(5000*(*consecutiveErrors), maxBackoff)
	backoff := time.Duration(backoffMs) * time.Millisecond
	if cd.testBackoffOverride > 0 {
		backoff = cd.testBackoffOverride
	}
	cd.sleep(ctx, backoff)
	return false
}

// processBatch walks a successful response's messages, computes live-mode
// offsets for messages that arrived without a replay offset, dedups by ID,
// and appends newly-seen messages to cd.messages. Returns the new-in-batch
// count and the last message's timestampText for progress reporting.
//
// Pre-stream "waiting-room" chat legitimately produces negative offsets, so
// HasOffset (not OffsetMs == 0) is the sentinel for "offset not yet set".
func (cd *ChatDownloader) processBatch(resp *ChatApiResponse) (newInBatch int, lastTs string) {
	// Collect newly-seen messages first, then commit the whole batch under a
	// single cd.mu acquisition rather than locking once per message. Offset
	// computation and dedup touch only loop-goroutine-owned state (cd.dedup,
	// cd.streamStartMs), so they stay outside the lock as before.
	var fresh []ChatMessage
	for i := range resp.Messages {
		msg := &resp.Messages[i]

		if !msg.HasOffset && cd.streamStartMs > 0 && msg.TimestampUsec != "" {
			usec, err := strconv.ParseInt(msg.TimestampUsec, 10, 64)
			if err != nil {
				cd.logDebug("chat: timestampUsec parse failed", "videoID", cd.opts.VideoID, "value", msg.TimestampUsec, "err", err)
			} else if usec > 0 {
				msg.OffsetMs = usec/1000 - cd.streamStartMs
				msg.HasOffset = true
			}
		}

		// Dedup by ID (skip empty IDs to avoid silent dedup of malformed messages)
		if msg.ID != "" && !cd.dedup.Add(msg.ID) {
			continue
		}
		fresh = append(fresh, *msg)
	}
	if len(fresh) > 0 {
		// messageCount is read concurrently via MessageCount() (orchestrator
		// goroutine) — mutate under the same lock the reader takes.
		cd.mu.Lock()
		cd.messages = append(cd.messages, fresh...)
		cd.messageCount += len(fresh)
		cd.mu.Unlock()
		newInBatch = len(fresh)
	}
	if len(resp.Messages) > 0 {
		lastTs = resp.Messages[len(resp.Messages)-1].TimestampText
	}
	return
}

// maybeFlush writes accumulated messages to disk if at least writeInterval
// has elapsed since the last flush. Updates *lastWriteAt on flush.
func (cd *ChatDownloader) maybeFlush(lastWriteAt *time.Time, newInBatch int) {
	if newInBatch == 0 {
		return
	}
	now := time.Now()
	if !lastWriteAt.IsZero() && now.Sub(*lastWriteAt) < writeInterval {
		return
	}
	// writeChatFile updates the header count in the same handle (full write on
	// the first flush, folded into the append thereafter), so no separate
	// updateChatFileHeader call is needed here — the final one in Start() keeps
	// the hard-error (C8) guarantee for the last flush.
	cd.writeChatFile()
	cd.saveResume()
	*lastWriteAt = now
}

// recoverStaleContinuation runs the exponential-backoff fresh-continuation
// retry loop when the current continuation expired mid-stream. Returns true
// if a fresh token was obtained (cd.continuation updated), false if the
// maxStaleContinuationAttempts cap was exhausted or the loop was asked to
// stop. Sleeps *between* attempts (not before the first), matching C15.
func (cd *ChatDownloader) recoverStaleContinuation(ctx context.Context) bool {
	fresh, _, freshErr := cd.api.FetchFreshContinuation(ctx, cd.opts.VideoID)
	if freshErr == nil && fresh != "" {
		cd.continuation = fresh
		return true
	}

	contRetryDelay := 10 * time.Second
	contRetries := 1 // the initial failed call above counts as attempt #1
	for !cd.shouldStop() && contRetries < maxStaleContinuationAttempts {
		cd.sleep(ctx, contRetryDelay)
		if cd.shouldStop() {
			return false
		}
		retry, _, retryErr := cd.api.FetchFreshContinuation(ctx, cd.opts.VideoID)
		if retryErr == nil && retry != "" {
			cd.continuation = retry
			return true
		}
		contRetries++
		// Exponential backoff: 10s, 20s, 40s, 80s, cap at 5min.
		contRetryDelay = min(contRetryDelay*2, 5*time.Minute)
	}
	return false
}

// handleEndOfStream processes the end-of-stream / stale continuation case.
// CRITICAL: the signal must be closed (setLiveContinuationOpen(false)) BEFORE
// attempting recovery. This ordering guarantees that the signal state is
// observable even if recovery fails.
// Returns true if recovery succeeded and polling should resume, false if the
// loop should break.
func (cd *ChatDownloader) handleEndOfStream(ctx context.Context) bool {
	cd.setLiveContinuationOpen(false)
	recovered := false
	if cd.testRecoveryOverride != nil {
		recovered = cd.testRecoveryOverride(ctx)
	} else {
		recovered = cd.recoverStaleContinuation(ctx)
	}
	return recovered
}

// computePollDelay returns how long to wait before the next chat fetch,
// respecting YouTube's TimeoutMs hint when positive and falling back to 5s
// (live) or 0 (replay) otherwise. A non-positive TimeoutMs is deliberately
// *not* treated as "poll immediately" — YouTube has historically shipped 0
// as a backpressure signal ("nothing to give you") and hammering the API
// would be wasteful (audit chat.md R4). parseResponse initialises TimeoutMs
// to -1 so an absent field is distinguishable from an explicit zero, and
// both fall through to the live default here.
func (cd *ChatDownloader) computePollDelay(resp *ChatApiResponse) time.Duration {
	waitMs := resp.TimeoutMs
	if waitMs <= 0 {
		if cd.opts.IsReplay {
			return 0
		}
		waitMs = 5000
	}
	return time.Duration(waitMs) * time.Millisecond
}

// SetOutputFile updates the output file path. Used when early chat is started
// before the staging directory exists — the path is set later when staging is created.
func (cd *ChatDownloader) SetOutputFile(path string) {
	cd.mu.Lock()
	defer cd.mu.Unlock()
	cd.opts.OutputFile = path
	// Only re-derive ResumeFile when it was auto-synthesized in
	// NewChatDownloader. Tracking via a flag avoids the literal-string
	// footgun where a caller passes ResumeFile=".resume.json" explicitly
	// and gets it silently overwritten (audit chat.md C7).
	if cd.resumeFileAuto {
		cd.opts.ResumeFile = path + ".resume.json"
	}
}

// getOutputPaths copies the output and resume file paths under lock.
// Callers running outside the main loop (or after SetOutputFile may be called)
// should use these copies instead of reading cd.opts directly.
func (cd *ChatDownloader) getOutputPaths() (outputFile, resumeFile string) {
	cd.mu.Lock()
	outputFile = cd.opts.OutputFile
	resumeFile = cd.opts.ResumeFile
	cd.mu.Unlock()
	return
}

// writeChatFile writes chat data to the output file.
// Two paths:
//   - Not flushed: all messages are in memory, write complete JSON atomically.
//   - Flushed: the on-disk file already has old messages. Use incremental append:
//     read only the last bytes to locate ']', truncate there, then append new
//     messages + closing structure. Memory cost: O(new messages) not O(file size).
//
// On success, clears the in-memory buffer and marks flushedToDisk = true.
func (cd *ChatDownloader) writeChatFile() {
	outputFile, _ := cd.getOutputPaths()
	if outputFile == "" {
		return // No output file set yet (early chat), buffer in memory
	}
	if err := os.MkdirAll(filepath.Dir(outputFile), 0o755); err != nil {
		return
	}

	if !cd.flushedToDisk {
		// All messages in memory — write complete file atomically
		cd.writeFullChatFile()
		cd.messages = nil // All written to disk, free memory
		cd.flushedToDisk = true
		return
	}

	// Incremental append: open existing file and append new messages.
	// If any step fails, fall back to a full rewrite that prepends on-disk
	// messages — both paths clear the in-memory buffer on success.
	if cd.incrementalAppend(outputFile) {
		cd.messages = nil
	} else {
		cd.prependExistingMessages(outputFile)
		cd.writeFullChatFile()
		cd.messages = nil
	}
}

// incrementalAppend performs an in-place append of cd.messages to the existing
// chat file on disk via utils.AppendChatMessages. Returns true on success or
// on the truncate-then-write-failure path (file broken but caller should
// advance in-memory state, per utils.ErrChatFilePartialWrite). Returns false
// when the caller should fall back to a full rewrite.
func (cd *ChatDownloader) incrementalAppend(outputFile string) bool {
	newMessages := cd.messages
	if len(newMessages) == 0 {
		return true
	}

	err := utils.AppendChatMessages(outputFile, newMessages, cd.messageCount, chatWarnAdapter{cd})
	if err == nil {
		return true
	}
	cd.reportIOError(fmt.Errorf("chat file append: %w", err))
	if errors.Is(err, utils.ErrChatFilePartialWrite) {
		// File was truncated but WriteAt failed — falling back to full rewrite
		// would read the broken file, recover zero prior messages, and drop
		// history. Advance in-memory state instead; the C8 reportIOError path
		// already preserved the resume file (audit chat.md C8).
		return true
	}
	return false
}

func (cd *ChatDownloader) writeFullChatFile() {
	outputFile, _ := cd.getOutputPaths()

	data := ChatData{
		VideoID:         cd.opts.VideoID,
		VideoTitle:      cd.opts.VideoTitle,
		ChannelName:     cd.opts.ChannelName,
		StreamStartTime: cd.opts.StreamStartTime,
		DownloadedAt:    time.Now().UTC().Format(time.RFC3339),
		MessageCount:    cd.messageCount,
		Messages:        cd.messages,
	}

	if err := utils.WriteChatFileAtomic(outputFile, &data); err != nil {
		cd.reportIOError(fmt.Errorf("write chat file: %w", err))
	}
}

// readExistingMessages attempts to read previously-flushed messages from the
// chat file on disk. Returns nil on any error (caller should log a warning).
func (cd *ChatDownloader) readExistingMessages(path string) []ChatMessage {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var chatData ChatData
	if err := json.Unmarshal(data, &chatData); err != nil {
		return nil
	}
	return chatData.Messages
}

// prependExistingMessages reads previously-flushed messages from disk and prepends
// them to cd.messages. It also registers their IDs in seenIDs to prevent duplicates
// on subsequent API responses that may overlap with the recovered messages.
func (cd *ChatDownloader) prependExistingMessages(outputFile string) {
	existing := cd.readExistingMessages(outputFile)
	if existing == nil {
		return
	}
	// Locked for the same reason as processBatch: MessageCount() reads
	// messageCount from another goroutine.
	cd.mu.Lock()
	cd.messages = append(existing, cd.messages...)
	cd.messageCount = len(cd.messages)
	cd.mu.Unlock()
	// Register recovered message IDs in the dedup to prevent duplicates
	// on subsequent polls.
	for _, msg := range existing {
		if msg.ID != "" {
			cd.dedup.Add(msg.ID)
		}
	}
}

// updateChatFileHeader updates messageCount and downloadedAt in the JSON
// header without rewriting the entire file. IO failures route through
// reportIOError so the resume file is preserved (audit chat.md C8).
func (cd *ChatDownloader) updateChatFileHeader() {
	outputFile, _ := cd.getOutputPaths()
	if outputFile == "" {
		return // No output file set yet (early chat)
	}
	if err := utils.UpdateChatFileHeaderFields(outputFile, cd.messageCount); err != nil {
		cd.reportIOError(fmt.Errorf("update chat header: %w", err))
	}
}

func (cd *ChatDownloader) loadResume() (*ChatResumeState, error) {
	store := utils.ResumeStore[ChatResumeState]{Path: cd.opts.ResumeFile}
	state, err := store.Load()
	if err != nil {
		return nil, err
	}
	return &state, nil
}

func (cd *ChatDownloader) saveResume() {
	// Snapshot the fields under lock, then release before the ~200 KB marshal
	// + file write. Holding cd.mu for the entire write would block concurrent
	// MessageCount() / IsRunning() callers (TUI/web UI refreshers) on every
	// batched flush.
	cd.mu.Lock()
	outputFile := cd.opts.OutputFile
	resumeFile := cd.opts.ResumeFile
	if resumeFile == "" || outputFile == "" {
		cd.mu.Unlock()
		return // No output path yet (early chat)
	}
	// Deterministic insertion-order snapshot, capped to dedupKeepSize so the
	// resume file stays bounded.
	recentIDs := cd.dedup.Snapshot(dedupKeepSize)

	state := ChatResumeState{
		MessageCount: cd.messageCount,
		Continuation: cd.continuation,
		Timestamp:    time.Now().Unix(),
		VideoID:      cd.opts.VideoID,
		RecentIDs:    recentIDs,
	}
	cd.mu.Unlock()

	store := utils.ResumeStore[ChatResumeState]{Path: resumeFile}
	if err := store.Save(state); err != nil {
		if cd.OnError != nil {
			cd.OnError(fmt.Errorf("save resume state: %w", err))
		}
	}
}

func (cd *ChatDownloader) clearResume() {
	_, resumeFile := cd.getOutputPaths()
	store := utils.ResumeStore[ChatResumeState]{Path: resumeFile}
	if err := store.Clear(); err != nil {
		// Not fatal, but stale state left on disk could cause the next
		// Start() to reload resume data from a superseded run. Route
		// through OnError so operators see permission/AV-lock problems.
		if cd.OnError != nil {
			cd.OnError(fmt.Errorf("clear resume state: %w", err))
		}
	}
}

// sleep waits for the given duration, but returns early if the context is cancelled
// (which happens on Stop() or MarkStreamEnded()).
func (cd *ChatDownloader) sleep(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
