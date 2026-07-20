# Connectivity Detection Hardening — Tiered Follow-on to the 2026-06 Redesign

**Date:** 2026-07-14 (design) · 2026-07-17 (review addendum) · 2026-07-18 (OS-accelerator addendum)
**Status:** Converged design, reviewed against current code; parked for later pickup with open decisions (see addenda)
**Relationship:** Follow-on to the shipped [2026-06-02 connectivity redesign](2026-06-02-connectivity-detection-redesign-design.md). That work fixed the oracle to report reachability instead of adapter state; this hardens the residual bugs it left standing (stuck-offline on IPv6/blocked probes, transient-4xx wrongful give-up, discovery blackout, cross-service pause).
**Owner decisions preserved:** speed-first / instant-up recovery; pure-Go no-CGo probe; asymmetric-default classifier; captive-portal body-probe out of scope; FULL sqlite durability; ~60Hz progress cadence.
**How this converged:** multi-perspective consensus session — round-2 synthesis after six perspectives (concurrency, state-machine, YAGNI, maintainability/codebase-fit, network, operability) reached agreement. Tiering is the structural output: a low-risk load-bearing set (Tier 1) ships now; a coupled evidence-gated cluster (Tier 2) is deferred because the Tier-1 probe-target switch removes the root cause the deferred items were compensating for.

---

## 2026-07-17 REVIEW ADDENDUM — read this first

The design below was re-checked against the current tree (10-agent fan-out: 6 fact-checkers + 4 independent critics — network, concurrency, YAGNI/priority, adversarial red-team). **The spine is sound and factually verified.** T1 (probe the real service hosts), T2 (whitelist-terminal-4xx), and T4 (fix the false comment) hold up; the migration mechanics were confirmed correct down to the `NeedsAutoPersist` detail; the deferral of D1/D7/D5-damping and the CUT of D8/D5-part2 were endorsed. What follows is what changed, ordered by how much it should alter the plan. **Every file:line below is current as of 2026-07-17; the older body text still carries 2026-07-14 anchors — trust the addendum's numbers.**

### DECIDE FIRST — should the passive OFFLINE-trip be deleted? (reframes Tier 1)
The spec applies its own load-bearing insight ("T1 makes probe-reachability == fetch-reachability, so the divergence is gone") to cut D1 — but never applies it to the **passive tracker's offline-trip itself**. The oracle composes `nowOnline = online && !passiveOffline` (`internal/connectivity/monitor.go:185`). Under T1 the active probe already dials both real hosts with any-success semantics and flips offline within its 2-poll (~10s) debounce — which beats the download loop's ~30s give-up, so the loop sees `!IsOnline()` and waits before it can hard-error. The only residual the passive **offline**-trip uniquely catches is *both* services TCP-accepting while app-failing simultaneously — which is captive-portal/DPI territory the owner already ruled out of scope. Meanwhile that same offline-trip is the *source* of the cross-service-pause bug T6 exists to suppress and the idle-clear invariant T3 breaks.

That makes **T3 + T6 a mechanism built to contain a mechanism** — exactly the pattern the owner's standing principle says to delete rather than wrap ([[feedback_check_the_simple_model]], added to memory *after* this spec was written). The honest simple model:
- **Set the offline direction to `nowOnline = online`** (drop `passiveOffline` from the AND at `monitor.go:185`).
- **Keep `ReportSuccess → transition(true)`** (`monitor.go:161-174`) — the passive tracker's *recovery* path is a genuine speed-first asset the owner wants; only the *offline-trip* is the deviation.
- This **cuts T3, T6, and T5's lastSuccess-for-D1 rationale outright**, and dissolves the cross-service pause at its root.

If the passive offline-trip is kept instead, the spec must justify what it uniquely detects beyond the active real-host probe, given captive-portal is out of scope. **This is the single decision that most changes the shape of Tier 1 — make it before touching code.**

