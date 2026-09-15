# Sweep Arc 1 — Twitch chat correctness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make live-recorded Twitch chat render exactly what went over the wire — correct emote spans after emoji, `/me` messages without their ACTION wrapper, a VOD chat tail that is not stranded by a burst second, an emote cache that cannot latch a failure, a bounded resume sidecar, and a half-open IRC socket detected in ~75 s instead of 6 minutes.

**Architecture:** Seven behaviour changes inside `internal/twitch` plus one replay-side correction in the frontend. Twitch IRC `emotes=` offsets index Unicode CODE POINTS; the emitted `TwitchEmoteRef.Start/End` stay UTF-16 code units because `player.js` slices with `String.prototype.substring`. Files written from now on carry a header scalar `emoteOffsets: "utf16"`; the player converts unmarked legacy IRC messages at load time, once per file, before any rendering. VOD chat pages by the server's own edge cursors after the first page. The IRC session grows a client-initiated `PING :moombox` keepalive on an injectable duration struct modelled on `internal/engine`'s `delays`.

**Tech Stack:** Go 1.27 (`internal/twitch`, `internal/utils`), `github.com/coder/websocket`, `net/http/httptest`, vanilla ES modules (`web/public/modules/{player,chat-timeline}.js`), `node --test` + jsdom (`web/tests`).

**Spec:** `docs/superpowers/specs/2026-09-15-sweep-fix-chain-design.md` § 4 (Arc 1 — Twitch chat correctness, branch `sweep-1-twitch-chat`). §1–§3 and §11–§12 of that document bind this arc too. Evidence and file:line anchors: `reports/sweep-2026-09-15.md` items T1-1, T1-3, T1-11, T2-15, T3-25, T4-34.

## Global Constraints

Copied verbatim from spec §3. Every task's requirements implicitly include this section.

- `go 1.27`, no CGo, pure-Go dependencies; Windows x64 + Linux x64 + Linux arm64 must build.
- LF line endings in every file the chain touches. Commits carry the two trailers
  `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>` and
  `Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq`.
- The anonymous logger interface (`Debug/Info/Warn/Error(msg string, args ...any)`) stays anonymous
  per struct. Every goroutine has an inline `defer func() { if r := recover(); … }()`.
- `internal/tui` never imports `internal/web/routes` or `internal/bgutils` (compile-time embed check).
- Helpers are defined in the task that first uses them (staticcheck U1000 is a hard gate).
- TDD per task: the failing test is written and run red before the change; every new assertion names
  the mutant that fails it (the reviewer verifies at least one).
- Every JS-touching task gates `go test ./internal/web/routes/` (its tests lift app.js/player.js
  bodies into goja) AND `node --test web/tests/*.test.mjs`. Every task that renames, moves or deletes a
  Go symbol, or edits `docs/spec/*.md`/`SPEC.md`, gates `go test ./internal/docs/` (the citation test
  requires the DECLARING file).
- Behaviour that the owner's rulings protect stays: ~60 Hz progress pipeline and DB write cadence
  (make updates cheaper, never rarer); DB layer untouched for perf; `monitors.probe_cooldown` default 0;
  the BotGuard interpreter gate; `/retry` vs `/resume` gates never shared; the Web cookie import stays
  unbounded (a test forbids WithTimeout).
- Reviewers never edit files (they reproduce in `git archive` scratchpad exports); implementers use git
  only for `add`/`commit` on the arc branch; no stashing, no rebasing, no checkout of other branches.
- User-facing strings: match the twin UI's exact output before mirroring it (read the twin's code).

### Arc 1 gates (spec §4 "Gates", plus the §2 merge-candidate set)

Per task, run only the gates its files touch. Before the merge, the controller runs the full set.

- Per task: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/twitch/...`
- JS-touching tasks additionally: `cd web/tests && node --test *.test.mjs` and
  `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/web/routes/`
- Doc/symbol-renaming tasks additionally: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/docs/`
- Merge candidate (controller): `gofmt -l ./cmd ./internal ./tools ./web` empty, `go vet ./...`,
  `staticcheck ./...` (pinned 2026.2.1) clean, `go build ./...`, `GOOS=linux GOARCH=amd64 go build ./...`,
  `GOOS=linux GOARCH=arm64 go build ./...`, ONE `go test -count=1 ./...`,
  `node --test web/tests/*.test.mjs`.
- No live gate applies to this arc: it touches no extraction client, no goja, no sidecar payload and no
  cipher.

### Worktree recipe (spec §2)

```bash
git worktree add -b sweep-1-twitch-chat .worktrees/sweep-1-twitch-chat main
# copy the gitignored inputs a fresh worktree lacks:
#   internal/bgutils/embed/{node-windows-amd64.gz,node-linux-amd64.gz,node-linux-arm64.gz,sidecar.tar.gz}
#   internal/cipher/testdata/*.js
cd .worktrees/sweep-1-twitch-chat/web/tests && npm ci --no-audit --no-fund
```

### Verified wire facts this arc rests on

Re-verified 2026-09-15 by the planner, by scanning the 18 real Shachimu `*.chat.json` archives under
`D:\Moombox\output\Shachimu` (the lossless `raw` IRC lines):

1. **Offsets index code points.** Of the 120 emote ranges whose message contains a non-BMP character
   before the range, 120/120 slice to a whole-word token under code-point indexing and 0/120 under
   UTF-16 indexing. Example: wire text `🤘 shachiOrcaWail`, range `2-15` → code points `shachiOrcaWail`,
   UTF-16 ` shachiOrcaWai`. (`C:\Users\Wulf\.claude\projects\D--Git-Moombox\memory\reference_twitch_emote_offsets_codepoints.md`)
2. **ACTION offsets index the STRIPPED text.** Of the 2 emote ranges found on real `\x01ACTION …\x01`
   PRIVMSGs, 2/2 slice to a whole-word token against the text with `"\x01ACTION "` and the trailing
   `"\x01"` removed, and 0/2 against the wrapped wire text. Example: wire
   `\x01ACTION take care everychat shachiOrcaLove\x01`, range `20-33` → stripped slice
   `shachiOrcaLove`, wrapped slice `erychat shachi`. This is why Task 2's strip must happen BEFORE
   Task 1's mapping, and why the two land in that order.
3. **Do NOT use `references/chatterino7/src/util/SampleData.cpp:180` as evidence.** Its ACTION sample
   (`emotes=86:30-39/822112:73-79`) is off by +1 against its own message text under every indexing —
   it is hand-edited UI sample data, not a wire capture. The real client code
   (`TwitchIrc.cpp` `codepointToUtf16Idx`) agrees with fact 1.

### File structure

| File | Responsibility | Tasks |
|---|---|---|
| `internal/twitch/chat_irc.go` | IRC session, line parsing, emote-tag parsing, keepalive | 1, 2, 7 |
| `internal/twitch/types.go` | `TwitchChatMessage.IsAction`, `TwitchChatData.EmoteOffsets`, `VodCommentEdge.Cursor` | 2, 3, 4 |
| `internal/twitch/chat.go` | const block (`chatResumeIDCap`, keepalive constants), `saveResumeState` | 6, 7 |
| `internal/twitch/delays.go` | NEW — injectable keepalive durations | 7 |
| `internal/twitch/chat_recording.go` | live writer (`writeFullChatFileTo`), `resolveEmotesCached` comment | 3, 5 |
| `internal/twitch/vod_chat.go` | VOD paging loop, VOD writer, VOD resume cap | 3, 4, 6 |
| `internal/twitch/api.go` | `GetVodComments` request/response shape | 4 |
| `internal/twitch/service.go` | `Service.GetVodComments` passthrough | 4 |
| `internal/twitch/emotes.go` | third-party fetchers, cache entries + TTL | 5 |
| `web/public/modules/chat-timeline.js` | NEW `correctLegacyTwitchEmotes` (pure, load-time) | 3 |
| `web/public/modules/player.js` | two call sites in `_fetchChatData` | 3 |
| `docs/spec/platform-services.md`, `docs/spec/data-and-storage.md` | living docs | 8 |

New/changed test files: `internal/twitch/chat_test.go` (extended), `internal/twitch/emotes_test.go`
(extended), `internal/twitch/chat_file_marker_test.go` (new), `internal/twitch/vod_chat_paging_test.go`
(new), `internal/twitch/chat_keepalive_test.go` (new), `internal/twitch/chat_resume_cap_test.go` (new),
`web/tests/chat-timeline.test.mjs` (extended), `web/tests/player.test.mjs` (extended).

### Planner's deviations from spec §4 (read before starting)

1. **§4.3 says the replay correction lives in `player.js` `_appendTwitchMessage` (≈:1782-1815). It
   cannot.** That method's signature is `_appendTwitchMessage(container, message, nativeEmotes)`
   (`web/public/modules/player.js:1782`) and both call sites pass only the message STRING and the
   emote array (`player.js:1150` → `this.appendChatContent(contentSpan, msg.message || [], msg.emotes)`
   and `player.js:1708` → the same for the overlay). It never sees `msg.raw`, and it never sees the
   chat file's header, so it cannot evaluate either half of the spec's gate. It is also a per-RENDER
   path — the niconico overlay rebuilds elements continuously — so a conversion there would run
   repeatedly over the same message. This plan instead corrects each file ONCE at load, in
   `_fetchChatData` (`player.js:994-1037`), beside the existing `deriveMissingOffsets` calls at
   `:1014` (per part) and `:1035` (single file), which is the exact seam the multi-part path needs:
   `mergePartChats` (`web/public/modules/chat-timeline.js:157-170`) builds a merged object carrying
   only `platform`/`streamStartTime`/`emotes`/`messages`, so a per-part header scalar has to be
   consumed before the merge. Every observable requirement of §4.3 is met (marker-gated, `raw`-gated,
   converted before any slicing) and the jsdom pin still drives `_appendTwitchMessage` end to end.
2. **§4 lists `chat-timeline.js` as in-scope "only if the header merge needs the new scalar".** The
   new pure function lands there anyway (beside `deriveMissingOffsets`, its exact twin) because that
   module is the DOM-free chat-file load-time normalizer with a plain `node --test` suite;
   `player.js` is DOM-coupled and only reachable through jsdom. `mergePartChats` itself is unchanged.
3. **Known bounded window (Task 3).** The marker is written by the two FULL-file writers. A part file
   created before this change and APPENDED to after it (a live job resumed across the upgrade into the
   same part) keeps its unmarked header while its new messages already carry UTF-16 offsets, so the
   player will over-shift those few messages. Closing it would mean either a whole-file rewrite on
   every legacy resume (a 50 MB read+write for a marathon part) or carrying a second "keep writing
   legacy offsets" mode through the parser. Both were rejected as worse than the defect. The window is
   one part of one job, closes at the next part roll or job end, and is documented in the struct
   comment and in `data-and-storage.md`.
4. **Anchor corrections** (ledger line numbers were taken at `5bbf16e8`; these are the verified ones):
   - `parseEmoteTags` is `internal/twitch/chat_irc.go:559-605` (the wrong-premise comment is `:559-564`).
     The ledger's ":565-608" is the function plus two lines of slack.
   - The ACTION site the ledger calls ":418-421" is `parsePrivmsg`'s message-text extraction at
     `internal/twitch/chat_irc.go:423-428`, with the emote call at `:443` (and the USERNOTICE twin at
     `:511`).
   - The `"two missed PINGs"` comment (T4-34) is `internal/twitch/chat_irc.go:266-271`, not `:269`.
   - The false "a failed resolve is not cached" comment (T1-11) is
     `internal/twitch/chat_recording.go:230-234`, not `:246` (`:246` is the closing brace).
   - The emote cache write (T1-11) is `internal/twitch/emotes.go:141-152`; the `total > 0` check the
     ledger points at is `:158-165`.
   - `internal/twitch/vod_chat.go:21` is `vodChatResumeMaxRecentIDs = 1000` — correct as cited.
   - `internal/twitch/api.go` VOD comment mapping is `:966-1021` (the ledger's ":975-990" is the
     fragment loop inside it); the request is `:915-919`.

---

### Task 1: Emote-tag offsets are code points, emitted as UTF-16

**Files:**
- Modify: `internal/twitch/chat_irc.go:1-15` (imports), `:559-605` (`parseEmoteTags` + its doc comment)
- Test: `internal/twitch/chat_test.go:233-264` (rewrite both existing cases, add three)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `parseEmoteTags(emotesStr, message string) []TwitchEmoteRef` — SAME signature. Contract
  change: `emotesStr` offsets are read as Unicode code points into `message`; the returned
  `TwitchEmoteRef.Start/End` are UTF-16 code-unit indices (inclusive) and `Name` is the code-point
  slice. A range that is out of bounds or inverted keeps today's behaviour: raw `Start`/`End` as sent,
  empty `Name`.

- [ ] **Step 1: Write the failing test**

Replace `internal/twitch/chat_test.go:233-264` (`TestParseEmoteTagsOutOfBounds` and
`TestParseEmoteTagsNonBMP`) with the block below. `TestParseEmoteTags` at `:146-231` is untouched — all
seven of its cases are pure ASCII, where code points and UTF-16 units coincide.

```go
func TestParseEmoteTagsOutOfBounds(t *testing.T) {
	// Emote position extends beyond message length — name should be empty and
	// the raw wire offsets survive untouched.
	refs := parseEmoteTags("25:0-99", "short")
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref, got %d", len(refs))
	}
	if refs[0].Name != "" {
		t.Errorf("expected empty name for out-of-bounds emote, got %q", refs[0].Name)
	}
	if refs[0].Start != 0 || refs[0].End != 99 {
		t.Errorf("out-of-bounds ref = (%d,%d), want the raw wire offsets (0,99)",
			refs[0].Start, refs[0].End)
	}
}

// TestParseEmoteTagsInvertedRange pins a range whose end precedes its start.
//
// Mutant: dropping the `start <= end` half of the bounds guard. The old code
// guarded only `start >= 0 && end < len(msgUnits)`, so "25:4-2" on "Kappa"
// evaluated msgUnits[4:3] and PANICKED — inside the IRC read loop, which would
// take the whole chat session down on one malformed tag.
func TestParseEmoteTagsInvertedRange(t *testing.T) {
	refs := parseEmoteTags("25:4-2", "Kappa")
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref, got %d", len(refs))
	}
	if refs[0].Name != "" || refs[0].Start != 4 || refs[0].End != 2 {
		t.Errorf("inverted ref = {Name:%q Start:%d End:%d}, want {\"\" 4 2}",
			refs[0].Name, refs[0].Start, refs[0].End)
	}
}

// TestParseEmoteTagsNonBMP pins THE wire fact: Twitch's emote-tag offsets count
// Unicode CODE POINTS, and the Start/End we emit are UTF-16 code units because
// player.js slices with String.prototype.substring and the VOD path (api.go,
// utf16Len) already emits UTF-16.
//
// Verified 2026-09-15 over the raw IRC lines of 18 real Twitch chat archives:
// of 120 emote ranges preceded by a non-BMP character, 120 are whole-word
// tokens under code-point slicing and 0 under UTF-16 slicing. chatterino7
// (codepointToUtf16Idx), gempir/go-twitch-irc ([]rune slicing) and
// robotty/twitch-irc-rs (chars().skip().take()) all index by code point.
//
// Mutants this kills:
//   - raw pass-through ("Twitch sends UTF-16 already"): Start would be 2, not 3.
//   - UTF-16-indexed slicing (the behaviour before this arc, commit 5031cd2b):
//     Name would be " Kapp" and Start 2.
//   - forgetting the End sentinel (End = cpToUnit[end], not cpToUnit[end+1]-1):
//     End would be 6, clipping the last character off every emote span.
func TestParseEmoteTagsNonBMP(t *testing.T) {
	for _, tc := range []struct {
		name      string
		emotesStr string
		message   string
		want      TwitchEmoteRef
	}{
		{
			// "🎉 Kappa": 🎉 is code point 0 (UTF-16 [0..1]), space code point 1
			// (UTF-16 [2]), "Kappa" code points 2..6 (UTF-16 [3..7]).
			name:      "emoji then emote",
			emotesStr: "25:2-6",
			message:   "🎉 Kappa",
			want:      TwitchEmoteRef{ID: "25", Name: "Kappa", Start: 3, End: 7},
		},
		{
			// A real captured line: wire text "🤘 shachiOrcaWail", range 2-15.
			name:      "real wire range",
			emotesStr: "301428:2-15",
			message:   "🤘 shachiOrcaWail",
			want:      TwitchEmoteRef{ID: "301428", Name: "shachiOrcaWail", Start: 3, End: 16},
		},
		{
			// Two non-BMP characters before the emote: the drift is per code
			// point, so a single-character fudge factor cannot pass this.
			name:      "two emoji then emote",
			emotesStr: "25:4-8",
			message:   "🤘🤝 x Kappa",
			want:      TwitchEmoteRef{ID: "25", Name: "Kappa", Start: 6, End: 10},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refs := parseEmoteTags(tc.emotesStr, tc.message)
			if len(refs) != 1 {
				t.Fatalf("expected 1 ref, got %d", len(refs))
			}
			if refs[0] != tc.want {
				t.Errorf("parseEmoteTags(%q, %q) = %+v, want %+v",
					tc.emotesStr, tc.message, refs[0], tc.want)
			}
			// The emitted span must be sliceable by JavaScript's own
			// substring(start, end+1) — which is what player.js does.
			units := utf16.Encode([]rune(tc.message))
			if got := string(utf16.Decode(units[refs[0].Start : refs[0].End+1])); got != tc.want.Name {
				t.Errorf("UTF-16 slice [%d:%d] = %q, want %q — the emitted span does not "+
					"select the emote in player.js's index space",
					refs[0].Start, refs[0].End+1, got, tc.want.Name)
			}
		})
	}
}
```

