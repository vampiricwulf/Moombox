//go:build windows

package utils

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// fullControl is FILE_ALL_ACCESS, the mask Explorer shows as "Full control".
const fullControl windows.ACCESS_MASK = 0x1F01FF

// daclEntry is one access-control entry as the file system reports it.
type daclEntry struct {
	sid     string
	aceType uint8
	flags   uint8
	mask    windows.ACCESS_MASK
}

// readDACL reads path's DACL back from the file system: whether it is
// protected from inheritance, and every entry in it.
func readDACL(t *testing.T, path string) (protected bool, entries []daclEntry) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo(%q): %v", path, err)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatalf("security descriptor control of %q: %v", path, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatalf("DACL of %q: %v", path, err)
	}
	if dacl == nil {
		t.Fatalf("%q has a NULL DACL", path)
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatalf("GetAce(%d) of %q: %v", i, path, err)
		}
		e := daclEntry{aceType: ace.Header.AceType, flags: ace.Header.AceFlags, mask: ace.Mask}
		if e.aceType == windows.ACCESS_ALLOWED_ACE_TYPE || e.aceType == windows.ACCESS_DENIED_ACE_TYPE {
			e.sid = (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
		}
		entries = append(entries, e)
	}
	return control&windows.SE_DACL_PROTECTED != 0, entries
}

// ownerOnlySIDs is the set the restricted DACL must name: the account the
// test runs as, SYSTEM and the Administrators group.
func ownerOnlySIDs(t *testing.T) map[string]bool {
	t.Helper()
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatalf("token user: %v", err)
	}
	want := map[string]bool{tu.User.Sid.String(): true}
	for _, kind := range []windows.WELL_KNOWN_SID_TYPE{windows.WinLocalSystemSid, windows.WinBuiltinAdministratorsSid} {
		sid, err := windows.CreateWellKnownSid(kind)
		if err != nil {
			t.Fatalf("well-known SID %d: %v", kind, err)
		}
		want[sid.String()] = true
	}
	return want
}

// assertOwnerOnlyDACL fails unless path carries exactly the restricted DACL:
// protected, with one full-control allow entry for each of the three
// principals and nothing else.
func assertOwnerOnlyDACL(t *testing.T, path string) {
	t.Helper()
	protected, entries := readDACL(t, path)
	if !protected {
		t.Errorf("%s: the DACL is not protected — the folder's inherited entries still apply to the file", path)
	}
	want := ownerOnlySIDs(t)
	if len(entries) != len(want) {
		t.Errorf("%s: %d entries %+v, want exactly %d (current user, SYSTEM, Administrators)", path, len(entries), entries, len(want))
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if e.aceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			t.Errorf("%s: entry %+v is not an allow entry", path, e)
			continue
		}
		if !want[e.sid] {
			t.Errorf("%s: allow entry for %s, which is none of the current user, SYSTEM or Administrators", path, e.sid)
		}
		if e.mask != fullControl {
			t.Errorf("%s: %s has access mask %#x, want full control %#x", path, e.sid, uint32(e.mask), uint32(fullControl))
		}
		if e.flags != 0 {
			t.Errorf("%s: %s has entry flags %#x, want 0 (an explicit entry, not an inherited one)", path, e.sid, e.flags)
		}
		seen[e.sid] = true
	}
	for sid := range want {
		if !seen[sid] {
			t.Errorf("%s: no allow entry for %s", path, sid)
		}
	}
}

// TestRestrictFileToOwnerSetsAProtectedThreePrincipalDACL reads the ACL back
// from the file system. A file created in a folder starts with that folder's
// inherited entries (in a default install folder that includes Users and
// Authenticated Users); afterwards it must answer to the current user, SYSTEM
// and Administrators only, and no longer inherit.
//
// Mutants: leave PROTECTED_DACL_SECURITY_INFORMATION out -> the inherited
// entries are merged back and the protected flag is clear; drop SYSTEM or
// Administrators -> the entry count and the missing-SID check fail.
func TestRestrictFileToOwnerSetsAProtectedThreePrincipalDACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(path, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if protected, _ := readDACL(t, path); protected {
		t.Fatal("the fixture file already has a protected DACL — the test would prove nothing")
	}

	if err := RestrictFileToOwner(path); err != nil {
		t.Fatalf("RestrictFileToOwner: %v", err)
	}

	assertOwnerOnlyDACL(t, path)
	if got, err := os.ReadFile(path); err != nil || string(got) != "secret" {
		t.Errorf("the file is no longer readable by its own writer: %q, %v", got, err)
	}
}

func TestRestrictFileToOwnerReportsAMissingFile(t *testing.T) {
	if err := RestrictFileToOwner(filepath.Join(t.TempDir(), "nope.txt")); err == nil {
		t.Error("RestrictFileToOwner on a missing file returned nil — the caller logs this failure and must be told")
	}
}

// TestReplaceFileKeepsTheRestrictedDACL proves what the cookie and config
// writers rely on: they restrict the TEMP file and rename it over the target,
// and the target must end up with the temp file's DACL, not with the replaced
// file's and not with the folder's inherited one.
func TestReplaceFileKeepsTheRestrictedDACL(t *testing.T) {
	for _, tc := range []struct {
		name     string
		existing bool
	}{
		{"over an existing file that inherits the folder's permissions", true},
		{"to a name that does not exist yet", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dst := filepath.Join(dir, "cookies.txt")
			if tc.existing {
				if err := os.WriteFile(dst, []byte("old"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			tmp := filepath.Join(dir, "cookies.txt.1.tmp")
			if err := os.WriteFile(tmp, []byte("new"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := RestrictFileToOwner(tmp); err != nil {
				t.Fatalf("RestrictFileToOwner: %v", err)
			}

			if err := ReplaceFile(tmp, dst); err != nil {
				t.Fatalf("ReplaceFile: %v", err)
			}

			assertOwnerOnlyDACL(t, dst)
		})
	}
}

// TestDACLProtectedReadsTheFolderFlag pins the read the startup notice is
// built on: a folder that inherits answers false, one whose inheritance was
// removed answers true, and a folder that cannot be read is an error rather
// than either answer.
func TestDACLProtectedReadsTheFolderFlag(t *testing.T) {
	inheriting := t.TempDir()
	if got, err := DACLProtected(inheriting); err != nil || got {
		t.Errorf("DACLProtected(inheriting folder) = %v, %v; want false, nil", got, err)
	}

	stripped := filepath.Join(t.TempDir(), "stripped")
	if err := os.Mkdir(stripped, 0o755); err != nil {
		t.Fatal(err)
	}
	// The same protected DACL, set on a folder: what the read looks at is the
	// flag, and no process is spawned to set it.
	if err := RestrictFileToOwner(stripped); err != nil {
		t.Fatalf("protecting the fixture folder: %v", err)
	}
	if got, err := DACLProtected(stripped); err != nil || !got {
		t.Errorf("DACLProtected(folder without inheritance) = %v, %v; want true, nil", got, err)
	}

	if _, err := DACLProtected(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("DACLProtected(missing folder) returned no error")
	}
}
