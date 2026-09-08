//go:build !windows && !linux

package cookies

import "testing"

// newQueryableTestJob: process jobs are inert on this platform (job_other.go
// answers queryable() false for every job), so the kill-ordering tests have
// nothing to observe here.
func newQueryableTestJob(t *testing.T) *processJob {
	t.Helper()
	t.Skip("process jobs are inert on this platform; the kill ordering is observable only on Windows and Linux")
	return nil
}
