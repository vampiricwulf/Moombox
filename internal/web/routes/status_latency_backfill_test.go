package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/worker"
)

// TestLatencyBackfillRoute pins GET /api/latency-backfill (owner ruling
// RV-20): the wired status in its wire shape (state, pause, until, done,
// total), read on each request, and no route when nothing is wired.
//
// Mutants: the route registered without its getter (the unwired router
// answers); the status read once at registration (the second read is stale).
func TestLatencyBackfillRoute(t *testing.T) {
	get := func(deps *StatusRouteDeps) *httptest.ResponseRecorder {
		r := chi.NewRouter()
		StatusRoute(r, deps)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/latency-backfill", nil))
		return rec
	}
	if rec := get(&StatusRouteDeps{Version: "test", StartTime: time.Now()}); rec.Code != http.StatusNotFound {
		t.Errorf("unwired: %d, want 404", rec.Code)
	}

	until := time.Date(2026, 11, 27, 14, 5, 0, 0, time.UTC)
	now := worker.LatencyBackfillStatus{State: worker.LatencyBackfillRunning, Done: 1, Total: 3}
	r := chi.NewRouter()
	StatusRoute(r, &StatusRouteDeps{Version: "test", StartTime: time.Now(),
		GetLatencyBackfill: func() worker.LatencyBackfillStatus { return now }})
	read := func() map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/latency-backfill", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/latency-backfill = %d", rec.Code)
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := read(); got["state"] != "running" || got["done"] != 1.0 || got["total"] != 3.0 || got["until"] != nil || got["pause"] != nil {
		t.Errorf("running: %v", got)
	}
	now = worker.LatencyBackfillStatus{State: worker.LatencyBackfillPaused, Pause: worker.LatencyPauseRateLimit, Until: until, Done: 2, Total: 3}
	if got := read(); got["state"] != "paused" || got["pause"] != "rate_limit" || got["until"] != "2026-11-27T14:05:00Z" || got["done"] != 2.0 {
		t.Errorf("paused: %v", got)
	}
}
