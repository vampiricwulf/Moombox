# Live Segment 403 Credential Recovery Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A live download that starts getting 403s on segments that demonstrably exist must refresh its credentials in-process and keep going, instead of declaring those segments permanently gone and stalling forever.

**Architecture:** Port the recovery shape both upstreams converged on. The engine gains a mutable PO token (mirroring the existing atomic `BaseURL` override) and a `OnCredentialRefresh` callback that re-resolves the format URL *and* re-mints the GVS PO token from a fresh player response. A 403 burst while behind head fires that callback (cooldown-gated), retries the segment, and only falls back to today's `ErrSegmentPermanent` behaviour once refreshed credentials have also failed. 410, and 403 at-or-past head, keep their current end-of-stream meaning untouched.

**Tech Stack:** Go 1.26 (no CGo), `internal/engine` segment downloader, `internal/worker` strategies, `internal/bgutils` PO-token provider.

## Background — why this exists (read before Task 1)

On 2026-08-15, archiving an **already-in-progress** live stream stalled every time:

- Video wrote segments 0–95, audio 0–95, then every subsequent segment returned 403. The stream's head was at **2776** — those segments existed.
- The job never recovered on its own. Cancelling and resuming (which mints a **fresh PO token and fresh URL**) resumed downloading immediately, for another ~60–100 segments, then stalled the same way. Repeated across four runs: video `lastSeq` 95 → 287, file 41 MB → 131 MB. **The 403s are transient; only our handling is permanent.**
- Reproduced on `moombox-baseline.exe` built from `9a8f415` — pre-dating all recent POT work — so this is **not** a regression from the attestation/hardening changes. It is a latent bug that only a mid-stream join exercises. Every historical capture joined at go-live, where the sequential path keeps pace and catch-up never runs (all five rotated logs contain zero `stopping at gap` lines).

Root cause, in code: `internal/engine/downloader_fetch.go:204`

```go
if status == 403 || status == 410 {
    return nil, ErrSegmentPermanent   // "gone for good. Don't retry."
}
```

403 gets zero retries and zero backoff, and the only repair wired to a 403 burst (`OnCipherFailure`) returns a fresh **URL** while `opts.PoToken` stays fixed for the downloader's entire life.

### What upstream does (the design being ported)

**moonarchive** (`references/moonarchive/src/moonarchive/downloaders/youtube/_dash.py`) — its `frag_iterator`:
1. On 403 sets `fragment_access_expired = True`, whose only effect is to skip the heartbeat branch and fall through to `_get_web_player_response(video_id)` — *"stream access expired? retrieve a fresh manifest"* — then retry the same sequence.
2. If the **same** sequence 403s again with fresh credentials **and** `max_seq - 2 > cur_seq` (still behind head), it advances past that one fragment: *"there are instances when YouTube repeatedly responds with a 403 on a fragment that should be valid. Skip it."* Caught up, a repeat instead ends the download.
3. Damps request rate after trouble: `batch_count = min(1 + int(time_since_check / 10), batch_count)`.

**yt-dlp** (`references/yt-dlp/yt_dlp/extractor/youtube/_video.py` + `downloader/fragment.py`):
1. Every live format carries a `url_feed` callback that re-fetches the player response and returns a fresh base URL for that itag.
2. The refresh is failure-driven with a cooldown: `url_feed(itag, client_name, 5 if no_fragment_score > 15 else 18000)` — the third argument is the minimum age before refetching, so a healthy download effectively never refreshes and a failing one refreshes every 5 s.
3. `RetryManager(fragment_retries)` (default **10**) retries each fragment on any `HTTPError`, 403 included; on exhaustion `report_skip_fragment` skips it and continues.

Neither treats 403 as terminal, and neither refreshes only the URL — both go back to the **player response**, which is what also produces fresh PO-token context.

## Global Constraints

- Pure Go, no CGo. Windows is the primary platform.
- The logger interface is an anonymous 4-method interface repeated per struct — do not extract a named one (CLAUDE.md). `engine.DownloaderLogger` is already named; leave it.
- **Do not change what 410 means.** 410 → `ErrSegmentPermanent`, always, no retry, no refresh.
- **Do not change end-of-stream detection.** A 403 at or past head must still terminate a finished stream: VOD and post-live finalization depend on it (`handleGoneError`, `behindHeadTailPending`). Recovery is gated on `behindHeadTailPending()` being true.
- After every task: `go build ./... && go vet ./...` clean, `gofmt -l internal/` empty.
- Commit messages end with:
  ```
  Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01R7cRqJNFF17EW9dNZ9af5Z
  ```

## File structure

| File | Responsibility | Change |
|---|---|---|
| `internal/engine/downloader.go` | Downloader state + options | Add `poTokenOverride` atomic + `SetPoToken`/`getPoToken`; add `OnCredentialRefresh` option; add refresh-cooldown state |
| `internal/engine/downloader_fetch.go` | Segment fetch + retry classification | Read token via `getPoToken()`; give 403 a credential-refresh + retry path before `ErrSegmentPermanent` |
| `internal/engine/downloader_dash.go` | DASH loop, gone handling | Fire `OnCredentialRefresh` on a behind-head 403 burst |
| `internal/engine/downloader_parallel.go` | Parallel catch-up | Damp batch size after a failure episode; surface skip-one |
| `internal/worker/strategy_youtube_manifestless_dash.go` | Live strategy wiring | Supply `OnCredentialRefresh` (fresh URL + re-minted POT) |
| `internal/worker/strategies.go` | Shared strategy helpers | `refreshGvsCredentials` helper used by the strategies |

