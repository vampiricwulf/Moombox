package worker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/chat"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// TestInterruptionSignalRecordsZeroFormatsLive pins observe()'s exact
// classification: the broadcast-interrupted signature is StreamStatus
// "live" with zero formats — a genuinely ended stream keeps post-live
// formats, so formats present (even while nominally "live") must never
// mark the signal, and freshness must expire after
// interruptionSignalStaleAfter.
func TestInterruptionSignalRecordsZeroFormatsLive(t *testing.T) {
	t.Run("live status with zero formats marks fresh", func(t *testing.T) {
		sig := &interruptionSignal{}
		sig.observe(&youtube.VideoInfo{StreamStatus: youtube.StreamLive, Formats: nil})
		if !sig.fresh() {
			t.Error("live status + zero formats must mark the signal fresh")
		}
	})

	t.Run("formats present must not mark fresh", func(t *testing.T) {
		sig := &interruptionSignal{}
		sig.observe(&youtube.VideoInfo{StreamStatus: youtube.StreamLive, Formats: []youtube.Format{{Itag: 140}}})
		if sig.fresh() {
			t.Error("live status with formats present must NOT mark the signal fresh -- that's a healthy live response, not an interruption")
		}
	})

	t.Run("status ended must not mark fresh", func(t *testing.T) {
		sig := &interruptionSignal{}
		sig.observe(&youtube.VideoInfo{StreamStatus: youtube.StreamPostLive, Formats: nil})
		if sig.fresh() {
			t.Error("a non-live status must NOT mark the signal fresh, even with zero formats")
		}
	})

	t.Run("nil info is a safe no-op", func(t *testing.T) {
		sig := &interruptionSignal{}
		sig.observe(nil)
		if sig.fresh() {
			t.Error("observe(nil) must never mark the signal fresh")
		}
	})

	t.Run("nil receiver is a safe no-op", func(t *testing.T) {
		var sig *interruptionSignal
		sig.observe(&youtube.VideoInfo{StreamStatus: youtube.StreamLive, Formats: nil}) // must not panic
		if sig.fresh() {
			t.Error("a nil *interruptionSignal must never report fresh")
		}
	})

	// I4 fix: the auth-wall guard used to live only in
	// observeYouTubeStatusProbe (the cookieless CheckStreamStatus path).
	// Moved centrally into observe() itself so every call site -- including
	// the three AUTHENTICATED sites (orchestrator_youtube.go's
	// quality-refresh and still-live re-verification paths, and
	// strategies.go's credential-refresh re-probe) that arm on a bit-
	// identical live+zero-formats shape when cookies die mid-stream -- is
	// covered via the one central path, not by each site duplicating the
	// check.
	for _, tc := range []struct {
		name string
		err  youtube.PlayabilityError
	}{
		{"members-only", youtube.PlayabilityMembersOnly},
		{"age-restricted", youtube.PlayabilityAgeRestricted},
		{"login-required", youtube.PlayabilityLoginRequired},
	} {
		t.Run("auth-walled live+zero-formats ("+tc.name+") must not mark fresh, via the central observe() path", func(t *testing.T) {
			sig := &interruptionSignal{}
			sig.observe(&youtube.VideoInfo{StreamStatus: youtube.StreamLive, Formats: nil, PlayabilityError: tc.err})
			if sig.fresh() {
				t.Error("an auth-walled live+zero-formats observation must NOT mark the signal fresh -- this could be a healthy auth-walled stream OR cookies that died mid-stream, neither of which is resume evidence")
			}
		})
	}

	t.Run("non-auth-walled live+zero-formats still marks fresh (the guard must not swallow the real signature)", func(t *testing.T) {
		sig := &interruptionSignal{}
		sig.observe(&youtube.VideoInfo{StreamStatus: youtube.StreamLive, Formats: nil, PlayabilityError: youtube.PlayabilityOK})
		if !sig.fresh() {
			t.Error("a non-auth-walled live+zero-formats observation must still mark the signal fresh")
		}
	})

	t.Run("freshness expires after the stale window", func(t *testing.T) {
		sig := &interruptionSignal{}
		sig.observe(&youtube.VideoInfo{StreamStatus: youtube.StreamLive, Formats: nil})
		if !sig.fresh() {
			t.Fatal("expected fresh immediately after observe")
		}
		// Directly age the stored timestamp past interruptionSignalStaleAfter.
		// Same package, so poking the unexported field is "inject time", not
		// a reimplementation of fresh()'s own comparison logic -- fresh() is
		// still the thing being asserted on below.
		sig.lastSeen.Store(time.Now().Add(-interruptionSignalStaleAfter - time.Second))
		if sig.fresh() {
			t.Error("signal must go stale once interruptionSignalStaleAfter has elapsed since the last observation")
		}
	})
}

