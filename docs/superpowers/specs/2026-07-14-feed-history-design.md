# Dated Per-Channel Feed History

**Status:** Design approved — ready for an implementation plan
**Date:** 2026-07-14, rewritten 2026-07-15
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
video back. A date-based scope defends independently, so a history purge can no
longer re-arm out-of-scope content.

## Goals

1. An RSS (or any single-source) failure must not change discovery scope.
2. Scope is a stable **archival window**. "Stable" means *invariant with respect to
   fetch outcomes* — scope is a function of the channel's content and the operator's
   configuration, never of what a given cycle happened to retrieve.
   It is deliberately **not** immutable. Scope moves when the *inputs* move, and that
   is the boundary working as intended: widening the window admits older content;
   time passing pushes an item out; flipping `membership_discovery` off hides members
   rows. Each is an operator or channel action with a visible cause.
   What must never happen is scope moving because **a fetch failed** — that is the
   one input nobody chose.
3. **Upcoming and live content is never missed, never throttled, and never consumes
   a slot.**
4. **Nothing is ever archived on a guessed date.** Every job follows a fresh probe,
   and the probe's date — not a listing's guess — decides whether the item was inside
   the window.
5. Steady-state cost must not exceed today's.

## Non-Goals

- Replacing RSS as the steady-state discovery source (see Decisions).
- Unifying the `history` table into the new store (see Decisions).
- Changing the probe-failure/cooldown machinery (`MetadataTracker`, `ProbeCooldown`).
- Any Twitch-side change. The Twitch monitor is live-only and has no cap.
- Any DECAPI-side change. DECAPI parses a single video ID out of its response
  (`internal/monitor/decapi.go:523-534`) — the channel's *latest* — so it cannot
  reach back to old content and cannot be the source of this bug class. It keeps
  its existing `HasActiveJob`/`HasProcessed`/probe path and does **not** write to
  `feed_items`; anything it finds is by construction the newest item on the channel.
  This is why `decapi` is absent from the `source` enum.

**Worker changes are *not* a non-goal.** An earlier draft scoped this to the monitor
and store. It cannot be: the queue this design creates sits directly on top of
`internal/worker/queue.go`, and one bug there (see decision 9) already loses streams
on a stock config.

## Key Insights

These shaped the design and are recorded because they are non-obvious.

**Discovery order is not recency order.** A store that prepends newly-seen unique
IDs would *preserve this exact bug*: `gr-ZTohjwnQ` was newly **discovered** on 7/14
despite being three weeks old. The store must be keyed on the item's actual publish
date, frozen at first insert.

**Rank comes from the scan, not from dates.** The `/videos`, `/streams` and
`/membership` tabs list newest-first, so a scanned row's *position* is its rank
within that listing. Dates exist to **merge** the three tabs and to place new RSS
items — not to order within one. This is the single most load-bearing insight in the
document, and an earlier draft got it backwards: it built an `assumed`-exclusion rule
on "an undated row cannot be ranked", which is false, then invented a probe carve-out
to repair the rows that rule stranded, then needed a reaper to bound the probes the
carve-out created. Three mechanisms, all resting on one wrong premise.

**A coarse date is an upper bound on recency, and the probe adjudicates.** `"1 week
ago"` means *somewhere in [7d, 14d)*. Stored as the newest instant consistent with
the text, it is admitted to a 7-day window, probed, and dropped if its true date
falls outside. Nothing is ever excluded on a date we have not verified — because an
excluded row is never probed, and so never gets the date that would have included it.

**Within one source, ordering is recency — so one probe settles the boundary.** Once
a probe places an item of source `S` outside the window, every later item of `S` is
older by construction. That is what bounds the probe cost, and it is why the pass is
serial rather than concurrent.

**The probe is how we learn rank, not just status.** Anything undatable from a
listing gets a direct probe and returns an authoritative date. This is what
guarantees goal 4.

**A probe that lacks what it needs does not fail — it lies. But it also tells us it
lied, and the design must read that.** A probe of members-only content without
cookies returns `upcoming` with no error, and `upcoming` is exempt from every gate,
so the lie is jobbed unconditionally. The natural safety argument — "no cookies ⇒ the
probe fails ⇒ no job" — is therefore false. See **The `denied` rule**, which is the
whole answer and the single most-revised part of this design.

**The window bounds the probes; nothing else needs to.** A row outside the window is
never archived whatever its status turns out to be, so its status is not worth a
request. It stays stored and correctly ordered, costing one row of disk. An item
leaves the probe list exactly as it leaves scope.

## Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Release scope | One spec, **both parts ship together** | Parts are build order, not release boundaries. Part 1 alone leaves the established gate permanently shut on any channel whose RSS never succeeds — only Part 2 sets `backfilled_at`, the gate's second key. Shipping Part 1 alone trades a wrong-archive for a **silent** no-archive |
| Scope | An **N-day window**, not a top-N row count | A date cut never splits a coarse lump, so the total order stops being load-bearing for the cut. It also matches how an operator thinks about archiving |
| Throughput | **M slots per channel**, most-recent-first; a completed job frees a slot | Everything inside the window is archived *eventually*; M paces it. M is throughput, not depth |
| M's blast radius | M paces the **back-catalogue only**. New content bypasses it entirely | A default of 3 is indefensible as a channel throttle — it would make a live stream wait behind history. As a backlog throttle it is obviously right |
| Defaults | **3 days, 3 slots** | |
| `max_feed_items` | **Dropped, not migrated** | It bounded depth; M bounds concurrency; the window has no old counterpart. Any mapping preserves a shape rather than an intent — and mapping `1000` onto a per-channel slot count reads as "archive deep" and behaves as 1000 simultaneous downloads |
| Probe scope | Probe **only what is in scope** — the window query's rows, walked serially per source with early exit | The window *is* the bound. A rank-blind probe list grows without limit once the backfill lands, and needs a reaper to contain it — a mechanism invented to contain a mechanism |
| Row retirement | **None.** No `last_listed_at`, no backoff columns | It cannot distinguish "unreachable" from "we stopped looking": a cookie expiry gates the membership fetch **and** the authenticated probe together, so both conjuncts drift true while nothing about the row changed, and the members catalogue erodes because a credential lapsed |
| Backlog queue | **`Queued` rows in the jobs table**; a scheduler admits them | A `Queued` job never enters `JobQueue` until a slot frees, which dissolves `maxLifecycle`, the silent pending drop, the missing FIFO, and crash recovery in one move |
| `num_parallel_downloads` | **VOD-only** (live/upcoming exempt), default **2 → 10** | Live cannot be throttled — a broadcast is happening now. `2` was only defensible because it was double-counting live streams it should never have touched |
| Store structure | SQLite table + **one** non-partial window index | Workload is one indexed range scan per channel per cycle (~3 per 5 min); in-memory/materialised-rank optimise microseconds while adding cache-skew and renumber races |
| Coarse-date skew | **New**, not old | Under a date cut, a too-old guess *silently excludes*; a too-new guess buys a probe that corrects it. The safe direction is a function of the cap semantics |
| `classifyStream:454` | Keep `denied`; **file `:454` separately** | The producer bug has download-path blast radius. `denied` is the cheap local containment |
| `history` table | Schema/API/UI untouched; the feed path writes **fewer** rows | Answers a different question ("acted on" vs "exists and when"). Dropping the skip/give-up writes is a population change, not a schema change |
| Catalog scan role | **Backfill only** | RSS stays the steady-state source; 3 MB/channel/cycle forever buys no correctness |
| Backfill trigger | One idempotent sweep keyed on `backfilled_at IS NULL`, at startup and from `kickMonitors`, plus a manual re-run | `kickMonitors` cannot distinguish an add from a reorder, so it must be idempotent |
| Backfill pacing | **Strictly serial across channels**, hours of wall-clock permitted, never gated on idleness | Upgrade day fires the sweep for every channel at once |
| `last_videos` | Removed | Dead code; `feed_items` supersedes it |

## External Assumptions (unverified in-repo)

Four things this design leans on are **external YouTube behavior, asserted from
observation and not enforced or documented anywhere in this codebase**. Called out
because they are load-bearing and could drift silently:

| Assumption | Status |
|---|---|
| `videos.xml` returns at most 15 items | Not asserted in code. `fetchFeed` (`feed.go:484`) and `parseFeedCandidates` impose no limit; the `15` at `feed.go:24` and `config.go:58` is the *old default cap*, an unrelated coincidence |
| The membership tab carries ~30 items per page | Not asserted anywhere. `parseMembershipTab`/`walkVideoRenderers` take whatever the page carries |
| **Within one source's listing, items are ordered strictly newest-first** | Unverified, and **the only one whose failure loses content** — see below |
| `relativeAgeRe` matches YouTube's current en-locale copy | A single regex (`channel_membership.go:49`) matched against serialized JSON; `itemAge` returns `0` on any miss |

Neither of the first two is safety-critical — if RSS returned 50 items the design
still holds, just with more first-cycle probes — but the plan should verify the RSS
figure empirically rather than inherit it, and **no code should encode either number
as a constant.**

**The third is different, and it is the one to watch.** It already underpins
`catalog_pos` and the whole "rank comes from the scan" reframing. What the source
walk changes is its blast radius: before, a mis-ordered tab cost *ranking precision*,
because every item still got a date and the date adjudicated. With early exit, a
mis-ordered source means the walk retires it at the wrong item and **silently skips
everything behind it** — completeness, not precision.

It is unverifiable from outside, so the walk **self-validates** rather than trusting
it: a probed date newer than the previous one from the same source disproves the
assumption for that source, disables early exit for it, and logs. See "The source
walk". This is the only assumption with a detector, because it is the only one that
fails silently.

**The fourth degrades gracefully, and this is worth stating because an earlier draft
built a whole mechanism to handle it.** One YouTube copy change makes *every* listing
item return `Age = 0` ⇒ `assumed` ⇒ `published = now` ⇒ inside every window ⇒ probed.
The failure mode is "everything looks new until probed, and probes repair it", which
is noisy but not lossy. It does **not** require the catalogue to be probed: the source
walk still retires each source at its first out-of-window probe.

## The `denied` rule

This is the least intuitive part of the design, and it took four rounds and three
separate patches to state properly.

`feed.go:743-746`: a probe of members-only content without cookies "gets no formats
and the classifier misfires it as **upcoming**". No error. And `upcoming` bypasses
every gate by design — the window (it carries `published = now`), M (never throttled),
and `include_non_live_content` — so a lie is jobbed *unconditionally*. It then writes
history, which blocks the real video from ever being archived correctly. That is the
2.7.2 bug.

The natural safety argument — "no cookies ⇒ the probe fails ⇒ not FRESH ⇒ no job" —
is therefore **false**, and every rule below exists because it is false.

### The `denied` predicate (canonical)

**Distrust a probe result only when YouTube said it refused us AND the classifier was
guessing.** YouTube states outright whether we were allowed to see the video, and
`parsePlayabilityStatus` (`internal/youtube/player_api_parsing.go:332-388`) already
decodes it:

```
denied  ⇔  StreamStatus == 'upcoming'
           AND PlayabilityError IN ('members_only', 'login_required')
```

**Both conjuncts are load-bearing.** Trust is *not* a function of `PlayabilityError`
alone — an earlier draft said "trusted iff `PlayabilityError == ok`" and that rule is
rejected below (it discards genuine premieres, which reach `upcoming` through
branches where playability is *not* `ok` by construction).

| Situation | `StreamStatus` | `PlayabilityError` | Trusted? |
|---|---|---|---|
| Members-only, no cookies (`LOGIN_REQUIRED` + "member"/"join") | `upcoming` | `members_only` (`:357-359`) | **denied** |
| Login required, not member-specific | `upcoming` | `login_required` | **denied** |
| Genuine upcoming (`LIVE_STREAM_OFFLINE`, "live event will begin") | `upcoming` | `ok` (`:350-351`) | trusted |
| Premiere via `videoDetails`/`liveStreamability`, not playability | `upcoming` | often **not** `ok` (`:433-454`) | trusted — see (a) |
| Status block absent, unmatched reason, or an unknown status code | any | `unknown` (`:333-335`, `:378`, `:385`, `:386-388`) | trusted — see (b) |
| Age-restricted VOD that returned formats | `vod` | `age_restricted` | **trusted** — formats came back, so the classification is grounded |
| Members-only that returned formats | `vod` | `members_only` | **trusted** — same reason; the `StreamStatus` conjunct is what allows this |

So a probe that returns `upcoming` with `PlayabilityError == members_only` did **not
observe an upcoming stream** — it observed a locked door and guessed. A probe that
returns `upcoming` with `ok` really did.

**The rule at the top of this section is the canonical `denied` predicate**, and the
table above derives it — both in this subsection, under the reader's eye. Every use
site elsewhere — the outcome list, the flow pseudocode, the contract table, the
tests — states only what `denied` *does* and refers here for what it *is*. **No use
site writes the predicate.**

That split is deliberate, and it is the fix for a defect this document generated five
times. An earlier draft wrote the predicate in five places. They drifted exactly as
duplicated rules always do: one copy was corrected to the minimal form while another
kept listing `age_restricted`/`…` as denied — a live goal-3 loss — and each round
patched the copy it happened to read. Two statements of one rule is two rules; five
is five. Predicate here, behaviour at the use site.

**Why minimal, and why everything else is trusted — including `unknown`.** Two
independent reasons, both verified in code, and both fatal to the broader rules this
went through first:

**(a) `ok` does not mean "genuine" — it means `status` was literally `"OK"`.**
`classifyStream` reaches `StreamUpcoming` through five guards — `:429`, `:432`,
`:439`, `:448`, `:454` — and `:429` *early-returns* on `isUpcomingPlayability`. So
the four after it are reachable **only when playability did *not* say "upcoming"** —
and `parsePlayabilityStatus` yields `ok` from exactly two sites
(`isUpcomingFromPlayability` `:350-351`, and `status == "OK"` `:355-356`). Those four
extra branches exist **precisely because the playability signal was found
insufficient**; `player_api_parsing.go:414-419` documents the case in the very client
the anonymous probe uses:

> "Some probes (**notably ANDROID_VR on unpublished premieres**) return this without
> a full microformat or `videoDetails.isUpcoming` flag — the raw fallthrough would
> misclassify those as `not_a_stream`. Detect it independently."

A rule of "trust `upcoming` only when playability is `ok`" therefore **denies genuine
premieres detected by the branches that exist to catch what playability misses** —
goal 3, violated from the opposite side, silently and forever.

**(b) `unknown` is not a refusal.** `parsePlayabilityStatus` returns it when the
status block is **absent** (`:333-335`), on an unmatched `UNPLAYABLE`/`ERROR` reason
(`:378`, `:385`), and from `default:` — **any status code YouTube adds that we do not
know** (`:386-388`). It means *"we could not interpret the answer"*. Treating that as
a refusal means one new YouTube status code silently converts every affected stream
to `denied`-forever — strictly worse than the `relativeAgeRe` fragility this spec
already flags.