Add `"unicode/utf16"` to `internal/twitch/chat_test.go`'s import block (the production file drops it in
Step 3; the test keeps it for the independent slice check).

- [ ] **Step 2: Run the test to verify it fails**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestParseEmoteTags' ./internal/twitch/`

Expected: FAIL. `TestParseEmoteTagsNonBMP/emoji_then_emote` reports
`= {ID:25 Name: Kapp Start:2 End:6}, want {ID:25 Name:Kappa Start:3 End:7}`;
`TestParseEmoteTagsInvertedRange` fails with `panic: runtime error: slice bounds out of range [4:3]`;
`TestParseEmoteTagsOutOfBounds` passes already.

- [ ] **Step 3: Replace `parseEmoteTags`**

Replace `internal/twitch/chat_irc.go:559-605` entirely with:

```go
// parseEmoteTags parses IRC emote tags like "id:start-end,start-end/id:start-end".
//
// TWO INDEX SPACES, and the whole point of this function is the conversion
// between them.
//
// The WIRE offsets count Unicode CODE POINTS of the PRIVMSG text — inclusive,
// zero-based. Not bytes, and NOT UTF-16 code units: this file asserted UTF-16
// from 2026-04-22 (commit 5031cd2b) until this arc, and the claim was wrong.
// It was re-measured 2026-09-15 over the raw IRC lines of 18 real archives: of
// 120 ranges preceded by a non-BMP character, 120 slice to a whole-word token
// by code point and 0 by UTF-16. Every reference client agrees —
// references/chatterino7/src/providers/twitch/TwitchIrc.cpp (codepointToUtf16Idx),
// gempir/go-twitch-irc ([]rune slicing), robotty/twitch-irc-rs
// (chars().skip().take()).
//
// The EMITTED Start/End count UTF-16 code units, because the only consumer is
// JavaScript: player.js renders the span with String.prototype.substring
// (web/public/modules/player.js, _appendTwitchMessage), and the VOD path emits
// UTF-16 already (utf16Len, api.go). Emitting the wire offsets unchanged would
// make the two producers disagree about what a chat file's offsets mean.
//
// So: read by code point, write by UTF-16, and Name comes from the code-point
// slice. For messages with no non-BMP character the two spaces coincide and
// nothing moves.
//
// A range that is inverted or runs past the end of the message is NOT a fatal
// input: the raw wire Start/End are passed through with an empty Name, exactly
// as before, so a malformed tag costs one unrendered emote rather than the
// session. The `start <= end` half of the guard is load-bearing — without it
// an inverted range slices backwards and panics inside the read loop.
func parseEmoteTags(emotesStr, message string) []TwitchEmoteRef {
	if emotesStr == "" {
		return nil
	}

	runes := []rune(message)
	// cpToUnit[i] is the UTF-16 index at which code point i begins. The extra
	// entry at len(runes) holds the message's total UTF-16 length, which is
	// what makes End computable as cpToUnit[end+1]-1 with no special case for
	// a range that ends on the last code point.
	cpToUnit := make([]int, len(runes)+1)
	units := 0
	for i, r := range runes {
		cpToUnit[i] = units
		if r >= 0x10000 {
			units += 2 // surrogate pair
		} else {
			units++
		}
	}
	cpToUnit[len(runes)] = units

	var refs []TwitchEmoteRef
	for group := range strings.SplitSeq(emotesStr, "/") {
		emoteID, positions, ok := strings.Cut(group, ":")
		if !ok {
			continue
		}

		for pos := range strings.SplitSeq(positions, ",") {
			startStr, endStr, ok := strings.Cut(pos, "-")
			if !ok {
				continue
			}
			start, err1 := strconv.Atoi(startStr)
			end, err2 := strconv.Atoi(endStr)
			if err1 != nil || err2 != nil {
				continue
			}

			ref := TwitchEmoteRef{ID: emoteID, Start: start, End: end}
			if start >= 0 && start <= end && end < len(runes) {
				ref.Name = string(runes[start : end+1])
				ref.Start = cpToUnit[start]
				ref.End = cpToUnit[end+1] - 1
			}
			refs = append(refs, ref)
		}
	}

	return refs
}
```

Then drop the now-unused `"unicode/utf16"` line from the import block at
`internal/twitch/chat_irc.go:3-15` (staticcheck rejects an unused import).

- [ ] **Step 4: Run the tests to verify they pass**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/twitch/`

Expected: PASS (`ok github.com/vampiricwulf/Moombox/internal/twitch`).

- [ ] **Step 5: Commit**

```bash
git add internal/twitch/chat_irc.go internal/twitch/chat_test.go
git commit -m "fix(twitch): IRC emote offsets are code points, emitted as UTF-16

Twitch's emotes= offsets index Unicode code points, not UTF-16 code
units. Re-measured over 120 real wire ranges: 120/120 slice to a whole
word by code point, 0/120 by UTF-16. parseEmoteTags now reads by code
point and emits UTF-16 spans for player.js's substring(), and refuses an
inverted range instead of panicking on it.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq"
```

---

### Task 2: `/me` ACTION wrapper stripped before parsing, reported as `isAction`

**Files:**
- Modify: `internal/twitch/types.go:54-73` (`TwitchChatMessage`)
- Modify: `internal/twitch/chat_irc.go:405-455` (`parsePrivmsg`)
- Test: `internal/twitch/chat_test.go` (append)

**Interfaces:**
- Consumes: `parseEmoteTags(emotesStr, message string) []TwitchEmoteRef` from Task 1 — offsets are
  code points into the message it is GIVEN, so it must be given the stripped text.
- Produces:
  - `TwitchChatMessage.IsAction bool` with JSON tag `isAction,omitempty` (field placed immediately
    after `MessageType`).
  - `stripActionWrapper(text string) (stripped string, isAction bool)` in `chat_irc.go`.

- [ ] **Step 1: Write the failing test**

Append to `internal/twitch/chat_test.go`:

```go
// TestParsePrivmsgStripsActionWrapper pins the /me shape end to end.
//
// A "/me" chat line reaches us as the CTCP form \x01ACTION <text>\x01, and the
// emote offsets index the STRIPPED text — verified 2026-09-15 against the two
// real ACTION ranges in the archives: both slice to a whole word after the
// wrapper is removed, neither does against the wrapped text.
//
// Mutants this kills:
//   - no strip at all (today): Message keeps "\x01ACTION Kappa\x01" and the
//     emote lands on "ACTION" — offsets shift by the wrapper's 8 code points.
//   - stripping AFTER parseEmoteTags: Message is right and the emote span is
//     still 8 units too far along.
//   - stripping only the prefix: the trailing \x01 renders as a stray glyph and
//     the message length is one too long.
//   - stripping on any message that merely CONTAINS the marker: the third case
//     below sends "\x01ACTION" mid-text and must come through untouched.
func TestParsePrivmsgStripsActionWrapper(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{
		ChannelLogin: "testchan",
		StreamID:     "stream-1",
	}, &testLogger{})

	const soh = "\x01"
	for _, tc := range []struct {
		name       string
		tags       string
		body       string
		wantText   string
		wantAction bool
		wantEmotes []TwitchEmoteRef
	}{
		{
			name:       "action with an emote at the head",
			tags:       "emotes=25:0-4;id=m1;tmi-sent-ts=1700000000000;user-id=u1;display-name=Viewer",
			body:       soh + "ACTION Kappa" + soh,
			wantText:   "Kappa",
			wantAction: true,
			wantEmotes: []TwitchEmoteRef{{ID: "25", Name: "Kappa", Start: 0, End: 4}},
		},
		{
			name:       "real wire action range",
			tags:       "emotes=301428:20-33;id=m2;tmi-sent-ts=1700000000000;user-id=u1;display-name=Viewer",
			body:       soh + "ACTION take care everychat shachiOrcaLove" + soh,
			wantText:   "take care everychat shachiOrcaLove",
			wantAction: true,
			wantEmotes: []TwitchEmoteRef{{ID: "301428", Name: "shachiOrcaLove", Start: 20, End: 33}},
		},
		{
			name:       "ordinary message is untouched",
			tags:       "emotes=25:0-4;id=m3;tmi-sent-ts=1700000000000;user-id=u1;display-name=Viewer",
			body:       "Kappa hello",
			wantText:   "Kappa hello",
			wantAction: false,
			wantEmotes: []TwitchEmoteRef{{ID: "25", Name: "Kappa", Start: 0, End: 4}},
		},
		{
			name:       "the marker mid-text is not a wrapper",
			tags:       "id=m4;tmi-sent-ts=1700000000000;user-id=u1;display-name=Viewer",
			body:       "look: " + soh + "ACTION is the CTCP form",
			wantText:   "look: " + soh + "ACTION is the CTCP form",
			wantAction: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line := "@" + tc.tags + " :viewer!viewer@viewer.tmi.twitch.tv PRIVMSG #testchan :" + tc.body
			msg := cd.parseLine(line)
			if msg == nil {
				t.Fatal("parseLine returned nil for a well-formed PRIVMSG")
			}
			if msg.Message != tc.wantText {
				t.Errorf("Message = %q, want %q", msg.Message, tc.wantText)
			}
			if msg.IsAction != tc.wantAction {
				t.Errorf("IsAction = %v, want %v", msg.IsAction, tc.wantAction)
			}
			if len(msg.Emotes) != len(tc.wantEmotes) {
				t.Fatalf("Emotes = %+v, want %+v", msg.Emotes, tc.wantEmotes)
			}
			for i := range tc.wantEmotes {
				if msg.Emotes[i] != tc.wantEmotes[i] {
					t.Errorf("Emotes[%d] = %+v, want %+v", i, msg.Emotes[i], tc.wantEmotes[i])
				}
			}
			// The lossless line is the archive's only record of what Twitch
			// actually sent, so the wrapper must still be in it.
			if msg.Raw != line {
				t.Errorf("Raw was rewritten; it must stay the verbatim wire line")
			}
		})
	}
}

// TestActionIsNotStrippedFromUsernotice pins the narrowness of the strip.
// USERNOTICE bodies (subs, raids, announcements) are never CTCP-wrapped, and a
// strip there would silently eat the head of any system message that happened
// to start with the marker.
func TestActionIsNotStrippedFromUsernotice(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{
		ChannelLogin: "testchan",
		StreamID:     "stream-1",
	}, &testLogger{})
	const soh = "\x01"
	line := "@id=u1;tmi-sent-ts=1700000000000;msg-id=sub;display-name=Viewer;user-id=u1 " +
		":tmi.twitch.tv USERNOTICE #testchan :" + soh + "ACTION Kappa" + soh
	msg := cd.parseLine(line)
	if msg == nil {
		t.Fatal("parseLine returned nil for a well-formed USERNOTICE")
	}
	if msg.Message != soh+"ACTION Kappa"+soh {
		t.Errorf("USERNOTICE body = %q, want it verbatim", msg.Message)
	}
	if msg.IsAction {
		t.Error("IsAction set on a USERNOTICE — the strip must be PRIVMSG-only")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestParsePrivmsgStripsActionWrapper|TestActionIsNotStrippedFromUsernotice' ./internal/twitch/`

Expected: FAIL to COMPILE — `msg.IsAction undefined (type *TwitchChatMessage has no field or method IsAction)`.

- [ ] **Step 3: Add the field**

In `internal/twitch/types.go`, insert immediately after the `MessageType` line (`:66`):

```go
	// IsAction marks a /me message. Twitch sends those as the CTCP form
	// \x01ACTION <text>\x01; parsePrivmsg unwraps them so Message holds only
	// the text and the emote offsets (which index the UNWRAPPED text — see
	// parseEmoteTags) line up. Raw keeps the verbatim wire line.
	IsAction bool `json:"isAction,omitempty"`
```

- [ ] **Step 4: Strip the wrapper before parsing**

In `internal/twitch/chat_irc.go`, replace the message-text extraction in `parsePrivmsg` (`:423-428`):

```go
	// Extract message text (after the last ':')
	var messageText string
	if len(parts) >= 4 {
		messageText = parts[3]
		messageText = strings.TrimPrefix(messageText, ":")
	}
```

with:

```go
	// Extract message text (after the last ':')
	var messageText string
	if len(parts) >= 4 {
		messageText = parts[3]
		messageText = strings.TrimPrefix(messageText, ":")
	}

	// Unwrap /me BEFORE the emote tags are read. The offsets index the
	// unwrapped text — measured 2026-09-15 on real ACTION lines — so parsing
	// against the wrapped form lands every emote eight code points early and
	// renders the word "ACTION" as part of the message.
	messageText, isAction := stripActionWrapper(messageText)
```

and add `IsAction: isAction,` to the `TwitchChatMessage` literal at `:435-452`, immediately after the
`MessageType: msgType,` line.

Then add, directly above `parseEmoteTags` (i.e. before the comment block that now starts at `:559`):

```go
// stripActionWrapper unwraps the CTCP form Twitch sends a /me message in:
// \x01ACTION <text>\x01. It returns the text and whether it was wrapped.
//
// PRIVMSG only, and only as a whole-value wrapper: the prefix must be at the
// very start, and the trailing \x01 is removed only when the prefix matched.
// A chat line that merely mentions the marker mid-text is an ordinary message,
// and a USERNOTICE body is never wrapped at all.
func stripActionWrapper(text string) (string, bool) {
	rest, ok := strings.CutPrefix(text, "\x01ACTION ")
	if !ok {
		return text, false
	}
	return strings.TrimSuffix(rest, "\x01"), true
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/twitch/`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/twitch/chat_irc.go internal/twitch/types.go internal/twitch/chat_test.go
git commit -m "fix(twitch): unwrap /me ACTION messages before parsing emotes

Twitch sends /me as the CTCP form \\x01ACTION <text>\\x01 and its emote
offsets index the UNWRAPPED text (verified on real wire lines), so the
wrapper is stripped in parsePrivmsg before parseEmoteTags runs. The fact
is reported as TwitchChatMessage.IsAction (isAction); the raw field
still carries the verbatim wire line.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq"
```

---

### Task 3: `emoteOffsets` file marker + replay correction for legacy files

**Files:**
- Modify: `internal/twitch/types.go:83-99` (`TwitchChatData`)
- Modify: `internal/twitch/chat.go:20-48` (const block — add `chatEmoteOffsetsUTF16`)
- Modify: `internal/twitch/chat_recording.go:119-137` (`writeFullChatFileTo`)
- Modify: `internal/twitch/vod_chat.go:379-391` (`writeFullFile`)
- Create: `internal/twitch/chat_file_marker_test.go`
- Modify: `web/public/modules/chat-timeline.js:1-23` (module doc), append `correctLegacyTwitchEmotes`
- Modify: `web/public/modules/player.js:1014`, `:1035` (two call sites), `:1-40` (import list)
- Test: `web/tests/chat-timeline.test.mjs` (imports + append), `web/tests/player.test.mjs` (append)

**Interfaces:**
- Consumes: `TwitchChatMessage.IsAction` (Task 2), `parseEmoteTags`'s UTF-16 output (Task 1).
- Produces:
  - `TwitchChatData.EmoteOffsets string` with JSON tag `emoteOffsets,omitempty`, positioned AFTER
    `MessageCount` and BEFORE `Emotes`/`Messages`.
  - `chatEmoteOffsetsUTF16 = "utf16"` in `internal/twitch/chat.go`.
  - `correctLegacyTwitchEmotes(data)` exported from `web/public/modules/chat-timeline.js`; mutates
    `data.messages` in place and returns `data`.

- [ ] **Step 1: Write the failing Go test**

Create `internal/twitch/chat_file_marker_test.go`:

```go
package twitch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// readChatHeader decodes just the scalars of a chat file.
func readChatHeader(t *testing.T, path string) TwitchChatData {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var data TwitchChatData
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return data
}

// TestLiveChatFileCarriesTheOffsetMarker pins the marker on the IRC writer.
//
// Mutant: writing the field but leaving it empty (or omitting it from the
// literal). player.js reads its ABSENCE as "this file predates the code-point
// fix" and re-shifts every emote span in it, so an unmarked new file renders
// worse than an unfixed old one.
func TestLiveChatFileCarriesTheOffsetMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat.json")
	cd := NewChatDownloader(ChatDownloaderOptions{
		ChannelLogin: "testchan",
		StreamID:     "stream-1",
		OutputPath:   path,
	}, &testLogger{})

	msgs := []TwitchChatMessage{{ID: "m1", TimestampMs: 1700000000000, Message: "Kappa"}}
	if err := cd.writeFullChatFileTo(path, msgs, len(msgs), 0); err != nil {
		t.Fatalf("writeFullChatFileTo: %v", err)
	}
	if got := readChatHeader(t, path).EmoteOffsets; got != "utf16" {
		t.Errorf("emoteOffsets = %q, want %q", got, "utf16")
	}
}

