package tui

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/notifications"
)

// notificationIDsFromJS runs the SHIPPED settings.js and reads the ids out of
// NOTIFICATION_EVENT_GROUPS, group by group.
//
// Executed rather than pattern-matched, for the reason settings_restart_parity
// _test.go gives: a source match on a JS list passes on a list that is
// malformed, commented out, or shadowed three lines later. What the browser
// gets is what evaluates.
//
// NOTIFICATION_EVENT_GROUPS is a module-level `const`, which lives in the
// global lexical environment rather than on the global object, so it is read
// as the completion value of an expression rather than via vm.Get.
func notificationIDsFromJS(t *testing.T) ([]string, map[string][]string) {
	t.Helper()
	vm := settingsVM(t)
	v, err := vm.RunString("NOTIFICATION_EVENT_GROUPS")
	if err != nil {
		t.Fatalf("settings.js no longer defines NOTIFICATION_EVENT_GROUPS: %v", err)
	}
	groups, ok := v.Export().([]any)
	if !ok {
		t.Fatalf("NOTIFICATION_EVENT_GROUPS is %T, want an array", v.Export())
	}
	var flat []string
	byGroup := map[string][]string{}
	for i, g := range groups {
		entry, ok := g.(map[string]any)
		if !ok {
			t.Fatalf("NOTIFICATION_EVENT_GROUPS[%d] is %T, want an object", i, g)
		}
		name, _ := entry["name"].(string)
		events, ok := entry["events"].([]any)
		if !ok {
			t.Fatalf("group %q has no events array", name)
		}
		for j, e := range events {
			ev, ok := e.(map[string]any)
			if !ok {
				t.Fatalf("group %q event %d is %T, want an object", name, j, e)
			}
			id, _ := ev["id"].(string)
			label, _ := ev["label"].(string)
			if id == "" {
				t.Fatalf("group %q event %d has no id", name, j)
			}
			if label == "" {
				t.Errorf("event %q has no label — the web editor renders the raw id", id)
			}
			flat = append(flat, id)
			byGroup[name] = append(byGroup[name], id)
		}
	}
	if len(flat) == 0 {
		t.Fatal("NOTIFICATION_EVENT_GROUPS is empty — nothing below can be concluded")
	}
	return flat, byGroup
}

// TestNotificationVocabulariesAgree pins the one fact written in three places:
// the Go registry the manager filters on, the flat list the TUI editor derives,
// and the labelled mirror the web editor renders.
//
// Whichever is edited alone keeps working perfectly while the other UI silently
// stops offering the key — the operator ticks every box they can see and still
// misses the event. Compared as SETS in both directions, and group by group, so
// neither list can grow, shrink, or move a key to the wrong section alone.
//
// internal/tui is the only package that can see all three: it imports
// internal/notifications, owns allNotifEvents, and has the goja harness for the
// shipped settings.js.
func TestNotificationVocabulariesAgree(t *testing.T) {
	jsFlat, jsByGroup := notificationIDsFromJS(t)

	goByGroup := map[string][]string{}
	goFlat := map[string]bool{}
	for _, g := range notifications.EventGroups {
		goByGroup[g.Name] = append(goByGroup[g.Name], g.Events...)
		for _, e := range g.Events {
			goFlat[e] = true
		}
	}

	jsSet := map[string]bool{}
	for _, id := range jsFlat {
		jsSet[id] = true
		if !goFlat[id] {
			t.Errorf("settings.js offers %q, which is not in notifications.EventGroups — the manager will warn it is unknown and never match it", id)
		}
	}
	for e := range goFlat {
		if !jsSet[e] {
			t.Errorf("notifications.EventGroups has %q and settings.js does not — the web editor cannot subscribe to it", e)
		}
	}

	for name, want := range goByGroup {
		got := jsByGroup[name]
		if len(got) != len(want) {
			t.Errorf("group %q: settings.js has %d events, Go has %d (%v vs %v)", name, len(got), len(want), got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("group %q position %d: settings.js has %q, Go has %q — the two editors list the events in different orders", name, i, got[i], want[i])
			}
		}
	}

	// The TUI derives from EventGroups, so this catches a hand-written list
	// creeping back in.
	if len(allNotifEvents) != len(goFlat) {
		t.Errorf("allNotifEvents has %d entries, EventGroups has %d — the TUI list is no longer derived", len(allNotifEvents), len(goFlat))
	}
}

// TestRecoveryEventsAreKnownAndAliased pins the §0 ruling: each close is its
// own key so a target can subscribe to it alone, AND it is aliased to its
// alert so a target that already filters the alert receives the close without
// touching its config.
//
// Mutants this kill:
//   - adding the keys without the aliases: every existing filtered target
//     silently never sees a close.
//   - aliasing the wrong way round (alert -> close): the alias map maps the
//     NEWER key to the OLDER one it split from, and reversing it would make a
//     target that filters only disk_ok start receiving disk warnings.
//   - retyping the map instead of editing it in place, which drops N1's
//     connectivity_resume retirement entry.
func TestRecoveryEventsAreKnownAndAliased(t *testing.T) {
	for _, e := range []string{"sidecar_down", "sidecar_restored", "disk_ok", "channel_healthy"} {
		if !notifications.KnownEvents[e] {
			t.Errorf("%q is not a known event — NewManager warns on a config that lists it", e)
		}
	}
	// All FOUR entries, not just this arc's three: the map is edited in place
	// and the one thing that must never happen to it is an entry being lost.
	// N1's connectivity_resume retirement is the entry a careless retype
	// drops, and dropping it silences a legacy connectivity_pause filter
	// through every outage.
	//
	// closeEv, not close: the builtin is shadowed otherwise, which vet lets
	// pass and staticcheck's predeclared check does not.
	for closeEv, alert := range map[string]string{
		"disk_ok":             "disk_warning",
		"channel_healthy":     "channel_unhealthy",
		"sidecar_restored":    "sidecar_down",
		"disk_critical":       "disk_warning",
		"connectivity_resume": "connectivity_pause",
	} {
		if got := notifications.AliasOf(closeEv); got != alert {
			t.Errorf("alias of %q = %q, want %q", closeEv, got, alert)
		}
	}
}
