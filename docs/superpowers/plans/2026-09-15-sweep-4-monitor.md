# Sweep Arc 4 — Monitor, Notifications, Connectivity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop the three monitors from spending request budget and probe deadlines on answers that cannot change, make the connectivity boot probe fire once, and make a rejected Discord webhook and a non-200 feed response say what happened without reading the whole body.

**Architecture:** Eight independently reviewable tasks on branch `sweep-4-monitor`. The DECAPI monitor gains a probe context of its own and a per-channel terminal memo; the feed monitor gains a non-member memo with a hard "at least one authenticated fetch per cycle" floor so the YouTube liveness signal never goes dark; the Twitch monitor's inter-chunk stagger moves out of the success path; `connectivity.Monitor.Start` seeds from one probe instead of two; Discord quotes the rejected body; the feed's non-200 drain is bounded like DECAPI's. Every behavioural change is pinned by a test written red first.

**Tech Stack:** Go 1.27, stdlib only (`net/http/httptest`, `context`, `sync`), `modernc/sqlite` via `internal/database` in the monitor tests, no new dependencies.

**Spec:** `docs/superpowers/specs/2026-09-15-sweep-fix-chain-design.md` — **§7 (Arc 4)** is this plan's mandate; §1–§3 and §11–§12 bind it. Ledger items: T1-10, T2-12, T2-13, T3-28, T3-30, T4-35 (Discord body; feed drain) in `reports/sweep-2026-09-15.md`.

## Global Constraints

Copied verbatim from spec §3 — every task's requirements implicitly include all of these:

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

### Arc gates (spec §7, plus §2's per-arc merge gates)

Run after every task, and again on the finished branch before the merge candidate:

- `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/monitor/...`
- `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/notifications/...`
- `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/connectivity/...`
- `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/docs/`
- Task 3 additionally gates `./internal/youtube/...` and `./cmd/moombox/...` (it changes a signature declared in `internal/youtube` and consumed in `cmd/moombox`).
- Merge candidate (spec §2): `gofmt -l ./cmd ./internal ./tools ./web` empty, `go vet ./...`,
  `staticcheck ./...` (pinned 2026.2.1) clean, `go build ./...`, `GOOS=linux GOARCH=amd64 go build ./...`,
  `GOOS=linux GOARCH=arm64 go build ./...`, ONE controller-run `go test -count=1 ./...`.
  No JS changes in this arc, so the node suite is not required. No extraction/goja/cipher/sidecar
  changes, so the live gates are not required.
- Every go command is prefixed `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp`.

### Arc-specific rulings

- **Ruling: `monitors.probe_cooldown` keeps its 0 (disabled) default — no task reads, writes, or proposes a default for it.** Why: standing owner ruling (`feedback_probe_cooldown_disabled_default`); the poll intervals are the throttle. Cost if wrong: the memos in Tasks 2 and 3 would be re-litigated as a cooldown change and rejected.
- **Ruling: the only connectivity change is the boot double-probe (T3-30); no probe-cadence, probe-target, or fast-path work.** Why: the 2026-07-18 connectivity audit rejected every perf candidate with reasons (`project_connectivity_optimization_audit`), and Task 5 is a correctness/boot-latency fix, not a cadence change. Cost if wrong: re-proposing rejected work.
- **Ruling: Task 3 extends `youtube.Service.FetchMembershipVideos` with a `hasAccess` result and touches `cmd/moombox/monitor_callbacks.go`, beyond §7's package list.** Why: the memo must never latch onto a MEMBER, and at the monitor boundary a member with an empty tab and a non-member are today byte-identical (`internal/youtube/channel_membership.go:121-124` collapses both to a nil slice; the adapter at `cmd/moombox/monitor_callbacks.go:1014` then rebuilds a non-nil empty slice for both). Cost if wrong: a members-only live stream on a member channel whose tab was empty at memo time goes undiscovered for up to 6 h — exactly the archive miss membership discovery exists to prevent. File sets stay disjoint from the concurrent Arc 3 (which edits `watch_page.go`, `player_api_strategy.go`, `player_api.go`, `pot_provider.go`, `jar.go`), and Arc 7 (the other `cmd/moombox` arc) runs after this one in §11's order.
- **Ruling: a membership fetch that ERRORS consumes the cycle's liveness nomination and writes no memo.** Why: a failed fetch is not a verdict (the same rule `routeLivenessVerdict` applies at `cmd/moombox/monitor_callbacks.go:865-872`), and a cycle whose one nominated fetch failed is a cycle where the network is down for every channel anyway. Cost if wrong: one cycle without a liveness observation during a transient failure; the next cycle nominates again.

### Anchor corrections (verified at `9975dbd1`, one docs-only commit above the spec's `5bbf16e8`)

