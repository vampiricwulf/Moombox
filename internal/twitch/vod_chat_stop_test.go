package twitch

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// installBlockingGQLStub answers every GQL request by parking until the
// request's own context is cancelled, then reporting that cancellation — the
// exact shape of a page fetch in flight when the job finalizes.
func installBlockingGQLStub(t *testing.T) chan struct{} {
	t.Helper()
	entered := make(chan struct{}, 1)
	prev := twitchHTTPClient
	t.Cleanup(func() { twitchHTTPClient = prev })
	twitchHTTPClient = &http.Client{Transport: probeRoundTripper(func(req *http.Request) (*http.Response, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	return entered
}

// TestVodChatStopAbortsTheInFlightPage is TWITCH-11 (report row #63). Stop()
// only flipped `running`, so a goroutine mid-GQL was bounded by the 30 s client
// timeout times four attempts plus ~7 s of backoff — minutes past the
// orchestrator's 2 s grace, during which the job finalized and removed staging.
//
// Mutant: dropping the sessionCancel call from Stop() — Start never returns and
// this test fails on its own 5 s deadline instead of hanging the suite.
//
// It also pins the log hygiene of that cancel: a deliberate Stop is not a fetch
// error, and Warn is the default level, so the cancelled page must produce no
// "vod chat fetch error" line — only finishInterrupted's one Info line, which
// is what leaves the outcome `incomplete` with a consistent sidecar offset.
//
// Mutant: removing the `if ctx.Err() != nil` guard at the top of the error
// branch in Start — the cancellation is counted as consecutiveErrors=1 and
// logged, so the Warn count becomes 1.
func TestVodChatStopAbortsTheInFlightPage(t *testing.T) {
	entered := installBlockingGQLStub(t)
	out := filepath.Join(t.TempDir(), "chat.json")
	rl := &renderingLogger{}
	vcd := NewVodChatDownloader(NewAPI(&testLogger{}), VodChatOptions{
		VodID:      "v1",
		OutputPath: out,
	}, rl)

	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- nil
			}
		}()
		done <- vcd.Start(context.Background())
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the downloader never reached a page fetch; the stub was not exercised")
	}

	vcd.Stop()

	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Start returned %v, want nil or context.Canceled — a deliberate Stop is "+
				"not a download failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return within 5 s of Stop() — the in-flight page was not " +
			"cancelled, so it outlives the orchestrator's 2 s grace and the staging removal")
	}

	if n := rl.countLinesContaining("vod chat fetch error"); n != 0 {
		t.Errorf("the cancelled page produced %d \"vod chat fetch error\" WARN line(s), want 0 — "+
			"a Stop that lands mid-page is a shutdown, not a fetch failure:\n%s", n, rl.allLines())
	}
	if n := rl.countLinesContaining("VOD chat download stopped before completion"); n != 1 {
		t.Errorf("finishInterrupted logged %d times, want exactly 1 — the quiet-cancel guard "+
			"must still flush and preserve the resume offset:\n%s", n, rl.allLines())
	}
}

// TestVodChatFlushNeverRecreatesARemovedOutputDirectory is the orphan half of
// TWITCH-11: the goroutine's exit flush called os.MkdirAll unconditionally, so
// a job that had already removed <staging>/<jobID>/ got it back, holding a
// chat.json and a resume sidecar nothing ever cleans up.
//
// Mutant: removing the outputDirGone() guard from flush() — the directory
// exists again after the second flush.
func TestVodChatFlushNeverRecreatesARemovedOutputDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "job-1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	out := filepath.Join(dir, "chat.json")
	vcd := NewVodChatDownloader(NewAPI(&testLogger{}), VodChatOptions{
		VodID:      "v1",
		OutputPath: out,
	}, &testLogger{})

	vcd.messages = []TwitchChatMessage{{ID: "c1", Message: "first"}}
	vcd.totalCount.Store(1)
	vcd.flush()
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("the first flush wrote no file (%v) — this test says nothing without one", err)
	}

	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("remove staging: %v", err)
	}

	vcd.messages = []TwitchChatMessage{{ID: "c2", Message: "second"}}
	vcd.totalCount.Store(2)
	vcd.flush()
	vcd.saveResumeState(0)

	if _, err := os.Stat(dir); err == nil {
		t.Error("the exit flush recreated the removed staging directory — an orphan tree " +
			"holding a chat.json and a resume sidecar that nothing ever cleans")
	}
}

// TestVodChatFirstFlushStillCreatesItsDirectory guards the other edge: the
// guard must not stop a genuine first write. Before any file exists the
// directory is the worker's own staging dir and MkdirAll is the ordinary path.
//
// Mutant: an unconditional dir-exists guard (no wroteFile gate) — the first
// flush then writes nothing.
func TestVodChatFirstFlushStillCreatesItsDirectory(t *testing.T) {
	out := filepath.Join(t.TempDir(), "not-created-yet", "chat.json")
	vcd := NewVodChatDownloader(NewAPI(&testLogger{}), VodChatOptions{
		VodID:      "v1",
		OutputPath: out,
	}, &testLogger{})

	vcd.messages = []TwitchChatMessage{{ID: "c1", Message: "first"}}
	vcd.totalCount.Store(1)
	vcd.flush()

	if _, err := os.Stat(out); err != nil {
		t.Errorf("the first flush wrote nothing (%v) — the orphan guard must only fire for a "+
			"directory that existed and was removed", err)
	}
}
