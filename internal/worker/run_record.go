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
