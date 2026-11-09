package config

import (
	"slices"
	"testing"
)

// TestNetworkAccessPredicates (K40) pins the alias rule the web, routes and
// TUI checks share, and that "public" is accepted by validation but offered
// by no settings surface.
func TestNetworkAccessPredicates(t *testing.T) {
	for _, tc := range []struct {
		v                string
		external, beyond bool
		settable         bool
	}{
		{"localhost", false, false, true},
		{"lan", false, true, true},
		{"external", true, true, true},
		{"public", true, true, false},
		{"", false, false, false},
		{"bogus", false, false, false},
	} {
		if got := IsExternalAccess(tc.v); got != tc.external {
			t.Errorf("IsExternalAccess(%q) = %v", tc.v, got)
		}
		if got := ListensBeyondLoopback(tc.v); got != tc.beyond {
			t.Errorf("ListensBeyondLoopback(%q) = %v", tc.v, got)
		}
		if got := slices.Contains(SettableNetworkAccess, tc.v); got != tc.settable {
			t.Errorf("%q settable = %v", tc.v, got)
		}
	}
}
