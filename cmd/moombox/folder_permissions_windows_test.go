//go:build windows

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// folderSDDL reads a folder's owner, group and DACL back as one SDDL string.
func folderSDDL(t *testing.T, path string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo(%q): %v", path, err)
	}
	return sd.String()
}

// installFolder builds a folder shaped like an install (or, without the
// output tree, like a dedicated secrets folder) and optionally removes its
// inheritance, the state an earlier version's lockdown left behind. The
// protected DACL is set with Win32 calls; no icacls is run.
func installFolder(t *testing.T, name string, holdsOutput, stripped bool) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if holdsOutput {
		if err := os.Mkdir(filepath.Join(dir, "output"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if stripped {
		if err := utils.RestrictFileToOwner(dir); err != nil {
			t.Fatalf("removing the fixture folder's inheritance: %v", err)
		}
	}
	return dir
}

// TestStartupNoticeOnRealFolders runs the notice's real predicates —
// utils.DirHoldsSharedData and the DACL read — over four real folders, one per
// combination. Only the install folder that lost its inheritance is named, and
// it is left exactly as it was found: Moombox reports this state, it does not
// repair it.
//
// Mutants: have DACLProtected answer from anything but the protected flag ->
// the wrong folders are named; "repair" the folder -> its descriptor changes.
func TestStartupNoticeOnRealFolders(t *testing.T) {
	strippedInstall := installFolder(t, "stripped-install", true, true)
	healthyInstall := installFolder(t, "healthy-install", true, false)
	lockedSecrets := installFolder(t, "locked-secrets", false, true)
	plainSecrets := installFolder(t, "plain-secrets", false, false)
	before := folderSDDL(t, strippedInstall)
	rec := &infoRecorder{}

	var named []string
	for _, dir := range []string{strippedInstall, healthyInstall, lockedSecrets, plainSecrets} {
		named = append(named, sharedFoldersWithoutInheritance(
			filepath.Join(dir, "cookies.txt"), filepath.Join(dir, "config.toml"))...)
	}
	logUninheritedFolders(rec, named)

	if want := []string{strippedInstall}; !reflect.DeepEqual(named, want) {
		t.Errorf("folders named = %q, want %q", named, want)
	}
	if len(rec.lines) != 1 {
		t.Errorf("%d lines logged, want 1: %q", len(rec.lines), rec.lines)
	}
	if after := folderSDDL(t, strippedInstall); after != before {
		t.Errorf("the folder was changed:\n before %s\n after  %s", before, after)
	}
}
