package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

const (
	tuiHandleID = "UChandlehandlehandlehand"
	tuiPlainID  = "UCabcdefghijklmnopqrstuv"
)

// stubTUIHandleLookup answers "@SomeHandle" without youtube.com and leaves
// every other input to the real normaliser (the /channel/ URL resolves
// locally; a watch URL is refused).
func stubTUIHandleLookup(t *testing.T) {
	t.Helper()
	prev := normalizeChannelID
	normalizeChannelID = func(ctx context.Context, input string) (*utils.ResolvedChannel, error) {
		if strings.TrimSpace(input) == "@SomeHandle" {
			return &utils.ResolvedChannel{ID: tuiHandleID, Name: "Some Handle", Platform: "youtube"}, nil
		}
		return utils.NormalizeChannelID(ctx, input)
	}
	t.Cleanup(func() { normalizeChannelID = prev })
}

// resolveFor runs resolveChannelCmd for input and returns its answer.
func resolveFor(t *testing.T, input string) channelResolvedMsg {
	t.Helper()
	msg, ok := (&App{}).resolveChannelCmd(input)().(channelResolvedMsg)
	if !ok {
		t.Fatalf("resolveChannelCmd(%q) did not answer a channelResolvedMsg", input)
	}
	return msg
}

// settingsAdding opens the Settings channel editor on a new channel over
// chs, with the ID box holding id.
func settingsAdding(chs []config.ChannelConfig, id string) *SettingsModel {
	m := NewSettingsModel()
	m.channels = chs
	m.handleChannelKey("a")
	m.channelEditValues["id"] = id
	return m
}

// enterAndResolve presses Enter in the Settings channel editor and, when it
// asks for a resolve, runs it and hands the answer back as App.Update does.
func enterAndResolve(t *testing.T, m *SettingsModel) {
	t.Helper()
	if m.handleChannelKey(keyEnter) != "resolve_channel" {
		return
	}
	r := resolveFor(t, m.GetChannelResolveInput())
	m.HandleChannelResolved(r.Input, r.ID, r.Name, r.Platform, r.Err)
}

// TestSettingsChannelEditorResolvesBareHandle pins W25-12 in the TUI: a
// bare @handle — the field's help says "ID, @handle, or URL" — is resolved
// before it is saved, where Enter used to save "@SomeHandle" as the ID.
//
// Mutants killed: the Enter gate back on the youtube.com/youtu.be/twitch.tv
// test (saved verbatim); the resolved ID not written into the values.
func TestSettingsChannelEditorResolvesBareHandle(t *testing.T) {
	stubTUIHandleLookup(t)
	m := settingsAdding(nil, "@SomeHandle")
	if act := m.handleChannelKey(keyEnter); act != "resolve_channel" {
		t.Fatalf("Enter on a bare @handle: action %q, want resolve_channel (channels %+v)", act, m.channels)
	}
	r := resolveFor(t, m.GetChannelResolveInput())
	m.HandleChannelResolved(r.Input, r.ID, r.Name, r.Platform, r.Err)
	if len(m.channels) != 1 || m.channels[0].ID != tuiHandleID || m.channels[0].Name != "Some Handle" {
		t.Errorf("saved %+v, want the handle's UC ID named Some Handle", m.channels)
	}
}

// TestSettingsChannelEditorRefusesNonChannelURL pins W25-14 in the TUI
// Settings editor: a URL that names no channel is refused with "not a
// YouTube or Twitch channel URL" and the editor stays open. resolveChannelCmd
// used to answer an unresolvable URL with the URL itself as the ID, and the
// editor saved it.
//
// Mutants killed: HandleChannelResolved saving through an error;
// resolveChannelCmd answering an error with the input as the ID.
func TestSettingsChannelEditorRefusesNonChannelURL(t *testing.T) {
	m := settingsAdding(nil, "https://www.youtube.com/watch?v=dQw4w9WgXcQ")
	enterAndResolve(t, m)
	if len(m.channels) != 0 {
		t.Fatalf("an unresolvable URL was saved as a channel: %+v", m.channels)
	}
	if m.errorMsg != "Channel ID: not a YouTube or Twitch channel URL" || m.status != saveError {
		t.Errorf("errorMsg %q status %v, want the not-a-channel refusal", m.errorMsg, m.status)
	}
	if m.channelMode != "edit" || m.channelResolving {
		t.Errorf("mode %q resolving %v: the editor should stay open and idle", m.channelMode, m.channelResolving)
	}
}