- §7.1 cites `decapi.go:428, :488` — correct (`context.WithTimeout` at :428, `processResponse(ctx, …)` at :488). `checkChannel` itself starts at **:425**.
- §7.2 cites `processResponse ≈:574-592` — correct (`HasProcessed` at :574, `ProcessYouTubeVideo(` at :592). `processResponse` itself starts at **:532**.
- §7.2 cites the feed walk's terminal skip as `walk.go:96-98`; the whole skip is **`walk.go:95-107`** (`:96-98` is only the `case "unknown", "upcoming", "live"` arm; the `vod` carve-out is :98-103 and `not_a_stream` is :104-106).
- §7.3 cites `feed.go:513-520` and `channel_membership.go:121-124` — both correct.
- §7.3 cites `monitor_callbacks.go:1055-1062` for the accepted cost; the `ObserveLiveness` CALL is at **`cmd/moombox/monitor_callbacks.go:1010`** (`routeLivenessVerdict(s.cookieRefresh.ObserveLiveness, verdict)`), inside the `FetchMembership` adapter that starts at :993.
- §7.4 cites `twitch.go:312-318, :339` — correct (`wholeErr` early `continue` at :312-318, `time.NewTimer(twitchStagger)` at :339).
- §7.5 cites `connectivity/monitor.go:89-91` — correct.
- §7.6 cites `discord.go:132, :178` — correct (`sendOnce`'s default arm at :132, `Send`'s default arm at :178; `Send`'s 5xx arm is at :175-177).
- §7.7 cites `feed.go:635` — correct.

---

### Task 1: DECAPI probe budget (T1-10)

Today `checkChannel` wraps the whole channel check in one `decapiRequestTimeout` (15 s) context and hands that same context to the classification phase, so the player probe and the §9 date fetch run on whatever is left after DECAPI answered. A 14 s DECAPI answer leaves the probe ~1 s.

**Files:**
- Modify: `internal/monitor/decapi.go:21-26` (const block), `:425-489` (`checkChannel` split)
- Test: `internal/monitor/decapi_test.go` (append; add `net/http` + `net/http/httptest` imports)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `const decapiProbeBudget = 60 * time.Second`
  - `var decapiLatestVideoURL = "https://decapi.me/youtube/latest_video?id=%s"` (test seam)
  - `func (dm *DecapiMonitor) fetchLatestVideo(ctx context.Context, ch *config.ChannelConfig) (string, error)`
  - `func (dm *DecapiMonitor) checkChannel(ctx context.Context, ch *config.ChannelConfig) error` (signature unchanged)

- [ ] **Step 1: Write the failing test**

Append to `internal/monitor/decapi_test.go`, and add `"net/http"` and `"net/http/httptest"` to its import block:

```go
// TestDecapi_ProbeBudgetIsIndependentOfTheRequestTimeout pins T1-10: the
// classification phase must NOT inherit the DECAPI request deadline.
//
// The old code wrapped the whole channel check in one decapiRequestTimeout
// context and passed it to processResponse, so the player probe and the §9
// date fetch shared whatever was left of 15 s after DECAPI answered. The
// fixture's server is fast; it does not need to burn 14 s to kill that
// mutant, because a probe running under the REQUEST context sees a deadline
// of at most decapiRequestTimeout (15 s) no matter how quick the answer was,
// and the assertion below demands 30 s.
//
// Mutants this fails on:
//   - `return dm.processResponse(ctx, …)` with the request context: deadline ~15 s.
//   - no probe context at all: the cycle context here carries no deadline.
//   - `context.WithTimeout(context.Background(), decapiProbeBudget)`: the
//     cancel-propagation assertion fires (a stopped monitor must cancel an
//     in-flight probe).
func TestDecapi_ProbeBudgetIsIndependentOfTheRequestTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond) // DECAPI latency, in miniature
		fmt.Fprint(w, decapiBody("vidDecBud06", "budget probe"))
	}))
	t.Cleanup(srv.Close)

	origURL := decapiLatestVideoURL
	decapiLatestVideoURL = srv.URL + "?id=%s"
	t.Cleanup(func() { decapiLatestVideoURL = origURL })

	cycleCtx, cancelCycle := context.WithCancel(context.Background())
	t.Cleanup(cancelCycle)

	var (
		budget             time.Duration
		hadDeadline        bool
		cancelledWithCycle bool
	)
	db := newTestDB(t)
	dm := newTestDecapiMonitor(t, db, func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		var dl time.Time
		dl, hadDeadline = ctx.Deadline()
		budget = time.Until(dl)
		// A CHILD of the cycle context, not of context.Background(): stopping
		// the monitor has to cancel a probe that is already in flight.
		cancelCycle()
		cancelledWithCycle = ctx.Err() != nil
		return &VideoProbeResult{StreamStatus: "live", Title: "budget probe"}, nil
	})

	ch := &config.ChannelConfig{ID: "UC1", Name: "UC1"}
	if err := dm.checkChannel(cycleCtx, ch); err != nil {
		t.Fatalf("checkChannel: %v", err)
	}
	if !hadDeadline {
		t.Fatal("the probe context carried no deadline — the probe must run under decapiProbeBudget")
	}
	if budget < 30*time.Second {
		t.Fatalf("probe budget = %v, want >= 30s — the probe is still running under the %v request timeout", budget, decapiRequestTimeout)
	}
	if budget > decapiProbeBudget {
		t.Fatalf("probe budget = %v, want <= %v", budget, decapiProbeBudget)
	}
	if !cancelledWithCycle {
		t.Fatal("cancelling the cycle context did not cancel the probe context — the probe budget must derive from the cycle context, not context.Background()")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/monitor/ -run TestDecapi_ProbeBudgetIsIndependentOfTheRequestTimeout -count=1 -v`

Expected: FAIL to COMPILE with `undefined: decapiLatestVideoURL` and `undefined: decapiProbeBudget`.

- [ ] **Step 3: Add the constant and the URL seam**

In `internal/monitor/decapi.go`, replace the const block at `:21-26` with:

```go
const (
	decapiRequestTimeout   = 15 * time.Second
	decapiStagger          = 1 * time.Second
	decapiMinInterval      = 15 * time.Second
	decapiDefaultRateLimit = 60
	// decapiProbeBudget bounds the classification work that runs AFTER the
	// DECAPI body is in hand — ProcessYouTubeVideo's player probe and, for a
	// dateless vod-family result, the §9 date fetch. It is derived from the
	// CYCLE context, never from the request context: a DECAPI answer that took
	// 14 s used to leave the probe ~1 s, and a probe that dies on a leftover
	// deadline is a MISSED live stream, not a slow one.
	decapiProbeBudget = 60 * time.Second
)

// decapiLatestVideoURL is the latest-video endpoint as a printf template. A
// package var so a test can aim checkChannel at an httptest server — the same
// seam shape internal/youtube uses for membershipPageBase. Production never
// rewrites it.
var decapiLatestVideoURL = "https://decapi.me/youtube/latest_video?id=%s"
```

- [ ] **Step 4: Split checkChannel into fetch and classify phases**

In `internal/monitor/decapi.go`, replace `checkChannel` (`:425-489`) with:

```go
// checkChannel polls one channel's latest video and classifies it.
//
// The two phases own SEPARATE contexts on purpose (T1-10). fetchLatestVideo
// holds the decapiRequestTimeout deadline and releases it the moment the body
// is read; the classification phase then runs under a fresh decapiProbeBudget
// derived from the cycle context, so how long the probe gets never depends on
// how long DECAPI took to answer.
func (dm *DecapiMonitor) checkChannel(ctx context.Context, ch *config.ChannelConfig) error {
	body, err := dm.fetchLatestVideo(ctx, ch)
	if err != nil {
		return err
	}

	probeCtx, cancel := context.WithTimeout(ctx, decapiProbeBudget)
	defer cancel()
	return dm.processResponse(probeCtx, body, ch)
}

// fetchLatestVideo GETs the channel's DECAPI latest_video line under the
// request timeout and returns the raw body. Rate-limit accounting and the
// passive-connectivity report live here; the timeout context is cancelled
// before this returns, so nothing downstream inherits it.
func (dm *DecapiMonitor) fetchLatestVideo(ctx context.Context, ch *config.ChannelConfig) (string, error) {
	url := fmt.Sprintf(decapiLatestVideoURL, ch.ID)

	ctx, cancel := context.WithTimeout(ctx, decapiRequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Moombox/1.0")

	resp, err := monitorHTTPClient.Do(req)
	if err != nil {
		// Transport-level failure — feeds the passive offline tracker.
		reportMonitorResult("monitor/decapi", true)
		return "", fmt.Errorf("decapi request: %w", err)
	}
	defer resp.Body.Close()

	// Start a 1-minute rate limit window if none is active, and decrement
	// remaining count after response (matches TS fetchDecapi post-fetch).
	dm.mu.Lock()
	if dm.rateLimit.resetAt.IsZero() {
		dm.rateLimit.resetAt = time.Now().Add(60 * time.Second)
	}
	if dm.rateLimit.remaining > 0 {
		dm.rateLimit.remaining--
	}
	dm.mu.Unlock()

	// Always update rate limit from headers (server headers override)
	dm.updateRateLimit(resp)

	if resp.StatusCode == http.StatusTooManyRequests {
		// 429 reached the server — explicit throttle, not a connectivity
		// problem. Don't report as failure or success.
		retryAfter := resp.Header.Get("Retry-After")
		if secs, err := strconv.Atoi(retryAfter); err == nil {
			dm.mu.Lock()
			dm.rateLimit.resetAt = time.Now().Add(time.Duration(secs) * time.Second)
			dm.rateLimit.remaining = 0
			dm.mu.Unlock()
		}
		// Drain so the connection can be reused (closing an unread body
		// discards the TCP connection — costly during a sustained 429 storm).
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("rate limited (429)")
	}

	if resp.StatusCode != http.StatusOK {
		// Non-2xx — server reachable but unhappy; leave tracker alone.
		// Drain a bounded amount before close to keep the connection reusable.
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("decapi http %d", resp.StatusCode)
	}
	reportMonitorResult("monitor/decapi", false)

	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20)) // 5MB limit
	if err != nil {
		return "", err
	}
	return string(body), nil
}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/monitor/ -run TestDecapi_ -count=1 -v`

Expected: PASS, including the four pre-existing `TestDecapi_*` tests.

- [ ] **Step 6: Run the package and vet**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/monitor/... && GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/monitor/`

Expected: `ok` and no vet output.

- [ ] **Step 7: Commit**

```bash
git add internal/monitor/decapi.go internal/monitor/decapi_test.go
git commit -m "fix(monitor): give DECAPI probes a budget of their own" -m "checkChannel wrapped the whole channel check in one 15 s request context and handed it to processResponse, so the player probe and the date fetch ran on whatever DECAPI left behind — a 14 s answer left the probe ~1 s. Split the fetch into fetchLatestVideo, which owns and releases the request deadline, and run the classification under a fresh 60 s decapiProbeBudget derived from the cycle context." -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq"
```

---

### Task 2: DECAPI terminal memo (T2-12)

DECAPI reports a channel's NEWEST video. On a dormant channel that is the same finished VOD every cycle, so with the 15 s interval floor Moombox spends ~240 anonymous player probes per hour per channel re-learning an answer that cannot change. The feed walk already refuses to re-probe terminal rows (`internal/monitor/walk.go:95-107`).

**How "processed" is determined:** `dm.db.HasProcessed(videoID)` at `internal/monitor/decapi.go:574`, whose result is the local `reprobe`. `HasProcessed` is declared at `internal/database/database_extras.go:10-23` and is a single indexed read of the `history` table (`SELECT 1 FROM history WHERE video_id = ? LIMIT 1`). It is deliberately NOT folded into the memo: clearing an orphaned history row is the documented remedy for putting a video back in play (`internal/database/database_extras.go:48-56`, `DeleteHistoryEntries` at :96), and that must take effect on the very next cycle.

**Files:**
- Modify: `internal/monitor/decapi.go:37-79` (struct field), `:84-91` (`PruneHealth`), `:532-655` (`processResponse`), plus new helpers appended after `archiveWindowDays`
- Modify: `internal/monitor/utils.go:217-232` (`ProcessYouTubeVideoResult.StreamStatus` doc), `:454-480` (two skip returns)
- Test: `internal/monitor/decapi_test.go` (append)

**Interfaces:**
- Consumes: Task 1's `fetchLatestVideo` / `checkChannel` split (this task edits `processResponse`, which Task 1 left untouched).
- Produces:
  - `type decapiTerminalMemo struct { videoID string; status string }`
  - `func decapiTerminalStatus(status string) bool`
  - `func (dm *DecapiMonitor) terminalMemoHit(channelID, videoID string) bool`
  - `func (dm *DecapiMonitor) recordTerminalMemo(channelID, videoID, status string)`
  - `ProcessYouTubeVideoResult.StreamStatus` is now populated on the two "skipped" arms too (was empty).

- [ ] **Step 1: Write the failing tests**

Append to `internal/monitor/decapi_test.go`:

```go
// TestDecapi_TerminalMemoSkipsTheUnchangedNewestVideo pins T2-12's first
// case: a channel whose newest video is a processed, terminal VOD must not be
// re-probed every cycle.
//
// Mutant: dropping the memo check restores two probes — ~240 anonymous player
// calls/hour/channel for an answer that cannot change.
func TestDecapi_TerminalMemoSkipsTheUnchangedNewestVideo(t *testing.T) {
	db := newTestDB(t)
	probes := 0
	dm := newTestDecapiMonitor(t, db, func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		probes++
		return &VideoProbeResult{StreamStatus: "vod", Title: "finished vod"}, nil
	})

	// include_non_live_content is off, so the vod arm skips AND writes the
	// history row that makes the next sighting a re-probe.
	ch := &config.ChannelConfig{ID: "UC1", Name: "UC1"}
	body := decapiBody("vidDecMem07", "finished vod")
	for cycle := range 2 {
		if err := dm.processResponse(context.Background(), body, ch); err != nil {
			t.Fatalf("cycle %d: processResponse: %v", cycle, err)
		}
	}
	if probes != 1 {
		t.Fatalf("probes = %d, want 1 — the second cycle must skip a processed terminal VOD it already classified", probes)
	}
}

// TestDecapi_TerminalMemoProbesANewVideoID pins the reset half: publishing
// something new must re-open the channel immediately.
//
// Mutant: memoizing per CHANNEL without comparing the video ID blinds the
// channel forever — the headline monitor bug through a third door.
func TestDecapi_TerminalMemoProbesANewVideoID(t *testing.T) {
	db := newTestDB(t)
	var probed []string
	dm := newTestDecapiMonitor(t, db, func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		probed = append(probed, videoID)
		return &VideoProbeResult{StreamStatus: "vod", Title: "finished vod"}, nil
	})

	ch := &config.ChannelConfig{ID: "UC1", Name: "UC1"}
	if err := dm.processResponse(context.Background(), decapiBody("vidDecOld08", "old vod"), ch); err != nil {
		t.Fatalf("cycle 1: %v", err)
	}
	if err := dm.processResponse(context.Background(), decapiBody("vidDecNew09", "new vod"), ch); err != nil {
		t.Fatalf("cycle 2: %v", err)
	}
	if len(probed) != 2 || probed[0] != "vidDecOld08" || probed[1] != "vidDecNew09" {
		t.Fatalf("probed = %v, want [vidDecOld08 vidDecNew09] — a new newest video must reset the memo", probed)
	}
}

// TestDecapi_TerminalMemoDoesNotLatchOnUpcoming pins the terminal predicate:
// an upcoming stream's classification changes (upcoming -> live -> vod), so
// memoizing it would mean never noticing it went live.
//
// Mutant: treating any classification as terminal probes once and stops.
func TestDecapi_TerminalMemoDoesNotLatchOnUpcoming(t *testing.T) {
	db := newTestDB(t)
	probes := 0
	dm := newTestDecapiMonitor(t, db, func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		probes++
		return &VideoProbeResult{StreamStatus: "upcoming", Title: "premiere"}, nil
	})
	recordDecapiVideoFound(dm)

	ch := &config.ChannelConfig{ID: "UC1", Name: "UC1"}
	body := decapiBody("vidDecUpc10", "premiere")
	for cycle := range 2 {
		if err := dm.processResponse(context.Background(), body, ch); err != nil {
			t.Fatalf("cycle %d: processResponse: %v", cycle, err)
		}
	}
	if probes != 2 {
		t.Fatalf("probes = %d, want 2 — an upcoming classification is not terminal", probes)
	}
}

