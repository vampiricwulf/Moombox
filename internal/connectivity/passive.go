package connectivity

import (
	"sync"
	"time"
)

const (
	defaultPassiveWindow   = 30 * time.Second
	defaultPassiveMinFails = 5
	defaultPassiveMinTags  = 2

	// maxFailureEntries caps the failure slice size to prevent unbounded
	// growth if a subsystem emits failures faster than the 30s window can
	// prune them. The threshold logic only needs minFails + minTags worth
	// of entries, so anything beyond a comfortable cap is wasted memory.
	maxFailureEntries = 256
)

type failureEntry struct {
	tag string
	at  time.Time
}

// PassiveTracker tracks network-level failures across subsystems.
type PassiveTracker struct {
	mu        sync.Mutex
	failures  []failureEntry
	triggered bool
	window    time.Duration
	minFails  int
	minTags   int
}

func NewPassiveTracker() *PassiveTracker {
	return &PassiveTracker{
		window:   defaultPassiveWindow,
		minFails: defaultPassiveMinFails,
		minTags:  defaultPassiveMinTags,
	}
}

func (pt *PassiveTracker) ReportFailure(tag string) {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	pt.failures = append(pt.failures, failureEntry{tag: tag, at: time.Now()})
	pt.pruneOld()
	// Hard cap: if a subsystem spams failures faster than pruneOld can
	// drop them, trim the oldest entries to keep memory bounded.
	if len(pt.failures) > maxFailureEntries {
		pt.failures = pt.failures[len(pt.failures)-maxFailureEntries:]
	}
}

// ReportSuccess removes failure entries for the given tag. The tracker
// triggers when multiple distinct tags have piled up failures within the
// window, so clearing per-tag gives the threshold logic a truthful picture
// rather than the prior "any success clears everything" behaviour — which
// let a single subsystem's intermittent-success pattern mask a genuine
// multi-subsystem outage.
//
// The triggered flag is cleared only when the pruned failure set can no
// longer meet the trigger threshold, so callers that rely on
// IsTriggered() state see a stable signal.
func (pt *PassiveTracker) ReportSuccess(tag string) {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	// Remove entries for this specific tag.
	filtered := pt.failures[:0]
	for _, f := range pt.failures {
		if f.tag != tag {
			filtered = append(filtered, f)
		}
	}
	pt.failures = filtered

	// Clear the latch once the surviving failures can no longer meet the
	// trigger threshold so consumers polling IsTriggered() don't see stale
	// positives. meetsThresholdLocked checks BOTH minFails and minTags.
	if pt.triggered && !pt.meetsThresholdLocked() {
		pt.triggered = false
	}
}

// ShouldTriggerOffline returns true when the failure window has piled up
// enough distinct subsystems to declare the network offline.
//
// **Side effect**: when this returns true, the tracker latches the
// `triggered` flag so subsequent IsTriggered() polls report offline until
// a ReportSuccess clears the failures below threshold. The name reads as
// a predicate but the latch is intentional — Monitor.ReportFailure relies
// on the latched state. Audit reports/small-packages.md.
func (pt *PassiveTracker) ShouldTriggerOffline() bool {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	pt.pruneOld()

	if !pt.meetsThresholdLocked() {
		return false
	}

	pt.triggered = true
	return true
}

// IsTriggered reads the cached latch state without pruning. Callers
// that have NOT recently invoked ShouldTriggerOffline() / ReportFailure()
// / ReportSuccess() may see a stale `true` for some time after the
// failure window has aged out — pruneOld is the only path that clears
// the latch on idle, and it runs only from those three methods. For
// fresh state without the trigger side-effect, use IsTriggeredPruned()
// (ShouldTriggerOffline() also prunes but latches `triggered` true when
// the threshold is met, which a read-only caller does not want).
func (pt *PassiveTracker) IsTriggered() bool {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	return pt.triggered
}

// IsTriggeredPruned prunes aged-out failures — which clears the latch when
// the survivors no longer meet the trigger threshold — and then returns the
// latch state. This is the correct call for a background poller
// (Monitor.poll) that does not otherwise feed the tracker: IsTriggered()
// reads the cached latch WITHOUT pruning, so a latch set during an outage
// would never clear once every subsystem gates off its network I/O (no
// ReportSuccess / ReportFailure / ShouldTriggerOffline fires) — pinning the
// monitor offline forever even after the active probe recovers.
func (pt *PassiveTracker) IsTriggeredPruned() bool {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	pt.pruneOld()
	return pt.triggered
}

func (pt *PassiveTracker) pruneOld() {
	cutoff := time.Now().Add(-pt.window)
	i := 0
	for i < len(pt.failures) && pt.failures[i].at.Before(cutoff) {
		i++
	}
	if i > 0 {
		pt.failures = pt.failures[i:]
	}
	// Clear the latch when pruning has dropped the surviving failures back
	// below the trigger threshold — checking BOTH minFails and minTags via
	// meetsThresholdLocked. Without this, an idle Moombox that goes quiet
	// after a brief failure burst keeps reporting IsTriggered()=true forever
	// (no ReportSuccess fires because no HTTP request runs to clear it), and
	// a set that aged down to a single tag would stay latched even though
	// ShouldTriggerOffline would no longer trigger.
	if pt.triggered && !pt.meetsThresholdLocked() {
		pt.triggered = false
	}
}

// meetsThresholdLocked reports whether the current failure set satisfies BOTH
// the minFails and minTags trigger conditions. The caller must hold pt.mu and
// is responsible for pruning aged-out entries first when a time-fresh answer
// is required. Centralising the predicate keeps the trigger condition and the
// latch-clear condition from drifting apart.
func (pt *PassiveTracker) meetsThresholdLocked() bool {
	if len(pt.failures) < pt.minFails {
		return false
	}
	tags := make(map[string]struct{}, pt.minTags)
	for _, f := range pt.failures {
		tags[f.tag] = struct{}{}
	}
	return len(tags) >= pt.minTags
}
