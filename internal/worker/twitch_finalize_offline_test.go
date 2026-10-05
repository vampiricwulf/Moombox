package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// flipOnEndChat is a ChatSource whose MarkStreamEnded runs a hook — the
// moment the finalize's chat drain starts.
type flipOnEndChat struct {
	stop    chan struct{}
	once    sync.Once
	running atomic.Bool
	onEnd   func()
}

func (f *flipOnEndChat) Start(ctx context.Context) error {
	f.running.Store(true)
	defer f.running.Store(false)
	select {
	case <-f.stop:
	case <-ctx.Done():
	}
	return nil
}
func (f *flipOnEndChat) Stop()             { f.once.Do(func() { close(f.stop) }) }
func (f *flipOnEndChat) MessageCount() int { return 3 }
func (f *flipOnEndChat) IsRunning() bool   { return f.running.Load() }
func (f *flipOnEndChat) MarkStreamEnded() {
	if f.onEnd != nil {
		f.onEnd()
	}
	f.Stop()
}

// The connectivity callback stayed live after the session loop, and the
// finalize ran on the session context it cancels: a blip during the chat
// drain or the final mux — none of which needs the network — cancelled
// FFmpeg, and a complete recording landed in Error. The finalize now runs on
// a session of its own that only a user cancel or shutdown can end.
//
// Mutant: the callback still cancelling once finalizing, or the finalize
// running on the session context again.
func TestTwitchFinalizeSurvivesAnOfflineBlip(t *testing.T) {
	ts := oneSecondTS(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/live.m3u8" {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:1.000,\nseg0.ts\n#EXT-X-ENDLIST\n"))
			return
		}
		w.Header().Set("Content-Type", "video/mp2t")
		_, _ = w.Write(ts)
	}))
	t.Cleanup(srv.Close)

	h := newEndVerdictHarness(t, "tw_finalize_blip")
	h.variant.URL = srv.URL + "/live.m3u8"
	h.variant.CheckStreamFn = func(context.Context) (bool, error) { return true, nil }
	h.jobCtx.OutputDir = t.TempDir()
	h.jobCtx.Filename = "finalize-blip"
	chat := &flipOnEndChat{stop: make(chan struct{}), onEnd: func() {
		h.conn.set(false)
		h.conn.set(true)
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	err := h.o.ExecuteTwitch(ctx, h.jobCtx, h.variant, false, chat)
	fresh, _ := h.db.GetJob(h.job.ID)
	if err != nil || fresh == nil || fresh.Status != database.StatusFinished {
		t.Errorf("an offline blip during the finalize ended it as err=%v status=%v, want a clean Finished", err, statusOf(fresh))
	}
}
