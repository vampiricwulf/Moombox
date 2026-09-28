// Package stats derives the figures both dashboards show — the Web Stats tab
// and the TUI's E T overlay — from the job aggregate and the disk reading.
// It imports only the database package so the TUI can use it without the
// HTTP layer.
package stats

import (
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// Disk is the disk reading both UIs show — the same four facts
// routes.DiskStatus carries; kept separate so this package never imports
// the HTTP layer.
type Disk struct {
	Free      uint64
	Total     uint64
	UsedPct   float64
	WarnLevel string // "ok", "warn", "critical"; "" when no reading exists yet
}

// Snapshot is every number the Web Stats tab and the TUI E T overlay show,
// derived once here from the job aggregate and the disk reading.
type Snapshot struct {
	Disk Disk

	TotalSize      int64            // finished + error + cancelled bytes
	JobCount       int              // finished + error + cancelled + active + muxing (queued excluded, as the route always did)
	SizeByPlatform map[string]int64 // "youtube", "twitch"
	SizeByStatus   map[string]int64 // "finished", "error", "cancelled"

	TotalFinished     int
	TotalDuration     int64 // seconds
	TotalChatMessages int64
	ActiveDownloads   int
	ActiveMuxing      int
	CountByPlatform   map[string]int // "youtube", "twitch"

	Uptime time.Duration // filled by the TUI callback only; the Web derives uptime client-side from /api/status
}

// Build derives a Snapshot. nil js (query failed) → zeros with the maps
// present; nil d → the zero Disk.
func Build(js *database.JobStats, d *Disk) Snapshot {
	s := Snapshot{
		SizeByPlatform:  map[string]int64{"youtube": 0, "twitch": 0},
		SizeByStatus:    map[string]int64{"finished": 0, "error": 0, "cancelled": 0},
		CountByPlatform: map[string]int{"youtube": 0, "twitch": 0},
	}
	if d != nil {
		s.Disk = *d
	}
	if js == nil {
		return s
	}
	s.TotalSize = js.FinishedSize + js.ErrorSize + js.CancelledSize
	s.SizeByPlatform["youtube"] = js.YouTubeSize
	s.SizeByPlatform["twitch"] = js.TwitchSize
	s.SizeByStatus["finished"] = js.FinishedSize
	s.SizeByStatus["error"] = js.ErrorSize
	s.SizeByStatus["cancelled"] = js.CancelledSize
	s.JobCount = js.FinishedCount + js.ErrorCount + js.CancelledCount + js.ActiveCount + js.MuxingCount
	s.TotalFinished = js.FinishedCount
	s.TotalDuration = js.TotalDuration
	s.TotalChatMessages = js.TotalChatMessages
	s.ActiveDownloads = js.ActiveCount
	s.ActiveMuxing = js.MuxingCount
	s.CountByPlatform["youtube"] = js.YouTubeCount
	s.CountByPlatform["twitch"] = js.TwitchCount
	return s
}
