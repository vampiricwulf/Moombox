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
