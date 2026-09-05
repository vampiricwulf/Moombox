# Arc A — Toolchain, Dependencies, Node Pin, Housekeeping — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every Moombox build (local, CI, Docker) runs on a patched Go 1.27 toolchain with current direct dependencies and Node v24 LTS in the sidecar, and the stale worktree, branch, and implemented plan docs are gone.

**Architecture:** Pin-only changes plus regenerated embed blobs. No production logic changes. Each dependency moves in its own commit so a regression bisects to one module. The goja bump goes last behind the cipher and sidecar live gates.

**Tech Stack:** Go 1.27.1, Go modules, Node v24.20.0 (embedded via `tools/fetch-node`), npm, GitHub Actions, Docker.

**Spec:** `docs/superpowers/specs/2026-09-04-improvement-chain-design.md` §3 (Arc A), §2 (execution model).

## Global Constraints

- go.mod: `go 1.27` and `toolchain go1.27.1` — the toolchain line is a floor, never a pin.
- Embedded Node: `v24.20.0` with SHA-256 `6cac9ffbca8f6a47091e4b5c772e0606049c3871cb67d900c0cedde630e545ba` (win-x64.zip), `2f2c0da162318f0de47665410c7c8c2ed3d36c8f3105de4bbc61176c70a7cbf2` (linux-x64.tar.xz), `5f4ddab610c1ab2016b3c227cebdbf6d9495161487e4739c7b90090595f465f7` (linux-arm64.tar.xz).
- Direct module targets: `modernc.org/sqlite@v1.58.0`, `golang.org/x/crypto@v0.56.0`, `github.com/mattn/go-runewidth@v0.0.29`, `github.com/yuin/goldmark@v1.8.6`, `github.com/dop251/goja@v0.0.0-20260903201622-f87b40ad7341`. Indirects move only where `go mod tidy` requires.
- The implementer works ONLY in `D:/Git/Moombox/.worktrees/improvement-a-toolchain` (branch `improvement-a-toolchain`). Task 6's worktree/branch removal runs from the main checkout `D:/Git/Moombox`.
- Every `go` command carries `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp`.
- ONE `go test -count=1 ./...` at a time in the whole machine; single-package runs are free.
- `MOOMBOX_LIVE_BG_TEST=1` and `MOOMBOX_LIVE_CIPHER_TEST=1` gates run where stated. `TestBotGuardLiveFingerprint` (goja BotGuard, `internal/bgutils/botguard_live_test.go`) is a KNOWN long-standing failure and is NOT a gate — never call it a regression.
- Nothing pushes except Task 6's `git push origin --delete player-review-plan` (owner-ruled Q13).
- Commit trailer on every commit:
  ```
  Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01Jw2Ne9cGsn3R9bJBNuXudp
  ```
- Ledger: `D:/Git/Moombox/.superpowers/sdd/2026-09-04-improvement-chain/progress.md` (gitignored). Append one dated line per task: what landed, gate results, anything ruled.

---

### Task 1: Worktree, ledger, and the Go 1.27 floor

**Files:**
- Modify: `go.mod:3` (`go 1.26` → `go 1.27` + `toolchain go1.27.1`)
- Modify: `Dockerfile:40` (`golang:1.26-bookworm` → `golang:1.27-bookworm`)
- Modify: `SPEC.md:5`, `SPEC.md:800`
- Modify: `docs/spec/operations.md:9`, `docs/spec/operations.md:99`
- Modify: `docs/spec/appendix-metrics.md:20`
- Create (gitignored): `.superpowers/sdd/2026-09-04-improvement-chain/progress.md`

**Interfaces:**
- Produces: the worktree every later task edits in, with the embed blobs present so `go build ./...` works.

- [ ] **Step 1: Create the worktree from main and copy the embed blobs in**

The blobs are gitignored, so a fresh worktree cannot compile without them.

```bash
cd D:/Git/Moombox
git worktree add -b improvement-a-toolchain .worktrees/improvement-a-toolchain main
cp internal/bgutils/embed/node-windows-amd64.gz internal/bgutils/embed/node-linux-amd64.gz \
   internal/bgutils/embed/node-linux-arm64.gz internal/bgutils/embed/sidecar.tar.gz \
   .worktrees/improvement-a-toolchain/internal/bgutils/embed/
ls -la .worktrees/improvement-a-toolchain/internal/bgutils/embed/
```
Expected: four blobs listed beside `version.txt`.

- [ ] **Step 2: Open the chain ledger**

```bash
mkdir -p D:/Git/Moombox/.superpowers/sdd/2026-09-04-improvement-chain
cat > D:/Git/Moombox/.superpowers/sdd/2026-09-04-improvement-chain/progress.md <<'EOF'
# Improvement chain — ledger (spec: docs/superpowers/specs/2026-09-04-improvement-chain-design.md)

Order: A, D, B, C, E, F, G, H, I, J, K. One arc at a time; one full `go test` at a time.

## Arc A — toolchain, dependencies, Node pin, housekeeping (branch improvement-a-toolchain)
EOF
```

