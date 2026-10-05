package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// captureLiveArchive runs one live capture that commits messages 0..total-1
// of the synthetic archive and then gives up on a stale continuation, leaving
// the file and its sidecar (replay mark included) behind.
func captureLiveArchive(t *testing.T, out string, total int) {
	t.Helper()
	live := NewChatDownloader(ChatDownloaderOptions{
		VideoID: "vid", OutputFile: out, InitialContinuation: "live0",
		IsLiveOrUpcoming: true, StreamStartTime: "2025-01-01T00:00:00Z",
	})
	call := 0
	live.testFetchOverride = func(ctx context.Context) (*ChatApiResponse, error) {
		call++
		if call == 1 {
			resp := &ChatApiResponse{NextContinuation: "live1", TimeoutMs: 1}
			for i := range total {
				resp.Messages = append(resp.Messages, buildArchiveMessage(i))
			}
			return resp, nil
		}
		return &ChatApiResponse{IsComplete: true}, nil
	}
	live.testRecoveryOverride = func(ctx context.Context) bool { return false }
	_ = live.Start(context.Background())
	if st, ok := readSidecar(t, out+".resume.json"); !ok || st.ReplayHighWaterUsec == nil {
		t.Fatalf("precondition: the live run left a sidecar with a replay mark: %v %+v", ok, st)
	}
}

// replayArchive runs a replay-token pass over the whole archive.
func replayArchive(t *testing.T, out string, total int) {
	t.Helper()
	replay := NewChatDownloader(ChatDownloaderOptions{
		VideoID: "vid", OutputFile: out, InitialContinuation: "replay-top",
		IsLiveOrUpcoming: true, IsReplay: true, StreamStartTime: "2025-01-01T00:00:00Z",
	})
	replay.testFetchOverride = func(ctx context.Context) (*ChatApiResponse, error) {
		from := 0
		if replay.continuation != "replay-top" {
			fmt.Sscanf(replay.continuation, "replay-%d", &from)
		}
		return archivePage(from, total), nil
	}
	_ = replay.Start(context.Background())
}

// TestHistoryTheDiskLostIsNotTreatedAsCommitted: the restored replay mark and
// dedup describe what the sidecar's run committed. When the file is gone, or
// a salvage keeps only part of it, they no longer describe the disk — and a
// replay pass dropped every lost message below the mark: 0 of 6000 recovered
// after the file vanished, half after a cut. Both are now cleared with the
// file, or rebuilt from what the salvage kept.
//
// Mutants: keep the mark in the stat-failure arm (the "file gone" row
// recovers nothing); skip the rebuild after a damaged salvage (the "cut in
// half" row loses the tail).
func TestHistoryTheDiskLostIsNotTreatedAsCommitted(t *testing.T) {
	const total = 6000
	for _, tc := range []struct {
		name   string
		damage func(t *testing.T, out string)
	}{
		{"file gone", func(t *testing.T, out string) {
			if err := os.Remove(out); err != nil {
				t.Fatal(err)
			}
		}},
		{"cut in half", func(t *testing.T, out string) {
			st, err := os.Stat(out)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(out, st.Size()/2); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "chat.json")
			captureLiveArchive(t, out, total)
			tc.damage(t, out)
			replayArchive(t, out, total)
			if records, dups := readChatFileMessages(t, out); records != total || dups != 0 {
				t.Errorf("%d records, %d duplicates; want all %d archive messages once", records, dups, total)
			}
		})
	}
}

