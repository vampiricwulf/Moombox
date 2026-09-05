package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
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

func TestUpdateDismissSkipsVersionAndClearsSharedInfo(t *testing.T) {
	r, store := newUpdateFixture(t, &UpdateRouteDeps{Version: "2.6.0-test"})

	// Seed a "we have an update" state so the dismiss has something to skip.
	SharedUpdateInfo.Store(&updater.ReleaseInfo{Version: "2.7.0", TagName: "v2.7.0"})
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

	// Version-scoped: the pending tag is recorded as skipped, and
	// AutoCheckUpdates stays ON — the old behavior disabled all update
	// awareness as the dialog's only non-apply action.
	var auto bool
	var skipped string
	store.Read(func(c *config.MoomboxConfig) {
		auto = c.Updates.AutoCheckUpdates
		skipped = c.Updates.SkippedVersion
	})
	if !auto {
		t.Error("AutoCheckUpdates must remain true after a version-scoped dismiss")
	}
	if skipped != "v2.7.0" {
		t.Errorf("SkippedVersion: want v2.7.0, got %q", skipped)
	}

	// SharedUpdateInfo cleared so /api/update/status returns available=false
	if SharedUpdateInfo.Load() != nil {
		t.Error("SharedUpdateInfo should be nil after dismiss")
	}
}

// TestUpdateDismissNotifiesOnDismissed: the dismiss route reports the tag it
// skipped through OnDismissed so the other UI (the TUI's badge) can drop the
// release too — the Web hides its own indicator from SharedUpdateInfo, but the
// TUI holds its own copy and would otherwise keep advertising a version the
// operator already dismissed.
func TestUpdateDismissNotifiesOnDismissed(t *testing.T) {
	var got []string
	r, _ := newUpdateFixture(t, &UpdateRouteDeps{
		Version:     "2.6.0-test",
		OnDismissed: func(tag string) { got = append(got, tag) },
	})
	SharedUpdateInfo.Store(&updater.ReleaseInfo{Version: "9.9.9", TagName: "v9.9.9"})

	req := httptest.NewRequest("POST", "/api/update/dismiss", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("dismiss: want 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if len(got) != 1 || got[0] != "v9.9.9" {
		t.Fatalf(`OnDismissed calls: want ["v9.9.9"], got %q`, got)
	}
}

// TestUpdateDismissWithoutPendingSkipsOnDismissed: nothing was skipped, so
// nothing is announced — a 400 must not clear a badge that is still valid.
func TestUpdateDismissWithoutPendingSkipsOnDismissed(t *testing.T) {
	called := false
	r, _ := newUpdateFixture(t, &UpdateRouteDeps{
		Version:     "2.6.0-test",
		OnDismissed: func(string) { called = true },
	})

	req := httptest.NewRequest("POST", "/api/update/dismiss", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("dismiss with no pending update: want 400, got %d", rec.Code)
	}
	if called {
		t.Error("OnDismissed must not fire when there was nothing to dismiss")
	}
}

// TestUpdateDismissConfigSaveFailureDoesNotNotify: the skip is not persisted,
// so nothing may act as if it were. The 500 tells the dashboard the release is
// still pending, and OnDismissed must stay silent — firing it would put out the
// TUI's badge for a version that will be offered again on the next launch,
// which is worse than the failure it is reporting.
func TestUpdateDismissConfigSaveFailureDoesNotNotify(t *testing.T) {
	resetUpdateGlobals(t)
	t.Cleanup(func() { resetUpdateGlobals(t) })

	// A DIRECTORY where the config file belongs: config.Save cannot write it,
	// on Windows or Linux, without any permission trickery.
	blocked := filepath.Join(t.TempDir(), "config.toml")
	if err := os.MkdirAll(blocked, 0o755); err != nil {
		t.Fatalf("mkdir the blocking directory: %v", err)
	}
	store := config.NewStore(config.Defaults(), blocked)

	called := false
	r := chi.NewRouter()
	UpdateRoutes(r, &UpdateRouteDeps{
		Version:     "2.6.0-test",
		OnDismissed: func(string) { called = true },
	}, store)
	SharedUpdateInfo.Store(&updater.ReleaseInfo{Version: "9.9.9", TagName: "v9.9.9"})

	req := httptest.NewRequest("POST", "/api/update/dismiss", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("dismiss with an unwritable config: want 500, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if called {
		t.Error("OnDismissed fired although the skip was never persisted")
	}
	// And the release is still pending, so the dashboard keeps showing it.
	if SharedUpdateInfo.Load() == nil {
		t.Error("SharedUpdateInfo was cleared although the skip failed to save")
	}
	var skipped string
	store.Read(func(c *config.MoomboxConfig) { skipped = c.Updates.SkippedVersion })
	if skipped != "" {
		t.Errorf("in-memory SkippedVersion = %q, want empty — DismissUpdate must roll its write back when the save fails", skipped)
	}
}

func TestUpdateDismissWithoutPendingIs400(t *testing.T) {
	r, _ := newUpdateFixture(t, &UpdateRouteDeps{Version: "2.6.0-test"})
	// No SharedUpdateInfo seeded — nothing to skip.
	req := httptest.NewRequest("POST", "/api/update/dismiss", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("dismiss with no pending update: want 400, got %d", rec.Code)
	}
}

// TestDismissUpdateSkipsExactlyThePendingTag: the helper the route and the
// TUI share persists the skipped version and clears the shared pointer only
// when it still holds that tag — a newer release found meanwhile survives.
func TestDismissUpdateSkipsExactlyThePendingTag(t *testing.T) {
	_, store := newUpdateFixture(t, &UpdateRouteDeps{Version: "2.6.0-test"})
	SharedUpdateInfo.Store(&updater.ReleaseInfo{TagName: "v9.9.9"})
	if err := DismissUpdate(store, "v9.9.9"); err != nil {
		t.Fatal(err)
	}
	var skipped string
	store.Read(func(c *config.MoomboxConfig) { skipped = c.Updates.SkippedVersion })
	if skipped != "v9.9.9" {
		t.Errorf("SkippedVersion = %q", skipped)
	}
	if SharedUpdateInfo.Load() != nil {
		t.Error("pending pointer not cleared")
	}
	SharedUpdateInfo.Store(&updater.ReleaseInfo{TagName: "v10.0.0"})
	if err := DismissUpdate(store, "v9.9.9"); err != nil {
		t.Fatal(err)
	}
	if p := SharedUpdateInfo.Load(); p == nil || p.TagName != "v10.0.0" {
		t.Error("a newer pending release must survive a stale dismiss")
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

	SharedUpdateInfo.Store(&updater.ReleaseInfo{Version: "2.7.0", TagName: "v2.7.0"})

	req := httptest.NewRequest("POST", "/api/update/dismiss", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("dismiss: want 200, got %d", rec.Code)
	}

	// Reload from disk and check the skip persisted (survives restarts) and
	// auto-check stayed enabled.
	reloaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if reloaded.Updates.SkippedVersion != "v2.7.0" {
		t.Errorf("on-disk SkippedVersion: want v2.7.0, got %q", reloaded.Updates.SkippedVersion)
	}
	if !reloaded.Updates.AutoCheckUpdates {
		t.Error("on-disk AutoCheckUpdates must remain true after a version-scoped dismiss")
	}
}
