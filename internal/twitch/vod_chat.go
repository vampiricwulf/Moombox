package twitch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	vodChatMaxConsecutiveErrors = 5
	vodChatFlushInterval        = 5 * time.Second
	vodChatResumeMaxRecentIDs   = 1000
)

// VodChatDownloader downloads chat messages from a Twitch VOD.
type VodChatDownloader struct {
	api           *API
	vodID         string
	channelLogin  string
	channelName   string
	channelID     string
	authToken     string
	outputPath    string
	vodDuration   int   // seconds, used for progress % estimation
	vodStartMs    int64 // epoch ms when VOD started
	messages      []TwitchChatMessage
	seenIDs       map[string]struct{}
	totalCount    int
	running       atomic.Bool
	emoteResolver *EmoteResolver

	logger interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}

	OnProgress func(count int)
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
		seenIDs:       make(map[string]struct{}),
		logger:        logger,
	}
}

// Start downloads all VOD chat comments.
func (vcd *VodChatDownloader) Start(ctx context.Context) error {
	vcd.running.Store(true)
	defer vcd.running.Store(false)

	vcd.logger.Info("starting VOD chat download", "vodID", vcd.vodID)

	var contentOffset float64
	consecutiveErrors := 0

	// Try loading resume state
	if state, err := vcd.loadResumeState(); err == nil && state != nil {
		if state.StreamID == vcd.vodID {
			contentOffset = state.LastOffsetSeconds
			vcd.totalCount = state.MessageCount
			for _, id := range state.RecentIDs {
				vcd.seenIDs[id] = struct{}{}
			}
			vcd.logger.Info("resumed VOD chat download",
				"vodID", vcd.vodID,
				"offset", contentOffset,
				"previousMessages", vcd.totalCount,
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
			time.Sleep(2 * time.Duration(consecutiveErrors) * time.Second)
			continue
		}
		consecutiveErrors = 0

		newCount := 0
		for _, edge := range edges {
			if _, seen := vcd.seenIDs[edge.ID]; seen {
				continue
			}
			vcd.seenIDs[edge.ID] = struct{}{}

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
			vcd.totalCount++
			newCount++

			if vcd.OnProgress != nil {
				vcd.reportProgress(contentOffset)
			}
		}

		// Server returned no results — end of VOD comments
		if len(edges) == 0 {
			vcd.logger.Info("[TwitchVodChat] Reached end of VOD comments")
			break
		}

		// No new messages at this offset — we've exhausted duplicates
		if newCount == 0 {
			break
		}

		if !hasNext {
			break
		}

		// Advance offset to the last edge's offset
		if len(edges) > 0 {
			contentOffset = edges[len(edges)-1].ContentOffsetSeconds
		}

		// Periodic flush every 5 seconds
		if time.Since(lastFlush) >= vodChatFlushInterval {
			vcd.flush()
			vcd.saveResumeState(contentOffset)
			lastFlush = time.Now()
		}

		// Prune dedup map — keep chatDedupMax entries (TS: slice(-DEDUP_IDS))
		if len(vcd.seenIDs) > chatDedupMax {
			// Map iteration order is random in Go, but we just need to reduce
			// the size. Keep chatDedupMax entries by deleting the excess.
			excess := len(vcd.seenIDs) - chatDedupMax
			for id := range vcd.seenIDs {
				if excess <= 0 {
					break
				}
				delete(vcd.seenIDs, id)
				excess--
			}
		}
	}

	vcd.flush()

	// Resolve and inject third-party emotes (7TV, BTTV, FFZ)
	if vcd.totalCount > 0 && vcd.emoteResolver != nil && vcd.channelID != "" {
		vcd.logger.Info("resolving emotes for VOD chat", "channelID", vcd.channelID)
		emoteData := vcd.emoteResolver.Resolve(ctx, vcd.channelID, vcd.channelLogin)
		if emoteData != nil {
			if err := EnrichWithEmotes(vcd.outputPath, emoteData); err != nil {
				vcd.logger.Warn("emote injection failed", "err", err)
			}
		}
	}

	vcd.removeResumeState()
	vcd.logger.Info("VOD chat download complete", "vodID", vcd.vodID, "messages", vcd.totalCount)
	return nil
}

func (vcd *VodChatDownloader) flush() {
	if len(vcd.messages) == 0 || vcd.outputPath == "" {
		return
	}

	dir := filepath.Dir(vcd.outputPath)
	os.MkdirAll(dir, 0o755)

	// Check if this is the first flush (file doesn't exist yet)
	_, statErr := os.Stat(vcd.outputPath)
	isFirstFlush := statErr != nil

	if isFirstFlush {
		// First flush: write complete file
		chatData := TwitchChatData{
			Platform:           "twitch",
			ChannelLogin:       vcd.channelLogin,
			ChannelDisplayName: vcd.channelName,
			StreamID:           vcd.vodID,
			DownloadedAt:       time.Now().UTC().Format(time.RFC3339),
			MessageCount:       vcd.totalCount,
			Messages:           vcd.messages,
		}

		data, err := json.MarshalIndent(chatData, "", "  ")
		if err != nil {
			vcd.logger.Error("marshal vod chat data", "err", err)
			return
		}

		tmpPath := vcd.outputPath + ".tmp"
		if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
			vcd.logger.Error("write vod chat file", "err", err)
			return
		}
		os.Rename(tmpPath, vcd.outputPath)
	} else {
		// Subsequent flushes: append new messages to existing file
		if err := vcd.appendToFile(vcd.messages); err != nil {
			vcd.logger.Error("append vod chat file", "err", err)
			// Fallback: full rewrite
			vcd.writeFullFile()
		}
	}

	// Clear messages from memory after successful write to prevent unbounded growth
	vcd.messages = vcd.messages[:0]
}

