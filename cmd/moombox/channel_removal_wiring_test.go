package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestCreateYouTubeJobAsksTheLiveConfigFirst pins the guard a channel
// removal's "delete its pending jobs" depends on (W25-09): a monitor cycle
// takes its channel list as it starts, so without the guard a cycle already
// under way recreated, history and all, the very rows the removal had just
// deleted. createYouTubeJob — the one place the feed and DECAPI monitors
// turn a found video into a row — must ask channelMonitored (the live
// config, liveChannelEnabled) and return before it reaches s.db.AddJob.
// liveChannelEnabled's own answers are TestLiveChannelEnabledReadsTheLiveConfig's.
//
// Mutants killed: the guard removed; the guard moved below AddJob; the guard
// falling through instead of returning.
func TestCreateYouTubeJobAsksTheLiveConfigFirst(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "monitor_callbacks.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var body *ast.BlockStmt
	ast.Inspect(f, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		if id, ok := as.Lhs[0].(*ast.Ident); ok && id.Name == "createYouTubeJob" {
			if lit, ok := as.Rhs[0].(*ast.FuncLit); ok {
				body = lit.Body
			}
		}
		return true
	})
	if body == nil {
		t.Fatal("no createYouTubeJob closure in monitor_callbacks.go")
	}

	guard, addJob := token.NoPos, token.NoPos
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.IfStmt:
			if guard == token.NoPos && callsIdent(n.Cond, "channelMonitored") && endsInReturn(n.Body) {
				guard = n.Pos()
			}
		case *ast.CallExpr:
			if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "AddJob" && addJob == token.NoPos {
				addJob = n.Pos()
			}
		}
		return true
	})
	if addJob == token.NoPos {
		t.Fatal("createYouTubeJob no longer calls AddJob; this test needs updating")
	}
	if guard == token.NoPos {
		t.Fatal("createYouTubeJob creates a job without asking channelMonitored whether its channel is still monitored")
	}
	if guard > addJob {
		t.Errorf("the channelMonitored guard (%s) comes after AddJob (%s)", fset.Position(guard), fset.Position(addJob))
	}
}

func callsIdent(e ast.Expr, name string) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if id, ok := c.Fun.(*ast.Ident); ok && id.Name == name {
				found = true
			}
		}
		return !found
	})
	return found
}

func endsInReturn(b *ast.BlockStmt) bool {
	if len(b.List) == 0 {
		return false
	}
	_, ok := b.List[len(b.List)-1].(*ast.ReturnStmt)
	return ok
}