- [ ] **Step 3: Verify the baseline in the worktree**

```bash
cd D:/Git/Moombox/.worktrees/improvement-a-toolchain
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./... && GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./...
```
Expected: no output, exit 0.

- [ ] **Step 4: Raise the Go floor**

```bash
cd D:/Git/Moombox/.worktrees/improvement-a-toolchain
go mod edit -go=1.27 -toolchain=go1.27.1
sed -n 1,6p go.mod
```
Expected:
```
module github.com/vampiricwulf/Moombox

go 1.27

toolchain go1.27.1
```

- [ ] **Step 5: Move the Dockerfile build stage**

In `Dockerfile:40` change
```
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build
```
to
```
FROM --platform=$BUILDPLATFORM golang:1.27-bookworm AS build
```

- [ ] **Step 6: Update the four doc lines that name Go 1.26**

`SPEC.md:5` — replace `Go 1.26, single binary.` with `Go 1.27, single binary.`

`SPEC.md:800` — replace the leading `Go 1.26 required.` with `Go 1.27 required (go.mod carries `toolchain go1.27.1` as the floor, so an older local Go auto-downloads it).`

`docs/spec/operations.md:9` — replace `Build requires **Go 1.26** (see `go.mod` for exact patch version).` with `Build requires **Go 1.27**; `go.mod` carries `toolchain go1.27.1`, the floor every build (local, CI, Docker) auto-downloads.`

`docs/spec/operations.md:99` — replace `` (`golang:1.26-bookworm`) `` with `` (`golang:1.27-bookworm`) ``.

`docs/spec/appendix-metrics.md:20` — replace `- **Go version:** 1.26` with `- **Go version:** 1.27 (`toolchain go1.27.1`)`.

- [ ] **Step 7: Build, vet, and run the ONE full suite**

```bash
cd D:/Git/Moombox/.worktrees/improvement-a-toolchain
go version
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./... && GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./... && gofmt -l ./cmd ./internal ./tools
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./... 2>&1 | grep -vE '^ok|no test files'; echo "exit ${PIPESTATUS[0]}"
```
Expected: `go version go1.27.1 windows/amd64`; build/vet/gofmt silent; the test line prints only `exit 0` (28 ok).

- [ ] **Step 8: Commit**

```bash
cd D:/Git/Moombox/.worktrees/improvement-a-toolchain
git add go.mod Dockerfile SPEC.md docs/spec/operations.md docs/spec/appendix-metrics.md
git commit -F - <<'MSG'
chore(toolchain): go 1.27 with toolchain go1.27.1 as the floor

The local exe was built with go1.26.1 (16 reachable stdlib vulns) while CI
resolved go1.26.7 — go.mod had no floor. The toolchain line makes every
build auto-download at least 1.27.1; the Dockerfile build stage and the
four doc lines naming Go 1.26 follow.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Jw2Ne9cGsn3R9bJBNuXudp
MSG
```
Append to the ledger: `- <date> Task 1: go 1.27 floor landed (<sha>); full suite 28/28.`

---

### Task 2: Four direct module bumps (sqlite, x/crypto, runewidth, goldmark)

**Files:**
- Modify: `go.mod`, `go.sum` (four commits)

**Interfaces:**
- Consumes: the Task 1 worktree.
- Produces: go.mod at the four target versions; goja untouched.

Run each bump as its own cycle: get, tidy, build, the affected package tests, commit. Do NOT batch them.

- [ ] **Step 1: modernc.org/sqlite → v1.58.0**

```bash
cd D:/Git/Moombox/.worktrees/improvement-a-toolchain
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go get modernc.org/sqlite@v1.58.0 && GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go mod tidy
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./... && GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/database/ ./internal/worker/
git diff --stat go.mod go.sum
```
Expected: build silent; `ok internal/database`, `ok internal/worker`. The diff moves `modernc.org/sqlite` and whatever `modernc.org/*` it requires — nothing else.

```bash
git add go.mod go.sum && git commit -F - <<'MSG'
chore(deps): modernc.org/sqlite v1.58.0

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Jw2Ne9cGsn3R9bJBNuXudp
MSG
```

- [ ] **Step 2: golang.org/x/crypto → v0.56.0**

```bash
cd D:/Git/Moombox/.worktrees/improvement-a-toolchain
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go get golang.org/x/crypto@v0.56.0 && GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go mod tidy
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./... && GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/web/ ./internal/updater/ ./internal/cookies/...
git diff --stat go.mod go.sum
```
Expected: build silent; the three packages `ok`. `golang.org/x/sys`/`x/net` may move with it (tidy-required) — acceptable; anything outside `golang.org/x/*` moving is not, revert and report.

