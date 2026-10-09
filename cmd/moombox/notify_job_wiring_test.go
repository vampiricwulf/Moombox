package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
	"github.com/vampiricwulf/Moombox/internal/web"
)

// A delete must reach the notifier, or the edit-mode state it holds for the
// job outlives the row: a YouTube job's id is its video id, so a re-add of the
// same video edits the deleted job's message instead of opening its own.
// onJobDeleted is the single delete's path; onJobsChange is the bulk prune's,
// which fires no per-job event.
//
// Mutants: onJobDeleted without its ForgetJob call; onJobsChange without its
// RetainJobs call, or handing it anything but the listed ids.
func TestEveryDeletePathReachesTheNotifier(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	rec := notificationtest.New()
	s := &runState{db: db, wsHub: web.NewWebSocketHub(sweepTestLogger{}), notifyMgr: rec}

	t.Run("a single delete", func(t *testing.T) {
		s.onJobDeleted("doomed")
		if got := rec.Forgotten(); !slices.Equal(got, []string{"doomed"}) {
			t.Errorf("ForgetJob calls = %v, want [doomed]", got)
		}
	})

	t.Run("a bulk delete", func(t *testing.T) {
		s.configStore = config.NewStore(config.Defaults(), "")
		s.onJobsChange([]*database.Job{{ID: "kept"}, {ID: "alsoKept"}})
		got := rec.Retained()
		if len(got) != 1 {
			t.Fatalf("RetainJobs called %d times, want once", len(got))
		}
		if ids := slices.Sorted(maps.Keys(got[0])); !slices.Equal(ids, []string{"alsoKept", "kept"}) {
			t.Errorf("RetainJobs live set = %v, want exactly the listed jobs", ids)
		}
	})
}

// The two subscribers above are only reached if wireMonitorCallbacks
// registers them, and that function is not callable from a test (it wires
// every monitor, the cookie refresh and the log forwarder at once), so the
// registration is asserted on its source.
//
// Mutants: OnJobsChange registered with anything but s.onJobsChange;
// OnJobDeleted's subscriber not calling s.onJobDeleted with the event's id.
func TestTheDeleteSubscribersAreRegistered(t *testing.T) {
	calls, _ := callsInFunc(t, "monitor_callbacks.go", "wireMonitorCallbacks")
	var jobsChange, jobDeleted bool
	for _, c := range calls {
		switch {
		case c.recv == "s.db" && c.name == "OnJobsChange":
			jobsChange = slices.Equal(c.args, []string{"s.onJobsChange"})
		case c.recv == "s.db" && c.name == "OnJobDeleted":
			jobDeleted = len(c.args) == 1 && strings.Contains(c.args[0], "s.onJobDeleted(ev.JobID)")
		}
	}
	if !jobsChange {
		t.Error("wireMonitorCallbacks does not register s.onJobsChange with s.db.OnJobsChange — a bulk delete never reaches RetainJobs")
	}
	if !jobDeleted {
		t.Error("wireMonitorCallbacks' OnJobDeleted subscriber does not call s.onJobDeleted(ev.JobID) — a delete never reaches ForgetJob")
	}
}

// closureAssigned returns the function literal assigned to lhs (as written,
// "createYouTubeJob" or "s.twitchMon.OnStreamFound") inside the named
// top-level function of file.
func closureAssigned(t *testing.T, file, fn, lhs string) (*ast.FuncLit, *token.FileSet) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	text := func(n ast.Node) string {
		var buf bytes.Buffer
		_ = printer.Fprint(&buf, fset, n)
		return buf.String()
	}
	var lit *ast.FuncLit
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != fn || fd.Body == nil {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 || text(as.Lhs[0]) != lhs {
				return true
			}
			if l, ok := as.Rhs[0].(*ast.FuncLit); ok {
				lit = l
				return false
			}
			return true
		})
	}
	if lit == nil {
		t.Fatalf("%s: no function literal is assigned to %s in %s — re-anchor this test rather than deleting it", file, lhs, fn)
	}
	return lit, fset
}

// callsIn lists the calls inside node as name -> positions, by the called
// name (an identifier, or a selector's last part).
func callsIn(node ast.Node) map[string][]*ast.CallExpr {
	out := map[string][]*ast.CallExpr{}
	ast.Inspect(node, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			out[fun.Name] = append(out[fun.Name], call)
		case *ast.SelectorExpr:
			out[fun.Sel.Name] = append(out[fun.Sel.Name], call)
		}
		return true
	})
	return out
}

