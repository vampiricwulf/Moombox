package tui

import "testing"

// The first-run wizard was the sixth apply site for the two FlexDuration
// fields and the only one that still parsed them as ints: "0.5" was
// untypeable there (validateDigitsOnly) and, had it arrived any other way,
// strconv.Atoi would have written 0 — while the settings overlay and the Web
// UI both accept the fraction (CORE-7 / B-10).
//
// Driven through finishAdvancedSetup, the production builder, so what is
// asserted is the config the wizard hands to the save.
//
// Mutant: restore vNum/strconv.Atoi at the two apply sites — feedCheckInterval
// lands as 10 (the default: Atoi("1.5") errors and the n > 0 guard skips the
// assignment) and hideAge as 0.
func TestAdvancedWizardAcceptsFractionalDurations(t *testing.T) {
	m := NewSetupWizardModel()
	m.values["feedCheckInterval"] = "1.5"
	m.values["hideAge"] = "0.5"

	if got := m.finishAdvancedSetup(); got != "save" {
		t.Fatalf("finishAdvancedSetup = %q, want \"save\" (errorMsg %q)", got, m.errorMsg)
	}
	if m.pendingConfig == nil {
		t.Fatal("finishAdvancedSetup built no config")
	}
	if got := m.pendingConfig.Monitors.FeedCheckInterval.Value; got != 1.5 {
		t.Errorf("feed_check_interval = %v, want 1.5 — the wizard must not truncate a fraction the "+
			"settings overlay and the Web UI both accept", got)
	}
	if got := m.pendingConfig.Monitors.HideFinishedAgeDays.Value; got != 0.5 {
		t.Errorf("hide_finished_age_days = %v, want 0.5", got)
	}
}

// The keystroke filter is what made the fraction untypeable in the first
// place, and it is per FIELD, not per step: the two FlexDuration-backed rows
// take one decimal point, every other number row still refuses it (a port or
// a file count has no fractional meaning).
//
// Mutant: wire validateDigitsOnly to setupFieldDecimal too (or drop the type)
// — "0.5" is refused on the two rows and the first loop fails.
func TestWizardDecimalFieldsAcceptAPoint(t *testing.T) {
	decimal := map[string]bool{"feedCheckInterval": true, "hideAge": true}
	seen := 0
	for _, step := range advancedSetupSteps {
		for _, f := range step.fields {
			switch f.ftype {
			case setupFieldDecimal:
				if !decimal[f.key] {
					t.Errorf("%s is a decimal field but is not one of the FlexDuration-backed rows", f.key)
				}
				if err := validateDecimal("0.5"); err != nil {
					t.Errorf("%s: validateDecimal(\"0.5\") = %v, want nil", f.key, err)
				}
				seen++
			case setupFieldNumber:
				if decimal[f.key] {
					t.Errorf("%s must be a decimal field — its config home is a config.FlexDuration", f.key)
				}
				if err := validateDigitsOnly("0.5"); err == nil {
					t.Errorf("%s: an int field must keep validateDigitsOnly", f.key)
				}
			}
		}
	}
	if seen != len(decimal) {
		t.Errorf("found %d decimal fields, want %d — re-anchor this test rather than deleting it",
			seen, len(decimal))
	}
}
