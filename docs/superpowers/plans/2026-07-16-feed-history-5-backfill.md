# Feed History 5/5 — Backfill & Integration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The window-depth backfill (InnerTube `/browse` continuation paging over `/videos` + `/streams` + `/membership`), the every-cycle sweep with all four arms, the in-flight set with mid-scan widen detection, cancel-before-prune channel removal, progress in both UIs, the manual re-run, and the docs/cleanup tail.

**Architecture:** A `BackfillWorker` (own goroutine, strictly serial across channels, 1 page/sec) owned by the host (`cmd/moombox`), fed by a sweep evaluated in the monitor cycle. The `/browse` client lives in `internal/youtube`, modeled on `internal/chat`'s continuation paging — NOT ported from yt-dlp, which is only the reference for the response shape.

**Depends on:** Plans 1–4.

**Spec:** `docs/superpowers/specs/2026-07-15-feed-history.md` §11 (all), §7 (store rules the scan reuses). Global Constraints from Plan 1 apply.

---

### Task 1: InnerTube `/browse` continuation client

**Files:**
- Create: `internal/youtube/browse.go`
- Test: `internal/youtube/browse_test.go` (inline JSON fixtures)

**Interfaces:**
- Produces:

```go
type TabItem struct { VideoID, Title string; Age time.Duration } // Age via itemAge — badge short-circuit included
type TabPage struct { Items []TabItem; Continuation string }     // Continuation "" ⇒ tab exhausted
func (c *Client) FetchChannelTabPage(ctx context.Context, channelID, tab, continuation string) (*TabPage, error)
// tab ∈ {"videos","streams","membership"}. membership uses the authed fetch
// pattern from channel_membership.go (:65 HasAuthCookies early return stays the
// caller's job — the scanner gates eligibility, §11).
```

- Reuse as-is: `extractYtInitialData` (`channel_membership.go:295`), `ytInitialTabs` (`:114`), `walkVideoRenderers` (`:176`), `lockupTitle` (`:261`), `rendererTitle` (`:267`), **`itemAge`** (`:220-257`, never re-implement — the live-badge short-circuit is load-bearing, §11).
- Model the transport/loop on `internal/chat`: `api.go:172-178` (continuation request), `downloader.go:412-423` (paging loop), `:558-583` (stale-token recovery). yt-dlp `_tab.py` is the reference for shape only: token at `continuationItemRenderer.continuationEndpoint.continuationCommand.token`, page-2+ items under `onResponseReceivedActions[].appendContinuationItemsAction.continuationItems[]`, loop detection via a `seenContinuations` set, `visitorData` re-extracted per page.

- [ ] **Step 1: Failing tests** — three, with inline fixtures: (a) page 1 (ytInitialData shape with two video renderers + a `continuationItemRenderer` carrying token "TOK2") parses both items and the token; (b) page 2 (`appendContinuationItemsAction` shape, no further token) parses and terminates; (c) a page returning an already-seen token errors with a loop-detected sentinel. Fixture JSON: build the renderer maps exactly as `channel_membership_test.go:27`'s inline HTML/JSON does — copy its item shape, wrap in the two envelope shapes above.
- [ ] **Step 2:** Verify failure → implement (`POST` to the InnerTube `/browse` endpoint via the existing transport — `player_api_strategy.go:346`, headers via `auth.go:53 GenerateAPIHeaders`, base URL from `constants.YouTubeURLs.API`; browseId = channelID + `params` per tab, with the `UC`→`UU` uploads-playlist fallback for channels lacking tabs).
- [ ] **Step 3:** Verify pass; commit `feat(youtube): /browse continuation client`.

---

### Task 2: The scanner — window depth, two stop arms

**Files:**
- Create: `internal/monitor/backfill.go`
- Test: `internal/monitor/backfill_test.go`

