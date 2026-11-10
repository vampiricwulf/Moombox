package worker

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/jobstate"
)

// offlineRefusal is the error processJob hands setJobError for a Twitch job
// whose channel was offline at stream processing: the stream processor's
// refusal (processTwitchLive's offline branch) through AsError, as the run
// builds it.
func offlineRefusal(job *database.Job, stagingBase string) error {
	msg, reason := twitchOfflineError(job, stagingBase)
	return (&StreamProcessResult{Error: msg, ParkReason: reason}).AsError()
}

// TestOfflineRefusalCarriesItsParkReasonAndItsExactText pins the writer's
// half of owner ruling R-I1: the refusal names its park reason beside the
// text, AsError carries it to parkReasonForError, and the text setJobError
// stores is byte for byte what it was — the operator reads it, and the
// release before the park reasons keys on it.
//
// Mutants: AsError dropping the reason (parkReasonForError answers none);
// AsError decorating the text as it does for a sentinel (the stored error is
// no longer the frozen text); the two reasons crossed in twitchOfflineError.
func TestOfflineRefusalCarriesItsParkReasonAndItsExactText(t *testing.T) {
	base := t.TempDir()
	stageMediaFor(t, base, "staged")
	for _, tc := range []struct {
		name   string
		job    *database.Job
		text   string
		reason database.ParkReason
	}{
		{"the offline flap", &database.Job{ID: "empty", Status: database.StatusLive},
			database.LegacyTwitchOfflineError, database.ParkReasonTwitchOffline},
		{"the broadcast ended while offline", &database.Job{ID: "staged", Status: database.StatusDownloading},
			database.LegacyTwitchEndedOfflineError, database.ParkReasonTwitchEndedOffline},
	} {
		err := offlineRefusal(tc.job, base)
		if err == nil || err.Error() != tc.text {
			t.Errorf("%s: error = %v, want exactly %q", tc.name, err, tc.text)
		}
		if got := parkReasonForError(err); got != tc.reason {
			t.Errorf("%s: parkReasonForError = %q, want %q", tc.name, got, tc.reason)
		}
		if cookiesStatusError(err) || errors.Is(err, ErrNonActionable) || errors.Is(err, ErrCancelled) {
			t.Errorf("%s: the refusal reads as another category", tc.name)
		}
	}
	// A result that names no reason is the plain error it always was.
	if got := parkReasonForError((&StreamProcessResult{Error: "boom"}).AsError()); got != database.ParkReasonNone {
		t.Errorf("a plain refusal: parkReasonForError = %q, want none", got)
	}
	// The same text from any other producer is not the flap: only the
	// stream processor's refusal names the reason.
	if got := parkReasonForError(errors.New(TwitchOfflineErrMsg)); got != database.ParkReasonNone {
		t.Errorf("the bare text: parkReasonForError = %q, want none — the reason is never inferred from the text", got)
	}
	// A cookie sentinel still parks at COOKIES? under its own reason.
	parked := (&StreamProcessResult{Error: "sub only", ErrSentinel: ErrCookiesRequired, ParkReason: database.ParkReasonTwitchOffline}).AsError()
	if got := parkReasonForError(parked); got != database.ParkReasonAuth || !cookiesStatusError(parked) {
		t.Errorf("a COOKIES? refusal: reason %q, cookies %v — want auth, true", got, cookiesStatusError(parked))
	}
}

