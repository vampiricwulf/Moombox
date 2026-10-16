package worker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
	"github.com/vampiricwulf/Moombox/internal/twitch"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// outageWorker is testWorkerSetup with a connectivity monitor wired, as
// NewDownloadWorker wires the real one.
func outageWorker(t *testing.T, online bool) (*DownloadWorker, *database.Database, *fakeConn) {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := &config.MoomboxConfig{}
	cfg.Paths.StagingDirectory = filepath.Join(dir, "staging")
	conn := &fakeConn{}
	conn.online.Store(online)
	w := NewDownloadWorker(db, nil, cfg, &discardLogger{}, &DownloadWorkerDeps{Conn: conn})
	return w, db, conn
}

// shortOutageConfirmWindow shrinks outageConfirmWindow for one test.
func shortOutageConfirmWindow(t *testing.T, d time.Duration) {
	t.Helper()
	old := outageConfirmWindow
	outageConfirmWindow = d
	t.Cleanup(func() { outageConfirmWindow = old })
}

// runJob starts processJob for jobID and returns a channel closed when it
// returns.
func runJob(t *testing.T, w *DownloadWorker, ctx context.Context, jobID string) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("processJob panicked: %v", r)
			}
		}()
		w.processJob(ctx, jobID)
	}()
	return done
}

func awaitDone(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("processJob never returned: %s", what)
	}
}

func jobRow(t *testing.T, db *database.Database, id string) *database.Job {
	t.Helper()
	row, err := db.GetJob(id)
	if err != nil || row == nil {
		t.Fatalf("GetJob(%s) = %v, %v", id, row, err)
	}
	return row
}

// TestOfflineBootWaitsForTheNetworkInPlace is W27-06's boot case. Moombox
// starts with no network (the PC back before the router): the connectivity
// monitor's Start seeds offline, and enqueueExistingJobs re-enqueues every
// Upcoming, Live and Downloading row. processJob ran stream processing on
// each at once, its first fetch failed with the outage's dial error, and the
// row ended in Error for good — a scheduled stream never recorded, the rest
// of an interrupted Twitch capture lost. Now stream processing waits for the
// network, the row keeping its status under the wait's progress line, and
// runs once it is back.
//
// Mutants this kills:
//   - processJob calling processStream directly again → no wait at all
//   - processStreamThroughOutages' offline gate dropped (`if w.offline()`
//     never taken) → stream processing runs offline (no wait line, a call
//     before the network is back)
//   - waitForNetwork not writing networkWaitProgress, or not clearing it →
//     the line never shows, or outlives the wait
//   - waitForNetwork returning before the monitor reports online → stream
//     processing runs offline
func TestOfflineBootWaitsForTheNetworkInPlace(t *testing.T) {
	w, db, conn := outageWorker(t, false)
	ch := "UC_boot"
	addSchedJob(t, db, &ch, "boot_upcoming", database.StatusUpcoming, 0)
	addSchedJob(t, db, &ch, "boot_capture", database.StatusDownloading, 0)
	if _, err := db.AddJob(&database.Job{ID: "tw_314", VideoID: "314", URL: "https://www.twitch.tv/streamer",
		Platform: "twitch", Status: database.StatusLive}); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	w.processStreamFn = func(ctx context.Context, job *database.Job) (*StreamProcessResult, error) {
		calls.Add(1)
		if !conn.IsOnline() {
			return nil, fmt.Errorf("full fetch failed: %w", dialRefused)
		}
		return cancelledResult(), nil // ends the run quietly once it ran
	}

	for _, tc := range []struct {
		id     string
		status database.JobStatus
	}{
		{"boot_upcoming", database.StatusUpcoming},
		{"boot_capture", database.StatusDownloading},
		{"tw_314", database.StatusLive},
	} {
		calls.Store(0)
		conn.set(false)
		done := runJob(t, w, context.Background(), tc.id)
		waitForCond(t, 5*time.Second, tc.id+" waiting for the network", func() bool {
			return jobRow(t, db, tc.id).Progress == networkWaitProgress
		})
		if n := calls.Load(); n != 0 {
			t.Errorf("%s: stream processing ran %d time(s) while offline", tc.id, n)
		}
		if row := jobRow(t, db, tc.id); row.Status != tc.status || row.Error != "" {
			t.Errorf("%s: waiting row reads %s (%q), want the %s it came in with", tc.id, row.Status, row.Error, tc.status)
		}
		conn.set(true)
		awaitDone(t, done, tc.id)
		if n := calls.Load(); n != 1 {
			t.Errorf("%s: stream processing ran %d time(s), want once, after the network returned", tc.id, n)
		}
		row := jobRow(t, db, tc.id)
		if row.Status != tc.status || row.Error != "" {
			t.Errorf("%s: after the wait the row reads %s (%q), want %s", tc.id, row.Status, row.Error, tc.status)
		}
		if row.Progress != "" {
			t.Errorf("%s: the wait's progress line %q outlived it", tc.id, row.Progress)
		}
	}
}

