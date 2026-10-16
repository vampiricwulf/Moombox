package worker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/chat"
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
// of an interrupted Twitch capture lost. Now the fetch that fails on the
// outage waits for the network, the row keeping its status under the wait's
// progress line, and stream processing runs again once it is back.
//
// Mutants this kills:
//   - processJob calling processStream directly again → no wait at all
//   - waitForNetwork not writing networkWaitProgress, or not clearing it →
//     the line never shows, or outlives the wait
//   - waitForNetwork returning before the monitor reports online → stream
//     processing runs again offline
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
		if n := calls.Load(); n != 1 {
			t.Errorf("%s: stream processing ran %d time(s) while offline, want once: the fetch the wait follows", tc.id, n)
		}
		if row := jobRow(t, db, tc.id); row.Status != tc.status || row.Error != "" {
			t.Errorf("%s: waiting row reads %s (%q), want the %s it came in with", tc.id, row.Status, row.Error, tc.status)
		}
		conn.set(true)
		awaitDone(t, done, tc.id)
		if n := calls.Load(); n != 2 {
			t.Errorf("%s: stream processing ran %d time(s), want twice: the failed fetch, then once the network returned", tc.id, n)
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
// going on to stream processing again on a dead ctx — processJob's own ctx
// read still routes that to handleCancellation, so the count of
// stream-processing runs is what tells them apart).
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
	if n := calls.Load(); n != 1 {
		t.Errorf("stream processing ran %d time(s), want once: the fetch the wait follows, and none after the shutdown ended it", n)
	}
}

// TestWronglyOfflineMonitorDoesNotHoldTheFirstFetch: the connectivity monitor
// can report offline with the network up — its probe dials only
// probe_targets, which a firewall can block, and nothing the platforms answer
// brings it back. Stream processing used to wait on its word before the first
// fetch, so every job processJob ran (here an interrupted capture re-enqueued
// at boot) sat under the wait's progress line for as long as the monitor was
// wrong, with nothing ever fetched. A wait now follows a failed fetch only:
// this one, which the network answers, goes ahead at once.
//
// Mutant killed: an up-front `if !w.conn.IsOnline()` wait restored ahead of
// processStream → no call before networkWaitFloor (a minute here).
func TestWronglyOfflineMonitorDoesNotHoldTheFirstFetch(t *testing.T) {
	w, db, _ := outageWorker(t, false) // the monitor says offline throughout
	ch := "UC_wrong"
	addSchedJob(t, db, &ch, "wrong_oracle", database.StatusDownloading, 0)
	var calls atomic.Int32
	w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
		calls.Add(1) // the platform answers: the network is up
		return cancelledResult(), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runJob(t, w, ctx, "wrong_oracle")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		row := jobRow(t, db, "wrong_oracle")
		cancel()
		<-done
		t.Fatalf("processJob still waiting after 5s with the network up (row %s, progress %q, %d stream-processing run(s))",
			row.Status, row.Progress, calls.Load())
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("stream processing ran %d time(s), want once", n)
	}
}

