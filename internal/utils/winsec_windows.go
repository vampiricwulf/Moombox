//go:build windows

package utils

import (
	"fmt"
	"os/exec"
	"os/user"
	"strings"
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
//   /inheritance:r       remove ALL inherited ACEs (including SYSTEM /
//                        Administrators / Users)
//   /grant:r <user>:(OI)(CI)F
//                        replace any explicit ACE for the trustee with
//                        Full Control + Object Inherit + Container
//                        Inherit so child files / subdirs share the
//                        restriction
//   /Q                   suppress per-file success output
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