// TestOfflineBootShutdownLeavesTheRowToResume: a shutdown while a job waits
// for the network is a shutdown like any other — the row keeps its status
// and resumes on the next start — rather than a failure. The auditor's probe
// (a ctx that ends while the device is offline throughout) ended both rows in
// Error.
//
// Mutant killed: waitForNetwork's false ignored in waitOutOutage (the run
// going on to stream processing on a dead ctx — processJob's own ctx read
// still routes that to handleCancellation, so the count of stream-processing
// runs is what tells them apart).
func TestOfflineBootShutdownLeavesTheRowToResume(t *testing.T) {
	w, db, _ := outageWorker(t, false)
	ch := "UC_boot"
	addSchedJob(t, db, &ch, "boot_shutdown", database.StatusUpcoming, 0)
	var calls atomic.Int32
	w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
		calls.Add(1)
		return nil, fmt.Errorf("full fetch failed: %w", dialRefused)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	w.processJob(ctx, "boot_shutdown")
	if row := jobRow(t, db, "boot_shutdown"); row.Status != database.StatusUpcoming || row.Error != "" {
		t.Errorf("status = %s (%q), want the Upcoming row left to resume", row.Status, row.Error)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("stream processing ran %d time(s) after the shutdown ended the wait", n)
	}
}

// TestWaitedOutRowThatLeftIsNotProcessed: a run that waited minutes or hours
// for the network re-reads its row before it goes on, as Process re-reads it
// after its fetch. A row that left the processable states meanwhile is not
// stream-processed on the strength of the read made before the wait.
//
// Mutant killed: waitOutOutage not asking ShouldProcess of the re-read row
// (`return cur != nil`) → stream processing runs for the finished row.
func TestWaitedOutRowThatLeftIsNotProcessed(t *testing.T) {
	w, db, conn := outageWorker(t, false)
	ch := "UC_boot"
	addSchedJob(t, db, &ch, "boot_left", database.StatusUpcoming, 0)
	var calls atomic.Int32
	w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
		calls.Add(1)
		return cancelledResult(), nil
	}
	done := runJob(t, w, context.Background(), "boot_left")
	waitForCond(t, 5*time.Second, "the network wait", func() bool {
		return jobRow(t, db, "boot_left").Progress == networkWaitProgress
	})
	db.UpdateJobFields("boot_left", map[string]any{"status": database.StatusFinished})
	conn.set(true)
	awaitDone(t, done, "boot_left")
	if n := calls.Load(); n != 0 {
		t.Errorf("stream processing ran %d time(s) for a row that left the queue's states during the wait", n)
	}
	if row := jobRow(t, db, "boot_left"); row.Status != database.StatusFinished {
		t.Errorf("status = %s, want the Finished the row was given", row.Status)
	}
}

