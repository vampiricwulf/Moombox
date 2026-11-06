package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestTwitchClientIDRoundTrip is G70's settings half: `[twitch] client_id`
// defaults to empty (the built-in Client-ID), survives a save and a load,
// and a malformed one — anything a header could be split on — is reported by
// Validate and put back to the default by Normalize, as every other setting
// is.
//
// Mutants: ValidTwitchClientID accepting any string (the header-splitting
// value passes Validate); Normalize leaving the bad value in place; the TOML
// tag misspelt (the value is lost on the round trip).
func TestTwitchClientIDRoundTrip(t *testing.T) {
	if got := Defaults().Twitch.ClientID; got != "" {
		t.Fatalf("default client_id = %q, want empty", got)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := Defaults()
	cfg.Twitch.ClientID = "ue6666qo983tsx6so1t0vnawi233wa"
	if err := Save(cfg, path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Twitch.ClientID != "ue6666qo983tsx6so1t0vnawi233wa" {
		t.Errorf("client_id after a round trip = %q", loaded.Twitch.ClientID)
	}

	bad := Defaults()
	bad.Twitch.ClientID = "abc\r\nX-Evil: 1"
	if errs := Validate(bad); len(errs) == 0 {
		t.Error("Validate accepted a Client-ID carrying a header break")
	}
	Normalize(bad)
	if bad.Twitch.ClientID != "" {
		t.Errorf("Normalize left client_id = %q, want the default", bad.Twitch.ClientID)
	}

	if err := os.WriteFile(path, []byte("[twitch]\nclient_id = \"has space\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if loaded, err := Load(path); err != nil || loaded.Twitch.ClientID != "" || len(loaded.NormalizedOnLoad) == 0 {
		t.Errorf("a bad client_id on disk loaded as %q (normalized %v, err %v), want the default and a note", loaded.Twitch.ClientID, loaded.NormalizedOnLoad, err)
	}
}
