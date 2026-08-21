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
