package tui

import (
	"fmt"
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/mattn/go-runewidth"
)

// --- Rendering ---

// View renders the settings panel.
func (m *SettingsModel) View() string {
	if !m.visible {
		return ""
	}

	// Restart overlay
	if m.showRestartOverlay {
		return m.renderRestartOverlay()
	}

	boxW := min(max(m.width-4, 40), m.width)
	innerW := boxW - 4
	h := max(m.height-2, 10)

	var content strings.Builder

	// Header: "Settings — General | Downloader | ..."
	content.WriteString(m.renderHeader(innerW))
	content.WriteString("\n")

	// Divider
	content.WriteString(DimStyle.Render(strings.Repeat("\u2500", innerW)))
	content.WriteString("\n")

	sec := sections[m.sectionIndex]

	// Buttons row is always present — subtract 1 line from content area
	buttonLine := 1

	// Section content
	switch sec.name {
	case "Network":
		// Network fields + embedded security sub-editor
		if m.secMode != securityStatus {
			content.WriteString(m.renderSecurity(innerW))
		} else {
			// h-12: extra 4 lines vs default h-8 for the compact security sub-section below
			content.WriteString(m.renderFields(sec, innerW, h-12-buttonLine))
			content.WriteString("\n")
			content.WriteString(m.renderSecurityCompact(innerW))
		}
	case "Channels":
		content.WriteString(m.renderChannels(innerW, h-8-buttonLine))
	case "Integrations":
		content.WriteString(m.renderNotifications(innerW, h-8-buttonLine))
	default:
		content.WriteString(m.renderFields(sec, innerW, h-8-buttonLine))
	}

	// Action buttons (always visible)
	content.WriteString("\n")
	content.WriteString(m.renderActionButtons())

	// Status line
	content.WriteString("\n")
	if m.closeConfirm {
		content.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Render(
			"Save changes? [Y]es / [N]o / [Esc] Cancel"))
	} else {
		switch m.status {
		case saveSaved:
			content.WriteString(lipgloss.NewStyle().Foreground(ColorGreen).Render("Saved"))
		case saveError:
			content.WriteString(lipgloss.NewStyle().Foreground(ColorRed).Render(m.errorMsg))
		default:
			if m.dirty {
				content.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Faint(true).Render("Unsaved changes"))
			}
		}
	}

	// Hints
	content.WriteString("\n")
	hintLeft := DimStyle.Render("Esc: Close")
	hintRight := m.renderHintText()
	hintGap := innerW - runewidth.StringWidth("Esc: Close") - runewidth.StringWidth(hintRight)
	hintGap = max(hintGap, 1)
	content.WriteString(hintLeft + strings.Repeat(" ", hintGap) + DimStyle.Render(hintRight))

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorCyan).
		Width(boxW - 2).
		Height(h + 2).
		Render(content.String())

	return centerBox(box, m.width, m.height)
}

func (m *SettingsModel) renderHeader(w int) string {
	left := lipgloss.NewStyle().Foreground(ColorCyan).Bold(true).Render("Settings") +
		DimStyle.Render(" \u2500 ")

	var tabParts []string
	for i, sec := range sections {
		if i > 0 {
			tabParts = append(tabParts, DimStyle.Render(" \u2502 "))
		}
		if i == m.sectionIndex {
			tabParts = append(tabParts, lipgloss.NewStyle().Foreground(ColorCyan).Bold(true).Render(sec.name))
		} else {
			tabParts = append(tabParts, DimStyle.Render(sec.name))
		}
	}
	tabs := strings.Join(tabParts, "")

	right := DimStyle.Render(fmt.Sprintf("%d/%d", m.sectionIndex+1, len(sections)))

	// Build full header, truncate to available width
	header := left + tabs + " " + right
	return lipgloss.NewStyle().MaxWidth(w).Render(header)
}

