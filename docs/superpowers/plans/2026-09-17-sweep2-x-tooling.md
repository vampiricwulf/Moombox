# Arc X — Tooling, Dependencies, CI, Docs, Utils — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the twelve cross-cutting sweep-2 rows that belong to no single subsystem — the dependency pins, the CI gates, the repository line-ending and ignore rules, two shared `internal/utils` helpers, the seventeen production `modernize` sites, and the doc rot in `SPEC.md`, `CLAUDE.md` and `docs/spec/appendix-metrics.md` — so the chain ends with the toolchain, the gates and the standalone reference docs all telling the truth.

**Architecture:** Arc X is the last of seven arcs and runs **alone in wave 3**, cut from `main` only after Arcs E, Y, T, M, W and C have all merged. That ordering is the whole point: this arc edits files the other six own (`internal/tui/settings.go`, `internal/web/middleware.go`, `internal/web/routes/*.go`, `internal/utils/*`, `SPEC.md`), and it regenerates metrics and re-runs an analyzer over a tree the other six have just reshaped. Nothing here may be planned against a line number — every step below locates its target by text or symbol, and the three enumerating tasks (appendix regeneration, `modernize` site list, widened citation test) **run their enumeration at execution time** and act on what comes back, not on numbers frozen into this document.

**Tech Stack:** Go 1.27 (no CGo, pure-Go deps), `modernc.org/sqlite`, `dop251/goja`, GitHub Actions (ubuntu-latest + windows-latest), Node 24 (`bgutil-sidecar` + `web/tests`), Git Bash on Windows for every command below.

**Spec:** `docs/superpowers/specs/2026-09-17-sweep2-fix-chain-design.md` (§0 decisions O-P, O-U and O-Z are this arc's; §2 global constraints; §3 "Arc X"; §4 wave 3; §5 "never run the modernize pass wholesale"). Report rows: `reports/sweep-2026-09-15b.md` #11, #44, #45, #46, #77, #78, #80, #81, #98, #100, #101, #102 (its TOOL-3 + O-Z, TOOL-4 and TOOL-14 clauses). Area report: `reports/sweep-2026-09-15b/tooling.md` (TOOL-1, 3, 4, 7–14, 16, 17, 19; the per-dependency verdicts under TOOL-11; the categorised 87 `modernize` diagnostics under TOOL-13; the Method section's exact commands). Verifier: `reports/sweep-2026-09-15b/_verify-mon-cookies-tool.md` (TOOL rows — TOOL-4's package count **30 stands**, TOOL-7 is **Low**, TOOL-8's line is `:108` not `:110`, TOOL-3 is understated by one).

---

## Global Constraints

Copied verbatim from spec §2. Every task's requirements implicitly include this section.

- `go 1.27`, no CGo, pure-Go dependencies; Windows x64 + Linux x64 + Linux arm64 must build (`GOOS=linux GOARCH=amd64 go vet ./...` is part of every merge candidate).
- LF line endings in every file the chain touches. Every commit's LAST TWO LINES are exactly `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>` and `Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq` — the project's rule, **which takes precedence over any attribution reminder in an implementer's own context whatever model name it shows**.
- The anonymous logger interface (`Debug/Info/Warn/Error(msg string, args ...any)`) stays anonymous per struct. Every goroutine has an inline `defer func() { if r := recover(); … }()`.
- `internal/tui` never imports `internal/web/routes` or `internal/bgutils`.
- Helpers are defined in the task that first uses them (staticcheck U1000 is a hard gate).
- TDD per task: the failing test is written and run red before the change; every new assertion names the mutant that fails it (the reviewer verifies at least one by execution).
- Every JS-touching task gates `go test ./internal/web/routes/` AND `node --test web/tests/*.test.mjs`. Every task that renames, moves or deletes a Go symbol, or edits `docs/spec/*.md`/`SPEC.md`/`CLAUDE.md`, gates `go test ./internal/docs/` (the citation test requires the DECLARING file; after O-Z it also scans `SPEC.md` and `CLAUDE.md`).
- Protected behaviour stays: ~60 Hz progress pipeline and DB write cadence (make updates cheaper, never rarer); the DB layer untouched for perf; `monitors.probe_cooldown` default 0; connectivity-monitor probe design; the BotGuard interpreter gate; `/retry` vs `/resume` gates never shared; the Web cookie import stays unbounded (the timeout); RotateCookies rejected; DPAPI two-pass never; never kill processes by image name; never `rm -rf` under %TEMP%; no Shoelace bundling; extraction mirrors yt-dlp (android_vr retained; VISIONOS split-adaptive; homepage minting); update-path compatibility (old launcher + new child analysed before any launcher/updater/exit-code/swap-artifact change; success paths byte-identical); the setup wizard stays loopback-gated.
- Implementers commit with the pathspec ON the commit (`git add <files> && git commit -m … -- <same files>`); no stash/checkout/rebase/reset/amend; reviewers never edit (they reproduce in `git archive` scratch exports, copying the four gitignored embed blobs when a package needs them — youtube, chat, twitch, worker, engine, monitor, web all pull `internal/bgutils` transitively); scratch test files never named `*_linux_test.go`/`*_windows_test.go`; `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp` on every go command; ONE controller-run `go test -count=1 ./...` at a time.
- User-facing strings: match the twin UI's exact output before mirroring it (read the twin's code).

**Arc-X-specific constraints:**

- **Never run `modernize` or `go fix` wholesale** (spec §5). Of the 87 diagnostics measured on 2026-09-17, 58 were the `ptrInt`/`intPtr`/`ptr`/`ptrBool`/`boolPtr`/`ptrInt64` → `new(expr)` class that the standing memory rule says breaks gopls and has to be reverted afterwards. The analyzer is run **read-only, never with `-fix`**, and only the filtered production set is applied by hand.
- **The goja BotGuard live test (`TestBotGuardLiveFingerprint`) is a long-standing known failure, not a gate** and not a regression (O-U). The gates for the goja bump are the cipher live gate, the **sidecar** live gate and `TestSidecarSolverGojaParity`.
- All line/row numbers quoted from the reports are as of `main` @ `710904bf`. **Six arcs merge before this one executes — every one of them will have drifted.** Locate by the quoted text or symbol, never by the number.

---

## Branch and worktree

Cut **after all six other arcs have merged to `main`** (spec §4: wave 3, X alone). Confirm before cutting:

```bash
cd /d/Git/Moombox
git log --oneline -12 main            # expect merge commits for sweep2-e/y/t/m/w/c
git worktree list                     # expect no leftover sweep2-* worktrees
```

Worktree recipe (spec §2):

```bash
cd /d/Git/Moombox
git worktree add -b sweep2-x-tooling .worktrees/sweep2-x-tooling main
cp internal/bgutils/embed/node-windows-amd64.gz \
   internal/bgutils/embed/node-linux-amd64.gz \
   internal/bgutils/embed/node-linux-arm64.gz \
   internal/bgutils/embed/sidecar.tar.gz \
   .worktrees/sweep2-x-tooling/internal/bgutils/embed/
cp internal/cipher/testdata/*.js .worktrees/sweep2-x-tooling/internal/cipher/testdata/
cd .worktrees/sweep2-x-tooling/web/tests && npm ci --no-audit --no-fund
```

