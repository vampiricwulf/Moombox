package twitch

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

const (
	chatMaxConsecutiveErrs = 20
	chatDedupMax           = 5000
	chatSaveInterval       = 1 * time.Second
	// ircReadDeadline bounds each conn.Read in runIRCSession. Twitch sends
	// PING every ~5 min; this gives us one missed heartbeat plus slack
	// before we treat the socket as dead and trigger the reconnect path.
	ircReadDeadline = 6 * time.Minute
)

// ChatDownloader connects to Twitch IRC and records live chat messages.
type ChatDownloader struct {
	mu sync.Mutex
	// flushMu serializes flush() — it's called from the session goroutine
	// (reconnect/exit paths) and from the periodic flusher goroutine; two
	// interleaved flushes would snapshot overlapping batches and
	// double-append them to the chat file.
	flushMu        sync.Mutex
	channelLogin   string
	channelDisplay string
	channelID      string
	streamID       string
	authToken      string
	// recordingStartMs is the OffsetMs base for the CURRENT part file.
	// Atomic: the IRC session goroutine reads it per message while RollFile
	// rebases it at part boundaries from the orchestrator goroutine.
	recordingStartMs atomic.Int64
	streamStartTime  string
	streamStartMs    int64
	messages         []TwitchChatMessage // Unwritten messages in memory
	dedup            *utils.OrderedDedup[string]
	outputPath       string
	running          bool
	streamEnded      bool  // set by MarkStreamEnded — distinguishes drain from interruption
	totalCount       int   // cumulative across all part files (job-level metric)
	fileCount        int   // messages belonging to the CURRENT part file (header count)
	lastTimestampMs  int64 // Last message timestamp (epoch ms) for resume state
	flushedToDisk    bool
	emoteResolver    *EmoteResolver
	// emoteData caches the third-party emote resolve so multi-part jobs hit
	// the 7TV/BTTV/FFZ APIs once, not once per part. Guarded by emoteMu,
	// which is held across the resolve itself to single-flight concurrent
	// callers (background part mux + stream-end drain).
	emoteMu   sync.Mutex
	emoteData *TwitchEmoteData

	logger interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}

	// onProgress is read from addMessage under onProgressMu; callers must
	// use SetOnProgress rather than direct field assignment to avoid a
	// data race if the callback is reassigned after Start (audit
	// reports/worker.md F3).
	onProgressMu sync.RWMutex
	onProgress   func(count int)
}

// SetOnProgress installs the progress callback. Safe to call before or
// after Start — the IRC session goroutine reads via callOnProgress under
// the same lock.
func (cd *ChatDownloader) SetOnProgress(fn func(count int)) {
	cd.onProgressMu.Lock()
	cd.onProgress = fn
	cd.onProgressMu.Unlock()
}

// callOnProgress snapshots the current progress callback under the lock and
// invokes it outside the lock so a slow callback doesn't block a concurrent
// SetOnProgress.
func (cd *ChatDownloader) callOnProgress(count int) {
	cd.onProgressMu.RLock()
	fn := cd.onProgress
	cd.onProgressMu.RUnlock()
	if fn != nil {
		fn(count)
	}
}

// ChatDownloaderOptions configures the chat downloader.
type ChatDownloaderOptions struct {
	ChannelLogin    string
	ChannelDisplay  string
	ChannelID       string
	StreamID        string
	AuthToken       string
	OutputPath      string
	StreamStartTime string
	EmoteResolver   *EmoteResolver
}

