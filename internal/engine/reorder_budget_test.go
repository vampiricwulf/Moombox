package engine

import (
	"sync"
	"testing"
	"time"
)

// budgetReserved reads the shared reservation under the budget's own lock.
// In-package, so the test can look straight at the field the production code
// guards; going through a getter that only tests call would be dead weight
// staticcheck's U1000 gate would then have to be argued with.
func budgetReserved(bg *reorderBudget) int {
	bg.mu.Lock()
	defer bg.mu.Unlock()
	return bg.reserved
}

// awaitClosed fails the test if ch is not closed within d. Every wait in this
// file is bounded: the failures this file exists to catch are DEADLOCKS, and
// an unbounded wait turns a caught deadlock into a hung suite.
func awaitClosed(t *testing.T, ch <-chan struct{}, d time.Duration, msg string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(d):
		t.Fatal(msg)
	}
}

// withFreshSharedBudget points sharedReorderBudget at a pristine instance for
// one test and restores the original afterwards.
//
// Tests elsewhere in this package build reorderBuffers and never release()
// them (they have no reason to — before the budget existed, a buffer owned
// nothing outside itself), so the process budget accumulates their registry
// entries and reservations over a run. That is invisible while the total is
// unbounded, and a trap for the next test that configures a non-zero TOTAL:
// it would inherit whatever earlier tests left reserved. Swapping the whole
// object rather than resetting the counters also discards the stale registry.
//
// A test that calls this MUST NOT call t.Parallel: it mutates a package var.
// That costs nothing — Go releases top-level parallel tests only after every
// sequential one has finished, so a sequential test can never overlap one.
func withFreshSharedBudget(t *testing.T) {
	t.Helper()
	prior := sharedReorderBudget
	sharedReorderBudget = newReorderBudget(catchUpBufferBytes, 0)
	t.Cleanup(func() { sharedReorderBudget = prior })
}

// TestSharedBudgetBlocksASecondBuffersNonHeadUntilTheFirstFlushes is the
// headline property of the process-wide budget: two concurrent downloads must
// not each hold a full per-job ceiling. Buffer A saturates a budget that has
// room for exactly two segments; buffer B's non-head then has to WAIT, and is
// released when A flushes — by B's own cond, woken across buffers.
//
// MUTANT: drop the shared reservation from admit (keep only the per-buffer
// `b.bytes < b.limit` test) — B's non-head is admitted immediately and the
// first select below fails. MUTANT: have free() skip waking the registered
// buffers — B never unblocks and the second select's timeout fires.
func TestSharedBudgetBlocksASecondBuffersNonHeadUntilTheFirstFlushes(t *testing.T) {
	const seg = 1 << 10
	bg := newReorderBudget(8*seg /* a per-job ceiling far above the budget */, 2*seg)

	a := newReorderBufferOn(bg, 8*seg, 0)
	b := newReorderBufferOn(bg, 8*seg, 100)

	if !a.admit(0, make([]byte, seg)) { // A's head
		t.Fatal("A's head was refused")
	}
	if !a.admit(1, make([]byte, seg)) { // A's non-head; budget now at 2/2
		t.Fatal("A's first non-head was refused while the budget had room")
	}
	if got := budgetReserved(bg); got != 2*seg {
		t.Fatalf("budget reserved = %d, want %d — admit must charge the shared total", got, 2*seg)
	}

	blocked := make(chan struct{})
	go func() {
		b.admit(101, make([]byte, seg)) // B's NON-head: seq 101 != head 100
		close(blocked)
	}()

	select {
	case <-blocked:
		t.Fatal("B's non-head was admitted while the shared budget was full — " +
			"the process-wide ceiling is not enforced, so N concurrent downloads hold N per-job ceilings")
	case <-time.After(200 * time.Millisecond):
	}

	if _, ok := a.take(0); !ok {
		t.Fatal("take(0) found nothing")
	}
	awaitClosed(t, blocked, 2*time.Second,
		"B's non-head never unblocked after A flushed — free() does not wake buffers registered with the budget")

	if got := budgetReserved(bg); got != 2*seg {
		t.Errorf("budget reserved after the hand-off = %d, want %d", got, 2*seg)
	}
}