---

### Task 1: Mutable PO token in the engine

**Files:**
- Modify: `internal/engine/downloader.go` (near `baseURLOverride` at :274 and `SetBaseURL` at :316)
- Modify: `internal/engine/downloader_fetch.go:131`, `:343`
- Modify: `internal/engine/eviction_probe.go:119`
- Test: `internal/engine/downloader_test.go`

**Interfaces:**
- Produces: `func (d *SegmentDownloader) SetPoToken(token string)` and `func (d *SegmentDownloader) getPoToken() string`. `getPoToken` returns the override when set, else `d.opts.PoToken`.

- [ ] **Step 1: Write the failing test**

Append to `internal/engine/downloader_test.go`:

```go
// TestSetPoTokenOverridesOptions pins the mutable-token seam that credential
// recovery depends on: opts.PoToken is the initial value only, and a later
// SetPoToken must be what subsequent fetches use. Mirrors SetBaseURL.
func TestSetPoTokenOverridesOptions(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    "https://example.invalid/v",
		OutputFile: "x",
		PoToken:    "initial",
	})
	if got := d.getPoToken(); got != "initial" {
		t.Fatalf("getPoToken() = %q, want the options value %q", got, "initial")
	}
	d.SetPoToken("refreshed")
	if got := d.getPoToken(); got != "refreshed" {
		t.Errorf("getPoToken() = %q, want %q after SetPoToken", got, "refreshed")
	}
	// An empty refresh must not blank a working token — a failed re-mint
	// returns "" and must leave the existing credential in place.
	d.SetPoToken("")
	if got := d.getPoToken(); got != "refreshed" {
		t.Errorf("getPoToken() = %q, want the previous token retained after an empty SetPoToken", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/engine/ -run TestSetPoTokenOverridesOptions -v`
Expected: FAIL — compile error, `d.getPoToken` and `d.SetPoToken` undefined.

- [ ] **Step 3: Implement**

In `internal/engine/downloader.go`, beside `baseURLOverride atomic.Pointer[string]` in the struct:

```go
	// poTokenOverride carries a re-minted GVS PO token. opts.PoToken is the
	// value the downloader STARTED with; credential recovery replaces it in
	// place rather than tearing the downloader down (both upstreams refresh
	// credentials inside the segment loop — see the plan's background).
	// Mirrors baseURLOverride exactly.
	poTokenOverride atomic.Pointer[string]
```

Beside `SetBaseURL`:

```go
// SetPoToken atomically replaces the PO token used for subsequent segment
// fetches. An empty token is ignored: a failed re-mint must not blank a
// credential that is still working. Nothing in flight is interrupted — the
// swap is visible to the next getPoToken() call.
func (d *SegmentDownloader) SetPoToken(token string) {
	if token == "" {
		return
	}
	d.poTokenOverride.Store(&token)
}

// getPoToken returns the current PO token: the refreshed override when one
// has been installed, otherwise the token the downloader was constructed
// with.
func (d *SegmentDownloader) getPoToken() string {
	if p := d.poTokenOverride.Load(); p != nil {
		return *p
	}
	return d.opts.PoToken
}
```

Replace all three read sites (`downloader_fetch.go:131`, `downloader_fetch.go:343`, `eviction_probe.go:119`): `d.opts.PoToken` → `d.getPoToken()`.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/engine/ -run "TestSetPoToken|TestSetBaseURL" -count=1 -v`
Expected: PASS. Then `grep -rn "opts.PoToken" internal/engine/ | grep -v _test` must show only the `getPoToken` fallback line.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/downloader.go internal/engine/downloader_fetch.go internal/engine/eviction_probe.go internal/engine/downloader_test.go
git commit -m "feat(engine): make the segment PO token replaceable in place"
```

---

### Task 2: `OnCredentialRefresh` option + cooldown state

**Files:**
- Modify: `internal/engine/downloader.go` (options block ending at :303, struct state near :254)
- Test: `internal/engine/downloader_test.go`

**Interfaces:**
- Consumes: `SetPoToken` / `SetBaseURL` (Task 1).
- Produces:
  ```go
  // In DownloaderOptions:
  OnCredentialRefresh func() (baseURL string, poToken string)
  ```
  plus `func (d *SegmentDownloader) refreshCredentials() bool` — fires the callback at most once per `credentialRefreshCooldown`, installs whatever non-empty values come back, and reports whether anything was installed.

- [ ] **Step 1: Write the failing test**

