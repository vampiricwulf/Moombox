package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// damagedResumeMsg is one live message at a distinct wallclock instant.
func damagedResumeMsg(id string, i int) ChatMessage {
	return ChatMessage{
		ID:            id,
		TimestampUsec: fmt.Sprintf("%d", time.Date(2025, 1, 1, 0, 1, 0, 0, time.UTC).UnixMicro()+int64(i)*1000),
		AuthorName:    "u",
		Message:       []MessagePart{{Type: "text", Text: "x"}},
	}
}

// runLiveUntilStale runs one live capture that writes ids and then gives up
// on a stale continuation, leaving the file and its resume sidecar behind.
func runLiveUntilStale(t *testing.T, out string, ids []string, errs *[]error) *ChatDownloader {
	t.Helper()
	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID:             "vid",
		OutputFile:          out,
		InitialContinuation: "live0",
		IsLiveOrUpcoming:    true,
		StreamStartTime:     "2025-01-01T00:00:00Z",
	})
	if errs != nil {
		cd.OnError = func(err error) { *errs = append(*errs, err) }
	}
	call := 0
	cd.testFetchOverride = func(ctx context.Context) (*ChatApiResponse, error) {
		call++
		if call == 1 {
			msgs := make([]ChatMessage, len(ids))
			for i, id := range ids {
				msgs[i] = damagedResumeMsg(id, i)
			}
			return &ChatApiResponse{Messages: msgs, NextContinuation: "live1", TimeoutMs: 1}, nil
		}
		return &ChatApiResponse{IsComplete: true}, nil
	}
	cd.testRecoveryOverride = func(ctx context.Context) bool { return false }
	_ = cd.Start(context.Background())
	return cd
}

// readDamagedResumeFile parses the chat file, failing the test if it does not.
func readDamagedResumeFile(t *testing.T, out string) ChatData {
	t.Helper()
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var d ChatData
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("the chat file no longer parses: %v", err)
	}
	return d
}

func damagedResumeIDs(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("old%03d", i)
	}
	return ids
}

// TestResumeOverDamagedChatFileSalvagesHistory covers the two shapes a crash
// leaves: a tail that grew but was never written (zeros, so no ']' at all),
// and a file cut mid-record (its last ']' inside the final message). The
// resume trusted the file on a stat alone: the first shape had its 50
// messages replaced by the one new batch while the header counted 51, and the
// second had the batch spliced into the last record, reported as success,
// leaving a file that no longer parsed. Both now keep every intact message,
// keep the original bytes as .corrupt, report it, and count what the array
// holds.
//
// Mutants: drop the ChatFileEndIntact repair in Start AND the closing-'}'
// check in AppendChatMessages — the cut file stays unparseable; make
// prependExistingMessages ignore a damaged read again (return true with no
// messages) — the zero-tail file keeps 1 message.
func TestResumeOverDamagedChatFileSalvagesHistory(t *testing.T) {
	for _, tc := range []struct {
		name    string
		damage  func(t *testing.T, out string)
		minKept int
	}{
		{"zero-filled tail", func(t *testing.T, out string) {
			f, err := os.OpenFile(out, os.O_WRONLY|os.O_APPEND, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err := f.Write(make([]byte, 300)); err != nil {
				t.Fatal(err)
			}
		}, 50},
		{"cut mid-record", func(t *testing.T, out string) {
			st, err := os.Stat(out)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(out, st.Size()-40); err != nil {
				t.Fatal(err)
			}
		}, 49},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "chat.json")
			runLiveUntilStale(t, out, damagedResumeIDs(50), nil)
			if _, ok := readSidecar(t, out+".resume.json"); !ok {
				t.Fatal("precondition: the stale exit keeps its sidecar")
			}
			tc.damage(t, out)
			damaged, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}

			var errs []error
			cd := NewChatDownloader(ChatDownloaderOptions{
				VideoID:             "vid",
				OutputFile:          out,
				InitialContinuation: "fresh0",
				IsLiveOrUpcoming:    true,
				StreamStartTime:     "2025-01-01T00:00:00Z",
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

			d := readDamagedResumeFile(t, out)
			if len(d.Messages) < tc.minKept+1 {
				t.Errorf("file holds %d messages, want the %d intact old ones plus the new one", len(d.Messages), tc.minKept)
			}
			if d.Messages[len(d.Messages)-1].ID != "new1" {
				t.Errorf("last message %q, want the new batch after the history", d.Messages[len(d.Messages)-1].ID)
			}
			if d.MessageCount != len(d.Messages) || cd.MessageCount() != len(d.Messages) {
				t.Errorf("header %d / downloader %d, want the array's %d", d.MessageCount, cd.MessageCount(), len(d.Messages))
			}
			kept, err := os.ReadFile(out + corruptChatSuffix)
			if err != nil {
				t.Fatalf("the damaged original was not kept: %v", err)
			}
			if string(kept) != string(damaged) {
				t.Error("the .corrupt copy is not the damaged original")
			}
			if len(errs) == 0 {
				t.Error("the damage was not reported")
			}
		})
	}
}

// TestAppendRefusesAFileCutMidRecord pins the shared append's own check: a
// last ']' that is not followed by the closing '}' belongs to a message's own
// array, and splicing there left a file that no longer parsed while reporting
// success. Nothing is written, and the error says the file is damaged.
//
// Mutant: drop the closesChatDocument check — the append succeeds.
func TestAppendRefusesAFileCutMidRecord(t *testing.T) {
	out := filepath.Join(t.TempDir(), "chat.json")
	runLiveUntilStale(t, out, damagedResumeIDs(5), nil)
	st, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(out, st.Size()-40); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(out)
	err = utils.AppendChatMessages(out, []ChatMessage{damagedResumeMsg("new1", 9)}, 6, nil)
	if !errors.Is(err, utils.ErrChatFileDamaged) {
		t.Fatalf("append over a cut file: %v, want ErrChatFileDamaged", err)
	}
	if after, _ := os.ReadFile(out); string(after) != string(before) {
		t.Error("the refused append changed the file")
	}
}

// TestResumeWithItsFileGoneCountsFromZero: a sidecar whose chat file is gone
// kept its count, so the first full write's header — and the job row fed from
// MessageCount — counted messages the new array did not have.
//
// Mutant: drop `cd.messageCount = 0` in the stat-failure arm — header 11,
// array 1.
func TestResumeWithItsFileGoneCountsFromZero(t *testing.T) {
	out := filepath.Join(t.TempDir(), "chat.json")
	runLiveUntilStale(t, out, damagedResumeIDs(10), nil)
	if err := os.Remove(out); err != nil {
		t.Fatal(err)
	}
	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID:             "vid",
		OutputFile:          out,
		InitialContinuation: "fresh0",
		IsLiveOrUpcoming:    true,
		StreamStartTime:     "2025-01-01T00:00:00Z",
	})
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

	d := readDamagedResumeFile(t, out)
	if len(d.Messages) != 1 || d.MessageCount != 1 || cd.MessageCount() != 1 {
		t.Errorf("array %d, header %d, downloader %d; want 1 each", len(d.Messages), d.MessageCount, cd.MessageCount())
	}
}
