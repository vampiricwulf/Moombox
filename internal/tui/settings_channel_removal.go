package tui

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// Removing a channel in Settings → Channels asks what to do with its jobs
// (W25-09, owner decision), as the dashboard's Remove does: D counts the
// channel's jobs (OnChannelRemovalSummary, off the update loop) and the
// prompt that replaces the key list offers Enter — remove the channel and
// keep its jobs, the default — P — remove it and delete its pending jobs,
// offered only when there are any — and Esc. "Pending" is the Queued,
// Upcoming and COOKIES? rows whose staging is empty; a parked recording with
// footage is kept either way and named, and active downloads are not
// touched. The removal itself waits for the overlay's save, like every other
// change made here, and so does the delete: a save that leaves the channel
// out of the config hands its ID to the App (TakeChannelPrunes), which runs
// OnDeletePendingChannelJobs. Discarding the changes discards the choice:
// the next Open forgets it.

// ChannelRemovalInfo is what removing a channel would do to its jobs — the
// TUI's copy of worker.ChannelRemoval, adapted in cmd/moombox.
type ChannelRemovalInfo struct {
	Total   int      // every job of the channel (worker.ChannelRemoval.Total)
	Pending int      // what "delete its pending jobs" deletes
	Footage []string // titles of the parked recordings with footage, kept either way
	Active  int      // downloads in progress, which nothing touches
}

// channelRemovalSummaryMsg carries OnChannelRemovalSummary's answer for ID.
type channelRemovalSummaryMsg struct {
	ID   string
	Info ChannelRemovalInfo
	Err  error
}

// channelJobsPrunedMsg reports the pending-jobs deletes a save handed over.
type channelJobsPrunedMsg struct {
	Deleted, FootageKept int
	Failed               []string
}

// channelRemovalState is the prompt's state. channelDeleteConf says whether
// the prompt is up; this says what it shows.
type channelRemovalState struct {
	removalID       string
	removalPlatform string
	removalName     string
	removalLoading  bool
	removalInfo     *ChannelRemovalInfo
	removalErr      string
	// channelPrunes holds the IDs removed with "delete its pending jobs",
	// waiting for the save; readyPrunes the ones a save handed over.
	channelPrunes map[string]bool
	readyPrunes   []string
}

// beginChannelRemoval opens the prompt for the selected channel and asks the
// App to count its jobs.
func (m *SettingsModel) beginChannelRemoval() string {
	ch := m.channels[m.channelIndex]
	m.channelDeleteConf = true
	m.removalID = ch.ID
	m.removalPlatform = ch.GetPlatform()
	m.removalName = cmp.Or(ch.Name, ch.ID)
	m.removalLoading = true
	m.removalInfo = nil
	m.removalErr = ""
	return "channel_removal_summary"
}

// ChannelRemovalID is the channel the prompt is counting, and its platform,
// which decides what ties a job to it: a Twitch channel's jobs carry no
// channel ID.
func (m *SettingsModel) ChannelRemovalID() (id, platform string) {
	return m.removalID, m.removalPlatform
}

// HandleChannelRemovalSummary lands the count. A count for a prompt that has
// since closed, or for another channel, is dropped.
func (m *SettingsModel) HandleChannelRemovalSummary(id string, info ChannelRemovalInfo, err error) {
	if !m.channelDeleteConf || id != m.removalID {
		return
	}
	m.removalLoading = false
	if err != nil {
		m.removalErr = err.Error()
		return
	}
	m.removalInfo = &info
}

// handleChannelRemovalKey answers the prompt. Enter (and K, and D — the old
// "press D again") removes the channel and keeps its jobs; P removes it and
// deletes its pending jobs once saved, when the count says there are any;
// any other key cancels.
func (m *SettingsModel) handleChannelRemovalKey(key string) string {
	switch key {
	case keyEnter, "k", "K", "d", "D":
		m.removeSelectedChannel(false)
	case "p", "P":
		if m.removalInfo == nil || m.removalInfo.Pending == 0 {
			return "" // nothing to delete, or not counted: the prompt stays
		}
		m.removeSelectedChannel(true)
	}
	m.channelDeleteConf = false
	return ""
}

