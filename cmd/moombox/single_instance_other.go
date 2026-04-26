//go:build !windows

package main

// acquireSingleInstanceLock is a no-op on non-Windows. Moombox is
// Windows-only per CLAUDE.md, but the build tags keep the package
// buildable on other OSes for tooling / IDE support.
func acquireSingleInstanceLock() error { return nil }

// releaseSingleInstanceLock is a no-op on non-Windows.
func releaseSingleInstanceLock() {}
