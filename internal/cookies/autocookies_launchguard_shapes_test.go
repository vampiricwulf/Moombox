package cookies

import "testing"

// TestLaunchGuardRecognisesEveryPlatformProfileShape: the guard compares a
// separator-normalised, lowercased absolute path against one list that holds
// the Windows %AppData% shapes AND the Linux/macOS dotfile shapes, so a real
// browser tree is refused on every OS. Before 2026-09-08 the list knew only
// the Windows shapes, so on Linux (Docker) a real ~/.mozilla/firefox profile
// was never recognised and the opt-in guard was inert there. Every row is
// exercised on every OS: filepath.Abs may prefix a working directory or
// swap separators, and the normalisation absorbs both.
func TestLaunchGuardRecognisesEveryPlatformProfileShape(t *testing.T) {
	refused := []string{
		// Windows
		`C:\Users\test\AppData\Roaming\Mozilla\Firefox\Profiles\xxxxx.default-release`,
		`C:\Users\test\AppData\Local\Google\Chrome\User Data\Default`,
		`C:\Users\test\AppData\Local\Microsoft\Edge\User Data`,
		// Linux
		"/home/test/.mozilla/firefox/xxxxx.default-release",
		"/home/test/snap/firefox/common/.mozilla/firefox/xxxxx.default",
		"/home/test/.var/app/org.mozilla.firefox/.mozilla/firefox/xxxxx.default",
		"/home/test/.config/google-chrome/Default",
		"/home/test/.config/chromium/Default",
		"/home/test/.config/BraveSoftware/Brave-Browser/Default",
		"/home/test/.config/microsoft-edge/Default",
		"/home/test/.config/vivaldi/Default",
		"/home/test/.thunderbird/xxxxx.default",
		"/home/test/.librewolf/xxxxx.default",
		"/home/test/.waterfox/xxxxx.default",
		// macOS
		"/Users/test/Library/Application Support/Firefox/Profiles/xxxxx.default-release",
		"/Users/test/Library/Application Support/Google/Chrome/Default",
	}
	for _, p := range refused {
		if err := validateBrowserProfileDirForLaunch(p); err == nil {
			t.Errorf("%q: a real browser profile tree must be refused; got nil", p)
		}
	}

	allowed := []string{
		"",                           // inert service
		"/home/test/moombox/profile", // the README's copied-profile recipe
		"/data/firefox-profile",      // a Docker mount of a COPY
		`D:\Moombox\profile`,         // a copy beside the binary
		"/home/test/.mozilla-backup/firefox/xxxx", // a backup, not the live tree
		"/srv/chrome-exports/Default",             // an export, not ~/.config
	}
	for _, p := range allowed {
		if err := validateBrowserProfileDirForLaunch(p); err != nil {
			t.Errorf("%q: not a live browser tree, must be allowed; got %v", p, err)
		}
	}
}