This arc also needs the sidecar's own `node_modules` for Task 2's local verification:

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling/bgutil-sidecar && npm ci --no-audit --no-fund --ignore-scripts
```

Every `go` command below is prefixed `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp`. Every path below is relative to the worktree root `D:/Git/Moombox/.worktrees/sweep2-x-tooling`.

---

## File Structure

| File | Task | Responsibility after this arc |
|---|---|---|
| `go.mod`, `go.sum` | 1, 3 | Direct pins: sqlite v1.59.0 (whose own go.mod pins libc v1.75.7 exactly), x/text v0.42.0, go-runewidth v0.0.30, goja @20260915173639; x/crypto + x/sync carried for MVS tidiness. LF, pinned by `.gitattributes`. |
| `.github/workflows/ci.yml` | 2, 3 | Adds: npm cache over both lockfiles, sidecar `npm test` (Linux), `go test -race` over logger + database + web (Linux), the libc-pin gate, the `go mod tidy -diff` gate. |
| `.github/workflows/release.yml` | 2 | go-winres pinned to a released tag; npm cache on its conditional `setup-node`. |
| `.gitattributes` | 3 | `go.mod`/`go.sum` pinned `text eol=lf` so `go mod tidy -diff` is a usable gate. |
| `.gitignore` | 3 | `.claude/*` + `!.claude/skills/` so a newly authored project skill shows in `git status`. |
| `internal/utils/http.go` (+`_test.go`) | 4 | `FetchBody` errors past the 50 MB ceiling instead of returning a silently truncated body. |
| `internal/utils/writefile.go` (+`_test.go`) — **new** | 5 | `utils.WriteFileAtomic(path, data, perm)`: `os.CreateTemp` + write + fsync + chmod + `utils.ReplaceFile`. One home for the pattern three packages had drifted copies of. |
| `internal/cipher/player_cache.go` | 5 | Its `atomicWrite` delegates to `utils.WriteFileAtomic` — gains the unique temp name, the fsync and the Windows rename retry it was missing. |
| the filtered production `modernize` sites | 6 | Enumerated at execution; on 2026-09-17 that was 14 sites in 9 files (4 × `strings.SplitSeq`, 2 × `min`, 3 × `slices.Backward`, 4 × `errors.AsType`, 1 × range-over-int). |
| `internal/docs/citations_test.go` | 7 | `specDocs` widened to `SPEC.md` + `CLAUDE.md`; a `docPath` helper resolves the two root docs. |
| `SPEC.md`, `CLAUDE.md` | 7 | Four invented-symbol passages and one wrong path corrected, then mechanically guarded. |
| `docs/spec/appendix-metrics.md` | 8 | Regenerated Package Scale + Totals + Key Dependencies; package count stays **30**. |
| `docs/superpowers/plans/2026-09-17-sweep2-x-tooling.md` | 9 | Deleted — git history is the archive. |

---

### Task 1: Dependency bumps — sqlite/libc pin, x/text, go-runewidth, goja

Rows #11 (TOOL-1) and #81 (TOOL-11), owner decision **O-U**. `modernc.org/sqlite`'s own `doc.go` requires a downstream module to pin **the exact** `modernc.org/libc` version its go.mod names (gitlab.com/cznic/sqlite#177); a past `go get -u` sweep left us one patch ahead of what sqlite v1.58.0 pinned. v1.59.0 pins v1.75.7 exactly — the version we already carry — so the bump closes the gap from the other side.

**Files:**
- Modify: `go.mod`
- Modify: `go.sum`

**Interfaces:**
- Consumes: nothing from earlier tasks (this is the first task).
- Produces: `modernc.org/sqlite v1.59.0`, `golang.org/x/text v0.42.0`, `github.com/mattn/go-runewidth v0.0.30`, `github.com/dop251/goja v0.0.0-20260915173639-b3fa02110dbd`, `golang.org/x/crypto v0.57.0`, `golang.org/x/sync v0.23.0`. Task 2 adds a CI gate that asserts the libc invariant this task restores. Task 8 copies the resulting versions into `docs/spec/appendix-metrics.md`'s Key Dependencies table.

- [ ] **Step 1: Run the libc-pin check and see it RED**

This is the failing check this task exists to turn green. It compares the `modernc.org/libc` version our build selects against the version `modernc.org/sqlite`'s own `go.mod` pins:

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
ours=$(go list -m -f '{{.Version}}' modernc.org/libc)
want=$(sed -n 's#^[[:space:]]*modernc.org/libc \(v[^ ]*\).*#\1#p' "$(go list -m -f '{{.GoMod}}' modernc.org/sqlite)")
echo "ours=$ours want=$want"
[ "$ours" = "$want" ] && echo OK || echo "MISMATCH"
```

Expected (measured 2026-09-17 on `710904bf`): `ours=v1.75.7 want=v1.75.6` → `MISMATCH`.

- [ ] **Step 2: Bump the four dependencies O-U names, plus the two MVS-tidiness ones**

One `go get` so MVS resolves the whole set at once:

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go get modernc.org/sqlite@v1.59.0 \
       golang.org/x/text@v0.42.0 \
       github.com/mattn/go-runewidth@v0.0.30 \
       github.com/dop251/goja@v0.0.0-20260915173639-b3fa02110dbd \
       golang.org/x/crypto@v0.57.0 \
       golang.org/x/sync@v0.23.0
go mod tidy
```

Why each (TOOL-11's module-cache source diffs, verbatim verdicts):
- **sqlite v1.59.0** — pins libc v1.75.7 exactly; the transpiled SQLite is unchanged (3.53.4, byte-identical on all 20 targets); libc v1.75.7 replaces transpiled musl `memcpy`/`memmove`/`memset`/`memcmp`/`strcspn`/`fabs`/`strlen` with native Go.
- **x/text v0.42.0** — two real `unicode/norm` composition fixes on code we use (`norm.NFD` at `internal/monitor/utils.go` and `internal/utils/text.go`).
- **go-runewidth v0.0.30** — joiner bitmap + ASCII fast path in `StringWidth`/`Wrap`, on the TUI's 60 Hz render path.
- **goja @20260915173639** — a genuine `new`-expression stack-base fix, plus a unicode-mode `\c` control escape that now errors instead of being written literally. That second one is the behaviour-change risk, and it is confined to the **goja fallback** path (sidecar is primary). Hence the live gates in Step 5.
- **x/crypto v0.57.0, x/sync v0.23.0** — pure churn for us (`scrypt` and errgroup/singleflight are untouched); taken only so MVS does not carry two different minor versions.

Do **not** take the seven indirect-only updates (ultraviolet, x/exp/slice, regexp2/v2, pprof, x/net, x/sys, xo/terminfo) — O-U does not name them, and `go mod tidy` will not move them on its own.

- [ ] **Step 3: Run the libc-pin check again and see it GREEN**

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
ours=$(go list -m -f '{{.Version}}' modernc.org/libc)
want=$(sed -n 's#^[[:space:]]*modernc.org/libc \(v[^ ]*\).*#\1#p' "$(go list -m -f '{{.GoMod}}' modernc.org/sqlite)")
echo "ours=$ours want=$want"
[ "$ours" = "$want" ] && echo OK || { echo "MISMATCH"; exit 1; }
```

Expected: `ours=v1.75.7 want=v1.75.7` → `OK`. If it is not OK, do **not** hand-edit the `// indirect` line — `go get modernc.org/libc@<want>` and re-tidy, because hand-editing leaves `go mod tidy -diff` (Task 3's gate) failing.

- [ ] **Step 4: Build all three shipped targets**

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go build ./...
go vet ./...
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go vet ./...
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /dev/null ./cmd/moombox
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o /dev/null ./cmd/moombox
```

Expected: all silent, exit 0. (The embed blobs copied into the worktree are what make `go build ./...` possible at all.)

- [ ] **Step 5: Run the packages that actually consume the bumped modules**

Single packages only — the one full-suite run belongs to the controller at the merge candidate.

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go test -count=1 ./internal/database/     # modernc.org/sqlite
go test -count=1 ./internal/monitor/      # x/text norm.NFD
go test -count=1 ./internal/utils/        # x/text norm.NFD
go test -count=1 ./internal/tui/          # go-runewidth
go test -count=1 ./internal/cipher/       # goja fallback solver
go test -count=1 ./internal/goja/         # goja runtime shims
go test -count=1 ./internal/bgutils/      # goja BotGuard fallback
```

Expected: `ok` for each.

- [ ] **Step 6: Run the O-U live gates**

These need network and the embed blobs. Run them from the worktree:

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
MOOMBOX_LIVE_CIPHER_TEST=1 go test -count=1 -timeout 180s -run "TestSidecarSolver" ./internal/cipher/...
MOOMBOX_LIVE_BG_TEST=1 go test -count=1 -timeout 180s -run "TestSidecarLive" ./internal/bgutils/...
```

The first command is the one `.claude/skills/moombox-upstream-porting/SKILL.md` names for a goja/cipher change, and it covers **both** O-U cipher gates: `TestSidecarSolverLive` and `TestSidecarSolverGojaParity` (both in `internal/cipher/solver_sidecar_live_test.go`). The second is the sidecar live gate — `TestSidecarLivePoToken` and `TestSidecarLiveGvsMint` in `internal/bgutils/sidecar/sidecar_live_test.go`.

Expected: PASS on all four. **`TestBotGuardLiveFingerprint` (the goja BotGuard live test) is deliberately excluded from the `-run` pattern**: it is a long-standing known failure and explicitly not a gate (O-U, and the standing memory rule). If you run the skill's fuller pattern `-run "TestBotGuardLive\|TestSidecarLive"` and see `TestBotGuardLiveFingerprint` fail, that is the known state, not a regression from this bump — confirm by checking it fails identically on `main` before the bump.

If `TestSidecarSolverGojaParity` fails, that is the `\c`-escape risk landing: capture the failing fixture, report it, and do **not** work around it by weakening the parity assertion.

- [ ] **Step 7: Commit**

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
git add go.mod go.sum
git commit -m "chore(deps): sqlite v1.59.0 restores the exact libc pin; x/text, go-runewidth, goja

modernc.org/sqlite's doc.go requires downstream modules to pin the exact
modernc.org/libc version its own go.mod names (gitlab.com/cznic/sqlite#177).
A past 'go get -u' sweep left us on libc v1.75.7 while sqlite v1.58.0 pinned
v1.75.6; v1.59.0 pins v1.75.7, closing the gap from the driver's side with
the transpiled SQLite byte-identical at 3.53.4.

x/text v0.42.0 carries two real unicode/norm composition fixes on the
norm.NFD calls in internal/monitor and internal/utils; go-runewidth v0.0.30
adds a joiner bitmap and an ASCII fast path on the TUI's render path; goja
@20260915173639 fixes a 'new'-expression stack base. x/crypto and x/sync are
churn for us and are carried only for MVS tidiness.

Gated by the three builds, the consuming packages, MOOMBOX_LIVE_CIPHER_TEST
(TestSidecarSolverLive + TestSidecarSolverGojaParity) and MOOMBOX_LIVE_BG_TEST
(TestSidecarLive*). The goja BotGuard live test is a known failure, not a gate.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- go.mod go.sum
```

---

### Task 2: CI gates — pin go-winres, run the sidecar tests, run `-race`, cache npm, guard the libc pin

Rows #44 (TOOL-8), #45 (TOOL-10), #46 (TOOL-9 + **O-P**), #77 (TOOL-16), plus the mechanical guard for Task 1's invariant.

There is no unit test for a workflow file. The TDD discipline here is **run every command this task adds, locally, and observe the claimed result before writing it into the YAML** — each step below does exactly that.

**Files:**
- Modify: `.github/workflows/ci.yml`
- Modify: `.github/workflows/release.yml`

**Interfaces:**
- Consumes: Task 1's `go.mod`/`go.sum` (the libc gate asserts Task 1's invariant; it would fail on the pre-Task-1 tree).
- Produces: five new CI steps. Task 3 adds a sixth (`go mod tidy -diff`) to the same file, immediately after the `gofmt` step — Task 3's edit and this task's edits are in different steps of the same job and do not overlap.

- [ ] **Step 1: Prove the `-race` command runs and passes locally**

O-P names the scope exactly: `./internal/logger/... ./internal/database/... ./internal/web/...`.

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go test -race -count=1 ./internal/logger/... ./internal/database/... ./internal/web/...
```

Expected: `ok` for `internal/logger`, `internal/database`, `internal/web` and `internal/web/routes`, with no `WARNING: DATA RACE`. (Measured 2026-09-17: `internal/logger` alone passes under `-race` in 1.44 s on this Windows host, so the detector is available locally too — but the CI step is Linux-only per O-P; the Windows leg is unchanged.)

This is the first time anything has actually checked `internal/logger/logger_stress_test.go`'s "The race detector should not flag any access to `l.file` / `l.currentSize`" — and the nine other test files across `internal/config`, `internal/cookies`, `internal/database`, `internal/monitor` and `internal/twitch` that carry "run under `-race`" instructions. If a race IS reported, **stop and report it**: that is a real finding, not a reason to narrow the step.

- [ ] **Step 2: Prove the sidecar `npm test` runs and passes locally**

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling/bgutil-sidecar
[ -d node_modules ] || npm ci --no-audit --no-fund --ignore-scripts
npm test
```

Expected: `node --test --test-force-exit test/minter-keying.test.mjs test/homepage-extract.test.mjs` runs and reports `pass` with `fail 0`. These two files cover the homepage-ytcfg extraction and the minter keying behind the open premiere-attestation field gate, and `grep -rn "npm test\|npm run test" .github/workflows/` returns nothing today.

- [ ] **Step 3: Prove the libc-pin gate script fails on a mismatch and passes on a match**

Run it against the current (post-Task-1) tree — expect PASS:

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
ours=$(go list -m -f '{{.Version}}' modernc.org/libc)
want=$(sed -n 's#^[[:space:]]*modernc.org/libc \(v[^ ]*\).*#\1#p' "$(go list -m -f '{{.GoMod}}' modernc.org/sqlite)")
if [ "$ours" != "$want" ]; then echo "libc is $ours but sqlite pins $want"; exit 1; fi
echo "libc pin OK: $ours"
```

Expected: `libc pin OK: v1.75.7`, exit 0.

Then prove it is not vacuous, in a scratch copy only (never in the worktree):

```bash
SC="C:/Users/Wulf/AppData/Local/Temp/claude/D--Git-Moombox/<session>/scratchpad/libcgate"
rm -rf "$SC"; mkdir -p "$SC"
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling && git archive HEAD | tar -x -C "$SC"
cd "$SC" && export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go mod edit -require=modernc.org/sqlite@v1.58.0
ours=$(go list -m -f '{{.Version}}' modernc.org/libc)
want=$(sed -n 's#^[[:space:]]*modernc.org/libc \(v[^ ]*\).*#\1#p' "$(go list -m -f '{{.GoMod}}' modernc.org/sqlite)")
if [ "$ours" != "$want" ]; then echo "RED as expected: libc is $ours but sqlite pins $want"; else echo "GATE IS VACUOUS"; fi
```

Expected: `RED as expected: libc is v1.75.7 but sqlite pins v1.75.6`. That is the mutant the gate kills.

- [ ] **Step 4: Add `cache: npm` to `ci.yml`'s `setup-node`**

Locate the step in `.github/workflows/ci.yml` by its text — the `- uses: actions/setup-node@v7` block with `node-version: '24'` (the only one in the file) — and replace it with:

```yaml
      - uses: actions/setup-node@v7
        with:
          node-version: '24'
          # Both npm projects in the repo: the sidecar payload built on a
          # cache miss, and web/tests' node suite whose `npm ci` runs on
          # EVERY Linux run and is covered by no other cache key (the
          # embed-blob cache keys off bgutil-sidecar/package-lock.json).
          cache: npm
          cache-dependency-path: |
            bgutil-sidecar/package-lock.json
            web/tests/package-lock.json
```

- [ ] **Step 5: Add the sidecar test suite step to `ci.yml`'s Linux leg**

Insert immediately **before** the existing `- name: Frontend test suite` step (the two Linux-only node suites then sit together):

```yaml
      - name: Sidecar test suite
        # bgutil-sidecar's wired `npm test` covers the homepage-ytcfg
        # extraction and minter keying — the logic behind the open
        # premiere-attestation field gate — and until now no workflow ran it.
        # The `npm ci` guard is required because the sidecar build step above
        # is skipped on an embed-cache HIT, which leaves node_modules absent.
        if: runner.os == 'Linux'
        working-directory: bgutil-sidecar
        run: |
          [ -d node_modules ] || npm ci --no-audit --no-fund --ignore-scripts
          npm test
```

- [ ] **Step 6: Add the `-race` step to `ci.yml`'s Linux leg**

Insert immediately **after** the existing `- name: go test` step (`run: go test -count=1 ./...`):

```yaml
      - name: go test -race (logger, database, web)
        # Owner ruling O-P. Three cheap packages, Linux only — the Windows
        # leg is unchanged. internal/logger's lock-hierarchy comment, and the
        # "run under -race" instructions in nine other test files, had never
        # been checked by anything: `grep -rn -- "-race" .github/ docs/
        # SPEC.md CLAUDE.md` returned zero hits before this step existed.
        if: runner.os == 'Linux'
        run: go test -race -count=1 ./internal/logger/... ./internal/database/... ./internal/web/...
```

- [ ] **Step 7: Add the libc-pin gate step to `ci.yml`**

Insert immediately **after** the `- name: go vet` step:

```yaml
      - name: modernc.org/libc pin matches the sqlite driver
        # modernc.org/sqlite's doc.go requires downstream modules to pin the
        # EXACT libc version its own go.mod names (gitlab.com/cznic/sqlite#177).
        # A `go get -u` sweep broke that once and nothing noticed until a
        # code review: build, vet, staticcheck and the full suite were all
        # green with the wrong pin. This is the gate that notices.
        run: |
          ours=$(go list -m -f '{{.Version}}' modernc.org/libc)
          want=$(sed -n 's#^[[:space:]]*modernc.org/libc \(v[^ ]*\).*#\1#p' "$(go list -m -f '{{.GoMod}}' modernc.org/sqlite)")
          if [ "$ours" != "$want" ]; then
            echo "modernc.org/libc is $ours but modernc.org/sqlite pins $want"
            echo "Bump modernc.org/sqlite (preferred) or match its libc pin exactly."
            exit 1
          fi
          echo "libc pin OK: $ours"
```

- [ ] **Step 8: Pin go-winres in `release.yml`**

Locate `go install github.com/tc-hib/go-winres@latest` inside the `- name: Generate Windows resources` step (the verifier puts it at `:108`; it is the **only** `@latest` anywhere in `.github/workflows/`, `Dockerfile`, `tools/` or `bgutil-sidecar/build.mjs`). Replace that one line with:

```bash
          # Pinned to a released tag, like staticcheck in ci.yml: this runs
          # inside the job that produces the .syso linked into the Windows
          # binary the same job then signs, so an unpinned third-party
          # `go install` here is a supply-chain hole in a signed artifact.
          go install github.com/tc-hib/go-winres@v0.3.3
```

v0.3.3 is the latest released tag (`go list -m -versions github.com/tc-hib/go-winres` → `… v0.3.0 v0.3.1 v0.3.2 v0.3.3`). Verify it resolves before committing:

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go list -m -versions github.com/tc-hib/go-winres | tr ' ' '\n' | tail -3
```

Expected: `v0.3.1 / v0.3.2 / v0.3.3` on the last three lines.

- [ ] **Step 9: Add `cache: npm` to `release.yml`'s `setup-node`**

Locate the `- uses: actions/setup-node@v7` block in `.github/workflows/release.yml` (it carries `if: steps.cache-embed.outputs.cache-hit != 'true'`) and add the two cache keys, keeping the `if:` exactly as it is. Only the sidecar lockfile is relevant there — `web/tests` is never installed in the release job:

```yaml
      - uses: actions/setup-node@v7
        if: steps.cache-embed.outputs.cache-hit != 'true'
        with:
          node-version: '24'
          cache: npm
          cache-dependency-path: bgutil-sidecar/package-lock.json
```

- [ ] **Step 10: Verify both workflow files structurally**

There is no YAML linter in this toolchain (no `pyyaml`, no `actionlint`, no node `yaml` module), so verify by assertion instead:

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
# YAML forbids tabs for indentation — must print nothing:
grep -nP '^\t' .github/workflows/ci.yml .github/workflows/release.yml
# The five ci.yml additions, one line each:
grep -n "cache: npm" .github/workflows/ci.yml .github/workflows/release.yml
grep -n "cache-dependency-path" .github/workflows/ci.yml .github/workflows/release.yml
grep -n "Sidecar test suite" .github/workflows/ci.yml
grep -n "go test -race -count=1" .github/workflows/ci.yml
grep -n "modernc.org/libc pin matches" .github/workflows/ci.yml
# No @latest anywhere in the build any more — must print nothing:
grep -rn "@latest" .github/workflows/ Dockerfile tools/ bgutil-sidecar/build.mjs | grep -v '^\.github/workflows/ci\.yml:[0-9]*:#'
# Every `- name:` in ci.yml, to eyeball step order and indentation:
grep -n "^      - name:\|^      - uses:" .github/workflows/ci.yml
```

Expected: no tabs; each added key present exactly where Steps 4–9 put it; the only surviving `@latest` mention is the explanatory comment at the top of `ci.yml` ("staticcheck is a hard gate, pinned to a release rather than @latest"). Then read the full diff once: `git diff .github/workflows/`.

- [ ] **Step 11: Commit**

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
git add .github/workflows/ci.yml .github/workflows/release.yml
git commit -m "ci: pin go-winres, run the sidecar tests and -race, cache npm, guard the libc pin

release.yml ran 'go install github.com/tc-hib/go-winres@latest' inside the job
that produces the .syso linked into the Windows binary it then signs — the only
unpinned tool in the whole build. Pinned to v0.3.3, the way ci.yml already pins
staticcheck.

bgutil-sidecar's wired 'npm test' (homepage-ytcfg extraction + minter keying)
was never invoked by any workflow; it now runs on the Linux leg, with an
'npm ci' guard because the sidecar build step is skipped on an embed-cache hit.

Owner ruling O-P: 'go test -race' over internal/logger, internal/database and
internal/web on the Linux leg only. Ten test files carried 'run under -race'
instructions that nothing honoured.

Neither workflow cached npm, so web/tests' npm ci ran uncached on every Linux
run; 'cache: npm' now covers both lockfiles.

And a gate for the invariant the previous commit restored: modernc.org/sqlite
requires its exact libc pin, and build/vet/staticcheck/tests were all green
while it was wrong.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- .github/workflows/ci.yml .github/workflows/release.yml
```

---

### Task 3: Repository hygiene — `go.mod`/`go.sum` line endings, the tidy gate, and `.claude/`

Rows #100 (TOOL-12) and #101 (TOOL-7, Impact lowered to **L** by the verifier: the seven committed skills are unaffected because git consults `.gitignore` only for untracked paths; the whole blast radius is that an eighth newly authored skill needs `git add -f`).

**Files:**
- Modify: `.gitattributes`
- Modify: `.gitignore`
- Modify: `go.mod`, `go.sum` (line endings only — zero content change)
- Modify: `.github/workflows/ci.yml` (one new step, in a different part of the job from Task 2's)

**Interfaces:**
- Consumes: Task 1's `go.mod`/`go.sum` content; Task 2's `ci.yml` (this task adds one further step to the same job).
- Produces: a usable `go mod tidy -diff` gate, which every later task in this arc must keep green.

- [ ] **Step 1: Measure the problem — see `go mod tidy -diff` emit a 329-line no-op diff**

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
perl -0777 -ne 'print "go.mod CRs: ", tr/\r//, "\n"' go.mod
perl -0777 -ne 'print "go.sum CRs: ", tr/\r//, "\n"' go.sum
go mod tidy -diff | wc -l
```

Expected (measured 2026-09-17): `go.mod CRs: 65`, `go.sum CRs: 162`, and a diff of roughly 329 lines with **zero** content change — the whole file rewritten because the working tree is CRLF under `core.autocrlf=true` while the Go toolchain writes LF. Use `perl -0777 -ne 'print tr/\r//'` and nothing else to count CRs: under Git Bash, `grep -c $'\r'` returns the LINE count and `awk '/\r/'` reports 0 on CRLF files (standing shell-gotcha rule).

- [ ] **Step 2: Pin both files to LF in `.gitattributes`**

`.gitattributes` already pins two individual files for exactly this reason. Locate the block that starts with the comment `# tools/fetch-node rewrites this tracked manifest with LF;` and insert **above** it (keeping the file's existing blank-line rhythm):

```
# The Go toolchain writes go.mod/go.sum with LF, but core.autocrlf=true gives
# them CRLF in the working tree — so `go mod tidy -diff` reported a 329-line
# whole-file diff with zero content change, which ruled it out as a CI gate.
# Pinning both to LF makes that gate usable (ci.yml runs it).
go.mod text eol=lf
go.sum text eol=lf
```

Note: `.gitattributes` itself is CRLF in the working tree (it has no `eol` pin of its own) while the index holds LF — `git ls-files --eol` shows `i/lf` for every tracked file in this repo. Write the new lines with the file's existing convention; git normalises on commit. Verify after editing that the index will be LF by checking nothing else changed: `git diff --stat .gitattributes` should show a small `+N` only.

- [ ] **Step 3: Normalise the two files in the working tree**

Do **not** use `git checkout`/`git add --renormalize` (the arc forbids checkout-class commands). Strip the CRs directly — the result is byte-identical to what git will now check out:

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
perl -0777 -pi -e 's/\r\n/\n/g' go.mod go.sum
perl -0777 -ne 'print "go.mod CRs: ", tr/\r//, "\n"' go.mod
perl -0777 -ne 'print "go.sum CRs: ", tr/\r//, "\n"' go.sum
```

Expected: `go.mod CRs: 0`, `go.sum CRs: 0`.

- [ ] **Step 4: Prove the gate is now clean**

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go mod tidy -diff
echo "exit=$?"
```

Expected: no output, `exit=0`. (Verified 2026-09-17 on a CR-stripped copy of `710904bf`: the 329-line diff collapses to nothing.)

- [ ] **Step 5: Add the tidy gate to `ci.yml`**

Insert immediately **after** the existing `- name: gofmt` step (so a tidy problem is reported next to a formatting one, before the expensive steps):

```yaml
      - name: go mod tidy check
        # Usable only because .gitattributes pins go.mod/go.sum to LF: under
        # core.autocrlf=true the working-tree CRLF made `-diff` emit the whole
        # file with no content change.
        run: go mod tidy -diff
```

- [ ] **Step 6: Narrow the `.claude/` ignore**

Locate the two lines in `.gitignore`:

```
# Claude Code session state (per-machine, never commit)
.claude/
```

and replace them with:

```
# Claude Code session state (per-machine, never commit).
# `.claude/*` rather than `.claude/`, with the skills directory re-included:
# the project's skills under .claude/skills/ are TRACKED, and a blanket ignore
# of the directory means a newly authored skill never shows in `git status`
# and needs `git add -f`. Ignoring the entries and re-including the one
# tracked subtree keeps settings.local.json, worktrees and plugin caches out.
.claude/*
!.claude/skills/
```

`.claude/*` must be used rather than `.claude/`: git cannot re-include a path whose **parent directory** is excluded, so `.claude/` + `!.claude/skills/` would not work.

- [ ] **Step 7: Verify the ignore rules behave**

`git check-ignore` is read-only.

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
git check-ignore -v .claude/skills/moombox-web-ui/SKILL.md; echo "tracked-skill exit=$? (want 1)"
git check-ignore -v .claude/skills/moombox-brand-new/SKILL.md; echo "new-skill exit=$? (want 1)"
git check-ignore -v .claude/settings.local.json; echo "settings exit=$? (want 0)"
git status --porcelain .claude
```

Expected: exit 1 for both skill paths (**not** ignored — this is the fix), exit 0 with `.gitignore:<n>:.claude/*` for `settings.local.json`, and `git status --porcelain .claude` showing nothing (the seven tracked skills are unmodified; no untracked skill exists today). This pattern shape was verified in a throwaway repo on 2026-09-17: `.claude/*` + `!.claude/skills/` leaves `settings.local.json` and `worktrees/` ignored while un-ignoring everything under `skills/`.

- [ ] **Step 8: Commit**

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
git add .gitattributes .gitignore go.mod go.sum .github/workflows/ci.yml
git commit -m "chore(repo): pin go.mod/go.sum to LF, gate 'go mod tidy -diff', un-ignore .claude/skills

Both files are written LF by the Go toolchain but checked out CRLF under
core.autocrlf=true, so 'go mod tidy -diff' emitted a 329-line whole-file diff
with zero content change — which ruled it out as a gate. Pinned in
.gitattributes (which already pins version.txt and the citation allowlist for
the same reason), normalised in the working tree, and wired into ci.yml next
to gofmt.

.gitignore's blanket '.claude/' contradicted the project skills tracked under
it: a newly authored skill never showed in 'git status' and needed 'git add
-f'. Narrowed to '.claude/*' + '!.claude/skills/' — '.claude/*' rather than
'.claude/' because git cannot re-include a path under an excluded directory.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- .gitattributes .gitignore go.mod go.sum .github/workflows/ci.yml
```

---

### Task 4: `FetchBody` errors past the 50 MB ceiling instead of truncating

Row #78 (TOOL-17). `FetchBody` returns `io.ReadAll(io.LimitReader(resp.Body, MaxFetchBodySize))`, so a response above the ceiling comes back **silently truncated with a nil error** and its callers parse half a page as a complete one.

**Files:**
- Modify: `internal/utils/http.go` (the `FetchBody` function)
- Test: `internal/utils/http_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `FetchBody(ctx, url, timeout, headers) ([]byte, error)` — same signature, new failure mode: a body longer than `utils.MaxFetchBodySize` (50 << 20) returns `nil, error` whose message contains `response exceeds MaxFetchBodySize`. Callers unchanged: `internal/utils/youtube.go`, `internal/youtube/service.go`, `internal/youtube/watch_page.go`.

- [ ] **Step 1: Write the failing test**

Append to `internal/utils/http_test.go` (locate `TestFetchBodyCapsAtMaxFetchBodySize` and put the new test directly after it):

```go
// TestFetchBodyErrorsPastMaxFetchBodySize pins the difference between a
// truncated read and a reported one. Before this, FetchBody handed back
// exactly MaxFetchBodySize bytes and a nil error for an over-long response,
// and internal/youtube/service.go and watch_page.go would parse that half a
// page as a legitimate result.
//
// Mutant this kills: dropping the length check (or restoring the plain
// io.LimitReader(resp.Body, MaxFetchBodySize)) makes err nil and len(body)
// exactly MaxFetchBodySize, and the first assertion fails.
func TestFetchBodyErrorsPastMaxFetchBodySize(t *testing.T) {
	chunk := make([]byte, 1<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		// One MiB past the ceiling. Writes may fail once the client gives
		// up reading; that is expected and not the test's business.
		for written := 0; written <= MaxFetchBodySize; written += len(chunk) {
			if _, err := rw.Write(chunk); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	body, err := FetchBody(t.Context(), srv.URL, 60*time.Second, nil)
	if err == nil {
		t.Fatalf("FetchBody over the ceiling: want an error, got nil with %d bytes", len(body))
	}
	if !strings.Contains(err.Error(), "response exceeds MaxFetchBodySize") {
		t.Errorf("error message: want it to name the ceiling, got %v", err)
	}
	if body != nil {
		t.Errorf("body on the over-long path: want nil, got %d bytes", len(body))
	}
}
```

`httptest`, `http`, `strings` and `time` are already imported by this file; confirm before adding any import.

- [ ] **Step 2: Run the test to verify it fails**

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestFetchBodyErrorsPastMaxFetchBodySize -v ./internal/utils/
```

Expected: FAIL — `FetchBody over the ceiling: want an error, got nil with 52428800 bytes`.

- [ ] **Step 3: Fix `FetchBody`**

In `internal/utils/http.go`, replace the body of `FetchBody` (locate it by the doc comment "FetchBody performs an HTTP GET and returns the response body as bytes."). The final `return` becomes:

```go
	// Read ONE byte past the ceiling so an over-long body is DETECTABLE.
	// io.LimitReader(resp.Body, MaxFetchBodySize) on its own truncates
	// silently with a nil error, and the callers (internal/youtube's
	// service.go and watch_page.go) would parse half a page as a complete
	// one — a watch page missing its tail parses as "no formats", which is
	// indistinguishable from a real extraction failure.
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxFetchBodySize+1))
	if err != nil {
		return nil, err
	}
	if len(body) > MaxFetchBodySize {
		return nil, fmt.Errorf("%s: response exceeds MaxFetchBodySize (%d bytes)", url, MaxFetchBodySize)
	}
	return body, nil
```

Also update the function's doc comment to state the new contract — replace the existing two-line comment with:

```go
// FetchBody performs an HTTP GET and returns the response body as bytes.
// Uses a single timeout context that spans both the HTTP request and body read.
// A body longer than MaxFetchBodySize is an ERROR, not a truncated success:
// silently handing back half a page let callers parse it as a whole one.
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go test -count=1 -run 'TestFetchBody' -v ./internal/utils/
go test -count=1 ./internal/utils/
go test -count=1 ./internal/youtube/          # the two real callers
```

Expected: the new test PASSes; `TestFetchBodyCapsAtMaxFetchBodySize` (the 100-byte normal-path test) still PASSes; both packages `ok`.

- [ ] **Step 5: Correct the stale comment on the neighbouring test**

`TestFetchBodyCapsAtMaxFetchBodySize`'s doc comment says "confirm the read truncates without error", which is now false. Replace that comment with:

```go
// TestFetchBodyCapsAtMaxFetchBodySize verifies the ordinary under-the-cap
// path still returns the whole body. The over-the-cap path is
// TestFetchBodyErrorsPastMaxFetchBodySize.
```

Re-run `go test -count=1 -run 'TestFetchBody' ./internal/utils/` — expected `ok`.

- [ ] **Step 6: Commit**

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
git add internal/utils/http.go internal/utils/http_test.go
git commit -m "fix(utils): FetchBody reports an over-long body instead of truncating it

io.ReadAll(io.LimitReader(resp.Body, MaxFetchBodySize)) returned exactly 50 MB
and a nil error for anything larger, so internal/youtube/service.go and
watch_page.go would parse half a watch page as a complete one — which presents
as 'no formats', indistinguishable from a real extraction failure. Read one
byte past the ceiling and error when it arrives.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/utils/http.go internal/utils/http_test.go
```

---

### Task 5: `utils.WriteFileAtomic`, adopted by the cipher player cache

Row #80 (TOOL-19). Three temp-then-rename implementations of the same job had drifted: `internal/cookies/cookie_files.go` documents exactly why a **fixed** `.tmp` name is unsafe when two writers can target one file and fsyncs before the rename, while `internal/cipher/player_cache.go` uses a fixed `path + ".tmp"` guarded only by an in-process mutex, skips the fsync, and calls `os.Rename` directly (no Windows AV/indexer retry).

**Scope note (read before widening this task).** Arc X adopts the new helper in **`internal/cipher/player_cache.go` only**. `internal/engine/downloader_resume.go`'s site was handled in Arc E (row #10, TOOL-2: swapped to `utils.ReplaceFile`), and `internal/cookies/cookie_files.go` keeps its own writer because it layers the DACL tightening on top — both are outside this arc's file set. `internal/utils/chatfile.go`'s `WriteChatFileAtomic` and `internal/utils/resumestore.go`'s `ResumeStore.Save` are deliberately **not** refactored onto the helper: both are pinned by tests that assert the fixed `<path>.tmp` name, and neither directory has the stale-temp sweeper that a unique-name writer needs (`internal/cookies` has one; nothing else does). Converting them is a separate change with its own risk; record it as a residual rather than folding it in here.

**Files:**
- Create: `internal/utils/writefile.go`
- Test: `internal/utils/writefile_test.go`
- Modify: `internal/cipher/player_cache.go` (the `atomicWrite` method)

**Interfaces:**
- Consumes: `utils.ReplaceFile(tmp, path string) error` (existing, `internal/utils/replacefile.go`).
- Produces: `func WriteFileAtomic(path string, data []byte, perm os.FileMode) error` in package `utils` — creates a uniquely named temp file beside `path`, writes, fsyncs, chmods to `perm`, then `ReplaceFile`s it over `path`, removing the temp on every failure path. Used by `internal/cipher/player_cache.go`'s `atomicWrite`.

- [ ] **Step 1: Write the failing test**

Create `internal/utils/writefile_test.go`:

```go
package utils

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestWriteFileAtomicWritesContentAndPerm is the ordinary path.
//
// Mutant this kills: dropping the os.Chmod leaves the file at CreateTemp's
// 0600 on POSIX, and the perm assertion fails.
func TestWriteFileAtomicWritesContentAndPerm(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "player.js")

	if err := WriteFileAtomic(path, []byte("payload"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "payload" {
		t.Errorf("content: want %q, got %q", "payload", string(got))
	}
	if runtime.GOOS != "windows" {
		// Windows has no POSIX mode bits to assert; everywhere else the
		// chmod is what lifts the file off os.CreateTemp's 0600.
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if info.Mode().Perm() != 0o644 {
			t.Errorf("perm: want 0644, got %v", info.Mode().Perm())
		}
	}
}

// TestWriteFileAtomicOverwritesAndLeavesNoTemp pins two properties at once:
// the replace is in-place (not an append or a second file), and no temp file
// survives a successful write.
//
// Mutant this kills: removing the ReplaceFile call and writing to path
// directly leaves the old content (or a partial file) and, with CreateTemp
// still in place, one leftover temp — either assertion fails.
func TestWriteFileAtomicOverwritesAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "player.js")

	if err := os.WriteFile(path, []byte("stale-and-longer"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := WriteFileAtomic(path, []byte("fresh"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "fresh" {
		t.Errorf("content after overwrite: want %q, got %q", "fresh", string(got))
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file %q survived a successful write", e.Name())
		}
	}
	if len(entries) != 1 {
		t.Errorf("directory entries after one write: want 1, got %d", len(entries))
	}
}

// TestWriteFileAtomicUsesAUniqueTempName pins the reason this helper exists
// rather than `path + ".tmp"`: two writers aiming at one file must not be
// able to interleave into a single temp and rename the corrupt result into
// place. Rather than racing two writers, the property is checked directly --
// the directory is pre-seeded with the FIXED name a second writer would be
// using, and the helper must leave it untouched.
//
// Mutant this kills: reverting to `tmpPath := path + ".tmp"` clobbers the
// seeded file, and the assertion that it still holds its marker fails.
func TestWriteFileAtomicUsesAUniqueTempName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "player.js")
	fixed := path + ".tmp"

	if err := os.WriteFile(fixed, []byte("another-writer-is-mid-write"), 0o644); err != nil {
		t.Fatalf("seed fixed temp: %v", err)
	}
	if err := WriteFileAtomic(path, []byte("fresh"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	got, err := os.ReadFile(fixed)
	if err != nil {
		t.Fatalf("the fixed .tmp name was consumed: %v", err)
	}
	if string(got) != "another-writer-is-mid-write" {
		t.Errorf("fixed .tmp content: want it untouched, got %q", string(got))
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestWriteFileAtomic -v ./internal/utils/
```

Expected: FAIL to build — `undefined: WriteFileAtomic`.

- [ ] **Step 3: Write the helper**

Create `internal/utils/writefile.go`:

```go
package utils

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteFileAtomic writes data to path via a uniquely named temp file in the
// same directory, fsyncs it, chmods it to perm, and replaces path with it.
// It is the one home for a pattern three packages had drifted copies of.
//
// Two properties the drifted copies were missing, each of which cost a real
// failure mode:
//
//   - A UNIQUE temp name (os.CreateTemp, not path+".tmp"): two writers aiming
//     at the same target would otherwise interleave into one temp file and
//     rename a corrupt result into place. internal/cookies/cookie_files.go
//     documents this; internal/cipher/player_cache.go did not do it.
//   - fsync BEFORE the rename: without it a crash can journal the rename
//     while the data pages never reach disk, leaving a zero-length or torn
//     file that the next read silently trusts.
//
// The replace goes through ReplaceFile, so the Windows AV/indexer sharing
// window is retried rather than reported as a hard failure.
//
// Callers that need more than this (the cookie writer's DACL tightening, the
// chat file's in-place header rewrite) keep their own writers; this helper is
// for the plain "replace this file's whole contents" case.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmpFile, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("fsync temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := ReplaceFile(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename temp file: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go test -count=1 -run TestWriteFileAtomic -v ./internal/utils/
go test -count=1 ./internal/utils/
```

Expected: all three new tests PASS; the package is `ok`.

- [ ] **Step 5: Adopt it in the cipher player cache**

In `internal/cipher/player_cache.go`, locate the `atomicWrite` method (by its doc comment "atomicWrite writes data to path via a temp file + rename.") and replace the method and its comment with:

```go
// atomicWrite writes data to path through utils.WriteFileAtomic, which gives
// the cache what its own copy lacked: a unique temp name (so two writers can
// never interleave into one temp), an fsync before the rename, and the
// Windows AV/indexer rename retry. pc.mu still serialises writers within this
// process; the unique name is what makes a second process safe too.
func (pc *PlayerCache) atomicWrite(path string, data string) error {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	return utils.WriteFileAtomic(path, []byte(data), 0o644)
}
```

Add the import. `internal/cipher` does not import `internal/utils` today, so add it to the import block beside `internal/httpx`:

```go
	"github.com/vampiricwulf/Moombox/internal/httpx"
	"github.com/vampiricwulf/Moombox/internal/utils"
```

No cycle: `internal/utils` imports only `internal/connectivity` and `internal/httpx`.

Then check whether `os` and `fmt` are still used elsewhere in `player_cache.go` (they are — the file has other file and error handling) with `GOTMPDIR=… go build ./internal/cipher/`; if the build reports an unused import, remove it.

- [ ] **Step 6: Run the cipher tests**

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go build ./internal/cipher/
go test -count=1 ./internal/cipher/
go vet ./internal/cipher/ ./internal/utils/
```

Expected: `ok` for the package, silent vet. If a cipher test asserts the fixed `<path>.tmp` name, that assertion is now wrong in the same way the code was — update it to assert "no `*.tmp` survives" rather than a specific name, and say so in the commit body.

- [ ] **Step 7: Commit**

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
git add internal/utils/writefile.go internal/utils/writefile_test.go internal/cipher/player_cache.go
git commit -m "refactor(utils,cipher): one utils.WriteFileAtomic; the player cache adopts it

Three temp-then-rename implementations had drifted. The cookie writer
documents why a fixed '.tmp' name is unsafe when two writers can target one
file, and fsyncs before the rename; the cipher player cache used a fixed
path+'.tmp' guarded only by an in-process mutex, skipped the fsync, and
renamed with os.Rename (no Windows AV/indexer retry).

utils.WriteFileAtomic(path, data, perm) is CreateTemp + write + fsync + chmod
+ utils.ReplaceFile, and PlayerCache.atomicWrite now delegates to it. The
cookie writer keeps its own copy (it layers the DACL tightening on top) and
the engine's site was converted to utils.ReplaceFile in Arc E.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/utils/writefile.go internal/utils/writefile_test.go internal/cipher/player_cache.go
```

---

### Task 6: Apply the production `modernize` sites by hand

Row #98 (TOOL-13). Of 87 diagnostics measured on 2026-09-17, **58** were the `ptrInt`/`intPtr`/`ptr`/`ptrBool`/`boolPtr`/`ptrInt64` → `new(expr)` class that the standing memory rule says breaks gopls and has to be reverted after any wholesale pass. Spec §5: **never run the pass wholesale.** This task runs the analyzer **read-only**, filters, and edits by hand.

**Files:** enumerated at execution time by Step 1. On 2026-09-17 the filtered set was 14 sites across 9 files: `internal/tui/settings.go` (×4), `internal/utils/chatfile.go` (×2), `internal/engine/downloader_fetch.go`, `internal/web/routes/jobs.go`, `internal/web/middleware.go`, `internal/utils/jsoncandidates.go`, `internal/notifications/discord.go`, `internal/worker/probe_classify.go`, `internal/web/routes/cookies.go`, `internal/web/routes/ffmpeg.go`. **Six arcs have edited several of those files since — re-enumerate; do not trust this list.**

**Interfaces:**
- Consumes: Task 1's Go 1.27 module (all four rewrites below need stdlib added in 1.21–1.26: the `min` builtin, `slices.Backward`, `strings.SplitSeq`, `errors.AsType`).
- Produces: no API change. Every rewrite is behaviour-preserving.

- [ ] **Step 1: Enumerate — run the analyzer read-only and filter**

This is the exact command the tooling reviewer used (gopls' `modernize` analyzer, run out-of-tree, **never with `-fix`**):

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go run golang.org/x/tools/gopls/internal/analysis/modernize/cmd/modernize@latest ./... > /tmp/modernize.txt 2>&1
echo "total: $(grep -c ':' /tmp/modernize.txt)"
# The set to apply: production files only, minus the ptr*->new(expr) class.
grep -v '_test\.go' /tmp/modernize.txt | grep -v 'new(expr)\|new(x)' | grep -v '^exit status' | sort
```

Notes:
- The analyzer exits **3** when it reports diagnostics. That is not a failure.
- `grep -v '_test\.go'` drops the test-file half; `grep -v 'new(expr)\|new(x)'` drops the `ptr*` class (which includes `internal/config/config.go`'s three **production** `boolPtr` diagnostics — they are in the excluded class and are **not** applied).
- Expected shape (2026-09-17 baseline, before the six arcs): 87 total, 17 production lines, **14 after the `new(expr)` filter** — 4 × `Ranging over SplitSeq is more efficient`, 2 × `if statement can be modernized using min`, 3 × `backward loop over slice can be modernized using slices.Backward`, 4 × `errors.As can be simplified using AsType[…]`, 1 × `for loop can be modernized using range over int`. If the count comes back wildly different (say, 0 or 60), the analyzer or the filter is wrong — investigate rather than proceeding.

Record the filtered list; it is the checklist for Steps 2–5.

- [ ] **Step 2: Apply the `strings.SplitSeq` sites**

`strings.Split` allocates the whole slice; `strings.SplitSeq` yields an `iter.Seq[string]`, so the `range` form takes **one** variable, not two. Each site is the same shape — in `internal/tui/settings.go` on 2026-09-17, the trusted-proxies and probe-targets loops in the validation function and again in the apply function:

```go
	// before
	for _, p := range strings.Split(m.values["trusted_proxies"], ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		...
	}

	// after
	for p := range strings.SplitSeq(m.values["trusted_proxies"], ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		...
	}
```

and the assignment-style twin:

```go
	// before
	proxies := []string(nil)
	for _, p := range strings.Split(m.values["trusted_proxies"], ",") {
		if p = strings.TrimSpace(p); p != "" {
			proxies = append(proxies, p)
		}
	}

	// after
	proxies := []string(nil)
	for p := range strings.SplitSeq(m.values["trusted_proxies"], ",") {
		if p = strings.TrimSpace(p); p != "" {
			proxies = append(proxies, p)
		}
	}
```

Do **not** convert a `strings.Split` whose result is indexed, re-used after the loop, or whose length is taken — `SplitSeq` returns no slice. The analyzer only flags the ones where the result is consumed purely by a single `range`.

- [ ] **Step 3: Apply the `min` sites**

```go
	// before (internal/engine/downloader_fetch.go)
	probeCap := n + 1
	if probeCap > capBytes {
		probeCap = capBytes // n == capBytes: the probe must not exceed the ceiling
	}

	// after — keep the comment, it explains the clamp
	// n == capBytes: the probe must not exceed the ceiling
	probeCap := min(n+1, capBytes)
```

```go
	// before (internal/web/routes/jobs.go, chatJSONLooksComplete)
	n := int64(chatJSONWindow)
	if size < n {
		n = size
	}

	// after
	n := min(int64(chatJSONWindow), size)
```

- [ ] **Step 4: Apply the `slices.Backward` sites**

`slices.Backward(s)` yields `(index, value)` from the last element down. Add `"slices"` to the file's import block.

```go
	// before (internal/utils/chatfile.go — the closing-bracket scan)
	bracketOffset := -1
	for i := len(tailBuf) - 1; i >= 0; i-- {
		if tailBuf[i] == ']' {
			bracketOffset = i
			break
		}
	}

	// after
	bracketOffset := -1
	for i, b := range slices.Backward(tailBuf) {
		if b == ']' {
			bracketOffset = i
			break
		}
	}
```

```go
	// before (internal/utils/chatfile.go — the has-existing-entries scan)
	for i := len(checkBuf) - 1; i >= 0; i-- {
		b := checkBuf[i]
		if b == '}' {
			hasExisting = true
			break
		} else if b != ' ' && b != '\n' && b != '\r' && b != '\t' {
			break
		}
	}

	// after — the index is unused here
	for _, b := range slices.Backward(checkBuf) {
		if b == '}' {
			hasExisting = true
			break
		} else if b != ' ' && b != '\n' && b != '\r' && b != '\t' {
			break
		}
	}
```

```go
	// before (internal/web/middleware.go — the X-Forwarded-For walk)
	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		hop := canonicalizeForwardedIP(parts[i])
		if !proxies.contains(hop) {
			// … the long comment about failing CLOSED stays exactly as it is
			return hop
		}
	}

	// after
	parts := strings.Split(xff, ",")
	for _, part := range slices.Backward(parts) {
		hop := canonicalizeForwardedIP(part)
		if !proxies.contains(hop) {
			// … the long comment about failing CLOSED stays exactly as it is
			return hop
		}
	}
```

This last one is security-relevant code (`EffectiveClientIP`, the reverse-proxy auth-bypass fix). The rewrite must preserve the **last-to-first** direction exactly — `slices.Backward` does — and every comment in the body. Gate it with `go test -count=1 ./internal/web/` and read the diff twice.

- [ ] **Step 5: Apply the `errors.AsType` sites and the range-over-int site**

`errors.AsType[E error](err error) (E, bool)` (Go 1.26) replaces the declare-then-`errors.As` pair.

```go
	// before (internal/notifications/discord.go — the webhook-token redaction)
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return 0, "", "", fmt.Errorf("%s %s: %w", uerr.Op, redactURLForLog(uerr.URL), uerr.Err)
	}

	// after
	if uerr, ok := errors.AsType[*url.Error](err); ok {
		return 0, "", "", fmt.Errorf("%s %s: %w", uerr.Op, redactURLForLog(uerr.URL), uerr.Err)
	}
```

```go
	// before (internal/worker/probe_classify.go — the value is unused)
	var netErr net.Error
	if errors.As(err, &netErr) {
		return classNetwork
	}

	// after — keep the comment above it explaining why one As covers
	// *net.OpError and *net.DNSError
	if _, ok := errors.AsType[net.Error](err); ok {
		return classNetwork
	}
```

```go
	// before (internal/web/routes/cookies.go — the value is unused)
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		jsonError(rw, fmt.Sprintf("that cookie file is larger than the %d KiB this endpoint accepts",
			maxCookieImportBytes/1024), http.StatusRequestEntityTooLarge)
		return "", false
	}

	// after
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		jsonError(rw, fmt.Sprintf("that cookie file is larger than the %d KiB this endpoint accepts",
			maxCookieImportBytes/1024), http.StatusRequestEntityTooLarge)
		return "", false
	}
```

```go
	// before (internal/web/routes/ffmpeg.go)
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode() == exitCodeRebootRequired
	}
	return false

	// after
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitErr.ExitCode() == exitCodeRebootRequired
	}
	return false
```

```go
	// before (internal/utils/jsoncandidates.go — ScanBalancedJSONObject)
	for i := 0; i < len(s); i++ {
		c := s[i]

	// after — i is never assigned inside the loop, so the rewrite is safe
	for i := range len(s) {
		c := s[i]
```

If the analyzer also reports `errors.As` sites where the bound variable IS used later outside the `if`, leave them alone and note why — `AsType`'s variable is scoped to the `if`.

- [ ] **Step 6: Verify the rewrites are behaviour-preserving**

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
gofmt -l ./cmd ./internal ./tools ./web           # must print nothing
go build ./...
go vet ./...
staticcheck ./...
# One package per touched file — the 2026-09-17 set:
go test -count=1 ./internal/tui/ ./internal/utils/ ./internal/engine/ \
                 ./internal/web/ ./internal/web/routes/ \
                 ./internal/notifications/ ./internal/worker/
node --test web/tests/*.test.mjs                  # routes/jobs.go is JS-adjacent
```

Then re-run the analyzer and confirm the filtered set is now **empty**:

```bash
go run golang.org/x/tools/gopls/internal/analysis/modernize/cmd/modernize@latest ./... 2>&1 \
  | grep -v '_test\.go' | grep -v 'new(expr)\|new(x)' | grep -v '^exit status'
```

Expected: no output. The `_test.go` and `new(expr)` diagnostics remain and are **correct to leave**.

- [ ] **Step 7: Commit**

Substitute `$FILES` with the space-separated list of files Step 1's filtered
enumeration named (on 2026-09-17 that was: `internal/tui/settings.go
internal/utils/chatfile.go internal/utils/jsoncandidates.go
internal/engine/downloader_fetch.go internal/web/middleware.go
internal/web/routes/jobs.go internal/web/routes/cookies.go
internal/web/routes/ffmpeg.go internal/notifications/discord.go
internal/worker/probe_classify.go`). The same list goes on both sides of the
commit.

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
git add $FILES
git commit -m "refactor: apply the production modernize sites by hand

Of 87 modernize diagnostics, 58 are the ptrInt/intPtr/ptr*/boolPtr ->
new(expr) class that breaks gopls and has to be reverted after any wholesale
pass, so the pass is never run wholesale (and never with -fix). This applies
the filtered production set by hand: strings.SplitSeq on the TUI settings
render path, two min clamps, three slices.Backward walks (including the
X-Forwarded-For walk, whose last-to-first direction and fail-closed comments
are preserved verbatim), four errors.AsType conversions, and one
range-over-int. The three production boolPtr diagnostics in
internal/config/config.go are in the excluded class and are left alone.

