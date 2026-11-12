package engine

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// deadAfter is a transport that passes requests to next until the segment
// lastServed has been answered, and fails every request after that — segments
// and head probes alike — with a transport error: an edge that stops
// answering while the rest of the network works.
type deadAfter struct {
	next       http.RoundTripper
	lastServed int
	dead       atomic.Bool
	failed     atomic.Int32
}

func (t *deadAfter) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.dead.Load() {
		t.failed.Add(1)
		return nil, errors.New("dial tcp: i/o timeout")
	}
	resp, err := t.next.RoundTrip(req)
	if seq, convErr := strconv.Atoi(req.URL.Query().Get("sq")); convErr == nil && seq == t.lastServed {
		t.dead.Store(true)
	}
	return resp, err
}

// serveGVSThenDie serves sequences 0..lastServed from a memGVS reporting head,
// then fails every request. Call it outside the bubble.
func serveGVSThenDie(t *testing.T, head, lastServed int) *deadAfter {
	t.Helper()
	serveGVS(t, constHead(head), func(*http.Request, int, int) gvsAnswer { return gvsServe })
	dt := &deadAfter{next: engineHTTPClient.Transport, lastServed: lastServed}
	engineHTTPClient = &http.Client{Transport: dt} // serveGVS's cleanup restores the original
	return dt
}

// runUntilBounded runs the downloader on the bubble's clock and fails the test
// if it is still running after limit: the loop this file pins used to retry a
// transport failure for ever.
func runUntilBounded(t *testing.T, d *SegmentDownloader, limit time.Duration) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), limit)
	defer cancel()
	err := d.Start(ctx)
	if ctx.Err() != nil {
		t.Fatalf("Start still running after %v (returned %v): the transport failure was retried without a bound", limit, err)
	}
	return err
}

// Owner ruling R2-E1: a transport failure that persists while the device is
// online is bounded exactly like a 5xx. The three tests below drive the DASH
// loop, with production delays on a synctest clock, into an edge that stops
// answering after sequence 5 while IsOnline stays true.
//
// Mutant for all three: handleDashError's last branch sleeping genericRetry
// and returning nil (the code before the change) — Start never returns.

// At the live edge of a stream YouTube still calls live, the no-segment gap
// reaches MaxTimeout and the recording is finalized, as it is for a 5xx.
func TestDashTransportFailureAtTheEdgeFinalizesAtMaxTimeout(t *testing.T) {
	const head = 5
	dt := serveGVSThenDie(t, head, head)

	synctest.Test(t, func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "video_stream")
		var checks atomic.Int32
		d := NewSegmentDownloader(DownloaderOptions{
			BaseURL:           gvsBase("edge"),
			OutputFile:        out,
			InitInlineAtStart: true,
			MaxTimeout:        10 * time.Minute,
			IsOnline:          func() bool { return true },
			CheckStreamStatus: func(context.Context) (bool, error) {
				checks.Add(1)
				return false, nil // still live
			},
		})
		start := time.Now()
		if err := runUntilBounded(t, d, time.Hour); err != nil {
			t.Fatalf("Start = %v, want nil (finalized at MaxTimeout)", err)
		}
		if got := d.finishedWith(); got != finishMaxTimeout {
			t.Errorf("finished as %v, want finishMaxTimeout", got)
		}
		if waited := time.Since(start); waited < 10*time.Minute {
			t.Errorf("finalized after %v, want the whole MaxTimeout", waited)
		}
		if checks.Load() == 0 {
			t.Error("CheckStreamStatus never ran during the outage")
		}
		if d.FinalizedBehindHead() {
			t.Error("FinalizedBehindHead latched for a capture that had reached its head")
		}
		wantFile(t, out, gvsRecording(0, head))
		if dt.failed.Load() == 0 {
			t.Fatal("no request failed — the test did not exercise the outage")
		}
	})
}

