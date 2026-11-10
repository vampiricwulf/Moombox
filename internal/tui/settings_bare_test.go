package tui

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// The bare form of Settings (settings_bare.go, settings_bare_lists.go;
// owner ruling R-C13).

const (
	bareHookToken = "SECRETtokenCHARACTERS-that-must-never-be-drawn"
	bareHookID    = "1234567890123456789"
	// A non-Discord URL longer than the 50 cells a list ever shows of one.
	bareOtherHook = "https://hooks.example.com/services/T00000000/B00000000/tail-PAST-the-fifty-cells"
)

// bareSettingsRig is a rig at w x h with channels, webhooks and a password
// configured, and Settings open.
func bareSettingsRig(t *testing.T, w, h int) *compactRig {
	t.Helper()
	r := newCompactRig(t, w, h)
	a := r.app
	off := false
	a.cfg.Channels = []config.ChannelConfig{
		{ID: "UCabcdefghijklmnopqrstuv", Name: "Some Channel Name That Is Long"},
		{ID: "streamer_name", Platform: "twitch", Enabled: &off},
	}
	for i := range 20 {
		a.cfg.Channels = append(a.cfg.Channels, config.ChannelConfig{ID: fmt.Sprintf("UC%022d", i), Name: fmt.Sprintf("Channel %d", i)})
	}
	a.cfg.Notifications = []config.NotificationConfig{
		{URL: "https://discord.com/api/webhooks/" + bareHookID + "/" + bareHookToken},
		{URL: bareOtherHook, Enabled: &off},
		{URL: "https://discord.com/api/webhooks/5555555555555555555/" + bareHookToken, Mention: "@here"},
	}
	a.OnHashPassword = func(pw string) string { return "hash:" + pw }
	a.OnVerifyPassword = func(pw, hash string) bool { return "hash:"+pw == hash }
	a.OnSaveConfig = func(*config.MoomboxConfig) error { r.saves++; return r.saveErr }
	a.OnChannelRemovalSummary = func(string, string) (ChannelRemovalInfo, error) {
		return ChannelRemovalInfo{Total: 12, Pending: 3}, nil
	}
	r.press("`")
	if !a.settings.IsVisible() {
		t.Fatalf("%dx%d: ` did not open Settings: %q", w, h, a.feedback.msg)
	}
	return r
}

// toSection moves Settings to the section called name, as Tab would.
func toSection(t *testing.T, m *SettingsModel, name string) {
	t.Helper()
	i := slices.IndexFunc(sections, func(s settingsSection) bool { return s.name == name })
	if i < 0 {
		t.Fatalf("no section %q", name)
	}
	m.switchSection(i)
	m.updateTextInputForField()
}

// toField puts the focus of the current field section on the field key.
func toField(t *testing.T, m *SettingsModel, key string) {
	t.Helper()
	i := slices.IndexFunc(sections[m.sectionIndex].fields, func(f fieldDef) bool { return f.key == key })
	if i < 0 {
		t.Fatalf("section %q has no field %q", sections[m.sectionIndex].name, key)
	}
	m.fieldIndex = i
	m.ensureFieldVisible()
	m.updateTextInputForField()
}

const bareLongError = "Save failed: open C:\\Users\\somebody\\AppData\\Roaming\\Moombox\\config.toml: The process cannot access the file because it is being used by another process."

