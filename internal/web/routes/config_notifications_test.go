package routes

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/notifications"
)

// putConfigExpect PUTs a body and returns the recorder so a test can assert
// on a 400's details map as well as on a 200.
func putConfigExpect(t *testing.T, f *configRoutesFixture, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("PUT", "/api/config", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

// TestConfigPutPublicURLValidation mirrors config.ValidatePublicURL at the API
// edge so a value the route accepts is never one Normalize then rewrites
// behind the operator's back.
func TestConfigPutPublicURLValidation(t *testing.T) {
	f := newConfigRoutesFixture(t)
	rec := putConfigExpect(t, f, map[string]any{"network": map[string]any{"public_url": "moombox.example.com"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a scheme-less public_url returned %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Details map[string]string `json:"details"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if _, ok := resp.Details["network.public_url"]; !ok {
		t.Errorf("the 400 did not name network.public_url: %v", resp.Details)
	}
}

// TestConfigPutPublicURLStoredCanonical: the route stores the canonical form,
// so the value the manager pastes into embeds is the same whichever editor
// typed it.
func TestConfigPutPublicURLStoredCanonical(t *testing.T) {
	f := newConfigRoutesFixture(t)
	putConfig(t, f, map[string]any{"network": map[string]any{"public_url": "  https://x.example/  "}})
	var got string
	f.store.Read(func(c *config.MoomboxConfig) { got = c.Network.PublicURL })
	if got != "https://x.example" {
		t.Errorf("stored public_url = %q, want the canonical form", got)
	}
}

// TestConfigPutPublicURLFiresNotificationsCallback is the hot-reload gap.
// public_url lives in [network] and the web form's Save sends `network`
// without `notifications` when no webhook is configured, so without this arm
// a public_url change would sit in the file until the next restart.
func TestConfigPutPublicURLFiresNotificationsCallback(t *testing.T) {
	f := newConfigRoutesFixture(t)
	putConfig(t, f, map[string]any{"network": map[string]any{"public_url": "https://x.example"}})
	if !f.notifs.Load() {
		t.Error("OnNotificationsChange did not fire for a public_url change — the manager would keep " +
			"the old base URL until a restart")
	}
}

// TestConfigPutPublicURLUnchangedDoesNotFire keeps the callback honest:
// re-saving the same value must not rebuild the target list, matching every
// other change-gated callback on this route.
func TestConfigPutPublicURLUnchangedDoesNotFire(t *testing.T) {
	f := newConfigRoutesFixture(t)
	putConfig(t, f, map[string]any{"network": map[string]any{"public_url": "https://x.example"}})
	f.notifs.Store(false)
	putConfig(t, f, map[string]any{"network": map[string]any{"public_url": "https://x.example"}})
	if f.notifs.Load() {
		t.Error("OnNotificationsChange fired for an unchanged public_url")
	}
}

// TestConfigPutNotificationKeysPersist covers enabled, mention and the
// three-way mention_events through the real merge path.
func TestConfigPutNotificationKeysPersist(t *testing.T) {
	f := newConfigRoutesFixture(t)
	putConfig(t, f, map[string]any{"notifications": []any{
		map[string]any{"url": "discord://1/aaa", "enabled": false},
		map[string]any{"url": "discord://2/bbb", "mention": "<@&123456789012345678>"},
		map[string]any{"url": "discord://3/ccc", "mention": "@here", "mention_events": []any{}},
		map[string]any{"url": "discord://4/ddd", "mention": "@here", "mention_events": []any{"finished"}},
	}})
	var n []config.NotificationConfig
	f.store.Read(func(c *config.MoomboxConfig) { n = append(n, c.Notifications...) })
	if len(n) != 4 {
		t.Fatalf("stored %d targets, want 4", len(n))
	}
	if n[0].IsEnabled() {
		t.Error("target 0: enabled = false was not stored")
	}
	if n[1].Mention != "<@&123456789012345678>" {
		t.Errorf("target 1: mention = %q", n[1].Mention)
	}
	if n[1].MentionEvents != nil {
		t.Error("target 1: an absent mention_events must stay absent so the default list applies")
	}
	if e := n[2].MentionEvents; e == nil || len(*e) != 0 {
		t.Errorf("target 2: explicit empty mention_events came back as %v — \"never ping\" was lost", e)
	}
	if e := n[3].MentionEvents; e == nil || len(*e) != 1 || (*e)[0] != "finished" {
		t.Errorf("target 3: mention_events = %v", e)
	}
}

// TestConfigPutRejectsBadMention: a token Discord cannot resolve renders as
// literal text and pings nobody, which looks like a delivery failure.
func TestConfigPutRejectsBadMention(t *testing.T) {
	f := newConfigRoutesFixture(t)
	rec := putConfigExpect(t, f, map[string]any{"notifications": []any{
		map[string]any{"url": "discord://1/aaa", "mention": "@ops-team"},
	}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "notifications[0].mention") {
		t.Errorf("the 400 did not name the field: %s", rec.Body.String())
	}
}

// TestConfigPutStripsUnknownEvents is audit §4's web/TUI divergence:
// toggleNotificationEvent splices the stored array and applyConfigUpdates
// stored any string, so an unknown key survived a web save forever. The
// all-unknown case is the exception and it is load-bearing — stripping it to
// empty would turn "matches nothing" into "matches EVERYTHING".
func TestConfigPutStripsUnknownEvents(t *testing.T) {
	f := newConfigRoutesFixture(t)
	putConfig(t, f, map[string]any{"notifications": []any{
		map[string]any{"url": "discord://1/aaa", "events": []any{"finished", "bogus_event", "error"}},
		map[string]any{"url": "discord://2/bbb", "events": []any{"bogus_only"}},
		map[string]any{"url": "discord://3/ccc", "mention": "@here", "mention_events": []any{"error", "nope"}},
	}})
	var n []config.NotificationConfig
	f.store.Read(func(c *config.MoomboxConfig) { n = append(n, c.Notifications...) })

	if strings.Join(n[0].Events, ",") != "finished,error" {
		t.Errorf("target 0 events = %v, want the unknown entry stripped and the order kept", n[0].Events)
	}
	if strings.Join(n[1].Events, ",") != "bogus_only" {
		t.Errorf("target 1 events = %v — an ALL-unknown filter must survive as written; stripping it to "+
			"empty makes the manager treat the target as unfiltered and deliver every event", n[1].Events)
	}
	if e := n[2].MentionEvents; e == nil || strings.Join(*e, ",") != "error" {
		t.Errorf("target 2 mention_events = %v, want the unknown entry stripped", e)
	}
}

// TestNotificationsApplyUsesTheSharedDecode is the forward-compat pin. The
// notifications arm rebuilt each target from the keys it had an explicit arm
// for, so every field added later needed a route edit too — and the first one
// anybody forgets (N3's `mode`) is silently erased by an unrelated Settings
// save, because the SPA sends the whole stored object back and the route is
// what drops it. The `channels` arm has always decoded instead.
//
// This is a source-level assertion on purpose: no BEHAVIOURAL test can tell
// the two apart until a field exists that the route has no arm for, and the
// whole point of the change is that such a field must never need one. When N3
// adds `Mode`, replace this with a `mode` round trip through putConfig.
func TestNotificationsApplyUsesTheSharedDecode(t *testing.T) {
	src, err := os.ReadFile("config_routes.go")
	if err != nil {
		t.Fatal(err)
	}
	arm := regexp.MustCompile(`(?s)// Notifications\n.*?\n\t}\n`).Find(src)
	if arm == nil {
		t.Fatal("the // Notifications arm of applyConfigUpdates was not found")
	}
	if !strings.Contains(string(arm), "json.Unmarshal") {
		t.Error("the notifications arm still builds config.NotificationConfig field by field; " +
			"every field a later arc adds would then need a route edit, and N3's `mode` would be " +
			"erased by any unrelated Settings save. Decode the array the way the channels arm does.")
	}
}

// TestDefaultMentionEventsMirroredInSettingsJS is the cheap insurance the
// audit asked for, applied to the one list this arc adds on both sides. The
// web editor preselects these chips when a mention is first typed; a drift
// would show one set in the browser and ping on another.
func TestDefaultMentionEventsMirroredInSettingsJS(t *testing.T) {
	raw, err := os.ReadFile("../../../web/public/modules/settings.js")
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile(`(?s)const DEFAULT_MENTION_EVENTS = \[(.*?)\];`).FindSubmatch(raw)
	if block == nil {
		t.Fatal("DEFAULT_MENTION_EVENTS not found in settings.js")
	}
	var js []string
	for _, m := range regexp.MustCompile(`"([a-z_]+)"`).FindAllSubmatch(block[1], -1) {
		js = append(js, string(m[1]))
	}
	if strings.Join(js, ",") != strings.Join(config.DefaultMentionEvents(), ",") {
		t.Errorf("settings.js DEFAULT_MENTION_EVENTS = %v, Go = %v", js, config.DefaultMentionEvents())
	}
	// The vocabulary the chips are drawn from must know every default that is
	// already a real key. sidecar_down arrives with Arc N2a; until then it is
	// inert by design, so it is exempt rather than asserted.
	for _, e := range config.DefaultMentionEvents() {
		if e == "sidecar_down" {
			continue
		}
		if !notifications.KnownEvents[e] {
			t.Errorf("default mention event %q is not in the vocabulary", e)
		}
	}
}