**Interfaces:**
- Produces: `type BackfillWorker struct{...}` with `func (bw *BackfillWorker) scanChannel(ctx context.Context, ch *config.ChannelConfig, chID string, windowDays int, withMembership bool) error`.
- Consumes: Task 1's client, `db.UpsertFeedItem`, `db.SaveBackfillCursor(chID, stateJSON string) error` + `db.LoadBackfillCursor(chID) (string, error)` (add to Plan 1's file — two trivial upsert/select methods on `channel_state.backfill_state`).
- Rules (spec §11): per tab — page at 1 page/sec (global ticker), write each page's rows immediately (`published = now - Age` coarse / `= now` assumed; **status always `'unknown'`**; `catalog_pos` = provisional per-tab index), save the cursor after each page, stop when **(a)** an entire page's items are ALL older than the window (page-granular — the margin), or **(b)** a **non-empty** page has no datable item (parser-failure arm: log loudly, stop the tab, `backfilled_at` stays NULL). An EMPTY page with no continuation is neither arm — natural exhaustion, the tab completes CLEANLY (spec §11: an empty channel must finish its backfill, set `backfilled_at`, and establish; misreading the parser arm as vacuously true rescans empty channels every cycle forever). Log any item dated newer than an earlier item in the same tab (ordering evidence). Membership tab only when `withMembership`.

- [ ] **Step 1: Failing tests** — stub client returning scripted pages:
  - (a) page-granular stop: page 1 mixed in/out-of-window ⇒ keeps paging; page 2 fully out ⇒ tab stops; rows from BOTH pages persisted.
  - (b) parser-failure arm: pages of `Age==0` items ⇒ tab stops after one full undatable page, error recorded, completion NOT reported.
  - (c) resume: cursor saved after page 1; a new scanner with the same DB resumes at page 2 (stub asserts the continuation it was called with).
  - (d) live item (`Age==0` from the badge) among datable items ⇒ stored `assumed`/`unknown`, does not trigger arm (b) (the arm needs a whole NON-EMPTY page undatable).
  - (e) **empty channel:** every tab returns zero items and no continuation ⇒ all tabs complete CLEANLY, the completion path runs (Task 3: `backfilled_at` set over zero rows), no parser-failure recorded, and a second sweep does NOT rescan it.
- [ ] **Step 2:** Verify failure → implement. **Step 3:** Verify pass; commit `feat(monitor): backfill scanner — page-granular window stop + parser-failure arm`.

---

### Task 3: The ordering pass + completion

**Files:**
- Modify: `internal/monitor/backfill.go`
- Modify: `internal/database/database_feed_items.go` (batch update + completion writer)
- Test: `internal/monitor/backfill_test.go`

**Interfaces:**
- Produces: `db.RenumberCatalog(chID string, orderedVideoIDs []string) error` (collect-then-update: the CALLER selects rows into a slice, closes the cursor, sorts, then this method writes `catalog_pos = 0..n-1` — `SetMaxOpenConns(1)`, never update under an open cursor); `db.SetChannelBackfilled(chID string, windowDays int, withMembership bool, ts string) error` (upserts `backfilled_at`, `backfilled_window_days`, `backfilled_with_membership`, and **clears `backfill_state`** — cursor lifecycle, §11).
- Rules: after all eligible tabs stop cleanly (arm (a), not arm (b)): read the channel's rows, sort by `(published DESC, provisional pos ASC, video_id ASC)`, renumber channel-global, then `SetChannelBackfilled`. If ANY tab ended on arm (b) or errored: save cursors, do NOT renumber, do NOT set `backfilled_at` — the sweep retries next cycle.

- [ ] **Step 1: Failing tests** — (a) two tabs with overlapping items ⇒ after completion `catalog_pos` is channel-global by the sort key and `backfilled_at`/`backfilled_window_days`/`backfilled_with_membership` are set and `backfill_state` is NULL; (b) arm-(b) run ⇒ nothing of the above.
- [ ] **Step 2:** Implement → verify → commit `feat(monitor): backfill ordering pass + completion record`.

---

### Task 4: Sweep, in-flight set, widen/eligibility detection, prune