// bareSettingsStates are the states of Settings a test draws, each reached
// from a freshly opened overlay. The ones in bareSettingsCompactOnly exist
// only below the normal layout.
var bareSettingsStates = map[string]func(t *testing.T, r *compactRig){
	"network": func(t *testing.T, r *compactRig) {},
	"network, a toggle row": func(t *testing.T, r *compactRig) {
		toField(t, r.app.settings, "https_enabled")
	},
	"network, the longest help": func(t *testing.T, r *compactRig) {
		toField(t, r.app.settings, "public_url")
	},
	"network, last field edited": func(t *testing.T, r *compactRig) {
		toField(t, r.app.settings, "probe_targets")
		r.press("x")
	},
	"downloader, a preview": func(t *testing.T, r *compactRig) {
		m := r.app.settings
		for i, sec := range sections {
			for _, fd := range sec.fields {
				if fd.previewFn != nil && fd.previewFn(m.values[fd.key]) != "" {
					m.switchSection(i)
					toField(t, m, fd.key)
					return
				}
			}
		}
		t.Fatal("premise lost: no field draws a preview")
	},
	"a long value": func(t *testing.T, r *compactRig) {
		toSection(t, r.app.settings, "Paths")
		for range 12 {
			r.press("l", "o", "n", "g", "/")
		}
	},
	"save prompt": func(t *testing.T, r *compactRig) {
		r.press("1", "esc")
		if !r.app.settings.closeConfirm {
			t.Fatal("premise lost: Esc with an edit did not ask")
		}
	},
	"save error": func(t *testing.T, r *compactRig) {
		r.press("1")
		r.app.settings.status, r.app.settings.errorMsg = saveError, bareLongError
	},
	"restart prompt": func(t *testing.T, r *compactRig) {
		r.app.settings.showRestartOverlay = true
	},
	"password, set": func(t *testing.T, r *compactRig) {
		toField(t, r.app.settings, "https_enabled")
		r.press("`", "h", "u", "n", "t", "e", "r", " ")
	},
	"password, change": func(t *testing.T, r *compactRig) {
		r.app.cfg.Network.PasswordHash = "hash:old"
		toField(t, r.app.settings, "https_enabled")
		r.press("`", "o", "l", "d", "down", "h", "u", "n", "t", "e", "r", "2", "2", "enter")
		if r.app.settings.secMessage == "" {
			t.Fatal("premise lost: a mismatched confirm left no message")
		}
	},
	"password, a long refusal": func(t *testing.T, r *compactRig) {
		r.app.cfg.Network.PasswordHash = "hash:old"
		toField(t, r.app.settings, "https_enabled")
		r.press("`")
		r.app.settings.secMessage, r.app.settings.secMessageColor = bareLongError, ColorRed
	},
	"password, remove": func(t *testing.T, r *compactRig) {
		r.app.cfg.Network.PasswordHash = "hash:old"
		r.app.cfg.Network.NetworkAccess = "external"
		toField(t, r.app.settings, "https_enabled")
		r.press("~", "w", "r", "o", "n", "g", "enter")
		if r.app.settings.secMode != securityRemove || r.app.settings.secMessage == "" {
			t.Fatal("premise lost: a wrong password did not leave the removal open with a message")
		}
	},
	"channels": func(t *testing.T, r *compactRig) {
		toSection(t, r.app.settings, "Channels")
	},
	"channels, far down": func(t *testing.T, r *compactRig) {
		toSection(t, r.app.settings, "Channels")
		for range 15 {
			r.press("down")
		}
	},
	"channels, none": func(t *testing.T, r *compactRig) {
		r.app.settings.channels = nil
		toSection(t, r.app.settings, "Channels")
	},
	"removal, counting": func(t *testing.T, r *compactRig) {
		toSection(t, r.app.settings, "Channels")
		r.press("d")
	},
	"removal, counted": func(t *testing.T, r *compactRig) {
		toSection(t, r.app.settings, "Channels")
		r.press("d")
		m := r.app.settings
		m.HandleChannelRemovalSummary(m.removalID, ChannelRemovalInfo{Total: 12, Pending: 3}, nil)
	},
	"removal, everything": func(t *testing.T, r *compactRig) {
		toSection(t, r.app.settings, "Channels")
		r.press("d")
		m := r.app.settings
		m.HandleChannelRemovalSummary(m.removalID, ChannelRemovalInfo{
			Total: 123456, Pending: 12345, Active: 2, Footage: []string{"A parked recording with a long title", "another"},
		}, nil)
	},
	"removal, nothing to delete": func(t *testing.T, r *compactRig) {
		toSection(t, r.app.settings, "Channels")
		r.press("d")
		m := r.app.settings
		m.HandleChannelRemovalSummary(m.removalID, ChannelRemovalInfo{}, nil)
	},
	"removal, not counted": func(t *testing.T, r *compactRig) {
		toSection(t, r.app.settings, "Channels")
		r.press("d")
		m := r.app.settings
		m.HandleChannelRemovalSummary(m.removalID, ChannelRemovalInfo{}, errors.New("database is locked (5) (SQLITE_BUSY) while counting the jobs of the channel"))
	},
	"channel editor": func(t *testing.T, r *compactRig) {
		toSection(t, r.app.settings, "Channels")
		r.press("enter")
	},
	"channel editor, twitch, last field": func(t *testing.T, r *compactRig) {
		toSection(t, r.app.settings, "Channels")
		r.press("down", "enter")
		for range len(channelFields) {
			r.press("down")
		}
	},
	"channel editor, an error": func(t *testing.T, r *compactRig) {
		toSection(t, r.app.settings, "Channels")
		r.press("enter")
		for r.app.settings.visibleChannelFields()[r.app.settings.channelEditField].key != "archive_slots" {
			r.press("down")
		}
		r.press("9", "9", "9", "enter")
		if r.app.settings.status != saveError {
			t.Fatal("premise lost: 999 archive slots were accepted")
		}
	},
	"channel editor, resolving": func(t *testing.T, r *compactRig) {
		toSection(t, r.app.settings, "Channels")
		r.press("a", "@", "h", "a", "n", "d", "l", "e", "enter")
		if !r.app.settings.channelResolving {
			t.Fatal("premise lost: a handle is not being resolved")
		}
	},
	"integrations": func(t *testing.T, r *compactRig) {
		toSection(t, r.app.settings, "Integrations")
	},
	"integrations, none": func(t *testing.T, r *compactRig) {
		r.app.settings.notifications, r.app.settings.notifFrom = nil, nil
		toSection(t, r.app.settings, "Integrations")
	},
	"integrations, a test sent, delete armed": func(t *testing.T, r *compactRig) {
		toSection(t, r.app.settings, "Integrations")
		r.press("t", "d")
	},
	"integrations, a test failed": func(t *testing.T, r *compactRig) {
		toSection(t, r.app.settings, "Integrations")
		r.press("down", "down")
		r.app.settings.SetNotifTestResult("webhook returned 404 Not Found: {\"message\": \"Unknown Webhook\", \"code\": 10015}")
	},
	"webhook editor": func(t *testing.T, r *compactRig) {
		toSection(t, r.app.settings, "Integrations")
		r.press("enter")
	},
	"webhook editor, mention": func(t *testing.T, r *compactRig) {
		toSection(t, r.app.settings, "Integrations")
		r.press("down", "down", "enter", "down", "down")
	},
	"webhook editor, the last event": func(t *testing.T, r *compactRig) {
		toSection(t, r.app.settings, "Integrations")
		r.press("enter")
		for range notifEditEventBase + len(allNotifEvents) {
			r.press("down")
		}
		r.press("space", "m")
	},
	"webhook editor, an error": func(t *testing.T, r *compactRig) {
		toSection(t, r.app.settings, "Integrations")
		r.press("a", "n", "o", "p", "e", "enter")
		if r.app.settings.status != saveError {
			t.Fatal("premise lost: \"nope\" was accepted as a webhook URL")
		}
	},
}

