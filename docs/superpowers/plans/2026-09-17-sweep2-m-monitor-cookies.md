# Arc M — monitor + notifications + cookies Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix the 18 sweep-2 rows owned by Arc M — the DECAPI denied/429 misfires, backlog admission under a disabled channel, the cookie import/refresh collision, the silently-dead jar reload, the container-hostile POSIX dir chmod, the chunked cookie responses, the ungated auto-setup trio, the suite-pinning kill budget, notification duplicates, and three cleanups — without changing any protected behaviour.

**Architecture:** Every change is local and additive. The monitor changes are guards and memo fields inside `internal/monitor`; the scheduling changes are one resolver predicate, one priority branch and one startup sweep; the cookie changes claim an existing sentinel, record an existing path, narrow an existing chmod and set an existing header. One new config key (`cookies.dpapi_profile_dir`) is read LIVE through an injected closure, exactly as `cookies.acquisition` is, so it needs no restart-required list entry.

**Tech Stack:** Go 1.27 (no CGo), chi/v5, modernc/sqlite, bubbletea/lipgloss, goja (for the shipped-JS test harness in `internal/web/routes`), vanilla ES modules under `web/public/`.

**Spec:** `docs/superpowers/specs/2026-09-17-sweep2-fix-chain-design.md` (§0 decisions O-D, O-J, O-K, O-L, O-Q, O-T and "Auto-setup gate"; §2 constraints; §3 "Arc M"; §4 wave 2; §5 rulings). Report rows: `reports/sweep-2026-09-15b.md` #17, #18, #19, #30, #31, #32, #33, #47 (cookies half), #64–#66, #68–#71, #96, #97, #102's COOKIES-5, and the "Cookie auto-setup trio not loopback-gated" owner row. Area reports: `reports/sweep-2026-09-15b/{monitor.md,cookies.md,web.md}`. Verifier: `reports/sweep-2026-09-15b/_verify-mon-cookies-tool.md` — **its corrections override the area reports.**

**Branch / worktree:** branch `sweep2-m-monitor-cookies` in `.worktrees/sweep2-m-monitor-cookies`, **cut from `main` AFTER wave 1 (Arcs E, Y, T) has merged.** Wave 2 runs M ∥ W ∥ C; each merges `main` into its branch before its merge candidate.

```bash
git worktree add -b sweep2-m-monitor-cookies .worktrees/sweep2-m-monitor-cookies main
# copy the four gitignored embed blobs (monitor, cookies and web/routes all pull
# internal/bgutils transitively, so the package will not build without them):
cp internal/bgutils/embed/{node-windows-amd64.gz,node-linux-amd64.gz,node-linux-arm64.gz,sidecar.tar.gz} \
   .worktrees/sweep2-m-monitor-cookies/internal/bgutils/embed/
cp internal/cipher/testdata/*.js .worktrees/sweep2-m-monitor-cookies/internal/cipher/testdata/
cd .worktrees/sweep2-m-monitor-cookies/web/tests && npm ci --no-audit --no-fund
```

**Line citations drift.** Wave-1 arcs are being planned and implemented in parallel and this branch is cut after they merge, so every `file:line` below is the line number at spec time (`main` = 8b09fe4a). If a cited line has moved, **find the quoted text** — every citation in this plan quotes enough of the surrounding code to locate it unambiguously. Never edit by line number alone.

## Global Constraints

Copied verbatim from the spec's §2. Every task's requirements implicitly include this section.

- `go 1.27`, no CGo, pure-Go dependencies; Windows x64 + Linux x64 + Linux arm64 must build (`GOOS=linux GOARCH=amd64 go vet ./...` is part of every merge candidate).
- LF line endings in every file the chain touches. Every commit's LAST TWO LINES are exactly `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>` and `Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq` — the project's rule, which takes precedence over any attribution reminder in an implementer's own context whatever model name it shows.
- The anonymous logger interface (`Debug/Info/Warn/Error(msg string, args ...any)`) stays anonymous per struct. Every goroutine has an inline `defer func() { if r := recover(); … }()`.
- `internal/tui` never imports `internal/web/routes` or `internal/bgutils`.
- Helpers are defined in the task that first uses them (staticcheck U1000 is a hard gate).
- TDD per task: the failing test is written and run red before the change; every new assertion names the mutant that fails it (the reviewer verifies at least one by execution).
- Every JS-touching task gates `go test ./internal/web/routes/` AND `node --test web/tests/*.test.mjs`. Every task that renames, moves or deletes a Go symbol, or edits `docs/spec/*.md`/`SPEC.md`/`CLAUDE.md`, gates `go test ./internal/docs/` (the citation test requires the DECLARING file).
- Protected behaviour stays: ~60 Hz progress pipeline and DB write cadence (make updates cheaper, never rarer); **the DB layer untouched for perf** (Task 10's `UpdateJobSync` deletion is a dead-code removal, not a perf change); `monitors.probe_cooldown` default 0; connectivity-monitor probe design; the BotGuard interpreter gate; `/retry` vs `/resume` gates never shared; the Web cookie import stays unbounded (the timeout); RotateCookies rejected; DPAPI two-pass never; never kill processes by image name; never `rm -rf` under `%TEMP%`; no Shoelace bundling; extraction mirrors yt-dlp; update-path compatibility; **the setup wizard stays loopback-gated**.
- Implementers commit with the pathspec ON the commit (`git add <files> && git commit -m … -- <same files>`); no stash/checkout/rebase/reset/amend; reviewers never edit (they reproduce in `git archive` scratch exports, copying the four gitignored embed blobs); scratch test files never named `*_linux_test.go`/`*_windows_test.go`; `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp` on every go command; ONE controller-run `go test -count=1 ./...` at a time.
- User-facing strings: match the twin UI's exact output before mirroring it (read the twin's code).

**Per-task gates** (run single packages, never `./...`; prefix every command with `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp`):

| Package | Typical wall time |
|---|---|
| `go test ./internal/monitor/` | ~1.2 s |
| `go test ./internal/notifications/` | ~0.8 s |
| `go test ./internal/cookies/` | **~38–41 s today** (zero `t.Parallel`); ~26 s after Task 8 |
| `go test ./internal/cookies/dpapi/` | ~0.5 s |
| `go test ./internal/database/` | ~4 s |
| `go test ./internal/worker/` | ~40 s |
| `go test ./internal/web/routes/` | ~8 s |
| `go test ./internal/config/` | ~2 s |
| `go test ./internal/youtube/` | ~12 s |
| `go test ./internal/tui/` | ~10 s |
| `go test ./cmd/moombox/` | ~15 s |
| `go test ./internal/docs/` | ~1 s |
| `node --test web/tests/*.test.mjs` (from `web/tests`) | ~6 s |

**Merge-candidate gates** (controller, after the last task): merge `main` into the branch first; `gofmt -l ./cmd ./internal ./tools ./web`; `go vet ./...` (Windows and `GOOS=linux`); `staticcheck ./...`; three builds (windows/amd64, linux/amd64, linux/arm64); `node --test web/tests/*.test.mjs`; ONE full `go test -count=1 ./...`. Arc M has no live gate.

## File Structure

| File | Task(s) | Responsibility in this arc |
|---|---|---|
| `internal/monitor/utils.go` | 1 | `ProcessYouTubeVideo` gains the `OutcomeDenied` arm; `ProcessYouTubeVideoResult` gains `Denied` |
| `internal/monitor/decapi.go` | 1, 2 | terminal memo latches a denied verdict; 429 always engages the limiter; `scheduleNext`/`runCycle` ctx guards |
| `internal/monitor/feed.go` | 2, 9 | `scheduleNext`/`runCycle` ctx guards; `probeRowDated`'s corrected comment |
| `internal/monitor/twitch.go` | 2 | `scheduleNext`/`runCycle` ctx guards |
| `internal/notifications/manager.go` | 9 | `buildTargets` dedupes by resolved webhook URL |
| `internal/worker/scheduler.go` | 2 | `Run` performs one admission sweep before waiting |
| `internal/database/{database.go,database_jobs.go}` | 10 | delete `updateJobExec` + `UpdateJobSync` (no perf change) |
| `cmd/moombox/services.go` | 2, 10 | archive-slots resolver returns 0 for a disabled channel; `DpapiProfileDir` closure |
| `cmd/moombox/monitor_callbacks.go` | 2 | `resumeCookieParkedJobs` respects `queue_priority` and wakes the scheduler |
| `cmd/moombox/tui_wiring.go` | 4 | `cookieBadgeFor` learns the unreadable-file state |
| `internal/tui/status_bar.go` | 4 | `CookieStatusFileUnreadable` + its two render arms |
| `internal/cookies/jar.go` | 4 | `Load` records `filePath` on a failed read; `LastLoadError` |
| `internal/cookies/refresh_auth_status.go` | 4 | `AuthStatus.CookieFileError` |
| `internal/cookies/refresh_pass.go` | 4 | populates `CookieFileError` from the jar |
| `internal/cookies/cookie_import.go` | 3 | `ImportCookies` claims the `refreshCmd` sentinel |
| `internal/cookies/autocookies.go` | 8, 10 | `launchWindowKillBudget` becomes a `var`; `DpapiProfileDir` field |
| `internal/cookies/autocookies_setup.go` | 7 | `StartSetup` validates `platform` |
| `internal/cookies/autocookies_dpapi.go` | 10 | explicit profile dir takes precedence over discovery |
| `internal/cookies/autocookies_refresh.go` | 10 | passes the configured dir through |
| `internal/cookies/cookie_files.go` | 5 | POSIX dir tightening only on a dedicated directory |
| `internal/cookies/dpapi/profiles.go` | 10 | `ValidateProfileDir` |
| `internal/cookies/errors.go` | 7 | `ErrUnsupportedPlatform` |
| `internal/utils/dedicateddir.go` | 5 | **new** — the shared "is this a dedicated secrets directory" rule |
| `internal/config/config.go` | 5, 10 | `Save`'s chmod twin follows the same rule; validates the new key |
| `internal/config/types.go` | 10 | `CookiesConfig.DpapiProfileDir` |
| `internal/web/routes/cookies.go` | 4, 6, 7 | `fileError` on both payloads; `Content-Length` before the blocking re-check; loopback gate + platform 400 |
| `web/public/modules/utils.js` | 4, 7 | `cookieIndicatorState`'s unreadable arm; `viewerIsAtTheHost` |
| `web/public/modules/settings.js` | 7 | the two Set-up buttons refuse off-host |
| `internal/youtube/{player_api_strategy.go,service.go}` | 9 | the two false "anonymous" doc comments |
| `docs/spec/architecture.md` | 2 | §Backlog Scheduler: disabling a channel pauses its backlog |
| `docs/spec/data-and-storage.md` | 3, 6, 10, 11 | the O-D rule at :902; the :778 sentence; the new config row; COOKIES-5 |
| `docs/spec/security.md` | 11 | COOKIES-5 |
| `config.example.toml` | 10 | documents `dpapi_profile_dir` |

---

### Task 1: DECAPI stops jobbing a refused video, and a 429 always engages the limiter

Report rows #17 (MON-1) and #30 (MON-2). Both verified CONFIRMED-by-reproduction in `_verify-mon-cookies-tool.md`.

MON-1 has TWO halves and the verifier is explicit that **both must land together** — "or the fix trades a wrong job for a permanent probe loop". Half one: `ProcessYouTubeVideo` (whose ONLY non-test caller is `decapi.go`) treats `OutcomeDenied` exactly like a plain `upcoming`, so a members-only or `login_required` newest video becomes an Upcoming job, a "Stream Found" notification and then a COOKIES? park. Half two: a denied classification is not terminal, so without a memo latch the same video is re-probed every 15 s forever.

MON-2: `remaining` is written ONLY inside the `strconv.Atoi` success arm, so a 429 with no `Retry-After`, or an HTTP-date one, leaves the limiter untouched and the cycle keeps hitting decapi.me at the 1 s stagger.

