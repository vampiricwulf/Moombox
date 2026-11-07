package tui

import (
	"fmt"
	"image/color"
	"math"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/table"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/vampiricwulf/Moombox/internal/utils"
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
	// Damaged and Segmented are the server's marks (GET /api/formats, the
	// adapter's pickerFormat): a rendition serving under half the video's
	// length, and one served only as fragments, which a whole-file VOD
	// cannot fetch. Each is a note in the picker's Note column — Segmented
	// only on a stream that is not live or upcoming (notesSegmented).
	Damaged   bool
	Segmented bool
}

// AudioFormat represents an audio format option.
type AudioFormat struct {
	Itag            int
	MimeType        string
	Bitrate         int
	AudioQuality    string
	AudioSampleRate string
	Damaged         bool // see VideoFormat
	Segmented       bool // see VideoFormat
}

// BestItags is the server's [Best …] pick per container/codec (GET
// /api/formats' bestItags), 0 for none. The server already passes damaged and
// segmented renditions over and falls back to the first of a kind when every
// one is either, so the picker shows its verdict rather than computing one.
// BestAudio is the audio rendition automatic selection takes, of any codec
// (owner decision FE-10) — a 5.1 E-AC-3 track that outranks both codec picks
// — noted "Best" beside the codec notes. BestVideo is its video half (owner
// decision PE-1): the video rendition automatic selection takes, of either
// container, under the operator's resolution cap.
type BestItags struct {
	BestWebmVideo int
	BestMp4Video  int
	BestOpusAudio int
	BestAacAudio  int
	BestAudio     int
	BestVideo     int
}

// FormatsData holds format info fetched from API.
type FormatsData struct {
	VideoID       string
	Title         string
	ChannelName   string
	LengthSeconds int
	VideoFormats  []VideoFormat
	AudioFormats  []AudioFormat
	// BestItags is the answer's own nested bestItags object. The four picks
	// were flat fields here, which the decode never filled — the API nests
	// them — so the TUI showed no Best note at all.
	BestItags BestItags
	// StreamStatus is the answer's streamStatus, youtube.StreamStatus as
	// the extraction called the video: "live", "upcoming", "vod",
	// "post_live" or "not_a_stream". The Note column reads it to leave the
	// segmented mark off a live or upcoming stream's rows (notesSegmented).
	StreamStatus string
}

// notesSegmented reports whether the picker marks a segmented rendition on a
// stream of status, the answer's streamStatus — the owner's decision on the
// picker's third follow-up. While a stream is live or upcoming every
// adaptive rendition it lists is segmented (live adaptive, served as &sq=N
// fragments), and the live path fetches exactly those, so the mark on every
// row said nothing a row could be told apart by. A VOD's and a finished
// stream's (post_live) segmented rows are the ones a whole-file download
// cannot fetch, and keep it, as does a status the answer did not state.
// Only the note is left off: the Best picks still pass a segmented
// rendition over, as the server's bestItags and the cursor's preselectedRow
// do. The dashboard reads the same status the same way
// (populateFormatSelects, web/public/app.js).
func notesSegmented(status string) bool {
	return status != "live" && status != "upcoming"
}

// preselectedRow is the row a format table's cursor opens on, hidden while
// the table is on Auto (owner decision PR-R8: Enter on a freshly opened
// table takes Auto, as the dashboard's untouched selects do) and shown there
// by the operator's first navigation key (startPicking) — the Best row the
// owner's decision on the picker's first follow-up put it on, so a choice
// starts from what Auto would take. picks are the server's Best
// itags in the dashboard's order: for video the [Best] pick (BestVideo,
// automatic selection's pick of either container — owner decision PE-1)
// ahead of WEBM ahead of MP4, which only an answer without BestVideo falls
// to, and, for audio, the
// [Best] pick (BestAudio, automatic selection's youtube.AudioRank pick, of
// any codec — owner decision FQ-1) ahead of Opus ahead of AAC, which only an
// answer without BestAudio falls to (0 for a pick the answer had none of):
// the first whose row is intact,
// neither damaged nor segmented, else the first with a row at all, as the
// dashboard falls back to a marked badge only when no badge is intact. With
// no Best row — no pick at all, or none matching a row — row 0, where the
// cursor always opened. row answers each row's itag and whether it is
// intact; n is the number of rows.
func preselectedRow(n int, row func(i int) (itag int, intact bool), picks ...int) int {
	for _, wantIntact := range []bool{true, false} {
		for _, pick := range picks {
			if pick == 0 {
				continue
			}
			for i := range n {
				if itag, intact := row(i); itag == pick && (intact || !wantIntact) {
					return i
				}
			}
		}
	}
	return 0
}

