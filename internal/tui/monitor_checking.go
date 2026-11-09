package tui

import "time"

// MonitorCheckingSentinelMs is the epoch-ms value a monitor reports for
// NextCheckAt while a check cycle is actually running (as opposed to 0 =
// "no channels" or a real future timestamp). The header renders it as "…".
const MonitorCheckingSentinelMs = -1

// monitorCheckingTime is the exact time.Time carrier for the "checking"
// state inside the TUI (which models check times as time.Time, not ms).
// time.UnixMilli(-1) is a real, non-zero instant 1ms before the epoch, so
// it never collides with a genuine future check time or the zero value.
var monitorCheckingTime = time.UnixMilli(MonitorCheckingSentinelMs)

// MonitorCheckingTime returns the sentinel time.Time meaning "a check is
// running right now"; the status header renders it as "…" with a live dot.
func MonitorCheckingTime() time.Time { return monitorCheckingTime }

// isMonitorChecking reports whether a check-time value is the checking
// sentinel.
func isMonitorChecking(t time.Time) bool { return t.Equal(monitorCheckingTime) }

// MonitorNextCheckTime decodes a monitor's NextCheckAt (epoch ms) into the
// TUI's time.Time model: MonitorCheckingSentinelMs becomes the checking
// sentinel, 0 ("no channels", nothing scheduled) or any other non-positive
// value the zero time — a blank timer, the Web's "--" — and anything else
// the instant it names. time.UnixMilli(0) would be a real instant in the
// past, which the header read as a green "now".
func MonitorNextCheckTime(ms int64) time.Time {
	switch {
	case ms == MonitorCheckingSentinelMs:
		return monitorCheckingTime
	case ms <= 0:
		return time.Time{}
	default:
		return time.UnixMilli(ms)
	}
}

// MonitorKind names the monitor a CheckTimersMsg is about.
type MonitorKind int

const (
	MonitorFeed MonitorKind = iota
	MonitorDecapi
	MonitorTwitch
)
