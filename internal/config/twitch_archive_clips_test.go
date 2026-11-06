package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTwitchArchiveClipsRoundTripsAndStaysTwitchOnly pins the per-channel
// twitch_archive_clips setting (owner decision D26 follow-up 2, named so by
// FE-34): off unless set, written to config.toml as `twitch_archive_clips =
// true` and read back, and cleared by Normalize on a channel that is not
// Twitch (where nothing would read it).
//
// Mutants: the TOML tag misspelt, or back to the unreleased archive_clips
// (the file lacks the key); the Normalize arm dropped (the YouTube channel
// keeps it).
func TestTwitchArchiveClipsRoundTripsAndStaysTwitchOnly(t *testing.T) {
	if (ChannelConfig{}).TwitchArchiveClips {
		t.Fatal("twitch_archive_clips must default off")
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := Defaults()
	cfg.Channels = []ChannelConfig{{ID: "vanillatv", Name: "VanillaTV", Platform: "twitch", TwitchArchiveClips: true}}
	if err := Save(cfg, path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "twitch_archive_clips = true") {
		t.Errorf("config.toml lacks twitch_archive_clips = true:\n%s", raw)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Channels) != 1 || !loaded.Channels[0].TwitchArchiveClips {
		t.Errorf("reloaded channels = %+v, want twitch_archive_clips kept", loaded.Channels)
	}

	yt := Defaults()
	yt.Channels = []ChannelConfig{{ID: "UCx", Name: "x", TwitchArchiveClips: true}}
	Normalize(yt)
	if yt.Channels[0].TwitchArchiveClips {
		t.Error("Normalize left twitch_archive_clips on a YouTube channel")
	}
}

// TestTwitchOnlyChannelKeysClearedOffTwitch pins owner decision PR-I3: the
// VOD backlog and the ad-break gap-fill are Twitch's alone, as
// twitch_archive_clips is. A config file that sets them on a YouTube channel
// loads with them cleared (Normalize), Validate refuses them, and a Twitch
// channel keeps them.
//
// Mutants: either new arm in validateOrNormalize dropped (the YouTube channel
// keeps that key, and Validate passes it).
func TestTwitchOnlyChannelKeysClearedOffTwitch(t *testing.T) {
	yt := Defaults()
	yt.Channels = []ChannelConfig{{ID: "UCx", Name: "x", TwitchVodBacklog: true, TwitchGapFillAdBreaks: true}}
	if errs := Validate(yt); len(errs) != 2 {
		t.Errorf("Validate = %v, want the backlog and the ad-break fill refused", errs)
	}
	Normalize(yt)
	if yt.Channels[0].TwitchVodBacklog || yt.Channels[0].TwitchGapFillAdBreaks {
		t.Errorf("Normalize left a Twitch-only key on a YouTube channel: %+v", yt.Channels[0])
	}

	tw := Defaults()
	tw.Channels = []ChannelConfig{{ID: "streamer", Name: "s", Platform: "twitch", TwitchVodBacklog: true, TwitchGapFillAdBreaks: true}}
	if errs := Validate(tw); len(errs) != 0 {
		t.Errorf("Validate refused a Twitch channel's own keys: %v", errs)
	}
	Normalize(tw)
	if !tw.Channels[0].TwitchVodBacklog || !tw.Channels[0].TwitchGapFillAdBreaks {
		t.Errorf("Normalize cleared a Twitch channel's own keys: %+v", tw.Channels[0])
	}
}