```go
// TestRefreshCredentialsInstallsAndCoolsDown pins the two properties the 403
// recovery path relies on: a refresh installs whatever the callback returns,
// and repeated 403s inside the cooldown window cannot turn the callback into
// a hot loop (yt-dlp gates its url_feed refetch the same way).
func TestRefreshCredentialsInstallsAndCoolsDown(t *testing.T) {
	var calls int
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    "https://example.invalid/old",
		OutputFile: "x",
		PoToken:    "old-token",
		OnCredentialRefresh: func() (string, string) {
			calls++
			return "https://example.invalid/new", "new-token"
		},
	})

	if !d.refreshCredentials() {
		t.Fatal("first refresh should report that credentials were installed")
	}
	if calls != 1 {
		t.Fatalf("callback calls = %d, want 1", calls)
	}
	if got := d.getPoToken(); got != "new-token" {
		t.Errorf("token = %q, want new-token", got)
	}
	if got := d.getBaseURL(); got != "https://example.invalid/new" {
		t.Errorf("baseURL = %q, want the refreshed URL", got)
	}

	// Second call inside the cooldown must not re-invoke the callback.
	if d.refreshCredentials() {
		t.Error("refresh inside the cooldown window should report no new install")
	}
	if calls != 1 {
		t.Errorf("callback calls = %d, want still 1 (cooldown)", calls)
	}
}

// TestRefreshCredentialsNilCallback: downloaders without the callback (Twitch
// HLS, VOD) must be unaffected.
func TestRefreshCredentialsNilCallback(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{BaseURL: "https://example.invalid/v", OutputFile: "x"})
	if d.refreshCredentials() {
		t.Error("refreshCredentials with no callback should report false")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/engine/ -run TestRefreshCredentials -v`
Expected: FAIL — `OnCredentialRefresh` and `refreshCredentials` undefined.

- [ ] **Step 3: Implement**

Add the constant next to the other DASH timings in `internal/engine/downloader_dash.go`:

```go
	// credentialRefreshCooldown is the minimum gap between two
	// OnCredentialRefresh invocations. Each refresh costs a player-response
	// round trip plus a PO-token mint, and a 403 burst produces hundreds of
	// failures per second, so without this the recovery path would hammer
	// YouTube harder than the failure did. yt-dlp gates its equivalent
	// (url_feed's delay argument) at 5s once fragments start failing.
	credentialRefreshCooldown = 5 * time.Second
```

In `DownloaderOptions` (after `OnCipherFailure`):

```go
	// OnCredentialRefresh is called when segments 403 while the downloader is
	// still behind the live head — i.e. the segments demonstrably exist and
	// our credentials, not the stream, are the problem. The callback should
	// re-fetch the player response and return a freshly-resolved BaseURL and
	// a freshly-minted GVS PO token; either may be "" to leave that half
	// unchanged. Both upstreams recover this way rather than treating the
	// 403 as terminal (see docs/superpowers/plans/2026-08-15-live-403-
	// credential-recovery.md). Optional; nil disables recovery, restoring
	// the previous behaviour exactly.
	OnCredentialRefresh func() (baseURL string, poToken string)
```

In the downloader struct, beside the other timestamps:

```go
	// lastCredentialRefresh gates OnCredentialRefresh to one call per
	// credentialRefreshCooldown.
	lastCredentialRefresh atomicTime
```

Use the existing `atomicTime` helper (`downloader.go:222`): `Store(time.Time)`, `StoreNow()`, `Since() time.Duration`. Verified: its zero value Loads as the zero time, so `Since()` on a never-set field returns a multi-century duration — the first call therefore always passes the cooldown gate, with no initialisation needed.

Method:

```go
// refreshCredentials asks the owner for fresh download credentials and
// installs whatever it returns. Returns true when a refresh actually ran and
// installed at least one value. Cooldown-gated: a 403 burst can call this on
// every failed segment, but only one player-response round trip per
// credentialRefreshCooldown is allowed through.
func (d *SegmentDownloader) refreshCredentials() bool {
	if d.opts.OnCredentialRefresh == nil {
		return false
	}
	if d.lastCredentialRefresh.Since() < credentialRefreshCooldown {
		return false
	}
	d.lastCredentialRefresh.StoreNow()

	freshURL, freshToken := d.opts.OnCredentialRefresh()
	installed := false
	if freshURL != "" {
		d.SetBaseURL(freshURL)
		installed = true
	}
	if freshToken != "" {
		d.SetPoToken(freshToken)
		installed = true
	}
	if installed && d.logger != nil {
		d.logger.Info("[Downloader] credentials refreshed after 403",
			"newURL", freshURL != "", "newToken", freshToken != "")
	}
	return installed
}
```

Verified: `lastCredentialRefresh` starts at the zero time and `atomicTime.Since()` returns `time.Since(time.Time{})` — a multi-century duration — so the first call always passes the gate.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/engine/ -run TestRefreshCredentials -count=1 -v`
Expected: PASS (both tests).

- [ ] **Step 5: Commit**

```bash
git add internal/engine/downloader.go internal/engine/downloader_dash.go internal/engine/downloader_test.go
git commit -m "feat(engine): add cooldown-gated OnCredentialRefresh seam"
```

---

### Task 3: Retry a behind-head 403 after refreshing credentials

**Files:**
- Modify: `internal/engine/downloader_fetch.go:186-220` (`fetchSegmentWithRetry`)
- Test: `internal/engine/downloader_fetch_403_test.go` (create)

**Interfaces:**
- Consumes: `refreshCredentials()` (Task 2), `behindHeadTailPending()` (existing, `downloader_dash.go`).
- Produces: no new exported surface. `fetchSegmentWithRetry` still returns `ErrSegmentPermanent` when recovery fails, so every existing caller and all end-of-stream logic keeps working unchanged.

**Critical constraint:** 410 keeps returning `ErrSegmentPermanent` immediately. A 403 only earns a retry when `d.behindHeadTailPending()` is true — segments below the harvested head demonstrably exist, so a 403 there is a credential problem. At or past head, 403 keeps meaning end-of-stream and returns `ErrSegmentPermanent` on the spot.

- [ ] **Step 1: Write the failing test**

Create `internal/engine/downloader_fetch_403_test.go`:

```go
package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
)

