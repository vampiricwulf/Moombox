package main

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/tui"
)

// The TUI learns a pending release is withdrawn from an UpdateStatusMsg with
// an empty Version and the withdrawn tag — it drops its badge only when the
// tag names the release it shows. A missing tag would match nothing, and a
// Version would relight the badge instead.
//
// Mutant: announceUpdateCleared sending the message without the tag.
func TestAnnounceUpdateClearedTellsTheTUIWhichTag(t *testing.T) {
	ch := make(chan tui.UpdateStatusMsg, 1)
	announceUpdateCleared(nil, ch, "v9.9.9")
	select {
	case msg := <-ch:
		if msg.TagName != "v9.9.9" || msg.Version != "" {
			t.Errorf("TUI message = %+v, want a clear (no Version) naming v9.9.9", msg)
		}
	default:
		t.Fatal("no message reached the TUI")
	}
}
