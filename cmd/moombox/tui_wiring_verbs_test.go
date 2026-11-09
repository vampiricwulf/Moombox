package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/worker"
)

// TestQuietIfAlreadyPending: the TUI's Resume and Reinitialize answer the
// worker's ErrAlreadyPending — the same verb already on its way — as success,
// as REST does (owner ruling R-A3), and show every other error in red.
func TestQuietIfAlreadyPending(t *testing.T) {
	if err := quietIfAlreadyPending(fmt.Errorf("resume %w", worker.ErrAlreadyPending)); err != nil {
		t.Errorf("the same verb already on its way = %v, want nil", err)
	}
	pending := fmt.Errorf("reinitialize %w", worker.ErrResetPending)
	if err := quietIfAlreadyPending(pending); !errors.Is(err, worker.ErrResetPending) {
		t.Errorf("a different verb pending = %v, want the refusal kept", err)
	}
	if err := quietIfAlreadyPending(nil); err != nil {
		t.Errorf("nil = %v", err)
	}
}
