//go:build race

package youtube

// raceEnabled is true in a `go test -race` build. The allocation-ceiling
// tests read it: the race detector adds its own allocations to every call it
// instruments, so a budget measured without it is not a statement about this
// code under it (measured: 17-19 vs a ceiling of 16). Skipping them is what
// lets the whole package run under `-race` unfiltered, which is the gate
// (close-review Finding 5).
const raceEnabled = true
