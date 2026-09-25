package tui

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// maxResolutionField finds the Downloader section's max_video_resolution row.
func maxResolutionField(t *testing.T) *fieldDef {
	t.Helper()
	for i := range sections {
		for j := range sections[i].fields {
			if sections[i].fields[j].key == "max_video_resolution" {
				return &sections[i].fields[j]
			}
		}
	}
	t.Fatal("no max_video_resolution row — the TUI cannot set the cap")
	return nil
}

// TestMaxResolutionRowOffersThePresets pins the picker. The row stays a NUMBER
// row so a custom value can still be typed (a cycle row gets no text input at
// all), and it carries the preset list the arrows step through — the same seven
// values the dashboard offers.
//
// Mutant: the options dropped — the row is an unguided number box again and the
// length assertion fails.
func TestMaxResolutionRowOffersThePresets(t *testing.T) {
	fd := maxResolutionField(t)
	if fd.ftype != fieldNumber {
		t.Errorf("max_video_resolution is %v, want fieldNumber — a cycle row cannot hold a custom value", fd.ftype)
	}
	want := []string{"0", "480", "720", "1080", "1440", "2160", "4320"}
	if len(fd.options) != len(want) {
		t.Fatalf("max_video_resolution has %d options, want %d: %v", len(fd.options), len(want), fd.options)
	}
	for i, opt := range want {
		if fd.options[i] != opt {
			t.Errorf("option %d = %q, want %q", i, fd.options[i], opt)
		}
	}
	// The spec's wording plus the arrow affordance — the row renders as a
	// plain text input, so nothing else on screen says the presets are there.
	if fd.help != "shorter edge in pixels; 0 = unbounded (e.g. 1080, 2160); ←/→ step the presets" {
		t.Errorf("help = %q, want the short-edge wording with the arrow affordance", fd.help)
	}
	if fd.previewFn == nil {
		t.Fatal("max_video_resolution has no previewFn — the numbers need their preset names")
	}
}

// TestResolutionPreviewNamesThePreset: the dim line under the row says what the
// number means, and says Custom for anything off the ladder.
//
// Mutant: the Custom branch dropped (an off-ladder value renders blank and the
// operator cannot tell 1234 from a typo).
func TestResolutionPreviewNamesThePreset(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"0", "Unbounded — always the largest"},
		{"480", "480p"},
		{"1080", "1080p"},
		{"2160", "4K (2160)"},
		{"4320", "8K (4320)"},
		{"1234", "Custom: 1234"},
		{"", ""},
		{"abc", ""},
	} {
		if got := resolutionPreview(tc.in); got != tc.want {
			t.Errorf("resolutionPreview(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestArrowsCycleThePresetsOnTheResolutionRow: a number row that declares
// options steps through them on the arrow keys, and the text input follows so
// the next keystroke does not sync the stale text back over the new value.
//
// Mutant: updateTextInputForField() dropped after the cycle — the input still
// shows the old number and the value silently reverts.
func TestArrowsCycleThePresetsOnTheResolutionRow(t *testing.T) {
	cfg := config.Defaults()
	m := NewSettingsModel()
	// Open reads through m.configStore (settings.go, (*Store).Read takes an
	// RLock), so a nil store is a nil-pointer panic that aborts the whole
	// package binary — the sibling TestUnboundedResolutionSaves sets it too.
	m.configStore = config.NewStore(cfg, "")
	m.cfg = cfg
	m.Open(cfg)
	m.focusFieldForTest(t, "max_video_resolution")

	if got := m.values["max_video_resolution"]; got != "2160" {
		t.Fatalf("seeded value = %q, want the 2160 default", got)
	}
	m.HandleKey(keyRight)
	if got := m.values["max_video_resolution"]; got != "4320" {
		t.Errorf("right arrow gave %q, want 4320 — the next preset", got)
	}
	if got := m.textInput.Value(); got != "4320" {
		t.Errorf("the text input shows %q after the cycle, want 4320", got)
	}
	m.HandleKey(keyLeft)
	m.HandleKey(keyLeft)
	if got := m.values["max_video_resolution"]; got != "1440" {
		t.Errorf("two left arrows gave %q, want 1440", got)
	}
}

// TestArrowsStepToTheNearestPresetFromACustomValue: arrowing off a typed value
// that is on no preset lands on the NEIGHBOURING preset, not at one end of the
// ladder. The generic cycleFieldOption falls through to options[0] ("0" =
// Unbounded) or options[len-1] ("4320") for an unrecognised value, which would
// turn a → on 1234 into "record everything at full size".
//
// Mutant: cycleFieldForward/Reverse used for a number row instead of
// cycleNumberPreset — 1234 steps to "0" and back to "4320".
func TestArrowsStepToTheNearestPresetFromACustomValue(t *testing.T) {
	cfg := config.Defaults()
	m := NewSettingsModel()
	m.configStore = config.NewStore(cfg, "")
	m.cfg = cfg
	m.Open(cfg)
	m.focusFieldForTest(t, "max_video_resolution")

	for _, tc := range []struct {
		why  string
		from string
		key  string
		want string
	}{
		{"off-ladder steps up to the next preset above", "1234", keyRight, "1440"},
		{"off-ladder steps down to the next preset below", "1234", keyLeft, "1080"},
		{"nothing above 4320, so it wraps to the ladder head", "5000", keyRight, "0"},
		{"nothing below 0, so it wraps to the ladder tail", "-7", keyLeft, "4320"},
		{"the exact-preset wrap is unchanged", "4320", keyRight, "0"},
		{"the exact-preset wrap the other way", "0", keyLeft, "4320"},
		{"mid-edit with nothing typed falls to the head", "", keyRight, "0"},
	} {
		m.values["max_video_resolution"] = tc.from
		m.updateTextInputForField()
		m.HandleKey(tc.key)
		if got := m.values["max_video_resolution"]; got != tc.want {
			t.Errorf("%s from %q gave %q, want %q (%s)", tc.key, tc.from, got, tc.want, tc.why)
		}
	}
}

// TestSetupWizardAcceptsUnboundedResolution is the fourth surface. vNum returns
// 0 for an EMPTY field and for a literal "0" alike, so the apply site's old
// `n > 0` guard discarded an explicit 0 and left the 2160 default — the TUI
// wizard would have offered no way to reach the mode the dashboard wizard
// reaches, a dual-UI parity break this arc would have created itself.
//
// Mutant: the `if n := vNum("maxRes"); n > 0` guard restored — the "0 is
// unbounded" case comes back 2160.
func TestSetupWizardAcceptsUnboundedResolution(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry string
		want  int
	}{
		{"0 is unbounded", "0", 0},
		{"empty leaves the default", "", 2160},
		{"a typed cap is taken", "1440", 1440},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewSetupWizardModel()
			m.values["maxRes"] = tc.entry
			if got := m.finishAdvancedSetup(); got != "save" {
				t.Fatalf("finishAdvancedSetup = %q for entry %q, want \"save\" (errorMsg %q)",
					got, tc.entry, m.errorMsg)
			}
			if m.pendingConfig == nil {
				t.Fatalf("finishAdvancedSetup built no config for entry %q", tc.entry)
			}
			if got := m.pendingConfig.Downloader.MaxVideoResolution; got != tc.want {
				t.Errorf("MaxVideoResolution = %d for entry %q, want %d", got, tc.entry, tc.want)
			}
		})
	}
}

