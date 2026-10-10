package config

import (
	"strings"
	"testing"
)

// TestPasswordLengthErrorBounds pins the bounds every password surface
// shares: 8 to 128 bytes, the 128 being the one /api/auth/login has always
// enforced. Setup accepted anything from 8 up, so a password the login
// refuses (W26-10).
//
// Mutants killed: `<` as `<=` (8 refused); `>` as `>=` (128 refused);
// dropping the maximum's arm (129 accepted).
func TestPasswordLengthErrorBounds(t *testing.T) {
	for _, tc := range []struct {
		pw   string
		want string
	}{
		{"", PasswordTooShortMsg},
		{strings.Repeat("a", 7), PasswordTooShortMsg},
		{strings.Repeat("a", 8), ""},
		{strings.Repeat("a", 128), ""},
		{strings.Repeat("a", 129), PasswordTooLongMsg},
		// Bytes, as the login counts: 43 three-byte characters are 129.
		{strings.Repeat("あ", 43), PasswordTooLongMsg},
	} {
		if got := PasswordLengthError(tc.pw); got != tc.want {
			t.Errorf("PasswordLengthError(%d bytes) = %q, want %q", len(tc.pw), got, tc.want)
		}
	}
}