// appendToFile appends new messages to the existing JSON file incrementally.
func (vcd *VodChatDownloader) appendToFile(msgs []TwitchChatMessage) error {
	if len(msgs) == 0 {
		return nil
	}

	info, err := os.Stat(vcd.outputPath)
	if err != nil || info.Size() < 10 {
		return fmt.Errorf("file missing or too small")
	}

	f, err := os.OpenFile(vcd.outputPath, os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	fileSize := info.Size()

	// Read last 10 bytes to find ']'
	tailSize := int64(10)
	if tailSize > fileSize {
		tailSize = fileSize
	}
	tailBuf := make([]byte, tailSize)
	if _, err := f.ReadAt(tailBuf, fileSize-tailSize); err != nil {
		return err
	}

	tail := string(tailBuf)
	bracketOffset := -1
	for i := len(tail) - 1; i >= 0; i-- {
		if tail[i] == ']' {
			bracketOffset = i
			break
		}
	}
	if bracketOffset == -1 {
		return fmt.Errorf("no closing bracket found")
	}

	bracketBytePos := fileSize - tailSize + int64(bracketOffset)

	// Check if there are existing messages
	hasExisting := false
	if bracketBytePos > 5 {
		checkSize := int64(5)
		if checkSize > bracketBytePos {
			checkSize = bracketBytePos
		}
		checkBuf := make([]byte, checkSize)
		f.ReadAt(checkBuf, bracketBytePos-checkSize)
		for i := len(checkBuf) - 1; i >= 0; i-- {
			if checkBuf[i] == '}' {
				hasExisting = true
				break
			} else if checkBuf[i] != ' ' && checkBuf[i] != '\n' && checkBuf[i] != '\r' && checkBuf[i] != '\t' {
				break
			}
		}
	}

	var appendStr string
	if hasExisting {
		appendStr = ",\n"
	}
	for i, msg := range msgs {
		msgBytes, err := json.Marshal(msg)
		if err != nil {
			continue
		}
		appendStr += "    " + string(msgBytes)
		if i < len(msgs)-1 {
			appendStr += ",\n"
		}
	}
	appendStr += "\n  ]\n}"

	if err := f.Truncate(bracketBytePos); err != nil {
		return err
	}
	_, err = f.WriteAt([]byte(appendStr), bracketBytePos)
	if err != nil {
		return err
	}

	// Update messageCount and downloadedAt in file header (matches TS appendToFile)
	vcd.updateVodChatFileHeader()

	return nil
}

// updateVodChatFileHeader updates messageCount and downloadedAt in the JSON header without rewriting.
func (vcd *VodChatDownloader) updateVodChatFileHeader() {
	info, err := os.Stat(vcd.outputPath)
	if err != nil || info.Size() < 50 {
		return
	}

	f, err := os.OpenFile(vcd.outputPath, os.O_RDWR, 0o644)
	if err != nil {
		return
	}
	defer f.Close()

	headerSize := int64(1024)
	if headerSize > info.Size() {
		headerSize = info.Size()
	}

	headerBuf := make([]byte, headerSize)
	n, err := f.ReadAt(headerBuf, 0)
	if err != nil && n == 0 {
		return
	}
	header := string(headerBuf[:n])

	// Replace messageCount value
	mcStart := strings.Index(header, `"messageCount":`)
	if mcStart >= 0 {
		mcValStart := mcStart + len(`"messageCount":`)
		for mcValStart < len(header) && (header[mcValStart] == ' ' || header[mcValStart] == '\t') {
			mcValStart++
		}
		mcValEnd := mcValStart
		for mcValEnd < len(header) && header[mcValEnd] >= '0' && header[mcValEnd] <= '9' {
			mcValEnd++
		}
		if mcValEnd > mcValStart {
			newVal := strconv.Itoa(vcd.totalCount)
			header = header[:mcValStart] + newVal + header[mcValEnd:]
		}
	}

	// Replace downloadedAt value (matches TS appendToFile header updates)
	replaceQuotedField(&header, `"downloadedAt":`, time.Now().UTC().Format(time.RFC3339))

	updatedBytes := []byte(header)
	if len(updatedBytes) == n {
		f.WriteAt(updatedBytes, 0)
	}
}

// writeFullFile writes the complete file as fallback (e.g., after append failure).
func (vcd *VodChatDownloader) writeFullFile() {
	chatData := TwitchChatData{
		Platform:           "twitch",
		ChannelLogin:       vcd.channelLogin,
		ChannelDisplayName: vcd.channelName,
		StreamID:           vcd.vodID,
		DownloadedAt:       time.Now().UTC().Format(time.RFC3339),
		MessageCount:       vcd.totalCount,
		Messages:           vcd.messages,
	}

	data, err := json.MarshalIndent(chatData, "", "  ")
	if err != nil {
		return
	}

	tmpPath := vcd.outputPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return
	}
	os.Rename(tmpPath, vcd.outputPath)
}

