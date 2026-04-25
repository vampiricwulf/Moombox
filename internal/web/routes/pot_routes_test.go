package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/web"
)

// fakePotProvider satisfies the PotProviderInterface so tests can
// control responses without standing up a real BotGuard runtime.
type fakePotProvider struct {
	stringErr        error
	sessionToken     string
	sessionBinding   string
	sessionErr       error
	invalidateCount  atomic.Int32
	invalidateITHits atomic.Int32
	cacheKeys        []string
}

func (f *fakePotProvider) GeneratePoTokenString(ctx context.Context, contentBinding string, bypassCache bool) (string, error) {
	if f.stringErr != nil {
		return "", f.stringErr
	}
	return "fake-token", nil
}
func (f *fakePotProvider) GeneratePoTokenSession(ctx context.Context, contentBinding string, bypassCache bool) (string, string, error) {
	if f.sessionErr != nil {
		return "", "", f.sessionErr
	}
	return f.sessionToken, f.sessionBinding, nil
}
func (f *fakePotProvider) InvalidateCaches() {
	f.invalidateCount.Add(1)
}
func (f *fakePotProvider) InvalidateIntegrityTokens() {
	f.invalidateITHits.Add(1)
}
func (f *fakePotProvider) GetMinterCacheKeys() []string {
	return f.cacheKeys
}

type potFixture struct {
	router chi.Router
	prov   *fakePotProvider
}

func newPotFixture(t *testing.T) *potFixture {
	t.Helper()
	prov := &fakePotProvider{
		sessionToken:   "test-pot-string",
		sessionBinding: "binding-out",
	}
	rl := web.NewRateLimiter(100, time.Minute)
	t.Cleanup(rl.Close)

	r := chi.NewRouter()
	PotRoutes(r, &PotRoutesDeps{
		PotProvider: prov,
		StartTime:   time.Now(),
		RateLimit:   rl,
		Logger:      silentLogger{},
	})
	return &potFixture{router: r, prov: prov}
}

// loopbackPotRequest builds a POT request whose RemoteAddr is loopback
// so the LoopbackOnly middleware passes — POT endpoints are wrapped
// to be local-only since they sit on the same port as the user-facing
// API and the yt-dlp plugin is the only legitimate caller.
func loopbackPotRequest(method, target string, body any) *http.Request {
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

// --- /get_pot ---

func TestPotGetRejectsNonLoopback(t *testing.T) {
	// /get_pot is wrapped in LoopbackOnly because it bypasses
	// authentication on a local-only port. A LAN/WAN caller must be
	// rejected before reaching the rate limiter or provider.
	f := newPotFixture(t)
	req := loopbackPotRequest("POST", "/get_pot", map[string]any{"content_binding": "x"})
	req.RemoteAddr = "10.0.0.5:1234" // private LAN, not loopback
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("non-loopback: want 403, got %d", rec.Code)
	}
}

func TestPotGetSucceedsWithBinding(t *testing.T) {
	f := newPotFixture(t)
	req := loopbackPotRequest("POST", "/get_pot", map[string]any{"content_binding": "yt-vid-123"})
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("get_pot: want 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	var resp PotSessionData
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp.PoToken != "test-pot-string" {
		t.Errorf("PoToken: want test-pot-string, got %q", resp.PoToken)
	}
	if resp.ContentBinding != "binding-out" {
		t.Errorf("ContentBinding: want binding-out, got %q", resp.ContentBinding)
	}
}

func TestPotGetRejectsDataSyncID(t *testing.T) {
	// data_sync_id is a deprecated yt-dlp field; the route surfaces a
	// clean 400 with a deprecation message rather than silently
	// processing it. Mirror behavior in TS.
	f := newPotFixture(t)
	req := loopbackPotRequest("POST", "/get_pot", map[string]any{
		"content_binding": "x",
		"data_sync_id":    "old-style-id",
	})
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("data_sync_id present: want 400, got %d", rec.Code)
	}
}

func TestPotGetRejectsVisitorData(t *testing.T) {
	f := newPotFixture(t)
	req := loopbackPotRequest("POST", "/get_pot", map[string]any{
		"content_binding": "x",
		"visitor_data":    "old-vd",
	})
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("visitor_data present: want 400, got %d", rec.Code)
	}
}

