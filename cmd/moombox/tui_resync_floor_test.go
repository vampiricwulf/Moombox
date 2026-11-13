package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// The floor a snapshot is sent with is read BEFORE the list. A write that
// lands between the two reads is then above the floor, and its event is
// applied by the TUI as usual. Read after the list, that write would be at or
// below the floor and missing from the list: the TUI would throw its event
// away and never show it (tui-2).
//
// Mutant: reading seen.Load() after getAll() in tuiJobSnapshot.
func TestSnapshotFloorIsReadBeforeTheList(t *testing.T) {
	var seen atomic.Uint64
	seen.Store(5)
	snap, err := tuiJobSnapshot(&seen, func() ([]*database.Job, error) {
		// A write is committed and forwarded while the list is being read.
		noteSeenJobVersion(&seen, 9)
		return []*database.Job{{ID: "a"}}, nil
	})
	if err != nil || len(snap.Jobs) != 1 {
		t.Fatalf("snapshot = %+v, %v", snap, err)
	}
	if snap.Floor != 5 {
		t.Errorf("floor = %d, want 5: the version seen before the list was read", snap.Floor)
	}

	if _, err := tuiJobSnapshot(&seen, func() ([]*database.Job, error) { return nil, errResyncTest }); err == nil {
		t.Error("a failed read must be reported, so the replay stays armed")
	}
}

// The forwarders run on whichever goroutine wrote, so the mark only ever
// rises, whatever order they reach it in.
//
// Mutant: a plain Store in noteSeenJobVersion.
func TestNoteSeenJobVersionKeepsTheHighest(t *testing.T) {
	var seen atomic.Uint64
	for _, v := range []uint64{3, 9, 4, 0, 9, 7} {
		noteSeenJobVersion(&seen, v)
	}
	if got := seen.Load(); got != 9 {
		t.Fatalf("seen = %d, want 9", got)
	}
	var wg sync.WaitGroup
	for v := uint64(1); v <= 64; v++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			noteSeenJobVersion(&seen, v)
		}()
	}
	wg.Wait()
	if got := seen.Load(); got != 64 {
		t.Errorf("seen = %d after concurrent notes up to 64", got)
	}
}

// End to end against a real database and the production replay: after a
// stall, the catch-up snapshot's floor covers every event still queued ahead
// of it AND the write that was dropped, while the snapshot itself holds that
// write.
//
// Mutant: not noting the version of a DROPPED event (noting it only on a
// successful send): the floor stops at the last queued event, which still
// passes the queued-event check but fails the dropped-write one.
func TestReplayedSnapshotFloorCoversTheQueuedBacklog(t *testing.T) {
	r := newResyncRig(t)
	resync := r.resync()
	r.jobUpdateCh = make(chan *database.JobChange, 2)

	var mu sync.Mutex
	var written []uint64
	unsub := r.db.OnJobChange(func(ev *database.JobChange) {
		mu.Lock()
		written = append(written, ev.Job.Version)
		mu.Unlock()
		// The production OnJobChange forwarder, statement for statement
		// (TestJobForwardersNoteTheVersionBeforeTheySend holds it to this).
		noteSeenJobVersion(&r.seen, ev.Job.Version)
		forwardOrDrop(r.jobUpdateCh, ev, ev.Job.ID, resync, &r.dropped, &r.needed, r.log)
	})
	defer unsub()

	// Two ticks fill the channel; the Finished write is dropped.
	for i, fields := range []map[string]any{
		{"progress": "V:1 A:1"},
		{"progress": "V:2 A:2"},
		{"status": database.StatusFinished},
	} {
		if r.db.UpdateJobFields("a", fields) == nil {
			t.Fatalf("write %d returned no row", i)
		}
	}
	if r.dropped.Load() != 1 || !r.needed.Load() {
		t.Fatalf("dropped = %d, armed = %v; want the third write dropped and the replay armed", r.dropped.Load(), r.needed.Load())
	}
	mu.Lock()
	finishedVersion := written[len(written)-1]
	mu.Unlock()
	if finishedVersion == 0 {
		t.Fatal("the database handed out no write version; this test proves nothing")
	}

	// The 1 s backstop (or the next successful send) runs the replay.
	resync()
	var snap = <-r.jobsCh
	if snap.Floor < finishedVersion {
		t.Errorf("floor = %d, below the dropped write's version %d", snap.Floor, finishedVersion)
	}
	for len(r.jobUpdateCh) > 0 {
		if ev := <-r.jobUpdateCh; ev.Job.Version > snap.Floor {
			t.Errorf("a queued event has version %d, above the snapshot's floor %d: the TUI would apply it over the snapshot", ev.Job.Version, snap.Floor)
		}
	}
	for _, j := range snap.Jobs {
		if j.ID == "a" && j.Status != database.StatusFinished {
			t.Errorf("the snapshot carries status %q, want Finished", j.Status)
		}
	}
}

// The two forwarders whose events carry a write version note it as their
// FIRST statement, ahead of the send: a version noted after the send could be
// read by a replay in between, and the event already in the channel would
// then be above the floor. The bulk-list forwarder states no floor, since its
// list is read inside the bulk write and may be older than a write already
// seen. Structural for the reason the test beside it is: runTUI cannot be
// driven from here.
//
// Mutants: moving noteSeenJobVersion below forwardOrDrop in a forwarder;
// giving the OnJobsChange forwarder's message a Floor.
func TestJobForwardersNoteTheVersionBeforeTheySend(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "tui_wiring.go", nil, 0)
	if err != nil {
		t.Fatalf("parse tui_wiring.go: %v", err)
	}
	want := map[string]bool{"OnJobChange": false, "OnJobAdded": false}
	bulkSeen := false
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !selectorIs(sel.X, "s", "db") {
			return true
		}
		lit, ok := call.Args[0].(*ast.FuncLit)
		if !ok || lit.Body == nil || len(lit.Body.List) == 0 {
			return true
		}
		if sel.Sel.Name == "OnJobsChange" {
			bulkSeen = true
			ast.Inspect(lit.Body, func(m ast.Node) bool {
				if kv, ok := m.(*ast.KeyValueExpr); ok {
					if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Floor" {
						t.Error("the s.db.OnJobsChange forwarder states a Floor: its list is the bulk write's own snapshot and may be older than a write already seen")
					}
				}
				return true
			})
			return true
		}
		if _, watched := want[sel.Sel.Name]; !watched {
			return true
		}
		first, ok := lit.Body.List[0].(*ast.ExprStmt)
		if ok {
			if c, ok := first.X.(*ast.CallExpr); ok {
				if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "noteSeenJobVersion" {
					want[sel.Sel.Name] = true
				}
			}
		}
		return true
	})
	for name, ok := range want {
		if !ok {
			t.Errorf("the s.db.%s forwarder does not call noteSeenJobVersion as its first statement", name)
		}
	}
	if !bulkSeen {
		t.Error("runTUI no longer subscribes s.db.OnJobsChange: re-anchor this test rather than deleting it")
	}
}
