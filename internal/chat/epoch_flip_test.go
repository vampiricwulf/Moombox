package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestReplayPassInALiveRunKeepsTheFilesEpoch pins one epoch per file across
// the live→replay flip. The live half's offsets count from the file's epoch
// (the scheduled start here); the replay pass is handed YouTube's
// videoOffsetTimeMsec, which counts from the actual start. Kept as is, the
// replay tail sat (actual − scheduled) early relative to the live half, and
// the player's one per-file bias could not line both up.
//
// Mutant: derive only when `!msg.HasOffset` again — R1 keeps its
// actual-start offset and the gap reads 5 minutes short.
func TestReplayPassInALiveRunKeepsTheFilesEpoch(t *testing.T) {
	out := filepath.Join(t.TempDir(), "chat.json")
	scheduled := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	actual := scheduled.Add(5 * time.Minute) // went live five minutes late
	liveAt := scheduled.Add(10 * time.Minute)
	replayAt := scheduled.Add(20 * time.Minute)
	msg := func(id string, at time.Time) ChatMessage {
		return ChatMessage{
			ID:            id,
			TimestampUsec: fmt.Sprintf("%d", at.UnixMicro()),
			AuthorName:    "u",
			Message:       []MessagePart{{Type: "text", Text: "x"}},
		}
	}

	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID:             "vid",
		OutputFile:          out,
		InitialContinuation: "live0",
		IsLiveOrUpcoming:    true,
		StreamStartTime:     scheduled.Format(time.RFC3339),
	})
	call := 0
	cd.testFetchOverride = func(ctx context.Context) (*ChatApiResponse, error) {
		call++
		switch call {
		case 1:
			return &ChatApiResponse{Messages: []ChatMessage{msg("L1", liveAt)}, NextContinuation: "live1", TimeoutMs: 1}, nil
		case 2:
			return &ChatApiResponse{IsComplete: true}, nil // the live endpoint closes
		case 3:
			r := msg("R1", replayAt)
			r.OffsetMs = replayAt.Sub(actual).Milliseconds() // video-relative
			r.HasOffset = true
			return &ChatApiResponse{Messages: []ChatMessage{r}, IsComplete: true}, nil
		}
		return &ChatApiResponse{IsComplete: true}, nil
	}
	cd.testRecoveryOverride = func(ctx context.Context) bool {
		cd.adoptFreshContinuation("replay0", true) // the page now says isReplay
		return true
	}
	if err := cd.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !cd.isReplay() {
		t.Fatal("the run never flipped to the replay endpoint")
	}

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var data ChatData
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := map[string]int64{}
	for _, m := range data.Messages {
		got[m.ID] = m.OffsetMs
	}
	if want := liveAt.Sub(scheduled).Milliseconds(); got["L1"] != want {
		t.Errorf("L1 offset %d, want %d", got["L1"], want)
	}
	if want := replayAt.Sub(scheduled).Milliseconds(); got["R1"] != want {
		t.Errorf("R1 offset %d, want %d (the file's epoch, not YouTube's actual-start offset %d)",
			got["R1"], want, replayAt.Sub(actual).Milliseconds())
	}
}
