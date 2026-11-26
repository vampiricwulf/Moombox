package disk

import (
	"errors"
	"fmt"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// ErrNoRoom is what every NoRoomError wraps.
var ErrNoRoom = errors.New("not enough room on the volume")

// NoRoomError is RoomFor's refusal: the write needs Need bytes free, the
// volume has Free, and writing them would leave it AfterPct used. Crosses is
// set when there was room but AfterPct reaches the disk_critical threshold
// (CriticalPercent); otherwise Free is short of Need. Each caller words its
// own message from these fields.
type NoRoomError struct {
	Need, Free      uint64
	AfterPct        float64
	CriticalPercent int
	Crosses         bool
}

func (e *NoRoomError) Error() string {
	if e.Crosses {
		return fmt.Sprintf("writing %d bytes would take the volume to %.1f%% used, past the disk_critical threshold (%d%%)", e.Need, e.AfterPct, e.CriticalPercent)
	}
	return fmt.Sprintf("needs %d bytes free, the volume has %d", e.Need, e.Free)
}

func (e *NoRoomError) Unwrap() error { return ErrNoRoom }

// RoomFor is the one check a write of need bytes onto a volume is made
// against, before anything is written: the chapter re-mux's copy
// (worker.remuxRoom, owner decision PR-I4) and the archive import's
// extraction (owner ruling R4-11). It refuses when the volume has less than
// need free, or when the write would take it to the disk_critical threshold
// (config.DiskConfig.AtCritical, the reading that closes backlog admission
// and alerts the operator). need includes the caller's headroom. A volume
// that could not be read (space nil) passes: the write's own failure is
// clean, and refusing on no reading would block every write on a volume the
// OS cannot measure.
func RoomFor(space *DiskSpace, need uint64, thresholds config.DiskConfig) *NoRoomError {
	if space == nil {
		return nil
	}
	after := space.UsedPct
	if space.Total > 0 {
		after += float64(need) / float64(space.Total) * 100
	}
	e := &NoRoomError{Need: need, Free: space.Free, AfterPct: after, CriticalPercent: thresholds.CriticalPercent}
	if space.Free < need {
		return e
	}
	if thresholds.AtCritical(after) {
		e.Crosses = true
		return e
	}
	return nil
}

// WriteHeadroom is the room every RoomFor caller asks for on top of what it
// writes: for the copy's own overheads (an index growing past the
// original's, a temporary file beside it) and for whatever else writes to
// the volume meanwhile, a live capture among them.
const WriteHeadroom = 256 << 20
