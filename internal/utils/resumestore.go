package utils

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// ErrNoResume is returned by ResumeStore.Load when the sidecar file does not
// exist. Callers can distinguish "first run" from a real read failure via
// errors.Is(err, ErrNoResume).
var ErrNoResume = errors.New("no resume state")

// ResumeStore persists a resume-state struct of type T to a JSON sidecar file
// at Path. Save uses atomic .tmp + rename; Load returns ErrNoResume when the
// file is missing; Clear tolerates a missing file.
//
// The zero value with Path == "" is an explicit no-op: Save returns nil,
// Load returns ErrNoResume, Clear returns nil. This matches the "early chat"
// semantics in the chat downloader where the output path hasn't been
// negotiated yet.
type ResumeStore[T any] struct {
	Path string
}

// Save marshals state to JSON and writes it to s.Path through WriteFileAtomic:
// a uniquely named temp file in the same directory, fsync, chmod 0644 and
// ReplaceFile. Returns nil when Path is empty (no-op) — that guard runs
// FIRST, before the marshal and before the shared writer, because
// filepath.Dir("") is "." and an unguarded call would drop a stray temp in the
// process working directory.
//
// What the shared writer changed: the temp file's NAME. This used to open a
// fixed Path + ".tmp", so a second writer aiming at the same sidecar could
// interleave into it and rename a torn result into place; os.CreateTemp gives
// each writer its own. The fsync BEFORE the rename, the Windows
// sharing-violation retry inside ReplaceFile and the removal of the temp on
// every failure path are all carried over unchanged. The fsync order and both
// cleanup paths now come from the shared writer — pinned by writefile_test.go's
// TestWriteFileAtomicSyncsBeforeReplacingTarget,
// TestWriteFileAtomicSyncFailureLeavesNoTempAndTargetUntouched and
// TestWriteFileAtomicRenameFailureLeavesNoTempAndTargetIntact; the retry by
// replacefile_test.go's TestReplaceFileRetriesATransientRefusalThenSucceeds.
// The encoded bytes are unchanged (compact json.Marshal, no trailing newline), pinned by
// TestResumeStoreSaveEncodingIsUnchanged; the POSIX mode is now exactly 0644
// rather than 0644 masked by the umask.
//
// The fsync before rename matters: without it, an OS crash / power loss can
// journal the rename while the data pages never hit disk, leaving a
// zero-length/corrupt sidecar. Consumers treat any Load failure as "no resume"
// and start fresh. For the YouTube chat downloader a fresh start no longer
// means losing the archive: its adoption rule (adoptExistingChatFile,
// internal/chat/downloader.go) makes a LIVE/upcoming run adopt the chat.json
// already on disk and append to it, so a corrupt sidecar now costs only the
// saved continuation and dedup window. A REPLAY run is not adopted — it
// re-reads the archive from the top — so there a corrupt sidecar still means a
// full rewrite, which for a replay is the intended behaviour rather than a
// loss.
func (s ResumeStore[T]) Save(state T) error {
	if s.Path == "" {
		return nil
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return WriteFileAtomic(s.Path, data, 0o644)
}

// Load reads s.Path and unmarshals into T. Returns the zero value + ErrNoResume
// when the file does not exist, the zero value + a wrapped error on read /
// parse failure.
func (s ResumeStore[T]) Load() (T, error) {
	var zero T
	if s.Path == "" {
		return zero, ErrNoResume
	}
	raw, err := os.ReadFile(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return zero, ErrNoResume
		}
		return zero, fmt.Errorf("read: %w", err)
	}
	var state T
	if err := json.Unmarshal(raw, &state); err != nil {
		return zero, fmt.Errorf("unmarshal: %w", err)
	}
	return state, nil
}

// Clear removes the sidecar file. A missing file is not an error. Returns a
// wrapped error for any other removal failure (permission denied, etc.).
func (s ResumeStore[T]) Clear() error {
	if s.Path == "" {
		return nil
	}
	if err := os.Remove(s.Path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove: %w", err)
	}
	return nil
}
