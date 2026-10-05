package worker

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

// watchStatuses records every status this job is written with until the
// returned stop func is called, which returns the sequence. The finalize path
// is the only one that writes Muxing, so its presence says which exit
// ExecuteTwitch took.
func (h *endVerdictHarness) watchStatuses() func() []database.JobStatus {
	var mu sync.Mutex
	var seq []database.JobStatus
	unsubscribe := h.db.OnJobUpdate(func(j *database.Job) {
		if j.ID != h.job.ID {
			return
		}
		mu.Lock()
		seq = append(seq, j.Status)
		mu.Unlock()
	})
	return func() []database.JobStatus {
		unsubscribe()
		mu.Lock()
		defer mu.Unlock()
		return append([]database.JobStatus(nil), seq...)
	}
}

func sawMuxing(seq []database.JobStatus) bool {
	for _, s := range seq {
		if s == database.StatusMuxing {
			return true
		}
	}
	return false
}

// errUsher stands in for the master-playlist failures a variant refresh really
// meets — a usher 5xx, a rate limit, an access-token blip — none of which say
// anything about whether the broadcast is over.
var errUsher = errors.New("usher 503")

// TestVariantRefreshFailureOnALiveBroadcastLandsInError is sweep-2 residual R2
// (graded Important by the fix-round-1 re-review, reproduced there as
// "CheckStreamFn calls = 1; status sequence = [Downloading Muxing Muxing
// Muxing]"). ErrQualityLost is only ever produced by the engine's verdictLive
// arm, which post-O-C means the broadcast was CONFIRMED live — so a
// master-playlist refresh failing right after it must not finalize the job.
//
// Mutant: the bare `break` this site used to take (no latchIfUnconfirmed) —
// the job goes down the finalize path instead, so Muxing appears, the
// returned error is the mux failure rather than the refresh failure, and
// CheckStreamFn is called once instead of twice.
func TestVariantRefreshFailureOnALiveBroadcastLandsInError(t *testing.T) {
	h := newEndVerdictHarness(t, "tw_refresh_live")
	fastRefreshRetries(t)
	h.variant.CheckStreamFn = func(context.Context) (bool, error) {
		h.checks.Add(1)
		return true, nil // live at the engine's consult AND at the re-verify
	}
	var refreshes atomic.Int32
	h.variant.FetchVariantsFn = func(context.Context) ([]twitch.TwitchHLSVariant, error) {
		refreshes.Add(1)
		return nil, errUsher
	}

	statuses := h.watchStatuses()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	err := h.o.ExecuteTwitch(ctx, h.jobCtx, h.variant, false, nil)
	seq := statuses()

	if !errors.Is(err, errUsher) {
		t.Fatalf("ExecuteTwitch = %v, want the refresh failure (%v) — a variant refresh that fails "+
			"on a broadcast the consult just confirmed LIVE must land the job in Error, never "+
			"Finished", err, errUsher)
	}
	if got := h.checks.Load(); got != 2 {
		t.Errorf("CheckStreamFn calls = %d, want 2 (the engine's 404 consult and the refresh "+
			"site's re-verify)", got)
	}
	if got := refreshes.Load(); got != liveRefreshAttempts {
		t.Errorf("the refresh was tried %d times before the job gave up, want %d", got, liveRefreshAttempts)
	}
	if sawMuxing(seq) {
		t.Errorf("status sequence = %v — the refresh-failure exit returns an error, so it must not "+
			"advertise the job as Muxing", seq)
	}
	if _, statErr := os.Stat(h.jobCtx.StagingDir); statErr != nil {
		t.Errorf("staging dir: %v — it must survive; the non-nil return is what makes processJob "+
			"take setJobError and skip os.RemoveAll", statErr)
	}
}

// TestVariantRefreshFailureOnAnEndedBroadcastFinalizes is the other half of
// the same rule: the re-verify is what decides, so a refresh failure on a
// broadcast that HAS ended still finalizes exactly as before. (The broadcast
// ends between the engine's consult — which had to answer live to produce
// ErrQualityLost at all — and the re-verify.)
//
// Mutant: latching regardless of the re-verify's answer — the job would then
// land in Error on every refresh failure, so the finalize path is never taken
// and Muxing never appears.
func TestVariantRefreshFailureOnAnEndedBroadcastFinalizes(t *testing.T) {
	h := newEndVerdictHarness(t, "tw_refresh_ended")
	fastRefreshRetries(t)
	h.variant.CheckStreamFn = func(context.Context) (bool, error) {
		return h.checks.Add(1) == 1, nil // live at the consult, over at the re-verify
	}
	h.variant.FetchVariantsFn = func(context.Context) ([]twitch.TwitchHLSVariant, error) {
		return nil, errUsher
	}

	statuses := h.watchStatuses()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	err := h.o.ExecuteTwitch(ctx, h.jobCtx, h.variant, false, nil)
	seq := statuses()

	if errors.Is(err, errUsher) {
		t.Errorf("ExecuteTwitch = %v — a CONFIRMED end must still finalize what was captured, not "+
			"latch the refresh failure", err)
	}
	if got := h.checks.Load(); got != 2 {
		t.Errorf("CheckStreamFn calls = %d, want 2", got)
	}
	if !sawMuxing(seq) {
		t.Errorf("status sequence = %v — a confirmed end takes the finalize path, which flips the "+
			"row to Muxing", seq)
	}
}

