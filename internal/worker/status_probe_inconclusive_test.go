package worker

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// botWalledProbe is an inconclusive status probe as Service.ProbeVideoStatus
// returns it (G8): ANDROID_VR's bot wall, then a signed-out web_embedded
// escalation that failed with escalationErr.
func botWalledProbe(escalationErr error) *youtube.StatusProbeInconclusiveError {
	return &youtube.StatusProbeInconclusiveError{
		Playability:   youtube.PlayabilityLoginRequired,
		Reason:        "Sign in to confirm you’re not a bot",
		Escalation:    "web_embedded",
		EscalationErr: escalationErr,
	}
}

// errEscalation400 is the escalation failure that matters to the classifier:
// an HTTP 4xx in the text, which its terminal arm reads as the video gone.
var errEscalation400 = errors.New("WEB_EMBEDDED API error: HTTP 400 — INVALID_ARGUMENT: Request contains an invalid argument")

// TestAnInconclusiveStatusProbeIsNetworkClassAndAnAnswer is G8's waiting-room
// half (owner decision D6): an inconclusive status probe never counts toward
// the waiting loop's give-up, whatever its escalation met, and tells the
// connectivity oracle the service answered — ANDROID_VR did, with a refusal.
// A cancelled context still reads as the cancellation.
//
// Mutants: classifyProbeErr without the inconclusive arm (the escalation's
// "HTTP 400" meets the terminal "http 4" arm: classServer); the arm placed
// ahead of the cancellation (a cancelled escalation keeps the loop waiting
// instead of letting it stop); probeVote without the inconclusive arm (a
// refused escalation casts no vote, a timed-out or unreachable one votes the
// network down).
func TestAnInconclusiveStatusProbeIsNetworkClassAndAnAnswer(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		wantClass probeErrClass
		wantVote  probeReport
	}{
		{"the escalation refused too", botWalledProbe(errors.New("refused with no broadcast details too (login_required)")), classNetwork, reportSuccess},
		{"the escalation answered HTTP 400", botWalledProbe(errEscalation400), classNetwork, reportSuccess},
		{"the escalation timed out", botWalledProbe(context.DeadlineExceeded), classNetwork, reportSuccess},
		{"the escalation could not reach the service", botWalledProbe(&net.DNSError{Err: "no such host", Name: "www.youtube.com"}), classNetwork, reportSuccess},
		{"the caller's context ended", &youtube.StatusProbeInconclusiveError{Playability: youtube.PlayabilityLoginRequired, EscalationErr: context.Canceled}, classCancelled, reportNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyProbeErr(tc.err); got != tc.wantClass {
				t.Errorf("classifyProbeErr(%v) = %v, want %v", tc.err, got, tc.wantClass)
			}
			if got := probeVote(tc.err); got != tc.wantVote {
				t.Errorf("probeVote(%v) = %v, want %v", tc.err, got, tc.wantVote)
			}
		})
	}
}

// TestWaitingLoopWaitsThroughAnInconclusiveStatusProbe is G8's waiting-room
// test: a signed-out waiting room whose status probe stays bot-walled for a
// quarter of an hour — some thirty 30 s polls, three times
// maxConsecutiveProbeErrors, each escalation answering HTTP 400 — goes on
// waiting, and records the stream once the probe can see it go live. Before,
// the probe answered not_a_stream and the default arm ended the job
// "unexpected status: not_a_stream" on the first poll.
//
// Mutant: classifyProbeErr without the inconclusive arm (the loop gives up
// "max probe errors" after maxConsecutiveProbeErrors polls).
func TestWaitingLoopWaitsThroughAnInconclusiveStatusProbe(t *testing.T) {
	const walled = 15 * time.Minute
	run := driveWaitForLive(t, 2*time.Minute, 2*time.Hour, func(start time.Time) waitFakes {
		return waitFakes{
			status: func(at time.Duration) (*youtube.VideoInfo, error) {
				if at < walled {
					return nil, botWalledProbe(errEscalation400)
				}
				return liveAnswer(start), nil
			},
			full: func(time.Duration) (*youtube.VideoInfo, error) { return liveAnswer(start), nil },
		}
	})
	if run.count("status") <= maxConsecutiveProbeErrors {
		t.Fatalf("%d status probes, want more than maxConsecutiveProbeErrors (%d) inside the wall:%s", run.count("status"), maxConsecutiveProbeErrors, run)
	}
	if run.returned < walled || run.result == nil || !run.result.ShouldDownload || run.result.IsVod {
		t.Fatalf("the wait ended with %+v at %v, want a live download once the wall lifted at %v:%s", run.result, run.returned, walled, run)
	}
}

