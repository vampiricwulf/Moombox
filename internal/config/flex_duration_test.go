package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestDurationStringsUseTheFieldsUnit: UnmarshalTOML cannot see which field
// it decodes, so it guessed — days for a d/w suffix, minutes for the rest —
// and probe_cooldown = "2m" (a SECONDS field, and config.example.toml's own
// example) loaded as 2 seconds, refresh_interval = "1d" as one minute, and
// hide_finished_age_days = "12h" as 720 days (then reset to 30).
//
// Mutant: drop the resolveFlexDurationStrings call in loadFromFile — every
// row but the plain number fails.
func TestDurationStringsUseTheFieldsUnit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := `
[monitors]
feed_check_interval = "1d"
hide_finished_age_days = "12h"
probe_cooldown = "2m"

[downloader]
interruption_timeout = "1d"
incomplete_staging_expiry_days = "36h"

[cookies]
refresh_interval = 720
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		key  string
		got  float64
		want float64
	}{
		{"feed_check_interval (minutes)", cfg.Monitors.FeedCheckInterval.Value, 1440},
		{"hide_finished_age_days (days)", cfg.Monitors.HideFinishedAgeDays.Value, 0.5},
		{"probe_cooldown (seconds)", cfg.Monitors.ProbeCooldown.Value, 120},
		{"interruption_timeout (minutes)", cfg.Downloader.InterruptionTimeout.Value, 1440},
		{"incomplete_staging_expiry_days (days)", cfg.Downloader.IncompleteStagingExpiryDays.Value, 1.5},
		{"refresh_interval (a plain number stays as written)", cfg.Cookies.RefreshInterval.Value, 720},
	} {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.key, c.got, c.want)
		}
	}
}

// TestParseFlexDurationSeconds: the web PUT parses probe_cooldown with unit
// "seconds", which parseStringDuration had no arm for, so "30s" kept the old
// value while the save reported success.
//
// Mutant: delete the "seconds" case — the result is the default 7.
func TestParseFlexDurationSeconds(t *testing.T) {
	for in, want := range map[string]float64{"30s": 30, "2m": 120, "1500ms": 1.5, "45": 45} {
		if got := ParseFlexDuration(in, "seconds", 7).Value; got != want {
			t.Errorf("ParseFlexDuration(%q, seconds) = %v, want %v", in, got, want)
		}
	}
}

// TestFlexDurationFieldsListsEveryField: a FlexDuration added to the config
// without an entry in flexDurationFields would decode duration strings with
// UnmarshalTOML's guess again.
func TestFlexDurationFieldsListsEveryField(t *testing.T) {
	cfg := Defaults()
	listed := map[*FlexDuration]bool{}
	for _, f := range flexDurationFields {
		listed[f.field(cfg)] = true
	}
	flexType := reflect.TypeFor[FlexDuration]()
	var walk func(v reflect.Value, path string)
	walk = func(v reflect.Value, path string) {
		for i := range v.NumField() {
			sf, fv := v.Type().Field(i), v.Field(i)
			switch {
			case sf.Type == flexType:
				if !listed[fv.Addr().Interface().(*FlexDuration)] {
					t.Errorf("%s.%s is a FlexDuration with no flexDurationFields entry", path, sf.Name)
				}
			case sf.Type.Kind() == reflect.Struct && sf.IsExported():
				walk(fv, path+"."+sf.Name)
			}
		}
	}
	walk(reflect.ValueOf(cfg).Elem(), "MoomboxConfig")
	if len(listed) != len(flexDurationFields) {
		t.Errorf("flexDurationFields has %d entries for %d distinct fields", len(flexDurationFields), len(listed))
	}
}