func (m *SettingsModel) renderHintText() string {
	sec := sections[m.sectionIndex]
	if sec.name == "Network" && m.secMode != securityStatus {
		return "Esc: Back  \u2191/\u2193/Tab: Navigate  Enter: Save"
	}
	if m.isFieldSection() {
		// Button focus mode
		if m.buttonFocus >= 0 {
			return "\u2190/\u2192: Switch button  \u2191: Back to fields  Enter: Activate"
		}
		field := sec.fields[m.fieldIndex]
		hint := "Shift+\u2190/\u2192: Section  \u2191/\u2193: Navigate"
		if field.ftype == fieldToggle || field.ftype == fieldCycle {
			hint = "\u2190/\u2192: Toggle  " + hint
		}
		if sec.name == "Network" {
			if m.hasPassword() {
				hint += "  `: Change pw  ~: Remove pw"
			} else {
				hint += "  `: Set pw"
			}
		}
		if sec.name == "Paths" && field.key == "ffmpeg_path" {
			hint += "  I: Install FFmpeg"
		}
		return hint
	}
	return "Shift+\u2190/\u2192: Section  \u2191/\u2193: Navigate  A: Add  Enter: Edit  D: Delete"
}

// renderActionButtons renders action buttons at the bottom of the settings panel.
// Always shows "Return" button. Shows "Save & Return" only when dirty.
func (m *SettingsModel) renderActionButtons() string {
	focusedStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("0")).
		Background(ColorCyan).
		Bold(true)
	blurredStyle := lipgloss.NewStyle().
		Foreground(ColorGray)

	if m.dirty {
		saveLabel := "[ Save & Return ]"
		discardLabel := "[ Return Without Saving ]"

		var saveStr, discardStr string
		if m.buttonFocus == 0 {
			saveStr = focusedStyle.Render(saveLabel)
		} else {
			saveStr = blurredStyle.Render(saveLabel)
		}
		if m.buttonFocus == 1 {
			discardStr = focusedStyle.Render(discardLabel)
		} else {
			discardStr = blurredStyle.Render(discardLabel)
		}
		return saveStr + "  " + discardStr
	}

	// Not dirty — just show Return button
	returnLabel := "[ Return ]"
	if m.buttonFocus == 0 {
		return focusedStyle.Render(returnLabel)
	}
	return blurredStyle.Render(returnLabel)
}

func (m *SettingsModel) renderFields(sec settingsSection, w, maxH int) string {
	if sec.fields == nil {
		return DimStyle.Render("No settings in this section")
	}

	// Compute max label width for alignment
	maxLabel := 0
	for _, fd := range sec.fields {
		if len(fd.label) > maxLabel {
			maxLabel = len(fd.label)
		}
	}
	padWidth := maxLabel + 2

	var lines []string
	end := min(m.scrollOffset+maxH, len(sec.fields))

	for i := m.scrollOffset; i < end; i++ {
		fd := sec.fields[i]
		selected := i == m.fieldIndex
		isChanged := m.values[fd.key] != m.originalValues[fd.key]
		needsRestart := isChanged && restartRequiredKeys[fd.key]

		// Prefix
		prefix := "  "
		if selected {
			prefix = "> "
		}

		// Label
		labelStr := padRight(fd.label, padWidth)
		labelStyle := lipgloss.NewStyle().Foreground(ColorGray)
		if selected {
			labelStyle = lipgloss.NewStyle().Foreground(ColorCyan).Bold(true)
		}

		// Value
		var value string
		switch fd.ftype {
		case fieldToggle:
			value = renderToggle(m.values[fd.key])
		case fieldCycle:
			value = renderCycleOptions(fd.options, m.values[fd.key], selected)
		default:
			indicatorW := 0
			if isChanged || needsRestart {
				indicatorW = 2 // " *"
			}
			valueMaxW := max(w-len(prefix)-padWidth-indicatorW, 5)
			if selected {
				m.textInput.SetWidth(valueMaxW)
				value = m.textInput.View()
			} else {
				value = renderInactiveInput(m.values[fd.key], valueMaxW, ColorWhite)
			}
		}

		// Change indicator
		indicator := ""
		if needsRestart {
			indicator = lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Faint(true).Render(" *")
		} else if isChanged {
			indicator = lipgloss.NewStyle().Foreground(ColorGreen).Faint(true).Render(" *")
		}

		prefixStyle := lipgloss.NewStyle()
		if selected {
			prefixStyle = lipgloss.NewStyle().Foreground(ColorCyan)
		}

		line := prefixStyle.Render(prefix) + labelStyle.Render(labelStr) + value + indicator
		lines = append(lines, line)

		if fd.previewFn != nil {
			preview := fd.previewFn(m.values[fd.key])
			if preview != "" {
				lines = append(lines, "  "+DimStyle.Render(preview))
			}
		}
	}

	// Info area: divider + help text for focused field
	if m.fieldIndex < len(sec.fields) {
		fd := sec.fields[m.fieldIndex]
		isChanged := m.values[fd.key] != m.originalValues[fd.key]
		needsRestart := isChanged && restartRequiredKeys[fd.key]

		lines = append(lines, "")
		lines = append(lines, DimStyle.Render(strings.Repeat("\u2500", w-4)))

		var infoParts []string
		if fd.help != "" {
			infoParts = append(infoParts, fd.help)
		}
		if needsRestart {
			infoParts = append(infoParts, lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Render("[restart required]"))
		} else if isChanged {
			infoParts = append(infoParts, lipgloss.NewStyle().Foreground(ColorGreen).Render("[modified]"))
		}

		if len(infoParts) > 0 {
			lines = append(lines, DimStyle.Render(strings.Join(infoParts, "  ")))
		}
	}

	return strings.Join(lines, "\n")
}