// TestSetJobErrorRecordsTheOfflineParkReasonInTheErrorWrite: the reason lands
// in the write that makes the row an Error, so no reader ever sees the Error
// without it, and the row is the one the monitor's recovery takes — with no
// Job Failed while a retry is still likely.
//
// Mutants: the reason written by a second write after the Error one (the
// first Error the subscriber sees has none); parkReasonForError not reading
// the refusal's reason (the row has none, and with a reworded text nothing
// would recover it).
func TestSetJobErrorRecordsTheOfflineParkReasonInTheErrorWrite(t *testing.T) {
	w, db := testWorkerSetup(t)
	base := t.TempDir()
	stageMediaFor(t, base, "tw_ended")

	var mu sync.Mutex
	firstError := map[string]database.ParkReason{}
	unsub := db.OnJobUpdate(func(j *database.Job) {
		mu.Lock()
		defer mu.Unlock()
		if _, seen := firstError[j.ID]; !seen && j.Status == database.StatusError {
			firstError[j.ID] = j.ParkReason
		}
	})
	defer unsub()

	for _, tc := range []struct {
		id     string
		status database.JobStatus
		text   string
		reason database.ParkReason
		retry  bool
	}{
		{"tw_flap", database.StatusLive, database.LegacyTwitchOfflineError, database.ParkReasonTwitchOffline, true},
		{"tw_ended", database.StatusDownloading, database.LegacyTwitchEndedOfflineError, database.ParkReasonTwitchEndedOffline, false},
	} {
		job := &database.Job{ID: tc.id, VideoID: tc.id[3:], URL: "https://twitch.tv/streamer", Platform: "twitch", Status: tc.status}
		if _, err := db.AddJob(job); err != nil {
			t.Fatal(err)
		}
		w.setJobError(job, offlineRefusal(job, base))

		got, err := db.GetJob(tc.id)
		if err != nil || got == nil {
			t.Fatalf("%s: GetJob: %v", tc.id, err)
		}
		if got.Status != database.StatusError || got.Error != tc.text || got.ParkReason != tc.reason {
			t.Errorf("%s: row = %s / %q / %q, want Error / %q / %q", tc.id, got.Status, got.Error, got.ParkReason, tc.text, tc.reason)
		}
		if jobstate.TwitchAutoRetryPending(got) != tc.retry {
			t.Errorf("%s: TwitchAutoRetryPending = %v, want %v", tc.id, !tc.retry, tc.retry)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			mu.Lock()
			reason, seen := firstError[tc.id]
			mu.Unlock()
			if seen {
				if reason != tc.reason {
					t.Errorf("%s: the Error write carried park_reason %q, want %q in that same write", tc.id, reason, tc.reason)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: no Error update was observed", tc.id)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// TestOfflineFlapRowIsReinitialisedByItsParkReasonAlone is the offline flap
// end to end on the worker, with the text taken out of it (R-I1): a row whose
// error has been reworded is still the recovery's by its park reason, the
// automatic re-initialisation resets it to Upcoming with the reason cleared
// and one retry spent, and the flap that follows is recorded the same way —
// until the budget is spent, when the row waits for the operator.
//
// Mutants: TwitchAutoRetryPending keyed on the text (the reworded row is
// refused); reinitResetFields not clearing park_reason (the Upcoming row
// still carries twitch_offline).
func TestOfflineFlapRowIsReinitialisedByItsParkReasonAlone(t *testing.T) {
	w, db := testWorkerSetup(t)
	const id = "tw_9001"
	job := &database.Job{ID: id, VideoID: "9001", URL: "https://twitch.tv/streamer", Platform: "twitch", Status: database.StatusLive}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt <= jobstate.MaxTwitchAutoRetries; attempt++ {
		row, _ := db.GetJob(id)
		w.setJobError(row, offlineRefusal(row, t.TempDir()))
		// The wording changes under the row; the reason is what routes.
		if db.UpdateJobFields(id, map[string]any{"error": "Twitch says the channel is not live"}) == nil {
			t.Fatal("the reworded error was not written")
		}
		row, _ = db.GetJob(id)
		if row.ParkReason != database.ParkReasonTwitchOffline || row.AutoRetryCount != attempt {
			t.Fatalf("attempt %d: row = %q, %d retries", attempt, row.ParkReason, row.AutoRetryCount)
		}
		pending := jobstate.TwitchAutoRetryPending(row)
		if want := attempt < jobstate.MaxTwitchAutoRetries; pending != want {
			t.Fatalf("attempt %d: TwitchAutoRetryPending = %v, want %v", attempt, pending, want)
		}
		if got, want := jobstate.PendingAutoAction(row) == jobstate.AutoActionRetry, pending; got != want {
			t.Errorf("attempt %d: PendingAutoAction retry = %v, want %v", attempt, got, want)
		}
		if !pending {
			break
		}
		if err := w.AutoReinitializeJob(id); err != nil {
			t.Fatalf("attempt %d: AutoReinitializeJob: %v", attempt, err)
		}
		row, _ = db.GetJob(id)
		if row.Status != database.StatusUpcoming || row.ParkReason != database.ParkReasonNone || row.Error != "" ||
			row.AutoRetryCount != attempt+1 {
			t.Fatalf("attempt %d: re-initialised row = %s / %q / %q / %d", attempt, row.Status, row.ParkReason, row.Error, row.AutoRetryCount)
		}
	}
}

// TestEveryExitFromErrorClearsTheOfflineParkReasons: the verbs that take a
// row out of Error clear park_reason for the two offline reasons as for
// every other — Reinitialize (the operator's and the automatic one) and
// Resume — so no later run's row carries a reason its own failure did not
// write. The Mux's clear, in its Muxing write, is pinned where a Mux runs:
// TestMuxedTwitchCaptureKeepsAGapFillPlan.
//
// Mutants: park_reason dropped from reinitResetFields or from ResumeJob's
// write.
func TestEveryExitFromErrorClearsTheOfflineParkReasons(t *testing.T) {
	for _, reason := range []database.ParkReason{database.ParkReasonTwitchOffline, database.ParkReasonTwitchEndedOffline} {
		for _, verb := range []string{"reinitialize", "auto-reinitialize", "resume"} {
			t.Run(string(reason)+"/"+verb, func(t *testing.T) {
				w, db := testWorkerSetup(t)
				const id = "tw_77"
				platform := "twitch"
				if verb == "resume" {
					platform = "youtube" // Resume is YouTube's; the clear is the verb's, whatever the row
				}
				if _, err := db.AddJob(&database.Job{ID: id, VideoID: "77", URL: "https://twitch.tv/streamer",
					Platform: platform, Status: database.StatusError, Error: "x"}); err != nil {
					t.Fatal(err)
				}
				db.UpdateJobFields(id, map[string]any{"park_reason": reason})
				var err error
				switch verb {
				case "reinitialize":
					err = w.ReinitializeJob(id)
				case "auto-reinitialize":
					err = w.AutoReinitializeJob(id)
				case "resume":
					err = w.ResumeJob(id)
				}
				if err != nil {
					t.Fatalf("%s: %v", verb, err)
				}
				w.wg.Wait()
				row, _ := db.GetJob(id)
				if row == nil || row.ParkReason != database.ParkReasonNone {
					t.Errorf("after %s: park_reason = %q (status %s), want cleared", verb, row.ParkReason, row.Status)
				}
			})
		}
	}
}
