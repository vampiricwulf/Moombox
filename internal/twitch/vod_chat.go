package twitch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

const (
	vodChatMaxConsecutiveErrors = 5
	vodChatFlushInterval        = 5 * time.Second
	vodChatResumeMaxRecentIDs   = 1000
)

// VodChatDownloader downloads chat messages from a Twitch VOD.
//
// **Concurrency contract** (audit twitch.md #1):
//
//   - Start() owns the long-running goroutine and is the only writer of
//     `messages`. flush() and writeFullFile() are called only from Start's
//     goroutine, so the in-memory message slice is single-goroutine by
//     construction.
//   - Cross-goroutine reads use the atomic counters: `totalCount` (via
//     MessageCount), `running` (via IsRunning).
//   - `dedup` is utils.OrderedDedup which carries its own mutex.
//   - `onProgress` is guarded by `onProgressMu` (RWMutex; reassign-safe).
//
// Stop() cancels the ctx and waits via `running.Load()`; it does NOT
// touch `messages` directly. Callers must NOT read `messages` from
// outside Start's goroutine — use MessageCount() instead.
type VodChatDownloader struct {
	api           *API
	vodID         string
	channelLogin  string
	channelName   string
	channelID     string
	authToken     string
	outputPath    string
	vodDuration   int                 // seconds, used for progress % estimation
	vodStartMs    int64               // epoch ms when VOD started
	messages      []TwitchChatMessage // single-writer: Start goroutine only
	dedup         *utils.OrderedDedup[string]
	totalCount    atomic.Int64
	running       atomic.Bool
	emoteResolver *EmoteResolver

	logger interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}

	// onProgress is read from reportProgress under onProgressMu; callers
	// must use SetOnProgress rather than direct field assignment to avoid a
	// data race if the callback is reassigned after Start (audit
	// reports/worker.md F3).
	onProgressMu sync.RWMutex
	onProgress   func(count int)
}

// SetOnProgress installs the progress callback. Safe to call before or
// after Start.
func (vcd *VodChatDownloader) SetOnProgress(fn func(count int)) {
	vcd.onProgressMu.Lock()
	vcd.onProgress = fn
	vcd.onProgressMu.Unlock()
}

// callOnProgress snapshots the callback under the lock and invokes it
// outside.
func (vcd *VodChatDownloader) callOnProgress(count int) {
	vcd.onProgressMu.RLock()
	fn := vcd.onProgress
	vcd.onProgressMu.RUnlock()
	if fn != nil {
		fn(count)
	}
}

// VodChatOptions configures the VOD chat downloader.
type VodChatOptions struct {
	VodID         string
	ChannelLogin  string
	ChannelName   string
	ChannelID     string
	AuthToken     string
	OutputPath    string
	VodDuration   int   // seconds, used for progress % estimation
	VodStartMs    int64 // epoch ms when VOD started, for computing absolute timestamps
	EmoteResolver *EmoteResolver
}

