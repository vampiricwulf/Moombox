# Player Chat Sync, Niconico Overlay and Before/After-Video Chat — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make chat replay time-correct on every job type, bring the web player's niconico overlay up to flying-comment engine norms (no overlap, no silent loss, size/rate/visibility aware), present chat from before and after the recorded video deliberately, and fix the two broken player features (third-party emote CSP, segment indicator layout).

**Architecture:** (1) The Go producers write single-epoch, signed chat offsets and the CSP admits the emote CDNs, so the browser receives data it can place. (2) Two new pure ES modules — `web/public/modules/chat-timeline.js` (bias, normalisation, pre/post partition, cursor search) and `web/public/modules/nico-lanes.js` (media-time two-edge lane allocator) — hold all the math and are covered by `node:test`; `player.js` becomes a thin DOM adapter over them. (3) The sidebar gains explicit pre-show and post-end states with divider rows and header counts; the overlay never animates the backlog or the post-end tail.

**Tech Stack:** Go 1.26 (chi/v5, modernc sqlite, no CGo), vanilla JS ES modules, Shoelace 2.16 (CDN), Web Animations API, CSS container queries, `node:test` (Node 20+; the dev box runs v25).

**Spec:** `reports/player-review-2026-09-03.md` (gitignored) — the consolidated review; the three detailed reports are its Appendices A (timeline), B (nico) and C (ui). Finding IDs below (T-Fn, N-Fn, U-Hn/Mn/Ln) refer to those.

## Global Constraints

- Line references are to `main` @ `b8b1f0a`; re-locate by content if the file moved.
- No CGo; pure-Go dependencies only. Logger stays the anonymous 4-method interface — never extract a named one.
- Subscribed job fields go through `db.UpdateJobFields(jobID, map[string]any{...})`; `resume_position`/`chat_offset` stay on the silent path (`updateSingleColumnSilent`). No DB durability/cadence changes (owner ruling 2026-07-03).
- All REST routes under `/api/` (no version); frontend fetch calls must match route registration.
- Every goroutine has an inline `defer func(){ if r := recover(); ... }()`; HTTP handlers rely on `RecoveryMiddleware`.
- Web assets are `go:embed`ded — after any change under `web/public/` run `go build -o moombox.exe ./cmd/moombox` before testing in a browser.
- JS tests: `node --test web/tests/*.test.mjs`; only pure modules (no DOM/WebSocket/fetch) are tested there. Tests import from `../public/modules/<name>.js`.
- Run ONE `go test ./...` at a time on this machine (memory rule 2026-09-03). Use `go test ./internal/<pkg>/...` per task.
- Progress/UI updates stay near-real-time; make updates cheaper, never rarer.
- Never `taskkill /IM`; never `rm -rf` under %TEMP%; never force-update tags.
- Commit after every task: `git add <files> && git commit -m "<type>(<scope>): <summary>"` with the project's standard `Co-Authored-By` trailer. Work on a feature branch/worktree, never directly on `main`.
- Priorities: Correctness > Reliability > Efficiency > Simple UX > Polish.

## Owner decisions (defaults chosen; override before the phase that uses them)

| # | Decision | Default used by this plan | Used in |
|---|----------|---------------------------|---------|
| D1 | Pre-video overlay seed | Spawn only messages from the last `NICO_MAX_LATENESS_MS` (2000 ms) at t=0 or after a seek; older backlog is sidebar-only | Task 13 |
| D2 | Full-lanes policy | Defer FIFO up to 2000 ms, then drop and count; show a transient "+N not shown" pill; no overlap | Task 13 |
| D3 | Rows/font | Font scales with overlay height (`clamp(0.9rem, 4.5cqh, 2.4rem)`), rows = floor(height / measured line box) | Task 14 |
| D4 | Motion model | Keep constant 8 s traverse (niconico model) with the two-edge lane rule | Tasks 12–13 |
| D5 | Multi-part Twitch chat | New `GET /api/jobs/{id}/segments/{index}/chat` + client-side merge shifted by each part's start offset | Tasks 8, 11 |
| D6 | Reduced motion | Overlay defaults OFF under `prefers-reduced-motion: reduce` (user can still switch it on); remove the CSS static-stack fallback | Task 15 |
| D7 | DOM test harness | Optional; adds `jsdom` as the first web dev dependency | Task 24 |

## Verification gates (run at the end of each phase)

```bash
go build ./... && go vet ./...
go test ./internal/chat/... ./internal/twitch/... ./internal/web/... ./internal/worker/... ./internal/database/...
node --test web/tests/*.test.mjs
go build -o moombox.exe ./cmd/moombox     # re-embed web assets before browser checks
```
Browser checks (needs real archives): a multi-part Twitch job; a YouTube job that started late; 2× playback; a 10 s pause; a background tab for 5 min; fullscreen mid-flight; a 375 px-wide viewport; `prefers-reduced-motion` on.

---

# Phase 1 — Data fidelity (Go)

### Task 1: Admit the third-party emote CDNs in the CSP (U-H1)

**Files:**
- Modify: `internal/web/middleware.go:96`
- Test: `internal/web/middleware_csp_test.go` (create)

**Interfaces:**
- Consumes: `SecurityHeaders(next http.Handler) http.Handler` (`middleware.go:62`).
- Produces: nothing new; `img-src` gains `https://cdn.betterttv.net https://cdn.7tv.app https://cdn.frankerfacez.com`.

- [ ] **Step 1: Write the failing test**

```go
package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The chat replay renders BTTV / 7TV / FFZ emotes as <img> from their CDNs
// (internal/twitch/emotes.go). A CSP img-src that omits a host makes the
// browser refuse the image silently and the player falls back to the emote
// code as text — the whole emote pipeline looked dead in the field.
func TestSecurityHeadersCSPAdmitsEmoteCDNs(t *testing.T) {
	h := SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	imgSrc := ""
	for _, d := range strings.Split(rec.Header().Get("Content-Security-Policy"), ";") {
		if d = strings.TrimSpace(d); strings.HasPrefix(d, "img-src ") {
			imgSrc = d
		}
	}
	if imgSrc == "" {
		t.Fatal("CSP has no img-src directive")
	}
	for _, host := range []string{
		"https://cdn.betterttv.net", "https://cdn.7tv.app", "https://cdn.frankerfacez.com",
		"https://*.jtvnw.net", "https://yt3.ggpht.com", "https://i.ytimg.com",
	} {
		if !strings.Contains(imgSrc, " "+host) {
			t.Errorf("img-src lacks %s: %q", host, imgSrc)
		}
	}
}
```

- [ ] **Step 2: Run it — expect FAIL** for the three emote hosts.

Run: `go test ./internal/web/ -run TestSecurityHeadersCSPAdmitsEmoteCDNs -v`

- [ ] **Step 3: Add the hosts** at `internal/web/middleware.go:96`:

```go
"img-src 'self' data: https://i.ytimg.com https://yt3.ggpht.com https://*.jtvnw.net https://*.ttvnw.net https://cdn.betterttv.net https://cdn.7tv.app https://cdn.frankerfacez.com https://cdn.jsdelivr.net https://fonts.gstatic.com; "+
```

- [ ] **Step 4: Run the test — expect PASS.** Also `go test ./internal/web/...`.
- [ ] **Step 5: Update docs**: `docs/spec/security.md` CSP paragraph — add the three hosts with the reason (emote `<img>` in the player).
- [ ] **Step 6: Commit** — `fix(web): admit BTTV/7TV/FFZ emote CDNs in CSP img-src`.

---

### Task 2: Twitch live chat keeps signed (negative) pre-recording offsets (N-F2, Twitch half)

**Files:**
- Modify: `internal/twitch/chat.go:806-812`, `internal/twitch/types.go:57` (doc comment), `docs/spec/platform-services.md:543`
- Test: `internal/twitch/chat_offset_sign_test.go` (create)

