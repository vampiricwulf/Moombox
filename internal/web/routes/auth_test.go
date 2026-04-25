package routes

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/web"
)

// silentLogger discards log output for tests. The four-method shape
// matches both the narrower AuthRoutesDeps.Logger interface (Info+Warn)
// and the wider Debug+Info+Warn+Error interface used by updater.New
// and other packages, so tests across this package can share it.
type silentLogger struct{}

func (silentLogger) Debug(msg string, args ...any) {}
func (silentLogger) Info(msg string, args ...any)  {}
func (silentLogger) Warn(msg string, args ...any)  {}
func (silentLogger) Error(msg string, args ...any) {}

// authFixture wires the dependencies AuthRoutes needs against a temp
// database + config + permissive rate limiters. Returned router has the
// auth + client-token routes mounted ready to receive httptest requests.
type authFixture struct {
	router chi.Router
	store  *config.Store
	auth   *web.AuthService
	db     *database.Database
}

func newAuthFixture(t *testing.T) *authFixture {
	t.Helper()
	dir := t.TempDir()

	db, err := database.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	cfg := config.Defaults()
	store := config.NewStore(cfg, filepath.Join(dir, "config.toml"))

	auth := web.NewAuthService()
	auth.Start()
	t.Cleanup(auth.Stop)

	// Generous rate-limit budgets so tests don't flake on local-loop
	// short-window collisions.
	loginRL := web.NewRateLimiter(100, time.Minute)
	t.Cleanup(loginRL.Close)
	pwRL := web.NewRateLimiter(100, time.Minute)
	t.Cleanup(pwRL.Close)

	r := chi.NewRouter()
	deps := &AuthRoutesDeps{
		Auth:       auth,
		DB:         db,
		LoginRL:    loginRL,
		PasswordRL: pwRL,
		Logger:     silentLogger{},
	}
	AuthRoutes(r, deps, store)
	ClientTokenRoutes(r, deps)

	return &authFixture{router: r, store: store, auth: auth, db: db}
}

// setPasswordHash bypasses the route layer to seed a password hash for
// tests that exercise login / verify flows.
func (f *authFixture) setPasswordHash(t *testing.T, password string) string {
	t.Helper()
	hash, err := f.auth.HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if err := f.store.Update(func(c *config.MoomboxConfig) {
		c.Network.PasswordHash = hash
	}); err != nil {
		t.Fatalf("store.Update: %v", err)
	}
	return hash
}

// loopbackRequest builds a request whose RemoteAddr matches loopback so
// IsLocalOrPrivateRequest returns true (auth/set-password and
// remove-password allow loopback callers without a session).
func loopbackRequest(method, target string, body any) *http.Request {
	var rdr *bytes.Reader
	if body != nil {
		buf, _ := json.Marshal(body)
		rdr = bytes.NewReader(buf)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, target, rdr)
	req.RemoteAddr = "127.0.0.1:54321"
	return req
}

func decodeJSONResponse(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode response %q: %v", string(body), err)
	}
	return m
}

// --- /api/auth/status ---

func TestAuthStatusNoPassword(t *testing.T) {
	f := newAuthFixture(t)

	req := loopbackRequest("GET", "/api/auth/status", nil)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	resp := decodeJSONResponse(t, rec.Body.Bytes())
	if resp["hasPassword"] != false {
		t.Errorf("hasPassword: want false, got %v", resp["hasPassword"])
	}
	if resp["authenticated"] != false {
		t.Errorf("authenticated: want false (no session), got %v", resp["authenticated"])
	}
}

func TestAuthStatusWithPasswordAndSession(t *testing.T) {
	f := newAuthFixture(t)
	f.setPasswordHash(t, "correct-horse-battery-staple")

	// Pre-create a session so authenticated=true is observable.
	tok, err := f.auth.CreateSession()
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	req := loopbackRequest("GET", "/api/auth/status", nil)
	req.AddCookie(&http.Cookie{Name: "moombox_session", Value: tok})
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	resp := decodeJSONResponse(t, rec.Body.Bytes())
	if resp["hasPassword"] != true {
		t.Errorf("hasPassword: want true, got %v", resp["hasPassword"])
	}
	if resp["authenticated"] != true {
		t.Errorf("authenticated: want true with valid session cookie, got %v", resp["authenticated"])
	}
}

// --- /api/auth/login ---

