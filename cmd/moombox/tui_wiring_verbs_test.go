package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/tui"
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

// TestMuxRefusalIsMarkedForTheTUI: A M shows the worker's refusals of a Mux
// as REST answers them — ErrNotAllowed and ErrStagingBusy (409),
// ErrJobNotFound (404) — and prefixes "Mux failed: " only to a fault, so the
// wiring marks exactly those three as a tui.RefusalError and leaves the
// worker's sentinel reachable.
//
// Mutant: any one of the three left unmarked.
func TestMuxRefusalIsMarkedForTheTUI(t *testing.T) {
	for _, sentinel := range []error{worker.ErrNotAllowed, worker.ErrStagingBusy, worker.ErrJobNotFound} {
		err := muxRefusal(fmt.Errorf("mux: %w", sentinel))
		var refusal tui.RefusalError
		if !errors.As(err, &refusal) {
			t.Errorf("muxRefusal(%v) = %T, want a tui.RefusalError", sentinel, err)
		}
		if !errors.Is(err, sentinel) {
			t.Errorf("muxRefusal(%v) lost the sentinel", sentinel)
		}
	}
	fault := errors.New("ffmpeg exited 1")
	var refusal tui.RefusalError
	if err := muxRefusal(fault); errors.As(err, &refusal) || err != fault {
		t.Errorf("muxRefusal(a fault) = %v (%T), want the fault unmarked", err, err)
	}
	if err := muxRefusal(nil); err != nil {
		t.Errorf("muxRefusal(nil) = %v", err)
	}
}
