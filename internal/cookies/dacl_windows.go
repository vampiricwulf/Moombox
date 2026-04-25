//go:build windows

package cookies

import (
	"fmt"
	"os/exec"
	"os/user"
	"strings"
)

// applyUserOnlyDACL restricts the given path's ACL so only the current
// user has read/write access. On Windows, the mode bits passed to
// os.MkdirAll are advisory — the actual ACL inherits from the parent
// directory. When Moombox runs from a non-user-private location
// (Program Files, a shared drive, %TEMP%, etc.), the inherited ACL
// leaves the browser profile dir (containing cookies.sqlite, browsing
// history, login state) readable by other non-admin users on the same
// host. Audit reports/cookies.md #25.
//
// Implementation: shells out to icacls — the standard Windows ACL tool.
// /inheritance:r removes ALL inherited ACEs (including SYSTEM /
// Administrators / Users), and /grant:r replaces any existing explicit
// ACE for the trustee with the new one (rather than appending).
// (OI)(CI) propagates the ACE to files (Object Inherit) and subdirs
// (Container Inherit) so cookies.sqlite and friends inherit the same
// restriction.
//
// Trade-offs of the simple "user only" form:
//   - SYSTEM loses access. Backup / antivirus running as SYSTEM can no
//     longer read profile contents. Acceptable: this is browser cache /
//     cookie data, not user documents — it's recreated on the next
//     refresh anyway.
//   - If the user is a member of Administrators, removing the
//     Administrators inherited ACE means admins can't read the dir
//     except via take-ownership. Acceptable: same data-recovery
//     argument applies.
//
// Errors are returned to the caller, which logs at Warn and proceeds —
// a failed ACL tightening doesn't break setup, the dir is still
// functional, just less locked-down than ideal.
func applyUserOnlyDACL(path string) error {
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

	// /Q suppresses per-file success output. /T would recurse, but for
	// the initial directory creation there are no files inside yet —
	// adding /T is harmless but adds latency for each pre-existing
	// file (relevant only when re-applying to an existing profile).
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
