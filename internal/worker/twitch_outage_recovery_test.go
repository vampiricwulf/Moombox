package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// liveTwitchWindow serves a live media playlist that never ends (a sliding
// window of one-second segments) and a real one-second MPEG-TS for every
// segment, and signals the first segment request.
func liveTwitchWindow(t *testing.T) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	ts := oneSecondTS(t)
	start := time.Now()
	var reqs atomic.Int32
	first := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			head := int(time.Since(start).Seconds())
			lo := max(0, head-5)
			var b strings.Builder
			fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:%d\n", lo)
			for s := lo; s <= head; s++ {
				fmt.Fprintf(&b, "#EXTINF:1.000,\n/seg/%d.ts\n", s)
			}
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = w.Write([]byte(b.String()))
			return
		}
		if reqs.Add(1) == 1 {
			first <- struct{}{}
		}
		w.Header().Set("Content-Type", "video/mp2t")
		_, _ = w.Write(ts)
	}))
	t.Cleanup(srv.Close)
	return srv, first
}

// oneSecondTS renders a real one-second MPEG-TS segment.
func oneSecondTS(t *testing.T) []byte {
	t.Helper()
	ffmpegPath, _ := requireFFmpegTools(t)
	segPath := filepath.Join(t.TempDir(), "seg.ts")
	if out, err := exec.Command(ffmpegPath, "-nostdin", "-y", "-f", "lavfi", "-i", "testsrc=size=64x64:rate=5:duration=1",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-f", "mpegts", segPath).CombinedOutput(); err != nil {
		t.Fatalf("make segment: %v\n%s", err, out)
	}
	ts, err := os.ReadFile(segPath)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

// outageThenRecover runs ExecuteTwitch on a broadcast that stays live, cuts
// connectivity once segments are landing, restores it, and returns how the
// run ended.
func outageThenRecover(t *testing.T, h *endVerdictHarness, srv *httptest.Server, first <-chan struct{}) (*database.Job, error) {
	t.Helper()
	old := postOutageRetryDelay
	postOutageRetryDelay = 10 * time.Millisecond
	t.Cleanup(func() { postOutageRetryDelay = old })

	h.variant.URL = srv.URL + "/live.m3u8"
	h.variant.CheckStreamFn = func(context.Context) (bool, error) { return true, nil }
	h.jobCtx.OutputDir = t.TempDir()
	h.jobCtx.Filename = "outage"

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.o.ExecuteTwitch(ctx, h.jobCtx, h.variant, false, h.chat) }()
	select {
	case <-first:
	case <-time.After(20 * time.Second):
		t.Fatal("no segment fetched")
	}
	time.Sleep(1500 * time.Millisecond)
	h.conn.set(false)
	time.Sleep(300 * time.Millisecond)
	h.conn.set(true)
	select {
	case err := <-done:
		fresh, _ := h.db.GetJob(h.job.ID)
		return fresh, err
	case <-time.After(45 * time.Second):
		t.Fatal("ExecuteTwitch did not return")
		return nil, nil
	}
}

// Back online after an outage, with the SAME broadcast confirmed live, a
// failed master-playlist refresh used to finalize the job as Finished on the
// spot — mid-broadcast — and the monitor, seeing a finished job for that
// stream, never re-archived the rest. It is retried, then re-verified like the
// in-loop refresh: a broadcast still live leaves the job in Error with its
// staging, never Finished.
//
// Mutant: the recovery finalizing on the first failed refresh again, or not
// retrying it.
func TestPostOutageRefreshFailureDoesNotFinishALiveBroadcast(t *testing.T) {
	srv, first := liveTwitchWindow(t)
	h := newEndVerdictHarness(t, "tw_outage_refresh")
	var refreshes atomic.Int32
	h.variant.FetchVariantsFn = func(context.Context) ([]twitch.TwitchHLSVariant, error) {
		refreshes.Add(1)
		return nil, errors.New("usher 503")
	}
	fresh, err := outageThenRecover(t, h, srv, first)
	if err == nil || fresh.Status == database.StatusFinished {
		t.Errorf("a still-live broadcast ended as err=%v status=%s, want an error and no Finished", err, fresh.Status)
	}
	if n := refreshes.Load(); n < postOutageRefreshAttempts {
		t.Errorf("the post-outage refresh was tried %d times, want %d", n, postOutageRefreshAttempts)
	}
}

