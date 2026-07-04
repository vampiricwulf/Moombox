package notifications

// EventGroup names a set of related notification events for UI display.
// The Go registry here is the canonical vocabulary — the TUI derives its
// filter editor directly from EventGroups, and the web UI keeps a mirrored
// copy (web/public/modules/settings.js NOTIFICATION_EVENT_GROUPS) that
// carries display labels the Go side doesn't need. When adding an event:
// add it here, mirror it in settings.js, and document it in
// docs/spec/operations.md's event table.
type EventGroup struct {
	Name   string
	Events []string
}

// EventGroups is the canonical notification event vocabulary. Every Event
// string passed to Manager.Send MUST appear here: targets with an event
// filter treat the list as an allowlist, and the UIs' edit-save paths
// rebuild a target's Events strictly from this vocabulary — an
// unregistered event is silently unfilterable AND stripped from
// hand-edited configs on the next UI save.
var EventGroups = []EventGroup{
	{"Job Lifecycle", []string{"found", "added", "scheduled", "rescheduled", "downloading", "quality_split", "gap_split", "muxing", "finished", "error", "cancelled", "auth"}},
	{"Connectivity", []string{"connectivity_pause", "connectivity_resume", "connectivity_split", "connectivity_lost", "connectivity_restored"}},
	{"Trim", []string{"trim_created", "trim_deleted", "trim_error"}},
	{"System", []string{"disk_warning", "disk_critical", "update_available", "update_applied", "update_failed", "crash_recovered", "channel_unhealthy"}},
}

// KnownEvents is the flat membership set derived from EventGroups.
// NewManager warns when a configured Events filter names an event outside
// this set (a typo would otherwise silently filter forever).
var KnownEvents = func() map[string]bool {
	m := make(map[string]bool)
	for _, g := range EventGroups {
		for _, e := range g.Events {
			m[e] = true
		}
	}
	return m
}()

// eventAliases maps a NEWER, more specific event name to the OLDER, broader
// name it split from. A target whose allowlist contains the older name also
// receives the newer event, so existing filters keep working across the
// split (e.g. a "disk_warning" filter still receives "disk_critical"
// alerts — silently losing the MORE urgent alert after an upgrade would be
// the worst possible migration behavior).
var eventAliases = map[string]string{
	"disk_critical": "disk_warning",
}

// IDLabel returns the embed field name for a job's platform ID: Twitch
// broadcasts/VODs carry stream IDs, everything else video IDs. Centralized
// because worker, routes, and cmd emit the same field and previously
// disagreed on the label for Twitch jobs.
func IDLabel(platform string) string {
	if platform == "twitch" {
		return "Stream ID"
	}
	return "Video ID"
}
