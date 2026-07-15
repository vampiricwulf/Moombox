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
   channel's content and the operator's configuration, never of what a given cycle
   happened to retrieve.
   It is deliberately **not** immutable. Scope moves when the *inputs* move, and
   that is the boundary working as intended: raising `N` widens it for content
   already stored; publishing N newer items pushes an item out; flipping
   `membership_discovery` off hides members rows from it. Each is an operator or
   channel action with a visible cause.
   What must never happen is scope moving because **a fetch failed** — that is the
   one input nobody chose.
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

**A probe that lacks what it needs does not fail — it lies. But it also tells us it
lied, and the design must read that.** This is the least intuitive fact here, and it
took four rounds and three separate patches to state properly.

`feed.go:743-746`: a probe of members-only content without cookies "gets no formats
and the classifier misfires it as **upcoming**". No error. And `upcoming` is
cap-exempt, so a lie is jobbed *unconditionally* — bypassing the cap and
`include_non_live_content` both. It then writes history, which blocks the real video
from ever being archived correctly. That is the 2.7.2 bug.

The natural safety argument — "no cookies ⇒ the probe fails ⇒ not FRESH ⇒ no job" —
is therefore **false**, and every rule below exists because it is false:

**The authoritative fix: distrust a probe result only when YouTube said it refused
us AND the classifier was guessing.** YouTube states outright whether we were allowed
to see the video, and `parsePlayabilityStatus`
(`internal/youtube/player_api_parsing.go:332-388`) already decodes it:

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

**The block above is the canonical `denied` predicate.** The table below *derives*
it, in this same section, under the reader's eye. Every use site elsewhere — the
outcome list, the flow pseudocode, the contract table, the tests — states only what
`denied` *does* and refers here for what it *is*. No use site writes the predicate.

That split is deliberate, and it is the fix for a defect this document generated five
times. An earlier draft wrote the predicate in five places. They drifted exactly as
duplicated rules always do: one copy was corrected to the minimal form while another
kept listing `age_restricted`/`…` as denied — a live goal-3 loss — and each round
patched the copy it happened to read. Two statements of one rule is two rules; five
is five.

Predicate here, behaviour at the use site. A reader coding the flow needs to know
`denied` writes nothing and retries; they do not need the predicate inlined, and
inlining it is what let the copies disagree.

**Why minimal, and why everything else is trusted — including `unknown`.** Two
independent reasons, both verified in code, and both fatal to the broader rules this
went through first:

**(a) `ok` does not mean "genuine" — it means `status` was literally `"OK"`.**
`classifyStream` reaches `StreamUpcoming` through five guards — `:429`, `:432`,
`:439`, `:448`, `:454` — and `:429` *early-returns* on `isUpcomingPlayability`. So
the four after it are reachable **only when playability did *not* say "upcoming"** —
and
`parsePlayabilityStatus` yields `ok` from exactly two sites
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

So a
refusal that *still carries live metadata* is a content-level refusal on known-live
content — the 2.7.2 signature exactly. A refusal carrying none (anti-bot / IP-block
`LOGIN_REQUIRED`, "Sign in to confirm you're not a bot", which returns no
`videoDetails`) falls to `:451` ⇒ `not_a_stream`, and the rule never fires on it.

**Known residual, accepted.** A members refusal phrased as `UNPLAYABLE` with none of
the matched keywords yields `unknown` (`:378`) ⇒ trusted ⇒ the lie survives. That is
the price of keeping `unknown` trusted, and (b) above is why the price is worth
paying.

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
radius (every strategy branches on `StreamStatus`), which is well outside this spec's
non-goals. `denied` is the cheap, local containment. File `:454` as its own issue; if
it is ever fixed, `denied` becomes redundant rather than wrong.

A `denied` result is not FRESH, writes no status, and creates no job — it is retried
next cycle, exactly like `errored`. This requires surfacing the field, which
`VideoProbeResult` does not carry today (see Phase 1).

**Why `upcoming` and not "any non-`ok`".** `classifyStream` only *guesses* on the
no-formats path — `!hasFormats && (lbd != nil || isLiveContent) ⇒ StreamUpcoming`
(`player_api_parsing.go:454`). Every other classification is grounded in formats
YouTube actually returned. So:

| Probe result | Meaning | Rule |
|---|---|---|
| `upcoming` + `members_only`/`login_required` | we were refused; the status is a guess | **`denied`** |
| `upcoming` + `ok` | genuine scheduled stream | trust it — **goal 3** |
| `upcoming` + `age_restricted` | reachable (`:361-363`, `:379-380`), and **trusted** — see below | trust it |
| `upcoming` + `unknown` | we could not read the answer | trust it |
| `vod`/`post_live`/`not_a_stream` + any playability | formats came back; the classification is grounded | trust it |

**`age_restricted` is trusted, and the list above is exhaustive — no "…".** An
earlier draft wrote `members_only`/`login_required`/`age_restricted`/… here, which is
"any non-`ok`" — the rule the paragraph below rejects, contradicting the rule stated
above. It is not academic: `PlayabilityAgeRestricted` comes from
`LOGIN_REQUIRED` + "age" (`:361-363`) and `AGE_VERIFICATION_REQUIRED` (`:379-380`),
neither of which satisfies `isUpcomingFromPlayability`, so an age-restricted premiere
carrying `videoDetails.isUpcoming` and no formats reaches guard `:439` ⇒ `upcoming` +
`age_restricted`. Denying it loses a real premiere forever.

A broader rule ("any non-`ok` ⇒ denied") would refuse content we can actually
download — an age-restricted VOD that returns formats classifies `vod`, not
`upcoming`, and the worker can fetch it. Denying that would be a self-inflicted
archival gap, trading one lie for one silence.

**`denied` creates no sink.** It writes nothing, so the row keeps whatever it had —
`unknown` (probed, per the status rule) or `assumed` (excluded from ranking, which is
correct: we still do not know its date). Permanently-denied content — age-restricted
we can never see, a members video we have no cookies for — therefore sits in the
retry pool, un-archived and un-ranked, and recovers the moment access returns. That
is the accepted re-probe cost from "Probe-list growth", not a new failure: it is
exactly the state of *knowing nothing*, which is what a refusal means.

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
| Gate the store reads on `membershipActive()` | it folds in **cookie state**, so the ranking moved on a fetch failure — the Jerry bug, reintroduced |
| Split: reads on config, probe on `membershipActive()` | the **refresh** probe was ungated, and `ProbeVideoAuth` has no cookie guard, so it did not fail — it lied, and the lie was jobbed |
| Gate the refresh probe too | `source` *picks* the probe, and a stale `source` lies identically |
| Update `source` on every sighting | `source` flips only on a **sighting**, and the fetch is gated — with `membership_discovery = false` it can never flip at all |
| **Read `PlayabilityError`** | ✅ depends on nothing we stored |
| (first draft: any non-`ok` ⇒ denied) | too broad — would refuse a downloadable age-restricted VOD |

Four consecutive fixes patched a symptom, each one correct about the case in front
of it and blind to the next. The signal that ends the sequence was in
`player_api_parsing.go` the entire time; `VideoProbeResult` simply dropped it. The
lesson generalises past this design: **when a guard needs a stored value to be
current, ask whether the source of truth can be read directly instead.**

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
| `history` table | Schema/API/UI untouched; the feed path writes **fewer** rows | Answers a different question ("acted on" vs "exists and when"); unifying would touch Twitch, DECAPI, orphan API, Web UI, TUI. See "What history comes to mean" — dropping the skip/give-up writes is a population change, not a schema change |
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
    date_precision TEXT NOT NULL,         -- 'exact'|'day'|'coarse'|'assumed' (ladder)
    catalog_pos  INTEGER NOT NULL DEFAULT 0,
    source       TEXT NOT NULL,           -- rss|membership|videos|streams
    status       TEXT NOT NULL,           -- unknown|upcoming|live|vod|not_a_stream
                                          -- probe 'post_live' NORMALIZES to 'vod'
    first_seen   TEXT NOT NULL,
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
higher-precision source (`assumed` → `coarse` → `day` → `exact`). It is never recomputed
from `now`, which is what makes ranking stable across cycles.

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

**The guard moved from `WHERE` to `CASE` deliberately.** A statement-level `WHERE`
gates the *whole* `DO UPDATE`, which would drag `source` along with the date rule —
and a stale `source` selects the wrong probe, which does not fail but **lies** (see
"`source` decides which probe to use"). Per-column `CASE` lets the two fields move
on their own schedules in one statement: `source` always, dates only upward.

The guard makes the write monotonic: a later, *worse* estimate can never overwrite
a better one, so ordering cannot regress no matter which source sees an item next.
This is reachable, not theoretical: the backfill records a 2-day-old stream as
`coarse`, RSS later carries an exact date for it, and being recent it is competing
for the top N — exactly where ordering decides whether it is archived.

Note `status` is deliberately **not** in the `DO UPDATE` set at all. Listing-derived
status is weaker than probe-derived status, and a stale listing must never demote a
probed `live` back to `vod`. (`source` is the opposite case: the listing is the
*only* authority on where the item currently appears.)

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
   -- AND source <> 'membership'   ← iff NOT MembershipDiscoveryEnabled()
   --                                 (the OPERATOR'S toggle only — never
   --                                  membershipActive(), which folds in live
   --                                  cookie state and would move scope on a
   --                                  cookie lapse)
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
CREATE partial index with `status NOT IN (...)`   → accepted