var bareSettingsCompactOnly = map[string]func(t *testing.T, r *compactRig){
	"the installer refused": func(t *testing.T, r *compactRig) {
		toSection(t, r.app.settings, "Paths")
		toField(t, r.app.settings, "ffmpeg_path")
		r.press("ctrl+o")
	},
	"password set, its result": func(t *testing.T, r *compactRig) {
		toField(t, r.app.settings, "https_enabled")
		r.press("`")
		for _, k := range "hunter22" {
			r.press(string(k))
		}
		r.press("down")
		for _, k := range "hunter22" {
			r.press(string(k))
		}
		r.press("enter")
		if r.app.settings.secMode != securityStatus {
			t.Fatalf("premise lost: the password was not set: %q", r.app.settings.secMessage)
		}
	},
}

// Every state of Settings is exactly the terminal's size at every compact
// size, wide-short and narrow-tall included.
//
// Mutant: a message appended without taking a row; an input a cell too
// wide; a window one row too tall.
func TestBareSettingsFillsEveryCompactSize(t *testing.T) {
	compactSizes(func(w, h int) {
		for _, states := range []map[string]func(*testing.T, *compactRig){bareSettingsStates, bareSettingsCompactOnly} {
			for name, build := range states {
				r := bareSettingsRig(t, w, h)
				build(t, r)
				what := fmt.Sprintf("%s at %dx%d", name, w, h)
				wantExact(t, what, r.app.settings.View(), w, h)
				if got := r.app.View().Content; got != r.app.settings.View() {
					t.Errorf("%s: the screen is not the bare Settings", what)
				}
			}
		}
	})
}

// Settings in any state, shrunk through the compact sizes and grown back,
// draws the frame of one that was never shrunk: the bare form sets nothing
// on the shared text input, the scroll offset or the focus.
//
// Mutant: bareInput laying out the model's own input; SetSize running
// ensureFieldVisible at a bare size.
func TestBareSettingsSurvivesAShrinkAndBack(t *testing.T) {
	for name, build := range bareSettingsStates {
		for _, normal := range [][2]int{{60, 20}, {120, 40}} {
			open := func() *compactRig {
				r := bareSettingsRig(t, normal[0], normal[1])
				build(t, r)
				return r
			}
			want := open().app.View().Content
			r := open()
			for _, s := range [][2]int{{30, 10}, {200, 8}, {30, 50}, {30, 6}, {40, 10}} {
				r.app.Update(tea.WindowSizeMsg{Width: s[0], Height: s[1]})
				wantExact(t, fmt.Sprintf("%s shrunk to %dx%d", name, s[0], s[1]), r.app.View().Content, s[0], s[1])
			}
			r.app.Update(tea.WindowSizeMsg{Width: normal[0], Height: normal[1]})
			if got := r.app.View().Content; got != want {
				t.Errorf("%s: the %dx%d frame changed across a shrink:\n%s\nnever shrunk:\n%s", name, normal[0], normal[1], ansi.Strip(got), ansi.Strip(want))
			}
		}
	}
}

// The bare Settings at the compact floor, state by state.
func TestBareSettingsFrames(t *testing.T) {
	frame := func(state string, more ...string) (*compactRig, string) {
		r := bareSettingsRig(t, 30, 6)
		build, ok := bareSettingsStates[state]
		if !ok {
			build = bareSettingsCompactOnly[state]
		}
		build(t, r)
		r.press(more...)
		return r, r.app.View().Content
	}
	for _, tt := range []struct {
		state string
		more  []string
		want  []string
	}{
		{"network", nil, []string{
			"Settings 1/13 Network",
			"> Port",
			"  774",
			"  Network access  [localhost]",
			"web dashboard port, 1-65535 (…",
			"pw:none (keys: toggle row)",
		}},
		{"network, a toggle row", []string{"right"}, []string{
			"Settings 1/13 Network        *",
			"  Network access  [localhost]",
			"*>HTTPS enabled",
			"  [Yes] / No",
			"serve over HTTPS, needs TLS c…",
			"pw:none  `:set",
		}},
		{"save prompt", nil, []string{
			"Settings 1/13 Network        *",
			"*>Port",
			"  7741",
			"  Network access  [localhost]",
			"web dashboard port, 1-65535 (…",
			"Save changes? Y/N  Esc:back",
		}},
		{"restart prompt", nil, []string{
			"Restart needed.",
			"Some changes apply only after",
			"a restart.",
			"",
			"Enter: restart now",
			"Esc: close, restart later",
		}},
		{"password, change", nil, []string{
			"Change password",
			"  Current  •••",
			"> New      ********",
			"  Confirm",
			"Passwords do not match",
			"Enter:save  Esc:cancel",
		}},
		{"password, remove", nil, []string{
			"Remove password",
			"Network access will be reset",
			"to localhost.",
			"> Current  *****",
			"Current password is incorrect",
			"Enter:confirm  Esc:cancel",
		}},
		{"the installer refused", nil, []string{
			"Settings 2/13 Paths",
			"> FFmpeg path",
			"",
			"empty = system PATH",
			"Installer needs 60x20",
			"Tab:section Esc:close ↑↓:field",
		}},
		{"channels", []string{"down"}, []string{
			"Settings 12/13 Channels",
			"  YT Some Channel Name That I…",
			"> TW streamer_name (disabled)",
			"  YT Channel 0",
			"  YT Channel 1",
			"A:add Ent:edit D:del Tab:next",
		}},
		{"removal, counted", nil, []string{
			"Remove \"Some Channel Name T…\"?",
			"It has 12 jobs.",
			"Enter: remove, keep 12 jobs",
			"P: remove + delete 3 pending",
			"Esc: cancel",
			"",
		}},
		{"removal, nothing to delete", nil, []string{
			"Remove \"Some Channel Name T…\"?",
			"No job will be deleted.",
			"Enter: remove",
			"Esc: cancel",
			"",
			"",
		}},
		{"integrations", []string{"down"}, []string{
			"Settings 13/13 Integrations",
			"  …/1234567890123456789",
			"> https://hooks.example… Muted",
			"  …/5555555555555555555",
			"",
			"A:add  Ent:edit  D:del  T:test",
		}},
		{"integrations, a test sent, delete armed", nil, []string{
			"Settings 13/13 Integrations",
			"> …/1234567890123456789",
			"  https://hooks.example… Muted",
			"  …/5555555555555555555",
			"Sending test notification...",
			"Press D again to delete",
		}},
	} {
		r, got := frame(tt.state, tt.more...)
		_ = r
		wantFrame(t, tt.state, got, 30, 6, tt.want)
	}
}

