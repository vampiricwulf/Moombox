//go:build race

package web

// raceEnabled is true in this file's build, which the `race` tag selects.
// TestCompressionReusesGzipWriters measures allocations, and the race
// detector's shadow memory is charged to the allocating goroutine: the same
// pooled-writer workload that costs ~4 KB/op plain measures ~291 KB/op under
// -race. The gate has to move with it or the suite is red for everyone who
// runs `go test -race` (sweep R5/T3).
const raceEnabled = true
