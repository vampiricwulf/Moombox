# Operations

## Scope

This document covers building, testing, releasing, updating, and running Moombox in production. It describes the build toolchain, CI pipeline, release process, self-update mechanism, launcher/supervisor pattern, shutdown sequence, notification dispatch, disk monitoring, and reference repository management. It is the authoritative reference for everything between writing code and running the binary.

## Rules and Constraints

- Build requires **Go 1.26** (see `go.mod` for exact patch version). Produces binaries for Windows x64, Linux x64, and Linux arm64 (cross-compiled via `GOOS`/`GOARCH` env vars; no CGo means the toolchain handles the rest transparently).
- **FFmpeg is required at runtime** — must be on PATH or configured via `cfg.Paths.FFmpegPath`. The first-run setup wizard validates FFmpeg availability and can install it via chocolatey or winget.
- **CI builds on tag push only** (tags matching `v*`). The workflow reads `RELEASE_NOTES.md` from the repository root for the GitHub release body.
- **Ed25519 signature verification is mandatory** before any binary swap during self-update. Updates without a valid `.sig` file are rejected.
- **Exit code 42** is the restart signal. The launcher process respawns the child when it exits with this code. All other exit codes propagate and terminate.
- **Version is set in `cmd/moombox/main.go`** as `var version = "x.y.z"`. CI overrides this via `-ldflags -X main.version=...` at build time.
- **Windows resource embedding** uses `go-winres` to generate `.syso` files at build time. These files are not committed to the repository.
- **CGO_ENABLED=0** — the build uses no C dependencies. This is enforced in CI and expected locally.
- The signing private key exists only as a GitHub Actions secret (`SIGNING_KEY`). It is never committed, logged, or embedded in the binary.

---

## Build

### Commands

```bash
go build ./...                                      # Build all packages (compile check)
go build -o moombox.exe ./cmd/moombox               # Build binary
go test ./...                                       # Run all tests
go test -v ./internal/engine/...                    # Single package
go test -v -run TestParseDash ./internal/engine/... # Single test
go vet ./...                                        # Static analysis
```

These commands default to the host OS and architecture. For cross-compilation (e.g., building Linux binaries from Windows), set `GOOS` and `GOARCH` explicitly — `CGO_ENABLED=0` means no C toolchain is required regardless of target platform. See BUILDING.md for per-platform build commands.

### BotGuard Sidecar Embed Prerequisites