**Files:**
- Modify: `internal/monitor/utils.go` (`ProcessYouTubeVideoResult`, `ProcessYouTubeVideo`'s outcome switch ~`:431-448`)
- Modify: `internal/monitor/decapi.go` (`decapiTerminalMemo` ~`:70-74`, `terminalMemoHit` ~`:799-806`, `recordTerminalMemo` ~`:813-820`, `fetchLatestVideo`'s 429 block ~`:540-558`, `waitForRateLimit` ~`:455-465`, the `recordTerminalMemo` call ~`:715`)
- Test: `internal/monitor/utils_test.go`, `internal/monitor/decapi_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `monitor.ProcessYouTubeVideoResult.Denied bool`; `(*DecapiMonitor).recordTerminalMemo(channelID, videoID, status string, denied bool)` (signature change, one caller); `(*DecapiMonitor).note429(resp *http.Response)`; package const `decapiDefaultRateLimitWindow = 60 * time.Second`. No later task consumes these.

- [ ] **Step 1: Write the failing tests**

Append to `internal/monitor/utils_test.go`:

```go
// TestProcessYouTubeVideo_DeniedIsNotAJob is MON-1's first half. A DENIED
// probe (upcoming + members_only/login_required — see isDenied) is YouTube
// refusing us, not a stream we found. Routing it to ShouldProcess=true is the
// 2.7.2 misfire the feed's ARCHIVE path already routes away (archive.go's
// `case OutcomeDenied:` arm), re-entering through DECAPI's door: an Upcoming
// job, a "Stream Found" notification, and then a COOKIES? park the config
// never asked for.
//
// Mutants each assertion kills:
//   - drop the OutcomeDenied arm from ProcessYouTubeVideo -> ShouldProcess
//     comes back true for both playability values (the shipped bug).
//   - return ShouldProcess=false but forget Denied -> the memo half in
//     decapi.go can never latch, so TestDecapi_DeniedVerdictIsLatched's
//     production wiring is dead and the refusal is re-probed every 15 s.
//   - write history on the denied arm -> histCalls becomes 1; a refusal is
//     not "we dealt with this video", and a history row would make the
//     members-only escalation's later sighting read as a re-probe.
func TestProcessYouTubeVideo_DeniedIsNotAJob(t *testing.T) {
	for _, playability := range []string{"members_only", "login_required"} {
		t.Run(playability, func(t *testing.T) {
			var histCalls int
			res := ProcessYouTubeVideo(ProcessYouTubeVideoParams{
				Ctx: context.Background(), VideoID: "v", Title: "T",
				Channel: &config.ChannelConfig{Name: "c"},
				ProbeVideo: func(ctx context.Context, id string) (*VideoProbeResult, error) {
					return &VideoProbeResult{StreamStatus: "upcoming", PlayabilityError: playability}, nil
				},
				AddToHistory: func(id string) error { histCalls++; return nil },
				Tracker:      NewMetadataFailureTracker(), Logger: silentLogger{},
			})
			if res.ShouldProcess {
				t.Errorf("ShouldProcess = true for a %s refusal — DECAPI would create an Upcoming job and park it in COOKIES?", playability)
			}
			if !res.Denied {
				t.Errorf("Denied = false — the DECAPI terminal memo latches on this flag; without it the refusal is re-probed every 15 s")
			}
			if res.StreamStatus != "upcoming" {
				t.Errorf("StreamStatus = %q, want %q — the memo records what the probe said", res.StreamStatus, "upcoming")
			}
			if histCalls != 0 {
				t.Errorf("AddToHistory called %d times — a refusal is not a video we dealt with", histCalls)
			}
		})
	}
}

// TestProcessYouTubeVideo_ProbedUpcomingStillJobs is the guard on the arm
// above: a genuine upcoming premiere (playability ok) must be unaffected.
// Mutant: widen the denied arm to every "upcoming" -> ShouldProcess goes
// false and Moombox stops archiving premieres entirely.
func TestProcessYouTubeVideo_ProbedUpcomingStillJobs(t *testing.T) {
	res := ProcessYouTubeVideo(ProcessYouTubeVideoParams{
		Ctx: context.Background(), VideoID: "v", Title: "T",
		Channel: &config.ChannelConfig{Name: "c"},
		ProbeVideo: func(ctx context.Context, id string) (*VideoProbeResult, error) {
			return &VideoProbeResult{StreamStatus: "upcoming", PlayabilityError: "ok"}, nil
		},
		Tracker: NewMetadataFailureTracker(), Logger: silentLogger{},
	})
	if !res.ShouldProcess || res.Denied {
		t.Fatalf("a public upcoming premiere must still job: %+v", res)
	}
}
```

Append to `internal/monitor/decapi_test.go` (add `"net/http"`, `"net/http/httptest"` and `"time"` to its imports if absent):

```go
// TestDecapi_DeniedVerdictIsLatched is MON-1's second half. "Denied" is not a
// terminal STATUS (it rides on "upcoming", which becomes live and then vod),
// so decapiTerminalStatus cannot latch it — yet DECAPI reports the channel's
// NEWEST video every cycle, and at the 15 s interval floor an un-latched
// refusal is ~240 anonymous player probes an hour for an answer only a cookie
// change can alter (and the feed's membership path owns that answer).
//
// Mutants:
//   - drop the `m.denied` arm from terminalMemoHit -> the first assertion
//     fails and the refusal is re-probed every cycle.
//   - latch on videoID alone (ignore denied) -> the second assertion fails
//     and a newly published stream is never probed.
//   - record denied for every outcome -> the third assertion fails and a
//     premiere is never picked up when it goes live.
func TestDecapi_DeniedVerdictIsLatched(t *testing.T) {
	dm := &DecapiMonitor{logger: silentLogger{}}

	dm.recordTerminalMemo("UC_a", "vid_denied_11", "upcoming", true)
	if !dm.terminalMemoHit("UC_a", "vid_denied_11", false, 30) {
		t.Error("a denied verdict did not latch — the same refusal is re-probed every 15 s forever")
	}
	if dm.terminalMemoHit("UC_a", "vid_other_111", false, 30) {
		t.Error("the latch fired for a DIFFERENT video — a newly published stream would never be probed")
	}

	// A non-denied "upcoming" is still not terminal: it becomes live.
	dm.recordTerminalMemo("UC_b", "vid_upcoming1", "upcoming", false)
	if dm.terminalMemoHit("UC_b", "vid_upcoming1", false, 30) {
		t.Error("an ordinary upcoming latched — the premiere would never be picked up when it goes live")
	}
}

// TestDecapi_429AlwaysEngagesTheLimiter is MON-2. `remaining` used to be
// written only inside the strconv.Atoi success arm, so a 429 with no
// Retry-After, or an HTTP-date one, left the limiter untouched and the cycle
// kept hitting decapi.me at the 1 s stagger — a throttle the code answered by
// continuing to request.
//
// Mutants:
//   - move `remaining = 0` back inside the Atoi arm -> rows 1, 2 and 4 see
//     remaining unchanged at 60.
//   - drop the `secs > 0` guard -> "-5" parses and puts resetAt in the PAST,
//     which waitForRateLimit's proactive reset immediately undoes (row 4).
func TestDecapi_429AlwaysEngagesTheLimiter(t *testing.T) {
	for _, tc := range []struct {
		name       string
		retryAfter string
		wantWindow time.Duration
	}{
		{"absent", "", decapiDefaultRateLimitWindow},
		{"http-date", "Wed, 21 Oct 2026 07:28:00 GMT", decapiDefaultRateLimitWindow},
		{"numeric seconds", "30", 30 * time.Second},
		{"negative seconds", "-5", decapiDefaultRateLimitWindow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			t.Cleanup(srv.Close)

			dm := &DecapiMonitor{logger: silentLogger{}}
			dm.rateLimit = rateLimitState{limit: 60, remaining: 60}

			resp, err := srv.Client().Get(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { resp.Body.Close() })
			dm.note429(resp)

			dm.mu.Lock()
			remaining := dm.rateLimit.remaining
			until := time.Until(dm.rateLimit.resetAt)
			dm.mu.Unlock()

			if remaining != 0 {
				t.Errorf("remaining = %d after a 429 — the limiter never engages and the cycle keeps requesting", remaining)
			}
			if until < tc.wantWindow-2*time.Second || until > tc.wantWindow+2*time.Second {
				t.Errorf("resetAt is %s away, want ~%s", until.Round(time.Second), tc.wantWindow)
			}
		})
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -run 'TestProcessYouTubeVideo_Denied|TestProcessYouTubeVideo_ProbedUpcoming|TestDecapi_DeniedVerdictIsLatched|TestDecapi_429' ./internal/monitor/ -v
```

Expected: compile failures — `res.Denied` undefined, `recordTerminalMemo` takes 3 args not 4, `decapiDefaultRateLimitWindow` undefined, `dm.note429` undefined. A compile failure IS the red state for a test that names symbols the change introduces; do not stub them out to make it "fail properly".

- [ ] **Step 3: Add `Denied` and the denied arm in `internal/monitor/utils.go`**

In `ProcessYouTubeVideoResult`, after the `PublishedAt` field, add:

```go
	// Denied is true when the probe COMPLETED and isDenied flagged it: YouTube
	// refused us (upcoming + members_only/login_required) rather than telling
	// us about a stream. ShouldProcess is always false alongside it. DECAPI
	// reads it to latch the verdict in its terminal memo — the classification
	// rides on "upcoming", which decapiTerminalStatus can never treat as
	// terminal, so without this flag the refusal is re-probed every cycle.
	Denied bool
```

Extend the outcome switch (the one whose existing arms are `OutcomeCooldown` and `OutcomeErrored`) with a third arm, and replace the paragraph that begins `// cr.Outcome is OutcomeProbed or OutcomeDenied here.`:

```go
	case OutcomeDenied:
		// DENIED FIRST, and for the same reason archive.go puts it first: a
		// refusal carries StreamStatus "upcoming" BY DEFINITION (see
		// isDenied), so a table that reaches the classification switch below
		// launders the 2.7.2 misfire into a broadcast job — an Upcoming row,
		// a "Stream Found" notification, and then a COOKIES? park the config
		// never asked for.
		//
		// NO AddToHistory. A refusal is not "we dealt with this video": the
		// members-only escalation lives on the FEED path, which owns the
		// authenticated answer, and a history row here would make its later
		// sighting read as a re-probe.
		//
		// The cost of routing login_required away is that a channel under
		// sustained anti-bot pushback gets its upcoming streams from the feed
		// path only — which already lives with that, for exactly the window
		// YouTube is refusing anonymous probes anyway.
		deniedLog := p.Logger.Info
		if p.IsReprobe {
			deniedLog = p.Logger.Debug
		}
		deniedLog(fmt.Sprintf("[Monitor] YouTube refused this video (%s); not creating a job: %s (%s)",
			cr.PlayabilityError, p.Title, p.VideoID))
		return ProcessYouTubeVideoResult{
			ShouldProcess: false,
			Denied:        true,
			Title:         p.Title,
			StreamStatus:  cr.StreamStatus,
		}
	}

	// cr.Outcome is OutcomeProbed here — the only outcome that reaches a
	// classification. OutcomeDenied used to fall through to this switch and be
	// treated as a plain "upcoming"; it now returns in the arm above.
```

- [ ] **Step 4: Latch the denied verdict in `internal/monitor/decapi.go`**

Add a field to `decapiTerminalMemo`, after `windowDays int`:

```go
	// denied records that the sighting ended because YouTube REFUSED the
	// probe (isDenied: upcoming + members_only/login_required). It is a
	// separate fact from `status` because the status it rides on is
	// "upcoming", which decapiTerminalStatus can never call terminal — so
	// without this the refusal is re-probed at the 15 s interval floor
	// forever, ~240 anonymous player calls an hour for an answer only a
	// cookie change can alter.
	//
	// Latching until the newest video CHANGES is deliberate and loses
	// nothing: the feed monitor probes the same video on its own cadence and
	// owns the authenticated members-only answer (walk.go's probeRow), so
	// DECAPI re-opening a refusal would only duplicate it.
	denied bool
```

`recordTerminalMemo` takes the flag — both facts are learned at the same instant, unlike the window verdict, which is why this is a parameter and `noteTerminalMemoOutsideWindow` is a second narrow update:

```go
func (dm *DecapiMonitor) recordTerminalMemo(channelID, videoID, status string, denied bool) {
	dm.mu.Lock()
	defer dm.mu.Unlock()
	if dm.terminalMemo == nil {
		dm.terminalMemo = make(map[string]decapiTerminalMemo)
	}
	dm.terminalMemo[channelID] = decapiTerminalMemo{videoID: videoID, status: status, denied: denied}
}
```

`terminalMemoHit` consults it before the terminal-status test:

```go
	m, ok := dm.terminalMemo[channelID]
	if !ok || m.videoID != videoID {
		return false
	}
	if m.denied {
		// A refusal needs no second reason. `reprobe` cannot apply (the denied
		// arm writes no history row) and the window check never ran, so the
		// two arms below would both answer false for a verdict that is in
		// fact settled until the channel publishes something new.
		return true
	}
	if !decapiTerminalStatus(m.status) {
		return false
	}
	return reprobe || (m.outsideWindow && m.windowDays == windowDays)
```

Update the single call site in `processResponse` (the line reading `dm.recordTerminalMemo(ch.ID, videoID, result.StreamStatus)`):

```go
	dm.recordTerminalMemo(ch.ID, videoID, result.StreamStatus, result.Denied)
```

- [ ] **Step 5: Make every 429 engage the limiter**

Add the constant beside the other DECAPI constants (near `decapiStagger`):

```go
// decapiDefaultRateLimitWindow is the window DECAPI's limiter assumes when the
// server throttles us without telling us for how long. 60 s is not a new
// number: it is the window fetchLatestVideo opens on every request and the one
// waitForRateLimit's defensive arm already synthesises for a remaining=0 with
// no resetAt.
const decapiDefaultRateLimitWindow = 60 * time.Second
```

Replace the two bare `60 * time.Second` literals — the one in `fetchLatestVideo` (`dm.rateLimit.resetAt = time.Now().Add(60 * time.Second)`) and the one in `waitForRateLimit`'s defensive arm (`rl.resetAt = time.Now().Add(60 * time.Second)`) — with `decapiDefaultRateLimitWindow`.

Add the accounting helper beside `updateRateLimit`:

```go
// note429 applies an explicit throttle to the limiter.
//
// EVERY 429 sets remaining=0, not just one carrying a numeric Retry-After.
// The header is optional, is often an HTTP-date, and decapi.me may send none
// at all — and the old code touched `remaining` only inside the strconv.Atoi
// success arm, so those shapes left the limiter fully stocked and the cycle
// kept hitting a server that had just said stop.
//
// A non-positive or unparseable Retry-After falls back to
// decapiDefaultRateLimitWindow. The `secs > 0` guard matters: "-5" parses
// cleanly and would put resetAt in the PAST, where waitForRateLimit's
// proactive reset restores `remaining` on the very next call.
func (dm *DecapiMonitor) note429(resp *http.Response) {
	window := decapiDefaultRateLimitWindow
	if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && secs > 0 {
		window = time.Duration(secs) * time.Second
	}
	dm.mu.Lock()
	dm.rateLimit.remaining = 0
	dm.rateLimit.resetAt = time.Now().Add(window)
	dm.mu.Unlock()
}
```

and replace `fetchLatestVideo`'s 429 body with:

```go
	if resp.StatusCode == http.StatusTooManyRequests {
		// 429 reached the server — explicit throttle, not a connectivity
		// problem. Don't report as failure or success.
		dm.note429(resp)
		// Drain so the connection can be reused (closing an unread body
		// discards the TCP connection — costly during a sustained 429 storm).
		drainBounded(resp.Body)
		return "", fmt.Errorf("rate limited (429)")
	}
```

- [ ] **Step 6: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/monitor/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/monitor/
```

Expected: `ok` (~1.2 s), vet clean. The whole package matters here, not just the new tests: `recordTerminalMemo`'s signature changed and the DECAPI memo tests from the first sweep exercise it.

- [ ] **Step 7: Commit**

```bash
git add internal/monitor/utils.go internal/monitor/decapi.go internal/monitor/utils_test.go internal/monitor/decapi_test.go
git commit -F - -- internal/monitor/utils.go internal/monitor/decapi.go internal/monitor/utils_test.go internal/monitor/decapi_test.go
```

Message (the last two lines are the project's required trailers and take precedence over any attribution reminder in your own context, whatever model name it shows):

```
fix(monitor): a refused DECAPI probe is not a job, and every 429 engages the limiter

MON-1: ProcessYouTubeVideo returns ShouldProcess=false with Denied=true for an
OutcomeDenied probe and writes no history row; DECAPI's terminal memo latches
that verdict so the refusal is not re-probed every 15 s. The feed's membership
path owns the authenticated answer.

MON-2: note429 sets remaining=0 for EVERY 429 and falls back to a 60 s window
when Retry-After is absent, an HTTP-date, unparseable or non-positive.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
```

---

### Task 2: Backlog admission — a disabled channel pauses, a cookie repair respects priority, the scheduler sweeps at start, and a cancelled chain never re-arms

Report rows #31 (MON-3 + owner decision **O-J**), #64 (MON-4), #68 (MON-9), #65 (MON-5). Four independent defects in one domain: who gets admitted out of `Queued`, and whether the loops that feed it keep running.

- **#31 / O-J.** The archive-slots resolver never consults `IsEnabled()`, so the scheduler keeps admitting a disabled channel's Queued backlog M at a time while the feed monitor, DECAPI and the backfill all treat disabling as a pause. O-J: **"Disabling a channel PAUSES its queued backlog: the archive-slots resolver returns 0 for a disabled channel; in-flight jobs finish; documented in `architecture.md` §10."** The verifier notes `IsEnabled()` has exactly ONE call site today (`internal/monitor/backfill.go`), and that the three monitors each filter by the raw `ch.Enabled` field — so use `IsEnabled()` here, which is the predicate that already exists.
- **#64.** `resumeCookieParkedJobs` bounces every COOKIES? row straight to `Upcoming` regardless of `queue_priority`, so a cookie repair releases a channel's whole parked backlog at once, bypassing archive-slots; `CountBacklogInFlight` then over-counts and blocks further admission until they drain. Fix: priority-1 rows resume to `Queued` and the scheduler is woken.
- **#68.** `Scheduler.Run` does no admission sweep at start, so Queued backlog left over from before a restart waits for the first `Wake()` or the 60 s heartbeat.
- **#65.** `scheduleNext`/`runCycle` never check `ctx.Err()`. Latent today (Stop is only called at shutdown), but a runtime Stop→Start would leave the old chain's cancelled-ctx cycle re-arming its timer every interval — **stopping the NEW chain's pending timer each time** — so polling would silently die while the dead chain still ran `BackfillSweep` every cycle.

**Files:**
- Modify: `cmd/moombox/services.go` — the archive-slots resolver only (`dlWorker.SetArchiveSlotsResolver(func(channelID string) int {…})`, ~`:712-727`)
- Modify: `cmd/moombox/monitor_callbacks.go` — `resumeCookieParkedJobs` and its two call sites (~`:81-105`, `:291`, `:350`)
- Modify: `internal/worker/scheduler.go` — `Run`
- Modify: `internal/monitor/feed.go`, `internal/monitor/decapi.go`, `internal/monitor/twitch.go` — `scheduleNext` + `runCycle`
- Modify: `docs/spec/architecture.md` — `### Backlog Scheduler`
- Test: `cmd/moombox/monitor_callbacks_test.go` (or the existing file that holds `sweepShouldResume`'s tests), `internal/worker/scheduler_test.go`, `internal/monitor/feed_test.go`

**Interfaces:**
- Consumes: nothing from Task 1.
- Produces: `resumeCookieParkedJobs(db *database.Database, log <anonymous logger>, wake func(), platform, currentIdentity string) int` — a new fourth parameter, `wake`, before the two strings. No later task consumes it.

- [ ] **Step 1: Write the failing tests**

`internal/worker/scheduler_test.go` — append:

```go
// TestScheduler_RunSweepsBeforeWaiting is MON-9. Run used to enter its
// select immediately, so Queued backlog left over from before a restart sat
// there until the first Wake() (a new discovery, or a job finishing) or the
// 60 s heartbeat. Nothing wakes a freshly started process whose only backlog
// predates it.
//
// Mutant: delete the s.sweep() above the inner loop -> nothing is admitted
// inside the 2 s window (the next chance is heartbeatInterval away).
func TestScheduler_RunSweepsBeforeWaiting(t *testing.T) {
	s, db, log := testSchedulerSetup(t, 2)
	chID := "UC_startup"
	addSchedJob(t, db, &chID, "v_leftover", database.StatusQueued, 1)
	addFeedItemRow(t, db, chID, "v_leftover", "2026-07-01T00:00:00Z")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.Run(ctx)

	waitForCond(t, 2*time.Second, "the startup sweep to admit the leftover backlog", func() bool {
		return log.enqueueCount() >= 1
	})
	if got := log.admitted(); len(got) != 1 || got[0] != "v_leftover" {
		t.Fatalf("admitted %v, want [v_leftover] — Run must sweep once before it waits", got)
	}
}
```

`internal/monitor/feed_test.go` — append (the same shape covers all three monitors; `decapi_test.go`/`twitch_test.go` get the sibling calls in the same test to keep the three honest about being identical):

```go
// TestMonitors_ACancelledChainNeverReArms is MON-5. Stop() cancels the chain's
// context but leaves the AfterFunc armed, and Start() then installs a NEW
// cancel. The old chain's cycle therefore still passed scheduleNext's only
// guard (`cancel == nil`) and re-armed the SHARED timer field every interval —
// stopping the live chain's pending cycle each time. Polling would die
// silently while the dead chain kept running a full cycle.
//
// Mutants:
//   - drop runCycle's ctx guard -> the dead chain runs doCheck (probes
//     recorded) and installs its scheduleNext defer.
//   - drop scheduleNext's ctx guard -> the dead chain overwrites the timer
//     and NextCheckAt, so the live chain's pending cycle is cancelled.
func TestMonitors_ACancelledChainNeverReArms(t *testing.T) {
	dead, cancel := context.WithCancel(context.Background())
	cancel()

	t.Run("feed", func(t *testing.T) {
		fm := &FeedMonitor{logger: silentLogger{}, configStore: storeWithOneYouTubeChannel(t)}
		liveCancel := func() {}
		fm.cancel = liveCancel // a LIVE chain owns the monitor now
		fm.NextCheckAt = 4242

		fm.scheduleNext(dead, time.Time{})
		if fm.timer != nil {
			t.Error("a cancelled chain armed fm.timer — it would cancel the live chain's pending cycle every interval")
		}
		if fm.NextCheckAt != 4242 {
			t.Errorf("NextCheckAt = %d, want 4242 — a dead chain must not rewrite the live chain's countdown", fm.NextCheckAt)
		}

		fm.runCycle(dead) // must return before the `checking` latch and before doCheck
		if fm.checking {
			t.Error("a cancelled chain latched `checking` — the live chain's next cycle would be dropped as a duplicate")
		}
		if fm.timer != nil {
			t.Error("runCycle's deferred scheduleNext ran for a cancelled chain")
		}
	})

	t.Run("decapi", func(t *testing.T) {
		dm := &DecapiMonitor{logger: silentLogger{}, configStore: storeWithOneYouTubeChannel(t)}
		dm.cancel = func() {}
		dm.NextCheckAt = 4242
		dm.scheduleNext(dead, time.Time{})
		if dm.timer != nil || dm.NextCheckAt != 4242 {
			t.Errorf("decapi: timer=%v NextCheckAt=%d — a cancelled chain must arm nothing", dm.timer != nil, dm.NextCheckAt)
		}
		dm.runCycle(dead)
		if dm.checking {
			t.Error("decapi: a cancelled chain latched `checking`")
		}
	})

	t.Run("twitch", func(t *testing.T) {
		tm := &TwitchMonitor{logger: silentLogger{}, configStore: storeWithOneTwitchChannel(t)}
		tm.cancel = func() {}
		tm.NextCheckAt = 4242
		tm.scheduleNext(dead, time.Time{})
		if tm.timer != nil || tm.NextCheckAt != 4242 {
			t.Errorf("twitch: timer=%v NextCheckAt=%d — a cancelled chain must arm nothing", tm.timer != nil, tm.NextCheckAt)
		}
		tm.runCycle(dead)
		if tm.checking {
			t.Error("twitch: a cancelled chain latched `checking`")
		}
	})
}
```

If `storeWithOneYouTubeChannel` / `storeWithOneTwitchChannel` do not already exist in the package's test files, add them beside the test — the monitors return early from `scheduleNext` when the channel list is empty, so the guard must be reached with at least one channel configured:

```go
// storeWithOneYouTubeChannel builds the smallest config store that gets a
// feed/DECAPI monitor past scheduleNext's "no channels" early return.
func storeWithOneYouTubeChannel(t *testing.T) *config.Store {
	t.Helper()
	cfg := config.Defaults()
	cfg.Channels = []config.ChannelConfig{{ID: "UC_x", Name: "x", Platform: "youtube"}}
	return config.NewStoreForTest(cfg)
}

func storeWithOneTwitchChannel(t *testing.T) *config.Store {
	t.Helper()
	cfg := config.Defaults()
	cfg.Channels = []config.ChannelConfig{{ID: "tw_x", Name: "x", Platform: "twitch"}}
	return config.NewStoreForTest(cfg)
}
```

> **Implementer note:** `config.NewStoreForTest` is a placeholder for whatever constructor the package's existing monitor tests already use to build a `*config.Store` (grep `configStore:` in `internal/monitor/*_test.go` and reuse it verbatim). Do not add a new exported constructor to `internal/config` — that package belongs to Arc C this wave.

`cmd/moombox` — append to the test file that already holds `sweepShouldResume`'s tests (grep `TestSweepShouldResume`):

```go
// TestResumeCookieParkedJobs_RespectsQueuePriority is MON-4. A cookie repair
// used to bounce EVERY parked row straight to Upcoming, which the worker's
// heartbeat poller then processes — so a channel's whole members-only backlog
// was released at once, bypassing the archive-slots pacing that exists to stop
// exactly that. CountBacklogInFlight then over-counts and blocks further
// admission until they drain.
//
// Mutants:
//   - send priority-1 rows to Upcoming -> the backlog row's status is wrong.
//   - send priority-0 rows to Queued -> a live/upcoming job would be stranded:
//     the scheduler only admits rows that have a channel_id and priority 1.
//   - drop the wake call -> wakes == 0 and the resumed backlog waits up to
//     60 s for the heartbeat.
func TestResumeCookieParkedJobs_RespectsQueuePriority(t *testing.T) {
	db := testDB(t)
	chID := "UC_park"
	for _, j := range []*database.Job{
		{ID: "backlog1", VideoID: "backlog1", URL: "u", Platform: "youtube",
			Status: database.StatusCookies, ChannelID: &chID, QueuePriority: 1},
		{ID: "broadcast1", VideoID: "broadcast1", URL: "u", Platform: "youtube",
			Status: database.StatusCookies, ChannelID: &chID, QueuePriority: 0},
	} {
		if _, err := db.AddJob(j); err != nil {
			t.Fatal(err)
		}
	}

	var wakes int
	resumed := resumeCookieParkedJobs(db, discardLog{}, func() { wakes++ }, "youtube", "")
	if resumed != 2 {
		t.Fatalf("resumed = %d, want 2", resumed)
	}

	backlog, _ := db.GetJob("backlog1")
	if backlog.Status != database.StatusQueued {
		t.Errorf("priority-1 row resumed to %q, want %q — going straight to Upcoming releases the whole backlog at once and bypasses archive-slots",
			backlog.Status, database.StatusQueued)
	}
	broadcast, _ := db.GetJob("broadcast1")
	if broadcast.Status != database.StatusUpcoming {
		t.Errorf("priority-0 row resumed to %q, want %q — the scheduler never admits a priority-0 row, so Queued would strand it forever",
			broadcast.Status, database.StatusUpcoming)
	}
	if wakes == 0 {
		t.Error("the scheduler was not woken — the resumed backlog waits up to 60 s for the heartbeat")
	}
	if backlog.ParkReason != database.ParkReasonNone || backlog.ParkIdentity != "" || backlog.Error != "" {
		t.Errorf("the park fields were not cleared on the Queued arm: %+v", backlog)
	}
}

// TestArchiveSlotsResolver_DisabledChannelGetsNone is MON-3 / owner decision
// O-J: disabling a channel PAUSES its queued backlog. Every discovery path
// already treats disabling as a pause (the three monitors filter on
// ch.Enabled; backfill.go's comment calls it "a pause, not a removal") while
// the resolver kept handing out slots, so the operator watched a channel they
// had just disabled keep starting downloads.
//
// Mutants:
//   - drop the IsEnabled() check -> the disabled channel still gets its slots.
//   - return 0 for an ABSENT channel too -> a removed channel's leftover
//     Queued rows would be stranded with no way out (row 3).
func TestArchiveSlotsResolver_DisabledChannelGetsNone(t *testing.T) {
	no, yes := false, true
	five := 5
	store := storeWithChannels(t, 3, []config.ChannelConfig{
		{ID: "UC_on", Platform: "youtube", Enabled: &yes},
		{ID: "UC_off", Platform: "youtube", Enabled: &no, ArchiveSlots: &five},
	})
	resolve := archiveSlotsResolver(store)

	if got := resolve("UC_on"); got != 3 {
		t.Errorf("enabled channel = %d, want 3 (the monitors.archive_slots default)", got)
	}
	if got := resolve("UC_off"); got != 0 {
		t.Errorf("disabled channel = %d, want 0 — its backlog must rest until it is re-enabled (O-J); the per-channel override must not rescue it either", got)
	}
	if got := resolve("UC_gone"); got != 3 {
		t.Errorf("channel with no config entry = %d, want 3 — a removed channel's leftover Queued rows must still drain", got)
	}
}
```

> **Implementer note:** `testDB`, `discardLog` and `storeWithChannels` are placeholders for whatever the `cmd/moombox` test package already has (grep `database.Open(` and `config.Store` in `cmd/moombox/*_test.go`). Reuse the existing helpers; add a tiny local one only if none exists.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -run 'TestScheduler_RunSweepsBeforeWaiting' ./internal/worker/ -v
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -run 'TestMonitors_ACancelledChainNeverReArms' ./internal/monitor/ -v
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -run 'TestResumeCookieParkedJobs_RespectsQueuePriority|TestArchiveSlotsResolver_DisabledChannelGetsNone' ./cmd/moombox/ -v
```

Expected: the scheduler test FAILS on the 2 s wait ("the startup sweep to admit the leftover backlog"); the monitor test FAILS on `fm.timer != nil`; the two `cmd/moombox` tests fail to compile (`resumeCookieParkedJobs` takes 4 args; `archiveSlotsResolver` undefined).

- [ ] **Step 3: The resolver returns 0 for a disabled channel**

In `cmd/moombox/services.go`, extract the inline closure into a named function so it can be tested, and add the gate. Replace the whole `dlWorker.SetArchiveSlotsResolver(func(channelID string) int { … })` call with:

```go
	dlWorker.SetArchiveSlotsResolver(archiveSlotsResolver(s.configStore))
```

and add, immediately below it (keeping the existing doc comment above `archiveSlotsResolver` and extending it):

```go
// archiveSlotsResolver builds the per-channel archive-slots resolver the
// backlog scheduler consults on every admission sweep (spec §10). The
// per-channel archive_slots override falls back to monitors.archive_slots, and
// the config store is re-read on every call so config edits take effect without
// a restart — channels are few, the scan is cheap.
//
// A DISABLED channel gets 0, by owner decision O-J: disabling PAUSES the
// channel's queued backlog. Every discovery path already reads disabling that
// way (the three monitors skip the channel, and internal/monitor/backfill.go
// keeps it in `active` while never scanning it, calling that "a pause, not a
// removal"), while this resolver kept handing out slots — so the scheduler went
// on admitting Queued rows M at a time for a channel the operator had just
// switched off. In-flight jobs are untouched: they have already left Queued, and
// the count this feeds is an admission budget, not a kill switch.
//
// A channel with NO config entry still gets the global default. That is a
// removed channel with leftover Queued rows, and returning 0 for it would
// strand them with no path out of Queued at all — the opposite failure to the
// one O-J fixes.
func archiveSlotsResolver(store *config.Store) func(channelID string) int {
	return func(channelID string) int {
		slots := 0
		store.Read(func(c *config.MoomboxConfig) {
			slots = c.Monitors.ArchiveSlots
			for i := range c.Channels {
				ch := &c.Channels[i]
				if ch.ID != channelID {
					continue
				}
				if !ch.IsEnabled() {
					slots = 0
					return
				}
				if ch.ArchiveSlots != nil && *ch.ArchiveSlots > 0 {
					slots = *ch.ArchiveSlots
				}
				return
			}
		})
		return slots
	}
}
```

`IsEnabled()` is the predicate `internal/monitor/backfill.go` already uses; the three monitors read the raw `ch.Enabled` pointer. Use `IsEnabled()` — one predicate, stated once.

- [ ] **Step 4: `resumeCookieParkedJobs` respects `queue_priority` and wakes the scheduler**

In `cmd/moombox/monitor_callbacks.go`, change the signature and the loop body:

```go
// resumeCookieParkedJobs applies sweepShouldResume to every job and returns
// how many were resumed. Split out of the callback closures so the decision
// and the database loop it actually drives can both be tested directly.
//
// THE TARGET STATUS DEPENDS ON queue_priority, and getting it wrong breaks the
// pacing in one direction or strands a job in the other:
//
//   - priority 1 (backlog) resumes to Queued and the scheduler is woken. It is
//     the only path out of Queued, so it re-admits these archive_slots at a
//     time. Sending them to Upcoming instead — what this did before — handed
//     the whole of a channel's parked backlog to the worker's heartbeat poller
//     at once, bypassed archive-slots entirely, and left CountBacklogInFlight
//     over-counting until they drained.
//   - priority 0 (live, upcoming, manually added) resumes to Upcoming. The
//     scheduler never admits a priority-0 row, so Queued would strand it.
//
// wake is the scheduler's Wake (production: s.dlWorker.Scheduler().Wake).
// Called once, after the loop, and only when something was resumed: Wake
// coalesces into a capacity-1 channel, so one signal is all a sweep can use.
func resumeCookieParkedJobs(db *database.Database, log interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}, wake func(), platform, currentIdentity string) int {
	jobs, err := db.GetAllJobs()
	if err != nil {
		log.Warn("cookie-parked sweep: GetAllJobs failed", "platform", platform, "err", err)
		return 0
	}
	resumed := 0
	for _, job := range jobs {
		if !sweepShouldResume(job, platform, currentIdentity) {
			continue
		}
		status := database.StatusUpcoming
		if job.QueuePriority == 1 {
			status = database.StatusQueued
		}
		db.UpdateJobFields(job.ID, map[string]any{
			"status":        status,
			"error":         "",
			"park_reason":   database.ParkReasonNone,
			"park_identity": "",
		})
		resumed++
	}
	if resumed > 0 && wake != nil {
		wake()
	}
	return resumed
}
```

Update the two call sites (`OnAuthRecovered` and the identity-observed sweep) to pass the scheduler's `Wake`:

```go
		resumed := resumeCookieParkedJobs(s.db, s.log, s.dlWorker.Scheduler().Wake, platform, "")
```

```go
		resumed := resumeCookieParkedJobs(s.db, s.log, s.dlWorker.Scheduler().Wake, platform, identity)
```

- [ ] **Step 5: `Scheduler.Run` sweeps once before it waits**

In `internal/worker/scheduler.go`, inside `Run`'s inner `func()`, between the recover defer and the `for {` loop:

```go
			// Startup admission sweep (MON-9). Nothing Wakes a process whose
			// only backlog predates it: the wake sites are backlog CREATION and
			// job COMPLETION, and a restart has neither, so leftover Queued rows
			// waited for the 60 s heartbeat. sweep() is idempotent and
			// single-threaded by construction, so running it again after a
			// panic-restart costs one extra pass.
			s.sweep()
```

- [ ] **Step 6: The three monitors refuse to serve a cancelled chain**

In each of `internal/monitor/{feed,decapi,twitch}.go`, add the guard to `runCycle` immediately after the recover defer and BEFORE the `checking` latch (use the receiver name each file already uses — `fm` / `dm` / `tm`):

```go
	// A cancelled context means this cycle belongs to a STOPPED chain. Stop()
	// cancels the context but leaves the AfterFunc armed, and a later Start()
	// installs a new cancel — so the dead chain's cycle used to pass every
	// guard, run a full doCheck, and then re-arm the SHARED timer field,
	// cancelling the live chain's pending cycle every interval. Returning here,
	// before the `checking` latch and before the scheduleNext defer is
	// installed, is what stops that. Latent today (Stop runs only at shutdown).
	if ctx.Err() != nil {
		return
	}
```

and in each `scheduleNext`, immediately after the existing `if <recv>.cancel == nil { … }` block inside the same locked section:

```go
	// Same rule as runCycle's guard, for the path that arms the timer. Checked
	// AFTER `cancel == nil` and deliberately WITHOUT touching NextCheckAt: the
	// countdown belongs to whichever chain is live now, and a dead chain
	// zeroing it would blank the UI's next-check time for no reason.
	if ctx.Err() != nil {
		<recv>.mu.Unlock()
		return
	}
```

- [ ] **Step 7: Document O-J in `docs/spec/architecture.md`**

In `### Backlog Scheduler`, after the `resolveSlots is injected by cmd/moombox…` bullet, add:

```markdown
- A **disabled** channel resolves to 0 slots, so disabling it PAUSES its queued backlog (owner decision O-J, 2026-09-17). Every discovery path already reads `enabled = false` as a pause — the feed, DECAPI and Twitch monitors skip the channel, and the backfill keeps it in `active` while never scanning it — and the resolver was the one place that did not, so a disabled channel went on starting downloads M at a time. In-flight jobs are untouched: they have already left `Queued`, and this number is an admission budget rather than a kill switch. A channel with **no config entry at all** still gets the global default, so a removed channel's leftover `Queued` rows are not stranded.
```

- [ ] **Step 8: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/monitor/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/worker/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./cmd/moombox/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/docs/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/monitor/ ./internal/worker/ ./cmd/moombox/
```

Expected: all `ok`. `./internal/docs/` is gated because `architecture.md` changed and because `resumeCookieParkedJobs`'s signature moved.

- [ ] **Step 9: Commit**

```bash
git add cmd/moombox/services.go cmd/moombox/monitor_callbacks.go internal/worker/scheduler.go \
        internal/monitor/feed.go internal/monitor/decapi.go internal/monitor/twitch.go \
        docs/spec/architecture.md \
        internal/worker/scheduler_test.go internal/monitor/feed_test.go cmd/moombox/monitor_callbacks_test.go
git commit -F - -- <the same paths>
```

Message:

```
fix(worker,monitor): disabling a channel pauses its backlog; admission is priority-aware and swept at start

O-J / MON-3: the archive-slots resolver returns 0 for a disabled channel, so
its Queued backlog rests until it is re-enabled. In-flight jobs finish; a
channel with no config entry still gets the global default. Documented in
architecture.md's Backlog Scheduler section.

MON-4: resumeCookieParkedJobs resumes priority-1 rows to Queued and wakes the
scheduler, so a cookie repair no longer releases a whole backlog at once.

MON-9: Scheduler.Run performs one admission sweep before entering its wait.

MON-5: all three monitors return early on a cancelled context, so a chain that
Stop() retired can never re-arm the timer the live chain owns.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
```

---

### Task 3: `ImportCookies` claims the refresh slot, and the Arc 8 ruling text is rewritten to O-D

Report row #18 (COOKIES-1) + owner decision **O-D**. Reproduced by both the area reviewer and the verifier: an operator's paste landing inside a browser-refresh or recovery pass's read → verify (≤ 12 s) → write gap is silently overwritten by that pass's merge of the PRE-paste file, while the import answers `Wrote=true / youtube=ok / twitch=ok`. On the recovery path the rows that replace the paste are the dead ones that raised the alarm.

**O-D, verbatim:** *"`ImportCookies` claims the `refreshCmd` sentinel; during a pass it answers `ErrRefreshInProgress` → HTTP 409 'refresh in progress, try again shortly'; the no-lock property is kept."*

The verifier's correction (which overrides the area report): the row's stated justification — "the ruling predates `ImportCookies`" — is **wrong**; `data-and-storage.md:902` already names `ImportCookies` as the fifth writer and already prices the widened ~12 s window. The genuinely new fact is the **direction of the loss**: the ruling says "loses at most a rotation the next 30-minute pass repairs", and what is actually lost is the operator's paste. **Rewrite that paragraph to the O-D rule** — do not merely append to it.

**Files:**
- Modify: `internal/cookies/cookie_import.go` (`ImportCookies`, the doc paragraph beginning `// NOT gated on the `stopped` latch` and the function's first statements)
- Modify: `internal/web/routes/cookies.go` (the import handler's error switch)
- Modify: `docs/spec/data-and-storage.md` (the "The five share no lock, by owner ruling (Arc 8, 2026-08-29)…" sentence through the end of that paragraph, ~`:902`)
- Test: `internal/cookies/cookie_import_test.go` (or the existing import test file), `internal/web/routes/cookies_test.go`

**Interfaces:**
- Consumes: nothing from Tasks 1–2.
- Produces: `ImportCookies` may now return an error wrapping the existing `cookies.ErrRefreshInProgress` sentinel with `ImportResult{}` (`Wrote` false). No new symbol.

- [ ] **Step 1: Write the failing tests**

`internal/cookies` — append to the file that already exercises `ImportCookies` (grep `func TestImportCookies`):

```go
// TestImportCookiesIsRefusedWhileARefreshHoldsTheSlot is COOKIES-1 / owner
// decision O-D. ImportCookies used to take no slot at all, so a paste landing
// inside a browser-refresh or recovery pass's read -> verify (<=12 s) -> write
// gap was overwritten by that pass's merge of the PRE-paste file — and the
// import reported Wrote=true with both platforms ok while it happened. On the
// recovery path the rows that replace the paste are the dead ones that raised
// the alarm.
//
// A 409 the operator retries within ~2 minutes is the trade O-D chose over
// re-reading and re-merging in every pass. The no-lock property is kept: this
// is the SAME sentinel StartSetup and RefreshCookiesDetailed already gate on,
// not a new mutex over cookies.txt.
//
// Mutants:
//   - drop the refreshCmd check -> err is nil, Wrote is true, and the file on
//     disk carries the paste that the pass is about to overwrite.
//   - claim the slot but never release it -> the second half fails: every
//     later import (and every refresh, and StartSetup) is refused forever.
func TestImportCookiesIsRefusedWhileARefreshHoldsTheSlot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	s := NewAutoCookieService(t.TempDir(), path, NewCookieJar(), nopAutoCookieLogger{})

	// The slot as RefreshCookiesDetailed claims it: a sentinel with no process.
	s.mu.Lock()
	s.refreshCmd = &exec.Cmd{}
	s.mu.Unlock()

	res, err := s.ImportCookies(context.Background(), netscapeWithYouTubeAuth)
	if !errors.Is(err, ErrRefreshInProgress) {
		t.Fatalf("err = %v, want ErrRefreshInProgress — an import that lands inside a pass is silently destroyed by it", err)
	}
	if res.Wrote {
		t.Error("Wrote = true on a refused import — nothing may be written while a pass holds the slot")
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("cookies.txt exists after a refused import (stat err = %v)", statErr)
	}

	// Releasing the slot lets the retry through, and the import releases the
	// slot it claims.
	s.mu.Lock()
	s.refreshCmd = nil
	s.mu.Unlock()

	res, err = s.ImportCookies(context.Background(), netscapeWithYouTubeAuth)
	if err != nil || !res.Wrote {
		t.Fatalf("the retry after the pass finished must succeed: res=%+v err=%v", res, err)
	}
	s.mu.Lock()
	held := s.refreshCmd
	s.mu.Unlock()
	if held != nil {
		t.Error("ImportCookies did not release the refresh slot — every later refresh, setup and import would be refused for the life of the process")
	}
}
```

> **Implementer note:** `netscapeWithYouTubeAuth` is a placeholder for the package's existing valid-paste fixture (grep the import tests for the Netscape literal they already share) and `nopAutoCookieLogger` already exists. Add `"os/exec"`, `"errors"`, `"os"`, `"path/filepath"` to the imports as needed.

`internal/web/routes` — append to `cookies_test.go`:

```go
// TestCookieImportAnswers409WhileARefreshRuns pins the wire half of O-D. The
// arm has to sit AHEAD of the `result.Wrote` and default arms: Wrote is false
// here, so without its own case the refusal would answer 500 "cookie import
// failed" — a server fault for a condition the operator fixes by waiting two
// minutes, which is the same answer StartSetup already gives for the same
// sentinel.
//
// Mutants:
//   - drop the ErrRefreshInProgress case -> 500.
//   - answer 503 (the ErrServiceStopped shape) -> the status assertion fails;
//     503 says "this never clears", and this one clears on its own.
//   - answer 401 -> app.js's global fetch interceptor treats 401 as an expired
//     session and RELOADS the page, losing the operator's paste.
func TestCookieImportAnswers409WhileARefreshRuns(t *testing.T) { /* … */ }
```

Write that test against whatever harness `cookies_test.go` already uses to exercise the import route (grep `"/api/cookies/import"` in the file and copy the surrounding setup verbatim). Assert `rec.Code == http.StatusConflict` and that the JSON `error` string contains `"cookie refresh in progress"`.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -run 'TestImportCookiesIsRefusedWhileARefreshHoldsTheSlot' ./internal/cookies/ -v
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -run 'TestCookieImportAnswers409WhileARefreshRuns' ./internal/web/routes/ -v
```

Expected: the cookies test FAILS with `err = <nil>, want ErrRefreshInProgress` (and `Wrote = true`); the routes test FAILS with `409 != 500`.

- [ ] **Step 3: Claim the slot in `ImportCookies`**

Replace the doc paragraph that begins `// NOT gated on the `stopped` latch, unlike StartSetup and` with:

```go
// GATED ON THE REFRESH SLOT, and on nothing else (owner decision O-D,
// 2026-09-17). A paste that lands inside a browser-refresh or recovery pass's
// read -> verify -> write gap is destroyed by that pass's merge of the
// PRE-paste file, and the import answers "imported" while it happens; on the
// recovery path the rows that replace it are the dead ones that raised the
// alarm. Claiming the same refreshCmd sentinel RefreshCookiesDetailed and
// StartSetup already use makes the two mutually exclusive without introducing
// a lock over cookies.txt — the no-lock property the Arc 8 ruling protects is
// kept, and the answer is the one StartSetup already gives: "please try again
// shortly", HTTP 409, retried within ~2 minutes.
//
// Still NOT gated on the `stopped` latch. That refusal is about launching or
// steering a browser PROCESS; this launches nothing, and refusing an import
// during a drain would throw away credentials the operator supplied by hand
// for the sake of a shutdown that is about to read the file back on the next
// start anyway.
//
// Still NOT gated on setupInProgressLocked either, and that is a RESIDUAL
// rather than an oversight: a wizard finish is a third writer with its own
// read -> write gap, but it is gated by the setup slot, not this one, and
// widening the import's gate to cover it would refuse the container operator's
// only re-authentication route for the 60 s grace a stale setup slot lingers.
// O-D was scoped to the refresh collision, which is the one that was
// reproduced.
//
// A claim held here makes Stop()'s killRefreshProcess poll for
// launchWindowKillBudget before giving up, exactly as it already does for a
// refresh caught inside its launch window — a bounded shutdown delay, not a
// new one.
```

Insert the claim as the function's first act, ahead of the `s.cookiePath == ""` guard so no branch can return without releasing what it took:

```go
func (s *AutoCookieService) ImportCookies(ctx context.Context, netscape string) (ImportResult, error) {
	s.mu.Lock()
	if s.refreshCmd != nil {
		s.mu.Unlock()
		// The same sentence StartSetup answers with for the same sentinel, so
		// the two surfaces cannot drift; the Web route maps it to 409.
		return ImportResult{}, fmt.Errorf("please try again shortly: %w", ErrRefreshInProgress)
	}
	s.refreshCmd = &exec.Cmd{} // sentinel to claim the slot (see RefreshCookiesDetailed)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.refreshCmd = nil
		s.mu.Unlock()
	}()

	if s.cookiePath == "" {
```

Add `"os/exec"` to the file's imports.

- [ ] **Step 4: Answer 409 on the route**

In `internal/web/routes/cookies.go`, in the import handler's `switch` (the one whose first arm is `errors.Is(err, cookies.ErrImportNotNetscape)`), add as the FIRST case:

```go
			// A CONDITION that clears on its own, so 409 and "try again
			// shortly" — the identical answer /auto-setup/start gives for the
			// identical sentinel. It must sit ahead of the `result.Wrote` and
			// default arms: Wrote is false on this exit, so without its own
			// case a refusal would read as a 500 server fault for something
			// the operator fixes by waiting two minutes. Never 401 — app.js's
			// global fetch interceptor treats 401 as an expired session and
			// reloads the page, which would throw the paste away.
			case errors.Is(err, cookies.ErrRefreshInProgress):
				jsonError(rw, err.Error(), http.StatusConflict)
```

- [ ] **Step 5: Rewrite the ruling text in `docs/spec/data-and-storage.md`**

Replace the sentence beginning **"The five share no lock, by owner ruling (Arc 8, 2026-08-29): …"** and the bolded follow-on **"That window is wider on the pasted import since 2.8.7: …"** — i.e. everything from "The five share no lock" to the end of that paragraph — with:

```markdown
The five share no lock, and they never will: the containment is a SLOT, not a mutex over `cookies.txt` (owner decision O-D, 2026-09-17, superseding the Arc 8 ruling of 2026-08-29). `ImportCookies` now claims the same `refreshCmd` sentinel `RefreshCookiesDetailed` and `StartSetup` already gate on, and answers `ErrRefreshInProgress` — "please try again shortly", HTTP 409 on the Web route, the identical sentence `/api/cookies/auto-setup/start` gives for the identical sentinel — while a pass holds it. What reopened the Arc 8 ruling was not the width of the window but the DIRECTION of the loss: the ruling priced it as "loses at most a rotation the next 30-minute pass repairs", and the reproduction showed the pass destroying the OPERATOR'S PASTE, replacing it on the recovery path with the dead rows that raised the alarm, while the import reported `Wrote=true` with both platforms `ok`. The window it closes is the read → verify → write gap, which since 2.8.7 also contains the pre-write snapshot's two verification round trips — `checkPlatformAuth` bounds those with one `authVerifyTimeout` (12 s) PER PLATFORM, run concurrently, so the gap is bounded by ~12 s rather than by the merge alone. The re-read-and-re-merge containment the Arc 8 text named as its preferred fix was NOT taken: it keeps the import always-accepted at the cost of an extra read and merge in every pass, and a refusal the operator retries within two minutes is the cheaper honest answer. One residual is deliberate and stated: the setup wizard's finish is a third writer with its own read → write gap, gated by the SETUP slot rather than this one, and widening the import's gate to cover it would refuse a container operator's only re-authentication route for the 60 s grace a stale setup slot lingers.
```

- [ ] **Step 6: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/cookies/          # ~38-41 s
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/web/routes/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/docs/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/cookies/ ./internal/web/routes/
```

Expected: all `ok`. Pay attention to the AST call-site test (`TestEveryCookieWriteRecheckIsDeferred` and the Arc 7 R7 call-site test) — `ImportCookies` gained an early return ahead of every write, which is exactly the shape those tests protect.

- [ ] **Step 7: Commit**

```bash
git add internal/cookies/cookie_import.go internal/web/routes/cookies.go docs/spec/data-and-storage.md \
        internal/cookies/cookie_import_test.go internal/web/routes/cookies_test.go
git commit -F - -- <the same paths>
```

Message:

```
fix(cookies,web): an import refuses rather than being destroyed by a refresh pass

COOKIES-1 / owner decision O-D. ImportCookies claims the refreshCmd sentinel
RefreshCookiesDetailed and StartSetup already gate on, and answers
ErrRefreshInProgress -> HTTP 409 "please try again shortly" while a pass holds
it. Before this, a paste landing inside a pass's read -> verify -> write gap was
overwritten by that pass's merge of the pre-paste file while the import
reported Wrote=true with both platforms ok.

The Arc 8 no-lock ruling text in data-and-storage.md is rewritten to O-D: what
reopened it was the direction of the loss, not the width of the window.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
```

---

### Task 4: a failed `Load` keeps retrying, and "could not be read" reaches both dashboards

Report rows #19 (COOKIES-2) and #70 (COOKIES-6) — the report itself pairs them.

- **#19.** A boot `Load` that fails with anything but ENOENT never records `j.filePath`, so every later `Reload()` — the 30-minute pass, every `SyncCookies` before an extraction, `twitch.Auth.Reload` — returns nil having read nothing. Fixing the permission on the host does nothing until a restart. Reproduced by both reviewers.
- **#70.** An unreadable `cookies.txt` renders on BOTH dashboards as never-configured (`HasYouTubeCookies=false` → `found:false`, TUI `CookieStatusNone`), because `AuthStatus` has no "file could not be read" state. The only evidence is one boot Warn — and, per #19, nothing afterwards.

**Files:**
- Modify: `internal/cookies/jar.go` (`CookieJar` struct, `Load`'s error arms, `parseInto`'s install block, new `LastLoadError`)
- Modify: `internal/cookies/refresh_auth_status.go` (`AuthStatus`, `authStatusChanged`)
- Modify: `internal/cookies/refresh_pass.go` (the locked snapshot block and the `rs.status = AuthStatus{…}` literal)
- Modify: `internal/web/routes/cookies.go` (`CookieStatusPayload`, `TwitchAuthStatusPayload`)
- Modify: `web/public/modules/utils.js` (`cookieIndicatorState`)
- Modify: `internal/tui/status_bar.go` (`CookieStatus` enum, the two render ladders, a label helper)
- Modify: `cmd/moombox/tui_wiring.go` (`cookieBadgeFor` and its two call sites)
- Test: `internal/cookies/jar_storage_test.go`, `internal/web/routes/cookies_test.go` (new Go test exercising the shipped `utils.js` through the existing goja harness), `internal/tui/status_bar_test.go`

**Interfaces:**
- Consumes: nothing from Tasks 1–3.
- Produces:
  - `func (j *CookieJar) LastLoadError() string` — empty when the last `Load` succeeded or found no file; otherwise a bounded, path-only sentence.
  - `cookies.AuthStatus.CookieFileError string` (JSON `cookieFileError,omitempty`).
  - `tui.CookieStatusFileUnreadable` (appended to the enum — never inserted; `CookieStatusNone` must stay the zero value).
  - `cookieBadgeFor(authenticated, hasCookies, fileUnreadable bool, verdict cookies.RefreshVerdict) tui.CookieStatus` — a new THIRD parameter.
  - wire key `fileError` on both cookie status payloads; `status.fileError` read by `cookieIndicatorState`.

- [ ] **Step 1: Write the failing tests**

`internal/cookies/jar_storage_test.go` — append:

```go
// TestLoadRecordsThePathEvenWhenTheReadFails is COOKIES-2. Only the ENOENT arm
// recorded j.filePath, so a boot Load that failed with EACCES (the compose
// `user:` uid mismatch the docs name as the likeliest container failure) left
// filePath empty — and Reload() short-circuits on an empty path. Every later
// credential read for the life of the process then silently read nothing:
// the 30-minute pass, every SyncCookies before an extraction, twitch.Auth.Reload.
// Repairing the permission on the host did nothing until a restart.
//
// Mutants:
//   - drop `j.filePath = filePath` from the error arm -> Reload is a no-op and
//     hasAuth stays false after the permission is repaired.
//   - clear lastLoadErr in the error arm -> LastLoadError() is empty and both
//     dashboards go back to reporting a mounted file as never-configured.
//   - set lastLoadErr on the ENOENT arm -> the "absent" subtest fails; a file
//     that does not exist is never-configured, not unreadable.
func TestLoadRecordsThePathEvenWhenTheReadFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")

	jar := NewCookieJar()

	// Absent: recorded, no error sentinel.
	if err := jar.Load(path); err != nil {
		t.Fatalf("an absent cookies.txt is not an error: %v", err)
	}
	if got := jar.LastLoadError(); got != "" {
		t.Errorf("LastLoadError() = %q for an ABSENT file — that is never-configured, not unreadable", got)
	}

	// Unreadable: recorded, sentinel set, maps untouched.
	restore := makeUnreadable(t, path)
	err := jar.Load(path)
	if err == nil {
		t.Fatal("an unreadable cookies.txt must return an error")
	}
	sentinel := jar.LastLoadError()
	if sentinel == "" {
		t.Error("LastLoadError() is empty after a failed read — both dashboards would show the mounted file as never-configured")
	}
	if !strings.Contains(sentinel, "cookies.txt") {
		t.Errorf("LastLoadError() = %q — it must name the path so the operator knows which file to fix", sentinel)
	}
	if len(sentinel) > cookieLoadErrorMaxLen {
		t.Errorf("LastLoadError() is %d bytes — it reaches a status line and must stay bounded", len(sentinel))
	}

	// Repairing the permission is enough: Reload picks the file up with no restart.
	restore()
	if err := os.WriteFile(path, []byte(netscapeWithYouTubeAuth), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := jar.Reload(); err != nil {
		t.Fatalf("Reload after the repair: %v", err)
	}
	if !jar.HasAnyYouTubeAuthCookie() {
		t.Error("Reload() read nothing — the failed boot Load never recorded the path, so it stays a no-op until a restart")
	}
	if got := jar.LastLoadError(); got != "" {
		t.Errorf("LastLoadError() = %q after a successful reload — the sentinel must clear", got)
	}
}
```

`makeUnreadable` must work on Windows AND Linux, so do NOT rely on `os.Chmod(0o000)` (a no-op for the owner on Windows and often for root on Linux). Use the package's existing `cookieJarReadFile` seam instead — it is the documented way to drive this branch:

```go
// makeUnreadable swaps the jar's read seam for one that fails with
// fs.ErrPermission for this path, and returns the restore func. The seam
// rather than a real chmod: 0o000 is a no-op for the file's owner on Windows
// and for root on Linux, so a real permission change cannot drive this branch
// portably — and this test must run on both CI legs.
func makeUnreadable(t *testing.T, path string) (restore func()) {
	t.Helper()
	real := cookieJarReadFile
	cookieJarReadFile = func(p string) ([]byte, error) {
		if p == path {
			return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrPermission}
		}
		return real(p)
	}
	restore = func() { cookieJarReadFile = real }
	t.Cleanup(restore)
	return restore
}
```

> **Implementer note:** confirm `cookieJarReadFile` is a package `var` (it is used as a seam elsewhere in this package) before relying on it; if the package already has a helper that installs a failing read seam, use that one instead of adding a second.

`internal/web/routes/cookies_test.go` — append (the package already has `utilsVM`/`jsCall` for running the SHIPPED `utils.js` under goja, in `cookies_setup_utilsvm_test.go`):

```go
// TestCookieIndicatorNamesAnUnreadableFile is COOKIES-6's Web half, run out of
// the shipped utils.js. An unreadable cookies.txt used to fall into the
// `!found` arm and render as never-configured — the container operator was
// told "no cookies" about a file sitting on the volume.
//
// The arm sits AFTER `authenticated`, deliberately: a later reload failing over
// a jar that still holds working credentials is not a reason to redden a badge
// whose requests are succeeding. It is the "no cookies" misreport that is being
// corrected, not the green state.
//
// Mutants:
//   - drop the fileError arm -> row 1 renders the absent-copy (indicator-warn
//     or the platform's `absent` title) instead of naming the file.
//   - put the arm ahead of `authenticated` -> row 3 turns red while
//     authenticated requests are demonstrably working.
func TestCookieIndicatorNamesAnUnreadableFile(t *testing.T) {
	vm := utilsVM(t)
	for _, tc := range []struct {
		name         string
		status       map[string]any
		wantClass    string
		wantContains string
	}{
		{
			"unreadable file, nothing loaded",
			map[string]any{"found": false, "authenticated": false, "verification": "unknown",
				"fileError": "open /data/cookies.txt: permission denied"},
			"indicator-error", "could not be read",
		},
		{
			"ordinary never-configured",
			map[string]any{"found": false, "authenticated": false, "verification": "unknown", "fileError": ""},
			"", "",
		},
		{
			"unreadable file but the jar still authenticates",
			map[string]any{"found": true, "authenticated": true, "verification": "ok",
				"fileError": "open /data/cookies.txt: permission denied"},
			"indicator-ok", "Authenticated",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := jsCall(t, vm, "cookieIndicatorState", "youtube", tc.status, false, false).(map[string]any)
			class, _ := got["className"].(string)
			title, _ := got["title"].(string)
			if tc.wantClass != "" && class != tc.wantClass {
				t.Errorf("className = %q, want %q (title %q)", class, tc.wantClass, title)
			}
			if tc.wantContains != "" && !strings.Contains(title, tc.wantContains) {
				t.Errorf("title = %q, want it to contain %q", title, tc.wantContains)
			}
			if tc.wantClass == "" && strings.Contains(title, "could not be read") {
				t.Errorf("title = %q — a file that is merely absent must not claim it could not be read", title)
			}
		})
	}
}
```

`internal/tui/status_bar_test.go` — append:

```go
// TestStatusBarNamesAnUnreadableCookieFile is COOKIES-6's TUI half. The bar had
// no state for it, so an unreadable cookies.txt rendered as CookieStatusNone —
// the yellow "never configured" badge — for a file sitting on the volume.
//
// Red and NOT gated on `healthy`, the same shape CookieStatusRelogin already
// has: it is a conclusive, operator-actionable failure, so it must survive
// every tier rather than dropping out at tierEssential like Unknown does.
//
// Mutants:
//   - render it through the `healthy` gate -> the tierEssential row loses the
//     badge exactly when the bar is narrowest and the operator most confused.
//   - reuse "YT!" (the Relogin abbreviation) -> the tierTight row becomes
//     indistinguishable from a re-login prompt, which has a different remedy.
func TestStatusBarNamesAnUnreadableCookieFile(t *testing.T) { /* … */ }
```

Write it against the package's existing status-bar rendering harness (grep `CookieStatusRelogin` in `status_bar_test.go` and copy the setup verbatim). Assert: at `tierFull` the rendered bar contains `YT: cookies.txt unreadable`; at `tierTight` and at `tierEssential` it still contains a `YT` badge; and that the string differs from the Relogin rendering at `tierTight`.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -run 'TestLoadRecordsThePathEvenWhenTheReadFails' ./internal/cookies/ -v
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -run 'TestCookieIndicatorNamesAnUnreadableFile' ./internal/web/routes/ -v
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -run 'TestStatusBarNamesAnUnreadableCookieFile' ./internal/tui/ -v
```

Expected: compile failure on `LastLoadError`/`cookieLoadErrorMaxLen`/`CookieStatusFileUnreadable`; the JS test fails on the title assertion.

- [ ] **Step 3: The jar records the path and the reason**

`internal/cookies/jar.go` — add the field to `CookieJar`, after `logger`:

```go
	// lastLoadErr is the reason the most recent Load could not READ the file,
	// bounded and path-only (never content). Empty when the last Load
	// succeeded, and empty for an ABSENT file — "there is no cookies.txt" is
	// never-configured, which the dashboards already render correctly; this
	// field exists for the case they used to render as never-configured
	// WRONGLY, a file that is present and unreadable. Written under j.mu by
	// Load and cleared by parseInto's install.
	lastLoadErr string
```

Add the bound and the accessor near `Reload`:

```go
// cookieLoadErrorMaxLen bounds what LastLoadError will hand out. The value
// comes from os and is already short ("open /data/cookies.txt: permission
// denied"), but it reaches a status line on both dashboards and a pathological
// path must not be able to push a badge title to arbitrary length.
const cookieLoadErrorMaxLen = 200

// LastLoadError reports why the most recent Load could not read the cookie
// file, or "" when it could (or when there was no file).
//
// PATH AND ERRNO ONLY. The string is produced by os and names the path and the
// failure class; no cookie value can reach it, because the read failed before
// any byte was parsed. That is what makes it safe to project onto AuthStatus
// and render in a badge.
func (j *CookieJar) LastLoadError() string {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.lastLoadErr
}
```

In `Load`'s ENOENT arm, inside the existing `j.mu.Lock()` block that already sets `j.filePath`, add:

```go
			j.lastLoadErr = ""
```

Replace `Load`'s general error return:

```go
		// Record the path even though the read FAILED, and record why.
		//
		// The path, because Reload() short-circuits on an empty filePath: a
		// boot Load that failed with anything but ENOENT used to leave it
		// empty, so the 30-minute pass, every SyncCookies before an extraction
		// and twitch.Auth.Reload all returned nil having read nothing, for the
		// life of the process. Repairing the permission on the host did nothing
		// until a restart.
		//
		// The reason, because "the file is there and I cannot read it" is a
		// state both dashboards used to render as never-configured (see
		// AuthStatus.CookieFileError).
		//
		// The MAPS ARE LEFT ALONE. A failed read is not evidence that the
		// credentials already in memory are wrong, and clearing them would turn
		// a permission slip into an outage.
		j.mu.Lock()
		j.filePath = filePath
		j.lastLoadErr = boundString(fmt.Sprintf("%v", err), cookieLoadErrorMaxLen)
		j.mu.Unlock()
		return fmt.Errorf("failed to read cookie file: %w", err)
```

In `parseInto`'s install block (the one that already assigns `j.filePath = filePath`), add:

```go
	j.lastLoadErr = ""
```

Add the bounding helper beside `cookieLoadErrorMaxLen` (rune-safe so a multi-byte path cannot be cut mid-rune):

```go
// boundString truncates s to at most n bytes without splitting a rune, adding
// an ellipsis when it cuts.
func boundString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - 3
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
```

Add `"unicode/utf8"` to the imports.

- [ ] **Step 4: Carry the sentinel onto `AuthStatus`**

`internal/cookies/refresh_auth_status.go` — add to `AuthStatus` after `TwitchError`:

```go
	// CookieFileError is the jar's last read failure, bounded and path-only
	// (CookieJar.LastLoadError). Empty when cookies.txt loaded, and empty when
	// there is no cookies.txt — an ABSENT file is never-configured, which both
	// dashboards already say correctly. This exists for the case they used to
	// get wrong: a file that is present on the volume and unreadable, which
	// rendered identically to "you have not set cookies up".
	//
	// PLATFORM-INDEPENDENT on purpose. One file holds both platforms' rows, so
	// both status payloads project it and either badge can name it.
	CookieFileError string `json:"cookieFileError,omitempty"`
```

Add it to `authStatusChanged` — it is a badge transition the operator must see, which is exactly what that function's own doc comment says the gate is for:

```go
		next.TwitchVerification != prev.TwitchVerification ||
		next.CookieFileError != prev.CookieFileError
```

`internal/cookies/refresh_pass.go` — declare `cookieFileErr` beside the other snapshot variables and sample it in the SAME locked block as `hasYTCookies` (so every snapshot this check reasons about describes one reload):

```go
		// Sampled here with the rest of the snapshot, and AFTER doRefresh's
		// jar.Reload() at the top, so it describes the read that just happened.
		cookieFileErr = rs.jar.LastLoadError()
```

and add the field to the `rs.status = AuthStatus{…}` literal:

```go
			CookieFileError:     cookieFileErr,
```

- [ ] **Step 5: Project it onto both wire payloads**

`internal/web/routes/cookies.go` — add to BOTH `CookieStatusPayload` and `TwitchAuthStatusPayload`:

```go
		"fileError":     status.CookieFileError,
```

and extend the doc comment above `CookieStatusPayload` (the paragraph explaining that `verification` and `found` are ADDITIVE) with one sentence:

```go
// `fileError` joins them on the same additive terms, and on the same
// no-response-bodies rule: it is produced by os over a read that failed before
// any byte was parsed, so it names a path and an errno and can carry no cookie
// value. It is platform-independent — one file holds both platforms — so both
// payloads carry it and either badge can name it.
```

- [ ] **Step 6: Render it on the Web badge**

`web/public/modules/utils.js`, in `cookieIndicatorState`, insert between the `authenticated` arm and the `!status?.found` arm:

```js
  // AFTER `authenticated` and BEFORE `!found`. A cookies.txt that cannot be
  // read renders as never-configured on the `!found` arm — the container
  // operator is told "no cookies" about a file sitting on the volume — but a
  // jar that still authenticates is doing real work, and reddening that badge
  // would report a stale-reload problem as a credential problem. `fileError`
  // is path-and-errno only (see AuthStatus.CookieFileError); an older binary
  // omits the key entirely and this arm never fires, which is the additive
  // contract every other key here follows.
  if (status?.fileError) {
    return {
      className: "indicator-error",
      title: `${meta.name}: cookies.txt could not be read (${status.fileError})`,
    };
  }
```

- [ ] **Step 7: Render it on the TUI status bar**

`internal/tui/status_bar.go` — APPEND to the `CookieStatus` const block (never insert; `CookieStatusNone` is the zero value and the wiring and tests rely on it):

```go
	// CookieStatusFileUnreadable: cookies.txt is PRESENT and could not be read
	// (permission, a wrong mount). Appended for the same reason
	// CookieStatusUnknown was, and distinct from CookieStatusNone for the
	// reason this state exists at all: a file that cannot be read used to
	// render as "never configured", which sends the operator to set cookies up
	// again instead of to the permission that is actually broken.
	CookieStatusFileUnreadable
```

Add the label helper beside `cookieUnknownLabel`:

```go
// cookieFileErrorLabel is cookieUnknownLabel's sibling for the unreadable-file
// state. Abbreviates at tierTight like the others, but to the BARE code rather
// than to "YT!" — that spelling belongs to the re-login prompt, whose remedy is
// a browser login and not a permission fix, and two red alerts that render
// identically are one alert.
func cookieFileErrorLabel(code string, t barTier) string {
	if t >= tierTight {
		return code
	}
	return code + ": cookies.txt unreadable"
}
```

Add one arm to EACH of the two ladders, immediately after the `CookieStatusRelogin` arm (so it outranks the parked/CookiesOnly arm — a file that cannot be read explains every other symptom below it), and NOT gated on `healthy`:

```go
		case m.ytCookie == CookieStatusFileUnreadable:
			parts = append(parts, statusBarRedStyle.Render(cookieFileErrorLabel("YT", t)))
```

```go
		case m.twCookie == CookieStatusFileUnreadable:
			parts = append(parts, statusBarRedStyle.Render(cookieFileErrorLabel("TW", t)))
```

- [ ] **Step 8: Wire it in `cmd/moombox/tui_wiring.go`**

Extend `cookieBadgeFor` with a third boolean and one arm, and add the bullet to its ordering contract:

```go
//   - an UNREADABLE cookie file reports FILE UNREADABLE, ahead of everything
//     but `authenticated`. It explains every symptom below it, and the two
//     states it used to render as — NONE for a jar that loaded nothing, or
//     UNKNOWN for one holding stale rows — both send the operator to the wrong
//     remedy. `authenticated` still wins: a jar that is doing authenticated
//     work is not a credential problem, whatever a later reload failed to do.
func cookieBadgeFor(authenticated, hasCookies, fileUnreadable bool, verdict cookies.RefreshVerdict) tui.CookieStatus {
	switch {
	case authenticated:
		return tui.CookieStatusOK
	case fileUnreadable:
		return tui.CookieStatusFileUnreadable
	case !hasCookies:
		return tui.CookieStatusNone
	case verdict == cookies.RefreshFailed:
		return tui.CookieStatusCookiesOnly
	default:
		return tui.CookieStatusUnknown
	}
}
```

and both call sites in `authStatusToTUI`:

```go
		unreadable := auth.CookieFileError != ""
		yt := cookieBadgeFor(auth.YouTubeAuthenticated, auth.HasYouTubeCookies, unreadable, auth.YouTubeVerification)
		tw := cookieBadgeFor(auth.TwitchAuthenticated, auth.HasTwitchCookies, unreadable, auth.TwitchVerification)
```

- [ ] **Step 9: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/cookies/          # ~38-41 s
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/web/routes/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/tui/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./cmd/moombox/
cd web/tests && node --test *.test.mjs && cd ../..
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/cookies/ ./internal/web/routes/ ./internal/tui/ ./cmd/moombox/
```

Expected: all `ok`. The node suite is gated because `utils.js` changed (it is not expected to gain a case here — the Go goja harness is where this property is pinned — but a regression in the module's other exports must not slip through).

- [ ] **Step 10: Commit**

```bash
git add internal/cookies/jar.go internal/cookies/refresh_auth_status.go internal/cookies/refresh_pass.go \
        internal/web/routes/cookies.go web/public/modules/utils.js \
        internal/tui/status_bar.go cmd/moombox/tui_wiring.go \
        internal/cookies/jar_storage_test.go internal/web/routes/cookies_test.go internal/tui/status_bar_test.go
git commit -F - -- <the same paths>
```

Message:

```
fix(cookies,tui,web): a failed cookie load keeps retrying and says so on both dashboards

COOKIES-2: Load records j.filePath and the reason on a non-ENOENT read failure,
so Reload() keeps retrying and repairing the permission on the host no longer
needs a restart. The maps are left alone — a failed read is not evidence that
the credentials in memory are wrong.

COOKIES-6: AuthStatus carries a bounded, path-only CookieFileError, projected
onto both cookie status payloads as `fileError`. The Web badge names the file
and the TUI status bar gains CookieStatusFileUnreadable, a red state that
survives every tier. Both sit behind `authenticated`: a jar doing authenticated
work is not a credential problem.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
```

---

### Task 5: the POSIX 0700 parent chmod applies only to a dedicated directory

Report row #32 (COOKIES-3) + owner decision **O-K**.

On Linux `tightenCookieDirOnce` chmods the cookie file's PARENT to 0700, and in the Docker image that parent is `/data` itself — the bind mount that also holds `output/`, `staging/`, the database and the log. The first cookie write (seed import, paste, or the 30-minute rotation) therefore undoes the Dockerfile's deliberate `chmod 777 /data`, and a `./data` Docker auto-created as root becomes unreadable to the host user. POSIX files are already 0600 (`writeFileAtomic`'s chmod, `SaveMeta`), so the directory chmod buys nothing there. `config.Save` does the same thing to `/data` on every container's first settings save.

**O-K, verbatim:** *"The POSIX 0700 parent chmod applies only when the cookie file's parent is a DEDICATED directory — never the output/DB/log parent (e.g. `/data` in the image); files stay 0600; Windows icacls unchanged; `config.Save`'s twin follows the same rule."*

**"Dedicated" is defined by content**, per the ruling: the directory holds no OTHER Moombox-managed surface — no `output`/`staging`/`logs` directory, no database, no log file. Content, not config: the predicate has to work in `internal/config` (which cannot import `internal/cookies`) and in `internal/cookies` (which deliberately does not import `internal/config`), it has to work in the `moombox add` side process which wires no config store, and it must not need a new injection site in `cmd/moombox/services.go` — a file this arc only owns one hunk of.

**Files:**
- Create: `internal/utils/dedicateddir.go`
- Modify: `internal/cookies/cookie_files.go` (`tightenCookieDirOnce`)
- Modify: `internal/config/config.go` (`Save`'s `utils.ApplyUserOnlyDACL(dir)` block)
- Test: `internal/utils/dedicateddir_test.go`

> `internal/utils/**` is Arc X's file set, but Arc X is **wave 3** and runs only after every other arc has merged, so there is no concurrent writer. Arc X merges `main` first, as every arc does.

**Interfaces:**
- Consumes: nothing from Tasks 1–4.
- Produces: `utils.DirHoldsSharedData(dir string) bool` and `utils.DirTighteningAllowed(dir string) bool`. Task 10 does not use them; nothing later does.

- [ ] **Step 1: Write the failing test**

Create `internal/utils/dedicateddir_test.go`:

```go
package utils

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// mkTree builds a directory holding exactly the named entries; a name ending
// in "/" becomes a subdirectory, anything else an empty file.
func mkTree(t *testing.T, entries ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, e := range entries {
		p := filepath.Join(dir, filepath.FromSlash(e))
		if len(e) > 0 && e[len(e)-1] == '/' {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestDirHoldsSharedData is the content half of owner decision O-K. The Docker
// image's /data holds cookies.txt BESIDE output/, staging/, the database and
// the log, and chmodding it 0700 takes the operator's archives away from their
// own host user. A directory that holds nothing but Moombox's secrets is a
// different thing and still earns the hardening.
//
// Mutants:
//   - drop the directory-name check -> rows 3 and 4 answer false and /data is
//     chmodded 0700 again (the shipped bug).
//   - match only the exact default basenames (moombox.db / moombox.log) ->
//     rows 6 and 7 answer false; paths.database_path and paths.log_file_path
//     are operator-settable.
//   - drop the unreadable-directory arm -> row 9 answers false, and a
//     directory we cannot even list is assumed dedicated.
func TestDirHoldsSharedData(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []string
		want    bool
	}{
		{"empty", nil, false},
		{"secrets only", []string{"cookies.txt", "config.toml", "browser-profile/"}, false},
		{"holds output/", []string{"cookies.txt", "output/"}, true},
		{"holds staging/", []string{"cookies.txt", "staging/"}, true},
		{"holds logs/", []string{"cookies.txt", "logs/"}, true},
		{"holds the default database", []string{"cookies.txt", "moombox.db"}, true},
		{"holds a renamed database", []string{"cookies.txt", "archive.sqlite3"}, true},
		{"holds a renamed log", []string{"cookies.txt", "archiver.log"}, true},
		{"holds a WAL sidecar only", []string{"cookies.txt", "moombox.db-wal"}, true},
		{"case-insensitive directory name", []string{"cookies.txt", "Output/"}, true},
		{"a temp sibling of cookies.txt is not shared data", []string{"cookies.txt", "cookies.txt.1234.tmp"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DirHoldsSharedData(mkTree(t, tc.entries...)); got != tc.want {
				t.Errorf("DirHoldsSharedData = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("a directory that cannot be listed is not assumed dedicated", func(t *testing.T) {
		if got := DirHoldsSharedData(filepath.Join(t.TempDir(), "does-not-exist")); !got {
			t.Error("DirHoldsSharedData = false for an unlistable directory — a dir we cannot inspect must never be chmodded 0700")
		}
	})
}

// TestDirTighteningAllowedIsPOSIXOnly pins the OS half of O-K: "Windows icacls
// unchanged". The Windows tightening writes inheritable ACEs and takes nothing
// away from a sibling service that a shared POSIX 0700 does.
//
// Mutant: apply the content test on Windows too -> the first assertion fails on
// the Windows CI leg and every default install (exe dir holds output/, staging/,
// the DB and the log) silently loses its cookie-directory hardening.
func TestDirTighteningAllowedIsPOSIXOnly(t *testing.T) {
	shared := mkTree(t, "cookies.txt", "output/", "moombox.db")
	dedicated := mkTree(t, "cookies.txt")

	if !DirTighteningAllowed(dedicated) {
		t.Error("a dedicated secrets directory must still be tightened on every OS")
	}
	wantShared := runtime.GOOS == "windows"
	if got := DirTighteningAllowed(shared); got != wantShared {
		t.Errorf("DirTighteningAllowed(shared) = %v on %s, want %v — Windows icacls is unchanged by O-K; POSIX must not 0700 the data directory",
			got, runtime.GOOS, wantShared)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -run 'TestDirHoldsSharedData|TestDirTighteningAllowedIsPOSIXOnly' ./internal/utils/ -v
```

Expected: compile failure — `DirHoldsSharedData` and `DirTighteningAllowed` undefined.

- [ ] **Step 3: Write the predicate**

Create `internal/utils/dedicateddir.go`:

```go
package utils

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// dedicatedDirScanLimit bounds the listing. A directory Moombox keeps only its
// secrets in has a handful of entries; one with hundreds is, whatever else it
// is, not that — so hitting the limit disqualifies rather than truncating.
const dedicatedDirScanLimit = 512

// sharedDataDirNames are the subdirectory names that mark a directory as
// Moombox's DATA directory rather than a dedicated secrets directory. Matched
// case-insensitively because Windows and macOS filesystems are.
var sharedDataDirNames = map[string]bool{
	"output":  true,
	"staging": true,
	"logs":    true,
}

// sharedDataFileExts are the file extensions that mark the same thing. The
// database and the log are both operator-settable (paths.database_path,
// paths.log_file_path), so matching the default basenames alone would miss a
// renamed one — and a renamed database in /data is exactly as much a reason not
// to chmod /data 0700 as moombox.db is. SQLite's -wal/-shm sidecars are
// stripped before the extension is taken.
var sharedDataFileExts = map[string]bool{
	".db":      true,
	".sqlite":  true,
	".sqlite3": true,
	".log":     true,
}

// DirHoldsSharedData reports whether dir holds a Moombox surface OTHER than its
// secrets — the output tree, the staging tree, the database, or the log.
//
// It is the content test behind owner decision O-K (2026-09-17). In the Docker
// image cookies.txt lives at /data/cookies.txt, so its parent is the bind mount
// that also holds output/, staging/, the database and the log; chmodding that
// 0700 undoes the Dockerfile's deliberate `chmod 777 /data` and takes the
// operator's archives away from their own host user. A directory holding
// nothing but cookies.txt, config.toml and a browser profile is a different
// thing and still earns the hardening.
//
// CONTENT, not config, for three reasons: internal/config cannot import
// internal/cookies and internal/cookies deliberately does not import
// internal/config, so a shared config-driven predicate would need a new home
// and a new injection site; the `moombox add` side process wires no config
// store at all; and the answer has to be right for a hand-edited layout the
// running config has never seen.
//
// A directory that cannot be LISTED answers true. Erring toward "shared" means
// the worst case is a missed hardening on a host where the files are already
// 0600; erring the other way reinstates the container bug on exactly the
// deployments whose directories are the most unusual.
func DirHoldsSharedData(dir string) bool {
	f, err := os.Open(dir)
	if err != nil {
		return true
	}
	defer f.Close()

	entries, err := f.ReadDir(dedicatedDirScanLimit)
	if err != nil && len(entries) == 0 {
		return true
	}
	if len(entries) == dedicatedDirScanLimit {
		// More entries than any secrets directory has. Whatever this is, it is
		// not a directory Moombox keeps only its own secrets in.
		return true
	}
	for _, e := range entries {
		name := strings.ToLower(e.Name())
		if e.IsDir() {
			if sharedDataDirNames[name] {
				return true
			}
			continue
		}
		base := strings.TrimSuffix(strings.TrimSuffix(name, "-wal"), "-shm")
		if sharedDataFileExts[filepath.Ext(base)] {
			return true
		}
	}
	return false
}

// DirTighteningAllowed reports whether ApplyUserOnlyDACL may be applied to dir.
//
// Always true on Windows: icacls writes inheritable ACEs onto the directory and
// its children rather than removing traversal from everyone else's world, so
// the sharing problem O-K is about does not arise there, and the ruling says
// Windows is unchanged.
//
// On POSIX it is the dedicated-directory test. Files stay 0600 either way —
// every cookie write chmods its temp file before the rename, and SaveMeta
// writes 0600 — so what the directory chmod buys on POSIX is an untraversable
// parent, which is worth having on a multi-user desktop with a dedicated
// directory and actively harmful on a shared data volume.
func DirTighteningAllowed(dir string) bool {
	if runtime.GOOS == "windows" {
		return true
	}
	return !DirHoldsSharedData(dir)
}
```

- [ ] **Step 4: Gate the cookie directory's tightening**

In `internal/cookies/cookie_files.go`, at the very top of `tightenCookieDirOnce`, BEFORE the memo claim:

```go
func tightenCookieDirOnce(dir string) {
	// Owner decision O-K: on POSIX the 0700 applies only to a DEDICATED
	// directory. In the Docker image this parent is /data — the bind mount
	// holding output/, staging/, the database and the log — and the first
	// cookie write used to undo the Dockerfile's deliberate `chmod 777 /data`.
	// Windows is unchanged. Checked ahead of the memo claim rather than inside
	// the goroutine so a refused directory is never recorded as in-flight; the
	// listing costs a readdir per cookie write, which happens on the 30-minute
	// refresh cadence and on imports.
	if !utils.DirTighteningAllowed(dir) {
		return
	}

	tightenedCookieDirsMu.Lock()
```

Extend the function's existing doc comment with one sentence naming the gate, so the next reader does not look for a memo entry that will never appear for `/data`.

- [ ] **Step 5: Gate `config.Save`'s twin**

In `internal/config/config.go`'s `Save`, wrap the apply. Replace:

```go
		if daclErr := utils.ApplyUserOnlyDACL(dir); daclErr != nil {
			slog.Debug("could not restrict config dir to current user", "dir", dir, "err", daclErr)
		}
```

with:

```go
		// Owner decision O-K: on POSIX the 0700 applies only to a DEDICATED
		// directory. config.toml sits at /data/config.toml in the image, so
		// this is the same /data the cookie writer used to chmod — and a
		// settings save is the gesture most likely to be the FIRST one on a new
		// container. Windows icacls is unchanged. See utils.DirTighteningAllowed.
		if !utils.DirTighteningAllowed(dir) {
			slog.Debug("config dir also holds the output/staging/database/log surfaces — leaving its mode alone", "dir", dir)
		} else if daclErr := utils.ApplyUserOnlyDACL(dir); daclErr != nil {
			slog.Debug("could not restrict config dir to current user", "dir", dir, "err", daclErr)
		}
```

The `dacledDirs` memo is untouched: it still marks the directory before the apply, so the listing runs at most once per directory per process.

- [ ] **Step 6: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/utils/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/cookies/          # ~38-41 s
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/config/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/utils/ ./internal/cookies/ ./internal/config/
GOOS=linux GOARCH=amd64 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/utils/ ./internal/cookies/ ./internal/config/
```

Expected: all `ok`. Run the `GOOS=linux` vet here specifically — this task is the one whose behaviour differs by OS, and the POSIX arm is the one that changed.

Note for the reviewer: `internal/cookies`' existing DACL-memo tests drive `applyUserOnlyDACL` through its seam over `t.TempDir()` directories, which hold no shared-data entries, so they are unaffected on both OSes. Confirm that by running the package, not by reading.

- [ ] **Step 7: Commit**

```bash
git add internal/utils/dedicateddir.go internal/utils/dedicateddir_test.go \
        internal/cookies/cookie_files.go internal/config/config.go
git commit -F - -- <the same paths>
```

Message:

```
fix(cookies,config): the POSIX 0700 parent chmod applies only to a dedicated directory

COOKIES-3 / owner decision O-K. utils.DirTighteningAllowed answers true on
Windows (icacls unchanged) and, on POSIX, only for a directory holding no other
Moombox surface — no output/staging/logs directory, no database, no log. Both
writers consult it: the cookie directory's tightening and config.Save's twin.

In the Docker image both parents are /data, the bind mount holding output/,
staging/, the database and the log, so the first cookie write or settings save
undid the Dockerfile's deliberate chmod 777 and took the operator's archives
away from their own host user. Files stay 0600 either way.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
```

---

### Task 6: `Content-Length` before the blocking re-check

Report row #33 (COOKIES-4) + owner decision **O-L**. Measured by both reviewers with a standalone `flushprobe`: a handler that writes JSON, `Flush()`es and then sleeps 1.5 s delivers headers at 1 ms and **the body at 1.501 s**, because with no `Content-Length` net/http switches to chunked encoding and the terminating chunk is only sent when the handler returns. `fetch().json()` awaits the body, so the setup dialog's 60 s `AbortController` (`settings.js`, `setup.js`) now spans `FinishSetup` PLUS up to 45 s of re-check. With an explicit `Content-Length` the same probe completes the body at 1 ms.

The route comment (`// the Flush is what stops the client waiting / // out a re-check it has already been answered for`) and `data-and-storage.md:778` ("…nor be made to wait out a re-check it has already been answered for") are both false today.

**O-L, verbatim:** *"The cookie import/finish handlers marshal to a buffer and set `Content-Length` before writing; the deferred re-check stays blocking on the handler goroutine; the route comment and `data-and-storage.md:778` corrected."*

The gzip wrapper is safe: only `startGzip()` deletes `Content-Length`; `commitPlain()` does not, so a sub-threshold identity JSON keeps the header (verifier-confirmed).

**Files:**
- Modify: `internal/web/routes/cookies.go` (two new helpers; the import handler's and the finish handler's exits; the two deferred-recheck comments)
- Modify: `docs/spec/data-and-storage.md` (the sentence at ~`:778` ending "…nor be made to wait out a re-check it has already been answered for.")
- Test: `internal/web/routes/cookies_test.go`

**Interfaces:**
- Consumes: Task 3's `ErrRefreshInProgress` arm lives in the import handler's switch and must be converted along with its neighbours.
- Produces: `jsonResponseSized(w http.ResponseWriter, data any)` and `jsonErrorSized(w http.ResponseWriter, msg string, code int)` in `internal/web/routes/cookies.go`. Task 7 uses `jsonErrorSized` for the loopback refusal on the same handlers.

- [ ] **Step 1: Write the failing test**

Append to `internal/web/routes/cookies_test.go`:

```go
// TestCookieWritersSetContentLength is COOKIES-4 / owner decision O-L. Both
// handlers Flush and then run a <=45 s auth re-check on the handler goroutine.
// Without Content-Length net/http uses chunked encoding and the terminating
// chunk is written only when the handler RETURNS, so the Flush released the
// headers and nothing else: fetch().json() awaited the body for the whole
// re-check, inside a dialog with a 60 s abort budget that also has to cover
// FinishSetup.
//
// The property is asserted on the RECORDED HEADER rather than by timing a real
// re-check: Content-Length is exactly what makes net/http send an identity body
// it can terminate without waiting for the handler.
//
// Mutants:
//   - revert either handler to jsonResponse -> that row's Content-Length is
//     empty and Transfer-Encoding is chunked on the wire.
//   - set Content-Length on the success exit only -> the error rows fail, and
//     those are the exits the re-check matters most on (the jar-reload error
//     runs over a cookies.txt that has already been replaced).
func TestCookieWritersSetContentLength(t *testing.T) { /* … */ }
```

Write it against the harness `cookies_test.go` already uses to exercise these two routes. Drive at least four exits and assert, for each, that `rec.Header().Get("Content-Length")` equals `strconv.Itoa(rec.Body.Len())`:

1. `POST /api/cookies/import` — a successful import.
2. `POST /api/cookies/import` — a refused paste (e.g. a body that is not Netscape → 422).
3. `POST /api/cookies/auto-setup/finish` — `ErrNoSetupInProgress` → 404.
4. `POST /api/cookies/auto-setup/finish` — a successful finish.

If the existing harness cannot produce a successful finish, assert rows 1–3 and add a fourth row for the import's `ErrRefreshInProgress` 409 from Task 3 (claim the slot on the service before the request); the point is to cover a success exit, an error exit and a `jsonError` exit on each handler that has them.

- [ ] **Step 2: Run the test to verify it fails**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -run 'TestCookieWritersSetContentLength' ./internal/web/routes/ -v
```

Expected: FAIL — `Content-Length = "" want "NNN"` on every row.

- [ ] **Step 3: Add the two sized writers**

In `internal/web/routes/cookies.go`, beside `jsonErrorCause`:

```go
// jsonResponseSized and jsonErrorSized are jsonResponse / jsonError with an
// explicit Content-Length.
//
// They exist for the two handlers that run a deferred, BLOCKING auth re-check
// after answering (the cookie import and the setup-wizard finish). jsonResponse
// streams through a json.Encoder and sets no length, so net/http falls back to
// chunked encoding — and the terminating chunk is written when the HANDLER
// returns, not when the body is flushed. The Flush those handlers perform
// therefore released the headers and nothing else, and `fetch().json()`, which
// awaits the body, sat through the whole re-check: measured at 1.501 s for a
// 1.5 s stand-in, against 1 ms with the header set. The setup dialog's 60 s
// AbortController has to cover FinishSetup as well, so that was a real budget.
//
// Marshalling to a buffer first is what makes the length knowable. A marshal
// error is answered as a 500 rather than silently writing a truncated body —
// the encoder's silent `_ =` could not do that, because it had already written
// a 200 status by then.
//
// Safe under the gzip wrapper: only startGzip() deletes Content-Length, and
// these payloads are sub-threshold JSON that commitPlain() sends identity.
func jsonResponseSized(w http.ResponseWriter, data any) {
	buf, err := json.Marshal(data)
	if err != nil {
		jsonErrorSized(w, "failed to encode response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(buf)))
	_, _ = w.Write(buf)
}

func jsonErrorSized(w http.ResponseWriter, msg string, code int) {
	buf, err := json.Marshal(map[string]string{"error": msg})
	if err != nil {
		// Unreachable for a map[string]string, and handled anyway so this
		// function has no path that writes a header it then contradicts.
		buf = []byte(`{"error":"failed to encode response"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(buf)))
	w.WriteHeader(code)
	_, _ = w.Write(buf)
}
```

Add `"strconv"` to the file's imports if absent.

- [ ] **Step 4: Convert the two handlers' exits**

In `POST /api/cookies/import` and `POST /api/cookies/auto-setup/finish` ONLY, replace every `jsonResponse(rw, …)` with `jsonResponseSized(rw, …)` and every `jsonError(rw, …)` with `jsonErrorSized(rw, …)`. That is every arm of both error switches (including Task 3's new `ErrRefreshInProgress` case), the `autoCookieSvc == nil` guard on each, and each handler's final success write.

Leave `writeBrowserReadError` alone. It is shared with the refresh handler, and its two exits are reached only when `FinishSetupDetailed` failed BEFORE writing — `result.Wrote` is false there, so the deferred re-check does not run and no body is held.

Leave `readCookieImportBody`'s own error writes alone for the same reason: they answer before `ImportCookies` is called.

- [ ] **Step 5: Correct the two route comments**

In the import handler's deferred re-check comment, replace the clause `and the Flush is what stops the client waiting / out a re-check it has already been answered for` with:

```go
		// and the Content-Length the response now carries is what stops the
		// client waiting out a re-check it has already been answered for —
		// the Flush alone could not: with no length net/http chunks the body
		// and writes the terminating chunk only when the handler RETURNS, so
		// fetch().json() sat through the whole re-check (measured 1.5 s for a
		// 1.5 s stand-in, 1 ms with the header).