No behaviour change. gofmt/vet/staticcheck clean; the touched packages and the
node suite pass; re-running the analyzer leaves the filtered set empty.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- $FILES
```

---

### Task 7: Widen the citation test to `SPEC.md` + `CLAUDE.md`, and fix what it then catches

Row #102's TOOL-3 clause with owner decision **O-Z**, plus TOOL-14. `internal/docs`' citation test checks `docs/spec/*.md` only, and `SPEC.md` — the standalone reference an AI reads first — names symbols and wire values that do not exist. The verifier found the row **understated by one**.

**Files:**
- Modify: `internal/docs/citations_test.go`
- Modify: `SPEC.md`
- Modify: `CLAUDE.md`
- Possibly modify: `internal/docs/citation_allowlist.txt` (only for a genuine checker false positive, each entry with its `#` reason line above it — `parseAllowlist` rejects an unexplained entry)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `specDocs` now lists eight docs; a new `func docPath(root, name string) string` resolves `SPEC.md`/`CLAUDE.md` to the repo root and everything else to `docs/spec/`. Every later task that edits `SPEC.md`, `CLAUDE.md` or `docs/spec/*.md` must keep `go test ./internal/docs/` green — Task 8 edits `docs/spec/appendix-metrics.md`, which is deliberately **not** in `specDocs` (volatile numbers) and is unaffected.

- [ ] **Step 1: Write the failing test**

Add to `internal/docs/citations_test.go`, directly after `TestCitationAllowlistParsing`:

```go
// TestRootDocsAreChecked pins owner decision O-Z: SPEC.md and CLAUDE.md, the
// two most hand-edited docs and the ones an AI reads first, are inside the
// citation checks. A sweep found four invented symbols in SPEC.md and a
// non-existent path in CLAUDE.md precisely because nothing checked them.
//
// Mutants this kills: dropping either name from specDocs (the membership
// assertion fails), and making docPath send them to docs/spec/ (docLines
// t.Fatalf's on the missing file).
func TestRootDocsAreChecked(t *testing.T) {
	root := repoRoot(t)
	for _, name := range []string{"SPEC.md", "CLAUDE.md"} {
		found := false
		for _, d := range specDocs {
			if d == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s is not in specDocs -- O-Z put both root docs under the citation checks", name)
			continue
		}
		if n := len(docLines(t, root, name)); n < 50 {
			t.Errorf("%s resolved to %d lines -- docPath is not finding the repo-root doc", name, n)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestRootDocsAreChecked -v ./internal/docs/
```

Expected: FAIL — `SPEC.md is not in specDocs …` and the same for `CLAUDE.md`.

- [ ] **Step 3: Widen `specDocs` and add `docPath`**

In `internal/docs/citations_test.go`, replace the `specDocs` declaration and its comment:

```go
// specDocs are the docs whose code citations are checked: the six deep-dive
// docs under docs/spec/, plus the two hand-edited root docs added by owner
// decision O-Z (2026-09-17) after a sweep found four passages in SPEC.md
// naming symbols and wire values that do not exist, and a path in CLAUDE.md
// that does not. appendix-metrics.md (volatile numbers), design-philosophy.md
// and vision-and-purpose.md (prose) are out on purpose.
var specDocs = []string{
	"architecture.md",
	"data-and-storage.md",
	"operations.md",
	"platform-services.md",
	"security.md",
	"user-interfaces.md",
	"SPEC.md",
	"CLAUDE.md",
}
```

and add `docPath` immediately above `docLines`:

```go
// docPath resolves a specDocs entry to its file: the deep-dive docs live in
// docs/spec/, SPEC.md and CLAUDE.md at the repo root.
func docPath(root, name string) string {
	if name == "SPEC.md" || name == "CLAUDE.md" {
		return filepath.Join(root, name)
	}
	return filepath.Join(root, "docs", "spec", name)
}
```

and change `docLines` to use it:

```go
// docLines reads a spec doc, LF-normalised.
func docLines(t *testing.T, root, name string) []string {
	t.Helper()
	b, err := os.ReadFile(docPath(root, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
}
```

The fenced-block escape (`forEachProseLine` skips everything between ``` fences) and the allowlist escape (`<doc>.md|<path>` and `<doc>.md|<symbol>|<path>` keys) now apply to the two root docs unchanged — their keys are `SPEC.md|…` and `CLAUDE.md|…`.

- [ ] **Step 4: Run the whole `internal/docs` suite and collect every failure**

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/docs/ 2>&1 | tee /tmp/docs-widened.txt
```

**Expected shape, not an expected list.** Measured on `710904bf` before the six arcs: the widened test reported exactly **one** failure, a §-heading reference. Six arcs have since edited `SPEC.md` (Arc C owns `SPEC.md:665` for O-Y) and several docs, so the real list may be longer. Work through whatever comes back, using these rules:

- A **file/directory citation that does not exist** → fix the doc (the path moved or was wrong). Never allowlist it.
- A **symbol/path pair where the file does not declare the symbol** → read the code, then fix whichever is wrong. The check requires the **DECLARING** file, and a symbol surviving only in a `//` comment does not resolve.
- A **§-heading reference with no such heading** → fix the reference. Note the shape that bit on 2026-09-17: a sentence naming two sections of another doc, where only the first carries the doc link, so the second § resolves against the *citing* doc's own headings.
- A **genuine checker false positive** (prose shaped like a citation, a TOML key that looks like an identifier, a deliberately illustrative path) → allowlist it with a `#` reason line immediately above; `parseAllowlist` fails the build on an unexplained entry.

The 2026-09-17 failure and its fix, verified green:

```
SPEC.md:695 references §Credential Notifications for what an ope in SPEC.md, which has no such heading
```

The sentence names two headings of `docs/spec/operations.md` but only links the doc before the first §, so `docNameRe` cannot see a target for the second and defaults to `SPEC.md`. Both headings do exist in `operations.md` (`## Browser Cookie Acquisition (Platform Differences)` and `### Credential Notifications`). Fix by repeating the link:

```
 and § Credential Notifications for what an operator is actually told.
→
 and [docs/spec/operations.md](docs/spec/operations.md) § Credential Notifications for what an operator is actually told.
```

- [ ] **Step 5: Fix the four `SPEC.md` passages TOOL-3 names (plus the fifth the verifier found)**

Each replacement below was applied and verified green against `710904bf` on 2026-09-17. Locate each by its quoted text, not by line number.

**(a) The Twitch monitor's persisted query** (the `**TwitchMonitor**` paragraph). `UseLive` has **zero** hits in the tree; the monitor calls `GetStreamInfoBatch`, which sends the `StreamMetadata` + `ComscoreStreamingQuery` pair as one batched request.

```
before: Uses the `UseLive` persisted query to check if the channel is live.
after:  Batches the `StreamMetadata` and `ComscoreStreamingQuery` persisted queries through `GetStreamInfoBatch` (`internal/twitch/api.go`) to check whether each channel is live.
```

**(b) The stream-status set** (the `**Stream status classification:**` paragraph). The real set is five values in `internal/youtube/types.go`; `StreamProcessing` has zero hits and `StreamOffline` exists only as the JSON key `liveStreamOfflineSlateRenderer`, while the real `StreamPostLive` and `StreamVOD` were missing.

```
before: `StreamUpcoming` (scheduled, not yet live), `StreamProcessing` (recently ended, being processed), `StreamOffline` (ended or unavailable). Classification uses
after:  `StreamUpcoming` (scheduled, not yet live), `StreamPostLive` (the broadcast ended and YouTube is still processing the recording), `StreamVOD` (a finished stream served as an ordinary video). Those five are the whole of `StreamStatus` in `internal/youtube/types.go`. Classification uses
```

The trailing `` `StreamStatus` in `internal/youtube/types.go` `` is deliberate: " in " is one of the connectors the checker recognises, so the pair is now mechanically verified against the declaring file.

**(c) The Twitch GQL operation set** (the `**GQL operations:**` paragraph). The four persisted hashes are `TwitchGQLHashes` in `internal/constants/constants.go`; `UseLive` does not exist; `GetStreamInfo` is a Go method (`internal/twitch/api.go`), not a GQL operation — the error is the category, not the name; and the verifier's fifth finding is that `PlaybackAccessToken` is really `StreamPlaybackAccessToken` (with a `VideoPlaybackAccessToken` twin), both of which send GraphQL text rather than a persisted hash.

```
before: **GQL operations:** `UseLive` (stream info), `PlaybackAccessToken` (access tokens for HLS), `VideoCommentsByOffsetOrCursor` (VOD chat), `GetStreamInfo` (stream metadata). Each operation is identified by its SHA256 persisted query hash, not the query text
after:  **GQL operations:** `StreamMetadata` and `ComscoreStreamingQuery` (live stream info, sent as one batched request), `VideoMetadata` (VOD metadata), `VideoCommentsByOffsetOrCursor` (VOD chat) — those four persisted hashes are `TwitchGQLHashes` in `internal/constants/constants.go`. The two playback-access-token calls (`StreamPlaybackAccessToken`, `VideoPlaybackAccessToken`) send GraphQL text rather than a persisted hash. Each persisted operation is identified by its SHA256 hash, not the query text
```

**(d) The WebSocket event list** (the `**Server-to-client message types:**` bullets). `cookie_status` is never broadcast; `backfill_status` and `connectivity` are real and were missing. **Enumerate the real set at execution time** — Arc W adds a `job_progress` frame (O-O), so the list has changed since this plan was written:

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
grep -rn 'Broadcast("' --include=*.go internal cmd | grep -v '_test\.go' \
  | sed 's/.*Broadcast("\([^"]*\)".*/\1/' | sort -u