Two embed blobs must be present in `internal/bgutils/embed/` before `go build` will succeed (the `//go:embed` directives in `internal/bgutils/embed/embed.go` reference files that don't exist on a fresh checkout):

```bash
# 1. Fetch + gzip the pinned Node.js binaries for all 3 platforms (~150 MB total).
go run ./tools/fetch-node                 # idempotent; skips on version match.

# 2. Build the JS sidecar payload (~3.5 MB tarball).
cd bgutil-sidecar
npm ci --omit=dev --ignore-scripts        # production deps only.
node build.mjs                            # writes ../internal/bgutils/embed/sidecar.tar.gz
cd ..

# 3. Now build Moombox normally.
go build -o moombox.exe ./cmd/moombox
```

CI runs steps 1 and 2 automatically (see `.github/workflows/release.yml`). For local builds, run them once after fresh checkout; subsequent `go build` calls reuse the embedded blobs until `version.txt` drifts (Node version bump or sidecar JS change).

The two embed sources are independent:
- `tools/fetch-node/main.go` is a Go tool that downloads the pinned Node release from `nodejs.org/dist/` for all three platforms (Windows x64, Linux x64, Linux arm64), SHA-256 verifies each against hardcoded constants in the source, gzips them to `internal/bgutils/embed/node-windows-amd64.gz`, `node-linux-amd64.gz`, and `node-linux-arm64.gz`, and updates `internal/bgutils/embed/version.txt` (committed file used as the cache-invalidation key for first-launch extraction). 5-minute HTTP timeout + 200 MB body cap per file.
- `bgutil-sidecar/build.mjs` is a Node.js script that `tar -czf` packages the production-only `node_modules/` + `src/server.js` + `package*.json` into `dist/sidecar.tar.gz` and copies the result to `internal/bgutils/embed/sidecar.tar.gz`. Build-time `tar` is required (system binary; available on Windows 10+, all Linux distros, and macOS) but the runtime extraction inside Moombox uses pure Go (`archive/tar` + `compress/gzip` from stdlib) — end users do NOT need a system tar.

To skip the sidecar entirely (smaller binary, but PO tokens fall back to websafe-only), set `[bgutils] use_sidecar = false` in `config.toml`. The embed blobs are still required at build time though — they're either present or the binary doesn't compile.

### Windows Resource Embedding

The executable embeds an icon and Windows version information via `.syso` files generated by [`go-winres`](https://github.com/tc-hib/go-winres).

- **Source metadata:** `cmd/moombox/winres/winres.json` (icon path, manifest, version info template)
- **Generated files:** `.syso` files in `cmd/moombox/` — created at build time, not committed to the repository
- **CI behavior:** The release workflow patches `winres.json` with the tag version and short commit hash before running `go-winres make --arch amd64`
- **Local builds with icon:**
  ```bash
  go install github.com/tc-hib/go-winres@latest
  cd cmd/moombox
  go-winres make
  cd ../..
  go build -o moombox.exe ./cmd/moombox
  ```
- **Local builds without icon:** Simply `go build -o moombox.exe ./cmd/moombox` — the absence of `.syso` files is not an error; the binary just lacks the embedded icon and version metadata.

### Runtime Dependency: FFmpeg

FFmpeg is the only external runtime dependency. It is used for:
- Muxing downloaded video + audio + chat segments into final output files
- Probing media metadata (duration, codecs)

Resolution order for the FFmpeg binary path:
1. `cfg.Paths.FFmpegPath` if explicitly configured
2. `ffmpeg` on `PATH`

The first-run setup wizard checks for FFmpeg and offers to install it via chocolatey (`choco install ffmpeg`) or winget. If FFmpeg is missing, Moombox can still start but download jobs will fail at the mux step.

### Docker Image

**Files:** `Dockerfile`, `docker/entrypoint.sh`, `docker-compose.yml`, `.dockerignore`
**Registry:** `ghcr.io/vampiricwulf/moombox` (published by `.github/workflows/docker-publish.yml`)

Three-stage build that runs the entire pipeline inside the image build — no host Go/Node toolchain needed:

1. **sidecar** (`node:22-bookworm-slim`): `npm ci --ignore-scripts` + `node build.mjs` → `sidecar.tar.gz` (mirrors release.yml).
2. **build** (`golang:1.26-bookworm`): `go run ./tools/fetch-node`, then `CGO_ENABLED=0` cross-compile for `$TARGETOS/$TARGETARCH`. Both stages run on `$BUILDPLATFORM`, so multi-arch builds don't emulate the compile.
3. **runtime** (`debian:bookworm-slim` + ffmpeg + ca-certificates + tzdata): must be glibc — the sidecar extracts an official nodejs.org Linux binary at runtime, and those are glibc-linked (Alpine/musl won't run it).

Container conventions:
- All state under a single `/data` volume (config, DB, logs, staging, output, sidecar cache — `HOME=/data` keeps the one-time sidecar extraction on the volume). `WORKDIR /data` so the binary's cwd-relative defaults land there too.
- `MOOMBOX_NO_TUI=1` baked in; the launcher/supervisor runs as usual and forwards SIGTERM for graceful `docker stop`.
- The entrypoint seeds `/data/config.toml` on first run, then execs the binary. The seed is mandatory, not cosmetic: with the `"localhost"` default the server binds `127.0.0.1` (unreachable through a published port), and the first-run web setup wizard cannot run either — `/api/setup/complete` is loopback-gated, and requests through Docker's bridge never appear as loopback. Seeding a config sets `ConfigLoaded`, which intentionally skips the wizard. An existing config is never touched.
- Seeded values (each with an explanatory comment in the generated file): `network_access = "lan"`, `port = 774` (paired with `EXPOSE` and the compose healthcheck), absolute `/data` paths, `cookie_file = "/data/cookies.txt"` (reached through the existing `./data` volume — the file must NOT be bind-mounted individually, because the periodic YouTube session write-back replaces it via temp-file + rename and a rename cannot replace a single-file bind mount), and `updates.auto_check_updates = false` — an in-app update swaps the binary inside the container and is silently reverted when the container is recreated from its image, so the update path is pulling a new image; the manual "Check for updates" button still works. Everything else keeps the binary's normal defaults (notably `use_sidecar = true` — the Debian runtime runs the glibc sidecar Node binary fine — and stdout logging, which `docker logs` picks up alongside the rotating `/data/moombox.log`).

**Cookies in a container.** `cookies.auto_enabled` stays **false** — the seed does not set it, and it should not be turned on. All it owns is a slow headless-browser refresh timer and one automatic browser attempt when auth fails, and this image ships no browser; it does NOT gate the profile import, and the in-process `RefreshService` that keeps an imported YouTube session alive runs regardless (see [data-and-storage.md](data-and-storage.md) § Cookies for exactly what the flag does and does not own). The designated workflow is: bind-mount a Firefox profile directory — the one holding `prefs.js` and `cookies.sqlite`, copied with Firefox closed and **with its `cookies.sqlite-wal` sidecar**, because recent cookies live in that sidecar and a copy without it reads as empty — at `/data/browser-profile`, then trigger the read yourself after each host-side refresh: `R F` in the TUI, shift+click the dashboard header's "Refresh cookies", or the Settings page's "Refresh cookies from browser profile" button. Those are one gesture with three affordances, all importing straight from the profile with no browser and over whatever `cookies.txt` already holds; on a phone or tablet use the Settings button, because shift+click needs a keyboard. First boot needs none of it — the automatic import runs on its own shortly after start, but **only when there is no `cookies.txt` to lose** (`automaticImportGuard`: absent, or present with zero rows; an unreadable file aborts rather than counting as absent). After that the profile is deliberately not re-read on a timer, because nothing inside the container changes it. `docker/entrypoint.sh` writes this same guidance into the generated config as comments, and that copy is the operator-facing reference wording.

**Re-authentication without touching the volume.** `POST /api/cookies/import` accepts a Netscape cookie file — pasted as `text/plain` or uploaded as a multipart `cookies` part, capped at 512 KiB — behind session auth, the CSRF middleware and the shared `heavy` limiter. It MERGES rather than replaces (a YouTube-only paste leaves the Twitch rows alone), writes through `writeFileAtomic`, reloads the jar, verifies live, and answers with the same three-state per-platform verdict the setup wizard's finish returns, so a signed-out export is reported at paste time rather than at the next members-only stream. It is allowed from **any authenticated client**, not loopback-gated: the setup wizard's gate protects an unclaimed instance, and a loopback-gated ingest would be useless in exactly the deployment this exists for. **There is no corresponding GET and there must never be one** — accepting credential bytes and never serving them is what keeps it from being an exfiltration path. Replacing the file on the volume still works and is still the faster path for anyone with a shell.

**IPv6-enabled compose network.** `docker-compose.yml` declares its `default` network with `enable_ipv6: true` and a ULA subnet. The reason is Moombox's `lan` seed: Docker's userland proxy accepts IPv6 connections to a published port and re-originates them from the bridge gateway's *private IPv4* address, so an internet IPv6 client would arrive looking like a LAN client and pass the `lan` filter. On Docker Engine 27+ — where ip6tables is enabled by default for IPv6-enabled networks — inbound IPv6 is DNATed to the container instead. Because Moombox binds IPv4 only, the effect is that those connections are **refused at the container**, not that the filter judges the real IPv6 client. Reach the dashboard over the host's IPv4 address.

Two limits. On Engine < 27, ip6tables is off by default, the userland proxy still handles IPv6, and the misclassification persists silently — nothing in-app can detect it. And a host with IPv6 disabled in-kernel (e.g. booted with `ipv6.disable=1`) fails to *create* the network at all: `docker compose up` errors rather than degrading. Recovery is to delete the `networks:` block, or set `enable_ipv6: false` and drop the `ipam:` subnet with it, accepting that the hole reopens on that host. The compose file carries both notes inline, and its comments are the reference wording. For the operator-facing version — VPN, reverse proxy, `trusted_proxies`, Docker Desktop, host-firewall bypass — see "Remote Access" in `README.md` and the Docker source-IP caveats in [security.md](security.md).

**No CI validation of `docker-compose.yml`.** `.github/workflows/docker-publish.yml` builds from the `Dockerfile` with `context: .`, and `.dockerignore` excludes `docker-compose.yml` from the build context entirely — CI never parses it. A compose-file change therefore ships unverified by the image-build gate; the real verification is `docker compose up -d` on a daemon-equipped host.

---

## Browser Cookie Acquisition (Platform Differences)

The interactive cookie setup and the headless refresh both launch a real browser, and both depend on a liveness primitive: a Job Object on Windows, a process group on Linux, nothing on darwin or the fallback build. What each platform can and cannot OBSERVE is stated here because the differences are recorded residuals, not oversights. The mechanism itself — launch args, extraction, the CDP ladder, the DPAPI fallback — is in [data-and-storage.md](data-and-storage.md) § Cookies.

**The reap keys on a liveness primitive reporting zero live processes, never on `cmd.Wait()`.** On Windows that primitive is a Job Object; on Linux it is the browser's own process group. A Firefox-family launcher hands off to the real browser and exits in ~170 ms (measured), so the process Moombox spawned exiting says nothing about whether the page loaded — closing the job at that moment killed the browser mid-load, and was the reason every Firefox-family refresh silently did nothing. `setupBrowserGone` (`internal/cookies/autocookies.go`) therefore returns TWO bools: whether the job is empty, and whether anything could say. `known == false` must be read as "still running", never as "gone"; the same launcher-exit is a normal mid-`FinishSetup` state, not evidence of abandonment.

**Windows counts a Job Object; Linux counts a process group; nothing else can say.** `processJob.queryable()` is `j != nil && j.handle != 0` in `job_windows.go` and `j != nil && j.group.queryable()` — that is, a process group was adopted — in `job_linux.go`. `configureCmdSysProcAttr` sets `Setpgid` on Linux, so every browser this package launches leads a group whose id is its own pid; `activeProcesses` counts that group's members by walking `/proc`, and `assign` REFUSES a process that did not lead its own group, because recording one that inherited Moombox's group would later point the kill at Moombox itself — in Docker, at the container. `job_other.go` is still unconditionally `false`: darwin and the fallback build have no primitive, `realSetupBrowserGone` always answers "not known" there, and a browser left open by an abandoned setup is not reaped. One Linux case answers "not known" too, honestly: a container whose `/proc` cannot be walked (a failed read is an error, never a zero — an empty table would read as "the group is empty", which is the answer that releases the slot). One answers WRONGLY: a browser that calls `setsid()` leaves the group, the count then reads the group as empty, and the reap releases the slot when the grace runs out with the browser still on screen — no kill, because the group it would signal has no members, but the operator's next finish answers `ErrNoSetupInProgress`. Which Linux packagings do that (a snap or flatpak wrapper is the suspect, not the browser binary) is unmeasured. A zombie is not a member: the `/proc` parser skips state `Z`, because a container where Moombox is PID 1 without an init (`docker-compose.yml` sets `init: true`; a bare `docker run` does not) never reaps an orphaned grandchild, and counted it would hold the group open for the life of the process. **The Linux reap is BUILT, NOT FIELD-VERIFIED.** Its decisions are unit-tested against a fake process table (`internal/cookies/job_pgroup_test.go`, which is why they live in a file with no build tag); nothing here has been run against a real Linux desktop or container, and a user's bug report is the gate. One named residual comes with it: a Job Object handle cannot name a process that is not in the job, but a pgid can, because the kernel recycles pids — `pgroupJob.killGroup` refuses to signal a group it cannot see or that has no members left, which narrows the window as far as it can be narrowed without closing it.

**`AbandonSetup` releases only where releasing cannot kill.** `POST /api/cookies/auto-setup/abandon` is what a CLIENT reports when the client itself went away — the dashboard tab unloaded — and it is deliberately not `/cancel`: a click is consent to close the browser, a tab unload is not, and the setup flow's own instructions send the operator AWAY from that tab to sign in. It asks `setupBrowserGone` for `known` and branches on it. Known (Windows, live job) → do nothing; the reap owns the slot on its own correct predicate, and releasing here would mean `cleanupLocked`, which closes a `KILL_ON_JOB_CLOSE` handle on the window the operator is typing a password into. Not known → release the slot, killing nothing, because nothing was tracking the browser anyway. Since the process-group reap landed, Linux takes the SAME declining arm as Windows wherever a group was adopted: the reap owns that slot. The release arm is now scoped to the cases where nothing can answer — a failed assign or an unreadable `/proc` on Linux, darwin and the fallback build — and it is still the only release there; the `cleanupLocked` it runs asks for a group kill on Linux, and that kill refuses a group it cannot see, so releasing still kills nothing. (A browser that left its group is the OTHER arm's problem: it reads as gone — see above.) The rule did not change; the set of platforms on each side of it did. The call is redundant wherever a group or a Job Object was adopted — Windows, and Linux since the process-group reap — and load-bearing wherever nothing was: darwin, the fallback build, and any Linux launch whose group could not be adopted. Neither arm is dead code on either platform; deleting the check would restore the kill on both.

**The grace window is priced against the clients, not guessed.** `setupAbandonGrace` is 60 s and exists for exactly one caller: a `FinishSetup` already in flight. Both clients cap one finish at 60 s — the Web dialog's `AbortController` and the TUI's `finishCtx` in `cmd/moombox/tui_wiring.go` — and `FinishSetup` re-stamps `setupRetainedSince` when it takes the slot, so a finish always gets a full window from the moment it started. Inside that window the BINDING server-side column is Chromium (~45.3 s: `cdpExtractTimeout` 30 s + `taskkillDrainDelay` 0.3 s + `authVerifyTimeout` 15 s), not Firefox (~17.3 s), so the real margin is ~14.7 s. Do not lower it to 30 s. The TUI wizard's own countdown is a separate number — 300 s, `cookieSetupCountdownSeconds` in `internal/tui/setup_wizard.go` — and on expiry it calls `AutoCookieService.CancelSetup` in-process, through `OnCancelAutoCookie` (wired at `cmd/moombox/tui_wiring.go`, no HTTP: the TUI shares this process). That is a cancel, not a finish, and the 60 s cap is scoped to one `FinishSetup`, so the two do not interact.

**The Linux launcher-handoff premise is UNMEASURED.** Mozilla documents the Firefox launcher process as a Windows feature, so "Linux Firefox hands off the same way" is an inherited claim rather than an observation, and several comments in `internal/cookies/` rest on it. Nothing depends on it being true — the process group counts and kills whatever is in it whether or not a handoff happened, and where nothing outlives the direct child the drain lands on lap zero exactly as before — but it should not be repeated as a fact. The settling experiment is one run of the drained-launch subtest on any Linux box.

**The drain's numbers are observations, not a healthy band.** `drainJob` (`internal/cookies/autocookies_firefox.go`) polls the job every 50 ms until it empties or the shared launch budget runs out. Recorded Windows passes: Waterfox 2.848 s over 53 polls and Firefox 1.734 s over 32 polls on 2026-08-25; a clean pass on different hardware on 2026-08-26 took 13.96 s over 276 polls with the same successful outcome and no error. The diagnostic is `errBrowserDrainTimeout` — returned when the budget expires with processes still alive — and nothing about the elapsed time or the poll count on its own. A failed count query degrades to the pre-drain behaviour (returns nil) rather than spinning on a failing syscall for the whole budget. LibreWolf and Zen remain unverified. So does every non-Windows target — but the reason changed on Linux: it now HAS a count (the process group), so `drainJob` waits there for anything that outlives the direct child — a wrapper's handoff, a straggling content process — up to the budget, where before it returned on lap zero regardless. Without a launcher the direct child is the browser itself and `cmd.Wait()` already waited for it, so where nothing outlives it the timing is unchanged. That is the Arc 0 fix arriving on Linux, and it is unobserved: no timing has been recorded on any Linux box.

**"Acted" is a separate question from "succeeded", and each engine answers it differently.** Success measured against `cookies.txt` is not evidence, because the independent 30-minute `RefreshService` keeps that file alive regardless — which is how a refresh whose browser was killed mid-load still logged "cookie refresh succeeded". `browserActed` (`refreshCookiesDetailed`, `internal/cookies/autocookies.go`) starts TRUE and only the browser branch may clear it. That scoping is deliberate: the profile-import path and the empty-profile fallback launch no browser, so there is no browser inaction to detect, and gating them here would make every containerised import report a refresh that never renews, permanently, on every restart.

- **Firefox** — a per-launch `--screenshot` artifact. `browserLaunchActed` requires no launch error AND a non-empty screenshot, and both halves cover failures the other cannot see: the error covers a browser killed mid-load or cancelled, the screenshot covers every failure that returns NO error (a failed job query, a failed assign, a launcher handoff nothing waited for). The artifact is cleared before every launch, because one platform's screenshot is otherwise indistinguishable from the next platform's. A launch that already reported acted but finished under 1 s gets a Debug note and nothing more.
- **Chromium** — the navigation fold, a weaker signal than the screenshot and the one this path can produce. `navigateAllPlatforms` ANDs the per-platform outcomes, and `errNavigateBudgetExhausted` (a navigation whose read loop never saw `Page.loadEventFired`) counts as NOT OBSERVED rather than as a failure: it never joins the failure list, and it flips the pass's answer only when it was EVERY platform's outcome. One platform timing out beside a sibling that fired its load event is a slow page on a browser that demonstrably works — do not loosen "every" to "any".

A launch that cannot be confirmed is logged as exactly that, "could not be confirmed", and no more: with no job to drain, a detached browser may render moments after the check, so asserting either outcome would be unearned.

**The DPAPI fallback cannot read an App-Bound (v20) cookie store, and that is a capability limit rather than a bug.** Chrome 127+ tags a cookie value with a `v20` prefix whose key is held by a SYSTEM-level elevation service and is not recoverable with a plain CURRENT_USER `CryptUnprotectData`. `internal/cookies/dpapi/dpapi.go` recognises the prefix precisely so the failure can say WHY nothing came out — those rows are counted as `ErrAppBoundEncryption` rather than silently dropped, which is the difference between "this fallback cannot work on your browser at all", "this profile is not yours" and "you are simply not signed in". Detection is prefix-based, never browser-name-based, so it applies to whichever browser writes a v20 value. **Brave appears to be one of them, and the older claim that Brave kept v10 and still worked was wrong.** The hedge on that correction is the code comment's own and is kept here verbatim in substance: as of 2026-08-29, PER THIRD-PARTY SECURITY RESEARCH — not Brave's own changelog, and not independently cross-confirmed — Brave registers an elevation service of its own, which a browser with no App-Bound-style key service would have no reason to do. Treat "Brave has moved to v20" as the current best guess rather than an established fact, and re-verify before it drives anything more consequential than this fallback's error message. Upstream settles nothing either way: `references/yt-dlp/yt_dlp/cookies.py` has no App-Bound handling at all — zero matches for `v20`, `app_bound` or `elevat` — and its Windows model is documented as "cookies are either v10 or not v10" with "not v10: encrypted with DPAPI", so it can neither confirm nor deny which browsers use v20. Any operator-facing copy that puts "Brave" and "DPAPI fallback" in one sentence should be re-checked against that comment before it ships.

**A refresh can decline with a NIL error, and there are exactly three causes an operator can meet.** `cookies.RefreshDeclinedCauses` is the shared copy for that — a setup or another refresh already in flight, or no platform with cookies worth refreshing. A fourth decline exists in the code, the `stopped` latch after `Stop()`, and is deliberately left out: the only way to reach it is a "Refresh now" that raced process shutdown, so by the time the toast rendered the service it describes is gone, and naming it would put an unactionable cause in front of every operator who hits one of the real three. The invariant is exhaustiveness over the declines a RUNNING service can produce. Three surfaces render the constant (the worker's log line, the TUI's `R F` feedback, the Web toast) and the Web copy is pinned against it by a test, since `app.js` cannot import it. `Ran` separates a pass that declined (never looked) from one that started and aborted (looked, learned nothing); `Renewed` is `importedFromProfile || browserActed`.

---

## Memory Limits

Moombox bounds steady-state memory for both the Go process and the embedded BotGuard sidecar. Configured via `[memory]` in `config.toml`:

```toml
[memory]
go_soft_limit_mb = 256        # Go runtime soft cap (debug.SetMemoryLimit)
sidecar_soft_limit_mb = 200   # RSS threshold to fire sidecar GC
sidecar_hard_limit_mb = 512   # V8 --max-old-space-size for the sidecar
```

### Go Process

`debug.SetMemoryLimit(GoSoftLimitMB << 20)` is applied at startup. It is a *soft* limit: as the heap approaches the cap, Go's GC runs more aggressively and returns memory to the OS more eagerly. Allocations that genuinely need more memory still succeed beyond the cap — the runtime never OOM-aborts on this knob alone. Setting `0` disables the call (Go uses its default unbounded behaviour).

Default 256 MB sits ~10% above the observed p99 active-download Sys (~238 MB), so GC fires only on real growth, not on routine streaming work.

### Sidecar (V8)

V8 has no soft-limit primitive, so the sidecar combines a hard ceiling with proactive GC triggers:

1. **Hard ceiling**: launched with `--max-old-space-size=<SidecarHardLimitMB>`. Hitting this OOM-aborts the sidecar (V8 has no graceful soft stop). The launcher auto-respawns it on the next call, but in-flight requests get errors. Set `0` to use V8's default (~512–1500 MB depending on host).
2. **Proactive GC**: launched with `--expose-gc`. The 2-minute memory-log loop in `cmd/moombox/main.go` reads sidecar RSS via `Sidecar.MemoryStats()` and, when RSS exceeds `SidecarSoftLimitMB`, fires `Sidecar.TriggerGC()` (a `triggerGC` JSON-RPC method that runs `globalThis.gc()` and returns before/after stats). Set `0` to disable.

Default 200 MB soft / 512 MB hard. The soft sits above the post-mint plateau (~150 MB) so GC fires only when something is genuinely growing; the hard sits above the historical peak observed during BotGuard processing bursts (~544 MB during the leak that prompted these limits, ~400-500 MB normally) so legitimate work doesn't trigger an OOM.

### Tuning

Lower the soft caps to trade CPU for memory; raise them when GC pressure becomes visible (look for `[Memory] sidecar soft limit hit; ran GC` log lines repeatedly reclaiming little). The hard sidecar cap should always sit above the soft cap by a comfortable margin — 2x is a good rule of thumb. Setting the hard cap below observed BotGuard peaks (~500 MB) will manifest as the sidecar process restarting under load.

---

## CI Pipeline

### Workflow

**File:** `.github/workflows/release.yml`
**Trigger:** Tag push matching `v*` (e.g., `v2.6.3`)
**Runner:** `ubuntu-latest` (single job; cross-compiles all platforms from Linux)
**Permissions:** `contents: write` (to create releases and upload assets)

### Steps

1. **Checkout** — `actions/checkout@v6`
2. **Restore embed blob cache** — `actions/cache@v5` keyed by hash of `version.txt` + sidecar `package-lock.json` + `build.mjs` + `tools/fetch-node/main.go`. On cache hit, the sidecar build and Node fetch are skipped entirely (~55s saved). Cache evicts after 7 days of disuse.
3. **Set up Go** — `actions/setup-go@v6` with version from `go.mod`
4. **Set up Node** — `actions/setup-node@v6` (only when cache missed)
5. **Build BotGuard sidecar payload** — `npm ci --omit=dev --ignore-scripts && node build.mjs` (only when cache missed)
6. **Fetch embedded Node binaries** — `go run ./tools/fetch-node` — downloads pinned Node v22 LTS for all 3 platforms, SHA-256 verifies, gzips to per-platform embed files (only when cache missed)
7. **Generate Windows resources** — Patches `winres.json` with tag version + commit hash via `jq`, runs `go-winres make --arch amd64` in `cmd/moombox/`. `go-winres` runs on any host OS; the resulting `.syso` uses filename build constraints so it's included only under `GOOS=windows`.
8. **Compute version + ldflags** — Exports `VERSION`, `COMMIT`, `LDFLAGS` to `$GITHUB_ENV` once so all per-binary steps reference the same values.
9. **Build Moombox.exe** — `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "$LDFLAGS"`
10. **Build moombox-linux-amd64** — `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$LDFLAGS"`
11. **Build moombox-linux-arm64** — `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$LDFLAGS"`
12. **Sign Moombox.exe** — `go run ./cmd/sign Moombox.exe` → `Moombox.exe.sig`
13. **Sign moombox-linux-amd64** → `moombox-linux-amd64.sig`
14. **Sign moombox-linux-arm64** → `moombox-linux-arm64.sig`
15. **Build release body** — If `RELEASE_NOTES.md` exists and is non-empty, prepends three download links and uses the file as the release body. Otherwise, falls back to GitHub's auto-generated release notes.
16. **Create GitHub Release** — `softprops/action-gh-release@v3` with body from step 15 and 6 assets: `Moombox.exe` + `.sig`, `moombox-linux-amd64` + `.sig`, `moombox-linux-arm64` + `.sig`. Tags containing `-` (e.g. `-rc.1`, `-test.1`) are marked as pre-releases.

Steps 9–11 are sequential (not parallel). On a 4-vCPU runner each `go build` saturates the CPU, so concurrent builds contend for cores and re-download every module dep three times. Sequential is faster end-to-end; the first build also warms the module cache for the next two.

### Release Body Format

When `RELEASE_NOTES.md` is present, the release body is assembled as:

```
[**`Download Moombox.exe for Windows (x64)`**](download-url)
[**`Download moombox-linux-amd64 for Linux (x64)`**](download-url)
[**`Download moombox-linux-arm64 for Linux (arm64)`**](download-url)

---

<contents of RELEASE_NOTES.md>
```

### Docker Publish Workflow

**File:** `.github/workflows/docker-publish.yml`
**Trigger:** Same `v*` tag push as release.yml (runs in parallel with it), plus `workflow_dispatch` for testing image changes without cutting a release.
**Permissions:** `contents: read`, `packages: write`

Builds the multi-arch (linux/amd64 + linux/arm64) image via buildx and pushes to `ghcr.io/vampiricwulf/moombox`. Release tags produce `X.Y.Z`, `X.Y`, and `latest` (pre-release tags containing `-` skip `latest`, matching release.yml's pre-release handling); manual dispatch on `main` produces `edge`. `VERSION`/`COMMIT` build args mirror release.yml's ldflags. QEMU is only used for the small Debian runtime stage of the arm64 image — the Go compile cross-compiles natively.

---

## Release Process

This is the manual process performed by the developer before CI takes over:

1. **Generate `RELEASE_NOTES.md`** — Run `git log --oneline <prev-tag>..HEAD` and group commits into sections: Features, Improvements, Bug Fixes, Internal. Omit empty sections. No top-level heading in the file.
2. **Bump version** — Edit `cmd/moombox/main.go`, change `version = "x.y.z"` to the new version.
3. **Commit both together** — `chore: bump version to x.y.z — short summary`
4. **Tag** — `git tag vx.y.z`
5. **Push** — `git push && git push origin vx.y.z`

The tag push triggers CI, which builds, signs, and publishes the release.

### Version Format

Semver: `MAJOR.MINOR.PATCH`. The `v` prefix is used only on git tags (`v2.3.20`). The `version` variable in source code stores it without the prefix (`2.3.20`). The `init()` function strips any `v` prefix in case ldflags includes it.

The `commit` variable is resolved at build time via ldflags, or at runtime from `debug.ReadBuildInfo()` if ldflags are absent (local development builds).

---

## Self-Update Flow

The self-updater lives in `internal/updater/`. It checks GitHub Releases, downloads the new binary, verifies its signature, and replaces the running executable.

### Step-by-Step

1. **Check** (`updater.go: CheckForUpdate`) — Queries `https://api.github.com/repos/vampiricwulf/Moombox/releases/latest`. Compares the remote version against the current version using semver comparison. Returns `nil` if already up-to-date, or a `ReleaseInfo` struct if a newer version exists. HTTP timeout: 10 seconds.

2. **Download binary** (`updater.go: ApplyUpdate`) — Downloads `Moombox.exe` from the release assets to `<exe-path>.new`. HTTP timeout: 5 minutes (separate client from the 10-second API client, since binaries are 10-30 MB).

3. **Download signature** — Downloads `Moombox.exe.sig` to `<exe-path>.new.sig`.

4. **Verify** (`signing.go: VerifySignature`) — Reads the `.new` binary and `.new.sig` file. Verifies using the **embedded Ed25519 public key** (`71ce2f926296a552950faa1fd7d3e89574e14ec353aa253f2577f6883fdf51eb`). Signature must be exactly 64 bytes. On failure: `.new` and `.new.sig` files are deleted, error is returned to the caller.

5. **Replace** (3-step rename dance):
   - Remove stale `.old` file if it exists
   - `current.exe` -> `current.exe.old` (rename running binary out of the way)
   - `current.exe.new` -> `current.exe` (place new binary)
   - If the second rename fails: attempts rollback by renaming `.old` back to the current path. If rollback also fails, logs an error (binary may be in an inconsistent state).

6. **Breadcrumb** — After a successful swap, `ApplyUpdate` writes `<exe-path>.update-pending` containing the target release tag. The next boot resolves it: a boot running that version deletes it (update landed); a boot running a *different* version alongside a failed-update marker (the launcher auto-rolled back — see below) records the tag as `updates.skipped_version` so automatic checks stop offering the broken release (a manual "Check for updates" still retries it deliberately), then deletes it. Binaries that predate the breadcrumb ignore it; stale copies are inert until the next aware boot cleans them up.

7. **Restart** — The caller invokes `triggerRestart("update")`, which exits with code 42. The launcher respawns, picking up the new binary.

8. **Cleanup** (`updater.go: CleanupOldBinary`) — Called at the first-successful-boot milestone (database opened, web bind resolved). Removes stale `.old`, `.new`, and `.new.sig` files left by previous updates or interrupted downloads.

### Automatic Rollback

If the **first boot of a freshly-applied update** fails within `postUpdateFailureWindow` (2 minutes) — or the new binary fails to even start — the launcher rolls back automatically (`launcher.go: attemptAutoRollback`): the broken binary is removed (it is bit-identical to the published GitHub asset, so nothing is lost), the preserved rollback artifact (`~` on Windows, `.old` on Linux) is renamed back to the plain name, an `.update-failed` marker documents the rollback, and the restored binary is respawned as a fresh launch. The next boot announces the marker as a notification and — via the `.update-pending` breadcrumb — marks the failed version skipped. Rollback ping-pong is impossible: the restored binary is not "first after update", so a quick death of it takes the normal fail-fast path. When the artifact is already gone (the boot survived to the milestone sweep before dying) or the restore itself fails, the launcher falls back to preserving what remains with written manual-recovery instructions (`preserveUpdateRollback`).

### Signature Verification of Current Binary

`VerifyCurrentSignature` allows verifying the running binary against its published signature. It fetches the `.sig` file for the current version's tag from GitHub, downloads it to a temp file, and verifies against the running executable. This is used for integrity checks (e.g., verifying the binary hasn't been tampered with post-install). Returns an error for local/dev builds that have no corresponding GitHub release.

### Error Handling

| Scenario | Behavior |
|----------|----------|
| GitHub API unreachable | Error returned, no update attempted |
| No `Moombox.exe` asset in release | Error returned ("no Moombox.exe asset found") |
| No `.sig` asset in release | Error returned ("no signature file found") |
| Download fails | `.new` file cleaned up, error returned |
| Signature verification fails | `.new` and `.new.sig` cleaned up, error returned |
| Rename of current binary fails | `.new` cleaned up, error returned |
| Rename of `.new` to current fails | Rollback attempted (`.old` -> current), error returned |

---

## Launcher/Supervisor Pattern

### Purpose

The launcher enables graceful restarts without process chain buildup. When Moombox needs to restart (config change, update applied, setup wizard completion), it exits with code 42 and the launcher respawns it. This picks up any new binary on disk (for updates) and gives the child a clean process state.

### Mechanism

**Environment variable:** `_MOOMBOX_CHILD`

- **Parent process** (no `_MOOMBOX_CHILD` in environment): Runs `launchAndSupervise()`. Spawns itself as a child process with `_MOOMBOX_CHILD=1` and waits.
- **Child process** (`_MOOMBOX_CHILD=1`): Runs the full application service stack.

**Parent behavior on child exit:**
- Exit code 42: Respawn the child (loop continues). If `<exe>.old` exists (from an update), rename it to `<exe>~` to free the `.old` name for future updates.
- Any other exit code: Propagate the exit code and terminate.
- Normal exit (code 0): Terminate.

**Windows-specific details:**
- The parent ignores `os.Interrupt` (Ctrl+C) — the child handles signals.
- The `createNoWindow` flag (`0x08000000`) is used when spawning cleanup processes, not the child itself. The child inherits the parent's console (stdin/stdout/stderr are piped through).

**Old binary cleanup:** After an update, the old binary is at `<exe>.old` but is locked because the launcher (parent) is still running from the old binary. The launcher renames `.old` -> `<exe>~`. On exit, the launcher spawns a detached `cmd /C ping 127.0.0.1 -n 3 >nul & del /f /q <exe>~` process to delete the stale file after the launcher fully exits.

### Restart Triggers

All restart triggers call `triggerRestart(source)`, which:
1. Logs `"Restart requested"` with the source string
2. Sets `restartRequested.Store(true)` (atomic bool)
3. Calls `cancel()` to cancel the root context (propagates to all services)
4. Calls `quitTUI()` if the TUI is running

| Source | Trigger |
|--------|---------|
| `"API"` | `POST /api/restart` (gated by `network_access` + CSRF + auth) |
| `"setup"` | Setup wizard completion |
| `"update"` | Update applied via API |
| `"TUI settings"` | Config change saved from TUI |
| `"TUI update"` | Update applied from TUI |
| `"TUI setup wizard"` | Setup wizard completed from TUI |

---

## Shutdown Sequence

Shutdown is triggered by context cancellation (from signal handler, restart trigger, or TUI quit). The sequence is ordered to stop consumers first, flush data, and tear down infrastructure last.

### Order

1. **Stop monitors** — TwitchMonitor, DecapiMonitor, FeedMonitor (prevents new job creation)
2. **Stop download worker** — Waits for active downloads to save state (resume files)
3. **Flush notifications** — `notifyMgr.Wait()` blocks until all in-flight notification goroutines complete
4. **Stop cookie services** — CookieRefresh, AutoCookies
5. **Cleanup PO token provider** — Releases Goja VMs
6. **Stop web server** — Closes HTTP listener and WebSocket connections
7. **Unsubscribe event listeners** — Log forwarder, WebSocket job update subscribers
8. **Close database** — Flushes pending writes, closes SQLite connection

Each service stop is wrapped in `stopService(name, fn)` which provides:
- Panic recovery (one failing service cannot block shutdown of others)
- Debug logging of each service stop

### Force-Exit Timer

A 10-second `time.AfterFunc` timer starts at shutdown entry. If graceful shutdown has not completed within 10 seconds, the timer fires:
1. Logs `"Graceful shutdown timed out, forcing exit"`
2. Calls `log.Close()` to flush buffered logs
3. Calls `os.Exit(1)`

After graceful shutdown completes, if `restartRequested` is true, the child process exits with code 42 (triggering launcher respawn). Otherwise, it exits with code 0.

---

## Signing Tool

**Location:** `cmd/sign/main.go`

A standalone CLI tool used exclusively by CI to sign release binaries.

### Usage

```bash
# Sign a binary (reads SIGNING_KEY from environment)
go run ./cmd/sign Moombox.exe
# Output: Moombox.exe.sig (raw 64-byte Ed25519 signature)

# Generate a new key pair (one-time setup)
go run ./cmd/sign -genkey
# Output: public key (for embedding in signing.go) + private key (for GitHub secret)
```

### Details

- **Algorithm:** Ed25519 (deterministic, no randomness needed at sign time)
- **Private key source:** `SIGNING_KEY` environment variable (hex-encoded, 128 hex chars / 64 bytes)
- **Output:** `<input-path>.sig` containing the raw 64-byte signature
- **Public key location:** Embedded in `internal/updater/signing.go` as `updatePublicKeyHex`
- **Key management:** Private key stored as a GitHub Actions secret. Never committed, never logged. The `-genkey` subcommand generates a fresh key pair for initial setup or rotation.

---

## Notifications (Discord Webhooks)

### Configuration

Notifications are configured in the TOML config as an array of notification targets, each with a URL and optional event filter.

### URL Formats

| Format | Example | Behavior |
|--------|---------|----------|
| Full HTTPS | `https://discord.com/api/webhooks/123/abc` | Used directly |
| Shorthand | `discord://123/abc` | Expanded to `https://discord.com/api/webhooks/123/abc` |

URL validation rejects non-HTTPS Discord webhook URLs and URLs with invalid ID/token structure. Unsupported URL schemes are logged as warnings and skipped.

### Event Types

These are the event strings used for filtering. A target with no event filter receives all events.

| Event | When Fired |
|-------|------------|
| `found` | Monitor detects a new stream/video |
| `added` | Job manually added (API or CLI) |
| `scheduled` | Upcoming stream detected with scheduled start time |
| `rescheduled` | Stream scheduled start time changed |
| `downloading` | Download begins or resumes |
| `muxing` | FFmpeg mux step begins |
| `finished` | Job completed successfully |
| `error` | Job failed |
| `cancelled` | Job cancelled by user |
| `auth` | Any credential problem or recovery — cookies expired, member-only content, refresh failure, COOKIES? jobs resumed, Twitch chat downgraded to anonymous. See **Credential Notifications** below for the full set |
| `quality_split` | Stream quality changed mid-download; previous part closed |
| `gap_split` | Twitch live segments expired unrecoverably; part closed, new part at live edge |
| `connectivity_pause` | Twitch live download paused — connectivity lost, waiting to resume |
| `connectivity_resume` | Connectivity restored; same job resumed |
| `connectivity_split` | Broadcast/VOD lost during the outage; captured data finalized |
| `connectivity_restored` | Global connectivity restored — fires the "Outage Alert": start/end as Discord dynamic timestamps plus the duration. Deliberately the ONLY global-outage event: a lost-connectivity webhook has no connectivity to deliver over, so there is no `connectivity_lost` (removed in v2.8; stale filter entries warn at startup and strip on the next UI save) |
| `trim_created` | Trim clip created |
| `trim_deleted` | Trim clip deleted |
| `trim_error` | Trim operation failed |
| `disk_warning` | Disk usage exceeds warning threshold (also fired for monitoring-read failures) |
| `disk_critical` | Disk usage exceeds critical threshold (targets filtering on `disk_warning` also receive it, via the manager's event alias) |
| `update_available` | New version detected |
| `update_applied` | Moombox restarted on a different version than the previous run (embed reports whether the web dashboard came back) |
| `update_failed` | A failed-update marker (`.update-broken` / `.update-failed`) was found at boot — manual attention needed |
| `crash_recovered` | The launcher respawned Moombox after an abnormal exit |
| `channel_unhealthy` | A monitored channel failed a sustained streak of checks on EVERY monitor covering it (renamed/banned/misconfigured) — its streams are being missed. Cross-monitor confirmed: a YouTube channel still reachable via DECAPI while its RSS feed 404s during peak hours does NOT fire (avoids the false positive). |

The canonical event vocabulary is `notifications.EventGroups`
(internal/notifications/events.go). The TUI filter editor derives from it
directly; the web UI keeps a labeled mirror (`NOTIFICATION_EVENT_GROUPS` in
web/public/modules/settings.js) that MUST be updated in lockstep. Filtered
targets treat the vocabulary as an allowlist, the TUI's edit-save path
strips unknown events from hand-edited configs, and `NewManager` logs a
warning for any configured filter entry outside the vocabulary.

### Credential Notifications

Every notification below carries `Event: "auth"`, so one filter entry covers the family. An empty `Event` would bypass every target's allowlist — the filter applies only when `Event != ""` — which is why none of them omits it.

| Title | Type | Fires from | What it asserts |
|-------|------|-----------|-----------------|
| Cookie Re-Authentication Required | Error | `handleRecoveryNeeded` (`cmd/moombox/monitor_callbacks.go`), `auto_enabled` off | A platform answered a conclusive not-authenticated and nothing automatic will attempt to restore it. Claims nothing about a refresh, because none ran, and does NOT claim cookies are present — the file may have been deleted outright |
| Cookie Auto-Refresh Failed | Error | `runCookieRecovery`, the pass returned an error **or** came back with a conclusive `RefreshFailed` verdict and no error | The automatic refresh ran and failed |
| Cookie File Unreadable | Error | `runCookieRecovery`, `ErrCookieFileUnreadable` | The existing `cookies.txt` could not be READ, so nothing was written to it. Its own copy rather than the generic one, because the generic text would tell the operator to overwrite the one file Moombox deliberately did not touch — it may hold a working credential for another platform. The remedy named is the filesystem or mount, and Moombox retries on its own |
| Cookie Auto-Refresh Ineffective | Warning | `runCookieRecovery`, verdict neither OK nor a conclusive failure | The refresh ran, did not restore auth, and could not establish why. Asserts no cause |
| Authentication Required | Warning | `internal/worker/worker.go`, a job parked in `COOKIES?` | This job needs credentials it does not have |
| Authentication Recovered | Info | `OnAuthRecovered` | N jobs parked on this platform's cookies were resumed. This edge also does one more thing on Twitch and says nothing about it: `OnAuthRecovered` also tells every live Twitch chat session to reconnect (`ReauthenticateTwitchChats` counts downloaders told, never sessions authenticated), because a transient refusal heals with the credential fingerprint UNCHANGED and so fires no `OnCredentialsChanged`. The notification still reports only the jobs — the chat reconnect is a log line (`twitch credentials usable again`), not an operator decision |
| Parked Jobs Re-evaluated | Info | `OnCredentialsChanged` | N parked jobs were resumed after the platform's saved credentials were re-observed. States no cause on purpose: this fires on the first authenticated observation of EVERY process, not only on a real change, so "a different account was supplied" would often be false. It also fires for Twitch, whose credential is a bearer token and a login name rather than an account — which is why the wording says "saved credentials" |
| Twitch chat is anonymous for {channel} | Warning | `sendTwitchChatDowngrade` (`internal/worker/stream_processor_twitch.go`) | A job that HAD Twitch credentials is capturing chat anonymously. Warning rather than Error because nothing failed — this capture is fine and the NEXT one starts anonymous. Fields: Channel, Job, Reason (one of the four chat-handshake `AuthDowngrade*` tokens, never a credential — the fifth, playback-token route, never reaches this notice). The SAME report also marks the platform (`NoteTwitchAuthLoss`), which is what fires "Cookie Re-Authentication Required" or the one automatic recovery attempt above — so an operator with `auto_enabled` off can receive both, one naming the job and one naming the platform. See [platform-services.md](platform-services.md) § IRC Chat (Live) |

`withAuthFailureCooldown` (`cmd/moombox/monitor_callbacks.go`) bounds the first four to one per platform per 30 minutes; it does not withhold the first. The two Info recoveries send inline with no cooldown. The Twitch chat notice is latched once per downloader instead, and is deliberately NOT deduped across jobs — a later job with the same dead cookies must notify again. That latch is reset by `Reauthenticate()`, so a repaired credential that fails AGAIN on the same job notifies again too; the platform mark it fires beside is deduped separately, by `shouldFireRecovery`, and so raises one alarm per loss however many jobs report it.

**The two-tier cookie liveness pilot is DISARMED, and its notifications do not fire.** `const livenessRecoveryArmed = false` (`internal/cookies/refresh.go`) gates tier 2, so the observation, the dedupe and the freshness accounting all run and are logged while only the last step is withheld. Nothing in the table above is produced by a liveness verdict today; the sole producer of a recovery is `shouldFireRecovery`'s conclusive not-authenticated — reached from `refresh`'s own validate pass and, now, from `RefreshService.NoteTwitchAuthLoss`, which passes a chat downgrade or an anonymous playback token as `(nowAuth=false, checkErr=nil)`, so the check reads conclusive for both and both share the same once-per-loss dedupe (`noteRecoveryDecided`). Arming the liveness pilot is a deliberate, separate change and adds a SECOND producer — not a side effect of wiring something else.

### Dispatch Behavior

- **Asynchronous:** Each notification is dispatched in a goroutine tracked by a `sync.WaitGroup`
- **Panic recovery:** Each goroutine has `defer recover()` — a panic in one notification sender cannot crash the application
- **Event filtering:** If a target has an event filter list, only matching events are sent. Targets with no filter receive everything.
- **Timeout:** Discord webhook HTTP requests have a 15-second timeout per attempt
- **Retry:** Bounded delivery loop, max 3 attempts total — transport errors and Discord 5xx back off 2s/5s; 429 honors a validated `Retry-After` (≤30s); other 4xx are permanent. Cumulative sleep is capped at 30s so a notification's semaphore slot can't be held past ~75s worst-case.
- **Hot-reload:** Notification config edits apply immediately — the web config route fires `OnNotificationsChange` → `Manager.Reload`, and the TUI save path calls `Reload` directly. No restart required.
- **Save-time validation:** Webhook URLs are validated at save (web `validateConfigUpdates` + TUI editor) via `notifications.ValidateURL`; `POST /api/notifications/test {url}` sends a single-attempt test embed (used by the web Test buttons and the TUI `T` action, including for unsaved URLs).
- **Graceful shutdown:** `Manager.Wait()` blocks until all in-flight notifications complete (called during shutdown step 3)
- **Embed format:** Discord rich embeds with title, description, color (by notification type), optional fields, thumbnail, image, footer ("Moombox Go"), and ISO 8601 timestamp

### Notification Type Colors

| Type | Color | Hex |
|------|-------|-----|
| Info | Blue | `#3498db` |
| Success | Green | `#2ecc71` |
| Warning | Yellow | `#f1c40f` |
| Error | Red | `#e74c3c` |
| Download | Teal | `#1abc9c` |
| Muxing | Purple | `#9b59b6` |
| Cancelled | Orange | `#e67e22` |

---

## Disk Monitoring

### Implementation

**File:** `internal/disk/disk_windows.go`

Uses Windows kernel32 `GetDiskFreeSpaceExW` via `syscall` FFI (no CGo). Queries the volume containing a given path and returns:

```go
type DiskSpace struct {
    Free    uint64  // bytes free for caller
    Total   uint64  // total bytes on volume
    UsedPct float64 // percentage used (0-100)
}
```

The path is resolved to an absolute path, then the volume root is extracted (`filepath.VolumeName(abs) + "\"`).

### Thresholds

Configured in the `[disk]` section of the TOML config:

| Setting | Default | Range | Purpose |
|---------|---------|-------|---------|
| `disk_warn_percent` | 90 | 1-99 | Warning level — surfaces in status bar and notifications |
| `disk_critical_percent` | 95 | 1-99 | Critical level — more urgent warnings |

Validation rules:
- Both values must be between 1 and 99 (invalid values reset to defaults)
- `critical_percent` must be greater than `warn_percent` (if not, it is set to `warn_percent + 5`, capped at 99)

### Status Reporting

Disk space information is included in the `GET /api/status` response and displayed in both the Web UI status bar and TUI status bar. When usage exceeds thresholds, a `disk_warning` notification is dispatched.

---

## Status and Diagnostics Endpoints

| Endpoint | Method | Purpose |
|----------|--------|---------|
| `/api/status` | GET | Server status: version, disk space, cookie state, monitor state |
| `/api/stats` | GET | Statistics dashboard data: job counts, download totals, etc. |
| `/api/logs` | GET | Recent log lines from the in-memory ring buffer |

These endpoints are used by both the Web UI (via fetch) and the TUI (via internal HTTP client with `X-Internal-Token` authentication).

---

## Reference Repositories

The `references/` directory (gitignored) contains clones of upstream projects that Moombox tracks for protocol changes, extraction logic updates, and implementation reference.

### Repositories

| Repository | What Moombox Tracks |
|------------|-------------------|
| **yt-dlp** | YouTube format selection, cipher/signature extraction, Twitch extractor, PO token handling, cookie extraction |
| **BgUtils** | BotGuard challenge protocol, PO token minting. The `bgutils-js` npm package (MIT, by LuanRT) is bundled inside the Moombox.exe via the Node sidecar; we track this repo for upstream changes to the BotGuard protocol. |
| **ejs** | yt-dlp external JS for YouTube cipher solving |
| **chatterino7** | Twitch IRC protocol, emote/badge handling |
| **bgutil-ytdlp-pot-provider** | yt-dlp PO token plugin (reference for PO token flow). NOT a runtime dependency of Moombox — that package is GPL-3.0-only and Moombox is MIT, so we re-implement the ~100-line `SessionManager` glue inline in `bgutil-sidecar/src/server.js`. |
| **moonarchive** | Python stream archiver (segment download strategies, DASH/HLS handling) |
| **moombox** | Original Python moombox (predecessor project) |

### Update Command

```bash
# Pull all upstream repos, show new commits and relevant file changes
bash references/update-all.sh

# Same, but include file-level diffs for deeper investigation
bash references/update-all.sh --diff
```

The script pulls each repository, displays new commits since the last pull, and highlights files that are relevant to Moombox's implementations (extractors, cipher logic, protocol handling, download strategies). Review the output to identify upstream changes worth porting.

---

## Cross-References

- **`architecture.md`** — Launcher/supervisor pattern overview, service initialization order, process model
- **`security.md`** — Ed25519 verification details, signing key management, CSRF and auth middleware
- **`data-and-storage.md`** — Config file paths, database path, output directory structure
- **`design-philosophy.md`** — Priority ordering that governs operational decisions (correctness > reliability > efficiency)

### Source Files

| Path | Relevance |
|------|-----------|
| `cmd/moombox/main.go` | Launcher, shutdown sequence, restart triggers, service init |
| `cmd/sign/main.go` | Signing tool |
| `internal/updater/updater.go` | Update checker, binary downloader, apply logic |
| `internal/updater/signing.go` | Ed25519 verification, embedded public key |
| `internal/notifications/manager.go` | Notification dispatch, event filtering, target management |
| `internal/notifications/discord.go` | Discord webhook sender |
| `internal/disk/disk_windows.go` | Disk space queries via kernel32 (Windows) |
| `internal/disk/disk_unix.go` | Disk space queries via statfs (Linux) |
| `internal/config/config.go` | Default values, validation (including disk thresholds) |
| `.github/workflows/release.yml` | CI pipeline definition |
| `cmd/moombox/winres/winres.json` | Windows resource metadata template |
