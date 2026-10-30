# Operations

## Scope

This document covers building, testing, releasing, updating, and running Moombox in production. It describes the build toolchain, CI pipeline, release process, self-update mechanism, launcher/supervisor pattern, shutdown sequence, notification dispatch, disk monitoring, and reference repository management. It is the authoritative reference for everything between writing code and running the binary.

## Rules and Constraints

- Build requires **Go 1.27**; `go.mod` carries `toolchain go1.27.3`, the floor local builds and CI auto-download; the Docker stage takes its patch from the floating `golang:1.27-bookworm` tag (`GOTOOLCHAIN=local` inside the image). Produces binaries for Windows x64, Linux x64, and Linux arm64 (cross-compiled via `GOOS`/`GOARCH` env vars; no CGo means the toolchain handles the rest transparently).
- **FFmpeg is required at runtime** — must be on PATH or configured via `cfg.Paths.FFmpegPath`. The first-run setup wizard validates FFmpeg availability and can install it via chocolatey or winget.
- **CI publishes on tag push only** (tags matching `v*`). The test suite runs on the tagged commit beside the release and gates nothing (see Release Workflow § Jobs). The workflow reads `RELEASE_NOTES.md` from the repository root for the GitHub release body.
- **Ed25519 signature verification is mandatory** before any binary swap during self-update. Updates without a valid `.sig` file are rejected, and so are releases without a valid signed manifest (`moombox-manifest.json`) naming that release, a newer version and the binary's SHA-256. Only a release before `FirstManifestVersion` (2.9.0), which never had a manifest, is one to install by hand; from it on, a release without its signed manifest is refused as a failure, and a binary installed by hand would fail Verify Signature for the same missing assets.
- **Exit code 42** is the restart signal. The launcher process respawns the child when it exits with this code, unless the launcher forwarded a stop to it (then it exits 0). Code 0 and a user-intent code (130/143, or a launcher-forwarded stop) propagate and terminate. Any other non-zero code is either an automatic rollback (first boot after an update), a fail-fast propagation (a fresh launch that died inside the 60 s healthy window), or a supervised crash respawn with backoff — see §Launcher/Supervisor Pattern.
- **Exit code 3** (`exitCodeStartupError`) is a DETERMINISTIC startup failure — an unreadable config, a logger that cannot open its file, a refused database migration, or (headless only) a web bind the host will not give. It always propagates, never crash-respawns, and the post-update rule never rolls back on it — the environment failed, not the new binary. Its timing decides nothing: an interactive child waits for a keypress before exiting 3, so how long it "ran" measures the operator, not the binary (`classifyChildExit`, `cmd/moombox/launcher.go`). Only an interactive one: the wait (`awaitKeypress`, `cmd/moombox/helpers.go`) runs only in a TUI session, and a headless run — `-no-tui`, `-headless`, `MOOMBOX_NO_TUI`, or no terminal — exits at once. A container started with a TTY (`docker run -t`, compose `tty: true`) has a terminal on stdin that nothing ever writes to, and checking that stdin was a terminal held its exit 3 forever, so a restart policy never fired. SIGTERM and SIGINT end the wait too, with the same exit 3: they cancel the run's signal context, which a blocked read used to swallow, so `docker stop` waited out its grace period into a SIGKILL.
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

# 2. Build the JS sidecar payload (~4 MB tarball).
cd bgutil-sidecar
npm ci --omit=dev --ignore-scripts        # production deps only.
node build.mjs                            # writes ../internal/bgutils/embed/sidecar.tar.gz
cd ..

