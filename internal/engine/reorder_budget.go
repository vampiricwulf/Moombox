package engine

import (
	"sync"
	"sync/atomic"
)

// reorderBudget is the PROCESS-WIDE ceiling on out-of-order segment bytes.
//
// Each reorderBuffer already bounds one download (see its type doc). Nothing
// bounded the sum: at the per-job ceiling, ten concurrent VOD jobs could hold
// ten ceilings' worth of segments resident, which on an arm64 box with 2 GB
// of RAM is the whole machine. This object is that missing sum — one
// reservation counter every live buffer charges its admissions against, with
// the ceilings themselves settable at runtime so a config save takes effect
// without a restart (owner ruling R3, 2026-09-24).
//
// Two invariants make it safe to put a second gate in front of admit():
//
//  1. The HEAD segment is never gated, here or per-buffer. Nothing frees the
//     budget except a flush and nothing flushes without its head, so a head
//     that waited on this counter would deadlock every download in the
//     process at once — not just its own. A head is still CHARGED (the
//     counter may exceed totalBytes), because release() has to be able to
//     free exactly what was taken.
//
//  2. Waiters are woken across buffers, THROUGH THEIR OWN MUTEX. A buffer
//     blocked on the shared total is waiting on ITS OWN cond, which only its
//     own take/setHead/markFailed/release broadcast — none of which will
//     fire, because the bytes it is waiting on belong to a different
//     download. So every buffer registers here at construction and
//     unregisters at release, and free()/configure() wake the whole registry.
//
//     The wake is NOT a bare Broadcast. admit() holds its own mu
//     continuously from the failed reserve() (which drops bg.mu on the way
//     out) to cond.Wait(); a free() that lands inside that window and
//     broadcasts without taking the waiter's mu fires into a cond with no
//     waiter yet, the signal is lost, and the worker parks with the budget
//     showing room. That is a lost wakeup, and it is exactly what the
//     buffer's own take/setHead/markFailed/release avoid by holding mu (or
//     having just held it) before they broadcast. wakeWaiters keeps the same
//     convention from the outside: snapshot the registry under bg.mu, DROP
//     bg.mu, then for each waiter Lock/Unlock its mu and only then
//     Broadcast. Acquiring the waiter's mu means the wake cannot overtake a
//     caller that is on its way into Wait — it blocks until that caller is
//     inside Wait and has released mu.
//
// Lock order is always buffer.mu -> reorderBudget.mu: admit() calls reserve()
// with its own lock held, while take() and release() call free() AFTER
// dropping theirs — so wakeWaiters, which holds NO buffer lock and no bg.mu
// when it takes a waiter's mu, never inverts it.
type reorderBudget struct {
	mu sync.Mutex
	// perJobBytes is the ceiling a NEW reorderBuffer is built with (the two
	// call sites read it through perJobLimit). A live buffer keeps the
	// ceiling it was constructed with; reconfiguring mid-download changes
	// what the NEXT buffer gets, which is the cheap half of hot reload and
	// the only half that matters — a download that is already running has
	// already sized its residency.
	perJobBytes int
	// totalBytes is the shared ceiling, enforced live. 0 = unbounded.
	totalBytes int
	// reserved is what every registered buffer currently holds between them.
	reserved int
	// waiters is every live buffer, so free/configure can wake the ones
	// parked on this ceiling. Registered by newReorderBufferOn, removed by
	// release() — which both production call sites always reach (the HLS VOD
	// path defers it; the catch-up path wires it to a watcher goroutine on
	// the always-closed `done` channel).
	waiters map[*reorderBuffer]struct{}
}

func newReorderBudget(perJobBytes, totalBytes int) *reorderBudget {
	return &reorderBudget{
		perJobBytes: perJobBytes,
		totalBytes:  totalBytes,
		waiters:     make(map[*reorderBuffer]struct{}),
	}
}

// sharedReorderBudget is the one every production reorderBuffer uses. Its
// starting values are what an UNCONFIGURED process gets — every test in this
// package, and any embedding that does not boot through cmd/moombox: the
// engine's own conservative per-job ceiling and no process-wide cap, so the
// behaviour before this change is exactly preserved until ConfigureReorder
// runs. cmd/moombox calls that once at boot and again on every config save.
var sharedReorderBudget = newReorderBudget(catchUpBufferBytes, 0)

// ConfigureReorder sets the process-wide reorder ceilings from config
// (downloader.reorder_buffer_mb / reorder_budget_mb, resolved to bytes by
// config.DownloaderConfig.ReorderLimitBytes). 0 on either means unbounded.
//
// Called from cmd/moombox at boot and from the config hot-reload applier —
// deliberately NOT threaded through engine.DownloaderOptions or the worker's
// strategies, which would have meant a new field on seven call sites to carry
// one process-wide number. Safe to call at any time, including with downloads
// in flight: it changes the ceilings and nothing else.
func ConfigureReorder(perJobBytes, totalBytes int) {
	sharedReorderBudget.configure(perJobBytes, totalBytes)
}

// ReorderLimits reports the ceilings ConfigureReorder last set, in bytes.
// Diagnostic — nothing on the download path reads it; it exists so the boot
// wiring can be tested from cmd/moombox without exporting the budget itself.
func ReorderLimits() (perJobBytes, totalBytes int) {
	bg := sharedReorderBudget
	bg.mu.Lock()
	defer bg.mu.Unlock()
	return bg.perJobBytes, bg.totalBytes
}