**Interfaces:**
- Consumes: `newTestChatDownloader`, `readChatData`, `cd.addMessage`, `cd.flush`, `cd.SetRecordingStartTime` (all exist in `chat_roll_test.go` / `chat.go`).
- Produces: `TwitchChatMessage.OffsetMs` may be negative (message sent before the part's recording start).

- [ ] **Step 1: Write the failing test**

```go
package twitch

import (
	"path/filepath"
	"testing"
	"time"
)

// A message that reached IRC before the recording base was sent BEFORE the
// video starts. Clamping it to 0 piled every such message onto 0:00 and made
// them indistinguishable from real 0:00 chat; the player renders negative
// offsets ("-1:30") and needs the sign to place them as pre-show.
func TestAddMessageKeepsNegativeOffsetBeforeRecordingStart(t *testing.T) {
	out := filepath.Join(t.TempDir(), "chat.json")
	cd := newTestChatDownloader(t, out)
	cd.SetRecordingStartTime("2026-06-11T10:00:00Z")
	base := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC).UnixMilli()

	cd.addMessage(&TwitchChatMessage{ID: "early", AuthorName: "a", Message: "hi", TimestampMs: base - 90_000})
	cd.addMessage(&TwitchChatMessage{ID: "late", AuthorName: "b", Message: "yo", TimestampMs: base + 5_000})
	cd.flush()

	d := readChatData(t, out)
	want := map[string]int64{"early": -90_000, "late": 5_000}
	for _, m := range d.Messages {
		if m.OffsetMs != want[m.ID] {
			t.Errorf("%s offsetMs = %d, want %d", m.ID, m.OffsetMs, want[m.ID])
		}
	}
}
```

- [ ] **Step 2: Run — expect FAIL** (`early offsetMs = 0, want -90000`).

Run: `go test ./internal/twitch/ -run TestAddMessageKeepsNegativeOffsetBeforeRecordingStart -v`

- [ ] **Step 3: Implement** — in `internal/twitch/chat.go` replace

```go
	if baseMs > 0 {
		msg.OffsetMs = max(msg.TimestampMs-baseMs, 0)
	}
```
with
```go
	if baseMs > 0 {
		// Signed on purpose: a message that arrived before this part's
		// recording base was sent BEFORE the video starts. The player renders
		// negative offsets as pre-show chat ("-1:30"); clamping to 0 used to
		// pile them onto 0:00 (review 2026-09-03, N-F2).
		msg.OffsetMs = msg.TimestampMs - baseMs
	}
```
Update the `OffsetMs` doc comment in `types.go:57`: `// OffsetMs is the SIGNED ms offset from the part's recording start (negative = before recording began).`

- [ ] **Step 4: Run — expect PASS.** Then `go test ./internal/twitch/...` (TestRollFile uses `TimestampMs: 1..3` against a 2026 base and asserts no offsets — must still pass; if any test asserted the clamp, update it to the signed value).
- [ ] **Step 5: Docs** — `docs/spec/platform-services.md:543`: "`OffsetMs` computed as `tmiSentTs - baseMs` (signed; negative before the recording base)".
- [ ] **Step 6: Commit** — `fix(twitch): keep signed chat offsets for pre-recording messages`.

---

### Task 3: YouTube replay recovers negative pre-stream offsets from `timestampText` (N-F2, YouTube half)

**Files:**
- Modify: `internal/chat/api.go:429-436` (parseAction tail), add helper next to `extractReplayOffset`
- Test: `internal/chat/api_test.go` (append)

**Interfaces:**
- Consumes: `NewChatAPI(apiKey, visitorData string, cookieHeader func() string) *ChatAPI`, `(*ChatAPI).parseAction(map[string]any) *ChatMessage`, `ChatMessage.TimestampText` (set from `renderer.timestampText.simpleText` at `api.go:515-517`).
- Produces: `parseNegativeTimestampText(s string) (ms int64, ok bool)`.

- [ ] **Step 1: Write the failing tests** (append to `api_test.go`):

```go
func TestParseNegativeTimestampText(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"-0:05", -5_000, true},
		{"-1:23", -83_000, true},
		{"-1:02:03", -3_723_000, true},
		{"0:05", 0, false},   // not negative — not our business
		{"-5", 0, false},     // no colon — not a relative time
		{"-x:y", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := parseNegativeTimestampText(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("parseNegativeTimestampText(%q) = (%d,%v), want (%d,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func replayAction(offset any, timestampText string) map[string]any {
	renderer := map[string]any{
		"id":            "pre1",
		"timestampUsec": "1700000000000000",
		"message":       map[string]any{"runs": []any{map[string]any{"text": "hi"}}},
		"authorName":    map[string]any{"simpleText": "U"},
	}
	if timestampText != "" {
		renderer["timestampText"] = map[string]any{"simpleText": timestampText}
	}
	return map[string]any{
		"replayChatItemAction": map[string]any{
			"videoOffsetTimeMsec": offset,
			"actions": []any{map[string]any{
				"addChatItemAction": map[string]any{
					"item": map[string]any{"liveChatTextMessageRenderer": renderer},
				},
			}},
		},
	}
}

// YouTube reports videoOffsetTimeMsec = 0 for every message sent before the
// stream started and keeps the real relative time only in timestampText
// ("-2:30"). Recover it so waiting-room chat is not piled onto 0:00.
func TestParseActionReplayZeroOffsetRecoversNegativeFromTimestampText(t *testing.T) {
	api := NewChatAPI("k", "", nil)
	msg := api.parseAction(replayAction("0", "-2:30"))
	if msg == nil {
		t.Fatal("parseAction returned nil")
	}
	if !msg.HasOffset || msg.OffsetMs != -150_000 {
		t.Errorf("OffsetMs = %d (hasOffset %v), want -150000", msg.OffsetMs, msg.HasOffset)
	}
}

func TestParseActionReplayZeroOffsetWithoutMinusStaysZero(t *testing.T) {
	api := NewChatAPI("k", "", nil)
	msg := api.parseAction(replayAction("0", "0:00"))
	if msg == nil || !msg.HasOffset || msg.OffsetMs != 0 {
		t.Fatalf("want offset 0 with hasOffset, got %+v", msg)
	}
}

func TestParseActionReplayPositiveOffsetIgnoresTimestampText(t *testing.T) {
	api := NewChatAPI("k", "", nil)
	msg := api.parseAction(replayAction("12345", "-2:30"))
	if msg == nil || msg.OffsetMs != 12345 {
		t.Fatalf("a non-zero replay offset is authoritative; got %+v", msg)
	}
}
```

- [ ] **Step 2: Run — expect compile FAIL** (`parseNegativeTimestampText` undefined).

Run: `go test ./internal/chat/ -run 'TestParseNegativeTimestampText|TestParseActionReplay' -v`

- [ ] **Step 3: Implement** — in `internal/chat/api.go`, after `extractReplayOffset`:

```go
// parseNegativeTimestampText parses YouTube's relative replay timestamp text
// for a PRE-STREAM message ("-1:23", "-1:02:03") into signed milliseconds.
// YouTube zeroes videoOffsetTimeMsec for messages sent before the stream
// started and keeps the real time only here. Returns (0, false) for anything
// that is not a leading-minus M:SS / H:MM:SS.
func parseNegativeTimestampText(s string) (int64, bool) {
	if !strings.HasPrefix(s, "-") {
		return 0, false
	}
	parts := strings.Split(s[1:], ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	var total int64
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return 0, false
		}
		total = total*60 + int64(n)
	}
	return -total * 1000, true
}
```
and in `parseAction` replace the `if hasReplayOffset {` block with:
```go
	if hasReplayOffset {
		if replayOffsetMs == 0 {
			// Pre-stream replay messages arrive with offset 0; the relative
			// time survives only in timestampText (review 2026-09-03, N-F2).
			if neg, ok := parseNegativeTimestampText(msg.TimestampText); ok {
				replayOffsetMs = neg
			}
		}
		// Negative offsets are legitimate for pre-stream waiting-room chat.
		msg.OffsetMs = replayOffsetMs
		msg.HasOffset = true
	}
```
(`strings` and `strconv` are already imported.)

- [ ] **Step 4: Run — expect PASS**; then `go test ./internal/chat/...`.
- [ ] **Step 5: Docs** — `internal/chat/types.go:5-10` comment: mention replay recovery; `docs/spec/platform-services.md` YouTube chat section: one sentence on the recovery.
- [ ] **Step 6: Commit** — `fix(chat): recover negative pre-stream offsets in YouTube replay`.

---

### Task 4: A YouTube chat file keeps ONE epoch across restarts and adoption (T-F2)

**Files:**
- Modify: `internal/chat/types.go:78-84` (`ChatResumeState`), `internal/chat/downloader.go` (`Start` ~340-366, `adoptExistingChatFile` 1038-1072, `readExistingMessages` ~985, `saveResume` 1119-1150, `writeFullChatFile` ~970)
- Test: `internal/chat/downloader_epoch_test.go` (create)

**Interfaces:**
- Consumes: harness in `downloader_history_test.go` — `startWithScript(t, cd, resps...)`, `chatResponseWithIDs(ids, next)`, `readSidecar(t, path)`, `readChatFileHeader(t, path)`, `cd.testRecoveryOverride`.
- Produces: `ChatResumeState.StreamStartMs int64` (json `streamStartMs,omitempty`); `(cd *ChatDownloader) readExistingChatData(path string) (*ChatData, error)`; `(cd *ChatDownloader) epochRFC3339() string`.

- [ ] **Step 1: Write the failing tests**

```go
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
```

- [ ] **Step 2: Run — expect compile FAIL** (`StreamStartMs` undefined).

Run: `go test ./internal/chat/ -run 'TestResumeKeepsFileEpochOverNewOptions|TestAdoptionKeepsFileEpoch' -v`

- [ ] **Step 3: Implement**

`types.go` — add to `ChatResumeState`:
```go
	// StreamStartMs is the epoch (ms) every offsetMs in the chat file was
	// computed against. A resumed or adopted run keeps it even when its own
	// options carry a newer start time — one file, one epoch.
	StreamStartMs int64 `json:"streamStartMs,omitempty"`
```

`downloader.go`:
1. `saveResume`: add `StreamStartMs: cd.streamStartMs,` to the `state := ChatResumeState{...}` literal.
2. In `Start`, inside `if err == nil && state != nil && state.VideoID == cd.opts.VideoID {` (after the `cd.messageCount = state.MessageCount` line):
```go
		if state.StreamStartMs > 0 && state.StreamStartMs != cd.streamStartMs {
			cd.logDebug("chat: keeping the file's epoch over the run's start time",
				"videoID", cd.opts.VideoID, "fileEpochMs", state.StreamStartMs, "optsEpochMs", cd.streamStartMs)
			cd.streamStartMs = state.StreamStartMs
		}
```
3. Replace `readExistingMessages` with a data-returning reader and keep the old name as a wrapper:
```go
func (cd *ChatDownloader) readExistingChatData(path string) (*ChatData, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var chatData ChatData
	if err := json.Unmarshal(data, &chatData); err != nil {
		return nil, err
	}
	return &chatData, nil
}

func (cd *ChatDownloader) readExistingMessages(path string) ([]ChatMessage, error) {
	d, err := cd.readExistingChatData(path)
	if err != nil {
		return nil, err
	}
	return d.Messages, nil
}
```
4. `adoptExistingChatFile`: call `readExistingChatData` and, before the `cd.mu.Lock()`, adopt the epoch:
```go
	existingData, err := cd.readExistingChatData(outputFile)
	if err != nil { /* unchanged error handling */ }
	existing := existingData.Messages
	if len(existing) == 0 {
		return 0
	}
	if existingData.StreamStartTime != "" {
		if t, perr := time.Parse(time.RFC3339, existingData.StreamStartTime); perr == nil && t.UnixMilli() != cd.streamStartMs {
			cd.logDebug("chat: adopting the file's epoch", "videoID", cd.opts.VideoID, "fileEpoch", existingData.StreamStartTime)
			cd.streamStartMs = t.UnixMilli()
		}
	}
```
5. Header always reflects the epoch in use:
```go
// epochRFC3339 renders the epoch offsets are computed against — the file's
// epoch once one is adopted/resumed, else the options' start time.
func (cd *ChatDownloader) epochRFC3339() string {
	if cd.streamStartMs > 0 {
		return time.UnixMilli(cd.streamStartMs).UTC().Format(time.RFC3339)
	}
	return cd.opts.StreamStartTime
}
```
and in `writeFullChatFile` use `StreamStartTime: cd.epochRFC3339(),`.

- [ ] **Step 4: Run — expect PASS**; then `go test ./internal/chat/...` (the adoption/history tests must still pass).
- [ ] **Step 5: Docs** — `docs/spec/data-and-storage.md` §Chat resume state: add `streamStartMs`; `docs/spec/platform-services.md` completion/adoption paragraph: "the epoch travels with the sidecar and is adopted from the file header".
- [ ] **Step 6: Commit** — `fix(chat): keep one offset epoch per chat file across resume and adoption`.

---

### Task 5: VOD/post-live classification refreshes `stream_start_time` to the actual start (T-F4)

**Files:**
- Modify: `internal/worker/stream_processor.go:287-297`
- Test: `internal/worker/stream_processor_vod_start_test.go` (create)

**Interfaces:**
- Produces: `vodStatusUpdates(job *database.Job, info *youtube.VideoInfo) map[string]any`.

- [ ] **Step 1: Write the failing test**

```go
package worker

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// A job created Upcoming holds the SCHEDULED start. When it is later processed
// as a VOD (live capture never ran), the replay chat offsets count from the
// ACTUAL start that YouTube now reports in ScheduledStartTime — the row must
// follow or the player biases the chat late by the late-start delta.
func TestVodStatusUpdatesRefreshesStaleStart(t *testing.T) {
	job := &database.Job{ID: "j", StreamStartTime: "2026-06-11T10:00:00Z"}
	info := &youtube.VideoInfo{ScheduledStartTime: "2026-06-11T10:12:00Z"}
	u := vodStatusUpdates(job, info)
	if u["status"] != database.StatusDownloading || u["is_vod"] != true {
		t.Errorf("status/is_vod = %v/%v", u["status"], u["is_vod"])
	}
	if u["stream_start_time"] != "2026-06-11T10:12:00Z" {
		t.Errorf("stream_start_time = %v, want the actual start", u["stream_start_time"])
	}
}

func TestVodStatusUpdatesLeavesMatchingOrUnknownStart(t *testing.T) {
	same := vodStatusUpdates(&database.Job{StreamStartTime: "2026-06-11T10:12:00Z"},
		&youtube.VideoInfo{ScheduledStartTime: "2026-06-11T10:12:00Z"})
	if _, ok := same["stream_start_time"]; ok {
		t.Error("equal times must not write stream_start_time")
	}
	none := vodStatusUpdates(&database.Job{StreamStartTime: "2026-06-11T10:12:00Z"}, &youtube.VideoInfo{})
	if _, ok := none["stream_start_time"]; ok {
		t.Error("an empty ScheduledStartTime must not clear the row")
	}
}
```

- [ ] **Step 2: Run — expect compile FAIL.**

Run: `go test ./internal/worker/ -run TestVodStatusUpdates -v`

- [ ] **Step 3: Implement** — in `stream_processor.go` add near `updateJobMetadata`:

```go
// vodStatusUpdates builds the row update for a stream classified VOD or
// post-live. A job created Upcoming still carries the SCHEDULED start; for a
// finished stream YouTube's ScheduledStartTime is the actual start the replay
// chat offsets count from, so it is refreshed here (review 2026-09-03, T-F4).
func vodStatusUpdates(job *database.Job, info *youtube.VideoInfo) map[string]any {
	updates := map[string]any{
		"status": database.StatusDownloading,
		"is_vod": true,
	}
	if info != nil && info.ScheduledStartTime != "" && info.ScheduledStartTime != job.StreamStartTime {
		updates["stream_start_time"] = info.ScheduledStartTime
	}
	return updates
}
```
and in the `case youtube.StreamVOD, youtube.StreamPostLive:` branch replace the `sp.db.UpdateJobFields(job.ID, map[string]any{"status": ..., "is_vod": true})` call with `sp.db.UpdateJobFields(job.ID, vodStatusUpdates(job, info))`.

- [ ] **Step 4: Run — expect PASS**; `go test ./internal/worker/...`.
- [ ] **Step 5: Commit** — `fix(worker): VOD classification refreshes stream_start_time to the actual start`.

---

### Task 6: Silent watch updates report "job not found" (U-M9)

**Files:**
- Modify: `internal/database/database.go:453-482`, `internal/web/routes/watch.go:45, 63, 149, 156`
- Test: `internal/web/routes/watch_test.go` (append)

**Interfaces:**
- Produces: `updateSingleColumnSilent(...) bool`, `UpdateResumePosition(jobID string, seconds float64) bool`, `UpdateChatOffset(jobID string, offset float64) bool` (true when a row was updated). The only callers are the four route lines above.

- [ ] **Step 1: Write the failing tests** (append to `watch_test.go`; the fixture `newWatchFixture`/`addJob` exists):

```go
func TestResumePositionPut404OnUnknown(t *testing.T) {
	f := newWatchFixture(t)
	body, _ := json.Marshal(map[string]any{"position": 12.5})
	req := httptest.NewRequest("PUT", "/api/jobs/no-such/resume-position", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown job: want 404, got %d", rec.Code)
	}
}

func TestChatOffsetPutAndDelete404OnUnknown(t *testing.T) {
	f := newWatchFixture(t)
	body, _ := json.Marshal(map[string]any{"chatOffset": -1.5})
	for _, tc := range []struct{ method string; body []byte }{{"PUT", body}, {"DELETE", nil}} {
		req := httptest.NewRequest(tc.method, "/api/jobs/no-such/chat-offset", bytes.NewReader(tc.body))
		rec := httptest.NewRecorder()
		f.router.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s unknown job: want 404, got %d", tc.method, rec.Code)
		}
	}
}
```

- [ ] **Step 2: Run — expect FAIL** (204s).

Run: `go test ./internal/web/routes/ -run 'TestResumePositionPut404OnUnknown|TestChatOffsetPutAndDelete404OnUnknown' -v`

- [ ] **Step 3: Implement**

`database.go`:
```go
func (db *Database) updateSingleColumnSilent(jobID, column string, value any, opName string) bool {
	if _, ok := silentColumns[column]; !ok {
		if db.logger != nil {
			db.logger.Error(opName+": disallowed column", "column", column)
		}
		return false
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	res, err := db.db.ExecContext(db.getCtx(),
		"UPDATE jobs SET "+column+" = ? WHERE id = ?", value, jobID)
	if err != nil {
		if db.logger != nil {
			db.logger.Error(opName+" failed", "jobID", jobID, "err", err)
		}
		return false
	}
	n, err := res.RowsAffected()
	if err != nil {
		return true // the driver cannot say; assume the row existed
	}
	return n > 0
}

func (db *Database) UpdateResumePosition(jobID string, seconds float64) bool {
	return db.updateSingleColumnSilent(jobID, "resume_position", seconds, "UpdateResumePosition")
}

func (db *Database) UpdateChatOffset(jobID string, offset float64) bool {
	return db.updateSingleColumnSilent(jobID, "chat_offset", offset, "UpdateChatOffset")
}
```
`watch.go` — PUT resume-position (`:45`): `if !db.UpdateResumePosition(jobID, body.Position) { jsonError(rw, "job not found", http.StatusNotFound); return }`; POST twin (`:63`): `if !db.UpdateResumePosition(...) { rw.WriteHeader(http.StatusNotFound); return }`; chat-offset PUT (`:149`) and DELETE (`:156`): `if !db.UpdateChatOffset(...) { jsonError(rw, "job not found", http.StatusNotFound); return }`.

- [ ] **Step 4: Run — expect PASS**; `go test ./internal/database/... ./internal/web/routes/...`.
- [ ] **Step 5: Commit** — `fix(web): watch/offset writes return 404 for unknown jobs`.

---

### Task 7: Revalidating cache headers for media and chat; `/chat` supports 304 (T-F5, T-F6)

**Files:**
- Modify: `internal/web/routes/jobs.go:332-336` (video), `:524-528` (segment video), `:594-605` (chat)
- Test: `internal/web/routes/jobs_test.go` (append)

**Interfaces:**
- Consumes: `newJobsFixture`, `f.addJob`, `doRequest`.
- Produces: `Cache-Control: private, no-cache` on `/video`, `/segments/{i}/video`, `/chat`; `/chat` sends `Last-Modified` and answers `If-Modified-Since` with 304.

- [ ] **Step 1: Write the failing tests**

```go
// Media URLs are stable but their CONTENT changes (retry/reinit, incomplete-tail
// resume, part merge). `immutable` let a browser keep serving the old file for
// a year; Last-Modified revalidation costs one conditional request per play.
func TestJobVideoIsRevalidatedNotImmutable(t *testing.T) {
	f := newJobsFixture(t)
	if err := os.WriteFile(filepath.Join(f.outputDir, "cc.mp4"), []byte("mp4"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.addJob(t, "yt_cc", func(j *database.Job) { j.Filename = "cc.mp4" })
	rec := doRequest(t, f.router, "GET", "/api/jobs/yt_cc/video", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	cc := rec.Header().Get("Cache-Control")
	if strings.Contains(cc, "immutable") || !strings.Contains(cc, "no-cache") {
		t.Errorf("Cache-Control = %q, want a revalidating policy", cc)
	}
	if rec.Header().Get("Last-Modified") == "" {
		t.Error("Last-Modified missing — the browser cannot revalidate")
	}
}

// The chat file is 50–100 MB on a long VOD and was re-read and re-gzipped on
// every selection. A conditional GET must get a body-less 304.
func TestJobChatAnswers304WhenUnmodified(t *testing.T) {
	f := newJobsFixture(t)
	chatPath := filepath.Join(f.outputDir, "chat.json")
	if err := os.WriteFile(chatPath, []byte(`{"messages":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	f.addJob(t, "yt_chat304", func(j *database.Job) { j.ChatFilename = "chat.json" })

	first := doRequest(t, f.router, "GET", "/api/jobs/yt_chat304/chat", nil)
	if first.Code != http.StatusOK {
		t.Fatalf("first GET: want 200, got %d", first.Code)
	}
	lm := first.Header().Get("Last-Modified")
	if lm == "" {
		t.Fatal("first GET has no Last-Modified")
	}
	req := httptest.NewRequest("GET", "/api/jobs/yt_chat304/chat", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("If-Modified-Since", lm)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Errorf("conditional GET: want 304, got %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("304 must have no body, got %d bytes", rec.Body.Len())
	}
}
```
(Add `"strings"` to the test imports if missing.)

- [ ] **Step 2: Run — expect FAIL.**

Run: `go test ./internal/web/routes/ -run 'TestJobVideoIsRevalidatedNotImmutable|TestJobChatAnswers304WhenUnmodified' -v`

- [ ] **Step 3: Implement**

Both video handlers: replace the finished/immutable branch with a single line
```go
		// Revalidate, never immutable: retry/reinit, incomplete-tail resume and
		// part merges rewrite the file behind the same URL. ServeFile emits
		// Last-Modified and honours If-Modified-Since / Range.
		rw.Header().Set("Cache-Control", "private, no-cache")
```
`/chat` handler: replace the tail (`data, err := os.ReadFile(chatPath)` … `rw.Write(data)`) with
```go
		f, err := os.Open(chatPath)
		if err != nil {
			jsonError(rw, "Chat file is corrupt or unreadable", http.StatusUnprocessableEntity)
			return
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			jsonError(rw, "Chat file is corrupt or unreadable", http.StatusUnprocessableEntity)
			return
		}
		data, err := io.ReadAll(f)
		if err != nil || !json.Valid(data) {
			jsonError(rw, "Chat file is corrupt or unreadable", http.StatusUnprocessableEntity)
			return
		}
		rw.Header().Set("Content-Type", "application/json")
		rw.Header().Set("Cache-Control", "private, no-cache")
		// ServeContent adds Last-Modified and answers If-Modified-Since with a
		// body-less 304 — the gzip middleware then has nothing to compress.
		http.ServeContent(rw, req, "chat.json", fi.ModTime(), bytes.NewReader(data))
```
(add `bytes`/`io` imports if absent.)

- [ ] **Step 4: Run — expect PASS**; `go test ./internal/web/routes/...` (existing chat tests still pass: body identical, 422 paths unchanged).
- [ ] **Step 5: Docs** — `docs/spec/user-interfaces.md` API table: `/video`, `/segments/{i}/video`, `/chat` "private, no-cache + Last-Modified"; fix line 511's claim that `/api/jobs/{id}` is immutably cached (it is `no-cache`, `jobs.go:239-244`) (T-F15).
- [ ] **Step 6: Commit** — `fix(web): revalidating cache policy for media and chat; 304 for unchanged chat`.

---

### Task 8: Serve per-part chat files — `GET /api/jobs/{id}/segments/{index}/chat` (T-F1 server half, D5)

**Files:**
- Modify: `internal/web/routes/jobs.go` (register after the segment video route, ~`:531`)
- Test: `internal/web/routes/jobs_test.go` (append)

**Interfaces:**
- Consumes: `db.GetSegments(jobID)`, `database.Segment.ChatFile`, `validatePathTraversal(filePath, outputDir)`, `f.db.AddSegment(&database.Segment{...})`.
- Produces: route returning the raw part chat JSON (same 404/403/422 semantics as `/chat`).

- [ ] **Step 1: Write the failing tests**

```go
func TestSegmentChat404WhenPartHasNoChat(t *testing.T) {
	f := newJobsFixture(t)
	f.addJob(t, "tw_parts0", func(j *database.Job) { j.Platform = "twitch" })
	if err := f.db.AddSegment(&database.Segment{JobID: "tw_parts0", SegmentIndex: 0, Quality: "1080p", Filename: "p1.mp4",
		FilePath: filepath.Join(f.outputDir, "p1.mp4")}); err != nil {
		t.Fatal(err)
	}
	rec := doRequest(t, f.router, "GET", "/api/jobs/tw_parts0/segments/0/chat", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("part without chat: want 404, got %d", rec.Code)
	}
}

func TestSegmentChatServesPartFile(t *testing.T) {
	f := newJobsFixture(t)
	f.addJob(t, "tw_parts", func(j *database.Job) { j.Platform = "twitch" })
	chatPath := filepath.Join(f.outputDir, "p2.chat.json")
	body := []byte(`{"platform":"twitch","messages":[{"id":"a","offsetMs":5000}]}`)
	if err := os.WriteFile(chatPath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.db.AddSegment(&database.Segment{JobID: "tw_parts", SegmentIndex: 1, Quality: "1080p", Filename: "p2.mp4",
		FilePath: filepath.Join(f.outputDir, "p2.mp4"), ChatFile: chatPath}); err != nil {
		t.Fatal(err)
	}
	rec := doRequest(t, f.router, "GET", "/api/jobs/tw_parts/segments/1/chat", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != string(body) {
		t.Errorf("body mismatch: %s", rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
}

func TestSegmentChatRefusesPathOutsideOutputDir(t *testing.T) {
	f := newJobsFixture(t)
	f.addJob(t, "tw_escape", func(j *database.Job) { j.Platform = "twitch" })
	outside := filepath.Join(filepath.Dir(f.outputDir), "outside.chat.json")
	if err := os.WriteFile(outside, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.db.AddSegment(&database.Segment{JobID: "tw_escape", SegmentIndex: 0, Quality: "q", Filename: "x.mp4",
		FilePath: filepath.Join(f.outputDir, "x.mp4"), ChatFile: outside}); err != nil {
		t.Fatal(err)
	}
	rec := doRequest(t, f.router, "GET", "/api/jobs/tw_escape/segments/0/chat", nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("path outside output dir: want 403, got %d", rec.Code)
	}
}
```

- [ ] **Step 2: Run — expect FAIL** (404 from chi for the unregistered route on the 200 case).

Run: `go test ./internal/web/routes/ -run TestSegmentChat -v`

- [ ] **Step 3: Implement** — register after the segment video route:

```go
	// GET /api/jobs/:id/segments/:index/chat — serves one part's chat file.
	// Twitch live recordings roll the chat at every part boundary with offsets
	// rebased to that part's recording start; the player fetches each part and
	// shifts by the part's start offset on the global timeline (T-F1).
	r.Get("/api/jobs/{id}/segments/{index}/chat", func(rw http.ResponseWriter, req *http.Request) {
		jobID := chi.URLParam(req, "id")
		segIndex, err := strconv.Atoi(chi.URLParam(req, "index"))
		if err != nil {
			jsonError(rw, "invalid segment index", http.StatusBadRequest)
			return
		}
		job, err := db.GetJob(jobID)
		if err != nil || job == nil {
			jsonError(rw, "job not found", http.StatusNotFound)
			return
		}
		segments, err := db.GetSegments(jobID)
		if err != nil {
			jsonError(rw, "failed to get segments", http.StatusInternalServerError)
			return
		}
		var seg *database.Segment
		for i := range segments {
			if segments[i].SegmentIndex == segIndex {
				seg = &segments[i]
				break
			}
		}
		if seg == nil || seg.ChatFile == "" {
			jsonError(rw, "no chat for this segment", http.StatusNotFound)
			return
		}
		var cfgOutputDir string
		store.Read(func(c *config.MoomboxConfig) { cfgOutputDir = c.Paths.OutputDirectory })
		outputDir := job.OutputDirectory
		if outputDir == "" {
			outputDir = cfgOutputDir
		}
		if outputDir == "" {
			outputDir = "./output"
		}
		chatPath, err := filepath.Abs(seg.ChatFile)
		if err != nil {
			jsonError(rw, "invalid path", http.StatusBadRequest)
			return
		}
		resolved, ok := validatePathTraversal(chatPath, outputDir)
		if !ok {
			jsonError(rw, "access denied", http.StatusForbidden)
			return
		}
		f, err := os.Open(resolved)
		if err != nil {
			if os.IsNotExist(err) {
				jsonError(rw, "chat file not found", http.StatusNotFound)
				return
			}
			jsonError(rw, "Chat file is corrupt or unreadable", http.StatusUnprocessableEntity)
			return
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			jsonError(rw, "Chat file is corrupt or unreadable", http.StatusUnprocessableEntity)
			return
		}
		data, err := io.ReadAll(f)
		if err != nil || !json.Valid(data) {
			jsonError(rw, "Chat file is corrupt or unreadable", http.StatusUnprocessableEntity)
			return
		}
		rw.Header().Set("Content-Type", "application/json")
		rw.Header().Set("Cache-Control", "private, no-cache")
		http.ServeContent(rw, req, "chat.json", fi.ModTime(), bytes.NewReader(data))
	})
```

- [ ] **Step 4: Run — expect PASS**; `go test ./internal/web/routes/...`.
- [ ] **Step 5: Docs** — add the route to `docs/spec/user-interfaces.md` API table and to the moombox-api-routes skill's route list if it enumerates routes.
- [ ] **Step 6: Commit** — `feat(web): serve per-part chat files for multi-segment jobs`.

---

# Phase 2 — Timeline contract in the browser

### Task 9: `chat-timeline.js` — bias, normalisation, partition, cursor search; wire the bias (T-F1/T-F3 client half, T-F12)

**Files:**
- Create: `web/public/modules/chat-timeline.js`
- Modify: `web/public/modules/player.js:664-690` (bias block in `onPlayerJobSelect`), imports at `:4-5`
- Test: `web/tests/chat-timeline.test.mjs` (create)

**Interfaces:**
- Produces:
  - `normalizeOffsetMs(raw) → number` (finite number or 0; accepts json.Number strings)
  - `computeChatBiasMs({ platform, chatStreamStartTime, jobStreamStartTime }) → number` (ms to SUBTRACT from every offset)
  - `partitionChatByVideo(messages, totalDurationMs) → { preCount, firstLiveIndex, postCount, firstPostIndex }`
  - `indexAfter(messages, offsetMs) → number` (first index with `offsetMs > value`, binary search)
  - `mergePartChats(parts) → { platform, streamStartTime, emotes, messages }` where `parts = [{ startOffsetSec, data }]` (used by Task 11)

- [ ] **Step 1: Write the failing tests**

```js
// Tests for web/public/modules/chat-timeline.js
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  normalizeOffsetMs, computeChatBiasMs, partitionChatByVideo, indexAfter, mergePartChats,
} from "../public/modules/chat-timeline.js";

test("normalizeOffsetMs: numbers, json.Number strings, garbage", () => {
  assert.equal(normalizeOffsetMs(1500), 1500);
  assert.equal(normalizeOffsetMs(-90000), -90000);
  assert.equal(normalizeOffsetMs("123"), 123);   // imported chat: json.Number as string
  assert.equal(normalizeOffsetMs(undefined), 0);
  assert.equal(normalizeOffsetMs(null), 0);
  assert.equal(normalizeOffsetMs("abc"), 0);
  assert.equal(normalizeOffsetMs(NaN), 0);
});

test("computeChatBiasMs: twitch offsets are already video-relative → 0", () => {
  assert.equal(computeChatBiasMs({
    platform: "twitch",
    chatStreamStartTime: "2026-06-11T10:00:00Z",   // Twitch startedAt
    jobStreamStartTime: "2026-06-11T10:00:00Z",
  }), 0);
});

test("computeChatBiasMs: youtube = actual start − scheduled start (late start)", () => {
  assert.equal(computeChatBiasMs({
    platform: undefined,                              // YouTube files carry no platform field
    chatStreamStartTime: "2026-06-11T10:00:00Z",      // epoch the offsets count from
    jobStreamStartTime: "2026-06-11T10:12:00Z",       // actual start = video t=0
  }), 12 * 60 * 1000);
});

test("computeChatBiasMs: same epoch, missing or unparsable → 0", () => {
  assert.equal(computeChatBiasMs({ chatStreamStartTime: "2026-06-11T10:12:00Z", jobStreamStartTime: "2026-06-11T10:12:00Z" }), 0);
  assert.equal(computeChatBiasMs({ chatStreamStartTime: "", jobStreamStartTime: "2026-06-11T10:12:00Z" }), 0);
  assert.equal(computeChatBiasMs({ chatStreamStartTime: "2026-06-11T10:00:00Z", jobStreamStartTime: undefined }), 0);
  assert.equal(computeChatBiasMs({ chatStreamStartTime: "garbage", jobStreamStartTime: "2026-06-11T10:12:00Z" }), 0);
});

const msgs = (...offsets) => offsets.map((o, i) => ({ id: String(i), offsetMs: o }));

test("partitionChatByVideo: pre-show, in-video and post-end counts", () => {
  const p = partitionChatByVideo(msgs(-90000, -5000, 0, 1000, 5000, 61000, 65000), 60000);
  assert.deepEqual(p, { preCount: 2, firstLiveIndex: 2, postCount: 2, firstPostIndex: 5 });
});

test("partitionChatByVideo: no negatives, unknown duration", () => {
  assert.deepEqual(partitionChatByVideo(msgs(0, 1000), 0), { preCount: 0, firstLiveIndex: 0, postCount: 0, firstPostIndex: -1 });
  assert.deepEqual(partitionChatByVideo([], 60000), { preCount: 0, firstLiveIndex: -1, postCount: 0, firstPostIndex: -1 });
  assert.deepEqual(partitionChatByVideo(msgs(-3, -2), 60000), { preCount: 2, firstLiveIndex: -1, postCount: 0, firstPostIndex: -1 });
});

test("indexAfter: first index whose offset is strictly greater", () => {
  const m = msgs(0, 0, 1000, 1000, 5000);
  assert.equal(indexAfter(m, -1), 0);
  assert.equal(indexAfter(m, 0), 2);      // equal offsets are NOT after
  assert.equal(indexAfter(m, 999), 2);
  assert.equal(indexAfter(m, 1000), 4);
  assert.equal(indexAfter(m, 5000), 5);
  assert.equal(indexAfter([], 0), 0);
});

test("mergePartChats: shifts each part by its start offset and keeps first header", () => {
  const merged = mergePartChats([
    { startOffsetSec: 0,    data: { platform: "twitch", streamStartTime: "S", emotes: { bttv: [] }, messages: [{ id: "a", offsetMs: 5000 }] } },
    { startOffsetSec: 3600.5, data: { platform: "twitch", messages: [{ id: "b", offsetMs: "1000" }, { id: "c", offsetMs: -2000 }] } },
  ]);
  assert.equal(merged.platform, "twitch");
  assert.equal(merged.streamStartTime, "S");
  assert.deepEqual(merged.emotes, { bttv: [] });
  assert.deepEqual(merged.messages.map((m) => [m.id, m.offsetMs]), [["a", 5000], ["b", 3601500], ["c", 3598500]]);
});
```

- [ ] **Step 2: Run — expect FAIL** (module not found).

Run: `node --test web/tests/chat-timeline.test.mjs`

- [ ] **Step 3: Create the module**

```js
/**
 * Chat ↔ video timeline math. Pure — no DOM, no fetch — so it is covered by
 * web/tests/chat-timeline.test.mjs.
 *
 * Offset semantics (verified against the Go producers, review 2026-09-03):
 * - YouTube chat.json: offsetMs counts from chat.streamStartTime (the start
 *   the downloader was created with — the SCHEDULED time for early chat).
 *   The video begins at the ACTUAL start (job.streamStartTime; DASH backfills
 *   from sequence 0), so bias = actual − scheduled. Negative offsets are
 *   waiting-room chat.
 * - Twitch chat.json (platform:"twitch"): live IRC offsets count from the
 *   part's recording start and VOD offsets from the VOD start — both already
 *   video-relative. Bias is 0. Multi-part files are shifted per part.
 */

export function normalizeOffsetMs(raw) {
  const n = typeof raw === "number" ? raw : Number(raw);
  return Number.isFinite(n) ? n : 0;
}

export function computeChatBiasMs({ platform, chatStreamStartTime, jobStreamStartTime }) {
  if (platform === "twitch") return 0;
  const chatStart = Date.parse(chatStreamStartTime || "");
  const jobStart = Date.parse(jobStreamStartTime || "");
  if (!Number.isFinite(chatStart) || !Number.isFinite(jobStart)) return 0;
  return jobStart - chatStart;
}

/** First index whose offsetMs is strictly greater than `offsetMs` (messages sorted ascending). */
export function indexAfter(messages, offsetMs) {
  let lo = 0;
  let hi = messages.length;
  while (lo < hi) {
    const mid = (lo + hi) >>> 1;
    if (messages[mid].offsetMs <= offsetMs) lo = mid + 1;
    else hi = mid;
  }
  return lo;
}

/**
 * Split a sorted list into pre-show (offset < 0), in-video and post-end
 * (offset > totalDurationMs) regions. totalDurationMs <= 0 means unknown.
 */
export function partitionChatByVideo(messages, totalDurationMs) {
  const preCount = indexAfter(messages, -1);           // count of offsets <= -1 … i.e. < 0
  const firstLiveIndex = preCount < messages.length ? preCount : -1;
  let firstPostIndex = -1;
  if (totalDurationMs > 0) {
    const idx = indexAfter(messages, totalDurationMs);
    if (idx < messages.length) firstPostIndex = idx;
  }
  const postCount = firstPostIndex === -1 ? 0 : messages.length - firstPostIndex;
  return { preCount, firstLiveIndex, postCount, firstPostIndex };
}

/**
 * Merge per-part chat files onto the global timeline: each part's offsets are
 * part-relative, so add the part's start offset. Header fields come from the
 * first part that has them.
 * @param {Array<{startOffsetSec:number, data:object}>} parts in playback order
 */
export function mergePartChats(parts) {
  const merged = { platform: undefined, streamStartTime: undefined, emotes: undefined, messages: [] };
  for (const { startOffsetSec, data } of parts) {
    if (!data) continue;
    merged.platform ??= data.platform;
    merged.streamStartTime ??= data.streamStartTime;
    merged.emotes ??= data.emotes;
    const shiftMs = Math.round((startOffsetSec || 0) * 1000);
    for (const m of data.messages || []) {
      merged.messages.push({ ...m, offsetMs: normalizeOffsetMs(m.offsetMs) + shiftMs });
    }
  }
  return merged;
}
```
Note on `preCount`: offsets are integers in practice, but to be exact against fractional negatives use a direct scan instead: `let preCount = 0; while (preCount < messages.length && messages[preCount].offsetMs < 0) preCount++;` — use the scan (it is O(pre) and pre is usually small).

- [ ] **Step 4: Run — expect PASS.**
- [ ] **Step 5: Wire into player.js** — add `import { normalizeOffsetMs, computeChatBiasMs } from "./chat-timeline.js";` and replace the block from `// Compute chat-to-video timing correction.` through the `.sort(...)` (lines ~664-690) with:

```js
          // Chat-to-video timing correction (see chat-timeline.js for the
          // semantics per platform). Multi-part YouTube jobs use the same
          // rule: the video begins at the actual stream start regardless of
          // when Moombox started downloading.
          const chatBiasMs = computeChatBiasMs({
            platform: this.playerChatData.platform,
            chatStreamStartTime: this.playerChatData.streamStartTime,
            jobStreamStartTime: this.playerJob.streamStartTime,
          });
          this.playerChatMessages = (this.playerChatData.messages || [])
            .map((m) => ({ ...m, offsetMs: normalizeOffsetMs(m.offsetMs) - chatBiasMs }))
            .sort((a, b) => a.offsetMs - b.offsetMs);
```
Then release the raw array to halve peak memory (U-L15): after building the emote map, set `this.playerChatData.messages = null;` and grep `player.js` for other readers of `playerChatData.messages` (there are none besides `filterChat`, which reads `playerChatMessages`).

- [ ] **Step 6: `go build -o moombox.exe ./cmd/moombox`**, open a YouTube job that started late and a Twitch multi-part job: chat must now line up on the Twitch job (previously early by the detection lag).
- [ ] **Step 7: Commit** — `fix(web): platform-aware chat bias; extract chat-timeline module with tests`.

---

### Task 10: Selection race, unhandled `play()` promises, degenerate ArrowRight (T-F7/U-M7, U-L4, T-F8)

**Files:**
- Modify: `web/public/modules/player.js:593-599, 269, 287, 1213, 1226, 1232`, `web/public/modules/segments.js:88, 128`, `web/public/modules/utils.js` (add `safePlay`)
- Test: `web/tests/utils.test.mjs` (append)

**Interfaces:**
- Produces: `safePlay(media)` in `utils.js` — calls `media.play()` and swallows the returned promise's rejection (autoplay policy / AbortError on rapid src swaps).

- [ ] **Step 1: Write the failing test**

```js
import { safePlay } from "../public/modules/utils.js";

test("safePlay swallows a rejected play() promise and tolerates a void return", async () => {
  let rejections = 0;
  const onUnhandled = () => { rejections++; };
  process.on("unhandledRejection", onUnhandled);
  try {
    safePlay({ play: () => Promise.reject(new Error("AbortError")) });
    safePlay({ play: () => undefined });
    await new Promise((r) => setImmediate(r));
    assert.equal(rejections, 0);
  } finally {
    process.off("unhandledRejection", onUnhandled);
  }
});
```

- [ ] **Step 2: Run — expect FAIL** (`safePlay` not exported).

Run: `node --test web/tests/utils.test.mjs`

- [ ] **Step 3: Implement**

`utils.js`:
```js
/**
 * play() returns a promise that rejects on autoplay refusal or when the source
 * changes before playback starts (AbortError on rapid cross-segment seeks).
 * Nothing needs to react — the user gets the native controls either way — so
 * the rejection is swallowed instead of surfacing as console noise.
 */
export function safePlay(media) {
  const p = media && media.play ? media.play() : undefined;
  if (p && typeof p.catch === "function") p.catch(() => {});
}
```
`player.js`: import `safePlay`; replace every bare `video.play()` (space key `:269`, resume overlay `:1213, 1226, 1232`) with `safePlay(video)` (for `document.getElementById("player-video").play()` use `safePlay(document.getElementById("player-video"))`). `segments.js`: import `safePlay` and replace `video.play()` at `:88` and `:128`.

Selection race — replace
```js
      const res = await fetch(`/api/jobs/${jobId}`);
      if (!res.ok || this._selectionSeq !== selectionId) return;
      this.playerJob = await res.json();
```
with
```js
      const res = await fetch(`/api/jobs/${jobId}`);
      if (!res.ok || this._selectionSeq !== selectionId) return;
      const job = await res.json();
      // The body of an OLDER selection can resolve after a newer one completed —
      // re-check before anything observable (playerJob, video.src) is touched.
      if (this._selectionSeq !== selectionId) return;
      this.playerJob = job;
```
ArrowRight (`:287`): `const maxSec = this._seg.totalDuration > 0 ? this._seg.totalDuration : Infinity; this.seekToGlobalTime(Math.min(maxSec, globalSec));`

- [ ] **Step 4: Run `node --test web/tests/*.test.mjs` — expect PASS.** Rebuild; rapidly switch jobs in the dropdown: the video shown must always match the selection.
- [ ] **Step 5: Commit** — `fix(web): close the player selection race; swallow play() rejections`.

---

### Task 11: Multi-part Twitch chat in the player (T-F1 client half, D5)

**Files:**
- Modify: `web/public/modules/player.js` (`onPlayerJobSelect` chat-loading block ~652-705; new `_fetchChatData(jobId, selectionId)`)

**Interfaces:**
- Consumes: `GET /api/jobs/{id}/segments/{index}/chat` (Task 8), `mergePartChats` (Task 9), `this._seg.segOffsets[i].startOffset` (`segments.js:31-42`), `segment.chatFile` (already in the job JSON).
- Produces: `async _fetchChatData(jobId, selectionId) → object|null` — the chat data object (`platform`, `streamStartTime`, `emotes`, `messages`) or `null` when stale/unavailable.

- [ ] **Step 1: Implement the fetcher**

```js
  /**
   * Load the chat for the selected job. A multi-part job whose parts carry
   * their own chat files (Twitch live: offsets are part-relative) is merged
   * onto the global timeline part by part; everything else uses the job-level
   * file. Returns null when the selection changed underneath us or nothing
   * was available.
   */
  async _fetchChatData(jobId, selectionId) {
    const segments = this.playerJob.segments || [];
    const withChat = segments.filter((s) => s.chatFile);
    if (segments.length > 1 && withChat.length > 0 && this._seg.active) {
      const parts = await Promise.all(withChat.map(async (s) => {
        try {
          const r = await fetch(`/api/jobs/${jobId}/segments/${s.segmentIndex}/chat`);
          if (!r.ok) return null;
          const data = await r.json();
          const off = this._seg.segOffsets.find((o) => o.segmentIndex === s.segmentIndex);
          return { startOffsetSec: off ? off.startOffset : 0, data };
        } catch {
          return null;
        }
      }));
      if (this._selectionSeq !== selectionId) return null;
      const merged = mergePartChats(parts.filter(Boolean));
      if (merged.messages.length > 0) return merged;
    }
    const chatRes = await fetch(`/api/jobs/${jobId}/chat`);
    if (this._selectionSeq !== selectionId) return null;
    if (!chatRes.ok) return null;
    const data = await chatRes.json();
    return this._selectionSeq !== selectionId ? null : data;
  }
```
Import `mergePartChats` from `./chat-timeline.js`. In `onPlayerJobSelect` replace the `const chatRes = await fetch(...)` … `this.playerChatData = await chatRes.json();` lines with `this.playerChatData = await this._fetchChatData(jobId, selectionId); if (!this.playerChatData) return;` keeping the surrounding `try/catch` and the `if (this.playerJob.chatFilename)` gate loosened to `if (this.playerJob.chatFilename || (this.playerJob.segments || []).some((s) => s.chatFile))`.

- [ ] **Step 2: Rebuild and verify** on a Twitch job with ≥2 parts: the sidebar count equals the sum of the parts' `messageCount`; seek into part 2 — chat matches the video.
- [ ] **Step 3: Commit** — `feat(web): merge per-part Twitch chat onto the player timeline`.

---

# Phase 3 — Overlay engine

### Task 12: `nico-lanes.js` — media-time two-edge lane allocator (N-F3, N-F5)

**Files:**
- Create: `web/public/modules/nico-lanes.js`
- Test: `web/tests/nico-lanes.test.mjs` (create)

**Interfaces:**
- Produces: `class LaneAllocator { constructor(laneCount); reset(laneCount?); get laneCount; freeAt(lane, widthPx, stageWidthPx, durationMs, gapMs); allocate({ nowMs, widthPx, stageWidthPx, durationMs, lanesNeeded, gapMs }) → laneIndex | -1 }`. All times are MEDIA milliseconds supplied by the caller.

- [ ] **Step 1: Write the failing tests**

```js
import { test } from "node:test";
import assert from "node:assert/strict";
import { LaneAllocator } from "../public/modules/nico-lanes.js";

const W = 1000, D = 8000;
const alloc = (la, nowMs, widthPx, extra = {}) =>
  la.allocate({ nowMs, widthPx, stageWidthPx: W, durationMs: D, lanesNeeded: 1, gapMs: 0, ...extra });

test("empty lanes: first allocation takes lane 0, a simultaneous one takes lane 1", () => {
  const la = new LaneAllocator(3);
  assert.equal(alloc(la, 0, 100), 0);
  assert.equal(alloc(la, 0, 100), 1);
});

test("wide follower must wait for the two-edge bound: D·wj/(W+wj)", () => {
  // leader 100 px at t=0; follower 600 px: bound = 8000*600/1600 = 3000 ms
  const la = new LaneAllocator(1);
  assert.equal(alloc(la, 0, 100), 0);
  assert.equal(alloc(la, 1000, 600), -1);   // right edge cleared (930 ms) but the follower would overtake
  assert.equal(alloc(la, 2999, 600), -1);
  assert.equal(alloc(la, 3000, 600), 0);
});

test("narrow follower behind a wide leader waits for the leader's tail: D·wi/(W+wi)", () => {
  const la = new LaneAllocator(1);
  assert.equal(alloc(la, 0, 600), 0);          // bound for any follower ≤ 600 px = 3000 ms
  assert.equal(alloc(la, 2999, 100), -1);
  assert.equal(alloc(la, 3000, 100), 0);
});

test("gapMs adds a fixed buffer", () => {
  const la = new LaneAllocator(1);
  assert.equal(alloc(la, 0, 100), 0);
  assert.equal(alloc(la, 3000, 600, { gapMs: 150 }), -1);
  assert.equal(alloc(la, 3150, 600, { gapMs: 150 }), 0);
});

test("lanesNeeded requires consecutive free lanes", () => {
  const la = new LaneAllocator(3);
  assert.equal(alloc(la, 0, 100), 0);                            // lane 0 busy
  assert.equal(alloc(la, 0, 100, { lanesNeeded: 2 }), 1);       // lanes 1-2
  assert.equal(alloc(la, 0, 100, { lanesNeeded: 2 }), -1);
});

test("media time: the same nowMs never frees a lane (no wall clock inside)", () => {
  const la = new LaneAllocator(1);
  assert.equal(alloc(la, 5000, 300), 0);
  assert.equal(alloc(la, 5000, 300), -1);
  assert.equal(alloc(la, 5000 + D, 300), 0);   // a full traverse later it is certainly free
});

test("reset clears occupancy and can change the lane count", () => {
  const la = new LaneAllocator(2);
  alloc(la, 0, 100); alloc(la, 0, 100);
  la.reset(4);
  assert.equal(la.laneCount, 4);
  assert.equal(alloc(la, 0, 100), 0);
});
```

- [ ] **Step 2: Run — expect FAIL.**

Run: `node --test web/tests/nico-lanes.test.mjs`

- [ ] **Step 3: Create the module**

```js
/**
 * Lane allocator for right-to-left scrolling comments with a CONSTANT traverse
 * duration (the niconico model): every comment crosses stageWidth + ownWidth in
 * durationMs, so wider comments move faster and can overtake a narrower leader.
 *
 * A lane is free for a follower of width wj at media time t only if
 *     t >= leader.spawnAt + durationMs * max(wi, wj) / (stageWidth + max(wi, wj)) + gapMs
 * This single bound covers both collision conditions — "the leader's tail has
 * cleared the spawn edge" (needs wi) and "the follower cannot catch the
 * leader before it exits" (needs wj) — because w/(W+w) increases with w.
 *
 * All times are MEDIA milliseconds passed in by the caller: lanes freeze while
 * the video is paused and scale with playbackRate for free. Pure; tested in
 * web/tests/nico-lanes.test.mjs.
 */
export class LaneAllocator {
  constructor(laneCount) {
    this.lanes = [];
    this.reset(laneCount);
  }

  /** Clear all occupancy; optionally change the lane count. */
  reset(laneCount = this.lanes.length) {
    this.lanes = Array.from({ length: Math.max(0, laneCount | 0) }, () => null);
  }

  get laneCount() {
    return this.lanes.length;
  }

  /** Media time at which lane `l` accepts a follower of `widthPx`; -Infinity when empty. */
  freeAt(l, widthPx, stageWidthPx, durationMs, gapMs) {
    const lead = this.lanes[l];
    if (!lead) return -Infinity;
    const w = Math.max(lead.width, widthPx);
    return lead.spawnAt + (durationMs * w) / (stageWidthPx + w) + gapMs;
  }

  /**
   * Occupy `lanesNeeded` consecutive lanes at `nowMs` and return the first
   * index, or -1 when no run of lanes is free yet.
   */
  allocate({ nowMs, widthPx, stageWidthPx, durationMs, lanesNeeded = 1, gapMs = 0 }) {
    const n = this.lanes.length;
    for (let l = 0; l + lanesNeeded <= n; l++) {
      let ok = true;
      for (let k = 0; k < lanesNeeded; k++) {
        if (nowMs < this.freeAt(l + k, widthPx, stageWidthPx, durationMs, gapMs)) {
          ok = false;
          break;
        }
      }
      if (ok) {
        for (let k = 0; k < lanesNeeded; k++) this.lanes[l + k] = { spawnAt: nowMs, width: widthPx };
        return l;
      }
    }
    return -1;
  }
}
```

- [ ] **Step 4: Run — expect PASS.**
- [ ] **Step 5: Commit** — `feat(web): media-time two-edge lane allocator for the nico overlay`.

---

### Task 13: Rewire `spawnNicoMessages` — media-time lanes, FIFO deferral, drop counter, rate, visibility, no play-reset (N-F1, N-F4, N-F5, N-F7, N-F8, D1, D2)

**Files:**
- Modify: `web/public/modules/player.js` — constructor (`:22-26`), `initPlayer` handlers (`:106-131`, `:163-200`, `:230-245`), `clearPlayer` (`:406-420`), `onPlayerJobSelect` (`:652`, after messages are built), `onPlayerTimeUpdate` (`:842-860`), `clearNicoOverlay` (`:899-909`), `spawnNicoMessages` (`:911-1084`)
- Modify: `web/public/index.html:331` (add the drop pill), `web/public/moombox.css` (pill style)

**Interfaces:**
- Consumes: `LaneAllocator` (Task 12), `indexAfter` (Task 9).
- Produces (instance fields): `this._lanes: LaneAllocator`, `this.nicoCursor: number` (index of the next message to consider), `this._nicoAnims: Set<Animation>`, `this.nicoDropped: number`; method `_resetNicoCursor(effectiveMs)`; constants at module top:

```js
const NICO_DURATION_MS = 8000;      // traverse time; niconico uses 4 s, Moombox keeps its 8 s feel (D4)
const NICO_MAX_LATENESS_MS = 2000;  // a message not placed within 2 s of its time is dropped and counted (D1/D2)
const NICO_LANE_GAP_MS = 150;       // spacing buffer between consecutive occupants of a lane
const NICO_MAX_PER_TICK = 20;       // DOM work cap per timeupdate tick
```

- [ ] **Step 1: State and cursor helper** — in the constructor replace `nicoLaneCount/nicoLaneAvail/nicoLastSpawnMs` with:

```js
    this._lanes = new LaneAllocator(15);   // lane count is re-derived from geometry in Task 14
    this.nicoCursor = -1;                  // -1 = not anchored yet
    this._nicoAnims = new Set();
    this.nicoDropped = 0;
    this._nicoDropPillTimer = null;
```
Add the method:
```js
  /**
   * Anchor the overlay cursor at `effectiveMs`: the next tick considers only
   * messages newer than effectiveMs − NICO_MAX_LATENESS_MS (a short seed of
   * "chat that was already flying"), never the whole pre-show backlog. Also
   * frees every lane — the caller has cleared the overlay.
   */
  _resetNicoCursor(effectiveMs) {
    this.nicoCursor = indexAfter(this.playerChatMessages, effectiveMs - NICO_MAX_LATENESS_MS);
    this._lanes.reset();
  }
```
Replace every `this.nicoLastSpawnMs = …` site: `seeked` handler and offset input/reset → `this.clearNicoOverlay(); this._resetNicoCursor(currentMs + this.playerCustomOffsetMs);`; `clearPlayer` and `onPlayerJobSelect` (`:652`) → `this.nicoCursor = -1;`. In the **`play` handler delete the cursor assignment entirely** (N-F1) — keep only the animation resume loop, now over `this._nicoAnims`:
```js
    video.addEventListener("pause", () => { for (const a of this._nicoAnims) a.pause(); });
    video.addEventListener("play", () => { for (const a of this._nicoAnims) a.play(); });
    video.addEventListener("ratechange", () => {
      const rate = video.playbackRate || 1;
      for (const a of this._nicoAnims) a.playbackRate = rate;
    });
    document.addEventListener("visibilitychange", () => {
      // Hidden documents never dispatch animation finish events, so spawned
      // messages would pile up until the tab is shown. Clear and re-anchor.
      if (document.hidden) {
        this.clearNicoOverlay();
      } else if (this.playerChatMessages.length) {
        this._resetNicoCursor(this.getGlobalTimeMs() + this.playerCustomOffsetMs);
      }
    });
```
`clearNicoOverlay` becomes:
```js
  clearNicoOverlay() {
    for (const a of this._nicoAnims) a.cancel();
    this._nicoAnims.clear();
    const overlay = document.getElementById("player-nico-overlay");
    if (overlay) overlay.replaceChildren();
    this._lanes.reset();
  }
```

- [ ] **Step 2: Rewrite `spawnNicoMessages`**

```js
  spawnNicoMessages(effectiveMs) {
    const messages = this.playerChatMessages;
    if (!messages.length || document.hidden) return;
    if (this.nicoCursor < 0) this._resetNicoCursor(effectiveMs);

    const overlay = document.getElementById("player-nico-overlay");
    const video = document.getElementById("player-video");
    if (!overlay || !video) return;
    const stageW = overlay.clientWidth;
    const stageH = overlay.clientHeight;
    if (!stageW || !stageH) return;
    const laneCount = this._lanes.laneCount || 1;
    const laneHeight = stageH / laneCount;
    const rate = video.playbackRate || 1;

    let work = 0;
    while (this.nicoCursor < messages.length && messages[this.nicoCursor].offsetMs <= effectiveMs) {
      if (work++ >= NICO_MAX_PER_TICK) break;
      const msg = messages[this.nicoCursor];
      const lateness = effectiveMs - msg.offsetMs;
      if (lateness > NICO_MAX_LATENESS_MS) {
        // Waited too long for a lane (or seeded too far back) — drop, but count it.
        this.nicoDropped++;
        this.nicoCursor++;
        continue;
      }

      const el = this._buildNicoEl(msg);
      if (!el) { this.nicoCursor++; continue; }            // empty content (system-only message)
      el.style.left = `${stageW}px`;
      el.style.top = "0";
      overlay.appendChild(el);
      const w = el.offsetWidth;
      const h = el.offsetHeight;
      const lanesNeeded = Math.max(1, Math.ceil(h / laneHeight));

      const lane = this._lanes.allocate({
        nowMs: effectiveMs, widthPx: w, stageWidthPx: stageW,
        durationMs: NICO_DURATION_MS, lanesNeeded, gapMs: NICO_LANE_GAP_MS,
      });
      if (lane === -1) {
        el.remove();
        break; // FIFO: this message and everything after it wait for the next tick
      }

      el.style.top = `${lane * laneHeight}px`;
      const anim = el.animate(
        [{ transform: "translateX(0)" }, { transform: `translateX(-${stageW + w}px)` }],
        { duration: NICO_DURATION_MS, fill: "forwards" },
      );
      anim.playbackRate = rate;
      if (video.paused) anim.pause();
      anim.onfinish = () => { this._nicoAnims.delete(anim); el.remove(); };
      this._nicoAnims.add(anim);
      this.nicoCursor++;
    }
    this._updateNicoDropPill();
  }

  /** Build an overlay element for `msg`, or null when it has no renderable content. */
  _buildNicoEl(msg) {
    const el = document.createElement("div");
    el.className = "nico-message";
    if (msg.messageType === "announcement") {
      el.classList.add("announcement", `announcement-${announcementColorClass(msg.announcementColor)}`);
    }
    this.appendChatContent(el, msg.message || [], msg.emotes);
    if (!el.hasChildNodes()) return null;
    el.querySelectorAll(".chat-emoji").forEach((img) => { img.loading = "eager"; });
    return el;
  }

  /** Show "+N not shown" for a few seconds whenever the drop counter grew. */
  _updateNicoDropPill() {
    const pill = document.getElementById("player-nico-dropped");
    if (!pill) return;
    if (this.nicoDropped === this._nicoDroppedShown) return;
    this._nicoDroppedShown = this.nicoDropped;
    pill.textContent = `+${this.nicoDropped} not shown`;
    pill.hidden = false;
    clearTimeout(this._nicoDropPillTimer);
    this._nicoDropPillTimer = setTimeout(() => { pill.hidden = true; }, 3000);
  }
```
`onPlayerTimeUpdate` passes `effectiveMs = currentMs + this.playerCustomOffsetMs` to `spawnNicoMessages` (it currently passes `currentMs` and adds the offset inside — keep one convention: the argument IS effective time). Reset `this.nicoDropped = 0; this._nicoDroppedShown = 0;` in `onPlayerJobSelect` and `clearPlayer`.

- [ ] **Step 3: Markup + CSS** — `index.html:331` add inside `#player-video-wrapper`: `<div id="player-nico-dropped" class="nico-dropped-pill" hidden></div>`; CSS:
```css
.nico-dropped-pill {
    position: absolute; top: 8px; right: 8px;
    background: rgba(0, 0, 0, 0.6); color: #fff;
    font-size: var(--sl-font-size-x-small); padding: 2px 8px;
    border-radius: var(--sl-border-radius-small); pointer-events: none;
}
```

- [ ] **Step 4: Rebuild and verify**: (a) a Twitch VOD with offset-0 messages — they now appear at 0:00; (b) a busy chat at 2× — messages keep pace, the pill reports drops instead of silently thinning; (c) pause 10 s, resume — no overlap in the first seconds; (d) background the tab 5 min, return — overlay is empty, then resumes from the current time; (e) `node --test web/tests/*.test.mjs` still green.
- [ ] **Step 5: Commit** — `fix(web): nico overlay — media-time lanes, FIFO deferral with drop count, rate/visibility aware; remove play-handler cursor reset`.

---

### Task 14: Geometry — rows from the measured line box, container-query font, overlay on the video rect, resize handling, emote height, long-message clamp (N-F6, N-F9, D3)

**Files:**
- Modify: `web/public/moombox.css:1481-1510` (overlay + `.nico-message` + `.chat-emoji`), `web/public/modules/player.js` (`initPlayer` listeners; new `_updateNicoGeometry()`; `spawnNicoMessages` uses `this._nicoGeo`)

**Interfaces:**
- Produces: `this._nicoGeo = { width, height, laneHeight, rows, version }`; `_updateNicoGeometry()` called on `loadedmetadata`, the video's `resize` event, a `ResizeObserver` on `#player-video-wrapper`, and `fullscreenchange`.

- [ ] **Step 1: CSS**

```css
#player-nico-overlay {
    position: absolute;          /* left/top/width/height set by _updateNicoGeometry */
    pointer-events: none;
    overflow: hidden;
    container-type: size;        /* lets .nico-message size its font from the overlay height */
}

.nico-message {
    position: absolute;
    white-space: nowrap;
    color: #fff;
    font-weight: bold;
    font-size: clamp(0.9rem, 4.5cqh, 2.4rem);   /* ≈ 17 rows at line-height 1.3; niconico ≈ 13 */
    line-height: 1.3;
    max-width: 150%;
    overflow: hidden;
    text-overflow: ellipsis;
    paint-order: stroke fill;
    -webkit-text-stroke: 2px rgba(0, 0, 0, 0.7);
    text-shadow: 0 0 4px rgba(0, 0, 0, 0.6);
    will-change: transform;
    pointer-events: none;
}

.nico-message .chat-emoji {
    height: 1em;                 /* never taller than the line box → one lane per message */
    width: auto;
    vertical-align: -0.15em;
    display: inline-block;
}
```
(Drop the four offset shadows; keep the announcement `--nico-accent` rules, switching them to `-webkit-text-stroke-color: var(--nico-accent)`.)

- [ ] **Step 2: JS geometry**

```js
  /**
   * Size the overlay to the VIDEO'S RENDERED RECT (not the wrapper, which has
   * letterbox bars), derive the row count from a measured line box, and clear
   * in-flight messages — their keyframes were computed for the old geometry.
   */
  _updateNicoGeometry() {
    const video = document.getElementById("player-video");
    const overlay = document.getElementById("player-nico-overlay");
    if (!video || !overlay) return;
    const bw = video.clientWidth, bh = video.clientHeight;
    let w = bw, h = bh, left = video.offsetLeft, top = video.offsetTop;
    const vw = video.videoWidth, vh = video.videoHeight;
    if (vw > 0 && vh > 0 && bw > 0 && bh > 0) {
      const scale = Math.min(bw / vw, bh / vh);
      w = Math.round(vw * scale);
      h = Math.round(vh * scale);
      left += Math.round((bw - w) / 2);
      top += Math.round((bh - h) / 2);
    }
    Object.assign(overlay.style, { left: `${left}px`, top: `${top}px`, width: `${w}px`, height: `${h}px` });

    const probe = document.createElement("div");
    probe.className = "nico-message";
    probe.style.visibility = "hidden";
    probe.textContent = "Ag";
    overlay.appendChild(probe);
    const rowH = probe.offsetHeight || 24;
    probe.remove();

    const rows = Math.max(1, Math.floor(h / rowH));
    const version = (this._nicoGeo?.version || 0) + 1;
    this._nicoGeo = { width: w, height: h, laneHeight: h / rows, rows, version };
    this.clearNicoOverlay();
    this._lanes.reset(rows);
    if (this.playerChatMessages.length) {
      this._resetNicoCursor(this.getGlobalTimeMs() + this.playerCustomOffsetMs);
    }
  }
```
In `initPlayer`: `video.addEventListener("loadedmetadata", () => this._updateNicoGeometry()); video.addEventListener("resize", () => this._updateNicoGeometry()); document.addEventListener("fullscreenchange", () => this._updateNicoGeometry());` and
```js
    const wrapper = document.getElementById("player-video-wrapper");
    if (wrapper && "ResizeObserver" in window) {
      let pending = 0;
      new ResizeObserver(() => {
        cancelAnimationFrame(pending);
        pending = requestAnimationFrame(() => this._updateNicoGeometry());
      }).observe(wrapper);
    }
```
In `spawnNicoMessages` replace the `stageW/stageH/laneCount/laneHeight` reads with `const geo = this._nicoGeo; if (!geo || !geo.width || !geo.height) return; const stageW = geo.width; const laneHeight = geo.laneHeight;`.

- [ ] **Step 3: Rebuild and verify**: 375 px-wide viewport — text is smaller, ≥10 rows, one lane per emote message; fullscreen mid-flight — overlay clears and continues at full-screen scale; a 4:3 video in a 16:9 wrapper — nothing flies over the black bars.
- [ ] **Step 4: Commit** — `fix(web): nico overlay sized to the video rect with height-relative rows and font`.

---

### Task 15: Reduced motion — overlay defaults off; remove the CSS static-stack fallback (N-F12, D6)

**Files:**
- Modify: `web/public/modules/player.js:68-75` (toggle restore), `web/public/moombox.css:2710-2721` (delete the `#player-nico-overlay`/`.nico-message` reduced-motion rules; keep the global `animation-duration` rule)

- [ ] **Step 1: Implement** — in `initPlayer`, after reading `savedNico`:
```js
    const reduceMotion = window.matchMedia?.("(prefers-reduced-motion: reduce)").matches === true;
    if (savedNico === null && reduceMotion) {
      // Flying text is exactly what this preference asks to avoid. Default the
      // overlay off; the checkbox still lets the user opt in.
      nicoToggle.checked = false;
      this.nicoEnabled = false;
      document.getElementById("player-nico-overlay").style.display = "none";
    }
```
Delete the reduced-motion block for `#player-nico-overlay` and `.nico-message` in the CSS (lines 2710-2721).

- [ ] **Step 2: Verify** with the OS setting on: fresh profile → overlay unchecked; check it → animation runs. Commit — `fix(web): nico overlay defaults off under prefers-reduced-motion`.

---

# Phase 4 — Before/after-video chat in the sidebar

### Task 16: Pre-show and post-end dividers, header counts, `.post` state, Sync-to-end (N-F10, policy §Before/After)

**Files:**
- Modify: `web/public/modules/player.js` — `onPlayerJobSelect` (after messages are built), `_buildChatMessageEl`, `updateSidebarActiveState`, `resetSidebarToTime`, `onSegmentEnded`, `syncSidebarToTime`, `initPlayer` (`loadedmetadata`, sync button)
- Modify: `web/public/moombox.css` (after `.chat-msg.active`)

**Interfaces:**
- Consumes: `partitionChatByVideo` (Task 9).
- Produces: `this._chatParts = { preCount, firstLiveIndex, postCount, firstPostIndex }`; `_computeChatParts()`, `_markPostEnd()`, `_updateSidebarHeader()`; CSS classes `.chat-msg.divider-before[data-divider]`, `.chat-msg.post`.

- [ ] **Step 1: Partition + header**

```js
  /** Known video length in ms: segment sum, else the element's duration, else job metadata. */
  _videoDurationMs() {
    const video = document.getElementById("player-video");
    if (this._seg.active && this._seg.totalDuration > 0) return this._seg.totalDuration * 1000;
    if (video && Number.isFinite(video.duration) && video.duration > 0) return video.duration * 1000;
    const len = this.playerJob?.lengthSeconds;
    return len > 0 ? len * 1000 : 0;
  }

  _computeChatParts() {
    this._chatParts = partitionChatByVideo(this.playerChatMessages, this._videoDurationMs());
    this._updateSidebarHeader();
  }

  _updateSidebarHeader() {
    const n = this.playerChatMessages.length;
    const p = this._chatParts || { preCount: 0, postCount: 0 };
    let text = `${n} messages`;
    if (p.preCount > 0) text += ` · ${p.preCount} pre-show`;
    if (p.postCount > 0) text += ` · ${p.postCount} after end`;
    document.getElementById("player-sidebar-msg-count").textContent = text;
  }
```
Call `this._computeChatParts()` in `onPlayerJobSelect` right after `this.playerChatMessages` is built (replace the existing `msg-count` line), and in `initPlayer` add `video.addEventListener("loadedmetadata", () => { if (this.playerChatMessages.length) { this._computeChatParts(); this._applyDividers(); } });` (single-file jobs learn their duration here).

- [ ] **Step 2: Divider rows without breaking `children[i] === messages[i]`** — dividers are `::before` pseudo-content on the first message of each region:

```js
  /** Stamp/clear divider classes on the two boundary rows (idempotent). */
  _applyDividers() {
    const children = document.getElementById("player-sidebar-messages").children;
    for (const el of children) {
      if (el.classList.contains("divider-before")) { el.classList.remove("divider-before"); delete el.dataset.divider; }
    }
    const p = this._chatParts;
    if (!p) return;
    if (p.preCount > 0 && p.firstLiveIndex >= 0 && children[p.firstLiveIndex]) {
      children[p.firstLiveIndex].classList.add("divider-before");
      children[p.firstLiveIndex].dataset.divider = `Waiting room — ${p.preCount} messages before the stream`;
    }
    if (p.firstPostIndex >= 0 && children[p.firstPostIndex]) {
      children[p.firstPostIndex].classList.add("divider-before");
      children[p.firstPostIndex].dataset.divider = `Recording ended — ${p.postCount} messages after it`;
    }
  }
```
In `_buildChatMessageEl(msg, index)` add the same stamping for `index === firstLiveIndex` / `index === firstPostIndex` so chunk-built rows are right on creation (the loop above only clears stale ones — call `_applyDividers()` at the end of `buildFrom` when the build completes and after `_computeChatParts()` on `loadedmetadata`).

CSS:
```css
.chat-msg.divider-before::before {
    content: attr(data-divider);
    display: block;
    margin: 6px calc(-1 * var(--sl-spacing-small)) 4px;
    padding: 2px var(--sl-spacing-small);
    font-size: var(--sl-font-size-x-small);
    color: var(--sl-color-neutral-600);
    background: var(--sl-color-neutral-200);
    border-top: 1px solid var(--sl-color-neutral-300);
    border-bottom: 1px solid var(--sl-color-neutral-300);
}
.chat-msg.post { opacity: 1; color: var(--sl-color-neutral-600); }
```

- [ ] **Step 3: Post-end state**

```js
  /** After the recording ends, the tail is "after it", not "future": readable, labelled, reachable. */
  _markPostEnd() {
    const p = this._chatParts;
    if (!p || p.firstPostIndex < 0) return;
    const children = document.getElementById("player-sidebar-messages").children;
    for (let i = p.firstPostIndex; i < children.length; i++) {
      children[i].classList.remove("future");
      children[i].classList.add("post");
    }
  }
```
Call it from `onSegmentEnded` in the `!advanced` branch and from `onPlayerTimeUpdate` when `currentMs + 250 >= this._videoDurationMs() && this._videoDurationMs() > 0`. In `resetSidebarToTime`, the "previously active but now future" loop must also `classList.remove("post")`, and add after it:
```js
    // Seeking back off the end returns post-end rows to "future".
    const p = this._chatParts;
    if (p && p.firstPostIndex >= 0 && newActiveIndex < p.firstPostIndex) {
      for (let i = p.firstPostIndex; i < children.length; i++) children[i].classList.remove("post");
    }
```

- [ ] **Step 4: Sync-to-end** — in `syncSidebarToTime`, before the existing target computation:
```js
    const video = document.getElementById("player-video");
    const p = this._chatParts;
    if (video?.ended && p && p.firstPostIndex >= 0 && container.children[p.firstPostIndex]) {
      this._programmaticScroll = true;
      container.scrollTop = Math.max(0, container.children[p.firstPostIndex].offsetTop - 8);
      requestAnimationFrame(() => { this._programmaticScroll = false; });
      return;
    }
```

- [ ] **Step 5: Rebuild and verify**: a YouTube job with waiting-room chat shows the "Waiting room — N messages" divider and header "… · N pre-show"; play to the end → tail rows readable with the "Recording ended" divider, Sync scrolls to it; seek back → they dim again. `node --test` green.
- [ ] **Step 6: Commit** — `feat(web): sidebar pre-show and post-end dividers, counts and post-end state`.

---

# Phase 5 — UI hardening

### Task 17: Segment indicator below the video as keyboard-reachable buttons (U-H2, U-M8)

**Files:**
- Modify: `web/public/index.html:328-332` (wrap the wrapper), `web/public/moombox.css:1464-1480, 1704-1712, 2188-2205`, `web/public/modules/player.js:474-513`

- [ ] **Step 1: Markup** — wrap `#player-video-wrapper` in `<div id="player-video-column">…</div>` inside `#player-viewport`.
- [ ] **Step 2: CSS**
```css
#player-video-column { flex: 1; min-width: 0; min-height: 0; display: flex; flex-direction: column; }
#player-video-wrapper { flex: 1; min-height: 0; position: relative; overflow: hidden; background-color: #000; border-radius: var(--sl-border-radius-medium); display: flex; align-items: center; justify-content: center; }
.segment-indicator-block { border: 0; font: inherit; font-size: 11px; padding: 0 4px; /* keep existing flex/colour rules */ }
.segment-indicator-block:focus-visible { outline: 2px solid var(--sl-color-primary-500); outline-offset: -2px; }
@media only screen and (max-width: 992px) { #player-video-column { flex: none; } }
```
(Remove `flex: 1` from the old `#player-video-wrapper` mobile override in favour of the column rule.)
- [ ] **Step 3: JS** — in `buildSegmentIndicator` create `const block = document.createElement("button"); block.type = "button";` and set `block.setAttribute("aria-label", `Seek to segment ${i + 1}, ${seg.quality}`)`; append the indicator with `document.getElementById("player-video-column")?.appendChild(indicator)`.
- [ ] **Step 4: Rebuild; a multi-segment job shows the bar under the video full-width on desktop; Tab reaches the blocks. Commit** — `fix(web): segment indicator renders below the video; blocks are buttons`.

### Task 18: Resume overlay — Escape guards, `aria-modal`, focus restore (U-M1, U-M8)

**Files:** `web/public/modules/player.js:1183-1246`

- [ ] **Step 1:** In `_showResumeDialog`: `overlay.setAttribute("aria-modal", "true"); this._resumeReturnFocus = document.activeElement;` and change the keydown handler to
```js
    document.addEventListener("keydown", (e) => {
      if (e.key !== "Escape") return;
      if (document.querySelector("sl-dialog[open]") || isTypingInInput(e)) return;
      e.preventDefault();
      dismiss();
      safePlay(document.getElementById("player-video"));
      this._startWatchTracking(jobId);
    }, { signal: sig });
```
In `_dismissResumeDialog`: after removing the overlay, `if (this._resumeReturnFocus?.isConnected) this._resumeReturnFocus.focus({ preventScroll: true }); this._resumeReturnFocus = null;`.
- [ ] **Step 2: Verify**: with the overlay up, press `?` then Escape — the help closes, the overlay stays. Commit — `fix(web): resume overlay ignores Escape meant for dialogs/inputs; restores focus`.

### Task 19: One helper for the chat-offset UI (U-M2)

**Files:** `web/public/modules/player.js:163-245, 415-417, 730-740`

- [ ] **Step 1:** Add
```js
  /** Apply a persisted/cleared offset to state, input text and the reset button. */
  _applyOffsetUI(seconds) {
    const s = Number.isFinite(seconds) ? seconds : 0;
    this.playerCustomOffsetMs = Math.round(s * 1000);
    const input = document.getElementById("player-chat-offset");
    if (input) input.value = s === 0 ? "" : String(s);
    this._syncOffsetResetButton();
  }

  _syncOffsetResetButton() {
    const btn = document.getElementById("player-chat-offset-reset");
    if (btn) btn.style.display = this.playerCustomOffsetMs !== 0 ? "" : "none";
  }
```
Use `_applyOffsetUI(this.playerJob.chatOffset || 0)` in the load path (replacing lines 730-740), `_applyOffsetUI(0)` in `clearPlayer` and in the reset-button click; in the `input` handler keep the sanitiser and replace the inline button toggling with `this._syncOffsetResetButton()`.
- [ ] **Step 2: Verify**: job with a saved offset shows the ✕; switching to a job without one hides it. Commit — `fix(web): chat-offset reset button tracks the loaded offset`.

### Task 20: Job list refreshes while a video is loaded (U-M3)

**Files:** `web/public/modules/player.js:527-580`

- [ ] **Step 1:** Delete the `isPlaying` early-return block (keep the "current job vanished → clearPlayer()" check). Guard the rebuild against any synthetic `sl-change`:
```js
      this._rebuildingOptions = true;
      try {
        select.querySelectorAll("sl-option").forEach((o) => o.remove());
        all.forEach(/* unchanged */);
        if (select.updateComplete) await select.updateComplete.catch(() => {});
        if (currentValue && all.some((j) => j.id === currentValue)) select.value = currentValue;
      } finally {
        this._rebuildingOptions = false;
      }
```
and at the top of the `sl-change` listener: `if (this._rebuildingOptions) return;`.
- [ ] **Step 2: Verify**: while a video plays, finish an import — the new job appears in the dropdown without interrupting playback. Commit — `fix(web): player job list refreshes during playback`.

### Task 21: Keyboard focus and shortcut collisions (U-M4)

**Files:** `web/public/modules/player.js:57-66, 250-330`, `web/public/modules/utils.js:69-78`, `web/public/app.js:379, 3744-3749`, `web/public/index.html:328`

- [ ] **Step 1:** `index.html`: `<div id="player-video-wrapper" tabindex="-1">`. In the job `sl-change` handler after `onPlayerJobSelect(val)`: `jobSelect.blur(); document.getElementById("player-video-wrapper")?.focus({ preventScroll: true });`. In `app.js` player tab-show branch: `requestAnimationFrame(() => document.getElementById("player-video-wrapper")?.focus({ preventScroll: true }));`.
- [ ] **Step 2:** `utils.js` `isTypingInInput`: replace the `SL-SELECT` clause with `if (tag === "SL-SELECT") return el.open === true;`.
- [ ] **Step 3:** `_playerKeyHandler`: after the typing guard add
```js
      const target = e.composedPath()[0];
      const tTag = target instanceof HTMLElement ? target.tagName : "";
      if (e.key === " " && /^(BUTTON|SL-BUTTON|SL-ICON-BUTTON|SL-CHECKBOX|SL-SWITCH)$/.test(tTag)) return; // let the control handle Space
      const key = e.key.length === 1 ? e.key.toLowerCase() : e.key;
      switch (key) {
```
(and use `key` in the `case` labels).
- [ ] **Step 4:** `app.js` global handler: `case "f": if (!isPlayerActive) { …focus search… }`.
- [ ] **Step 5: Verify**: select a video, press Space immediately — it plays; click the Player tab, press → — it seeks, not switches tabs; Caps Lock + F → fullscreen. Commit — `fix(web): player shortcuts work right after selection; no tab/control collisions`.

### Task 22: Mobile, touch and accessibility attributes (U-M5, U-M6, U-M8)

**Files:** `web/public/index.html:322-345`, `web/public/modules/player.js:145-153`, `web/public/moombox.css`

- [ ] **Step 1:** `<video id="player-video" controls preload="metadata" playsinline webkit-playsinline>`; `<div id="player-nico-overlay" aria-hidden="true">`; `sl-select` gets `label="Video"` and `sl-input#chat-search` gets `label="Search chat"`, both visually hidden:
```css
#player-job-select::part(form-control-label), #chat-search::part(form-control-label) {
    position: absolute; width: 1px; height: 1px; overflow: hidden; clip: rect(0 0 0 0); white-space: nowrap;
}
```
- [ ] **Step 2:** Scroll-lock via pointer events:
```js
    sidebarMessages.addEventListener("pointerenter", (e) => { if (e.pointerType !== "touch") this.playerScrollLock = true; });
    sidebarMessages.addEventListener("pointerleave", (e) => { if (e.pointerType !== "touch" && this.playerAutoScroll) this.playerScrollLock = false; });
```
(remove the `mouseenter`/`mouseleave` pair.)
- [ ] **Step 3: Verify** on a phone-sized viewport / device emulation; commit — `fix(web): inline playback on iOS, touch-safe scroll lock, a11y labels`.

### Task 23: Documentation

**Files:** `docs/spec/user-interfaces.md` (rows ~43, 457, 511, API table), `docs/spec/data-and-storage.md` §Chat Files, `docs/spec/platform-services.md` (~543, ~671, YouTube chat), `docs/spec/security.md` (CSP), `web/public/index.html:331, 2014-2024`

- [ ] **Step 1:** `user-interfaces.md`: player row → "~1.4k lines; niconico overlay (media-time lanes, `nico-lanes.js`), sidebar with pre-show/post-end dividers, chat search, per-job chat offset (positive = chat earlier: effective = video + offset), resume/watched tracking, per-part chat merge (`chat-timeline.js`)"; add `GET /api/jobs/{id}/segments/{index}/chat`; correct the `/api/jobs/{id}` caching note; note that Player `F` overrides Global `F` while the player tab is active.
- [ ] **Step 2:** `index.html`: controls hint → `Space: Play · ←→: Seek · ↑↓: Vol · F: Full · M: Mute · C: Chat · S: Sidebar`.
- [ ] **Step 3:** `data-and-storage.md`: chat.json `offsetMs` is SIGNED for both platforms; `ChatResumeState.streamStartMs`. `platform-services.md`: Twitch signed offsets; YouTube replay negative recovery; the epoch rule.
- [ ] **Step 4: Commit** — `docs: player chat timeline, overlay engine and per-part chat`.

### Task 24 (optional, D7): jsdom harness for `player.js`

**Files:** `web/tests/package.json` (create: `{"private": true, "devDependencies": {"jsdom": "^25"}}`), `web/tests/helpers/player-dom.mjs`, `web/tests/player.test.mjs`, `web/tests/README.md`

- [ ] **Step 1:** Helper: build a `JSDOM` (`pretendToBeVisual: true`) from the player panel markup (`index.html:318-357`), define stub custom elements for `sl-select`, `sl-option`, `sl-checkbox`, `sl-input`, `sl-button`, `sl-icon-button`, `sl-icon`, `sl-tab-panel` (HTMLElement subclasses with `value`/`checked`/`open` and `updateComplete = Promise.resolve()`), stub `globalThis.fetch` with a route table that records calls, `navigator.sendBeacon`, `Element.prototype.animate` (returns `{ pause(){}, play(){}, cancel(){}, playbackRate: 1, onfinish: null }`), `HTMLMediaElement.prototype.play/pause/load`, and `window.matchMedia`. Export `makePlayer({ jobs, archived, job, watchState, chat })` returning `{ player, fetchLog, flush }`.
- [ ] **Step 2:** First test — the selection race from Task 10: resolve job A's body after job B completed; assert `player.playerJob.id === "B"` and `video.src` ends with `/B/video`.
- [ ] **Step 3:** Next four: offset restore/reset (Task 19); chunked-build alignment with a seek mid-build; search ↔ autoscroll state machine; keyboard gating (Space ignored with `sl-dialog[open]`, `C` flips the checkbox + localStorage).
- [ ] **Step 4:** README: how to `npm ci` in `web/tests` and run; commit — `test(web): jsdom harness and first player tests`.

---

## Self-review

- **Spec coverage**: High findings — T-F1 (Tasks 8, 9, 11), T-F3 (9), T-F2 (4), N-F1 (13), N-F2 (2, 3), N-F3 (12–13), N-F4 (13), U-H1 (1), U-H2 (17). Medium — T-F4 (5), T-F5/T-F6 (7), T-F7 (10), N-F5 (12–13), N-F6/N-F9 (14), N-F7/N-F8 (13), N-F10 (16), U-M1 (18), U-M2 (19), U-M3 (20), U-M4 (21), U-M5/U-M6/U-M8 (17, 22), U-M9 (6). Low — T-F8/U-L4 (10), T-F12 (9), T-F15 (7), N-F12 (15), N-F14 (14 CSS), U-L15 partial (9, 13), docs (23). Deliberately NOT addressed: N-F11 (lateness positioning — conflicts with the lane rule; ≤250 ms jitter accepted), T-F9/T-F10/T-F11/T-F14, N-F13, U-L1/L2/L3/L5/L7/L9/L13, U-L16 — leave for a follow-up once the above has soaked.
- **Placeholder scan**: every code step carries the code; browser checks are enumerated per task.
- **Type consistency**: `indexAfter`, `partitionChatByVideo`, `computeChatBiasMs`, `normalizeOffsetMs`, `mergePartChats` (Task 9) are the names used in Tasks 11, 13, 16; `LaneAllocator.allocate({nowMs, widthPx, stageWidthPx, durationMs, lanesNeeded, gapMs})` (Task 12) matches Task 13; `_resetNicoCursor`, `_nicoGeo`, `_nicoAnims`, `nicoCursor`, `nicoDropped` are consistent across Tasks 13–14; `safePlay` (Task 10) is used in Task 18; `UpdateResumePosition/UpdateChatOffset → bool` (Task 6) match the route edits.
