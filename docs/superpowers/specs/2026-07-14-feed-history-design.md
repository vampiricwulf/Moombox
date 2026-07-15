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
2. `max_feed_items` becomes a stable **archival-depth** boundary. "Stable" means
   *invariant with respect to fetch outcomes* — scope is a function of the
   channel's content and `N`, never of what a given cycle happened to retrieve.
   It is deliberately **not** immutable: raising `N` widens scope for content
   already in the store, and publishing N newer items pushes an item out of scope.
   Both are the boundary working as intended. What must never happen is scope
   moving because a fetch failed.
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
| Store structure | SQLite table + partial rank index + status index | Workload is one indexed top-N query per channel per cycle (~3 per 5 min); in-memory/materialised-rank optimise microseconds while adding cache-skew and renumber races |
| Cap gate | Coarse pre-filter **removed**. Scope is a store-driven top-N query; every job still follows a fresh probe | The pre-filter was the sole cause of the miss risk, and guarded a probe budget that no longer exists once status is recorded |
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
    probe_fails  INTEGER NOT NULL DEFAULT 0,  -- consecutive probe failures
    next_probe_at TEXT,                       -- NULL = probe freely; else earliest retry
    PRIMARY KEY (channel_id, video_id)
);
-- Serves the archival pass's top-N query. PARTIAL: rows excluded from ranking are
-- excluded from the index itself, so they cost nothing to skip. This matters —
-- 'assumed' rows carry published=now and therefore sort FIRST, so a non-partial
-- index would fetch and discard every one of them ahead of every real row, on
-- every query, forever (and under the relativeAgeRe breakage that is ~30 rows per
-- channel). Not a covering index: date_precision/status/title still require a
-- table lookup per returned row, which is fine at N rows.
CREATE INDEX IF NOT EXISTS idx_feed_items_rank
    ON feed_items(channel_id, published DESC, catalog_pos ASC, video_id ASC)
    WHERE date_precision <> 'assumed'
      AND status NOT IN ('upcoming', 'live');

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

**`itemAge` cannot express this today — changing it is Phase 1 work.** It returns a
bare `time.Duration` (`channel_membership.go:252`: `return time.Duration(n) * unit`)
and **discards the unit**. The unit is not recoverable from the result: `504h` is
either `"3 weeks"` or `"21 days"`, and `24h` is either `"1 day"` or `"24 hours"` —
identical durations, different skews. A naive implementation reading only this
spec's `now - Age - unit` would guess `now.Add(-2 * age)`, which is correct for
weeks and wrong for months and years.

So `itemAge` must return the **upper bound of the consistent range** (or the unit
alongside the age) rather than the truncated lower bound. That propagates to
`MembershipVideo.Age`, and to `membershipCandidates`' `v.Age > 0` test at
`feed.go:673` — which keeps working, since the upper bound is zero exactly when the
age is. This is a signature change across three files and must be budgeted, not
discovered.

**Two zeros, one meaning — almost.** `itemAge` returns `0` from two branches: the
explicit live-badge check (`:227-231`) and the unrecognized fallback (`:256`). This
spec's `assumed` framing ("a fabricated date") is only accurate for the second. For
a genuinely live item, `now` is a *correct* date, not a fabrication. It makes no
behavioral difference — both are excluded from ranking and both are probed — but the
justification text should not be read as claiming live items are mis-dated.

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
 WHERE channel_id = ?
   AND date_precision <> 'assumed'
   AND status NOT IN ('upcoming', 'live')
 ORDER BY published DESC, catalog_pos ASC, video_id ASC
 LIMIT ?