// TestMayResumeClosureTruthTable exercises the REAL buildMayResume
// (shared by ExecuteWithChat and this test — no reimplementation of its
// or/fallback logic) across the (signal fresh?, chat open?, chat nil?)
// truth table. The "chat open" cases use a real *chat.ChatDownloader
// seeded via SetLiveContinuationOpenForTesting (chat package, exported
// for exactly this cross-package need) so the closure reads the actual
// production LiveContinuationOpen() accessor, not a stand-in.
func TestMayResumeClosureTruthTable(t *testing.T) {
	freshSignal := func() *interruptionSignal {
		sig := &interruptionSignal{}
		sig.observe(&youtube.VideoInfo{StreamStatus: youtube.StreamLive, Formats: nil})
		return sig
	}
	staleSignal := &interruptionSignal{} // never observed -- never fresh

	newChat := func(open bool) *chat.ChatDownloader {
		cd := chat.NewChatDownloader(chat.ChatDownloaderOptions{VideoID: "x", OutputFile: "unused"})
		cd.SetLiveContinuationOpenForTesting(open)
		return cd
	}

	cases := []struct {
		name   string
		sig    *interruptionSignal
		chatDl *chat.ChatDownloader
		want   bool
	}{
		{"signal fresh, chat nil -- falls back to signal only, true", freshSignal(), nil, true},
		{"signal stale, chat nil -- falls back to signal only, false", staleSignal, nil, false},
		{"signal nil, chat nil -- false", nil, nil, false},
		{"signal nil, chat open -- chat wins", nil, newChat(true), true},
		{"signal fresh, chat closed -- signal wins", freshSignal(), newChat(false), true},
		{"signal stale, chat open -- chat wins", staleSignal, newChat(true), true},
		{"signal stale, chat closed -- neither, false", staleSignal, newChat(false), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildMayResume(c.sig, c.chatDl)()
			if got != c.want {
				t.Errorf("buildMayResume(...)() = %v, want %v", got, c.want)
			}
		})
	}
}

// newInterruptedTestDownloader drives a REAL engine.SegmentDownloader
// through a genuine Tier-2 interruption finalize: handleGoneError's
// budget-expired fallthrough defers on a permanently-true MayResume
// (stallForPossibleResume latches), then the InterruptionTimeout ceiling
// expires and it finalizes with finalizedDuringInterruption latched --
// same mechanism internal/engine/downloader_interruption_test.go's
// TestBackstopCeilingExpires exercises. Reproduced here with a minimal
// local fake GVS server (rather than a hand-rolled struct implementing a
// FinalizedDuringInterruption()-shaped interface) so this package's
// finalizeIncompleteTail wiring is proven against the actual production
// accessor, which can't be faked -- engine.SegmentDownloader is a
// concrete type, not an interface.
func newInterruptedTestDownloader(t *testing.T) *engine.SegmentDownloader {
	t.Helper()
	const head = 1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sqStr := r.URL.Query().Get("sq")
		w.Header().Set("X-Head-Seqnum", strconv.Itoa(head))
		if sqStr == "" {
			w.WriteHeader(http.StatusOK)
			return
		}
		sq, err := strconv.Atoi(sqStr)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if sq <= head {
			fmt.Fprintf(w, "seg-%05d;", sq)
			return
		}
		w.WriteHeader(http.StatusForbidden) // routes to handleGoneError (site 1)
	}))
	t.Cleanup(srv.Close)

	d := engine.NewSegmentDownloader(engine.DownloaderOptions{
		BaseURL:             srv.URL + "/videoplayback?itag=140",
		OutputFile:          filepath.Join(t.TempDir(), "v"),
		MaxTimeout:          200 * time.Millisecond,
		InterruptionTimeout: 200 * time.Millisecond,
		CheckStreamStatus: func(context.Context) (bool, error) {
			return false, errors.New("status check unavailable") // defers the verdict
		},
	})
	d.MayResume = func() bool { return true } // never resolves itself -- only the ceiling ends this run

	done := make(chan error, 1)
	go func() { done <- d.Start(context.Background()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start = %v, want nil (ceiling-forced finalize)", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Start did not return once the InterruptionTimeout ceiling expired -- test setup could not reproduce a Tier-2 finalize")
	}

	if !d.FinalizedDuringInterruption() {
		t.Fatal("test setup invalid: FinalizedDuringInterruption() = false, want true")
	}
	if d.FinalizedBehindHead() {
		t.Fatal("test setup invalid: downloader finalized behind head -- must isolate the interruption evidence from the behind-head evidence")
	}
	return d
}

// TestFinalizeIncompleteTailInterruption pins the third computeIncompleteTail
// input: a DownloadResult whose downloader latched a Tier-2 interruption
// finalize (FinalizedDuringInterruption()==true) must persist
// incomplete_tail=true even though NEITHER downloader finalized behind
// head. Would catch: computeIncompleteTail or finalizeIncompleteTail never
// consulting FinalizedDuringInterruption at all (the flag would stay
// false, silently discarding staging on cleanup even though the resume
// sidecar was deliberately preserved for exactly this case).
func TestFinalizeIncompleteTailInterruption(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	jobID := "yt_finalizeIncompleteTailInterruption"
	job := &database.Job{
		ID:      jobID,
		VideoID: jobID,
		URL:     "https://youtube.com/watch?v=" + jobID,
		Status:  database.StatusDownloading,
	}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}

	o := &DownloadOrchestrator{db: db, logger: &discardLogger{}}

	dl := newInterruptedTestDownloader(t)

	incomplete, _, _, _, _ := o.finalizeIncompleteTail(jobID, &DownloadResult{VideoDownloader: dl}, false)
	if !incomplete {
		t.Fatal("a downloader that latched a Tier-2 interruption finalize must flag incomplete_tail even with both behind-head false")
	}

	got, err := db.GetJob(jobID)
	if err != nil || got == nil {
		t.Fatalf("GetJob: %v", err)
	}
	if !got.IncompleteTail {
		t.Error("finalizeIncompleteTail did not persist incomplete_tail=true for an interrupted finalize")
	}
}