// TestAdvanceToNewPartFailureLandsInError covers the other four early exits:
// the part advance failed, which only happens when os.MkdirAll for the next
// part dir failed. Staging is unusable, so the job must land in Error with
// what it has — never Finished. No re-verify is involved: the failure is
// local, not a statement about the broadcast, which is why CheckStreamFn is
// called once (the engine's consult) and not twice.
//
// The quality-split break is the one reachable from a harness; the other
// three are pinned by TestAdvanceToNewPartCallSitesAllLatch.
//
// Mutant: the bare `break` these sites used to take — no latch, so the job
// takes the finalize path (Muxing appears) and the MkdirAll cause never
// reaches the row.
func TestAdvanceToNewPartFailureLandsInError(t *testing.T) {
	h := newEndVerdictHarness(t, "tw_advance_fail")
	h.variant.CheckStreamFn = func(context.Context) (bool, error) {
		h.checks.Add(1)
		return true, nil
	}
	// A DIFFERENT quality, so the refresh routes into the quality-split
	// branch and its advanceToNewPart.
	h.variant.FetchVariantsFn = func(context.Context) ([]twitch.TwitchHLSVariant, error) {
		return []twitch.TwitchHLSVariant{{
			URL: "http://127.0.0.1:1/x.m3u8", Name: "480p", Width: 854, Height: 480, FPS: 30,
		}}, nil
	}
	// The part is younger than minSegmentDuration, so the split discards it
	// and reuses index 0: a plain FILE where seg_0 must be makes MkdirAll
	// fail without touching permissions (which behave differently per OS).
	blocker := filepath.Join(h.jobCtx.StagingDir, "seg_0")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	statuses := h.watchStatuses()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	err := h.o.ExecuteTwitch(ctx, h.jobCtx, h.variant, false, nil)
	seq := statuses()

	if err == nil || !strings.Contains(err.Error(), "advance to new part") {
		t.Fatalf("ExecuteTwitch = %v, want an error naming the failed part advance — a job whose "+
			"staging could not be extended must land in Error, not Finished", err)
	}
	if !strings.Contains(err.Error(), "seg_0") {
		t.Errorf("ExecuteTwitch = %v — the latched error must carry the MkdirAll cause so the row "+
			"says which path failed", err)
	}
	if got := h.checks.Load(); got != 1 {
		t.Errorf("CheckStreamFn calls = %d, want 1 — a local I/O failure is not a stream verdict, "+
			"so this exit must not spend a re-verify on it", got)
	}
	if sawMuxing(seq) {
		t.Errorf("status sequence = %v — this exit returns an error, so it must not advertise the "+
			"job as Muxing", seq)
	}
	if _, statErr := os.Stat(h.jobCtx.StagingDir); statErr != nil {
		t.Errorf("staging dir: %v — it must survive", statErr)
	}
}

// identCall returns n as a call to the plain (non-method) function fnName, or
// nil. advanceToNewPart and latchPartFailure are both closures called by bare
// identifier, so *ast.Ident is the only shape either takes.
func identCall(n ast.Node, fnName string) *ast.CallExpr {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return nil
	}
	id, ok := call.Fun.(*ast.Ident)
	if !ok || id.Name != fnName {
		return nil
	}
	return call
}

// blockLatchesThenBreaks reports whether body calls latchPartFailure at
// statement level and whether its LAST statement is a break (labelled or not).
func blockLatchesThenBreaks(body *ast.BlockStmt) (latches, breaks bool) {
	if body == nil || len(body.List) == 0 {
		return false, false
	}
	for _, stmt := range body.List {
		exprStmt, ok := stmt.(*ast.ExprStmt)
		if !ok {
			continue
		}
		if identCall(exprStmt.X, "latchPartFailure") != nil {
			latches = true
		}
	}
	br, ok := body.List[len(body.List)-1].(*ast.BranchStmt)
	return latches, ok && br.Tok == token.BREAK
}