// TestVodChatFileCarriesTheOffsetMarker is the VOD twin. The VOD path always
// emitted UTF-16 (api.go utf16Len), so its files must be marked too — an
// unmarked one would be corrected a second time at replay.
func TestVodChatFileCarriesTheOffsetMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vod.json")
	vcd := NewVodChatDownloader(NewAPI(&testLogger{}), VodChatOptions{
		VodID:      "v1",
		OutputPath: path,
	}, &testLogger{})
	if err := vcd.writeFullFile([]TwitchChatMessage{{ID: "c1", Message: "Kappa"}}); err != nil {
		t.Fatalf("writeFullFile: %v", err)
	}
	if got := readChatHeader(t, path).EmoteOffsets; got != "utf16" {
		t.Errorf("emoteOffsets = %q, want %q", got, "utf16")
	}
}

// TestMarkedChatFileStaysAppendableAndReadable pins the three readers that walk
// a chat file's header against the NEW scalar. The header's field order is
// already load-bearing (AppendChatMessages splices at the last ']', so the
// messages array must stay last), and this is the regression that would prove
// the new field broke it.
//
// Mutants: placing EmoteOffsets AFTER Messages in the struct (the append then
// splices into the scalar and the file stops parsing); a header reader that
// treats an unrecognised key as end-of-header (recordingStartTime would go
// missing and every resumed part's offsets would rebase onto the restart).
func TestMarkedChatFileStaysAppendableAndReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat.json")
	cd := NewChatDownloader(ChatDownloaderOptions{
		ChannelLogin: "testchan",
		StreamID:     "stream-1",
		OutputPath:   path,
	}, &testLogger{})

	const baseMs = int64(1700000000000)
	first := []TwitchChatMessage{{ID: "m1", TimestampMs: baseMs, Message: "one"}}
	if err := cd.writeFullChatFileTo(path, first, 1, baseMs); err != nil {
		t.Fatalf("writeFullChatFileTo: %v", err)
	}

	second := []TwitchChatMessage{{ID: "m2", TimestampMs: baseMs + 1000, Message: "two"}}
	if err := utils.AppendChatMessages(path, second, 2, cd.logger); err != nil {
		t.Fatalf("AppendChatMessages against a marked file: %v", err)
	}

	data := readChatHeader(t, path)
	if data.EmoteOffsets != "utf16" {
		t.Errorf("emoteOffsets after append = %q, want %q", data.EmoteOffsets, "utf16")
	}
	if len(data.Messages) != 2 || data.Messages[0].ID != "m1" || data.Messages[1].ID != "m2" {
		t.Fatalf("messages after append = %+v, want m1 then m2", data.Messages)
	}

	gotBase, ok, err := chatFileRecordingBaseMs(path)
	if err != nil {
		t.Fatalf("chatFileRecordingBaseMs: %v", err)
	}
	if !ok || gotBase != baseMs {
		t.Errorf("chatFileRecordingBaseMs = (%d, %v), want (%d, true) — the marker hid the "+
			"recording base from the header scan", gotBase, ok, baseMs)
	}

	summary, err := readChatPartFileSummary(path)
	if err != nil {
		t.Fatalf("readChatPartFileSummary against a marked file: %v", err)
	}
	if summary.messages != 2 {
		t.Errorf("summary.messages = %d, want 2", summary.messages)
	}
	if strings.Join(summary.recentIDs, ",") != "m1,m2" {
		t.Errorf("summary.recentIDs = %v, want [m1 m2]", summary.recentIDs)
	}
}
```

- [ ] **Step 2: Run the Go test to verify it fails**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'OffsetMarker|MarkedChatFile' ./internal/twitch/`

Expected: FAIL to COMPILE — `data.EmoteOffsets undefined (type TwitchChatData has no field or method EmoteOffsets)`.

- [ ] **Step 3: Add the field and the two writers**

In `internal/twitch/types.go`, inside `TwitchChatData`, insert between `MessageCount` (`:92`) and the
`// Emotes must serialize BEFORE Messages` comment:

```go
	// EmoteOffsets names the index space TwitchEmoteRef.Start/End count in.
	// "utf16" is the only value any writer here produces, and its ABSENCE is
	// what the player reads as "written before 2026-09-15, when the live IRC
	// path mistook Twitch's code-point offsets for UTF-16 units" — an unmarked
	// file's IRC messages are corrected at load (correctLegacyTwitchEmotes,
	// web/public/modules/chat-timeline.js).
	//
	// It is a HEADER SCALAR and must stay one, before "emotes"/"messages":
	// chatFileRecordingBaseMs stops its scan at the first composite value, and
	// AppendChatMessages splices at the file's last ']'.
	//
	// KNOWN WINDOW: only the two full-file writers set it, so a part file
	// created before this change and APPENDED to after it keeps an unmarked
	// header while its new messages already carry UTF-16 offsets. Those few
	// messages are over-shifted at replay. Rewriting a marathon part's whole
	// file on resume, or carrying a second "keep emitting legacy offsets"
	// parser mode, both cost more than the defect; the window closes at the
	// next part roll or at job end.
	EmoteOffsets string `json:"emoteOffsets,omitempty"`
```

In `internal/twitch/chat.go`, add to the const block (after `chatCorruptSuffix`, `:47`):

```go
	// chatEmoteOffsetsUTF16 is the only value TwitchChatData.EmoteOffsets ever
	// carries. One constant with two writers — the IRC full-file write and the
	// VOD one — because a file written with the marker misspelled is
	// indistinguishable at replay from a legacy file, and would be "corrected"
	// a second time.
	chatEmoteOffsetsUTF16 = "utf16"
```

In `internal/twitch/chat_recording.go`, add to the `TwitchChatData` literal in `writeFullChatFileTo`
(`:123-132`), immediately after `MessageCount: count,`:

```go
		EmoteOffsets:       chatEmoteOffsetsUTF16,
```

In `internal/twitch/vod_chat.go`, add the same line to the literal in `writeFullFile` (`:381-389`),
immediately after `MessageCount: int(vcd.totalCount.Load()),`:

```go
		EmoteOffsets:       chatEmoteOffsetsUTF16,
```

- [ ] **Step 4: Run the Go tests to verify they pass**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/twitch/`

Expected: PASS.

- [ ] **Step 5: Write the failing JS unit test**

In `web/tests/chat-timeline.test.mjs`, add `correctLegacyTwitchEmotes` to the import list at `:4-7`,
and append:

```js
// ── Legacy Twitch emote-offset correction ───────────────────────────────────

const SOH = "\u0001";

/** One live-IRC message: `raw` is what marks it IRC-recorded rather than VOD. */
const ircMsg = (message, emotes) => ({
  offsetMs: 0, authorName: "u", message, emotes,
  raw: `@emotes=x :u!u@u.tmi.twitch.tv PRIVMSG #c :${message}`,
});

test("correctLegacyTwitchEmotes: a marked file is left exactly as written", () => {
  // Mutant: correcting on the strength of `raw` alone. Every IRC message has
  // `raw`, so that mutant re-shifts every span in every NEW file — the marker
  // is the only thing that separates the two eras.
  const data = {
    platform: "twitch", emoteOffsets: "utf16",
    messages: [ircMsg("🎉 Kappa", [{ id: "25", name: "Kappa", start: 3, end: 7 }])],
  };
  correctLegacyTwitchEmotes(data);
  assert.deepEqual(data.messages[0].emotes, [{ id: "25", name: "Kappa", start: 3, end: 7 }]);
  assert.equal(data.messages[0].message, "🎉 Kappa");
});

test("correctLegacyTwitchEmotes: an unmarked IRC message is mapped code point → UTF-16", () => {
  // The legacy producer stored the WIRE (code-point) offsets and a name sliced
  // out of them in UTF-16 space, i.e. " Kapp". Both have to be repaired.
  // Mutant: mapping start but not end (end = cpToUnit[end]) clips the span to
  // "Kapp"; mutant: leaving `name` alone renders the garbled " Kapp" as the
  // emote's alt text.
  const data = {
    platform: "twitch",
    messages: [ircMsg("🎉 Kappa", [{ id: "25", name: " Kapp", start: 2, end: 6 }])],
  };
  correctLegacyTwitchEmotes(data);
  assert.deepEqual(data.messages[0].emotes, [{ id: "25", name: "Kappa", start: 3, end: 7 }]);
  assert.equal(data.messages[0].message.substring(3, 8), "Kappa");
});

test("correctLegacyTwitchEmotes: an unmarked VOD comment is untouched", () => {
  // No `raw` = it came from the GQL VOD path, which emitted UTF-16 all along.
  // Mutant: correcting every message in an unmarked file shifts every VOD
  // archive's emotes the wrong way.
  const vod = { offsetMs: 0, authorName: "u", message: "🎉 Kappa",
    emotes: [{ id: "25", name: "Kappa", start: 3, end: 7 }] };
  const data = { platform: "twitch", messages: [vod] };
  correctLegacyTwitchEmotes(data);
  assert.deepEqual(data.messages[0].emotes, [{ id: "25", name: "Kappa", start: 3, end: 7 }]);
});

test("correctLegacyTwitchEmotes: an unmarked /me is unwrapped and re-indexed", () => {
  // Legacy stored the wrapped text with offsets that index the STRIPPED text.
  // Mutant: unwrapping without re-indexing leaves the span 8 units off the end
  // of a short message and the emote vanishes.
  const data = {
    platform: "twitch",
    messages: [ircMsg(`${SOH}ACTION 🎉 Kappa${SOH}`, [{ id: "25", name: "x", start: 2, end: 6 }])],
  };
  correctLegacyTwitchEmotes(data);
  assert.equal(data.messages[0].message, "🎉 Kappa");
  assert.equal(data.messages[0].isAction, true);
  assert.deepEqual(data.messages[0].emotes, [{ id: "25", name: "Kappa", start: 3, end: 7 }]);
});

test("correctLegacyTwitchEmotes: out-of-range and inverted spans are left alone", () => {
  // The Go producer passes a malformed wire range through untouched; the
  // corrector must not invent a span for it (a negative slice end would make
  // `name` "" and hide the text after it).
  const data = {
    platform: "twitch",
    messages: [ircMsg("short", [
      { id: "25", name: "", start: 0, end: 99 },
      { id: "26", name: "", start: 4, end: 2 },
    ])],
  };
  correctLegacyTwitchEmotes(data);
  assert.deepEqual(data.messages[0].emotes, [
    { id: "25", name: "", start: 0, end: 99 },
    { id: "26", name: "", start: 4, end: 2 },
  ]);
});

test("correctLegacyTwitchEmotes: a YouTube file is not its business", () => {
  const data = { messages: [{ offsetMs: 0, message: [{ text: "hi" }] }] };
  correctLegacyTwitchEmotes(data);
  assert.deepEqual(data.messages[0].message, [{ text: "hi" }]);
});
```

- [ ] **Step 6: Run the JS test to verify it fails**

Run: `cd web/tests && node --test chat-timeline.test.mjs`

Expected: FAIL — `SyntaxError: The requested module '../public/modules/chat-timeline.js' does not
provide an export named 'correctLegacyTwitchEmotes'`.

- [ ] **Step 7: Implement `correctLegacyTwitchEmotes`**

Append to `web/public/modules/chat-timeline.js`:

```js
/** The CTCP wrapper Twitch sends a /me message in: \x01ACTION <text>\x01. */
const TWITCH_ACTION_PREFIX = "\u0001ACTION ";
const TWITCH_SOH = "\u0001";

/**
 * Repair a Twitch chat file written before 2026-09-15, in place.
 *
 * Until then the live IRC producer read Twitch's emote offsets — which count
 * Unicode CODE POINTS — as if they were UTF-16 code units, and left /me
 * messages wrapped in \x01ACTION …\x01. Files written since carry the header
 * scalar `emoteOffsets: "utf16"` (Go: TwitchChatData.EmoteOffsets), so its
 * ABSENCE is the era marker and this function is the era's reader.
 *
 * Three gates, each load-bearing:
 * - `platform === "twitch"`: a YouTube chat file has no such offsets.
 * - `emoteOffsets !== "utf16"`: a marked file is already right, and correcting
 *   it a second time would shift every span the other way.
 * - per message, `raw`: only the IRC path records the verbatim wire line, so it
 *   is how a legacy file's IRC messages are told from its VOD comments — the
 *   VOD path (api.go utf16Len) emitted UTF-16 from the start and must not move.
 *
 * Run ONCE per file at load, before any rendering and before mergePartChats
 * (which keeps no header scalars, so a multi-part job has to correct each part
 * against its own header).
 *
 * Returns the same object, mutated in place.
 * @param {{platform?:string, emoteOffsets?:string, messages?:Array}} data
 */
export function correctLegacyTwitchEmotes(data) {
  if (!data || data.platform !== "twitch" || data.emoteOffsets === "utf16") return data;
  for (const m of data.messages || []) {
    if (!m || !m.raw) continue;
    let text = typeof m.message === "string" ? m.message : "";
    if (text.startsWith(TWITCH_ACTION_PREFIX)) {
      text = text.slice(TWITCH_ACTION_PREFIX.length);
      if (text.endsWith(TWITCH_SOH)) text = text.slice(0, -1);
      m.message = text;
      m.isAction = true;
    }
    const emotes = m.emotes;
    if (!Array.isArray(emotes) || emotes.length === 0) continue;
    // cpToUnit[i] is the UTF-16 index at which code point i begins; the
    // sentinel at the end holds the total length, so `end` maps without a
    // special case for a span that reaches the last character.
    const cps = [...text];
    const cpToUnit = new Array(cps.length + 1);
    let units = 0;
    for (let i = 0; i < cps.length; i++) {
      cpToUnit[i] = units;
      units += cps[i].length; // 1, or 2 for a surrogate pair
    }
    cpToUnit[cps.length] = units;
    for (const e of emotes) {
      const s = Number(e.start);
      const en = Number(e.end);
      // Same bounds rule as the Go producer: a malformed wire range keeps the
      // offsets it was sent with and renders as plain text.
      if (!Number.isInteger(s) || !Number.isInteger(en)) continue;
      if (s < 0 || s > en || en >= cps.length) continue;
      e.name = cps.slice(s, en + 1).join("");
      e.start = cpToUnit[s];
      e.end = cpToUnit[en + 1] - 1;
    }
  }
  return data;
}
```

Also widen the module doc's opening line (`web/public/modules/chat-timeline.js:1-3`) from

```js
/**
 * Chat ↔ video timeline math. Pure — no DOM, no fetch — so it is covered by
 * web/tests/chat-timeline.test.mjs.
```

to

```js
/**
 * Chat-file load-time normalization: timeline math, plus the legacy Twitch
 * emote-offset repair. Pure — no DOM, no fetch — so it is covered by
 * web/tests/chat-timeline.test.mjs.
```

- [ ] **Step 8: Run the JS test to verify it passes**

Run: `cd web/tests && node --test chat-timeline.test.mjs`

Expected: PASS.

- [ ] **Step 9: Wire it into the player's two load paths**

In `web/public/modules/player.js`, add `correctLegacyTwitchEmotes` to the existing
`./modules/chat-timeline.js` import list (the same import that already brings in `deriveMissingOffsets`,
`mergePartChats`, `normalizeOffsetMs`, …).

Replace the per-part call at `:1008-1014`:

```js
          // Per part, against the PART's own header epoch — before
          // mergePartChats shifts it onto the global timeline (one file, one
          // epoch). Skipped for the Twitch parts this path normally serves:
          // a Twitch part's offsets are already video-relative and its header
          // epoch is the recording start, so nothing here may touch them. It
          // earns its keep on a legacy YouTube part that has a header epoch.
          if (data && data.platform !== "twitch") deriveMissingOffsets(data.messages, data.streamStartTime);
```

with:

```js
          // Per part, against the PART's own header — before mergePartChats
          // shifts it onto the global timeline (one file, one epoch, and the
          // merge keeps no header scalars at all).
          //
          // YouTube: recover the offset of a message the producer left without
          // one, from the header epoch. Twitch parts are skipped there — their
          // offsets are already video-relative and their header epoch is the
          // recording start — and get the other repair instead: a part written
          // before the emote-offset fix carries code-point spans and wrapped
          // /me text, which correctLegacyTwitchEmotes maps into the UTF-16
          // space _appendTwitchMessage slices in.
          if (data && data.platform === "twitch") correctLegacyTwitchEmotes(data);
          else if (data) deriveMissingOffsets(data.messages, data.streamStartTime);
```

Replace the single-file call at `:1030-1035`:

```js
    // A message the producer left without an offset of its own (offsetMs 0
    // and no hasOffset) is recovered from the header epoch before the caller
    // applies the bias and sorts (T-F12); one that already carries a real
    // offset is authoritative and untouched. Twitch files are skipped
    // outright — their offsets are already video-relative (F1).
    if (data && data.platform !== "twitch") deriveMissingOffsets(data.messages, data.streamStartTime);
```

with:

```js
    // A message the producer left without an offset of its own (offsetMs 0
    // and no hasOffset) is recovered from the header epoch before the caller
    // applies the bias and sorts (T-F12); one that already carries a real
    // offset is authoritative and untouched. Twitch files are skipped
    // outright — their offsets are already video-relative (F1) — and take the
    // legacy emote-offset repair instead (see the per-part branch above).
    if (data && data.platform === "twitch") correctLegacyTwitchEmotes(data);
    else if (data) deriveMissingOffsets(data.messages, data.streamStartTime);
```

- [ ] **Step 10: Write the failing jsdom pin**

Append to `web/tests/player.test.mjs`:

```js
// ── Legacy Twitch emote replay (Arc 1, T1-1) ────────────────────────────────

/** The alt text and surrounding text of one sidebar row's content span. */
const rowContent = (h, i) => {
  const span = h.sidebar().children[i].lastChild;
  return Array.from(span.childNodes).map((n) =>
    (n.tagName === "IMG" ? `[${n.alt}]` : n.textContent));
};

