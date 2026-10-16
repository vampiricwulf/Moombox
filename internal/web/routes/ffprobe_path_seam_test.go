package routes

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryFFprobeCallAppliesThePathSeam is W27-05 for this package: the
// import's duration probe (importProbeDuration) hands ffprobe its file through
// engine.FFmpegPathArg, the seam every FFmpeg argv goes through (sweep-2
// ENGINE-12). ffprobe opens a file through the same libavformat file protocol
// FFmpeg does, so a path that needs the \\?\ prefix for one needs it for the
// other. The import probes its own temporary name in imports/, which sits
// under the output directory, however deep that is.
//
// Structural, as internal/worker's twin is, because the seam's Windows branch
// can only be switched on inside internal/engine. It reads the package's own
// source, so a new ffprobe call that forgets the seam fails it too.
//
// Mutant it kills: importProbeDuration handing ffprobe its file raw.
func TestEveryFFprobeCallAppliesThePathSeam(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	found := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			// childproc.CommandContext is how the probe runs (tied to this
			// process); an exec call naming ffprobe is held to the seam too.
			if pkg, ok := sel.X.(*ast.Ident); !ok || (pkg.Name != "exec" && pkg.Name != "childproc") {
				return true
			}
			var program ast.Expr
			switch {
			case sel.Sel.Name == "Command" && len(call.Args) >= 2:
				program = call.Args[0]
			case sel.Sel.Name == "CommandContext" && len(call.Args) >= 3:
				program = call.Args[1]
			default:
				return true
			}
			if !mentionsFFprobe(program) {
				return true
			}
			found++
			if call.Ellipsis != token.NoPos || !isEngineFFmpegPathArgCall(call.Args[len(call.Args)-1]) {
				t.Errorf("%s: ffprobe is run with its file argument not wrapped in engine.FFmpegPathArg", fset.Position(call.Pos()))
			}
			return true
		})
	}
	if found == 0 {
		t.Fatal("found no ffprobe call in the package: the pin no longer sees importProbeDuration")
	}
}

// mentionsFFprobe reports whether an expression names ffprobe (ffprobePath).
func mentionsFFprobe(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && strings.Contains(strings.ToLower(id.Name), "ffprobe") {
			found = true
		}
		return !found
	})
	return found
}

// isEngineFFmpegPathArgCall reports whether e is a call engine.FFmpegPathArg(…).
func isEngineFFmpegPathArgCall(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "engine" && sel.Sel.Name == "FFmpegPathArg"
}