// TestSettingsChannelEditorRefusesDuplicateID pins W25-15: the Settings
// editor refuses an ID another entry has — compared case-insensitively, on
// add and on edit, and after a URL resolves to it — as the setup wizard
// always did. It used to save the second entry, which the dashboard's Remove
// then deleted only half of and its reorder refused forever. Editing a
// channel and keeping its own ID is not a duplicate.
//
// Mutants killed: dropping the plain-ID check; dropping the check after
// resolution; channelIDTaken comparing case-sensitively; channelIDTaken not
// skipping the entry being edited.
func TestSettingsChannelEditorRefusesDuplicateID(t *testing.T) {
	existing := []config.ChannelConfig{{ID: tuiPlainID, Name: "A"}, {ID: "shroud", Platform: "twitch"}}

	m := settingsAdding(existing, strings.ToLower(tuiPlainID))
	enterAndResolve(t, m)
	if len(m.channels) != 2 || m.errorMsg != `Channel "ucabcdefghijklmnopqrstuv" already added` {
		t.Errorf("adding an existing ID in other case: channels %d, errorMsg %q", len(m.channels), m.errorMsg)
	}

	m = settingsAdding(existing, "youtube.com/channel/"+tuiPlainID)
	enterAndResolve(t, m)
	if len(m.channels) != 2 || m.errorMsg != `Channel "`+tuiPlainID+`" already added` {
		t.Errorf("a URL resolving to an existing ID: channels %d, errorMsg %q", len(m.channels), m.errorMsg)
	}
	if m.channelEditValues["id"] != tuiPlainID || m.channelMode != "edit" {
		t.Errorf("after the refusal the box holds %q in mode %q, want the resolved ID, still editing", m.channelEditValues["id"], m.channelMode)
	}

	// Edit "shroud" into A's ID: refused.
	m = NewSettingsModel()
	m.channels = append([]config.ChannelConfig(nil), existing...)
	m.channelIndex = 1
	m.handleChannelKey(keyEnter)
	m.channelEditValues["id"] = tuiPlainID
	m.handleChannelKey(keyEnter)
	if m.channels[1].ID != "shroud" || m.errorMsg == "" {
		t.Errorf("editing into another channel's ID: channels %+v, errorMsg %q", m.channels, m.errorMsg)
	}

	// Edit A keeping its own ID: saved.
	m = NewSettingsModel()
	m.channels = append([]config.ChannelConfig(nil), existing...)
	m.handleChannelKey(keyEnter)
	m.channelEditValues["name"] = "A renamed"
	m.handleChannelKey(keyEnter)
	if m.errorMsg != "" || m.channels[0].Name != "A renamed" || m.channelMode != "list" {
		t.Errorf("editing a channel under its own ID: errorMsg %q, channels %+v", m.errorMsg, m.channels)
	}
}

// TestSettingsChannelResolveDropsAStaleAnswer: an answer for text the ID box
// no longer holds — typed over while the lookup ran — is dropped, not saved
// over the new text, and Enter works again.
//
// Mutant killed: HandleChannelResolved without the input check.
func TestSettingsChannelResolveDropsAStaleAnswer(t *testing.T) {
	m := settingsAdding(nil, "youtube.com/channel/"+tuiPlainID)
	if m.handleChannelKey(keyEnter) != "resolve_channel" {
		t.Fatal("Enter on a URL did not resolve")
	}
	r := resolveFor(t, m.GetChannelResolveInput())
	m.channelEditValues["id"] = "shroud" // typed while the lookup ran
	m.HandleChannelResolved(r.Input, r.ID, r.Name, r.Platform, r.Err)
	if len(m.channels) != 0 || m.channelResolving {
		t.Fatalf("a stale answer was applied: channels %+v resolving %v", m.channels, m.channelResolving)
	}
	m.handleChannelKey(keyEnter)
	if len(m.channels) != 1 || m.channels[0].ID != "shroud" {
		t.Errorf("Enter after the stale answer: channels %+v, want shroud saved", m.channels)
	}
}

