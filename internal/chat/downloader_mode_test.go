package chat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// --- harness -------------------------------------------------------------

// recordingHandler is scriptedHandler plus a record of the continuation token
// each poll carried, so a test can assert WHICH token a run started from —
// the sidecar's or the caller's own. Requests past the script get the last
// (terminal) response so an unexpected extra poll ends the loop instead of
// hanging it; the count is checked on the test goroutine afterwards.
type recordingHandler struct {
	mu        sync.Mutex
	responses []map[string]any
	seen      []string
}

func (h *recordingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body struct {
		Continuation string `json:"continuation"`
	}
	_ = json.Unmarshal(raw, &body)

	h.mu.Lock()
	h.seen = append(h.seen, body.Continuation)
	idx := len(h.seen) - 1
	if idx >= len(h.responses) {
		idx = len(h.responses) - 1
	}
	resp := h.responses[idx]
	h.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	out, _ := json.Marshal(resp)
	w.Write(out)
}

func (h *recordingHandler) tokens() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.seen...)
}

// startWithRecordedScript runs Start to completion against a recording server
// and returns the continuation token of every poll, in order.
func startWithRecordedScript(t *testing.T, cd *ChatDownloader, resps ...map[string]any) []string {
	t.Helper()
	handler := &recordingHandler{responses: resps}
	server := httptest.NewServer(handler)
	defer server.Close()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	cd.api.client = &http.Client{Transport: rewriteTransport{target: target}}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := cd.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("test context expired during Start — the run took a cancellation path, not the one under test")
	}
	return handler.tokens()
}

// writeSidecar writes a resume sidecar verbatim, the way a prior run's
// saveResume would have left it.
func writeSidecar(t *testing.T, path string, state ChatResumeState) {
	t.Helper()
	store := utils.ResumeStore[ChatResumeState]{Path: path}
	if err := store.Save(state); err != nil {
		t.Fatalf("seed sidecar: %v", err)
	}
}

// seedLiveHalf writes the artefacts a LIVE/upcoming run leaves behind: a
// chat.json holding the live half (epoch = the scheduled start) and the
// sidecar describing it. mode is written verbatim into the sidecar so a test
// can seed a legacy (empty) one.
func seedLiveHalf(t *testing.T, out, epoch string, epochMs int64, mode string) {
	t.Helper()
	seed := ChatData{
		VideoID:         "vidMode",
		StreamStartTime: epoch,
		DownloadedAt:    time.Now().UTC().Format(time.RFC3339),
		MessageCount:    1,
		Messages:        []ChatMessage{makeTestMessage("live1")},
	}
	if err := utils.WriteChatFileAtomic(out, &seed); err != nil {
		t.Fatalf("seed chat file: %v", err)
	}
	writeSidecar(t, out+".resume.json", ChatResumeState{
		MessageCount:  1,
		Continuation:  "sidecarTok",
		Timestamp:     time.Now().Unix(),
		VideoID:       "vidMode",
		RecentIDs:     []string{"dup1"},
		StreamStartMs: epochMs,
		Mode:          mode,
	})
}

// --- the mode rule (CP1) -------------------------------------------------

