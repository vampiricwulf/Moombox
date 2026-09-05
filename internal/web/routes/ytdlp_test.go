package routes

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// redirectPluginDir points ytdlpPluginDir() at a temp directory for the
// duration of the test, so nothing here writes into the operator's real
// yt-dlp plugin folder. The function reads only the environment on the two
// platforms CI runs (APPDATA on Windows, XDG_CONFIG_HOME elsewhere); darwin
// resolves the home directory instead, which a test must not rewrite, so the
// test skips there rather than pretending.
func redirectPluginDir(t *testing.T) string {
	t.Helper()
	// Cleaned: GOTMPDIR can carry forward slashes on Windows, and
	// ytdlpPluginDir's filepath.Join normalises them, so the raw TempDir
	// string is not a prefix of its own subdirectory.
	dir := filepath.Clean(t.TempDir())
	t.Setenv("APPDATA", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	got := ytdlpPluginDir()
	if got == "" || !strings.HasPrefix(got, dir) {
		t.Skipf("ytdlpPluginDir() is not redirectable on %s (got %q)", runtime.GOOS, got)
	}
	return got
}

// TestYtdlpPluginStatusReportsInstallAndPortMismatch pins the computation the
// GET route and the TUI's R Y overlay now share: absent → installed → the
// same file read against a different port.
func TestYtdlpPluginStatusReportsInstallAndPortMismatch(t *testing.T) {
	pluginDir := redirectPluginDir(t)

	info, err := YtdlpPluginStatus(7740, false)
	if err != nil {
		t.Fatalf("YtdlpPluginStatus on an empty dir: %v", err)
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

	if err := InstallYtdlpPlugin(7740, false); err != nil {
		t.Fatalf("InstallYtdlpPlugin: %v", err)
	}
	if _, err := os.Stat(filepath.Join(pluginDir, "moombox", "yt_dlp_plugins", "extractor", "getpot_moombox.py")); err != nil {
		t.Fatalf("install wrote no plugin file: %v", err)
	}

	info, err = YtdlpPluginStatus(7740, false)
	if err != nil {
		t.Fatalf("YtdlpPluginStatus after install: %v", err)
	}
	if !info.Installed {
		t.Error("the installed plugin is not reported as installed")
	}
	if info.InstalledPort != 7740 {
		t.Errorf("InstalledPort = %d, want 7740", info.InstalledPort)
	}
	if info.PortMismatch {
		t.Error("the plugin was written for this exact port and reports a mismatch")
	}

	// Same file, different live port — this is the state the overlay's I key
	// exists to fix.
	info, err = YtdlpPluginStatus(7741, false)
	if err != nil {
		t.Fatalf("YtdlpPluginStatus on the other port: %v", err)
	}
	if !info.Installed || info.InstalledPort != 7740 {
		t.Errorf("Installed=%v InstalledPort=%d, want true/7740", info.Installed, info.InstalledPort)
	}
	if !info.PortMismatch {
		t.Error("a plugin pointing at 7740 while the server runs on 7741 is not flagged")
	}

	// The scheme is half the mismatch: same port, HTTPS on.
	info, err = YtdlpPluginStatus(7740, true)
	if err != nil {
		t.Fatalf("YtdlpPluginStatus with https: %v", err)
	}
	if !info.PortMismatch {
		t.Error("an http plugin under an https server is not flagged")
	}
}

// TestYtdlpPluginInfoJSONKeys holds the wire contract of
// GET /api/ytdlp-plugin/status. The handler used to build the map inline; the
// struct's tags are now the only thing keeping settings.js's seven reads
// (loadYtdlpPluginStatus) pointed at real fields.
func TestYtdlpPluginInfoJSONKeys(t *testing.T) {
	blob, err := json.Marshal(YtdlpPluginInfo{})
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
	want := []string{"currentPort", "extractedPath", "httpsEnabled", "installed", "installedPort", "pluginDir", "portMismatch"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("JSON keys = %v, want %v", got, want)
	}
}
