package engine

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// dashEdgeStatsMsg is the one Debug line runDashLoop logs when it exits.
const dashEdgeStatsMsg = "[Downloader] DASH loop exit: requests at the live edge"

// dashEdgeStats measures what the sequential DASH loop spends at the live
// edge. Once a capture has caught up, the loop asks for the next sequence as
// soon as it has written one, and that segment is usually not published yet.
// Whether googlevideo then answers at once with an error (a wasted request
// per segment, per track) or holds the request until the segment exists was
// never measured. These counters measure it and change nothing: no request,
// wait or decision reads them.
//
// It is a local of runDashLoop, like dashRetryState: one goroutine, never
// stored.
type dashEdgeStats struct {
	// segmentsWritten counts the segments the sequential path wrote.
	// Parallel catch-up's segments are not in it: catch-up runs far behind
	// the head, never at the edge.
	segmentsWritten int
	// failures counts the sequential fetches that failed at or past the
	// known head.
	failures int
	// byStatus tallies those failures by HTTP status. 0 is a failure with
	// no HTTP answer (a transport error or a timeout); a 2xx is an answer
	// with an empty body.
	byStatus map[int]int
	// How long those failed fetches took, request to answer. A latency near
	// zero is a fast refusal; one near the segment length is a held request.
	latencySum, latencyMin, latencyMax time.Duration
}

// noteFailure records one failed fetch at the live edge.
func (s *dashEdgeStats) noteFailure(status int, took time.Duration) {
	if s.byStatus == nil {
		s.byStatus = make(map[int]int)
	}
	s.byStatus[status]++
	if s.failures == 0 || took < s.latencyMin {
		s.latencyMin = took
	}
	if took > s.latencyMax {
		s.latencyMax = took
	}
	s.latencySum += took
	s.failures++
}

// avgLatency is the mean time a failed at-edge fetch took.
func (s *dashEdgeStats) avgLatency() time.Duration {
	if s.failures == 0 {
		return 0
	}
	return s.latencySum / time.Duration(s.failures)
}

// statuses renders the tally as "status:count" pairs in status order, e.g.
// "0:1 404:212".
func (s *dashEdgeStats) statuses() string {
	codes := make([]int, 0, len(s.byStatus))
	for code := range s.byStatus {
		codes = append(codes, code)
	}
	slices.Sort(codes)
	var b strings.Builder
	for i, code := range codes {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%d:%d", code, s.byStatus[code])
	}
	return b.String()
}

// logOnExit writes the one Debug line. A loop that neither wrote a segment
// nor failed at the edge (a run that only caught up, or stopped at once) has
// nothing to report and logs nothing.
func (s *dashEdgeStats) logOnExit(logger DownloaderLogger) {
	if s.segmentsWritten == 0 && s.failures == 0 {
		return
	}
	logger.Debug(dashEdgeStatsMsg,
		"segmentsWritten", s.segmentsWritten,
		"atEdgeFailures", s.failures,
		"statuses", s.statuses(),
		"latencyMin", s.latencyMin,
		"latencyAvg", s.avgLatency(),
		"latencyMax", s.latencyMax)
}
