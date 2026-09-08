//go:build linux

package cookies

import (
	"os/exec"
	"testing"
)

// newQueryableTestJob returns a processJob that answers queryable() true. On
// Linux that means a real child in its own process group: a fresh job knows
// no group until assign() adopts one (job_linux.go), so the Windows-flavoured
// "a real handle starts queryable" premise does not hold here. The child is a
// sleep that the cleanup kills itself — the kill seam is stubbed by the
// ordering tests, so nothing else would.
func newQueryableTestJob(t *testing.T) *processJob {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	configureCmdSysProcAttr(cmd)
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a child process here: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	job, err := newProcessJob()
	if err != nil {
		t.Fatalf("newProcessJob: %v", err)
	}
	if err := job.assign(cmd.Process); err != nil {
		t.Fatalf("assign the child's process group: %v", err)
	}
	if !job.queryable() {
		t.Fatal("premise: a job with an adopted group must be queryable")
	}
	return job
}
