//go:build !windows

package sidecar

import "os"

// processJob is a no-op stub on non-Windows builds. Moombox is Windows-only
// per CLAUDE.md but the package must still compile on other GOOS so cross-
// platform CI / `go test ./...` from a Linux dev box doesn't break.
//
// If/when Moombox grows Linux/macOS support, this file becomes the place
// to plug in a Cgroup-based or SetPgid-based equivalent.
type processJob struct{}

func newProcessJob() (*processJob, error)  { return &processJob{}, nil }
func (j *processJob) assign(*os.Process) error { return nil }
func (j *processJob) close()               {}
