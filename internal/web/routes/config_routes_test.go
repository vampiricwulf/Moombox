package routes

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// configRoutesFixture wires ConfigRoutes against a fresh temp store.
// The Callbacks atomics let tests verify which hot-reload paths fired.
type configRoutesFixture struct {
	router   chi.Router
	store    *config.Store
	logLevel atomic.Pointer[string] // last value passed to OnLogLevelChange
	parallel atomic.Int32           // last value passed to OnMaxParallelChange
	hideAge  atomic.Bool            // OnHideFinishedAgeChanged was invoked
	channels atomic.Bool            // OnChannelChange was invoked
}

func newConfigRoutesFixture(t *testing.T) *configRoutesFixture {
	t.Helper()
	dir := t.TempDir()

	cfg := config.Defaults()
	store := config.NewStore(cfg, filepath.Join(dir, "config.toml"))

	r := chi.NewRouter()
	f := &configRoutesFixture{router: r, store: store}

	cb := &ConfigRoutesCallbacks{
		OnLogLevelChange:         func(level string) { f.logLevel.Store(&level) },
		OnMaxParallelChange:      func(n int) { f.parallel.Store(int32(n)) },
		OnHideFinishedAgeChanged: func() { f.hideAge.Store(true) },
		OnChannelChange:          func() { f.channels.Store(true) },
	}
	ConfigRoutes(r, store, cb)

	return f
}

// --- GET /api/config ---

func TestConfigGetReturnsDefaults(t *testing.T) {
	f := newConfigRoutesFixture(t)

	req := httptest.NewRequest("GET", "/api/config", nil)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/config: want 200, got %d", rec.Code)
	}

	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Default port is 774 per Defaults(); locking that down here means
	// any change to the default surfaces in this test instead of as a
	// silent UI regression.
	network := resp["network"].(map[string]any)
	if network["port"] != float64(774) {
		t.Errorf("default port: want 774, got %v", network["port"])
	}

	// hasPassword reflects empty-hash state from Defaults()
	if resp["hasPassword"] != false {
		t.Errorf("hasPassword: want false, got %v", resp["hasPassword"])
	}
}

func TestConfigGetOmitsPasswordHash(t *testing.T) {
	// PasswordHash uses `json:"-"` so it must never appear in /api/config
	// responses; this test guards against accidental tag removal that
	// would leak the hash to any client with read access.
	f := newConfigRoutesFixture(t)
	if err := f.store.Update(func(c *config.MoomboxConfig) {
		c.Network.PasswordHash = "this-must-not-leak"
	}); err != nil {
		t.Fatalf("seed hash: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/config", nil)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), "this-must-not-leak") {
		t.Error("password hash leaked into /api/config response")
	}
	if strings.Contains(rec.Body.String(), "passwordHash") {
		t.Error("passwordHash field key should not appear (json:\"-\")")
	}

	// hasPassword should still be true so the UI can hide the password form.
	var resp map[string]any
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp["hasPassword"] != true {
		t.Errorf("hasPassword: want true when hash is set, got %v", resp["hasPassword"])
	}
}

// --- PUT /api/config — validation ---

func TestConfigPutInvalidJSON(t *testing.T) {
	f := newConfigRoutesFixture(t)

	req := httptest.NewRequest("PUT", "/api/config", bytes.NewReader([]byte("not-json")))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON: want 400, got %d", rec.Code)
	}
}