```

On 2026-09-17 that produced: `backfill_status, check_timers, config_update, connectivity, disk_status, job_deleted, job_update, jobs_update, log, update_available` (`test` comes from `websocket_test.go` and is excluded by the `_test.go` filter). `initial_state` is not a `Broadcast` call — it is marshalled directly in `internal/web/websocket.go` and sent on connect — so its bullet stays. Reconcile the bullet list against that output: delete `cookie_status`, add anything missing with a payload description read from the broadcast site, and anchor the list:

```
before: - `cookie_status` — Cookie auth change (payload: auth status)
after:  - `backfill_status` — Catalog-backfill progress for one channel (payload: `{channel, tab, pages, state}`)
        - `connectivity` — Network reachability changed (payload: `{online}`)
        (blank line)
        That list is every `Broadcast` call in `internal/web/websocket.go` plus the `initial_state` snapshot the hub sends on connect.
```

plus a bullet for `job_progress` with the payload O-O specifies (id, status, progress, percent, speed, eta, lastVideoSeq, lastAudioSeq, totalChatMessages, updatedAt) — read Arc W's `websocket.go` hunk for the exact field names it shipped rather than copying them from the spec table.

The closing sentence's `` `Broadcast` `` + " call in " + `` `internal/web/websocket.go` `` is again a deliberate pair: `WebSocketHub.Broadcast` is declared in that file, so the anchor is mechanically checked.

- [ ] **Step 6: Fix `CLAUDE.md` (TOOL-14)**

In the Config-migrations pattern section, the sentence citing the migration helper names a top-level `config/` directory that does not exist:

```
before: `migrateOldFormat()` in `config/config.go` handles backward compat
after:  `migrateOldFormat()` in `internal/config/config.go` handles backward compat
```

Note **why this one still needs a human**: `config/config.go` does not start with any of the checker's `citationPrefixes` (`internal/`, `cmd/`, `web/`, `tools/`, `docs/`, `bgutil-sidecar/`, `.github/`), so the widened test reads it as prose and never flagged it. The widening guards the **corrected** form (`internal/config/config.go` is a real path, and `migrateOldFormat` is declared there, so the pair is now checked) — it could not have caught the original.

- [ ] **Step 7: Run the suite green**

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go test -count=1 ./internal/docs/
go test -count=1 -run TestRootDocsAreChecked -v ./internal/docs/
gofmt -l ./internal/docs
```

