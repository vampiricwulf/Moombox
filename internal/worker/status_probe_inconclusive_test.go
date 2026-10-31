package worker

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

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
