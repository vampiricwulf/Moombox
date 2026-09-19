package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/logger"
)

// A TUI job update dropped on a full channel must be replayed: the flag is
// set on the drop and the next forwarded event (or the 1 s backstop) pushes a
// full GetAllJobs snapshot down jobsUpdateCh. Without it a dropped
// Downloading->Finished leaves a stale row for the rest of the session, and
// user-interfaces.md documents a recovery that did not exist (CORE-6).
//
// Mutant: making newTUIResync a no-op when the flag is set (or dropping the
// CompareAndSwap so it never fires) — no snapshot arrives.
func TestDroppedJobUpdateSchedulesAResync(t *testing.T) {
	var needed atomic.Bool
	jobsCh := make(chan []*database.Job, 1)
	snapshot := []*database.Job{{ID: "a"}, {ID: "b"}}

	resync := newTUIResync(&needed, jobsCh, func() ([]*database.Job, error) {
		return snapshot, nil
	})

	// Nothing dropped yet: the resync must be free.
	resync()
	select {
	case got := <-jobsCh:
		t.Fatalf("no drop happened; nothing must be pushed, got %d jobs", len(got))
	default:
	}

	needed.Store(true)
	resync()
	select {
	case got := <-jobsCh:
		if len(got) != len(snapshot) {
			t.Errorf("resync pushed %d jobs, want %d", len(got), len(snapshot))
		}
	default:
		t.Fatal("a pending drop must push a full snapshot")
	}
	if needed.Load() {
		t.Error("a delivered snapshot must clear the pending flag")
	}
}

// A snapshot that cannot be fetched or cannot be delivered must leave the
// flag ARMED so the next event tries again — silently clearing it would turn
// one dropped update into a permanently stale row.
//
// Mutant: clearing the flag unconditionally instead of re-arming on failure.
func TestResyncStaysArmedWhenItCannotDeliver(t *testing.T) {
	var needed atomic.Bool
	full := make(chan []*database.Job) // unbuffered, nobody reading
	resync := newTUIResync(&needed, full, func() ([]*database.Job, error) {
		return []*database.Job{{ID: "a"}}, nil
	})

	needed.Store(true)
	resync()
	if !needed.Load() {
		t.Error("an undeliverable snapshot must leave the resync armed")
	}

	var needed2 atomic.Bool
	ok := make(chan []*database.Job, 1)
	failing := newTUIResync(&needed2, ok, func() ([]*database.Job, error) {
		return nil, errResyncTest
	})
	needed2.Store(true)
	failing()
	if !needed2.Load() {
		t.Error("a failed GetAllJobs must leave the resync armed")
	}
}

// resyncRig is the production wiring of one TUI job forwarder: a real
// database (so the fetch is the real GetAllJobs), a real logger (so the
// warning count is the real one), the production channel shapes — and
// forwardOrDrop itself as the body. Nothing here re-implements the
// forwarder; a mutation of the production helper fails these tests.
type resyncRig struct {
	db          *database.Database
	log         *logger.Logger
	lines       chan string
	jobUpdateCh chan *database.JobChange
	jobsCh      chan []*database.Job
	dropped     atomic.Int64
	needed      atomic.Bool
	fetches     int
}

