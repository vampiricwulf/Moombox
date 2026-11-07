package worker

import "sync"

// runRecord is a job run's record of what holds for the whole run however
// many times its strategies are set up again: every mid-stream refresh of a
// live capture re-runs a strategy's setup (refreshDownload), and a line, a
// switch or a path that belongs to the run, not to one setup, must not repeat
// with each or be forgotten by the next. Shared by pointer, as
// JobContext.Interruption is, so the value-copies of JobContext the live loop
// makes for its parts (segJobCtx := *jobCtx and friends) all read the same
// record. Set once in buildJobContext; nil in the JobContext literals tests
// build, where first answers true every time and the capture is never on the
// no-JS path, as a run with no record of its own would be.
type runRecord struct {
	mu   sync.Mutex
	done map[string]bool
	// onNoJS: the capture records on the formats that need no player JS
	// (noJSFromStart) — the challenge-free setup fallback put it there, or
	// a mid-stream path switch did (switchLivePath) — so its refreshes stay
	// there (refreshDownload).
	onNoJS bool
	// onHlsFallback: a fallback put the capture on the live HLS playlist
	// (liveHlsInstead: a from-start setup or refresh that failed for want of
	// a GVS token or on the player's JS challenges). The marathon live-edge
	// capture and an evicted start's HLS are not fallbacks: their from-start
	// path is gone, not unsolvable.
	onHlsFallback bool
	// fallbackKey is the mid-stream switch (switchLivePath's noJSSwitchKey
	// or hlsSwitchKey) that put the capture on the fallback path it records
	// on, empty when a setup fallback put it there — a setup fallback spends
	// no switch. The switch back gives that one use back (markSwitchedBack,
	// owner decision PE-3).
	fallbackKey string
}

// switchBackKey is the run's one switch back to the from-start path
// (canSwitchBack, markSwitchedBack; owner decision PR-R3).
const switchBackKey = "live-switch:back"

// noJSSwitchKey and hlsSwitchKey are the run's mid-stream path switches
// (switchLivePath), each spent once: to the no-JS from-start path, and to
// the live HLS playlist.
const (
	noJSSwitchKey = "live-switch:nojs"
	hlsSwitchKey  = "live-switch:hls"
)

// markSwitchedTo records that the mid-stream switch key (noJSSwitchKey or
// hlsSwitchKey) put the capture on the fallback path it now records on, the
// use the switch back gives back (markSwitchedBack). A nil record keeps
// nothing.
func (r *runRecord) markSwitchedTo(key string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.fallbackKey = key
	r.mu.Unlock()
}

// markHlsFallback records that a fallback put the capture on the live HLS
// playlist. A nil record keeps nothing.
func (r *runRecord) markHlsFallback() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.onHlsFallback = true
	r.mu.Unlock()
}

// canSwitchBack reports whether the capture records on a fallback path —
// the formats that need no player JS, or the live HLS playlist a fallback
// chose — and has not yet used the run's one switch back to the from-start
// path that needs the JS (owner decision PR-R3).
func (r *runRecord) canSwitchBack() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return (r.onNoJS || r.onHlsFallback) && !r.done[switchBackKey]
}

// markSwitchedBack records the run's one switch back: the capture records on
// the from-start path again, its refreshes with it (refreshDownload), and no
// other switch back is made this run.
//
// It re-arms one fallback (owner decision PE-3): the mid-stream switch that
// put the capture on the path it leaves (fallbackKey) is given back, so a
// second systemic failure later in the run — the same transient cause
// again, a sidecar restart, a player CDN outage — can fall back once more,
// in a new part with the recordings so far kept, where it found the switch
// spent and the capture stopped on the failure. One use only, the switch
// the capture is leaving: a no-JS attempt that found no pair before the HLS
// switch stays spent, and the switch back itself stays spent, so a capture
// that falls back again after it records on that fallback to the end of
// the run. A setup fallback spent no switch, and nothing is given back
// for it.
func (r *runRecord) markSwitchedBack() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onNoJS, r.onHlsFallback = false, false
	if r.done == nil {
		r.done = map[string]bool{}
	}
	if r.fallbackKey != "" {
		delete(r.done, r.fallbackKey)
		r.fallbackKey = ""
	}
	r.done[switchBackKey] = true
}

// first reports whether key is new to the run, and records it.
func (r *runRecord) first(key string) bool {
	if r == nil {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done[key] {
		return false
	}
	if r.done == nil {
		r.done = map[string]bool{}
	}
	r.done[key] = true
	return true
}

// markNoJS records that the capture now records on the formats that need no
// player JS. A nil record keeps nothing.
func (r *runRecord) markNoJS() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.onNoJS = true
	r.mu.Unlock()
}

// noJS reports whether the capture records on the formats that need no
// player JS.
func (r *runRecord) noJS() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.onNoJS
}
