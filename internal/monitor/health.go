package monitor

import (
	"sync"
	"time"
)

// unhealthyThreshold is how many CONSECUTIVE failed checks a channel must
// accumulate before the OnChannelUnhealthy callback fires (once per
// crossing). At the fastest cadence (Twitch, ~15s) this is ~5 minutes of
// sustained failure — long enough to ride out transient GQL/network blips,
// short enough that a genuinely dead channel (renamed/banned/typo) surfaces
// before many streams are missed.
const unhealthyThreshold = 20

// ChannelHealth is the per-channel monitor status exposed via /api/status.
type ChannelHealth struct {
	ChannelID         string `json:"channelId"`
	LastCheckedAt     int64  `json:"lastCheckedAt"`       // epoch ms, 0 if never
	LastError         string `json:"lastError,omitempty"` // empty when last check succeeded
	ConsecutiveErrors int    `json:"consecutiveErrors"`
}

type channelState struct {
	lastCheckedAt     time.Time
	lastError         string
	consecutiveErrors int
	notified          bool // OnChannelUnhealthy already fired for the current failure streak
}

// healthTracker records per-channel check outcomes so persistently failing
// channels (which otherwise only log at Debug and are noticed when a stream
// is missed) become visible in /api/status and trigger a notification.
// Shared by all three monitors; its own mutex, independent of the monitor's.
type healthTracker struct {
	mu   sync.Mutex
	byID map[string]*channelState
	// onUnhealthy fires once when a channel crosses unhealthyThreshold
	// consecutive failures. Set by the wiring layer; nil = track only.
	onUnhealthy func(channelID string, consecutive int, lastErr string)
	// onHealthy fires ONCE when a channel that crossed the threshold answers
	// a check again. It is the pair of onUnhealthy: the unhealthy alert tells
	// an operator to go and check a rename, a ban or a typo, and without this
	// nothing ever tells them to stop. Set by the wiring layer; nil = track
	// only.
	//
	// It fires on the tracker's `notified` flag, which says an alert was
	// RAISED here. Whether that alert was actually delivered is a question one
	// layer up — cmd/moombox suppresses an alert a sibling monitor
	// contradicts — so the wiring, not this tracker, decides whether the close
	// is worth sending.
	onHealthy func(channelID string)
}

func newHealthTracker() *healthTracker {
	return &healthTracker{byID: make(map[string]*channelState)}
}

func (h *healthTracker) state(id string) *channelState {
	s := h.byID[id]
	if s == nil {
		s = &channelState{}
		h.byID[id] = s
	}
	return s
}

// recordSuccess clears a channel's failure streak, and fires onHealthy once
// when the streak it clears had crossed the threshold.
func (h *healthTracker) recordSuccess(id string) {
	h.mu.Lock()
	s := h.state(id)
	s.lastCheckedAt = time.Now()
	s.lastError = ""
	s.consecutiveErrors = 0
	fire := s.notified
	s.notified = false
	cb := h.onHealthy
	h.mu.Unlock()

	if fire && cb != nil {
		cb(id)
	}
}

// recordError bumps a channel's failure streak and fires onUnhealthy once
// when it crosses the threshold; the streak then stays silent until
// recordSuccess resets it.
func (h *healthTracker) recordError(id string, err error) {
	h.mu.Lock()
	s := h.state(id)
	s.lastCheckedAt = time.Now()
	s.lastError = err.Error()
	s.consecutiveErrors++
	fire := !s.notified && s.consecutiveErrors >= unhealthyThreshold
	if fire {
		s.notified = true
	}
	cb := h.onUnhealthy
	count := s.consecutiveErrors
	lastErr := s.lastError
	h.mu.Unlock()

	if fire && cb != nil {
		cb(id, count, lastErr)
	}
}

// snapshot returns the health of every tracked channel (unordered).
func (h *healthTracker) snapshot() []ChannelHealth {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]ChannelHealth, 0, len(h.byID))
	for id, s := range h.byID {
		var ms int64
		if !s.lastCheckedAt.IsZero() {
			ms = s.lastCheckedAt.UnixMilli()
		}
		out = append(out, ChannelHealth{
			ChannelID:         id,
			LastCheckedAt:     ms,
			LastError:         s.lastError,
			ConsecutiveErrors: s.consecutiveErrors,
		})
	}
	return out
}

// prune drops tracked channels no longer in the active set (removed from
// config) so the map can't grow unbounded on a 24/7 process.
func (h *healthTracker) prune(activeIDs map[string]struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id := range h.byID {
		if _, ok := activeIDs[id]; !ok {
			delete(h.byID, id)
		}
	}
}
