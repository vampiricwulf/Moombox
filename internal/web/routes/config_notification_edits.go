package routes

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"net/http"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/notifications"
)

// A dashboard Settings save writes only what the page changed (owner
// decision, the TUI's rule). The page sends the fields edited since it loaded
// or last saved, and PUT /api/config applies each key on its own, so a field
// the page did not touch keeps the live value — whatever the TUI saved
// meanwhile. The notification targets cannot be sent that way: a list sent
// whole puts the page's copy of every target back. The page sends its own
// target changes instead (notification_edits), and the server merges them
// into the live list by each target's identity, through the merge the TUI's
// save uses (notifications.MergeTargetEdits).

// notificationEdits is PUT /api/config's notification_edits:
//
//	{"base": [...], "edited": [...], "from": [...]}
//
// base is the target list as the editor last read it (GET /api/config, or
// the list a previous save answered with), edited the list as it holds it
// now, and from, for each edited entry, the index of the base entry it was
// edited from, or -1 for one it added; a base index no entry names was
// deleted.
type notificationEdits struct {
	base, edited []config.NotificationConfig
	from         []int
}

// notificationsReplacedWhole refuses PUT /api/config's whole-list
// notifications: undocumented as an API, sent by nothing since the dashboard
// moved to notification_edits, and what reverted every target change made
// elsewhere while a page was open.
const notificationsReplacedWhole = "the target list is not replaced whole: send notification_edits ({base, edited, from}) with your own changes"

// decodeNotificationEdits reads updates' notification_edits, normalised as
// a stored list is (normalizeNotificationEntries) — base and edited alike,
// so a target the editor did not touch compares equal to its own base copy
// and is not rewritten. It returns nil with no errors when the body carries
// none, and nil with the field errors when it is malformed: edited's keyed
// notifications[i].<field>, after the editor's own list.
func decodeNotificationEdits(updates map[string]any) (*notificationEdits, map[string]string) {
	raw, present := updates["notification_edits"]
	if !present {
		return nil, nil
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, map[string]string{"notification_edits": "must be an object {base, edited, from}"}
	}
	errs := map[string]string{}
	base, baseOK := obj["base"].([]any)
	if !baseOK {
		errs["notification_edits.base"] = "must be an array of targets (the list as last read)"
	}
	edited, editedOK := obj["edited"].([]any)
	if !editedOK {
		errs["notification_edits.edited"] = "must be an array of targets (the list as edited)"
	}
	from, fromOK := obj["from"].([]any)
	if !fromOK {
		errs["notification_edits.from"] = "must be an array of base indexes, -1 for an added target"
	}
	if len(errs) > 0 {
		return nil, errs
	}

	var e notificationEdits
	var decErrs map[string]string
	if e.base, decErrs = decodeConfigEntries[config.NotificationConfig]("notification_edits.base", base); decErrs != nil {
		maps.Copy(errs, decErrs)
	}
	maps.Copy(errs, notificationEntryErrors("notifications", edited))
	if len(errs) == 0 {
		e.edited, _ = decodeConfigEntries[config.NotificationConfig]("notifications", edited)
	}
	if msg := notificationEditsFromError(from, len(edited), len(base)); msg != "" {
		errs["notification_edits.from"] = msg
	}
	if len(errs) > 0 {
		return nil, errs
	}
	e.from = make([]int, len(from))
	for i, f := range from {
		e.from[i] = int(f.(float64))
	}
	normalizeNotificationEntries(e.base)
	normalizeNotificationEntries(e.edited)
	return &e, nil
}

// notificationEditsFromError checks from against edited's length and base's:
// one integer per edited entry, each -1 or a base index no other entry names.
func notificationEditsFromError(from []any, editedLen, baseLen int) string {
	if len(from) != editedLen {
		return fmt.Sprintf("must name one base index per edited target: %d for %d", len(from), editedLen)
	}
	seen := make(map[int]bool, len(from))
	for i, raw := range from {
		v, ok := raw.(float64)
		if !ok || v != math.Trunc(v) || v < -1 || v >= float64(baseLen) {
			return fmt.Sprintf("[%d] must be -1 or an index into base, which has %d entries", i, baseLen)
		}
		if f := int(v); f >= 0 {
			if seen[f] {
				return fmt.Sprintf("[%d] names base[%d] a second time", i, f)
			}
			seen[f] = true
		}
	}
	return ""
}

// mergeNotificationEdits merges e into live (the store's list, under its
// write lock) and checks what the merge brings in. A target whose URL the
// live list does not already carry — added, or its URL edited — must parse
// (notifications.ValidateURL), as a pasted one always had to; one the live
// list carries is grandfathered, a hand-edited legacy URL the manager
// warn-skips at boot included. changed is false, and the list nil, when the
// edits change nothing in live.
func mergeNotificationEdits(e *notificationEdits, live []config.NotificationConfig) ([]config.NotificationConfig, bool, map[string]string) {
	merged, changed := notifications.MergeTargetEdits(e.base, e.edited, e.from, live)
	if !changed {
		return nil, false, nil
	}
	known := make(map[string]bool, len(live))
	for _, n := range live {
		known[n.URL] = true
	}
	errs := map[string]string{}
	for _, n := range merged {
		if known[n.URL] || n.URL == "" {
			continue
		}
		if err := notifications.ValidateURL(n.URL); err != nil {
			errs[editedTargetKey(e.edited, n.URL)] = err.Error()
		}
	}
	if len(errs) > 0 {
		return nil, false, errs
	}
	return merged, true, nil
}

