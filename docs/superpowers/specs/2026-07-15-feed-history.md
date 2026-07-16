# Feed History

**Status:** Ready for review
**Date:** 2026-07-15
**Schema:** v15 → v16

Written fresh from the owner's decisions and from code verified in this repository.
Every `file.go:NNN` below was checked against the tree.

---

## 1. The bug

On 2026-07-14 Moombox downloaded `gr-ZTohjwnQ` — a members-only VOD from June 24,
three weeks old — from a channel configured with `max_feed_items = 3`.

`max_feed_items` does not mean "the 3 newest videos on the channel". It means *"the 3
newest of whatever we happened to collect this cycle"*. `checkChannel`
(`feed.go:425-479`) treats an RSS failure as non-fatal, then caps the survivors:

```go
data, rssErr := fm.fetchFeed(ctx, ch)
var candidates []discoveredVideo
if rssErr == nil { ... candidates = append(candidates, parsed...) }   // skipped on 404
if fm.membershipActive() { candidates = append(candidates, membershipCandidates(...)...) }
candidates = mergeCandidates(candidates, fm.maxFeedItems(ch))         // caps the survivors
```

When RSS 404'd, ~15 public items vanished from the pool. Two membership items remained
— under the cap — so nothing was truncated and the old VOD reached a probe.

Reconstructed from `moombox.log`:

| RSS that cycle | Cycles | `gr-ZTohjwnQ` surfaced |
|---|---|---|
| Succeeded | 419 | **0** |
| 404'd | 31 | **23** |

Across 480 cycles, RSS failed ~13% of the time on every YouTube channel. `itemAge` is
not at fault — it dates the VOD correctly at ~3 weeks, which is why it sinks whenever
RSS succeeds. **The flaw is that a partial fetch failure silently changes what the cap
means.**

The video had also been cancelled the day before. `AddToHistory` runs at job creation
(`monitor_callbacks.go:260`), so it *was* in history — but an orphaned-history purge
removed 80 rows including its own, and the next 404 cycle re-downloaded it. So
`history` was the only thing holding it back, and history is prunable.

---

## 2. Purpose

Moombox archives streams that are at risk of disappearing. `docs/spec/vision-and-purpose.md`
states it: *"VODs may be deleted, made members-only, or lose chat entirely… never miss
a stream, never lose an archive, never babysit the process."*

That is what makes the failure mode asymmetric and drives every trade below: **a
wrongly-archived video is visible and cancellable; a stream that was never archived is
gone.**

---

## 3. The model

Per channel, Moombox keeps **a list of every video it has seen, ordered by recency**.

- **The backfill** populates it once, from `/videos` + `/streams` + `/membership`
  combined, deduped and ordered by recency, down to the configured depth.
- **RSS and the membership tab** then keep it current each cycle: they update metadata
  on entries already in the list and add new ones.
- **Scope is a time window** — `archive_window_days`, default 3. Everything published
  inside it is archived; everything outside is not.
- **`archive_slots` (M, default 3) is throughput, not depth.** Everything inside the
  window is archived *eventually*, M at a time per channel, most-recent-first. A
  completed job frees a slot and the next item fills it.
- **Upcoming and live are exempt from all of it** — never windowed, never throttled,
  never counted.

The list is the thing that fixes the bug: an RSS 404 means "nothing new was added this
cycle", not "the channel now has two videos".

---

## 4. Glossary

| Term | Means |
|---|---|
| **the window** | `published >= now - archive_window_days`. Scope |
| **Q1 / Q2** | the two queries that form scope. Q1 = the window range. Q2 = the unconditional `upcoming`/`live` union |
| **M** | `archive_slots`. Max in-flight backlog jobs per channel |
| **backlog** | a video already in the list before this cycle. The only thing M paces |
| **new** | a video first inserted into the list this cycle |
| **admitted** | a job the scheduler moved `Queued → Upcoming` and enqueued |
| **FRESH** | this cycle's probe returned metadata (`outcome == probed`) |
| **denied** | a probe result we distrust: YouTube refused us *and* the classifier guessed |
| **the walk** | the serial, per-source, early-exiting probe pass over the window |
| **established** | `last_rss_ok_at IS NOT NULL OR backfilled_at IS NOT NULL` |

**One `now` per cycle.** Compute `cutoff := time.Now().UTC().Add(-window)` once at the
top of the cycle; every window test uses it. The walk is serial and can run for tens of
seconds — re-evaluating `now` per row lets an item cross the boundary mid-pass.

---

## 5. Config

`max_feed_items` is **deleted**, not migrated. It bounded depth; M bounds concurrency;
the window has no old counterpart. Any mapping would preserve a shape, not an intent.
Every install takes the new defaults.

| Setting | Range | Default | Scope |
|---|---|---|---|
| `archive_window_days` | 1–3650 | 3 | global + per-channel override |
| `archive_slots` | 1–100 | 3 | global + per-channel override |
| `num_parallel_downloads` | ≥1 | **10** (was 2) | global |

**Both new settings are two-level**, mirroring `max_feed_items` today: a global
`Monitors.<X>` plus a `ChannelConfig.<X> *int` override, resolved override → global →
constant, exactly as `fm.maxFeedItems(ch)` does at `feed.go:553-565`. Global-only would
drop a capability operators have now, and the window makes per-channel matter more: a
daily streamer and a monthly one want different depths.

**One validator, in `config.go`.** Today only the global is checked (`config.go:490`);
`ChannelConfig.MaxFeedItems` (`types.go:265`) is range-checked **nowhere**. The new
validator must iterate `cfg.Channels` and check each non-nil override against the same
range — otherwise `archive_slots = 100000` on one channel bypasses the stated range,
which is the six-way validation split this deletion exists to end. Out-of-range follows
the established contract (`config.go:490-493`): warn and reset to the default; an
out-of-range **override** is cleared to nil, falling back to the global.

Note `fm.maxFeedItems` treats `*ch.MaxFeedItems > 0` as "set", so an override of `0`
silently falls through to the global. The new resolvers replicate that: `0` means unset.

`include_non_live_content` is unchanged: a per-channel plain `bool` (`types.go:264`),
defaulting **false**. With it off, VODs never job at all.

---

## 6. Schema (v16)

```sql
CREATE TABLE IF NOT EXISTS feed_items (
    channel_id     TEXT NOT NULL,
    video_id       TEXT NOT NULL,
    title          TEXT NOT NULL DEFAULT '',
    published      TEXT NOT NULL,   -- RFC3339 UTC. Frozen at insert; only upgraded
    date_precision TEXT NOT NULL,   -- assumed|coarse|day|exact|started
    catalog_pos    INTEGER NOT NULL DEFAULT 0,
    source         TEXT NOT NULL,   -- rss|membership|videos|streams
    status         TEXT NOT NULL,   -- unknown|upcoming|live|vod|not_a_stream
    first_seen     TEXT NOT NULL,
    PRIMARY KEY (channel_id, video_id)
);

-- Q1: the window range. DESC lets the index satisfy ORDER BY as well as the
-- filter, so there is no temp b-tree.
CREATE INDEX IF NOT EXISTS idx_feed_items_window
    ON feed_items(channel_id, published DESC, catalog_pos ASC, video_id ASC);

-- Q2: the upcoming/live union, read with no date term. Without this index Q2 scans
-- every row the channel has, every cycle — and that set only grows, because the
-- store never forgets. The window index cannot serve it: status is not in it, and
-- published leads.
CREATE INDEX IF NOT EXISTS idx_feed_items_status
    ON feed_items(channel_id, status);

-- Both writers (the feed monitor's last_rss_ok_at, the backfill's backfilled_at)
-- use INSERT ... ON CONFLICT(channel_id) DO UPDATE, so neither depends on the other
-- having run and nothing creates rows up front.
CREATE TABLE IF NOT EXISTS channel_state (
    channel_id             TEXT PRIMARY KEY,
    backfilled_at          TEXT,     -- NULL until a scan completes
    backfilled_window_days INTEGER,  -- the window the scan ran AT
    backfill_state         TEXT,     -- JSON: per-tab cursor (see §11)
    last_rss_ok_at         TEXT
);

ALTER TABLE jobs ADD COLUMN channel_id TEXT;
ALTER TABLE jobs ADD COLUMN queue_priority INTEGER NOT NULL DEFAULT 1;
```

**Column contracts.** Every column has one writer and one reader:

| Column | Written by | Read by |
|---|---|---|
| `published`, `date_precision` | §7 step 2 upsert (guarded); the probe write (same statement) | Q1, the walk |
| `catalog_pos` | step 2, from the listing index; the backfill's ordering pass overwrites it once | the walk's lump tiebreak |
| `source` | step 2, **every** sighting; a `members_only` refusal (§9) | probe selection, the read arm |
| `status` | probes only — **never** a listing (§7 step 2) | the walk, the archival pass |
| `first_seen` | step 2 insert | `RETURNING`, to identify new rows this cycle (§10) |
| `channel_state.*` | the feed monitor and the backfill, both upserting | the established gate, the sweep |
| `jobs.channel_id` | job creation | the scheduler's M count |
| `jobs.queue_priority` | job creation | the scheduler's ordering |

`jobs.channel_id` is NULL on legacy, Twitch and manual rows; the scheduler filters
`channel_id IS NOT NULL`. `queue_priority` **must** default to 1: `ALTER TABLE` without
a default yields NULL, and SQLite sorts NULL *first* under `ORDER BY … ASC`, so every
legacy job would sort ahead of all backlog.

### The upsert

`INSERT OR IGNORE` cannot work — it only ever inserts, so a first-seen coarse date
would be permanent.

```sql
INSERT INTO feed_items
  (channel_id, video_id, title, published, date_precision, catalog_pos, source,
   status, first_seen)
VALUES (?, ?, ?, ?, ?, ?, ?, 'unknown', ?)
ON CONFLICT(channel_id, video_id) DO UPDATE SET
    -- ALWAYS: a fact about THIS sighting. It selects the probe, so it must be current
    source = excluded.source,
    catalog_pos = excluded.catalog_pos,
    -- GUARDED: monotonic. A worse estimate never overwrites a better one
    published = CASE WHEN <newRank> > <oldRank>
                     THEN excluded.published ELSE feed_items.published END,
    date_precision = CASE WHEN <newRank> > <oldRank>
                     THEN excluded.date_precision ELSE feed_items.date_precision END,
    title = CASE WHEN excluded.title <> '' AND excluded.title <> 'Unknown Title'
                 THEN excluded.title ELSE feed_items.title END
RETURNING first_seen;

-- <newRank>/<oldRank>:
--   CASE {excluded.|feed_items.}date_precision
--     WHEN 'started' THEN 5 WHEN 'exact' THEN 4 WHEN 'day' THEN 3
--     WHEN 'coarse' THEN 2 ELSE 1 END
```

Three rules, each load-bearing:

- **The guard is per-column `CASE`, not a statement-level `WHERE`.** A `WHERE` gates the
  whole `DO UPDATE`, dragging `source` along with the date rule — and a stale `source`
  picks the wrong probe, which does not fail, it lies (§9).
- **`status` is not in the `DO UPDATE` at all, and the INSERT always writes
  `'unknown'`.** A listing supplies a *date*, never a classification. Probe-derived
  status must never be demoted by a later listing, and a listing must never write a
  terminal status.
- **`ELSE 1` ranks an unrecognised precision as `assumed`** — the safe direction (a typo
  loses to everything), but a misspelt rung then fails silently. Pin the five values in
  a test.

**One guard, not two.** The probe's status/date write goes through this same statement.
A second copy of the ladder in Go is two statements of one rule, which is two rules.

---

## 7. The monitor cycle

Per channel, per cycle.

```
1. FETCH   RSS and /membership, independently. Either may fail; neither is fatal.

2. STORE   Upsert every item seen (§6). Collect the video IDs whose RETURNING
           first_seen == this cycle's instant — those are NEW; the rest are backlog.
           RSS      → published = <published>, precision 'exact'
           listings → published = now - itemAge(item), precision 'coarse'
                      (or 'assumed'/now when itemAge returns 0)
           status   → always 'unknown'. Probes classify; listings do not.

3. WALK    The serial probe pass over scope (§8).

4. ARCHIVE Re-read scope — step 3 has corrected dates, so rows may have entered or
           left it — and decide per item (§10).

5. On RSS success: channel_state.last_rss_ok_at = now.
```

**Step 5 must run between steps 1 and 2, not last.** As the final step it opens the
established gate a full cycle late: on a fresh install's first successful cycle, step 4
reads a gate that step 5 has not yet set, and past content waits a cycle for no reason.

**`checkChannel`'s zero-candidate early return must go.** `feed.go:457-459` reads
`if len(candidates) == 0 { return rssErr }`. That is correct when the response *is* the
work list and fatal when the store is: a cycle whose fetches return nothing must still
run steps 3 and 4, because the store has rows and a live stream may be in one. It is
the original bug's sibling, and it is easy to leave behind because the function is being
rewritten around it rather than deleted.

**The budget is 60 seconds, not 5 minutes.** `feedProcessTimeout = 60s`
(`feed.go:22-23`) bounds the per-channel process at `feed.go:469`. The walk is serial and
Q1 has no `LIMIT`, so a wide window can exhaust it and silently truncate. Steps 3 and 4
take **separate contexts** — a truncated walk must not also kill archival — and the
timeout scales with the window rather than staying a 60s constant, since
`archive_window_days` validates to 3650.

### Scope

Two queries, unioned in Go and deduped by `video_id`, merged into Q1's order.

```sql
-- Q1: the window
SELECT video_id, title, published, date_precision, status, source, catalog_pos
  FROM feed_items
 WHERE channel_id = ? AND published >= ?
   -- AND source <> 'membership'   ← iff NOT MembershipDiscoveryEnabled()
 ORDER BY published DESC, catalog_pos ASC, video_id ASC;

-- Q2: upcoming/live. UNCONDITIONAL — no date term
SELECT ...same columns...
  FROM feed_items
 WHERE channel_id = ? AND status IN ('upcoming','live')
   -- AND source <> 'membership'   ← same read arm
-- no ORDER BY: a handful of rows, merged in Go
```

**Q2 is not optional, and its absence loses streams.** `published` is frozen at insert,
so an `upcoming` row's date is *when we first saw it*, not when it airs. RSS
`<published>` is the **announcement** time. A stream announced 5 days before it airs is
already outside a 3-day window on the cycle it is discovered — so without Q2 it is never
probed, never becomes `live`, and is missed at every stage. Q2 is what delivers "never
miss a stream".

**Why not one query with an `OR`.** Measured on `modernc.org/sqlite`, 6,000 rows,
4 channels, after `ANALYZE`:

```
WHERE channel_id=? AND (published >= ? OR status IN ('upcoming','live'))

  with no status index:   SEARCH feed_items USING INDEX idx_feed_items_window (channel_id=?)
                          ^^ the published>? term is GONE — a full scan of every row
                             the channel has, every cycle, forever.
  with a status index:    MULTI-INDEX OR + USE TEMP B-TREE FOR ORDER BY

Two queries:
  Q1  SEARCH feed_items USING INDEX idx_feed_items_window (channel_id=? AND published>?)
  Q2  SEARCH feed_items USING INDEX idx_feed_items_status (channel_id=? AND status=?)
  no SCAN, no temp b-tree.
```

The cost is one extra round trip to a local file returning ~3 rows.

**The read arm is the operator's toggle only.** `AND source <> 'membership'` iff
`MembershipDiscoveryEnabled()` is false — **never** `membershipActive()`, which folds in
live cookie state (`feed.go:517-523` → `monitor_callbacks.go:218-224`). Cookies lapse for
reasons nobody chose; gating scope on them means a cookie rotation changes what is in
scope, which is this document's opening bug through a new door.

---

## 8. The walk

Scope admits the whole straddling bucket (§12), and probing all of it is unnecessary:
**within one source the listing is recency order**, so once a probe places an item of
source `S` outside the window, every later item of `S` is older by construction.

```
candidates = Q1 ∪ Q2, in Q1's order
exhausted  = {}          -- per PASS, in memory, never persisted
lastDate   = {}          -- per source

for row in candidates:                  -- SERIAL. Concurrent probes are already in
                                        -- flight past the boundary before it is known.
    if row.source in exhausted AND row.status NOT IN (upcoming, live):
        skip
        -- The status exemption is not optional: Q2 puts upcoming/live here
        -- unconditionally, and their published is an insert instant, not a listing
        -- position — "everything behind it is older" says nothing about them.

    apply the probe gates below, in order
    outcome, date := probe(row)

    if outcome == probed AND date supplied:
        if lastDate[src] IS SET and date is NEWER than lastDate[src]:
            -- the ordering assumption is false for this source
            disable early exit for src this cycle; log
            -- The IS SET guard is load-bearing: the zero value is older than every
            -- real date, so without it this fires on the FIRST probe of every source,
            -- every cycle, and early exit never runs. It would fail silently, because
            -- the fallback is just "probe more".
        lastDate[src] = date
        if date OUTSIDE the window
           AND src is date-ordered
           AND row.date_precision == 'coarse':
            exhausted += src
            -- 'coarse' means the stored date came from a LISTING. Exhaustion is an
            -- inference from listing order, so it may only be drawn from a row whose
            -- position came from a listing. An 'assumed' row's published=now is a
            -- claim of ignorance, not a coordinate.

    -- errored / denied / cooldown / probed-with-no-date: DO NOT exhaust.
    -- The boundary was not learned. Retiring on these truncates a source on a
    -- transient fault.

stop when every source is exhausted, or the list ends.
```

