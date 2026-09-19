package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
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

// The replay is ONE-SHOT and it runs on the SUCCESSFUL send: a streak of K
// drops arms the flag once and costs nothing else, and the first event that
// gets through satisfies it with a single full snapshot carrying the
// transition the streak swallowed.
//
// Measured end to end — a real database (so the fetch is the real
// GetAllJobs), a real logger (so the warning count is the real one), the
// production channel shapes, and the forwarder body in its production form.
// The forwarder here is a copy; that the copy is faithful is what
// TestEveryTUIJobForwarderReplaysOnTheSuccessfulSend below pins, and the two
// together are the pin. This is the interleaving the earlier version of this
// test failed to model: drops and forwards ALTERNATE, so a replay at the top
// of the body is taken by the very next dropped event.
//
// Mutant: move resync() back to the top of the forwarder body — the streak
// then runs K full-table reads on the ~60 Hz UpdateJobFields writer goroutine
// (3.15 ms each over 500 rows), queues K full-list rebuilds into the TUI it
// is already failing to keep up with, and logs K warnings. Both the
// during-streak assertion and the "want 1" assertions fail.
func TestNDroppedUpdatesProduceExactlyOneRefresh(t *testing.T) {
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
	lines := log.Subscribe()

	// Production shapes: the job-change channel is the one that fills, the
	// full-list channel is the one the replay pushes down.
	jobUpdateCh := make(chan *database.JobChange, 1)
	jobsUpdateCh := make(chan []*database.Job, 10)
	var dropped atomic.Int64
	var needed atomic.Bool
	fetches := 0
	resync := newTUIResync(&needed, jobsUpdateCh, func() ([]*database.Job, error) {
		fetches++
		return db.GetAllJobs()
	})
	forward := func(ev *database.JobChange) {
		select {
		case jobUpdateCh <- ev:
			resync()
		default:
			dropped.Add(1)
			if needed.CompareAndSwap(false, true) {
				log.Warn("TUI job update dropped — a full refresh is queued",
					slog.String("job", ev.Job.ID))
			}
		}
	}

	// Fill the channel: the TUI's Update loop is stalled, so every send below
	// drops.
	jobUpdateCh <- &database.JobChange{Job: &database.Job{ID: "filler"}}

	const K = 6
	for i := 0; i < K; i++ {
		forward(&database.JobChange{
			Job:     &database.Job{ID: "a", Status: database.StatusDownloading},
			Changes: []string{"status"},
		})
	}
	// The transition the streak swallowed.
	if db.UpdateJobFields("a", map[string]any{"status": database.StatusFinished}) == nil {
		t.Fatal("UpdateJobFields returned no row")
	}

	if got := dropped.Load(); got != K {
		t.Errorf("K=%d events on a full channel dropped %d, want %d", K, got, K)
	}
	if fetches != 0 {
		t.Errorf("the writer goroutine ran GetAllJobs %d times while the channel was full, want 0 — "+
			"the replay belongs on the successful send, not on every dropped event", fetches)
	}
	if got := len(jobsUpdateCh); got != 0 {
		t.Errorf("%d full-list snapshots were queued mid-streak, want 0", got)
	}

	// The TUI drains one slot and the next event gets through.
	<-jobUpdateCh
	forward(&database.JobChange{Job: &database.Job{ID: "a"}, Changes: []string{"status"}})

	if fetches != 1 {
		t.Errorf("K=%d drops then one successful send ran %d refreshes, want exactly 1", K, fetches)
	}
	if got := len(jobsUpdateCh); got != 1 {
		t.Errorf("K=%d drops then one successful send queued %d snapshots, want exactly 1", K, got)
	}
	var status database.JobStatus
	select {
	case snap := <-jobsUpdateCh:
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
	if needed.Load() {
		t.Error("a delivered snapshot must clear the pending flag")
	}

	warns := 0
	for draining := true; draining; {
		select {
		case line := <-lines:
			if strings.Contains(line, "TUI job update dropped") {
				warns++
			}
		default:
			draining = false
		}
	}
	if warns != 1 {
		t.Errorf("a streak of %d drops logged %d warnings, want 1 — one line per streak, not per "+
			"dropped message (a stall would otherwise flood the 200-slot log channel)", K, warns)
	}
	t.Logf("K=%d drops then one successful send: refreshes=%d warnings=%d snapshots=1 status=%q",
		K, fetches, warns, status)
}

// resyncForwarders are the five DB subscriptions runTUI forwards to the TUI.
// The four that count a drop must arm the replay there and run it from the
// SUCCESSFUL send; the fifth (OnJobsChange) delivers a full list, which IS
// the refresh, so it clears the flag in its own success branch instead of
// fetching a second snapshot.
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
// Mutants: move resyncTUIJobs() back to the top of a forwarder body (the
// replay then fires once per DROPPED event — K full-table reads on the
// writer goroutine); delete the call entirely (a drop on that channel is
// never replayed); delete the CompareAndSwap arm (the counter still counts
// and nothing catches up, which is exactly CORE-6); send a snapshot from
// inside a drop branch (one refresh per drop).
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

		sent := successClause(lit.Body)
		if sent == nil {
			t.Errorf("the s.db.%s forwarder has no select clause that sends the event", sel.Sel.Name)
			return true
		}
		if !counts {
			// OnJobsChange: its own delivered list satisfies a pending
			// replay, so it clears the flag rather than fetching again.
			if countResyncCalls(lit.Body) != 0 {
				t.Errorf("the s.db.%s forwarder calls resyncTUIJobs — the full list it just "+
					"delivered IS the refresh; a second snapshot into the same channel is waste",
					sel.Sel.Name)
			}
			if !hasCallTo(sent, "tuiResyncNeeded", "Store") {
				t.Errorf("the s.db.%s forwarder does not clear tuiResyncNeeded on a delivered "+
					"list — the next event would fetch a snapshot older than the one just sent",
					sel.Sel.Name)
			}
			return true
		}
		if n := countResyncCalls(lit.Body); n != 1 {
			t.Errorf("the s.db.%s forwarder calls resyncTUIJobs %d times, want exactly 1 (in the "+
				"successful-send branch)", sel.Sel.Name, n)
		}
		if countResyncCalls(sent) != 1 {
			t.Errorf("the s.db.%s forwarder does not run resyncTUIJobs() from its successful-send "+
				"branch — at the top of the body the replay fires once per DROPPED event, running a "+
				"full GetAllJobs on the ~60 Hz writer goroutine for every message the stalled TUI "+
				"could not take", sel.Sel.Name)
		}
		drop := dropBranch(lit.Body)
		if drop == nil {
			t.Errorf("the s.db.%s forwarder has no select default branch that counts a drop", sel.Sel.Name)
			return true
		}
		if !hasCallTo(drop, "tuiResyncNeeded", "CompareAndSwap") {
			t.Errorf("the s.db.%s forwarder counts a drop without arming tuiResyncNeeded — the "+
				"counter is read once at TUI exit and nothing catches the list up (CORE-6)", sel.Sel.Name)
		}
		if hasSend(drop) {
			t.Errorf("the s.db.%s forwarder sends from inside its drop branch — the replay is "+
				"one-shot per streak, not one refresh per dropped message", sel.Sel.Name)
		}
		return true
	})

	for name := range resyncForwarders {
		if !seen[name] {
			t.Errorf("runTUI no longer subscribes s.db.%s — re-anchor this test rather than deleting it", name)
		}
	}
}

// countResyncCalls counts bare resyncTUIJobs() calls under n.
func countResyncCalls(n ast.Node) int {
	count := 0
	ast.Inspect(n, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "resyncTUIJobs" {
			count++
		}
		return true
	})
	return count
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

// dropBranch returns the first select-default clause under body that counts a
// dropped job message.
func dropBranch(body *ast.BlockStmt) *ast.CommClause {
	var found *ast.CommClause
	ast.Inspect(body, func(n ast.Node) bool {
		clause, ok := n.(*ast.CommClause)
		if !ok || clause.Comm != nil || found != nil {
			return true
		}
		if hasCallTo(clause, "tuiDroppedJobs", "Add") {
			found = clause
		}
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
