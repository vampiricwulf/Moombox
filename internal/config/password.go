package config

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
