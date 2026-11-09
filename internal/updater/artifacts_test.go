package updater

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestArtifactNamesAreTheUpdatePathBytes pins every on-disk suffix to the
// bytes released versions read and write: an old launcher beside a new child
// (and the reverse) must find the same files. A change here breaks the update
// path, not just a test.
func TestArtifactNamesAreTheUpdatePathBytes(t *testing.T) {
	for got, want := range map[string]string{
		NewSuffix:                ".new",
		NewSigSuffix:             ".new.sig",
		OldSuffix:                ".old",
		FailedBinarySuffix:       ".failed",
		LauncherImageSuffix:      "~",
		UpdateFailedMarkerSuffix: ".update-failed",
		UpdateBrokenMarkerSuffix: ".update-broken",
		PendingVersionSuffix:     ".update-pending",
	} {
		if got != want {
			t.Errorf("suffix %q, want %q", got, want)
		}
	}
}

// TestFailureMarkersBrokenThenFailed (K59): FailureMarkers replaced four
// copies of the marker loop. It lists what is present and nothing else, and
// broken before failed — the order the boot's announcement joins into the
// dedupe stamp it persists, so a stamp an older boot wrote compares equal.
//
// Mutant: the suffix list reversed — the both-present case fails.
func TestFailureMarkersBrokenThenFailed(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "moombox.exe")
	broken, failed := exe+UpdateBrokenMarkerSuffix, exe+UpdateFailedMarkerSuffix
	write := func(p string) {
		t.Helper()
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := FailureMarkers(exe); len(got) != 0 {
		t.Errorf("no markers: %q", got)
	}
	write(failed)
	if got := FailureMarkers(exe); !slices.Equal(got, []string{failed}) {
		t.Errorf("failed only: %q", got)
	}
	write(broken)
	if got := FailureMarkers(exe); !slices.Equal(got, []string{broken, failed}) {
		t.Errorf("both: %q, want broken then failed", got)
	}
	if err := os.Remove(failed); err != nil {
		t.Fatal(err)
	}
	if got := FailureMarkers(exe); !slices.Equal(got, []string{broken}) {
		t.Errorf("broken only: %q", got)
	}
	// A stale "~" or ".failed" binary is not a marker.
	write(exe + LauncherImageSuffix)
	write(exe + FailedBinarySuffix)
	if err := os.Remove(broken); err != nil {
		t.Fatal(err)
	}
	if got := FailureMarkers(exe); len(got) != 0 {
		t.Errorf("artifacts read as markers: %q", got)
	}
}
