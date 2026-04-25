package routes

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
)

type channelRoutesFixture struct {
	router        chi.Router
	store         *config.Store
	channelChange atomic.Int32 // count of OnChannelChange invocations
}

func newChannelRoutesFixture(t *testing.T) *channelRoutesFixture {
	t.Helper()
	dir := t.TempDir()

	cfg := config.Defaults()
	store := config.NewStore(cfg, filepath.Join(dir, "config.toml"))

	r := chi.NewRouter()
	f := &channelRoutesFixture{router: r, store: store}
	ChannelRoutes(r, store, func() { f.channelChange.Add(1) })
	return f
}

// channelsLen returns the current channel-list length under store.Read so
// assertions don't race with the route's writer.
func (f *channelRoutesFixture) channelsLen() int {
	var n int
	f.store.Read(func(c *config.MoomboxConfig) { n = len(c.Channels) })
	return n
}

// --- POST /api/config/channels ---

func TestChannelAddInsertsNewChannel(t *testing.T) {
	f := newChannelRoutesFixture(t)

	body, _ := json.Marshal(config.ChannelConfig{
		ID:       "UCfoo",
		Name:     "Foo",
		Platform: "youtube",
	})
	req := httptest.NewRequest("POST", "/api/config/channels", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST channel: want 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if got := f.channelsLen(); got != 1 {
		t.Errorf("channel count: want 1, got %d", got)
	}
	if f.channelChange.Load() != 1 {
		t.Errorf("OnChannelChange: want 1 invocation, got %d", f.channelChange.Load())
	}
}

func TestChannelAddUpdatesExistingByID(t *testing.T) {
	// Same ID = upsert, not duplicate. The route uses the position-in-slice
	// for the in-place update so display order is stable across edits.
	f := newChannelRoutesFixture(t)
	if err := f.store.Update(func(c *config.MoomboxConfig) {
		c.Channels = []config.ChannelConfig{{ID: "UCfoo", Name: "OldName", Platform: "youtube"}}
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	body, _ := json.Marshal(config.ChannelConfig{
		ID:       "UCfoo",
		Name:     "NewName",
		Platform: "youtube",
	})
	req := httptest.NewRequest("POST", "/api/config/channels", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST upsert: want 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if got := f.channelsLen(); got != 1 {
		t.Errorf("channel count: want 1 after upsert, got %d", got)
	}
	var name string
	f.store.Read(func(c *config.MoomboxConfig) { name = c.Channels[0].Name })
	if name != "NewName" {
		t.Errorf("channel name: want NewName, got %q", name)
	}
}

func TestChannelAddRejectsEmptyID(t *testing.T) {
	f := newChannelRoutesFixture(t)

	body, _ := json.Marshal(config.ChannelConfig{Name: "no-id"})
	req := httptest.NewRequest("POST", "/api/config/channels", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty ID: want 400, got %d", rec.Code)
	}
	if f.channelsLen() != 0 {
		t.Error("config should not have been mutated for invalid input")
	}
}

func TestChannelAddRejectsInvalidJSON(t *testing.T) {
	f := newChannelRoutesFixture(t)

	req := httptest.NewRequest("POST", "/api/config/channels", bytes.NewReader([]byte("garbage")))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON: want 400, got %d", rec.Code)
	}
}

// --- DELETE /api/config/channels/{id} ---

func TestChannelDeleteRemovesByID(t *testing.T) {
	f := newChannelRoutesFixture(t)
	if err := f.store.Update(func(c *config.MoomboxConfig) {
		c.Channels = []config.ChannelConfig{
			{ID: "UCfoo", Platform: "youtube"},
			{ID: "UCbar", Platform: "youtube"},
		}
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	req := httptest.NewRequest("DELETE", "/api/config/channels/UCfoo", nil)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE channel: want 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if got := f.channelsLen(); got != 1 {
		t.Errorf("channel count after delete: want 1, got %d", got)
	}
	var remaining string
	f.store.Read(func(c *config.MoomboxConfig) { remaining = c.Channels[0].ID })
	if remaining != "UCbar" {
		t.Errorf("remaining channel: want UCbar, got %q", remaining)
	}
}

func TestChannelDeleteUnknownIDReturns404(t *testing.T) {
	f := newChannelRoutesFixture(t)
	if err := f.store.Update(func(c *config.MoomboxConfig) {
		c.Channels = []config.ChannelConfig{{ID: "UCfoo", Platform: "youtube"}}
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	req := httptest.NewRequest("DELETE", "/api/config/channels/UCghost", nil)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("delete unknown id: want 404, got %d", rec.Code)
	}
	if f.channelsLen() != 1 {
		t.Error("config should not have been mutated by failed delete")
	}
	if f.channelChange.Load() != 0 {
		t.Error("OnChannelChange should not fire on 404 delete")
	}
}

// --- PUT /api/config/channels/reorder ---

func TestChannelReorderSwapsOrder(t *testing.T) {
	f := newChannelRoutesFixture(t)
	if err := f.store.Update(func(c *config.MoomboxConfig) {
		c.Channels = []config.ChannelConfig{
			{ID: "UC1", Platform: "youtube"},
			{ID: "UC2", Platform: "youtube"},
			{ID: "UC3", Platform: "youtube"},
		}
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	body, _ := json.Marshal(map[string][]string{"ids": {"UC3", "UC1", "UC2"}})
	req := httptest.NewRequest("PUT", "/api/config/channels/reorder", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("PUT reorder: want 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	var order []string
	f.store.Read(func(c *config.MoomboxConfig) {
		for _, ch := range c.Channels {
			order = append(order, ch.ID)
		}
	})
	want := []string{"UC3", "UC1", "UC2"}
	if len(order) != len(want) {
		t.Fatalf("reordered length: want %d, got %d", len(want), len(order))
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("position %d: want %q, got %q", i, want[i], order[i])
		}
	}
}

func TestChannelReorderRejectsLengthMismatch(t *testing.T) {
	// Reorder takes a complete permutation of existing IDs — anything
	// shorter or longer is a frontend bug we surface immediately rather
	// than silently truncate the list.
	f := newChannelRoutesFixture(t)
	if err := f.store.Update(func(c *config.MoomboxConfig) {
		c.Channels = []config.ChannelConfig{
			{ID: "UC1", Platform: "youtube"},
			{ID: "UC2", Platform: "youtube"},
		}
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	body, _ := json.Marshal(map[string][]string{"ids": {"UC1"}}) // missing UC2
	req := httptest.NewRequest("PUT", "/api/config/channels/reorder", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("length mismatch: want 400, got %d", rec.Code)
	}
}

func TestChannelReorderRejectsDuplicates(t *testing.T) {
	f := newChannelRoutesFixture(t)
	if err := f.store.Update(func(c *config.MoomboxConfig) {
		c.Channels = []config.ChannelConfig{
			{ID: "UC1", Platform: "youtube"},
			{ID: "UC2", Platform: "youtube"},
		}
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	body, _ := json.Marshal(map[string][]string{"ids": {"UC1", "UC1"}})
	req := httptest.NewRequest("PUT", "/api/config/channels/reorder", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("duplicate id: want 400, got %d", rec.Code)
	}
}

func TestChannelReorderRejectsUnknownID(t *testing.T) {
	f := newChannelRoutesFixture(t)
	if err := f.store.Update(func(c *config.MoomboxConfig) {
		c.Channels = []config.ChannelConfig{
			{ID: "UC1", Platform: "youtube"},
		}
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	body, _ := json.Marshal(map[string][]string{"ids": {"UCghost"}})
	req := httptest.NewRequest("PUT", "/api/config/channels/reorder", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown id: want 400, got %d", rec.Code)
	}
}

// --- POST /api/resolve-channel ---

func TestResolveChannelRequiresInput(t *testing.T) {
	// Empty input should fail fast — utils.ResolveChannelInput would
	// otherwise fire a network request just to discover the empty
	// string isn't a URL.
	f := newChannelRoutesFixture(t)

	body, _ := json.Marshal(map[string]string{"input": ""})
	req := httptest.NewRequest("POST", "/api/resolve-channel", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty input: want 400, got %d", rec.Code)
	}
}

func TestResolveChannelEchoesUnrecognizedInput(t *testing.T) {
	// Non-URL input that ResolveChannelInput can't parse: return the
	// input as-is with resolved=false so the caller can disambiguate
	// "not a URL" from "URL but lookup failed".
	f := newChannelRoutesFixture(t)

	body, _ := json.Marshal(map[string]string{"input": "just-a-string"})
	req := httptest.NewRequest("POST", "/api/resolve-channel", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("non-URL input: want 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["id"] != "just-a-string" {
		t.Errorf("id: want echoed input, got %v", resp["id"])
	}
	if resp["resolved"] != false {
		t.Errorf("resolved: want false for non-URL, got %v", resp["resolved"])
	}
}
