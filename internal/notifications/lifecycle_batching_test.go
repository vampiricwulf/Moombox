package notifications

import "testing"

// TestEditModeTargetNeverBatches: the ruling excludes edit-mode targets from
// the 5 s found/added/auth window. Without this an edit-mode target's first
// two lifecycle events would arrive as one two-embed message with no id to
// edit afterwards.
func TestEditModeTargetNeverBatches(t *testing.T) {
	for _, tc := range []struct {
		mode          string
		wantImmediate bool
	}{
		{ModeSeparate, false},
		{"", false},
		{ModeEdit, true},
	} {
		var emitted []Message
		b := newBatcher(batchWindow, &fakeBatchClock{}, func(m Message) { emitted = append(emitted, m) }, testLogger{})
		b.setMode(normalizeTargetMode(tc.mode))
		b.Add(Embed{Opts: SendOptions{Event: "found", JobID: "yt_1"}}, "", nil)
		// An edit-mode target emits AT ONCE; a separate-mode one holds the
		// embed until its window closes.
		if got := len(emitted) == 1; got != tc.wantImmediate {
			t.Errorf("mode %q: emitted immediately = %v, want %v", tc.mode, got, tc.wantImmediate)
		}
	}
}
