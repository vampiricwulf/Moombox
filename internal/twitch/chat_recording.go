package twitch

import (
	"os"
	"path/filepath"
	"time"
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
		writeErr = appendChatMessages(cd.outputPath, msgs, cd.logger)
		if writeErr == nil {
			updateChatFileHeaderFields(cd.outputPath, count, cd.logger)
		} else {
			// Fallback to full rewrite on append error
			cd.logger.Debug("append failed, falling back to full write", "err", writeErr)
			writeErr = cd.writeFullChatFile(msgs, count)
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
	return writeChatFileAtomic(cd.outputPath, &chatData)
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
