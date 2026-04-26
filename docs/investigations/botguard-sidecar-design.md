# BotGuard Node Sidecar — Design Doc

**Status**: design approved, implementation pending.
**Owner decision date**: 2026-04-26.
**Replaces**: BotGuard goja-only path (test.50–test.55 Option 2 work, which
shipped a real-class DOM shim but couldn't pass BotGuard's timing check).
**Result of**: docs/investigations/botguard-option-a.md (Day 1 happy-dom
investigation), docs/investigations/botguard-option-2-plan.md (Option 2
hand-rolled DOM plan), docs/investigations/botguard-option-2-results.md
(why Option 2 failed: goja interpreter is ~100× faster than V8 JIT, so
BotGuard's timing fingerprint always rejects).

---

## 1. Goal

PO tokens generate cleanly inside Moombox.exe, end-to-end, with no user-
installed secondary programs. yt-dlp's `bgutil-ytdlp-pot-provider` plugin
keeps querying `http://localhost:774/get_pot` exactly as it does today —
the implementation behind that endpoint switches from goja+stub-DOM
(currently produces only websafe-fallback tokens) to a bundled Node + V8 +
JSDOM sidecar (proven to produce real integrity tokens because that's
what the upstream `bgutil-ytdlp-pot-provider/server/` runs in production).

**Default**: opt-out. `cfg.Bgutils.UseSidecar` defaults to `true` on
Windows. Owner can flip to `false` to fall back to the goja path (still
useful for debugging or if the sidecar refuses to start).

## 2. Non-goals

- Cross-platform Node bundling. Moombox is Windows-only per CLAUDE.md.
  When/if Linux/macOS support is added, additional Node binaries get
  bundled (each ~50 MB extracted), but that's deferred.
- Replacing goja entirely. Cipher player.js execution stays in goja —
  BotGuard is a different problem (anti-bot fingerprinting) than cipher
  (deterministic sig + n-param decryption that goja handles fine). Two
  JS environments running in parallel with no shared state.
- Hot-reloading the sidecar JS. A change to `bgutil-server.js` requires
  rebuilding Moombox.exe.
- Multiple concurrent sidecar instances. One per Moombox process; calls
  serialized through Go-side request queue if needed.

## 3. Architecture

### 3.1 Logical layout

```
                  yt-dlp + bgutil-ytdlp-pot-provider plugin
                                  │
                                  │ HTTP GET http://localhost:774/get_pot?content_binding=...
                                  ↓
                  ┌────────────────────────────────────────┐
                  │ Moombox HTTP server :774                │  (UNCHANGED)
                  │ /get_pot, /invalidate_caches,           │
                  │ /invalidate_it -- LoopbackOnly,         │
                  │ CSRF-exempt                             │
                  └────────────────┬───────────────────────┘
                                   ↓
                  ┌────────────────────────────────────────┐
                  │ internal/bgutils/PotProvider            │  (extended)
                  │  ├── sessionCache (Go map)              │
                  │  ├── minterCache (Go map; legacy)       │
                  │  ├── inflightDedup (Go sync.Map)        │
                  │  └── sidecar *Sidecar                   │
                  └────────────────┬───────────────────────┘
                                   ↓
                  ┌────────────────────────────────────────┐
                  │ internal/bgutils/sidecar.Sidecar        │  (NEW)
                  │  ├── go:embed node.exe.gz               │
                  │  ├── go:embed bgutil-server.js.gz       │
                  │  ├── extract-on-first-launch logic      │
                  │  ├── exec.Cmd + Job Object pinning      │
                  │  ├── stdin/stdout JSON-RPC channel      │
                  │  └── reqID -> chan response mux         │
                  └────────────────┬───────────────────────┘
                                   ↓ stdin/stdout pipes (no sockets)
                  ┌────────────────────────────────────────┐
                  │ Bundled node.exe                        │
                  │  └── bgutil-server.js (~50-line glue)  │
                  │       └── BgUtils SessionManager        │
                  │             └── BgUtils +              │
                  │                 JSDOM +                │
                  │                 V8 (real)              │
                  └────────────────────────────────────────┘
```

### 3.2 Build pipeline

```
references/bgutil-sidecar/         (gitignored except for the build inputs)
├── package.json                    # bgutil-ytdlp-pot-provider, jsdom, esbuild
├── src/server.js                   # ~50-line stdin/stdout JSON-RPC server
├── build.mjs                       # esbuild config + gzip + copy step
└── (built) dist/server.js          # bundled output, gzipped

tools/fetch-node/                   (committed; downloaded blob is gitignored)
├── main.go                         # Downloads pinned Node version,
│                                   # SHA-256 verifies, gzips node.exe,
│                                   # writes internal/bgutils/embed/
└── (downloaded blob, gitignored)   # node-vXX.X.X-win-x64.zip

internal/bgutils/embed/             (the embedded blobs)
├── node.exe.gz                     # ~28 MB; gitignored, generated by tools/fetch-node
├── server.js.gz                    # ~5 MB;  gitignored, generated by references/bgutil-sidecar/build.mjs
└── version.txt                     # committed; "node@vXX.X.X bundle@<sha256>"
                                    # used as cache-invalidation key
```

`go build ./cmd/moombox` reads the embedded files via `go:embed`. CI runs
`tools/fetch-node` and `references/bgutil-sidecar/build.mjs` before
building so the blobs are populated. Local dev: same scripts, run on
demand. Missing blobs at build time produce a clear error (a build tag
that compiles a stub when blobs are absent so CI without the build step
still produces a binary -- with sidecar disabled).

### 3.3 Sidecar lifecycle

**Startup** (called from `cmd/moombox/services.go` after PotProvider
construction, before web server starts accepting requests):

1. Resolve cache dir: `os.UserCacheDir() + "/Moombox/sidecar"` (preferred
   over `os.TempDir()` because Defender heuristics treat `%TEMP%`
   extractions as more suspicious than `%LOCALAPPDATA%`).
2. Read `version.txt` from embed; compare to existing
   `<cacheDir>/version.txt`. If equal, skip extraction; if not (or files
   missing), gunzip-extract `node.exe` + `server.js` to cacheDir, write
   the new `version.txt`.
3. Verify extracted files' SHA-256 against the value baked into Go at
   build time. Mismatch = error (means user tampered with cache, or AV
   stripped a section). On mismatch, refuse to start sidecar; PotProvider
   falls back to goja path with a one-time WARN log.
4. `cmd := exec.Command(cacheDir+"/node.exe", cacheDir+"/server.js")`.
   Wire `cmd.Stdin` / `cmd.Stdout` to pipes; `cmd.Stderr` to a Go-side
   line-reader that routes to Moombox's logger at DEBUG level.
5. `cmd.Start()` then `processJob.assign(cmd.Process)` to pin the child
   to a Windows Job Object so it dies when Moombox exits (we have this
   pattern in `internal/cookies/process_job_windows.go`).
6. Spawn a `readPump` goroutine that reads stdout line-by-line, parses
   JSON, looks up `reqID` in the pending-requests map, sends the response
   on the appropriate channel.
7. Send a `ping` JSON-RPC request and wait up to 5s for a `pong`
   response. If timeout: kill the process and report sidecar startup
   failure (PotProvider falls back to goja).
8. Sidecar is now ready; PotProvider can route mints through it.

**Per-request flow** (from PotProvider.generateAndMint):

```go
func (s *Sidecar) GeneratePoToken(ctx context.Context, binding string) (string, error) {
    reqID := s.nextReqID.Add(1)
    ch := make(chan rpcResponse, 1)
    s.pending.Store(reqID, ch)
    defer s.pending.Delete(reqID)

    req := rpcRequest{ID: reqID, Method: "generatePoToken", Params: map[string]any{"binding": binding}}
    if err := s.writeRequest(req); err != nil {
        return "", fmt.Errorf("sidecar write: %w", err)
    }
    select {
    case resp := <-ch:
        if resp.Error != "" { return "", fmt.Errorf("sidecar: %s", resp.Error) }
        return resp.Result.(map[string]any)["poToken"].(string), nil
    case <-ctx.Done():
        return "", ctx.Err()
    }
}
```

**Shutdown** (called from Moombox's graceful shutdown path):

1. Send `{"method":"shutdown"}` on stdin (sidecar replies, then exits cleanly).
2. Wait up to 5s for the process to exit.
3. If still running, `cmd.Process.Kill()` + Job Object terminates child tree.
4. Close the stdin/stdout pipes.

**Crash recovery**: if `readPump` sees stdout EOF or a write fails:

1. Mark sidecar as unhealthy (`atomic.Bool`).
2. Drain pending requests with an error.
3. Spawn a single-shot restart goroutine (with backoff: 1s, 5s, 30s).
4. While unhealthy, PotProvider falls back to the goja path (PotProvider
   checks `sidecar.IsHealthy()` before each call).

### 3.4 IPC protocol

JSON-RPC-style, line-delimited (one JSON object per line, separated by
`\n`). All fields ASCII-safe.

**Request shape:**

```json
{"id": 42, "method": "generatePoToken", "params": {"binding": "CgtqeHFFMXFkTXdXUSiH"}}
```

**Response shape (success):**

```json
{"id": 42, "result": {"poToken": "MnUxOWZmS...", "binding": "CgtqeHFFMXFkTXdXUSiH", "expiresAt": 1730123456000}}
```

**Response shape (error):**

```json
{"id": 42, "error": "BotGuard challenge fetch failed: connect ETIMEDOUT"}
```

**Methods:**

| Method | Params | Result | Notes |
|---|---|---|---|
| `ping` | (none) | `"pong"` | startup health check |
| `generatePoToken` | `{binding}` | `{poToken, binding, expiresAt}` | the hot path |
| `invalidateCaches` | (none) | `"ok"` | wipes sidecar's session + minter caches |
| `invalidateIT` | (none) | `"ok"` | wipes only minter cache (force fresh BotGuard) |
| `getStats` | (none) | `{cachedMinters, cachedSessions, mintsTotal, mintsErrored}` | observability |
| `shutdown` | (none) | `"bye"` | graceful exit, then process.exit(0) |

stderr from Node is a separate channel: routed unmodified to Moombox's
logger at DEBUG. Errors during JSON parse on stdout are logged at WARN
and the line is dropped (defensive against partial writes during crash).

### 3.5 PotProvider integration

`internal/bgutils/pot_provider.go` (existing) gets a new field and a
small branch:

```go
type PotProvider struct {
    // ... existing fields ...
    sidecar *sidecar.Sidecar  // nil if disabled or failed to start
}

func (pp *PotProvider) generateAndMint(ctx context.Context, binding string, bypassCache bool) (*SessionData, error) {
    // Sidecar path (preferred when healthy)
    if pp.sidecar != nil && pp.sidecar.IsHealthy() {
        token, err := pp.sidecar.GeneratePoToken(ctx, binding)
        if err == nil {
            return &SessionData{
                PoToken:        token,
                ContentBinding: binding,
                ExpiresAt:      time.Now().Add(pp.config.sessionTTL()),
            }, nil
        }
        pp.logger.Warn("sidecar PO generation failed; falling through to goja", "err", err)
    }

    // Existing goja path (fallback)
    return pp.generateAndMintViaGoja(ctx, binding, bypassCache)
}
```

The sessionCache + inflight-dedup logic from existing `GeneratePoToken`
stays in front of `generateAndMint`, so cache hits don't pay sidecar
latency. The minterCache (which currently caches a goja-backed minter)
becomes redundant when the sidecar is in use — leave it in place but
never populate it on the sidecar path. Cleanup-on-cache-eviction code
keeps working on the no-op empty map.

`InvalidateIntegrityTokens()` and `InvalidateCaches()` fan out to both
Moombox's caches AND the sidecar's caches (new sidecar method calls).

### 3.6 Cache layering

**Single source of truth: Moombox's PotProvider.sessionCache.**

| Cache | Authority | Behavior |
|---|---|---|
| PotProvider.sessionCache | Authoritative | Holds per-binding tokens for 6h; consulted by /get_pot before any sidecar call. |
| PotProvider.minterCache | Effectively unused under sidecar mode | Stays in code for goja fallback. |
| PotProvider.inflight | Authoritative | Dedups concurrent /get_pot calls for same binding so only one sidecar round-trip happens. |
| Sidecar SessionManager._minterCache (JS) | Pass-through | The sidecar internally caches the integrity-token minter (per binding key) so back-to-back generatePoToken calls don't re-run BotGuard. Cleared on `invalidateIT`. |
| Sidecar SessionManager.youtubeSessionDataCaches (JS) | Disabled | We don't pass a cache object on construction so the sidecar's per-session cache is null. Moombox's sessionCache covers this. |

This keeps Moombox the single source of truth for "is this token still
fresh?" while the sidecar caches the expensive BotGuard VM internally.

### 3.7 Wire compatibility with yt-dlp

The /get_pot HTTP route in `internal/web/routes/webpo_routes.go` (or
wherever it lives — to be looked up during implementation) is unchanged.
yt-dlp's `bgutil-ytdlp-pot-provider` plugin calls the same URL, gets the
same JSON shape, with the same content_binding param. Implementation
detail of which JS engine produced the token is invisible.

`/invalidate_caches` and `/invalidate_it` continue to work; they now
route through both Moombox's caches AND the sidecar (via the new methods).

## 4. Build pipeline details

### 4.1 Pinned Node version

Node v22 LTS (latest patch at implementation time). Pin in
`tools/fetch-node/main.go`:

```go
const nodeVersion = "v22.11.0"  // bump quarterly or on critical security advisory
const sha256Win64 = "..."        // verified at download time
```

Update process:
1. Bump `nodeVersion`, update `sha256Win64`.
2. Run `go run ./tools/fetch-node`.
3. Verify the new node.exe.gz size hasn't ballooned unexpectedly.
4. Run `MOOMBOX_LIVE_BG_TEST=1 go test ./internal/bgutils/...` to confirm
   nothing broke at the BgUtils end.
5. Commit `internal/bgutils/embed/version.txt`. The .gz blobs stay
   gitignored — CI rebuilds them.

### 4.2 esbuild config (references/bgutil-sidecar/build.mjs)

```js
import { build } from 'esbuild';
import { gzipSync } from 'zlib';
import { writeFileSync, readFileSync, mkdirSync } from 'fs';

await build({
    entryPoints: ['src/server.js'],
    bundle: true,
    format: 'esm',           // Node 20+ ESM
    platform: 'node',        // pull in Node-specific stdlib
    target: 'node22',
    outfile: 'dist/server.js',
    minify: true,
    sourcemap: false,
});

const raw = readFileSync('dist/server.js');
const gz = gzipSync(raw, { level: 9 });
mkdirSync('../../internal/bgutils/embed', { recursive: true });
writeFileSync('../../internal/bgutils/embed/server.js.gz', gz);
console.log(`bundled ${raw.length} bytes -> ${gz.length} bytes gzipped`);
```

### 4.3 fetch-node tool (tools/fetch-node/main.go)

```go
// Downloads Node Windows x64 release, extracts node.exe, gzips, writes
// to internal/bgutils/embed/node.exe.gz. Verifies SHA-256 against a
// pinned constant.
package main

import (
    "archive/zip"
    "bytes"
    "compress/gzip"
    "crypto/sha256"
    "encoding/hex"
    "fmt"
    "io"
    "net/http"
    "os"
    "path/filepath"
)

const nodeVersion = "v22.11.0"
const expectedSHA = "abc123..." // updated when nodeVersion bumps

func main() {
    url := fmt.Sprintf("https://nodejs.org/dist/%s/node-%s-win-x64.zip", nodeVersion, nodeVersion)
    fmt.Println("fetching", url)
    resp, err := http.Get(url)
    if err != nil { die(err) }
    defer resp.Body.Close()

    raw, err := io.ReadAll(resp.Body)
    if err != nil { die(err) }
    sum := sha256.Sum256(raw)
    if hex.EncodeToString(sum[:]) != expectedSHA {
        die(fmt.Errorf("SHA-256 mismatch: got %x want %s", sum, expectedSHA))
    }

    zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
    if err != nil { die(err) }
    for _, f := range zr.File {
        if filepath.Base(f.Name) == "node.exe" {
            extractAndGzip(f, "internal/bgutils/embed/node.exe.gz")
            // Write version.txt (consumed by sidecar.Start for cache invalidation)
            os.WriteFile("internal/bgutils/embed/version.txt",
                []byte(fmt.Sprintf("node@%s sha256@%s\n", nodeVersion, expectedSHA)), 0644)
            return
        }
    }
    die(fmt.Errorf("node.exe not found in zip"))
}

func extractAndGzip(f *zip.File, outPath string) { /* ... */ }
func die(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
```

### 4.4 CI integration

`.github/workflows/release.yml` (existing) adds two steps before the
existing `go build`:

```yaml
- uses: actions/setup-node@v4
  with:
    node-version: '22'

- name: Fetch embedded Node binary
  run: go run ./tools/fetch-node

- name: Build BotGuard sidecar bundle
  working-directory: references/bgutil-sidecar
  run: |
    npm ci
    node build.mjs

- name: Build Moombox
  run: go build -o moombox.exe ./cmd/moombox
```

`go-winres` for the icon stays unchanged.

### 4.5 Local dev workflow

First-time setup:
```bash
go run ./tools/fetch-node
cd references/bgutil-sidecar && npm ci && node build.mjs && cd ../..
go build ./cmd/moombox
```

Iterating on the JS sidecar:
```bash
cd references/bgutil-sidecar && node build.mjs && cd ../..
go build ./cmd/moombox
```

Iterating on Go-only code: just `go build`. Embedded blobs from the last
build step stay valid until version.txt drift forces a rebuild.

## 5. Configuration

```toml
# config.toml additions
[bgutils]
use_sidecar = true                # default true on Windows; false elsewhere
sidecar_startup_timeout_secs = 5  # ping/pong deadline at startup
sidecar_request_timeout_secs = 30 # per-method deadline
sidecar_restart_backoff_secs = [1, 5, 30]  # restart spacing on crash
```

`config/config.go` migration: since this is a new section, no migration
needed; defaults applied on missing config.

## 6. Implementation phase plan

### Phase 1 — Build inputs (1 day)

- `references/bgutil-sidecar/package.json` with deps: `bgutil-ytdlp-pot-provider`, `jsdom`, `esbuild`.
- `references/bgutil-sidecar/src/server.js` — the ~50-line stdin/stdout glue from §3.4.
- `references/bgutil-sidecar/build.mjs` — esbuild config from §4.2.
- Smoke test: `node dist/server.js` standalone, manually feed it `{"id":1,"method":"ping"}`, expect `{"id":1,"result":"pong"}`.
- Real BotGuard test: `{"id":2,"method":"generatePoToken","params":{"binding":"someBindingValue"}}`, expect a non-empty `poToken`.

### Phase 2 — Embedded Node (0.5 day)

- `tools/fetch-node/main.go` from §4.3.
- Run it locally; confirm `node.exe.gz` lands in `internal/bgutils/embed/`.
- Add `internal/bgutils/embed/.gitignore` excluding `*.gz`.
- Commit `version.txt`.

### Phase 3 — Subprocess manager (1.5 days)

- `internal/bgutils/sidecar/sidecar.go` with the API in §3.3.
- `internal/bgutils/sidecar/sidecar_windows.go` — Job Object pinning (copied from `internal/cookies/process_job_windows.go` pattern).
- `internal/bgutils/sidecar/embed.go` — `go:embed` directives + extraction logic with SHA-256 verification.
- Tests:
  - `TestSidecarPingPong` — start, ping, get pong, stop. Doesn't hit network.
  - `TestSidecarGracefulShutdown` — Stop returns within 5s.
  - `TestSidecarHardKill` — kill the child mid-request; readPump cleanup; pending requests get errors.

### Phase 4 — PotProvider integration (0.5 day)

- Add `sidecar *sidecar.Sidecar` field to PotProvider.
- Branch in `generateAndMint` per §3.5.
- Wire `Sidecar.InvalidateCaches()` / `Sidecar.InvalidateIT()` into the existing `InvalidateCaches` / `InvalidateIntegrityTokens` flow.
- Config flag plumbing in `cmd/moombox/services.go` — start sidecar after config load, pass to PotProvider constructor.
- Existing `TestPotProvider*` tests should keep passing (sidecar = nil in unit tests; goja path is exercised).

### Phase 5 — Live integration test (1 day)

- Extend `internal/bgutils/botguard_live_test.go` `TestBotGuardLiveFingerprint` with a sidecar variant: skip unless `MOOMBOX_LIVE_BG_TEST=1` AND sidecar prerequisites are present.
- New test `TestSidecarLivePoToken` — runs the full sidecar flow and asserts a non-empty `poToken` comes back from the real Google endpoint via the sidecar.
- Test `TestSidecarFallsBackOnDeath` — start sidecar, kill it, request a token; PotProvider should log a warn and fall through to the goja path (which still returns the websafe-only error, but the test verifies the fallback path is exercised).

### Phase 6 — Build automation + AV submission + ship (1 day)

- Update `.github/workflows/release.yml` per §4.4.
- Update CLAUDE.md "Build & Test" section with the new prerequisites (Node + npm install step).
- Update `RELEASE_NOTES.md` template to mention the sidecar.
- Code-sign the new larger binary, run through VirusTotal, submit to Microsoft Defender + Bitdefender + Kaspersky pre-release for false-positive prevention.
- Tag the first release with the sidecar enabled by default.

**Total: ~5 days focused work.**

## 7. Risk register

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| Antivirus false positives on the embedded node.exe extraction | High | High (releases can't ship if AV flags) | Code-sign extracted node.exe with our cert; pre-submit binary to MS + Bitdefender + Kaspersky; document at-launch SHA-256 verification so users can audit |
| Node version security advisory mid-release | Medium | Medium (users on the affected version) | Quarterly LTS bumps as standard maintenance; out-of-band patch if a critical CVE drops |
| Sidecar adds 30+ MB to Moombox.exe | Certain | Medium (download size, slower update apply) | Document in release notes; gzipped extraction makes runtime footprint bearable |
| Sidecar process leaks across Moombox crashes | Medium | Low (one zombie node.exe per crash) | Job Object pins child to parent; on next Moombox start, scan for orphaned Moombox-tagged node.exe processes and kill |
| First-launch extraction fails (disk full / locked file / Defender quarantine) | Low | High (PO tokens unavailable) | Fall back to goja path with one-time WARN; surface in dashboard "PO tokens degraded" status pill |
| BgUtils upstream API changes break our `bgutil-server.js` glue | Medium | Medium (sidecar fails after npm update) | Pin to specific bgutil-ytdlp-pot-provider version in package.json; integration test catches breakage at build time |
| User runs Moombox.exe from a read-only network share | Low | Medium (extraction fails) | Cache dir defaults to `%LOCALAPPDATA%/Moombox/sidecar` which is always writable; fallback to `%TEMP%` if not |
| stdin/stdout deadlock under heavy concurrent load | Low | Medium | Single-writer stdin (mutex on Sidecar.writeRequest); readPump goroutine drains stdout continuously into per-request channels |
| Goja-path tests stop being run / atrophy | High | Low (works until users disable sidecar) | Keep `TestPotProvider*` exercising the goja path with `UseSidecar=false`; CI runs both modes |

## 8. Open questions

1. **Node binary source verification**: pull from `nodejs.org/dist/` directly, or mirror to a Moombox-controlled S3 / Cloudflare R2 bucket? Direct is simpler; mirror gives us audit trail + offline build capability. **Recommendation: direct from nodejs.org for now; mirror if a build-reliability incident occurs.**
2. **First-launch UX**: should we extract the sidecar binary asynchronously after Moombox starts (faster perceived startup, PO tokens unavailable for ~1s) or synchronously before (slower startup, PO tokens ready immediately)? **Recommendation: synchronous, with progress reported to TUI/Web UI splash. The 1-2s extraction is one-time per Node version.**
3. **Cleanup on uninstall**: who removes `%LOCALAPPDATA%/Moombox/sidecar`? Nothing in Moombox today does cleanup. **Recommendation: add a `moombox uninstall-data` CLI subcommand; document in README; not required for first ship.**
4. **Telemetry**: do we want to ship sidecar success/failure metrics to a Moombox-controlled endpoint? **Recommendation: no. Logs locally; user can share if reporting bug.**
5. **Sidecar's BgUtils version vs reality**: `bgutil-ytdlp-pot-provider` releases new versions periodically. Pin or float? **Recommendation: pin major.minor in package.json (`^1.2.0`); accept patch updates; review minor bumps before merging to ensure server.js glue still works.**

## 9. Test coverage matrix

| Test | Type | Network? | Gated on env var? |
|---|---|---|---|
| `TestPotProviderSessionCache*` (existing) | Unit | No | No |
| `TestPotProviderInflightDedup*` (existing) | Unit | No | No |
| `TestSidecarPingPong` (Phase 3) | Integration | No (subprocess only) | Yes (`MOOMBOX_BUILD_SIDECAR=1` to ensure embed blobs exist) |
| `TestSidecarGracefulShutdown` (Phase 3) | Integration | No | Yes |
| `TestSidecarHardKill` (Phase 3) | Integration | No | Yes |
| `TestBotGuardLiveFingerprint` (existing, goja path) | Live | Yes | Yes (`MOOMBOX_LIVE_BG_TEST=1`) |
| `TestSidecarLivePoToken` (Phase 5) | Live | Yes | Yes (both env vars) |
| `TestSidecarFallsBackOnDeath` (Phase 5) | Integration | No | Yes |
| `TestSidecarConcurrentRequests` (Phase 5) | Integration | No | Yes |

CI runs the unit tests by default; integration tests require the Node
binary + JS bundle to be present (which they will be on the release
build runner). Live tests run on a manually-triggered workflow only.

## 10. Open-source attribution

Embedded inside Moombox.exe:

- **Node.js** — MIT-style license. Bundle the LICENSE file as a build artifact alongside Moombox.exe; reference in README.
- **bgutil-ytdlp-pot-provider** — MIT (verify). Same.
- **jsdom** — MIT (verify). Same.
- **bgutils-js** (transitive) — MIT (verify). Same.

Run a license audit step in CI (`license-checker --production` for the npm side; `go-licenses` for the Go side) and fail the build on incompatibility.

## 11. Documentation deliverables

- **CLAUDE.md** — add the Node + npm prerequisites to "Build & Test" section.
- **README.md** — add a "BotGuard PO tokens" section explaining the sidecar exists, runs locally, no telemetry, can be disabled.
- **docs/spec/platform-services.md** — extend the YouTube section to describe the sidecar architecture.
- **RELEASE_NOTES.md** — first release with sidecar gets a prominent "What changed: PO tokens now generate end-to-end via embedded Node sidecar; binary size grew by ~35 MB".
- **This doc** — link from PHASE-PLAN.md / next-iteration plan.

---

End of design doc.
