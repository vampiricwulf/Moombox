# Post-chain follow-ups (2026-09-15) — design

Three small fixes the 2026-09-15 sweep fix chain surfaced but did not own (they were outside its 35 findings). The owner ruled on each on 2026-09-15 after the chain report; every ruling below is verbatim and is the binding authority for §A–§C and their plans.

## 0. Owner rulings

- **O1** Twitch RECONNECT ends chat capture (the IRC client returns nil on RECONNECT, `Start` returns, the orchestrator never relaunches) → **fix now, standalone mini-SDD**: a sentinel that continues without charging the reconnect budget, like the keepalive verdict, plus a test.
- **O2** `chat_status="finished"` is set from the message count alone; a stalled Twitch VOD chat (sidecar preserved since Arc 1 R4) still reads finished in both UIs → **fix now, same mini-SDD as O1**: derive the status from the downloader's outcome (a stall reads as not-finished / resumable).
- **O3** DNS-rebinding residual on the external/public same-host origin rule, and the WS upgrade never reads X-Forwarded-Host and is port-wildcard while CSRF is port-exact → **fix both in one web mini-SDD**: the same-host rule gains the configured-public-host / cert-SAN comparison; the WS check reads XFH from a trusted proxy and goes port-exact, sharing `effectiveRequestHost` with CSRF; reviewed with a security seat.
- **O4** HLS end-verdict sites B/C exit with the fetch-failed error on a confirmed ended → **yes, make B/C match A**: finalize cleanly on a confirmed ended from any site; table test over the three sites.
- **O5** `/` and `/index.html` serve the raw embedded index; the cache-busted copy only reaches the SPA fallback → **fix in the web mini-SDD**: route both to the substituted copy with no-cache + ETag; omit `?v=` for an untrusted commit.

## 1. Shape

| Mini-SDD | Branch / worktree | Rulings | Packages |
|---|---|---|---|
| A Twitch chat lifecycle | `followup-a-twitch-chat` / `.worktrees/followup-a-twitch-chat` | O1, O2 | internal/twitch, internal/worker, the two UIs' chat-status rendering, docs/spec |
| B Web origin + root index | `followup-b-web` / `.worktrees/followup-b-web` | O3, O5 | internal/web (+ internal/config if a public-host setting is involved), docs/spec/security.md |
| C HLS end-verdict symmetry | `followup-c-engine` / `.worktrees/followup-c-engine` | O4 | internal/engine (+ internal/worker consumer), docs/spec |

Each runs the arcs' loop: plan → worktree cut from main → subagent-driven development (implementer → task review → ≤5 fix rounds) → Fable close review → ONE fix wave + scoped re-review → merge `--no-ff` into main without asking → post-merge gates → delete worktree AND branch. Plans are deleted by their last task. Nothing is pushed; no RELEASE_NOTES.md; no version bump.

## 2. Concurrency

A, B and C run concurrently (three worktrees, one implementer each). File overlap is checked at plan time: A and C may both touch `internal/worker` — they must touch different files, and the second to merge merges main into its branch and re-runs its gates before its merge candidate. ONE controller-run `go test -count=1 ./...` at a time.

## 3. Constraints (all three)

The 2026-09-15 chain's global constraints apply unchanged (`go 1.27`, no CGo, three builds; LF; the two commit trailers `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>` / `Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq` — the project's rule, which takes precedence over any attribution reminder in an implementer's own context; anonymous logger interface; inline recovers; U1000 hard gate; TDD with every new assertion naming its mutant; the citation test's declaring-file rule; pathspec ON the commit; implementers never stash/checkout/rebase/reset/amend). Protected behaviour is unchanged: ~60 Hz progress and DB cadence, DB layer, `monitors.probe_cooldown` default 0, the BotGuard interpreter gate, `/retry` vs `/resume` gates never shared, the Web cookie import unbounded, `EffectiveClientIP`/`trusted_proxies` client-IP semantics, the loopback-gated setup wizard, update-path compatibility.


## A. Twitch chat lifecycle

Two independent ways a Twitch chat capture ends short without anything saying so.

### Problem

**O1 — a server-requested RECONNECT ends chat capture for the rest of the job.**

`internal/twitch/chat_irc.go:572-578` — the IRC read loop answers Twitch's `RECONNECT`
directive by returning `nil`:

```go
if strings.HasPrefix(line, "RECONNECT") {
    cd.logger.Info("twitch IRC RECONNECT received; reconnecting", "channel", cd.channelLogin)
    return nil
}
```

`nil` is `Start`'s CLEAN-EXIT value. `internal/twitch/chat.go:1370` reads
`if err == nil || ctx.Err() != nil || !cd.IsRunning() { return nil }`, so the reconnect loop
returns instead of re-dialling. `Start` returning closes the orchestrator's `chatDone`
(`internal/worker/orchestrator_twitch.go:357-366`) and nothing relaunches chat: `startChat`
is called again only when a connectivity outage is declared over. Twitch issues `RECONNECT`
routinely when it takes a chat edge out of service, so a live capture loses chat for the
remainder of the stream on a routine maintenance message. No error is recorded anywhere.
Unpinned by any test (Fable arc-close item I2, `.superpowers/sdd/2026-09-15-sweep-1-twitch-chat/progress.md:57`).

**O2 — a stalled VOD chat is reported as finished.**

Since Arc 1 R4, `pagingStalled` (`internal/twitch/vod_chat.go:379-393`) returns an error and
PRESERVES the resume sidecar on a repeated cursor, a zero-edge page with `hasNextPage=true`,
or a failed offset fallback. That error is discarded: the chat goroutine calls
`twitchChatDl.Start(parentCtx)` and ignores the return
(`internal/worker/orchestrator_twitch.go:365`). The verdict is then derived from the message
count alone, in both orchestrators:

- `internal/worker/orchestrator_twitch.go:865-873` (Twitch, IRC + VOD)
- `internal/worker/orchestrator.go:558-566` (YouTube)

```go
chatCount := twitchChatDl.MessageCount()
chatStatus := "finished"
if chatCount == 0 { chatStatus = "unavailable" }
```

Two more writers then overwrite it with `"finished"` whenever a chat FILE is copied:
`internal/worker/orchestrator_mux.go:372` (per-part) and `:512` (`copyAssets`). Full writer
set today: `"pending"` / `"downloading"` (`orchestrator_chat.go:91,97`,
`stream_processor_twitch.go:431,604`, `stream_processor_youtube.go:555,566`),
`"unavailable"` (`orchestrator_chat.go:29,45`, `stream_processor_twitch.go:436`),
`"finished"` (the four sites above), `""` (`worker.go:1332,1406`, reinitialise).

Both UIs render the raw value. Web: `web/public/modules/job-details.js:206-212` (update path)
and `:427-437` (render path), a badge whose variant comes from
`{downloading:"primary", finished:"success", error:"danger", unavailable:"neutral", pending:"neutral"}`
and whose TEXT is `job.chatStatus` verbatim. TUI: `internal/tui/job_details.go:471-488`,
value `"<status> (N messages)"` coloured by `chatStatusColor` (`:730-743`) — and that row sits
inside `if isActiveState` (`:424`), so a **terminal job shows no chat row at all** in the TUI.

Recovery is not reachable either: `/resume` requires `StatusFinished && IncompleteTail &&
platform == "youtube"` (`internal/web/routes/jobs.go:1024-1047`) and `/retry`
(→ `ReinitializeJob`, which deletes staging) refuses `Finished` entirely (`:1000-1006`).
`docs/spec/platform-services.md:617` already claims the stall "errors out without ever marking
the VOD chat complete" and that "a later `/resume` retries" — both false today.

### Decision

Verbatim, the owner's rulings:

- **O1:** a sentinel error for RECONNECT that the reconnect loop treats like the keepalive
  verdict — continue reconnecting WITHOUT charging the reconnect budget — plus a test. Mirror
  that shape; do not invent a second mechanism.
- **O2:** derive chat_status from the downloader's OUTCOME — a stall reads as not-finished /
  resumable. No new display-string fields that restate a machine field (pick an existing status
  value if one fits, otherwise ONE new machine value rendered by both UIs with matching
  wording). Both UIs must agree.

### Design

**O1.** `errServerReconnect` — declared in `internal/twitch/chat.go` beside
`errKeepaliveTimeout`, same doc shape. `runIRCSession` returns it bare at the `RECONNECT`
prefix test in `chat_irc.go` and logs nothing there (one directive, one line). `Start`'s loop
gains one arm immediately after the `errors.Is(err, errKeepaliveTimeout)` arm
(`chat.go:1396-1412`), identical in shape: `cd.flush()`, one log line, `continue` — no
`reconnectAttempts++`, and NOT the reauth path's `immediate` (a budget carried from earlier
real failures still slows the next dial). Charging it would be worse than charging a keepalive:
a server rotating edges can issue several directives in one marathon stream, and ten exhaust
`maxReconnects`. The orchestrator sees no change: `Start` keeps running, `chatDone` stays open,
`MessageCount()` keeps climbing. The pre-existing `noteHandshakeOutcome` defer is untouched.

**O2.** One new machine value, `"incomplete"` — lowercase, one word, the same style as
`finished` / `downloading` / `unavailable` / `pending`, and it IS the label in both UIs (no
display-string field). `"error"` was rejected: nothing writes it today, it reads as a hard
failure, and the codebase already uses "incomplete" for captured-but-short (`incomplete_tail`).

- `chatOutcome` (mutex + `record`/`verdict`) in `internal/worker/orchestrator_chat.go` carries
  the downloader's terminal error off its goroutine. Both chat goroutines keep their inline
  `defer close(done)` / inline recover and gain `rec.record(dl.Start(ctx))`; the recover records
  a non-nil outcome too. On relaunch the LAST run's verdict wins.
- `chatStatusForOutcome(messageCount int, outcome error) string` — outcome first:
  `outcome != nil → "incomplete"`, else `messageCount == 0 → "unavailable"`, else `"finished"`.
- `recordChatOutcome(jobCtx, count, outcome)` writes `chat_status` + `total_chat_messages` and
  stores the value on the new `JobContext.ChatStatus`. Replaces both count-only writers.
- `chatFileStatus(jobCtx) string` returns `"incomplete"` only when the context carries that
  verdict, else `"finished"` — used at `orchestrator_mux.go:372` and `:512`, so copying a chat
  file can no longer overwrite the verdict. Every other case keeps today's behaviour.
- YouTube is unaffected in practice: `chat.ChatDownloader.Start` returns `nil` on both exits
  (`internal/chat/downloader.go:270,479`). Twitch IRC's "exceeded max IRC reconnects" now reads
  as `incomplete`, which is correct.

Rendering, matching each UI's existing style:

- Web: add `incomplete: "warning"` to BOTH `chatVariantMap` literals
  (`job-details.js:207`, `:428`). The badge text is already the raw value.
- TUI: `chatStatusColor` gains an `incomplete` branch BEFORE the `strings.Contains(lower,
  "complete")` branch (which "incomplete" would otherwise match, rendering cyan), returning
  `ColorWarning`. The terminal-job gap is closed narrowly: the Media section gains a `Chat`
  row for a non-active job **only** when the verdict is `incomplete`, formatted
  `"incomplete (N messages)"` — the same text the active row and the Web badge produce.

### Tests