// TestWaitingLoopKeepsItsAuthorityThroughAnInconclusiveStatusProbe: the
// calls that can still see the broadcast keep their cadence while the
// status probe is blind. The periodic full fetch still gives up on a stream
// that was removed — whose ANDROID_VR refusal is as blind as the bot wall's,
// and whose job the default arm used to end on the probe's not_a_stream — and
// the WEB schedule probe still sees a stream begin. Waited out blind (the
// error path's plain continue), neither ran: the removed stream's job waited
// for good, and a stream that went live behind the wall was not recorded
// until the wall lifted.
//
// Only the inconclusive probe gets the stand-in: one that did not reach
// YouTube at all skips the rest of its tick as before, since the calls behind
// it would not reach YouTube either.
//
// Mutants: the stand-in dropped (continue on an inconclusive probe like any
// network-class error): the first two rows wait to the budget; the stand-in
// taken for every network-class error: the last row makes the full fetch and
// the schedule probe on an unreachable network.
func TestWaitingLoopKeepsItsAuthorityThroughAnInconclusiveStatusProbe(t *testing.T) {
	blind := func(time.Duration) (*youtube.VideoInfo, error) { return nil, botWalledProbe(errEscalation400) }

	t.Run("the full fetch says the stream is gone", func(t *testing.T) {
		run := driveWaitForLive(t, 2*time.Minute, 2*time.Hour, func(time.Time) waitFakes {
			return waitFakes{
				status: blind,
				full: func(time.Duration) (*youtube.VideoInfo, error) {
					return &youtube.VideoInfo{StreamStatus: youtube.StreamUpcoming, PlayabilityError: youtube.PlayabilityUnavailable,
						PlayabilityReason: "This video is unavailable"}, nil
				},
			}
		})
		if run.returned < 0 || run.result == nil || run.result.ShouldDownload {
			t.Fatalf("the wait ended with %+v at %v, want it given up on the full fetch's verdict:%s", run.result, run.returned, run)
		}
		if run.returned > fullFetchInterval+time.Minute || run.count("full") != 1 {
			t.Errorf("gave up at %v after %d full fetches, want the first periodic full fetch (%v):%s", run.returned, run.count("full"), fullFetchInterval, run)
		}
		if run.result.Error != "stream unavailable: This video is unavailable" {
			t.Errorf("Error = %q, want the full fetch's reason", run.result.Error)
		}
	})

	t.Run("the schedule probe sees the stream begin", func(t *testing.T) {
		const goesLive = 10 * time.Minute
		run := driveWaitForLive(t, 2*time.Minute, 2*time.Hour, func(start time.Time) waitFakes {
			return waitFakes{
				status: blind,
				schedule: func(at time.Duration) (*youtube.VideoInfo, error) {
					if at < goesLive {
						return upcomingAnswer(start), nil
					}
					return liveAnswer(start), nil
				},
				full: func(at time.Duration) (*youtube.VideoInfo, error) {
					if at < goesLive {
						return upcomingAnswer(start), nil
					}
					return liveAnswer(start), nil
				},
			}
		})
		if run.returned < goesLive || run.returned > goesLive+scheduleProbeMin+time.Minute ||
			run.result == nil || !run.result.ShouldDownload || run.result.IsVod {
			t.Fatalf("the wait ended with %+v at %v, want a live download within a schedule probe of %v:%s", run.result, run.returned, goesLive, run)
		}
	})

	t.Run("a probe that did not reach YouTube still skips its tick", func(t *testing.T) {
		run := driveWaitForLive(t, 2*time.Minute, fullFetchInterval+10*time.Minute, func(time.Time) waitFakes {
			return waitFakes{status: func(time.Duration) (*youtube.VideoInfo, error) {
				return nil, &net.DNSError{Err: "no such host", Name: "www.youtube.com"}
			}}
		})
		if run.returned >= 0 {
			t.Fatalf("the wait ended with %+v at %v through a network outage, want it still waiting:%s", run.result, run.returned, run)
		}
		if n, m := run.count("full"), run.count("schedule"); n != 0 || m != 0 {
			t.Errorf("%d full fetches and %d schedule probes behind an unreachable status probe, want none:%s", n, m, run)
		}
	})
}