**And the codebase already disagrees with the broad reading.**
`isTerminalPlayability` (`internal/worker/stream_processor_youtube.go:27-35`)
classifies `members_only`/`login_required`/`age_restricted` as **non-terminal** —
only `private`/`unavailable`/`region_blocked` are terminal, because "only states that
no amount of waiting or re-auth can fix are terminal". And the download path recovers
age-restricted content via `web_embedded` (`player_api_strategy.go:150-160`,
`:246-258`). Non-`ok` is a **routing signal** there, not a verdict. The probe is
single-client (`ProbeVideoStatus` = ANDROID_VR only, `:20-22`) and has none of those
fallbacks, so promoting its playability to a verdict claims more than it knows.

`members_only` and `login_required` are the exceptions: they cannot mean anything but
"authenticate to see this", and paired with the classifier's `upcoming` guess they
are exactly the 2.7.2 signature. Denying them refuses nothing we could have had —
without cookies the content is unreachable regardless.

**Which way to be wrong.** Too broad silently discards real streams; too narrow
leaves a phantom `upcoming` job that is visible in the UI and loses nothing. Goal 3
decides: be minimal.

**The `upcoming` conjunct is a metadata-presence test in disguise**, which is what
makes `login_required` safe to include. `classifyStream` returns `StreamUpcoming`
from exactly five guards, and **every one requires live metadata to be present**
(guard lines, not the `return` beneath each):

| Guard | Condition | Implies |
|---|---|---|
| `:429` | `isUpcomingPlayability` | forces `ok` (`:350-351`) — `denied` cannot fire here at all |
| `:432` | `isUpcomingPremiere` | `isPremiere` ⇒ `lbd != nil` |
| `:439` | `isUpcomingVD && !hasFormats` | `videoDetails.isUpcoming` present |
| `:448` | `hasLiveStreamability && !hasFormats` | `liveStreamability` present |
| `:454` | `!hasFormats && (lbd != nil \|\| isLiveContent)` | `lbd` or `isLiveContent` |

So a refusal that *still carries live metadata* is a content-level refusal on
known-live content — the 2.7.2 signature exactly. A refusal carrying none (anti-bot /
IP-block `LOGIN_REQUIRED`, "Sign in to confirm you're not a bot", which returns no
`videoDetails`) falls to `:451` ⇒ `not_a_stream`, and the rule never fires on it.

**Known residual, accepted.** A members refusal phrased as `UNPLAYABLE` with none of
the matched keywords yields `unknown` (`:378`) ⇒ trusted ⇒ the lie survives. That is
the price of keeping `unknown` trusted, and (b) above is why the price is worth
paying.

**`denied` creates no sink.** It writes no status and no date, so the row keeps its
classification. Permanently-denied content — age-restricted we can never see, a
members video we have no cookies for — sits in the retry pool, un-archived, and
recovers the moment access returns. That is exactly the state of *knowing nothing*,
which is what a refusal means.

**Why this and not a heuristic.** The tempting cross-check — "a row with a past
`published` that probes `upcoming` is contradictory" — is wrong and would violate
goal 3. An RSS-announced upcoming stream legitimately has a past `published` (RSS
`<published>` is the *announcement* time), so that check would discard real upcoming
streams. YouTube's own answer has no such ambiguity.

**The three gates remain, demoted to what they actually are: efficiency.**

| Rule | Purpose now | If removed |
|---|---|---|
| Discovery probe gated on `membershipActive()` | skip a request we know will be denied | wasted probes; no lie survives (playability catches it) |
| Refresh probe carries the same gate | same | same |
| `source` updates on **every** sighting | pick the right probe first time | more denials, so more retries |

Before the playability rule these three were *correctness* controls, and each was
found separately, rounds apart, because each time the missing piece was assumed to be
a failure rather than a falsehood. They were also never sufficient: `source` can only
flip if a listing we **fetch** mentions the item, and `membershipActive()` gates the
fetch — so with `membership_discovery = false` a public→members video keeps
`source='rss'` **forever**, no gate sees it, and the lie is jobbed on a channel where
members discovery is off. Three gates keyed on a value that cannot update is not
defence in depth; it is one lock with three copies of the same broken key.

Reading the playability answer closes every door at once, including that one, because
it depends on nothing we stored.

**How this was arrived at, recorded because the wrong turns are instructive:**

| Attempt | Why it failed |
|---|---|
| Gate the store reads on `membershipActive()` | it folds in **cookie state**, so scope moved on a fetch failure — the Jerry bug, reintroduced |
| Split: reads on config, probe on `membershipActive()` | the **refresh** probe was ungated, and `ProbeVideoAuth` has no cookie guard, so it did not fail — it lied, and the lie was jobbed |
| Gate the refresh probe too | `source` *picks* the probe, and a stale `source` lies identically |
| Update `source` on every sighting | `source` flips only on a **listing** sighting, and the fetch is gated — with `membership_discovery = false` a locked video never flips |
| **Read `PlayabilityError`** | ✅ depends on nothing we stored |
| (first draft: any non-`ok` ⇒ denied) | too broad — would refuse a downloadable age-restricted VOD |
| **Escalate on the refusal, and flip `source` from it** | ✅ the refusal *is* the sighting — no fetch, no tab, no toggle needed |

Four consecutive fixes patched a symptom, each one correct about the case in front of
it and blind to the next. The signal that ends the sequence was in
`player_api_parsing.go` the entire time; `VideoProbeResult` simply dropped it. The
lesson generalises past this design: **when a guard needs a stored value to be
current, ask whether the source of truth can be read directly instead.**

### Follow-up: `classifyStream:454` diverges from yt-dlp (file separately)

`denied` is a **compensating control in the consumer for a producer bug**, and the
producer bug should be recorded even though fixing it is out of scope here.

`player_api_parsing.go:454` infers `upcoming` from the *absence* of formats:

```go
if !hasFormats && (lbd != nil || isLiveContent) { return StreamUpcoming, ... }
```

yt-dlp never does this. `_list_formats`
(`references/yt-dlp/yt_dlp/extractor/youtube/_video.py:3858-3870`) reads the flag and
falls back the other way:

```python
is_upcoming = get_first(video_details, 'isUpcoming')
live_status = ('post_live' if post_live else 'is_live' if is_live
               else 'is_upcoming' if is_upcoming
               else 'was_live' if live_content ...)
```

For a members video with no formats and `isLiveContent = true`, yt-dlp returns
`was_live` — a past stream, correct — where Moombox returns `upcoming`. Under the
project's standing "match yt-dlp for extraction" rule, `:454` is the actual defect.

**Not fixed here, deliberately.** Changing the classifier has download-path blast
radius (every strategy branches on `StreamStatus`). `denied` is the cheap, local
containment. File `:454` as its own issue; if it is ever fixed, `denied` becomes
redundant rather than wrong.

**Why `:454` is filed and the download-slot bug (decision 9) is folded in**, since
both are worker-adjacent producer bugs decided opposite ways: `:454` is reachable
only through the probe this spec already guards, so `denied` contains it. The
download-slot bug has **no containment** and loses streams on today's build.

## Part 1 — The Dated Store

### Schema (v16)

```sql
CREATE TABLE IF NOT EXISTS feed_items (
    channel_id   TEXT NOT NULL,
    video_id     TEXT NOT NULL,
    title        TEXT NOT NULL DEFAULT '',
    published    TEXT NOT NULL,           -- RFC3339 UTC, best-known
    date_precision TEXT NOT NULL,         -- 'assumed'|'coarse'|'day'|'exact' (ladder)
    catalog_pos  INTEGER NOT NULL DEFAULT 0,
    source       TEXT NOT NULL,           -- rss|membership|videos|streams
    status       TEXT NOT NULL,           -- unknown|upcoming|live|vod|not_a_stream
                                          -- probe 'post_live' NORMALIZES to 'vod'
    first_seen   TEXT NOT NULL,
    PRIMARY KEY (channel_id, video_id)
);

-- Serves the window query, which the discovery walk and the archival pass BOTH
-- read. A plain range scan: seek to now-window_days, walk forward in recency
-- order, stop. The DESC matters — it is what lets the index satisfy ORDER BY as
-- well as the filter, with no temp b-tree.
--
-- NOT partial. An earlier draft carried `WHERE date_precision <> 'assumed' AND
-- status NOT IN ('upcoming','live')` to keep those rows out of a fixed-size
-- top-N. A date range has no slots to evict from, so both exclusions are gone and
-- the predicate with them — which also retires a real hazard: a partial index is
-- used only when SQLite can SYNTACTICALLY match its WHERE against the query's
-- terms, so rewording a predicate degrades it to a full scan, silently, and the
-- failure stays invisible until the catalog is large.
CREATE INDEX IF NOT EXISTS idx_feed_items_window
    ON feed_items(channel_id, published DESC, catalog_pos ASC, video_id ASC);

CREATE TABLE IF NOT EXISTS channel_state (
    channel_id     TEXT PRIMARY KEY,
    backfilled_at  TEXT,   -- NULL until a full 3-tab scan completes
    backfill_state TEXT,   -- JSON: per-tab continuation cursor
    last_rss_ok_at TEXT    -- the "established" gate for archival
);
```

There is **one** index. An earlier draft specified two — a partial
`idx_feed_items_rank` and an `idx_feed_items_status`; neither exists now. The status
index served a rank-blind probe list and a cap-exempt union, both of which are gone,
and nothing queries `feed_items` by status.

`published` is frozen at first insert and only ever *upgraded* by a higher-precision
source (`assumed` → `coarse` → `day` → `exact`). It is never recomputed from `now`,
which is what makes scope stable across cycles.

That upgrade requires a **precision-guarded upsert, not `INSERT OR IGNORE`** — the
latter can only ever insert, so a first-seen coarse date would be permanent:

```sql
INSERT INTO feed_items (channel_id, video_id, title, published, date_precision, source, ...)
VALUES (?, ?, ?, ?, ?, ?, ...)
ON CONFLICT(channel_id, video_id) DO UPDATE SET
    -- ALWAYS: a fact about this sighting; decides which probe to use
    source = excluded.source,
    -- GUARDED: monotonic estimate; a worse date never overwrites a better one
    published = CASE WHEN <newRank> > <oldRank>
                     THEN excluded.published ELSE feed_items.published END,
    date_precision = CASE WHEN <newRank> > <oldRank>
                     THEN excluded.date_precision ELSE feed_items.date_precision END

-- where <newRank>/<oldRank> are:
--   CASE {excluded.|feed_items.}date_precision
--     WHEN 'exact' THEN 4 WHEN 'day' THEN 3 WHEN 'coarse' THEN 2 ELSE 1 END
```

**The guard is per-column `CASE`, not a statement-level `WHERE`, deliberately.** A
`WHERE` gates the *whole* `DO UPDATE`, which would drag `source` along with the date
rule — and a stale `source` selects the wrong probe, which does not fail but **lies**
(see "`source` decides which probe to use"). Per-column `CASE` lets the two fields
move on their own schedules in one statement: `source` always, dates only upward.

The guard makes the write monotonic: a later, *worse* estimate can never overwrite a
better one. This is reachable, not theoretical: the backfill records a 2-day-old
stream as `coarse`, RSS later carries an exact date for it, and being recent it is
inside the window — exactly where the date decides whether it is archived.

`status` is deliberately **not** in the `DO UPDATE` set at all. Listing-derived status
is weaker than probe-derived status, and a stale listing must never demote a probed
`live` back to `vod`. (`source` is the opposite case: the listing is the *only*
authority on where the item currently appears.)

`title` is stored (the archival pass and job creation both need it) and refreshed when
a probe returns a better one, mirroring today's rule at
`internal/monitor/utils.go:341-344` that an `"Unknown Title"` placeholder must not
overwrite a real feed title.

### Coarse dates skew new, and the probe adjudicates

**A coarse date is an upper bound on recency: the item is this new *at most*.**

`itemAge` (`internal/youtube/channel_membership.go:220-257`) truncates — `"1 week
ago"` is displayed for anything 7 to 13 days old, `"1 year ago"` for anything from
365 to 729 days. It returns `n * unit` (`:252`), the **lower bound of the true age**,
so `now - itemAge()` is the **newest instant consistent with the text**.

That is exactly what the window wants, and it needs no new code:

```
published      = now - itemAge(item)      -- the naive formula, already correct
date_precision = 'coarse'                 -- meaning: this date, or older
```

**Why new is the safe direction here — and why an earlier draft said the opposite.**
The skew direction is a function of the cap semantics, not of the parser:

| Cap | A too-**new** guess | A too-**old** guess | Safe skew |
|---|---|---|---|
| top-N rank *(rejected)* | **promotes** an old VOD into scope — the Jerry shape | sinks it harmlessly | old |
| N-day window *(chosen)* | admits it to the window, where a **mandatory probe** corrects it | **silently excludes** it — no probe, no job, no trace | **new** |

Under a date cut, exclusion is the only unrecoverable outcome, because an excluded
row is never probed and so never gets the exact date that would have included it.
Inclusion costs a probe. So every item that *could* fall inside the window does, and
**nothing is ever excluded on a date we have not verified**.

Worked example, `archive_window_days = 7`:

| Displayed | Stored | True age | Outcome |
|---|---|---|---|
| `6 days ago` | `now - 6d` | [6d, 7d) | inside → probe → job |
| `1 week ago` | `now - 7d` | **[7d, 14d)** | inside (boundary) → probe → job **only if truly ≤ 7d** |
| `2 weeks ago` | `now - 14d` | [14d, 21d) | outside → never probed ✓ |

The `1 week ago` bucket is the *straddling bucket*: seven days of content collapsed
onto one timestamp, all of it admitted, most of it dropped once probed. That is the
design working, not leaking.

**The over-inclusion is paid once per item, not per cycle.** A straddling item is
probed, its exact date is written back by the precision guard, and the row falls out
of the window permanently. The source walk bounds the per-cycle cost further, to one
probe per source (see "The source walk").

**This deletes a refactor.** The rejected skew-old rule needed `now - Age - unit`, and
the unit is **not recoverable** from `itemAge`'s return value — `504h` is either
`"3 weeks"` or `"21 days"`; identical durations, different skews. That forced a
signature change across `itemAge`, `MembershipVideo.Age`, and `membershipCandidates`'
`v.Age > 0` test at `feed.go:673`. Skew-new wants the truncated lower bound, which is
what the function already returns. **Do not change `itemAge`.**

**The skew applies to `coarse` only.** An `assumed` row has no matched unit — `Age = 0`
is the *absence* of a parse, not a measurement of zero — and live/upcoming items carry
`Age = 0` by the badge short-circuit (`:227-231`), so nothing about them depends on
this.

### `published` for `assumed`, `upcoming` and `live` rows

Never store the **scheduled start time**. It is in the future, so it would sit inside
every window for weeks and claim to be the newest thing on the channel.

An `upcoming` or `live` row keeps whatever it was first stored with — `assumed`/`now`
from `itemAge`'s zero. The probe supplies **no** date for these statuses, so nothing
overwrites it. `published = now` places them inside every window, which is correct and
is what goal 3 wants: they are always in scope, always probed, never throttled. The
date starts mattering exactly when the row becomes `vod`, and the probe that observes
that transition supplies the real one.