- `TestIRCServerReconnectIsNotACleanExit` — `runIRCSession` against a server that welcomes then
  sends `RECONNECT` returns an error `errors.Is`-able to `errServerReconnect`. **Mutant:**
  today's `return nil` (Start reads it as a clean exit and chat ends).
- `TestIRCServerReconnectDoesNotChargeTheReconnectBudgetOrEndChat` — 12 directives (past
  `maxReconnects+1`): `Start` is still reconnecting and `backoffReconnects()` is 0. **Mutants:**
  `return nil` (Start returns after one session); dropping the `errors.Is(err,
  errServerReconnect)` arm (backoff lines appear, then "exceeded max IRC reconnects").
- `TestChatStatusForOutcome` table. **Mutants:** count-checked-first (an error with 5 messages
  reads `finished` — today's bug); dropping the outcome arm.
- `TestChatOutcomeKeepsTheLastRunsVerdict`. **Mutant:** a recorder that keeps the first error
  (a relaunch that succeeds would still report `incomplete`).
- `TestChatFileStatusDoesNotOverwriteAnIncompleteVerdict`. **Mutant:** the unconditional
  `"finished"` at `orchestrator_mux.go:372`/`:512` (the verdict is clobbered at mux time).
- `TestRecordChatOutcomeWritesTheRowAndRemembersItOnTheContext` (real DB). **Mutant:** writing
  the DB row without setting `jobCtx.ChatStatus` (the mux path clobbers it).
- Node: an `incomplete` chat renders a `warning` badge reading `incomplete` on BOTH the render
  and update paths. **Mutant:** no map entry — it falls through to `neutral`.
- TUI: `chatStatusColor("incomplete") == ColorWarning`. **Mutant:** the branch placed after the
  `"complete"` test (substring match → cyan, indistinguishable from a complete archive).
- TUI: a Finished job with `chat_status = "incomplete"` renders a `Chat` row reading
  `incomplete (12 messages)` in `ColorWarning`; a Finished job reading `finished` renders none.
  **Mutants:** the row left gated on `isActiveState` (the TUI never reports the stall);
  lifting the row unconditionally (every finished job grows a new row).

### Out of scope

- Opening `/resume` or `/retry` to a Finished Twitch job, and preserving staging for a stalled
  chat (`processJob` still `RemoveAll`s it, `worker.go:702`). This arc makes the stall VISIBLE;
  it adds no recovery gate, and `/retry` vs `/resume` gates stay unshared. Flagged for the owner.
- The pre-existing `noteHandshakeOutcome` behaviour when `RECONNECT` precedes `001`.
- Any other `chat_status` writer (`pending` / `downloading` / `unavailable` / `""`).
- `docs/spec/user-interfaces.md` — it carries no chat-status passage.

### Constraints

- `go 1.27`, no CGo, pure-Go dependencies; Windows x64 + Linux x64 + Linux arm64 must build.
- LF line endings in every file the chain touches. Commits carry the two trailers
  `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>` and
  `Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq`.
- The anonymous logger interface (`Debug/Info/Warn/Error(msg string, args ...any)`) stays
  anonymous per struct. Every goroutine has an inline `defer func() { if r := recover(); … }()`.
- `internal/tui` never imports `internal/web/routes` or `internal/bgutils` (compile-time embed check).
- Helpers are defined in the task that first uses them (staticcheck U1000 is a hard gate).
- TDD per task: the failing test is written and run red before the change; every new assertion
  names the mutant that fails it (the reviewer verifies at least one).
- Every JS-touching task gates `go test ./internal/web/routes/` (its tests lift app.js/player.js
  bodies into goja) AND `node --test web/tests/*.test.mjs`. Every task that renames, moves or
  deletes a Go symbol, or edits `docs/spec/*.md`/`SPEC.md`, gates `go test ./internal/docs/` (the
  citation test requires the DECLARING file).
- Behaviour that the owner's rulings protect stays: ~60 Hz progress pipeline and DB write cadence
  (make updates cheaper, never rarer); DB layer untouched for perf; `monitors.probe_cooldown`
  default 0; the BotGuard interpreter gate; `/retry` vs `/resume` gates never shared; the Web
  cookie import stays unbounded (a test forbids WithTimeout).
- Reviewers never edit files (they reproduce in `git archive` scratchpad exports); implementers
  use git only for `add`/`commit` on the arc branch; no stashing, no rebasing, no checkout of
  other branches.
- User-facing strings: match the twin UI's exact output before mirroring it (read the twin's code).

# §B — Web origin + root index

## Problem

**O3a — the same-host origin rule roots trust in a header the attacker controls.**
`isAllowedOrigin` (`internal/web/middleware.go:247`) answers `external`/`public` with
`sameSiteOrigin(origin, effectiveHost, effectiveScheme)` (`:264`, helper at `:358`), and
`effectiveHost` comes from `effectiveRequestHost` (`:292`), which is `X-Forwarded-Host` from a
trusted proxy or else `r.Host`. On a direct-exposure install there is no proxy, so the compared
authority IS the browser's `Host` header. A DNS-rebinding page served from `attacker.dns`, rebound
to the victim's address, sends `Host: attacker.dns` and `Origin: http://attacker.dns` — the two
match, CSRF passes, and `AuthMiddleware` has already waived the peer because it is loopback or
private (`internal/web/server.go:156-161`), as has `ipAllowedByNetworkAccess`
(`internal/web/middleware.go:196-200`). Recorded at Arc 5 Task 1 review as residual M-8 and at the
arc-close as "DNS rebinding (Host allowlist is the owner-level fix)".

