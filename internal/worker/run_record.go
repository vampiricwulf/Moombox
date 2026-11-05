package worker

import "sync"

// runOnce is a job run's record of the things it does once however many
// times its strategies are set up again: every mid-stream refresh of a live
// capture re-runs a strategy's setup (refreshDownload), and a line or a
// switch that belongs to the run, not to one setup, must not repeat with
// each. Shared by pointer, as JobContext.Interruption is, so the value-copies
// of JobContext the live loop makes for its parts (segJobCtx := *jobCtx and
// friends) all read the same record. Set once in buildJobContext; nil in the
// JobContext literals tests build, where first answers true every time, as
// a run with no record of its own would.
type runOnce struct {
	mu   sync.Mutex
	done map[string]bool
}

// first reports whether key is new to the run, and records it.
func (o *runOnce) first(key string) bool {
	if o == nil {
		return true
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.done[key] {
		return false
	}
	if o.done == nil {
		o.done = map[string]bool{}
	}
	o.done[key] = true
	return true
}
