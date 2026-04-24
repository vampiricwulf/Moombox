package twitch

import (
	"os"
	"path/filepath"
	"time"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

func (cd *ChatDownloader) flush() {
	cd.mu.Lock()
	snapshotLen := len(cd.messages)
	msgs := make([]TwitchChatMessage, snapshotLen)
	copy(msgs, cd.messages)
	count := cd.totalCount
	flushed := cd.flushedToDisk
	cd.mu.Unlock()

	if snapshotLen == 0 || cd.outputPath == "" {
		return
	}

	dir := filepath.Dir(cd.outputPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		cd.logger.Error("create chat output dir", "err", err)
		return
	}

	var writeErr error
	if !flushed {
		// First flush: write complete file
		writeErr = cd.writeFullChatFile(msgs, count)
	} else {
		// Subsequent flushes: append new messages to existing file
		writeErr = utils.AppendChatMessages(cd.outputPath, msgs, cd.logger)
		if writeErr == nil {
			if err := utils.UpdateChatFileHeaderFields(cd.outputPath, count); err != nil {
				cd.logger.Warn("update chat header", "err", err)
			}
		} else {
			// Fallback: read the existing file, merge with the current batch,
			// and rewrite. The earlier implementation passed only `msgs` to
			// writeFullChatFile here, which replaced the aggregate with just
			// the latest batch — silently dropping every message from prior
			// flushes whenever append hit a transient I/O glitch.
			cd.logger.Warn("append failed, merging existing file with current batch", "err", writeErr)
			existing, readErr := readChatFileMessages(cd.outputPath)
			if readErr != nil {
				// Can't recover without destroying data — leave the file
				// alone and keep the batch in the in-memory buffer so the
				// next flush retries. The early-return below skips the
				// `cd.messages = cd.messages[snapshotLen:]` truncation.
				cd.logger.Error("append failed and cannot read existing file for merge; preserving file, retrying next flush", "err", readErr)
				return
			}
			merged := append(existing, msgs...)
			writeErr = cd.writeFullChatFile(merged, count)
		}
	}

	if writeErr != nil {
		cd.logger.Error("write chat file", "err", writeErr)
		return
	}

	// Remove only the messages we successfully wrote, preserving any
	// that arrived concurrently during the write.
	cd.mu.Lock()
	cd.messages = cd.messages[snapshotLen:]
	cd.flushedToDisk = true
	cd.mu.Unlock()

	// Prune dedup set to prevent unbounded memory growth
	cd.pruneDedup()

	// Save resume state after each flush (matches TS periodicSave -> saveResumeState)
	cd.saveResumeState()
}

// writeFullChatFile writes all messages as a complete JSON file atomically.
func (cd *ChatDownloader) writeFullChatFile(msgs []TwitchChatMessage, count int) error {
	chatData := TwitchChatData{
		Platform:           "twitch",
		ChannelLogin:       cd.channelLogin,
		ChannelDisplayName: cd.channelDisplay,
		StreamID:           cd.streamID,
		StreamStartTime:    cd.streamStartTime,
		DownloadedAt:       time.Now().UTC().Format(time.RFC3339),
		MessageCount:       count,
		Messages:           msgs,
	}
	if cd.recordingStartMs > 0 {
		chatData.RecordingStartTime = time.UnixMilli(cd.recordingStartMs).UTC().Format(time.RFC3339)
	}
	return utils.WriteChatFileAtomic(cd.outputPath, &chatData)
}

// pruneDedup trims the seenIDs set to keep only the most recent entries.
func (cd *ChatDownloader) pruneDedup() {
	cd.mu.Lock()
	defer cd.mu.Unlock()
	if len(cd.seenOrder) <= chatDedupMax {
		return
	}
	keep := chatDedupMax
	removeIDs := cd.seenOrder[:len(cd.seenOrder)-keep]
	for _, id := range removeIDs {
		delete(cd.seenIDs, id)
	}
	cd.seenOrder = cd.seenOrder[len(cd.seenOrder)-keep:]
}
