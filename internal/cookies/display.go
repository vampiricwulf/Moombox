package cookies

// hostLacksDisplay is StartSetup's display check (realHostLacksDisplay:
// display_linux.go, display_other.go), a package variable so a test can
// answer it on any OS, the convention setupBrowserGone and detectBrowser
// follow. Nothing in production reassigns it.
var hostLacksDisplay = realHostLacksDisplay

// SetHostLacksDisplayForTesting makes StartSetup's display check answer
// lacks, and returns the function that puts the real check back. For tests
// only, as web.SetPasswordHashCostForTesting is: the TestMains of the
// packages that drive StartSetup answer "a display exists", so a Linux CI
// runner with no DISPLAY runs them as Windows does, and the tests of the
// ErrNoDisplay refusal answer the other way.
func SetHostLacksDisplayForTesting(lacks bool) (restore func()) {
	prev := hostLacksDisplay
	hostLacksDisplay = func() bool { return lacks }
	return func() { hostLacksDisplay = prev }
}
