package twitch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

const (
	chatMaxConsecutiveErrs = 20
	chatDedupMax           = 5000
	chatSaveInterval       = 1 * time.Second
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
	seenIDs          map[string]struct{}
	seenOrder        []string // Insertion-order tracking for deterministic culling
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

// twitchChatResumeState persists IRC chat state for resume after reconnection.
// Matches TypeScript TwitchChatResumeState interface.
type twitchChatResumeState struct {
	MessageCount    int      `json:"messageCount"`
	LastTimestampMs int64    `json:"lastTimestampMs"`
	Timestamp       int64    `json:"timestamp"`
	StreamID        string   `json:"streamId"`
	RecentIDs       []string `json:"recentIds,omitempty"`
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
		seenIDs:         make(map[string]struct{}),
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
func (cd *ChatDownloader) loadResumeState() *twitchChatResumeState {
	resumePath := cd.getResumeFilePath()
	data, err := os.ReadFile(resumePath)
	if err != nil {
		return nil
	}

	var state twitchChatResumeState
	if err := json.Unmarshal(data, &state); err != nil {
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
	recentIDs := make([]string, 0, len(cd.seenOrder))
	recentIDs = append(recentIDs, cd.seenOrder...)
	state := twitchChatResumeState{
		MessageCount:    cd.totalCount,
		LastTimestampMs: cd.lastTimestampMs,
		Timestamp:       time.Now().UnixMilli(),
		StreamID:        cd.streamID,
		RecentIDs:       recentIDs,
	}
	cd.mu.Unlock()

	data, err := json.Marshal(state)
	if err != nil {
		cd.logger.Warn("marshal chat resume state", "err", err)
		return
	}

	resumePath := cd.getResumeFilePath()
	tmpPath := resumePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		cd.logger.Warn("write chat resume state", "err", err)
		return
	}
	if err := os.Rename(tmpPath, resumePath); err != nil {
		cd.logger.Warn("rename chat resume state", "err", err)
	}
}

// clearResumeState deletes the resume state file on successful completion.
func (cd *ChatDownloader) clearResumeState() {
	if err := os.Remove(cd.getResumeFilePath()); err != nil && !os.IsNotExist(err) {
		cd.logger.Warn("remove chat resume state", "err", err)
	}
}

// Start connects to Twitch IRC and begins recording chat messages.
// C1: Includes reconnect logic with exponential backoff on error limit.
func (cd *ChatDownloader) Start(ctx context.Context) error {
	cd.mu.Lock()
	cd.running = true
	cd.mu.Unlock()

	// Try to resume from saved state (matches TS start() resume logic)
	resumeState := cd.loadResumeState()
	if resumeState != nil && len(resumeState.RecentIDs) > 0 {
		cd.mu.Lock()
		cd.totalCount = resumeState.MessageCount
		cd.lastTimestampMs = resumeState.LastTimestampMs
		cd.flushedToDisk = true
		cd.seenIDs = make(map[string]struct{}, len(resumeState.RecentIDs))
		cd.seenOrder = make([]string, 0, len(resumeState.RecentIDs))
		for _, id := range resumeState.RecentIDs {
			cd.seenIDs[id] = struct{}{}
			cd.seenOrder = append(cd.seenOrder, id)
		}
		cd.mu.Unlock()
		cd.logger.Info("[TwitchChat] Resuming from saved state", "messages", resumeState.MessageCount)
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

	// C1: Reconnect loop -- on error limit, save state and reconnect
	maxReconnects := 10
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

		err := cd.runIRCSession(ctx)
		if err == nil || ctx.Err() != nil || !cd.IsRunning() {
			return nil
		}
		reconnectAttempts++
		cd.logger.Warn("IRC session error, will reconnect", "err", err, "channel", cd.channelLogin)
	}

	return fmt.Errorf("exceeded max IRC reconnects for %s", cd.channelLogin)
}

func (cd *ChatDownloader) addMessage(msg *TwitchChatMessage) {
	cd.mu.Lock()
	defer cd.mu.Unlock()

	// Dedup
	if _, seen := cd.seenIDs[msg.ID]; seen {
		return
	}
	cd.seenIDs[msg.ID] = struct{}{}
	cd.seenOrder = append(cd.seenOrder, msg.ID)

	// Prune dedup map -- keep most recent chatDedupMax entries by insertion order
	if len(cd.seenOrder) > chatDedupMax*2 {
		removeIDs := cd.seenOrder[:len(cd.seenOrder)-chatDedupMax]
		for _, id := range removeIDs {
			delete(cd.seenIDs, id)
		}
		cd.seenOrder = cd.seenOrder[len(cd.seenOrder)-chatDedupMax:]
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

// MarkStreamEnded signals that the stream has ended.
func (cd *ChatDownloader) MarkStreamEnded() {
	cd.Stop()
}
