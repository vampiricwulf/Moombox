package worker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// fakeStreamInfoSource replays a scripted sequence of GetStreamInfo answers
// and counts the calls.
type fakeStreamInfoSource struct {
	answers []struct {
		info *twitch.TwitchStreamInfo
		err  error
	}
	calls int
}

func (f *fakeStreamInfoSource) next() (*twitch.TwitchStreamInfo, error) {
	i := f.calls
	f.calls++
	if i >= len(f.answers) {
		i = len(f.answers) - 1
	}
	return f.answers[i].info, f.answers[i].err
}

func liveInfo() *twitch.TwitchStreamInfo {
	return &twitch.TwitchStreamInfo{IsLive: true, StreamID: "s1"}
}

// TestConfirmTwitchStreamInfo pins O-C's two-sample rule.
//
// Mutants, and the rows each one kills:
//   - returning the first sample unconditionally kills rows 2, 3 and 5 (and
//     TestConfirmTwitchStreamInfoHonoursCancellation): ONE nil reads as
//     "ended" and truncates a live broadcast — the sweep-2 ENGINE-3 defect.
//   - sampling twice even when the first says live kills row 1: every consult
//     on a healthy stream pays an extra GQL round trip.
//   - swallowing the second sample's error kills row 5: an unreachable API
//     reads as "ended" instead of deferring the verdict.
//
// Row 4 (a first-sample error) is killed by the first mutant too — it is the
// only row that pins the SHORT-circuit on an error, so it is listed here
// rather than left unattributed.
func TestConfirmTwitchStreamInfo(t *testing.T) {
	type answer = struct {
		info *twitch.TwitchStreamInfo
		err  error
	}
	slotErr := errors.New("twitch stream metadata slot unavailable")

	for _, tc := range []struct {
		name      string
		answers   []answer
		wantCalls int
		wantLive  bool
		wantErr   bool
	}{
		{"live on the first sample stops there", []answer{{liveInfo(), nil}}, 1, true, false},
		{"one nil is not a verdict", []answer{{nil, nil}, {liveInfo(), nil}}, 2, true, false},
		{"two nils confirm the end", []answer{{nil, nil}, {nil, nil}}, 2, false, false},
		{"first-sample error defers", []answer{{nil, slotErr}}, 1, false, true},
		{"second-sample error defers", []answer{{nil, nil}, {nil, slotErr}}, 2, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &fakeStreamInfoSource{answers: tc.answers}
			prev := twitchEndConfirmDelay
			t.Cleanup(func() { twitchEndConfirmDelay = prev })
			twitchEndConfirmDelay = time.Millisecond

			info, err := confirmTwitchLiveness(context.Background(), src.next)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got := info != nil && info.IsLive; got != tc.wantLive {
				t.Errorf("live = %v, want %v", got, tc.wantLive)
			}
			if src.calls != tc.wantCalls {
				t.Errorf("GetStreamInfo calls = %d, want %d", src.calls, tc.wantCalls)
			}
		})
	}
}

// TestConfirmTwitchStreamInfoHonoursCancellation pins that a cancelled wait
// between the two samples defers rather than concluding "ended" — a shutdown
// must never be read as the end of a broadcast.
//
// Mutant: ignoring utils.Sleep's error — a shutdown landing in the 5 s gap
// finalizes the recording.
func TestConfirmTwitchStreamInfoHonoursCancellation(t *testing.T) {
	prev := twitchEndConfirmDelay
	t.Cleanup(func() { twitchEndConfirmDelay = prev })
	twitchEndConfirmDelay = time.Second

	ctx, cancel := context.WithCancel(context.Background())
	src := &fakeStreamInfoSource{answers: []struct {
		info *twitch.TwitchStreamInfo
		err  error
	}{{nil, nil}}}
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()

	if _, err := confirmTwitchLiveness(ctx, src.next); err == nil {
		t.Fatal("confirmTwitchLiveness = nil error on a cancelled wait, want the cancellation")
	}
	if src.calls != 1 {
		t.Errorf("GetStreamInfo calls = %d, want 1 (the second sample must not run)", src.calls)
	}
}