// TestTheHeadIsAdmittedWithTheSharedBudgetExhausted pins the invariant that
// keeps the whole design live: the segment matching a buffer's head is
// admitted immediately at BOTH levels. Nothing frees the budget except a
// flush, and nothing flushes without its head — so a head that waited on the
// shared ceiling would deadlock every download at once.
//
// MUTANT: route the head through the ordinary budget check (delete the
// `seq == b.head` early return from admit) — this test's bounded wait fires,
// which is the deadlock the mutant introduces.
func TestTheHeadIsAdmittedWithTheSharedBudgetExhausted(t *testing.T) {
	const seg = 1 << 10
	bg := newReorderBudget(8*seg, seg)

	a := newReorderBufferOn(bg, 8*seg, 0)
	if !a.admit(0, make([]byte, seg)) { // exhausts the budget on its own
		t.Fatal("A's head was refused")
	}

	b := newReorderBufferOn(bg, 8*seg, 500)
	admitted := make(chan struct{})
	go func() {
		b.admit(500, make([]byte, 4*seg)) // B's HEAD, four times the whole budget
		close(admitted)
	}()
	awaitClosed(t, admitted, 2*time.Second,
		"a head segment blocked on the exhausted process-wide budget — nothing can ever free it, "+
			"so every concurrent download deadlocks together")

	// The head is charged even though it was never refused: accounting has to
	// stay exact or release() would free bytes the budget never knew about.
	if got, want := budgetReserved(bg), 5*seg; got != want {
		t.Errorf("budget reserved = %d, want %d — an over-committed head must still be charged", got, want)
	}
}

// TestAFreeInsideTheWaitWindowIsNotLost pins the WAKE PROTOCOL, which the
// tests above cannot reach: they all sleep before freeing, so the waiter is
// already inside cond.Wait() and any broadcast finds it.
//
// admit holds its own mu from the failed reserve() straight through to
// cond.Wait(). A free() that lands in that window and broadcasts WITHOUT
// taking the waiter's mu fires into a cond with no waiter, the signal is
// lost, and the worker parks against a budget that has room. In production
// today the consumer's setHead() eventually rescues it, so the symptom is a
// worker idling until its own index is the head rather than a hang — but the
// rescue is an accident of these two call sites, not a property of the
// buffer, and configure() has the same window (raising reorder_budget_mb
// would silently not take effect).
//
// beforeReorderWait widens the window to 100 ms and fires the ONLY free of
// the test inside it.
//
// MUTANT: replace wakeWaiters' `rb.mu.Lock(); rb.cond.Broadcast();
// rb.mu.Unlock()` with a bare `rb.cond.Broadcast()` — the free completes
// during the sleep, the broadcast is lost, and `admitted` never closes.
func TestAFreeInsideTheWaitWindowIsNotLost(t *testing.T) {
	const seg = 1 << 10
	bg := newReorderBudget(8*seg, seg)

	holder := newReorderBufferOn(bg, 8*seg, 0)
	if !holder.admit(0, make([]byte, seg)) { // the budget is now wholly held
		t.Fatal("the holder's head was refused")
	}
	waiter := newReorderBufferOn(bg, 8*seg, 500)

	t.Cleanup(func() { beforeReorderWait = nil })
	var once sync.Once
	freed := make(chan struct{})
	beforeReorderWait = func() {
		once.Do(func() {
			// Issued from another goroutine because this hook runs with the
			// waiter's mu HELD: a correct wakeWaiters blocks on that mu
			// until cond.Wait() releases it, which is exactly the barrier
			// under test.
			go func() {
				holder.take(0)
				close(freed)
			}()
			time.Sleep(100 * time.Millisecond)
		})
	}

	admitted := make(chan struct{})
	go func() {
		waiter.admit(501, make([]byte, seg))
		close(admitted)
	}()

	awaitClosed(t, admitted, 3*time.Second,
		"the only free() landed between the failed reserve() and cond.Wait() and was LOST — "+
			"wakeWaiters must take each waiter's mu before broadcasting")
	awaitClosed(t, freed, 3*time.Second, "the freeing goroutine never returned")
	waiter.release()
	holder.release()
}

