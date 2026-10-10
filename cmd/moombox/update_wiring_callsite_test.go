package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"
)

// The update fixes live in helpers their own tests drive directly
// (dismissUpdateFromTUI, applyUpdateFromTUI, pendingUpdateTag with
// postUpdateBoot.restarted); what puts them in production is a call site in
// runTUI or in the launcher loop, which only a live tea.Program or a real
// child process reaches. So those call sites are read from source, in the
// idiom of TestRunHandsTheRollbackToTheAnnouncementAndTheBreadcrumb.

// funcBody parses file and returns the body of its function or method named
// name.
func funcBody(t *testing.T, file, name string) (*token.FileSet, *ast.BlockStmt) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == name && fn.Body != nil {
			return fset, fn.Body
		}
	}
	t.Fatalf("%s declares no %s", file, name)
	return nil, nil
}

// The launcher arms the boot of an applied update with the release the
// .update-pending breadcrumb names, read at the restart that applies it. That
// record is what lets a rollback after the update's boot passed its milestone
// (and removed the breadcrumb) still skip the release and report a rollback
// rather than "Update Applied v2→v1"; TestALateRollbackSkipsTheReleaseAndIsNoUpdate
// drives it from pendingUpdateTag on, and this pins that the loop is where the
// tag comes from.
//
// Mutants: the childRestart arm passing "" for the tag
// (`boot.restarted(handleUpdateRestart(exePath), "", wasFirstAfterUpdate, ranFor)`)
// — restoreUpdateBreadcrumb has nothing to restore and a late Windows
// rollback again skips nothing and is announced as an update; passing ""
// for the artifact — no boot is ever armed and no failed update is rolled
// back.
func TestTheLauncherArmsTheBootWithTheBreadcrumbsRelease(t *testing.T) {
	const want = "boot.restarted(handleUpdateRestart(exePath), pendingUpdateTag(exePath), wasFirstAfterUpdate, ranFor)"
	fset, body := funcBody(t, "launcher.go", "launchAndSupervise")
	var found []string
	ast.Inspect(body, func(n ast.Node) bool {
		cc, ok := n.(*ast.CaseClause)
		if !ok || len(cc.List) != 1 {
			return true
		}
		if id, ok := cc.List[0].(*ast.Ident); !ok || id.Name != "childRestart" {
			return true
		}
		ast.Inspect(cc, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "restarted" {
				got := types.ExprString(call)
				found = append(found, got)
				if got != want {
					t.Errorf("the childRestart arm at %s calls %s, want %s", fset.Position(call.Pos()), got, want)
				}
			}
			return true
		})
		return false
	})
	if len(found) != 1 {
		t.Fatalf("the childRestart arm of launchAndSupervise calls boot.restarted %d times (%v), want once", len(found), found)
	}
}

// The TUI's update keys are the helpers that hold them to the release on
// screen and tell the dashboards: OnDismissUpdate is dismissUpdateFromTUI,
// which announces update_cleared after the skip, and OnApplyUpdate is
// applyUpdateFromTUI, which installs only the release tagged tag.
// TestTheTUISkipClearsEveryDashboardsBadge and TestTheTUIActsOnlyOnTheReleaseItShows
// drive those helpers; this pins that runTUI installs them.
//
// Mutants: `app.OnDismissUpdate = func(tag string) error { return
// routes.DismissUpdate(s.configStore, tag) }` (the pre-fix closure) — a TUI
// skip again reaches no dashboard, whose badge stays up over two failing
// buttons; OnApplyUpdate as the pre-fix closure installing
// routes.SharedUpdateInfo.Load() whatever ver says — the TUI installs a
// release whose notes were never on screen; either assignment deleted.
func TestTheTUIUpdateKeysAreTheTagCheckedHelpers(t *testing.T) {
	fset, body := funcBody(t, "tui_wiring.go", "runTUI")
	want := map[string]string{
		"app.OnDismissUpdate": "s.dismissUpdateFromTUI",
		"app.OnApplyUpdate":   "s.applyUpdateFromTUI",
	}
	seen := map[string]int{}
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		lhs := types.ExprString(as.Lhs[0])
		w, ok := want[lhs]
		if !ok {
			return true
		}
		seen[lhs]++
		if got := types.ExprString(as.Rhs[0]); got != w {
			t.Errorf("runTUI sets %s at %s to %s, want %s", lhs, fset.Position(as.Pos()), got, w)
		}
		return true
	})
	for lhs := range want {
		if seen[lhs] != 1 {
			t.Errorf("runTUI sets %s %d times, want once", lhs, seen[lhs])
		}
	}
}