**`src` is the source the row carried when the walk read it.** A `members_only` refusal
can relabel a row mid-walk (§9); the relabel takes effect next cycle. Otherwise a row
could exhaust a source it was not in when the list was built.

**`rss` is never date-ordered.** The store orders `rss` rows by `<published>` — the
announcement time — while the probe returns the broadcast start. For a scheduled stream
those legitimately disagree, without bound. So `rss` never exhausts and never runs the
ordering check. It needs neither: its rows are `exact` already, so there is no coarse
bucket to bound.

### Probe gates

```
per row, in this order:
  skip if HasActiveJob                          -- the worker owns it
  skip if NOT term-match                        -- terms gate jobbing; an unjobbable
                                                   item's status is not worth a request
  skip if source='membership' AND NOT membershipActive()
  then by status:
    unknown / upcoming / live  → probe
    vod / not_a_stream         → do not probe, EXCEPT:
        status='vod' AND date_precision='started' AND no job row exists
        → probe (the restart case, below)
  probe choice: source='membership' ⇒ authenticated, else anonymous
```

This is the order `processCandidate` uses today (`feed.go:704-725`) and it must stay:
probing before term matching would pay `/player` fetches for content that can never be
jobbed on a channel using `terms`.

### The restart case

A stream can restart on the same video ID — `feed.go:702-703` says so ("not if merely
finished — a stream may restart on the same URL").

**Where it does not matter:** `AddJob` is `INSERT OR IGNORE` on the video ID
(`database_jobs.go:13-33`) and `monitor_callbacks.go:252-258` returns early on
`added == false`. A video that already has a job row **cannot be re-jobbed** whatever the
probe says. So probing it would be waste.

**Where it does:** with `include_non_live_content = false` — the default — a stream that
ended before we saw it live is probed once as `unknown`, becomes `vod` + `started`, and
is never jobbed. No job row exists. Today it stays in RSS's window and is re-probed every
cycle, so a restart is caught. Store-driven, `vod` is terminal and nothing looks again.

Hence the carve-out: `vod` + `started` + **no job row**.

- `started` means a probe read a real `liveBroadcastDetails.startTimestamp` — this *was*
  a broadcast. A plain upload cannot restart.
- **"No job row" is `NOT EXISTS (SELECT 1 FROM jobs WHERE id = ?)`, not `HasProcessed`.**
  `HasProcessed` reads the **history** table (`database_extras.go:10-22`), and history
  rows exist with no job row — DECAPI give-up (`decapi.go:583-594`) and every
  pre-existing row. `pruneHistory` is a global 10k FIFO, so the two are uncorrelated.
  Using `HasProcessed` would skip exactly the rows this carve-out exists for. The dedup
  key is the jobs table, so the predicate reads the jobs table.

Cost: ~3 probes/channel/cycle at the default; **~0** at `include_non_live_content = true`,
where those rows have job rows.

**Accepted residual:** a stream only ever listed by a coarse source, never probed while
`unknown`, never reaches `started`, so its restart is not caught. Widening the carve-out
to `coarse` would probe every past upload on the channel.

---

## 9. Probes

### Which probe

`source = 'membership'` selects the authenticated probe. Today this rides on an
in-memory field — `membershipCandidates` sets `authProbe: true` (`feed.go:681`), read at
`feed.go:748` — which cannot survive store-driven passes: a row read back from
`feed_items` has no `authProbe`.

Members content probed without cookies gets no formats and the classifier misfires it as
`upcoming` (`feed.go:743-746`). That is the 2.7.2 bug, and it is why `source` must be
current.

### Outcomes

`ProcessYouTubeVideo` (`utils.go:244-355`) is shared with DECAPI (`decapi.go:583`), so no
`feed_items` write may be hooked inside it. It splits:

| | used by | does |
|---|---|---|
| `probeAndClassify` | the feed monitor | probe; record cooldown; update `MetadataTracker`; return outcome + status + title + publish date. **No history writes, no job verdict** |
| `ProcessYouTubeVideo` | DECAPI only | `probeAndClassify` + today's `nonLiveSkipReason`/`AddToHistory`/`ShouldProcess` logic, byte-identical |

The split is required, not cosmetic: `ProcessYouTubeVideo` has side effects the feed path
must not trigger — `AddToHistory` at `utils.go:284` (give-up), `:313` (`not_a_stream`
skipped), `:330` (`vod` skipped) — and `nonLiveSkipReason(includeNonLive=false, _)`
**always** skips (`utils.go:229-231`), so on a default-config channel those fire for every
plain upload.

Feed-path outcomes:

| outcome | means | store effect |
|---|---|---|
| `probed` | a probe ran, returned metadata, and is not `denied` | write status/date/title. **FRESH** |
| `denied` | see the predicate below | nothing. Not fresh. Retry next cycle |
| `errored` | a probe ran and failed (`utils.go:269-294`) | nothing. Not fresh |
| `cooldown` | `ProbeCooldown` suppressed it (`utils.go:258-262`) | nothing. Not fresh |

**`StreamStatus` is meaningful iff `outcome == probed`.** `meta` is assigned at
`utils.go:268`, so the cooldown and passthrough returns precede it and the error return
holds its zero value — reading `StreamStatus` on those yields `""`.

**`probeAndClassify` requires a wired probe.** `ProcessYouTubeVideo` has a fifth outcome:
`p.ProbeVideo == nil` returns `ShouldProcess: **true**` at `utils.go:248-250`, passing the
item through un-probed. So "not `ShouldProcess`" is not a proxy for "not fresh". The feed
path has no such mode — production always wires it (`monitor_callbacks.go:180`) and the
only nil caller is a test.

**FRESH is `outcome == probed`. It is NOT `ShouldProcess == true`.** A probe that runs and
**succeeds** returns `ShouldProcess = false` whenever `nonLiveSkipReason` skips —
`utils.go:315` (`not_a_stream`) and `:332` (`vod`). Deriving the store write from
`ShouldProcess` therefore strands every plain upload at `unknown` **forever** on the
default config: the probe succeeded, so no failure path engages, and the row is re-probed
at full rate for the life of the install. Two predicates, not one:

| | value |
|---|---|
| store write | `outcome == probed` — record metadata whenever it came back, regardless of jobbability |
| job | `outcome == probed` **plus** the archival rules |

### The `denied` predicate

**Distrust a probe result only when YouTube said it refused us AND the classifier was
guessing.**

```
denied  ⇔  StreamStatus == 'upcoming'
           AND PlayabilityError IN ('members_only', 'login_required')
```

**This is stated once. Every use site says what `denied` does and refers here for what it
is.** `parsePlayabilityStatus` (`player_api_parsing.go:332-388`) already decodes it.

| Situation | Status | PlayabilityError | Trusted? |
|---|---|---|---|
| Members-only, no cookies | `upcoming` | `members_only` (`:357-359`) | **denied** |
| Login required, not member-specific | `upcoming` | `login_required` (`:364`) | **denied** |
| Genuine upcoming (`LIVE_STREAM_OFFLINE`) | `upcoming` | `ok` (`:350-351`) | trusted |
| Premiere via `videoDetails`/`liveStreamability` | `upcoming` | often not `ok` | trusted |
| Status block absent / unmatched reason / unknown code | any | `unknown` (`:333-335`, `:378`, `:385`, `:386-388`) | trusted |
| Age-restricted VOD that returned formats | `vod` | `age_restricted` | **trusted** |
| Members-only that returned formats | `vod` | `members_only` | **trusted** |

**Both conjuncts are load-bearing, and the tempting broader rules are wrong:**

- **"Trust only `ok`" discards genuine premieres.** `classifyStream` reaches
  `StreamUpcoming` through five guards (`:429`, `:432`, `:439`, `:448`, `:454`), and
  `:429` early-returns on `isUpcomingPlayability`. So the four after it are reachable
  **only when playability did not say upcoming** — they exist precisely because the
  playability signal was insufficient. `player_api_parsing.go:414-419` documents it in the
  client the anonymous probe uses: *"Some probes (notably ANDROID_VR on unpublished
  premieres) return this without a full microformat or `videoDetails.isUpcoming`."*
- **"Any non-`ok` is denied" refuses content we can download.** An age-restricted VOD that
  returns formats classifies `vod`, and the worker recovers it via `web_embedded`
  (`player_api_strategy.go:150-160`).