// A live/upcoming run's sidecar describes the LIVE half of a chat file: its
// count, continuation, dedup window and epoch were all reached against the
// live endpoint. A replay/VOD run that adopted them would append its archive
// on top of the live half and compute its offsets against the live run's
// epoch — the whole replay half then reads early by the late-start delta
// (player ledger CP1). The replay run must refuse a live-tagged sidecar and
// take the full-rewrite path the adoption rule already says it takes.
func TestReplayRunRefusesLiveRunSidecar(t *testing.T) {
	out := filepath.Join(t.TempDir(), "chat.json")
	liveEpoch := "2026-06-11T10:00:00Z"   // scheduled start — the live run's epoch
	replayEpoch := "2026-06-11T10:12:00Z" // actual start — the replay run's own
	liveEpochMs := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC).UnixMilli()
	replayEpochMs := time.Date(2026, 6, 11, 10, 12, 0, 0, time.UTC).UnixMilli()
	seedLiveHalf(t, out, liveEpoch, liveEpochMs, "live")

	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID: "vidMode", OutputFile: out, InitialContinuation: "replayTok", ApiKey: "k",
		IsReplay: true, IsLiveOrUpcoming: false, StreamStartTime: replayEpoch,
	})
	var logged []string
	logger := &recordingChatLogger{lines: &logged}
	cd.Logger = logger

	tokens := startWithRecordedScript(t, cd, chatResponseWithIDs([]string{"dup1"}, ""))

	if len(tokens) != 1 || tokens[0] != "replayTok" {
		t.Errorf("polled with %v, want [replayTok] — the replay run must keep its OWN continuation, not the live run's sidecar token", tokens)
	}
	got := readChatFileHeader(t, out)
	if len(got.Messages) != 1 || got.Messages[0].ID != "dup1" {
		t.Fatalf("chat file holds %d messages (%+v), want a full rewrite holding only this run's dup1 — a refused sidecar contributes neither count nor dedup IDs", len(got.Messages), got.Messages)
	}
	if got.MessageCount != 1 {
		t.Errorf("header messageCount = %d, want 1 (the sidecar's count must not be adopted)", got.MessageCount)
	}
	if want := testMsgUsec/1000 - replayEpochMs; got.Messages[0].OffsetMs != want {
		t.Errorf("offsetMs = %d, want %d (computed against the REPLAY run's epoch, not the live run's)", got.Messages[0].OffsetMs, want)
	}
	if got.StreamStartTime != replayEpoch {
		t.Errorf("header streamStartTime = %q, want %q (the refused sidecar's epoch must not travel)", got.StreamStartTime, replayEpoch)
	}
	// At INFO, not Debug: the rule discards a position on disk and rewrites
	// the file, so it has to be visible without turning debug on. And the
	// logger reaching this at all is what the worker's `dl.Logger = o.logger`
	// wiring buys — before it, ChatDownloader.Logger was nil in production
	// and this line went nowhere (D6).
	const refusal = "chat: ignoring the live run's resume sidecar for a replay run"
	if !containsLine(logged, refusal) {
		t.Errorf("no refusal log line; got %v", logged)
	}
	if !logger.loggedAt("info", refusal) {
		t.Errorf("refusal was not logged at info; got %v at %v", logged, logger.levels)
	}
}

// The mode rule is narrow: it refuses ONLY a live-tagged sidecar on a replay
// run. A replay run's own sidecar is its position in the archive, and a
// sidecar with no mode was written before the field existed — an upgrade must
// not strand a job that was mid-resume. Both are adopted exactly as before.
func TestReplayRunAdoptsNonLiveSidecar(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode string
	}{
		{"replay-tagged", "replay"},
		{"legacy sidecar with no mode", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "chat.json")
			epoch := "2026-06-11T10:00:00Z"
			epochMs := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC).UnixMilli()
			seedLiveHalf(t, out, epoch, epochMs, tc.mode)

			cd := NewChatDownloader(ChatDownloaderOptions{
				VideoID: "vidMode", OutputFile: out, InitialContinuation: "replayTok", ApiKey: "k",
				IsReplay: true, IsLiveOrUpcoming: false, StreamStartTime: "2026-06-11T10:12:00Z",
			})

			tokens := startWithRecordedScript(t, cd, chatResponseWithIDs([]string{"dup1", "new1"}, ""))

			if len(tokens) != 1 || tokens[0] != "sidecarTok" {
				t.Errorf("polled with %v, want [sidecarTok] — a replay sidecar's continuation always wins", tokens)
			}
			got := readChatFileHeader(t, out)
			if len(got.Messages) != 2 || got.Messages[0].ID != "live1" || got.Messages[1].ID != "new1" {
				t.Fatalf("chat file holds %+v, want the seeded live1 appended with new1 (dup1 suppressed by the restored dedup)", got.Messages)
			}
			if got.MessageCount != 2 {
				t.Errorf("header messageCount = %d, want 2 (sidecar count 1 + 1 new)", got.MessageCount)
			}
			if want := testMsgUsec/1000 - epochMs; got.Messages[1].OffsetMs != want {
				t.Errorf("appended offsetMs = %d, want %d (the sidecar's epoch is adopted)", got.Messages[1].OffsetMs, want)
			}
		})
	}
}

