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
		// R-I1: the offline flap is a park reason, and its text routes
		// nothing. Mutants killed: the predicate still keyed on the text (the
		// reworded rows are refused); the reason accepted without the
		// segment or budget terms; the ended-offline reason read as a flap.
		{
			name: "offline reason is recoverable whatever the error text",
			job:  &database.Job{Status: database.StatusError, Error: "the channel went away", ParkReason: database.ParkReasonTwitchOffline},
			want: true,
		},
		{
			name: "offline reason with no error text at all",
			job:  &database.Job{Status: database.StatusError, ParkReason: database.ParkReasonTwitchOffline, AutoRetryCount: 1},
			want: true,
		},
		{
			name: "offline reason with segments downloaded is not recoverable",
			job:  &database.Job{Status: database.StatusError, Error: TwitchOfflineErrMsg, ParkReason: database.ParkReasonTwitchOffline, LastVideoSeq: intPtr(1)},
			want: false,
		},
		{
			name: "offline reason with the budget spent is not recoverable",
			job:  &database.Job{Status: database.StatusError, Error: TwitchOfflineErrMsg, ParkReason: database.ParkReasonTwitchOffline, AutoRetryCount: 2},
			want: false,
		},
		{
			name: "offline reason on a row no longer in Error",
			job:  &database.Job{Status: database.StatusCancelled, Error: TwitchOfflineErrMsg, ParkReason: database.ParkReasonTwitchOffline},
			want: false,
		},
		{
			name: "ended-offline reason is the operator's Mux, never a retry",
			job:  &database.Job{Status: database.StatusError, Error: "ended", ParkReason: database.ParkReasonTwitchEndedOffline},
			want: false,
		},
		{
			name: "ended-offline reason is not a retry even under the flap's text",
			job:  &database.Job{Status: database.StatusError, Error: database.LegacyTwitchOfflineError, ParkReason: database.ParkReasonTwitchEndedOffline},
			want: false,
		},
		// The one-release fallback (legacyOfflineFlapRow): a row an earlier
		// release wrote, the frozen text and no reason.
		{
			name: "legacy row: the frozen text with no reason is recoverable",
			job:  &database.Job{Status: database.StatusError, Error: database.LegacyTwitchOfflineError},
			want: true,
		},
		{
			name: "the frozen text under another reason is that reason's row",
			job:  &database.Job{Status: database.StatusError, Error: database.LegacyTwitchOfflineError, ParkReason: database.ParkReasonMuxAfterCancel},
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
// Over every combination a writer of that time could produce
// (writtenBeforeOfflineParkReasons), both copies' answers are unchanged. The
// one shape retryLikely differed on — a row marked
// ParkReasonTwitchEndUnconfirmed whose error is the offline flap's text — is
// not among them: the latch keeps its cause's text, a download failure
// (worker endUnconfirmedError).
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
						// Since the offline park reasons (R-I1) the flap's
						// text routes only a row no reason claimed, so the
						// table is the rows a writer could produce — which
						// leaves out the one shape the two copies differed
						// on as well.
						if !writtenBeforeOfflineParkReasons(job) {
							continue
						}
						got := TwitchAutoRetryPending(job)
						if want := oldMonitor(job); got != want {
							t.Errorf("monitor caller changed on %+v: %v, was %v", job, got, want)
						}
						if want := oldRetryLikely(job); got != want {
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
		{"offline flap, a legacy row: the text and no reason", tw(database.StatusError, database.LegacyTwitchOfflineError, database.ParkReasonNone), AutoActionRetry},
		{"offline flap", tw(database.StatusError, TwitchOfflineErrMsg, database.ParkReasonTwitchOffline), AutoActionRetry},
		{"offline flap, by its reason under a reworded text", tw(database.StatusError, "the channel went away", database.ParkReasonTwitchOffline), AutoActionRetry},
		{"manual add, offline reason: no monitor retries it", &database.Job{ID: "tw_4242", Platform: "twitch", Status: database.StatusError, Error: TwitchOfflineErrMsg, ParkReason: database.ParkReasonTwitchOffline, ManuallyAdded: true}, AutoActionNone},
		{"offline reason on a YouTube row", &database.Job{Platform: "youtube", Status: database.StatusError, ParkReason: database.ParkReasonTwitchOffline}, AutoActionNone},
		{"ended while offline, by its reason", tw(database.StatusError, "ended", database.ParkReasonTwitchEndedOffline), AutoActionNone},
		{"unreachable", tw(database.StatusError, "dial tcp: no such host", database.ParkReasonTwitchUnreachable), AutoActionRetry},
		// A manual add never reaches the recovery: the add writes no history
		// row, so processStreamInfo never finds it HasProcessed (and a
		// tw_manual_ id is not the tw_<stream id> the monitor builds). Its end
		// latch is still the mux's: dispatchEndedBroadcasts reads the login
		// from either id shape.
		{"manual add, unreachable: no monitor retries it", &database.Job{ID: "tw_manual_x_1", Platform: "twitch", Status: database.StatusError, Error: "dial tcp", ParkReason: database.ParkReasonTwitchUnreachable, ManuallyAdded: true}, AutoActionNone},
		{"manual add live at add time, unreachable", &database.Job{ID: "tw_4242", Platform: "twitch", Status: database.StatusError, Error: "dial tcp", ParkReason: database.ParkReasonTwitchUnreachable, ManuallyAdded: true}, AutoActionNone},
		{"manual add, end unconfirmed: still the mux's", &database.Job{ID: "tw_manual_x_1", Platform: "twitch", Status: database.StatusError, Error: "twitch HLS: 500", ParkReason: database.ParkReasonTwitchEndUnconfirmed, ManuallyAdded: true}, AutoActionMux},
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
	// The row alone cannot tell whether the monitor still polls the channel
	// (a removed channel's row waits on nothing), so each sentence states
	// that condition rather than promising the action.
	for _, a := range []AutoAction{AutoActionMux, AutoActionRetry} {
		if !strings.HasSuffix(a.Text(), ", while the channel is monitored") {
			t.Errorf("%s: Text %q does not state the monitored-channel condition", a, a.Text())
		}
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

// The rule as it stood before the Twitch-offline park reasons (owner ruling
// R-I1), re-stated verbatim: the offline flap was the row's error text.
// TestOfflineParkReasonsKeepTheRecoveryTruthTable holds the predicates to it.
const preParkReasonOfflineText = "twitch channel is offline"
const preParkReasonEndedOfflineText = "stream ended while Moombox was offline — captured data can be muxed via the Mux action"

func preParkReasonRetryPending(job *database.Job) bool {
	if job == nil || job.Status != database.StatusError {
		return false
	}
	if job.Error != preParkReasonOfflineText && job.ParkReason != database.ParkReasonTwitchUnreachable {
		return false
	}
	if job.ParkReason == database.ParkReasonTwitchEndUnconfirmed {
		return false
	}
	return job.LastVideoSeq == nil && job.AutoRetryCount < MaxTwitchAutoRetries
}

func preParkReasonPendingAutoAction(job *database.Job) AutoAction {
	switch {
	case job != nil && job.Platform == "twitch" && job.Status == database.StatusError &&
		job.ParkReason == database.ParkReasonTwitchEndUnconfirmed:
		return AutoActionMux
	case job != nil && job.Platform == "twitch" && !job.ManuallyAdded && preParkReasonRetryPending(job):
		return AutoActionRetry
	}
	return AutoActionNone
}

// writtenBeforeOfflineParkReasons reports whether a writer from before the
// Twitch-offline park reasons could have produced the row's status, park
// reason and error text together:
//   - an Error row never carries a COOKIES? reason (setJobError writes the
//     reason and the status from the same error);
//   - a reason an Error row does carry was never written with either offline
//     text — twitch_unreachable keeps the transport failure's text,
//     twitch_end_unconfirmed its cause's (a download failure), and
//     mux_after_cancel the failed Mux's.
//
// A row that is not in Error is answered by its status alone, so every
// combination is kept.
func writtenBeforeOfflineParkReasons(job *database.Job) bool {
	if job.Status != database.StatusError {
		return true
	}
	switch job.ParkReason {
	case database.ParkReasonNone:
		return true
	case database.ParkReasonAuth, database.ParkReasonMembership:
		return false
	}
	return job.Error != preParkReasonOfflineText && job.Error != preParkReasonEndedOfflineText
}

// TestOfflineParkReasonsKeepTheRecoveryTruthTable is the recovery's truth
// table — status × park reason × error text × LastVideoSeq × AutoRetryCount ×
// manual — pinned ahead of the Twitch-offline park reasons (R-I1): for every
// row a writer could produce before them, TwitchAutoRetryPending and
// PendingAutoAction answer as the text-keyed rule did.
func TestOfflineParkReasonsKeepTheRecoveryTruthTable(t *testing.T) {
	seq := 3
	statuses := []database.JobStatus{database.StatusError, database.StatusCookies, database.StatusCancelled,
		database.StatusFinished, database.StatusUpcoming, database.StatusMuxing, database.StatusDownloading}
	errs := []string{preParkReasonOfflineText, preParkReasonEndedOfflineText, "twitch HLS: dial tcp: connection refused", ""}
	reasons := []database.ParkReason{database.ParkReasonNone, database.ParkReasonAuth, database.ParkReasonMembership,
		database.ParkReasonTwitchEndUnconfirmed, database.ParkReasonTwitchUnreachable, database.ParkReasonMuxAfterCancel}
	rows := 0
	for _, st := range statuses {
		for _, e := range errs {
			for _, r := range reasons {
				for _, last := range []*int{nil, &seq} {
					for _, n := range []int{0, 1, 2, 3} {
						for _, manual := range []bool{false, true} {
							job := &database.Job{Platform: "twitch", Status: st, Error: e, ParkReason: r,
								LastVideoSeq: last, AutoRetryCount: n, ManuallyAdded: manual}
							if !writtenBeforeOfflineParkReasons(job) {
								continue
							}
							rows++
							if got, want := TwitchAutoRetryPending(job), preParkReasonRetryPending(job); got != want {
								t.Errorf("TwitchAutoRetryPending changed on %+v: %v, was %v", job, got, want)
							}
							if got, want := PendingAutoAction(job), preParkReasonPendingAutoAction(job); got != want {
								t.Errorf("PendingAutoAction changed on %+v: %q, was %q", job, got, want)
							}
						}
					}
				}
			}
		}
	}
	if rows == 0 {
		t.Fatal("no row was checked")
	}

	// The rows today's writers produce: the reason beside the text. Each
	// answers as the bare text it replaces did — and does so under any
	// wording, the text routing nothing any more.
	for _, shape := range []struct {
		reason database.ParkReason
		was    string
	}{
		{database.ParkReasonTwitchOffline, preParkReasonOfflineText},
		{database.ParkReasonTwitchEndedOffline, preParkReasonEndedOfflineText},
	} {
		for _, st := range statuses {
			for _, e := range append([]string{"reworded"}, errs...) {
				for _, last := range []*int{nil, &seq} {
					for _, n := range []int{0, 1, 2, 3} {
						for _, manual := range []bool{false, true} {
							job := &database.Job{Platform: "twitch", Status: st, Error: e, ParkReason: shape.reason,
								LastVideoSeq: last, AutoRetryCount: n, ManuallyAdded: manual}
							was := *job
							was.Error, was.ParkReason = shape.was, database.ParkReasonNone
							if got, want := TwitchAutoRetryPending(job), preParkReasonRetryPending(&was); got != want {
								t.Errorf("TwitchAutoRetryPending on %+v: %v, the text-keyed row answered %v", job, got, want)
							}
							if got, want := PendingAutoAction(job), preParkReasonPendingAutoAction(&was); got != want {
								t.Errorf("PendingAutoAction on %+v: %q, the text-keyed row answered %q", job, got, want)
							}
						}
					}
				}
			}
		}
	}
}

// TestLegacyOfflineTextIsTheWritersText holds the frozen text the fallback
// and the boot backfill match (database.LegacyTwitchOfflineError) to the text
// the worker writes, for as long as an older binary may run on this database
// and key on it. Once the fallback is removed (the release after the one
// that introduced the park reason) the text routes nothing and this test goes
// with it.
func TestLegacyOfflineTextIsTheWritersText(t *testing.T) {
	if TwitchOfflineErrMsg != database.LegacyTwitchOfflineError || preParkReasonOfflineText != database.LegacyTwitchOfflineError {
		t.Errorf("TwitchOfflineErrMsg = %q, the frozen legacy text is %q", TwitchOfflineErrMsg, database.LegacyTwitchOfflineError)
	}
	if preParkReasonEndedOfflineText != database.LegacyTwitchEndedOfflineError {
		t.Errorf("the ended-offline text pinned here is %q, the frozen legacy text is %q", preParkReasonEndedOfflineText, database.LegacyTwitchEndedOfflineError)
	}
}
