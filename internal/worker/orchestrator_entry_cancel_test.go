package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
	"github.com/vampiricwulf/Moombox/internal/twitch"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// Each orchestrator's first status write — Downloading — was decided on
// processJob's read of the row. An operator's Cancel writes the row before it
// cancels the run, so a Cancel that landed after the stream processor's
// go-live write, or after processJob's last ctx check, was in the row and not
// yet in ctx. ExecuteTwitch wrote Downloading over it, announced "Twitch
// Download Starting" and started the capture; ExecuteWithChat's pre-check
// read the row first, and a Cancel landing between that read and its write
// was still written over. Both writes now apply only to a row with no
// outcome, and a refusal for one returns ErrCancelled before anything is
// announced or staged.

// stopRecordingChat is a ChatSource that records what was asked of it.
type stopRecordingChat struct{ started, stopped atomic.Bool }

func (c *stopRecordingChat) Start(context.Context) error { c.started.Store(true); return nil }
func (c *stopRecordingChat) Stop()                       { c.stopped.Store(true) }
func (c *stopRecordingChat) MarkStreamEnded()            {}
func (c *stopRecordingChat) MessageCount() int           { return 0 }
func (c *stopRecordingChat) IsRunning() bool             { return false }

// statusLog records each status the row is written with, collapsing repeats.
type statusLog struct {
	mu   sync.Mutex
	seen []database.JobStatus
}

func watchStatuses(t *testing.T, db *database.Database, id string) *statusLog {
	t.Helper()
	l := &statusLog{}
	unsub := db.OnJobUpdate(func(j *database.Job) {
		if j.ID != id {
			return
		}
		l.mu.Lock()
		if n := len(l.seen); n == 0 || l.seen[n-1] != j.Status {
			l.seen = append(l.seen, j.Status)
		}
		l.mu.Unlock()
	})
	t.Cleanup(unsub)
	return l
}

func (l *statusLog) statuses() []database.JobStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]database.JobStatus(nil), l.seen...)
}

// An orchestrator handed a row that holds an outcome writes nothing over it,
// announces nothing, creates no staging and starts no chat: the row Cancelled
// with the run's ctx still live (the Cancel's ctx cancel still to come), the
// row Cancelled with ctx cancelled (the whole Cancel landed after processJob's
// last check), and the row gone.
//
// Mutants: either entry write unconditional again (UpdateJobFields — that
// platform's Cancelled rows read Downloading, and its start is announced);
// either refusal ignored (the same); ExecuteTwitch's chat Stop dropped (the
// twitch rows' chat is not stopped).
func TestOrchestratorEntryStopsOnARowWithAnOutcome(t *testing.T) {
	for _, platform := range []string{"youtube", "twitch"} {
		for _, shape := range []string{"cancelled, ctx live", "cancelled, ctx cancelled", "row gone"} {
			t.Run(platform+"/"+shape, func(t *testing.T) {
				w, db := testWorkerSetup(t)
				rec := notificationtest.New()
				w.orchestrator.notifier = rec
				job := &database.Job{ID: "entry_" + platform, VideoID: "entry_" + platform, Platform: platform,
					URL: "https://twitch.tv/streamer", Title: "T", ChannelName: "C", Status: database.StatusLive}
				if shape != "row gone" {
					if _, err := db.AddJob(job); err != nil {
						t.Fatal(err)
					}
					// The operator's Cancel lands after processJob read the row.
					if !db.UpdateJobFieldsUnlessTerminal(job.ID, map[string]any{"status": database.StatusCancelled}) {
						t.Fatal("could not cancel the row")
					}
				}
				log := watchStatuses(t, db, job.ID)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if shape == "cancelled, ctx cancelled" {
					cancel()
				}
				staging := filepath.Join(t.TempDir(), "staging")
				jobCtx := &JobContext{Job: job, DB: db, Config: &JobConfig{}, StagingDir: staging, Logger: discardLogger{}}

				var err error
				chat := &stopRecordingChat{}
				if platform == "twitch" {
					err = w.orchestrator.ExecuteTwitch(ctx, jobCtx,
						&TwitchVariantInfo{URL: "http://127.0.0.1:1/x.m3u8", Name: "720p"}, false, chat)
				} else {
					err = w.orchestrator.ExecuteWithChat(ctx, jobCtx,
						&youtube.VideoInfo{StreamStatus: youtube.StreamLive, IsLive: true}, false, nil)
				}

				if !errors.Is(err, ErrCancelled) {
					t.Errorf("returned %v, want ErrCancelled", err)
				}
				if got := log.statuses(); len(got) != 0 {
					t.Errorf("statuses written %v, want none", got)
				}
				if calls := rec.Calls(); len(calls) != 0 {
					t.Errorf("sent %q, want nothing", calls[0].Title)
				}
				if _, statErr := os.Stat(staging); !errors.Is(statErr, os.ErrNotExist) {
					t.Errorf("staging was created (stat: %v)", statErr)
				}
				if platform == "twitch" && (chat.started.Load() || !chat.stopped.Load()) {
					t.Errorf("chat started %v, stopped %v; want it stopped and never started",
						chat.started.Load(), chat.stopped.Load())
				}
			})
		}
	}
}