// settingsWalk opens Settings at w x h and, with keys alone, visits every
// section, every field of it, every field of the channel editor on both
// platforms and every row of the webhook editor, editing each one. It
// returns what it reached and edited. At each stop the focused row must be
// on screen.
func settingsWalk(t *testing.T, w, h int) []string {
	t.Helper()
	r := bareSettingsRig(t, w, h)
	a, m := r.app, r.app.settings
	var reached []string
	onScreen := func(what, label string) {
		t.Helper()
		cut := truncateString(label, w-2)
		for _, row := range plainRows(a.View().Content) {
			if i := strings.Index(row, cut); i >= 0 && strings.ContainsAny(row[:i], ">") {
				return
			}
		}
		t.Errorf("%dx%d: %s: the focused row %q is not on screen:\n%s", w, h, what, cut, plainFrame(a))
	}
	edit := func(what string, ftype fieldType, options []string, read func() string) {
		t.Helper()
		before := read()
		switch ftype {
		case fieldToggle, fieldCycle:
			r.press("right")
		case fieldNumber:
			r.press("1")
		default:
			r.press("x")
		}
		if ftype == fieldCycle && len(options) < 2 {
			return
		}
		if read() == before {
			t.Errorf("%dx%d: %s: the edit did not change it from %q", w, h, what, before)
		}
	}

	for si, sec := range sections {
		for range len(sections) {
			if m.sectionIndex == si {
				break
			}
			r.press("tab")
		}
		if m.sectionIndex != si {
			t.Fatalf("%dx%d: Tab never reached section %q", w, h, sec.name)
		}
		for fi, fd := range sec.fields {
			if m.fieldIndex != fi {
				t.Fatalf("%dx%d: %s: Down did not reach field %d", w, h, sec.name, fi)
			}
			what := sec.name + "/" + fd.key
			onScreen(what, fd.label)
			edit(what, fd.ftype, fd.options, func() string { return m.values[fd.key] })
			onScreen(what+" edited", fd.label)
			reached = append(reached, what)
			r.press("down")
		}
	}

	// The channel editor, a YouTube channel and then a Twitch one.
	for range len(sections) {
		if sections[m.sectionIndex].name == "Channels" {
			break
		}
		r.press("tab")
	}
	for _, platform := range []string{"youtube", "twitch"} {
		r.press("a")
		if m.channelMode != "edit" {
			t.Fatalf("%dx%d: A did not open the channel editor", w, h)
		}
		if platform == "twitch" {
			r.press("down", "down", "right")
			if m.channelEditValues["platform"] != "twitch" {
				t.Fatalf("%dx%d: the platform row did not switch to twitch", w, h)
			}
			r.press("up", "up")
		}
		for fi, fd := range m.visibleChannelFields() {
			if m.channelEditField != fi {
				t.Fatalf("%dx%d: channel editor: Down did not reach field %d", w, h, fi)
			}
			what := "channel/" + platform + "/" + fd.key
			onScreen(what, bareChannelLabelAt(fd.label, w, h))
			if fd.key != "platform" {
				edit(what, fd.ftype, fd.options, func() string { return m.channelEditValues[fd.key] })
			}
			reached = append(reached, what)
			r.press("down")
		}
		r.press("esc")
	}

	// The webhook editor.
	r.press("tab")
	if sections[m.sectionIndex].name != "Integrations" {
		t.Fatalf("%dx%d: Tab from Channels did not reach Integrations", w, h)
	}
	r.press("a")
	if m.notifMode != "edit" {
		t.Fatalf("%dx%d: A did not open the webhook editor", w, h)
	}
	for row := range notifEditEventBase + len(allNotifEvents) {
		if m.notifEditFocus != row {
			t.Fatalf("%dx%d: webhook editor: Down did not reach row %d", w, h, row)
		}
		what := fmt.Sprintf("webhook/%d", row)
		switch row {
		case notifEditURLRow:
			edit(what, fieldText, nil, func() string { return m.notifEditURL })
		case notifEditMentionRow:
			edit(what, fieldText, nil, func() string { return m.notifEditMention })
		case notifEditEnabledRow:
			was := m.notifEditEnabled
			r.press("space")
			if m.notifEditEnabled == was {
				t.Errorf("%dx%d: Space did not toggle Enabled", w, h)
			}
		case notifEditDeliveryRow:
			was := m.notifEditDelivery
			r.press("space")
			if m.notifEditDelivery == was {
				t.Errorf("%dx%d: Space did not toggle Delivery", w, h)
			}
		default:
			event := allNotifEvents[row-notifEditEventBase]
			what = "webhook/" + event
			if layoutModeFor(w, h) == layoutCompact {
				onScreen(what, event)
			}
			ticked, pinged := m.notifEditEvents[event], m.notifEditMentionEvents[event]
			r.press("space", "m")
			if m.notifEditEvents[event] == ticked || m.notifEditMentionEvents[event] == pinged {
				t.Errorf("%dx%d: %s: Space and m did not toggle its tick and its mention", w, h, event)
			}
		}
		reached = append(reached, what)
		r.press("down")
	}
	return reached
}