**An `assumed` row is a claim of ignorance, not a date — and under a window that is
harmless.** `itemAge` returns `0` for anything it cannot parse, which becomes
`published = now` ⇒ inside the window ⇒ probed ⇒ dated ⇒ it either stays or drops out
on a verified date. Goal 4 is satisfied because **nothing is archived on that guess**:
an `assumed` row is `unknown` or `upcoming`/`live`, and `unknown` never jobs.

This is where an earlier draft went wrong, and the error is worth recording because it
cascaded. It excluded `assumed` rows from ranking on the reasoning that "a fabricated
date must not rank" — true under a top-N, where a fabricated `now` sits at rank 1 and
*evicts real content*. Under a window there are no slots to evict from: a wrong `now`
costs a probe, and the probe corrects it. The exclusion then stranded the very rows it
excluded (they could never be dated), which required a probe carve-out, which created
unbounded probe growth, which required a reaper. Three mechanisms, one wrong premise.

### The probe has no publish date today — adding one is Phase 1 work

This spec repeatedly says the probe returns an authoritative date. **That data does
not exist anywhere in the chain**, and unlike `Outcome`/`StreamStatus` it is not
something `ProcessYouTubeVideo` computes and discards — it is simply absent:

- `internal/monitor/utils.go:32-36` — `VideoProbeResult{StreamStatus, Title, ChannelName}`
- `internal/youtube/types.go:46` — `VideoInfo` carries only `ScheduledStartTime`
- `cmd/moombox/monitor_callbacks.go:174-178`, `:193-197` — both wiring sites copy
  exactly those three fields

**This is the load-bearing refactor.** Under a window the probe's date is what
adjudicates every straddling item; without it the whole coarse bucket is admitted and
never resolved, and every job is decided on a guess — goal 4, lost outright.

**Do not reuse `ScheduledStartTime` as-is.** `extractScheduledStartTime`
(`internal/youtube/player_api_parsing.go:113-138`) is a *conflated* accessor: it
returns `liveBroadcastDetails.startTimestamp`, else a `liveStreamability` epoch, else
microformat `uploadDate`/`publishDate` (`:129-135`). So it holds a genuine publish
date for a plain upload and a **future** timestamp for an upcoming stream. An
implementer looking for "the probe's publish date" will find it, and it will look
right.

Status-aware extraction:

```
status vod / post_live   → liveBroadcastDetails.startTimestamp   → precision 'exact'
                            (the stream's ACTUAL start; RFC3339, second-granular)
                         → ELSE uploadDate / publishDate         → precision 'day'
                            (fallback — must not be skipped, see below)
status not_a_stream      → microformat uploadDate / publishDate  → precision 'day'
status upcoming / live   → no date stored
                            (startTimestamp is the FUTURE scheduled start here)
```

The `liveStreamability` epoch branch is never a publish date — it is the scheduled
start of an upcoming stream. It must not feed `PublishedAt` at all.

**The `vod` fallback is not optional, and `post_live` is why.** `classifyStream`
(`player_api_parsing.go:391-461`) makes the two mutually exclusive:

```go
isPostLiveDVR := lbd != nil && getStr(lbd, "endTimestamp") != "" && !isLiveNow  // :407
if lbd == nil && !isLiveContent && !isPremiere { return StreamNotAStream }      // :451
if !hasFormats && (lbd != nil || isLiveContent) { return StreamUpcoming }       // :454
if isPostLiveDVR { return StreamPostLive }                                      // :457
return StreamVOD                                                               // :460
```

An ended stream that *has* an `endTimestamp` short-circuits to `post_live` at `:457`
and never reaches `:460`. So `StreamVOD` is reachable only via `lbd == nil &&
isLiveContent && hasFormats` (**no `liveBroadcastDetails` at all**) or `lbd != nil &&
endTimestamp == ""` (`startTimestamp` not guaranteed). The fixture at
`player_api_parsing_test.go:317-328` is `TestClassifyStream_PostLiveDVR` — it
demonstrates presence for `post_live`, not `vod`. A rule that only reads
`startTimestamp` therefore yields no date at all for the normal `vod` case.

Nor is microformat guaranteed per client: `player_api_parsing.go:414-419` notes "Some
probes (notably ANDROID_VR on unpublished premieres) return this without a full
microformat", and the authenticated probe uses TV_DOWNGRADED
(`player_api_strategy.go:28-34`).

Hence the chain, and the final rung:

```
startTimestamp → 'exact'  |  uploadDate/publishDate → 'day'  |  neither → leave as-is
```

**Invariant: never write a terminal status without a rankable date.** If the probe
classifies a past item but supplies no date, the row stays **`unknown`** — it does
*not* become `vod`.

Without it, a members row stored `assumed` probes to `vod` with no date, and the row
becomes `vod` + `assumed`: `published = now`, so it sits inside the window forever
claiming to be new, while `vod` is terminal so no discovery probe ever revisits it.
On a channel with `include_non_live_content = false` the refresh probe never fires
either, so nothing can ever correct it. Keeping it `unknown` costs a probe per cycle
and buys self-healing: the moment any probe returns a usable date, the row takes it.
A terminal status is a promise that we know enough to stop looking; without a date we
do not.

**`uploadDate` is day-granular, so the ladder needs a rung for it.** Calling a
date-only value `exact` alongside RSS's second-granular `<published>` would
manufacture ties. Four rungs:

```
assumed  <  coarse  <  day  <  exact
```

A VOD's `startTimestamp` is genuinely `exact` — `day` applies only to plain uploads,
which are the case where YouTube itself only publishes a date.

New fields:

```
VideoInfo.PublishedAt          (new, internal/youtube)
VideoProbeResult.PublishedAt   (new, internal/monitor)
+ both monitor_callbacks.go wiring sites
```

### `post_live` normalizes to `vod` on write

The probe returns five statuses — `internal/youtube/types.go:13` defines
`StreamPostLive = "post_live"` — but the store's enum has no `post_live`. That was an
omission, and per the analysis above it is not a rare one: `post_live` is the *common*
classification for an ended stream.

Left unstated, a probed `post_live` row would match **no** archival branch
(`upcoming/live`, `vod/not_a_stream`, `unknown`) while still sitting inside the
window — in scope and undecidable.

So the store **normalizes `post_live` → `vod` on write.** The store's vocabulary
exists to answer two questions — is this in the window, and should it be archived —
and `post_live` and `vod` answer both identically: today's `nonLiveSkipReason` is
reached from a single `case "post_live", "vod":` (`utils.go:326`). The distinction is
a *download-strategy* concern (DVR window, still-processing manifests), and the worker
re-probes for that anyway.

The date rule above is written in probe terms (`vod`/`post_live` → `startTimestamp`)
precisely because it runs *before* this normalization.

### The window query

Scope is a **date range**, not a rank. One query serves the discovery walk and the
archival pass both — they read the same set, which is what makes "probe only what we
would archive" true rather than aspirational.

```sql
SELECT video_id, title, published, status, source, catalog_pos
  FROM feed_items
 WHERE channel_id = ?
   AND published >= ?              -- now - archive_window_days
   -- AND source <> 'membership'   ← iff NOT MembershipDiscoveryEnabled()
   --                                 (the OPERATOR'S toggle only — never
   --                                  membershipActive(), which folds in live
   --                                  cookie state and would move scope on a
   --                                  cookie lapse)
 ORDER BY published DESC, catalog_pos ASC, video_id ASC
```

**No `LIMIT`, and no status or precision exclusions.** Each one is gone for its own
reason, and the earlier top-N formulation needed all three plus a partial index:

| Removed | Why it is unnecessary now |
|---|---|
| `LIMIT N` | the window is the bound. `M` is throughput, not depth — see "The scheduler" |
| `status NOT IN ('upcoming','live')` | it existed to stop premieres **evicting VODs from a fixed-size top-N**. A date range has no fixed size, so there is nothing to evict from. They carry `published = now`, sit inside every window by construction, and cost nothing to anyone |
| `date_precision <> 'assumed'` | it rested on "an undated row cannot be ranked" — false. `assumed` rows land inside the window and get probed, which is exactly what we want |

The ordering still uses the full `(published DESC, catalog_pos ASC, video_id ASC)`
key — not to cut a lump at rank N, but because **the source walk consumes rows in
recency order**, and `catalog_pos` is what makes order well-defined inside a
coarse-date lump.

**Verified, not assumed** — run against `modernc.org/sqlite` (the driver actually in
use), 6,000 rows across 4 channels, after `ANALYZE`:

```
window query (all sources)
  SEARCH feed_items USING INDEX idx_feed_items_window (channel_id=? AND published>?)

window query (membership_discovery = false)
  SEARCH feed_items USING INDEX idx_feed_items_window (channel_id=? AND published>?)
```

No `SCAN`, and no `USE TEMP B-TREE FOR ORDER BY` — the index satisfies the ordering
as well as the filter, which is why the `DESC` in its definition matters. The
`source <> 'membership'` arm is a post-filter on rows the range already narrowed, so
it does not disturb the plan.

**Widening the window admits already-stored content, with no probe.** Because `vod`
rows are terminal and never *discovery*-probed, a per-candidate check evaluated only
at probe time would leave an item skipped at 3 days skipped forever, even after the
operator sets 30. The query re-evaluates scope every cycle from the store, so a config
change takes effect immediately.

Precisely: **entering scope needs no probe; becoming a job still does.** The store
already holds the date and status, so re-scoping is pure SQL. But the item then hits
the archival branch, which fires a refresh probe before creating the job — because a
job must never be built on stale metadata.

**An `unknown` row inside the window is probed, not skipped.** An RSS-sourced row
carries an `exact` date, so a video that is listed but never successfully probed sits
at `unknown` inside the window indefinitely. It is never jobbed (`unknown` ⇒ no job),
and it costs one probe per cycle while it remains in the window — bounded, and it
resolves the moment the probe succeeds or the window moves past it. Under the rejected
top-N this row was worse: it *held an archival slot* and masked real content behind
it. A date range has no slots to hold.

## The passes

### Descriptions and term matching

Descriptions are deliberately **not** stored. The description used for term matching
is not a property of the item — `parseFeedCandidates` derives it from feed context,
stripping boilerplate lines that also appear in the `num_desc_lookbehind` neighbouring
entries (`internal/monitor/feed.go:615-620`). Freezing that in a row would preserve a
value computed against a feed state that no longer exists, and the backfill cannot
produce it at all (tab listings carry no descriptions).

So descriptions stay exactly where they are today: computed per cycle, in memory, from
the RSS response. The archival pass uses them when this cycle's RSS carried the item —
which is always the case for a newly-seen item.

The one degradation: an item re-evaluated from the store *without* being in this
cycle's RSS response (for example after the window is widened) matches on **title
only**. This is not a new behavior — membership and DECAPI candidates are already
title-only (`feed.go:694-696`) — but it must be documented.

**Store non-matching items; do not probe them.** Storing is what makes the store a
catalogue rather than a filtered view: terms can change, and the backfill writes
everything regardless. Probing them would pay `/player` fetches for content that can
never be jobbed on a channel using `terms` — a cost regression against goal 5,
introduced purely by splitting the passes. So the discovery pass keeps today's order:
`HasActiveJob` → terms → probe (`feed.go:704-725`).

**There is no `assumed` carve-out, and an earlier draft's was cargo cult.** That draft
probed `assumed` rows *regardless of terms*, to obtain a date. Its stated justification
was that `assumed` rows were excluded from the top-N, so an unprobed one could never
be ranked and scope would be computed over a subset. Both halves of that are gone: the
exclusion no longer exists, and a window has no subset to miscompute. A non-matching
`assumed` row now sits inside the window at `published = now`, is never probed, and is
never jobbed — which is correct, because terms already said we do not want it. It costs
one row in the query result. Skipping it costs nothing, because nothing downstream
consults its date.

Term matching cannot be a SQL predicate — it needs the in-memory description for
RSS-carried items — so it stays a Go-side filter over the query's rows, exactly as it
is a Go-side filter over candidates today.

### Probe rules

Probes have two distinct triggers. Conflating them is a mistake: one is discovery, the
other is metadata freshness before job creation.

**Discovery probes** — every cycle, over the window's rows only:

```
per row, in this order:
  skip if HasActiveJob                        (the worker owns it)
  skip if NOT term-match                      (terms gate jobbing; an unjobbable
                                               item's status is not worth a request)
  skip if source='membership' AND NOT membershipActive()
                                              (no cookies / discovery off ⇒ an
                                               authenticated probe is useless)
  then, by status:
    unknown  → probe (nothing can be missed)
    upcoming → probe (goal 3)
    live     → probe (goal 3)
    vod / not_a_stream → NOT probed for discovery
  probe choice: source='membership' ⇒ authenticated, else anonymous
  ESCALATION: an anonymous probe returning members_only sets source:='membership'
    and re-probes with cookies THIS CYCLE if membershipActive() (cooldown bypassed
    for the re-probe). login_required does NOT escalate: it is also YouTube's
    anti-bot refusal on PUBLIC videos, so it must not relabel — and without a
    relabel the escalation would recur every cycle forever.
    See "A refusal escalates, and sets `source` itself".
  (no row is ever deferred or retired — the window is the bound)
```

**Stating this as the bare status rule is a known trap.** It then reads as "probe
every `unknown` row unconditionally", which drops the term gate and reintroduces a
goal-5 cost regression on any channel using `terms`.

### The source walk

The window admits the whole straddling bucket, and probing all of it would be
wasteful. It is not necessary: **within one source the listing is recency order**, so
once a probe places an item of source `S` outside the window, every later item of `S`
is older by construction and no probe can change that.

So the discovery pass is a **serial walk in store order, with per-source exhaustion**:

```
candidates = window query, ORDER BY published DESC, catalog_pos ASC, video_id ASC
exhausted  = {}                          -- per PASS, in memory; never persisted
lastDate   = {}                          -- per source, for the ordering check

for row in candidates:                   -- SERIAL. Not concurrent: parallel probes
                                         -- are already in flight past the boundary
                                         -- before it is known.
    if row.source in exhausted:  skip    -- proven older than a verified boundary
    apply the per-row gates above (HasActiveJob / terms / membership)
    outcome, date := probe(row)

    if outcome == probed AND date supplied:
        if date is NEWER than lastDate[row.source]:
            -- the ordering assumption is FALSE for this source
            disable early-exit for row.source this cycle; log a warning
        lastDate[row.source] = date
        if date is OUTSIDE the window:
            exhausted += row.source       -- everything behind it is older
    -- errored / denied / cooldown / probed-with-no-date: DO NOT exhaust.
    -- The boundary was not learned. Retiring on these truncates the source on a
    -- transient fault — "unreachable" is not "we stopped looking".

stop when every source is exhausted, or the candidate list ends
```

Sources that need the walk are the coarse ones — `videos`, `streams`, `membership`.
RSS rows carry `exact` dates already, so no probe can move them and the walk never
retires that source on their account.

**The exhaustion trigger is narrow by design.** Only a probe that *returned a date*
may retire a source. This is the retirement mistake in a new place: a transient cookie
fault that denies four members rows in a row must not be read as "the source ends
here".