const ircChatMsg = (message, emotes) => ({
  offsetMs: 1000, authorName: "u", message, emotes,
  raw: `@emotes=x :u!u@u.tmi.twitch.tv PRIVMSG #c :${message}`,
});

test("a marked chat file's Twitch emote spans are rendered exactly as written", { skip }, async () => {
  const h = harness.makePlayer({
    jobs: [finished("j1", { chatFilename: "chat.json" })],
    watchState: {},
    chat: {
      platform: "twitch", emoteOffsets: "utf16",
      messages: [ircChatMsg("🎉 Kappa", [{ id: "25", name: "Kappa", start: 3, end: 7 }])],
    },
    storage: { "player-nico-toggle": "false", "player-sidebar-toggle": "true" },
  });
  await h.selectJob("j1");
  // Mutant: correcting a marked file anyway would shift the span to [5..9] and
  // the row would read "🎉 Ka" + [ppa].
  assert.deepEqual(rowContent(h, 0), ["🎉 ", "[Kappa]"]);
});

test("an unmarked legacy IRC message is re-indexed before it is rendered", { skip }, async () => {
  const h = harness.makePlayer({
    jobs: [finished("j1", { chatFilename: "chat.json" })],
    watchState: {},
    chat: {
      // No emoteOffsets: written before 2026-09-15. The stored span is the raw
      // code-point range and the stored name is the garbled UTF-16 slice.
      platform: "twitch",
      messages: [ircChatMsg("🎉 Kappa", [{ id: "25", name: " Kapp", start: 2, end: 6 }])],
    },
    storage: { "player-nico-toggle": "false", "player-sidebar-toggle": "true" },
  });
  await h.selectJob("j1");
  // Mutant: no correction at all renders "🎉" + [ Kapp] + "a".
  assert.deepEqual(rowContent(h, 0), ["🎉 ", "[Kappa]"]);
});

test("an unmarked VOD comment (no raw line) is rendered untouched", { skip }, async () => {
  const h = harness.makePlayer({
    jobs: [finished("j1", { chatFilename: "chat.json" })],
    watchState: {},
    chat: {
      platform: "twitch",
      messages: [{ offsetMs: 1000, authorName: "u", message: "🎉 Kappa",
        emotes: [{ id: "25", name: "Kappa", start: 3, end: 7 }] }],
    },
    storage: { "player-nico-toggle": "false", "player-sidebar-toggle": "true" },
  });
  await h.selectJob("j1");
  // Mutant: correcting every message in an unmarked file would shift this
  // already-UTF-16 span to [5..9] and render "🎉 Ka" + [ppa].
  assert.deepEqual(rowContent(h, 0), ["🎉 ", "[Kappa]"]);
});

test("a multi-part job corrects each part against its own header, before the merge", { skip }, async () => {
  // mergePartChats keeps platform/streamStartTime/emotes/messages and nothing
  // else, so a per-file scalar has to be consumed per part. Mutant: correcting
  // after the merge reads the merged object's (absent) marker and re-shifts the
  // marked part too — row 1 would render "🎉 Ka" + [ppa].
  //
  // The job is built inline rather than through segmented() (:183-190): that
  // helper's segments carry no chatFile, so the per-part fetch path would not
  // fire at all. This mirrors the job in "a multi-part job's chat comes from
  // the per-part files" (:664-671).
  const job = finished("j1", {
    segments: [
      { segmentIndex: 0, durationSeconds: 60, quality: "720p", chatFile: "p0.chat.json" },
      { segmentIndex: 1, durationSeconds: 60, quality: "720p", chatFile: "p1.chat.json" },
    ],
  });
  const h = harness.makePlayer({
    jobs: [job],
    watchState: {},
    segmentChatById: {
      "j1/0": { platform: "twitch",
        messages: [ircChatMsg("🎉 Kappa", [{ id: "25", name: " Kapp", start: 2, end: 6 }])] },
      "j1/1": { platform: "twitch", emoteOffsets: "utf16",
        messages: [ircChatMsg("🎉 Kappa", [{ id: "25", name: "Kappa", start: 3, end: 7 }])] },
    },
    storage: { "player-nico-toggle": "false", "player-sidebar-toggle": "true" },
  });
  await h.selectJob("j1");
  assert.equal(h.sidebar().children.length, 2);
  assert.deepEqual(rowContent(h, 0), ["🎉 ", "[Kappa]"]);
  assert.deepEqual(rowContent(h, 1), ["🎉 ", "[Kappa]"]);
});
```

- [ ] **Step 11: Run the jsdom pin to verify it fails**

Run: `cd web/tests && node --test player.test.mjs`

Expected: two of the four FAIL — "an unmarked legacy IRC message is re-indexed before it is rendered"
and the multi-part case's row 0 — each with
`AssertionError [ERR_ASSERTION]: Expected values to be strictly deep-equal:` and an actual of
`[ '🎉', '[ Kapp]', 'a' ]` against the expected `[ '🎉 ', '[Kappa]' ]`. The two that assert "untouched"
(the marked file and the VOD comment) pass already, which is the point: only Step 9's wiring moves the
legacy case, and it must move nothing else.

(If the implementer does Step 9 before Steps 10–11, all four are green on the first run. Then
temporarily revert the two `player.js` call sites, re-run to see the two red, and restore — the
requirement is to have SEEN the red, not the order the files were edited in.)

- [ ] **Step 12: Run the full JS + Go gates**

```bash
cd web/tests && node --test *.test.mjs
cd ../.. && GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/twitch/ ./internal/web/routes/
```

Expected: PASS for both.

- [ ] **Step 13: Commit**

```bash
git add internal/twitch/types.go internal/twitch/chat.go internal/twitch/chat_recording.go \
        internal/twitch/vod_chat.go internal/twitch/chat_file_marker_test.go \
        web/public/modules/chat-timeline.js web/public/modules/player.js \
        web/tests/chat-timeline.test.mjs web/tests/player.test.mjs
git commit -m "feat(twitch): mark chat files utf16 and repair legacy ones at replay

Every chat file this writes now carries a header scalar
emoteOffsets: \"utf16\". Its absence marks a file written while the live
IRC path mistook Twitch's code-point offsets for UTF-16 units, so the
player maps those messages' spans (and unwraps their /me text) once at
load — per part, before mergePartChats, which keeps no header scalars.
VOD comments in such a file carry no raw line and are left alone.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq"
```

---

### Task 4: VOD chat pages by edge cursor after the first page

**Files:**
- Modify: `internal/twitch/types.go:115-126` (`VodCommentEdge`)
- Modify: `internal/twitch/api.go:914-1022` (`GetVodComments`)
- Modify: `internal/twitch/service.go:57-60` (`Service.GetVodComments`)
- Modify: `internal/twitch/vod_chat.go:170-284` (the paging loop)
- Create: `internal/twitch/vod_chat_paging_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `VodCommentEdge.Cursor string` — the Relay edge cursor as sent.
  - `(*API).GetVodComments(ctx context.Context, vodID string, contentOffsetSeconds float64, cursor string, authToken string) ([]VodCommentEdge, bool, error)` — a non-empty `cursor`
    replaces `contentOffsetSeconds` in the GQL variables.
  - `(*Service).GetVodComments(ctx context.Context, vodID string, contentOffsetSeconds float64, cursor string) ([]VodCommentEdge, bool, error)`.

- [ ] **Step 1: Write the failing test**

Create `internal/twitch/vod_chat_paging_test.go`:

```go
package twitch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/constants"
)

// vodCommentPage is one GQL page of the fake server's script: `count` edges
// that ALL share the same content offset second — the burst that made the
// offset-based pager loop on itself — each carrying its own cursor.
type vodCommentPage struct {
	count   int
	offset  float64
	hasNext bool
}

// vodCommentsReply renders the real VideoCommentsByOffsetOrCursor response
// shape around one page. Edge i of page p gets id "c<p>-<i>" and cursor
// "cur<p>-<i>"; the fake server serves page p+1 when asked for "cur<p>-<last>".
func vodCommentsReply(page int, p vodCommentPage) string {
	edges := make([]map[string]any, 0, p.count)
	for i := range p.count {
		edges = append(edges, map[string]any{
			"cursor": fmt.Sprintf("cur%d-%d", page, i),
			"node": map[string]any{
				"id":                   fmt.Sprintf("c%d-%d", page, i),
				"contentOffsetSeconds": p.offset,
				"commenter":            map[string]any{"displayName": "Viewer", "id": "u1", "login": "viewer"},
				"message": map[string]any{
					"fragments":  []map[string]any{{"text": "hello"}},
					"userBadges": []map[string]any{},
					"userColor":  nil,
				},
			},
		})
	}
	body, err := json.Marshal(map[string]any{
		"data": map[string]any{"video": map[string]any{"comments": map[string]any{
			"edges":    edges,
			"pageInfo": map[string]any{"hasNextPage": p.hasNext},
		}}},
	})
	if err != nil {
		panic(err) // a literal this file built cannot fail to marshal
	}
	return string(body)
}

// installVodCommentStub points the package HTTP client at a fake GQL server
// serving `pages` in order, and returns a recorder of how each page was ASKED
// for ("offset:<v>" or "cursor:<v>"). It reuses probeRoundTripper
// (liveness_probe_test.go) rather than declaring a second identical type.
//
// The swap is why no test in this file may call t.Parallel: twitchHTTPClient is
// shared with every other test in the package.
func installVodCommentStub(t *testing.T, pages []vodCommentPage) *[]string {
	t.Helper()
	var mu sync.Mutex
	asked := make([]string, 0, len(pages))
	prev := twitchHTTPClient
	t.Cleanup(func() { twitchHTTPClient = prev })

	twitchHTTPClient = &http.Client{Transport: probeRoundTripper(func(req *http.Request) (*http.Response, error) {
		if !strings.HasPrefix(req.URL.String(), constants.TwitchURLs.GQL) {
			return nil, fmt.Errorf("stub received an unexpected request host")
		}
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		var q struct {
			Variables struct {
				VideoID              string   `json:"videoID"`
				ContentOffsetSeconds *float64 `json:"contentOffsetSeconds"`
				Cursor               *string  `json:"cursor"`
			} `json:"variables"`
			Extensions struct {
				PersistedQuery struct {
					Hash string `json:"sha256Hash"`
				} `json:"persistedQuery"`
			} `json:"extensions"`
		}
		if err := json.Unmarshal(raw, &q); err != nil {
			return nil, fmt.Errorf("stub could not parse the request body: %w", err)
		}
		if q.Extensions.PersistedQuery.Hash != constants.TwitchGQLHashes.VideoCommentsByOffsetOrCursor {
			return nil, fmt.Errorf("stub: the persisted-query hash changed")
		}

		page := 0
		switch {
		case q.Variables.Cursor != nil && *q.Variables.Cursor != "":
			cursor := *q.Variables.Cursor
			mu.Lock()
			asked = append(asked, "cursor:"+cursor)
			mu.Unlock()
			// "cur<p>-<i>" asks for page p+1.
			var p, i int
			if _, err := fmt.Sscanf(cursor, "cur%d-%d", &p, &i); err != nil {
				return nil, fmt.Errorf("stub: unrecognised cursor")
			}
			page = p + 1
		case q.Variables.ContentOffsetSeconds != nil:
			mu.Lock()
			asked = append(asked, fmt.Sprintf("offset:%v", *q.Variables.ContentOffsetSeconds))
			mu.Unlock()
		default:
			return nil, fmt.Errorf("stub: the request carried neither an offset nor a cursor")
		}
		if page >= len(pages) {
			return nil, fmt.Errorf("stub: asked for page %d of %d — the loop ran past the script", page, len(pages))
		}

		h := make(http.Header)
		h.Set("Content-Type", "application/json")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     h,
			Body:       io.NopCloser(bytes.NewReader([]byte(vodCommentsReply(page, pages[page])))),
			Request:    req,
		}, nil
	})}
	return &asked
}

// TestVodChatPagesByCursorThroughABurstSecond is T1-3.
//
// A second of a popular VOD can hold more comments than one page (59 edges),
// and every edge in that second reports the SAME integer contentOffsetSeconds.
// The offset-based pager therefore asked for the same page again, saw only
// duplicates, found the offset had not advanced, and BROKE — stranding the
// whole rest of the VOD's chat behind that one second.
//
// Mutants this kills:
//   - paging by offset (today): 59 messages archived, the run stops with the
//     "offset did not advance" Warn.
//   - paging by the FIRST edge's cursor instead of the last: page 2 repeats
//     58 of page 1's edges and the count lands short.
//   - sending both variables: the stub's request classifier is a real
//     discriminator — Twitch's own resolver prefers one, and shipping both
//     leaves which one undefined.
func TestVodChatPagesByCursorThroughABurstSecond(t *testing.T) {
	pages := []vodCommentPage{
		{count: 59, offset: 1200, hasNext: true},
		{count: 59, offset: 1200, hasNext: true},
		{count: 10, offset: 1200, hasNext: false},
	}
	asked := installVodCommentStub(t, pages)

	vcd := NewVodChatDownloader(NewAPI(&testLogger{}), VodChatOptions{
		VodID:      "v1",
		OutputPath: filepath.Join(t.TempDir(), "vod.chat.json"),
	}, &testLogger{})

	if err := vcd.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := vcd.MessageCount(); got != 128 {
		t.Errorf("archived %d comments, want 128 — the tail past the burst second was stranded", got)
	}
	want := []string{"offset:0", "cursor:cur0-58", "cursor:cur1-58"}
	if !slices.Equal(*asked, want) {
		t.Errorf("pages were asked for as %v, want %v", *asked, want)
	}
}

// TestVodChatResumeStartsFromTheSavedOffset pins the half that stays offset-
// based: a cursor is opaque and is not persisted, so a resumed run re-enters by
// LastOffsetSeconds and switches to cursors from its second page on.
//
// Mutant: persisting the cursor in the sidecar and resuming from it — Twitch's
// cursors are not documented as durable across sessions, and a stale one
// returns an empty page that today's `len(edges) == 0` arm reads as "end of
// VOD".
func TestVodChatResumeStartsFromTheSavedOffset(t *testing.T) {
	pages := []vodCommentPage{{count: 2, offset: 900, hasNext: false}}
	asked := installVodCommentStub(t, pages)

	dir := t.TempDir()
	out := filepath.Join(dir, "vod.chat.json")
	vcd := NewVodChatDownloader(NewAPI(&testLogger{}), VodChatOptions{
		VodID:      "v1",
		OutputPath: out,
	}, &testLogger{})
	vcd.saveResumeState(900)

	if err := vcd.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(*asked) != 1 || (*asked)[0] != "offset:900" {
		t.Errorf("resumed run asked %v, want a single offset:900 request", *asked)
	}
}

// TestVodChatStopsOnAStuckCursor replaces the offset-advance guard the cursor
// pager retires. A server that answers every cursor with the same page and
// hasNextPage=true would otherwise spin forever.
//
// Mutant: deleting the guard with nothing in its place. The fake server's
// "ran past the script" error would eventually end the run through the
// consecutive-error budget, which is why the assertion is on the REQUEST COUNT,
// not merely on termination.
func TestVodChatStopsOnAStuckCursor(t *testing.T) {
	var mu sync.Mutex
	var requests int
	prev := twitchHTTPClient
	t.Cleanup(func() { twitchHTTPClient = prev })
	twitchHTTPClient = &http.Client{Transport: probeRoundTripper(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		requests++
		mu.Unlock()
		h := make(http.Header)
		h.Set("Content-Type", "application/json")
		// Always page 0, always hasNext — a cursor that never advances.
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     h,
			Body: io.NopCloser(bytes.NewReader([]byte(
				vodCommentsReply(0, vodCommentPage{count: 3, offset: 60, hasNext: true})))),
			Request: req,
		}, nil
	})}

	vcd := NewVodChatDownloader(NewAPI(&testLogger{}), VodChatOptions{
		VodID:      "v1",
		OutputPath: filepath.Join(t.TempDir(), "vod.chat.json"),
	}, &testLogger{})
	if err := vcd.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	mu.Lock()
	got := requests
	mu.Unlock()
	if got != 2 {
		t.Errorf("made %d requests against a stuck cursor, want 2 (the page, then the repeat "+
			"that proves the cursor did not move)", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestVodChat' ./internal/twitch/`