// bareChannelLabelAt is a channel field's label as the editor draws it at
// w x h: without the platform suffix below the normal layout.
func bareChannelLabelAt(label string, w, h int) string {
	if layoutModeFor(w, h) == layoutCompact {
		return bareChannelLabel(label)
	}
	return label
}

// Every setting reachable and editable at the normal size is reachable and
// editable at the compact floor (owner ruling R-C13), with the same keys:
// every field of every section, every channel field of both platforms and
// every row of the webhook editor.
//
// Mutant: a bare window that does not follow the focus — the focused row is
// off screen. Mutant: bareKey swallowing Down.
func TestEverySettingIsReachableAndEditableAtTheFloor(t *testing.T) {
	normal := settingsWalk(t, 60, 20)
	if len(normal) < 60 {
		t.Fatalf("premise lost: the walk at 60x20 reached only %d settings", len(normal))
	}
	for _, size := range [][2]int{{30, 6}, {40, 10}, {200, 6}, {30, 50}} {
		if got := settingsWalk(t, size[0], size[1]); !slices.Equal(got, normal) {
			t.Errorf("%v: the walk reached %d settings, 60x20 reaches %d:\n%v\nwant:\n%v", size, len(got), len(normal), got, normal)
		}
	}
}

// Unfocused rows of one page are told apart at the compact floor: no two
// labels are cut to the same text.
func TestBareSettingsLabelsStayDistinct(t *testing.T) {
	r := bareSettingsRig(t, 30, 6)
	m := r.app.settings
	check := func(page string, fields []bareField) {
		seen := map[string]string{}
		for _, f := range fields {
			f.mark = "  "
			f.value = strings.Repeat("v", 40)
			if f.ftype == fieldToggle || f.ftype == fieldCycle {
				f.value = "audio_only"
			}
			row := ansi.Strip(m.bareFieldRows(f, false, 13)[0])
			label, _, _ := strings.Cut(strings.TrimSpace(row), "  ")
			if other, dup := seen[label]; dup {
				t.Errorf("%s: %q and %q are both drawn %q", page, other, f.label, label)
			}
			seen[label] = f.label
		}
	}
	for _, sec := range sections {
		var fields []bareField
		for _, fd := range sec.fields {
			fields = append(fields, bareField{label: fd.label, ftype: fd.ftype, options: fd.options})
		}
		check(sec.name, fields)
	}
	var fields []bareField
	for _, fd := range channelFields {
		fields = append(fields, bareField{label: bareChannelLabel(fd.label), ftype: fd.ftype, options: fd.options})
	}
	check("channel editor", fields)
}

// Ctrl+O on the FFmpeg path is refused below the normal layout (owner
// ruling R-C10), before the save prompt it would raise: nothing is saved,
// Settings stays open and the refusal is gone on the next key.
//
// Mutant: the refusal after handleKey — with an edit pending the prompt is
// up, and Y saves and closes onto an installer that cannot be drawn.
func TestBareSettingsRefusesTheInstaller(t *testing.T) {
	r := bareSettingsRig(t, 30, 6)
	a, m := r.app, r.app.settings
	toSection(t, m, "Paths")
	toField(t, m, "ffmpeg_path")
	r.press("x") // an unsaved edit
	edited := m.values["ffmpeg_path"]
	r.press("ctrl+o")
	if !m.IsVisible() || m.closeConfirm || m.afterClose != "" || a.ffmpegCheck.IsVisible() || r.saves != 0 {
		t.Fatalf("Ctrl+O at 30x6: open %v, prompt %v, after %q, installer %v, saves %d", m.IsVisible(), m.closeConfirm, m.afterClose, a.ffmpegCheck.IsVisible(), r.saves)
	}
	if !m.dirty || m.values["ffmpeg_path"] != edited {
		t.Errorf("Ctrl+O changed the field: %q", m.values["ffmpeg_path"])
	}
	if frame := plainFrame(a); !strings.Contains(frame, bareInstallerRefusal) {
		t.Errorf("the refusal is not on screen:\n%s", frame)
	}
	r.press("up")
	if frame := plainFrame(a); strings.Contains(frame, bareInstallerRefusal) {
		t.Errorf("the refusal outlived the next key:\n%s", frame)
	}

	// At the normal size the key still opens the installer.
	n := bareSettingsRig(t, 60, 20)
	toSection(t, n.app.settings, "Paths")
	toField(t, n.app.settings, "ffmpeg_path")
	n.press("ctrl+o")
	if !n.app.ffmpegCheck.IsVisible() {
		t.Error("Ctrl+O at 60x20 no longer opens the installer")
	}
}