```

`idx_feed_items_rank` is **partial**, and its `WHERE` matches these two predicates
exactly — so excluded rows are not in the index at all: one seek, N rows, nothing
scanned and discarded. It is *not* a covering index (`date_precision`, `status` and
`title` still cost a table lookup per returned row), but that is N lookups, not a
scan.

**Verified, not assumed** (SQLite 3.50.4, 4000 rows, 3 channels). SQLite only uses
a partial index when it can prove the index's `WHERE` holds for every row the query
needs, and that proof is a syntactic term match — so the index `WHERE` and the query
predicates must be written identically, as they are above. Confirmed:

```
CREATE partial index with `status NOT IN (...)`  → accepted
EXPLAIN QUERY PLAN (top-N)          → SEARCH feed_items USING INDEX idx_feed_items_rank (channel_id=?)
EXPLAIN QUERY PLAN (discovery list) → SEARCH feed_items USING INDEX idx_feed_items_status (channel_id=? AND status=?)
```

No `SCAN`, and no `USE TEMP B-TREE FOR ORDER BY` — the index satisfies the ordering
as well as the filter, which is why the `DESC` in its definition matters. If the
implementation ever reworks these predicates, re-check the query plan: a partial
index silently degrades to a full scan when the term match stops lining up, and the
failure is invisible until the catalog is large.

(Checked against CPython's SQLite; `modernc.org/sqlite` is a transpilation of the
same upstream C source, so the planner behaviour is the same. Worth one assertion in
the migration test regardless.)

**`status NOT IN ('upcoming','live')` is what makes goal 3 true.** Without it the
exemption is only additive (`top-N ∪ {upcoming, live}`) — which archives them, but
leaves them *ranked*. They carry the newest dates, so they occupy the top of the
order and evict VODs from scope. At `N=3`, a channel with three scheduled premieres
would have a top-3 consisting entirely of premieres and an effective archival depth
of **zero**. On a channel that keeps two or three streams scheduled a week out, that
is the steady state, not an edge case. "Never consumes a cap slot" has to mean
excluded from the ranking, not merely exempted from the cut.

A permanently-failing row needs no exclusion of its own: it is stuck at `unknown`
(excluded by the `assumed` rule) or at `upcoming`/`live` (excluded by the status
rule), so it can never evict real content from scope. See "Bounding the probe list".

**The transition is the point, not a wrinkle.** When an upcoming stream ends it
becomes `vod` and *enters* the ranking at its (recent) date, taking rank 1 and
pushing the previous rank N out of scope. That is correct: a finished stream is
exactly the kind of content the archival depth is measured in, and it should
displace older content. Nothing needs to re-probe for this — the row is already
`exact` from the probe that observed the transition.

### `published` for upcoming and live rows

Never store the **scheduled start time**. It is in the future, so it would sort
above every real row — for weeks, on a stream scheduled far out — which is the same
eviction bug in a different disguise.

An `upcoming` or `live` row stores the moment we first saw it (`assumed`/`now` from
`itemAge`'s zero, or the probe's publish date once known). Its stored date is only
ever a *placeholder*, because those statuses are excluded from the ranking anyway.
The date starts mattering exactly when the row becomes `vod`, at which point the
probe that observed the transition has written an authoritative date.

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

**The honest cost:** excluding rows shrinks the population the top-N is computed
over, so while an `assumed` row is pending it is possible to admit an item that is
truly rank N+1. This is a real trade, not a free win — but it is the right
direction. Over-admitting by one position archives one slightly-older item; letting
a fabricated date rank *evicts real content* and, at `N=3`, can freeze a channel.
The window is normally one cycle (the discovery probe promotes the row to `exact`
immediately), and it only persists while probes are failing — during which we
would not be archiving that item anyway.

This is expressed as a **top-N query rather than a per-candidate rank check** for
two reasons:

1. **Raising `max_feed_items` must widen scope for existing content.** Because
   `vod` items are terminal and never *discovery*-probed, a per-candidate rank
   check evaluated only at probe time would leave an item skipped at `N=3` skipped
   forever, even after the operator sets `N=20`. The top-N query re-evaluates scope
   every cycle from the store, so a config change takes effect immediately.

   Precisely: **entering scope needs no probe; becoming a job still does.** The
   store already holds the date and status, so re-ranking is pure SQL. But the item
   then hits the archival branch, which fires a refresh probe before creating the
   job — because a job must never be built on stale metadata. The two statements are
   about different steps, and an earlier draft conflated them into "needs no probe".
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
(any status → deferred while next_probe_at > now; see the backoff below)
```