// Below the head of a still-live stream the segment is known to exist: once
// it has failed MaxSegmentRetries times and the gap has reached the status
// check, the loop hands back for a rebuild on a fresh player response.
func TestDashTransportFailureBelowTheHeadHandsBackForRebuild(t *testing.T) {
	const head, lastServed = 20, 5
	serveGVSThenDie(t, head, lastServed)

	synctest.Test(t, func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "video_stream")
		d := NewSegmentDownloader(DownloaderOptions{
			BaseURL:           gvsBase("below"),
			OutputFile:        out,
			InitInlineAtStart: true,
			MaxTimeout:        10 * time.Minute,
			IsOnline:          func() bool { return true },
			CheckStreamStatus: func(context.Context) (bool, error) { return false, nil },
		})
		start := time.Now()
		err := runUntilBounded(t, d, time.Hour)
		if !errors.Is(err, ErrQualityLost) {
			t.Fatalf("Start = %v, want ErrQualityLost (rebuild on a fresh player response)", err)
		}
		if waited := time.Since(start); waited < streamStatusCheckInterval || waited > 2*time.Minute {
			t.Errorf("handed back after %v, want soon after the %v status-check gap", waited, streamStatusCheckInterval)
		}
		wantFile(t, out, gvsRecording(0, lastServed))
	})
}

// The end-of-stream tail rule keeps its meaning: a transport failure reads no
// X-Head-Seqnum, so when a capture stopped below its head finalizes after such
// a spell, the head it finalized behind is the one from before the outage and
// is not called current.
func TestDashTransportFailureLeavesTheHeadStaleAtFinalize(t *testing.T) {
	const head, lastServed = 8, 5
	serveGVSThenDie(t, head, lastServed)

	synctest.Test(t, func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "video_stream")
		d := NewSegmentDownloader(DownloaderOptions{
			BaseURL:           gvsBase("tail"),
			OutputFile:        out,
			InitInlineAtStart: true,
			MaxTimeout:        10 * time.Minute,
			IsOnline:          func() bool { return true },
			CheckStreamStatus: func(context.Context) (bool, error) { return true, nil }, // over
		})
		if err := runUntilBounded(t, d, time.Hour); err != nil {
			t.Fatalf("Start = %v, want nil", err)
		}
		if got := d.finishedWith(); got != finishBehindHead {
			t.Errorf("finished as %v, want finishBehindHead", got)
		}
		if !d.FinalizedBehindHead() {
			t.Error("FinalizedBehindHead = false for a capture that stopped 3 below its head")
		}
		if d.HeadCurrentAtFinalize() {
			t.Error("HeadCurrentAtFinalize = true: nothing answered after the last segment, so the head is from before the outage")
		}
		wantFile(t, out, gvsRecording(0, lastServed))
	})
}

// A fetch the loop's own cancel cut short (operator Cancel, shutdown) is also
// a status-0 error, and it is not an edge failure. The handler leaves it to
// the loop top: no retry counted, no stream-status check on a dead context and
// no finalize, even with the no-segment gap already past MaxTimeout.
//
// Mutant: handleDashError without its cancel check — CheckStreamStatus runs on
// the dead context and the download is recorded as finished at MaxTimeout.
func TestHandleDashErrorLeavesACancelledFetchToTheLoop(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cancel func(d *SegmentDownloader, cancelCtx context.CancelFunc)
	}{
		{"context cancelled", func(_ *SegmentDownloader, cancelCtx context.CancelFunc) { cancelCtx() }},
		{"cancel flag set", func(d *SegmentDownloader, _ context.CancelFunc) { d.cancelled.Store(true) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checks := 0
			d := NewSegmentDownloader(DownloaderOptions{
				OutputFile: filepath.Join(t.TempDir(), "v"),
				MaxTimeout: time.Minute,
				IsOnline:   func() bool { return true },
				CheckStreamStatus: func(context.Context) (bool, error) {
					checks++
					return false, nil
				},
			})
			d.delays = fastDelays()
			activity := ActivityNone
			d.OnActivity = func(a DownloadActivity) { activity = a }
			d.headSeq.Store(10)
			d.currentSeq.Store(5)
			d.lastHeadProbeTime.StoreNow()                        // no head probe: there is no server
			d.lastSegTime.Store(time.Now().Add(-2 * time.Minute)) // past MaxTimeout
			ctx, cancelCtx := context.WithCancel(t.Context())
			defer cancelCtx()
			tc.cancel(d, cancelCtx)

			rs := &dashRetryState{sameSegRetries: MaxSegmentRetries, lastRetrySeq: 5}
			if err := d.handleDashError(ctx, 0, context.Canceled, rs, true, 60); err != nil {
				t.Fatalf("handleDashError = %v, want nil (the loop top returns the cancel)", err)
			}
			if checks != 0 {
				t.Errorf("CheckStreamStatus ran %d times for a cancelled fetch", checks)
			}
			if got := d.finishedWith(); got != finishNone {
				t.Errorf("finished as %v, want finishNone: a cancel is not a finalize", got)
			}
			if d.FinalizedBehindHead() {
				t.Error("FinalizedBehindHead latched by a cancelled fetch")
			}
			if rs.sameSegRetries != MaxSegmentRetries {
				t.Errorf("sameSegRetries = %d, want %d: a cancelled fetch is not a retry", rs.sameSegRetries, MaxSegmentRetries)
			}
			if activity != ActivityNone {
				t.Errorf("activity = %v, want none", activity)
			}
		})
	}
}

