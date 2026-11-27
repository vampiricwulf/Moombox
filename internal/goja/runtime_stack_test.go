package goja

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dop251/goja"
)

// TestEveryRuntimeConstructorBoundsRecursion runs an endless recursion on a
// runtime from each constructor in this package and expects goja's
// stack-overflow abort within MaxCallStackSize frames, quickly and without the
// heap growth an unbounded run shows (306 MB in 5 s, RV-13). The second script
// recurses through a native call (Array.prototype.forEach), the path a
// bound counted only on direct JS calls would miss.
//
// Not parallel: the heap figure is process-wide.
func TestEveryRuntimeConstructorBoundsRecursion(t *testing.T) {
	constructors := map[string]func(t *testing.T) *goja.Runtime{
		"NewPlainRuntime": func(*testing.T) *goja.Runtime { return NewPlainRuntime() },
		"NewRuntime":      func(*testing.T) *goja.Runtime { return NewRuntime() },
		"NewRuntimeWithShims": func(t *testing.T) *goja.Runtime {
			vm, tm, err := NewRuntimeWithShims(t.Context(), "")
			if err != nil {
				t.Fatalf("NewRuntimeWithShims: %v", err)
			}
			t.Cleanup(tm.CancelAll)
			return vm
		},
		"NewRuntimeForCipher": func(t *testing.T) *goja.Runtime {
			vm, err := NewRuntimeForCipher("")
			if err != nil {
				t.Fatalf("NewRuntimeForCipher: %v", err)
			}
			return vm
		},
	}
	scripts := map[string]string{
		"direct":         "var depth = 0; function f() { depth++; return f() + 1; } f();",
		"through_native": "var depth = 0; function f() { depth++; [0].forEach(f); } f();",
	}
	// The bounded runs allocate about 9 MB (direct) and 22 MB (through a native
	// call). The RV-13 research saw an unbounded run reach 306 MB in 5 s.
	const allocCeiling = 128 << 20

	for cname, mk := range constructors {
		for sname, src := range scripts {
			t.Run(cname+"/"+sname, func(t *testing.T) {
				vm := mk(t)
				var before, after runtime.MemStats
				runtime.GC()
				runtime.ReadMemStats(&before)
				start := time.Now()
				_, err := RunStringWithTimeout(context.Background(), vm, src, 5*time.Second)
				elapsed := time.Since(start)
				runtime.ReadMemStats(&after)

				var so *goja.StackOverflowError
				if !errors.As(err, &so) {
					t.Fatalf("want *goja.StackOverflowError after %v, got %T: %v", elapsed, err, err)
				}
				depth := vm.Get("depth").ToInteger()
				t.Logf("aborted at depth %d after %v, %d KB allocated", depth, elapsed, (after.TotalAlloc-before.TotalAlloc)>>10)
				if depth > MaxCallStackSize {
					t.Errorf("recursion reached depth %d, past MaxCallStackSize %d", depth, MaxCallStackSize)
				}
				if alloc := after.TotalAlloc - before.TotalAlloc; alloc > allocCeiling {
					t.Errorf("the aborted run allocated %d MB, want under %d MB", alloc>>20, allocCeiling>>20)
				}
				// The runtime stays usable after the abort.
				if v, err := vm.RunString("1 + 1"); err != nil || v.ToInteger() != 2 {
					t.Errorf("runtime unusable after the abort: %v, %v", v, err)
				}
			})
		}
	}
}

// TestEveryGojaRuntimeIsMadeByNewPlainRuntime holds the rule that every goja
// runtime in the tree, test helpers included, comes from NewPlainRuntime, so
// none runs without the call-stack bound (owner ruling RV-13). It parses every
// Go file in the module and fails on any call of the goja package's New outside
// NewPlainRuntime.
func TestEveryGojaRuntimeIsMadeByNewPlainRuntime(t *testing.T) {
	root := moduleRoot(t)
	skipDirs := map[string]bool{
		".git": true, ".worktrees": true, ".superpowers": true, "references": true,
		"node_modules": true, "vendor": true, "bgutil-sidecar": true, "docs-site": true,
	}
	allowed := filepath.Join(root, "internal", "goja", "runtime.go")
	fset := token.NewFileSet()
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		local := ""
		for _, imp := range file.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == "github.com/dop251/goja" {
				local = "goja"
				if imp.Name != nil {
					local = imp.Name.Name
				}
			}
		}
		if local == "" || local == "_" {
			return nil
		}
		for _, decl := range file.Decls {
			ast.Inspect(decl, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "New" {
					return true
				}
				if id, ok := sel.X.(*ast.Ident); !ok || id.Name != local {
					return true
				}
				fn, isFunc := decl.(*ast.FuncDecl)
				if path == allowed && isFunc && fn.Recv == nil && fn.Name.Name == "NewPlainRuntime" {
					return true
				}
				found = append(found, fset.Position(call.Pos()).String())
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the module: %v", err)
	}
	if len(found) > 0 {
		t.Errorf("goja.New called outside NewPlainRuntime; such a runtime has no call-stack bound. "+
			"Use internal/goja's NewPlainRuntime (or NewRuntime) instead:\n  %s", strings.Join(found, "\n  "))
	}
}

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
