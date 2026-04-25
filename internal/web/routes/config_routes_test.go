package routes

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	router    chi.Router
	store     *config.Store
	logLevel  atomic.Pointer[string] // last value passed to OnLogLevelChange
	parallel  atomic.Int32           // last value passed to OnMaxParallelChange
	hideAge   atomic.Bool            // OnHideFinishedAgeChanged was invoked
	channels  atomic.Bool            // OnChannelChange was invoked
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

// --- isSafePath unit (used by validateConfigUpdates path fields) ---

func TestIsSafePath(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"", true},
		{"output", true},
		{"output/sub", true},
		{"output/sub/file.txt", true},
		{"../escape", false},
		{"output/../escape", false},
		{"/abs/path", false},
		{"\\abs\\path", false},
		{"C:\\Windows", false},
		{"D:\\path", false},
		{"a:lower", false},
	}
	for _, tt := range tests {
		if got := isSafePath(tt.in); got != tt.want {
			t.Errorf("isSafePath(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