// hangAfter passes requests to next until the segment lastServed has been
// answered, then holds every request until its context ends: an edge that
// accepts the connection and never answers.
type hangAfter struct {
	next       http.RoundTripper
	lastServed int
	dead       atomic.Bool
	hanging    atomic.Int32
}

func (t *hangAfter) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.dead.Load() {
		// Count the held fetch of the next segment only, not the head
		// probes held beside it.
		if seq, convErr := strconv.Atoi(req.URL.Query().Get("sq")); convErr == nil && seq == t.lastServed+1 {
			t.hanging.Add(1)
			defer t.hanging.Add(-1)
		}
		<-req.Context().Done()
		return nil, req.Context().Err()
	}
	resp, err := t.next.RoundTrip(req)
	if seq, convErr := strconv.Atoi(req.URL.Query().Get("sq")); convErr == nil && seq == t.lastServed {
		t.dead.Store(true)
	}
	return resp, err
}

// The same through the loop: the context is cancelled while a fetch hangs,
// with the no-segment gap and the time since the last status check both past
// streamStatusCheckInterval, so the failed fetch would be due a check. Start
// returns the cancel and CheckStreamStatus is never called on the dead context.
func TestDashCancelDuringAHangingFetchReturnsTheCancel(t *testing.T) {
	const head = 5
	serveGVS(t, constHead(head), func(*http.Request, int, int) gvsAnswer { return gvsServe })
	ht := &hangAfter{next: engineHTTPClient.Transport, lastServed: head}
	engineHTTPClient = &http.Client{Transport: ht} // serveGVS's cleanup restores the original

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var lastCheck, lastSeg atomic.Int64
		var deadChecks atomic.Int32
		d := NewSegmentDownloader(DownloaderOptions{
			BaseURL:           gvsBase("cancel"),
			OutputFile:        filepath.Join(t.TempDir(), "video_stream"),
			InitInlineAtStart: true,
			MaxTimeout:        time.Hour,
			IsOnline:          func() bool { return true },
			CheckStreamStatus: func(c context.Context) (bool, error) {
				if c.Err() != nil {
					deadChecks.Add(1)
				}
				lastCheck.Store(time.Now().UnixNano())
				return false, nil // still live
			},
		})
		d.OnProgress = func(DownloadProgress) { lastSeg.Store(time.Now().UnixNano()) }

		done := make(chan error, 1)
		go func() { done <- d.Start(ctx) }()

		since := func(v *atomic.Int64) time.Duration { return time.Since(time.Unix(0, v.Load())) }
		deadline := time.Now().Add(10 * time.Minute)
		for {
			time.Sleep(100 * time.Millisecond)
			if time.Now().After(deadline) {
				t.Fatal("never saw a hanging fetch that was due a status check")
			}
			if ht.hanging.Load() > 0 && lastSeg.Load() != 0 &&
				since(&lastSeg) > streamStatusCheckInterval && since(&lastCheck) > streamStatusCheckInterval {
				break
			}
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("Start = %v, want context.Canceled", err)
		}
		if n := deadChecks.Load(); n != 0 {
			t.Errorf("CheckStreamStatus ran %d times on the cancelled context", n)
		}
		if got := d.finishedWith(); got != finishNone {
			t.Errorf("finished as %v, want finishNone", got)
		}
	})
}
