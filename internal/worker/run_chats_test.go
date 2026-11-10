package worker

import (
	"context"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// stoppableChat is a chat downloader as the run-chat registry sees one. Stop
// records what the row's chat_status read when it was called, which is how a
// test tells a stop made before the settle from one made after it.
type stoppableChat struct {
	db    *database.Database
	jobID string
	stops atomic.Int32
	mu    sync.Mutex
	saw   []database.ChatStatus
}

func (s *stoppableChat) Stop() {
	s.stops.Add(1)
	if s.db == nil || s.jobID == "" {
		return
	}
	if row, _ := s.db.GetJob(s.jobID); row != nil {
		s.mu.Lock()
		s.saw = append(s.saw, row.ChatStatus)
		s.mu.Unlock()
	}
}

func (s *stoppableChat) firstSaw() database.ChatStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.saw) == 0 {
		return "<never stopped>"
	}
	return s.saw[0]
}

// TestAPanickedRunStopsItsChatBeforeTheSettle: after a panic nothing stopped
// the run's chat downloader — the orchestrators' chat teardown is a set of
// explicit calls, and a panic unwinds past them — so the capture went on
// polling for a failed job, and a YouTube downloader's OnStart, arriving
// after recordRunPanic's settle, wrote "downloading" back over it. The run's
// recover now stops what the run registered (runChatRegistry) before it
// records the panic: the downloader is stopped while the row still reads the
// capture in flight, the settle follows, and a "downloading" write the
// downloader makes after that is refused.
//
// Mutants: processJob's recover without the stop — the downloader is never
// stopped and the late write lands; the stop after recordRunPanic — Stop sees
// the settled value; whileRunning ignoring the stop — the late write lands.
func TestAPanickedRunStopsItsChatBeforeTheSettle(t *testing.T) {
	for _, tc := range []struct {
		name, id, platform string
	}{
		{"youtube", "yt_panics", "youtube"},
		{"twitch", "tw_901", "twitch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			if _, err := db.AddJob(&database.Job{ID: tc.id, VideoID: tc.id, URL: "u", Platform: tc.platform,
				Status: database.StatusUpcoming}); err != nil {
				t.Fatal(err)
			}
			src := &stoppableChat{db: db, jobID: tc.id}
			var run *runChats
			reached, proceed := make(chan struct{}), make(chan struct{})
			w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
				close(reached)
				<-proceed
				// The run builds its chat downloader and the capture starts.
				run = w.runChats.add(tc.id, src)
				db.UpdateJobFields(tc.id, map[string]any{"status": database.StatusDownloading, "chat_status": database.ChatPending})
				run.whileRunning(func() {
					db.UpdateJobFields(tc.id, map[string]any{"chat_status": database.ChatDownloading})
				})
				panic("boom")
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started := make(chan struct{})
			go func() {
				defer close(started)
				w.Start(ctx) // enqueues the Upcoming row and runs it
			}()
			<-reached
			done := w.queue.Done(tc.id)
			close(proceed)
			<-done
			cancel()
			<-started
			w.wg.Wait() // the run's goroutine, recover included, has returned

			if n := src.stops.Load(); n == 0 {
				t.Fatalf("the panicked run's chat downloader was never stopped")
			}
			if saw := src.firstSaw(); saw != database.ChatDownloading {
				t.Errorf("Stop found chat_status %q, want %q: the stop comes before the settle", saw, database.ChatDownloading)
			}
			row, _ := db.GetJob(tc.id)
			if row.Status != database.StatusError || row.ChatStatus != database.ChatIncomplete {
				t.Fatalf("row = %s %q, want Error %q", row.Status, row.ChatStatus, database.ChatIncomplete)
			}
			// The downloader's OnStart, arriving late.
			wrote := run.whileRunning(func() {
				db.UpdateJobFields(tc.id, map[string]any{"chat_status": database.ChatDownloading})
			})
			if row, _ := db.GetJob(tc.id); wrote || row.ChatStatus != database.ChatIncomplete {
				t.Errorf("a late downloading write landed (ran %v): chat_status = %q, want %q",
					wrote, row.ChatStatus, database.ChatIncomplete)
			}
		})
	}
}