// TestAttachMayResumeAlwaysInstalls (I1 fix) REPLACES the pre-I1 gate test
// (formerly TestAttachMayResumeGatedByInterruptionTimeout): its old
// "InterruptionTimeout<=0 leaves MayResume nil" assertion is DELIBERATELY
// INVERTED here. attachMayResume no longer looks at InterruptionTimeout at
// all -- it installs mayResume onto any non-nil downloader unconditionally.
// The config-0 ("stall disabled") contract is now upheld at construction
// time instead, via engineInterruptionTimeout mapping the value that goes
// into engine.DownloaderOptions.InterruptionTimeout (see
// TestEngineInterruptionTimeoutMapping) -- not by withholding MayResume.
// This calls the actual function attachProgress delegates to, so it
// exercises the real wiring rather than a re-implementation of the gate.
func TestAttachMayResumeAlwaysInstalls(t *testing.T) {
	mayResume := func() bool { return true }

	t.Run("installs regardless of the downloader's own InterruptionTimeout (0)", func(t *testing.T) {
		dl := engine.NewSegmentDownloader(engine.DownloaderOptions{OutputFile: "x", InterruptionTimeout: 0})
		attachMayResume(dl, mayResume)
		if dl.MayResume == nil {
			t.Fatal("attachMayResume must install MayResume unconditionally (I1 fix) -- the config-0 gate moved to construction time (engineInterruptionTimeout), not here")
		}
		if !dl.MayResume() {
			t.Error("the installed MayResume must be the passed-in closure, not a stand-in")
		}
	})

	t.Run("installs regardless of a positive InterruptionTimeout", func(t *testing.T) {
		dl := engine.NewSegmentDownloader(engine.DownloaderOptions{OutputFile: "x", InterruptionTimeout: time.Minute})
		attachMayResume(dl, mayResume)
		if dl.MayResume == nil {
			t.Fatal("MayResume must be installed")
		}
	})

	t.Run("nil downloader is a safe no-op", func(t *testing.T) {
		attachMayResume(nil, mayResume) // must not panic
	})
}

// TestEngineInterruptionTimeoutMapping (I1 fix) pins engineInterruptionTimeout
// -- the helper every LIVE YouTube strategy site now routes
// job.Config.InterruptionTimeout through before it reaches
// engine.DownloaderOptions.InterruptionTimeout, so "config-0 job ⇒
// MayResume installed with the InterruptionNoStall sentinel" holds:
// attachMayResume (tested above) installs MayResume unconditionally, and
// THIS mapping is what keeps a config-0 downloader from colliding with the
// engine's own "0 = unbounded" meaning.
func TestEngineInterruptionTimeoutMapping(t *testing.T) {
	cases := []struct {
		name       string
		configured time.Duration
		want       time.Duration
	}{
		{"config-0 (stall disabled) maps to the sentinel", 0, engine.InterruptionNoStall},
		{"a negative config value also maps to the sentinel (defensive)", -1 * time.Second, engine.InterruptionNoStall},
		{"a positive config value passes through unchanged", time.Minute, time.Minute},
		{"the default 2h passes through unchanged", 2 * time.Hour, 2 * time.Hour},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := engineInterruptionTimeout(c.configured)
			if got != c.want {
				t.Errorf("engineInterruptionTimeout(%v) = %v, want %v", c.configured, got, c.want)
			}
		})
	}
}

// TestResumeEvidence (I1 fix) pins resumeEvidence -- the evidence half of
// the config-independent evidence/permission split. Unlike the old
// shouldWaitForResume, this never looks at interruptionTimeout at all.
func TestResumeEvidence(t *testing.T) {
	alwaysTrue := func() bool { return true }
	alwaysFalse := func() bool { return false }

	cases := []struct {
		name        string
		signalFresh bool
		mayResume   func() bool
		want        bool
	}{
		{"fresh signal alone is evidence", true, nil, true},
		{"mayResume true alone is evidence", false, alwaysTrue, true},
		{"both true is evidence", true, alwaysTrue, true},
		{"fresh signal wins even if mayResume is false", true, alwaysFalse, true},
		{"nil mayResume + stale signal is no evidence", false, nil, false},
		{"mayResume false + stale signal is no evidence", false, alwaysFalse, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resumeEvidence(c.signalFresh, c.mayResume)
			if got != c.want {
				t.Errorf("resumeEvidence(%v, ...) = %v, want %v", c.signalFresh, got, c.want)
			}
		})
	}
}

