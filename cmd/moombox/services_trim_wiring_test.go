package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestWorkerSharesTheTrimService pins that initServices builds ONE trim
// service and hands it to the download worker, which runs every job's
// post-download trim through it. Structural, for the reason
// TestEveryReorderBudgetCallSiteIsWired is: initServices builds the whole
// process, so this package cannot drive it. The worker's half — the service
// handed in is the one the post-download trim runs through — is
// TestNewDownloadWorkerSharesTheTrimService in internal/worker.
//
// The post-download trim used to build a TrimService of its own, whose
// one-trim-per-job slot the dashboard's and the TUI's could not see: a
// dashboard trim of the range it was encoding started beside it and both
// FFmpegs wrote one .partial.mp4.
//
// MUTANT: drop `TrimService: trimSvc` from the worker's deps — the field is
// optional, so it compiles, and every post-download trim then fails with no
// trim service. MUTANT: hand the worker a second NewTrimService — its slot is
// one the dashboard's cannot see again.
func TestWorkerSharesTheTrimService(t *testing.T) {
	src, err := os.ReadFile("services.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	if n := strings.Count(body, "worker.NewTrimService("); n != 1 {
		t.Fatalf("services.go builds %d trim services, want 1", n)
	}
	newWorker := strings.Index(body, "worker.NewDownloadWorker(")
	if newWorker < 0 {
		t.Fatal("services.go no longer calls worker.NewDownloadWorker — renamed?")
	}
	deps := body[newWorker:]
	if end := strings.Index(deps, "})"); end >= 0 {
		deps = deps[:end]
	}
	if !regexp.MustCompile(`\bTrimService:\s+trimSvc,`).MatchString(deps) {
		t.Errorf("the download worker's deps do not carry the shared trim service:\n%s", deps)
	}
}
