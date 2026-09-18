package twitch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// resumeTimestampFloor is 2020-01-01T00:00:00Z in epoch MILLISECONDS. Any
// seconds-valued stamp taken this decade is three orders of magnitude below
// it, so this one comparison separates the two units without freezing a clock.
const resumeTimestampFloor int64 = 1577836800000

// TestBothChatResumeWritersStampMilliseconds is TWITCH-8 (report row #95). The
// shared ChatResumeState.Timestamp was written in ms by the IRC path and in
// seconds by the VOD path; nothing reads it, so the two sidecars silently
// disagreed about the unit of a field that exists only for a human reading the
// file. Milliseconds wins: it matches the dominant writer and LastTimestampMs
// beside it.
//
// Mutant: restoring time.Now().Unix() in vod_chat.go's saveResumeState — the
// VOD subtest's value drops below the floor by a factor of 1000.
func TestBothChatResumeWritersStampMilliseconds(t *testing.T) {
	t.Run("irc", func(t *testing.T) {
		cd := newSidecarTestChatDownloader(t)
		cd.saveResumeState()
		got := readResumeTimestamp(t, chatResumePath(cd.outputPath))
		if got < resumeTimestampFloor {
			t.Errorf("the IRC sidecar's timestamp is %d, which is seconds, not milliseconds", got)
		}
	})

	t.Run("vod", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "chat.json")
		vcd := NewVodChatDownloader(NewAPI(&testLogger{}), VodChatOptions{
			VodID:      "v1",
			OutputPath: out,
		}, &testLogger{})
		vcd.saveResumeState(0)
		got := readResumeTimestamp(t, out+".resume.json")
		if got < resumeTimestampFloor {
			t.Errorf("the VOD sidecar's timestamp is %d, which is seconds, not milliseconds", got)
		}
	})
}

// readResumeTimestamp reads one sidecar's `timestamp` field.
func readResumeTimestamp(t *testing.T, path string) int64 {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read sidecar %s: %v", path, err)
	}
	var state ChatResumeState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("unmarshal sidecar %s: %v", path, err)
	}
	if state.Timestamp == 0 {
		t.Fatalf("sidecar %s carries no timestamp", path)
	}
	return state.Timestamp
}

// TestResumeTimestampFloorIsBelowNow keeps the floor above honest: if the
// constant ever drifted past the current clock, the test above would pass for
// the wrong reason.
func TestResumeTimestampFloorIsBelowNow(t *testing.T) {
	if now := time.Now().UnixMilli(); resumeTimestampFloor >= now {
		t.Fatalf("resumeTimestampFloor (%d) is not in the past (now %d)", resumeTimestampFloor, now)
	}
}
