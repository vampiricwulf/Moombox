package twitch

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
		writeErr = cd.appendToFile(msgs)
		if writeErr != nil {
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

	data, err := json.MarshalIndent(chatData, "", "  ")
	if err != nil {
		return err
	}
	data = utils.PadMessageCountJSON(data)

	tmpPath := cd.outputPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, cd.outputPath)
}

// appendToFile appends new messages to the existing JSON file by finding the
// closing ']' of the messages array and appending new entries. This avoids
// re-serializing the entire file on each flush, keeping memory O(new messages).
func (cd *ChatDownloader) appendToFile(msgs []TwitchChatMessage) error {
	if len(msgs) == 0 {
		return nil
	}

	info, err := os.Stat(cd.outputPath)
	if err != nil || info.Size() < 10 {
		return fmt.Errorf("file missing or too small")
	}

	f, err := os.OpenFile(cd.outputPath, os.O_RDWR, 0o644)
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

	// Check if there are existing messages (look for '}' before ']')
	hasExisting := false
	if bracketBytePos > 5 {
		checkSize := int64(5)
		if checkSize > bracketBytePos {
			checkSize = bracketBytePos
		}
		checkBuf := make([]byte, checkSize)
		if _, err := f.ReadAt(checkBuf, bracketBytePos-checkSize); err != nil {
			return fmt.Errorf("check existing messages: %w", err)
		}
		for i := len(checkBuf) - 1; i >= 0; i-- {
			if checkBuf[i] == '}' {
				hasExisting = true
				break
			} else if checkBuf[i] != ' ' && checkBuf[i] != '\n' && checkBuf[i] != '\r' && checkBuf[i] != '\t' {
				break
			}
		}
	}

	// Build append content
	var sb strings.Builder
	if hasExisting {
		sb.WriteString(",\n")
	}
	for i, msg := range msgs {
		msgBytes, err := json.Marshal(msg)
		if err != nil {
			cd.logger.Warn("marshal chat message failed", "err", err, "id", msg.ID)
			continue
		}
		sb.WriteString("    ")
		sb.Write(msgBytes)
		if i < len(msgs)-1 {
			sb.WriteString(",\n")
		}
	}
	sb.WriteString("\n  ]\n}")
	appendStr := sb.String()

	// Truncate at ']' position, then write new content
	if err := f.Truncate(bracketBytePos); err != nil {
		return err
	}
	if _, err := f.WriteAt([]byte(appendStr), bracketBytePos); err != nil {
		return fmt.Errorf("write appended messages: %w", err)
	}

	// Update messageCount in file header
	cd.updateChatFileHeader()

	return nil
}

// updateChatFileHeader updates messageCount and downloadedAt in the JSON header without rewriting.
func (cd *ChatDownloader) updateChatFileHeader() {
	info, err := os.Stat(cd.outputPath)
	if err != nil || info.Size() < 50 {
		return
	}

	f, err := os.OpenFile(cd.outputPath, os.O_RDWR, 0o644)
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

	// Replace messageCount value (padded to fixed width)
	header = utils.ReplaceMessageCount(header, cd.totalCount)

	// Replace downloadedAt value (matches TS: header.replace(/"downloadedAt": "[^"]*"/, ...))
	utils.ReplaceQuotedField(&header, `"downloadedAt":`, time.Now().UTC().Format(time.RFC3339))

	// With padded messageCount, the header size should be constant.
	// Fallback handles legacy files written before padding was added.
	updatedBytes := []byte(header)
	if len(updatedBytes) == n {
		if _, err := f.WriteAt(updatedBytes, 0); err != nil {
			cd.logger.Warn("update chat header: write", "err", err)
		}
	} else if len(updatedBytes) > n {
		// Header grew -- must read rest BEFORE writing header, since the new
		// header is longer and would overwrite the first byte(s) of the rest data.
		restSize := info.Size() - int64(n)
		var restBuf []byte
		if restSize > 0 {
			restBuf = make([]byte, restSize)
			nRest, _ := f.ReadAt(restBuf, int64(n))
			restBuf = restBuf[:nRest]
		}
		if _, err := f.WriteAt(updatedBytes, 0); err != nil {
			cd.logger.Warn("update chat header: write expanded", "err", err)
			return
		}
		if len(restBuf) > 0 {
			if _, err := f.WriteAt(restBuf, int64(len(updatedBytes))); err != nil {
				cd.logger.Warn("update chat header: write rest", "err", err)
				return
			}
		}
		if err := f.Truncate(int64(len(updatedBytes)) + restSize); err != nil {
			cd.logger.Warn("update chat header: truncate", "err", err)
		}
	}
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
