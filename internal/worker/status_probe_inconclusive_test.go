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