// configure replaces both ceilings and wakes every registered buffer so one
// blocked on the OLD total re-checks against the new one. Outstanding
// reservations are untouched: they are bytes that are genuinely resident, and
// zeroing the counter here would hand out a second budget on top of the first.
func (bg *reorderBudget) configure(perJobBytes, totalBytes int) {
	bg.mu.Lock()
	bg.perJobBytes = perJobBytes
	bg.totalBytes = totalBytes
	waiters := bg.snapshotWaitersLocked()
	bg.mu.Unlock()
	wakeWaiters(waiters)
}

// perJobLimit is the ceiling the two buffer call sites construct with.
func (bg *reorderBudget) perJobLimit() int {
	bg.mu.Lock()
	defer bg.mu.Unlock()
	return bg.perJobBytes
}

// reserve accounts n bytes against the shared ceiling for a NON-head segment,
// reporting false without charging anything when there is no room. The
// caller re-checks after being woken; it never retries in a spin.
//
// The rule mirrors the per-buffer one exactly — room is tested BEFORE the new
// bytes are added, so one admission may cross the line — so the two levels
// behave alike and the combined bound is the same shape the per-buffer doc
// already describes.
//
// A zero-byte reservation always succeeds: runHlsVodParallel admits a nil GAP
// SENTINEL for every failed fetch, and parking a zero-cost entry behind a
// byte ceiling would wedge the consumer on an index that costs nothing.
func (bg *reorderBudget) reserve(n int) bool {
	if n <= 0 {
		return true
	}
	bg.mu.Lock()
	defer bg.mu.Unlock()
	if bg.totalBytes > 0 && bg.reserved >= bg.totalBytes {
		return false
	}
	bg.reserved += n
	return true
}

// admitHead charges the head segment, which is never refused at either level
// (see the type doc). The counter may exceed totalBytes as a result; that is
// the price of the liveness guarantee, and it is bounded by one head segment
// per live download.
func (bg *reorderBudget) admitHead(n int) {
	if n <= 0 {
		return
	}
	bg.mu.Lock()
	defer bg.mu.Unlock()
	bg.reserved += n
}

// free returns n bytes to the shared ceiling and wakes every registered
// buffer. The floor at zero is defensive: the callers pass what they tracked,
// so it should never trip, but a negative reservation would read as unlimited
// room for every download in the process.
//
// Cost: one uncontended bg.mu acquisition plus one Lock/Unlock per registered
// buffer, per segment flush. At a handful of concurrent downloads that is
// ~N+1 uncontended lock pairs on a path that already does a map delete and a
// disk write, and it is off the ~60 Hz progress pipeline entirely. It IS a
// new process-wide serialisation point that grows with concurrency, and the
// obvious narrowing — skip the wake when totalBytes <= 0, since nothing can
// be parked on an unbounded shared ceiling — is deliberately NOT taken here:
// it would make the wake path behave differently in the two configurations,
// which is where a lost wakeup would hide. Revisit only with a profile.
func (bg *reorderBudget) free(n int) {
	if n <= 0 {
		return
	}
	bg.mu.Lock()
	bg.reserved -= n
	if bg.reserved < 0 {
		bg.reserved = 0
	}
	waiters := bg.snapshotWaitersLocked()
	bg.mu.Unlock()
	wakeWaiters(waiters)
}

func (bg *reorderBudget) register(rb *reorderBuffer) {
	bg.mu.Lock()
	defer bg.mu.Unlock()
	bg.waiters[rb] = struct{}{}
}

func (bg *reorderBudget) unregister(rb *reorderBuffer) {
	bg.mu.Lock()
	defer bg.mu.Unlock()
	delete(bg.waiters, rb)
}

// snapshotWaitersLocked copies the registry so the wake below can run with
// bg.mu released — taking a buffer's mu while holding bg.mu would invert the
// buffer.mu -> bg.mu order admit() establishes. Called with bg.mu held.
func (bg *reorderBudget) snapshotWaitersLocked() []*reorderBuffer {
	if len(bg.waiters) == 0 {
		return nil
	}
	out := make([]*reorderBuffer, 0, len(bg.waiters))
	for rb := range bg.waiters {
		out = append(out, rb)
	}
	return out
}

// wakeWaiters wakes each buffer THROUGH ITS OWN MUTEX — see invariant 2 in
// the type doc. Holding rb.mu across the Broadcast is the whole point and
// must not be "simplified" away: acquiring it blocks until any caller that is
// between its failed reserve() and its cond.Wait() has reached Wait and
// released mu, which is the window a bare Broadcast loses the signal in.
// Must be called with NO lock held.
func wakeWaiters(waiters []*reorderBuffer) {
	for _, rb := range waiters {
		rb.mu.Lock()
		rb.cond.Broadcast()
		rb.mu.Unlock()
	}
}

// beforeReorderWait is a test seam: admit() calls it after an admission
// attempt has failed and BEFORE cond.Wait(), with the buffer's mu still held.
// Nil in production, so the cost is one atomic load inside a lock already
// held, off the fetch path.
//
// It exists because the lost-wakeup window wakeWaiters closes is a few
// instructions wide and cannot be hit by timing: pinning the protocol needs a
// test that can run a free() INSIDE that window. Nothing in production
// assigns it.
//
// atomic.Pointer, not a bare func(): admit() reads this from every blocked
// worker goroutine, so a plain package global would be a data race the moment
// a test assigned it while another test's buffers were still admitting.
var beforeReorderWait atomic.Pointer[func()]