### Bounding the probe list without ever giving up

**A terminal "unreachable" state is the wrong answer, and an earlier draft of this
spec adopted it before catching why.** `MetadataTracker` gives up after a handful of
*consecutive* failures — a cookie hiccup or a YouTube 5xx spell is enough. Marking
the row terminal at that point means a members upcoming stream that was briefly
unprobeable is never probed again and is missed forever: the identical
unrecoverable goal-3 violation as the `HasProcessed` gate, reintroduced by the fix
meant to bound cost. Today a transient give-up stops nothing — the cooldown is a
no-op at its default of 0 and re-probing simply continues.

So the row is never retired; it is **rescheduled**. `probe_fails` and
`next_probe_at` give the store a durable per-item backoff:

```
probe fails    ⇒ probe_fails++
                 next_probe_at = now + backoff(probe_fails)  ONLY if status='unknown'
probe succeeds ⇒ probe_fails = 0, next_probe_at = NULL
discovery list = status IN (unknown, upcoming, live)
                 AND (next_probe_at IS NULL OR next_probe_at <= now)
```

**A known `upcoming` or `live` row is never deferred.** This asymmetry is the whole
point, and it follows from the project's stated priority order
(Correctness > Reliability > Efficiency):

- **`unknown`** — we have never successfully probed it, so we do not know it is a
  stream at all. Deferring costs nothing we can name. Safe.
- **`upcoming` / `live`** — a *successful* probe told us this is a stream. Deferring
  it trades the one thing that must not be traded: a deferred upcoming row that goes
  live during its backoff window is recorded late, and a late start is a partial
  miss of an unrepeatable event. Efficiency does not get to win that.

This also respects what give-up actually means. `RecordFailure`
(`internal/monitor/utils.go:61-76`) **deletes the counter** when it gives up at
`maxMetadataFailures = 3` (`:19-21`), and `utils.go:275-279` calls that "backing
off" and notes "giveUp also resets the tracker's escalation to 0". Give-up is a
recurring, deliberately transient backoff — not a verdict on the video. Treating it
as permanent (which a terminal state, or an unbounded backoff on a known stream,
would) misreads it.

An `unknown` dead ID decays toward a floor (say hourly, then daily) instead of
costing a probe every cycle forever; a recoverable one returns to full rate on its
first success. Nothing is ever permanently abandoned, so no stream can be lost this
way.

**Residual, accepted and named:** a row that was successfully probed as `upcoming`
and *then* deleted keeps `status='upcoming'`, is never deferred, and therefore
probes every cycle forever. We cannot distinguish "deleted permanently" from
"YouTube is 5xx-ing" without error-kind information `ProbeVideo` does not currently
surface — and given that ambiguity, probing a dead ID is the correct error to make.
The accumulation is slow (cancelled/deleted scheduled streams are rare — a handful
per channel per year), and bounded work per row is one `/player` call per cycle.

The clean fix is a `last_listed_at` column: a row that no listing has mentioned for
N days *and* whose probes are failing is gone, and can be deferred safely without
ever guessing about a stream that still exists. That needs listing-coverage data
Part 2 provides, so it is named here as a follow-up rather than designed now.

This is also strictly better than today in a way worth stating: `MetadataTracker` is
**in-memory** and resets on every restart, so today's give-up does not survive a
process restart at all. Persisting the schedule is the point of having a store.

**Decision flagged for the operator:** this is new machinery, adjacent to the
non-goal "changing the probe-failure/cooldown machinery", and it sits near a
standing preference that poll intervals — not cooldowns — should be the throttle.
It differs from `probe_cooldown` in that it never touches a healthy item: a row that
probes successfully has `next_probe_at = NULL` and is polled at full rate forever.
The backoff applies only to rows that are already failing, where the alternative is
a guaranteed-useless request every cycle in perpetuity.

### Why the probe list must be bounded at all

`status IN (unknown, upcoming, live)` is read from a store that **never forgets**,
and that breaks an assumption today's code gets for free.