// createYouTubeJob holds back "Stream Found" for a backlog VOD: a deep
// backfill queues one per catalog VOD, and each announces itself with
// "Download Starting" when the scheduler admits it. The gate is
// announceYouTubeJobFound's, and it is only as good as the disposition the
// closure hands it — 9eaa52d tested the helper, and passing it any other
// value at the call site survived the suite.
//
// Asserted on the source because createYouTubeJob is a closure inside
// wireMonitorCallbacks, which a test cannot call.
//
// Mutant: announceYouTubeJobFound(s.notifyMgr, job, monitor.DispositionBroadcast)
// — or any argument but the closure's own disposition parameter.
func TestCreateYouTubeJobGatesFoundOnItsOwnDisposition(t *testing.T) {
	lit, fset := closureAssigned(t, "monitor_callbacks.go", "wireMonitorCallbacks", "createYouTubeJob")
	disposition := ""
	for _, field := range lit.Type.Params.List {
		var buf bytes.Buffer
		_ = printer.Fprint(&buf, fset, field.Type)
		if buf.String() == "monitor.JobDisposition" && len(field.Names) == 1 {
			disposition = field.Names[0].Name
		}
	}
	if disposition == "" {
		t.Fatal("createYouTubeJob takes no monitor.JobDisposition parameter — re-anchor this test rather than deleting it")
	}
	announces := callsIn(lit)["announceYouTubeJobFound"]
	if len(announces) != 1 {
		t.Fatalf("createYouTubeJob calls announceYouTubeJobFound %d times, want once", len(announces))
	}
	args := announces[0].Args
	if len(args) != 3 {
		t.Fatalf("announceYouTubeJobFound takes %d arguments here, want 3 — re-anchor this test", len(args))
	}
	if id, ok := args[2].(*ast.Ident); !ok || id.Name != disposition {
		var buf bytes.Buffer
		_ = printer.Fprint(&buf, fset, args[2])
		t.Errorf("createYouTubeJob hands announceYouTubeJobFound %q, want its own disposition %q — "+
			"a backlog VOD then announces itself twice", buf.String(), disposition)
	}
}

// A new job is announced BEFORE the worker is handed it (d60ed14). Each target
// delivers in queue order, and the worker's first event can be queued the
// moment it has the job; an edit-mode target's "Stream Found" or "Video
// Added" queued behind it edits the message that event created back to
// "Found" or "Added", possibly for hours. Both monitors' closures and both
// branches of the Web add route announce first.
//
// Asserted on the source, like the gate above: the monitor closures live
// inside wireMonitorCallbacks, and the add route's worker is a concrete
// *worker.DownloadWorker with nothing a test can observe at hand-off. The
// route's half is in internal/web/routes (TestAddedIsQueuedBeforeTheWorkerHasTheJob).
//
// Mutants: the YouTube closure's announceYouTubeJobFound after its
// EnqueueJob; the Twitch closure's notifyStreamFound after its EnqueueJob.
func TestNewJobsAreAnnouncedBeforeTheWorkerHasThem(t *testing.T) {
	for _, tc := range []struct{ closure, announce string }{
		{"createYouTubeJob", "announceYouTubeJobFound"},
		{"s.twitchMon.OnStreamFound", "notifyStreamFound"},
	} {
		lit, fset := closureAssigned(t, "monitor_callbacks.go", "wireMonitorCallbacks", tc.closure)
		calls := callsIn(lit)
		announces, enqueues := calls[tc.announce], calls["EnqueueJob"]
		if len(announces) != 1 || len(enqueues) == 0 {
			t.Errorf("%s: %d calls to %s and %d to EnqueueJob, want one and at least one — re-anchor this test",
				tc.closure, len(announces), tc.announce, len(enqueues))
			continue
		}
		for _, e := range enqueues {
			if e.Pos() < announces[0].Pos() {
				t.Errorf("%s: EnqueueJob (%s) comes before %s (%s) — an edit-mode target's lifecycle "+
					"message is edited back to its first state by the announcement",
					tc.closure, fset.Position(e.Pos()), tc.announce, fset.Position(announces[0].Pos()))
			}
		}
	}
}