**The cost this removes is per-cycle, not total.** Each probe writes an exact date
back, so that row leaves the window permanently and the bucket drains at one item per
source per cycle regardless. The walk bounds the **per-cycle** cost to
`items-inside + 1 per source`, which is the number that matters on a 24/7 process. The
worst case is stark: at a 30-day window, every item displayed `1 month ago` has a true
age of at least 30 days, so the entire bucket is admitted and the entire bucket fails —
one probe settles it instead of thirty days of content.

**Serial probing costs latency, and the budget is there.** `items-inside` sequential
`/player` calls at ~200 ms each is a few seconds per channel against a 5-minute cycle.

**It self-validates, because it promotes an assumption.** See External Assumptions for
why. The check costs one comparison and one variable per source, needs no extra
requests, and uses dates the pass already has. It cannot repair a mis-ordered source —
it turns a silent completeness loss into a logged, self-detecting degradation that
falls back to probing the full bucket. That is the same trade as `denied`: distrust the
inference exactly when the evidence says it was a guess.

### The probe outcome must surface to the caller, not be hooked inside

`ProcessYouTubeVideo` is shared: it is called from `internal/monitor/feed.go:753`
**and** `internal/monitor/decapi.go:583`. So no `feed_items` write may be hooked inside
it — DECAPI would then write rows, breaking the "no DECAPI-side change" non-goal.

Instead the result gains an **outcome** discriminator:

```
feed path (probeAndClassify) — four outcomes:
  probed      — a probe ran, returned metadata, and the result is NOT denied.
                NOT "PlayabilityError == ok": that was an earlier, over-broad
                rule, and pairing it with the current `denied` leaves
                upcoming+unknown premieres and age_restricted VODs in NEITHER
                bucket — undefined, never FRESH, never jobbed.
  denied      — YouTube refused us and the classifier was guessing.
                Predicate: see "The `denied` predicate (canonical)". Do not restate it.
                Behaviour: NOT FRESH; writes no status and no date; retried next
                cycle. (A `members_only` refusal separately sets `source` before
                the outcome is classified — that is the refusal's write, not
                `denied`'s.)
  errored     — a probe ran and failed                     (utils.go:269-294)
  cooldown    — no probe ran; ProbeCooldown suppressed it  (utils.go:258-262)

DECAPI (composed ProcessYouTubeVideo) — keeps a fifth:
  passthrough — no probe ran; ProbeVideo is not wired      (utils.go:248-250)
```

**Contract: `StreamStatus` is meaningful if and only if `outcome == probed`.** This
falls straight out of the existing control flow, and the type should enforce it rather
than rely on the reader:

| return site | `meta` | outcome |
|---|---|---|
| `utils.go:249` | not yet assigned | `passthrough` |
| `utils.go:261` | not yet assigned | `cooldown` |
| `utils.go:294` | zero value (`err != nil`) | `errored` |
| `utils.go:315`, `:332`, `:350` | valid | `probed`, or `denied` (predicate: see "The `denied` predicate (canonical)") |

`meta` is assigned at `:268`, so the first two returns precede it entirely. Reading
`StreamStatus` on any non-`probed` outcome yields `""` and would classify silently
wrong — "empty string means not-a-stream-ish" is exactly the kind of thing that
compiles.

**`passthrough` breaks the obvious rule, which is why it must be named.** An earlier
draft claimed all non-`probed` outcomes "collapse to `ShouldProcess=false`". Three do —
`utils.go:258-262` (cooldown) and `:294` (error) — but `p.ProbeVideo == nil` returns
`ShouldProcess: **true**` at `:248-250`, passing the item straight through un-probed.
So "not `ShouldProcess`" is not a usable proxy for "not fresh", and a naive
implementation deriving freshness from `ShouldProcess` would job on no metadata
whenever the probe is unwired.

The feed path sidesteps it entirely by **requiring a wired probe**: `probeAndClassify`
has no passthrough outcome, and a nil `ProbeVideo` there is a programming error.
Production always wires it (`cmd/moombox/monitor_callbacks.go:180`), and the only nil
caller is a test (`utils_test.go:215`). DECAPI keeps the composed function and with it
today's nil behavior, unchanged.

**`outcome == probed` is the FRESH predicate** — for the store write *and* as the
precondition for a job. It is not a new source of truth: it reads a decision
`ProcessYouTubeVideo` already makes internally and currently discards.

### The split must be a function split, not just a richer return type

Adding fields is not enough, because `ProcessYouTubeVideo` does not merely *decide* —
it has **side effects the feed path must no longer trigger**:

```
utils.go:284  AddToHistory  — probe give-up
utils.go:313  AddToHistory  — not_a_stream skipped by nonLiveSkipReason
utils.go:330  AddToHistory  — post_live/vod skipped by nonLiveSkipReason
```

And `nonLiveSkipReason(includeNonLive=false, _)` **always** skips (`utils.go:229-231`),
so on a default-config channel those fire for *every* plain upload.

| | used by | does |
|---|---|---|
| `probeAndClassify` | feed monitor | probe; record cooldown; update `MetadataTracker`; return `Outcome` + `StreamStatus` + title/channel/publish date. **No history writes, no job verdict.** |
| `ProcessYouTubeVideo` | DECAPI only | `probeAndClassify` + today's `nonLiveSkipReason`/`AddToHistory`/`ShouldProcess` logic, byte-identical |

**Two consequences worth stating:**

- **`IsReprobe` becomes dead for the feed path.** It exists only to drive
  `nonLiveSkipReason` and to demote log level; the archival pass consults
  `HasProcessed` directly. DECAPI still passes it.
- **Feed-path give-up no longer writes history.** Today give-up calls `AddToHistory`,
  whose only effect is flipping the reprobe/log-level flag — it explicitly does *not*
  stop re-probing (`utils.go:272-279`), so nothing is lost. This stops manufacturing
  orphaned-history rows for videos that were never jobbed, which is precisely the row
  class that accumulated into the 80-entry purge that re-armed `gr-ZTohjwnQ`.

**Only FRESH items become jobs**, where fresh means **this cycle's probe returned
metadata** — `outcome == probed`. It does **not** mean `ShouldProcess == true`, and
conflating the two is a trap that survived eight review rounds:

`ShouldProcess` is *not* "the probe worked". A probe that **runs and succeeds** returns
`ShouldProcess=false` whenever `nonLiveSkipReason` skips — `utils.go:315`
(`not_a_stream`) and `:332` (`post_live`/`vod`). The mapping is not invertible: `false`
means "errored **or** cooled down **or** succeeded-but-not-jobbable".

Deriving the store write from `ShouldProcess` breaks the design on the **default**
configuration (`IncludeNonLiveContent` is a plain `bool` at
`internal/config/types.go:264`, so it defaults to `false`):

1. RSS carries a plain upload ⇒ stored `unknown`/`exact`.
2. Discovery probes it ⇒ succeeds ⇒ `not_a_stream` ⇒ `nonLiveSkipReason(false, _)`
   skips ⇒ `ShouldProcess=false`.
3. If that means "not fresh", the status write never fires ⇒ the row stays `unknown`
   **forever**.
4. Nothing bounds it: the probe *succeeded*, so no failure path engages either.

Every plain upload on every default-config channel would then be probed at full rate
forever. So the design needs **two predicates, not one**:

| Predicate | Value | Purpose |
|---|---|---|
| store write | `outcome == probed` | record status/date/title whenever metadata came back — **regardless of jobbability** |
| job | `outcome == probed` **plus the archival rules** | the archival pass decides from the fresh `StreamStatus` |

They coincide on the `live`/`upcoming` branches (`utils.go:319-324` fall through to
`ShouldProcess: true`), which is exactly why the divergence hid.

### Refresh probe

On demand, only when a stored item is about to become a job:

```
in window AND NOT HasProcessed AND term-match AND status IN (vod, not_a_stream)
    AND include_non_live_content          ← BEFORE probing; a channel that archives
                                            no VODs must not pay the probe
    AND channel established
    AND NOT (source='membership' AND NOT membershipActive())   ← same gate as discovery
    → re-probe now, WRITE THE RESULT BACK, then RE-CHECK THE WINDOW against the
      probe's date, then decide (denied/errored/cooldown ⇒ no job)
```

**The window re-check is the crux of the whole skew-new design.** A `coarse` row is
admitted on an *upper bound* — `"1 week ago"` could be 13 days old. The refresh probe
supplies the exact date; if that date falls outside the window, **there is no job**,
and the row drops out permanently on the date just written. Skipping this re-check
would archive on a guess and lose goal 4 outright.

**The membership gate applies here too.** With the `denied` rule in place this is now
an *efficiency* control, not a correctness one — a cookieless probe of members content
returns `members_only`, which is `denied`, so no job results either way. The reasoning
below is retained because it is what the gate was *derived* from, and because it is
exactly what happens if the `denied` rule is ever weakened: `ProbeVideoAuth` carries no
cookie check — `cmd/moombox/monitor_callbacks.go:188-198` calls
`ProbeVideoStatusAuthenticated` unconditionally — so with no cookies the
"authenticated" probe still *runs*, gets no formats, and misclassifies the video as
`upcoming`. Ungated, that misclassification is laundered straight into a job, and
`AddToHistory` then poisons the row against ever being archived correctly.

**The refresh probe's result is authoritative, and both halves matter.** An earlier
draft phrased the rule as "job iff *still* non-live" and got both wrong:

- A stream can restart on the same video ID — `feed.go:702-703` says so explicitly
  ("not if merely finished — a stream may restart on the same URL"). So a stored `vod`
  can legitimately refresh to `live`. Treating a non-`vod` refresh as "skip" would
  refuse to archive a live stream: a goal-3 violation.
- Discarding the refresh result would leave the row `vod` forever. `vod` is not in the
  discovery probe list, so the restarted stream would never be looked at again and
  would be missed **permanently**, not just this cycle.

It also keeps a documented user workflow intact. `internal/monitor/utils.go:226-227`
describes deleting a job and clearing its Orphaned history entry so the video "can be
picked up again". That still works: clearing history makes `HasProcessed` false, the
item is still in the window, so the refresh probe fires and it is re-jobbed — provided
it is still within the window, which is the intended new limit.

The refresh probe is rare by construction: it fires only for an item that is in
window, unprocessed, and terminal — i.e. after a history clear or a window widening.

### Steady-state flow (per channel, per cycle)

```
1. fetch RSS ─┐ (independent; either may fail)
   fetch /membership ─┘
2. UPSERT every item seen → feed_items  (precision-guarded; never downgrades)
      RSS  → published exact
      memb → coarse from Age (skewed NEW); undatable → 'assumed'/now
3. DISCOVERY PASS — the source walk over the window query
        └─ read from the STORE, not this cycle's response
        └─ serial; per-source early exit; see "The source walk"
      per item: skip if HasActiveJob → skip if NOT term-match → probe
        └─ same order as today (feed.go:704-725)
        └─ source='membership' ⇒ authenticated probe (ProbeVideoAuth); else
           anonymous. This replaces today's in-memory discoveredVideo.authProbe
           (feed.go:681/:748), which cannot survive store-driven candidates.
        └─ a members_only refusal sets source:='membership' (the refusal IS the
           sighting) AND escalates to the authenticated probe in the same cycle if
           membershipActive(), bypassing the cooldown gate. login_required does
           NEITHER.
      outcome per item:
        probed     ⇒ UPDATE title
                     UPDATE status (post_live→vod)
                       └─ EXCEPT: a terminal status is only written if the row will
                          have a rankable date. No date ⇒ stay 'unknown'
                     UPDATE published + date_precision ONLY IF the probe supplied a
                       date AND its precision is strictly better than the stored one
                       └─ SAME precision guard as the upsert. Without it a probe of
                          a plain upload writes uploadDate ('day') over RSS's
                          second-granular <published> ('exact')
                       └─ upcoming/live supply NO date; must never write published=''
                     mark FRESH for this cycle
                     └─ fires whenever METADATA came back — NOT gated on
                        ShouldProcess, which is false for a successful probe
                        of a non-jobbable item (utils.go:315, :332)
        denied     ⇒ (predicate: see "The `denied` predicate (canonical)")
                     No status/date written; NOT fresh; retry next cycle
        errored    ⇒ store untouched; NOT fresh; retry next cycle
        cooldown   ⇒ probe skipped; NOT fresh; retry next cycle
        (no passthrough on this path — probeAndClassify requires a wired probe)
4. ARCHIVAL PASS  (runs after the probe pass, on corrected dates)
      re-read the window query — the discovery pass has just corrected dates,
      so rows may have entered or left scope since step 3
      [excludes source='membership' iff NOT MembershipDiscoveryEnabled() — the
       operator's choice only. Reads must be gated, not just fetches, and no read
       may depend on cookie state, or a cookie lapse moves scope]
      per item: skip if HasActiveJob → skip if NOT term-match → decide:
         upcoming/live    → job iff FRESH this cycle
                            (never throttled; NOT gated by HasProcessed — see below)
         vod/not_a_stream → skip if HasProcessed
                            skip unless include_non_live_content   ← before probing
                            skip unless channel established
                            skip if source='membership' AND NOT membershipActive()
                            → refresh-probe, WRITE THE RESULT BACK, RE-CHECK THE
                              WINDOW on the probe's date, then decide:
                                 denied           → NO JOB, retry next cycle
                                   └─ MUST be listed FIRST: `denied` carries
                                      StreamStatus=='upcoming' by definition, so
                                      any table that omits it routes it straight
                                      to `live/upcoming → job` — the 2.7.2
                                      misfire, laundered
                                 outside window   → NO JOB, ever (the coarse date
                                                    was an upper bound; the exact
                                                    date disproved it)
                                 live/upcoming    → job (never throttled)
                                 vod/not_a_stream → job (M-gated if backlog)
                                 probe errored    → no job, retry next cycle
                                 cooldown-skipped → no job, retry next cycle
         unknown          → NO JOB. We do not know what it is. Retry next cycle.
5. AddToHistory on job creation  (unchanged)
6. on RSS success ⇒ channel_state.last_rss_ok_at = now
```

**`HasProcessed` must gate ONLY the `vod`/`not_a_stream` branch.** This is not a
stylistic choice — gating live/upcoming on it loses streams permanently.

Today `HasProcessed` never touches live/upcoming. `feed.go:730` passes it as
`IsReprobe`, and `IsReprobe` is consulted in exactly one place: `nonLiveSkipReason`
(`utils.go:228-236`), reachable only from the `not_a_stream` and `post_live`/`vod`
branches. Job creation dedups on the **job row** (`AddJob` returning `added=false`,
`monitor_callbacks.go:252-259`), not on history.

That distinction is load-bearing because **history rows exist with no job row**, so
`HasProcessed` is true for videos that were never archived. A `HasProcessed` gate on
the live branch would skip such a row the moment its probe returned `live` — **and the
stream is gone forever.**

**Where such rows come from — after this design lands.** The obvious source is the feed
path's own probe give-up, and an earlier draft cited it. That citation is now dead: the
`probeAndClassify` split removes that write. The rule stands anyway, because two
sources survive:

- **DECAPI give-up.** `decapi.go:583-594` still calls the composed
  `ProcessYouTubeVideo` with `AddToHistory` wired. DECAPI and the feed monitor see the
  same YouTube channels.
- **Every pre-existing row.** Installs upgrading into this design carry years of them —
  the 80-row purge in the Problem section is direct evidence.

