package childproc

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// moduleRoot walks up from the package directory to the go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the package directory")
		}
		dir = parent
	}
}

// namesFFmpeg is what a program argument that runs FFmpeg or ffprobe reads
// like in this tree: m.ffmpegPath, ffprobePath, o.mux().FFprobePath(),
// "ffmpeg".
var namesFFmpeg = regexp.MustCompile(`(?i)ff(mpeg|probe)`)

// TestNoFFmpegCommandOutsideChildproc holds every production file in the
// module to CommandContext for FFmpeg and ffprobe: an exec.Command or
// exec.CommandContext whose program names either is a program a crash of
// this process leaves running (W28-A01). It reads the source, so a new call
// site that reaches for os/exec fails it — the mux, the progress runner, the
// probes, the import's duration probe and both FFmpeg checks all go through
// here today. Test files are exempt: they build fixtures with FFmpeg, in
// processes nothing respawns.
//
// Mutant: any one site (internal/engine/muxer.go's runFFmpeg, say) back on
// exec.CommandContext — this test names it.
func TestNoFFmpegCommandOutsideChildproc(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	checked := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "references" || name == "node_modules" ||
				name == "vendor" || name == "testdata") {
				return filepath.SkipDir
			}
			if path != root {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir // another module
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		checked++
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "exec" {
				return true
			}
			var program ast.Expr
			switch {
			case sel.Sel.Name == "Command" && len(call.Args) >= 1:
				program = call.Args[0]
			case sel.Sel.Name == "CommandContext" && len(call.Args) >= 2:
				program = call.Args[1]
			default:
				return true
			}
			var b strings.Builder
			_ = printer.Fprint(&b, fset, program)
			if namesFFmpeg.MatchString(b.String()) {
				rel, _ := filepath.Rel(root, fset.Position(call.Pos()).Filename)
				t.Errorf("%s:%d runs %s through os/exec: FFmpeg and ffprobe start through childproc.CommandContext, "+
					"or a crash of this process leaves them running", filepath.ToSlash(rel), fset.Position(call.Pos()).Line, b.String())
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 100 {
		t.Fatalf("read %d source files under %s; the walk is not seeing the module", checked, root)
	}
}
