//go:build windows

package dpapi

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// TestFindBrowserProfilesIn drives the Windows filesystem walk against a
// synthetic LOCALAPPDATA tree so the test runs deterministically on any
// CI box. Layout under tempdir:
//
//	{tmp}\Google\Chrome\User Data\Default\
//	{tmp}\Google\Chrome\User Data\Profile 1\
//	{tmp}\Google\Chrome\User Data\System Profile\   (excluded)
//	{tmp}\Microsoft\Edge\User Data\Default\
//	{tmp}\BraveSoftware\Brave-Browser\User Data\    (no profile subdirs)
//	{tmp}\Vivaldi\User Data\Default\Cache\          (only Cache, but Default dir itself counts)
//
// Expected output: 4 entries — Chrome Default, Chrome Profile 1, Edge
// Default, Vivaldi Default. Brave's empty User Data root produces no
// entries; System Profile is excluded by isChromiumProfileDirName.
func TestFindBrowserProfilesIn(t *testing.T) {
	tmp := t.TempDir()

	mustMkdir(t, filepath.Join(tmp, `Google\Chrome\User Data\Default`))
	mustMkdir(t, filepath.Join(tmp, `Google\Chrome\User Data\Profile 1`))
	mustMkdir(t, filepath.Join(tmp, `Google\Chrome\User Data\System Profile`))
	mustMkdir(t, filepath.Join(tmp, `Microsoft\Edge\User Data\Default`))
	mustMkdir(t, filepath.Join(tmp, `BraveSoftware\Brave-Browser\User Data`))
	mustMkdir(t, filepath.Join(tmp, `Vivaldi\User Data\Default`))

	got := findBrowserProfilesIn(tmp)
	if len(got) != 4 {
		t.Fatalf("got %d profiles, want 4: %+v", len(got), got)
	}

	// Order is browser-list order (chrome before edge before brave before
	// vivaldi) but tests against a sorted-by-path slice so a future
	// reorder of chromiumBrowsers doesn't break the assertion.
	sort.Slice(got, func(i, j int) bool { return got[i].Path < got[j].Path })

	wantBrowsers := []string{"chrome", "chrome", "edge", "vivaldi"}
	wantNames := []string{"Default", "Profile 1", "Default", "Default"}
	wantDefaults := []bool{true, false, true, true}
	for i := range got {
		if got[i].Browser != wantBrowsers[i] {
			t.Errorf("[%d] Browser = %q, want %q", i, got[i].Browser, wantBrowsers[i])
		}
		if got[i].Name != wantNames[i] {
			t.Errorf("[%d] Name = %q, want %q", i, got[i].Name, wantNames[i])
		}
		if got[i].IsDefault != wantDefaults[i] {
			t.Errorf("[%d] IsDefault = %v, want %v", i, got[i].IsDefault, wantDefaults[i])
		}
	}
}

// TestFindBrowserProfilesIn_NoBrowsersInstalled returns an empty slice
// (NOT nil) when LOCALAPPDATA exists but no Chromium-family browsers
// have been installed. Callers can range without nil-checks.
func TestFindBrowserProfilesIn_NoBrowsersInstalled(t *testing.T) {
	tmp := t.TempDir()
	got := findBrowserProfilesIn(tmp)
	if got == nil {
		t.Fatal("got nil; want empty slice")
	}
	if len(got) != 0 {
		t.Errorf("got %d profiles, want 0: %+v", len(got), got)
	}
}

// TestFindBrowserProfilesIn_FilesNotMistakenForProfiles guards the
// IsDir() check inside the walk — a regular file named "Default"
// inside a User Data dir must NOT be reported as a profile.
func TestFindBrowserProfilesIn_FilesNotMistakenForProfiles(t *testing.T) {
	tmp := t.TempDir()
	mustMkdir(t, filepath.Join(tmp, `Google\Chrome\User Data`))
	if err := os.WriteFile(filepath.Join(tmp, `Google\Chrome\User Data\Default`), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := findBrowserProfilesIn(tmp)
	if len(got) != 0 {
		t.Errorf("regular file should not register as a profile; got %+v", got)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}