// TestAFailedRunStopsTheChatItLeftRunning: setJobError stops the run's chat
// downloaders before its Error write and the settle, for the failing path
// that did not stop its own (R-H1: nothing may write "downloading" back onto
// the Error row). A downloader the orchestrator already stopped is stopped
// again, which every downloader's Stop allows.
//
// Mutant: setJobError without the stop — never stopped, and the late write
// lands.
func TestAFailedRunStopsTheChatItLeftRunning(t *testing.T) {
	for _, platform := range []string{"youtube", "twitch"} {
		t.Run(platform, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			job := runningJob(t, w, db, "fails")
			db.UpdateJobFields(job.ID, map[string]any{"platform": platform, "chat_status": database.ChatDownloading})
			job.Platform = platform
			src := &stoppableChat{db: db, jobID: job.ID}
			run := w.runChats.add(job.ID, src)

			w.setJobError(job, context.DeadlineExceeded)

			if src.stops.Load() == 0 {
				t.Fatal("the failed run's chat downloader was never stopped")
			}
			if saw := src.firstSaw(); saw != database.ChatDownloading {
				t.Errorf("Stop found chat_status %q, want %q: the stop comes before the settle", saw, database.ChatDownloading)
			}
			wrote := run.whileRunning(func() {
				db.UpdateJobFields(job.ID, map[string]any{"chat_status": database.ChatDownloading})
			})
			if row, _ := db.GetJob(job.ID); wrote || row.Status != database.StatusError || row.ChatStatus != database.ChatIncomplete {
				t.Errorf("row = %s %q (late write ran %v), want Error %q", row.Status, row.ChatStatus, wrote, database.ChatIncomplete)
			}
		})
	}
}

// TestRunChatRegistry pins the registry's own rules: a nil registry and a nil
// entry gate nothing; stop reaches every downloader of that job and no other
// job's; a downloader registered after the stop is stopped at once; release
// closes the old entry's gate and gives the next run a fresh one.
func TestRunChatRegistry(t *testing.T) {
	var nilReg *runChatRegistry
	if rc := nilReg.add("a", &stoppableChat{}); rc != nil {
		t.Errorf("a nil registry returned an entry")
	}
	ran := false
	if !(*runChats)(nil).whileRunning(func() { ran = true }) || !ran {
		t.Errorf("a nil entry refused a write")
	}
	nilReg.stop("a")
	nilReg.release("a")

	r := newRunChatRegistry()
	a1, a2, b := &stoppableChat{}, &stoppableChat{}, &stoppableChat{}
	runA := r.add("a", a1)
	if runA == nil || r.add("a", a2) != runA {
		t.Fatalf("two downloaders of one run did not share one entry")
	}
	runB := r.add("b", b)
	if n := r.stop("a"); n != 2 || a1.stops.Load() != 1 || a2.stops.Load() != 1 || b.stops.Load() != 0 {
		t.Errorf("stop(a) = %d, stops a1=%d a2=%d b=%d; want 2 and 1,1,0", n, a1.stops.Load(), a2.stops.Load(), b.stops.Load())
	}
	if runA.whileRunning(func() { t.Error("a write ran after the stop") }) {
		t.Errorf("whileRunning reported a write after the stop")
	}
	if !runB.whileRunning(func() {}) {
		t.Errorf("another job's stop closed this job's gate")
	}
	late := &stoppableChat{}
	r.add("a", late)
	if late.stops.Load() != 1 {
		t.Errorf("a downloader added to a stopped run was stopped %d times, want 1", late.stops.Load())
	}
	r.release("b")
	if runB.whileRunning(func() { t.Error("a write ran after the release") }) {
		t.Errorf("whileRunning reported a write after the release")
	}
	if b.stops.Load() != 0 {
		t.Errorf("release stopped a downloader; it only closes the gate")
	}
	r.release("a")
	fresh := &stoppableChat{}
	if next := r.add("a", fresh); next == runA || !next.whileRunning(func() {}) || fresh.stops.Load() != 0 {
		t.Errorf("the job's next run did not start from a fresh entry")
	}
}

// TestEveryChatDownloaderIsRegisteredWithItsRun is the source pin for the
// four places a run builds a chat downloader: each registers it
// (runChats.add), and the two YouTube ones — whose "downloading" write is the
// downloader goroutine's own OnStart, not the run's — make that write through
// the entry (whileRunning). A fifth builder that registers nothing would be a
// downloader a panic leaves running.
func TestEveryChatDownloaderIsRegisteredWithItsRun(t *testing.T) {
	for _, tc := range []struct {
		file          string
		builds        []string // the constructor calls, one each
		gatedOnStarts int
	}{
		{"orchestrator_chat.go", []string{"chat.NewChatDownloader("}, 1},
		{"stream_processor_youtube.go", []string{"chat.NewChatDownloader("}, 1},
		{"stream_processor_twitch.go", []string{"twitch.NewVodChatDownloader(", "twitch.NewChatDownloader("}, 0},
	} {
		src, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatal(err)
		}
		s := string(src)
		for _, b := range tc.builds {
			if n := strings.Count(s, b); n != 1 {
				t.Errorf("%s: %d calls of %s, want 1 — register a new one with its run and count it here", tc.file, n, b)
			}
		}
		if n := strings.Count(s, ".runChats.add("); n != len(tc.builds) {
			t.Errorf("%s: %d runChats.add calls for %d downloaders built", tc.file, n, len(tc.builds))
		}
		if n := strings.Count(s, ".whileRunning("); n != tc.gatedOnStarts {
			t.Errorf("%s: %d whileRunning calls, want %d", tc.file, n, tc.gatedOnStarts)
		}
	}
}
