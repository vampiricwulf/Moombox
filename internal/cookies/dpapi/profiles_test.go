package dpapi

import (
	"testing"
)

// TestIsChromiumProfileDirName covers the directory-name allowlist used
// by FindBrowserProfiles to filter out system-managed dirs that aren't
// real user profiles.
func TestIsChromiumProfileDirName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"Default profile", "Default", true},
		{"Profile 1", "Profile 1", true},
		{"Profile 2", "Profile 2", true},
		{"Profile 12 (high count)", "Profile 12", true},
		{"Profile name with space-trailing legacy form", "Profile WorkAccount", true},
		{"empty", "", false},
		{"random folder", "Cache", false},
		{"system profile excluded", "System Profile", false},
		{"guest profile excluded", "Guest Profile", false},
		{"crashpad excluded", "Crashpad", false},
		{"shader cache excluded", "ShaderCache", false},
		{"capitalisation matters - lowercase default rejected", "default", false},
		{"prefix without space rejected", "ProfileFoo", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isChromiumProfileDirName(tt.in); got != tt.want {
				t.Errorf("isChromiumProfileDirName(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// TestChromiumBrowsersHaveUniqueIds guards against accidental duplicate
// entries in the chromiumBrowsers table — a stale entry collapse would
// silently misroute one of the browsers.
func TestChromiumBrowsersHaveUniqueIds(t *testing.T) {
	seen := map[string]bool{}
	for _, b := range chromiumBrowsers {
		if seen[b.id] {
			t.Errorf("chromiumBrowsers contains duplicate id %q", b.id)
		}
		seen[b.id] = true
	}
}

// TestChromiumBrowsersHaveUserDataSuffix locks the layout assumption
// shared by ReadChromeCookies: every entry's userDataPath must end in
// "User Data" so loadChromeMasterKey's filepath.Dir(profilePath) lands
// at the User Data root where Local State lives.
func TestChromiumBrowsersHaveUserDataSuffix(t *testing.T) {
	const want = `User Data`
	for _, b := range chromiumBrowsers {
		if len(b.userDataPath) < len(want) || b.userDataPath[len(b.userDataPath)-len(want):] != want {
			t.Errorf("browser %q has userDataPath %q; expected to end in %q (Local State lookup depends on it)",
				b.id, b.userDataPath, want)
		}
	}
}
