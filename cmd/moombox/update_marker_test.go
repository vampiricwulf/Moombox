package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/updater"
)

// TestClearSupersededFailureMarker pins when a failed-update marker is stale:
// only when the .update-pending breadcrumb names the running version (the
// update landed) AND is newer than the marker (it was applied after the
// failure). The marker is the launcher's own (writeAutoRollbackMarker), so a
// drift in its name fails here rather than leaving it in place forever. The
// .update-broken marker is never touched.
//
// Mutants: drop the own-version check (a breadcrumb naming some OTHER release
// than the one running — this binary did not land it — deletes the marker);
// drop the age comparison (a marker this very update's failed first launch
// wrote is deleted by its relaunch). The rollback shape — the restored older
// binary reading the tag it was updating to — is kept by both terms.
func TestClearSupersededFailureMarker(t *testing.T) {
	failedAt := time.Now().Add(-48 * time.Hour)
	for _, tc := range []struct {
		name       string
		pendingTag string
		pendingAt  time.Time // zero = no breadcrumb
		wantClear  bool
	}{
		{"a later update landed", "v9.9.9", failedAt.Add(24 * time.Hour), true},
		{"this update's own failed first launch", "v9.9.9", failedAt.Add(-time.Hour), false},
		{"the restored binary after a rollback", "v10.0.0", failedAt.Add(-time.Hour), false},
		{"a later update to some other version", "v10.0.0", failedAt.Add(24 * time.Hour), false},
		{"no breadcrumb", "v9.9.9", time.Time{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exePath := filepath.Join(t.TempDir(), "moombox.exe")
			writeAutoRollbackMarker(exePath, 1, true)
			marker := exePath + ".update-failed"
			if err := os.Chtimes(marker, failedAt, failedAt); err != nil {
				t.Fatal(err)
			}
			broken := exePath + ".update-broken"
			if err := os.WriteFile(broken, []byte("both renames failed"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(broken, failedAt, failedAt); err != nil {
				t.Fatal(err)
			}
			pendingPath := exePath + updater.PendingVersionSuffix
			if !tc.pendingAt.IsZero() {
				if err := os.WriteFile(pendingPath, []byte(tc.pendingTag), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(pendingPath, tc.pendingAt, tc.pendingAt); err != nil {
					t.Fatal(err)
				}
			}

			cleared, err := clearSupersededFailureMarker(exePath, pendingPath, tc.pendingTag, "9.9.9")
			if err != nil {
				t.Fatalf("clearSupersededFailureMarker: %v", err)
			}
			_, statErr := os.Stat(marker)
			if gone := os.IsNotExist(statErr); gone != tc.wantClear {
				t.Errorf("marker removed = %v, want %v", gone, tc.wantClear)
			}
			if tc.wantClear && cleared != marker {
				t.Errorf("returned %q, want the marker's path %q for the log line", cleared, marker)
			}
			if !tc.wantClear && cleared != "" {
				t.Errorf("returned %q for a marker it kept", cleared)
			}
			if _, err := os.Stat(broken); err != nil {
				t.Errorf("the .update-broken marker was touched: %v", err)
			}
		})
	}
}

// TestRunClearsTheSupersededMarkerBeforeTheBreadcrumbGoes pins the call
// site: run() asks clearSupersededFailureMarker about the pending breadcrumb
// BEFORE removing it, since the breadcrumb's age is the proof the helper
// reads.
//
// Mutants: drop the call (no marker is ever cleared); move it after
// os.Remove(pendingPath) (the stat finds no breadcrumb, nothing is cleared).
func TestRunClearsTheSupersededMarkerBeforeTheBreadcrumbGoes(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var clearAt, removeAt token.Pos
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "run" {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fun := call.Fun.(type) {
			case *ast.Ident:
				if fun.Name == "clearSupersededFailureMarker" && clearAt == token.NoPos {
					clearAt = call.Pos()
				}
			case *ast.SelectorExpr:
				if x, ok := fun.X.(*ast.Ident); ok && x.Name == "os" && fun.Sel.Name == "Remove" && len(call.Args) == 1 {
					if arg, ok := call.Args[0].(*ast.Ident); ok && arg.Name == "pendingPath" {
						removeAt = call.Pos()
					}
				}
			}
			return true
		})
		return false
	})
	if clearAt == token.NoPos {
		t.Fatal("run() never calls clearSupersededFailureMarker — a marker a later update superseded stays forever")
	}
	if removeAt == token.NoPos {
		t.Fatal("run() no longer removes the pending breadcrumb by name; this test's anchor moved")
	}
	if clearAt > removeAt {
		t.Errorf("clearSupersededFailureMarker (%s) runs after os.Remove(pendingPath) (%s) — it reads the breadcrumb's age",
			fset.Position(clearAt), fset.Position(removeAt))
	}
}
