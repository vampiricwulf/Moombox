# Dated Per-Channel Feed History

**Status:** Design approved, awaiting implementation plan
**Date:** 2026-07-14
**Schema:** v15 → v16

## Problem

On 2026-07-14 at 19:57, Moombox downloaded `gr-ZTohjwnQ` — a members-only VOD from
June 24, three weeks old — from the channel `Jerry` (`UCkb-r702uhx4-6Lrmetp-Ow`),
despite `max_feed_items = 3`.

The cause is that `max_feed_items` does not mean "the 3 newest videos on the
channel". As implemented in `internal/monitor/feed.go:425-479`, it means *"the 3
newest of whatever candidates we successfully collected this cycle"*.
`checkChannel` treats an RSS fetch failure as non-fatal — it records the error for
channel-health accounting, then merges and caps whatever it has:

```go
data, rssErr := fm.fetchFeed(ctx, ch)
var candidates []discoveredVideo
if rssErr == nil { ... candidates = append(candidates, parsed...) }   // skipped on 404
if fm.membershipActive() { candidates = append(candidates, membershipCandidates(...)...) }
candidates = mergeCandidates(candidates, fm.maxFeedItems(ch))         // caps the survivors
```

When Jerry's RSS returned 404, the ~15 recent public items vanished from the pool.
Only 2 membership items remained — under the cap of 3 — so nothing was truncated
and the old VOD, normally ranked far down the list, sailed through to a probe.
With `include_non_live_content = true`, an ended stream not in history becomes a
download.

### Evidence

Every Jerry discovery cycle was reconstructed from `moombox.log` and
`moombox.log.1`. The separation is total:

| Jerry's RSS that cycle | Cycles | `gr-ZTohjwnQ` surfaced |
|---|---|---|
| Succeeded | 419 | **0** |
| 404'd | 31 | **23** |

The 19:57 download cycle was a 404. RSS failure is not rare: across 480 cycles,
Jerry failed 62 times (58×404, 4×500), Shachi 62, ShachiToo 70 — roughly **13% of
cycles**, on every YouTube channel. The cap is therefore unprotected about one
cycle in eight.

`itemAge` is **not** at fault: `gr-ZTohjwnQ` receives a correct ~3-week age, which
is exactly why it sinks below the RSS items and vanishes whenever RSS succeeds.
Neither the cap nor the age parser malfunctions. The flaw is that a *partial
discovery failure silently changes what the cap means*.

### Why cancelling didn't stop it

The same video was discovered and cancelled on 7/13 at 23:14:45. `AddToHistory`
runs at job creation (`cmd/moombox/monitor_callbacks.go:260`), so it *was* in
history — but at 7/14 01:45:23 the log records `removed orphaned history entries
count=80`, deleting its row. That made the video look never-seen, so the next 404
cycle re-downloaded it. The DB confirms: its history row is dated
`2026-07-15T02:57:23Z`, the *second* download, not the first.

This is a second, independent lesson: `history` was the **only** thing holding the
video back. A date-based cap defends independently, so a history purge can no
longer re-arm out-of-scope content.

## Goals

1. An RSS (or any single-source) failure must not change discovery scope.
2. `max_feed_items` becomes a stable **archival-depth** boundary: content below
   rank N is permanently out of scope, not "out of scope depending on what we
   fetched this cycle".
3. **Upcoming and live content is never missed and never consumes a cap slot.**
4. Nothing is ever ranked on a guessed date.
5. Steady-state cost must not exceed today's.

## Non-Goals

- Replacing RSS as the steady-state discovery source (see Decisions).
- Unifying the `history` table into the new store (see Decisions).
- Changing the probe-failure/cooldown machinery.
- Any Twitch-side change. The Twitch monitor is live-only and has no cap.
- Any DECAPI-side change. DECAPI returns only the channel's single latest video
  (`internal/monitor/decapi.go:523-534`), so it cannot reach back to old content.

## Key Insights

These shaped the design and are recorded because they are non-obvious.

**Discovery order is not recency order.** A store that prepends newly-seen unique
IDs would *preserve this exact bug*: `gr-ZTohjwnQ` was newly **discovered** on 7/14
despite being three weeks old. The store must be keyed on the item's actual
publish date, frozen at first insert.

