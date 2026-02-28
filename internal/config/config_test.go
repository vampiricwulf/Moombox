package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaults(t *testing.T) {
	cfg := Defaults()
	if cfg.Network.Port != 774 {
		t.Errorf("expected port 774, got %d", cfg.Network.Port)
	}
	if cfg.Network.NetworkAccess != "localhost" {
		t.Errorf("expected localhost, got %s", cfg.Network.NetworkAccess)
	}
	if cfg.Downloader.MaxVideoResolution != 1080 {
		t.Errorf("expected 1080, got %d", cfg.Downloader.MaxVideoResolution)
	}
	if cfg.Downloader.NumParallelDownloads != 2 {
		t.Errorf("expected 2, got %d", cfg.Downloader.NumParallelDownloads)
	}
	if !cfg.Downloader.DownloadChat {
		t.Error("expected download_chat to be true")
	}
	if !cfg.Downloader.Prefer60fps {
		t.Error("expected prefer_60fps to be true")
	}
	if cfg.Paths.OutputDirectory != "./output" {
		t.Errorf("expected ./output, got %s", cfg.Paths.OutputDirectory)
	}
	if cfg.Cookies.CookieFile != "./cookies.txt" {
		t.Errorf("expected ./cookies.txt, got %s", cfg.Cookies.CookieFile)
	}
}

func TestLoadFromFile(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	content := `
[network]
port = 8080
network_access = "lan"

[logs]
log_level = "DEBUG"

[downloader]
max_video_resolution = 720
num_parallel_downloads = 4

[paths]
output_directory = "/tmp/output"
staging_directory = "/tmp/staging"

[[channels]]
id = "UC1234567890"
name = "Test Channel"
terms = "live|stream"
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Network.Port != 8080 {
		t.Errorf("expected port 8080, got %d", cfg.Network.Port)
	}
	if cfg.Network.NetworkAccess != "lan" {
		t.Errorf("expected lan, got %s", cfg.Network.NetworkAccess)
	}
	if cfg.Logs.LogLevel != "DEBUG" {
		t.Errorf("expected DEBUG, got %s", cfg.Logs.LogLevel)
	}
	if cfg.Downloader.MaxVideoResolution != 720 {
		t.Errorf("expected 720, got %d", cfg.Downloader.MaxVideoResolution)
	}
	if cfg.Downloader.NumParallelDownloads != 4 {
		t.Errorf("expected 4, got %d", cfg.Downloader.NumParallelDownloads)
	}
	if cfg.Paths.OutputDirectory != "/tmp/output" {
		t.Errorf("expected /tmp/output, got %s", cfg.Paths.OutputDirectory)
	}
	if len(cfg.Channels) != 1 {
		t.Fatalf("expected 1 channel, got %d", len(cfg.Channels))
	}
	if cfg.Channels[0].ID != "UC1234567890" {
		t.Errorf("expected UC1234567890, got %s", cfg.Channels[0].ID)
	}
}

func TestLoadOldFormatMigration(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	content := `
port = 8080
network_access = "lan"
log_level = "DEBUG"
database_path = "./test.db"
max_feed_items = 20
hide_finished_age_days = 14

[downloader]
max_video_resolution = 720
num_parallel_downloads = 4
output_directory = "/tmp/output"
staging_directory = "/tmp/staging"
cookie_file = "./test-cookies.txt"
ffmpeg_path = "/usr/bin/ffmpeg"

[auto_cookies]
enabled = true
browser_profile_dir = "./test-profile"

[[channels]]
id = "UC1234567890"
name = "Test Channel"
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}

	// Verify top-level fields migrated to Network
	if cfg.Network.Port != 8080 {
		t.Errorf("expected port 8080, got %d", cfg.Network.Port)
	}
	if cfg.Network.NetworkAccess != "lan" {
		t.Errorf("expected lan, got %s", cfg.Network.NetworkAccess)
	}
	// Verify top-level fields migrated to Logs
	if cfg.Logs.LogLevel != "DEBUG" {
		t.Errorf("expected DEBUG, got %s", cfg.Logs.LogLevel)
	}
	// Verify top-level fields migrated to Paths
	if cfg.Paths.DatabasePath != "./test.db" {
		t.Errorf("expected ./test.db, got %s", cfg.Paths.DatabasePath)
	}
	// Verify downloader fields migrated to Paths
	if cfg.Paths.OutputDirectory != "/tmp/output" {
		t.Errorf("expected /tmp/output, got %s", cfg.Paths.OutputDirectory)
	}
	if cfg.Paths.StagingDirectory != "/tmp/staging" {
		t.Errorf("expected /tmp/staging, got %s", cfg.Paths.StagingDirectory)
	}
	if cfg.Paths.FfmpegPath != "/usr/bin/ffmpeg" {
		t.Errorf("expected /usr/bin/ffmpeg, got %s", cfg.Paths.FfmpegPath)
	}
	// Verify downloader.cookie_file migrated to Cookies
	if cfg.Cookies.CookieFile != "./test-cookies.txt" {
		t.Errorf("expected ./test-cookies.txt, got %s", cfg.Cookies.CookieFile)
	}
	// Verify auto_cookies migrated to Cookies
	if !cfg.Cookies.AutoEnabled {
		t.Error("expected auto_enabled to be true")
	}
	if cfg.Cookies.BrowserProfileDir != "./test-profile" {
		t.Errorf("expected ./test-profile, got %s", cfg.Cookies.BrowserProfileDir)
	}
	// Verify Monitors
	if cfg.Monitors.MaxFeedItems != 20 {
		t.Errorf("expected 20, got %d", cfg.Monitors.MaxFeedItems)
	}
	if cfg.Monitors.HideFinishedAgeDays.Value != 14 {
		t.Errorf("expected 14, got %f", cfg.Monitors.HideFinishedAgeDays.Value)
	}
}

