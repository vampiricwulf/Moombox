# Sweep chain close Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the 2026-09-15 sweep fix chain by landing the five parked-minor groups (A1, A3, C, A2, A4) that the seven arcs deferred, then deleting this plan.

**Architecture:** Five independent tasks, each one commit, in cheap-to-expensive order. Four of them touch only tests, comments and docs; the fifth (A4) is the one real refactor — it lifts the watch-page brace scanner and the candidate-iteration loop out of `internal/youtube` into `internal/utils` so `internal/chat`'s twin extractor can share them, because `internal/chat` and `internal/youtube` do not (and must not) import each other while both already import `internal/utils`.

**Tech Stack:** Go 1.27 (stdlib `regexp`, `encoding/json`, `net/http/httptest`), no CGo, no new dependencies.

**Spec:** `docs/superpowers/specs/2026-09-15-sweep-fix-chain-design.md`
**Brief:** `.superpowers/sdd/2026-09-15-sweep-fix-chain/chain-close-brief.md` (groups A + C; groups B and D go to the owner report, not to this branch)

## Global Constraints

Copied verbatim from spec §3 — every task's requirements implicitly include all of it:

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

**Arc gate list (branch `sweep-close`), run before the merge candidate:**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/monitor/ ./internal/worker/ \
  ./internal/engine/ ./internal/utils/ ./internal/youtube/ ./internal/chat/ ./internal/docs/
gofmt -l ./cmd ./internal ./tools ./web          # must print nothing
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./...
staticcheck ./...                                # pinned 2026.2.1, clean
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./...
GOOS=linux GOARCH=amd64 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./...
GOOS=linux GOARCH=arm64 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./...
# ONE controller-run `go test -count=1 ./...` at merge time (spec §2)
MOOMBOX_LIVE_YT_TEST=1 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 \
  -run 'TestLivePublicExtraction|TestLiveLoginMarkersPresent' ./internal/youtube/
