package worker

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestTwitchOfflineErrorNamesTheMuxForAnInterruptedCapture pins which error a
// non-manual Twitch job takes when its channel is offline. A Downloading row
// re-enqueued at boot with footage in staging was interrupted mid-broadcast;
// "twitch channel is offline" hid that, and the auto-recovery never takes such
// a row, so its message must name the Mux action. Everything else keeps the
// exact string the recovery keys on.
//
// Mutants: dropping the status term — a Live row with stale staging is told
// to mux; dropping the HasSegmentFiles term — a Downloading row with nothing
// staged is.
func TestTwitchOfflineErrorNamesTheMuxForAnInterruptedCapture(t *testing.T) {
	base := t.TempDir()
	stageMediaFor(t, base, "staged")

	for _, tc := range []struct {
		name string
		job  *database.Job
		want string
	}{
		{"interrupted capture", &database.Job{ID: "staged", Status: database.StatusDownloading}, twitchEndedOfflineErrMsg},
		{"downloading, nothing staged", &database.Job{ID: "empty", Status: database.StatusDownloading}, TwitchOfflineErrMsg},
		{"not yet downloading", &database.Job{ID: "staged", Status: database.StatusLive}, TwitchOfflineErrMsg},
	} {
		if got := twitchOfflineError(tc.job, base); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