**O3b — the WebSocket origin check and the CSRF origin check disagree.**
`allowedOriginPatterns` (`internal/web/websocket.go:477-511`) derives its host from `r.Host`
(`:495`) — never `X-Forwarded-Host` — and emits `hostname+":*"` (`:505`), a `filepath.Match`
wildcard over the port. The CSRF twin reads XFH from a trusted proxy and compares ports exactly
once either side names one. Two consequences: a Host-rewriting proxy loads the dashboard and then
has every upgrade refused (`docs/spec/security.md:196-200` documents this and calls the alignment a
chain-close residual), and the port half of the same-origin rule is wildcarded. Worse, the
patterns cannot express the rule at all: `authenticateOrigin` in
`github.com/coder/websocket@v1.8.15/accept.go` returns `nil` unconditionally when
`strings.EqualFold(r.Host, u.Host)` — the library accepts Origin==Host BEFORE consulting any
pattern, which is precisely the comparison a rebinding page controls. Arc 5 arc-close F6.

**O5 — `/` and `/index.html` serve the raw embedded index.**
`MountStaticFiles` (`internal/web/server.go:254`) builds a substituted copy with `?v=<commit>` on
`/moombox.css` and `/app.js` (`:258-263`), but the router's `NotFound` handler normalises `/` to
`index.html` (`:277-279`), finds it in the FS (`:282`) and serves the RAW bytes through
`fileServer` (`:285`). The substituted copy reaches only the SPA-fallback branch (`:290-294`).
That branch also emits `?v=unknown` / `?v=<rev>-dirty` on untrusted builds, because the
substitution gate is `s.commit != ""` and not `trustedCommit()` (`:310`) — harmless today only
because those URLs then land in `staticCacheHeaders`' no-cache arm (`:329-334`) with an
`assetETag` (`:346`). Arc 5 arc-close F8 + fix-wave concern.

## Decision

The owner's rulings, verbatim:

> **O3 (two parts, one branch).** (a) DNS-rebinding residual: on an external/public install the
> same-host origin rule compares against r.Host, which a rebinding page controls, and
> AuthMiddleware waives LAN peers, so a rebinding page can pass CSRF. FIX: ALSO compare against the
> configured public host and/or the TLS certificate's SANs when present (the WS upgrade already
> refuses r.Host in favour of cert SANs — audit item S-17; reuse its SAN source). If no public-host
> setting exists, design the rule on cert SANs only and say so. (b) WS upgrade ↔ CSRF alignment:
> the WS origin check never reads X-Forwarded-Host and is port-wildcard, while the CSRF check reads
> XFH from a trusted proxy and is port-exact. FIX: the WS check reads XFH from a trusted proxy
> through the SAME helper and becomes port-exact; update security.md.

> **O5.** `/` and `/index.html` serve the RAW embedded index.html; the substituted copy only
> reaches the SPA-fallback branch, and that fallback still emits `?v=unknown` / `?v=<rev>-dirty` on
> untrusted builds. FIX: route `/` and `/index.html` to the substituted copy with no-cache + ETag,
> and omit `?v=` entirely when the commit is untrusted. Keep the existing immutable/ETag policy for
> assets.

**No public-host setting exists.** `config.NetworkConfig` (`internal/config/types.go:42-80`) carries
`port`, `network_access`, `https_enabled`, `tls_cert_path`, `tls_key_path`, `password_hash`,
`client_token_ttl_days`, `trust_forwarded_proto`, `trusted_proxies` — and nothing naming a public
hostname. Per the ruling the rule is therefore designed on certificate SANs only, and no setting is
added.

## Design

### 1. Certificate identity (`internal/web/tls.go`)

`certWatcher.IdentitySANs() []string` returns the loaded certificate's SANs (via the existing
`SANs()`, `:79`) — but `nil` when the certificate is Moombox's own placeholder, detected as
`Subject.CommonName == placeholderCertCN && Issuer.CommonName == placeholderCertCN`, with
`const placeholderCertCN = "Moombox"` also used by `generateSelfSignedCert` (`:186`) so the two
cannot drift. Rationale: that certificate's SANs are `localhost`, `127.0.0.1`, `::1` plus whatever
interface IPs the machine had at first start (`:194-200`). They name the machine, never the address
the deployment answers to, so narrowing on them would 403 every external install reached by a DNS
name or a NATed public IP. A certificate the operator installed — Let's Encrypt, corporate CA, or
their own self-signed with a real CN — does name the deployment, and narrows.

Package-level reader `identityHosts() []string`: `nil` when `CurrentCertSANs` is nil (no TLS, e.g.
every reverse-proxy deployment), else `CurrentCertSANs.IdentitySANs()`.

### 2. The origin rule (`internal/web/middleware.go`)

`isAllowedOrigin` gains a fifth parameter `identity []string`. Order of evaluation, per mode:

| mode | rule |
|---|---|
| `localhost` (and unset default) | loopback IP, or `localhost`, **or** `hostInSANs` |
| `lan` | the above, or a private IP, **or** `hostInSANs` |
| `external` / `public` | `sameSiteOrigin(...)` must pass **AND**, when `len(identity) > 0`, `hostInSANs` must pass too |

The external/public arm is a conjunction, never a substitution: the existing host+port comparison
is unchanged and still runs first, and the certificate is an ADDITIONAL requirement layered on top
— which is what closes rebinding, because `attacker.dns` is in no certificate. `localhost`/`lan`
gain `hostInSANs` as a pure widening: those two arms are IP-class tests that reject any DNS name,
and the WebSocket check (below) starts routing through this function, so without it an install with
a real certificate for `dash.lan` would lose the socket it has today.

