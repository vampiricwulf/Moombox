# Cookie Remediation - Field-Test Plan (before arming, before the version bump)

Written 2026-08-29 against the working tree at `ed89fd2` (branch `cookie-arc9-docs-tests`, one
commit past `main` @ `8389009`). `origin/main` is at `613ae12`; nothing since has been pushed.
Every claim below cites the plan (`docs/superpowers/plans/2026-08-25-cookie-subsystem-remediation.md`)
or a ledger in `.superpowers/sdd/2026-08-25-cookie-subsystem-remediation/`. Where they disagree,
the ledger won (plan §EXECUTION STATUS).

Two rules for every step in this document:

- Never read, print, paste, or share a cookie VALUE or a webhook URL. Cookie names, row counts,
  timestamps and log lines are the only things you ever need to look at (plan §Global Constraints).
- Never test against your real install when a step says "throwaway instance". The A1-A3 method
  notes tell you why (plan §Arc 1 acceptance, method notes): a second instance needs a one-line
  mutex-name build variant and a redirected `LOCALAPPDATA`, and the notification sender does not log
  send outcomes, so delivery is only ever confirmed in Discord.

---

## Part 1 - What "arming" is

**Arming is one line.** `const livenessRecoveryArmed = false` at `internal/cookies/refresh.go:606`
(the decision comment is `:559-605`). Line numbers in this document are at commit `ed89fd2`; the
working tree currently carries UNCOMMITTED edits to `refresh.go` (+185 lines, Arc 9 Task 2's doc
reconciliation in flight) that move the constant to `:632` - always re-derive with
`grep -n "livenessRecoveryArmed = false" internal/cookies/refresh.go`. Arming means changing `false` to `true`, rebuilding, and
running that binary. It is a source constant, not a config flag: it cannot be toggled at runtime,
`-ldflags` cannot reach it, and the only way back is another build (progress-arc1.md, Task 6
review: "accidental arming impossible").

**What is gated.** Moombox has two tiers of session-liveness checking (plan §Arc 1):

- Tier 1 - the 30-minute `RefreshService` guide check (`checkAndRefreshYouTube` /
  `checkTwitchAuth`). **Not gated.** It already fires recovery and notifications today; A1/A2/A3
  proved that end to end on 2026-08-27 (plan §Arc 1 acceptance).
- Tier 2 - the per-channel membership-page probe (every feed cycle, channels with membership
  discovery on) and the channel-independent `/feed/subscriptions` fallback probe (only when no
  tier-2 observation landed in the last ~25 minutes; first one ~30 min after boot, and `R C` /
  `POST /api/cookies/recheck` deliberately never run it - plan §Staged rollout item 1). **This is
  what the constant gates.** Both feed `ObserveLiveness` (`refresh.go:796`).

**Disarmed (today):** a tier-2 verdict writes one log line and does nothing else:
`liveness observation platform=youtube loggedIn=<bool> wouldFireRecovery=<bool> armed=false`
(`refresh.go:822-830`). `wouldFireRecovery=true` on that line is the only evidence of what arming
would have done.

**Armed:** the same verdict, if signed out and past the 30-minute dedupe (`livenessRefireWindow`),
logs `a liveness observation reports this platform is signed out, triggering recovery` and calls
`OnRecoveryNeeded` (`refresh.go:831-839`). From there `handleRecoveryNeeded` in
`cmd/moombox/monitor_callbacks.go` splits on `cookies.auto_enabled` (refresh.go:562-594 decision
comment; plan §EXECUTION STATUS "settled meaning"):

| `auto_enabled` | What fires |
|---|---|
| `true` | ONE headless-browser refresh under a 2-minute timeout. Quiet if it succeeds or if it was declined because another pass held the single-flight. Otherwise a Discord notification: "Cookie Auto-Refresh Failed" (error) for a transport error or a conclusive failure, "Cookie Auto-Refresh Ineffective" (warning) for a pass that ran but reached no answer. |
| `false` | No browser. An immediate "Cookie Re-Authentication Required" (error) notification naming the cookie file. |

A per-platform 30-minute cooldown (`lastAuthFailNotify`, monitor_callbacks.go) bounds repeats; it
does not withhold the first one. **A wrong verdict therefore re-fires every 30 minutes forever** -
a notification every half hour on `auto_enabled = false`, a browser launch every half hour on
`auto_enabled = true` (plan §Staged rollout item 7; refresh.go:596-605).

**So "is it just testing?" - no. Arming is four things, in this order:**

1. **A soak (testing).** Run the disarmed build against real traffic for days and read the log
   (Part 2). This is the only thing that can show a systematically wrong verdict BEFORE it pages you
   every 30 minutes.
2. **A decision (yours, not testing).** Plan §Staged rollout item 7: when a session is genuinely
   dead, do you want the re-alarm every 30 minutes forever (what the code does today), once per
   process, or a back-off? Tier 1 alone notifies once per process; tier 2 armed re-fires on
   `livenessRefireWindow`. **Where the answer goes:** record it under plan §Staged rollout item 7
   as a ruling. If the answer is anything other than "every 30 minutes", that is a code change in
   `refresh.go` (`livenessRefireWindow`) and/or `monitor_callbacks.go` (`withAuthFailureCooldown`)
   that must land BEFORE the flip - not part of the flip.
3. **The flip (not testing).** `false` -> `true` at `refresh.go:606`, its own commit
   ("Flipping this to true is a deliberate, separate change" - refresh.go:604-605). Before you
   flip, re-read `refresh.go:559-605` and the two comments corrected at `cc51b81` (plan §Staged
   rollout item 3) - they are the decision documents and some of their justifications expire the
   moment the constant is true.
4. **An armed re-run (testing).** A5 must be re-run ARMED (Part 3) - the disarmed run passes for
   the wrong mechanism. Then the armed build soaks again with the same reading rules as Part 2,
   now watching for the Warn line and the notifications.

What arming requires of you, in the ledger's own words: "three runs (items 4, 6, and the five-day
soak) and one decision (item 7). Everything code-side is done." (progress-arc8.md §ARMING). Add A4
to that list - the plan's acceptance section still marks it "worth one live run".

---

## Part 2 - The pre-arming soak

Sources: plan §Staged rollout items 1-11; plan §Arc 0 acceptance (the "STILL OPEN" item);
progress-arc8.md §ARMING; progress-arc1.md Task 6 (log granularity rulings).

**Build.** From `main` (or this branch - see Part 6 about which), `go build -o moombox.exe
./cmd/moombox` with the constant still `false`. Confirm: `grep -n "livenessRecoveryArmed = false"
internal/cookies/refresh.go` before you build.

