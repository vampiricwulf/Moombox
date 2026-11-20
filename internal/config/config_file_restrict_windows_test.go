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

// TestSaveIntoASharedFolderRestrictsTheFileNotTheFolder is config.Save's half
// of the incident, with nothing faked: the real gate, the real icacls lockdown
// and the real file restriction, over a folder shaped like a default install
// (config.toml beside output/, staging/, the database and the log). Save runs
// its lockdown synchronously, so the folder is read back as soon as it returns.
//
// The folder's security descriptor must read back identical; config.toml must
// carry the restricted DACL — the one utils.RestrictFileToOwner sets, whose
// exact contents internal/utils reads back entry by entry.
//
// Mutants: answer true for every Windows folder in utils.DirTighteningAllowed
// (the shipped behaviour) -> icacls rewrites the folder's descriptor; drop
// Save's restrictFileToOwner call -> config.toml reads back with the folder's
// inherited DACL.
func TestSaveIntoASharedFolderRestrictsTheFileNotTheFolder(t *testing.T) {
	dir := configDirFixture(t, "output/", "staging/", "moombox.db", "moombox.log")
	before := securitySDDL(t, dir)
	target := filepath.Join(dir, "config.toml")

	if err := Save(Defaults(), target); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if after := securitySDDL(t, dir); after != before {
		t.Errorf("the folder's permissions changed:\n before %s\n after  %s", before, after)
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
