# Feed History 3/5 — Monitor Cycle Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rewrite the feed monitor's per-channel cycle as FETCH → STORE → WALK → ARCHIVE over `feed_items`, with the source walk (serial, per-source early exit, self-validating), the escalation, the membership gate asymmetry, the restart carve-out, the established gate, and the DECAPI window check — killing the original `gr-ZTohjwnQ` bug.

**Architecture:** `checkChannel` (`internal/monitor/feed.go:425-479`) is rewritten around the store: fetches only feed the upsert; both passes read `db.FeedScope`. Probing goes through Plan 2's `probeAndClassify`. Job creation goes through a widened `OnVideoFound` carrying a disposition Plan 4 consumes.

**Depends on:** Plans 1 and 2.

**Spec:** `docs/superpowers/specs/2026-07-15-feed-history.md` §7 (cycle, scope), §8 (walk, gates, restart), §9 (probes, denied use, escalation, membership gating), §10 (archive decisions), §11 (established gate), §13 (DECAPI). Global Constraints from Plan 1 apply. **One `now` per cycle:** `cutoff := time.Now().UTC().Add(-time.Duration(windowDays) * 24 * time.Hour)` computed once at cycle top; every window test uses it (§4).

---

### Task 1: RSS fetch seam + `feed_test.go` scaffold

**Files:**
- Modify: `internal/monitor/feed.go` (add injectable field beside `MembershipFetchFunc` at `:94`)
- Create: `internal/monitor/feed_test.go`