// TestDecapi_TerminalMemoReleasesWhenHistoryIsCleared pins the third conjunct.
// Clearing an orphaned history row is the documented way to put a video back
// in play (internal/database/database_extras.go:48-56), so the memo must be
// gated on a LIVE HasProcessed read, never on a remembered one.
//
// Mutant: caching the processed flag alongside the memo makes the operator's
// remedy silently do nothing.
func TestDecapi_TerminalMemoReleasesWhenHistoryIsCleared(t *testing.T) {
	db := newTestDB(t)
	probes := 0
	dm := newTestDecapiMonitor(t, db, func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		probes++
		return &VideoProbeResult{StreamStatus: "vod", Title: "finished vod"}, nil
	})

	ch := &config.ChannelConfig{ID: "UC1", Name: "UC1"}
	body := decapiBody("vidDecClr11", "finished vod")
	if err := dm.processResponse(context.Background(), body, ch); err != nil {
		t.Fatalf("cycle 1: %v", err)
	}
	if n, err := db.DeleteHistoryEntries([]string{"vidDecClr11"}); err != nil || n != 1 {
		t.Fatalf("DeleteHistoryEntries = (%d, %v), want (1, nil)", n, err)
	}
	if err := dm.processResponse(context.Background(), body, ch); err != nil {
		t.Fatalf("cycle 2: %v", err)
	}
	if probes != 2 {
		t.Fatalf("probes = %d, want 2 — clearing the history row must re-open the video on the next cycle", probes)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/monitor/ -run 'TestDecapi_TerminalMemo' -count=1 -v`

Expected: `TestDecapi_TerminalMemoSkipsTheUnchangedNewestVideo` FAILS with `probes = 2, want 1`; the other three PASS (they pin behaviour that must survive).

- [ ] **Step 3: Make the skipped classifications report their status**

In `internal/monitor/utils.go`, replace the `StreamStatus` doc lines at `:222-226` with:

```go
	// StreamStatus is the probe's classification ("live", "upcoming", "vod",
	// "post_live", "not_a_stream"). It is populated whenever a probe COMPLETED
	// — including the two arms that then decline to process the video — and is
	// empty only when no probe ran (the nil-probe passthrough) or the probe
	// errored or was suppressed by the cooldown. DECAPI reads it to pick a
	// JobDisposition (spec §10's creator table) and, since T2-12, to memoize a
	// terminal classification it must not pay for again next cycle.
	StreamStatus string
```

In the same file, replace the `not_a_stream` skip return at `:460`:

```go
			return ProcessYouTubeVideoResult{ShouldProcess: false, Title: p.Title, StreamStatus: cr.StreamStatus}
```

and the `post_live`/`vod` skip return at `:477`:

```go
			return ProcessYouTubeVideoResult{ShouldProcess: false, Title: p.Title, StreamStatus: cr.StreamStatus}
```

(The `OutcomeCooldown` and `OutcomeErrored` returns keep an empty `StreamStatus` — an absent classification is not a terminal one.)

- [ ] **Step 4: Add the memo to the DECAPI monitor**

In `internal/monitor/decapi.go`, add the memo type immediately above `// DecapiMonitor polls DECAPI…` (before `:37`):

```go
// decapiTerminalMemo is one channel's last DECAPI answer: the newest video ID
// the endpoint reported and the classification the probe gave it.
type decapiTerminalMemo struct {
	videoID string
	status  string
}

// decapiTerminalStatus reports whether a classification can no longer change.
// Only "vod" and "not_a_stream" qualify: "upcoming" becomes "live" becomes
// "vod", and "post_live" is the transitional state that becomes "vod". This is
// the same terminal set the feed walk refuses to re-probe (walk.go:95-107).
func decapiTerminalStatus(status string) bool {
	return status == "vod" || status == "not_a_stream"
}
```

Add the field to the `DecapiMonitor` struct, immediately after `rateLimit rateLimitState` (`:52`):

```go
	// terminalMemo remembers, per channel, the newest video ID DECAPI reported
	// and what the probe made of it. Guarded by mu; keyed by channel ID, so it
	// is bounded by the configured channel list and pruned by PruneHealth.
	terminalMemo map[string]decapiTerminalMemo
```

Replace `PruneHealth` (`:84-91`) with:

```go
// PruneHealth drops health entries — and terminal memos — for channels no
// longer configured.
func (dm *DecapiMonitor) PruneHealth() {
	active := make(map[string]struct{})
	for _, ch := range dm.getYouTubeChannels() {
		active[ch.ID] = struct{}{}
	}
	dm.health.prune(active)

	dm.mu.Lock()
	for id := range dm.terminalMemo {
		if _, ok := active[id]; !ok {
			delete(dm.terminalMemo, id)
		}
	}
	dm.mu.Unlock()
}
```

Append the two accessors after `archiveWindowDays` (after `:663`):

```go
// terminalMemoHit reports whether videoID is the exact video this channel's
// last completed probe classified as terminal. A different ID — the channel
// published something new — is a miss, and the fresh probe overwrites the memo.
func (dm *DecapiMonitor) terminalMemoHit(channelID, videoID string) bool {
	dm.mu.Lock()
	defer dm.mu.Unlock()
	m, ok := dm.terminalMemo[channelID]
	return ok && m.videoID == videoID && decapiTerminalStatus(m.status)
}

// recordTerminalMemo stores this cycle's classification for the channel,
// replacing any previous one. An empty status (probe errored, cooldown
// suppressed it, or no probe is wired) is recorded as-is and is not terminal,
// so the next cycle probes again.
func (dm *DecapiMonitor) recordTerminalMemo(channelID, videoID, status string) {
	dm.mu.Lock()
	defer dm.mu.Unlock()
	if dm.terminalMemo == nil {
		dm.terminalMemo = make(map[string]decapiTerminalMemo)
	}
	dm.terminalMemo[channelID] = decapiTerminalMemo{videoID: videoID, status: status}
}
```

- [ ] **Step 5: Gate the probe on the memo**

In `internal/monitor/decapi.go`, immediately after the `reprobe, hpErr := dm.db.HasProcessed(videoID)` block (`:574-577`) and BEFORE the `if reprobe {` log block at `:579`, insert:

```go
	// Terminal memo (T2-12): DECAPI reports the channel's NEWEST video, so a
	// dormant channel returns the same finished VOD every cycle — with the
	// 15 s interval floor that is ~240 anonymous player probes/hour/channel
	// for an answer that cannot change. Skip only when all three hold: the ID
	// is the one we classified last, that classification was terminal, and
	// history still says the video was processed.
	//
	// The HasProcessed read above is deliberately NOT memoized. Clearing an
	// orphaned history row is the documented way to put a video back in play
	// (database_extras.go:48-56), and it has to work on the very next cycle.
	if reprobe && dm.terminalMemoHit(ch.ID, videoID) {
		dm.logger.Debug("decapi: newest video unchanged and terminal; skipping re-probe",
			"videoID", videoID, "channel", ch.Name)
		return nil
	}
```

Then, immediately after the `result := ProcessYouTubeVideo(ProcessYouTubeVideoParams{…})` call closes (after `:603`), insert:

```go
	// Record what the probe made of this ID so the next cycle can skip a
	// classification that cannot change. An errored or cooled-down probe
	// leaves StreamStatus empty, which is never terminal.
	dm.recordTerminalMemo(ch.ID, videoID, result.StreamStatus)
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/monitor/ -count=1 -v -run 'TestDecapi_|TestProcessYouTubeVideo'`

Expected: PASS for all `TestDecapi_*` and all `TestProcessYouTubeVideo*`.

- [ ] **Step 7: Run the package and vet**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/monitor/... && GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/monitor/`

Expected: `ok` and no vet output.

- [ ] **Step 8: Commit**

```bash
git add internal/monitor/decapi.go internal/monitor/utils.go internal/monitor/decapi_test.go
git commit -m "perf(monitor): stop DECAPI re-probing an unchanged terminal VOD" -m "DECAPI reports a channel's newest video, so a dormant channel re-probed the same finished VOD every cycle. Memoize {newest video ID, classification} per channel and skip the probe when the ID is unchanged, the classification was vod/not_a_stream, and a live HasProcessed read still says processed — so clearing an orphaned history row still re-opens the video next cycle. ProcessYouTubeVideo now reports StreamStatus on its two skip arms, which is what the memo reads." -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq"
```

---

### Task 3: Membership non-member memo with the liveness floor (T2-13)

Today every YouTube channel's authenticated `/membership` tab is fetched every feed cycle (`internal/monitor/feed.go:513-520`), member or not — 50 channels at 10 min is ~7,200 authenticated ~1 MB page loads a day mostly re-learning "not a member".

**How `ObserveLiveness` consumes that fetch today (the reason this task is the sensitive one):** the feed monitor calls `fm.FetchMembership(mctx, chID)` at `internal/monitor/feed.go:520`. In production that closure is the adapter installed at `cmd/moombox/monitor_callbacks.go:993-1019`; it calls `s.ytService.FetchMembershipVideos(ctx, channelID)` (:993), and at **`:1010`** hands the returned `SessionAuthState` to `routeLivenessVerdict(s.cookieRefresh.ObserveLiveness, verdict)` — before the error return, so even a failed fetch routes whatever verdict came back. `routeLivenessVerdict` (`:865-872`) forwards only `SessionAuthLoggedIn`/`SessionAuthLoggedOut`. The membership fetch is documented at `internal/youtube/channel_membership.go:36-40` as the arc's PREFERRED liveness signal precisely because it runs for every channel every cycle. **A memo that skipped the fetch would therefore also stop observing the YouTube session** — cookies could die with both dashboards clean. Hence the floor: every cycle fetches at least one channel.

`hasAccess` cannot be derived from the video list. `internal/youtube/channel_membership.go:121-124` returns `nil` videos for a non-member, and a MEMBER whose tab lists nothing also produces a nil slice (`parseMembershipTab` returns `(nil, true)` — `videos` is never appended to). The adapter's `out := make([]monitor.MembershipVideo, len(vids))` (`cmd/moombox/monitor_callbacks.go:1014`) then makes both a non-nil empty slice. So the bit is plumbed explicitly.

The clock: `FeedMonitor` already owns an injectable `now func() time.Time` (`internal/monitor/feed.go:178-184`, defaulted in `NewFeedMonitor`, pinned in tests by `withNow`). No new clock seam is needed.

**Files:**
- Modify: `internal/youtube/channel_membership.go:55-127` (`FetchMembershipVideos` gains `hasAccess`)
- Modify: `internal/youtube/channel_membership_test.go:336, :369, :393, :418, :452, :492` (mechanical), plus one new test
- Modify: `cmd/moombox/monitor_callbacks.go:993-1019` (adapter)
- Modify: `internal/monitor/feed.go:20-27` (consts), `:88-92` (`MembershipFetchFunc`), `:98-185` (struct fields), `:405-459` (`doCheck`), `:512-527` (the fetch), plus new helpers after `membershipActive`
- Test: `internal/monitor/feed_test.go:48-49` (`withMembership`), `:177-185` (`membWith`), plus new helpers and two new tests

**Interfaces:**
- Consumes: nothing from Tasks 1–2.
- Produces:
  - `func (s *Service) FetchMembershipVideos(ctx context.Context, channelID string) (videos []MembershipVideo, auth SessionAuthState, hasAccess bool, err error)`
  - `type MembershipFetchFunc func(ctx context.Context, channelID string) (videos []MembershipVideo, hasAccess bool, err error)`
  - `const membershipMemoTTL = 6 * time.Hour`
  - `var feedStagger = 500 * time.Millisecond` (was a const; a var so tests can shrink it)
  - `func (fm *FeedMonitor) armMembershipLiveness(channels []config.ChannelConfig, now time.Time)`
  - `func (fm *FeedMonitor) membershipFetchAllowed(chID string, now time.Time) bool`
  - `func (fm *FeedMonitor) recordMembershipAccess(chID string, hasAccess bool, now time.Time)`
  - Test helpers: `setChannels(fm *FeedMonitor, chans ...config.ChannelConfig)`, `withClock(t *time.Time) feedMonitorOpt`, `chYT(id string) config.ChannelConfig`, `shrinkFeedStagger(t *testing.T)`

- [ ] **Step 1: Write the failing youtube test**

Append to `internal/youtube/channel_membership_test.go`:

```go
// emptyMemberTabJSON is a MEMBER whose membership tab currently lists nothing:
// the sponsorships tab IS selected, but it holds no video renderers. This is
// the page that makes hasAccess load-bearing — its video list is identical to
// a non-member's.
const emptyMemberTabJSON = `{"contents": {"twoColumnBrowseResultsRenderer": {"tabs": [
	{"tabRenderer": {"selected": true, "tabIdentifier": "TAB_ID_SPONSORSHIPS", "content": {"richGridRenderer": {"contents": []}}}}
]}}}`

// TestFetchMembershipVideosReportsAccessSeparatelyFromTheVideoList pins the
// bit the feed monitor's non-member memo hangs on. A member whose tab happens
// to list nothing and a non-member both return zero videos, so the list cannot
// tell them apart — and memoizing a member as a non-member would delay
// members-only LIVE discovery by up to membershipMemoTTL, which is exactly the
// archive miss membership discovery exists to prevent.
//
// Mutant: returning `len(videos) > 0` as hasAccess passes the non-member case
// and fails the empty-member case.
func TestFetchMembershipVideosReportsAccessSeparatelyFromTheVideoList(t *testing.T) {
	cases := []struct {
		name          string
		initialData   string
		wantHasAccess bool
	}{
		{"member with an empty tab", emptyMemberTabJSON, true},
		{"non-member home fallback", homeFallbackJSON, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.Write(membershipHTML(true, tc.initialData))
			}))
			defer srv.Close()

			s := newMembershipProbeService(t, srv.URL, halfClearedCookieFile)
			videos, _, hasAccess, err := s.FetchMembershipVideos(context.Background(), "UCabc")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(videos) != 0 {
				t.Fatalf("videos = %d, want 0 — this fixture is the ambiguous case", len(videos))
			}
			if hasAccess != tc.wantHasAccess {
				t.Errorf("hasAccess = %v, want %v — the video list cannot answer this question", hasAccess, tc.wantHasAccess)
			}
		})
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/youtube/ -run TestFetchMembershipVideosReportsAccessSeparatelyFromTheVideoList -count=1 -v`

Expected: FAIL to COMPILE with `assignment mismatch: 4 variables but s.FetchMembershipVideos returns 3 values`.

- [ ] **Step 3: Return hasAccess from FetchMembershipVideos**

In `internal/youtube/channel_membership.go`, replace the function signature at `:81` and the three return sites in its body, and extend the doc block at `:72-75`:

Replace the doc lines at `:72-75`:

```go
// videos is nil when the account is not a member of the channel: the
// /membership URL then resolves to a public tab or a "join" upsell with no
// selected TAB_ID_SPONSORSHIPS tab. hasAccess reports that separately, and it
// is NOT derivable from the video list — a member whose tab currently lists
// nothing returns (nil, …, true, nil) and a non-member (nil, …, false, nil).
// The feed monitor's non-member memo (monitor.membershipMemoTTL) hangs on the
// distinction: memoizing a MEMBER would delay members-only live discovery.
// Callers may still treat "no videos" as "nothing to ingest" — but the verdict
// and the access bit have to be read separately.
```

Replace the signature at `:81`:

```go
func (s *Service) FetchMembershipVideos(ctx context.Context, channelID string) (videos []MembershipVideo, auth SessionAuthState, hasAccess bool, err error) {
```

Replace the `HasAnyAuthCookie` early return (`:85`):

```go
		return nil, SessionAuthUnknown, false, nil
```

Replace the `fetchLivenessPage` error return (`:110`):

```go
		return nil, SessionAuthUnknown, false, fmt.Errorf("fetch membership tab: %w", err)
```

Replace the final block (`:120-126`):

```go
	// Parse straight off the response bytes — no string(body)/[]byte(raw) copies
	// of the ~1MB payload. json.Unmarshal copies any strings it keeps, so the
	// body is free to be GC'd once this returns.
	vids, access := parseMembershipTab(body)
	if !access {
		return nil, verdict, false, nil
	}
	return vids, verdict, true, nil
}
```

- [ ] **Step 4: Update the six existing youtube call sites**

In `internal/youtube/channel_membership_test.go`, insert `_,` before `err` at lines `:336`, `:369`, `:393`, `:418`, `:452`, `:492`, e.g.:

```go
			videos, verdict, _, err := s.FetchMembershipVideos(context.Background(), "UC_probe_channel")
