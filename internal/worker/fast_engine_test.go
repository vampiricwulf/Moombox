package worker

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/engine"
)

// engineFastScale is the factor fastEngine divides the engine's waits by:
// SetFastDelaysForTests moves every one of them by it, so a test's escalation
// counts and orderings stay production's.
const engineFastScale = 20

// fastEngine runs every engine downloader the orchestrator builds during the
// test at 1/engineFastScale of production timing: a playlist reload every
// 50 ms rather than every second, a connectivity poll every 250 ms rather than
// every 5 s, and the retry ladders shrunk alike. It is for tests whose fake
// servers answer at once, where those waits are all that is left of the run.
func fastEngine(t *testing.T) {
	t.Helper()
	prev := newSegmentDownloader
	newSegmentDownloader = func(opts engine.DownloaderOptions) *engine.SegmentDownloader {
		d := engine.NewSegmentDownloader(opts)
		d.SetFastDelaysForTests(engineFastScale)
		return d
	}
	t.Cleanup(func() { newSegmentDownloader = prev })
}
