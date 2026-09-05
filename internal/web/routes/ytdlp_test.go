package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/ytdlpplugin"
)

// TestYtdlpStatusRouteReportsNullPortWhenNotInstalled drives the seven-key
// wire contract through the actual handler, not just the helper: settings.js's
// loadYtdlpPluginStatus reads all seven, and `installedPort` must arrive as a
// JSON null (never 0) when no plugin file parsed — a 0 would read as a real
// port to anything stricter than a truthiness test.
//
// The plugin dir is redirected the same way the ytdlpplugin tests do it (the
// two env vars ytdlpplugin.Dir consults), so this never reads or writes the
// operator's real yt-dlp plugin folder; darwin resolves the home directory
// instead and is skipped rather than rewritten.
func TestYtdlpStatusRouteReportsNullPortWhenNotInstalled(t *testing.T) {
	dir := filepath.Clean(t.TempDir())
	t.Setenv("APPDATA", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	if got := ytdlpplugin.Dir(); got == "" || !strings.HasPrefix(got, dir) {
		t.Skipf("ytdlpplugin.Dir() is not redirectable on %s (got %q)", runtime.GOOS, got)
	}

	r := chi.NewRouter()
	YtdlpRoutes(r, func() int { return 7740 }, false)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/ytdlp-plugin/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/ytdlp-plugin/status: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}

	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
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
	if m["installed"] != false {
		t.Errorf("installed = %v, want false on an empty plugin dir", m["installed"])
	}
	if v, ok := m["installedPort"]; !ok || v != nil {
		t.Errorf("installedPort = %v (present=%v), want a present null", v, ok)
	}
	if m["currentPort"] != float64(7740) {
		t.Errorf("currentPort = %v, want 7740 (the getter's value)", m["currentPort"])
	}
}