top-N (archival)
  SEARCH feed_items USING INDEX idx_feed_items_rank (channel_id=?)

discovery probe list (status IN (unknown,upcoming,live))
  SEARCH feed_items USING INDEX idx_feed_items_status (channel_id=? AND status=?)

cap-exempt union (status IN (upcoming,live))
  SEARCH feed_items USING INDEX idx_feed_items_status (channel_id=? AND status=?)
```

The `status=?` in the latter two is how SQLite renders an `IN`-list — it expands to
one index seek per value, not an equality mismatch.

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

### The probe has no publish date today — adding one is Phase 1 work

This spec repeatedly says the probe returns an authoritative date and promotes an
`assumed` row to a real one. **That data does not exist anywhere in the chain**, and
unlike `Outcome`/`StreamStatus` it is not something `ProcessYouTubeVideo` computes
and discards — it is simply absent:

- `internal/monitor/utils.go:32-36` — `VideoProbeResult{StreamStatus, Title, ChannelName}`
- `internal/youtube/types.go:46` — `VideoInfo` carries only `ScheduledStartTime`
- `cmd/moombox/monitor_callbacks.go:174-178`, `:193-197` — both wiring sites copy
  exactly those three fields

**Why this is blocking rather than cosmetic.** Without a probe date, an `assumed`
row's *status* can be written but its `published`/`date_precision` cannot. So:

```
members item → assumed/unknown → probed → becomes vod
             → now vod + assumed
             → vod is terminal        ⇒ never discovery-probed again
             → assumed is excluded    ⇒ never in the top-N
             ⇒ silently unarchivable, forever
