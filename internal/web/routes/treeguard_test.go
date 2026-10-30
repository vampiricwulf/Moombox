package routes

import (
	"context"
	"os"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/treeguard"
	"github.com/vampiricwulf/Moombox/internal/web"
)

// realImportProbeDuration is the ffprobe-backed importProbeDuration, kept for
// the tests that run the real probe (useRealImportProbe).
var realImportProbeDuration = importProbeDuration

// TestMain runs the package's tests through treeguard.Run, which fails the
// run when they leave a file in this directory.
//
// It lowers the scrypt cost of the passwords these tests hash and verify
// through web.AuthService — the same N for both, which is all a hash needs
// to verify — from about 50 ms a hash (half a second under the race
// detector) to microseconds; internal/web pins the production N.
//
// It also answers the import's duration probe without ffprobe. The recordings
// these tests import are a few bytes that are not media, which the real probe
// answers 0 for — after starting an ffprobe process per import, 67 of them a
// run, which is most of what an import test cost. The tests that are about the
// probe put the real one back (useRealImportProbe).
func TestMain(m *testing.M) {
	web.SetPasswordHashCostForTesting(16)
	importProbeDuration = func(context.Context, string, string) float64 { return 0 }
	os.Exit(treeguard.Run(m))
}

// useRealImportProbe restores the ffprobe-backed duration probe for one test.
func useRealImportProbe(t *testing.T) {
	t.Helper()
	prev := importProbeDuration
	importProbeDuration = realImportProbeDuration
	t.Cleanup(func() { importProbeDuration = prev })
}