// TestFetchMeetingAnOutageWaitsAndRunsAgain is W27-06's mid-run case: a job
// whose first fetch meets an outage the monitor reports waits for the
// network and runs stream processing again from the top, keeping the row's
// status; it used to end in Error.
//
// Mutants this kills:
//   - processStreamThroughOutages returning the failure without asking
//     outageCalled → the row ends in Error
//   - outageCalled skipping its read of the monitor after subscribing (the
//     outage here is called before the subscription exists, so no
//     transition reaches it) → the window runs out and the row ends in Error
func TestFetchMeetingAnOutageWaitsAndRunsAgain(t *testing.T) {
	shortOutageConfirmWindow(t, 50*time.Millisecond)
	w, db, conn := outageWorker(t, true)
	ch := "UC_mid"
	addSchedJob(t, db, &ch, "mid_upcoming", database.StatusUpcoming, 0)
	var calls atomic.Int32
	w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
		if calls.Add(1) == 1 {
			conn.online.Store(false) // the monitor has called it, no callback needed
			return nil, fmt.Errorf("full fetch failed: %w", dialRefused)
		}
		return cancelledResult(), nil
	}
	done := runJob(t, w, context.Background(), "mid_upcoming")
	waitForCond(t, 5*time.Second, "the network wait", func() bool {
		return jobRow(t, db, "mid_upcoming").Progress == networkWaitProgress
	})
	if row := jobRow(t, db, "mid_upcoming"); row.Status != database.StatusUpcoming || row.Error != "" {
		t.Errorf("waiting row reads %s (%q), want Upcoming", row.Status, row.Error)
	}
	conn.set(true)
	awaitDone(t, done, "mid_upcoming")
	if n := calls.Load(); n != 2 {
		t.Errorf("stream processing ran %d time(s), want twice: the failed fetch, then once the network was back", n)
	}
	if row := jobRow(t, db, "mid_upcoming"); row.Status != database.StatusUpcoming || row.Error != "" {
		t.Errorf("status = %s (%q), want Upcoming", row.Status, row.Error)
	}
}

// TestTransportFailureTheMonitorCallsLate: the monitor needs two failed polls
// to call an outage, so a fetch that met its first seconds fails while the
// monitor still reports online. outageCalled waits up to outageConfirmWindow
// for the call; one that comes — here the monitor calls the outage and the
// network is back before the window is up — is waited out like any other.
//
// Mutant killed: outageCalled not hearing the monitor's transitions (the
// `<-called` arm dropped) → the window runs out and the row ends in Error.
func TestTransportFailureTheMonitorCallsLate(t *testing.T) {
	shortOutageConfirmWindow(t, 2*time.Second)
	w, db, conn := outageWorker(t, true)
	ch := "UC_late"
	addSchedJob(t, db, &ch, "late_upcoming", database.StatusUpcoming, 0)
	var calls atomic.Int32
	w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
		if calls.Add(1) == 1 {
			go func() {
				defer func() { _ = recover() }()
				time.Sleep(20 * time.Millisecond)
				conn.set(false)
				time.Sleep(20 * time.Millisecond)
				conn.set(true)
			}()
			return nil, fmt.Errorf("full fetch failed: %w", dialRefused)
		}
		return cancelledResult(), nil
	}
	w.processJob(context.Background(), "late_upcoming")
	if n := calls.Load(); n != 2 {
		t.Errorf("stream processing ran %d time(s), want twice", n)
	}
	if row := jobRow(t, db, "late_upcoming"); row.Status != database.StatusUpcoming || row.Error != "" {
		t.Errorf("status = %s (%q), want Upcoming", row.Status, row.Error)
	}
}