```

That is members-only content — precisely the class this bug is about — and it also
disables the repair path the External Assumptions section leans on ("probes repair it
automatically"). They cannot repair what they cannot supply.

**Do not reuse `ScheduledStartTime` as-is.** `extractScheduledStartTime`
(`internal/youtube/player_api_parsing.go:113-138`) is a *conflated* accessor: it
returns `liveBroadcastDetails.startTimestamp`, else a `liveStreamability` epoch, else
microformat `uploadDate`/`publishDate` (`:129-135`). So it holds a genuine publish
date for a plain upload and a **future** timestamp for an upcoming stream — exactly
what the next section forbids storing. An implementer looking for "the probe's
publish date" will find it, and it will look right.

But the underlying data is better than that accessor implies, and the extraction
should take the **best** source per status rather than the safest single one:

```
status vod / post_live   → liveBroadcastDetails.startTimestamp   → precision 'exact'
                            (the stream's ACTUAL start; RFC3339, second-granular)
                         → ELSE uploadDate / publishDate         → precision 'day'
                            (fallback — see below; must not be skipped)
status not_a_stream      → microformat uploadDate / publishDate  → precision 'day'
                            (date-only, e.g. "2025-06-15")
status upcoming / live   → no ranking date stored
                            (startTimestamp is the FUTURE scheduled start here —
                             never store it; these statuses are excluded from the
                             ranking anyway, so nothing needs it)
```

**The `vod` fallback is not optional.** If a past stream's response carries no
`liveBroadcastDetails`, a rule that only reads `startTimestamp` yields no date at
all — and the row then keeps whatever precision it had. For a members item first
seen without an age text that is `assumed`, which reinstates the exact dead-end this
section exists to close: `vod` + `assumed` is terminal *and* unrankable. Falling
back to `uploadDate` costs nothing and removes the cliff.

**`post_live`, not `vod`, is the status that reliably has `startTimestamp`** — and
getting this backwards is what makes the fallback mandatory. `classifyStream`
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
demonstrates presence for `post_live`, not `vod`.

Nor is microformat guaranteed per client: `player_api_parsing.go:414-419` notes
"Some probes (notably ANDROID_VR on unpublished premieres) return this without a
full microformat", and the authenticated probe uses TV_DOWNGRADED
(`player_api_strategy.go:28-34`). So a members past stream can classify `vod` with
no `lbd` and no microformat at all.

Hence the chain, and the final rung:

```
startTimestamp → 'exact'   |  uploadDate/publishDate → 'day'  |  neither → leave as-is
```

**Invariant: never write a terminal status without a rankable date.** If the probe
classifies a past item but supplies no date, the row stays **`unknown`** — it does
*not* become `vod`.

Without this rule the chain has a permanent sink. A members row stored `assumed`
probes to `vod` with no date (no `lbd`, no microformat — a real case per the
analysis above), and the precision guard correctly declines to write a date it
wasn't given. The row is then `vod` + `assumed`: terminal, so never discovery-probed
again, *and* excluded from the top-N, so never archived. It can never recover, even
though the very next probe might have supplied a date. That is the dead-end this
whole section exists to close, reached by a different route.

Keeping it `unknown` costs a probe per cycle (the accepted cost — see "Probe-list
growth") and buys self-healing: the moment any probe returns a usable date, the row
takes it, becomes `vod`, and ranks normally. A terminal status is a promise that we
know enough to stop looking; without a date we do not.

**This invariant is exactly necessary and sufficient — given the probe carve-outs.**
Enumerate the full state space (5 statuses × 4 precisions) against the three rules:

```
discovery-probed  iff  status IN (unknown, upcoming, live)
                       AND (term-match OR date_precision = 'assumed')   ← see below
in the top-N      iff  date_precision <> 'assumed'
                       AND status NOT IN ('upcoming','live')
cap-exempt        iff  status IN (upcoming, live)
```

**The first premise is easy to state wrongly, and an earlier draft of this proof did
— which is how it missed a sink.** Written as the bare `status IN (unknown, upcoming,
live)`, it ignores the term gate at `feed.go:704-725`, and the enumeration then
"proves" that `unknown`+`assumed` is always fine because it is always probed. It is
not: a *non-term-matching* `unknown`+`assumed` row is never probed, never dated, and
therefore never rankable — a third sink, invisible to the proof because the proof
had already assumed it away. The `OR date_precision = 'assumed'` carve-out (see
"Descriptions and term matching") is what makes the premise true, and therefore what
makes the enumeration below sound.

With the corrected premise, exactly **two** states are neither probeable nor
reachable by any archival path:

| status | precision | probed | in top-N | cap-exempt | outcome |
|---|---|---|---|---|---|
| `vod` | `assumed` | no | no | no | **stuck** |
| `not_a_stream` | `assumed` | no | no | no | **stuck** |

Every other combination is either probed (so it can change **when the world
changes**) or rankable (so it can be archived).

**`denied` qualifies that premise, and the qualification matters.** A `denied` probe
*succeeds* at the HTTP level and is discarded, so a row denied every cycle — a
members video on a channel we hold no cookies for — is probed forever and never
changes. That is not a design sink: it changes the instant cookies arrive, which is
also the instant the content becomes archivable at all. It is excluded from ranking
(`assumed`), so it evicts nothing; it is never archived, which is correct, because we
have never seen it. The row is a faithful record of *knowing nothing*, and the only
thing that can end that is access — not a probe.

Reading the table back: `unknown`+`assumed` is fine — it is probed.
`upcoming`/`live`+`assumed` are fine — they are cap-exempt. The only sinks are a
terminal status paired with a fabricated date, and forbidding that pairing is
precisely this invariant. Nothing else needs guarding, and nothing less would do.

The `liveStreamability` epoch branch is never a publish date — it is the scheduled
start of an upcoming stream. It must not feed `PublishedAt` at all.

Phase 1 therefore adds a **distinct** field, with its own status-aware extraction:

```
VideoInfo.PublishedAt          (new, internal/youtube)
VideoProbeResult.PublishedAt   (new, internal/monitor)
+ both monitor_callbacks.go wiring sites
```

This is a cross-package signature change of the same shape as the unit-preserving
`itemAge` change, and it must be budgeted, not discovered.

**`uploadDate` is day-granular, so the ladder needs a rung for it.** Calling a
date-only value `exact` alongside RSS's second-granular `<published>` would
manufacture ties and undercut "RSS dates are exact and distinct, so they effectively
never tie". Four rungs:

```
assumed  <  coarse  <  day  <  exact
```

The upsert's precision guard ranks all four; the top-N excludes only `assumed`. Note
a VOD's `startTimestamp` is genuinely `exact` — `day` applies only to plain uploads,
which are the case where YouTube itself only publishes a date.

### `post_live` normalizes to `vod` on write

The probe returns five statuses — `internal/youtube/types.go:13` defines
`StreamPostLive = "post_live"` — but the store's enum has no `post_live`. That was an
omission, and per the analysis above it is not a rare one: `post_live` is the *common*
classification for an ended stream, since any stream with an `endTimestamp`
short-circuits there.

Left unstated, a probed `post_live` row would match **no** archival branch
(`upcoming/live`, `vod/not_a_stream`, `unknown`) while still passing both rank-index
predicates — in scope, ranked, and undecidable.

So the store **normalizes `post_live` → `vod` on write.** The store's vocabulary
exists to answer two questions — where does this rank, and should it be archived —
and `post_live` and `vod` answer both identically: today's `nonLiveSkipReason` is
reached from a single `case "post_live", "vod":` (`utils.go:326`). The distinction is
a *download-strategy* concern (DVR window, still-processing manifests), and the
worker re-probes for that anyway; it is not the store's business. Normalizing keeps
the enum at five values with every branch defined, rather than adding a sixth that
would be a synonym everywhere it appeared.

The date rule above is written in probe terms (`vod`/`post_live` → `startTimestamp`)
precisely because it runs *before* this normalization.

### `published` for upcoming and live rows

Never store the **scheduled start time**. It is in the future, so it would sort
above every real row — for weeks, on a stream scheduled far out — which is the same
eviction bug in a different disguise.

An `upcoming` or `live` row keeps whatever it was first stored with — `assumed`/`now`
from `itemAge`'s zero. The probe supplies **no** date for these statuses, so nothing
overwrites it. That is fine: the value is a placeholder, because those statuses are
excluded from the ranking entirely. The date starts mattering exactly when the row
becomes `vod`, and the probe that observes that transition supplies the real one.

**`date_precision <> 'assumed'` is what enforces goal 4.** An `assumed` row carries a
fabricated date: `itemAge` returns `0` for any item it cannot parse, which becomes
`published = now`. Letting that rank would place a guess at position 1 ahead of
every real date — the opposite of the goal. Excluding it costs nothing, because
every `assumed` row is always discovery-probed — an `unknown` one by the status rule
plus the term carve-out ("Descriptions and term matching"), an `upcoming`/`live` one
by the status rule directly. (Not every `assumed` row is `unknown`: `upcoming`/`live`
rows store no date and stay `assumed` too.) The probe promotes it to a real date and
it enters the ranking, usually within the same cycle.

This also removes a denial-of-scope failure. `published` is frozen at insert, so if
the corrective probe fails permanently the fabricated "now" would sit in the top N
indefinitely, evicting real content from scope — at `N=3`, three such rows would
freeze a channel's archival scope entirely. An excluded row cannot evict anything.

An item that can never be probed therefore never enters scope, which is the correct
outcome: we know nothing about it, so we archive nothing on its behalf. It is still
probed every cycle, so it recovers the moment YouTube answers.

**The honest cost:** excluding rows shrinks the population the top-N is computed
over, so while an `assumed` row is pending it is possible to admit an item that is
truly rank N+1. "Pending" is normally one cycle, because every `assumed` row is
probed (terms notwithstanding) — **except a persistently `denied` row, which stays
pending for as long as we lack access.** That is correct rather than unfortunate: an
item we are refused has no knowable date, so ranking it would be the guess goal 4
forbids. This is a real trade, not a free win — but it is the right
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

**Discovery probes** — every cycle, rank ignored. The status rule is only half of
it; the gates are not optional, and they are stated here in full because a reader
looking up "when do we probe?" stops at this block:

```
per row, in this order:
  skip if HasActiveJob                        (the worker owns it)
  skip if NOT term-match  UNLESS date_precision='assumed'
                                              (terms gate JOBBING; an assumed row is
                                               probed anyway to get a DATE, which
                                               RANKING needs — see the carve-out in
                                               "Descriptions and term matching")
  skip if source='membership' AND NOT membershipActive()
                                              (no cookies / discovery off ⇒ an
                                               authenticated probe is useless)
  then, by status:
    unknown  → probe (nothing can be missed)
    upcoming → probe (goal 3)
    live     → probe (goal 3)
    vod / not_a_stream → NOT probed for discovery
  probe choice: source='membership' ⇒ authenticated, else anonymous
  (no row is ever deferred in Phase 1 — see "Probe-list growth")
```

**Stating this as the bare status rule is a known trap.** It then reads as "probe
every `unknown` row unconditionally", which drops the term gate and reintroduces the
goal-5 cost regression on any channel using `terms`. It is also exactly the false
premise that made an earlier state-space proof miss a sink (see "This invariant is
exactly necessary and sufficient"). The status rule alone is never the answer.

### Probe-list growth: named, accepted, and deferred to Part 2

Two designs were tried here and **both were wrong**; the honest answer is to do
nothing in Phase 1.

**Rejected: a terminal `unreachable` status.** `MetadataTracker` gives up after
three *consecutive* failures (`utils.go:19-21`, `:61-76`) and then **deletes the
counter** — `utils.go:275-279` calls it "backing off" and notes it "resets the
tracker's escalation to 0". It is a recurring, deliberately transient backoff, not a
verdict. Marking a row terminal on it means a members stream that was briefly
unprobeable is never probed again: the same unrecoverable goal-3 loss as the
`HasProcessed` gate.

**Rejected: a durable per-item backoff, deferring only `unknown` rows.** The
exemption was justified as "`unknown` — we have never successfully probed it, so we
do not know it is a stream at all; deferring costs nothing we can name." That is
false, and this spec names the cost itself in its own goal-3 worked example: *"cookies
hiccup; the probe fails enough times to give up ⇒ status stays `unknown`. Cookies
recover; the next discovery probe succeeds ⇒ status becomes `live`."*

Under an `unknown`-only backoff there **is no next discovery probe** — the row is
deferred. And the failure is structural, not incidental: **members content cannot
reach `upcoming`/`live` in the store without a successful authenticated probe**,
because the membership tab yields `itemAge == 0` ⇒ `assumed`/`unknown`. So the
"never defer a known stream" exemption protects rows whose probe already succeeded,
and defers exactly the rows where the same transient fault prevented success. It
guards everything except the case it was written for.

**Accepted for Phase 1: no backoff.** A row that can never be usefully probed is
re-probed every cycle. Today's escape — scrolling out of RSS's 15-item window — is
gone, because reading the store is what fixes the original bug, so this *is* a real
regression. It costs one `/player` call per cycle per such row.

Two classes qualify, and the second is **not** rare:

- **Dead IDs** — deleted before ever being probed successfully. Genuinely rare.
- **Persistently `denied` rows** — a members video on a channel we hold no cookies
  for. By design, permanent while access is missing, and bounded by how much members
  content such a channel has (tens, not thousands). The membership probe gate removes
  most of these before they cost anything: with `membershipActive()` false the probe
  is skipped outright, so only rows whose `source` is stale reach a real request.

Correctness and reliability outrank efficiency here, and every bounding scheme tried
above bought efficiency with a chance of losing a stream. `last_listed_at` (below)
retires both classes for the same reason and with the same evidence.

**The Part 2 fix, deferred deliberately.** `last_listed_at` is the correct signal: a
row that **no listing has mentioned for N days** *and* whose probes fail or are
denied is gone (or permanently out of reach) —
which is a fact about YouTube's catalog, not a guess about a probe. It carries no
miss risk, because a row that still appears in any listing is never deferred. It
needs the listing-coverage data only the three-tab scan provides, so it belongs to
Part 2 and is specified there rather than approximated now.

This is why the Phase 1 schema carries no backoff columns: the wrong mechanism,
persisted, is harder to remove than absent.

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

Phase 1 accepts it rather than bounding it badly (see above). Note the *ranking* is
unaffected regardless: a row stuck at `unknown` is excluded from the top-N by the
`assumed` rule, and one stuck at `upcoming`/`live` by the status rule — so a dead ID
can never evict real content from scope, however often it is retried. The cost is
purely wasted requests, which is why it can wait for the correct signal.

`MetadataTracker` and `ProbeCooldown` are untouched, per the non-goal.

### The probe outcome must surface to the caller, not be hooked inside

`ProcessYouTubeVideo` is shared: it is called from `internal/monitor/feed.go:753`
**and** `internal/monitor/decapi.go:583`. So no `feed_items` write may be hooked
inside it — DECAPI would then write rows, breaking the "no DECAPI-side change"
non-goal and putting rank-1 DECAPI hits into a store the design says they never
enter.

Instead `ProcessYouTubeVideoResult` gains an **outcome** discriminator:

```
feed path (probeAndClassify) — four outcomes:
  probed      — a probe ran, returned metadata, and the result is NOT denied.
                NOT "PlayabilityError == ok": that was an earlier, over-broad
                rule, and pairing it with the current `denied` leaves
                upcoming+unknown premieres and age_restricted VODs in NEITHER
                bucket — undefined, never FRESH, never jobbed.
  denied      — YouTube refused us and the classifier was guessing.
                Predicate: see "The authoritative fix" — do not restate it here.
                Behaviour: NOT FRESH; writes no status; retried next cycle.
  errored     — a probe ran and failed                     (utils.go:269-294)
  cooldown    — no probe ran; ProbeCooldown suppressed it  (utils.go:258-262)

DECAPI (composed ProcessYouTubeVideo) — keeps a fourth:
  passthrough — no probe ran; ProbeVideo is not wired      (utils.go:248-250)
```

**Contract: `StreamStatus` is meaningful if and only if `outcome == probed`.** This
falls straight out of the existing control flow, and the type should enforce it
rather than rely on the reader:

| return site | `meta` | outcome |
|---|---|---|
| `utils.go:249` | not yet assigned | `passthrough` |
| `utils.go:261` | not yet assigned | `cooldown` |
| `utils.go:294` | zero value (`err != nil`) | `errored` |
| `utils.go:315`, `:332`, `:350` | valid | `probed`, or `denied` (predicate: see "The authoritative fix") |

`meta` is assigned at `:268`, so the first two returns precede it entirely and the
error return holds only its zero value. Reading `StreamStatus` on any non-`probed`
outcome yields `""` and would classify silently wrong. The spec never does — the
store write and the job decision both require `probed` first — but the coupling
should be explicit, because "empty string means not-a-stream-ish" is exactly the
kind of thing that compiles.

**There are four, not three, and the fourth breaks the obvious rule.** An earlier
draft claimed all outcomes "collapse to `ShouldProcess=false`". Three do —
`utils.go:258-262` (cooldown) and `:294` (error) — but `p.ProbeVideo == nil` returns
`ShouldProcess: **true**` at `:248-250`, passing the item straight through
un-probed. So "not `ShouldProcess`" is not a usable proxy for "not fresh", and a
naive implementation deriving freshness from `ShouldProcess` would job on no
metadata whenever the probe is unwired.

**`passthrough` does not produce a job on the feed path, and this is a deliberate
behavior change.** An earlier draft claimed it "counts as fresh and still jobs,
exactly as today". That was wishful: the archival pass dispatches on **stored
status**, and a passthrough writes no status, so the row stays `unknown` — whose
branch is an explicit NO JOB. Declaring it fresh changes nothing, because no branch
consults freshness for `unknown`.

Rather than carve out a special case, the feed path simply **requires a wired
probe**: `probeAndClassify` has no passthrough outcome, and a nil `ProbeVideo` there
is a programming error, not a supported mode. Production always wires it
(`cmd/moombox/monitor_callbacks.go:180`), and the only nil caller is a test
(`utils_test.go:215`). DECAPI keeps the composed `ProcessYouTubeVideo` and with it
today's nil behavior, unchanged.

So the feed path's outcome set is four values (`probed`/`denied`/`errored`/`cooldown`)
and `outcome == probed` is the FRESH predicate without qualification. `passthrough`
still matters — it is why `ShouldProcess` cannot be used as a freshness proxy — but
it lives on DECAPI's side of the split, so it is not in the feed path's set at all.

The feed monitor reads the outcome and owns both store writes; DECAPI ignores it and
behaves exactly as it does today.

**`outcome == probed` is the FRESH predicate** — for the store write *and* as the
precondition for a job. It is not a new source of truth: it reads a decision
`ProcessYouTubeVideo` already makes internally and currently discards.

`Outcome` alone is not sufficient, though. The result must **also** carry
`StreamStatus` and the authoritative publish date, because `ShouldProcess` conflates
"the probe worked" with "this should become a job" — see "Only FRESH items become
jobs". The feed path reads `Outcome` + `StreamStatus` and decides for itself;
`ShouldProcess` remains for DECAPI.

### The split must be a function split, not just a richer return type

Adding fields is not enough, because `ProcessYouTubeVideo` does not merely *decide* —
it has **side effects the feed path must no longer trigger**:

```
utils.go:284  AddToHistory  — probe give-up
utils.go:313  AddToHistory  — not_a_stream skipped by nonLiveSkipReason
utils.go:330  AddToHistory  — post_live/vod skipped by nonLiveSkipReason
```

And `nonLiveSkipReason(includeNonLive=false, _)` **always** skips (`utils.go:229-231`),
so on a default-config channel those fire for *every* plain upload. If the feed path
keeps calling the function while ignoring its verdict, it silently inherits history
writes it does not control.

So `ProcessYouTubeVideo` splits in two:

| | used by | does |
|---|---|---|
| `probeAndClassify` | feed monitor | probe; record cooldown; update `MetadataTracker`; return `Outcome` + `StreamStatus` + title/channel/publish date. **No history writes, no job verdict.** |
| `ProcessYouTubeVideo` | DECAPI only | `probeAndClassify` + today's `nonLiveSkipReason`/`AddToHistory`/`ShouldProcess` logic, byte-identical |

The feed monitor then owns every decision it is specified to own — status write,
archival gating, history — with no hidden writes underneath it. DECAPI keeps the
composed function and is untouched, which is what makes the "no DECAPI-side change"
non-goal true rather than aspirational.

**Two consequences worth stating:**

- **`IsReprobe` becomes dead for the feed path.** It exists only to drive
  `nonLiveSkipReason` and to demote log level; the archival pass consults
  `HasProcessed` directly. `probeAndClassify` does not take it. DECAPI still passes
  it.
- **Feed-path give-up no longer writes history.** Today give-up calls
  `AddToHistory`, whose only effect is flipping the reprobe/log-level flag — it
  explicitly does *not* stop re-probing (`utils.go:272-279`), so nothing is lost by
  dropping it. This is a deliberate behavior change: it stops manufacturing
  orphaned-history rows for videos that were never jobbed, which is precisely the row
  class that accumulated into the 80-entry purge that re-armed `gr-ZTohjwnQ`.

**Refresh probe** — on demand, only when a stored item is about to become a job:

```
in-scope AND NOT HasProcessed AND term-match AND status IN (vod, not_a_stream)
    AND include_non_live_content          ← BEFORE probing; a channel that archives
                                            no VODs must not pay the probe
    AND channel established
    AND NOT (source='membership' AND NOT membershipActive())   ← same gate as discovery
    → re-probe now, then job on the fresh result (denied/errored/cooldown ⇒ no job)
```

**The membership gate applies here too.** With the `denied` rule in place this is
now an *efficiency* control, not a correctness one — a cookieless probe of members
content returns `members_only`, which is `denied`, so no job results either way. The
gate's job is to skip a request we already know will be refused. The reasoning below
is retained because it is what the gate was *derived* from, and because it is exactly
what happens if the `denied` rule is ever weakened:
`ProbeVideoAuth` carries no cookie check — `cmd/moombox/monitor_callbacks.go:188-198`
calls `ProbeVideoStatusAuthenticated` unconditionally — so with no cookies the
"authenticated" probe still *runs*, gets no formats, and misclassifies the video as
`upcoming`. That is the failure `feed.go:743-746` documents and 2.7.2 fixed.

Ungated, that misclassification is laundered straight into a job:

1. Operator raises `max_feed_items` 3 → 20 (the explicitly-supported widening case),
   so members `vod` rows enter the top-N. The read arms gate on config only, so a
   cookie lapse does not remove them — correctly.
2. Cookies lapse that cycle. The archival pass fires a refresh probe on a members
   `vod`.
3. No cookies ⇒ no formats ⇒ returns `upcoming`. The outcome is `probed`, so it is
   **FRESH**.
4. Written back ⇒ the row becomes `upcoming`. Re-decide on the fresh status ⇒
   `upcoming → job (cap exempt)`. A job is created for a members VOD that cannot
   download.
5. `AddToHistory` fires on job creation ⇒ `HasProcessed`. Cookies return, discovery
   corrects the row to `vod`, and the archival `vod` branch now hits
   `skip if HasProcessed`. **The VOD is never archived**, and recovering it requires
   clearing orphaned history by hand.

So the gate is not symmetry for its own sake. Without it, cookie state does far more
than skip a doomed probe: the probe is not skipped, does not fail, and returns a
*wrong answer* that this design routes directly to a job — and then poisons the row
against ever being archived correctly.

Gated, the refresh probe simply does not fire during a lapse: no fresh result, no
job, retry next cycle — the behavior already specified for `errored` and `cooldown`.

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
                     [AND source <> 'membership' iff NOT membershipActive()
                      — probing may be opportunistic; ranking may not]
        └─ read from the STORE, not this cycle's response
      per item: skip if HasActiveJob → skip if NOT term-match → probe
        └─ same order as today: HasActiveJob → terms → probe (feed.go:704-725)
        └─ CARVE-OUT: an 'assumed' row is probed even if terms do not match —
           it needs a DATE for ranking, and ranking counts all channel content.
           Without this it is never probed, never dated, never rankable: a
           permanent sink.
        └─ source='membership' ⇒ authenticated probe (ProbeVideoAuth); else
           anonymous. This replaces today's in-memory discoveredVideo.authProbe
           (feed.go:681/:748), which cannot survive store-driven candidates.
      outcome per item:
        probed     ⇒ UPDATE title
                     UPDATE status (post_live→vod)
                       └─ EXCEPT: a terminal status (vod/not_a_stream) is only
                          written if the row will have a rankable date. No date
                          ⇒ stay 'unknown', so the row keeps getting probed
                          instead of becoming terminal-and-unrankable forever
                     UPDATE published + date_precision ONLY IF the probe supplied a
                       date AND its precision is strictly better than the stored one
                       └─ SAME precision guard as the upsert. Without it a probe of
                          a plain upload writes uploadDate ('day') over RSS's
                          second-granular <published> ('exact') — a downgrade of the
                          value while upgrading the label, and a direct breach of
                          "published is frozen and only ever upgraded"
                       └─ upcoming/live supply NO date, so this clause simply does
                          not fire for them; it must never write published=''
                     mark FRESH for this cycle
                     └─ fires whenever METADATA came back — NOT gated on
                        ShouldProcess, which is false for a successful probe
                        of a non-jobbable item (utils.go:315, :332)
        denied     ⇒ (predicate: see "The authoritative fix")
                     Store untouched; NOT fresh; retry next cycle. The backstop
                     that depends on nothing we stored — not `source`, not
                     cookies.
        errored    ⇒ store untouched; NOT fresh; retry next cycle
        cooldown   ⇒ probe skipped; NOT fresh; retry next cycle
        (no passthrough on this path — probeAndClassify requires a wired probe)
4. ARCHIVAL PASS  (runs after the probe pass, on corrected dates)
      in-scope = top-N query  ∪  {items WHERE status IN (upcoming, live)}
                 [BOTH arms exclude source='membership' iff NOT
                  MembershipDiscoveryEnabled() — the operator's choice only.
                  Reads must be gated, not just fetches, and neither read arm
                  may depend on cookie state, or a cookie lapse moves scope]
      per item: skip if HasActiveJob → skip if NOT term-match → decide:
         upcoming/live    → job iff FRESH this cycle
                            (cap exempt; NOT gated by HasProcessed — see below)
         vod/not_a_stream → skip if HasProcessed
                            skip unless include_non_live_content   ← before probing
                            skip unless channel established
                            skip if source='membership' AND NOT membershipActive()
                              └─ ProbeVideoAuth has NO cookie guard, so without
                                 cookies it RUNS, gets no formats, and returns
                                 'upcoming' — which `denied` intercepts
                                 (members_only). Gate = skip a doomed request.
                            → refresh-probe, WRITE THE RESULT BACK to the store,
                              then re-decide on the FRESH status:
                                 denied           → NO JOB, retry next cycle
                                   └─ MUST be listed FIRST: `denied` carries
                                      StreamStatus=='upcoming' by definition, so
                                      any table that omits it routes it straight
                                      to `live/upcoming → job (cap exempt)` —
                                      the 2.7.2 misfire, laundered.
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

That distinction is load-bearing because **history rows exist with no job row**, so
`HasProcessed` is true for videos that were never archived:

1. `X` has a history row but no job. (Sources below.)
2. A discovery probe succeeds ⇒ status becomes `live`.
3. A `HasProcessed` gate on the live branch skips it. **The stream is never
   archived, and it is gone forever.**

Today step 3 jobs it, because `HasProcessed` never reaches the live branch.

**Where such rows come from — after this design lands.** The obvious source is the
feed path's own probe give-up (`utils.go:283-285`), and an earlier draft cited it.
That citation is now dead: the `probeAndClassify` split deliberately removes that
write (see "The split must be a function split"). The rule nonetheless stands, and
must not be dropped with its original justification, because two sources survive:

- **DECAPI give-up.** `decapi.go:583-594` still calls the composed
  `ProcessYouTubeVideo` with `AddToHistory` wired, so a DECAPI probe that gives up
  still writes a history row with no job. DECAPI and the feed monitor see the same
  YouTube channels.
- **Every pre-existing row.** Installs upgrading into this design carry years of
  them — the 80-row purge in the Problem section is direct evidence, and those rows
  were orphaned precisely because they had no job.

So the guard is not hypothetical; only its most obvious arranger went away. A design
that regressed this would violate goal 3 via the transient failures this spec claims
to leave untouched.

**Only FRESH items become jobs**, where fresh means **this cycle's probe returned
metadata** — `outcome == probed`. It does **not** mean today's
`ShouldProcess == true`, and conflating the two is a trap that survived eight review
rounds of this document:

`ShouldProcess` is *not* "the probe worked". A probe that **runs and succeeds**
returns `ShouldProcess=false` whenever `nonLiveSkipReason` skips — `utils.go:315`
(`not_a_stream`) and `:332` (`post_live`/`vod`). The mapping is not invertible:
`false` means "errored **or** cooled down **or** succeeded-but-not-jobbable".

Deriving the store write from `ShouldProcess` therefore breaks the design on the
**default** configuration (`IncludeNonLiveContent` is a plain `bool` at
`internal/config/types.go:264`, so it defaults to `false`):

1. RSS carries a plain upload ⇒ stored `unknown`/`exact`.
2. Discovery probes it — that pass gates on `HasActiveJob` → terms only; the
   `include_non_live_content` check lives in the archival branch.
3. The probe **succeeds**, returns `not_a_stream`, and `nonLiveSkipReason(false, _)`
   skips ⇒ `ShouldProcess=false`.
4. If that means "not fresh", the status write never fires ⇒ the row stays
   `unknown` **forever**.
5. Nothing bounds it: the probe *succeeded*, so no failure path engages either.

Every plain upload on every default-config channel would then be probed at full rate
forever: ~15 per channel per cycle against today's 3. That inverts goal 5 and
falsifies "`vod` is not discovery-probed", which silently *depends* on the status
write landing.

So the design needs **two predicates, not one**:

| Predicate | Value | Purpose |
|---|---|---|
| store write | `outcome == probed` | record status/date/title whenever metadata came back — **regardless of jobbability** |
| job | `outcome == probed` **plus the archival rules** | the archival pass decides from the fresh `StreamStatus` |

They coincide on the `live`/`upcoming` branches (`utils.go:319-324` fall through to
`ShouldProcess: true`), which is exactly why the divergence hid — it exists only on
the succeeded-but-skipped path.

Consequently the feed monitor consumes the probe's **metadata**, not
`ProcessYouTubeVideo`'s verdict: `ProcessYouTubeVideoResult` must expose
`StreamStatus` and the authoritative publish date alongside `Outcome`. The archival
pass already re-derives the job decision (upcoming/live → job; vod → gated on
`include_non_live_content` and `established`), so `ShouldProcess` is simply unused by
the feed path. DECAPI keeps using it, unchanged — which is also why it must stay.

This preserves the existing invariant through the two-pass split, which otherwise
silently breaks it:

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

So the discovery pass keeps today's order — `HasActiveJob` → terms → probe — **with
one carve-out: an `assumed` row is probed regardless of terms.**

Without the carve-out the term gate opens a third permanent sink, and it defeats the
very rule this section exists to state. A non-matching row that entered `assumed`
(no parseable age text) would never be probed, so its date could never be upgraded;
`published` is frozen at insert and only a probe can improve it. It would stay
`assumed` forever, and `assumed` rows are excluded from the top-N — so the row is
stored but **not ranked**, and the top-N is computed over a subset. That is exactly
the "compute the top-N over a subset and get it wrong" failure that justifies
storing non-matching items at all.

The carve-out is cheap and self-limiting, because the two gates exist for different
reasons:

- **Terms gate the probe** to avoid repeatedly fetching metadata for content that
  could never become a job. That reasoning is about *jobbing*.
- **An `assumed` row is probed to get a date**, which *ranking* needs — and ranking
  counts all channel content, matching or not.

So a non-matching row is probed until it *has* a date, then never again while it
stays non-matching. For the common case that is once: the probe returns
`vod`/`not_a_stream` with a date (terminal, and rankable).

Two cases cost more, and neither is unbounded:

- **The probe returns `upcoming`/`live`.** Those statuses store no date, so the row
  stays `assumed` and the carve-out keeps probing it every cycle — a non-matching
  members premiere scheduled three weeks out costs one authenticated probe per cycle
  for three weeks. It stops when the stream ends and `post_live` supplies
  `startTimestamp`. Bounded by the stream's lifetime, and such a row is exactly the
  one whose status we most want current.
- **The probe returns `vod` with no date.** The row stays `unknown` per the terminal
  invariant and is retried — the accepted cost in "Probe-list growth".

Rows that already carry a real date (RSS `exact`, membership/backfill `coarse`) are
never probed on this path at all — they are already rankable.

A non-matching row's `status` may well remain `unknown` forever, and that part *is*
harmless: status drives probing and job decisions, and a dated `unknown` row does
neither. The harm was never the status — it was the precision the status was
blocking.

Term matching cannot be a SQL predicate — it needs the in-memory description for
RSS-carried items — so it stays a Go-side filter over the query's rows, exactly as
it is a Go-side filter over candidates today.

### What `history` comes to mean

Removing the feed path's hidden `AddToHistory` calls (`utils.go:284/313/330`) is a
**population** change, not a schema one — the table, its API, the orphan overlay and
Twitch/DECAPI writers are all untouched. But it is worth stating what it does to the
meaning of the table, because the change is an improvement and reviewers will notice
the row count drop.

Today `history` conflates three things: *we jobbed this* (`monitor_callbacks.go:260`),
*we looked at this and declined* (`nonLiveSkipReason` skips), and *we gave up probing
this* (give-up). Only the first has a job row — which is exactly why
`ListOrphanedHistory` finds anything at all, and why an operator eventually purges 80
rows and re-arms content that was never supposed to come back.

On the feed path it now means only the first: **we created a job for this video.**
That is what "already acted on" should have meant, it makes `HasProcessed` a precise
predicate rather than a fuzzy one, and it means far fewer orphans are manufactured in
the first place. DECAPI and Twitch keep writing exactly as they do today, so the
orphan tooling remains necessary and correct — just less busy.

This composes with the date-based cap into defence in depth: purging history can no
longer re-arm an out-of-scope VOD (the cap stops it), *and* there is much less
spurious history to purge.

### `source` decides which probe to use

Today the authenticated-probe choice rides on the in-memory candidate:
`membershipCandidates` sets `authProbe: true` (`feed.go:681`) and `processCandidate`
reads it (`feed.go:748`) to pick `ProbeVideoAuth`. That field cannot survive the
move to store-driven passes — a row read back from `feed_items` has no `authProbe`.

**`source = 'membership'` is the signal.** It is the only durable record that an item
came from the members tab, and members-only content must be probed with cookies or
the classifier misfires it as `upcoming` (`feed.go:743-746`) — the bug 2.7.2 fixed.

This is one of two jobs `source` does — the other is the toggle predicate below.
Both are reads of a column that was, until this round, written and never read.

**The transition cases, and why the failure directions are acceptable:**

- **Members → public** (creator unlocks a video). RSS now lists it with an `exact`
  date; the stored row is `coarse`, so the precision guard fires and `source`
  becomes `rss`. Subsequent probes are anonymous — correct, it *is* public now.
- **Public → members** (creator locks an existing video). RSS drops it; the
  membership tab lists it `coarse`. **`source` must flip to `membership` here, and
  the precision guard must not be allowed to prevent that** — see below.

**`source` is updated on every sighting, independent of the precision guard.** An
earlier draft folded `source` into the guarded `DO UPDATE`, so a public→members
transition left `source='rss'` (the membership tab's `coarse` date loses to the
stored `exact`), and dismissed the result as "wrong but safe: we decline to archive
rather than misfire". **That was false, and it is the same mistake the refresh-probe
gate exists to prevent: a probe without cookies does not fail, it lies.**

Traced properly, `source='rss'` on a members video means:

1. The discovery pass picks the **anonymous** probe (`source <> 'membership'`), and
   no membership gate applies — the row does not look like members content.
2. `feed.go:743-746`: an anonymous probe of members-only content "gets no formats
   and the classifier misfires it as **upcoming**". It returns no error.
3. Outcome would be `probed` ⇒ **FRESH** (this is what the `denied` rule now
   intercepts: `members_only` ⇒ `denied` ⇒ no job). The row becomes `upcoming`.
4. `upcoming` is **cap-exempt** ⇒ in scope regardless of rank ⇒ `job iff FRESH` ⇒
   **job created**, bypassing both the cap and `include_non_live_content`.

That is precisely the misfire `feed.go:743-746` documents and 2.7.2 fixed — reached
by trusting a stale `source`. It is not a decline; it is a phantom upcoming stream
that will never go live, jobbed forever.

So the upsert splits the two concerns, because they are unrelated:

- **`source` records where we last saw the item.** It is a fact about *this*
  sighting and is always current. It decides which probe to use, so it must track
  reality immediately.
- **`published`/`date_precision` record our best estimate.** They are monotonic, so
  a worse estimate never overwrites a better one.

Coupling them made a date-quality rule silently govern probe selection. Both
directions are then correct: members→public flips to `rss` (anonymous probe —
correct, it is public), and public→members flips to `membership` (authenticated
probe, and the membership gates apply — correct, it is members-only).

The only way two sources contend for one row in a cycle is the backfill's `/videos`
and `/streams` tabs, which both list past streams; both are public, so either value
selects the anonymous probe and last-writer-wins is harmless.

### `membership_discovery = false` must gate reads, not just writes

The store-driven passes create a hazard the response-driven design never had, and
closing it at write time is not enough.

`membershipActive()` (`internal/monitor/feed.go:518-523`) stops the *fetch*. Today
that is sufficient, because the candidate pool **is** the response — no fetch, no
candidates, no jobs. But the discovery and archival passes read the **store**, which
still holds every members row written while the toggle was on. So:

1. `membership_discovery = true`; a members premiere `X` is stored `unknown`
   (`source='membership'`).
2. The operator sets `membership_discovery = false` — cookies expired, or they no
   longer want members content.
3. Next cycle: no membership fetch. But the probe list is *"read from the STORE, not
   this cycle's response"*, and `X` is `unknown` ⇒ it is in the list, and probed
   with cookies.
4. It returns `upcoming` ⇒ cap-exempt ⇒ in scope ⇒ **job created.**

Moombox then downloads members-only content on a channel where members-only
discovery is off. The Part 2 backfill gate does not help: this needs no backfill.

**The fix is a read-side predicate — but the three arms must not use the same
predicate.** This is the trap, and an earlier draft of this section fell into it by
gating all three on `membershipActive()`.

**`membershipActive()` is not the toggle.** It folds in live cookie state:

```
membershipActive()          = FetchMembership != nil && MembershipEnabled()   feed.go:518-523
MembershipEnabled()         = MembershipDiscoveryEnabled() && HasAuthCookies() monitor_callbacks.go:218-224
```

and its own comment says it "re-reads the config flag **AND cookie state** live each
cycle". Cookies lapse for reasons nobody chose: the exporter rotates the file
(`cookies/jar.go:72-90` — `Load` on a missing file clears the jar and returns nil),
the browser session ends, a refresh races. `SyncCookies` runs before every
membership fetch and every authenticated probe.

So gating the **ranking** on `membershipActive()` reintroduces this spec's own bug:

1. `N=3`. Store: three recent members VODs at ranks 1-3; public VODs at ranks 4-6.
2. Cookies lapse for one cycle ⇒ `membershipActive()` false ⇒ the top-N appends
   `AND source <> 'membership'` ⇒ the top-3 becomes the three **public** VODs.
3. They are in scope, unprocessed, term-matching, non-live ⇒ refresh-probed
   anonymously (they are public, so it succeeds) ⇒ **three jobs created**.
4. Cookies return; scope reverts. The downloads are permanent and now in `history`.

That is scope moving because a fetch input failed — *"a partial discovery failure
silently changes what the cap means"*, which is the Problem statement of this
document. Round 20b's goal-2 wording claims every scope-mover is "an operator or
channel action with a visible cause"; a cookie rotation is neither.

**So the arms split by what they are for:**

```sql
-- READ arms (top-N ranking, cap-exempt union): the OPERATOR'S CHOICE only
AND source <> 'membership'      -- iff NOT MembershipDiscoveryEnabled()

-- PROBE arm (discovery probe list): config AND cookies
AND source <> 'membership'      -- iff NOT membershipActive()
```

The asymmetry is the point:

- **What is visible may only move on an operator's decision.**
  `MembershipDiscoveryEnabled()` is exactly that decision and nothing else. Both read
  arms use it, so a cookie lapse cannot change what is in scope *or* what is
  cap-exempt.
- **Probing may be opportunistic.** With no cookies an authenticated probe is
  useless, so skipping it costs nothing — and loses nothing, because the row stays
  ranked, stays cap-exempt, and simply cannot become a job without a FRESH result.

**Why the cap-exempt union uses config and not `membershipActive()`:** it makes the
cookie-lapse question disappear rather than answering it. Both choices reach the
same outcome — during a lapse the row is unprobeable, so it is never FRESH, so it is
never jobbed, and it is caught the moment cookies return — but keying it on cookie
state invites the reader to ask "does a cookie lapse hide a live members stream?"
every time they encounter it. Keying both read arms on the operator's choice means
cookie state can only skip a probe that would have failed. The probe-side membership
gates make that true cheaply; the `denied` rule makes it true *regardless*, by
intercepting the wrong answer even on an ungated path.

**This demotion applies to the probe arms only. The read arms are not redundant, and
the reason is ranking — not job prevention.** An earlier draft justified them by
claiming `denied` cannot cover "a members probe with valid cookies, which returns
`ok` and would be jobbed". That reason is wrong: with `membership_discovery = false`
a `source='membership'` row hits the *probe* gate (`membershipActive()` is false), is
never probed, never becomes FRESH, and therefore cannot be jobbed at all. Nothing
about jobbing needs the read arms.

What needs them is scope. With the toggle off and no read gate, members rows still
**rank** — so three members VODs at ranks 1-3 hold the top-N slots and evict the
channel's public VODs at ranks 4-6 out of scope, which are then never archived. The
operator disabled members *discovery* and silently lost their public archival depth.
`denied` cannot reach this: it decides whether a probe's answer is trusted, and no
probe is involved in ranking a stored row.

So the three controls partition cleanly by what they protect:

| Control | Protects | Can another cover it? |
|---|---|---|
| Read arms — `MembershipDiscoveryEnabled()` | **scope**: members rows must not hold slots when the operator turned discovery off | No — nothing else touches ranking |
| Probe gates — `membershipActive()` | **efficiency**: skip a request we know will be refused | Yes, `denied` catches the result anyway |
| `denied` — playability | **correctness**: a refusal must never be read as `upcoming` | No — it is the only control that depends on nothing we stored |

A cookie lapse therefore leaves scope *exactly* where it was: members rows keep
their ranks, hold their slots, stay cap-exempt, and simply go un-probed for a cycle.
Goal 2 holds at no cost, and goal 3 is unaffected — the stream is jobbed on the
first cycle cookies return, which is also the first cycle it could have been
downloaded at all.

**Excluding them from the ranking when the operator turns the toggle off is
deliberate.** With it off, today's cap counts only public items, because members
items never enter the pool at all. Ranking invisible rows would let hidden content
evict public content from scope — a silent change in the opposite direction. The
rows stay stored (turning it back on must not lose them) and stop being visible.

Both predicates compose with the partial rank index: each is conjunctive, so the
index's `WHERE` stays implied and usable, and `source` is a residual filter over the
rows returned.

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
     └─ EARLY EXIT: if backfilled_at IS NOT NULL and a full page yields no new
        video IDs, stop paging this tab (a re-run; the rest is already stored).
        NEVER early-exit when backfilled_at IS NULL — the store is non-empty from
        Phase 1's RSS/membership rows, so page 1 can be entirely known while the
        catalogue behind it is entirely unscanned.
2. when all eligible tabs are exhausted, run one ORDERING PASS:
     └─ SELECT the channel's rows into a slice, CLOSE the cursor
     └─ sort by (published DESC, provisional pos ASC, video_id ASC)
        (video_id makes this a total order; an earlier draft had a "source rank"
         term here but never defined one, leaving step 2 unimplementable. Any
         total order satisfies the requirement — cross-source ties inside a
         coarse lump are deterministic-but-arbitrary by design, see below)
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

### Early exit — only on a re-run, never on the first scan

A re-run should not re-page a channel's entire back catalogue to discover what it
already knows. So a tab **stops paging once a full page yields no new video IDs**:
everything below is, by construction, already stored.

A full page of known IDs is the threshold rather than a single known ID, because
YouTube reorders shelves and interleaves; one familiar item proves nothing about the
next page. A whole page does.

**The trap: early exit must be gated on `backfilled_at IS NOT NULL`.** On a *first*
scan the store is not empty — RSS and membership have been populating it since
Phase 1 shipped, so `/videos` page 1 is very likely *entirely* known. A naive early
exit would fire on page 1, declare the tab exhausted, and set `backfilled_at` having
scanned nothing — permanently marking the channel complete with no catalogue behind
it, and no sweep would ever revisit it (`backfilled_at IS NULL` is the only trigger).

So:

```
backfilled_at IS NULL      → full scan, no early exit (the store proves nothing)
backfilled_at IS NOT NULL  → early exit allowed (the catalogue is already complete,
                              so a page of known IDs really does mean "the rest is
                              known")
```

This is the reason the first backfill is expensive exactly once, and every re-run
after it is cheap.

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

Phase 1 accepts that regression rather than bounding it badly — see "Probe-list
growth: named, accepted, and deferred to Part 2". Every bounding scheme considered
bought efficiency with a chance of losing a stream; the correct signal
(`last_listed_at`) needs Part 2's listing coverage.

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
  later `exact` write does overwrite a stored `coarse`. All four rungs ordered
  (`assumed` < `coarse` < `day` < `exact`)
- **`PublishedAt` extraction is status-aware.** `post_live`/`vod` take
  `liveBroadcastDetails.startTimestamp` (`exact`, second-granular), **falling back
  to `uploadDate` (`day`) when `lbd` is absent** — which is the *normal* `vod` case,
  since an ended stream with an `endTimestamp` classifies `post_live`; a
  `not_a_stream` takes `uploadDate` (`day`); an `upcoming` stores **no** ranking
  date — specifically, its future `startTimestamp` never reaches `published`
- **The probe write is precision-guarded.** A probe of a plain upload (`uploadDate`,
  `day`) must NOT overwrite RSS's second-granular `exact` date, and must never write
  `published=''` for an `upcoming`/`live` row
- **`post_live` normalizes to `vod` on write** — no `post_live` row ever reaches the
  archival decision table, where it would match no branch
- **A probe returning `vod` with no usable date leaves the row `unknown`, not
  `vod`.** Guards the permanent sink: `vod`+`assumed` is terminal *and* unrankable,
  so it could never recover even though the next probe might supply a date
- An `assumed`/`unknown` members row that probes to `vod` gets a real date and
  **becomes rankable** — the dead-end guard (without a probe date it would be
  `vod`+`assumed`: terminal and unrankable, i.e. unarchivable forever)
- A stale listing never demotes a probed `live` back to `vod`
- An `assumed` row never enters the top-N query (goal 4), and a permanently
  unprobeable `assumed` row cannot evict real content from scope
- An in-scope `unknown` row is never jobbed — and an RSS row whose probe fails
  stays `unknown` with its **exact** date, holding its true rank rather than
  yielding the slot to an older item (the top-N excludes `assumed` precision, not
  `unknown` status)
- `include_non_live_content = false`: a past members VOD is stored but never
  discovery-probed, preserving today's drop behavior
- **`HasProcessed` does not block a live/upcoming job.** Arrange a history row with
  **no** job row — via a DECAPI give-up (`decapi.go:583-594` still wires
  `AddToHistory`) or a pre-existing/legacy row, **not** via a feed-path give-up,
  which no longer writes one — then let a discovery probe return `live`. The stream
  must still be jobbed. This is the goal-3 regression guard.
- A stale stored `live` whose probe errored this cycle produces **no** job
- With `probe_cooldown > 0`, a cooldown-skipped item produces **no** job
- Coarse dates skew old: `"3 weeks ago"` stores `now-28d`, not `now-21d`
- `membership_discovery = false` ⇒ backfill writes no members rows, and none are
  jobbed via the store
- **A denied probe never becomes a job, whatever `source` says.** The decisive
  case: `membership_discovery = false`, a public video locked to members after we
  stored it (`source` stays `rss` **forever** — no membership fetch means no
  sighting means no flip), then `max_feed_items` is raised so it enters scope. The
  anonymous refresh probe returns `upcoming` with `PlayabilityError = members_only`
  ⇒ `denied` ⇒ no job. Without the playability rule this jobs members content on a
  channel with members discovery **off**, and no gate can see it.
- **A genuine upcoming stream is NOT denied.** `LIVE_STREAM_OFFLINE` /
  "live event will begin" ⇒ `PlayabilityError == ok` (`player_api_parsing.go:349-351`)
  ⇒ `probed` ⇒ FRESH ⇒ jobbed. Guards goal 3 against over-eager denial — and against
  the tempting "past `published` + `upcoming` = contradiction" heuristic, which an
  RSS-announced stream legitimately trips.
- **An age-restricted VOD that returns formats is NOT denied.** It classifies `vod`
  (not `upcoming`), so the grounded-classification rule applies and it is archived
  normally. Guards against the over-broad "any non-`ok` ⇒ denied" rule, which would
  trade one lie for one silence.
- **A cookie lapse must not create a job either.** Members `vod` rows in scope (e.g.
  after raising `max_feed_items`) + no cookies ⇒ the refresh probe must **not** fire.
  Ungated it still issues a pointless request; the `denied` rule stops the result
  becoming a job. Assert **no probe is issued** — the job assertion cannot
  distinguish gated from ungated now, and would pass vacuously.
- **With `membership_discovery = false`, members rows must not hold ranking slots.**
  Three members VODs at ranks 1-3 + public VODs at 4-6 + the toggle off ⇒ the public
  VODs must be in scope. This is what the read arms protect, and neither the probe
  gates nor `denied` can: no probe is involved in ranking a stored row.
- **A cookie lapse must NOT move scope.** Arrange members rows at ranks 1-3 and
  public VODs at 4-6, then make `HasAuthCookies()` false for one cycle: the top-3
  must still be the members rows, and **no public VOD may be jobbed**. Gating the
  ranking on `membershipActive()` (which folds in cookie state) instead of
  `MembershipDiscoveryEnabled()` fails this — and is this spec's own bug, reached
  through a new door.
- **Turning `membership_discovery` OFF takes effect immediately, on rows already
  stored.** Arrange a members row written while it was on, then flip it off: the row
  must not be probed, must not rank, and must not be jobbed — even though no
  membership fetch occurs. (Today this is free, because the pool *is* the response;
  the store-driven passes are what make it a real hazard.)
- Turning it back ON restores those rows (they were hidden, not deleted)
- Channel removed from config ⇒ its `feed_items`/`channel_state` rows are pruned;
  re-adding it triggers a fresh backfill rather than inheriting `backfilled_at`
- Backfill skips Twitch channels entirely
- A stored `vod` whose refresh probe returns `live` (stream restarted on the same
  ID) **is** jobbed, and the store is updated to `live` so it stays in the
  discovery list — it is not skipped as "no longer non-live"
- Backfill: rows persist per page, so a restart mid-scan resumes from the cursor
  rather than re-scanning; `catalog_pos` is only final once `backfilled_at` is set
- **Early exit fires on a re-run and NEVER on a first scan.** Specifically: a first
  scan of a channel whose store already holds its 15 newest RSS items must still
  page the whole catalogue — an early exit there would set `backfilled_at` on an
  unscanned channel that no sweep would revisit
- Removing a channel mid-backfill cancels the scan before the prune, and leaves no
  resurrected rows
- **Upcoming/live rows do not occupy ranking slots.** `N=3` + three scheduled
  premieres + one VOD from yesterday ⇒ the VOD is still in scope (goal 3)
- An `upcoming` row that ends becomes `vod`, **enters** the ranking at rank 1, and
  pushes the previous rank N out of scope
- A scheduled start time is never stored as `published`
- **Probe give-up never retires or defers a row.** An upcoming/members stream that
  is unprobeable for several cycles (cookie hiccup, YouTube 5xx) is probed again on
  the very next cycle and archived once it recovers. This is the goal-3 guard
  against both rejected bounding designs.
- A row stuck at `unknown` after repeated failures is still probed every cycle
  (accepted cost — see "Probe-list growth")
- **`probeAndClassify` requires a wired probe** — the feed path has no passthrough
  outcome, so `outcome == probed` is FRESH without qualification. DECAPI keeps
  today's nil behavior via the composed function. (Freshness must never be derived
  from `ShouldProcess`, which is `true` on the nil path.)
- **A successful probe of a non-jobbable item still writes its status.** Default
  config (`include_non_live_content = false`), RSS plain upload ⇒ probe succeeds ⇒
  `not_a_stream` ⇒ `ShouldProcess=false` ⇒ the row must become `not_a_stream`, NOT
  stay `unknown`. Guards the goal-5 inversion where every upload is probed forever.
- `ShouldProcess` is never used by the feed path; DECAPI's behaviour is byte-identical
- **No hidden history writes on the feed path.** A plain upload probed on a
  default-config channel writes **no** history row (today `nonLiveSkipReason` →
  `AddToHistory` at `utils.go:313` fires for every one), and a feed-path give-up
  writes none either — give-up's only effect today is the reprobe flag
- A DECAPI probe still writes history exactly as today (regression guard on the
  `probeAndClassify` split)
- **`source='membership'` selects the authenticated probe.** A members row read back
  from the store is probed with cookies, not anonymously — otherwise it misclassifies
  as `upcoming` (`feed.go:743-746`), the 2.7.2 bug
- **A public video that becomes members-only flips `source` to `membership` on the
  next membership listing, even though its stored `exact` date beats the listing's
  `coarse`.** The precision guard must not gate `source`. Regression test for the
  phantom-upcoming misfire: left as `rss`, it is probed anonymously, lies
  `upcoming` — which `denied` intercepts. Assert the **`denied` outcome and the
  absent job**, not "source flipped": this is the case where `source` provably
  cannot flip (no membership fetch ⇒ no sighting), which is exactly why the
  playability rule, not the gate, is what saves it.
- A members video that becomes public flips `source` to `rss` and is probed
  anonymously thereafter
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
- Top-N counts non-term-matching items. A non-term-matching item with a real date
  is never probed (matching today's `HasActiveJob` → terms → probe order) — **but a
  non-term-matching `assumed` item IS probed once**, to date it, or it could never
  be ranked and the top-N would silently be computed over a subset
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

**Phase 1** — independently shippable, fixes the bug. Nothing here depends on
Phase 2; the established gate simply rests on `last_rss_ok_at` alone until Phase 2
exists (see "Phase 1 limitation").

*Store*
- Schema v16: `feed_items`, `channel_state`, and **both** indexes —
  `idx_feed_items_rank` (partial) and `idx_feed_items_status`. Omitting the latter
  turns two per-cycle queries into full catalog scans.
- Precision-guarded upsert, **per-column**: `published`/`date_precision` move only
  upward; `source` updates on **every** sighting. Coupling them lets a date-quality
  rule pick the probe, and the wrong probe does not fail — it lies.
- Top-N in-scope query; archival scope excludes `assumed`, `upcoming`, `live`

*Monitor*
- Discovery pass / archival pass split, with the FRESH rule (`outcome == probed`)
- `post_live` → `vod` normalization on write; the probe write is precision-guarded
- **The terminal-status invariant**: never write `vod`/`not_a_stream` without a
  rankable date — a probe that classifies a past item but cannot date it leaves the
  row `unknown`. This is the rule that closes the only two sinks in the state space.
- The membership gate on **both** probe triggers (discovery and refresh)
- `source` becomes a **read** column: it selects the authenticated probe (replacing
  the in-memory `discoveredVideo.authProbe`) and gates the store queries — the
  **read** arms (top-N, cap-exempt union) on `MembershipDiscoveryEnabled()`
  (operator choice only), the **probe** arm on `membershipActive()` (which includes
  cookie state). The asymmetry is load-bearing: gating a read on cookie state moves
  scope on a fetch failure.
- The `assumed`-row probe carve-out (probed regardless of terms, to obtain a date)
- Refresh probe on the archival `vod` branch, writing its result back
- Established gate; rewired `checkChannel`/`processCandidate`
- **Channel-removal prune** from `kickMonitors`, mirroring `PruneHealth`
  (`feed.go:151-158`, called at `services.go:571-573`). Needed in Phase 1, not
  Phase 2: `feed_items` is populated by RSS/membership from day one, so without it a
  removed channel leaks rows forever. Phase 1's prune is the simple form — there is
  no backfill to cancel yet, so cancel-before-prune arrives with Phase 2.

*Refactors this depends on* (none of which exist today — budget them)
- **Split `ProcessYouTubeVideo`** into `probeAndClassify` (feed path: probe +
  classify + tracker/cooldown; no history writes, no verdict) and the existing
  composed function (DECAPI only, unchanged). Required because the feed path must
  consume probe *metadata*, not `ShouldProcess` — which is `false` for a successful
  probe of a non-jobbable item and would otherwise strand every plain upload at
  `unknown` forever — and because it must not inherit the hidden `AddToHistory`
  side effects at `utils.go:284/313/330`.
- **`VideoProbeResult.PlayabilityError`** — `VideoInfo` already carries it
  (`types.go:17-28`, parsed at `player_api_parsing.go:332-388`) but
  `VideoProbeResult` (`utils.go:32-36`) drops it, so the monitor cannot tell an
  observation from a refusal. Surfacing it is what makes the `denied` outcome
  possible, and `denied` is the only membership protection that does not depend on
  a stored value being current. Both `monitor_callbacks.go` wiring sites.
- **A publish date on the probe** — `VideoInfo.PublishedAt` +
  `VideoProbeResult.PublishedAt` + both `monitor_callbacks.go` wiring sites, with
  status-aware extraction:
  `post_live`/`vod` → `liveBroadcastDetails.startTimestamp` (`exact`), **else
  `uploadDate`/`publishDate` (`day`) — the fallback is NOT optional; it is the
  normal `vod` case, since an ended stream with an `endTimestamp` classifies
  `post_live`**; `not_a_stream` → `uploadDate` (`day`); `upcoming`/`live` → none.
  **No date exists in the probe chain today** — without this, an `assumed` row can
  never be promoted and members content becomes permanently unarchivable. Also adds
  the `day` rung to the precision ladder and the guard's `CASE`.
- **Unit-preserving `itemAge`** — today it discards the unit, making the coarse skew
  uncomputable. Propagates to `MembershipVideo.Age` and `membershipCandidates`.
- **An injectable RSS fetch seam + a new `feed_test.go`** — `checkChannel` calls
  `fm.fetchFeed` directly and has no test, so the headline regression test is not
  writable without this.

*Cleanup*
- Delete `TestMergeCandidatesRecencyCap` (asserts the deleted cap-crowding behavior)
- Remove `last_videos` + `GetLastVideo`/`SetLastVideo`
- The included fixes; doc updates (including the in-code comments)

**Phase 2** — lands on top of a shipped Phase 1.

*Scanner*
- InnerTube `/browse` continuation client, modelled on `internal/chat`
  (`api.go:172-178`, `downloader.go:412-423`, `:558-583`) — **not** ported from
  yt-dlp, which is only the reference for the browse response shape
- Three-tab scan (`/videos` + `/streams` + `/membership`), unioned and deduped;
  membership gated on `MembershipDiscoveryEnabled() && HasAuthCookies()`; the whole
  scan filtered to `GetPlatform() == "youtube"`
- Listing classification via `itemAge` (badge short-circuit included)
- Merged channel-global `catalog_pos` ordering pass (collect-then-update)

*Worker*
- Throttled, resumable backfill; rows written per page; `backfilled_at` set only on
  completion of the eligible tabs
- Early exit on re-runs (a full page of known IDs ends a tab), **gated on
  `backfilled_at IS NOT NULL`** — never on a first scan, where the store's existing
  RSS rows would trigger it against an unscanned catalogue
- The idempotent `backfilled_at IS NULL` sweep, with an **in-flight set** so
  repeated `kickMonitors` calls cannot launch concurrent scans of one channel
- **Cancel-before-prune**: the in-flight set is what lets `kickMonitors` cancel a
  scan for a departing channel before pruning its rows (Phase 1's prune is the
  simple form; this upgrades it)
- Debounced manual re-run (`R` chord + API), modelled on
  `POST /api/monitors/check-now` and its 30s `atomic.Int64` guard

*Then enabled by the scan's data*
- `last_listed_at` + deferral for rows no listing has mentioned for N days — the
  correct bound on dead-row probing that Phase 1 deliberately does without (see
  "Probe-list growth")
- Turning `membership_discovery` on must clear `backfilled_at` to force a rescan,
  since the eligible tab set changed

*UI*
- Progress via `hub.Broadcast` (web) + a TUI `tea.Msg` channel, **including** the
  `InitialState` seed (`ws_wiring.go:87-111`) — a long-running scan makes mid-flight
  connect the common case
