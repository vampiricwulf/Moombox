package config

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

func TestDefaults(t *testing.T) {
	cfg := Defaults()
	if cfg.Network.Port != 774 {
		t.Errorf("expected port 774, got %d", cfg.Network.Port)
	}
	if cfg.Network.NetworkAccess != "localhost" {
		t.Errorf("expected localhost, got %s", cfg.Network.NetworkAccess)
	}
	if cfg.Downloader.MaxVideoResolution != 2160 {
		t.Errorf("expected 2160, got %d", cfg.Downloader.MaxVideoResolution)
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
	if cfg.Network.ClientTokenTTLDays != 365 {
		t.Errorf("expected ClientTokenTTLDays default 365, got %d", cfg.Network.ClientTokenTTLDays)
	}
}

// TestValidateMonitorBounds verifies the new upper bounds on
// MaxFeedItems / FeedCheckInterval / HideFinishedAgeDays.
// Pre-Finding-13 only the lower bound was enforced — setting
// max_feed_items = 1000000 was silently accepted.
func TestValidateMonitorBounds(t *testing.T) {
	tests := []struct {
		name             string
		mutate           func(*MoomboxConfig)
		wantMaxFeed      int
		wantFeedInterval float64
		wantHideAge      float64
	}{
		{
			name: "defaults pass through",
			mutate: func(cfg *MoomboxConfig) {
				// no mutation
			},
			wantMaxFeed:      Defaults().Monitors.MaxFeedItems,
			wantFeedInterval: Defaults().Monitors.FeedCheckInterval.Value,
			wantHideAge:      Defaults().Monitors.HideFinishedAgeDays.Value,
		},
		{
			name: "max_feed_items above 1000 resets",
			mutate: func(cfg *MoomboxConfig) {
				cfg.Monitors.MaxFeedItems = 5000
			},
			wantMaxFeed:      Defaults().Monitors.MaxFeedItems,
			wantFeedInterval: Defaults().Monitors.FeedCheckInterval.Value,
			wantHideAge:      Defaults().Monitors.HideFinishedAgeDays.Value,
		},
		{
			name: "max_feed_items at upper bound passes",
			mutate: func(cfg *MoomboxConfig) {
				cfg.Monitors.MaxFeedItems = 1000
			},
			wantMaxFeed:      1000,
			wantFeedInterval: Defaults().Monitors.FeedCheckInterval.Value,
			wantHideAge:      Defaults().Monitors.HideFinishedAgeDays.Value,
		},
		{
			name: "feed_check_interval > 1440 resets",
			mutate: func(cfg *MoomboxConfig) {
				cfg.Monitors.FeedCheckInterval = FlexDuration{Value: 5000}
			},
			wantMaxFeed:      Defaults().Monitors.MaxFeedItems,
			wantFeedInterval: Defaults().Monitors.FeedCheckInterval.Value,
			wantHideAge:      Defaults().Monitors.HideFinishedAgeDays.Value,
		},
		{
			name: "feed_check_interval at upper bound passes",
			mutate: func(cfg *MoomboxConfig) {
				cfg.Monitors.FeedCheckInterval = FlexDuration{Value: 1440}
			},
			wantMaxFeed:      Defaults().Monitors.MaxFeedItems,
			wantFeedInterval: 1440,
			wantHideAge:      Defaults().Monitors.HideFinishedAgeDays.Value,
		},
		{
			name: "hide_finished_age_days > 365 resets",
			mutate: func(cfg *MoomboxConfig) {
				cfg.Monitors.HideFinishedAgeDays = FlexDuration{Value: 9999}
			},
			wantMaxFeed:      Defaults().Monitors.MaxFeedItems,
			wantFeedInterval: Defaults().Monitors.FeedCheckInterval.Value,
			wantHideAge:      Defaults().Monitors.HideFinishedAgeDays.Value,
		},
		{
			name: "hide_finished_age_days at upper bound passes",
			mutate: func(cfg *MoomboxConfig) {
				cfg.Monitors.HideFinishedAgeDays = FlexDuration{Value: 365}
			},
			wantMaxFeed:      Defaults().Monitors.MaxFeedItems,
			wantFeedInterval: Defaults().Monitors.FeedCheckInterval.Value,
			wantHideAge:      365,
		},
		{
			name: "all three above bounds reset together",
			mutate: func(cfg *MoomboxConfig) {
				cfg.Monitors.MaxFeedItems = 1000000
				cfg.Monitors.FeedCheckInterval = FlexDuration{Value: 99999}
				cfg.Monitors.HideFinishedAgeDays = FlexDuration{Value: 99999}
			},
			wantMaxFeed:      Defaults().Monitors.MaxFeedItems,
			wantFeedInterval: Defaults().Monitors.FeedCheckInterval.Value,
			wantHideAge:      Defaults().Monitors.HideFinishedAgeDays.Value,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Defaults()
			tt.mutate(cfg)
			Normalize(cfg)
			if cfg.Monitors.MaxFeedItems != tt.wantMaxFeed {
				t.Errorf("MaxFeedItems = %d, want %d", cfg.Monitors.MaxFeedItems, tt.wantMaxFeed)
			}
			if cfg.Monitors.FeedCheckInterval.Value != tt.wantFeedInterval {
				t.Errorf("FeedCheckInterval = %v, want %v", cfg.Monitors.FeedCheckInterval.Value, tt.wantFeedInterval)
			}
			if cfg.Monitors.HideFinishedAgeDays.Value != tt.wantHideAge {
				t.Errorf("HideFinishedAgeDays = %v, want %v", cfg.Monitors.HideFinishedAgeDays.Value, tt.wantHideAge)
			}
		})
	}
}

