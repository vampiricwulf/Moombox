package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

// POST /api/monitors/check-now is the ORIGINAL of the debounce contract
// backfill.go copied; both now sit on the shared callDebouncer
// (internal/web/routes/debounce.go). backfill_test.go pinned the copy, and its
// header noted that check-now itself had no route test — so the fold onto the
// shared type had only half a differential. This file is the other half: the
// two routes' observable answers (status code, body keys, retryAfterMs range,
// callback count, the 503 with no callback) are pinned side by side, so an
// extraction that changed either one is caught here rather than in the field.
//
// THE MUTANT for the fold: make writeDebounced emit 429 instead of 200, or drop
// any of the three body keys — TestMonitorCheckNowKicksOnceThenDebounces and
// TestBackfillRescanKicksOnceThenDebounces both fail together, which is exactly
// what "the extraction moved nothing" has to mean.

func postCheckNow(t *testing.T, r chi.Router) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/monitors/check-now", nil)
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

func TestMonitorCheckNowKicksOnceThenDebounces(t *testing.T) {
	calls := 0
	r := chi.NewRouter()
	MonitorRoutes(r, &MonitorRouteDeps{CheckNow: func() { calls++ }})

	// First call: kicks all three monitors.
	code, resp := postCheckNow(t, r)
	if code != http.StatusOK {
		t.Fatalf("first check-now: want 200, got %d", code)
	}
	if resp["success"] != true {
		t.Errorf("first check-now success = %v, want true", resp["success"])
	}
	if calls != 1 {
		t.Fatalf("check-now callback ran %d times, want 1", calls)
	}

	// Second call inside the 30s window: debounced — still 200, the exact
	// debounce payload, and the callback NOT re-invoked.
	code, resp = postCheckNow(t, r)
	if code != http.StatusOK {
		t.Fatalf("debounced check-now: want 200, got %d", code)
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

func TestMonitorCheckNowUnavailableWithoutCallback(t *testing.T) {
	r := chi.NewRouter()
	MonitorRoutes(r, &MonitorRouteDeps{})

	code, _ := postCheckNow(t, r)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("nil CheckNow: want 503, got %d", code)
	}
}

// TestDebouncedRoutesAnswerIdentically is the fold's direct differential: the
// two routes that carried byte-identical inline copies must still answer
// byte-identically now that one type serves both. Compares the debounced
// bodies key-for-key (retryAfterMs aside, which is a clock reading).
//
// MUTANT: give one route its own wait shape (seconds instead of ms, or a
// Retry-After header on one side only) — the key sets diverge and this fails.
func TestDebouncedRoutesAnswerIdentically(t *testing.T) {
	mr := chi.NewRouter()
	MonitorRoutes(mr, &MonitorRouteDeps{CheckNow: func() {}})
	br := chi.NewRouter()
	BackfillRoutes(br, &BackfillRouteDeps{Rescan: func() {}})

	postCheckNow(t, mr)
	postRescan(t, br)

	mReq := httptest.NewRequest("POST", "/api/monitors/check-now", nil)
	mRec := httptest.NewRecorder()
	mr.ServeHTTP(mRec, mReq)
	bReq := httptest.NewRequest("POST", "/api/backfill/rescan", nil)
	bRec := httptest.NewRecorder()
	br.ServeHTTP(bRec, bReq)

	if mRec.Code != bRec.Code {
		t.Errorf("status codes diverged: check-now %d, rescan %d", mRec.Code, bRec.Code)
	}
	if got, want := mRec.Header().Get("Content-Type"), bRec.Header().Get("Content-Type"); got != want {
		t.Errorf("Content-Type diverged: check-now %q, rescan %q", got, want)
	}
	if got, want := mRec.Header().Get("Retry-After"), bRec.Header().Get("Retry-After"); got != want {
		t.Errorf("Retry-After diverged: check-now %q, rescan %q", got, want)
	}
	var m, b map[string]any
	if err := json.NewDecoder(mRec.Body).Decode(&m); err != nil {
		t.Fatalf("decode check-now: %v", err)
	}
	if err := json.NewDecoder(bRec.Body).Decode(&b); err != nil {
		t.Fatalf("decode rescan: %v", err)
	}
	if len(m) != len(b) {
		t.Fatalf("body shapes diverged: check-now %v, rescan %v", m, b)
	}
	for k, v := range m {
		bv, ok := b[k]
		if !ok {
			t.Errorf("rescan is missing key %q", k)
			continue
		}
		if k == "retryAfterMs" {
			continue // a clock reading, not a contract value
		}
		if v != bv {
			t.Errorf("key %q diverged: check-now %v, rescan %v", k, v, bv)
		}
	}
}