- **`unknown` is not a refusal.** It is returned when the status block is absent, on an
  unmatched reason, and from `default:` — **any status code YouTube adds**. Treating it as
  a refusal means one new code silently denies every affected stream forever.

The codebase already agrees: `isTerminalPlayability`
(`stream_processor_youtube.go:27-35`) classifies `members_only`/`login_required`/
`age_restricted` as **non-terminal**. Non-`ok` is a routing signal, not a verdict. The
probe is single-client (ANDROID_VR only), so promoting its playability to a verdict claims
more than it knows.

**The `upcoming` conjunct is a metadata-presence test in disguise**, which is what makes
`login_required` safe to include. All five `StreamUpcoming` guards require live metadata:

| Guard | Condition | Implies |
|---|---|---|
| `:429` | `isUpcomingPlayability` | forces `ok` — `denied` cannot fire here |
| `:432` | `isUpcomingPremiere` | `lbd != nil` |
| `:439` | `isUpcomingVD && !hasFormats` | `videoDetails.isUpcoming` present |
| `:448` | `hasLiveStreamability && !hasFormats` | `liveStreamability` present |
| `:454` | `!hasFormats && (lbd != nil \|\| isLiveContent)` | `lbd` or `isLiveContent` |

So a refusal that still carries live metadata is a content-level refusal on known-live
content. A refusal carrying none — anti-bot `LOGIN_REQUIRED`, "Sign in to confirm you're
not a bot", no `videoDetails` — falls to `:451` ⇒ `not_a_stream`, and the rule never fires.

**Which way to be wrong:** too broad silently discards real streams; too narrow leaves a
phantom `upcoming` job that is visible and loses nothing. Be minimal.

**Known residual:** a members refusal phrased as `UNPLAYABLE` with none of the matched
keywords yields `unknown` ⇒ trusted ⇒ the lie survives. That is the price of keeping
`unknown` trusted.

**Follow-up, filed separately.** `denied` is a compensating control for a producer bug:
`player_api_parsing.go:454` infers `upcoming` from the *absence* of formats. yt-dlp never
does — `_video.py:3858-3870` reads `videoDetails.isUpcoming` and falls back to `was_live`.
Under the standing "match yt-dlp for extraction" rule, `:454` is the real defect. Not fixed
here: `classifyStream` is on the download path and every strategy branches on
`StreamStatus`. If `:454` is ever fixed, `denied` becomes redundant rather than wrong.

### Escalation

A **`members_only`** refusal — from *any* probe — sets `source := 'membership'`.

```
probe returns members_only
  ⇒ source := 'membership'          -- unconditional. The refusal proves membership
  ⇒ if the probe was anonymous AND membershipActive():
       re-probe with cookies, same cycle, BYPASSING the cooldown gate
  ⇒ classify the final result through the SAME outcome rules as any probe
```

**The refusal is the sighting.** A public video later locked to members can never flip
`source` from a listing: the membership tab is the only listing that would mention it, and
`membershipActive()` gates that fetch. So the row would stay `source='rss'`, be probed
anonymously forever, refused forever, and never archived. YouTube stating `members_only` is
a direct answer that needs no fetch, no tab, and no toggle.

**Only `members_only`, never `login_required`.** `login_required` is the *fallback* for any
`LOGIN_REQUIRED` mentioning neither member/join nor age (`:364`) — which includes YouTube's
anti-bot refusal on **public** videos. Relabelling one would mark a public video as members
content forever, and with `membership_discovery = false` the read arm would then hide it
from scope entirely.

**The relabel is what bounds the escalation.** Because `members_only` sets `source`, the
escalation fires at most **once**: next cycle the row is `membership` and the authenticated
probe is selected directly. An escalation that cannot relabel never terminates — `source`
stays `rss`, the next cycle probes anonymously, is refused, and escalates again: 2 probes
per cycle for the duration of an IP block, permanently, doubling our request rate exactly
when YouTube is throttling us. **An escalation is only bounded if its trigger is also what
stops it recurring.**

**The cooldown must be bypassed.** A `members_only` refusal is a *successful* probe and
records the cooldown (`utils.go:299`); gating the same-cycle retry on it would suppress the
escalation entirely for any operator with `probe_cooldown > 0`.

**Setting `source` even on a failed escalation is the point:**

| Next cycle | Before | After |
|---|---|---|
| Wrong tier, cookies valid | anon + escalated auth = **2/cycle** | auth directly = **1/cycle** |
| No cookies | anon, refused = **1/cycle** | probe gate skips it = **0/cycle** |

**The toggle still governs.** With `membership_discovery = false` no escalation happens and
the row is `denied`. `source` still flips, which is what lets the read arm hide it.

### Membership gating: reads and probes use different predicates

```
READ arm (Q1, Q2):        source <> 'membership' iff NOT MembershipDiscoveryEnabled()
PROBE gate (the walk):    source <> 'membership' iff NOT membershipActive()
```

| Control | Protects | Could another cover it? |
|---|---|---|
| Read arm — the operator's toggle | **scope** | No — nothing else touches scope |
| Probe gate — config **and** cookies | **efficiency**: skip a request we know is refused | Yes — `denied` catches the result anyway |
| `denied` — playability | **correctness** | No — it is the only control that depends on nothing we stored |

**What is visible may only move on an operator's decision. Probing may be opportunistic.**
A cookie lapse therefore leaves scope exactly where it was: members rows stay in scope and
simply go un-probed for a cycle, and the stream is jobbed on the first cycle cookies return
— which is also the first cycle it could have been downloaded at all.

**Accepted residual:** a cookie lapse longer than `archive_window_days` permanently loses
members content published during it. The fetch is gated, so no rows are written; on cookie
return the content has aged out. It remains downloadable, so this is a real loss — but it
is structurally identical to an RSS outage (being blind for longer than the window loses
what aged out while blind), and the alternative — anchoring the window to first-observation
rather than publish date — is discovery-order-is-not-recency-order, which is the original
bug.

---

## 10. Archival and throughput

Step 4 re-reads scope and decides per item:

```
skip if HasActiveJob
skip if NOT term-match

upcoming / live   → job iff FRESH this cycle. Never windowed, never M-gated, and NOT
                    gated by HasProcessed.
vod / not_a_stream→ skip if HasProcessed
                    skip unless include_non_live_content   ← BEFORE probing
                    skip unless established
                    skip if source='membership' AND NOT membershipActive()
                    if already probed in step 3 → use that result (it is FRESH)
                    else → refresh-probe now, write the result back
                    then RE-CHECK THE WINDOW against the probe's date, then:
                       denied            → no job, retry next cycle   ← FIRST
                       outside window    → no job, ever
                       live / upcoming   → job (never throttled)
                       vod/not_a_stream  → job (M-gated if backlog)
                       errored/cooldown  → no job, retry next cycle
unknown           → no job. We do not know what it is. Retry next cycle.
```

**`denied` must be listed first.** It carries `StreamStatus == 'upcoming'` by definition,
so any table that omits it routes it straight to `live/upcoming → job` — the 2.7.2 misfire,
laundered.

**The window re-check is the crux.** A `coarse` row is admitted on an *upper bound* — "1
week ago" could be 13 days. The probe supplies the exact date; if it falls outside, there is
no job, and the row drops out permanently on the date just written. Skipping this re-check
archives on a guess.

**An item probed in step 3 is FRESH; step 4 must not re-probe it.** Otherwise every VOD
entering scope costs **two** probes per cycle — step 3 probes it as `unknown`, it becomes
`vod`, step 4 sees `vod` and refresh-probes it seconds later. `probe_cooldown` defaults to
0, so nothing suppresses it. The refresh probe exists to stop a job being built on *stale*
metadata; metadata from earlier in the same cycle is not stale.

**`HasProcessed` gates ONLY the `vod`/`not_a_stream` branch.** Today it never touches
live/upcoming: `feed.go:730` reads it and `:762` passes it as `IsReprobe`, which is consulted
only in `nonLiveSkipReason` (`utils.go:228-236`). This matters because **history rows exist
with no job row** — DECAPI give-up and every pre-existing row. A `HasProcessed` gate on the
live branch would skip such a row the moment its probe returned `live`, and the stream is
gone forever.

### The scheduler

Job creation, by source:

| Creator | Status at creation | Why |
|---|---|---|
| feed monitor, backlog VOD | `Queued`, `queue_priority = 1` | the only thing M paces |
| feed monitor, new VOD | `Queued`, `queue_priority = 0` | ordered ahead of backlog, never M-gated |
| feed monitor, live/upcoming | `Upcoming` (admitted) | never throttled |
| DECAPI | `Upcoming` (admitted) | it writes no `feed_items` row by design (§13), so a `Queued` DECAPI job would have no row to JOIN for `published` and would strand un-ordered forever |
| Twitch, manual adds | `Upcoming` (admitted) | the scheduler is YouTube-only; neither has a `feed_items` row |