```bash
git add go.mod go.sum && git commit -F - <<'MSG'
chore(deps): golang.org/x/crypto v0.56.0

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Jw2Ne9cGsn3R9bJBNuXudp
MSG
```

- [ ] **Step 3: github.com/mattn/go-runewidth → v0.0.29**

```bash
cd D:/Git/Moombox/.worktrees/improvement-a-toolchain
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go get github.com/mattn/go-runewidth@v0.0.29 && GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go mod tidy
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./... && GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/tui/ ./internal/utils/
```
Expected: build silent; both packages `ok` (runewidth feeds the TUI's column math and `utils` text helpers).

```bash
git add go.mod go.sum && git commit -F - <<'MSG'
chore(deps): github.com/mattn/go-runewidth v0.0.29

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Jw2Ne9cGsn3R9bJBNuXudp
MSG
```

- [ ] **Step 4: github.com/yuin/goldmark → v1.8.6**

```bash
cd D:/Git/Moombox/.worktrees/improvement-a-toolchain
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go get github.com/yuin/goldmark@v1.8.6 && GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go mod tidy
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./... && GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/tui/
```
Expected: build silent; `ok internal/tui` (goldmark renders the release-notes overlay through glamour).

```bash
git add go.mod go.sum && git commit -F - <<'MSG'
chore(deps): github.com/yuin/goldmark v1.8.6

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Jw2Ne9cGsn3R9bJBNuXudp
MSG
```

- [ ] **Step 5: Confirm only goja remains outdated among direct modules**

```bash
cd D:/Git/Moombox/.worktrees/improvement-a-toolchain
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go list -m -u -f '{{if and .Update (not .Indirect)}}{{.Path}} {{.Version}} -> {{.Update.Version}}{{end}}' all
```
Expected: exactly one line, `github.com/dop251/goja v0.0.0-20260806115107-493f22071ef6 -> v0.0.0-20260903201622-f87b40ad7341`.

Append to the ledger: `- <date> Task 2: sqlite/x-crypto/runewidth/goldmark bumped in four commits (<shas>); package tests green.`

---

### Task 3: goja bump behind the live gates

**Files:**
- Modify: `go.mod`, `go.sum`

**Interfaces:**
- Consumes: Task 2's go.mod.
- Produces: goja at `v0.0.0-20260903201622-f87b40ad7341`; proof that the cipher solver and the sidecar mint still work live.

- [ ] **Step 1: Bump and tidy**

```bash
cd D:/Git/Moombox/.worktrees/improvement-a-toolchain
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go get github.com/dop251/goja@v0.0.0-20260903201622-f87b40ad7341 && GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go mod tidy
git diff go.mod
```
Expected: goja moves; `github.com/dlclark/regexp2/v2` and `github.com/dop251/goja_nodejs` may move if goja now requires newer ones. Record what moved in the ledger.

- [ ] **Step 2: Offline tests for every goja consumer**

```bash
cd D:/Git/Moombox/.worktrees/improvement-a-toolchain
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./... && GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/goja/ ./internal/cipher/ ./internal/bgutils/ ./internal/bgutils/sidecar/ ./internal/youtube/
```
Expected: all five `ok`. (`internal/bgutils/sidecar` takes ~35 s; it starts the embedded Node.)

- [ ] **Step 3: Cipher live gate**

```bash
cd D:/Git/Moombox/.worktrees/improvement-a-toolchain
MOOMBOX_LIVE_CIPHER_TEST=1 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -v -run 'TestSidecarSolverLive' ./internal/cipher/ 2>&1 | tail -5
```
Expected: `--- PASS: TestSidecarSolverLive` and `ok`. A `SKIP` means the env var did not reach the test — fix the invocation, do not proceed.

- [ ] **Step 4: Sidecar live gate**

```bash
cd D:/Git/Moombox/.worktrees/improvement-a-toolchain
MOOMBOX_LIVE_BG_TEST=1 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -v -run 'TestSidecarLivePoToken|TestSidecarLiveGvsMint' ./internal/bgutils/sidecar/ 2>&1 | grep -E '^(--- |ok|FAIL)'
```
Expected: both `--- PASS` and `ok`. Do NOT run `TestBotGuardLiveFingerprint` (goja path) as a gate — it is the documented long-standing failure.

- [ ] **Step 5: Commit**

```bash
cd D:/Git/Moombox/.worktrees/improvement-a-toolchain
git add go.mod go.sum && git commit -F - <<'MSG'
chore(deps): github.com/dop251/goja 20260903

Bumped last and alone; TestSidecarSolverLive and the two sidecar live mints
pass on the new goja. The goja BotGuard fingerprint test is the documented
long-standing failure and is not a gate.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Jw2Ne9cGsn3R9bJBNuXudp
MSG
```
Append to the ledger with the live-gate results and any indirect that moved.

---

### Task 4: Node v24.20.0 in fetch-node, Dockerfile, release workflow, docs; regenerate the embeds

**Files:**
- Modify: `tools/fetch-node/main.go:15`, `:43-47`, `:69`, `:75`, `:81`, `:268`
- Modify: `Dockerfile:26`
- Modify: `.github/workflows/release.yml:69`, `:83`
- Modify: `SPEC.md:38`, `SPEC.md:404`
- Modify: `docs/spec/operations.md:98`, `docs/spec/operations.md:197`
- Modify: `docs/spec/platform-services.md:687`, `docs/spec/platform-services.md:736`
- Modify (generated): `internal/bgutils/embed/version.txt`
- Regenerated (gitignored): the three `node-*.gz` blobs and `sidecar.tar.gz`
- Test: `tools/fetch-node/main_test.go` (existing), `internal/bgutils/sidecar` (existing integration + live)

**Interfaces:**
- Produces: `version.txt` beginning `node@v24.20.0` — the value the release workflow's cache key hashes.

- [ ] **Step 1: Edit the fetch-node manifest**

`tools/fetch-node/main.go:15` — replace
```
//  1. Pick a new Node v22 LTS patch from https://nodejs.org/dist/index.json
```
with
```
//  1. Pick a new Node v24 LTS patch from https://nodejs.org/dist/index.json
```

`tools/fetch-node/main.go:43-47` — replace
```go
// Pinned Node.js v22 LTS release. Bump quarterly or on critical CVE.
//
// Last bumped: 2026-04-26 — v22.22.2 was the latest v22 LTS (Jod) at the
// time the sidecar landed.
const nodeVersion = "v22.22.2"
```
with
```go
// Pinned Node.js v24 LTS release. Bump quarterly or on critical CVE.
//
// Last bumped: 2026-09-04 — v24.20.0 was the latest v24 LTS (Krypton);
// moved off the v22 (Jod) line, which is in maintenance until 2027-04.
const nodeVersion = "v24.20.0"
```

The three `expectedSHA` values:
- `:69` (windows/amd64, `win-x64`): `7c93e9d92bf68c07182b471aa187e35ee6cd08ef0f24ab060dfff605fcc1c57c` → `6cac9ffbca8f6a47091e4b5c772e0606049c3871cb67d900c0cedde630e545ba`
- `:75` (linux/amd64, `linux-x64`): `88fd1ce767091fd8d4a99fdb2356e98c819f93f3b1f8663853a2dee9b438068a` → `2f2c0da162318f0de47665410c7c8c2ed3d36c8f3105de4bbc61176c70a7cbf2`
- `:81` (linux/arm64, `linux-arm64`): `e9e1930fd321a470e29bb68f30318bf58e3ecb4acb4f1533fb19c58328a091fe` → `5f4ddab610c1ab2016b3c227cebdbf6d9495161487e4739c7b90090595f465f7`

`tools/fetch-node/main.go:268` — replace `// e.g. node-v22.22.2-linux-x64/bin/node` with `// e.g. node-v24.20.0-linux-x64/bin/node`.

- [ ] **Step 2: Run the fetch-node unit tests**

```bash
cd D:/Git/Moombox/.worktrees/improvement-a-toolchain
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./tools/fetch-node/
```
Expected: `ok`.

- [ ] **Step 3: Move the Dockerfile sidecar stage and the release workflow**

`Dockerfile:26` — `node:22-bookworm-slim` → `node:24-bookworm-slim`.

`.github/workflows/release.yml:69` — `node-version: '22'` → `node-version: '24'`.

`.github/workflows/release.yml:83` — replace `# Downloads pinned Node v22 LTS binaries for Windows x64, Linux x64,` with `# Downloads pinned Node v24 LTS binaries for Windows x64, Linux x64,`.

- [ ] **Step 4: Update the six doc lines naming Node v22**

`SPEC.md:38` — `| Node.js v22 LTS (embedded) |` → `| Node.js v24 LTS (embedded) |`.

`SPEC.md:404` — `A bundled Node.js v22 binary plus` → `A bundled Node.js v24 binary plus`.

`docs/spec/operations.md:98` — `` (`node:22-bookworm-slim`) `` → `` (`node:24-bookworm-slim`) ``.

`docs/spec/operations.md:197` — `downloads pinned Node v22 LTS for all 3 platforms` → `downloads pinned Node v24 LTS for all 3 platforms`.

`docs/spec/platform-services.md:687` — `Moombox embeds a Node.js v22 binary plus` → `Moombox embeds a Node.js v24 binary plus`.

`docs/spec/platform-services.md:736` — `gzipped Node.js v22 binary for the build's GOOS/GOARCH` → `gzipped Node.js v24 binary for the build's GOOS/GOARCH`.

Then confirm nothing else names the old pin:
```bash
cd D:/Git/Moombox/.worktrees/improvement-a-toolchain
grep -rnE 'v22\.22\.2|Node(\.js)? ?v?22|node:22' --include='*.md' --include='*.go' --include='*.yml' --include='Dockerfile' . | grep -v '^./references/' | grep -v node_modules
```
Expected: no output.

- [ ] **Step 5: Regenerate the embedded Node blobs (network; ~100 MB)**

```bash
cd D:/Git/Moombox/.worktrees/improvement-a-toolchain
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go run ./tools/fetch-node
cat internal/bgutils/embed/version.txt
```
Expected: three downloads, each `SHA-256 OK`; `version.txt` reads `node@v24.20.0 windows-amd64@6cac9ffb… linux-amd64@2f2c0da1… linux-arm64@5f4ddab6…` (full digests). A SHA mismatch means the manifest digest is wrong — stop and re-fetch `SHASUMS256.txt`, do not edit the digest to match the download.

- [ ] **Step 6: Rebuild the sidecar payload against the same manifest**

```bash
cd D:/Git/Moombox/.worktrees/improvement-a-toolchain/bgutil-sidecar
npm ci --omit=dev --no-audit --no-fund --ignore-scripts && node build.mjs
ls -la ../internal/bgutils/embed/
```
Expected: `sidecar.tar.gz` freshly written beside the three new `node-*.gz` blobs.

- [ ] **Step 7: Sidecar integration tests on Node 24 and the live mint**

```bash
cd D:/Git/Moombox/.worktrees/improvement-a-toolchain
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./... && GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/bgutils/... ./internal/cipher/
MOOMBOX_LIVE_BG_TEST=1 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -v -run 'TestSidecarLivePoToken|TestSidecarLiveGvsMint' ./internal/bgutils/sidecar/ 2>&1 | grep -E '^(--- |ok|FAIL)'
MOOMBOX_LIVE_CIPHER_TEST=1 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -v -run 'TestSidecarSolverLive' ./internal/cipher/ 2>&1 | grep -E '^(--- |ok|FAIL)'
```
Expected: all offline packages `ok`; the two sidecar live mints and the cipher live solve `--- PASS`. These prove the extracted Node 24 binary runs the sidecar JS identically.

- [ ] **Step 8: Commit (version.txt is tracked; the blobs are not)**

```bash
cd D:/Git/Moombox/.worktrees/improvement-a-toolchain
git status --short
git add tools/fetch-node/main.go Dockerfile .github/workflows/release.yml SPEC.md docs/spec/operations.md docs/spec/platform-services.md internal/bgutils/embed/version.txt
git commit -F - <<'MSG'
chore(sidecar): embedded Node v24.20.0 (Krypton LTS)

fetch-node manifest, Dockerfile sidecar stage, release setup-node, and the
six doc lines move from the v22 (Jod) line. Blobs regenerated; the sidecar
integration suite, both live mints, and the live cipher solve pass on 24.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Jw2Ne9cGsn3R9bJBNuXudp
MSG
```
Expected `git status` before the add: only those files modified; the `.gz` blobs do not appear (gitignored). Append to the ledger.

---

### Task 5: qemu action v4 and the web/tests jsdom harmonization

**Files:**
- Modify: `.github/workflows/docker-publish.yml:28`
- Modify: `web/tests/package.json`, `web/tests/package-lock.json`
- Test: `web/tests/*.test.mjs` (existing, 92 tests)

- [ ] **Step 1: Bump the qemu action**

`.github/workflows/docker-publish.yml:28` — `- uses: docker/setup-qemu-action@v3` → `- uses: docker/setup-qemu-action@v4`.

- [ ] **Step 2: Move web/tests to jsdom 30 (the sidecar's line)**

```bash
cd D:/Git/Moombox/.worktrees/improvement-a-toolchain/web/tests
npm install --save-dev jsdom@^30.0.1 --no-audit --no-fund
grep -n jsdom package.json
```
Expected: `"jsdom": "^30.0.1"`; `package-lock.json` rewritten.

- [ ] **Step 3: Run the node suite**

```bash
cd D:/Git/Moombox/.worktrees/improvement-a-toolchain/web/tests
node --test ./*.test.mjs 2>&1 | grep -E '^ℹ (tests|pass|fail|skipped)'
```
Expected: `tests 92`, `pass 92`, `fail 0`, `skipped 0` (the DOM suite must run, not skip — a skip means jsdom failed to load).

- [ ] **Step 4: Commit**

```bash
cd D:/Git/Moombox/.worktrees/improvement-a-toolchain
git add .github/workflows/docker-publish.yml web/tests/package.json web/tests/package-lock.json
git commit -F - <<'MSG'
chore(ci,web-tests): setup-qemu-action v4; web/tests on jsdom 30

qemu v3 targets the deprecated Node 20 action runtime. web/tests moves to the
same jsdom major the sidecar uses; the 92-test node suite passes with the DOM
suite running.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Jw2Ne9cGsn3R9bJBNuXudp
MSG
```

---

### Task 6: Housekeeping — stale worktree and branch, implemented plan docs, local staticcheck

**Files:**
- Remove (main checkout): worktree `.claude/worktrees/player-review-plan`, branch `player-review-plan` (local + `origin`)
- Delete (arc branch): 19 files under `docs/superpowers/plans/`, 16 files under `docs/superpowers/specs/` (list in Step 3)
- Modify: `cmd/moombox/services.go:640`, `internal/cipher/solver_composite.go:15` (comments citing a spec being deleted)

**Interfaces:**
- Produces: `docs/superpowers/plans/` holds only `2026-08-29-cookie-remediation-field-test-plan.md` and this plan; `docs/superpowers/specs/` holds only `2026-09-04-improvement-chain-design.md`.

- [ ] **Step 1: Remove the stale worktree and branch (from the MAIN checkout)**

Its lock names a session PID that no longer exists (verified 2026-09-04). The commit it carried (`b49152f`, the player plan doc) is superseded by the merged implementation `5b90360`.

```bash
cd D:/Git/Moombox
git worktree unlock .claude/worktrees/player-review-plan
git worktree remove --force .claude/worktrees/player-review-plan
git worktree prune
git branch -D player-review-plan
git push origin --delete player-review-plan
git worktree list; git branch -a
```
Expected: worktree list shows `D:/Git/Moombox` and `.worktrees/improvement-a-toolchain` only; no `player-review-plan` locally or on `origin`. If `git worktree remove` fails with a Windows "in use" error, report the holding process — do NOT kill anything by image name.

- [ ] **Step 2: Verify each plan/spec slated for removal is implemented**

Run from the arc worktree. Every row must show its evidence before Step 3 deletes it; a row with no evidence stays and is listed in the ledger.

| File | Evidence command | Expected |
|---|---|---|
| plans/2026-05-02-linux-build-support.md + specs/…-design.md | `ls internal/utils/dacl_unix.go internal/bgutils/embed/embed_linux_amd64.go` | both exist (Linux binaries ship since 2.6.x) |
| plans/2026-05-03-twitch-flap-auto-recovery.md | `git log --oneline -1 --grep='Twitch flap auto-recovery'` | `0fc7bb9 … 2.6.10` |
| plans/2026-05-05-cipher-via-ejs-sidecar.md + specs/…-design.md | `git log --oneline -1 --grep='cipher via ejs sidecar'` | `ab26aec … 2.6.15` |
| plans/2026-06-02-connectivity-detection-redesign.md + specs/…-design.md | `ls internal/connectivity/probe.go internal/connectivity/passive.go` | both exist |
| plans/2026-06-26-downloader-activity-indicator.md + specs/…-design.md | `git log --oneline -1 --grep='activity indicator'` | `526a1ba … 2.7.0` |
| specs/2026-07-07-hls-ffmpeg-reload-pacing-design.md | `git log --oneline -1 --grep='reload pacing'` | `7b60025` |
| specs/2026-07-14-connectivity-detection-hardening-design.md | `git log --oneline -1 -i --grep='connectivity hardening'` | `f86f180` |
| specs/2026-07-15-feed-history.md + plans/2026-07-16-feed-history-{1..5}-*.md | `git log --oneline -1 --grep='feed history'`; `ls internal/monitor/backfill.go` | `ee2b1d1 … 2.7.6`; exists |
| plans/2026-08-20-interruption-resume.md + specs/…-design.md | `ls internal/engine/downloader_resume.go`; `grep -c InterruptionTimeout internal/engine/downloader.go` | exists; ≥1 |
| plans/2026-08-29-arc10-twitch-credential-lifecycle.md + specs/…-design.md | `git log --oneline -1 05813d6` | the arc10 merge |
| plans/2026-09-02-a1-linux-process-group-reap.md + specs/…-design.md | `git log --oneline -1 742a2bb` | the A1 merge |
| plans/2026-09-02-arc11-docker-ingest.md + specs/…-design.md | `git log --oneline -1 0b4d0e5` | the arc11 merge |
| plans/2026-09-02-arc12a-tui-cookie-login-and-merge-key.md + specs/…-design.md | `git log --oneline -1 aaca2b8` | the arc12a merge |
| plans/2026-09-02-arc12b-twitch-entitlement-probe.md + specs/…-design.md | `git log --oneline -1 b91bad3` | the arc12b merge |
| plans/2026-09-02-arc12c-acquisition-mode.md + specs/…-design.md | `git log --oneline -1 72eee1b` | the arc12c merge |
| plans/2026-09-03-housekeeping-h1.md + specs/…-design.md | `git log --oneline -1 01a511a` | the H1 merge |
| plans/2026-09-03-housekeeping-h2.md + specs/…-design.md | `git log --oneline -1 30dd1de` | the H2 merge |

Kept on purpose: `plans/2026-08-29-cookie-remediation-field-test-plan.md` (a live field procedure), `plans/2026-09-04-improvement-a-toolchain.md` (this plan, deleted in Task 7), `specs/2026-09-04-improvement-chain-design.md`.

- [ ] **Step 3: Delete them and repoint the two code comments**

```bash
cd D:/Git/Moombox/.worktrees/improvement-a-toolchain
git rm docs/superpowers/plans/2026-05-02-linux-build-support.md \
  docs/superpowers/plans/2026-05-03-twitch-flap-auto-recovery.md \
  docs/superpowers/plans/2026-05-05-cipher-via-ejs-sidecar.md \
  docs/superpowers/plans/2026-06-02-connectivity-detection-redesign.md \
  docs/superpowers/plans/2026-06-26-downloader-activity-indicator.md \
  docs/superpowers/plans/2026-07-16-feed-history-1-store-and-config.md \
  docs/superpowers/plans/2026-07-16-feed-history-2-probe-chain.md \
  docs/superpowers/plans/2026-07-16-feed-history-3-monitor-cycle.md \
  docs/superpowers/plans/2026-07-16-feed-history-4-worker-scheduler.md \
  docs/superpowers/plans/2026-07-16-feed-history-5-backfill.md \
  docs/superpowers/plans/2026-08-20-interruption-resume.md \
  docs/superpowers/plans/2026-08-29-arc10-twitch-credential-lifecycle.md \
  docs/superpowers/plans/2026-09-02-a1-linux-process-group-reap.md \
  docs/superpowers/plans/2026-09-02-arc11-docker-ingest.md \
  docs/superpowers/plans/2026-09-02-arc12a-tui-cookie-login-and-merge-key.md \
  docs/superpowers/plans/2026-09-02-arc12b-twitch-entitlement-probe.md \
  docs/superpowers/plans/2026-09-02-arc12c-acquisition-mode.md \
  docs/superpowers/plans/2026-09-03-housekeeping-h1.md \
  docs/superpowers/plans/2026-09-03-housekeeping-h2.md \
  docs/superpowers/specs/2026-05-02-linux-build-support-design.md \
  docs/superpowers/specs/2026-05-05-cipher-via-ejs-sidecar-design.md \
  docs/superpowers/specs/2026-06-02-connectivity-detection-redesign-design.md \
  docs/superpowers/specs/2026-06-26-downloader-activity-indicator-design.md \
  docs/superpowers/specs/2026-07-07-hls-ffmpeg-reload-pacing-design.md \
  docs/superpowers/specs/2026-07-14-connectivity-detection-hardening-design.md \
  docs/superpowers/specs/2026-07-15-feed-history.md \
  docs/superpowers/specs/2026-08-20-interruption-resume-design.md \
  docs/superpowers/specs/2026-08-29-arc10-twitch-credential-lifecycle-design.md \
  docs/superpowers/specs/2026-09-02-a1-linux-process-group-reap-design.md \
  docs/superpowers/specs/2026-09-02-arc11-docker-ingest-design.md \
  docs/superpowers/specs/2026-09-02-arc12a-tui-cookie-login-and-merge-key-design.md \
  docs/superpowers/specs/2026-09-02-arc12b-twitch-entitlement-probe-design.md \
  docs/superpowers/specs/2026-09-02-arc12c-acquisition-mode-design.md \
  docs/superpowers/specs/2026-09-03-housekeeping-h1-design.md \
  docs/superpowers/specs/2026-09-03-housekeeping-h2-design.md
ls docs/superpowers/plans/ docs/superpowers/specs/
```
Expected listing: plans = `2026-08-29-cookie-remediation-field-test-plan.md`, `2026-09-04-improvement-a-toolchain.md`; specs = `2026-09-04-improvement-chain-design.md`.

`cmd/moombox/services.go:640` and `internal/cipher/solver_composite.go:15` cite `docs/superpowers/specs/2026-05-05-cipher-via-ejs-sidecar-design.md`. Append ` (removed after implementation; read it from git history, e.g. `git show 67755b9:docs/superpowers/specs/2026-05-05-cipher-via-ejs-sidecar-design.md`)` to each citation, keeping the line under 120 columns by wrapping onto a second `//` line if needed. Precedent: `internal/engine/downloader.go:203` already cites a plan that lives only in history.

```bash
cd D:/Git/Moombox/.worktrees/improvement-a-toolchain
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./... && GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/docs/
```
Expected: build silent; `ok internal/docs` (the citation-rot checker is unaffected — it checks symbols and headings in `docs/spec/`, not these paths).

- [ ] **Step 4: Commit**

```bash
cd D:/Git/Moombox/.worktrees/improvement-a-toolchain
git add -A docs/superpowers cmd/moombox/services.go internal/cipher/solver_composite.go
git commit -F - <<'MSG'
docs: remove implemented plan and design docs (git history keeps them)

Nineteen plans and sixteen design specs whose work is merged and released;
each verified against its merge commit or shipped files before removal. The
live field-test plan and the improvement-chain spec stay. Two code comments
now point at git history for the cipher-sidecar design.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Jw2Ne9cGsn3R9bJBNuXudp
MSG
```

- [ ] **Step 5: Reinstall staticcheck for Go 1.27 (local tooling, nothing committed)**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go install honnef.co/go/tools/cmd/staticcheck@latest
cd D:/Git/Moombox/.worktrees/improvement-a-toolchain && staticcheck ./... 2>&1 | grep -v _test.go
```
Expected: exactly the six pre-existing findings (`manager.go:20,25`, `strategies.go:556`, `browse.go:254`, `player_api.go:127,171`) — Arc C removes them; anything NEW is a regression from this arc and must be fixed before Task 7. Record the count in the ledger.

---

### Task 7: Arc close — metrics date, plan removal, full gates

**Files:**
- Modify: `docs/spec/appendix-metrics.md:3`
- Delete: `docs/superpowers/plans/2026-09-04-improvement-a-toolchain.md`

- [ ] **Step 1: Stamp the metrics appendix**

`docs/spec/appendix-metrics.md:3` — `> **Last verified:** 2026-09-03` → `> **Last verified:** 2026-09-04`.

- [ ] **Step 2: Full gates, in this order, in the worktree**

```bash
cd D:/Git/Moombox/.worktrees/improvement-a-toolchain
gofmt -l ./cmd ./internal ./tools
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./... && GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./...
GOOS=linux GOARCH=amd64 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build -o /dev/null ./cmd/moombox
GOOS=linux GOARCH=arm64 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build -o /dev/null ./cmd/moombox
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./... 2>&1 | grep -vE '^ok|no test files'; echo "exit ${PIPESTATUS[0]}"
(cd web/tests && node --test ./*.test.mjs 2>&1 | grep -E '^ℹ (tests|pass|fail)')
```
Expected: gofmt silent; builds silent; `exit 0` with nothing but that line; `tests 92 / pass 92 / fail 0`.

- [ ] **Step 3: Delete this plan and commit**

```bash
cd D:/Git/Moombox/.worktrees/improvement-a-toolchain
git rm docs/superpowers/plans/2026-09-04-improvement-a-toolchain.md
git add docs/spec/appendix-metrics.md
git commit -F - <<'MSG'
chore: Arc A implemented — remove its plan, stamp the metrics appendix

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Jw2Ne9cGsn3R9bJBNuXudp
MSG
git log --oneline main..HEAD
```
Expected: ten commits on the branch (1 floor + 4 deps + 1 goja + 1 Node + 1 qemu/jsdom + 1 docs prune + 1 close).

Append to the ledger: `- <date> Task 7: Arc A gates green (build/vet/gofmt/linux amd64+arm64/28-28/node 92); READY FOR ARC-CLOSE REVIEW.`

---

## After the plan (controller, not the implementer)

1. Fable arc-close review of `main..improvement-a-toolchain` (completeness against spec §3, quality). Fix rounds on the branch.
2. `cd D:/Git/Moombox && git merge --no-ff improvement-a-toolchain -m "Merge improvement-a-toolchain: go 1.27 floor, direct deps, Node v24 sidecar, housekeeping"` (with the trailers), then `git worktree remove .worktrees/improvement-a-toolchain && git branch -d improvement-a-toolchain`.
3. On main: `go run ./tools/fetch-node` (version.txt now says v24 → re-downloads), `cd bgutil-sidecar && npm ci --omit=dev --ignore-scripts && node build.mjs`, then `go build -o moombox.exe ./cmd/moombox`, `go version moombox.exe` → `go1.27.1`, `go run golang.org/x/vuln/cmd/govulncheck@latest ./...` → `No vulnerabilities found`. The owner restarts the running daemon when convenient.
4. Post-merge gates on main (ONE full test). Ledger line. Arc D next.