// scriptedConn is a Connectivity the test drives by hand. OnStateChange has
// two live subscribers inside ExecuteTwitch (the offline-cancels-the-session
// registration and waitForOnline's), so it keeps a set rather than one slot.
type scriptedConn struct {
	mu     sync.Mutex
	online bool
	subs   map[int]func(bool)
	nextID int
}

func newScriptedConn() *scriptedConn {
	return &scriptedConn{online: true, subs: map[int]func(bool){}}
}

func (c *scriptedConn) IsOnline() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.online
}

func (c *scriptedConn) OnStateChange(fn func(bool)) func() {
	c.mu.Lock()
	id := c.nextID
	c.nextID++
	c.subs[id] = fn
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		delete(c.subs, id)
		c.mu.Unlock()
	}
}

// set flips the reported state and notifies every subscriber, outside the
// lock — ExecuteTwitch's callback cancels the session from inside it.
func (c *scriptedConn) set(online bool) {
	c.mu.Lock()
	c.online = online
	fns := make([]func(bool), 0, len(c.subs))
	for _, fn := range c.subs {
		fns = append(fns, fn)
	}
	c.mu.Unlock()
	for _, fn := range fns {
		fn(online)
	}
}

// endVerdictHarness is the shared setup for the two ExecuteTwitch exits below:
// a worker + DB, a scripted connectivity monitor, a live Twitch job, and a
// 404 media playlist. A 404 is the ONE engine failure that reaches the
// orchestrator in milliseconds: runHlsLoop consults the stream status on the
// first 404 and a "still live" answer returns ErrQualityLost immediately.
//
// variant.FetchVariantsFn is deliberately left nil so that ErrQualityLost
// reaches the normal-stop branch (the quality/gap split branch is guarded on
// that field being set) — the unknown-verdict exit is what these tests are
// about. The outage test installs it mid-run, at the one point where nothing
// else reads it, so the recovery path can refresh its variant.
type endVerdictHarness struct {
	o       *DownloadOrchestrator
	db      *database.Database
	conn    *scriptedConn
	job     *database.Job
	jobCtx  *JobContext
	variant *TwitchVariantInfo
	checks  atomic.Int32
}

func newEndVerdictHarness(t *testing.T, jobID string) *endVerdictHarness {
	t.Helper()
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	t.Cleanup(dead.Close)

	w, db := testWorkerSetup(t)
	h := &endVerdictHarness{o: w.orchestrator, db: db, conn: newScriptedConn()}
	h.o.conn = h.conn

	h.job = &database.Job{
		ID: jobID, VideoID: jobID, URL: "https://twitch.tv/streamer",
		Platform: "twitch", Status: database.StatusDownloading,
	}
	if _, err := db.AddJob(h.job); err != nil {
		t.Fatal(err)
	}
	h.jobCtx = &JobContext{Job: h.job, DB: db, Config: &JobConfig{}, StagingDir: t.TempDir(), Logger: &discardLogger{}}
	h.variant = &TwitchVariantInfo{
		URL: dead.URL + "/x.m3u8", Name: "720p60", Width: 1280, Height: 720, FPS: 30,
	}
	h.variant.RecheckStreamFn = func(context.Context) (*twitch.TwitchStreamInfo, error) {
		return &twitch.TwitchStreamInfo{IsLive: true}, nil
	}
	return h
}

