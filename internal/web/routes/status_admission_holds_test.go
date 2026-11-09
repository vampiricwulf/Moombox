package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/jobstate"
)

// TestStatusCarriesAdmissionHolds pins the additive admissionHolds field of
// GET /api/status (owner ruling R-D3): the backlog scheduler's snapshot, its
// keys as the dashboard reads them — offline, diskHeld, rateLimitedUntil
// (absent when none stands) and each held row's attempt, of and until — and
// absent altogether when nothing is wired, so the older keys are untouched.
//
// Mutants: the field not added; rateLimitedUntil sent as the zero time.
func TestStatusCarriesAdmissionHolds(t *testing.T) {
	until := time.Date(2026, 11, 9, 14, 10, 0, 0, time.UTC)
	get := func(deps *StatusRouteDeps) string {
		r := chi.NewRouter()
		StatusRoute(r, deps)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/status = %d", rec.Code)
		}
		return rec.Body.String()
	}
	if body := get(&StatusRouteDeps{Version: "test", StartTime: time.Now()}); strings.Contains(body, "admissionHolds") {
		t.Errorf("admissionHolds sent with nothing wired: %s", body)
	}

	body := get(&StatusRouteDeps{Version: "test", StartTime: time.Now(), GetAdmissionHolds: func() jobstate.AdmissionHolds {
		return jobstate.AdmissionHolds{DiskHeld: true,
			Jobs: map[string]jobstate.BacklogHold{"j": {Attempt: 2, Of: 3, Until: until}}}
	}})
	var got struct {
		AdmissionHolds map[string]json.RawMessage `json:"admissionHolds"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	h := got.AdmissionHolds
	if string(h["diskHeld"]) != "true" || string(h["offline"]) != "false" {
		t.Errorf("admissionHolds = %s, want diskHeld true, offline false", body)
	}
	if _, ok := h["rateLimitedUntil"]; ok {
		t.Errorf("rateLimitedUntil sent with no rate-limit hold: %s", body)
	}
	if want := `{"j":{"attempt":2,"of":3,"until":"2026-11-09T14:10:00Z"}}`; string(h["jobs"]) != want {
		t.Errorf("jobs = %s, want %s", h["jobs"], want)
	}
}
