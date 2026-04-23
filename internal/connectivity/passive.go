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

	// If the remaining failures no longer meet the trigger threshold, clear
	// the triggered flag so consumers that poll IsTriggered don't see
	// stale positives.
	if !pt.triggered {
		return
	}
	if len(pt.failures) < pt.minFails {
		pt.triggered = false
		return
	}
	tags := make(map[string]struct{})
	for _, f := range pt.failures {
		tags[f.tag] = struct{}{}
	}
	if len(tags) < pt.minTags {
		pt.triggered = false
	}
}

func (pt *PassiveTracker) ShouldTriggerOffline() bool {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	pt.pruneOld()

	if len(pt.failures) < pt.minFails {
		return false
	}

	tags := make(map[string]struct{})
	for _, f := range pt.failures {
		tags[f.tag] = struct{}{}
	}
	if len(tags) < pt.minTags {
		return false
	}

	pt.triggered = true
	return true
}

func (pt *PassiveTracker) IsTriggered() bool {
	pt.mu.Lock()
	defer pt.mu.Unlock()
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
}
