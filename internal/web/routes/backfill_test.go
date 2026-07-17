package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

// POST /api/backfill/rescan copies the check-now debounce contract
// (monitors.go) byte-for-byte; check-now itself has no route test, so these
// pin the shared contract on the copy: one kick, then 200 with the exact
// {"success":false,"debounced":true,"retryAfterMs":N} payload inside the
// 30s window, and 503 when the service callback is unwired.

func postRescan(t *testing.T, r chi.Router) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/backfill/rescan", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var resp map[string]any
	if rec.Body.Len() > 0 {
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return rec.Code, resp
}

func TestBackfillRescanKicksOnceThenDebounces(t *testing.T) {
	calls := 0
	r := chi.NewRouter()
	BackfillRoutes(r, &BackfillRouteDeps{Rescan: func() { calls++ }})

	// First call: kicks the forced sweep.
	code, resp := postRescan(t, r)
	if code != http.StatusOK {
		t.Fatalf("first rescan: want 200, got %d", code)
	}
	if resp["success"] != true {
		t.Errorf("first rescan success = %v, want true", resp["success"])
	}
	if calls != 1 {
		t.Fatalf("rescan callback ran %d times, want 1", calls)
	}

	// Second call inside the 30s window: debounced — still 200, the exact
	// debounce payload, and the callback NOT re-invoked.
	code, resp = postRescan(t, r)
	if code != http.StatusOK {
		t.Fatalf("debounced rescan: want 200, got %d", code)
	}
	if resp["success"] != false || resp["debounced"] != true {
		t.Errorf("debounced payload = %v, want success=false debounced=true", resp)
	}
	retry, ok := resp["retryAfterMs"].(float64)
	if !ok || retry <= 0 || retry > float64((30*time.Second).Milliseconds()) {
		t.Errorf("retryAfterMs = %v, want in (0, 30000]", resp["retryAfterMs"])
	}
	if calls != 1 {
		t.Errorf("debounced call re-invoked the callback (%d calls, want 1)", calls)
	}
}

func TestBackfillRescanUnavailableWithoutCallback(t *testing.T) {
	r := chi.NewRouter()
	BackfillRoutes(r, &BackfillRouteDeps{})

	code, _ := postRescan(t, r)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("nil Rescan: want 503, got %d", code)
	}
}