This means `monitor_callbacks.go` **must change**: `:242` currently creates every job as
`StatusUpcoming` unconditionally and `:261` calls `EnqueueJob` immediately. A reader told
"job creation is unchanged" leaves this alone and the scheduler never sees a `Queued` row.

**`Queued` means un-admitted, and nothing else.** `ShouldProcess` must return **false** for
it. That is the opposite of the obvious answer and the whole structure depends on it:
`ShouldProcess` (`queue.go:350-357`) is not a classifier, it is an **enqueuer** — both
callers feed it straight to `queue.Enqueue`, at `worker.go:314` (startup recovery) and
`worker.go:349` (the 60s heartbeat). Returning true would sweep the entire backlog into
`JobQueue` within a minute and past 100 into the silent pending drop (`queue.go:93-98`).

**The admission transition:**

```
admit(job):
    1. UpdateJobFields(job.ID, {"status": StatusUpcoming})    -- the DURABLE step
    2. queue.Enqueue(job.ID, StatusUpcoming)
    in that order.
```

Step 1 is what makes the admission observable. `Enqueue` touches no DB row, so without it M
counts 0 forever and admits on every tick. `Upcoming` needs no invention — it is already what
`monitor_callbacks.go:242` writes for "created, awaiting processing", and `ShouldProcess`
accepts it, so a crash between the steps is self-healing via `recoverJobs`
(`worker.go:295-318`).

```
Queued → Upcoming → (Live | Downloading) → Muxing → Finished
   ^        ^
   |        admitted; JobQueue owns it
   un-admitted; the scheduler owns it
```

**The M query:**

```sql
SELECT COUNT(*) FROM jobs
 WHERE channel_id = ?
   AND queue_priority = 1
   AND status IN ('Upcoming','Live','Downloading','Muxing');   -- ALLOW-list
```

**It must be an allow-list.** A `NOT IN` formulation has to enumerate the resting states,
and `COOKIES?` (`types.go:15`) is in neither the terminal set (`IsTerminal()` =
Finished/Error/Cancelled, `types.go:92-94`) nor `Queued` — so it would hold a slot
**forever**, and a channel whose cookies lapse would silently lose its throughput with no
error anywhere. `COOKIES?` is a resting state awaiting the operator, like `Queued`.

**Admission order:** `ORDER BY queue_priority ASC, published DESC`, where `published` comes
from an INNER JOIN on `feed_items(channel_id, video_id)` — the PK, so it is indexed, and the
date stays in one place. The join is guaranteed to hit because only the feed monitor creates
`Queued` rows. The scheduler filters `status = 'Queued' AND channel_id IS NOT NULL`.

**The scheduler gates M, and only M.** `AcquireDownloadSlot` stays the pool's arbiter. It
cannot gate the pool: `result.IsVod` does not exist until `streamProc.Process` returns
(`worker.go:409`) — *after* admission; Twitch VODs take pool slots through the same
`worker.go:446` and are invisible to a YouTube-only scheduler; and `ActiveCount()`
(`queue.go:303-307`) reads `activeDownloads`, incremented *inside* `AcquireDownloadSlot`, so
between admission and acquisition it reads 0.

**Count-then-admit holds a per-channel lock.** The scheduler is woken by job completion *and*
by a heartbeat; otherwise two wakeups read M−1 and both admit.

**It is a goroutine and owns the only path out of `Queued`** — if it dies the backlog strands
silently with no error anywhere. Wrap it in the restart-on-panic pattern `pollForJobs`
(`worker.go:322-360`) already uses.

**`JobStats.ActiveCount`** (`database_jobs.go:622-645`) is `SUM(status IN
('Downloading','Live'))`. `Queued` is **not** active — folding it in makes the dashboard claim
downloads that are not running.

### `num_parallel_downloads`: VOD-only, 2 → 10

**The bug this fixes is live today.** `AcquireDownloadSlot` (`queue.go:149-180`) is called for
**every** job at `worker.go:446` — live included — with no timeout and no priority; waiters
wake in Go runtime order. Two long streams fill the default of 2, and a third channel going
live blocks **indefinitely**. That is a missed stream on a stock config.

So live and upcoming are exempt from the pool, and therefore unbounded — because they must be.
A broadcast cannot be throttled.

**The exemption predicate is `result.IsVod`, not the job status.** They disagree:
`stream_processor.go:229-245` writes `Downloading` + `IsVod: true` for `not_a_stream` +
`AllowNonStream`. The pool means "how many VOD downloads at once", so it gates on what a VOD
download is.

**The default lives in two places.** `config.go:67` (`Defaults()`) **and** `queue.go:56-57`
(`if maxDownloads <= 0 { maxDownloads = 2 }`), an independent hardcoded fallback no config
value reaches. Miss it and they disagree for every test.

There is **no maximum** to raise — validation only rejects `< 1` (`config.go:559-560`). The
key is commented out in `config.example.toml:104`, so this reaches every install that never
set it. **Peak concurrency becomes `(live streams) + 10`** where it was a hard 2. The help
text must say so.

---

## 11. Backfill

Populates the list once per channel, from `/videos` + `/streams` + `/membership` combined and
deduped, ordered by recency, **to the window depth**.

**Depth: page each tab until an item's date is older than `archive_window_days`, then stop.**
Everything behind it is older still — the same source-ordering fact the walk rests on — so it
is outside the window, and a row outside the window is never probed, never archived, never
read. At the 3-day default that is ~1 page per tab: **~3 requests per channel**.

Coarse dates make this sound without a margin: `now - itemAge()` is the *newest* instant
consistent with the text, so an item whose coarse date is already outside is outside on any
reading.

**Second stop arm: a full page with no datable item.** An undatable item gets `published = now`
— *inside* every window — so the date arm never fires on a tab of them, and under a
`relativeAgeRe` break that is every item on every channel. This arm is a parser-failure
detector, not a completeness rule: log it, stop the tab, and leave `backfilled_at` NULL so the
scan retries when the parser is fixed.

**Classification:**

```
age := itemAge(item)     -- live-badge short-circuit FIRST, then the age regex
age > 0  ⇒ date_precision='coarse', published = now - age
age == 0 ⇒ date_precision='assumed', published = now
status   ⇒ always 'unknown'
```

**Call `itemAge`; do not re-implement half of it.** It checks the live badge first and returns
`0` (`channel_membership.go:226-231`) — *"Currently live → 'now', regardless of any 'streaming
for N' elapsed text"*. `relativeAgeRe` (`:49`) is matched against the serialized JSON of the
whole item, so a live renderer's `"Started streaming 2 hours ago"` matches it. Without the
badge check, a live stream on `/streams` gets dated two hours old.

**`catalog_pos` must be channel-global**, not per-tab: `/videos` and `/streams` overlap
heavily, a past stream appears in both at unrelated positions, and the row is deduped by
`(channel_id, video_id)` — so a per-tab value would be permanently whichever tab scanned
first.

```
1. scan each tab page by page
     └─ write each page's rows IMMEDIATELY, catalog_pos = provisional per-tab index
     └─ advance the per-tab cursor in channel_state.backfill_state
     └─ stop per the two arms above
2. when all eligible tabs are done, ONE ordering pass:
     └─ SELECT the channel's rows into a slice, CLOSE the cursor
     └─ sort by (published DESC, provisional pos ASC, video_id ASC)
     └─ UPDATE each row's catalog_pos = 0..n-1   (unguarded — the precision guard
        governs the upsert only, and would reject a coarse write onto an exact RSS row)
3. set backfilled_at AND backfilled_window_days = <resolved archive_window_days>
```

**Write per page, do not buffer.** Scan-everything-then-merge is incompatible with "resumable
via cursor": a restart would destroy the buffer and the cursor would resume into a merge whose
earlier half no longer exists.

**The ordering pass must collect-then-update.** With `SetMaxOpenConns(1)` (`database.go:177`),
issuing UPDATEs while a SELECT cursor is open deadlocks on the single connection — the hazard
documented at `migrations.go:242-244`.

### Trigger

There is no config watcher. `internal/config/store.go` exposes `Read`/`Snapshot`/`Update`/
`SaveLocked`; "hot-reload" means callbacks fired by the writer. `kickMonitors`
(`services.go:568-577`) is the funnel for channel mutations — and it is a bare `func()` with no
add/remove/reorder discrimination, so the sweep must be idempotent rather than event-driven.

