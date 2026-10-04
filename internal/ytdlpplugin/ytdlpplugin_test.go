package ytdlpplugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// redirectPluginDir points Dir() at a temp directory for the duration of the
// test, so nothing here writes into the operator's real yt-dlp plugin folder.
// The function reads only the environment on the two platforms CI runs
// (APPDATA on Windows, XDG_CONFIG_HOME elsewhere); darwin resolves the home
// directory instead, which a test must not rewrite, so the test skips there
// rather than pretending.
func redirectPluginDir(t *testing.T) string {
	t.Helper()
	// Cleaned: GOTMPDIR can carry forward slashes on Windows, and Dir's
	// filepath.Join normalises them, so the raw TempDir string is not a
	// prefix of its own subdirectory.
	dir := filepath.Clean(t.TempDir())
	t.Setenv("APPDATA", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	got := Dir()
	if got == "" || !strings.HasPrefix(got, dir) {
		t.Skipf("Dir() is not redirectable on %s (got %q)", runtime.GOOS, got)
	}
	return got
}

// TestStatusReportsInstallAndPortMismatch pins the computation the GET route
// and the TUI's E Y overlay share: absent → installed → the same file read
// against a different port.
func TestStatusReportsInstallAndPortMismatch(t *testing.T) {
	pluginDir := redirectPluginDir(t)

	info, err := Status(7740, false)
	if err != nil {
		t.Fatalf("Status on an empty dir: %v", err)
	}
	if info.Installed {
		t.Error("an empty plugin dir reports Installed")
	}
	if info.PluginDir != pluginDir {
		t.Errorf("PluginDir = %q, want %q", info.PluginDir, pluginDir)
	}
	if info.CurrentPort != 7740 || info.HTTPSEnabled {
		t.Errorf("CurrentPort/HTTPSEnabled = %d/%v, want 7740/false", info.CurrentPort, info.HTTPSEnabled)
	}
	if want := filepath.Join(pluginDir, "moombox"); info.ExtractedPath != want {
		t.Errorf("ExtractedPath = %q, want %q", info.ExtractedPath, want)
	}
	if info.PortMismatch {
		t.Error("a plugin that is not installed cannot have a port mismatch")
	}
	// The pointer is the wire's null: nothing was parsed, so there is no port.
	if info.InstalledPort != nil {
		t.Errorf("InstalledPort = %d on an empty dir, want nil", *info.InstalledPort)
	}

	if err := Install(7740, false); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := os.Stat(filepath.Join(pluginDir, "moombox", "yt_dlp_plugins", "extractor", "getpot_moombox.py")); err != nil {
		t.Fatalf("install wrote no plugin file: %v", err)
	}

	info, err = Status(7740, false)
	if err != nil {
		t.Fatalf("Status after install: %v", err)
	}
	if !info.Installed {
		t.Error("the installed plugin is not reported as installed")
	}
	if info.InstalledPort == nil || *info.InstalledPort != 7740 {
		t.Errorf("InstalledPort = %v, want 7740", info.InstalledPort)
	}
	if info.PortMismatch {
		t.Error("the plugin was written for this exact port and reports a mismatch")
	}

	// Same file, different live port — this is the state the overlay's I key
	// exists to fix.
	info, err = Status(7741, false)
	if err != nil {
		t.Fatalf("Status on the other port: %v", err)
	}
	if !info.Installed || info.InstalledPort == nil || *info.InstalledPort != 7740 {
		t.Errorf("Installed=%v InstalledPort=%v, want true/7740", info.Installed, info.InstalledPort)
	}
	if !info.PortMismatch {
		t.Error("a plugin pointing at 7740 while the server runs on 7741 is not flagged")
	}

	// The scheme is half the mismatch: same port, HTTPS on.
	info, err = Status(7740, true)
	if err != nil {
		t.Fatalf("Status with https: %v", err)
	}
	if !info.PortMismatch {
		t.Error("an http plugin under an https server is not flagged")
	}
	// ...and the scheme is reported, or both UIs can only show two equal
	// ports beside a "mismatch". Mutant: drop the InstalledScheme assignment.
	if info.InstalledScheme != "http" {
		t.Errorf("InstalledScheme = %q, want http", info.InstalledScheme)
	}
}

// TestInstallReportsWhyItFailed: the web install route used to write the file
// itself and answer "failed to write plugin", dropping the cause; it calls
// Install now, whose errors name the step and keep the OS error.
func TestInstallReportsWhyItFailed(t *testing.T) {
	pluginDir := redirectPluginDir(t)
	// A FILE where the plugin's directory tree must go.
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "moombox"), []byte("in the way"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Install(7740, false)
	if err == nil || !strings.Contains(err.Error(), "create plugin directory") {
		t.Errorf("Install over a blocking file = %v, want the create-directory cause", err)
	}
}

// TestInfoJSONKeys holds the wire contract of GET /api/ytdlp-plugin/status.
// The handler used to build the map inline; the struct's tags are now the only
// thing keeping settings.js's reads (loadYtdlpPluginStatus) pointed at
// real fields, and the un-omitempty pointer is what keeps "installedPort" a
// null rather than a 0 when nothing is installed.
func TestInfoJSONKeys(t *testing.T) {
	blob, err := json.Marshal(Info{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(blob, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := make([]string, 0, len(m))
	for k := range m {
		got = append(got, k)
	}
	sort.Strings(got)
	want := []string{"currentPort", "extractedPath", "httpsEnabled", "installed", "installedPort", "installedScheme", "pluginDir", "portMismatch", "unparseable"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("JSON keys = %v, want %v", got, want)
	}
	if v, ok := m["installedPort"]; !ok || v != nil {
		t.Errorf("installedPort = %v (present=%v), want a present null", v, ok)
	}
}

// TestGeneratedPluginPointsAtThisRepo: the plugin told yt-dlp users to file
// Moombox bugs at github.com/Wulf/Moombox, which is not this project. Every
// other GitHub reference in the tree says vampiricwulf.
//
// Mutant: restoring any other owner in BUG_REPORT_LOCATION fails this.
func TestGeneratedPluginPointsAtThisRepo(t *testing.T) {
	src := Generate(774, false)
	const want = "BUG_REPORT_LOCATION = 'https://github.com/vampiricwulf/Moombox/issues'"
	if !strings.Contains(src, want) {
		t.Errorf("the generated plugin does not carry %q", want)
	}
	owners := regexp.MustCompile(`github\.com/([A-Za-z0-9_.-]+)/`).FindAllStringSubmatch(src, -1)
	if len(owners) == 0 {
		t.Fatal("no github.com reference found at all — the fixture assumption is broken")
	}
	for _, m := range owners {
		if m[1] != "vampiricwulf" {
			t.Errorf("the plugin references github.com/%s/ — this project is vampiricwulf/Moombox", m[1])
		}
	}
}

// TestStatusFlagsAnUnparseablePluginFile: a file that exists but whose base-URL
// line does not parse used to report installed + no mismatch — a green badge
// and a green TUI row for a plugin yt-dlp cannot use.
//
// Mutant: dropping the else that sets Unparseable leaves it false and fails this.
func TestStatusFlagsAnUnparseablePluginFile(t *testing.T) {
	pluginDir := redirectPluginDir(t) // the file's existing helper: temp dir + skip where Dir() is not redirectable
	target := filepath.Join(pluginDir, "moombox", "yt_dlp_plugins", "extractor")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "getpot_moombox.py"), []byte("# truncated by a half-written install\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	info, err := Status(774, false)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !info.Installed {
		t.Fatal("a file is on disk — Installed must stay true")
	}
	if !info.Unparseable {
		t.Error("a plugin file that does not parse must be reported as unrecognised")
	}
	if info.PortMismatch {
		t.Error("PortMismatch is about two known ports; there is no installed port here")
	}
	if info.InstalledPort != nil {
		t.Errorf("InstalledPort = %v, want nil", *info.InstalledPort)
	}
}
