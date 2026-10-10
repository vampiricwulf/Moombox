package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
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

// resolveUpdateBreadcrumb asks clearSupersededFailureMarker about the
// breadcrumb BEFORE removing it, since the breadcrumb's age is the proof that
// helper reads: a later update that landed clears an earlier failure's marker,
// and the breadcrumb goes either way.
//
// Mutants: drop the clearSupersededFailureMarker call (no marker is ever
// cleared); move it after os.Remove(pendingPath) (its stat finds no
// breadcrumb, nothing is cleared); drop the os.Remove (the breadcrumb stays).
func TestResolvingTheBreadcrumbClearsTheSupersededMarkerFirst(t *testing.T) {
	exePath := filepath.Join(t.TempDir(), "moombox.exe")
	failedAt := time.Now().Add(-48 * time.Hour)
	writeAutoRollbackMarker(exePath, 1, true)
	marker := exePath + ".update-failed"
	if err := os.Chtimes(marker, failedAt, failedAt); err != nil {
		t.Fatal(err)
	}
	pendingPath := exePath + updater.PendingVersionSuffix
	if err := os.WriteFile(pendingPath, []byte("v9.9.9"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := config.NewStore(config.Defaults(), filepath.Join(t.TempDir(), "config.toml"))

	resolveUpdateBreadcrumb(store, exePath, "9.9.9", "", true, &nopLogger{})

	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("the marker a later update superseded is still there (stat: %v)", err)
	}
	if _, err := os.Stat(pendingPath); !os.IsNotExist(err) {
		t.Errorf("the breadcrumb is still there once resolved (stat: %v)", err)
	}
	var skipped string
	store.Read(func(c *config.MoomboxConfig) { skipped = c.Updates.SkippedVersion })
	if skipped != "" {
		t.Errorf("an update that landed marked %q skipped", skipped)
	}
}

// The boot the launcher rolled back to marks the release it failed skipped —
// the only thing that stops the daily check offering it again — and only once
// a config file exists: a first run writes none until setup does, and writing
// one here would skip the wizard on the next boot. The breadcrumb goes either
// way.
//
// Mutants: resolveUpdateBreadcrumb not writing SkippedVersion (`_ =
// rolledBackFrom`) — the rollback is never skipped; dropping `&& configLoaded`
// — a first run writes the skip.
func TestARolledBackReleaseIsMarkedSkipped(t *testing.T) {
	for _, tc := range []struct {
		name         string
		configLoaded bool
		want         string
	}{
		{"config loaded", true, "v2.0.0"},
		{"first run", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exePath := filepath.Join(t.TempDir(), "moombox.exe")
			pendingPath := exePath + updater.PendingVersionSuffix
			if err := os.WriteFile(pendingPath, []byte("v2.0.0"), 0o644); err != nil {
				t.Fatal(err)
			}
			writeAutoRollbackMarker(exePath, 2, true)
			store := config.NewStore(config.Defaults(), filepath.Join(t.TempDir(), "config.toml"))

			rolledBackFrom := rolledBackRelease(exePath, "1.0.0")
			if rolledBackFrom != "v2.0.0" {
				t.Fatalf("rolledBackRelease = %q, want v2.0.0", rolledBackFrom)
			}
			resolveUpdateBreadcrumb(store, exePath, "1.0.0", rolledBackFrom, tc.configLoaded, &nopLogger{})

			var skipped string
			store.Read(func(c *config.MoomboxConfig) { skipped = c.Updates.SkippedVersion })
			if skipped != tc.want {
				t.Errorf("skipped version = %q, want %q", skipped, tc.want)
			}
			if _, err := os.Stat(pendingPath); !os.IsNotExist(err) {
				t.Errorf("the breadcrumb is still there once resolved (stat: %v)", err)
			}
		})
	}
}

