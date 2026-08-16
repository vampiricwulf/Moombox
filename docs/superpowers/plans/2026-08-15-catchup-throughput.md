# Catch-up Throughput Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make live catch-up use the bandwidth that is actually available — segment concurrency becomes a user setting instead of a hardcoded 6, and catch-up stops stalling its whole worker pool at every batch boundary.

**Architecture:** `ParallelDownloads` stops being the operative number: `DownloaderOptions.SegmentWorkers` carries a per-download value plumbed from `downloader.segment_workers`, and every derived limit (batch ceiling, damping floor, channel sizes, HTTP idle-conn pool) is computed from it. The catch-up reorder buffer gains a byte ceiling so raising workers costs connections rather than unbounded RAM. Then `runParallelCatchUp` becomes a rolling window: workers pull the next unfetched sequence continuously and completed segments flush in order as they arrive, instead of the pool draining and idling at each 48-segment boundary.

**Tech Stack:** Go 1.26 (no CGo), `internal/engine` segment downloader, `internal/config`, `internal/web/routes`, `internal/tui`, `web/public/modules/settings.js`.

## Background — the measurements this is based on (read before Task 1)

Taken 2026-08-15 against a live stream being archived, after the android_vr→WEB client fix removed the 403 storm:

| measurement | result |
|---|---|
| Moombox catch-up, 6 workers | **5.96 MB/s** (1.60 seg/s, avg segment 3.73 MB) |
| one `curl` connection, same URL | **2.86 MB/s** |
| six parallel `curl` connections | **11.28 MB/s** |

So the line sustains at least 11.3 MB/s at the same connection count Moombox uses, and one connection alone is nearly half of Moombox's total. We are neither bandwidth-limited nor per-connection throttled — we are running about 55% efficient at six workers, and six is far below what the connection supports.

Two independent causes, one task group each:

1. **`ParallelDownloads = 6` is a compile-time constant** (`internal/engine/downloader.go:46`) with no way to change it. The setting that *looks* like the knob, `downloader.num_parallel_downloads`, feeds `NewJobQueue` (`internal/worker/worker.go:159`) and controls how many **jobs** run at once — the owner had it at 1000 with no effect on segment throughput.
2. **Catch-up drains its pool at every batch.** `runParallelCatchUp` feeds exactly `targetSeq - curSeq` items, waits for every worker, returns, and `runDashLoop` then runs `probeHeadSequence` before re-entering. Segment writes already stream in order as results arrive (the buffer/flush loop is fine) — the stall is the boundary itself, plus a head probe per batch.

## Global Constraints

- Pure Go, no CGo. Windows is the primary platform.
- The logger interface is an anonymous 4-method interface repeated per struct — do not extract a named one (CLAUDE.md).
- **`segment_workers` has NO upper limit.** Minimum 1; values above `segmentWorkersWarnThreshold` log a warning about bot-detection risk rather than being clamped. The owner asked for this explicitly: a cap is not to be reintroduced.
- **The sequential-write guarantee is inviolable.** Segments must reach `d.outputFile` in strictly ascending order with no holes; a DASH fMP4 with a gap in the middle does not mux. Any restructuring keeps that property.
- Adding a setting follows the project's settings checklist (`.claude/skills/moombox-settings`): config struct → default → validate → API validate → API apply → Web UI → TUI. Missing a surface breaks dual-UI parity.
- After every task: `go build ./... && go vet ./...` clean, `gofmt -l internal/` empty.
- Commit messages end with:
  ```
  Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01R7cRqJNFF17EW9dNZ9af5Z
  ```

## File structure

| File | Change |
|---|---|
| `internal/config/types.go` | `SegmentWorkers int` on the downloader struct |
| `internal/config/config.go` | default 12; validate min 1 + warn-above-threshold |
| `internal/web/routes/jobs.go` | `validateConfigUpdates` + `applyConfigUpdates` entries |
| `web/public/modules/settings.js` | input, populate, gather, dirty-tracking, warning hint |
| `internal/tui/settings.go` | `fieldDef` + load/apply |
| `internal/worker/worker.go` | carry into `JobConfig` |
| `internal/worker/strategy_youtube_*.go` | pass into `DownloaderOptions` |
| `internal/engine/downloader.go` | `SegmentWorkers` option, `segmentWorkers()` accessor, derived limits, byte-ceiling constant |
| `internal/engine/downloader_fetch.go` | transport idle-conn sizing |
| `internal/engine/downloader_parallel.go` | dynamic sizes, byte-bounded buffer, rolling window |
| `internal/engine/downloader_hls.go` | dynamic pool size |
| `docs/spec/*` | document the setting and the distinction from `num_parallel_downloads` |

