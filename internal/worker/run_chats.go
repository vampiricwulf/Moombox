package worker

import (
	"slices"
	"sync"
)

// chatStopper is the part of a chat downloader the run-chat registry needs:
// *chat.ChatDownloader, *twitch.ChatDownloader and *twitch.VodChatDownloader
// all have it, and each one's Stop is safe to call again on a downloader
// that has already stopped or finished.
type chatStopper interface {
	Stop()
}

// runChatRegistry holds, per job, the chat downloaders its current run
// built, so the two exits that end a run without going through an
// orchestrator's own chat teardown can stop them: a panic, which unwinds past
// the orchestrator's explicit cleanup calls (none is deferred), and
// setJobError, for a downloader the failing path did not stop itself.
//
// It exists because nothing else can reach them. The YouTube early chat is a
// local of the upcoming-wait loop, the orchestrator's own downloader a local
// of ExecuteWithChat, and a Twitch one travels in the StreamProcessResult —
// after a panic none of those frames is left, and the downloader went on
// running: its OnStart wrote chat_status "downloading" over the Error row's
// settle (settleErrorChat), and its polls went on for a job that had failed.
//
// Keyed by job ID because the question asked is "this run's downloaders".
// One run per job at a time (the queue's registration), and processJob
// releases the entry on its way out, so the next run starts from none.
//
// An entry holds what its run still owns, not everything it ever built: the
// owner that drops a downloader has it forgotten (forget), and a run that
// goes round again in place starts its new pass from none (nextPass).
type runChatRegistry struct {
	mu   sync.Mutex
	runs map[string]*runChats
}

// runChats is one run's entry: its downloaders, and whether the run has
// stopped them. mu is held across a "downloading" write (whileRunning), so
// stop returns only once no such write is in flight and none can start: the
// settle that follows a stop is the last word on chat_status.
type runChats struct {
	mu      sync.Mutex
	stopped bool
	sources []chatStopper
}

func newRunChatRegistry() *runChatRegistry {
	return &runChatRegistry{runs: make(map[string]*runChats)}
}

// add registers a downloader the job's current run built and returns the
// run's entry, which the downloader's "downloading" write goes through
// (whileRunning). A nil registry — an orchestrator or stream processor built
// on its own in a test — registers nothing and returns nil, which gates
// nothing. A downloader added to a run already stopped is stopped at once:
// it was built by a run that has ended.
func (r *runChatRegistry) add(jobID string, s chatStopper) *runChats {
	if r == nil || s == nil {
		return nil
	}
	r.mu.Lock()
	rc := r.runs[jobID]
	if rc == nil {
		rc = &runChats{}
		r.runs[jobID] = rc
	}
	r.mu.Unlock()
	rc.mu.Lock()
	rc.sources = append(rc.sources, s)
	stopped := rc.stopped
	rc.mu.Unlock()
	if stopped {
		s.Stop()
	}
	return rc
}

// whileRunning runs write unless the run has stopped its chat, and reports
// whether it ran. write is a chat_status "downloading" write: one made after
// the stop would land on the row the run's exit settled.
func (rc *runChats) whileRunning(write func()) bool {
	if rc == nil {
		write()
		return true
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.stopped {
		return false
	}
	write()
	return true
}

// stop stops every downloader the job's current run registered and closes
// the run's "downloading" gate, and reports how many it stopped. Idempotent:
// a second stop finds the gate closed and stops them again, which each
// downloader's Stop allows.
func (r *runChatRegistry) stop(jobID string) int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	rc := r.runs[jobID]
	r.mu.Unlock()
	if rc == nil {
		return 0
	}
	rc.mu.Lock()
	rc.stopped = true
	sources := append([]chatStopper(nil), rc.sources...)
	rc.mu.Unlock()
	// Outside the entry's lock: a downloader's Stop takes its own.
	for _, s := range sources {
		s.Stop()
	}
	return len(sources)
}

// release ends the run's entry, at processJob's exit: the gate closes, so a
// downloader that outlived the run writes "downloading" over nothing, and the
// job's next run registers into a fresh entry. It stops no downloader — an
// exit that is not a failure has run its orchestrator's own teardown.
func (r *runChatRegistry) release(jobID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	rc := r.runs[jobID]
	delete(r.runs, jobID)
	r.mu.Unlock()
	if rc == nil {
		return
	}
	rc.mu.Lock()
	rc.stopped = true
	rc.mu.Unlock()
}

// forget lets go of a downloader its owner has stopped or seen finish and is
// dropping: the waiting-room chat the upcoming wait replaces each time
// YouTube ends it, or stops at its exits. Without it a run's entry held every
// downloader the run ever built until the run ended — over a multi-day wait,
// each with its options, closures and buffered messages. The run is not
// named: the owners that drop one (stopEarlyChat) hold only the instance, and
// the entries are as few as the running jobs.
func (r *runChatRegistry) forget(s chatStopper) {
	if r == nil || s == nil {
		return
	}
	r.mu.Lock()
	entries := make([]*runChats, 0, len(r.runs))
	for _, rc := range r.runs {
		entries = append(entries, rc)
	}
	r.mu.Unlock()
	for _, rc := range entries {
		rc.mu.Lock()
		rc.sources = slices.DeleteFunc(rc.sources, func(held chatStopper) bool { return held == s })
		rc.mu.Unlock()
	}
}

// nextPass ends a pass of the job's run that is about to run again in place
// (processJob's loop: a rate limit or a setup outage waited out): what the
// earlier pass registered is stopped — its own teardown has run, and each
// Stop is safe to repeat; this is the net under it — and let go, and the next
// pass's downloaders register into the same entry, its gate still open. It
// reports how many it let go.
func (r *runChatRegistry) nextPass(jobID string) int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	rc := r.runs[jobID]
	r.mu.Unlock()
	if rc == nil {
		return 0
	}
	rc.mu.Lock()
	sources := rc.sources
	rc.sources = nil
	rc.mu.Unlock()
	// Outside the entry's lock, as stop's.
	for _, s := range sources {
		s.Stop()
	}
	return len(sources)
}
