package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestArchiveClipsRoundTripsAndStaysTwitchOnly pins the per-channel
// archive_clips setting (owner decision D26 follow-up 2): off unless set,
// written to config.toml as `archive_clips = true` and read back, and cleared
// by Normalize on a channel that is not Twitch (where nothing would read it).
//
// Mutants: the TOML tag misspelt (it reloads false); the Normalize arm
// dropped (the YouTube channel keeps it).
func TestArchiveClipsRoundTripsAndStaysTwitchOnly(t *testing.T) {
	if (ChannelConfig{}).ArchiveClips {
		t.Fatal("archive_clips must default off")
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := Defaults()
	cfg.Channels = []ChannelConfig{{ID: "vanillatv", Name: "VanillaTV", Platform: "twitch", ArchiveClips: true}}
	if err := Save(cfg, path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "archive_clips = true") {
		t.Errorf("config.toml lacks archive_clips = true:\n%s", raw)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Channels) != 1 || !loaded.Channels[0].ArchiveClips {
		t.Errorf("reloaded channels = %+v, want archive_clips kept", loaded.Channels)
	}

	yt := Defaults()
	yt.Channels = []ChannelConfig{{ID: "UCx", Name: "x", ArchiveClips: true}}
	Normalize(yt)
	if yt.Channels[0].ArchiveClips {
		t.Error("Normalize left archive_clips on a YouTube channel")
	}
}