func (m *SettingsModel) removeSelectedChannel(deletePending bool) {
	if m.channelIndex >= len(m.channels) || m.channels[m.channelIndex].ID != m.removalID {
		return
	}
	m.channels = slices.Delete(m.channels, m.channelIndex, m.channelIndex+1)
	if m.channelIndex >= len(m.channels) && m.channelIndex > 0 {
		m.channelIndex--
	}
	m.dirty = true
	m.structDirty = true
	if deletePending {
		if m.channelPrunes == nil {
			m.channelPrunes = map[string]bool{}
		}
		m.channelPrunes[m.removalID] = true
	} else {
		delete(m.channelPrunes, m.removalID)
	}
}

// handOverChannelPrunes runs after a save succeeded: every channel removed
// with "delete its pending jobs" that the saved config no longer holds is
// handed to the App. One added back before the save keeps its jobs.
func (m *SettingsModel) handOverChannelPrunes() {
	if len(m.channelPrunes) == 0 {
		return
	}
	configured := map[string]bool{}
	read := func(c *config.MoomboxConfig) {
		for _, ch := range c.Channels {
			configured[ch.ID] = true
		}
	}
	if m.configStore != nil {
		m.configStore.Read(read)
	} else if m.cfg != nil {
		read(m.cfg)
	}
	for id := range m.channelPrunes {
		if !configured[id] {
			m.readyPrunes = append(m.readyPrunes, id)
		}
	}
	slices.Sort(m.readyPrunes)
	m.channelPrunes = nil
}

// TakeChannelPrunes returns, once, the channel IDs whose pending jobs a save
// has just committed to deleting.
func (m *SettingsModel) TakeChannelPrunes() []string {
	ids := m.readyPrunes
	m.readyPrunes = nil
	return ids
}

// resetChannelRemoval forgets the prompt and every choice not yet saved.
func (m *SettingsModel) resetChannelRemoval() {
	m.channelDeleteConf = false
	m.channelRemovalState = channelRemovalState{}
}

// channelRemovalPromptUp says whether the removal prompt is on screen: it
// owns every key while it is (handleChannelKey).
func (m *SettingsModel) channelRemovalPromptUp() bool {
	return m.channelDeleteConf && m.channelMode != "edit" && sections[m.sectionIndex].name == "Channels"
}

// channelRemovalHint is the overlay footer while the prompt is up: the
// prompt's own keys. The list's "Enter: Edit  D: Delete" stayed there, and
// Enter is the prompt's confirm — the key the footer called Edit removed the
// channel — while the footer's "Esc: Close" cancelled the prompt (View
// swaps that for "Esc: Cancel").
func (m *SettingsModel) channelRemovalHint() string {
	hint := "Enter: Remove, keep jobs"
	if in := m.removalInfo; in != nil && in.Total == 0 {
		hint = "Enter: Remove"
	}
	if in := m.removalInfo; in != nil && in.Pending > 0 {
		hint += "  P: Remove, delete pending"
	}
	return hint
}