// TestFailuresThatAreNotTheOutageAreRecorded pins the other side: what
// processJob still records at once. A transport failure the monitor never
// calls an outage is the platform's failure on a network that is up, and is
// recorded once the window is up — never waited on for ever; an answer from
// the service (a 404) is a verdict on the video, recorded at once even with
// the monitor reporting offline.
//
// Mutants this kills:
//   - outageCalled answering true when its window runs out → the first job
//     never ends
//   - outageCalled not asking isTransportFailure → the 404 met offline is
//     waited on instead of recorded
func TestFailuresThatAreNotTheOutageAreRecorded(t *testing.T) {
	shortOutageConfirmWindow(t, 50*time.Millisecond)
	t.Run("a transport failure the monitor never calls", func(t *testing.T) {
		w, db, _ := outageWorker(t, true)
		ch := "UC_up"
		addSchedJob(t, db, &ch, "up_upcoming", database.StatusUpcoming, 0)
		w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
			return nil, fmt.Errorf("full fetch failed: %w", dialRefused)
		}
		awaitDone(t, runJob(t, w, context.Background(), "up_upcoming"), "up_upcoming")
		if row := jobRow(t, db, "up_upcoming"); row.Status != database.StatusError {
			t.Errorf("status = %s, want Error", row.Status)
		}
	})
	t.Run("an answer from the service met offline", func(t *testing.T) {
		w, db, conn := outageWorker(t, true)
		ch := "UC_up"
		addSchedJob(t, db, &ch, "gone_upcoming", database.StatusUpcoming, 0)
		w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
			conn.online.Store(false)
			return nil, errors.New("full fetch failed: WEB API error: HTTP 404")
		}
		awaitDone(t, runJob(t, w, context.Background(), "gone_upcoming"), "gone_upcoming")
		if row := jobRow(t, db, "gone_upcoming"); row.Status != database.StatusError {
			t.Errorf("status = %s (progress %q), want Error", row.Status, row.Progress)
		}
	})
}

// TestTwitchTransportFailureIsMarkedRecoverable is W27-06's Twitch half. A
// Twitch live job whose stream processing never reached Twitch, on a network
// the monitor says is up, ends in Error marked ParkReasonTwitchUnreachable —
// the marker the Twitch monitor's recovery keys on, as it keys on
// TwitchOfflineErrMsg — where its error used to be the transport failure's
// own text, which the recovery never takes. Nothing else is marked: a Twitch
// VOD (the recovery is for live broadcasts), an answer from Twitch, a
// YouTube job.
//
// Mutants this kills:
//   - parkReasonForError not reading twitchUnreachableError → no marker
//   - processStreamThroughOutages returning the error unmarked → no marker
//   - markTwitchUnreachable ignoring the tw_v prefix → the VOD is marked
//   - markTwitchUnreachable ignoring isTransportFailure → the 503 is marked
//   - markTwitchUnreachable ignoring the platform → the YouTube job is marked
func TestTwitchTransportFailureIsMarkedRecoverable(t *testing.T) {
	shortOutageConfirmWindow(t, 20*time.Millisecond)
	for _, tc := range []struct {
		name, id, videoID, platform string
		err                         error
		want                        database.ParkReason
	}{
		{"a Twitch live job that never reached Twitch", "tw_271", "271", "twitch",
			fmt.Errorf("twitch stream info: gql exhausted 3 retries: %w", dialRefused), database.ParkReasonTwitchUnreachable},
		{"a Twitch VOD", "tw_v272", "tw_v272", "twitch",
			fmt.Errorf("twitch VOD error: %w", dialRefused), database.ParkReasonNone},
		{"an answer from Twitch", "tw_273", "273", "twitch",
			errors.New("twitch stream info: gql http 503"), database.ParkReasonNone},
		{"a YouTube job", "yt_274", "yt_274", "youtube",
			fmt.Errorf("full fetch failed: %w", dialRefused), database.ParkReasonNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db, _ := outageWorker(t, true)
			if _, err := db.AddJob(&database.Job{ID: tc.id, VideoID: tc.videoID, URL: "u",
				Platform: tc.platform, Status: database.StatusLive}); err != nil {
				t.Fatal(err)
			}
			w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
				return nil, tc.err
			}
			w.processJob(context.Background(), tc.id)
			row := jobRow(t, db, tc.id)
			if row.Status != database.StatusError {
				t.Fatalf("status = %s, want Error", row.Status)
			}
			if row.ParkReason != tc.want {
				t.Errorf("park_reason = %q, want %q", row.ParkReason, tc.want)
			}
			if row.Error != tc.err.Error() {
				t.Errorf("error = %q, want the failure's own text %q", row.Error, tc.err.Error())
			}
		})
	}
}

