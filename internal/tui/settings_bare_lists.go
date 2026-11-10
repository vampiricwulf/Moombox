package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The Channels and Integrations pages of the bare Settings
// (settings_bare.go): the two lists, the channel-removal prompt and the two
// editors.

// ---- Channels ----

// bareChannels draws the channel list: the platform, the name and
// "(disabled)", which is never cut; the name gives way to it. The ID and
// the filter are left to the editor.
func (m *SettingsModel) bareChannels() string {
	w := m.width
	msg, style := m.bareMessage()
	if len(m.channels) == 0 {
		sheet := m.bareShell("A:add", "Tab:next", m.bareEscKey())
		items := [][]string{{DimStyle.Render("No channels. A adds one.")}}
		return m.barePage(sheet, items, -1, nil, msg, style)
	}
	sheet := m.bareShell("A:add", "Ent:edit", "D:del", "Tab:next", "Sh+↑↓:move", m.bareEscKey())

	items := make([][]string, len(m.channels))
	for i, ch := range m.channels {
		selected := i == m.channelIndex
		mark, nameColor := "  ", ColorWhite
		if selected {
			mark, nameColor = lipgloss.NewStyle().Foreground(ColorCyan).Render("> "), ColorCyan
		}
		platform, platformColor := "YT", ColorRed
		if ch.GetPlatform() == "twitch" {
			platform, platformColor = "TW", ColorCookies
		}
		name := ch.Name
		if name == "" {
			name = ch.ID
		}
		nameStyle := lipgloss.NewStyle().Foreground(nameColor)
		tag := ""
		if !ch.IsEnabled() {
			tag = " (disabled)"
			nameStyle = nameStyle.Faint(true)
		}
		items[i] = []string{mark + lipgloss.NewStyle().Foreground(platformColor).Render(platform) + " " +
			nameStyle.Render(truncateString(name, max(w-5-len(tag), 1))) + YellowFaintStyle.Render(tag)}
	}
	// Where the list is longer than the screen, its last row says how far
	// down the cursor is.
	var tail []string
	if rows := m.height - 2; len(m.channels) > rows && rows >= 5 {
		tail = []string{DimStyle.Render(fmt.Sprintf("  %d of %d", m.channelIndex+1, len(m.channels)))}
	}
	return m.barePage(sheet, items, m.channelIndex, tail, msg, style)
}

// bareChannelRemoval draws the removal prompt: the whole screen, one fact a
// row. Only the channel's name is ever shortened. The counts wrap and are
// never cut: P deletes the pending jobs once the removal is saved. The two
// lines about what is kept either way are shown where they fit whole.
func (m *SettingsModel) bareChannelRemoval() string {
	w, h := m.width, m.height
	const head, tail = `Remove "`, `"?`
	title := head + truncateString(m.removalName, max(w-len(head)-len(tail), 1)) + tail

	wrap := func(s string) []string { return bareWrap(s, w, len(s)+1) }
	var count, info, keys []string
	keep := "Enter: remove, keep its jobs"
	del := ""
	switch {
	case m.removalLoading:
		count = []string{"Counting its jobs…"}
	case m.removalInfo == nil:
		count = append([]string{"Jobs not counted; all kept:"}, bareStyled(bareWrap(m.removalErr, w, 2), DimStyle)...)
	default:
		in := m.removalInfo
		if in.Total == 0 {
			count = []string{"No job will be deleted."}
			keep = "Enter: remove"
		} else {
			count = wrap("It has " + countOf(in.Total, "job", "jobs") + ".")
			keep = "Enter: remove, keep " + countOf(in.Total, "job", "jobs")
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
			del = fmt.Sprintf("P: remove + delete %d pending", in.Pending)
		}
	}
	keys = wrap(keep)
	if del != "" {
		keys = append(keys, wrap(del)...)
	}
	keys = append(keys, "Esc: cancel")

	// The Enter row carries the total: where the rows do not all fit, the
	// count's own row gives way to the keys.
	if m.removalInfo != nil && m.removalInfo.Total > 0 && 1+len(count)+len(keys) > h {
		count = nil
	}
	body := count
	for _, line := range info {
		rows := wrap(line)
		if 1+len(body)+len(rows)+len(keys) > h {
			break
		}
		body = append(body, bareStyled(rows, DimStyle)...)
	}
	body = append(body, keys...)
	return bareSheet{left: YellowStyle.Render(title), body: body}.render(w, h)
}

// bareChannelLabel is a channel field's label without its platform suffix:
// the editor lists only the fields of the channel's platform.
func bareChannelLabel(label string) string {
	label = strings.TrimSuffix(label, " (YouTube only)")
	return strings.TrimSuffix(label, " (Twitch only)")
}