// A Cancel whose row write lands after the stream processor's go-live write,
// with its ctx cancel held back — CancelJob runs the write's subscribers
// before queue.Cancel, and a slow one holds the window open — leaves the row
// Cancelled through the rest of processJob, and no download start is
// announced. The Twitch run wrote Downloading, then Muxing, over it and sent
// "Twitch Download Starting" before handleCancellation wrote Cancelled again.
//
// Mutant: either orchestrator's entry write unconditional again (that
// platform's row reads Downloading after the Cancel, and its start is sent).
func TestACancelBeforeTheOrchestratorWriteStands(t *testing.T) {
	for _, platform := range []string{"youtube", "twitch"} {
		t.Run(platform, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			rec := notificationtest.New()
			w.orchestrator.notifier = rec
			w.notifier = rec
			id := "pj_" + platform
			if _, err := db.AddJob(&database.Job{ID: id, VideoID: id, Platform: platform, URL: "https://twitch.tv/streamer",
				Status: database.StatusUpcoming, Title: "T", ChannelName: "C"}); err != nil {
				t.Fatal(err)
			}
			w.queue.Enqueue(id, database.StatusUpcoming)
			got, ctx, ok := w.queue.Dequeue(context.Background())
			if !ok || got != id {
				t.Fatalf("Dequeue = %q %v", got, ok)
			}

			log := watchStatuses(t, db, id)
			cancelWritten := make(chan struct{})
			released := make(chan struct{})
			var once sync.Once
			// Registered before processJob runs, so ahead of the orchestrator's
			// own listener: while it holds the Cancel's notification chain, the
			// orchestrator has not heard the Cancel and its ctx is live.
			unsub := db.OnJobUpdate(func(j *database.Job) {
				if j.ID != id || j.Status != database.StatusCancelled {
					return
				}
				first := false
				once.Do(func() { first = true; close(cancelWritten) })
				if first {
					select {
					case <-released:
					case <-time.After(5 * time.Second):
					}
				}
			})
			defer unsub()

			cancelDone := make(chan struct{})
			w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
				// The go-live write applied, then the Cancel's row write lands.
				db.UpdateJobFieldsUnlessTerminal(id, map[string]any{"status": database.StatusLive})
				go func() {
					defer close(cancelDone)
					w.CancelJob(id)
				}()
				<-cancelWritten
				if platform == "twitch" {
					return &StreamProcessResult{ShouldDownload: true,
						TwitchVariant: &twitch.TwitchHLSVariant{URL: "http://127.0.0.1:1/x.m3u8", Name: "720p"}}, nil
				}
				return &StreamProcessResult{ShouldDownload: true,
					VideoInfo: &youtube.VideoInfo{StreamStatus: youtube.StreamLive, IsLive: true}}, nil
			}
			w.processJob(ctx, id)
			close(released)
			<-cancelDone

			seq := log.statuses()
			for i, s := range seq {
				if s == database.StatusCancelled {
					if rest := seq[i+1:]; len(rest) != 0 {
						t.Errorf("after the Cancel the row was written %v (statuses %v)", rest, seq)
					}
					break
				}
			}
			if got := rec.ByEvent("downloading"); len(got) != 0 {
				t.Errorf("%q was sent for a job the operator had cancelled", got[0].Title)
			}
			if row, _ := db.GetJob(id); row == nil || row.Status != database.StatusCancelled {
				t.Errorf("row = %+v, want Cancelled", row)
			}
		})
	}
}
