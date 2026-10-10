package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// TestSettingsFieldSwitchParksTheCursorAtTheEnd is the defect as the operator
// met it: a port edited to "774" leaves the text input's cursor at offset 3,
// the move down to output_directory loads "./output", and because offset 3
// still fits inside the new value bubbles' SetValue leaves it there — so the
// first keystroke lands mid-value and the field reads "./oxutput".
//
// Mutant: loadTextInput without its CursorEnd — the keystroke goes to offset 3
// again.
func TestSettingsFieldSwitchParksTheCursorAtTheEnd(t *testing.T) {
	cfg := config.Defaults()
	m := NewSettingsModel()
	m.configStore = config.NewStore(cfg, "")
	m.cfg = cfg
	m.Open(cfg)

	m.values["port"] = "774"
	m.focusFieldForTest(t, "port")
	if got := m.textInput.Position(); got != 3 {
		t.Fatalf("premise lost: cursor at %d after loading port %q, want 3", got, m.values["port"])
	}

	m.values["output_directory"] = "./output"
	m.focusFieldForTest(t, "output_directory")
	if got, want := m.textInput.Position(), len("./output"); got != want {
		t.Fatalf("cursor at %d after moving to output_directory, want the end (%d)", got, want)
	}

	m.UpdateComponents(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if got := m.values["output_directory"]; got != "./outputx" {
		t.Errorf("first keystroke on the new field produced %q, want %q", got, "./outputx")
	}
}

// TestFieldReloadParksTheCursorAtTheEnd covers the sibling sites that reload a
// shared text input for a different field through the same mechanism: the
// import dialog's Tab between Title and Channel, the add-video dialog's step
// change from the URL to the timestamps, and the trim dialog's Tab between the
// start and end times. Each is driven with a cursor that fits inside the next
// value, which is the only case SetValue gets wrong.
func TestFieldReloadParksTheCursorAtTheEnd(t *testing.T) {
	t.Run("import dialog Tab", func(t *testing.T) {
		d := NewImportDialogModel()
		d.Open(t.TempDir())
		d.step = 1
		d.title = "abc"
		d.channel = "xyzw"
		d.syncToTextInput()
		d.textInput.SetCursor(1)

		d.HandleKey(keyTab)
		if got, want := d.textInput.Position(), len("xyzw"); got != want {
			t.Errorf("Tab to Channel left the cursor at %d, want %d", got, want)
		}
		d.UpdateComponents(tea.KeyPressMsg{Code: 'x', Text: "x"})
		if d.channel != "xyzwx" {
			t.Errorf("first keystroke on Channel produced %q, want %q", d.channel, "xyzwx")
		}
	})

	t.Run("add-video step change", func(t *testing.T) {
		d := NewAddVideoModel()
		d.Open()
		d.urlInput = "https://youtu.be/abc"
		d.syncToTextInput()
		d.textInput.SetCursor(2)

		d.step = AddStepTimestamps
		d.startTimeInput = "1:00"
		d.syncToTextInput()
		if got, want := d.textInput.Position(), len("1:00"); got != want {
			t.Errorf("the timestamps step left the cursor at %d, want %d", got, want)
		}
	})

	t.Run("trim dialog Tab", func(t *testing.T) {
		d := NewTrimDialogModel()
		d.Open("job", "title")
		d.startTimeInput = "0:10"
		d.endTimeInput = "1:00:00"
		loadTextInput(&d.textInput, d.startTimeInput)
		d.textInput.SetCursor(1)

		d.HandleKey(keyTab)
		if got, want := d.textInput.Position(), len("1:00:00"); got != want {
			t.Errorf("Tab to End left the cursor at %d, want %d", got, want)
		}
	})
}
