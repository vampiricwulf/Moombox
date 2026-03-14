package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// AddVideoStep represents steps in the add video dialog.
type AddVideoStep int

const (
	AddStepURL         AddVideoStep = iota // Enter URL/ID
	AddStepVideoFormat                     // Select video format (advanced)
	AddStepAudioFormat                     // Select audio format (advanced)
	AddStepTimestamps                      // Start/end times (advanced)
	AddStepConfirm                         // Confirmation (advanced)
)

// VideoFormat represents a video format option.
type VideoFormat struct {
	Itag         int
	MimeType     string
	Width        int
	Height       int
	Fps          int
	Bitrate      int
	QualityLabel string
}

// AudioFormat represents an audio format option.
type AudioFormat struct {
	Itag            int
	MimeType        string
	Bitrate         int
	AudioQuality    string
	AudioSampleRate string
}

// FormatsData holds format info fetched from API.
type FormatsData struct {
	VideoID       string
	Title         string
	ChannelName   string
	LengthSeconds int
	VideoFormats  []VideoFormat
	AudioFormats  []AudioFormat
	BestWebmVideo int
	BestMp4Video  int
	BestOpusAudio int
	BestAacAudio  int
}

// AddVideoModel manages the add video dialog.
type AddVideoModel struct {
	visible bool
	width   int
	height  int

	// URL mode state
	step            AddVideoStep
	urlInput        string
	videoID         string
	platform        string // "youtube" or "twitch"
	advancedEnabled bool   // checkbox state at step 0
	advancedMode    bool   // in advanced wizard
	errorMsg        string

	// Format selection (advanced)
	formats           *FormatsData
	selectedVideoItag *int // nil=auto, -1=none, else specific itag
	selectedAudioItag *int
	loading           bool

	// Format tables (built when formats arrive)
	videoTable table.Model
	audioTable table.Model

	// Timestamps (advanced)
	startTimeInput string
	endTimeInput   string
	timeInputFocus int // 0=start, 1=end

	// Shared text input component (holds the currently-active text field)
	textInput textinput.Model

	// Loading spinner
	spinner spinner.Model
}

// NewAddVideoModel creates a new add video dialog.
func NewAddVideoModel() *AddVideoModel {
	return &AddVideoModel{
		textInput: newTextInput(),
		spinner:   newSpinner(),
	}
}

// Open shows the dialog and resets state.
func (m *AddVideoModel) Open() {
	m.visible = true
	m.reset()
}

// Close hides the dialog.
func (m *AddVideoModel) Close() {
	m.visible = false
}

// IsVisible returns true if the dialog is shown.
func (m *AddVideoModel) IsVisible() bool {
	return m.visible
}

// SetSize updates the dialog dimensions.
func (m *AddVideoModel) SetSize(w, h int) {
	m.width = w
	m.height = h
}

func (m *AddVideoModel) reset() {
	m.step = AddStepURL
	m.urlInput = ""
	m.videoID = ""
	m.platform = "youtube"
	m.advancedEnabled = false
	m.advancedMode = false
	m.errorMsg = ""
	m.formats = nil
	m.selectedVideoItag = nil
	m.selectedAudioItag = nil
	m.loading = false
	m.startTimeInput = ""
	m.endTimeInput = ""
	m.timeInputFocus = 0
	m.syncToTextInput()
}

// SetFormats is called after format data is fetched from the API.
func (m *AddVideoModel) SetFormats(f *FormatsData) {
	m.formats = f
	m.loading = false
	m.buildFormatTables()
}