// savedResponse is PUT /api/config's answer: success, whether the save
// changed the live config, and — for a save carrying notification_edits —
// the target list as it now stands, copied with nothing shared (it is
// encoded after the lock is released), so the editor's next edits start
// from the live list rather than its own stale one.
func savedResponse(edits *notificationEdits, live []config.NotificationConfig, changed bool) map[string]any {
	resp := map[string]any{"success": true, "changed": changed}
	if edits != nil {
		list := make([]config.NotificationConfig, len(live))
		for i, n := range live {
			list[i] = notifications.CloneTarget(n)
		}
		resp["notifications"] = list
	}
	return resp
}

// writeValidationFailed is the 400 every refused PUT /api/config check
// answers with: details keyed by field.
func writeValidationFailed(rw http.ResponseWriter, details map[string]string) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(rw).Encode(map[string]any{
		"error":   "Validation failed",
		"details": details,
	})
}

// editedTargetKey names the edited entry carrying url, as the editor knows
// it: a merged target's URL is always one of edited's, written verbatim.
func editedTargetKey(edited []config.NotificationConfig, url string) string {
	for i, n := range edited {
		if n.URL == url {
			return fmt.Sprintf("notifications[%d].url", i)
		}
	}
	return "notification_edits.edited"
}

// normalizeNotificationEntries canonicalises decoded targets as they are
// stored: the mention in its canonical form, and unknown event names
// stripped.
func normalizeNotificationEntries(ncs []config.NotificationConfig) {
	for i := range ncs {
		n := &ncs[i]
		if n.Mention != "" {
			// Unreachable on error — the mention was checked
			// (notificationEntryErrors) — so an error here leaves the
			// decoded (operator-typed) value untouched.
			if canonical, _, _, err := config.ParseMention(n.Mention); err == nil {
				n.Mention = canonical
			}
		}
		// Unknown event names are stripped UNLESS doing so would empty an
		// otherwise non-empty filter: buildTargets treats an absent/nil
		// Events filter as "every event", so reducing an all-garbage filter
		// to empty would turn "matches nothing" into "matches everything".
		// The manager's startup Warn is what tells the operator about the
		// garbage that survives, and the web UI's own chip editor can't
		// produce this state.
		if len(n.Events) > 0 {
			kept := make([]string, 0, len(n.Events))
			for _, e := range n.Events {
				if notifications.KnownEvents[e] {
					kept = append(kept, e)
				}
			}
			if len(kept) > 0 {
				n.Events = kept
			}
		}
		// mention_events has no such trap — stripping only ever narrows who
		// gets pinged, and an emptied list is the meaningful "never" — so
		// unknown entries are dropped unconditionally, all the way down to an
		// explicit empty list.
		if n.MentionEvents != nil {
			kept := make([]string, 0, len(*n.MentionEvents))
			for _, e := range *n.MentionEvents {
				if notifications.KnownEvents[e] {
					kept = append(kept, e)
				}
			}
			n.MentionEvents = &kept
		}
	}
}

// mergedValidationView is updates as validateConfigUpdates should judge
// them: laid over the live config for the fields its cross-field rules pair
// — a disk threshold with the other, a sidecar limit with the other, a
// browser_path with the browser_type stored beside it. The dashboard sends
// only the fields it edited, so a raised warning threshold arrives without
// the critical one it must stay under; checked alone it passed, and
// config.Save refused the merged config as a bare 500. Only the view is
// filled in: the update applied is the one sent, so a live value copied here
// is never written back over a change made since. The check under the write
// lock (config.Validate on the merged copy) is the one that decides.
func mergedValidationView(updates map[string]any, live *config.MoomboxConfig) map[string]any {
	view := maps.Clone(updates)
	pair := func(section, a, b string, liveA, liveB any) {
		sec, ok := view[section].(map[string]any)
		if !ok {
			return
		}
		_, hasA := sec[a]
		_, hasB := sec[b]
		if hasA == hasB {
			return
		}
		sec = maps.Clone(sec)
		if hasA {
			sec[b] = liveB
		} else {
			sec[a] = liveA
		}
		view[section] = sec
	}
	pair("disk", "disk_warn_percent", "disk_critical_percent",
		float64(live.Disk.WarnPercent), float64(live.Disk.CriticalPercent))
	pair("memory", "sidecar_soft_limit_mb", "sidecar_hard_limit_mb",
		float64(live.Memory.SidecarSoftLimitMB), float64(live.Memory.SidecarHardLimitMB))
	if ck, ok := view["cookies"].(map[string]any); ok {
		_, hasPath := ck["browser_path"]
		if _, hasType := ck["browser_type"]; hasPath && !hasType {
			ck = maps.Clone(ck)
			ck["browser_type"] = live.Cookies.BrowserType
			view["cookies"] = ck
		}
	}
	return view
}
