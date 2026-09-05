# Arc D — CI Test Workflow and the Notifications Wait Timeout — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every push to `main` and every pull request runs the full Moombox gate set on ubuntu and windows runners, and the notifications package's 30-second real sleep becomes an injectable timeout so the suite loses ~30 s of wall time.

**Architecture:** One new GitHub Actions workflow (`ci.yml`) that mirrors `release.yml`'s embed-blob cache and setup, adds FFmpeg so the two ffmpeg-gated tests run, and runs gofmt/vet/staticcheck/build/test on both OSes plus the node suite and the arm64 cross-build on ubuntu. In `internal/notifications`, `Manager` gains a `waitTimeout` field defaulted to 30 s; `Wait()` reads it (zero → default) and the one slow test sets milliseconds.

**Tech Stack:** GitHub Actions (`actions/checkout@v7`, `actions/cache@v6`, `actions/setup-go@v7`, `actions/setup-node@v7`), Go 1.27 toolchain, staticcheck, actionlint (via `go run`), Node 24, FFmpeg via apt/choco.

**Spec:** `docs/superpowers/specs/2026-09-04-improvement-chain-design.md` §4 (Arc D), §2 (execution model).

## Global Constraints

- Triggers: `push` to `main` and `pull_request`. Matrix `os: [ubuntu-latest, windows-latest]`. `permissions: contents: read`. Concurrency group per ref with cancel-in-progress.
- Steps on BOTH runners, in order: checkout → embed-blob cache (same key family as `release.yml`, plus `runner.os`) → setup-go from `go.mod` → setup-node `'24'` → on cache miss: sidecar payload build + `go run ./tools/fetch-node` → install FFmpeg (`apt-get` / `choco`) → `gofmt -l` must print nothing → `go vet ./...` → staticcheck → `go build ./...` → `go test -count=1 ./...`. ubuntu only: `GOOS=linux GOARCH=arm64 go build`, `npm ci` in `web/tests`, `node --test web/tests/*.test.mjs`.
- **Ruling (controller, 2026-09-04):** the staticcheck step is `continue-on-error: true` in this arc because seven pre-existing findings exist on `main` (spec §6 item 1 removes them and makes the gate hard in Arc C). The step's comment names Arc C. Cost if wrong: an advisory step that should have blocked — for one arc.
- `internal/notifications`: `Manager.waitTimeout time.Duration`; `defaultWaitTimeout = 30 * time.Second`; `NewManager` sets the default; `Wait()` treats `<= 0` as the default (test files build `Manager` literals without the field and call `Wait`); the timed-out Warn log reports the actual duration. `TestManagerWaitTimesOut` sets 50 ms and drops its `testing.Short` skip.
- No production behaviour change other than the log line's text; nothing pushes.
- Worktree: `D:/Git/Moombox/.worktrees/improvement-d-ci` (branch `improvement-d-ci`). Fresh worktrees need the gitignored inputs copied in: `internal/bgutils/embed/{node-windows-amd64.gz,node-linux-amd64.gz,node-linux-arm64.gz,sidecar.tar.gz}` and `internal/cipher/testdata/*.js`. Every `go` command carries `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp`. ONE `go test -count=1 ./...` at a time on the machine.
- Commit trailer on every commit:
  ```
  Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01Jw2Ne9cGsn3R9bJBNuXudp
  ```
- Chain ledger: `D:/Git/Moombox/.superpowers/sdd/2026-09-04-improvement-chain/progress.md` — one dated line per task under "## Arc D".

---

### Task 1: Injectable notifications Wait timeout (TDD)

**Files:**
- Modify: `internal/notifications/manager.go:160-173` (struct), `:305-322` (`NewManager`), `:392-421` (`Wait` + doc comment)
- Test: `internal/notifications/manager_dispatch_test.go:12-60` (rewrite `TestManagerWaitTimesOut`; add `TestManagerWaitTimeoutDefaults`)

**Interfaces:**
- Produces: unexported `Manager.waitTimeout time.Duration`, unexported `const defaultWaitTimeout = 30 * time.Second`, unexported `func (m *Manager) effectiveWaitTimeout() time.Duration`. Nothing exported changes.

- [ ] **Step 1: Confirm the worktree and the baseline package time**

