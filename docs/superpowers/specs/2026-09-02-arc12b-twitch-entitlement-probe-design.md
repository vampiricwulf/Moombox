# Arc 12b — the Twitch tier-2 entitlement probe: design

**Status:** owner-scheduled 2026-08-29 (Q10, `progress-arc9.md:660` "Twitch tier-2 liveness probe",
carried in `post-arc9-worklist.md:57` as "Twitch tier-2 entitlement probe (playback-access-token
request as the probe; distinct from Arc 10's capture-time mark)"); drafted 2026-09-02 from
`arc12-file-map.md`; plan-reviewed 2026-09-02 (`arc12b-plan-review.md`). The liveness pilot stays disarmed — this probe is an OBSERVATION producer, exactly
like YouTube's tier 2, and inherits the pilot's gate.
**Scope:** `internal/cookies/refresh.go` (a Twitch twin of `FallbackLiveness`), `cmd/moombox/services.go`
(the closure — `internal/cookies` cannot import `internal/twitch`), `internal/twitch` (one reusable
probe function over the existing playback-token request), docs.

## 1. The problem, as the code stands

`checkTwitchAuth` (`internal/cookies/refresh.go:3349`) validates the `auth-token` with `oauth2/validate`,
which answers 200 for a token that is valid but no longer entitled to authenticated playback. Arc 10
made a dead token visible at CAPTURE time (`playbackTokenReportsAnonymous`, the
`playback-token-anonymous` mark), but a Twitch install with no capture running learns nothing until a
stream goes live. YouTube has a tier-2 producer (`FallbackLiveness`, `refresh.go:673`, wired in
`cmd/moombox/services.go:831-838`); `docs/spec/data-and-storage.md:838` states "Twitch has no tier-2
producer".

## 2. Required behaviour

**R1 — The probe is the playback-access-token request.** `internal/twitch` exposes ONE function the
wiring can call: given the jar and a channel login, request the stream playback access token the way
`GetHLSMasterPlaylist` does (`internal/twitch/api.go:689 GetStreamAccessToken`) and answer
`(signedIn, conclusive bool)` from `PlaybackTokenSession(value)` (`playback_token.go:41`): `user_id`
present → signed in; JSON null → anonymous; absent/undecodable/transport error → inconclusive; a
401/403 from the GQL call is INCONCLUSIVE too (`gqlRequest` raises `ErrTwitchAuthExpired` for both,
and which one a real expiry produces is unmeasured — `platform-services.md:531`), wrapped so the
field-test arm can still find it. It never logs or returns the token value — and that takes active
work: `gqlRequest` interpolates the response body into its 4xx/auth errors, so the probe SYNTHESISES
every error it returns (a test reverts that and watches the body leak).

**R2 — Which channel.** The probe targets the first configured Twitch monitor channel (the wiring
reads the config it already holds); with no Twitch channel configured the closure answers
inconclusive (Debug says why) and the existing `recordInconclusiveLiveness("twitch")` supplies the
once-per-process Info line — `internal/cookies` cannot know whether a channel is configured, so no
new dedupe lives there. Whether an OFFLINE channel's token identifies the session is the one thing
Arc 10 did not measure (it confirmed `user_id` as the discriminator on a live channel): the plan's
Task 0 is ONE gated live probe of the offline case, recording field NAMES only. Branch A (offline
answers): probe the first configured channel. Branch B (only live channels answer): the closure
re-issues the monitor's own `GetStreamInfoBatch` over the first 30 configured logins (the monitor's
own chunk size) to find a live configured channel — no new state, but a second GQL request per
tick, stated against R4. **Branch B is the default when Task 0 has not been run:** every premise
it rests on is measured (Arc 10 confirmed `user_id` on a LIVE channel), while branch A's failure
mode if its premise is wrong is a conclusive false `loggedIn=false` every tick — the one direction
this arc must not fail in. Task 0 never blocks execution; a later BRANCH A finding moves the
picker in its own commit.

**R3 — A Twitch twin of the YouTube seam, no new mechanism.** `RefreshService` gains
`TwitchFallbackLiveness func(ctx) (loggedIn, conclusive bool)`, called from `doRefresh` beside the
YouTube call (`refresh.go:1630-1669`) under the same conditions (`allowFallback`, not observed
recently for `"twitch"`, only when the jar holds a Twitch `auth-token` — the NARROW
`jar.HasTwitchAuthCookies()` snapshot, not `HasAnyTwitchAuthCookie` which also accepts
`twilight-user`, because the probe SENDS the bearer token — and, one placement difference from
YouTube stated so it is not read as drift, that jar gate covers the WHOLE Twitch block, verdict arm
and inconclusive arm alike, where YouTube's `hasYTCookies` gates only the inconclusive arm because
YouTube's probe carries its own first gate and Twitch's refuses to send without the token), feeding
`ObserveLiveness("twitch", loggedIn)` (`:938`) — which the disarmed pilot gates exactly as it gates
YouTube. Nothing here writes `AuthStatus` directly; the capture-time mark (Arc 10) remains the direct
writer. The back-off re-alarm (worklist §6, Q1) applies per platform when it lands.

**R4 — Cost and cadence.** One GQL request per refresh tick per configured install, only when a
token exists (two on branch B, the default: the stream-info lookup over at most 30 configured
logins, then the token); the existing tick
(`refresh_interval`) is the throttle; no new timer; a 20 s ctx on the closure.

**R5 — Docs.** `data-and-storage.md:838` flips; `platform-services.md` § Twitch gains the probe row;
the field-test plan gains a row (the probe's first conclusive observation on a real token; arming
still the owner's).

## 3. Non-goals

No arming (`livenessRecoveryArmed` stays false). No change to `checkTwitchAuth`. No change to
Arc 10's mark. No probing of unconfigured channels. No entitlement check per channel (the probe is a
session-level signal).

## 4. Invariants

`internal/cookies` never imports `internal/twitch` (the closure lives in `cmd/moombox`); the token
value never reaches a log, an error string or a payload; every new goroutine has the inline recover
(none expected — the call is inline in `doRefresh`); the anonymous logger; every assertion
mutation-checked (the conditions, the platform string, the inconclusive arms, the narrow-vs-broad
jar gate — which only a `twilight-user`-only jar can tell apart); the comments and docs that
COUNT producers ("two producers, both YouTube" in `ObserveLiveness`, "two YouTube producers" in
`data-and-storage.md`) are rewritten in the task that makes them false.
