package engine

import (
	"errors"
	"fmt"
	"testing"
)

// TestIs403LikelyCipher locks down the body-content gate that prevents
// spurious cipher invalidations when YouTube returns 403 for non-
// cipher reasons (PO-token rejection, bot detection). Audit
// reports/engine.md #25.
func TestIs403LikelyCipher(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error → assume cipher",
			err:  nil,
			want: true,
		},
		{
			name: "empty body → assume cipher",
			err:  errors.New("HTTP 403"),
			want: true,
		},
		{
			name: "generic ERROR body → assume cipher",
			err:  errors.New("HTTP 403: ERROR"),
			want: true,
		},
		{
			name: "missing_pot marker → not cipher",
			err:  errors.New(`HTTP 403: {"error":"missing_pot"}`),
			want: false,
		},
		{
			name: "po token prose → not cipher",
			err:  errors.New("HTTP 403: PO Token required"),
			want: false,
		},
		{
			name: "lowercase 'po token' marker → not cipher",
			err:  errors.New("HTTP 403: po token validation failed"),
			want: false,
		},
		{
			name: "bot detection marker → not cipher",
			err:  errors.New("HTTP 403: This request was identified as a bot"),
			want: false,
		},
		{
			name: "automated marker → not cipher",
			err:  errors.New("HTTP 403: Automated requests blocked"),
			want: false,
		},
		{
			name: "captcha marker → not cipher",
			err:  errors.New("HTTP 403: please solve captcha"),
			want: false,
		},
		{
			name: "case-insensitive matching",
			err:  errors.New("HTTP 403: BOT DETECTED"),
			want: false,
		},
		{
			name: "wrapped error preserves marker visibility",
			err:  fmt.Errorf("fetch failed: %w", errors.New("HTTP 403: missing_pot")),
			want: false,
		},
		{
			name: "unknown anti-bot prose → cipher (false negative cost is one recompile)",
			err:  errors.New("HTTP 403: We've detected unusual traffic"),
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := is403LikelyCipher(tt.err)
			if got != tt.want {
				t.Errorf("is403LikelyCipher(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestNon403CipherMarkersAreLowercase guards against accidentally
// adding a marker with mixed case — the helper uses ToLower on the
// error message, so any uppercase character in the marker list would
// silently never match.
func TestNon403CipherMarkersAreLowercase(t *testing.T) {
	for _, marker := range non403CipherMarkers {
		for _, r := range marker {
			if r >= 'A' && r <= 'Z' {
				t.Errorf("marker %q contains uppercase %q — won't match the lowercased input", marker, string(r))
			}
		}
	}
}