// TestAnUnrestoredPartialAppendWritesNoDuplicates: an append whose write
// stopped partway and whose end could not be put back leaves the messages it
// did write in the file. The batch is kept for the retry, the retry finds the
// file damaged and salvages those messages — and the batch then wrote them a
// second time.
//
// Mutant: prepend the salvaged history without dropping the batch's IDs it
// already holds — m2 is in the file twice.
func TestAnUnrestoredPartialAppendWritesNoDuplicates(t *testing.T) {
	out := filepath.Join(t.TempDir(), "chat.json")
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "v1", OutputFile: out})
	cd.processBatch(&ChatApiResponse{Messages: []ChatMessage{buildArchiveMessage(1)}})
	cd.writeChatFile()

	real := appendChatMessages
	t.Cleanup(func() { appendChatMessages = real })
	appendChatMessages = func(path string, msgs []ChatMessage, _ int, _ utils.ChatFileLogger) error {
		// What AppendChatMessages leaves when WriteAt got the first message
		// and part of the second out, and the "]\n}" restore failed too.
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		pos := bytes.LastIndexByte(raw, ']')
		first, _ := json.Marshal(msgs[0])
		partial := append([]byte(",\n    "), first...)
		partial = append(partial, []byte(",\n    {\"id\":\"m0000")...)
		f, err := os.OpenFile(path, os.O_RDWR, 0o644)
		if err != nil {
			return err
		}
		_, _ = f.WriteAt(partial, int64(pos))
		_ = f.Close()
		return fmt.Errorf("%w: input/output error", utils.ErrChatFilePartialWrite)
	}
	cd.processBatch(&ChatApiResponse{Messages: []ChatMessage{buildArchiveMessage(2), buildArchiveMessage(3)}})
	cd.writeChatFile()
	appendChatMessages = real
	cd.processBatch(&ChatApiResponse{Messages: []ChatMessage{buildArchiveMessage(4)}})
	cd.writeChatFile()

	records, dups := readChatFileMessages(t, out)
	if records != 4 || dups != 0 {
		t.Errorf("%d records, %d duplicates; want m1..m4 once each", records, dups)
	}
	if cd.MessageCount() != records {
		t.Errorf("MessageCount %d, the file holds %d", cd.MessageCount(), records)
	}
}

// TestASalvageThatKeptNothingWritesAnEmptyArray: a salvage with no intact
// message wrote "messages": null, and the next append — finding no ']' —
// reported an IO error before rewriting the file.
//
// Mutant: write cd.messages as is (nil) in writeFullChatFile.
func TestASalvageThatKeptNothingWritesAnEmptyArray(t *testing.T) {
	out := filepath.Join(t.TempDir(), "chat.json")
	runLiveUntilStale(t, out, damagedResumeIDs(3), nil)
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	head := []byte(`"messages": [`)
	i := bytes.Index(raw, head)
	if i < 0 {
		t.Fatal("no messages array in the file")
	}
	if err := os.WriteFile(out, append(raw[:i+len(head)], make([]byte, 300)...), 0o644); err != nil {
		t.Fatal(err)
	}

	var errs []error
	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID: "vid", OutputFile: out, InitialContinuation: "fresh0",
		IsLiveOrUpcoming: true, StreamStartTime: "2025-01-01T00:00:00Z",
	})
	cd.OnError = func(err error) { errs = append(errs, err) }
	call := 0
	cd.testFetchOverride = func(ctx context.Context) (*ChatApiResponse, error) {
		call++
		if call == 1 {
			return &ChatApiResponse{Messages: []ChatMessage{damagedResumeMsg("new1", 100)}, NextContinuation: "n", TimeoutMs: 1}, nil
		}
		return &ChatApiResponse{IsComplete: true}, nil
	}
	cd.testRecoveryOverride = func(ctx context.Context) bool { return false }
	_ = cd.Start(context.Background())

	for _, err := range errs {
		if strings.Contains(err.Error(), "chat file append") {
			t.Errorf("the append after an empty salvage failed: %v", err)
		}
	}
	d := readDamagedResumeFile(t, out)
	if len(d.Messages) != 1 || d.Messages[0].ID != "new1" || d.MessageCount != 1 {
		t.Errorf("file holds %d messages (header %d), want just new1", len(d.Messages), d.MessageCount)
	}
}
