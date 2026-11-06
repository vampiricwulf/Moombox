package main

import (
	"os"
	"strings"
	"testing"
)

// TestWireRoutesInstallsThePanicLogger: routes.SetPanicLogger is how the
// restart, update-apply and setup post-save goroutines report a recovered
// panic, and nothing in production called it, so those panics went to raw
// stderr — missing from moombox.log and drawn over the TUI. Pinned by source,
// like the other wiring checks here: wireRoutes needs the whole run state.
//
// Mutant: drop the SetPanicLogger call.
func TestWireRoutesInstallsThePanicLogger(t *testing.T) {
	src, err := os.ReadFile("routes_wiring.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func (s *runState) wireRoutes() func() {")
	if start < 0 {
		t.Fatal("wireRoutes not found")
	}
	install := strings.Index(body[start:], "routes.SetPanicLogger(s.log)")
	first := strings.Index(body[start:], "routes.JobRoutes(")
	if install < 0 || first < 0 || install > first {
		t.Error("wireRoutes must install the panic logger before it registers the first route")
	}
}

// TestWireRoutesRegistersTheChaptersRoute: GET /api/jobs/{id}/chapters is
// registered with its deps filled in — the player fetches it on every open,
// and a route left unwired answers 404 (the player then shows no chapters,
// silently, which is exactly what makes a missing wire easy to miss). Pinned
// by source, as the panic-logger check above is.
//
// Mutants: the ChapterRoutes call dropped; its Logger left nil.
func TestWireRoutesRegistersTheChaptersRoute(t *testing.T) {
	src, err := os.ReadFile("routes_wiring.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "routes.ChapterRoutes(s.r, &routes.ChapterRouteDeps{")
	if start < 0 {
		t.Fatal("wireRoutes does not register routes.ChapterRoutes")
	}
	end := strings.Index(body[start:], "})")
	call := body[start : start+end]
	for _, field := range []string{"DB:     s.db", "Store:  s.configStore", "Logger: s.log"} {
		if !strings.Contains(call, field) {
			t.Errorf("ChapterRoutes deps lack %q", field)
		}
	}
}