// TestUnconfirmedEndExitReturnsBeforeTheMuxingStatus drives the real
// ExecuteTwitch onto the unknown-verdict exit — the download failed while the
// re-verify still says the broadcast is live — and pins BOTH halves of that
// exit: it returns the download error (so processJob's setJobError keeps the
// staging dir and its resume sidecar) and it returns before the row is
// advertised as Muxing.
//
// Mutants:
//   - leaving the exit below the `status: Muxing` write (the shipped order
//     before fix round 1): Muxing appears in the status sequence, so the UI
//     shows a job muxing that is on its way to Error.
//   - dropping the latch (never assigning unconfirmedEndErr): ExecuteTwitch
//     falls through to finalize and returns a mux error instead of dlErr, so
//     the ErrQualityLost assertion fails.
func TestUnconfirmedEndExitReturnsBeforeTheMuxingStatus(t *testing.T) {
	h := newEndVerdictHarness(t, "tw_unconfirmed")
	// Both consults — the engine's 404 consult and the exit's re-verify —
	// answer "still live", so the verdict is never confirmed.
	h.variant.CheckStreamFn = func(context.Context) (bool, error) {
		h.checks.Add(1)
		return true, nil
	}

	var mu sync.Mutex
	var statuses []database.JobStatus
	unsubscribe := h.db.OnJobUpdate(func(j *database.Job) {
		if j.ID != h.job.ID {
			return
		}
		mu.Lock()
		statuses = append(statuses, j.Status)
		mu.Unlock()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	err := h.o.ExecuteTwitch(ctx, h.jobCtx, h.variant, false, nil)
	unsubscribe()

	if !errors.Is(err, engine.ErrQualityLost) {
		t.Fatalf("ExecuteTwitch = %v, want the download error (%v) — the unknown-verdict exit must "+
			"return it so the job lands in Error with its staging intact", err, engine.ErrQualityLost)
	}
	if got := h.checks.Load(); got != 2 {
		t.Errorf("CheckStreamFn calls = %d, want 2 (the engine's 404 consult and the exit's re-verify)", got)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, s := range statuses {
		if s == database.StatusMuxing {
			t.Fatalf("the job was advertised as %q on the unknown-verdict exit (sequence %v) — that "+
				"exit returns an error, so it must return BEFORE the Muxing write",
				database.StatusMuxing, statuses)
		}
	}
}

// TestUnconfirmedEndLatchDoesNotOutliveItsSession is fix round 1's Important
// finding, driven through the real session loop: connectivity dies INSIDE the
// unknown-verdict re-verify, so that session latches its error; the outage
// handler then resumes the same job in a fresh session which runs to a clean
// end. The latch belonged to the session that took it — carrying it across
// the resume marks a completed capture as Error and skips its final mux.
//
// Mutant: dropping the `unconfirmedEndErr = nil` reset at the top of the
// session loop — ExecuteTwitch then returns the stale ErrQualityLost from
// session 1 even though session 2 finished cleanly.
func TestUnconfirmedEndLatchDoesNotOutliveItsSession(t *testing.T) {
	var session2Hits atomic.Int32
	ended := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session2Hits.Add(1)
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n" +
			"#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-ENDLIST\n"))
	}))
	defer ended.Close()

	h := newEndVerdictHarness(t, "tw_latch_session")
	h.variant.CheckStreamFn = func(ctx context.Context) (bool, error) {
		switch h.checks.Add(1) {
		case 1:
			// The engine's 404 consult: still live ⇒ ErrQualityLost, which
			// reaches the normal stop because FetchVariantsFn is still nil.
			return true, nil
		case 2:
			// The unknown-verdict re-verify. This is the 5-35 s window the
			// finding is about: the connectivity monitor flips offline
			// inside it and cancels the session, so the check fails and the
			// latch is taken. Installing the variant fetcher here is safe —
			// the download goroutine has already delivered its error and the
			// quality monitor never started (the field was nil at startup).
			h.variant.FetchVariantsFn = func(context.Context) ([]twitch.TwitchHLSVariant, error) {
				return []twitch.TwitchHLSVariant{{
					URL: ended.URL + "/x.m3u8", Name: "720p60", Width: 1280, Height: 720, FPS: 30,
				}}, nil
			}
			h.conn.set(false)
			h.conn.set(true) // back immediately: the recovery wait must not stall the test
			return false, ctx.Err()
		default:
			return true, nil
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	err := h.o.ExecuteTwitch(ctx, h.jobCtx, h.variant, false, nil)

	if session2Hits.Load() == 0 {
		t.Fatal("the outage handler never resumed the job — this test cannot say anything about " +
			"the latch's lifetime without a second session")
	}
	if errors.Is(err, engine.ErrQualityLost) {
		t.Errorf("ExecuteTwitch = %v — session 1's unconfirmed-end latch outlived its session and "+
			"errored a capture that session 2 completed cleanly", err)
	}
}