The controller creates the worktree per the Global Constraints recipe. Confirm and measure:

```bash
cd D:/Git/Moombox/.worktrees/improvement-d-ci
git log --oneline -1
ls internal/bgutils/embed/*.gz internal/cipher/testdata/*.js | wc -l
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/notifications/ 2>&1 | tail -1
```
Expected: HEAD is main's tip; `5` files (four blobs + one fixture, or more fixtures); the last line is `ok  github.com/vampiricwulf/Moombox/internal/notifications  3x.xxxs` — record the seconds in your report (baseline; ~31 s).

- [ ] **Step 2: Rewrite the slow test and add the defaults test (RED)**

Replace lines 12–60 of `internal/notifications/manager_dispatch_test.go` (the doc comment through the closing brace of `TestManagerWaitTimesOut`) with:

```go
// TestManagerWaitTimesOut verifies the timeout branch in Wait() fires
// when senders never complete. The sender blocks on a never-closed
// channel so the WaitGroup stays held — the only way to exercise the
// branch, since DiscordWebhook has its own 15s ctx deadline that would
// unblock wg.Wait early. waitTimeout is injected (50 ms) so the test
// costs milliseconds instead of the production 30 s.
// Audit reports/small-packages.md notifications Wait timeout.
func TestManagerWaitTimesOut(t *testing.T) {
	hangForever := make(chan struct{}) // intentionally never closed
	t.Cleanup(func() {
		// The sender goroutine is wedged on a never-closing channel; the
		// test process exits when the test completes and collects it.
		_ = hangForever
	})

	hanging := senderFunc(func(string, string, int, []Field, SendOptions) error {
		<-hangForever
		return nil
	})

	const injected = 50 * time.Millisecond
	m := &Manager{
		logger:      testLogger{},
		semaphore:   make(chan struct{}, maxInflightNotifications),
		targets:     []notificationTarget{{sender: hanging}},
		waitTimeout: injected,
	}
	m.Send("t", "d", TypeInfo, nil, SendOptions{})

	start := time.Now()
	done := make(chan struct{})
	go func() {
		m.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return within 5s — timeout branch did not fire")
	}
	elapsed := time.Since(start)
	if elapsed < injected {
		t.Errorf("Wait returned in %v — before the injected %v timeout", elapsed, injected)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Wait took %v — the injected %v timeout was not honoured", elapsed, injected)
	}
}

// TestManagerWaitTimeoutDefaults pins the production timeout: NewManager
// sets 30 s, and a Manager built without the field (the test literals in
// this package) still waits 30 s rather than zero. Deleting the fallback
// in effectiveWaitTimeout, or changing defaultWaitTimeout, fails this.
func TestManagerWaitTimeoutDefaults(t *testing.T) {
	if defaultWaitTimeout != 30*time.Second {
		t.Fatalf("defaultWaitTimeout = %v, want 30s", defaultWaitTimeout)
	}
	m := NewManager(&config.MoomboxConfig{}, testLogger{})
	if got := m.effectiveWaitTimeout(); got != defaultWaitTimeout {
		t.Errorf("NewManager waitTimeout = %v, want %v", got, defaultWaitTimeout)
	}
	zero := &Manager{logger: testLogger{}}
	if got := zero.effectiveWaitTimeout(); got != defaultWaitTimeout {
		t.Errorf("zero-value Manager effective timeout = %v, want %v", got, defaultWaitTimeout)
	}
	explicit := &Manager{logger: testLogger{}, waitTimeout: 7 * time.Second}
	if got := explicit.effectiveWaitTimeout(); got != 7*time.Second {
		t.Errorf("explicit waitTimeout = %v, want 7s", got)
	}
}
```

The file already imports `config`, `testing`, `time`, `sync`, `sync/atomic`; `config.MoomboxConfig{}` with no notification URLs yields zero targets, which is fine for this test.

- [ ] **Step 3: Run the package tests to verify they fail to compile**

```bash
cd D:/Git/Moombox/.worktrees/improvement-d-ci
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/notifications/ 2>&1 | head -6
```
Expected: `unknown field waitTimeout in struct literal`, `undefined: defaultWaitTimeout`, `m.effectiveWaitTimeout undefined` — a compile failure (`FAIL … [build failed]`).

