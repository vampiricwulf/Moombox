package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/updater"
)

// resetUpdateGlobals clears the package-level SharedUpdateInfo and the
// updateInProgress flag between tests. /api/update/* routes mutate both,
// so leaving residue would make tests order-dependent.
func resetUpdateGlobals(t *testing.T) {
	t.Helper()
	SharedUpdateInfo.Store(nil)
	updateInProgress.Store(false)
}

func newUpdateFixture(t *testing.T, deps *UpdateRouteDeps) (chi.Router, *config.Store) {
	t.Helper()
	resetUpdateGlobals(t)
	t.Cleanup(func() { resetUpdateGlobals(t) })

	dir := t.TempDir()
	cfg := config.Defaults()
	store := config.NewStore(cfg, filepath.Join(dir, "config.toml"))

	r := chi.NewRouter()
	UpdateRoutes(r, deps, store)
	return r, store
}

// --- /api/update/status ---

func TestUpdateStatusNoRelease(t *testing.T) {
	r, _ := newUpdateFixture(t, &UpdateRouteDeps{Version: "2.6.0-test"})

	req := httptest.NewRequest("GET", "/api/update/status", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	var resp map[string]any
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp["currentVersion"] != "2.6.0-test" {
		t.Errorf("currentVersion: want 2.6.0-test, got %v", resp["currentVersion"])
	}
	if resp["available"] != false {
		t.Errorf("available: want false (no release), got %v", resp["available"])
	}
}

func TestUpdateStatusWithRelease(t *testing.T) {
	r, _ := newUpdateFixture(t, &UpdateRouteDeps{Version: "2.6.0-test"})

	// Seed a known release through the package atomic
	SharedUpdateInfo.Store(&updater.ReleaseInfo{
		Version:      "2.7.0",
		TagName:      "v2.7.0",
		ReleaseNotes: "Cool stuff",
		PublishedAt:  "2026-04-25T00:00:00Z",
	})

	req := httptest.NewRequest("GET", "/api/update/status", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	var resp map[string]any
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp["available"] != true {
		t.Errorf("available: want true, got %v", resp["available"])
	}
	if resp["version"] != "2.7.0" {
		t.Errorf("version: want 2.7.0, got %v", resp["version"])
	}
	if resp["tagName"] != "v2.7.0" {
		t.Errorf("tagName: want v2.7.0, got %v", resp["tagName"])
	}
}

// --- /api/update/check, /apply, /verify with nil Updater ---

func TestUpdateCheckNoUpdater(t *testing.T) {
	// Updater is optional in the deps; nil-Updater installs are
	// supported by main.go (e.g. dev builds with no GitHub credentials).
	// The route must surface 503 rather than nil-deref.
	r, _ := newUpdateFixture(t, &UpdateRouteDeps{Version: "2.6.0-test"})

	req := httptest.NewRequest("POST", "/api/update/check", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("check with nil Updater: want 503, got %d", rec.Code)
	}
}

func TestUpdateApplyNoUpdater(t *testing.T) {
	r, _ := newUpdateFixture(t, &UpdateRouteDeps{Version: "2.6.0-test"})

	req := httptest.NewRequest("POST", "/api/update/apply", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("apply with nil Updater: want 503, got %d", rec.Code)
	}
}

func TestUpdateVerifyNoUpdater(t *testing.T) {
	r, _ := newUpdateFixture(t, &UpdateRouteDeps{Version: "2.6.0-test"})

	req := httptest.NewRequest("POST", "/api/update/verify", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("verify with nil Updater: want 503, got %d", rec.Code)
	}
}

// --- /api/update/apply concurrency + missing-release guards ---

func TestUpdateApplyAlreadyInProgress(t *testing.T) {
	// updater.New must succeed before the CAS check happens; we don't
	// actually invoke its methods here — we trip the in-progress flag
	// first, so the route returns 409 before reaching ApplyUpdate.
	upd, err := updater.New("2.6.0-test", silentLogger{})
	if err != nil {
		t.Fatalf("updater.New: %v", err)
	}
	r, _ := newUpdateFixture(t, &UpdateRouteDeps{Version: "2.6.0-test", Updater: upd})

	updateInProgress.Store(true)

	req := httptest.NewRequest("POST", "/api/update/apply", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Errorf("apply when in-progress: want 409, got %d", rec.Code)
	}
	// The CAS must NOT have flipped — we acquired the slot before the
	// route saw it; the route must surface conflict, not steal.
	if !updateInProgress.Load() {
		t.Error("updateInProgress should still be true after 409")
	}
}

func TestUpdateApplyNoReleaseAvailable(t *testing.T) {
	upd, err := updater.New("2.6.0-test", silentLogger{})
	if err != nil {
		t.Fatalf("updater.New: %v", err)
	}
	r, _ := newUpdateFixture(t, &UpdateRouteDeps{Version: "2.6.0-test", Updater: upd})

	// Pre-condition: SharedUpdateInfo is nil (resetUpdateGlobals).
	req := httptest.NewRequest("POST", "/api/update/apply", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("apply with no release: want 400, got %d", rec.Code)
	}
	// Critical: the CAS reservation must roll back so future /check
	// + /apply cycles can proceed. If updateInProgress stayed true, we'd
	// be stuck until the next process restart.
	if updateInProgress.Load() {
		t.Error("updateInProgress should be reset after 400 / no release")
	}
}

// --- /api/update/dismiss ---

func TestUpdateDismissClearsAutoCheckAndSharedInfo(t *testing.T) {
	r, store := newUpdateFixture(t, &UpdateRouteDeps{Version: "2.6.0-test"})

	// Seed a "we have an update" state so the dismiss has something to clear.
	SharedUpdateInfo.Store(&updater.ReleaseInfo{Version: "2.7.0"})
	if err := store.Update(func(c *config.MoomboxConfig) {
		c.Updates.AutoCheckUpdates = true
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	req := httptest.NewRequest("POST", "/api/update/dismiss", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("dismiss: want 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	// AutoCheckUpdates flipped off
	var auto bool
	store.Read(func(c *config.MoomboxConfig) { auto = c.Updates.AutoCheckUpdates })
	if auto {
		t.Error("AutoCheckUpdates should be false after dismiss")
	}

	// SharedUpdateInfo cleared so /api/update/status returns available=false
	if SharedUpdateInfo.Load() != nil {
		t.Error("SharedUpdateInfo should be nil after dismiss")
	}
}

func TestUpdateDismissPersistsToDisk(t *testing.T) {
	// SaveLocked writes through the savePath; verify the on-disk TOML
	// reflects the dismiss so the next launch doesn't ask again.
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	cfg := config.Defaults()
	cfg.Updates.AutoCheckUpdates = true
	store := config.NewStore(cfg, configPath)
	// Persist initial state so the file exists and Save can rewrite it.
	if err := config.Save(cfg, configPath); err != nil {
		t.Fatalf("initial save: %v", err)
	}

	resetUpdateGlobals(t)
	t.Cleanup(func() { resetUpdateGlobals(t) })

	router := chi.NewRouter()
	UpdateRoutes(router, &UpdateRouteDeps{Version: "2.6.0-test"}, store)

	req := httptest.NewRequest("POST", "/api/update/dismiss", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("dismiss: want 200, got %d", rec.Code)
	}

	// Reload from disk and check
	reloaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if reloaded.Updates.AutoCheckUpdates {
		t.Error("on-disk AutoCheckUpdates should be false after dismiss")
	}
}