---

### Task 1: `downloader.segment_workers` setting, all surfaces

**Files:**
- Modify: `internal/config/types.go` (downloader struct, beside `NumParallelDownloads`)
- Modify: `internal/config/config.go` (`Defaults()`, `validate()`)
- Modify: `internal/web/routes/jobs.go` (`validateConfigUpdates`, `applyConfigUpdates`)
- Modify: `web/public/modules/settings.js`
- Modify: `internal/tui/settings.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `config.MoomboxConfig.Downloader.SegmentWorkers int` (toml/json `segment_workers`), default 12, minimum 1, no maximum. `config.SegmentWorkersWarnThreshold = 16` exported for the UIs to reuse in their hint text.

- [ ] **Step 1: Write the failing test**

Append to `internal/config/config_test.go`:

```go
// TestSegmentWorkersValidation pins the owner's explicit requirement: no
// upper clamp. A high value is the operator's call — YouTube may treat a
// large fan-out as bot-like, so it warns, but it must not be silently
// rewritten (a clamp would look like the setting did nothing, which is
// exactly the trap num_parallel_downloads already set).
func TestSegmentWorkersValidation(t *testing.T) {
	tests := []struct {
		name string
		in   int
		want int
	}{
		{"zero falls back to the default", 0, 12},
		{"negative falls back to the default", -4, 12},
		{"one is honoured", 1, 1},
		{"default", 12, 12},
		{"far above the warn threshold is NOT clamped", 256, 256},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Downloader.SegmentWorkers = tc.in
			cfg.validate()
			if got := cfg.Downloader.SegmentWorkers; got != tc.want {
				t.Errorf("SegmentWorkers = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestSegmentWorkersDefault(t *testing.T) {
	if got := Defaults().Downloader.SegmentWorkers; got != 12 {
		t.Errorf("default SegmentWorkers = %d, want 12", got)
	}
}
```

**Implementer note:** `validate()` may be unexported and may take arguments or a logger — read its real signature and adapt the call above; keep the assertions.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestSegmentWorkers -v`
Expected: FAIL — `SegmentWorkers` undefined.

- [ ] **Step 3: Implement config**

`internal/config/types.go`, beside `NumParallelDownloads`:

```go
	// SegmentWorkers is how many segments are fetched CONCURRENTLY within a
	// single download. Distinct from NumParallelDownloads, which is how many
	// jobs run at once — a distinction that cost real debugging time on
	// 2026-08-15, when a value of 1000 there had no effect on catch-up speed.
	//
	// Higher values catch up faster on an in-progress stream (measured: six
	// connections sustained 11.3 MB/s where Moombox managed 5.96 MB/s), at
	// the cost of a wider fan-out to YouTube. There is deliberately no upper
	// limit; past SegmentWorkersWarnThreshold a warning is logged because a
	// large simultaneous fan-out is the kind of traffic shape that attracts
	// bot detection.
	SegmentWorkers int `toml:"segment_workers" json:"segment_workers"`
```

`internal/config/config.go` — in `Defaults()`'s downloader block: `SegmentWorkers: 12,`

Beside the other config constants:

```go
// SegmentWorkersWarnThreshold is the point past which segment_workers is
// reported as risky. Not a cap: the value is honoured as written.
const SegmentWorkersWarnThreshold = 16
```

In `validate()`, mirroring the `NumParallelDownloads` block that precedes it:

```go
	if d.SegmentWorkers < 1 {
		d.SegmentWorkers = defaults.Downloader.SegmentWorkers
	}
```

And, wherever `validate()` can surface a warning (match how the function already reports — if it only mutates, put the warning at the load site that logs), emit:
`"downloader.segment_workers %d is high — a large simultaneous fan-out to YouTube raises bot-detection risk; reduce it if downloads start returning 403"`.

- [ ] **Step 4: API surfaces**

`internal/web/routes/jobs.go` → `validateConfigUpdates`: reject `< 1` with `"downloader.segment_workers": "must be >= 1"`. Do NOT add an upper bound.
→ `applyConfigUpdates`: apply `downloader.segment_workers` from the update map (follow the neighbouring int fields exactly).

- [ ] **Step 5: Web UI**

`web/public/modules/settings.js`: add a `cfg-segment-workers` number input in the downloader section, populate it in `populateConfigForm()`, gather it into the nested snake_case payload in `saveConfig()`, and wire `sl-change`/`sl-input` for dirty tracking. Add help text: *"Segments fetched at once within one download (not the number of concurrent downloads). Higher is faster on catch-up; very high values raise bot-detection risk."* This is NOT a restart-required field.

- [ ] **Step 6: TUI**

`internal/tui/settings.go`: add a `fieldNumber` `fieldDef` in the downloader section, load it in `loadValues()`, apply it in `applyValues()`. Match the surrounding entries' style.

- [ ] **Step 7: Run tests**

Run: `go test ./internal/config/ ./internal/web/routes/ ./internal/tui/ -count=1`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/config/ internal/web/routes/jobs.go web/public/modules/settings.js internal/tui/settings.go
git commit -m "feat(config): add downloader.segment_workers (default 12, uncapped)"
```

---

### Task 2: Thread the worker count into the engine

**Files:**
- Modify: `internal/engine/downloader.go` (options, accessor, derived limits)
- Modify: `internal/engine/downloader_fetch.go:38` (transport idle conns)
- Modify: `internal/engine/downloader_parallel.go` (channel + pool sizing)
- Modify: `internal/engine/downloader_hls.go:576,577,624` (pool sizing)
- Modify: `internal/worker/worker.go` (`JobConfig`), `internal/worker/strategy_youtube_manifestless_dash.go`, `strategy_youtube_dash.go`, `strategy_youtube_hls.go`, `strategy_youtube_vod.go`, `internal/worker/orchestrator_twitch.go`
- Test: `internal/engine/downloader_test.go`

**Interfaces:**
- Consumes: `config.MoomboxConfig.Downloader.SegmentWorkers` (Task 1).
- Produces: `DownloaderOptions.SegmentWorkers int` (0 = use `ParallelDownloads`), `func (d *SegmentDownloader) segmentWorkers() int`, `func (d *SegmentDownloader) maxCatchupBatch() int` (= `8 × segmentWorkers()`), and `JobConfig.SegmentWorkers int`.

**Critical detail — the HTTP transport is a package-level singleton.** `engineHTTPClient` (`downloader_fetch.go:35-41`) sets `MaxIdleConnsPerHost: ParallelDownloads + 2`. If workers exceed that, connections stop being reused and every segment pays a fresh TCP+TLS handshake — which would eat the gain this whole plan is chasing. Since the client is process-wide and built once, size it generously rather than per-download: use a dedicated constant `engineMaxIdleConnsPerHost = 64` with a comment explaining it must comfortably exceed any plausible `segment_workers`, and that exceeding it degrades to new connections rather than failing.

- [ ] **Step 1: Write the failing test**

```go
// TestSegmentWorkersOption pins the fallback contract: an unset option keeps
// the historical ParallelDownloads behaviour, so every downloader that does
// not opt in (Twitch, VOD) is unaffected.
func TestSegmentWorkersOption(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{BaseURL: "https://example.invalid/v", OutputFile: "x"})
	if got := d.segmentWorkers(); got != ParallelDownloads {
		t.Errorf("unset SegmentWorkers = %d, want the ParallelDownloads default %d", got, ParallelDownloads)
	}
	if got := d.maxCatchupBatch(); got != 8*ParallelDownloads {
		t.Errorf("unset maxCatchupBatch = %d, want %d", got, 8*ParallelDownloads)
	}

	d2 := NewSegmentDownloader(DownloaderOptions{BaseURL: "https://example.invalid/v", OutputFile: "x", SegmentWorkers: 20})
	if got := d2.segmentWorkers(); got != 20 {
		t.Errorf("SegmentWorkers = %d, want 20", got)
	}
	if got := d2.maxCatchupBatch(); got != 160 {
		t.Errorf("maxCatchupBatch = %d, want 8*20", got)
	}

	// No upper clamp anywhere in the engine either.
	d3 := NewSegmentDownloader(DownloaderOptions{BaseURL: "https://example.invalid/v", OutputFile: "x", SegmentWorkers: 500})
	if got := d3.segmentWorkers(); got != 500 {
		t.Errorf("SegmentWorkers = %d, want 500 (the engine must not clamp)", got)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/engine/ -run TestSegmentWorkersOption -v` → FAIL (undefined).

- [ ] **Step 3: Implement the engine side**

`DownloaderOptions`:

```go
	// SegmentWorkers is how many segments this download fetches
	// concurrently. Zero means ParallelDownloads, preserving the historical
	// behaviour for callers that do not set it. No upper limit is enforced —
	// see config.SegmentWorkersWarnThreshold for why that is deliberate.
	SegmentWorkers int
```

Accessors on `*SegmentDownloader`:

```go
// segmentWorkers is the operative concurrency for this download.
func (d *SegmentDownloader) segmentWorkers() int {
	if d.opts.SegmentWorkers > 0 {
		return d.opts.SegmentWorkers
	}
	return ParallelDownloads
}

// maxCatchupBatch is the per-call catch-up ceiling, derived from the
// operative worker count so a wider pool gets a proportionally wider window
// instead of starving against a fixed 48.
func (d *SegmentDownloader) maxCatchupBatch() int {
	return 8 * d.segmentWorkers()
}
```

Replace the package-level `maxCatchupBatch` constant and every use (`downloader_parallel.go` batch bound, `catchUpBatchLimit`'s cap and its full-recovery threshold, `catchUpDampedFloor`) with the methods. `catchUpDampedFloor` becomes `d.segmentWorkers()` — its meaning is "one full parallel wave", which is now dynamic.

Replace `ParallelDownloads` with `d.segmentWorkers()` at: `downloader_parallel.go:53,54,66` and `downloader_hls.go:576,577,624`.

Transport: replace `MaxIdleConnsPerHost: ParallelDownloads + 2` with `engineMaxIdleConnsPerHost` (64) and the comment described above.

- [ ] **Step 4: Plumb from config**

`internal/worker/worker.go`: add `SegmentWorkers int` to `JobConfig` and populate it from `cfg.Downloader.SegmentWorkers` where `MaximumTimeout` is populated (~:704, ~:755).

In each strategy that builds `engine.DownloaderOptions` — `strategy_youtube_manifestless_dash.go` (video + audio), `strategy_youtube_dash.go` (video + audio), `strategy_youtube_hls.go`, `strategy_youtube_vod.go` (video + audio), `orchestrator_twitch.go` — add `SegmentWorkers: job.Config.SegmentWorkers,`. Twitch and VOD included: they fetch segments too and the setting is described to users as applying to downloads generally.

- [ ] **Step 5: Run tests**

Run: `go build ./... && go test ./internal/engine/ ./internal/worker/ -count=1` → PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/engine/ internal/worker/
git commit -m "feat(engine): make segment concurrency configurable per download"
```

---

### Task 3: Byte-bounded reorder buffer

**Files:**
- Modify: `internal/engine/downloader.go` (buffer ceiling constant)
- Modify: `internal/engine/downloader_parallel.go` (buffer accounting)
- Test: `internal/engine/downloader_parallel_test.go`

**Why:** the catch-up reorder buffer holds out-of-order segments until the missing lower sequence arrives. Its only bound today is `segmentWorkers × 3` *segments*; at the 3.7–6.2 MB segments measured on a 1080p60 live stream that is ~90 MB at six workers and would become ~250 MB at sixteen — RAM growing with a throughput setting, in a process that runs 24/7. Bounding by bytes means raising workers costs connections, not memory: when the buffer is full, workers wait.

**Interfaces:**
- Produces: `catchUpBufferBytes = 256 << 20` (256 MB) and buffer accounting inside `runParallelCatchUp` that blocks result acceptance while the ceiling is exceeded.

- [ ] **Step 1: Write the failing test**

```go
// TestCatchUpBufferByteCeiling pins that the reorder buffer is bounded by
// BYTES, not segment count: raising SegmentWorkers must not multiply memory.
// The head segment is withheld so nothing can flush, forcing every arriving
// segment to accumulate.
func TestCatchUpBufferByteCeiling(t *testing.T) {
	// … construct a downloader with a large SegmentWorkers against a fake
	// GVS that serves multi-MB segments but stalls seq == curSeq, then assert
	// that resident buffered bytes never exceed catchUpBufferBytes …
}
```

**Implementer note:** this one is genuinely awkward to test at the seam — the buffer is a local inside `runParallelCatchUp`. Prefer extracting the accounting into a tiny helper type (e.g. `type reorderBuffer struct { seg map[int][]byte; bytes, limit int }` with `add`/`take`/`full` methods) and unit-test THAT directly, rather than contorting the loop. If you conclude an honest test is impractical, say so in your report rather than writing one that asserts nothing.

- [ ] **Step 2: Run to verify it fails**

- [ ] **Step 3: Implement**

Add beside the other engine constants:

```go
	// catchUpBufferBytes caps the RAM held by catch-up's out-of-order reorder
	// buffer. The buffer was previously bounded only by segment COUNT
	// (segmentWorkers*3), so memory scaled with a throughput setting: at the
	// 3.7-6.2 MB segments of a 1080p60 live stream, sixteen workers would
	// hold ~250 MB. Bounding by bytes means a wider pool costs connections,
	// not memory — workers simply wait for the head segment to land.
	catchUpBufferBytes = 256 << 20
```

In `runParallelCatchUp`, track resident bytes as segments enter and leave the buffer, and stop consuming from `results` (or have workers block before sending) once the ceiling is reached, resuming after a flush. The head segment must always be admittable, or a full buffer deadlocks: never refuse the segment equal to `nextSeq`.

- [ ] **Step 4: Run tests** — `go test ./internal/engine/ -count=1` PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/
git commit -m "feat(engine): bound the catch-up reorder buffer by bytes"
```

---

### Task 4: Rolling-window catch-up

**Files:**
- Modify: `internal/engine/downloader_parallel.go` (`runParallelCatchUp`)
- Modify: `internal/engine/downloader_dash.go` (per-batch head probe)
- Test: `internal/engine/downloader_parallel_test.go`, `internal/engine/downloader_dash_integration_test.go`

**The defect:** `runParallelCatchUp` feeds exactly one batch, waits for every worker (`wg.Wait`), returns, and `runDashLoop` runs `probeHeadSequence` before re-entering. Every boundary drains the pool — the slowest segment in a batch idles all other workers — and adds a head round trip. Measured effect: ~55% efficiency at six workers.

**The shape:** keep one worker pool alive across the whole catch-up span. Workers claim the next sequence from a shared counter and push results; the consumer flushes in ascending order as they arrive (the existing flush loop already does this correctly — preserve it). Refill the claim window as segments are written, so a worker never waits for a batch boundary. Return when the window reaches the target, when the head segment fails permanently (the gap contract), or on cancellation.

**Invariants that must not change:**
- Segments reach `d.outputFile` strictly in ascending order, no holes.
- On a gap, return `nextSeq` pointing AT the missing segment, and emit the same `OnGap` / `stopping at gap` reporting so the sequential loop's recovery still engages.
- `OnProgress` still emits with `CatchingUp: true` and last-WRITTEN seq semantics.
- The byte ceiling from Task 3 still bounds resident memory.
- Head refresh must still happen — but drive it off the head harvested from segment responses (`noteHeadSeqFromResponse` already does this on every fetch) plus the existing `HeadProbeInterval` pacing, rather than one probe per batch.

- [ ] **Step 1: Write the failing test**

A throughput-shaped test rather than a timing-flaky one: using the existing `fakeGVS` harness, serve N segments where each fetch sleeps a fixed duration, and assert the total wall clock is close to `N/workers × delay` rather than the batch-quantised `ceil(N/batch) × ...`. Pick numbers with a wide margin (e.g. assert < 60% of the batched-model time) so it fails on the old structure but cannot flake on a busy machine. Read `newFakeGVS`'s real signature first.

- [ ] **Step 2: Run it against the current code** — must FAIL. Report the observed timing both before and after.

- [ ] **Step 3: Implement the rolling window**

- [ ] **Step 4: Verify the invariants**

Beyond the new test, these existing tests pin the contracts above and must stay green: `downloader_dash_integration_test.go` (whole-loop behaviour incl. the credential-recovery scenario), `downloader_parallel_test.go`, `downloader_dash_headseq_test.go`, `downloader_dash_activity_test.go`. Run the whole engine package.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/
git commit -m "perf(engine): rolling-window catch-up instead of per-batch barriers"
```

---

### Task 5: Docs and full verification

**Files:**
- Modify: `docs/spec/data-and-storage.md` (config reference) and/or `docs/spec/architecture.md` — wherever downloader settings are documented; grep for `num_parallel_downloads` and document `segment_workers` beside it.

- [ ] **Step 1: Document the setting and the distinction**

State plainly that `num_parallel_downloads` = concurrent **jobs** and `segment_workers` = concurrent **segments within one download**; that `segment_workers` has no upper limit and warns above 16 because a wide fan-out attracts bot detection; and record the measurements from this plan's Background so the default's rationale survives.

- [ ] **Step 2: Full verification**

```bash
cd bgutil-sidecar && node build.mjs && cd ..
go build ./... && go vet ./... && gofmt -l internal/
go test ./... -count=1
```

- [ ] **Step 3: Commit**

```bash
git add docs/
git commit -m "docs: document segment_workers and its distinction from num_parallel_downloads"
```

---

## Field verification (after merge)

Re-run the measurement from the Background against an in-progress live stream: expect catch-up well above 5.96 MB/s at the default 12 workers, and a `gapFrom`/`gapTo` cadence without the per-batch stalls. If throughput does not scale with `segment_workers`, the next suspect is the HTTP transport's idle-connection pool (Task 2's `engineMaxIdleConnsPerHost`) — a too-small pool shows up as per-segment TLS handshakes rather than as errors.

Watch for 403s returning at high worker counts: that is the bot-detection risk the warning exists for, and it would show up as `[Downloader] credentials refreshed after 403` lines reappearing.