// TestSegmentProgressResetsStallCounters is the I3 fix's dedicated
// coverage for the echo-suppression decision extracted from
// runLiveStreamDownload's onSegmentProgress closure: a deferred final
// progress report from an already-cancelled downloader repeats the EXACT
// SAME cumulative Bytes as its last live report, and that echo must NOT be
// treated as fresh progress (it must not reset consecutiveLiveChecks /
// lastSegTime), or maxConsecutiveLiveChecks is defeated and a stuck-live
// abandoned broadcast livelocks the job in Downloading forever.
func TestSegmentProgressResetsStallCounters(t *testing.T) {
	t.Run("Bytes=0 is never progress (catch-up bookend / failed-download echo)", func(t *testing.T) {
		var lastBytes atomic.Int64
		if segmentProgressResetsStallCounters(engine.DownloadProgress{Bytes: 0}, &lastBytes) {
			t.Error("Bytes=0 must never reset the stall counters")
		}
	})

	t.Run("first real progress report is a delta from zero and resets", func(t *testing.T) {
		var lastBytes atomic.Int64
		if !segmentProgressResetsStallCounters(engine.DownloadProgress{Bytes: 1024}, &lastBytes) {
			t.Error("a fresh stream's first Bytes>0 report must reset the stall counters")
		}
		if lastBytes.Load() != 1024 {
			t.Errorf("lastBytes = %d, want 1024 (stored after a genuine delta)", lastBytes.Load())
		}
	})

	t.Run("a strictly increasing follow-up report is progress and resets", func(t *testing.T) {
		var lastBytes atomic.Int64
		lastBytes.Store(1024)
		if !segmentProgressResetsStallCounters(engine.DownloadProgress{Bytes: 2048}, &lastBytes) {
			t.Error("Bytes increasing from 1024 to 2048 must reset the stall counters")
		}
		if lastBytes.Load() != 2048 {
			t.Errorf("lastBytes = %d, want 2048", lastBytes.Load())
		}
	})

	// This is the exact regression: re-running an already-cancelled
	// downloader hits its own deferred final OnProgress again, reporting
	// the SAME cumulative Bytes as its last live report. That echo must be
	// silently ignored -- no reset, no lastBytes mutation.
	t.Run("a same-cumulative-Bytes echo (deferred final report from a re-run cancelled downloader) does NOT reset", func(t *testing.T) {
		var lastBytes atomic.Int64
		lastBytes.Store(5_000_000)
		if segmentProgressResetsStallCounters(engine.DownloadProgress{Bytes: 5_000_000}, &lastBytes) {
			t.Error("a byte-identical echo of the last observed cumulative Bytes must NOT reset the stall counters -- this is the I3 livelock bug")
		}
		if lastBytes.Load() != 5_000_000 {
			t.Errorf("lastBytes = %d, want unchanged 5000000", lastBytes.Load())
		}
	})

	t.Run("a decreasing Bytes report (should never happen, but defensively) does NOT reset", func(t *testing.T) {
		var lastBytes atomic.Int64
		lastBytes.Store(5_000_000)
		if segmentProgressResetsStallCounters(engine.DownloadProgress{Bytes: 1_000_000}, &lastBytes) {
			t.Error("a Bytes value lower than the last observed total must NOT reset the stall counters")
		}
	})

	t.Run("per-stream independence: a fresh downloader's tracker starts at 0 regardless of another stream's peak", func(t *testing.T) {
		// Simulates attachProgress resetting lastVideoBytes to 0 for a
		// brand-new video downloader instance while an unrelated audio
		// stream's tracker sits at a much higher cumulative total -- the
		// two must never be conflated.
		var lastVideoBytes, lastAudioBytes atomic.Int64
		lastAudioBytes.Store(9_000_000)
		if !segmentProgressResetsStallCounters(engine.DownloadProgress{Bytes: 100}, &lastVideoBytes) {
			t.Error("a fresh video downloader's first report (100 bytes) must count as progress even though the UNRELATED audio tracker sits far higher")
		}
	})
}

// TestShouldWaitForResume is the table-driven test for the C2 fix's
// decision function: a failed live-refresh should wait-and-retry instead
// of ending the recording immediately, but only when the stall is enabled
// AND there's resume evidence. Signature updated for I1: takes the
// already-computed evidence bool (see resumeEvidence) rather than
// signalFresh+mayResume directly -- shouldWaitForResume is now PERMISSION
// only. Further updated for I3: also takes a *waitDeadline + now, since
// permission is now ALSO bounded by the episode's own hard deadline (see
// TestShouldWaitForResume_Deadline below for that dimension specifically).
func TestShouldWaitForResume(t *testing.T) {
	errRefresh := errors.New("refresh failed")
	now := time.Unix(1_700_000_000, 0)

	cases := []struct {
		name                string
		refreshErr          error
		evidence            bool
		interruptionTimeout time.Duration
		want                bool
	}{
		{"nil refreshErr never waits, even with evidence", nil, true, time.Minute, false},
		{"disabled (0) never waits despite evidence", errRefresh, true, 0, false},
		{"disabled (negative) never waits despite evidence", errRefresh, true, -1 * time.Second, false},
		{"enabled + evidence waits", errRefresh, true, time.Minute, true},
		{"enabled + no evidence does not wait", errRefresh, false, time.Minute, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var d waitDeadline
			got := shouldWaitForResume(c.refreshErr, c.evidence, c.interruptionTimeout, &d, now)
			if got != c.want {
				t.Errorf("shouldWaitForResume(...) = %v, want %v", got, c.want)
			}
		})
	}
}

