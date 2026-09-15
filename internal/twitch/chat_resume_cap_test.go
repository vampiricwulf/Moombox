package twitch

import (
	"path/filepath"
	"strconv"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// TestIRCResumeSidecarIsCapped is T2-15.
//
// The IRC sidecar snapshotted the ENTIRE dedup set — up to chatDedupMax*2
// entries — and it is written on every flush, i.e. about once a second on a
// busy channel. That is a ~200 KB marshal + fsync + rename every second, for a
// window the resume only ever needs the recent end of. The VOD path already
// capped at 1000; one constant now serves both.
//
// Mutants this kills:
//   - Snapshot(0) (today): 5000 IDs in the sidecar.
//   - Snapshot from the FRONT (the oldest 1000): a reconnect replays the most
//     RECENT messages, so an oldest-1000 window dedups nothing and the archive
//     gains duplicates. The order assertion below is what catches it.
func TestIRCResumeSidecarIsCapped(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "chat.json")
	cd := NewChatDownloader(ChatDownloaderOptions{
		ChannelLogin: "testchan",
		StreamID:     "stream-1",
		OutputPath:   out,
	}, &testLogger{})

	// OrderedDedup is deliberately not thread-safe (internal/utils/dedup.go):
	// its callers hold the downloader's mutex, so the fill does too. It must be
	// released before saveResumeState, which takes the same lock.
	const seen = 5000
	cd.mu.Lock()
	for i := range seen {
		cd.dedup.Add("msg_" + strconv.Itoa(i))
	}
	cd.mu.Unlock()

	cd.saveResumeState()

	store := utils.ResumeStore[ChatResumeState]{Path: chatResumePath(out)}
	state, err := store.Load()
	if err != nil {
		t.Fatalf("load resume state: %v", err)
	}
	if len(state.RecentIDs) != chatResumeIDCap {
		t.Fatalf("sidecar carries %d IDs, want %d", len(state.RecentIDs), chatResumeIDCap)
	}
	wantFirst := "msg_" + strconv.Itoa(seen-chatResumeIDCap)
	wantLast := "msg_" + strconv.Itoa(seen-1)
	if state.RecentIDs[0] != wantFirst {
		t.Errorf("first ID = %q, want %q — the cap kept the wrong end of the window",
			state.RecentIDs[0], wantFirst)
	}
	if state.RecentIDs[len(state.RecentIDs)-1] != wantLast {
		t.Errorf("last ID = %q, want %q", state.RecentIDs[len(state.RecentIDs)-1], wantLast)
	}
}

// TestBothChatResumePathsShareOneCap pins that the VOD side reads the same
// constant. Mutant: leaving vodChatResumeMaxRecentIDs in place beside the new
// name — two constants with the same value drift the moment one is tuned.
func TestBothChatResumePathsShareOneCap(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "vod.chat.json")
	vcd := NewVodChatDownloader(NewAPI(&testLogger{}), VodChatOptions{
		VodID:      "v1",
		OutputPath: out,
	}, &testLogger{})
	for i := range 5000 {
		vcd.dedup.Add("c" + strconv.Itoa(i))
	}
	vcd.saveResumeState(123)

	store := utils.ResumeStore[ChatResumeState]{Path: vcd.resumeStatePath()}
	state, err := store.Load()
	if err != nil {
		t.Fatalf("load resume state: %v", err)
	}
	if len(state.RecentIDs) != chatResumeIDCap {
		t.Errorf("VOD sidecar carries %d IDs, want %d", len(state.RecentIDs), chatResumeIDCap)
	}
}