func renderToggle(value string) string {
	if value == "Yes" {
		return lipgloss.NewStyle().Foreground(ColorGreen).Render("Yes") + DimStyle.Render(" / No")
	}
	return DimStyle.Render("Yes / ") + lipgloss.NewStyle().Foreground(ColorRed).Render("No")
}

func renderCycleOptions(options []string, selected string, focused bool) string {
	var parts []string
	for _, opt := range options {
		if strings.EqualFold(opt, selected) {
			color := ColorWhite
			if focused {
				color = ColorCyan
			}
			parts = append(parts, lipgloss.NewStyle().Foreground(color).Bold(true).Render("["+opt+"]"))
		} else {
			parts = append(parts, DimStyle.Render(opt))
		}
	}
	return strings.Join(parts, DimStyle.Render(" / "))
}

func (m *SettingsModel) renderChannels(w, maxH int) string {
	if m.channelMode == "edit" {
		return m.renderChannelEdit(w)
	}

	var lines []string

	// Action bar
	actionBar := DimStyle.Render("A: Add  Enter: Edit  D: Delete  ")
	if m.channelDeleteConf {
		actionBar += lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Render("Press D again to confirm delete")
	}
	lines = append(lines, actionBar)

	if len(m.channels) == 0 {
		lines = append(lines, "")
		lines = append(lines, DimStyle.Render("  No channels configured. Press A to add one."))
		return strings.Join(lines, "\n")
	}

	channelCount := 0
	for i, ch := range m.channels {
		selected := i == m.channelIndex

		prefix := "  "
		if selected {
			prefix = "> "
		}

		// Platform icon
		platformIcon := "YT"
		platformColor := ColorRed
		if ch.GetPlatform() == "twitch" {
			platformIcon = "TW"
			platformColor = ColorCookies
		}
		platStr := lipgloss.NewStyle().Foreground(platformColor).Render("[" + platformIcon + "]")

		// Name + ID
		name := ch.Name
		if name == "" {
			name = ch.ID
		}
		nameColor := ColorWhite
		if selected {
			nameColor = ColorCyan
		}
		enabled := ch.IsEnabled()
		nameStyle := lipgloss.NewStyle().Foreground(nameColor)
		if !enabled {
			nameStyle = nameStyle.Faint(true)
		}

		idStr := DimStyle.Render(truncateString(ch.ID, 24))

		line := prefix + platStr + " " + nameStyle.Render(truncateString(name, 20)) + " " + idStr
		if !enabled {
			line += lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Faint(true).Render(" (disabled)")
		}
		terms := ch.Terms.Simple
		if terms != "" {
			line += DimStyle.Render(" filter: " + truncateString(terms, 20))
		}

		if selected {
			line = lipgloss.NewStyle().Render(line)
		}

		lines = append(lines, line)
		channelCount++
		if channelCount >= maxH {
			break
		}
	}

	return strings.Join(lines, "\n")
}