// TestAZeroByteGapSentinelIsAdmittedWithTheBudgetExhausted covers
// runHlsVodParallel's nil GAP SENTINEL: a failed fetch admits a zero-length
// entry so the consumer's nextIdx never wedges on an index that is not
// coming. A zero-cost admission must never be refused by a byte ceiling.
//
// MUTANT: delete reserve()'s `n <= 0` fast path — the sentinel parks behind a
// full budget and this bounded wait fires.
func TestAZeroByteGapSentinelIsAdmittedWithTheBudgetExhausted(t *testing.T) {
	const seg = 1 << 10
	bg := newReorderBudget(8*seg, seg)

	a := newReorderBufferOn(bg, 8*seg, 0)
	a.admit(0, make([]byte, seg)) // budget full

	b := newReorderBufferOn(bg, 8*seg, 500)
	admitted := make(chan struct{})
	go func() {
		b.admit(501, nil) // a NON-head gap sentinel
		close(admitted)
	}()
	awaitClosed(t, admitted, 2*time.Second,
		"a zero-byte gap sentinel blocked on the shared budget — the consumer waits forever for an index "+
			"that costs nothing to store")
}

// TestTakeAndReleaseFreeExactlyWhatThisBufferReserved pins the accounting.
// take frees one segment's share; release frees whatever is still resident,
// exactly once, however many times it is called.
//
// MUTANT: release() frees nothing — the budget leaks a whole buffer's worth
// per download and the process quietly stops admitting anything. MUTANT:
// release() frees b.bytes without clearing the per-buffer counter — the
// second release double-frees and the final assertion reads a negative
// reservation as 0 while a sibling buffer over-admits.
func TestTakeAndReleaseFreeExactlyWhatThisBufferReserved(t *testing.T) {
	const seg = 1 << 10
	bg := newReorderBudget(8*seg, 100*seg)

	a := newReorderBufferOn(bg, 8*seg, 0)
	a.admit(0, make([]byte, seg))
	a.admit(1, make([]byte, 2*seg))
	a.admit(2, make([]byte, 3*seg))
	if got, want := budgetReserved(bg), 6*seg; got != want {
		t.Fatalf("budget reserved = %d, want %d", got, want)
	}

	a.take(1)
	if got, want := budgetReserved(bg), 4*seg; got != want {
		t.Errorf("budget reserved after take(1) = %d, want %d — take must free that segment's share", got, want)
	}

	a.release()
	if got := budgetReserved(bg); got != 0 {
		t.Errorf("budget reserved after release = %d, want 0 — release must free the whole outstanding reservation", got)
	}
	a.release() // idempotent
	if got := budgetReserved(bg); got != 0 {
		t.Errorf("budget reserved after a second release = %d, want 0 — release must not double-free", got)
	}

	// A take AFTER release must not free anything a second time either.
	a.take(2)
	if got := budgetReserved(bg); got != 0 {
		t.Errorf("budget reserved after a post-release take = %d, want 0", got)
	}
}