func TestAuthLoginNoPasswordConfigured(t *testing.T) {
	f := newAuthFixture(t)

	req := loopbackRequest("POST", "/api/auth/login", map[string]string{"password": "anything"})
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("login with no password configured: want 400, got %d (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestAuthLoginWrongPassword(t *testing.T) {
	f := newAuthFixture(t)
	f.setPasswordHash(t, "secret")

	req := loopbackRequest("POST", "/api/auth/login", map[string]string{"password": "wrong"})
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong password: want 401, got %d", rec.Code)
	}
}

func TestAuthLoginSuccess(t *testing.T) {
	f := newAuthFixture(t)
	f.setPasswordHash(t, "secret")

	req := loopbackRequest("POST", "/api/auth/login", map[string]string{"password": "secret"})
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("login success: want 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	// Session cookie must be set
	var sessionCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "moombox_session" {
			sessionCookie = c
			break
		}
	}
	if sessionCookie == nil {
		t.Fatal("expected moombox_session cookie to be set on successful login")
	}
	if sessionCookie.Value == "" {
		t.Error("session cookie value is empty")
	}
	if !sessionCookie.HttpOnly {
		t.Error("session cookie must be HttpOnly")
	}

	// And the session must validate
	if !f.auth.ValidateSession(sessionCookie.Value) {
		t.Error("session created by /login does not validate")
	}
}

func TestAuthLoginEmptyPassword(t *testing.T) {
	f := newAuthFixture(t)
	f.setPasswordHash(t, "secret")

	req := loopbackRequest("POST", "/api/auth/login", map[string]string{"password": ""})
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty password: want 400, got %d", rec.Code)
	}
}

func TestAuthLoginPasswordTooLong(t *testing.T) {
	f := newAuthFixture(t)
	f.setPasswordHash(t, "secret")

	req := loopbackRequest("POST", "/api/auth/login", map[string]string{
		"password": strings.Repeat("x", 129),
	})
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("129-char password: want 400, got %d", rec.Code)
	}
}

func TestAuthLoginInvalidBody(t *testing.T) {
	f := newAuthFixture(t)
	f.setPasswordHash(t, "secret")

	req := httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader([]byte("not-json")))
	req.RemoteAddr = "127.0.0.1:54321"
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON: want 400, got %d", rec.Code)
	}
}

// --- /api/auth/logout ---

func TestAuthLogoutInvalidatesSession(t *testing.T) {
	f := newAuthFixture(t)
	tok, err := f.auth.CreateSession()
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if !f.auth.ValidateSession(tok) {
		t.Fatal("session should be valid before logout")
	}

	req := loopbackRequest("POST", "/api/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: "moombox_session", Value: tok})
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("logout: want 200, got %d", rec.Code)
	}
	if f.auth.ValidateSession(tok) {
		t.Error("session should be invalidated after logout")
	}

	// Cookie should be cleared (MaxAge < 0 sends Set-Cookie with past expiry)
	hasClear := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == "moombox_session" && c.MaxAge < 0 {
			hasClear = true
			break
		}
	}
	if !hasClear {
		t.Error("expected logout to clear the moombox_session cookie")
	}
}

// --- /api/auth/set-password ---

func TestAuthSetPasswordRejectsNonLoopbackUnauthenticated(t *testing.T) {
	f := newAuthFixture(t)

	req := httptest.NewRequest("POST", "/api/auth/set-password", bytes.NewReader([]byte(`{"newPassword":"newsecret123"}`)))
	req.RemoteAddr = "203.0.113.5:1234" // public IP; not loopback or LAN
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("non-loopback without session: want 401, got %d", rec.Code)
	}
}

func TestAuthSetPasswordTooShort(t *testing.T) {
	f := newAuthFixture(t)

	req := loopbackRequest("POST", "/api/auth/set-password", map[string]string{
		"newPassword": "short",
	})
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("7-char password: want 400, got %d", rec.Code)
	}
}

func TestAuthSetPasswordTooLong(t *testing.T) {
	f := newAuthFixture(t)

	req := loopbackRequest("POST", "/api/auth/set-password", map[string]string{
		"newPassword": strings.Repeat("x", 129),
	})
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("129-char password: want 400, got %d", rec.Code)
	}
}

func TestAuthSetPasswordWrongCurrent(t *testing.T) {
	f := newAuthFixture(t)
	f.setPasswordHash(t, "current-secret")

	req := loopbackRequest("POST", "/api/auth/set-password", map[string]any{
		"currentPassword": "wrong",
		"newPassword":     "newsecret123",
	})
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong current: want 401, got %d", rec.Code)
	}
}