Expected: `ok`, the new test PASSes, gofmt silent. Also confirm the vacuity floors in `TestSpecDocCitationsResolve` did not fall (they only rise when docs are added) — if a floor now fails, the scan broke; do **not** lower a floor to go green.

- [ ] **Step 8: Verify the widening is not vacuous**

Prove the widened scan actually reads the two root docs, in a scratch copy only:

```bash
SC="C:/Users/Wulf/AppData/Local/Temp/claude/D--Git-Moombox/<session>/scratchpad/ozmutant"
rm -rf "$SC"; mkdir -p "$SC"
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling && git archive HEAD | tar -x -C "$SC"
cd "$SC"
# Plant rot in each root doc, one at a time:
perl -pi -e 's{`internal/config/config\.go`}{`internal/config/gone.go`}' CLAUDE.md
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestSpecDocCitationsResolve ./internal/docs/ 2>&1 | head -5
```

Expected: FAIL naming `CLAUDE.md:<n> cites \`internal/config/gone.go\`, which does not exist`. Repeat with a bogus symbol in `SPEC.md` (e.g. change `` `StreamStatus` in `internal/youtube/types.go` `` to `` `StreamStatusGone` in `internal/youtube/types.go` ``) and expect `…but that file …`. Delete the scratch copy's changes by deleting the directory — never edit the worktree for this.

