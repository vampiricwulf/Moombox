package twitch

import (
	"context"
	"fmt"
	"sync"
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
	mu               sync.Mutex
	channelLogin     string
	channelDisplay   string
	channelID        string
	streamID         string
	authToken        string
	recordingStartMs int64
	streamStartTime  string
	streamStartMs    int64
	messages         []TwitchChatMessage // Unwritten messages in memory
	dedup            *utils.OrderedDedup[string]
	outputPath       string
	running          bool
	totalCount       int
	lastTimestampMs  int64 // Last message timestamp (epoch ms) for resume state
	flushedToDisk    bool
	emoteResolver    *EmoteResolver

	logger interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}

	OnProgress func(count int)
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
		cd.mu.Lock()
		cd.recordingStartMs = t.UnixMilli()
		cd.mu.Unlock()
	}
}

// getResumeFilePath returns the path to the resume state file.
func (cd *ChatDownloader) getResumeFilePath() string {
	return cd.outputPath + ".resume.json"
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
	cd.mu.Lock()
	recentIDs := cd.dedup.Snapshot(0)
	state := ChatResumeState{
		MessageCount:    cd.totalCount,
		LastTimestampMs: cd.lastTimestampMs,
		Timestamp:       time.Now().UnixMilli(),
		StreamID:        cd.streamID,
		RecentIDs:       recentIDs,
	}
	cd.mu.Unlock()

	store := utils.ResumeStore[ChatResumeState]{Path: cd.getResumeFilePath()}
	if err := store.Save(state); err != nil {
		cd.logger.Warn("save chat resume state", "err", err)
	}
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
			cd.mu.Lock()
			cd.totalCount = resumeState.MessageCount
			cd.lastTimestampMs = resumeState.LastTimestampMs
			cd.flushedToDisk = true
			cd.dedup.Restore(resumeState.RecentIDs)
			cd.mu.Unlock()
			cd.logger.Info("[TwitchChat] Resuming from saved state", "messages", resumeState.MessageCount)
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
		cd.mu.Unlock()
		cd.flush()

		if panicked {
			// Don't clear resume state on panic — allow resume on restart
			return
		}

		// Clean exit: clear resume state
		cd.clearResumeState()

		// Inject third-party emotes (7TV, BTTV, FFZ) after final flush.
		// Use a fresh context -- the original ctx may already be cancelled.
		if cd.totalCount > 0 && cd.emoteResolver != nil && cd.channelID != "" {
			cd.logger.Info("resolving emotes for Twitch chat", "channelID", cd.channelID)
			emoteCtx, emoteCancel := context.WithTimeout(context.Background(), 30*time.Second)
			emoteData := cd.emoteResolver.Resolve(emoteCtx, cd.channelID, cd.channelLogin)
			emoteCancel()
			if emoteData != nil {
				if err := EnrichWithEmotes(cd.outputPath, emoteData); err != nil {
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

	cd.messages = append(cd.messages, *msg)
	cd.totalCount++
	if msg.TimestampMs > cd.lastTimestampMs {
		cd.lastTimestampMs = msg.TimestampMs
	}

	if cd.OnProgress != nil {
		cd.OnProgress(cd.totalCount)
	}
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
// chat downloader should drain. For IRC this is identical to Stop — both
// flip `running=false` and let the session loop unwind. The wrapper exists
// for symmetry with VodChatDownloader.MarkStreamEnded (which is a no-op
// because VOD pagination terminates on its own when the server says
// hasNextPage=false). Keeping both methods on the TwitchChatDownloader
// interface lets the orchestrator call MarkStreamEnded on either flavour
// without type-asserting. Audit-finding twitch.md #45.
func (cd *ChatDownloader) MarkStreamEnded() {
	cd.Stop()
}