// bareChannelEdit draws the channel editor: every field of the platform in
// a window that follows the focus, the focused field's explanation, the
// keys. While a handle or URL is being resolved the title says so.
func (m *SettingsModel) bareChannelEdit() string {
	w, h := m.width, m.height
	fields := m.visibleChannelFields()
	title := "Edit channel"
	if m.channelIndex >= len(m.channels) {
		title = "Add channel"
	}
	if m.channelResolving {
		title = "Resolving channel…"
	}
	sheet := bareSheet{
		left: TitleStyle.Render(title),
		keys: bareKeyRow(w, "Enter:save", "Esc:cancel", "←→:change"),
	}
	if len(fields) == 0 {
		return sheet.render(w, h)
	}
	focus := min(max(m.channelEditField, 0), len(fields)-1)
	sheet.right = DimStyle.Render(fmt.Sprintf("%d/%d", focus+1, len(fields)))

	longest := 0
	helps := make([]string, len(fields))
	for i, field := range fields {
		longest = max(longest, ansi.StringWidth(bareChannelLabel(field.label)))
		helps[i] = field.help
	}
	col := m.bareLabelCol(longest)
	items := make([][]string, len(fields))
	for i, field := range fields {
		val := m.channelEditValues[field.key]
		if val == "" && (field.ftype == fieldToggle || field.ftype == fieldCycle) && len(field.options) > 0 {
			val = field.options[0]
		}
		mark := "  "
		if i == focus {
			mark = lipgloss.NewStyle().Foreground(ColorCyan).Render("> ")
		}
		items[i] = m.bareFieldRows(bareField{
			label: bareChannelLabel(field.label), ftype: field.ftype, options: field.options, value: val, mark: mark,
		}, i == focus, col)
	}

	helpRows := bareHelpRows(helps, w, max(h-2-3, 1))
	help := make([]string, helpRows)
	if text := fields[focus].help; text != "" {
		help = bareRows(bareStyled(bareWrap(text, w, helpRows), DimStyle), helpRows)
	}
	msg, style := m.bareEditorMessage()
	return m.barePage(sheet, items, focus, help, msg, style)
}

// ---- Integrations ----

// bareWebhookLabel is what the bare Integrations list shows of a webhook URL
// in room cells. A Discord-style URL (…/webhooks/<id>/<token>) is its id,
// which tells two webhooks of one channel apart; the token after it is never
// shown. Any other URL is cut from its end as the normal list cuts it, to at
// most the 50 cells that list shows.
func bareWebhookLabel(url string, room int) string {
	const marker = "/webhooks/"
	if i := strings.Index(url, marker); i >= 0 {
		id, _, _ := strings.Cut(url[i+len(marker):], "/")
		if id != "" && strings.Trim(id, "0123456789") == "" {
			return truncateString("…/"+id, room)
		}
	}
	return truncateString(url, min(50, room))
}

// bareNotifications draws the webhook list. The row above the keys is the
// test notification's progress and result, which wraps upward over the list
// but never over the row it was sent from. The armed delete takes the keys
// row, so a result that lands meanwhile does not replace it.
func (m *SettingsModel) bareNotifications() string {
	w := m.width
	msg, style := m.bareMessage()
	if len(m.notifications) == 0 {
		sheet := m.bareShell("A:add", "Tab:next", m.bareEscKey())
		items := [][]string{{DimStyle.Render("No webhooks. A adds one.")}}
		return m.barePage(sheet, items, -1, nil, msg, style)
	}
	sheet := m.bareShell("A:add", "Ent:edit", "D:del", "T:test", "Tab:next", m.bareEscKey())
	if m.notifDeleteConf && !m.closeConfirm {
		sheet.keys = YellowStyle.Render("Press D again to delete")
	}

	items := make([][]string, len(m.notifications))
	for i, n := range m.notifications {
		mark, nameStyle := "  ", lipgloss.NewStyle()
		if i == m.notifIndex {
			mark = lipgloss.NewStyle().Foreground(ColorCyan).Render("> ")
			nameStyle = lipgloss.NewStyle().Foreground(ColorCyan)
		}
		muted := ""
		if !n.IsEnabled() {
			muted = " Muted"
		}
		items[i] = []string{mark + nameStyle.Render(bareWebhookLabel(n.URL, max(w-2-len(muted), 1))) + YellowStyle.Render(muted)}
	}
	// The result row is kept while there is no result, so the list does not
	// move when one lands.
	var tail []string
	if msg == "" {
		tail = []string{""}
	}
	return m.barePage(sheet, items, m.notifIndex, tail, msg, style)
}

