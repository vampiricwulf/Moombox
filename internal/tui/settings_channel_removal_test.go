package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// The Settings → Channels removal prompt (W25-09, owner decision): D counts
// the channel's jobs and asks — Enter removes it and keeps them, P removes it
// and deletes its pending ones once the overlay is saved, Esc cancels.

type removalHarness struct {
	a         *App
	cfg       *config.MoomboxConfig
	deletes   []string
	counted   []string
	platforms []string // the platform each count was asked for
}

func newRemovalHarness(t *testing.T, info ChannelRemovalInfo) *removalHarness {
	t.Helper()
	cfg := config.Defaults()
	cfg.Channels = []config.ChannelConfig{{ID: "UCalpha", Name: "Alpha"}, {ID: "UCbeta", Name: "Beta"}}
	h := &removalHarness{a: NewApp(), cfg: cfg}
	h.a.SetConfig(cfg)
	h.a.SetConfigStore(config.NewStore(cfg, ""))
	h.a.OnChannelRemovalSummary = func(id, platform string) (ChannelRemovalInfo, error) {
		h.counted = append(h.counted, id)
		h.platforms = append(h.platforms, platform)
		return info, nil
	}
	h.a.OnDeletePendingChannelJobs = func(id string) (int, int, error) {
		h.deletes = append(h.deletes, id)
		return info.Pending, len(info.Footage), nil
	}
	h.a.settings.OnSave = func(*config.MoomboxConfig) error { return nil }
	h.a.settings.SetSize(120, 40)
	h.a.settings.Open(cfg)
	h.a.settings.switchSection(slices.IndexFunc(sections, func(s settingsSection) bool { return s.name == "Channels" }))
	return h
}

// press sends one key through the App and runs whatever command comes back,
// feeding its message to Update the way the runtime would.
func (h *removalHarness) press(t *testing.T, key string) {
	t.Helper()
	_, cmd := h.a.handleKey(keyMsg(key))
	h.run(cmd)
}

func (h *removalHarness) run(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			h.run(c)
		}
		return
	}
	if msg != nil {
		_, next := h.a.Update(msg)
		h.run(next)
	}
}

func (h *removalHarness) channelIDs() []string {
	var ids []string
	for _, ch := range h.a.settings.channels {
		ids = append(ids, ch.ID)
	}
	return ids
}

var removalInfo = ChannelRemovalInfo{Total: 4, Pending: 2, Footage: []string{"Parked live"}, Active: 1}

// TestChannelRemovalPromptCountsAndOffersBothChoices: D asks the backend for
// the selected channel's counts, and the prompt shows them — the parked
// recording kept either way, by title, and the downloads left alone — with
// both choices and Esc.
//
// Mutants killed: D removing at once again (no prompt); the footage line
// dropped; the P line offered without its count.
func TestChannelRemovalPromptCountsAndOffersBothChoices(t *testing.T) {
	h := newRemovalHarness(t, removalInfo)
	h.press(t, "d")
	if !slices.Equal(h.counted, []string{"UCalpha"}) {
		t.Fatalf("counted %v, want the selected channel", h.counted)
	}
	view := stripANSI(h.a.settings.View())
	for _, want := range []string{
		`Remove "Alpha"? It has 4 jobs.`,
		`1 parked recording with footage will be kept: "Parked live"`,
		"1 download in progress is not affected.",
		"Enter: Remove, keep its 4 jobs",
		"P: Remove, delete its 2 pending jobs",
		"Esc: Cancel",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("the prompt lacks %q:\n%s", want, view)
		}
	}
	if !slices.Equal(h.channelIDs(), []string{"UCalpha", "UCbeta"}) {
		t.Errorf("channels = %v before any answer, want both", h.channelIDs())
	}
}

// TestChannelRemovalCountsByTheChannelsPlatform: D asks for the selected
// channel's counts with its platform — a Twitch channel's jobs carry no
// channel ID, and counted as a YouTube channel's they were none.
//
// Mutant killed: beginChannelRemoval not recording the platform (the count
// is asked for "").
func TestChannelRemovalCountsByTheChannelsPlatform(t *testing.T) {
	h := newRemovalHarness(t, removalInfo)
	h.a.settings.channels = append(h.a.settings.channels,
		config.ChannelConfig{ID: "somestreamer", Name: "Some Streamer", Platform: "twitch"})
	h.press(t, "d") // Alpha, YouTube by default
	h.press(t, "esc")
	h.a.settings.channelIndex = 2
	h.press(t, "d")
	if !slices.Equal(h.counted, []string{"UCalpha", "somestreamer"}) || !slices.Equal(h.platforms, []string{"youtube", "twitch"}) {
		t.Errorf("counted %v on %v, want UCalpha on youtube and somestreamer on twitch", h.counted, h.platforms)
	}
}