# 3. Now build Moombox normally.
go build -o moombox.exe ./cmd/moombox
```

CI runs steps 1 and 2 automatically (see `.github/workflows/release.yml`). For local builds, run them once after fresh checkout; subsequent `go build` calls reuse the embedded blobs. Re-run step 1 when the Node pin in `version.txt` moves, and step 2 whenever anything under `bgutil-sidecar/` changes — `version.txt` records the Node pin only. A stale `sidecar.tar.gz` is caught by `internal/bgutils/embed/embed_test.go`, which fails when the tarball's `src/`, `package.json`, `package-lock.json` or ejs pin differ from the tree. The pin is `vendor/ejs/VERSION`, which `build.mjs` packs for exactly this comparison: the tarball carries ejs only as a generated bundle that cannot be checked against its source, but every re-pin rewrites that file.

The two embed sources are independent:
- `tools/fetch-node/main.go` is a Go tool that downloads the pinned Node release from `nodejs.org/dist/` for all three platforms (Windows x64, Linux x64, Linux arm64), SHA-256 verifies each against hardcoded constants in the source, gzips them to `internal/bgutils/embed/node-windows-amd64.gz`, `node-linux-amd64.gz`, and `node-linux-arm64.gz`, and updates `internal/bgutils/embed/version.txt` (committed file used as the cache-invalidation key for first-launch extraction). 5-minute HTTP timeout + 200 MB body cap per file.
- `bgutil-sidecar/build.mjs` is a Node.js script that `tar -czf` packages the production-only `node_modules/` + `src/server.js` + `package*.json` into `dist/sidecar.tar.gz` and copies the result to `internal/bgutils/embed/sidecar.tar.gz`. Build-time `tar` is required (system binary; available on Windows 10+, all Linux distros, and macOS) but the runtime extraction inside Moombox uses pure Go (`archive/tar` + `compress/gzip` from stdlib) — end users do NOT need a system tar.

To run without the sidecar, set `[bgutils] use_sidecar = false` in `config.toml`; Moombox then mints no PO tokens and cannot solve signature ciphers, so formats that need either become unavailable. The embed blobs are still required at build time though — they're either present or the binary doesn't compile.

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

**FFmpeg and ffprobe die with the process that started them.** Every one Moombox runs — the mux and its progress runner, the trims, every probe, the import's duration probe, both FFmpeg checks — is started through `CommandContext` (`internal/childproc/childproc.go`), and `TestNoFFmpegCommandOutsideChildproc` (same package) fails a production file that starts one through `os/exec` instead. On Linux the program gets a parent-death signal (`Pdeathsig` SIGKILL, `tie_linux.go`). The kernel sends that when the THREAD that forked the program exits, and Go retires a thread whenever a goroutine exits locked to it — any goroutine can be scheduled onto the thread the spawning one left while it waits on a pipe — so `Start` locks the calling goroutine to its OS thread and `Wait` unlocks it once the program has exited. It also gets a process group of its own (`Setpgid`), so the group-wide signal a closing terminal or a terminal's Ctrl+C sends reaches Moombox, whose shutdown ends the program, and not the program (see Stop forwarding). On Windows the program is assigned to a kill-on-close Job Object the child process owns (`tie_windows.go`): one per process, created on first use, never closed and never inherited, so it closes with the process however the process ends. A process already in a job is assigned to a second one by nesting it (Windows 8 and later), so the launcher's own job is still around it, and the launcher dying still takes everything. Elsewhere nothing is tied. The tie is best effort: a platform that refuses it logs one Warn and runs the program untied. It covers the program Moombox started: an `ffmpeg_path` wrapper that execs FFmpeg keeps it, one that forks FFmpeg and waits leaves FFmpeg itself untied on Linux. Before W28-A01 none of this held, and a child that died mid-mux (an OOM kill, a fatal runtime error, a kill of the child alone) left its FFmpeg writing while the crash-respawned child's boot re-mux wrote the same archive beside it.

**No FFmpeg writes an output's final name.** The copy mux, the two-pass encode, the part merge's concat, a set-aside recording's recovery and a trim's encode all write `<stem>.moombox-tmp-<run>-<n><ext>` beside the file they make, and the engine renames it into place (`utils.ReplaceFile`, after an fsync) only once FFmpeg has exited 0 and the output's own checks have passed on the temporary file — the ENGINE-9 duration check of the archive, part and aside muxes runs there (`OutputCheck`, `writeOutput`, `internal/engine/muxer_output.go`; `verifiedCopy`, `internal/worker/orchestrator_mux.go`, judged by `verifyAgainstStaging`, `internal/worker/mux_repair.go`). Any other outcome — FFmpeg failing, a cancel, a rejected check — removes the temporary file and leaves the final name as it was; a cancelled mux no longer keeps its moov-less partial for a salvage. `<run>` is a token drawn once per process. A crash or a kill leaves only the temporary file, and the next start removes what another run left (`RemoveStaleMuxTemps`, `internal/worker/mux_temps.go`, beside the trim and import temp sweeps in `cmd/moombox/services.go`) under the output directory and each job's own `output_directory`, walked whole, and in the directory of every recorded archive and part and the `trim` directory beside each (`trimDirsOf`) — a trim of an archive an earlier `output_directory` left behind writes its encode there, outside every tree. It never takes a live run's. This run's carry this run's token and are passed over unopened. Another token is not proof of a gone process: a second Moombox writing to the same output tree under its own data directory — and so its own single-instance lock — writes under its own, and so did a child still shutting down after only its launcher was killed (no longer: the child holds the instance lock too, § Launcher/Supervisor Pattern), and the sweep that took their files on the token alone failed their muxes (FFmpeg's faststart pass reopens its output by name). So the engine creates each temporary file itself, before FFmpeg opens it with `-y`, and holds an exclusive lock on it until the file is renamed or removed (`createMuxTemp`; `flock` on Linux, and on Windows `LockFileEx` on the one byte at 2^62 — a byte-range lock there is mandatory, so it must lie past anything FFmpeg writes — through a handle that shares delete, so the rename and the removal happen with it open; `muxer_output_lock_unix.go`, `muxer_output_lock_windows.go`). Another run's file goes only when the sweep can take that lock (`removeStaleMuxTemp`): the lock goes with the process that held it however that process ended, the sweep removes the file while holding it, and a writer that finds its file gone once its own lock lands draws a new name rather than hand FFmpeg one the lock is not on. A held file is left and logged at Debug. On a filesystem that cannot lock at all the file is written unlocked, and the sweep takes it only once it has sat untouched for a day (`muxTempIdleAge`) — a live FFmpeg rewrites its output as it goes. The sweep and the boot re-mux of a `Muxing` row therefore run side by side; the re-mux writes a temporary file of its own and replaces whatever sits under the archive's name only with a copy that passed.

### Docker Image

**Files:** `Dockerfile`, `docker/entrypoint.sh`, `docker-compose.yml`, `.dockerignore`
**Registry:** `ghcr.io/vampiricwulf/moombox` (published by `.github/workflows/docker-publish.yml`)

Three-stage build that runs the entire pipeline inside the image build — no host Go/Node toolchain needed:

1. **sidecar** (`node:24-bookworm-slim`): `npm ci --ignore-scripts` + `node build.mjs` → `sidecar.tar.gz` (mirrors release.yml).
2. **build** (`golang:1.27-bookworm`): `go run ./tools/fetch-node`, then `CGO_ENABLED=0` cross-compile for `$TARGETOS/$TARGETARCH`. Both stages run on `$BUILDPLATFORM`, so multi-arch builds don't emulate the compile.
3. **runtime** (`debian:bookworm-slim` + ffmpeg + ca-certificates + tzdata): must be glibc — the sidecar extracts an official nodejs.org Linux binary at runtime, and those are glibc-linked (Alpine/musl won't run it).

Container conventions:
- All state under a single `/data` volume (config, DB, logs, staging, output, sidecar cache — `HOME=/data` keeps the one-time sidecar extraction on the volume). `WORKDIR /data` so the binary's cwd-relative defaults land there too. The two large scratch file sets are written on the output volume, not under the container's `/tmp`: a multi-part trim's re-encoded intermediates beside the clip in the job's `trim/` directory (see [architecture.md](architecture.md) § 11. Trim Service), and a dashboard import's upload, up to 500 MB, spooled into `<output>/imports`, where its entries are extracted (`POST /api/import`, [user-interfaces.md](user-interfaces.md)). What else goes under the system temp directory is small: FFmpeg's concat lists and two-pass logs, the cipher player cache, the Firefox cookie-database snapshot, and the updater's manifest and signature checks.
- `MOOMBOX_NO_TUI=1` baked in; the launcher/supervisor runs as usual and forwards SIGTERM for graceful `docker stop`, and Ctrl+C for a `docker run` attached without `-t`.
- The entrypoint seeds `/data/config.toml` on first run, keeps `/data/README-docker.txt` current on every start (below), then execs the binary. The seed is mandatory, not cosmetic: with the `"localhost"` default the server binds `127.0.0.1` (unreachable through a published port), and the first-run web setup wizard cannot run either — `/api/setup/complete` is loopback-gated, and requests through Docker's bridge never appear as loopback. Seeding a config sets `ConfigLoaded`, which intentionally skips the wizard. An existing config is never touched.
- Seeded values (each explained in `/data/README-docker.txt`): `network_access = "lan"`, `port = 774` (paired with the container side of the port publish — the image `EXPOSE`s 774 — and the compose healthcheck), absolute `/data` paths, `cookie_file = "/data/cookies.txt"` (reached through the existing `./data` volume — the file must NOT be bind-mounted individually, because the periodic YouTube session write-back replaces it via temp-file + rename and a rename cannot replace a single-file bind mount), and `updates.auto_check_updates = false` — an in-app update swaps the binary inside the container and is silently reverted when the container is recreated from its image, so the update path is pulling a new image; the manual "Check for updates" button still works. Everything else keeps the binary's normal defaults (notably `use_sidecar = true` — the Debian runtime runs the glibc sidecar Node binary fine — and stdout logging, which `docker logs` picks up alongside the rotating `/data/moombox.log`). The seed also carries empty `[memory]`, `[bgutils]` and `[connectivity]` tables, the three sections `loadFromFile` would otherwise flag with `NeedsAutoPersist`, so the boot's auto-persist has nothing to add.
- **The guidance is `/data/README-docker.txt`, not comments in `config.toml`.** `config.toml` keeps no comments: `Save` (`internal/config/config.go`) encodes the settings struct, so every save rewrites the file without them — and the first boot saves it, because `run()` stamps `last_run_version` on any loaded config whose stamp differs from the running version (so does the first boot after every update), and so does any change from either UI. The seed's own comments, including its first line pointing at `README-docker.txt`, are therefore gone once the first start is up; they are read only when that start fails before the stamp. `README-docker.txt` is a file no save touches. The entrypoint writes it when it is missing and refreshes it when the image's text differs — but only while the file still holds, word for word, a text an entrypoint wrote, to it or beside it as `README-docker.txt.new`: `/data/.README-docker.txt.sha256` lists the SHA-256 of every such text, each once. An operator-edited copy is never overwritten: a text not given before goes to `README-docker.txt.new` beside it and is logged — once per text, so a `.new` the operator deleted is not written back, and an edit of the image's own text gets no `.new`, having nothing new to put beside it. Moving the `.new` over the edited copy (or deleting the copy) lets later images refresh it again — whichever image starts next, which is usually the next update, since the entrypoint runs only at container start (Moombox's own exit-42 restarts happen inside the launcher): the moved text is listed as an entrypoint's. Refresh-when-unedited was chosen over write-when-missing because the guidance describes the image: an image that changes what it seeds or how the container behaves must be able to say so, and a copy frozen at the first install's text would keep the old advice forever. A failure to write the guidance is reported and never stops the container from starting. The seed and the guidance are covered by `internal/config/docker_seed_test.go` (the seed through `Load`, then the first boot's saves) and `internal/config/docker_entrypoint_test.go` (the entrypoint itself, run under `sh` with a stub binary); nothing in CI builds or starts the image.

**Cookies in a container.** `cookies.auto_enabled` stays **false** — the seed does not set it, and it should not be turned on. All it owns is a slow headless-browser refresh timer and one automatic browser attempt when auth fails, and this image ships no browser; it does NOT gate the profile import, and the in-process `RefreshService` that keeps an imported YouTube session alive runs regardless (see [data-and-storage.md](data-and-storage.md) § Cookies for exactly what the flag does and does not own). The designated workflow is: bind-mount a Firefox profile directory — the one holding `prefs.js` and `cookies.sqlite`, copied with Firefox closed and **with its `cookies.sqlite-wal` sidecar**, because recent cookies live in that sidecar and a copy without it reads as empty — at `/data/browser-profile`, then trigger the read yourself after each host-side refresh: `R F` in the TUI, shift+click the dashboard header's "Refresh cookies", or the Settings page's "Refresh cookies from browser profile" button. Those are one gesture with three affordances, all importing straight from the profile with no browser and over whatever `cookies.txt` already holds; on a phone or tablet use the Settings button, because shift+click needs a keyboard. First boot needs none of it — the automatic import runs on its own shortly after start, but **only when there is no `cookies.txt` to lose** (`automaticImportGuard`: absent, or present with zero rows; an unreadable file aborts rather than counting as absent). After that the profile is deliberately not re-read on a timer, because nothing inside the container changes it. `docker/entrypoint.sh` writes this same guidance to `/data/README-docker.txt`, and that file is the operator-facing reference wording. `cookies.acquisition` needs no entry in a container either: with no browser installed, `"auto"` already takes the import path. It exists for the desktop case — a host that HAS a browser and whose operator wants their real profile read instead — and it is the setting that lifts the launch-path profile-dir guard on the two read-only sites (`validateBrowserProfileDirForLaunch` stays on every subprocess site regardless).

**Re-authentication without touching the volume.** `POST /api/cookies/import` accepts a Netscape cookie file — pasted as `text/plain` or uploaded as a multipart `cookies` part, capped at 512 KiB — behind session auth, the CSRF middleware and the shared `heavy` limiter. It MERGES rather than replaces (a YouTube-only paste leaves the Twitch rows alone), writes through `writeFileAtomic`, reloads the jar, verifies live, and answers with the same three-state per-platform verdict the setup wizard's finish returns, so a signed-out export is reported at paste time rather than at the next members-only stream. Since 2.8.7 it also **verifies before it commits and rolls back per platform**, exactly as the two refresh paths do and through the same helpers (`snapshotPlatformAuth`, `platformsToRestoreOnRegression`, `restorePlatformRows` — see [data-and-storage.md](data-and-storage.md) § Auto-Cookie Service): a paste whose rows for one platform are dead no longer replaces that platform's working rows, and the response says per platform whether the rows were imported, rolled back, rejected, or never touched. An inconclusive check is not a failure and rolls nothing back. This matters most in exactly this deployment, where the rejected paste used to be unrecoverable without shell access to the volume — the thing this endpoint exists to avoid needing. It is allowed from **any authenticated client**, not loopback-gated: the setup wizard's gate protects an unclaimed instance, and a loopback-gated ingest would be useless in exactly the deployment this exists for. **There is no corresponding GET and there must never be one** — accepting credential bytes and never serving them is what keeps it from being an exfiltration path. It answers **409 `cookie refresh in progress`** while a refresh pass — or another import — holds the refresh slot (owner decision O-D): the paste is refused rather than destroyed by the pass's merge of the pre-paste file, and a retry within ~2 minutes lands, `refreshOverallBudget` being the pass's own bound. Replacing the file on the volume still works and is still the faster path for anyone with a shell.

**IPv6-enabled compose network.** `docker-compose.yml` declares its `default` network with `enable_ipv6: true` and a ULA subnet. The reason is Moombox's `lan` seed: Docker's userland proxy accepts IPv6 connections to a published port and re-originates them from the bridge gateway's *private IPv4* address, so an internet IPv6 client would arrive looking like a LAN client and pass the `lan` filter. On Docker Engine 27+ — where ip6tables is enabled by default for IPv6-enabled networks — inbound IPv6 is DNATed to the container instead, with its source address kept. Moombox's `lan` listener is dual-stack (`0.0.0.0` with network `"tcp"`, which Go opens as `[::]` wherever the kernel has IPv6), so those connections are accepted and the filter judges the real IPv6 client: a ULA or link-local one is admitted, and a global-unicast one — an internet client, or a LAN client on a global IPv6 address — gets 403 Forbidden, which browsers do not retry over IPv4. Reach the dashboard over the host's IPv4 address.

Two limits. On Engine < 27, ip6tables is off by default, the userland proxy still handles IPv6, and the misclassification persists silently — nothing in-app can detect it. And a host with IPv6 disabled in-kernel (e.g. booted with `ipv6.disable=1`) fails to *create* the network at all: `docker compose up` errors rather than degrading. Recovery is to delete the `networks:` block, or set `enable_ipv6: false` and drop the `ipam:` subnet with it, accepting that the hole reopens on that host. The compose file carries both notes inline, and its comments are the reference wording. For the operator-facing version — VPN, reverse proxy, `trusted_proxies`, Docker Desktop, host-firewall bypass — see "Remote Access" in `README.md` and the Docker source-IP caveats in [security.md](security.md).

**No CI validation of `docker-compose.yml`.** `.github/workflows/docker-publish.yml` builds from the `Dockerfile` with `context: .`, and `.dockerignore` excludes `docker-compose.yml` from the build context entirely — CI never parses it. A compose-file change therefore ships unverified by the image-build gate; the real verification is `docker compose up -d` on a daemon-equipped host.

---

## Browser Cookie Acquisition (Platform Differences)

The interactive cookie setup and the headless refresh both launch a real browser, and both depend on a liveness primitive: a Job Object on Windows, a process group on Linux, nothing on darwin or the fallback build. What each platform can and cannot OBSERVE is stated here because the differences are recorded residuals, not oversights. The mechanism itself — launch args, extraction, the CDP ladder, the DPAPI fallback — is in [data-and-storage.md](data-and-storage.md) § Cookies.

**The reap keys on a liveness primitive reporting zero live processes, never on `cmd.Wait()`.** On Windows that primitive is a Job Object; on Linux it is the browser's own process group. A Firefox-family launcher hands off to the real browser and exits in ~170 ms (measured), so the process Moombox spawned exiting says nothing about whether the page loaded — closing the job at that moment killed the browser mid-load, and was the reason every Firefox-family refresh silently did nothing. `setupBrowserGone` (`internal/cookies/autocookies_setup_slot.go`) therefore returns TWO bools: whether the job is empty, and whether anything could say. `known == false` must be read as "still running", never as "gone"; the same launcher-exit is a normal mid-`FinishSetup` state, not evidence of abandonment.

**Windows counts a Job Object; Linux counts a process group; nothing else can say.** `processJob.queryable()` is `j != nil && j.handle != 0` in `job_windows.go` and `j != nil && j.group.queryable()` — that is, a process group was adopted — in `job_linux.go`. `configureCmdSysProcAttr` sets `Setpgid` on Linux, so every browser this package launches leads a group whose id is its own pid; `activeProcesses` counts that group's members by walking `/proc`, and `assign` REFUSES a process that did not lead its own group, because recording one that inherited Moombox's group would later point the kill at Moombox itself — in Docker, at the container. `job_other.go` is still unconditionally `false`: darwin and the fallback build have no primitive, `realSetupBrowserGone` always answers "not known" there, and a browser left open by an abandoned setup is not reaped. One Linux case answers "not known" too, honestly: a container whose `/proc` cannot be walked (a failed read is an error, never a zero — an empty table would read as "the group is empty", which is the answer that releases the slot). One answers WRONGLY: a browser that calls `setsid()` leaves the group, the count then reads the group as empty, and the reap releases the slot when the grace runs out with the browser still on screen — no kill, because the group it would signal has no members, but the operator's next finish answers `ErrNoSetupInProgress`. Which Linux packagings do that (a snap or flatpak wrapper is the suspect, not the browser binary) is unmeasured. A zombie is not a member: the `/proc` parser skips state `Z`, because a container where Moombox is PID 1 without an init (`docker-compose.yml` sets `init: true`; a bare `docker run` does not) never reaps an orphaned grandchild, and counted it would hold the group open for the life of the process. **The Linux reap is BUILT, NOT FIELD-VERIFIED.** Its decisions are unit-tested against a fake process table (`internal/cookies/job_pgroup_test.go`, which is why they live in a file with no build tag); nothing here has been run against a real Linux desktop or container, and a user's bug report is the gate. One named residual comes with it: a Job Object handle cannot name a process that is not in the job, but a pgid can, because the kernel recycles pids — `pgroupJob.killGroup` refuses to signal a group it cannot see or that has no members left, which narrows the window as far as it can be narrowed without closing it. A second residual is crash-time cleanup: a crashed Moombox signals nothing on Linux — `Pdeathsig` (`configureCmdSysProcAttr`, `job_linux.go`) covers only the direct child, and the group is counted and killed only by a running Moombox — so where a wrapper handed off, what it handed off to outlives a Moombox crash, which is the one thing `KILL_ON_JOB_CLOSE` on a Windows Job Object handle covers that a process group cannot; unmeasured, like the handoff itself.

**`AbandonSetup` releases only where releasing cannot kill.** `POST /api/cookies/auto-setup/abandon` is what a CLIENT reports when the client itself went away — the dashboard tab unloaded — and it is deliberately not `/cancel`: a click is consent to close the browser, a tab unload is not, and the setup flow's own instructions send the operator AWAY from that tab to sign in. It asks `setupBrowserGone` for `known` and branches on it. Known (Windows, live job) → do nothing; the reap owns the slot on its own correct predicate, and releasing here would mean `cleanupLocked`, which closes a `KILL_ON_JOB_CLOSE` handle on the window the operator is typing a password into. Not known → release the slot, killing nothing, because nothing was tracking the browser anyway. Since the process-group reap landed, Linux takes the SAME declining arm as Windows wherever a group was adopted: the reap owns that slot. The release arm is now scoped to the cases where nothing can answer — a failed assign or an unreadable `/proc` on Linux, darwin and the fallback build — and it is still the only release there; the `cleanupLocked` it runs asks for a group kill on Linux, and that kill refuses a group it cannot see, so releasing still kills nothing. (A browser that left its group is the OTHER arm's problem: it reads as gone — see above.) The rule did not change; the set of platforms on each side of it did. The call is redundant wherever a group or a Job Object was adopted — Windows, and Linux since the process-group reap — and load-bearing wherever nothing was: darwin, the fallback build, and any Linux launch whose group could not be adopted. Neither arm is dead code on either platform; deleting the check would restore the kill on both.

**The grace window is priced against the clients, not guessed.** `setupAbandonGrace` is 60 s and exists for exactly one caller: a `FinishSetup` already in flight. Both clients cap one finish at 60 s — the Web dialog's `AbortController` and the TUI's `finishCtx` in `cmd/moombox/tui_wiring.go` — and `FinishSetup` re-stamps `setupRetainedSince` when it takes the slot, so a finish always gets a full window from the moment it started. Inside that window the BINDING server-side column is Chromium (~42.3 s: `cdpExtractTimeout` 30 s + `taskkillDrainDelay` 0.3 s + `authVerifyTimeout` 12 s — one window PER PLATFORM since the split, but the platforms verify concurrently so a whole `checkPlatformAuth` call costs one window, and `FinishSetup` makes exactly one), not Firefox (~14.3 s), so the real margin is ~17.7 s. Do not lower it to 30 s, and do not serialise `checkPlatformAuth` or raise `authVerifyTimeout` back to 15 s without raising this and both client caps with it: serialised the Chromium column is 54.3 s, and serialised at 15 s it is 60.3 s, over all three. The TUI wizard's own countdown is a separate number — 300 s, `cookieSetupCountdownSeconds` in `internal/tui/setup_wizard.go` — and on expiry it calls `AutoCookieService.CancelSetup` in-process, through the wizard's `OnCancelAutoCookie` (`internal/tui/setup_wizard.go`), whose callback is supplied at `cmd/moombox/tui_wiring.go` and bound in `internal/tui/app.go` — no HTTP: the TUI shares this process. That is a cancel, not a finish, and the 60 s cap is scoped to one `FinishSetup`, so the two do not interact.

**The Linux launcher-handoff premise is UNMEASURED.** Mozilla documents the Firefox launcher process as a Windows feature, so "Linux Firefox hands off the same way" is an inherited claim rather than an observation, and several comments in `internal/cookies/` rest on it. Nothing depends on it being true — the process group counts and kills whatever is in it whether or not a handoff happened, and where nothing outlives the direct child the drain lands on lap zero exactly as before — but it should not be repeated as a fact. The settling experiment is one run of the drained-launch subtest on any Linux box.

**The drain's numbers are observations, not a healthy band.** `drainJob` (`internal/cookies/autocookies_firefox.go`) polls the job every 50 ms until it empties or the shared launch budget runs out. Recorded Windows passes: Waterfox 2.848 s over 53 polls and Firefox 1.734 s over 32 polls on 2026-08-25; a clean pass on different hardware on 2026-08-26 took 13.96 s over 276 polls with the same successful outcome and no error. The diagnostic is `errBrowserDrainTimeout` — returned when the budget expires with processes still alive — and nothing about the elapsed time or the poll count on its own. A failed count query degrades to the pre-drain behaviour (returns nil) rather than spinning on a failing syscall for the whole budget. LibreWolf and Zen remain unverified. So does every non-Windows target — but the reason changed on Linux: it now HAS a count (the process group), so `drainJob` waits there for anything that outlives the direct child — a wrapper's handoff, a straggling content process — up to the budget, where before it returned on lap zero regardless. Without a launcher the direct child is the browser itself and `cmd.Wait()` already waited for it, so where nothing outlives it the timing is unchanged. That is the Arc 0 fix arriving on Linux, and it is unobserved: no timing has been recorded on any Linux box.

**"Acted" is a separate question from "succeeded", and each engine answers it differently.** Success measured against `cookies.txt` is not evidence, because the independent 30-minute `RefreshService` keeps that file alive regardless — which is how a refresh whose browser was killed mid-load still logged "cookie refresh succeeded". `browserActed` (`refreshCookiesDetailed`, `internal/cookies/autocookies_refresh.go`) starts TRUE and only the browser branch may clear it. That scoping is deliberate: the profile-import path and the empty-profile fallback launch no browser, so there is no browser inaction to detect, and gating them here would make every containerised import report a refresh that never renews, permanently, on every restart.

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

### Release Workflow

**File:** `.github/workflows/release.yml`
**Trigger:** Tag push matching `v*` (e.g., `v2.6.3`), plus `workflow_dispatch` as a dry run
**Runner:** `ubuntu-latest` for the release job (cross-compiles all platforms from Linux)
**Permissions:** `contents: read` workflow-wide; each job states its own — `contents: write` on the release job (to create releases and upload assets), `packages: write` on the Docker job

#### Jobs

- **`test`** — calls `.github/workflows/ci.yml` (lint, the node suites, and the Go suite's linux, race and windows shards — see Test Workflow) for the tagged commit, beside the other two jobs. Nothing `needs` it: its result is reported in the release's own run, and a red suite marks the run failed while the release still publishes. "Re-run failed jobs" on the same tag re-runs only the jobs of it that failed. It gated both jobs from v2.8.9 (published while CI on the same commit was red) until v2.9.6, whose first two tag runs were held by failures that were not the code's — a test race exposed by the race detector's slower scheduling, then a Windows runner out of memory before `go vet` could start.
- **`release`** — the binaries; steps below.
- **`docker`** — calls `.github/workflows/docker-publish.yml` with `push` true only for a tag push. It `needs` `release`, so a failed build or signature check cannot leave a pushed `latest` image for a version that has no release.

**Dry run.** "Run workflow" on any ref runs all three jobs exactly as a tag would. What publishes is the tag *push* — every gate tests `github.event_name == 'push'` — so a manual run started on a tag ref is still a dry run and cannot overwrite that release's assets. In a dry run the image is built for both architectures but not pushed, and the release step uploads the eight assets to a *draft* release named `dryrun-<run id>-<attempt>` — never public, and it creates no git tag — which the following step checks (a draft with eight assets) and deletes. The upload therefore runs with the same action, token and file list a tag uses. Signing runs, self-check included, so a wrong `SIGNING_KEY` is caught before a tag exists — and so does the manifest step, which writes and signs a manifest for the dry run's `-dryrun` version and draft tag, so the manifest pipeline is exercised too. The dry run's version is the one `cmd/moombox/main.go` declares plus `-dryrun`. It exists because this workflow otherwise runs on tags alone and a tag is never replaced: a break in it was found by the release it broke.

#### Release job steps

1. **Checkout** — `actions/checkout@v7`
2. **Set up Go** — `actions/setup-go@v7` with version from `go.mod`
3. **Compute version + ldflags** — Exports `VERSION`, `COMMIT`, `LDFLAGS` to `$GITHUB_ENV` once so the Windows resource step and all per-binary steps reference the same values, plus the release target (`RELEASE_TAG`, `RELEASE_DRAFT`). `VERSION` is the tag without its `v`, or on a dry run the declared version plus `-dryrun`. On a tag push the step fails unless the tag equals the version `cmd/moombox/main.go` declares at that commit: a mismatch means the tag is on the wrong commit or the bump was not committed, and `RELEASE_NOTES.md` there would be the previous release's.
4. **Set up Node** — `actions/setup-node@v7`
5. **Build BotGuard sidecar payload** — `npm ci --ignore-scripts && node build.mjs`
6. **Fetch embedded Node binaries** — `go run ./tools/fetch-node` — downloads pinned Node v24 LTS for all 3 platforms, SHA-256 verifies, gzips to per-platform embed files
7. **Generate Windows resources** — Patches `winres.json` with the version + commit hash via `jq`, runs `go-winres make --arch amd64` in `cmd/moombox/`. `go-winres` runs on any host OS; the resulting `.syso` uses filename build constraints so it's included only under `GOOS=windows`.
8. **Build Moombox.exe** — `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$LDFLAGS"`
9. **Build moombox-linux-amd64** — `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$LDFLAGS"`
10. **Smoke-test moombox-linux-amd64** — runs the one binary this runner can execute with `--version` and requires exactly `moombox <VERSION> (<COMMIT>)`: it starts, and the ldflags reached it.
11. **Build moombox-linux-arm64** — `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$LDFLAGS"`
12. **Sign Moombox.exe** — `go run ./cmd/sign Moombox.exe` → `Moombox.exe.sig`, verified against the updater's embedded public key before the step succeeds (see Signing Tool)
13. **Sign moombox-linux-amd64** → `moombox-linux-amd64.sig`
14. **Sign moombox-linux-arm64** → `moombox-linux-arm64.sig`
15. **Write and sign the release manifest** — `go run ./cmd/sign -manifest -version "$VERSION" -tag "$RELEASE_TAG"` → `moombox-manifest.json` (the version, the tag, and each platform's asset name and SHA-256) and `moombox-manifest.json.sig`, self-checked like the binaries' (see Signing Tool, and [security.md](security.md) § Release Manifest for why the binaries' own signatures are not enough).
16. **Build release body** — If `RELEASE_NOTES.md` exists and is non-empty, prepends three download links and uses the file as the release body. Otherwise, falls back to GitHub's auto-generated release notes.
17. **Create GitHub Release** — `softprops/action-gh-release@v3` with body from step 16 and 8 assets: `Moombox.exe` + `.sig`, `moombox-linux-amd64` + `.sig`, `moombox-linux-arm64` + `.sig`, `moombox-manifest.json` + `.sig`, with `fail_on_unmatched_files` so a missing asset fails the step instead of publishing without it. Tags containing `-` (e.g. `-rc.1`, `-test.1`) are marked as pre-releases. One step for both modes: a tag push publishes under its tag; a dry run uploads to the draft described above.
18. **Check and delete the draft** (dry run only) — requires a draft with eight assets, and deletes it first so a failed check leaves nothing behind. `TestReleaseWorkflowPublishesTheSignedManifest` (`cmd/sign/main_test.go`) holds that count to the upload list.

Steps 5 and 6 run on every release, with no `actions/cache` in front of them: the embed blobs a signed binary carries are built from the tagged commit. The job used to cache them, and that cache never hit — a cache saved by one tag's run is not readable from another tag's (v2.8.3 through v2.8.10 all missed) — while its key left out `bgutil-sidecar/src` and the vendored ejs, so a hit would have shipped the previous sidecar JS under a new version number.

The three builds (steps 8, 9 and 11) are sequential (not parallel). On a 4-vCPU runner each `go build` saturates the CPU, so concurrent builds contend for cores and re-download every module dep three times. Sequential is faster end-to-end; the first build also warms the module cache for the next two.

### Test Workflow

**File:** `.github/workflows/ci.yml`, with four scripts beside it: `.github/scripts/test-shard.sh` (which packages each test shard runs), `.github/scripts/retry.sh` (bounded retries for network steps), `.github/scripts/install-ffmpeg.sh` and `.github/scripts/go-cache-trim.sh` (when a restored cache trims itself)
**Trigger:** push to `main`, every pull request, `workflow_dispatch`, and `workflow_call` — release.yml runs it as its `test` job, beside the release rather than ahead of it
**Runners:** `ubuntu-latest` for `lint`, `js` and the linux and race flavours; `windows-latest` for the windows flavour — the Windows-only code paths (DPAPI cookie reading, Job Objects, cookie profile paths) have tests that skip everywhere else. The test matrix is `fail-fast: false`: a failing shard cancels none of the others.
**Permissions:** `contents: read`; one run per calling workflow and ref (`concurrency` with cancel-in-progress — the group carries `github.workflow`, the caller's name under `workflow_call`, so a release dry run from `main` and a push to `main` do not cancel each other); a 15-minute timeout on every job (10 on `js`)

The jobs all start at once, so a run takes as long as its slowest job. They replaced one serial job per OS — about 13 minutes on ubuntu (setup ~1.5, vet 0:46, staticcheck 0:54, `go test` 2:25, `go test -race` 5:46, the arm64 cross-build 0:45, the node suites 0:43) and about 8 on windows (setup ~3, `go test` 3:46) — and every gate those ran is still run, by exactly one job:

- **`lint`** (ubuntu) — `gofmt -l` must print nothing → `go mod tidy -diff` → `go vet ./...` for linux/amd64, windows/amd64 and linux/arm64 → the `modernc.org/libc` pin check (the version `modernc.org/sqlite`'s own `go.mod` names must be the one this module pins) → the Dockerfile Go-version check (its `golang:<major.minor>` build stage must match `go.mod`'s `go` line; the image's Go does not download a newer toolchain, and nothing else builds the image before a release) → `staticcheck ./...` for linux/amd64 and windows/amd64 → `go build ./...` → the windows/amd64 and linux/arm64 cross-builds (`go build ./...` with `CGO_ENABLED=0`, which links every main package, `cmd/moombox` among them) → `test-shard.sh --check`. None of it runs code, so none of it needs the real embed blobs: an empty file first stands in for every file a `//go:embed` directive names that the checkout lacks and git ignores — the sidecar tarball and the three Node binaries, found from the directives as in the vulnerability scan; a tracked file missing from a commit gets none, so vet and the builds still fail on it — and `lint` downloads no Node, builds no sidecar and installs no FFmpeg. vet and staticcheck for windows run from ubuntu with `GOOS=windows` instead of on a windows runner: with cgo off, as every Moombox build is, both type-check the same files.
- **`js`** (ubuntu) — `npm ci` and `node build.mjs` in `bgutil-sidecar` (the build writes `vendor/ejs.bundle.js` and restores the devDependencies it prunes for the tarball), then its `npm test`; then the frontend suite (`npm ci` in `web/tests`, `node --test ./*.test.mjs` — jsdom is that package's devDependency, so the DOM suites execute rather than skip). No Go.
- **`test`** — `go test -count=1` over one shard's packages per job, in three flavours: linux (shards `worker`, `rest`), race (`-race` over the whole module: `worker`, `engine`, `web`, `rest`) and windows (`worker`, `engine`, `cookies`, `web`, `rest`). Each job: checkout → the FFmpeg install, started in the background (`install-ffmpeg.sh --start`: `apt-get` on ubuntu, `choco` on windows) because it needs nothing the next steps make → the Node binary cache → `setup-go` from `go.mod` → `setup-node` 24 → its flavour's Go cache and the npm cache → `go mod download` → the sidecar payload build, on every run (the tarball is never cached, so the Go tests embed the sidecar JS of the commit under test) → `go run ./tools/fetch-node` on a cache miss → `install-ffmpeg.sh --wait`, which prints the install's log and fails the job if it failed, so `muxer_concatcopy_test.go` and `probe_params_test.go` run instead of skipping → `go test -timeout=7m` in a step with a timeout of 11 minutes, under the job's 15. `-timeout` is the limit for one test binary, past which it dumps every goroutine and fails — what names a deadlocked test (one that hangs only under the race detector's scheduling, say). It counts from that binary's own start, not the step's, so the four minutes between it and the step's limit are for the compiling and queueing ahead of a binary's start: room to spare on a warm cache, and the dump lands before the step is cut off unless a cold cache holds a binary back longer than that. It is still over twice the slowest binary's run on any runner (`internal/worker` on windows, 205 s at worst). The race flavour has covered the whole module since 2026-10-09 (owner ruling O-P began it on `internal/logger`, `internal/database` and `internal/web`); a test that budgets allocations or fits a probe inside one wall-clock second widens its gate or skips under the race build through its package's `raceEnabled` constant, and the linux flavour still runs it.

**Shards.** `test-shard.sh` holds the layout. A named shard is a fixed list of packages — `worker` (`internal/worker`), `engine` (`internal/engine`), `cookies` (`internal/cookies`), `web` (`internal/bgutils/sidecar`, `cmd/moombox`, `internal/web/routes`) — and each flavour lists the named shards it runs. Its `rest` is every package `go list ./...` reports for that flavour's GOOS that none of its named shards lists, so a package added tomorrow runs in `rest` with no edit anywhere, and moving one out of `rest` is one line of the script. A flavour names a shard only where one package's tests take a large share of a job on their own. The layout was cut from the per-package times of the serial jobs (plain / race / windows, under `-p 4`): `internal/worker` 96–100 / 142–147 / 139–205 s, `internal/engine` 55 / 65 / 63–118, `internal/cookies` 17 / 31 / 67–158, `internal/bgutils/sidecar` 14 / 76–82 / 44–91, `internal/web/routes` 10 / 53–57 / 35–55, `cmd/moombox` 8 / 42 / 30–65, and under 40 s for every other package everywhere. The aim is a run whose slowest job ends about three minutes after the push; while `internal/worker`'s tests alone take over two minutes under `-race` and on windows, those two `worker` shards are the long pole, and the layout stays sound as they shrink: a named shard that becomes short is only an early finisher, and folding its package back into `rest` is deleting the shard's name from the flavour's line in the script and its row from the matrix (`--check` fails until both are gone). `bash .github/scripts/test-shard.sh <flavour>/<shard>` prints a shard's import paths — what its job passes to `go test`, and the way to run one shard locally. `test-shard.sh --check` fails unless, for every flavour, every named package exists, no package is in two shards, no shard is empty, the shards together are `go list ./...` exactly once each, and `ci.yml`'s matrix rows (written `- { flavour: F, shard: S }`, the form the check reads) are exactly that flavour's shards.

**Caches.** Each flavour — lint, linux, race, windows — keeps its own Go cache: `GOCACHE` plus the module zips (`$GOMODCACHE/cache/download`; `lint` also keeps staticcheck's cache), keyed by flavour, OS, architecture, Go version and the hash of `go.sum`, and falling back to the newest entry of the same flavour and Go version. `setup-go`'s own cache is off. It has one key per runner OS, and with several jobs on one OS whichever saved first would decide what every one of them restores (`lint`'s build holds no race-instrumented package, a race shard's no plain one), while the others raced to save the same entry and failed. One job per flavour saves (`lint` itself, and each flavour's `rest` shard), only on a push to `main` — the one branch whose caches every pull request and every later run can read — only when its key missed, and only once its compiling steps ran, pass or fail (`go test` in a shard; the last cross-build in `lint`): a job that failed in setup holds next to nothing, and saved under the new key it would be every later run's exact hit, never replaced until `go.sum` or Go moved. A restored cache keeps the mtimes and the `trim.txt` it was saved with, and a go command that builds anything trims GOCACHE as it exits once `trim.txt` is a day old, dropping every entry unused for five days — all of a cache saved a week earlier — and staticcheck does the same to its cache; the job's first build (`go install` of staticcheck in `lint`, `go run ./tools/fetch-node` in a test job whose Node cache missed) used to delete what every later step was about to reuse. `.github/scripts/go-cache-trim.sh hold` therefore writes the current time into `trim.txt` right after the restore, which holds the trim off for the job; a job that saves trims once, after its last use (`go-cache-trim.sh now`, a `go build -n` that compiles nothing, and `due` ahead of the last staticcheck run), so the saved cache keeps what the job used and does not grow from one `go.sum` to the next. A `rest` shard's own tests compile every third-party package its flavour's other shards need but a handful of small ones (all 132 for linux; race and windows miss four or five `golang.org/x/text/encoding` and `go-isatty` packages), and the module's own packages in a saved cache go stale with the next commit anyway, so nothing compiles extra to fill it. The three pinned Node binaries are cached per runner OS (keyed by the hash of `version.txt` and `tools/fetch-node/main.go`), as is the sidecar's npm cache, and the linux and windows `rest` shards save those, each only after the step that fills it succeeded; `js` keeps `setup-node`'s npm cache for both npm projects.

**Network steps are bounded.** Every installer and download — `npm ci`, `fetch-node`, the module downloads, the staticcheck install, `apt-get`, `choco` — runs through `retry.sh`, which gives each attempt a time limit (GNU `timeout`, where the runner has it) and makes three attempts, inside a step `timeout-minutes` of 10. apt's own timeout fires only on total silence, so a mirror connection that trickled bytes once hung the ubuntu leg's FFmpeg install for 22 minutes behind `-qq` output; `install-ffmpeg.sh` runs apt with `-q`, 90 seconds per attempt, downloading first (`--download-only`) and then installing with `--no-download`, so the part that cannot be cut off half-way never waits on a mirror, and the step that waits for it carries the 10-minute bound. A windows run once died in `go mod tidy` on one reset HTTP/2 stream from `proxy.golang.org`. `lint` now fetches everything first, where a failure is retried: `go mod download`, then the modules `go mod tidy` additionally reads (the tests of dependencies), by tidying a scratch copy through `-modfile` so the real `go.mod` and `go.sum` are untouched and the tidy check itself needs no network, then the staticcheck install. Cache restores give up after five minutes and a stalled cache download after three (`SEGMENT_DOWNLOAD_TIMEOUT_MINS`), and the job carries on as on a miss.

`staticcheck ./...` is a hard gate, installed at a pinned release (`2026.2.2` in `.github/workflows/ci.yml`) because staticcheck lags Go releases and `@latest` can refuse a new toolchain. The pin moves with the toolchain: `2026.2.1` cannot read the export data Go 1.27.2 writes (format version 5), so the toolchain pin in `go.mod` and this pin were bumped together. The live gates (`MOOMBOX_LIVE_*`) never run in CI: they need YouTube and Twitch.

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
**Trigger:** `workflow_call` from release.yml's `docker` job (after the release job, so it has no tag trigger of its own), plus `workflow_dispatch` for testing image changes without cutting a release. Both take a boolean `push` input, default true: release.yml passes true only for a tag push, and a manual run with it unticked builds both architectures without pushing.
**Permissions:** `contents: read`, `packages: write`

Builds the multi-arch (linux/amd64 + linux/arm64) image via buildx and pushes to `ghcr.io/vampiricwulf/moombox`. Release tags produce `X.Y.Z`, `X.Y`, and `latest` (pre-release tags containing `-` skip `latest`, matching release.yml's pre-release handling); manual dispatch on `main` produces `edge`. `VERSION`/`COMMIT` build args mirror release.yml's ldflags. QEMU is only used for the small Debian runtime stage of the arm64 image — the Go compile cross-compiles natively.

### Vulnerability Scan Workflow

**File:** `.github/workflows/vuln-scan.yml`
**Trigger:** weekly `schedule` (Mondays 06:17 UTC), plus `workflow_dispatch`
**Permissions:** `contents: read`

Runs `govulncheck ./...` (pinned, like staticcheck) and `npm audit` on the two npm projects — the sidecar's runtime dependencies and the frontend test harness, failing at moderate and above. govulncheck fails only when the code reaches the vulnerable symbol. It needs every package to load, so two empty files stand in for the embed blobs instead of building them. Deliberately not a step in ci.yml: an advisory is published on the database's schedule, not on a commit, and would turn an unrelated pull request red. A failed scheduled run is emailed by GitHub to whoever last changed the workflow's schedule.

---

## Release Process

This is the manual process performed by the developer before CI takes over:

1. **Generate `RELEASE_NOTES.md`** — Run `git log --oneline <prev-tag>..HEAD` and group commits into sections: Features, Improvements, Bug Fixes, Internal. Omit empty sections. No top-level heading in the file.
2. **Bump version** — Edit `cmd/moombox/main.go`, change `version = "x.y.z"` to the new version.
3. **Commit both together** — `chore: bump version to x.y.z — short summary`
4. **Tag** — `git tag vx.y.z`
5. **Push** — `git push && git push origin vx.y.z`

The tag push triggers release.yml, which builds, signs, and publishes the binaries and the image, and runs the test suite on the tagged commit beside them; a red suite shows in that run without holding the release. To exercise that pipeline before step 4, run release.yml by hand from the Actions tab ("Run workflow") — the dry run described under Release Workflow.

### Version Format

Semver: `MAJOR.MINOR.PATCH`. The `v` prefix is used only on git tags (`v2.3.20`). The `version` variable in source code stores it without the prefix (`2.3.20`). The `init()` function strips any `v` prefix in case ldflags includes it.

The `commit` variable is resolved at build time via ldflags, or at runtime from `debug.ReadBuildInfo()` if ldflags are absent (local development builds).

---

## Self-Update Flow

The self-updater lives in `internal/updater/`. It checks GitHub Releases, verifies the release's signed manifest, downloads the new binary, verifies its signature and its SHA-256 against the manifest, and replaces the running executable.

### Step-by-Step

1. **Check** (`updater.go: CheckForUpdate`) — Queries `https://api.github.com/repos/vampiricwulf/Moombox/releases/latest`. Compares the remote version against the current version using semver comparison. Returns `nil` if already up-to-date, or a `ReleaseInfo` struct if a newer version exists. HTTP timeout: 10 seconds. An up-to-date answer from any check — the periodic one, the Web's "Check for updates", the TUI's `R V` — withdraws a release still pending (`routes.ClearPendingUpdate`): it was pulled from GitHub, and its download no longer exists. Only the release pending when the check STARTED is withdrawn — another check can find one during this one's GitHub round trip. Both UIs drop the badge (`update_cleared`, and the TUI's tagged clear). A release kept that way is the check's answer: `POST /api/update/check` reports it as available and `R V` as found (`checkUpdateFromTUI`). And the UI that asked drops its own badge on an up-to-date answer only while it still shows the release it showed when the check started — a release that reached it during the round trip was found after the answer and is still pending.

2. **Verify the manifest** (`manifest.go: verifiedManifestEntry`) — Before anything else is downloaded, fetches the release's `moombox-manifest.json` and `moombox-manifest.json.sig` (recorded by the check) to a temp directory, refuses one over 64 KiB, verifies its signature with the embedded public key, and refuses unless its version and tag are exactly the release being applied, that version is newer than the running one, and it has an entry for the running GOOS/GOARCH naming the asset step 3 downloads. A release that publishes no manifest, or publishes it without its `.sig`, is still reported by the check, but its apply is refused, and how depends on the release. Every release from `FirstManifestVersion` (2.9.0) on — which covers every release a binary carrying this check can be offered — is published with the signed manifest, so for one of those its absence is a failure, the missing asset named and no manual install advised ("release vX publishes no manifest (moombox-manifest.json), though every release from 2.9.0 on is published with a signed one — refused: … so it must not be installed by hand either"): a binary installed by hand would fail `VerifyCurrentSignature` for the same assets (§ Signature Verification of Current Binary). A release before it never had a manifest, and the refusal points at its release page: "release vX publishes no signed manifest … update manually: download it from <release page> and replace the binary". See [security.md](security.md) § Release Manifest.

3. **Download binary** (`updater.go: ApplyUpdate`) — Downloads the running platform's binary asset from the release — `Moombox.exe`, `moombox-linux-amd64` or `moombox-linux-arm64`, chosen by GOOS/GOARCH through `releaseAssetMap` — to `<exe-path>.new`. A separate client from the 10-second API client, since binaries are 78-87 MB: what ends a download is a STALL — no byte for `downloadStallTimeout` (60 s), response headers included — reported as "download stalled: no data for 1m0s". A total deadline only backstops it (`downloadMaxDuration`, 2 hours, about 12 KB/s for the largest binary). The Web's apply runs detached from its request (`context.WithoutCancel`): a browser stops waiting for the response long before a slow download ends (Firefox after 300 s), and the abort used to cancel the download with it; the dashboard now reads a lost response as "the update may still be downloading", and the restart follows when it lands. The 5-minute total deadline this used to carry killed every download slower than about 2.3 Mbit/s however steadily it was arriving.

4. **Download signature** — Downloads that asset's `.sig` (`Moombox.exe.sig`, `moombox-linux-amd64.sig`, …) to `<exe-path>.new.sig`.

5. **Verify** (`signing.go: VerifySignature`, then `manifest.go: verifyFileSHA256`) — Reads the `.new` binary and `.new.sig` file. Verifies using the **embedded Ed25519 public key** (`71ce2f926296a552950faa1fd7d3e89574e14ec353aa253f2577f6883fdf51eb`). Signature must be exactly 64 bytes. Then hashes `.new` and compares it with the manifest's SHA-256 for this platform — a validly signed binary that is not the one this release published for this platform fails here. On failure: `.new` and `.new.sig` files are deleted, error is returned to the caller.

6. **Replace** — remove a stale `.old` file if one exists, then keep the running binary at `.old` and place `.new` at the plain name:
   - **Linux**: `.old` is a hard link to the running binary (`keepBackupByLink`), and `current.new` -> `current` is one rename over it, so the plain name always holds the old binary or the new one — a kill or power loss mid-swap cannot leave it empty. If the rename fails, the link and `.new` are removed and the running binary is untouched. (A filesystem without hard links falls back to the Windows sequence.)
   - **Windows**, which cannot rename over a running image: `current.exe` -> `current.exe.old` (rename running binary out of the way), then `current.exe.new` -> `current.exe` (place new binary). For those milliseconds the plain name is empty. If the second rename fails: attempts rollback by renaming `.old` back to the current path. If rollback also fails, logs an error and writes `<exe>.update-broken` (binary may be in an inconsistent state; `.new` is kept).

7. **Breadcrumb** — After a successful swap, `ApplyUpdate` writes `<exe-path>.update-pending` containing the target release tag. The next boot resolves it: a boot running that version deletes it (update landed) — and first deletes an `.update-failed` marker OLDER than the breadcrumb, logging its path: a later update has applied successfully, so that marker's rollback instructions are stale (`clearSupersededFailureMarker`, `cmd/moombox/helpers.go`; a marker newer than the breadcrumb is this very update's own failed first launch and stays, and `.update-broken` is never touched); a boot running a *different* version alongside a failed-update marker (the launcher auto-rolled back — see below) records the tag as `updates.skipped_version` so automatic checks stop offering the broken release (a manual "Check for updates" still retries it deliberately), then deletes it — and does not announce its version change as `update_applied` (`rolledBackRelease`, `announcesVersionChange`, `resolveUpdateBreadcrumb`, `cmd/moombox/helpers.go`): the marker announces the rollback. The launcher reads the tag at the restart that arms the boot (`pendingUpdateTag`) and writes the breadcrumb back before it rolls back (`restoreUpdateBreadcrumb`), since the update's own boot deletes it at its milestone and a Windows first update is still rolled back after that. Binaries that predate the breadcrumb ignore it; stale copies are inert until the next aware boot cleans them up.

8. **Restart** — The caller invokes `triggerRestart("update")`, which exits with code 42. The launcher respawns, picking up the new binary. Until it does, the process is restart-pending: a placed update latches the Updater's `applied` flag and every later `ApplyUpdate` in that process is refused ("an update is already applied — restart pending"). A second apply — `R U` pressed again inside the restart's grace window, or a TUI apply after a Web one — would otherwise make `.old` the binary the first apply had just placed, so the running binary, the only rollback artifact, would be gone from disk. A failed apply changed nothing and does not latch. The TUI drops its update badge and `R U` once an apply succeeds.

9. **Cleanup** (`updater.go: CleanupOldBinary`) — Called at the first-successful-boot milestone (database opened, web bind resolved). Removes stale `.old`, `.new`, `.new.sig` and `.failed` files left by previous updates, interrupted downloads or an automatic rollback; on Windows it also sweeps an orphaned `~`. `<exe>.sig` is deliberately spared — Moombox never writes it, and it is the published signature asset a manual verifier leaves beside the binary.

### Automatic Rollback

If the **first boot of a freshly-applied update** fails within `postUpdateFailureWindow` (2 minutes) while its rollback artifact is still on disk — or the new binary fails to even start — the launcher rolls back automatically (`launcher.go: attemptAutoRollback`): the broken binary is KEPT as `<exe>.failed` and named in the `.update-failed` marker — the restored (older) binary may refuse a database the new version already migrated, and its refusal message points the operator at exactly that file, named from the RUNNING binary's own filename — a Linux install reads `moombox.failed` and `moombox.update-failed`, not the Windows spelling the message used to hard-code, and it no longer mentions a `.old` artifact, because an automatic rollback renames the rollback artifact back to the plain name and leaves none. The two suffixes are mirrored by `RollbackArtifactSuffixes` (`internal/database/migrations.go`) — package main is unimportable and `internal/database` imports nothing internal, so `TestDowngradeRefusalNamesTheLauncherArtifacts` (`cmd/moombox/rollback_artifact_names_test.go`) is what keeps the two sides from drifting — the preserved rollback artifact is renamed back to the plain name (the file `handleUpdateRestart` named at the restart that armed the boot, recorded then in `postUpdateBoot` rather than looked up at the exit: on Windows, `cmd/moombox/launcher_windows.go`, `<exe>~` when it renamed `.old` there — every first update of a launcher lifetime — and `<exe>.old` when it could not; on Linux, `cmd/moombox/launcher_unix.go`, always `.old`), the marker documents the rollback and names the kept `.failed` path, and the restored binary is respawned as a fresh launch. The next boot announces the marker as a notification and — via the `.update-pending` breadcrumb, which the launcher writes back first in case the failed boot had already deleted it — marks the failed version skipped, and announces no `update_applied` for the change back. Before that write, a Windows first update rolled back after its milestone left the restored boot with no breadcrumb: the release was offered again by the daily check, and the version stamp it had written made the rollback go out as "updated … and restarted successfully". Rollback ping-pong is impossible: the restored binary is not "first after update", so a quick death of it takes the normal fail-fast path. On Linux the broken binary is kept at `.failed` by a hard link (`keepAsideByLink`, `cmd/moombox/launcher_unix.go`) and the artifact is renamed over the plain name in one step, so the plain name is never empty and a failed restore leaves the broken binary there (its link removed); Windows moves it aside first, with retries, because only that step can be retried around a scanner still holding the fresh download. When the restore itself fails, the launcher falls back to preserving what remains with written manual-recovery instructions (`preserveUpdateRollback`). A boot whose artifact is already gone (it survived to the milestone sweep before dying) never takes this path; it is the supervised crash of the next paragraph.

A boot whose artifact is already gone has nothing to roll back to (`postUpdatePastRollback`, `cmd/moombox/launcher.go`): on Linux it reached the first-successful-boot milestone, whose sweep removed `.old`, and its exit is an ordinary crash — supervised, so it is respawned however soon it comes rather than taking the fail-fast arm. Such an exit used to end the launcher on preserve-with-instructions, with nothing to preserve, where any other boot's crash is respawned. Windows keeps the launcher's `~` image past that sweep, so where `~` is the artifact a release that crashes soon after starting is still rolled back while the window lasts. Where it is not — a later update in the same launcher lifetime, whose `.old` could not be renamed over that image — the boot is judged by its `.old`: once its milestone sweeps it, the boot is past rollback as on Linux. The `~` file is then two versions back, and restoring it used to roll the install back past the release in between, which the milestone had just deleted.

A deterministic startup failure (exit code 3, `exitCodeStartupError`) is never treated as a broken update: `classifyPostUpdateExit` sends it down the preserve path instead, the rollback is skipped, the artifact is preserved with written instructions, and — because the next boot runs the same version the `.update-pending` breadcrumb names — the release is not marked skipped. The environment failed, not the binary.

The kept `.failed` file is swept by `CleanupOldBinary` at the next boot's first-successful-boot milestone, so it costs one binary's worth of disk only until a boot succeeds. That is also exactly when it is still needed: if the restored (older) binary cannot open a database the newer version already migrated, it dies inside `initServices` well before the sweep, so the file its refusal message names is still there.

### Signature Verification of Current Binary

`VerifyCurrentSignature` allows verifying the running binary against its published release. It fetches the `.sig` file for the current version's tag from GitHub, downloads it to a temp file, and verifies against the running executable; then it verifies the release's signed manifest (`moombox-manifest.json` and its `.sig`) and checks that it names the running release and that the running platform's entry hashes to the running executable — the `.sig` alone would also pass a validly signed binary of another release or platform. From `FirstManifestVersion` (2.9.0, the first release cut by the manifest pipeline) on, a release that publishes no manifest, or an unsigned one, fails the verify with the missing asset named. A release before it publishes none and is verified by its `.sig` alone, and the Web (`manifest: false` from `POST /api/update/verify`) and the TUI (`R S`) say so in the warning colour instead of calling it the full check ([security.md](security.md) § Release Manifest). This is used for integrity checks (e.g., verifying the binary hasn't been tampered with post-install). Returns an error for local/dev builds that have no corresponding GitHub release.

### Error Handling

| Scenario | Behavior |
|----------|----------|
| GitHub API unreachable | Error returned, no update attempted |
| Running on a platform `releaseAssetMap` does not list | Error returned ("auto-update unsupported on <os>/<arch>") |
| No binary asset for this platform in the release | Error returned ("no <asset> asset found in release <tag>", e.g. "no moombox-linux-arm64 asset found in release v2.8.10") |
| No `.sig` asset in release | Error returned ("no signature file found") |
| No `moombox-manifest.json` (or its `.sig`) in a release from `FirstManifestVersion` (2.9.0) on | Offered by the check (a Warn line says it will be refused and must not be installed by hand); the apply fails before any download, naming the missing asset, with no manual install advised ("… publishes no manifest … though every release from 2.9.0 on is published with a signed one — refused") — installed by hand, it would fail Verify Signature too |
| No `moombox-manifest.json` (or its `.sig`) in a release before 2.9.0 | Offered by the check (a Warn line says it must be installed manually); the apply is refused before any download ("… publishes no signed manifest … update manually") |
| Manifest signature invalid, over 64 KiB, malformed, for another version or tag, not newer than the running version, or without this platform's entry | Refused before the binary is downloaded; error names the cause |
| Binary's SHA-256 differs from the manifest's | `.new` cleaned up, error returned ("manifest check failed: … does not match the signed manifest's …") |
| Download fails | `.new` file cleaned up, error returned |
| Signature verification fails | `.new` and `.new.sig` cleaned up, error returned |
| Rename of current binary fails | `.new` cleaned up, error returned |
| Rename of `.new` to current fails | Rollback attempted (`.old` -> current), error returned |
| An update was already applied in this process | Refused before downloading ("an update is already applied — restart pending") |
| An earlier swap failed both ways (`.update-broken` present), or nothing is at the exe path | Refused before downloading ("… recover by hand before updating"): that state's only binary is `.old`, and a retry overwrote the kept `.new` and removed `.old` before its rename failed for want of an exe (`swapLeftBroken`) |

---

## Launcher/Supervisor Pattern

### Purpose

The launcher enables graceful restarts without process chain buildup. When Moombox needs to restart (config change, update applied, setup wizard completion), it exits with code 42 and the launcher respawns it. This picks up any new binary on disk (for updates) and gives the child a clean process state.

### Mechanism

**Environment variable:** `_MOOMBOX_CHILD`

- **Parent process** (no `_MOOMBOX_CHILD` in environment): Runs `launchAndSupervise()`. Spawns itself as a child process with `_MOOMBOX_CHILD=1` and waits.
- **Child process** (`_MOOMBOX_CHILD=1`): Runs the full application service stack.
- **Instance lock** (`acquireSingleInstanceLock`, `cmd/moombox/single_instance_unix.go` / `_windows.go`): the launcher takes it before its first child. On Unix it is a `flock` on `$HOME/.local/share/moombox/moombox.lock`, and the launcher hands every child a descriptor on the same locked file (`shareInstanceLock`, `_MOOMBOX_INSTANCE_LOCK_FD`), which the child keeps for its whole life, close-on-exec so nothing it starts inherits it (`holdInheritedInstanceLock`, first thing past the `_MOOMBOX_CHILD` gate in `main`). A flock is released only when the last descriptor on the file closes, so the lock is held until the launcher and its child are both gone: a launcher killed with `kill -9` while its child spends up to `forceExitAfter` shutting down (its parent-death SIGTERM) no longer lets a second instance start beside that child — the start is refused with "another moombox instance is already running, or the last one is still shutting down" until it has exited. Before, the lock went with the launcher, and the second instance's child ran on the same database as the first: its boot sweep took the first child's live temporary output and its boot re-mux of the same `Muxing` row ran beside the first's. `releaseSingleInstanceLock` closes its descriptor and never unlocks (`LOCK_UN` through either descriptor would release the lock for both). On Windows nothing is passed: the launcher's kill-on-close Job Object ends the child with the launcher.
- **Daemon record** (`cmd/moombox/daemon_record.go`): which database the running daemon has open, for `moombox add` ([architecture.md](architecture.md) § Subcommands and Flags). The launcher takes the lock and the child opens the database, so before every spawn the launcher removes the record a previous child left — a crashed one removes nothing — and names its path in `_MOOMBOX_DAEMON_RECORD` (`childDaemonRecordEnv`; set to no path when it holds no lock with a directory, the Windows session mutex alone); the child writes it atomically once its database is open (`writeDaemonRecord`, in `initServices`) — its PID, the database file it opened (`Database.File`: `database_path`, or the file an earlier release kept that path's database in, `database.ResolveFile`), its config file and its working directory, all absolute and resolved as the OS resolves them (`resolveUnder`) — and removes it when the database closes. The launcher removes it too when it takes the lock and when it releases it. It is `moombox-daemon.json` beside the lock file — a file of its own, since the byte `LockFileEx` locks on Windows cannot be read through another handle — and `add` trusts it only while the lock is held (`daemonLockHeld`, a shared non-blocking flock or `LockFileEx` tried and dropped at once), so a record a dead daemon left is never read as a running one's. A launcher from before the record names nothing — and the supervisor keeps running the binary it started from across every in-place update, the in-app Restart included — so a child whose launcher left `_MOOMBOX_DAEMON_RECORD` unset, but set `_MOOMBOX_LAUNCHER_VERSION`, finds the record's place itself while a lock is held there, removes the record a crashed predecessor left before its database opens, and records and removes its own there (`childDaemonRecordPath`): without that, `add` refused every add after an in-app update into this release until Moombox was stopped and started. A child run by hand, with no `_MOOMBOX_LAUNCHER_VERSION`, writes none; nor does one under a launcher from before the version handshake as well (v2.7.1 and earlier), which it cannot tell from one run by hand, so `add` refuses under such a launcher until a full stop/start.
- **Stop forwarding** (`forwardStop`, `cmd/moombox/launcher.go`): the launcher catches the stop signals (`stopSignals`, `cmd/moombox/stop_signals_unix.go` / `stop_signals_windows.go`) — Ctrl+C (SIGINT), SIGTERM and, on Unix, a hangup (SIGHUP) — instead of dying from them, passes each on to the child as the same signal (killed only if it cannot be signalled), records the stop, and waits for the child's shutdown. A stop that ended only the launcher would leave the child running unsupervised (it keeps the instance lock until it exits — see Instance lock above). A signal sent to the launcher's PID alone reaches nothing else — `kill`, `docker run` attached without `-t` proxying Ctrl+C to the container's main process, a supervisor that signals the main PID — and Ctrl+C used to be ignored there, so nothing stopped. A signal sent to the whole process group (a terminal's Ctrl+C, the hangup a closed terminal window or a dropped SSH session sends) reaches the child twice, which it shrugs off (`catchStopSignals`), and reaches nothing Moombox started: on Linux every FFmpeg and ffprobe run is in a process group of its own (`Setpgid`, `internal/childproc/tie_linux.go`), as the BotGuard sidecar and the cookie browsers already were, so the shutdown ends it on its own schedule. In the launcher's group FFmpeg died of the signal first, while its mux was still live: the mux was recorded as failed and the row went to `Error`, which the next start's re-mux skips, instead of staying `Muxing` for it; a trim was reported failed instead of stopped. On Windows FFmpeg still shares the console, and with it a Ctrl+C at a `--headless` console and the console's close. A hangup the process inherited as ignored (`nohup`) stays ignored by both processes, so a nohup'd Moombox does not stop when its shell exits. With no child to hand a signal to (before the first spawn, in a crash backoff) the launcher exits with 128 plus the signal's number. On Windows the launcher records the stop and sends nothing: Go raises SIGTERM there only for the console close, logoff and shutdown events, and `os.Interrupt` only for Ctrl+C and Ctrl+Break, which reach every process on the console, so the child already has its own and is shutting down gracefully inside Windows' grace period. Signalling cannot deliver either on Windows, and the `Kill` fallback the launcher used to take was `TerminateProcess` milliseconds into that shutdown.
- **The child's stop signals** (`catchStopSignals`, `stopOnSignal`, `cmd/moombox/stop_signals.go`): the same set ends the run's context and starts the graceful shutdown, and every later one is caught too, so a repeated signal mid-shutdown — a hangup comes from the terminal, the shell and the launcher — cannot kill the child with its default action (`signal.NotifyContext`, used before, unregistered the signals when the TUI exited). The TUI quits when the context ends (`tui.Run`), since bubbletea handles SIGINT and SIGTERM but not SIGHUP; and after a hangup with stdout on that terminal the logger stops writing to stdout (`muteHungUpTerminal`) — moombox.log and the dashboard still get every line — so the shutdown makes no attempt on a terminal that is gone.

**Parent behavior on child exit** (the `switch` in `launchAndSupervise`, `cmd/moombox/launcher.go`, in this order):
- A launcher-forwarded stop (Ctrl+C, SIGTERM or a hangup): propagate the child's code without respawning, whatever
  the code — a restart the child exits 42 for included, which the launcher exits 0 for (`propagatedExitCode`) — and
  without writing an update-failed marker: stopping the service right after an update must not leave a scary
  "update failed" marker behind. A stop that landed in a restart's drain used to be answered with a respawn: on a
  terminal that had just closed, the new child ran headless on the deleted pty, recording and holding the instance
  lock where nobody could see or stop it. The child lets a stop override its pending restart too (`restartPending`,
  §Restart Triggers); this rule covers a stop that reaches it only after it settled on 42.
- Exit code 42 (`exitCodeRestart`): Respawn the child (loop continues). If `<exe>.old` exists (from an
  update), rename it to `<exe>~` to free the `.old` name for future updates. A rename that fails — the
  `~` name is still held by this launcher's own mapped image, which happens on the second update of one
  launcher lifetime — is reported on stderr and leaves `.old` in place as the rollback artifact.
  A restart that found `.old` arms the next child as that update's first boot and records its
  rollback artifact (`postUpdateBoot`, `cmd/moombox/launcher.go`, which judges that boot's exit and
  rolls it back); a config restart inside a still-unproven first boot's `postUpdateFailureWindow`
  carries both forward, and any other exit of that boot ends the one-shot.
- Normal exit (code 0): Terminate.
- A non-zero exit from the FIRST boot after an update, inside `postUpdateFailureWindow` (2 minutes)
  and with its rollback artifact still on disk: automatic rollback (above), unless the code is
  `exitCodeStartupError`, which is preserved-with-instructions instead — whenever it arrives, since
  its timing is the operator's keypress. With the artifact gone the exit is an ordinary crash, and a
  supervised one (below) however soon it comes.
- Exit code 130 or 143 (128+SIGINT / 128+SIGTERM): propagate — user intent.
- Exit code 3 (`exitCodeStartupError`) otherwise: propagate, whatever the timing and whether or not
  the child was a respawn — a respawn hits the same wall.
- Any other non-zero exit within `launcherHealthyWindow` (60 s) on a FRESH launch: propagate and
  terminate — a deterministic startup failure must fail fast and visibly, not crash-loop against the
  same wall.
- Any other non-zero exit from a child that had proven it can run, or from a respawned child: crash
  supervision respawns it with 1/2/4/8/16 s backoff (`crashBackoff`, doubling and capped at one
  minute), giving up after `maxConsecutiveCrashes` (5) consecutive quick deaths. For a 24/7 unattended
  archiver a dead-until-noticed daemon is the worst outcome.

**Crash output** (`cmd/moombox/crash_output.go`): what crash supervision respawns is almost always a
crash the logger never saw — a fatal runtime error, a panic in a goroutine without a recover, a fatal
signal — and the Go runtime writes those to stderr only, which in TUI mode is the alternate screen the
respawned TUI clears. So each child gives the runtime a second place to write it
(`armCrashOutput`, `debug.SetCrashOutput`): `<log name>-crash.log` beside the log
(`crashOutputPath`, `moombox-crash.log` for the default), created empty as soon as the logger is open.
The next start that finds output there (`takePreviousCrash`, any start, not only a crash respawn —
the launcher's give-up and fail-fast exits tell the operator to check the logs) logs an ERROR with the
output's first line as `cause` (for a fatal error, whose own `fatal error: …` line the runtime prints
before it counts itself as crashing and so to stderr only, the first frame it struck in outside the
runtime — for one raised on the system stack, a stack overflow or running out of memory, whose output
starts at a `runtime stack:` block, the first such frame of the goroutine after it: the recursing
function, the allocation site — `crashCause`) and its first 8 KiB (`crashExcerptBytes`, cut at a line, saying how
much it left out) as `output`, with every URL cut to its origin and any PO token removed
(`redactCrashText`), then keeps the whole output as `<crash file>.1`, replacing the crash before it
and cut to 1 MiB (`crashKeepBytes`) — at most two crashes' output on disk. When the crash file cannot be
moved there (on Windows, a viewer, a scanner or the indexer holding either file without delete sharing;
anywhere, a `.1` that rename cannot replace) it is copied there instead (`copyCrashOutput`), since the
start's own capture empties the crash file; when the copy fails too, the output is not kept beyond the
logged excerpt, and the log line (`crashOutput="not kept"`) and the notice say so rather than naming a
file that no longer holds it. A crash respawn's `crash_recovered` notification names that cause and the
kept file (`crashRecoveredNotice`), or says the process left no crash output (a SIGKILL or the OOM killer
reports none) instead of sending the operator to logs that hold nothing more.

**Windows-specific details:**
- The parent catches `os.Interrupt` (Ctrl+C, Ctrl+Break) and SIGTERM (console close, logoff, shutdown) and sends nothing on: each event reached every process on the console, the child included. It records the stop, so the child's exit propagates.
- The `createNoWindow` flag (`0x08000000`) is used when spawning cleanup processes, not the child itself. The child inherits the parent's console (stdin/stdout/stderr are piped through).

**Old binary cleanup:** After an update, the old binary is at `<exe>.old` but is locked because the launcher (parent) is still running from the old binary. The launcher renames `.old` -> `<exe>~`. On exit, the launcher spawns a detached `cmd /C ping 127.0.0.1 -n 5 >nul & del /f /q "%MOOMBOX_OLD_LAUNCHER%" >nul 2>nul` process to delete the stale file after the launcher fully exits — 5 pings at the 1 s default interval is about 4 s of wall-clock delay, enough for the launcher to release the file lock. `ping` is used rather than `timeout` because it has no stdin dependency in a detached process. The path reaches `del` through that environment variable, never as text on the command line (`deferDeleteCommand`, `cmd/moombox/launcher_windows.go`): as a bare argument, an install directory like `D:\Tools&Apps` split the line at its `&` and `del /f /q D:\Tools` silently emptied the sibling directory, and a `%NAME%` in the path was expanded.

When that rename cannot happen, the surviving `.old` is the freshest previous binary and the `~` file is
one version older still; `handleUpdateRestart` names `.old` as the update's artifact for exactly that
reason, and the `~` file never stands in for it, even after the boot's milestone has swept it. The `~` file is
swept by the next launcher start once neither failed-update marker (`.update-failed`, `.update-broken`)
remains (`cleanupOrphans`, `cmd/moombox/launcher_windows.go`) — or by a healthy boot's `CleanupOldBinary`,
whenever no running launcher still holds it. An `.update-failed` marker does not stand forever: once a
LATER update has landed, its boot deletes the marker (`clearSupersededFailureMarker`, step 6 above), and
the launcher's `~` cleanup — the deferred delete on exit and the startup sweep — resumes.

### Restart Triggers

All restart triggers call `triggerRestart(source)` (`cmd/moombox/services.go`), which:
1. Logs `"Restart requested"` with the source string
2. Sets `restartRequested.Store(true)` (atomic bool)
3. Starts the web server's drain (`StartDrain`), so no new request is taken
4. After a 5-second grace — time for the response that asked for the restart to reach its client — calls `cancel()` to cancel the root context (propagates to all services) and `quitTUI()` if the TUI is running

A stop signal (Ctrl+C, SIGTERM, a hangup) overrides a requested restart, whether it arrives in the drain, during the shutdown or before the request (`restartPending`, `cmd/moombox/stop_signals.go`): the run ends with exit 0 and the log says `"A stop signal arrived while a restart was pending: stopping instead of restarting"`. An update the restart was to start is already on disk and runs at the next start, as an ordinary launch.

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
2. **Stop the trim service** — `TrimService.Stop` cancels every trim it runs (a dashboard's detached one, a TUI's in-process one, a finished job's post-download one), waits up to 2 seconds for each to remove its `.partial.mp4`, and refuses any trim asked for after it. A stopped trim sends no `trim_error`: nothing failed, and a UI that asked has its answer. The exception is the post-download trim, which nobody asked for from a dialog: it sends `trim_error` itself (`postDownloadTrim`)
3. **Stop download worker** — Waits for active downloads to save state (resume files)
4. **Flush notifications** — `notifyMgr.BeginShutdown()` ran before step 1, so every embed emitted during the stop is a SINGLE attempt; `notifyMgr.Wait()` then closes each target's queue and drains what is already in it. The real bound is the process's own 15-second force-exit, not `Wait`'s 30-second timeout: step 2 can spend up to 2 of those seconds (`worker.TrimStopWait`) on a trim whose FFmpeg will not die, and step 3 can legitimately spend 12 (`worker.StopBudget`) waiting out in-flight jobs and their muxes, so the drain gets whatever is left — as little as 1 s. An embed emitted during a shutdown with a slow Discord is lost, by design (owner ruling) — extending the force-exit would trade a hung shutdown for one embed. So is one a target is holding through an outage when the drain closes it: with the connectivity monitor still reporting offline, the queue discards what it holds rather than spend single attempts on the dead link (see Dispatch Behavior, Outage hold).
5. **Stop cookie services** — CookieRefresh, then AutoCookies. `AutoCookieService.Stop` ends the service's own two goroutines — the periodic browser refresh and the boot profile seed — kills a refresh browser, and waits up to 2 seconds (`cookies.AutoCookieStopWait`) for a pass in flight to return — the post-pass auth re-check included, which runs on the same goroutine under the same context (`OnPassCompleted(ctx)`), so the run context's cancel at the start of the shutdown already ends its validate round trips (a re-check cut short is inconclusive and flags no auth loss). A pass that observes the cancel has therefore finished writing `cookies.txt` and its meta file, and fired its callbacks, before the stop returns; one with a step that ignores the cancel past the 2 seconds is logged (`auto-cookie refresh pass still running after stop`) and left running, and can still write after the stop. A pass that ends after its goroutine's context did fires no post-pass re-check. The wait is normally milliseconds (the pass is cancelled first) and is not budgeted into the force-exit: a backstop that fires during it exits with the pass where it is, as every shutdown did before the wait existed
6. **Cleanup PO token provider** — Releases Goja VMs
7. **Stop web server** — Closes HTTP listener and WebSocket connections
8. **Unsubscribe event listeners** — Log forwarder, WebSocket job update subscribers
9. **Close database** — Flushes pending writes, closes SQLite connection

Each service stop is wrapped in `stopService(name, fn)` which provides:
- Panic recovery (one failing service cannot block shutdown of others)
- Debug logging of each service stop

### Force-Exit Timer

A `time.AfterFunc` timer (`forceExitAfter`, 15 seconds) starts at shutdown entry. It is the download worker's whole stop budget (`worker.StopBudget`: a 10-second wait for in-flight jobs, then mux cancellation and a 2-second grace) plus a 3-second margin — it must outlast that budget and the trim service's stop ahead of it (up to 2 seconds, `worker.TrimStopWait`) together, because its clock starts first and a shorter backstop would exit before the worker cancelled its muxes. (What that cancel was first for — FFmpeg left writing an archive the restarted child re-muxes with `-y` — no longer rests on it: FFmpeg dies with the process and writes only a temporary file of its own, § Runtime Dependency: FFmpeg. A mux the exit kills instead of cancels leaves that temporary file for the next start's sweep.) What the trim stop and the worker leave of the margin is the notification flush's; the timer is not widened for the trim stop, since the owner's ruling caps a graceful shutdown at 15 s. If graceful shutdown has not completed in time, the timer fires:
1. Logs `"Graceful shutdown timed out, forcing exit"`
2. Closes the rate limiters, the database (final WAL checkpoint) and the log (flushing buffered lines)
3. Exits with code 42 when a restart is pending (`restartPending`: requested, and no stop signal overrode it), else 0 — never 1, which the launcher would treat as a crash and respawn a daemon the user just quit

After graceful shutdown completes, if a restart is pending (`restartPending`), the child process exits with code 42 (triggering launcher respawn). Otherwise, it exits with code 0.

---

## Signing Tool

**Location:** `cmd/sign/main.go`

A standalone CLI tool used exclusively by CI to sign release binaries and the release manifest.

### Usage

```bash
# Sign a binary (reads SIGNING_KEY from environment)
go run ./cmd/sign Moombox.exe
# Output: Moombox.exe.sig (raw 64-byte Ed25519 signature)

# Write the release manifest for the binaries in -dir (default .), sign it,
# and verify both (reads SIGNING_KEY from environment)
go run ./cmd/sign -manifest -version 2.9.0 -tag v2.9.0
# Output: moombox-manifest.json + moombox-manifest.json.sig

# Generate a new key pair (one-time setup)
go run ./cmd/sign -genkey
# Output: public key (for embedding in signing.go) + private key (for GitHub secret)
```

### Details

- **Algorithm:** Ed25519 (deterministic, no randomness needed at sign time)
- **Private key source:** `SIGNING_KEY` environment variable (hex-encoded, 128 hex chars / 64 bytes)
- **Output:** `<input-path>.sig` containing the raw 64-byte signature
- **Self-check:** after writing the `.sig`, the tool runs `VerifySignature` on it — the same call an installed Moombox makes, against the public key in the source tree being released. Ed25519 signs with any well-formed key, so a wrong or rotated `SIGNING_KEY` would otherwise produce a green release whose update every existing install refuses. On a mismatch the `.sig` is deleted and the tool exits non-zero, which fails the release before the publish step.
- **Manifest (`-manifest`):** `updater.BuildManifest` hashes every platform binary `releaseAssetMap` lists (a missing one fails the step), the JSON is written to `moombox-manifest.json` and read back through `updater.ParseManifest` — the parser installs run — then signed and self-checked like a binary. On any failure neither file is left for the publish step.
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
| Shorthand | `discord://123/abc` | Expanded to `https://discord.com/api/webhooks/123/abc`, keeping a query (`?thread_id=…`) whether or not a slash comes before it; any path past the token is dropped |

URL validation rejects non-HTTPS Discord webhook URLs and URLs with invalid ID/token structure. Unsupported URL schemes are logged as warnings and skipped.

Every log line and error that names a notification URL carries `redact.URLOrigin`'s form of it (`internal/redact/url.go`), never the URL: an `http`/`https` URL is reduced to its scheme and host name (`https://discord.com/…<redacted>` — no userinfo, port, path, query or fragment), any other scheme to the scheme alone (`tgram://…<redacted>`), and a string with no `scheme://` to `…<redacted>`. An `http`/`https` URL with an `@` past the first `/`, `?` or `#` after its `://` keeps its scheme alone too: net/url ends the authority at that delimiter, so a credential holding one (`https://tk_SECRET/half@host`) would be read with its first half as the host. The rule has to be that strict because nothing validates an entry before `buildTargets` logs it — `config.Validate` does not check notification URLs and a disabled entry is never parsed — so a hand-edited URL can carry its credential anywhere: in the userinfo (`https://user:password@host`, `ntfys://token@host`), in the authority (`tgram://<bot token>/<chat>`), in a query with no path before it, or in Discord's own webhook path. The same form replaces the URL in a Discord request's construction and transport errors (`DiscordWebhook.do`, `internal/notifications/discord.go`), which reach the queue's failure log and the test route's response.

### Target Options

Each entry in the `[[notifications]]` array is a `NotificationConfig` (`internal/config/types.go`) and carries four options besides its URL and event filter:

- **`enabled`** — a mute switch. `false` keeps the target, its filter and its mention in the config but delivers nothing; absent means enabled (`IsEnabled`, `internal/config/notifications.go`). `buildTargets` (`internal/notifications/manager.go`), called by both `NewManager` and `Reload`, skips a disabled target before it is ever given a queue and logs the skip at Info with the URL redacted. `HasTargets` (`internal/notifications/manager.go`) reports false once every configured target is muted (same as no targets at all), which correctly short-circuits the three producer guards that check it before building an embed nobody would see: the update-available check in `cmd/moombox/helpers.go`, the Twitch Stream Found site in `cmd/moombox/monitor_callbacks.go`, and the TUI cancel (`cancelJobFromTUI`, `cmd/moombox/job_notifications.go`). The YouTube Stream Found, sent by the worker (`sendStreamFound`), builds unconditionally like the disk alerts. The disk alerts (`cmd/moombox/disk_alerts.go`) build unconditionally; with every target muted, `Send` has no queue to hand them to.
- **`mention` + `mention_events`** — `mention` is a Discord ping: a role (`<@&ROLE_ID>`), a user (`<@USER_ID>`, also accepting the legacy `<@!USER_ID>` nickname spelling), `@everyone`, or `@here`. `ParseMention` (`internal/config/notifications.go`) validates and canonicalises it; `MentionParse` (`internal/notifications/discord.go`) turns the canonical text into the `allowed_mentions` object Discord requires beside a ping in `content` — a role or user ping produces an empty `parse` list plus the id under `roles`/`users`, `@everyone`/`@here` produce `parse: ["everyone"]`. `mention_events` picks which events carry the ping and is alias-aware the same way an event filter is (`mentionFor`, `internal/notifications/queue.go`): a target that mentions on a legacy event name is still pinged for the event it split into. A close (`disk_ok`, `channel_healthy`, `sidecar_restored`, `auth_recovered` — `closeEvents`, `internal/notifications/events.go`) is delivered through its alert's filter but never pinged through its alert's mention: an all-clear asks nothing of anyone, and following the alias pinged the role for "BotGuard Sidecar Restored" under the default six, which hold `sidecar_down`, and for every "Authentication Recovered", since they hold `auth` too. A target that wants a close pinged lists it in `mention_events` itself. Three states, not two: the key absent means the default six (`DefaultMentionEvents`, `internal/config/notifications.go` — `error`, `auth`, `disk_critical`, `update_failed`, `crash_recovered`, `sidecar_down`); `mention_events = []` means never; a written list means exactly those events (`ResolveMentionEvents`, `internal/config/notifications.go`). A config save that touches only `mention` or `mention_events` on a surviving target is picked up by the same reload as an event-filter change, through `setMention` (`internal/notifications/queue.go`), the mention twin of `setEvents`.
- **`mode`** — `"separate"` (default) or `"edit"`. See **Delivery Modes** below.
- **`network.public_url`** — not a per-target key, but the config field (`PublicURL`, `internal/config/types.go`) every target's deep link depends on. Notifications are not its only consumer: the web server trusts it as the dashboard's own address, its port on `localhost`/`lan` and its host on `external`/`public` ([security.md](security.md) § 3. HostGateMiddleware, § 4. CORSMiddleware), so setting it for clickable embeds widens what the dashboard admits. When set, a job embed's title links to `{public_url}/#job=<id>` (`JobDeepLink`, `internal/notifications/mentions.go`) instead of the platform page, which moves to the embed's author line instead; a send with no `Author` gets no rewrite, since there is nowhere for the platform link to move to. Three families have none now. The whole System family (a disk alert names no channel); the platform-level credential sends, which carry only their event (`cmd/moombox/monitor_callbacks.go`, three `auth` sites) — the per-job "Authentication Required" alert is not one of them and does carry both; and `connectivity_restored`, the global-outage alert, for the same reason (`cmd/moombox/monitor_callbacks.go`). Separately, a job whose channel is unknown has an `Author` of nil even though it carries a `JobID` — that is what `moombox add` produces (`cliAddedFacts`, `cmd/moombox/job_notifications.go`, sets no `Channel` for a YouTube add or a Twitch VOD add), so that embed keeps the platform link in its title. The eight mid-lifecycle sends that used to be in the same boat — `downloading`, `muxing`, `scheduled`/`rescheduled`, the `gap_split`/`quality_split` pair, and the two `connectivity_*` events — carry all three now: Arc N3 routed every one of them through the same `NotifyFacts`/`notifyAuthor` pair (`internal/worker/notify_facts.go`) the rest of the package uses, so their embeds deep-link and carry the channel author line too. The two trim sends went the same way in the same arc — `sendTrimFailed` (`internal/worker/orchestrator.go`) and the `trim_deleted` half of `DeleteTrim` (`internal/worker/trim.go`) — so no job send in the program builds its options by hand any more. `network.public_url` is validated and canonicalised by `ValidatePublicURL` (`internal/config/notifications.go`) — an absolute http(s) URL, no query, no fragment, no userinfo, trailing slash trimmed. It is read at send time (`Manager.Send`, `internal/notifications/manager.go`) from the same field `Reload` writes under the same lock, so a save from either editor applies without a restart.

### Event Types

These are the event strings used for filtering. A target with no event filter receives all events — both UIs agree on this now (unticking every event box widens a target back to everything rather than narrowing it to nothing), so a target that should receive nothing uses the `enabled` mute (see Target Options above) instead of an empty filter.

| Event | When Fired |
|-------|------------|
| `found` | Monitor detects a new stream/video. A YouTube find is sent by the worker after its first metadata fetch: the monitor's creator inserts the job with `announce_found` set (`announcesJobFound`, `cmd/moombox/monitor_callbacks.go`), the first fetch takes the flag (`TakeAnnounceFound`), and `sendStreamFound` (`internal/worker/found_announce.go`) sends the embed with what the fetch learned. The description says what was found ("Upcoming:", "Upcoming premiere:", "Live:", "Premiering:", "Ended stream:", "Video:"); a fetch YouTube refused (members-only, age-restricted) keeps the neutral "Found matching stream:", because a refusal says too little to classify the video. The fields are Starts At (absolute and countdown, for an upcoming stream with a schedule, in place of a separate `scheduled` embed), Length (where a video exists), Ended or Published (for a finished video; a publish date YouTube gave as a bare day renders as that plain date, one with a time as a timestamp), Access (members-only, age-restricted and other playability restrictions), and a Description excerpt. A failed first fetch still sends it, from what the row held — except one that never reached YouTube (a transport failure), which leaves the flag owed: `processJob` may wait the outage out and fetch again (`processStreamThroughOutages`), and the fetch that answers sends the one embed with what it learned (taken on the failure, the flag went on a feed-title embed sent into the dead link and the retry's schedule followed as a separate `scheduled`); when `processJob` records that failure instead, it sends the embed from the row itself (`announceFoundOnFailure`, `internal/worker/outage_wait.go`). A fetch whose run was cancelled (the job cancelled, or deleted — on its own, or with its channel's pending jobs when the channel is removed — or the process stopping) does not, whether it failed or succeeded, and takes no flag: one stopped by shutdown stays owed to the next run. A fetch can succeed on a run already cancelled (`GetVideoInfo` returns the watch page's parse when the clients after it fail on the cancelled context), and that run stops there (`Process`, `internal/worker/stream_processor.go`): it writes nothing to the row and sends nothing, the standalone `scheduled` included, so the next run's `found` is the discovery's one embed. Cancelled is what the row says as well as the context: an operator's Cancel writes the row (`Cancelled`, the flag cleared) and a delete removes it before either cancels the run, and a channel removal's bulk delete cancels no run at all, so a run whose fetch succeeded without taking a flag reads the row before it writes anything and stops on a `Cancelled` or missing one, whatever its context says (a read that fails stops nothing). A run whose take came before such a write took the flag and sends its `found`; the waiting loop's first write (status `Upcoming`) then applies only to a row with no outcome (`UpdateJobFieldsUnlessTerminal`), so it cannot turn that Cancel back into `Upcoming`, and the loop's first check stops the run. A target whose filter drops `found` but allows `scheduled` receives the `scheduled` embed instead whenever the find carries Starts At (`SendOptions.Fallback`, routed per target by `Manager.Send`), so a schedule known at discovery still reaches it; a target that takes `found` and pings on `scheduled` is pinged by the merged embed. An upcoming stream with no schedule has no Starts At: its upload date is not read as one, nor is a refused response's (`extractScheduledStartTime`), and once it goes live the row takes the broadcast's start (or, failing that, its upload date as before, or the current time). The flag lives in the row, so a duplicate insert or a re-detection of a known job cannot touch it, a restart before the fetch keeps it owed, and a cancel clears it. A Twitch find is sent by the monitor itself (`notifyStreamFound`) |
| `added` | Job manually added (API or CLI) |
| `scheduled` | An UPCOMING stream's scheduled start time was confirmed (`IsUpcoming && !IsLive`) after its discovery was announced: a stream found with no schedule that gains one later, or a job owed no deferred discovery (a manual add, or a row from before schema v22). A schedule known at discovery rides on `found` as "Starts At" instead. Both render Starts At the same way, absolute and countdown (`notifications.StartsAtValue`). A stream first observed already live does not fire it — "Download Starting" carries the same time in its own "Scheduled For" field |
| `rescheduled` | An UPCOMING stream's scheduled start time changed (`IsUpcoming && !IsLive`, as for `scheduled`). The actual start a stream that has begun or is already over (post_live, VOD) carries in the same field is written to the row and announced by neither event |
| `downloading` | Download begins or resumes |
| `muxing` | FFmpeg mux step begins — for every mux, including a manual one (`A M` / `POST /api/jobs/{id}/mux`), and for both finalize shapes, single-file and multi-part (quality/gap-split). The multi-part path used to return before the send and silently skip it. A manual mux of a row that was not already Muxing (an Error, Cancelled or parked job) reads "Muxing the captured media" rather than "Download complete, muxing" (`JobContext.CapturedMux`, set by `MuxJob`): what is staged may stop short of the end. A boot re-mux of an interrupted Muxing row keeps "Download complete" |
| `finished` | Job completed successfully. Warning-coloured rather than Success when the row carries `incomplete_tail` — the recording is knowingly short and Resume appends the rest; the description then reads "Archived with its end missing" instead of "Successfully archived" — and when the archive was muxed from a repaired copy of a damaged staged recording: a Gaps field gives the stretches left out and their seconds and says staging was kept, and the description reads "Archived with a gap" — only for stretches left out of the archive itself (its file, a part, a part the part merge folded into another); one left out of a file beside it, a set-aside recording's sibling or a part a complete VOD download superseded, is named in the Gaps field as that and leaves the description as it is. An archive that could not be checked against its staged fragments carries the Gaps field and the colour without a gap to name. The embed also reports an incomplete chat capture and any set-aside recordings still waiting in staging |
| `error` | Job failed. The embed names the stage (`mux` or `download`, read off the error prefixes the orchestrator writes — `mux…`, `no media files to mux`, `create output dir`) and whether staging survived, which is the Retry-versus-Resume distinction: Retry deletes staging, Resume preserves it |
| `cancelled` | Job cancelled by user |
| `auth` | Any credential problem — cookies expired, member-only content, refresh failure, Twitch chat downgraded to anonymous. See **Credential Notifications** below for the full set |
| `auth_recovered` | Credentials work again ("Authentication Recovered"), or parked jobs were re-evaluated against re-observed credentials ("Parked Jobs Re-evaluated"). The close of `auth`, aliased to it — a target filtering `auth` also receives it — but never pinged through `auth`'s mention: they used to carry `auth` itself, and `auth` is in the default mention six, so every recovery pinged the role like the failure did |
| `quality_split` | Stream quality changed mid-download; previous part closed |
| `gap_split` | Twitch live segments expired unrecoverably; part closed, new part at live edge |
| `connectivity_resume` | Connectivity restored; the same Twitch job resumed. Only after an outage the connectivity monitor reported for `connectivity.MinOutage` (60 s) — a live capture rides a shorter one out in place and sends nothing. Carries the pause instant — when the link went down, not when the minimum ran out — as a relative timestamp and the outage duration ("Paused `<t:x:R>` · resumed after 4m12s"), which is what the retired `connectivity_pause` event used to say on its own. That embed was sent WHILE the machine was offline and so mostly never arrived; a target still filtering on the old key receives this one through the manager's event alias, for one release |
| `connectivity_split` | Broadcast ended or changed during the outage; captured data finalized. Not sent for a VOD: an outage mid-VOD does not interrupt it — the download waits for connectivity and carries on where it stopped |
| `connectivity_restored` | Global connectivity restored — fires the "Outage Alert": start/end as Discord dynamic timestamps plus the duration. Deliberately the ONLY global-outage event: a lost-connectivity webhook has no connectivity to deliver over, so there is no `connectivity_lost` (removed in v2.8; stale filter entries warn at startup and strip on the next UI save). Debounced, ONE alert per outage episode (`outageAlerts`, `cmd/moombox/outage_alerts.go`): it is sent once the link has stayed up for `outageCoalesceWindow` (2 min) after a restore, and a loss inside that window continues the same episode, so a flapping link — the monitor comes back online on its first good poll, and a link dropping two polls in three used to restore, and alert, every ~15 s — sends one alert whose Duration is the time offline summed over the episode and whose Flaps field counts its outages. An episode offline less than `connectivity.MinOutage` (60 s) in all sends nothing: the engines rode it out and the monitors re-checked on every restore. A link that never settles is reported at the first restore `outageReportMaxWait` (30 min) or more after its episode's first restore, so it is heard about at most every half hour — counted from that first restore, not from the first loss, because what it bounds is flapping: a single outage of any length still waits out its window, and the bounce that so often follows a long one joins its alert rather than sending a second; a shutdown reports an episode that is over and only waiting out its window. The alert follows the notifications the outage held (see Dispatch Behavior, Outage hold) |
| `trim_created` | Trim clip created |
| `trim_deleted` | Trim clip deleted |
| `trim_error` | A trim failed: a Trim Video from either UI that broke (FFmpeg, the disk or the database failed — not a refusal, which the requester is told, nor one a stop cut short), or a post-download trim that did not produce its file for any reason — it broke, it was stopped, or it could not run at all: a start at or past the end of the recording when no end was given, a recording whose length is unknown with no end given, a range or a row the trim service refuses (the job no longer Finished, a trim of the job already running), or a job row that could not be read (`postDownloadTrim`, `internal/worker/orchestrator.go`). The one exception is a job deleted before its trim ran or while it ran: whoever deleted it wants none of it |
| `disk_warning` | Disk usage reached the warning threshold (also fired for monitoring-read failures) |
| `disk_critical` | Disk usage reached the critical threshold (targets filtering on `disk_warning` also receive it, via the manager's event alias) |
| `disk_ok` | Disk usage fell back under the warning threshold after a warning or critical alert was sent ("Disk Space Recovered"), or disk monitoring recovered after a read-failure alert ("Disk Monitoring Recovered"). Success-coloured; the close of the `disk_warning`/`disk_critical` family. Targets filtering on `disk_warning` also receive it, via the manager's event alias, so an incident that was reported always gets an end. A reading that closes both incidents at once sends both embeds — two alerts, two closes |
| `update_available` | New version detected |
| `update_applied` | Moombox restarted on a different version than the previous run (embed reports whether the web dashboard came back) — never on the launcher's rollback from a failed update, which `update_failed` announces |
| `update_failed` | A failed-update marker (`.update-broken` / `.update-failed`) was found at boot — manual attention needed |
| `crash_recovered` | The launcher respawned Moombox after an abnormal exit; names the crash's cause from its crash output, or says it left none (§Launcher/Supervisor Pattern) |
| `channel_unhealthy` | A monitored channel failed a sustained streak of checks on EVERY monitor covering it (renamed/banned/misconfigured) — its streams are being missed. Cross-monitor confirmed: a YouTube channel still reachable via DECAPI while its RSS feed 404s during peak hours does NOT fire (avoids the false positive). |
| `channel_healthy` | A channel that fired `channel_unhealthy` answered a check again. Fires only when the alert was actually SENT — a streak suppressed by the cross-monitor confirmation has no alert to close. The two YouTube monitors share one incident per channel (`channelIncidents`, `cmd/moombox/monitor_callbacks.go`): an outage both observe sends one alert, and the first monitor to reach the channel again closes it — a set per monitor sent two identical alerts and left one open when only DECAPI recovered. Aliased to `channel_unhealthy` |
| `sidecar_down` | The BotGuard sidecar has been unhealthy for a continuous 60 seconds. Error-coloured and mention-eligible. The supervisor's restart ladder handles everything shorter, so this is the failure it could not fix: no PO token is minted and signature-ciphered formats are unavailable until it returns. Carries the reason the child died and how many successful restarts this process has made |
| `sidecar_restored` | The sidecar is healthy again after a `sidecar_down` was sent. Success-coloured. Aliased to `sidecar_down`, so a target that filters the outage also receives its close |

**Open alerts survive a restart.** Each close above fires only when its alert was SENT, and which alerts are open used to live only in the memory of the process that sent them — so an alert open across a restart (an update, a settings change, a crash) never got its close. The open set is now persisted in `open-alerts.json` beside the database (same directory as `paths.database_path`; `openAlerts`, `cmd/moombox/open_alerts.go`), written atomically (`utils.WriteFileAtomic`) on every open and every close: the disk family (an open warning or critical with the time it was sent, which the 30-minute repeat cooldown runs from, and an open "Disk Monitoring Failed"), an open `sidecar_down`, each platform's channels with an open `channel_unhealthy`, and each platform whose `auth` failure was announced (with its time, the auth cooldown's stamp). The backlog scheduler's full-disk hold is kept there too (`diskGateHeld`, below), though it is not an alert. `run()` loads it before any alerter is wired or any observer starts, drops what nothing in the current configuration can close — a channel removed or disabled, and the sidecar's outage while `use_sidecar` is off, each logged — and seeds every alerter from it, so the first healthy observation after the restart sends the matching close (`disk_ok`, `sidecar_restored`, `channel_healthy`, `auth_recovered`) exactly as it would have without the restart. An observation that is still unhealthy sends nothing the restart alone would add for disk, the sidecar and channels: the disk family's 30-minute repeat runs on from the restored send time, and a restored `sidecar_down` or `channel_unhealthy` is not announced again. Auth is the exception. The cookie refresh fires its recovery on the first conclusive check of every start (`shouldFireRecovery`, `internal/cookies/refresh_pass.go`), and the failure that recovery reports reaches the auth cooldown, so a platform still dead after the restart is announced again unless the restored stamp is still inside its 30 minutes (`withPersistedAuthFailureCooldown`, `cmd/moombox/monitor_callbacks.go`) — the once-per-start announcement every start made before the stamp was persisted, now held back only within the cooldown. Two healthy paths needed help to see a restored entry at all: the monitors' health trackers fire their healthy callback only after a streak they saw cross the threshold, so every monitor covering the platform is told which channels are open (`FeedMonitor.RestoreUnhealthy`, `internal/monitor/feed.go`, and its DECAPI and Twitch twins), and the cookie refresh fires its recovered transition only on a not-authenticated → authenticated change it witnessed, so it is told which platforms are open (`SetUnrecoveredPlatforms`, `internal/cookies/refresh.go`) and closes each on its first conclusive authenticated check. A missing or corrupt file means nothing is open, with one Warn, and is replaced by an empty one.

The canonical event vocabulary is `notifications.EventGroups`
(internal/notifications/events.go). The TUI filter editor derives from it
directly; the web UI keeps a labeled mirror (`NOTIFICATION_EVENT_GROUPS` in
web/public/modules/settings.js) that MUST be updated in lockstep. Filtered
targets treat the vocabulary as an allowlist, the TUI's edit-save path
strips unknown events from hand-edited configs, and `NewManager` logs a
warning for any configured filter entry outside the vocabulary.

`connectivity_pause` is **retired**. It is absent from `EventGroups`, so
neither UI offers it and the TUI strips it from a hand-edited config on the
next save; it remains in `KnownEvents` (so no startup warning) and is the
target of `eventAliases`' `connectivity_resume` entry (so an unmigrated filter
keeps receiving the folded embed). The alias is a migration and is removed one
release after the retirement ships.

### Delivery Modes

Each target carries a `mode`: `"separate"` (the default) or `"edit"`.

**`separate`** — one message per event, never rewritten. This is what every
target did before the `mode` key existed and what an unset `mode` still means.

**`edit`** — one Discord message per job, per target. The first ALLOWED
lifecycle event creates it (`POST …?wait=true`, which per the Discord API docs
"waits for server confirmation of message send before response, and returns the
created message body" — the only way to learn the id); every later allowed
lifecycle event rewrites it in place with
`PATCH /webhooks/{id}/{token}/messages/{message_id}`. The rewritten embed
carries the new event's own title, description and fields plus two more: a
**Status** field naming the current state, and a **History** field of
`<t:unix:R> State` lines, one per state that reached this target, clamped to the
field budget by dropping the OLDEST lines. Each line — and every embed's own
timestamp, in either mode — is dated when the event happened (`Embed.At`,
stamped by `Manager.Send`), not when the target delivered it: after a Discord
backlog every line used to read the delivery time.

The lifecycle set is `found`, `added`, `scheduled`, `rescheduled`,
`downloading`, `quality_split`, `gap_split`, `connectivity_resume`,
`connectivity_split`, `muxing`, `finished` (`lifecycleEvents`,
`internal/notifications/lifecycle.go`). Everything else — `error`, `cancelled`,
`auth`, the `trim_*` family and all of System — is always its own post: those
may carry a mention, and an edit notifies nobody. Every lifecycle event carries
its job id; the eight that did not before this arc now do.

A target's mention rides an edited message the same way it rides a separate one
— it is message-level `content`, not an embed field, so a PATCH carries it too.
It rides as TEXT once the message exists, though: Discord does not notify on
an edit, so on an edit-mode target only the events that stay separate posts
(`error`, `cancelled`, `auth`, `trim_*`, System) — and whichever lifecycle
event happens to CREATE the job's message, which is a real POST — can
actually ping. An operator who adds `finished` to `mention_events` on such a
target sees the mention in the message and gets a notification only when
`finished` is the event that created it.

`error` and `cancelled` do **both**: the lifecycle message is edited to its
terminal look, and the separate embed is still posted, when a lifecycle
message is already open; a terminal event never creates one, so a target
whose first word about a job is "failed" simply posts it. Two messages on
failure, by design — the separate one is what pings. A failure whose report is
suppressed (`worker.ErrNonActionable`: age-restricted, probe budget exhausted)
still closes the open message, and only that: its `error` send is marked
`EditOnly`, so the edit carries no mention, no separate embed follows, and a
target with no open message — or in `separate` mode — gets nothing.

There are no progress edits. A cadence-driven PATCH would spend the bucket for
nothing.

**What persists.** The created message's id is stored on the job row, in
`jobs.notification_msgs` (schema 20) — a JSON object keyed by the first 16 hex
digits of SHA-256 over the RESOLVED webhook URL, the same value target dedupe
uses. It is written ONCE per (job, target), on the first successful POST,
through `UpdateNotificationMsgs`: a silent single-column write that bumps no
`updated_at` and wakes no subscriber. A restart therefore keeps editing the same
message. The History does not persist — the message keeps being edited, but its
History begins again at the first state after the restart.

Ids are keyed on the resolved URL, so every spelling of one webhook shares one
message — `discord://ID/TOKEN`, the legacy `discordapp.com`, a `ptb.`/`canary.`
host, a trailing slash and an empty query (a bare `?`) all resolve to the one
`https://discord.com/…` form
(`canonicalDiscordURL`), which is also what target dedupe keys on (a slash or a
`ptb.` host used to build a second target that posted every embed again, and in
edit mode opened new messages for every job in progress). The query is folded
too (`canonicalWebhookQuery`, `internal/notifications/discord_edit.go`): its
parameters sorted by name, as the sender already encodes them on the wire,
with every nameless pair (a stray `&`) and every `wait` dropped — the sender
adds `wait=true` itself to the one request that reads the created message and
strips it from the edit route, so a configured one changed only the key.
`thread_id` and every other named parameter stay, even with an empty value,
and a query `net/url` refuses to parse (a `;` separator, a bad escape) is kept
as typed. `…?thread_id=9`, `…?thread_id=9&` and `…?wait=true&thread_id=9`
are one target; `…?thread_id=9` and the bare URL are two. The shorthand keeps
its query too, with or without a slash before it: `discord://ID/TOKEN/?thread_id=9`
used to lose it with the path past the token, and posted to the channel —
beside its https spelling, as a second target. The releases that
shipped edit mode, 2.8.9 and 2.8.10, rewrote only `discordapp.com` and kept the
rest as typed, so a row they wrote under one of the other spellings holds the
id under that spelling's own key. A target remembers the old key of every
spelling folded into it (`legacyResolvedURL`, `internal/notifications/manager.go`)
whose old resolution named the same webhook — not the shorthand's with a slash
before its query, which resolved to the bare channel webhook: the ids under
that key are channel messages, the thread's edit route cannot reach them, and
when the channel is configured too they are the channel target's own — and
reads it when the current key misses: the id is adopted under the current
key in memory and the old key leaves the map (`messageID`,
`internal/notifications/lifecycle.go`), so a job open across the upgrade keeps
editing its message — and its error or cancel still closes it — and the old key
leaves the row at the job's next write, with no write of its own. Those releases
built one target per spelling, so a webhook configured in two spellings (say
with and without the trailing slash) posted a message per spelling for every
job, and a job open across the upgrade holds a different id under each key.
The second is not dropped: it stays under its old key and every edit rewrites
it with the same body, the terminal edit included, which closes it beside the
first (`extraMsg`, `patchExtra`, `internal/notifications/lifecycle.go`); one
Discord no longer has is forgotten, with nothing posted in its place. A target the operator removed leaves an orphaned entry that nothing
reads; a target the operator adds starts a new message at its next allowed
event; and deleting the job drops the row and the ids with it (no DELETE is ever
sent to Discord) — and the running process's copy too: `onJobDeleted` calls
`ForgetJob`, and the jobs-list subscriber (`onJobsChange`) calls `RetainJobs` for
the bulk delete of a removed channel's pending jobs, which fires no per-job event
(`cmd/moombox/monitor_callbacks.go`).
A YouTube job's id is its video id, so the same id comes back on a re-add or a
re-detection, and a process still holding the deleted job's id PATCHed its old
message — far up the channel, where an edit notifies nobody — instead of opening
a new one. Both drop in each target's delivery order, not at once
(`forgetInOrder`, `internal/notifications/lifecycle.go`): a step goes onto the
FIFO of every target that can hold edit-mode state, behind what it already
holds, and drops only that target's id and History — under its current key and
under every old-spelling key it carries, since a job open across the upgrade can
still hold the id under one, loaded by another target's send and not yet
adopted, and that id is what the target's queued cancel closes. A separate-mode target gets
none — it never records an id — unless it was flipped out of edit mode while an
edit was still in flight, which keeps it getting steps until a delivery starts
under the new mode (`editKeys`, `internal/notifications/queue.go`). A step is
never shed and never counts toward the queue cap, so a batch delete behind a
slow delivery cannot make the queue shed an alert. Deleting an active job cancels it first, so its `cancelled` is already
queued; dropped at once, a busy target dispatched that cancel with no id left
and posted it plain, and the message read "Downloading" for good — and a POST
in flight when the delete landed came back and remembered its id afresh, so a
re-add PATCHed the deleted job's message after all. Until a target's step runs,
any id it holds for the job under that key is the deleted job's, and it is kept
out of every row write (`markDropping`): a quicker target may already have
posted for a job re-added under the same id, and that POST writes the new row —
with the slow target's old id in it, the re-added job would edit the deleted
job's message on that target. The mark is set for every key a step is queued
for, before the step, whether or not the target holds anything yet — its
deleted-job send can still be queued or in flight, and the id that POST brings
back is the deleted job's too — and it is counted per step, so a job deleted
twice before a slow target drains stays marked until both drops have run. It
lives beside the per-job entry, not in it, so a release or an eviction of the
entry cannot take it.
`RetainJobs` marks nothing, because its list is a snapshot taken at the bulk
write and a job added since is missing from it without being deleted. Such a
job loses only its in-process History: a tracker entry that loses a target's
key is re-read from the row on its next touch (`dropKeysLocked`), so the job's
next event on that target still edits the message the row names rather than
opening a second one. A
removed target's queue
still runs its steps after its in-flight delivery (the rest of its queue is
discarded); a target whose queue has already exited — after shutdown — drops
at once.

The in-memory half — the message ids a running process is holding, and the
History lines — is released when a job reaches its terminal edit, and capped at
`maxTrackedJobs` for jobs that never do. That is a cache eviction, not a close:
the persisted id is what survives, so a Retry after a release re-reads the row
once and keeps editing the same message. The row can be gone by the time a
queued send is dispatched, though — deleting an active job queues its cancel
first — so every send an edit-mode target will create, edit or close a message
with pins its job's entry from the moment it is queued, loading it while the
row is still there, until it leaves the queue by any route: delivered, shed,
refused or discarded (`hold`, `internal/notifications/lifecycle.go`). The cap
passes a pinned entry over, and may sit past `maxTrackedJobs` by as many jobs
as the queues hold sends for. Without the pin an entry evicted — by a
backfill's `found`s on any edit-mode target — before the cancel was queued, or
while it waited behind a busy target, left the cancel with neither the id nor
the row, and it posted plain beside a message that read "Downloading" for good.
A release waits for the pin too: a terminal edit delivered while later sends
of the job are queued behind it closes its target's story but keeps the entry,
and the last pin to let go drops it if the story is still closed
(`dropIfClosedLocked`). Released at once, an `error` delivered ahead of a
Retry's `downloading` and the delete's `cancelled` left them the row alone,
which the delete had taken: the `downloading` opened a second message, or the
cancel posted plain, and the job's message read "Failed" for good. A managed
send's lookup opens the story it writes again (`messageID`), so a Retry that
edits the closed message is mid-story to every later release, not closed.

**During shutdown** every request on this path is single-attempt, like every
other send: the owner's ruling caps a graceful shutdown at 15 s, and one
lifecycle edit's retry ladder could spend all of it.

**When the message is gone.** A PATCH answered `404` with `Unknown Message`
(code 10008) makes the notifier post a new message and overwrite the stored id
— except for a terminal event (`error`, `cancelled`), which never creates a
lifecycle message: the stored id is forgotten and the event's separate embed,
posted next as always, is the whole report.
A `404` naming `Unknown Webhook` (10015) is not that — the webhook itself was
revoked, and it stays a permanent failure.

**Budget.** Edits buy channel quiet, not request budget: per the Discord API
docs the rate-limit bucket's top-level resource is `webhook_id + webhook_token`,
so a PATCH spends the same bucket as a POST and goes through the same
three-attempt loop, the same `Retry-After` handling and the same per-target
FIFO. That FIFO is what makes edit mode safe — a job's states can never
reorder, and a retried edit can never land after a later one.

**Batching does not apply.** The 5 s `found`/`added`/`auth` window is
separate-mode only: for an edit-mode target the `found` embed IS the job's
lifecycle message, and coalescing it would defeat one-message-per-job.

**A mode flip flushes the open window.** `applyTargets`
(`internal/notifications/manager.go`) rebinds a surviving queue's dispatch
decision through `setDispatch` (`internal/notifications/queue.go`) BEFORE it
calls `setMode` (`internal/notifications/batch.go`) to flush that target's
batcher. A flip INTO edit mode therefore flushes through the already-rebound
edit path: a window holding exactly one pending **lifecycle** embed (a `found`
or an `added`) is delivered by the edit branch of `dispatchOne`
(`internal/notifications/lifecycle.go`) and becomes
that job's newly created lifecycle message; a window holding more than one is
still several jobs sharing a POST, so the same function's single-embed guard
sends it as an ordinary multi-embed post instead, and each job's next event
opens its own lifecycle message under the new mode. A flip back to separate
mode flushes an always-empty window, because edit mode never opens one. An
`Add` racing the flip re-checks the mode under the same hold that appends, so a
flip landing between its first check and the append emits that embed at once
instead of arming a window on an edit-mode target.

### Credential Notifications

Every notification below carries `Event: "auth"` — or `"auth_recovered"`, the family's close, which the `auth` filter entry delivers through its alias — so one filter entry covers the family. An empty `Event` would bypass every target's allowlist — the filter applies only when `Event != ""` — which is why none of them omits it.

| Title | Type | Fires from | What it asserts |
|-------|------|-----------|-----------------|
| Cookie Re-Authentication Required | Error | `handleRecoveryNeeded` (`cmd/moombox/monitor_callbacks.go`), `auto_enabled` off | A platform answered a conclusive not-authenticated and nothing automatic will attempt to restore it. Claims nothing about a refresh, because none ran, and does NOT claim cookies are present — the file may have been deleted outright |
| Cookie Auto-Refresh Failed | Error | `runCookieRecovery`, the pass returned an error other than the two below **or** came back with a conclusive `RefreshFailed` verdict and no error | The automatic refresh ran and failed |
| Cookie Auto-Refresh Skipped | Error | `runCookieRecovery`, `ErrProfileInUse` | The platform is not authenticated and the refresh that would restore it was SKIPPED: a browser the profile's `SingletonLock` names may still be running on it, so nothing was launched. Carries that sentence — the host to close the browser on, and the lock to delete if no browser there is using the profile — and names cookie replacement as the other way out. It went out as "Cookie Auto-Refresh Failed", which named replacement as the only one. Error, as Cookie Re-Authentication Required is, because nothing will restore the session on its own; the log line is a Warn skip, and the re-login flag is still raised |
| Cookie File Unreadable | Error | `runCookieRecovery`, `ErrCookieFileUnreadable` | The existing `cookies.txt` could not be READ, so nothing was written to it. Its own copy rather than the generic one, because the generic text would tell the operator to overwrite the one file Moombox deliberately did not touch — it may hold a working credential for another platform. The remedy named is the filesystem or mount, and Moombox retries on its own |
| Cookie Auto-Refresh Ineffective | Warning | `runCookieRecovery`, verdict neither OK nor a conclusive failure | The refresh ran, did not restore auth, and could not establish why. Asserts no cause |
| Authentication Required | Warning | `internal/worker/worker.go`, a job parked in `COOKIES?` | This job needs credentials it does not have |
| Authentication Recovered | Info | `OnAuthRecovered` | The platform's credentials work again. It is the CLOSE for a failure that was announced, so it fires whenever one was — reporting "N jobs resumed" or "no jobs were parked" — and exactly ONCE per failure episode: the close consumes the announcement stamp, so the many later `OnAuthRecovered` edges a healthy process crosses stay silent, and the next failure is a new episode that announces immediately rather than inside the old one's 30-minute cooldown. A recovery for a platform never reported broken sends nothing. This edge also does one more thing on Twitch and says nothing about it: `OnAuthRecovered` also tells every live Twitch chat session to reconnect (`ReauthenticateTwitchChats` counts downloaders told, never sessions authenticated), because a transient refusal heals with the credential fingerprint UNCHANGED and so fires no `OnCredentialsChanged`. The notification still reports only the jobs — the chat reconnect is a log line (`twitch credentials usable again`), not an operator decision |
| Parked Jobs Re-evaluated | Info | `OnCredentialsChanged` | N parked jobs were resumed after the platform's saved credentials were re-observed. States no cause on purpose: this fires on the first authenticated observation of EVERY process, not only on a real change, so "a different account was supplied" would often be false. It also fires for Twitch, whose credential is a bearer token and a login name rather than an account — which is why the wording says "saved credentials" |
| Twitch chat is anonymous for {channel} | Warning | `sendTwitchChatDowngrade` (`internal/worker/stream_processor_twitch.go`) | A job that HAD Twitch credentials is capturing chat anonymously. Warning rather than Error because nothing failed — this capture is fine and the NEXT one starts anonymous. Fields: Channel, Job, Reason (one of the four chat-handshake `AuthDowngrade*` tokens, never a credential — the fifth, playback-token route, never reaches this notice). The SAME report also marks the platform (`NoteTwitchAuthLoss`), which is what fires "Cookie Re-Authentication Required" or the one automatic recovery attempt above — so an operator with `auto_enabled` off can receive both, one naming the job and one naming the platform. See [platform-services.md](platform-services.md) § IRC Chat (Live) |

`withAuthFailureCooldown` (`cmd/moombox/monitor_callbacks.go`) bounds the first five to one per platform per 30 minutes; it does not withhold the first. The two Info recoveries send inline with no cooldown. The Twitch chat notice is latched once per downloader instead, and is deliberately NOT deduped across jobs — a later job with the same dead cookies must notify again. That latch is reset by `Reauthenticate()`, so a repaired credential that fails AGAIN on the same job notifies again too; the platform mark it fires beside is deduped separately, by `shouldFireRecovery`, and so raises one alarm per loss however many jobs report it.

**The two-tier cookie liveness pilot is ARMED, and its notifications fire.** `const livenessRecoveryArmed = true` (`internal/cookies/refresh_liveness.go`) gates tier 2 and has been true since 2026-09-03 (owner ruling; the five-day pre-arming soak was skipped by that ruling). There are therefore TWO producers of everything in the table above. The first is `shouldFireRecovery`'s conclusive not-authenticated — reached from `refresh`'s own validate pass and from `RefreshService.NoteTwitchAuthLoss`, which passes a chat downgrade or an anonymous playback token as `(nowAuth=false, checkErr=nil)`, so the check reads conclusive for both and both share the same once-per-loss dedupe (`noteRecoveryDecided`). The second is a tier-2 liveness verdict: signed out and past the per-platform back-off, it logs `a liveness observation reports this platform is signed out, triggering recovery` at Warn and calls `OnRecoveryNeeded`. The back-off (`recordLiveness`) re-alarms 30 minutes after the first alarm, doubles per alarm to a 24-hour cap, and resets to the base only on a conclusive signed-in verdict; a tier-1 fire's `noteRecoveryDecided` stamp keeps tier 2 from raising a second alarm for a loss tier 1 already raised. The constant is source, not config — a wrong verdict in the field is reversed by setting it back to `false` and rebuilding.

### Batching

`found`, `added`, and per-job `auth` sends (the "Authentication Required" parked-job alert, which — unlike the platform-level cookie family — carries a `JobID`) coalesce; everything else — `error`, `cancelled`, the whole `trim_*` family, every System event, and an unfiltered send with no event at all — is delivered immediately (`isBatchable`, `internal/notifications/batch.go`). A batchable send opens a per-target coalescing window measured from its FIRST member and never re-armed by a later one (`batchWindow`, `internal/notifications/batch.go`, 5 seconds) — a steady trickle of `found`s cannot hold a message open indefinitely, and conversely a single batchable send on an otherwise idle target still waits out the full 5 seconds before it is delivered; coalescing does not special-case a window of one.

At the window's close — or at shutdown, or when the target is retired — its embeds are chopped into one or more `Message`s (`internal/notifications/message.go`) that each satisfy BOTH of Discord's per-message caps: at most ten embeds (`maxEmbedsPerMessage`, `internal/notifications/batch.go`) and at most `limitTotal` characters summed across them (`internal/notifications/limits.go` — Discord's 6000, measured over the CLAMPED embeds, so what the splitter counts is what the payload carries). `splitMessages` (`internal/notifications/batch.go`) enforces both, preserving arrival order and rolling overflow forward into the next message — nothing is dropped by the split itself.

A `Message` is one queue item, so the per-target FIFO's drop policy and the ordering between messages (see Dispatch Behavior below) are unchanged by batching — with one exception: a non-batchable send (an `error`, a `cancelled`) arriving while a window is open is delivered immediately, ahead of the embeds still coalescing, because a five-second wait is exactly wrong for an alert. Ahead of OTHER jobs' embeds only: when the window holds one this send follows — the same job's `found`, or a per-job "Authentication Required" that an `auth_recovered` closes — the window is flushed first (`holdsPredecessor`), so a job's "Download Starting" no longer lands above its own "Stream Found", nor a quick recovery above the alarm it ends. A batch is low-tier, for the drop policy's purposes, only when every embed in it is (`batchIsLowTier`, `internal/notifications/batch.go`) — one normal-tier embed riding in an otherwise-`found` sweep protects the whole message from the drop policy the same way a lone alert always has. Discord applies `content` and `allowed_mentions` per message, never per embed, so a batch pings once per message, not once per window: the first non-empty mention handed into the window rides every message that window splits into, never withheld from the second or third.

A retired target's open window is flushed into its queue before that queue starts discarding: `applyTargets` (`internal/notifications/manager.go`) stops the batcher before it retires the queue, so a webhook removed mid-window does not take its coalesced embeds with it. The flush is about ACCOUNTING, not about delivery — once in the queue the flushed message is treated exactly like anything else already queued for a removed target, which means it either wins the race with `stopDiscard` and goes out, or meets the same "target removed — discarding its queued notifications" Warn an ordinary queued item would. Without the flush it would simply vanish with the batcher, counted nowhere. `BeginShutdown` and `Wait` (`internal/notifications/manager.go`) flush every open window immediately, AFTER switching targets to single-attempt delivery — so the rescued batch is itself single-attempt rather than spending the shutdown budget on a 2s/5s ladder — and before closing the drain, since `enqueue` drops with a Warn once the queue is closing. A window open at shutdown is delivered, never held open to wait out its own 5 seconds.

The window is separate-mode only: an edit-mode target's `found` embed IS the job's lifecycle message, so it is never coalesced. See **Delivery Modes**.

### Dispatch Behavior

- **Per-target FIFO queues.** One bounded queue (256 entries, `notificationQueueCap`) and one draining goroutine per target, created by `applyTargets` (`internal/notifications/manager.go`). `Send` snapshots the target list under an RWMutex, applies each filter, and appends — it never blocks the caller and never spawns. Because one goroutine drains a target, a job's embeds can never reorder, and a burst can never put several concurrent POSTs into one webhook's rate bucket.
- **One queue item is one message.** Since batching landed, a target's FIFO holds a `Message` (`internal/notifications/message.go`) — one to ten embeds delivered as a single Discord POST — never a single embed; a producer that still sends one item at a time gets a one-embed `Message` (`One`, `internal/notifications/message.go`), so the SHAPE of an ordinary send is unchanged, byte for byte. What batching changes is WHEN a batchable send reaches this queue: not at `Send`, but when its window closes. The 6000-character total under Embed limits below is a per-MESSAGE budget, not a per-embed allowance repeated for each — see Batching above. The queue cap above (256, `notificationQueueCap`), the drop-oldest-low-tier counters below, and the "queue drained — totals" Warn all count messages this way too: each one is worth up to ten notifications, not one. The steps a job delete queues on an edit-mode target (see Delivery Modes) are not messages and are not counted.
- **Batching window.** `found`, `added`, and per-job `auth` sends are coalesced by a per-target, non-sliding 5-second window (`batchWindow`, `internal/notifications/batch.go`) before they ever reach the FIFO above — see Batching above for the split, the single per-message mention, and the ordering exception for a non-batchable send that arrives mid-window.
- **Drop policy.** On a full queue the OLDEST low-tier entry goes — a message whose embeds are ALL `found`, `added`, `scheduled`, or `rescheduled` (`Tier`, `internal/notifications/manager.go`; `batchIsLowTier`, `internal/notifications/batch.go`, which runs for every message, one embed or ten) — with a Warn naming the event and title of its first embed. With nothing low-tier queued, the ARRIVAL is dropped instead, so an older alert is never displaced by a newer one. Alerts (`error`, `auth`, everything in System) are never the victim — one normal-tier embed anywhere in a batch protects the whole message. The Warn is **coalesced**: a queue may speak at most once every 5 seconds (`dropWarnInterval`) and each line carries the count of messages shed since the last, with a final total when the queue empties — a backfill re-scan against a dead Discord sheds on nearly every send, and a line per victim buries the incident in its own symptom.
- **Panic recovery:** each queue's goroutine carries a top-level `recover`, and each delivery carries its own — a panic in one send cannot strand every later notification for that target.
- **Event filtering:** if a target has an event filter list, only matching events are sent. Targets with no filter receive everything. An event that split from a broader legacy name also matches targets allowlisting the old name (`eventAliases`, `internal/notifications/events.go`).
- **Timeout:** Discord webhook HTTP requests have a 15-second timeout per attempt.
- **Retry:** bounded delivery loop, max 3 attempts total — transport errors and Discord 5xx back off 2s/5s; 429 honors a validated `Retry-After` (≤30s); other 4xx are permanent. Cumulative *inter-attempt* sleep is capped at 30s, bounding one notification's hold on its target's queue at ~75s — plus any pre-emptive rate-bucket waits, which are deliberately outside that budget (charging them to it would let one legitimate window wait forfeit the retries a following 5xx needs), plus, when the last attempt failed at the transport level, the outage verdict wait below. An outage holds outside all of it.
- **Outage hold:** the manager is given the connectivity monitor at boot (`Manager.SetConnectivity`, wired in `cmd/moombox/services.go` beside `SetMessageStore`; `internal/notifications/outage.go`), and nothing is spent on a link it calls down. While it reports offline a target HOLDS: its goroutine delivers no message (a job delete's queued step still runs — it needs no network), and every transition wakes every target, so the online edge itself resumes the queue. A link that fails under a delivery the monitor still called online holds that delivery in place the moment the monitor reports offline — before each attempt the ladder asks, so the outage costs the one attempt that was in flight — and when the LAST attempt failed at the transport level with the monitor still saying online, the ladder waits up to `outageVerdictWait` (15 s, three of the monitor's polls — the window the engine's chunk ladder gives it, `awaitOutageVerdict`) for a verdict before dropping the notification: three refused dials are over in ~7 s, and the monitor needs two failed polls. A hold ends in a fresh ladder once the link is back. On the online edge the backlog goes out in the order it was raised, each message marked: a line of `content`, after any mention, reading "Delayed by a connectivity outage — raised `<t:…:f>`" (the earliest embed's time for a batch; each embed's own timestamp still dates it) — content rather than a field because it is outside the embeds' 6000-character budget, so a batch the splitter packed to the limit still fits. Only what was raised while the monitor reported offline, was queued when it went offline, or was being delivered when the outage held it, is marked: the queue goes on holding from the online edge until its goroutine next looks, and a notification queued in between was raised after the outage and goes out unmarked — what the restore itself sets off, such as a Twitch capture's `connectivity_resume` or a stream a monitor's re-check on the restore finds. The Outage Alert is usually not among them: it is queued only once the link has stayed up for its coalescing window — at the restore itself only for an episode at its never-settles bound (see Event Types) — and goes out unmarked either way. The marking is done at the offline transition itself, on whatever each target has queued (`targetQueue.noteOffline`), as well as by the goroutine's own looks, and on every one of them, not only the first of a hold: a message waiting behind a request still in flight is marked though the goroutine never looked while the monitor said offline, and a second outage marks what queued in the online gap behind a delivery the first one held in place and is still retrying. An edit-mode target keeps its one-message-per-job rule through a hold: its held lifecycle events still create and then rewrite the job's one message in order, and the rewrite says it was delayed in a `Delayed` field ("By a connectivity outage — raised `<t:…:f>`", placed after Status and History) rather than in the content note — Discord keeps the content of a message an edit sends none for, so the note would outlive the state it described, where the next rewrite replaces the embed and the field with it; the terminal event's own separate post carries the note. One edit-mode event can make several requests — the create or rewrite of the job's message, the rewrite of a message an older spelling of the webhook opened, the terminal event's separate post — and an outage that holds one of them part-way through its ladder holds every one after it: the held request is resent with the `Delayed` field, and each request after it carries its mark too, the separate post its note (`managedBody`, `internal/notifications/lifecycle.go`), as a separate-mode target's held post is resent with the note. A request made before the hold goes out unmarked: it went before the outage. The queue cap (256 messages) still applies while held, but the policy flips: the OLDEST goes — the oldest low-tier message first, then the oldest of all — never the arrival, because what an operator reads on the online edge should be the latest of what happened; the count is reported once, on the online edge, as a Warn and a "Notifications Dropped During Outage" embed to that target ahead of the backlog (no event, so no filter or mention applies). The Outage Alert (`connectivity_restored`) comes AFTER the held notifications: they go out at the restore, and the alert only once the link has stayed up for its coalescing window (see Event Types) — and behind them in each FIFO even if they have not all gone yet — closing the story they tell rather than announcing one whose parts have not arrived. A target still held at shutdown discards what it holds with one Warn naming the count (see Graceful shutdown). A Manager with no monitor (`moombox add` builds one) never holds.
- **Rate bucket:** `X-RateLimit-Remaining` and `X-RateLimit-Reset-After` are read from every non-429 response. A remaining count of 0 arms a pre-emptive sleep (capped at 30s, plus a 50ms `bucketSkewPad` covering the header's millisecond rounding) on that webhook's sender, so the next embed waits out the window instead of spending one of its three attempts on a 429 Discord has already promised. Per Discord's rate-limit docs the bucket is discoverable only from these headers — there is no published numeric cap. A 429 the delivery ENDS on — its `Retry-After` past the 30s cap or missing, or the ladder's last attempt — arms the same sleep from its `Retry-After`, else `X-RateLimit-Reset-After`, else the cap (`noteRateLimitGiveUp`): with nothing recorded, the queue's next embed POSTed straight into the same limit, and the one after it, so a 45s `Retry-After` (Discord's per-channel webhook limit commonly asks 30-60s) dropped every queued alert within milliseconds.
- **Embed limits:** every embed is clamped on rune boundaries inside `buildPayload` before it is sent — title 256, description 4096, field name 256, field value 1024, footer 2048, author name 256, at most 25 fields and 6000 characters in total, with a `…` marker. Over any one of them is a permanent 400, so an unclamped embed was a silently dropped alert. A field whose name or value is empty is a permanent 400 too, and is dropped at the same clamp rather than losing the message. Producers use `ClampRunes` (`internal/notifications/limits.go`) for their own excerpts and `EscapeMarkdown` (same file) for job-supplied text — titles, channel names, categories, error text — which neutralises Discord's markdown, `<…>` forms and `[text](url)` masked links.
- **Hot-reload:** notification config edits apply immediately — the web config route fires `OnNotificationsChange` → `Manager.Reload`, and the TUI save path calls `Reload` directly. The diff is on the resolved webhook URL: a target that is still configured keeps its goroutine, its queued backlog and its learned rate bucket; a removed one finishes its in-flight delivery and exits, discarding the rest with one Warn naming the count. A webhook re-added under the same URL while that delivery is still running takes over the retired queue's sender — its lifecycle message ids and rate bucket — and starts draining only once the retired goroutine exits, so a mute-then-unmute mid-POST cannot open a second lifecycle message for the same job. No restart required.
- **Delivery mode:** per target, `separate` (default) or `edit` — see **Delivery Modes** above. Hot-reloads with the rest of the notifications array; no restart.
- **Save-time validation:** webhook URLs are validated at save (web `validateConfigUpdates` + TUI editor) via `notifications.ValidateURL`; `POST /api/notifications/test {url}` sends a single-attempt test embed (used by the web Test buttons and the TUI `T` action, including for unsaved URLs). Both `discord.com` and the legacy `discordapp.com` host are accepted; the latter is canonicalised, so the two spellings of one webhook collapse to one target.
- **Graceful shutdown:** `BeginShutdown` switches every target to single-attempt delivery, then `Wait` drains the queues — see the Shutdown Sequence above for the force-exit cap that actually bounds it. That single attempt's own rate-bucket wait is capped at 2s (`shutdownBucketWaitCap`), not the Rate bucket bullet's normal 30s: a wait that long could outrun the force-exit on its own, or starve every item still behind it in that target's queue. A target the connectivity monitor still holds offline when `Wait` closes it makes no attempt at all: it discards what it holds with one Warn naming the count (`closeOfflineLocked`, `internal/notifications/queue.go`), runs its queued steps and exits, and a delivery held mid-ladder gives up — single attempts on a link that is down would only spend the force-exit's seconds.
- **Embed format:** Discord rich embeds with title, description, color (by notification type), optional fields, an author line (channel name, avatar, channel page), thumbnail, image, footer (`Moombox · {platform} · {job id}`, or just `Moombox`), and an ISO 8601 timestamp. A mention, when a target is configured for one, rides the message `content` with a matching `allowed_mentions` — embeds never mention on their own.

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

**Files:** `internal/disk/disk_windows.go`, `internal/disk/disk_unix.go`

On Windows, kernel32 `GetDiskFreeSpaceExW` via `syscall` FFI (no CGo); on Linux, `statfs(2)`, with block counts multiplied by `f_frsize` (`blockUnit`, `internal/disk/blockunit_linux.go`) — the unit they are counted in, which `f_bsize` (the preferred I/O size, 1 MiB on a CIFS mount) is not. Both query the volume containing a given path (a path that does not exist yet answers for the nearest existing ancestor) and return:

```go
type DiskSpace struct {
    Free    uint64  // bytes free for caller
    Total   uint64  // total bytes on volume
    UsedPct float64 // percentage used (0-100)
}
```

On Windows the path is resolved to an absolute path and the deepest existing directory on it is queried (`queryNearestDirectory`, walking up to the drive root only while a level fails): `GetDiskFreeSpaceExW` answers for the volume a directory is on, so a recordings disk mounted into a folder such as `C:\Recordings` reports its own space. Querying the drive root, as it used to, reported `C:`'s, and the low-space alert never fired for the disk actually filling up.

### Thresholds

Configured in the `[disk]` section of the TOML config:

| Setting | Default | Range | Purpose |
|---------|---------|-------|---------|
| `disk_warn_percent` | 90 | 1-99 | Warning level — surfaces in status bar and notifications |
| `disk_critical_percent` | 95 | 1-99 | Critical level — more urgent warnings, and backlog admission holds from it until usage is 2 points below it (see below) |

Validation rules:
- Both values must be between 1 and 99 (invalid values reset to defaults)
- `critical_percent` must be greater than `warn_percent` (if not, it is set to `warn_percent + 5`, capped at 99)

### Status Reporting

Disk space information is included in the `GET /api/status` response and displayed in both the Web UI status bar and TUI status bar. It is read at boot, then every third stats tick (~6 minutes), and at once after a save from either UI that changes `disk_warn_percent`, `disk_critical_percent` or `output_directory` (`OnDiskSettingsChange` / the TUI save, both through `requestDiskRecheck`, `cmd/moombox`), and each reading goes through `diskAlerts` (`cmd/moombox/disk_alerts.go`), the boot reading included — the ticker's first check is six minutes in, and a volume already full at boot went unannounced for that long. A level is reached when usage is AT OR ABOVE its threshold (`ComputeWarnLevel`). Reaching warn sends `disk_warning`, reaching critical `disk_critical`; the same level repeats at most every 30 minutes, and a change of level is sent at once. An open alert holds until usage falls 2 points below its threshold (`DiskRecoveryMargin`, `internal/config/types.go`; `DiskConfig.ClearOfCritical` and `DiskConfig.ClearOfWarn`): only then does a critical step down to a warning or a warning close with `disk_ok`, so a volume sitting on a line no longer alerts and recovers on every check. Disk reads that fail twice in a row send `disk_warning` ("Disk Monitoring Failed") and the next good reading `disk_ok`.

### Backlog Admission on a Full Disk

The critical level is also the one thing that holds on its own. From the moment the output directory's volume reaches `disk_critical_percent`, the backlog scheduler admits no backlog VOD — they wait in `Queued` (`Scheduler.diskGateClosed`, `internal/worker/scheduler.go`) — until usage is 2 points below it. It reads the same volume against the same rules as the alerts (`DownloadWorker.readOutputDisk`, `internal/worker/disk_gate.go`; `DiskConfig.AtCritical` and `DiskConfig.ClearOfCritical`, `internal/config/types.go`), so admission stops on the reading that sends `disk_critical` and resumes on the one that steps it down, but fresh on every sweep that has a backlog to admit — and on every sweep while admission is held, backlog or none, so a hold does not outlive the incident that set it — rather than on the six-minute cadence above: the first sweep that reads usage 2 points below the threshold — at most one 60 s heartbeat after that much space is freed — admits again. A reading in between leaves admission as it was, so a volume sitting on the line does not admit a backlog VOD every time it dips under it. The log says once when admission stops and once when it resumes. A restart does not reopen it: the hold is recorded in `open-alerts.json` (above) on every stop and resume (`diskGateHeld`; `Scheduler.RecordDiskHold`), and that hold, or a `disk_critical` alert still open when the process stopped, starts the next process's admission held (`restoreDiskGate`, `cmd/moombox/disk_alerts.go`; `Scheduler.RestoreDiskHold`), so the restored alert and the hold end on the same reading — a new process used to admit at 94% against 95 while the alert it restored stayed critical. The alert alone did not cover a hold that began between two of its six-minute readings, which the next process started without. Live, upcoming and manually added jobs are never held, nor is a backlog VOD already admitted. A reading that fails leaves admission as the last good reading left it.

A backlog VOD admitted below the threshold can still fill what is left. One whose download or mux fails for want of space (`isDiskFull`, `internal/worker/disk_full.go` — the mux's refusal to write the repaired copy of a damaged staged recording the staging volume has no room for among them) goes back to `Queued` rather than to Error — held for 5, then 10, then 20 minutes, and after that for as long as the gate above stays closed — and ends in Error, saying it gave up, only on its fourth run in a row to end that way (`requeueBacklogAfterDiskFull`, `internal/worker/backlog_retry.go`). Its staging is kept. A broadcast or a manually added video that runs out of space still ends in Error.

---

## Status and Diagnostics Endpoints

| Endpoint | Method | Purpose |
|----------|--------|---------|
| `/api/status` | GET | Server status: version, disk space, cookie state, monitor state, and the host zone's UTC offset (`utcOffsetMinutes`, for the template example) |
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
| `internal/updater/manifest.go` | Signed release manifest: format, builder, parser, the checks `ApplyUpdate` makes |
| `internal/notifications/manager.go` | Notification dispatch, event filtering, target management |
| `internal/notifications/discord.go` | Discord webhook sender |
| `internal/notifications/lifecycle.go` | Edit-in-place lifecycle messages: the event set, the per-(job, target) message-id store, the POST-or-PATCH decision, the Status/History rewrite |
| `internal/notifications/discord_edit.go` | `?wait=true` create + `PATCH …/messages/{id}` edit, and the Unknown-Message refusal |
| `internal/disk/disk_windows.go` | Disk space queries via kernel32 (Windows) |
| `internal/disk/disk_unix.go` | Disk space queries via statfs (Linux) |
| `internal/config/config.go` | Default values, validation (including disk thresholds) |
| `.github/workflows/release.yml` | Release pipeline definition |
| `.github/workflows/ci.yml` | Test workflow: the `lint`, `js` and sharded `test` jobs |
| `.github/scripts/test-shard.sh` | Which packages each test shard runs, and the check that the shards are `go list ./...` exactly once |
| `.github/scripts/retry.sh` | Time limit and retries for CI's network steps |
| `.github/scripts/install-ffmpeg.sh` | FFmpeg for the test jobs, installed in the background |
| `.github/scripts/go-cache-trim.sh` | When a restored Go (or staticcheck) cache trims itself in a CI job |
| `cmd/moombox/winres/winres.json` | Windows resource metadata template |
