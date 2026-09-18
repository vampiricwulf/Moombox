package engine

import "time"

// delays is every retry and backoff wait the live download loops sleep on, in
// one place so tests can run the same loops at millisecond scale. Production
// values are the package constants named beside each field (pinned by
// TestDefaultDelaysMatchConstants), so the constants remain the documentation
// of intent and this struct is the only knob. Tests poke it directly after
// NewSegmentDownloader, the way they already poke MayResume and OnActivity;
// nothing outside the package sees it. The first-segment hunt
// (firstSegmentHuntDelay), the direct-download backoffs (downloader_fetch.go)
// and the eviction probe keep their own constants: no test waits on them.
//
// fetchHardCeiling is the one member that is not a sleep: it is a per-fetch
// deadline, and it lives here for the same reason the sleeps do — a test that
// has to reach a 15-minute bound needs a knob, and this struct is the knob
// this package already has. The idle half of that pair (SegmentTimeout) is a
// package var instead, because the probes share it.
type delays struct {
	singleGoneRetry        time.Duration // singleGoneRetryDelay — one 410/404 while behind head
	interruptionStallRetry time.Duration // interruptionStallRetryDelay — the may-resume stall arm
	transientFailureRetry  time.Duration // transientFailureRetryDelay — 5xx/timeouts at the edge
	genericRetry           time.Duration // genericRetryDelay — manifest / unknown-status retries
	hlsPlaylistRetry       time.Duration // hlsPlaylistRetryDelay — playlist fetch or parse failed
	hlsStuckRetry          time.Duration // hlsStuckRetryDelay — a segment or the init keeps failing
	connectivityPoll       time.Duration // connectivityPollInterval — waitForConnectivity's ticker
	atEdgeBackoffUnit      time.Duration // the second the 429 backoff and same-head retry count in
	hlsReloadUnit          time.Duration // the second hlsReloadDelay scales playlist durations by
	hlsResumeSave          time.Duration // hlsResumeSaveInterval — live-loop resume sidecar floor
	fetchHardCeiling       time.Duration // segmentHardCeiling — one segment/chunk fetch's absolute lifetime
}

// defaultDelays returns production timing.
func defaultDelays() delays {
	return delays{
		singleGoneRetry:        singleGoneRetryDelay,
		interruptionStallRetry: interruptionStallRetryDelay,
		transientFailureRetry:  transientFailureRetryDelay,
		genericRetry:           genericRetryDelay,
		hlsPlaylistRetry:       hlsPlaylistRetryDelay,
		hlsStuckRetry:          hlsStuckRetryDelay,
		connectivityPoll:       connectivityPollInterval,
		atEdgeBackoffUnit:      time.Second,
		hlsReloadUnit:          time.Second,
		hlsResumeSave:          hlsResumeSaveInterval,
		fetchHardCeiling:       segmentHardCeiling,
	}
}
