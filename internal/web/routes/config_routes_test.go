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
	router     chi.Router
	store      *config.Store
	logLevel   atomic.Pointer[string] // last value passed to OnLogLevelChange
	parallel   atomic.Int32           // last value passed to OnMaxParallelChange
	hideAge    atomic.Bool            // OnHideFinishedAgeChanged was invoked
	channels   atomic.Bool            // OnChannelChange was invoked
	goSoft     atomic.Int32           // last value passed to OnGoSoftLimitChange
	trustProto atomic.Pointer[bool]   // last value passed to OnTrustForwardedProtoChange
	ffmpeg     atomic.Pointer[string] // last value passed to OnFfmpegPathChange
	notifs     atomic.Bool            // OnNotificationsChange was invoked
}

func newConfigRoutesFixture(t *testing.T) *configRoutesFixture {
	t.Helper()
	dir := t.TempDir()

	cfg := config.Defaults()
	store := config.NewStore(cfg, filepath.Join(dir, "config.toml"))

	r := chi.NewRouter()
	f := &configRoutesFixture{router: r, store: store}

	cb := &ConfigRoutesCallbacks{
		OnLogLevelChange:            func(level string) { f.logLevel.Store(&level) },
		OnMaxParallelChange:         func(n int) { f.parallel.Store(int32(n)) },
		OnHideFinishedAgeChanged:    func() { f.hideAge.Store(true) },
		OnChannelChange:             func() { f.channels.Store(true) },
		OnGoSoftLimitChange:         func(mb int) { f.goSoft.Store(int32(mb)) },
		OnTrustForwardedProtoChange: func(b bool) { f.trustProto.Store(&b) },
		OnFfmpegPathChange:          func(p string) { f.ffmpeg.Store(&p) },
		OnNotificationsChange:       func() { f.notifs.Store(true) },
	}
	ConfigRoutes(r, store, cb)

	return f
}