// TestAdvanceToNewPartCallSitesAllLatch pins the four part-advance breaks a
// harness cannot reach (the gap/init "continue the tail" sub-path, the
// init-segment-change split, the gap split and the post-outage one) alongside
// the quality-split one TestAdvanceToNewPartFailureLandsInError drives.
//
// It reads the SYNTAX TREE, not the text: the round-2 version counted strings
// and could be made to false-pass by dropping a latch and adding a comment
// that quoted `latchPartFailure(err)` — a comment is not a call, so the tree
// cannot be fooled that way — while a behaviour-identical reformat made it
// false-fail. go/ast for a site that cannot be driven with a fake is the
// package's existing technique (stream_processor_early_chat_test.go).
//
// The shape every site must take:
//
//	if err := advanceToNewPart(…); err != nil {
//		latchPartFailure(err)
//		break            // or: break sessionLoop
//	}
//
// A call anywhere else, a body that does not latch, or a body that falls
// through instead of breaking all mean a job whose staging could not be
// extended can still be advertised Finished.
//
// Mutants:
//   - dropping latchPartFailure at any one site: the "does not latch" arm
//     fires, naming that site's line.
//   - adding a comment that quotes latchPartFailure(err) beside it: the pin
//     still fails, which is exactly the false-pass the string version had.
//   - reformatting a site (a multi-line call, a renamed error variable): the
//     pin still passes — the tree is the same.
//   - adding or removing a call site: the count assertion fires, so a new
//     exit cannot be added without a decision about its latch.
func TestAdvanceToNewPartCallSitesAllLatch(t *testing.T) {
	const file = "orchestrator_twitch.go"
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	// Pass 1: every if-statement whose init calls advanceToNewPart.
	guarded := map[token.Pos]bool{}
	ast.Inspect(parsed, func(n ast.Node) bool {
		ifStmt, ok := n.(*ast.IfStmt)
		if !ok || ifStmt.Init == nil {
			return true
		}
		assign, ok := ifStmt.Init.(*ast.AssignStmt)
		if !ok || len(assign.Rhs) != 1 {
			return true
		}
		call := identCall(assign.Rhs[0], "advanceToNewPart")
		if call == nil {
			return true
		}
		guarded[call.Pos()] = true
		where := fset.Position(call.Pos())
		latches, breaks := blockLatchesThenBreaks(ifStmt.Body)
		if !latches {
			t.Errorf("%s: the advanceToNewPart failure branch does not call latchPartFailure — a "+
				"job whose staging could not be extended finalizes Finished instead of Error", where)
		}
		if !breaks {
			t.Errorf("%s: the advanceToNewPart failure branch does not end in a break — the loop "+
				"would carry on against a part dir that does not exist", where)
		}
		return true
	})

	// Pass 2: every call to advanceToNewPart anywhere, so a site in some
	// other position is a finding rather than something the pin never saw.
	var sites []token.Position
	ast.Inspect(parsed, func(n ast.Node) bool {
		call := identCall(n, "advanceToNewPart")
		if call == nil {
			return true
		}
		where := fset.Position(call.Pos())
		sites = append(sites, where)
		if !guarded[call.Pos()] {
			t.Errorf("%s: this advanceToNewPart call is not the init of an `if err := …; err != nil` "+
				"statement, so its failure cannot be latched", where)
		}
		return true
	})

	if len(sites) != 5 {
		t.Errorf("found %d advanceToNewPart call sites (%v), want exactly 5 — a new one needs its "+
			"own latch decision, and a vanished one means this pin is reading the wrong thing",
			len(sites), sites)
	}
}

// fieldCaptureLogger records every log call's message and key/value args. The
// orchestrator logs from background mux goroutines too, so it locks.
type fieldCaptureLogger struct {
	mu    sync.Mutex
	lines [][]any
}

func (l *fieldCaptureLogger) log(msg string, args []any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, append([]any{msg}, args...))
}

func (l *fieldCaptureLogger) Debug(msg string, args ...any) { l.log(msg, args) }
func (l *fieldCaptureLogger) Info(msg string, args ...any)  { l.log(msg, args) }
func (l *fieldCaptureLogger) Warn(msg string, args ...any)  { l.log(msg, args) }
func (l *fieldCaptureLogger) Error(msg string, args ...any) { l.log(msg, args) }

// field returns the value logged under key on the first line whose message is
// msg, and whether that line was logged at all.
func (l *fieldCaptureLogger) field(msg, key string) (any, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		if len(line) == 0 || line[0] != msg {
			continue
		}
		for i := 1; i+1 < len(line); i += 2 {
			if line[i] == key {
				return line[i+1], true
			}
		}
		return nil, true
	}
	return nil, false
}