func newResyncRig(t *testing.T) *resyncRig {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.AddJob(&database.Job{
		ID: "a", VideoID: "a", URL: "https://example.invalid/a",
		Platform: "youtube", Status: database.StatusDownloading,
	}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	log, err := logger.New(filepath.Join(t.TempDir(), "resync.log"), "info", 4096, 1)
	if err != nil {
		t.Fatalf("logger.New: %v", err)
	}
	t.Cleanup(func() { log.Close() })

	return &resyncRig{
		db:          db,
		log:         log,
		lines:       log.Subscribe(),
		jobUpdateCh: make(chan *database.JobChange, 1),
		jobsCh:      make(chan []*database.Job, 10),
	}
}

// resync is the rig's counting wrapper around the production replay.
func (r *resyncRig) resync() func() {
	return newTUIResync(&r.needed, r.jobsCh, func() ([]*database.Job, error) {
		r.fetches++
		return r.db.GetAllJobs()
	})
}

// forward runs the PRODUCTION forwarder body on one event.
func (r *resyncRig) forward(resync func(), ev *database.JobChange) {
	forwardOrDrop(r.jobUpdateCh, ev, ev.Job.ID, resync, &r.dropped, &r.needed, r.log)
}

func (r *resyncRig) warnCount() int {
	n := 0
	for {
		select {
		case line := <-r.lines:
			if strings.Contains(line, "TUI job update dropped") {
				n++
			}
		default:
			return n
		}
	}
}

// The replay is ONE-SHOT and it runs on the SUCCESSFUL send: a streak of K
// drops arms the flag once and costs nothing else, and the first event that
// gets through satisfies it with a single full snapshot carrying the
// transition the streak swallowed.
//
// Measured end to end against forwardOrDrop, the production body. This is the
// interleaving the earlier version of this test failed to model: drops and
// forwards ALTERNATE, so a replay at the top of the body is taken by the very
// next dropped event. Two streak lengths because "one refresh per streak" and
// "one refresh" are the same number at K=1 and must not be at K=20.
//
// Mutant: move resync() back to the top of forwardOrDrop — the streak then
// runs K full-table reads on the ~60 Hz UpdateJobFields writer goroutine
// (3.15 ms each over 500 rows), queues K full-list rebuilds into the TUI it
// is already failing to keep up with, and logs K warnings. Both the
// during-streak assertion and the "want 1" assertions fail.
// Mutant: hoist the Warn out of the CompareAndSwap — warnings = drops.
func TestNDroppedUpdatesProduceExactlyOneRefresh(t *testing.T) {
	for _, K := range []int{6, 20} {
		t.Run(fmt.Sprintf("K=%d", K), func(t *testing.T) {
			r := newResyncRig(t)
			resync := r.resync()

			// Fill the channel: the TUI's Update loop is stalled, so every
			// send below drops.
			r.jobUpdateCh <- &database.JobChange{Job: &database.Job{ID: "filler"}}

			for range K {
				r.forward(resync, &database.JobChange{
					Job:     &database.Job{ID: "a", Status: database.StatusDownloading},
					Changes: []string{"status"},
				})
			}
			// The transition the streak swallowed.
			if r.db.UpdateJobFields("a", map[string]any{"status": database.StatusFinished}) == nil {
				t.Fatal("UpdateJobFields returned no row")
			}

			if got := r.dropped.Load(); got != int64(K) {
				t.Errorf("K=%d events on a full channel dropped %d, want %d", K, got, K)
			}
			if r.fetches != 0 {
				t.Errorf("the writer goroutine ran GetAllJobs %d times while the channel was full, want 0 — "+
					"the replay belongs on the successful send, not on every dropped event", r.fetches)
			}
			if got := len(r.jobsCh); got != 0 {
				t.Errorf("%d full-list snapshots were queued mid-streak, want 0", got)
			}

			// The TUI drains one slot and the next event gets through.
			<-r.jobUpdateCh
			r.forward(resync, &database.JobChange{Job: &database.Job{ID: "a"}, Changes: []string{"status"}})

			if r.fetches != 1 {
				t.Errorf("K=%d drops then one successful send ran %d refreshes, want exactly 1", K, r.fetches)
			}
			if got := len(r.jobsCh); got != 1 {
				t.Errorf("K=%d drops then one successful send queued %d snapshots, want exactly 1", K, got)
			}
			var status database.JobStatus
			select {
			case snap := <-r.jobsCh:
				for _, j := range snap {
					if j.ID == "a" {
						status = j.Status
					}
				}
			default:
				t.Error("the successful send did not deliver the pending snapshot")
			}
			if status != database.StatusFinished {
				t.Errorf("the replayed snapshot carries status %q, want %q — the whole point is that the "+
					"dropped transition is recovered", status, database.StatusFinished)
			}
			if r.needed.Load() {
				t.Error("a delivered snapshot must clear the pending flag")
			}

			warns := r.warnCount()
			if warns != 1 {
				t.Errorf("a streak of %d drops logged %d warnings, want 1 — one line per streak, not per "+
					"dropped message (a stall would otherwise flood the 200-slot log channel)", K, warns)
			}
			t.Logf("K=%d drops then one successful send: refreshes=%d warnings=%d snapshots=1 status=%q",
				K, r.fetches, warns, status)
		})
	}
}

// Every event the TUI has room for is DELIVERED. The replay exists for the
// messages a full channel swallowed; it is not licence to collapse the ~60 Hz
// stream into fewer, later frames (the protected "cheaper, never rarer"
// ruling). N sends into a channel with N slots must yield N events, in order.
//
// Mutant: a coalescing check ahead of forwardOrDrop's select — "skip this
// event if one for the same job is already queued" — delivers fewer than N.
func TestEverySendWithRoomIsDelivered(t *testing.T) {
	r := newResyncRig(t)
	resync := r.resync()

	const N = 32
	wide := make(chan *database.JobChange, N)
	for i := range N {
		forwardOrDrop(wide, &database.JobChange{
			Job:     &database.Job{ID: "a", Progress: fmt.Sprintf("V:%d A:%d", i, i)},
			Changes: []string{"progress"},
		}, "a", resync, &r.dropped, &r.needed, r.log)
	}

	if got := len(wide); got != N {
		t.Fatalf("%d of %d events reached a channel with room — the forwarder must not coalesce", got, N)
	}
	for i := range N {
		ev := <-wide
		if want := fmt.Sprintf("V:%d A:%d", i, i); ev.Job.Progress != want {
			t.Fatalf("event %d carries %q, want %q — delivery must stay in order", i, ev.Job.Progress, want)
		}
	}
	if got := r.dropped.Load(); got != 0 {
		t.Errorf("%d events were counted as dropped on a channel with room", got)
	}
	if r.fetches != 0 {
		t.Errorf("%d replays ran with nothing pending, want 0 — a successful send costs one failed CAS", r.fetches)
	}
}

// A bulk list delivered by OnJobsChange must NOT clear a pending replay. That
// list is a time-of-write snapshot taken inside BatchSetWatched /
// DeleteJobsAndHistoryForChannel; an UpdateJobFields that lands after it and
// is then dropped by a full jobUpdateCh is not in it. Clearing the flag on
// delivery discarded that replay, and the dropped transition (a
// Downloading → Finished, say) was never recovered — CORE-6's own bug shape
// in a narrow window (B-1).
//
// Mutant: restore tuiResyncNeeded.Store(false) in the OnJobsChange success
// branch (spelled here as the same Store on the rig's flag) — the pending
// replay is dropped and the following successful send fetches nothing.
func TestADeliveredBulkListDoesNotClearAPendingReplay(t *testing.T) {
	r := newResyncRig(t)
	resync := r.resync()

	// A drop arms the replay: the channel is full and one event is swallowed.
	r.jobUpdateCh <- &database.JobChange{Job: &database.Job{ID: "filler"}}
	r.forward(resync, &database.JobChange{
		Job:     &database.Job{ID: "a", Status: database.StatusDownloading},
		Changes: []string{"status"},
	})
	if !r.needed.Load() {
		t.Fatal("a dropped event must arm the replay")
	}
	// The transition the drop swallowed, then a bulk write's list lands —
	// the production OnJobsChange shape: a bare non-blocking send.
	if r.db.UpdateJobFields("a", map[string]any{"status": database.StatusFinished}) == nil {
		t.Fatal("UpdateJobFields returned no row")
	}
	stale := []*database.Job{{ID: "a", Status: database.StatusDownloading}}
	select {
	case r.jobsCh <- stale:
	default:
		t.Fatal("the bulk list must fit")
	}
	if !r.needed.Load() {
		t.Fatal("a delivered bulk list must leave a pending replay armed — it is a time-of-write " +
			"snapshot and may predate the drop that armed it")
	}

	// The next successful send does the catch-up the drop needs.
	<-r.jobUpdateCh
	r.forward(resync, &database.JobChange{Job: &database.Job{ID: "a"}, Changes: []string{"status"}})
	if r.fetches != 1 {
		t.Fatalf("after a delivered bulk list the pending replay ran %d times, want 1", r.fetches)
	}
	<-r.jobsCh // the stale bulk list
	select {
	case snap := <-r.jobsCh:
		for _, j := range snap {
			if j.ID == "a" && j.Status != database.StatusFinished {
				t.Errorf("the replayed snapshot carries status %q, want %q", j.Status, database.StatusFinished)
			}
		}
	default:
		t.Error("the replay did not push a snapshot")
	}
}

// resyncForwarders are the five DB subscriptions runTUI forwards to the TUI.
// The four that count a drop must delegate to forwardOrDrop — the body the
// tests above measure — passing resyncTUIJobs as the replay. The fifth
// (OnJobsChange) already carries a full list, so it neither replays nor
// CLEARS a pending replay: its body is a bare non-blocking send with an empty
// default.
var resyncForwarders = map[string]bool{ // name -> counts a drop
	"OnJobChange":    true,
	"OnJobAdded":     true,
	"OnJobDeleted":   true,
	"OnTrimsChanged": true,
	"OnJobsChange":   false,
}

// TestEveryTUIJobForwarderReplaysOnTheSuccessfulSend pins the wiring the unit
// tests above cannot reach: runTUI builds the whole TUI and runs the
// bubbletea program, so this package cannot drive it and the seam is the
// shape of each forwarder — structural for the reason the FFmpeg callsite
// test beside it is. It is also what makes the measured forwarder in
// TestNDroppedUpdatesProduceExactlyOneRefresh a faithful copy.
//
// Mutants: inline a hand-copy of forwardOrDrop into one forwarder (the
// measured tests above then bind a copy again, and the copy is free to
// drift); pass a different replay (or nil) instead of resyncTUIJobs (a drop
// on that channel is never replayed, which is exactly CORE-6); restore
// tuiResyncNeeded.Store(false) to the OnJobsChange success branch (a
// delivered snapshot discards a replay armed after it — B-1); give
// OnJobsChange a non-empty default (its own drop is harmless, a snapshot
// queued when a snapshot cannot be queued).
func TestEveryTUIJobForwarderReplaysOnTheSuccessfulSend(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "tui_wiring.go", nil, 0)
	if err != nil {
		t.Fatalf("parse tui_wiring.go: %v", err)
	}

	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "runTUI" && fn.Body != nil {
			body = fn.Body
		}
	}
	if body == nil {
		t.Fatal("tui_wiring.go has no runTUI with a body — re-anchor this test rather than deleting it")
	}

	seen := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !selectorIs(sel.X, "s", "db") {
			return true
		}
		counts, watched := resyncForwarders[sel.Sel.Name]
		if !watched {
			return true
		}
		lit, ok := call.Args[0].(*ast.FuncLit)
		if !ok || lit.Body == nil || len(lit.Body.List) == 0 {
			t.Errorf("s.db.%s is not subscribed with a function literal body", sel.Sel.Name)
			return true
		}
		seen[sel.Sel.Name] = true

		if counts {
			// The four that can drop delegate to the one measured body.
			args := soleCallArgs(lit.Body, "forwardOrDrop")
			if args == nil {
				t.Errorf("the s.db.%s forwarder does not call forwardOrDrop exactly once — a "+
					"hand-copied select is free to drift from the body tui_resync_test.go measures "+
					"(the warning outside its CAS, a coalescing check ahead of the select)",
					sel.Sel.Name)
				return true
			}
			if !hasIdentArg(args, "resyncTUIJobs") {
				t.Errorf("the s.db.%s forwarder does not pass resyncTUIJobs as its replay — a drop "+
					"on that channel is never caught up (CORE-6)", sel.Sel.Name)
			}
			if hasSend(lit.Body) {
				t.Errorf("the s.db.%s forwarder sends on a channel itself instead of through "+
					"forwardOrDrop", sel.Sel.Name)
			}
			return true
		}

		// OnJobsChange: a bare non-blocking send. It must not touch the flag
		// at all — its list is a time-of-write snapshot that may predate the
		// drop which armed a pending replay (B-1).
		sent := successClause(lit.Body)
		if sent == nil {
			t.Errorf("the s.db.%s forwarder has no select clause that sends the event", sel.Sel.Name)
			return true
		}
		if hasCallTo(lit.Body, "tuiResyncNeeded", "Store") {
			t.Errorf("the s.db.%s forwarder writes tuiResyncNeeded — a DELIVERED list is not newer "+
				"than a pending replay (it is the bulk write's own time-of-write snapshot), so "+
				"clearing the flag there throws the replay away", sel.Sel.Name)
		}
		if soleCallArgs(lit.Body, "forwardOrDrop") != nil {
			t.Errorf("the s.db.%s forwarder calls forwardOrDrop — its own drop is harmless (a "+
				"snapshot is queued when a snapshot cannot be queued) and it counts no drop",
				sel.Sel.Name)
		}
		def := defaultClause(lit.Body)
		if def == nil {
			t.Errorf("the s.db.%s forwarder has no select default — the send must never block the "+
				"writer goroutine", sel.Sel.Name)
			return true
		}
		if len(def.Body) != 0 {
			t.Errorf("the s.db.%s forwarder's default branch is not empty (%d statements)",
				sel.Sel.Name, len(def.Body))
		}
		return true
	})

	for name := range resyncForwarders {
		if !seen[name] {
			t.Errorf("runTUI no longer subscribes s.db.%s — re-anchor this test rather than deleting it", name)
		}
	}
}