// putConfig PUTs one JSON body and fails the test on a non-200.
func putConfig(t *testing.T, f *configRoutesFixture, body map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("PUT", "/api/config", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT %v: want 200, got %d (body: %s)", body, rec.Code, rec.Body.String())
	}
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

func TestConfigPutFiresGoSoftLimitCallback(t *testing.T) {
	f := newConfigRoutesFixture(t)
	putConfig(t, f, map[string]any{"memory": map[string]any{"go_soft_limit_mb": 512}})
	if got := f.goSoft.Load(); got != 512 {
		t.Errorf("OnGoSoftLimitChange: want 512, got %d", got)
	}
}

func TestConfigPutFiresTrustForwardedProtoCallback(t *testing.T) {
	f := newConfigRoutesFixture(t)
	putConfig(t, f, map[string]any{"network": map[string]any{"trust_forwarded_proto": true}})
	if got := f.trustProto.Load(); got == nil || !*got {
		t.Errorf("OnTrustForwardedProtoChange: want true, got %v", got)
	}
}

func TestConfigPutFiresFfmpegPathCallback(t *testing.T) {
	f := newConfigRoutesFixture(t)
	// An absolute path that pathFieldError accepts (read pathFieldError: it
	// rejects traversal and, for required fields, blanks; ffmpeg_path is optional).
	putConfig(t, f, map[string]any{"paths": map[string]any{"ffmpeg_path": "C:/tools/ffmpeg.exe"}})
	if got := f.ffmpeg.Load(); got == nil || *got != "C:/tools/ffmpeg.exe" {
		t.Errorf("OnFfmpegPathChange: want C:/tools/ffmpeg.exe, got %v", got)
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
	if f.goSoft.Load() != 0 {
		t.Error("OnGoSoftLimitChange should NOT fire for port-only PUT")
	}
	if f.trustProto.Load() != nil {
		t.Error("OnTrustForwardedProtoChange should NOT fire for port-only PUT")
	}
	if f.ffmpeg.Load() != nil {
		t.Error("OnFfmpegPathChange should NOT fire for port-only PUT")
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

// TestAcquisitionModeValidation is the API half of cookies.acquisition's
// contract, and it exists because applyConfigUpdates assigns the value
// straight through — validateConfigUpdates is the ONLY thing standing between
// a typo in a PUT body and a config the cookie service has to normalise behind
// the operator's back.
//
// The accept rows are the junction guard. Without them a validator that
// rejected every value would pass the reject rows and lock the setting out of
// both UIs.
func TestAcquisitionModeValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   any
		wantErr bool
	}{
		{"auto", "auto", false},
		{"profile", "profile", false},
		{"empty means leave it at the default", "", false},
		{"unknown word", "headless", true},
		{"a near miss", "profiles", true},
		{"the dropped third value", "browser", true},
		{"wrong type is ignored, not rejected", 3.0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateConfigUpdates(map[string]any{
				"cookies": map[string]any{"acquisition": tc.value},
			})
			got := errs["cookies.acquisition"] != ""
			if got != tc.wantErr {
				t.Errorf("rejected = %v, want %v (errs: %v)", got, tc.wantErr, errs)
			}
		})
	}
}

// TestAcquisitionModeApplied pins the second half. A field that validates and
// never persists is the checklist's first named mistake, and it is invisible
// from the UI: the save reports success and the setting silently reverts on the
// next load.
func TestAcquisitionModeApplied(t *testing.T) {
	cfg := config.Defaults()
	applyConfigUpdates(cfg, map[string]any{
		"cookies": map[string]any{"acquisition": "  PROFILE  "},
	})
	if cfg.Cookies.Acquisition != "profile" {
		t.Errorf("acquisition = %q, want %q — the value is trimmed and lower-cased on the way in "+
			"so a pasted or capitalised mode does not become an unrecognised one",
			cfg.Cookies.Acquisition, "profile")
	}

	// Omitting the key must leave the stored value alone, the same way every
	// other optional cookie field behaves. A save from a UI that has not
	// rendered this control yet must not reset the operator's choice.
	kept := config.Defaults()
	kept.Cookies.Acquisition = "profile"
	applyConfigUpdates(kept, map[string]any{"cookies": map[string]any{}})
	if kept.Cookies.Acquisition != "profile" {
		t.Errorf("an update with no acquisition key reset it to %q", kept.Cookies.Acquisition)
	}
}

func TestConfigUpdatesClientTokenTTL(t *testing.T) {
	for _, bad := range []float64{0, -1, 3651} {
		u := map[string]any{"network": map[string]any{"client_token_ttl_days": bad}}
		if errs := validateConfigUpdates(u); errs["network.client_token_ttl_days"] == "" {
			t.Errorf("ttl %v accepted: %v", bad, errs)
		}
	}
	good := map[string]any{"network": map[string]any{"client_token_ttl_days": float64(30)}}
	if errs := validateConfigUpdates(good); len(errs) != 0 {
		t.Errorf("ttl 30 rejected: %v", errs)
	}
	cfg := config.Defaults()
	applyConfigUpdates(cfg, good)
	if cfg.Network.ClientTokenTTLDays != 30 {
		t.Errorf("apply: %d, want 30", cfg.Network.ClientTokenTTLDays)
	}
}

func TestConfigUpdatesProbeTargets(t *testing.T) {
	bad := map[string]any{"connectivity": map[string]any{"probe_targets": []any{"1.1.1.1:443", "8.8.8.8"}}}
	if errs := validateConfigUpdates(bad); errs["connectivity.probe_targets"] == "" {
		t.Errorf("host without port accepted: %v", errs)
	}
	empty := map[string]any{"connectivity": map[string]any{"probe_targets": []any{}}}
	if errs := validateConfigUpdates(empty); errs["connectivity.probe_targets"] == "" {
		t.Errorf("empty list accepted (config.Validate would reject it): %v", errs)
	}
	notStrings := map[string]any{"connectivity": map[string]any{"probe_targets": []any{443}}}
	if errs := validateConfigUpdates(notStrings); errs["connectivity.probe_targets"] == "" {
		t.Errorf("non-string entry accepted: %v", errs)
	}
	good := map[string]any{"connectivity": map[string]any{"probe_targets": []any{" 1.1.1.1:443 ", "[2606:4700::1111]:443"}}}
	if errs := validateConfigUpdates(good); len(errs) != 0 {
		t.Errorf("valid targets rejected: %v", errs)
	}
	cfg := config.Defaults()
	applyConfigUpdates(cfg, good)
	want := []string{"1.1.1.1:443", "[2606:4700::1111]:443"}
	if len(cfg.Connectivity.ProbeTargets) != 2 || cfg.Connectivity.ProbeTargets[0] != want[0] || cfg.Connectivity.ProbeTargets[1] != want[1] {
		t.Errorf("apply: %v, want %v (trimmed)", cfg.Connectivity.ProbeTargets, want)
	}
}

// Two Web validator ranges were wider than config.Validate, so a value the
// API's own message called valid was refused inside config.Save as an opaque
// 500 "failed to save config" (CORE-21).
//
// Mutant: restoring 1..100 / no-maximum — the 100 and 20000 rows report no
// field error.
func TestWebValidatorRangesMatchConfigValidate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		updates map[string]any
		field   string
	}{
		{"disk warn 100", map[string]any{"disk": map[string]any{"disk_warn_percent": float64(100)}}, "disk.disk_warn_percent"},
		{"disk critical 100", map[string]any{"disk": map[string]any{"disk_critical_percent": float64(100)}}, "disk.disk_critical_percent"},
		{"refresh 20000", map[string]any{"cookies": map[string]any{"refresh_interval": float64(20000)}}, "cookies.refresh_interval"},
	} {
		errs := validateConfigUpdates(tc.updates)
		if _, ok := errs[tc.field]; !ok {
			t.Errorf("%s: want a field error on %s, got %v", tc.name, tc.field, errs)
		}
	}
}