**The cap only needs the newest N items to be correct.** With a dated store, a
3-week-old VOD inserts at its true date, lands at rank ~17, and never enters the
top 3 — even if the store is only partially populated. The full-catalog backfill is
therefore *not* required for correctness. It matters when `N` exceeds what RSS
returns (RSS is hard-capped at 15 items; `max_feed_items` allows up to 1000).

**Coarse dates lump, and lumps break naive ranking.** If twenty items all resolve
to "3 weeks ago" they share one timestamp. A naive `COUNT(*) WHERE published > ?`
gives them all the same rank, so a boundary landing inside a lump admits the
*entire lump* — this bug, amplified. A total order is mandatory.

**Tab order recovers lost precision.** YouTube renders tabs newest-first, so an
item's position within a listing is true recency ordering *within* a coarse lump.
Storing `catalog_pos` recovers what the relative text threw away.

**The probe is how we learn rank, not just status.** Anything undatable from a
listing gets a direct probe and returns an authoritative date. This is what
guarantees goal 4.

**The dated store makes probes cheaper, not rarer.** Today the capped N items are
probed *every cycle forever*, and most results are discarded by `nonLiveSkipReason`
immediately after. With recorded status, an item needs probing once. Steady state
falls from 3-per-channel-per-cycle to ~0–2 per cycle. This is why the pre-probe
rank filter could be removed rather than mitigated.

## Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Scope | One spec, phased implementation | Part 1 ships alone and fixes the bug; Part 2 lands on top |
| Store structure | SQLite table + covering index | Workload is one indexed top-N query per channel per cycle (~3 per 5 min); in-memory/materialised-rank optimise microseconds while adding cache-skew and renumber races |
| Cap gate | Coarse pre-filter **removed**; authoritative post-probe gate | Pre-filter was the sole cause of the miss risk, and guarded a budget that no longer exists |
| Cap meaning | Archival depth, not probe budget | Matches goal 2; probe volume is naturally bounded by sources |
| `history` table | Left alone, separate | Answers a different question ("acted on" vs "exists and when"); unifying would touch Twitch, DECAPI, orphan API, Web UI, TUI |
| Catalog scan role | Backfill only | Part 1 makes RSS 404s harmless; 3 MB/channel/cycle forever buys no correctness |
| Backfill trigger | Auto on startup for un-backfilled channels, auto on channel add, plus manual re-run | No action needed on upgrade; new channels start complete |
| `last_videos` | Removed | Dead code; `feed_items` supersedes it (per-channel **and** dated) |

## Part 1 — Dated Store and Cap-by-Date

### Schema (v16)

```sql
CREATE TABLE IF NOT EXISTS feed_items (
    channel_id   TEXT NOT NULL,
    video_id     TEXT NOT NULL,
    title        TEXT NOT NULL DEFAULT '',
    published    TEXT NOT NULL,           -- RFC3339 UTC, best-known
    date_precision TEXT NOT NULL,         -- 'exact' | 'coarse' | 'assumed'
    catalog_pos  INTEGER NOT NULL DEFAULT 0,
    source       TEXT NOT NULL,           -- rss|membership|videos|streams|decapi
    status       TEXT NOT NULL,           -- unknown|upcoming|live|vod|not_a_stream
    first_seen   TEXT NOT NULL,
    PRIMARY KEY (channel_id, video_id)
);
CREATE INDEX IF NOT EXISTS idx_feed_items_rank
    ON feed_items(channel_id, published DESC, catalog_pos ASC, video_id ASC);

CREATE TABLE IF NOT EXISTS channel_state (
    channel_id     TEXT PRIMARY KEY,
    backfilled_at  TEXT,   -- NULL until a full 3-tab scan completes
    backfill_state TEXT,   -- JSON: per-tab continuation cursor
    last_rss_ok_at TEXT    -- the "established" gate for archival
);
```

`published` is frozen at first insert and only ever *upgraded* by a
higher-precision source (`assumed` → `coarse` → `exact`). It is never recomputed
from `now`, which is what makes ranking stable across cycles.

`title` is stored (the archival pass and job creation both need it) and refreshed
when a probe returns a better one, mirroring today's rule at
`internal/monitor/utils.go:341-344` that an `"Unknown Title"` placeholder must not
overwrite a real feed title.

### Descriptions and term matching

