package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// The waiting loop's taper (rateLimitConfirmTaper, rate_limit.go): a go-live
// confirmation YouTube refused is made again 2, 5, 10 and 20 minutes after
// each refusal of a spell, then every half hour, at those instants exactly.

// TestRateLimitedConfirmationTapers drives the loop through two spells. The
// first refuses seven confirmations, made 2, 5, 10, 20, 30 and 30 minutes
// apart — at those instants, not on the first 30 s probe past them — until
// the probe reads the stream upcoming again; a periodic full fetch then
// answers, which ends the spell; and the second spell's confirmations start
// the taper over, from two minutes, until the one past the limit goes live.
// One backlog hold a spell. Before, every refused confirmation waited a flat
// half hour: a stream that had just gone live waited thirty minutes on a
// limit that lifted in two, its opening lost where it had no DVR window.
//
// Mutants: hold every confirmation for rateLimitRetryInterval — the gaps are
// 30 minutes; count the refusal after picking its hold — the first gap is
// 30; drop the probe timer's cut to the hold's end — the gaps run a probe
// over; keep the count through an answer — the second spell's first gap is
// 30 minutes.
func TestRateLimitedConfirmationTapers(t *testing.T) {
	m := time.Minute
	live := func(at time.Duration) bool { return at >= 3*m && at < 110*m || at >= 150*m }
	run := driveWaitForLive(t, 2*m, 4*time.Hour, func(start time.Time) waitFakes {
		return waitFakes{
			status: func(at time.Duration) (*youtube.VideoInfo, error) {
				if live(at) {
					return liveAnswer(start), nil
				}
				return upcomingAnswer(time.Time{}), nil
			},
			full: func(at time.Duration) (*youtube.VideoInfo, error) {
				switch {
				case at >= 125*m && at < 150*m:
					return upcomingAnswer(start), nil // the periodic fetch answers
				case at >= 180*m:
					return liveAnswer(start), nil // the limit is over
				}
				return rateLimitedInfo(youtube.StreamUpcoming, youtube.SessionAuthLoggedOut), nil
			},
		}
	})
	fulls := fullCalls(run)
	if run.result == nil || !run.result.ShouldDownload {
		t.Fatalf("result = %+v, want the download; calls:%s", run.result, run)
	}
	if len(fulls) != 14 {
		t.Fatalf("full fetches at %v, want 7 confirmations, 2 periodic fetches and 5 confirmations; calls:%s", fulls, run)
	}
	gaps := func(from, to int) []time.Duration {
		var out []time.Duration
		for i := from; i < to; i++ {
			out = append(out, fulls[i+1]-fulls[i])
		}
		return out
	}
	for _, spell := range []struct {
		name     string
		from, to int
		want     []time.Duration
	}{
		{"first spell", 0, 6, []time.Duration{2 * m, 5 * m, 10 * m, 20 * m, 30 * m, 30 * m}},
		{"second spell", 9, 13, []time.Duration{2 * m, 5 * m, 10 * m, 20 * m}},
	} {
		got := gaps(spell.from, spell.to)
		for i := range spell.want {
			if got[i] != spell.want[i] {
				t.Errorf("%s: confirmations %v apart, want %v; calls:%s", spell.name, got, spell.want, run)
				break
			}
		}
	}
	if p1, p2 := fulls[7], fulls[8]; live(p1) || live(p2) || p2 < 125*m {
		t.Errorf("periodic fetches at %s and %s, want two while the probe read upcoming, the second answered", p1, p2)
	}
	if run.returned != fulls[13] {
		t.Errorf("the wait returned at %s, want at the confirmation that answered, %s", run.returned, fulls[13])
	}
	if n := run.count("hold"); n != 2 {
		t.Errorf("%d backlog holds, want one a spell; calls:%s", n, run)
	}
}

// TestHeldConfirmationDoesNotSpinOnProbeErrors: the probe timer is cut to a
// held confirmation's end only while that end is ahead. A probe that fails
// leaves the tick before the hold is lifted, so a hold that has run out can
// stand for as long as the probe fails; cut to it, the timer fired at once,
// every time, and the loop spun on the failing probe at one instant.
//
// Mutant: cut the timer to heldFor whenever a confirmation is held, with no
// guard on its sign — the probe is called again at the same instant.
func TestHeldConfirmationDoesNotSpinOnProbeErrors(t *testing.T) {
	var mu sync.Mutex
	refused := false
	at0, same, spun := time.Duration(-1), 0, false
	run := driveWaitForLive(t, 2*time.Minute, 15*time.Minute, func(start time.Time) waitFakes {
		return waitFakes{
			status: func(at time.Duration) (*youtube.VideoInfo, error) {
				mu.Lock()
				defer mu.Unlock()
				if at == at0 {
					same++
				} else {
					at0, same = at, 0
				}
				if same > 20 {
					spun = true
					return nil, context.Canceled // ends the loop
				}
				switch {
				case at < 3*time.Minute:
					return upcomingAnswer(time.Time{}), nil
				case !refused:
					return liveAnswer(start), nil
				}
				return nil, dialRefused // not counted: the loop waits through it
			},
			full: func(time.Duration) (*youtube.VideoInfo, error) {
				mu.Lock()
				refused = true
				mu.Unlock()
				return rateLimitedInfo(youtube.StreamUpcoming, youtube.SessionAuthLoggedOut), nil
			},
		}
	})
	mu.Lock()
	defer mu.Unlock()
	if spun {
		t.Fatalf("the loop called the failing probe again at the same instant once its hold ran out; calls:%s", run)
	}
	if run.count("full") != 1 {
		t.Errorf("%d full fetches, want the one refused confirmation; calls:%s", run.count("full"), run)
	}
}

// TestCancelDuringAHeldConfirmationWins: an operator's Cancel that lands
// while a refused go-live confirmation is held off ends the loop on its next
// probe, as a cancelled run, with no confirmation made and the wait's line
// cleared from the Cancelled row.
//
// Mutant: drop the loop's deferred endRateLimit — the Cancelled row keeps
// "Rate-limited by YouTube; fetching again... (…)".
func TestCancelDuringAHeldConfirmationWins(t *testing.T) {
	run := driveWaitForLive(t, 2*time.Minute, time.Hour, func(start time.Time) waitFakes {
		return waitFakes{
			status: func(at time.Duration) (*youtube.VideoInfo, error) {
				if at < 3*time.Minute {
					return upcomingAnswer(time.Time{}), nil
				}
				return liveAnswer(start), nil
			},
			full: func(time.Duration) (*youtube.VideoInfo, error) {
				return rateLimitedInfo(youtube.StreamUpcoming, youtube.SessionAuthLoggedOut), nil
			},
			// The first confirmation comes at about 3 minutes and holds the
			// next off for 2.
			cancelAt: 4 * time.Minute,
		}
	})
	if run.result == nil || !errors.Is(run.result.ErrSentinel, ErrCancelled) {
		t.Fatalf("result = %+v, want a cancelled run; calls:%s", run.result, run)
	}
	if n := run.count("full"); n != 1 {
		t.Errorf("%d full fetches, want only the refused confirmation before the Cancel; calls:%s", n, run)
	}
	if run.returned < 4*time.Minute || run.returned > 4*time.Minute+probeIntervalImminent+probeIntervalImminent/10 {
		t.Errorf("the loop returned at %s, want the first probe past the Cancel at 4m", run.returned)
	}
	if run.row.Progress != "" {
		t.Errorf("the Cancelled row's progress = %q, want the wait's line cleared", run.row.Progress)
	}
}