// The buttons are not drawn below the normal layout, so nothing reaches
// them: Shift+Down does nothing on a field page, and focus left on a button
// by the normal form is dropped before a key can press it. In the channel
// list the real Shift+Down and Shift+Up keys still reorder.
//
// Mutant: bareKey passing Shift+Down through — the text input blurs and the
// next Enter closes Settings.
func TestBareSettingsHasNoButtons(t *testing.T) {
	shift := func(code rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: code, Mod: tea.ModShift} }

	r := bareSettingsRig(t, 30, 6)
	a, m := r.app, r.app.settings
	a.Update(shift(tea.KeyDown))
	if m.buttonFocus != -1 || !m.textInput.Focused() {
		t.Errorf("Shift+Down at 30x6 focused a button nobody can see (focus %d, input focused %v)", m.buttonFocus, m.textInput.Focused())
	}
	r.press("enter", "1")
	if m.buttonFocus != -1 || !m.IsVisible() || m.values["port"] != "7741" {
		t.Errorf("after Shift+Down, Enter, 1: button focus %d, open %v, port %q", m.buttonFocus, m.IsVisible(), m.values["port"])
	}

	n := bareSettingsRig(t, 60, 20)
	a, m = n.app, n.app.settings
	a.Update(shift(tea.KeyDown))
	if m.buttonFocus != 0 {
		t.Fatalf("premise lost: Shift+Down at 60x20 did not focus a button")
	}
	a.Update(tea.WindowSizeMsg{Width: 30, Height: 6})
	n.press("enter", "1")
	if m.buttonFocus != -1 || !m.IsVisible() || m.values["port"] != "7741" {
		t.Errorf("button focus carried into the bare form: focus %d, open %v, port %q", m.buttonFocus, m.IsVisible(), m.values["port"])
	}

	toSection(t, r.app.settings, "Channels")
	m = r.app.settings
	first, second := m.channels[0].ID, m.channels[1].ID
	r.app.Update(shift(tea.KeyDown))
	if m.channels[0].ID != second || m.channels[1].ID != first || m.channelIndex != 1 {
		t.Errorf("Shift+Down at 30x6 did not move the channel down")
	}
	if rows := plainRows(r.app.View().Content); !strings.HasPrefix(rows[2], "> YT Some Channel") {
		t.Errorf("the moved channel is not the marked second row:\n%s", strings.Join(rows, "\n"))
	}
	r.app.Update(shift(tea.KeyUp))
	if m.channels[0].ID != first || m.channelIndex != 0 {
		t.Errorf("Shift+Up at 30x6 did not move the channel back")
	}
}

// Unsaved edits are as safe as at the normal size: Esc asks, on a row that
// is whole; Esc again goes back to the edit; N discards; Y saves. After a
// save that failed Esc still asks. A save that needs a restart shows the
// restart prompt, and Enter restarts.
func TestBareSettingsEscStillAsks(t *testing.T) {
	last := func(a *App) string {
		rows := plainRows(a.View().Content)
		return rows[len(rows)-1]
	}
	r := bareSettingsRig(t, 30, 6)
	a, m := r.app, r.app.settings
	toField(t, m, "https_enabled")
	r.press("right", "esc")
	if !m.closeConfirm || !m.IsVisible() || last(a) != bareSavePrompt {
		t.Fatalf("Esc with an edit: prompt %v, open %v, last row %q", m.closeConfirm, m.IsVisible(), last(a))
	}
	r.press("x") // not an answer
	if !m.closeConfirm {
		t.Error("a key that is no answer dismissed the prompt")
	}
	r.press("esc")
	if m.closeConfirm || !m.IsVisible() || !m.dirty {
		t.Error("Esc on the prompt did not go back to the edit")
	}
	r.press("esc", "n")
	if m.IsVisible() || r.saves != 0 {
		t.Errorf("N: open %v, saves %d", m.IsVisible(), r.saves)
	}

	// A save that fails: the error is on screen and Esc asks again.
	r = bareSettingsRig(t, 30, 6)
	a, m = r.app, r.app.settings
	r.saveErr = errors.New("disk full")
	toSection(t, m, "Downloader")
	r.press("right", "left", "down")
	for m.dirty == false {
		r.press("right", "down")
	}
	r.press("esc", "y")
	if !m.IsVisible() || m.status != saveError || r.saves != 1 {
		t.Fatalf("a failed save: open %v, status %d, saves %d", m.IsVisible(), m.status, r.saves)
	}
	if frame := plainFrame(a); !strings.Contains(frame, "Save failed: disk full") {
		t.Errorf("the save error is not on screen:\n%s", frame)
	}
	r.press("esc")
	if !m.IsVisible() || !m.closeConfirm || last(a) != bareSavePrompt {
		t.Errorf("Esc after a failed save: open %v, prompt %v, last row %q", m.IsVisible(), m.closeConfirm, last(a))
	}

	// A restart-required save: the prompt, then Enter restarts.
	r = bareSettingsRig(t, 30, 6)
	a, m = r.app, r.app.settings
	r.press("1", "esc", "y")
	if !m.showRestartOverlay || r.saves != 1 {
		t.Fatalf("saving the port: restart prompt %v, saves %d", m.showRestartOverlay, r.saves)
	}
	if rows := plainRows(a.View().Content); rows[0] != "Restart needed." || rows[4] != "Enter: restart now" {
		t.Errorf("the restart prompt:\n%s", strings.Join(rows, "\n"))
	}
	r.pressRunLast("enter")
	if r.restarted != 1 {
		t.Errorf("Enter on the restart prompt restarted %d times", r.restarted)
	}
}