// TestValidateCookiesRefreshInterval covers the upper-bound check
// added for audit reports/config.md Finding 15.
func TestValidateCookiesRefreshInterval(t *testing.T) {
	tests := []struct {
		name    string
		value   float64
		wantSet bool // true => caller-supplied value preserved; false => reset to default
	}{
		{"below lower bound resets", 5, false},
		{"at lower bound passes", 10, true},
		{"default passes", Defaults().Cookies.RefreshInterval.Value, true},
		{"daily passes", 1440, true},
		{"at upper bound passes", 10080, true},
		{"above upper bound resets", 10081, false},
		{"absurd large resets", 1_000_000, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Cookies.RefreshInterval = FlexDuration{Value: tt.value}
			Normalize(cfg)
			if tt.wantSet {
				if cfg.Cookies.RefreshInterval.Value != tt.value {
					t.Errorf("RefreshInterval = %v, want kept %v", cfg.Cookies.RefreshInterval.Value, tt.value)
				}
			} else {
				if cfg.Cookies.RefreshInterval.Value != Defaults().Cookies.RefreshInterval.Value {
					t.Errorf("RefreshInterval = %v, want reset to default %v", cfg.Cookies.RefreshInterval.Value, Defaults().Cookies.RefreshInterval.Value)
				}
			}
		})
	}
}

// TestValidateClientTokenTTL verifies range enforcement on the configurable
// "remember me" cookie lifetime — reports/web.md Q3.
func TestValidateClientTokenTTL(t *testing.T) {
	tests := []struct {
		name     string
		input    int
		expected int
	}{
		{"zero resets to default", 0, 365},
		{"negative resets to default", -1, 365},
		{"one day allowed", 1, 1},
		{"365 allowed", 365, 365},
		{"3650 allowed (max)", 3650, 3650},
		{"over 3650 resets to default", 3651, 365},
		{"very large resets to default", 1000000, 365},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Network.ClientTokenTTLDays = tt.input
			Normalize(cfg)
			if cfg.Network.ClientTokenTTLDays != tt.expected {
				t.Errorf("validate(%d) → %d, want %d", tt.input, cfg.Network.ClientTokenTTLDays, tt.expected)
			}
		})
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

