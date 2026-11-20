//go:build windows

package config

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// securitySDDL reads path's owner, group and DACL back from the file system
// as one SDDL string.
func securitySDDL(t *testing.T, path string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo(%q): %v", path, err)
	}
	return sd.String()
}

// folderPermissions reads a folder's permissions back from the file system in
// this platform's own terms: owner, group and DACL as one SDDL string.
func folderPermissions(t *testing.T, dir string) string {
	t.Helper()
	return securitySDDL(t, dir)
}

// TestSaveRestrictsTheFileOnWindows: with nothing faked, config.toml saved
// into a folder shaped like a default install (beside output/, staging/, the
// database and the log) carries the restricted DACL: the one
// utils.RestrictFileToOwner sets, whose exact contents internal/utils reads
// back entry by entry. That the folder itself is left alone is
// TestSaveLeavesAnExistingFolderAlone.
//
// Mutant: drop Save's restrictFileToOwner call -> config.toml reads back with
// the folder's inherited DACL.
func TestSaveRestrictsTheFileOnWindows(t *testing.T) {
	dir := configDirFixture(t, "output/", "staging/", "moombox.db", "moombox.log")
	target := filepath.Join(dir, "config.toml")

	if err := Save(Defaults(), target); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reference := filepath.Join(t.TempDir(), "reference")
	if err := os.WriteFile(reference, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	inherited := securitySDDL(t, reference)
	if err := utils.RestrictFileToOwner(reference); err != nil {
		t.Fatalf("RestrictFileToOwner(reference): %v", err)
	}
	want := securitySDDL(t, reference)
	if want == inherited {
		t.Fatal("the reference file reads the same restricted and unrestricted — the comparison below would prove nothing")
	}
	if got := securitySDDL(t, target); got != want {
		t.Errorf("config.toml:\n got  %s\n want %s (current user, SYSTEM and Administrators only, not inherited)", got, want)
	}
}

// TestSaveCreatedFolderInheritsOnWindows: a folder Save creates for
// config.toml gets no ACL call. It inherits from its parent like any new
// folder, and the file restriction is what protects the password hash.
//
// Mutant: lock the new folder down with ApplyUserOnlyDACL (what Save did to
// every folder up to v2.9.9) -> its DACL reads back protected.
func TestSaveCreatedFolderInheritsOnWindows(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "moombox")
	if err := Save(Defaults(), filepath.Join(dir, "config.toml")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	protected, err := utils.DACLProtected(dir)
	if err != nil {
		t.Fatalf("DACLProtected: %v", err)
	}
	if protected {
		t.Errorf("the created config folder stopped inheriting from its parent: %s", securitySDDL(t, dir))
	}
}
