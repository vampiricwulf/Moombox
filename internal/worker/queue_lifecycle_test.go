package worker

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// jobIDf names the synthetic jobs these tests enqueue in bulk.
func jobIDf(i int) string { return "job-" + strconv.Itoa(i) }

// TestDequeueDoesNotTakeALifecycleSlot pins owner decision O-F: the wait phase
// (Upcoming, a manually-added offline Twitch channel) runs slot-free, so a
// hundred waiting jobs cannot stop a newly live stream from being dequeued.
//
// Two separate hundreds are in play and the test keeps them apart. maxLifecycle
// is 100, and the backlog cap (len(q.pending) >= 100) is a different hundred
// that TestEnqueueDropLogsOncePerJob owns: the 101st job is therefore enqueued
// only AFTER the first hundred have left the backlog, because offering it up
// front would be swallowed by the cap and would prove nothing about slots.
//
// Mutant: restoring `q.activeLifecycle < q.maxLifecycle` to Dequeue's
// condition and the `q.activeLifecycle++` beside it — the 101st Dequeue blocks
// and the live job is never started (not dropped, not logged).
func TestDequeueDoesNotTakeALifecycleSlot(t *testing.T) {
	q := NewJobQueue(10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for i := range 100 {
		q.Enqueue(jobIDf(i), database.StatusUpcoming)
	}
	for range 100 {
		if _, _, ok := q.Dequeue(ctx); !ok {
			t.Fatal("Dequeue = false while jobs were pending")
		}
	}
	if n := q.LifecycleCount(); n != 0 {
		t.Fatalf("LifecycleCount() = %d with a hundred jobs in the wait phase, want 0 — "+
			"the slot belongs to the ShouldDownload decision, not to Dequeue", n)
	}

	q.Enqueue("newly-live", database.StatusLive)
	got := make(chan bool, 1)
	go func() {
		id, _, ok := q.Dequeue(ctx)
		got <- ok && id == "newly-live"
	}()
	select {
	case ok := <-got:
		if !ok {
			t.Fatal("Dequeue = false for the 101st waiting job")
		}
	case <-time.After(time.Second):
		t.Fatal("Dequeue blocked on the 101st waiting job — the wait phase is still holding lifecycle slots")
	}
}

// TestDeclinedJobNeverTouchesTheLifecycleCount states the owner's ruling at the
// queue level: a job stream processing DECLINES (disabled channel, filter,
// duplicate) never takes the slot, so dequeue → Complete with no acquire in
// between leaves both counters exactly where they started and leaves every
// slot there to be taken afterwards.
//
// Mutants: taking the slot in Dequeue — the first assertion reads 1; releasing
// unconditionally in Complete instead of only for a holder — the count goes
// negative and the "one too many" acquire at the end succeeds.
func TestDeclinedJobNeverTouchesTheLifecycleCount(t *testing.T) {
	q := NewJobQueue(10)
	q.maxLifecycle = 3
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	q.Enqueue("declined", database.StatusUpcoming)
	if _, _, ok := q.Dequeue(ctx); !ok {
		t.Fatal("Dequeue = false with one job pending")
	}
	if n, a := q.LifecycleCount(), q.ActiveCount(); n != 0 || a != 0 {
		t.Fatalf("after Dequeue: LifecycleCount = %d, ActiveCount = %d; want 0/0", n, a)
	}

	q.Complete("declined")
	if n, a := q.LifecycleCount(), q.ActiveCount(); n != 0 || a != 0 {
		t.Fatalf("after Complete: LifecycleCount = %d, ActiveCount = %d; want 0/0", n, a)
	}

	// Nothing leaked: all three slots are still there.
	free, freeCancel := context.WithTimeout(ctx, 2*time.Second)
	defer freeCancel()
	for i := range 3 {
		if !q.AcquireLifecycleSlot(free, jobIDf(i)) {
			t.Fatalf("AcquireLifecycleSlot(%d) = false — the declined job spent a slot it never used", i)
		}
	}
	// ...and nothing was over-released: the cap still holds.
	full, fullCancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer fullCancel()
	if q.AcquireLifecycleSlot(full, "one-too-many") {
		t.Fatal("a fourth AcquireLifecycleSlot succeeded past maxLifecycle = 3")
	}
}

// TestAcquireLifecycleSlotGatesDownloads pins the other half: the slot is
// taken at the ShouldDownload decision, and released by Complete.
//
// Mutants, one per assertion:
//   - never taking the slot: the cap is gone entirely and the 101st download
//     starts.
//   - not releasing it in Complete: the cap leaks and the pool wedges after
//     100 finished jobs.
func TestAcquireLifecycleSlotGatesDownloads(t *testing.T) {
	q := NewJobQueue(10)
	q.maxLifecycle = 2
	ctx := context.Background()

	// Bounded on purpose: under the mutant that restores the Dequeue gate the
	// third dequeue blocks forever, and an unbounded setup turns that kill
	// into a timeout panic dump instead of a named failure (Task 9 review,
	// Minor 2).
	setup, setupCancel := context.WithTimeout(ctx, 2*time.Second)
	defer setupCancel()
	for i := range 3 {
		q.Enqueue(jobIDf(i), database.StatusLive)
		if _, _, ok := q.Dequeue(setup); !ok {
			t.Fatalf("setup dequeue %d = false — the queue gated the dequeue on a lifecycle slot", i)
		}
	}
	if !q.AcquireLifecycleSlot(ctx, jobIDf(0)) || !q.AcquireLifecycleSlot(ctx, jobIDf(1)) {
		t.Fatal("the first two AcquireLifecycleSlot calls must succeed")
	}

	blocked := make(chan bool, 1)
	waitCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	go func() { blocked <- q.AcquireLifecycleSlot(waitCtx, jobIDf(2)) }()
	if ok := <-blocked; ok {
		t.Fatal("AcquireLifecycleSlot succeeded past maxLifecycle")
	}

	q.Complete(jobIDf(0))
	// Bounded on purpose: the "Complete does not release" mutant makes this
	// acquire block forever, and a test that hangs to the package timeout
	// reports a panic dump instead of naming the invariant it lost.
	released, releasedCancel := context.WithTimeout(ctx, 2*time.Second)
	defer releasedCancel()
	if !q.AcquireLifecycleSlot(released, jobIDf(2)) {
		t.Fatal("Complete did not release the lifecycle slot")
	}
}

// TestCompleteReleasesTheLifecycleSlotWithoutAProcessingRow pins the release as
// keyed by the SLOT, not by the processing row. processJob reaches Complete
// twice on its error paths — setJobError and handleCancellation free the slot
// before their DB writes and the deferred Complete runs again on the way out —
// and the row-deleted path reaches it with the processing entry already gone.
//
// Mutants: nesting releaseLifecycleSlotLocked back inside the
// `if cancel, ok := q.processing[jobID]` branch — the orphan's slot leaks and
// the second acquire never returns; dropping the holdingLifecycle guard — the
// repeat Complete over-releases and the final count reads 0 instead of 1.
func TestCompleteReleasesTheLifecycleSlotWithoutAProcessingRow(t *testing.T) {
	q := NewJobQueue(10)
	q.maxLifecycle = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if !q.AcquireLifecycleSlot(ctx, "orphan") {
		t.Fatal("first AcquireLifecycleSlot = false")
	}
	// No Dequeue, so q.processing holds no entry for "orphan" — the shape a
	// job whose row vanished mid-flight leaves behind.
	q.Complete("orphan")
	if n := q.LifecycleCount(); n != 0 {
		t.Fatalf("LifecycleCount() = %d after Complete, want 0 — the slot outlived the processing row", n)
	}

	second, secondCancel := context.WithTimeout(ctx, time.Second)
	defer secondCancel()
	if !q.AcquireLifecycleSlot(second, "next") {
		t.Fatal("Complete did not release the lifecycle slot — the pool is wedged at one job")
	}

	// The second Complete every error path fires must not over-release.
	q.Complete("orphan")
	if n := q.LifecycleCount(); n != 1 {
		t.Fatalf("LifecycleCount() = %d after a repeat Complete of an already-released job, want 1", n)
	}
}

// TestEnqueueDropLogsOncePerJob pins the last clause of O-F: the 60 s
// heartbeat re-offers every dropped job forever, and the Warn used to fire
// every time — one line per minute per job.
//
// Mutants: dropping the droppedLogged bookkeeping — the first subtest reads 3
// instead of 1; dropping the delete beside the successful append — the second
// subtest reads 1 instead of 2, so a job that got in and was later dropped
// again is silently dropped, and the map grows without bound.
func TestEnqueueDropLogsOncePerJob(t *testing.T) {
	q := NewJobQueue(10)
	counter := &warnCaptureLogger{}
	q.SetLogger(counter)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for i := range 100 {
		q.Enqueue(jobIDf(i), database.StatusUpcoming)
	}
	t.Run("once per job", func(t *testing.T) {
		for range 3 { // three simulated heartbeats re-offering the same job
			q.Enqueue("overflow", database.StatusUpcoming)
		}
		if len(counter.warns) != 1 {
			t.Fatalf("drop warns = %d, want 1 per job: %v", len(counter.warns), counter.warns)
		}
	})

	t.Run("re-arms once the job gets in", func(t *testing.T) {
		q.Dequeue(ctx)                                   // frees one backlog place
		q.Enqueue("overflow", database.StatusLive)       // this offer gets in
		q.Cancel("overflow")                             // ...and leaves again
		q.Enqueue(jobIDf(1000), database.StatusUpcoming) // backlog back at 100
		q.Enqueue("overflow", database.StatusUpcoming)   // a NEW drop episode
		if len(counter.warns) != 2 {
			t.Fatalf("drop warns = %d after the job got in and was dropped again, want 2: %v",
				len(counter.warns), counter.warns)
		}
	})
}

// TestLifecycleWaitWarnsWhenItBlocks pins sweep-2 Task 11's extra item (c)
// (Task 9 review, Important 1): at the cap a live capture waited with no log
// line at all — a hundred slots held, a job parked indefinitely, and nothing
// anywhere to say which job or how many were in front of it. One Warn per
// wait, once the wait passes the threshold, naming the job and the held
// count; and the wait still ends promptly on ctx, so Stop is not delayed
// behind it.
//
// Mutants: dropping the Warn (the first select times out); re-arming the
// threshold timer on every wakeup, so a wedged pool logs once per release
// instead of once per wait (the second select fires); removing the ctx arm
// from the acquire's select (the cancelled acquire never returns).
func TestLifecycleWaitWarnsWhenItBlocks(t *testing.T) {
	q := NewJobQueue(10)
	q.maxLifecycle = 1
	q.lifecycleWarnAfter = 20 * time.Millisecond
	lg := newWarnChanLogger()
	q.SetLogger(lg)

	if !q.AcquireLifecycleSlot(context.Background(), "holder") {
		t.Fatal("the first AcquireLifecycleSlot must succeed")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	parked := make(chan bool, 1)
	go func() { parked <- q.AcquireLifecycleSlot(ctx, "parked-job") }()

	select {
	case line := <-lg.warns:
		joined := fmt.Sprint(line...)
		for _, want := range []string{"parked-job", "1"} {
			if !strings.Contains(joined, want) {
				t.Errorf("the blocked-acquire Warn %q does not name %q — the operator needs the job and the held count", joined, want)
			}
		}
		// The measured wait and the threshold that released the line are
		// separate fields: "waited=30s" alone reads as an elapsed time and is
		// not one (fix round 1, Minor 6). Mutant: logging warnAfter under
		// "waited" and dropping "threshold" — the field lookup below fails.
		fields := map[string]string{}
		for i := 1; i+1 < len(line); i += 2 {
			fields[fmt.Sprint(line[i])] = fmt.Sprint(line[i+1])
		}
		if fields["threshold"] != q.lifecycleWarnAfter.String() {
			t.Errorf("Warn threshold field = %q, want %q", fields["threshold"], q.lifecycleWarnAfter)
		}
		waited, err := time.ParseDuration(fields["waited"])
		if err != nil {
			t.Errorf("Warn waited field = %q, which does not parse as a duration: %v", fields["waited"], err)
		} else if waited < q.lifecycleWarnAfter {
			t.Errorf("Warn waited = %v, which is less than the threshold %v it fired at — it is not a measurement", waited, q.lifecycleWarnAfter)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no Warn after the wait threshold — a job parked behind the lifecycle cap is invisible")
	}

	// Once per wait, not once per wakeup: nudge the waiter awake with a
	// spurious signal and confirm it does not log the same wait again.
	select {
	case q.lifeNotify <- struct{}{}:
	default:
	}
	select {
	case line := <-lg.warns:
		t.Errorf("a second Warn for the same wait: %v — one line per wait, or a wedged pool is a log flood", line)
	case <-time.After(200 * time.Millisecond):
	}

	cancel()
	select {
	case ok := <-parked:
		if ok {
			t.Error("AcquireLifecycleSlot = true after its context was cancelled")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("AcquireLifecycleSlot ignored its cancelled context — Stop would block behind the wait")
	}
	q.Complete("holder")
}

// TestAcquireLifecycleSlotCascadingWakeup mirrors
// TestAcquireDownloadSlotCascadingWakeup for the lifecycle pool, which is a
// line-for-line copy of that mechanism and shipped with neither of its guards
// (sweep-2 Task 9 review, Important 2). lifeNotify has capacity 1, so two
// releases in quick succession collapse into one signal; without the
// acquirer's cascade forward one of two parked waiters would sleep beside a
// free slot until the NEXT release.
//
// Mutants: deleting the lifeNotify send from releaseLifecycleSlotLocked (m11 —
// neither waiter is ever woken); NewJobQueue leaving lifeNotify nil (m12 — a
// nil channel never delivers, same symptom); removing the stillFree cascade
// forward (the second waiter is stranded).
func TestAcquireLifecycleSlotCascadingWakeup(t *testing.T) {
	q := NewJobQueue(10)
	q.maxLifecycle = 2
	ctx := context.Background()

	if !q.AcquireLifecycleSlot(ctx, "a") || !q.AcquireLifecycleSlot(ctx, "b") {
		t.Fatal("initial acquires failed")
	}

	acquired := make(chan string, 2)
	for _, id := range []string{"w1", "w2"} {
		go func(id string) {
			if q.AcquireLifecycleSlot(ctx, id) {
				acquired <- id
			}
		}(id)
	}
	// Let both waiters park on lifeNotify before releasing.
	time.Sleep(50 * time.Millisecond)

	q.Complete("a")
	q.Complete("b")

	for i := range 2 {
		select {
		case <-acquired:
		case <-time.After(2 * time.Second):
			t.Fatalf("waiter %d never acquired a free slot (lost wakeup)", i+1)
		}
	}
}

// warnChanLogger forwards Warn lines to a buffered channel, so a test can wait
// for one the acquiring goroutine emits instead of racing it on a slice.
type warnChanLogger struct{ warns chan []any }

func newWarnChanLogger() *warnChanLogger { return &warnChanLogger{warns: make(chan []any, 8)} }

func (l *warnChanLogger) Debug(string, ...any) {}
func (l *warnChanLogger) Info(string, ...any)  {}
func (l *warnChanLogger) Error(string, ...any) {}
func (l *warnChanLogger) Warn(msg string, args ...any) {
	select {
	case l.warns <- append([]any{msg}, args...):
	default:
	}
}

// --- structural pin -------------------------------------------------------

// TestLifecycleSlotTakenOnceAfterShouldDownload pins WHERE the only take site
// sits. processJob needs a DB, a probe and a live stream to drive, so the shape
// is read from the syntax tree — the package's technique for undrivable sites
// (twitch_endverdict_test.go, stream_processor_early_chat_test.go).
//
// The ruling, clause by clause: exactly ONE AcquireLifecycleSlot call in the
// package's non-test sources; it is inside processJob; it comes AFTER the
// `if !result.ShouldDownload` decline block (a declined job never touches the
// count) and BEFORE acquireDownloadSlot (the slot covers the download and
// nothing else takes it first); and processJob's deferred queue.Complete — the
// release that every exit path after the take runs through — is registered
// before it.
//
// Mutants:
//   - moving the take above the decline block: the "before the decline" arm
//     fires, and a declined job is back to holding a slot it never uses.
//   - moving it below acquireDownloadSlot: the ordering arm fires.
//   - adding a second take site anywhere in the package: the count arm fires,
//     so no second claim can appear without a decision about its release.
//   - deleting the deferred Complete, or registering it after the take: the
//     defer arm fires — an early error would leak the slot for the process's
//     lifetime.
func TestLifecycleSlotTakenOnceAfterShouldDownload(t *testing.T) {
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob the package sources: %v", err)
	}
	fset := token.NewFileSet()
	var takes []token.Pos
	var takeFiles []string
	var processJob *ast.FuncDecl
	processJobFile := ""
	for _, name := range sources {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, pos := range methodCallPositions(parsed, "AcquireLifecycleSlot") {
			takes = append(takes, pos)
			takeFiles = append(takeFiles, name)
		}
		for _, decl := range parsed.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "processJob" {
				processJob, processJobFile = fn, name
			}
		}
	}
	if processJob == nil {
		t.Fatal("no processJob declaration in the package")
	}
	if len(takes) != 1 {
		t.Fatalf("AcquireLifecycleSlot call sites = %d %v, want exactly 1 — the lifecycle slot "+
			"has one take site and one release path", len(takes), takeFiles)
	}
	take := takes[0]
	if takeFiles[0] != processJobFile || take < processJob.Pos() || take > processJob.End() {
		t.Fatalf("the AcquireLifecycleSlot call is at %s, outside processJob (%s)",
			fset.Position(take), fset.Position(processJob.Pos()))
	}

	declineEnd := declineBlockEnd(processJob)
	if declineEnd == token.NoPos {
		t.Fatal("processJob no longer has an `if !result.ShouldDownload` block — the decision the " +
			"slot hangs off is gone")
	}
	if take < declineEnd {
		t.Errorf("AcquireLifecycleSlot at line %d runs before the !result.ShouldDownload block ends "+
			"(line %d): a job stream processing declines would hold a slot it never uses",
			fset.Position(take).Line, fset.Position(declineEnd).Line)
	}

	dl := methodCallPositions(processJob, "acquireDownloadSlot")
	if len(dl) != 1 {
		t.Fatalf("acquireDownloadSlot call sites inside processJob = %d, want 1", len(dl))
	}
	if take > dl[0] {
		t.Errorf("AcquireLifecycleSlot at line %d runs after acquireDownloadSlot (line %d): the "+
			"lifecycle slot must be claimed before any download work starts",
			fset.Position(take).Line, fset.Position(dl[0]).Line)
	}

	completeDefer := deferredCallPos(processJob, "Complete")
	if completeDefer == token.NoPos {
		t.Fatal("processJob no longer defers queue.Complete — the release every exit path after " +
			"the take depends on")
	}
	if completeDefer > take {
		t.Errorf("processJob defers Complete at line %d, AFTER the take at line %d: an error "+
			"between the two leaks the slot",
			fset.Position(completeDefer).Line, fset.Position(take).Line)
	}
}

// methodCallPositions returns the position of every call to a method named
// method inside n, whatever the receiver expression. The package's existing
// selectorCallPos matches only an identifier receiver, and w.queue.Acquire…
// is a selector on a selector.
func methodCallPositions(n ast.Node, method string) []token.Pos {
	var out []token.Pos
	ast.Inspect(n, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == method {
			out = append(out, call.Pos())
		}
		return true
	})
	return out
}

// declineBlockEnd returns the end position of the `if !<expr>.ShouldDownload`
// block inside n, or token.NoPos when there is none.
func declineBlockEnd(n ast.Node) token.Pos {
	end := token.NoPos
	ast.Inspect(n, func(node ast.Node) bool {
		if end != token.NoPos {
			return false
		}
		ifStmt, ok := node.(*ast.IfStmt)
		if !ok {
			return true
		}
		unary, ok := ifStmt.Cond.(*ast.UnaryExpr)
		if !ok || unary.Op != token.NOT {
			return true
		}
		if sel, ok := unary.X.(*ast.SelectorExpr); ok && sel.Sel.Name == "ShouldDownload" {
			end = ifStmt.End()
			return false
		}
		return true
	})
	return end
}

// deferredCallPos returns the position of the first deferred statement inside n
// whose body calls a method named method, or token.NoPos.
func deferredCallPos(n ast.Node, method string) token.Pos {
	found := token.NoPos
	ast.Inspect(n, func(node ast.Node) bool {
		if found != token.NoPos {
			return false
		}
		def, ok := node.(*ast.DeferStmt)
		if !ok {
			return true
		}
		if len(methodCallPositions(def, method)) > 0 {
			found = def.Pos()
			return false
		}
		return true
	})
	return found
}