// TestWaitingLoopGivesUpOnARemovalTheRulesCannotPlace: a scheduled stream
// YouTube removed with a reason parsePlayabilityStatus has no keyword for —
// an ERROR refusal it can only call unknown — is given up on at the first
// periodic full fetch, while the status probe calls its own refusal of it
// inconclusive. Before G8 the probe's not_a_stream ended the job on the next
// poll; after it, with only the verdicts the rules place
// (isTerminalPlayability) counted, nothing ended it, and the job polled every
// 30 s past its start for good.
//
// Only that shape: a full fetch walled the way the probe is (a sign-in
// refusal), an UNPLAYABLE the rules cannot place, an ERROR they place as an
// age gate, and an ERROR answer that still states an upcoming broadcast keep
// the loop waiting, as they did.
//
// Mutants: fullFetchSaysRemoved left out of the full fetch's give-up (the
// removal rows wait out the budget); its ERROR conjunct dropped (the
// UNPLAYABLE row gives up); its unknown conjunct dropped (the age-gate row
// gives up); its not_a_stream conjunct dropped (the upcoming row gives up);
// the parse no longer stamping PlayabilityStatus is
// TestAnExtractionKeepsTheRefusalsPlayabilityStatus's
// (internal/youtube/status_probe_test.go).
func TestWaitingLoopGivesUpOnARemovalTheRulesCannotPlace(t *testing.T) {
	blind := func(time.Duration) (*youtube.VideoInfo, error) { return nil, botWalledProbe(errEscalation400) }
	const (
		tos        = "This video has been removed for violating YouTube's Terms of Service"
		terminated = "This video is no longer available because the YouTube account associated with this video has been terminated."
	)
	for _, tc := range []struct {
		name     string
		full     youtube.VideoInfo
		wantGone bool
	}{
		{"a Terms of Service removal", youtube.VideoInfo{StreamStatus: youtube.StreamNotAStream, PlayabilityStatus: "ERROR",
			PlayabilityError: youtube.PlayabilityUnknown, PlayabilityReason: tos}, true},
		{"a terminated account", youtube.VideoInfo{StreamStatus: youtube.StreamNotAStream, PlayabilityStatus: "ERROR",
			PlayabilityError: youtube.PlayabilityUnknown, PlayabilityReason: terminated}, true},
		{"the full fetch walled too", youtube.VideoInfo{StreamStatus: youtube.StreamNotAStream, PlayabilityStatus: "LOGIN_REQUIRED",
			PlayabilityError: youtube.PlayabilityLoginRequired, PlayabilityReason: "Sign in to confirm you’re not a bot"}, false},
		{"an UNPLAYABLE the rules cannot place", youtube.VideoInfo{StreamStatus: youtube.StreamNotAStream, PlayabilityStatus: "UNPLAYABLE",
			PlayabilityError: youtube.PlayabilityUnknown, PlayabilityReason: "This video can't be played here"}, false},
		{"an ERROR the rules place as an age gate", youtube.VideoInfo{StreamStatus: youtube.StreamNotAStream, PlayabilityStatus: "ERROR",
			PlayabilityError: youtube.PlayabilityAgeRestricted, PlayabilityReason: "Sign in to confirm your age"}, false},
		{"an ERROR that still states the broadcast", youtube.VideoInfo{StreamStatus: youtube.StreamUpcoming, IsUpcoming: true,
			PlayabilityStatus: "ERROR", PlayabilityError: youtube.PlayabilityUnknown, PlayabilityReason: tos}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := driveWaitForLive(t, 2*time.Minute, 2*fullFetchInterval+10*time.Minute, func(time.Time) waitFakes {
				return waitFakes{
					status: blind,
					full: func(time.Duration) (*youtube.VideoInfo, error) {
						info := tc.full
						return &info, nil
					},
				}
			})
			if !tc.wantGone {
				if run.returned >= 0 {
					t.Fatalf("the wait ended with %+v at %v, want it still waiting:%s", run.result, run.returned, run)
				}
				return
			}
			if run.returned < 0 || run.result == nil || run.result.ShouldDownload {
				t.Fatalf("the wait ended with %+v at %v, want it given up on the full fetch's verdict:%s", run.result, run.returned, run)
			}
			if run.returned > fullFetchInterval+time.Minute || run.count("full") != 1 {
				t.Errorf("gave up at %v after %d full fetches, want the first periodic full fetch (%v):%s", run.returned, run.count("full"), fullFetchInterval, run)
			}
			if want := "stream unavailable: " + tc.full.PlayabilityReason; run.result.Error != want {
				t.Errorf("Error = %q, want %q", run.result.Error, want)
			}
			if !errors.Is(run.result.ErrSentinel, ErrNonActionable) {
				t.Errorf("ErrSentinel = %v, want ErrNonActionable, as every give-up on the full fetch's verdict", run.result.ErrSentinel)
			}
		})
	}
}