Expected: FAIL — `TestVodChatPagesByCursorThroughABurstSecond` reports
`stub: the request carried neither an offset nor a cursor`… no: it reports
`pages were asked for as [offset:0 offset:1200] , want [offset:0 cursor:cur0-58 cursor:cur1-58]` and
`archived 59 comments, want 128`.

- [ ] **Step 3: Carry the cursor on the edge and accept one in the request**

In `internal/twitch/types.go`, add to `VodCommentEdge` (after `ID`):

```go
	// Cursor is the Relay edge cursor Twitch sends beside the node. It is the
	// ONLY reliable way to page a VOD: contentOffsetSeconds is an integer
	// second, and a second of a busy VOD holds more comments than one page, so
	// an offset-based next-page request asks for the page it just read.
	Cursor string
```

In `internal/twitch/api.go`, replace the signature and the request builder (`:914-921`):

```go
// GetVodComments fetches VOD chat comments at a given offset.
func (a *API) GetVodComments(ctx context.Context, vodID string, contentOffsetSeconds float64, authToken string) ([]VodCommentEdge, bool, error) {
	query := newPersistedQuery("VideoCommentsByOffsetOrCursor", constants.TwitchGQLHashes.VideoCommentsByOffsetOrCursor, map[string]any{
		"videoID":              vodID,
		"contentOffsetSeconds": contentOffsetSeconds,
	})
```

with:

```go
// GetVodComments fetches one page of VOD chat comments.
//
// The operation is VideoCommentsByOffsetOrCursor and the OR is the point: a
// non-empty cursor selects the page AFTER the edge it came from, and
// contentOffsetSeconds selects the page containing a moment. They are mutually
// exclusive — sending both leaves it to Twitch's resolver which one wins — so
// the cursor takes precedence here and the offset is used only to ENTER a VOD
// (a fresh start, or a resume from the sidecar's LastOffsetSeconds).
//
// The persisted-query hash is unchanged: both forms are the same operation.
func (a *API) GetVodComments(ctx context.Context, vodID string, contentOffsetSeconds float64, cursor, authToken string) ([]VodCommentEdge, bool, error) {
	vars := map[string]any{"videoID": vodID}
	if cursor != "" {
		vars["cursor"] = cursor
	} else {
		vars["contentOffsetSeconds"] = contentOffsetSeconds
	}
	query := newPersistedQuery("VideoCommentsByOffsetOrCursor", constants.TwitchGQLHashes.VideoCommentsByOffsetOrCursor, vars)
```

In the response struct (`internal/twitch/api.go:929-953`), add the edge-level cursor: change

```go
					Edges []struct {
						Node struct {
```

to

```go
					Edges []struct {
						Cursor string `json:"cursor"`
						Node   struct {
```

and in the mapping loop (`:1000-1005`) add the field to the `VodCommentEdge` literal:

```go
		edge := VodCommentEdge{
			ID:                   node.ID,
			Cursor:               e.Cursor,
			ContentOffsetSeconds: node.ContentOffsetSeconds,
			MessageText:          strings.Join(msgParts, ""),
			Emotes:               emotes,
		}
```

In `internal/twitch/service.go:57-60`, mirror the signature:

```go
// GetVodComments fetches a page of VOD comments. A non-empty cursor selects the
// page after that edge; otherwise the offset selects the page containing it.
func (s *Service) GetVodComments(ctx context.Context, vodID string, contentOffsetSeconds float64, cursor string) ([]VodCommentEdge, bool, error) {
	return s.API.GetVodComments(ctx, vodID, contentOffsetSeconds, cursor, s.Auth.GetAuthToken())
}
```

- [ ] **Step 4: Page the loop by cursor**

In `internal/twitch/vod_chat.go`, declare the cursor beside the offset. Replace `:151-152`:

```go
	var contentOffset float64
	consecutiveErrors := 0
```

with:

```go
	var contentOffset float64
	// cursor is empty for the FIRST request of a run (a fresh start or a
	// resume, both of which enter by offset) and holds the previous page's
	// last edge cursor thereafter. It is deliberately not persisted: a Twitch
	// cursor is opaque and undocumented as durable, and a stale one answers
	// with an empty page that this loop reads as the end of the VOD.
	var cursor string
	consecutiveErrors := 0
```

Replace the fetch at `:179`:

```go
		edges, hasNext, err := vcd.api.GetVodComments(ctx, vcd.vodID, contentOffset, vcd.currentAuthToken())
```

with:

```go
		edges, hasNext, err := vcd.api.GetVodComments(ctx, vcd.vodID, contentOffset, cursor, vcd.currentAuthToken())
```

Replace the advance block at `:258-271`:

```go
		// Advance offset to the last edge's offset so the next page
		// moves forward in time even when the current page was entirely
		// duplicates (newCount==0 with hasNext==true).
		if len(edges) > 0 {
			newOffset := edges[len(edges)-1].ContentOffsetSeconds
			// Guard against a pathological server response that never
			// advances the offset — break to avoid an infinite loop.
			if newCount == 0 && newOffset <= contentOffset {
				vcd.logger.Warn("[TwitchVodChat] offset did not advance on all-duplicate page; stopping",
					"offset", contentOffset)
				break
			}
			contentOffset = newOffset
		}
```

with:

```go
		// Advance by the LAST edge's cursor, not by its offset.
		// contentOffsetSeconds is an integer second, and a second of a busy
		// VOD holds more than one page of comments — so an offset-based next
		// request asks for the page just read, sees only duplicates, and used
		// to break here with the rest of the VOD's chat unarchived (T1-3).
		//
		// contentOffset keeps tracking the last edge's offset because it is
		// what the resume sidecar and the progress line are written from; it
		// is no longer what the next request is built from.
		last := edges[len(edges)-1]
		if last.Cursor == "" {
			vcd.logger.Warn("[TwitchVodChat] page carried no cursor; stopping",
				"offset", last.ContentOffsetSeconds)
			break
		}
		if last.Cursor == cursor {
			// A server that answers a cursor with the page that cursor came
			// from would otherwise spin forever. This replaces the old
			// offset-advance guard, which the burst second tripped legitimately.
			vcd.logger.Warn("[TwitchVodChat] cursor did not advance; stopping",
				"offset", last.ContentOffsetSeconds)
			break
		}
		cursor = last.Cursor
		contentOffset = last.ContentOffsetSeconds
```

(The `if len(edges) > 0` wrapper is dropped: the loop already `break`s on `len(edges) == 0` at
`:240-243`, so `edges` is non-empty by the time control reaches here.)

- [ ] **Step 5: Run the tests to verify they pass**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/twitch/`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/twitch/types.go internal/twitch/api.go internal/twitch/service.go \
        internal/twitch/vod_chat.go internal/twitch/vod_chat_paging_test.go
git commit -m "fix(twitch): page VOD chat by edge cursor after the first page

contentOffsetSeconds is an integer second and a page holds 59 edges, so
a second with 59+ comments made the next offset-based request ask for
the page it had just read; the loop saw only duplicates, found the
offset unmoved, and stopped with the rest of the VOD's chat unarchived.
Pages now advance by the last edge's cursor (same persisted-query hash,
the operation already supports both); the offset still ENTERS a VOD on
start and on resume, and a cursor that repeats stops the loop.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq"
```

---

### Task 5: The emote cache stops latching failures, and expires

**Files:**
- Modify: `internal/twitch/emotes.go:16-176` (struct, `Resolve`, `Clear`), `:178-361` (the three fetchers)
- Modify: `internal/twitch/chat_recording.go:230-234` (the false comment)
- Test: `internal/twitch/emotes_test.go` (rewrite the four existing tests, add three)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `(*EmoteResolver).fetchBTTV/fetchFFZ/fetch7TV(ctx context.Context, channelID string) ([]EmoteInfo, bool)` — the bool is "the provider ANSWERED", not "it had emotes".
  - `(*EmoteResolver).Resolve(ctx, channelID string, channelLogin ...string) *TwitchEmoteData` — same
    signature; returns **nil** when nothing is cached and no provider answered.
  - `emoteCacheTTL = 24 * time.Hour`, and `EmoteResolver.now func() time.Time` (defaults to
    `time.Now`; tests replace it).

- [ ] **Step 1: Write the failing test**

Replace the whole body of `internal/twitch/emotes_test.go` below its imports with the block below
(`testLogger` at `:8-13` stays exactly as it is — the other test files use it).

```go
// seedEmoteCache puts a FRESH entry in the cache, the way a successful Resolve
// would. Tests must not poke er.cache directly any more: an entry with a zero
// fetchedAt is expired, so a direct poke would silently turn a cache-hit test
// into a network test.
func seedEmoteCache(er *EmoteResolver, key string, data *TwitchEmoteData) {
	er.mu.Lock()
	defer er.mu.Unlock()
	er.cache[key] = emoteCacheEntry{data: data, fetchedAt: er.now()}
	er.cacheOrder = append(er.cacheOrder, key)
}

// installEmoteFetchStub points the package HTTP client at one canned reply for
// every provider. status 0 means "the transport itself fails", i.e. all three
// providers are down. It returns the request counter.
//
// The swap is why no test in this file may call t.Parallel: twitchHTTPClient is
// shared with every other test in the package.
func installEmoteFetchStub(t *testing.T, status int, body string) *atomic.Int64 {
	t.Helper()
	var calls atomic.Int64
	prev := twitchHTTPClient
	t.Cleanup(func() { twitchHTTPClient = prev })
	twitchHTTPClient = &http.Client{Transport: probeRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		if status == 0 {
			return nil, fmt.Errorf("emote provider unreachable")
		}
		h := make(http.Header)
		h.Set("Content-Type", "application/json")
		return &http.Response{
			StatusCode: status,
			Header:     h,
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})}
	return &calls
}

func TestEmoteResolverCacheHit(t *testing.T) {
	er := NewEmoteResolver(&testLogger{})
	seedEmoteCache(er, "testchannel", &TwitchEmoteData{
		BTTV: []EmoteInfo{{ID: "1", Code: "test", URL: "https://example.com"}},
	})
	// No HTTP stub installed: a fetch here would hit the real network, which is
	// itself the assertion that the cache was used.
	result := er.Resolve(context.Background(), "ignored", "TestChannel")
	if result == nil {
		t.Fatal("expected cached result")
	}
	if len(result.BTTV) != 1 || result.BTTV[0].Code != "test" {
		t.Errorf("unexpected cached result: %+v", result)
	}
}

func TestEmoteResolverLRUEviction(t *testing.T) {
	er := NewEmoteResolver(&testLogger{})
	for i := range 200 {
		seedEmoteCache(er, "channel_"+string(rune('a'+i%26))+string(rune('a'+i/26)), &TwitchEmoteData{})
	}
	if len(er.cache) != 200 {
		t.Fatalf("expected 200 cache entries, got %d", len(er.cache))
	}
	oldestKey := er.cacheOrder[0]

	er.mu.Lock()
	er.evictIfFullLocked()
	er.cache["new_channel"] = emoteCacheEntry{data: &TwitchEmoteData{}, fetchedAt: er.now()}
	er.cacheOrder = append(er.cacheOrder, "new_channel")
	er.mu.Unlock()

	if _, exists := er.cache[oldestKey]; exists {
		t.Error("expected oldest entry to be evicted")
	}
	if _, exists := er.cache["new_channel"]; !exists {
		t.Error("expected new entry to exist")
	}
	if len(er.cache) != 200 {
		t.Errorf("expected 200 cache entries after eviction, got %d", len(er.cache))
	}
}

func TestEmoteResolverLRUPromotion(t *testing.T) {
	er := NewEmoteResolver(&testLogger{})
	seedEmoteCache(er, "a", &TwitchEmoteData{})
	seedEmoteCache(er, "b", &TwitchEmoteData{})
	seedEmoteCache(er, "c", &TwitchEmoteData{})

	er.Resolve(context.Background(), "ignored", "A")

	if er.cacheOrder[len(er.cacheOrder)-1] != "a" {
		t.Errorf("expected 'a' at end of order after access, got order: %v", er.cacheOrder)
	}
}

func TestEmoteResolverClear(t *testing.T) {
	er := NewEmoteResolver(&testLogger{})
	seedEmoteCache(er, "test", &TwitchEmoteData{})

	er.Clear()

	if len(er.cache) != 0 {
		t.Errorf("expected empty cache after Clear(), got %d entries", len(er.cache))
	}
	if len(er.cacheOrder) != 0 {
		t.Errorf("expected empty cacheOrder after Clear(), got %d entries", len(er.cacheOrder))
	}
}

// TestEmoteResolverDoesNotCacheATotalFailure is the first half of T1-11.
//
// Resolve used to write the cache BEFORE looking at whether anything came back,
// so three simultaneously-failing providers — one BTTV outage, one flaky
// network minute — poisoned that channel for the whole process lifetime. A
// 24/7 daemon then served an empty emote set for days.
//
// Mutants: caching unconditionally (the second Resolve makes no requests and
// returns non-nil); returning a non-nil empty set on total failure (the
// per-downloader cache in resolveEmotesCached latches THAT instead, which is
// the same bug one layer up).
func TestEmoteResolverDoesNotCacheATotalFailure(t *testing.T) {
	er := NewEmoteResolver(&testLogger{})
	calls := installEmoteFetchStub(t, 0, "")

	if got := er.Resolve(context.Background(), "chan-1", "chan-1"); got != nil {
		t.Errorf("Resolve with every provider down = %+v, want nil", got)
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("first Resolve made %d requests, want 3", n)
	}
	er.mu.Lock()
	cached := len(er.cache)
	er.mu.Unlock()
	if cached != 0 {
		t.Errorf("cache holds %d entries after a total failure, want 0", cached)
	}

	er.Resolve(context.Background(), "chan-1", "chan-1")
	if n := calls.Load(); n != 6 {
		t.Errorf("second Resolve brought the total to %d requests, want 6 — the failure was cached", n)
	}
}

// TestEmoteResolverCachesAPartialSuccess pins the other edge: ONE provider
// answering is enough to cache, including when it answers with no emotes.
//
// Mutant: gating the cache on `total > 0` instead of on "any provider
// answered" — a channel with no third-party emotes at all would then be
// refetched from three APIs on every part of every job, forever.
func TestEmoteResolverCachesAPartialSuccess(t *testing.T) {
	er := NewEmoteResolver(&testLogger{})
	// A 200 with a body every provider parses to zero emotes.
	calls := installEmoteFetchStub(t, http.StatusOK, `{}`)

	if got := er.Resolve(context.Background(), "chan-1", "chan-1"); got == nil {
		t.Fatal("Resolve with three answering providers returned nil")
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("first Resolve made %d requests, want 3", n)
	}
	er.Resolve(context.Background(), "chan-1", "chan-1")
	if n := calls.Load(); n != 3 {
		t.Errorf("second Resolve brought the total to %d requests, want 3 — an emote-less "+
			"channel must still be cached", n)
	}
}

// TestEmoteResolverRefetchesAfterTheTTL is the second half of T1-11: a daemon
// that runs for weeks must pick up a channel's new 7TV/BTTV/FFZ emotes.
//
// Mutants: no TTL at all (the post-expiry Resolve makes no requests); an
// expiry that DROPS the entry before refetching (the failing refetch below
// would then return nil and the job would lose the emote set it had).
func TestEmoteResolverRefetchesAfterTheTTL(t *testing.T) {
	er := NewEmoteResolver(&testLogger{})
	now := time.Now()
	er.now = func() time.Time { return now }

	stale := &TwitchEmoteData{BTTV: []EmoteInfo{{ID: "1", Code: "old"}}}
	seedEmoteCache(er, "chan-1", stale)

	now = now.Add(emoteCacheTTL - time.Minute)
	calls := installEmoteFetchStub(t, 0, "")
	er.Resolve(context.Background(), "chan-1", "chan-1")
	if n := calls.Load(); n != 0 {
		t.Fatalf("an entry one minute inside the TTL made %d requests, want 0", n)
	}

	now = now.Add(2 * time.Minute) // now past emoteCacheTTL
	got := er.Resolve(context.Background(), "chan-1", "chan-1")
	if n := calls.Load(); n != 3 {
		t.Errorf("an expired entry made %d requests, want 3", n)
	}
	if got == nil || len(got.BTTV) != 1 || got.BTTV[0].Code != "old" {
		t.Errorf("a failed refetch returned %+v, want the stale set served rather than dropped", got)
	}
}
```