```
sweep condition:
    backfilled_at IS NULL
 OR backfilled_window_days IS NULL          -- NULL < 30 is NULL, not true. Without
 OR backfilled_window_days < <resolved>     -- this arm the widen check never fires.

evaluated EVERY MONITOR CYCLE, plus startup, kickMonitors, and a manual re-run.
```

**Every cycle, because no hook fires on a settings change.** `config_routes.go:743-744` fires
the channel callback only under `if _, hasChannels := updates["channels"]`, so a settings PUT
that changes only the window fires nothing — and a hand-edited `config.toml` fires nothing
ever. An operator would set 3 → 30, watch scope immediately admit stored rows, and never learn
the catalogue behind RSS was never scanned. **Only the trigger runs in the cycle**; the scan
runs on its own throttled path, never through the monitor's per-video retry/backoff loop.

**The sweep reads the config's channel list and LEFT JOINs `channel_state`.** A missing row
reads as "never backfilled", not "no rows returned" — otherwise a newly-added channel matches
no `WHERE` and is never backfilled and never establishes.

**A widen mid-scan cancels the scan and restarts it deeper, resetting the per-tab cursor.** A
deeper rescan resuming from a shallow cursor skips exactly the pages it was restarted to
fetch. Narrowing does neither: a running deeper scan records a deeper depth, which satisfies
every narrower window it will be asked for.

**The in-flight set** (channel ID → cancel func), owned by the backfill worker, is the
scan-dedup condition: `kickMonitors` fires on every add, remove, reorder, bulk PUT and TUI
save, so without it a user reordering three times launches three concurrent scans of one
channel. **A scan that ends for any reason — success, failure, cancellation — must remove
itself**, or the channel is never retried for the life of the process.

### Operational rules

- **YouTube channels only.** Re-apply `ch.GetPlatform() == "youtube"`. Note this is *stricter*
  than the precedent: `getYouTubeChannels` (`feed.go:809-823`) excludes with
  `if ch.Platform == "twitch" { continue }`, so a future third platform falls through it. The
  backfill must allow-list. Without it a Twitch channel is scanned as
  `youtube.com/channel/<twitch_login>/videos`, 404s on all tabs, never sets `backfilled_at`,
  and retries forever.
- **The membership tab is gated on `MembershipDiscoveryEnabled() && HasAuthCookies()`.**
  `/videos` and `/streams` are public and ungated. If the backfill wrote members rows on a
  channel with discovery off, they would enter scope and be jobbed — the toggle defeated by the
  indirection that fixes the original bug.
- **`backfilled_at` is set on completion of the *eligible* tabs**, not all three — otherwise a
  `membership_discovery = false` channel could never complete. Turning membership discovery
  **on** must therefore clear `backfilled_at`: the eligible set changed.
- **Throttled: one page per second, globally.** A constant, not config.
- **Strictly serial across channels.** On upgrade day every channel has `backfilled_at IS
  NULL` and the sweep fires for all of them. At the default that is ~3 requests each and the
  queue costs nothing; it earns its place at a wide window, where the scan is deep again.
- Resumable via the per-tab cursor. Inline `defer recover()` in the scan goroutine.
- Progress via `hub.Broadcast` (`websocket.go:441`) and a TUI `tea.Msg` channel — **including
  the `InitialState` seed** (`ws_wiring.go:87-111`), which today returns only jobs, logs, the
  three next-check times, connectivity and `hideFinishedAgeDays`. A long scan makes mid-flight
  connect the common case.

### The `/browse` client

No continuation paging exists on any channel path today — the only `continuationItemRenderer`
under `internal/youtube/` is a test fixture (`channel_membership_test.go:27`). But paging is not
new: `internal/chat` does full InnerTube continuation paging in production against the same API
— `api.go:172-178`, the loop at `downloader.go:412-423`, stale-token recovery at `:558-583`.
Model the transport on that; use yt-dlp's `_tab.py` only as the reference for the browse
response shape (`seen_continuations` loop detection at `:585-590`, `visitorData` re-extraction
at `:608`, `appendContinuationItemsAction` unwrapping at `:628-631`, token at `_base.py:1041`).

Reusable as-is: `extractYtInitialData` (`channel_membership.go:295`), `ytInitialTabs` (`:114`),
`walkVideoRenderers` (`:176`), `lockupTitle` (`:261`), `rendererTitle` (`:267`).
Membership-specific and needing parameterisation: `TAB_ID_SPONSORSHIPS` (`:22`) and its match
(`:150`), the `/membership` URL literal (`:75`), the `HasAuthCookies()` early return (`:65`).

### Channel removal

The same sweep prunes departing channels. Precedent: `PruneHealth` (`health.go:110-112`) exists
so per-channel state "can't grow unbounded on a 24/7 process". This is the **first
channel-keyed DB cleanup** in the codebase — nothing in `internal/database/` deletes by channel
today.

- **Cancel any in-flight scan first, wait for it to observe cancellation, then prune.**
  Otherwise the prune deletes rows the scan is still writing, resurrecting a channel that no
  longer exists with a NULL `backfilled_at` no sweep will revisit. The worker re-checks the
  active set before each page write, so a cancellation landing between check and write costs one
  stale page, which the prune then removes.
- **Delete `feed_items` and `channel_state`.**
- **Delete `Queued`, `Upcoming` and `COOKIES?` jobs — and their `history` rows.** They never
  started. `AddToHistory` fires at job *creation* (`monitor_callbacks.go:260`), so deleting the
  job alone leaves a history row with no job row: an orphan, and a permanent archival block on
  the rename-and-re-add path (re-add ⇒ `HasProcessed` true ⇒ the `vod` branch skips it ⇒ never
  archived, silently). `Upcoming` matters too: `ShouldProcess` accepts it, so `pollForJobs`
  re-enqueues it every 60s forever, polling YouTube for a deleted channel.
- **Leave `Live`/`Downloading`/`Muxing` running.** Killing a download mid-write to honour a
  config edit is worse than finishing it, and removal is sometimes a rename-and-re-add.
  `Finished`/`Error`/`Cancelled` are terminal records and stay.

Ordering: `kickMonitors` fires *after* the config is written (`channel_routes.go:121-123`), so
the active set is authoritative when the sweep reads it.

### The established gate

**Past content is not archived until `last_rss_ok_at IS NOT NULL OR backfilled_at IS NOT
NULL`.** Upcoming/live always pass. A fresh install whose first cycle 404s would otherwise hold
only membership items and treat them as the whole channel.

A missing `channel_state` row is **not** established.

**Both keys must exist at ship — this is why the two parts do not ship apart.** Only the
backfill sets `backfilled_at`. Without it, a channel whose RSS is *permanently* broken — a dead
channel ID, or a members-only channel whose public feed legitimately carries nothing — never
establishes and never archives past content, indefinitely and silently. Live streams still
work, which is what makes the silence hard to notice.

**One residual:** an RSS fetch returning *zero* entries also sets `last_rss_ok_at`. For a
members-only channel the store then holds only membership items and all of them are in scope.
That is correct — it looks like the original bug and is not.

---

## 12. Dates

### The ladder

```
assumed  <  coarse  <  day  <  exact  <  started
```

| Rung | Source | Meaning |
|---|---|---|
| `assumed` | listing, `itemAge` returned 0 | `now`. A claim of ignorance, not a date |
| `coarse` | listing, `itemAge > 0` | `now - age`. **This date or older** |
| `day` | probe: microformat `uploadDate`/`publishDate` | date-only |
| `exact` | RSS `<published>` | second-granular. For a stream this is the **announcement** |
| `started` | probe: `liveBroadcastDetails.startTimestamp` | the broadcast's actual start |

**`started` outranks `exact` because authority is not precision.** Both are
second-granular. But RSS's `<published>` for a stream is when it was *announced*, and the
window asks when it *aired* — potentially days apart. On a four-rung ladder both are `exact`,
the guard's `>` test fails, and **the probe's real start can never overwrite the announcement**:
the window would cut on the announcement forever, and the refresh probe's re-check would reject
the row every cycle without ever changing it.

`day` still loses to `exact`, which is correct: a plain upload's `uploadDate` must not overwrite
RSS's second-granular date for the same upload. Only `started` outranks `exact`, and only a
probe produces it.

### Coarse dates skew new

`itemAge` truncates — `"1 week ago"` displays for anything 7 to 13 days old — and returns
`n * unit` (`channel_membership.go:252`), the **lower bound** of the true age. So
`now - itemAge()` is the **newest instant consistent with the text**. That is what the window
wants, and it needs no new code.

**Why new is the safe direction:** under a date cut, a too-old guess **silently excludes** — no
probe, no job, no trace — while a too-new guess admits the item to a probe that corrects it.
Exclusion is the only unrecoverable outcome, because an excluded row is never probed and so
never gets the date that would have included it. **Nothing is ever excluded on a date we have
not verified.**