// TestSetupWizardResolutionHelpDescribesTheShortEdge: the wizard row's help
// said "Maximum video dimension (width or height)", which R1 makes false.
//
// Mutant: the old wording restored.
func TestSetupWizardResolutionHelpDescribesTheShortEdge(t *testing.T) {
	for _, step := range advancedSetupSteps {
		for _, f := range step.fields {
			if f.key != "maxRes" {
				continue
			}
			if f.help != "Shorter edge in pixels; 0 = unbounded (always the largest)" {
				t.Errorf("maxRes help = %q, want the short-edge wording", f.help)
			}
			return
		}
	}
	t.Fatal("no maxRes field in the advanced setup steps")
}

// TestUnboundedResolutionSaves: 0 must reach the config, not the validator's
// error path. The TUI table used to demand >= 1.
//
// Mutant: the floor left at 1 — status is saveError and the config keeps 2160.
func TestUnboundedResolutionSaves(t *testing.T) {
	cfg := config.Defaults()
	store := config.NewStore(cfg, "")
	m := NewSettingsModel()
	m.configStore = store
	m.cfg = cfg
	m.Open(cfg)
	m.values["max_video_resolution"] = "0"
	m.dirty = true
	m.OnSave = func(*config.MoomboxConfig) error { return nil }

	m.saveAndClose()

	if m.status != saveSaved {
		t.Fatalf("status = %v (%s), want saveSaved — 0 is the unbounded mode", m.status, m.errorMsg)
	}
	if got := cfg.Downloader.MaxVideoResolution; got != 0 {
		t.Errorf("MaxVideoResolution = %d after saving 0, want 0", got)
	}
}

// focusFieldForTest selects the named field in its own section and wires the
// text input to it, the way the overlay does when the operator arrows onto it.
func (m *SettingsModel) focusFieldForTest(t *testing.T, key string) {
	t.Helper()
	for si := range sections {
		for fi := range sections[si].fields {
			if sections[si].fields[fi].key != key {
				continue
			}
			m.sectionIndex = si
			m.fieldIndex = fi
			m.updateTextInputForField()
			return
		}
	}
	t.Fatalf("no %s field in any section", key)
}
