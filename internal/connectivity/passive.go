package connectivity

import (
	"sync"
	"time"
)

const (
	defaultPassiveWindow   = 30 * time.Second
	defaultPassiveMinFails = 5
	defaultPassiveMinTags  = 2
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
}

func (pt *PassiveTracker) ReportSuccess(tag string) {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	pt.triggered = false
	pt.failures = pt.failures[:0]
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
