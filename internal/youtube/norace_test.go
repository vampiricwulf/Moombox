//go:build !race

package youtube

// raceEnabled is false in an ordinary build — see race_test.go.
const raceEnabled = false
