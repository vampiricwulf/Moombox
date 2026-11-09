package jobstate

import "strings"

// WaitMarker is what marks a progress line as a wait rather than a delivery
// counter: every wait line is "<label>... (<tail>)", the tail the time waited
// or the time left. The TUI's task list renders a row whose line carries it
// as a dim "⋯ wait" tag instead of a frozen percent, and the dashboard shows
// an Upcoming row's line in place of its "Last check" while one is up
// (owner ruling R-D1). utils.js WAIT_MARKER is this constant, compared by
// cmd/moombox's job-state parity test.
const WaitMarker = "... ("

// WaitLine builds a wait's progress line, "<label>... (<tail>)": the one
// constructor every writer of a wait line calls (internal/worker's waitLine),
// so no line can leave out the marker IsWaitLine reads. A label that already
// ends in "..." is not given a second one.
func WaitLine(label, tail string) string {
	return strings.TrimSuffix(label, "...") + WaitMarker + tail + ")"
}

// IsWaitLine reports whether a progress line is a wait line WaitLine built:
// it carries WaitMarker and closes its tail. No delivery counter has that
// shape ("V: 12/40 A: 12/40 C: 3", "(V: 1234/1300 …)", "V:95.3% C: 2").
func IsWaitLine(s string) bool {
	return strings.Contains(s, WaitMarker) && strings.HasSuffix(s, ")")
}