Today, an item that can never be probed eventually scrolls out of RSS's 15-item
window and probing simply stops. There is no such escape here. An upcoming stream
that is cancelled and deleted probes 404 forever, keeps `status='upcoming'`, and is
re-probed **every cycle for the life of the install** — while also sitting in the
cap-exempt union forever. Every such event permanently adds one probe per cycle per
channel, monotonically, on a 24/7 process. Under the `relativeAgeRe` breakage
described in External Assumptions, that is ~30 permanent per-cycle probes per
channel.

An earlier draft of this spec claimed "no regression" here. That was wrong: it is a
regression created precisely by reading the store instead of the response, which is
the same property that fixes the original bug.

The backoff above is what bounds it. Note the ranking is unaffected either way: a
row stuck at `unknown` is excluded from the top-N by the `assumed` rule, and a row
stuck at `upcoming`/`live` is excluded by the status rule — so a dead ID can never
evict real content from scope regardless of how often it is retried.

`MetadataTracker` and `ProbeCooldown` themselves are untouched, per the non-goal.
The backoff is an additional store-level schedule layered on the outcome they
already produce.

### The probe outcome must surface to the caller, not be hooked inside

`ProcessYouTubeVideo` is shared: it is called from `internal/monitor/feed.go:753`
**and** `internal/monitor/decapi.go:583`. So the backoff must **not** be hooked into
its give-up branch — DECAPI would then write `feed_items` rows, breaking the
"no DECAPI-side change" non-goal and putting rank-1 DECAPI hits into a store the
design says they never enter.

Instead `ProcessYouTubeVideoResult` gains an **outcome** discriminator:

```
probed      — a probe ran and returned metadata          (utils.go:268-300)
errored     — a probe ran and failed                     (utils.go:269-294)
cooldown    — no probe ran; ProbeCooldown suppressed it  (utils.go:258-262)
passthrough — no probe ran; ProbeVideo is not wired      (utils.go:248-250)
```

**There are four, not three, and the fourth breaks the obvious rule.** An earlier
draft claimed all outcomes "collapse to `ShouldProcess=false`". Three do —
`utils.go:258-262` (cooldown) and `:294` (error) — but `p.ProbeVideo == nil` returns
`ShouldProcess: **true**` at `:248-250`, passing the item straight through
un-probed. So "not `ShouldProcess`" is not a usable proxy for "not fresh", and a
naive implementation deriving freshness from `ShouldProcess` would job on no
metadata whenever the probe is unwired.

`passthrough` counts as **fresh**, which preserves today's behavior exactly: with no
probe wired, `ProcessYouTubeVideo` jobs the item today, and it must continue to. It
is a test-and-backwards-compatibility path, not a production one — but the
discriminator has to be exhaustive or it silently re-derives the bug.

The feed monitor reads the outcome and owns both store writes; DECAPI ignores it and
behaves exactly as it does today.

This single addition serves both new rules: **`outcome == probed` is precisely the
"FRESH" predicate** the archival pass needs, and `outcome == errored` is what drives
`probe_fails`/`next_probe_at`. Neither is a new source of truth — both read a
decision `ProcessYouTubeVideo` already makes internally and currently discards.

**Refresh probe** — on demand, only when a stored item is about to become a job:

```
in-scope AND NOT HasProcessed AND term-match AND status IN (vod, not_a_stream)
    → re-probe now, then job on the fresh result
```

This preserves today's invariant that **job creation always follows a fresh
probe**. Without it, the archival pass would create jobs from probe data captured
in an arbitrarily old cycle — stale titles, and videos that may since have been
deleted or privated.

**The refresh probe writes its result back, and its result is authoritative.** Both
halves matter, and an earlier draft of this spec got both wrong by phrasing the rule
as "job iff *still* non-live":

- A stream can restart on the same video ID — `feed.go:702-703` says so explicitly
  ("not if merely finished — a stream may restart on the same URL"). So a stored
  `vod` can legitimately refresh to `live`. Treating a non-`vod` refresh as "skip"
  would refuse to archive a live stream: a goal-3 violation.
