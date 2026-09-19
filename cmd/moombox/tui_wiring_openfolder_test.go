package main

import (
	"os"
	"strings"
	"testing"
)

// TestOpenFolderChordUsesTheSharedDetachedSpawn pins the THIRD open-path call
// site onto the same helper as the other two.
//
// `O F` (app.OnOpenFolder, wired here) carried its own
// windows/darwin/default switch, one of three copies of the same rule in the
// tree — and the copies had already drifted: the dashboard's route spelled
// only the Windows arm until WEB-6, and none of the three reaped its child on
// Unix. Folding this one onto web.OpenPathCommand leaves ONE file-manager
// switch in the tree, and web.StartDetached gives this path the handle release
// (Windows) and the reap (Unix) it never had at all.
//
// Structural, and deliberately so: calling the closure would open a real file
// manager window on the developer's desktop, and its inputs are a live
// database plus a config store.
//
// THE MUTANT: paste the `switch runtime.GOOS { case "windows": …
// exec.Command("explorer", dir) … }` block back — the shared-helper
// assertions fail, and so does the per-program one.
func TestOpenFolderChordUsesTheSharedDetachedSpawn(t *testing.T) {
	src, err := os.ReadFile("tui_wiring.go")
	if err != nil {
		t.Fatalf("read tui_wiring.go: %v", err)
	}
	text := strings.ReplaceAll(string(src), "\r\n", "\n")

	if !strings.Contains(text, "web.OpenPathCommand(dir)") {
		t.Error("the O F chord does not build its command with web.OpenPathCommand — a fourth copy of " +
			"the file-manager switch is exactly how the route ended up Windows-only (WEB-6)")
	}
	if !strings.Contains(text, "web.StartDetached(cmd)") {
		t.Error("the O F chord starts its child without web.StartDetached — it releases nothing on " +
			"Windows and reaps nothing on Unix")
	}
	for _, program := range []string{"explorer", "open", "xdg-open"} {
		if strings.Contains(text, `exec.Command("`+program+`"`) {
			t.Errorf("tui_wiring.go still names %q itself; the one switch lives in "+
				"web.OpenPathCommand", program)
		}
	}
}