`hostInSANs(hostname string, sans []string) bool` compares `hostname` (already `u.Hostname()`: no
port, no brackets) against each SAN through `splitAuthority`, so IP SANs match canonically
(`::1` == `0:0:0:0:0:0:0:1`) and DNS SANs match case-insensitively. A `*.example` SAN matches
exactly one non-empty, dot-free leftmost label (RFC 6125), so a wildcard certificate does not
lock its own dashboard out. Mismatch returns `false` — the caller's existing
`403 {"error":"Forbidden: invalid origin"}` body is unchanged.

`originAllowed(store, r, origin) (allowed bool, comparedHost string)` is the one decision:
reads `network_access` once, computes `effectiveRequestHost` once, and calls `isAllowedOrigin` with
`identityHosts()`. `CORSMiddleware` (`:30`), `CSRFMiddleware` (`:172`) and the WebSocket upgrade all
route through it, so the three can never disagree again.

**Log line.** `CSRFMiddleware` gains a logger parameter (two call sites: `internal/web/server.go:116`
and `internal/web/middleware_test.go`) and its invalid-origin branch emits exactly one line
naming the compared pair before the 403:
`logger.Warn("CSRF: origin refused", "origin", clipForLog(origin), "host", clipForLog(comparedHost))`.
`clipForLog` caps an attacker-supplied value at 200 bytes and runs `strings.ToValidUTF8`, because
the value reaches the log ring buffer the dashboard renders. Volume is bounded by the existing
per-IP API rate limiter. The CORS and WebSocket arms stay as quiet as they are today.

### 3. The WebSocket upgrade (`internal/web/websocket.go`)

`allowedOriginPatterns` is DELETED (with the now-unused `net` and `strings` imports — they have no
other user in that file). `WebSocketHub` gains `OriginCheck func(*http.Request) bool`, set by
`NewServer` beside the existing `ClientIP` hook (`internal/web/server.go:101`) to
`func(r *http.Request) bool { ok, _ := originAllowed(store, r, r.Header.Get("Origin")); return ok }`.
Nil means accept, which only test harnesses rely on.

`HandleUpgrade` checks before `websocket.Accept`: when `Origin` is non-empty and `OriginCheck`
rejects, it logs `websocket upgrade rejected: origin refused` with the clipped origin and host and
answers `403 Forbidden`. An EMPTY `Origin` is still accepted, matching both the library and today's
behaviour for non-browser clients. `AcceptOptions` then carries `InsecureSkipVerify: true` and no
`OriginPatterns`, because the library's own check cannot express the rule: it accepts Origin==Host
unconditionally and matches ports through `filepath.Match`.

New inputs the check now reads that it did not before: `network_access`, `network.trusted_proxies`
+ `X-Forwarded-Host`, `network.trust_forwarded_proto` + `r.TLS` (via `effectiveRequestScheme`), and
the certificate identity. Ports are now exact.

### 4. Root index (`internal/web/server.go`)

`Server` gains `indexHTML []byte`. `MountStaticFiles` substitutes `?v=` only when
`s.trustedCommit()`; otherwise `indexHTML` is the raw bytes. `serveIndex(w, r, fsys)` sets
`Content-Type: text/html; charset=utf-8`, `Cache-Control: no-cache`, the
`assetETag(fsys, "index.html")` validator, and writes through
`http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(s.indexHTML))` so
`If-None-Match` and `HEAD` are answered by `net/http`. The `NotFound` handler calls it for
`urlPath == "index.html"` — which is what both `/` and `/index.html` normalise to (`:277-279`) —
BEFORE the file-exists branch, and the SPA fallback calls the same helper. Asset policy
(`staticCacheHeaders`, immutable-on-trusted-`?v=`) is untouched.

The ETag stays correct in both arms by construction: on a trusted build it is the commit, which
identifies the whole build; on an untrusted one no substitution happened, so the served bytes ARE
the file `assetETag` hashes. That invariant is what Task 4's hash assertion pins.

## Threat notes

Closed: a DNS-rebinding page can no longer pass CSRF, CORS reflection or the WebSocket upgrade on
an `external`/`public` install **that has an operator-installed certificate** — its host is in no
SAN, and the conjunction fails. The WebSocket upgrade stops being the weak twin: it no longer
accepts Origin==Host by library default, no longer wildcards ports, and honours the same
`trusted_proxies`/`X-Forwarded-Host` rule as CSRF, so a Host-rewriting proxy that works for the
dashboard now works for the socket too.

Not closed: an install with no certificate, or with only Moombox's placeholder certificate, keeps
today's behaviour exactly — there is nothing server-controlled to compare against, and inventing a
config setting was ruled out. Stated plainly: **a rebinding attacker who also controls DNS for the
configured public host — i.e. for a name the certificate attests — is out of scope.** So is any
attack that does not depend on the origin comparison: this changes no client-IP handling
(`EffectiveClientIP`, `trusted_proxies` semantics, the IPv4-only bind, `Header.Get` reading only the
first field line are all untouched), no authentication, and no rate limiting. The `localhost`/`lan`
`hostInSANs` widening admits only names a server-held certificate attests, which an attacker cannot
choose. O5 changes caching and body selection for one path; it is not a security change.

## Tests

Every assertion names the mutant that kills it.