// TestNetworkAccessExplicitOverridesLegacy guards the audit Finding 8
// fix: when a user has BOTH a legacy `allow_lan = true` line AND an
// explicit `[network] network_access = "localhost"`, the explicit
// new-format value must win. Pre-fix, the legacy migration ran
// unconditionally when NetworkAccess was "" or "localhost" (the
// default), so the user's explicit "localhost" got silently
// overridden to "lan".
func TestNetworkAccessExplicitOverridesLegacy(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name: "explicit [network] network_access wins over legacy allow_lan",
			content: `
allow_lan = true
[network]
network_access = "localhost"
`,
			want: "localhost",
		},
		{
			name: "explicit top-level network_access wins over legacy allow_lan",
			content: `
allow_lan = true
network_access = "localhost"
`,
			want: "localhost",
		},
		{
			name: "no explicit network_access, legacy allow_lan migrates",
			content: `
allow_lan = true
`,
			want: "lan",
		},
		{
			name: "no explicit network_access, legacy allow_external migrates",
			content: `
allow_lan = true
allow_external = true
`,
			want: "external",
		},
		{
			name: "explicit [network] network_access alone (no legacy)",
			content: `
[network]
network_access = "external"
`,
			want: "external",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.toml")
			if err := os.WriteFile(configPath, []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Network.NetworkAccess != tt.want {
				t.Errorf("NetworkAccess = %q, want %q", cfg.Network.NetworkAccess, tt.want)
			}
		})
	}
}

