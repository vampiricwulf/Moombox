package tui

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
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

// InputMode for add video dialog.
type InputMode int

const (
	InputModeURL    InputMode = iota
	InputModeImport
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
	scrollOffset      int
	loading           bool

	// Timestamps (advanced)
	startTimeInput string
	endTimeInput   string
	timeInputFocus int // 0=start, 1=end

	// Import mode state
	inputMode       InputMode
	importPath      string
	importTitle     string
	importChannel   string
	importStep      int // 0=path, 1=metadata, 2=uploading
	importMetaFocus int // 0=title, 1=channel
}

// NewAddVideoModel creates a new add video dialog.
func NewAddVideoModel() *AddVideoModel {
	return &AddVideoModel{}
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
	m.scrollOffset = 0
	m.loading = false
	m.startTimeInput = ""
	m.endTimeInput = ""
	m.timeInputFocus = 0
	m.inputMode = InputModeURL
	m.importPath = ""
	m.importTitle = ""
	m.importChannel = ""
	m.importStep = 0
	m.importMetaFocus = 0
}

// SetFormats is called after format data is fetched from the API.
func (m *AddVideoModel) SetFormats(f *FormatsData) {
	m.formats = f
	m.loading = false
}

// SetError is called when a format fetch or import fails.
func (m *AddVideoModel) SetError(err string) {
	m.errorMsg = err
	m.loading = false
}