// rolledBackRelease is the boot's one reading of the breadcrumb as a
// rollback: a tag that is not this binary's own version, with either
// failed-update marker beside it. A breadcrumb with no marker is a manual
// binary swap, and one naming the running version is the update that landed.
//
// Mutants: ignoring the markers (`marker := true`) — a manual swap is taken
// for a rollback; reading only .update-failed — the .update-broken row fails.
func TestRolledBackReleaseReadsTheBreadcrumbAndMarkers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tag     string // "" = no breadcrumb
		markers []string
		want    string
	}{
		{"rollback", "v2.0.0", []string{".update-failed"}, "v2.0.0"},
		{"rollback beside a broken swap's marker", "v2.0.0", []string{".update-broken"}, "v2.0.0"},
		{"manual binary swap", "v2.0.0", nil, ""},
		{"the update landed", "v1.0.0", []string{".update-failed"}, ""},
		{"no breadcrumb", "", []string{".update-failed"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exePath := filepath.Join(t.TempDir(), "moombox.exe")
			if tc.tag != "" {
				if err := os.WriteFile(exePath+updater.PendingVersionSuffix, []byte(tc.tag+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			for _, m := range tc.markers {
				if err := os.WriteFile(exePath+m, []byte("failed"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := rolledBackRelease(exePath, "1.0.0"); got != tc.want {
				t.Errorf("rolledBackRelease = %q, want %q", got, tc.want)
			}
		})
	}
}

// A boot announces "Update Applied" for a version change it was not rolled
// back into: never on the first stamp, never on the launcher's rollback.
//
// Mutant: announcesVersionChange ignoring rolledBackFrom — the rollback is
// announced as an update "from v2.0.0 to v1.0.0 … restarted successfully".
func TestARollbackIsNotAnnouncedAsAnUpdate(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		last, current, rolledBak string
		want                     bool
	}{
		{"update", "1.0.0", "2.0.0", "", true},
		{"manual downgrade", "2.0.0", "1.0.0", "", true},
		{"first stamp", "", "2.0.0", "", false},
		{"same version", "2.0.0", "2.0.0", "", false},
		{"rollback", "2.0.0", "1.0.0", "v2.0.0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := announcesVersionChange(tc.last, tc.current, tc.rolledBak); got != tc.want {
				t.Errorf("announcesVersionChange(%q, %q, %q) = %v, want %v", tc.last, tc.current, tc.rolledBak, got, tc.want)
			}
		})
	}
}

// run() reads the rollback once (rolledBackRelease) and hands that reading to
// both of its consumers: the "Update Applied" decision and the breadcrumb's
// resolution, which marks the release skipped. run() itself boots the whole
// service graph, so its call sites are read from source.
//
// Mutants: run() dropping the resolveUpdateBreadcrumb call (the breadcrumb is
// never resolved and no rollback is skipped); passing "" in place of
// rolledBackFrom to either call; dropping the rolledBackRelease call.
func TestRunHandsTheRollbackToTheAnnouncementAndTheBreadcrumb(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var readAt token.Pos
	argOf := map[string]ast.Expr{} // callee → the argument that must be rolledBackFrom
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "run" {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				if len(n.Lhs) == 1 && len(n.Rhs) == 1 {
					if id, ok := n.Lhs[0].(*ast.Ident); ok && id.Name == "rolledBackFrom" {
						if call, ok := n.Rhs[0].(*ast.CallExpr); ok {
							if fun, ok := call.Fun.(*ast.Ident); ok && fun.Name == "rolledBackRelease" {
								readAt = n.Pos()
							}
						}
					}
				}
			case *ast.CallExpr:
				fun, ok := n.Fun.(*ast.Ident)
				if !ok {
					return true
				}
				switch fun.Name {
				case "announcesVersionChange":
					if len(n.Args) == 3 {
						argOf[fun.Name] = n.Args[2]
					}
				case "resolveUpdateBreadcrumb":
					if len(n.Args) == 6 {
						argOf[fun.Name] = n.Args[3]
					}
				}
			}
			return true
		})
		return false
	})
	if readAt == token.NoPos {
		t.Fatal("run() never assigns rolledBackFrom from rolledBackRelease")
	}
	for _, callee := range []string{"announcesVersionChange", "resolveUpdateBreadcrumb"} {
		arg, ok := argOf[callee]
		if !ok {
			t.Errorf("run() never calls %s", callee)
			continue
		}
		if id, ok := arg.(*ast.Ident); !ok || id.Name != "rolledBackFrom" {
			t.Errorf("run() calls %s at %s without the rollback it read", callee, fset.Position(arg.Pos()))
		} else if id.Pos() < readAt {
			t.Errorf("run() calls %s at %s before reading the rollback", callee, fset.Position(id.Pos()))
		}
	}
}
