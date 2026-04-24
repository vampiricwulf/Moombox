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

// Save marshals state to JSON and writes it atomically to s.Path via a .tmp
// intermediate + os.Rename. Returns nil when Path is empty (no-op).
func (s ResumeStore[T]) Save(state T) error {
	if s.Path == "" {
		return nil
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, s.Path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
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
