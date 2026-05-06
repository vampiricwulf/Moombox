package cipher

import (
	"context"
	"os"
	"path/filepath"
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

// TestSidecarSolverGojaParity74edf1a3 proves the EJS-via-sidecar path
// produces the SAME sig and n output as the in-process goja extractor
// for a player both can handle. This is the strongest correctness
// signal short of feeding decrypted URLs into a real YouTube playback
// request: if EJS and goja agree byte-for-byte on a player goja can
// statically extract, EJS isn't producing junk on the players where
// goja fails to extract — it's producing the actual algorithm output
// that the player would compute itself.
//
// Uses player_74edf1a3.js because:
//   - goja's static AST/regex extraction works on it (TestPreprocessPlayer74edf1a3
//     in extractor_test.go validates this directly).
//   - It's already in the testdata convention (gitignored fixture; tests
//     skip when missing).
//
// SKIPS unless MOOMBOX_LIVE_CIPHER_TEST=1 is set (sidecar startup cost
// makes this too slow for default test runs).
func TestSidecarSolverGojaParity74edf1a3(t *testing.T) {
	if os.Getenv("MOOMBOX_LIVE_CIPHER_TEST") != "1" {
		t.Skip("set MOOMBOX_LIVE_CIPHER_TEST=1 to run the goja-vs-ejs parity test")
	}
	if len(bgembed.EmbeddedNode) == 0 || len(bgembed.SidecarTarGz) == 0 {
		t.Skip("sidecar embed blobs missing; run `go run ./tools/fetch-node` and `node bgutil-sidecar/build.mjs` first")
	}

	playerJSPath := filepath.Join("testdata", "player_74edf1a3.js")
	data, err := os.ReadFile(playerJSPath)
	if err != nil {
		t.Skipf("fixture %s not available: %v", playerJSPath, err)
	}
	playerJS := string(data)

	// Distinguishable inputs — different lengths so any "preserves length
	// without transforming" bug shows up; mix of upper/lower/digits/dashes
	// covers the websafe-base64 alphabet sig algorithms operate on.
	sigIn := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	nIn := "abcdefghij_12345"

	// --- Goja path ---
	// Bypass the player-cache HTTP fetch by calling the lower-level
	// preprocessor directly with the fixture bytes. Mirrors what
	// TestPreprocessPlayer74edf1a3 does in extractor_test.go.
	prepared, err := preprocessPlayerFull(playerJS)
	if err != nil {
		t.Fatalf("goja preprocessPlayerFull: %v", err)
	}
	gojaSolvers, err := getFromPrepared(prepared)
	if err != nil {
		t.Fatalf("goja getFromPrepared: %v", err)
	}
	if gojaSolvers.Sig == nil {
		t.Fatal("goja sig solver nil on player_74edf1a3.js — fixture / extraction regressed")
	}
	if gojaSolvers.N == nil {
		t.Fatal("goja n solver nil on player_74edf1a3.js — fixture / extraction regressed")
	}
	gojaSigOut, err := gojaSolvers.Sig(sigIn)
	if err != nil {
		t.Fatalf("goja sig solve: %v", err)
	}
	gojaNOut, err := gojaSolvers.N(nIn)
	if err != nil {
		t.Fatalf("goja n solve: %v", err)
	}
	t.Logf("goja sig: %q -> %q", sigIn, gojaSigOut)
	t.Logf("goja n:   %q -> %q", nIn, gojaNOut)

	// --- Sidecar (EJS) path ---
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

	src := &fixedPlayerSource{playerID: "74edf1a3", js: playerJS}
	solver := NewSidecarSolver(s, src)

	ejsSigOut, err := solver.Sig(ctx, "74edf1a3", sigIn)
	if err != nil {
		t.Fatalf("ejs sig solve: %v", err)
	}
	ejsNOut, err := solver.N(ctx, "74edf1a3", nIn)
	if err != nil {
		t.Fatalf("ejs n solve: %v", err)
	}
	t.Logf("ejs  sig: %q -> %q", sigIn, ejsSigOut)
	t.Logf("ejs  n:   %q -> %q", nIn, ejsNOut)

	// --- Parity assertions ---
	// Byte-for-byte equality is the strong claim. Any divergence on a
	// player both paths can handle would mean EJS or goja disagrees with
	// the player's own URL builder — exactly what we don't want.
	if ejsSigOut != gojaSigOut {
		t.Errorf("sig parity mismatch:\n  goja: %q\n  ejs:  %q", gojaSigOut, ejsSigOut)
	}
	if ejsNOut != gojaNOut {
		t.Errorf("n parity mismatch:\n  goja: %q\n  ejs:  %q", gojaNOut, ejsNOut)
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

type liveLogger struct{ t *testing.T }

func (l *liveLogger) Debug(msg string, args ...any) { l.t.Logf("[DEBUG] "+msg+" %v", args) }
func (l *liveLogger) Info(msg string, args ...any)  { l.t.Logf("[INFO ] "+msg+" %v", args) }
func (l *liveLogger) Warn(msg string, args ...any)  { l.t.Logf("[WARN ] "+msg+" %v", args) }
func (l *liveLogger) Error(msg string, args ...any) { l.t.Logf("[ERROR] "+msg+" %v", args) }