Descriptions are deliberately **not** stored. The description used for term
matching is not a property of the item — `parseFeedCandidates` derives it from feed
context, stripping boilerplate lines that also appear in the `num_desc_lookbehind`
neighbouring entries (`internal/monitor/feed.go:615-620`). Freezing that in a row
would preserve a value computed against a feed state that no longer exists, and the
backfill cannot produce it at all (tab listings carry no descriptions).

So descriptions stay exactly where they are today: computed per cycle, in memory,
from the RSS response. The archival pass uses them when this cycle's RSS carried
the item — which is always the case for a newly-seen item, since `unknown` items
are probed and archived in the same cycle they first appear.

The one degradation: an item re-evaluated from the store *without* being in this
cycle's RSS response (for example after `max_feed_items` is raised) matches on
**title only**. This is not a new behavior — membership and DECAPI candidates are
already title-only (`internal/monitor/feed.go:697`: *"matching is title-only, like
DECAPI"*) — but it must be documented, since an RSS item whose *description* alone
matched the channel's terms would not be picked up by a later scope widening.

### The in-scope query

Total order is `(published DESC, catalog_pos ASC, video_id ASC)`. The archival
scope for a channel is simply the top N of the store:

```sql
SELECT video_id, title, published, status FROM feed_items
 WHERE channel_id = ?
 ORDER BY published DESC, catalog_pos ASC, video_id ASC
 LIMIT ?
```

The index covers this exactly — one seek, N rows, no scan.

This is expressed as a **top-N query rather than a per-candidate rank check** for
two reasons:

1. **Raising `max_feed_items` must widen scope for existing content.** Because
   `vod` items are terminal and never re-probed, a per-candidate rank check
   evaluated only at probe time would leave an item skipped at `N=3` skipped
   forever, even after the operator sets `N=20`. The top-N query re-evaluates
   scope every cycle from the store, so a config change takes effect immediately —
   and needs no probe, since the date and status are already recorded.
2. It is cheaper: one indexed query returning N rows, instead of a counting query
   per candidate against a store that holds thousands of backfilled rows.

The total order is what makes this safe against coarse-date lumps: `LIMIT N` over a
strict total order returns exactly N items, whereas a rank-by-count formulation
would tie every member of a lump at the same rank and admit all of them.

### Probe rules

```
unknown  → probe once, rank ignored          (nothing can be missed)
upcoming → probe every cycle, rank ignored   (goal 3)
live     → probe every cycle, rank ignored
vod      → never probe again
not_a_stream → never probe again
```

Probe volume is bounded naturally: RSS returns ≤15 and the membership tab ~30, so
a cycle cannot exceed ~45 unknowns, and only once. The first cycle after upgrade
pays a one-time cost (~17 probes for a 3-channel install).

### Steady-state flow (per channel, per cycle)

```
1. fetch RSS ─┐ (independent; either may fail)
   fetch /membership ─┘
2. INSERT OR IGNORE every item seen → feed_items
      RSS  → published exact
      memb → coarse from Age; undatable → 'assumed' (provisional)
3. PROBE PASS
      probe list = feed_items[channel] WHERE status IN (unknown, upcoming, live)
        └─ read from the STORE, not this cycle's response
      per item: skip if HasActiveJob → probe
        └─ members-only content uses the authenticated probe (unchanged)
      probe result ⇒ UPDATE published (exact), date_precision='exact', status, title
4. ARCHIVAL PASS  (runs after the probe pass, on corrected dates)
      in-scope = top-N query  ∪  {items WHERE status IN (upcoming, live)}
      per item: skip if HasActiveJob or HasProcessed → term match → decide:
         upcoming/live    → job  (cap exempt)
         vod/not_a_stream → job iff include_non_live_content
                                AND channel established
5. AddToHistory on job creation  (unchanged)
6. on RSS success ⇒ channel_state.last_rss_ok_at = now
```

**The passes read the store, not the response.** This is what removes the bug
class: an RSS 404 no longer changes either work list, it only means nothing new was
added that cycle.

**The two passes are separate because dates change mid-cycle.** The probe pass
corrects `published` to an authoritative value; the archival pass must run
afterwards so scope is computed on corrected dates rather than provisional ones.
Merging them would rank an item using the guess the probe just disproved.

**Step 2 stores items that do not match terms.** Non-obvious but load-bearing:
today the cap is applied at `feed.go:464` *before* term matching at `feed.go:721`,
so scope covers all channel content. Storing only term-matched items would compute
the top-N over a subset and get it wrong. Terms gate the *job*, not the store.

### The established gate

A fresh install whose very first cycle 404s would hold only membership items, and
top-3 of a 2-item store includes the old VOD. Therefore: **past content is not
archived until `channel_state.last_rss_ok_at IS NOT NULL` or `backfilled_at IS NOT
NULL`.** Upcoming/live still pass, being cap-exempt. This gate is independent of
the backfill, so a failed backfill never blocks normal operation.

## Part 2 — Full-Catalog Backfill

### Current capability

There is **no continuation pagination anywhere in the codebase**; every YouTube
path is first-page-only (~30 items). The only occurrence of
`continuationItemRenderer` is a test fixture
(`internal/youtube/channel_membership_test.go:27`) proving the walker ignores it.

Reusable as-is from `internal/youtube/channel_membership.go`:

- `extractYtInitialData` (`:295`) — tab-agnostic brace-balancing scanner
- `ytInitialTabs` envelope (`:114`) — lazy `json.RawMessage` tab bodies
- `walkVideoRenderers` (`:176`) — handles `lockupViewModel` and classic renderers
- `lockupTitle` (`:261`), `rendererTitle` (`:267`), fetch/cookie/header pattern

Membership-specific and needing parameterisation: the `TAB_ID_SPONSORSHIPS`
constant (`:22`) and its match (`:150`), the `/membership` URL literal (`:75`), the
`HasAuthCookies()` early return (`:65`), and the inverted `hasAccess` semantics.

The InnerTube transport already exists (`player_api_strategy.go:346`,
`auth.go:53` `GenerateAPIHeaders`, `constants.YouTubeURLs.API`) but nothing calls
`/browse`.

### Scan

Union `/videos` + `/streams` + `/membership`, dedup by video ID. This is
deliberately broader than yt-dlp, which omits the membership tab from "all
uploads" (`references/yt-dlp/yt_dlp/extractor/youtube/_tab.py:2318` carries
`XXX: Members-only tab should also be extracted`).

