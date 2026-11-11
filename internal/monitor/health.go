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

	// restored holds the channels whose unhealthy alert a PREVIOUS process
	// raised and never closed (restoreUnhealthy). Each counts as a streak
	// that already crossed the threshold: its next success fires onHealthy,
	// and its next failure joins the streak silently, exactly as the same
	// channel would have behaved had the process not restarted. Kept apart
	// from byID so a channel this process has not checked yet stays out of
	// the /api/status snapshot.
	restored map[string]bool
}

func newHealthTracker() *healthTracker {
	return &healthTracker{byID: make(map[string]*channelState), restored: make(map[string]bool)}
}

// restoreUnhealthy marks channels as already alerted (see restored).
func (h *healthTracker) restoreUnhealthy(ids []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, id := range ids {
		h.restored[id] = true
	}
}

// takeRestored folds a restored alert into the channel's live state the first
// time the channel is checked. Caller holds h.mu.
func (h *healthTracker) takeRestored(id string, s *channelState) {
	if h.restored[id] {
		s.notified = true
		delete(h.restored, id)
	}
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
// when the streak it clears had crossed the threshold. It returns the length
// of the streak it cleared, 0 when the channel was not failing.
func (h *healthTracker) recordSuccess(id string) int {
	h.mu.Lock()
	s := h.state(id)
	h.takeRestored(id, s)
	s.lastCheckedAt = time.Now()
	s.lastError = ""
	cleared := s.consecutiveErrors
	s.consecutiveErrors = 0
	fire := s.notified
	s.notified = false
	cb := h.onHealthy
	h.mu.Unlock()

	if fire && cb != nil {
		cb(id)
	}
	return cleared
}

// recordError bumps a channel's failure streak and fires onUnhealthy once
// when it crosses the threshold; the streak then stays silent until
// recordSuccess resets it. It returns the streak's new length: 1 for the first
// failure after a success.
func (h *healthTracker) recordError(id string, err error) int {
	h.mu.Lock()
	s := h.state(id)
	h.takeRestored(id, s)
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
	return count
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
	for id := range h.restored {
		if _, ok := activeIDs[id]; !ok {
			delete(h.restored, id)
		}
	}
}

// FailingStreak is how many consecutive failed checks EVERY monitor covering
// a channel must have before both UIs flag the channel in their channel
// lists (owner ruling R-H1). It is far below unhealthyThreshold on purpose:
// the Discord alert waits 20 checks, which is over three hours at the feed
// monitor's default cadence, and until then a renamed, banned or mistyped
// channel looked healthy. The alert and its threshold are separate and
// unchanged.
const FailingStreak = 3

// ChannelMonitors is each monitor's health list, grouped by platform: the
// input of the "channel is failing" flag. It is the additive channelMonitors
// field of GET /api/status and of the initial_state frame, and the payload of
// the channel_monitors frame. YouTube holds the feed monitor's list and then
// DECAPI's; Twitch holds its one monitor's.
type ChannelMonitors struct {
	YouTube [][]ChannelHealth `json:"youtube"`
	Twitch  [][]ChannelHealth `json:"twitch"`
}

// ChannelFailing is the one rule for "this channel is failing for the
// operator" (owner ruling R-H1): covering holds one entry per monitor that
// tracks the channel, and the channel is failing when there is at least one
// and every one of them has FailingStreak or more consecutive failed checks.
// A monitor that still reaches the channel vouches for it, as it does for the
// Discord alert, so a lone RSS 404 beside a working DECAPI stays quiet; a
// monitor that has not checked the channel has no entry and says nothing.
// The flag is derived on every read and never stored: one success on any
// monitor resets that monitor's streak and so clears it.
//
// utils.js channelFailing is its mirror; the two are compared over
// web/tests/fixtures/channel-failing-parity.json.
func ChannelFailing(covering []ChannelHealth) bool {
	if len(covering) == 0 {
		return false
	}
	for _, h := range covering {
		if h.ConsecutiveErrors < FailingStreak {
			return false
		}
	}
	return true
}

// FailingChannels applies ChannelFailing to every channel in one platform's
// monitor lists and returns the failing ones by channel ID. The entry kept
// for a channel is the most recently checked one (a later list wins a tie),
// so the error shown is the newest.
func FailingChannels(monitors [][]ChannelHealth) map[string]ChannelHealth {
	covering := make(map[string][]ChannelHealth)
	for _, list := range monitors {
		for _, h := range list {
			covering[h.ChannelID] = append(covering[h.ChannelID], h)
		}
	}
	out := make(map[string]ChannelHealth)
	for id, entries := range covering {
		if !ChannelFailing(entries) {
			continue
		}
		shown := entries[0]
		for _, h := range entries[1:] {
			if h.LastCheckedAt >= shown.LastCheckedAt {
				shown = h
			}
		}
		out[id] = shown
	}
	return out
}

// Failing reports whether the channel is failing (ChannelFailing) and, when
// it is, the entry to show for it. An unknown platform reads as YouTube, as
// config.ChannelConfig.GetPlatform does.
func (c ChannelMonitors) Failing(platform, channelID string) (ChannelHealth, bool) {
	lists := c.YouTube
	if platform == "twitch" {
		lists = c.Twitch
	}
	h, ok := FailingChannels(lists)[channelID]
	return h, ok
}
