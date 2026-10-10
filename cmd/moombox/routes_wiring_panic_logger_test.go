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