// TestShouldWaitForResume_Deadline is the I3 fix's dedicated coverage for
// the deadline-refusal dimension: shouldWaitForResume/waitDeadline compose
// to bound the wait branch's total retries to interruptionTimeout, closing
// the gap where maxConsecutiveLiveChecks (a DIFFERENT branch's bound) never
// applied here and evidence holding indefinitely (a stuck-live abandoned
// broadcast) could retry this branch forever.
func TestShouldWaitForResume_Deadline(t *testing.T) {
	errRefresh := errors.New("refresh failed")
	const timeout = 10 * time.Minute
	episodeStart := time.Unix(1_700_000_000, 0)

	t.Run("first call within budget waits and latches the episode start", func(t *testing.T) {
		var d waitDeadline
		if !shouldWaitForResume(errRefresh, true, timeout, &d, episodeStart) {
			t.Fatal("first call within budget must wait")
		}
		if d.start != episodeStart {
			t.Errorf("waitDeadline.start = %v, want latched to the first call's now (%v)", d.start, episodeStart)
		}
	})

	t.Run("repeated calls before the deadline keep waiting without moving the latched start", func(t *testing.T) {
		var d waitDeadline
		if !shouldWaitForResume(errRefresh, true, timeout, &d, episodeStart) {
			t.Fatal("first call must wait")
		}
		later := episodeStart.Add(timeout / 2)
		if !shouldWaitForResume(errRefresh, true, timeout, &d, later) {
			t.Error("a retry still within the budget must still wait")
		}
		if d.start != episodeStart {
			t.Errorf("waitDeadline.start moved to %v, want it to stay latched at the FIRST call's now (%v)", d.start, episodeStart)
		}
	})

	t.Run("a call at or past the deadline refuses to wait", func(t *testing.T) {
		var d waitDeadline
		if !shouldWaitForResume(errRefresh, true, timeout, &d, episodeStart) {
			t.Fatal("first call must wait")
		}
		atDeadline := episodeStart.Add(timeout)
		if shouldWaitForResume(errRefresh, true, timeout, &d, atDeadline) {
			t.Error("a call at the deadline must refuse to wait -- the episode's budget is exhausted")
		}
		pastDeadline := episodeStart.Add(timeout + time.Minute)
		if shouldWaitForResume(errRefresh, true, timeout, &d, pastDeadline) {
			t.Error("a call past the deadline must refuse to wait")
		}
	})

	t.Run("reset gives a later episode a fresh budget", func(t *testing.T) {
		var d waitDeadline
		shouldWaitForResume(errRefresh, true, timeout, &d, episodeStart)
		pastDeadline := episodeStart.Add(timeout + time.Minute)
		if shouldWaitForResume(errRefresh, true, timeout, &d, pastDeadline) {
			t.Fatal("sanity: the first episode's budget must already be exhausted before reset")
		}
		d.reset()
		if !shouldWaitForResume(errRefresh, true, timeout, &d, pastDeadline) {
			t.Error("after reset, a call must wait again -- a later episode gets its own fresh budget, not the expired clock of a resolved earlier one")
		}
	})
}