// TestAnUnboundedPerJobCeilingAdmitsEveryNonHead pins reorder_buffer_mb = 0.
//
// MUTANT: drop admit()'s `b.limit <= 0` arm — a configured-unbounded buffer
// has limit 0, `b.bytes < 0` is never true, and every non-head blocks
// forever. The bounded wait below is what catches it.
func TestAnUnboundedPerJobCeilingAdmitsEveryNonHead(t *testing.T) {
	bg := newReorderBudget(0, 0)
	rb := newReorderBufferOn(bg, 0 /* unbounded */, 0)
	rb.admit(0, make([]byte, 1<<20))

	done := make(chan struct{})
	go func() {
		for seq := 1; seq <= 8; seq++ {
			rb.admit(seq, make([]byte, 1<<20))
		}
		close(done)
	}()
	awaitClosed(t, done, 2*time.Second,
		"a non-head blocked against an UNBOUNDED per-job ceiling — reorder_buffer_mb = 0 deadlocks every download")

	if got, want := rb.residentBytes(), 9<<20; got != want {
		t.Errorf("residentBytes = %d, want %d", got, want)
	}
}

// TestAnUnboundedBudgetAdmitsEveryNonHead pins reorder_budget_mb = 0.
//
// MUTANT: drop reserve()'s `bg.totalBytes > 0` guard — reserved >= 0 is
// always true, so every non-head blocks and the bounded wait fires.
func TestAnUnboundedBudgetAdmitsEveryNonHead(t *testing.T) {
	bg := newReorderBudget(1<<30, 0 /* unbounded */)
	rb := newReorderBufferOn(bg, 1<<30, 0)

	done := make(chan struct{})
	go func() {
		for seq := range 8 {
			rb.admit(seq, make([]byte, 1<<20))
		}
		close(done)
	}()
	awaitClosed(t, done, 2*time.Second,
		"a non-head blocked against an UNBOUNDED process budget — reorder_budget_mb = 0 deadlocks every download")
}

// TestReconfiguringTwiceKeepsTheReservationsAccounted is the hot-reload case:
// a save while downloads are in flight changes the CEILINGS and nothing else.
// Reservations already taken stay taken, and the frees that follow are still
// exact.
//
// MUTANT: have configure() reset bg.reserved to 0 — the first assertion
// fails, and in production a reconfigure while three downloads are resident
// would hand out a second full budget on top of the first. MUTANT: have
// release() free the CURRENT per-job ceiling rather than what this buffer
// reserved — the last assertion reads a non-zero leftover.
func TestReconfiguringTwiceKeepsTheReservationsAccounted(t *testing.T) {
	const seg = 1 << 10
	bg := newReorderBudget(10*seg, 100*seg)

	rb := newReorderBufferOn(bg, 10*seg, 0)
	rb.admit(0, make([]byte, 4*seg))
	rb.admit(1, make([]byte, 4*seg))
	if got, want := budgetReserved(bg), 8*seg; got != want {
		t.Fatalf("budget reserved = %d, want %d", got, want)
	}

	bg.configure(20*seg, 200*seg)
	bg.configure(20*seg, 200*seg)

	if got, want := budgetReserved(bg), 8*seg; got != want {
		t.Errorf("budget reserved after two reconfigures = %d, want %d — reconfiguring changes the ceilings, "+
			"never the outstanding reservations", got, want)
	}
	if got := bg.perJobLimit(); got != 20*seg {
		t.Errorf("perJobLimit after reconfigure = %d, want %d", got, 20*seg)
	}

	rb.release()
	if got := budgetReserved(bg); got != 0 {
		t.Errorf("budget reserved after release = %d, want 0", got)
	}
}

// TestRaisingTheBudgetWakesABlockedBuffer pins the other half of hot reload:
// an operator who raises reorder_budget_mb because downloads are crawling
// must see it take effect on the downloads that are already crawling.
//
// MUTANT: drop the wake from configure() — the blocked admit sleeps until
// something else frees the budget, which in a stalled process is never, and
// the bounded wait fires.
func TestRaisingTheBudgetWakesABlockedBuffer(t *testing.T) {
	const seg = 1 << 10
	bg := newReorderBudget(8*seg, seg)

	a := newReorderBufferOn(bg, 8*seg, 0)
	a.admit(0, make([]byte, seg)) // budget full

	unblocked := make(chan struct{})
	go func() {
		a.admit(1, make([]byte, seg))
		close(unblocked)
	}()
	select {
	case <-unblocked:
		t.Fatal("the non-head was admitted before the budget was raised")
	case <-time.After(200 * time.Millisecond):
	}

	bg.configure(8*seg, 100*seg)
	awaitClosed(t, unblocked, 2*time.Second,
		"raising reorder_budget_mb did not wake the buffers blocked on the old ceiling")
}

