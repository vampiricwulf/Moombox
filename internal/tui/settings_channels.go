package tui

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// --- Channel sub-editor ---

func (m *SettingsModel) visibleChannelFields() []channelFieldDef {
	return filterChannelFieldsByPlatform(channelFields, m.channelEditValues)
}

func channelToValues(ch config.ChannelConfig) map[string]string {
	terms := ch.Terms.Simple
	enabled := "Yes"
	if ch.Enabled != nil && !*ch.Enabled {
		enabled = "No"
	}
	return map[string]string{
		"id":                  ch.ID,
		"name":                ch.Name,
		"platform":            ch.GetPlatform(),
		"enabled":             enabled,
		"terms":               terms,
		"include_non_live":    boolToDisplay(ch.IncludeNonLiveContent),
		"quality_preference":  cmp.Or(ch.QualityPreference, "best"),
		"output_directory":    ch.OutputDirectory,
		"archive_window_days": optIntString(ch.ArchiveWindowDays),
		"archive_slots":       optIntString(ch.ArchiveSlots),
	}
}

// optIntString renders an optional override for the form: "" when unset.
func optIntString(p *int) string {
	if p == nil {
		return ""
	}
	return strconv.Itoa(*p)
}

// optIntFromString parses a form value back to an optional override; blank
// or unparseable text clears it (validateChannelValues rejects the latter
// before the save reaches here).
func optIntFromString(s string) *int {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return nil
	}
	return &n
}

// valuesToChannel turns the editor's form values into a ChannelConfig. For an
// edit, existing is the channel being edited: every field is copied from it
// first, so the fields this editor does not show (named terms, and the
// retired num_desc_lookbehind) survive
// the round trip — the bug this parameter exists to close. nil means a new
// channel. Shown fields are assigned unconditionally so clearing one clears
// it in the result too.
func valuesToChannel(vals map[string]string, existing *config.ChannelConfig) config.ChannelConfig {
	var ch config.ChannelConfig
	if existing != nil {
		ch = *existing
	}
	ch.ID = strings.TrimSpace(vals["id"])
	ch.Name = strings.TrimSpace(vals["name"])
	ch.Platform = vals["platform"]
	switch vals["enabled"] {
	case "No":
		boolFalse := false
		ch.Enabled = &boolFalse
	case "Yes":
		boolTrue := true
		ch.Enabled = &boolTrue
	}
	// The editor shows one pattern string. Unchanged text keeps whatever
	// shape the config had (a named map shows as its Simple, ""); changed
	// text becomes the simple form, and "" clears the terms.
	if existing == nil || vals["terms"] != channelToValues(*existing)["terms"] {
		ch.Terms = config.ChannelTerms{}
		if vals["terms"] != "" {
			ch.Terms = config.ChannelTerms{Simple: vals["terms"]}
		}
	}
	ch.IncludeNonLiveContent = vals["platform"] == "youtube" && vals["include_non_live"] == "Yes"
	ch.QualityPreference = ""
	if q := vals["quality_preference"]; q != "" && q != "best" {
		ch.QualityPreference = q
	}
	ch.OutputDirectory = strings.TrimSpace(vals["output_directory"])
	ch.ArchiveWindowDays = optIntFromString(vals["archive_window_days"])
	ch.ArchiveSlots = optIntFromString(vals["archive_slots"])
	return ch
}

// validateChannelValues checks the numeric overrides before a save so a
// typo produces a field error instead of a silently-cleared override.
// Bounds match config.Validate's global monitors bounds.
func validateChannelValues(vals map[string]string) string {
	check := func(key, label string, min, max int) string {
		s := strings.TrimSpace(vals[key])
		if s == "" {
			return ""
		}
		n, err := strconv.Atoi(s)
		if err != nil || n < min || n > max {
			return fmt.Sprintf("%s must be a whole number %d-%d (blank = use the global/default value)", label, min, max)
		}
		return ""
	}
	if msg := check("archive_window_days", "Archive window", 1, 3650); msg != "" {
		return msg
	}
	return check("archive_slots", "Archive slots", 1, 100)
}

