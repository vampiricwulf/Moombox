package tui

import (
	"strconv"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// reorderRow finds a Downloader field by key.
func reorderRow(t *testing.T, key string) fieldDef {
	t.Helper()
	for _, sec := range sections {
		if sec.name != "Downloader" {
			continue
		}
		for _, fd := range sec.fields {
			if fd.key == key {
				return fd
			}
		}
	}
	t.Fatalf("the Downloader section has no %q row — the setting is unreachable from the terminal", key)
	return fieldDef{}
}

// TestReorderRowsAreNumbersThatDocumentZero: both ceilings are integers whose
// 0 means unbounded, and the hint is the only place the operator learns that
// — a blank numeric field they assume means "default" would instead be
// refused by the validator.
//
// MUTANT: drop "0 = unbounded" from either hint — that row's assertion fires.
// MUTANT: make either row a fieldText — the ftype assertion fires and the
// overlay stops digit-validating the input.
func TestReorderRowsAreNumbersThatDocumentZero(t *testing.T) {
	for _, tc := range []struct{ key, wantHintFragment string }{
		{"reorder_buffer_mb", "MB of out-of-order segments one download may hold in RAM; 0 = unbounded"},
		{"reorder_budget_mb", "MB every download's reorder buffer may hold between them; 0 = unbounded"},
	} {
		fd := reorderRow(t, tc.key)
		if fd.ftype != fieldNumber {
			t.Errorf("%s row ftype = %v, want fieldNumber", tc.key, fd.ftype)
		}
		if !strings.Contains(fd.help, tc.wantHintFragment) {
			t.Errorf("%s hint = %q, want it to contain %q", tc.key, fd.help, tc.wantHintFragment)
		}
		if !strings.Contains(fd.help, "arm64") {
			t.Errorf("%s hint = %q, want it to name the arm64 default — the two platforms differ and the "+
				"operator cannot otherwise tell which one they are looking at", tc.key, fd.help)
		}
	}
}

// TestReorderValuesRoundTrip: loaded into the form and written back
// unchanged, zero included.
//
// MUTANT: drop either seed line — the loaded value reads "" and the first
// assertion fires; applyValues then refuses the save outright. MUTANT: drop
// either apply line — the config keeps its old value and the second
// assertion fires, which is a save that silently reverted.
func TestReorderValuesRoundTrip(t *testing.T) {
	cfg := config.Defaults()
	cfg.Downloader.ReorderBufferMB = 768
	cfg.Downloader.ReorderBudgetMB = 0

	store := config.NewStore(cfg, "")
	m := NewSettingsModel()
	m.configStore = store
	m.cfg = cfg
	m.Open(cfg)

	if got := m.values["reorder_buffer_mb"]; got != "768" {
		t.Errorf("loaded reorder_buffer_mb = %q, want \"768\"", got)
	}
	if got := m.values["reorder_budget_mb"]; got != "0" {
		t.Errorf("loaded reorder_budget_mb = %q, want \"0\"", got)
	}

	m.values["reorder_buffer_mb"] = strconv.Itoa(2048)
	m.applyValues()
	if m.status == saveError {
		t.Fatalf("applyValues refused a valid config: %s", m.errorMsg)
	}
	if cfg.Downloader.ReorderBufferMB != 2048 {
		t.Errorf("ReorderBufferMB = %d, want 2048", cfg.Downloader.ReorderBufferMB)
	}
	if cfg.Downloader.ReorderBudgetMB != 0 {
		t.Errorf("ReorderBudgetMB = %d, want 0 — an explicit unbounded must survive an unrelated save",
			cfg.Downloader.ReorderBudgetMB)
	}
}

// TestReorderValidatorAcceptsZeroAndRefusesGarbage: the pre-write range check
// mirrors config.Validate (Save REFUSES to persist a config that fails it),
// so an unparseable field has to be caught here or it poisons the live config
// while the overlay reports "Saved".
//
// MUTANT: give either row a floor of 1 — the zero case reports saveError and
// "unbounded" becomes untypeable in the terminal. MUTANT: omit either row
// from the validator table — the empty case saves 0 silently, which happens
// to mean unbounded and so is invisible until a machine runs out of RAM.
func TestReorderValidatorAcceptsZeroAndRefusesGarbage(t *testing.T) {
	for _, tc := range []struct {
		name          string
		perJob, total string
		wantErr       bool
	}{
		{"zero on both", "0", "0", false},
		{"ordinary pair", "1024", "4096", false},
		{"empty per-job", "", "4096", true},
		{"empty budget", "1024", "", true},
		{"non-numeric per-job", "lots", "4096", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Defaults()
			store := config.NewStore(cfg, "")
			m := NewSettingsModel()
			m.configStore = store
			m.cfg = cfg
			m.Open(cfg)
			m.values["reorder_buffer_mb"] = tc.perJob
			m.values["reorder_budget_mb"] = tc.total

			m.applyValues()
			if got := m.status == saveError; got != tc.wantErr {
				t.Errorf("saveError = %v (%q), want %v", got, m.errorMsg, tc.wantErr)
			}
		})
	}
}

// TestReorderKeysAreNotRestartRequired: both are re-applied on save through
// runState.applyReorderBudget, so warning about a restart would be a lie —
// and TestRestartRequiredListsAgree would then demand the same lie from the
// dashboard.
//
// MUTANT: add either key to restartRequiredKeys — this fails, and
// TestRestartRequiredListsAgree fails too (the Web list has neither).
func TestReorderKeysAreNotRestartRequired(t *testing.T) {
	for _, key := range []string{"reorder_buffer_mb", "reorder_budget_mb"} {
		if restartRequiredKeys[key] {
			t.Errorf("%s is marked restart-required, but cmd/moombox re-applies it on every save", key)
		}
	}
}