// TestChannelRemovalZeroCountClaimsNoAbsence: a zero count is no proof the
// channel has no jobs — a YouTube row from before jobs carried their
// channel's ID, or one added by hand, is never counted — so the prompt says
// what it can promise, that none will be deleted, and offers a bare Remove.
//
// Mutant killed: the old " It has no jobs." line.
func TestChannelRemovalZeroCountClaimsNoAbsence(t *testing.T) {
	h := newRemovalHarness(t, ChannelRemovalInfo{})
	h.press(t, "d")
	view := stripANSI(h.a.settings.View())
	if !strings.Contains(view, `Remove "Alpha"? No job will be deleted.`) || !strings.Contains(view, "Enter: Remove") ||
		strings.Contains(view, "no jobs") {
		t.Errorf("zero-count prompt:\n%s", view)
	}
}

// TestChannelRemovalFooterFollowsThePrompt: while the prompt is up the
// overlay's footer names the prompt's keys. It kept the list's "Enter: Edit
// D: Delete" beside "Esc: Close", and Enter is the prompt's confirm — the key
// the footer called Edit removed the channel — while Esc cancelled the
// prompt rather than closing anything. Cancelled, the list's footer is back.
//
// Mutants killed: renderHintText without its prompt branch ("Enter: Edit"
// stays); View's Esc label not swapped ("Esc: Close" stays); the P entry
// offered with nothing pending.
func TestChannelRemovalFooterFollowsThePrompt(t *testing.T) {
	footer := func(h *removalHarness) string {
		lines := strings.Split(stripANSI(h.a.settings.View()), "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			if strings.Contains(lines[i], "Esc: ") {
				return lines[i]
			}
		}
		return ""
	}
	h := newRemovalHarness(t, removalInfo)
	h.press(t, "d")
	got := footer(h)
	for _, want := range []string{"Esc: Cancel", "Enter: Remove, keep jobs", "P: Remove, delete pending"} {
		if !strings.Contains(got, want) {
			t.Errorf("footer with the prompt up lacks %q: %q", want, got)
		}
	}
	for _, stale := range []string{"Esc: Close", "Enter: Edit", "D: Delete"} {
		if strings.Contains(got, stale) {
			t.Errorf("footer with the prompt up still offers %q: %q", stale, got)
		}
	}
	h.press(t, "esc")
	if got := footer(h); !strings.Contains(got, "Esc: Close") || !strings.Contains(got, "Enter: Edit") {
		t.Errorf("footer after the prompt was cancelled: %q", got)
	}

	h = newRemovalHarness(t, ChannelRemovalInfo{Total: 3})
	h.press(t, "d")
	if got := footer(h); strings.Contains(got, "P: ") {
		t.Errorf("footer offers a delete with nothing pending: %q", got)
	}
}

// TestChannelRemovalDeletePendingWaitsForTheSave: P removes the channel from
// the list at once but deletes nothing until the overlay is saved; the save
// hands the channel over and the App runs the delete, then says so.
//
// Mutants killed: removeSelectedChannel not recording the choice; the save
// not handing it over (handOverChannelPrunes); the settings key path not
// running channelPruneCmd.
func TestChannelRemovalDeletePendingWaitsForTheSave(t *testing.T) {
	h := newRemovalHarness(t, removalInfo)
	h.press(t, "d")
	h.press(t, "p")
	if !slices.Equal(h.channelIDs(), []string{"UCbeta"}) {
		t.Fatalf("channels = %v after P, want Alpha gone", h.channelIDs())
	}
	if len(h.deletes) != 0 {
		t.Fatalf("pending jobs deleted before the save: %v", h.deletes)
	}
	h.press(t, "esc") // dirty: "Save changes?"
	h.press(t, "y")
	if !slices.Equal(h.deletes, []string{"UCalpha"}) {
		t.Errorf("deletes after the save = %v, want [UCalpha]", h.deletes)
	}
	if !strings.Contains(h.a.feedback.msg, "Deleted 2 pending jobs of the removed channel; kept 1 parked recording with footage") {
		t.Errorf("feedback = %q", h.a.feedback.msg)
	}
}

