package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestProgressIntervalDefault pins the default the whole arc is calibrated
// against: 16 ms, the value ProgressTracker's gate carried as a constant
// before it became configurable, so an operator who never writes the key
// keeps exactly today's ~60 reports/second per job.
//
// MUTANT: leaving ProgressIntervalMS out of the Defaults() literal — it reads
// 0 here, and every job would then take NewProgressTracker's fallback rather
// than a value anyone can see in the config.
func TestProgressIntervalDefault(t *testing.T) {
	if got := Defaults().Downloader.ProgressIntervalMS; got != 16 {
		t.Errorf("default ProgressIntervalMS = %d, want 16", got)
	}
}

// TestProgressIntervalValidation mirrors SegmentWorkers' clamp style rather
// than the reorder ceilings': 0 is NOT a documented "disabled" value here.
// An ungated ProgressTracker writes the database once per arriving segment
// callback on every download at once, which is what the gate exists to
// prevent — so anything below 1 ms is an error that resets to the default.
// There is deliberately no maximum: progress_interval_ms = 1000 is a
// once-a-second progress line, which is slow but legal.
//
// MUTANT: writing the guard as `< 0` (the reorder ceilings' shape) — the
// "zero" row then keeps 0 and fails. MUTANT: adding an upper clamp — the
// 1000 row fails. MUTANT: resetting to a literal 16 instead of
// defaults.Downloader.ProgressIntervalMS — passes today and silently
// diverges the moment the default moves, which is why
// TestProgressIntervalDefault above is a separate assertion.
func TestProgressIntervalValidation(t *testing.T) {
	tests := []struct {
		name string
		in   int
		want int
	}{
		{"zero falls back to the default", 0, 16},
		{"negative falls back to the default", -8, 16},
		{"one millisecond is honoured", 1, 1},
		{"the eight-millisecond knob is honoured", 8, 8},
		{"the default", 16, 16},
		{"a slow value is NOT clamped", 1000, 1000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Downloader.ProgressIntervalMS = tc.in
			Normalize(cfg)
			if got := cfg.Downloader.ProgressIntervalMS; got != tc.want {
				t.Errorf("ProgressIntervalMS = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestProgressIntervalValidateReportsWithoutMutating pins the other half of
// the validateOrNormalize contract (DECISIONS #9): Validate REPORTS, Normalize
// REWRITES. Save runs Validate and refuses to write a failing config, so the
// error text is what an operator sees when a hand-edited 0 blocks their next
// settings save — it has to name the key.
//
// MUTANT: calling fail() outside the reportOnly branch or forgetting the
// `if !reportOnly` guard around the assignment — cfg is mutated here and the
// second assertion fails. MUTANT: a message that does not name the key (e.g.
// "progress interval too small") — the Contains assertion fails.
func TestProgressIntervalValidateReportsWithoutMutating(t *testing.T) {
	cfg := Defaults()
	cfg.Downloader.ProgressIntervalMS = 0

	errs := Validate(cfg)
	if len(errs) == 0 {
		t.Fatal("Validate accepted progress_interval_ms = 0")
	}
	var joined []string
	for _, err := range errs {
		joined = append(joined, err.Error())
	}
	all := strings.Join(joined, "\n")
	if !strings.Contains(all, "downloader.progress_interval_ms") {
		t.Errorf("no error named the key:\n%s", all)
	}
	if cfg.Downloader.ProgressIntervalMS != 0 {
		t.Errorf("Validate mutated cfg: ProgressIntervalMS = %d, want the offending 0 left alone",
			cfg.Downloader.ProgressIntervalMS)
	}
}

// TestProgressIntervalRoundTripsThroughSave proves Save accepts the 8 ms
// value the owner asked to be able to try, and that Load reads it back. Save
// validates first and REFUSES a failing config, so a value the validator did
// not know would make every subsequent settings save fail — losing every
// other edit in the same save. The same pin CookiesAcquisition carries.
//
// MUTANT: a toml tag that does not match the documented key (e.g.
// `progress_interval`) — Save writes one spelling and Load looks for another,
// so the reloaded value is the default 16 and this fails.
func TestProgressIntervalRoundTripsThroughSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	cfg := Defaults()
	cfg.Downloader.ProgressIntervalMS = 8
	if err := Save(cfg, path); err != nil {
		t.Fatalf("Save refused a legal progress_interval_ms: %v", err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Downloader.ProgressIntervalMS != 8 {
		t.Errorf("progress_interval_ms round-tripped as %d, want 8",
			reloaded.Downloader.ProgressIntervalMS)
	}
}
