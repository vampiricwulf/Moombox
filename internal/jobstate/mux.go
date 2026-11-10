package jobstate

import (
	"strconv"
	"strings"
)

// The mux readout (owner ruling R-D1 of 2026-11-10): while a job's archive is being
// written, its progress line is replaced by how far FFmpeg's stream copy has
// got — MuxProgressLine while the output grows, MuxFinalizingLine once FFmpeg
// has stopped reporting (the faststart pass, then the checks on the output).
// The worker writes the lines (muxReadout, internal/worker); the dashboard
// shows a Muxing row's progress line as it is, and the TUI's task list reads
// the percent back with MuxReadout for its row tag.

const muxProgressPrefix = "Muxing "

// MuxFinalizingLine is the readout's line for the end of a mux.
const MuxFinalizingLine = "Muxing: finalizing"

// MuxProgressLine is the readout's line for a copy percent done, "Muxing 42%".
func MuxProgressLine(percent int) string {
	return muxProgressPrefix + strconv.Itoa(percent) + "%"
}

// MuxReadout reads a progress line the mux readout wrote: the copy's percent,
// or finalizing. ok is false for any other line — a download's counters, a
// wait line, an empty one.
func MuxReadout(progress string) (percent int, finalizing, ok bool) {
	if progress == MuxFinalizingLine {
		return 0, true, true
	}
	rest, found := strings.CutPrefix(progress, muxProgressPrefix)
	if !found {
		return 0, false, false
	}
	digits, found := strings.CutSuffix(rest, "%")
	if !found || digits == "" || len(digits) > 3 {
		return 0, false, false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 0, false, false
		}
	}
	n, _ := strconv.Atoi(digits)
	return n, false, true
}
