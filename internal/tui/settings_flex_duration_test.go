package tui

import (
	"strconv"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// Five FlexDuration settings were loaded with %.0f/int() and written back as
// integers on ANY TUI save, so a "90s" feed interval (1.5 minutes) became 2
// and a 0.5 interruption timeout became 0 — which config.go documents as "0
// disables". The loss happened whether or not the operator touched the field
// (CORE-7).
//
// Mutant: restoring fmt.Sprintf("%.0f", …) on the load side, or
// strconv.Atoi on the apply side — the round-tripped value is rounded.
func TestFlexDurationSettingsRoundTripFractions(t *testing.T) {
	cfg := config.Defaults()
	cfg.Monitors.FeedCheckInterval = config.FlexDuration{Value: 1.5}
	cfg.Monitors.ProbeCooldown = config.FlexDuration{Value: 2.5}
	cfg.Monitors.HideFinishedAgeDays = config.FlexDuration{Value: 0.5}
	cfg.Downloader.InterruptionTimeout = config.FlexDuration{Value: 0.5}
	cfg.Downloader.IncompleteStagingExpiryDays = config.FlexDuration{Value: 0.5}
	cfg.Cookies.RefreshInterval = config.FlexDuration{Value: 90.5}

	store := config.NewStore(cfg, "")
	m := NewSettingsModel()
	m.configStore = store
	m.cfg = cfg
	m.Open(cfg)

	for _, c := range []struct{ key, want string }{
		{"feed_check_interval", "1.5"},
		{"probe_cooldown", "2.5"},
		{"hide_finished_age_days", "0.5"},
		{"interruption_timeout", "0.5"},
		{"incomplete_staging_expiry_days", "0.5"},
		{"refresh_interval", "90.5"},
	} {
		if got := m.values[c.key]; got != c.want {
			t.Errorf("loaded %s = %q, want %q", c.key, got, c.want)
		}
	}

	m.applyValues()
	if m.status == saveError {
		t.Fatalf("applyValues refused a valid config: %s", m.errorMsg)
	}

	for _, c := range []struct {
		name string
		got  float64
		want float64
	}{
		{"feed_check_interval", cfg.Monitors.FeedCheckInterval.Value, 1.5},
		{"probe_cooldown", cfg.Monitors.ProbeCooldown.Value, 2.5},
		{"hide_finished_age_days", cfg.Monitors.HideFinishedAgeDays.Value, 0.5},
		{"interruption_timeout", cfg.Downloader.InterruptionTimeout.Value, 0.5},
		{"incomplete_staging_expiry_days", cfg.Downloader.IncompleteStagingExpiryDays.Value, 0.5},
		{"refresh_interval", cfg.Cookies.RefreshInterval.Value, 90.5},
	} {
		if c.got != c.want {
			t.Errorf("after applyValues %s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// validateDigitsOnly made a fractional value untypeable in the first place.
// The six FlexDuration-backed number fields accept one decimal point.
//
// Mutant: wiring validateDigitsOnly to every fieldNumber again — "0.5" is
// rejected.
func TestDecimalFieldsAcceptAPoint(t *testing.T) {
	if err := validateDecimal("0.5"); err != nil {
		t.Errorf("validateDecimal(\"0.5\") = %v, want nil", err)
	}
	if err := validateDecimal("30"); err != nil {
		t.Errorf("validateDecimal(\"30\") = %v, want nil", err)
	}
	if err := validateDecimal("0.5.5"); err == nil {
		t.Error("validateDecimal must reject a second decimal point")
	}
	if err := validateDecimal("1e3"); err == nil {
		t.Error("validateDecimal must reject anything but digits and one point")
	}
	if !decimalValueFields["refresh_interval"] {
		t.Error("refresh_interval is FlexDuration-backed and must accept a decimal point")
	}
	if decimalValueFields["log_max_files"] {
		t.Error("log_max_files is an int field and must keep the digits-only validator")
	}
}

// The map and the validator are inert unless updateTextInputForField actually
// installs them — that wiring is what the operator's keystrokes go through.
//
// Mutant: dropping the decimalValueFields case from the normal-field switch
// (so every fieldNumber gets validateDigitsOnly again) — "." is refused on
// probe_cooldown and the field stays untypeable.
func TestTheDecimalValidatorIsWiredToTheFlexFields(t *testing.T) {
	cfg := config.Defaults()
	store := config.NewStore(cfg, "")
	m := NewSettingsModel()
	m.configStore = store
	m.cfg = cfg
	m.Open(cfg)

	for _, c := range []struct {
		key          string
		acceptsPoint bool
	}{
		{"feed_check_interval", true},
		{"probe_cooldown", true},
		{"hide_finished_age_days", true},
		{"interruption_timeout", true},
		{"incomplete_staging_expiry_days", true},
		{"refresh_interval", true},
		// Every other numeric field keeps the digits-only filter.
		{"log_max_files", false},
		{"archive_slots", false},
		{"segment_workers", false},
		{"disk_warn_percent", false},
	} {
		if !focusSettingsField(m, c.key) {
			t.Errorf("no settings field keyed %q", c.key)
			continue
		}
		if m.textInput.Validate == nil {
			t.Errorf("%s: no keystroke filter installed", c.key)
			continue
		}
		err := m.textInput.Validate("0.5")
		if c.acceptsPoint && err != nil {
			t.Errorf("%s: typing \"0.5\" is refused (%v) — the field is FlexDuration-backed", c.key, err)
		}
		if !c.acceptsPoint && err == nil {
			t.Errorf("%s: typing \"0.5\" is accepted — an int field must keep validateDigitsOnly", c.key)
		}
	}
}

// focusSettingsField moves the overlay's cursor onto the field keyed key and
// reconfigures the text input exactly as a cursor move does.
func focusSettingsField(m *SettingsModel, key string) bool {
	for si, sec := range sections {
		for fi, f := range sec.fields {
			if f.key == key {
				m.sectionIndex = si
				m.fieldIndex = fi
				m.updateTextInputForField()
				return true
			}
		}
	}
	return false
}

// An out-of-range Twitch/DECAPI interval must be REPORTED, not silently
// cleared to "dynamic", and the help text must name the range config.Validate
// actually enforces (floor 5, not 1) (CORE-20).
//
// Mutant: restoring the silent `else { … = nil }` fallback — status stays
// saveSaved-able and the value vanishes.
func TestOutOfRangeIntervalIsReported(t *testing.T) {
	cfg := config.Defaults()
	store := config.NewStore(cfg, "")
	m := NewSettingsModel()
	m.configStore = store
	m.cfg = cfg
	m.Open(cfg)

	m.values["twitch_check_interval"] = "3"
	m.applyValues()
	if m.status != saveError {
		t.Fatalf("a 3-second Twitch interval must be refused, status = %v", m.status)
	}

	for _, f := range sections {
		for _, fd := range f.fields {
			if fd.key == "twitch_check_interval" && strings.Contains(fd.help, "1-3600") {
				t.Errorf("help text still claims the 1-3600 range config.Validate refuses: %q", fd.help)
			}
		}
	}
}

// R1 — the form shows the CANONICAL FLOAT in the field's documented unit
// (minutes for feed_check_interval / interruption_timeout / refresh_interval,
// days for hide_finished_age_days / incomplete_staging_expiry_days, seconds
// for probe_cooldown), never the string form the config FILE may carry.
// FlexDuration keeps no string: ParseFlexDuration turns "90s" into 1.5
// minutes at load and the struct holds only the float, so the string is
// already gone before the overlay ever sees it. FormatFloat(-1) is what makes
// that float survive the display — and it is byte-for-byte what
// FlexDuration.MarshalTOML writes back.
//
// Mutant: %.0f or strconv.Itoa(int(…)) on the load side — "90s" shows as "2".
func TestFlexDurationFormShowsTheCanonicalFloat(t *testing.T) {
	cfg := config.Defaults()
	// Exactly what a config.toml carrying feed_check_interval = "90s" loads as.
	cfg.Monitors.FeedCheckInterval = config.ParseFlexDuration("90s", "minutes", 10)
	if cfg.Monitors.FeedCheckInterval.Value != 1.5 {
		t.Fatalf(`ParseFlexDuration("90s") = %v, want 1.5`, cfg.Monitors.FeedCheckInterval.Value)
	}

	store := config.NewStore(cfg, "")
	m := NewSettingsModel()
	m.configStore = store
	m.cfg = cfg
	m.Open(cfg)

	if got := m.values["feed_check_interval"]; got != "1.5" {
		t.Errorf(`feed_check_interval displays %q, want "1.5"`, got)
	}
	// No trailing zeros on a whole value: "2", not "2.0" or "2.000000".
	if got := m.values["archive_window_days"]; got != "3" {
		t.Errorf("an int field is untouched by this change: %q", got)
	}
	cfg.Monitors.ProbeCooldown = config.FlexDuration{Value: 2}
	m.Open(cfg)
	if got := m.values["probe_cooldown"]; got != "2" {
		t.Errorf(`probe_cooldown displays %q for 2, want "2" (no trailing zeros)`, got)
	}

	// The string form is a config-FILE spelling, not a form spelling: the
	// keystroke filter refuses it, which is why the canonical float is the
	// only thing the overlay can show.
	if err := validateDecimal("90s"); err == nil {
		t.Error(`validateDecimal("90s") must reject the string form — the form carries the float`)
	}
}

// R1 (round-trip contract) — a config loaded into the form and saved with NO
// edits must write back exactly what config.Save would have written from the
// original struct. FlexDuration.MarshalTOML is strconv.FormatFloat(v,'f',-1,
// 64), the same spelling loadValues uses, so comparing the marshalled bytes
// is the byte-identical comparison without the Save shell-out.
//
// Mutant: %.0f restored on the load side — 1.5 comes back as 2 and the
// marshalled bytes differ.
func TestUneditedSaveIsByteIdenticalForTheSixFields(t *testing.T) {
	cfg := config.Defaults()
	cfg.Monitors.FeedCheckInterval = config.FlexDuration{Value: 1.5}
	cfg.Monitors.ProbeCooldown = config.FlexDuration{Value: 0.25}
	cfg.Monitors.HideFinishedAgeDays = config.FlexDuration{Value: 0.5}
	cfg.Downloader.InterruptionTimeout = config.FlexDuration{Value: 0.5}
	cfg.Downloader.IncompleteStagingExpiryDays = config.FlexDuration{Value: 3.5}
	cfg.Cookies.RefreshInterval = config.FlexDuration{Value: 10.5}

	before := map[string]string{
		"feed_check_interval":            marshalFlex(t, cfg.Monitors.FeedCheckInterval),
		"probe_cooldown":                 marshalFlex(t, cfg.Monitors.ProbeCooldown),
		"hide_finished_age_days":         marshalFlex(t, cfg.Monitors.HideFinishedAgeDays),
		"interruption_timeout":           marshalFlex(t, cfg.Downloader.InterruptionTimeout),
		"incomplete_staging_expiry_days": marshalFlex(t, cfg.Downloader.IncompleteStagingExpiryDays),
		"refresh_interval":               marshalFlex(t, cfg.Cookies.RefreshInterval),
	}

	store := config.NewStore(cfg, "")
	m := NewSettingsModel()
	m.configStore = store
	m.cfg = cfg
	m.Open(cfg)
	// No edits at all — this is the "operator changed one unrelated toggle"
	// save that used to round every one of these away.
	m.applyValues()
	if m.status == saveError {
		t.Fatalf("applyValues refused an untouched config: %s", m.errorMsg)
	}

	after := map[string]string{
		"feed_check_interval":            marshalFlex(t, cfg.Monitors.FeedCheckInterval),
		"probe_cooldown":                 marshalFlex(t, cfg.Monitors.ProbeCooldown),
		"hide_finished_age_days":         marshalFlex(t, cfg.Monitors.HideFinishedAgeDays),
		"interruption_timeout":           marshalFlex(t, cfg.Downloader.InterruptionTimeout),
		"incomplete_staging_expiry_days": marshalFlex(t, cfg.Downloader.IncompleteStagingExpiryDays),
		"refresh_interval":               marshalFlex(t, cfg.Cookies.RefreshInterval),
	}

	for key, want := range before {
		if after[key] != want {
			t.Errorf("%s = %s in config.toml after an unedited save, want %s", key, after[key], want)
		}
	}
}

func marshalFlex(t *testing.T, d config.FlexDuration) string {
	t.Helper()
	b, err := d.MarshalTOML()
	if err != nil {
		t.Fatalf("MarshalTOML: %v", err)
	}
	return string(b)
}

// R3 — a 0.5-minute interruption timeout is a 30-second stall ceiling, not
// "disabled". config.go documents 0 as "disabled, finalize never stalls", so
// rounding 0.5 down changed the FEATURE, not just the number.
//
// Mutant: strconv.Atoi restored on the apply side — the value becomes 0 and
// the engine gets the InterruptionNoStall sentinel instead of a 30 s ceiling.
func TestAFractionalInterruptionTimeoutIsNotDisabled(t *testing.T) {
	cfg := config.Defaults()
	store := config.NewStore(cfg, "")
	m := NewSettingsModel()
	m.configStore = store
	m.cfg = cfg
	m.Open(cfg)

	m.values["interruption_timeout"] = "0.5"
	m.values["incomplete_staging_expiry_days"] = "0.5"
	m.values["probe_cooldown"] = "0.5"
	m.applyValues()
	if m.status == saveError {
		t.Fatalf("applyValues refused a fractional timeout: %s", m.errorMsg)
	}
	if got := cfg.Downloader.InterruptionTimeout.Value; got != 0.5 {
		t.Errorf("interruption_timeout = %v, want 0.5 (0 means DISABLED)", got)
	}
	if got := cfg.Downloader.IncompleteStagingExpiryDays.Value; got != 0.5 {
		t.Errorf("incomplete_staging_expiry_days = %v, want 0.5 (0 means preserve forever)", got)
	}
	// probe_cooldown's 0 = disabled/probe every cycle is a standing product
	// ruling; a half-second cooldown must stay a half-second cooldown.
	if got := cfg.Monitors.ProbeCooldown.Value; got != 0.5 {
		t.Errorf("probe_cooldown = %v, want 0.5 (0 means disabled)", got)
	}
}

// R2 — the TUI's pre-validation must accept EXACTLY what config.Validate
// accepts, at the boundaries. The TUI gate exists because config.Save refuses
// a config Validate rejects, so a value the TUI waves through and Validate
// refuses loses the whole save; a value the TUI refuses and Validate would
// have taken is a field the operator cannot set at all.
//
// Only strings validateDecimal can type are in the table: NaN/Inf and
// negatives are unreachable through the keystroke filter (they need letters
// or a sign), which is why applyValues' explicit NaN/Inf rejection — kept
// from hide_finished_age_days — is belt-and-braces over a hand-edited config
// that Open loaded, not a divergence from Validate.
//
// Mutant: any range bound in the float table changed by one — the boundary
// row on that side disagrees with config.Validate.
func TestFlexRangesMatchConfigValidate(t *testing.T) {
	cases := []struct {
		key    string
		apply  func(*config.MoomboxConfig, float64)
		values []string
	}{
		{"feed_check_interval",
			func(c *config.MoomboxConfig, v float64) {
				c.Monitors.FeedCheckInterval = config.FlexDuration{Value: v}
			},
			[]string{"1", "1440", "0.999999", "1440.000001", "1.5", "0"},
		},
		{"hide_finished_age_days",
			func(c *config.MoomboxConfig, v float64) {
				c.Monitors.HideFinishedAgeDays = config.FlexDuration{Value: v}
			},
			[]string{"0", "365", "365.000001", "0.5", ".5"},
		},
		{"probe_cooldown",
			func(c *config.MoomboxConfig, v float64) {
				c.Monitors.ProbeCooldown = config.FlexDuration{Value: v}
			},
			[]string{"0", "0.5", "86400", "999999999"},
		},
		{"interruption_timeout",
			func(c *config.MoomboxConfig, v float64) {
				c.Downloader.InterruptionTimeout = config.FlexDuration{Value: v}
			},
			[]string{"0", "0.5", "120", "999999999"},
		},
		{"incomplete_staging_expiry_days",
			func(c *config.MoomboxConfig, v float64) {
				c.Downloader.IncompleteStagingExpiryDays = config.FlexDuration{Value: v}
			},
			[]string{"0", "0.5", "7", "999999999"},
		},
		{"refresh_interval",
			func(c *config.MoomboxConfig, v float64) {
				c.Cookies.RefreshInterval = config.FlexDuration{Value: v}
			},
			[]string{"10", "10080", "9.999999", "10080.000001", "1.5", "360.5"},
		},
	}

	for _, c := range cases {
		for _, s := range c.values {
			if err := validateDecimal(s); err != nil {
				t.Errorf("%s: validateDecimal(%q) = %v — the table must only hold typeable strings", c.key, s, err)
				continue
			}
			v, err := strconv.ParseFloat(s, 64)
			if err != nil {
				t.Errorf("%s: ParseFloat(%q) = %v", c.key, s, err)
				continue
			}

			// config.Validate's verdict for this exact value.
			ref := config.Defaults()
			c.apply(ref, v)
			validateAccepts := true
			for _, e := range config.Validate(ref) {
				if strings.Contains(e.Error(), c.key) {
					validateAccepts = false
				}
			}

			// The TUI's verdict for the same value, typed into the form.
			cfg := config.Defaults()
			store := config.NewStore(cfg, "")
			m := NewSettingsModel()
			m.configStore = store
			m.cfg = cfg
			m.Open(cfg)
			m.values[c.key] = s
			m.applyValues()
			tuiAccepts := m.status != saveError

			if tuiAccepts != validateAccepts {
				t.Errorf("%s = %q: TUI accepts=%v, config.Validate accepts=%v (TUI said %q)",
					c.key, s, tuiAccepts, validateAccepts, m.errorMsg)
			}
		}
	}
}