// wizardOnChannelEditor opens the setup wizard's simple-mode channel editor
// on a new channel over chs, with the ID box holding id.
func wizardOnChannelEditor(a *App, chs []config.ChannelConfig, id string) *SetupWizardModel {
	m := a.setupWiz
	m.Open()
	m.mode = setupModeSimple
	m.simpleStage = setupSimpleChannels
	m.channels = chs
	m.channelIndex = len(chs)
	m.handleChannelListKey("a", func() string { return "" }, func() string { return "" })
	m.channelEditValues["id"] = id
	return m
}

// runResolve presses Enter through App.handleKey — the path a keypress takes
// — and delivers the resolve command's answer through App.Update.
func runResolve(t *testing.T, a *App) {
	t.Helper()
	_, cmd := a.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Enter on the wizard's channel editor issued no resolve command")
	}
	var deliver func(tea.Msg)
	deliver = func(msg tea.Msg) {
		switch m := msg.(type) {
		case tea.BatchMsg:
			for _, c := range m {
				if c != nil {
					deliver(c())
				}
			}
		case channelResolvedMsg:
			a.Update(m)
		}
	}
	deliver(cmd())
}

// TestSetupWizardResolvesChannelIDs pins W25-14 in the TUI first-run wizard:
// it gained the Settings editor's resolve step. A pasted channel URL — the
// field's help invites one — was stored as the channel ID, and a bare
// @handle as typed; one that names no channel is refused, and a resolved ID
// the list has is refused as a typed one is.
//
// Mutants killed: the wizard's Enter saving without the resolve step (URL
// stored); App.handleKey not issuing the wizard's resolve_channel command;
// App.Update not handing channelResolvedMsg to the wizard; the wizard's
// HandleChannelResolved saving through an error; dropping its duplicate
// check after resolution.
func TestSetupWizardResolvesChannelIDs(t *testing.T) {
	stubTUIHandleLookup(t)
	a := NewApp()

	m := wizardOnChannelEditor(a, nil, "https://www.youtube.com/channel/"+tuiPlainID)
	runResolve(t, a)
	if len(m.channels) != 1 || m.channels[0].ID != tuiPlainID || m.channels[0].Platform != "youtube" {
		t.Fatalf("wizard stored %+v, want the URL's UC ID on youtube", m.channels)
	}

	m = wizardOnChannelEditor(a, m.channels, "@SomeHandle")
	runResolve(t, a)
	if len(m.channels) != 2 || m.channels[1].ID != tuiHandleID || m.channels[1].Name != "Some Handle" {
		t.Fatalf("wizard stored %+v, want the handle's UC ID second", m.channels)
	}

	chs := m.channels
	m = wizardOnChannelEditor(a, chs, "https://www.youtube.com/watch?v=dQw4w9WgXcQ")
	runResolve(t, a)
	if len(m.channels) != 2 || m.errorMsg != "Channel ID: not a YouTube or Twitch channel URL" || m.channelMode != "edit" {
		t.Errorf("a watch URL: channels %d, errorMsg %q, mode %q", len(m.channels), m.errorMsg, m.channelMode)
	}

	m = wizardOnChannelEditor(a, chs, "youtube.com/channel/"+tuiPlainID)
	runResolve(t, a)
	if len(m.channels) != 2 || m.errorMsg != `Channel "`+tuiPlainID+`" already added` {
		t.Errorf("a URL resolving to a listed ID: channels %d, errorMsg %q", len(m.channels), m.errorMsg)
	}
}

