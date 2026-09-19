package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestRunTUIWiresFfmpegPathChangeToApplyFfmpegPath pins the one line that
// closes the chain the TUI half cannot reach.
//
// applyFfmpegPath is the ONLY re-application of paths.ffmpeg_path to the two
// consumers that captured it when their muxers were built (the trim service
// and the download orchestrator), and OnSaveConfig returns before its own
// hot-reload block when the write is refused — deliberately, because the
// settings panel rolls its config back afterwards. So a validated FFmpeg path
// whose save failed reaches the muxer only through App.OnFfmpegPathChange.
// internal/tui pins that the App fires the hook on both save outcomes and
// hot_reload_test pins what applyFfmpegPath then does; this is the join.
//
// Structural for the reason profiledir_verdict_callsite_test.go is: runTUI
// builds the whole TUI and runs the bubbletea program, so this package cannot
// drive it, and the seam is an assignment's presence.
//
// Mutants: delete the assignment, or point it at some other function — the
// fatal below fires and names what breaks.
func TestRunTUIWiresFfmpegPathChangeToApplyFfmpegPath(t *testing.T) {
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

	wired := false
	ast.Inspect(body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return true
		}
		if selectorIs(assign.Lhs[0], "app", "OnFfmpegPathChange") && selectorIs(assign.Rhs[0], "s", "applyFfmpegPath") {
			wired = true
		}
		return true
	})
	if !wired {
		t.Fatal("runTUI does not wire app.OnFfmpegPathChange = s.applyFfmpegPath — a validated FFmpeg " +
			"path whose config save is refused never reaches the download worker or the trim service, " +
			"so muxing keeps failing for the whole session with the binary sitting right there")
	}
}