func (m *SettingsModel) handleChannelKey(key string) string {
	if m.channelMode == "edit" {
		return m.handleChannelEditKey(key)
	}

	// List mode. The removal prompt (settings_channel_removal.go) owns
	// every key while it is up.
	if m.channelDeleteConf {
		return m.handleChannelRemovalKey(key)
	}

	switch key {
	case keyEsc:
		return m.handleClose()
	case keyUp:
		if m.channelIndex > 0 {
			m.channelIndex--
		}
	case keyDown:
		if m.channelIndex < len(m.channels)-1 {
			m.channelIndex++
		}
	case keyEnter:
		if len(m.channels) > 0 && m.channelIndex < len(m.channels) {
			m.channelMode = "edit"
			m.channelEditValues = channelToValues(m.channels[m.channelIndex])
			m.channelEditField = 0
			m.updateTextInputForField()
		}
	case "a", "A":
		m.channelEditValues = map[string]string{
			"id": "", "name": "", "platform": "youtube",
			"enabled": "Yes", "terms": "",
			"include_non_live": "No", "quality_preference": "best",
			"output_directory":    "",
			"archive_window_days": "", "archive_slots": "",
		}
		m.channelEditField = 0
		m.channelIndex = len(m.channels) // Will be new index
		m.channelMode = "edit"
		m.updateTextInputForField()
	case "d", "D":
		if len(m.channels) > 0 && m.channelIndex < len(m.channels) {
			return m.beginChannelRemoval()
		}
	case keyTab:
		m.switchSection((m.sectionIndex + 1) % len(sections))
		m.updateTextInputForField()
	case "shift+left":
		if m.sectionIndex > 0 {
			m.switchSection(m.sectionIndex - 1)
			m.updateTextInputForField()
		}
	case "shift+right":
		if m.sectionIndex < len(sections)-1 {
			m.switchSection(m.sectionIndex + 1)
			m.updateTextInputForField()
		}
	}
	return ""
}

func (m *SettingsModel) handleChannelEditKey(key string) string {
	fields := m.visibleChannelFields()
	if len(fields) == 0 {
		return ""
	}
	if m.channelEditField >= len(fields) {
		m.channelEditField = len(fields) - 1
	}
	field := fields[m.channelEditField]

	switch key {
	case keyEsc:
		m.channelMode = "list"
		// "a" set channelIndex = len(channels) for the pending add — clamp
		// it back or list-mode Enter indexes out of range (mirrors the
		// setup wizard's handleChannelEditKey Esc).
		if m.channelIndex >= len(m.channels) && len(m.channels) > 0 {
			m.channelIndex = len(m.channels) - 1
		} else if len(m.channels) == 0 {
			m.channelIndex = 0
		}
		m.channelResolving = false
		m.textInput.Blur()
		return ""
	case keyEnter:
		if m.channelResolving {
			return ""
		}
		id := strings.TrimSpace(m.channelEditValues["id"])
		if id == "" {
			m.errorMsg = "Channel ID is required"
			return ""
		}
		if msg := validateChannelValues(m.channelEditValues); msg != "" {
			m.errorMsg = msg
			m.status = saveError
			return ""
		}
		// A URL or a bare @handle is resolved first, off the update loop
		// (resolveChannelCmd → HandleChannelResolved); a bare handle used
		// to be saved as typed, an ID no monitor could poll.
		if utils.NeedsChannelResolve(id) {
			m.channelResolving = true
			return "resolve_channel"
		}
		if channelIDTaken(m.channels, m.channelIndex, id) {
			m.errorMsg = fmt.Sprintf("Channel %q already added", id)
			m.status = saveError
			return ""
		}
		m.saveCurrentChannel()
		return ""
	case keyUp:
		if m.channelEditField > 0 {
			m.channelEditField--
			m.updateTextInputForField()
		}
		return ""
	case keyDown:
		if m.channelEditField < len(fields)-1 {
			m.channelEditField++
			m.updateTextInputForField()
		}
		return ""
	case keyLeft:
		if field.ftype == fieldToggle || field.ftype == fieldCycle {
			m.cycleChannelOption(field, -1)
		}
		return ""
	case keyRight:
		if field.ftype == fieldToggle || field.ftype == fieldCycle {
			m.cycleChannelOption(field, 1)
		}
		return ""
	}
	return ""
}

