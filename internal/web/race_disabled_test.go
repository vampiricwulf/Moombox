//go:build !race

package web

// raceEnabled is false in the ordinary build — the one CI runs. See
// race_enabled_test.go for why the constant exists at all.
const raceEnabled = false