// TestAnEscalatedStatusProbeAnswerIsReadForTheBroadcastAlone: an answer the
// status probe's escalation gave in ANDROID_VR's place
// (VideoInfo.StatusProbeEscalation) says what the broadcast is doing and
// nothing about ANDROID_VR, so the three consumers that read ANDROID_VR's
// shape ask which it was:
//
//   - the waiting room's B1 switch: a signed-in escalation's answer means
//     the cookieless probe is blind and TV is not, which is the switch to the
//     cookied probe that ANDROID_VR's own login_required refusal made before
//     G8 put TV's answer in its place — after which every poll paid both;
//   - the stall check: "live with zero formats" from another client is not
//     the interruption signature, and arming it would hand a healthy walled
//     stream resume evidence;
//   - the quality probe: the cookied hop is the call the signed-in
//     escalation already made, answered or not, so it is not made twice.
//
// Mutants: waitingRoomAuthFlip without the tv_auth arm (the escalated rows
// do not switch); observe() called on an escalated answer (the signal is
// armed); the cookied hop not skipped after a TV escalation (probeAuth == 1
// in the first two quality rows); tvEscalationAsked without its
// inconclusive-error arm (the second quality row asks TV again).
func TestAnEscalatedStatusProbeAnswerIsReadForTheBroadcastAlone(t *testing.T) {
	tvAnswer := func() *youtube.VideoInfo {
		return &youtube.VideoInfo{StreamStatus: youtube.StreamUpcoming, IsUpcoming: true, PlayabilityError: youtube.PlayabilityOK,
			StatusProbeEscalation: youtube.StatusProbeEscalationTVAuth}
	}

	t.Run("the waiting room switches to the cookied probe on a signed-in escalation", func(t *testing.T) {
		if !waitingRoomAuthFlip(tvAnswer(), true) {
			t.Error("a TV escalation's answer did not switch the poll to the cookied probe")
		}
		if waitingRoomAuthFlip(tvAnswer(), false) {
			t.Error("switched with no cookies to switch to")
		}
		embedded := tvAnswer()
		embedded.StatusProbeEscalation = youtube.StatusProbeEscalationWebEmbedded
		if waitingRoomAuthFlip(embedded, true) {
			t.Error("a web_embedded escalation's answer switched the poll: it is the signed-out escalation, and says nothing about the cookied probe")
		}
	})

	t.Run("the stall check reads no interruption signature off an escalated answer", func(t *testing.T) {
		for _, escalation := range []string{youtube.StatusProbeEscalationTVAuth, youtube.StatusProbeEscalationWebEmbedded} {
			job := &JobContext{Job: &database.Job{ID: "j1"}, Interruption: &interruptionSignal{}}
			info := &youtube.VideoInfo{StreamStatus: youtube.StreamLive, IsLive: true, PlayabilityError: youtube.PlayabilityOK,
				StatusProbeEscalation: escalation}

			ended, err := observeYouTubeStatusProbe(job, info, nil)

			if err != nil || ended {
				t.Errorf("%s: (ended %v, err %v), want the escalation's live answer: not ended", escalation, ended, err)
			}
			if job.Interruption.fresh() {
				t.Errorf("%s: an escalated live answer with zero formats armed the interruption signal", escalation)
			}
		}
	})

	t.Run("the quality probe does not ask TV twice", func(t *testing.T) {
		for _, tc := range []struct {
			name          string
			probeInfo     *youtube.VideoInfo
			probeErr      error
			wantProbeAuth int
		}{
			{"TV answered for android_vr", tvAnswer(), nil, 0},
			{"TV settled nothing for android_vr", nil, &youtube.StatusProbeInconclusiveError{Playability: youtube.PlayabilityLoginRequired,
				Escalation: youtube.StatusProbeEscalationTVAuth, EscalationErr: errors.New("refused too")}, 0},
			{"web_embedded settled nothing for android_vr", nil, botWalledProbe(errEscalation400), 1},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f := &fakeProbeClient{hasCookies: true, probeInfo: tc.probeInfo, probeErr: tc.probeErr,
					probeAuthInfo: &youtube.VideoInfo{}, fullInfo: selectable("cascade")}

				info, err := probeVideoInfo(context.Background(), f, "vid", false, nil)

				if err != nil || info == nil || info.Title != "cascade" {
					t.Fatalf("probeVideoInfo = (%+v, %v), want the cascade's answer", info, err)
				}
				if f.probe != 1 || f.probeAuth != tc.wantProbeAuth || f.full != 1 {
					t.Errorf("calls: probe=%d probeAuth=%d full=%d; want probe=1 probeAuth=%d full=1",
						f.probe, f.probeAuth, f.full, tc.wantProbeAuth)
				}
			})
		}
	})
}

