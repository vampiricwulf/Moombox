package sidecar

import (
	"os"
	"path/filepath"
	"testing"
)

// requireCipherFixtureEnv turns a missing cipher fixture from a skip into a
// failure. CI sets it only after .github/scripts/fetch-cipher-fixture.sh put
// the file in place, so these tests cannot skip there unnoticed.
const requireCipherFixtureEnv = "MOOMBOX_REQUIRE_CIPHER_FIXTURE"

// cipherFixturePath is the real YouTube player the solve tests feed a real
// child. internal/cipher/testdata is gitignored.
var cipherFixturePath = filepath.Join("..", "..", "cipher", "testdata", "player_cb017549.js")

// readCipherFixture returns the player fixture, or skips the test without it
// (fails instead when requireCipherFixtureEnv is "1").
func readCipherFixture(t testing.TB) []byte {
	t.Helper()
	return readFixtureAt(t, cipherFixturePath)
}

func readFixtureAt(t testing.TB, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err == nil {
		return data
	}
	const fetch = "bash .github/scripts/fetch-cipher-fixture.sh downloads it"
	if os.Getenv(requireCipherFixtureEnv) == "1" {
		t.Fatalf("cipher fixture missing with %s=1: %v (%s)", requireCipherFixtureEnv, err, fetch)
		return nil
	}
	t.Skipf("cipher fixture missing: %v (%s)", err, fetch)
	return nil
}

// fixtureTB records what readFixtureAt asked of the test instead of ending it.
type fixtureTB struct {
	testing.TB
	failed, skipped bool
}

func (f *fixtureTB) Helper()               {}
func (f *fixtureTB) Fatalf(string, ...any) { f.failed = true }
func (f *fixtureTB) Skipf(string, ...any)  { f.skipped = true }

// A missing fixture skips, unless CI's switch is set, when it fails: the
// switch is what keeps the real-player solve from skipping unnoticed there.
func TestMissingCipherFixtureSkipsUnlessRequired(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "player_cb017549.js")

	t.Setenv(requireCipherFixtureEnv, "")
	var tb fixtureTB
	if readFixtureAt(&tb, missing) != nil || !tb.skipped || tb.failed {
		t.Errorf("without the switch: skipped=%v failed=%v, want a skip", tb.skipped, tb.failed)
	}

	t.Setenv(requireCipherFixtureEnv, "1")
	tb = fixtureTB{}
	if readFixtureAt(&tb, missing) != nil || !tb.failed || tb.skipped {
		t.Errorf("with the switch: skipped=%v failed=%v, want a failure", tb.skipped, tb.failed)
	}
}