- [ ] **Step 9: Commit**

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
git add internal/docs/citations_test.go SPEC.md CLAUDE.md
git commit -m "docs(spec): put SPEC.md and CLAUDE.md under the citation test, and fix what it caught

Owner decision O-Z. The citation checks covered docs/spec/*.md only, so the
two most hand-edited docs — the ones an AI reads first — drifted unchecked:
SPEC.md named a StreamProcessing/StreamOffline pair that does not exist (the
real StreamStatus set is five values in internal/youtube/types.go), a UseLive
GQL operation that has zero hits and a GetStreamInfo 'GQL operation' that is
really a Go method, PlaybackAccessToken instead of StreamPlaybackAccessToken,
and a cookie_status WebSocket event that is never broadcast while the real
backfill_status and connectivity were missing. CLAUDE.md cited a top-level
config/ directory that does not exist.

specDocs gains SPEC.md and CLAUDE.md, with a docPath helper resolving them to
the repo root; the fenced-block and allowlist escapes apply unchanged. Each
corrected passage now ends in a symbol/path pair the checker verifies against
the declaring file, so the next drift fails the suite instead of a review.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/docs/citations_test.go SPEC.md CLAUDE.md
```

---

### Task 8: Regenerate `docs/spec/appendix-metrics.md`

Row #102's TOOL-4 clause. Every Totals figure and most per-package rows are stale. The verifier **REJECTED one sub-claim**: the package count stays **30**, because `appendix-metrics.md` excludes `internal/docs` from the Package Scale rows and the internal/ Totals by its own stated rule. Do not change it to 31.

This task runs last among the content tasks so the numbers reflect everything the arc (and the six before it) changed.

**Files:**
- Modify: `docs/spec/appendix-metrics.md`

**Interfaces:**
- Consumes: Task 1's `go.mod` (Key Dependencies), and every file every earlier task and arc touched (the scale tables).
- Produces: a regenerated appendix. `docs/spec/appendix-metrics.md` is deliberately **not** in `specDocs`, so this task does not need the citation test — but run it anyway (it is cheap) to confirm nothing else regressed.

- [ ] **Step 1: Regenerate the per-package table with the file's own recipe**

The recipe is printed at the top of the file. Run it verbatim:

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
for d in $(find internal -type d); do
  printf "%-32s %6s lines %3s src %3s test\n" "$d" \
    "$(find "$d" -maxdepth 1 -name '*.go' ! -name '*_test.go' -exec cat {} + 2>/dev/null | wc -l)" \
    "$(find "$d" -maxdepth 1 -name '*.go' ! -name '*_test.go' | wc -l)" \
    "$(find "$d" -maxdepth 1 -name '*_test.go' | wc -l)"
done | sort -k2 -rn
```

Rewrite every row of the **Package Scale** table from this output, keeping the existing Description column text unless a package's purpose actually changed. Source lines are rounded to the nearest 10 (`~20,450`). Drop `internal/docs` — the file's own rule excludes it.

- [ ] **Step 2: Regenerate the Totals**

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
echo "internal src files : $(find internal -name '*.go' ! -name '*_test.go' ! -path 'internal/docs/*' | wc -l)"
echo "internal src lines : $(find internal -name '*.go' ! -name '*_test.go' ! -path 'internal/docs/*' -exec cat {} + | wc -l)"
echo "internal test files: $(find internal -name '*_test.go' ! -path 'internal/docs/*' | wc -l)"
echo "internal test lines: $(find internal -name '*_test.go' ! -path 'internal/docs/*' -exec cat {} + | wc -l)"
echo "internal packages  : $(find internal -name '*.go' ! -name '*_test.go' ! -path 'internal/docs/*' -printf '%h\n' | sort -u | wc -l)"
echo "cmd src files      : $(find cmd -name '*.go' ! -name '*_test.go' | wc -l)"
echo "cmd src lines      : $(find cmd -name '*.go' ! -name '*_test.go' -exec cat {} + | wc -l)"
echo "cmd test files     : $(find cmd -name '*_test.go' | wc -l)"
echo "frontend files     : $(find web/public -type f | wc -l)"
echo "frontend lines     : $(find web/public -type f -exec cat {} + | wc -l)"
echo "frontend KB        : $(du -sk web/public | cut -f1)"
ls web/public web/public/modules
```

These commands were validated on `710904bf` against the verifier's independently regenerated figures and reproduce them exactly: 296 src files / 108,063 src lines / 408 test files / 115,545 test lines / **30** packages for internal (excluding `internal/docs`), and 21 / 7,352 / 28 for cmd. **The six merged arcs will have moved all of them** — use what the commands print.

Rewrite the four Totals bullets from this output, and re-derive the Frontend bullet's module list from the `ls` (the bullet names every module file explicitly).

- [ ] **Step 3: Refresh the Key Dependencies table from `go.mod`**

Task 1 moved four of these. Read the current values rather than typing them:

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
for m in github.com/go-chi/chi/v5 charm.land/bubbletea/v2 charm.land/bubbles/v2 \
         charm.land/huh/v2 charm.land/lipgloss/v2 charm.land/glamour/v2 \
         github.com/dop251/goja modernc.org/sqlite github.com/coder/websocket \
         github.com/BurntSushi/toml; do
  printf "%-32s %s\n" "$m" "$(go list -m -f '{{.Version}}' "$m")"
done
```

Update each row. The goja row is abbreviated in the table (`v0.0.0-20260903`) — keep that abbreviation style and update the date portion to match the new pseudo-version.

- [ ] **Step 4: Refresh Runtime and Test Baseline**

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
grep -n '^go \|^toolchain ' go.mod
grep -rn 'version = "' cmd/moombox/main.go | head -2          # Current app version
grep -rn 'schemaVersion = ' internal/database/migrations.go   # Database schema version
go list ./... | wc -l                                          # Packages
grep -c '^	{' internal/cookies/autocookies_detect.go          # knownBrowsers rows (sanity-check by reading)
```

Expected on `710904bf`: `go 1.27` / `toolchain go1.27.1`, app version `2.8.8` (this chain does **no** version bump — leave it), schema version 19 unless an arc changed it, 36 packages. The Test Baseline's "32 ok / 0 fail" and the four no-test packages hold unless an arc added a package; confirm against the controller's full-suite run at the merge candidate rather than guessing.

- [ ] **Step 5: Stamp the verification date**

Replace the `> **Last verified:** 2026-09-05` line with today's date:

```bash
date +%Y-%m-%d
```

- [ ] **Step 6: Verify**

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go test -count=1 ./internal/docs/
grep -n "30 packages" docs/spec/appendix-metrics.md
grep -n "internal/docs is excluded" docs/spec/appendix-metrics.md
```

Expected: `ok`; the Totals bullet still says **30 packages**; the exclusion rule sentence is still present (it is what makes 30 correct).

- [ ] **Step 7: Commit**

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
git add docs/spec/appendix-metrics.md
git commit -m "docs(spec): regenerate appendix-metrics after the sweep-2 chain

Every Totals figure and most per-package rows were stale (last verified
2026-09-05, before seven arcs). Regenerated with the file's own recipe, plus
Key Dependencies from go.mod after this arc's bumps.

The package count stays 30: the file excludes internal/docs from the Package
Scale rows and the internal/ Totals by its own stated rule, so the sweep's
'30 -> 31' sub-claim was rejected by the verifier.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- docs/spec/appendix-metrics.md
```

---

### Task 9: Arc gates, then delete this plan

**Files:**
- Delete: `docs/superpowers/plans/2026-09-17-sweep2-x-tooling.md`

**Interfaces:**
- Consumes: every earlier task.
- Produces: a merge candidate.

- [ ] **Step 1: Merge `main` into the branch**

Per the spec's merge-candidate recipe, `main` goes in first (no arc should merge after this one, but run it anyway so the gates are honest):

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
git merge main
```

If there are conflicts, resolve them in the worktree and re-run every gate below. (`git merge` is not in the forbidden stash/checkout/rebase/reset/amend list.)

- [ ] **Step 2: Run the full merge-candidate gate set**

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
gofmt -l ./cmd ./internal ./tools ./web
go vet ./...
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go vet ./...
staticcheck ./...
go mod tidy -diff
go build ./...
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /dev/null ./cmd/moombox
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o /dev/null ./cmd/moombox
node --test web/tests/*.test.mjs
cd bgutil-sidecar && npm test && cd ..
go test -race -count=1 ./internal/logger/... ./internal/database/... ./internal/web/...
```

Expected: gofmt silent, vet silent on both platforms, staticcheck silent, `go mod tidy -diff` exit 0 with no output, three builds clean, both node suites pass, `-race` clean.

- [ ] **Step 3: The ONE full suite run (controller)**

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./...
```

Expected: `ok` / `no test files` for all packages, zero failures. Only one of these runs at a time across the whole session.

- [ ] **Step 4: The arc's live gates**

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
MOOMBOX_LIVE_CIPHER_TEST=1 go test -count=1 -timeout 180s -run "TestSidecarSolver" ./internal/cipher/...
MOOMBOX_LIVE_BG_TEST=1 go test -count=1 -timeout 180s -run "TestSidecarLive" ./internal/bgutils/...
```

Expected: PASS. `TestBotGuardLiveFingerprint` is excluded on purpose (known failure, not a gate — O-U).

- [ ] **Step 5: Delete this plan**

Implemented and verified plans are deleted; git history is the archive.

```bash
cd /d/Git/Moombox/.worktrees/sweep2-x-tooling
rm docs/superpowers/plans/2026-09-17-sweep2-x-tooling.md
git add -A docs/superpowers/plans/2026-09-17-sweep2-x-tooling.md
git commit -m "chore(plans): delete the Arc X plan — implemented and verified

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- docs/superpowers/plans/2026-09-17-sweep2-x-tooling.md
```

- [ ] **Step 6: Hand off**

Report to the controller: every gate's output, the `modernize` filtered set as enumerated (Task 6 Step 1) and as re-run empty (Step 6), the widened-citation-test failure list and how each was resolved (Task 7 Step 4), the regenerated Totals (Task 8), and the residuals listed below. The controller runs the Fable close review, merges `--no-ff` without asking, runs the post-merge gates, deletes the worktree **and** the branch, and closes the ledger.

---

## Residuals to report (not fixed by this arc, by design)

- **TOOL-20** (`tools/fetch-node`'s Node pin one patch behind v24.21.0) — dropped by the spec; v24 is the right LTS line and the pinned release carries no security flag. Revisit after the October 2026 v26 LTS promotion.
- **`internal/utils/chatfile.go`'s `WriteChatFileAtomic` and `internal/utils/resumestore.go`'s `ResumeStore.Save`** keep their fixed `<path>.tmp` writers rather than adopting `utils.WriteFileAtomic` — their tests pin that name, and neither directory has the stale-temp sweeper a unique-name writer wants. A separate change.
- **`FetchBody` does not pre-size its buffer from `Content-Length`** — TOOL-17 listed that as optional and it overlaps the first sweep's engine `io.ReadAll` pre-sizing item (#17 there).
- **The seven indirect-only module updates** (ultraviolet, x/exp/slice, regexp2/v2, pprof, x/net, x/sys, xo/terminfo) are available and untaken; O-U does not name them.
- **The 58 `ptr*` → `new(expr)` modernize diagnostics** (including three production `boolPtr` ones in `internal/config/config.go`) are deliberately left; applying them breaks gopls.
- **`TestBotGuardLiveFingerprint`** remains a known live failure on the goja BotGuard path.

---

## Self-review

**Spec coverage.** Every row spec §3 assigns to Arc X has a task: #11 + #81 + O-U → Task 1; #44, #45, #46 (O-P), #77 → Task 2; #100, #101 → Task 3; #78 → Task 4; #80 → Task 5; #98 → Task 6; #102's TOOL-3 + O-Z and TOOL-14 → Task 7; #102's TOOL-4 → Task 8; plan deletion → Task 9. Spec §5's "never run the modernize pass wholesale" is a named Arc-X constraint and is enforced in Task 6 Step 1 (read-only, filtered) and Step 6 (re-run, filtered set empty). Spec §4's "wave 3, alone, after every other arc merged" is the Branch-and-worktree precondition. The three enumerating tasks (6, 7, 8) each run their enumeration at execution time and treat the 2026-09-17 figures as an expected *shape*, never a list to copy.

**Placeholder scan.** No "TBD", no "add appropriate error handling", no "similar to Task N". The three tasks whose exact target set cannot be known in advance each give the enumeration command, the filter, the expected shape, the decision rules for each failure class, and worked examples that were executed and verified on `710904bf`.

**Type consistency.** `utils.WriteFileAtomic(path string, data []byte, perm os.FileMode) error` is declared in Task 5 Step 3 and called with exactly that signature in Task 5 Step 5. `docPath(root, name string) string` is declared in Task 7 Step 3 and used by `docLines` in the same step and by `TestRootDocsAreChecked` (via `docLines`) in Step 1. `FetchBody`'s signature is unchanged in Task 4; only its failure mode moves, and the test asserts the exact substring the implementation emits (`response exceeds MaxFetchBodySize`). The libc-pin shell check is written identically in Task 1 Steps 1/3 and Task 2 Steps 3/7.