func TestAuthSetPasswordSuccess(t *testing.T) {
	f := newAuthFixture(t)
	f.setPasswordHash(t, "current-secret")

	req := loopbackRequest("POST", "/api/auth/set-password", map[string]any{
		"currentPassword": "current-secret",
		"newPassword":     "newsecret123",
	})
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("set-password success: want 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	// New password should now verify; old should not.
	var newHash string
	f.store.Read(func(c *config.MoomboxConfig) { newHash = c.Network.PasswordHash })
	if !f.auth.VerifyPassword("newsecret123", newHash) {
		t.Error("new password does not verify against stored hash")
	}
	if f.auth.VerifyPassword("current-secret", newHash) {
		t.Error("old password should no longer verify")
	}
}

func TestAuthSetPasswordFirstTime(t *testing.T) {
	// No password configured (oldHash == ""); set-password is allowed and
	// skips the current-password check per the route's "if oldHash != ''".
	f := newAuthFixture(t)

	req := loopbackRequest("POST", "/api/auth/set-password", map[string]any{
		"newPassword": "first-time-pw-2026",
	})
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("first-time set-password: want 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	var hash string
	f.store.Read(func(c *config.MoomboxConfig) { hash = c.Network.PasswordHash })
	if hash == "" {
		t.Fatal("password hash was not persisted")
	}
	if !f.auth.VerifyPassword("first-time-pw-2026", hash) {
		t.Error("set password does not verify against stored hash")
	}
}

// --- /api/auth/remove-password ---

func TestAuthRemovePasswordWrongCurrent(t *testing.T) {
	f := newAuthFixture(t)
	f.setPasswordHash(t, "the-secret")

	req := loopbackRequest("POST", "/api/auth/remove-password", map[string]string{
		"currentPassword": "wrong",
	})
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong current: want 401, got %d", rec.Code)
	}

	// Hash should still be set
	var stillSet bool
	f.store.Read(func(c *config.MoomboxConfig) {
		stillSet = c.Network.PasswordHash != ""
	})
	if !stillSet {
		t.Error("password hash should not be cleared on wrong-current failure")
	}
}

func TestAuthRemovePasswordResetsExternalAccess(t *testing.T) {
	f := newAuthFixture(t)
	f.setPasswordHash(t, "the-secret")
	// Simulate an "external" config with the password protecting it.
	if err := f.store.Update(func(c *config.MoomboxConfig) {
		c.Network.NetworkAccess = "external"
	}); err != nil {
		t.Fatalf("seed external access: %v", err)
	}

	req := loopbackRequest("POST", "/api/auth/remove-password", map[string]string{
		"currentPassword": "the-secret",
	})
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("remove-password: want 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	// Password cleared AND network_access reset to localhost — defense
	// against leaving an unprotected external endpoint live.
	var hash, netAccess string
	f.store.Read(func(c *config.MoomboxConfig) {
		hash = c.Network.PasswordHash
		netAccess = c.Network.NetworkAccess
	})
	if hash != "" {
		t.Errorf("password hash should be empty, got %q", hash)
	}
	if netAccess != "localhost" {
		t.Errorf("network_access should reset to localhost, got %q", netAccess)
	}

	// Response signals the access change so frontends can prompt for re-config
	resp := decodeJSONResponse(t, rec.Body.Bytes())
	if resp["networkAccessChanged"] != true {
		t.Errorf("response should set networkAccessChanged=true, got %v", resp["networkAccessChanged"])
	}
}

func TestAuthRemovePasswordNoPasswordSet(t *testing.T) {
	f := newAuthFixture(t)

	req := loopbackRequest("POST", "/api/auth/remove-password", map[string]string{
		"currentPassword": "anything",
	})
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("remove with no password set: want 400, got %d", rec.Code)
	}
}

// --- redactIP ---

func TestRedactIP(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"192.168.1.42", "192.168.1.0"},
		{"10.0.0.1", "10.0.0.0"},
		{"203.0.113.7", "203.0.113.0"},
		{"2001:db8:1234:5678::1", "2001:db8:1234:5678::"},
		{"::1", ""}, // too few segments — refuses to leak
		{"garbage", ""},
	}
	for _, tt := range tests {
		got := redactIP(tt.in)
		if got != tt.want {
			t.Errorf("redactIP(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
