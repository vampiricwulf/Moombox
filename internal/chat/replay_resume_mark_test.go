package chat

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestAReplayTokenRunOverALiveCaptureAppendsNothingTwice: a live run captured
// a 6000-message archive and left; the next run of the same job starts on a
// REPLAY token (the broadcast ended between probes) and pages the archive from
// the top. The replay high-water mark is what bounds that pass, and it was
// per-run state reset at Start, so the second run had none: the 5000-ID
// window could not span the archive, and every page's cull evicted exactly the
// next page's IDs — all 6000 came back as duplicates. The mark now travels in
// the sidecar and, on the sidecar-less path, is read off the adopted file.
//
// Mutants: drop the sidecar restore (the "sidecar" row duplicates); drop the
// adoption seed (the "adopted file" row duplicates).
func TestAReplayTokenRunOverALiveCaptureAppendsNothingTwice(t *testing.T) {
	const total = 6000
	for _, tc := range []struct {
		name        string
		dropSidecar bool
	}{
		{"sidecar", false},
		{"adopted file", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "chat.json")

			live := NewChatDownloader(ChatDownloaderOptions{
				VideoID:             "vid",
				OutputFile:          out,
				InitialContinuation: "live0",
				IsLiveOrUpcoming:    true,
				StreamStartTime:     "2025-01-01T00:00:00Z",
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
			if records, _ := readChatFileMessages(t, out); records != total {
				t.Fatalf("precondition: the live run wrote %d messages, want %d", records, total)
			}
			if tc.dropSidecar {
				if err := os.Remove(out + ".resume.json"); err != nil {
					t.Fatal(err)
				}
			}

			replay := NewChatDownloader(ChatDownloaderOptions{
				VideoID:             "vid",
				OutputFile:          out,
				InitialContinuation: "replay-top",
				IsLiveOrUpcoming:    true,
				IsReplay:            true,
				StreamStartTime:     "2025-01-01T00:00:00Z",
			})
			replay.testFetchOverride = func(ctx context.Context) (*ChatApiResponse, error) {
				from := 0
				if replay.continuation != "replay-top" {
					fmt.Sscanf(replay.continuation, "replay-%d", &from)
				}
				return archivePage(from, total), nil
			}
			if err := replay.Start(context.Background()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			records, dups := readChatFileMessages(t, out)
			if dups != 0 || records != total {
				t.Errorf("after the replay pass: %d records, %d duplicates; want %d and none", records, dups, total)
			}
		})
	}
}