**Files:**
- Modify: `internal/monitor/backfill.go` (the worker loop + in-flight set), `cmd/moombox/services.go:568-577` (`kickMonitors` wiring), the monitor cycle (one call per cycle)
- Test: `internal/monitor/backfill_test.go`

**Interfaces:**

```go
type inFlight struct{ cancel context.CancelFunc; windowDays int; withMembership bool }
func (bw *BackfillWorker) Sweep(channels []ChannelRef) // ChannelRef{Ch *config.ChannelConfig, ChID string, WindowDays int, WithMembership bool}
func (bw *BackfillWorker) CancelAndPrune(chID string)  // cancel → wait observed → DeleteChannelFeedData + DeleteJobsAndHistoryForChannel(chID, {Queued, Upcoming, COOKIES?})
```

- Sweep condition (spec §11, all four arms — iterate the CONFIG's channel list, LEFT-JOIN semantics: a missing `channel_state` row reads as never-backfilled):

```
backfilled_at IS NULL
OR backfilled_window_days IS NULL              -- NULL < x is NULL, not true
OR backfilled_window_days < resolvedWindow
OR (backfilled_with_membership = 0 AND membershipEligibleNow)
   -- membershipEligibleNow = MembershipDiscoveryEnabled() && HasAuthCookies()
```

- In-flight handling: a channel already scanning is skipped UNLESS its recorded `windowDays < resolvedWindow` (or membership newly eligible) ⇒ **cancel, reset cursor, restart deeper** (mid-scan `backfilled_at IS NULL` makes the DB condition trivially true — the in-flight entry's recorded values are the ONLY way to detect a stale running scan, §11). Every scan removes itself from the set on success, failure, AND cancellation — a leaked entry is a permanent silent stall.
- Serial execution: the sweep pushes eligible channels onto a queue consumed by ONE goroutine (inline `defer recover()`); the scanner re-checks the active channel set before each page write (a cancellation landing between check and write costs one stale page, which the prune removes because the prune runs last).
- Triggers: every monitor cycle (one comparison per channel), startup, `kickMonitors` (note: `kickMonitors` fires on add/remove/reorder/bulk-PUT/TUI-save with no discrimination — the sweep MUST be idempotent), and the Task 6 manual re-run.
- Prune (spec §11): channels leaving the active set ⇒ `CancelAndPrune`: cancel any in-flight scan, wait for it to observe, then `DeleteChannelFeedData` + `DeleteJobsAndHistoryForChannel(chID, {Queued, Upcoming, COOKIES?})`. `Live`/`Downloading`/`Muxing` jobs keep running; terminal rows stay.

- [ ] **Step 1: Failing tests** — (a) fresh channel with no `channel_state` row IS swept; (b) widen 3→30 mid-scan cancels and restarts (stub scanner records its ctx cancellation + the new depth), narrow does not; (c) membership toggle-on after completion triggers a rescan; toggle-off does not; (d) removal mid-scan: no resurrected rows, Queued+Upcoming+COOKIES? jobs and their history gone, Downloading job untouched; (e) a scan that fails removes its in-flight entry (next sweep retries).
- [ ] **Step 2:** Implement → verify → commit `feat(monitor): backfill sweep — four arms, in-flight widen detection, cancel-before-prune`.

---

### Task 5: Progress surfacing

**Files:**
- Modify: `cmd/moombox/main.go` / `services.go` (broadcast plumbing, modeled EXACTLY on `disk_status`: generic `hub.Broadcast` at `websocket.go:441`, sender `main.go:676-681`), `cmd/moombox/ws_wiring.go:87-111` (`InitialState` — ADD the backfill snapshot; a long scan makes mid-flight connect the common case, §11), `internal/tui/app.go:60-64`-style msg type + channel + `tui_wiring.go:404`-style wiring + `app_update.go:277-279`-style handler (the trailing `return a, a.listenForUpdates()` re-arm is MANDATORY), seed the TUI like `tui_wiring.go:409-415`
- Test: host-level compile + one WS payload-shape unit test if the repo has precedent (grep `disk_status` tests; mirror or skip with a manual-verification note in the PR)

Payload: `{"channel": chID, "tab": "videos", "pages": 3, "state": "scanning|done|error|idle"}` broadcast on page completion and state change.

- [ ] Implement → `go build ./...` → commit `feat(ui): backfill progress in web + TUI, seeded in InitialState`.

---

### Task 6: Manual re-run — `R B` chord + debounced route

**Files:**
- Modify: `internal/tui/app.go` (callback field near `:411`), `internal/tui/app_actions.go` (menu registration gated on non-nil, dispatch — the chord system lives HERE, not app.go; CLAUDE.md is wrong about that and Task 7 fixes it), `cmd/moombox/tui_wiring.go` (host wiring near `:225-229`), `internal/web/routes/` (new `POST /api/backfill/rescan`)
- Test: route-level test modeled on the check-now debounce test if one exists (grep `monitors_test`)

Model on `R M` "Check Monitors Now" exactly: callback field, registration gated on non-nil (`app_actions.go:489-491`), dispatch (`:185-189`), async via the `R V` shape (`:173-184` — capture fn, return `safeCmd(...)`, never block the update loop). The API route copies `POST /api/monitors/check-now` (`monitors.go:34`) including its **30s `atomic.Int64` debounce** (`monitors.go:22,:39-51`) returning `{"success":false,"debounced":true,"retryAfterMs":N}`. Both front doors call one service func: clear `backfilled_at` for all channels? NO — the manual re-run just calls `bw.Sweep` with a `force` flag that treats every channel as `backfilled_at IS NULL`. Add `Sweep(channels, force bool)`.

- [ ] Implement → verify chord appears in the action menu + help (single source of truth `buildMenuItems()`) → commit `feat(ui): manual backfill re-run — R B chord + debounced API route`.

---

### Task 7: Docs & cleanup tail

**Files:** run each grep; fix every hit.

- [ ] `grep -rn "max_feed_items\|MaxFeedItems" docs/ SPEC.md CLAUDE.md` → rewrite each site for the two new settings (window archives N days back, upcoming/live always covered; slots pace backlog only).
- [ ] `grep -rn "last_videos" docs/ SPEC.md` → remove/replace (NOT `LastVideoSeq`).
- [ ] `CLAUDE.md`: job status lifecycle gains `Queued → Upcoming → ...`; fix the chord-system location (`buildMenuItems()`/`dispatchAction()` live in `internal/tui/app_actions.go`, not `app.go`).
- [ ] `docs/spec/data-and-storage.md`: add the v16 row to the migration table; document `feed_items`/`channel_state`.
- [ ] `.claude/skills/moombox-database-migrations/SKILL.md`: refresh to v16 reality (`PRAGMA user_version` via `writeUserVersion`, `db.db.ExecContext`, no transaction, `SetMaxOpenConns(1)` collect-then-update constraint); scope its "Update Field Maps" step to the `jobs` table (`fieldToColumn` is jobs-only, enforced by `TestFieldToColumnCoverage`).
- [ ] `config.example.toml`: final read-through of the two new settings' comments + `num_parallel_downloads` peak note.
- [ ] Full suite: `go build ./... && go vet ./... && go test ./...` → all green.
- [ ] Commit `docs: v16 documentation sweep` — and update `RELEASE_NOTES.md` per the release process ONLY when the owner says to release (memory: owner controls release timing).

## Self-check before handoff

- Spec §11 coverage walk: depth ✓ (Task 2), stop arms ✓ (2), ordering pass ✓ (3), sweep arms ✓ (4), in-flight/widen ✓ (4), serial+throttle ✓ (2/4), prune ✓ (4), established gate second key ✓ (3 writes `backfilled_at`), progress ✓ (5), re-run ✓ (6), YouTube-only filter ✓ (4's `ChannelRef` construction must apply `ch.GetPlatform() == "youtube"` — allow-list, NOT the `!= "twitch"` deny-list precedent, §11).
- Every goroutine added: `grep -n "go func" internal/monitor/backfill.go` — each has `defer recover()`.