func TestValidation(t *testing.T) {
	cfg := Defaults()
	cfg.Network.Port = -1
	cfg.Network.NetworkAccess = "invalid"
	cfg.Downloader.NumParallelDownloads = 0
	cfg.Downloader.MaxVideoResolution = -100

	validate(cfg)

	if cfg.Network.Port != 774 {
		t.Errorf("expected port reset to 774, got %d", cfg.Network.Port)
	}
	if cfg.Network.NetworkAccess != "localhost" {
		t.Errorf("expected network_access reset to localhost, got %s", cfg.Network.NetworkAccess)
	}
	if cfg.Downloader.NumParallelDownloads != 2 {
		t.Errorf("expected parallel downloads reset to 2, got %d", cfg.Downloader.NumParallelDownloads)
	}
	if cfg.Downloader.MaxVideoResolution != 1080 {
		t.Errorf("expected resolution reset to 1080, got %d", cfg.Downloader.MaxVideoResolution)
	}
}

func TestResolveTemplate(t *testing.T) {
	date := "2024-03-15T10:30:00Z"
	vars := TemplateVariables{
		Title:   "Test Stream - Part 1!",
		ID:      "dQw4w9WgXcQ",
		Channel: "Test Channel @123",
		Date:    &date,
	}

	result := ResolveTemplate("${channel}/${start_date} ${title} [${id}]", vars)

	if result == "" {
		t.Error("expected non-empty result")
	}
	// Verify the ID and date are present
	if !contains(result, "dQw4w9WgXcQ") {
		t.Error("expected result to contain video ID")
	}
	if !contains(result, "20240315") {
		t.Error("expected result to contain date")
	}
}

func TestFlexDurationParse(t *testing.T) {
	tests := []struct {
		input    interface{}
		unit     string
		def      float64
		expected float64
	}{
		{10, "minutes", 0, 10},
		{int64(5), "minutes", 0, 5},
		{"10m", "minutes", 0, 10},
		{"1h", "minutes", 0, 60},
		{"7d", "days", 0, 7},
		{"invalid", "minutes", 15, 15},
	}

	for _, tt := range tests {
		result := ParseFlexDuration(tt.input, tt.unit, tt.def)
		if result.Value != tt.expected {
			t.Errorf("ParseFlexDuration(%v, %s) = %f, want %f", tt.input, tt.unit, result.Value, tt.expected)
		}
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
