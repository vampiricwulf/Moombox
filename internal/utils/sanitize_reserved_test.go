package utils

import "testing"

// Windows reserves a device name with any extension and with trailing spaces
// before it; a component that merely starts with one is an ordinary name.
func TestIsWindowsReservedName(t *testing.T) {
	for name, want := range map[string]bool{
		"CON": true, "nul.mp4": true, "Com1.tar.gz": true, "aux ": true, "CONIN$": true,
		"Console": false, "LPT10": false, "_CON": false, "": false, "con-stream": false,
	} {
		if got := IsWindowsReservedName(name); got != want {
			t.Errorf("IsWindowsReservedName(%q) = %v, want %v", name, got, want)
		}
	}
}