```

```go
	_, verdict, _, err := s.FetchMembershipVideos(context.Background(), "UCabc")
```

```go
	videos, verdict, _, err := s.FetchMembershipVideos(context.Background(), "UCabc")
```

(:393, :418 and :452 all take the third form; :492 takes the second.)

- [ ] **Step 5: Run the youtube package**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/youtube/...`

Expected: PASS, including the new test.

- [ ] **Step 6: Write the failing monitor tests**

In `internal/monitor/feed_test.go`, replace `withMembership` (`:46-50`) and `membWith` (`:173-185`) and add the new helpers:

```go
// withMembership injects a fake membership-tab fetcher in place of the real
// youtube.Service.FetchMembershipVideos wiring.
func withMembership(fn MembershipFetchFunc) feedMonitorOpt {
	return func(fm *FeedMonitor) { fm.FetchMembership = fn }
}

// withClock pins fm.now to a caller-controlled instant — withNow's mutable
// twin. The pointed-at value is what fm.now() reports, so a test can advance
// the cycle clock between doCheck calls (the membership memo's horizon is the
// only thing in the package that needs more than one instant).
func withClock(clock *time.Time) feedMonitorOpt {
	return func(fm *FeedMonitor) { fm.now = func() time.Time { return *clock } }
}

// setChannels writes chans into the monitor's config store so doCheck's
// getYouTubeChannels sees them. Tests that drive checkChannel directly never
// needed this; the membership memo is armed per CYCLE, so its tests drive
// doCheck.
func setChannels(fm *FeedMonitor, chans ...config.ChannelConfig) {
	_ = fm.configStore.Update(func(c *config.MoomboxConfig) { c.Channels = chans })
}

// chYT is a minimal enabled YouTube channel with the given ID as both ID and
// display name.
func chYT(id string) config.ChannelConfig {
	return config.ChannelConfig{ID: id, Name: id}
}

// shrinkFeedStagger cuts the inter-channel pacing sleep for a test that drives
// several full cycles (3 channels x 3 cycles would otherwise sleep 3 s).
func shrinkFeedStagger(t *testing.T) {
	t.Helper()
	orig := feedStagger
	feedStagger = time.Millisecond
	t.Cleanup(func() { feedStagger = orig })
}

// membWith adapts youtube.MembershipVideo fixtures — the real fetcher's
// return type — into a MembershipFetchFunc, mirroring the production adapter
// closure in cmd/moombox/monitor_callbacks.go (youtube.MembershipVideo ->
// monitor.MembershipVideo). It answers hasAccess=true: a fixture of a
// successful member fetch, empty list or not, which is what keeps every
// pre-existing test fetching on every cycle.
func membWith(videos ...youtube.MembershipVideo) MembershipFetchFunc {
	return func(ctx context.Context, channelID string) ([]MembershipVideo, bool, error) {
		out := make([]MembershipVideo, len(videos))
		for i, v := range videos {
			out[i] = MembershipVideo{VideoID: v.VideoID, Title: v.Title, Age: v.Age}
		}
		return out, true, nil
	}
}
```

Append the two new tests to `internal/monitor/feed_test.go`:

```go
// TestFeed_MembershipMemoSkipsNonMembersButNotMembers pins two of T2-13's
// three cases: a "not a member" answer suppresses the authenticated ~1 MB
// fetch for membershipMemoTTL, and a MEMBER is fetched every cycle.
//
// Mutants this fails on:
//   - no memo: UC1/UC2 are fetched on every cycle (the 7,200 loads/day bug).
//   - memoizing on an empty video list: UC3 (a member) stops being fetched,
//     and a members-only live stream goes undiscovered for up to 6 h.
//   - a horizon that never expires: the third cycle does not re-check UC1.
func TestFeed_MembershipMemoSkipsNonMembersButNotMembers(t *testing.T) {
	shrinkFeedStagger(t)
	db := newTestDB(t)
	clock := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	fetches := map[string]int{}
	fm := newTestFeedMonitor(t, db,
		withRSS(rssWith()),
		withProbe(stubProbeErrored()),
		withClock(&clock),
		withMembership(func(ctx context.Context, channelID string) ([]MembershipVideo, bool, error) {
			fetches[channelID]++
			if channelID == "UC3" {
				return []MembershipVideo{{VideoID: "memberVid01", Title: "members only"}}, true, nil
			}
			return nil, false, nil // signed in, simply not a member
		}),
	)
	setChannels(fm, chYT("UC1"), chYT("UC2"), chYT("UC3"))

	fm.doCheck(context.Background()) // cycle 1: nothing memoized yet
	assertFetches(t, "cycle 1", fetches, map[string]int{"UC1": 1, "UC2": 1, "UC3": 1})

	clock = clock.Add(10 * time.Minute)
	fm.doCheck(context.Background()) // cycle 2: only the member
	assertFetches(t, "cycle 2", fetches, map[string]int{"UC1": 1, "UC2": 1, "UC3": 2})

	clock = clock.Add(membershipMemoTTL)
	fm.doCheck(context.Background()) // cycle 3: the horizon expired
	assertFetches(t, "cycle 3", fetches, map[string]int{"UC1": 2, "UC2": 2, "UC3": 3})
}

// TestFeed_MembershipMemoAlwaysFetchesOneForLiveness pins T2-13's third case
// and the reason the memo is safe at all. The authenticated membership fetch
// is the system's preferred YouTube liveness probe: the production adapter
// routes its SessionAuthState to ObserveLiveness
// (cmd/moombox/monitor_callbacks.go:1010) and only when the fetch actually
// runs. A cycle where EVERY channel is memoized must still fetch exactly one,
// and must rotate — otherwise a dead session is never observed.
//
// Mutants this fails on:
//   - skipping every memoized channel: cycle 2 makes zero fetches and the
//     liveness signal goes dark.
//   - fetching every memoized channel "for liveness": cycle 2 makes three.
//   - a fixed nominee: cycle 3 re-fetches UC1 instead of rotating to UC2.
func TestFeed_MembershipMemoAlwaysFetchesOneForLiveness(t *testing.T) {
	shrinkFeedStagger(t)
	db := newTestDB(t)
	clock := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	var order []string
	fm := newTestFeedMonitor(t, db,
		withRSS(rssWith()),
		withProbe(stubProbeErrored()),
		withClock(&clock),
		withMembership(func(ctx context.Context, channelID string) ([]MembershipVideo, bool, error) {
			order = append(order, channelID)
			return nil, false, nil
		}),
	)
	setChannels(fm, chYT("UC1"), chYT("UC2"), chYT("UC3"))

	fm.doCheck(context.Background())
	if got := len(order); got != 3 {
		t.Fatalf("cycle 1 fetches = %d (%v), want 3", got, order)
	}

	clock = clock.Add(10 * time.Minute)
	order = nil
	fm.doCheck(context.Background())
	if len(order) != 1 || order[0] != "UC1" {
		t.Fatalf("cycle 2 fetches = %v, want exactly [UC1] — one nominated fetch keeps the liveness signal alive", order)
	}

	clock = clock.Add(10 * time.Minute)
	order = nil
	fm.doCheck(context.Background())
	if len(order) != 1 || order[0] != "UC2" {
		t.Fatalf("cycle 3 fetches = %v, want exactly [UC2] — the nomination must rotate to the earliest horizon", order)
	}
}

// assertFetches compares a per-channel fetch tally against want, naming the
// channel that diverged.
func assertFetches(t *testing.T, label string, got, want map[string]int) {
	t.Helper()
	for id, n := range want {
		if got[id] != n {
			t.Fatalf("%s: %s fetched %d times, want %d (all: %v)", label, id, got[id], n, got)
		}
	}
}
```

- [ ] **Step 7: Run the monitor tests to verify they fail**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/monitor/ -run 'TestFeed_MembershipMemo' -count=1 -v`

Expected: FAIL to COMPILE with `undefined: membershipMemoTTL`, `undefined: feedStagger` as an assignable var, and `cannot use func literal … as MembershipFetchFunc` (the fetcher still returns two values).

- [ ] **Step 8: Implement the memo in the feed monitor**

In `internal/monitor/feed.go`, replace the const block (`:20-27`) with:

```go
const (
	feedFetchTimeout         = 15 * time.Second
	defaultArchiveWindowDays = 3
	// membershipMemoTTL is how long a "not a member of this channel" answer
	// suppresses that channel's authenticated /membership fetch. RSS never
	// lists members-only content, so the fetch is the only discovery source
	// for it — but for a channel the operator is not a member of, it is a ~1 MB
	// authenticated page load per cycle that can only ever say the same thing.
	// Six hours is short enough that joining a channel's membership starts
	// working the same day without a restart.
	membershipMemoTTL = 6 * time.Hour
)

