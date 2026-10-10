package routes

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
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
	ChannelRoutes(r, store, func() { f.channelChange.Add(1) }, nil)
	ChannelRemovalRoutes(r, &ChannelRemovalRoutesDeps{Store: store,
		OnChannelChange: func() { f.channelChange.Add(1) }, Logger: nopRouteLogger{}})
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

// TestChannelAddRejectsUnknownPlatform: POST accepted any platform string,
// while PUT /api/config rejected the same entry. The monitors poll anything
// that is not "twitch" as YouTube, and the next full-form save 400'd on it.
func TestChannelAddRejectsUnknownPlatform(t *testing.T) {
	f := newChannelRoutesFixture(t)

	body, _ := json.Marshal(config.ChannelConfig{ID: "someone", Platform: "kick"})
	req := httptest.NewRequest("POST", "/api/config/channels", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown platform: want 400, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if f.channelsLen() != 0 {
		t.Error("config should not have been mutated for invalid input")
	}
}

// TestChannelAddTrimsTheID: an ID padded with whitespace is the same channel
// as the unpadded one (PUT /api/config compares trimmed IDs), not a second
// entry that no monitor can resolve.
func TestChannelAddTrimsTheID(t *testing.T) {
	f := newChannelRoutesFixture(t)
	if err := f.store.Update(func(c *config.MoomboxConfig) {
		c.Channels = []config.ChannelConfig{{ID: "UCfoo", Name: "OldName", Platform: "youtube"}}
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	body, _ := json.Marshal(config.ChannelConfig{ID: "  UCfoo ", Name: "NewName", Platform: "youtube"})
	req := httptest.NewRequest("POST", "/api/config/channels", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("padded ID: want 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	var chans []config.ChannelConfig
	f.store.Read(func(c *config.MoomboxConfig) { chans = slices.Clone(c.Channels) })
	if len(chans) != 1 || chans[0].ID != "UCfoo" || chans[0].Name != "NewName" {
		t.Errorf("channels = %+v, want the one UCfoo entry renamed", chans)
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

// TestChannelDeleteDecodesEscapedID pins W25-13: the dashboard sends the ID
// through encodeURIComponent, and chi matches on the escaped RawPath Go keeps
// for '@' and ':' — so a channel stored as "@SomeHandle" or as a URL could
// not be removed from the dashboard at all (404, channel still configured).
// The literal-'%' rows pin the other half: an ID chi matched on the decoded
// Path must not be decoded a second time.
//
// Mutants killed: pathParam returning chi.URLParam unchanged (the '@', URL
// and "@50%" rows 404); pathParam decoding even without a RawPath (the
// "a%40b" row removes nothing, then 404s).
func TestChannelDeleteDecodesEscapedID(t *testing.T) {
	for _, tc := range []struct {
		id, target string
	}{
		{"@SomeHandle", "/api/config/channels/%40SomeHandle"},
		{"https://www.youtube.com/@foo", "/api/config/channels/https%3A%2F%2Fwww.youtube.com%2F%40foo"},
		{"@50%", "/api/config/channels/%4050%25"},
		{"a%40b", "/api/config/channels/a%2540b"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			f := newChannelRoutesFixture(t)
			if err := f.store.Update(func(c *config.MoomboxConfig) {
				c.Channels = []config.ChannelConfig{{ID: tc.id, Platform: "youtube"}, {ID: "a@b", Platform: "youtube"}}
			}); err != nil {
				t.Fatalf("seed: %v", err)
			}
			rec := httptest.NewRecorder()
			f.router.ServeHTTP(rec, httptest.NewRequest("DELETE", tc.target, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("DELETE %s: want 200, got %d (body: %s)", tc.target, rec.Code, rec.Body.String())
			}
			var left []string
			f.store.Read(func(c *config.MoomboxConfig) {
				for _, ch := range c.Channels {
					left = append(left, ch.ID)
				}
			})
			if !slices.Equal(left, []string{"a@b"}) {
				t.Errorf("channels left after deleting %q: %q, want only a@b", tc.id, left)
			}
		})
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