- [ ] **Step 4: Implement the field, the default, and the fallback (GREEN)**

In `internal/notifications/manager.go`:

Above the `Manager` type (after the `notificationTarget`-related comment block ends and before `// Manager dispatches notifications`), add:
```go
// defaultWaitTimeout bounds Manager.Wait during graceful shutdown. Tests
// inject a shorter waitTimeout; a Manager built without one uses this.
const defaultWaitTimeout = 30 * time.Second
```

In the struct, after `semaphore chan struct{}`, add:
```go
	// waitTimeout bounds Wait; zero means defaultWaitTimeout (test literals
	// omit it). Set once at construction, never written afterwards.
	waitTimeout time.Duration
```

In `NewManager`, the literal becomes:
```go
	m := &Manager{
		logger:      logger,
		semaphore:   make(chan struct{}, maxInflightNotifications),
		targets:     buildTargets(cfg, logger),
		waitTimeout: defaultWaitTimeout,
	}
```

Add the accessor directly above `Wait`:
```go
// effectiveWaitTimeout returns waitTimeout, or defaultWaitTimeout when the
// field was never set (zero or negative).
func (m *Manager) effectiveWaitTimeout() time.Duration {
	if m.waitTimeout <= 0 {
		return defaultWaitTimeout
	}
	return m.waitTimeout
}
```

Change `Wait`'s doc comment first sentence from `or a 30-second timeout expires, whichever comes first.` to `or the wait timeout (defaultWaitTimeout, 30 s, unless injected) expires, whichever comes first.` and its body's select to:
```go
	timeout := m.effectiveWaitTimeout()
	select {
	case <-done:
	case <-time.After(timeout):
		if m.logger != nil {
			m.logger.Warn("notification wait timed out", "after", timeout)
		}
	}
```
(The previous literal `"notification wait timed out after 30s"` is gone; the duration is now a structured attribute.)

- [ ] **Step 5: Run the package tests to verify they pass, and time them**

```bash
cd D:/Git/Moombox/.worktrees/improvement-d-ci
gofmt -l internal/notifications
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -v -run 'TestManagerWait' ./internal/notifications/ 2>&1 | grep -E '^(=== RUN|--- |ok|FAIL)'
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/notifications/ 2>&1 | tail -1
```
Expected: gofmt empty; `--- PASS: TestManagerWaitTimesOut (0.0Xs)` and `--- PASS: TestManagerWaitTimeoutDefaults`; the package line now `ok … 0.x–1.xs` (record it beside the Step 1 baseline).

- [ ] **Step 6: Mutation check (do not commit the mutants)**

Temporarily change `if m.waitTimeout <= 0 {` to `if false {` → run `-run TestManagerWaitTimeoutDefaults` → must FAIL (zero-value case). Revert. Temporarily change `defaultWaitTimeout` to `31 * time.Second` → the same test must FAIL. Revert. Confirm `git diff --stat` shows only the two intended files.

- [ ] **Step 7: Commit**

```bash
cd D:/Git/Moombox/.worktrees/improvement-d-ci
git add internal/notifications/manager.go internal/notifications/manager_dispatch_test.go
git commit -F - <<'MSG'
perf(notifications): injectable Wait timeout — the 30 s test sleep is now 50 ms

Manager.waitTimeout (default 30 s via NewManager; zero falls back to the
default so test literals keep production semantics) replaces the literal in
Wait. TestManagerWaitTimesOut injects 50 ms and loses its -short skip;
TestManagerWaitTimeoutDefaults pins the default and the fallback. Package
time <baseline>s → <after>s.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Jw2Ne9cGsn3R9bJBNuXudp
MSG
```
Fill in the two measured times. Ledger line under "## Arc D": `- <date> Task 1: notifications waitTimeout injected (<sha>); package <baseline>s → <after>s.`

---

### Task 2: The CI workflow

**Files:**
- Create: `.github/workflows/ci.yml`

**Interfaces:**
- Consumes: the embed-blob cache key shape from `.github/workflows/release.yml:41-55`; the sidecar build commands from `release.yml:71-79`; `tools/fetch-node`.
- Produces: a workflow named `CI` whose job id is `test`; Task 3's docs describe exactly these steps.