Paging ports yt-dlp's `_entries` loop (`_tab.py:571`), including:

- `seen_continuations` loop detection (`_tab.py:585-590`)
- `visitorData` re-extraction per page (`_tab.py:607`)
- `appendContinuationItemsAction.continuationItems` unwrapping (`_tab.py:628-631`)
- token extraction from
  `continuationItemRenderer.continuationEndpoint.continuationCommand.token`
  (`_base.py:1034`)

`UC`→`UU` uploads-playlist swap (`_tab.py:2326`) is the fallback handle for
channels lacking tabs.

### Classification during scan

The listing carries what we need, so the backfill leaves **nothing** in the
unknown pool:

- `"Streamed N <unit> ago"` present ⇒ `status='vod'`, `date_precision='coarse'`
- live badge ⇒ `status='live'`
- scheduled/upcoming badge ⇒ `status='upcoming'`
- `catalog_pos` = index within the listing

### Operational rules

- Throttled: one page per interval (constant, not config).
- Resumable: per-tab cursor in `channel_state.backfill_state`.
- `backfilled_at` set **only** on full completion of all three tabs, so a partial
  scan retries rather than silently claiming completeness.
- Runs in its own throttled path — **not** through the monitor's cycle loop, which
  does retries and backoff per video and would be pathological over thousands of
  items.
- Progress surfaced in Web UI and TUI.
- Inline `defer recover()` in the scan goroutine, per project rule.

## Error Handling

| Failure | Behavior |
|---|---|
| RSS 404/500 | Membership still runs; store untouched; `last_rss_ok_at` keeps prior value so trust persists |
| Membership fetch fails | Debug log only — unchanged; never marks RSS unhealthy |
| Probe fails | Existing `MetadataTracker` give-up + `ProbeCooldown`, untouched |
| Backfill fails mid-scan | Cursor saved, `backfilled_at` stays NULL, retries next startup |
| DB error | Skip the item this cycle — existing `HasActiveJob`/`HasProcessed` pattern |

The probe-failure path is deliberately untouched. A permanently unprobeable item
is re-probed each cycle exactly as today — `internal/monitor/utils.go:272-279` is
explicit that history does not stop re-probing and the cooldown is the only
limiter. No regression, no cooldown-default change.

**Noted follow-up (not this spec):** the store makes a durable per-item probe
backoff possible. `MetadataTracker` is in-memory today and resets on every restart.

## Testing

