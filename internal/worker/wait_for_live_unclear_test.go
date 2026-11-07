package worker

import (
	"errors"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// The waiting loop's default arm: what it does once it is on the cookied
// status probe and that probe cannot read the stream. It used to make a full
// fetch, a watch page and six player calls, on every poll; a stream made
// private kept that up for 31 hours, until the operator cancelled it.

// unclearFrom is the status probe of a signed-in wait: a TV escalation's answer
// in the first minute switches the loop to the cookied probe (B1), which reads
// the stream upcoming until from and cannot read it after that.
func unclearFrom(from time.Duration) func(time.Duration) (*youtube.VideoInfo, error) {
	return func(at time.Duration) (*youtube.VideoInfo, error) {
		if at < time.Minute || at >= from {
			return membersArmProbe(at)
		}
		return upcomingAnswer(time.Time{}), nil
	}
}

// loginRequiredAnswer is a full fetch refused by a sign-in wall, saying nothing
// about the stream: a verdict no wait may end on.
func loginRequiredAnswer() *youtube.VideoInfo {
	return &youtube.VideoInfo{StreamStatus: youtube.StreamNotAStream, PlayabilityStatus: "LOGIN_REQUIRED",
		PlayabilityError: youtube.PlayabilityLoginRequired, PlayabilityReason: "Please sign in"}
}

// privateAnswer is a full fetch that says the stream was made private.
func privateAnswer() *youtube.VideoInfo {
	return &youtube.VideoInfo{StreamStatus: youtube.StreamNotAStream, PlayabilityStatus: "LOGIN_REQUIRED",
		PlayabilityError: youtube.PlayabilityPrivate, PlayabilityReason: "Private video"}
}

// A stream made private under the cookied probe is given up on at the first
// poll that cannot read it, on one full fetch, with no half hour to wait out.
//
// Mutant: a spell's first answer not due a full fetch (the wait runs to the
// budget).
func TestUnclearProbeGivesUpOnAPrivateStreamAtOnce(t *testing.T) {
	const privateAt = 5 * time.Minute
	run := driveWaitForLive(t, 2*time.Minute, 20*time.Minute, func(time.Time) waitFakes {
		return waitFakes{
			signedIn: true,
			status:   unclearFrom(privateAt),
			full:     func(time.Duration) (*youtube.VideoInfo, error) { return privateAnswer(), nil },
		}
	})
	if run.result == nil || run.result.ShouldDownload || run.returned < privateAt || run.returned > privateAt+time.Minute {
		t.Fatalf("the wait ended with %+v at %v, want it given up on the first poll past %v:%s", run.result, run.returned, privateAt, run)
	}
	if n := run.count("full"); n != 1 {
		t.Errorf("%d full fetches, want the one that said private:%s", n, run)
	}
	if run.result.Error != "stream unavailable: Private video" || !errors.Is(run.result.ErrSentinel, ErrNonActionable) {
		t.Errorf("result = %q, %v; want the reason named, non-actionable", run.result.Error, run.result.ErrSentinel)
	}
}

// A cookied probe that stays unreadable, over a full fetch that never settles
// anything, costs one full fetch per fullFetchInterval and one schedule probe
// on every other poll: what the 31-hour wait should have cost. That holds for a
// refusal the give-up rules do not place, whichever words it uses, so the bound cannot lean on the give-up, and for a fetch that keeps failing,
// which is not retried every poll.
//
// Mutants: the fetch made on every poll (some 650 of them); lastFullFetch not
// set on the failed fetch (the failing row fetches on every poll); the schedule
// probe left out of the polls between (their ticks make only the status call).
func TestUnclearProbeCostsOneFullFetchPerInterval(t *testing.T) {
	const budget = 6 * time.Hour
	for _, tc := range []struct {
		name string
		full func(time.Duration) (*youtube.VideoInfo, error)
	}{
		{"refused with a sign-in wall", func(time.Duration) (*youtube.VideoInfo, error) { return loginRequiredAnswer(), nil }},
		{"failing", func(time.Duration) (*youtube.VideoInfo, error) { return nil, errors.New("innertube: 503") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := driveWaitForLive(t, 2*time.Minute, budget, func(time.Time) waitFakes {
				return waitFakes{signedIn: true, status: membersArmProbe, full: tc.full}
			})
			if run.returned >= 0 {
				t.Fatalf("the wait ended with %+v at %v, want it still waiting:%s", run.result, run.returned, run)
			}
			fulls := fullCalls(run)
			if len(fulls) < 11 || len(fulls) > 13 {
				t.Fatalf("%d full fetches in %v, want one per %v plus the first: %v", len(fulls), budget, fullFetchInterval, fulls)
			}
			for i := 1; i < len(fulls); i++ {
				if gap := fulls[i] - fulls[i-1]; gap < fullFetchInterval {
					t.Errorf("full fetches %d and %d came %v apart, want at least %v", i, i+1, gap, fullFetchInterval)
				}
			}
			// Every poll past the switch: the cookied probe, then the one call.
			for _, tick := range run.ticks()[1:] {
				if got := kinds(tick); got != "status_auth,schedule" && got != "status_auth,full" {
					t.Fatalf("a poll made %s, want status_auth and one of schedule or full:%s", got, run)
				}
			}
			if run.row.Status != database.StatusUpcoming {
				t.Errorf("row status = %q, want %q", run.row.Status, database.StatusUpcoming)
			}
		})
	}
}

// The schedule probe is what sees a stream the cookied probe cannot read go
// live, and its answer gets the confirming full fetch on that same poll, so a
// go-live that probe can see is not an instant slower than when every poll made
// the full fetch. An answer that says the stream is over goes the same way.
//
// Mutants: a begun answer not due a full fetch (the wait runs to the next half
// hour); no schedule probe in the arm (likewise).
func TestUnclearProbeConfirmsWhatTheScheduleProbeSeesBegin(t *testing.T) {
	const begins = 10 * time.Minute
	for _, tc := range []struct {
		name   string
		answer func(time.Time) *youtube.VideoInfo
		vod    bool
	}{
		{"live", liveAnswer, false},
		{"over", endedAnswer, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := driveWaitForLive(t, 2*time.Minute, 25*time.Minute, func(start time.Time) waitFakes {
				begun := func(at time.Duration) (*youtube.VideoInfo, error) {
					if at < begins {
						return upcomingAnswer(start), nil
					}
					return tc.answer(start), nil
				}
				return waitFakes{signedIn: true, status: membersArmProbe, schedule: begun, full: begun}
			})
			if run.result == nil || !run.result.ShouldDownload || run.result.IsVod != tc.vod {
				t.Fatalf("the wait ended with %+v at %v, want the download:%s", run.result, run.returned, run)
			}
			if run.returned < begins || run.returned > begins+time.Minute {
				t.Errorf("the wait ended at %v, want within a poll of %v:%s", run.returned, begins, run)
			}
			ticks := run.ticks()
			if got := kinds(ticks[len(ticks)-1]); got != "status_auth,schedule,full" {
				t.Errorf("the last poll made %s, want status_auth,schedule,full:%s", got, run)
			}
		})
	}
}