- Discarding the refresh result would leave the row `vod` forever. `vod` is not in
  the discovery probe list, so the restarted stream would never be looked at again
  and would be missed **permanently**, not just this cycle.

The refresh probe is therefore an ordinary probe that happens to be triggered by
archival rather than discovery: it updates `published`/`date_precision`/`status`/`title`
exactly like the discovery pass, and the job decision is re-made against what it
returned — never against what the store said beforehand.

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

**Listing-derived status is unconditional, and it subsumes the
`include_non_live_content` case.** One rule covers every source and every channel:

```
any listing item with a relative-age text (Age > 0) ⇒ status='vod', coarse
any listing item without one                        ⇒ status='unknown', assumed
```

This is a property of the **item**, not of the channel's config, so it must not be
scoped to `include_non_live_content = false` — an earlier draft specified it only
there, leaving membership status undefined on `include_non_live = true` channels.

Today `membershipCandidates` drops members items with `Age > 0` outright when
`include_non_live_content` is false (`internal/monitor/feed.go:673-675`), precisely
because they could never become jobs. The unified rule reproduces that outcome
without a special case: such an item is `vod`, `vod` is not discovery-probed, and
the archival branch skips it on the `include_non_live_content` check *before*
probing. Same behavior as today — and unlike today, the row is retained so ranking
still counts it (see "Step 2 stores items that do not match terms").

RSS items carry no status signal at all, so they enter as `unknown` and are probed —
exactly as today.

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
      per item: skip if HasActiveJob → skip if NOT term-match → probe
        └─ same order as today: HasActiveJob → terms → probe (feed.go:704-725)
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
                            → refresh-probe, WRITE THE RESULT BACK to the store,
                              then re-decide on the FRESH status:
                                 live/upcoming    → job (cap exempt)
                                 vod/not_a_stream → job
                                 probe errored    → no job, retry next cycle
                                 cooldown-skipped → no job, retry next cycle
                                 (same FRESH rule as the discovery pass —
                                  no fresh result, no job, never fall through
                                  on stored status)
         unknown          → NO JOB. We do not know what it is. Retry next cycle.
                            (Reachable: an RSS row enters as unknown with an
                            EXACT date, so it is in the top-N legitimately —
                            the query excludes 'assumed' precision, not
                            'unknown' status. If its probe fails it stays
                            unknown and holds its true rank, which is correct:
                            it is a real video at a real position, and an older
                            item must not take its slot.)
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

**Step 2 stores items that do not match terms — but does not probe them.** Both
halves matter, and they pull in opposite directions:

- **Store them.** Today the cap is applied at `feed.go:464` *before* term matching
  at `feed.go:721`, so scope covers all channel content. Storing only term-matched
  items would compute the top-N over a subset and get it wrong. Terms gate the
  *job*, not the store.
- **Do not probe them.** Today term matching *gates the probe*: `processCandidate`
  runs `HasActiveJob` → term match → **return if no match** → probe
  (`feed.go:704-725`). A non-matching video is never probed. Probing before term
  matching would start paying `/player` fetches for content that can never be
  jobbed on a channel using `terms` — a cost regression against goal 5, introduced
  purely by splitting the passes.

So the discovery pass keeps today's exact order: `HasActiveJob` → terms → probe. A
non-matching item is stored and ranked (so the top-N stays correct) and never
probed (so it stays free). Its `status` simply remains `unknown` forever, which is
harmless: status only drives probing and job decisions, and it does neither.

Term matching cannot be a SQL predicate — it needs the in-memory description for
RSS-carried items — so it stays a Go-side filter over the query's rows, exactly as
it is a Go-side filter over candidates today.

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

Classification **calls `itemAge` itself** — badge check included — rather than
re-deriving a rule from its regex:

```
age := itemAge(item)          // live-badge short-circuit FIRST, then the age regex
age > 0  ⇒ status='vod',     date_precision='coarse'   (skewed old; see above)
age == 0 ⇒ status='unknown', date_precision='assumed'  → probed
```

**The live-badge short-circuit is load-bearing and must not be dropped.** An earlier
draft specified "presence of a relative-age text, and nothing else", claiming it
mirrored `itemAge`'s philosophy. It mirrored half of it and discarded the guard that
makes that half safe:

- `itemAge` checks the live badge **first** and returns `0` immediately
  (`channel_membership.go:226-231`), with the comment *"Currently live → 'now',
  **regardless of any 'streaming for N' elapsed text**"*. The guard exists precisely
  because live items carry elapsed text.
- `relativeAgeRe` (`:49`) is matched against the **serialized JSON of the whole
  item**, so a live renderer's `"Started streaming 2 hours ago"` matches it
  (verified: the regex matches that string).

A bare-regex rule would therefore insert a **live stream** as a terminal `vod` — on
`/streams`, the tab most likely to list one. `status` is deliberately excluded from
the upsert's `DO UPDATE`, and `vod` is never discovery-probed, so nothing would ever
correct it. That is the precise failure the badge-derived-status objection was
raised against, arrived at from the opposite direction.

Calling `itemAge` keeps the safe property real rather than asserted: a live or
upcoming item short-circuits to `0` → `unknown` → probed. RSS-visible streams were
protected anyway by entering as `unknown`, but a members-only live stream first seen
by the backfill's membership tab has no such protection — it is exactly the case this
guard saves.

An earlier draft classified from badges (`live badge ⇒ live`, `scheduled badge ⇒
upcoming`, `"Streamed N ago" ⇒ vod`) and claimed it left "nothing in the unknown
pool". Two defects:

- **A plain upload matched none of the three rules.** No `"Streamed"` text, no
  badge — and that is most of `/videos` on most channels. Its status was undefined.
- **Badge-derived terminal status is unsafe.** Writing `not_a_stream`/`vod` from
  badge text means one DOM change at YouTube silently marks live and upcoming
  streams terminal. Terminal statuses are never discovery-probed, so those streams
  would never be looked at again — goal 3, permanently, with no correction path.

Calling `itemAge` fixes both. `relativeAgeRe` (`channel_membership.go:49`) matches a
bare `"3 weeks ago"` — the `"Streamed"` prefix is not required — so a plain upload
dates correctly as `coarse`/`vod`. And the badge short-circuit ahead of it keeps a
live item at `0` even though its renderer carries elapsed text, so it lands in
`unknown` and gets probed.

**The safe-side property holds only because of the badge check**, not because of the
age regex: `itemAge` returns `0` for a live item, for an upcoming item, and for
anything it cannot parse — and every one of those is probed. A wrong answer costs a
probe; it cannot cost a stream. That guarantee evaporates the moment the badge
short-circuit is dropped, which is why the classification calls `itemAge` rather
than re-implementing part of it.

`vod` versus `not_a_stream` is not worth distinguishing here: both are terminal,
both are gated by `include_non_live_content`, and the archival pass treats them
identically. The backfill writes `vod`; a later probe refines it if it ever matters.

**Probe volume stays bounded.** Only rows where `itemAge` returns `0` are probed —
live, upcoming, and unparseable items, a handful per channel — not the thousands of
catalog rows a naive `unknown` default would have queued.

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
        (an unguarded UPDATE — the precision guard governs the upsert only, and
         would otherwise reject a coarse backfill write onto a pre-existing
         exact RSS row, leaving the merged position permanently unapplied to
         exactly the recent rows where it decides archival)
