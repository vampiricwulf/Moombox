package chat

import (
	"context"
	"os"
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

	// Run 3: a saveResume regression that persists the OPTIONS epoch instead
	// of cd.streamStartMs would be invisible after run 2 alone — run 2's
	// in-memory offset is computed from cd.streamStartMs directly, not from
	// what saveResume wrote. Only a THIRD restart, which loads run 2's
	// sidecar, exposes it: it must still adopt the run-1 scheduled epoch, not
	// run 2's actual-start options.
	third := "2026-06-11T10:20:00Z"
	cd3 := NewChatDownloader(ChatDownloaderOptions{
		VideoID: "vidEpoch", OutputFile: out, InitialContinuation: "tok2", ApiKey: "k",
		IsLiveOrUpcoming: true, StreamStartTime: third,
	})
	cd3.testRecoveryOverride = func(context.Context) bool { return false }
	startWithScript(t, cd3, chatResponseWithIDs([]string{"m3"}, ""))

	got3 := readChatFileHeader(t, out)
	if len(got3.Messages) != 3 {
		t.Fatalf("want 3 messages after run 3, got %d", len(got3.Messages))
	}
	if want := testMsgUsec/1000 - schedMs; got3.Messages[2].OffsetMs != want {
		t.Errorf("run-3 offset = %d, want %d (still the run-1 file epoch, not run 2's persisted sidecar)", got3.Messages[2].OffsetMs, want)
	}
	if state3, ok := readSidecar(t, out+".resume.json"); !ok || state3.StreamStartMs != schedMs {
		t.Errorf("sidecar after run 3 streamStartMs = %d (present %v), want %d (saveResume must persist cd.streamStartMs, not the run's own options epoch)", state3.StreamStartMs, ok, schedMs)
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

// TestResumeKeepsFileEpochOverNewOptions's header assertion is vacuous on its
// own: run 2 there takes the incremental-append path, which never rewrites
// streamStartTime, so a mutant that makes epochRFC3339() return
// cd.opts.StreamStartTime unconditionally still passes it. Force the OTHER
// write path: delete chat.json (keep the sidecar) before run 2 — Start's
// on-disk cross-check then clears flushedToDisk because the file it expects
// to append to is gone, so run 2's first (and only) flush goes through
// writeFullChatFile, and the header it writes is the one under test.
func TestFullRewriteKeepsFileEpoch(t *testing.T) {
	out := filepath.Join(t.TempDir(), "chat.json")
	sched := "2026-06-11T10:00:00Z"
	actual := "2026-06-11T10:12:00Z"
	schedMs := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC).UnixMilli()

	cd1 := NewChatDownloader(ChatDownloaderOptions{
		VideoID: "vidFullRewrite", OutputFile: out, InitialContinuation: "tok0", ApiKey: "k",
		IsLiveOrUpcoming: true, StreamStartTime: sched,
	})
	cd1.testRecoveryOverride = func(context.Context) bool { return false } // stale exit keeps the sidecar
	startWithScript(t, cd1, chatResponseWithIDs([]string{"m1"}, ""))

	if _, ok := readSidecar(t, out+".resume.json"); !ok {
		t.Fatal("sidecar missing after run 1")
	}
	if err := os.Remove(out); err != nil {
		t.Fatalf("remove chat.json: %v", err)
	}

	cd2 := NewChatDownloader(ChatDownloaderOptions{
		VideoID: "vidFullRewrite", OutputFile: out, InitialContinuation: "tok1", ApiKey: "k",
		IsLiveOrUpcoming: true, StreamStartTime: actual,
	})
	cd2.testRecoveryOverride = func(context.Context) bool { return false }
	startWithScript(t, cd2, chatResponseWithIDs([]string{"m2"}, ""))

	got := readChatFileHeader(t, out)
	if got.StreamStartTime != sched {
		t.Errorf("full-rewrite header streamStartTime = %q, want the original %q", got.StreamStartTime, sched)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("want 1 message (chat.json was deleted before run 2, so run 1's message isn't recovered), got %d", len(got.Messages))
	}
	if want := testMsgUsec/1000 - schedMs; got.Messages[0].OffsetMs != want {
		t.Errorf("run-2 offset = %d, want %d (the sidecar's file epoch, via the full rewrite)", got.Messages[0].OffsetMs, want)
	}
}

// A sidecar saved before StreamStartMs existed has no such field. The
// `state.StreamStartMs > 0` guard on the resume branch must leave
// cd.streamStartMs at whatever NewChatDownloader already parsed from this
// run's own options rather than zeroing it — processBatch only computes an
// offset when cd.streamStartMs > 0, so zeroing it would silently strip
// offsets from every message this run appends.
func TestPreUpgradeSidecarKeepsOptionsEpoch(t *testing.T) {
	out := filepath.Join(t.TempDir(), "chat.json")
	optsStart := "2026-06-11T10:12:00Z"
	optsMs := time.Date(2026, 6, 11, 10, 12, 0, 0, time.UTC).UnixMilli()

	seed := ChatData{
		VideoID: "vidPreUpgrade", StreamStartTime: "2026-06-11T10:00:00Z",
		DownloadedAt: time.Now().UTC().Format(time.RFC3339),
		MessageCount: 1, Messages: []ChatMessage{makeTestMessage("m1")},
	}
	if err := utils.WriteChatFileAtomic(out, &seed); err != nil {
		t.Fatalf("seed chat file: %v", err)
	}
	// Hand-write a pre-upgrade sidecar: every field the CURRENT
	// ChatResumeState has except streamStartMs, which did not exist yet.
	preUpgrade := `{"messageCount":1,"continuation":"tok-old","timestamp":1700000000,"videoId":"vidPreUpgrade","recentIds":["m1"]}`
	if err := os.WriteFile(out+".resume.json", []byte(preUpgrade), 0o644); err != nil {
		t.Fatalf("write pre-upgrade sidecar: %v", err)
	}

	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID: "vidPreUpgrade", OutputFile: out, InitialContinuation: "tok1", ApiKey: "k",
		IsLiveOrUpcoming: true, StreamStartTime: optsStart,
	})
	cd.testRecoveryOverride = func(context.Context) bool { return false }
	startWithScript(t, cd, chatResponseWithIDs([]string{"m2"}, ""))

	got := readChatFileHeader(t, out)
	var appended *ChatMessage
	for i := range got.Messages {
		if got.Messages[i].ID == "m2" {
			appended = &got.Messages[i]
		}
	}
	if appended == nil {
		t.Fatalf("appended message m2 not found in %+v", got.Messages)
	}
	if want := testMsgUsec/1000 - optsMs; appended.OffsetMs != want {
		t.Errorf("appended offset = %d, want %d (the run's OWN options epoch — the pre-upgrade sidecar has no streamStartMs to override it)", appended.OffsetMs, want)
	}
}

