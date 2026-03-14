package twitch

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// chatLogger is the minimal interface needed by chat file helpers.
type chatLogger interface {
	Warn(msg string, args ...any)
}

// writeChatFileAtomic writes a TwitchChatData struct as JSON to path atomically
// via a .tmp intermediate file. The messageCount field is padded for in-place updates.
func writeChatFileAtomic(path string, chatData *TwitchChatData) error {
	data, err := json.MarshalIndent(chatData, "", "  ")
	if err != nil {
		return err
	}
	data = utils.PadMessageCountJSON(data)

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// appendChatMessages appends messages to an existing chat JSON file by finding
// the closing ']' of the messages array and inserting new entries.
// This avoids re-serializing the entire file, keeping memory O(new messages).
func appendChatMessages(path string, msgs []TwitchChatMessage, logger chatLogger) error {
	if len(msgs) == 0 {
		return nil
	}

	info, err := os.Stat(path)
	if err != nil || info.Size() < 10 {
		return fmt.Errorf("file missing or too small")
	}

	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	fileSize := info.Size()

	// Read last 10 bytes to find ']'
	tailSize := min(int64(10), fileSize)
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
		checkSize := min(int64(5), bracketBytePos)
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
			if logger != nil {
				logger.Warn("marshal chat message failed", "err", err, "id", msg.ID)
			}
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

	return nil
}

// updateChatFileHeaderFields updates messageCount and downloadedAt in the JSON
// header without rewriting the entire file. Uses padded messageCount for stable
// header size across updates.
func updateChatFileHeaderFields(path string, count int, logger chatLogger) {
	info, err := os.Stat(path)
	if err != nil || info.Size() < 50 {
		return
	}

	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return
	}
	defer f.Close()

	headerSize := min(int64(1024), info.Size())

	headerBuf := make([]byte, headerSize)
	n, err := f.ReadAt(headerBuf, 0)
	if err != nil && n == 0 {
		return
	}
	header := string(headerBuf[:n])

	// Replace messageCount value (padded to fixed width)
	header = utils.ReplaceMessageCount(header, count)

	// Replace downloadedAt value
	utils.ReplaceQuotedField(&header, `"downloadedAt":`, time.Now().UTC().Format(time.RFC3339))

	// With padded messageCount, the header size should be constant.
	// Fallback handles legacy files written before padding was added.
	updatedBytes := []byte(header)
	if len(updatedBytes) < n {
		if logger != nil {
			logger.Warn("chat header shrank during update, file may have stale bytes",
				"expected", n, "actual", len(updatedBytes))
		}
	}
	if len(updatedBytes) == n {
		if _, err := f.WriteAt(updatedBytes, 0); err != nil {
			if logger != nil {
				logger.Warn("update chat header: write", "err", err)
			}
		}
	} else if len(updatedBytes) > n {
		// Header grew — must read rest BEFORE writing header, since the new
		// header is longer and would overwrite the first byte(s) of the rest data.
		restSize := info.Size() - int64(n)
		var restBuf []byte
		if restSize > 0 {
			restBuf = make([]byte, restSize)
			nRest, _ := f.ReadAt(restBuf, int64(n))
			restBuf = restBuf[:nRest]
		}
		if _, err := f.WriteAt(updatedBytes, 0); err != nil {
			if logger != nil {
				logger.Warn("update chat header: write expanded", "err", err)
			}
			return
		}
		if len(restBuf) > 0 {
			if _, err := f.WriteAt(restBuf, int64(len(updatedBytes))); err != nil {
				if logger != nil {
					logger.Warn("update chat header: write rest", "err", err)
				}
				return
			}
		}
		if err := f.Truncate(int64(len(updatedBytes)) + restSize); err != nil {
			if logger != nil {
				logger.Warn("update chat header: truncate", "err", err)
			}
		}
	}
}