// The differential the row actually asks for: for each of the two fields the
// Web validator and config.validateOrNormalize must agree at min-1, min, max
// and max+1. A one-sided fix (tightening the maximum but not the minimum, or
// only one of the two disk fields) still passes the coarse test above.
//
// config.Validate's messages are matched on the field name AND "out of
// range" so the disk cross-check ("critical_percent N must be >
// warn_percent M"), which the single-field Web payloads cannot trigger, is
// not mistaken for a range verdict.
//
// Mutant: leaving one range divergent — e.g. disk_critical_percent at 1..100
// while disk_warn_percent moves to 1..99 — and the 100 probe for that field
// reports web=false cfg=true.
func TestWebValidatorRangeBoundariesAgreeWithConfigValidate(t *testing.T) {
	cases := []struct {
		field   string                               // the Web validator's field key
		cfgName string                               // the name config.Validate uses
		updates func(float64) map[string]any         // single-field PUT body
		apply   func(*config.MoomboxConfig, float64) // the same value on a config
		probes  []float64
	}{
		{
			field:   "disk.disk_warn_percent",
			cfgName: "disk.warn_percent",
			updates: func(v float64) map[string]any {
				return map[string]any{"disk": map[string]any{"disk_warn_percent": v}}
			},
			apply:  func(c *config.MoomboxConfig, v float64) { c.Disk.WarnPercent = int(v) },
			probes: []float64{0, 1, 99, 100, 500},
		},
		{
			field:   "disk.disk_critical_percent",
			cfgName: "disk.critical_percent",
			updates: func(v float64) map[string]any {
				return map[string]any{"disk": map[string]any{"disk_critical_percent": v}}
			},
			apply:  func(c *config.MoomboxConfig, v float64) { c.Disk.CriticalPercent = int(v) },
			probes: []float64{0, 1, 99, 100, 500},
		},
		{
			field:   "cookies.refresh_interval",
			cfgName: "cookies.refresh_interval",
			updates: func(v float64) map[string]any {
				return map[string]any{"cookies": map[string]any{"refresh_interval": v}}
			},
			apply:  func(c *config.MoomboxConfig, v float64) { c.Cookies.RefreshInterval = config.FlexDuration{Value: v} },
			probes: []float64{0, 9, 10, 10080, 10081, 20000},
		},
	}

	for _, tc := range cases {
		for _, v := range tc.probes {
			_, webRejects := validateConfigUpdates(tc.updates(v))[tc.field]

			cfg := config.Defaults()
			tc.apply(cfg, v)
			cfgRejects := false
			for _, err := range config.Validate(cfg) {
				msg := err.Error()
				if strings.Contains(msg, tc.cfgName) && strings.Contains(msg, "out of range") {
					cfgRejects = true
				}
			}

			if webRejects != cfgRejects {
				t.Errorf("%s = %v: web rejects %v, config.Validate rejects %v — the two ranges must be the same range",
					tc.field, v, webRejects, cfgRejects)
			}
		}
	}
}

