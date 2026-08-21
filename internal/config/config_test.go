package config

import (
	"bytes"
	"encoding/json"
	"maps"
	"math"
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
	if cfg.Downloader.NumParallelDownloads != 10 {
		t.Errorf("expected 10, got %d", cfg.Downloader.NumParallelDownloads)
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
// ArchiveWindowDays / FeedCheckInterval / HideFinishedAgeDays.
// Pre-Finding-13 only the lower bound was enforced — setting
// max_feed_items = 1000000 (the field's predecessor) was silently accepted.
func TestValidateMonitorBounds(t *testing.T) {
	tests := []struct {
		name                  string
		mutate                func(*MoomboxConfig)
		wantArchiveWindowDays int
		wantFeedInterval      float64
		wantHideAge           float64
	}{
		{
			name: "defaults pass through",
			mutate: func(cfg *MoomboxConfig) {
				// no mutation
			},
			wantArchiveWindowDays: Defaults().Monitors.ArchiveWindowDays,
			wantFeedInterval:      Defaults().Monitors.FeedCheckInterval.Value,
			wantHideAge:           Defaults().Monitors.HideFinishedAgeDays.Value,
		},
		{
			name: "archive_window_days above 3650 resets",
			mutate: func(cfg *MoomboxConfig) {
				cfg.Monitors.ArchiveWindowDays = 5000
			},
			wantArchiveWindowDays: Defaults().Monitors.ArchiveWindowDays,
			wantFeedInterval:      Defaults().Monitors.FeedCheckInterval.Value,
			wantHideAge:           Defaults().Monitors.HideFinishedAgeDays.Value,
		},
		{
			name: "archive_window_days at upper bound passes",
			mutate: func(cfg *MoomboxConfig) {
				cfg.Monitors.ArchiveWindowDays = 3650
			},
			wantArchiveWindowDays: 3650,
			wantFeedInterval:      Defaults().Monitors.FeedCheckInterval.Value,
			wantHideAge:           Defaults().Monitors.HideFinishedAgeDays.Value,
		},
		{
			name: "feed_check_interval > 1440 resets",
			mutate: func(cfg *MoomboxConfig) {
				cfg.Monitors.FeedCheckInterval = FlexDuration{Value: 5000}
			},
			wantArchiveWindowDays: Defaults().Monitors.ArchiveWindowDays,
			wantFeedInterval:      Defaults().Monitors.FeedCheckInterval.Value,
			wantHideAge:           Defaults().Monitors.HideFinishedAgeDays.Value,
		},
		{
			name: "feed_check_interval at upper bound passes",
			mutate: func(cfg *MoomboxConfig) {
				cfg.Monitors.FeedCheckInterval = FlexDuration{Value: 1440}
			},
			wantArchiveWindowDays: Defaults().Monitors.ArchiveWindowDays,
			wantFeedInterval:      1440,
			wantHideAge:           Defaults().Monitors.HideFinishedAgeDays.Value,
		},
		{
			name: "hide_finished_age_days > 365 resets",
			mutate: func(cfg *MoomboxConfig) {
				cfg.Monitors.HideFinishedAgeDays = FlexDuration{Value: 9999}
			},
			wantArchiveWindowDays: Defaults().Monitors.ArchiveWindowDays,
			wantFeedInterval:      Defaults().Monitors.FeedCheckInterval.Value,
			wantHideAge:           Defaults().Monitors.HideFinishedAgeDays.Value,
		},
		{
			name: "hide_finished_age_days at upper bound passes",
			mutate: func(cfg *MoomboxConfig) {
				cfg.Monitors.HideFinishedAgeDays = FlexDuration{Value: 365}
			},
			wantArchiveWindowDays: Defaults().Monitors.ArchiveWindowDays,
			wantFeedInterval:      Defaults().Monitors.FeedCheckInterval.Value,
			wantHideAge:           365,
		},
		{
			name: "all three above bounds reset together",
			mutate: func(cfg *MoomboxConfig) {
				cfg.Monitors.ArchiveWindowDays = 1000000
				cfg.Monitors.FeedCheckInterval = FlexDuration{Value: 99999}
				cfg.Monitors.HideFinishedAgeDays = FlexDuration{Value: 99999}
			},
			wantArchiveWindowDays: Defaults().Monitors.ArchiveWindowDays,
			wantFeedInterval:      Defaults().Monitors.FeedCheckInterval.Value,
			wantHideAge:           Defaults().Monitors.HideFinishedAgeDays.Value,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Defaults()
			tt.mutate(cfg)
			Normalize(cfg)
			if cfg.Monitors.ArchiveWindowDays != tt.wantArchiveWindowDays {
				t.Errorf("ArchiveWindowDays = %d, want %d", cfg.Monitors.ArchiveWindowDays, tt.wantArchiveWindowDays)
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

func TestMembershipDiscoveryDefaultOnAndPersist(t *testing.T) {
	dir := t.TempDir()

	// 1. An existing config WITHOUT the key must load as enabled (default on),
	//    and normalization must make the pointer concrete (non-nil).
	absent := filepath.Join(dir, "absent.toml")
	if err := os.WriteFile(absent, []byte("[monitors]\nfeed_check_interval = 5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(absent)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Monitors.MembershipDiscoveryEnabled() {
		t.Error("membership discovery should default ON when the key is absent")
	}
	if cfg.Monitors.MembershipDiscovery == nil {
		t.Error("load should normalize the nil pointer to a concrete value")
	}

	// 2. An explicit false must survive the load (disable persists).
	off := filepath.Join(dir, "off.toml")
	if err := os.WriteFile(off, []byte("[monitors]\nmembership_discovery = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgOff, err := Load(off)
	if err != nil {
		t.Fatal(err)
	}
	if cfgOff.Monitors.MembershipDiscoveryEnabled() {
		t.Error("explicit membership_discovery=false must stay disabled after load")
	}

	// 3. Normalize directly: nil -> enabled; explicit false left untouched.
	nilCfg := &MoomboxConfig{}
	Normalize(nilCfg)
	if !nilCfg.Monitors.MembershipDiscoveryEnabled() {
		t.Error("Normalize should turn a nil membership_discovery ON")
	}
	f := false
	falseCfg := &MoomboxConfig{Monitors: MonitorsConfig{MembershipDiscovery: &f}}
	Normalize(falseCfg)
	if falseCfg.Monitors.MembershipDiscoveryEnabled() {
		t.Error("Normalize must not flip an explicit false to on")
	}

	// 4. Save→reload round-trip: an explicit false must survive the ENCODER —
	//    omitempty on a *bool keys off nil, not the pointed value, so a non-nil
	//    &false is still written. This is the "…AndPersist" half of the contract.
	saved := filepath.Join(dir, "saved.toml")
	if err := Save(cfgOff, saved); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reloaded, err := Load(saved)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Monitors.MembershipDiscoveryEnabled() {
		t.Error("membership_discovery=false must survive a Save→reload round-trip")
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
	if cfg.Downloader.NumParallelDownloads != 10 {
		t.Errorf("expected parallel downloads reset to 10, got %d", cfg.Downloader.NumParallelDownloads)
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
		// Non-finite values fall back to default: ParseFloat accepts
		// "nan"/"inf" and NaN passes every range check in
		// validateOrNormalize (all NaN comparisons are false)
		{"nan", "minutes", 10, 10},
		{"inf", "minutes", 10, 10},
		{"-inf", "minutes", 10, 10},
		{math.NaN(), "minutes", 10, 10},
		{math.Inf(1), "minutes", 10, 10},
	}

	for _, tt := range tests {
		result := ParseFlexDuration(tt.input, tt.unit, tt.def)
		if result.Value != tt.expected {
			t.Errorf("ParseFlexDuration(%v, %s) = %f, want %f", tt.input, tt.unit, result.Value, tt.expected)
		}
	}
}

func TestFlexDurationUnmarshalTOMLRejectsNonFinite(t *testing.T) {
	var d FlexDuration
	for _, in := range []any{math.NaN(), math.Inf(1), math.Inf(-1), "nan", "inf"} {
		if err := d.UnmarshalTOML(in); err == nil {
			t.Errorf("UnmarshalTOML(%v) = nil error, want rejection", in)
		}
	}
	// Sanity: normal values still work
	if err := d.UnmarshalTOML(int64(30)); err != nil || d.Value != 30 {
		t.Errorf("UnmarshalTOML(30) = %v (value %v), want nil error and 30", nil, d.Value)
	}
}

func TestValidationMemoryLimits(t *testing.T) {
	// Negatives clamp to 0 (disabled)
	cfg := Defaults()
	cfg.Memory.GoSoftLimitMB = -1
	cfg.Memory.SidecarSoftLimitMB = -5
	cfg.Memory.SidecarHardLimitMB = -10
	if errs := Validate(cfg); len(errs) != 3 {
		t.Errorf("Validate negative memory limits: got %d errors, want 3: %v", len(errs), errs)
	}
	Normalize(cfg)
	if cfg.Memory.GoSoftLimitMB != 0 || cfg.Memory.SidecarSoftLimitMB != 0 || cfg.Memory.SidecarHardLimitMB != 0 {
		t.Errorf("Normalize negative memory limits: got %+v, want all 0", cfg.Memory)
	}

	// Hard <= soft (both > 0) is unsatisfiable — reset pair to defaults
	cfg = Defaults()
	cfg.Memory.SidecarSoftLimitMB = 600
	cfg.Memory.SidecarHardLimitMB = 512
	if errs := Validate(cfg); len(errs) != 1 {
		t.Errorf("Validate hard<=soft: got %d errors, want 1: %v", len(errs), errs)
	}
	Normalize(cfg)
	def := Defaults()
	if cfg.Memory.SidecarSoftLimitMB != def.Memory.SidecarSoftLimitMB ||
		cfg.Memory.SidecarHardLimitMB != def.Memory.SidecarHardLimitMB {
		t.Errorf("Normalize hard<=soft: got soft=%d hard=%d, want defaults %d/%d",
			cfg.Memory.SidecarSoftLimitMB, cfg.Memory.SidecarHardLimitMB,
			def.Memory.SidecarSoftLimitMB, def.Memory.SidecarHardLimitMB)
	}

	// Hard with soft disabled (0) is fine
	cfg = Defaults()
	cfg.Memory.SidecarSoftLimitMB = 0
	cfg.Memory.SidecarHardLimitMB = 128
	if errs := Validate(cfg); len(errs) != 0 {
		t.Errorf("Validate hard-only: got errors %v, want none", errs)
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

func TestArchiveSettingsDefaults(t *testing.T) {
	cfg := Defaults()
	if cfg.Monitors.ArchiveWindowDays != 3 || cfg.Monitors.ArchiveSlots != 3 {
		t.Fatalf("defaults %d/%d, want 3/3", cfg.Monitors.ArchiveWindowDays, cfg.Monitors.ArchiveSlots)
	}
	if cfg.Downloader.NumParallelDownloads != 10 {
		t.Fatalf("num_parallel_downloads default %d, want 10", cfg.Downloader.NumParallelDownloads)
	}
}

func TestArchiveSettingsValidation(t *testing.T) {
	cfg := Defaults()
	cfg.Monitors.ArchiveWindowDays = 5000 // > 3650
	bad := 200                            // > 100
	cfg.Channels = []ChannelConfig{{Name: "c", ArchiveSlots: &bad}}
	Normalize(cfg)
	if cfg.Monitors.ArchiveWindowDays != 3 {
		t.Fatal("global out-of-range must warn-and-reset to default (config.go:490 contract)")
	}
	if cfg.Channels[0].ArchiveSlots != nil {
		t.Fatal("out-of-range per-channel override must clear to nil (fall back to global) — spec §5: ONE validator covers overrides too")
	}
}

func TestMaxFeedItemsIgnored(t *testing.T) {
	// An existing TOML carrying max_feed_items must load without error and
	// without effect (spec §5: dropped, not migrated). No `loadFromString`
	// test helper exists in this package (grep: `func load\|toml.Decode` only
	// matches the TOML round-trip test at TestChannelTermsTOMLRoundTrip) — use
	// the write-temp-file-then-Load pattern already established by
	// TestLoadFromFile/TestMembershipDiscoveryDefaultOnAndPersist above.
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.toml")
	content := "[monitors]\nmax_feed_items = 500\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("legacy key must not error: %v", err)
	}
	if cfg.Monitors.ArchiveWindowDays != 3 {
		t.Fatal("defaults must apply")
	}
}

// TestSegmentWorkersValidation pins the owner's explicit requirement: no
// upper clamp. A high value is the operator's call — YouTube may treat a
// large fan-out as bot-like, so it warns, but it must not be silently
// rewritten (a clamp would look like the setting did nothing, which is
// exactly the trap num_parallel_downloads already set).
func TestSegmentWorkersValidation(t *testing.T) {
	tests := []struct {
		name string
		in   int
		want int
	}{
		{"zero falls back to the default", 0, 12},
		{"negative falls back to the default", -4, 12},
		{"one is honoured", 1, 1},
		{"default", 12, 12},
		{"far above the warn threshold is NOT clamped", 256, 256},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Downloader.SegmentWorkers = tc.in
			// validateOrNormalize (config.go) is unexported; Normalize is the
			// public entry point that runs it with reportOnly=false, i.e. it
			// mutates cfg in place exactly like the brief's `cfg.validate()`
			// would — replacing invalid values with defaults, never clamping
			// valid-but-high ones.
			Normalize(cfg)
			if got := cfg.Downloader.SegmentWorkers; got != tc.want {
				t.Errorf("SegmentWorkers = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestSegmentWorkersDefault(t *testing.T) {
	if got := Defaults().Downloader.SegmentWorkers; got != 12 {
		t.Errorf("default SegmentWorkers = %d, want 12", got)
	}
}

// TestInterruptionTimeoutDefault pins the default of 120 minutes (2 hours).
func TestInterruptionTimeoutDefault(t *testing.T) {
	if got := Defaults().Downloader.InterruptionTimeout.Value; got != 120 {
		t.Errorf("default InterruptionTimeout = %v, want 120", got)
	}
}

// TestInterruptionTimeoutValidation mirrors ProbeCooldown's clamp style: 0 is
// legal (disables the resume stall — finalize never waits) and must survive
// Normalize unchanged; only a negative value is invalid and resets to the
// default. There is deliberately no upper bound.
func TestInterruptionTimeoutValidation(t *testing.T) {
	tests := []struct {
		name string
		in   float64
		want float64
	}{
		{"negative falls back to the default", -30, 120},
		{"zero survives round-trip (disabled)", 0, 0},
		{"default", 120, 120},
		{"far above the default is NOT clamped", 10080, 10080},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Downloader.InterruptionTimeout = FlexDuration{Value: tc.in}
			Normalize(cfg)
			if got := cfg.Downloader.InterruptionTimeout.Value; got != tc.want {
				t.Errorf("InterruptionTimeout = %v, want %v", got, tc.want)
			}
		})
	}
}
