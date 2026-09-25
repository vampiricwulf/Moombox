package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestRecoverAsidesWiringForwardsTheWorker pins the two one-line delegates the
// set-aside verb hangs off, in the same idiom as
// TestOpenFolderChordUsesTheSharedDetachedSpawn: by reading tui_wiring.go as
// text.
//
// Structural, and for the same reason that one is: every `On…` body here is
// installed on a tui.App that runTUI hands to a live tea.Program, over a real
// database, a real config store and a real DownloadWorker — there is no seam
// to call these closures through, so nothing behavioural reaches them.
//
// THE MUTANTS, both of which survived the arc's whole suite:
//   - `OnRecoverAsides` returning nil instead of the worker's answer: the
//     chord reports success for a refusal the worker never accepted, and the
//     operator watches a recovery that is not running.
//   - the orphan mapping dropping `Asides: e.Asides`: the TUI's orphan overlay
//     stops naming the set-aside recordings a staging dir holds, which is the
//     one thing that tells captured footage from scratch space before Delete.
func TestRecoverAsidesWiringForwardsTheWorker(t *testing.T) {
	src, err := os.ReadFile("tui_wiring.go")
	if err != nil {
		t.Fatalf("read tui_wiring.go: %v", err)
	}
	text := strings.ReplaceAll(string(src), "\r\n", "\n")

	if !strings.Contains(text, "return s.dlWorker.RecoverAsides(jobID)") {
		t.Error("OnRecoverAsides does not return s.dlWorker.RecoverAsides(jobID) — a swallowed refusal " +
			"tells the operator a recovery started that the worker declined")
	}
	if !strings.Contains(text, "s.dlWorker.Asides(jobID)") {
		t.Error("JobAsides does not probe s.dlWorker.Asides(jobID) — the details panel and the A S gate " +
			"would both read an empty summary for every job")
	}
	// Whitespace-tolerant: gofmt aligns this key with its neighbours, so the
	// run of spaces moves whenever a field is added to OrphanedFileEntry.
	if !regexp.MustCompile(`(?m)^\s*Asides:\s+e\.Asides,$`).MatchString(text) {
		t.Error("the tui.OrphanedFileEntry mapping drops OrphanedEntry.Asides — the orphan overlay " +
			"stops counting the set-aside recordings a staging orphan holds")
	}
}