// A go-live the schedule probe is refused too is seen only by the cascade's
// other clients, so it waits for the next fetch due. That is the price of not
// making the fetch on every poll, and this pins its bound: the half-hourly fetch
// finds the stream, and nothing is later than that.
//
// Mutant: the fetch due only on a spell's first answer (the wait runs to the
// budget).
func TestUnclearProbeSeesAGoLiveTheScheduleProbeCannotAtTheNextFetchDue(t *testing.T) {
	const begins = 5 * time.Minute
	run := driveWaitForLive(t, 2*time.Minute, 2*fullFetchInterval, func(start time.Time) waitFakes {
		return waitFakes{
			signedIn: true,
			status:   membersArmProbe,
			schedule: func(time.Duration) (*youtube.VideoInfo, error) { return loginRequiredAnswer(), nil },
			full: func(at time.Duration) (*youtube.VideoInfo, error) {
				if at < begins {
					return loginRequiredAnswer(), nil
				}
				return liveAnswer(start), nil
			},
		}
	})
	if run.result == nil || !run.result.ShouldDownload {
		t.Fatalf("the wait ended with %+v at %v, want the download:%s", run.result, run.returned, run)
	}
	fulls := fullCalls(run)
	if len(fulls) != 2 || fulls[0] >= begins {
		t.Fatalf("full fetches at %v, want the spell's first ahead of %v and the one that found it:%s", fulls, begins, run)
	}
	if gap := fulls[1] - fulls[0]; gap < fullFetchInterval || gap > fullFetchInterval+time.Minute {
		t.Errorf("the go-live was found %v after the first fetch, want within a poll of %v:%s", gap, fullFetchInterval, run)
	}
}

