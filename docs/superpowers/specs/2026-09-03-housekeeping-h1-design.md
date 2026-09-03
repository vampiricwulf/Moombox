# Housekeeping H1 — six items outside the cookie subsystem: design

**Status:** owner-ruled items from the post-Arc-9 worklist §6 (2026-08-29) and the Arc 11/12 reviews;
drafted 2026-09-03 from `h1-file-map.md`. Runs on branch `cookie-housekeeping-h1` from `main` in a
worktree, in parallel with Arc 12c, because none of these files collide with it (the two shared files
— `internal/config/types.go`, `cmd/moombox/services.go` — are touched at non-overlapping ranges).
**Scope:** `internal/chat`, `internal/worker`, `internal/twitch/api.go`, `internal/config/types.go`
(one comment), `.gitattributes`, a new `internal/docs` citation test, the spec sentences each item
flips. No behaviour change except items 1 and 3 (item 3's `gqlBaseRetryDelay` becomes a `var` with the
same value — a test seam, not a behaviour change).

## R1 — `MarkStreamEnded` closes `liveContinuationOpen`
`internal/chat/downloader.go` `MarkStreamEnded()` (:329-336) closes `liveContinuationOpen` the way its
closest sibling `Stop()` (:352) does — assigned directly under the `cd.mu` it already holds (the
`setLiveContinuationOpen` setter at :405-409 takes the lock itself and would deadlock here), BEFORE
waking `cancelCtx()`. It joins the exits that already close the signal: `Stop`, the two permanent
`handleFetchError` branches (:580, :601) and the definitive end in `handleEndOfStream` (:730). The
joint-idle gate (`internal/worker/interruption.go:221-233 buildMayResume`) then sees the signal closed
the moment the orchestrator marks the stream ended, instead of waiting for the chat loop's own exit.
A fifth test in `downloader_livestate_test.go` (`TestMarkStreamEndedClosesLiveContinuationOpen`) joins
the four; the file header's enumeration lists five exits. Mutation: drop the assignment → the test
fails. `TestMarkStreamEndedSetsFlag` stays. Known, pre-existing for `Stop()` too, and subject to the
plan's RULING NEEDED: `noteLivePollResult` (:450-457, called at :535) re-opens the signal for a poll
that completed just before the exit, because it checks none of the exit flags.

## R2 — The shared segment clock's write site is pinned
`internal/worker/orchestrator_youtube.go:166-171` — `onSegmentProgress` stores `lastSegTime.StoreNow()`
when `segmentProgressResetsStallCounters` says so. Today no test reaches it (nothing calls
`runLiveStreamDownload`). Extract the callback's body into a pure, testable function (the file's own
pattern: extracted pure helpers) — `noteSegmentProgress(p, lastBytes, lastSegTime, consecutiveLiveChecks)`
in `interruption.go`, beside the predicate it wraps — called from the closure unchanged in behaviour.
`atomicTimeValue` (`interruption.go:19-29`) has `Store(time.Time)` and `StoreNow()` and no injected
clock, and adding one would put a `time.Now()` into every progress callback for no behavioural gain, so
the test does not fake the clock: it brackets the call (`before := time.Now()` … `after := time.Now()`)
and the stored stamp must fall inside — an exact assertion. A byte-advancing progress stores the time
and zeroes the counter; a non-advancing one does neither. Mutation: delete the `StoreNow()` → the test
fails; delete the counter reset → the test fails.

## R3 — No GQL response body reaches a log line or an error string
`internal/twitch/api.go` `gqlRequest`: the 429/5xx errors built at `:233` and `:239` interpolate
`string(respData)`; the retry Debug line at `:196-198` logs `prev_err` verbatim, so an intermediary's
error page (which can echo the `Authorization` header) reaches the log on the next attempt. The three
un-retried arms (`:252`, `:254`, `:260`) interpolate the body too, and those are the errors that travel
up to callers who log them. Decision (the plan grepped the six callers — every one does
`if err != nil { return err }`; none reads the body from the error string): the body leaves ALL FIVE
errors — each renders `gqlBodySize(respData)` = `"<n>-byte body"`, a count, never the bytes — with every
message prefix byte-identical (`worker.classifyProbeErr` reads `http 5`/`http 4` out of `gql http <code> (`
positionally; the auth prefix `gql auth failure (<code>) (` deliberately contains neither and is
preserved for that reason), and the retry line logs the op, the attempt, the delay and `prev_status`
(the previous HTTP status; 0 for a transport error), never `prev_err`. A test with a logger that RENDERS
args (the existing recorders drop args — build one that formats them as key=value) drives each of the
five arms against a stubbed GQL whose body carries a marker string; the two retried arms run to
exhaustion with `gqlBaseRetryDelay` shrunk to a millisecond (it becomes a package `var` with the same
value, restored in `t.Cleanup`) so every retry line AND the returned error are inspected — a test that
cuts the backoff with a context deadline sees neither, and its mutants survive. Mutations: the body back
at any one of the five sites → that arm's subtest fails; `prev_err` added back to the retry line → the
retried subtests fail; the status no longer tracked → the retried subtest fails. `docs/spec/security.md`
gains one bullet: upstream response bodies never reach a log line or an error string (citing
`gqlRequest`, `gqlBodySize`, `classifyProbeErr` and the two tests by name and file).