// soleCallArgs returns the arguments of the ONE call to the plain function
// name under n, or nil when there is not exactly one.
func soleCallArgs(n ast.Node, name string) []ast.Expr {
	var args []ast.Expr
	count := 0
	ast.Inspect(n, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == name {
			count++
			args = call.Args
		}
		return true
	})
	if count != 1 {
		return nil
	}
	return args
}

// hasIdentArg reports whether one of args is exactly the identifier name.
func hasIdentArg(args []ast.Expr, name string) bool {
	for _, a := range args {
		if ident, ok := a.(*ast.Ident); ok && ident.Name == name {
			return true
		}
	}
	return false
}

// successClause returns the select clause that SENDS — the branch taken when
// the TUI channel had room.
func successClause(body *ast.BlockStmt) *ast.CommClause {
	var found *ast.CommClause
	ast.Inspect(body, func(n ast.Node) bool {
		clause, ok := n.(*ast.CommClause)
		if !ok || found != nil {
			return true
		}
		if _, isSend := clause.Comm.(*ast.SendStmt); isSend {
			found = clause
		}
		return true
	})
	return found
}

// defaultClause returns the first select-default clause under body.
func defaultClause(body *ast.BlockStmt) *ast.CommClause {
	var found *ast.CommClause
	ast.Inspect(body, func(n ast.Node) bool {
		clause, ok := n.(*ast.CommClause)
		if !ok || clause.Comm != nil || found != nil {
			return true
		}
		found = clause
		return true
	})
	return found
}

func hasCallTo(n ast.Node, recv, sel string) bool {
	found := false
	ast.Inspect(n, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if ok && selectorIs(call.Fun, recv, sel) {
			found = true
		}
		return true
	})
	return found
}

func hasSend(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(node ast.Node) bool {
		if _, ok := node.(*ast.SendStmt); ok {
			found = true
		}
		return true
	})
	return found
}

var errResyncTest = errTest("boom")

type errTest string

func (e errTest) Error() string { return string(e) }
