package tui

import (
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// TestSettingsTwitchClientID: the overlay's Twitch Client-ID row loads the
// configured value, refuses what config.Validate would (so a save cannot
// fail on it while reporting "Saved"), writes a valid one trimmed, and is
// restart-required in step with the dashboard (G70).
//
// Mutants: the validateSettingsValues gate removed; writeSettingsField
// storing the untrimmed value; the row left out of restartRequiredKeys.
func TestSettingsTwitchClientID(t *testing.T) {
	cfg := config.Defaults()
	cfg.Twitch.ClientID = "abc123"
	v := map[string]string{}
	loadSettingsValues(v, cfg)
	if v["client_id"] != "abc123" {
		t.Fatalf("loaded client_id = %q", v["client_id"])
	}
	noBrowser := func(string, string) string { return "" }
	v["client_id"] = "not valid!"
	if _, msg := validateSettingsValues(v, "", noBrowser); !strings.Contains(msg, "Client-ID") {
		t.Errorf("a bad Client-ID validated: %q", msg)
	}
	v["client_id"] = "  ue6666qo983tsx6so1t0vnawi233wa "
	p, msg := validateSettingsValues(v, "", noBrowser)
	if msg != "" {
		t.Fatalf("a valid Client-ID was refused: %q", msg)
	}
	c := config.Defaults()
	if !writeSettingsField(c, "client_id", v, p) || c.Twitch.ClientID != "ue6666qo983tsx6so1t0vnawi233wa" {
		t.Errorf("written client_id = %q", c.Twitch.ClientID)
	}
	if !restartRequiredKeys["client_id"] {
		t.Error("client_id is not restart-required, but it is applied at boot")
	}
}