// A post-outage recheck that never gets an answer is an UNKNOWN verdict; it
// used to read as "not live" and finalize the job Finished.
//
// Mutant: recheckTwitchBroadcast returning no error when every attempt failed,
// or the caller ignoring it.
func TestPostOutageRecheckFailureDoesNotFinishTheJob(t *testing.T) {
	srv, first := liveTwitchWindow(t)
	h := newEndVerdictHarness(t, "tw_outage_recheck")
	h.variant.RecheckStreamFn = func(context.Context) (*twitch.TwitchStreamInfo, error) {
		return nil, errors.New("gql unreachable")
	}
	fresh, err := outageThenRecover(t, h, srv, first)
	if err == nil || fresh.Status == database.StatusFinished {
		t.Errorf("an unanswered recheck ended as err=%v status=%s, want an error and no Finished", err, fresh.Status)
	}
}

// TestOutageFinalizeKeepsABoundarySpill: a part roll that could not write its
// boundary batch spilled it beside the part it closed and counted it in the
// downloader's rollUnwritten, which the current part's resume sidecar carries
// across a restart. When the broadcast then ends while connectivity is down,
// the outage finalize ends chat with Stop(), never MarkStreamEnded, and
// Start's interrupted exit returned nil before it read that count: the row
// read chat "finished" over a capture short by the spilled messages, and the
// staging cleanup deleted the spill with everything else. The verdict now
// says incomplete, and the cleanup keeps the chat capture, spill included.
//
// The staging tree is what a restart into the second part finds: the closed
// part at the root with its spill beside it, and seg_1 holding the current
// part and the sidecar with the count.
//
// Mutant: Start's interrupted-exit arm not returning rollUnwrittenErr — the
// row reads "finished" and the spill is gone after the cleanup.
func TestOutageFinalizeKeepsABoundarySpill(t *testing.T) {
	prevIRC := constants.TwitchURLs.IRCWS
	constants.TwitchURLs.IRCWS = "ws://127.0.0.1:1/" // refused at once: no IRC in this test
	t.Cleanup(func() { constants.TwitchURLs.IRCWS = prevIRC })

	srv, first := liveTwitchWindow(t)
	h := newEndVerdictHarness(t, "tw_outage_spill")
	// The recovery finds the broadcast over: the outage finalize.
	h.variant.RecheckStreamFn = func(context.Context) (*twitch.TwitchStreamInfo, error) {
		return &twitch.TwitchStreamInfo{IsLive: false}, nil
	}

	writeJSON := func(path string, v any) {
		t.Helper()
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	msg := func(id string, at int64) twitch.TwitchChatMessage {
		return twitch.TwitchChatMessage{ID: id, TimestampMs: at, Message: "hi", MessageType: "chat"}
	}
	staging := h.jobCtx.StagingDir
	rootChat := filepath.Join(staging, "chat.json")
	writeJSON(rootChat, twitch.TwitchChatData{Platform: "twitch", ChannelLogin: "streamer", StreamID: "s1",
		MessageCount: 2, EmoteOffsets: "utf16",
		Messages: []twitch.TwitchChatMessage{msg("a", 1767225600000), msg("b", 1767225601000)}})
	spill := rootChat + ".lostbatch.json"
	writeJSON(spill, []twitch.TwitchChatMessage{msg("x1", 1767225602000), msg("x2", 1767225602100),
		msg("x3", 1767225602200), msg("x4", 1767225602300), msg("x5", 1767225602400)})
	partChat := filepath.Join(staging, "seg_1", "chat.json")
	writeJSON(partChat, twitch.TwitchChatData{Platform: "twitch", ChannelLogin: "streamer", StreamID: "s1",
		MessageCount: 1, EmoteOffsets: "utf16", Messages: []twitch.TwitchChatMessage{msg("c", 1767225603000)}})
	writeJSON(partChat+".resume.json", twitch.ChatResumeState{MessageCount: 1, TotalCount: 3, RollUnwritten: 5,
		StreamID: "s1", RecentIDs: []string{"a", "b", "c"}})

	h.chat = twitch.NewChatDownloader(twitch.ChatDownloaderOptions{ChannelLogin: "streamer", StreamID: "s1",
		OutputPath: rootChat}, &discardLogger{})
	fresh, err := outageThenRecover(t, h, srv, first)
	if err != nil || fresh.Status != database.StatusFinished {
		t.Fatalf("the outage finalize ended as err=%v status=%s, want it Finished — this test is about its chat verdict",
			err, fresh.Status)
	}
	if fresh.ChatStatus != chatStatusIncomplete {
		t.Errorf("chat_status = %q after an outage finalize over a capture with 5 boundary messages spilled, want %q",
			fresh.ChatStatus, chatStatusIncomplete)
	}

	h.w.cleanupStagingAfterMux(h.job.ID, staging)
	if _, err := os.Stat(spill); err != nil {
		t.Errorf("the staging cleanup deleted the boundary spill: %v", err)
	}
}

// TestOutageFinalizeSpillsAFailedFinalFlush: the outage finalize ends chat
// with Stop() and records the verdict of Start's interrupted exit, and that
// exit ignored a final flush that could not write the pending messages. They
// stayed in memory, which nothing wrote afterwards; the row read chat
// "finished" counting them, and the staging cleanup deleted the sidecar. The
// exit now spills them beside the part and reports the capture incomplete, so
// the cleanup keeps the spill, and the row's total follows what the part
// files hold.
//
// The job resumes into a second part (seg_1) with a directory where that
// part's chat file belongs: the chat can neither read it nor write over it, so
// every flush holds the messages the fake IRC delivers. A later part, so the
// mux counts the directory as no chat rather than failing a copy of it — that
// would read "incomplete" for a reason of its own.
//
// Mutant: Start's interrupted-exit arm not calling spillUnwrittenBatch —
// the row reads "finished" with 2 messages and nothing is spilled.
func TestOutageFinalizeSpillsAFailedFinalFlush(t *testing.T) {
	at := time.Now().Add(-time.Minute)
	fakeIRC(t, privmsg("tail-1", at), privmsg("tail-2", at.Add(time.Second)), func() {})

	srv, first := liveTwitchWindow(t)
	h := newEndVerdictHarness(t, "tw_outage_flushfail")
	// The recovery finds the broadcast over: the outage finalize.
	h.variant.RecheckStreamFn = func(context.Context) (*twitch.TwitchStreamInfo, error) {
		return &twitch.TwitchStreamInfo{IsLive: false}, nil
	}
	staging := h.jobCtx.StagingDir
	partChat := filepath.Join(staging, "seg_1", "chat.json")
	if err := os.MkdirAll(partChat, 0o755); err != nil {
		t.Fatal(err)
	}
	h.chat = twitch.NewChatDownloader(twitch.ChatDownloaderOptions{ChannelLogin: "testchan", StreamID: "s1",
		OutputPath: filepath.Join(staging, "chat.json")}, &discardLogger{})

	fresh, err := outageThenRecover(t, h, srv, first)
	if err != nil || fresh.Status != database.StatusFinished {
		t.Fatalf("the outage finalize ended as err=%v status=%s, want it Finished — this test is about its chat verdict",
			err, fresh.Status)
	}
	if fresh.ChatStatus != chatStatusIncomplete {
		t.Errorf("chat_status = %q after an outage finalize whose final flush could not write its messages, want %q",
			fresh.ChatStatus, chatStatusIncomplete)
	}
	if n := fresh.TotalChatMessages; n == nil {
		t.Error("total_chat_messages unset, want 0: no part file holds a message")
	} else if *n != 0 {
		t.Errorf("total_chat_messages = %d, want 0: no part file holds a message", *n)
	}

	h.w.cleanupStagingAfterMux(h.job.ID, staging)
	raw, err := os.ReadFile(partChat + ".lostbatch.json")
	if err != nil {
		t.Fatalf("no spill of the unwritten messages after the cleanup: %v", err)
	}
	var spilled []twitch.TwitchChatMessage
	if err := json.Unmarshal(raw, &spilled); err != nil {
		t.Fatalf("the spill does not parse: %v", err)
	}
	if len(spilled) != 2 {
		t.Errorf("the spill holds %d messages, want the 2 the IRC delivered", len(spilled))
	}
}