// TestForbiddenBehindHeadRefreshesAndRetries is the regression test for the
// 2026-08-15 live stall: segments that exist (currentSeq well below the
// harvested head) started 403ing, and the downloader declared them
// permanently gone with zero retries, so the recording stopped forever while
// a manual restart with fresh credentials resumed it instantly.
func TestForbiddenBehindHeadRefreshesAndRetries(t *testing.T) {
	var refreshed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Head-Seqnum", "5000")
		// The token only becomes acceptable after a refresh.
		if r.URL.Query().Get("pot") == "fresh" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("segment-bytes"))
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/v?x=1",
		OutputFile: "x",
		PoToken:    "stale",
		OnCredentialRefresh: func() (string, string) {
			refreshed.Store(true)
			return "", "fresh"
		},
	})
	// Behind head: head 5000, current 10.
	d.noteHeadSeq(5000)
	d.currentSeq.Store(10)

	body, err := d.fetchSegmentWithRetry(context.Background(), d.buildSegmentURL(10))
	if err != nil {
		t.Fatalf("fetch failed after refresh: %v", err)
	}
	if string(body) != "segment-bytes" {
		t.Errorf("body = %q, want the segment", string(body))
	}
	if !refreshed.Load() {
		t.Error("OnCredentialRefresh was never called")
	}
}

// TestForbiddenAtHeadStaysPermanent guards end-of-stream detection: a 403 at
// or past the head is how a finished stream terminates, and VOD/post-live
// finalization depends on it. Recovery must not touch that case.
func TestForbiddenAtHeadStaysPermanent(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("X-Head-Seqnum", "100")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	var refreshCalls atomic.Int32
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/v?x=1",
		OutputFile: "x",
		PoToken:    "stale",
		OnCredentialRefresh: func() (string, string) {
			refreshCalls.Add(1)
			return "", "fresh"
		},
	})
	d.noteHeadSeq(100)
	d.currentSeq.Store(101) // past head — this is the end of the stream

	_, err := d.fetchSegmentWithRetry(context.Background(), d.buildSegmentURL(101))
	if err != ErrSegmentPermanent {
		t.Fatalf("err = %v, want ErrSegmentPermanent at head", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("HTTP attempts = %d, want exactly 1 (no retry past head)", got)
	}
	if got := refreshCalls.Load(); got != 0 {
		t.Errorf("refresh calls = %d, want 0 past head", got)
	}
}

// TestGoneAlwaysPermanent: 410 means the segment is really evicted. It must
// never trigger a refresh or a retry regardless of head position.
func TestGoneAlwaysPermanent(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("X-Head-Seqnum", "5000")
		w.WriteHeader(http.StatusGone)
	}))
	defer srv.Close()

	var refreshCalls atomic.Int32
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/v?x=1",
		OutputFile: "x",
		OnCredentialRefresh: func() (string, string) {
			refreshCalls.Add(1)
			return "", "fresh"
		},
	})
	d.noteHeadSeq(5000)
	d.currentSeq.Store(10)

	if _, err := d.fetchSegmentWithRetry(context.Background(), d.buildSegmentURL(10)); err != ErrSegmentPermanent {
		t.Fatalf("err = %v, want ErrSegmentPermanent for 410", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("HTTP attempts = %d, want exactly 1 for 410", got)
	}
	if got := refreshCalls.Load(); got != 0 {
		t.Errorf("refresh calls = %d, want 0 for 410", got)
	}
}

var _ = strconv.Itoa // keep strconv imported if unused after edits
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/engine/ -run "TestForbidden|TestGoneAlways" -v`
Expected: `TestForbiddenBehindHeadRefreshesAndRetries` FAILS (returns `ErrSegmentPermanent`, refresh never called). The other two should already pass — they pin current behaviour that must survive.

- [ ] **Step 3: Implement**

Replace the classification block in `fetchSegmentWithRetry` (`downloader_fetch.go`, currently lines 203-206):

```go
		if status == 410 {
			// Genuinely evicted. Never retried, never refreshed.
			return nil, ErrSegmentPermanent
		}
		if status == 403 {
			// 403 is overloaded. Past the head it is how a finished stream
			// signals "no such segment" and MUST stay terminal — VOD and
			// post-live finalization are built on it. BELOW the head the
			// segment demonstrably exists (X-Head-Seqnum told us so), so a
			// 403 there means our credentials went stale, which is exactly
			// what a manual cancel-and-resume fixes. Refresh them and retry
			// rather than declaring live segments permanently gone (the
			// 2026-08-15 mid-stream-join stall).
			if !d.behindHeadTailPending() {
				return nil, ErrSegmentPermanent
			}
			if attempt >= forbiddenRefreshAttempts-1 {
				// Out of refresh attempts for this segment — report it the
				// way we always did so no caller hangs. The caller re-attempts
				// the segment on its next pass, by which point the cooldown
				// may have allowed another refresh.
				return nil, ErrSegmentPermanent
			}
			// Best-effort: a false return means the cooldown is still open or
			// no callback is installed. Retry anyway — a refresh fired by the
			// previous failing segment may already have installed working
			// credentials that this attempt will pick up.
			d.refreshCredentials()
			d.emitActivity(ActivityRetrying)
			utils.Sleep(ctx, singleGoneRetryDelay)
			continue
		}