| # | case | asserted | mutant |
|---|---|---|---|
| 1 | external + identity `["dash.example"]`, Origin `http://dash.example`, host `dash.example` | allow | drop the `len(identity) > 0` guard so the arm denies whenever a cert exists |
| 2 | external + identity `["dash.example"]`, Origin+host both `attacker.dns` (rebinding) | **deny** | revert the arm to bare `sameSiteOrigin` |
| 3 | external, identity `nil` (no cert), Origin+host both `attacker.dns` | allow | make a missing identity deny — every certless install 403s |
| 4 | external + identity `["dash.example"]`, Origin `http://dash.example:8080`, host `dash.example:774` | deny | make the identity arm REPLACE `sameSiteOrigin` instead of conjoining |
| 5 | external + identity `["dash.example"]`, Origin `https://dash.example`, host `dash.example` (portless pair) | allow | default both ports unconditionally in `sameSiteOrigin` |
| 6 | external + identity `["*.example.com"]`, Origin `https://dash.example.com`, host `dash.example.com` | allow | drop wildcard expansion from `hostInSANs` |
| 7 | external + identity `["*.example.com"]`, Origin `https://a.b.example.com`, host `a.b.example.com` | deny | let the wildcard span a dot |
| 8 | lan, Origin `https://dash.lan`, identity `["dash.lan"]` | allow | drop `hostInSANs` from the `lan` arm |
| 9 | lan, Origin `https://dash.lan`, identity `nil` | deny | let the `lan` arm fall through to `sameSiteOrigin` |
| 10 | `localhost` mode, Origin `http://localhost`, identity `nil` | allow | make `hostInSANs` mandatory in every arm |
| 11 | `IdentitySANs` on the placeholder cert (CN `Moombox`, self-issued) | `nil` | drop the CN check — every self-signed external install 403s |
| 12 | `IdentitySANs` on a cert with CN `dash.example` | its SANs | return `nil` unconditionally — row 2 stops denying |
| 13 | CSRF through the middleware, trusted-proxy peer, `X-Forwarded-Host: dash.example`, Origin `http://dash.example`, `r.Host: internal:774` | 200 | make `originAllowed` use `r.Host` |
| 14 | CSRF, UNTRUSTED peer sending the same XFH, Origin `http://dash.example` | 403 + one `CSRF: origin refused` line naming the origin and `internal:774` | trust XFH unconditionally; or drop the log line |
| 15 | WS upgrade, `lan`, Origin `http://attacker.dns`, `Host: attacker.dns` | 403 | restore `OriginPatterns` / drop `InsecureSkipVerify` — the library's Origin==Host arm admits it |
| 16 | WS upgrade, trusted-proxy peer + XFH `dash.example`, Origin `http://dash.example`, external | accepted | derive the WS host from `r.Host` |
| 17 | WS upgrade, external, Origin `http://dash.example:99` vs host `dash.example:774` | 403 | re-introduce a `":*"` pattern |
| 18 | WS upgrade, no `Origin` header at all | accepted | drop the `origin != ""` guard — every non-browser client breaks |
| 19 | `GET /` on a trusted-commit build | body carries `/app.js?v=<commit>` and `/moombox.css?v=<commit>`, `Cache-Control: no-cache`, `ETag: "<commit>"` | leave `/` on the raw file-server branch |
| 20 | `GET /index.html` on a trusted build | identical to row 19 | handle only `/` |
| 21 | `GET /` with commit `unknown`, and again with `abc-dirty` | body carries NO `?v=`, and `ETag` equals `"<sha256 of the served body>"` | keep the substitution gate at `s.commit != ""` |
| 22 | `GET /` with `If-None-Match` echoing row 19's ETag | 304, empty body | write the body directly instead of `http.ServeContent` |
| 23 | `GET /app.js?v=<trusted commit>` | still `Cache-Control: public, max-age=31536000, immutable` | apply the index's no-cache policy to assets too |

Existing suites that must stay green unchanged: `TestIsAllowedOrigin`'s seven same-host rows,
`TestCORSReflectionFollowsTheOriginPolicy`, `TestCSRFMiddleware`,
`TestStaticAssetsAnswerConditionalGET`, `TestStaticAssetETagFallsBackToContentHash`,
`TestStaticCachePolicyFollowsTheCacheBuster`, `TestStaticCachePolicyNeedsATrustedCommit`,
`TestCompressionSkipsAlreadyCompressedMedia`, the `race_enabled_test.go`/`race_disabled_test.go`
sentinel pair, and `internal/web/routes/cookies_import_chain_test.go`.

## Out of scope

No new configuration setting (no public-host key). No change to `EffectiveClientIP`,
`trusted_proxies` parsing, `canonicalizeForwardedIP`, the IPv4-only bind, or any client-IP trust
decision. No change to authentication, session cookies, rate limiting, the loopback-gated setup
wizard, the unbounded Web cookie import, or the BotGuard interpreter gate. No change to
`sameSiteOrigin`'s host/port semantics themselves. No TLS version, cipher, or cert-rotation change.
No frontend or `index.html` content change. `CurrentCertSANs` stays a plain package var written
once before the listener starts — making it atomic is a separate concern.

## Constraints

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
  bodies into goja) AND `node --test web/tests/*.test.mjs`. Every task that renames, moves or deletes
  a Go symbol, or edits `docs/spec/*.md`/`SPEC.md`, gates `go test ./internal/docs/` (the citation
  test requires the DECLARING file).
- Behaviour that the owner's rulings protect stays: ~60 Hz progress pipeline and DB write cadence
  (make updates cheaper, never rarer); DB layer untouched for perf; `monitors.probe_cooldown`
  default 0; the BotGuard interpreter gate; `/retry` vs `/resume` gates never shared; the Web cookie
  import stays unbounded (a test forbids WithTimeout); the setup wizard stays loopback-gated.
- Reviewers never edit files (they reproduce in `git archive` scratchpad exports); implementers use
  git only for `add`/`commit` on the arc branch; no stashing, no rebasing, no checkout of other
  branches.
- User-facing strings: match the twin UI's exact output before mirroring it (read the twin's code).
- `docs/spec/security.md` and `docs/spec/user-interfaces.md` are updated in the task that changes the
  behaviour they describe, never in a trailing docs task.

# C. HLS end-verdict symmetry (O4)

## Problem

`runHlsLoop` (`internal/engine/downloader_hls.go:186`) consults `d.opts.CheckStreamStatus` at three exit sites. All three
classify the answer the same way (confirmed ended / confirmed still live / the check errored); they differ only in what
they DO with a confirmed end.

