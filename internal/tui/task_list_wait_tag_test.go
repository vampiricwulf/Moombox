package tui

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestNetworkWaitRendersAsWait: a Downloading row waiting out a connectivity
// outage in processJob — a VOD or a live capture re-enqueued at an offline
// boot — carries the worker's network-wait line, and its progress slot must
// read as a wait ("⋯ wait"), as the engine's own wait for the same outage
// does, not as the percent it froze at. The line used to carry no
// "... (<elapsed>)" tail, so the row showed "45% " for the length of the
// outage, the hung look isWaitProgress exists to prevent. The line is the
// worker's (worker.networkWaitLine), quoted here as it renders. So is the
// line a YouTube download shows while it waits out its player response's
// pre-roll ad before the first request (worker.playbackWaitLine), whose tail
// counts down — "(15s left)" — rather than up.
//
// Mutant killed: isWaitProgress answering false (`return false`) → every
// wait renders as the frozen percent. (The worker-side mutants — a line
// without its tail — are TestNetworkWaitLineCarriesTheWaitMarker's and
// TestPlaybackWaitLineCarriesTheWaitMarker's.)
func TestNetworkWaitRendersAsWait(t *testing.T) {
	m := NewTaskListModel()
	for _, tc := range []struct {
		name, progress string
		wantText       string
		wantWait       bool
	}{
		{"the engine's outage wait", "Connection lost - reconnecting... (5s)", "⋯ wait ", true},
		{"processJob's network wait", "Waiting for network connectivity... (1m 20s)", "⋯ wait ", true},
		{"the pre-roll ad wait", "Waiting out YouTube's pre-roll ad... (15s left)", "⋯ wait ", true},
		{"a segment counter", "(V: 1234/1300 A: 1234/1300)", "45% ", false},
	} {
		job := &database.Job{ID: tc.name, Status: database.StatusDownloading, Percent: 45, Progress: tc.progress}
		if text, wait := m.progressCellText(job); text != tc.wantText || wait != tc.wantWait {
			t.Errorf("%s: progress slot = %q (wait %v), want %q (wait %v)", tc.name, text, wait, tc.wantText, tc.wantWait)
		}
	}
}
