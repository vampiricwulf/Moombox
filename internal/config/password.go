package config

import "strings"

// PasswordMinLen and PasswordMaxLen bound the dashboard password wherever one
// is set — both first-run wizards, POST /api/auth/set-password, TUI Settings →
// Security, and the boot-time hashing of a plaintext password_hash — and
// PasswordMaxLen bounds every check of one too (login, remove-password).
// Lengths are bytes, as /api/auth/login has always counted them. The
// dashboard's passwordLengthError (web/public/modules/utils.js) is the same
// rule.
const (
	PasswordMinLen = 8
	PasswordMaxLen = 128
)

// The two refusals, worded once: every surface that sets a password answers
// with these.
const (
	PasswordTooShortMsg = "Password must be at least 8 characters"
	PasswordTooLongMsg  = "Password too long (max 128 characters)"
)

// PasswordLengthError returns the refusal for a password shorter than
// PasswordMinLen or longer than PasswordMaxLen, or "" for one within them.
//
// One rule for every surface, because they disagreed: setup accepted any
// password of at least 8 characters while /api/auth/login refuses one over
// 128 bytes, so a long passphrase pasted into either wizard set a password
// the remote dashboard could never be logged into (W26-10).
func PasswordLengthError(pw string) string {
	switch {
	case len(pw) < PasswordMinLen:
		return PasswordTooShortMsg
	case len(pw) > PasswordMaxLen:
		return PasswordTooLongMsg
	}
	return ""
}

// PasswordOuterSpaceWarning is what a surface that sets the password shows
// while the one typed starts or ends with whitespace — shown, never refused.
// The dashboard's PASSWORD_OUTER_SPACE_WARNING (web/public/modules/utils.js)
// is the same text.
const PasswordOuterSpaceWarning = "This password starts or ends with a space. It is kept as typed."

// PasswordHasOuterSpace reports whether pw starts or ends with whitespace.
//
// No surface trims a password: what is typed is what is stored and what is
// checked (docs/spec/security.md, Password rules). Both first-run wizards
// used to trim it while the login and every password change did not, so a
// password typed with a stray space at either end could not be logged in
// with. The surfaces that set one warn with PasswordOuterSpaceWarning
// instead, and VerifyPassword (internal/web/auth.go) accepts the trimmed form
// of such a password for the installs those wizards set up.
func PasswordHasOuterSpace(pw string) bool {
	return strings.TrimSpace(pw) != pw
}
