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
