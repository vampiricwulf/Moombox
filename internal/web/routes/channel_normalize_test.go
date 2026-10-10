package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/utils"
	"github.com/vampiricwulf/Moombox/internal/web"
)

const handleChannelID = "UChandlehandlehandlehand"

// stubHandleLookups answers the lookups of "@SomeHandle" (a channel) and
// "@Gone" (a failed fetch) without youtube.com, and leaves every other
// input to the real normaliser: trimming, the no-network /channel/ and
// twitch.tv URLs, and the refusal of URLs that name no channel.
func stubHandleLookups(t *testing.T) {
	t.Helper()
	prev := normalizeChannelID
	normalizeChannelID = func(ctx context.Context, input string) (*utils.ResolvedChannel, error) {
		switch strings.TrimSpace(input) {
		case "@SomeHandle":
			return &utils.ResolvedChannel{ID: handleChannelID, Name: "Some Handle", Platform: "youtube"}, nil
		case "@Gone":
			return nil, errors.New("failed to fetch channel page: HTTP 404")
		}
		return utils.NormalizeChannelID(ctx, input)
	}
	t.Cleanup(func() { normalizeChannelID = prev })
}

func storedChannels(store *config.Store) (out []config.ChannelConfig) {
	store.Read(func(c *config.MoomboxConfig) { out = slices.Clone(c.Channels) })
	return out
}

// TestChannelAddResolvesBareHandle pins W25-12 on POST /api/config/channels:
// a bare @handle — what the dialogs advertise — is resolved to the
// channel's UC ID before it is stored, taking the resolved name, where it
// used to be stored verbatim and polled as feeds/videos.xml?channel_id=@….
// The lookup is the one the URL branch rate limits, so a handle rides the
// same limiter.
//
// Mutants killed: saving without the normaliser (stored "@SomeHandle");
// a NeedsChannelResolve without its "@" arm (the second handle post is
// not limited).
func TestChannelAddResolvesBareHandle(t *testing.T) {
	stubHandleLookups(t)
	store := config.NewStore(config.Defaults(), filepath.Join(t.TempDir(), "config.toml"))
	r := chi.NewRouter()
	ChannelRoutes(r, store, func() {}, web.NewRateLimiterCtx(t.Context(), 1, time.Minute))

	if rec := postChannel(t, r, map[string]any{"id": " @SomeHandle ", "enabled": true}); rec.Code != http.StatusOK {
		t.Fatalf("POST @SomeHandle: %d (body %s)", rec.Code, rec.Body.String())
	}
	got := storedChannels(store)
	if len(got) != 1 || got[0].ID != handleChannelID || got[0].Name != "Some Handle" || got[0].Platform != "youtube" {
		t.Fatalf("stored %+v, want the handle's UC ID, named, on youtube", got)
	}
	if rec := postChannel(t, r, map[string]any{"id": "@SomeHandle"}); rec.Code != http.StatusTooManyRequests {
		t.Errorf("a second handle post inside the window: %d, want 429 (a handle is a lookup)", rec.Code)
	}
}

// putChannels PUTs a channels[] replace and returns the recorder.
func putChannels(t *testing.T, r http.Handler, path string, chs []map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"channels": chs})
	req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestConfigPutNormalizesChannelIDs pins W25-14 on PUT /api/config: each
