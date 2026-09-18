package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// fakeStreamInfoSource replays a scripted sequence of GetStreamInfo answers
// and counts the calls.
type fakeStreamInfoSource struct {
	answers []struct {
		info *twitch.TwitchStreamInfo
		err  error
	}
	calls int
}

func (f *fakeStreamInfoSource) next() (*twitch.TwitchStreamInfo, error) {
	i := f.calls
	f.calls++
	if i >= len(f.answers) {
		i = len(f.answers) - 1
	}
	return f.answers[i].info, f.answers[i].err
}

func live() *twitch.TwitchStreamInfo { return &twitch.TwitchStreamInfo{IsLive: true, StreamID: "s1"} }

// TestConfirmTwitchStreamInfo pins O-C's two-sample rule.
//
// Mutants, one per row:
//   - returning the first sample unconditionally: ONE nil reads as "ended"
//     and truncates a live broadcast (the sweep-2 ENGINE-3 defect).
//   - sampling twice even when the first says live: every consult on a
//     healthy stream pays an extra GQL round trip.
//   - swallowing the second sample's error: an unreachable API reads as
//     "ended" instead of deferring the verdict.
func TestConfirmTwitchStreamInfo(t *testing.T) {
	type answer = struct {
		info *twitch.TwitchStreamInfo
		err  error
	}
	slotErr := errors.New("twitch stream metadata slot unavailable")

	for _, tc := range []struct {
		name      string
		answers   []answer
		wantCalls int
		wantLive  bool
		wantErr   bool
	}{
		{"live on the first sample stops there", []answer{{live(), nil}}, 1, true, false},
		{"one nil is not a verdict", []answer{{nil, nil}, {live(), nil}}, 2, true, false},
		{"two nils confirm the end", []answer{{nil, nil}, {nil, nil}}, 2, false, false},
		{"first-sample error defers", []answer{{nil, slotErr}}, 1, false, true},
		{"second-sample error defers", []answer{{nil, nil}, {nil, slotErr}}, 2, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &fakeStreamInfoSource{answers: tc.answers}
			prev := twitchEndConfirmDelay
			t.Cleanup(func() { twitchEndConfirmDelay = prev })
			twitchEndConfirmDelay = time.Millisecond

			info, err := confirmTwitchLiveness(context.Background(), src.next)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got := info != nil && info.IsLive; got != tc.wantLive {
				t.Errorf("live = %v, want %v", got, tc.wantLive)
			}
			if src.calls != tc.wantCalls {
				t.Errorf("GetStreamInfo calls = %d, want %d", src.calls, tc.wantCalls)
			}
		})
	}
}

// TestConfirmTwitchStreamInfoHonoursCancellation pins that a cancelled wait
// between the two samples defers rather than concluding "ended" — a shutdown
// must never be read as the end of a broadcast.
//
// Mutant: ignoring utils.Sleep's error — a shutdown landing in the 5 s gap
// finalizes the recording.
func TestConfirmTwitchStreamInfoHonoursCancellation(t *testing.T) {
	prev := twitchEndConfirmDelay
	t.Cleanup(func() { twitchEndConfirmDelay = prev })
	twitchEndConfirmDelay = time.Second

	ctx, cancel := context.WithCancel(context.Background())
	src := &fakeStreamInfoSource{answers: []struct {
		info *twitch.TwitchStreamInfo
		err  error
	}{{nil, nil}}}
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()

	if _, err := confirmTwitchLiveness(ctx, src.next); err == nil {
		t.Fatal("confirmTwitchLiveness = nil error on a cancelled wait, want the cancellation")
	}
	if src.calls != 1 {
		t.Errorf("GetStreamInfo calls = %d, want 1 (the second sample must not run)", src.calls)
	}
}