// formatNote is a picker row's Note: its Best label, if any, then the marks,
// joined as yt-dlp joins a format_note (_video.py:3453-3460, ", ").
func formatNote(best string, damaged, segmented bool) string {
	var parts []string
	if best != "" {
		parts = append(parts, best)
	}
	if damaged {
		parts = append(parts, "damaged")
	}
	if segmented {
		parts = append(parts, "segmented")
	}
	return strings.Join(parts, ", ")
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
	// fetchFailed marks a format fetch that came back with an error, so the
	// 2 s auto-advance to Confirm applies to that failure and nothing else
	// (an Esc back to the URL step clears it).
	fetchFailed bool

	// submitting is set while POST /api/jobs is in flight: Enter does not
	// post again (the second answer, a 409, used to overwrite "Added to
	// queue"), and only the dialog this flag is on is closed by the result.
	submitting bool

	// Format tables (built when formats arrive)
	videoTable table.Model
	audioTable table.Model
	// videoPicking / audioPicking: the operator has moved into that table
	// to choose a rendition. Until then the table is on Auto (owner
	// decision PR-R8): no row is drawn highlighted, and Enter takes Auto —
	// nothing is pinned unless chosen. The first navigation key only shows
	// the cursor where it opened, on the Best row (preselectedRow); the
	// next ones move it, and Enter then takes the highlighted row.
	videoPicking bool
	audioPicking bool

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
	m.fetchFailed = false
	m.submitting = false
	m.startTimeInput = ""
	m.endTimeInput = ""
	m.timeInputFocus = 0
	m.syncToTextInput()
}

// SetFormats is called after format data is fetched from the API.
func (m *AddVideoModel) SetFormats(f *FormatsData) {
	m.formats = f
	m.loading = false
	m.videoPicking, m.audioPicking = false, false
	m.buildFormatTables()
}

// autoTableStyles is a format table's styles while it is on Auto: the
// cursor's row drawn as any other, so no rendition reads as chosen.
func autoTableStyles() table.Styles {
	s := formatTableStyles()
	s.Selected = s.Cell
	return s
}

// startPicking leaves Auto for the step's table on the operator's first
// navigation key: the cursor, on the Best row, is drawn from now on and the
// key itself is spent showing it. It reports whether it did.
func (m *AddVideoModel) startPicking(msg tea.Msg) bool {
	kp, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return false
	}
	t, picking := &m.videoTable, &m.videoPicking
	if m.step == AddStepAudioFormat {
		t, picking = &m.audioTable, &m.audioPicking
	}
	if *picking {
		return false
	}
	km := t.KeyMap
	if !key.Matches(kp, km.LineUp, km.LineDown, km.PageUp, km.PageDown, km.HalfPageUp, km.HalfPageDown, km.GotoTop, km.GotoBottom) {
		return false
	}
	*picking = true
	t.SetStyles(formatTableStyles())
	return true
}

// noteColumnWidth fits the longest Note formatNote writes, "Best, Best
// WEBM, damaged, segmented" (automatic selection's pick, its container's
// pick too, both marks — the Best fallback when every rendition of a kind
// is both): the video table is then 69 columns wide, which the dialog's
// content (boxW-2, 78 at most) holds from a 75-column terminal up. Narrower,
// down to the TUI's minTermWidth, fitFormatTable gives the Note column what
// is left.
const noteColumnWidth = 35

// audioNoteColumnWidth is the audio table's: its longest Note is "Best, Best
// OPUS, damaged, segmented" (the automatic pick, its codec's pick too, both
// marks — the fallback when every rendition is both). Its other columns are
// 8 narrower than the video table's, so at this width it is still the
// narrower table (61 columns), and narrows the same way (fitFormatTable).
const audioNoteColumnWidth = 35

// audioCodecLabel is an audio row's Codec column: OPUS, AAC, and the 5.1
// codecs the shared rank can put first — EC-3 (E-AC-3) and AC-3 — named for
// what they are. The mime's container said nothing of them (an E-AC-3 track
// is audio/mp4, and its column read blank). Checked ahead of AAC: "ec-3" is
// no "mp4a", but the order keeps a future mp4a-prefixed surround codec out of
// the AAC label too.
func audioCodecLabel(mimeType string) string {
	codecs := strings.ToLower(mimeType)
	switch {
	case strings.Contains(codecs, "opus"):
		return "OPUS"
	case strings.Contains(codecs, "ec-3") || strings.Contains(codecs, "eac3"):
		return "EC-3"
	case strings.Contains(codecs, "ac-3"):
		return "AC-3"
	case strings.Contains(codecs, "mp4a") || strings.Contains(codecs, "aac"):
		return "AAC"
	}
	return ""
}

