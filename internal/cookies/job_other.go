//go:build !windows

package cookies

import "os"

// processJob is a no-op on non-Windows platforms.
type processJob struct{}

func newProcessJob() (*processJob, error) { return nil, nil }
func (j *processJob) assign(p *os.Process) error { return nil }
func (j *processJob) close()                      {}
