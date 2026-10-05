package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// TestQualityChangeInfoRetriesUntilSegmentsGoQuiet pins the quality-change
// branch's player fetch: a failed fetch is retried while segments have been
// quiet for less than streamSegmentTimeout — the verify branch's own clock —
// instead of ending a still-live job in Error on the first transient fault.
//
// Mutants:
//   - returning on the first error: "transient failure" sees one fetch.
//   - dropping the quietFor bound: "persistent failure" never returns (the
//     test's fetch budget fails it).
//   - dropping the ctx check after pause: "cancelled while waiting" fetches
//     again on a dead context.
func TestQualityChangeInfoRetriesUntilSegmentsGoQuiet(t *testing.T) {
	errFetch := errors.New("player fetch failed")
	want := &youtube.VideoInfo{}

	t.Run("transient failure", func(t *testing.T) {
		calls, pauses := 0, 0
		info, err := qualityChangeInfo(context.Background(),
			func(context.Context) (*youtube.VideoInfo, error) {
				calls++
				if calls < 3 {
					return nil, errFetch
				}
				return want, nil
			},
			func() time.Duration { return time.Minute },
			func(context.Context, error) { pauses++ },
		)
		if err != nil || info != want {
			t.Fatalf("got (%v, %v), want the third fetch's answer", info, err)
		}
		if calls != 3 || pauses != 2 {
			t.Errorf("calls=%d pauses=%d, want 3 fetches with a pause between each", calls, pauses)
		}
	})

	t.Run("persistent failure", func(t *testing.T) {
		calls := 0
		quiet := time.Duration(0)
		_, err := qualityChangeInfo(context.Background(),
			func(context.Context) (*youtube.VideoInfo, error) {
				calls++
				if calls > 10 {
					t.Fatal("still fetching after segments went quiet past streamSegmentTimeout")
				}
				return nil, errFetch
			},
			func() time.Duration { return quiet },
			func(context.Context, error) { quiet += streamEndVerifyInterval },
		)
		if !errors.Is(err, errFetch) {
			t.Fatalf("err = %v, want the fetch's own error once segments went quiet", err)
		}
		if want := int(streamSegmentTimeout/streamEndVerifyInterval) + 1; calls != want {
			t.Errorf("calls = %d, want %d (one per verify interval until streamSegmentTimeout)", calls, want)
		}
	})

	t.Run("cancelled while waiting", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		_, err := qualityChangeInfo(ctx,
			func(c context.Context) (*youtube.VideoInfo, error) {
				calls++
				if c.Err() != nil {
					t.Error("fetched again on a cancelled context")
				}
				return nil, errFetch
			},
			func() time.Duration { return 0 },
			func(context.Context, error) { cancel() },
		)
		if !errors.Is(err, context.Canceled) || calls != 1 {
			t.Errorf("got err=%v after %d fetches, want context.Canceled after 1", err, calls)
		}
	})
}