// HandleKey processes key input. Returns (action, data) where action can be:
// "submit" with URL, "fetch_formats" with video ID, "import" with path,
// or "" for no action.
func (m *AddVideoModel) HandleKey(key string) (string, string) {
	// Clear error on input
	if key != keyEnter && key != keyEsc && key != keyTab {
		m.errorMsg = ""
	}

	if key == keyEsc {
		return m.handleEscape()
	}

	if m.inputMode == InputModeImport {
		return m.handleImportKey(key)
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
	if m.inputMode == InputModeImport {
		if m.importStep > 0 {
			m.importStep--
			return "", ""
		}
		m.inputMode = InputModeURL
		m.advancedEnabled = false
		return "", ""
	}

	switch m.step {
	case AddStepURL:
		m.Close()
	case AddStepVideoFormat:
		m.step = AddStepURL
		m.advancedMode = false
	case AddStepAudioFormat:
		m.step = AddStepVideoFormat
	case AddStepTimestamps:
		m.step = AddStepAudioFormat
	case AddStepConfirm:
		m.step = AddStepTimestamps
	}
	return "", ""
}

func (m *AddVideoModel) handleURLStep(key string) (string, string) {
	switch key {
	case keyTab:
		// Cycle: not advanced → advanced → import mode → back
		if !m.advancedEnabled {
			m.advancedEnabled = true
		} else {
			m.advancedEnabled = false
			m.inputMode = InputModeImport
			m.importStep = 0
		}
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

		// Advanced: move to format selection
		m.advancedMode = true
		m.step = AddStepVideoFormat
		m.scrollOffset = 0
		m.loading = true
		return "fetch_formats", vid

	default:
		m.handleTextInput(key, &m.urlInput)
	}
	return "", ""
}

func (m *AddVideoModel) handleFormatStep(key string, isVideo bool) (string, string) {
	switch key {
	case keyEnter, "a", "A":
		// Auto selection
		if isVideo {
			m.selectedVideoItag = nil
			m.step = AddStepAudioFormat
		} else {
			m.selectedAudioItag = nil
			m.step = AddStepTimestamps
		}
		m.scrollOffset = 0
		return "", ""

	case "n", "N":
		none := -1
		if isVideo {
			m.selectedVideoItag = &none
			m.step = AddStepAudioFormat
		} else {
			m.selectedAudioItag = &none
			m.step = AddStepTimestamps
		}
		m.scrollOffset = 0
		return "", ""

	case keyUp:
		if m.scrollOffset > 0 {
			m.scrollOffset--
		}
	case keyDown:
		m.scrollOffset++
	case keyPgUp:
		m.scrollOffset -= 10
		if m.scrollOffset < 0 {
			m.scrollOffset = 0
		}
	case keyPgDown:
		m.scrollOffset += 10

	default:
		// Digit selection (1-9)
		if len(key) == 1 && key[0] >= '1' && key[0] <= '9' {
			idx := int(key[0]-'0') - 1
			if isVideo && m.formats != nil && idx < len(m.formats.VideoFormats) {
				itag := m.formats.VideoFormats[idx].Itag
				m.selectedVideoItag = &itag
				m.step = AddStepAudioFormat
				m.scrollOffset = 0
			} else if !isVideo && m.formats != nil && idx < len(m.formats.AudioFormats) {
				itag := m.formats.AudioFormats[idx].Itag
				m.selectedAudioItag = &itag
				m.step = AddStepTimestamps
				m.scrollOffset = 0
			}
		}
	}
	return "", ""
}

func (m *AddVideoModel) handleTimestampsStep(key string) (string, string) {
	switch key {
	case keyTab:
		m.timeInputFocus = 1 - m.timeInputFocus
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
		return "", ""
	default:
		if m.timeInputFocus == 0 {
			m.handleTextInput(key, &m.startTimeInput)
		} else {
			m.handleTextInput(key, &m.endTimeInput)
		}
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

func (m *AddVideoModel) handleImportKey(key string) (string, string) {
	switch m.importStep {
	case 0:
		switch key {
		case keyTab:
			m.inputMode = InputModeURL
			m.advancedEnabled = false
			return "", ""
		case keyEnter:
			path := strings.TrimSpace(m.importPath)
			if path == "" {
				m.errorMsg = "Please enter a file path"
				return "", ""
			}
			if !strings.HasSuffix(strings.ToLower(path), ".zip") {
				m.errorMsg = "File must be a .zip archive"
				return "", ""
			}
			// Validate file exists (match TS fs.pathExists check)
			if _, err := os.Stat(path); err != nil {
				m.errorMsg = "File not found"
				return "", ""
			}
			m.importStep = 1
			return "", ""
		default:
			m.handleTextInput(key, &m.importPath)
		}

	case 1:
		switch key {
		case keyTab:
			m.importMetaFocus = 1 - m.importMetaFocus
			return "", ""
		case keyEnter:
			m.importStep = 2
			return "import", m.importPath
		default:
			if m.importMetaFocus == 0 {
				m.handleTextInput(key, &m.importTitle)
			} else {
				m.handleTextInput(key, &m.importChannel)
			}
		}

	case 2:
		// No input during upload
		return "", ""
	}
	return "", ""
}

func (m *AddVideoModel) handleTextInput(key string, target *string) {
	switch key {
	case "backspace":
		if len(*target) > 0 {
			runes := []rune(*target)
			*target = string(runes[:len(runes)-1])
		}
	case "ctrl+v":
		// Clipboard paste support (match TS: take first line only)
		if clip := readClipboard(); clip != "" {
			// Match TS: text.split(/[\r\n]/)[0].trim()
			if idx := strings.IndexAny(clip, "\r\n"); idx >= 0 {
				clip = clip[:idx]
			}
			clip = strings.TrimSpace(clip)
			if clip != "" {
				*target += clip
			}
		}
	default:
		if len(key) == 1 && key[0] >= 0x20 && key[0] < 0x7f {
			*target += key
		} else if len(key) > 1 && key[0] != 0x1b {
			*target += key
		}
	}
}

// GetSelectedVideoItag returns the selected video itag (nil=auto, -1=none).
func (m *AddVideoModel) GetSelectedVideoItag() *int { return m.selectedVideoItag }

// GetSelectedAudioItag returns the selected audio itag (nil=auto, -1=none).
func (m *AddVideoModel) GetSelectedAudioItag() *int { return m.selectedAudioItag }

// GetStartTime returns the start time input.
func (m *AddVideoModel) GetStartTime() string { return m.startTimeInput }

// GetEndTime returns the end time input.
func (m *AddVideoModel) GetEndTime() string { return m.endTimeInput }

// GetImportTitle returns the import title.
func (m *AddVideoModel) GetImportTitle() string { return m.importTitle }

// GetImportChannel returns the import channel name.
func (m *AddVideoModel) GetImportChannel() string { return m.importChannel }

// GetPlatform returns the detected platform.
func (m *AddVideoModel) GetPlatform() string { return m.platform }

// View renders the dialog.
func (m *AddVideoModel) View() string {
	if !m.visible {
		return ""
	}

	boxW := min(80, m.width-4)  // match TS: min(width, 80)
	boxH := min(30, m.height-4) // match TS: min(height-4, 30)
	if boxW < 30 {
		boxW = 30
	}
	if boxH < 8 {
		boxH = 8
	}

	contentW := boxW - 2

	var borderColor lipgloss.Color
	if m.inputMode == InputModeImport {
		borderColor = ColorGreen
	} else if m.advancedMode || m.advancedEnabled {
		borderColor = ColorCookies // magenta
	} else {
		borderColor = ColorCyan
	}

	var content string
	if m.inputMode == InputModeImport {
		content = m.renderImport(contentW, boxH)
	} else {
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
	lines = append(lines, renderInputBox(m.urlInput, w-2, m.advancedEnabled))
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
	lines = append(lines, DimStyle.Render("Tab: Cycle mode | Enter: Continue | Esc: Cancel"))

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
		lines = append(lines, "Fetching formats...")
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
		var fmtLines []string
		if isVideo {
			for i, f := range m.formats.VideoFormats {
				line := fmt.Sprintf("[%d] %dx%d", i+1, f.Width, f.Height)
				if f.Fps > 0 {
					line += fmt.Sprintf("@%d", f.Fps)
				}
				if strings.Contains(f.MimeType, "mp4") {
					line += " MP4"
				} else if strings.Contains(f.MimeType, "webm") {
					line += " WEBM"
				}
				if f.Bitrate > 0 {
					line += fmt.Sprintf(" %dkbps", f.Bitrate/1000)
				}
				if f.Itag == m.formats.BestMp4Video {
					line += " [Best MP4]"
				}
				if f.Itag == m.formats.BestWebmVideo {
					line += " [Best WEBM]"
				}
				fmtLines = append(fmtLines, line)
			}
		} else {
			for i, f := range m.formats.AudioFormats {
				line := fmt.Sprintf("[%d]", i+1)
				if f.Bitrate > 0 {
					line += fmt.Sprintf(" %dkbps", f.Bitrate/1000)
				}
				if f.AudioSampleRate != "" {
					if rate, err := strconv.Atoi(f.AudioSampleRate); err == nil {
						line += fmt.Sprintf(" %dkHz", rate/1000)
					}
				}
				if strings.Contains(f.MimeType, "opus") {
					line += " OPUS"
				} else if strings.Contains(f.MimeType, "mp4a") || strings.Contains(f.MimeType, "aac") {
					line += " AAC"
				}
				if f.Itag == m.formats.BestOpusAudio {
					line += " [Best OPUS]"
				}
				if f.Itag == m.formats.BestAacAudio {
					line += " [Best AAC]"
				}
				fmtLines = append(fmtLines, line)
			}
		}

		maxVisible := h - 8
		if maxVisible < 3 {
			maxVisible = 3
		}
		if m.scrollOffset > len(fmtLines)-maxVisible {
			m.scrollOffset = max(0, len(fmtLines)-maxVisible)
		}
		end := m.scrollOffset + maxVisible
		if end > len(fmtLines) {
			end = len(fmtLines)
		}
		if m.scrollOffset > 0 {
			lines = append(lines, DimStyle.Render("  [↑ more above]"))
		}
		for i := m.scrollOffset; i < end; i++ {
			lines = append(lines, fmtLines[i])
		}
		if end < len(fmtLines) {
			lines = append(lines, DimStyle.Render("  [↓ more below]"))
		}
	}

	lines = append(lines, "")
	lines = append(lines, DimStyle.Render("Enter/A: Auto | N: None | 1-9: Select | Esc: Back"))

	return strings.Join(lines, "\n")
}

func (m *AddVideoModel) renderTimestamps(w, h int) string {
	var lines []string

	lines = append(lines, TitleStyle.Render("Timestamps (Optional)")+" "+DimStyle.Render("(Step 3/4)"))
	lines = append(lines, "")

	startLabel := "  Start: "
	endLabel := "  End:   "
	if m.timeInputFocus == 0 {
		startLabel = "> Start: "
	}
	if m.timeInputFocus == 1 {
		endLabel = "> End:   "
	}

	accentColor := ColorCookies
	startStyle := DimStyle
	endStyle := DimStyle
	if m.timeInputFocus == 0 {
		startStyle = lipgloss.NewStyle().Foreground(accentColor)
	}
	if m.timeInputFocus == 1 {
		endStyle = lipgloss.NewStyle().Foreground(accentColor)
	}

	lines = append(lines, startStyle.Render(startLabel)+renderInputBoxCursor(m.startTimeInput, w-12, false, m.timeInputFocus == 0))
	lines = append(lines, endStyle.Render(endLabel)+renderInputBoxCursor(m.endTimeInput, w-12, false, m.timeInputFocus == 1))
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

func (m *AddVideoModel) renderImport(w, h int) string {
	var lines []string

	switch m.importStep {
	case 0:
		lines = append(lines, lipgloss.NewStyle().Foreground(ColorGreen).Bold(true).Render("Import Archive")+" "+DimStyle.Render("(Step 1/3)"))
		lines = append(lines, "")
		lines = append(lines, "Enter path to a .zip file:")
		lines = append(lines, "")
		lines = append(lines, renderInputBox(m.importPath, w-2, false))
		lines = append(lines, "")
		lines = append(lines, DimStyle.Render("ZIP should contain video file + optional chat JSON"))

	case 1:
		lines = append(lines, lipgloss.NewStyle().Foreground(ColorGreen).Bold(true).Render("Metadata (Optional)")+" "+DimStyle.Render("(Step 2/3)"))
		lines = append(lines, "")
		fn := m.importPath
		if len(fn) > 50 {
			fn = "..." + fn[len(fn)-47:]
		}
		lines = append(lines, DimStyle.Render("File: "+fn))
		lines = append(lines, "")

		titleLabel := "  Title:   "
		chanLabel := "  Channel: "
		if m.importMetaFocus == 0 {
			titleLabel = "> Title:   "
		}
		if m.importMetaFocus == 1 {
			chanLabel = "> Channel: "
		}

		titleStyle := DimStyle
		chanStyle := DimStyle
		if m.importMetaFocus == 0 {
			titleStyle = lipgloss.NewStyle().Foreground(ColorGreen)
		}
		if m.importMetaFocus == 1 {
			chanStyle = lipgloss.NewStyle().Foreground(ColorGreen)
		}

		lines = append(lines, titleStyle.Render(titleLabel)+renderInputBox(m.importTitle, w-14, false))
		lines = append(lines, chanStyle.Render(chanLabel)+renderInputBox(m.importChannel, w-14, false))
		lines = append(lines, "")
		lines = append(lines, DimStyle.Render("Leave blank to auto-detect from archive"))

	case 2:
		lines = append(lines, lipgloss.NewStyle().Foreground(ColorGreen).Bold(true).Render("Importing...")+" "+DimStyle.Render("(Step 3/3)"))
		lines = append(lines, "")
		lines = append(lines, "Reading file and importing...")
	}

	if m.errorMsg != "" {
		lines = append(lines, "")
		lines = append(lines, ErrorStyle.Render(m.errorMsg))
	}

	lines = append(lines, "")
	switch m.importStep {
	case 0:
		lines = append(lines, DimStyle.Render("Tab: URL mode | Enter: Continue | Esc: Cancel"))
	case 1:
		lines = append(lines, DimStyle.Render("Tab: Switch field | Enter: Import | Esc: Back"))
	}

	return strings.Join(lines, "\n")
}

// renderInputBox renders a text input with optional cursor.
func renderInputBox(value string, w int, accent bool) string {
	return renderInputBoxCursor(value, w, accent, true)
}

// renderInputBoxCursor renders a text input with cursor only if showCursor is true.
func renderInputBoxCursor(value string, w int, accent bool, showCursor bool) string {
	color := ColorCyan
	if accent {
		color = ColorCookies
	}
	display := value
	if showCursor {
		display += "_"
	}
	if runewidth.StringWidth(display) > w {
		display = truncateString(display, w)
	}
	return lipgloss.NewStyle().Foreground(color).Render(display)
}

// centerBox centers content on screen.
func centerBox(box string, screenW, screenH int) string {
	boxLines := strings.Split(box, "\n")
	boxH := len(boxLines)
	boxW := 0
	for _, l := range boxLines {
		if w := runewidth.StringWidth(l); w > boxW {
			boxW = w
		}
	}

	padTop := (screenH - boxH) / 2
	padLeft := (screenW - boxW) / 2
	if padTop < 0 {
		padTop = 0
	}
	if padLeft < 0 {
		padLeft = 0
	}

	var b strings.Builder
	for range padTop {
		b.WriteString(strings.Repeat(" ", screenW) + "\n")
	}
	for _, line := range boxLines {
		b.WriteString(strings.Repeat(" ", padLeft) + line + "\n")
	}
	remaining := screenH - padTop - boxH
	for range remaining {
		b.WriteString(strings.Repeat(" ", screenW) + "\n")
	}

	return b.String()
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
	if idx := strings.Index(input, "v="); idx >= 0 {
		vid := input[idx+2:]
		if ampIdx := strings.Index(vid, "&"); ampIdx >= 0 {
			vid = vid[:ampIdx]
		}
		vid = strings.TrimRight(vid, "/")
		if len(vid) == 11 && isVideoIDChar(vid) {
			return vid
		}
	}

	// Path-based patterns: youtu.be/, /live/, /shorts/, /embed/, /v/
	pathPatterns := []string{"youtu.be/", "/live/", "/shorts/", "/embed/", "/v/"}
	for _, pat := range pathPatterns {
		if idx := strings.Index(input, pat); idx >= 0 {
			vid := input[idx+len(pat):]
			if qIdx := strings.Index(vid, "?"); qIdx >= 0 {
				vid = vid[:qIdx]
			}
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
		if idx := strings.Index(rest, "://"); idx >= 0 {
			rest = rest[idx+3:]
		}
		host := rest
		pathStr := ""
		if idx := strings.Index(rest, "/"); idx >= 0 {
			host = rest[:idx]
			pathStr = rest[idx:]
		}
		host = strings.TrimPrefix(host, "www.")
		// Remove port if present
		if idx := strings.Index(host, ":"); idx >= 0 {
			host = host[:idx]
		}

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
	if idx := strings.Index(path, "/"); idx >= 0 {
		path = path[:idx]
	}
	if idx := strings.Index(path, "?"); idx >= 0 {
		path = path[:idx]
	}
	return path
}

func splitPathSegments(path string) []string {
	path = strings.TrimPrefix(path, "/")
	if qIdx := strings.Index(path, "?"); qIdx >= 0 {
		path = path[:qIdx]
	}
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
		mins, err := strconv.ParseFloat(parts[0], 64)
		if err != nil {
			return 0, err
		}
		secs, err := strconv.ParseFloat(parts[1], 64)
		if err != nil {
			return 0, err
		}
		return math.Floor(mins)*60 + secs, nil
	case 3:
		hours, err := strconv.ParseFloat(parts[0], 64)
		if err != nil {
			return 0, err
		}
		mins, err := strconv.ParseFloat(parts[1], 64)
		if err != nil {
			return 0, err
		}
		secs, err := strconv.ParseFloat(parts[2], 64)
		if err != nil {
			return 0, err
		}
		return math.Floor(hours)*3600 + math.Floor(mins)*60 + secs, nil
	}
	return 0, fmt.Errorf("invalid time format")
}