// buildFormatTables constructs bubbles/table models for video and audio format selection.
func (m *AddVideoModel) buildFormatTables() {
	if m.formats == nil {
		return
	}

	ts := formatTableStyles()

	// Video table
	videoCols := []table.Column{
		{Title: "#", Width: 3},
		{Title: "Resolution", Width: 12},
		{Title: "FPS", Width: 4},
		{Title: "Codec", Width: 5},
		{Title: "Bitrate", Width: 10},
		{Title: "Note", Width: 12},
	}
	var videoRows []table.Row
	for i, f := range m.formats.VideoFormats {
		codec := ""
		if strings.Contains(f.MimeType, "mp4") {
			codec = "MP4"
		} else if strings.Contains(f.MimeType, "webm") {
			codec = "WEBM"
		}
		bitrate := ""
		if f.Bitrate > 0 {
			bitrate = fmt.Sprintf("%dkbps", f.Bitrate/1000)
		}
		fps := ""
		if f.Fps > 0 {
			fps = fmt.Sprintf("%d", f.Fps)
		}
		note := ""
		if f.Itag == m.formats.BestMp4Video {
			note = "Best MP4"
		}
		if f.Itag == m.formats.BestWebmVideo {
			note = "Best WEBM"
		}
		videoRows = append(videoRows, table.Row{
			fmt.Sprintf("%d", i+1),
			fmt.Sprintf("%dx%d", f.Width, f.Height),
			fps, codec, bitrate, note,
		})
	}
	m.videoTable = table.New(
		table.WithColumns(videoCols),
		table.WithRows(videoRows),
		table.WithHeight(min(8, len(videoRows))),
		table.WithFocused(true),
		table.WithStyles(ts),
	)

	// Audio table
	audioCols := []table.Column{
		{Title: "#", Width: 3},
		{Title: "Bitrate", Width: 10},
		{Title: "Rate", Width: 8},
		{Title: "Codec", Width: 5},
		{Title: "Note", Width: 12},
	}
	var audioRows []table.Row
	for i, f := range m.formats.AudioFormats {
		bitrate := ""
		if f.Bitrate > 0 {
			bitrate = fmt.Sprintf("%dkbps", f.Bitrate/1000)
		}
		sampleRate := ""
		if f.AudioSampleRate != "" {
			if rate, err := strconv.Atoi(f.AudioSampleRate); err == nil {
				sampleRate = fmt.Sprintf("%dkHz", rate/1000)
			}
		}
		codec := ""
		if strings.Contains(f.MimeType, "opus") {
			codec = "OPUS"
		} else if strings.Contains(f.MimeType, "mp4a") || strings.Contains(f.MimeType, "aac") {
			codec = "AAC"
		}
		note := ""
		if f.Itag == m.formats.BestOpusAudio {
			note = "Best OPUS"
		}
		if f.Itag == m.formats.BestAacAudio {
			note = "Best AAC"
		}
		audioRows = append(audioRows, table.Row{
			fmt.Sprintf("%d", i+1),
			bitrate, sampleRate, codec, note,
		})
	}
	m.audioTable = table.New(
		table.WithColumns(audioCols),
		table.WithRows(audioRows),
		table.WithHeight(min(8, len(audioRows))),
		table.WithFocused(true),
		table.WithStyles(ts),
	)
}

// SetError is called when a format fetch fails.
func (m *AddVideoModel) SetError(err string) {
	m.errorMsg = err
	m.loading = false
}