- [ ] **Step 1: Write the workflow**

Create `.github/workflows/ci.yml` with exactly this content:

```yaml
name: CI

# Build, lint, and test on every push to main and every pull request, on
# both desktop platforms Moombox ships for. release.yml only runs on tag
# push and never runs the tests; before this workflow existed the suite
# ran only on developer machines.
#
# Shape:
#   - matrix over ubuntu-latest + windows-latest (the Windows-only code —
#     DPAPI cookie reading, Job Objects, cookie profile paths — has tests
#     that skip everywhere else)
#   - the same embed-blob cache as release.yml (plus runner.os, because the
#     sidecar tarball is built per runner); on a miss the sidecar payload
#     and the pinned Node binaries are rebuilt, ~1 min
#   - FFmpeg installed so muxer_concatcopy_test.go and probe_params_test.go
#     stop skipping
#   - gofmt / vet / staticcheck / build / full `go test`; ubuntu also
#     cross-builds linux/arm64 and runs the node suite in web/tests
#
# staticcheck is ADVISORY (continue-on-error) until Arc C of the 2026-09-04
# improvement chain removes the seven pre-existing findings; Arc C flips it
# to a hard gate.

on:
  push:
    branches: [main]
  pull_request:

permissions:
  contents: read

concurrency:
  group: ci-${{ github.ref }}
  cancel-in-progress: true

defaults:
  run:
    shell: bash

jobs:
  test:
    name: test (${{ matrix.os }})
    runs-on: ${{ matrix.os }}
    timeout-minutes: 45
    strategy:
      fail-fast: false
      matrix:
        os: [ubuntu-latest, windows-latest]
    steps:
      - uses: actions/checkout@v7

      - name: Restore embed blob cache
        # Same inputs as release.yml's key, plus the runner OS: the three
        # Node binaries are identical everywhere but the sidecar tarball is
        # produced by the runner's own tar.
        id: cache-embed
        uses: actions/cache@v6
        with:
          path: |
            internal/bgutils/embed/sidecar.tar.gz
            internal/bgutils/embed/node-windows-amd64.gz
            internal/bgutils/embed/node-linux-amd64.gz
            internal/bgutils/embed/node-linux-arm64.gz
          key: embed-${{ runner.os }}-${{ hashFiles('internal/bgutils/embed/version.txt', 'bgutil-sidecar/package-lock.json', 'bgutil-sidecar/build.mjs', 'tools/fetch-node/main.go') }}

      - uses: actions/setup-go@v7
        with:
          go-version-file: go.mod

      - uses: actions/setup-node@v7
        with:
          node-version: '24'

      - name: Build BotGuard sidecar payload
        # --ignore-scripts: a compromised dep's postinstall could exfiltrate
        # GITHUB_TOKEN; jsdom + bgutils-js have no install scripts we need.
        if: steps.cache-embed.outputs.cache-hit != 'true'
        working-directory: bgutil-sidecar
        run: |
          npm ci --no-audit --no-fund --ignore-scripts
          node build.mjs

      - name: Fetch embedded Node binaries (all platforms)
        if: steps.cache-embed.outputs.cache-hit != 'true'
        run: go run ./tools/fetch-node

      - name: Install FFmpeg (ubuntu)
        if: runner.os == 'Linux'
        run: |
          sudo apt-get update -qq
          sudo apt-get install -y -qq ffmpeg
          ffmpeg -version | head -1

      - name: Install FFmpeg (windows)
        if: runner.os == 'Windows'
        run: |
          choco install ffmpeg -y --no-progress
          ffmpeg -version | head -1

      - name: gofmt
        run: |
          out="$(gofmt -l ./cmd ./internal ./tools)"
          if [ -n "$out" ]; then
            echo "gofmt would reformat:"; echo "$out"; exit 1
          fi

      - name: go vet
        run: go vet ./...

      - name: staticcheck (advisory until Arc C)
        continue-on-error: true
        run: |
          go install honnef.co/go/tools/cmd/staticcheck@latest
          staticcheck ./...

      - name: go build
        run: go build ./...

      - name: go test
        run: go test -count=1 ./...

      - name: Cross-build linux/arm64
        if: runner.os == 'Linux'
        env:
          CGO_ENABLED: '0'
          GOOS: linux
          GOARCH: arm64
        run: go build -o /dev/null ./cmd/moombox

      - name: Frontend test suite
        if: runner.os == 'Linux'
        working-directory: web/tests
        run: |
          npm ci --no-audit --no-fund
          node --test ./*.test.mjs
```

