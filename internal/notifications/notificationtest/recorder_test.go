package notificationtest

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/notifications"
)

// TestReadersDoNotAliasTheRecordedFields is T1-m1.
//
// Send already copies the caller's slice, so a producer reusing a
// FieldBuilder cannot corrupt the record. The readers were the other half and
// were missing it: Calls and ByEvent copied the outer []Call, but a Call is
// copied by VALUE, and its Fields still pointed at the recorder's own array.
// A test that sorted or normalised `got[0].Fields` before comparing was
// editing the recorder, and the damage surfaced in whatever assertion came
// next — a failure attributed to the producer, not to the fixture.
//
// THE MUTANT: `out = append(out, c)` in either reader.
func TestReadersDoNotAliasTheRecordedFields(t *testing.T) {
	const original = "the original failure"

	newRecorder := func() *Recorder {
		r := New()
		r.Send("Job Failed", "", notifications.TypeError,
			[]notifications.Field{{Name: "Error", Value: original}},
			notifications.SendOptions{Event: "error"})
		return r
	}

	t.Run("Calls", func(t *testing.T) {
		r := newRecorder()
		r.Calls()[0].Fields[0].Value = "an assertion's scratch edit"
		if got, _ := r.Calls()[0].Field("Error"); got != original {
			t.Errorf("Error = %q, want %q — Calls handed out the recorder's own Fields array", got, original)
		}
	})

	t.Run("ByEvent", func(t *testing.T) {
		r := newRecorder()
		r.ByEvent("error")[0].Fields[0].Value = "an assertion's scratch edit"
		if got, _ := r.ByEvent("error")[0].Field("Error"); got != original {
			t.Errorf("Error = %q, want %q — ByEvent handed out the recorder's own Fields array", got, original)
		}
	})
}
