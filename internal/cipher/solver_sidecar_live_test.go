package cipher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bgembed "github.com/vampiricwulf/Moombox/internal/bgutils/embed"
	"github.com/vampiricwulf/Moombox/internal/bgutils/sidecar"
)

// TestSidecarSolverLive runs the FULL solver path against a real
// sidecar subprocess loading the cb017549 player fixture. SKIPS unless
// MOOMBOX_LIVE_CIPHER_TEST=1 is set so it doesn't slow down the default
// test run (sidecar startup + ejs preprocessing takes ~10-15s on a
// warm cache).
//
// PASS = sig and n produce non-empty, non-identity transformations
// for distinguishable inputs.
func TestSidecarSolverLive(t *testing.T) {
	if os.Getenv("MOOMBOX_LIVE_CIPHER_TEST") != "1" {
		t.Skip("set MOOMBOX_LIVE_CIPHER_TEST=1 to run the live cipher solver test")
	}
	if len(bgembed.EmbeddedNode) == 0 || len(bgembed.SidecarTarGz) == 0 {
		t.Skip("sidecar embed blobs missing; run `go run ./tools/fetch-node` and `node bgutil-sidecar/build.mjs` first")
	}

	playerJSPath := filepath.Join("testdata", "player_cb017549.js")
	playerJS, err := os.ReadFile(playerJSPath)
	if err != nil {
		t.Fatalf("read fixture %s: %v", playerJSPath, err)
	}

	// Construct a real sidecar with the embed blobs.
	s := sidecar.New(sidecar.Config{
		CacheDir:       t.TempDir(),
		StartupTimeout: 30 * time.Second,
		RequestTimeout: 30 * time.Second,
		Logger:         &liveLogger{t: t},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("sidecar Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })

	// fixedPlayerSource serves the loaded fixture for one playerID.
	src := &fixedPlayerSource{playerID: "cb017549", js: string(playerJS)}
	solver := NewSidecarSolver(s, src)

	sigIn := "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ-_"
	nIn := "abcdefghij_12345"

	gotSig, err := solver.Sig(ctx, "cb017549", sigIn)
	if err != nil {
		t.Fatalf("Sig live: %v", err)
	}
	if gotSig == "" || gotSig == sigIn {
		t.Errorf("sig should produce a non-identity transformation; got %q", gotSig)
	}
	t.Logf("sig: %q -> %q", sigIn, gotSig)

	gotN, err := solver.N(ctx, "cb017549", nIn)
	if err != nil {
		t.Fatalf("N live: %v", err)
	}
	if gotN == "" || gotN == nIn {
		t.Errorf("n should produce a non-identity transformation; got %q", gotN)
	}
	t.Logf("n: %q -> %q", nIn, gotN)
}

// TestSidecarSolverGojaParity proves byte-for-byte equality between
// the goja and EJS-via-sidecar outputs across every player fixture
// goja can statically handle. The parity claim only applies on
// fixtures where goja's AST/regex extraction succeeds (sig is
// extractable + n is extractable); on fixtures where goja can't
// reach a sig algorithm (e.g. the cb017549-family), we log and
// skip the comparison since there's no goja output to compare
// against.
//
// One subtest per fixture so a single regression on one player
// doesn't mask the others.
//
// SKIPS unless MOOMBOX_LIVE_CIPHER_TEST=1 because of sidecar
// startup cost.
func TestSidecarSolverGojaParity(t *testing.T) {
	if os.Getenv("MOOMBOX_LIVE_CIPHER_TEST") != "1" {
		t.Skip("set MOOMBOX_LIVE_CIPHER_TEST=1 to run the goja-vs-ejs parity test")
	}
	if len(bgembed.EmbeddedNode) == 0 || len(bgembed.SidecarTarGz) == 0 {
		t.Skip("sidecar embed blobs missing; run `go run ./tools/fetch-node` and `node bgutil-sidecar/build.mjs` first")
	}

	fixtures, err := filepath.Glob(filepath.Join("testdata", "player_*.js"))
	if err != nil {
		t.Fatalf("glob testdata: %v", err)
	}
	if len(fixtures) == 0 {
		t.Skip("no player fixtures in testdata/ — install at least one and retry")
	}

	// One sidecar shared across subtests — startup cost is amortised
	// and per-player cache stays warm across fixtures (each fixture
	// has a unique playerID embedded in its filename).
	s := sidecar.New(sidecar.Config{
		CacheDir:       t.TempDir(),
		StartupTimeout: 30 * time.Second,
		RequestTimeout: 30 * time.Second,
		Logger:         &liveLogger{t: t},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("sidecar Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })

	sigIn := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	nIn := "abcdefghij_12345"

	for _, fixturePath := range fixtures {
		// playerID extracted from the filename: testdata/player_<id>.js
		// → <id>. Used as both the parity-test subtest name and the
		// sidecar-side player cache key.
		base := filepath.Base(fixturePath)
		playerID := strings.TrimSuffix(strings.TrimPrefix(base, "player_"), ".js")

		t.Run(playerID, func(t *testing.T) {
			data, err := os.ReadFile(fixturePath)
			if err != nil {
				t.Skipf("read %s: %v", fixturePath, err)
			}
			playerJS := string(data)

			// --- Goja path ---
			prepared, err := preprocessPlayerFull(playerJS)
			if err != nil {
				t.Skipf("goja preprocessPlayerFull failed (skip parity for this fixture): %v", err)
			}
			gojaSolvers, err := getFromPrepared(prepared)
			if err != nil {
				t.Skipf("goja getFromPrepared failed: %v", err)
			}
			if gojaSolvers.Sig == nil || gojaSolvers.N == nil {
				t.Skipf("goja could not extract sig or n for this player (HasSig=%v HasN=%v); parity claim doesn't apply",
					gojaSolvers.Sig != nil, gojaSolvers.N != nil)
			}

			gojaSig, err := gojaSolvers.Sig(sigIn)
			if err != nil {
				t.Skipf("goja sig solve errored: %v", err)
			}
			gojaN, err := gojaSolvers.N(nIn)
			if err != nil {
				t.Skipf("goja n solve errored: %v", err)
			}
			t.Logf("goja sig: %q -> %q", sigIn, gojaSig)
			t.Logf("goja n:   %q -> %q", nIn, gojaN)

			// --- Sidecar (EJS) path ---
			src := &fixedPlayerSource{playerID: playerID, js: playerJS}
			solver := NewSidecarSolver(s, src)

			ejsSig, err := solver.Sig(ctx, playerID, sigIn)
			if err != nil {
				t.Fatalf("ejs sig solve: %v", err)
			}
			ejsN, err := solver.N(ctx, playerID, nIn)
			if err != nil {
				t.Fatalf("ejs n solve: %v", err)
			}
			t.Logf("ejs  sig: %q -> %q", sigIn, ejsSig)
			t.Logf("ejs  n:   %q -> %q", nIn, ejsN)

			if ejsSig != gojaSig {
				t.Errorf("sig parity mismatch on %s:\n  goja: %q\n  ejs:  %q", playerID, gojaSig, ejsSig)
			}
			if ejsN != gojaN {
				t.Errorf("n parity mismatch on %s:\n  goja: %q\n  ejs:  %q", playerID, gojaN, ejsN)
			}
		})
	}
}

type fixedPlayerSource struct {
	playerID string
	js       string
}

func (f *fixedPlayerSource) PlayerJS(playerID string) (string, error) {
	if playerID != f.playerID {
		return "", os.ErrNotExist
	}
	return f.js, nil
}

// RemovePlayerJS satisfies cipher.PlayerSource. The live sidecar tests
// don't exercise the reactive-invalidation path; this is a no-op so the
// interface stays satisfied.
func (f *fixedPlayerSource) RemovePlayerJS(playerID string) error {
	return nil
}

type liveLogger struct{ t *testing.T }

func (l *liveLogger) Debug(msg string, args ...any) { l.t.Logf("[DEBUG] "+msg+" %v", args) }
func (l *liveLogger) Info(msg string, args ...any)  { l.t.Logf("[INFO ] "+msg+" %v", args) }
func (l *liveLogger) Warn(msg string, args ...any)  { l.t.Logf("[WARN ] "+msg+" %v", args) }
func (l *liveLogger) Error(msg string, args ...any) { l.t.Logf("[ERROR] "+msg+" %v", args) }