// UpdateComponents routes tea.Msg to embedded textinput/spinner/table and syncs.
func (m *AddVideoModel) UpdateComponents(msg tea.Msg) tea.Cmd {
	if !m.visible {
		return nil
	}
	var cmds []tea.Cmd
	// Route spinner tick when loading
	if m.loading {
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	// Route table events during format selection
	if !m.loading && m.formats != nil {
		switch m.step {
		case AddStepVideoFormat:
			var cmd tea.Cmd
			m.videoTable, cmd = m.videoTable.Update(msg)
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
		case AddStepAudioFormat:
			var cmd tea.Cmd
			m.audioTable, cmd = m.audioTable.Update(msg)
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	}
	// Route text input
	if m.textInput.Focused() {
		prev := m.textInput.Value()
		var cmd tea.Cmd
		m.textInput, cmd = m.textInput.Update(msg)
		if m.textInput.Value() != prev {
			m.syncFromTextInput()
		}
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return tea.Batch(cmds...)
}

// activeTextTarget returns a pointer to the currently active text field, or nil if none.
func (m *AddVideoModel) activeTextTarget() *string {
	switch m.step {
	case AddStepURL:
		return &m.urlInput
	case AddStepTimestamps:
		if m.timeInputFocus == 0 {
			return &m.startTimeInput
		}
		return &m.endTimeInput
	}
	return nil
}

// syncFromTextInput writes the textinput value back to the active field.
func (m *AddVideoModel) syncFromTextInput() {
	if target := m.activeTextTarget(); target != nil {
		*target = m.textInput.Value()
	}
}

// syncToTextInput loads the active field into the textinput and sets validation.
func (m *AddVideoModel) syncToTextInput() {
	target := m.activeTextTarget()
	if target != nil {
		m.textInput.SetValue(*target)
		if m.step == AddStepTimestamps {
			m.textInput.Validate = validateTimeChars
		} else {
			m.textInput.Validate = nil
		}
		m.textInput.Focus()
	} else {
		m.textInput.Blur()
	}
}

// HandleKey processes key input. Returns (action, data) where action can be:
// "submit" with URL, "fetch_formats" with video ID, or "" for no action.
func (m *AddVideoModel) HandleKey(key string) (string, string) {
	// Clear error on input
	if key != keyEnter && key != keyEsc && key != keyTab {
		m.errorMsg = ""
	}

	if key == keyEsc {
		return m.handleEscape()
	}

	switch m.step {
	case AddStepURL:
		return m.handleURLStep(key)
	case AddStepVideoFormat:
		return m.handleFormatStep(key, true)
	case AddStepAudioFormat:
		return m.handleFormatStep(key, false)
	case AddStepTimestamps:
		return m.handleTimestampsStep(key)
	case AddStepConfirm:
		return m.handleConfirmStep(key)
	}

	return "", ""
}

func (m *AddVideoModel) handleEscape() (string, string) {
	switch m.step {
	case AddStepURL:
		m.Close()
	case AddStepVideoFormat:
		m.step = AddStepURL
		m.advancedMode = false
		m.syncToTextInput()
	case AddStepAudioFormat:
		m.step = AddStepVideoFormat
	case AddStepTimestamps:
		m.step = AddStepAudioFormat
		m.textInput.Blur()
	case AddStepConfirm:
		m.step = AddStepTimestamps
		m.syncToTextInput()
	}
	return "", ""
}

func (m *AddVideoModel) handleURLStep(key string) (string, string) {
	switch key {
	case keyTab:
		m.advancedEnabled = !m.advancedEnabled
		return "", ""

	case keyEnter:
		input := strings.TrimSpace(m.urlInput)
		if input == "" {
			m.errorMsg = "Please enter a URL or video ID"
			return "", ""
		}

		vid, plat := extractMediaID(input)
		if plat == "twitch_clip" {
			m.errorMsg = "Twitch clips are not supported"
			return "", ""
		}
		if vid == "" {
			m.errorMsg = "Invalid video ID or URL"
			return "", ""
		}

		m.videoID = vid
		m.platform = plat

		// Twitch: no advanced options, submit directly with parsed ID
		if plat == "twitch" {
			return "submit", vid
		}

		// YouTube: check advanced mode
		if !m.advancedEnabled {
			return "submit", vid
		}

		// Advanced: move to format selection (blur textinput)
		m.advancedMode = true
		m.step = AddStepVideoFormat
		m.loading = true
		m.spinner = newSpinner()
		m.textInput.Blur()
		return "fetch_formats", vid
	}
	return "", ""
}

func (m *AddVideoModel) handleFormatStep(key string, isVideo bool) (string, string) {
	if m.loading {
		return "", ""
	}
	switch key {
	case "a", "A":
		// Auto selection
		if isVideo {
			m.selectedVideoItag = nil
			m.step = AddStepAudioFormat
		} else {
			m.selectedAudioItag = nil
			m.step = AddStepTimestamps
			m.syncToTextInput()
		}
		return "", ""

	case "n", "N":
		none := -1
		if isVideo {
			m.selectedVideoItag = &none
			m.step = AddStepAudioFormat
		} else {
			m.selectedAudioItag = &none
			m.step = AddStepTimestamps
			m.syncToTextInput()
		}
		return "", ""

	case keyEnter:
		// Select the table's highlighted row
		if m.formats == nil {
			return "", ""
		}
		if isVideo {
			idx := m.videoTable.Cursor()
			if idx >= 0 && idx < len(m.formats.VideoFormats) {
				itag := m.formats.VideoFormats[idx].Itag
				m.selectedVideoItag = &itag
				m.step = AddStepAudioFormat
			}
		} else {
			idx := m.audioTable.Cursor()
			if idx >= 0 && idx < len(m.formats.AudioFormats) {
				itag := m.formats.AudioFormats[idx].Itag
				m.selectedAudioItag = &itag
				m.step = AddStepTimestamps
				m.syncToTextInput()
			}
		}
		return "", ""

	default:
		// Digit selection (1-9)
		if len(key) == 1 && key[0] >= '1' && key[0] <= '9' {
			idx := int(key[0]-'0') - 1
			if isVideo && m.formats != nil && idx < len(m.formats.VideoFormats) {
				itag := m.formats.VideoFormats[idx].Itag
				m.selectedVideoItag = &itag
				m.step = AddStepAudioFormat
			} else if !isVideo && m.formats != nil && idx < len(m.formats.AudioFormats) {
				itag := m.formats.AudioFormats[idx].Itag
				m.selectedAudioItag = &itag
				m.step = AddStepTimestamps
				m.syncToTextInput()
			}
		}
		// Arrow keys and other navigation are handled by table via UpdateComponents
	}
	return "", ""
}

func (m *AddVideoModel) handleTimestampsStep(key string) (string, string) {
	switch key {
	case keyTab:
		m.timeInputFocus = 1 - m.timeInputFocus
		m.syncToTextInput()
		return "", ""
	case keyEnter:
		if m.startTimeInput != "" {
			if _, err := parseTimeToSeconds(m.startTimeInput); err != nil {
				m.errorMsg = "Invalid start time format"
				return "", ""
			}
		}
		if m.endTimeInput != "" {
			if _, err := parseTimeToSeconds(m.endTimeInput); err != nil {
				m.errorMsg = "Invalid end time format"
				return "", ""
			}
		}
		if m.startTimeInput != "" && m.endTimeInput != "" {
			s, _ := parseTimeToSeconds(m.startTimeInput)
			e, _ := parseTimeToSeconds(m.endTimeInput)
			if e <= s {
				m.errorMsg = "End time must be after start time"
				return "", ""
			}
		}
		m.step = AddStepConfirm
		m.textInput.Blur()
		return "", ""
	}
	return "", ""
}

func (m *AddVideoModel) handleConfirmStep(key string) (string, string) {
	if key == keyEnter {
		if m.selectedVideoItag != nil && *m.selectedVideoItag == -1 &&
			m.selectedAudioItag != nil && *m.selectedAudioItag == -1 {
			m.errorMsg = "Cannot select None for both video and audio"
			return "", ""
		}
		return "submit", m.videoID
	}
	return "", ""
}

// GetSelectedVideoItag returns the selected video itag (nil=auto, -1=none).
func (m *AddVideoModel) GetSelectedVideoItag() *int { return m.selectedVideoItag }

// GetSelectedAudioItag returns the selected audio itag (nil=auto, -1=none).
func (m *AddVideoModel) GetSelectedAudioItag() *int { return m.selectedAudioItag }

// GetStartTime returns the start time input.
func (m *AddVideoModel) GetStartTime() string { return m.startTimeInput }

// GetEndTime returns the end time input.
func (m *AddVideoModel) GetEndTime() string { return m.endTimeInput }

// SpinnerInit returns the spinner's initial tick command when loading.
func (m *AddVideoModel) SpinnerInit() tea.Cmd { return m.spinner.Tick }

// GetPlatform returns the detected platform.
func (m *AddVideoModel) GetPlatform() string { return m.platform }

// View renders the dialog.
func (m *AddVideoModel) View() string {
	if !m.visible {
		return ""
	}

	boxW := max(min(80, m.width-4), 30)  // match TS: min(width, 80)
	boxH := max(min(30, m.height-4), 8) // match TS: min(height-4, 30)

	contentW := boxW - 2

	var borderColor lipgloss.Color
	if m.advancedMode || m.advancedEnabled {
		borderColor = ColorCookies // magenta
	} else {
		borderColor = ColorCyan
	}

	var content string
	switch m.step {
	case AddStepURL:
		content = m.renderURLStep(contentW, boxH)
	case AddStepVideoFormat:
		content = m.renderFormatStep(contentW, boxH, true)
	case AddStepAudioFormat:
		content = m.renderFormatStep(contentW, boxH, false)
	case AddStepTimestamps:
		content = m.renderTimestamps(contentW, boxH)
	case AddStepConfirm:
		content = m.renderConfirm(contentW, boxH)
	}

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor).
		Width(contentW).
		Height(boxH).
		Render(content)

	return centerBox(box, m.width, m.height)
}

func (m *AddVideoModel) renderURLStep(w, h int) string {
	var lines []string

	title := "Add Video"
	if m.advancedEnabled {
		title = "Add Video (Advanced)"
	}
	lines = append(lines, TitleStyle.Render(title))
	lines = append(lines, "")
	lines = append(lines, "Enter a YouTube URL, video ID, or Twitch channel:")
	lines = append(lines, "")
	color := ColorCyan
	if m.advancedEnabled {
		color = ColorCookies
	}
	m.textInput.TextStyle = lipgloss.NewStyle().Foreground(color)
	m.textInput.Cursor.Style = lipgloss.NewStyle().Foreground(color)
	m.textInput.Width = w - 2
	lines = append(lines, m.textInput.View())
	lines = append(lines, "")

	check := "[ ]"
	if m.advancedEnabled {
		check = "[✓]"
	}
	checkStyle := lipgloss.NewStyle()
	if m.advancedEnabled {
		checkStyle = checkStyle.Foreground(ColorCookies)
	}
	lines = append(lines, checkStyle.Render(check+" Advanced Options"))

	if m.advancedEnabled {
		lines = append(lines, DimStyle.Render("  • Choose video/audio format"))
		lines = append(lines, DimStyle.Render("  • Set start/end timestamps"))
	}

	if m.errorMsg != "" {
		lines = append(lines, "")
		lines = append(lines, ErrorStyle.Render(m.errorMsg))
	}

	lines = append(lines, "")
	lines = append(lines, DimStyle.Render("Tab: Advanced | Enter: Continue | Esc: Cancel"))

	return strings.Join(lines, "\n")
}

func (m *AddVideoModel) renderFormatStep(w, h int, isVideo bool) string {
	var lines []string

	stepNum := "1/4"
	title := "Select Video Format"
	if !isVideo {
		stepNum = "2/4"
		title = "Select Audio Format"
	}
	lines = append(lines, TitleStyle.Render(title)+" "+DimStyle.Render("(Step "+stepNum+")"))
	lines = append(lines, "")

	if m.loading {
		lines = append(lines, m.spinner.View()+" Fetching formats...")
		return strings.Join(lines, "\n")
	}

	if m.errorMsg != "" {
		lines = append(lines, lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Render(m.errorMsg))
		lines = append(lines, "")
	}

	lines = append(lines, lipgloss.NewStyle().Foreground(ColorCyan).Render("[a] Auto (best quality)"))
	if isVideo {
		lines = append(lines, lipgloss.NewStyle().Foreground(ColorCyan).Render("[n] None (audio only)"))
	} else {
		lines = append(lines, lipgloss.NewStyle().Foreground(ColorCyan).Render("[n] None (video only)"))
	}
	lines = append(lines, "")

	if m.formats != nil {
		// Adjust table height to fit available space
		tableH := max(h-10, 3)
		if isVideo {
			m.videoTable.SetHeight(min(tableH, len(m.formats.VideoFormats)))
			lines = append(lines, m.videoTable.View())
		} else {
			m.audioTable.SetHeight(min(tableH, len(m.formats.AudioFormats)))
			lines = append(lines, m.audioTable.View())
		}
	}

	lines = append(lines, "")
	lines = append(lines, DimStyle.Render("A: Auto | N: None | Enter/1-9: Select | Esc: Back"))

	return strings.Join(lines, "\n")
}

func (m *AddVideoModel) renderTimestamps(w, h int) string {
	var lines []string

	lines = append(lines, TitleStyle.Render("Timestamps (Optional)")+" "+DimStyle.Render("(Step 3/4)"))
	lines = append(lines, "")

	lines = append(lines, renderTimeInputPair(m.startTimeInput, m.endTimeInput, m.timeInputFocus, m.textInput, w, ColorCookies)...)
	lines = append(lines, "")
	lines = append(lines, DimStyle.Render("Format: HH:MM:SS, MM:SS, or seconds (blank = default)"))

	if m.errorMsg != "" {
		lines = append(lines, "")
		lines = append(lines, ErrorStyle.Render(m.errorMsg))
	}

	lines = append(lines, "")
	lines = append(lines, DimStyle.Render("Tab: Switch field | Enter: Continue | Esc: Back"))

	return strings.Join(lines, "\n")
}

func (m *AddVideoModel) renderConfirm(w, h int) string {
	var lines []string

	lines = append(lines, TitleStyle.Render("Confirmation")+" "+DimStyle.Render("(Step 4/4)"))
	lines = append(lines, "")

	lines = append(lines, fmt.Sprintf("  Video ID:  %s", m.videoID))
	if m.formats != nil && m.formats.Title != "" {
		lines = append(lines, fmt.Sprintf("  Title:     %s", truncateString(m.formats.Title, w-14)))
	}
	lines = append(lines, "")

	vfLabel := "Auto (best quality)"
	if m.selectedVideoItag != nil {
		if *m.selectedVideoItag == -1 {
			vfLabel = "None (audio only)"
		} else {
			vfLabel = fmt.Sprintf("itag %d", *m.selectedVideoItag)
			if m.formats != nil {
				for _, f := range m.formats.VideoFormats {
					if f.Itag == *m.selectedVideoItag {
						container := "MP4"
						if strings.Contains(strings.ToLower(f.MimeType), "webm") {
							container = "WEBM"
						}
						vfLabel = fmt.Sprintf("%dx%d", f.Width, f.Height)
						if f.Fps > 0 {
							vfLabel += fmt.Sprintf("@%d", f.Fps)
						}
						vfLabel += " " + container
						break
					}
				}
			}
		}
	}
	lines = append(lines, fmt.Sprintf("  Video:     %s", vfLabel))

	afLabel := "Auto (best quality)"
	if m.selectedAudioItag != nil {
		if *m.selectedAudioItag == -1 {
			afLabel = "None (video only)"
		} else {
			afLabel = fmt.Sprintf("itag %d", *m.selectedAudioItag)
			if m.formats != nil {
				for _, f := range m.formats.AudioFormats {
					if f.Itag == *m.selectedAudioItag {
						container := "AAC"
						if strings.Contains(strings.ToLower(f.MimeType), "opus") || strings.Contains(strings.ToLower(f.MimeType), "webm") {
							container = "OPUS"
						}
						afLabel = fmt.Sprintf("%dkbps %s", f.Bitrate/1000, container)
						break
					}
				}
			}
		}
	}
	lines = append(lines, fmt.Sprintf("  Audio:     %s", afLabel))

	if m.startTimeInput != "" || m.endTimeInput != "" {
		start := "0:00 (beginning)"
		if m.startTimeInput != "" {
			start = m.startTimeInput
		}
		end := "(end of video)"
		if m.endTimeInput != "" {
			end = m.endTimeInput
		}
		lines = append(lines, fmt.Sprintf("  Time:      %s - %s", start, end))
	}

	if m.errorMsg != "" {
		lines = append(lines, "")
		lines = append(lines, ErrorStyle.Render(m.errorMsg))
	}

	lines = append(lines, "")
	lines = append(lines, DimStyle.Render("Enter: Submit | Esc: Back"))

	return strings.Join(lines, "\n")
}

// centerBox centers content on screen.
func centerBox(box string, screenW, screenH int) string {
	boxLines := strings.Split(box, "\n")
	boxH := len(boxLines)
	boxW := 0
	for _, l := range boxLines {
		if w := lipgloss.Width(l); w > boxW {
			boxW = w
		}
	}

	padTop := max((screenH-boxH)/2, 0)
	padLeft := max((screenW-boxW)/2, 0)

	lines := make([]string, 0, screenH)
	pad := strings.Repeat(" ", screenW)
	for range padTop {
		lines = append(lines, pad)
	}
	leftPad := strings.Repeat(" ", padLeft)
	for _, line := range boxLines {
		lines = append(lines, leftPad+line)
	}
	remaining := screenH - padTop - boxH
	for range remaining {
		lines = append(lines, pad)
	}

	return strings.Join(lines, "\n")
}

// extractMediaID extracts video ID and platform from URL or ID string.
func extractMediaID(input string) (string, string) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", ""
	}

	// Check for explicit Twitch URLs first (match TS: extractMediaId checks twitch.tv URLs first)
	if strings.Contains(input, "twitch.tv") || strings.Contains(input, "clips.twitch.tv") {
		id, ttype := extractTwitchTarget(input)
		if ttype != "" {
			return id, ttype
		}
	}

	// Try YouTube
	vid := extractYouTubeVideoID(input)
	if vid != "" {
		return vid, "youtube"
	}

	// Try Twitch (raw usernames, VOD IDs, etc.)
	id, ttype := extractTwitchTarget(input)
	if ttype != "" {
		return id, ttype
	}

	return "", ""
}