func (m *SettingsModel) renderChannelEdit(w int) string {
	var lines []string

	title := "Edit Channel"
	if m.channelIndex >= len(m.channels) {
		title = "Add Channel"
	}
	hint := " (Enter: save, Esc: cancel)"
	if m.channelResolving {
		hint = " (resolving URL...)"
	}
	lines = append(lines, lipgloss.NewStyle().Foreground(ColorCyan).Bold(true).Render(title)+
		DimStyle.Render(hint))

	fields := m.visibleChannelFields()
	maxLabel := 0
	for _, f := range fields {
		if len(f.label) > maxLabel {
			maxLabel = len(f.label)
		}
	}
	padW := maxLabel + 2

	for idx, field := range fields {
		isFocused := idx == m.channelEditField
		val := m.channelEditValues[field.key]
		if val == "" && (field.ftype == fieldToggle || field.ftype == fieldCycle) && len(field.options) > 0 {
			val = field.options[0]
		}

		prefix := "  "
		if isFocused {
			prefix = "> "
		}
		labelStyle := lipgloss.NewStyle().Foreground(ColorGray)
		if isFocused {
			labelStyle = lipgloss.NewStyle().Foreground(ColorCyan).Bold(true)
		}
		prefixStyle := lipgloss.NewStyle()
		if isFocused {
			prefixStyle = lipgloss.NewStyle().Foreground(ColorCyan)
		}

		var value string
		switch field.ftype {
		case fieldToggle:
			value = renderToggle(val)
		case fieldCycle:
			value = renderCycleOptions(field.options, val, isFocused)
		default:
			valueMaxW := max(w-runewidth.StringWidth(prefix)-padW-2, 5)
			if isFocused {
				m.textInput.SetWidth(valueMaxW)
				value = m.textInput.View()
			} else {
				value = renderInactiveInput(val, valueMaxW, ColorWhite)
			}
		}

		line := prefixStyle.Render(prefix) + labelStyle.Render(padRight(field.label, padW)) + value
		if field.help != "" && isFocused {
			line += DimStyle.Render(" (" + field.help + ")")
		}
		lines = append(lines, line)
	}

	return strings.Join(lines, "\n")
}

func (m *SettingsModel) renderNotifications(w, maxH int) string {
	if m.notifMode == "edit" {
		return m.renderNotifEdit(w)
	}

	var lines []string

	actionBar := DimStyle.Render("A: Add  Enter: Edit  D: Delete  ")
	if m.notifDeleteConf {
		actionBar += lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Render("Press D again to confirm delete")
	}
	lines = append(lines, actionBar)

	if len(m.notifications) == 0 {
		lines = append(lines, "")
		lines = append(lines, DimStyle.Render("  No webhooks configured. Press A to add one."))
		return strings.Join(lines, "\n")
	}

	for i, n := range m.notifications {
		selected := i == m.notifIndex
		prefix := "  "
		if selected {
			prefix = "> "
		}

		urlDisplay := truncateString(n.URL, 50)
		eventCount := len(allNotifEvents)
		if len(n.Events) > 0 {
			eventCount = len(n.Events)
		}

		nameStyle := lipgloss.NewStyle()
		if selected {
			nameStyle = lipgloss.NewStyle().Foreground(ColorCyan)
		}

		line := prefix + nameStyle.Render(urlDisplay) +
			DimStyle.Render(fmt.Sprintf(" (%d/%d events)", eventCount, len(allNotifEvents)))

		lines = append(lines, line)
		if len(lines) >= maxH {
			break
		}
	}

	return strings.Join(lines, "\n")
}

