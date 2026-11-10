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
// text the offline flap has always carried. Each names its park reason beside
// the text (owner ruling R-I1), which is what the recovery and the Mux's
// gap-fill tail key on.
//
// Mutants: dropping the status term — a Live row with stale staging is told
// to mux; dropping the HasSegmentFiles term — a Downloading row with nothing
// staged is.
func TestTwitchOfflineErrorNamesTheMuxForAnInterruptedCapture(t *testing.T) {
	base := t.TempDir()
	stageMediaFor(t, base, "staged")

	for _, tc := range []struct {
		name   string
		job    *database.Job
		want   string
		reason database.ParkReason
	}{
		{"interrupted capture", &database.Job{ID: "staged", Status: database.StatusDownloading}, twitchEndedOfflineErrMsg, database.ParkReasonTwitchEndedOffline},
		{"downloading, nothing staged", &database.Job{ID: "empty", Status: database.StatusDownloading}, TwitchOfflineErrMsg, database.ParkReasonTwitchOffline},
		{"not yet downloading", &database.Job{ID: "staged", Status: database.StatusLive}, TwitchOfflineErrMsg, database.ParkReasonTwitchOffline},
	} {
		if got, reason := twitchOfflineError(tc.job, base); got != tc.want || reason != tc.reason {
			t.Errorf("%s: got %q (%q), want %q (%q)", tc.name, got, reason, tc.want, tc.reason)
		}
	}
	// The texts are still the frozen ones an earlier release keys on.
	if TwitchOfflineErrMsg != database.LegacyTwitchOfflineError || twitchEndedOfflineErrMsg != database.LegacyTwitchEndedOfflineError {
		t.Error("an offline error text no longer equals its frozen legacy text; see legacyEndedOfflineRow for when it may be reworded")
	}
}
