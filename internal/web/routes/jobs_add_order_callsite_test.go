package routes

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"testing"
)

// TestAddedIsQueuedBeforeTheWorkerHasTheJob pins d60ed14 on the Web add
// route: "Video Added" is sent BEFORE the worker is handed the job, in both
// the Twitch and the YouTube branch. Each target delivers in queue order, and
// the worker can queue its first event the moment it has the job; an
// edit-mode target's "Added" queued behind that event edits the message the
// event created back to "Added" until the next one, possibly hours later.
//
// Structural, because the route takes a concrete *worker.DownloadWorker and
// nothing about it is observable at the hand-off: the job sits in an
// unexported queue, and a started worker would race the assertion rather than
// decide it. cmd/moombox holds the monitors' half
// (TestNewJobsAreAnnouncedBeforeTheWorkerHasThem).
//
// Mutants: the Twitch branch's notifier.Send(notifications.JobAdded(...))
// moved below its w.EnqueueJob; the YouTube branch's w.EnqueueJob moved above
// its facts block — either puts an EnqueueJob ahead of its JobAdded.
func TestAddedIsQueuedBeforeTheWorkerHasTheJob(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "jobs.go", nil, 0)
	if err != nil {
		t.Fatalf("parse jobs.go: %v", err)
	}
	handler := routeHandlerLit(t, file, "/api/jobs")

	type step struct {
		pos   token.Pos
		added bool // a JobAdded send; otherwise an EnqueueJob
	}
	var steps []step
	ast.Inspect(handler, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "EnqueueJob":
			steps = append(steps, step{call.Pos(), false})
		case "Send":
			if len(call.Args) != 1 {
				return true
			}
			if inner, ok := call.Args[0].(*ast.CallExpr); ok {
				if is, ok := inner.Fun.(*ast.SelectorExpr); ok && is.Sel.Name == "JobAdded" {
					steps = append(steps, step{call.Pos(), true})
				}
			}
		}
		return true
	})
	slices.SortFunc(steps, func(a, b step) int { return int(a.pos - b.pos) })

	enqueues := 0
	for i, s := range steps {
		if s.added {
			continue
		}
		enqueues++
		if i == 0 || !steps[i-1].added {
			t.Errorf("POST /api/jobs hands the worker the job at %s before queueing its \"Video Added\" — "+
				"an edit-mode target's message is edited back to Added", fset.Position(s.pos))
		}
	}
	if enqueues != 2 {
		t.Errorf("POST /api/jobs calls EnqueueJob %d times, want 2 (the Twitch and the YouTube branch) — re-anchor this test", enqueues)
	}
}