func TestValidation(t *testing.T) {
	cfg := Defaults()
	cfg.Network.Port = -1
	cfg.Network.NetworkAccess = "invalid"
	cfg.Downloader.NumParallelDownloads = 0
	cfg.Downloader.MaxVideoResolution = -100

	Normalize(cfg)

	if cfg.Network.Port != 774 {
		t.Errorf("expected port reset to 774, got %d", cfg.Network.Port)
	}
	if cfg.Network.NetworkAccess != "localhost" {
		t.Errorf("expected network_access reset to localhost, got %s", cfg.Network.NetworkAccess)
	}
	if cfg.Downloader.NumParallelDownloads != 2 {
		t.Errorf("expected parallel downloads reset to 2, got %d", cfg.Downloader.NumParallelDownloads)
	}
	if cfg.Downloader.MaxVideoResolution != 2160 {
		t.Errorf("expected resolution reset to 2160, got %d", cfg.Downloader.MaxVideoResolution)
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
	if !strings.Contains(result, "dQw4w9WgXcQ") {
		t.Error("expected result to contain video ID")
	}
	if !strings.Contains(result, "20240315") {
		t.Error("expected result to contain date")
	}
}

// TestFlexDurationAsDuration verifies that AsDuration preserves sub-base-unit
// precision — the legacy `time.Duration(d.Minutes()) * time.Minute` pattern
// truncated 1.5 minutes to 1 minute (cookies.md #15).
func TestFlexDurationAsDuration(t *testing.T) {
	cases := []struct {
		name  string
		value float64
		base  time.Duration
		want  time.Duration
	}{
		{"whole minutes", 30, time.Minute, 30 * time.Minute},
		{"fractional minutes preserved", 1.5, time.Minute, 90 * time.Second},
		{"fractional days", 0.5, 24 * time.Hour, 12 * time.Hour},
		{"zero", 0, time.Minute, 0},
	}
	for _, tc := range cases {
		got := FlexDuration{Value: tc.value}.AsDuration(tc.base)
		if got != tc.want {
			t.Errorf("%s: AsDuration(%v) of value=%v: got %v, want %v",
				tc.name, tc.base, tc.value, got, tc.want)
		}
	}
}

func TestFlexDurationParse(t *testing.T) {
	tests := []struct {
		input    any
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
		// Negative values should fall back to default
		{-5, "minutes", 10, 10},
		{int64(-3), "days", 7, 7},
		{-1.5, "minutes", 30, 30},
		// Zero is allowed
		{0, "minutes", 10, 0},
	}

	for _, tt := range tests {
		result := ParseFlexDuration(tt.input, tt.unit, tt.def)
		if result.Value != tt.expected {
			t.Errorf("ParseFlexDuration(%v, %s) = %f, want %f", tt.input, tt.unit, result.Value, tt.expected)
		}
	}
}

func TestMigrationPartialSection(t *testing.T) {
	// Test that migration works when the new section exists but is partial.
	// E.g., [paths] has database_path but not output_directory, and [downloader]
	// has output_directory — the latter should be migrated.
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	content := `
[paths]
database_path = "./custom.db"

[downloader]
output_directory = "/from/downloader"
staging_directory = "/staging/downloader"
ffmpeg_path = "/usr/bin/ffmpeg"
cookie_file = "./dl-cookies.txt"

[cookies]
auto_enabled = true
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}

	// database_path from [paths] should be kept
	if cfg.Paths.DatabasePath != "./custom.db" {
		t.Errorf("expected ./custom.db, got %s", cfg.Paths.DatabasePath)
	}
	// output_directory should be migrated from [downloader] since [paths] doesn't define it
	if cfg.Paths.OutputDirectory != "/from/downloader" {
		t.Errorf("expected /from/downloader, got %s", cfg.Paths.OutputDirectory)
	}
	// staging_directory should also migrate
	if cfg.Paths.StagingDirectory != "/staging/downloader" {
		t.Errorf("expected /staging/downloader, got %s", cfg.Paths.StagingDirectory)
	}
	// ffmpeg_path should also migrate
	if cfg.Paths.FfmpegPath != "/usr/bin/ffmpeg" {
		t.Errorf("expected /usr/bin/ffmpeg, got %s", cfg.Paths.FfmpegPath)
	}
	// cookie_file should NOT migrate because [cookies] section exists with auto_enabled
	// but cookie_file is not defined there, so it should migrate
	if cfg.Cookies.CookieFile != "./dl-cookies.txt" {
		t.Errorf("expected ./dl-cookies.txt, got %s", cfg.Cookies.CookieFile)
	}
}

func TestValidationDiskThresholds(t *testing.T) {
	cfg := Defaults()
	// Critical <= Warn should auto-adjust
	cfg.Disk.WarnPercent = 95
	cfg.Disk.CriticalPercent = 90

	Normalize(cfg)

	if cfg.Disk.CriticalPercent <= cfg.Disk.WarnPercent {
		t.Errorf("expected critical > warn, got critical=%d, warn=%d",
			cfg.Disk.CriticalPercent, cfg.Disk.WarnPercent)
	}
}

func TestChannelTermsTOMLRoundTrip(t *testing.T) {
	// Named (map) terms must encode as an INLINE table: MarshalTOML's output
	// lands at the value position, so the multi-line document form would
	// produce `terms = key = "value"` — invalid TOML that corrupts the saved
	// config and prevents the next startup from parsing it.
	type doc struct {
		Terms ChannelTerms `toml:"terms"`
	}
	cases := []struct {
		name string
		in   ChannelTerms
	}{
		{"simple", ChannelTerms{Simple: `(?i)kara"o\ke`}},
		{"named", ChannelTerms{Named: map[string]string{"stream": "live", `we"ird key`: `va\l"ue`}, IsMap: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := toml.NewEncoder(&buf).Encode(doc{Terms: tc.in}); err != nil {
				t.Fatalf("encode: %v", err)
			}
			var out doc
			if _, err := toml.Decode(buf.String(), &out); err != nil {
				t.Fatalf("decode failed for %q: %v", buf.String(), err)
			}
			if out.Terms.IsMap != tc.in.IsMap {
				t.Fatalf("IsMap mismatch: got %v want %v (encoded: %q)", out.Terms.IsMap, tc.in.IsMap, buf.String())
			}
			if tc.in.IsMap {
				if !maps.Equal(out.Terms.Named, tc.in.Named) {
					t.Errorf("named terms mismatch: got %#v want %#v (encoded: %q)", out.Terms.Named, tc.in.Named, buf.String())
				}
			} else if out.Terms.Simple != tc.in.Simple {
				t.Errorf("simple terms mismatch: got %q want %q", out.Terms.Simple, tc.in.Simple)
			}
		})
	}
}