### CORRECTNESS — the central claim is overstated (4 critics independently)
The load-bearing sentence says the probe "succeeds iff a real request to those hosts could — the divergence is gone." That biconditional is **false in the sufficiency direction**: `reachabilityProbe` does a bare TCP connect + immediate close, no TLS, no HTTP (`internal/connectivity/probe.go:38-46`). A completed handshake to `www.youtube.com:443` proves an edge accepts SYNs — not that TLS-for-our-SNI or a 2xx follows. T1 closes only the **false-OFFLINE** direction (probe-blocked-but-service-up); it does **not** dissolve false-ONLINE. Still-uncaught, all of which read online while real requests fail:
- **Captive portal** terminating :443 with an interstitial (owner: body-probe out of scope — but then don't claim the probe proves reachability).
- **Anycast edge under incident** — SYN-ACK from a PoP, 5xx from the app.
- **DNS poison** to a walled-garden IP that accepts 443.
- **Corporate explicit-proxy** — the real clients honor `Proxy: http.ProxyFromEnvironment` (`internal/httpx/client.go:40`); the probe uses a raw `net.Dialer` (`probe.go:36-39`) that bypasses it. On an `HTTPS_PROXY`-mandated network, real requests succeed via the proxy while the probe's direct dial fails → **stuck-offline, the exact class T1 claims to fix.** So T1 dissolves the *firewall-allowlist* egress variant but **not** the *explicit-proxy* variant — narrow the claim (line 22) accordingly.

None of these are regressions (today's `1.1.1.1:443` also handshakes blind), but the prose promises a guarantee the probe doesn't give — the very false-guarantee class **T4** exists to stamp out. **Reword the insight to claim only the false-OFFLINE half.** This also promotes the TLS/SNI item (below): a cert-verifying TLS handshake is the cheapest thing that narrows the captive-portal/edge gap — but it is worthless with `InsecureSkipVerify` and still won't catch app-layer 403/5xx.

### PRIORITY INVERSION — D4 is the only true data-loss bug and it's tiered below a log
D4 (live-recording give-up hole) is the sole **Correctness/Reliability** item in the whole effort, yet it sits in Tier 2 "owner scope decision," beneath the T5 diagnostic log and the T3 discovery floor — a direct inversion of `Correctness > … > Polish`. Worse, **T1 arguably makes it worse for the common single-service outage**: T1's any-success probe keeps the oracle online whenever the *other* service is up, so a YouTube download during a YouTube-only outage keeps `IsOnline()==true`, skips the `waitOnline` branch, and walks into the give-up path. T1's "protects in-progress recordings" rationale therefore holds **only for both-services outages**; single-service recording protection depends on D4. Fact-check refinement: the loss shape is **premature finalize/mux of a still-live stream** (YouTube after ≥10 min via `streamSegmentTimeout`; Twitch immediately on `sessionLoop` break), not a job flipping to `Error`. **Surface D4 as the highest-priority item of the whole effort.**

### T6 IS UNDER-SPECIFIED / PARTLY BROKEN AS WRITTEN (only relevant if the passive trip is kept)
- **Raw `req.URL.Host` does not equal "service."** Moombox dials `www.youtube.com`, `youtubei.googleapis.com`, `decapi.me`, and `*.googlevideo.com` for the *one* logical YouTube service. Tagging by raw host yields ≥2 distinct tags in a YouTube-only outage → `minTags>=2` still trips → the cross-service pause T6 targets **survives**. T6 needs an explicit shared **host→service canonicalizer** (`www.youtube.com`/`googlevideo.com`/`youtubei.googleapis.com`/`decapi.me`/`ytimg` → `youtube`; `gql.twitch.tv`/`*.ttvnw.net`/`usher` → `twitch`), byte-identical across every failure **and** success site.
- **Factual error in the impl note:** it says retag `utils/http` at `http.go:59` where "`req.URL.Host` is already in scope." It is **not** — line 59 is inside `reportConnResult(failed bool)`, which takes no request. The host is only in scope at the callers (`FetchWithTimeout`, `http.go:84/88`); retagging requires threading it in or tagging at the call sites.
- **Single-service regression:** coarse `youtube`/`twitch` tags make `minTags>=2` **unreachable** for a YouTube-only install → the passive tracker can never trip for them. Today two *code-path* tags (`engine/fetch` + `monitor/feed`) do trip on a real full outage, so T6 removes a defense layer those users currently have, leaving the T1 active probe (with the false-ONLINE blind spots above) as their sole detector. Document this, or gate T6 on ≥2 services configured. **If the passive offline-trip is deleted per the first decision, T6 disappears and all of this is moot.**

### FLAP RISK — T1 raises it; the item that would damp it is deferred on a premise T1 weakens
T1 trades 3 *independent* neutral IPs for 2 *correlated* service hosts **and** adds DNS resolution as a new per-poll failure input (`probe.go:39`). On a marginal link (correlated CDN/DNS trouble, flapping egress filter, short-TTL misses on both names) the oracle can oscillate where the neutral triple held online. The online edge has **no up-debounce** (one good poll flips it, `monitor.go:189-193`); the offline edge is down-debounced 2 polls. Critically, the **Twitch offline edge is not a passive wait** — `OnStateChange(offline)` calls `callCancel()` (`orchestrator_twitch.go:100-105`), tearing down and rebuilding a fresh session each flap (~4 cancel/rebuild churns per minute at a ~15s flap period). The spec's "seamless waitOnline resume" framing is true for the YouTube HLS loop but wrong for Twitch. D5-damping is deferred "only if flapping is observed" on the premise it's "already rare" — but T1 is the change that makes it *less* rare. **Recommend an up-side debounce (≥2 good polls before the online edge) or coupling D5-damping to T1**, not deferring it behind observation. (The 2026-07-18 OS-accelerator addendum's pump/debounce/coalesce pattern is a cleaner structural answer to this same flap concern.)

### D7 CALLBACK-ORDERING RACE IS PRE-EXISTING, NOT D1-ONLY
`transition()` gates who-fires with an atomic Swap but delivers callbacks **outside any lock** (`monitor.go:202` gate, `225-227` fire), with three concurrent callers (`ReportSuccess`, `ReportFailure`, `poll`). Two opposite-direction transitions can both pass the `old != online` gate and deliver **out of order** — the offline edge landing after the online edge while `IsOnline()==false` — so subscribers (`orchestrator_twitch.go:100-105`, the notify closure) latch the wrong final edge. This is reachable in Tier 1 today (needs a `Report*` racing a poll edge; rare but real). D7's fix (single serialized notifier draining an ordered edge channel) is **Tier-1-eligible independent of D1.** Note: deleting the passive offline-trip (first decision) removes the `ReportFailure → transition(false)` caller, shrinking this race to poll-vs-`ReportSuccess` only.

### T3 IS THE WEAKEST TIER-1 ITEM — it manufactures its own coupling
`passive.go:121-128` documents that "every subsystem gates off its network I/O once we go offline … poll() is the only live path" — that's how the latch clears on idle. T3 deliberately breaks that by keeping monitors polling (and failing, and feeding the tracker) while offline — which is *why* it "would strengthen the wrongful cross-service latch" and forces T6 to co-ship. So **T3 introduces the regression T6 must repair.** Its upside is tiny: under T1 the oracle self-heals in ~5-10s; YouTube RSS has backfill + 10-min cadence (loss ≈ 0) and Twitch's own ~15s poll catches the next cycle. New drift to handle if T3 survives: `feed.go` `doCheck` now calls `BackfillSweep()` *after* the offline gate (`feed.go:417-423`), so a floor-pass would launch backfill scans mid-outage — gate the sweep online-only. **Recommend: cut T3, or demote to Tier-2 gated on evidence that streams are actually missed in the sub-10s window.**

### MIGRATION — value-equality is defensible, but name its real cost (critics split)
Two critics called for mirroring the codebase's anti-clobber precedent (the `network_access` migration checks raw-key *presence* before migrating, `config.go:200-208`) and only upgrading when `raw["connectivity"]["probe_targets"]` is **absent**. But the fact-check surfaces why the spec chose value-equality: the default triple is **force-persisted** — "since the encoder writes the full struct, any save persists defaults" (`config.go:169-171`), and the setup wizard saves — so for most users the key is *present* with the old triple even though they never chose it. **Raw-key-absence would therefore miss the majority and defeat the migration.** The genuine, unavoidable cost of value-equality is that it **cannot distinguish an explicit neutral-triple choice from a persisted default**, so the rare user who deliberately set `["1.1.1.1:443",…]` (e.g. a pihole that blocks youtube.com) gets clobbered, and a *partial* custom list (`["1.1.1.1:443","8.8.8.8:443"]`) is never `slices.Equal` → stranded on stale literals forever. Keep value-equality as pragmatically correct given force-persist, but (a) state the explicit-choice clobber plainly as a conscious tradeoff, (b) consider a **config schema-version bump** as the clean way to say "upgrade the persisted default once" without guessing intent, and (c) lean on the T5 log to make a stranded partial-custom list visible. Elevate to an owner call rather than a silent value-equality side effect.

### Smaller factual / line-number fixes (apply at pickup; structural claims survive)
- `services.go:153` → **`services.go:154`** (SetProbeTargets, still unconditional and pre-Start).
- `monitor_callbacks.go:584` → **`monitor_callbacks.go:693`** (feed-history insertions shifted it +109; comment text unchanged, T4 still valid).
- The advisory's "`api error: http`" body-contamination target is **misattributed** — that format is YouTube's and carries **no body**; the body-embedding formats are Twitch's `gql*` family only (`internal/twitch/api.go:225/231/244/246/252`).
- IPv6 fix is mis-cited as "Happy-Eyeballs v2 / RFC 8305" — the real mechanism is: **hostnames trigger DNS**, a DNS64 resolver's synthesized AAAA is used, and Go's dual-stack dialer (RFC 6555) connects. Reword.
- `ProbeCooldown` precedent for T3's per-monitor field exists on **feed/decapi only** — `TwitchMonitor` would get its first per-monitor stateful field (cosmetic).
- T1 safety ("in-progress loops take `waitOnline`") is true for **LIVE loops only**; the DASH transport-error path (status 0) rides out offline via infinite 2s retries, and **VOD paths have no oracle awareness** — oracle-offline there means a silent gap or a 3-retry hard error, not a pause. Scope the claim.
- T2 justification refinement: `404/410` on the probe paths are **endpoint-level** failures, not the stream-gone signal (stream-gone is in-band — YouTube `playabilityStatus`, Twitch `IsLive=false`); keeping them terminal preserves the pinned `probe_classify_test.go` behavior, which is the honest reason.

---

## 2026-07-18 ADDENDUM — OS-level offline accelerator (new candidate; event-driven local-down)

**What it is.** A new, additive layer that addresses a gap the whole Tier-1 design shares: for the case where the **local machine's own connection is down** (WiFi drop, cable unplug, adapter disabled, no default route), the active TCP probe detects it only *indirectly* (every dial fails) and *slowly* — up to one 5s poll boundary plus the 2-poll (~10s) debounce, so ~10–15s. The OS knows in **milliseconds**. This layer subscribes to OS link/route-change notifications and uses them as a fast-path offline signal. It is **strictly additive and negative-only** — it never declares *online*.

**Relationship to the rest of this doc.** Orthogonal to the "delete the passive offline-trip" decision (it's a different, more certain signal than the passive tracker) — do either, both, or neither. It's also a cleaner answer to the FLAP-RISK and single-service-latency concerns in the review addendum than the deferred Tier-2 machinery, and because it's negative-only it does **not** touch the false-online problem, so it composes with everything above.

### The negative-only contract (the core design — do not violate)
- An OS event **never directly sets online.** It triggers a re-evaluation of local ground truth: is a *usable* default route present, and is any non-loopback / non-APIPA interface up?
- **No usable default route / all interfaces down → go offline immediately, skip the 2-poll (~10s) debounce.** This is the one case with unambiguous local truth — no remote host to blame, no captive-portal / edge-5xx / DNS ambiguity.
- **Route reappears / link-up → do NOT declare online.** Instead *kick an immediate active probe* to confirm real reachability — this speeds the owner's prized instant-up recovery (vs. waiting for the next 5s poll) without trusting link-up as internet.
- The active TCP probe stays the **sole authority for "online."** OS link-up ≠ internet (ISP down, dead resolver, egress block, service outage are all invisible to the OS).
- **Caveat — "usable" is load-bearing.** VPNs and virtual adapters (Hyper-V / WSL / Docker / VirtualBox host-only) can leave a default route that goes nowhere, so "a default route exists" is only a *hint that leans online-ambiguous* → fall through to the probe. Only the strong negative — "no default route at all" or "the adapter owning the default route went down" — is the certain-offline trigger.

### Library decision (2026-07-18 research; sources fetched and verified, not recalled)
- **Complete off-the-shelf answer: `tailscale.com/net/netmon`** (BSD-3, pure-Go, event-driven on Windows + Linux incl. arm64 + more, and confirmed to do **no remote probing** — its internet-reachability logic lives in a separate `netcheck` package). **Rejected as a dependency:** importing it drags the entire `tailscale.com` module into `go.sum` and, since ~2025, mandates instantiating a `tailscale.com/util/eventbus.Bus` — a poor fit for Moombox's minimal-dep single-binary philosophy.
- **Recommended path — the lean pair (netmon's own guts, minus `tailscale.com`):**
  - *Windows:* `golang.zx2c4.com/wireguard/windows/tunnel/winipcfg` (MIT) — ergonomic wrappers over `NotifyRouteChange2` / `NotifyUnicastIpAddressChange` and `GetIPForwardTable2` / `GetAdaptersAddresses`. Its **leaf package compiles light** (only `x/sys/windows` + `net/netip` + `x/text`); the module's GUI siblings land in `go.sum` but are never compiled into the binary. (Alternative with zero new deps: bind `NotifyRouteChange2` yourself via `golang.org/x/sys/windows` — already in Moombox's graph — at the cost of hand-writing the `NewCallback`/handle-retention plumbing winipcfg gives you for free.)
  - *Linux:* `github.com/mdlayher/netlink` (MIT, pure-Go) for the `NETLINK_ROUTE` event socket; **hand-parse `/proc/net/route`** for the point-query to avoid even a route-parsing dependency.
  - *Cross-platform point-query alternative (no events):* `github.com/libp2p/go-netroute` (BSD-3, pure-Go; deps only `x/net` + `x/sys`, both already present) — it is essentially the pre-packaged version of netmon's two `defaultRoute()` functions.
- **Avoid (anti-pattern — reintroduces false-online):** `go-ole` `INetworkListManager` (NLM/NCSI does its own MSFT `connecttest` probe, un-retargetable) and `iamcalledrob/netstatus` (requires CGo, no Linux, and surfaces the OS's own remote-probe verdict).

### Two implementation scopes — pick by how much the latency matters
- **Scope A — full event subscription (sub-second local-down).** Adds `winipcfg` + `mdlayher/netlink` behind a small `linkState` interface (revives the `monitor_windows.go` / `monitor_linux.go` split the June redesign collapsed into the pure probe). ~200 lines we own and can test. Highest value, highest per-platform testing burden (you can't easily unit-test "cable unplugged").
- **Scope B — point-query on probe-failure (cheaper; ~80% of the value). RECOMMENDED starting point.** Keep today's poll loop untouched; the instant a probe round fails, call `go-netroute` once: **no default route → declare offline immediately (skip the debounce); route exists → remote/ambiguous, fall through to today's behavior.** One light already-present-ish dep, almost no new code, no per-platform callback plumbing or OS-thread-callback testing. Gives up the sub-second unplug detection of Scope A but captures the core certainty win (disambiguating local-down from remote-down). Start here; escalate to Scope A only if the latency actually bothers you.

### Patterns to crib from netmon (verified in source, files read 2026-07-18)
- **Three-goroutine core (this is the flap suppression the review flagged, ~40 lines):** `pump()` blocks on the OS event source, drops `ignore()`d messages, and does a **non-blocking send to a buffered-size-1 channel** (`select { case change <- x: default: }`) so a storm coalesces to one pending signal → `debounce()` reads one signal, does the work, then **unconditionally sleeps ~1s** before accepting the next. netmon's own comment names our exact scenario: *"roaming onto wifi will often generate multiple events in quick succession as interfaces flap … avoid spamming consumers."*
- **Equal-state short-circuit:** recompute the local snapshot on each event and **do nothing if it equals the cached state**; ignore transient `FlagRunning` / MTU churn — only up/down transitions and routable-IP changes count as real.
- **Windows OS-thread safety (three real gotchas):** the iphlpapi callback fires on a **Windows-owned thread** → do minimal work and `go` the handoff, never block it; guard with an `isActive()` check to drop events before `Start()` / after `Close()`; and keep a **long-period "no-deadlock" ticker** alive so the Go runtime — which is blind to Windows callbacks — doesn't think the program is deadlocked.
- **Linux netlink:** subscribe to **route AND address** groups (`RTMGRP_IPV4/6_IFADDR | RTMGRP_IPV4/6_ROUTE`) — a DHCP renewal changes the address without changing the route yet reachability changed; **fall back to polling** if the kernel rejects `RTMGRP` (e.g. gVisor/Cloud Run); filter normal link-up route churn (multicast / link-local dests in tables 254/255) so bringing a link up doesn't spuriously fire.
- **Point-query default route:** *Linux* — parse `/proc/net/route` for destination `00000000` with flags `RTF_UP|RTF_GATEWAY`; *Windows* — `GetIPForwardTable2` + `GetAdaptersAddresses`, pick the lowest-metric prefix-length-0 route on an `Up`, non-loopback interface. (`go-netroute` packages both.)
- **Sleep/wake backstop:** OS network events are unreliable across sleep/resume, so netmon also runs a cheap ~15s wall-clock-jump check (elapsed > 150% of the interval → probably just woke) that forces a re-check. Relevant for a desktop archiver that sleeps; Moombox's active probe already covers most of it, so this is optional.

### What to leave behind
netmon's rich `ChangeDelta` (PAC/proxy config, expensive-interface detection, socket-rebind heuristics), the `eventbus`/`Publisher` machinery, and all Tailscale-interface filtering. Moombox wants a **negative-only boolean**, not a delta.

**Reference (read, not vendored):** `tailscale.com/net/netmon` @ `main`, BSD-3-Clause — `netmon.go` (pump/debounce/coalesce), `netmon_windows.go`, `netmon_linux.go`, `interfaces_windows.go`, `interfaces_linux.go`.

---

Supersedes the round-1 revision. Incorporates all six round-2 perspectives. The key
structural change is **tiering**: a small, load-bearing set that fixes the real bugs with
low risk, and an evidence-gated cluster deferred because switching the probe target (T1)
removes the root cause the deferred items were compensating for.

## The load-bearing insight
> **[2026-07-17 correction — see addendum]** The "succeeds iff a real request could … the
> divergence is gone" claim below is overstated. A bare TCP handshake closes only the
> **false-OFFLINE** direction; false-ONLINE (captive portal, edge-5xx, DNS-poison, explicit-proxy)
> remains. Read the addendum's Correctness section before relying on this paragraph.

The round-1 stuck-offline (HIGH) required a **divergence** between the synthetic probe and
the real services: probe blocked, services reachable. **T1 makes the probe targets BE the
service hosts**, so `checkFn()` (a TCP handshake to `www.youtube.com:443` / `gql.twitch.tv:443`)
succeeds iff a real request to those hosts could — the divergence is gone. Consequently:
- The old motivating scenarios (IPv6-only, dead resolver, ISP null-routing 1.1.1.1, corp
  allowlist egress) were all artifacts of probing neutral IP literals; T1 dissolves them.
  **[2026-07-17 correction]** T1 dissolves the *firewall-allowlist* egress variant but NOT the
  *explicit-proxy* variant — the probe raw-dials, bypassing `http.ProxyFromEnvironment` that the
  real clients honor, so `HTTPS_PROXY`-mandated networks stay stuck-offline. See addendum.
- D1 ("real success authoritative") drops from a correctness fix to a ~5s speed optimization,
  and with it go the sawtooth (window-vs-floor) and the `transitionMu` re-entrancy hazard it
  induced. Not taking D1 is *more* stable, not just cheaper.

---

## TIER 1 — ship now (minimal sufficient; low risk, high value)

### T1. Probe targets → service hostnames + migration + single-source  (resolves #1/#2 + the #1-vs-#5 conflict)
- Default `["www.youtube.com:443", "gql.twitch.tv:443"]` — the **exact hosts the app dials**
  (privacy: RSS uses `www.youtube.com`, not apex `youtube.com`; matching them means probe
  reachability == fetch reachability, and respects a user's pihole/allowlist).
- Hostnames (not literals) → DNS is exercised (dead resolver correctly reads offline) and
  DNS64/CLAT synthesizes IPv6 on v6-only nets via Go's Happy-Eyeballs v2 (RFC 8305). Fixes
  the IPv6-only false-offline and the dead-resolver false-online.
- **Service-only — NO neutral fallback.** Decision rationale (network vs YAGNI conflict,
  resolved by Moombox's priority ordering): on a transient *both-services* blip while general
  internet is up, service-only correctly reads **offline** → in-progress download loops take
  the safe `waitOnline` branch and resume seamlessly; a neutral host would hold
  `IsOnline()==true` and risk **hard-erroring the live recording** via the download-loop hole.
  Protecting an in-progress recording > avoiding a bounded discovery pause. Single-service
  outages are unaffected (the other service host keeps the oracle online).
- **Config migration (MANDATORY — the omission every other perspective missed):**
  `ProbeTargets` is a persisted TOML field, so changing `DefaultProbeTargets` only reaches
  NEW installs. Add a non-destructive `migrateOldFormat` step: if the stored value deep-equals
  the old default `["1.1.1.1:443","8.8.8.8:443","9.9.9.9:443"]`, replace with the new default.
  Without it, the users most likely to hit the stuck-offline (corp/IPv6) are exactly the ones
  T1 never reaches.
- Collapse the duplicated default (`connectivity/probe.go:17` and `config/config.go:22`) to a
  single source so they can't drift as the value changes.
- Doc wording: DNS64 *synthesizes* the AAAA; CLAT *translates* packets — don't conflate.

### T2. Classifier → whitelist-of-terminal 4xx (not blacklist-of-transient)  (resolves #3 robustly)
Replace the `case strings.Contains(msg,"http 4"): return classServer` catch-all. Return
`classServer` ONLY for enumerated terminal codes `{400,404,405,406,410,411-417,422,426,431,451}`;
**every other 4xx → classNetwork**. Rationale: the classifier's own doctrine is "unknown →
classNetwork (safe)," but the catch-all made the *unsafe* direction the 4xx default — a
blacklist is un-completable (421 Misdirected Request already slips through → wrongful give-up;
449/460/407 lurk). Whitelist makes any unenumerated 4xx inherit the safe side while preserving
give-up exactly for 404/410 (the "stream gone" signal).
- Keep existing network-token cases (429/401/403/5xx/tls/timeout/reset/refused/no-such-host/eof).
- Tests: 408, 421, 425 → classNetwork; 404, 410, 451 → classServer; the real **Twitch** strings
  (`"gql rate limited (429) …"`, `"gql auth failure (401) …"`, `"gql http 503 …"`,
  `"gql http 404 …"`, transport-wrapped, `"gql exhausted …"`); and a case documenting that
  Twitch 429/401/403 currently land safe only via the asymmetric default.

### T3. Discovery-monitor offline floor  (defense-in-depth for the discovery asymmetry)
Give `FeedMonitor`/`DecapiMonitor`/`TwitchMonitor` `doCheck` the same floor `waitForLive`
already has: when `!IsOnline()`, still run one poll per `offlineDiscoveryFloor` instead of
hard-returning. Closes the architectural asymmetry (we already protect the job-wait loop but
leave discovery hard-skipping). **Works WITHOUT D1**: under T1 the active probe self-recovers
the oracle within one poll (~5s), so a floor success simply keeps discovery alive during the
~10-15s transient-offline window and feeds `ReportSuccess`. No window/floor sawtooth exists
here because there is no `recentSuccess` window competing with the floor (that only arises with
D1). Keep floor-path failure logging at Debug so a long real outage doesn't spam logs.

### T4. Fix the false-guarantee comment
`monitor_callbacks.go:584` claims transitions "can't flap-spam." The debounce is down-only;
correct the comment so no maintainer trusts a guarantee the code doesn't give.

### T5. Edge-triggered "why offline" diagnostic log  (OPERABILITY — not correctness; cheap)
On the offline transition only (not per-poll), log which probe targets failed and the age of
the last real service success — e.g. `connectivity lost: probe targets [www.youtube.com:443
gql.twitch.tv:443] all unreachable; last real success 47s ago`. It's the only way an operator
can distinguish "internet genuinely down (wait)" from "probe targets blocked but internet fine
(fix egress/allowlist/pihole)." Keep `probe_targets` config-file-only (a UI knob invites the
misconfiguration it would prevent; the log makes a bad value visible instead).
NOTE (concurrency): the "last real success" figure means T5 introduces ONE new field —
`lastSuccess atomic.Int64` (Store UnixNano on every `ReportSuccess`, Load on the offline
transition). In Tier 1 it is **log-only / decision-irrelevant**, so it is race-free and adds no
sawtooth/TOCTOU (it gains decision meaning only if D1 is later taken). So "Tier 1 adds no new
state" is corrected to "Tier 1 adds exactly one hazard-free log-only atomic."

### T6. Per-destination failure tags  (PROMOTED from Tier 2 — real bug, self-contained, no D1 coupling)
Tag passive-tracker failures/successes by **destination host** (`youtube` / `twitch`, derived
from `req.URL.Host`) instead of code-path (`utils/http`, `engine/fetch`, `probe/youtube`, …).
Fixes a real bug that bites the PRIMARY concurrent-YouTube+Twitch use case: today a YouTube-only
outage during an active YouTube download piles ≥5 failures across ≥2 *code-path* tags
(`engine/fetch` + `probe/youtube`) within 30s → `PassiveTracker` trips (`minFails=5,minTags=2`)
→ `nowOnline = online && !passiveOffline` goes false → **Twitch wrongly paused** though its host
is reachable. Per-destination tagging makes `minTags≥2` mean "two distinct *services* failing,"
so a single-service outage yields one tag (<2) → `ShouldTriggerOffline()` never latches → no
cross-service pause. This has ZERO coupling to D1's window machinery (host is already in scope at
every emission site; `FetchWithTimeout` even has the URL in hand but hardcodes `"utils/http"`),
adds no new concurrency surface (PassiveTracker already serializes tag access), and pairs
naturally with T1 (the dual-host active probe backstops full-internet outages directly).
Endorsed as the clean root fix by the concurrency, state-machine, and YAGNI perspectives, and
**co-shipping with T3 is required** (not optional): T3 keeps `doCheck` polling while offline,
which — with today's code-path tags — would *strengthen* the wrongful cross-service latch (a
YouTube outage keeps `monitor/feed` feeding the shared tracker instead of letting it age out).
Per-destination tags dissolve that interaction.
IMPL NOTE (completeness — from state-machine R3): switch BOTH failure and success emission sites
to the destination tag uniformly (else `ReportSuccess` won't clear the matching failure entries),
and apply it at **every** site, not just the four monitor/probe literals: `engine/fetch`
(`downloader_fetch.go:141` — derive from the job's platform / target URL host) and `utils/http`
(`http.go:59` — derive from `req.URL.Host`, which is already in scope). If those stay generic, a
downloading YouTube stream contributes a second tag alongside `youtube` and the latch re-trips.

Tier-1 net: fixes the reported bug class robustly, the IPv6/DNS/block reachability gaps, the
transient-4xx wrongful-error, the discovery blackout, AND the cross-service pause — with no new
state machine, no sawtooth, no deadlock, and exactly one hazard-free log-only atomic.

### Advisory implementation notes for Tier 1 (non-blocking; from round-3 confirmation)
- **T2 range codes:** implement `411-417` as explicit codes or a parsed integer — NOT
  `strings.Contains(msg,"http 41")`, which would wrongly swallow transient 418/419 into
  classServer.
- **T2 doc wording:** the prose "preserving give-up exactly for 404/410" undersells the broader
  enumerated terminal set — reconcile (the broad set is correct).
- **Twitch body contamination (pre-existing, Twitch-only, advisory):** Twitch error strings embed
  the response body, and the classifier substring-matches the whole message. Extract the status
  positionally (it follows `"gql http "` / `"api error: http "`) or thread a structured code,
  rather than scanning the body. Not a regression (identical exposure today); fix opportunistically.
- Doc wording: DNS64 *synthesizes* the AAAA; CLAT *translates* packets — don't conflate.

---

## TIER 2 — evidence-gated; do as a COUPLED set or not at all
These address real-but-narrower residuals. Taking any of the window-widening items (D1/D6)
pulls in the others, so treat as one unit.

### D6. Single-service outage must not trip GLOBAL offline  (real residual T1 does NOT fully fix)
Even under T1, a YouTube-only outage piles up failures across code-path-scoped tags
(`utils/http`, `engine/fetch`, `probe/youtube`) → passive tracker trips → `nowOnline =
probeOK && !passiveOffline` goes false → **Twitch wrongly paused**. Fix by **tagging failures
per-destination** (so `minTags≥2` means two distinct *services*), NOT by reusing a `lastSuccess`
window. Per-destination tagging is local to the emission sites, legible, and makes `minTags`
honest — the state-machine and YAGNI perspectives both prefer it over the clever coupling.
(NOTE: T6 promotes exactly this fix into Tier 1; D6 is retained here as the original framing and
the "why not the window" rationale.)

### D4. Download-loop service-vs-network classification  (pre-existing hole; separable)
`downloader_hls.go`'s `>5 consecutive failures` give-up trusts `IsOnline()` alone; when
`CheckStreamStatus` ALSO fails (a real service outage) it "assumes ended" and hard-errors a
LIVE recording. Constraints if implemented: classify on the **structured `plStatus` int**
`fetchSegment` already returns (NOT the body-bearing error string — that flips the cost model
and can make a dead download never give up), AND **preserve a hard give-up bound on the Twitch
path** (`EnforceMaxTimeout=false` means the counter is the only bound). Ship the anti-leak test
("a genuinely-dead variant still gives up") — its absence is how the leak would ship silently.
This bug is pre-existing (independent of the probe redesign); flag for owner scope decision.

### D1 + D7 (only if instant service-driven recovery is wanted beyond T1's ~5s poll recovery)
- D1: `lastSuccess atomic`; `nowOnline = (probeOK || recentSuccess) && !passiveOffline`;
  `ReportSuccess` recovers online when offline & passive-clear. `ReportSuccess` must fire only
  on genuine 2xx (never a captive-portal interstitial) or it can latch false-online.
- Interlock (mandatory if D1 taken): `serviceSuccessWindow ≥ offlineDiscoveryFloor + poll_debounce`
  and `coalesce_hold ≥ serviceSuccessWindow`. Concretely e.g. floor 20s, window 45s. As-written
  30s/60s sawtooths — do NOT ship those numbers.
- D7 (required with D1): serialize **decision + swap + callback-snapshot** under a mutex, then
  **release BEFORE invoking callbacks** (never hold the lock across callback delivery — callbacks
  may synchronously re-enter `Report*`→`transition` and would deadlock a non-reentrant mutex).
  Equivalent: a single serialized notifier goroutine draining an ordered edge channel.

### D5-damping. Consumer-side flap suppression (only if flapping is actually observed)
Coalesce "Connectivity Lost/Restored" notifications until state is stable ≥ the interlock hold.
Under Tier 1 (no D1) flapping is already rare (needs ≥2 consecutive bad polls then good on a
marginal link), so gate this on real evidence.

---

## CUT (do not build)
- **D5-part2** — rewriting the Twitch orchestrator to react to its own I/O instead of the oracle
  edge. Scope creep into working, deliberate download-session-cancel/seamless-resume control
  flow to defend a now-rare false edge. High blast radius, low payoff.
- **D8-atomic** — `SetProbeTargets` → `atomic.Pointer`. Hardens against a hot-reload caller that
  doesn't exist, against a documented init-before-Start contract. YAGNI.

## Opportunistic polish (optional, low priority)
- Thread the `Start()` seed probe's ctx + result into the first `poll()` (avoid the ~6s
  double-probe startup stall on a boot-offline host; make it cancellable).
- Minimal TLS/SNI handshake in the probe (catches SNI/DPI filtering; proves the edge serves our
  name) — gate behind the any-success race so a TLS quirk on one host can't sink the probe.

## Implementation-precision notes (from maintainability/codebase-fit R3 — make it build as written)
- **T1 migration is a NEW category for this codebase** (a *persisted-default upgrade*, not a
  legacy-key relocation). Moombox's `migrateOldFormat` never clobbers an explicit current-section
  value; this deliberately does. Implement guarded by value-equality and MUST set the persist flag:
  `if slices.Equal(cfg.Connectivity.ProbeTargets, oldProbeTargets) { cfg.Connectivity.ProbeTargets
  = DefaultProbeTargets; cfg.NeedsAutoPersist = true }`. The `NeedsAutoPersist = true` is
  essential — for the target population `raw["connectivity"]` is present, so the existing
  auto-persist check won't fire and the on-disk file would keep the old triple. Document it as a
  conscious default-upgrade. Validation needs no change (`net.SplitHostPort` accepts hostnames).
- **"Single-source the default" is a layering decision, not a one-liner** — `connectivity/probe.go`
  and `config` import each other in neither direction. Keep `config.DefaultProbeTargets`
  authoritative; leave connectivity's in-package fallback as a documented belt-and-suspenders
  (only reached if `SetProbeTargets` is never called — `services.go:153` always calls it), just
  update its value to match.
- **T3 floor state must be a per-monitor STRUCT FIELD, not a loop-local** — `doCheck` runs once
  per scheduled cycle (unlike `waitForLive`'s single long-running loop), so a local would reset
  each cycle. Add one field per monitor (like the existing `ProbeCooldown`) + a small shared
  `offlineFloorPasses(last *time.Time, floor)` helper to avoid re-triplicating the offline-skip
  block + a monitor-pkg `offlineDiscoveryFloor` const.
- **T5 needs the offline CAUSE plumbed in** — `transition(false)` is reached from three causes
  (probe-all-failed; passive latch in poll; passive latch via `ReportFailure`). On a passive-only
  trip the probe may still be succeeding, so a literal "probe targets all unreachable" message
  would be false. Pass a `reason` into `transition()` (or branch the message). The "age of last
  success" figure is the T5 `lastServiceSuccess atomic.Int64` (read-only in Tier 1).
- **No CLAUDE.md convention conflicts** — verified against logger interface, atomic.Pointer
  reporter pattern, UpdateJobFields, panic recovery, API prefix, web embedding. Tier 1 adds no
  goroutines, writes no jobs, changes no routes.
- **Suggested Tier-1 order:** T1a (default value + fallback sync) → T1b (migration + tests) →
  {T2 whitelist, T4 comment} in parallel → T5 (after T1) → T3 + T6 co-shipped (largest surface,
  last). D4 (Tier 2) requires an explicit owner scope decision — it's pre-existing live-recording
  data loss, mitigated (not fixed) by T1; if taken, classify on the structured `plStatus` int and
  preserve the Twitch hard give-up bound (`EnforceMaxTimeout=false`).

## Preserved owner decisions (unchanged)
Instant-up recovery / speed-first; pure-Go no-CGo probe; asymmetric-default classifier;
captive-portal body-probe out of scope; FULL sqlite durability; ~60Hz progress cadence.