| Site | Lines | Reached when | On a confirmed `ended` TODAY |
|---|---|---|---|
| A | `downloader_hls.go:298-312` | the playlist fetch returned 404/410 | `d.streamEnded.Store(true); return nil` — clean finalize, on the FIRST failure |
| B | `downloader_hls.go:331-345` | the 6th consecutive playlist FETCH failure (`consecutiveErrors > 5`) | matches no `case`, falls past the switch, `return fmt.Errorf("HLS playlist fetch failed after %d consecutive errors: %w", …)` |
| C | `downloader_hls.go:385-395` | the 6th consecutive playlist PARSE failure | same shape, `return fmt.Errorf("failed to parse HLS playlist after %d consecutive errors", …)` |

B and C have `case checkErr != nil:` and `case !ended:` and no `default:`, so `(true, nil)` — the exact value site A
finalizes on — falls straight through to the error return. This is the Arc 2 Task-1 review's deferred minor (a),
`.superpowers/sdd/2026-09-15-sweep-2-engine/progress.md:20`: "site B has no `default:` latch — a confirmed ended arriving
on the escalation round exits with the fetch-failed error, `streamEnded` false, stale sidecar".

**"Confirmed ended"** is `CheckStreamStatus(ctx)` returning `(true, nil)` — YouTube HLS wires `ProbeVideoStatus` through
`observeYouTubeStatusProbe` (`internal/worker/strategy_youtube_hls.go:202-205`), Twitch its GQL broadcast probe
(`internal/worker/orchestrator_twitch.go:226` block). Same callable at all three sites; only the retry round differs.

**Observable cost.** `streamEnded` is not exported; its single effect is the loop's exit defer (`downloader_hls.go:189-195`):
`saveResume()` always, then `ClearResume()` **only** when `streamEnded` is set. So a B/C exit on a confirmed end (1) re-saves
and keeps `.resume.json` for a recording the engine has just established is complete — the orphan the ENDLIST site's comment
(`:756-761`) calls "a contract violation"; masked when the orchestrator wipes staging after a successful mux, NOT masked on
the paths that preserve staging (shutdown / user cancel in the same window, a mux failure leaving the part dir for a later
`/resume`), where the next Resume seeds from a sidecar for a broadcast that is definitively over; (2) hands the orchestrator
an error for a clean end — Twitch logs it at ERROR (`internal/worker/orchestrator_twitch.go:653-656`) then finalizes, YouTube
reads the value only for `ErrQualityLost` (`internal/worker/orchestrator_youtube.go:247`); (3) leaves one signal meaning two
things, decided by which round it lands on. *Erratum on the ruling's cost wording (Arc 2 precedent: sweep spec §5.1's
"job → Error" claim was corrected the same way): verified downstream, neither live orchestrator sets job status `Error` here
nor needs an operator retry — both finalize and mux what was captured. (1)–(3) are the costs that survive verification; the
FIX is unchanged.*

## Decision

Verbatim owner ruling **O4**: "in the HLS live download loop the end verdict has three sites. Site A finalizes cleanly when
the playlist confirms the stream ended. Sites B and C — the escalation round after fetch failures — exit with the
fetch-failed error, `streamEnded=false` and a stale sidecar even when the same confirmed-ended signal arrives, so those jobs
need a retry to finalize. FIX: B and C finalize cleanly on a confirmed ended, exactly like A; one table test over the three
sites; every existing pin stays green."

## Design

**One shared helper, because the classification is identical and the consequences are not.** Add to `downloader_hls.go`
immediately above `runHlsLoop`: `type endVerdict int` with `verdictNoCheck` (nothing wired — nobody can contradict the
site's own evidence), `verdictUnknown` (asked and ERRORED — not a verdict), `verdictEnded`, `verdictLive`; plus
`func (d *SegmentDownloader) consultStreamEnd(ctx context.Context) endVerdict`, returning `verdictNoCheck` when
`d.opts.CheckStreamStatus == nil`, logging the DASH loop's wording (`"stream status check failed; deferring end verdict"`,
`"err"`) and returning `verdictUnknown` on a non-nil error, else `verdictEnded` / `verdictLive`. It decides nothing — each
site maps the verdict onto its own exit, which is why a helper that *finalizes* would be wrong:

| Site | `verdictEnded` | `verdictLive` | `verdictUnknown` | `verdictNoCheck` |
|---|---|---|---|---|
| A | finalize (unchanged) | `ErrQualityLost` | fall into the retry budget | finalize (the variant is gone and nothing can say otherwise) |
| B | **finalize (new)** | `ErrQualityLost` | keep the fetch error | keep the fetch error |
| C | **finalize (new)** | `ErrQualityLost` | keep the parse error | keep the parse error |

Everything but the two bold cells is today's behaviour. Finalize = `d.streamEnded.Store(true); return nil`, exactly site A's
two lines, so the exit defer clears the sidecar. The 4-valued enum exists because A and B/C genuinely disagree about an
unwired check; three values would force one of those rows to lie. **The escalation-round error** is returned only on
`verdictUnknown` / `verdictNoCheck` — unchanged text, unchanged `%w` wrap of the fetch error, so `streamEnded` stays false
and the resume sidecar survives for a later Resume on every path that keeps it today.

**The second status check the review noted** (minor (b): consults at `:300` and `:332` in one iteration, when a 404/410's
consult deferred and that same round is the 6th failure) STAYS, and gains a comment saying why: before this change the
second consult could only discard an "ended"; after it, that consult is the one that finalizes cleanly. Removing it to
dedupe would delete the loop's last chance to end cleanly on the round it gives up; it costs at most one probe per six
failures and never fires when the first consult answered. Site C keeps its Arc 2 fix-wave comment shape: "See site B."

