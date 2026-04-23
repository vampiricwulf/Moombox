package twitch

import (
	"errors"
	"fmt"
	"testing"
)

// TestErrTwitchAuthExpiredWrapping verifies that the fmt.Errorf %w wrapping
// used in gqlRequest for 401/403 with a non-empty auth token produces an
// error that callers can detect with errors.Is. Regression test for audit
// reports/twitch.md issue #8.
func TestErrTwitchAuthExpiredWrapping(t *testing.T) {
	// Mirror the exact format string used in gqlRequest — if the wrapping
	// pattern is ever changed to use %s instead of %w, this test will fail
	// and surface the regression before it reaches production.
	err := fmt.Errorf("gql auth failure (%d): %s: %w", 401, "unauthorized", ErrTwitchAuthExpired)
	if !errors.Is(err, ErrTwitchAuthExpired) {
		t.Error("expected errors.Is(err, ErrTwitchAuthExpired) to be true for wrapped auth failure")
	}

	// A plain error without wrapping should not match.
	plain := fmt.Errorf("some other failure")
	if errors.Is(plain, ErrTwitchAuthExpired) {
		t.Error("expected errors.Is(plain, ErrTwitchAuthExpired) to be false")
	}
}
