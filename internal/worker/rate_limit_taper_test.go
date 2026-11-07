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
// the spell's first refused one, then every half hour, at those instants
// exactly.

// TestRateLimitedConfirmationTapers drives the loop through two spells. The
// first refuses seven confirmations, made at +2, +5, +10 and +20 minutes from
// the first refusal, then half an hour apart — 2, 3, 5, 10, 30 and 30 minutes
// apart, at those instants, not on the first 30 s probe past them — until
// the probe reads the stream upcoming again; a periodic full fetch then
// answers, which ends the spell; and the second spell's confirmations start
// the taper over, from two minutes, until the one past the limit goes live.
// One backlog hold a spell. Before, every refused confirmation waited a flat
// half hour: a stream that had just gone live waited thirty minutes on a
// limit that lifted in two, its opening lost where it had no DVR window; and
// then the taper was read as gaps between refusals (+2, +7, +17, +37), where
// the owner's reading counts it from the first (FA's first follow-up).
//
// Mutants: hold every confirmation for rateLimitRetryInterval — the gaps are
// 30 minutes; read the taper as gaps (hold the n-th refusal's entry) — the
// gaps are 2, 5, 10, 20; anchor the taper at each refusal rather than the
// first — likewise; drop the probe timer's cut to the hold's end — the gaps
// run a probe over; keep the anchor through an answer — the second spell's
// first gap is 30 minutes.
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
				case at >= 170*m:
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
		{"first spell", 0, 6, []time.Duration{2 * m, 3 * m, 5 * m, 10 * m, 30 * m, 30 * m}},
		{"second spell", 9, 13, []time.Duration{2 * m, 3 * m, 5 * m, 10 * m}},
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

// membersArmProbe is the status probe of a signed-in wait whose cookied
// probe cannot read the stream: a TV escalation's answer in the first minute
// switches the loop to the cookied probe (B1), and every probe after it reads
// not_a_stream, so the switch's default arm is what decides (membersOnly).
func membersArmProbe(at time.Duration) (*youtube.VideoInfo, error) {
	if at < time.Minute {
		return &youtube.VideoInfo{StreamStatus: youtube.StreamUpcoming, IsUpcoming: true, PlayabilityError: youtube.PlayabilityOK,
			StatusProbeEscalation: youtube.StatusProbeEscalationTVAuth}, nil
	}
	return &youtube.VideoInfo{StreamStatus: youtube.StreamNotAStream, PlayabilityError: youtube.PlayabilityMembersOnly}, nil
}

