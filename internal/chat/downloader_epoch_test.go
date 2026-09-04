package chat

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

const testMsgUsec = int64(1_700_000_000_000_000) // chatResponseWithIDs' fixed timestampUsec

// Run 1 (early chat) computes offsets against the SCHEDULED start; the stream
// goes live 12 min late and Moombox restarts. Run 2 is built with the ACTUAL
// start but appends to the same file. One file, two epochs, one player bias
// — half the chat lands 12 min off. The sidecar must carry the epoch and the
// second run must keep it.
func TestResumeKeepsFileEpochOverNewOptions(t *testing.T) {
	out := filepath.Join(t.TempDir(), "chat.json")
	sched := "2026-06-11T10:00:00Z"
	actual := "2026-06-11T10:12:00Z"
	schedMs := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC).UnixMilli()

	cd1 := NewChatDownloader(ChatDownloaderOptions{
		VideoID: "vidEpoch", OutputFile: out, InitialContinuation: "tok0", ApiKey: "k",
		IsLiveOrUpcoming: true, StreamStartTime: sched,
	})
	cd1.testRecoveryOverride = func(context.Context) bool { return false } // stale exit keeps the sidecar
	startWithScript(t, cd1, chatResponseWithIDs([]string{"m1"}, ""))

	state, ok := readSidecar(t, out+".resume.json")
	if !ok || state.StreamStartMs != schedMs {
		t.Fatalf("sidecar streamStartMs = %d (present %v), want %d", state.StreamStartMs, ok, schedMs)
	}

	cd2 := NewChatDownloader(ChatDownloaderOptions{
		VideoID: "vidEpoch", OutputFile: out, InitialContinuation: "tok1", ApiKey: "k",
		IsLiveOrUpcoming: true, StreamStartTime: actual,
	})
	cd2.testRecoveryOverride = func(context.Context) bool { return false }
	startWithScript(t, cd2, chatResponseWithIDs([]string{"m2"}, ""))

	got := readChatFileHeader(t, out)
	if len(got.Messages) != 2 {
		t.Fatalf("want 2 messages, got %d", len(got.Messages))
	}
	wantOffset := testMsgUsec/1000 - schedMs
	if got.Messages[1].OffsetMs != wantOffset {
		t.Errorf("run-2 offset = %d, want %d (computed against the FILE's epoch, not the new options)", got.Messages[1].OffsetMs, wantOffset)
	}
	if got.StreamStartTime != sched {
		t.Errorf("header streamStartTime = %q, want the original %q", got.StreamStartTime, sched)
	}
}

// Same protection for the sidecar-less path: an adopted file's header epoch
// wins over the options the new run was built with.
func TestAdoptionKeepsFileEpoch(t *testing.T) {
	out := filepath.Join(t.TempDir(), "chat.json")
	sched := "2026-06-11T10:00:00Z"
	schedMs := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC).UnixMilli()
	seed := ChatData{
		VideoID: "vidAdoptEpoch", StreamStartTime: sched,
		DownloadedAt: time.Now().UTC().Format(time.RFC3339),
		MessageCount: 1, Messages: []ChatMessage{makeTestMessage("m1")},
	}
	if err := utils.WriteChatFileAtomic(out, &seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID: "vidAdoptEpoch", OutputFile: out, InitialContinuation: "tok0", ApiKey: "k",
		IsLiveOrUpcoming: true, StreamStartTime: "2026-06-11T10:12:00Z",
	})
	cd.testRecoveryOverride = func(context.Context) bool { return false }
	startWithScript(t, cd, chatResponseWithIDs([]string{"m2"}, ""))

	got := readChatFileHeader(t, out)
	if len(got.Messages) != 2 {
		t.Fatalf("want 2 messages, got %d", len(got.Messages))
	}
	if want := testMsgUsec/1000 - schedMs; got.Messages[1].OffsetMs != want {
		t.Errorf("appended offset = %d, want %d (file epoch)", got.Messages[1].OffsetMs, want)
	}
}