- [ ] **Step 2: Lint the workflow with actionlint**

```bash
cd D:/Git/Moombox/.worktrees/improvement-d-ci
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go run github.com/rhysd/actionlint/cmd/actionlint@latest .github/workflows/ci.yml .github/workflows/release.yml .github/workflows/docker-publish.yml
echo "actionlint exit: $?"
```
Expected: no findings for `ci.yml`; exit 0. (If `release.yml`/`docker-publish.yml` produce pre-existing findings, list them in the report and do not touch those files — the exit code is then non-zero; re-run on `ci.yml` alone and require exit 0 there.) The `go run` downloads actionlint's module once (network).

- [ ] **Step 3: Dry-run the gofmt step's shell locally**

The gofmt step is the only step with logic. Run its body verbatim from the worktree root:
```bash
cd D:/Git/Moombox/.worktrees/improvement-d-ci
out="$(gofmt -l ./cmd ./internal ./tools)"; if [ -n "$out" ]; then echo "gofmt would reformat:"; echo "$out"; exit 1; fi; echo "gofmt step OK"
```
Expected: `gofmt step OK`.

- [ ] **Step 4: Commit**

```bash
cd D:/Git/Moombox/.worktrees/improvement-d-ci
git add .github/workflows/ci.yml
git commit -F - <<'MSG'
ci: build, lint, and test on push and pull request, ubuntu + windows

release.yml runs only on tag push and never tests. ci.yml mirrors its
embed-blob cache and setup, installs FFmpeg so the two ffmpeg-gated tests
run, then gofmt/vet/staticcheck/build/test on both runners; ubuntu also
cross-builds linux/arm64 and runs the node suite. staticcheck is advisory
until Arc C removes the seven pre-existing findings.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Jw2Ne9cGsn3R9bJBNuXudp
MSG
```
Ledger line: `- <date> Task 2: ci.yml added (<sha>); actionlint clean.`

---

### Task 3: Docs — the CI section and CLAUDE.md

**Files:**
- Modify: `docs/spec/operations.md:181-210` (§CI Pipeline: retitle the release subsection, fix four action versions, add a Test Workflow subsection)
- Modify: `CLAUDE.md:24` (the Build & Test paragraph)

- [ ] **Step 1: Retitle and correct the release-workflow subsection**

In `docs/spec/operations.md`:
- `:183` `### Workflow` → `### Release Workflow`
- `:192` `1. **Checkout** — `actions/checkout@v6`` → `actions/checkout@v7`
- `:193` `actions/cache@v5` → `actions/cache@v6` (only the version token on that line)
- `:194` `actions/setup-go@v6` → `actions/setup-go@v7`
- `:195` `actions/setup-node@v6` → `actions/setup-node@v7`

Confirm against `.github/workflows/release.yml` (`grep -n 'uses:' .github/workflows/release.yml`) that those are the four versions in use.

- [ ] **Step 2: Add the Test Workflow subsection**

Insert immediately before `### Release Body Format` (`operations.md:211`):

