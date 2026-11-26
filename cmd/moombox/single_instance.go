package main

import "errors"

// errInstanceRunning marks an instance-lock refusal that means another Moombox
// holds the lock (or the last one is still shutting down). Only that answer
// earns the launcher's "wait a few seconds and try again" hint: any other
// failure to take the lock (a lock directory that cannot be created, a
// filesystem without flock) is the environment, and waiting changes nothing.
var errInstanceRunning = errors.New("another moombox instance is already running")

// instanceRunningError is a refusal whose text each platform words itself
// (the operator docs quote those texts) and which errors.Is matches against
// errInstanceRunning.
type instanceRunningError string

func (e instanceRunningError) Error() string        { return string(e) }
func (e instanceRunningError) Is(target error) bool { return target == errInstanceRunning }

// instanceLockRefusal is what launchAndSupervise prints, line by line, when
// it cannot take the instance lock.
func instanceLockRefusal(err error) []string {
	lines := []string{err.Error()}
	if errors.Is(err, errInstanceRunning) {
		lines = append(lines, "If you believe this is in error (the previous instance crashed), wait a few seconds and try again.")
	}
	return lines
}
