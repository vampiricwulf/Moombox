package twitch

import (
	"errors"
	"testing"
)

// TestGetStreamInfoErrorCollapse pins O-C's propagate-only change: a
// transient StreamMetadata slot failure must reach the caller as an ERROR so
// the end verdict defers, while a login that does not resolve keeps the
// historical (nil, nil) "offline" contract every other caller relies on.
//
// Mutants, one per row:
//   - restoring errStreamSlotUnavailable to the collapse list: a GQL flap
//     reads as "the channel went offline" and finalizes a live recording.
//   - dropping ErrChannelNotFound from it: a renamed/banned login errors out
//     of processTwitchLive and waitForTwitchLive instead of parking offline.
func TestGetStreamInfoErrorCollapse(t *testing.T) {
	for _, tc := range []struct {
		name      string
		in        error
		wantErr   error
		wantIsNil bool
	}{
		{"slot failure propagates", errStreamSlotUnavailable, errStreamSlotUnavailable, false},
		{"channel not found collapses to offline", ErrChannelNotFound, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info, err := collapseStreamInfoError(nil, tc.in)
			if tc.wantErr == nil && err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantIsNil && info != nil {
				t.Fatalf("info = %v, want nil", info)
			}
		})
	}
}
