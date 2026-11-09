package jobstate

import (
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestTwitchAutoRetryPending is the recovery predicate's table, moved here
// with it from internal/monitor (it was isRecoverableTwitchError).
func TestTwitchAutoRetryPending(t *testing.T) {
	intPtr := func(n int) *int { i := n; return &i }

	tests := []struct {
		name string
		job  *database.Job
		want bool
	}{
		{
			name: "exact flap signature is recoverable",
			job: &database.Job{
				Status:         database.StatusError,
				Error:          TwitchOfflineErrMsg,
				LastVideoSeq:   nil,
				AutoRetryCount: 0,
			},
			want: true,
		},
		{
			name: "different error string is not recoverable",
			job: &database.Job{
				Status:         database.StatusError,
				Error:          "twitch HLS error: bad request",
				LastVideoSeq:   nil,
				AutoRetryCount: 0,
			},
			want: false,
		},
		{
			name: "non-error status is not recoverable",
			job: &database.Job{
				Status:         database.StatusFinished,
				Error:          TwitchOfflineErrMsg,
				LastVideoSeq:   nil,
				AutoRetryCount: 0,
			},
			want: false,
		},
		{
			name: "started downloading is not recoverable (download-time failure)",
			job: &database.Job{
				Status:         database.StatusError,
				Error:          TwitchOfflineErrMsg,
				LastVideoSeq:   intPtr(42),
				AutoRetryCount: 0,
			},
			want: false,
		},
		{
			name: "retry budget exhausted is not recoverable",
			job: &database.Job{
				Status:         database.StatusError,
				Error:          TwitchOfflineErrMsg,
				LastVideoSeq:   nil,
				AutoRetryCount: 2,
			},
			want: false,
		},
		{
			name: "retry budget at 1 of 2 is recoverable",
			job: &database.Job{
				Status:         database.StatusError,
				Error:          TwitchOfflineErrMsg,
				LastVideoSeq:   nil,
				AutoRetryCount: 1,
			},
			want: true,
		},
		{
			name: "nil job is not recoverable",
			job:  nil,
			want: false,
		},
		// W27-06: a Twitch live job whose stream processing never reached
		// Twitch, on a network the connectivity monitor said was up, carries
		// the transport failure's own text — which the exact-string match
		// never took, so the rest of a still-live broadcast was lost. It is
		// marked ParkReasonTwitchUnreachable instead, and recovered on the
		// marker whatever its text. Mutant killed: the predicate keyed on
		// TwitchOfflineErrMsg alone.
		{
			name: "unreachable marker is recoverable whatever the error text",
			job: &database.Job{
				Status:     database.StatusError,
				Error:      `twitch stream info: Post "https://gql.twitch.tv/gql": dial tcp: lookup gql.twitch.tv: no such host`,
				ParkReason: database.ParkReasonTwitchUnreachable,
			},
			want: true,
		},
		{
			name: "unreachable marker with segments downloaded is not recoverable",
			job: &database.Job{
				Status:       database.StatusError,
				Error:        "twitch HLS: dial tcp: connection refused",
				ParkReason:   database.ParkReasonTwitchUnreachable,
				LastVideoSeq: intPtr(7),
			},
			want: false,
		},
		{
			name: "unreachable marker with the budget spent is not recoverable",
			job: &database.Job{
				Status:         database.StatusError,
				Error:          "twitch HLS: dial tcp: connection refused",
				ParkReason:     database.ParkReasonTwitchUnreachable,
				AutoRetryCount: 2,
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TwitchAutoRetryPending(tt.job); got != tt.want {
				t.Errorf("TwitchAutoRetryPending(%+v) = %v, want %v", tt.job, got, tt.want)
			}
		})
	}
}

// TestBothRecoveryCallersKeepTheirAnswers pins that TwitchAutoRetryPending
// answers as each copy it replaced did. The two are re-stated here verbatim:
// the monitor's isRecoverableTwitchError (at MaxTwitchAutoRetries, the only
// budget it was ever called with) and setJobError's retryLikely, which was
// asked of the values the failure writes (its status, its error text, its
// park reason) and the job's LastVideoSeq and AutoRetryCount.
//
// Over every combination the two copies could see, the monitor's answers are
// unchanged everywhere. retryLikely's differ on exactly one shape — a row
// marked ParkReasonTwitchEndUnconfirmed whose error is the offline flap's
// text — which no producer writes: the latch keeps its cause's text, a
// download failure (worker endUnconfirmedError), so setJobError's answer is
// unchanged for every failure it can be handed.
func TestBothRecoveryCallersKeepTheirAnswers(t *testing.T) {
	oldMonitor := func(job *database.Job) bool {
		if job.Status != database.StatusError {
			return false
		}
		if job.Error != TwitchOfflineErrMsg && job.ParkReason != database.ParkReasonTwitchUnreachable {
			return false
		}
		if job.ParkReason == database.ParkReasonTwitchEndUnconfirmed {
			return false
		}
		return job.LastVideoSeq == nil && job.AutoRetryCount < MaxTwitchAutoRetries
	}
	oldRetryLikely := func(job *database.Job) bool {
		return job.Status == database.StatusError &&
			(job.Error == TwitchOfflineErrMsg || job.ParkReason == database.ParkReasonTwitchUnreachable) &&
			job.LastVideoSeq == nil &&
			job.AutoRetryCount < MaxTwitchAutoRetries
	}
	seq := 3
	statuses := []database.JobStatus{database.StatusError, database.StatusCookies, database.StatusFinished, database.StatusUpcoming}
	errs := []string{TwitchOfflineErrMsg, "twitch HLS: dial tcp: connection refused", ""}
	reasons := []database.ParkReason{database.ParkReasonNone, database.ParkReasonAuth, database.ParkReasonMembership,
		database.ParkReasonTwitchEndUnconfirmed, database.ParkReasonTwitchUnreachable, database.ParkReasonMuxAfterCancel}
	for _, st := range statuses {
		for _, e := range errs {
			for _, r := range reasons {
				for _, last := range []*int{nil, &seq} {
					for _, n := range []int{0, 1, 2, 3} {
						job := &database.Job{Status: st, Error: e, ParkReason: r, LastVideoSeq: last, AutoRetryCount: n}
						got := TwitchAutoRetryPending(job)
						if want := oldMonitor(job); got != want {
							t.Errorf("monitor caller changed on %+v: %v, was %v", job, got, want)
						}
						unreachable := r == database.ParkReasonTwitchEndUnconfirmed && e == TwitchOfflineErrMsg
						if want := oldRetryLikely(job); got != want && !unreachable {
							t.Errorf("setJobError caller changed on %+v: %v, was %v", job, got, want)
						}
					}
				}
			}
		}
	}
}

// TestPendingAutoAction is the derived pending action's table, by status,
// park reason and error. Both UIs read it; the dashboard's twin is checked
// against the same cases by cmd/moombox's job-state parity test.
func TestPendingAutoAction(t *testing.T) {
	seq := 1
	tw := func(st database.JobStatus, errText string, r database.ParkReason) *database.Job {
		return &database.Job{Platform: "twitch", Status: st, Error: errText, ParkReason: r}
	}
	cases := []struct {
		name string
		job  *database.Job
		want AutoAction
	}{
		{"nil", nil, AutoActionNone},
		{"end unconfirmed is the mux's", tw(database.StatusError, "twitch HLS: 500", database.ParkReasonTwitchEndUnconfirmed), AutoActionMux},
		{"end unconfirmed with the flap's text is still the mux's", tw(database.StatusError, TwitchOfflineErrMsg, database.ParkReasonTwitchEndUnconfirmed), AutoActionMux},
		{"end unconfirmed on YouTube: no monitor takes it", &database.Job{Platform: "youtube", Status: database.StatusError, ParkReason: database.ParkReasonTwitchEndUnconfirmed}, AutoActionNone},
		{"end unconfirmed but no longer Error", tw(database.StatusCancelled, "", database.ParkReasonTwitchEndUnconfirmed), AutoActionNone},
		{"offline flap", tw(database.StatusError, TwitchOfflineErrMsg, database.ParkReasonNone), AutoActionRetry},
		{"unreachable", tw(database.StatusError, "dial tcp: no such host", database.ParkReasonTwitchUnreachable), AutoActionRetry},
		{"offline flap with a segment", &database.Job{Platform: "twitch", Status: database.StatusError, Error: TwitchOfflineErrMsg, LastVideoSeq: &seq}, AutoActionNone},
		{"offline flap, budget spent", &database.Job{Platform: "twitch", Status: database.StatusError, Error: TwitchOfflineErrMsg, AutoRetryCount: MaxTwitchAutoRetries}, AutoActionNone},
		{"offline flap text on a YouTube row", &database.Job{Platform: "youtube", Status: database.StatusError, Error: TwitchOfflineErrMsg}, AutoActionNone},
		{"ended while offline: the operator's Mux", tw(database.StatusError, "stream ended while Moombox was offline — captured data can be muxed via the Mux action", database.ParkReasonNone), AutoActionNone},
		{"mux after cancel", tw(database.StatusError, "mux: failed", database.ParkReasonMuxAfterCancel), AutoActionNone},
		{"other error", tw(database.StatusError, "boom", database.ParkReasonNone), AutoActionNone},
		{"COOKIES? auth", tw(database.StatusCookies, "401", database.ParkReasonAuth), AutoActionNone},
		{"COOKIES? membership", tw(database.StatusCookies, "member", database.ParkReasonMembership), AutoActionNone},
		{"Finished", tw(database.StatusFinished, "", database.ParkReasonNone), AutoActionNone},
		{"Upcoming", tw(database.StatusUpcoming, "", database.ParkReasonNone), AutoActionNone},
	}
	for _, c := range cases {
		if got := PendingAutoAction(c.job); got != c.want {
			t.Errorf("%s: PendingAutoAction = %q, want %q", c.name, got, c.want)
		}
	}
	if AutoActionNone.Text() != "" || AutoActionMux.Text() == "" || AutoActionRetry.Text() == "" {
		t.Error("Text: none must be empty and each action must have a sentence")
	}
}

// TestWaitLineIsWhatIsWaitLineReads pins the writer and the reader of the
// wait marker together, over the counters a progress line otherwise holds.
func TestWaitLineIsWhatIsWaitLineReads(t *testing.T) {
	for _, l := range []string{
		WaitLine("Waiting for a download slot", "3m"),
		WaitLine("Waiting for network connectivity...", "1m 20s"),
		WaitLine("YouTube is rate-limiting this session; fetching again", "24m 0s left"),
	} {
		if !IsWaitLine(l) || strings.Count(l, "...") != 1 {
			t.Errorf("WaitLine built %q, which IsWaitLine does not read as one wait line", l)
		}
	}
	for _, c := range []string{"", "V: 12/40 A: 12/40 C: 3", "(V: 1234/1300 A: 1234/1300 C: 5678)", "V:95.3% C: 2",
		"Seq: 42 C: 1", "Waiting for stream...", "Muxing..."} {
		if IsWaitLine(c) {
			t.Errorf("IsWaitLine(%q) = true for a line that is not a wait", c)
		}
	}
}
