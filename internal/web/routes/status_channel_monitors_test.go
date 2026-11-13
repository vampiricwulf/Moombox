package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

// TestStatusCarriesChannelMonitors pins the additive channelMonitors field of
// GET /api/status (owner ruling R-H1): sent as wired, beside an unchanged
// channelHealth, and absent when nothing is wired.
//
// Mutants: the field not added; the field replacing channelHealth.
func TestStatusCarriesChannelMonitors(t *testing.T) {
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

	if body := get(&StatusRouteDeps{Version: "test", StartTime: time.Now()}); strings.Contains(body, "channelMonitors") {
		t.Errorf("channelMonitors sent with nothing wired: %s", body)
	}

	body := get(&StatusRouteDeps{
		Version:   "test",
		StartTime: time.Now(),
		GetChannelHealth: func() map[string]any {
			return map[string]any{"youtube": []any{}, "twitch": []any{}}
		},
		GetChannelMonitors: func() any {
			return map[string]any{"youtube": [][]any{{}, {}}, "twitch": [][]any{{}}}
		},
	})
	var resp struct {
		ChannelHealth   map[string]json.RawMessage `json:"channelHealth"`
		ChannelMonitors map[string]json.RawMessage `json:"channelMonitors"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	if string(resp.ChannelMonitors["youtube"]) != "[[],[]]" || string(resp.ChannelMonitors["twitch"]) != "[[]]" {
		t.Errorf("channelMonitors = %v", resp.ChannelMonitors)
	}
	if string(resp.ChannelHealth["youtube"]) != "[]" {
		t.Errorf("channelHealth changed: %v", resp.ChannelHealth)
	}
}