// feedStagger spaces consecutive channel feed fetches. Decapi and Twitch
// already stagger; a tight loop of YouTube RSS fetches on a big channel
// list looks like scraping behavior from a single source IP. Package var so
// tests driving several full cycles can shrink it.
var feedStagger = 500 * time.Millisecond
```

Replace `MembershipFetchFunc` (`:88-92`) with:

```go
// MembershipFetchFunc fetches the members-only videos listed on a channel's
// authenticated /membership tab. hasAccess reports whether the signed-in
// account actually HAS membership access: a member whose tab currently lists
// nothing returns (nil, true, nil), a non-member (nil, false, nil). The two are
// indistinguishable from the video list alone, and the non-member memo
// (membershipMemoTTL) must never latch onto a member. An error leaves both
// questions unanswered and writes no memo.
// Typically wired to youtube.Service.FetchMembershipVideos.
type MembershipFetchFunc func(ctx context.Context, channelID string) (videos []MembershipVideo, hasAccess bool, err error)
```

Add two fields to `FeedMonitor`, immediately after `MembershipEnabled func() bool` (`:162`):

```go
	// nonMemberUntil memoizes the channels whose /membership tab answered "not
	// a member": the horizon (cycle now + membershipMemoTTL) before which the
	// authenticated fetch is skipped. Keyed by channel ID and pruned to the
	// configured list every cycle by armMembershipLiveness, so it is bounded by
	// the channel count. Guarded by fm.mu.
	nonMemberUntil map[string]time.Time
	// membershipLivenessID is the ONE memoized channel this cycle fetches
	// anyway, set by armMembershipLiveness and consumed by the first
	// membershipFetchAllowed that matches it. Empty means "no nomination
	// needed" — some channel is being fetched on its own account. Guarded by
	// fm.mu.
	membershipLivenessID string
```

Replace the membership fetch block in `checkChannel` (`:510-527`) with:

```go
	// Members-only discovery: RSS never lists members-only content, so this is
	// the only source for members live/upcoming streams (and, with
	// include_non_live_content, their VODs).
	var membVideos []MembershipVideo
	if fm.membershipActive() && fm.membershipFetchAllowed(chID, cycleNow) {
		// defer cancel() inside the closure so a panic in FetchMembership can't
		// leak the timeout timer, while still releasing it the moment the fetch
		// returns (not held for the rest of checkChannel).
		vids, hasAccess, mErr := func() ([]MembershipVideo, bool, error) {
			mctx, cancel := context.WithTimeout(ctx, feedFetchTimeout)
			defer cancel()
			return fm.FetchMembership(mctx, chID)
		}()
		if mErr != nil {
			// A failed fetch answers neither question, so it writes no memo —
			// the same rule routeLivenessVerdict applies to a verdict we never
			// got (monitor_callbacks.go:865-872).
			fm.logger.Debug("membership discovery failed", "channel", ch.Name, "err", mErr)
		} else {
			membVideos = vids
			fm.recordMembershipAccess(chID, hasAccess, cycleNow)
		}
	}
```

Append the three helpers immediately after `membershipActive` (after `:650`):

```go
// armMembershipLiveness prepares this cycle's membership decisions: it prunes
// nonMemberUntil to the configured channels and, when EVERY channel is inside
// its non-member horizon, nominates the one with the earliest horizon to be
// fetched anyway.
//
// The nomination is what keeps the memo honest about credentials. The
// authenticated membership fetch is also the system's PREFERRED YouTube
// liveness probe: the production adapter routes the SessionAuthState it
// returns to (*cookies.RefreshService).ObserveLiveness
// (cmd/moombox/monitor_callbacks.go:1010), and that only happens when the
// fetch actually runs. A memo that skipped every channel would silently stop
// observing the session — the operator's cookies could die with both
// dashboards clean.
//
// Called once per cycle from doCheck. This is a SECOND fm.now() read per
// cycle, distinct from checkChannel's one-`now` rule (spec §7): it dates no
// stored row, only the 6 h memo horizons.
func (fm *FeedMonitor) armMembershipLiveness(channels []config.ChannelConfig, now time.Time) {
	fm.mu.Lock()
	defer fm.mu.Unlock()

	fm.membershipLivenessID = ""
	if len(fm.nonMemberUntil) == 0 {
		return
	}

	configured := make(map[string]struct{}, len(channels))
	for i := range channels {
		configured[channels[i].ID] = struct{}{}
	}
	for id := range fm.nonMemberUntil {
		if _, ok := configured[id]; !ok {
			delete(fm.nonMemberUntil, id)
		}
	}

	var earliestID string
	var earliest time.Time
	for i := range channels {
		id := channels[i].ID
		until, memoized := fm.nonMemberUntil[id]
		if !memoized || !now.Before(until) {
			return // some channel is fetched on its own account; no nomination
		}
		if earliestID == "" || until.Before(earliest) {
			earliestID, earliest = id, until
		}
	}
	fm.membershipLivenessID = earliestID
}

// membershipFetchAllowed reports whether this cycle fetches chID's
// authenticated /membership tab: yes when the channel is not memoized as a
// non-member (or its horizon has passed), and yes for the one channel
// armMembershipLiveness nominated. The nomination is consumed on the first
// match so a second memoized channel in the same cycle still skips.
func (fm *FeedMonitor) membershipFetchAllowed(chID string, now time.Time) bool {
	fm.mu.Lock()
	defer fm.mu.Unlock()

	until, memoized := fm.nonMemberUntil[chID]
	if !memoized || !now.Before(until) {
		return true
	}
	if chID != fm.membershipLivenessID {
		return false
	}
	fm.membershipLivenessID = ""
	return true
}

// recordMembershipAccess stores this cycle's membership verdict for chID. A
// member is never memoized — their tab is the only place a members-only live
// stream is ever listed, and it can go from empty to live between two cycles.
func (fm *FeedMonitor) recordMembershipAccess(chID string, hasAccess bool, now time.Time) {
	fm.mu.Lock()
	defer fm.mu.Unlock()

	if hasAccess {
		delete(fm.nonMemberUntil, chID)
		return
	}
	if fm.nonMemberUntil == nil {
		fm.nonMemberUntil = make(map[string]time.Time)
	}
	fm.nonMemberUntil[chID] = now.Add(membershipMemoTTL)
}
```

In `doCheck`, insert the arm call between the `len(channels) == 0` early return and the `fm.logger.Info("checking feeds", …)` line (`:414-418`):

```go
	channels := fm.getYouTubeChannels()
	if len(channels) == 0 {
		return
	}
	fm.armMembershipLiveness(channels, fm.now().UTC())

	fm.logger.Info("checking feeds", "channels", len(channels))
```

- [ ] **Step 9: Update the production adapter**

In `cmd/moombox/monitor_callbacks.go`, replace the `FetchMembership` closure (`:993-1019`) — keeping its existing comment block and adding the access note:

```go
	s.feedMon.FetchMembership = func(ctx context.Context, channelID string) ([]monitor.MembershipVideo, bool, error) {
		vids, verdict, hasAccess, err := s.ytService.FetchMembershipVideos(ctx, channelID)
		// The login verdict is a credential-health signal, not a discovery
		// result, so MembershipFetchFunc absorbs it here rather than carrying
		// it. hasAccess IS returned: the feed monitor's non-member memo
		// (monitor.membershipMemoTTL) must never latch onto a member, and an
		// empty video list cannot tell the two apart.
		//
		// Routed BEFORE the error return on purpose. Whether the tab scan
		// produced videos is a different question from whether YouTube
		// recognised the session, and this placement does not depend on the
		// two answers being packaged together. It costs nothing today —
		// FetchMembershipVideos returns SessionAuthUnknown on every failure
		// path — and it means a conclusive verdict reported alongside a failed
		// fetch would still reach the health signal rather than being dropped
		// by an early return nobody re-read.
		//
		// This closure runs once per configured channel per feed cycle, minus
		// the channels the non-member memo skips — and the monitor guarantees
		// at least one call per cycle precisely so this line keeps firing. A
		// dead session still arrives as several identical verdicts;
		// ObserveLiveness owns the de-duplication — see livenessRefireWindow
		// in internal/cookies.
		routeLivenessVerdict(s.cookieRefresh.ObserveLiveness, verdict)
		if err != nil {
			return nil, false, err
		}
		out := make([]monitor.MembershipVideo, len(vids))
		for i, v := range vids {
			out[i] = monitor.MembershipVideo{VideoID: v.VideoID, Title: v.Title, Age: v.Age}
		}
		return out, hasAccess, nil
	}
```

- [ ] **Step 10: Run the tests to verify they pass**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/monitor/... ./internal/youtube/... ./cmd/moombox/...`

Expected: `ok` for all three, including both new membership memo tests.

- [ ] **Step 11: Build and vet the whole module**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./... && GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./...`

Expected: no output (the signature change reaches `cmd/moombox`, so a whole-module build is the gate).

- [ ] **Step 12: Commit**

```bash
git add internal/youtube/channel_membership.go internal/youtube/channel_membership_test.go cmd/moombox/monitor_callbacks.go internal/monitor/feed.go internal/monitor/feed_test.go
git commit -m "perf(monitor): memoize non-member channels, keeping one membership fetch per cycle" -m "Every YouTube channel's authenticated /membership tab was fetched every feed cycle, member or not — ~7,200 authenticated ~1 MB page loads a day at 50 channels, mostly re-learning 'not a member'. Memoize a non-member answer for 6 h. A member is never memoized, and every cycle still fetches at least one channel (the memoized one with the earliest horizon, rotating) because that fetch is what feeds ObserveLiveness — without it a dead YouTube session would never be observed. FetchMembershipVideos now reports hasAccess, which an empty video list cannot." -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq"
```

---

### Task 4: Twitch inter-chunk stagger on a whole-batch failure (T3-28)

`doCheck`'s `wholeErr != nil` arm `continue`s (`internal/monitor/twitch.go:312-318`), which skips the inter-chunk stagger at `:339`. With more than 30 channels during a GQL 429 or 5xx, every remaining chunk fires back to back — pacing is dropped at exactly the moment it matters.

**Files:**
- Modify: `internal/monitor/twitch.go:16-19` (consts), `:38-52` (struct fields), `:282-346` (`doCheck` split)
- Create: `internal/monitor/twitch_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `var twitchStagger = 500 * time.Millisecond` (was a const)
  - `type StreamInfoBatchFunc func(ctx context.Context, logins []string) (infos []*twitch.TwitchStreamInfo, errs []error, wholeErr error)`
  - `TwitchMonitor.FetchBatch StreamInfoBatchFunc`
  - `func (tm *TwitchMonitor) streamInfoBatch(ctx context.Context, logins []string) ([]*twitch.TwitchStreamInfo, []error, error)`
  - `func (tm *TwitchMonitor) checkChunk(ctx context.Context, chunk []config.ChannelConfig)`

- [ ] **Step 1: Write the failing test**

Create `internal/monitor/twitch_test.go`:

