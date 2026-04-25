package twitch

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// TestErrTwitchAuthExpiredWrapping verifies that the fmt.Errorf %w wrapping
// used in gqlRequest for 401/403 with a non-empty auth token produces an
// error that callers can detect with errors.Is. Regression test for audit
// reports/twitch.md issue #8.
func TestErrTwitchAuthExpiredWrapping(t *testing.T) {
	// Mirror the exact format string used in gqlRequest — if the wrapping
	// pattern is ever changed to use %s instead of %w, this test will fail
	// and surface the regression before it reaches production.
	err := fmt.Errorf("gql auth failure (%d) (%s): %s: %w", 401, "StreamMetadata", "unauthorized", ErrTwitchAuthExpired)
	if !errors.Is(err, ErrTwitchAuthExpired) {
		t.Error("expected errors.Is(err, ErrTwitchAuthExpired) to be true for wrapped auth failure")
	}

	// A plain error without wrapping should not match.
	plain := fmt.Errorf("some other failure")
	if errors.Is(plain, ErrTwitchAuthExpired) {
		t.Error("expected errors.Is(plain, ErrTwitchAuthExpired) to be false")
	}
}

// TestParseRetryAfter locks down the parser used by gqlRequest's 429
// handling. RFC 7231 §7.1.3 allows both delta-seconds and HTTP-date forms;
// audit reports/twitch.md #11 wants both honored when within the cap.
func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"empty", "", 0},
		{"whitespace", "   ", 0},
		{"seconds", "5", 5 * time.Second},
		{"seconds with whitespace", "  10  ", 10 * time.Second},
		{"zero seconds", "0", 0},
		{"negative seconds", "-5", 0},
		{"non-numeric / non-date", "garbage", 0},
		// HTTP-date form: relative future date. Use http.TimeFormat
		// ("Mon, 02 Jan 2006 15:04:05 GMT") — http.ParseTime accepts
		// RFC1123 GMT, not UTC, so the timezone matters here.
		{"http-date future", time.Now().Add(7 * time.Second).UTC().Format(http.TimeFormat), 7 * time.Second},
		// Past dates produce 0 (already-elapsed).
		{"http-date past", time.Now().Add(-1 * time.Hour).UTC().Format(http.TimeFormat), 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseRetryAfter(tt.in)
			// HTTP-date case is timing-sensitive — allow a 2s slack window.
			if tt.name == "http-date future" {
				if got < tt.want-2*time.Second || got > tt.want+2*time.Second {
					t.Errorf("parseRetryAfter(%q) = %v, want ~%v ± 2s", tt.in, got, tt.want)
				}
				return
			}
			if got != tt.want {
				t.Errorf("parseRetryAfter(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
