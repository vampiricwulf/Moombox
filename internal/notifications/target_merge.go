package notifications

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// The merge both Settings editors save their notification-target changes
// through: the TUI overlay (internal/tui, applyValues) and the dashboard's
// cards (PUT /api/config's notification_edits). Each holds a copy of the
// target list it read, and a save writes only that editor's own changes into
// the live list as it stands, so neither reverts what the other changed
// meanwhile.

// TargetFormValues is a target as the editors write it, each field in a form
// that tells apart exactly what the manager tells apart: the URL as stored
// (the manager refuses a padded one, so trimming it is an edit), the mute as
// IsEnabled reads it, the event filter as a set (targetFormEvents), the
// canonical mention, the mention filter as a set (or "default", absent), and
// the delivery mode with absent reading separate. Two targets with equal form
// values differ only in spelling, so a save that compared the stored structs
// would read an edit that changed nothing — the TUI editor's Enter spells an
// absent enabled out as true — as an edit; and a form that hid a difference
// the manager acts on would drop a real edit as none.
func TargetFormValues(n config.NotificationConfig) map[string]string {
	mention := strings.TrimSpace(n.Mention)
	if canonical, _, _, err := config.ParseMention(n.Mention); err == nil {
		mention = canonical
	}
	mentionEvents := "default"
	if n.MentionEvents != nil {
		list := slices.Clone(*n.MentionEvents)
		slices.Sort(list)
		mentionEvents = "[" + strings.Join(slices.Compact(list), ",") + "]"
	}
	mode := "separate"
	if strings.TrimSpace(n.Mode) == "edit" {
		mode = "edit"
	}
	return map[string]string{
		"url":            n.URL,
		"enabled":        strconv.FormatBool(n.IsEnabled()),
		"events":         targetFormEvents(n.Events),
		"mention":        mention,
		"mention_events": mentionEvents,
		"mode":           mode,
	}
}

// vocabularySorted is EventGroups' events, sorted: the vocabulary both
// editors offer a row or a chip for.
var vocabularySorted = func() []string {
	var out []string
	for _, g := range EventGroups {
		out = append(out, g.Events...)
	}
	slices.Sort(out)
	return out
}()

// targetFormEvents is an event filter as TargetFormValues compares it: "*"
// for every event — no filter, or one naming each vocabulary event and
// nothing else — and otherwise the names it holds, as a sorted set. A filter
// naming only events outside the vocabulary is its own value, not "*": the
// manager reads it as a filter (one naming a retired key matches that key's
// alias; connectivity_lost matches nothing), so turning it into "every event"
// is an edit.
func targetFormEvents(events []string) string {
	if len(events) == 0 {
		return "*"
	}
	set := slices.Clone(events)
	slices.Sort(set)
	set = slices.Compact(set)
	if slices.Equal(set, vocabularySorted) {
		return "*"
	}
	return fmt.Sprintf("%q", set)
}

// WriteTargetField writes the form field key — one of TargetFormValues'
// keys — from src onto dst, and reports false for a key it does not know.
// The slices and pointers are copied, never shared: dst goes into the live
// config, src stays with the editor.
func WriteTargetField(dst *config.NotificationConfig, src config.NotificationConfig, key string) bool {
	switch key {
	case "url":
		dst.URL = src.URL
	case "enabled":
		dst.Enabled = nil
		if src.Enabled != nil {
			on := *src.Enabled
			dst.Enabled = &on
		}
	case "events":
		dst.Events = slices.Clone(src.Events)
	case "mention":
		dst.Mention = src.Mention
	case "mention_events":
		dst.MentionEvents = nil
		if src.MentionEvents != nil {
			list := append([]string{}, *src.MentionEvents...)
			dst.MentionEvents = &list
		}
	case "mode":
		dst.Mode = src.Mode
	default:
		return false
	}
	return true
}

// CloneTarget copies n with nothing shared.
func CloneTarget(n config.NotificationConfig) config.NotificationConfig {
	out := n
	for k := range TargetFormValues(n) {
		WriteTargetField(&out, n, k)
	}
	return out
}

// TargetIdentities names each target of list by its trimmed URL and how many
// earlier entries carry the same one: the URL is all a target has to be known
// by, and a webhook listed twice (which the manager posts to once) is still
// two entries to the editors.
func TargetIdentities(list []config.NotificationConfig) []string {
	seen := make(map[string]int, len(list))
	ids := make([]string, len(list))
	for i, n := range list {
		url := strings.TrimSpace(n.URL)
		ids[i] = url + "#" + strconv.Itoa(seen[url])
		seen[url]++
	}
	return ids
}