// Chat offsets are millisecond quantities, so a fractional-second start time
// has to survive the header round-trip: rendering it with RFC3339 (which has
// no fractional field) would truncate the epoch by up to 999 ms and leave the
// header describing a different clock from the offsets underneath it. Same
// shape as TestFullRewriteKeepsFileEpoch — chat.json is deleted between the
// runs so the second one takes the full-rewrite path, the only one that
// writes the header under test.
func TestFullRewriteKeepsFractionalFileEpoch(t *testing.T) {
	out := filepath.Join(t.TempDir(), "chat.json")
	sched := "2026-06-11T10:00:00.250Z"
	actual := "2026-06-11T10:12:00Z"
	schedMs := time.Date(2026, 6, 11, 10, 0, 0, 250_000_000, time.UTC).UnixMilli()

	cd1 := NewChatDownloader(ChatDownloaderOptions{
		VideoID: "vidFracEpoch", OutputFile: out, InitialContinuation: "tok0", ApiKey: "k",
		IsLiveOrUpcoming: true, StreamStartTime: sched,
	})
	cd1.testRecoveryOverride = func(context.Context) bool { return false } // stale exit keeps the sidecar
	startWithScript(t, cd1, chatResponseWithIDs([]string{"m1"}, ""))

	if state, ok := readSidecar(t, out+".resume.json"); !ok || state.StreamStartMs != schedMs {
		t.Fatalf("sidecar streamStartMs = %d (present %v), want %d", state.StreamStartMs, ok, schedMs)
	}
	if err := os.Remove(out); err != nil {
		t.Fatalf("remove chat.json: %v", err)
	}

	cd2 := NewChatDownloader(ChatDownloaderOptions{
		VideoID: "vidFracEpoch", OutputFile: out, InitialContinuation: "tok1", ApiKey: "k",
		IsLiveOrUpcoming: true, StreamStartTime: actual,
	})
	cd2.testRecoveryOverride = func(context.Context) bool { return false }
	startWithScript(t, cd2, chatResponseWithIDs([]string{"m2"}, ""))

	got := readChatFileHeader(t, out)
	parsed, err := time.Parse(time.RFC3339, got.StreamStartTime)
	if err != nil {
		t.Fatalf("header streamStartTime %q does not parse as RFC3339: %v", got.StreamStartTime, err)
	}
	if parsed.UnixMilli() != schedMs {
		t.Errorf("header streamStartTime = %q (%d ms), want %d ms — the fraction must survive the round-trip",
			got.StreamStartTime, parsed.UnixMilli(), schedMs)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("want 1 message (chat.json was deleted before run 2), got %d", len(got.Messages))
	}
	if want := testMsgUsec/1000 - schedMs; got.Messages[0].OffsetMs != want {
		t.Errorf("run-2 offset = %d, want %d (the same fractional epoch the header claims)", got.Messages[0].OffsetMs, want)
	}
}

// A run with no start time at all has no epoch to describe, and epochRFC3339's
// `cd.streamStartMs > 0` guard is what keeps the header honest about it:
// rendering UnixMilli(0) instead would stamp 1970-01-01T00:00:00Z, which the
// player reads as a real epoch and subtracts from every message — a 56-year
// bias on a file whose messages carry no offset in the first place.
func TestNoStartTimeWritesEmptyHeaderEpoch(t *testing.T) {
	out := filepath.Join(t.TempDir(), "chat.json")

	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID: "vidNoEpoch", OutputFile: out, InitialContinuation: "tok0", ApiKey: "k",
		IsLiveOrUpcoming: true, // StreamStartTime deliberately unset
	})
	cd.testRecoveryOverride = func(context.Context) bool { return false }
	startWithScript(t, cd, chatResponseWithIDs([]string{"m1"}, ""))

	got := readChatFileHeader(t, out)
	if got.StreamStartTime != "" {
		t.Errorf("header streamStartTime = %q, want empty — there is no epoch to describe", got.StreamStartTime)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("want 1 message, got %d", len(got.Messages))
	}
	if got.Messages[0].HasOffset {
		t.Errorf("message hasOffset = true (offsetMs %d), want false with no epoch to count from", got.Messages[0].OffsetMs)
	}
}
