package chat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// replayFetch serves an archive of total messages page by page from the
// downloader's own continuation ("replay-N"), and fails with a 503 once the
// page it is asked for starts at or past failFrom (-1: never).
func replayFetch(cd *ChatDownloader, total, failFrom int) func(context.Context) (*ChatApiResponse, error) {
	return func(context.Context) (*ChatApiResponse, error) {
		var from int
		fmt.Sscanf(cd.continuation, "replay-%d", &from)
		if failFrom >= 0 && from >= failFrom {
			return nil, errors.New("HTTP 503 from get_live_chat_replay")
		}
		return archivePage(from, total), nil
	}
}

// runReplay runs one replay pass over out and returns Start's error.
func runReplay(t *testing.T, out string, total, failFrom int) (*ChatDownloader, error) {
	t.Helper()
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "vidRerun", OutputFile: out, IsReplay: true, InitialContinuation: "replay-0"})
	cd.testBackoffOverride = time.Millisecond
	cd.testFetchOverride = replayFetch(cd, total, failFrom)
	return cd, cd.Start(context.Background())
}

// TestReplayRerunKeepsTheMoreCompleteArchive is V6's case: a replay chat
// completed (2000 messages, sidecar cleared), the process restarted, and the
// re-run gave up after one page. The complete archive must survive it, and
// the downloader must report the archive's count, not the fragment's.
//
// Mutants: make beginReplayRerun return nil — the re-run's first flush
// rewrites chat.json and the give-up leaves 200 messages; skip the count
// restore — MessageCount reports the fragment's 200; skip the .rerun removal
// — the fragment stays on disk; apply the completion rule after a discard —
// a sidecar describing the discarded re-run lands beside the archive.
func TestReplayRerunKeepsTheMoreCompleteArchive(t *testing.T) {
	out := filepath.Join(t.TempDir(), "chat.json")
	if _, err := runReplay(t, out, 2000, -1); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	cd, err := runReplay(t, out, 2000, archivePageSize)
	if err == nil {
		t.Fatal("run 2 was meant to give up")
	}
	if n, dups := readChatFileMessages(t, out); n != 2000 || dups != 0 {
		t.Errorf("chat.json holds %d messages (%d duplicates) after the re-run gave up, want the complete 2000", n, dups)
	}
	if got := cd.MessageCount(); got != 2000 {
		t.Errorf("MessageCount = %d, want the kept archive's 2000", got)
	}
	if _, err := os.Stat(out + replayRerunSuffix); !os.IsNotExist(err) {
		t.Errorf("the discarded re-run is still on disk (%v)", err)
	}
	if _, err := os.Stat(out + ".resume.json"); !os.IsNotExist(err) {
		t.Errorf("a sidecar now describes the archive (%v); it would describe the discarded re-run", err)
	}
}

// TestReplayRerunReplacesOnCompletion: a re-run that reaches the end of the
// archive is the archive as YouTube now serves it, even when it is shorter.
//
// Mutant: drop `completed ||` from finishReplayRerun — the shorter complete
// re-run is discarded.
func TestReplayRerunReplacesOnCompletion(t *testing.T) {
	out := filepath.Join(t.TempDir(), "chat.json")
	if _, err := runReplay(t, out, 2000, -1); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if _, err := runReplay(t, out, 1500, -1); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if n, _ := readChatFileMessages(t, out); n != 1500 {
		t.Errorf("chat.json holds %d messages, want the completed re-run's 1500", n)
	}
}

// TestReplayRerunThatGetsFurtherReplacesAndResumes: a re-run that gives up
// holding more than the archive it found replaces it, and — being no
// completion — leaves the sidecar that lets the next run carry on from there.
//
// Mutant: drop `got >= rr.existing` from finishReplayRerun — the further
// re-run is discarded.
func TestReplayRerunThatGetsFurtherReplacesAndResumes(t *testing.T) {
	out := filepath.Join(t.TempDir(), "chat.json")
	// An archive with no sidecar that stopped at 400 (an older build cleared
	// the sidecar of a replay that gave up).
	if _, err := runReplay(t, out, 2000, 2*archivePageSize); err == nil {
		t.Fatal("run 1 was meant to give up")
	}
	os.Remove(out + ".resume.json")

	if _, err := runReplay(t, out, 2000, 3*archivePageSize); err == nil {
		t.Fatal("run 2 was meant to give up")
	}
	if n, _ := readChatFileMessages(t, out); n != 3*archivePageSize {
		t.Fatalf("chat.json holds %d messages, want the re-run's %d", n, 3*archivePageSize)
	}
	if _, err := runReplay(t, out, 2000, -1); err != nil {
		t.Fatalf("run 3: %v", err)
	}
	if n, dups := readChatFileMessages(t, out); n != 2000 || dups != 0 {
		t.Errorf("after the resume chat.json holds %d messages (%d duplicates), want 2000", n, dups)
	}
}

// TestReplayGiveUpKeepsItsSidecar: a replay that gives up is not a
// completion, so its sidecar — its position in the archive — survives, and
// the next run resumes there instead of paging from the top.
//
// Mutant: put the completion rule back to `|| !IsLiveOrUpcoming` — the
// give-up clears the sidecar.
func TestReplayGiveUpKeepsItsSidecar(t *testing.T) {
	out := filepath.Join(t.TempDir(), "chat.json")
	if _, err := runReplay(t, out, 2000, 2*archivePageSize); err == nil {
		t.Fatal("run 1 was meant to give up")
	}
	if _, err := os.Stat(out + ".resume.json"); err != nil {
		t.Fatalf("the give-up cleared its sidecar: %v", err)
	}
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "vidRerun", OutputFile: out, IsReplay: true, InitialContinuation: "replay-0"})
	var firstFrom = -1
	fetch := replayFetch(cd, 2000, -1)
	cd.testFetchOverride = func(ctx context.Context) (*ChatApiResponse, error) {
		if firstFrom < 0 {
			fmt.Sscanf(cd.continuation, "replay-%d", &firstFrom)
		}
		return fetch(ctx)
	}
	if err := cd.Start(context.Background()); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if firstFrom != 2*archivePageSize {
		t.Errorf("run 2 started at message %d, want the sidecar's %d", firstFrom, 2*archivePageSize)
	}
	if n, dups := readChatFileMessages(t, out); n != 2000 || dups != 0 {
		t.Errorf("chat.json holds %d messages (%d duplicates), want 2000", n, dups)
	}
}
