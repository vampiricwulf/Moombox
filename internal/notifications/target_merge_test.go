package notifications

import (
	"slices"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

const (
	mergeHookA = "https://discord.com/api/webhooks/1/aaa"
	mergeHookB = "https://discord.com/api/webhooks/2/bbb"
	mergeHookD = "https://discord.com/api/webhooks/4/ddd"
)

func mergeURLs(ns []config.NotificationConfig) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = n.URL
	}
	return out
}

// TestMergeTargetEditsIdentityEdges pins the corners of a target's identity
// an editor can reach. The editors' own suites drive the merge through a
// save (internal/tui settings_save_merge_test.go, internal/web/routes
// config_notification_edits_test.go).
//
// Mutants killed: an identity without its occurrence count (deleting one of
// two entries with one URL removes both); an added target always appended (a
// webhook both editors added is listed twice); an edit of a target the other
// editor removed dropped instead of added back.
func TestMergeTargetEditsIdentityEdges(t *testing.T) {
	x := config.NotificationConfig{URL: mergeHookA}
	t.Run("one of two entries with one URL deleted", func(t *testing.T) {
		got, changed := MergeTargetEdits(
			[]config.NotificationConfig{x, x}, []config.NotificationConfig{x}, []int{0},
			[]config.NotificationConfig{x, x})
		if !changed || len(got) != 1 {
			t.Errorf("merged %q (changed %v), want one entry left", mergeURLs(got), changed)
		}
	})
	t.Run("added on both sides", func(t *testing.T) {
		mine := config.NotificationConfig{URL: mergeHookB, Mention: "@here"}
		got, _ := MergeTargetEdits(
			[]config.NotificationConfig{x}, []config.NotificationConfig{x, mine}, []int{0, -1},
			[]config.NotificationConfig{x, {URL: mergeHookB}})
		if !slices.Equal(mergeURLs(got), []string{mergeHookA, mergeHookB}) || got[1].Mention != "@here" {
			t.Errorf("merged %q, want B once, as this editor saved it", mergeURLs(got))
		}
	})
	t.Run("edited here, removed there", func(t *testing.T) {
		muted := false
		edited := config.NotificationConfig{URL: mergeHookA, Enabled: &muted}
		got, _ := MergeTargetEdits(
			[]config.NotificationConfig{x}, []config.NotificationConfig{edited}, []int{0}, nil)
		if len(got) != 1 || got[0].IsEnabled() {
			t.Errorf("merged %q, want this editor's muted A added back", mergeURLs(got))
		}
	})
	t.Run("nothing changed", func(t *testing.T) {
		live := []config.NotificationConfig{{URL: mergeHookD}}
		got, changed := MergeTargetEdits(
			[]config.NotificationConfig{x}, []config.NotificationConfig{x}, []int{0}, live)
		if changed || &got[0] != &live[0] {
			t.Errorf("an untouched list was rewritten (changed %v)", changed)
		}
	})
}

// TestEveryTargetFieldIsWritable pins WriteTargetField to TargetFormValues'
// fields: a field the editors compare but the write does not know would be
// detected as edited and silently never written.
//
// Mutant killed: deleting any one case from WriteTargetField.
func TestEveryTargetFieldIsWritable(t *testing.T) {
	for key := range TargetFormValues(config.NotificationConfig{}) {
		if !WriteTargetField(&config.NotificationConfig{}, config.NotificationConfig{}, key) {
			t.Errorf("the editors compare %q but a save cannot write it", key)
		}
	}
	if WriteTargetField(&config.NotificationConfig{}, config.NotificationConfig{}, "no_such_field") {
		t.Error("WriteTargetField accepted a field it does not know")
	}
}

// TestAnEveryEventFilterIsNotNoFilter: a filter naming each vocabulary
// event is a filter, which the manager keeps to the events it names — one a
// later build adds is not among them — where no filter takes every event, so
// the merge tells the two apart. It used to read them as one value, and the
// dashboard's "Clear filter" on the every-event filter its old Filter... chip
// stored wrote nothing: the filter stayed, and the chips with it.
//
// Mutant killed: targetFormEvents reading a filter that names every
// vocabulary event as "*" (no filter) — the clear is no edit.
func TestAnEveryEventFilterIsNotNoFilter(t *testing.T) {
	var every []string
	for _, g := range EventGroups {
		every = append(every, g.Events...)
	}
	filtered := config.NotificationConfig{URL: mergeHookA, Events: every}
	cleared := config.NotificationConfig{URL: mergeHookA}
	got, changed := MergeTargetEdits(
		[]config.NotificationConfig{filtered}, []config.NotificationConfig{cleared}, []int{0},
		[]config.NotificationConfig{filtered})
	if !changed || len(got) != 1 || got[0].Events != nil {
		t.Errorf("clearing an every-event filter merged to events %v (changed %v), want no filter", got[0].Events, changed)
	}
	// The order and repeats a filter is written with are still no edit.
	reordered := slices.Clone(every)
	slices.Reverse(reordered)
	reordered = append(reordered, every[0])
	if _, changed := MergeTargetEdits(
		[]config.NotificationConfig{filtered}, []config.NotificationConfig{{URL: mergeHookA, Events: reordered}}, []int{0},
		[]config.NotificationConfig{filtered}); changed {
		t.Error("the same filter written in another order read as an edit")
	}
}