**Do not change `itemAge`.** The opposite rule (skew old) needs `now - Age - unit`, and the unit
is not recoverable from the return value — `504h` is either "3 weeks" or "21 days". Skew-new
wants the truncated lower bound the function already returns.

**A bucket is admitted iff its lower bound is strictly inside the window.** `published` is frozen
at insert and compared against a *later* `now`, so a bucket whose lower bound equals the window
is already outside.

Worked example, `archive_window_days = 10`:

| Displayed | True age | vs. a 10-day window |
|---|---|---|
| `6 days ago` | [6d, 7d) | entirely inside → probe → job |
| `1 week ago` | **[7d, 14d)** | **straddles**: admitted (7d < 10d), probed, jobbed only if truly ≤ 10d |
| `2 weeks ago` | [14d, 21d) | entirely outside → never probed |

**The 3-day default has no straddling bucket at all.** YouTube's sub-week buckets are
day-granular, so a 3-day window admits `2 days ago` ([2d, 3d), entirely inside) and excludes
`3 days ago` ([3d, 4d), entirely outside). The same alignment holds at 7. Straddling costs appear
only at windows falling inside a bucket — 10 days, or anything past a month.

### The probe's publish date

**This data does not exist in the chain today** and adding it is the one cross-package refactor:

- `utils.go:32-36` — `VideoProbeResult{StreamStatus, Title, ChannelName}`
- `types.go:32-51` — `VideoInfo` carries `ScheduledStartTime` and `PlayabilityError` (`:49`)
- `monitor_callbacks.go:174-178`, `:193-197` — both wiring sites copy exactly three fields

Add `VideoInfo.PublishedAt` + `VideoProbeResult.PublishedAt` + both wiring sites, and
`VideoProbeResult.PlayabilityError` (which `VideoInfo` already has but `VideoProbeResult` drops
— without it the monitor cannot tell an observation from a refusal, and `denied` is impossible).

**Do not reuse `ScheduledStartTime`.** `extractScheduledStartTime`
(`player_api_parsing.go:113-138`) is a conflated accessor: `startTimestamp`, else a
`liveStreamability` epoch, else microformat `uploadDate` (`:129-135`). So it holds a genuine
publish date for an upload and a **future** timestamp for an upcoming stream. An implementer
looking for "the probe's publish date" will find it, and it will look right.

```
status vod / post_live  → liveBroadcastDetails.startTimestamp  → 'started'
                        → ELSE uploadDate / publishDate        → 'day'
status not_a_stream     → microformat uploadDate / publishDate → 'day'
status upcoming / live  → no date stored
```

The `liveStreamability` epoch is never a publish date — it is a scheduled start.

**The `vod` fallback is not optional, and `post_live` is why.** `classifyStream` makes them
mutually exclusive:

```go
isPostLiveDVR := lbd != nil && getStr(lbd, "endTimestamp") != "" && !isLiveNow  // :407
if lbd == nil && !isLiveContent && !isPremiere { return StreamNotAStream }      // :451
if !hasFormats && (lbd != nil || isLiveContent) { return StreamUpcoming }       // :454
if isPostLiveDVR { return StreamPostLive }                                      // :457
return StreamVOD                                                               // :460
```

An ended stream *with* an `endTimestamp` short-circuits to `post_live` at `:457` and never
reaches `:460`. So `StreamVOD` is reachable only via `lbd == nil && isLiveContent && hasFormats`
(no `liveBroadcastDetails` at all) or `lbd != nil && endTimestamp == ""`. A rule reading only
`startTimestamp` yields no date for the normal `vod` case. Nor is microformat guaranteed per
client (`player_api_parsing.go:414-419`).

**`post_live` normalizes to `vod` on write.** The probe returns five statuses
(`types.go:13`); the store's enum has four content states. `post_live` and `vod` answer the
store's only two questions identically — is it in the window, should it be archived — and
today's `nonLiveSkipReason` is reached from a single `case "post_live", "vod":`
(`utils.go:326`). The DVR distinction is a download-strategy concern and the worker re-probes
for it anyway. The date rule above runs *before* this normalization.

**Invariant: never write a terminal status without a rankable date.** If the probe classifies a
past item but supplies no date, the row stays **`unknown`**.

Without it, a row becomes `vod` + `assumed`: `published = now`, so it sits in the window claiming
to be new for a full `archive_window_days`, while `vod` is terminal so no discovery probe
revisits it — and on `include_non_live_content = false` the refresh probe never fires either.
The archival pass would job it on a fabricated date. Keeping it `unknown` costs a probe per cycle
and buys self-healing: the moment any probe returns a date, the row takes it. A terminal status
is a promise that we know enough to stop looking.

---

## 13. DECAPI

DECAPI is **redundancy for RSS**, not a history mechanism. RSS fails ~13% of cycles; DECAPI
independently asks "what is the newest video on this channel" so a stream going live during a
404 is still caught. It cannot see members content.

It keeps its existing path — `HasActiveJob` (`decapi.go:543`), `HasProcessed` (`:565`),
`ProcessYouTubeVideo` (`:583`), `AddToHistory` (`:589`) — and **writes no `feed_items` row**.
That is why `decapi` is absent from the `source` enum.

**But it needs one date check.** `decapi.go:523-604` has none, and "the newest video on the
channel" is not the same as "recent": on a dormant channel it can be a year old. With
`include_non_live_content = true`, adding such a channel has DECAPI job a six-month-old VOD on
the first cycle — this document's opening bug through a second door.

```
DECAPI probe returns live / upcoming  → job, ALWAYS. No date check. This IS the
                                        redundancy, and a date must never block it.
DECAPI probe returns vod / not_a_stream
                                      → job only if the probe's PublishedAt is inside
                                        archive_window_days. Otherwise no job.
```

It reads the probe's own `PublishedAt` — which §12 adds anyway — so this needs no store read, no
store write, and no change to its path.

---

## 14. Twitch

The Twitch monitor is live-only and has no cap. **But Twitch is not untouched**, and any claim
otherwise is false: the download pool is shared, and Twitch jobs reach the same
`worker.go:446`. So Twitch live downloads go from **hard-capped at 2** to unbounded.

That is intended. A Twitch broadcast is exactly as unthrottleable as a YouTube one, and capping
it at 2 is the same stock-config stream-miss bug. Leaving it would mean two rules for one queue
and one platform still losing streams.

`processTwitchVod` (`stream_processor_twitch.go:78-95`) sets `IsVod: true`, so Twitch VODs *do*
take pool slots — correctly. The scheduler is YouTube-only: Twitch never produces `Queued` rows
and never gets a `queue_priority`.

---

## 15. What `history` comes to mean

Removing the feed path's hidden `AddToHistory` calls is a **population** change, not a schema
one. The table, its API, the orphan overlay and the Twitch/DECAPI writers are untouched.

Today `history` conflates three things: *we jobbed this* (`monitor_callbacks.go:260`), *we looked
and declined* (`nonLiveSkipReason` skips), and *we gave up probing* (give-up). Only the first has
a job row — which is why `ListOrphanedHistory` finds anything, and why an operator eventually
purges 80 rows and re-arms content that was never supposed to come back.

On the feed path it now means only the first: **we created a job for this video.** Feed-path
give-up no longer writes history — its only effect today is flipping the reprobe/log-level flag,
and `utils.go:272-279` says outright that history does not stop re-probing, so nothing is lost.

This composes with the window into defence in depth: purging history can no longer re-arm an
out-of-window VOD, *and* far fewer orphans are manufactured.

---

## 16. Migration (v16)

`schemaVersion = 15` (`migrations.go:26`). Follow the established pattern: a sequential
`if version < 16 { ... return db.writeUserVersion(16) }`, `CREATE TABLE/INDEX IF NOT EXISTS`,
tables also added to `createSchema`.

1. Create `feed_items`, `channel_state`, and **both** indexes.
2. `ALTER TABLE jobs ADD COLUMN channel_id TEXT;` and
   `ADD COLUMN queue_priority INTEGER NOT NULL DEFAULT 1;` — guard with the established
   `isDuplicateColumnErr` pattern (`migrations.go:234-238`). Migrations run outside a
   transaction and `user_version` is written last, so a crash mid-block re-runs the whole block.
