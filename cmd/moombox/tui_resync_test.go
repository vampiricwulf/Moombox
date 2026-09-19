package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sync/atomic"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
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

// The replay is ONE-SHOT, not a poll: a streak of N drops arms the flag once
// (which is also why the warning is logged once per streak, not N times) and
// the events that follow satisfy it with a single full snapshot. The ~60 Hz
// progress pipeline is untouched by this — a drop stays a drop and still
// increments the counter; only the catching-up is added.
//
// Mutant: re-arming (or never clearing) the flag after a delivered snapshot —
// every later event re-reads the whole jobs table and pushes it again, which
// is the periodic poll this must not become.
func TestNDroppedUpdatesProduceExactlyOneRefresh(t *testing.T) {
	var needed atomic.Bool
	jobsCh := make(chan []*database.Job, 4)
	fetches := 0
	resync := newTUIResync(&needed, jobsCh, func() ([]*database.Job, error) {
		fetches++
		return []*database.Job{{ID: "a"}}, nil
	})

	// Three drops in a row: the arming CompareAndSwap the forwarders run
	// transitions once.
	armed := 0
	for i := 0; i < 3; i++ {
		if needed.CompareAndSwap(false, true) {
			armed++
		}
	}
	if armed != 1 {
		t.Errorf("three drops armed the resync %d times, want 1 (one log line per streak)", armed)
	}

	// Three later events all run the replay.
	for i := 0; i < 3; i++ {
		resync()
	}
	if got := len(jobsCh); got != 1 {
		t.Errorf("three drops then three events pushed %d snapshots, want exactly 1", got)
	}
	if fetches != 1 {
		t.Errorf("GetAllJobs ran %d times, want 1 — the replay is one-shot, not a poll", fetches)
	}
}

// resyncForwarders are the five DB subscriptions runTUI forwards to the TUI.
// The four that count a drop must also arm the replay; the fifth
// (OnJobsChange) already carries a full list, so it only has to satisfy a
// pending replay on the way in.
var resyncForwarders = map[string]bool{ // name -> counts a drop
	"OnJobChange":    true,
	"OnJobAdded":     true,
	"OnJobDeleted":   true,
	"OnTrimsChanged": true,
	"OnJobsChange":   false,
}

// TestEveryTUIJobForwarderRunsAndArmsTheResync pins the wiring the unit tests
// above cannot reach: runTUI builds the whole TUI and runs the bubbletea
// program, so this package cannot drive it and the seam is the shape of each
// forwarder — structural for the reason the FFmpeg callsite test beside it is.
//
// Mutants: delete the resyncTUIJobs() call from one forwarder (a drop on that
// channel is never replayed); delete the CompareAndSwap arm (the counter
// still counts and nothing catches up, which is exactly CORE-6); send a
// snapshot from inside a drop branch (one refresh per drop — the poll this
// must not become).
func TestEveryTUIJobForwarderRunsAndArmsTheResync(t *testing.T) {
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

		if !callsResync(lit.Body.List[0]) {
			t.Errorf("the s.db.%s forwarder does not run resyncTUIJobs() first — a drop on its "+
				"channel is never replayed and the row stays stale for the session", sel.Sel.Name)
		}
		if !counts {
			return true
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

// callsResync reports whether stmt is a bare resyncTUIJobs() call.
func callsResync(stmt ast.Stmt) bool {
	expr, ok := stmt.(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := expr.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	ident, ok := call.Fun.(*ast.Ident)
	return ok && ident.Name == "resyncTUIJobs"
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