3. set backfilled_at
```

Rows without a `published` (live/upcoming/unparseable, which enter as
`assumed`/`now`) sort by that `now` and land at the top of the merged order. Their
`catalog_pos` is irrelevant — those statuses are excluded from the rank index
entirely — so the sort is well-defined for every row rather than only for
coarse-dated ones.

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

**Idempotent at the DB level is not idempotent at the worker level.** This is the
trap: `backfilled_at` is set only on *completion*, so a channel whose scan is
in-flight still reads `backfilled_at IS NULL`. `kickMonitors` fires on every add,
remove, reorder, bulk PUT, and TUI settings save — so a user reordering channels
three times launches three concurrent scans of the same three tabs for the same
channel. The 30s debounce specified for the manual re-run does not help; it lives on
the POST route, not in the sweep.

The sweep therefore holds an **in-flight set** (channel ID → cancel func), owned by
the backfill worker, and skips any channel already scanning. The condition is
"`backfilled_at IS NULL` **and not already in flight**". The in-flight set is also
what makes cancellation-before-prune possible, below — one structure serves both.

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
| RSS 404/500 | Membership still runs and still writes its items. No RSS-sourced rows are added, and previously-stored ones remain — so scope is unchanged. `last_rss_ok_at` keeps its prior value, so trust persists. **This is the bug's failure mode, now inert.** |
| Membership fetch fails | Debug log only — unchanged; never marks RSS unhealthy |
| Probe fails | Existing `MetadataTracker` give-up + `ProbeCooldown`, untouched |
| Backfill fails mid-scan | Cursor saved, `backfilled_at` stays NULL, retries next startup |
| DB error | Skip the item this cycle — existing `HasActiveJob`/`HasProcessed` pattern |

The probe-failure **machinery** is deliberately untouched: `MetadataTracker` and
`ProbeCooldown` behave exactly as today, and the cooldown default does not change.
`internal/monitor/utils.go:272-279` remains accurate that history does not stop
re-probing and the cooldown is the only limiter.

**But "a permanently unprobeable item behaves exactly as today" is false, and an
earlier draft claimed it.** Today such an item scrolls out of RSS's 15-item window
once 15 newer items exist, and probing stops as a side effect of the response being
the work list. Reading the store instead removes that escape — the same property
that fixes the original bug — so the item would be probed forever.

That is what the durable per-item backoff (`probe_fails`/`next_probe_at`) is for:
see "Bounding the probe list without ever giving up". It reschedules a failing row
rather than retiring it, so cost is bounded without any possibility of permanently
abandoning a stream that could still recover. It also makes give-up survive a
restart, which today's in-memory `MetadataTracker` does not.

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
- An in-scope `unknown` row is never jobbed — and an RSS row whose probe fails
  stays `unknown` with its **exact** date, holding its true rank rather than
  yielding the slot to an older item (the top-N excludes `assumed` precision, not
  `unknown` status)
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
- A stored `vod` whose refresh probe returns `live` (stream restarted on the same
  ID) **is** jobbed, and the store is updated to `live` so it stays in the
  discovery list — it is not skipped as "no longer non-live"
- Backfill: rows persist per page, so a restart mid-scan resumes from the cursor
  rather than re-scanning; `catalog_pos` is only final once `backfilled_at` is set
- Removing a channel mid-backfill cancels the scan before the prune, and leaves no
  resurrected rows
- **Upcoming/live rows do not occupy ranking slots.** `N=3` + three scheduled
  premieres + one VOD from yesterday ⇒ the VOD is still in scope (goal 3)
- An `upcoming` row that ends becomes `vod`, **enters** the ranking at rank 1, and
  pushes the previous rank N out of scope
- A scheduled start time is never stored as `published`
- Probe give-up sets a backoff (`probe_fails`/`next_probe_at`), never a terminal
  state: an upcoming stream that is unprobeable for a few cycles (cookie hiccup,
  YouTube 5xx) is still probed later and still archived once it recovers. This is
  the goal-3 guard against the terminal-state design that was rejected.
- A successful probe resets `probe_fails` to 0 and clears `next_probe_at`, so a
  recovered row returns to full poll rate
- A healthy row is never deferred: `next_probe_at` stays NULL for anything that
  probes successfully
- **A known `upcoming`/`live` row is NEVER deferred**, however many times its probe
  has failed — it goes live on time and is caught on time. Only `unknown` rows back
  off.
- `passthrough` (`ProbeVideo` unwired) counts as fresh and still jobs, exactly as
  today — freshness must not be derived from `ShouldProcess`, which is `true` on
  that path
- **A DECAPI probe give-up writes no `feed_items` row.** `ProcessYouTubeVideo` is
  shared with `decapi.go:583`; the outcome is surfaced to the caller so only the
  feed monitor persists it (non-goal guard)
- Backfill: a plain upload (no "Streamed" text, no badge, just "3 weeks ago") is
  classified `vod`/`coarse` — not left `unknown`, not probed
- **Backfill: a LIVE item whose renderer carries elapsed text** (e.g. `"Started
  streaming 2 hours ago"`, which `relativeAgeRe` matches) is left `unknown` and
  **is** probed — not written as a terminal `vod`. This is the badge-short-circuit
  guard; without it the `/streams` tab silently retires live streams.