```

Add beside the other DASH constants in `downloader_dash.go`:

```go
	// forbiddenRefreshAttempts bounds how many times one segment is retried
	// through a credential refresh before it is reported permanently gone.
	// Kept small: the refresh itself is cooldown-gated, so a higher number
	// mostly buys sleep, and the caller (catch-up or the sequential loop)
	// re-attempts the segment on its next pass anyway.
	forbiddenRefreshAttempts = 3
```

Note for the implementer: `fetchSegmentWithRetry` loops `for attempt := range d.opts.MaxRetries` (default 5), so `attempt` is in scope. `behindHeadTailPending` lives in `downloader_dash.go` — read it before use and confirm it needs no arguments and does not itself sleep.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/engine/ -count=1`
Expected: all three new tests PASS, and the whole package stays green — particularly `downloader_dash_integration_test.go`, `downloader_dash_headseq_test.go` and `downloader_dash_activity_test.go`, which pin end-of-stream behaviour.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/downloader_fetch.go internal/engine/downloader_dash.go internal/engine/downloader_fetch_403_test.go
git commit -m "fix(engine): refresh credentials and retry a behind-head 403 instead of giving up"
```

---

### Task 4: Damp the catch-up batch after a failure episode

**Files:**
- Modify: `internal/engine/downloader_parallel.go:12-30` (batch sizing in `runParallelCatchUp`)
- Modify: `internal/engine/downloader.go` (episode timestamp state)
- Test: `internal/engine/downloader_parallel_test.go` (create)

**Interfaces:**
- Consumes: nothing from Tasks 1-3 beyond the downloader itself.
- Produces: `func (d *SegmentDownloader) catchUpBatchLimit() int` — the current per-call batch ceiling, `maxCatchupBatch` normally, shrunk after a recent failure episode.

**Why:** during the stall the downloader fired the full 48-wide batch into the wall hundreds of times a second. moonarchive shrinks its batch after trouble and regrows it (`batch_count = min(1 + time_since_check/10, batch_count)`); this is that behaviour.

- [ ] **Step 1: Write the failing test**

Create `internal/engine/downloader_parallel_test.go`:

```go
package engine

import (
	"testing"
	"time"
)

// TestCatchUpBatchLimitDamping ports moonarchive's post-failure throttle: a
// fresh downloader may use the full batch, a downloader that just hit a
// failure episode drops to a small batch, and the ceiling regrows with time
// so a single blip doesn't cripple catch-up for the rest of the archive.
func TestCatchUpBatchLimitDamping(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{BaseURL: "https://example.invalid/v", OutputFile: "x"})

	if got := d.catchUpBatchLimit(); got != maxCatchupBatch {
		t.Errorf("fresh limit = %d, want the full %d", got, maxCatchupBatch)
	}

	d.noteCatchUpFailureEpisode()
	got := d.catchUpBatchLimit()
	if got != 1 {
		t.Errorf("limit immediately after a failure = %d, want 1", got)
	}

	// 30s later the ceiling should have regrown (1 per 10s) but not to full.
	d.lastCatchUpFailure.Store(time.Now().Add(-30 * time.Second))
	got = d.catchUpBatchLimit()
	if got != 4 {
		t.Errorf("limit 30s after a failure = %d, want 4", got)
	}

	// Long after, the full batch is available again.
	d.lastCatchUpFailure.Store(time.Now().Add(-2 * time.Hour))
	if got := d.catchUpBatchLimit(); got != maxCatchupBatch {
		t.Errorf("limit long after a failure = %d, want the full %d", got, maxCatchupBatch)
	}
}
```

`atomicTime.Store` takes a `time.Time` (verified at `downloader.go:224`), so these two lines compile as written.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/engine/ -run TestCatchUpBatchLimitDamping -v`
Expected: FAIL — `catchUpBatchLimit`, `noteCatchUpFailureEpisode`, `lastCatchUpFailure` undefined.

- [ ] **Step 3: Implement**

In the downloader struct:

```go
	// lastCatchUpFailure timestamps the most recent catch-up failure episode.
	// The batch ceiling is throttled relative to it so a 403 burst stops
	// being answered with 48 simultaneous requests (moonarchive damps its
	// batch the same way and regrows it over time).
	lastCatchUpFailure atomicTime
```

Methods in `downloader_parallel.go`:

```go
// noteCatchUpFailureEpisode records that catch-up just hit failures, which
// throttles the next batches via catchUpBatchLimit.
func (d *SegmentDownloader) noteCatchUpFailureEpisode() {
	d.lastCatchUpFailure.StoreNow()
}

// catchUpBatchLimit is the per-call segment ceiling for parallel catch-up:
// the full maxCatchupBatch normally, and a throttled value that regrows by 1
// every catchUpRegrowInterval after a failure episode. Mirrors moonarchive's
// batch_count = min(1 + time_since_check/10, batch_count).
func (d *SegmentDownloader) catchUpBatchLimit() int {
	since := d.lastCatchUpFailure.Since()
	if since >= time.Duration(maxCatchupBatch)*catchUpRegrowInterval {
		return maxCatchupBatch
	}
	limit := 1 + int(since/catchUpRegrowInterval)
	return min(limit, maxCatchupBatch)
}
```

Constant beside `maxCatchupBatch` in `downloader.go`:

```go
	// catchUpRegrowInterval is how much elapsed time restores one segment of
	// catch-up batch width after a failure episode.
	catchUpRegrowInterval = 10 * time.Second
```

In `runParallelCatchUp`, replace the batch bound:

```go
	targetSeq = min(targetSeq, curSeq+d.catchUpBatchLimit())
```

And in the worker's error switch (`downloader_parallel.go`, the `ErrSegmentPermanent` / `ErrSegmentRetriesExhausted` cases), call `d.noteCatchUpFailureEpisode()` so an episode is recorded exactly once per failing segment:

```go
				switch {
				case errors.Is(fetchErr, ErrSegmentPermanent):
					d.noteCatchUpFailureEpisode()
					d.logger.Debug("[Downloader] catch-up segment permanently gone (403/410)",
						"seq", item.seq)
				case errors.Is(fetchErr, ErrSegmentRetriesExhausted):
					d.noteCatchUpFailureEpisode()
					d.logger.Debug("[Downloader] catch-up segment retries exhausted",
						"seq", item.seq)
				}
```

Verified: a zero `lastCatchUpFailure` yields `Since()` of a multi-century duration, which exceeds `maxCatchupBatch * catchUpRegrowInterval` (48 x 10s = 8m), so a downloader that has never failed gets the full batch with no initialisation.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/engine/ -count=1`
Expected: PASS, whole package green.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/downloader.go internal/engine/downloader_parallel.go internal/engine/downloader_parallel_test.go
git commit -m "feat(engine): throttle catch-up batches after a failure episode"
```

---

### Task 5: Wire the refresh callback in the live strategy

**Files:**
- Modify: `internal/worker/strategies.go` (new helper next to `invalidate403Caches`)
- Modify: `internal/worker/strategy_youtube_manifestless_dash.go` (both `DownloaderOptions` literals, video ~:267 and audio ~:321, plus their existing `OnCipherFailure` blocks)
- Test: `internal/worker/strategies_test.go` (append; create if absent)

**Interfaces:**
- Consumes: `engine.DownloaderOptions.OnCredentialRefresh` (Task 2).
- Produces:
  ```go
  func refreshGvsCredentials(ctx context.Context, job *JobContext, videoInfo *youtube.VideoInfo,
      itag int, routedSolver cipher.Solver, cipherSolver *cipher.GojaResolver,
      potProvider *bgutils.PotProvider, tag string) (baseURL string, poToken string)
  ```

