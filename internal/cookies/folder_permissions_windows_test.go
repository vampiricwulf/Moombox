//go:build windows

package cookies

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// folderPermissions reads a folder's permissions back from the file system in
// this platform's own terms: owner, group and DACL as one SDDL string.
func folderPermissions(t *testing.T, dir string) string {
	t.Helper()
	return securitySDDL(t, dir)
}

// folderIsOwnerOnly reports whether dir reads back as restricted the way
// utils.ApplyUserOnlyDACL leaves a folder: it no longer inherits from its
// parent.
func folderIsOwnerOnly(t *testing.T, dir string) bool {
	t.Helper()
	protected, err := utils.DACLProtected(dir)
	if err != nil {
		t.Fatalf("DACLProtected(%q): %v", dir, err)
	}
	return protected
}

// TestCookieFolderMoomboxCreatesInheritsOnWindows: a folder Moombox creates
// for the cookie file gets no ACL call. It inherits from its parent like any
// new folder, and the file restriction is what protects the secret.
//
// Mutant: lock the new folder down with ApplyUserOnlyDACL -> its DACL reads
// back protected.
func TestCookieFolderMoomboxCreatesInheritsOnWindows(t *testing.T) {
	dir := t.TempDir() + `\secrets`
	if err := SaveMeta(dir+`\cookies.txt`, CookieMeta{}); err != nil {
		t.Fatalf("SaveMeta: %v", err)
	}
	if folderIsOwnerOnly(t, dir) {
		t.Errorf("the created cookie folder stopped inheriting from its parent: %s", folderPermissions(t, dir))
	}
}
