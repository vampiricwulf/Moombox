//go:build windows

package utils

import (
	"fmt"
	"os/exec"
	"os/user"
	"strings"

	"golang.org/x/sys/windows"
)

// ApplyUserOnlyDACL restricts the given path's ACL so only the current
// user has read/write access. On Windows, mode bits passed to
// os.MkdirAll / os.OpenFile are advisory — the actual ACL inherits
// from the parent directory. When Moombox runs from a non-user-private
// location (Program Files, a shared drive, %TEMP%, etc.), inherited
// ACLs leave sensitive files (cookies.sqlite, moombox.toml with
// password hash, etc.) readable by other non-admin users on the same
// host. Audit reports/cookies.md #25 + reports/config.md #22.
//
// Implementation: shells out to icacls — the standard Windows ACL tool.
//
//	/inheritance:r       remove ALL inherited ACEs (including SYSTEM /
//	                     Administrators / Users)
//	/grant:r <user>:(OI)(CI)F
//	                     replace any explicit ACE for the trustee with
//	                     Full Control + Object Inherit + Container
//	                     Inherit so child files / subdirs share the
//	                     restriction
//	/Q                   suppress per-file success output
//
// Trade-offs of the simple "user only" form:
//   - SYSTEM loses access. Backup / antivirus running as SYSTEM can no
//     longer read the path's contents. Acceptable for Moombox's data
//     surfaces (cookies, config) — they're recreated on demand.
//   - If the user is a member of Administrators, removing the
//     Administrators inherited ACE means admins can't read the path
//     except via take-ownership. Acceptable: same data-recovery
//     argument applies.
//
// Errors are returned to the caller. Callers typically log at Warn and
// proceed — a failed ACL tightening doesn't break the underlying
// operation, just leaves the path less locked-down than ideal.
func ApplyUserOnlyDACL(path string) error {
	u, err := user.Current()
	if err != nil {
		return fmt.Errorf("get current user: %w", err)
	}
	// user.Current().Username on Windows returns "DOMAIN\\name". icacls
	// accepts that form directly.
	username := u.Username
	if username == "" {
		return fmt.Errorf("current user has empty username")
	}

	cmd := exec.Command("icacls", path,
		"/inheritance:r",
		"/grant:r", username+":(OI)(CI)F",
		"/Q",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		// icacls writes diagnostics to stdout/stderr that the caller's
		// logger will surface — strip CRLF so it appears on a single
		// log line.
		clean := strings.ReplaceAll(strings.ReplaceAll(string(output), "\r", ""), "\n", " ")
		return fmt.Errorf("icacls failed: %w: %s", err, clean)
	}
	return nil
}

// fileAllAccess is FILE_ALL_ACCESS, what Explorer shows as "Full control".
const fileAllAccess windows.ACCESS_MASK = 0x1F01FF

// RestrictFileToOwner replaces path's DACL with a protected one (nothing is
// inherited from the folder) that grants full control to exactly three
// principals: the account this process runs as, NT AUTHORITY\SYSTEM and
// BUILTIN\Administrators. It is the per-FILE hardening for the secrets Moombox
// writes (cookies.txt and its sidecar, config.toml), and unlike
// ApplyUserOnlyDACL it changes nothing but the one file: the folder, and
// everything else in it, keeps the permissions it has.
//
// SYSTEM and Administrators are kept on purpose. Both can take the file
// whatever its DACL says, so leaving them out protects nothing, and it breaks
// backup and recovery tools that run as either.
//
// Win32 calls only, no process is started: it runs on every cookie write and
// every config save. A rename keeps a file's DACL, so the callers apply this
// to their temp file and the target ends up with it.
func RestrictFileToOwner(path string) error {
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("get current user SID: %w", err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return fmt.Errorf("well-known SID for SYSTEM: %w", err)
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return fmt.Errorf("well-known SID for Administrators: %w", err)
	}
	// A process that itself runs as SYSTEM names SYSTEM twice; one entry per
	// distinct principal.
	sids := []*windows.SID{tokenUser.User.Sid}
	for _, sid := range []*windows.SID{system, admins} {
		if !sid.Equals(tokenUser.User.Sid) {
			sids = append(sids, sid)
		}
	}
	entries := make([]windows.EXPLICIT_ACCESS, 0, len(sids))
	for _, sid := range sids {
		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: fileAllAccess,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		})
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return fmt.Errorf("build DACL: %w", err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil); err != nil {
		return fmt.Errorf("set DACL: %w", err)
	}
	return nil
}

// DACLProtected reports whether path's DACL is protected, that is, whether the
// path has stopped inheriting permissions from its parent (SE_DACL_PROTECTED).
// It only reads. `icacls <path> /inheritance:r`, which ApplyUserOnlyDACL runs,
// is one of the things that sets the flag.
func DACLProtected(path string) (bool, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, fmt.Errorf("read DACL: %w", err)
	}
	control, _, err := sd.Control()
	if err != nil {
		return false, fmt.Errorf("read security descriptor control: %w", err)
	}
	return control&windows.SE_DACL_PROTECTED != 0, nil
}