**Interfaces:**
- Produces: `FeedMonitor.FetchRSS func(ctx context.Context, ch *config.ChannelConfig) ([]byte, error)` — nil ⇒ default `fm.fetchFeed` (today's HTTP GET at `:484`); tests inject. Also a test constructor `newTestFeedMonitor(t, db, opts...)` used by every test in this plan.

- [ ] **Step 1:** Add the field; in `checkChannel` replace the direct `fm.fetchFeed(ctx, ch)` call (`:426`) with `fm.rssFetch(ctx, ch)` where:

```go
func (fm *FeedMonitor) rssFetch(ctx context.Context, ch *config.ChannelConfig) ([]byte, error) {
	if fm.FetchRSS != nil {
		return fm.FetchRSS(ctx, ch)
	}
	return fm.fetchFeed(ctx, ch)
}
```

- [ ] **Step 2:** Create `feed_test.go` with the constructor. Run `grep -n "func NewFeedMonitor" internal/monitor/feed.go` for the real constructor signature and wrap it; fixtures are inline strings (a minimal 2-item RSS XML — copy the shape from `membership_test.go:207`'s inline XML). Include helpers: `rssWith(items ...rssItem) fetchFunc`, `rss404() fetchFunc` (returns a 404-shaped error identical to what `fetchFeed` returns — grep `fetchFeed` for its error construction and mirror it), `membWith(videos ...youtube.MembershipVideo) MembershipFetchFunc`.
- [ ] **Step 3:** Smoke test: monitor constructed, one cycle runs against an empty store without panic. `go test -run TestFeedMonitorSmoke ./internal/monitor/ -v` → PASS.
- [ ] **Step 4: Commit** — `git commit -m "test(monitor): injectable RSS seam + feed_test scaffold"`

---

### Task 2: FETCH + STORE steps

**Files:**
- Modify: `internal/monitor/feed.go` (`checkChannel`, `parseFeedCandidates` `:586-645`, `membershipCandidates` `:670-685`)
- Test: `internal/monitor/feed_test.go`

**Interfaces:**
- Consumes: `db.UpsertFeedItem`, `db.SetChannelRSSOK` (Plan 1).
- Produces: `checkChannel` populates the store and returns `newIDs map[string]bool` (inserted this cycle) for the ARCHIVE step. `parseFeedCandidates` writes `source: "rss"` (renamed from `"feed"` at `:641` — rename the in-memory value too, or the discovery log disagrees with the store). Candidates carry their fetch index for `catalog_pos`.
- Rules (spec §7): RSS → `published = <published>`, `exact`; listings → `published = now - itemAge(item)`, `coarse` (or `assumed` + `published = cycle-now` when `itemAge` returns 0); **status is always `'unknown'`** (the upsert enforces it); `last_rss_ok_at` written **in FETCH**, immediately on success, not at cycle end.

- [ ] **Step 1: Write the failing tests**

```go
func TestStoreStep_UpsertsAndClassifiesDates(t *testing.T) {
	db := newTestDB(t)
	fm := newTestFeedMonitor(t, db,
		withRSS(rssWith(rssItem{ID: "pub1", Published: "2026-07-15T10:00:00Z", Title: "A"})),
		withMembership(membWith(
			memberVideo("m1", "3 weeks ago"), // coarse: now - 21d
			memberVideo("m2", ""),            // undatable: assumed, published = cycle now
		)))
	fm.runCycleForTest(t, "UC1")

	pub1 := mustGetFeedItem(t, db, "UC1", "pub1")
	if pub1.DatePrecision != "exact" || pub1.Source != "rss" || pub1.Status != "unknown" {
		t.Fatalf("rss row: %+v", pub1)
	}
	m1 := mustGetFeedItem(t, db, "UC1", "m1")
	if m1.DatePrecision != "coarse" || m1.Source != "membership" {
		t.Fatalf("coarse row: %+v", m1)
	}
	m2 := mustGetFeedItem(t, db, "UC1", "m2")
	if m2.DatePrecision != "assumed" {
		t.Fatalf("assumed row: %+v", m2)
	}
}

func TestFetchStep_RSSSuccessEstablishes_404DoesNot(t *testing.T) {
	db := newTestDB(t)
	fm := newTestFeedMonitor(t, db, withRSS(rss404()), withMembership(membWith()))
	fm.runCycleForTest(t, "UC1")
	if establishedForTest(t, db, "UC1") {
		t.Fatal("404 must not establish")
	}
	fm2 := newTestFeedMonitor(t, db, withRSS(rssWith()), withMembership(membWith()))
	fm2.runCycleForTest(t, "UC1") // zero entries but 200 — still establishes (§11 residual)
	if !establishedForTest(t, db, "UC1") {
		t.Fatal("empty-but-200 RSS must establish")
	}
}
```

`runCycleForTest` calls the rewritten `checkChannel` with a fixed clock injected (add `fm.now func() time.Time`, defaulting `time.Now`; tests pin it — the one-`now` rule makes this trivial). `establishedForTest` reads `channel_state.last_rss_ok_at`.

- [ ] **Step 2: Verify failure**, then **Step 3: Implement**:

Rewrite the top of `checkChannel`:

```go
cycleNow := fm.now().UTC()
cutoff := cycleNow.Add(-time.Duration(fm.archiveWindowDays(ch)) * 24 * time.Hour).Format(time.RFC3339)

// 1. FETCH — independent; neither failure is fatal.
data, rssErr := fm.rssFetch(ctx, ch)
if rssErr == nil {
	if err := fm.db.SetChannelRSSOK(chID, cycleNow.Format(time.RFC3339)); err != nil {
		fm.logger.Warn("last_rss_ok_at write failed", "err", err)
	}
}
var membVideos []youtube.MembershipVideo
if fm.membershipActive() { /* existing gated fetch, unchanged */ }

// 2. STORE — upsert every item seen; collect NEW ids.
newIDs := map[string]bool{}
first := cycleNow.Format(time.RFC3339)
for i, c := range fm.parseFeedCandidates(data, ch) { // skipped entirely when rssErr != nil
	ins, err := fm.db.UpsertFeedItem(database.FeedItem{
		ChannelID: chID, VideoID: c.videoID, Title: c.title,
		Published: c.published, DatePrecision: "exact",
		CatalogPos: i, Source: "rss", FirstSeen: first,
	})
	if err != nil { fm.logger.Warn("upsert failed; skipping item this cycle", "id", c.videoID, "err", err); continue }
	if ins { newIDs[c.videoID] = true }
}
for i, v := range membVideos {
	pub, prec := cycleNow.Format(time.RFC3339), "assumed"
	if v.Age > 0 {
		pub, prec = cycleNow.Add(-v.Age).Format(time.RFC3339), "coarse"
	}
	ins, err := fm.db.UpsertFeedItem(database.FeedItem{
		ChannelID: chID, VideoID: v.ID, Title: v.Title,
		Published: pub, DatePrecision: prec,
		CatalogPos: i, Source: "membership", FirstSeen: first,
	})
	if err != nil { fm.logger.Warn("upsert failed; skipping item this cycle", "id", v.ID, "err", err); continue }
	if ins { newIDs[v.ID] = true }
}
```

`chID` is the channel identifier `feed_items` is keyed by. Run: `grep -n "channel_id=" internal/monitor/feed.go` — use the same field `fetchFeed` uses to build the RSS URL (expected `ch.ChannelID`; if it differs, use what you find, everywhere in this plan). Rename `parseFeedCandidates`' source literal `"feed"` → `"rss"` (`:641`). **Delete the zero-candidate early return at `:457-459`** — a cycle with empty fetches must still run WALK + ARCHIVE (spec §7). Delete `mergeCandidates` and its cap (the walk replaces it); delete `TestMergeCandidatesRecencyCap` (`membership_test.go:83-108`).

- [ ] **Step 4: Verify pass** — `go test -run 'TestStoreStep|TestFetchStep' ./internal/monitor/ -v` → PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(monitor): FETCH+STORE — store-driven cycle, rss establishes in fetch"`

---

### Task 3: The WALK

**Files:**
- Create: `internal/monitor/walk.go`
- Test: `internal/monitor/walk_test.go`

**Interfaces:**
- Consumes: `db.FeedScope`, `db.HasAnyJob`, `db.HasActiveJob`, `db.ApplyProbeToFeedItem`, `db.SetFeedItemSource`, `probeAndClassify` (Plan 2).
- Produces: `fm.walk(ctx, ch, chID, cutoff string, scope []database.FeedItem) map[string]ProbeClassifyResult` — the FRESH set, keyed by videoID, consumed by ARCHIVE.
- Rules (spec §8, in order per row): skip if exhausted-source AND status ∉ {upcoming,live} AND precision ≠ assumed → skip if `HasActiveJob` → skip if NOT term-match → skip if `source='membership' && !membershipActive()` → status rule: probe `unknown/upcoming/live`; `vod` only via the restart carve-out (`vod` + `started` + `!HasAnyJob`). Serial. Exhaustion: only a dated probe of a **coarse** row, source date-ordered (never `rss`), sets it; ordering check has the **IS-SET guard**; `src` is the source the row carried when the walk read it (mid-walk relabel takes effect next cycle).

- [ ] **Step 1: Write the failing tests**

```go
func TestWalk_EarlyExitPerSource(t *testing.T) {
	// Spec §17: 10-day window; five "1 week ago" membership rows (one stored
	// published, catalog_pos 0..4) with true ages 8/9/11/12/13 days. Exactly
	// THREE probes: 8d, 9d, then 11d retires the source; 12d/13d never probed.
	db := newTestDB(t)
	now := fixedNow()
	seedCoarseLump(t, db, "UC1", now, "membership", "w1", "w2", "w3", "w4", "w5")
	var order []string
	probe := probeReturningTrueAges(now, map[string]int{"w1": 8, "w2": 9, "w3": 11, "w4": 12, "w5": 13}, &order)
	fm := newTestFeedMonitor(t, db, withProbe(probe), withWindowDays(10), withNow(now))
	fm.walkForTest(t, "UC1")
	if len(order) != 3 || order[0] != "w1" || order[1] != "w2" || order[2] != "w3" {
		t.Fatalf("probe order %v, want [w1 w2 w3] — serial, and exhaustion after the first out-of-window DATED probe", order)
	}
}

func TestWalk_OnlyDatedCoarseProbesRetire(t *testing.T) {
	// errored / denied / cooldown / dateless leave the source live (§8);
	// an assumed row's date is not a listing coordinate and never retires (§8).
	// Four sub-cases; each seeds two rows and asserts the SECOND is still probed.
	for _, mode := range []string{"errored", "denied", "cooldown", "dateless", "assumed"} {
		t.Run(mode, func(t *testing.T) { assertSecondRowStillProbed(t, mode) })
	}
}

func TestWalk_OrderingCheck(t *testing.T) {
	// 8d then 7d (newer) → early exit disabled for that source this cycle, all
	// rows probed, warning logged. And: the check must NOT fire on the FIRST
	// probe of a source (IS-SET guard) — zero-value time is older than any date.
	// And: per-source — a mis-ordered membership must not disable videos.
	/* three assertions over a seeded two-source scope */
}

func TestWalk_RestartCarveOut(t *testing.T) {
	db := newTestDB(t)
	now := fixedNow()
	seedRow(t, db, "UC1", "r1", now.AddDate(0, 0, -1), "started", "rss", "vod") // in-window ended broadcast
	// History row WITHOUT a job row (the HasProcessed trap, §8): via history only.
	db.AddToHistory("r1")
	var probed []string
	fm := newTestFeedMonitor(t, db, withProbe(recordingProbe(&probed)), withNow(now))
	fm.walkForTest(t, "UC1")
	if !contains(probed, "r1") {
		t.Fatal("vod+started with NO JOB ROW must be probed — the predicate is the jobs table, NOT HasProcessed")
	}
	// Now add a job row: never probed again.
	addFinishedJob(t, db, "r1")
	probed = nil
	fm.walkForTest(t, "UC1")
	if contains(probed, "r1") {
		t.Fatal("vod+started WITH a job row must not be probed — AddJob dedups re-jobs anyway")
	}
}
```

- [ ] **Step 2: Verify failure**, then **Step 3: Implement `walk.go`** (the spec's §8 pseudocode, literally):

```go
type walkState struct {
	exhausted map[string]bool
	lastDate  map[string]time.Time
	noExit    map[string]bool // ordering check tripped this cycle
}

func dateOrdered(source string) bool { return source != "rss" } // §8: rss is announcement-ordered

func (fm *FeedMonitor) walk(ctx context.Context, ch *config.ChannelConfig, chID, cutoff string, scope []database.FeedItem) map[string]ProbeClassifyResult {
	fresh := map[string]ProbeClassifyResult{}
	st := walkState{exhausted: map[string]bool{}, lastDate: map[string]time.Time{}, noExit: map[string]bool{}}
	for _, row := range scope {
		src := row.Source // the source the row carried when the walk read it (§8)
		if st.exhausted[src] && row.Status != "upcoming" && row.Status != "live" && row.DatePrecision != "assumed" {
			continue
		}
		if active, err := fm.db.HasActiveJob(row.VideoID); err != nil || active {
			continue // DB read error ⇒ skip the item, continue the cycle (§7)
		}
		if !fm.termMatch(ch, row.Title) { // title-only for store rows; reuse the existing matcher (grep: matchesTerms / feed.go:694-696 comment)
			continue
		}
		if src == "membership" && !fm.membershipActive() {
			continue
		}
		switch row.Status {
		case "unknown", "upcoming", "live":
			// probe below
		case "vod":
			hasJob, err := fm.db.HasAnyJob(row.VideoID)
			if err != nil || hasJob || row.DatePrecision != "started" {
				continue // restart carve-out only: vod + started + no job row (§8)
			}
		default:
			continue // not_a_stream
		}
		res := fm.probeRow(ctx, ch, chID, row) // Task 4: probe choice + escalation
		if res.Outcome == OutcomeProbed {
			fm.applyProbe(chID, row.VideoID, res) // normalization + terminal invariant, below
			fresh[row.VideoID] = res
			if res.PublishedAt != "" {
				d, _ := time.Parse(time.RFC3339, res.PublishedAt)
				if last, ok := st.lastDate[src]; ok && d.After(last) {
					st.noExit[src] = true // ordering disproved for THIS source (IS-SET guard)
					fm.logger.Warn("listing order violated; early exit disabled", "source", src, "channel", chID)
				}
				st.lastDate[src] = d
				if res.PublishedAt < cutoff && dateOrdered(src) && row.DatePrecision == "coarse" && !st.noExit[src] {
					st.exhausted[src] = true
				}
			}
		}
		// denied/errored/cooldown/dateless: never exhaust — the boundary was not learned (§8)
	}
	return fresh
}

// applyProbe: post_live→vod normalization and the terminal invariant (§12) live
// HERE, before the store write — a terminal status with no usable date stays 'unknown'.
func (fm *FeedMonitor) applyProbe(chID, videoID string, r ProbeClassifyResult) {
	status := r.StreamStatus
	if status == "post_live" {
		status = "vod"
	}
	if (status == "vod" || status == "not_a_stream") && r.PublishedAt == "" {
		status = "unknown"
	}
	if err := fm.db.ApplyProbeToFeedItem(chID, videoID, status, r.Title, r.PublishedAt, r.PublishedPrecision); err != nil {
		fm.logger.Warn("probe write failed; row retried next cycle", "id", videoID, "err", err)
	}
}
```

(RFC3339 strings compare lexically, so `res.PublishedAt < cutoff` is the window test — both are UTC. Keep it; it matches the one-`now` rule.)

- [ ] **Step 4: Verify pass** — `go test -run TestWalk ./internal/monitor/ -v` → PASS. Assert **ordering**, not counts (spec §17): the recording probe appends to a slice.
- [ ] **Step 5: Commit** — `git commit -m "feat(monitor): the source walk — serial, per-source early exit, self-validating"`

---

### Task 4: Probe choice + escalation

**Files:**
- Modify: `internal/monitor/walk.go` (add `probeRow`)
- Test: `internal/monitor/walk_test.go`

**Interfaces:**
- Consumes: `fm.ProbeVideo` wiring, `fm.ProbeVideoAuth` (`feed.go:131`), `fm.membershipActive()` (`:518-523`), `db.SetFeedItemSource`, `isDenied` via `probeAndClassify`.
- Rules (spec §9): `source='membership'` ⇒ authenticated probe, else anonymous. A `members_only` refusal from ANY probe flips `source:='membership'` **unconditionally**; if the probe was anonymous AND `membershipActive()`, re-probe with cookies **same cycle, bypassing the cooldown** (pass `Cooldown: nil` on the re-probe); classify the final result through the same outcome rules. `login_required` neither relabels nor escalates.

- [ ] **Step 1: Failing tests** — four, straight from spec §17:

```go
func TestEscalation_MembersOnlyFlipsAndEscalatesOnce(t *testing.T)
// anon probe → members_only ⇒ source flips in store; auth probe runs SAME cycle;
// auth returns ok/vod+date ⇒ OutcomeProbed ⇒ jobbed later. Next cycle: exactly ONE
// probe (authenticated) — assert via call recorder.

func TestEscalation_FailedEscalationStillFlips(t *testing.T)
// auth also returns members_only ⇒ OutcomeDenied, but source='membership' persists ⇒
// next cycle one auth probe, not two.

func TestEscalation_LoginRequiredNeverRelabels(t *testing.T)
// anon probe → upcoming+login_required ⇒ denied; assert source STILL 'rss' AND no
// second probe issued (anti-bot on public videos, §9).

func TestEscalation_NoCookiesNoEscalation(t *testing.T)
// membershipActive false: refusal flips source, no re-probe; next cycle the
// membership gate skips the row entirely (zero probes).
```

- [ ] **Step 2: Verify failure**, then **Step 3: Implement**:

```go
func (fm *FeedMonitor) probeRow(ctx context.Context, ch *config.ChannelConfig, chID string, row database.FeedItem) ProbeClassifyResult {
	probe := fm.probeAnon // the ProbeVideo wiring
	if row.Source == "membership" {
		probe = fm.probeAuth // ProbeVideoAuth wiring (feed.go:131)
	}
	res := probeAndClassify(ProbeClassifyParams{Ctx: ctx, VideoID: row.VideoID, Channel: ch,
		ProbeVideo: probe, Tracker: fm.MetadataTracker, Cooldown: fm.ProbeCooldown, Logger: fm.logger})
	if res.PlayabilityError == "members_only" {
		// The refusal IS the sighting (§9): relabel unconditionally.
		if err := fm.db.SetFeedItemSource(chID, row.VideoID, "membership"); err != nil {
			fm.logger.Warn("source flip failed", "id", row.VideoID, "err", err)
		}
		if row.Source != "membership" && fm.membershipActive() {
			// Same-cycle escalation, cooldown BYPASSED — the refusal already
			// recorded it (utils.go:299) and gating the retry on it would
			// suppress escalation for any probe_cooldown > 0 (§9).
			res = probeAndClassify(ProbeClassifyParams{Ctx: ctx, VideoID: row.VideoID, Channel: ch,
				ProbeVideo: fm.probeAuth, Tracker: fm.MetadataTracker, Cooldown: nil, Logger: fm.logger})
		}
	}
	return res
}
```

`fm.probeAnon`/`fm.probeAuth` adapt the existing `ProbeVideo`/`ProbeVideoAuth` fields to `func(ctx, id) (*VideoProbeResult, error)` — grep their exact types first: `grep -n "ProbeVideo\b\|ProbeVideoAuth" internal/monitor/feed.go | head -5`.

- [ ] **Step 4: Verify pass**, **Step 5: Commit** — `git commit -m "feat(monitor): probe choice by source + members_only escalation"`

---

### Task 5: The ARCHIVE step

**Files:**
- Modify: `internal/monitor/feed.go` (`checkChannel` tail; delete `processCandidate` `:697-763` — its logic moves here)
- Create/extend: `internal/monitor/archive.go`, tests in `feed_test.go`
- Modify: `internal/monitor/decapi.go` + `cmd/moombox/monitor_callbacks.go` (widened callback signature)

**Interfaces:**
- Produces: `type JobDisposition int` with `DispositionBroadcast` (live/upcoming — admitted, priority 0), `DispositionNewVOD` (admitted, priority 0), `DispositionBacklogVOD` (Queued, priority 1). `OnVideoFound(videoID, title, videoURL string, ch *config.ChannelConfig, d JobDisposition)` — **Plan 4 implements the creation semantics**; until then the host wiring maps every disposition to today's behavior (Upcoming + enqueue) with a `// PLAN4` marker. DECAPI passes `DispositionBroadcast` for live/upcoming results and `DispositionNewVOD` otherwise.
- Rules (spec §10, the decision table **in this order**): skip `HasActiveJob` → skip non-term-match → `upcoming/live` ⇒ job iff FRESH (never HasProcessed-gated) → `vod/not_a_stream` ⇒ skip if `HasProcessed`; skip unless `include_non_live_content`; skip unless established; skip membership-without-cookies; reuse the walk's FRESH result else refresh-probe (write back via `applyProbe`); then **denied FIRST** → no job; **outside window on the probe's date** → no job ever; live/upcoming → job; vod/not_a_stream → job (backlog disposition iff NOT in this cycle's `newIDs`); errored/cooldown → retry next cycle → `unknown` ⇒ never a job.

- [ ] **Step 1: Failing tests** (the load-bearing §17 rows):

```go
func TestArchive_HeadlineRegression(t *testing.T) {
	// THE bug: RSS 404 cycle + membership lists a 3-week-old members VOD,
	// window 3d, include_non_live_content=true ⇒ stored, NOT archived.
}
func TestArchive_Q2CarriesUpcomingPastWindow(t *testing.T) {
	// Announced 5 days out, window 3: cycle 1 stores exact(announcement)+probes
	// upcoming; advance fm.now 4 days (row outside Q1) ⇒ still in scope via Q2,
	// probe returns live ⇒ DispositionBroadcast job. Never missed.
}
func TestArchive_WindowRecheckOnProbeDate(t *testing.T) {
	// Coarse "1 week ago" row inside a 10-day window; refresh probe returns true
	// age 13d ⇒ NO job, date written back, next cycle not probed (out of Q1).
}
func TestArchive_FreshReuseNoDoubleProbe(t *testing.T) {
	// unknown row probed by the walk to vod ⇒ archive step must NOT probe again
	// this cycle (assert exactly one probe call), and still jobs it.
}
func TestArchive_UnknownNeverJobs_HasProcessedOnlyGatesVod(t *testing.T) {
	// (a) in-window unknown row with exact date whose probe errors ⇒ no job.
	// (b) history row with no job row + probe returns live ⇒ JOB (goal-3 guard).
}
func TestArchive_CookieLapseLongerThanWindow(t *testing.T) {
	// §17: members upcoming written assumed/unknown; cookies off; advance past the
	// window; cookies on ⇒ row still in scope (unresolved arm), probed, jobbed
	// when live.
}
```

- [ ] **Step 2: Verify failure**, then **Step 3: Implement** `fm.archive(ctx, ch, chID, cutoff, scope, newIDs, fresh)` translating the table above line-for-line; disposition:

```go
d := DispositionBacklogVOD
if newIDs[row.VideoID] {
	d = DispositionNewVOD
}
// live/upcoming results always use DispositionBroadcast, whatever newIDs says.
```

Wire `checkChannel`: `scope := fm.db.FeedScope(chID, cutoff, fm.membershipDiscoveryEnabled())` (config toggle ONLY — never `membershipActive()`; add the small accessor reading `MembershipDiscoveryEnabled` from the config store, mirroring `monitor_callbacks.go:218-224`'s config half), then `fresh := fm.walk(...)`, then **re-read** `scope = fm.db.FeedScope(...)` (the walk corrected dates), then `fm.archive(...)`. WALK and ARCHIVE each get their own context: `walkCtx, cancel := context.WithTimeout(ctx, passBudget(len(scope)))` where

```go
func passBudget(rows int) time.Duration { // §7: scales with scope, floor 60s, cap 15m
	d := 60*time.Second + time.Duration(rows)*time.Second
	if d > 15*time.Minute {
		d = 15 * time.Minute
	}
	return d
}
```

The established gate: `established := last_rss_ok_at != nil || backfilled_at != nil` read from `channel_state` (add `db.GetChannelEstablished(chID) (bool, error)` — a missing row is NOT established).

- [ ] **Step 4: Verify pass** — full `go test ./internal/monitor/ -v`.
- [ ] **Step 5: Commit** — `git commit -m "feat(monitor): archive pass — decision table, FRESH reuse, window re-check"`

---

### Task 6: DECAPI window check

**Files:**
- Modify: `internal/monitor/decapi.go` (after the `ProcessYouTubeVideo` call at `:583-594`, before `OnVideoFound` at `:599-600`)
- Test: `internal/monitor/decapi_test.go` (create; DECAPI has no test file — same inline-fixture rule)

- [ ] **Step 1: Failing tests** — (a) probe returns `vod` with `PublishedAt` 6 months old, window 3 ⇒ **no** `OnVideoFound` call, debug log; (b) probe returns `live` with no date ⇒ `OnVideoFound` fires (a date must never block the redundancy, §13); (c) probe returns `vod` with **no** date ⇒ no job (cannot verify the window ⇒ treat as outside — DECAPI writes no store row, so there is no self-healing `unknown` path here; log at Info so it is visible).
- [ ] **Step 2:** Implement:

```go
if result.ShouldProcess && (result.StreamStatus == "vod" || result.StreamStatus == "post_live" || result.StreamStatus == "not_a_stream") {
	cutoff := time.Now().UTC().Add(-time.Duration(dm.archiveWindowDays(ch)) * 24 * time.Hour).Format(time.RFC3339)
	if result.PublishedAt == "" || result.PublishedAt < cutoff {
		dm.logger.Info("decapi: newest video is outside the archive window; skipping",
			"videoID", videoID, "published", result.PublishedAt)
		return nil
	}
}
```

(`dm.archiveWindowDays` — same resolver shape as `fm`'s; share it via a package-level helper `resolveArchiveWindowDays(store, ch)` used by both monitors so the rule exists once.)

- [ ] **Step 3: Verify pass**, **Step 4: Commit** — `git commit -m "feat(decapi): window-check vod finds only"`

## Self-check before handoff

- `grep -n "membershipActive" internal/monitor/feed.go internal/monitor/walk.go internal/monitor/archive.go` — appears in PROBE gates only, never in a `FeedScope` call (reads use the config toggle; cookie state must not move scope, §9).
- `grep -c "isDenied" internal/monitor/` — still exactly one definition; the archive step branches on `OutcomeDenied`, never re-tests playability.
- The deleted code is really gone: `mergeCandidates`, `processCandidate`, `discoveredVideo.authProbe`, the `:457-459` early return.
