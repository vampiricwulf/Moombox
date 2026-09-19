package dpapi

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
// encrypted master key lives in a `Local State` that ChromeLocalStatePath can
// find (beside the directory for Chromium, inside it for Opera), and the cookie
// store is `Cookies` in the profile dir or `Network/Cookies` beside it on newer
// Chromium.
//
// Mutants, each named with the subtest that catches it:
//   - drop the Local State check -> "no Local State beside it or inside it"
//     validates, and the read later fails on a master key it cannot find.
//   - go back to looking only one level up -> "Opera's self-contained layout"
//     fails, and one of the three cases this key exists for is unreachable
//     again (I-2).
//   - accept only `Cookies` -> "Network/Cookies" fails, and every post-M96
//     Chromium (which moved the store into Network/) is refused.
//   - skip the "is a directory" test -> "a file, not a directory" fails on the
//     WORDING. Every later check refuses a file too, so the presence of an
//     error proves nothing here; only the message tells the operator what to
//     fix, which is why that subtest asserts it.
//   - swap Lstat for Stat -> "a symlink is refused, never followed" validates
//     a SYMLINK, and the directory judged is no longer the one named.
//   - drop os.ModeIrregular from linkLike -> "a directory junction is refused
//     with the same sentence" fails on the wording, because Go reports a
//     junction as irregular and the IsDir() arm then answers instead (m1).
//   - resolve a relative value against anything but the working directory ->
//     "a relative path is resolved against the working directory" fails (m2).
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
	t.Run("no Local State beside it or inside it", func(t *testing.T) {
		if err := ValidateProfileDir(mk(t, "Cookies", false)); err == nil {
			t.Error("ValidateProfileDir = nil — without Local State the DPAPI reader has no master key to decrypt with")
		}
	})
	t.Run("Opera's self-contained layout", func(t *testing.T) {
		// I-2. Opera collapses the User Data root into the profile directory:
		// …\Opera Software\Opera Stable holds its OWN Local State, with
		// Network/Cookies beside it and no root above. It is one of the three
		// cases cookies.dpapi_profile_dir was built for, and the parent-only
		// Local State rule refused it outright.
		//
		// Mutant: go back to looking only one level up -> this fails, and the
		// key's own documentation goes back to naming a browser it cannot read.
		profile := filepath.Join(t.TempDir(), "Opera Software", "Opera Stable")
		if err := os.MkdirAll(filepath.Join(profile, "Network"), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, rel := range []string{"Local State", filepath.Join("Network", "Cookies")} {
			if err := os.WriteFile(filepath.Join(profile, rel), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := ValidateProfileDir(profile); err != nil {
			t.Errorf("ValidateProfileDir = %v, want nil for Opera's layout", err)
		}
	})
	t.Run("a relative path is resolved against the working directory", func(t *testing.T) {
		// m2. Relative values stay legal (Chromium's own --user-data-dir
		// accepts them) and nothing here rewrites them: they resolve against
		// Moombox's WORKING directory, the same as paths.* and
		// browser_profile_dir, not against the config file's directory. Pinned
		// rather than merely documented so a later "helpful" rebase against
		// config.LoadedFrom has to argue with a test — it would make this the
		// only config path in the project that behaves differently.
		//
		// Mutant R9: resolve against anything but the CWD (os.TempDir(), the
		// config file's directory) -> "User Data/Default" names nothing and
		// this fails.
		//
		// t.Chdir rather than filepath.Rel from the test's own working
		// directory: under GOTMPDIR on another volume there IS no relative
		// path, and the subtest then SKIPPED — on the one host the chain runs
		// on, so R9 was unpinned exactly where it was being checked. Moving
		// the working directory makes the relative value trivial and the
		// assertion unconditional. (t.Chdir forbids t.Parallel; nothing in
		// this package uses it.)
		userData := filepath.Dir(mk(t, "Cookies", true))
		t.Chdir(filepath.Dir(userData))

		rel := filepath.Join("User Data", "Default")
		if err := ValidateProfileDir(rel); err != nil {
			t.Errorf("ValidateProfileDir(%q) = %v, want nil — a relative value resolves against the working directory", rel, err)
		}
	})
	t.Run("no cookie store", func(t *testing.T) {
		if err := ValidateProfileDir(mk(t, "", true)); err == nil {
			t.Error("ValidateProfileDir = nil for a profile holding neither Cookies nor Network/Cookies")
		}
	})
	t.Run("the User Data root is named as the likely mistake", func(t *testing.T) {
		// THE mistake this setting invites, and the arm it actually lands on.
		// A User Data root holds its own `Local State`, and since Opera's
		// layout was accepted (I-2) ChromeLocalStatePath takes an inside one —
		// so the root sails past the key check and fails for want of a cookie
		// store, where the message used to say only "holds neither", with the
		// "name the PROFILE directory" advice sitting on an arm the root never
		// reaches.
		//
		// Mutants:
		//   - leave the advice on the Local State arm -> this fails; the root
		//     never takes that arm.
		//   - drop the Default / Profile N test -> this fails, and the operator
		//     is told what is missing with no hint where to look.
		//   - fire the hint unconditionally -> the "no cookie store" row above
		//     is blamed on a root it is not, since mk builds …/User Data/Default
		//     with no profile children of its own.
		userData := filepath.Dir(mk(t, "Cookies", true))
		err := ValidateProfileDir(userData)
		if err == nil {
			t.Fatal("ValidateProfileDir = nil for a User Data root — it holds no cookie store of its own")
		}
		for _, want := range []string{"Default", "name the PROFILE directory", "not the User Data root"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %v, want it to contain %q", err, want)
			}
		}
	})
	t.Run("a Profile N child counts too", func(t *testing.T) {
		userData := filepath.Join(t.TempDir(), "User Data")
		if err := os.MkdirAll(filepath.Join(userData, "Profile 2"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(userData, "Local State"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := ValidateProfileDir(userData)
		if err == nil {
			t.Fatal("ValidateProfileDir = nil for a User Data root")
		}
		if !strings.Contains(err.Error(), "Profile 2") {
			t.Errorf("error = %v, want it to name the Profile 2 child — a second Chrome profile is "+
				"exactly the case an operator reaches for this setting over discovery", err)
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
		err := ValidateProfileDir(link)
		if err == nil {
			t.Fatal("ValidateProfileDir = nil for a symlink — the profile directory is judged as written, " +
				"so a link out of it would describe a different directory than the one the reader opens")
		}
		if !strings.Contains(err.Error(), "symlink or junction") {
			t.Errorf("error = %v, want the symlink/junction sentence", err)
		}
	})
	t.Run("a directory junction is refused with the same sentence", func(t *testing.T) {
		// m1, end to end. Go's Lstat reports a Windows junction as
		// os.ModeIrregular rather than os.ModeSymlink, so before linkLike
		// existed a junction fell through to the IsDir() arm and the operator
		// was told their directory was not a directory — which it visibly is.
		// Junctions are the likelier Windows shape of this mistake because,
		// unlike symlinks, they need no Developer Mode.
		//
		// mklink is cmd.exe's builtin and exists only on Windows; anywhere it
		// cannot run this is a skip, and TestLinkLikeMode carries the rule on
		// both CI legs.
		if runtime.GOOS != "windows" {
			t.Skip("junctions are a Windows filesystem feature")
		}
		target := mk(t, "Cookies", true)
		userData := filepath.Join(t.TempDir(), "User Data")
		if err := os.MkdirAll(userData, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(userData, "Local State"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(userData, "Default")
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
			t.Skipf("cannot create a junction on this host: %v (%s)", err, strings.TrimSpace(string(out)))
		}
		err := ValidateProfileDir(link)
		if err == nil {
			t.Fatal("ValidateProfileDir = nil for a junction")
		}
		if !strings.Contains(err.Error(), "symlink or junction") {
			t.Errorf("error = %v, want the symlink/junction sentence — \"is not a directory\" is the wrong "+
				"answer for a directory junction", err)
		}
	})
}

// TestChromeLocalStatePath is I-2's shared rule, and the reason the rule is
// shared at all: ValidateProfileDir decides whether a directory is usable and
// loadChromeMasterKey (dpapi_windows.go) decides where the master key is, and
// before this helper existed the two answered differently — validation looked
// one level up, the reader hard-coded one level up, and Opera keeps its
// `Local State` INSIDE the profile directory, so neither could ever see it.
//
// Order matters: the PARENT is preferred. A Chromium profile directory that
// happens to contain a stray `Local State` (a copied file, a half-finished
// profile move) must still decrypt with the real User Data key beside it.
//
// Mutants:
//   - return only the parent path -> "Opera's own Local State" fails, and the
//     whole layout is unreachable again.
//   - prefer the inside one -> "the parent wins over a stray inside" fails and
//     a normal profile decrypts against the wrong key.
//   - drop the error arm -> "neither" returns a path that does not exist and
//     the reader fails late with a file-not-found it cannot explain.
func TestChromeLocalStatePath(t *testing.T) {
	write := func(t *testing.T, path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("the User Data root beside it", func(t *testing.T) {
		userData := filepath.Join(t.TempDir(), "User Data")
		profile := filepath.Join(userData, "Default")
		if err := os.MkdirAll(profile, 0o755); err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(userData, "Local State")
		write(t, want)
		got, err := ChromeLocalStatePath(profile)
		if err != nil {
			t.Fatalf("ChromeLocalStatePath = %v, want nil", err)
		}
		if got != want {
			t.Errorf("ChromeLocalStatePath = %q, want %q", got, want)
		}
	})

	t.Run("Opera's own Local State", func(t *testing.T) {
		// Opera collapses the User Data root into the profile directory:
		// …\Opera Software\Opera Stable\Local State, with no root above it.
		profile := filepath.Join(t.TempDir(), "Opera Software", "Opera Stable")
		want := filepath.Join(profile, "Local State")
		write(t, want)
		got, err := ChromeLocalStatePath(profile)
		if err != nil {
			t.Fatalf("ChromeLocalStatePath = %v, want nil — Opera keeps its master key inside the profile dir", err)
		}
		if got != want {
			t.Errorf("ChromeLocalStatePath = %q, want %q", got, want)
		}
	})

	t.Run("the parent wins over a stray inside", func(t *testing.T) {
		userData := filepath.Join(t.TempDir(), "User Data")
		profile := filepath.Join(userData, "Default")
		want := filepath.Join(userData, "Local State")
		write(t, want)
		write(t, filepath.Join(profile, "Local State"))
		got, err := ChromeLocalStatePath(profile)
		if err != nil {
			t.Fatalf("ChromeLocalStatePath = %v, want nil", err)
		}
		if got != want {
			t.Errorf("ChromeLocalStatePath = %q, want the User Data root's %q — a stray copy inside a normal "+
				"profile must not displace the key the profile was actually encrypted with", got, want)
		}
	})

	t.Run("neither", func(t *testing.T) {
		profile := filepath.Join(t.TempDir(), "Default")
		if err := os.MkdirAll(profile, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := ChromeLocalStatePath(profile); err == nil {
			t.Error("ChromeLocalStatePath = nil error for a directory with no Local State beside it or inside it")
		}
	})
}

// TestLoadChromeMasterKeyUsesTheSharedLocalStateRule is the other half of I-2,
// and it is STRUCTURAL because it has to run on both CI legs: the reader lives
// in dpapi_windows.go behind a //go:build windows tag, so a behavioural test
// of it could only ever run on one of them — and the Linux leg is where a
// regression would land unnoticed.
//
// What it pins: loadChromeMasterKey resolves the Local State through
// ChromeLocalStatePath and nowhere else. The two used to be separate
// judgements (validation looked up, the reader hard-coded up) and Opera fell
// through the gap between them.
//
// Mutants:
//   - restore `filepath.Join(filepath.Dir(profilePath), "Local State")` -> the
//     hard-coded-parent check fires.
//   - drop the helper call -> the call check fires.
func TestLoadChromeMasterKeyUsesTheSharedLocalStateRule(t *testing.T) {
	src, err := os.ReadFile("dpapi_windows.go")
	if err != nil {
		t.Fatalf("read dpapi_windows.go: %v", err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "dpapi_windows.go", src, 0)
	if err != nil {
		t.Fatalf("parse dpapi_windows.go: %v", err)
	}

	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "loadChromeMasterKey" {
			body = fn.Body
		}
	}
	if body == nil {
		t.Fatal("dpapi_windows.go has no loadChromeMasterKey — re-anchor this test rather than deleting it")
	}

	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, body); err != nil {
		t.Fatalf("print body: %v", err)
	}
	rendered := buf.String()
	if !strings.Contains(rendered, "ChromeLocalStatePath(") {
		t.Error("loadChromeMasterKey does not call ChromeLocalStatePath — the reader and ValidateProfileDir " +
			"are judging the master key's location separately again, which is the gap Opera fell through")
	}
	if strings.Contains(rendered, `filepath.Dir(profilePath)`) {
		t.Error("loadChromeMasterKey still derives a path from filepath.Dir(profilePath) — the PARENT-only rule " +
			"is what refused Opera, whose Local State sits inside the profile dir")
	}
}

// TestLinkLikeMode is m1: a Windows directory JUNCTION must be refused with the
// SYMLINK sentence, not with "is not a directory".
//
// Go reports a junction through Lstat as os.ModeIrregular, not
// os.ModeSymlink — so before this predicate existed the junction fell through
// to the IsDir() arm and the operator was told their directory was not a
// directory, which it visibly is. Junctions are the LIKELIER Windows shape of
// this mistake, because unlike symlinks they need no Developer Mode.
//
// A mode table rather than a real junction, so the rule is pinned on both CI
// legs; TestValidateProfileDir has the end-to-end junction subtest beside it,
// which skips where mklink is unavailable.
//
// Mutant: drop os.ModeIrregular -> the junction row fails.
func TestLinkLikeMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode os.FileMode
		want bool
	}{
		{"a plain directory", os.ModeDir, false},
		{"a regular file", 0, false},
		{"a symlink", os.ModeSymlink, true},
		{"a Windows junction (Lstat reports irregular)", os.ModeIrregular, true},
		{"a symlinked directory", os.ModeDir | os.ModeSymlink, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := linkLike(tc.mode); got != tc.want {
				t.Errorf("linkLike(%v) = %v, want %v", tc.mode, got, tc.want)
			}
		})
	}
}
