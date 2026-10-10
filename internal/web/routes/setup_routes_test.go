package routes

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/web"
)

// setupFixture wires SetupRoutes against a temp store with deps that
// don't trigger external work — OnInstallYtdlp / OnRestart are nil, so
// the post-save goroutine in /api/setup/complete is a noop sleep.
type setupFixture struct {
	router chi.Router
	store  *config.Store
	auth   *web.AuthService
}

func newSetupFixture(t *testing.T) *setupFixture {
	t.Helper()
	dir := t.TempDir()

	cfg := config.Defaults()
	// The directories a complete creates, in the temp dir: the defaults
	// are relative, and left alone they land in this package's directory.
	cfg.Paths.OutputDirectory = filepath.Join(dir, "output")
	cfg.Paths.StagingDirectory = filepath.Join(dir, "staging")
	store := config.NewStore(cfg, filepath.Join(dir, "config.toml"))

	auth := web.NewAuthService()
	auth.Start()
	t.Cleanup(auth.Stop)

	r := chi.NewRouter()
	deps := &SetupDeps{
		Auth: auth,
		// OnInstallYtdlp + OnRestart left nil; the post-save goroutine
		// (which runs after /setup/complete responds) becomes a no-op
		// sleep that exits before t.Cleanup runs.
	}
	SetupRoutes(r, deps, store)
	return &setupFixture{router: r, store: store, auth: auth}
}

// --- /api/setup/status ---

func TestSetupStatusFirstRun(t *testing.T) {
	// Defaults() leaves ConfigLoaded=false; the route must report
	// isFirstRun=true so the wizard shows.
	f := newSetupFixture(t)

	req := httptest.NewRequest("GET", "/api/setup/status", nil)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	var resp map[string]any
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp["isFirstRun"] != true {
		t.Errorf("isFirstRun: want true (defaults have ConfigLoaded=false), got %v", resp["isFirstRun"])
	}
	// ffmpegValid is a real probe of `ffmpeg` on PATH; we only assert
	// the field is populated (true or false) — not whether the host
	// happens to have ffmpeg installed.
	if _, ok := resp["ffmpegValid"]; !ok {
		t.Error("response should include ffmpegValid field")
	}
}