// TestWaitingLoopSwitchesToTheCookiedProbeOnATVEscalation: a signed-in
// waiting room whose cookieless probe is bot-walled gets its answer from the
// probe's TV escalation, and switches to the cookied probe for the rest of
// the wait (B1), as ANDROID_VR's own login_required refusal made it switch
// before G8 — one TV call a poll, not ANDROID_VR and TV both. When the stream
// is then removed, the cookied probe's refusal goes to the default arm's full
// fetch, which gives up on it as the periodic full fetch does: that arm
// polled a gone stream for good, a full fetch every 30 s.
//
// Mutants: waitingRoomAuthFlip without the tv_auth arm (every poll stays
// cookieless, and the removal ends the job "unexpected status"); the default
// arm's give-up dropped (the wait never ends).
func TestWaitingLoopSwitchesToTheCookiedProbeOnATVEscalation(t *testing.T) {
	const removedAt = 5 * time.Minute
	const tos = "This video has been removed for violating YouTube's Terms of Service"
	removed := func() *youtube.VideoInfo {
		return &youtube.VideoInfo{StreamStatus: youtube.StreamNotAStream, PlayabilityStatus: "ERROR",
			PlayabilityError: youtube.PlayabilityUnknown, PlayabilityReason: tos}
	}
	run := driveWaitForLive(t, 2*time.Minute, fullFetchInterval-5*time.Minute, func(time.Time) waitFakes {
		return waitFakes{
			signedIn: true,
			status: func(at time.Duration) (*youtube.VideoInfo, error) {
				if at < removedAt {
					return &youtube.VideoInfo{StreamStatus: youtube.StreamUpcoming, IsUpcoming: true, PlayabilityError: youtube.PlayabilityOK,
						StatusProbeEscalation: youtube.StatusProbeEscalationTVAuth}, nil
				}
				return removed(), nil
			},
			full: func(time.Duration) (*youtube.VideoInfo, error) { return removed(), nil },
		}
	})
	if n := run.count("status"); n != 1 {
		t.Errorf("%d cookieless status probes, want 1: the first poll's TV escalation switches the wait to the cookied probe:%s", n, run)
	}
	if run.count("status_auth") == 0 {
		t.Errorf("the cookied probe was never asked:%s", run)
	}
	if run.returned < removedAt || run.result == nil || run.result.ShouldDownload {
		t.Fatalf("the wait ended with %+v at %v, want it given up once the stream was removed at %v:%s", run.result, run.returned, removedAt, run)
	}
	if want := "stream unavailable: " + tos; run.result.Error != want {
		t.Errorf("Error = %q, want %q", run.result.Error, want)
	}
}