// TestChannelRemovalDefaultKeepsTheJobs: Enter — and D again, the old
// "press D again" — remove the channel and keep its jobs.
//
// Mutant killed: the keep keys recording a delete.
func TestChannelRemovalDefaultKeepsTheJobs(t *testing.T) {
	for _, key := range []string{"enter", "d"} {
		h := newRemovalHarness(t, removalInfo)
		h.press(t, "d")
		h.press(t, key)
		if !slices.Equal(h.channelIDs(), []string{"UCbeta"}) {
			t.Fatalf("%s: channels = %v, want Alpha removed", key, h.channelIDs())
		}
		h.press(t, "esc")
		h.press(t, "y")
		if len(h.deletes) != 0 {
			t.Errorf("%s: the keep choice deleted %v", key, h.deletes)
		}
	}
}

// TestChannelRemovalOffersNoDeleteWithNothingPending: with nothing pending
// the prompt has no P line, and P does nothing — the prompt stays up and
// the channel stays.
//
// Mutant killed: P accepted whatever the count.
func TestChannelRemovalOffersNoDeleteWithNothingPending(t *testing.T) {
	h := newRemovalHarness(t, ChannelRemovalInfo{Total: 3})
	h.press(t, "d")
	if strings.Contains(stripANSI(h.a.settings.View()), "P: Remove") {
		t.Error("the prompt offers a delete with nothing pending")
	}
	h.press(t, "p")
	if !h.a.settings.channelDeleteConf || len(h.channelIDs()) != 2 {
		t.Errorf("P with nothing pending: prompt up = %v, channels = %v; want the prompt kept and both channels",
			h.a.settings.channelDeleteConf, h.channelIDs())
	}
}

// TestChannelRemovalUncountedOffersKeepOnly: a count that fails leaves keep
// and Esc, and says why.
func TestChannelRemovalUncountedOffersKeepOnly(t *testing.T) {
	h := newRemovalHarness(t, removalInfo)
	h.a.OnChannelRemovalSummary = func(string, string) (ChannelRemovalInfo, error) {
		return ChannelRemovalInfo{}, errors.New("db locked")
	}
	h.press(t, "d")
	view := stripANSI(h.a.settings.View())
	if !strings.Contains(view, "could not be counted (db locked); all will be kept") || strings.Contains(view, "P: Remove") {
		t.Errorf("uncounted prompt:\n%s", view)
	}
}

// TestChannelRemovalReAddedBeforeTheSaveKeepsItsJobs: a channel removed
// with P and added back before the save is still configured once saved, so
// nothing is deleted.
//
// Mutant killed: handOverChannelPrunes handing over every recorded channel
// without asking the saved config.
func TestChannelRemovalReAddedBeforeTheSaveKeepsItsJobs(t *testing.T) {
	h := newRemovalHarness(t, removalInfo)
	h.press(t, "d")
	h.press(t, "p")
	h.a.settings.channels = append(h.a.settings.channels, config.ChannelConfig{ID: "UCalpha", Name: "Alpha"})
	h.press(t, "esc")
	h.press(t, "y")
	if len(h.deletes) != 0 {
		t.Errorf("a channel added back before the save had its pending jobs deleted: %v", h.deletes)
	}
}

// TestChannelRemovalDiscardedChoiceIsForgotten: discarding the overlay's
// changes discards the choice. Here the channel is then removed elsewhere —
// the dashboard's Remove, keeping its jobs — and the next TUI save, of
// anything, must not delete what the discarded P asked for.
//
// Mutant killed: Open not resetting the removal state.
func TestChannelRemovalDiscardedChoiceIsForgotten(t *testing.T) {
	h := newRemovalHarness(t, removalInfo)
	h.press(t, "d")
	h.press(t, "p")
	h.press(t, "esc")
	h.press(t, "n") // discard
	if err := h.a.configStore.Update(func(c *config.MoomboxConfig) {
		c.Channels = []config.ChannelConfig{{ID: "UCbeta", Name: "Beta"}}
	}); err != nil {
		t.Fatal(err)
	}
	h.a.settings.Open(h.cfg)
	h.a.settings.dirty = true
	h.a.settings.structDirty = true
	h.a.settings.saveAndClose()
	h.run(h.a.channelPruneCmd())
	if len(h.deletes) != 0 {
		t.Errorf("a discarded removal deleted %v on a later save", h.deletes)
	}
}