// channels[] ID goes through the normaliser POST uses before it is stored.
// The route used to trim the ID for its empty and duplicate checks only and
// store it as sent — " UC… " and "https://www.youtube.com/@foo" were both
// saved, and the monitors polled them forever.
//
// Mutants killed: dropping the normalizeChannelUpdates call from the PUT;
// not writing the normalised ID back into updates (stored padded / as a
// URL); not filling the resolved name or platform.
func TestConfigPutNormalizesChannelIDs(t *testing.T) {
	stubHandleLookups(t)
	f := newConfigRoutesFixture(t)

	rec := putChannels(t, f.router, "/api/config", []map[string]any{
		{"id": " UCpaddedpaddedpaddedpadd "},
		{"id": "https://www.youtube.com/channel/UCabcdefghijklmnopqrstuv", "name": "Kept"},
		{"id": "@SomeHandle"},
		{"id": "twitch.tv/shroud"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: %d (body %s)", rec.Code, rec.Body.String())
	}
	got := storedChannels(f.store)
	want := []config.ChannelConfig{
		{ID: "UCpaddedpaddedpaddedpadd"},
		{ID: "UCabcdefghijklmnopqrstuv", Name: "Kept", Platform: "youtube"},
		{ID: handleChannelID, Name: "Some Handle", Platform: "youtube"},
		{ID: "shroud", Platform: "twitch"},
	}
	if len(got) != len(want) {
		t.Fatalf("stored %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].ID != want[i].ID || got[i].Name != want[i].Name || got[i].Platform != want[i].Platform {
			t.Errorf("channels[%d] = {%q %q %q}, want {%q %q %q}", i,
				got[i].ID, got[i].Name, got[i].Platform, want[i].ID, want[i].Name, want[i].Platform)
		}
	}
}

// TestConfigPutRefusesUnresolvableChannelIDs: an ID that names no channel,
// or whose lookup fails, is a 400 naming the entry — the message POST
// answers — and nothing is stored. A URL and its resolved ID are one
// channel, so the pair is a duplicate.
//
// Mutants killed: not copying the normaliser's errors into the 400's
// details (the watch URL is stored); keying them by anything but
// channels[i].id.
func TestConfigPutRefusesUnresolvableChannelIDs(t *testing.T) {
	stubHandleLookups(t)
	f := newConfigRoutesFixture(t)

	rec := putChannels(t, f.router, "/api/config", []map[string]any{
		{"id": "https://www.youtube.com/watch?v=dQw4w9WgXcQ"},
		{"id": "@Gone"},
		{"id": "UCabcdefghijklmnopqrstuv"},
		{"id": "youtube.com/channel/UCabcdefghijklmnopqrstuv"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT: %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Details map[string]string `json:"details"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	for key, want := range map[string]string{
		"channels[0].id": "not a YouTube or Twitch channel URL",
		"channels[1].id": "failed to resolve channel",
		"channels[3].id": "duplicate channel ID",
	} {
		if resp.Details[key] != want {
			t.Errorf("details[%s] = %q, want %q (body %s)", key, resp.Details[key], want, rec.Body.String())
		}
	}
	if got := storedChannels(f.store); len(got) != 0 {
		t.Errorf("a refused PUT stored channels: %+v", got)
	}
}

// TestConfigPutChannelResolveRidesTheLimiter: a PUT carrying an ID to
// resolve costs a lookup per ID, so it rides the limiter POST resolves
// under; a save with nothing to resolve never touches it.
//
// Mutant killed: channelUpdatesNeedResolve answering false (the second
// resolving PUT goes through).
func TestConfigPutChannelResolveRidesTheLimiter(t *testing.T) {
	stubHandleLookups(t)
	store := config.NewStore(config.Defaults(), filepath.Join(t.TempDir(), "config.toml"))
	r := chi.NewRouter()
	ConfigRoutes(r, store, &ConfigRoutesCallbacks{ResolveRateLimit: web.NewRateLimiterCtx(t.Context(), 1, time.Minute)})

	if rec := putChannels(t, r, "/api/config", []map[string]any{{"id": "@SomeHandle"}}); rec.Code != http.StatusOK {
		t.Fatalf("first resolving PUT: %d (body %s)", rec.Code, rec.Body.String())
	}
	if rec := putChannels(t, r, "/api/config", []map[string]any{{"id": "@SomeHandle"}}); rec.Code != http.StatusTooManyRequests {
		t.Errorf("second resolving PUT inside the window: %d, want 429", rec.Code)
	}
	for i := range 3 {
		if rec := putChannels(t, r, "/api/config", []map[string]any{{"id": handleChannelID}}); rec.Code != http.StatusOK {
			t.Fatalf("plain PUT %d was limited: %d (body %s)", i, rec.Code, rec.Body.String())
		}
	}
}

// TestSetupCompleteNormalizesChannelIDs: the web wizard's
// /api/setup/complete shares PUT's apply path, and its channels go through
// the same normaliser.
//
// Mutant killed: dropping the normalizeChannelUpdates call from the setup
// route (the padded ID is stored as sent, the watch URL accepted).
func TestSetupCompleteNormalizesChannelIDs(t *testing.T) {
	stubHandleLookups(t)
	post := func(f *setupFixture, chs []map[string]any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(map[string]any{"channels": chs})
		req := httptest.NewRequest(http.MethodPost, "/api/setup/complete", bytes.NewReader(raw))
		req.RemoteAddr = "127.0.0.1:0"
		rec := httptest.NewRecorder()
		f.router.ServeHTTP(rec, req)
		return rec
	}

	refused := newSetupFixture(t)
	if rec := post(refused, []map[string]any{{"id": "https://www.youtube.com/watch?v=dQw4w9WgXcQ"}}); rec.Code != http.StatusBadRequest {
		t.Errorf("a watch URL as a channel: %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}

	f := newSetupFixture(t)
	if rec := post(f, []map[string]any{{"id": " UCpaddedpaddedpaddedpadd "}, {"id": "@SomeHandle"}}); rec.Code != http.StatusOK {
		t.Fatalf("setup/complete: %d (body %s)", rec.Code, rec.Body.String())
	}
	got := storedChannels(f.store)
	if len(got) != 2 || got[0].ID != "UCpaddedpaddedpaddedpadd" || got[1].ID != handleChannelID {
		t.Errorf("stored %+v, want the trimmed ID and the handle's UC ID", got)
	}
}

// TestChannelIDsCompareCaseInsensitively: config.Validate refuses two IDs
// that differ only in case (Save's last line against W25-15's duplicate),
// so the writers compare the same way — PUT /api/config refuses the pair
// with a 400 naming the entry rather than a bare 500 from Save, and POST
// /api/config/channels matches "Shroud" to the stored "shroud" instead of
// appending a second entry Save would then refuse.
//
// Mutants killed: PUT's duplicate key not lowercased (500); POST's match
// back to == (500, nothing stored).
func TestChannelIDsCompareCaseInsensitively(t *testing.T) {
	f := newConfigRoutesFixture(t)
	rec := putChannels(t, f.router, "/api/config", []map[string]any{{"id": "shroud"}, {"id": "Shroud"}})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"channels[1].id":"duplicate channel ID"`) {
		t.Errorf("PUT of a case pair: %d %s, want 400 naming channels[1].id", rec.Code, rec.Body.String())
	}

	cf := newChannelRoutesFixture(t)
	if err := cf.store.Update(func(c *config.MoomboxConfig) {
		c.Channels = []config.ChannelConfig{{ID: "shroud", Platform: "twitch"}}
	}); err != nil {
		t.Fatal(err)
	}
	if rec := postChannel(t, cf.router, map[string]any{"id": "Shroud", "platform": "twitch", "name": "Renamed", "edit": true}); rec.Code != http.StatusOK {
		t.Fatalf("POST Shroud over shroud: %d (body %s)", rec.Code, rec.Body.String())
	}
	if got := storedChannels(cf.store); len(got) != 1 || got[0].Name != "Renamed" {
		t.Errorf("stored %+v, want the one entry updated", got)
	}
}

// TestChannelWritersResolveMixedCaseURLs: POST /api/config/channels and
// PUT /api/config resolve a channel link whose host is written in mixed
// case ("Twitch.tv/shroud", "https://www.YouTube.com/channel/UC…") and
// refuse a URL on any other host. The shared gate compared the host as
// typed, so each of these was stored as the channel ID with a 200.
//
// Mutant killed: utils.LooksLikeURL matching the input as typed.
func TestChannelWritersResolveMixedCaseURLs(t *testing.T) {
	for in, want := range map[string]string{
		"https://www.YouTube.com/channel/UCabcdefghijklmnopqrstuv": "UCabcdefghijklmnopqrstuv",
		"https://www.Twitch.tv/shroud":                             "shroud",
		"Twitch.tv/shroud":                                         "shroud",
	} {
		f := newChannelRoutesFixture(t)
		if rec := postChannel(t, f.router, map[string]any{"id": in, "enabled": true}); rec.Code != http.StatusOK {
			t.Errorf("POST %q: %d (body %s)", in, rec.Code, rec.Body.String())
		}
		if got := storedChannels(f.store); len(got) != 1 || got[0].ID != want {
			t.Errorf("POST %q stored %+v, want %s", in, got, want)
		}

		c := newConfigRoutesFixture(t)
		if rec := putChannels(t, c.router, "/api/config", []map[string]any{{"id": in}}); rec.Code != http.StatusOK {
			t.Errorf("PUT %q: %d (body %s)", in, rec.Code, rec.Body.String())
		}
		if got := storedChannels(c.store); len(got) != 1 || got[0].ID != want {
			t.Errorf("PUT %q stored %+v, want %s", in, got, want)
		}
	}

	f := newChannelRoutesFixture(t)
	if rec := postChannel(t, f.router, map[string]any{"id": "https://Example.com/shroud"}); rec.Code != http.StatusBadRequest {
		t.Errorf("POST of a URL on another host: %d (body %s), want 400", rec.Code, rec.Body.String())
	}
	if got := storedChannels(f.store); len(got) != 0 {
		t.Errorf("a URL on another host was stored: %+v", got)
	}
}