// channelRemovalPromptLines renders the prompt in place of the key list, one
// line each, cut to w: the question, what is kept regardless, then the keys.
// When maxLines is short, the two informational lines go first.
func (m *SettingsModel) channelRemovalPromptLines(w, maxLines int) []string {
	head := fmt.Sprintf("Remove %q?", m.removalName)
	var info []string
	keep := "Enter: Remove, keep its jobs"
	var del string
	switch {
	case m.removalLoading:
		head += " Counting its jobs…"
	case m.removalInfo == nil:
		head += " Its jobs could not be counted (" + m.removalErr + "); all will be kept."
	default:
		in := m.removalInfo
		if in.Total == 0 {
			// No proof it has none (a YouTube row older than jobs.channel_id,
			// or one added by hand, is never counted), only that nothing
			// will be deleted.
			head += " No job will be deleted."
			keep = "Enter: Remove"
		} else {
			head += " It has " + countOf(in.Total, "job", "jobs") + "."
			keep = "Enter: Remove, keep its " + countOf(in.Total, "job", "jobs")
		}
		if len(in.Footage) > 0 {
			line := countOf(len(in.Footage), "parked recording", "parked recordings") +
				" with footage will be kept: " + fmt.Sprintf("%q", in.Footage[0])
			if len(in.Footage) > 1 {
				line += fmt.Sprintf(" and %d more", len(in.Footage)-1)
			}
			info = append(info, line)
		}
		if in.Active > 0 {
			verb := "are"
			if in.Active == 1 {
				verb = "is"
			}
			info = append(info, countOf(in.Active, "download", "downloads")+" in progress "+verb+" not affected.")
		}
		if in.Pending > 0 {
			del = "P: Remove, delete its " + countOf(in.Pending, "pending job", "pending jobs")
		}
	}

	lines := []string{YellowStyle.Render(truncateWidth(head, w, "…"))}
	keys := []string{keep}
	if del != "" {
		keys = append(keys, del)
	}
	keys = append(keys, "Esc: Cancel")
	for len(info) > 0 && 1+len(info)+len(keys) > maxLines {
		info = info[:len(info)-1]
	}
	for _, l := range info {
		lines = append(lines, DimStyle.Render(truncateWidth(l, w, "…")))
	}
	for _, l := range keys {
		lines = append(lines, truncateWidth(l, w, "…"))
	}
	return lines
}

// channelRemovalSummaryCmd counts a channel's jobs off the update loop.
// Without the callback the prompt is answered at once as uncounted: keep
// and Esc only.
func (a *App) channelRemovalSummaryCmd(id, platform string) tea.Cmd {
	count := a.OnChannelRemovalSummary
	if count == nil {
		a.settings.HandleChannelRemovalSummary(id, ChannelRemovalInfo{}, fmt.Errorf("job counts unavailable"))
		return nil
	}
	return safeCmd(func() tea.Msg {
		info, err := count(id, platform)
		return channelRemovalSummaryMsg{ID: id, Info: info, Err: err}
	})
}

// channelPruneCmd deletes the pending jobs of the channels a save just
// removed with that choice, off the update loop. Nil when there are none.
func (a *App) channelPruneCmd() tea.Cmd {
	ids := a.settings.TakeChannelPrunes()
	if len(ids) == 0 || a.OnDeletePendingChannelJobs == nil {
		return nil
	}
	del := a.OnDeletePendingChannelJobs
	return safeCmd(func() tea.Msg {
		var res channelJobsPrunedMsg
		for _, id := range ids {
			n, kept, err := del(id)
			if err != nil {
				res.Failed = append(res.Failed, id)
				continue
			}
			res.Deleted += n
			res.FootageKept += kept
		}
		return res
	})
}

// channelJobsPrunedFeedback is the feedback line for a channelJobsPrunedMsg.
func channelJobsPrunedFeedback(msg channelJobsPrunedMsg) (string, feedbackSeverity) {
	line := "Deleted " + countOf(msg.Deleted, "pending job", "pending jobs") + " of the removed channel"
	if msg.FootageKept > 0 {
		line += "; kept " + countOf(msg.FootageKept, "parked recording", "parked recordings") + " with footage"
	}
	if len(msg.Failed) > 0 {
		return line + "; could not delete the pending jobs of " + strings.Join(msg.Failed, ", ") +
			" — they are kept", severityError
	}
	return line, severitySuccess
}

// countOf is "1 job" / "2 jobs".
func countOf(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