// A message never covers the focused input, at any compact size: the
// password editor's (which does not go away on the next key), a save error
// on a field page and a validation error in the two editors. One that fits
// the rows the page can give is whole.
//
// Mutant: errorUnder given a body without the focused rows — at 30x6 the
// long refusal takes the input's row.
func TestBareSettingsMessagesNeverCoverTheInput(t *testing.T) {
	// A message is read without its spaces: a word longer than a row (a
	// path) is broken where the row ends.
	words := func(s string) string { return strings.Join(strings.Fields(s), "") }
	compactSizes(func(w, h int) {
		at := func(state string) string { return fmt.Sprintf("%s at %dx%d", state, w, h) }
		build := func(state string) (*compactRig, []string) {
			r := bareSettingsRig(t, w, h)
			bareSettingsStates[state](t, r)
			return r, plainRows(r.app.View().Content)
		}
		has := func(rows []string, prefix string) bool {
			return slices.ContainsFunc(rows, func(row string) bool { return strings.HasPrefix(row, prefix) })
		}

		_, rows := build("password, a long refusal")
		for _, label := range []string{"> Current", "  New", "  Confirm"} {
			if !has(rows, label) {
				t.Errorf("%s: the %q row is covered:\n%s", at("password, a long refusal"), label, strings.Join(rows, "\n"))
			}
		}
		_, rows = build("password, remove")
		if !has(rows, "> Current") || !strings.Contains(words(strings.Join(rows, " ")), words("Network access will be reset to localhost.")) {
			t.Errorf("%s: the input or the warning is cut:\n%s", at("password, remove"), strings.Join(rows, "\n"))
		}

		_, rows = build("save error")
		if !has(rows, "*>Port") || !has(rows, "  7741") {
			t.Errorf("%s: the focused field is covered:\n%s", at("save error"), strings.Join(rows, "\n"))
		}
		if w*(h-3) >= 2*len(bareLongError) && !strings.Contains(words(strings.Join(rows, " ")), words(bareLongError)) {
			t.Errorf("%s: the error had room and is cut:\n%s", at("save error"), strings.Join(rows, "\n"))
		}

		r, rows := build("channel editor, an error")
		if !has(rows, "> Archive slots") || !has(rows, "  999") {
			t.Errorf("%s: the focused field is covered:\n%s", at("channel editor, an error"), strings.Join(rows, "\n"))
		}
		if msg := r.app.settings.errorMsg; !strings.Contains(words(strings.Join(rows, " ")), words(msg)) {
			t.Errorf("%s: %q is cut:\n%s", at("channel editor, an error"), msg, strings.Join(rows, "\n"))
		}

		_, rows = build("webhook editor, an error")
		if !has(rows, "> URL") || !has(rows, "  nope") {
			t.Errorf("%s: the focused field is covered:\n%s", at("webhook editor, an error"), strings.Join(rows, "\n"))
		}

		// The test result never covers the row it was sent from.
		_, rows = build("integrations, a test failed")
		if !has(rows, "> …/5555555555555555555") || !strings.Contains(words(strings.Join(rows, " ")), "UnknownWebhook") {
			t.Errorf("%s: the cursor row or the result is cut:\n%s", at("integrations, a test failed"), strings.Join(rows, "\n"))
		}
	})
}

// The removal prompt's counts are never cut (P deletes that many pending
// jobs once the removal is saved), whatever the size and however large:
// the rows wrap, and the count's own row gives way to the keys.
//
// Mutant: the Enter and P rows cut to the width — at 30 columns a five-digit
// pending count loses its last digits.
func TestBareRemovalPromptCountsAreWhole(t *testing.T) {
	compactSizes(func(w, h int) {
		r := bareSettingsRig(t, w, h)
		bareSettingsStates["removal, everything"](t, r)
		text := strings.Join(strings.Fields(strings.Join(plainRows(r.app.View().Content), " ")), " ")
		for _, want := range []string{"Enter: remove, keep 123456 jobs", "P: remove + delete 12345 pending", "Esc: cancel", `Remove "`} {
			if !strings.Contains(text, want) {
				t.Errorf("%dx%d: the prompt lacks %q:\n%s", w, h, want, plainFrame(r.app))
			}
		}
		// D confirms as it does at the normal size, keeping the jobs.
		n := len(r.app.settings.channels)
		r.press("d")
		if len(r.app.settings.channels) != n-1 || len(r.app.settings.channelPrunes) != 0 {
			t.Errorf("%dx%d: D on the prompt did not remove the channel and keep its jobs", w, h)
		}
	})
}

// No secret is drawn (owner ruling): the Integrations list shows a Discord
// webhook's id and no character of its token, at any size, and no more of
// any other URL than the normal list's 50 cells; a typed password is never
// on screen.
//
// Mutant: bareWebhookLabel returning the URL's tail.
func TestBareSettingsDrawsNoSecret(t *testing.T) {
	compactSizes(func(w, h int) {
		r := bareSettingsRig(t, w, h)
		bareSettingsStates["integrations"](t, r)
		for range 3 {
			frame := plainFrame(r.app)
			if strings.Contains(frame, bareHookToken[:4]) || strings.Contains(frame, "tail-PAST") {
				t.Fatalf("%dx%d: the list draws a token:\n%s", w, h, frame)
			}
			if !strings.Contains(frame, "…/"+bareHookID) && h > 6 {
				t.Errorf("%dx%d: the list does not show the webhook's id:\n%s", w, h, frame)
			}
			r.press("down")
		}

		for _, state := range []string{"password, set", "password, change", "password, remove"} {
			r := bareSettingsRig(t, w, h)
			bareSettingsStates[state](t, r)
			for range 3 {
				if frame := plainFrame(r.app); strings.Contains(frame, "hunt") || strings.Contains(frame, "old") || strings.Contains(frame, "wrong") {
					t.Fatalf("%dx%d: %s draws what was typed:\n%s", w, h, state, frame)
				}
				r.press("down")
			}
		}
	})
	if got := bareWebhookLabel("https://discord.com/api/webhooks/"+bareHookID+"/"+bareHookToken, 200); got != "…/"+bareHookID {
		t.Errorf("bareWebhookLabel = %q", got)
	}
	if got := bareWebhookLabel(bareOtherHook, 200); ansi.StringWidth(got) > 50 {
		t.Errorf("bareWebhookLabel shows %d cells of a URL, the normal list at most 50", ansi.StringWidth(got))
	}
}