// TestNoteRefreshFailure (I1 fix) is the exact granularity the coordinator's
// test plan asked for: config-0 + evidence must still LATCH (Tier-2
// preservation) even though it never WAITS; config-0 + no evidence does
// neither; config>0 behaves exactly like the pre-I1 shouldWaitForResume
// (latch and wait together). This is the composition
// runLiveStreamDownload's refresh-failure branch actually calls, not a
// hand-assembly of resumeEvidence+shouldWaitForResume+latch.wait() by the
// test.
func TestNoteRefreshFailure(t *testing.T) {
	errRefresh := errors.New("refresh failed")
	alwaysTrue := func() bool { return true }
	alwaysFalse := func() bool { return false }
	now := time.Unix(1_700_000_000, 0)

	t.Run("nil refreshErr: no latch, no wait", func(t *testing.T) {
		var l resumeWaitLatch
		var d waitDeadline
		wait := noteRefreshFailure(&l, &d, nil, true, alwaysTrue, time.Minute, now)
		if wait {
			t.Error("wait = true for a nil refreshErr, want false")
		}
		if l.value() {
			t.Error("latch must not fire for a nil refreshErr")
		}
	})

	t.Run("config-0 + evidence: latches WITHOUT waiting", func(t *testing.T) {
		var l resumeWaitLatch
		var d waitDeadline
		wait := noteRefreshFailure(&l, &d, errRefresh, true, alwaysFalse, 0, now)
		if wait {
			t.Error("wait = true with the stall disabled (config 0), want false -- interruption_timeout=0 must never actually stall")
		}
		if !l.value() {
			t.Error("latch must still fire when evidence holds, even with the stall disabled -- interruption_timeout=0 disables the WAIT, not Tier-2 preservation")
		}
	})

	t.Run("config-0 + no evidence: neither latches nor waits", func(t *testing.T) {
		var l resumeWaitLatch
		var d waitDeadline
		wait := noteRefreshFailure(&l, &d, errRefresh, false, alwaysFalse, 0, now)
		if wait {
			t.Error("wait = true, want false")
		}
		if l.value() {
			t.Error("latch must not fire when there is no resume evidence at all")
		}
	})

	t.Run("config>0 + evidence: latches AND waits (unchanged pre-I1 behavior)", func(t *testing.T) {
		var l resumeWaitLatch
		var d waitDeadline
		wait := noteRefreshFailure(&l, &d, errRefresh, true, alwaysFalse, time.Minute, now)
		if !wait {
			t.Error("wait = false, want true -- an enabled stall with evidence must still wait")
		}
		if !l.value() {
			t.Error("latch must fire")
		}
	})

	t.Run("config>0 + no evidence: neither latches nor waits", func(t *testing.T) {
		var l resumeWaitLatch
		var d waitDeadline
		wait := noteRefreshFailure(&l, &d, errRefresh, false, alwaysFalse, time.Minute, now)
		if wait {
			t.Error("wait = true, want false")
		}
		if l.value() {
			t.Error("latch must not fire")
		}
	})

	// I3: evidence holding forever (e.g. a stuck-live abandoned broadcast
	// whose chat continuation never closes) must still stop WAITING once
	// the episode's deadline passes, even though it keeps LATCHING —
	// mirroring the config-0 split (wait disabled, latch not) but driven by
	// elapsed time instead of config.
	t.Run("config>0 + evidence + deadline exceeded: latches WITHOUT waiting", func(t *testing.T) {
		var l resumeWaitLatch
		var d waitDeadline
		const timeout = 10 * time.Minute
		if wait := noteRefreshFailure(&l, &d, errRefresh, true, alwaysFalse, timeout, now); !wait {
			t.Fatal("first call within budget must wait")
		}
		later := now.Add(timeout + time.Minute)
		wait := noteRefreshFailure(&l, &d, errRefresh, true, alwaysFalse, timeout, later)
		if wait {
			t.Error("wait = true after the episode's deadline passed, want false")
		}
		if !l.value() {
			t.Error("latch must still fire past the deadline -- Tier-2 evidence preservation is independent of the wait-permission deadline")
		}
	})
}