// TestConfigureReorderDrivesTheSharedBudget pins the exported seam
// cmd/moombox calls, and the argument ORDER. It runs against a fresh shared
// budget so it neither sees nor leaves anything for its neighbours.
//
// MUTANT: swap ConfigureReorder's two arguments — both assertions fail.
func TestConfigureReorderDrivesTheSharedBudget(t *testing.T) {
	withFreshSharedBudget(t)

	ConfigureReorder(7<<20, 11<<20)
	perJob, total := ReorderLimits()
	if perJob != 7<<20 {
		t.Errorf("per-job limit = %d, want %d", perJob, 7<<20)
	}
	if total != 11<<20 {
		t.Errorf("total limit = %d, want %d", total, 11<<20)
	}
	if got := sharedReorderBudget.perJobLimit(); got != 7<<20 {
		t.Errorf("sharedReorderBudget.perJobLimit() = %d, want %d", got, 7<<20)
	}
}

// TestTheDefaultSharedBudgetMatchesTheEngineConstant pins what an
// UNCONFIGURED process (every test in this package, and any embedding that
// does not boot through cmd/moombox) gets: the engine's own conservative
// per-job ceiling and no process-wide cap.
//
// MUTANT: initialise sharedReorderBudget with a total — every existing
// ceiling test in this package starts sharing one budget and the ones that
// never release begin starving their successors.
func TestTheDefaultSharedBudgetMatchesTheEngineConstant(t *testing.T) {
	withFreshSharedBudget(t) // the same constructor production uses at init

	perJob, total := ReorderLimits()
	if perJob != catchUpBufferBytes {
		t.Errorf("the unconfigured per-job ceiling = %d, want catchUpBufferBytes (%d)", perJob, catchUpBufferBytes)
	}
	if total != 0 {
		t.Errorf("the unconfigured process-wide total = %d, want 0 (unbounded)", total)
	}
}

// TestManyBuffersUnderOneBudgetStayBounded is the fan-out proof: raising the
// number of concurrent downloads must not multiply resident memory.
//
// MUTANT: remove the shared reservation — residency climbs to
// buffers * perJob instead of plateauing at the budget.
func TestManyBuffersUnderOneBudgetStayBounded(t *testing.T) {
	const seg = 64 << 10
	const buffers = 12
	const budget = 4 * seg
	bg := newReorderBudget(100*seg, budget)

	var wg sync.WaitGroup
	rbs := make([]*reorderBuffer, buffers)
	for i := range buffers {
		rb := newReorderBufferOn(bg, 100*seg, i*1000)
		rbs[i] = rb
		wg.Add(1)
		go func(rb *reorderBuffer, base int) {
			defer wg.Done()
			for seq := base + 1; seq <= base+10; seq++ {
				rb.admit(seq, make([]byte, seg))
			}
		}(rb, i*1000)
	}

	time.Sleep(300 * time.Millisecond)

	// admit checks `reserved < total` BEFORE adding, so the last admission
	// through the gate can push the total to budget+seg-1; with every buffer
	// racing, at most one per buffer can be mid-flight past that check.
	if got, max := budgetReserved(bg), budget+buffers*seg; got > max {
		t.Errorf("budget reserved = %d with %d buffers, want <= %d — the shared ceiling is not holding",
			got, buffers, max)
	}

	for _, rb := range rbs {
		rb.release()
	}
	wg.Wait()
	if got := budgetReserved(bg); got != 0 {
		t.Errorf("budget reserved after releasing every buffer = %d, want 0", got)
	}
}
