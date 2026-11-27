//go:build !windows

package main

import "os"

// openSourceShared opens the cookie source read-only; a rename over it is
// never blocked off Windows.
func openSourceShared(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY, 0)
}
