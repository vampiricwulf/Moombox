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

// TestSidecarSolverGojaParity proves byte-for-byte equality between the
// goja fallback (the Go port of ejs, preprocessPlayer, run by getFromPrepared)
// and ejs in the sidecar, on every player fixture in testdata/, for n and for
// signatures: a plain one, one holding '%' (which the player only takes
// percent-escaped, as ejs hands it over), and syntheticSigChallenge, the
// input sig-spec learning sends (owner ruling RV-12). The port is ejs, so a
// fixture it cannot prepare or solve fails the test; it used to be skipped.
//
// One subtest per fixture so a single regression on one player doesn't mask
// the others. SKIPS unless MOOMBOX_LIVE_CIPHER_TEST=1 because of sidecar
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

	sigIns := []string{
		"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_",
		"AOq0QJ8wRAIgX%2Fu7DlT%3D=Zk-HmAf%9cQ_ebd%dRsFtYvQyAiB0eIyc8E9zVbn2xL%0",
		syntheticSigChallenge(104),
	}
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

			prepared, _, err := preprocessPlayer(playerJS)
			if err != nil {
				t.Fatalf("goja preprocessPlayer: %v", err)
			}
			gojaSolvers, err := getFromPrepared(prepared)
			if err != nil {
				t.Fatalf("goja getFromPrepared: %v", err)
			}
			src := &fixedPlayerSource{playerID: playerID, js: playerJS}
			solver := NewSidecarSolver(s, src)

			gojaN, err := gojaSolvers.DecryptN(nIn)
			if err != nil {
				t.Fatalf("goja n: %v", err)
			}
			ejsN, err := solver.N(ctx, playerID, nIn)
			if err != nil {
				t.Fatalf("ejs n: %v", err)
			}
			if ejsN != gojaN {
				t.Errorf("n parity mismatch on %s:\n  goja: %q\n  ejs:  %q", playerID, gojaN, ejsN)
			}
			for _, sigIn := range sigIns {
				gojaSig, err := gojaSolvers.DecryptSig(sigIn)
				if err != nil {
					t.Errorf("goja sig(%q): %v", sigIn, err)
					continue
				}
				ejsSig, err := solver.Sig(ctx, playerID, sigIn)
				if err != nil {
					t.Errorf("ejs sig(%q): %v", sigIn, err)
					continue
				}
				if ejsSig != gojaSig {
					t.Errorf("sig parity mismatch on %s for %q:\n  goja: %q\n  ejs:  %q", playerID, sigIn, gojaSig, ejsSig)
				}
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

// tvFixtureSource serves a main body ejs cannot prepare and, as the tv body,
// a real player.
type tvFixtureSource struct {
	fixedPlayerSource
	tvJS string
}

func (f *tvFixtureSource) TVPlayerJS(_ context.Context, playerID string) (string, error) {
	if playerID != f.playerID {
		return "", os.ErrNotExist
	}
	return f.tvJS, nil
}

// TestSidecarSolverLiveTVRetry runs the main-then-tv retry (owner ruling
// RV-11) against a real sidecar: the main body is one ejs throws on while
// preparing it, the tv body is the cb017549 fixture. The solve must be
// answered, with the answer the fixture gives as a main body, and the
// sidecar's store must hold the prepared copy under the "<id>-tv" key and
// none under the bare id. That is what shows the real sidecar takes the
// suffixed key and keeps the two bodies apart with no change on its side.
// SKIPS unless MOOMBOX_LIVE_CIPHER_TEST=1.
func TestSidecarSolverLiveTVRetry(t *testing.T) {
	if os.Getenv("MOOMBOX_LIVE_CIPHER_TEST") != "1" {
		t.Skip("set MOOMBOX_LIVE_CIPHER_TEST=1 to run the live cipher solver test")
	}
	if len(bgembed.EmbeddedNode) == 0 || len(bgembed.SidecarTarGz) == 0 {
		t.Skip("sidecar embed blobs missing; run `go run ./tools/fetch-node` and `node bgutil-sidecar/build.mjs` first")
	}
	playerJS, err := os.ReadFile(filepath.Join("testdata", "player_cb017549.js"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	cacheDir := t.TempDir()
	s := sidecar.New(sidecar.Config{
		CacheDir:       cacheDir,
		StartupTimeout: 30 * time.Second,
		RequestTimeout: 30 * time.Second,
		Logger:         &liveLogger{t: t},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("sidecar Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })

	const nIn = "abcdefghij_12345"
	want, err := NewSidecarSolver(s, &fixedPlayerSource{playerID: "aaaaaaaa", js: string(playerJS)}).N(ctx, "aaaaaaaa", nIn)
	if err != nil {
		t.Fatalf("control N on the fixture as a main body: %v", err)
	}

	src := &tvFixtureSource{
		fixedPlayerSource: fixedPlayerSource{playerID: "cb017549", js: "var notAPlayer = 1;"},
		tvJS:              string(playerJS),
	}
	solver := newSidecarSolverWith(s, src)
	got, err := solver.N(ctx, "cb017549", nIn)
	if err != nil {
		t.Fatalf("N with a main body ejs cannot prepare: %v", err)
	}
	if got != want {
		t.Errorf("N from the tv body = %q, want %q, the fixture's own answer", got, want)
	}
	if !solver.mem.usesTV("cb017549") {
		t.Errorf("the player is not remembered as a tv one")
	}
	stored, err := filepath.Glob(filepath.Join(cacheDir, "players", "cb017549*"))
	if err != nil || len(stored) != 1 || !strings.HasPrefix(filepath.Base(stored[0]), "cb017549-tv.") {
		t.Errorf("prepared copies of the player in the sidecar's store = %v (%v), want one, under the cb017549-tv key", stored, err)
	}
}
