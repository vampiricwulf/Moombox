package notifications

import (
	"testing"
	"time"
)

// TestRetiredConnectivityPauseIsAliasedNotForgotten is C8's migration
// contract, and all three halves matter.
//
// The key leaves the vocabulary, so neither UI offers it and the TUI strips it
// from a hand-edited config on the next save. It stays KNOWN, so an operator
// who has not opened the settings UI since upgrading is not warned at every
// startup about a key Moombox itself told them to use. And it aliases to
// connectivity_resume, so their filter keeps receiving the embed that now
// carries the pause — for one release, after which the alias entry goes.
//
// THE MUTANT: deleting the key without the alias. A target filtered to
// ["connectivity_pause"] then goes silent through every outage.
func TestRetiredConnectivityPauseIsAliasedNotForgotten(t *testing.T) {
	for _, g := range EventGroups {
		for _, e := range g.Events {
			if e == "connectivity_pause" {
				t.Errorf("connectivity_pause is still in EventGroups group %q — both UIs would go on offering a key nothing produces", g.Name)
			}
		}
	}
	if !KnownEvents["connectivity_pause"] {
		t.Error("connectivity_pause is not in KnownEvents — an unmigrated config warns at every startup about a key we told them to use")
	}
	if got := eventAliases["connectivity_resume"]; got != "connectivity_pause" {
		t.Errorf("eventAliases[connectivity_resume] = %q, want connectivity_pause", got)
	}
	// The pre-existing alias must survive the change.
	if got := eventAliases["disk_critical"]; got != "disk_warning" {
		t.Errorf("eventAliases[disk_critical] = %q, want disk_warning", got)
	}
}

// TestKnownEventsCoversEveryAliasValue is the rule rather than the instance: an
// alias target is by construction a key that used to exist, so a config naming
// it is a MIGRATED config, not a typo.
func TestKnownEventsCoversEveryAliasValue(t *testing.T) {
	for newKey, legacy := range eventAliases {
		if !KnownEvents[newKey] {
			t.Errorf("alias key %q is not a known event — it can never be produced", newKey)
		}
		if !KnownEvents[legacy] {
			t.Errorf("alias value %q is not in KnownEvents — a config listing it warns at startup although it still works", legacy)
		}
	}
}

// TestALegacyPauseFilterStillReceivesTheResume drives the whole path: a target
// configured before the retirement gets the folded embed.
func TestALegacyPauseFilterStillReceivesTheResume(t *testing.T) {
	rec := &recordingSender{}
	m := newTestManager(t, time.Second, notificationTarget{
		sender: rec,
		events: map[string]bool{"connectivity_pause": true},
		key:    "legacy",
	})
	m.Send("Twitch Download Resumed", "", TypeDownload, nil, SendOptions{Event: "connectivity_resume"})
	m.Send("Download Finished", "", TypeSuccess, nil, SendOptions{Event: "finished"})
	m.Wait()

	got := rec.titles()
	if len(got) != 1 || got[0] != "Twitch Download Resumed" {
		t.Fatalf("delivered %v, want only the resume embed — a legacy connectivity_pause filter must keep receiving it", got)
	}
}