func TestConfigPutValidationErrorIncludesDetails(t *testing.T) {
	// validateConfigUpdates returns per-field errors; the route surfaces
	// them in `details` so the UI can mark the offending field.
	f := newConfigRoutesFixture(t)

	body, _ := json.Marshal(map[string]any{
		"network": map[string]any{
			"port": 99999, // out of range
		},
	})
	req := httptest.NewRequest("PUT", "/api/config", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("port=99999: want 400, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	var resp map[string]any
	json.NewDecoder(rec.Body).Decode(&resp)
	details, ok := resp["details"].(map[string]any)
	if !ok {
		t.Fatalf("expected details map, got %v", resp["details"])
	}
	if _, hasPort := details["network.port"]; !hasPort {
		t.Errorf("expected network.port in details, got keys %v", details)
	}
}

func TestConfigPutHideFinishedAgeRange(t *testing.T) {
	// The API validator must enforce the same inclusive 0..365 range as
	// config.Validate (config.go ~line 465). Values OUTSIDE the range get a
	// clean 400 + per-field detail; before the upper bound was added here an
	// over-range value slipped past validation and failed later in config.Save
	// with an opaque 500 "failed to save config". The boundaries 0 and 365 are
	// accepted.
	t.Run("out-of-range rejected", func(t *testing.T) {
		for _, v := range []float64{-1, 366, 400} {
			f := newConfigRoutesFixture(t)
			var before float64
			f.store.Read(func(c *config.MoomboxConfig) { before = c.Monitors.HideFinishedAgeDays.Value })

			body, _ := json.Marshal(map[string]any{"monitors": map[string]any{"hide_finished_age_days": v}})
			req := httptest.NewRequest("PUT", "/api/config", bytes.NewReader(body))
			rec := httptest.NewRecorder()
			f.router.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("hide_finished_age_days=%v: want 400, got %d (body: %s)", v, rec.Code, rec.Body.String())
			}
			var resp map[string]any
			json.NewDecoder(rec.Body).Decode(&resp)
			details, _ := resp["details"].(map[string]any)
			if _, ok := details["monitors.hide_finished_age_days"]; !ok {
				t.Errorf("hide_finished_age_days=%v: expected details key, got %v", v, resp["details"])
			}
			// A rejected update must not mutate the live config.
			var after float64
			f.store.Read(func(c *config.MoomboxConfig) { after = c.Monitors.HideFinishedAgeDays.Value })
			if after != before {
				t.Errorf("hide_finished_age_days=%v: live config changed %v -> %v on a rejected request", v, before, after)
			}
		}
	})

	t.Run("inclusive boundaries accepted", func(t *testing.T) {
		for _, v := range []float64{0, 365} {
			f := newConfigRoutesFixture(t)
			body, _ := json.Marshal(map[string]any{"monitors": map[string]any{"hide_finished_age_days": v}})
			req := httptest.NewRequest("PUT", "/api/config", bytes.NewReader(body))
			rec := httptest.NewRecorder()
			f.router.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("hide_finished_age_days=%v: want 200, got %d (body: %s)", v, rec.Code, rec.Body.String())
			}
			var got float64
			f.store.Read(func(c *config.MoomboxConfig) { got = c.Monitors.HideFinishedAgeDays.Value })
			if got != v {
				t.Errorf("hide_finished_age_days=%v: live config = %v, want %v", v, got, v)
			}
		}
	})
}