```

The package list is exactly the packages these six tasks touch, plus `./internal/docs/` because Tasks 1
and 5 edit `docs/spec/*.md` and Task 5 moves a Go symbol between packages. No JS changes, so the node
suite is not a gate for this branch. The LAST line is an internet test: Task 5 rewrites the watch-page
player-response and `ytInitialData` extractors, which is exactly the code the live gate exists to cover
(see `reference_live_tests_catch_client_death` — assert CAPABILITIES, not mechanisms).

**Worktree recipe (spec §2):** `git worktree add -b sweep-close .worktrees/sweep-close main` (cut AFTER
Arc 7 is merged), then copy the gitignored inputs a fresh worktree lacks:
`internal/bgutils/embed/{node-windows-amd64.gz,node-linux-amd64.gz,node-linux-arm64.gz,sidecar.tar.gz}`,
`internal/cipher/testdata/*.js`, and `cd web/tests && npm ci --no-audit --no-fund`. Prefix every go
command with `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp`.

## File Structure

| File | Task | Responsibility |
|---|---|---|
| `internal/monitor/decapi_test.go` | 1 | Pins the dateless-narrowing guard with a second cycle |
| `internal/monitor/feed.go` | 1 | `armMembershipLiveness` doc comment: one orphaned line rewrapped |
| `docs/spec/data-and-storage.md` | 1 | The liveness-nominee sentence gains the "not recently errored" qualifier |
| `internal/worker/orchestrator_chat.go` | 2 | Comment: the watch page has been `[]byte` since Arc 3 |
| `internal/worker/part_merge.go` | 2 | `mergeChatFiles` doc: the fifth chat writer and its missing marker |
| `docs/superpowers/specs/2026-09-15-sweep-fix-chain-design.md` | 3 | New `## Errata (chain close, 2026-09-15)` section |
| `internal/engine/downloader_fetch_cancel_test.go` | 4 | Table over the five `reportFetchFailure` sites; one reporter double |
| `internal/engine/downloader_fetch_headprobe_test.go` | 4 | The header-present-but-unparseable fallback case |
| `internal/engine/downloader_test.go` | 4 | `fakeReporter` deleted; its one user repointed at `countingReporter` |
| `internal/utils/http_test.go` | 4 | Both cancel goroutines gain a recover that the test body reads |
| `internal/utils/jsoncandidates.go` (new) | 5 | `ScanBalancedJSONObject`, `FindJSONObjectCandidate`, `IsNonEmptyJSONObject` |
| `internal/utils/jsoncandidates_test.go` (new) | 5 | The iterator's own pins, including the rejected-offset set |
| `internal/youtube/watch_page.go` | 5 | `scanBalancedObject` removed; `extractPlayerResponse` uses the iterator |
| `internal/youtube/channel_membership.go` | 5 | `extractYtInitialData` iterates candidates |
| `internal/youtube/watch_page_test.go` | 5 | One new row: a forged `ytInitialData` loses to the real one |
| `internal/chat/api.go` | 5 | `ExtractChatContinuation([]byte)`, anchors instead of the lazy regex |
| `internal/chat/api_continuation_test.go` (new) | 5 | The chat twin's forged-candidate and window-form pins |
| `docs/spec/platform-services.md` | 5 | Three sentences follow the scanner to its new home |
| `docs/superpowers/plans/2026-09-15-sweep-close.md` | 6 | Deleted (implemented-plans rule) |

---

### Task 1: Monitor parked minors (group A1)

Three unrelated one-liners that Arc 4 parked, in the one package. They are one commit because none of
them is worth a reviewer's gate on its own and all three are `internal/monitor` + its doc.

**Files:**
- Modify: `internal/monitor/decapi_test.go:150-170` (`TestDecapi_DateFetchErrorSkips`)
- Modify: `internal/monitor/feed.go:761-770` (`armMembershipLiveness` doc comment)
- Modify: `docs/spec/data-and-storage.md:867`

**Interfaces:**
- Consumes: nothing from other tasks.
- Produces: nothing other tasks rely on. No Go symbol is added, renamed or removed.

- [ ] **Step 1: Read the memo machinery before touching the test**

Read, in this order, so the assertion below is written against the real structure and not a guess:
`internal/monitor/decapi.go:744-758` (the §13 window-skip arm and the `result.PublishedAt != ""` guard
inside it), `:680-684` (the `terminalMemoHit` gate that runs BEFORE the probe), `:793-841`
(`terminalMemoHit`, `recordTerminalMemo`, `noteTerminalMemoOutsideWindow`), and
`internal/monitor/decapi_test.go:352-420` (`TestDecapi_TerminalMemoCoversTheOutOfWindowArm` — the test
this new assertion is the negative twin of).

The fact the new assertion turns on: with `IncludeNonLiveContent: true` the vod arm writes no history
row, so `reprobe` stays false forever and the ONLY thing that can make `terminalMemoHit` return true is
`m.outsideWindow && m.windowDays == windowDays` — which is precisely what the guard withholds when the
date could not be fetched.

- [ ] **Step 2: Write the failing test (extend the existing one with a second cycle)**

Replace `TestDecapi_DateFetchErrorSkips` (`internal/monitor/decapi_test.go`) in full:

```go
// TestDecapi_DateFetchErrorSkips: a FAILED date fetch leaves the window
// unverifiable ⇒ the §13 treated-as-outside arm (no job, Info-logged);
// a later sighting retries.
//
// The second cycle is the pin for the guard at decapi.go:747
// (`if result.PublishedAt != ""` inside the window-skip arm). A DATED
// verdict is durable and is memoized; a DATELESS one must not be, because
// "treated as outside" there means only that this cycle could not verify the
// window. Latching it would let one transient DECAPI/date failure freeze the
// channel until its newest video changes — and on an
// include_non_live_content channel the out-of-window arm writes no history
// row, so `reprobe` never re-opens it either.
//
// Mutant this kills: deleting the `if result.PublishedAt != ""` guard so the
// dateless skip calls noteTerminalMemoOutsideWindow. Cycle 2 then hits the
// memo and costs no requests at all — probes/dates stay 1/1. (Before this
// cycle existed, deleting the guard passed the whole package.)
func TestDecapi_DateFetchErrorSkips(t *testing.T) {
	db := newTestDB(t)
	probes, dates := 0, 0
	dm := newTestDecapiMonitor(t, db, func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		probes++
		return &VideoProbeResult{StreamStatus: "vod", Title: "fresh vod"}, nil
	})
	dm.ProbeDate = func(ctx context.Context, videoID string) (string, string, error) {
		dates++
		return "", "", fmt.Errorf("boom")
	}
	found := recordDecapiVideoFound(dm)

	// include_non_live_content keeps the vod arm from writing a history row,
	// so `reprobe` is false on every cycle and the memo's outsideWindow arm
	// is the only one that could ever fire here.
	ch := &config.ChannelConfig{ID: "UC1", Name: "UC1", IncludeNonLiveContent: true}
	body := decapiBody("vidDecTpe05", "fresh vod")

	if err := dm.processResponse(context.Background(), body, ch); err != nil {
		t.Fatalf("cycle 1: processResponse: %v", err)
	}
	if len(*found) != 0 {
		t.Fatalf("a window-unverifiable vod must not job: %v", *found)
	}
	if probes != 1 || dates != 1 {
		t.Fatalf("cycle 1: probes=%d dates=%d, want 1/1 — the first sighting classifies and tries to date the video", probes, dates)
	}

	if err := dm.processResponse(context.Background(), body, ch); err != nil {
		t.Fatalf("cycle 2: processResponse: %v", err)
	}
	if probes != 2 || dates != 2 {
		t.Fatalf("cycle 2: probes=%d dates=%d, want 2/2 — a DATELESS treated-as-outside skip must not be memoized, or one transient date failure freezes the channel until its newest video changes", probes, dates)
	}
	if len(*found) != 0 {
		t.Fatalf("a window-unverifiable vod must not job on a later sighting either: %v", *found)
	}
}
```

- [ ] **Step 3: Run it — it must PASS on unmodified source, then prove the mutant dies**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestDecapi_DateFetchErrorSkips ./internal/monitor/
```
Expected: PASS (this test pins existing behaviour; it is not a behaviour change).

Now prove it is a real pin. Temporarily delete the guard at `internal/monitor/decapi.go:747` — that is,
replace

```go
			if result.PublishedAt != "" {
				// … comment …
				dm.noteTerminalMemoOutsideWindow(ch.ID, videoID, windowDays)
			}
```

with the bare call `dm.noteTerminalMemoOutsideWindow(ch.ID, videoID, windowDays)` — and re-run.
Expected: FAIL with `cycle 2: probes=1 dates=1, want 2/2`. **Restore the guard immediately**
(`git diff internal/monitor/decapi.go` must be empty before Step 4; `decapi.go` is NOT in this task's
commit pathspec).

- [ ] **Step 4: Rewrap the orphaned comment line in feed.go**

`internal/monitor/feed.go:761-770` currently leaves `routeLivenessVerdict` alone on line 763. Replace
lines 761-770 with the same words rewrapped (no wording change, no line holding a single identifier):

```go
// The fetch feeds a verdict — cmd/moombox's FetchMembership adapter hands the
// SessionAuthState to (*cookies.RefreshService).ObserveLiveness — but the
// routeLivenessVerdict that gets it there forwards only LoggedIn/LoggedOut, so
// a page carrying no login marker observes nothing even though the fetch
// succeeded. That residue is the tier-2 FallbackLiveness probe's job: it runs
// precisely when no conclusive observation has landed recently
// (livenessObservedRecently in internal/cookies/refresh_liveness.go; wired to
// ProbeAccountLiveness in cmd/moombox/services.go). This floor is the cheap
// first tier, not the whole guarantee.
```

- [ ] **Step 5: Correct the liveness-nominee sentence in the spec doc**

In `docs/spec/data-and-storage.md:867`, the parenthetical currently reads:

> and `armMembershipLiveness` nominates the memoized channel with the earliest horizon so the signal keeps firing on an install where every channel is memoized

Replace exactly that span with:

> and `armMembershipLiveness` nominates the memoized channel with the earliest horizon **that has not recently errored** (`membershipFetchErrored`, `internal/monitor/feed.go`) so the signal keeps firing on an install where every channel is memoized — when every candidate has errored the earliest errored one is tried anyway, bounded by `membershipLivenessMaxTries`

Verified in source before writing it: `armMembershipLiveness` (`internal/monitor/feed.go:796-869`)
skips a channel in `fm.membershipFetchErrored` when picking `earliestID`, collects the earliest errored
one separately as `erroredID`, and falls back to it only when the preferred slot came up empty.

- [ ] **Step 6: Run the gates**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/monitor/ ./internal/docs/
gofmt -l ./internal/monitor
```
Expected: both packages `ok`; `gofmt -l` prints nothing. `./internal/docs/` is gated because
`data-and-storage.md` changed and the new text carries a symbol+path citation pair
(`membershipFetchErrored`, `internal/monitor/feed.go`) that the citation test will resolve against the
declaring file.

- [ ] **Step 7: Commit**

```bash
git add internal/monitor/decapi_test.go internal/monitor/feed.go docs/spec/data-and-storage.md
git commit -m "fix(monitor): pin the dateless-narrowing guard, unwrap an orphan comment line, qualify the liveness nominee

The DECAPI window-skip arm only memoizes a DATED verdict (decapi.go:747);
that guard was deletable with the whole package still green. A second cycle
in TestDecapi_DateFetchErrorSkips now pins it: a dateless treated-as-outside
skip must cost a fresh probe and date fetch next cycle.

armMembershipLiveness' doc comment left routeLivenessVerdict alone on its own
line; rewrapped, same words. data-and-storage.md said the nominee is the
memoized channel with the earliest horizon — it is the earliest horizon that
has not recently errored, with the errored ones retried only when every
candidate has failed.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/monitor/decapi_test.go internal/monitor/feed.go docs/spec/data-and-storage.md
```

---

### Task 2: Stale comments in internal/worker (group A3)

Comments only. No behaviour changes, so no TDD cycle — the deliverable is that two comments stop
asserting things that are false or missing.

**Files:**
- Modify: `internal/worker/orchestrator_chat.go:34-36`
- Modify: `internal/worker/part_merge.go:544-555` (`mergeChatFiles` doc comment)

**Interfaces:**
- Consumes: nothing.
- Produces: nothing. No Go symbol changes.

- [ ] **Step 1: Verify the premise before rewriting the first comment**

Read `internal/youtube/watch_page.go` `FetchWatchPage` and its result type, and confirm two things:
the page is read as `[]byte` and the extractors read it in place (no `string(body)` copy), and the
returned `ChatContinuation` is produced by `json.Unmarshal` (which allocates a fresh string and
therefore does not alias the page's backing array — the comment at `watch_page.go:598-604` states this).
Write the replacement only against what you actually read.

- [ ] **Step 2: Rewrite the orchestrator comment**

`internal/worker/orchestrator_chat.go:34-36`, replace:

```go
	// Chat continuation is extracted at watch-page parse time (see watch_page.go);
	// reading from the result avoids re-parsing the 5 MB HTML and lets the body
	// string be GC'd before this point.
```

with:

```go
	// Chat continuation is extracted at watch-page parse time (see watch_page.go);
	// reading from the result avoids re-parsing the ~5 MB HTML. There is no body
	// STRING to collect: since the 2026-09-15 sweep (Arc 3) FetchWatchPage reads
	// the page as []byte and its extractors read it in place, and the token
	// json.Unmarshal produced does not alias the page — so the result retains
	// none of those bytes and the page is collectable by the time this runs.
```

- [ ] **Step 3: Extend the mergeChatFiles doc comment**

`internal/worker/part_merge.go`, append to the existing doc comment (after the sentence ending
`see merge's doc comment.`, before `func mergeChatFiles`):

```go
// This is a FIFTH chat writer, and the only one that writes a chat file with
// no emoteOffsets marker (chatEmoteOffsetsUTF16, internal/twitch/chat.go; the
// IRC full-file write and the VOD write are the two that carry it). That is
// harmless today for exactly the reason above: every Twitch part this can be
// handed fails the chat.ChatData unmarshal, so it never produces a Twitch
// file at all. If it is ever taught to merge twitch.TwitchChatData, it MUST
// carry chatEmoteOffsetsUTF16 into the merged file — a Twitch chat file
// without the marker is indistinguishable at replay from a legacy
// code-point-offset file and would be "corrected" a second time.
```

- [ ] **Step 4: Run the gates**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/worker/
gofmt -l ./internal/worker
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/worker/
```
Expected: `ok`, no gofmt output, vet silent.

- [ ] **Step 5: Note the two already-done docs items (do not edit them)**

Brief item A3.7 is already satisfied and MUST NOT be re-edited — verified at this branch's base:
- `docs/spec/data-and-storage.md:759-762` carries the `Reload()` / `Load` `(size, mtime)` memo clause
  that Arc 7 Task 10 added.
- `docs/spec/user-interfaces.md:267` (the `R Y` chord row) and `:879` (the
  `GET /api/ytdlp-plugin/status` row) both describe the unrecognised-file row and the `unparseable`
  eighth key.

Record this verification in the task report; change nothing.

- [ ] **Step 6: Commit**

```bash
git add internal/worker/orchestrator_chat.go internal/worker/part_merge.go
git commit -m "docs(worker): correct the watch-page body comment; name the fifth chat writer's missing marker

orchestrator_chat.go claimed reading the continuation off the result lets the
body STRING be collected; there has been no string since Arc 3 — the page is
a []byte the extractors read in place and the result retains none of it.

mergeChatFiles is a fifth chat writer with no emoteOffsets marker. Harmless
today because it always errors on Twitch parts; the doc now says what has to
happen if that ever changes.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/worker/orchestrator_chat.go internal/worker/part_merge.go
```

---

### Task 3: Spec errata (group C)

Docs only, and not a `docs/spec/*.md` file: the citation test reads only `docs/spec/<name>` for six
named files (`internal/docs/citations_test.go:19-25`, `docLines` at `:126-134`), so the chain design
doc under `docs/superpowers/specs/` is not scanned. `./internal/docs/` is still run as a no-op sanity.

**Files:**
- Modify: `docs/superpowers/specs/2026-09-15-sweep-fix-chain-design.md` (append one section at EOF)

**Interfaces:**
- Consumes: nothing.
- Produces: nothing.

- [ ] **Step 1: Re-read the four spec passages the errata correct**

Before writing anything, read: §5 items 1 and 5 (`:134-146` and `:161-163`), §6 item 7 (`:206-208`),
§7 item 3 (`:220-224`), and §12 (`:376-381`). Then read the three source facts the errata assert:
`internal/monitor/twitch_recover.go` + its one caller `internal/monitor/twitch.go:405`;
`internal/cipher/solver.go:14-19` (`solverCacheSize = 10`, "the previous cap of 3");
`cmd/moombox/launcher_windows_test.go:63-120` (the shipped case-(b) test, whose body comment already
states the corrected `MoveFileEx` premise).

- [ ] **Step 2: Append the errata section**

Append this to the END of `docs/superpowers/specs/2026-09-15-sweep-fix-chain-design.md`, verbatim,
preceded by one blank line:

```markdown
## Errata (chain close, 2026-09-15)

Corrections recorded at chain close. The design sections above are left as written — they are the
record of what was PLANNED. These entries are what the implementation proved, and they win wherever
they disagree with the text above.

1. **§5.1 — "job → Error, which `isRecoverableTwitchError` can recover" is false.**
   `isRecoverableTwitchError` (`internal/monitor/twitch_recover.go`) has exactly one caller: the Twitch
   MONITOR, when it sights a live channel whose existing job is already in `Error`
   (`internal/monitor/twitch.go:405`). It is not a hand-off the HLS loop reaches, and the YouTube
   orchestrators finalize an errored job themselves. So the gain from deferring the end verdict is not
   "the job gets recovered": it is that an unverifiable playlist failure RETRIES instead of truncating
   the recording as Finished, and that the resume sidecar survives for a later `/resume`. The design's
   behaviour is unchanged and correct; only that clause of its justification was wrong.

2. **§5.5 — the 4xx drain was DROPPED, not implemented.** Go 1.27's `net/http` `Transport` already
   drains an unread response body of up to 256 KiB when the caller closes early (`maybeDrainBody`), so
   the brief's RED did not reproduce and the specified 64 KiB drain could not rescue a connection the
   stdlib does not already keep (verified with a 300 KiB body: red without the fix, still red with it).
   Arc 2 Task 5 was dropped by ruling and committed nothing. A body larger than 256 KiB is beyond any
   sane drain cap, so nothing is owed here.

3. **§7.3 — "so `ObserveLiveness` still receives a verdict" overstates the floor.** What
   `armMembershipLiveness` guarantees is one membership fetch per cycle that RETURNS, and only while
   some memoized channel has not recently errored; when every candidate has errored, the retry is
   bounded by `membershipLivenessMaxTries` and the cycle may legitimately end with nothing returned. A
   fetch that returns is also not the same thing as a verdict: `routeLivenessVerdict` forwards only
   `LoggedIn`/`LoggedOut`, so a page carrying no login marker observes nothing even after a successful
   fetch. Tier-2 `FallbackLiveness` remains the backstop for BOTH gaps — it is not a redundancy the
   memo made optional.

4. **§6.7 — "3-slot LRU" is the figure being CORRECTED, not a current one.** The sentence is written in
   the present tense against a state Arc 3 has since fixed; read it as past tense. The cipher solver
   cache holds at most 10 compiled VMs (`solverCacheSize`, `internal/cipher/solver.go`) — it was 3
   until 2026-04, when multi-channel monitoring routinely holding 4+ active player URLs made the
   smaller cap re-compile constantly. `.claude/skills/moombox-upstream-porting/SKILL.md` and
   `docs/spec/platform-services.md` both state 10 as of Arc 3.

5. **§10.1 — the case-(b) premise was wrong and was corrected inside Arc 7.** The design assumed that a
   second update in one launcher lifetime fails `os.Rename(.old → ~)` because a `~` file is already
   there. It does not: Go's `os.Rename` on Windows is `MoveFileEx` with `MOVEFILE_REPLACE_EXISTING`,
   which replaces a plain `~` file. The rename fails only when something denies delete-sharing on that
   name — in the field the launcher's own mapped image (which is also why the child's
   `CleanupOldBinary` could not delete it), or a directory at that path. The shipped test
   (`TestHandleUpdateRestartReportsARenameItCouldNotDo`, `cmd/moombox/launcher_windows_test.go`) pins
   the corrected premise and reproduces the failure with an ordinary open `*os.File` handle, which
   denies exactly the same sharing mode. The FIX (report the failure, prefer a surviving `.old` as the
   rollback artifact) is unaffected — it is right for the real failure mode too.

6. **Arcs 5 and 6 — the node-suite test counts in those plans were written against a 159 baseline.**
   Arc 1 raised `node --test web/tests/*.test.mjs` to 169 before either arc ran, so every absolute
   count quoted in their task steps was stale by 10 on arrival. Ruling taken at the time: the counts
   are informational, and implementers report the count they actually observe. The gate is the suite
   passing, never a number. Those plans are deleted, so this entry is the surviving record.

7. **Arc 6 — commit `fedf98c5` carries TWO tasks.** A pathspec-less `git commit` swept Task 5's
   already-staged `internal/jobfilter` files into Task 4's commit, whose subject names only T2-20d. The
   commit contains T2-20d (the TUI log-level cache) AND T2-24 (the jobfilter case-insensitive fold);
   both halves were reviewed separately, by pathspec-scoped diffs. Ruling: no history rewrite — the six
   hashes are cited across the ledgers and review packages, and the merge commit body records the
   pairing. The process rule this produced is now in the global constraints: implementers commit with
   `git commit -m … -- <their files>`, pathspec on the commit too.
```

- [ ] **Step 3: Run the no-op sanity gate**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/docs/
gofmt -l ./cmd ./internal ./tools ./web
```
Expected: `ok github.com/vampiricwulf/Moombox/internal/docs`; no gofmt output. (Neither can react to
this file; both are run to prove the branch is still clean.)

- [ ] **Step 4: Commit**

```bash
git add docs/superpowers/specs/2026-09-15-sweep-fix-chain-design.md
git commit -m "docs(spec): errata for the sweep fix chain design

Seven corrections recorded at chain close: the §5.1 recovery clause (the
orchestrators finalize; the gain is retry-not-truncate plus sidecar
survival), §5.5 dropped as stdlib-subsumed, §7.3's overstated liveness floor,
§6.7's 3-slot figure (10 VMs since 2026-04), Arc 7's case-(b) rename premise
(MoveFileEx replaces a plain ~; only a held handle blocks it), the Arc 5/6
node counts written against a 159 baseline, and fedf98c5 carrying two tasks.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- docs/superpowers/specs/2026-09-15-sweep-fix-chain-design.md
```

---

### Task 4: Engine cancel-site table, unparseable head header, one reporter double (group A2)

Arc 2's three parked minors, all test-side. Item 8 (a table over the five `reportFetchFailure` sites,
plus the unparseable-header case Task 4's review parked), item 9 (the utils cancel goroutines gain the
project's inline recover), item 10 (`countingReporter`/`fakeReporter` unified).

**Files:**
- Modify: `internal/engine/downloader_fetch_cancel_test.go` (whole file rewritten around the table)
- Modify: `internal/engine/downloader_fetch_headprobe_test.go` (one new test appended)
- Modify: `internal/engine/downloader_test.go:672-678` (delete `fakeReporter`) and `:700-730`
  (`TestConnectivityReporter_SetAndClear` repointed)
- Modify: `internal/utils/http_test.go:96-100` and `:209-213` (both cancel goroutines)

**Interfaces:**
- Consumes: `reportFetchFailure(parent context.Context, tag string)` (`internal/engine/downloader_fetch.go:148`)
  and its five call sites — `fetchSegment` (`:230`), `probeHeadAt` (`:531`), `probeFileSize` (`:577`),
  `fetchChunk` (`:655`), `ProbeSegmentAvailable` (`internal/engine/eviction_probe.go:129`).
- Produces (test-only, same package): `type fetchSite struct { name string; call func(*testing.T, context.Context, *SegmentDownloader) }`
  and `var engineFetchSites []fetchSite` in `internal/engine`; `func goCancelAfter(t *testing.T, d time.Duration, cancel context.CancelFunc) func()`
  in BOTH `internal/engine` and `internal/utils` (separate packages, separate declarations).
  `countingReporter` becomes the engine package's only `ConnectivityReporter` double.

- [ ] **Step 1: Read the five sites and the Arc 2 ledger entries that parked these items**

Read `internal/engine/downloader_fetch.go:140-153` (`reportFetchFailure` and its doc — the guard asks
the PARENT, deliberately), then each of the five call sites and the function around it: `fetchSegment`
(`:213-250`), `probeHeadAt` (`:517-563` — note its derived timeout is a hardcoded 10 s, so only a
parent cancel can finish it early), `probeFileSize` (`:564-605` — returns only an `int64`, no error),
`fetchChunk` (`:641-680`), `ProbeSegmentAvailable` (`internal/engine/eviction_probe.go:115-145`).
Then read `.superpowers/sdd/2026-09-15-sweep-2-engine/progress.md` lines 20-48 — the Task 4 review
minor ("the header-present-but-unparseable sub-case has no dedicated test") and the Task 6 review
("(gap) eviction_probe.go is a FIFTH identical site", "countingReporter duplicates fakeReporter") are
the exact items this task closes.

- [ ] **Step 2: Write the failing unparseable-header test**

Append to `internal/engine/downloader_fetch_headprobe_test.go`:

```go
// TestProbeHeadSequenceFallsBackOnAnUnparseableHeader pins the sub-case Arc 2
// Task 4's review parked: the edge ANSWERED and sent X-Head-Seqnum, but the
// value is not a number. probeHeadAt wraps that as
// `%w: parse %q: %v` around errNoHeadSeqUsable, so it is still an
// answered-but-unusable outcome and the currentSeq+1000 fallback must fire —
// exactly as it does for a missing header. An opaque error page from a CDN
// that stamps a non-numeric header is the field shape.
//
// Mutant this kills: comparing with `err == errNoHeadSeqUsable` instead of
// errors.Is in probeHeadSequence — the wrapped parse error then fails the
// check, no fallback runs, and the server sees 1 probe instead of 2.
func TestProbeHeadSequenceFallsBackOnAnUnparseableHeader(t *testing.T) {
	var probes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		if strings.Contains(r.URL.String(), "999999999") {
			w.Header().Set("X-Head-Seqnum", "not-a-number") // present, unparseable
			fmt.Fprint(w, "nope")
			return
		}
		w.Header().Set("X-Head-Seqnum", "1500")
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(srv.Close)

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/v?x=1",
		OutputFile: filepath.Join(t.TempDir(), "video_stream"),
	})
	d.currentSeq.Store(500) // > 0, so the fallback is reachable

	seq, err := d.probeHeadSequence(t.Context())
	if err != nil {
		t.Fatalf("probeHeadSequence() = %v, want the fallback's answer", err)
	}
	if seq != 1500 {
		t.Errorf("probeHeadSequence() = %d, want 1500 (the fallback probe's header)", seq)
	}
	if got := probes.Load(); got != 2 {
		t.Errorf("server saw %d probes, want 2 — an unparseable header is answered-but-unusable, same as a missing one", got)
	}
}
```

- [ ] **Step 3: Run it, then prove the mutant**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestProbeHeadSequenceFallsBackOnAnUnparseableHeader ./internal/engine/
```
Expected: PASS (it pins existing behaviour). Then temporarily change
`internal/engine/downloader_fetch.go:518` from
`if cur := int(d.currentSeq.Load()); cur > 0 && errors.Is(err, errNoHeadSeqUsable) {` to
`… && err == errNoHeadSeqUsable {` and re-run. Expected: FAIL with
`probeHeadSequence() = ... parse "not-a-number" ...`. **Revert `downloader_fetch.go` immediately** — it
is not in this task's commit pathspec.

- [ ] **Step 4: Rewrite downloader_fetch_cancel_test.go around the table**

Replace the whole file. Three of its four current tests become table rows (each new table covers all
FIVE sites, so nothing is lost); `TestFetchSegmentDerivedTimeoutIsAConnectivityFailure` stays as-is
because the table cannot express it — only `fetchSegment` has a shrinkable derived timeout
(`SegmentTimeout`), so it is the one place where the parent stays alive while the derived context
finishes.

```go
package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// countingReporter is an atomic ConnectivityReporter: the engine's reporter is
// a package global, so a plain-int recorder is not safe to install while any
// other test is in flight. It is the package's ONLY reporter double.
type countingReporter struct {
	fails     atomic.Int32
	successes atomic.Int32
}

func (c *countingReporter) ReportFailure(string) { c.fails.Add(1) }
func (c *countingReporter) ReportSuccess(string) { c.successes.Add(1) }

// goCancelAfter runs cancel after d in a goroutine carrying the project's
// inline recover (every goroutine has one). A panic there would otherwise take
// the whole test binary down with a stack naming only this line. It cannot
// call t.Fatal itself — no t method is legal from a goroutine that may outlive
// the test — so the value is handed back through a buffered channel and the
// returned join func, which the test body calls while it is still running,
// reports it against the right test.
func goCancelAfter(t *testing.T, d time.Duration, cancel context.CancelFunc) func() {
	t.Helper()
	panicked := make(chan any, 1)
	done := make(chan struct{})
	go func() {
		defer close(done) // registered first, so it runs AFTER the recover below
		defer func() {
			if r := recover(); r != nil {
				panicked <- r
			}
		}()
		time.Sleep(d)
		cancel()
	}()
	return func() {
		t.Helper()
		<-done
		select {
		case r := <-panicked:
			t.Fatalf("the cancel goroutine panicked: %v", r)
		default:
		}
	}
}

// fetchSite is one of the five places the engine reports a connectivity
// failure from (reportFetchFailure, downloader_fetch.go:230/:531/:577/:655 and
// eviction_probe.go:129). call drives exactly one of them to its error return
// and asserts the call failed where the signature exposes that; probeFileSize
// returns only a size, so its row asserts 0 instead.
type fetchSite struct {
	name string
	call func(t *testing.T, ctx context.Context, d *SegmentDownloader)
}

var engineFetchSites = []fetchSite{
	{"fetchSegment", func(t *testing.T, ctx context.Context, d *SegmentDownloader) {
		if _, _, err := d.fetchSegment(ctx, d.buildSegmentURL(1)); err == nil {
			t.Fatal("fetchSegment returned nil error")
		}
	}},
	{"probeHeadAt", func(t *testing.T, ctx context.Context, d *SegmentDownloader) {
		if _, err := d.probeHeadAt(ctx, 999999999); err == nil {
			t.Fatal("probeHeadAt returned nil error")
		}
	}},
	{"probeFileSize", func(t *testing.T, ctx context.Context, d *SegmentDownloader) {
		if size := d.probeFileSize(ctx); size != 0 {
			t.Fatalf("probeFileSize = %d, want 0 on a request that never answered", size)
		}
	}},
	{"fetchChunk", func(t *testing.T, ctx context.Context, d *SegmentDownloader) {
		if _, _, err := d.fetchChunk(ctx, 0, 1023); err == nil {
			t.Fatal("fetchChunk returned nil error")
		}
	}},
	{"ProbeSegmentAvailable", func(t *testing.T, ctx context.Context, d *SegmentDownloader) {
		if _, _, err := d.ProbeSegmentAvailable(ctx, 1); err == nil {
			t.Fatal("ProbeSegmentAvailable returned nil error")
		}
	}},
}

// newFetchSiteDownloader builds a downloader pointed at srv with a throwaway
// output path. The query-style base URL is what buildSegmentURL turns into
// `&sq=N`, so every site below issues a real request at the test server.
func newFetchSiteDownloader(t *testing.T, srvURL string) *SegmentDownloader {
	t.Helper()
	return NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srvURL + "/v?x=1",
		OutputFile: filepath.Join(t.TempDir(), "video_stream"),
	})
}

// TestEveryFetchSiteIgnoresACallerCancel pins T4-35 across all five sites: a
// shutdown, a quality split or a superseded refresh cancels the download
// context, the in-flight request dies with it, and that is a decision Moombox
// made — not evidence about the network. Counting it drags the connectivity
// oracle toward "offline" on every clean stop.
//
// The handler blocks until the client goes away, so the ONLY way each request
// ends is the caller's cancel. probeHeadAt and probeFileSize derive a
// hardcoded 10 s timeout, well past this test, so the parent cancel is the
// only thing that can finish them — which is exactly the guard under test.
//
// Mutant this kills: reporting unconditionally at any one of the five sites
// (that row's fails becomes 1). It does NOT discriminate a guard on parent
// from a guard on the SHADOWED derived ctx — cancelling parent finishes both.
// TestFetchSegmentDerivedTimeoutIsAConnectivityFailure below is what kills
// that mutant.
//
// Do not add t.Parallel(): the reporter is a package global.
func TestEveryFetchSiteIgnoresACallerCancel(t *testing.T) {
	for _, site := range engineFetchSites {
		t.Run(site.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				<-r.Context().Done()
			}))
			t.Cleanup(srv.Close)

			rec := &countingReporter{}
			SetConnectivityReporter(rec)
			t.Cleanup(func() { SetConnectivityReporter(nil) })

			ctx, cancel := context.WithCancel(t.Context())
			joinCancel := goCancelAfter(t, 50*time.Millisecond, cancel)
			site.call(t, ctx, newFetchSiteDownloader(t, srv.URL))
			joinCancel()
			cancel()

			if got := rec.fails.Load(); got != 0 {
				t.Errorf("connectivity failures = %d, want 0 — a caller cancel was recorded as a network failure", got)
			}
		})
	}
}

// TestEveryFetchSiteReportsATransportError is the other half across all five
// sites: with a perfectly healthy caller context, a request that dies on the
// wire IS network evidence and must still be reported.
//
// The server hijacks and closes without answering, which is a transport error
// at the client and happens instantly — long before any derived deadline — so
// each site's own context is still alive when it reports.
//
// Mutant this kills: suppressing every error outright at any of the five
// sites (that row's fails drops to 0), which is what a guard written against
// the wrong condition would do.
//
// Do not add t.Parallel(): the reporter is a package global.
func TestEveryFetchSiteReportsATransportError(t *testing.T) {
	for _, site := range engineFetchSites {
		t.Run(site.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hj, ok := w.(http.Hijacker)
				if !ok {
					t.Error("test server does not support hijacking")
					return
				}
				conn, _, err := hj.Hijack()
				if err == nil {
					conn.Close() // no response at all
				}
			}))
			t.Cleanup(srv.Close)

			rec := &countingReporter{}
			SetConnectivityReporter(rec)
			t.Cleanup(func() { SetConnectivityReporter(nil) })

			site.call(t, t.Context(), newFetchSiteDownloader(t, srv.URL))

			if got := rec.fails.Load(); got != 1 {
				t.Errorf("connectivity failures = %d, want 1 — a transport error with a healthy caller IS network evidence", got)
			}
		})
	}
}

// TestFetchSegmentDerivedTimeoutIsAConnectivityFailure is the discriminator
// the two tables above cannot be: it is the only scenario where the parent
// stays alive but the DERIVED (shadowed) context is the one that finishes.
// SegmentTimeout is shrunk to a few milliseconds so the derived context times
// out against a server that never answers, while the caller's own context
// (t.Context()) never expires. A request that genuinely ran out of time with a
// healthy caller IS network evidence and must still be reported. Only
// fetchSegment and fetchChunk take their timeout from SegmentTimeout, and only
// fetchSegment needs no byte range, which is why this stays a single case
// rather than a sixth table.
//
// Mutant this kills: guarding on the SHADOWED/derived ctx instead of parent —
// fails would stay 0, exactly backwards from what a guard against
// caller-cancellation is supposed to catch.
//
// Do not add t.Parallel(): shrinks the package-global SegmentTimeout, and the
// reporter is a package global.
func TestFetchSegmentDerivedTimeoutIsAConnectivityFailure(t *testing.T) {
	orig := SegmentTimeout
	SegmentTimeout = 20 * time.Millisecond
	t.Cleanup(func() { SegmentTimeout = orig })

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	rec := &countingReporter{}
	SetConnectivityReporter(rec)
	t.Cleanup(func() { SetConnectivityReporter(nil) })

	d := newFetchSiteDownloader(t, srv.URL)
	if _, _, err := d.fetchSegment(t.Context(), d.buildSegmentURL(1)); err == nil {
		t.Fatal("fetchSegment against a server that never answers returned nil error")
	}
	if got := rec.fails.Load(); got != 1 {
		t.Errorf("connectivity failures = %d, want 1 — a derived-context timeout with a healthy caller IS network evidence", got)
	}
}
```

Tests folded in (every assertion they made is now made for all five sites, not one or two):
`TestFetchSegmentCancelIsNotAConnectivityFailure` → `TestEveryFetchSiteIgnoresACallerCancel/fetchSegment`;
`TestProbeSegmentAvailableCancelIsNotAConnectivityFailure` → `…/ProbeSegmentAvailable`;
`TestFetchSegmentTransportErrorIsAConnectivityFailure` → `TestEveryFetchSiteReportsATransportError/fetchSegment`.

- [ ] **Step 5: Run the engine tests and prove one table mutant**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestEveryFetchSite|TestFetchSegmentDerivedTimeout|TestProbeHeadSequence' -v ./internal/engine/
```
Expected: 5 + 5 sub-tests PASS, plus the three head-probe tests.

Mutant proof (do ONE): temporarily change `reportFetchFailure` (`internal/engine/downloader_fetch.go:148-153`)
to `func reportFetchFailure(parent context.Context, tag string) { reportFailure(tag) }` and re-run.
Expected: all five `TestEveryFetchSiteIgnoresACallerCancel` sub-tests FAIL with
`connectivity failures = 1, want 0`. **Revert `downloader_fetch.go` immediately.**

- [ ] **Step 6: Unify the reporter doubles**

In `internal/engine/downloader_test.go`, delete:

```go
type fakeReporter struct {
	fails     int
	successes int
}

func (f *fakeReporter) ReportFailure(string) { f.fails++ }
func (f *fakeReporter) ReportSuccess(string) { f.successes++ }
```

and in `TestConnectivityReporter_SetAndClear` replace `f := &fakeReporter{}` with
`f := &countingReporter{}` and the assertion

```go
	if f.fails != 1 || f.successes != 1 {
		t.Errorf("expected fails=1 successes=1, got fails=%d successes=%d", f.fails, f.successes)
	}
```

with

```go
	if f.fails.Load() != 1 || f.successes.Load() != 1 {
		t.Errorf("expected fails=1 successes=1, got fails=%d successes=%d", f.fails.Load(), f.successes.Load())
	}
```

`countingReporter` is declared in `downloader_fetch_cancel_test.go`, same package — no import, no move.

- [ ] **Step 7: Give the utils cancel goroutines a recover**

`internal/utils/http_test.go` has two bare `go func(){ time.Sleep(...); cancel() }()` goroutines, at
`:96-100` (`TestFetchBodyHonoursCtxCancel`) and `:209-213`
(`TestFetchWithTimeoutCallerCancelIsNotAFailure`). Add this helper to the file (place it immediately
above `TestFetchBodyHonoursCtxCancel`, which is its first user — helpers live with their first use):

```go
// goCancelAfter runs cancel after d in a goroutine carrying the project's
// inline recover (the global rule: every goroutine has one). Without it a
// panic here takes the whole test binary down with a stack naming only the
// goroutine, attributed to no test. The goroutine cannot report the panic
// itself — no t method is legal from a goroutine that may outlive the test —
// so the value goes into a buffered channel and the returned join func, which
// the test body calls while it is still running, fails the right test with it.
func goCancelAfter(t *testing.T, d time.Duration, cancel context.CancelFunc) func() {
	t.Helper()
	panicked := make(chan any, 1)
	done := make(chan struct{})
	go func() {
		defer close(done) // registered first, so it runs AFTER the recover below
		defer func() {
			if r := recover(); r != nil {
				panicked <- r
			}
		}()
		time.Sleep(d)
		cancel()
	}()
	return func() {
		t.Helper()
		<-done
		select {
		case r := <-panicked:
			t.Fatalf("the cancel goroutine panicked: %v", r)
		default:
		}
	}
}
```

Then at `:96-100` replace

```go
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := FetchBody(ctx, srv.URL, 5*time.Second, nil)
```

with

```go
	ctx, cancel := context.WithCancel(t.Context())
	joinCancel := goCancelAfter(t, 50*time.Millisecond, cancel)
	_, err := FetchBody(ctx, srv.URL, 5*time.Second, nil)
	joinCancel()
```

and at `:209-213` replace

```go
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	if _, _, err := FetchWithTimeout(ctx, srv.URL, 5*time.Second, nil); err == nil {
		t.Fatal("FetchWithTimeout on a cancelled context returned nil error")
	}
	cancel()
```

with

```go
	ctx, cancel := context.WithCancel(t.Context())
	joinCancel := goCancelAfter(t, 50*time.Millisecond, cancel)
	_, _, err := FetchWithTimeout(ctx, srv.URL, 5*time.Second, nil)
	joinCancel()
	if err == nil {
		t.Fatal("FetchWithTimeout on a cancelled context returned nil error")
	}
	cancel()
```

The `if` is split off the call because `joinCancel()` must run between the fetch and the assertion: a
`t.Fatal` on the fetch's error would otherwise end the test with the goroutine's panic still unread.

No new imports: `context`, `testing` and `time` are already imported by this file.

- [ ] **Step 8: Prove the recover actually reports**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestFetchBodyHonoursCtxCancel|TestFetchWithTimeoutCallerCancelIsNotAFailure' ./internal/utils/
```
Expected: PASS.

Named mutant: remove the `defer func(){ if r := recover(); … }()` from `goCancelAfter`. To prove the
assertion is live, temporarily insert `panic("mutant")` immediately before `time.Sleep(d)` in
`goCancelAfter` and re-run. Expected WITH the recover: `FAIL … the cancel goroutine panicked: mutant`,
naming both tests. Expected WITHOUT it: the test binary dies with `panic: mutant` and no test is
blamed. **Remove the injected panic afterwards.**

- [ ] **Step 9: Run the gates**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/engine/ ./internal/utils/
gofmt -l ./internal/engine ./internal/utils
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/engine/ ./internal/utils/
staticcheck ./internal/engine/... ./internal/utils/...
```
Expected: both packages `ok`; no gofmt output; vet and staticcheck silent. Confirm
`git status --short` shows ONLY the four files in this task's pathspec (the three temporary source
mutants must all be reverted).

- [ ] **Step 10: Commit**

```bash
git add internal/engine/downloader_fetch_cancel_test.go internal/engine/downloader_fetch_headprobe_test.go internal/engine/downloader_test.go internal/utils/http_test.go
git commit -m "test(engine,utils): table the five cancel sites, pin the unparseable head header, one reporter double

The caller-cancel guard was pinned at two of the five reportFetchFailure
sites; two tables now cover all five (cancel = not reported, transport error =
still reported), with the derived-timeout discriminator kept separate because
only fetchSegment has a shrinkable timeout. probeHeadSequence gains the
header-present-but-unparseable case Arc 2 parked. fakeReporter is gone —
countingReporter is the package's only reporter double.

Both utils cancel goroutines now carry the project's inline recover, handing
the panic back to the test body rather than killing the binary unattributed.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/engine/downloader_fetch_cancel_test.go internal/engine/downloader_fetch_headprobe_test.go internal/engine/downloader_test.go internal/utils/http_test.go
```

---

### Task 5: One JSON candidate iterator for three extractors (group A4)

`extractPlayerResponse` (`internal/youtube/watch_page.go`) iterates candidates and skips ones that fail
the brace scan or decode; its two twins — `extractYtInitialData`
(`internal/youtube/channel_membership.go`) and `chat.ExtractChatContinuation` (`internal/chat/api.go`)
— still take the FIRST match, so a forged `ytInitialData = {}` ahead of the real assignment denies the
real document. This task gives all three the same loop.

**RULING (carried from the chain controller, do not re-litigate):** the shared iterator lives in
`internal/utils`, beside `jsonwalk.go`. Verified with `go list -deps`: `internal/chat` imports
`internal/utils` and NOT `internal/youtube`; `internal/youtube` imports `internal/utils` and not
`internal/chat`; `internal/utils` imports only `internal/connectivity`, `internal/constants`,
`internal/httpx`. So utils is the only existing shared home, and `internal/chat` must not gain a
`internal/youtube` import for one helper. Cost if wrong: a scanner in utils that only three callers use.

Related fact, worth recording in the task report: `go test ./internal/chat/` needs NONE of the four
gitignored embed blobs — `internal/chat`'s full Moombox dep set is constants/httpx/connectivity/utils,
with no `internal/bgutils/embed`. `./internal/youtube/` DOES need them (it reaches `bgutils/embed`
through `bgutils/sidecar`), so a worktree missing the blobs fails that package's build, not chat's.

**Files:**
- Create: `internal/utils/jsoncandidates.go`
- Create: `internal/utils/jsoncandidates_test.go`
- Create: `internal/chat/api_continuation_test.go`
- Modify: `internal/youtube/watch_page.go:703-731` (`extractPlayerResponse`), `:863` (the ytAtN scan
  call site), `:998-1036` (`scanBalancedObject` — deleted)
- Modify: `internal/youtube/channel_membership.go:349-390` (`extractYtInitialData`)
- Modify: `internal/youtube/watch_page_test.go` (one new row in `TestExtractChatContinuationShapes`)
- Modify: `internal/chat/api.go:53` (the lazy regex), `:203` (the caller), `:280-333`
  (`ExtractChatContinuation`)
- Modify: `docs/spec/platform-services.md:197`, `:222`, `:920`

**Interfaces:**
- Consumes: `playerResponseAnchors` (`internal/youtube/watch_page.go:60-64`, unchanged — three anchors,
  order load-bearing) and `ytInitialDataStartRe` (`internal/youtube/channel_membership.go:53`).
- Produces (all exported from `internal/utils`, used by `internal/youtube` and `internal/chat`):
  - `func ScanBalancedJSONObject(s []byte) ([]byte, bool)` — the complete `{…}` literal starting at
    `s[0]`, tracking `'`, `"` and backtick string state; a sub-slice of `s`, no copy.
  - `func FindJSONObjectCandidate(page []byte, anchors []*regexp.Regexp, accept func(obj []byte) bool) ([]byte, bool)`
    — per anchor in order, per occurrence in page order: brace-scan from the `{` the match ends on,
    skip a candidate whose scan fails or whose `accept` returns false, return the first accepted
    literal (a sub-slice of `page`).
  - `func IsNonEmptyJSONObject(obj []byte) bool` — the default `accept`: valid JSON with something
    between the braces.

- [ ] **Step 1: Read all three extractors and the tests that pin them**

Read `internal/youtube/watch_page.go:44-64` (the anchor block and why the order is load-bearing),
`:682-731` (`extractPlayerResponse` and its doc — the reasoning the new helper must preserve),
`:855-870` (the ytAtN call site), `:998-1036` (`scanBalancedObject`);
`internal/youtube/channel_membership.go:22-53` (the `ytInitialDataStartRe` doc, which explains why a
BARE `ytInitialData = {` is not an anchor) and `:349-390`; `internal/chat/api.go:53`, `:195-203`,
`:280-333`. Then the pins: `internal/youtube/watch_page_test.go:446-503`
(`TestPlayerResponseExtractionMatchesTheLegacyPatterns`), `:525-558` (the benchmark and the 16-alloc
ceiling), `:560-708` (`TestExtractChatContinuationShapes`), `:710-741`
(`TestPlayerResponseSkipsAForgedCandidate` — the model for the two new tests); and
`internal/chat/api_cookie_test.go:97-103` (`watchPageHTML`, which every chat cookie test routes
through).

- [ ] **Step 2: Write the failing utils test**

Create `internal/utils/jsoncandidates_test.go`:

```go
package utils

import (
	"regexp"
	"testing"
)

// TestScanBalancedJSONObjectIgnoresBracesInsideQuotes pins the string-state
// tracking that makes this a scanner rather than a brace counter. It moved
// here from internal/youtube (where it was scanBalancedObject); the three
// quote kinds are JS string state, not JSON, because the literal is embedded
// in a <script> body and the surrounding page is JavaScript.
//
// Mutant this kills: dropping the '\'' and '`' arms of the quote switch — the
// single-quote and backtick rows then truncate at the brace inside the
// string and return the wrong literal.
func TestScanBalancedJSONObjectIgnoresBracesInsideQuotes(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"plain", `{"a":1} tail`, `{"a":1}`, true},
		{"brace inside a double-quoted string", `{"a":"}"} tail`, `{"a":"}"}`, true},
		{"escaped quote inside a string", `{"a":"\""} tail`, `{"a":"\""}`, true},
		{"brace inside a single-quoted string", `{'a':'}'} tail`, `{'a':'}'}`, true},
		{"brace inside a backtick string", "{`a`:`}`} tail", "{`a`:`}`}", true},
		{"nested", `{"a":{"b":2}} tail`, `{"a":{"b":2}}`, true},
		{"never closes", `{"a":1`, "", false},
		{"does not start on a brace", `x{"a":1}`, "", false},
		{"empty", ``, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ScanBalancedJSONObject([]byte(tc.in))
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (got %q)", ok, tc.ok, got)
			}
			if ok && string(got) != tc.want {
				t.Errorf("literal = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFindJSONObjectCandidateReturnsTheFirstAcceptedLiteral pins the search
// itself: a candidate the accept function rejects must not end it.
//
// Mutant this kills: returning on the first match (the pre-change control
// flow of extractYtInitialData and chat.ExtractChatContinuation — one
// FindIndex, one scan). It returns the forged `{}` instead of the real
// object, which is how a page-authored `var X = {}` denied the real document.
func TestFindJSONObjectCandidateReturnsTheFirstAcceptedLiteral(t *testing.T) {
	page := []byte(`<p>var X = {} </p><script>var X = {"real":true};</script>`)
	anchors := []*regexp.Regexp{regexp.MustCompile(`var X\s*=\s*\{`)}

	var seen []string
	obj, ok := FindJSONObjectCandidate(page, anchors, func(o []byte) bool {
		seen = append(seen, string(o))
		return IsNonEmptyJSONObject(o)
	})
	if !ok {
		t.Fatal("FindJSONObjectCandidate found nothing — a rejected candidate ended the search")
	}
	if string(obj) != `{"real":true}` {
		t.Errorf("literal = %q, want the real object", obj)
	}
	if len(seen) != 2 || seen[0] != `{}` || seen[1] != `{"real":true}` {
		t.Errorf("accept saw %q, want the forged {} then the real object", seen)
	}
}

// TestFindJSONObjectCandidateDoesNotRescanRejectedOffsets pins the
// rejected-offset set. The real anchor lists overlap by construction: the
// player-response list's third anchor is a BARE `ytInitialPlayerResponse\s*=\s*\{`,
// which matches INSIDE every `var ytInitialPlayerResponse = {` occurrence the
// first anchor already tried. Without the set, a page where every candidate
// fails pays for each one twice — scan and accept — on a payload that is
// megabytes on a real watch page.
//
// Mutant this kills: dropping the rejected map (or recording the match start
// instead of the brace offset, which never collides) — accept is then called
// 4 times instead of 2, twice per distinct `{`.
func TestFindJSONObjectCandidateDoesNotRescanRejectedOffsets(t *testing.T) {
	page := []byte(`<p>var X = {} and var X = {}</p>`)
	anchors := []*regexp.Regexp{
		regexp.MustCompile(`var X\s*=\s*\{`),
		regexp.MustCompile(`X\s*=\s*\{`), // the bare twin: matches inside every `var X = {`
	}

	calls := 0
	obj, ok := FindJSONObjectCandidate(page, anchors, func([]byte) bool {
		calls++
		return false
	})
	if ok || obj != nil {
		t.Fatalf("FindJSONObjectCandidate = (%q, %v), want (nil, false) — every candidate was rejected", obj, ok)
	}
	if calls != 2 {
		t.Errorf("accept was called %d times, want 2 — one per distinct `{`; a later anchor must not re-offer an offset an earlier one already rejected", calls)
	}
}

// TestIsNonEmptyJSONObjectRejectsTheTrivialShapes pins the default accept.
// `{}` scans and decodes fine, so without the emptiness half a forged empty
// object wins the search; `{ x }` balances but is not JSON, which is the
// other shape page-authored text can reach.
//
// Mutant this kills: `return json.Valid(obj)` alone — the `{}` and `{  }`
// rows then pass.
func TestIsNonEmptyJSONObjectRejectsTheTrivialShapes(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{`{"a":1}`, true},
		{`{}`, false},
		{`{  }`, false},
		{"{\n\t}", false},
		{`{ x }`, false},
		{`{"a":}`, false},
	} {
		if got := IsNonEmptyJSONObject([]byte(tc.in)); got != tc.want {
			t.Errorf("IsNonEmptyJSONObject(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
```

- [ ] **Step 3: Run it to verify it fails**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestScanBalancedJSONObject|TestFindJSONObjectCandidate|TestIsNonEmptyJSONObject' ./internal/utils/
```
Expected: FAIL to build — `undefined: ScanBalancedJSONObject`, `undefined: FindJSONObjectCandidate`,
`undefined: IsNonEmptyJSONObject`.

- [ ] **Step 4: Write internal/utils/jsoncandidates.go**

```go
package utils

import (
	"bytes"
	"encoding/json"
	"regexp"
)

// ScanBalancedJSONObject returns the complete `{...}` literal starting at
// s[0], tracking JS string state so braces inside quoted payloads never affect
// the depth count. Returns ok=false when the literal never closes, when s is
// empty, or when s does not start on a brace. The result is a sub-slice of s:
// no copy, so a caller holding it holds the page's backing array.
//
// All three quote kinds are tracked because the literal is embedded in a
// <script> body: the surrounding text is JavaScript, and a single-quoted or
// template string there can carry a brace the JSON grammar alone would not
// admit.
//
// []byte rather than string because every caller holds the raw response body:
// the watch-page extractors stopped copying the ~1-5 MB page into a string for
// the sake of the handful of extractors that only read it.
//
// Moved here from internal/youtube (where it was scanBalancedObject) at the
// 2026-09-15 chain close, so internal/chat's ytInitialData extractor can share
// it: chat must not import youtube (nor youtube chat), and both already import
// this package.
func ScanBalancedJSONObject(s []byte) ([]byte, bool) {
	if len(s) == 0 || s[0] != '{' {
		return nil, false
	}
	depth := 0
	var quote byte
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == quote:
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			quote = c
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[:i+1], true
			}
		}
	}
	return nil, false
}

// FindJSONObjectCandidate finds the JSON object literal a page assigns, given
// assignment-PREFIX anchors that each end ON the opening brace. Per anchor in
// order, and per occurrence of that anchor in page order, it brace-scans from
// that `{` and offers the literal to accept; the first accepted literal wins
// and is returned as a sub-slice of page.
//
// Every occurrence is tried, not just the first. First-occurrence was never a
// safety property: on a real watch page the `name="description"` and
// `og:description` meta tags are emitted thousands of bytes BEFORE the real
// assignment, so page-authored text genuinely does come first. What limits
// forgery is that such text cannot spell a valid non-empty JSON object — HTML
// attribute escaping turns `"` into `&quot;`, and inside a JSON string `\"`
// breaks the scan — so a forged candidate can only fail the scan or fail
// accept. Stopping at the first match turned that harmless inability into a
// DENIAL of the real document; skipping the failed candidate and searching on
// turns it back into nothing at all.
//
// Rejected `{` offsets are remembered, because real anchor lists overlap by
// construction: a bare `X\s*=\s*\{` anchor matches inside every
// `var X\s*=\s*\{` occurrence an earlier anchor already tried, so without the
// set a page where every candidate fails pays for each one twice — on a
// multi-megabyte payload. The map is allocated lazily, so the ordinary page
// (first candidate accepted) allocates nothing here.
func FindJSONObjectCandidate(page []byte, anchors []*regexp.Regexp, accept func(obj []byte) bool) ([]byte, bool) {
	var rejected map[int]struct{}
	for _, re := range anchors {
		for start := 0; start < len(page); {
			loc := re.FindIndex(page[start:])
			if loc == nil {
				break
			}
			// The match ends ON the opening brace, so the literal starts one
			// byte back from the match end.
			brace := start + loc[1] - 1
			// Resume one byte past THIS match's start, so a rejected candidate
			// cannot be re-found and the scan above is free to run off the end
			// of a forged literal.
			start += loc[0] + 1
			if _, seen := rejected[brace]; seen {
				continue
			}
			if obj, ok := ScanBalancedJSONObject(page[brace:]); ok && accept(obj) {
				return obj, true
			}
			if rejected == nil {
				rejected = make(map[int]struct{})
			}
			rejected[brace] = struct{}{}
		}
	}
	return nil, false
}

// IsNonEmptyJSONObject is the default accept for FindJSONObjectCandidate: the
// literal must be real JSON and must carry something between its braces.
//
// json.Valid is a scan, not a decode — it allocates nothing and does not build
// the map or envelope the caller is about to build anyway — so a caller that
// unmarshals afterwards pays one extra pass, not one extra decode. The
// emptiness half is load-bearing on its own: `{}` scans and decodes perfectly
// well, so without it a forged empty object would win the search exactly as a
// forged non-object cannot.
func IsNonEmptyJSONObject(obj []byte) bool {
	// obj always comes from ScanBalancedJSONObject, so it is at least `{}`.
	return len(bytes.TrimSpace(obj[1:len(obj)-1])) > 0 && json.Valid(obj)
}
```

- [ ] **Step 5: Run the utils tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestScanBalancedJSONObject|TestFindJSONObjectCandidate|TestIsNonEmptyJSONObject' -v ./internal/utils/
```
Expected: all sub-tests PASS. Then prove the rejected-offset mutant: temporarily delete the
`if _, seen := rejected[brace]; seen { continue }` lines and re-run. Expected: FAIL with
`accept was called 4 times, want 2`. **Restore them.**

- [ ] **Step 6: Write the failing youtube row and the failing chat tests**

Add one row to `TestExtractChatContinuationShapes`
(`internal/youtube/watch_page_test.go`), immediately after the existing `"forged ytInitialData in a
description loses to the real one"` row:

```go
		{
			// The twin of TestPlayerResponseSkipsAForgedCandidate for the
			// ytInitialData locator: a page-authored `var ytInitialData = {}`
			// ahead of the real assignment matches the anchor and scans
			// cleanly, but decodes to an empty object. Taking the first match
			// returned it, and the empty envelope then read as "no
			// liveChatRenderer found" — a silent denial of chat capture.
			//
			// Mutant: a first-match locator (the pre-change
			// `ytInitialDataStartRe.FindIndex(data)`) returns `{}` here and
			// this row fails with "no liveChatRenderer found".
			name:      "a forged empty ytInitialData ahead of the real one loses",
			page:      `<p>var ytInitialData = {} </p>` + head + body(`{"continuations":[{"reloadContinuationData":{"continuation":"STOK"}}]}`) + `;</script>`,
			wantToken: "STOK",
		},
```

Create `internal/chat/api_continuation_test.go`:

```go
package chat

import "testing"

// chatPage renders a watch page whose ytInitialData carries a chat
// continuation, in the given assignment form. It mirrors watchPageHTML in
// api_cookie_test.go, which every chat cookie test routes through.
func chatPage(assignment, token string) string {
	return `<html><body><script>` + assignment +
		`{"contents":{"twoColumnWatchNextResults":{"conversationBar":{"liveChatRenderer":` +
		`{"isReplay":false,"continuations":[{"reloadContinuationData":{"continuation":"` + token + `"}}]}}}}};` +
		`</script></body></html>`
}

// TestExtractChatContinuationSkipsAForgedCandidate is the chat twin of
// internal/youtube's TestPlayerResponseSkipsAForgedCandidate. A page-authored
// `var ytInitialData = {}` ahead of the real assignment matches the anchor and
// scans cleanly, but is empty — it must not end the search.
//
// Mutant this kills: returning on the first match (the pre-change
// `ytInitialDataRegex.FindStringSubmatch`, and any iterator that stops at the
// first candidate). The forged `{}` then wins and the function reports "no
// liveChatRenderer found" — chat capture silently off for that stream.
func TestExtractChatContinuationSkipsAForgedCandidate(t *testing.T) {
	page := `<p>var ytInitialData = {} </p>` + chatPage(`var ytInitialData = `, "REALTOK")

	tok, replay, err := ExtractChatContinuation([]byte(page))
	if err != nil {
		t.Fatalf("ExtractChatContinuation: %v — a forged candidate ahead of the real assignment ended the search", err)
	}
	if tok != "REALTOK" {
		t.Errorf("token = %q, want %q", tok, "REALTOK")
	}
	if replay {
		t.Errorf("isReplay = true, want false")
	}
}

// TestExtractChatContinuationReadsTheWindowAssignmentForm pins the second
// anchor. The lazy regex this replaces matched only `var ytInitialData = `
// with exactly one space and required a `;</script>` terminator, so the
// window-property form YouTube has historically served read as "ytInitialData
// not found" — the same spelling internal/youtube's locator has always
// accepted.
//
// Mutant this kills: dropping the window anchor from ytInitialDataAnchors.
func TestExtractChatContinuationReadsTheWindowAssignmentForm(t *testing.T) {
	page := chatPage(`window["ytInitialData"]   =   `, "WTOK")

	tok, _, err := ExtractChatContinuation([]byte(page))
	if err != nil {
		t.Fatalf("ExtractChatContinuation: %v", err)
	}
	if tok != "WTOK" {
		t.Errorf("token = %q, want %q", tok, "WTOK")
	}
}

// TestExtractChatContinuationReportsAMissingBlob keeps the not-found error
// text, which is the only signal a caller gets when a page carries no
// ytInitialData at all (a consent wall body, an error page).
//
// Mutant this kills: returning a nil error with an empty token when the
// locator finds nothing — orchestrator_chat.go and the downloader both treat
// an empty token as "no chat", so the reason would vanish from the log.
func TestExtractChatContinuationReportsAMissingBlob(t *testing.T) {
	if _, _, err := ExtractChatContinuation([]byte(`<html><body>nothing here</body></html>`)); err == nil ||
		err.Error() != "ytInitialData not found" {
		t.Errorf("err = %v, want %q", err, "ytInitialData not found")
	}
}
```

- [ ] **Step 7: Run both to verify they fail**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestExtractChatContinuationShapes ./internal/youtube/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestExtractChatContinuation ./internal/chat/
```
Expected: youtube FAILS on the new row with `err = no liveChatRenderer found, want ...` (the
first-match locator took the forged `{}`); chat FAILS to build with
`cannot use []byte(page) (value of type []byte) as string value`.

- [ ] **Step 8: Repoint internal/youtube at the shared helpers**

In `internal/youtube/watch_page.go`:

(a) Replace the BODY of `extractPlayerResponse` (`:703-731`). Its existing doc comment (`:682-702`)
stays exactly as it is; append this one paragraph to the end of it, then the new body:

```go
// The loop itself — every occurrence of every anchor, skipping a candidate
// whose scan or accept fails, and never re-offering a `{` an earlier anchor
// already rejected — lives in utils.FindJSONObjectCandidate. The bare third
// anchor matches inside every `var` occurrence, so that last part is what
// keeps a failing page from paying for each candidate twice.
func extractPlayerResponse(page []byte) (map[string]any, bool) {
	var pr map[string]any
	// The decode happens inside accept because it IS the acceptance test: a
	// candidate that does not decode, or decodes to an empty object, is not
	// the player response and must not end the search.
	if _, ok := utils.FindJSONObjectCandidate(page, playerResponseAnchors, func(obj []byte) bool {
		var cand map[string]any
		if json.Unmarshal(obj, &cand) != nil || len(cand) == 0 {
			return false
		}
		pr = cand
		return true
	}); !ok {
		return nil, false
	}
	return pr, true
}
```

(b) At `:863`, replace `obj, ok := scanBalancedObject(page[loc[1]-1:])` with
`obj, ok := utils.ScanBalancedJSONObject(page[loc[1]-1:])`.

(c) Delete `scanBalancedObject` and its doc comment (`:998-1036`) entirely. Confirm with
`grep -rn "scanBalancedObject" internal/` that nothing in Go source still names it.

In `internal/youtube/channel_membership.go`:

(d) Rename `ytInitialDataStartRe` to `ytInitialDataAnchors` and wrap it in a slice — the one regex
already spells both assignment forms as an alternation, which finds occurrences in PAGE order (better
than two separate anchors here), so this stays one element and its existing doc comment stays true.
Append one sentence to that comment:

```go
// Since the 2026-09-15 chain close the locator iterates candidates through
// utils.FindJSONObjectCandidate rather than taking the first match, so a
// forged assignment that scans but is empty or is not JSON no longer denies
// the real document — it is skipped.
var ytInitialDataAnchors = []*regexp.Regexp{
	regexp.MustCompile(`(?:var ytInitialData|window\["ytInitialData"\])\s*=\s*\{`),
}
```

(e) Replace `extractYtInitialData` (`:349-390`, the whole function including its own inline brace
scanner) with:

```go
// extractYtInitialData pulls the ytInitialData JSON object out of a channel
// (or watch) page: an anchored assignment prefix, then a string-aware
// brace-depth scan from the `{` the match ends on. Returns a sub-slice of the
// input (no copy) and true on success. Balancing braces rather than using a
// non-greedy regex is necessary because the channel payload is large and
// deeply nested; working on []byte avoids copying the ~1 MB page.
//
// Candidates are iterated, not first-matched (utils.FindJSONObjectCandidate).
// The acceptance test is deliberately cheap — valid, non-empty JSON, by scan
// rather than by decode — because both callers (parseMembershipTab,
// extractChatContinuation) unmarshal the literal into their own typed
// envelopes immediately afterwards, and a full map decode of a megabyte-scale
// literal purely to decide whether to accept it would cost more than the
// parse it guards.
func extractYtInitialData(data []byte) ([]byte, bool) {
	return utils.FindJSONObjectCandidate(data, ytInitialDataAnchors, utils.IsNonEmptyJSONObject)
}
```

(f) Add `"github.com/vampiricwulf/Moombox/internal/utils"` to `channel_membership.go`'s import block
(it currently imports only `constants` from the module). `watch_page.go` already imports utils.

- [ ] **Step 9: Repoint internal/chat at the shared helpers**

In `internal/chat/api.go`:

(a) Replace `var ytInitialDataRegex = regexp.MustCompile(...)` (`:53`) with:

```go
// ytInitialDataAnchors are assignment-PREFIX anchors for the ytInitialData
// blob. Each ends ON the opening brace; the object's extent comes from the
// balanced scan, never from the regex.
//
// This replaces a lazy `var ytInitialData = ({.+?});</script>`, which had
// three problems: it required exactly one space around `=`, it required a
// `;</script>` terminator the page need not supply, and the lazy body stopped
// at the first `};</script>` anywhere on the page. The two spellings here are
// the ones internal/youtube's locator has always accepted; a BARE
// `ytInitialData = {` is deliberately NOT one, because the watch page embeds
// attacker-authored video metadata and a shortDescription can spell it.
var ytInitialDataAnchors = []*regexp.Regexp{
	regexp.MustCompile(`var ytInitialData\s*=\s*\{`),
	regexp.MustCompile(`window\["ytInitialData"\]\s*=\s*\{`),
}
```

(b) At `:203`, replace `return ExtractChatContinuation(string(body))` with
`return ExtractChatContinuation(body)` and delete the now-false half of the comment above the
`io.ReadAll` that talks about the string copy, if one remains after reading it.

(c) Replace the head of `ExtractChatContinuation` (`:280-293`) with:

```go
// ExtractChatContinuation extracts a chat continuation token from watch page
// HTML.
//
// The page is []byte, not string: the caller holds the response body (capped
// at maxWatchPageBytes, 10 MB) and copying it to run a regex over it was the
// single largest allocation on this path. The blob is located by an anchored
// assignment prefix plus a balanced scan over every candidate
// (utils.FindJSONObjectCandidate), so a forged `var ytInitialData = {}` ahead
// of the real assignment is skipped instead of ending the search. Shape
// mirrors internal/youtube's extractChatContinuation, which is the same
// extraction with a typed envelope in place of this map walk.
func ExtractChatContinuation(page []byte) (string, bool, error) {
	raw, ok := utils.FindJSONObjectCandidate(page, ytInitialDataAnchors, utils.IsNonEmptyJSONObject)
	if !ok {
		return "", false, fmt.Errorf("ytInitialData not found")
	}

	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return "", false, fmt.Errorf("parse ytInitialData: %w", err)
	}
```

The rest of the function (the `contents` → `liveChatRenderer` walk and the four continuation keys) is
unchanged.

(d) Add `"github.com/vampiricwulf/Moombox/internal/utils"` to `api.go`'s import block.

- [ ] **Step 10: Run the three packages**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/utils/ ./internal/youtube/ ./internal/chat/
```
Expected: all three `ok`, with the new youtube row and the three new chat tests passing and every
pre-existing pin still green — in particular `TestPlayerResponseSkipsAForgedCandidate`,
`TestPlayerResponseExtractionMatchesTheLegacyPatterns` (all 6 rows),
`TestExtractChatContinuationShapes` (all 14 rows, including "renderer absent and the envelope decode
errored", whose `{"contents":5}` is valid non-empty JSON and therefore still accepted), the
`channel_membership` membership-tab tests, and `internal/chat`'s cookie tests, which route through
`watchPageHTML`.

Also run and RECORD the allocation ceiling — `IsNonEmptyJSONObject` adds a `json.Valid` pass to
`extractChatContinuation`:

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestExtractChatContinuationAllocationCeiling -v ./internal/youtube/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -run '^$' -bench BenchmarkExtractChatContinuation -benchmem ./internal/youtube/
```
Expected: the ceiling test PASSES (it was 6 allocs/op against a ceiling of 16; `json.Valid` uses a
pooled scanner and should add none after `AllocsPerRun`'s warm-up). Report the benchmark's before/after
`ns/op` in the task report. **If the ceiling test fails, STOP and report** — do not raise
`chatContinuationAllocCeiling`; the ceiling is the pin.

- [ ] **Step 11: Follow the scanner in the spec doc**

`docs/spec/platform-services.md`, three edits:

(a) `:197`, append to the bullet:

> Candidates are iterated rather than first-matched (`FindJSONObjectCandidate`, `internal/utils/jsoncandidates.go`), so a forged assignment that scans but is empty or is not JSON is skipped instead of denying the real document.

(b) `:222`, replace `scanBalancedObject` with the moved name and its new home:

> Each match ends on the opening brace; `ScanBalancedJSONObject` (`internal/utils/jsoncandidates.go`) then walks the literal tracking JS string

(c) `:920`, replace `(`scanBalancedObject`)` with
``(`ScanBalancedJSONObject`, `internal/utils/jsoncandidates.go`)``.

Both (b) and (c) form symbol+path citation pairs the citation test resolves against the DECLARING
file, which is why the path must be the new one.

- [ ] **Step 12: Run the full gate set for this task**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/utils/ ./internal/youtube/ ./internal/chat/ ./internal/docs/
gofmt -l ./internal/utils ./internal/youtube ./internal/chat
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/utils/ ./internal/youtube/ ./internal/chat/
staticcheck ./internal/utils/... ./internal/youtube/... ./internal/chat/...
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./...
MOOMBOX_LIVE_YT_TEST=1 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 \
  -run 'TestLivePublicExtraction|TestLiveLoginMarkersPresent' -v ./internal/youtube/
```
Expected: four packages `ok`; no gofmt output; vet and staticcheck silent (staticcheck U1000 is a hard
gate — every new exported helper has a caller, and the deleted `scanBalancedObject`/`fakeReporter`
leave no unused residue); `go build ./...` succeeds; the two live tests PASS. The live gate is an
internet test and asserts CAPABILITIES, not mechanisms — if it fails, confirm the capability is
genuinely lost before calling it a regression, and report rather than "fixing" the extractor.

- [ ] **Step 13: Commit**

```bash
git add internal/utils/jsoncandidates.go internal/utils/jsoncandidates_test.go internal/youtube/watch_page.go internal/youtube/channel_membership.go internal/youtube/watch_page_test.go internal/chat/api.go internal/chat/api_continuation_test.go docs/spec/platform-services.md
git commit -m "refactor(utils,youtube,chat): one JSON candidate iterator for the three page extractors

extractPlayerResponse already skipped a candidate whose scan or decode
failed; its two twins — extractYtInitialData and chat.ExtractChatContinuation
— still took the first match, so a page-authored 'var ytInitialData = {}'
ahead of the real assignment denied the real document (chat silently off).

scanBalancedObject moves to internal/utils as ScanBalancedJSONObject, joined
by FindJSONObjectCandidate (every occurrence of every anchor, skip what the
scan or accept rejects, never re-offer a brace an earlier anchor rejected —
the bare third player-response anchor matches inside every 'var' one) and
IsNonEmptyJSONObject. utils is the only shared home: chat must not import
youtube, nor youtube chat, and both already import utils.

chat.ExtractChatContinuation now takes []byte, so the caller stops copying a
page of up to 10 MB, and its lazy ';</script>'-terminated regex becomes the
same two anchored assignment forms youtube accepts.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/utils/jsoncandidates.go internal/utils/jsoncandidates_test.go internal/youtube/watch_page.go internal/youtube/channel_membership.go internal/youtube/watch_page_test.go internal/chat/api.go internal/chat/api_continuation_test.go docs/spec/platform-services.md
```

---

### Task 6: Delete this plan

Project rule: once a plan is implemented and verified, the plan doc is deleted — git history is the
archive, and living docs (`SPEC.md`, `docs/spec/*.md`, `CLAUDE.md`, `.claude/skills/*`) are updated
instead. Spec §2 states it for this chain: "Plans are deleted in the arc's last commit once implemented."

**Files:**
- Delete: `docs/superpowers/plans/2026-09-15-sweep-close.md`

**Interfaces:**
- Consumes: Tasks 1-5 are all committed and their gates green.
- Produces: nothing.

- [ ] **Step 1: Confirm every task is committed**

```bash
git log --oneline main..HEAD
git status --short
```
Expected: five commits (Tasks 1-5, in that order) and a clean working tree. If anything is
uncommitted, STOP — the plan is not implemented yet.

- [ ] **Step 2: Delete the plan**

```bash
git rm docs/superpowers/plans/2026-09-15-sweep-close.md
```

- [ ] **Step 3: Commit**

```bash
git commit -m "chore: remove implemented sweep-close plan

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- docs/superpowers/plans/2026-09-15-sweep-close.md
```

- [ ] **Step 4: Run the branch's full arc gate list**

Run every command in the **Arc gate list** at the top of this plan, in order, and record the output.
The single `go test -count=1 ./...` is controller-run (one at a time across all worktrees), and the
YouTube live gate is the last line.

---

## Self-review

**Brief coverage.** A1.1 → Task 1 Steps 2-3; A1.2 → Task 1 Step 4; A1.3 → Task 1 Step 5. A2 item 8
(table over five sites + the unparseable-header case) → Task 4 Steps 2-5; item 9 (utils goroutine
recover) → Task 4 Steps 7-8; item 10 (double unification) → Task 4 Step 6. A3.5 → Task 2 Step 2; A3.6 →
Task 2 Step 3; A3.7 → Task 2 Step 5 (verified present, deliberately not re-edited). A4 → Task 5 in
full, all three consumers plus the anchor-overlap skip. C → Task 3, all seven errata. Plan deletion →
Task 6. Groups B and D go to the owner report, not to this branch, per the brief's last line.

**Type consistency.** `ScanBalancedJSONObject`, `FindJSONObjectCandidate`, `IsNonEmptyJSONObject`,
`ytInitialDataAnchors` (two different variables, one per package — `internal/youtube`'s is a
one-element alternation, `internal/chat`'s is two anchors), `fetchSite`, `engineFetchSites`,
`newFetchSiteDownloader`, `goCancelAfter` (declared once in `internal/engine` and once in
`internal/utils`; separate packages, identical shape), `countingReporter` — every name used in a later
step is defined in an earlier one, with the same signature.

**Deliberate deletions.** Task 4 removes three engine tests whose every assertion is re-made by the
tables for all five sites (mapping listed in its Step 4), and `fakeReporter`. Task 5 removes
`scanBalancedObject` (moved, not dropped) and `ytInitialDataRegex` (replaced by anchors). Nothing else
is deleted.

**Source mutants injected during the plan** (Task 1 Step 3, Task 4 Steps 3/5/8, Task 5 Step 5) are all
temporary and must be reverted before the step's commit; each step says so, and each commit uses a
pathspec that excludes the mutated file where it is not part of the change.
