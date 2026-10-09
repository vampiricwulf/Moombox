package engine

import "time"

// delays is every retry and backoff wait the live download loops sleep on, in
// one place so tests can run the same loops at millisecond scale. Production
// values are the package constants named beside each field (pinned by
// TestDefaultDelaysMatchConstants), so the constants remain the documentation
// of intent and this struct is the only knob. Tests poke it directly after
// NewSegmentDownloader, the way they already poke MayResume and OnActivity;
// nothing outside the package sees it. The first-segment hunt
// (firstSegmentHuntDelay) and the eviction probe (evictionProbeRetryDelay)
// keep their own constants: no test waits on them. The whole-file
// direct-download path is not among them — its size probe backs off in
// genericRetry, its chunk ladder and outage verdict count in
// atEdgeBackoffUnit, and its outage waits poll on connectivityPoll.
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
	atEdgeBackoffUnit      time.Duration // the second the 429 backoff, same-head retry, per-chunk backoff and outage verdict count in
	hlsReloadUnit          time.Duration // the second hlsReloadDelay scales playlist durations by
	hlsResumeSave          time.Duration // hlsResumeSaveInterval — live-loop resume sidecar floor
	fetchHardCeiling       time.Duration // segmentHardCeiling — one segment/chunk fetch's absolute lifetime
}

// SetFastDelaysForTests divides every retry and backoff wait by scale. It
// exists for tests in OTHER packages: internal/worker's
// TestFinalizeIncompleteTailInterruption drives a real SegmentDownloader
// through a Tier-2 finalize and paid the production retry ladder — 10.1 s,
// a quarter of the whole suite's wall time — because this struct is
// unexported and its in-package fastDelays() helper unreachable from there
// (sweep-2 TOOL-6, owner decision O-Q).
//
// Every member moves by the one factor, fetchHardCeiling included, so
// escalation counts and orderings stay exactly production's — a field left
// behind would keep one production wait in the loop, which is the cost this
// seam exists to remove.
//
// Production never calls this: NewSegmentDownloader installs defaultDelays()
// and nothing else writes the field, so the shipped timings are unchanged.
// A production caller is a review finding. A scale of 0 or less is ignored.
func (d *SegmentDownloader) SetFastDelaysForTests(scale int) {
	if scale <= 0 {
		return
	}
	n := time.Duration(scale)
	p := defaultDelays()
	d.delays = delays{
		singleGoneRetry:        p.singleGoneRetry / n,
		interruptionStallRetry: p.interruptionStallRetry / n,
		transientFailureRetry:  p.transientFailureRetry / n,
		genericRetry:           p.genericRetry / n,
		hlsPlaylistRetry:       p.hlsPlaylistRetry / n,
		hlsStuckRetry:          p.hlsStuckRetry / n,
		connectivityPoll:       p.connectivityPoll / n,
		atEdgeBackoffUnit:      p.atEdgeBackoffUnit / n,
		hlsReloadUnit:          p.hlsReloadUnit / n,
		hlsResumeSave:          p.hlsResumeSave / n,
		fetchHardCeiling:       p.fetchHardCeiling / n,
	}
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
