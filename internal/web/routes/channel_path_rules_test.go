package routes

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// A channel's output_directory took neither path rule from any writer, so a
// pasted Explorer "Copy as path" value was stored and the channel's next
// recording failed at mux after the whole download. Every web writer refuses
// a NEW quoted or ".." value now, through config.OutputDirOverrideRule, and
// still accepts one stored before the rule when it is sent back unchanged.
//
// Mutants it kills: saveChannel, the PUT handler or setup/complete not
// calling the rule (the quoted value is stored, or a 200 comes back);
// newChannelPathErrors comparing against "" instead of the stored channel
// (the unchanged stored value is refused by the toggle or the full save).
func TestChannelWritersRefuseANewQuotedOrTraversingOutputDirectory(t *testing.T) {
	storedDir := func(f *channelRoutesFixture) (d string) {
		f.store.Read(func(c *config.MoomboxConfig) {
			if len(c.Channels) > 0 {
				d = c.Channels[0].OutputDirectory
			}
		})
		return d
	}

	// POST /api/config/channels: an add and an edit.
	for _, dir := range []string{`"D:\Archives\VTuber"`, `'/mnt/archive'`, `../elsewhere`} {
		f := newChannelRoutesFixture(t)
		if rec := postChannel(t, f.router, map[string]any{"id": "UCx", "output_directory": dir}); rec.Code != http.StatusBadRequest ||
			!strings.Contains(rec.Body.String(), "Output directory cannot") {
			t.Errorf("add with %s: %d %s, want a 400 naming the output directory", dir, rec.Code, rec.Body.String())
		}
		if n := f.channelsLen(); n != 0 {
			t.Errorf("add with %s: a refused channel was stored", dir)
		}
		f.store.Update(func(c *config.MoomboxConfig) {
			c.Channels = []config.ChannelConfig{{ID: "UCx", Platform: "youtube", OutputDirectory: "D:/ok"}}
		})
		if rec := postChannel(t, f.router, map[string]any{"id": "UCx", "output_directory": dir, "edit": true}); rec.Code != http.StatusBadRequest {
			t.Errorf("edit to %s: %d %s, want 400", dir, rec.Code, rec.Body.String())
		}
		if got := storedDir(f); got != "D:/ok" {
			t.Errorf("edit to %s: stored %q, want D:/ok kept", dir, got)
		}
	}

	// Stored quoted before the rule: the enable toggle posts it back as is.
	f := newChannelRoutesFixture(t)
	f.store.Update(func(c *config.MoomboxConfig) {
		c.Channels = []config.ChannelConfig{{ID: "UCx", Platform: "youtube", OutputDirectory: `"E:\Old"`}}
	})
	if rec := postChannel(t, f.router, map[string]any{"id": "UCx", "platform": "youtube", "enabled": false, "output_directory": `"E:\Old"`, "edit": true}); rec.Code != http.StatusOK {
		t.Errorf("toggle of a channel stored quoted: %d %s, want 200", rec.Code, rec.Body.String())
	}

	// PUT /api/config channels[]: keyed by entry, the stored value passes.
	c := newConfigRoutesFixture(t)
	c.store.Update(func(cfg *config.MoomboxConfig) {
		cfg.Channels = []config.ChannelConfig{{ID: "UCa", Platform: "youtube", OutputDirectory: `"E:\Old"`}}
	})
	rec := putConfigExpect(t, c, map[string]any{"channels": []any{
		map[string]any{"id": "UCa", "platform": "youtube", "output_directory": `"E:\Old"`},
		map[string]any{"id": "UCb", "platform": "youtube", "output_directory": `"E:\New"`},
	}})
	var resp struct {
		Details map[string]string `json:"details"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if rec.Code != http.StatusBadRequest || resp.Details["channels[1].output_directory"] != "Path "+config.PathRuleQuoted {
		t.Errorf("PUT with a new quoted channels[1].output_directory: %d %s, want 400 naming it", rec.Code, rec.Body.String())
	}
	if _, refused := resp.Details["channels[0].output_directory"]; refused {
		t.Errorf("PUT refused the unchanged stored value: %v", resp.Details)
	}
	putConfig(t, c, map[string]any{"channels": []any{
		map[string]any{"id": "UCa", "platform": "youtube", "output_directory": `"E:\Old"`},
	}})

	// /api/setup/complete takes channels[] through the same check.
	sf := newSetupFixture(t)
	rec = postSetupComplete(t, sf.router, map[string]any{"channels": []any{
		map[string]any{"id": "UCa", "platform": "youtube", "output_directory": `../out`},
	}})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "channels[0].output_directory") {
		t.Errorf("setup/complete with a traversing channel override: %d %s, want a 400 naming it", rec.Code, rec.Body.String())
	}
}