// autoDetectPlatform checks the ID field value and auto-switches the platform if it contains a known domain.
func (m *SettingsModel) autoDetectPlatform() {
	id := strings.ToLower(m.channelEditValues["id"])
	if strings.HasPrefix(strings.TrimSpace(id), "@") || strings.Contains(id, "youtube.com/") || strings.Contains(id, "youtu.be/") {
		m.channelEditValues["platform"] = "youtube"
	} else if strings.Contains(id, "twitch.tv/") {
		m.channelEditValues["platform"] = "twitch"
	}
}

// saveCurrentChannel saves the current channel edit values to the channel list.
func (m *SettingsModel) saveCurrentChannel() {
	var existing *config.ChannelConfig
	if m.channelIndex < len(m.channels) {
		existing = &m.channels[m.channelIndex]
	}
	ch := valuesToChannel(m.channelEditValues, existing)
	// An edit that changed nothing the form shows keeps the entry exactly
	// as it was. valuesToChannel spells defaults out (platform "youtube",
	// enabled true), and a spelled-out default reads as an edit to
	// mergeChannelEdits — which adds back a channel the dashboard removed
	// since the overlay opened.
	if existing != nil && maps.Equal(channelToValues(ch), channelToValues(*existing)) {
		ch = *existing
	}
	if m.channelIndex < len(m.channels) {
		m.channels[m.channelIndex] = ch
	} else {
		m.channels = append(m.channels, ch)
	}
	m.dirty = true
	// structDirty keeps recheckDirty from clearing the dirty flag — channel
	// edits aren't reflected in m.values, so a value-level recheck would
	// otherwise silently discard the add/edit on close.
	m.structDirty = true
	m.status = saveIdle
	m.channelMode = "list"
}

// mergeChannelEdits applies the Settings editor's own channel changes to
// the channel list as it stands at save time. base is the list Open copied,
// edited the list the editor holds now, live the store's list now; the
// result is a new slice (never live's array, which Snapshot readers and a
// rollback share), and changed is false — live returned as is — when the
// editor changed nothing.
//
// Matching is by channel ID, case-insensitively as config.Validate compares
// them. A base ID the editor no longer has (deleted, or renamed away) is
// removed from live. An entry the editor changed from its base copy is
// merged field by field into live's entry with that ID (mergeChannelFields):
// only the fields the editor changed are written, so a dashboard change to
// another field of the same channel — a disable, an output directory — made
// while the overlay was open survives. An entry with no base copy (added,
// or renamed to a new ID) replaces live's entry with that ID whole, and so
// does an edit of a channel the dashboard removed meanwhile, which is
// appended: the operator saved it on purpose. Everything else in live — a
// channel the dashboard added, disabled or edited, and one it removed that
// the editor did not touch — stays exactly as live has it.
func mergeChannelEdits(base, edited, live []config.ChannelConfig) ([]config.ChannelConfig, bool) {
	key := func(id string) string { return strings.ToLower(strings.TrimSpace(id)) }
	atOpen := make(map[string]config.ChannelConfig, len(base))
	for _, ch := range base {
		atOpen[key(ch.ID)] = ch
	}
	type upsert struct {
		ch      config.ChannelConfig
		was     config.ChannelConfig
		hasBase bool
	}
	kept := make(map[string]bool, len(edited))
	var upserts []upsert
	for _, ch := range edited {
		kept[key(ch.ID)] = true
		was, ok := atOpen[key(ch.ID)]
		if ok && reflect.DeepEqual(was, ch) {
			continue
		}
		upserts = append(upserts, upsert{ch: ch, was: was, hasBase: ok})
	}
	removed := make(map[string]bool)
	for _, ch := range base {
		if !kept[key(ch.ID)] {
			removed[key(ch.ID)] = true
		}
	}
	if len(removed) == 0 && len(upserts) == 0 {
		return live, false
	}

	out := make([]config.ChannelConfig, 0, len(live)+len(upserts))
	for _, ch := range live {
		if !removed[key(ch.ID)] {
			out = append(out, ch)
		}
	}
	for _, u := range upserts {
		i := slices.IndexFunc(out, func(c config.ChannelConfig) bool { return key(c.ID) == key(u.ch.ID) })
		switch {
		case i >= 0 && u.hasBase:
			out[i] = mergeChannelFields(out[i], u.was, u.ch)
		case i >= 0:
			out[i] = u.ch
		default:
			out = append(out, u.ch)
		}
	}
	return out, true
}