// TestObserveYouTubeStatusProbe (I1 fix) pins observeYouTubeStatusProbe --
// the exact function all five CheckStreamStatus closures (HLS, DASH x2,
// manifestless-DASH x2) delegate to (`return observeYouTubeStatusProbe(job,
// info, err)`), not a reimplementation. This is the only observation site
// that fires WHILE the engine is still internally retrying/stalling (the
// engine's own ~30s streamStatusCheckInterval throttle), so a regression
// here would make the interruption signal near-inert for exactly the
// scenario it exists to cover.
func TestObserveYouTubeStatusProbe(t *testing.T) {
	t.Run("probe error is passed through and does not observe", func(t *testing.T) {
		job := &JobContext{Job: &database.Job{ID: "j1"}, Interruption: &interruptionSignal{}}
		probeErr := errors.New("probe failed")

		ended, err := observeYouTubeStatusProbe(job, nil, probeErr)

		if !errors.Is(err, probeErr) {
			t.Errorf("err = %v, want %v", err, probeErr)
		}
		if ended {
			t.Error("ended = true on a probe error, want false")
		}
		if job.Interruption.fresh() {
			t.Error("a probe error must not mark the interruption signal fresh")
		}
	})

	t.Run("live status with zero formats observes and reports not-ended", func(t *testing.T) {
		job := &JobContext{Job: &database.Job{ID: "j1"}, Interruption: &interruptionSignal{}}
		info := &youtube.VideoInfo{StreamStatus: youtube.StreamLive, Formats: nil}

		ended, err := observeYouTubeStatusProbe(job, info, nil)

		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if ended {
			t.Error("ended = true for StreamLive, want false")
		}
		if !job.Interruption.fresh() {
			t.Error("a live + zero-formats probe must mark the interruption signal fresh -- this is THE fix: this site must observe during the engine's own stall, not just after the downloader exits")
		}
	})

	t.Run("live status with formats present reports not-ended but does not mark fresh", func(t *testing.T) {
		job := &JobContext{Job: &database.Job{ID: "j1"}, Interruption: &interruptionSignal{}}
		info := &youtube.VideoInfo{StreamStatus: youtube.StreamLive, Formats: []youtube.Format{{Itag: 140}}}

		ended, err := observeYouTubeStatusProbe(job, info, nil)

		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if ended {
			t.Error("ended = true for StreamLive, want false")
		}
		if job.Interruption.fresh() {
			t.Error("a healthy live probe (formats present) must NOT mark the interruption signal fresh")
		}
	})

	t.Run("post-live status reports ended and does not mark fresh", func(t *testing.T) {
		job := &JobContext{Job: &database.Job{ID: "j1"}, Interruption: &interruptionSignal{}}
		info := &youtube.VideoInfo{StreamStatus: youtube.StreamPostLive, Formats: nil}

		ended, err := observeYouTubeStatusProbe(job, info, nil)

		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if !ended {
			t.Error("ended = false for StreamPostLive, want true")
		}
		if job.Interruption.fresh() {
			t.Error("a non-live status must never mark the interruption signal fresh")
		}
	})

	t.Run("nil Interruption is a safe no-op", func(t *testing.T) {
		job := &JobContext{Job: &database.Job{ID: "j1"}} // Interruption left nil, as many real JobContext literals do
		info := &youtube.VideoInfo{StreamStatus: youtube.StreamLive, Formats: nil}

		ended, err := observeYouTubeStatusProbe(job, info, nil) // must not panic

		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if ended {
			t.Error("ended = true for StreamLive, want false")
		}
	})

	// Auth-walled mis-arm: CheckStreamStatus always probes via the
	// cookieless ANDROID_VR client, so a HEALTHY members-only/age-restricted/
	// login-required live stream reliably classifies as live+zero-formats
	// too (classifyStream derives StreamStatus from videoDetails.isLive
	// alone, before any playability/formats check) -- the SAME shape as a
	// genuine interruption. Without the PlayabilityError guard this would
	// arm the signal every ~30s for the entire run of any such stream.
	for _, tc := range []struct {
		name string
		err  youtube.PlayabilityError
	}{
		{"members-only", youtube.PlayabilityMembersOnly},
		{"age-restricted", youtube.PlayabilityAgeRestricted},
		{"login-required", youtube.PlayabilityLoginRequired},
	} {
		t.Run("auth-walled live+zero-formats ("+tc.name+") does not arm the signal", func(t *testing.T) {
			job := &JobContext{Job: &database.Job{ID: "j1"}, Interruption: &interruptionSignal{}}
			info := &youtube.VideoInfo{StreamStatus: youtube.StreamLive, Formats: nil, PlayabilityError: tc.err}

			ended, err := observeYouTubeStatusProbe(job, info, nil)

			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if ended {
				t.Error("ended = true for StreamLive, want false")
			}
			if job.Interruption.fresh() {
				t.Error("an auth-walled live+zero-formats probe must NOT arm the interruption signal -- this is a healthy stream this unauthenticated probe simply can't see formats for, not an interruption")
			}
		})
	}

	// PlayabilityOK (or the zero value) with zero formats must still arm --
	// the guard above must not accidentally swallow the real signature.
	t.Run("non-auth-walled live+zero-formats still arms the signal", func(t *testing.T) {
		job := &JobContext{Job: &database.Job{ID: "j1"}, Interruption: &interruptionSignal{}}
		info := &youtube.VideoInfo{StreamStatus: youtube.StreamLive, Formats: nil, PlayabilityError: youtube.PlayabilityOK}

		if _, err := observeYouTubeStatusProbe(job, info, nil); err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if !job.Interruption.fresh() {
			t.Error("a non-auth-walled live+zero-formats probe must still arm the signal -- the auth guard must not swallow the real interruption signature")
		}
	})

	t.Run("nil info without error is a safe no-op, not a panic", func(t *testing.T) {
		job := &JobContext{Job: &database.Job{ID: "j1"}, Interruption: &interruptionSignal{}}

		ended, err := observeYouTubeStatusProbe(job, nil, nil) // must not panic on info.StreamStatus

		if err == nil {
			t.Error("err = nil, want a non-nil error -- a nil info without an error must be treated as inconclusive, not as a confirmed verdict")
		}
		if ended {
			t.Error("ended = true for nil info, want false -- must not read as a confirmed 'still live' verdict (that would trigger an unwarranted ErrQualityLost upstream)")
		}
		if job.Interruption.fresh() {
			t.Error("nil info must not arm the interruption signal")
		}
	})
}