Set the file's import block to:

```go
import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestEmoteResolver' ./internal/twitch/`

Expected: FAIL to COMPILE — `undefined: emoteCacheEntry`, `er.now undefined`, `undefined: emoteCacheTTL`,
`er.evictIfFullLocked undefined`.

- [ ] **Step 3: Rework the resolver**

In `internal/twitch/emotes.go`, replace `:16-45` (the const, the struct and the constructor) with:

```go
const emoteTimeout = 8 * time.Second

// emoteCacheTTL bounds how long a channel's third-party emote set is served
// from cache. Moombox runs for weeks at a time and 7TV/BTTV/FFZ sets change
// daily, so a cache with no expiry archives a channel's chat against the emote
// set it had the first time the daemon saw it.
//
// A day is long enough that the three APIs are hit once per channel per day
// even for a channel that is live every day, and short enough that a new emote
// shows up in the next archive rather than the next restart.
const emoteCacheTTL = 24 * time.Hour

// emoteCacheEntry is one channel's resolved set plus WHEN it was resolved.
// The timestamp is the whole reason this is a struct rather than the bare
// pointer it used to be.
type emoteCacheEntry struct {
	data      *TwitchEmoteData
	fetchedAt time.Time
}

// EmoteResolver fetches and caches third-party emotes for Twitch channels.
type EmoteResolver struct {
	mu         sync.Mutex
	cache      map[string]emoteCacheEntry // channelLogin (lowered) -> entry
	cacheOrder []string                   // insertion order for LRU eviction
	inflight   map[string]chan struct{}   // dedup concurrent fetches for same key

	// now is the clock the TTL is measured against. A field so a test can
	// cross a 24-hour boundary without waiting for one; production never
	// assigns it.
	now func() time.Time

	logger interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

// NewEmoteResolver creates a new emote resolver.
func NewEmoteResolver(logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *EmoteResolver {
	return &EmoteResolver{
		cache:    make(map[string]emoteCacheEntry),
		inflight: make(map[string]chan struct{}),
		now:      time.Now,
		logger:   logger,
	}
}

// evictIfFullLocked drops the oldest entry when the cache is at its 200-channel
// ceiling. Caller holds er.mu.
func (er *EmoteResolver) evictIfFullLocked() {
	const maxEmoteCacheEntries = 200
	if len(er.cache) < maxEmoteCacheEntries || len(er.cacheOrder) == 0 {
		return
	}
	oldest := er.cacheOrder[0]
	er.cacheOrder = er.cacheOrder[1:]
	delete(er.cache, oldest)
}
```

Replace `Resolve` (`:47-168`) with:

```go
// Resolve fetches all third-party emotes for a channel.
//
// Results are cached per channel login (lowercased); the channelID is used for
// the API calls. Two rules beyond the plain LRU, and both exist because this
// process runs for weeks (T1-11):
//
//   - A FAILURE IS NOT A RESULT. The set is cached only when at least one of
//     the three providers answered — including answering with no emotes, which
//     is the honest state of many channels. Three providers failing together
//     (one outage, one flaky minute) used to be written to the cache and served
//     for the rest of the process lifetime.
//   - AN ANSWER GOES STALE. An entry older than emoteCacheTTL is refetched on
//     the next Resolve. If that refetch fails outright the STALE set is served
//     and kept: an emote set from yesterday beats none.
//
// Returns nil only when nothing is cached and no provider answered, so the
// caller's own cache (resolveEmotesCached, chat_recording.go) can retry later.
func (er *EmoteResolver) Resolve(ctx context.Context, channelID string, channelLogin ...string) *TwitchEmoteData {
	// Determine cache key: prefer channelLogin, fall back to channelID
	cacheKey := channelID
	if len(channelLogin) > 0 && channelLogin[0] != "" {
		cacheKey = strings.ToLower(channelLogin[0])
	}

	er.mu.Lock()
	var stale *TwitchEmoteData
	if cached, ok := er.cache[cacheKey]; ok {
		// LRU: move to end of order list
		for i, k := range er.cacheOrder {
			if k == cacheKey {
				er.cacheOrder = append(er.cacheOrder[:i], er.cacheOrder[i+1:]...)
				er.cacheOrder = append(er.cacheOrder, cacheKey)
				break
			}
		}
		if er.now().Sub(cached.fetchedAt) < emoteCacheTTL {
			er.mu.Unlock()
			return cached.data
		}
		// Expired: keep it in hand as the fallback for a refetch that fails,
		// and leave it in the map so a concurrent caller keeps being served.
		stale = cached.data
	}
	// Dedup: if another goroutine is already fetching this key, wait for it.
	// Respect ctx so a cancelled download doesn't sit here waiting on a
	// fetcher that may itself be blocked on network IO.
	if wait, ok := er.inflight[cacheKey]; ok {
		er.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return stale
		}
		// Now it should be in cache (unless ctx cancelled and fetcher hadn't
		// populated yet — in which case cache miss is fine, Twitch emote
		// resolution is best-effort).
		er.mu.Lock()
		cached, ok := er.cache[cacheKey]
		er.mu.Unlock()
		if ok {
			return cached.data
		}
		return stale
	}
	// Mark this key as inflight
	done := make(chan struct{})
	er.inflight[cacheKey] = done
	er.mu.Unlock()

	er.logger.Debug("resolving emotes", "channelID", channelID)

	// Fetch all providers in parallel
	var wg sync.WaitGroup
	var bttvResult, ffzResult, sevenTVResult []EmoteInfo
	var bttvOK, ffzOK, sevenTVOK bool

	wg.Add(3)

	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				er.logger.Error("BTTV emote fetch panic", "panic", r)
			}
		}()
		bttvResult, bttvOK = er.fetchBTTV(ctx, channelID)
	}()

	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				er.logger.Error("FFZ emote fetch panic", "panic", r)
			}
		}()
		ffzResult, ffzOK = er.fetchFFZ(ctx, channelID)
	}()

	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				er.logger.Error("7TV emote fetch panic", "panic", r)
			}
		}()
		sevenTVResult, sevenTVOK = er.fetch7TV(ctx, channelID)
	}()

	wg.Wait()

	data := &TwitchEmoteData{
		BTTV:    bttvResult,
		FFZ:     ffzResult,
		SevenTV: sevenTVResult,
	}
	answered := bttvOK || ffzOK || sevenTVOK

	er.mu.Lock()
	if answered {
		// Refreshing an EXPIRED entry must not evict anything and must not
		// append a second order entry: the key is already in both, and the LRU
		// promotion above already moved it to the end.
		if _, existing := er.cache[cacheKey]; !existing {
			er.evictIfFullLocked()
			er.cacheOrder = append(er.cacheOrder, cacheKey)
		}
		er.cache[cacheKey] = emoteCacheEntry{data: data, fetchedAt: er.now()}
	}
	delete(er.inflight, cacheKey)
	er.mu.Unlock()
	// Release er.mu before close(done) so any waiting goroutines wake up
	// and re-acquire er.mu cleanly without contending against the still-
	// held lock. Minor throughput win under high concurrent resolve rates.
	close(done)

	if !answered {
		if stale != nil {
			er.logger.Warn("emote refresh failed; serving the cached set",
				"channelID", channelID)
			return stale
		}
		er.logger.Warn("every third-party emote provider failed; not caching",
			"channelID", channelID)
		return nil
	}

	total := len(bttvResult) + len(ffzResult) + len(sevenTVResult)
	if total > 0 {
		er.logger.Debug("emotes resolved",
			"channelID", channelID,
			"bttv", len(bttvResult),
			"ffz", len(ffzResult),
			"7tv", len(sevenTVResult))
	}

	return data
}
```

Replace `Clear` (`:170-176`) so it builds the right map type:

```go
// Clear empties the emote cache (e.g., on shutdown).
func (er *EmoteResolver) Clear() {
	er.mu.Lock()
	er.cache = make(map[string]emoteCacheEntry)
	er.cacheOrder = nil
	er.mu.Unlock()
}
```

- [ ] **Step 4: Make the three fetchers report whether they answered**

In `internal/twitch/emotes.go`, change each fetcher's signature and every `return` in it. The rule:
`false` means the provider did not answer usably (transport error, non-200, unparseable body); `true`
means it answered, whatever the count.

`fetchBTTV` (`:178-226`):

```go
// fetchBTTV returns the channel's BTTV emotes and whether BTTV ANSWERED.
// The bool is not "found emotes": a channel with no BTTV emotes is a real,
// cacheable answer, and only a provider that could not be reached or read is a
// failure (see Resolve).
func (er *EmoteResolver) fetchBTTV(ctx context.Context, channelID string) ([]EmoteInfo, bool) {
```

- `return nil` after the `fetchJSON` error → `return nil, false`
- `return nil` after the `json.Unmarshal` error → `return nil, false`
- the final `return emotes` → `return emotes, true`

`fetchFFZ` (`:228-280`): same doc sentence with FFZ, same three edits (`return nil, false`,
`return nil, false`, `return emotes, true`).

`fetch7TV` (`:282-361`): same, with 7TV.

- [ ] **Step 5: Fix the false comment one layer up**

In `internal/twitch/chat_recording.go`, replace the doc comment at `:230-234`:

```go
// resolveEmotesCached resolves third-party emotes (7TV/BTTV/FFZ) once per
// downloader and caches the result, so multi-part jobs don't re-hit the
// emote APIs for every part. emoteMu is held across the resolve to
// single-flight concurrent callers; a failed resolve (nil) is not cached,
// letting a later part retry.
```

with:

```go
// resolveEmotesCached resolves third-party emotes (7TV/BTTV/FFZ) once per
// downloader and caches the result, so multi-part jobs don't re-hit the
// emote APIs for every part. emoteMu is held across the resolve to
// single-flight concurrent callers.
//
// A resolve in which NO provider answered returns nil (EmoteResolver.Resolve),
// which this leaves uncached so a later part retries. That sentence was here
// before the resolver could produce it: until 2026-09-15 a total failure came
// back as a non-nil empty set, which latched here for the life of the job AND
// in the resolver for the life of the process (T1-11). Both layers now turn on
// the same fact.
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/twitch/`

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/twitch/emotes.go internal/twitch/emotes_test.go internal/twitch/chat_recording.go
git commit -m "fix(twitch): never cache a failed emote resolve, and expire after 24h

Resolve wrote the cache before looking at what came back, so three
providers failing together poisoned a channel for the process lifetime
— and nothing ever refreshed a 24/7 daemon's emote sets. The fetchers
now report whether they ANSWERED (an emote-less channel is an answer),
Resolve caches only on an answer, entries older than emoteCacheTTL are
refetched, and a failed refetch serves the stale set rather than none.
The per-downloader comment that claimed this already held is corrected.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq"
```

---

### Task 6: One resume-ID cap for both chat downloaders

**Files:**
- Modify: `internal/twitch/chat.go:20-48` (const block), `:554-577` (`saveResumeState`)
- Modify: `internal/twitch/vod_chat.go:18-21` (const block), `:429`
- Create: `internal/twitch/chat_resume_cap_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `chatResumeIDCap = 1000` in `internal/twitch/chat.go`. `vodChatResumeMaxRecentIDs` is
  DELETED (its only reference is `vod_chat.go:429`).

- [ ] **Step 1: Write the failing test**

Create `internal/twitch/chat_resume_cap_test.go`:

```go
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'ResumeSidecarIsCapped|ShareOneCap' ./internal/twitch/`

Expected: FAIL to COMPILE — `undefined: chatResumeIDCap`.

- [ ] **Step 3: Introduce the shared constant**

In `internal/twitch/chat.go`, add to the const block (after `chatDedupMax`, `:22`):

```go
	// chatResumeIDCap bounds the dedup IDs a resume sidecar carries. ONE
	// constant for both chat downloaders: the VOD path has always capped at
	// 1000 and the IRC path snapshotted its whole 5000-entry set, which is a
	// ~200 KB marshal + fsync + rename every second on a busy channel
	// (chatSaveInterval). The window a reconnect replay can overlap is
	// seconds, so the newest 1000 is the whole of what the cap has to cover.
	chatResumeIDCap = 1000
```

In `internal/twitch/chat.go`, change `saveResumeState` (`:561`):

```go
	recentIDs := cd.dedup.Snapshot(0)
```

to

```go
	// Newest-first window, not the whole set — see chatResumeIDCap.
	recentIDs := cd.dedup.Snapshot(chatResumeIDCap)
```

In `internal/twitch/vod_chat.go`, delete `vodChatResumeMaxRecentIDs = 1000` from the const block
(`:21`) and change `:429`:

```go
	recentIDs := vcd.dedup.Snapshot(vodChatResumeMaxRecentIDs)
```

to

```go
	recentIDs := vcd.dedup.Snapshot(chatResumeIDCap)
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/twitch/`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/twitch/chat.go internal/twitch/vod_chat.go internal/twitch/chat_resume_cap_test.go
git commit -m "perf(twitch): cap the IRC chat resume sidecar at 1000 recent IDs

The IRC sidecar snapshotted the whole 5000-entry dedup set on every
flush — a ~200 KB marshal, fsync and rename about once a second on a
busy channel — for a window a reconnect replay can only overlap by
seconds. Both chat downloaders now share chatResumeIDCap (the value the
VOD path already used), keeping the NEWEST 1000 IDs.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq"
```

---

### Task 7: Client-initiated IRC keepalive on injectable durations

**Files:**
- Create: `internal/twitch/delays.go`
- Modify: `internal/twitch/chat.go:20-48` (const block), `:184-226` (struct field), `:293-307` (constructor)
- Modify: `internal/twitch/chat_irc.go:256-294` (keepalive goroutine, comment rewrite, error arm)
- Create: `internal/twitch/chat_keepalive_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `type chatDelays struct { keepaliveIdle, keepalivePongWait, keepaliveCheck time.Duration }` and
    `defaultChatDelays() chatDelays` in `internal/twitch/delays.go`.
  - Constants `ircKeepaliveIdle = 45 * time.Second`, `ircKeepalivePongWait = 10 * time.Second`,
    `ircKeepaliveCheck = 15 * time.Second`, `ircKeepalivePing = "PING :moombox"` in
    `internal/twitch/chat.go`.
  - `ChatDownloader.delays chatDelays`, installed by `NewChatDownloader`; tests assign it directly
    after construction, the way `internal/engine` tests poke `SegmentDownloader.delays`.

- [ ] **Step 1: Write the failing test**

Create `internal/twitch/chat_keepalive_test.go`:

