package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func loadBody(t *testing.T, body string) *MoomboxConfig {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

// A key nothing reads — misspelt, in a misspelt section, or under the wrong
// section — loads as the default and is erased by the next save. Load records
// each one so boot can say so. An unknown section is named once, without the
// keys under it, and a key repeated across [[channels]] entries once.
//
// Mutants: loadFromFile discarding the MetaData again (nothing recorded);
// unknownKeysIn without the prefix check (moniters.feed_check_interval too);
// without the seen check (channels.nmae twice).
func TestUnknownKeysAreRecordedOnLoad(t *testing.T) {
	cfg := loadBody(t, `
[moniters]
feed_check_interval = 5

[monitors]
twitch_check_intervall = 5
port = 8080

[downloader]
segment_workers = 4

[[channels]]
id = "UC1234567890"
nmae = "one"

[[channels]]
id = "UC1234567891"
nmae = "two"
`)
	want := []string{"moniters", "monitors.twitch_check_intervall", "monitors.port", "channels.nmae"}
	if !slices.Equal(cfg.UnknownOnLoad, want) {
		t.Errorf("UnknownOnLoad = %q, want %q", cfg.UnknownOnLoad, want)
	}
	if cfg.Downloader.SegmentWorkers != 4 {
		t.Errorf("SegmentWorkers = %d, want 4: the known keys must still load", cfg.Downloader.SegmentWorkers)
	}
}

// Every key an older build wrote and this one no longer decodes into the
// struct — read by migrateOldFormat, dropped without a migration, or retired —
// is not a mistake and is not reported as one. The file below holds every
// entry of legacyKeys; the loop fails for an entry it misses.
//
// Mutant: any entry removed from legacyKeys — that key is reported.
func TestLegacyKeysAreNotReportedUnknown(t *testing.T) {
	body := `
allow_lan = true
allow_external = false
port = 8080
network_access = "lan"
https_enabled = false
tls_cert_path = "./cert.pem"
tls_key_path = "./key.pem"
password_hash = ""
log_level = "DEBUG"
log_file_path = "./moombox.log"
log_max_file_size = 10
log_max_files = 3
database_path = "./test.db"
feed_check_interval = 10
decapi_check_interval = 60
twitch_check_interval = 30
hide_finished_age_days = 14

[tasklist]
hide_finished_age_days = 7

[monitors]
max_feed_items = 500

[downloader]
output_directory = "./out"
staging_directory = "./staging"
ffmpeg_path = "ffmpeg"
cookie_file = "./cookies.txt"
po_token = "abc"
visitor_data = "xyz"
pot_provider_url = "http://127.0.0.1:4416"
segment_retry_delay_cap = 30
segment_live_check_retries = 3

[auto_cookies]
enabled = true
browser_profile_dir = "./profile"
refresh_interval = 60
platforms = ["youtube"]

[[channels]]
id = "UC1234567890"
max_feed_items = 20

[[notifications]]
url = "https://discord.com/api/webhooks/1/abc"
tags = ["a"]
`
	for key := range legacyKeys {
		section, name := "", key
		if i := strings.LastIndex(key, "."); i >= 0 {
			section, name = key[:i], key[i+1:]
		}
		if section == "" && !strings.Contains(body, "\n"+name+" = ") && !strings.Contains(body, "["+name+"]") {
			t.Errorf("legacy key %s is not in this test's file", key)
		}
		if section != "" && !strings.Contains(body, "\n"+name+" = ") {
			t.Errorf("legacy key %s is not in this test's file", key)
		}
	}
	cfg := loadBody(t, body)
	if len(cfg.UnknownOnLoad) != 0 {
		t.Errorf("UnknownOnLoad = %q, want none: these are legacy keys", cfg.UnknownOnLoad)
	}
	if cfg.Network.Port != 8080 || cfg.Paths.OutputDirectory != "./out" || !cfg.Cookies.AutoEnabled {
		t.Errorf("the legacy file did not migrate: port %d, output %q, auto cookies %v",
			cfg.Network.Port, cfg.Paths.OutputDirectory, cfg.Cookies.AutoEnabled)
	}
}

// A value that decodes itself takes its whole table: the keys of a named
// terms table and of the table form of a duration ({ Value = 5.0 }, written
// by an old encoder) are operator data, not unknown keys.
//
// Mutant: none in this repo — it pins the TOML library marking everything
// under an Unmarshaler as decoded, which unknownKeysIn relies on.
func TestSelfDecodingTablesAreNotReportedUnknown(t *testing.T) {
	cfg := loadBody(t, `
[monitors]
feed_check_interval = { Value = 5.0 }

[[channels]]
id = "UC1234567890"
name = "one"

[channels.terms]
karaoke = "歌枠|karaoke"
"with space" = "x"
`)
	if len(cfg.UnknownOnLoad) != 0 {
		t.Errorf("UnknownOnLoad = %q, want none", cfg.UnknownOnLoad)
	}
	if cfg.Monitors.FeedCheckInterval.Value != 5 {
		t.Errorf("FeedCheckInterval = %v, want 5", cfg.Monitors.FeedCheckInterval.Value)
	}
	if len(cfg.Channels) != 1 || cfg.Channels[0].Terms.Named["karaoke"] == "" {
		t.Errorf("the named terms did not load: %+v", cfg.Channels)
	}
}

// config.example.toml documents only keys something reads, so a key that is
// renamed or removed in the struct cannot stay in the example unnoticed.
func TestExampleConfigHasNoUnknownKeys(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "config.example.toml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := loadBody(t, string(body))
	if len(cfg.UnknownOnLoad) != 0 || len(cfg.IgnoredOnLoad) != 0 {
		t.Errorf("config.example.toml holds keys nothing reads: unknown %q, retired %q", cfg.UnknownOnLoad, cfg.IgnoredOnLoad)
	}
}