// NewVodChatDownloader creates a new VOD chat downloader.
func NewVodChatDownloader(api *API, opts VodChatOptions, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *VodChatDownloader {
	return &VodChatDownloader{
		api:           api,
		vodID:         opts.VodID,
		channelLogin:  opts.ChannelLogin,
		channelName:   opts.ChannelName,
		channelID:     opts.ChannelID,
		authToken:     opts.AuthToken,
		outputPath:    opts.OutputPath,
		vodDuration:   opts.VodDuration,
		vodStartMs:    opts.VodStartMs,
		emoteResolver: opts.EmoteResolver,
		dedup:         utils.NewOrderedDedup[string](),
		logger:        logger,
	}
}

// Start downloads all VOD chat comments.
func (vcd *VodChatDownloader) Start(ctx context.Context) error {
	vcd.running.Store(true)
	defer vcd.running.Store(false)
	defer func() {
		if r := recover(); r != nil {
			vcd.logger.Error("VOD chat downloader panic", "panic", r, "vodID", vcd.vodID)
		}
	}()

	vcd.logger.Info("starting VOD chat download", "vodID", vcd.vodID)

	var contentOffset float64
	consecutiveErrors := 0

	// Try loading resume state
	if state, err := vcd.loadResumeState(); err == nil && state != nil {
		if state.StreamID == vcd.vodID {
			contentOffset = state.LastOffsetSeconds
			vcd.totalCount.Store(int64(state.MessageCount))
			vcd.dedup.Restore(state.RecentIDs)
			vcd.logger.Info("resumed VOD chat download",
				"vodID", vcd.vodID,
				"offset", contentOffset,
				"previousMessages", vcd.totalCount.Load(),
			)
		}
	}

	lastFlush := time.Now()

	for vcd.running.Load() {
		select {
		case <-ctx.Done():
			vcd.flush()
			vcd.saveResumeState(contentOffset)
			return nil
		default:
		}

		edges, hasNext, err := vcd.api.GetVodComments(ctx, vcd.vodID, contentOffset, vcd.authToken)
		if err != nil {
			consecutiveErrors++
			if consecutiveErrors >= vodChatMaxConsecutiveErrors {
				vcd.flush()
				vcd.saveResumeState(contentOffset)
				return fmt.Errorf("too many VOD chat errors: %w", err)
			}
			vcd.logger.Warn("vod chat fetch error", "err", err, "consecutive", consecutiveErrors)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Duration(consecutiveErrors) * time.Second):
			}
			continue
		}
		consecutiveErrors = 0

		newCount := 0
		for _, edge := range edges {
			if !vcd.dedup.Add(edge.ID) {
				continue
			}

			authorName := edge.CommenterDisplayName
			if authorName == "" {
				authorName = "Deleted User"
			}

			offsetMs := int64(edge.ContentOffsetSeconds * 1000)
			timestampMs := offsetMs
			if vcd.vodStartMs > 0 {
				timestampMs = vcd.vodStartMs + offsetMs
			}

			msg := TwitchChatMessage{
				ID:           edge.ID,
				TimestampMs:  timestampMs,
				OffsetMs:     offsetMs,
				AuthorName:   authorName,
				AuthorID:     edge.CommenterID,
				AuthorBadges: edge.UserBadges,
				AuthorColor:  edge.UserColor,
				Message:      edge.MessageText,
				Emotes:       edge.Emotes,
				MessageType:  "chat",
			}

			vcd.messages = append(vcd.messages, msg)
			vcd.totalCount.Add(1)
			newCount++
		}

		// Once per page, not per message — reportProgress logs an Info line,
		// and per-message it floods the log with thousands of identical
		// entries on comment-heavy VODs.
		if newCount > 0 {
			vcd.reportProgress(contentOffset)
		}

		// Server returned no results — end of VOD comments
		if len(edges) == 0 {
			vcd.logger.Info("[TwitchVodChat] Reached end of VOD comments")
			break
		}

		// If this page was entirely duplicates AND the server has no more
		// pages, we're done. Previously we broke on newCount==0 alone,
		// which killed the download on resume whenever the first page
		// back matched already-seen IDs — the rest of the VOD past that
		// offset was never archived. With hasNext==true we keep paging.
		if newCount == 0 && !hasNext {
			break
		}

		if !hasNext {
			break
		}

		// Advance offset to the last edge's offset so the next page
		// moves forward in time even when the current page was entirely
		// duplicates (newCount==0 with hasNext==true).
		if len(edges) > 0 {
			newOffset := edges[len(edges)-1].ContentOffsetSeconds
			// Guard against a pathological server response that never
			// advances the offset — break to avoid an infinite loop.
			if newCount == 0 && newOffset <= contentOffset {
				vcd.logger.Warn("[TwitchVodChat] offset did not advance on all-duplicate page; stopping",
					"offset", contentOffset)
				break
			}
			contentOffset = newOffset
		}

		// Periodic flush every 5 seconds
		if time.Since(lastFlush) >= vodChatFlushInterval {
			vcd.flush()
			vcd.saveResumeState(contentOffset)
			lastFlush = time.Now()
		}

		// Prune dedup — keep most recent chatDedupMax entries by insertion order
		if vcd.dedup.Len() > chatDedupMax*2 {
			vcd.dedup.Keep(chatDedupMax)
		}
	}

	// Distinguish Stop() (orchestrator's post-video chat timeout — pagination
	// forcibly cut short) from natural completion (loop exits via break when
	// the server has no more pages). On Stop, preserve the resume state so a
	// retry continues from this offset, and skip enrichment — the chat is
	// incomplete and an enriched-then-resumed file must not be re-appended.
	if !vcd.running.Load() {
		vcd.flush()
		vcd.saveResumeState(contentOffset)
		vcd.logger.Info("VOD chat download stopped before completion; resume state preserved",
			"vodID", vcd.vodID, "offset", contentOffset, "messages", vcd.totalCount.Load())
		return nil
	}

	vcd.flush()

	// Resolve and inject third-party emotes (7TV, BTTV, FFZ).
	// Use a fresh context — the original ctx may already be cancelled.
	if vcd.totalCount.Load() > 0 && vcd.emoteResolver != nil && vcd.channelID != "" {
		vcd.logger.Info("resolving emotes for VOD chat", "channelID", vcd.channelID)
		emoteCtx, emoteCancel := context.WithTimeout(context.Background(), 30*time.Second)
		emoteData := vcd.emoteResolver.Resolve(emoteCtx, vcd.channelID, vcd.channelLogin)
		emoteCancel()
		if emoteData != nil {
			if err := EnrichWithEmotes(vcd.outputPath, emoteData); err != nil {
				vcd.logger.Warn("emote injection failed", "err", err)
			}
		}
	}

	vcd.removeResumeState()
	vcd.logger.Info("VOD chat download complete", "vodID", vcd.vodID, "messages", vcd.totalCount.Load())
	return nil
}

