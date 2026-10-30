package main

import (
	"os"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/treeguard"
)

// TestMain runs the package's tests through treeguard.Run, which fails the
// run when they leave a file in this directory. Before that it sets GORACE so
// the helper processes these tests re-run this binary as skip the race
// runtime's sleep at exit (atexit_sleep_ms, 1000 by default): under -race
// each one cost over a second for that sleep alone. A race a helper hits is
// still reported as it happens and still fails it; only the second's wait
// for a report from another goroutine still running at exit goes.
//
// It also switches off the cookie directory hardening. The setup-cookie
// tests write cookies.txt through a real cookies.AutoCookieService over a
// t.TempDir, and every such write starts a goroutine nothing joins that runs
// utils.ApplyUserOnlyDACL against that directory — on Windows a real icacls
// still walking the TempDir while the test's cleanup removes it. The
// hardening is internal/cookies' to test, and it does, against fakes.
func TestMain(m *testing.M) {
	cookies.DisableDirTighteningForTesting()
	if g := os.Getenv("GORACE"); !strings.Contains(g, "atexit_sleep_ms") {
		os.Setenv("GORACE", strings.TrimSpace(g+" atexit_sleep_ms=0"))
	}
	os.Exit(treeguard.Run(m))
}
