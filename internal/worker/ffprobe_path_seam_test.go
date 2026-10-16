package worker

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryFFprobeCallAppliesThePathSeam is W27-05: every ffprobe this
// package runs hands it its file through engine.FFmpegPathArg, the seam the
// mux's own FFmpeg argv goes through (sweep-2 ENGINE-12). ffprobe opens its
// input through the same libavformat file protocol FFmpeg does, so on a
// Windows host without LongPathsEnabled and an FFmpeg build that does not add
// \\?\ itself, an archive path of 260 or more UTF-16 units muxed through the
// prefix and was then probed raw: runFFprobe failed, the row finished without
// resolution or duration and without the ENGINE-9 short-output check, a Tier 4
// merge aborted at its probe, and a trim fell back to 128 kbps audio.
//
// Structural, because the seam's Windows branch can only be switched on from
// inside internal/engine (ffmpegPathOS, pinned there by TestFFmpegPathArg and
// TestFFmpegPathArgIsTheSeamOutsideTheEngine): what this package can be held
// to is that every ffprobe argv ends in the seam's answer. It reads the
// package's own source, so a new ffprobe call that forgets the seam fails it
// too.
//
// Mutants it kills, one per site: runFFprobe (orchestrator_utils.go),
// probeStreamParams (probe_params.go) or probeAudioBitrate (trim.go) handing
// ffprobe filePath raw.
func TestEveryFFprobeCallAppliesThePathSeam(t *testing.T) {
	sites := ffprobeExecSites(t)
	want := map[string]bool{"runFFprobe": true, "probeStreamParams": true, "probeAudioBitrate": true}
	for fn, wrapped := range sites {
		if !wrapped {
			t.Errorf("%s runs ffprobe with its file argument not wrapped in engine.FFmpegPathArg", fn)
		}
		delete(want, fn)
	}
	for fn := range want {
		t.Errorf("found no ffprobe call in %s: the pin no longer sees the site it guards", fn)
	}
}

// ffprobeExecSites maps each function in this package's non-test source that
// builds an ffprobe command (childproc.CommandContext, or an exec.Command or
// exec.CommandContext, whose program names ffprobe) to whether every such
// call's last argument is engine.FFmpegPathArg(…).
func ffprobeExecSites(t *testing.T) map[string]bool {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	sites := map[string]bool{}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				program, ok := execProgramArg(call)
				if !ok || !namesFFprobe(program) {
					return true
				}
				wrapped := call.Ellipsis == token.NoPos && isEngineFFmpegPathArg(call.Args[len(call.Args)-1])
				if prev, seen := sites[fn.Name.Name]; seen {
					wrapped = wrapped && prev
				}
				sites[fn.Name.Name] = wrapped
				return true
			})
		}
	}
	return sites
}

// execProgramArg returns the program argument of a childproc.CommandContext,
// exec.Command or exec.CommandContext call.
func execProgramArg(call *ast.CallExpr) (ast.Expr, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil, false
	}
	if pkg, ok := sel.X.(*ast.Ident); !ok || (pkg.Name != "exec" && pkg.Name != "childproc") {
		return nil, false
	}
	switch {
	case sel.Sel.Name == "Command" && len(call.Args) >= 2:
		return call.Args[0], true
	case sel.Sel.Name == "CommandContext" && len(call.Args) >= 3:
		return call.Args[1], true
	}
	return nil, false
}

// namesFFprobe reports whether an expression mentions ffprobe by name:
// ffprobePath, o.mux().FFprobePath().
func namesFFprobe(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && strings.Contains(strings.ToLower(id.Name), "ffprobe") {
			found = true
		}
		return !found
	})
	return found
}

// isEngineFFmpegPathArg reports whether e is a call engine.FFmpegPathArg(…).
func isEngineFFmpegPathArg(e ast.Expr) bool {
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
