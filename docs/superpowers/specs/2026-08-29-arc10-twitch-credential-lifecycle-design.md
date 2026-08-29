# Arc 10 — Twitch credential lifecycle: design

**Status:** settled by the owner on 2026-08-29 (rulings Q3, Q3a-d, Q6 recorded in the Arc 9
ledger); this document is the spec the implementation plan argues from.
**Scope:** `internal/twitch`, `internal/cookies`, `internal/worker`, `cmd/moombox` wiring, the
four spec docs that describe these paths. No new UI state. The liveness pilot stays disarmed.

## 1. The problem, as the code stands

A live Twitch capture that HAD credentials can end up capturing chat anonymously by four
routes (`internal/twitch/chat.go`, the `AuthDowngrade*` constants): Twitch refused the
authenticated IRC login (`login-refused`), Twitch never acknowledged it
(`login-never-acknowledged`), the jar holds an `auth-token` with no `login` cookie
(`no-login-cookie`), or with a `login` that cannot be sent as a NICK
(`unusable-login-cookie`). Today the only consequence is one notification per job
(`internal/worker/stream_processor_twitch.go`, `sendTwitchChatDowngrade`). Nothing marks the
platform as needing re-authorization: `AuthStatus.TwitchAuthenticated` is written only by
`RefreshService.refresh` from `checkTwitchAuth` (`oauth2/validate`), which returns 200 for a
valid token even when `login` is missing — so two of the four routes stay green forever. And a
repaired credential is not picked up by a running capture: credentials are re-read only when
the IRC socket reconnects on its own, and after a refusal `authRefused` latches for the life of
the downloader, so even a natural reconnect stays anonymous.

The owner's framing: "it should downgrade and mark Twitch as needing reauthorization, and if
the user fixes authorization during an active download it should immediately apply the
updated cookie. A 'chat: anonymous' job-row indicator is a symptom of a larger issue."

## 2. Required behaviour

**R1 — Every downgrade marks Twitch.** All four routes (Q3a) cause the Twitch auth status to
become **not-authenticated with a reason string** (Q3b): `TwitchAuthenticated=false`,
`TwitchVerification=RefreshFailed` (conclusive), `TwitchError` = the operator sentence for the
route (the existing `twitchChatDowngradeReason` vocabulary, never a value or login). Every
existing surface reuses this: the push-driven status bars (via `OnAuthChange`), the per-request
reason rendering (`R C`, `POST /api/cookies/recheck`, `/api/status`), and the notifications.
No new `CookieStatus` member, no new payload key, no schema change.

**R2 — The mark feeds automatic recovery (Q3c).** The mark fires `OnRecoveryNeeded("twitch")`
through the same dedupe a validate-found loss uses (`shouldFireRecovery` semantics: fire on a
witnessed authenticated→not transition; never twice for one loss). With `auto_enabled` on that
is the one automatic headless-browser refresh attempt; with it off, the "Cookie
Re-Authentication Required" notification. The existing once-per-job "Twitch chat is anonymous
for <channel>" notice stays — it names the job; the re-auth notice names the platform.

**R3 — The mark is sticky against validate.** While the mark stands, the periodic
`checkTwitchAuth` 200 must NOT flip the status back to authenticated (that would un-mark the
two missing-`login` routes within one tick with nothing fixed). Validate still runs and its
result is recorded; the mark wins for `TwitchAuthenticated`/`TwitchError` until R4 clears it.

**R4 — What clears the mark (Q3d): a credential-pair change, confirmed by chat.** The jar
gains a Twitch identity fingerprint (SHA-256 over `auth-token` + `login`, mirroring
`YouTubeIdentity`). When the fingerprint observed by `RefreshService.refresh` differs from the
one the mark was taken under, the mark clears and validate decides the status again;
`OnCredentialsChanged("twitch", identity)` fires. Sources of change: `cookies.txt` edit +
Recheck, `R F` / shift+click / Settings button (browser refresh, followed by its re-check),
the automatic recovery attempt, and — when Arc 11 lands — the import endpoint. Each reload
site is followed by a `CheckNow`, which is where the comparison happens; the plan pins that
per site. Validate 200 alone never clears the mark.

**R5 — Active chat sessions reconnect on change, immediately.** On
`OnCredentialsChanged("twitch", …)`, every active Twitch chat downloader — including one that
started cookieless — is told to re-authenticate: reset `authRefused` and `downgradeReported`,
drop the current IRC connection cleanly (without spending the reconnect budget), and let the
existing per-session credential read produce a credentialed handshake. A credentialed `001`
logs at Info and needs no further signal (the mark is already clear). A second refusal
re-marks with the new reason and notifies the job again — the latches were reset, so both
sites fire again by design.

**R6 — HLS side (Q6), premise first.** Task 0 of the plan is ONE live probe: decode the
playback-access-token `Value` (a JSON document) with and without the auth token and record
which fields differ (field NAMES and booleans only — never the value). If the reply states
whether the token was honoured, `Service.GetHLSMasterPlaylist`'s caller marks via the same
path as R1 when the jar holds credentials but the reply is anonymous (`"playback-token-anonymous"`
joins the reason vocabulary), so a dead token is visible even with chat capture off. If the
reply cannot tell, the plan records the finding in the spec docs and builds nothing on that
side. The video download itself is untouched either way — the playback token is fetched once
per capture.

## 3. Non-goals

No job-row indicator (Q3: rejected as a symptom). No new UI state or REST key. No change to the
liveness pilot (`livenessRecoveryArmed` stays false) — the mark is a direct write, not a
liveness observation, so it works with the pilot disarmed. No Twitch keepalive (research
conclusion: none exists in-process). No entitlement probe (that is Arc 12's tier-2 item). No
change to YouTube chat's `ErrAuthRequired` permanent exit (Arc 5 ruling).

## 4. Invariants that must survive

- No cookie value, token, or login ever reaches a log line, a notification, a payload, or an
  error string; reason strings are the fixed vocabulary only.
- `AuthStatus` writes happen under `rs.mu`; the sole-writer property of `refresh`'s status
  block is REPLACED by "two writers, both under `rs.mu`, mark wins" — the doc sentence that
  states sole-writer changes with the code.
- `OnAuthDowngrade` is still called on the IRC session goroutine and must not block; the mark
  and the notification are delivered asynchronously by their consumers.
- Every new goroutine carries the inline `defer func() { if r := recover(); … }()`.
- The anonymous logger interface stays anonymous. Partial DB updates go through
  `UpdateJobFields` (no DB change is expected here).
- Standing test rule: every assertion is mutation-checked; the fake IRC server drives refusal
  and `001`; the fingerprint change drives the reconnect; a test proves validate 200 does NOT
  clear a standing mark; a test proves a change DOES.

## 5. Docs that change with the code

`docs/spec/platform-services.md` § Anonymous Fallback and the Downgrade Report ("chat-disabled
jobs get no signal", "once per job", "the running download is unaffected — the next capture
starts anonymous"); `docs/spec/data-and-storage.md` § Cookies (the `AuthStatus` contract, the
sole-writer sentence, `OnCredentialsChanged` "fires for youtube only", `TwitchIdentity`
absence); `docs/spec/user-interfaces.md` (the `twitchError` reason sources; the notification
row); `docs/spec/operations.md` § Credential Notifications; `SPEC.md` § Authentication. Each is
edited in the task that flips the sentence.