```

In the finish handler, replace the paragraph beginning `// The Flush is what makes "the client is not waiting on this" true.` with:

```go
		// The Content-Length is what makes "the client is not waiting on this"
		// true, and the Flush is what makes it PROMPT. Neither alone suffices:
		// jsonResponseSized sets a length so net/http can send an identity body
		// it is able to terminate without waiting for the handler, and the
		// Flush commits it now rather than at return. Before the length was
		// set, a browser on the setup dialog's 60 s AbortController waited for
		// FinishSetupDetailed PLUS up to 45 s of re-check and could abort a
		// setup that in fact succeeded. The gzip wrapper implements Flusher and
		// keeps the header for sub-threshold identity JSON;
		// /api/update/apply already relies on the same flush.
```

- [ ] **Step 6: Correct `docs/spec/data-and-storage.md`**

Find the sentence ending "…so a client that navigates away can neither cancel the fingerprint comparison its own write caused **nor be made to wait out a re-check it has already been answered for**." Replace that clause with:

```markdown
…and both answer with an explicit `Content-Length` (`jsonResponseSized` / `jsonErrorSized`, `internal/web/routes/cookies.go`) before flushing, so a client that navigates away can neither cancel the fingerprint comparison its own write caused nor be made to wait out a re-check it has already been answered for. The length is the load-bearing half and was missing until 2026-09-17 (owner decision O-L): with no `Content-Length` net/http falls back to chunked encoding and writes the terminating chunk only when the HANDLER returns, so the flush released the headers and `fetch().json()` — which awaits the body — sat through the entire re-check, inside a 60-second dialog budget that also has to cover `FinishSetup`. The re-check stays BLOCKING on the handler goroutine rather than being detached: that is the property the AST call-site test protects, and a goroutine would satisfy the test while deleting it.
```