**The passes read the store, not the response.** This is what removes the bug class: an
RSS 404 no longer changes either work list, it only means nothing new was added.

**The two passes are separate because dates change mid-cycle.** The probe pass corrects
`published` to an authoritative value; the archival pass must run afterwards so scope
is computed on corrected dates. Merging them would scope an item on the guess the probe
just disproved.

### `source` decides which probe to use

Today the authenticated-probe choice rides on the in-memory candidate:
`membershipCandidates` sets `authProbe: true` (`feed.go:681`) and `processCandidate`
reads it (`feed.go:748`). That field cannot survive the move to store-driven passes — a
row read back from `feed_items` has no `authProbe`.

**`source = 'membership'` is the signal.** Members-only content must be probed with
cookies or the classifier misfires it as `upcoming` (`feed.go:743-746`) — the bug 2.7.2
fixed. This is one of two jobs `source` does; the other is the toggle predicate below.

### A refusal escalates, and sets `source` itself

`source` is **not** only listing-derived. A **`members_only`** refusal — from *any*
probe — sets `source := 'membership'`, and this is the more authoritative of the two
writers.

**Only `members_only`, never `login_required`.** The two are not interchangeable, and
conflating them loses public videos. `parsePlayabilityStatus` returns `login_required`
as the *fallback* for any `LOGIN_REQUIRED` whose reason mentions neither member/join
nor age (`:357-363`) — which includes YouTube's anti-bot **"Sign in to confirm you're
not a bot"** refusal, served on **public** videos when it decides to IP-block us.
Flipping `source` on that would mark a public video as members content forever, and
with `membership_discovery = false` the read arm would then hide it from scope
entirely: a silent archival loss caused by an anti-bot block.

`members_only` proves membership — it is returned only when the reason says
"member"/"join" (`:358-359`) or `UNPLAYABLE` says "member" (`:366`).

**Why the probe beats the listing here.** An earlier draft had `source` flip only on a
listing sighting, and then had to concede that a public video later locked to members
can *never* flip: the membership tab is the only listing that would mention it, and
`membershipActive()` gates that fetch. The row was stuck with `source='rss'` forever,
probed anonymously forever, refused forever, and never archived — `denied` stopped the
phantom job but achieved nothing else.

The refusal itself dissolves that. YouTube stating `members_only` **is** the sighting:
a direct, authoritative answer about where the item lives, needing no fetch, no tab, and
no toggle.

```
probe returns members_only
  ⇒ source := 'membership'      ← the refusal proves the item is members-only
  ⇒ if the probe was anonymous AND membershipActive():
       re-probe with cookies, same cycle, bypassing the cooldown gate
  ⇒ classify the final result through the SAME outcome rules as any probe
     (probed / denied / errored) — this branch writes no bespoke predicate
```

**Escalate only on `members_only`, and only because it relabels.** The two are one
event, not two rules:

- `members_only` sets `source` — and *because* it sets `source`, the escalation fires
  at most **once**. Next cycle the row is `source='membership'` and the authenticated
  probe is selected directly.
- `login_required` does **not** escalate. It must not relabel — and an escalation that
  cannot relabel never terminates: `source` stays `rss`, the next cycle probes
  anonymously, is refused again, and escalates again. That is **2 probes per cycle for
  the duration of the block**, permanently, breaching goal 5 against today's 1 — and it
  doubles our request rate against YouTube during precisely the condition (an IP block)
  where more requests make recovery worse.

**An escalation is only bounded if its trigger is also what stops it recurring.**

**The escalated result is classified normally.** An earlier draft gave this branch its
own list (`ok ⇒ probed / members_only ⇒ denied / …`) — a `PlayabilityError`
trichotomy, which is both the over-broad rule this document rejects twice and a second
statement of the predicate at a use site. It disagreed with the canonical rule in three
places: `vod`+`members_only` (canonical: **trusted**, formats came back) would have
been denied; `not_a_stream`+`login_required` (canonical: trusted) would have been
denied; and `upcoming`+`age_restricted`/`unknown` matched no branch at all. The
escalated probe is a probe. Its result goes through the same classifier.

**The cooldown must be bypassed for the escalated re-probe.** A `members_only` refusal
is a *successful* probe and records the cooldown (`utils.go:299`); gating the
same-cycle retry on it would suppress the escalation entirely for any operator with
`probe_cooldown > 0`.

**Why setting `source` even on a failed escalation matters.** It looks pointless — we
still cannot see the item — but it is what bounds the cost:

| Next cycle | Before | After |
|---|---|---|
| Wrong tier, cookies valid | anon probe + escalated auth probe = **2/cycle** | auth probe directly = **1/cycle** |
| No cookies | anon probe, refused = **1/cycle** | probe gate skips it = **0/cycle** |

**The toggle still governs.** With `membership_discovery = false`, `membershipActive()`
is false, so no escalation happens and the row is `denied`. The operator asked not to
discover members content; discovering it *harder* would defeat them. `source` still
flips, which is correct and useful: the read arm then hides the row from scope, and the
probe gate skips it entirely.

**The transition cases:**

- **Members → public.** RSS now lists it with an `exact` date; the stored row is
  `coarse`, so the precision guard fires and `source` becomes `rss`. Subsequent probes
  are anonymous — correct, it *is* public now.
- **Public → members.** RSS drops it. The membership tab may never list it, so the
  *listing* may never flip `source`. **The probe does**, via the refusal.

**`source` is updated on every sighting, independent of the precision guard.** An
earlier draft folded `source` into the guarded `DO UPDATE`, so a public→members
transition left `source='rss'` (the membership tab's `coarse` date loses to the stored
`exact`), and dismissed the result as "wrong but safe: we decline to archive rather
than misfire". **That was false, and it is the same mistake the refresh-probe gate
exists to prevent: a probe without cookies does not fail, it lies.** `source='rss'` on
a members video picks the anonymous probe, which returns `upcoming` with no error, is
FRESH, and is jobbed — bypassing every gate.

The only way two sources contend for one row in a cycle is the backfill's `/videos`
and `/streams` tabs, which both list past streams; both are public, so either value
selects the anonymous probe and last-writer-wins is harmless.

### `membership_discovery = false` must gate reads, not just writes

The store-driven passes create a hazard the response-driven design never had, and
closing it at write time is not enough.

`membershipActive()` (`feed.go:518-523`) stops the *fetch*. Today that is sufficient,
because the candidate pool **is** the response. But the passes read the **store**, which
still holds every members row written while the toggle was on. So a members premiere
stored while it was on is still `unknown`, still in the window, still probed with
cookies, and still jobbed — on a channel where members discovery is off.

**The fix is a read-side predicate — but the arms must not use the same predicate.**
This is the trap, and an earlier draft fell into it by gating both on
`membershipActive()`.

**`membershipActive()` is not the toggle.** It folds in live cookie state:

```
membershipActive()          = FetchMembership != nil && MembershipEnabled()   feed.go:518-523
MembershipEnabled()         = MembershipDiscoveryEnabled() && HasAuthCookies() monitor_callbacks.go:218-224
```

and its own comment says it "re-reads the config flag **AND cookie state** live each
cycle". Cookies lapse for reasons nobody chose: the exporter rotates the file
(`cookies/jar.go:72-90`), the browser session ends, a refresh races.

So gating the **read** on `membershipActive()` reintroduces this spec's own bug: a
cookie lapse would remove members rows from scope, which is scope moving because a
fetch input failed — the Problem statement of this document, reached through a new door.

```sql
-- READ arm (the window query): the OPERATOR'S CHOICE only
AND source <> 'membership'      -- iff NOT MembershipDiscoveryEnabled()

-- PROBE arm (the discovery walk): config AND cookies
AND source <> 'membership'      -- iff NOT membershipActive()
```

The asymmetry is the point:

- **What is visible may only move on an operator's decision.**
  `MembershipDiscoveryEnabled()` is exactly that decision and nothing else.
- **Probing may be opportunistic.** With no cookies an authenticated probe is useless,
  so skipping it costs nothing — and loses nothing, because the row stays in the
  window and simply cannot become a job without a FRESH result.

**The read arm is not redundant, and the reason is scope — not job prevention.** An
earlier draft justified it by claiming `denied` cannot cover "a members probe with
valid cookies". That reason is wrong: with the toggle off, a `source='membership'` row
hits the *probe* gate, is never probed, never FRESH, and cannot be jobbed at all.

What needs the read arm is what the operator sees and what the walk spends. With the
toggle off and no read gate, members rows still occupy the window — they are walked,
they are counted, and with `M` slots they compete for the channel's archival
throughput against the public content the operator actually asked for. The operator
disabled members *discovery* and silently lost archival bandwidth to it.

So the three controls partition cleanly by what they protect:

| Control | Protects | Can another cover it? |
|---|---|---|
| Read arm — `MembershipDiscoveryEnabled()` | **scope**: members rows must not occupy the window when the operator turned discovery off | No — nothing else touches scope |
| Probe gates — `membershipActive()` | **efficiency**: skip a request we know will be refused | Yes, `denied` catches the result anyway |
| `denied` — playability | **correctness**: a refusal must never be read as `upcoming` | No — it is the only control that depends on nothing we stored |

A cookie lapse therefore leaves scope *exactly* where it was: members rows stay in the
window and simply go un-probed for a cycle. Goal 2 holds at no cost, and goal 3 is
unaffected — the stream is jobbed on the first cycle cookies return, which is also the
first cycle it could have been downloaded at all.

The rows stay stored when the toggle goes off (turning it back on must not lose them)
and stop being visible.

### The established gate

A fresh install whose very first cycle 404s would hold only membership items.
Therefore: **past content is not archived until `channel_state.last_rss_ok_at IS NOT
NULL` or `backfilled_at IS NOT NULL`.** Upcoming/live still pass. This gate is
independent of the backfill, so a failed backfill never blocks normal operation.

**Both keys must exist at ship, and this is why the phases do not ship apart.** Only
Phase 2 sets `backfilled_at`. With Phase 1 alone the gate rests entirely on
`last_rss_ok_at`, so a channel whose RSS is *permanently* broken — a dead or renamed
channel ID, or a members-only channel whose public feed legitimately carries nothing,
not the intermittent 404s that motivated this spec — never establishes and never
archives past content, indefinitely and silently. Live and upcoming streams are
unaffected, which is what makes the silence hard to notice: the channel looks like it
is working.

The gate itself is right. Without a single successful full listing we have no basis for
claiming to know a channel's recent content, and guessing is precisely the bug being
fixed. What is wrong is a gate with one key when the design specifies two.

**One residual worth naming:** a successful RSS fetch returning *zero* entries also sets
`last_rss_ok_at`. For a members-only channel with no public uploads, the store then
holds only membership items and all of them are in the window. That is **correct** —
but it looks like the original bug and is not.

## The scheduler

`archive_window_days` decides **what** is archived. `archive_slots` (M) decides **how
fast**. They are different questions, and an earlier draft conflated them into one
number, which is what made `max_feed_items` mean two things and neither well.

**Everything inside the window is archived eventually.** M paces it: at most M backlog
VODs in flight per channel, most-recent-first, and a completed job frees a slot the
next item fills. Defaults: **3 days, 3 slots**.

**M paces the back-catalogue, not the channel.** Newly-discovered content bypasses M
entirely and never counts against it. M exists so an archival backlog cannot flood a
channel; it must never make a live stream, a premiere, or a just-published VOD wait
behind old content. This is what keeps M defensible as a default of 3: it is not a
throttle on the channel, it is a throttle on history.

### A `Queued` job is a durable DB row, not a blocked goroutine

The monitor creates it and stops. A scheduler admits it — enqueues it into `JobQueue` —
only when a slot is actually free. Three problems dissolve at once:

| Problem | Why it is gone |
|---|---|
| `maxLifecycle = 100` (`queue.go:61`) caps alive jobs, and a slot-waiter holds one the whole time it blocks | a `Queued` job is never in `JobQueue`, so it holds nothing. The queue only ever contains new content plus admitted backlog — bounded as today |
| the pending queue **silently drops** jobs past 100 (`queue.go:93-98`) — an unarchived video with only a warning log | the backlog never reaches the pending queue. This one mattered most: it is this spec's own failure mode, re-entering from the worker side |
| `AcquireDownloadSlot` (`queue.go:149-180`) has no FIFO and no priority — waiters wake in Go runtime order, so "most recent first" does not exist | admission ordering lives in the scheduler, where it is an `ORDER BY`. The semaphore stops being the arbiter and becomes a safety net |
| a new `JobStatus` strands on crash, as `worker.go:306-312` already shows for interrupted `Muxing` | `Queued` is a **resting** state, not a transient one. It survives restart because it was never in memory — the scheduler simply re-reads it. No recovery special-case |

**Admission rules:**

```
upcoming / live  → admit immediately, always. No M, no download pool.
                   A broadcast cannot be throttled — it is happening now.
new VOD          → no M gate; still needs a download-pool slot.
                   Sorts AHEAD of all backlog.
backlog VOD      → M per channel, then a download-pool slot. Most-recent-first.

"new" = first seen by the FEED MONITOR this cycle. NOT a backfill row: a scan
writes thousands of rows with first_seen=now, and none of them are new content.
```

**Every VOD waiting on the pool shows as `Queued` — new or backlog.** "Skip the queue"
is **priority, not exemption**: a new VOD jumps the entire back-catalogue and starts
the instant a slot frees, but `num_parallel_downloads` is a hard resource limit and
nothing bypasses it. This also kills an existing lie rather than working around it:
today `stream_processor.go:220`/`:238` write `Downloading` *before*
`AcquireDownloadSlot` blocks at `worker.go:446`, so a job waiting on the pool is
persisted as downloading while it does nothing — byte-identical to one actively pulling
segments. Only `download_started_at` (`orchestrator.go:123-124`) separates them, and no
UI reads it. One waiting state, one truthful status, everywhere.

### `num_parallel_downloads` becomes VOD-only, default 2 → 10

**The bug this fixes is live on today's build.** `AcquireDownloadSlot` is called for
*every* job at `worker.go:446` — live included — with no timeout and no priority. Two
long streams fill the default of 2, and a third channel going live blocks
**indefinitely**. That is a missed stream from a stock config, and it is goal 3 failing
one layer below where this spec was looking.

So live and upcoming are exempt from the pool entirely, and therefore unbounded —
because they must be.

**The default moves 2 → 10** (`config.go:67`). There is **no maximum** to raise —
validation only rejects `< 1` (`config.go:559-560`). And the key is *commented out* in
`config.example.toml:104`, so installs that never touched it inherit the default: this
is a live behavior change on upgrade, not just for new installs.

Peak concurrency becomes **(live streams) + 10** where it was a hard 2. Twenty channels
with five live and a backlog running is fifteen concurrent downloads. That is the
correct direction: `2` was only ever defensible because it was double-counting live
streams it should never have touched. As a VOD-only archival knob it is far too low —
and with M = 3 per channel, saturating 10 takes four channels' backlogs at once. Live
concurrency was always dictated by reality rather than config; this stops pretending
otherwise. **The help text must state the peak**, so it is chosen rather than
discovered.

### The two settings

