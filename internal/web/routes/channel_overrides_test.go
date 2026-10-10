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
	"github.com/vampiricwulf/Moombox/internal/web"
)

func postChannel(t *testing.T, r http.Handler, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST", "/api/config/channels", bytes.NewReader(raw)))
	return rec
}

// Both channel writers accepted an out-of-range per-channel override, and
// Save's Validate then refused the whole config — a 500 "failed to save
// config" with the field's name lost, and nothing to tell an API client what
// to fix. They answer 400 naming the field now, before anything is stored.
//
// Mutant: dropping the ChannelOverrideErrors check from saveChannel or from
// the PUT channels[] arm.
func TestChannelWritersRefuseOutOfRangeOverridesBy400(t *testing.T) {
	f := newChannelRoutesFixture(t)
	for _, body := range []map[string]any{
		{"id": "UCx", "archive_slots": 0},
		{"id": "UCx", "archive_window_days": 3651},
		{"id": "UCx", "quality_preference": "1080i"},
	} {
		rec := postChannel(t, f.router, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST %v: %d, want 400 (body %s)", body, rec.Code, rec.Body.String())
			continue
		}
		for field := range body {
			if field != "id" && !strings.Contains(rec.Body.String(), field) {
				t.Errorf("POST %v: the 400 does not name %s: %s", body, field, rec.Body.String())
			}
		}
	}
	if n := f.channelsLen(); n != 0 {
		t.Errorf("a refused channel was stored (%d channels)", n)
	}

	c := newConfigRoutesFixture(t)
	rec := putConfigExpect(t, c, map[string]any{"channels": []any{
		map[string]any{"id": "UCa", "platform": "youtube"},
		map[string]any{"id": "UCb", "platform": "youtube", "archive_window_days": 0},
	}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT with channels[1].archive_window_days=0: %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Details map[string]string `json:"details"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if _, ok := resp.Details["channels[1].archive_window_days"]; !ok {
		t.Errorf("the 400 does not name channels[1].archive_window_days: %s", rec.Body.String())
	}
}

// A URL-shaped ID is resolved before it is stored. One that does not resolve
// to a channel used to be stored verbatim, and the monitor then polled
// channel_id=https://… forever; it is refused now. The resolve is a
// youtube.com fetch, so it rides the resolve route's limiter — but a plain ID,
// which every enable/disable toggle posts, never touches it.
//
// Mutant: storing the unresolved URL again, or dropping limitedBy from the
// URL branch, or applying it to the whole route.
func TestChannelAddResolvesURLsUnderTheLimiter(t *testing.T) {
	store := config.NewStore(config.Defaults(), filepath.Join(t.TempDir(), "config.toml"))
	r := chi.NewRouter()
	ChannelRoutes(r, store, func() {}, web.NewRateLimiterCtx(t.Context(), 1, time.Minute))
	count := func() (n int) {
		store.Read(func(c *config.MoomboxConfig) { n = len(c.Channels) })
		return n
	}

	// No network: a watch URL is not a channel URL, and /channel/UC… resolves locally.
	if rec := postChannel(t, r, map[string]any{"id": "https://www.youtube.com/watch?v=dQw4w9WgXcQ"}); rec.Code != http.StatusBadRequest {
		t.Errorf("a URL that is no channel: %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if n := count(); n != 0 {
		t.Fatalf("an unresolved URL was stored as a channel ID (%d channels)", n)
	}
	if rec := postChannel(t, r, map[string]any{"id": "https://www.youtube.com/channel/UCabcdefghijklmnopqrstuv"}); rec.Code != http.StatusTooManyRequests {
		t.Errorf("a second URL post inside the window: %d, want 429", rec.Code)
	}
	// Unlimited, a /channel/UC… URL resolves locally to its ID, which is
	// what is stored.
	plain := config.NewStore(config.Defaults(), filepath.Join(t.TempDir(), "config.toml"))
	pr := chi.NewRouter()
	ChannelRoutes(pr, plain, func() {}, nil)
	if rec := postChannel(t, pr, map[string]any{"id": "https://www.youtube.com/channel/UCabcdefghijklmnopqrstuv"}); rec.Code != http.StatusOK {
		t.Fatalf("a /channel/ URL: %d (body %s)", rec.Code, rec.Body.String())
	}
	plain.Read(func(c *config.MoomboxConfig) {
		if len(c.Channels) != 1 || c.Channels[0].ID != "UCabcdefghijklmnopqrstuv" || c.Channels[0].Platform != "youtube" {
			t.Errorf("stored %+v, want the resolved UC ID on youtube", c.Channels)
		}
	})

	for i := range 3 {
		if rec := postChannel(t, r, map[string]any{"id": "UCplain", "enabled": i%2 == 0, "edit": i > 0}); rec.Code != http.StatusOK {
			t.Fatalf("plain-ID post %d was limited: %d (body %s)", i, rec.Code, rec.Body.String())
		}
	}
}