// A schedule probe that says live is only a reason to ask: a full fetch that
// disagrees keeps the job waiting, with nothing downloaded and nothing written
// Live. It costs a full fetch per poll for as long as the two disagree, as the
// live arm's confirmation does.
//
// Mutant: the arm acting on the schedule answer without the full fetch (the
// job goes Live and downloads).
func TestUnclearProbeKeepsWaitingWhenTheFullFetchDisagrees(t *testing.T) {
	for _, tc := range []struct {
		name string
		full func(time.Time) *youtube.VideoInfo
	}{
		{"it says upcoming", upcomingAnswer},
		{"it is refused", func(time.Time) *youtube.VideoInfo { return loginRequiredAnswer() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := driveWaitForLive(t, 2*time.Minute, 15*time.Minute, func(start time.Time) waitFakes {
				return waitFakes{
					signedIn: true,
					status:   membersArmProbe,
					schedule: func(time.Duration) (*youtube.VideoInfo, error) { return liveAnswer(start), nil },
					full:     func(time.Duration) (*youtube.VideoInfo, error) { return tc.full(start), nil },
				}
			})
			if run.returned >= 0 {
				t.Fatalf("the wait ended with %+v at %v, want it still waiting:%s", run.result, run.returned, run)
			}
			if run.count("schedule") == 0 {
				t.Fatalf("the schedule probe never ran, so nothing was tested:%s", run)
			}
			if run.row.Status != database.StatusUpcoming {
				t.Errorf("row status = %q, want %q", run.row.Status, database.StatusUpcoming)
			}
		})
	}
}

// With the status probe blind and no switch to the cookied one, a stream made
// private is given up on at the first periodic full fetch: the other way a
// wait reaches the verdict.
//
// Mutant: isTerminalPlayability without private (the wait runs to the budget).
func TestInconclusiveProbeGivesUpOnAPrivateStreamAtTheFirstFullFetch(t *testing.T) {
	run := driveWaitForLive(t, 2*time.Minute, 2*fullFetchInterval, func(time.Time) waitFakes {
		return waitFakes{
			status: func(time.Duration) (*youtube.VideoInfo, error) { return nil, botWalledProbe(errEscalation400) },
			full:   func(time.Duration) (*youtube.VideoInfo, error) { return privateAnswer(), nil },
		}
	})
	if run.result == nil || run.result.ShouldDownload || run.returned > fullFetchInterval+time.Minute {
		t.Fatalf("the wait ended with %+v at %v, want it given up at the first periodic fetch (%v):%s", run.result, run.returned, fullFetchInterval, run)
	}
	if n := run.count("full"); n != 1 {
		t.Errorf("%d full fetches, want the one that said private:%s", n, run)
	}
}

// A schedule probe that fails is no reason for the cascade: the poll ends there.
//
// Mutant: falling through to the full fetch on the probe's error (a full fetch
// on every poll).
func TestUnclearProbeDoesNotFetchOnAFailedScheduleProbe(t *testing.T) {
	run := driveWaitForLive(t, 2*time.Minute, 20*time.Minute, func(time.Time) waitFakes {
		return waitFakes{
			signedIn: true,
			status:   membersArmProbe,
			schedule: func(time.Duration) (*youtube.VideoInfo, error) { return nil, errors.New("innertube: 503") },
			full:     func(time.Duration) (*youtube.VideoInfo, error) { return loginRequiredAnswer(), nil },
		}
	})
	if run.returned >= 0 {
		t.Fatalf("the wait ended with %+v at %v, want it still waiting:%s", run.result, run.returned, run)
	}
	if run.count("schedule") == 0 {
		t.Fatalf("the schedule probe never ran, so nothing was tested:%s", run)
	}
	if n := run.count("full"); n != 1 {
		t.Errorf("%d full fetches, want the spell's first alone:%s", n, run)
	}
}

