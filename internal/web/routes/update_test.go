package routes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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
	req.RemoteAddr = "127.0.0.1:50000" // a local caller: the route is loopback-only
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
	req.RemoteAddr = "127.0.0.1:50000" // a local caller: the route is loopback-only
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
	req.RemoteAddr = "127.0.0.1:50000" // a local caller: the route is loopback-only
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

// TestUpdateDismissNotifiesOnCleared: the dismiss route reports the tag it
// skipped through OnCleared so the other UI (the TUI's badge) can drop the
// release too — the Web hides its own indicator from SharedUpdateInfo, but the
// TUI holds its own copy and would otherwise keep advertising a version the
// operator already dismissed.
func TestUpdateDismissNotifiesOnCleared(t *testing.T) {
	var got []string
	r, _ := newUpdateFixture(t, &UpdateRouteDeps{
		Version:   "2.6.0-test",
		OnCleared: func(tag string) { got = append(got, tag) },
	})
	SharedUpdateInfo.Store(&updater.ReleaseInfo{Version: "9.9.9", TagName: "v9.9.9"})

	req := httptest.NewRequest("POST", "/api/update/dismiss", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("dismiss: want 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if len(got) != 1 || got[0] != "v9.9.9" {
		t.Fatalf(`OnCleared calls: want ["v9.9.9"], got %q`, got)
	}
}

// TestUpdateDismissWithoutPendingSkipsOnCleared: nothing was skipped, so
// nothing is announced — a 400 must not clear a badge that is still valid.
func TestUpdateDismissWithoutPendingSkipsOnCleared(t *testing.T) {
	called := false
	r, _ := newUpdateFixture(t, &UpdateRouteDeps{
		Version:   "2.6.0-test",
		OnCleared: func(string) { called = true },
	})

	req := httptest.NewRequest("POST", "/api/update/dismiss", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("dismiss with no pending update: want 400, got %d", rec.Code)
	}
	if called {
		t.Error("OnCleared must not fire when there was nothing to dismiss")
	}
}

// TestUpdateDismissConfigSaveFailureDoesNotNotify: the skip is not persisted,
// so nothing may act as if it were. The 500 tells the dashboard the release is
// still pending, and OnCleared must stay silent — firing it would put out the
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
		Version:   "2.6.0-test",
		OnCleared: func(string) { called = true },
	}, store)
	SharedUpdateInfo.Store(&updater.ReleaseInfo{Version: "9.9.9", TagName: "v9.9.9"})

	req := httptest.NewRequest("POST", "/api/update/dismiss", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("dismiss with an unwritable config: want 500, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if called {
		t.Error("OnCleared fired although the skip was never persisted")
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

// --- /api/update/check quota gate + /api/update/release-notes validation ---

// TestUpdateCheckIsDebounced is the WEB-12 pin. Each call spends one of
// GitHub's 60/h unauthenticated requests, and a held key (unauthenticated on a
// lan install) could exhaust the quota and suppress the daily auto-check for an
// hour — on a box whose YouTube/Twitch extractors rot without updates. The
// SCHEDULED check is unaffected: checkAndBroadcastUpdate (cmd/moombox/helpers.go)
// calls upd.CheckForUpdate directly and never travels this route.
//
// The request context is cancelled so CheckForUpdate fails before it dials:
// deps.Updater is a concrete *updater.Updater whose API base is unexported, so
// there is no way to point it at a test server from this package, and a real
// network call has no place in a unit test. What matters is that the quota gate
// stamped on the ATTEMPT — the request that spent the quota — and refused the
// second call.
//
// THE MUTANT: no debounce — the second call returns 502 (another attempted
// GitHub request) instead of a 200 debounced answer.
//
// The first call's body also carries the check's cause. It used to say only
// "check failed", so the dashboard could not show why (rate limit, no
// release, ...). Mutant: answer the fixed string again — the body test fails.
func TestUpdateCheckIsDebounced(t *testing.T) {
	upd, err := updater.New("2.6.0-test", silentLogger{})
	if err != nil {
		t.Fatalf("updater.New: %v", err)
	}
	r, _ := newUpdateFixture(t, &UpdateRouteDeps{Updater: upd, Version: "2.6.0-test"})

	call := func() *httptest.ResponseRecorder {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // the HTTP transport returns before it dials
		req := httptest.NewRequest("POST", "/api/update/check", nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	first := call()
	if first.Code != http.StatusBadGateway {
		t.Fatalf("first call: want 502 from the cancelled check, got %d", first.Code)
	}
	var firstBody map[string]any
	json.NewDecoder(first.Body).Decode(&firstBody)
	if msg, _ := firstBody["error"].(string); !strings.Contains(msg, "canceled") {
		t.Errorf("first call's error = %q, want the check's cause (context canceled)", msg)
	}

	rec := call()
	if rec.Code != http.StatusOK {
		t.Fatalf("second call: want 200 with a debounced body, got %d (%s)", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp["debounced"] != true {
		t.Errorf("second call inside the window: want debounced:true, got %v — every call spends one of "+
			"GitHub's 60/h", resp)
	}
	if ms, _ := resp["retryAfterMs"].(float64); ms <= 0 {
		t.Errorf("retryAfterMs: want a positive wait, got %v", resp["retryAfterMs"])
	}
}

// TestReleaseNotesRejectsANonVersionParam — the value is interpolated into the
// GitHub API path (updater.FetchReleaseNotes builds
// ".../releases/tags/v"+version) with no escaping. The host is fixed and the
// method is GET, so this is hygiene, not a hole; validating it keeps it that
// way. MUTANT: drop the regex — the request is made and the answer is a 502
// from GitHub instead of a 400 from us.
func TestReleaseNotesRejectsANonVersionParam(t *testing.T) {
	upd, err := updater.New("2.6.0-test", silentLogger{})
	if err != nil {
		t.Fatalf("updater.New: %v", err)
	}
	r, _ := newUpdateFixture(t, &UpdateRouteDeps{Updater: upd, Version: "2.6.0-test"})

	// "2.6.0?x=1" is the one that is not merely untidy: unvalidated, it made
	// the route answer 200 with v2.6.0's real notes, because the `?` ended
	// GitHub's PATH and everything after it became a query string. The others
	// reached the network as bogus tags (502).
	for _, bad := range []string{"../../../../users/attacker", "2.6", "latest", "2.6.0 ", "2.6.0/x", "2.6.0?x=1", "2.6.0%2f..", "v", "2.6.0.1"} {
		req := httptest.NewRequest("GET", "/api/update/release-notes?version="+url.QueryEscape(bad), nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("version=%q: want 400, got %d — the value reaches api.github.com's path unescaped",
				bad, rec.Code)
		}
	}
}

// TestReleaseNotesValidatesTheDefaultedVersionToo pins the ORDER: the regex runs
// AFTER the "no ?version= means the running version" default, so a build whose
// version string is malformed (this fixture's "2.6.0 beta") fails loudly here
// instead of quietly querying a tag GitHub has never had.
//
// The fixture is a version the regex still refuses. It used to be "2.6.0-test",
// which the pre-release widening (W-CW7) turned into a VALID version — and the
// request below carries an uncancelled context, so this committed test would
// have started dialling api.github.com. The fixture moves with the regex.
//
// MUTANT: validate before the default — an empty ?version= skips the check
// entirely and the malformed running version reaches the GitHub path anyway.
func TestReleaseNotesValidatesTheDefaultedVersionToo(t *testing.T) {
	upd, err := updater.New("2.6.0 beta", silentLogger{})
	if err != nil {
		t.Fatalf("updater.New: %v", err)
	}
	r, _ := newUpdateFixture(t, &UpdateRouteDeps{Updater: upd, Version: "2.6.0 beta"})

	req := httptest.NewRequest("GET", "/api/update/release-notes", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("no ?version= with a malformed running version: want 400, got %d", rec.Code)
	}
}

// TestReleaseNotesAcceptsEveryPublishedTagShape — the regex must not reject a
// version Moombox has actually shipped. The corpus is the real `git tag` set's
// shapes (v2.6.4 … v2.8.8) written both with and without the leading v, since
// the Web sends the bare version and the TUI's release-notes overlay sends the
// tag. An accepted value must NOT 400; it goes on to fail at the network
// (502 here, since the request context is cancelled before it dials).
//
// The last three rows are the pre-release shapes release.yml preserves into
// main.version: a build tagged v2.6.0-test.1 sends its own version here, and
// before W-CW7 it was answered "invalid version" for a tag that exists.
//
// MUTANT: tighten the regex to `^\d+\.\d+\.\d+$` — every tag-shaped value the
// TUI sends starts 400ing. MUTANT: drop the `(?:-[0-9A-Za-z.-]+)?` group — the
// three pre-release rows 400.
func TestReleaseNotesAcceptsEveryPublishedTagShape(t *testing.T) {
	upd, err := updater.New("2.6.0-test", silentLogger{})
	if err != nil {
		t.Fatalf("updater.New: %v", err)
	}
	r, _ := newUpdateFixture(t, &UpdateRouteDeps{Updater: upd, Version: "2.6.0-test"})

	for _, good := range []string{"2.6.4", "v2.6.4", "2.6.30", "v2.6.32", "2.7.0", "v2.8.8", "2.8.8", "10.0.0",
		"v2.6.0-test.1", "3.0.0-rc.1", "v3.0.0-rc1"} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // never dials; a 400 here would be the regex, not the network
		req := httptest.NewRequest("GET", "/api/update/release-notes?version="+url.QueryEscape(good), nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code == http.StatusBadRequest {
			t.Errorf("version=%q: rejected as malformed, but Moombox has published that shape", good)
		}
	}
}

// TestUpdateApplyRefusesALANPeerWithALoopbackOrigin: the gate used to read
// only the Origin, which the client chooses — a LAN peer sending
// `Origin: http://localhost:774` passed it. The direct peer must be loopback.
//
// Mutant: drop the IsLoopbackRequest check from updateApplyOriginAllowed.
func TestUpdateApplyRefusesALANPeerWithALoopbackOrigin(t *testing.T) {
	for _, tc := range []struct {
		peer, origin string
		want         bool
	}{
		{"192.168.1.20:50000", "http://localhost:774", false},
		{"192.168.1.20:50000", "", false},
		{"127.0.0.1:50000", "http://localhost:774", true},
		{"127.0.0.1:50000", "", true},
		{"127.0.0.1:50000", "http://192.168.1.10:774", false},
	} {
		req := httptest.NewRequest("POST", "/api/update/apply", nil)
		req.RemoteAddr = tc.peer
		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}
		if got := updateApplyOriginAllowed(req); got != tc.want {
			t.Errorf("peer %s origin %q: allowed = %v, want %v", tc.peer, tc.origin, got, tc.want)
		}
	}
}

// A check that finds nothing newer than the running version means the pending
// release was pulled from GitHub: the badge offered an update whose download
// no longer exists, and apply would fetch a dead asset. The check withdraws
// it and announces the tag through OnCleared, so the TUI and every open
// dashboard drop their own copies. With nothing pending, nothing is
// announced.
//
// Mutants: the check route leaving SharedUpdateInfo alone on an up-to-date
// answer, and announcing a clear when nothing was pending.
func TestUpdateCheckUpToDateWithdrawsThePendingRelease(t *testing.T) {
	orig := checkForUpdate
	t.Cleanup(func() { checkForUpdate = orig })
	checkForUpdate = func(*updater.Updater, context.Context) (*updater.ReleaseInfo, error) { return nil, nil }

	upd, err := updater.New("2.6.0-test", silentLogger{})
	if err != nil {
		t.Fatalf("updater.New: %v", err)
	}
	for _, pending := range []bool{true, false} {
		var cleared []string
		r, _ := newUpdateFixture(t, &UpdateRouteDeps{
			Updater:   upd,
			Version:   "2.6.0-test",
			OnCleared: func(tag string) { cleared = append(cleared, tag) },
		})
		if pending {
			SharedUpdateInfo.Store(&updater.ReleaseInfo{Version: "9.9.9", TagName: "v9.9.9"})
		}

		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest("POST", "/api/update/check", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("check: want 200, got %d (body: %s)", rec.Code, rec.Body.String())
		}
		if SharedUpdateInfo.Load() != nil {
			t.Errorf("pending=%v: SharedUpdateInfo still set after an up-to-date check", pending)
		}
		want := []string(nil)
		if pending {
			want = []string{"v9.9.9"}
		}
		if strings.Join(cleared, ",") != strings.Join(want, ",") {
			t.Errorf("pending=%v: OnCleared calls = %q, want %q", pending, cleared, want)
		}
	}
}

// An up-to-date answer withdraws only the release pending when the check
// started. Another check can find a release during this one's GitHub round
// trip; withdrawing whatever was pending at the end withdrew that, and every
// UI's badge with it until the next daily check.
//
// Mutant: the route loading SharedUpdateInfo after the check — v9.9.10 is
// withdrawn.
func TestUpdateCheckUpToDateKeepsAReleaseFoundDuringIt(t *testing.T) {
	newer := &updater.ReleaseInfo{Version: "9.9.10", TagName: "v9.9.10"}
	orig := checkForUpdate
	t.Cleanup(func() { checkForUpdate = orig })
	checkForUpdate = func(*updater.Updater, context.Context) (*updater.ReleaseInfo, error) {
		SharedUpdateInfo.Store(newer) // another check found it meanwhile
		return nil, nil
	}

	upd, err := updater.New("2.6.0-test", silentLogger{})
	if err != nil {
		t.Fatalf("updater.New: %v", err)
	}
	var cleared []string
	r, _ := newUpdateFixture(t, &UpdateRouteDeps{
		Updater:   upd,
		Version:   "2.6.0-test",
		OnCleared: func(tag string) { cleared = append(cleared, tag) },
	})
	SharedUpdateInfo.Store(&updater.ReleaseInfo{Version: "9.9.9", TagName: "v9.9.9"})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST", "/api/update/check", nil))
	if got := SharedUpdateInfo.Load(); got != newer {
		t.Errorf("SharedUpdateInfo = %+v, want the release found during the check", got)
	}
	if len(cleared) != 0 {
		t.Errorf("OnCleared = %q, want nothing withdrawn", cleared)
	}
}

// A slow link's download outlives what a browser waits for a response —
// Firefox gives up after 300 s — and the abort cancelled the request context
// the apply ran under: the very download the updater's stall timer exists to
// let finish. The apply now runs detached from the request.
//
// Mutant: the route passing r.Context() — the apply sees it cancelled.
func TestUpdateApplyOutlivesTheRequest(t *testing.T) {
	orig := applyUpdate
	t.Cleanup(func() { applyUpdate = orig })
	var sawCancelled bool
	applyUpdate = func(_ *updater.Updater, ctx context.Context, _ *updater.ReleaseInfo) error {
		sawCancelled = ctx.Err() != nil
		return errors.New("stop here")
	}
	upd, err := updater.New("2.6.0-test", silentLogger{})
	if err != nil {
		t.Fatalf("updater.New: %v", err)
	}
	r, _ := newUpdateFixture(t, &UpdateRouteDeps{Version: "2.6.0-test", Updater: upd})
	SharedUpdateInfo.Store(&updater.ReleaseInfo{Version: "9.9.9", TagName: "v9.9.9"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the browser has already stopped waiting
	req := httptest.NewRequest("POST", "/api/update/apply", nil).WithContext(ctx)
	req.RemoteAddr = "127.0.0.1:50000"
	r.ServeHTTP(httptest.NewRecorder(), req)
	if sawCancelled {
		t.Error("the apply ran under the request's cancelled context")
	}
}