**Config.** Your real install, real cookies, real channels. Two runs are required (item 4):

- Run A: `[cookies] auto_enabled = true` (your normal shape).
- Run B: `[cookies] auto_enabled = false`. This needs a restart - the flag is labelled
  restart-required in both UIs on purpose (plan §EXECUTION STATUS, Arc 6 F2 ruling).

Default `INFO` log level is enough (item 2 - the "learned nothing" line is Info on first occurrence).
`DEBUG` is needed only for the two Debug lines named below.

**Duration.** "A few days" (plan §Staged rollout) - the Arc 0 acceptance yardstick is the
five-day divergence at which the old refresh had lost your session (plan §Arc 0 acceptance "STILL
OPEN"; progress.md Fable review: "yardstick runs to ~2026-08-30"). Run A should be at least that
long; Run B a day is enough to see the tier-1 shape stays silent with the browser timer off.

**Reading rules - what to grep the log for:**

| Step | Command / action | Expected | STOP if |
|---|---|---|---|
| 1 | Start the binary; read the first minute | `Cookies loaded completeAuthSet=true anyAuthCookie=true expiredYouTubeAuth=0 expiredTwitchAuth=0` (`cmd/moombox/services.go:372-376`). Non-zero expired counts mean the file on disk is stale - fix the export before soaking. | `youtube auth lost, triggering recovery` or `auth lost and automatic cookie refresh is disabled` appears at boot with cookies you know work (tier-1 false positive). |
| 2 | Wait ~30 min (item 1: the first tier-2 observation lands one cadence after boot; nothing can force it) | At least one Info `liveness observation platform=youtube loggedIn=true wouldFireRecovery=false armed=false` per process (Task 6 I4: healthy install = about one line per process). | Nothing for hours AND no "learned nothing" line: the probes are not running. Check membership discovery is on for at least one YouTube channel and that the jar holds a YouTube auth cookie (both probes gate on `HasAnyAuthCookie`, item 2 re-read note). |
| 3 | Every day: `grep "liveness observation" moombox.log` | Only `loggedIn=true`. Repeats are Debug and invisible; a change of verdict is Info. | **Any `loggedIn=false` while downloads are succeeding = FALSE POSITIVE. Stop. Do not arm.** Diagnose the page shape first (plan §Staged rollout "Any LoggedOut while downloads are succeeding"). Also stop on `wouldFireRecovery=true` on a healthy install. |
| 4 | `grep "learned nothing" moombox.log` | Absent, or one Info line once and then a normal `loggedIn=true` line later. | The "learned nothing" line is the ONLY liveness line for days = "all Unknown". Item 5b: F1's authenticated-side body shape was inferred, never measured; this is the first thing to measure (log the cookie NAMES `processYouTubeSetCookies` updates on one authenticated cycle - never values). Re-open the page choice (plan §Staged rollout). |
| 5 | `grep "triggering recovery" moombox.log` | Absent (healthy). | Present without a real outage: tier-1 false positive; A3-class diagnosis. |
| 6 | Check Discord daily | Nothing from Moombox about cookies. (With jobs parked in `COOKIES?`, the first healthy check of a process may send an info "parked jobs re-evaluated" notice - that is correct resume behaviour, not an alarm; plan §A4 caveat.) | Any "Cookie Re-Authentication Required" / "Cookie Auto-Refresh Failed" while cookies work. |
| 7 | Twitch | Nothing to watch at tier 2 - Twitch has NO tier-2 signal (item 2). Its only early warning is `expiredTwitchAuth` on the startup line. | - |
| 8 | Item 11(a) - if you click Refresh / `R C` during the soak | Count passes from the LOG, never from clicks. A click landing during a ticker pass runs no pass; at `DEBUG` you will see `cookie refresh skipped, another pass is already in flight` (`refresh.go:1044`). After `R F` or a recovery, an Info `auth re-check after ... was skipped` line is expected, not a fault. A ticker tick landing during a manual pass is dropped for one interval. | - (nothing here is a failure) |
| 9 | Item 11(b) - only if you reset `config.toml` but kept the data dir | `cookies.meta.json`'s verified `Platforms` seeds the platform list first (`services.go:378-385`, source is logged). If the sidecar still records a platform the jar no longer holds, the first conclusive check fires "auth lost" for it. | Do NOT read that one line as a false verdict; it is the same witnessed-transition behaviour a persisted config produces on every restart. |
| 10 | Item 11(c) - reasons | `youtubeError`/`twitchError` render on `GET /api/cookies/status`, the web badge title (inconclusive arm only) and the `R C` line. The push-driven TUI status bar renders NO reason, by design. | A reason string reading stale on the always-on TUI bar (it should never be there at all). |
| 11 | End of Run A: is the browser profile still signed in? | `browser-profile/cookies.sqlite` mtime advances on each periodic browser pass; `R F` reports "renewed"; no "could not confirm" every pass. | The profile stops being written and the log still says success - the Arc 0 defect shape. |

Retest caveat that applies to every step: `lastAuthFailNotify` suppresses a repeat notification for
30 minutes per platform. Restart the process between deliberate-failure runs or a working fix reads
as broken (plan §Arc 1 acceptance, retest caveat).

---

## Part 3 - Acceptance items A1-A5 (plan §Arc 1 acceptance)

`POST /api/cookies/recheck` (or `R C`) runs a tier-1 pass synchronously - the instant trigger for
A1-A4. It does NOT run the tier-2 fallback probe (item 1), which is why A5 needs a monitor cycle.

| Item | Status | What it needs | The observation that closes it |
|---|---|---|---|
| A1 - dead-session detection fires | **PASSED 2026-08-27** (throwaway instance, port 7741, `auto_enabled = false`, garbage `SAPISID`+`LOGIN_INFO` values, delivery confirmed by you in Discord). Fired at STARTUP, seconds after launch. | - | Log: `youtube auth lost, triggering recovery` then `auth lost and automatic cookie refresh is disabled - manual re-authentication required platform=youtube`; Discord: "Cookie Re-Authentication Required". |
| A2 - the half-cleared jar (LOGIN_INFO absent, SAPISID present) | **PASSED 2026-08-27.** Log showed `Cookies loaded ... completeAuthSet=false` and the check still probed and fired. Identical notification copy is correct. | - | Same as A1. |
| A3 - no false alarm on a transient error | **PASSED 2026-08-27** (guide URL pointed at RFC 5737 TEST-NET via a build variant, reverted). | - | No auth-loss line, no notification. |
| A4 - a healthy session stays silent | **OPEN** - source-established (progress-arc1.md, Fable Arc 1 review) "still worth one live run". | Your real install, working cookies, no jobs parked in `COOKIES?` (or expect the info notice). | `POST /api/cookies/recheck` -> no notification; YouTube green in the TUI status bar (`YT` green) and the dashboard badge ("YouTube: Authenticated"). The badge needs the platform in `activePlatforms`. |
| A5 - N channels do not mean N alerts | **OPEN, and MUST BE RE-RUN ARMED.** Disarmed, `ObserveLiveness` withholds the call, so the one notification comes from tier 1 and the run passes for the wrong mechanism (plan §A5 caveat; progress-arc1.md Fable review). | A throwaway instance with several YouTube channels configured, membership discovery on, a Discord webhook copied config-to-config (never printed), and a deliberately broken cookie file: edit the NAME of the `LOGIN_INFO` row (e.g. `LOGIN_INFO_X`) or delete the row on the throwaway copy. Never touch the value. | **Disarmed run (do it now):** after one monitor cycle, N `liveness observation ... loggedIn=false` Info lines, exactly ONE with `wouldFireRecovery=true`, and exactly ONE Discord notification (tier 1). **Armed run (after the flip):** same N lines, ONE `a liveness observation reports this platform is signed out, triggering recovery` Warn, ONE notification per platform per 30 minutes - never N. Restart between runs (30-min cooldown). |

---

## Part 4 - Every open field gate

Nothing in Arcs 2-8 is field-verified; every "verified" in the ledgers is a source read, a
mutation, or a test-gate run (plan §Arc 8 "Field gates"; progress-arc2.md close). These are the
gates the ledgers left for you. Setup and observation are exact; "failed" is what a wrong outcome
looks like.

| # | Gate (opened by) | What it proves | Setup | Closes when you observe | Failed looks like |
|---|---|---|---|---|---|
| 1 | Five-day pilot soak (Arc 0 acceptance "STILL OPEN"; plan §Staged rollout; progress-arc8 §ARMING) | The headless refresh keeps the profile signed in past the point it used to die, and tier-2 verdicts are correct on real traffic | Part 2, Run A, five days | Profile mtime advancing every periodic pass; no `loggedIn=false`; no cookie notifications; downloads that need an account still work on day five | A `loggedIn=false` while downloads succeed; or the profile stops being written while the log claims success |
| 2 | Badges on a real subscriber-only Twitch capture (progress-arc5.md Tasks 7+8 field gate; plan §Arc 5 gate 1; §Arc 8 gate 1) | Twitch's IRC actually accepts `PASS oauth:<token>` + `NICK <login>` - a premise from chatterino7's shape, never observed here. Before Arc 5, IRC had NEVER authenticated (unconditional `justinfan` nick) | Real install, Twitch cookies present with `auth-token` AND `login` rows, chat capture on; archive a live stream on a channel you are subscribed to where subscriber-only mode or sub badges are in play | Subscriber badges (and sub-only messages) in the captured chat; NO `continuing anonymously` Warn and NO `no usable login cookie` Warn in the log. Reading rule: presence of chat does not distinguish success from failure - the Warn and the badges do (plan §Arc 5 gate 1) | Chat captured with no badges; or a Warn `twitch never acknowledged the authenticated IRC login` / `twitch rejected the authenticated IRC login` (`internal/twitch/chat.go:426-434`) |
| 3 | Degradation notification arriving in Discord (progress-arc8.md Task 10 concern 4; plan §Arc 8 gate 1, second half) | The once-per-job "Twitch chat is anonymous for <channel>" notice is delivered, with the NEXT-capture sentence | Throwaway instance, Discord webhook, Twitch cookies with the `login` row deliberately broken: rename it (e.g. `login_X`) or delete it; start a Twitch live capture with chat on | Log: `twitch chat: auth-token present but no usable login cookie` (`chat.go:361`); Discord: warning titled `Twitch chat is anonymous for <channel>` whose body says this download is unaffected and "the NEXT capture will start anonymous" (`internal/worker/stream_processor_twitch.go:146-152`). Exactly one per job, even across reconnects | No notification (nobody has ever seen it arrive); or two per job; or a body naming a token or login |
| 4 | Credentialed Twitch IRC reconnect surviving a real network outage (plan §Arc 5 gate 2; §Arc 8 gate 2; progress-arc5.md fix round 2) | The drop-vs-refusal discriminator: a dropped socket is not a refused login, so a reconnect after an outage stays credentialed instead of latching anonymous | A real Twitch capture with credentials and chat on; pull the network cable / disable the adapter for 1-2 minutes; restore | Chat resumes after reconnect WITH badges; no `continuing anonymously` Warn; the job is not abandoned | Chat resumes without badges (latched anonymous), or the reconnect budget burns and chat is abandoned |
| 5 | Real end-to-end YouTube archive spanning a cookie rotation on members-only content (plan §Arc 5 gate 3 = Task 8's own gate; progress-arc5.md "Layer 3") | Every long-lived consumer (chat, DASH segments) now reads the jar at use time; the wire only changes for a rotated jar, so the archive must run past at least one ~30-minute `RefreshService` pass | Real install; archive a members-only (or age-gated) live stream longer than 30 minutes | The archive completes with no 401/403 stall after the 30-minute mark; log shows the periodic refresh pass during the capture; chat capture continues past it | Segment or chat 401/403 after a refresh pass; chat ending at the rotation |
| 6 | Real end-to-end Twitch live archive (progress-arc5.md "Layer 3") | The two-jar split did not break the Twitch downloader path (Layer 1 static + Layer 2 live gates narrowed the risk; only this closes it) | Real install; archive any Twitch live stream to completion, chat on | Finished job, muxed output, chat file present | Playback-token failure, or a playlist that never authenticates |
| 7 | Twitch keepalive observation (plan §Arc 5 "Twitch keepalive"; §Deferred; research-twitch-keepalive.md §"What I could NOT establish" item 1) | Whether the browser pass's `twitch.tv` navigation renews `auth-token`. No in-process path exists; if the nav does not renew, the answer is the unbuilt ingest endpoint | Real install with `auto_enabled = true` and Twitch cookies. Note the startup line's `expiredTwitchAuth` and, at `DEBUG`, any log of the Twitch auth horizon. Run `R F` (rung 1). Compare `AuthCookieHorizonFor(twitch)` before and after - a TIMESTAMP, never a value. Today the horizon has no production log line (progress-arc5.md Task 78 review: zero callers), so the cheapest reading is the Twitch `auth-token` row's expiry column in `cookies.txt` before/after, or a one-line temporary Debug log of the horizon | Horizon moves forward after the browser pass -> the keepalive already ships. Unchanged -> it does not; the Deferred entry's answer is the ingest path | Either result closes the gate; "failed" is only a browser pass that reports `could not confirm` (nothing to compare) |
| 8 | S8 - Chromium `Cookies` DB WAL staleness (plan §Arc 8 S8 + gate 5; `internal/cookies/dpapi/dpapi_windows.go:143-182`) | Whether the DPAPI fallback's `mode=ro` read of a LIVE Chromium cookie DB sees committed `-wal` frames or a stale checkpoint | A signed-in Chromium-family browser RUNNING (Chrome/Edge/Brave...) on the machine, receiving Set-Cookie (browse a site). The exact check, quoted from the source: "query journal_mode and a row count/timestamp twice, a few seconds apart, while the signed-in browser is known to still be receiving Set-Cookie responses, and confirm the numbers move." Read only `PRAGMA journal_mode` and `SELECT COUNT(*)` / a max timestamp column - never a value | `journal_mode` is not `wal`, OR the count/timestamp moves between the two reads | The numbers do not move while the browser is demonstrably writing: the read is stale, and the fix is a snapshot mirroring `snapshotFirefoxCookieDB` (copy `Cookies` + `Cookies-wal`), per the same comment |
| 9 | Fresh-profile `--screenshot` mystery (progress.md §FIELD GATE RUN; plan §Follow-ups 7(a)) | Why `--screenshot` produces no file on a freshly-created profile (both Waterfox and Firefox, no Job Object involved; only clue `RenderCompositorSWGL failed mapping default framebuffer`). Production's `browserRendered` keys off the screenshot and discriminated correctly on real setup-created profiles | Optional investigation. Run `MOOMBOX_LIVE_BROWSER_REFRESH=1 go test -count=1 -v -timeout 180s -run TestLiveFirefoxRefreshWritesTheProfile ./internal/cookies/` and read the logged screenshot line; then try a fresh profile with a display/GPU pref variation | A reproducible cause; or a decision to leave it (the gate counts `moz_cookies` rows instead and passes) | Not a release blocker. It matters only if `R F` on YOUR real profile starts reporting `could not confirm` every pass - then the screenshot signal has broken in production too |
| 10 | LibreWolf / Zen drain gate (progress.md §FIELD GATE CLOSED "STILL UNVERIFIED"; plan §Follow-ups 7(b)) | The Firefox-family drain (wait for the Job Object to empty, not the launcher) works on the two family members never tested | Only if you have LibreWolf or Zen installed: `$env:MOOMBOX_LIVE_BROWSER_REFRESH="1"; $env:MOOMBOX_LIVE_BROWSER_PATH="<path to librewolf.exe or zen.exe>"; go test -count=1 -v -timeout 180s -run TestLiveFirefoxRefreshWritesTheProfile ./internal/cookies/` | Both subtests PASS: `killed at launcher exit` writes 0 rows, `drained launch` writes YouTube rows. Elapsed time is an observation, not a threshold (2-14 s are all clean; `autocookies_refresh_live_test.go:92-115`) | `errBrowserDrainTimeout` - a process left alive in the job; that browser would burn 30 s and report failure on every refresh |
| 11 | Linux launcher-handoff measurement (plan §Follow-ups 7(f); progress-arc8 §ARMING ruling; Arc 9 precondition) | Whether native-Linux Firefox has the launcher handoff at all (Mozilla ships it Windows-only), which decides whether Linux reports `could not confirm` on every refresh | Only if you have a Linux box with Firefox: same test as row 10 with `MOOMBOX_LIVE_BROWSER_REFRESH=1` (the drained-launch subtest is not Windows-gated) | Either outcome, recorded: rows written and `rendered` true (no handoff) or the not-acted shape. Six comments and the Docker docs then get rewritten to the measured fact | A Linux run that leaves a browser detached and holding `parent.lock` (the H3 gap) |
| 12 | Abandoned-setup reap (progress-arc3.md Task 2 field gate, as amended) | Closing the setup browser releases the slot within 60 s, and a launcher-style Chromium binary is NOT killed at T+60 s while the real window is open | Real install (setup writes to your real `cookies.txt` by MERGE, so a throwaway is safer). Settings -> browser cookie setup on a Chromium-family browser; CLOSE the browser window without finishing; poll Settings. Second pass: start setup, leave the window OPEN and signed-out for >60 s | First pass: within 60 s Settings stops saying "setup in progress", a new setup starts, no orphan browser. Second pass: the window is still open at T+90 s (the reap keys on the Job Object, never on `cmd.Wait()`). Also on Firefox now (Arc 3 T3 gave it a Job Object) | A wedged "setup in progress" until restart; or your live login window dying at T+60 s |
| 13 | Arc 2 write-path abort on a real read blip (progress-arc2.md close: "nothing in this arc is field-verified ... the next real read blip") | An unreadable `cookies.txt` ABORTS the merge, leaves the file byte-identical, and the notification does NOT tell you to replace the file | Throwaway instance. Deny read on its `cookies.txt` (Windows ACL / Linux `chmod 000`), then `R F` or `POST /api/cookies/auto-refresh` | A 422 carrying the `ErrCookieFileUnreadable` message; file unchanged (compare size and mtime); the notification names permissions/mount and says Moombox retries on its own | A 500; a rewritten file; or a notification saying "replace cookies.txt" |
| 14 | First real Docker deployment (MEMORY.md "Docker support": remaining gate; plan §Arc 6 Docker guidance; §Arc 3 residual) | The container workflow the arc designed: `auto_enabled` off, first-boot auto-import when no `cookies.txt`, then manual profile update + `R F` / Settings-page button imports | A real container from `docker/`: mount a Firefox profile copied WITH `cookies.sqlite-wal`; no `cookies.txt` on first boot | Boot: one automatic import (`StartProfileSeed`), cookies appear, `Cookies loaded` line. Later: update the mounted profile, press "Refresh cookies from browser profile" on Settings (or `R F`) -> import with no browser. An abandoned setup there is reaped by the process group ~60 s after the browser is gone (A1 built on Linux, not field-verified — Part 7 row); the `abandon` beacon still releases where no group could be adopted | Import declined on first boot with an empty data dir; `R F` launching nothing and importing nothing; setup wedged with no way out but restart (NOT expected any more — a wedge that outlives the grace is exactly the field report the owner's ruling names) |
| 15 | Twitch chat re-auth registration site (Arc 10 field gate 1; `2026-08-29-arc10-twitch-credential-lifecycle.md:5167`; ledger Task 7) | `ExecuteTwitch`'s `o.twitchChats.add(irc)` + `defer` actually registers a real capture's IRC connection for credential-change repair - compilation and the registry's own tests cannot reach a live download | Start a real Twitch capture with chat on | The `Reauthenticate` Info line names the channel once credentials later change | No `Reauthenticate` line for a channel you know is registered, or the registration never fires |
| 16 | The end-to-end Twitch credential repair, including the transient-refusal edge (Arc 10 field gates 2 and 2a; `2026-08-29-arc10-twitch-credential-lifecycle.md:5168-5169`; ledger Tasks 4 and 7) | A capture with an `auth-token` and no `login` row renders the missing-login reason on BOTH per-request surfaces - the TUI's `R C` result line and the dashboard badge's tooltip (Task 7 Steps 4a/4b; before them a marked platform showed the verdict with no reason on either) - then re-authenticates with exactly one accepted-login line once the `login` row is added and `R C` is pressed; and the same repair fires on a TRANSIENT validate refusal that never touched `cookies.txt`, the path `OnCredentialsChanged` cannot see because the fingerprint never moved | Start a capture with an `auth-token` and no `login` row; confirm the not-authenticated reason on both surfaces; add the `login` row; press `R C`. Separately, with chat authenticated: block `id.twitch.tv` at the firewall for one 30-minute cycle, or point `twitchValidateURL` at a 401, without touching `cookies.txt`; then restore it | One `re-authenticating live chat sessions` Info line with `sessions=1` followed by one `twitch chat: authenticated login accepted` line for that channel; on the transient case, `twitch credentials usable again - re-authenticating live chat sessions` with `platform=twitch` on the recovering pass, then an accepted-login line | The reason missing from either per-request surface; more than one accepted-login line; or the transient case producing no repair because it waited for `OnCredentialsChanged` |
| 17 | Validate stickiness over a real tick (Arc 10 field gate 3; `2026-08-29-arc10-twitch-credential-lifecycle.md:5170`; ledger Tasks 2 and 7) | A Twitch mark set by a downgrade does not self-heal on the next periodic validate tick - only a real credential change or a `login`-row repair clears it | With the mark raised (e.g. from row 16's first setup), leave the install running through one 30-minute `RefreshService` cycle | The badge is still not-authenticated after the tick | The badge returns to green on its own after the tick with nothing having changed |
| 18 | The `playback-token-anonymous` mark on a genuinely expired token (Arc 10 field gate 4; `2026-08-29-arc10-twitch-credential-lifecycle.md:5171`; ledger Task 8 branch A) | Task 8 branch A's inferred dead-token arm fires on a real expired `auth-token`, not just the anonymous-by-design reply Task 0's probe observed (self-review residual 17) | Run a capture on an `auth-token` you know has genuinely expired, chat capture OFF | The `playback-token-anonymous` mark is set | The expiry surfaces earlier as `ErrTwitchAuthExpired` inside the GQL call instead, so the inferred arm never fires the way the arc assumed |
| 19 | Recovery interaction, one attempt not one per refusal (Arc 10 field gate 5; `2026-08-29-arc10-twitch-credential-lifecycle.md:5172`; ledger Task 7) | With `auto_enabled = true`, a Twitch chat downgrade produces exactly ONE browser recovery attempt even when more than one refusal reports it | Trigger a Twitch chat downgrade with `auto_enabled = true` | Exactly one browser recovery attempt runs | One recovery attempt per refusal - repeated browser launches for what is really one credential problem |
| 20 | The four newly-wired reload sites (Arc 10 field gate 6a-6d; `2026-08-29-arc10-twitch-credential-lifecycle.md:5173`; ledger Task 7a) | Each site `recheckAfterCookieWrite` now reaches - the job-triggered refresh, a failed recovery attempt, both UIs' wizard finish, and the `auto_enabled` periodic tick - actually runs an auth re-check afterward in a real deployment. (d) is a NAMED RESIDUAL: nothing offline can pin the tick's call to `OnPassCompleted`, because that branch needs a browser profile, a browser and a network | (a) Let a Twitch job fail on an expired token with `auto_enabled` on. (b) Let a recovery attempt run and FAIL. (c) Finish the setup wizard from each UI. (d) Leave the `auto_enabled` periodic timer to fire once | (a)-(c) each show an auth re-check in the log right after the write, and (c) updates the badge without waiting for a tick; (d) shows the same after a tick that ran a pass | Any of the four writes leaving the badge stale until the next unrelated tick; for (d), a tick that ran a pass and did not fire the hook - which looks exactly like a tick that declined, per the residual note |
| 21 | Arc 11 re-auth ingest (Arc 11 Task 5 decision: no live Go gate is warranted; this is what replaces it) | That a real export, pasted or uploaded through a real browser, lands on disk, reloads, verifies and resumes parked jobs — including the two things no unit test reaches: a browser-assembled multipart body, and the single-file bind-mount write failure | Any instance with real cookies. (a) Export a fresh Netscape cookies.txt from a signed-in private window on any desktop; paste it into Settings -> Cookies and press Import. (b) Repeat with the file picker instead of the textarea. (c) On a throwaway container ONLY, bind-mount cookies.txt as an individual file (`- ./cookies.txt:/data/cookies.txt`) and import again | (a) and (b): a green toast per accepted platform, the file on the volume gaining the pasted rows while keeping the sibling platform's, the header badge going green without a restart, and any `COOKIES?` job resuming. (c) a 409 whose message names the bind mount, and cookies.txt unchanged | A toast that says "configured" while the badge stays red (the verdict is being read from a pre-import snapshot); a merged file that lost the sibling platform's rows; a 500 with "cookie import failed" for the bind-mount case; or any cookie value visible in the response, the log or the notification |
| 22 | Twitch tier-2 entitlement probe answering on a real session (Arc 12b; `2026-09-02-arc12b-twitch-entitlement-probe.md` Tasks 1-3) | That `TwitchFallbackLiveness` returns a CONCLUSIVE verdict against a real Twitch session on the periodic path — the arc's only claim no offline test can reach. Everything else is unit-tested and mutated; nobody has watched this probe answer | Real install with a Twitch `auth-token` in `cookies.txt` and at least one enabled `platform = "twitch"` channel configured. Leave it running through at least two 30-minute `RefreshService` cycles at `DEBUG` (Twitch has no cheaper producer to make the probe stand down and its own stamp ages out inside one cadence, so EVERY periodic tick pays for one probe; the probe asks about the FIRST enabled configured Twitch channel whether or not it is live — Task 0 measured on 2026-09-02 that an offline channel's authenticated token still carries `user_id` — so nothing needs to be live during the window). Do NOT press Recheck Cookies or `R C` — `CheckNow` deliberately skips the probe | An Info line `liveness observation` with `platform=twitch`, `loggedIn=true`, `wouldFireRecovery=false`, `armed=false`, once per process for a healthy session (repeats drop to Debug). No `tier-2 twitch liveness probe did not answer` line, and no `liveness fallback probe learned nothing about this session platform=twitch` | `loggedIn=false` while Twitch downloads and authenticated chat still work — a false verdict, and the reason the pilot is disarmed; report it rather than arming. Or `tier-2 twitch liveness probe did not answer` on every cycle: read its `err` — `not attempted` means the jar or channel gate refused (check the `auth-token` row and the `platform = "twitch"` spelling), `twitch refused the credentials` means a 401/403 the arc deliberately treats as inconclusive (that is field gate 18's question, not this one), `did not say which session` means Twitch renamed `user_id` and `PlaybackTokenSession` needs re-measuring |

Not listed: the "next premiere" attestation gate from `project_attestation_pot_coherence.md` - the
cookie plan does not reference it, so it is outside this document.

---

## Part 5 - Smoke tests of every user-visible change, per arc

Tick each on a local build. "Both UIs" means the web dashboard (port 774) and the TUI. Web copy
is `go:embed`-ed - rebuild after any frontend change.

| # | Arc | Action | Where to look | Expected |
|---|---|---|---|---|
| 1 | 6 | `R F` with a browser installed and `auto_enabled = true` (rung 1) | TUI feedback line; log | Log `Browser cookie refresh requested from TUI`, a browser launch, drain lines; feedback reports the refresh renewed the profile (not "could not establish") |
| 2 | 6 | `R F` with `auto_enabled = false` (restart after the change) and a browser profile dir present (rung 2) | TUI feedback; Task Manager | No browser process starts; an immediate import from the profile; feedback reports the import |
| 3 | 6 | `R F` with no browser profile directory at all (rung 3) | TUI feedback; log | Feedback is exactly `No browser profile found, running R C instead...` AND a real in-process refresh pass follows in the log (not the sentence alone - progress-arc6.md rung-3 ruling) |
| 4 | 6 | `R C` on an `auto_enabled = false` install | TUI `M` menu, `?` help, the chord | `R C` is listed, works, and reports per platform. Never gated |
| 5 | 6 | Web: shift+click the header "Refresh cookies" button; then the Settings-page "Refresh cookies from browser profile" button | Toasts; log | Same three rungs as `R F`; rung 3 toast is exactly `No browser profile found, running a normal cookie refresh instead...` then a plain recheck runs. The Settings button reaches the same endpoint (no shift needed) |
| 6 | 6 | Change `auto_enabled`, `cookie_file` or `browser_profile_dir` in Settings and save | Web settings (restart badge on the field, "Restart Required" dialog on save); TUI settings (`[restart required]` marker, restart banner) | Both UIs label all three as restart-required (`settings.js:97-99`, `internal/tui/settings.go:64`) |
| 7 | 6 | Boot with `auto_enabled = true`, then turn it OFF at runtime | Log over the next periodic interval | The periodic browser timer KEEPS launching until restart - by ruling (`gateExempt`, plan §EXECUTION STATUS). The restart label is the honest cover. Do not "fix" it |
| 8 | 6 | Boot with `auto_enabled = true` and no browser profile dir (G2) | Log at `DEBUG` | `periodic auto-cookie refresh skipped - no browser profile directory yet` (`autocookies.go:2866`) instead of silence |
| 9 | 4+7 | Force an inconclusive check: disconnect the network, then `R C` / click Refresh | Web badge title; web toast; TUI status bar; TUI `R C` line | Web: yellow `indicator-warn`, "YouTube: Cookies saved - Moombox could not establish whether they work (<reason>)"; toast "YouTube - could not establish". TUI: yellow `YT` (unknown label), `R C` line "YouTube - could not establish (YouTube: <reason>)" in yellow. NEVER red, NEVER "not authenticated", NEVER "failed" |
| 10 | 4+7 | Conclusive failure on the throwaway (garbage/renamed auth rows), `R C` | Same four surfaces | Web: red `indicator-error` "YouTube: Not authenticated"; toast "YouTube not authenticated". TUI: red `YT`; `R C` line red (progress-arc8.md T12b: red in `R C` as in `R F`) |
| 11 | 4+7 / 8 | A job parked in `COOKIES?` (e.g. the throwaway with dead cookies and a members-only URL) | Web header badge; TUI status bar | Badge goes RED for that job's platform ("A download stopped for want of usable credentials"; `utils.js:527-531`) and outranks a green check; clears when the job is resumed, retried or deleted (`job_update` / `jobs_update` / `job_deleted`). TUI red `YT`/`TW` likewise |
| 12 | 8 | After a failed browser refresh (e.g. bogus custom browser path) | Web Settings cookies panel; TUI `R C` | Web: a "Last cookie error: ..." line (`#auto-cookie-last-error`, hidden when empty). TUI: `R C` line ends `| Last cookie error: ...` and is at least yellow |
| 13 | 8 | `curl -s http://localhost:774/api/cookies/status` (authenticated session) | JSON | Keys `found`, `authenticated`, `verification` (`ok`/`unknown`/`failed`), `youtubeError`; Twitch half has `twitchError`. Reasons empty on a conclusive check. The TUI STATUS BAR shows no reason text (by design) |
| 14 | 8 | Shrink the terminal to ~40 columns with a recorded cookie failure, `R C` | TUI feedback line | The truncated line is still yellow/red - never green (the clamp used to run before the colour; `app_update.go:809-815`) |
| 15 | 5 | Twitch live capture with `auth-token` + `login` rows present, chat on | Captured chat; log | Badges present; no `continuing anonymously` / `no usable login cookie` Warn. Before Arc 5 the nick was always `justinfan` |
| 16 | 8 | Same capture with the `login` row renamed on the throwaway | Log; Discord | One Warn `auth-token present but no usable login cookie`; one "Twitch chat is anonymous for <channel>" warning notification with the next-capture sentence; the running download unaffected |
| 17 | 5 | Start the binary | Log | `Cookies loaded completeAuthSet= anyAuthCookie= expiredYouTubeAuth= expiredTwitchAuth=` - both platforms always printed (`services.go:372-376`) |
| 18 | 8 | `POST /api/cookies/recheck` twice within a second, or `R C` twice | Log at `DEBUG` | Second call: `cookie refresh skipped, another pass is already in flight`; exactly one `cookie refresh done` Debug line. Second `R C` is a no-op, not a `RefreshDeclinedCauses` message |
| 19 | 8 | `R F` while the ticker pass is running | Log | Info `auth re-check after browser refresh was skipped, a cookie refresh was already in flight - status may lag until the next refresh` (`tui_wiring.go:436`); badge catches up next tick |
| 20 | 3 | Browser cookie setup on Firefox: finish it; separately, cancel it | The browser window | Finish and Cancel both CLOSE the Firefox window Moombox opened (Arc 3 T3; the window used to dangle). Quitting Moombox mid-setup closes it too |
| 21 | 3 | TUI setup wizard: walk away | TUI | The countdown is 300 s, not 60 (`internal/tui/setup_wizard.go:46`) |
| 22 | 3 | Web setup: close the dashboard TAB mid-login | The browser window | The window stays open - a tab unload is not consent (`/api/cookies/auto-setup/abandon` releases the slot only where releasing cannot kill). Escape / Cancel still close it |
| 23 | 8 | A browser-read failure from the Chromium ladder (blocked ladder / unanswered CDP read), if one occurs | Browser DevTools -> Network -> the `auto-refresh` or `finish` response | 409 with `"cause":"browser-ladder-blocked"` or 502 with `"cause":"browser-read-unanswered"` beside `error` (`internal/web/routes/cookies.go:181-196`); the toast shows `error` verbatim |
| 24 | 0 | `R F` whose browser pass did not render (e.g. profile removed mid-run) | TUI feedback; web toast | "Browser cookie refresh ran but could not establish whether these cookies work" - never "successful"; web adds "so the last-refresh time is unchanged" |
| 25 | 4+7 | Web Refresh (plain click) vs TUI `R C` on the same state | Toast vs feedback line | Same sentence per platform ("YouTube OK", "Twitch not authenticated", "... - could not establish") - pinned by exact equality |
| 26 | 6 | Docker (or any browserless host): `auto_enabled = false`, update the mounted profile, `R F` / Settings button | Log; `cookies.txt` | Import with no browser, regardless of the flag and of what `cookies.txt` held; `docker/entrypoint.sh:69-93` explains it in the seeded config |
| 27 | 8 | Fresh `config.toml` with an existing data dir | Log | The platform-detection line names its source (sidecar `cookies.meta.json` first, loose cookie-name predicates second; `services.go:378-385`) |
| 28 | H1 | The next Twitch GQL failure on a real install — a 5xx/429 during a Twitch incident, or a 401/403 on a dead `auth-token` (gate 18's setup produces one); nothing to force | Log at `DEBUG`; the job's error text | Every `gql …` error ends in `<n>-byte body` (a count — `gql auth failure (401) (StreamPlaybackAccessToken): <n>-byte body`), never a fragment of the upstream body; every `twitch gql retry` line carries exactly `op`/`attempt`/`delay`/`prev_status` and no `prev_err`; a 5xx still keeps the job waiting (`classifyProbeErr` reads `http 5` out of the unchanged prefix). Failed looks like: any `{"error"` or HTML fragment inside a `gql …` error or a retry line, or a `prev_err=` key on one (`internal/twitch/api.go` `gqlRequest`, H1 R3) |

---

## Part 6 - Before the version bump

**State to know first.** `origin/main` is at **`613ae12`** and nothing since has been pushed:
115 commits on this tree, ten `--no-ff` merges (Arcs 0, 1, follow-ups, F1, 2, 3, 4+7, 6, 5, 8).
The working tree is on `cookie-arc9-docs-tests` @ `ed89fd2` (Arc 9 Task 1: Chromium helper
tests), one commit past `main` @ `8389009`. Arc 9 (docs, `.gitattributes` `*.html`, the two CRLF
JS files) is not finished (progress-arc9.md), and at the time of writing `git status` shows
uncommitted edits to `internal/cookies/refresh.go` and two of its tests (Arc 9 Task 2 in flight).
Build the soak binary from `main` @ `8389009`, or from this branch only after that task has
committed and its gates are green - never from a half-edited tree. Decide whether the release
waits for Arc 9's spec rewrite; the plan's own sequence is arm -> Arc 9 -> final whole-plan review.

**Gates - every one from ONE run, 27 packages / 0 failures** (plan §Arc 5 standing rules; progress-arc9 §Standing constraints):

```powershell
go build ./...
go vet ./...
$env:GOOS="linux"; go build ./...; Remove-Item Env:GOOS
gofmt -l internal/ cmd/          # must print nothing (repo-wide since Arc 8 T5)
go test -count=1 ./... 2>&1 | Tee-Object gates.txt   # count 'ok' lines: 27; FAIL: 0
```

Do not run two `go test ./...` at once - concurrent runs starve `TestPotProvider_BypassCache` past
its 10-minute timeout (progress-arc2.md). Ignore gopls diagnostics that a real build disproves
(~18 phantom batches on this plan).

**Live gates** (each skips unless its variable is set; none prints a cookie value):

| Variable | Test | What it needs / proves |
|---|---|---|
| `MOOMBOX_LIVE_YT_TEST=1` | `go test -count=1 -v -run 'TestLiveLoginMarkersPresent|TestLivePublicExtraction' ./internal/youtube/` | Network only. Anonymous fetches of the two probe pages still carry a readable `LOGGED_IN` marker (`liveness_markers_live_test.go:32`); the cookieless extraction cascade still yields playable VOD + live formats (`extraction_live_test.go:29`) |
| `MOOMBOX_LIVE_YT_COOKIES=<path>` | `go test -count=1 -v -run TestLiveAuthenticatedAccountProbe ./internal/youtube/` | Path to a Netscape cookie file for a signed-in YouTube session (the path alone opts in). Proves the YouTube jar's header authenticates a real session: verdict `LoggedIn` (`liveness_markers_live_test.go:96`). Last PASS 2026-08-28 at `387d24a` |
| `MOOMBOX_LIVE_TWITCH_COOKIES=<path>` | `go test -count=1 -v -run TestLiveAuthenticatedTokenValidate ./internal/twitch/` | Path to a Netscape file for a signed-in Twitch session. Proves the Twitch jar's `auth-token` reaches `oauth2/validate` and gets 200 (`auth_live_test.go:46`). Last PASS at `2727c29` |
| `MOOMBOX_LIVE_BROWSER_REFRESH=1` (+ optional `MOOMBOX_LIVE_BROWSER_PATH=<exe>`) | `go test -count=1 -v -timeout 180s -run TestLiveFirefoxRefreshWritesTheProfile ./internal/cookies/` | An installed Firefox-family browser, network, ~10 s. Launches a REAL browser against a throwaway profile; `killed at launcher exit` must write 0 rows, `drained launch` must write YouTube rows. `BROWSER_PATH` steers to a specific exe because `DetectBrowser` ranks Waterfox above Firefox (`autocookies_refresh_live_test.go:117-135`). Last PASS 2026-08-27 on Waterfox and Firefox |
| optional: `MOOMBOX_LIVE_BG_TEST=1`, `MOOMBOX_LIVE_CIPHER_TEST=1` | `./internal/bgutils/sidecar/...`, `./internal/cipher/` | Not touched by this plan, but Arc 8 T5 pinned the WEB client version across Go and the sidecar; the memory rule is to run the sidecar/cipher gates when client code moves. Both need the embed blobs |

**Open owner questions the ledgers recorded - answer or consciously defer before release:**

1. **T10 job-row "chat: anonymous" indicator** (progress-arc8.md Task 10 concern 3; plan §Deferred) -
   a v19->v20 column for one Twitch-only bool plus a render in both UIs. The notification already
   covers the operator need; the downloader is shaped for it (`OnAuthDowngrade` fires once with a
   stable token). Yes / no / later.
2. **The `mux-wait-chat-idle` worktree** (`.worktrees/mux-wait-chat-idle`, branch at `376ff90`,
   one commit off `5850ec2`: "release the chat-open resume signal on joint chat+segment idleness").
   It is outside the cookie plan and no cookie-arc review read it. Decide: merge it into this
   release (it needs a rebase across nine merges and its own gate run), or hold it. The ledgers
   only mention the worktree in passing (progress-arc47.md:229); I found no recorded ruling on it.
3. **F3 release-note copy** (progress-arc3.md Task 3 residuals; §Plan edits "Deferred and
   recorded, not lost") - the behaviour change that finishing or cancelling a Firefox setup now
   closes the browser it opened, and the TUI countdown going 60 s -> 300 s. Belongs in
   `RELEASE_NOTES.md`; nobody else will write it.
4. Two smaller deferred decisions the plan lists under §Deferred: the Twitch `login` diagnostic
   split (no advance warning before an expiring `login` is pruned) and the "capture started
   anonymous despite credentials" check (Part 7). Both are follow-ups, not blockers.

**Then the release checklist from `CLAUDE.md` §Release Process:**

1. `RELEASE_NOTES.md` from `git log --oneline v2.8.5..HEAD` (the previous tag is v2.8.5 at
   `613ae12`), grouped Features / Improvements / Bug Fixes / Internal, no heading. Include the F3
   copy above and, if you armed, say so plainly.
2. Bump `version = "2.8.5"` in `cmd/moombox/main.go:26`.
3. Commit both together: `chore: bump version to x.y.z - <short summary>`. Never re-tag; bump
   instead (memory: no tag replacement). Before committing, check that every non-Go text file you
   touched is LF (`.gitattributes` says `eol=lf`; a CRLF shebang breaks the container -
   progress-arc6.md close).
4. `git tag vx.y.z`, then `git push && git push origin vx.y.z`. CI reads `RELEASE_NOTES.md`.

The untracked plan document itself (`docs/superpowers/plans/2026-08-25-cookie-subsystem-remediation.md`)
is the source of truth for the whole remediation and is NOT in git. Never `git stash -u` or
`git clean` in this repo (progress-arc1.md Task 1b). Decide whether to commit it (and this file).

---

## Part 7 - What is deliberately NOT done (do not test for it)

| Item | Status | Source |
|---|---|---|
| Docker re-auth ingest endpoint (`POST /api/cookies/import`, paste/upload) | **Built, Arc 11 - no longer belongs on this list.** The endpoint merges a pasted or uploaded Netscape file into `cookies.txt`, reloads and verifies; `FlagManualRelogin` was re-added with its caller (Arc 11 Task 2), closing the reason it was deleted in Arc 8. What remains open is the field gate, not the feature - see Part 4 row 21 | `docker-ingest-brief.md`; Arc 11 ledger; Part 4 row 21 |
| "Capture started anonymous despite credentials in the jar" check | **Recommended follow-up, not built.** With chat capture disabled, a dead Twitch token is invisible at every level; the site is `Service.GetHLSMasterPlaylist` and the premise (does the playback-token reply say the token was honoured?) is unverified | plan §Deferred; progress-arc8.md Task 10 finding 2 |
| A1 (abandoned setup wedges acquisition) on Linux / Docker | **Built, not field-verified.** The reap fires on Linux and in Docker via a process group (`Setpgid` at launch, `/proc` for the count, an explicit group kill where Windows closes a Job Object handle); the decisions are unit-tested against a fake process table on Windows. By owner ruling there is NO Linux live gate here — a user's bug report is the gate. Still unreaped on darwin and the fallback build, where `abandon` remains the only release | plan §Arc 3 residual (closed); `a1-linux-process-group-reap-design.md` |
| Widening the `authStatusChanged` gate (`refresh.go:324-327`) so a push-driven surface could render `youtubeError`/`twitchError` | **Documented, not widened.** No push surface renders the reason strings; per-request paths do | plan §Arc 8 carry-over (i); progress-arc9.md ruling |
| An in-process Twitch keepalive | **Does not exist, by research conclusion.** yt-dlp never writes `auth-token` back; chatterino7 only detects expiry; the only issuer is `passport.twitch.tv/login`. Whether the browser pass renews it is Part 4 gate 7 | `research-twitch-keepalive.md`; plan §Deferred |
| Twitch tier-2 liveness / channel entitlement probe | Deferred; `checkTwitchAuth` (validate-only) is the only Twitch signal | plan §Deferred "Twitch liveness" |
| The two Arc 2 evaluation items (cross-writer lost update; browser path no-rollback overwrite) | **Ruled NO** in Arc 8 (11(h)); not open | plan §Deferred |
| G3/G4 explicit acquisition mode, V9 TUI cookie-setup entry point, A2, T3 within-platform convergence, DPAPI two-pass per-platform profile selection | Deferred, owner decisions | plan §Deferred |
| Periodic browser-free import from an unchanging profile in Docker | **Rejected** by your ruling - the trigger is manual (`R F` / Settings button) | progress-arc6.md "OWNER ANSWER" |
| Arc 9 docs (`SPEC.md`, `docs/spec/*`, `.gitattributes` `*.html`, CRLF `segments.js`/`trimmer.js`) | In progress on `cookie-arc9-docs-tests`; Task 1 landed (`ed89fd2`) | progress-arc9.md |
