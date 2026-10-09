//go:build race

package tui

// raceEnabled is true in a `go test -race` build. TestFrameCostAtLogCap reads
// it, and not because the detector skews its allocation COUNTS (measured
// under -race: 30 cached, 5,921 uncached — the same budgets hold). It is the
// window: steadyStateAllocs must fit a warm-up plus frameProbeRuns renders
// inside ONE wall-clock second, and the uncached log-panel probe is 65 full
// renders of a 1,000-line buffer. The whole test takes 4.6 s under the
// detector against 0.26 s without it; alone that still fits, but beside the
// rest of the module in CI's whole-module race step it did not (2026-10-09:
// "20 attempts all straddled a second boundary", passing alone on the rerun).
// Skipping it under -race is what lets the whole module run there unfiltered;
// the plain `go test` step still runs it on both legs.
const raceEnabled = true