```go
package twitch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/vampiricwulf/Moombox/internal/constants"
)

// fastChatDelays is defaultChatDelays() scaled so the whole keepalive cycle
// runs in ~150 ms instead of ~75 s, keeping the RATIOS production has: the
// idle window is three check ticks, and the pong window is between one and two.
// Nothing here is small enough to race the Windows timer granularity (~15 ms).
func fastChatDelays() chatDelays {
	return chatDelays{
		keepaliveIdle:     60 * time.Millisecond,
		keepalivePongWait: 30 * time.Millisecond,
		keepaliveCheck:    20 * time.Millisecond,
	}
}

// TestDefaultChatDelaysMatchConstants pins production timing: the struct the
// constructor installs equals the constants that document it, and no field is
// left at its zero value (a zero check interval makes time.NewTicker panic).
func TestDefaultChatDelaysMatchConstants(t *testing.T) {
	want := chatDelays{
		keepaliveIdle:     ircKeepaliveIdle,
		keepalivePongWait: ircKeepalivePongWait,
		keepaliveCheck:    ircKeepaliveCheck,
	}
	if got := defaultChatDelays(); got != want {
		t.Fatalf("defaultChatDelays() = %+v, want %+v", got, want)
	}
	for name, pair := range map[string][2]time.Duration{
		"keepaliveIdle":     {want.keepaliveIdle, 45 * time.Second},
		"keepalivePongWait": {want.keepalivePongWait, 10 * time.Second},
		"keepaliveCheck":    {want.keepaliveCheck, 15 * time.Second},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s = %v, want %v (production timing must not move in this arc)",
				name, pair[0], pair[1])
		}
		if pair[0] == 0 {
			t.Errorf("chatDelays.%s has no default", name)
		}
	}
	// The detection bound the doc promises: one idle window, plus at most one
	// check tick to notice it, plus the pong window.
	if ircKeepaliveIdle+ircKeepaliveCheck+ircKeepalivePongWait >= ircReadDeadline {
		t.Error("the keepalive cannot detect a dead socket sooner than ircReadDeadline does; " +
			"it would be doing nothing at all")
	}

	cd := NewChatDownloader(ChatDownloaderOptions{ChannelLogin: "c", StreamID: "s"}, &testLogger{})
	if cd.delays != want {
		t.Errorf("NewChatDownloader installed %+v, want the defaults", cd.delays)
	}
}

// keepaliveServer is a websocket IRC server that completes the handshake,
// welcomes the client, and then goes QUIET — recording every line the client
// sends afterwards and, when answerPong is set, answering a client PING with a
// PONG.
//
// It is separate from ircReplier (chat_irc_fallback_test.go) on purpose: that
// one reads exactly one post-script line and then parks, which is the shape its
// own tests need. These tests need every line, for as long as the session runs.
type keepaliveServer struct {
	server *httptest.Server
	mu     sync.Mutex
	lines  []string
}

func (k *keepaliveServer) clientLines() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.lines...)
}

func (k *keepaliveServer) pings() int {
	n := 0
	for _, l := range k.clientLines() {
		if strings.HasPrefix(l, "PING") {
			n++
		}
	}
	return n
}

func startKeepaliveServer(t *testing.T, answerPong bool) *keepaliveServer {
	t.Helper()
	k := &keepaliveServer{}
	k.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")

		for range 4 { // PASS, NICK, CAP REQ, JOIN
			if _, _, readErr := conn.Read(r.Context()); readErr != nil {
				return
			}
		}
		if writeErr := conn.Write(r.Context(), websocket.MessageText,
			[]byte(":tmi.twitch.tv 001 justinfan1 :Welcome, GLHF!")); writeErr != nil {
			return
		}
		for {
			_, data, readErr := conn.Read(r.Context())
			if readErr != nil {
				return
			}
			line := string(data)
			k.mu.Lock()
			k.lines = append(k.lines, line)
			k.mu.Unlock()
			if answerPong && strings.HasPrefix(line, "PING") {
				if writeErr := conn.Write(r.Context(), websocket.MessageText,
					[]byte(":tmi.twitch.tv PONG tmi.twitch.tv :moombox")); writeErr != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(k.server.Close)

	prev := constants.TwitchURLs.IRCWS
	constants.TwitchURLs.IRCWS = "ws" + strings.TrimPrefix(k.server.URL, "http")
	t.Cleanup(func() { constants.TwitchURLs.IRCWS = prev })
	return k
}

// newKeepaliveTestDownloader is an ANONYMOUS downloader (no Credentials), so
// runIRCSession installs no handshake-outcome defer and nothing in these tests
// can be mistaken for a login verdict.
func newKeepaliveTestDownloader(t *testing.T) *ChatDownloader {
	t.Helper()
	cd := NewChatDownloader(ChatDownloaderOptions{
		ChannelLogin: "testchan",
		StreamID:     "stream-1",
		OutputPath:   t.TempDir() + "/chat.json",
	}, &testLogger{})
	cd.delays = fastChatDelays()
	cd.mu.Lock()
	cd.running = true
	cd.mu.Unlock()
	return cd
}

// TestIRCKeepalivePingsAndGivesUpOnASilentSocket is T3-25.
//
// A half-open socket — one the OS still believes is connected — used to cost up
// to ircReadDeadline (6 minutes) of chat, and Twitch IRC has NO replay: every
// message in that window is simply absent from the archive. So the session
// speaks first, exactly as chatterino7 does
// (references/chatterino7/src/providers/twitch/IrcConnection2.cpp).
//
// Mutants this kills:
//   - no client PING at all (today): the server records zero lines and
//     runIRCSession blocks until the test's own deadline.
//   - PING sent but no pong deadline: pings() keeps climbing and the session
//     never returns, so the reconnect that recovers chat never happens.
//   - counting the keepalive tick as a read error: the session would burn
//     chatMaxConsecutiveErrs and return a "too many IRC errors" wrapper
//     instead — the error-text assertion is what separates the two.
func TestIRCKeepalivePingsAndGivesUpOnASilentSocket(t *testing.T) {
	k := startKeepaliveServer(t, false)
	cd := newKeepaliveTestDownloader(t)

	done := make(chan error, 1)
	go func() { done <- cd.runIRCSession(context.Background()) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a silent socket ended the session with nil — Start's loop treats that as a " +
				"clean close and does not reconnect")
		}
		if !strings.Contains(err.Error(), "keepalive") {
			t.Errorf("session ended with %v, want the keepalive error", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the session never noticed the silent socket")
	}

	if got := k.pings(); got != 1 {
		t.Errorf("the client sent %d PINGs, want exactly 1", got)
	}
	for _, l := range k.clientLines() {
		if strings.HasPrefix(l, "PING") && l != "PING :moombox" {
			t.Errorf("client PING was %q, want %q", l, "PING :moombox")
		}
	}
}

// TestIRCKeepaliveKeepsAConnectionThatAnswers is the other edge: a quiet
// channel is not a dead socket. Twitch answering PONG must reset the idle clock
// and leave the session running.
//
// Mutant: failing on the pong deadline regardless of what arrived (e.g.
// comparing against the last DATA message rather than the last inbound FRAME) —
// a channel with no chatter would then reconnect every ~75 s forever, and each
// reconnect costs the messages in flight.
func TestIRCKeepaliveKeepsAConnectionThatAnswers(t *testing.T) {
	k := startKeepaliveServer(t, true)
	cd := newKeepaliveTestDownloader(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- cd.runIRCSession(ctx) }()

	// Long enough for several idle windows, so a session that fails on the
	// pong deadline has many chances to.
	time.Sleep(400 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("the session ended while the server was answering PONG: %v", err)
	default:
	}
	if got := k.pings(); got < 1 {
		t.Errorf("the client sent %d PINGs over 400ms of silence, want at least 1", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("a cancelled session returned %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the session ignored its cancelled context")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'ChatDelays|Keepalive' ./internal/twitch/`

Expected: FAIL to COMPILE — `undefined: chatDelays`, `undefined: defaultChatDelays`,
`undefined: ircKeepaliveIdle`, `cd.delays undefined`.

- [ ] **Step 3: Add the delays struct**

Create `internal/twitch/delays.go`:

```go
package twitch

import "time"

// chatDelays is every keepalive wait the IRC session sleeps on, in one place so
// tests can drive the same loop at millisecond scale. Production values are the
// package constants named beside each field (pinned by
// TestDefaultChatDelaysMatchConstants), so the constants remain the
// documentation of intent and this struct is the only knob. Tests assign it
// directly after NewChatDownloader, the way internal/engine's tests poke
// SegmentDownloader.delays; nothing outside the package sees it.
//
// ircReadDeadline is NOT here: no test waits on it, and it is the outer bound
// rather than a loop wait.
type chatDelays struct {
	keepaliveIdle     time.Duration // ircKeepaliveIdle — silence before we speak first
	keepalivePongWait time.Duration // ircKeepalivePongWait — how long an answer may take
	keepaliveCheck    time.Duration // ircKeepaliveCheck — how often the two above are evaluated
}

// defaultChatDelays returns production timing.
func defaultChatDelays() chatDelays {
	return chatDelays{
		keepaliveIdle:     ircKeepaliveIdle,
		keepalivePongWait: ircKeepalivePongWait,
		keepaliveCheck:    ircKeepaliveCheck,
	}
}
```

In `internal/twitch/chat.go`, add to the const block (after `ircReadDeadline`, `:27`):

```go
	// ircKeepaliveIdle is how long the session tolerates hearing NOTHING from
	// Twitch before it speaks first. Twitch's own server PING is every ~5 min,
	// so 45 s of silence is not by itself alarming — it is simply the point at
	// which asking is cheaper than waiting.
	ircKeepaliveIdle = 45 * time.Second
	// ircKeepalivePongWait is how long Twitch has to produce ANY inbound frame
	// after our PING before the socket is declared dead. A live connection
	// answers in milliseconds; this is generous enough to survive a stalled
	// second on a congested link.
	ircKeepalivePongWait = 10 * time.Second
	// ircKeepaliveCheck is how often the two windows above are evaluated. It
	// bounds the detection overshoot: a dead socket is noticed within
	// ircKeepaliveIdle + ircKeepaliveCheck + ircKeepalivePongWait ≈ 70 s,
	// against ircReadDeadline's 6 minutes. Twitch IRC has no replay, so every
	// second of that difference is chat that would have been lost outright.
	ircKeepaliveCheck = 15 * time.Second
	// ircKeepalivePing is the exact line the keepalive sends. IRC PING/PONG
	// rather than a WebSocket ping frame: a WS pong proves the socket is open,
	// while this proves the IRC layer behind it is still serving us.
	// chatterino7 pings the same way
	// (references/chatterino7/src/providers/twitch/IrcConnection2.cpp).
	ircKeepalivePing = "PING :moombox"
```

In `internal/twitch/chat.go`, add the field to `ChatDownloader` immediately after the `sessionCancel`
block (`:214-218`):

```go
	// delays is every keepalive wait runIRCSession sleeps on;
	// defaultChatDelays() in production, a scaled copy in tests (see delays.go).
	// Assigned once at construction and never written again, so the session
	// goroutine reads it without the mutex.
	delays chatDelays
```

and to the literal in `NewChatDownloader` (`:293-306`), after `dedup:`:

```go
		delays:          defaultChatDelays(),
```

- [ ] **Step 4: Add the keepalive to the session**

In `internal/twitch/chat_irc.go`, add `"sync/atomic"` to the import block.

Insert between the flusher goroutine's closing `}()` (`:255`) and `consecutiveErrors := 0` (`:257`):

```go
	// Client-initiated keepalive. ircReadDeadline alone leaves a HALF-OPEN
	// socket — one the OS still believes is connected — parked for six
	// minutes, and Twitch IRC has NO replay: every message in that window is
	// simply absent from the archive. So this session speaks first, the way
	// chatterino7 does
	// (references/chatterino7/src/providers/twitch/IrcConnection2.cpp).
	//
	// A goroutine rather than a shorter per-read deadline, and that is a
	// property of the library rather than a preference: coder/websocket
	// installs a read context as a context.AfterFunc that CLOSES the
	// connection when it fires (setupReadTimeout, conn.go), so a 15-second
	// read deadline would kill the socket on every quiet fifteen seconds.
	// Writes are serialized inside the library, so this goroutine's PING
	// cannot interleave with the read loop's PONG.
	var lastInbound atomic.Int64
	lastInbound.Store(time.Now().UnixNano())
	keepaliveFailed := make(chan struct{})
	keepaliveDone := make(chan struct{})
	defer close(keepaliveDone)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				cd.logger.Error("chat keepalive panic", "panic", r)
			}
		}()
		ticker := time.NewTicker(cd.delays.keepaliveCheck)
		defer ticker.Stop()
		// pingSent is the zero time when no PING is outstanding.
		var pingSent time.Time
		for {
			select {
			case <-keepaliveDone:
				return
			case <-sessionCtx.Done():
				return
			case now := <-ticker.C:
				last := time.Unix(0, lastInbound.Load())
				if !pingSent.IsZero() {
					// ANY inbound frame answers — a PONG, a chat line, a
					// server PING. The question is whether the IRC layer is
					// still serving us, not whether it used the right verb.
					if last.After(pingSent) {
						pingSent = time.Time{}
						continue
					}
					if now.Sub(pingSent) < cd.delays.keepalivePongWait {
						continue
					}
					cd.logger.Warn("twitch IRC went silent after a keepalive PING; reconnecting",
						"channel", cd.channelLogin, "pongWait", cd.delays.keepalivePongWait)
					close(keepaliveFailed)
					// Unblock the read loop, which reads keepaliveFailed and
					// returns the error Start's reconnect path acts on.
					sessionCancel()
					return
				}
				if now.Sub(last) < cd.delays.keepaliveIdle {
					continue
				}
				if err := conn.Write(sessionCtx, websocket.MessageText, []byte(ircKeepalivePing)); err != nil {
					// The read loop is about to see the same failure; leaving
					// it to the one error path keeps one verdict per session.
					return
				}
				pingSent = now
			}
		}
	}()
```

Replace the read-deadline comment (`:266-271`) with:

```go
		// Read with a per-read deadline so a silent socket (e.g. NAT dropping
		// the connection mid-stream) cannot block until the parent context
		// cancels. This is the OUTER bound and nothing more: Twitch sends a
		// server PING about every 5 minutes, so six is one missed heartbeat
		// plus slack. What actually detects a half-open socket is the
		// keepalive above, within ircKeepaliveIdle + one ircKeepaliveCheck
		// tick + ircKeepalivePongWait. Derived from sessionCtx so
		// Stop/MarkStreamEnded unblock the read at once — and note that
		// coder/websocket CLOSES the connection when this context fires, which
		// is why the keepalive is a goroutine and not a shorter deadline here.
```

In the read-error branch, insert between the `reauthPending` arm (`:284-286`) and
`consecutiveErrors++` (`:287`):

```go
			// The keepalive cancelled this session because Twitch stopped
			// answering. Returning the error (rather than counting a read
			// failure) is what makes Start's loop reconnect at once instead of
			// spinning chatMaxConsecutiveErrs reads against a cancelled
			// context.
			select {
			case <-keepaliveFailed:
				return fmt.Errorf("twitch IRC keepalive: no response within %v", cd.delays.keepalivePongWait)
			default:
			}
```

On the success path, record the frame. Replace `:294`:

```go
		consecutiveErrors = 0
```

with:

```go
		consecutiveErrors = 0
		// Every inbound FRAME, before any of it is interpreted: the keepalive's
		// question is whether Twitch is still talking to us at all.
		lastInbound.Store(time.Now().UnixNano())
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/twitch/`

Expected: PASS. Then confirm there is no data race on `lastInbound`/`keepaliveFailed`:

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -race -run 'Keepalive' ./internal/twitch/`

Expected: PASS, no `WARNING: DATA RACE`.

- [ ] **Step 6: Commit**

```bash
git add internal/twitch/delays.go internal/twitch/chat.go internal/twitch/chat_irc.go \
        internal/twitch/chat_keepalive_test.go
git commit -m "feat(twitch): client-initiated IRC keepalive detects a half-open socket

ircReadDeadline alone left a half-open socket parked for six minutes,
and Twitch IRC has no replay, so all of that chat was lost outright. The
session now sends PING :moombox after 45s without an inbound frame and
expects any frame within 10s, evaluated every 15s — a ~70s bound. The
durations live in an injectable chatDelays struct (the internal/engine
delays pattern) so the tests run in milliseconds against a fake IRC
WebSocket server. Also corrects the read-deadline comment (T4-34).

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq"
```

---

### Task 8: Living docs, and delete this plan

**Files:**
- Modify: `docs/spec/platform-services.md:542`, `:550`, `:597-602`, `:628`, `:636`, `:657-663`
- Modify: `docs/spec/data-and-storage.md:1082-1100` (the Chat Files section)
- Delete: `docs/superpowers/plans/2026-09-15-sweep-1-twitch-chat.md`

**Interfaces:**
- Consumes: every symbol produced by Tasks 1–7 — `parseEmoteTags`, `stripActionWrapper`,
  `TwitchChatMessage.IsAction`, `TwitchChatData.EmoteOffsets`, `chatEmoteOffsetsUTF16`,
  `correctLegacyTwitchEmotes`, `VodCommentEdge.Cursor`, `GetVodComments`, `emoteCacheTTL`,
  `chatResumeIDCap`, `ircKeepaliveIdle`/`ircKeepalivePongWait`/`ircKeepaliveCheck`.
- Produces: nothing consumed by later tasks (this is the arc's last).

**Citation rule (spec §3):** a backticked symbol immediately followed by a connector and a backticked
path is checked by `internal/docs` — the symbol must be DECLARED in that file. The pairs below are
correct as written; do not re-point them at a caller.

- [ ] **Step 1: Run the citation gate first, to see it green before the edits**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/docs/`

Expected: PASS. (This is the baseline, so a failure after the edits is unambiguously the edits.)

- [ ] **Step 2: Rewrite the PRIVMSG emote line**

`docs/spec/platform-services.md:542`, replace:

```markdown
- Emote tags parsed from format `id:start-end,start-end/id:start-end` into `TwitchEmoteRef` structs with start/end as rune indices (not byte indices), matching Twitch's character offset convention.
```

with:

