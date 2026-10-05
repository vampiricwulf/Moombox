package utils

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
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

// ErrChatFileDamaged is returned by AppendChatMessages when the file's end is
// not the end of its messages array: no ']' in the tail at all (a crash left
// the tail zero-filled), or a last ']' followed by anything but the object's
// closing '}' (the file was cut mid-record, so that ']' belongs to a
// message's own array). Nothing is written. Splicing there reported success
// over a file that no longer parsed, and every later append did the same.
var ErrChatFileDamaged = errors.New("chat file does not end with its messages array")

// WriteChatFileAtomic writes data as JSON to path through WriteFileAtomic: a
// uniquely named temp file in the same directory, fsync, chmod 0644 and
// ReplaceFile. Calls PadMessageCountJSON on the marshaled bytes so
// subsequent UpdateChatFileHeaderFields keeps the header byte-size stable.
//
// What the shared writer changed: the temp file's NAME. This used to open a
// fixed path + ".tmp", so two writers aiming at one chat file could interleave
// into a single temp and rename a torn result into place; os.CreateTemp gives
// each writer its own. Everything else is carried over unchanged and is now
// the shared writer's to guarantee — the fsync BEFORE the rename, the Windows
// sharing-violation retry inside ReplaceFile, and the removal of the temp on
// every failure path (one deferred cleanup instead of four hand-written ones).
// The fsync order and both cleanup paths are pinned by writefile_test.go's
// TestWriteFileAtomicSyncsBeforeReplacingTarget,
// TestWriteFileAtomicSyncFailureLeavesNoTempAndTargetUntouched and
// TestWriteFileAtomicRenameFailureLeavesNoTempAndTargetIntact; the retry by
// replacefile_test.go's TestReplaceFileRetriesATransientRefusalThenSucceeds.
//
// The encoded bytes are byte-identical to what the old writer produced —
// MarshalIndent with a two-space indent, the padded count field, no trailing
// newline — because AppendChatMessages parses this layout by byte offset;
// TestWriteChatFileAtomicEncodingIsUnchanged pins it. The one deliberate
// difference is the POSIX mode: exactly 0644 now (WriteFileAtomic chmods)
// rather than 0644 masked by the process umask.
//
// AppendChatMessages and UpdateChatFileHeaderFields deliberately do NOT use
// this path: they rewrite a live file in place and own their own durability.
func WriteChatFileAtomic[T any](path string, data T) error {
	jsonBytes, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	jsonBytes = PadMessageCountJSON(jsonBytes)
	return WriteFileAtomic(path, jsonBytes, 0o644)
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
// is non-nil); a batch in which none marshal leaves the file untouched. A
// truncate-then-write-failure returns ErrChatFilePartialWrite
// so the chat-side caller can avoid the history-dropping fallback path; see
// the sentinel's doc.
//
// count is the new total message count; the header's messageCount/downloadedAt
// are updated in-place within this same open handle (folded in so a flush is
// one open+fsync instead of an append followed by a separate header open+write).
// A header-update failure is non-fatal — the appended messages are already
// durable — so it is only logged via logger, never returned; callers that want
// a hard guarantee on the final count use UpdateChatFileHeaderFields directly.
func AppendChatMessages[T any](path string, msgs []T, count int, logger ChatFileLogger) error {
	if len(msgs) == 0 {
		return nil
	}

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	if info.Size() < 10 {
		return fmt.Errorf("%w: file too small (%d bytes)", ErrChatFileDamaged, info.Size())
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
	for i, b := range slices.Backward(tailBuf) {
		if b == ']' {
			bracketOffset = i
			break
		}
	}
	if bracketOffset == -1 {
		return fmt.Errorf("%w: no closing bracket in the last %d bytes", ErrChatFileDamaged, tailSize)
	}
	if !closesChatDocument(tailBuf[bracketOffset+1:]) {
		return fmt.Errorf("%w: the last ']' is not followed by the closing '}'", ErrChatFileDamaged)
	}

	bracketBytePos := fileSize - tailSize + int64(bracketOffset)

	// Does the array already hold a message? Look back past the whitespace
	// before ']' for a '}'. Any amount of it: the window used to be 5 bytes,
	// and a layout with more ("}\n  \n  ]") read as empty, so the next append
	// left out its comma and the file stopped being JSON.
	hasExisting := false
	if bracketBytePos > 0 {
		checkSize := min(int64(256), bracketBytePos)
		checkBuf := make([]byte, checkSize)
		if _, err := f.ReadAt(checkBuf, bracketBytePos-checkSize); err != nil {
			return fmt.Errorf("check existing: %w", err)
		}
		for _, b := range slices.Backward(checkBuf) {
			if b == '}' {
				hasExisting = true
				break
			} else if b != ' ' && b != '\n' && b != '\r' && b != '\t' {
				break
			}
		}
	}

	// Marshal first, then join only what marshalled. The separator used to
	// follow every index but the batch's last, so a skipped final message
	// left "},\n\n  ]" — a trailing comma that made the whole file invalid
	// JSON, durably, since the next append's bracket scan still succeeded.
	encoded := make([][]byte, 0, len(msgs))
	for i, msg := range msgs {
		msgBytes, err := json.Marshal(msg)
		if err != nil {
			if logger != nil {
				logger.Warn("marshal chat message failed", "err", err, "index", i)
			}
			continue
		}
		encoded = append(encoded, msgBytes)
	}
	if len(encoded) == 0 {
		return nil // nothing to add; leave the file and its header as they are
	}

	var sb strings.Builder
	for i, msgBytes := range encoded {
		if i > 0 || hasExisting {
			sb.WriteString(",\n")
		}
		sb.WriteString("    ")
		sb.Write(msgBytes)
	}
	sb.WriteString("\n  ]\n}")
	appendStr := sb.String()

	// Write the new payload BEFORE truncating, not after. The old order
	// (Truncate then WriteAt) left the file with no closing "]\n}" — invalid
	// JSON — if the WriteAt failed or a crash landed between the two ops; the
	// next append then found a stray ']' inside the last message and spliced
	// mid-record. The new payload is self-closing ("...]\n}") and always
	// longer than the "]\n}" it overwrites, so once WriteAt succeeds the file
	// is already a complete valid document; the Truncate only trims a
	// theoretical shorter-old-tail remainder.
	if _, err := f.WriteAt([]byte(appendStr), bracketBytePos); err != nil {
		// Partial/failed write: restore a valid closing bracket so the file
		// stays parseable (dropping only this batch), then signal the sentinel
		// so the caller advances instead of doing a history-dropping rewrite.
		// The bytes before bracketBytePos are the old tail's "\n  ", so only
		// "]\n}" goes back: the file is then byte-identical to before.
		if _, rerr := f.WriteAt([]byte("]\n}"), bracketBytePos); rerr == nil {
			f.Truncate(bracketBytePos + int64(len("]\n}")))
			f.Sync()
		}
		return fmt.Errorf("%w: %v", ErrChatFilePartialWrite, err)
	}
	newSize := bracketBytePos + int64(len(appendStr))
	if err := f.Truncate(newSize); err != nil {
		return fmt.Errorf("truncate: %w", err)
	}
	// Fold the header messageCount/downloadedAt refresh into this same open
	// handle rather than a second open+read+write elsewhere. Non-fatal: the
	// appended messages are already written, so a header-count failure is
	// cosmetic and self-heals on the next flush (and the final standalone
	// UpdateChatFileHeaderFields, where callers keep the hard-error path).
	if hdrErr := writeHeaderFieldsToOpenFile(f, newSize, count); hdrErr != nil && logger != nil {
		logger.Warn("chat header update (folded into append) failed", "err", hdrErr)
	}
	// fsync so the appended messages AND the refreshed header are durable —
	// without it a crash can leave the metadata (size) updated but the data
	// pages unwritten, i.e. a zero/garbage tail that the next append's bracket
	// scan misreads.
	if err := f.Sync(); err != nil {
		return fmt.Errorf("fsync: %w", err)
	}
	return nil
}

// closesChatDocument reports whether rest — the bytes after the messages
// array's ']' — is the object's closing '}' and nothing else but whitespace.
func closesChatDocument(rest []byte) bool {
	closed := false
	for _, b := range rest {
		switch b {
		case ' ', '\n', '\r', '\t':
		case '}':
			if closed {
				return false
			}
			closed = true
		default:
			return false
		}
	}
	return closed
}

// ChatFileEndIntact reports whether the chat file at path still ends the way
// AppendChatMessages needs: its messages array's ']' followed by the closing
// '}'. It reads only the tail. A missing file is an error the caller can test
// with os.IsNotExist.
func ChatFileEndIntact(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	if info.Size() < 10 {
		return false, nil
	}
	tailSize := min(int64(256), info.Size())
	tail := make([]byte, tailSize)
	if _, err := f.ReadAt(tail, info.Size()-tailSize); err != nil {
		return false, err
	}
	for i, b := range slices.Backward(tail) {
		if b == ']' {
			return closesChatDocument(tail[i+1:]), nil
		}
	}
	return false, nil
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

	return writeHeaderFieldsToOpenFile(f, info.Size(), count)
}

// ReadChatFileMessageCount returns the messageCount in a chat file's JSON
// header, reading only the first 1 KB — the header AppendChatMessages refreshes
// (and fsyncs) on every append, so it is never behind the file it heads.
// ok is false when the file cannot be read or carries no count.
func ReadChatFileMessageCount(path string) (count int, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	buf := make([]byte, 1024)
	n, _ := io.ReadFull(f, buf)
	return parseMessageCount(string(buf[:n]))
}

// writeHeaderFieldsToOpenFile rewrites messageCount + downloadedAt in the JSON
// header of an already-open O_RDWR file whose current size is `size`. Shared by
// UpdateChatFileHeaderFields (standalone open) and AppendChatMessages (folded
// into the append's open handle). Does NOT fsync — the caller owns durability.
//
// For files this package writes, messageCount is fixed-width padded and
// downloadedAt is a fixed-length RFC3339 stamp, so the replacement is always
// the equal-length in-place WriteAt at offset 0. The grow branch only fires on
// a legacy unpadded header, once, before it becomes padded.
func writeHeaderFieldsToOpenFile(f *os.File, size int64, count int) error {
	if size < 50 {
		return nil
	}
	headerSize := min(int64(1024), size)
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
		restSize := size - int64(n)
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