**What it must do**, in order (this is the port of upstream's "re-fetch the player response"):
1. `invalidate403Caches(job, videoInfo.PlayerURL, cipherSolver, potProvider, tag)` — wipes the cipher solver, POT caches and visitor data, exactly as the existing `OnCipherFailure` does.
2. Re-resolve the URL for the chosen itag: `resolveFormatURLByItag(ctx, videoInfo.Formats, itag, routedSolver, cipherSolver, videoInfo.PlayerURL, job.Logger)`.
3. Re-mint the GVS token: `potProvider.GeneratePoTokenString(ctx, poTokenBinding(job, videoInfo), true)` — **`bypassCache: true`** is essential; a cached token is the very credential that just failed.
4. Return both; return `""` for whichever step errored, and log the failure at Warn with `jobID`. Never return an error — a failed refresh must degrade to "no new credentials", not kill the download.

- [ ] **Step 1: Write the failing test**

Append to `internal/worker/strategies_test.go`:

```go
// TestRefreshGvsCredentialsBypassesTokenCache pins the property that makes
// recovery work at all: the re-mint must bypass the session cache. The token
// that just earned a 403 is the cached one, so handing it back unchanged
// would make the refresh a no-op — which is precisely the difference between
// the failing download and the manual cancel-and-resume that fixes it.
func TestRefreshGvsCredentialsBypassesTokenCache(t *testing.T) {
	var gotBypass bool
	var gotBinding string
	fake := &fakePotProvider{
		generate: func(ctx context.Context, binding string, bypassCache bool) (string, error) {
			gotBinding = binding
			gotBypass = bypassCache
			return "fresh-token", nil
		},
	}
	// … construct a JobContext with a stub YT service whose GetVisitorData
	// returns "vd-123", and a videoInfo with one format for itag 140 …

	_, token := refreshGvsCredentials(context.Background(), job, videoInfo, 140, nil, nil, fake, "test")

	if token != "fresh-token" {
		t.Errorf("token = %q, want fresh-token", token)
	}
	if !gotBypass {
		t.Error("re-mint must pass bypassCache=true; a cached token is the one that just failed")
	}
	if gotBinding != "vd-123" {
		t.Errorf("binding = %q, want the visitor data", gotBinding)
	}
}
```

**Implementer note — define this interface first, the test and helper both use it.** `bgutils.PotProvider` is a concrete struct, so the helper takes a one-method interface instead. Add to `internal/worker/strategies.go`:

```go
// gvsTokenMinter is the slice of *bgutils.PotProvider that credential
// refresh needs. Declared here (consumer side) so refreshGvsCredentials is
// testable without a real BotGuard sidecar; *bgutils.PotProvider satisfies
// it implicitly.
type gvsTokenMinter interface {
	GeneratePoTokenString(ctx context.Context, contentBinding string, bypassCache bool) (string, error)
}
```

and the test's stub:

```go
type fakePotProvider struct {
	generate func(ctx context.Context, binding string, bypassCache bool) (string, error)
}

func (f *fakePotProvider) GeneratePoTokenString(ctx context.Context, binding string, bypassCache bool) (string, error) {
	return f.generate(ctx, binding, bypassCache)
}
```

`invalidate403Caches` takes the concrete `*bgutils.PotProvider` and calls `InvalidateCaches()` on it, which is NOT on this interface. Keep `refreshGvsCredentials` taking both: the concrete provider for `invalidate403Caches`, and `gvsTokenMinter` for the mint — or widen `gvsTokenMinter` with `InvalidateCaches()` and pass one value. Pick one, apply it consistently to the signature shown below, and say which in your report. Do not widen any other function's signature.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/worker/ -run TestRefreshGvsCredentials -v`
Expected: FAIL — `refreshGvsCredentials` undefined.

- [ ] **Step 3: Implement the helper**

In `internal/worker/strategies.go`, after `invalidate403Caches`:

```go
// refreshGvsCredentials produces a fresh URL + PO token for a live segment
// downloader whose current credentials started earning 403s below the live
// head. It is the in-process equivalent of cancelling and resuming a job,
// which is what operators had to do manually before this existed.
//
// Both upstreams recover the same way — yt-dlp's url_feed and moonarchive's
// "stream access expired? retrieve a fresh manifest" both go back to the
// player response rather than refreshing the URL alone, because the PO token
// is the half that actually went stale.
//
// Never returns an error: a failed refresh yields empty strings, the engine
// keeps its existing credentials, and the download fails the same way it
// would have without this callback.
func refreshGvsCredentials(
	ctx context.Context,
	job *JobContext,
	videoInfo *youtube.VideoInfo,
	itag int,
	routedSolver cipher.Solver,
	cipherSolver *cipher.GojaResolver,
	potProvider gvsTokenMinter,
	tag string,
) (baseURL string, poToken string) {
	invalidate403Caches(job, videoInfo.PlayerURL, cipherSolver, potProvider, tag)

	if fresh, err := resolveFormatURLByItag(ctx, videoInfo.Formats, itag, routedSolver, cipherSolver, videoInfo.PlayerURL, job.Logger); err != nil {
		job.Logger.Warn("[POT] credential refresh: URL re-resolve failed",
			"jobID", job.Job.ID, "tag", tag, "err", err)
	} else {
		baseURL = fresh
	}

	if potProvider != nil {
		binding := poTokenBinding(job, videoInfo)
		// bypassCache: the cached token is the credential that just 403'd.
		if token, err := potProvider.GeneratePoTokenString(ctx, binding, true); err != nil {
			job.Logger.Warn("[POT] credential refresh: re-mint failed",
				"jobID", job.Job.ID, "tag", tag, "err", err)
		} else {
			poToken = token
		}
	}

	job.Logger.Info("[POT] credential refresh", "jobID", job.Job.ID, "tag", tag,
		"newURL", baseURL != "", "newToken", poToken != "")
	return baseURL, poToken
}
```

`invalidate403Caches` currently takes `*bgutils.PotProvider`; if you introduce the `gvsTokenMinter` interface, either widen that parameter to the interface too or keep passing the concrete provider through a separate argument — whichever keeps the diff smaller. State which you chose.

- [ ] **Step 4: Wire it into the manifestless strategy**

In `strategy_youtube_manifestless_dash.go`, inside the `if videoStream != nil` block where `OnCipherFailure` is already installed, add to the `DownloaderOptions` literal:

```go
			OnCredentialRefresh: func() (string, string) {
				return refreshGvsCredentials(ctx, job, videoInfo, videoStream.Itag,
					routedSolver, cipherSolver, potProvider, "manifestless DASH video")
			},
```

and the audio equivalent with `audioStream.Itag` and `"manifestless DASH audio"`.

Capture the itag into a local before the closure (the existing `OnCipherFailure` blocks already do this with `videoItagChosen` / `audioItagChosen`) so the closure cannot observe a later mutation of the stream struct.

Leave `OnCipherFailure` in place: it fires earlier (at `postBytes403CipherThreshold`, on the pre-bytes cipher-rotation case) and handles a different failure.

- [ ] **Step 5: Run tests**

Run: `go build ./... && go test ./internal/worker/ ./internal/engine/ -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/worker/strategies.go internal/worker/strategy_youtube_manifestless_dash.go internal/worker/strategies_test.go
git commit -m "feat(worker): supply fresh URL + re-minted POT on behind-head 403s"
```

---

### Task 6: End-to-end recovery test, docs, verification

**Files:**
- Test: `internal/engine/downloader_dash_integration_test.go` (append)
- Modify: `docs/spec/platform-services.md` (the "Mid-job re-mint: none" subsection — it now says the opposite of the truth)

- [ ] **Step 1: Write the end-to-end test**

The file already has a `fakeGVS` harness with `&sq=N` addressing, `X-Head-Seqnum` on every response, and per-seq attempt counting. Append a scenario that reproduces the production failure through the whole loop:

```go
// TestDashLoopRecoversFromCredentialExpiry reproduces the 2026-08-15
// mid-stream-join stall end to end: segments below the head start 403ing
// after N successes, and the download must refresh credentials and carry on
// rather than stalling. Before the fix this test hangs at seq 20 forever.
func TestDashLoopRecoversFromCredentialExpiry(t *testing.T) {
	const expireAfter = 20
	var refreshed atomic.Bool

	srv := newFakeGVS(t, 200, func(seq int, attempt int) fakeGVSResponse {
		// Credentials go stale after `expireAfter` segments and only a
		// refresh revives them — exactly what production showed (~60-100
		// segments per credential, then 403 until a fresh token arrived).
		if seq >= expireAfter && !refreshed.Load() {
			return fakeGVSResponse{status: http.StatusForbidden}
		}
		return fakeGVSResponse{status: http.StatusOK, body: []byte("seg")}
	})
	defer srv.Close()

	// … construct the downloader against srv with OnCredentialRefresh
	// setting refreshed.Store(true) and returning ("", "fresh-token"),
	// EndSeq 60, and run the loop with a bounded context …

	// Assert: the run completes, every seq 0..60 landed, and the refresh
	// callback fired at least once.
}
```

**Implementer note:** read `newFakeGVS`'s actual signature and `fakeGVSResponse`'s actual fields before writing this — the names above describe the intent, and the existing harness's real API wins. Match the file's existing scenario style.

- [ ] **Step 2: Run it and confirm it fails on the pre-fix code path**

Run: `git stash && go test ./internal/engine/ -run TestDashLoopRecoversFromCredentialExpiry -timeout 60s; git stash pop`
Expected: FAIL or timeout without the fix. Then with the fix applied: PASS. Report both outcomes — a test that passes either way is worthless here.

- [ ] **Step 3: Correct the docs**

`docs/spec/platform-services.md` has a subsection titled **"Mid-job re-mint: none"** stating that a stale-token 403 mid-download is only fixed by a full downloader restart. That is now false. Rewrite it to describe the new behaviour: 403 below the head triggers a cooldown-gated `OnCredentialRefresh` that re-resolves the URL and re-mints the token with `bypassCache`, retries the segment, and only falls back to permanently-gone when refreshed credentials also fail; 410 and 403 at-or-past-head are unchanged. Include the upstream references (yt-dlp `url_feed` / `fragment_retries`, moonarchive `frag_iterator`) and note the catch-up batch damping.

- [ ] **Step 4: Full verification**

```bash
cd bgutil-sidecar && node build.mjs && cd ..
go build ./... && go vet ./... && gofmt -l internal/
go test ./... -count=1
```

Expected: all clean, 27 packages ok.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/downloader_dash_integration_test.go docs/spec/platform-services.md
git commit -m "test(engine): end-to-end credential-expiry recovery; docs: correct mid-job re-mint"
```

---

## Field verification (after merge — not part of the plan's tasks)

The unit tests cannot prove this fixes the real failure, because we still do not know *why* YouTube expires the credential. The real gate is behavioural:

1. Start a from-start archive of an **already-in-progress** live stream (this is the scenario that fails today; a go-live join will not exercise it).
2. Watch for `[Downloader] credentials refreshed after 403` and `[POT] credential refresh` in the log.
3. Success looks like: the download continues past the point where it previously stalled (~60–100 segments per credential), with periodic refresh lines rather than a `permanently gone` storm and a frozen file.
4. If it still stalls, the next diagnostic is the instrumented build that logs the true status code and response headers at the first failure of each burst — the refresh mechanism would be working but repairing the wrong credential.

## Known gaps deliberately not in this plan

- **Skip-one.** Both upstreams skip a fragment that 403s even with fresh credentials while behind head (moonarchive's `cur_seq += 1`, yt-dlp's `report_skip_fragment`). We do not, because our writer appends sequentially and a skipped segment leaves a hole mid-file; that deserves its own design pass covering the reorder buffer and gap accounting. Until then a poisoned segment can still stall an archive — but the refresh path removes the common case.
- **Why the credential expires.** Unknown. Possibly a per-URL fetch budget, a token use limit, or throttling of rapid past-segment access. Upstream does not diagnose it either; both just refresh and continue.
- **VOD/post-live paths** are untouched: they are not behind head in the live sense, so the new branch never fires for them.