// saveResume must tag every sidecar with the kind of run that wrote it —
// that tag is the only thing the mode rule above has to go on.
func TestSaveResumeRecordsRunMode(t *testing.T) {
	for _, tc := range []struct {
		name             string
		isLiveOrUpcoming bool
		want             string
	}{
		{"live run", true, "live"},
		{"replay run", false, "replay"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "chat.json")
			cd := NewChatDownloader(ChatDownloaderOptions{
				VideoID: "vidSave", OutputFile: out, InitialContinuation: "tok", ApiKey: "k",
				IsLiveOrUpcoming: tc.isLiveOrUpcoming, IsReplay: !tc.isLiveOrUpcoming,
			})
			cd.saveResume()

			state, ok := readSidecar(t, out+".resume.json")
			if !ok {
				t.Fatal("saveResume wrote no sidecar")
			}
			if state.Mode != tc.want {
				t.Errorf("sidecar mode = %q, want %q", state.Mode, tc.want)
			}
		})
	}
}

// The tag has to survive a REAL run, not just a direct saveResume call: a
// live/upcoming run that exits on stale-continuation exhaustion keeps and
// refreshes its sidecar (the completion rule), and that is exactly the
// sidecar a later replay run would find.
func TestLiveRunLeavesLiveTaggedSidecar(t *testing.T) {
	out := filepath.Join(t.TempDir(), "chat.json")
	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID: "vidLiveTag", OutputFile: out, InitialContinuation: "tok0", ApiKey: "k",
		IsLiveOrUpcoming: true, StreamStartTime: "2026-06-11T10:00:00Z",
	})
	cd.testRecoveryOverride = func(context.Context) bool { return false } // stale exit keeps the sidecar
	startWithScript(t, cd, chatResponseWithIDs([]string{"m1"}, ""))

	state, ok := readSidecar(t, out+".resume.json")
	if !ok {
		t.Fatal("stale exit cleared the sidecar")
	}
	if state.Mode != "live" {
		t.Errorf("sidecar mode = %q, want %q", state.Mode, "live")
	}
}

// --- helpers -------------------------------------------------------------

// recordingChatLogger captures the downloader's diagnostics AND the level each
// was emitted at. It implements the full CLAUDE.md logger shape because
// ChatDownloader.Logger is that shape — the worker assigns its own logger to
// it at both construction sites, so the fake has to match what production
// hands over.
type recordingChatLogger struct {
	mu     sync.Mutex
	lines  *[]string
	levels []string // parallel to *lines
}

func (l *recordingChatLogger) record(level, msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	*l.lines = append(*l.lines, msg)
	l.levels = append(l.levels, level)
}

func (l *recordingChatLogger) Debug(msg string, _ ...any) { l.record("debug", msg) }
func (l *recordingChatLogger) Info(msg string, _ ...any)  { l.record("info", msg) }
func (l *recordingChatLogger) Warn(msg string, _ ...any)  { l.record("warn", msg) }
func (l *recordingChatLogger) Error(msg string, _ ...any) { l.record("error", msg) }

// loggedAt reports whether msg was emitted at exactly that level.
func (l *recordingChatLogger) loggedAt(level, msg string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, m := range *l.lines {
		if m == msg && l.levels[i] == level {
			return true
		}
	}
	return false
}

func containsLine(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}