// An answer the cookied probe can read ends the spell, so the next time it
// cannot read the stream counts as new: a stream made private after a stretch
// of readable answers gives up on that poll, not at the half hour.
//
// Mutant: the spell never ended (the second unclear stretch makes no full fetch
// inside the half hour).
func TestAReadableAnswerEndsAnUnclearSpell(t *testing.T) {
	const m = time.Minute
	readable := func(at time.Duration) bool { return at >= 4*m && at < 8*m }
	run := driveWaitForLive(t, 2*m, 20*m, func(time.Time) waitFakes {
		return waitFakes{
			signedIn: true,
			status: func(at time.Duration) (*youtube.VideoInfo, error) {
				if readable(at) {
					return upcomingAnswer(time.Time{}), nil
				}
				return membersArmProbe(at)
			},
			full: func(time.Duration) (*youtube.VideoInfo, error) { return loginRequiredAnswer(), nil },
		}
	})
	fulls := fullCalls(run)
	if len(fulls) != 2 || fulls[0] > 2*m || fulls[1] < 8*m || fulls[1] > 9*m {
		t.Errorf("full fetches at %v, want the first spell's and the second's, at about 8m:%s", fulls, run)
	}
}

// What this arm's calls learn reaches the row: the periodic fetch and the
// schedule probe that would have told it never run on an answer that is not
// upcoming, so a start moved while the cookied probe cannot read the stream
// was seen by nothing else.
//
// Mutants: the full fetch's metadata not applied (the first row's start stays);
// the schedule answer dropped (the second's does).
func TestUnclearProbeWritesAMovedStartToTheRow(t *testing.T) {
	const moveBy = 15 * time.Minute
	for _, tc := range []struct {
		name  string
		fakes func(moved time.Time) waitFakes
	}{
		{"the full fetch sees it", func(moved time.Time) waitFakes {
			return waitFakes{
				full:     func(time.Duration) (*youtube.VideoInfo, error) { return upcomingAnswer(moved), nil },
				schedule: func(time.Duration) (*youtube.VideoInfo, error) { return upcomingAnswer(time.Time{}), nil }, // refused: no schedule
			}
		}},
		{"the schedule probe sees it", func(moved time.Time) waitFakes {
			return waitFakes{
				full:     func(time.Duration) (*youtube.VideoInfo, error) { return loginRequiredAnswer(), nil },
				schedule: func(time.Duration) (*youtube.VideoInfo, error) { return upcomingAnswer(moved), nil },
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := driveWaitForLive(t, 50*time.Minute, 20*time.Minute, func(start time.Time) waitFakes {
				f := tc.fakes(start.Add(moveBy))
				f.signedIn, f.status = true, membersArmProbe
				return f
			})
			if run.returned >= 0 {
				t.Fatalf("the wait ended with %+v at %v, want it still waiting:%s", run.result, run.returned, run)
			}
			if moved := run.start.Add(moveBy).Format(time.RFC3339); !sameInstant(run.row.StreamStartTime, moved) {
				t.Errorf("row start = %q, want the moved start %q:%s", run.row.StreamStartTime, moved, run)
			}
			if got := run.rec.ByEvent("rescheduled"); len(got) != 1 {
				t.Errorf("recorded %d \"rescheduled\" embeds for one move, want 1: %+v", len(got), got)
			}
		})
	}
}

// A days-out stream the cookied probe cannot read, its full fetches refused for
// over a day, is still waiting at the end of it: nothing has said it began, so
// nothing it airs is being lost, and those fetches run on the arm's own cadence
// rather than to confirm anything. They used to count, which ended such a job in
// Error before it began, and to hold the WEB probe off behind each refusal. The
// refusal still starts its spell, with the one backlog hold. A WEB probe that
// said begun once and upcoming since counts only that one refusal.
//
// Mutants: every default-arm fetch noted as a confirmation (the wait gives up
// after a day, and no schedule probe runs on a held poll); the arm's upcoming
// answer not clearing begun (the third row counts every routine fetch and gives
// up); no spell started by a routine refusal (no hold).
func TestAStreamTheCookiedProbeCannotReadOutlastsADayOfRefusedRoutineFetches(t *testing.T) {
	const budget = rateLimitCap + 6*time.Hour
	verdict := func(time.Duration) (*youtube.VideoInfo, error) {
		return rateLimitedInfo(youtube.StreamUpcoming, youtube.SessionAuthLoggedIn), nil
	}
	for _, tc := range []struct {
		name     string
		full     func(time.Duration) (*youtube.VideoInfo, error)
		schedule func(start time.Time) func(time.Duration) (*youtube.VideoInfo, error)
	}{
		{name: "the verdict", full: verdict},
		{name: "a 429", full: func(time.Duration) (*youtube.VideoInfo, error) { return nil, rateLimited429(0) }},
		{name: "the verdict, after the WEB probe said begun once", full: verdict,
			schedule: func(start time.Time) func(time.Duration) (*youtube.VideoInfo, error) {
				said := false
				return func(time.Duration) (*youtube.VideoInfo, error) {
					if !said {
						said = true
						return liveAnswer(start), nil
					}
					return upcomingAnswer(start), nil
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := driveWaitForLive(t, 72*time.Hour, budget, func(start time.Time) waitFakes {
				f := waitFakes{signedIn: true, status: membersArmProbe, full: tc.full}
				if tc.schedule != nil {
					f.schedule = tc.schedule(start)
				}
				return f
			})
			if run.returned >= 0 {
				t.Fatalf("the wait ended with %+v at %v, want it still waiting at %v", run.result, run.returned, budget)
			}
			if run.row.Status != database.StatusUpcoming || run.row.Progress != "" {
				t.Errorf("row = %q, progress %q; want Upcoming with no wait line", run.row.Status, run.row.Progress)
			}
			if n := run.count("hold"); n != 1 {
				t.Errorf("%d backlog holds, want the spell's one", n)
			}
			late := 0
			for _, c := range run.calls {
				if c.kind == "schedule" && c.at > rateLimitCap {
					late++
				}
			}
			if late == 0 {
				t.Error("no schedule probe past the day, want the WEB probe still polling")
			}
			fulls := fullCalls(run)
			if tc.schedule != nil {
				fulls = fulls[2:] // the spell's first, and the confirmation the begun answer asked for
			}
			for i := 1; i < len(fulls); i++ {
				if gap := fulls[i] - fulls[i-1]; gap < fullFetchInterval {
					t.Errorf("full fetches %d and %d came %v apart, want at least %v", i, i+1, gap, fullFetchInterval)
				}
			}
		})
	}
}

// falseGoLive says the stream has begun for this long (a test broadcast, or a
// slate that flipped), and upcoming after it: long enough for several refused
// confirmations on the taper.
const falseGoLive = 90 * time.Minute

// A go-live that did not hold cannot end the wait a day later. The confirmations
// refused while something said begun count, and nothing but an answered fetch
// used to clear that count, so with every fetch refused the first refusal a day
// on ended the job, though it counted for nothing itself and the stream was still
// two days out. Only a refusal that counted can end a wait now, and the periodic
// fetch is held to that as the default arm's routine one is.
//
// Mutant: outlasted not reading counted (every row gives up a day in).
func TestAGoLiveThatDidNotHoldCannotEndTheWaitADayLater(t *testing.T) {
	const budget = rateLimitCap + 6*time.Hour
	statusLiveThenUpcoming := func(start time.Time) func(time.Duration) (*youtube.VideoInfo, error) {
		return func(at time.Duration) (*youtube.VideoInfo, error) {
			if at < falseGoLive {
				return liveAnswer(start), nil
			}
			return upcomingAnswer(time.Time{}), nil
		}
	}
	for _, tc := range []struct {
		name  string
		fakes func(start time.Time) waitFakes
	}{
		{name: "default arm, the WEB probe says begun and then upcoming", fakes: func(start time.Time) waitFakes {
			return waitFakes{signedIn: true, status: membersArmProbe,
				schedule: func(at time.Duration) (*youtube.VideoInfo, error) {
					if at < falseGoLive {
						return liveAnswer(start), nil
					}
					return upcomingAnswer(start), nil
				}}
		}},
		{name: "periodic fetch, the status probe gone blind, the WEB probe says upcoming", fakes: func(start time.Time) waitFakes {
			return waitFakes{status: func(at time.Duration) (*youtube.VideoInfo, error) {
				if at < falseGoLive {
					return liveAnswer(start), nil
				}
				return nil, botWalledProbe(errEscalation400)
			}}
		}},
		{name: "periodic fetch, the WEB probe says upcoming", fakes: func(start time.Time) waitFakes {
			return waitFakes{status: statusLiveThenUpcoming(start)}
		}},
		{name: "periodic fetch, the WEB probe refused", fakes: func(start time.Time) waitFakes {
			return waitFakes{status: statusLiveThenUpcoming(start),
				schedule: func(time.Duration) (*youtube.VideoInfo, error) {
					return &youtube.VideoInfo{StreamStatus: youtube.StreamNotAStream, PlayabilityError: youtube.PlayabilityMembersOnly}, nil
				}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := driveWaitForLive(t, 72*time.Hour, budget, func(start time.Time) waitFakes {
				f := tc.fakes(start)
				f.full = func(time.Duration) (*youtube.VideoInfo, error) {
					return rateLimitedInfo(youtube.StreamUpcoming, youtube.SessionAuthLoggedIn), nil
				}
				return f
			})
			if run.returned >= 0 {
				t.Fatalf("the wait ended with %+v at %v, want it still waiting at %v", run.result, run.returned, budget)
			}
			early, late := 0, 0
			for _, at := range fullCalls(run) {
				if at < falseGoLive {
					early++
				}
				if at > rateLimitCap {
					late++
				}
			}
			if early < rateLimitCapRefusals+1 {
				t.Errorf("%d full fetches while the go-live stood, want enough refused confirmations to meet the cap", early)
			}
			if late == 0 {
				t.Error("no full fetch past the day, so nothing tested that a refusal then cannot end the wait")
			}
			if run.row.Status != database.StatusUpcoming || run.row.Progress != "" {
				t.Errorf("row = %q, progress %q; want Upcoming with no wait line", run.row.Status, run.row.Progress)
			}
		})
	}
}

// At the end of a hold past the taper the schedule probe is asked before the fetch
// that has come due, and a probe that does not say begun must not cost that fetch.
// It is what can give up on a stream made private, and it runs on nothing else
// here. Upcoming also takes begun back, so the arm's routine cadence resumes.
//
// Mutant: end the poll when the probe fails or says upcoming, as a poll with no
// fetch due does (the "fails" row makes no full fetch for the rest of the run,
// begun standing with nothing to clear it).
func TestADueFetchStillGoesOutWhenTheScheduleProbeDoesNotConfirmAtAHoldsEnd(t *testing.T) {
	const begunFor = 40 * time.Minute // past the taper's last step, into its first long hold
	for _, tc := range []struct {
		name  string
		after func(start time.Time) (*youtube.VideoInfo, error)
	}{
		{name: "says upcoming", after: func(start time.Time) (*youtube.VideoInfo, error) { return upcomingAnswer(start), nil }},
		{name: "fails", after: func(time.Time) (*youtube.VideoInfo, error) { return nil, errors.New("schedule probe failed") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := driveWaitForLive(t, 72*time.Hour, 4*time.Hour, func(start time.Time) waitFakes {
				return waitFakes{signedIn: true, status: membersArmProbe,
					schedule: func(at time.Duration) (*youtube.VideoInfo, error) {
						if at < begunFor {
							return liveAnswer(start), nil
						}
						return tc.after(start)
					},
					full: func(time.Duration) (*youtube.VideoInfo, error) {
						return rateLimitedInfo(youtube.StreamUpcoming, youtube.SessionAuthLoggedIn), nil
					}}
			})
			if run.returned >= 0 {
				t.Fatalf("the wait ended with %+v at %v, want it still waiting", run.result, run.returned)
			}
			var after []time.Duration
			for _, at := range fullCalls(run) {
				if at >= begunFor {
					after = append(after, at)
				}
			}
			if len(after) < 4 {
				t.Fatalf("full fetches past the go-live at %v, want them going on every %v", after, fullFetchInterval)
			}
			for i := 1; i < len(after); i++ {
				if gap := after[i] - after[i-1]; gap < fullFetchInterval || gap > fullFetchInterval+5*time.Minute {
					t.Errorf("full fetches %d and %d came %v apart, want about %v", i, i+1, gap, fullFetchInterval)
				}
			}
		})
	}
}
