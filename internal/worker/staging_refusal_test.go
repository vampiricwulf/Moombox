package worker

import (
	"errors"
	"fmt"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/engine"
)

// TestGapSplitLostData is fix round 1, Minor 4: a split caused by a blocked
// resume truncate must not be reported to the operator as lost segments.
//
// The engine returns ErrGapDetected joined with ErrTruncateBlocked for exactly
// that case, so the split still happens (the part is muxed, a fresh one
// begins) while the notification is suppressed.
//
// Mutant: dropping the `&& !errors.Is(err, engine.ErrTruncateBlocked)` term —
// the blocked-truncate row reports data loss that never happened.
func TestGapSplitLostData(t *testing.T) {
	blockedSplit := fmt.Errorf("%w: %w: %v", engine.ErrGapDetected, engine.ErrTruncateBlocked,
		errors.New("The process cannot access the file because it is being used by another process."))

	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"real CDN gap notifies", engine.ErrGapDetected, true},
		{"blocked truncate never notifies", blockedSplit, false},
		{"init-segment change is not a gap at all", engine.ErrInitSegmentChanged, false},
		{"a plain download failure is not a gap", errors.New("connection reset"), false},
		{"no error", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := gapSplitLostData(tc.err); got != tc.want {
				t.Errorf("gapSplitLostData(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestStagingPreservedRefusal is fix round 1, Minor 6: the YouTube live loop
// must surface the two engine refusals that leave staging intact instead of
// treating them as "the downloaders stopped, maybe the stream ended" and
// spending maxConsecutiveLiveChecks × streamEndVerifyInterval re-verifying.
//
// Mutant: returning false (or dropping either sentinel) — the guard trip falls
// into the stream-end verification path and burns half an hour before the job
// force-ends, with the reason only in the engine log.
func TestStagingPreservedRefusal(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"no-truncate guard", fmt.Errorf("%w: video_stream holds 1 bytes", engine.ErrStagedMediaPresent), true},
		{"blocked resume truncate", fmt.Errorf("%w: %w", engine.ErrTruncateBlocked, errors.New("sharing violation")), true},
		{"quality loss still uses its own path", engine.ErrQualityLost, false},
		{"an ordinary stop is not a refusal", nil, false},
		{"a network failure is not a refusal", errors.New("connection reset"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := stagingPreservedRefusal(tc.err); got != tc.want {
				t.Errorf("stagingPreservedRefusal(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
