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
- Any DECAPI-side change. DECAPI parses a single video ID out of its response
  (`internal/monitor/decapi.go:523-534`) — the channel's *latest* — so it cannot
  reach back to old content and cannot be the source of this bug class. It keeps
  its existing `HasActiveJob`/`HasProcessed`/probe path and does **not** write to
  `feed_items`; anything it finds is by construction rank 1 and would be admitted
  by the cap anyway. This is why `decapi` is absent from the `source` enum.

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
returns, and `max_feed_items` allows up to 1000.

**Coarse dates lump, and lumps break naive ranking.** If twenty items all resolve
to "3 weeks ago" they share one timestamp. A naive `COUNT(*) WHERE published > ?`
gives them all the same rank, so a boundary landing inside a lump admits the
*entire lump* — this bug, amplified. A total order is mandatory.

**Tab order recovers lost precision — but only within one scan.** YouTube renders
tabs newest-first, so an item's position within a listing is true recency ordering
*within* a coarse lump. `catalog_pos` recovers what the relative text threw away.

The caveat is essential: a position is a coordinate **within a single listing**, and
`/videos` and `/streams` overlap heavily (a past stream appears in both, at
unrelated positions). Comparing position 5 of `/videos` against position 5 of
`/streams` is meaningless. So `catalog_pos` must be a **channel-global** coordinate
assigned by one merged ordering pass, not whichever tab happened to scan first.
See "Assigning catalog_pos" under Part 2.

**The probe is how we learn rank, not just status.** Anything undatable from a
listing gets a direct probe and returns an authoritative date. This is what
guarantees goal 4.

**The dated store makes probes cheaper, not rarer.** Today the capped N items are
probed *every cycle forever*, and most results are discarded by `nonLiveSkipReason`
immediately after. With recorded status, an item needs probing once. Steady state
falls from 3-per-channel-per-cycle to ~0–2 per cycle. This is why the pre-probe
rank filter could be removed rather than mitigated.

## External Assumptions (unverified in-repo)

Two numbers this design leans on are **external YouTube behavior, asserted from
observation and not enforced or documented anywhere in this codebase**. Called out
because they are load-bearing and could drift silently:

| Assumption | Status |
|---|---|
| YouTube's `videos.xml` returns at most 15 items | Not asserted in code. `fetchFeed` (`feed.go:484`) and `parseFeedCandidates` impose no limit; the `15` at `feed.go:24` and `config.go:58` is the *default cap*, an unrelated coincidence. |
| The membership tab carries ~30 items per page | Not asserted anywhere. `parseMembershipTab`/`walkVideoRenderers` take whatever the page carries. |

They justify "the backfill is not required for correctness" and the "~45 unknowns
per cycle" probe bound. Neither is safety-critical — if RSS returned 50 items the
design still holds, just with more first-cycle probes — but the plan should verify
the RSS figure empirically rather than inherit it, and **no code should encode
either number as a constant.**

A third, sharper fragility deserves the same treatment. `relativeAgeRe`
(`internal/youtube/channel_membership.go:49`) is a single en-locale regex matched
against serialized JSON, and `itemAge` returns `0` on any miss. One YouTube copy
change makes *every* membership item on *every* channel return `Age=0`. Today that
is self-limiting, because the cap is recomputed per cycle from live data. Under this
design those items all become `assumed` — which is exactly why `assumed` rows are
excluded from the top-N query. The failure mode degrades to "members content is not
archived until probed" instead of "fabricated dates dominate the cap", and probes
repair it automatically. This is the single best argument for the `assumed`
exclusion, and worth stating as such.

## Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Scope | One spec, phased implementation | Part 1 ships alone and fixes the bug; Part 2 lands on top |
| Store structure | SQLite table + covering index | Workload is one indexed top-N query per channel per cycle (~3 per 5 min); in-memory/materialised-rank optimise microseconds while adding cache-skew and renumber races |
| Cap gate | Coarse pre-filter **removed**; authoritative post-probe gate | Pre-filter was the sole cause of the miss risk, and guarded a budget that no longer exists |
| Cap meaning | Archival depth, not probe budget | Matches goal 2; probe volume is naturally bounded by sources |
| `history` table | Left alone, separate | Answers a different question ("acted on" vs "exists and when"); unifying would touch Twitch, DECAPI, orphan API, Web UI, TUI |
| Catalog scan role | Backfill only | Part 1 makes RSS 404s harmless; 3 MB/channel/cycle forever buys no correctness |
| Backfill trigger | One idempotent sweep keyed on `backfilled_at IS NULL`, run at startup and from `kickMonitors` (which fires on every channel add/remove/reorder), plus a manual re-run | No action needed on upgrade; new channels start complete. Not two triggers — `kickMonitors` cannot distinguish an add from a reorder, so it must be idempotent (see Integration Points) |
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
    source       TEXT NOT NULL,           -- rss|membership|videos|streams
    status       TEXT NOT NULL,           -- unknown|upcoming|live|vod|not_a_stream
    first_seen   TEXT NOT NULL,
    PRIMARY KEY (channel_id, video_id)
);
-- Serves the archival pass's top-N query.
CREATE INDEX IF NOT EXISTS idx_feed_items_rank
    ON feed_items(channel_id, published DESC, catalog_pos ASC, video_id ASC);

