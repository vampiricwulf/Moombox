//go:build linux

package cookies

import "os"

// realHostLacksDisplay reports whether a browser window has nowhere to open:
// neither an X11 (DISPLAY) nor a Wayland (WAYLAND_DISPLAY) display is named
// in Moombox's environment. A headless server, a container, and a desktop
// host where Moombox runs as a service without those variables all answer
// true (owner ruling R4-7).
func realHostLacksDisplay() bool {
	return os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == ""
}