// TestChannelResolvedPanicAnswersTheEditor: a panic inside the resolve
// command answers the editor that is waiting, instead of the generic panic
// message that left it resolving for good.
//
// Mutant killed: resolveChannelCmd on safeCmd (the generic panic message).
func TestChannelResolvedPanicAnswersTheEditor(t *testing.T) {
	prev := normalizeChannelID
	normalizeChannelID = func(context.Context, string) (*utils.ResolvedChannel, error) { panic("boom") }
	t.Cleanup(func() { normalizeChannelID = prev })

	r := resolveFor(t, "@SomeHandle")
	if r.Input != "@SomeHandle" || r.Err == nil || errors.Is(r.Err, utils.ErrNotChannelURL) {
		t.Errorf("panic answer %+v, want an error for the input", r)
	}
}

// TestChannelEditorsResolveMixedCaseURLs: a channel link written with its
// host in mixed case, as bios write it ("Twitch.tv/shroud",
// "https://www.YouTube.com/channel/UC…"), is resolved by both TUI editors,
// and a URL on any other host is refused. The shared gate looked for
// "youtube.com/", "youtu.be/" or "twitch.tv/" as typed, so Enter saved such
// a URL as the channel ID with no resolve step.
//
// Mutant killed: utils.LooksLikeURL matching the input as typed (both
// editors save the URL verbatim).
func TestChannelEditorsResolveMixedCaseURLs(t *testing.T) {
	for in, want := range map[string]config.ChannelConfig{
		"https://www.Twitch.tv/shroud":                  {ID: "shroud", Platform: "twitch"},
		"Twitch.tv/shroud":                              {ID: "shroud", Platform: "twitch"},
		"https://www.YouTube.com/channel/" + tuiPlainID: {ID: tuiPlainID, Platform: "youtube"},
	} {
		m := settingsAdding(nil, in)
		if act := m.handleChannelKey(keyEnter); act != "resolve_channel" {
			t.Errorf("Settings: Enter on %q: action %q, want resolve_channel (channels %+v)", in, act, m.channels)
			continue
		}
		r := resolveFor(t, m.GetChannelResolveInput())
		m.HandleChannelResolved(r.Input, r.ID, r.Name, r.Platform, r.Err)
		if len(m.channels) != 1 || m.channels[0].ID != want.ID || m.channels[0].Platform != want.Platform {
			t.Errorf("Settings: %q saved %+v, want %s on %s", in, m.channels, want.ID, want.Platform)
		}

		a := NewApp()
		w := wizardOnChannelEditor(a, nil, in)
		runResolve(t, a)
		if len(w.channels) != 1 || w.channels[0].ID != want.ID || w.channels[0].Platform != want.Platform {
			t.Errorf("wizard: %q stored %+v, want %s on %s", in, w.channels, want.ID, want.Platform)
		}
	}

	m := settingsAdding(nil, "https://Example.com/shroud")
	enterAndResolve(t, m)
	if len(m.channels) != 0 || m.errorMsg != "Channel ID: not a YouTube or Twitch channel URL" {
		t.Errorf("a URL on another host: channels %+v, errorMsg %q, want refused", m.channels, m.errorMsg)
	}
}

// TestSettingsChannelPlatformAutoDetectReadsMixedCaseHost: typing a link into
// the Settings editor's ID box switches its platform whatever case the host
// is written in.
//
// Mutant killed: autoDetectPlatform comparing the ID as typed (the platform
// stays youtube for "Twitch.tv/shroud").
func TestSettingsChannelPlatformAutoDetectReadsMixedCaseHost(t *testing.T) {
	m := settingsAdding(nil, "Twitch.tv/shroud")
	m.autoDetectPlatform()
	if got := m.channelEditValues["platform"]; got != "twitch" {
		t.Errorf("platform after Twitch.tv/shroud: %q, want twitch", got)
	}
	m.channelEditValues["id"] = "https://www.YouTube.com/@SomeHandle"
	m.autoDetectPlatform()
	if got := m.channelEditValues["platform"]; got != "youtube" {
		t.Errorf("platform after a YouTube.com link: %q, want youtube", got)
	}
}
