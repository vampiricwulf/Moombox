package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/web"
)

// TestBothUIsShareOneHideFinishedAgeBroadcast pins the CORE-11 pairing: the
// Web PUT's ConfigRoutesCallbacks.OnHideFinishedAgeChanged and the TUI's
// OnSaveConfig call the SAME runState method, so a threshold changed from the
// terminal reaches every dashboard exactly the way a dashboard change does.
// Before this the TUI save broadcast nothing at all and the two UIs disagreed
// about which Finished jobs are archived.
//
// Structural because neither call site can be driven from a test: wireRoutes
// registers the whole REST surface and runTUI runs the bubbletea program. The
// seam is which function the two sites name — the same shape the FFmpeg
// callsite test beside it pins.
//
// Mutants: point OnHideFinishedAgeChanged back at an inline closure (the two
// copies drift — the original bug); delete the s.broadcastHideFinishedAge()
// call from OnSaveConfig (a TUI save never reaches the dashboards).
func TestBothUIsShareOneHideFinishedAgeBroadcast(t *testing.T) {
	fset := token.NewFileSet()

	routesFile, err := parser.ParseFile(fset, "routes_wiring.go", nil, 0)
	if err != nil {
		t.Fatalf("parse routes_wiring.go: %v", err)
	}
	declared := false
	for _, decl := range routesFile.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "broadcastHideFinishedAge" && fn.Recv != nil {
			declared = true
		}
	}
	if !declared {
		t.Fatal("routes_wiring.go declares no runState.broadcastHideFinishedAge — the shared " +
			"broadcast is what gives the TUI's save a dashboard-side effect (CORE-11)")
	}

	wired := false
	ast.Inspect(routesFile, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		key, ok := kv.Key.(*ast.Ident)
		if ok && key.Name == "OnHideFinishedAgeChanged" && selectorIs(kv.Value, "s", "broadcastHideFinishedAge") {
			wired = true
		}
		return true
	})
	if !wired {
		t.Error("ConfigRoutesCallbacks.OnHideFinishedAgeChanged is not s.broadcastHideFinishedAge — " +
			"a second copy of the body is exactly the divergence CORE-11 reports")
	}

	tuiFile, err := parser.ParseFile(fset, "tui_wiring.go", nil, 0)
	if err != nil {
		t.Fatalf("parse tui_wiring.go: %v", err)
	}
	called := false
	for _, decl := range tuiFile.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "runTUI" || fn.Body == nil {
			continue
		}
		if hasCallTo(fn.Body, "s", "broadcastHideFinishedAge") {
			called = true
		}
	}
	if !called {
		t.Error("runTUI never calls s.broadcastHideFinishedAge — a hide_finished_age_days change " +
			"saved from the TUI settings overlay leaves every open dashboard on the old threshold")
	}
}

// A smoke gate on the shared method itself, deliberately claiming only what
// it kills: run against a real store, a real database and a hub with no
// clients, which is the new TUI call site's ordinary case (a headless
// terminal session saves settings with no dashboard open anywhere). It does
// not observe the broadcast payload — the hub has no seam for that without a
// dialled websocket — so the payload's ordering and threshold-capture
// contract stays pinned by the method's comment and by the Web-side route
// tests, not here.
//
// Mutant: reading the config through s.cfg (nil in this construction, and in
// runState until initServices) instead of s.configStore.Read — the call
// panics.
func TestBroadcastHideFinishedAgeRunsAgainstALiveStore(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	cfg := config.Defaults()
	cfg.Monitors.HideFinishedAgeDays = config.FlexDuration{Value: 30}
	store := config.NewStore(cfg, "")
	s := &runState{db: db, wsHub: web.NewWebSocketHub(sweepTestLogger{}), configStore: store}

	s.broadcastHideFinishedAge()

	if err := store.Update(func(c *config.MoomboxConfig) {
		c.Monitors.HideFinishedAgeDays = config.FlexDuration{Value: 0.5}
	}); err != nil {
		t.Fatalf("store.Update: %v", err)
	}
	var live float64
	store.Read(func(c *config.MoomboxConfig) {
		live = c.Monitors.HideFinishedAgeDays.Value
	})
	if live != 0.5 {
		t.Fatalf("store threshold = %v, want 0.5", live)
	}
	s.broadcastHideFinishedAge()
}
