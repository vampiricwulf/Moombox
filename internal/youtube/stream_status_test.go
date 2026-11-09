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

// classified is a classification as SetStatus records it: the status and
// the three bools derived from it, the shape classifyStream returned before
// SetStatus took the bools over.
func classified(s StreamStatus) (StreamStatus, bool, bool, bool) {
	var v VideoInfo
	v.SetStatus(s)
	return v.StreamStatus, v.IsLive, v.IsUpcoming, v.IsPostLiveDVR
}

// TestSetStatusDerivesTheBools (K53): the three bools are a function of the
// status, written in one place. Every classification sets exactly the one
// that names it, and a re-classification clears the one before.
func TestSetStatusDerivesTheBools(t *testing.T) {
	for _, tc := range []struct {
		s                        StreamStatus
		live, upcoming, postLive bool
	}{
		{StreamLive, true, false, false},
		{StreamUpcoming, false, true, false},
		{StreamPostLive, false, false, true},
		{StreamVOD, false, false, false},
		{StreamNotAStream, false, false, false},
		{"", false, false, false},
	} {
		v := VideoInfo{IsLive: true, IsUpcoming: true, IsPostLiveDVR: true}
		v.SetStatus(tc.s)
		if v.StreamStatus != tc.s || v.IsLive != tc.live || v.IsUpcoming != tc.upcoming || v.IsPostLiveDVR != tc.postLive {
			t.Errorf("SetStatus(%q) = %+v", tc.s, v)
		}
		if v.NotYetLive() != (tc.s == StreamUpcoming) {
			t.Errorf("SetStatus(%q).NotYetLive() = %v", tc.s, v.NotYetLive())
		}
	}
}