// extractYouTubeVideoID extracts video ID from various YouTube URL formats or validates a raw 11-char ID.
// Supports: youtube.com/watch?v=, youtu.be/, /live/, /shorts/, /embed/, /v/
func extractYouTubeVideoID(input string) string {
	input = strings.TrimSpace(input)

	// Direct 11-character ID
	if len(input) == 11 && isVideoIDChar(input) {
		return input
	}

	// v= parameter (most common)
	if _, after, ok := strings.Cut(input, "v="); ok {
		vid, _, _ := strings.Cut(after, "&")
		vid = strings.TrimRight(vid, "/")
		if len(vid) == 11 && isVideoIDChar(vid) {
			return vid
		}
	}

	// Path-based patterns: youtu.be/, /live/, /shorts/, /embed/, /v/
	pathPatterns := []string{"youtu.be/", "/live/", "/shorts/", "/embed/", "/v/"}
	for _, pat := range pathPatterns {
		if _, after, ok := strings.Cut(input, pat); ok {
			vid, _, _ := strings.Cut(after, "?")
			vid = strings.TrimRight(vid, "/")
			if len(vid) == 11 && isVideoIDChar(vid) {
				return vid
			}
		}
	}

	return ""
}

// twitchReservedPaths are paths on twitch.tv that are not channel names (match TS).
var twitchReservedPaths = map[string]bool{
	"directory": true, "downloads": true, "jobs": true,
	"settings": true, "videos": true, "search": true, "p": true,
}