func TestConfigPutRejectsExternalWithoutPassword(t *testing.T) {
	// Cross-cutting safeguard — prevents "I made my Moombox public but
	// forgot to set a password" foot-guns. Defense in depth alongside
	// the auth/remove-password reset.
	f := newConfigRoutesFixture(t)

	body, _ := json.Marshal(map[string]any{
		"network": map[string]any{
			"network_access": "external",
		},
	})
	req := httptest.NewRequest("PUT", "/api/config", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("external without password: want 400, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	// Must NOT have flipped the live config
	var na string
	f.store.Read(func(c *config.MoomboxConfig) { na = c.Network.NetworkAccess })
	if na == "external" {
		t.Errorf("config should not have been written, but network_access = %q", na)
	}
}

// TestConfigPutRejectsPublicAsInput locks the coupling that lets the
// password guard above stay narrow. "public" is a documented config-FILE
// alias for "external" (a deployment behind an authenticating reverse
// proxy); the API deliberately does not accept it, and the UIs only offer
// localhost/lan/external.
//
// That rejection is load-bearing, not cosmetic. applyConfigUpdates assigns
// network_access straight through, so validateConfigUpdates is the ONLY
// thing keeping "public" out of this handler — and the "must set a password
// before enabling external access" guard 670 lines below checks == "external"
// alone. Widening the accepted enum without widening that guard reopens
// passwordless-external through the API.
func TestConfigPutRejectsPublicAsInput(t *testing.T) {
	f := newConfigRoutesFixture(t)

	body, _ := json.Marshal(map[string]any{
		"network": map[string]any{
			"network_access": "public",
		},
	})
	req := httptest.NewRequest("PUT", "/api/config", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("network_access=public: want 400, got %d (body: %s)\n"+
			"If you are intentionally accepting \"public\" as an API value, widen the "+
			"passwordless-external guard in the PUT handler to cover it too.",
			rec.Code, rec.Body.String())
	}

	var na string
	f.store.Read(func(c *config.MoomboxConfig) { na = c.Network.NetworkAccess })
	if na == "public" {
		t.Errorf("config should not have been written, but network_access = %q", na)
	}
}

// TestConfigPutOmittedNetworkAccessPreservesPublic is the server-side half of
// the web settings panel's fix for a "public" config.
//
// The Network Access dropdown deliberately has no "public" option (it is a
// config-file-level alias for "external", used behind an authenticating
// reverse proxy), so Shoelace resolves the select's value to "" for such a
// config. settings.js therefore OMITS network_access from the PUT payload
// rather than sending "" — which would fail validation and 400 the whole
// request, making every other setting on the page unsavable.
//
// This locks the behaviour that makes omission the right fix: an absent
// network_access is skipped by both validateConfigUpdates and
// applyConfigUpdates, so the stored value survives and the co-submitted
// fields still apply.
func TestConfigPutOmittedNetworkAccessPreservesPublic(t *testing.T) {
	f := newConfigRoutesFixture(t)
	if err := f.store.Update(func(c *config.MoomboxConfig) {
		c.Network.NetworkAccess = "public"
		c.Network.PasswordHash = "hash-present"
	}); err != nil {
		t.Fatalf("seed public access: %v", err)
	}

	// Exactly what settings.js now sends for a "public" config: the rest of
	// the network section, with network_access absent.
	body, _ := json.Marshal(map[string]any{
		"network": map[string]any{
			"port":          8080,
			"https_enabled": false,
		},
	})
	req := httptest.NewRequest("PUT", "/api/config", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("omitted network_access: want 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	var na string
	var port int
	f.store.Read(func(c *config.MoomboxConfig) {
		na = c.Network.NetworkAccess
		port = c.Network.Port
	})
	if na != "public" {
		t.Errorf("network_access = %q, want %q preserved across a save that omitted it", na, "public")
	}
	if port != 8080 {
		t.Errorf("port = %d, want 8080 — the co-submitted field must still apply", port)
	}
}

func TestConfigPutAcceptsExternalWithPassword(t *testing.T) {
	f := newConfigRoutesFixture(t)
	if err := f.store.Update(func(c *config.MoomboxConfig) {
		c.Network.PasswordHash = "hash-present"
	}); err != nil {
		t.Fatalf("seed hash: %v", err)
	}

	body, _ := json.Marshal(map[string]any{
		"network": map[string]any{
			"network_access": "external",
		},
	})
	req := httptest.NewRequest("PUT", "/api/config", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("external+pw: want 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	var na string
	f.store.Read(func(c *config.MoomboxConfig) { na = c.Network.NetworkAccess })
	if na != "external" {
		t.Errorf("network_access: want external after PUT, got %q", na)
	}
}

// --- PUT /api/config — hot-reload callbacks ---

func TestConfigPutFiresLogLevelCallback(t *testing.T) {
	f := newConfigRoutesFixture(t)

	body, _ := json.Marshal(map[string]any{
		"logs": map[string]any{
			"log_level": "DEBUG",
		},
	})
	req := httptest.NewRequest("PUT", "/api/config", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("PUT log_level: want 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	got := f.logLevel.Load()
	if got == nil || *got != "DEBUG" {
		t.Errorf("OnLogLevelChange: want DEBUG, got %v", got)
	}
}

func TestConfigPutFiresParallelCallback(t *testing.T) {
	f := newConfigRoutesFixture(t)

	body, _ := json.Marshal(map[string]any{
		"downloader": map[string]any{
			"num_parallel_downloads": 7,
		},
	})
	req := httptest.NewRequest("PUT", "/api/config", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("PUT num_parallel: want 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if got := f.parallel.Load(); got != 7 {
		t.Errorf("OnMaxParallelChange: want 7, got %d", got)
	}
}

func TestConfigPutFiresChannelChangeCallback(t *testing.T) {
	// channels[] in the PUT payload triggers OnChannelChange so monitors
	// can re-evaluate their lists immediately rather than waiting for the
	// next poll cycle.
	f := newConfigRoutesFixture(t)

	body, _ := json.Marshal(map[string]any{
		"channels": []map[string]any{
			{"id": "UCfoo", "name": "Foo", "platform": "youtube"},
		},
	})
	req := httptest.NewRequest("PUT", "/api/config", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("PUT channels: want 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if !f.channels.Load() {
		t.Error("OnChannelChange should fire when channels[] is in the payload")
	}
}

func TestConfigPutDoesNotFireUnchangedCallbacks(t *testing.T) {
	// Hot-reload callbacks are gated on actual value changes — a no-op
	// PUT (e.g. settings UI re-saves the same form) shouldn't broadcast
	// "log level changed" to every subscriber.
	f := newConfigRoutesFixture(t)

	body, _ := json.Marshal(map[string]any{
		"network": map[string]any{
			"port": 774, // matches default — no change
		},
	})
	req := httptest.NewRequest("PUT", "/api/config", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("PUT no-op port: want 200, got %d", rec.Code)
	}
	if f.logLevel.Load() != nil {
		t.Error("OnLogLevelChange should NOT fire for port-only PUT")
	}
	if f.parallel.Load() != 0 {
		t.Error("OnMaxParallelChange should NOT fire for port-only PUT")
	}
	if f.hideAge.Load() {
		t.Error("OnHideFinishedAgeChanged should NOT fire for port-only PUT")
	}
	if f.channels.Load() {
		t.Error("OnChannelChange should NOT fire when channels[] absent")
	}
}

// --- PUT /api/config — channel array validation ---

func TestConfigPutChannelsRejectsEmptyID(t *testing.T) {
	f := newConfigRoutesFixture(t)

	body, _ := json.Marshal(map[string]any{
		"channels": []map[string]any{
			{"id": "", "platform": "youtube"},
		},
	})
	req := httptest.NewRequest("PUT", "/api/config", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty channel id: want 400, got %d", rec.Code)
	}
}

func TestConfigPutChannelsRejectsDuplicates(t *testing.T) {
	f := newConfigRoutesFixture(t)

	body, _ := json.Marshal(map[string]any{
		"channels": []map[string]any{
			{"id": "UCfoo", "platform": "youtube"},
			{"id": "UCfoo", "platform": "youtube"},
		},
	})
	req := httptest.NewRequest("PUT", "/api/config", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("duplicate channel ids: want 400, got %d", rec.Code)
	}
}

func TestConfigPutChannelsRejectsUnknownPlatform(t *testing.T) {
	f := newConfigRoutesFixture(t)

	body, _ := json.Marshal(map[string]any{
		"channels": []map[string]any{
			{"id": "UCfoo", "platform": "kick"},
		},
	})
	req := httptest.NewRequest("PUT", "/api/config", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("kick platform: want 400, got %d", rec.Code)
	}
}

// --- PUT /api/config — browser_path / browser_type persistence (Critical fix) ---

func TestConfigPutPersistsBrowserPath(t *testing.T) {
	// browser_path and browser_type were previously silently dropped by
	// applyConfigUpdates. This test guards against regression.
	cfg := &config.MoomboxConfig{}
	updates := map[string]any{
		"cookies": map[string]any{
			"browser_path": "/usr/bin/firefox",
			"browser_type": "firefox",
		},
	}
	applyConfigUpdates(cfg, updates)
	if cfg.Cookies.BrowserPath != "/usr/bin/firefox" {
		t.Errorf("BrowserPath: got %q, want /usr/bin/firefox", cfg.Cookies.BrowserPath)
	}
	if cfg.Cookies.BrowserType != "firefox" {
		t.Errorf("BrowserType: got %q, want firefox", cfg.Cookies.BrowserType)
	}
}

func TestConfigPutValidationRejectsRelativeBrowserPath(t *testing.T) {
	updates := map[string]any{
		"cookies": map[string]any{
			"browser_path": "./firefox", // relative path
			"browser_type": "firefox",
		},
	}
	errs := validateConfigUpdates(updates)
	if _, found := errs["cookies.browser_path"]; !found {
		t.Errorf("expected validation error for relative browser_path, got: %v", errs)
	}
}

func TestConfigPutValidationRejectsUnknownBrowserType(t *testing.T) {
	updates := map[string]any{
		"cookies": map[string]any{
			"browser_path": "/some/path",
			"browser_type": "not-a-browser",
		},
	}
	errs := validateConfigUpdates(updates)
	if _, found := errs["cookies.browser_path"]; !found {
		t.Errorf("expected validation error for unknown browser_type, got: %v", errs)
	}
}

func TestConfigPutBrowserPathPersistsViaHTTP(t *testing.T) {
	// E2E: exercises the full PUT /api/config → validate → apply → configStore.Read
	// chain for browser_path/browser_type. The earlier TestConfigPutPersistsBrowserPath
	// only tests applyConfigUpdates in isolation; this test catches regressions in the
	// HTTP route layer (e.g. the round-4 bug where the handler silently dropped the
	// fields before calling applyConfigUpdates).
	//
	// ValidateBrowserPathQuick checks that the path exists and is a regular
	// file, so we need a real path. os.Executable() gives us the test binary —
	// guaranteed absolute, existing, and executable on any OS.
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}

	f := newConfigRoutesFixture(t)

	body, _ := json.Marshal(map[string]any{
		"cookies": map[string]any{
			"browser_path": exe,
			"browser_type": "firefox",
		},
	})
	req := httptest.NewRequest("PUT", "/api/config", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("PUT cookies.browser_path: want 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	var browserPath, browserType string
	f.store.Read(func(c *config.MoomboxConfig) {
		browserPath = c.Cookies.BrowserPath
		browserType = c.Cookies.BrowserType
	})
	if browserPath != exe {
		t.Errorf("BrowserPath: got %q, want %q", browserPath, exe)
	}
	if browserType != "firefox" {
		t.Errorf("BrowserType: got %q, want firefox", browserType)
	}
}

// --- path-field validation (traversal only; absolute paths are allowed) ---

// TestDockerSeededAbsoluteConfigRoundTrips is the regression test for the
// shipped bug: docker/entrypoint.sh seeds every path field as an absolute
// /data/... path, and web/public/modules/settings.js resubmits the whole
// paths + cookies block on every save. While the API rejected absolute
// paths, a containerized dashboard 400'd on EVERY settings save — including
// saves that changed nothing in those sections — with no UI workaround.
func TestDockerSeededAbsoluteConfigRoundTrips(t *testing.T) {
	f := newConfigRoutesFixture(t)

	// Byte-for-byte the values docker/entrypoint.sh writes, plus the
	// mounted-Firefox-profile dir the browser-free import feature needs.
	body, _ := json.Marshal(map[string]any{
		"paths": map[string]any{
			"database_path":     "/data/moombox.db",
			"log_file_path":     "/data/moombox.log",
			"output_directory":  "/data/output",
			"staging_directory": "/data/staging",
		},
		"cookies": map[string]any{
			"cookie_file":         "/data/cookies.txt",
			"browser_profile_dir": "/data/browser-profile",
		},
	})
	req := httptest.NewRequest("PUT", "/api/config", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Docker-seeded absolute config: want 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	var got config.PathsConfig
	var cookieFile, profileDir string
	f.store.Read(func(c *config.MoomboxConfig) {
		got = c.Paths
		cookieFile = c.Cookies.CookieFile
		profileDir = c.Cookies.BrowserProfileDir
	})
	for _, c := range []struct{ name, got, want string }{
		{"database_path", got.DatabasePath, "/data/moombox.db"},
		{"log_file_path", got.LogFilePath, "/data/moombox.log"},
		{"output_directory", got.OutputDirectory, "/data/output"},
		{"staging_directory", got.StagingDirectory, "/data/staging"},
		{"cookie_file", cookieFile, "/data/cookies.txt"},
		{"browser_profile_dir", profileDir, "/data/browser-profile"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

// TestConfigRequiredPathFieldsRejectEmpty closes the other half of the
// API-vs-config.Validate divergence around path fields. config.Validate
// refuses to save an empty database_path / log_file_path / output_directory
// / staging_directory / cookie_file, and the TUI settings panel already
// blocks them — but the API accepted them, applied them, and only failed
// inside config.Save, surfacing as an opaque 500 "failed to save config"
// with no field named. Same unsavable-settings-page experience as the
// absolute-path bug, from the opposite direction.
//
// ffmpeg_path and browser_profile_dir are deliberately absent: empty is
// meaningful for both ("use ffmpeg from PATH", "auto-cookies unconfigured")
// and config.Validate permits it.
func TestConfigRequiredPathFieldsRejectEmpty(t *testing.T) {
	cases := []struct{ section, key string }{
		{"paths", "database_path"},
		{"paths", "log_file_path"},
		{"paths", "output_directory"},
		{"paths", "staging_directory"},
		{"cookies", "cookie_file"},
	}
	for _, c := range cases {
		for _, empty := range []string{"", "   "} {
			f := newConfigRoutesFixture(t)
			body, _ := json.Marshal(map[string]any{c.section: map[string]any{c.key: empty}})
			req := httptest.NewRequest("PUT", "/api/config", bytes.NewReader(body))
			rec := httptest.NewRecorder()
			f.router.ServeHTTP(rec, req)

			field := c.section + "." + c.key
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s=%q: want 400, got %d (body: %s)", field, empty, rec.Code, rec.Body.String())
				continue
			}
			var resp map[string]any
			json.NewDecoder(rec.Body).Decode(&resp)
			details, _ := resp["details"].(map[string]any)
			if _, ok := details[field]; !ok {
				t.Errorf("%s=%q: expected details key %q, got %v", field, empty, field, resp["details"])
			}
		}
	}

	// The optional siblings must still accept empty.
	for _, c := range []struct{ section, key string }{
		{"paths", "ffmpeg_path"},
		{"cookies", "browser_profile_dir"},
		{"network", "tls_cert_path"},
		{"network", "tls_key_path"},
	} {
		errs := validateConfigUpdates(map[string]any{c.section: map[string]any{c.key: ""}})
		if msg, bad := errs[c.section+"."+c.key]; bad {
			t.Errorf("%s.%s=\"\" rejected: %s", c.section, c.key, msg)
		}
	}
}

// TestConfigPathFieldsAcceptAbsoluteRejectTraversal locks the post-fix
// contract for every path-shaped field the API validates. Absolute paths
// (POSIX and Windows drive-letter) are legitimate: PUT /api/config is
// admin-only, config.toml has always accepted them by hand, and the TUI
// accepts them — the Web UI was the sole outlier. ".." segments stay
// rejected as a typo/sanity guard.
func TestConfigPathFieldsAcceptAbsoluteRejectTraversal(t *testing.T) {
	fields := []struct{ section, key string }{
		{"network", "tls_cert_path"},
		{"network", "tls_key_path"},
		{"paths", "log_file_path"},
		{"paths", "database_path"},
		{"paths", "output_directory"},
		{"paths", "staging_directory"},
		{"paths", "ffmpeg_path"},
		{"cookies", "cookie_file"},
		{"cookies", "browser_profile_dir"},
	}
	accepted := []string{
		"/data/moombox",
		`C:\Moombox\data`,
		`\\server\share\moombox`, // UNC
		"./relative/still/fine",
		"my..file.txt",  // two dots inside a NAME, not a ".." segment
		"..hidden/file", // segment starts with .. but is not ".."
	}
	rejected := []string{
		"../escape",
		"output/../escape",
		`C:\data\..\escape`,
		"..",
		`C:..\escape`, // drive-relative traversal
	}

	for _, fl := range fields {
		for _, v := range accepted {
			errs := validateConfigUpdates(map[string]any{fl.section: map[string]any{fl.key: v}})
			if msg, bad := errs[fl.section+"."+fl.key]; bad {
				t.Errorf("%s.%s=%q rejected: %s", fl.section, fl.key, v, msg)
			}
		}
		for _, v := range rejected {
			errs := validateConfigUpdates(map[string]any{fl.section: map[string]any{fl.key: v}})
			if _, bad := errs[fl.section+"."+fl.key]; !bad {
				t.Errorf("%s.%s=%q accepted, want traversal rejection", fl.section, fl.key, v)
			}
		}
	}
}

// --- network.trusted_proxies (validate + apply) ---

func TestConfigUpdatesTrustedProxies(t *testing.T) {
	// validateConfigUpdates: entries must be IPs or CIDRs.
	bad := map[string]any{"network": map[string]any{
		"trusted_proxies": []any{"172.18.0.2", "not-an-ip"},
	}}
	if errs := validateConfigUpdates(bad); errs["network.trusted_proxies"] == "" {
		t.Errorf("expected a network.trusted_proxies validation error, got %v", errs)
	}
	good := map[string]any{"network": map[string]any{
		"trusted_proxies": []any{"172.18.0.2", "10.0.0.0/8"},
	}}
	if errs := validateConfigUpdates(good); len(errs) != 0 {
		t.Errorf("valid entries rejected: %v", errs)
	}

	// applyConfigUpdates: array applied; empty array clears.
	cfg := config.Defaults()
	applyConfigUpdates(cfg, good)
	if len(cfg.Network.TrustedProxies) != 2 || cfg.Network.TrustedProxies[0] != "172.18.0.2" {
		t.Errorf("apply: got %v, want [172.18.0.2 10.0.0.0/8]", cfg.Network.TrustedProxies)
	}
	applyConfigUpdates(cfg, map[string]any{"network": map[string]any{"trusted_proxies": []any{}}})
	if len(cfg.Network.TrustedProxies) != 0 {
		t.Errorf("apply empty: got %v, want cleared", cfg.Network.TrustedProxies)
	}
}