func TestSetupStatusAfterAlreadyConfigured(t *testing.T) {
	f := newSetupFixture(t)
	if err := f.store.Update(func(c *config.MoomboxConfig) {
		c.ConfigLoaded = true
	}); err != nil {
		t.Fatalf("seed ConfigLoaded: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/setup/status", nil)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	var resp map[string]any
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp["isFirstRun"] != false {
		t.Errorf("isFirstRun: want false after ConfigLoaded=true, got %v", resp["isFirstRun"])
	}
}

// --- /api/setup/complete ---

// TestSetupCompleteRejectsNonLoopback locks the round-2 security fix
// (parallel to F25 on /api/auth/set-password): first-time setup from
// a LAN/private IP must be rejected so a LAN peer can't claim the
// admin password before the legitimate user when the operator has
// pre-flipped network_access=lan but not yet completed the setup
// wizard.
func TestSetupCompleteRejectsNonLoopback(t *testing.T) {
	f := newSetupFixture(t)

	body, _ := json.Marshal(map[string]any{"password": "first-time-secret-2026"})
	req := httptest.NewRequest("POST", "/api/setup/complete", bytes.NewReader(body))
	// LAN peer (RFC1918 private). DefaultRemoteAddr from httptest is
	// "192.0.2.1:1234" which is also non-loopback — this assignment
	// is just explicit.
	req.RemoteAddr = "192.168.1.42:54321"
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("LAN setup: want 401, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	// Live config must NOT have been touched.
	var hash string
	f.store.Read(func(c *config.MoomboxConfig) { hash = c.Network.PasswordHash })
	if hash != "" {
		t.Errorf("password hash leaked through despite loopback rejection: %q", hash)
	}
}

func TestSetupCompleteRejectsAfterAlreadyCompleted(t *testing.T) {
	// The /complete endpoint guards against re-running setup so a
	// stale frontend tab can't reset the user's password / paths.
	f := newSetupFixture(t)
	if err := f.store.Update(func(c *config.MoomboxConfig) {
		c.ConfigLoaded = true
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	body, _ := json.Marshal(map[string]any{"password": "newsecret123"})
	req := httptest.NewRequest("POST", "/api/setup/complete", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:0"
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("complete after already-configured: want 400, got %d", rec.Code)
	}
}

func TestSetupCompleteRejectsInvalidJSON(t *testing.T) {
	f := newSetupFixture(t)

	req := httptest.NewRequest("POST", "/api/setup/complete", bytes.NewReader([]byte("not-json")))
	req.RemoteAddr = "127.0.0.1:0"
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON: want 400, got %d", rec.Code)
	}
}

func TestSetupCompleteRejectsValidationFailure(t *testing.T) {
	f := newSetupFixture(t)

	body, _ := json.Marshal(map[string]any{
		"network": map[string]any{"port": 99999}, // out of range
	})
	req := httptest.NewRequest("POST", "/api/setup/complete", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:0"
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad port: want 400, got %d", rec.Code)
	}
	var resp map[string]any
	json.NewDecoder(rec.Body).Decode(&resp)
	if _, hasDetails := resp["details"]; !hasDetails {
		t.Errorf("expected details map in validation response, got %v", resp)
	}
}

// TestSetupCompleteAcceptsAbsolutePaths confirms the absolute-path fix
// reaches the setup wizard too: /api/setup/complete shares
// validateConfigUpdates with PUT /api/config, so a first run that points
// the wizard at absolute locations (the normal case for a Linux install
// writing to /var/lib/moombox, or a Windows install outside the binary's
// directory) must succeed. Traversal stays rejected on this path as well.
func TestSetupCompleteAcceptsAbsolutePaths(t *testing.T) {
	f := newSetupFixture(t)

	// The two directories a complete creates are absolute paths in the
	// temp dir — on either OS — rather than /srv/… and C:\…, which the
	// complete now really creates (and refuses when it cannot).
	media := t.TempDir()
	outputDir := filepath.Join(media, "moombox")
	body, _ := json.Marshal(map[string]any{
		"paths": map[string]any{
			"database_path":     `C:\Moombox\moombox.db`,
			"log_file_path":     "/var/log/moombox.log",
			"output_directory":  outputDir,
			"staging_directory": filepath.Join(media, "staging"),
		},
		"cookies": map[string]any{"cookie_file": "/etc/moombox/cookies.txt"},
	})
	req := httptest.NewRequest("POST", "/api/setup/complete", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:0"
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("absolute paths in setup: want 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	var got string
	f.store.Read(func(c *config.MoomboxConfig) { got = c.Paths.OutputDirectory })
	if got != outputDir {
		t.Errorf("output_directory = %q, want %q", got, outputDir)
	}

	f2 := newSetupFixture(t)
	body2, _ := json.Marshal(map[string]any{
		"paths": map[string]any{"output_directory": "../../escape"},
	})
	req2 := httptest.NewRequest("POST", "/api/setup/complete", bytes.NewReader(body2))
	req2.RemoteAddr = "127.0.0.1:0"
	rec2 := httptest.NewRecorder()
	f2.router.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Errorf("traversal in setup: want 400, got %d (body: %s)", rec2.Code, rec2.Body.String())
	}
}

func TestSetupCompleteRejectsShortPassword(t *testing.T) {
	f := newSetupFixture(t)

	body, _ := json.Marshal(map[string]any{"password": "short"})
	req := httptest.NewRequest("POST", "/api/setup/complete", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:0"
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("7-char password: want 400, got %d", rec.Code)
	}
}

func TestSetupCompleteRequiresPasswordForExternal(t *testing.T) {
	// External-without-password is a security trap; defense in depth
	// alongside the config-routes equivalent test. The setup wizard's
	// network_access selector must not let users land on "external"
	// without first setting a password.
	f := newSetupFixture(t)

	body, _ := json.Marshal(map[string]any{
		"network": map[string]any{"network_access": "external"},
		// no password
	})
	req := httptest.NewRequest("POST", "/api/setup/complete", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:0"
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("external without password: want 400, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	// Live config must NOT have flipped — copy-on-write rollback.
	var na string
	f.store.Read(func(c *config.MoomboxConfig) { na = c.Network.NetworkAccess })
	if na == "external" {
		t.Errorf("config should not have been written; network_access = %q", na)
	}
}

// TestSetupCompleteRejectsPublicAsInput mirrors the config-routes lock:
// /api/setup/complete runs the same validateConfigUpdates, so "public"
// never reaches its "password required for external access" guard — which
// checks == "external" alone. The rejection is what keeps that guard safe
// to leave narrow; see the config-routes twin for the full reasoning.
func TestSetupCompleteRejectsPublicAsInput(t *testing.T) {
	f := newSetupFixture(t)

	body, _ := json.Marshal(map[string]any{
		"network": map[string]any{"network_access": "public"},
		// no password
	})
	req := httptest.NewRequest("POST", "/api/setup/complete", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:0"
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("network_access=public: want 400, got %d (body: %s)\n"+
			"If you are intentionally accepting \"public\" here, widen the "+
			"password-required-for-external guard to cover it too.",
			rec.Code, rec.Body.String())
	}

	var na string
	f.store.Read(func(c *config.MoomboxConfig) { na = c.Network.NetworkAccess })
	if na == "public" {
		t.Errorf("config should not have been written; network_access = %q", na)
	}
}

func TestSetupCompleteHashesAndPersistsPassword(t *testing.T) {
	f := newSetupFixture(t)

	body, _ := json.Marshal(map[string]any{
		"password": "first-time-secret-2026",
		"network": map[string]any{
			"network_access": "external",
		},
	})
	req := httptest.NewRequest("POST", "/api/setup/complete", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:0"
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("setup/complete: want 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	// Password hash persisted (not stored as plaintext) AND verifies.
	var hash, na string
	f.store.Read(func(c *config.MoomboxConfig) {
		hash = c.Network.PasswordHash
		na = c.Network.NetworkAccess
	})
	if hash == "" {
		t.Fatal("password hash was not persisted")
	}
	if hash == "first-time-secret-2026" {
		t.Fatal("password stored as plaintext — must be hashed")
	}
	if !f.auth.VerifyPassword("first-time-secret-2026", hash) {
		t.Error("stored hash does not verify against original password")
	}
	if na != "external" {
		t.Errorf("network_access: want external, got %q", na)
	}
}

func TestSetupCompletePersistsNetworkConfig(t *testing.T) {
	// Verify the full applyConfigUpdates path runs and persists to disk
	// — not just the password handling. Reload from disk to confirm.
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	cfg := config.Defaults()
	cfg.Paths.OutputDirectory = filepath.Join(dir, "output")
	cfg.Paths.StagingDirectory = filepath.Join(dir, "staging")
	store := config.NewStore(cfg, configPath)

	auth := web.NewAuthService()
	auth.Start()
	t.Cleanup(auth.Stop)

	router := chi.NewRouter()
	SetupRoutes(router, &SetupDeps{Auth: auth}, store)

	body, _ := json.Marshal(map[string]any{
		"network": map[string]any{
			"port":           8123,
			"network_access": "lan",
		},
		"logs": map[string]any{
			"log_level": "DEBUG",
		},
	})
	req := httptest.NewRequest("POST", "/api/setup/complete", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:0"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("setup/complete: want 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	// Live config + on-disk config both reflect the changes.
	var port int
	var lvl string
	store.Read(func(c *config.MoomboxConfig) {
		port = c.Network.Port
		lvl = c.Logs.LogLevel
	})
	if port != 8123 {
		t.Errorf("live port: want 8123, got %d", port)
	}
	if lvl != "DEBUG" {
		t.Errorf("live log level: want DEBUG, got %q", lvl)
	}

	reloaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if reloaded.Network.Port != 8123 {
		t.Errorf("reloaded port: want 8123, got %d", reloaded.Network.Port)
	}
	if !strings.EqualFold(reloaded.Logs.LogLevel, "DEBUG") {
		t.Errorf("reloaded log level: want DEBUG, got %q", reloaded.Logs.LogLevel)
	}
}

func TestSetupCompleteStripsInstallYtdlpKeyBeforeValidation(t *testing.T) {
	// install_ytdlp_plugin is a wizard-only flag that lives outside the
	// updateConfigSchema; the route must delete it before validation
	// otherwise validateConfigUpdates would flag it as "unknown field"
	// (or pass but bloat the saved TOML with junk).
	f := newSetupFixture(t)

	body, _ := json.Marshal(map[string]any{
		"install_ytdlp_plugin": true,
		"network":              map[string]any{"port": 1234},
	})
	req := httptest.NewRequest("POST", "/api/setup/complete", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:0"
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("setup with install_ytdlp_plugin: want 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	// Make sure the key didn't end up persisted on the cfg struct or
	// raw TOML. (The cfg struct has no field for it, so applyConfigUpdates
	// would have just dropped it; the test exists to lock down the
	// pre-validation strip so a future schema change can't accidentally
	// make it a validation failure.)
	var port int
	f.store.Read(func(c *config.MoomboxConfig) { port = c.Network.Port })
	if port != 1234 {
		t.Errorf("port: want 1234, got %d", port)
	}
}

// TestSetupCompleteRefusesAnUncreatableDirectory pins W26-13 on the Web
// wizard's side: an output or staging directory that cannot be created (here
// its parent is a regular file) used to be discarded after the save, the
// route answered {"success":true} and restarted, and the first recording
// downloaded in full before failing at mux. Now the directories are made
// before anything is saved and a failure is a 400 detailed under the field;
// nothing is saved.
//
// Mutants killed: discarding config.MakeSetupDirs' error in setup_routes.go
// (200, config saved); MakeSetupDirs ignoring MkdirAll's error; keying the
// staging directory's failure under the output directory's field.
func TestSetupCompleteRefusesAnUncreatableDirectory(t *testing.T) {
	for _, key := range []string{"output_directory", "staging_directory"} {
		t.Run(key, func(t *testing.T) {
			f := newSetupFixture(t)
			blocker := filepath.Join(t.TempDir(), "not-a-dir")
			if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}

			rec := postSetupComplete(t, f.router, map[string]any{
				"paths": map[string]any{key: filepath.Join(blocker, "sub")},
			})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("an uncreatable %s: %d, want 400 (body %s)", key, rec.Code, rec.Body.String())
			}
			var resp struct {
				Details map[string]string `json:"details"`
			}
			if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
				t.Fatal(err)
			}
			if resp.Details["paths."+key] == "" || len(resp.Details) != 1 {
				t.Errorf("details %v, want the failure under paths.%s alone", resp.Details, key)
			}
			var loaded bool
			f.store.Read(func(c *config.MoomboxConfig) { loaded = c.ConfigLoaded })
			if _, err := os.Stat(f.store.SavePath()); loaded || !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the setup was saved (ConfigLoaded=%v, config.toml stat %v)", loaded, err)
			}
		})
	}
}

// postSetupComplete sends body to /api/setup/complete from loopback, as the
// wizard does.
func postSetupComplete(t *testing.T, router http.Handler, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest("POST", "/api/setup/complete", bytes.NewReader(raw))
	req.RemoteAddr = "127.0.0.1:0"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// TestSetupCompleteGuardHoldsForOverlappingCompletes pins W26-11: two
// completes that overlap — the tab Moombox opens on boot and a second one —
// both passed the "setup already completed" check, which ran before the
// lock and was never repeated, so both applied, both saved over each other
// and both restarted. Held together at the lock, exactly one is applied; the
// other is refused and the saved port is the applied one's.
//
// Mutant killed: dropping the ConfigLoaded recheck under the lock (both
// answer 200).
func TestSetupCompleteGuardHoldsForOverlappingCompletes(t *testing.T) {
	f := newSetupFixture(t)

	var atLock sync.WaitGroup
	atLock.Add(2)
	prev := setupCompleteBeforeLock
	setupCompleteBeforeLock = func() { atLock.Done(); atLock.Wait() }
	t.Cleanup(func() { setupCompleteBeforeLock = prev })

	codes := make([]int, 2)
	var done sync.WaitGroup
	for i := range codes {
		done.Add(1)
		go func() {
			defer done.Done()
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("complete %d panicked: %v", i, p)
				}
			}()
			codes[i] = postSetupComplete(t, f.router, map[string]any{
				"network": map[string]any{"port": 7001 + i},
			}).Code
		}()
	}
	done.Wait()

	applied := -1
	for i, code := range codes {
		switch code {
		case http.StatusOK:
			if applied >= 0 {
				t.Fatalf("both overlapping completes were applied: codes %v", codes)
			}
			applied = i
		case http.StatusBadRequest:
		default:
			t.Fatalf("complete %d answered %d", i, code)
		}
	}
	if applied < 0 {
		t.Fatalf("neither complete was applied: codes %v", codes)
	}
	var port int
	f.store.Read(func(c *config.MoomboxConfig) { port = c.Network.Port })
	if port != 7001+applied {
		t.Errorf("saved port %d, want %d — the refused complete's settings reached the config", port, 7001+applied)
	}
}