```go
package monitor

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// newTestTwitchMonitor builds a TwitchMonitor over a real (temp-file) db and
// an in-memory config store, with the GQL batch call replaced by fetch. The
// concrete *twitch.Service is nil on purpose: streamInfoBatch must never reach
// it while FetchBatch is wired, and a nil dereference would say so loudly.
func newTestTwitchMonitor(t *testing.T, fetch StreamInfoBatchFunc, chans ...config.ChannelConfig) *TwitchMonitor {
	t.Helper()
	tm := NewTwitchMonitor(
		config.NewStore(config.Defaults(), ""),
		newTestDB(t),
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	tm.FetchBatch = fetch
	_ = tm.configStore.Update(func(c *config.MoomboxConfig) { c.Channels = chans })
	return tm
}

// twitchChans builds n enabled Twitch channels named tw0..tw(n-1).
func twitchChans(n int) []config.ChannelConfig {
	out := make([]config.ChannelConfig, n)
	for i := range out {
		out[i] = config.ChannelConfig{ID: fmt.Sprintf("tw%d", i), Name: fmt.Sprintf("tw%d", i), Platform: "twitch"}
	}
	return out
}

// shrinkTwitchStagger cuts the inter-chunk pacing sleep so the test measures a
// gap in tens of milliseconds instead of half a second.
func shrinkTwitchStagger(t *testing.T) {
	t.Helper()
	orig := twitchStagger
	twitchStagger = 50 * time.Millisecond
	t.Cleanup(func() { twitchStagger = orig })
}

// TestTwitch_StaggerRunsAfterAWholeBatchFailure pins T3-28: the inter-chunk
// stagger must run even when the batch request failed outright. A GQL 429 or
// 5xx is exactly when pacing matters, and the old `continue` skipped it — with
// >30 channels every remaining chunk fired back to back into a throttled API.
//
// Mutant: restoring the `continue` before the stagger makes the gap ~0.
func TestTwitch_StaggerRunsAfterAWholeBatchFailure(t *testing.T) {
	shrinkTwitchStagger(t)

	var calls []time.Time
	tm := newTestTwitchMonitor(t, func(ctx context.Context, logins []string) ([]*twitch.TwitchStreamInfo, []error, error) {
		calls = append(calls, time.Now())
		return nil, nil, fmt.Errorf("twitch gql: 429 Too Many Requests")
	}, twitchChans(twitchBatchChunk+1)...)

	tm.doCheck(context.Background())

	if len(calls) != 2 {
		t.Fatalf("batch calls = %d, want 2 (%d channels at a chunk of %d)", len(calls), twitchBatchChunk+1, twitchBatchChunk)
	}
	if gap := calls[1].Sub(calls[0]); gap < 40*time.Millisecond {
		t.Fatalf("inter-chunk gap = %v, want >= 40ms — a failed batch must still stagger before the next chunk", gap)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/monitor/ -run TestTwitch_StaggerRunsAfterAWholeBatchFailure -count=1 -v`

Expected: FAIL to COMPILE with `undefined: StreamInfoBatchFunc`, `tm.FetchBatch undefined`, and `cannot assign to twitchStagger (neither addressable nor a variable)`.

- [ ] **Step 3: Add the stagger var and the batch seam**

In `internal/monitor/twitch.go`, replace the const block (`:16-19`) with:

```go
const (
	twitchDefaultInterval = 15 * time.Second
)

// twitchStagger spaces consecutive GQL batch requests. Package var so tests
// can shrink it (the pattern discordRetryBackoff uses in internal/notifications).
var twitchStagger = 500 * time.Millisecond

// StreamInfoBatchFunc fetches live-stream info for one chunk of Twitch logins.
// Typically wired to twitch.Service.GetStreamInfoBatch; tests inject a fake via
// TwitchMonitor.FetchBatch — the seam FeedMonitor.FetchRSS is modelled on.
type StreamInfoBatchFunc func(ctx context.Context, logins []string) (infos []*twitch.TwitchStreamInfo, errs []error, wholeErr error)
```

Add the field to the `TwitchMonitor` struct, immediately after `IsOnline func() bool` (`:51`):

```go
	// FetchBatch overrides the GQL batch call (tm.tw.GetStreamInfoBatch) for
	// tests. Nil uses the real client — see streamInfoBatch.
	FetchBatch StreamInfoBatchFunc
```

- [ ] **Step 4: Split the chunk body out and stagger unconditionally**

In `internal/monitor/twitch.go`, replace the chunk loop body and the per-channel dispatch (`:296-346`, from `for start := 0;` through the closing brace of `doCheck`) with:

```go
	for start := 0; start < len(channels); start += twitchBatchChunk {
		select {
		case <-ctx.Done():
			return
		default:
		}
		end := min(start+twitchBatchChunk, len(channels))
		tm.checkChunk(ctx, channels[start:end])

		// Stagger between chunks (not between every channel any more). This
		// runs after EVERY chunk, including one whose whole-batch request
		// failed: a GQL 429 or 5xx is exactly when pacing matters, and the old
		// `continue` skipped it — with >30 channels every remaining chunk fired
		// back to back into a throttled API.
		if end < len(channels) {
			staggerTimer := time.NewTimer(twitchStagger)
			select {
			case <-ctx.Done():
				staggerTimer.Stop()
				return
			case <-staggerTimer.C:
			}
		}
	}
}

// streamInfoBatch is the injectable GQL batch seam: FetchBatch when a test has
// wired one, else the real client. Mirrors FeedMonitor.rssFetch.
func (tm *TwitchMonitor) streamInfoBatch(ctx context.Context, logins []string) ([]*twitch.TwitchStreamInfo, []error, error) {
	if tm.FetchBatch != nil {
		return tm.FetchBatch(ctx, logins)
	}
	return tm.tw.GetStreamInfoBatch(ctx, logins)
}

// checkChunk runs one batched GQL request and dispatches its per-channel
// results. A whole-request failure (transport/auth/malformed batch) is NOT any
// channel's fault — log once and leave every channel's health streak untouched
// (recording it would falsely mark all channels unhealthy on one shared
// outage). Retried next cycle; the caller still staggers before the next chunk.
func (tm *TwitchMonitor) checkChunk(ctx context.Context, chunk []config.ChannelConfig) {
	logins := make([]string, len(chunk))
	for i := range chunk {
		logins[i] = chunk[i].ID
	}

	infos, errs, wholeErr := tm.streamInfoBatch(ctx, logins)
	if wholeErr != nil {
		tm.logger.Debug("twitch batch check failed", "channels", len(chunk), "err", wholeErr)
		return
	}

	for i := range chunk {
		ch := &chunk[i]
		if errs[i] != nil {
			tm.health.recordError(ch.ID, errs[i])
			tm.logger.Debug("twitch check failed", "channel", ch.Name, "err", errs[i])
			continue
		}
		tm.health.recordSuccess(ch.ID)
		if infos[i] == nil {
			continue // offline
		}
		if err := tm.processStreamInfo(ctx, ch, infos[i]); err != nil {
			tm.logger.Debug("twitch process failed", "channel", ch.Name, "err", err)
		}
	}
}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/monitor/ -run TestTwitch_ -count=1 -v`

Expected: PASS.