-- Serves the discovery probe list and the cap-exempt union, both of which filter
-- on status every cycle. Without this they degrade to a full scan of the
-- channel's rows — after Part 2's backfill that is the entire catalog, scanned
-- twice per cycle, forever (violating goal 5). The rank index cannot serve them:
-- status is not in it, and published leads.
CREATE INDEX IF NOT EXISTS idx_feed_items_status
    ON feed_items(channel_id, status);

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

That upgrade requires a **precision-guarded upsert, not `INSERT OR IGNORE`** — the
latter can only ever insert, so a first-seen coarse date would be permanent:

```sql
INSERT INTO feed_items (channel_id, video_id, title, published, date_precision, ...)
VALUES (?, ?, ?, ?, ?, ...)
ON CONFLICT(channel_id, video_id) DO UPDATE SET
    published      = excluded.published,
    date_precision = excluded.date_precision,
    source         = excluded.source
WHERE CASE excluded.date_precision WHEN 'exact' THEN 3 WHEN 'coarse' THEN 2 ELSE 1 END
    > CASE feed_items.date_precision WHEN 'exact' THEN 3 WHEN 'coarse' THEN 2 ELSE 1 END
```

The guard makes the write monotonic: a later, *worse* estimate can never overwrite
a better one, so ordering cannot regress no matter which source sees an item next.
This is reachable, not theoretical: the backfill records a 2-day-old stream as
`coarse`, RSS later carries an exact date for it, and being recent it is competing
for the top N — exactly where ordering decides whether it is archived.

Note `status` is deliberately **not** in the `DO UPDATE` set. Listing-derived
status is weaker than probe-derived status, and a stale listing must never demote a
probed `live` back to `vod`.

`title` is stored (the archival pass and job creation both need it) and refreshed
when a probe returns a better one, mirroring today's rule at
`internal/monitor/utils.go:341-344` that an `"Unknown Title"` placeholder must not
overwrite a real feed title.

### Coarse dates must skew old, not new

`itemAge` (`internal/youtube/channel_membership.go:220-257`) truncates: `"3 weeks
ago"` becomes exactly `21d`, `"1 year ago"` exactly `365d`. The true age is a
*range* — `[21d, 28d)` and `[365d, 730d)` respectively. Storing `now - Age` picks
the **newest instant consistent with the text**, biasing every coarse date newer by
up to 6 days (weeks), 29 days (months), or 364 days (years).

That bias runs in the dangerous direction: newer means higher rank, and higher rank
means *admitted*. It is the same class of error as the bug being fixed, just
smaller — a coarse members VOD can outrank genuinely newer public content on a
low-volume channel.

So coarse dates store the **oldest** instant consistent with the text —
`now - Age - unit` (the exclusive upper bound of the range):

```
"3 weeks ago"  → now - 28d   (not now - 21d)     Age=21d, unit=7d
"1 year ago"   → now - 730d  (not now - 365d)    Age=365d, unit=365d
```

**The skew applies to `coarse` only.** It is defined in terms of the matched unit,
and an `assumed` row has no matched unit — `Age = 0` is the *absence* of a parse,
not a measurement of zero. `assumed` rows keep `published = now` and are excluded
from the top-N query anyway, so the skew never applies to them. Live and upcoming
items likewise carry `Age = 0` and are cap-exempt, so nothing about their ordering
depends on this.