// bareNotifEdit draws the webhook editor: the four head rows, then the
// events under their group names, in a window that follows the focus. An
// event row is its tick, its mention flag and its name: "@" pings on that
// event (yellow once there is somebody to ping), "·" does not.
func (m *SettingsModel) bareNotifEdit() string {
	w, h := m.width, m.height
	const labelW = 10
	total := notifEditEventBase + len(allNotifEvents)
	title := "Edit webhook"
	if m.notifIndex >= len(m.notifications) {
		title = "Add webhook"
	}
	keys := bareKeyRow(w, "Spc", "m:@", "Enter:save", "Esc:back")
	if w >= 41 { // the four in words are 41 cells
		keys = bareKeyRow(w, "Space:event", "m:mention", "Enter:save", "Esc:back")
	}
	sheet := bareSheet{
		left:  TitleStyle.Render(title),
		right: DimStyle.Render(fmt.Sprintf("%d/%d", min(m.notifEditFocus, total-1)+1, total)),
		keys:  keys,
	}

	cursor := func(focused bool) (mark string, label lipgloss.Style) {
		if focused {
			return lipgloss.NewStyle().Foreground(ColorCyan).Render("> "), lipgloss.NewStyle().Foreground(ColorCyan).Bold(true)
		}
		return "  ", DimStyle
	}
	textRow := func(row int, label, value string) []string {
		mark, style := cursor(m.notifEditFocus == row)
		if m.notifEditFocus == row {
			return []string{mark + style.Render(label), "  " + bareInput(m.textInput, w-3)}
		}
		return []string{mark + style.Render(padRight(label, labelW)) + truncateString(value, max(w-2-labelW, 1))}
	}
	checkRow := func(row int, label string, on bool, word string) []string {
		mark, style := cursor(m.notifEditFocus == row)
		box, color := "[ ] ", ColorGray
		if on {
			box, color = "[x] ", ColorGreen
		}
		return []string{mark + style.Render(padRight(label, labelW)) +
			lipgloss.NewStyle().Foreground(color).Render(truncateString(box+word, max(w-2-labelW, 1)))}
	}

	enabledWord := "muted"
	if m.notifEditEnabled {
		enabledWord = "delivering"
	}
	// The delivery words in full where they fit beside the label.
	deliveryWord := map[bool]string{false: "separate", true: "one per job"}
	if w >= 36 {
		deliveryWord = map[bool]string{false: "separate messages", true: "one message per job"}
	}
	onePerJob := m.notifEditDelivery == "edit"

	items := [][]string{
		textRow(notifEditURLRow, "URL", m.notifEditURL),
		checkRow(notifEditEnabledRow, "Enabled", m.notifEditEnabled, enabledWord),
		textRow(notifEditMentionRow, "Mention", m.notifEditMention),
		checkRow(notifEditDeliveryRow, "Delivery", onePerJob, deliveryWord[onePerJob]),
	}
	focus := min(m.notifEditFocus, notifEditEventBase-1)
	flat := 0
	for _, group := range notifEventGroups {
		items = append(items, []string{DimStyle.Render(truncateString("  "+group.name, w))})
		for _, event := range group.events {
			focused := m.notifEditFocus == flat+notifEditEventBase
			if focused {
				focus = len(items)
			}
			mark, nameStyle := "  ", lipgloss.NewStyle().Foreground(ColorWhite)
			if focused {
				mark = lipgloss.NewStyle().Foreground(ColorCyan).Render("> ")
				nameStyle = lipgloss.NewStyle().Foreground(ColorCyan)
			}
			box, boxColor := "[ ]", ColorGray
			if m.notifEditEvents[event] {
				box, boxColor = "[x]", ColorGreen
			}
			flag, flagColor := "·", ColorGray
			if m.notifEditMentionEvents[event] {
				flag = "@"
				if strings.TrimSpace(m.notifEditMention) != "" {
					flagColor = ColorYellow
				}
			}
			items = append(items, []string{mark + lipgloss.NewStyle().Foreground(boxColor).Render(box) + " " +
				lipgloss.NewStyle().Foreground(flagColor).Render(flag) + " " +
				nameStyle.Render(truncateString(event, max(w-8, 1)))})
			flat++
		}
	}

	// The mention formats, while the Mention row is the one being typed in.
	var tail []string
	if m.notifEditFocus == notifEditMentionRow && h >= 6 {
		tail = []string{DimStyle.Render(truncateString("<@&ROLE_ID>, <@USER_ID>, @here, @everyone", w))}
	}
	msg, style := m.bareEditorMessage()
	return m.barePage(sheet, items, focus, tail, msg, style)
}