func (m *SettingsModel) renderNotifEdit(w int) string {
	var lines []string

	title := "Edit Notification"
	if m.notifIndex >= len(m.notifications) {
		title = "Add Notification"
	}
	lines = append(lines, lipgloss.NewStyle().Foreground(ColorCyan).Bold(true).Render(title)+
		DimStyle.Render(" (Enter: save, Esc: cancel)"))

	// URL field
	urlFocused := m.notifEditFocus == 0
	urlPrefix := "  "
	if urlFocused {
		urlPrefix = "> "
	}
	urlLabel := "Webhook URL"
	urlLabelStyle := DimStyle
	if urlFocused {
		urlLabelStyle = lipgloss.NewStyle().Foreground(ColorCyan).Bold(true)
	}
	urlMaxW := max(w-runewidth.StringWidth(urlPrefix)-16-2, 10)
	var urlVal string
	if urlFocused {
		m.textInput.SetWidth(urlMaxW)
		urlVal = m.textInput.View()
	} else {
		urlVal = renderInactiveInput(m.notifEditURL, urlMaxW, ColorWhite)
	}
	prefixColor := ColorWhite
	if urlFocused {
		prefixColor = ColorCyan
	}
	lines = append(lines, lipgloss.NewStyle().Foreground(prefixColor).Render(urlPrefix)+urlLabelStyle.Render(padRight(urlLabel, 16))+urlVal)

	// Events header
	lines = append(lines, "")
	lines = append(lines, DimStyle.Render("  Events (Space to toggle):"))

	// Event checkboxes grouped by category
	flatIdx := 0
	for _, group := range notifEventGroups {
		lines = append(lines, "")
		lines = append(lines, DimStyle.Render("  "+group.name))
		for _, event := range group.events {
			isFocused := m.notifEditFocus == flatIdx+1
			isChecked := m.notifEditEvents[event]

			prefix := "  "
			if isFocused {
				prefix = "> "
			}

			checkStr := " "
			checkColor := ColorGray
			if isChecked {
				checkStr = "x"
				checkColor = ColorGreen
			}

			eventStyle := lipgloss.NewStyle().Foreground(ColorWhite)
			if isFocused {
				eventStyle = lipgloss.NewStyle().Foreground(ColorCyan)
			}

			lines = append(lines, lipgloss.NewStyle().Foreground(func() color.Color {
				if isFocused {
					return ColorCyan
				}
				return ColorWhite
			}()).Render(prefix)+
				lipgloss.NewStyle().Foreground(checkColor).Render("["+checkStr+"]")+
				eventStyle.Render(" "+event))
			flatIdx++
		}
	}

	return strings.Join(lines, "\n")
}

func (m *SettingsModel) renderSecurity(w int) string {
	switch m.secMode {
	case securitySet:
		return m.renderSecuritySet(w)
	case securityRemove:
		return m.renderSecurityRemove(w)
	default:
		return m.renderSecurityStatus(w)
	}
}

func (m *SettingsModel) renderSecurityStatus(w int) string {
	var lines []string

	// Password status
	lines = append(lines, "")
	if m.hasPassword() {
		lines = append(lines, "  Password: "+lipgloss.NewStyle().Foreground(ColorGreen).Bold(true).Render("Set"))
	} else {
		lines = append(lines, "  Password: "+DimStyle.Render("Not set"))
	}

	lines = append(lines, "")

	// Actions
	actionLine := DimStyle.Render("  `: Set password")
	if m.hasPassword() {
		actionLine = DimStyle.Render("  `: Change password  ~: Remove password")
	}
	lines = append(lines, actionLine)

	if m.secMessage != "" {
		lines = append(lines, "")
		lines = append(lines, "  "+lipgloss.NewStyle().Foreground(m.secMessageColor).Render(m.secMessage))
	}

	return strings.Join(lines, "\n")
}

// renderSecurityCompact renders a compact password status below Network fields.
func (m *SettingsModel) renderSecurityCompact(w int) string {
	var lines []string
	lines = append(lines, DimStyle.Render(strings.Repeat("\u2500", w-4)))

	status := DimStyle.Render("Not set")
	if m.hasPassword() {
		status = lipgloss.NewStyle().Foreground(ColorGreen).Render("Set")
	}
	lines = append(lines, "  Password: "+status)

	actionLine := DimStyle.Render("  `: Set password")
	if m.hasPassword() {
		actionLine = DimStyle.Render("  `: Change  ~: Remove")
	}
	lines = append(lines, actionLine)

	if m.secMessage != "" {
		lines = append(lines, "  "+lipgloss.NewStyle().Foreground(m.secMessageColor).Render(m.secMessage))
	}

	return strings.Join(lines, "\n")
}

