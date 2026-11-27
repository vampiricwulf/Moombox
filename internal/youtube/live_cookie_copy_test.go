package youtube

import (
	"os"
	"path/filepath"
	"testing"
)

// liveCookieFile returns the cookie file a live test should load for the path
// in env, or "" when env is unset. The jar writes renewed cookies back to the
// file it loaded, so a path given by hand is first copied into t.TempDir and
// the copy is loaded: the file named is never written. The live-test runner
// (.github/scripts/live-tests.sh) hands each package its own private copy and
// says so with MOOMBOX_LIVE_COOKIES_PRIVATE=1; that copy is loaded where it
// is, because the runner's redactor watches it for the renewed values it
// must mask. Nothing from the file is printed, the copy's path included.
func liveCookieFile(t *testing.T, env string) string {
	t.Helper()
	p := os.Getenv(env)
	if p == "" || os.Getenv("MOOMBOX_LIVE_COOKIES_PRIVATE") == "1" {
		return p
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read the cookie file named by %s (contents not shown)", env)
	}
	cp := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cp, data, 0o600); err != nil {
		t.Fatalf("copy the cookie file named by %s: %v", env, err)
	}
	return cp
}