// buildFormatTables constructs bubbles/table models for video and audio format selection.
func (m *AddVideoModel) buildFormatTables() {
	if m.formats == nil {
		return
	}

	noteSegmented := notesSegmented(m.formats.StreamStatus)

	// Video table
	videoCols := []table.Column{
		{Title: "#", Width: 3},
		{Title: "Resolution", Width: 12},
		{Title: "FPS", Width: 4},
		{Title: "Codec", Width: 5},
		{Title: "Bitrate", Width: 10},
		{Title: "Note", Width: noteColumnWidth},
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
		// "Best" is automatic selection's pick (BestVideo, owner decision
		// PE-1), ahead of the codec pick the same row may also be.
		var bests []string
		if f.Itag == m.formats.BestItags.BestVideo {
			bests = append(bests, "Best")
		}
		if f.Itag == m.formats.BestItags.BestWebmVideo {
			bests = append(bests, "Best WEBM")
		} else if f.Itag == m.formats.BestItags.BestMp4Video {
			bests = append(bests, "Best MP4")
		}
		best := strings.Join(bests, ", ")
		videoRows = append(videoRows, table.Row{
			fmt.Sprintf("%d", i+1),
			fmt.Sprintf("%dx%d", f.Width, f.Height),
			fps, codec, bitrate, formatNote(best, f.Damaged, f.Segmented && noteSegmented),
		})
	}
	m.videoTable = table.New(
		table.WithColumns(videoCols),
		table.WithRows(videoRows),
		table.WithHeight(min(8, len(videoRows))),
		table.WithFocused(true),
		table.WithStyles(autoTableStyles()),
	)
	vf := m.formats.VideoFormats
	m.videoTable.SetCursor(preselectedRow(len(vf), func(i int) (int, bool) {
		return vf[i].Itag, !vf[i].Damaged && !vf[i].Segmented
	}, m.formats.BestItags.BestVideo, m.formats.BestItags.BestWebmVideo, m.formats.BestItags.BestMp4Video))

	// Audio table
	audioCols := []table.Column{
		{Title: "#", Width: 3},
		{Title: "Bitrate", Width: 10},
		{Title: "Rate", Width: 8},
		{Title: "Codec", Width: 5},
		{Title: "Note", Width: audioNoteColumnWidth},
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
		codec := audioCodecLabel(f.MimeType)
		var bests []string
		if f.Itag == m.formats.BestItags.BestAudio {
			bests = append(bests, "Best")
		}
		if f.Itag == m.formats.BestItags.BestOpusAudio {
			bests = append(bests, "Best OPUS")
		} else if f.Itag == m.formats.BestItags.BestAacAudio {
			bests = append(bests, "Best AAC")
		}
		best := strings.Join(bests, ", ")
		audioRows = append(audioRows, table.Row{
			fmt.Sprintf("%d", i+1),
			bitrate, sampleRate, codec, formatNote(best, f.Damaged, f.Segmented && noteSegmented),
		})
	}
	m.audioTable = table.New(
		table.WithColumns(audioCols),
		table.WithRows(audioRows),
		table.WithHeight(min(8, len(audioRows))),
		table.WithFocused(true),
		table.WithStyles(autoTableStyles()),
	)
	af := m.formats.AudioFormats
	m.audioTable.SetCursor(preselectedRow(len(af), func(i int) (int, bool) {
		return af[i].Itag, !af[i].Damaged && !af[i].Segmented
	}, m.formats.BestItags.BestAudio, m.formats.BestItags.BestOpusAudio, m.formats.BestItags.BestAacAudio))
}

// SetError is called when a format fetch fails.
func (m *AddVideoModel) SetError(err string) {
	m.errorMsg = err
	m.loading = false
}

// SetFetchError applies a failed format fetch: the error, and the mark the
// auto-advance to Confirm checks for.
func (m *AddVideoModel) SetFetchError(err string) {
	m.SetError(err)
	m.fetchFailed = true
}

