//go:build windows

package cookies

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

// TestCookieWriteRestrictsTheFileOnWindows: with nothing faked, the cookie
// file and its sidecar, written into a folder shaped like a default install
// (cookies.txt beside output/, staging/, the database and the log), carry the
// restricted DACL: the one utils.RestrictFileToOwner sets, whose exact contents
// internal/utils reads back entry by entry. That the folder itself is left
// alone is TestCookieWriteLeavesAnExistingFolderAlone.
//
// Mutant: drop the restrictFileToOwner call -> the files read back with the
// folder's inherited DACL.
func TestCookieWriteRestrictsTheFileOnWindows(t *testing.T) {
	dir := cookieDirFixture(t, "output/", "staging/", "moombox.db", "moombox.log")
	cookiePath := filepath.Join(dir, "cookies.txt")
	if err := writeFileAtomic(cookiePath, []byte("# Netscape HTTP Cookie File\n"), 0o600); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	if err := SaveMeta(cookiePath, CookieMeta{}); err != nil {
		t.Fatalf("SaveMeta: %v", err)
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
	for _, path := range []string{cookiePath, MetaPath(cookiePath)} {
		if got := securitySDDL(t, path); got != want {
			t.Errorf("%s:\n got  %s\n want %s (current user, SYSTEM and Administrators only, not inherited)", filepath.Base(path), got, want)
		}
	}
}
