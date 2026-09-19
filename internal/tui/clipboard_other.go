//go:build !windows

package tui

// osClipboardFallback has no non-Windows implementation: there is no
// dependency-free equivalent of clip.exe (xclip/wl-copy are not installed by
// default and are not Moombox's to require), and a native X11/Wayland client
// would pull the CGo-shaped dependency the project refuses. OSC 52 is the
// mechanism everywhere else, and the feedback says so rather than claiming a
// copy the terminal may have dropped (CORE-15, O-W).
func osClipboardFallback(string) bool { return false }