## R4 — `types.go`'s `Platforms` comment states the seeding rule
`internal/config/types.go:209-212`: replace "auto-detected… (HasYouTubeAuthCookies / HasTwitchAuthCookies)"
with the truth of `detectCookiePlatforms` (`cmd/moombox/services.go`): sidecar first, then the LOOSE
`HasAnyYouTubeAuthCookie`/`HasAnyTwitchAuthCookie`; seeded only when both lists are empty; nothing
automatic prunes it — mirroring `docs/spec/data-and-storage.md:529`. Comment-only; `main.go:276-278`
is not touched (the `SetExpectedPlatforms` call is in the no-touch range).

## R5 — `*.css` / `*.svg` are pinned LF
`.gitattributes` gains `*.css text eol=lf` and `*.svg text eol=lf` beside the existing rules;
`git add --renormalize -- '*.css' '*.svg'` runs once (a no-op on today's index — both blobs are
already LF; the rule prevents a future CRLF commit from a Windows checkout). The commit says so.

## R6 — A citation-rot test guards the six spec docs
A Go test in a new `internal/docs` package (a one-line `doc.go` so `go build ./...` sees an ordinary
package; `go test ./...` reaches it) parses the six heavily-cited docs (`architecture.md`,
`data-and-storage.md`, `operations.md`, `platform-services.md`, `security.md`, `user-interfaces.md`)
and checks: (a) every `` `path/to/file.go` `` (and directory) citation names something that exists;
(b) every identifier-shaped `` `Symbol` `` / `` `pkg.Symbol` `` / `` `Symbol(args)` `` citation immediately
before a `.go` file citation, joined by a bare connector (the plan defines the pattern precisely and
pins it with `TestCitationShapes`), names an identifier that APPEARS in that file's code — as an
identifier or inside a string literal, comments excluded (`go/parser`, the `internal/web/routes` AST
precedent). Appears, not "is declared": the docs legitimately cite call and wiring sites, and a
declaration rule flagged five correct citations on `main`; a symbol alive only in a comment is the
`platform-services.md:907` rot and still fails; (c) five absence/state claims, each located by a
sentence substring (line numbers drift — the map's `:865`/`:1156`/`:529` are `:864`/`:1151`/`:529` on
`main`): `NewRefreshService(jar, 0, log)` has no production caller feeding the interval; the `Logger`
per-job buffer API is gone; the writers of `cfg.Cookies.Platforms` are exactly the seed + wizard merge
(`cmd/moombox/services.go`), the migration (`internal/config/config.go`) and the operator's
`PUT /api/config` (`internal/web/routes/config_routes.go`, the sole removal path the sentence names);
and `const livenessRecoveryArmed = false` as quoted by `data-and-storage.md` and `operations.md` (read,
never written) — all by walking non-`_test.go` ASTs; (d) every `§ Heading` cross-reference between the
six docs resolves. The test names every failure with doc:line. It runs in the normal suite (fast —
parsing, no network). The plan states what a first run finds on `main` (five rots, confirmed by running
the checker at `383ed7d`) and fixes those in the same task (each fix cites the truth).

## Non-goals
No cookie-subsystem file (`internal/cookies/**`) — those items are H2, after Arc 12c merges. No
`AuthCookieHorizonFor` sweep. No change to `SetExpectedPlatforms`. No behaviour change in the
orchestrator beyond the extraction (byte-identical behaviour, pinned by the existing tests).

## Invariants
Every assertion mutation-checked; no goroutine; the anonymous logger; no token/body/cookie value in
any log or test output; byte-wise LF; the branch merges cleanly onto `main` after Arc 12c (the
controller merges `main` into the branch at the gate and re-runs the citation test — plan Task 5
Step 11; new rot is fixed on the branch with a cited truth).
