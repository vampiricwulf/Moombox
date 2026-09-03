//go:build linux

package cookies

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestLivePgroupSeesAndKillsARealGroup is a SMOKE TEST and nothing depends on
// it.
//
// IT IS NOT RUN IN CI HERE, and that is deliberate rather than an oversight.
// The release workflow cross-compiles the Linux binaries from ubuntu and never
// runs `go test`; the machine this project is developed on is Windows; and the
// owner's ruling for this arc is explicit that there is no Linux live gate and
// that a user's bug report is the gate. Every DECISION this file's production
// code makes is already pinned cross-platform in job_pgroup_test.go against a
// fake process table. What is left here — Setpgid actually taking effect, /proc
// actually parsing, kill(-pgid) actually landing — needs a real kernel, so it
// sits behind an env var for whoever has one:
//
//	MOOMBOX_LIVE_PGROUP=1 go test -count=1 -run TestLivePgroup ./internal/cookies/
//
// It launches `sh -c "sleep 30"` and kills it. It never touches a browser, a
// profile or a cookie file.
func TestLivePgroupSeesAndKillsARealGroup(t *testing.T) {
	if os.Getenv("MOOMBOX_LIVE_PGROUP") != "1" {
		t.Skip("set MOOMBOX_LIVE_PGROUP=1 on a Linux box to run this; it is not a CI gate")
	}

	cmd := exec.Command("/bin/sh", "-c", "sleep 30")
	configureCmdSysProcAttr(cmd)
	if err := cmd.Start(); err != nil {
		t.Skipf("could not start /bin/sh: %v", err)
	}

	job, err := newProcessJob()
	if err != nil {
		t.Fatalf("newProcessJob: %v", err)
	}
	if err := job.assign(cmd.Process); err != nil {
		t.Fatalf("assign: %v — Setpgid did not put the child in its own group, "+
			"or /proc could not be read", err)
	}
	if !job.queryable() {
		t.Fatal("queryable() is false after a successful assign")
	}

	n, err := job.activeProcesses()
	if err != nil {
		t.Fatalf("activeProcesses: %v", err)
	}
	if n < 1 {
		t.Fatalf("activeProcesses() = %d for a group that is provably running", n)
	}

	if err := killTrackedProcesses(job); err != nil {
		t.Fatalf("killTrackedProcesses: %v", err)
	}
	// Reap the direct child BEFORE counting. Until it is waited on it is a
	// zombie whose stat line still carries the pgrp; the parser skips state Z
	// (decision 8), so this is belt and braces — but a smoke test that leaned
	// on that would be pinning the wrong thing. A deferred Wait here would
	// leave the zombie in place for the whole loop below.
	_ = cmd.Wait()

	deadline := time.Now().Add(5 * time.Second)
	for {
		n, err := job.activeProcesses()
		if err != nil {
			t.Fatalf("activeProcesses after the kill: %v", err)
		}
		if n == 0 {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("%d processes still in the group 5s after SIGKILL", n)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
