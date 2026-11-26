//go:build linux

package cookies

import "testing"

// The Linux display check reads the environment the browser would inherit:
// a window needs DISPLAY (X11) or WAYLAND_DISPLAY (Wayland), and either one
// is enough (owner ruling R4-7).
//
// Mutants it kills: requiring both variables; reading only one of them.
func TestRealHostLacksDisplayReadsBothVariables(t *testing.T) {
	for _, tc := range []struct {
		display, wayland string
		lacks            bool
	}{
		{"", "", true},
		{":0", "", false},
		{"", "wayland-0", false},
		{":0", "wayland-0", false},
	} {
		t.Setenv("DISPLAY", tc.display)
		t.Setenv("WAYLAND_DISPLAY", tc.wayland)
		if got := realHostLacksDisplay(); got != tc.lacks {
			t.Errorf("DISPLAY=%q WAYLAND_DISPLAY=%q: lacks display = %v, want %v", tc.display, tc.wayland, got, tc.lacks)
		}
	}
}