// An event row of the webhook editor fits the compact floor whole: the
// longest event id leaves it one cell to spare. A longer id needs a wider
// floor or a shorter name.
func TestBareEventRowsFitTheFloor(t *testing.T) {
	for _, event := range allNotifEvents {
		if row := "> [x] @ " + event; ansi.StringWidth(row) > compactMinWidth {
			t.Errorf("event %q makes a row of %d cells, the compact floor is %d", event, ansi.StringWidth(row), compactMinWidth)
		}
	}
	r := bareSettingsRig(t, 30, 6)
	bareSettingsStates["webhook editor, the last event"](t, r)
	last := allNotifEvents[len(allNotifEvents)-1]
	rows := plainRows(r.app.View().Content)
	if !slices.ContainsFunc(rows, func(row string) bool { return strings.HasPrefix(row, "> [") && strings.HasSuffix(row, " "+last) }) {
		t.Errorf("the last event's row is not on screen whole:\n%s", strings.Join(rows, "\n"))
	}
}

// The password editor's result is on the Network page after it closes, and
// gone on the next key; the page says a password is set.
func TestBarePasswordResultIsShown(t *testing.T) {
	r := bareSettingsRig(t, 30, 6)
	bareSettingsCompactOnly["password set, its result"](t, r)
	frame := plainFrame(r.app)
	if !strings.Contains(frame, "Password set successfully") {
		t.Errorf("the result is not on screen:\n%s", frame)
	}
	r.press("up")
	frame = plainFrame(r.app)
	if strings.Contains(frame, "Password set successfully") || !strings.Contains(frame, "pw:set") {
		t.Errorf("after the next key:\n%s", frame)
	}
}

// The wheel moves the cursor of the bare Settings one row and never
// reaches the task list underneath; a click does nothing.
func TestBareSettingsMouse(t *testing.T) {
	r := bareSettingsRig(t, 40, 10)
	a, m := r.app, r.app.settings
	cursor := a.taskList.list.Index()
	a.Update(tea.MouseWheelMsg{X: 5, Y: 3, Button: tea.MouseWheelDown})
	a.Update(tea.MouseWheelMsg{X: 5, Y: 3, Button: tea.MouseWheelDown})
	a.Update(tea.MouseWheelMsg{X: 5, Y: 3, Button: tea.MouseWheelUp})
	if m.fieldIndex != 1 {
		t.Errorf("the wheel left the cursor on field %d, want 1", m.fieldIndex)
	}
	before := a.View().Content
	for y := range 10 {
		a.Update(tea.MouseClickMsg{X: 3, Y: y, Button: tea.MouseLeft})
	}
	if a.View().Content != before || !m.IsVisible() || a.taskList.list.Index() != cursor {
		t.Error("a click changed the bare Settings or reached the task list")
	}
	toSection(t, m, "Integrations")
	a.Update(tea.MouseWheelMsg{X: 5, Y: 3, Button: tea.MouseWheelDown})
	if m.notifIndex != 1 {
		t.Errorf("the wheel left the webhook cursor on %d, want 1", m.notifIndex)
	}
}

// A paste reaches the focused input of the bare Settings: a webhook URL is
// pasted, not typed.
func TestBareSettingsTakesAPaste(t *testing.T) {
	r := bareSettingsRig(t, 30, 6)
	toSection(t, r.app.settings, "Integrations")
	r.press("a")
	r.app.Update(tea.PasteMsg{Content: "https://discord.com/api/webhooks/1/abc"})
	if got := r.app.settings.notifEditURL; got != "https://discord.com/api/webhooks/1/abc" {
		t.Errorf("the pasted URL is %q", got)
	}
	wantExact(t, "the editor after a paste", r.app.View().Content, 30, 6)
}

// The save prompt Ctrl+O raised at the normal size, answered after the
// terminal shrank: the save happens, and the installer, which cannot be
// drawn there, is refused by its size and not opened under a notice.
func TestInstallerPromptAnsweredAfterAShrink(t *testing.T) {
	r := bareSettingsRig(t, 60, 20)
	a, m := r.app, r.app.settings
	toSection(t, m, "Paths")
	toField(t, m, "ffmpeg_path")
	r.press("x", "ctrl+o")
	if !m.closeConfirm || m.afterClose != "open_ffmpeg" {
		t.Fatalf("premise lost: Ctrl+O with an edit did not ask first")
	}
	a.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	r.press("n")
	if m.IsVisible() || a.ffmpegCheck.IsVisible() || a.hasActiveOverlay() {
		t.Errorf("after N at 40x10: settings %v, installer %v", m.IsVisible(), a.ffmpegCheck.IsVisible())
	}
	if got, want := a.feedback.msg, compactRefusal(60, 20); got != want {
		t.Errorf("feedback %q, want %q", got, want)
	}
}