| | `archive_window_days` | `archive_slots` |
|---|---|---|
| Answers | what is archived | how fast |
| Default | 3 | 3 |
| Scope | per channel | per channel |
| Bounds | the probe walk and the archival set | in-flight backlog jobs only |
| Applies to new content | yes (it is inside the window by definition) | **no** — new content bypasses M |
| Applies to upcoming/live | they are always inside (`published = now`) | **no** — never throttled |

**With `include_non_live_content = false` (the default), VODs never job at all.** The
window and M then govern only the `was_live → live` safety walk — a pass that creates
no jobs, so M has nothing to gate. No special case is needed: M gates job creation, and
there are none. The window still decides how far back we look for a restarted stream,
which is the right knob for it.

**`max_feed_items` is dropped, not migrated.** It bounded depth; M bounds concurrency;
the window has no old counterpart. Carrying the number forward preserves a shape rather
than an intent — and mapping `1000` onto a per-channel slot count would read as
"archive deep" and behave as 1000 simultaneous downloads. Every install takes the new
defaults.

## What `history` comes to mean

Removing the feed path's hidden `AddToHistory` calls (`utils.go:284/313/330`) is a
**population** change, not a schema one — the table, its API, the orphan overlay and
Twitch/DECAPI writers are all untouched.

Today `history` conflates three things: *we jobbed this* (`monitor_callbacks.go:260`),
*we looked at this and declined* (`nonLiveSkipReason` skips), and *we gave up probing
this* (give-up). Only the first has a job row — which is exactly why
`ListOrphanedHistory` finds anything at all, and why an operator eventually purges 80
rows and re-arms content that was never supposed to come back.

On the feed path it now means only the first: **we created a job for this video.** That
is what "already acted on" should have meant, it makes `HasProcessed` a precise
predicate, and it means far fewer orphans are manufactured. DECAPI and Twitch keep
writing exactly as they do today.

This composes with the window into defence in depth: purging history can no longer
re-arm an out-of-window VOD, *and* there is much less spurious history to purge.

## Part 2 — Full-Catalog Backfill

### Current capability

No continuation paging exists on any YouTube **channel/browse listing** path — every
channel path is first-page-only. The only occurrence of `continuationItemRenderer`
under `internal/youtube/` is a test fixture (`channel_membership_test.go:27`) proving
the walker ignores it.

**But continuation paging is not new to this codebase.** `internal/chat` already does
full InnerTube continuation paging and is the in-repo pattern to follow rather than
porting yt-dlp from first principles:

- `internal/chat/api.go:172-178` — `FetchLiveChat`/`FetchChatReplay(ctx, continuation)`
- `internal/chat/downloader.go:412-423` — the paging loop
- `internal/chat/downloader.go:558-583` — stale-token recovery

yt-dlp's `_tab.py` remains the reference for the *browse-specific* response shape, but
the transport, retry, and loop mechanics should mirror `internal/chat`, which already
works in production against the same API.

Reusable as-is from `internal/youtube/channel_membership.go`:

- `extractYtInitialData` (`:295`) — tab-agnostic brace-balancing scanner
- `ytInitialTabs` envelope (`:114`) — lazy `json.RawMessage` tab bodies
- `walkVideoRenderers` (`:176`) — handles `lockupViewModel` and classic renderers
- `lockupTitle` (`:261`), `rendererTitle` (`:267`), fetch/cookie/header pattern

Membership-specific and needing parameterisation: the `TAB_ID_SPONSORSHIPS` constant
(`:22`) and its match (`:150`), the `/membership` URL literal (`:75`), the
`HasAuthCookies()` early return (`:65`), and the inverted `hasAccess` semantics.

The InnerTube transport already exists (`player_api_strategy.go:346`, `auth.go:53`
`GenerateAPIHeaders`, `constants.YouTubeURLs.API`) but nothing calls `/browse`.

### Scan

Union `/videos` + `/streams` + `/membership`, dedup by video ID. This is deliberately
broader than yt-dlp, which omits the membership tab from "all uploads"
(`_tab.py:2318` carries `XXX: Members-only tab should also be extracted`).

Paging ports yt-dlp's `_entries` loop (`_tab.py:571`), including:

- `seen_continuations` loop detection (`_tab.py:585-590`)
- `visitorData` re-extraction per page (`_tab.py:608`)
- `appendContinuationItemsAction.continuationItems` unwrapping (`_tab.py:628-631`)
- token extraction from
  `continuationItemRenderer.continuationEndpoint.continuationCommand.token`
  (`_base.py:1041`, unwrapped by `_extract_continuation_ep_data` at `_base.py:1027`)

`UC`→`UU` uploads-playlist swap (`_tab.py:2326`) is the fallback for channels lacking
tabs.

### Classification during scan

Classification **calls `itemAge` itself** — badge check included — rather than
re-deriving a rule from its regex:

```
age := itemAge(item)          // live-badge short-circuit FIRST, then the age regex
age > 0  ⇒ status='vod',     date_precision='coarse'   (skewed NEW: now - age)
age == 0 ⇒ status='unknown', date_precision='assumed'  → probed
```

**The live-badge short-circuit is load-bearing and must not be dropped.** An earlier
draft specified "presence of a relative-age text, and nothing else":

- `itemAge` checks the live badge **first** and returns `0` immediately
  (`channel_membership.go:226-231`), with the comment *"Currently live → 'now',
  **regardless of any 'streaming for N' elapsed text**"*. The guard exists precisely
  because live items carry elapsed text.
- `relativeAgeRe` (`:49`) is matched against the **serialized JSON of the whole item**,
  so a live renderer's `"Started streaming 2 hours ago"` matches it.

A bare-regex rule would therefore insert a **live stream** as a terminal `vod` — on
`/streams`, the tab most likely to list one. `status` is excluded from the upsert's
`DO UPDATE`, and `vod` is never discovery-probed, so nothing would ever correct it.

An earlier draft classified from badges (`live badge ⇒ live`, `scheduled ⇒ upcoming`,
`"Streamed N ago" ⇒ vod`) and claimed it left "nothing in the unknown pool". Two
defects: a plain upload matched **none** of the three rules (no `"Streamed"` text, no
badge — most of `/videos` on most channels), and badge-derived *terminal* status means
one DOM change silently marks live streams terminal, never to be probed again.

Calling `itemAge` fixes both. `relativeAgeRe` matches a bare `"3 weeks ago"` — the
`"Streamed"` prefix is not required — so a plain upload dates correctly as
`coarse`/`vod`. And the badge short-circuit keeps a live item at `0` even though its
renderer carries elapsed text, so it lands in `unknown` and gets probed.

**The safe-side property holds only because of the badge check**, not because of the
age regex: `itemAge` returns `0` for a live item, an upcoming item, and anything it
cannot parse — and every one of those is probed. A wrong answer costs a probe; it
cannot cost a stream.

`vod` versus `not_a_stream` is not worth distinguishing here: both are terminal, both
are gated by `include_non_live_content`, and the archival pass treats them identically.
The backfill writes `vod`; a later probe refines it if it ever matters.

**Probe volume stays bounded.** Only rows where `itemAge` returns `0` land in `unknown`
— live, upcoming, and unparseable items — and the source walk retires each source at
its first out-of-window probe regardless.

### Assigning catalog_pos

`catalog_pos` must be **channel-global**, not per-tab. A per-tab index is unusable as a
tiebreaker because the three tabs are unrelated coordinate systems and
`/videos`/`/streams` overlap heavily — a past stream appears in both at different
positions, and the row is deduped by `(channel_id, video_id)`, so a per-tab value would
be permanently whichever tab scanned first. (The precision guard makes this worse, not
better: both tabs write `coarse`, so the second write is rejected outright.)

```
1. scan /videos, /streams, /membership page by page
     └─ write each page's rows IMMEDIATELY, catalog_pos = provisional per-tab index
     └─ advance the per-tab cursor in channel_state.backfill_state
     └─ EARLY EXIT: if backfilled_at IS NOT NULL and a full page yields no new
        video IDs, stop paging this tab (a re-run; the rest is already stored).
        NEVER early-exit when backfilled_at IS NULL — see below
2. when all eligible tabs are exhausted, run one ORDERING PASS:
     └─ SELECT the channel's rows into a slice, CLOSE the cursor
     └─ sort by (published DESC, provisional pos ASC, video_id ASC)
        (video_id makes this a total order; an earlier draft had a "source rank"
         term here but never defined one, leaving the step unimplementable)
     └─ UPDATE each row's catalog_pos = 0..n-1
        (an unguarded UPDATE — the precision guard governs the upsert only, and
         would otherwise reject a coarse backfill write onto a pre-existing exact
         RSS row, leaving the merged position permanently unapplied to exactly the
         recent rows where it decides archival)
3. set backfilled_at
```

Rows with no real `published` (live/upcoming/unparseable, which enter as `assumed`/now)
sort by that `now` and land at the top of the merged order — which is where they belong:
they are always in the window and always probed.

### Early exit — only on a re-run, never on the first scan

A re-run should not re-page a channel's entire back catalogue to discover what it
already knows. So a tab **stops paging once a full page yields no new video IDs**.

A full page of known IDs is the threshold rather than a single known ID, because
YouTube reorders shelves and interleaves; one familiar item proves nothing about the
next page. A whole page does.

**The trap: early exit must be gated on `backfilled_at IS NOT NULL`.** On a *first*
scan the store is not empty — RSS and membership have been populating it since Phase 1,
so `/videos` page 1 is very likely *entirely* known. A naive early exit would fire on
page 1, declare the tab exhausted, and set `backfilled_at` having scanned nothing —
permanently marking the channel complete with no catalogue behind it, and no sweep would
ever revisit it (`backfilled_at IS NULL` is the only trigger).

```
backfilled_at IS NULL      → full scan, no early exit (the store proves nothing)
backfilled_at IS NOT NULL  → early exit allowed (the catalogue is already complete)
```

**Rows are written as they are scanned, not buffered to the end.** The obvious
formulation — scan everything, merge in memory, then write — is incompatible with
"resumable via cursor": a restart mid-scan would destroy the buffer, so the cursor would
resume into a merge whose earlier half no longer exists.

The cost is that `catalog_pos` is provisional until the ordering pass runs. That is
harmless: a partially-backfilled channel's un-ordered rows are old, coarse-dated content
outside the window, and `backfilled_at` stays NULL until the ordering pass completes.

**The ordering pass must collect-then-update.** With `SetMaxOpenConns(1)`
(`internal/database/database.go:177`), issuing UPDATEs while a SELECT cursor is open
deadlocks on the single connection — the hazard documented at `migrations.go:242-244`.
Read rows into a slice, close the cursor, then write.

Steady-state inserts (RSS, membership) keep using the per-fetch index, which is sound
there: RSS dates are `exact` and distinct, so they effectively never tie, and a
membership fetch is a single listing whose internal order is genuine recency.
`catalog_pos` only ever arbitrates *within* one `published` value.

**Known limit, accepted:** a tie between a backfilled row (global position) and a later
steady-state row (per-fetch position) is resolved deterministically but arbitrarily. It
requires identical `published` values across a coarse backfill row and a coarse
membership row, and it only changes ordering *within* a lump. Not worth a second
coordinate column.

### Operational rules

- **YouTube channels only.** The backfill must re-apply the platform filter
  (`ch.GetPlatform() == "youtube"`, as `getYouTubeChannels` does at `feed.go:809-823`).
  Twitch channels live in the *same* `[[channels]]` list, and this worker runs on its
  own path rather than through the monitor's cycle loop — so nothing else filters for
  it. Without this, a Twitch channel would be scanned as
  `youtube.com/channel/<twitch_login>/videos`, 404 on all three tabs, never set
  `backfilled_at`, and retry on every startup forever. The "no Twitch-side change"
  non-goal is only a guarantee if this filter exists.
- Throttled: one page per interval (constant, not config).
- **Strictly serial across channels — one channel scanned at a time, globally.** On
  upgrade day *every* YouTube channel has `backfilled_at IS NULL`, so the sweep fires
  for all of them at once; a single global queue is what stops that becoming a request
  storm. The numbers are not small: a 2,000-video channel is roughly 65 pages × 3 tabs
  ≈ 200 requests, and twenty channels ≈ 4,000 — aimed at an endpoint we already observe
  failing ~13% of cycles. Concurrency would multiply the rate exactly where it hurts,
  and backfill failures are silent by design (cursor saved, `backfilled_at` stays NULL),
  so throttling would present as slowness rather than as breakage.
- **Wall-clock is allowed to be hours, and that is not a defect.** The monitors keep
  running on RSS throughout; the store deepens underneath them. Nothing waits on the
  sweep except the established gate on a channel that has no working RSS, which had no
  path at all before this.
- **Never gated on idleness.** A tempting variant pauses paging while downloads or
  monitor cycles are active. On a 24/7 install — the target deployment — it may never
  finish, which leaves the established gate shut on dead-RSS channels forever: the exact
  hole that shipping both phases together exists to close.
- Resumable: per-tab cursor in `channel_state.backfill_state`. A restart mid-sweep costs
  one page, so serial + slow carries no risk of lost work.
- `backfilled_at` set **only** on full completion of all eligible tabs.
- Runs in its own throttled path — **not** through the monitor's cycle loop, which does
  retries and backoff per video and would be pathological over thousands of items.
- Progress surfaced in Web UI and TUI.
- Inline `defer recover()` in the scan goroutine, per project rule.

## Integration Points

### Trigger: there is no config watcher — `kickMonitors` is the hook

There is no file watcher and no `fsnotify` dependency; `internal/config/store.go`
exposes only `Read`/`Snapshot`/`Update`/`SaveLocked`. "Hot-reload" means *callbacks
fired by the writer*. The single funnel for every runtime channel mutation is
`s.kickMonitors` (`cmd/moombox/services.go:568-577`), called from
`channel_routes.go:80-82` (add), `:121-123` (delete), `:186-188` (reorder),
`config_routes.go:743-744` (bulk PUT), and `tui_wiring.go:222` (TUI settings, which
bypasses `ChannelRoutes` entirely).

**`kickMonitors` is a bare `func()` with no add/remove/reorder discrimination** — it
fires identically on a reorder. So "auto on channel add" **cannot** be event-driven. It
must be an idempotent sweep keyed on `backfilled_at IS NULL`, invoked from
`kickMonitors` and at startup.

**Idempotent at the DB level is not idempotent at the worker level.** `backfilled_at` is
set only on *completion*, so a channel whose scan is in-flight still reads
`backfilled_at IS NULL`. A user reordering channels three times would launch three
concurrent scans of the same tabs. The 30s debounce specified for the manual re-run does
not help; it lives on the POST route, not in the sweep.

The sweep therefore holds an **in-flight set** (channel ID → cancel func), owned by the
backfill worker, and skips any channel already scanning. The in-flight set is also what
makes cancellation-before-prune possible — one structure serves both.

`setup_routes.go:176-177` deliberately does not fire the channel-change callback (the
process restarts instead), so setup-wizard channels are covered by the startup sweep.

### Channel removal must prune, and there is a precedent