3. Add the `Queued` job status. **Not** terminal; `ShouldProcess` returns **false** for it.
4. `DROP TABLE IF EXISTS last_videos`; remove `GetLastVideo`/`SetLastVideo`
   (`database_extras.go:126-148`) and `TestLastVideos`; make the legacy JSON importer ignore
   `lastVideos` (`database_jobs.go:723`).

   `GetLastVideo`/`SetLastVideo` have **zero non-test callers**. DECAPI — the suspected consumer
   — makes exactly three DB calls (`decapi.go:543`, `:565`, `:589`). Rows can only arrive via the
   legacy importer and nothing reads them. **Not** to be confused with `LastVideoSeq`, the
   download-resume segment counter (`orchestrator.go:270`), which is live and untouched.

No data backfill inside the migration, so the `SetMaxOpenConns(1)` cursor hazard does not apply.

**Config migration** (`migrateOldFormat()`): drop `max_feed_items` — the global
(`types.go:83`), the per-channel override (`types.go:265`), `fm.maxFeedItems()`
(`feed.go:553-565`), and the parse at `config.go:303`. `num_parallel_downloads` default 2 → 10
in **both** places.

---

## 17. Tests

`checkChannel` is not testable today and this is work to budget, not an assumption: there is no
`feed_test.go`, and `checkChannel` calls `fm.fetchFeed` directly (`feed.go:426`), which does a
real HTTP GET (`feed.go:484`). RSS failure cannot be simulated. `FetchMembership` is already an
injectable func field (`MembershipFetchFunc`, `feed.go:94`) — the RSS path needs the same.
Fixtures stay **inline**: `internal/monitor/` has no `testdata/` and every existing fixture is
inline.

**Delete** `membership_test.go:83-108` `TestMergeCandidatesRecencyCap` — it asserts the cap
behaviour this design removes.

**The headline regression:** RSS 404 cycle + membership returns a 3-week-old VOD ⇒ not archived.

*Scope*
- Q2 carries an `upcoming` row announced 5 days out past a 3-day window; it is probed, goes
  live, and is jobbed
- Query plan: Q1 uses `idx_feed_items_window`, Q2 uses `idx_feed_items_status`, neither shows
  `SCAN` or `USE TEMP B-TREE FOR ORDER BY`. Assert via `EXPLAIN QUERY PLAN` on the live
  `modernc.org/sqlite` handle after `ANALYZE`
- A cookie lapse does not move scope: members rows stay in scope, no public VOD is jobbed
- `membership_discovery = false` hides stored members rows from scope immediately; turning it
  back on restores them
- A cycle with zero fetch results still runs both passes

*Dates*
- `itemAge` unchanged: `"1 week ago"` → `168h`
- Coarse skews new: `"1 week ago"` stores `now - 7d`, admitted to a **10-day** window (not 7 —
  at 7 the bucket's lower bound equals the window and is already outside)
- A straddling item probed to 13 days against a 10-day window is **never jobbed**, its exact
  date is written back, and it is not re-probed next cycle
- `"2 weeks ago"` against a 10-day window is **never probed**
- `started` overwrites a stored `exact`; `day` does not; all five rungs ordered; an unrecognised
  precision string ranks as `assumed`
- `PublishedAt` is status-aware, including the `uploadDate` fallback when `lbd` is absent
- `post_live` normalizes to `vod`
- A probe returning `vod` with no date leaves the row `unknown`
- A scheduled start time is never stored

*The walk*
- Early exit: against a 10-day window, a source whose `"1 week ago"` rows are truly
  8d/9d/11d/12d/13d costs exactly **three** probes; the 12d and 13d rows are never probed
- Only a dated `coarse` probe retires a source: four tests (`errored`, `denied`, cooldown,
  probed-with-no-date) each leave the source live
- An `assumed` row never retires a source
- The ordering check fires on 8d → 7d, disables early exit for **that source only**, and probes
  the rest
- The check does not fire on the first probe of a source
- `rss` never retires a source and never runs the check
- Exhaustion never skips an `upcoming`/`live` row
- Probes are serial: assert call **ordering**, not count — a concurrent implementation passes a
  count assertion while issuing every probe past the boundary
- A mid-walk `source` relabel does not retire the new source this cycle

*Probes*
- A genuine upcoming (`LIVE_STREAM_OFFLINE` ⇒ `ok`) is **not** denied and is jobbed
- An age-restricted VOD that returned formats is **not** denied
- A `members_only` refusal flips `source` and escalates once; a failed escalation still flips
  `source`, so the next cycle issues **one** probe
- Anti-bot `login_required` on a public video neither relabels nor escalates: assert `source`
  stays `rss` **and** no second probe is issued
- The escalated re-probe is not suppressed by `probe_cooldown > 0`
- A `vod` + `members_only` escalated result is **trusted**
- No cookies ⇒ no escalation ⇒ next cycle zero probes
- `source='membership'` selects the authenticated probe
- A public video locked to members is archived with no membership listing ever occurring
- The restart carve-out: a `vod`+`started` row with a **history row but no job row** IS probed
  (this is the `HasProcessed` trap — arrange via a DECAPI give-up)
- A `vod`+`started` row **with** a job row is not probed
- `HasProcessed` does not block a live/upcoming job — arrange a history row with no job row
- A successful probe of a non-jobbable item still writes its status (default config, plain
  upload ⇒ `not_a_stream`, not left `unknown`)
- `probeAndClassify` requires a wired probe; freshness is never derived from `ShouldProcess`
- No hidden history writes on the feed path; DECAPI still writes history exactly as today
- A stale stored `live` whose probe errored ⇒ no job; a cooldown-skipped item ⇒ no job

*Archival and throughput*
- An item probed in step 3 is not re-probed in step 4
- A stored `vod` whose refresh probe returns `live` **is** jobbed and the store updates to `live`
- Clearing orphaned history re-jobs an in-window VOD, not an out-of-window one
- New content bypasses M: M=1 with a full backlog, a new VOD still admits
- Live/upcoming bypass M and the pool: `num_parallel_downloads=1` saturated by a VOD, a stream
  going live still downloads
- `ShouldProcess(Queued) == false` — call `recoverJobs` against a DB holding a `Queued` row and
  assert it is neither enqueued nor mutated
- Admission writes the status before enqueueing; M counts it
- A `COOKIES?` job does not hold an M slot
- 300 backlog VODs with M=3 converge and never trip the pending drop
- DECAPI/Twitch/manual jobs are never `Queued`

*Backfill*
- Stops at the window depth, identically on a first scan and a re-run
- A tab of undatable items stops on the parser-failure arm and leaves `backfilled_at` NULL
- Widening triggers a deeper rescan (assert on `backfilled_window_days`); narrowing does not; a
  widen mid-scan cancels and **resets the cursor**
- A channel with no `channel_state` row is swept
- Skips Twitch channels
- Rows persist per page; a restart resumes from the cursor
- A live item whose renderer carries `"Started streaming 2 hours ago"` is dated `now`, not two
  hours old
- A plain upload (`"3 weeks ago"`, no badge) is dated `coarse` and left `unknown`
- `membership_discovery = false` ⇒ no members rows written
- Removing a channel mid-scan cancels before pruning and leaves no resurrected rows; its
  `Queued` jobs **and their history rows** are deleted
- Fixture-driven continuation paging, loop detection, resume-from-cursor

*Config and migration*
- One validator: an out-of-range **per-channel override** is rejected, not just the global
- `max_feed_items` in an existing TOML is ignored without error
- v15→v16 is idempotent; `queue_priority` on a pre-existing row is 1, not NULL

---

## 18. Known limits

| Limit | Why it is accepted |
|---|---|
| A cookie lapse longer than the window loses members content published during it | Structurally identical to an RSS outage. The alternative — anchoring the window to first-observation — is the original bug |
| A stream only ever listed by a coarse source, never probed while `unknown`, has its restart missed | Widening the carve-out to `coarse` would probe every past upload on the channel |
| A members refusal phrased as `UNPLAYABLE` with no matched keyword is trusted | The price of keeping `unknown` trusted, which protects against YouTube adding a status code |
| `classifyStream:454` diverges from yt-dlp | Fixing it has download-path blast radius. `denied` contains it |
| Peak concurrency becomes `(live) + 10` | Live cannot be throttled. The old hard 2 was only defensible because it was double-counting live |

## 19. External assumptions

Unverified in this repo, asserted from observation:

| Assumption | Risk |
|---|---|
| `videos.xml` returns ≤15 items | Low. Not encoded as a constant anywhere; more items just means more first-cycle probes |
| The membership tab carries ~30 items/page | Low. Same |
| **Within one source's listing, items are strictly newest-first** | **The only one whose failure loses content** — the walk stops at the wrong item and silently skips everything behind it. Which is why the walk self-validates and degrades to probing the full bucket |
| `relativeAgeRe` matches YouTube's current en-locale copy | Degrades safely: every item becomes `assumed`/`now`, lands in the window, and is probed. Noisy, not lossy. The backfill's parser-failure arm catches the scan case |