Errors then bias toward *exclusion*, which is the safe direction for an
archival-depth cap. This costs nothing real: ordering among old content does not
affect what gets archived, live/upcoming are cap-exempt regardless of date, and any
coarse item that matters is promoted to `exact` by its discovery probe.

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
already title-only (`internal/monitor/feed.go:694-696`: *"matching is title-only, like
DECAPI"*) — but it must be documented, since an RSS item whose *description* alone
matched the channel's terms would not be picked up by a later scope widening.

### The in-scope query

Total order is `(published DESC, catalog_pos ASC, video_id ASC)`. The archival
scope for a channel is simply the top N of the store:

```sql
SELECT video_id, title, published, status FROM feed_items
 WHERE channel_id = ? AND date_precision <> 'assumed'
 ORDER BY published DESC, catalog_pos ASC, video_id ASC
 LIMIT ?
```

The index covers this exactly — one seek, N rows, no scan.

**`date_precision <> 'assumed'` is what enforces goal 4.** An `assumed` row carries a
fabricated date: `itemAge` returns `0` for any item it cannot parse, which becomes
`published = now`. Letting that rank would place a guess at position 1 ahead of
every real date — the opposite of the goal. Excluding it costs nothing, because an
`assumed` row is always `unknown` and therefore always discovery-probed; the probe
promotes it to `exact` and it enters the ranking on a real date, usually within the
same cycle.

This also removes a denial-of-scope failure. `published` is frozen at insert, so if
the corrective probe fails permanently the fabricated "now" would sit in the top N
indefinitely, evicting real content from scope — at `N=3`, three such rows would
freeze a channel's archival scope entirely. An excluded row cannot evict anything.

An item that can never be probed therefore never enters scope, which is the correct
outcome: we know nothing about it, so we archive nothing on its behalf. It is still
probed every cycle, so it recovers the moment YouTube answers.

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

Probes have two distinct triggers. Conflating them is a mistake: one is discovery,
the other is metadata freshness before job creation.

**Discovery probes** — every cycle, rank ignored:

```
unknown  → probe (nothing can be missed)
upcoming → probe (goal 3)
live     → probe (goal 3)
vod / not_a_stream → NOT probed for discovery
```

**Refresh probe** — on demand, only when a stored item is about to become a job:

```
in-scope AND NOT HasProcessed AND term-match AND status IN (vod, not_a_stream)
    → re-probe now, then job on the fresh result
```

This preserves today's invariant that **job creation always follows a fresh
probe**. Without it, the archival pass would create jobs from probe data captured
in an arbitrarily old cycle — stale titles, and videos that may since have been
deleted or privated.

It also keeps a documented user workflow intact. `internal/monitor/utils.go:226-227`
describes deleting a job and clearing its Orphaned history entry so the video "can
be picked up again". That still works: clearing history makes `HasProcessed` false,
the item is still in the top N, so the refresh probe fires and it is re-jobbed —
provided it is still within the archival scope, which is the intended new limit.

The refresh probe is rare by construction: it only fires for an item that is
in-scope, unprocessed, and terminal — i.e. after a history clear or a
`max_feed_items` increase. Steady state never triggers it.

The efficiency win survives: a `vod` that is already processed, or out of scope, is
**never probed again**. That is the common case, and today it costs a full
`/player` fetch with retries every single cycle only to be discarded by
`nonLiveSkipReason` immediately after.

**`include_non_live_content = false` channels must not probe past content.** Today
`membershipCandidates` drops members items with `Age > 0` outright on such channels
(`internal/monitor/feed.go:673-675`) precisely because they could never become
jobs. This design still *stores* them — ranking must count all channel content (see
"Step 2 stores items that do not match terms") — but must not probe them. A
membership item with `Age > 0` carries a `"Streamed N ago"` text, which is a
positive past-stream signal, so it is inserted directly as `status='vod'`,
`date_precision='coarse'` and never enters the discovery probe list. Same outcome as
today, with the row retained for ordering. RSS items on such channels are still
probed, exactly as today, since RSS carries no status signal at all.

Discovery probe volume is bounded naturally: RSS returns ≤15 and the membership tab
~30, so a cycle cannot exceed ~45 unknowns, and only once. The first cycle after
upgrade pays a one-time cost (~17 probes for a 3-channel install).

### Steady-state flow (per channel, per cycle)

```
1. fetch RSS ─┐ (independent; either may fail)
   fetch /membership ─┘
2. UPSERT every item seen → feed_items  (precision-guarded; never downgrades)
      RSS  → published exact
      memb → coarse from Age; undatable → 'assumed' (provisional)
3. DISCOVERY PROBE PASS
      probe list = feed_items[channel] WHERE status IN (unknown, upcoming, live)
        └─ read from the STORE, not this cycle's response
      per item: skip if HasActiveJob → probe
        └─ members-only content uses the authenticated probe (unchanged)
      outcome per item:
        probed OK  ⇒ UPDATE published(exact), date_precision='exact', status, title
                     mark FRESH for this cycle
        probe error⇒ store untouched; NOT fresh
        cooldown   ⇒ probe skipped; NOT fresh
4. ARCHIVAL PASS  (runs after the probe pass, on corrected dates)
      in-scope = top-N query  ∪  {items WHERE status IN (upcoming, live)}
      per item: skip if HasActiveJob → skip if NOT term-match → decide:
         upcoming/live    → job iff FRESH this cycle
                            (cap exempt; NOT gated by HasProcessed — see below)
         vod/not_a_stream → skip if HasProcessed
                            skip unless include_non_live_content   ← before probing
                            skip unless channel established
                            → refresh-probe → job iff still non-live on the
                              fresh result
         unknown          → NO JOB. We do not know what it is. Retry next cycle.
                            (Unreachable via the top-N query, which excludes
                            'assumed' rows, but reachable for a coarse-dated
                            backfilled row whose probe fails — the branch must
                            exist.)
5. AddToHistory on job creation  (unchanged)
6. on RSS success ⇒ channel_state.last_rss_ok_at = now
```

**`HasProcessed` must gate ONLY the `vod`/`not_a_stream` branch.** This is not a
stylistic choice — gating live/upcoming on it loses streams permanently, which is
the worst outcome this system has.

Today `HasProcessed` never touches live/upcoming. `feed.go:730` passes it as
`IsReprobe`, and `IsReprobe` is consulted in exactly one place: `nonLiveSkipReason`
(`utils.go:228-236`), reachable only from the `not_a_stream` and `post_live`/`vod`
branches. The `live` and `upcoming` branches return `ShouldProcess: true`
regardless of history. Job creation dedups on the **job row** (`AddJob` returning
`added=false`, `cmd/moombox/monitor_callbacks.go:252-259`), not on history.

That distinction is load-bearing because **history can be written with no job row**:
the probe give-up branch calls `AddToHistory` after repeated failures
(`utils.go:283-285`). So:

1. A members upcoming stream is discovered; cookies hiccup; the probe fails enough
   times to give up ⇒ `AddToHistory(X)`, **no job row**, status stays `unknown`.
2. Cookies recover; the next discovery probe succeeds ⇒ status becomes `live`.
3. A `HasProcessed` gate on the live branch skips it. **The stream is never
   archived, and it is gone forever.**

Today step 3 jobs it. A design that regresses this would violate goal 3 via exactly
the transient failures this spec claims to leave untouched.

**Only FRESH items become jobs.** "Fresh" means this cycle's probe returned a
result — precisely today's `ShouldProcess == true`. This preserves the existing
invariant through the two-pass split, which otherwise silently breaks it:

- **Probe errored this cycle.** Stored status still says `live`; the stream has
  since ended and been privated. Jobbing on stored status would create a job for a
  dead video. Today the probe error returns `ShouldProcess=false` and nothing is
  created.
- **`ProbeCooldown` skipped the probe.** `utils.go:258-262` returns
  `ShouldProcess=false` when `ShouldProbe` is false. Since not changing the cooldown
  machinery is a non-goal, a stored status under a non-zero cooldown is arbitrarily
  stale and must not be treated as authoritative.

Freshness reconciles the two-pass split with both, and maps 1:1 onto today's
behavior: no fresh result, no job, retry next cycle.

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

**Phase 1 limitation, stated plainly.** Only Phase 2 ever sets `backfilled_at`, so
in Phase 1 the gate rests entirely on `last_rss_ok_at`. A channel whose RSS is
*permanently* broken — a dead or renamed channel ID, not the intermittent 404s that
motivated this spec — therefore never becomes established and never archives past
content, even legitimately new content, indefinitely. Live and upcoming streams are
unaffected.

This is the correct trade for Phase 1: without a single successful full listing we
have no basis for claiming to know a channel's newest N, and guessing is precisely
the bug being fixed. Phase 2 removes the limitation, because the catalog scan
establishes a channel without RSS at all. It is not a concern for the observed
failure — Jerry's RSS succeeds ~87% of cycles, so it establishes within minutes —
but an operator with a genuinely dead RSS feed would see silence, and that must be
documented rather than discovered.

**One residual worth naming:** a successful RSS fetch returning *zero* entries also
sets `last_rss_ok_at`. For a members-only channel with no public uploads, the store
then holds only membership items and all of them are in the top N. That is
**correct** — if a channel has fewer than N items total, its newest N is all of
them — but it is worth stating explicitly, because it looks like the original bug
and is not.

## Part 2 — Full-Catalog Backfill

### Current capability

No continuation paging exists on any YouTube **channel/browse listing** path —
every channel path is first-page-only. The only occurrence of
`continuationItemRenderer` under `internal/youtube/` is a test fixture
(`internal/youtube/channel_membership_test.go:27`) proving the walker ignores it.

**But continuation paging is not new to this codebase.** `internal/chat` already
does full InnerTube continuation paging and is the in-repo pattern Part 2 should
follow rather than porting yt-dlp from first principles:

- `internal/chat/api.go:172-178` — `FetchLiveChat`/`FetchChatReplay(ctx, continuation)`
- `internal/chat/downloader.go:412-423` — the paging loop (`cd.continuation = resp.NextContinuation`)
- `internal/chat/downloader.go:558-583` — stale-token recovery

yt-dlp's `_tab.py` remains the reference for the *browse-specific* response shape
(which renderers carry items, where the token hides), but the transport, retry, and
loop mechanics should mirror `internal/chat`, which already works in production
against the same API.

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
- `visitorData` re-extraction per page (`_tab.py:608`)
- `appendContinuationItemsAction.continuationItems` unwrapping (`_tab.py:628-631`)
- token extraction from
  `continuationItemRenderer.continuationEndpoint.continuationCommand.token`
  (`_base.py:1041`, unwrapped by `_extract_continuation_ep_data` at `_base.py:1027`)

`UC`→`UU` uploads-playlist swap (`_tab.py:2326`) is the fallback handle for
channels lacking tabs.

### Classification during scan

The listing carries what we need, so the backfill leaves **nothing** in the
unknown pool:

- `"Streamed N <unit> ago"` present ⇒ `status='vod'`, `date_precision='coarse'`
- live badge ⇒ `status='live'`
- scheduled/upcoming badge ⇒ `status='upcoming'`

### Assigning catalog_pos

`catalog_pos` must be **channel-global**, not per-tab. A per-tab index is unusable
as a tiebreaker because the three tabs are unrelated coordinate systems and
`/videos`/`/streams` overlap heavily — a past stream appears in both at different
positions, and the row is deduped by `(channel_id, video_id)`, so a per-tab value
would be permanently whichever tab scanned first. (The precision guard makes this
worse, not better: both tabs write `coarse`, so the second write is rejected
outright and cannot correct the position.)

The backfill therefore completes all three tab scans **before** assigning
positions, then does one merged ordering pass over the union:

```
1. scan /videos, /streams, /membership page by page
     └─ write each page's rows IMMEDIATELY, catalog_pos = provisional per-tab index
     └─ advance the per-tab cursor in channel_state.backfill_state
2. when all eligible tabs are exhausted, run one ORDERING PASS:
     └─ SELECT the channel's rows into a slice, CLOSE the cursor
     └─ sort by (published DESC, source rank, provisional pos ASC, video_id ASC)
     └─ UPDATE each row's catalog_pos = 0..n-1
3. set backfilled_at
```

**Rows are written as they are scanned, not buffered to the end.** The obvious
formulation — scan everything, merge in memory, then write — is incompatible with
"resumable via cursor": a restart mid-scan would destroy the buffer, so the cursor
would resume into a merge whose earlier half no longer exists. Writing per page
means progress survives a restart and the cursor means what it says.

The cost is that `catalog_pos` is provisional (per-tab) until the ordering pass
runs. That is harmless: a partially-backfilled channel's rows are old, coarse-dated
content that cannot reach the top N, and `backfilled_at` stays NULL until the
ordering pass completes, so nothing claims the catalog is ordered before it is.

**The ordering pass must collect-then-update.** With `SetMaxOpenConns(1)`
(`internal/database/database.go:177`), issuing UPDATEs while a SELECT cursor is
open deadlocks on the single connection — the hazard documented at
`internal/database/migrations.go:242-244`. Read rows into a slice, close the
cursor, then write.

Steady-state inserts (RSS, membership) keep using the per-fetch index, which is
sound there: RSS dates are `exact` and distinct, so they effectively never tie, and
a membership fetch is a single listing whose internal order is genuine recency.
`catalog_pos` only ever arbitrates *within* one `published` value.

**Known limit, accepted:** a tie between a backfilled row (global position) and a
later steady-state row (per-fetch position) is resolved deterministically but
arbitrarily. It requires identical `published` values across a coarse backfill row
and a coarse membership row, and it only changes ordering *within* a lump, never
whether the lump straddles N. Not worth a second coordinate column.

### Operational rules

- **YouTube channels only.** The backfill must re-apply the platform filter
  (`ch.GetPlatform() == "youtube"`, as `getYouTubeChannels` does at
  `internal/monitor/feed.go:809-823`). Twitch channels live in the *same*
  `[[channels]]` list, and this worker runs on its own path rather than through the
  monitor's cycle loop — so nothing else filters for it. Without this, a Twitch
  channel would be scanned as `youtube.com/channel/<twitch_login>/videos`, 404 on
  all three tabs, never set `backfilled_at`, and retry on every startup forever.
  The "no Twitch-side change" non-goal is only a guarantee if this filter exists.
- Throttled: one page per interval (constant, not config).
- Resumable: per-tab cursor in `channel_state.backfill_state`.
- `backfilled_at` set **only** on full completion of all three tabs, so a partial
  scan retries rather than silently claiming completeness.
- Runs in its own throttled path — **not** through the monitor's cycle loop, which
  does retries and backoff per video and would be pathological over thousands of
  items.
- Progress surfaced in Web UI and TUI.
- Inline `defer recover()` in the scan goroutine, per project rule.

## Integration Points

The spec previously assumed several of these existed in a shape they do not.

### Trigger: there is no config watcher — `kickMonitors` is the hook

There is no file watcher and no `fsnotify` dependency; `internal/config/store.go`
exposes only `Read`/`Snapshot`/`Update`/`SaveLocked`. "Hot-reload" means *callbacks
fired by the writer*. The single funnel for every runtime channel mutation is
`s.kickMonitors` (`cmd/moombox/services.go:568-577`), called from
`channel_routes.go:80-82` (add), `:121-123` (delete), `:186-188` (reorder),
`config_routes.go:743-744` (bulk PUT), and `tui_wiring.go:222` (TUI settings, which
bypasses `ChannelRoutes` entirely).

**`kickMonitors` is a bare `func()` with no add/remove/reorder discrimination** — it
fires identically on a reorder. So "auto on channel add" **cannot** be event-driven.
It must be an idempotent sweep keyed on `channel_state.backfilled_at IS NULL`,
invoked from `kickMonitors` and at startup. These are therefore **one trigger, not
two**, and the Decisions table's phrasing is shorthand for that.

`setup_routes.go:176-177` deliberately does not fire the channel-change callback
(the process restarts instead), so setup-wizard channels are covered by the startup
sweep.

### Channel removal must prune, and there is a precedent

`feed_items` is per-channel state on a 24/7 process, on disk, unbounded per channel
(the backfill writes the entire catalog), and it survives restarts. The established
pattern is `PruneHealth` — `internal/monitor/health.go:110-112` states it exists so
the map "can't grow unbounded on a 24/7 process", implemented at `feed.go:151-158`
and called from `services.go:571-573`.

So the same `kickMonitors` sweep deletes `feed_items`/`channel_state` rows for
channels no longer in the active set. This also fixes a real bug: a
removed-then-re-added channel would otherwise inherit a stale `backfilled_at` and
silently skip its backfill.

**Caveat:** this is the **first channel-keyed DB cleanup in the codebase**. Nothing
in `internal/database/` deletes by channel today — `ListOrphanedHistory` joins on
`jobs.id`, and `pruneHistory` is a global 10k FIFO. We are establishing a pattern,
not extending one.

**The prune races the backfill, and both arrive via `kickMonitors`.** Removing a
channel mid-scan means the prune deletes its rows while the scan is still writing
them — resurrecting a channel that no longer exists, with a half-written catalog and
a NULL `backfilled_at` that no sweep will ever revisit (it is no longer in the
active set). It would sit there forever.

Ordering fixes it: `kickMonitors` **cancels any in-flight backfill for channels
leaving the active set, waits for the worker to observe cancellation, and only then
prunes.** The backfill worker additionally re-checks membership of the active set
before each page write, so a cancellation that lands between the check and the write
costs at most one stale page — which the prune then removes, because the prune runs
last.

This is why the prune belongs in the same sweep as the trigger rather than in the
delete route: `channel_routes.go:121-123` fires `kickMonitors` *after* the config is
written, so the active set is already authoritative when the sweep reads it.

### The backfill must respect `membership_discovery` and the auth gate

The per-cycle membership path is gated by `membershipActive()`
(`internal/monitor/feed.go:516-523`) on `MembershipDiscoveryEnabled()`
(`internal/config/types.go:109-110`) and a wired fetcher. The reuse inventory flags
`HasAuthCookies()` (`channel_membership.go:65`) as needing "parameterisation" —
which must not mean *removing* the gate.

This matters more than it looks: **the archival pass reads the store, not the
response.** If the backfill writes members-only rows on a channel with
`membership_discovery = false`, those rows become in-scope and get jobbed — the
toggle is silently defeated by the very indirection that fixes the original bug.

So: the backfill's membership tab is gated on `MembershipDiscoveryEnabled() &&
HasAuthCookies()`. `/videos` and `/streams` are public and are not gated.

**`backfilled_at` semantics when the membership tab is skipped:** it is still set on
completion of the tabs that were *eligible*. The flag means "this channel's catalog
has been scanned as fully as its configuration permits", not "all three tabs ran" —
otherwise a `membership_discovery = false` channel could never complete a backfill
and would retry forever. Turning membership discovery **on** later must therefore
clear `backfilled_at` to force a rescan, since the eligible set changed.

### Progress: two mechanisms, and an initial-state trap

- **Web:** the generic `hub.Broadcast(msgType, payload)` (`internal/web/websocket.go:441`).
  No typed wrapper needed — `disk_status` (`cmd/moombox/main.go:676-681`) and
  `update_available` (`helpers.go:142`) already use the generic path.
- **TUI:** not WebSocket. A buffered channel plus `tea.Msg`, modelled on
  `DiskStatusMsg`: type at `internal/tui/app.go:60-64`, channel at
  `services.go:609`, non-blocking send with a `default:` drop at `main.go:684-689`,
  wired via `SetUpdateChannels` (`tui_wiring.go:404`), handled at
  `app_update.go:277-279` — where the trailing `return a, a.listenForUpdates()` is
  **mandatory** to re-arm the receive.

**The trap:** `InitialState` (`cmd/moombox/ws_wiring.go:87-111`) returns only
`jobs`, `logs`, `nextFeedCheck`, `nextDecapiCheck`, `nextTwitchCheck`,
`connectivity`, `hideFinishedAgeDays`. `disk_status` is absent, so a web client
connecting mid-event renders nothing until the next tick. The TUI seeds itself
(`tui_wiring.go:409-415`); the Web UI does not. A throttled full-catalog backfill is
long-running *by design*, so mid-flight connect is the **common case**: backfill
progress must be added to `InitialState` and the TUI seed mirrored.

### Manual re-run: `R` chord + debounced route

- **Chord `R` (Request)** — the global namespace (`A` is job-scoped). Model on
  `R M` "Check Monitors Now": callback field `internal/tui/app.go:411`, menu
  registration **gated on non-nil** (`app_actions.go:489-491` — an unregistered
  chord is rejected by the parser at `:492-495`), dispatch at `:185-189`, host
  wiring at `cmd/moombox/tui_wiring.go:225-229`. For an async trigger use the `R V`
  shape (`app_actions.go:173-184`): capture the fn and return `safeCmd(...)`
  producing a one-shot msg — never block the update loop.
- **API** — model on `POST /api/monitors/check-now` (`internal/web/routes/monitors.go:34`),
  which carries a **30s `atomic.Int64` debounce** (`monitors.go:22`, `:39-51`)
  returning HTTP 200 `{"success":false,"debounced":true,"retryAfterMs":N}`. A manual
  backfill re-run needs the same guard against overlapping scans.

Both front doors share one service func, exactly as `kickMonitors` does today.

Note: `buildMenuItems()`/`dispatchAction()` live in `internal/tui/app_actions.go:430`/`:18`,
**not** `app.go` as CLAUDE.md claims — see Included Fixes.

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

### Prerequisite: `checkChannel` is not testable today

The headline regression test cannot be written against the current code, and this
is implementation work the plan must budget for, not an assumption:

- **There is no `feed_test.go`.** `internal/monitor/` has `connectivity_test.go`,
  `health_test.go`, `membership_test.go`, `twitch_recover_test.go`,
  `utils_test.go` — nothing covers `checkChannel`.
- **There is no fetch seam.** `checkChannel` calls `fm.fetchFeed(ctx, ch)` directly
  (`feed.go:426`), which does a real HTTP GET (`feed.go:484`). RSS failure cannot be
  simulated. By contrast `FetchMembership` is already an injectable func field
  (`MembershipFetchFunc`, `feed.go:94`) — the RSS path needs the same treatment.

So Phase 1 includes: introduce an injectable RSS fetch seam mirroring
`MembershipFetchFunc`, and create `feed_test.go`. Fixtures stay **inline** —
`internal/monitor/` has no `testdata/` directory and every existing fixture is
inline (`membership_test.go:207` uses inline XML; `channel_membership_test.go:27`
inline HTML). Do not introduce `testdata/` for this.

### Test to delete

`internal/monitor/membership_test.go:83-108` `TestMergeCandidatesRecencyCap` calls
`mergeCandidates(cands, 2)` and asserts precisely the cap-crowding behavior this
design removes. It must be deleted or rewritten against the top-N query — it cannot
survive as-is.

### Regression test for this exact bug

- **RSS 404 cycle + membership returns a 3-week-old VOD ⇒ not archived.**

Then:

- Coarse-tie lump: 20 items sharing one date, `N=3` ⇒ exactly 3 admitted (proves
  the total order)
- Upcoming below rank N ⇒ still probed and jobbed (proves cap-exemption)
- Processed/out-of-scope `vod` never re-probed; `unknown` probed exactly once
- Job creation always follows a fresh probe, including via the archival pass
- Clearing orphaned history for an in-scope VOD re-jobs it (refresh probe fires);
  clearing it for an out-of-scope VOD does **not**
- Precision guard: a later `coarse` write never overwrites a stored `exact`; a
  later `exact` write does overwrite a stored `coarse`
- A stale listing never demotes a probed `live` back to `vod`
- An `assumed` row never enters the top-N query (goal 4), and a permanently
  unprobeable `assumed` row cannot evict real content from scope
- An in-scope `unknown` row is never jobbed
- `include_non_live_content = false`: a past members VOD is stored but never
  discovery-probed, preserving today's drop behavior
- **`HasProcessed` does not block a live/upcoming job.** Specifically: probe
  give-up wrote history with no job row ⇒ probe later succeeds ⇒ stream is still
  jobbed. This is the goal-3 regression guard.
- A stale stored `live` whose probe errored this cycle produces **no** job
- With `probe_cooldown > 0`, a cooldown-skipped item produces **no** job
- Coarse dates skew old: `"3 weeks ago"` stores `now-28d`, not `now-21d`
- `membership_discovery = false` ⇒ backfill writes no members rows, and none are
  jobbed via the store
- Channel removed from config ⇒ its `feed_items`/`channel_state` rows are pruned;
  re-adding it triggers a fresh backfill rather than inheriting `backfilled_at`
- Backfill skips Twitch channels entirely
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
- `docs/spec/data-and-storage.md:458,526` — the `MaxFeedItems` tables
- `docs/spec/data-and-storage.md:400` — wrongly claims `last_videos` "tracks the
  most recent video per channel for deduplication" (`:320-325` is the schema block
  above it; both go)
- `docs/spec/data-and-storage.md:337` — migration table; **add a v16 row**
- `docs/spec/data-and-storage.md:403` — `ImportFromJSON` description (changes with
  the `lastVideos` no-op)
- `docs/spec/data-and-storage.md:579,591` — migration table + `MaxFeedItems: min 1`
  validation doc
- `docs/spec/architecture.md:127` — describes `max_feed_items`; previously missed
  entirely
- `docs/spec/platform-services.md:178` (the `itemAge` cap description)
- `SPEC.md:210,653`
- TUI help text `internal/tui/settings.go:90` — currently *"RSS items per feed
  (default: 15)"*
- TUI setup wizard `internal/tui/setup_wizard.go:113` — *"RSS feed items to check
  per channel"*
- Web UI text: `web/public/index.html:795-800` (`cfg-max-feed-items` label/help) and
  `:1682` (`setup-max-feed-items` label) — previously no Web UI text was listed
- `internal/config/config.go:485` — the `O(N) per channel` per-tick comment

## Included Fixes

Pre-existing defects found while mapping this work, approved for inclusion.

**1. `max_feed_items` validation disagrees with itself — in three places, split 2-2.**

| Site | Limit |
|---|---|
| `internal/config/config.go:490` (TOML load + validate — authoritative) | 1–1000 |
| `internal/tui/settings.go:544` (TUI settings) | 1–1000 |
| `internal/web/routes/config_routes.go:169` (Web API) | **1–100** |
| `internal/tui/setup_wizard.go:1066` (TUI first-run wizard) | **1–100** |
| `web/public/index.html:798` (`cfg-max-feed-items`, Web settings form) | **1–100** |
| `web/public/index.html:1682` (`setup-max-feed-items`, Web setup wizard) | **1–100** |

The accepted range depends on *which UI you happen to use*: `500` is valid via TOML
or the TUI settings screen and rejected by four other front doors. Align every
100-gate to 1–1000, matching `config.go:490` — the only validator that actually
guards the loaded config. `web/public/modules/setup.js:682` feeds
`setup-max-feed-items` and needs no change beyond the `max` attribute.

Note both the "one outlier" and "even split" framings are wrong; this is four
client-side gates disagreeing with one authoritative server-side validator.

**2. `.claude/skills/moombox-database-migrations/SKILL.md` is stale.** It documents
v6 (line 8), a `schema_version` table with `UPDATE schema_version SET version = 7`
(line 27), and `tx.Exec` (lines 20-27). Reality is v15, `PRAGMA user_version` via
`writeUserVersion`, and direct `db.db.ExecContext` with no transaction wrapping the
migration. Anyone following it writes a broken migration. Update to match
`migrations.go`, and add the `SetMaxOpenConns(1)` collect-then-update constraint,
which the skill omits entirely.

While rewriting it, scope step 4 ("Update Field Maps") explicitly to the `jobs`
table. `fieldToColumn` (`internal/database/database.go:21`, consumed at `:356`) is
jobs-only and enforced by `TestFieldToColumnCoverage` (`database_test.go:1222`); the
step currently reads as unconditional, so the next reader adds `feed_items` entries
to a jobs-only allowlist. It is genuinely N/A for this design's new tables.

**3. `CLAUDE.md` misplaces the chord system.** It states `buildMenuItems()` is "in
`app.go`" and is the single source of truth for chords. Both `buildMenuItems()` and
`dispatchAction()` actually live in `internal/tui/app_actions.go` (`:430` and `:18`).
The instruction "one entry in `buildMenuItems()` + one case in `dispatchAction()`"
is still correct; only the file is wrong. This matters here because Part 2 adds an
`R` chord by following exactly that instruction.

## Implementation Phasing

**Phase 1** — independently shippable, fixes the bug:
schema v16 (`feed_items`, `channel_state`, both indexes), precision-guarded upsert,
top-N query, discovery/refresh probe split with the freshness rule, established
gate, channel-removal prune, **an injectable RSS fetch seam plus a new
`feed_test.go`** (neither exists today — see Testing), rewired
`checkChannel`/`processCandidate`, deletion of `TestMergeCandidatesRecencyCap`,
`last_videos` removal, the included fixes, doc updates.

**Phase 2** — lands on top:
InnerTube `/browse` continuation client (modelled on `internal/chat`, not ported
from yt-dlp), three-tab scan gated on `membership_discovery` + auth and filtered to
YouTube channels, listing classification, merged channel-global `catalog_pos`
assignment, throttled resumable backfill worker, the idempotent
`backfilled_at IS NULL` sweep, debounced manual re-run (`R` chord + API), and
progress via `hub.Broadcast` + a TUI `tea.Msg` channel — including the
`InitialState` seed.