// TestChannelRemovalStaleCountIsDropped: a count that lands after its
// prompt closed, or for another channel's prompt, changes nothing.
//
// Mutant killed: HandleChannelRemovalSummary without its ID check.
func TestChannelRemovalStaleCountIsDropped(t *testing.T) {
	m := newSettingsModelForSave(t)
	m.channels = []config.ChannelConfig{{ID: "UCalpha"}, {ID: "UCbeta"}}
	m.channelIndex = 1
	m.beginChannelRemoval()
	m.HandleChannelRemovalSummary("UCalpha", removalInfo, nil)
	if m.removalInfo != nil || !m.removalLoading {
		t.Errorf("UCalpha's count landed on UCbeta's prompt: %+v", m.removalInfo)
	}
}

// TestChannelRemovalClickCancelsWithoutSelecting: the prompt pushes the list
// down several rows, so a click while it is up selects nothing — it only
// cancels.
//
// Mutant killed: the click mapped to a row as if the prompt were one line.
func TestChannelRemovalClickCancelsWithoutSelecting(t *testing.T) {
	h := newRemovalHarness(t, removalInfo)
	h.press(t, "d")
	h.a.settings.handleMouseChannelClick(2)
	if h.a.settings.channelDeleteConf || h.a.settings.channelIndex != 0 {
		t.Errorf("after a click: prompt up = %v, selection = %d; want it cancelled and Alpha still selected",
			h.a.settings.channelDeleteConf, h.a.settings.channelIndex)
	}
}

// TestChannelRemovalPromptFitsTheScreen: at the supported floor sizes the
// prompt and a full list stay inside the frame — the prompt takes its rows
// from the list's window.
//
// Mutants killed: the list window not shrunk by the prompt's extra rows;
// the informational lines never dropped for space.
func TestChannelRemovalPromptFitsTheScreen(t *testing.T) {
	// 60x13 is below the floor the other fit tests hold: there the prompt
	// drops its two informational lines to stay inside the frame.
	for _, size := range [][2]int{{60, 13}, {60, 20}, {80, 24}} {
		m := newSettingsModelForSave(t)
		m.SetSize(size[0], size[1])
		m.switchSection(slices.IndexFunc(sections, func(s settingsSection) bool { return s.name == "Channels" }))
		for i := range 30 {
			m.channels = append(m.channels, config.ChannelConfig{ID: "UC" + strings.Repeat("x", i), Name: "Channel"})
		}
		m.beginChannelRemoval()
		m.HandleChannelRemovalSummary(m.removalID, removalInfo, nil)
		view := m.View()
		if got := lipgloss.Height(view); got > size[1] {
			t.Errorf("%dx%d: frame is %d rows with the prompt up", size[0], size[1], got)
		}
		// The box clips what overflows from the bottom: the prompt's keys
		// and the action button under the list are what would go.
		if plain := stripANSI(view); !strings.Contains(plain, "Esc: Cancel") || !strings.Contains(plain, "[ Return ]") {
			t.Errorf("%dx%d: the prompt's keys or the action button are cut off:\n%s", size[0], size[1], plain)
		}
	}
}

// TestChannelRemovalMouseSaveRunsTheDelete: [ Save & Return ] is a save too,
// and the pending-jobs delete it commits runs from the mouse path.
//
// Mutant killed: handleMouse not running channelPruneCmd.
func TestChannelRemovalMouseSaveRunsTheDelete(t *testing.T) {
	h := newRemovalHarness(t, removalInfo)
	h.press(t, "d")
	h.press(t, "p")
	view := stripANSI(h.a.settings.View())
	x, y := -1, -1
	for row, line := range strings.Split(view, "\n") {
		if i := strings.Index(line, "[ Save & Return ]"); i >= 0 {
			x, y = len([]rune(line[:i]))+2, row
			break
		}
	}
	if y < 0 {
		t.Fatalf("no Save & Return button in the view:\n%s", view)
	}
	_, cmd := h.a.handleMouse(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	h.run(cmd)
	if !slices.Equal(h.deletes, []string{"UCalpha"}) {
		t.Errorf("deletes after the mouse save = %v, want [UCalpha]", h.deletes)
	}
}

// TestChannelJobsPrunedFeedback: a delete that failed says which channel
// kept its jobs, in red.
func TestChannelJobsPrunedFeedback(t *testing.T) {
	line, sev := channelJobsPrunedFeedback(channelJobsPrunedMsg{Deleted: 1, Failed: []string{"UCgone"}})
	if sev != severityError || !strings.Contains(line, "could not delete the pending jobs of UCgone — they are kept") {
		t.Errorf("feedback = %q (%v)", line, sev)
	}
}