- [ ] **Step 7: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/web/routes/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/docs/
cd web/tests && node --test *.test.mjs && cd ../..
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/web/routes/
```

Expected: all `ok`. `./internal/docs/` is gated because `data-and-storage.md` now cites `jsonResponseSized`/`jsonErrorSized` beside `internal/web/routes/cookies.go` — the citation test requires both symbols to be declared in that exact file, which Step 3 does.

- [ ] **Step 8: Commit**

```bash
git add internal/web/routes/cookies.go docs/spec/data-and-storage.md internal/web/routes/cookies_test.go
git commit -F - -- internal/web/routes/cookies.go docs/spec/data-and-storage.md internal/web/routes/cookies_test.go
```

Message:

```
fix(web): the cookie writers set Content-Length so the client is not held by the re-check

COOKIES-4 / owner decision O-L. The import and setup-finish handlers marshal to
a buffer and set Content-Length before flushing, so net/http can terminate an
identity body without waiting for the handler to return. Without it the
response was chunked and fetch().json() waited out the whole <=45 s re-check —
measured 1.5 s against 1 ms — inside a 60 s dialog budget that also covers
FinishSetup. The re-check stays blocking on the handler goroutine.

The route comments and data-and-storage.md's "nor be made to wait out a
re-check" sentence are corrected: the length is the load-bearing half.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
```

---

### Task 7: the auto-setup trio is loopback-gated, and `platform` is validated

The spec's **"Auto-setup gate"** owner decision, plus report row #71 (COOKIES-8). Both land on the same handler, so they ship together.

**Auto-setup gate, verbatim:** *"`/api/cookies/auto-setup/{start,finish,cancel}` and the Settings 'Set up' buttons are gated to loopback (the setup wizard's gate shape); remote viewers see 'run this on the host'."*

The web report's evidence: on a `lan` install (no auth in that mode) any LAN device can pop a headed browser window on the host, bounded only by `apiRL`. The ONLY thing between a remote click and that window today is `reloginPromptTarget`'s client-side `atTheHost` predicate — whose own doc comment says so in as many words: *"the cookie setup trio is not [gated], so this predicate is the only thing between a remote click and a browser window on someone else's screen."* That sentence becomes false in this task and must be corrected.

**#71:** `POST /api/cookies/auto-setup/start` forwards any `platform` string and `StartSetup("anything")` proceeds with the YouTube login URL and `targetPlatform="anything"`, so the Chromium finish skips `cdpEnsurePageTarget` and the wizard judges both platforms as if `youtube` had been asked.

**Two deviations from the literal wording, both deliberate:**

1. **403, not the wizard's 401.** `/api/setup/complete` answers `http.StatusUnauthorized`, but `web/public/app.js` installs a global `window.fetch` interceptor that treats ANY 401 outside `/api/auth/` as an expired session and calls `window.location.reload()`. A 401 here would reload the dashboard out from under the operator. The gate's SHAPE is the wizard's — an inline `web.IsLoopbackRequest(req)` check answering through `jsonError` before anything else runs — and only the status differs, matching `web.LoopbackOnly`'s 403.
2. **`setup.js` (the first-run wizard's own cookie step) is not edited.** It is Arc W's file, and the server gate covers it: a remote viewer of an unconfigured instance now gets the same refusal, which is consistent with the protected "the setup wizard stays loopback-gated" rule rather than a new restriction.

**Files:**
- Modify: `internal/web/routes/cookies.go` (the three `/auto-setup/*` handlers)
- Modify: `internal/cookies/autocookies_setup.go` (`StartSetup` validates `platform`)
- Modify: `internal/cookies/errors.go` (`ErrUnsupportedPlatform`)
- Modify: `web/public/modules/utils.js` (extract `viewerIsAtTheHost`; correct `reloginPromptTarget`'s doc)
- Modify: `web/public/modules/settings.js` (the two Set-up button handlers)
- Test: `internal/web/routes/cookies_test.go`, `internal/cookies/autocookies_setup_test.go`

> `web/public/modules/utils.js` is nominally Arc W's file this wave. No Arc W row touches `reloginPromptTarget` or `cookieIndicatorState`; Task 4 already edits the latter. Merge `main` before the merge candidate, as every wave-2 arc does.

**Interfaces:**
- Consumes: `jsonErrorSized` from Task 6 (the finish handler's exits are all sized; the loopback refusal there uses the same writer so one handler does not mix the two).
- Produces: `cookies.ErrUnsupportedPlatform`; `viewerIsAtTheHost(hostname)` exported from `web/public/modules/utils.js`.

- [ ] **Step 1: Write the failing tests**

`internal/web/routes/cookies_test.go` — append:

```go
// TestAutoSetupTrioIsLoopbackGated is the owner's "Auto-setup gate" decision.
// These three endpoints START, FINISH and CANCEL a HEADED BROWSER WINDOW ON
// THE HOST. On a network_access=lan install there is no auth, so any LAN
// device could open one on a screen it cannot see — and the only thing
// stopping that was a client-side predicate in utils.js.
//
// 403, never 401: app.js's global fetch interceptor treats a 401 outside
// /api/auth/ as an expired session and reloads the page.
//
// Mutants:
//   - drop the gate from any one of the three -> that row answers 2xx/4xx-other
//     from a LAN address.
//   - answer 401 -> the status assertion fails and a remote click would reload
//     the dashboard instead of explaining itself.
//   - gate on IsLocalOrPrivateRequest instead of IsLoopbackRequest -> the LAN
//     row passes the gate, which is the exact population this refuses.
func TestAutoSetupTrioIsLoopbackGated(t *testing.T) {
	for _, path := range []string{
		"/api/cookies/auto-setup/start",
		"/api/cookies/auto-setup/finish",
		"/api/cookies/auto-setup/cancel",
	} {
		t.Run(path, func(t *testing.T) {
			// … build the request with RemoteAddr "192.168.1.20:5555" …
			// want: 403, and the error text contains "on the host"
			// then the same request from "127.0.0.1:5555" must NOT be 403
		})
	}
}

// TestAutoSetupStartRejectsAnUnknownPlatform is COOKIES-8. The handler
// forwarded any string and StartSetup proceeded with the YouTube login URL
// under targetPlatform="anything", so the Chromium finish skipped
// cdpEnsurePageTarget and the wizard judged both platforms as if youtube had
// been asked — a wrong input silently accepted.
//
// Mutants:
//   - drop the handler check -> "mastodon" answers 200.
//   - drop the StartSetup check -> the service-level subtest fails, and every
//     non-HTTP caller (the TUI's R L, the first-run wizard) keeps the hole.
//   - reject "" -> the default-to-youtube row fails; an absent field is not a
//     wrong one, and both the dashboard and the wizard omit it.
func TestAutoSetupStartRejectsAnUnknownPlatform(t *testing.T) { /* … */ }
```

Fill both in against the harness `cookies_test.go` already uses (grep `RemoteAddr` there for the shape the package uses to fake a client IP; `internal/web`'s `ExtractIP` reads `r.RemoteAddr` unless a trusted proxy is configured). For the platform test assert: `{"platform":"mastodon"}` → 400 whose error names the two accepted values; `{"platform":""}` and a body with no `platform` key → NOT 400.

`internal/cookies/autocookies_setup_test.go` — append:

```go
// TestStartSetupRejectsAnUnknownPlatform is COOKIES-8 at the service boundary,
// which is where the non-HTTP callers live (the TUI's R L chord and the
// first-run wizard both call StartSetup directly).
//
// Mutants:
//   - drop the check -> err is nil and the service claims the setup slot for a
//     platform no finish branch handles.
//   - check AFTER the slot claim -> the slot is taken and released for an
//     input that was never going to work; the assertion on SetupInProgress
//     after the call catches a claim that leaks.
func TestStartSetupRejectsAnUnknownPlatform(t *testing.T) {
	s := NewAutoCookieService(t.TempDir(), filepath.Join(t.TempDir(), "cookies.txt"), NewCookieJar(), nopAutoCookieLogger{})
	for _, p := range []string{"mastodon", "YouTube", "youtube ", "../youtube"} {
		if err := s.StartSetup(p); !errors.Is(err, ErrUnsupportedPlatform) {
			t.Errorf("StartSetup(%q) err = %v, want ErrUnsupportedPlatform — an unknown platform is driven with the YouTube login URL and judged as YouTube", p, err)
		}
	}
	s.mu.Lock()
	claimed := s.setupClaimed
	s.mu.Unlock()
	if claimed {
		t.Error("the setup slot was claimed for a rejected platform")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -run 'TestAutoSetupTrioIsLoopbackGated|TestAutoSetupStartRejectsAnUnknownPlatform' ./internal/web/routes/ -v
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -run 'TestStartSetupRejectsAnUnknownPlatform' ./internal/cookies/ -v
```

Expected: the trio test FAILS (no 403 from a LAN address); the platform tests fail to compile (`ErrUnsupportedPlatform` undefined) or fail on the status.

- [ ] **Step 3: Add the sentinel and validate in `StartSetup`**

`internal/cookies/errors.go`, beside `ErrSetupInProgress`:

```go
	// ErrUnsupportedPlatform is returned by StartSetup for anything that is
	// not "youtube" or "twitch" (empty means "youtube" and is not an error —
	// it is what both the dashboard and the first-run wizard send when the
	// field is omitted). Before this, an unknown value was accepted: the login
	// URL fell through to YouTube's while targetPlatform kept the wrong
	// string, so the Chromium finish skipped cdpEnsurePageTarget and the
	// wizard judged both platforms as if youtube had been asked. HTTP
	// consumers map to 400.
	ErrUnsupportedPlatform = errors.New("platform must be youtube or twitch")
```

`internal/cookies/autocookies_setup.go` — validate FIRST, before `s.mu.Lock()`, so a rejected input never touches the slot. Replace the existing default-to-youtube assignment (`if platform == "" { platform = "youtube" }`, currently inside the second locked section) by hoisting it:

```go
func (s *AutoCookieService) StartSetup(platform string) error {
	// Validated BEFORE the lock and before the claim: a wrong value must never
	// take the setup slot, and the two callers that are not the HTTP route —
	// the TUI's R L chord and the first-run wizard — reach this and nothing
	// else. The empty string is not an error; it is what a caller that omits
	// the field sends, and it has always meant YouTube.
	if platform == "" {
		platform = "youtube"
	}
	if platform != "youtube" && platform != "twitch" {
		return fmt.Errorf("%w (got %q)", ErrUnsupportedPlatform, platform)
	}

	s.mu.Lock()
```

and delete the later `if platform == "" { platform = "youtube" }` line that sat beside `s.targetPlatform = platform`.

The error wraps the value. That is safe: `platform` arrives from a JSON field the operator controls, not from a credential, and the two dashboards already render sentinel text verbatim.

- [ ] **Step 4: Gate the three handlers and validate on the route**

`internal/web/routes/cookies.go` — add the shared refusal beside `jsonErrorSized`:

```go
// requireLoopbackForBrowserSetup refuses an /auto-setup/* request that did not
// come from the host, and reports whether it did so.
//
// These three endpoints START, FINISH and CANCEL A HEADED BROWSER WINDOW ON THE
// HOST'S SCREEN. On a network_access=lan install there is no authentication at
// all, so before this any LAN device could open one on a screen its user cannot
// see, bounded only by the API rate limiter — and the only thing standing in
// the way was reloginPromptTarget, a CLIENT-side predicate in utils.js whose
// own comment said so. The remedy that works from anywhere is the paste import
// (POST /api/cookies/import), which stays ungated by the Arc 11 ruling for
// exactly the deployment this one refuses.
//
// The SHAPE is the first-run wizard's gate (an inline web.IsLoopbackRequest
// answered before anything else runs). The STATUS is 403 and deliberately not
// the wizard's 401: app.js installs a global window.fetch interceptor that
// treats any 401 outside /api/auth/ as an expired session and reloads the page,
// so a 401 here would throw the operator out of the dashboard instead of
// telling them where to click.
//
// IsLoopbackRequest, not IsLocalOrPrivateRequest: a private LAN address is
// precisely the population this refuses. An operator at the machine over
// RDP/VNC is still loopback.
func requireLoopbackForBrowserSetup(rw http.ResponseWriter, req *http.Request) bool {
	if web.IsLoopbackRequest(req) {
		return true
	}
	jsonErrorSized(rw, "browser cookie setup opens a window on the host, so run this on the host — "+
		"from anywhere else, paste a cookies.txt with Import instead", http.StatusForbidden)
	return false
}
```

Add it as the FIRST statement of each of the three handlers, ahead of the `autoCookieSvc == nil` guard:

```go
		if !requireLoopbackForBrowserSetup(rw, req) {
			return
		}
```

In the start handler's error switch, add the 400 arm:

```go
			// A wrong INPUT, not a server fault and not a condition that
			// clears: 400 with the sentinel's own sentence, which names the two
			// accepted values.
			case errors.Is(err, cookies.ErrUnsupportedPlatform):
				jsonErrorSized(rw, err.Error(), http.StatusBadRequest)
```

Use `jsonErrorSized` for the start and cancel handlers' exits too if they still use `jsonError` — one handler must not mix the two writers, or a reader cannot tell which exits carry a length. (The finish handler was converted in Task 6.)

- [ ] **Step 5: Extract the client-side predicate and correct its doc**

`web/public/modules/utils.js` — lift the `atTheHost` expression out of `reloginPromptTarget` into an exported helper, and have `reloginPromptTarget` call it:

```js
/**
 * Is the viewer sitting AT the host, as far as the page can tell?
 *
 * A strict SUBSET of what the server's isLoopback accepts
 * (internal/web/middleware.go: net.ParseIP(ip).IsLoopback() plus the literal
 * "localhost", so all of 127.0.0.0/8 and every spelling of ::1). The four below
 * are the ones a browser actually puts in location.hostname; anything else a
 * local viewer might have typed — 127.0.0.2, 127.1, foo.localhost — misses, and
 * a miss errs toward "you are remote", which costs that viewer one extra step
 * in a panel that holds both controls. Widen it only in that direction.
 *
 * ADVISORY ONLY. The server refuses the cookie setup trio from anywhere but
 * loopback (requireLoopbackForBrowserSetup, internal/web/routes/cookies.go);
 * this exists so the UI can say so BEFORE the click rather than surfacing a 403
 * afterwards.
 *
 * @param {string} hostname - location.hostname of the page making the request
 * @returns {boolean}
 */
export function viewerIsAtTheHost(hostname) {
  return hostname === "localhost" || hostname === "127.0.0.1" ||
    hostname === "::1" || hostname === "[::1]";
}
```

In `reloginPromptTarget`, replace the inline expression with `const atTheHost = viewerIsAtTheHost(hostname);` and **correct the now-false sentence** in its doc comment. Replace:

> *"Nothing on the server stops them: /api/setup/complete (the FIRST-RUN wizard) is loopback-gated, but the cookie setup trio is not, so this predicate is the only thing between a remote click and a browser window on someone else's screen. It is the same shape as the server's IsLoopbackRequest deliberately, so the two read 'local' the same way."*

with:

> *"Since 2026-09-17 the server refuses them too — the cookie setup trio is loopback-gated alongside /api/setup/complete — so this predicate is no longer the only thing between a remote click and a browser window on someone else's screen. It is still the same shape as the server's IsLoopbackRequest deliberately, so the two read 'local' the same way and the UI routes the viewer BEFORE the 403 rather than after it."*

- [ ] **Step 6: The two Set-up buttons refuse off-host**

`web/public/modules/settings.js` — import `viewerIsAtTheHost` (add it to the existing `from "./utils.js"` list, alphabetically) and add the guard at the top of `startAutoCookieSetup`, after the `if (!platform) return;` line:

```js
    // The browser this opens appears ON THE HOST. A remote viewer who clicks
    // would see nothing happen and have no way to learn that a login window is
    // waiting on a screen they cannot see — so say it here rather than let the
    // server's 403 arrive as a bare failure. The server refuses it either way
    // (requireLoopbackForBrowserSetup); this is the explanation, not the
    // enforcement. The import panel is the remedy that works from anywhere.
    if (!viewerIsAtTheHost(window.location.hostname)) {
      const resultEl = document.getElementById("auto-cookie-setup-result");
      if (resultEl) {
        resultEl.textContent = "Browser login opens a window on the host, so run this on the host. " +
          "From here, use Import cookies.txt instead.";
        resultEl.style.color = "var(--sl-color-danger-600)";
      }
      return;
    }
```

Leave the two `addEventListener` registrations at `settings.js`'s cookie-button block as they are — the guard belongs in the one function both of them call, not duplicated per button.

- [ ] **Step 7: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/web/routes/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/cookies/          # ~38-41 s
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./cmd/moombox/
cd web/tests && node --test *.test.mjs && cd ../..
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/web/routes/ ./internal/cookies/
```

Expected: all `ok`. `cmd/moombox` is gated because the TUI's `R L` wiring calls `StartSetup` and now has a new error to render (it renders sentinel text verbatim, so no change is needed — confirm by running, not by reading).

- [ ] **Step 8: Commit**

```bash
git add internal/web/routes/cookies.go internal/cookies/autocookies_setup.go internal/cookies/errors.go \
        web/public/modules/utils.js web/public/modules/settings.js \
        internal/web/routes/cookies_test.go internal/cookies/autocookies_setup_test.go
git commit -F - -- <the same paths>
```

Message:

```
fix(web,cookies): the auto-setup trio is loopback-gated and validates its platform

Owner decision "Auto-setup gate". /api/cookies/auto-setup/{start,finish,cancel}
open, finish and cancel a headed browser window ON THE HOST, and on a
network_access=lan install there is no auth — so any LAN device could pop one on
a screen it cannot see. The gate takes the first-run wizard's shape and answers
403, not 401: app.js's global fetch interceptor reloads the page on a 401. The
Settings Set-up buttons say "run this on the host" before the click, and
reloginPromptTarget's now-false "the trio is not gated" sentence is corrected.

COOKIES-8: StartSetup rejects any platform but youtube/twitch before claiming
the setup slot (empty still means youtube); the route answers 400.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
```

---

### Task 8: `launchWindowKillBudget` becomes a `var` seam

Report row #47's cookies half (TOOL-5) + owner decision **O-Q**.

`internal/cookies` is the suite's wall-time floor (solo: 41.4 s measured by the verifier), and ~13.5 s of it is **seven** tests waiting out the hard-coded 2 s `launchWindowKillBudget`. The verifier measured the fix in an export: setting the constant to 20 ms took the package from 41.4 s to 26.9 s.

**O-Q, verbatim:** *"`launchWindowKillBudget` becomes a package `var` seam (tests set ~20 ms); the engine fast-delays seam is exposed across the package boundary for the worker interruption test; production values unchanged."* (The engine half is Arc E's.)

**The verifier's two refinements override the row**, and both matter:
1. It is **7** tests, not 8, and ~13.5 s of the ~14.5 s saving: `TestCancelSetupReportsNothingToCancel` (2.45→0.48), `TestCancelSetupAgreesWithSetupInProgress` (2.44→0.47), `TestCancelDuringStartSetupPreparationIsHonoured` (2.03→0.06), `TestStartSetupClaimConsumesAPendingCancel` (2.01→0.05), `TestKillSetupProcessCannotBlockOnALauncherThatNeverPublishes` (2.01→0.05), `TestCleanupDoesNotEraseTheCancelFlag` (2.01→0.05), `TestRefreshCookiesDetailedCallersAreEnumerated` (1.77→0.09).
2. **`TestCancelCatchesABrowserPublishedInsideTheLaunchWindow` FAILS at 20 ms** and is the eighth test this task has to handle. It deliberately publishes a process `3 * killProcessTreePollDelay` (150 ms) after the kill starts; with a 20 ms budget `killSetupProcess` gives up on its first poll and the test asserts nothing. It keeps the production value, with a comment saying why.

`t.Parallel` is NOT added anywhere in this package as part of this task: the seam is a package-level `var` that tests mutate, and parallel tests sharing it would race. The census (zero `t.Parallel` across all 78 files) stays as it is.

**Files:**
- Modify: `internal/cookies/autocookies.go` (`launchWindowKillBudget` const → var)
- Modify: `internal/cookies/autocookies_setup_reap_test.go` (the helper; two tests)
- Modify: `internal/cookies/autocookies_lifecycle_test.go` (five tests)
- Modify: `internal/cookies/autocookies_autoimport_sites_test.go` (one test)

**Interfaces:**
- Consumes: nothing from Tasks 1–7.
- Produces: `withLaunchWindowKillBudget(t *testing.T, d time.Duration)` — a test helper in `internal/cookies`. Nothing after this uses it.

- [ ] **Step 1: Write the failing test**

Append to `internal/cookies/autocookies_setup_reap_test.go`, beside `captureKills`:

```go
// withLaunchWindowKillBudget shortens the launch-window poll cap for one test
// and restores it afterwards.
//
// The budget is a PRODUCTION value that seven tests otherwise wait out in full
// — ~13.5 s of this package's ~41 s, which is the whole suite's wall-time floor
// (owner decision O-Q, 2026-09-17). It is a package-level var, so tests that
// set it must not run in parallel with each other; this package has no
// t.Parallel anywhere and must not gain one while this seam exists.
func withLaunchWindowKillBudget(t *testing.T, d time.Duration) {
	t.Helper()
	prev := launchWindowKillBudget
	t.Cleanup(func() { launchWindowKillBudget = prev })
	launchWindowKillBudget = d
}

// testLaunchWindowKillBudget is what the seven tests set. Comfortably above
// killProcessTreePollDelay's own granularity so a single poll still happens,
// and small enough that waiting it out costs nothing.
const testLaunchWindowKillBudget = 20 * time.Millisecond

// TestLaunchWindowKillBudgetIsASeam pins the seam itself: it must be settable
// and it must be RESTORED, or one test's 20 ms silently disarms the launch-
// window poll for every test that runs after it — including
// TestCancelCatchesABrowserPublishedInsideTheLaunchWindow, which depends on the
// production value and would start failing for a reason no diff explains.
//
// Mutants:
//   - turn the var back into a const -> this does not compile.
//   - drop t.Cleanup from withLaunchWindowKillBudget -> the post-subtest
//     assertion fails.
func TestLaunchWindowKillBudgetIsASeam(t *testing.T) {
	before := launchWindowKillBudget
	t.Run("shortened", func(t *testing.T) {
		withLaunchWindowKillBudget(t, testLaunchWindowKillBudget)
		if launchWindowKillBudget != testLaunchWindowKillBudget {
			t.Fatalf("launchWindowKillBudget = %s, want %s", launchWindowKillBudget, testLaunchWindowKillBudget)
		}
	})
	if launchWindowKillBudget != before {
		t.Fatalf("the budget was not restored: %s, want %s — every later test would silently run with a disarmed launch window",
			launchWindowKillBudget, before)
	}
	if before != 2*time.Second {
		t.Fatalf("the PRODUCTION budget is %s, want 2s — O-Q changes the declaration, not the value", before)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -run 'TestLaunchWindowKillBudgetIsASeam' ./internal/cookies/ -v
```

Expected: compile failure — `cannot assign to launchWindowKillBudget (neither addressable nor a map index expression)`.

- [ ] **Step 3: Record the baseline wall time**

Before changing anything else, record the number the reviewer will check against:

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/cookies/
```

Write the reported wall time into the task's review note. The verifier measured 41.4 s solo; anything in that region is the baseline.

- [ ] **Step 4: Turn the constant into a var**

In `internal/cookies/autocookies.go`, move `launchWindowKillBudget` out of the `const (…)` block into its own `var` declaration immediately after it, carrying its doc comment and adding the reason:

```go
// launchWindowKillBudget caps how long a kill will wait for a launcher to
// publish the process it is about to start. Both slots have the same
// window — the claim is taken (setupClaimed / the refreshCmd sentinel)
// before the real process exists — so both killers poll for it, and both
// give up rather than let Stop() block on a launcher that errored before
// it ever assigned. See killSetupProcess and killRefreshProcess.
//
// A var rather than a const SOLELY so tests can shorten it (owner decision
// O-Q, 2026-09-17): seven of them wait this out in full, ~13.5 s of the ~41 s
// that makes this package the whole suite's wall-time floor. Nothing in
// production writes it, and the value is unchanged. Because it is package
// state that tests mutate, this package must stay free of t.Parallel — see
// withLaunchWindowKillBudget.
var launchWindowKillBudget = 2 * time.Second
```

- [ ] **Step 5: Shorten the seven tests**

Add `withLaunchWindowKillBudget(t, testLaunchWindowKillBudget)` as the FIRST statement of each of these seven, immediately after any `t.Helper()`/`captureKills` setup line that must run first:

| Test | File |
|---|---|
| `TestCancelSetupReportsNothingToCancel` | `internal/cookies/autocookies_lifecycle_test.go` |
| `TestCancelSetupAgreesWithSetupInProgress` | `internal/cookies/autocookies_lifecycle_test.go` |
| `TestCancelDuringStartSetupPreparationIsHonoured` | `internal/cookies/autocookies_lifecycle_test.go` |
| `TestStartSetupClaimConsumesAPendingCancel` | `internal/cookies/autocookies_lifecycle_test.go` |
| `TestCleanupDoesNotEraseTheCancelFlag` | `internal/cookies/autocookies_lifecycle_test.go` |
| `TestKillSetupProcessCannotBlockOnALauncherThatNeverPublishes` | `internal/cookies/autocookies_setup_reap_test.go` |
| `TestRefreshCookiesDetailedCallersAreEnumerated` | `internal/cookies/autocookies_autoimport_sites_test.go` |

`TestKillSetupProcessCannotBlockOnALauncherThatNeverPublishes` keeps BOTH of its assertions and they keep their meaning — they are written against `launchWindowKillBudget` rather than against a literal — but its second subtest's upper bound (`launchWindowKillBudget + 2*time.Second`) is now dominated by the slack rather than by the budget. Leave the bound alone; it is a ceiling on `Stop()`, not a measurement, and shrinking it would make the test flaky on a loaded CI box.

- [ ] **Step 6: Pin the eighth test at the production value**

In `TestCancelCatchesABrowserPublishedInsideTheLaunchWindow`, add a comment above its body (it takes NO `withLaunchWindowKillBudget` call):

```go
	// DELIBERATELY at the production budget. This test publishes the process
	// 3 * killProcessTreePollDelay (150 ms) after the kill starts, which is the
	// whole point — it proves the kill catches a browser the launcher publishes
	// a few poll intervals late. Under the 20 ms the seven fast tests use,
	// killSetupProcess gives up on its first poll and the assertion below
	// passes over a kill that never had anything to catch. If this test ever
	// needs to be fast, scale its own publish delay with the seam rather than
	// shortening the seam under it.
```

- [ ] **Step 7: Run the tests and record the new wall time**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/cookies/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestCancelCatchesABrowserPublishedInsideTheLaunchWindow' ./internal/cookies/ -v
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/cookies/
```

Expected: `ok` at roughly **26–28 s** (from ~41 s), and the eighth test still PASSES on its own — run it separately as well as in the package, because a seam that leaks would only show up in one of those two orders.

Also run the package a second time to catch ordering damage from a leaked seam:

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=2 ./internal/cookies/
```

- [ ] **Step 8: Commit**

```bash
git add internal/cookies/autocookies.go internal/cookies/autocookies_setup_reap_test.go \
        internal/cookies/autocookies_lifecycle_test.go internal/cookies/autocookies_autoimport_sites_test.go
git commit -F - -- <the same paths>
```

Message:

```
test(cookies): launchWindowKillBudget becomes a var seam (~41 s -> ~27 s)

Owner decision O-Q / TOOL-5. Seven tests waited out the hard-coded 2 s launch
window in full — ~13.5 s of the package that is the suite's wall-time floor.
The value is unchanged in production; only the declaration is.

TestCancelCatchesABrowserPublishedInsideTheLaunchWindow deliberately keeps the
production budget: it publishes a process 150 ms late, which a 20 ms budget
would let the kill give up on before there was anything to catch.

No t.Parallel is added: the seam is package state that tests mutate.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
```

---

### Task 9: notification targets dedupe by resolved URL, and the date probe's "anonymous" claim is corrected

Report rows #66 (MON-6) and #69 (MON-10). Row #67 (MON-7, the rare duplicate Discord post) is **NO CODE CHANGE** by owner decision **O-T** — "accept the rare duplicate post; retries stay on every transport error" — and is deliberately absent from this task.

**#66.** `buildTargets` does not dedupe by resolved webhook URL, so a config listing the same webhook twice — `discord://ID/TOKEN` and its `https://discord.com/api/webhooks/ID/TOKEN` form, or a hand-edited duplicate — posts every embed twice. `parseTarget` already normalises both spellings to the same `DiscordWebhook.URL`, so the key exists; nothing consults it.

**#69 — read this before implementing: the row's premise is FALSE, and the fix is a correction, not a new probe.** The row says "the section-9 date fetch is anonymous (WebSafari, no cookies) even for `membership`-source rows". It is not. `PlayerAPI.ProbeVideoDate` calls `fetchWithClient`, whose first act is `headers := p.auth.GenerateAPIHeaders(client, ytcfg)` — and that function sets `Cookie` from `a.jar.GetCookieHeader()`, the `SAPISIDHASH` `Authorization` header, `X-Origin` and `X-Youtube-Bootstrap-Logged-In` whenever the jar holds YouTube auth. (Contrast `ProbeVideoStatus`, which routes through `fetchWithCookielessClient` and genuinely sends none.) MON-10 is an L-impact row that no verifier reproduced — its own evidence column says "reasoned" — and what it actually found is **three doc comments that claim the call is anonymous when the code has always sent cookies**.

So: **no new API, no new wiring, no behaviour change.** Pin the property the row wanted (a `membership`-source row's date fetch is authenticated), and delete the false sentences. If the pin test FAILS, the premise was right after all and the implementer must stop and report rather than building the fix this task does not describe.

**Files:**
- Modify: `internal/notifications/manager.go` (`buildTargets`)
- Modify: `internal/youtube/player_api_strategy.go` (`ProbeVideoDate`'s doc comment)
- Modify: `internal/youtube/service.go` (`Service.ProbeVideoDate`'s doc comment)
- Modify: `internal/monitor/walk.go` (`probeRowDated`'s doc comment)
- Modify: `internal/monitor/decapi.go` (the §9 two-phase-probe comment inside `processResponse`)
- Test: `internal/notifications/manager_test.go`, `internal/youtube/player_api_strategy_test.go`

> `internal/youtube/**` is Arc Y's file set, but Arc Y is **wave 1** and has merged before this branch is cut, so there is no concurrent writer. The edits here are one test and two comments. If Arc Y's O-R change (skip the PLAYER PO token on probe-only WEB-family calls) has reshaped `ProbeVideoDate`, keep the doc correction and adapt the test to whatever path the function now takes — the assertion is about the `Cookie`/`Authorization` headers, which O-R does not touch.

**Interfaces:**
- Consumes: nothing from Tasks 1–8.
- Produces: nothing other tasks use.

- [ ] **Step 1: Write the failing tests**

`internal/notifications/manager_test.go` — append:

```go
// TestBuildTargetsDedupesByResolvedURL is MON-6. parseTarget already
// normalises discord://ID/TOKEN and the full https:// form to the same
// DiscordWebhook.URL, so a config carrying both spellings — or a hand-edited
// duplicate — built two targets over one webhook and posted every embed twice.
//
// The filters UNION, and a nil filter wins: nil means "every event", so a
// target listed once unfiltered and once filtered must keep the wider
// subscription the operator configured. Narrowing instead would silently drop
// alerts the config asked for.
//
// Mutants:
//   - drop the dedupe -> row 1 builds 2 targets and every embed posts twice.
//   - key on the CONFIGURED url instead of the resolved one -> row 1 builds 2
//     (the two spellings differ) while row 3 still builds 1, so only the
//     literal-duplicate case is fixed.
//   - intersect the filters instead of unioning -> row 2's merged target no
//     longer matches "found".
//   - let a filtered entry narrow a nil one -> row 4 fails.
func TestBuildTargetsDedupesByResolvedURL(t *testing.T) {
	const id, tok = "123456789012345678", "abcdefghijklmnopqrstuvwxyz0123456789-_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	full := "https://discord.com/api/webhooks/" + id + "/" + tok
	short := "discord://" + id + "/" + tok

	for _, tc := range []struct {
		name        string
		notifs      []config.NotificationConfig
		wantTargets int
		wantMatches map[string]bool // event -> the single merged target must match it
	}{
		{
			"the two spellings of one webhook",
			[]config.NotificationConfig{{URL: full}, {URL: short}},
			1, map[string]bool{"found": true, "error": true},
		},
		{
			"filters union",
			[]config.NotificationConfig{
				{URL: full, Events: []string{"found"}},
				{URL: short, Events: []string{"error"}},
			},
			1, map[string]bool{"found": true, "error": true},
		},
		{
			"a literal duplicate",
			[]config.NotificationConfig{{URL: full}, {URL: full}},
			1, map[string]bool{"found": true},
		},
		{
			"a nil filter wins over a narrow one, in either order",
			[]config.NotificationConfig{
				{URL: full, Events: []string{"found"}},
				{URL: short},
			},
			1, map[string]bool{"found": true, "error": true},
		},
		{
			"two genuinely different webhooks stay two",
			[]config.NotificationConfig{{URL: full}, {URL: "https://discord.com/api/webhooks/987654321098765432/" + tok}},
			2, nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.MoomboxConfig{Notifications: tc.notifs}
			targets := buildTargets(cfg, silentNotifyLogger{})
			if len(targets) != tc.wantTargets {
				t.Fatalf("built %d targets, want %d — a webhook listed twice receives every embed twice", len(targets), tc.wantTargets)
			}
			for event, want := range tc.wantMatches {
				got := targets[0].events == nil || targets[0].events[event]
				if got != want {
					t.Errorf("merged target matches %q = %v, want %v", event, got, want)
				}
			}
		})
	}
}
```

> **Implementer note:** `silentNotifyLogger` is a placeholder for whatever discard logger `internal/notifications`' tests already use (grep `buildTargets(` and `NewManager(` in `manager_test.go`). `config.NotificationConfig`'s field names must be taken from `internal/config/types.go`, not guessed.

`internal/youtube/player_api_strategy_test.go` — append:

```go
// TestProbeVideoDateSendsTheJarsCredentials pins what report row MON-10
// assumed was missing. The row claimed the section-9 date fetch is "anonymous
// (WebSafari, no cookies)" even for membership-source rows, and proposed adding
// an authenticated variant. It is not anonymous: ProbeVideoDate goes through
// fetchWithClient, whose headers come from Auth.GenerateAPIHeaders, which sets
// Cookie from the jar and the SAPISIDHASH Authorization header whenever the jar
// holds YouTube auth. The row found a DOC bug, not a behaviour bug, and this
// test is what stops the doc drifting back.
//
// Mutants:
//   - route ProbeVideoDate through fetchWithCookielessClient (the shape MON-10
//     assumed was already in place) -> both header assertions fail.
//   - drop the jar's cookie header from GenerateAPIHeaders -> the Cookie
//     assertion fails and every members-only date fetch goes out anonymous, the
//     state the row described.
func TestProbeVideoDateSendsTheJarsCredentials(t *testing.T) {
	const videoID = "test1234567"
	var gotCookie, gotAuthz string
	tr := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotCookie = req.Header.Get("Cookie")
		gotAuthz = req.Header.Get("Authorization")
		body := `{"playabilityStatus":{"status":"OK"},` +
			`"videoDetails":{"videoId":"` + videoID + `","title":"t","author":"a"},` +
			`"microformat":{"playerMicroformatRenderer":{"publishDate":"2026-07-14"}}}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	})
	orig := apiClient
	apiClient = &http.Client{Transport: tr}
	t.Cleanup(func() { apiClient = orig })

	jar := cookies.NewCookieJar()
	path := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(path, []byte(netscapeWithYouTubeAuth), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}

	api := NewPlayerAPI(NewAuth(jar, noopLogger{}), noopLogger{})
	if _, _, err := api.ProbeVideoDate(context.Background(), videoID, "vd"); err != nil {
		t.Fatalf("ProbeVideoDate: %v", err)
	}
	if gotCookie == "" {
		t.Error("the date probe sent no Cookie header — a members-only VOD's date fetch would go out anonymous")
	}
	if !strings.HasPrefix(gotAuthz, "SAPISIDHASH ") {
		t.Errorf("Authorization = %q, want a SAPISIDHASH — the probe is authenticated by construction", gotAuthz)
	}
}
```

> **Implementer note:** `roundTripFunc` and `netscapeWithYouTubeAuth` are placeholders. The package already swaps `apiClient` for a stub transport in `TestTryCookielessFallbacks` (`clientKeyedTransport`) — reuse that type if it can capture headers, otherwise add a two-line `roundTripFunc`. For the Netscape fixture, write the minimum the jar accepts as YouTube auth: tab-separated rows on `.youtube.com` for `SAPISID` and `LOGIN_INFO` (check `essentialYouTubeCookies` and `GenerateAuthorizationHeader`'s requirements in `internal/cookies`).

- [ ] **Step 2: Run the tests to verify they behave as expected**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -run 'TestBuildTargetsDedupesByResolvedURL' ./internal/notifications/ -v
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -run 'TestProbeVideoDateSendsTheJarsCredentials' ./internal/youtube/ -v
```

Expected: the notifications test FAILS (`built 2 targets, want 1`). **The youtube test is expected to PASS immediately** — it is a characterisation pin, not a red test, because the behaviour MON-10 asked for already exists. If it FAILS, STOP: the row's premise was right, this task does not describe the fix, and the controller must rule before anything else happens.

- [ ] **Step 3: Dedupe the notification targets**

In `internal/notifications/manager.go`'s `buildTargets`, keep everything up to and including the event-filter construction, then replace the trailing `targets = append(...)` with:

```go
		// Dedupe on the RESOLVED webhook URL, not the configured string:
		// parseTarget normalises discord://ID/TOKEN and the full https:// form
		// to the same DiscordWebhook.URL, so the two spellings of one webhook
		// are one destination and used to receive every embed twice.
		key := ""
		if d, ok := s.(*DiscordWebhook); ok {
			key = d.URL
		}
		if key != "" {
			if idx, dup := seen[key]; dup {
				// Redacted, like every other line here: the URL is the secret.
				logger.Warn("notification target is listed more than once — merged with the earlier entry",
					"url", redactURLForLog(url))
				// UNION, with a nil filter winning outright. nil means "every
				// event", so a webhook listed once unfiltered and once filtered
				// keeps the wider subscription the operator configured;
				// narrowing it would silently drop alerts the config asked for.
				switch {
				case targets[idx].events == nil || events == nil:
					targets[idx].events = nil
				default:
					for e := range events {
						targets[idx].events[e] = true
					}
				}
				continue
			}
			seen[key] = len(targets)
		}

		targets = append(targets, notificationTarget{
			sender: s,
			events: events,
		})
```

and declare the index beside `targets` at the top of the function:

```go
	var targets []notificationTarget
	// resolved webhook URL -> index into targets. Discord is the only sender
	// today, so every built target has a key; a future sender without one
	// simply never dedupes rather than colliding on "".
	seen := make(map[string]int, len(cfg.Notifications))
```

- [ ] **Step 4: Correct the four false "anonymous" comments**

`internal/youtube/player_api_strategy.go` — in `ProbeVideoDate`'s doc comment, replace the opening sentence *"ProbeVideoDate fetches ONLY a video's publish date via one anonymous WEB-family player call."* with:

```go
// ProbeVideoDate fetches ONLY a video's publish date via one WEB-family player
// call, carrying whatever credentials the jar holds.
//
// NOT anonymous, and the distinction is load-bearing for members-only content:
// this goes through fetchWithClient, so Auth.GenerateAPIHeaders attaches the
// jar's Cookie header and the SAPISIDHASH Authorization whenever YouTube auth
// is configured. (ProbeVideoStatus is the anonymous one — it routes through
// fetchWithCookielessClient.) The comment here claimed "anonymous" until
// 2026-09-17 and sent a sweep looking for a members-only date-fetch bug that
// does not exist; TestProbeVideoDateSendsTheJarsCredentials pins it.
```

Keep the rest of that comment (the microformat/WEB-client explanation and the ""/"" contract) unchanged.

`internal/youtube/service.go` — in `Service.ProbeVideoDate`'s doc comment, replace *"fetches only a video's publish date via one anonymous WEB-family player call"* with *"fetches only a video's publish date via one WEB-family player call, carrying whatever credentials the jar holds"*.

`internal/monitor/walk.go` — in `probeRowDated`'s doc comment, replace *"one ProbeDate call (an anonymous WEB player fetch) supplies it"* with:

```go
// one ProbeDate call (a WEB player fetch that carries the jar's credentials —
// see youtube.PlayerAPI.ProbeVideoDate; only the STATUS probes are anonymous)
// supplies it
```

`internal/monitor/decapi.go` — in `processResponse`'s §9 two-phase-probe comment, replace *"One WEB date fetch decides the window honestly"* with *"One WEB date fetch — authenticated when the jar holds credentials — decides the window honestly"*.

- [ ] **Step 5: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/notifications/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/youtube/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/monitor/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/notifications/ ./internal/youtube/ ./internal/monitor/
```

Expected: all `ok`. No `./internal/docs/` gate — no `docs/spec/*.md` changed and no symbol moved.

- [ ] **Step 6: Commit**

```bash
git add internal/notifications/manager.go internal/notifications/manager_test.go \
        internal/youtube/player_api_strategy.go internal/youtube/service.go \
        internal/youtube/player_api_strategy_test.go \
        internal/monitor/walk.go internal/monitor/decapi.go
git commit -F - -- <the same paths>
```

Message:

```
fix(notifications): dedupe targets by resolved webhook URL; correct the date probe's docs

MON-6: buildTargets keys on the resolved DiscordWebhook.URL, so the two
spellings of one webhook (discord://ID/TOKEN and the full https:// form) build
one target instead of two and stop posting every embed twice. Event filters
union, and a nil filter wins.

MON-10: the row's premise is false. ProbeVideoDate goes through fetchWithClient,
so GenerateAPIHeaders already attaches the jar's Cookie and SAPISIDHASH headers
— only the STATUS probes are anonymous. Four doc comments claimed otherwise;
they are corrected and the property is pinned. No behaviour change.

MON-7 is NO CHANGE by owner decision O-T.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
```

---

### Task 10: delete `UpdateJobSync`, and add `cookies.dpapi_profile_dir`

Report rows #97 (MON-8) and #96 (COOKIES-7). Two cleanups, one commit, because both are small and neither has a neighbour in this arc.

**#97.** `UpdateJobSync` and its shared body `updateJobExec` have **no non-test caller** (independently grepped across `internal`, `cmd`, `web`, `tools`), and the UPDATE list omits `channel_id`, `queue_priority`, `incomplete_tail`, `park_reason` and `park_identity` — a future caller would silently drop five columns, including the two that carry the whole backlog-pacing contract. **This is a dead-code deletion, not a perf change**: the protected "DB layer untouched for perf" ruling is not in play, and nothing about write cadence, durability or subscriber dispatch is altered.

**#96.** The DPAPI fallback discovers profiles only under `%LOCALAPPDATA%`'s eleven fixed `User Data` layouts, so a portable Chromium, a `--user-data-dir`-relocated profile, or Opera (excluded by layout shape) is invisible — and the pass answers "no Chromium-family profiles found under LOCALAPPDATA" even when `browser_path` names the very binary. The configured path carries no profile location and nothing lets the operator supply one.

**`cookies.dpapi_profile_dir` is the ONLY new config key in this entire chain.** It is read **LIVE** through an injected closure, exactly as `cookies.acquisition` is — not snapshotted at construction like `dpapi_fallback` — precisely so it does NOT become restart-required and therefore does NOT need an entry in either restart-required list (`restartRequiredKeys` in `internal/tui/settings.go` and `RESTART_REQUIRED_FIELDS` in `web/public/modules/settings.js`, pinned against each other by `TestRestartRequiredListsAgree`). Both of those lists belong to other wave-2 arcs; the live shape avoids touching them, and it is the better design anyway, since the directory is consulted once per pass.

**Files:**
- Modify: `internal/database/database.go` (delete `updateJobExec`), `internal/database/database_jobs.go` (delete `UpdateJobSync`), `internal/database/database_test.go` (delete `TestUpdateJobSync`)
- Modify: `internal/config/types.go` (`CookiesConfig.DpapiProfileDir`), `internal/config/config.go` (validation)
- Modify: `internal/cookies/autocookies.go` (`DpapiProfileDir func() string` field), `internal/cookies/autocookies_dpapi.go` (explicit dir takes precedence), `internal/cookies/autocookies_refresh.go` (pass it through)
- Modify: `internal/cookies/dpapi/profiles.go` (`ValidateProfileDir`)
- Modify: `cmd/moombox/services.go` (one closure beside `AcquisitionMode`)
- Modify: `config.example.toml`, `docs/spec/data-and-storage.md` (the cookies config table)
- Test: `internal/cookies/dpapi/profiles_test.go`, `internal/cookies/autocookies_dpapi_test.go`, `internal/config/config_test.go`

**Interfaces:**
- Consumes: nothing from Tasks 1–9.
- Produces: `dpapi.ValidateProfileDir(dir string) error`; `cookies.AutoCookieService.DpapiProfileDir func() string`; `config.CookiesConfig.DpapiProfileDir string`; `dpapiExtractAsNetscape(logger, configuredBrowserType, explicitProfileDir string)` (a third parameter). Nothing after this uses them.

- [ ] **Step 1: Write the failing tests**

`internal/database/database_test.go`: DELETE `TestUpdateJobSync` (lines ~870-912). Its subject is being removed; leaving it would not compile. This is the one "test change" in the task that is a deletion rather than an addition — the coverage it provided is already carried by `TestFieldToColumnCoverage` and the `UpdateJobFields` tests, which cover the path every caller actually uses.

`internal/cookies/dpapi/profiles_test.go` — append (this file has no build tag, so it runs on Linux CI too):

```go
// TestValidateProfileDir is COOKIES-7's guard. An operator-supplied
// dpapi_profile_dir must LOOK like a Chromium profile directory before the
// fallback trusts it, because pointing the DPAPI reader at the wrong directory
// produces "no cookies came out" with no hint about which of several causes it
// was. The two structural facts are the ones ReadChromeCookies needs: the
// encrypted master key lives in `Local State` ONE LEVEL UP (in User Data), and
// the cookie store is `Cookies` in the profile dir or `Network/Cookies` beside
// it on newer Chromium.
//
// Mutants:
//   - drop the Local State check -> row 3 validates and the read later fails
//     on a master key it cannot find.
//   - accept only `Cookies` -> row 4 fails, and every post-M96 Chromium (which
//     moved the store into Network/) is refused.
//   - skip the "is a directory" test -> row 5 validates a FILE.
func TestValidateProfileDir(t *testing.T) {
	// helper: build <root>/User Data/{Local State,<profile>/…}
	mk := func(t *testing.T, cookiesAt string, withLocalState bool) string {
		t.Helper()
		userData := filepath.Join(t.TempDir(), "User Data")
		profile := filepath.Join(userData, "Default")
		if err := os.MkdirAll(profile, 0o755); err != nil {
			t.Fatal(err)
		}
		if withLocalState {
			if err := os.WriteFile(filepath.Join(userData, "Local State"), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if cookiesAt != "" {
			p := filepath.Join(profile, filepath.FromSlash(cookiesAt))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return profile
	}

	t.Run("Cookies in the profile dir", func(t *testing.T) {
		if err := ValidateProfileDir(mk(t, "Cookies", true)); err != nil {
			t.Errorf("ValidateProfileDir = %v, want nil", err)
		}
	})
	t.Run("Network/Cookies", func(t *testing.T) {
		if err := ValidateProfileDir(mk(t, "Network/Cookies", true)); err != nil {
			t.Errorf("ValidateProfileDir = %v, want nil", err)
		}
	})
	t.Run("no Local State one level up", func(t *testing.T) {
		if err := ValidateProfileDir(mk(t, "Cookies", false)); err == nil {
			t.Error("ValidateProfileDir = nil — without Local State the DPAPI reader has no master key to decrypt with")
		}
	})
	t.Run("no cookie store", func(t *testing.T) {
		if err := ValidateProfileDir(mk(t, "", true)); err == nil {
			t.Error("ValidateProfileDir = nil for a profile holding neither Cookies nor Network/Cookies")
		}
	})
	t.Run("a file, not a directory", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "notadir")
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := ValidateProfileDir(p); err == nil {
			t.Error("ValidateProfileDir = nil for a regular file")
		}
	})
	t.Run("absent", func(t *testing.T) {
		if err := ValidateProfileDir(filepath.Join(t.TempDir(), "nope")); err == nil {
			t.Error("ValidateProfileDir = nil for a directory that does not exist")
		}
	})
}
```

`internal/cookies/autocookies_dpapi_test.go` — append:

```go
// TestDpapiExplicitProfileDirTakesPrecedence is COOKIES-7's routing half. The
// discovery walk knows eleven fixed %LOCALAPPDATA% layouts, so a portable
// Chromium, a --user-data-dir profile or Opera is invisible to it — and the
// pass answered "no Chromium-family profiles found under LOCALAPPDATA" even
// when browser_path named the binary. An explicit directory must SKIP the walk
// entirely, not be appended to it: the operator naming a directory is a
// stronger statement than a scoring pass over whatever else is installed.
//
// Mutants:
//   - append instead of replacing -> the discovery seam is still called.
//   - skip ValidateProfileDir -> the bad-directory subtest's error no longer
//     names the directory, and the operator gets "no cookies came out".
func TestDpapiExplicitProfileDirTakesPrecedence(t *testing.T) { /* … */ }
```

Write it against this file's existing seams: swap `dpapiFindBrowserProfiles` for one that records whether it was called and returns a profile, swap `dpapiReadChromeCookiesStats` for one that records the path it was handed, and force the Windows arm through the package's `runtimeGOOS` seam. Assert (a) with an explicit valid dir, `dpapiFindBrowserProfiles` is NOT called and the read receives exactly that dir; (b) with an explicit INVALID dir, the call returns an error naming the directory and still does not fall back to discovery; (c) with an empty explicit dir, discovery runs exactly as before.

`internal/config/config_test.go` — append:

```go
// TestValidateRejectsATraversingDpapiProfileDir pins the one validation rule
// the new key carries. Existence is deliberately NOT checked: a container's
// config is written before the volume is mounted, and a Validate that refused a
// not-yet-mounted path would fail the save that configures it. What IS checked
// is the shape — a path with ".." segments is either a mistake or an attempt to
// walk out of wherever the operator meant, and the DPAPI reader opens whatever
// it is given.
//
// Mutants:
//   - drop the rule -> row 2 reports no error.
//   - reject every non-absolute path -> row 3 fails; Chromium's own
//     --user-data-dir accepts relative paths and so must this.
func TestValidateRejectsATraversingDpapiProfileDir(t *testing.T) { /* … */ }
```

Write it with the package's existing `Validate` test shape (grep `cookies.acquisition %q must be one of` in `config_test.go` for the reportOnly/fail harness). Rows: `""` → no error; `"../../etc"` → an error naming `cookies.dpapi_profile_dir`; `"profiles/Default"` → no error; an absolute path → no error.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -run 'TestValidateProfileDir' ./internal/cookies/dpapi/ -v
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -run 'TestDpapiExplicitProfileDirTakesPrecedence' ./internal/cookies/ -v
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -run 'TestValidateRejectsATraversingDpapiProfileDir' ./internal/config/ -v
```

Expected: compile failures — `ValidateProfileDir` undefined, `dpapiExtractAsNetscape` takes 2 args, `DpapiProfileDir` undefined.

- [ ] **Step 3: Delete `UpdateJobSync` and `updateJobExec`**

Delete the whole `UpdateJobSync` function from `internal/database/database_jobs.go` (its doc comment included) and the whole `updateJobExec` function from `internal/database/database.go`. Do NOT touch the `executor` interface — `insertJobExec` still uses it — and do not touch anything else in either file.

- [ ] **Step 4: Add the config key**

`internal/config/types.go` — add to `CookiesConfig`, after `DpapiFallback`:

```go
	// DpapiProfileDir names a Chromium-family PROFILE directory for the DPAPI
	// fallback to read, taking precedence over discovery.
	//
	// Discovery walks eleven fixed %LOCALAPPDATA% "User Data" layouts, so a
	// portable Chromium, a --user-data-dir-relocated profile, and Opera (whose
	// layout shape is deliberately excluded) are all invisible to it — and the
	// pass then answers "no Chromium-family profiles found under LOCALAPPDATA"
	// even when cookies.browser_path names the very binary. The configured
	// path carries no profile location, so nothing let the operator supply one.
	//
	// The PROFILE dir, not the User Data root: "…/User Data/Default", not
	// "…/User Data". dpapi.ValidateProfileDir checks the two structural facts
	// the reader needs — `Local State` one level up, and `Cookies` or
	// `Network/Cookies` inside.
	//
	// Windows-only in effect, like dpapi_fallback itself, and read LIVE through
	// AutoCookieService.DpapiProfileDir — so it is NOT restart-required and is
	// deliberately absent from both restart-required lists.
	DpapiProfileDir string `toml:"dpapi_profile_dir,omitempty" json:"dpapi_profile_dir,omitempty"`
```

`internal/config/config.go` — add to `Validate`, immediately after the `cookies.acquisition` switch:

```go
	// Existence is NOT checked. A container's config.toml is written before
	// the volume that holds the profile is mounted, and a Validate that
	// refused a not-yet-present path would fail the save that configures it —
	// the same reason browser_profile_dir is not checked here either. What is
	// checked is the SHAPE: a traversing path is a mistake or an attempt to
	// walk out of wherever the operator meant, and the DPAPI reader opens
	// whatever it is handed. Relative paths stay legal (Chromium's own
	// --user-data-dir accepts them).
	if cfg.Cookies.DpapiProfileDir != "" {
		cleaned := filepath.Clean(cfg.Cookies.DpapiProfileDir)
		if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) ||
			strings.HasPrefix(cleaned, "../") {
			fail("cookies.dpapi_profile_dir %q must not traverse above its own root", cfg.Cookies.DpapiProfileDir)
			if !reportOnly {
				cfg.Cookies.DpapiProfileDir = ""
			}
		}
	}
```

- [ ] **Step 5: Validate a profile directory in `internal/cookies/dpapi`**

Append to `internal/cookies/dpapi/profiles.go` (no build tag — the shape test must run on both CI legs):

```go
// ValidateProfileDir reports whether dir looks like a Chromium-family PROFILE
// directory the DPAPI reader can work with.
//
// Two structural facts, both of which ReadChromeCookies needs:
//
//   - `Local State` ONE LEVEL UP. That is where Chromium keeps the
//     DPAPI-protected master key; a profile dir without it has no key to
//     decrypt the cookie values with, and the read would fail late with
//     "nothing came out".
//   - a cookie store INSIDE: `Cookies` on older Chromium, `Network/Cookies`
//     since the network-service move. Either satisfies it.
//
// The error names the directory and which fact was missing, because the whole
// point of the setting is an operator pointing at a directory by hand and
// needing to know when they pointed at the wrong one — the User Data root
// instead of the profile inside it being the likely mistake.
func ValidateProfileDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("cookies.dpapi_profile_dir %q: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("cookies.dpapi_profile_dir %q is not a directory", dir)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "Local State")); err != nil {
		return fmt.Errorf("cookies.dpapi_profile_dir %q has no \"Local State\" beside its parent — "+
			"name the PROFILE directory (…/User Data/Default), not the User Data root", dir)
	}
	for _, rel := range []string{"Cookies", filepath.Join("Network", "Cookies")} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err == nil {
			return nil
		}
	}
	return fmt.Errorf("cookies.dpapi_profile_dir %q holds neither \"Cookies\" nor \"Network/Cookies\"", dir)
}
```

Add `"fmt"`, `"os"` and `"path/filepath"` to that file's imports (it currently imports only `"strings"`).

- [ ] **Step 6: Let the explicit directory win**

`internal/cookies/autocookies.go` — add the injected reader beside `AcquisitionMode`:

```go
	// DpapiProfileDir returns cookies.dpapi_profile_dir from the ACTIVE config,
	// or "" when the operator set none. Injected by cmd/moombox and read LIVE,
	// the same shape as AcquisitionMode above and for the same reason: this
	// package cannot import config, and a value snapshotted at construction
	// would make the setting restart-required with nothing in either UI saying
	// so. Nil is treated as "".
	DpapiProfileDir func() string
```

`internal/cookies/autocookies_dpapi.go` — take the directory as a third parameter and short-circuit discovery:

```go
func dpapiExtractAsNetscape(logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}, configuredBrowserType, explicitProfileDir string) (string, error) {
	if runtimeGOOS() != "windows" {
		// … unchanged …
	}

	// An operator-named profile directory REPLACES discovery rather than
	// joining it. Discovery knows eleven fixed %LOCALAPPDATA% layouts, so a
	// portable Chromium, a --user-data-dir profile and Opera are invisible to
	// it; naming a directory is a stronger statement than a scoring pass over
	// whatever else happens to be installed, and scoring the named one against
	// discovered siblings could silently pick a different profile than the one
	// asked for.
	//
	// A directory that does not validate is an ERROR, not a fall-back to
	// discovery: falling back would answer "no profiles found under
	// LOCALAPPDATA" about a setting the operator had just written, which is
	// the exact confusion this setting exists to remove.
	if explicitProfileDir != "" {
		if err := dpapiValidateProfileDir(explicitProfileDir); err != nil {
			return "", fmt.Errorf("DPAPI fallback: %w", err)
		}
		if logger != nil {
			logger.Debug("DPAPI fallback: using the configured profile directory instead of discovery",
				"dir", explicitProfileDir)
		}
		return dpapiReadOneProfile(logger, dpapi.BrowserProfile{
			Browser:   configuredBrowserType,
			Name:      filepath.Base(explicitProfileDir),
			Path:      explicitProfileDir,
			IsDefault: filepath.Base(explicitProfileDir) == "Default",
		})
	}

	allProfiles := dpapiFindBrowserProfiles()
	// … unchanged from here …
```

Add the seam beside the two that already exist at the top of the file:

```go
	dpapiValidateProfileDir = dpapi.ValidateProfileDir
```

`dpapiReadOneProfile` is the single-candidate tail of the existing scoring loop. Extract it verbatim from the current body rather than writing a second one — the read, the per-reason failure counting and the "no relevant cookies" verdict must stay byte-identical between the two entry points, and duplicating them is how they drift. If the existing loop cannot be split cleanly, instead build a one-element `profiles` slice from the explicit directory and let the unchanged loop run over it; that is equally acceptable and strictly smaller. **Pick one and say which in the commit message.**

`internal/cookies/autocookies_refresh.go` — pass it at the single call site:

```go
		explicitProfileDir := ""
		if s.DpapiProfileDir != nil {
			explicitProfileDir = s.DpapiProfileDir()
		}
		fallbackCookies, fallbackErr := dpapiExtractAsNetscape(s.logger, cfgBrowserType, explicitProfileDir)
```

`cmd/moombox/services.go` — add the closure immediately after the `autoCookieSvc.AcquisitionMode = func() string {…}` block:

```go
	// cookies.dpapi_profile_dir, read LIVE. Same shape and same reason as
	// AcquisitionMode above: a snapshot here would make the setting
	// restart-required with nothing in either UI saying so, and the directory
	// is consulted once per pass, so there is nothing to cache.
	autoCookieSvc.DpapiProfileDir = func() string {
		var dir string
		s.configStore.Read(func(c *config.MoomboxConfig) {
			dir = c.Cookies.DpapiProfileDir
		})
		return dir
	}
```

- [ ] **Step 7: Document the key**

`config.example.toml` — after the `acquisition` block:

```toml
# dpapi_profile_dir names a Chromium-family PROFILE directory for the
# Windows DPAPI fallback to read, taking precedence over the automatic
# discovery walk. Set it when your profile is somewhere the walk cannot
# see: a portable Chromium, a --user-data-dir install, or Opera.
#
# Name the PROFILE directory, not the User Data root — Moombox checks for
# "Local State" one level up and for "Cookies" (or "Network/Cookies")
# inside, and refuses with a message naming what was missing.
#
# Windows-only in effect, like dpapi_fallback. Read live — no restart
# needed.
# dpapi_profile_dir = 'C:\PortableChrome\User Data\Default'
```

`docs/spec/data-and-storage.md` — add a row to the cookies config table, immediately after the `DpapiFallback` row:

```markdown
| DpapiProfileDir | string | "" | `dpapi_profile_dir` | Windows-only, and empty by default. Names a Chromium-family PROFILE directory for the DPAPI fallback to read, REPLACING the discovery walk rather than joining it — discovery knows eleven fixed `%LOCALAPPDATA%` `User Data` layouts, so a portable Chromium, a `--user-data-dir` profile and Opera are invisible to it. `dpapi.ValidateProfileDir` (`internal/cookies/dpapi/profiles.go`) checks the two structural facts the reader needs (`Local State` one level up; `Cookies` or `Network/Cookies` inside) and a directory that fails them is an error rather than a fall-back to discovery — falling back would answer "no profiles found under LOCALAPPDATA" about a setting the operator had just written. **Not** restart-required: `AutoCookieService.DpapiProfileDir` reads it live, the same shape as `acquisition`. `Validate` checks the path's shape only (no `..` traversal), never its existence — a container writes its config before the volume is mounted. |
```

- [ ] **Step 8: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/database/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/cookies/ ./internal/cookies/dpapi/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/config/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/web/routes/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/tui/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./cmd/moombox/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/docs/
cd web/tests && node --test *.test.mjs && cd ../..
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./...
GOOS=linux GOARCH=amd64 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./...
```

Expected: all `ok`. The gates are wide here for two reasons: deleting a Go symbol requires `./internal/docs/` (the citation test), and a new config field must not break the settings round-trips in `internal/web/routes`, `internal/tui`, `internal/webtest` or the node settings tests. `go vet ./...` on both GOOSes is warranted because `internal/cookies/dpapi` now has an OS-independent function in a package whose other halves are build-tagged.

Confirm by running, not by reading, that `TestRestartRequiredListsAgree` still passes — the new key must appear in NEITHER list.

- [ ] **Step 9: Commit**

```bash
git add internal/database/database.go internal/database/database_jobs.go internal/database/database_test.go \
        internal/config/types.go internal/config/config.go internal/config/config_test.go \
        internal/cookies/autocookies.go internal/cookies/autocookies_dpapi.go internal/cookies/autocookies_refresh.go \
        internal/cookies/autocookies_dpapi_test.go \
        internal/cookies/dpapi/profiles.go internal/cookies/dpapi/profiles_test.go \
        cmd/moombox/services.go config.example.toml docs/spec/data-and-storage.md
git commit -F - -- <the same paths>
```

Message (state which of the two `dpapiReadOneProfile` shapes you took):

```
chore(database,cookies): delete the callerless UpdateJobSync; add cookies.dpapi_profile_dir

MON-8: UpdateJobSync and updateJobExec had no non-test caller and their UPDATE
list omitted channel_id, queue_priority, incomplete_tail, park_reason and
park_identity. Dead-code deletion, not a perf change — nothing about write
cadence, durability or subscriber dispatch is touched.

COOKIES-7: cookies.dpapi_profile_dir names a Chromium profile directory for the
DPAPI fallback, replacing the eleven-layout %LOCALAPPDATA% discovery walk that
cannot see a portable Chromium, a --user-data-dir profile or Opera.
dpapi.ValidateProfileDir checks Local State one level up and Cookies /
Network/Cookies inside; a directory that fails is an error, not a fall-back.
Read live through AutoCookieService.DpapiProfileDir, so it is not
restart-required and appears in neither restart-required list.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
```

---

### Task 11: COOKIES-5's two stale doc sentences, and the plan deletes itself

Report row #102's COOKIES-5. Both docs still claim `dangerousProfilePathSubstrings` "is Windows-only by construction and is deliberately not widened with forward-slash variants … [which] would newly refuse Linux desktop users", while the list has carried the Linux dotfile, snap and flatpak shapes and the macOS `Library` shapes since 2026-09-08. `security.md`'s copy **contradicts itself inside one paragraph** — the same paragraph says the guard was inert on Linux "before 2026-09-08".

The list as it stands: 18 Windows `%LocalAppData%`/`%AppData%` shapes, 15 Linux `~/.mozilla` / `~/.config` / dotfile shapes (which snap's `~/snap/firefox/common` tree matches through the same `~/.mozilla` path), 6 flatpak `~/.var/app` sandboxes, and 7 macOS `~/Library/Application Support` shapes. All matched case-insensitively on the absolute path with separators normalised to `/`.

**Files:**
- Modify: `docs/spec/security.md` (the last sentence of "The launch boundary." paragraph, ~`:539`)
- Modify: `docs/spec/data-and-storage.md` (the last sentence of the launch-args paragraph, ~`:984`)
- Delete: `docs/superpowers/plans/2026-09-17-sweep2-m-monitor-cookies.md` (this file)

**Interfaces:**
- Consumes: nothing.
- Produces: nothing.

- [ ] **Step 1: Verify the claim before rewriting it**

There is no unit test for a doc sentence; the check is the reading, and it must be done rather than assumed:

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go doc -all ./internal/cookies 2>/dev/null | head -1   # builds the package
grep -n 'library/application support\|/\.var/app/\|/\.config/chromium/' internal/cookies/autocookies_browser_resolve.go
grep -rn 'Windows-only by construction' docs/spec/
```

Expected: the first grep prints macOS, flatpak and Linux entries; the second prints exactly the two sentences this task removes. If the second prints anything else, fix that occurrence too and say so in the commit message.

- [ ] **Step 2: Rewrite the `security.md` sentence**

In `docs/spec/security.md`, in the **"The launch boundary."** paragraph, DELETE the final sentence:

> *"`dangerousProfilePathSubstrings` is Windows-only by construction and is deliberately not widened with forward-slash variants: doing so would newly refuse Linux desktop users already pointing at a real profile, and the launch guard is the only place such variants would ever belong."*

and put in its place:

```markdown
`dangerousProfilePathSubstrings` covers all three OS trees and has since 2026-09-08 — 18 Windows `%LocalAppData%`/`%AppData%` shapes, the Linux `~/.mozilla`, `~/.config` and dotfile shapes (snap's `~/snap/firefox/common` tree matches through the same `~/.mozilla` path), the six flatpak `~/.var/app` sandboxes, and the macOS `~/Library/Application Support` shapes. The paragraph above says the same thing, and until 2026-09-17 this sentence contradicted it by still claiming the list was Windows-only and deliberately un-widened. A Linux desktop operator who genuinely wants Moombox to read their real profile is not refused outright: the READ boundary below lifts on `cookies.acquisition = "profile"`, which is the opt-in that case has always needed. What stays refused, on every OS, is LAUNCHING a browser against it.
```

- [ ] **Step 3: Rewrite the `data-and-storage.md` sentence**

In `docs/spec/data-and-storage.md`, in the paragraph that ends the **Launch args by engine** table's discussion, DELETE the final sentence:

> *"The list itself is Windows-only by construction and is deliberately NOT widened: forward-slash variants would newly refuse Linux desktop users already pointing at a real profile (audit G3)."*

and put in its place:

```markdown
The list covers Windows, Linux (dotfile, snap and flatpak trees) and macOS, and has since 2026-09-08; the "Windows-only by construction, deliberately NOT widened" sentence that stood here until 2026-09-17 described the pre-2026-09-08 list and had been false for over a week. The Linux desktop case the old sentence worried about is answered by the read boundary rather than by a narrow list: `cookies.acquisition = "profile"` lifts the guard for the two READ-ONLY sites, while launching a browser against a real profile stays refused on every OS.
```

- [ ] **Step 4: Run the docs gate**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/docs/
```

Expected: `ok`. The citation test requires every backticked symbol paired with a path citation to be declared in that file — `dangerousProfilePathSubstrings` and `cookies.acquisition` both already appear in these paragraphs with their existing citations, and neither rewrite introduces a new symbol.

- [ ] **Step 5: Commit the doc fix**

```bash
git add docs/spec/security.md docs/spec/data-and-storage.md
git commit -F - -- docs/spec/security.md docs/spec/data-and-storage.md
```

Message:

```
docs: dangerousProfilePathSubstrings is not Windows-only

COOKIES-5. Both docs still claimed the list "is Windows-only by construction
and is deliberately not widened", which has been false since 2026-09-08 — it
carries the Linux dotfile/snap/flatpak shapes and the macOS Library shapes —
and security.md's copy contradicted the paragraph it sat in. Both now state the
coverage and name the opt-in a Linux desktop needs for the read-only sites,
cookies.acquisition = "profile".

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
```

- [ ] **Step 6: Delete this plan**

The plan is implemented and verified; git history is the archive.

```bash
git rm docs/superpowers/plans/2026-09-17-sweep2-m-monitor-cookies.md
git commit -F - -- docs/superpowers/plans/2026-09-17-sweep2-m-monitor-cookies.md
```

Message:

```
chore(plans): delete the Arc M plan — implemented

Every task is merged into the branch and its gates are green. The design
authority (docs/superpowers/specs/2026-09-17-sweep2-fix-chain-design.md) and
git history carry everything this file said.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
```

- [ ] **Step 7: Hand the branch to the controller**

Do NOT merge, push or tag. Report to the controller:

- the eleven task commits, in order;
- the `internal/cookies` wall time before and after Task 8 (the O-Q number the owner asked for);
- anything a step's "Implementer note" made you choose differently from what this plan wrote, with the reason;
- whether the Task 9 youtube pin test passed on first run (it should — if it failed, that is a finding the controller must rule on).

The controller then runs the merge-candidate gates listed at the top of this plan.

---

## Residuals recorded, deliberately not fixed here

Each is inside Arc M's area and is left alone on purpose. They belong in the arc-close report, not in a task.

- **Row #67 / MON-7** — a Discord POST retried after a post-write transport error can post twice. **No code change, owner decision O-T:** "accept the rare duplicate post; retries stay on every transport error". A lost alert is worse than a duplicated one and Discord webhooks carry no idempotency key.
- **The setup-wizard finish is a third `cookies.txt` writer** with its own read → write gap, gated by the SETUP slot rather than the refresh slot Task 3 claims. O-D was scoped to the reproduced refresh collision; widening the import's gate would refuse a container operator's only re-authentication route for the 60 s grace a stale setup slot lingers. Stated in `data-and-storage.md` by Task 3.
- **`database_subscribers.go`'s dispatch ordering race** — two bulk writes within microseconds can deliver the older snapshot last. Inside the protected DB layer; the report records it for the owner and does not propose it, and Task 10's deletion goes nowhere near it.
- **Task 3's claim makes `Stop()`'s `killRefreshProcess` poll for `launchWindowKillBudget`** during an import before giving up — a bounded shutdown delay that already existed for a refresh caught inside its launch window, now reachable one more way. Named in the code comment.
- **`utils.DirHoldsSharedData` is a content heuristic.** A deployment that renames the output tree to something other than `output`/`staging`/`logs` AND keeps its database under an extension outside `.db`/`.sqlite`/`.sqlite3` AND its log outside `.log` would read as dedicated. The failure mode is the pre-O-K behaviour on an unusual layout, and the ruling's own definition is content-based; a config-driven predicate would need an injection site in files this arc does not own.

## Self-review

Run against the spec's Arc M row and the report rows it names.

**Spec coverage.** Every row in Arc M's list has a task: #17 → T1; #18 → T3; #19 → T4; #30 → T1; #31 → T2; #32 → T5; #33 → T6; #47 (cookies half) → T8; #64 → T2; #65 → T2; #66 → T9; #68 → T2; #69 → T9; #70 → T4; #71 → T7; #96 → T10; #97 → T10; #102's COOKIES-5 → T11; the auto-setup loopback gate → T7. Owner decisions: O-D → T3, O-J → T2, O-K → T5, O-L → T6, O-Q → T8, O-T → T9 (no change, recorded), "Auto-setup gate" → T7. #67 is explicitly not implemented, per O-T.

**Files against the spec's declared set.** Four files are touched that Arc M's list does not name, each for a reason stated in its task: `internal/utils/dedicateddir.go` (T5 — the O-K predicate needs one home both `internal/cookies` and `internal/config` can reach, and Arc X is wave 3); `internal/youtube/{player_api_strategy.go,service.go}` (T9 — two comments and one pin test; Arc Y is wave 1 and has merged); `internal/tui/status_bar.go` + `cmd/moombox/tui_wiring.go` (T4 — #70 requires BOTH dashboards, and the TUI's cookie badge has no other home; the hunks are `cookieBadgeFor` at the top of the file and two render arms, disjoint from Arc C's rows); `web/public/modules/utils.js` (T4 and T7 — the Web cookie badge and the `atTheHost` predicate both live there; no Arc W row touches either function). `cmd/moombox/services.go` takes a second hunk in T10 (one closure beside `AcquisitionMode`), distinct from T2's resolver hunk and from Arc C's `:394` row.

**Type consistency.** `ProcessYouTubeVideoResult.Denied` (T1) is read by `recordTerminalMemo`'s new fourth parameter (T1) and nowhere else. `resumeCookieParkedJobs`'s `wake func()` (T2) is the fourth parameter at both call sites. `AuthStatus.CookieFileError` (T4) is produced by `CookieJar.LastLoadError()` (T4), projected as `fileError` on both payloads (T4), read as `status.fileError` in `cookieIndicatorState` (T4) and as `auth.CookieFileError != ""` by `cookieBadgeFor`'s third parameter (T4). `jsonResponseSized`/`jsonErrorSized` (T6) are used by T7's `requireLoopbackForBrowserSetup`. `viewerIsAtTheHost` (T7) is called by `reloginPromptTarget` (same file) and imported by `settings.js` (T7). `utils.DirTighteningAllowed` (T5) is called from `internal/cookies` and `internal/config`. `dpapi.ValidateProfileDir` (T10) is reached through the `dpapiValidateProfileDir` seam. No symbol is named in one task and spelled differently in another.

**Placeholders.** Every code step carries the actual code. Four steps hand the implementer a test SKELETON with the assertions and mutants named but the harness left to the package (`TestCookieImportAnswers409WhileARefreshRuns`, `TestCookieWritersSetContentLength`, `TestAutoSetupTrioIsLoopbackGated`/`TestAutoSetupStartRejectsAnUnknownPlatform`, `TestStatusBarNamesAnUnreadableCookieFile`, `TestDpapiExplicitProfileDirTakesPrecedence`, `TestValidateRejectsATraversingDpapiProfileDir`). That is deliberate and bounded: each says exactly which existing harness to copy, what to drive, and what to assert, because these packages have bespoke per-file harnesses that must be reused verbatim rather than re-invented — inventing a second one is how a test ends up not exercising the shipped path. Every such step names the grep that finds the harness.