Regression test for this exact bug:

- **RSS 404 cycle + membership returns a 3-week-old VOD ⇒ not archived.**

Then:

- Coarse-tie lump: 20 items sharing one date, `N=3` ⇒ exactly 3 admitted (proves
  the total order)
- Upcoming below rank N ⇒ still probed and jobbed (proves cap-exemption)
- `vod` never re-probed; `unknown` probed exactly once
- Trust gate: fresh install, first cycle 404 ⇒ no past-content archival
- Top-N counts non-term-matching items
- Raising `max_feed_items` widens scope for already-stored VODs without a re-probe
- `published` frozen at first insert; upgraded only by higher precision
- Term matching: an RSS-carried description matches in-cycle; a store-only
  re-evaluation is title-only
- Backfill: fixture-driven continuation paging, loop detection, resume-from-cursor
- Migration v15→v16 idempotent

## Migration (v16)

Current `schemaVersion = 15` (`internal/database/migrations.go:26`). Follow the
established pattern: a sequential `if version < 16 { ... return
db.writeUserVersion(16) }` block, `CREATE TABLE/INDEX IF NOT EXISTS`, tables also
added to `createSchema`.

1. Create `feed_items`, `channel_state`, and `idx_feed_items_rank`.
2. `DROP TABLE IF EXISTS last_videos`.
3. Remove `GetLastVideo`/`SetLastVideo` (`database_extras.go:126-148`) and
   `TestLastVideos` (`database_test.go:119`).
4. Make the legacy JSON importer ignore `lastVideos` (`database_jobs.go:723`)
   rather than write a dropped table.

No data backfill inside the migration, so the `SetMaxOpenConns(1)` cursor hazard
(`migrations.go:242-244`) does not apply.

### `last_videos` removal justification

`GetLastVideo`/`SetLastVideo` have **zero non-test callers**. DECAPI — the
suspected consumer — makes exactly three DB calls: `HasActiveJob`
(`decapi.go:543`), `HasProcessed` (`decapi.go:565`), `AddToHistory`
(`decapi.go:589`). Rows can only ever arrive via the legacy JSON importer, and
nothing reads them.

Not to be confused with `LastVideoSeq`/`last_video_seq`, a **different and very
much live** field: the download-resume segment counter used in
`worker/orchestrator.go:270`, `strategy_youtube_dash.go:243`,
`twitch_recover.go:32`, and the TUI. That is untouched.

## Documentation Updates

`max_feed_items` keeps its name but changes meaning — archival depth, not per-tick
scan cost. Rewrite:

- `config.example.toml:69,208`
- `docs/spec/data-and-storage.md:458,526` (and `:320,400`, which wrongly claim
  `last_videos` "tracks the most recent video per channel for deduplication")
- `docs/spec/platform-services.md:178` (the `itemAge` cap description)
- `SPEC.md:210,653`
- TUI help text `internal/tui/settings.go:90` — currently *"RSS items per feed
  (default: 15)"*
- `internal/config/config.go:485` — the `O(N) per channel` per-tick comment

## Included Fixes

Two pre-existing defects, approved for inclusion.

**1. `max_feed_items` validation disagrees with itself.** `config.go:490` accepts
1–1000 and the TUI agrees (`internal/tui/settings.go:544`, *"must be 1-1000"*), but
`internal/web/routes/config_routes.go:170` rejects anything over 100. The same
setting has two limits depending on whether you edit TOML or the Web UI. The Web UI
is the outlier; align it to 1000.

**2. `.claude/skills/moombox-database-migrations/SKILL.md` is stale.** It documents
v6, a `schema_version` table, and `tx.Exec`. Reality is v15, `PRAGMA user_version`,
and direct `db.db` execution (no transaction wraps the migration). Anyone following
it writes a broken migration. Update to match `migrations.go`, and include the
`SetMaxOpenConns(1)` collect-then-update constraint, which the skill omits
entirely.

## Implementation Phasing

**Phase 1** — independently shippable, fixes the bug:
schema v16, `feed_items`/`channel_state`, rank query, probe rules, rewired
`checkChannel`/`processCandidate`, established gate, `last_videos` removal, the two
included fixes, doc updates.

**Phase 2** — lands on top:
InnerTube `/browse` continuation client, three-tab scan, listing classification,
throttled resumable backfill worker, triggers (startup/channel-add/manual),
progress UI.
