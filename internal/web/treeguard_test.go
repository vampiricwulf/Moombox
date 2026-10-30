package web

import (
	"os"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/treeguard"
)

// defaultScryptCost is scryptCost as the package left it, before TestMain
// lowered it: the cost a running instance spends.
var defaultScryptCost int

// TestMain runs the package's tests through treeguard.Run, which fails the
// run when they leave a file in this directory.
//
// It also lowers the scrypt cost every hash and verify spends. The tests make
// and verify their own hashes, and what they check — the format, the salt,
// the trimmed second attempt, a token's round trip — holds at any N; at the
// production N one scrypt costs about 50 ms, and about half a second under
// the race detector. TestScryptCostDefaultsToScryptN pins the production one.
func TestMain(m *testing.M) {
	defaultScryptCost = SetPasswordHashCostForTesting(testScryptCost)
	os.Exit(treeguard.Run(m))
}

// testScryptCost is the N the tests hash at: the smallest scrypt accepts
// for r=8 is 2, and 16 keeps the work a few microseconds.
const testScryptCost = 16

// TestScryptCostDefaultsToScryptN: a running instance hashes at scryptN, and
// scryptN is 16384. Mutant: initialising scryptCost to anything else.
func TestScryptCostDefaultsToScryptN(t *testing.T) {
	if defaultScryptCost != scryptN || scryptN != 16384 {
		t.Fatalf("passwords hash at N=%d (scryptN %d), want 16384", defaultScryptCost, scryptN)
	}
}