// reportProgress calls OnProgress and logs percentage if vodDuration is known.
func (vcd *VodChatDownloader) reportProgress(contentOffset float64) {
	if vcd.OnProgress != nil {
		vcd.OnProgress(vcd.totalCount)
	}
	if vcd.vodDuration > 0 {
		pct := contentOffset / float64(vcd.vodDuration) * 100
		if pct > 100 {
			pct = 100
		}
		vcd.logger.Info("VOD chat progress",
			"messages", vcd.totalCount,
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
	data, err := os.ReadFile(vcd.resumeStatePath())
	if err != nil {
		return nil, err
	}
	var state ChatResumeState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

// saveResumeState writes the current resume state to the sidecar JSON file.
func (vcd *VodChatDownloader) saveResumeState(contentOffset float64) {
	if vcd.outputPath == "" {
		return
	}

	// Collect recent IDs from seenIDs map (messages may be cleared after flush)
	recentIDs := make([]string, 0, vodChatResumeMaxRecentIDs)
	for id := range vcd.seenIDs {
		recentIDs = append(recentIDs, id)
		if len(recentIDs) >= vodChatResumeMaxRecentIDs {
			break
		}
	}

	state := ChatResumeState{
		MessageCount:      vcd.totalCount,
		LastOffsetSeconds: contentOffset,
		Timestamp:         time.Now().Unix(),
		StreamID:          vcd.vodID,
		RecentIDs:         recentIDs,
	}

	data, err := json.Marshal(state)
	if err != nil {
		vcd.logger.Error("marshal vod chat resume state", "err", err)
		return
	}

	tmpPath := vcd.resumeStatePath() + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		vcd.logger.Error("write vod chat resume state", "err", err)
		return
	}
	if err := os.Rename(tmpPath, vcd.resumeStatePath()); err != nil {
		vcd.logger.Error("rename vod chat resume state", "err", err)
	}
}

// removeResumeState deletes the sidecar resume state file after successful completion.
func (vcd *VodChatDownloader) removeResumeState() {
	if vcd.outputPath == "" {
		return
	}
	path := vcd.resumeStatePath()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		vcd.logger.Warn("remove vod chat resume state", "err", err)
	}
}

// MessageCount returns the total number of messages collected.
func (vcd *VodChatDownloader) MessageCount() int {
	return vcd.totalCount
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