- `itemAge` returns a unit-preserving age so the coarse skew is computable
- Trust gate: fresh install, first cycle 404 ⇒ no past-content archival
- Top-N counts non-term-matching items, **and** a non-term-matching item is never
  probed (matching today's `HasActiveJob` → terms → probe order)
- Raising `max_feed_items` brings an already-stored VOD into scope with **no**
  discovery probe (pure re-rank), and the resulting job **does** follow a refresh
  probe — the two assertions are about different steps
- `published` frozen at first insert; upgraded only by higher precision
- Term matching: an RSS-carried description matches in-cycle; a store-only
  re-evaluation is title-only
- Backfill: fixture-driven continuation paging, loop detection, resume-from-cursor
- Migration v15→v16 idempotent
- **Query plan assertion:** the top-N query uses `idx_feed_items_rank` and does not
  fall back to a scan or a temp b-tree sort. A partial index degrades silently when
  the predicate term-match stops lining up, and nothing else would catch it until
  the catalog is large.

## Migration (v16)

Current `schemaVersion = 15` (`internal/database/migrations.go:26`). Follow the
established pattern: a sequential `if version < 16 { ... return
db.writeUserVersion(16) }` block, `CREATE TABLE/INDEX IF NOT EXISTS`, tables also
added to `createSchema`.

1. Create `feed_items`, `channel_state`, and **both** indexes — `idx_feed_items_rank`
   (partial) and `idx_feed_items_status`. Omitting the status index turns two
   per-cycle queries into full scans of the channel's catalog (see the schema
   block); it is not optional.
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

**In-code comments that assert the deleted model.** These are the highest-traffic
explanations of the exact mechanism being removed, and would be left describing a
cap that no longer exists:

- `internal/youtube/channel_membership.go:206-219` — *"it is ranked by that age so
  old VODs sink and get **crowded out of the cap**"* (also the `MembershipVideo.Age`
  doc, which changes with the unit-preserving signature)
- `internal/monitor/feed.go:660-669` — *"letting them **occupy shared cap slots**
  would only crowd out public videos that CAN be jobbed"*
- `internal/monitor/feed.go:415-424` — `checkChannel`'s merge/cap doc block, which
  describes the per-cycle merge being replaced wholesale

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
top-N query, discovery/refresh probe split with the freshness rule, terminal
durable per-item probe backoff (`probe_fails`/`next_probe_at`), a **probe-outcome
discriminator on `ProcessYouTubeVideoResult`** (today `probed`/`errored`/`cooldown`
all collapse to `ShouldProcess=false`; it supplies both the FRESH predicate and the
backoff trigger, and keeps DECAPI out of the store), established gate,
channel-removal prune,
**a unit-preserving `itemAge` signature** (today it discards the unit, making the
coarse skew uncomputable — propagates to `MembershipVideo.Age` and
`membershipCandidates`), **an injectable RSS fetch seam plus a new `feed_test.go`**
(neither exists today — see Testing), rewired `checkChannel`/`processCandidate`,
deletion of `TestMergeCandidatesRecencyCap`, `last_videos` removal, the included
fixes, doc updates.

**Phase 2** — lands on top:
InnerTube `/browse` continuation client (modelled on `internal/chat`, not ported
from yt-dlp), three-tab scan gated on `membership_discovery` + auth and filtered to
YouTube channels, listing classification, merged channel-global `catalog_pos`
assignment, throttled resumable backfill worker, the idempotent
`backfilled_at IS NULL` sweep, debounced manual re-run (`R` chord + API), and
progress via `hub.Broadcast` + a TUI `tea.Msg` channel — including the
`InitialState` seed.
