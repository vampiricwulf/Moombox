package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validateMessages(cfg *MoomboxConfig) string {
	var msgs []string
	for _, e := range Validate(cfg) {
		msgs = append(msgs, e.Error())
	}
	return strings.Join(msgs, "\n")
}

// A platform that is right once trimmed and lower-cased is rewritten, and the
// channel keeps its Twitch switches (owner ruling R2-M2). Before, the monitors
// read "Twitch" as YouTube and the Twitch-only rule cleared the switches.
//
// Mutants killed: gating the Twitch-only rule on the raw string (the switches
// are reported and cleared); not rewriting the platform.
func TestValidateFoldsTheChannelPlatform(t *testing.T) {
	cfg := Defaults()
	cfg.Channels = []ChannelConfig{
		{ID: "shroud", Name: "Shroud", Platform: " Twitch ", TwitchVodBacklog: true, TwitchArchiveClips: true, TwitchGapFillAdBreaks: true},
		{ID: "UCabcdefghijklmnopqrstuv", Platform: "YouTube"},
	}
	got := validateMessages(cfg)
	for _, want := range []string{
		`channels[0].platform " Twitch " must be written "twitch" (channel "Shroud")`,
		`channels[1].platform "YouTube" must be written "youtube"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Validate missed %q; got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "applies to Twitch channels only") {
		t.Errorf("a Twitch channel spelt with a capital had its Twitch switches refused:\n%s", got)
	}
	if cfg.Channels[0].Platform != " Twitch " {
		t.Error("Validate mutated the config")
	}

	Normalize(cfg)
	ch := cfg.Channels[0]
	if ch.Platform != "twitch" || cfg.Channels[1].Platform != "youtube" {
		t.Errorf("platforms after Normalize = %q, %q", ch.Platform, cfg.Channels[1].Platform)
	}
	if !ch.TwitchVodBacklog || !ch.TwitchArchiveClips || !ch.TwitchGapFillAdBreaks {
		t.Errorf("Normalize cleared a Twitch switch: %+v", ch)
	}
	if !ch.IsEnabled() {
		t.Error("a channel whose platform only needed folding was disabled")
	}
	if rest := validateMessages(cfg); rest != "" {
		t.Errorf("Validate after Normalize still reports:\n%s", rest)
	}
}

// A platform that is neither youtube nor twitch disables the channel and
// nothing else: the line, its string and its switches stay, and Validate is
// clean afterwards, or every later Store.Update would be refused.
//
// Mutants killed: reporting a disabled channel's unknown platform (Validate
// is not clean after Normalize); clearing the platform string; stripping the
// Twitch switches of a channel whose platform is unknown.
func TestNormalizeDisablesAChannelWithAnUnknownPlatform(t *testing.T) {
	cfg := Defaults()
	cfg.Channels = []ChannelConfig{
		{ID: "somebody", Name: "Somebody", Platform: "kick", TwitchVodBacklog: true},
		{ID: "UCabcdefghijklmnopqrstuv"},
	}
	got := validateMessages(cfg)
	if want := `channels[0].platform "kick" must be youtube or twitch: channel "Somebody" is loaded disabled; correct the platform and set enabled = true`; !strings.Contains(got, want) {
		t.Fatalf("Validate missed %q; got:\n%s", want, got)
	}
	if strings.Contains(got, "applies to Twitch channels only") {
		t.Errorf("the switches of a channel with an unknown platform were refused:\n%s", got)
	}

	Normalize(cfg)
	ch := cfg.Channels[0]
	if ch.IsEnabled() {
		t.Error("the channel is still enabled")
	}
	if ch.Platform != "kick" || !ch.TwitchVodBacklog || ch.ID != "somebody" {
		t.Errorf("Normalize changed more than enabled: %+v", ch)
	}
	if !cfg.Channels[1].IsEnabled() {
		t.Error("the other channel was disabled")
	}
	if rest := validateMessages(cfg); rest != "" {
		t.Errorf("Validate after Normalize still reports:\n%s", rest)
	}

	// Enabling it again without fixing the spelling is refused.
	on := true
	cfg.Channels[0].Enabled = &on
	if !strings.Contains(validateMessages(cfg), "must be youtube or twitch") {
		t.Error("an enabled channel with an unknown platform passed Validate")
	}
}

// Load reports both cases for the boot Warn, naming the channel and the
// value, and a save keeps the unknown platform's line in the file.
func TestLoadReportsChannelPlatformIssues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "[[channels]]\nid = \"shroud\"\nname = \"Shroud\"\nplatform = \"Twitch\"\ntwitch_vod_backlog = true\n\n" +
		"[[channels]]\nid = \"somebody\"\nname = \"Somebody\"\nplatform = \"kick\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cfg.NormalizedOnLoad, "\n")
	for _, want := range []string{`"Twitch" must be written "twitch" (channel "Shroud")`, `platform "kick" must be youtube or twitch: channel "Somebody" is loaded disabled; correct the platform and set enabled = true`} {
		if !strings.Contains(joined, want) {
			t.Errorf("NormalizedOnLoad missed %q; got:\n%s", want, joined)
		}
	}
	if cfg.Channels[0].Platform != "twitch" || !cfg.Channels[0].TwitchVodBacklog {
		t.Errorf("first channel = %+v", cfg.Channels[0])
	}
	if err := Save(cfg, path); err != nil {
		t.Fatalf("Save after Load: %v", err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), `platform = "kick"`) || !strings.Contains(string(saved), "enabled = false") {
		t.Errorf("the saved file lost the unknown platform's line or its disable:\n%s", saved)
	}
}