// AwaitingFormats reports whether a format fetch for videoID is the one this
// dialog is waiting on. A result for anything else — an earlier ID the
// operator Esc'd away from, or a dialog since closed — is stale.
func (m *AddVideoModel) AwaitingFormats(videoID string) bool {
	return m.visible && m.loading && m.step == AddStepVideoFormat && m.videoID == videoID
}

// AutoAdvanceApplies reports whether the 2 s auto-advance armed by a failed
// fetch for videoID still applies: the dialog is still on the format step of
// that video, showing that failure.
func (m *AddVideoModel) AutoAdvanceApplies(videoID string) bool {
	return m.visible && m.fetchFailed && m.step == AddStepVideoFormat && m.videoID == videoID
}

// Submitting reports whether this dialog has a POST in flight for videoID.
func (m *AddVideoModel) Submitting(videoID string) bool {
	return m.visible && m.submitting && m.videoID == videoID
}

// UpdateComponents routes tea.Msg to embedded textinput/spinner/table and syncs.
func (m *AddVideoModel) UpdateComponents(msg tea.Msg) tea.Cmd {
	if !m.visible {
		return nil
	}
	var cmds []tea.Cmd
	// Route spinner tick when loading
	if m.loading || m.submitting {
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	// Route table events during format selection. A table still on Auto
	// spends the first navigation key showing its cursor (startPicking).
	if !m.loading && m.formats != nil && (m.step == AddStepVideoFormat || m.step == AddStepAudioFormat) && m.startPicking(msg) {
		return tea.Batch(cmds...)
	}
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
		loadTextInput(&m.textInput, *target)
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
	if m.submitting {
		// The POST is in flight: nothing but Esc, which closes the dialog
		// and leaves the answer to the feedback line.
		if key == keyEsc {
			m.Close()
		}
		return "", ""
	}
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
		// A fetch still in flight is abandoned: its result is dropped
		// (AwaitingFormats) and a failure's auto-advance disarmed.
		m.loading = false
		m.fetchFailed = false
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
		switch plat {
		case "twitch_clips":
			m.errorMsg = (&utils.TwitchTarget{Type: utils.TwitchClipsListing}).Unsupported()
			return "", ""
		case "twitch_listing":
			// A channel's video list, collection list or a collection (owner
			// decisions D18 and FE-27): the URL goes to the server as pasted —
			// its ?filter= and ?sort= are the server's to read — which queues
			// one VOD job per video.
		}
		if vid == "" {
			m.errorMsg = "Invalid video ID or URL"
			return "", ""
		}

		m.videoID = vid
		m.platform = plat

		// Twitch: no advanced options, submit directly with parsed ID.
		// YouTube without advanced mode likewise.
		if plat == "twitch" || !m.advancedEnabled {
			m.startSubmit()
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
		// Select the table's highlighted row — or Auto, while the operator
		// has not moved into the table to choose one (owner decision PR-R8).
		if m.formats == nil {
			return "", ""
		}
		if picking := m.videoPicking; !isVideo {
			picking = m.audioPicking
			if !picking {
				m.selectedAudioItag = nil
				m.step = AddStepTimestamps
				m.syncToTextInput()
				return "", ""
			}
		} else if !picking {
			m.selectedVideoItag = nil
			m.step = AddStepAudioFormat
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
		var s, e float64
		if m.startTimeInput != "" {
			var err error
			if s, err = parseTimeToSeconds(m.startTimeInput); err != nil {
				m.errorMsg = "Invalid start time format"
				return "", ""
			}
			if s < 0 {
				m.errorMsg = "Start time cannot be negative"
				return "", ""
			}
		}
		if m.endTimeInput != "" {
			var err error
			if e, err = parseTimeToSeconds(m.endTimeInput); err != nil {
				m.errorMsg = "Invalid end time format"
				return "", ""
			}
			// A blank start is the beginning of the video, so an end at or
			// before 0 is as empty a range as one before a typed start.
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
		m.startSubmit()
		return "submit", m.videoID
	}
	return "", ""
}

// startSubmit enters the in-flight state a submit holds until its result.
func (m *AddVideoModel) startSubmit() {
	m.submitting = true
	m.spinner = newSpinner()
	m.textInput.Blur()
}

// submittingLine is the hint row while a submit is in flight.
func (m *AddVideoModel) submittingLine() string {
	return m.spinner.View() + " Adding… " + DimStyle.Render("Esc: Close")
}

// GetSelectedVideoItag returns the selected video itag (nil=auto, -1=none).
func (m *AddVideoModel) GetSelectedVideoItag() *int { return m.selectedVideoItag }

// GetSelectedAudioItag returns the selected audio itag (nil=auto, -1=none).
func (m *AddVideoModel) GetSelectedAudioItag() *int { return m.selectedAudioItag }

// TimeRange returns the validated start and end, in seconds, for the POST
// body — nil for a blank field. POST /api/jobs decodes both as numbers, so
// the raw "1:30" the inputs hold was a 400 "invalid request body" for every
// add with a range. A zero start is no start at all and is left off, as the
// dashboard leaves it off.
func (m *AddVideoModel) TimeRange() (start, end *float64) {
	if s, err := parseTimeToSeconds(m.startTimeInput); err == nil && s > 0 {
		start = &s
	}
	if e, err := parseTimeToSeconds(m.endTimeInput); err == nil && e > 0 {
		end = &e
	}
	return start, end
}

// SpinnerInit returns the spinner's initial tick command when loading.
func (m *AddVideoModel) SpinnerInit() tea.Cmd { return spinnerTickCmd(m.spinner) }

// GetPlatform returns the detected platform.
func (m *AddVideoModel) GetPlatform() string { return m.platform }

// View renders the dialog.
func (m *AddVideoModel) View() string {
	if !m.visible {
		return ""
	}

	boxW := max(min(80, m.width-4), 30) // match TS: min(width, 80)
	boxH := max(min(30, m.height-4), 8) // match TS: min(height-4, 30)

	contentW := boxW - 2

	var borderColor color.Color
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
		Width(boxW).
		Height(boxH + 2).
		Render(content)

	return centerBox(box, m.width, m.height)
}

func (m *AddVideoModel) renderURLStep(w, _ int) string {
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
	tiStyles := m.textInput.Styles()
	tiStyles.Focused.Text = lipgloss.NewStyle().Foreground(color)
	tiStyles.Blurred.Text = lipgloss.NewStyle().Foreground(color)
	tiStyles.Cursor.Color = color
	m.textInput.SetStyles(tiStyles)
	m.textInput.SetWidth(w - 2)
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
	if m.submitting {
		lines = append(lines, m.submittingLine())
	} else {
		lines = append(lines, DimStyle.Render("Tab: Advanced | Enter: Continue | Esc: Cancel"))
	}

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
		lines = append(lines, YellowStyle.Render(m.errorMsg))
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
			fitFormatTable(&m.videoTable, w, min(tableH, len(m.formats.VideoFormats)), noteColumnWidth)
			lines = append(lines, m.videoTable.View())
		} else {
			fitFormatTable(&m.audioTable, w, min(tableH, len(m.formats.AudioFormats)), audioNoteColumnWidth)
			lines = append(lines, m.audioTable.View())
		}
	}

	lines = append(lines, "")
	if picking := m.videoPicking; (isVideo && !picking) || (!isVideo && !m.audioPicking) {
		lines = append(lines, DimStyle.Render("Enter/A: Auto | ↑↓: Choose a row | N: None | 1-9: Select | Esc: Back"))
	} else {
		lines = append(lines, DimStyle.Render("A: Auto | N: None | Enter/1-9: Select | Esc: Back"))
	}

	return strings.Join(lines, "\n")
}

// fitFormatTable sizes a format table to the dialog each time it is drawn,
// as its height always was. A table's rows are its viewport's, and a bubbles
// v2 viewport with no width draws nothing (viewport.View answers "" at width
// 0): built without one, both tables showed their header and its rule alone
// — no row at all — from the Charm v2 migration on. The width is the
// columns' own sum, the Note column (the last, the only one whose content
// varies in width) narrowed to what contentW leaves beside the others and
// never past maxNote (noteColumnWidth, or the audio table's
// audioNoteColumnWidth), so on a narrow terminal the table truncates a
// long note with "…" instead of the box wrapping every line of the table in
// two. The height is rows plus the header's lines, which SetHeight takes out
// of what it is given: rows alone showed two fewer — and nothing, for a
// table of two or fewer.
//
// Last, the cursor is brought into view. The table opens with it on its
// Best row (preselectedRow), placed by SetCursor, which leaves the
// viewport's offset where it was: the table draws from the cursor less the
// height on, so a cursor at or past the drawn height sat one row below the
// last row drawn, highlighted nowhere, until a key moved it. MoveDown(0)
// moves it nowhere and runs the table's own scroll rule for where it is,
// which scrolls a row below the drawn ones up into the last line and leaves
// a cursor already in view, the operator's own included, as it was.
func fitFormatTable(t *table.Model, contentW, rows, maxNote int) {
	cols := t.Columns()
	if len(cols) == 0 {
		return
	}
	note := &cols[len(cols)-1]
	others := 0
	for _, c := range cols[:len(cols)-1] {
		others += c.Width
	}
	note.Width = max(min(maxNote, contentW-others), len("Note"))
	t.SetColumns(cols)
	t.SetWidth(others + note.Width)
	t.SetHeight(rows + lipgloss.Height(formatTableStyles().Header.Render("Note")))
	t.MoveDown(0)
}

func (m *AddVideoModel) renderTimestamps(w, _ int) string {
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

func (m *AddVideoModel) renderConfirm(w, _ int) string {
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
	if m.submitting {
		lines = append(lines, m.submittingLine())
	} else {
		lines = append(lines, DimStyle.Render("Enter: Submit | Esc: Back"))
	}

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

	// Explicit Twitch VOD id ("v" + digits) before the YouTube check —
	// current VOD ids are 10 digits, so "v" + 10 digits is 11 chars and
	// would otherwise match the generic 11-char YouTube id pattern.
	if len(input) >= 2 && len(input) <= 13 && input[0] == 'v' && isNumeric(input[1:]) {
		return "tw_v" + input[1:], "twitch"
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

// extractTwitchTarget extracts a Twitch channel, VOD, clip or listing from a
// URL or raw input. Returns (id, type) where type is "twitch" (id a login,
// "tw_v"+VOD ID, or "tw_clip_"+clip slug — the clip job's own ID, owner
// decision D26), "twitch_listing" (a video list or collection; id is the
// input itself, which the add posts as the listing's URL), "twitch_clips"
// (a clip list, refused, id empty), or
// "". The parse is utils.ExtractTwitchTarget's — the one URL table the
// daemon's CLI and the dashboard follow too (internal/utils/testdata/
// twitch_urls.json) — rather than a copy that knew only www and bare hosts,
// /videos/ and /<ch>/video/, so /<ch>/v/<id>, schedule?vodID=, m./go./player.
// links and listings no longer became a channel's live-wait job.
func extractTwitchTarget(input string) (string, string) {
	target := utils.ExtractTwitchTarget(input)
	if target == nil {
		return "", ""
	}
	switch target.Type {
	case utils.TwitchChannel:
		return target.Value, "twitch"
	case utils.TwitchVOD:
		return "tw_v" + target.Value, "twitch"
	case utils.TwitchClip:
		return "tw_clip_" + target.Value, "twitch"
	case utils.TwitchVideosListing, utils.TwitchCollection:
		return strings.TrimSpace(input), "twitch_listing"
	}
	return "", "twitch_clips"
}

func isVideoIDChar(s string) bool {
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
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

// validClockFields reports whether the minutes and seconds of an MM:SS or
// HH:MM:SS time are on the clock face: whole minutes 0-59 and seconds in
// [0, 60) — "1:59.5" is a time, and the old `secs > 59` turned it away. Written
// as a range the value must fall INSIDE so a NaN or ±Inf ("1:NaN") is refused
// rather than slipping past two comparisons that are both false for it.
func validClockFields(mins int, secs float64) bool {
	return mins >= 0 && mins <= 59 && secs >= 0 && secs < 60
}

// parseTimeToSeconds parses HH:MM:SS, MM:SS, or raw seconds to float64. The
// result is always finite; a negative one (a raw "-5", or "-1:00:00") is
// returned as such for the caller to refuse with its own message.
func parseTimeToSeconds(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty time")
	}

	parts := strings.Split(s, ":")
	switch len(parts) {
	case 1:
		secs, err := strconv.ParseFloat(parts[0], 64)
		if err != nil {
			return 0, err
		}
		// ParseFloat takes "NaN" and "Inf" — neither is a time, and NaN
		// slips through every range comparison a caller makes after this.
		if math.IsNaN(secs) || math.IsInf(secs, 0) {
			return 0, fmt.Errorf("not a finite number of seconds")
		}
		return secs, nil
	case 2:
		mins, err := strconv.Atoi(parts[0])
		if err != nil {
			return 0, err
		}
		secs, err := strconv.ParseFloat(parts[1], 64)
		if err != nil {
			return 0, err
		}
		if !validClockFields(mins, secs) {
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
		if !validClockFields(mins, secs) {
			return 0, fmt.Errorf("minutes and seconds must be 0-59")
		}
		return float64(hours)*3600 + float64(mins)*60 + secs, nil
	}
	return 0, fmt.Errorf("invalid time format")
}