// TestConfigPutAcceptsUnboundedResolution: the Web validator's floor drops from
// 1 to 0 with config.Validate's (ruling R1). A rejected 0 would make the
// dashboard unable to save what its own picker offers.
//
// Mutant: the floor left at 1 — the first case reports the field error.
func TestConfigPutAcceptsUnboundedResolution(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   float64
		wantErr bool
	}{
		{"zero is unbounded", 0, false},
		{"negative is still invalid", -1, true},
		{"a normal cap", 1080, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateConfigUpdates(map[string]any{
				"downloader": map[string]any{"max_video_resolution": tc.value},
			})
			_, got := errs["downloader.max_video_resolution"]
			if got != tc.wantErr {
				t.Errorf("max_video_resolution=%v produced error %v, want %v (errs: %v)",
					tc.value, got, tc.wantErr, errs)
			}
		})
	}
}

// TestConfigPutRefreshIntervalNullAndStringForms: both forms used to reach
// the store unchecked and come back as an opaque 500 "failed to save config".
// null stored zero, which Validate refuses (10..10080 minutes); a string was
// parsed by the apply path but skipped by the validator, so "5m" (5 minutes)
// sailed through to the same refusal.
//
// MUTANT: restore the zero reset for null — the first PUT is a 500.
// MUTANT: check only the float64 form in validateConfigUpdates — the string
// PUT is a 500 instead of a 400 naming the field.
func TestConfigPutRefreshIntervalNullAndStringForms(t *testing.T) {
	f := newConfigRoutesFixture(t)
	f.store.Update(func(c *config.MoomboxConfig) { c.Cookies.RefreshInterval = config.FlexDuration{Value: 60} })

	putConfig(t, f, map[string]any{"cookies": map[string]any{"refresh_interval": nil}})
	if got, want := f.store.Snapshot().Cookies.RefreshInterval.Value, config.Defaults().Cookies.RefreshInterval.Value; got != want {
		t.Errorf("refresh_interval after null = %v, want the default %v", got, want)
	}

	raw, _ := json.Marshal(map[string]any{"cookies": map[string]any{"refresh_interval": "5m"}})
	req := httptest.NewRequest("PUT", "/api/config", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "cookies.refresh_interval") {
		t.Errorf("PUT refresh_interval \"5m\": got %d %s, want a 400 naming cookies.refresh_interval",
			rec.Code, rec.Body.String())
	}

	putConfig(t, f, map[string]any{"cookies": map[string]any{"refresh_interval": "2h"}})
	if got := f.store.Snapshot().Cookies.RefreshInterval.Value; got != 120 {
		t.Errorf("refresh_interval after \"2h\" = %v, want 120 minutes", got)
	}
}
