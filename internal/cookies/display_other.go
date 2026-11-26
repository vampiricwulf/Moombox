//go:build !linux

package cookies

// realHostLacksDisplay is false off Linux: Windows always has a desktop for
// the browser login to open on, and the display check is a Linux rule
// (owner ruling R4-7).
func realHostLacksDisplay() bool { return false }