`feed_items` is per-channel state on a 24/7 process, on disk, unbounded per channel, and
it survives restarts. The established pattern is `PruneHealth` — `health.go:110-112`
states it exists so the map "can't grow unbounded on a 24/7 process", implemented at
`feed.go:151-158` and called from `services.go:571-573`.

So the same `kickMonitors` sweep deletes `feed_items`/`channel_state` rows for channels
no longer in the active set. This also fixes a real bug: a removed-then-re-added channel
would otherwise inherit a stale `backfilled_at` and silently skip its backfill.

**Caveat:** this is the **first channel-keyed DB cleanup in the codebase**. Nothing in
`internal/database/` deletes by channel today — `ListOrphanedHistory` joins on
`jobs.id`, and `pruneHistory` is a global 10k FIFO. We are establishing a pattern, not
extending one.

**The prune races the backfill, and both arrive via `kickMonitors`.** Removing a channel
mid-scan means the prune deletes its rows while the scan is still writing them —
resurrecting a channel that no longer exists, with a half-written catalog and a NULL
`backfilled_at` that no sweep will ever revisit.

Ordering fixes it: `kickMonitors` **cancels any in-flight backfill for channels leaving
the active set, waits for the worker to observe cancellation, and only then prunes.** The
backfill worker additionally re-checks membership of the active set before each page
write, so a cancellation that lands between the check and the write costs at most one
stale page — which the prune then removes, because the prune runs last.

This is why the prune belongs in the sweep rather than the delete route:
`channel_routes.go:121-123` fires `kickMonitors` *after* the config is written, so the
active set is already authoritative when the sweep reads it.

### The backfill must respect `membership_discovery` and the auth gate

**The archival pass reads the store, not the response.** If the backfill writes
members-only rows on a channel with `membership_discovery = false`, those rows enter the
window and get jobbed — the toggle silently defeated by the very indirection that fixes
the original bug.