// TestFinalizeIncompleteTailWorkerWaitedForResume (residual Tier-2 fix)
// pins that workerWaitedForResume -- true when the live loop's
// shouldWaitForResume branch fired at least once, even if it eventually
// exhausted maxConsecutiveLiveChecks and gave up -- flags incomplete_tail
// even when NEITHER downloader ever latched FinalizedDuringInterruption
// itself (the whole point: that engine-side latch is never reached when
// the loop dies from repeated refresh failures rather than an engine-side
// budget expiry).
func TestFinalizeIncompleteTailWorkerWaitedForResume(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	jobID := "yt_finalizeIncompleteTailWorkerWaited"
	job := &database.Job{
		ID:      jobID,
		VideoID: jobID,
		URL:     "https://youtube.com/watch?v=" + jobID,
		Status:  database.StatusDownloading,
	}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}

	o := &DownloadOrchestrator{db: db, logger: &discardLogger{}}

	// Both downloaders nil (no FinalizedDuringInterruption evidence at all)
	// -- only workerWaitedForResume=true should flag this.
	incomplete, _, _, _, _ := o.finalizeIncompleteTail(jobID, &DownloadResult{}, true)
	if !incomplete {
		t.Fatal("workerWaitedForResume=true must flag incomplete_tail even with no downloader evidence at all")
	}

	got, err := db.GetJob(jobID)
	if err != nil || got == nil {
		t.Fatalf("GetJob: %v", err)
	}
	if !got.IncompleteTail {
		t.Error("finalizeIncompleteTail did not persist incomplete_tail=true for workerWaitedForResume=true")
	}
}

// TestResumeWaitLatch exercises resumeWaitLatch -- the ACTUAL type
// runLiveStreamDownload uses (wait()/resolved()/value(), not a
// reimplementation of its transitions) -- through the two exact sequences
// the reviewer described:
//  1. wait fired, refresh succeeded, then finalize: a resumed broadcast
//     must not taint a clean finish. value() must be false.
//  2. wait fired, refresh succeeded (clearing it), wait fired again,
//     never resolved (loop exhausts): value() must be true -- the SECOND,
//     unresolved wait is what finalizeIncompleteTail must see.
//
// This is finalize-scoped, not history-scoped: a latch that never clears
// on success would report waited=true in scenario 1 too (a regression the
// mutation check below confirms this test catches).
func TestResumeWaitLatch(t *testing.T) {
	t.Run("zero value starts not-waiting", func(t *testing.T) {
		var l resumeWaitLatch
		if l.value() {
			t.Error("zero-value latch must start false")
		}
	})

	t.Run("wait fired, refresh succeeded, finalize -- not interrupted", func(t *testing.T) {
		var l resumeWaitLatch
		l.wait()       // shouldWaitForResume's branch fires
		l.resolved()   // a later refreshDownload succeeds -- broadcast resumed
		if l.value() { // finalize
			t.Error("value() = true after wait()+resolved(), want false -- a resumed broadcast must not taint a clean finish")
		}
	})

	t.Run("wait, resolved, wait again, exhausted -- interrupted", func(t *testing.T) {
		var l resumeWaitLatch
		l.wait()     // first wait
		l.resolved() // resolved by a successful refresh
		l.wait()     // a LATER refresh fails again and we wait a second time
		// loop exhausts (maxConsecutiveLiveChecks) with no further resolved()
		if !l.value() {
			t.Error("value() = false after an unresolved second wait, want true -- the loop genuinely gave up mid-wait")
		}
	})
}

// TestFinalizeIncompleteTailResumeWaitLatchSequence drives the same two
// sequences through the real resumeWaitLatch AND finalizeIncompleteTail
// together (the actual DB write path), so the finalize-scoped fix is
// proven end to end, not just at the latch in isolation.
func TestFinalizeIncompleteTailResumeWaitLatchSequence(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	newJob := func(t *testing.T, id string) {
		t.Helper()
		if _, err := db.AddJob(&database.Job{
			ID: id, VideoID: id, URL: "https://youtube.com/watch?v=" + id,
			Status: database.StatusDownloading,
		}); err != nil {
			t.Fatal(err)
		}
	}
	o := &DownloadOrchestrator{db: db, logger: &discardLogger{}}

	t.Run("resumed broadcast: not flagged incomplete", func(t *testing.T) {
		jobID := "yt_resumeLatchSeq_resumed"
		newJob(t, jobID)

		var l resumeWaitLatch
		l.wait()
		l.resolved() // the broadcast resumed; the recording then finishes cleanly

		incomplete, _, _, _, _ := o.finalizeIncompleteTail(jobID, &DownloadResult{}, l.value())
		if incomplete {
			t.Error("a wait that was later resolved by a successful refresh must not flag incomplete_tail")
		}
		got, err := db.GetJob(jobID)
		if err != nil || got == nil {
			t.Fatalf("GetJob: %v", err)
		}
		if got.IncompleteTail {
			t.Error("incomplete_tail persisted true for a resumed-then-clean finish")
		}
	})

	t.Run("second wait never resolved: flagged incomplete", func(t *testing.T) {
		jobID := "yt_resumeLatchSeq_exhausted"
		newJob(t, jobID)

		var l resumeWaitLatch
		l.wait()
		l.resolved()
		l.wait() // a second, later wait that the loop never resolves

		incomplete, _, _, _, _ := o.finalizeIncompleteTail(jobID, &DownloadResult{}, l.value())
		if !incomplete {
			t.Error("an unresolved second wait must flag incomplete_tail even though an earlier wait was resolved")
		}
		got, err := db.GetJob(jobID)
		if err != nil || got == nil {
			t.Fatalf("GetJob: %v", err)
		}
		if !got.IncompleteTail {
			t.Error("incomplete_tail did not persist true for an unresolved second wait")
		}
	})
}