// extractTwitchTarget extracts Twitch channel/VOD/clip from URL or raw input.
// Returns (id, type) where type is "twitch", "twitch_clip", or "".
func extractTwitchTarget(input string) (string, string) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", ""
	}

	// Try URL patterns
	urlStr := input
	if !strings.HasPrefix(urlStr, "http://") && !strings.HasPrefix(urlStr, "https://") {
		if strings.HasPrefix(urlStr, "twitch.tv") || strings.HasPrefix(urlStr, "www.twitch.tv") || strings.HasPrefix(urlStr, "clips.twitch.tv") {
			urlStr = "https://" + urlStr
		}
	}

	if strings.HasPrefix(urlStr, "http") {
		// Extract host and path
		rest := urlStr
		if _, after, ok := strings.Cut(rest, "://"); ok {
			rest = after
		}
		host, pathStr, _ := strings.Cut(rest, "/")
		if pathStr != "" {
			pathStr = "/" + pathStr
		}
		host = strings.TrimPrefix(host, "www.")
		// Remove port if present
		host, _, _ = strings.Cut(host, ":")

		// clips.twitch.tv/{slug}
		if host == "clips.twitch.tv" {
			slug := firstPathSegment(pathStr)
			if slug != "" {
				return "", "twitch_clip"
			}
		}

		if host == "twitch.tv" {
			parts := splitPathSegments(pathStr)

			// /videos/{id}
			if len(parts) >= 2 && parts[0] == "videos" && isNumeric(parts[1]) {
				return "tw_v" + parts[1], "twitch"
			}

			// /{login}/video/{id}
			if len(parts) >= 3 && parts[1] == "video" && isNumeric(parts[2]) {
				return "tw_v" + parts[2], "twitch"
			}

			// /{login}/clip/{slug}
			if len(parts) >= 3 && parts[1] == "clip" && parts[2] != "" {
				return "", "twitch_clip"
			}

			// /{login} (not reserved)
			if len(parts) >= 1 && parts[0] != "" {
				login := strings.ToLower(parts[0])
				if !twitchReservedPaths[login] && len(login) <= 25 && isAlphaNumUnderscore(login) {
					return login, "twitch"
				}
			}
		}
	}

	// Raw VOD ID with "v" prefix (e.g., "v2345678901")
	if len(input) >= 2 && input[0] == 'v' && len(input) <= 13 && isNumeric(input[1:]) {
		return "tw_v" + input[1:], "twitch"
	}

	// Raw numeric ID → VOD (7-12 digits)
	if len(input) >= 7 && len(input) <= 12 && isNumeric(input) {
		return "tw_v" + input, "twitch"
	}

	// Raw username (1-25 alphanumeric+underscore, must start with letter — match TS)
	if len(input) >= 1 && len(input) <= 25 && ((input[0] >= 'a' && input[0] <= 'z') || (input[0] >= 'A' && input[0] <= 'Z')) {
		if isAlphaNumUnderscore(input) {
			return strings.ToLower(input), "twitch"
		}
	}

	return "", ""
}