// NewChatDownloader creates a new IRC chat downloader.
func NewChatDownloader(opts ChatDownloaderOptions, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *ChatDownloader {
	var streamStartMs int64
	if opts.StreamStartTime != "" {
		if t, err := time.Parse(time.RFC3339, opts.StreamStartTime); err == nil {
			streamStartMs = t.UnixMilli()
		}
	}

	return &ChatDownloader{
		channelLogin:    opts.ChannelLogin,
		channelDisplay:  opts.ChannelDisplay,
		channelID:       opts.ChannelID,
		streamID:        opts.StreamID,
		authToken:       opts.AuthToken,
		outputPath:      opts.OutputPath,
		streamStartTime: opts.StreamStartTime,
		streamStartMs:   streamStartMs,
		dedup:           utils.NewOrderedDedup[string](),
		emoteResolver:   opts.EmoteResolver,
		logger:          logger,
	}
}

// SetRecordingStartTime sets the recording start time for offset calculation.
// Should be called before Start() when the actual recording begins.
func (cd *ChatDownloader) SetRecordingStartTime(isoString string) {
	if t, err := time.Parse(time.RFC3339, isoString); err == nil {
		cd.recordingStartMs.Store(t.UnixMilli())
	}
}

// chatResumePath returns the resume-state sidecar path for a chat file. The
// suffix lives here exclusively — RollFile clears the CLOSED part's sidecar
// by the same rule that getResumeFilePath derives the current one.
func chatResumePath(chatPath string) string {
	return chatPath + ".resume.json"
}

// currentOutputPath snapshots the current part's chat path under cd.mu —
// required anywhere outside flushMu, because RollFile swaps outputPath from
// the orchestrator goroutine.
func (cd *ChatDownloader) currentOutputPath() string {
	cd.mu.Lock()
	defer cd.mu.Unlock()
	return cd.outputPath
}

// getResumeFilePath returns the path to the resume state file.
func (cd *ChatDownloader) getResumeFilePath() string {
	return chatResumePath(cd.currentOutputPath())
}

// loadResumeState loads the resume state from disk.
// Returns nil if no valid resume state exists for the current stream.
//
// Both the IRC and VOD paths share the exported ChatResumeState type from
// types.go — the previously separate `twitchChatResumeState` mirror was
// dropped (audit-finding twitch.md #43). LastOffsetSeconds is unused on the
// IRC side and serializes as 0; LastTimestampMs is unused on the VOD side.
func (cd *ChatDownloader) loadResumeState() *ChatResumeState {
	store := utils.ResumeStore[ChatResumeState]{Path: cd.getResumeFilePath()}
	state, err := store.Load()
	if err != nil {
		return nil
	}
	// Only use resume state if it matches the current stream
	if state.StreamID != cd.streamID {
		return nil
	}
	return &state
}

// saveResumeState persists the current chat state for resume after crash/reconnect.
// Uses atomic write pattern with .tmp file (matches TS saveResumeState).
func (cd *ChatDownloader) saveResumeState() {
	// Path captured in the SAME critical section as the counters — a
	// concurrent RollFile must not pair one part's counts with the other
	// part's sidecar.
	cd.mu.Lock()
	recentIDs := cd.dedup.Snapshot(0)
	state := ChatResumeState{
		MessageCount:    cd.fileCount,
		TotalCount:      cd.totalCount,
		LastTimestampMs: cd.lastTimestampMs,
		Timestamp:       time.Now().UnixMilli(),
		StreamID:        cd.streamID,
		RecentIDs:       recentIDs,
	}
	resumePath := chatResumePath(cd.outputPath)
	cd.mu.Unlock()

	store := utils.ResumeStore[ChatResumeState]{Path: resumePath}
	if err := store.Save(state); err != nil {
		cd.logger.Warn("save chat resume state", "err", err)
	}
}

// restoreResumeState applies a loaded resume snapshot to the downloader's
// counters and dedup set. fileCount continues the current part file's count;
// totalCount falls back to MessageCount for states written before
// part-splitting existed (one file meant the file count WAS the total).
//
// flushedToDisk is restored only when the chat file actually exists. A
// resume state without its file happens when a part was rolled and the
// daemon stopped before any message arrived (the exit path saves state for
// the new, never-written part) — blindly marking it flushed would route the
// first flush onto the append path against a missing file, which fails,
// merge-fails, and retries forever: the part's chat would never reach disk.
func (cd *ChatDownloader) restoreResumeState(state *ChatResumeState) {
	fileExists := false
	if path := cd.currentOutputPath(); path != "" {
		if _, err := os.Stat(path); err == nil {
			fileExists = true
		}
	}

	cd.mu.Lock()
	if fileExists {
		cd.fileCount = state.MessageCount
		cd.flushedToDisk = true
	} else {
		cd.fileCount = 0
		cd.flushedToDisk = false
	}
	cd.totalCount = max(state.TotalCount, state.MessageCount)
	cd.lastTimestampMs = state.LastTimestampMs
	cd.dedup.Restore(state.RecentIDs)
	cd.mu.Unlock()
}

// clearResumeState deletes the resume state file on successful completion.
func (cd *ChatDownloader) clearResumeState() {
	store := utils.ResumeStore[ChatResumeState]{Path: cd.getResumeFilePath()}
	if err := store.Clear(); err != nil {
		cd.logger.Warn("remove chat resume state", "err", err)
	}
}

// Start connects to Twitch IRC and begins recording chat messages.
// C1: Includes reconnect logic with exponential backoff on error limit.
//
// Start is safe to call once per ChatDownloader instance. Calling Start
// concurrently (or after a previous Start returns) returns an error rather
// than racing on the dedup/resume state — the struct retains seenIDs and
// seenOrder across calls, and re-initialising them while a previous session
// is still draining would drop messages.
func (cd *ChatDownloader) Start(ctx context.Context) error {
	cd.mu.Lock()
	if cd.running {
		cd.mu.Unlock()
		return fmt.Errorf("twitch chat downloader already running for %s", cd.channelLogin)
	}
	cd.running = true
	alreadyInitialized := cd.totalCount > 0 || cd.dedup.Len() > 0
	cd.mu.Unlock()

	// Try to resume from saved state (matches TS start() resume logic).
	// Only load resume state on a fresh Start — if the downloader already
	// has in-memory state from a prior session, preserve it rather than
	// replacing with the on-disk snapshot.
	if !alreadyInitialized {
		if resumeState := cd.loadResumeState(); resumeState != nil && len(resumeState.RecentIDs) > 0 {
			cd.restoreResumeState(resumeState)
			cd.logger.Info("[TwitchChat] Resuming from saved state",
				"fileMessages", resumeState.MessageCount, "totalMessages", cd.MessageCount())
		}
	}

	defer func() {
		panicked := false
		if r := recover(); r != nil {
			cd.logger.Error("chat downloader panic", "panic", r)
			panicked = true
		}

		cd.mu.Lock()
		cd.running = false
		streamEnded := cd.streamEnded
		cd.mu.Unlock()
		cd.flush()

		if panicked {
			// Don't clear resume state on panic — allow resume on restart
			return
		}

		if !streamEnded {
			// Interrupted exit (Stop() on shutdown/user-cancel, ctx
			// cancellation, reconnect exhaustion) — NOT the end of the
			// stream. Preserve resume state so the resumed session appends
			// to chat.json instead of rewriting it from scratch (clearing
			// here used to destroy all previously archived chat), and skip
			// emote enrichment: enriched files must not receive appends.
			cd.saveResumeState()
			return
		}

		// Stream-over drain: clear resume state
		cd.clearResumeState()

		// Inject third-party emotes (7TV, BTTV, FFZ) after final flush.
		// Use a fresh context -- the original ctx may already be cancelled.
		// Cached resolve: parts rolled earlier in the job already resolved
		// the emote set, so the final part reuses it. flushedToDisk gates
		// the case where every message landed in earlier parts and the
		// final part's file was never created.
		cd.mu.Lock()
		finalFileExists := cd.flushedToDisk
		cd.mu.Unlock()
		if cd.totalCount > 0 && finalFileExists && cd.emoteResolver != nil && cd.channelID != "" {
			cd.logger.Info("resolving emotes for Twitch chat", "channelID", cd.channelID)
			emoteCtx, emoteCancel := context.WithTimeout(context.Background(), 30*time.Second)
			emoteData := cd.resolveEmotesCached(emoteCtx)
			emoteCancel()
			if emoteData != nil {
				if err := EnrichWithEmotes(cd.currentOutputPath(), emoteData); err != nil {
					cd.logger.Warn("emote injection failed", "err", err)
				}
			}
		}
	}()

	// C1: Reconnect loop -- on error limit, save state and reconnect.
	// reconnectAttempts is reset after any session that stayed connected for
	// longer than reconnectResetUptime. Long-running (8+ hour) streams
	// previously exhausted the counter on sparse network hiccups and then
	// gave up chat for the remainder of the stream.
	const (
		maxReconnects        = 10
		reconnectResetUptime = 5 * time.Minute
	)
	reconnectAttempts := 0

	for reconnectAttempts <= maxReconnects {
		if ctx.Err() != nil || !cd.IsRunning() {
			return nil
		}

		if reconnectAttempts > 0 {
			// Exponential backoff: 1000 * 2^attempts, capped at 30s (matches TypeScript)
			shift := min(reconnectAttempts, 15) // cap shift to prevent overflow
			delayMs := min(1000*(1<<shift), 30000)
			delay := time.Duration(delayMs) * time.Millisecond
			cd.logger.Info("reconnecting to twitch IRC",
				"channel", cd.channelLogin, "attempt", reconnectAttempts, "max", maxReconnects, "delay", delay)
			cd.flush() // Save state before reconnect
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(delay):
			}
		}

		sessionStart := time.Now()
		err := cd.runIRCSession(ctx)
		sessionUptime := time.Since(sessionStart)

		if err == nil || ctx.Err() != nil || !cd.IsRunning() {
			return nil
		}

		// Session stayed connected long enough to be considered healthy —
		// treat the disconnect as an isolated hiccup and reset the counter.
		if sessionUptime >= reconnectResetUptime && reconnectAttempts > 0 {
			cd.logger.Info("IRC session was stable before disconnect; resetting reconnect counter",
				"channel", cd.channelLogin, "uptime", sessionUptime)
			reconnectAttempts = 0
		}

		reconnectAttempts++
		cd.logger.Warn("IRC session error, will reconnect", "err", err, "channel", cd.channelLogin)
	}

	return fmt.Errorf("exceeded max IRC reconnects for %s", cd.channelLogin)
}