// TestEveryConfirmationRefusalTapers: the rules of loopRateLimit hold for
// every full fetch that confirms a stream and every shape YouTube refuses
// one in — a 429 on the whole cascade in the live and VOD arms, its
// Retry-After a floor on each hold, and the default arm's full fetch, which
// confirms a stream the cookied probe cannot read once the schedule probe says
// it has begun, refused with the verdict reading upcoming or live, with a 429,
// or with a captcha, after the routine fetch that opened its spell. Each is
// made again
// 2, 5 and 10 minutes after the first refusal of its spell — the one after
// those answers and the stream downloads — with one backlog hold a spell, the
// wait's line cleared at the answer, and a captcha told the operator once,
// and told it cleared once.
// The 429 went the way of a blip, and the default arm was left out of the
// rules: both sent the whole cascade into the limit on every 30 s probe,
// holding nothing and bounded by nothing, and the default arm wrote Live on
// a refusal that read live and went on to a download with nothing to
// download.
//
// Mutants: drop noteFetchErr from the live arm, from the VOD arm, or from
// the default arm — that row confirms on every probe, with no hold; drop the
// Retry-After floor — the floored row's gaps are 2, 3 and 5 minutes; drop noteFetch
// from the default arm — its verdict rows confirm on every probe, the live
// one downloads on the first refusal; drop its confirmationHeld gate — every
// probe confirms.
func TestEveryConfirmationRefusalTapers(t *testing.T) {
	m := time.Minute
	for _, tc := range []struct {
		name     string
		signedIn bool
		status   func(at time.Duration, start time.Time) (*youtube.VideoInfo, error)
		refusal  func() (*youtube.VideoInfo, error)
		answer   func(start time.Time) *youtube.VideoInfo
		gaps     []time.Duration
		captcha  bool
		// webLive: the schedule probe says the stream is live, which is what
		// makes the default arm confirm between its half-hourly full fetches.
		webLive bool
	}{
		{name: "live arm, a 429", status: liveFrom3m(liveAnswer),
			refusal: func() (*youtube.VideoInfo, error) { return nil, rateLimited429(0) },
			answer:  liveAnswer, gaps: []time.Duration{2 * m, 3 * m, 5 * m}},
		{name: "live arm, a 429 asking for 4 minutes", status: liveFrom3m(liveAnswer),
			refusal: func() (*youtube.VideoInfo, error) { return nil, rateLimited429(4 * m) },
			answer:  liveAnswer, gaps: []time.Duration{4 * m, 4 * m, 4 * m}},
		{name: "VOD arm, a 429", status: liveFrom3m(vodAnswer),
			refusal: func() (*youtube.VideoInfo, error) { return nil, rateLimited429(0) },
			answer:  vodAnswer, gaps: []time.Duration{2 * m, 3 * m, 5 * m}},
		{name: "default arm, the verdict reading upcoming", signedIn: true, webLive: true, status: membersStatus,
			refusal: func() (*youtube.VideoInfo, error) {
				return rateLimitedInfo(youtube.StreamUpcoming, youtube.SessionAuthLoggedIn), nil
			},
			answer: liveAnswer, gaps: []time.Duration{2 * m, 3 * m, 5 * m}},
		{name: "default arm, the verdict reading live", signedIn: true, webLive: true, status: membersStatus,
			refusal: func() (*youtube.VideoInfo, error) {
				return rateLimitedInfo(youtube.StreamLive, youtube.SessionAuthLoggedIn), nil
			},
			answer: liveAnswer, gaps: []time.Duration{2 * m, 3 * m, 5 * m}},
		{name: "default arm, a 429", signedIn: true, webLive: true, status: membersStatus,
			refusal: func() (*youtube.VideoInfo, error) { return nil, rateLimited429(0) },
			answer:  liveAnswer, gaps: []time.Duration{2 * m, 3 * m, 5 * m}},
		{name: "default arm, a captcha", signedIn: true, webLive: true, status: membersStatus,
			refusal: func() (*youtube.VideoInfo, error) { return captchaInfo(), nil },
			answer:  liveAnswer, gaps: []time.Duration{2 * m, 3 * m, 5 * m}, captcha: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var first time.Duration
			// Refused until the confirmations the gaps space out are made:
			// the one after them answers.
			var span time.Duration
			for _, g := range tc.gaps {
				span += g
			}
			run := driveWaitForLive(t, 2*m, 2*time.Hour, func(start time.Time) waitFakes {
				f := waitFakes{
					signedIn: tc.signedIn,
					status:   func(at time.Duration) (*youtube.VideoInfo, error) { return tc.status(at, start) },
					full: func(at time.Duration) (*youtube.VideoInfo, error) {
						mu.Lock()
						if first == 0 {
							first = at
						}
						refusing := at < first+span
						mu.Unlock()
						if refusing {
							return tc.refusal()
						}
						return tc.answer(start), nil
					},
				}
				if tc.webLive {
					f.schedule = func(time.Duration) (*youtube.VideoInfo, error) { return liveAnswer(start), nil }
				}
				return f
			})
			fulls := fullCalls(run)
			if tc.webLive && len(fulls) > 0 {
				// The spell's first answer is routine: the schedule probe, which
				// is what makes a fetch a confirmation, runs only after it.
				fulls = fulls[1:]
			}
			if len(fulls) != len(tc.gaps)+1 {
				t.Fatalf("full fetches at %v, want %d refused confirmations and the one that answered; calls:%s",
					fulls, len(tc.gaps), run)
			}
			for i, want := range tc.gaps {
				if got := fulls[i+1] - fulls[i]; got != want {
					t.Errorf("confirmation %d came %s after the one before, want %s", i+2, got, want)
				}
			}
			if run.result == nil || !run.result.ShouldDownload || run.returned != fulls[len(fulls)-1] {
				t.Fatalf("result = %+v at %s, want the download at the confirmation that answered, %s; calls:%s",
					run.result, run.returned, fulls[len(fulls)-1], run)
			}
			if n := run.count("hold"); n != 1 {
				t.Errorf("%d backlog holds, want the spell's one; calls:%s", n, run)
			}
			if run.row.Progress != "" {
				t.Errorf("row progress = %q once the confirmation answered, want the wait's line cleared", run.row.Progress)
			}
			want := 0
			if tc.captcha {
				want = 1
			}
			if n := captchaSends(run.rec, captchaRequiredTitle); n != want {
				t.Errorf("%d captcha notifications, want %d", n, want)
			}
			if n := captchaSends(run.rec, captchaClearedTitle); n != want {
				t.Errorf("%d captcha-cleared notifications once the confirmation answered, want %d", n, want)
			}
		})
	}
}

// liveFrom3m is a status probe that reads upcoming for three minutes and then
// what answer says.
func liveFrom3m(answer func(time.Time) *youtube.VideoInfo) func(time.Duration, time.Time) (*youtube.VideoInfo, error) {
	return func(at time.Duration, start time.Time) (*youtube.VideoInfo, error) {
		if at < 3*time.Minute {
			return upcomingAnswer(time.Time{}), nil
		}
		return answer(start), nil
	}
}

// membersStatus is membersArmProbe in liveFrom3m's shape.
func membersStatus(at time.Duration, _ time.Time) (*youtube.VideoInfo, error) {
	return membersArmProbe(at)
}

// TestABlippedConfirmationIsNoRefusal: a go-live confirmation that failed
// other than on a 429 — a blip, a dial refused — is no refusal: nothing is
// held, no backlog hold is placed, and the confirmation is made again on the
// next probe, as it was before the 429 joined the rate limit's rules.
//
// Mutant: read every fetch failure as a refusal (noteFetchErr without its
// isRateLimited check) — the retry waits two minutes, under a backlog hold.
func TestABlippedConfirmationIsNoRefusal(t *testing.T) {
	var mu sync.Mutex
	blipped := false
	run := driveWaitForLive(t, 2*time.Minute, time.Hour, func(start time.Time) waitFakes {
		status := liveFrom3m(liveAnswer)
		return waitFakes{
			status: func(at time.Duration) (*youtube.VideoInfo, error) { return status(at, start) },
			full: func(time.Duration) (*youtube.VideoInfo, error) {
				mu.Lock()
				defer mu.Unlock()
				if !blipped {
					blipped = true
					return nil, dialRefused
				}
				return liveAnswer(start), nil
			},
		}
	})
	fulls := fullCalls(run)
	if len(fulls) != 2 || run.result == nil || !run.result.ShouldDownload {
		t.Fatalf("full fetches at %v, result %+v; want the blip and the download on the next probe; calls:%s", fulls, run.result, run)
	}
	if gap := fulls[1] - fulls[0]; gap >= confirmationHoldAfter(0) {
		t.Errorf("the confirmation after the blip came %s later, want the next probe's", gap)
	}
	if n := run.count("hold"); n != 0 {
		t.Errorf("%d backlog holds for a blip, want 0", n)
	}
}