func (vcd *VodChatDownloader) flush() {
	if len(vcd.messages) == 0 || vcd.outputPath == "" {
		return
	}

	dir := filepath.Dir(vcd.outputPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		vcd.logger.Error("create vod chat output dir", "err", err)
		return
	}

	// Check if this is the first flush (file doesn't exist yet)
	_, statErr := os.Stat(vcd.outputPath)
	isFirstFlush := statErr != nil

	if isFirstFlush {
		// First flush: write complete file
		if err := vcd.writeFullFile(vcd.messages); err != nil {
			vcd.logger.Error("write vod chat file", "err", err)
			return
		}
	} else {
		// Subsequent flushes: append new messages to existing file
		count := int(vcd.totalCount.Load())
		appendErr := utils.AppendChatMessages(vcd.outputPath, vcd.messages, vcd.logger)
		switch {
		case appendErr == nil:
			if err := utils.UpdateChatFileHeaderFields(vcd.outputPath, count); err != nil {
				vcd.logger.Warn("update vod chat header", "err", err)
			}
		case errors.Is(appendErr, utils.ErrChatFilePartialWrite):
			// Truncated-then-failed write: the on-disk tail is broken, and a
			// merge would parse-fail (dropping history). Per the sentinel's
			// contract, advance past the batch.
			vcd.logger.Error("partial vod chat append; advancing past batch", "err", appendErr)
		default:
			// Merge-and-rewrite fallback (mirrors the IRC path): rewriting
			// with only the current batch would replace hours of flushed
			// comments with the last few seconds' worth.
			vcd.logger.Warn("append failed, merging existing file with current batch", "err", appendErr)
			existing, readErr := readChatFileMessages(vcd.outputPath)
			if readErr != nil {
				// Can't recover without destroying data — keep the batch in
				// memory and retry on the next flush.
				vcd.logger.Error("append failed and cannot read existing file for merge; preserving file, retrying next flush", "err", readErr)
				return
			}
			merged := append(existing, vcd.messages...)
			if err := vcd.writeFullFile(merged); err != nil {
				vcd.logger.Error("fallback merged write failed", "err", err)
				return
			}
		}
	}

	// Clear messages from memory after successful write to prevent unbounded growth
	vcd.messages = vcd.messages[:0]
}

// writeFullFile writes the complete file atomically using the shared helper.
func (vcd *VodChatDownloader) writeFullFile(msgs []TwitchChatMessage) error {
	chatData := TwitchChatData{
		Platform:           "twitch",
		ChannelLogin:       vcd.channelLogin,
		ChannelDisplayName: vcd.channelName,
		StreamID:           vcd.vodID,
		DownloadedAt:       time.Now().UTC().Format(time.RFC3339),
		MessageCount:       int(vcd.totalCount.Load()),
		Messages:           msgs,
	}
	return utils.WriteChatFileAtomic(vcd.outputPath, &chatData)
}

