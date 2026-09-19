package dpapi

import (
	"os"
	"path/filepath"
	"strings"
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

// TestValidateProfileDir is COOKIES-7's guard. An operator-supplied
// dpapi_profile_dir must LOOK like a Chromium profile directory before the
// fallback trusts it, because pointing the DPAPI reader at the wrong directory
// produces "no cookies came out" with no hint about which of several causes it
// was. The two structural facts are the ones ReadChromeCookies needs: the
// encrypted master key lives in `Local State` ONE LEVEL UP (in User Data), and
// the cookie store is `Cookies` in the profile dir or `Network/Cookies` beside
// it on newer Chromium.
//
// Mutants, each named with the subtest that catches it:
//   - drop the Local State check -> "no Local State one level up" validates,
//     and the read later fails on a master key it cannot find.
//   - accept only `Cookies` -> "Network/Cookies" fails, and every post-M96
//     Chromium (which moved the store into Network/) is refused.
//   - skip the "is a directory" test -> "a file, not a directory" fails on the
//     WORDING. Every later check refuses a file too, so the presence of an
//     error proves nothing here; only the message tells the operator what to
//     fix, which is why that subtest asserts it.
//   - swap Lstat for Stat -> "a symlink is refused, never followed" validates
//     a SYMLINK, and every judgement made about the path as written (the
//     caller's browser-profile-tree deny-list included) then describes a
//     different directory than the one the reader opens.
func TestValidateProfileDir(t *testing.T) {
	// helper: build <root>/User Data/{Local State,<profile>/…}
	mk := func(t *testing.T, cookiesAt string, withLocalState bool) string {
		t.Helper()
		userData := filepath.Join(t.TempDir(), "User Data")
		profile := filepath.Join(userData, "Default")
		if err := os.MkdirAll(profile, 0o755); err != nil {
			t.Fatal(err)
		}
		if withLocalState {
			if err := os.WriteFile(filepath.Join(userData, "Local State"), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if cookiesAt != "" {
			p := filepath.Join(profile, filepath.FromSlash(cookiesAt))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return profile
	}

	t.Run("Cookies in the profile dir", func(t *testing.T) {
		if err := ValidateProfileDir(mk(t, "Cookies", true)); err != nil {
			t.Errorf("ValidateProfileDir = %v, want nil", err)
		}
	})
	t.Run("Network/Cookies", func(t *testing.T) {
		if err := ValidateProfileDir(mk(t, "Network/Cookies", true)); err != nil {
			t.Errorf("ValidateProfileDir = %v, want nil", err)
		}
	})
	t.Run("no Local State one level up", func(t *testing.T) {
		if err := ValidateProfileDir(mk(t, "Cookies", false)); err == nil {
			t.Error("ValidateProfileDir = nil — without Local State the DPAPI reader has no master key to decrypt with")
		}
	})
	t.Run("no cookie store", func(t *testing.T) {
		if err := ValidateProfileDir(mk(t, "", true)); err == nil {
			t.Error("ValidateProfileDir = nil for a profile holding neither Cookies nor Network/Cookies")
		}
	})
	t.Run("a file, not a directory", func(t *testing.T) {
		// Placed where a profile dir WOULD be — "Local State" beside it, so
		// the later checks cannot be what refuses it — and the message is
		// asserted, not merely the presence of an error: a file can never pass
		// the cookie-store check either, so only the wording tells the two
		// verdicts apart, and only the wording tells the operator what to fix.
		userData := filepath.Join(t.TempDir(), "User Data")
		if err := os.MkdirAll(userData, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(userData, "Local State"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(userData, "Default")
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := ValidateProfileDir(p)
		if err == nil {
			t.Fatal("ValidateProfileDir = nil for a regular file")
		}
		if !strings.Contains(err.Error(), "not a directory") {
			t.Errorf("error = %v, want it to say the path is not a directory", err)
		}
	})
	t.Run("absent", func(t *testing.T) {
		if err := ValidateProfileDir(filepath.Join(t.TempDir(), "nope")); err == nil {
			t.Error("ValidateProfileDir = nil for a directory that does not exist")
		}
	})
	t.Run("a symlink is refused, never followed", func(t *testing.T) {
		// The link sits exactly where a real profile dir would — inside a
		// User Data root that holds "Local State" — and points at a profile
		// that is itself perfectly valid. So FOLLOWING the link validates
		// everything; only refusing to follow it refuses. That is what makes
		// this the Lstat-vs-Stat mutant's only witness.
		//
		// Creating a symlink needs Developer Mode or an elevated shell on
		// Windows, so a failure to create one is a skip, not a verdict.
		target := mk(t, "Cookies", true)
		userData := filepath.Join(t.TempDir(), "User Data")
		if err := os.MkdirAll(userData, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(userData, "Local State"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(userData, "Default")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("cannot create a symlink on this host: %v", err)
		}
		if err := ValidateProfileDir(link); err == nil {
			t.Error("ValidateProfileDir = nil for a symlink — the deny-list and the \"Local State\" lookup " +
				"both judge the path as written, so a link out of it would describe a different directory " +
				"than the one the reader opens")
		}
	})
}