// TestVariantRefreshFailureLogsTheInnerLoopError is sweep-2 residual R7: the
// refresh-failure line logged only fetchErr, so an operator reading a job that
// stopped there saw the master-playlist failure and nothing about what ended
// the inner loop — and nothing else on that path logs dlErr either (the engine
// returns ErrQualityLost without logging it).
//
// Mutant: dropping the "downloadErr" field from that line — the value lookup
// reports the field missing while the line itself is still present, which is
// what separates this from a wording change.
func TestVariantRefreshFailureLogsTheInnerLoopError(t *testing.T) {
	h := newEndVerdictHarness(t, "tw_refresh_log")
	fastRefreshRetries(t)
	log := &fieldCaptureLogger{}
	h.o.logger = log
	h.variant.CheckStreamFn = func(context.Context) (bool, error) {
		h.checks.Add(1)
		return true, nil
	}
	h.variant.FetchVariantsFn = func(context.Context) ([]twitch.TwitchHLSVariant, error) {
		return nil, errUsher
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_ = h.o.ExecuteTwitch(ctx, h.jobCtx, h.variant, false, nil)

	got, logged := log.field("failed to refresh Twitch variants", "downloadErr")
	if !logged {
		t.Fatal("the refresh failure was never logged — this test is watching the wrong line")
	}
	inner, ok := got.(error)
	if !ok || !errors.Is(inner, engine.ErrQualityLost) {
		t.Errorf("downloadErr = %v, want the error that ended the inner loop (%v) — the operator "+
			"needs both halves, and neither is logged anywhere else on this path",
			got, engine.ErrQualityLost)
	}
}

// TestTwitchVodDownloadFailureLandsInError: a VOD has no live end to confirm,
// so latchIfUnconfirmed used to decline every VOD outright — a failed VOD
// download fell through to finalize, muxed the truncated capture, wrote
// Finished and let processJob delete the staging with its resume sidecar.
// It now returns the download error before the Muxing write, so the job
// lands in Error with staging intact.
//
// Mutant: restore the `isVod ||` short-circuit in latchIfUnconfirmed.
func TestTwitchVodDownloadFailureLandsInError(t *testing.T) {
	h := newEndVerdictHarness(t, "tw_vod_fail")
	statuses := h.watchStatuses()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	err := h.o.ExecuteTwitch(ctx, h.jobCtx, h.variant, true, nil)

	if err == nil {
		t.Fatal("ExecuteTwitch = nil for a VOD whose download failed — it was finalized as Finished")
	}
	if seq := statuses(); sawMuxing(seq) {
		t.Errorf("the failed VOD was advertised Muxing (sequence %v) on its way to Error", seq)
	}
}

// TestTwitchVodRidesOutAnOutage: a connectivity outage mid-VOD used to cancel
// the download and end the job in Error, and the only way on from there —
// Retry — downloaded the whole VOD again. A VOD has no live edge to lose, so
// the engine now waits the outage out and carries on from where it stopped.
//
// While "offline" the server answers every request with a 503, as a dead
// network would; the outage starts on the third segment's first request.
//
// Mutant: registering the offline cancel for VODs again.
func TestTwitchVodRidesOutAnOutage(t *testing.T) {
	const segments = 6
	ts := oneSecondTS(t)
	h := newEndVerdictHarness(t, "tw_vod_outage")
	var offline atomic.Bool
	var mu sync.Mutex
	served := map[string]int{}
	tripped := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if offline.Load() {
			http.Error(w, "unreachable", http.StatusServiceUnavailable)
			return
		}
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			var b strings.Builder
			b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-PLAYLIST-TYPE:VOD\n")
			for i := range segments {
				fmt.Fprintf(&b, "#EXTINF:1.000,\nseg%d.ts\n", i)
			}
			b.WriteString("#EXT-X-ENDLIST\n")
			_, _ = w.Write([]byte(b.String()))
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		mu.Lock()
		first := name == "seg2.ts" && !tripped
		if first {
			tripped = true
		} else {
			served[name]++
		}
		mu.Unlock()
		if first {
			offline.Store(true)
			h.conn.set(false)
			go func() {
				time.Sleep(time.Second)
				offline.Store(false)
				h.conn.set(true)
			}()
			http.Error(w, "unreachable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "video/mp2t")
		_, _ = w.Write(ts)
	}))
	t.Cleanup(srv.Close)

	h.variant.URL = srv.URL + "/vod.m3u8"
	h.jobCtx.OutputDir = t.TempDir()
	h.jobCtx.Filename = "vod"

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := h.o.ExecuteTwitch(ctx, h.jobCtx, h.variant, true, nil); err != nil {
		t.Fatalf("ExecuteTwitch = %v, want the VOD finished once connectivity returned", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for i := range segments {
		if n := served[fmt.Sprintf("seg%d.ts", i)]; n != 1 {
			t.Errorf("seg%d.ts was served %d times, want once — the download must continue, not start over", i, n)
		}
	}
}
