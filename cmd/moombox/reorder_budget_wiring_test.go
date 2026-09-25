package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/logger"
)

// TestApplyReorderBudgetReachesTheEngine: the applier is the ONLY path from
// the two config keys to the engine's process-wide budget, and it converts MB
// to bytes on the way.
//
// MUTANT: pass the MB values straight to ConfigureReorder — the limits read
// 1024 and 4096 bytes, and every download blocks on a ceiling a thousandth
// the size the operator asked for. MUTANT: swap the two arguments — the
// second assertion fails.
func TestApplyReorderBudgetReachesTheEngine(t *testing.T) {
	perJob0, total0 := engine.ReorderLimits()
	t.Cleanup(func() { engine.ConfigureReorder(perJob0, total0) })

	s := &runState{} // log deliberately nil: the applier must tolerate early wiring
	s.applyReorderBudget(config.DownloaderConfig{ReorderBufferMB: 1024, ReorderBudgetMB: 4096})

	perJob, total := engine.ReorderLimits()
	if perJob != 1024<<20 {
		t.Errorf("per-job limit = %d, want %d bytes", perJob, 1024<<20)
	}
	if total != 4096<<20 {
		t.Errorf("total limit = %d, want %d bytes", total, 4096<<20)
	}

	// 0 is unbounded, not "leave it alone".
	s.applyReorderBudget(config.DownloaderConfig{})
	if perJob, total = engine.ReorderLimits(); perJob != 0 || total != 0 {
		t.Errorf("limits after an all-zero config = (%d, %d), want (0, 0) — 0 means unbounded", perJob, total)
	}
}

// TestApplyReorderBudgetWarnsWhenTheClampFires: a per-job ceiling above the
// process budget is incoherent config. ReorderLimitBytes reconciles it; the
// operator has to be told, or they spend an afternoon wondering why
// reorder_buffer_mb = 8192 behaves like 1024.
//
// MUTANT: drop the clamped branch — no line names the two keys and the first
// assertion fails. MUTANT: warn unconditionally — the coherent pair below
// also warns and the second assertion fails.
func TestApplyReorderBudgetWarnsWhenTheClampFires(t *testing.T) {
	perJob0, total0 := engine.ReorderLimits()
	t.Cleanup(func() { engine.ConfigureReorder(perJob0, total0) })

	log, err := logger.New(filepath.Join(t.TempDir(), "reorder.log"), "info", 4096, 1)
	if err != nil {
		t.Fatalf("logger.New: %v", err)
	}
	t.Cleanup(func() { log.Close() })
	s := &runState{log: log}

	s.applyReorderBudget(config.DownloaderConfig{ReorderBufferMB: 8192, ReorderBudgetMB: 1024})
	if !linesMention(log.GetRecentLines(), "reorder_buffer_mb", "reorder_budget_mb") {
		t.Errorf("the clamp produced no line naming both keys; log was:\n%s",
			strings.Join(log.GetRecentLines(), "\n"))
	}
	if perJob, _ := engine.ReorderLimits(); perJob != 1024<<20 {
		t.Errorf("per-job limit = %d, want it clamped to the budget (%d)", perJob, 1024<<20)
	}

	log2, err := logger.New(filepath.Join(t.TempDir(), "reorder2.log"), "info", 4096, 1)
	if err != nil {
		t.Fatalf("logger.New: %v", err)
	}
	t.Cleanup(func() { log2.Close() })
	s2 := &runState{log: log2}
	s2.applyReorderBudget(config.DownloaderConfig{ReorderBufferMB: 1024, ReorderBudgetMB: 4096})
	if linesMention(log2.GetRecentLines(), "reorder_buffer_mb", "reorder_budget_mb") {
		t.Errorf("a coherent pair warned anyway; log was:\n%s", strings.Join(log2.GetRecentLines(), "\n"))
	}
}

// linesMention reports whether any single recent line contains every needle.
func linesMention(lines []string, needles ...string) bool {
	for _, line := range lines {
		hit := true
		for _, n := range needles {
			if !strings.Contains(line, n) {
				hit = false
				break
			}
		}
		if hit {
			return true
		}
	}
	return false
}

// TestEveryReorderBudgetCallSiteIsWired pins the three places the applier has
// to be reached from. Structural, for the reason
// tui_wiring_openfolder_test.go is: initServices builds the whole process and
// runTUI runs a bubbletea program, so this package cannot drive either, and
// each seam is one line's presence.
//
// MUTANT: delete the services.go line — the budget is whatever the engine
// constant says until the operator's first save, so a fresh arm64 boot runs
// at 256 MB per job with NO process cap at all. MUTANT: delete the
// tui_wiring.go line — a TUI save writes the file and changes nothing until
// restart, silently. MUTANT: delete the routes_wiring.go line — same, for the
// dashboard.
func TestEveryReorderBudgetCallSiteIsWired(t *testing.T) {
	for _, tc := range []struct{ file, want, why string }{
		{"services.go", "s.applyReorderBudget(cfg.Downloader)",
			"boot never configures the engine's reorder budget, so the config keys do nothing until the first save"},
		{"tui_wiring.go", "s.applyReorderBudget(snap.Downloader)",
			"a TUI settings save never reaches the engine's reorder budget"},
		{"routes_wiring.go", "OnReorderBudgetChange:       s.applyReorderBudget",
			"a dashboard settings save never reaches the engine's reorder budget"},
	} {
		src, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatalf("read %s: %v", tc.file, err)
		}
		// Whitespace-insensitive: gofmt realigns the struct literal keys.
		normalize := func(s string) string { return strings.Join(strings.Fields(s), " ") }
		if !strings.Contains(normalize(string(src)), normalize(tc.want)) {
			t.Errorf("%s does not contain %q — %s", tc.file, tc.want, tc.why)
		}
	}
}
