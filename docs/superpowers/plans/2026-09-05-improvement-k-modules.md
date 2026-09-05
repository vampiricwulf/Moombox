# Arc K — All-Module Upgrade Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bring every Go module in `go.mod` to its latest version in one gated bump, prove the result with the full gate ladder (build, vet, staticcheck, full suite, node, both Linux cross-builds, exe smoke, the cipher and sidecar live gates, govulncheck), hold back any module group that regresses, then regenerate the volatile metrics appendix.

**Architecture:** One bump commit (`go get -u ./...` + `go mod tidy`; 21 indirect modules at survey time, zero direct — Arc A already bumped the directs). The controller runs the gate ladder itself (it includes the arc's single full `go test ./...` and the network-bound live gates). A regression is bisected by reverting one module GROUP at a time (charm → goja chain → modernc → x/* → misc) and held at the old versions in a follow-up commit with the failing output filed as intake. Then a mechanical gopls-modernizer commit and the appendix regeneration.

**Tech Stack:** Go 1.27 / toolchain go1.27.1 (unchanged by this arc), staticcheck 2026.2.1 (unchanged unless forced — ruling K6), Node 24, `govulncheck`, the `MOOMBOX_LIVE_*` gates.

**Spec:** `docs/superpowers/specs/2026-09-04-improvement-chain-design.md` §10. Design notes + rulings K1–K8 and the module survey: `.superpowers/sdd/2026-09-05-improvement-k-modules/design-notes.md`.

## Global Constraints

- `go.mod`'s `go 1.27` and `toolchain go1.27.1` lines and ci.yml's `staticcheck@2026.2.1` are NOT changed by this arc (ruling K6); if a gate forces the question, stop and ledger a ruling.
- `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp` on every go command; exactly ONE `go test -count=1 ./...` per gate ladder run, controller-run.
- The goja BotGuard live test is a known long-standing failure — it is NOT a regression signal; the cipher live gate and the sidecar live gate are.
- Never edit a test expectation to absorb a rendering or regex change without reading and recording what changed.
- LF; two trailers on every commit; git read-only beyond add/commit for any subagent; nothing pushes.

---

### Task 1: the bump commit (K1)

**Files:** `go.mod`, `go.sum`.
- [ ] **Step 1:** record the survey: `go list -m -u -f '{{if .Update}}{{.Path}} {{.Version}} -> {{.Update.Version}}{{end}}' all | grep -v '^$' > .superpowers/sdd/2026-09-05-improvement-k-modules/before.txt` (the ledger dir is gitignored).
- [ ] **Step 2:** `go get -u ./... && go mod tidy`; `git diff --stat go.mod go.sum`; confirm the `go`/`toolchain` lines are unchanged (`git diff go.mod | grep -E '^[-+](go|toolchain) '` → empty).
- [ ] **Step 3:** `go build ./... && go vet ./... && staticcheck ./... && echo STATIC-OK` (the first rung; if staticcheck refuses a module's Go version → K6 ruling, stop).
- [ ] **Step 4:** commit `chore(deps): go get -u — every indirect module to its latest` with the body = the before.txt table (old → new per module, grouped charm / goja chain / modernc / x/* / misc) + trailers.

### Task 2: the gate ladder (K2; controller-run; bisect K3 only on failure)

Run in the worktree at Task 1's commit, log every rung to `.superpowers/sdd/2026-09-05-improvement-k-modules/gate-<sha>.txt`:
- [ ] **Rung 1:** gofmt silent; `go build ./... && go vet ./... && staticcheck ./...`.
- [ ] **Rung 2:** `go test -count=1 ./...` (the ONE full run; ~5 min) and `node --test web/tests/*.test.mjs` (expect the post-Arc-J count, 0 fail).
- [ ] **Rung 3:** `GOOS=linux GOARCH=amd64 go build -o /dev/null ./cmd/moombox` and `GOOS=linux GOARCH=arm64 go build -o /dev/null ./cmd/moombox` (on Windows write to `$GOTMPDIR/moombox-linux-{amd64,arm64}`; the embed blobs must be present in the worktree).
- [ ] **Rung 4:** `go build -o moombox.exe ./cmd/moombox` in the MAIN checkout after the merge (post-merge gate) — in the worktree, build to `$GOTMPDIR/moombox-k.exe` and run it with `--version` (or the launcher's `-h`) — it must start and exit 0.
- [ ] **Rung 5 (live, network):** `MOOMBOX_LIVE_CIPHER_TEST=1 go test -count=1 -run 'Live' ./internal/cipher/` and the sidecar live gate (`grep -rn MOOMBOX_LIVE_BG_TEST internal/bgutils/*_test.go` names the test; run the SIDECAR path — the goja BotGuard path's failure is expected and ignored). If the network is unavailable, record "live gates NOT RUN" in the ledger and re-run before the merge; never merge without them having passed once on the bumped tree.
- [ ] **Rung 6:** `go run golang.org/x/vuln/cmd/govulncheck@latest ./...` (Arc A: 0) — record the count; a new finding is intake, not a gate failure, unless it is in a direct code path (ruling on the spot).
- [ ] **Rung 7:** TUI rendering smoke — `go test -count=1 ./internal/tui/` passed in rung 2; additionally, if a terminal is available, open the exe's TUI once and view the task list, the help overlay and the stats overlay (`R T`); record "not run" otherwise.
- [ ] **On any failure (K3):** revert ONE group (`go get <module>@<old-version>` for each module in the group from before.txt, `go mod tidy`, re-run the failing rung) in the order charm → goja chain → modernc → x/* → misc; when the rung passes, commit `chore(deps): hold <group> at <versions> — <rung> regressed: <one line>` with the failing output pasted in the body, and file the regression as intake in the chain ledger. Never absorb a rendering/regex change by editing tests.

### Task 3: gopls modernizers (K7; sonnet; behaviour-free)

**Files:** the hint sites gopls reports after the bump — at least `internal/cookies/refresh_liveness_test.go:388` (`waitgroupgo` → `wg.Go`) and `:703` (`testingcontext` → `t.Context()`); run `gopls check` (or read the editor diagnostics the controller relays) over `./internal/...` and collect every `modernize`-class hint.
- [ ] Apply each; DO NOT run `go fix` blindly (it inlines the worker `ptrInt` test helper to `new(expr)` — the recorded opt-out); per touched package `go vet`, `staticcheck`, `go test -count=1`.
- [ ] Commit `chore: gopls modernizers — WaitGroup.Go, t.Context` + trailers.

### Task 4: appendix metrics regenerated (K5; sonnet)

**Files:** `docs/spec/appendix-metrics.md`; the memory's "Codebase Scale" line is the controller's.
- [ ] Run the recipe in the file's header verbatim (per-package lines/files/tests; frontend sizes; module counts; test counts) at the bump commit; update every table and the "Last verified" date; do not change prose that is not a number or a count unless it is now false.
- [ ] `go test -count=1 ./internal/docs/`; LF; commit `docs: appendix metrics regenerated after Arcs A–K` + trailers.

### Task 5: close (controller)

- [ ] Fable arc-close review of the branch (go.mod/go.sum diff + any hold commits + Tasks 3–4), one fix wave if needed, merge --no-ff, post-merge gate on main incl. the exe rebuild, ledgers/memory, recap (module table, gate outcomes incl. which live gates ran, held groups, and that the ubuntu CI leg on push is the first Linux RUN of the bumped modernc/libc).