// MergeTargetEdits applies one editor's own target changes to the target
// list as it stands at save time — the TUI's channel merge rule
// (mergeChannelEdits), with a target's identity in place of a channel ID.
// base is the list the editor read, edited the list it holds now, from the
// base index each edited entry was opened from (-1, or past its end: added in
// the editor), live the store's list now. The result is a new slice (never
// live's array, which Snapshot readers and a rollback share), and changed is
// false — live returned as is — when the editor changed nothing.
//
// A target is known by the URL it had in base (TargetIdentities), so one
// whose URL the editor changed is still the same target. A base target the
// editor deleted is removed from live. One it changed — compared by
// TargetFormValues, which tells apart what the manager does — is merged
// field by field into live's target with that identity: only the fields the
// editor changed are written, so the other editor's mute or mention on the
// same target survives an edit of its events, and a field both changed takes
// this editor's value, the save being the later write. An edit of a target
// the other editor removed meanwhile is added back, the operator having saved
// it on purpose. A target this editor added replaces one the other added with
// the same URL meanwhile — one webhook, listed once — and is appended
// otherwise. Everything else in live — a target the other editor added, muted
// or edited, and one it removed that this editor did not touch — stays
// exactly as live has it.
func MergeTargetEdits(base, edited []config.NotificationConfig, from []int, live []config.NotificationConfig) ([]config.NotificationConfig, bool) {
	baseIDs := TargetIdentities(base)
	type upsert struct {
		n, was  config.NotificationConfig
		id      string // the base identity; unused for an added target
		hasBase bool
	}
	kept := make([]bool, len(base))
	var upserts []upsert
	for i, n := range edited {
		f := -1
		if i < len(from) {
			f = from[i]
		}
		if f < 0 || f >= len(base) || kept[f] {
			upserts = append(upserts, upsert{n: n})
			continue
		}
		kept[f] = true
		if maps.Equal(TargetFormValues(base[f]), TargetFormValues(n)) {
			continue
		}
		upserts = append(upserts, upsert{n: n, was: base[f], id: baseIDs[f], hasBase: true})
	}
	removed := make(map[string]bool)
	inBase := make(map[string]bool, len(base))
	for i, id := range baseIDs {
		inBase[id] = true
		if !kept[i] {
			removed[id] = true
		}
	}
	if len(removed) == 0 && len(upserts) == 0 {
		return live, false
	}

	// outIDs follows out: each kept live target's identity, "" once an added
	// target has replaced it or for one appended, so two added targets with
	// one URL do not both land on the same entry the other editor added.
	out := make([]config.NotificationConfig, 0, len(live)+len(upserts))
	outIDs := make([]string, 0, cap(out))
	for i, id := range TargetIdentities(live) {
		if !removed[id] {
			out = append(out, live[i])
			outIDs = append(outIDs, id)
		}
	}
	for _, u := range upserts {
		i := -1
		if u.hasBase {
			i = slices.Index(outIDs, u.id)
		} else {
			url := strings.TrimSpace(u.n.URL)
			for j, id := range outIDs {
				if id != "" && !inBase[id] && strings.TrimSpace(out[j].URL) == url {
					i = j
					break
				}
			}
		}
		switch {
		case i >= 0 && u.hasBase:
			out[i] = mergeTargetFields(out[i], u.was, u.n)
		case i >= 0:
			out[i] = CloneTarget(u.n)
			outIDs[i] = ""
		default:
			out = append(out, CloneTarget(u.n))
			outIDs = append(outIDs, "")
		}
	}
	return out, true
}

// mergeTargetFields is the three-way merge of one target: live as it stands
// at save time, with each form field the editor changed — where edited
// differs from base, the copy the editor read, compared by TargetFormValues —
// taken from edited. A field the editor left alone keeps live's value,
// whatever the other editor set it to meanwhile.
func mergeTargetFields(live, base, edited config.NotificationConfig) config.NotificationConfig {
	was, now := TargetFormValues(base), TargetFormValues(edited)
	for k := range was {
		if was[k] != now[k] {
			WriteTargetField(&live, edited, k)
		}
	}
	return live
}