So: the backfill's membership tab is gated on `MembershipDiscoveryEnabled() &&
HasAuthCookies()`. `/videos` and `/streams` are public and are not gated.

**`backfilled_at` semantics when the membership tab is skipped:** it is still set on
completion of the tabs that were *eligible*. The flag means "this channel's catalog has
been scanned as fully as its configuration permits" — otherwise a
`membership_discovery = false` channel could never complete a backfill and would retry
forever. Turning membership discovery **on** later must therefore clear `backfilled_at`
to force a rescan, since the eligible set changed.

### Progress: two mechanisms, and an initial-state trap

- **Web:** the generic `hub.Broadcast(msgType, payload)` (`internal/web/websocket.go:441`).
  No typed wrapper needed — `disk_status` (`cmd/moombox/main.go:676-681`) and
  `update_available` (`helpers.go:142`) already use the generic path.
- **TUI:** not WebSocket. A buffered channel plus `tea.Msg`, modelled on
  `DiskStatusMsg`: type at `internal/tui/app.go:60-64`, channel at `services.go:609`,
  non-blocking send with a `default:` drop at `main.go:684-689`, wired via
  `SetUpdateChannels` (`tui_wiring.go:404`), handled at `app_update.go:277-279` — where
  the trailing `return a, a.listenForUpdates()` is **mandatory** to re-arm the receive.

**The trap:** `InitialState` (`cmd/moombox/ws_wiring.go:87-111`) returns only `jobs`,
`logs`, `nextFeedCheck`, `nextDecapiCheck`, `nextTwitchCheck`, `connectivity`,
`hideFinishedAgeDays`. `disk_status` is absent, so a web client connecting mid-event
renders nothing until the next tick. The TUI seeds itself (`tui_wiring.go:409-415`); the
Web UI does not. A throttled full-catalog backfill is long-running *by design*, so
mid-flight connect is the **common case**: backfill progress must be added to
`InitialState` and the TUI seed mirrored.

### Manual re-run: `R` chord + debounced route

- **Chord `R` (Request)** — the global namespace (`A` is job-scoped). Model on `R M`
  "Check Monitors Now": callback field `internal/tui/app.go:411`, menu registration
  **gated on non-nil** (`app_actions.go:489-491` — an unregistered chord is rejected by
  the parser at `:492-495`), dispatch at `:185-189`, host wiring at
  `cmd/moombox/tui_wiring.go:225-229`. For an async trigger use the `R V` shape
  (`app_actions.go:173-184`): capture the fn and return `safeCmd(...)` producing a
  one-shot msg — never block the update loop.
- **API** — model on `POST /api/monitors/check-now`
  (`internal/web/routes/monitors.go:34`), which carries a **30s `atomic.Int64` debounce**
  (`monitors.go:22`, `:39-51`) returning HTTP 200
  `{"success":false,"debounced":true,"retryAfterMs":N}`.

Both front doors share one service func, exactly as `kickMonitors` does today.

Note: `buildMenuItems()`/`dispatchAction()` live in `internal/tui/app_actions.go:430`/`:18`,
**not** `app.go` as CLAUDE.md claims — see Included Fixes.

## Error Handling

| Failure | Behavior |
|---|---|
| RSS 404/500 | Membership still runs and still writes its items. No RSS-sourced rows are added, and previously-stored ones remain — so scope is unchanged. `last_rss_ok_at` keeps its prior value. **This is the bug's failure mode, now inert.** |
| Membership fetch fails | Debug log only — unchanged; never marks RSS unhealthy |
| Probe fails | Existing `MetadataTracker` give-up + `ProbeCooldown`, untouched |
| Backfill fails mid-scan | Cursor saved, `backfilled_at` stays NULL, retries next startup |
| DB error | Skip the item this cycle — existing `HasActiveJob`/`HasProcessed` pattern |

The probe-failure **machinery** is deliberately untouched: `MetadataTracker` and
`ProbeCooldown` behave exactly as today, and the cooldown default does not change.
`internal/monitor/utils.go:272-279` remains accurate that history does not stop
re-probing and the cooldown is the only limiter.

**A permanently unprobeable item still escapes, but for a stated reason rather than an
accidental one.** Today it scrolls out of RSS's 15-item window once 15 newer items
exist, and probing stops as a side effect of the response being the work list. Reading
the store removes that escape — the same property that fixes the original bug — so the
escape is re-established deliberately: the probe list is the window, and the item leaves
it when time passes.

## Testing

### Prerequisite: `checkChannel` is not testable today

The headline regression test cannot be written against the current code, and this is
implementation work the plan must budget for:

- **There is no `feed_test.go`.** `internal/monitor/` has `connectivity_test.go`,
  `health_test.go`, `membership_test.go`, `twitch_recover_test.go`, `utils_test.go` —
  nothing covers `checkChannel`.
- **There is no fetch seam.** `checkChannel` calls `fm.fetchFeed(ctx, ch)` directly
  (`feed.go:426`), which does a real HTTP GET (`feed.go:484`). RSS failure cannot be
  simulated. By contrast `FetchMembership` is already an injectable func field
  (`MembershipFetchFunc`, `feed.go:94`) — the RSS path needs the same treatment.

Fixtures stay **inline** — `internal/monitor/` has no `testdata/` directory and every
existing fixture is inline (`membership_test.go:207` inline XML;
`channel_membership_test.go:27` inline HTML). Do not introduce `testdata/`.

### Test to delete

`internal/monitor/membership_test.go:83-108` `TestMergeCandidatesRecencyCap` calls
`mergeCandidates(cands, 2)` and asserts precisely the cap-crowding behavior this design
removes. It cannot survive.

### Regression test for this exact bug

- **RSS 404 cycle + membership returns a 3-week-old VOD ⇒ not archived.**

*The window and the skew*

- **Coarse dates skew new:** `"1 week ago"` stores `now - 7d`, not `now - 14d`, so the
  whole [7d, 14d) bucket is admitted to a 7-day window rather than excluded
- **A straddling item is admitted, probed, and dropped on its exact date.** 7-day
  window, item displayed `"1 week ago"`, probe returns a true age of 10 days ⇒ **never
  jobbed** — and the exact date is written back, so it leaves the window and is not
  re-probed next cycle. This is the refresh-probe window re-check; without it the item
  is archived on a guess (goal 4)
- **Nothing is excluded on an unverified date.** An item displayed `"2 weeks ago"`
  (stored `now - 14d`) is outside a 7-day window and must **never** be probed
- **`itemAge` is unchanged** — pin `"1 week ago" → 168h` (the truncated lower bound)
  against a well-meaning "fix" to the unit-preserving form the rejected skew-old rule
  needed
- Widening `archive_window_days` brings an already-stored VOD into scope with **no**
  discovery probe (pure re-scope), and the resulting job **does** follow a refresh probe
- `published` frozen at first insert; upgraded only by higher precision
- Precision guard: a later `coarse` write never overwrites a stored `exact`; a later
  `exact` does overwrite a stored `coarse`. All four rungs ordered
- A scheduled start time is never stored as `published`
- An `assumed` row sits inside every window and is probed; an `unknown` row is never
  jobbed
- **Query plan assertion:** the window query uses `idx_feed_items_window` and does not
  fall back to a `SCAN` or a `USE TEMP B-TREE FOR ORDER BY`, on both arms

*The source walk*

- **Early exit fires:** a source with items at true ages 5d/6d/10d/11d/12d against a
  7-day window, all displayed within the straddling bucket, costs exactly **three**
  probes — 5d, 6d, then 10d retires the source. The 11d and 12d rows are **never probed**
- **Only a dated probe retires a source.** Four tests, one per outcome: `errored`,
  `denied`, cooldown-skipped, and probed-with-no-date each leave the source live and the
  walk continues. A transient cookie fault must not truncate a source
- **The ordering check fires and degrades safely.** A source whose probed dates go
  8d → 7d disables early exit for that source, logs, and probes the remainder
- **The check is per-source, not global.** A mis-ordered `/streams` must not disable
  early exit for `/videos`
- **Exhaustion never persists.** A source retired in cycle N is walked again in N+1
- **RSS rows never retire a source.** They carry `exact` dates no probe moves
- **Probes are serial.** Assert call *ordering*, not just count: a concurrent
  implementation passes a count assertion while issuing every probe past the boundary

*The probe date and the terminal invariant*

- **`PublishedAt` extraction is status-aware.** `post_live`/`vod` take
  `liveBroadcastDetails.startTimestamp` (`exact`), **falling back to `uploadDate`
  (`day`) when `lbd` is absent** — the *normal* `vod` case; `not_a_stream` takes
  `uploadDate` (`day`); `upcoming` stores **no** date — its future `startTimestamp`
  never reaches `published`
- **The probe write is precision-guarded.** A probe of a plain upload (`uploadDate`,
  `day`) must NOT overwrite RSS's `exact` date, and must never write `published=''`
- **`post_live` normalizes to `vod` on write** — no `post_live` row ever reaches the
  archival decision table, where it would match no branch
- **A probe returning `vod` with no usable date leaves the row `unknown`, not `vod`.**
  Guards the sink: `vod`+`assumed` sits inside the window forever claiming to be new,
  terminal so never discovery-probed, and on `include_non_live_content = false` never
  refresh-probed either
- An `assumed`/`unknown` members row that probes to `vod` gets a real date and drops out
  of the window if it is old
- A stale listing never demotes a probed `live` back to `vod`

*Freshness and the split*

- **`probeAndClassify` requires a wired probe** — the feed path has no passthrough
  outcome, so `outcome == probed` is FRESH without qualification. (Freshness must never
  be derived from `ShouldProcess`, which is `true` on the nil path.)
- **A successful probe of a non-jobbable item still writes its status.** Default config,
  RSS plain upload ⇒ probe succeeds ⇒ `not_a_stream` ⇒ `ShouldProcess=false` ⇒ the row
  must become `not_a_stream`, NOT stay `unknown`. Guards the goal-5 inversion
- `ShouldProcess` is never used by the feed path; DECAPI's behaviour is byte-identical
- **No hidden history writes on the feed path.** A plain upload probed on a
  default-config channel writes **no** history row, and a feed-path give-up writes none
- A DECAPI probe still writes history exactly as today
- **A DECAPI probe give-up writes no `feed_items` row**
- A stale stored `live` whose probe errored this cycle produces **no** job
- With `probe_cooldown > 0`, a cooldown-skipped item produces **no** job
- Job creation always follows a fresh probe, including via the archival pass
- **`HasProcessed` does not block a live/upcoming job.** Arrange a history row with **no**
  job row — via a DECAPI give-up or a pre-existing/legacy row, **not** a feed-path
  give-up, which no longer writes one — then let a discovery probe return `live`. The
  stream must still be jobbed. This is the goal-3 regression guard
- A stored `vod` whose refresh probe returns `live` (stream restarted on the same ID)
  **is** jobbed, and the store is updated to `live`
- Clearing orphaned history for an in-window VOD re-jobs it; clearing it for an
  out-of-window VOD does **not**

*`denied` and the escalation*

- **A denied probe never becomes a job, whatever `source` says.** `membership_discovery
  = false`, a public video locked to members after we stored it (`source` stays `rss`
  forever), then the window is widened so it enters scope. The anonymous refresh probe
  returns `upcoming` + `members_only` ⇒ `denied` ⇒ no job
- **A genuine upcoming stream is NOT denied.** `LIVE_STREAM_OFFLINE` ⇒
  `PlayabilityError == ok` (`:349-351`) ⇒ `probed` ⇒ FRESH ⇒ jobbed. Guards goal 3
  against over-eager denial, and against the tempting "past `published` + `upcoming` =
  contradiction" heuristic, which an RSS-announced stream legitimately trips
- **An age-restricted VOD that returns formats is NOT denied.** It classifies `vod`, so
  the grounded-classification rule applies. Guards against "any non-`ok` ⇒ denied"
- **`source='membership'` selects the authenticated probe**
- **A public video that becomes members-only is archived without any membership
  listing.** Anonymous probe returns `members_only` ⇒ `source` flips ⇒ escalate ⇒
  authenticated probe returns `ok` ⇒ **jobbed**. This is the case the listing can never
  reach
- **A failed escalation still flips `source`.** Wrong tier ⇒ escalated probe also
  `members_only` ⇒ `denied` ⇒ but `source='membership'` persists, so next cycle issues
  **one** probe, not two
- **An anti-bot `login_required` on a PUBLIC video must neither relabel nor escalate.**
  Assert `source` stays `rss` **and** no second probe is issued
- **The escalated re-probe is not suppressed by `probe_cooldown`**
- **The escalated result is classified by the normal outcome rules** — a `vod` +
  `members_only` escalated result is **trusted**
- **No cookies ⇒ no escalation, and eventually no probe at all**
- **`membership_discovery = false` ⇒ no escalation.** The refusal still sets `source`
- A members video that becomes public flips `source` to `rss` and is probed anonymously

*Membership gating*

- **A cookie lapse must NOT move scope.** Members rows in the window + `HasAuthCookies()`
  false for one cycle ⇒ they stay in the window and **no** job changes. Gating the read
  on `membershipActive()` fails this — and is this spec's own bug through a new door
- **A cookie lapse must not create a job either.** Assert **no probe is issued** — the
  job assertion cannot distinguish gated from ungated, and would pass vacuously
- **`membership_discovery = false` hides stored members rows from the window**, takes
  effect immediately on rows already stored, and turning it back ON restores them
- `membership_discovery = false` ⇒ backfill writes no members rows

*The scheduler*

- **New content bypasses M.** M=1 with a full backlog ⇒ a newly-discovered VOD still
  admits ahead of every queued row
- **Live/upcoming bypass M and the download pool.** `num_parallel_downloads=1` saturated
  by a VOD ⇒ a stream going live still downloads immediately. This is the stock-config
  stream-miss guard
- **A backlog VOD waits as `Queued`, not as `Downloading`** — assert the DB status of a
  pool-blocked job
- **`Queued` survives a restart** — it is a resting state, re-read by the scheduler, not
  reset like interrupted `Muxing`
- **A completed job frees a slot and the next backlog item is admitted, most-recent-first**
- **The backlog never enters `JobQueue`** — 300 windowed VODs must not trip
  `maxLifecycle` or the 100-job pending drop
- `include_non_live_content = false` ⇒ a past members VOD is stored, walked, but never
  jobbed; M gates nothing

*Backfill*

- Backfill skips Twitch channels entirely
- Rows persist per page, so a restart mid-scan resumes from the cursor;
  `catalog_pos` is only final once `backfilled_at` is set
- **Early exit fires on a re-run and NEVER on a first scan.** A first scan of a channel
  whose store already holds its 15 newest RSS items must still page the whole catalogue
- Removing a channel mid-backfill cancels the scan before the prune, and leaves no
  resurrected rows
- Channel removed ⇒ its `feed_items`/`channel_state` rows are pruned; re-adding triggers
  a fresh backfill rather than inheriting `backfilled_at`
- A plain upload (no "Streamed" text, no badge, just "3 weeks ago") is classified
  `vod`/`coarse` — not left `unknown`, not probed
- **A LIVE item whose renderer carries elapsed text** (`"Started streaming 2 hours ago"`,
  which `relativeAgeRe` matches) is left `unknown` and **is** probed — not written as a
  terminal `vod`. Without this the `/streams` tab silently retires live streams
- Fixture-driven continuation paging, loop detection, resume-from-cursor

*Other*

- Established gate: fresh install, first cycle 404 ⇒ no past-content archival
- Term matching: an RSS-carried description matches in-cycle; a store-only re-evaluation
  is title-only. A non-term-matching item is stored but never probed
- Migration v15→v16 idempotent
- `max_feed_items` is removed from config and every UI; an existing value in TOML is
  ignored without error

## Migration (v16)

Current `schemaVersion = 15` (`internal/database/migrations.go:26`). Follow the
established pattern: a sequential `if version < 16 { ... return db.writeUserVersion(16) }`
block, `CREATE TABLE/INDEX IF NOT EXISTS`, tables also added to `createSchema`.

1. Create `feed_items`, `channel_state`, and the **single** index `idx_feed_items_window`
   (non-partial).
2. `DROP TABLE IF EXISTS last_videos`.
3. Remove `GetLastVideo`/`SetLastVideo` (`database_extras.go:126-148`) and
   `TestLastVideos` (`database_test.go:119`).
4. Make the legacy JSON importer ignore `lastVideos` (`database_jobs.go:723`) rather than
   write a dropped table.
5. Add the `Queued` job status — and place it in the lifecycle deliberately, since
   CLAUDE.md marks that a critical pattern. It is **not** terminal (`types.go:92-94`) and
   it **is** `ShouldProcess` (`queue.go:350-357`). It also touches `calculatePriority`
   (`queue.go:19-30`), the `JobStats` aggregate (`database_jobs.go:622-645`, which today
   counts pool-waiters as active), and both UIs' colour maps and filter buckets.

**Config migration** (`migrateOldFormat()`, not the DB migration):

- `max_feed_items` is **dropped, not carried**. `archive_window_days` and
  `archive_slots` take their defaults of 3 and 3.
- `num_parallel_downloads` default 2 → 10 (`config.go:67`); the key is commented out in
  `config.example.toml:104`, so this reaches every install that never set it.

No data backfill inside the migration, so the `SetMaxOpenConns(1)` cursor hazard
(`migrations.go:242-244`) does not apply.

### `last_videos` removal justification

`GetLastVideo`/`SetLastVideo` have **zero non-test callers**. DECAPI — the suspected
consumer — makes exactly three DB calls: `HasActiveJob` (`decapi.go:543`),
`HasProcessed` (`decapi.go:565`), `AddToHistory` (`decapi.go:589`). Rows can only ever
arrive via the legacy JSON importer, and nothing reads them.

Not to be confused with `LastVideoSeq`/`last_video_seq`, a **different and very much
live** field: the download-resume segment counter used in `worker/orchestrator.go:270`,
`strategy_youtube_dash.go:243`, `twitch_recover.go:32`, and the TUI. That is untouched.

## Documentation Updates

`max_feed_items` is **removed**, and two settings replace it. Every site that names it
must be rewritten, not relabelled:

- `config.example.toml:69,208` — remove `max_feed_items`; document
  `archive_window_days` and `archive_slots`, and state that upcoming/live are never
  throttled
- `config.example.toml:103-104` — `num_parallel_downloads`: new default, and the help
  text must state the peak is `(live streams) + N`
- `docs/spec/data-and-storage.md:458,526` — the `MaxFeedItems` tables
- `docs/spec/data-and-storage.md:400` — wrongly claims `last_videos` "tracks the most
  recent video per channel for deduplication" (`:320-325` is the schema block above it;
  both go)
- `docs/spec/data-and-storage.md:337` — migration table; **add a v16 row**
- `docs/spec/data-and-storage.md:403` — `ImportFromJSON` description
- `docs/spec/data-and-storage.md:579,591` — migration table + `MaxFeedItems: min 1`
  validation doc
- `docs/spec/architecture.md:127` — describes `max_feed_items`
- `docs/spec/platform-services.md:178` — the `itemAge` cap description
- `SPEC.md:210,653`
- TUI help text `internal/tui/settings.go:90` — currently *"RSS items per feed
  (default: 15)"*
- TUI setup wizard `internal/tui/setup_wizard.go:113` — *"RSS feed items to check per
  channel"*
- Web UI: `web/public/index.html:795-800` (`cfg-max-feed-items`) and `:1682`
  (`setup-max-feed-items`)
- `internal/config/config.go:485` — the `O(N) per channel` per-tick comment
- The job status lifecycle in **CLAUDE.md** — `Queued` joins it

**In-code comments that assert the deleted model:**

- `internal/youtube/channel_membership.go:206-219` — *"it is ranked by that age so old
  VODs sink and get **crowded out of the cap**"*. The `MembershipVideo.Age` doc needs a
  *comment* change but **no signature change**: it must now state that the value is the
  truncated **lower bound** of the true age, which is what makes `now - Age` the
  newest-possible date the window relies on
- `internal/monitor/feed.go:660-669` — *"letting them **occupy shared cap slots** would
  only crowd out public videos that CAN be jobbed"*
- `internal/monitor/feed.go:415-424` — `checkChannel`'s merge/cap doc block

## Included Fixes

Pre-existing defects found while mapping this work, approved for inclusion.

**1. `max_feed_items` validation disagrees with itself — four client gates vs one server
validator.** This is now a *deletion*, not a reconciliation: the setting is gone, so all
six sites go with it.

| Site | Limit |
|---|---|
| `internal/config/config.go:490` (TOML load + validate — authoritative) | 1–1000 |
| `internal/tui/settings.go:544` (TUI settings) | 1–1000 |
| `internal/web/routes/config_routes.go:169` (Web API) | **1–100** |
| `internal/tui/setup_wizard.go:1066` (TUI first-run wizard) | **1–100** |
| `web/public/index.html:798` (`cfg-max-feed-items`) | **1–100** |
| `web/public/index.html:1682` (`setup-max-feed-items`) | **1–100** |

The accepted range depended on *which UI you happened to use*: `500` was valid via TOML
or the TUI settings screen and rejected by four other front doors. Recorded because the
replacement settings must not repeat it: `archive_window_days` and `archive_slots` need
**one** validator, in `config.go`, with every UI deferring to it.
`web/public/modules/setup.js:682` feeds `setup-max-feed-items` and goes too.

**2. `.claude/skills/moombox-database-migrations/SKILL.md` is stale.** It documents v6
(line 8), a `schema_version` table with `UPDATE schema_version SET version = 7` (line
27), and `tx.Exec` (lines 20-27). Reality is v15, `PRAGMA user_version` via
`writeUserVersion`, and direct `db.db.ExecContext` with no transaction wrapping the
migration. Anyone following it writes a broken migration. Update to match
`migrations.go`, and add the `SetMaxOpenConns(1)` collect-then-update constraint, which
the skill omits entirely.

While rewriting it, scope step 4 ("Update Field Maps") explicitly to the `jobs` table.
`fieldToColumn` (`internal/database/database.go:21`, consumed at `:356`) is jobs-only and
enforced by `TestFieldToColumnCoverage` (`database_test.go:1222`); the step currently
reads as unconditional, so the next reader adds `feed_items` entries to a jobs-only
allowlist.

**3. `CLAUDE.md` misplaces the chord system.** It states `buildMenuItems()` is "in
`app.go`". Both `buildMenuItems()` and `dispatchAction()` actually live in
`internal/tui/app_actions.go` (`:430` and `:18`). The instruction "one entry in
`buildMenuItems()` + one case in `dispatchAction()`" is still correct; only the file is
wrong. This matters here because Part 2 adds an `R` chord by following exactly that
instruction.

**4. Live jobs consume a download slot** (`queue.go:149-180` via `worker.go:446`), with
no timeout and no priority. See "The scheduler" — folded into this spec rather than
filed, because it loses streams on a stock config and M would queue on top of it.

## Implementation Phasing

**Both phases ship in one release.** The phases are build order, not release boundaries —
Phase 1 is landable and testable on its own, and should be, but it is not *shippable* on
its own.

The reason is the established gate. It opens on `last_rss_ok_at IS NOT NULL` **or**
`backfilled_at IS NOT NULL`, and only Phase 2 ever sets the second. Ship Phase 1 alone
and a channel whose RSS never succeeds never establishes, so it never archives past
content at all — including a members-only channel, whose RSS legitimately returns nothing
useful. That trades a wrong-archive (today's bug) for a **silent** no-archive, which is
the worse of the two: the operator can see a bad download and cancel it; they cannot see
an archive that never happened.

### Phase 1 — store, passes, scheduler

*Store*
- Schema v16: `feed_items`, `channel_state`, the single non-partial index
  `idx_feed_items_window`
- Precision-guarded upsert, **per-column**: `published`/`date_precision` move only
  upward; `source` updates on **every** sighting. Coupling them lets a date-quality rule
  pick the probe, and the wrong probe does not fail — it lies
- The window query, read by the discovery walk and the archival pass alike

*Monitor*
- Discovery pass / archival pass split, with the FRESH rule (`outcome == probed`)
- **The source walk**: serial, per-source early exit, self-validating ordering check
- `post_live` → `vod` normalization on write; the probe write is precision-guarded
- **The terminal-status invariant**: never write `vod`/`not_a_stream` without a rankable
  date
- The membership gate on **both** probe triggers (discovery and refresh)
- `source` as a **read** column: it selects the authenticated probe and gates the window
  query — the **read** arm on `MembershipDiscoveryEnabled()` (operator choice only), the
  **probe** arm on `membershipActive()` (which includes cookie state). The asymmetry is
  load-bearing: gating a read on cookie state moves scope on a fetch failure
- **Refusal escalation** (`members_only` only; cooldown bypassed). **Lives in the feed
  monitor, not `probeAndClassify`** — `ProbeVideoAuth` (`feed.go:131`) and
  `membershipActive()` (`feed.go:518-523`) are `*FeedMonitor` members, unreachable from a
  `utils.go` free function that DECAPI also composes
- Refresh probe on the archival `vod` branch, writing its result back **and re-checking
  the window on the probe's date**
- Established gate; rewired `checkChannel`/`processCandidate`
- **Channel-removal prune** from `kickMonitors`, mirroring `PruneHealth`. Needed in
  Phase 1: `feed_items` is populated by RSS/membership from day one. Phase 1's prune is
  the simple form — there is no backfill to cancel yet

*Worker*
- **Live/upcoming exempt from `num_parallel_downloads`**; default 2 → 10
- The `Queued` status and the scheduler that admits from it
- Most-recent-first admission ordering; new content ahead of backlog

*Config*
- `archive_window_days` + `archive_slots` (defaults 3/3), one validator in `config.go`
- `max_feed_items` dropped from config and all six UI sites

*Refactors this depends on* (none of which exist today — budget them)
- **Split `ProcessYouTubeVideo`** into `probeAndClassify` (feed path: probe + classify +
  tracker/cooldown; no history writes, no verdict) and the existing composed function
  (DECAPI only, unchanged)
- **`VideoProbeResult.PlayabilityError`** — `VideoInfo` already carries it
  (`types.go:17-28`) but `VideoProbeResult` (`utils.go:32-36`) drops it, so the monitor
  cannot tell an observation from a refusal. Surfacing it is what makes `denied` possible
- **A publish date on the probe** — `VideoInfo.PublishedAt` +
  `VideoProbeResult.PublishedAt` + both `monitor_callbacks.go` wiring sites, with
  status-aware extraction and the `day` rung. **No date exists in the probe chain today**,
  and under a window it is what adjudicates every straddling item
- **An injectable RSS fetch seam + a new `feed_test.go`**

*Cleanup*
- Delete `TestMergeCandidatesRecencyCap`
- Remove `last_videos` + `GetLastVideo`/`SetLastVideo`
- The included fixes; doc updates (including the in-code comments)

### Phase 2 — backfill

*Scanner*
- InnerTube `/browse` continuation client, modelled on `internal/chat` — **not** ported
  from yt-dlp, which is only the reference for the browse response shape
- Three-tab scan, unioned and deduped; membership gated on `MembershipDiscoveryEnabled()
  && HasAuthCookies()`; the whole scan filtered to `GetPlatform() == "youtube"`
- Listing classification via `itemAge` (badge short-circuit included)
- Merged channel-global `catalog_pos` ordering pass (collect-then-update)

*Worker*
- Throttled, **strictly serial across channels**, resumable; rows written per page;
  `backfilled_at` set only on completion of the eligible tabs
- Early exit on re-runs, **gated on `backfilled_at IS NOT NULL`**
- The idempotent `backfilled_at IS NULL` sweep, with an **in-flight set**
- **Cancel-before-prune**
- Debounced manual re-run (`R` chord + API)
- Turning `membership_discovery` on must clear `backfilled_at`

*UI*
- Progress via `hub.Broadcast` (web) + a TUI `tea.Msg` channel, **including** the
  `InitialState` seed — a long-running scan makes mid-flight connect the common case

### Explicitly not included

- **`last_listed_at` / row retirement.** Nothing left to bound — the walk is bounded by
  the window — and it cannot distinguish "unreachable" from "we stopped looking", so a
  cookie expiry would erode the members catalogue
- **The `assumed` probe carve-out.** It existed to repair rows the `assumed` ranking
  exclusion stranded; both are gone
- **Unit-preserving `itemAge`.** It existed only to compute the rejected skew-old rule
- **`classifyStream:454`.** Filed separately; `denied` contains it