```markdown
- Emote tags parsed from format `id:start-end,start-end/id:start-end` into `TwitchEmoteRef` structs. **Two index spaces, and the conversion between them is the point.** The WIRE offsets count Unicode CODE POINTS of the message text — inclusive, zero-based, and neither bytes nor UTF-16 units. That was re-measured on 2026-09-15 over the raw IRC lines of 18 real archives (120 of 120 ranges preceded by a non-BMP character slice to a whole-word token by code point, 0 of 120 by UTF-16) after this file and `parseEmoteTags` (`internal/twitch/chat_irc.go`) had asserted UTF-16 since 2026-04-22; every reference client indexes by code point (`references/chatterino7/src/providers/twitch/TwitchIrc.cpp` `codepointToUtf16Idx`, gempir/go-twitch-irc, robotty/twitch-irc-rs). The EMITTED `TwitchEmoteRef.Start`/`End` are UTF-16 code units, because the only consumer is JavaScript: the player slices the span with `String.prototype.substring`, and the VOD path emits UTF-16 already (`utf16Len`, `internal/twitch/api.go`). A range that is inverted or runs past the end of the message keeps its raw wire offsets and gets an empty `Name` — one unrendered emote, never a panic inside the read loop.
- A `/me` message arrives as the CTCP form `\x01ACTION <text>\x01` and its offsets index the UNWRAPPED text (measured the same way). `stripActionWrapper` (`internal/twitch/chat_irc.go`) unwraps it BEFORE the emote tags are read and the fact is reported as `IsAction` (`internal/twitch/types.go`, JSON `isAction`); `Raw` still carries the verbatim wire line. PRIVMSG only — a USERNOTICE body is never wrapped, and a strip there would eat the head of any system message that began with the marker.
```

- [ ] **Step 3: Document the keepalive**

`docs/spec/platform-services.md:550`, replace:

```markdown
**PING handling**: Responds with `PONG :tmi.twitch.tv`.
```

with:

```markdown
**PING handling**: a server `PING` is answered with `PONG :tmi.twitch.tv`.

**The session also speaks first.** `ircReadDeadline` (6 minutes) is the OUTER bound only. A half-open socket — one the OS still believes is connected — used to cost up to six minutes of chat, and Twitch IRC has no replay, so those messages are absent from the archive rather than late to it. A keepalive goroutine inside `runIRCSession` (`internal/twitch/chat_irc.go`) therefore sends `ircKeepalivePing` (`internal/twitch/chat.go` — the line `PING :moombox`) after `ircKeepaliveIdle` (45 s) without ANY inbound frame, and declares the socket dead if no inbound frame of any kind arrives within `ircKeepalivePongWait` (10 s); both windows are evaluated every `ircKeepaliveCheck` (15 s), so the detection bound is about 70 seconds. Any frame answers — a PONG, a chat line, a server PING — because the question is whether the IRC layer is still serving us, not which verb it used. Failure cancels the session and the read loop returns an error into the existing reconnect path. It is a goroutine rather than a shorter read deadline because `coder/websocket` CLOSES the connection when a read context fires, so a 15-second read deadline would kill the socket on every quiet fifteen seconds. The durations live in `chatDelays` (`internal/twitch/delays.go`) so the tests drive the whole cycle in milliseconds. Upstream shape: `references/chatterino7/src/providers/twitch/IrcConnection2.cpp`.
```

- [ ] **Step 4: Rewrite the VOD pagination section**

`docs/spec/platform-services.md:597-602`, replace:

```markdown
#### Pagination

- Initial request: `contentOffsetSeconds = 0` (or resumed offset).
- Each response includes `hasNextPage` and edges with `contentOffsetSeconds`.
- After processing each page, `contentOffset` advances to the last edge's offset.
- Termination: no results returned, no new (non-duplicate) messages, or `hasNextPage == false`.
```

with:

```markdown
#### Pagination

The operation is `VideoCommentsByOffsetOrCursor` and the OR is load-bearing: `contentOffsetSeconds` selects the page containing a moment, `cursor` selects the page AFTER a given edge, and they are mutually exclusive (`GetVodComments`, `internal/twitch/api.go`, sends exactly one). The persisted-query hash is the same for both.

- **Entering** a VOD — a fresh start, or a resume from the sidecar's `lastOffsetSeconds` — uses the offset.
- **Every later page** uses the previous page's LAST edge cursor (`Cursor`, `internal/twitch/types.go`). A page holds 59 edges and `contentOffsetSeconds` is an integer second, so a second with 59 or more comments made an offset-based next request ask for the page just read; the loop saw only duplicates, found the offset unmoved, and stopped with the rest of the VOD's chat unarchived. The cursor is not persisted — it is opaque and undocumented as durable across sessions, and a stale one answers with an empty page the loop would read as the end of the VOD.
- `contentOffset` still tracks the last edge's offset, because that is what the resume sidecar and the progress line are written from.
- Termination: an empty page, `hasNextPage == false`, a page with no cursor, or a cursor that repeats the one just used (the replacement for the old offset-advance guard, which the burst second tripped legitimately).
```

- [ ] **Step 5: Update the resume-cap and emote-cache lines**

`docs/spec/platform-services.md:628`, replace:

```markdown
Maximum 1000 recent IDs in VOD chat resume state (`vodChatResumeMaxRecentIDs`).
```

with:

```markdown
Both chat downloaders cap their sidecar at the newest 1000 dedup IDs — one constant, `chatResumeIDCap` (`internal/twitch/chat.go`). The IRC path used to snapshot its whole 5000-entry set on every flush (about once a second on a busy channel: a ~200 KB marshal, fsync and rename), for a window an IRC reconnect replay can only overlap by seconds.
```

`docs/spec/platform-services.md:636`, replace:

```markdown
All three providers are fetched in parallel using a `sync.WaitGroup`. Each has an 8-second timeout (`emoteTimeout`). Failures are logged at debug level and return nil (non-fatal).
```

with:

```markdown
All three providers are fetched in parallel using a `sync.WaitGroup`. Each has an 8-second timeout (`emoteTimeout`). Each returns its emotes AND whether it ANSWERED — a channel with no third-party emotes is a real answer; only a provider that could not be reached or whose body could not be read is a failure. Failures are logged at warn level and are non-fatal.
```

`docs/spec/platform-services.md:657-663`, replace the `#### Caching` list with:

```markdown
#### Caching

- **Type**: LRU (Least Recently Used) with a time-to-live.
- **Max size**: 200 channels.
- **Key**: lowercased `channelLogin` (preferred) or `channelID` (fallback).
- **Eviction**: When full, the oldest entry (by insertion order) is removed.
- **A failure is not a result**: the set is cached only when at least one provider answered. Three providers failing together — one outage, one flaky minute — used to be written to the cache and served for the rest of the process lifetime, and the per-downloader cache one layer up (`resolveEmotesCached`, `internal/twitch/chat_recording.go`) latched the same empty set for the life of the job. A resolve in which nothing answered now returns nil, and both layers retry.
- **An answer goes stale**: an entry older than `emoteCacheTTL` (`internal/twitch/emotes.go` — 24 hours) is refetched on the next resolve. Moombox runs for weeks and 7TV/BTTV/FFZ sets change daily. If that refetch fails outright the STALE set is served and kept — yesterday's emotes beat none.
- **Inflight dedup**: If a request for the same cache key is already in-flight, subsequent callers block on a channel until the first completes, then read from cache.
```

- [ ] **Step 6: Document the Twitch chat file's header in data-and-storage.md**

`docs/spec/data-and-storage.md`, insert immediately before the `**Incremental append pattern:**`
heading (after the `StreamStartTime` paragraph at `:1099`):

```markdown
**The Twitch chat file** (`TwitchChatData`, `internal/twitch/types.go`) has its own header: `platform`, `channelLogin`, `channelDisplayName`, `streamId`, `streamStartTime`, `recordingStartTime`, `downloadedAt`, `messageCount`, `emoteOffsets`, then the optional `emotes` object and the `messages` array. Field ORDER is load-bearing on both ends: every scalar precedes `emotes`/`messages` because `chatFileRecordingBaseMs` (`internal/twitch/chat.go`) stops its bounded header scan at the first composite value, and `messages` is last because the append path splices at the file's final `]`.

- `emoteOffsets` names the index space each message's `emotes[].start`/`end` count in. `"utf16"` is the only value any writer produces (`chatEmoteOffsetsUTF16`, `internal/twitch/chat.go`), and its ABSENCE is the era marker: a file written before 2026-09-15 holds the live IRC path's raw code-point offsets, because that path mistook Twitch's code-point offsets for UTF-16 units. The player repairs such a file once at load — `correctLegacyTwitchEmotes` (`web/public/modules/chat-timeline.js`) maps the spans and unwraps `/me` text, per part, before `mergePartChats` (which keeps no header scalars). Only messages carrying `raw` are touched: VOD comments have none and were UTF-16 all along. **Known window:** only the two full-file writers set the scalar, so a part file created before the change and appended to after it keeps an unmarked header while its newest messages already carry UTF-16 offsets; those few are over-shifted at replay, and the window closes at the next part roll or at job end.
- `isAction` marks a `/me` message. The CTCP wrapper `\x01ACTION …\x01` is stripped at parse time so `message` holds only the text and the emote offsets line up; `raw` keeps the verbatim IRC line.
```

- [ ] **Step 7: Run the doc gate**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/docs/`

Expected: PASS. If a `symbol not found in cited file` failure appears, the fix is to cite the file that
DECLARES the symbol, never to delete the symbol from the sentence.

- [ ] **Step 8: Delete this plan**

```bash
git rm docs/superpowers/plans/2026-09-15-sweep-1-twitch-chat.md
```

(The delete-implemented-plans rule: git history is the archive; the living docs just updated are what
carries the knowledge forward. If `git rm` reports `did not match any files`, the plan was never
committed on this branch — `rm` the path instead and carry on.)

- [ ] **Step 9: Run the whole arc's gates**

```bash
gofmt -l ./cmd ./internal ./tools ./web
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./...
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./...
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/twitch/... ./internal/web/routes/ ./internal/docs/
cd web/tests && node --test *.test.mjs
```

Expected: `gofmt -l` prints nothing; everything else PASSES.

- [ ] **Step 10: Commit**

```bash
git add docs/spec/platform-services.md docs/spec/data-and-storage.md
git commit -m "docs: Twitch chat offsets, ACTION, keepalive, cursor paging, emote cache

platform-services.md now states the real emote index rule (wire = code
points, emitted = UTF-16), the /me unwrap, the client-initiated IRC
keepalive and its ~70s detection bound, cursor-based VOD paging, the
shared resume-ID cap, and the emote cache's answered-only + 24h TTL
rules. data-and-storage.md documents the Twitch chat file header, its
emoteOffsets marker (and the one window where a file can lack it) and
isAction. Deletes the implemented arc plan.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq"
```

---

## Self-review

### 1. Spec coverage

| Ledger item | Spec §4 design item | Task | Evidence it is implemented |
|---|---|---|---|
| T1-1 (emote offsets) | 1 | Task 1 | `parseEmoteTags` reads code points, emits UTF-16; `TestParseEmoteTagsNonBMP` flips to `"25:2-6"` → `{Kappa,3,7}` and adds the real wire range and a two-emoji case; `TestParseEmoteTagsOutOfBounds` unchanged; the wrong-premise comment and test doc rewritten |
| T1-1 (`/me` ACTION) | 2 | Task 2 | `stripActionWrapper` runs before `parseEmoteTags`; `IsAction`/`isAction` on `TwitchChatMessage`; `"\x01ACTION Kappa\x01"` + `emotes=25:0-4` → `Message:"Kappa"`, `IsAction:true`, emote `Kappa/0/4`; `Raw` unchanged; USERNOTICE exempt |
| T1-1 (file marker + replay) | 3 | Task 3 | `EmoteOffsets`/`emoteOffsets` = `"utf16"` from both full-file writers; tolerance pinned for `AppendChatMessages`, `chatFileRecordingBaseMs` and `decodeChatPartFile` (through `readChatPartFileSummary`); `correctLegacyTwitchEmotes` + two `player.js` call sites; jsdom pins for (a) marked, (b) unmarked-with-`raw`, (c) unmarked-without-`raw`, plus the multi-part case |
| T1-3 (VOD paging) | 4 | Task 4 | `VodCommentEdge.Cursor`; `GetVodComments(…, cursor, authToken)`; fake GQL server with 59+59+10 edges all at one offset second, each with a cursor → 128 archived, stopping only on `hasNextPage=false`; resume still enters by `LastOffsetSeconds`; hash unchanged (asserted by the stub) |
| T1-11 (emote cache) | 5 | Task 5 | fetchers return `([]EmoteInfo, bool)`; cache only when one answered; `emoteCacheTTL = 24 * time.Hour` with an injectable `now`; expired → refetch, stale served on failure; all-failed → nil so `resolveEmotesCached` retries too; its false comment corrected |
| T2-15 (resume cap) | 6 | Task 6 | `chatResumeIDCap = 1000` shared by both `saveResumeState`s; 5000 seen IDs → newest 1000 in order; `vodChatResumeMaxRecentIDs` deleted |
| T3-25 (IRC keepalive) | 7 | Task 7 | `ircKeepaliveIdle`/`ircKeepalivePongWait`/`ircKeepaliveCheck` + `ircKeepalivePing`; injectable `chatDelays` (the Arc E pattern); fake WebSocket IRC server: silent → exactly one `PING :moombox` then an error into the reconnect path; answering → session survives; server PING handling and `ircReadDeadline` untouched |
| T4-34 (`chat_irc.go` comment) | 7 | Task 7 | The "two missed PINGs" comment is rewritten to state the real bounds and to name the keepalive as the actual detector |
| — (docs) | 8 | Task 8 | `platform-services.md` (offsets, ACTION, keepalive, cursor paging, cache, resume cap) and `data-and-storage.md` (Twitch chat header, `emoteOffsets`, `isAction`); plan deleted |

No spec §4 item is without a task. Two spec statements are deliberately not implemented as written and
are argued in "Planner's deviations" above: the replay correction's location (§4.3) and
`chat-timeline.js`'s scope (§4 packages).

### 2. Placeholder scan

Searched the plan for `TBD`, `TODO`, `implement later`, `fill in details`, `appropriate error
handling`, `add validation`, `handle edge cases`, `write tests for the above`, `similar to Task`. No
hits. Every code step carries the literal code; every test step carries the literal test; every run
step carries the command and the expected output. Task 3's multi-part jsdom pin builds its own job
inline rather than reusing `segmented(id)` (`web/tests/player.test.mjs:183-190`), because that
helper's segments carry no `chatFile` and the per-part fetch path would never fire — the comment in
the test says so, with both line ranges.

### 3. Type consistency

- `parseEmoteTags(emotesStr, message string) []TwitchEmoteRef` — same name and signature in Tasks 1,
  2 and 8. `stripActionWrapper(text string) (string, bool)` — Task 2 declares it, Task 8 cites it at
  its declaring file.
- `TwitchChatMessage.IsAction` / JSON `isAction` — Task 2 declares, Task 3's JS sets the same JSON
  name (`m.isAction`), Task 8 documents both spellings.
- `TwitchChatData.EmoteOffsets` / JSON `emoteOffsets`, value `chatEmoteOffsetsUTF16 = "utf16"` — Task 3
  declares all three; the JS gate reads `data.emoteOffsets === "utf16"`; Task 8 documents them.
- `correctLegacyTwitchEmotes(data)` — one name in `chat-timeline.js`, in both `player.js` call sites,
  in `chat-timeline.test.mjs`'s import and in Task 8's doc citation.
- `VodCommentEdge.Cursor`, `(*API).GetVodComments(ctx, vodID, contentOffsetSeconds, cursor, authToken)`
  and `(*Service).GetVodComments(ctx, vodID, contentOffsetSeconds, cursor)` — declared in Task 4 and
  used with exactly those arities at `vod_chat.go:179` and `service.go:59`.
- `emoteCacheEntry{data, fetchedAt}`, `EmoteResolver.now`, `emoteCacheTTL`, `evictIfFullLocked()`,
  `fetchBTTV/fetchFFZ/fetch7TV(...) ([]EmoteInfo, bool)` — Task 5's test uses exactly the names Task 5's
  implementation declares (this is why the four existing emote tests are rewritten in the same task:
  they poke `er.cache` directly and would otherwise not compile).
- `chatResumeIDCap` — Task 6 declares it in `chat.go` and deletes `vodChatResumeMaxRecentIDs`;
  Task 8's doc line cites `internal/twitch/chat.go`, the declaring file.
- `chatDelays{keepaliveIdle, keepalivePongWait, keepaliveCheck}` + `defaultChatDelays()` +
  `ChatDownloader.delays`, against the package constants `ircKeepaliveIdle`, `ircKeepalivePongWait`,
  `ircKeepaliveCheck`, `ircKeepalivePing`. The spec pins the three `irc*` identifiers and they exist
  under exactly those names; the struct fields drop the `irc` prefix, mirroring `internal/engine`'s
  `delays` (field `singleGoneRetry`, constant `singleGoneRetryDelay`) so a field and a package-level
  constant never share a name.
- `probeRoundTripper` (declared in `liveness_probe_test.go`) is reused by Tasks 4 and 5 rather than
  re-declared, so neither task introduces a duplicate type. Task 7's `keepaliveServer` is new and its
  doc comment says why `ircReplier` could not serve.