```markdown
### Test Workflow

**File:** `.github/workflows/ci.yml`
**Trigger:** push to `main`, every pull request
**Runners:** `ubuntu-latest` and `windows-latest` (matrix, `fail-fast: false`) — the Windows-only code paths (DPAPI cookie reading, Job Objects, cookie profile paths) have tests that skip everywhere else
**Permissions:** `contents: read`; one run per ref (`concurrency` with cancel-in-progress); 45-minute job timeout

Steps, on both runners: checkout → the release workflow's embed-blob cache (key also carries `runner.os`, since the sidecar tarball is produced by the runner's own `tar`) → `setup-go` from `go.mod` → `setup-node` 24 → on a cache miss, the sidecar payload build and `go run ./tools/fetch-node` → FFmpeg (`apt-get` on ubuntu, `choco` on windows) so `muxer_concatcopy_test.go` and `probe_params_test.go` run instead of skipping → `gofmt -l` must print nothing → `go vet ./...` → `staticcheck ./...` → `go build ./...` → `go test -count=1 ./...`. ubuntu additionally cross-builds `linux/arm64` and runs the frontend suite (`npm ci` in `web/tests`, `node --test web/tests/*.test.mjs`).

`staticcheck` is advisory (`continue-on-error`) until Arc C of the 2026-09-04 improvement chain removes the seven pre-existing findings and flips it to a hard gate. The live gates (`MOOMBOX_LIVE_*`) never run in CI: they need YouTube and Twitch.
```

- [ ] **Step 3: CLAUDE.md**

`CLAUDE.md:24` currently: `Runtime requires FFmpeg on PATH. CI (`.github/workflows/release.yml`) cross-compiles all 3 platform binaries (Windows x64, Linux x64, Linux arm64) from a single ubuntu-latest job on tag push, reads `RELEASE_NOTES.md` for GitHub release body.`

Replace with: `Runtime requires FFmpeg on PATH. CI: `.github/workflows/ci.yml` runs gofmt/vet/staticcheck/build/`go test ./...` on ubuntu + windows for every push to main and every PR (ubuntu also cross-builds linux/arm64 and runs `node --test web/tests/*.test.mjs`); `.github/workflows/release.yml` cross-compiles all 3 platform binaries (Windows x64, Linux x64, Linux arm64) from a single ubuntu-latest job on tag push and reads `RELEASE_NOTES.md` for the GitHub release body.`

- [ ] **Step 4: The citation-rot checker and commit**

```bash
cd D:/Git/Moombox/.worktrees/improvement-d-ci
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/docs/
git add docs/spec/operations.md CLAUDE.md
git commit -F - <<'MSG'
docs(operations): the CI test workflow; release-workflow action versions at v7/v6/v7/v7

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Jw2Ne9cGsn3R9bJBNuXudp
MSG
```
Expected: `ok internal/docs`. Ledger line: `- <date> Task 3: operations.md Test Workflow section + action versions; CLAUDE.md CI line (<sha>).`

---

### Task 4: Arc close — full gates, plan removal

**Files:**
- Delete: `docs/superpowers/plans/2026-09-04-improvement-d-ci.md`

- [ ] **Step 1: Full gates in the worktree**

```bash
cd D:/Git/Moombox/.worktrees/improvement-d-ci
gofmt -l ./cmd ./internal ./tools
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./... && GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./...
GOOS=linux GOARCH=amd64 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build -o /dev/null ./cmd/moombox
GOOS=linux GOARCH=arm64 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build -o /dev/null ./cmd/moombox
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./... > /tmp/arc-d-gate.txt 2>&1; echo "exit $?"; grep -cE '^ok' /tmp/arc-d-gate.txt; grep -E '^(FAIL|---)' /tmp/arc-d-gate.txt | head; grep -E 'internal/notifications' /tmp/arc-d-gate.txt
```
Expected: gofmt empty; builds silent; `exit 0`; `28`; no FAIL lines; the notifications line shows ~1 s. Paste the transcript file's tail into the report (the LITERAL output — never paraphrase test output).

- [ ] **Step 2: Delete this plan and commit**

```bash
cd D:/Git/Moombox/.worktrees/improvement-d-ci
git rm docs/superpowers/plans/2026-09-04-improvement-d-ci.md
git commit -F - <<'MSG'
chore: Arc D implemented — remove its plan

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Jw2Ne9cGsn3R9bJBNuXudp
MSG
git log --oneline main..HEAD
```
Expected: four commits. Ledger line: `- <date> Task 4: Arc D gates green (full suite 28/28, notifications ~1 s); READY FOR ARC-CLOSE REVIEW.`

---

## After the plan (controller)

1. Fable arc-close review of `main..improvement-d-ci`; fix rounds on the branch.
2. Ask the owner; then `git merge --no-ff improvement-d-ci -F <msg-file>` (never `-F -`), delete worktree and branch, post-merge gates on main (ONE full test).
3. The workflow first runs when the owner next pushes `main`; watch that run (`gh run list --workflow ci.yml`) — the Windows leg is the one with no prior evidence. Arc B next.