func (cd *ChatDownloader) addMessage(msg *TwitchChatMessage) {
	cd.mu.Lock()
	defer cd.mu.Unlock()

	if !cd.dedup.Add(msg.ID) {
		return
	}
	// Prune at 2× threshold to amortize the Keep cost across inserts.
	if cd.dedup.Len() > chatDedupMax*2 {
		cd.dedup.Keep(chatDedupMax)
	}

	// OffsetMs is computed HERE, under the same lock RollFile holds to swap
	// the output file and rebase recordingStartMs — guaranteeing a message's
	// offset base always matches the part file it gets flushed into.
	// Computing it at parse time (outside the lock) let a message parsed
	// just before a roll land in the NEW part with an OLD-base offset,
	// replaying hours out of position.
	baseMs := cd.recordingStartMs.Load()
	if baseMs == 0 {
		baseMs = cd.streamStartMs
	}
	if baseMs > 0 {
		msg.OffsetMs = max(msg.TimestampMs-baseMs, 0)
	}

	cd.messages = append(cd.messages, *msg)
	cd.totalCount++
	cd.fileCount++
	if msg.TimestampMs > cd.lastTimestampMs {
		cd.lastTimestampMs = msg.TimestampMs
	}

	cd.callOnProgress(cd.totalCount)
}

// MessageCount returns the total number of messages collected.
func (cd *ChatDownloader) MessageCount() int {
	cd.mu.Lock()
	defer cd.mu.Unlock()
	return cd.totalCount
}

// IsRunning returns whether the downloader is currently running.
func (cd *ChatDownloader) IsRunning() bool {
	cd.mu.Lock()
	defer cd.mu.Unlock()
	return cd.running
}

// Stop cancels the chat download.
func (cd *ChatDownloader) Stop() {
	cd.mu.Lock()
	cd.running = false
	cd.mu.Unlock()
}

// MarkStreamEnded signals that the upstream live stream has ended and the
// chat downloader should drain. Unlike Stop (an interruption — shutdown or
// user cancel — after which the session must be resumable), MarkStreamEnded
// is a CLEAN end: Start's exit path clears the resume state and runs emote
// enrichment only on this flavour of shutdown. Audit-finding twitch.md #45 /
// full-project review 2026-06-09 (resume state destroyed on restart).
func (cd *ChatDownloader) MarkStreamEnded() {
	cd.mu.Lock()
	cd.streamEnded = true
	cd.running = false
	cd.mu.Unlock()
}
