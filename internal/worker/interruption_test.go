package worker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
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

	incomplete, _, _, _, _ := o.finalizeIncompleteTail(jobID, &DownloadResult{VideoDownloader: dl})
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

// TestAttachMayResumeGatedByInterruptionTimeout pins the config contract
// (C1 fix): InterruptionTimeout <= 0 means "finalize must never wait" --
// attachMayResume must leave dl.MayResume nil in that case, NOT install a
// non-nil callback with a zero ceiling (which the engine treats as "no
// ceiling", i.e. an UNBOUNDED stall for as long as MayResume keeps
// returning true -- the exact inversion this test guards against). This
// calls the actual function attachProgress delegates to, so it exercises
// the real wiring rather than a re-implementation of the gate.
func TestAttachMayResumeGatedByInterruptionTimeout(t *testing.T) {
	mayResume := func() bool { return true }

	t.Run("disabled (0) leaves MayResume nil", func(t *testing.T) {
		dl := engine.NewSegmentDownloader(engine.DownloaderOptions{OutputFile: "x"})
		attachMayResume(dl, mayResume, 0)
		if dl.MayResume != nil {
			t.Error("InterruptionTimeout=0 must leave MayResume nil -- installing it would let the engine stall unbounded (a zero ceiling means no ceiling, not already-expired) even though the config contract says 0 disables the stall entirely")
		}
	})

	t.Run("disabled (negative) leaves MayResume nil", func(t *testing.T) {
		dl := engine.NewSegmentDownloader(engine.DownloaderOptions{OutputFile: "x"})
		attachMayResume(dl, mayResume, -1*time.Second)
		if dl.MayResume != nil {
			t.Error("a negative InterruptionTimeout must also leave MayResume nil")
		}
	})

	t.Run("enabled installs the real closure", func(t *testing.T) {
		dl := engine.NewSegmentDownloader(engine.DownloaderOptions{OutputFile: "x"})
		attachMayResume(dl, mayResume, time.Minute)
		if dl.MayResume == nil {
			t.Fatal("InterruptionTimeout > 0 must install MayResume")
		}
		if !dl.MayResume() {
			t.Error("the installed MayResume must be the passed-in closure, not a stand-in")
		}
	})

	t.Run("nil downloader is a safe no-op", func(t *testing.T) {
		attachMayResume(nil, mayResume, time.Minute) // must not panic
	})
}

// TestShouldWaitForResume is the table-driven test for the C2 fix's
// decision function: a failed live-refresh should wait-and-retry instead
// of ending the recording immediately, but only when the stall is enabled
// AND there's resume evidence.
func TestShouldWaitForResume(t *testing.T) {
	errRefresh := errors.New("refresh failed")
	alwaysTrue := func() bool { return true }
	alwaysFalse := func() bool { return false }

	cases := []struct {
		name                string
		refreshErr          error
		signalFresh         bool
		mayResume           func() bool
		interruptionTimeout time.Duration
		want                bool
	}{
		{"nil refreshErr never waits, even with every other signal true", nil, true, alwaysTrue, time.Minute, false},
		{"disabled (0) falls back to old behavior despite fresh signal", errRefresh, true, alwaysTrue, 0, false},
		{"disabled (negative) falls back to old behavior despite mayResume", errRefresh, false, alwaysTrue, -1 * time.Second, false},
		{"enabled + fresh signal waits", errRefresh, true, alwaysFalse, time.Minute, true},
		{"enabled + mayResume true waits (signal not fresh)", errRefresh, false, alwaysTrue, time.Minute, true},
		{"enabled + nil mayResume + stale signal does not wait", errRefresh, false, nil, time.Minute, false},
		{"enabled + neither signal nor mayResume does not wait", errRefresh, false, alwaysFalse, time.Minute, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := shouldWaitForResume(c.refreshErr, c.signalFresh, c.mayResume, c.interruptionTimeout)
			if got != c.want {
				t.Errorf("shouldWaitForResume(...) = %v, want %v", got, c.want)
			}
		})
	}
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
}