// reportProgress calls OnProgress and logs percentage if vodDuration is known.
func (vcd *VodChatDownloader) reportProgress(contentOffset float64) {
	count := int(vcd.totalCount.Load())
	vcd.callOnProgress(count)
	if vcd.vodDuration > 0 {
		pct := min(contentOffset/float64(vcd.vodDuration)*100, 100)
		vcd.logger.Info("VOD chat progress",
			"messages", count,
			"percent", fmt.Sprintf("%.1f%%", pct),
			"offsetSeconds", fmt.Sprintf("%.0f", contentOffset),
			"durationSeconds", vcd.vodDuration,
		)
	}
}

// resumeStatePath returns the sidecar resume state file path.
func (vcd *VodChatDownloader) resumeStatePath() string {
	return vcd.outputPath + ".resume.json"
}

// loadResumeState attempts to load a resume state from the sidecar JSON file.
func (vcd *VodChatDownloader) loadResumeState() (*ChatResumeState, error) {
	store := utils.ResumeStore[ChatResumeState]{Path: vcd.resumeStatePath()}
	state, err := store.Load()
	if err != nil {
		return nil, err
	}
	return &state, nil
}

// saveResumeState writes the current resume state to the sidecar JSON file.
func (vcd *VodChatDownloader) saveResumeState(contentOffset float64) {
	if vcd.outputPath == "" {
		return
	}
	// Deterministic insertion-order snapshot capped to bound the resume file.
	recentIDs := vcd.dedup.Snapshot(vodChatResumeMaxRecentIDs)

	state := ChatResumeState{
		MessageCount:      int(vcd.totalCount.Load()),
		LastOffsetSeconds: contentOffset,
		Timestamp:         time.Now().Unix(),
		StreamID:          vcd.vodID,
		RecentIDs:         recentIDs,
	}

	store := utils.ResumeStore[ChatResumeState]{Path: vcd.resumeStatePath()}
	if err := store.Save(state); err != nil {
		vcd.logger.Error("save vod chat resume state", "err", err)
	}
}

// removeResumeState deletes the sidecar resume state file after successful completion.
func (vcd *VodChatDownloader) removeResumeState() {
	if vcd.outputPath == "" {
		return
	}
	store := utils.ResumeStore[ChatResumeState]{Path: vcd.resumeStatePath()}
	if err := store.Clear(); err != nil {
		vcd.logger.Warn("remove vod chat resume state", "err", err)
	}
}

// MessageCount returns the total number of messages collected.
func (vcd *VodChatDownloader) MessageCount() int {
	return int(vcd.totalCount.Load())
}

// IsRunning returns whether the downloader is currently running.
func (vcd *VodChatDownloader) IsRunning() bool {
	return vcd.running.Load()
}

// Stop cancels the VOD chat download.
func (vcd *VodChatDownloader) Stop() {
	vcd.running.Store(false)
}

// MarkStreamEnded signals that the stream has ended (no-op for VODs).
// VOD chat downloads run to completion when all pages are fetched, so
// this is intentionally a no-op.
func (vcd *VodChatDownloader) MarkStreamEnded() {
	// No-op for VOD — download completes when all pages are fetched.
}

// EnrichWithEmotes adds resolved emote data to the chat output.
func EnrichWithEmotes(chatPath string, emoteData *TwitchEmoteData) error {
	data, err := os.ReadFile(chatPath)
	if err != nil {
		return err
	}

	var chatData TwitchChatData
	if err := json.Unmarshal(data, &chatData); err != nil {
		return err
	}

	chatData.Emotes = emoteData

	out, err := json.MarshalIndent(chatData, "", "  ")
	if err != nil {
		return err
	}
	out = utils.PadMessageCountJSON(out)

	tmpPath := chatPath + ".tmp"
	if err := os.WriteFile(tmpPath, out, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, chatPath)
}

// BuildChatFilename generates the chat output filename for a Twitch stream/VOD.
func BuildChatFilename(channelLogin, streamOrVodID string) string {
	return fmt.Sprintf("%s_%s_chat.json", strings.ToLower(channelLogin), streamOrVodID)
}