func TestPotGetReturnsErrorOnProviderFailure(t *testing.T) {
	// PotProvider.GeneratePoTokenSession returning an error means
	// BotGuard is unavailable (cold-start failure, network outage).
	// Surface 500 — the yt-dlp plugin retries.
	f := newPotFixture(t)
	f.prov.sessionErr = errors.New("BotGuard unavailable")

	req := loopbackPotRequest("POST", "/get_pot", map[string]any{"content_binding": "x"})
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("provider error: want 500, got %d", rec.Code)
	}
}

// --- /invalidate_caches ---

func TestInvalidateCachesCallsProvider(t *testing.T) {
	f := newPotFixture(t)
	req := loopbackPotRequest("POST", "/invalidate_caches", nil)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("invalidate_caches: want 204, got %d", rec.Code)
	}
	if got := f.prov.invalidateCount.Load(); got != 1 {
		t.Errorf("InvalidateCaches calls: want 1, got %d", got)
	}
}

func TestInvalidateCachesRejectsNonLoopback(t *testing.T) {
	f := newPotFixture(t)
	req := loopbackPotRequest("POST", "/invalidate_caches", nil)
	req.RemoteAddr = "203.0.113.5:1234"
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-loopback: want 403, got %d", rec.Code)
	}
}

// --- /invalidate_it ---

func TestInvalidateITCallsProvider(t *testing.T) {
	f := newPotFixture(t)
	req := loopbackPotRequest("POST", "/invalidate_it", nil)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("invalidate_it: want 204, got %d", rec.Code)
	}
	if got := f.prov.invalidateITHits.Load(); got != 1 {
		t.Errorf("InvalidateIntegrityTokens calls: want 1, got %d", got)
	}
}

// --- /ping ---

func TestPingReturnsUptimeAndVersion(t *testing.T) {
	// /ping is the yt-dlp plugin's health probe — it's NOT
	// LoopbackOnly because the plugin pings before sending the first
	// /get_pot, and the response shape is documented.
	f := newPotFixture(t)
	req := httptest.NewRequest("GET", "/ping", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("ping: want 200, got %d", rec.Code)
	}
	var resp map[string]any
	json.NewDecoder(rec.Body).Decode(&resp)
	if _, ok := resp["server_uptime"]; !ok {
		t.Error("ping: missing server_uptime field")
	}
	if resp["version"] != "1.0.0" {
		t.Errorf("ping version: want 1.0.0, got %v", resp["version"])
	}
}

// --- /minter_cache (audit Q-23) ---

func TestMinterCacheReturnsKeys(t *testing.T) {
	f := newPotFixture(t)
	f.prov.cacheKeys = []string{"key1", "key2", "key3"}

	req := loopbackPotRequest("GET", "/minter_cache", nil)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("minter_cache: want 200, got %d", rec.Code)
	}
	var keys []string
	json.NewDecoder(rec.Body).Decode(&keys)
	if len(keys) != 3 {
		t.Errorf("keys: want 3, got %d", len(keys))
	}
}

func TestMinterCacheReturnsEmptyArrayForNilProvider(t *testing.T) {
	// Defensive: when the provider hasn't been initialised yet (early
	// startup or test scenarios) the route returns [] not null so
	// frontend code that .map()s over the response works without
	// special-case handling.
	rl := web.NewRateLimiter(100, time.Minute)
	t.Cleanup(rl.Close)

	r := chi.NewRouter()
	PotRoutes(r, &PotRoutesDeps{
		PotProvider: nil,
		StartTime:   time.Now(),
		RateLimit:   rl,
		Logger:      silentLogger{},
	})

	req := loopbackPotRequest("GET", "/minter_cache", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("minter_cache nil provider: want 200, got %d", rec.Code)
	}
	if rec.Body.String() != "[]\n" {
		t.Errorf("body: want %q, got %q", "[]\n", rec.Body.String())
	}
}

func TestMinterCacheRejectsNonLoopback(t *testing.T) {
	// Audit Q-23 specifically wraps /minter_cache in LoopbackOnly so
	// internal cache key data isn't reachable from LAN.
	f := newPotFixture(t)
	req := loopbackPotRequest("GET", "/minter_cache", nil)
	req.RemoteAddr = "192.168.1.50:1234"
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-loopback: want 403, got %d", rec.Code)
	}
}