func isVideoIDChar(s string) bool {
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func isAlphaNumUnderscore(s string) bool {
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_') {
			return false
		}
	}
	return true
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func firstPathSegment(path string) string {
	path = strings.TrimPrefix(path, "/")
	path, _, _ = strings.Cut(path, "/")
	path, _, _ = strings.Cut(path, "?")
	return path
}

func splitPathSegments(path string) []string {
	path = strings.TrimPrefix(path, "/")
	path, _, _ = strings.Cut(path, "?")
	path = strings.TrimRight(path, "/")
	if path == "" {
		return nil
	}
	return strings.Split(path, "/")
}

// parseTimeToSeconds parses HH:MM:SS, MM:SS, or raw seconds to float64.
func parseTimeToSeconds(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty time")
	}

	parts := strings.Split(s, ":")
	switch len(parts) {
	case 1:
		return strconv.ParseFloat(parts[0], 64)
	case 2:
		mins, err := strconv.Atoi(parts[0])
		if err != nil {
			return 0, err
		}
		secs, err := strconv.ParseFloat(parts[1], 64)
		if err != nil {
			return 0, err
		}
		if mins < 0 || mins > 59 || secs < 0 || secs > 59 {
			return 0, fmt.Errorf("minutes and seconds must be 0-59")
		}
		return float64(mins)*60 + secs, nil
	case 3:
		hours, err := strconv.Atoi(parts[0])
		if err != nil {
			return 0, err
		}
		mins, err := strconv.Atoi(parts[1])
		if err != nil {
			return 0, err
		}
		secs, err := strconv.ParseFloat(parts[2], 64)
		if err != nil {
			return 0, err
		}
		if mins < 0 || mins > 59 || secs < 0 || secs > 59 {
			return 0, fmt.Errorf("minutes and seconds must be 0-59")
		}
		return float64(hours)*3600 + float64(mins)*60 + secs, nil
	}
	return 0, fmt.Errorf("invalid time format")
}
