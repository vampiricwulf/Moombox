package tui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"

	tea "charm.land/bubbletea/v2"
)

// isSelectorCall reports whether fun is the selector call pkg.name.
func isSelectorCall(fun ast.Expr, pkg, name string) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == pkg
}

// TestTheOneProgramIsBuiltWithTheTargetFPS pins the call site. Bubbletea
// applies defaultFPS = 60 to a Program built with no options, so the renderer
// runs at half the owner's 120 the moment this argument goes missing — and
// nothing about the TUI breaks, it just draws less often. Structural rather
// than behavioural because a ProgramOption is an opaque closure: what can only
// be asserted here is that the one construction site passes it.
//
// The "exactly one site" half is load-bearing on its own: a second
// tea.NewProgram anywhere in the package would be an optionless Program that
// the behavioural pin below could never see.
//
// It asserts the PRESENCE of tea.WithFPS among the options, not the option
// COUNT: adding tea.WithAltScreen() or tea.WithoutSignalHandler() later is a
// correct change that must not fail a pin about the frame rate.
//
// MUTANT: restoring tea.NewProgram(app) — the "no tea.WithFPS option"
// assertion fires (via the arity branch). MUTANT: dropping tea.WithFPS while
// keeping some other option — the "no tea.WithFPS option" assertion fires.
// MUTANT: tea.WithFPS(60), or any literal in place of the constant — the
// identifier assertion fires. MUTANT: a second construction site added
// anywhere in the package — the count assertion fires.
func TestTheOneProgramIsBuiltWithTheTargetFPS(t *testing.T) {
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob package sources: %v", err)
	}
	fset := token.NewFileSet()
	sites := 0
	for _, name := range sources {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !isSelectorCall(call.Fun, "tea", "NewProgram") {
				return true
			}
			sites++
			where := fset.Position(call.Pos())
			if len(call.Args) < 2 {
				t.Errorf("%s: tea.NewProgram has %d arguments, want the model plus at least the tea.WithFPS option",
					where, len(call.Args))
				return true
			}
			found := false
			for _, arg := range call.Args[1:] {
				opt, ok := arg.(*ast.CallExpr)
				if !ok || !isSelectorCall(opt.Fun, "tea", "WithFPS") {
					continue // some other ProgramOption — not this pin's business
				}
				found = true
				if len(opt.Args) != 1 {
					t.Errorf("%s: tea.WithFPS has %d arguments, want 1", where, len(opt.Args))
					continue
				}
				id, ok := opt.Args[0].(*ast.Ident)
				if !ok || id.Name != "tuiTargetFPS" {
					t.Errorf("%s: tea.WithFPS takes something other than the tuiTargetFPS constant", where)
				}
			}
			if !found {
				t.Errorf("%s: no tea.WithFPS option passed to tea.NewProgram", where)
			}
			return true
		})
	}
	if sites != 1 {
		t.Errorf("tea.NewProgram call sites in package tui = %d, want exactly 1 (Run, app_commands.go)", sites)
	}
}

// programFPS reads the frame-rate ceiling bubbletea actually stored. Program.fps
// is unexported and there is no getter, but NewProgram resolves it eagerly
// (charm.land/bubbletea/v2 tea.go: below 1 becomes defaultFPS, above maxFPS is
// clamped to it), so constructing a Program is enough to observe the value the
// renderer will use. reflect.NewAt in a single expression is the form go vet's
// unsafeptr analyzer accepts.
func programFPS(t *testing.T, p *tea.Program) int {
	t.Helper()
	f := reflect.ValueOf(p).Elem().FieldByName("fps")
	if !f.IsValid() {
		t.Fatal("charm.land/bubbletea/v2's Program has no fps field any more — re-point this pin at whatever replaced it")
	}
	return int(reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().Int())
}

// TestTargetFPSReachesTheRenderer is the half the AST pin cannot see: that
// asking for tuiTargetFPS actually yields 120 frames a second. 120 is
// bubbletea's own maxFPS, so a dependency bump that lowered the ceiling would
// silently clamp the owner's 120 back down with no compile error and no AST
// change — this is what notices.
//
// The no-option control is not decoration: without it a programFPS that
// returned a constant 120 (a reflect read against the wrong field, say) would
// pass the first assertion and prove nothing.
//
// MUTANT: tuiTargetFPS = 240 — bubbletea clamps to 120 and the first assertion
// still passes, which is correct and intended; the AST pin is what holds the
// constant honest. MUTANT: a programFPS that reads the wrong field or returns
// a literal — the control assertion fires.
func TestTargetFPSReachesTheRenderer(t *testing.T) {
	if got := programFPS(t, tea.NewProgram(NewApp(), tea.WithFPS(tuiTargetFPS))); got != 120 {
		t.Errorf("Program fps with tea.WithFPS(tuiTargetFPS) = %d, want 120", got)
	}
	if got := programFPS(t, tea.NewProgram(NewApp())); got != 60 {
		t.Errorf("Program fps with no option = %d, want bubbletea's default of 60 — if this is already 120 the assertion above proves nothing", got)
	}
}

// TestTheFastProgressTickIsSizedToOneFrame ties the two halves of ruling F1
// together. The fast tick exists so a progress change reaches the model before
// the next frame is drawn; at 120 fps a frame is 8.33 ms, so a 16 ms tick lets
// a change sit through a whole extra frame, and a 4 ms tick wakes the loop
// twice for a frame that can only be drawn once.
//
// MUTANT: restoring progressFastInterval = 16ms — the first assertion fires
// with "longer than one frame". MUTANT: 4ms "to be safe" — the second fires.
// MUTANT: dropping tuiTargetFPS back to 60 — the frame budget becomes 16.6ms
// and the second assertion fires, so this test also guards the pair.
func TestTheFastProgressTickIsSizedToOneFrame(t *testing.T) {
	frame := time.Second / time.Duration(tuiTargetFPS)
	if progressFastInterval > frame {
		t.Errorf("progressFastInterval = %v, longer than one frame at %d fps (%v) — a progress change can miss a frame",
			progressFastInterval, tuiTargetFPS, frame)
	}
	if progressFastInterval*2 <= frame {
		t.Errorf("progressFastInterval = %v, more than twice per frame at %d fps (%v) — ticks the renderer cannot show",
			progressFastInterval, tuiTargetFPS, frame)
	}
}