- [ ] **Step 6: Run the package and vet**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/monitor/... && GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/monitor/`

Expected: `ok` and no vet output.

- [ ] **Step 7: Commit**

```bash
git add internal/monitor/twitch.go internal/monitor/twitch_test.go
git commit -m "fix(monitor): stagger after a failed Twitch batch too" -m "The whole-batch failure arm continued past the inter-chunk stagger, so with more than 30 channels a GQL 429 or 5xx fired every remaining chunk back to back — pacing dropped at exactly the moment it matters. Move the chunk body into checkChunk so the stagger runs after every chunk, and add the FetchBatch seam the test drives." -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq"
```

---

### Task 5: Connectivity boot probes once (T3-30)

`Start` runs the seed `checkFn()` and then, when it reports offline, calls `poll()` — which runs a SECOND full `checkFn()` synchronously (`internal/connectivity/monitor.go:89-92`). On a host that boots with no network that is roughly two probe timeouts of startup stall to learn something already known.

The two calls are behaviourally equivalent to one plus a direct transition. At `Start` the passive tracker is freshly constructed, so `poll`'s `m.passive.IsTriggeredPruned()` is false; and with `online` seeded true by the constructor and `checkFn` already false, `poll`'s only effect is `offlinePolls.Add(1)` followed by `transition(false)`. `transition(false)` does not read `offlinePolls`, and on recovery `transition(true)` stores 0, so the residual counter value (2 instead of 3) is unobservable.

**Files:**
- Modify: `internal/connectivity/monitor.go:86-92`
- Test: `internal/connectivity/monitor_test.go` (append; add `sync` to the import block)

**Interfaces:**
- Consumes: nothing.
- Produces: no new symbols; `Start`'s observable contract is unchanged except for the probe count.

- [ ] **Step 1: Write the failing test**

Append to `internal/connectivity/monitor_test.go`, and add `"sync"` to its import block:

```go
// TestMonitor_StartProbesOnceWhenOfflineAtBoot pins T3-30: booting offline
// must cost ONE probe, not two.
//
// Start seeded state with a synchronous checkFn and then called poll(), which
// runs checkFn again — so a machine that boots with no network paid two full
// probe timeouts (~6 s in production) before Start returned, to learn what the
// first probe already said.
//
// Mutant: restoring m.poll() makes the probe count 2 and roughly doubles the
// measured Start latency.
func TestMonitor_StartProbesOnceWhenOfflineAtBoot(t *testing.T) {
	const probeCost = 150 * time.Millisecond

	var probes atomic.Int32
	m := newTestMonitor(func() bool {
		probes.Add(1)
		time.Sleep(probeCost) // a probe timeout, in miniature
		return false
	})
	m.pollInterval = time.Hour // the ticker must not fire during this test

	var mu sync.Mutex
	var states []bool
	m.OnStateChange(func(online bool) {
		mu.Lock()
		states = append(states, online)
		mu.Unlock()
	})

	start := time.Now()
	m.Start(t.Context())
	elapsed := time.Since(start)
	t.Cleanup(m.Stop)

	if got := probes.Load(); got != 1 {
		t.Fatalf("boot probes = %d, want 1 — the seed check must not be followed by a second synchronous poll()", got)
	}
	// One probe is ~150ms; two are >=300ms. 250ms leaves ~85ms of scheduler
	// slack while still failing the two-probe mutant.
	if elapsed >= 250*time.Millisecond {
		t.Errorf("Start blocked %v, want ~one probe (%v) — a second synchronous probe doubles the boot stall", elapsed, probeCost)
	}
	if m.IsOnline() {
		t.Error("a monitor that booted offline must report offline")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(states) != 1 || states[0] {
		t.Errorf("state changes = %v, want exactly one false — the seed must still announce the outage", states)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/connectivity/ -run TestMonitor_StartProbesOnceWhenOfflineAtBoot -count=1 -v`

Expected: FAIL with `boot probes = 2, want 1`.

- [ ] **Step 3: Seed the transition without a second probe**

In `internal/connectivity/monitor.go`, replace `:86-92` with:

```go
	// Seed state with ONE synchronous probe so IsOnline() reflects reality
	// before the first tick fires. Without it, a machine that boots with no
	// network reports online=true for a whole poll interval.
	//
	// The transition is folded in rather than delegated to poll(): poll() would
	// spend a SECOND full probe timeout re-learning what checkFn just said,
	// which on an offline boot is the whole of the startup stall. Nothing else
	// is lost — poll()'s passive-tracker read cannot change this verdict (the
	// tracker was just constructed, and `online` is already false), and its
	// offlinePolls increment is unobservable once wasOnline is false.
	if !m.checkFn() {
		m.offlinePolls.Store(2) // skip the debounce — we already know we are offline
		m.transition(false)
	}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/connectivity/ -count=1 -v`

Expected: PASS for the new test and every pre-existing `TestMonitor_*`.

- [ ] **Step 5: Vet**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/connectivity/`

Expected: no output.

- [ ] **Step 6: Commit**

```bash
git add internal/connectivity/monitor.go internal/connectivity/monitor_test.go
git commit -m "fix(connectivity): probe once when booting offline" -m "Start ran the seed check and then poll(), which probes again — a host booting with no network paid two full probe timeouts (~6 s) to learn what the first probe already said. Fold the transition into the seed branch; poll()'s passive read and offlinePolls increment cannot change the verdict at boot." -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq"
```

---

### Task 6: Discord quotes the rejected response body (T4-35)

A rejected webhook yields `discord webhook returned 400` and nothing else (`internal/notifications/discord.go:132`, `:175-177`, `:178`) — the body that names the offending field is drained into `io.Discard`.

**Files:**
- Modify: `internal/notifications/discord.go:1-16` (imports), `:18-32` (consts), `:118-134` (`sendOnce`), `:170-179` (`Send`'s two non-429 error arms), `:194-225` (`post`), plus two new helpers
- Test: `internal/notifications/discord_test.go` (append)

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `const discordErrBodyBytes = 256`
  - `func discordErrSnippet(b []byte) string`
  - `func discordStatusErr(status int, snippet string) error`
  - `func (d *DiscordWebhook) post(body []byte) (status int, retryAfter, snippet string, err error)` (was 3 results)

- [ ] **Step 1: Write the failing tests**

Append to `internal/notifications/discord_test.go`:

```go
// TestDiscordWebhook4xxErrorQuotesTheBody pins T4-35: a rejected webhook must
// say WHY. Discord's 4xx bodies name the offending field
// ({"embeds": ["Must be 10 or fewer in length."]}); without them an operator
// sees only "discord webhook returned 400", which is unactionable.
//
// Mutants this fails on:
//   - draining the body into io.Discard without capturing it: no quote.
//   - quoting it raw: the embedded newline survives, and a remote body that
//     can forge a log line is a log-injection vector.
func TestDiscordWebhook4xxErrorQuotesTheBody(t *testing.T) {
	shrinkBackoff(t)
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusBadRequest)
		io.WriteString(rw, "{\"embeds\":\n[\"Must be 10 or fewer in length.\"]}")
	}))
	t.Cleanup(srv.Close)

	err := (&DiscordWebhook{URL: srv.URL}).Send("t", "d", 0, nil, SendOptions{})
	if err == nil {
		t.Fatal("400: want error, got nil")
	}
	if !strings.Contains(err.Error(), "Must be 10 or fewer in length.") {
		t.Errorf("error = %q, want the response body quoted", err)
	}
	if strings.ContainsAny(err.Error(), "\r\n") {
		t.Errorf("error = %q, must be a single line — a remote body must not forge a log line", err)
	}
}

// TestDiscordWebhookErrorBodyIsBounded pins the cap: the quote is a hint, not
// a transcript, and an error page can be arbitrarily large.
//
// Mutant: io.ReadAll without the LimitReader puts the whole page in the error
// (and in every log line and HTTP response it reaches).
func TestDiscordWebhookErrorBodyIsBounded(t *testing.T) {
	shrinkBackoff(t)
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusForbidden)
		io.WriteString(rw, strings.Repeat("a", 8192))
	}))
	t.Cleanup(srv.Close)

	err := (&DiscordWebhook{URL: srv.URL}).Send("t", "d", 0, nil, SendOptions{})
	if err == nil {
		t.Fatal("403: want error, got nil")
	}
	if n := len(err.Error()); n > discordErrBodyBytes+64 {
		t.Errorf("error is %d bytes, want <= %d — the body quote must be bounded", n, discordErrBodyBytes+64)
	}
}

// TestDiscordErrSnippet pins the sanitiser directly: control characters
// collapse to single spaces, runs collapse, and the result is trimmed.
func TestDiscordErrSnippet(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"  plain  ", "plain"},
		{"line one\nline two", "line one line two"},
		{"a\r\n\tb", "a b"},
		{"{\"message\": \"Unknown Webhook\", \"code\": 10015}", `{"message": "Unknown Webhook", "code": 10015}`},
	}
	for _, tc := range cases {
		if got := discordErrSnippet([]byte(tc.in)); got != tc.want {
			t.Errorf("discordErrSnippet(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/notifications/ -run 'TestDiscordWebhook4xxErrorQuotesTheBody|TestDiscordWebhookErrorBodyIsBounded|TestDiscordErrSnippet' -count=1 -v`

Expected: FAIL to COMPILE with `undefined: discordErrBodyBytes` and `undefined: discordErrSnippet`.

- [ ] **Step 3: Add the constant and helpers**

In `internal/notifications/discord.go`, add `"strings"` and `"unicode/utf8"` to the import block (`:1-16`), keeping it gofmt-sorted.

Append to the const block (`:18-32`), after `discordMaxSleepTotal`:

```go
	// discordErrBodyBytes bounds how much of a rejected webhook's response
	// body is quoted back in the error. Discord's 4xx bodies are small JSON
	// objects naming what it refused ({"message": "Unknown Webhook", "code":
	// 10015}); without them an operator sees only "discord webhook returned
	// 400" and has nothing to act on. An error PAGE, though, can be
	// arbitrarily large, and this error reaches log files and SendTest's HTTP
	// response.
	discordErrBodyBytes = 256
```

Add both helpers immediately above `post` (before `:194`):

```go
// discordErrSnippet renders a response-body prefix as a single-line, printable
// error fragment. The body is REMOTE input that lands in log lines and in
// SendTest's HTTP response, so newlines (which would forge a log line), other
// control characters, and the replacement rune a truncated multi-byte tail
// decodes to all collapse to spaces; runs of whitespace collapse to one.
func discordErrSnippet(b []byte) string {
	var sb strings.Builder
	sb.Grow(len(b))
	for _, r := range string(b) {
		if r < 0x20 || r == 0x7f || r == utf8.RuneError {
			sb.WriteByte(' ')
			continue
		}
		sb.WriteRune(r)
	}
	return strings.Join(strings.Fields(sb.String()), " ")
}

// discordStatusErr renders a non-2xx webhook response, quoting the body prefix
// when Discord sent one. Used by every site that reports a status — 4xx and
// 5xx alike: the 5xx text is what an operator sees after the retry budget is
// spent, which is precisely when the reason matters.
func discordStatusErr(status int, snippet string) error {
	if snippet == "" {
		return fmt.Errorf("discord webhook returned %d", status)
	}
	return fmt.Errorf("discord webhook returned %d: %s", status, snippet)
}
```

- [ ] **Step 4: Capture the snippet in post and use it at the three sites**

In `internal/notifications/discord.go`, replace `post`'s signature (`:196`) and its deferred drain + return (`:220-225`):

```go
func (d *DiscordWebhook) post(body []byte) (status int, retryAfter, snippet string, err error) {
```

```go
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL, bytes.NewReader(body))
	if err != nil {
		return 0, "", "", fmt.Errorf("create discord request: %w", err)
	}
```

```go
		var uerr *url.Error
		if errors.As(err, &uerr) {
			return 0, "", "", fmt.Errorf("%s %s: %w", uerr.Op, redactURLForLog(uerr.URL), uerr.Err)
		}
		return 0, "", "", err
	}
	defer func() {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
	if resp.StatusCode >= 400 {
		// Read the reason BEFORE the deferred drain throws the rest away.
		b, _ := io.ReadAll(io.LimitReader(resp.Body, discordErrBodyBytes))
		snippet = discordErrSnippet(b)
	}
	return resp.StatusCode, resp.Header.Get("Retry-After"), snippet, nil
}
```

In `sendOnce` (`:123-134`):

```go
	status, retryAfter, snippet, err := d.post(body)
	switch {
	case err != nil:
		return fmt.Errorf("discord webhook request: %w", err)
	case status < 400:
		return nil
	case status == http.StatusTooManyRequests:
		return fmt.Errorf("discord rate limited (retry-after: %s)", retryAfter)
	default:
		return discordStatusErr(status, snippet)
	}
}
```

In `Send`'s loop (`:155` and the two non-429 status arms at `:175-179`):

```go
		status, retryAfter, snippet, err := d.post(body)
```

```go
		case status >= 500:
			lastErr = discordStatusErr(status, snippet)
			delay = discordRetryBackoff[min(attempt-1, len(discordRetryBackoff)-1)]
		default:
			return discordStatusErr(status, snippet)
		}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/notifications/...`

Expected: PASS, including the pre-existing `TestDiscordWebhook4xxIsPermanent` and the 429 tests (whose messages are unchanged).

- [ ] **Step 6: Vet**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/notifications/`

Expected: no output.

- [ ] **Step 7: Commit**

```bash
git add internal/notifications/discord.go internal/notifications/discord_test.go
git commit -m "fix(notifications): quote the rejected Discord response body" -m "A refused webhook reported only 'discord webhook returned 400' while the body naming the offending field went to io.Discard. Capture up to 256 bytes on any >=400 response and quote it, sanitised to a single printable line — the body is remote input that reaches log files and SendTest's HTTP response. The 429 messages are unchanged." -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq"
```

---

### Task 7: Bound the feed's non-200 drain (T4-35)

`fetchFeed` drains a non-200 body with an unbounded `io.Copy` (`internal/monitor/feed.go:635`); DECAPI already bounds the same drain at 4 KB (`internal/monitor/decapi.go:471`, `:478`). A captive-portal interstitial or a large 5xx page is read in full purely to be thrown away.

**Files:**
- Modify: `internal/monitor/feed.go:20-27` (const), `:635` (drain), plus one new helper after `fetchFeed`
- Modify: `internal/monitor/decapi.go` (the two `io.LimitReader(resp.Body, 4096)` drains inside `fetchLatestVideo`, added in Task 1)
- Test: `internal/monitor/feed_test.go` (append)

**Interfaces:**
- Consumes: Task 1's `fetchLatestVideo`; Task 3's `feedStagger` var and the const block it rewrote.
- Produces:
  - `const monitorDrainLimit = 4096`
  - `func drainBounded(r io.Reader)`

- [ ] **Step 1: Write the failing test**

Append to `internal/monitor/feed_test.go`:

```go
// countingReader serves n bytes and reports how many were actually read.
type countingReader struct{ remaining, read int }

func (c *countingReader) Read(p []byte) (int, error) {
	if c.remaining <= 0 {
		return 0, io.EOF
	}
	n := min(len(p), c.remaining)
	c.remaining -= n
	c.read += n
	return n, nil
}

// TestDrainBoundedStopsAtTheLimit pins T4-35's feed half: a non-200 body is
// drained only far enough to keep the connection reusable, never in full.
//
// Mutant: the bare io.Copy(io.Discard, resp.Body) the feed fetcher used reads
// the whole 1 MB error page to throw it away.
func TestDrainBoundedStopsAtTheLimit(t *testing.T) {
	big := &countingReader{remaining: 1 << 20}
	drainBounded(big)
	if big.read != monitorDrainLimit {
		t.Fatalf("drained %d bytes of a 1MB body, want exactly %d", big.read, monitorDrainLimit)
	}

	// A body shorter than the limit still drains completely and returns.
	small := &countingReader{remaining: 17}
	drainBounded(small)
	if small.read != 17 {
		t.Fatalf("drained %d bytes of a 17-byte body, want 17", small.read)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/monitor/ -run TestDrainBoundedStopsAtTheLimit -count=1 -v`

Expected: FAIL to COMPILE with `undefined: drainBounded` and `undefined: monitorDrainLimit`.

- [ ] **Step 3: Add the constant and helper**

In `internal/monitor/feed.go`, append to the const block Task 3 rewrote (after `membershipMemoTTL`):

```go
	// monitorDrainLimit bounds how much of a non-200 response body is read
	// before Close. Draining returns the connection to the idle pool instead
	// of discarding it (costly during a sustained outage), but an unbounded
	// drain reads an arbitrarily large error page — a 5xx HTML page, a
	// captive-portal interstitial — purely to throw it away.
	monitorDrainLimit = 4096
```

Add the helper immediately after `fetchFeed` (after `:640`):

```go
// drainBounded reads and discards at most monitorDrainLimit bytes of r so the
// underlying connection returns to the idle pool. Shared by the feed and
// DECAPI fetchers, which are the two monitor paths that close on a non-200.
func drainBounded(r io.Reader) {
	io.Copy(io.Discard, io.LimitReader(r, monitorDrainLimit))
}
```

- [ ] **Step 4: Use it at the three call sites**

In `internal/monitor/feed.go`, replace `:635`:

```go
		drainBounded(resp.Body) // bounded drain for connection reuse
```

In `internal/monitor/fetchLatestVideo` (`internal/monitor/decapi.go`, added in Task 1), replace both drains:

```go
		// Drain so the connection can be reused (closing an unread body
		// discards the TCP connection — costly during a sustained 429 storm).
		drainBounded(resp.Body)
		return "", fmt.Errorf("rate limited (429)")
```

```go
		// Non-2xx — server reachable but unhappy; leave tracker alone.
		// Drain a bounded amount before close to keep the connection reusable.
		drainBounded(resp.Body)
		return "", fmt.Errorf("decapi http %d", resp.StatusCode)
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/monitor/...`

Expected: `ok`.

- [ ] **Step 6: Vet and check the call sites by eye**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/monitor/ && grep -rn "io.Copy(io.Discard" internal/monitor/`

Expected: no vet output, and the only remaining `io.Copy(io.Discard` in the package is the one inside `drainBounded` itself.

- [ ] **Step 7: Commit**

```bash
git add internal/monitor/feed.go internal/monitor/decapi.go internal/monitor/feed_test.go
git commit -m "fix(monitor): bound the feed's non-200 drain at 4 KB" -m "fetchFeed drained a non-200 body with an unbounded io.Copy, reading a whole error page or captive-portal interstitial to throw it away; DECAPI already bounded the same drain at 4 KB. One drainBounded helper now serves all three sites." -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq"
```

---

### Task 8: Spec prose and plan deletion

`docs/spec/architecture.md` describes each monitor's cadence and the feed's per-cycle membership fetch. Three of this arc's changes contradict that prose. Nothing in `docs/spec/*` or `SPEC.md` describes `connectivity.Monitor.Start`'s boot seed, the Discord error text, or the feed drain — those three need no doc edit (verified by grepping `connectivity`, `Discord`, and `drain` across `docs/spec/`).

Spec §2: "Plans are deleted in the arc's last commit once implemented."

**Files:**
- Modify: `docs/spec/architecture.md:128` (§13 membership fetch), `:135` (§14 DECAPI), `:138` (§15 Twitch)
- Delete: `docs/superpowers/plans/2026-09-15-sweep-4-monitor.md`

**Interfaces:**
- Consumes: `membershipMemoTTL`, `decapiProbeBudget`, `decapiTerminalStatus`, `checkChunk` — all declared by Tasks 1–4; each citation below names the file that DECLARES the symbol, which is what `internal/docs`' citation test requires.
- Produces: nothing.

- [ ] **Step 1: Update §13's membership sentence**

In `docs/spec/architecture.md`, replace the second half of line 128 — from "When `membership_discovery` is enabled" through "(independent signals)." — with:

```
When `membership_discovery` is enabled (default) and YouTube auth cookies are present, `checkChannel` additionally fetches the channel's authenticated `/membership` tab (`youtube.FetchMembershipVideos`) — the only discovery source for members-only content, which RSS/DECAPI never list. A channel whose tab answers "not a member" is memoized for `membershipMemoTTL` (`internal/monitor/feed.go`, 6 h) and skipped; a member is never memoized, because their tab is the only place a members-only live stream is ever listed. Every cycle still fetches at least ONE channel — when all of them are memoized, the one with the earliest horizon, rotating — because that same fetch carries the YouTube login verdict the cookie health signal reads (`routeLivenessVerdict` in `cmd/moombox/monitor_callbacks.go`), and a cycle that fetched nothing would observe nothing. Members-only candidates are probed with the authenticated `ProbeVideoAuth` closure so a members VOD classifies correctly instead of misfiring as "upcoming". A membership fetch failure is logged, writes no memo, and never marks the RSS feed unhealthy (independent signals).
```

- [ ] **Step 2: Update §14's DECAPI paragraph**

In `docs/spec/architecture.md`, replace line 135 with:

```
`monitor.NewDecapiMonitor()` uses the DECAPI API to find the latest video for YouTube channels. Rate-limited to 60 requests/minute (reads rate limit headers from responses). Stagger of 1 second between per-channel requests. The request and the classification hold separate deadlines: `fetchLatestVideo` (`internal/monitor/decapi.go`) owns the 15-second request timeout and releases it as soon as the body is read, and the probe then runs under `decapiProbeBudget` (`internal/monitor/decapi.go`, 60 s) derived from the cycle context — so a slow DECAPI answer never shortens the probe. Because DECAPI reports a channel's NEWEST video, a dormant channel would otherwise re-probe the same finished VOD every cycle; a per-channel memo skips the probe when the video ID is unchanged, its last classification was terminal (`decapiTerminalStatus` in `internal/monitor/decapi.go` — `vod` or `not_a_stream`), and the processing history still holds it. Clearing the history row re-opens the video on the next cycle.
```

- [ ] **Step 3: Update §15's Twitch paragraph**

In `docs/spec/architecture.md`, replace line 138 with:

```
`monitor.NewTwitchMonitor()` polls Twitch GQL for live streams. Default 15-second interval. Channels are batched into GQL requests of up to 30 logins, with a 500 ms stagger between chunks — including after a chunk whose whole request failed (`checkChunk` in `internal/monitor/twitch.go`), since a 429 or 5xx is exactly when pacing matters. Uses the Twitch service's GQL client for stream info queries.
```

- [ ] **Step 4: Run the citation test**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/docs/`

Expected: `ok` — every symbol above is declared in the file cited beside it.

- [ ] **Step 5: Run the full arc gate set**

Run:

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp gofmt -l ./cmd ./internal ./tools ./web
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./...
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./...
GOOS=linux GOARCH=amd64 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./...
GOOS=linux GOARCH=arm64 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./...
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/monitor/... ./internal/notifications/... ./internal/connectivity/... ./internal/youtube/... ./cmd/moombox/... ./internal/docs/
```

Expected: `gofmt -l` prints nothing, vet prints nothing, all three builds succeed, all test packages `ok`. (`staticcheck ./...` and the single whole-repo `go test -count=1 ./...` are controller-run per spec §2.)

- [ ] **Step 6: Delete the plan and commit**

```bash
git rm docs/superpowers/plans/2026-09-15-sweep-4-monitor.md
git add docs/spec/architecture.md
git commit -m "docs(spec): record the Arc 4 monitor cadence changes" -m "Feed §13 gains the non-member memo and the one-fetch-per-cycle liveness floor; DECAPI §14 gains the split request/probe deadlines and the terminal memo; Twitch §15 states that the inter-chunk stagger runs after a failed batch too. The implemented plan is deleted — git history is the archive." -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq"
```

---

## Self-review

### 1. Spec coverage

| Spec §7 design item | Ledger item | Task | Notes |
|---|---|---|---|
| 1. DECAPI probe budget (`decapiProbeBudget = 60 s`, cancel request ctx after the body read, probe-seam deadline test) | T1-10 | Task 1 | Test asserts `30 s ≤ budget ≤ 60 s` plus cycle-context derivation. |
| 2. DECAPI terminal memo (`{lastVideoID, lastStatus}`, reset on new ID, cooldown untouched; 3 tests) | T2-12 | Task 2 | Three named tests plus a fourth for the history conjunct. "Processed" = `db.HasProcessed` at `decapi.go:574`, cited. `probe_cooldown` untouched (Global Constraints + arc ruling). |
| 3. Membership memo, 6 h horizon, member never memoized, ≥1 fetch per cycle for liveness; 3 tests | T2-13 | Task 3 | Three cases across two tests (member/non-member/expiry; all-memoized fetches exactly one, rotating). `ObserveLiveness` consumption documented with `monitor_callbacks.go:1010`. Clock: existing `fm.now` seam. |
| 4. Twitch stagger before `continue` on whole-batch error | T3-28 | Task 4 | `checkChunk` extraction; timing test with a shrunk stagger var. |
| 5. Connectivity `Start` seeds from one check, no second `poll()` | T3-30 | Task 5 | Probe-count + latency + state-change assertions. |
| 6. Discord 4xx body, ≤256 bytes, in the error | T4-35 | Task 6 | Applied to every `>=400` (superset stated and justified in `discordStatusErr`'s doc). |
| 7. Feed non-200 drain bounded at 4 KB, mirroring `decapi.go` | T4-35 | Task 7 | One `drainBounded` helper now used by both fetchers. |
| 8. `docs/spec/architecture.md` monitor cadence prose | — | Task 8 | §13/§14/§15; plan deleted in the same commit per §2. |
| Gates `./internal/monitor/...`, `./internal/notifications/...`, `./internal/connectivity/...`, `./internal/docs/` | — | all | Listed in Global Constraints, plus `./internal/youtube/...` and `./cmd/moombox/...` for Task 3. |

No §7 item is unassigned. One item exceeded §7's package list (Task 3 → `internal/youtube` + `cmd/moombox`); the reason, the disjointness argument against the concurrent Arc 3, and the cost-if-wrong are recorded as an arc ruling.

### 2. Placeholder scan

Searched the plan for `TBD`, `TODO`, `implement later`, `fill in`, `add appropriate`, `handle edge cases`, `write tests for the above`, `similar to Task`: no occurrences. Every code step carries the literal code to write; every test step carries the literal test; every run step carries the exact command and the expected output. Step 4 of Task 3 spells out the six mechanical call-site edits rather than saying "update the call sites".

### 3. Type consistency

- `decapiProbeBudget`, `decapiLatestVideoURL`, `fetchLatestVideo` — declared in Task 1, re-used verbatim by Task 7 (both drain sites) and Task 8 (the §14 citation).
- `decapiTerminalStatus`, `terminalMemoHit(channelID, videoID string) bool`, `recordTerminalMemo(channelID, videoID, status string)` — Task 2 only; spelled identically in the struct field, the two accessors, both call sites, and Task 8's citation.
- `ProcessYouTubeVideoResult.StreamStatus` — Task 2 populates it on the two skip arms; the only consumer is `decapi.go` (verified: `ProcessYouTubeVideo` has exactly one non-test caller).
- `MembershipFetchFunc` is `(videos []MembershipVideo, hasAccess bool, err error)` in Task 3's type declaration, in `membWith`, in both new test fixtures, and in the `cmd/moombox` adapter's return. `FetchMembershipVideos` is `(videos, auth, hasAccess, err)` in the signature, the three returns, the six updated call sites, and the new youtube test.
- `armMembershipLiveness` / `membershipFetchAllowed` / `recordMembershipAccess` all take `now time.Time`, and every caller passes a `.UTC()` value (`doCheck` via `fm.now().UTC()`, `checkChannel` via the existing `cycleNow`).
- `feedStagger` and `twitchStagger` become package vars in Tasks 3 and 4 respectively; `shrinkFeedStagger` / `shrinkTwitchStagger` restore them via `t.Cleanup`, matching `shrinkBackoff`'s existing shape.
- `StreamInfoBatchFunc`'s result tuple matches `twitch.Service.GetStreamInfoBatch` exactly (`internal/twitch/service.go:48`).
- `post` is 4-valued in its declaration and at both call sites (`sendOnce`, `Send`); `discordStatusErr(status int, snippet string) error` is used at all three status sites.
- `monitorDrainLimit` / `drainBounded` — declared in Task 7 in `feed.go`, called from `feed.go` and `decapi.go` (same package).
- Test helpers used before their defining task: none. `newTestDB` and `newTestFeedMonitor` predate this plan; `setChannels`, `withClock`, `chYT`, `assertFetches`, `shrinkFeedStagger` are all defined in Task 3, which is the first to use them; `newTestTwitchMonitor`, `twitchChans`, `shrinkTwitchStagger` in Task 4; `countingReader` in Task 7.
