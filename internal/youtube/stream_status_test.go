package youtube

import "testing"

// TestStreamStatusFamilies pins the two predicate sets the monitors read in
// place of the literals they used to spell at each site: every status is in
// at most one family, and the empty status (no probe ran) is in neither.
func TestStreamStatusFamilies(t *testing.T) {
	for _, tc := range []struct {
		s                StreamStatus
		broadcast, vodFm bool
	}{
		{StreamLive, true, false},
		{StreamUpcoming, true, false},
		{StreamVOD, false, true},
		{StreamPostLive, false, true},
		{StreamNotAStream, false, true},
		{"", false, false},
	} {
		if got := tc.s.IsBroadcast(); got != tc.broadcast {
			t.Errorf("%q.IsBroadcast() = %v, want %v", tc.s, got, tc.broadcast)
		}
		if got := tc.s.IsVODFamily(); got != tc.vodFm {
			t.Errorf("%q.IsVODFamily() = %v, want %v", tc.s, got, tc.vodFm)
		}
	}
}