func (m *SettingsModel) renderSecuritySet(w int) string {
	var lines []string

	title := "Set Password"
	if m.hasPassword() {
		title = "Change Password"
	}
	lines = append(lines, lipgloss.NewStyle().Foreground(ColorCyan).Bold(true).Render(title)+
		DimStyle.Render(" (Enter: save, Esc: cancel)"))
	lines = append(lines, "")

	type pwField struct {
		label string
		value string
	}
	var fields []pwField
	if m.hasPassword() {
		fields = append(fields, pwField{"Current password", m.secCurrentPw})
	}
	fields = append(fields, pwField{"New password", m.secNewPw})
	fields = append(fields, pwField{"Confirm password", m.secConfirmPw})

	maxLabel := 0
	for _, f := range fields {
		if len(f.label) > maxLabel {
			maxLabel = len(f.label)
		}
	}
	padW := maxLabel + 2

	for idx, f := range fields {
		isFocused := idx == m.secFieldIndex
		prefix := "  "
		if isFocused {
			prefix = "> "
		}
		labelStyle := DimStyle
		if isFocused {
			labelStyle = lipgloss.NewStyle().Foreground(ColorCyan).Bold(true)
		}
		prefixStyle := lipgloss.NewStyle()
		if isFocused {
			prefixStyle = lipgloss.NewStyle().Foreground(ColorCyan)
		}

		var val string
		if isFocused {
			pwMaxW := max(w-len(prefix)-padW-2, 10)
			m.textInput.SetWidth(pwMaxW)
			val = m.textInput.View()
		} else {
			val = renderPasswordDots(f.value)
		}

		lines = append(lines, prefixStyle.Render(prefix)+labelStyle.Render(padRight(f.label, padW))+val)
	}

	if m.secMessage != "" {
		lines = append(lines, "")
		lines = append(lines, "  "+lipgloss.NewStyle().Foreground(m.secMessageColor).Render(m.secMessage))
	}

	return strings.Join(lines, "\n")
}

func (m *SettingsModel) renderSecurityRemove(w int) string {
	var lines []string

	lines = append(lines, lipgloss.NewStyle().Foreground(ColorRed).Bold(true).Render("Remove Password")+
		DimStyle.Render(" (Enter: confirm, Esc: cancel)"))
	lines = append(lines, "")

	isExternal := false
	if m.cfg != nil {
		if m.cfgMu != nil {
			m.cfgMu.RLock()
		}
		isExternal = m.cfg.Network.NetworkAccess == "external"
		if m.cfgMu != nil {
			m.cfgMu.RUnlock()
		}
	}
	if isExternal {
		lines = append(lines, "  "+lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Render(
			"Warning: Network access will be reset to localhost"))
	}

	// Password field
	labelStyle := lipgloss.NewStyle().Foreground(ColorCyan).Bold(true)
	pwMaxW := max(w-2-20-2, 10)
	m.textInput.SetWidth(pwMaxW)
	lines = append(lines, "> "+labelStyle.Render(padRight("Current password", 20))+m.textInput.View())

	if m.secMessage != "" {
		lines = append(lines, "")
		lines = append(lines, "  "+lipgloss.NewStyle().Foreground(m.secMessageColor).Render(m.secMessage))
	}

	return strings.Join(lines, "\n")
}

func (m *SettingsModel) renderRestartOverlay() string {
	w := min(50, m.width-4)
	h := 10

	var content strings.Builder
	content.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Bold(true).
		Render("Restart Required") + "\n\n")
	content.WriteString("Some settings require a restart to take effect:\n")
	content.WriteString(DimStyle.Render("port, network access, database path, log settings") + "\n\n")

	content.WriteString(lipgloss.NewStyle().Foreground(ColorCyan).Render("Enter: Restart now"))
	content.WriteString("  ")
	content.WriteString(DimStyle.Render("Esc: Close without restart"))

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#f1c40f")).
		Width(w).
		Height(h).
		Render(content.String())

	return centerBox(box, m.width, m.height)
}