// TestTwitchUnreachableRetryIsNotNotified pins setJobError's retryLikely
// mirror of monitor.isRecoverableTwitchError: a marked failure on a job the
// recovery has already retried, budget left, is the retry's to report — as a
// TwitchOfflineErrMsg flap is — so no Job Failed goes out for it.
//
// Mutant killed: retryLikely keyed on TwitchOfflineErrMsg alone → a Job
// Failed for a failure the monitor is about to retry.
func TestTwitchUnreachableRetryIsNotNotified(t *testing.T) {
	w, db := testWorkerSetup(t)
	rec := notificationtest.New()
	w.notifier = rec
	job := &database.Job{ID: "tw_E8", VideoID: "E8", Platform: "twitch", Title: "Unreachable",
		Status: database.StatusLive, AutoRetryCount: 1}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	w.setJobError(job, w.markTwitchUnreachable(job, fmt.Errorf("twitch stream info: %w", dialRefused)))
	if calls := rec.ByEvent("error"); len(calls) != 0 {
		t.Errorf("a retry-suppressed failure sent %+v, want nothing", calls)
	}
}

// TestStaleReextractionMeetingAnOutageWaits: the re-extraction a stale VOD
// makes once its slots are held is a fetch like the first, and one that
// meets an outage waits for the network and is made again — the VOD keeps
// its status — instead of ending in Error over a fetch the outage failed.
//
// Mutant killed: processJob calling refreshStaleVodInfo directly again → the
// manually added VOD ends in Error.
func TestStaleReextractionMeetingAnOutageWaits(t *testing.T) {
	shortOutageConfirmWindow(t, 50*time.Millisecond)
	w, db, conn := outageWorker(t, true)
	addSchedJob(t, db, nil, "stale_manual", database.StatusUpcoming, 0)
	w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
		return &StreamProcessResult{ShouldDownload: true, IsVod: true, VideoInfo: vodInfoAt("http://127.0.0.1:1", 1, 1)}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	w.refreshVodInfoFn = func(context.Context, *database.Job) (*youtube.VideoInfo, error) {
		if calls.Add(1) == 1 {
			conn.online.Store(false)
			return nil, dialRefused
		}
		cancel() // stop the run here; what follows the refresh is not under test
		return nil, context.Canceled
	}
	done := runJob(t, w, ctx, "stale_manual")
	waitForCond(t, 5*time.Second, "the network wait", func() bool {
		return jobRow(t, db, "stale_manual").Progress == networkWaitProgress
	})
	conn.set(true)
	awaitDone(t, done, "stale_manual")
	if n := calls.Load(); n != 2 {
		t.Errorf("re-extraction ran %d time(s), want twice", n)
	}
	if row := jobRow(t, db, "stale_manual"); row.Status != database.StatusUpcoming || row.Error != "" {
		t.Errorf("status = %s (%q), want Upcoming", row.Status, row.Error)
	}
}

// TestStaleReextractionWaitForARowThatLeft: a re-extraction's outage wait,
// like stream processing's, ends the run when the row left the queue's
// states meanwhile, and the run is a cancelled one — not a failure recorded
// over whatever the row now is.
//
// Mutant killed: processJob's refresh branch not routing ErrCancelled to
// handleCancellation → setJobError writes Error over the Finished row.
func TestStaleReextractionWaitForARowThatLeft(t *testing.T) {
	shortOutageConfirmWindow(t, 50*time.Millisecond)
	w, db, conn := outageWorker(t, true)
	addSchedJob(t, db, nil, "stale_left", database.StatusUpcoming, 0)
	w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
		return &StreamProcessResult{ShouldDownload: true, IsVod: true, VideoInfo: vodInfoAt("http://127.0.0.1:1", 1, 1)}, nil
	}
	var calls atomic.Int32
	w.refreshVodInfoFn = func(context.Context, *database.Job) (*youtube.VideoInfo, error) {
		calls.Add(1)
		conn.online.Store(false)
		return nil, dialRefused
	}
	done := runJob(t, w, context.Background(), "stale_left")
	waitForCond(t, 5*time.Second, "the network wait", func() bool {
		return jobRow(t, db, "stale_left").Progress == networkWaitProgress
	})
	db.UpdateJobFields("stale_left", map[string]any{"status": database.StatusFinished})
	conn.set(true)
	awaitDone(t, done, "stale_left")
	if n := calls.Load(); n != 1 {
		t.Errorf("re-extraction ran %d time(s), want once: the row left during the wait", n)
	}
	if row := jobRow(t, db, "stale_left"); row.Status != database.StatusFinished || row.Error != "" {
		t.Errorf("status = %s (%q), want the Finished the row was given", row.Status, row.Error)
	}
}