func TestChannelTermsJSON(t *testing.T) {
	// Simple terms round-trip: the Web UI sends terms as a plain string
	ch := ChannelConfig{
		ID:    "UC123",
		Name:  "Test",
		Terms: ChannelTerms{Simple: "(?i)karaoke"},
	}

	data, err := json.Marshal(ch)
	if err != nil {
		t.Fatal(err)
	}
	// Verify JSON contains terms as a string, not a struct
	if !strings.Contains(string(data), `"terms":"(?i)karaoke"`) {
		t.Errorf("expected terms as plain string in JSON, got: %s", data)
	}

	var decoded ChannelConfig
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Terms.Simple != "(?i)karaoke" {
		t.Errorf("expected simple terms '(?i)karaoke', got %q", decoded.Terms.Simple)
	}
	if decoded.Terms.IsMap {
		t.Error("expected IsMap=false for simple terms")
	}

	// Named terms round-trip
	namedCh := ChannelConfig{
		ID:    "UC456",
		Terms: ChannelTerms{Named: map[string]string{"stream": "live"}, IsMap: true},
	}
	data, err = json.Marshal(namedCh)
	if err != nil {
		t.Fatal(err)
	}
	var namedDecoded ChannelConfig
	if err := json.Unmarshal(data, &namedDecoded); err != nil {
		t.Fatal(err)
	}
	if !namedDecoded.Terms.IsMap {
		t.Error("expected IsMap=true for named terms")
	}
	if namedDecoded.Terms.Named["stream"] != "live" {
		t.Errorf("expected named term 'live', got %q", namedDecoded.Terms.Named["stream"])
	}

	// Empty terms: marshals as "" (Go's omitempty doesn't suppress struct types)
	emptyCh := ChannelConfig{ID: "UC789"}
	data, err = json.Marshal(emptyCh)
	if err != nil {
		t.Fatal(err)
	}
	var emptyDecoded ChannelConfig
	if err := json.Unmarshal(data, &emptyDecoded); err != nil {
		t.Fatalf("failed to unmarshal empty terms channel: %v", err)
	}
	if len(emptyDecoded.Terms.Patterns()) != 0 {
		t.Error("expected empty terms after round-trip")
	}

	// Web UI sends channels as array with string terms — the critical setup wizard path
	webPayload := `[{"id":"UC999","terms":"(?i)karaoke","platform":"youtube"},{"id":"UC000"}]`
	var channels []ChannelConfig
	if err := json.Unmarshal([]byte(webPayload), &channels); err != nil {
		t.Fatalf("failed to unmarshal web UI channel payload: %v", err)
	}
	if len(channels) != 2 {
		t.Fatalf("expected 2 channels, got %d", len(channels))
	}
	if channels[0].Terms.Simple != "(?i)karaoke" {
		t.Errorf("expected first channel terms '(?i)karaoke', got %q", channels[0].Terms.Simple)
	}
	if len(channels[1].Terms.Patterns()) != 0 {
		t.Error("expected second channel to have empty terms")
	}
}

func TestValidationQualityPreference(t *testing.T) {
	cfg := Defaults()
	enabled := true
	cfg.Channels = []ChannelConfig{
		{ID: "UC123", QualityPreference: "1080p60"},
		{ID: "UC456", QualityPreference: "invalid_quality"},
		{ID: "UC789", QualityPreference: "best", Enabled: &enabled},
	}

	Normalize(cfg)

	if cfg.Channels[0].QualityPreference != "1080p60" {
		t.Errorf("expected 1080p60, got %s", cfg.Channels[0].QualityPreference)
	}
	if cfg.Channels[1].QualityPreference != "" {
		t.Errorf("expected empty (invalid cleared), got %s", cfg.Channels[1].QualityPreference)
	}
	if cfg.Channels[2].QualityPreference != "best" {
		t.Errorf("expected best, got %s", cfg.Channels[2].QualityPreference)
	}
}

func TestValidate_ConnectivityProbeTargets(t *testing.T) {
	cfg := Defaults()
	if len(cfg.Connectivity.ProbeTargets) == 0 {
		t.Fatal("defaults must populate connectivity.probe_targets")
	}

	// Malformed entry is reported by Validate.
	bad := Defaults()
	bad.Connectivity.ProbeTargets = []string{"not-a-host-port"}
	errs := Validate(bad)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "connectivity.probe_targets") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a probe_targets validation error, got %v", errs)
	}

	// Normalize replaces a malformed list with defaults.
	norm := Defaults()
	norm.Connectivity.ProbeTargets = []string{"not-a-host-port"}
	Normalize(norm)
	if len(norm.Connectivity.ProbeTargets) == 0 || norm.Connectivity.ProbeTargets[0] == "not-a-host-port" {
		t.Fatalf("Normalize should restore defaults, got %v", norm.Connectivity.ProbeTargets)
	}
}
