package stats

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestBuildMatchesTheRouteDerivations pins the one derivation both UIs read:
// the sums and groupings the /api/stats handler used to compute inline.
func TestBuildMatchesTheRouteDerivations(t *testing.T) {
	js := &database.JobStats{
		FinishedCount: 3, ActiveCount: 1, MuxingCount: 1, ErrorCount: 2, CancelledCount: 1, QueuedCount: 4,
		YouTubeCount: 5, TwitchCount: 3,
		FinishedSize: 100, ErrorSize: 20, CancelledSize: 5, YouTubeSize: 90, TwitchSize: 35,
		TotalDuration: 3600, TotalChatMessages: 42,
	}
	d := &Disk{Free: 10, Total: 100, UsedPct: 90, WarnLevel: "warn"}
	s := Build(js, d)
	if s.TotalSize != 125 || s.JobCount != 8 {
		t.Errorf("TotalSize/JobCount = %d/%d, want 125/8 (queued excluded)", s.TotalSize, s.JobCount)
	}
	if s.SizeByPlatform["youtube"] != 90 || s.SizeByPlatform["twitch"] != 35 {
		t.Errorf("SizeByPlatform = %v", s.SizeByPlatform)
	}
	if s.SizeByStatus["finished"] != 100 || s.SizeByStatus["error"] != 20 || s.SizeByStatus["cancelled"] != 5 {
		t.Errorf("SizeByStatus = %v", s.SizeByStatus)
	}
	if s.TotalFinished != 3 || s.TotalDuration != 3600 || s.TotalChatMessages != 42 || s.ActiveDownloads != 1 || s.ActiveMuxing != 1 {
		t.Errorf("activity = %+v", s)
	}
	if s.CountByPlatform["youtube"] != 5 || s.CountByPlatform["twitch"] != 3 {
		t.Errorf("CountByPlatform = %v", s.CountByPlatform)
	}
	if s.Disk != *d {
		t.Errorf("Disk = %+v", s.Disk)
	}
	z := Build(nil, nil)
	if z.TotalSize != 0 || z.JobCount != 0 || z.SizeByPlatform["youtube"] != 0 || z.SizeByStatus["finished"] != 0 || z.CountByPlatform["twitch"] != 0 || z.Disk != (Disk{}) {
		t.Errorf("zero snapshot = %+v", z)
	}
	if z.SizeByPlatform == nil || z.SizeByStatus == nil || z.CountByPlatform == nil {
		t.Error("maps must be present (with zero entries) so callers never nil-check")
	}
}