// TestNetworkWaitTriesAgainWhileTheMonitorStaysOffline: a fetch that failed
// while the monitor reports offline waits for the network — and when the
// monitor is wrong it never reports it back. The wait gives the fetch another
// try every networkWaitFloor, as the waiting loops probe at least every
// offlineProbeFloor, so a job the network can serve is not stranded on the
// monitor's word.
//
// Mutant killed: waitForNetwork's floor arm dropped → the wait ends only on
// the monitor, which never comes back; the second fetch never happens.
func TestNetworkWaitTriesAgainWhileTheMonitorStaysOffline(t *testing.T) {
	old := networkWaitFloor
	networkWaitFloor = 50 * time.Millisecond
	t.Cleanup(func() { networkWaitFloor = old })
	w, db, _ := outageWorker(t, false) // offline throughout
	ch := "UC_floor"
	addSchedJob(t, db, &ch, "floor_upcoming", database.StatusUpcoming, 0)
	var calls atomic.Int32
	w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
		if calls.Add(1) == 1 {
			return nil, fmt.Errorf("full fetch failed: %w", dialRefused)
		}
		return cancelledResult(), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	w.processJob(ctx, "floor_upcoming")
	if ctx.Err() != nil {
		t.Fatal("processJob ran until the test's deadline: the wait never let the fetch try again")
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("stream processing ran %d time(s), want twice", n)
	}
	if row := jobRow(t, db, "floor_upcoming"); row.Status != database.StatusUpcoming || row.Error != "" || row.Progress != "" {
		t.Errorf("row = %s (%q, progress %q), want Upcoming with the wait's line cleared", row.Status, row.Error, row.Progress)
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
		if calls.Add(1) == 1 {
			return nil, fmt.Errorf("full fetch failed: %w", dialRefused)
		}
		return cancelledResult(), nil
	}
	done := runJob(t, w, context.Background(), "boot_left")
	waitForCond(t, 5*time.Second, "the network wait", func() bool {
		return jobRow(t, db, "boot_left").Progress == networkWaitProgress
	})
	db.UpdateJobFields("boot_left", map[string]any{"status": database.StatusFinished})
	conn.set(true)
	awaitDone(t, done, "boot_left")
	if n := calls.Load(); n != 1 {
		t.Errorf("stream processing ran %d time(s), want once: not again for a row that left the queue's states during the wait", n)
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

// foundOutageJob adds a freshly discovered Upcoming row that owes its "Stream
// Found" (announce_found), wires a recorder for the processor's sends, and
// parks the waiting loop's probe until the run ends — what follows the first
// fetch is not under test.
func foundOutageJob(t *testing.T, w *DownloadWorker, db *database.Database, id string) *notificationtest.Recorder {
	t.Helper()
	chID := "UC_abc"
	job := &database.Job{ID: id, VideoID: id, Platform: "youtube",
		Title: "Feed Title", ChannelName: "A Channel", Status: database.StatusUpcoming, ChannelID: &chID}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	owe(t, db, job)
	rec := notificationtest.New()
	w.streamProc.notifier = rec
	w.streamProc.probeStatusFn = func(ctx context.Context, _ string, _ bool) (*youtube.VideoInfo, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return rec
}

// TestOutageWaitedFirstFetchAnnouncesOnce: a discovery owes one "Stream
// Found", sent after its first fetch with what the fetch learned
// (found_announce.go). A first fetch that meets an outage is now waited out
// and made again, and Process used to spend the flag on the failure anyway —
// a feed-title embed with no Starts At, sent into the dead link — so the
// retry's schedule went out on its own as a "YouTube Start Time Confirmed":
// the two embeds the deferral exists to merge. Now the failed fetch leaves the
// flag owed, and the fetch that answers announces once, schedule and all.
//
// Mutant killed: Process taking the flag on a transport failure again (its
// `!isTransportFailure(err) &&` dropped) → a "found" during the wait, and a
// "scheduled" beside it.
func TestOutageWaitedFirstFetchAnnouncesOnce(t *testing.T) {
	shortOutageConfirmWindow(t, 50*time.Millisecond)
	w, db, conn := outageWorker(t, true)
	rec := foundOutageJob(t, w, db, "found_out")
	var calls atomic.Int32
	w.streamProc.getVideoInfoFn = func(context.Context, string) (*youtube.VideoInfo, error) {
		if calls.Add(1) == 1 {
			conn.online.Store(false) // the outage, as the monitor reports it
			return nil, fmt.Errorf("watch page: %w", dialRefused)
		}
		return &youtube.VideoInfo{Title: "Fetched Title", ChannelName: "A Channel",
			ScheduledStartTime: "2026-12-12T18:00:00Z", IsUpcoming: true,
			StreamStatus: youtube.StreamUpcoming, PlayabilityError: youtube.PlayabilityOK}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runJob(t, w, ctx, "found_out")
	waitForCond(t, 5*time.Second, "the network wait", func() bool {
		return jobRow(t, db, "found_out").Progress == networkWaitProgress
	})
	if got := rec.Calls(); len(got) != 0 {
		t.Errorf("sent %+v while the fetch was waited out, want nothing", got)
	}
	conn.set(true)
	waitForCond(t, 5*time.Second, "the retry's announcement", func() bool { return len(rec.ByEvent("found")) > 0 })
	cancel()
	awaitDone(t, done, "found_out")

	found := rec.ByEvent("found")
	if len(found) != 1 {
		t.Fatalf("recorded %d \"found\" embeds, want 1: %+v", len(found), rec.Calls())
	}
	if want := "Upcoming: Fetched Title"; found[0].Description != want {
		t.Errorf("description = %q, want %q: what the fetch that answered learned", found[0].Description, want)
	}
	if _, ok := found[0].Field("Starts At"); !ok {
		t.Error("\"Stream Found\" carries no Starts At")
	}
	if got := rec.ByEvent("scheduled"); len(got) != 0 {
		t.Errorf("recorded %d \"YouTube Start Time Confirmed\" beside the discovery, want 0: %+v", len(got), got)
	}
}

// TestUnwaitedTransportFailureStillAnnounces pins the other half: a first
// fetch that never reached YouTube and is NOT waited out — the monitor never
// calls an outage — ends the job in Error, and the discovery is still
// announced, once, from what the row holds, as any other failed first fetch
// announces (Process now leaves this one to processJob).
//
// Mutant killed: processStreamThroughOutages not calling
// announceFoundOnFailure → the failed discovery is never announced. Dropping
// announceFoundOnFailure's isTransportFailure check is an equivalent mutant:
// every other failure reaches it with the flag already taken by Process.
func TestUnwaitedTransportFailureStillAnnounces(t *testing.T) {
	shortOutageConfirmWindow(t, 20*time.Millisecond)
	w, db, _ := outageWorker(t, true) // online throughout
	rec := foundOutageJob(t, w, db, "found_err")
	w.streamProc.getVideoInfoFn = func(context.Context, string) (*youtube.VideoInfo, error) {
		return nil, fmt.Errorf("watch page: %w", dialRefused)
	}
	w.processJob(context.Background(), "found_err")
	if row := jobRow(t, db, "found_err"); row.Status != database.StatusError {
		t.Fatalf("status = %s, want Error", row.Status)
	}
	found := rec.ByEvent("found")
	if len(found) != 1 {
		t.Fatalf("recorded %d \"found\" embeds, want 1: %+v", len(found), rec.Calls())
	}
	if want := "Found matching stream: Feed Title"; found[0].Description != want {
		t.Errorf("description = %q, want %q: the row's own title", found[0].Description, want)
	}
	if db.TakeAnnounceFound("found_err") {
		t.Error("the flag outlived the announcement")
	}
}

// TestTransportFailedFetchLeavesTheFoundOwed is the unit-level pin of the
// rule in Process: a first fetch that never reached YouTube sends nothing and
// leaves the flag owed, for the fetch processJob makes again or for
// processJob's own announcement.
//
// Mutant killed: the same as TestOutageWaitedFirstFetchAnnouncesOnce's.
func TestTransportFailedFetchLeavesTheFoundOwed(t *testing.T) {
	sp, rec, job := foundTestJob(t)
	sp.getVideoInfoFn = func(context.Context, string) (*youtube.VideoInfo, error) {
		return nil, dialRefused
	}
	owe(t, sp.db, job)
	if _, err := sp.Process(context.Background(), job); err == nil {
		t.Fatal("Process succeeded on a failing fetch")
	}
	if got := rec.Calls(); len(got) != 0 {
		t.Errorf("sent %+v on a fetch that never reached YouTube, want nothing", got)
	}
	if !job.AnnounceFound || !sp.db.TakeAnnounceFound(job.ID) {
		t.Error("the flag was spent on a fetch that never reached YouTube")
	}
}

// closedPortURL is an HLS master URL on a port nothing listens on: its fetch
// fails with connection refused, a transport failure.
func closedPortURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return "http://" + addr + "/master.m3u8"
}

// liveHLSResult is a stream-processing result for a live YouTube broadcast
// whose download is set up from the HLS master playlist at master.
func liveHLSResult(master string) *StreamProcessResult {
	return &StreamProcessResult{ShouldDownload: true, VideoInfo: &youtube.VideoInfo{
		IsLive: true, StreamStatus: youtube.StreamLive, HlsManifestURL: master,
	}}
}

// TestDownloadSetupMeetingAnOutageWaitsAndRunsAgain is W27-06's last door: a
// broadcast whose stream processing succeeded, and whose download setup's
// first fetch (here the HLS master playlist) met the outage — the outage
// began between the two, as at go-live or on a network that flaps as it
// comes back. ExecuteWithChat had written Downloading, the setup failure went
// to setJobError, and the broadcast ended in Error with the monitor reporting
// offline: a YouTube Error has no automatic way out. Now it waits for the
// network holding no slot, keeping its status, and the job runs again from
// stream processing.
//
// Mutants this kills:
//   - processJob's waitOutSetupOutage branch removed → the row ends in Error
//   - waitOutSetupOutage not asking outageCalled (always waiting) → see
//     TestDownloadSetupAnswerIsRecorded
//   - the ReleaseSlots call dropped → the lifecycle slot is held through the
//     wait (and the next pass takes a second one)
//   - the stopEarlyChat call dropped → the waiting room's chat downloader
//     runs on through the wait, beside the one the next pass starts
//   - processJob not going round again after the wait (`continue` replaced by
//     `return`) → stream processing runs once
func TestDownloadSetupMeetingAnOutageWaitsAndRunsAgain(t *testing.T) {
	shortOutageConfirmWindow(t, 50*time.Millisecond)
	master := closedPortURL(t)
	w, db, conn := outageWorker(t, true)
	ch := "UC_setup"
	addSchedJob(t, db, &ch, "setup_live", database.StatusLive, 0)
	// The waiting room's chat, handed to ExecuteWithChat with the result.
	early := chat.NewChatDownloader(chat.ChatDownloaderOptions{VideoID: "setup_live",
		OutputFile: filepath.Join(t.TempDir(), "chat.json")})
	w.streamProc.trackChat(early)
	var calls atomic.Int32
	w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
		if calls.Add(1) == 1 {
			conn.online.Store(false) // the outage begins as the download starts
			res := liveHLSResult(master)
			res.ChatDownloader = early
			return res, nil
		}
		return cancelledResult(), nil // the second pass: end the run here
	}
	done := runJob(t, w, context.Background(), "setup_live")
	waitForCond(t, 5*time.Second, "the network wait", func() bool {
		return jobRow(t, db, "setup_live").Progress == networkWaitProgress
	})
	if row := jobRow(t, db, "setup_live"); row.Status != database.StatusDownloading || row.Error != "" {
		t.Errorf("waiting row reads %s (%q), want the Downloading ExecuteWithChat wrote", row.Status, row.Error)
	}
	if n := w.queue.LifecycleCount(); n != 0 {
		t.Errorf("%d lifecycle slot(s) held through the wait, want 0", n)
	}
	w.streamProc.mu.Lock()
	tracked := len(w.streamProc.activeChats)
	w.streamProc.mu.Unlock()
	if tracked != 0 {
		t.Errorf("the waiting room's chat downloader is still running through the wait (%d tracked)", tracked)
	}
	conn.set(true)
	awaitDone(t, done, "setup_live")
	if n := calls.Load(); n != 2 {
		t.Errorf("stream processing ran %d time(s), want twice: once before the setup failed, once after the wait", n)
	}
	if row := jobRow(t, db, "setup_live"); row.Status != database.StatusDownloading || row.Error != "" || row.Progress != "" {
		t.Errorf("row = %s (%q, progress %q), want Downloading with the wait's line cleared", row.Status, row.Error, row.Progress)
	}
}

// TestDownloadSetupAnswerIsRecorded: what the setup rule waits on is a
// transport failure the monitor calls. An answer — the master playlist's
// server refusing it with a 403 — is recorded as the job's Error at once even
// with the monitor reporting offline, as stream processing's answers are.
//
// Mutant killed: waitOutSetupOutage not asking outageCalled → the 403 is
// waited on and the run never ends. Its errors.Is(dlErr, errDownloadSetup)
// check is not killed by any test: no download failure but setup's brings a
// transport failure back to processJob today (the engine waits its own
// outages out in place), and the check keeps it that way.
func TestDownloadSetupAnswerIsRecorded(t *testing.T) {
	shortOutageConfirmWindow(t, 50*time.Millisecond)
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	w, db, conn := outageWorker(t, true)
	ch := "UC_setup"
	addSchedJob(t, db, &ch, "setup_403", database.StatusLive, 0)
	w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
		conn.online.Store(false)
		return liveHLSResult(srv.URL + "/master.m3u8"), nil
	}
	awaitDone(t, runJob(t, w, context.Background(), "setup_403"), "setup_403")
	if row := jobRow(t, db, "setup_403"); row.Status != database.StatusError {
		t.Errorf("status = %s (progress %q), want Error", row.Status, row.Progress)
	}
}