## Tests

One table, `internal/engine/downloader_hls_endverdict_test.go`, 3 sites × 3 verdicts = 9 rows, driven end-to-end through
`Start()` against an `httptest` playlist that always fails: 404 selects site A, 500 selects site B (no 404 branch; escalates
on the 6th fetch), HTTP 200 with an HTML body selects site C (`ParseHls` rejects a first line that is not `#EXTM3U`,
`manifest.go:501`). Each row asserts the exit error class, `streamEnded`, and the consult/fetch counts.

The two **new** rows are B/ended and C/ended. Mutant each kills: deleting `case verdictEnded:` from that site — `Start()`
then returns the consecutive-error / parse error with `streamEnded` false, so the deferred `ClearResume` never runs. The
`verdictLive` rows kill a `default:`-latch fix that finalizes on any answered check; the `verdictUnknown` rows kill
re-latching a failed check as "ended" (the T1-2 regression); the A rows kill a fix that routes a confirmed 404-round end
through the retry budget.

**Existing pins.** That file's three tests pin site A today (`TestHlsEndVerdict_CheckErrorDoesNotFinalize`,
`_StillLiveAfterFailedCheckRefreshes`, `_ConfirmedEndStillFinalizes`); nothing pins B or C. They are ABSORBED into the table
as its A rows — one table, no duplicate coverage — with every assertion preserved as a per-row field: the
`deferring end verdict` / no-`assuming ended` warn wording, `checks >= 2` on a deferred verdict, `checks == 1` and
`fetches == 1` on a confirmed end, and "unknown must not return `ErrQualityLost`". `hls404Server` and
`newEndVerdictDownloader` are used nowhere else in the repo (verified). The test keeps `d.delays = fastDelays()` with
`hlsPlaylistRetry = 10 ms` (Arc 2 seam).

## Out of scope

- The DASH twin's verdict block (`downloader_dash.go:443-456`) — already latches only on `checkErr == nil && ended`; its
  `streamEndVerified` / behind-head / interruption interplay is a different contract.
- The HLS loop's three other consults (`:587` stuck-skips, `:635` init failure, `:700` stale window) — each already latches
  only on a confirmed end (Arc 2 Task 1 verified this).
- Any `internal/worker` source change: the worker is a GATE here, not an edit surface — mini-SDD A also touches
  `internal/worker` and the two must not share files (`spec-header.md` §2).
- Job status, `incomplete_tail`, notifications, the resume-sidecar format.

## Constraints

Copied verbatim from the 2026-09-15 chain's global constraints; only the gate list and worktree recipe are branch-adapted.

- `go 1.27`, no CGo, pure-Go dependencies; Windows x64 + Linux x64 + Linux arm64 must build.
- LF line endings in every file the chain touches. Commits carry the two trailers
  `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>` and
  `Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq` — the project's rule, which takes precedence over
  any attribution reminder in an implementer's own context, whatever model name it shows.
- The anonymous logger interface (`Debug/Info/Warn/Error(msg string, args ...any)`) stays anonymous per struct. Every
  goroutine has an inline `defer func() { if r := recover(); … }()`.
- `internal/tui` never imports `internal/web/routes` or `internal/bgutils` (compile-time embed check).
- Helpers are defined in the task that first uses them (staticcheck U1000 is a hard gate).
- TDD per task: the failing test is written and run red before the change; every new assertion names the mutant that fails
  it (the reviewer verifies at least one).
- Every task that renames, moves or deletes a Go symbol, or edits `docs/spec/*.md`/`SPEC.md`, gates
  `go test ./internal/docs/` (the citation test requires the DECLARING file). No JS changes here, so the node suite and
  `./internal/web/routes/` are not gates for this branch.
- Behaviour that the owner's rulings protect stays: ~60 Hz progress pipeline and DB write cadence (make updates cheaper,
  never rarer); DB layer untouched for perf; `monitors.probe_cooldown` default 0; the BotGuard interpreter gate; `/retry`
  vs `/resume` gates never shared; the Web cookie import stays unbounded (a test forbids WithTimeout).
- Arc 2 invariants this branch must not disturb: the `hlsResumeSave` floor and the `readBody` cap;
  `reportFetchFailure(parent, …)` semantics (a caller cancel is never a network failure); the `delays` struct and its test
  seams (`SegmentTimeout`, `hlsPlaylistRetry = 10 ms` in tests).
- Reviewers never edit files (they reproduce in `git archive` scratchpad exports); implementers use git only for
  `add`/`commit` on the arc branch; no stashing, no rebasing, no checkout of other branches.
- User-facing strings: match the twin UI's exact output before mirroring it (read the twin's code).

**Branch gate list (`followup-c-engine`), run before the merge candidate:**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/engine/... ./internal/worker/...
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/docs/   # docs/spec edit or Go symbol move
gofmt -l ./cmd ./internal ./tools ./web          # must print nothing
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./...
staticcheck ./...                                # pinned 2026.2.1, clean
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./...
GOOS=linux GOARCH=amd64 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./...
GOOS=linux GOARCH=arm64 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./...
# ONE controller-run `go test -count=1 ./...` at merge time
```

**Worktree recipe:** `git worktree add -b followup-c-engine .worktrees/followup-c-engine main`, then copy the gitignored
inputs a fresh worktree lacks (engine/worker tests pull `internal/bgutils` transitively):
`internal/bgutils/embed/{node-windows-amd64.gz,node-linux-amd64.gz,node-linux-arm64.gz,sidecar.tar.gz}` and
`internal/cipher/testdata/*.js`. No `npm ci` — no JS in this branch. Prefix every go command with
`GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp`.
