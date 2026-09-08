//go:build windows

package cookies

import "testing"

// newQueryableTestJob returns a processJob that answers queryable() true. On
// Windows a fresh job object is a real handle, so the plain constructor is
// enough; only close() can turn queryable() false.
func newQueryableTestJob(t *testing.T) *processJob {
	t.Helper()
	job, err := newProcessJob()
	if err != nil {
		t.Fatalf("newProcessJob: %v", err)
	}
	if !job.queryable() {
		t.Fatal("premise: a fresh job object must be queryable")
	}
	return job
}