// mergeChannelFields is the three-way merge of one channel: live as it
// stands at save time, with each form field the editor changed — where
// edited differs from base, the copy Open took, compared as the form shows
// them (channelToValues) — taken from edited. A field the editor left alone
// keeps live's value, whatever the dashboard set it to meanwhile, and so
// does every field the form does not show. Writing the editor's entry whole
// put the overlay's Open-time copy of every other field over the
// dashboard's: a TUI rename re-enabled a channel disabled on the dashboard,
// or cleared the output directory set there.
func mergeChannelFields(live, base, edited config.ChannelConfig) config.ChannelConfig {
	was, now := channelToValues(base), channelToValues(edited)
	for k := range was {
		if was[k] != now[k] {
			writeChannelField(&live, edited, k)
		}
	}
	return live
}

// writeChannelField writes the form field key — one of channelToValues'
// keys — from src onto dst, and reports false for a key it does not know.
func writeChannelField(dst *config.ChannelConfig, src config.ChannelConfig, key string) bool {
	switch key {
	case "id":
		dst.ID = src.ID
	case "name":
		dst.Name = src.Name
	case "platform":
		dst.Platform = src.Platform
	case "enabled":
		dst.Enabled = src.Enabled
	case "terms":
		dst.Terms = src.Terms
	case "include_non_live":
		dst.IncludeNonLiveContent = src.IncludeNonLiveContent
	case "quality_preference":
		dst.QualityPreference = src.QualityPreference
	case "output_directory":
		dst.OutputDirectory = src.OutputDirectory
	case "archive_window_days":
		dst.ArchiveWindowDays = src.ArchiveWindowDays
	case "archive_slots":
		dst.ArchiveSlots = src.ArchiveSlots
	default:
		return false
	}
	return true
}

// GetChannelResolveInput returns the current channel ID being resolved.
func (m *SettingsModel) GetChannelResolveInput() string {
	if m.channelEditValues == nil {
		return ""
	}
	return strings.TrimSpace(m.channelEditValues["id"])
}

// HandleChannelResolved processes the result of an async channel resolution.
// If the user cancelled (Esc) before the result arrived, the result is
// silently discarded, and so is one for text the ID box no longer holds —
// typed over while the lookup ran; Enter resolves what is there now. An
// input that names no channel is refused with ErrNotChannelURL's sentence
// instead of saved, and a resolved ID another entry already has is refused
// as the plain-ID path refuses it.
func (m *SettingsModel) HandleChannelResolved(input, id, name, platform string, err error) {
	if !m.channelResolving {
		return // User cancelled or navigated away — discard stale result
	}
	m.channelResolving = false
	if input != m.GetChannelResolveInput() {
		return
	}
	if err != nil {
		m.errorMsg = channelResolveError(err)
		m.status = saveError
		return
	}
	m.channelEditValues["id"] = id
	if name != "" && m.channelEditValues["name"] == "" {
		m.channelEditValues["name"] = name
	}
	if platform != "" {
		m.channelEditValues["platform"] = platform
	}
	if channelIDTaken(m.channels, m.channelIndex, id) {
		m.errorMsg = fmt.Sprintf("Channel %q already added", id)
		m.status = saveError
		// The box shows the resolved ID, the one that collides.
		m.updateTextInputForField()
		return
	}
	m.saveCurrentChannel()
}

// channelIDTaken reports whether a channel in chs other than the one at
// index self already has id, compared case-insensitively — the rule
// config.Validate refuses a duplicate by. Shared by the Settings editor and
// the setup wizard, which used to be the only one of the two to check.
func channelIDTaken(chs []config.ChannelConfig, self int, id string) bool {
	id = strings.TrimSpace(id)
	for i, ch := range chs {
		if i != self && strings.EqualFold(strings.TrimSpace(ch.ID), id) {
			return true
		}
	}
	return false
}

// channelResolveError words a failed resolve for the channel editors: an
// input that names no channel gets ErrNotChannelURL's sentence, the one POST
// /api/config/channels answers; anything else is the lookup's own failure.
func channelResolveError(err error) string {
	if errors.Is(err, utils.ErrNotChannelURL) {
		return "Channel ID: " + err.Error()
	}
	return "Resolve failed: " + err.Error()
}

func (m *SettingsModel) cycleChannelOption(field channelFieldDef, direction int) {
	cycleFieldOption(m.channelEditValues, field.key, field.options, direction)
}
