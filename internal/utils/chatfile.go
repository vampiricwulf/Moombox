package utils

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// ChatFileLogger is the minimal logger interface the chat-file helpers use to
// surface non-fatal diagnostics (e.g. per-message marshal failures). nil is OK.
type ChatFileLogger interface {
	Warn(msg string, args ...any)
}

// ErrChatFilePartialWrite is returned by AppendChatMessages when the file was
// successfully truncated at the closing-bracket position but the subsequent
// WriteAt failed. The file is now in a broken state on disk, but the caller
// should advance its in-memory state rather than falling back to a full
// rewrite — a full rewrite would read the partial file (which no longer
// parses), recover no prior messages, and overwrite history with just the
// current batch.
var ErrChatFilePartialWrite = errors.New("chat file truncated but subsequent write failed")

// WriteChatFileAtomic writes data as JSON to path atomically via a .tmp
// intermediate plus os.Rename. Calls PadMessageCountJSON on the marshaled
// bytes so subsequent UpdateChatFileHeaderFields keeps the header byte-size
// stable. Includes f.Sync() before rename for crash-safety.
func WriteChatFileAtomic[T any](path string, data T) error {
	jsonBytes, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	jsonBytes = PadMessageCountJSON(jsonBytes)

	tmpFile := path + ".tmp"
	f, err := os.OpenFile(tmpFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("open tmp: %w", err)
	}
	if _, err := f.Write(jsonBytes); err != nil {
		f.Close()
		os.Remove(tmpFile)
		return fmt.Errorf("write: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmpFile)
		return fmt.Errorf("fsync: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpFile)
		return fmt.Errorf("close: %w", err)
	}
	if err := os.Rename(tmpFile, path); err != nil {
		os.Remove(tmpFile)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// AppendChatMessages appends msgs to an existing chat JSON file by locating
// the closing ']' of the messages array, truncating at that position, and
// writing the new entries plus closing structure. Memory cost is O(new msgs)
// rather than O(file size).
//
// Tail scan is 256 bytes to tolerate any realistic trailing-whitespace layout
// left by PadMessageCountJSON.
//
// Per-message json.Marshal failures are skipped after a logger.Warn (if logger
// is non-nil). A truncate-then-write-failure returns ErrChatFilePartialWrite
// so the chat-side caller can avoid the history-dropping fallback path; see
// the sentinel's doc.
func AppendChatMessages[T any](path string, msgs []T, logger ChatFileLogger) error {
	if len(msgs) == 0 {
		return nil
	}

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	if info.Size() < 10 {
		return fmt.Errorf("file too small (%d bytes)", info.Size())
	}

	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	fileSize := info.Size()

	tailSize := min(int64(256), fileSize)
	tailBuf := make([]byte, tailSize)
	if _, err := f.ReadAt(tailBuf, fileSize-tailSize); err != nil {
		return fmt.Errorf("read tail: %w", err)
	}

	bracketOffset := -1
	for i := len(tailBuf) - 1; i >= 0; i-- {
		if tailBuf[i] == ']' {
			bracketOffset = i
			break
		}
	}
	if bracketOffset == -1 {
		return fmt.Errorf("no closing bracket found")
	}

	bracketBytePos := fileSize - tailSize + int64(bracketOffset)

	hasExisting := false
	if bracketBytePos > 5 {
		checkSize := min(int64(5), bracketBytePos)
		checkBuf := make([]byte, checkSize)
		if _, err := f.ReadAt(checkBuf, bracketBytePos-checkSize); err != nil {
			return fmt.Errorf("check existing: %w", err)
		}
		for i := len(checkBuf) - 1; i >= 0; i-- {
			b := checkBuf[i]
			if b == '}' {
				hasExisting = true
				break
			} else if b != ' ' && b != '\n' && b != '\r' && b != '\t' {
				break
			}
		}
	}

	var sb strings.Builder
	if hasExisting {
		sb.WriteString(",\n")
	}
	for i, msg := range msgs {
		msgBytes, err := json.Marshal(msg)
		if err != nil {
			if logger != nil {
				logger.Warn("marshal chat message failed", "err", err, "index", i)
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

	if err := f.Truncate(bracketBytePos); err != nil {
		return fmt.Errorf("truncate: %w", err)
	}
	if _, err := f.WriteAt([]byte(appendStr), bracketBytePos); err != nil {
		return fmt.Errorf("%w: %v", ErrChatFilePartialWrite, err)
	}
	return nil
}

// UpdateChatFileHeaderFields updates messageCount and downloadedAt in the JSON
// header without rewriting the entire file. Reads only the first 1 KB.
//
// Three cases:
//   - len(new) == len(old): single WriteAt at offset 0.
//   - len(new) > len(old) (legacy unpadded header growing): reads the remainder
//     first, writes the new header, writes the remainder at the new offset,
//     then Truncates to the new total size.
//   - len(new) < len(old): no-op (old bytes remain; safe but technically stale).
//
// Returns a non-nil error only for IO failures. A missing file is not an error
// (returns nil, no-op); this matches the existing no-op behaviour when the
// chat file hasn't been flushed yet.
func UpdateChatFileHeaderFields(path string, count int) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat: %w", err)
	}
	if info.Size() < 50 {
		return nil
	}

	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	headerSize := min(int64(1024), info.Size())
	headerBuf := make([]byte, headerSize)
	n, err := f.ReadAt(headerBuf, 0)
	if err != nil && n == 0 {
		return fmt.Errorf("read: %w", err)
	}
	header := string(headerBuf[:n])

	header = ReplaceMessageCount(header, count)
	ReplaceQuotedField(&header, `"downloadedAt":`, time.Now().UTC().Format(time.RFC3339))

	updatedBytes := []byte(header)
	switch {
	case len(updatedBytes) == n:
		if _, err := f.WriteAt(updatedBytes, 0); err != nil {
			return fmt.Errorf("write: %w", err)
		}
	case len(updatedBytes) > n:
		restSize := info.Size() - int64(n)
		var restBuf []byte
		if restSize > 0 {
			restBuf = make([]byte, restSize)
			nRest, _ := f.ReadAt(restBuf, int64(n))
			restBuf = restBuf[:nRest]
		}
		if _, err := f.WriteAt(updatedBytes, 0); err != nil {
			return fmt.Errorf("write expanded: %w", err)
		}
		if len(restBuf) > 0 {
			if _, err := f.WriteAt(restBuf, int64(len(updatedBytes))); err != nil {
				return fmt.Errorf("write rest: %w", err)
			}
		}
		if err := f.Truncate(int64(len(updatedBytes)) + restSize); err != nil {
			return fmt.Errorf("truncate: %w", err)
		}
	}
	return nil
}