// TestTwitchVodFetchError pins which Twitch VOD fetch failures stream
// processing returns as an error rather than as a verdict: one that never
// reached Twitch (so processJob can wait an outage out) and a cancelled run
// (so processJob routes it to handleCancellation); an answer from Twitch
// stays the result it was.
//
// Mutants this kills:
//   - the transport check dropped → the dial error comes back nil
//   - the ctx check dropped → the cancelled fetch comes back nil
//   - either inverted → the 404 comes back as an error
func TestTwitchVodFetchError(t *testing.T) {
	live := context.Background()
	if err := twitchVodFetchError(live, "twitch VOD error", dialRefused); err == nil || !errors.Is(err, dialRefused) {
		t.Errorf("transport failure: %v, want the wrapped dial error", err)
	} else if err.Error() != "twitch VOD error: "+dialRefused.Error() {
		t.Errorf("message = %q, want the result form's", err.Error())
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := twitchVodFetchError(cancelled, "twitch VOD error", context.Canceled); err == nil {
		t.Error("a cancelled run's fetch came back as a verdict")
	}
	if err := twitchVodFetchError(live, "twitch VOD error", errors.New("gql http 404")); err != nil {
		t.Errorf("an answer from Twitch came back as an error: %v", err)
	}
}

// TestTwitchVodShutdownMidFetchLeavesTheRow: a shutdown in the middle of a
// Twitch VOD's info fetch ended the VOD in Error ("twitch VOD error: context
// canceled") — the result form went to setJobError, whose Cancelled guard is
// for an operator's Cancel, not a shutdown. It now reaches
// handleCancellation, which leaves the row to resume on the next start.
//
// Mutant killed: twitchVodFetchError's ctx check dropped.
func TestTwitchVodShutdownMidFetchLeavesTheRow(t *testing.T) {
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		select {
		case arrived <- struct{}{}:
		default:
		}
		// The request outlives the run's cancel, so the handler is released
		// by the test too, ahead of Close (Cleanup runs last-first).
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	oldGQL := constants.TwitchURLs.GQL
	constants.TwitchURLs.GQL = srv.URL + "/gql"
	t.Cleanup(func() { constants.TwitchURLs.GQL = oldGQL })

	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := &config.MoomboxConfig{}
	cfg.Paths.StagingDirectory = filepath.Join(dir, "staging")
	w := NewDownloadWorker(db, nil, cfg, &discardLogger{}, &DownloadWorkerDeps{TwitchService: twitch.NewService(nil, &discardLogger{})})
	if _, err := db.AddJob(&database.Job{ID: "tw_v99", VideoID: "tw_v99", URL: "https://www.twitch.tv/videos/99",
		Platform: "twitch", Status: database.StatusUpcoming}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := runJob(t, w, ctx, "tw_v99")
	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("the VOD info fetch never reached the server")
	}
	cancel()
	awaitDone(t, done, "tw_v99")
	if row := jobRow(t, db, "tw_v99"); row.Status != database.StatusUpcoming || row.Error != "" {
		t.Errorf("status = %s (%q), want the Upcoming row left to resume", row.Status, row.Error)
	}
}
