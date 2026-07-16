# Feed History 2/5 — Probe Chain Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The probe returns a publish date and a playability verdict, and the feed path gets its own side-effect-free probe function (`probeAndClassify`) with the four-outcome contract and the canonical `denied` predicate — while DECAPI's composed `ProcessYouTubeVideo` stays byte-identical in behavior.

**Architecture:** New extraction in `internal/youtube` (status-aware `PublishedAt`); `internal/monitor/utils.go` splits `ProcessYouTubeVideo` into `probeAndClassify` (probe + classify + tracker/cooldown, NO history writes, NO verdict) and the existing composed function (DECAPI only). `denied` is one function with one caller-facing contract.

**Tech Stack:** Go 1.25. No new dependencies.

**Depends on:** Plan 1 (nothing hard — only `database.PrecisionRank` naming for the precision strings).

**Spec:** `docs/superpowers/specs/2026-07-15-feed-history.md` §9 (outcomes, denied, escalation preconditions), §12 (dates, ladder, extraction chain, terminal invariant). Global Constraints from Plan 1 apply.

---

### Task 1: Pin `itemAge` against the well-meaning fix

**Files:**
- Test: `internal/youtube/channel_membership_test.go` (extend)

**Interfaces:** none — this is a regression pin. `itemAge` MUST NOT change (spec §12: skew-new wants the truncated lower bound it already returns; the unit is unrecoverable from the value, and that is fine).

- [ ] **Step 1: Write the pinning test**

```go
func TestItemAgeTruncatedLowerBound(t *testing.T) {
	// Spec §12: itemAge returns n*unit — the LOWER bound of the true age — so
	// now - itemAge() is the NEWEST instant consistent with the text. Do NOT
	// "fix" this to a midpoint or upper bound: the window design depends on it.
	item := map[string]any{"title": map[string]any{"simpleText": "x"},
		"publishedTimeText": map[string]any{"simpleText": "1 week ago"}}
	if got := itemAge(item); got != 7*24*time.Hour {
		t.Fatalf("itemAge(1 week ago) = %v, want 168h", got)
	}
	// The live-badge short-circuit outranks the age regex: a live renderer
	// carrying "Started streaming 2 hours ago" must return 0, not 2h.
	live := map[string]any{"publishedTimeText": map[string]any{"simpleText": "Started streaming 2 hours ago"},
		"thumbnailOverlays": []any{map[string]any{"thumbnailOverlayTimeStatusRenderer": map[string]any{"style": "THUMBNAIL_OVERLAY_BADGE_STYLE_LIVE"}}}}
	if got := itemAge(live); got != 0 {
		t.Fatalf("live badge must short-circuit to 0, got %v", got)
	}
}
```

- [ ] **Step 2: Run** — `go test -run TestItemAgeTruncated ./internal/youtube/ -v` — Expected: **PASS immediately** (it pins existing behavior; if it fails, the fixture shape is wrong — compare with the existing `itemAge` tests in this file and adjust the map shape, NOT the assertion values).
- [ ] **Step 3: Commit** — `git commit -m "test(youtube): pin itemAge truncated lower bound + badge short-circuit"`

---

### Task 2: `VideoInfo.PublishedAt` — status-aware extraction

**Files:**
- Modify: `internal/youtube/types.go` (`VideoInfo` struct, near `ScheduledStartTime :46` / `PlayabilityError :49`)
- Modify: `internal/youtube/player_api_parsing.go` (new function + call from the `VideoInfo` assembly site)
- Test: `internal/youtube/player_api_parsing_test.go` (extend)

**Interfaces:**
- Produces: `VideoInfo.PublishedAt string` (RFC3339 or ""), `VideoInfo.PublishedPrecision string` (`"started"`, `"day"`, or `""`).
- Rule (spec §12, verbatim behavior): `vod`/`post_live` → `liveBroadcastDetails.startTimestamp` ⇒ `started`; ELSE microformat `uploadDate`/`publishDate` ⇒ `day`. `not_a_stream` → microformat ⇒ `day`. `upcoming`/`live` → nothing. The `liveStreamability` epoch is NEVER a publish date. **Do not reuse `extractScheduledStartTime`** (`player_api_parsing.go:113-138`) — it conflates a future scheduled start with a publish date.

- [ ] **Step 1: Write the failing tests** (inline JSON fixtures, mirroring `TestClassifyStream_PostLiveDVR`'s style at `player_api_parsing_test.go:313-328`)

```go
func TestExtractPublishedAt(t *testing.T) {
	cases := []struct {
		name, status, wantTS, wantPrec string
		player                         map[string]any
	}{
		{"post_live takes startTimestamp as started", "post_live", "2026-07-14T20:00:00+00:00", "started",
			playerWith(map[string]any{"liveBroadcastDetails": map[string]any{"startTimestamp": "2026-07-14T20:00:00+00:00", "endTimestamp": "2026-07-14T22:00:00+00:00"}}, "2026-07-01")},
		{"vod without lbd falls back to uploadDate as day", "vod", "2026-07-01", "day",
			playerWith(nil, "2026-07-01")},
		{"not_a_stream takes uploadDate as day", "not_a_stream", "2026-07-01", "day",
			playerWith(nil, "2026-07-01")},
		{"upcoming stores nothing — startTimestamp here is the FUTURE", "upcoming", "", "",
			playerWith(map[string]any{"liveBroadcastDetails": map[string]any{"startTimestamp": "2027-01-01T00:00:00+00:00"}}, "2026-07-01")},
		{"live stores nothing", "live", "", "", playerWith(nil, "2026-07-01")},
		{"vod with neither yields nothing (caller enforces the terminal invariant)", "vod", "", "",
			playerWith(nil, "")},
	}
	for _, c := range cases {
		ts, prec := extractPublishedAt(c.status, c.player)
		if ts != c.wantTS || prec != c.wantPrec {
			t.Errorf("%s: got (%q,%q) want (%q,%q)", c.name, ts, prec, c.wantTS, c.wantPrec)
		}
	}
}

// playerWith builds the minimal player-response map: optional
// microformat.playerMicroformatRenderer.uploadDate and optional
// videoDetails-adjacent liveBroadcastDetails (match where classifyStream reads
// lbd from in THIS file — copy the path from the existing post_live fixture).
func playerWith(lbd map[string]any, uploadDate string) map[string]any {
	m := map[string]any{}
	if lbd != nil {
		setPath(m, lbd, /* same path the existing fixtures use for liveBroadcastDetails */)
	}
	if uploadDate != "" {
		m["microformat"] = map[string]any{"playerMicroformatRenderer": map[string]any{"uploadDate": uploadDate}}
	}
	return m
}
```

Copy the exact `liveBroadcastDetails` path from the existing `TestClassifyStream_PostLiveDVR` fixture (`player_api_parsing_test.go:313-328`) — the helper must place `lbd` where `classifyStream` actually reads it.

- [ ] **Step 2: Verify failure** — `go test -run TestExtractPublishedAt ./internal/youtube/ -v` → FAIL (undefined).

- [ ] **Step 3: Implement** in `player_api_parsing.go`:

```go
// extractPublishedAt returns the probe's authoritative publish date for the
// ladder (spec §12). Status-aware: startTimestamp is a REAL broadcast start
// only for past streams; for upcoming it is the future schedule and must never
// feed published. The liveStreamability epoch is never consulted.
func extractPublishedAt(status string, player map[string]any) (ts, precision string) {
	switch status {
	case "vod", "post_live":
		if lbd := liveBroadcastDetailsOf(player); lbd != nil { // reuse this file's existing lbd accessor — grep: grep -n "liveBroadcastDetails" internal/youtube/player_api_parsing.go | head -5
			if v := getStr(lbd, "startTimestamp"); v != "" {
				return v, "started"
			}
		}
		if v := microformatDate(player); v != "" {
			return v, "day" // the NORMAL vod case — an ended stream WITH endTimestamp classifies post_live, so plain vod usually has no lbd (spec §12)
		}
	case "not_a_stream":
		if v := microformatDate(player); v != "" {
			return v, "day"
		}
	}
	return "", ""
}

func microformatDate(player map[string]any) string {
	mf, _ := player["microformat"].(map[string]any)
	pmr, _ := mf["playerMicroformatRenderer"].(map[string]any)
	if v, _ := pmr["uploadDate"].(string); v != "" {
		return v
	}
	v, _ := pmr["publishDate"].(string)
	return v
}
```

Add `PublishedAt`, `PublishedPrecision string` to `VideoInfo` (`types.go`), and populate them at the same site that populates `VideoInfo.PlayabilityError` — Run: `grep -n "PlayabilityError:" internal/youtube/*.go` and add the two fields to that constructor with `extractPublishedAt(string(status), playerResponse)`.

- [ ] **Step 4: Verify pass** — `go test ./internal/youtube/ -v -run 'TestExtract|TestClassify'` → PASS, no regressions.
- [ ] **Step 5: Commit** — `git commit -m "feat(youtube): status-aware PublishedAt on the probe"`

---

### Task 3: Surface date + playability through `VideoProbeResult`

**Files:**
- Modify: `internal/monitor/utils.go:32-36` (`VideoProbeResult`)
- Modify: `cmd/moombox/monitor_callbacks.go:174-178` and `:193-197` (both wiring sites)
- Test: none new (compile-level; behavior tested in Task 4)

**Interfaces:**
- Produces: `VideoProbeResult{StreamStatus, Title, ChannelName, PublishedAt, PublishedPrecision, PlayabilityError string}` — the monitor can now tell an observation from a refusal (spec §12: without `PlayabilityError`, `denied` is impossible).

- [ ] **Step 1:** Add the three fields to `VideoProbeResult`; copy them from `VideoInfo` at BOTH `monitor_callbacks.go` sites (the anonymous probe `:174-178` and `ProbeVideoAuth` `:193-197`) — both currently copy exactly three fields; they now copy six.
- [ ] **Step 2:** `go build ./...` → PASS. Commit: `git commit -m "feat(monitor): probe result carries PublishedAt + PlayabilityError"`

---

### Task 4: Split `ProcessYouTubeVideo` — `probeAndClassify` + the canonical `denied`

**Files:**
- Modify: `internal/monitor/utils.go` (`ProcessYouTubeVideo` at `:244-355` recomposed; new types + `probeAndClassify` + `isDenied`)
- Test: `internal/monitor/utils_test.go` (extend)

**Interfaces (plan 3 consumes exactly these):**

```go
type ProbeOutcome int
const (
	OutcomeProbed ProbeOutcome = iota // metadata returned, NOT denied ⇒ FRESH
	OutcomeDenied                     // YouTube refused AND classifier guessed
	OutcomeErrored                    // probe ran and failed
	OutcomeCooldown                   // ProbeCooldown suppressed it
)
type ProbeClassifyParams struct { // subset of ProcessYouTubeVideoParams — NO AddToHistory, NO IsReprobe
	Ctx        context.Context
	VideoID    string
	Channel    *config.ChannelConfig
	ProbeVideo func(ctx context.Context, videoID string) (*VideoProbeResult, error) // REQUIRED — nil is a programming error (panic), the feed path has no passthrough mode
	Tracker    *MetadataTracker
	Cooldown   *ProbeCooldown
	Logger     logger
}
type ProbeClassifyResult struct {
	Outcome            ProbeOutcome
	StreamStatus       string // meaningful IFF Outcome == OutcomeProbed
	Title, ChannelName string
	PublishedAt        string
	PublishedPrecision string
	PlayabilityError   string // always carried — the escalation reads it even on denied
}
func probeAndClassify(p ProbeClassifyParams) ProbeClassifyResult
func isDenied(streamStatus, playabilityError string) bool // THE predicate — stated once (§9)
```

- [ ] **Step 1: Write the failing tests**

```go
func TestIsDenied_CanonicalTable(t *testing.T) {
	// Spec §9 table — all seven rows. Both conjuncts load-bearing.
	cases := []struct {
		status, playability string
		want                bool
	}{
		{"upcoming", "members_only", true},
		{"upcoming", "login_required", true},
		{"upcoming", "ok", false},              // genuine upcoming — goal 3
		{"upcoming", "age_restricted", false},  // premieres reach upcoming with non-ok playability
		{"upcoming", "unknown", false},         // unknown is not a refusal
		{"vod", "age_restricted", false},       // formats came back — grounded
		{"vod", "members_only", false},         // same — the status conjunct allows this
		{"not_a_stream", "login_required", false},
	}
	for _, c := range cases {
		if got := isDenied(c.status, c.playability); got != c.want {
			t.Errorf("isDenied(%q,%q) = %v, want %v", c.status, c.playability, got, c.want)
		}
	}
}

func TestProbeAndClassify_Outcomes(t *testing.T) {
	ch := &config.ChannelConfig{Name: "c"}
	mk := func(probe func(context.Context, string) (*VideoProbeResult, error)) ProbeClassifyParams {
		return ProbeClassifyParams{Ctx: context.Background(), VideoID: "v", Channel: ch,
			ProbeVideo: probe, Tracker: NewMetadataTracker(), Logger: testLogger(t)}
		// Cooldown nil ⇒ no suppression; grep the existing utils_test.go for the
		// logger + tracker helpers it already uses and reuse them.
	}
	// probed
	r := probeAndClassify(mk(func(ctx context.Context, id string) (*VideoProbeResult, error) {
		return &VideoProbeResult{StreamStatus: "vod", Title: "T", PublishedAt: "2026-07-14T20:00:00Z", PublishedPrecision: "started", PlayabilityError: "ok"}, nil
	}))
	if r.Outcome != OutcomeProbed || r.StreamStatus != "vod" || r.PublishedPrecision != "started" {
		t.Fatalf("probed: %+v", r)
	}
	// denied — upcoming + members_only
	r = probeAndClassify(mk(func(ctx context.Context, id string) (*VideoProbeResult, error) {
		return &VideoProbeResult{StreamStatus: "upcoming", PlayabilityError: "members_only"}, nil
	}))
	if r.Outcome != OutcomeDenied || r.PlayabilityError != "members_only" {
		t.Fatalf("denied: %+v (PlayabilityError must be carried — the escalation reads it)", r)
	}
	// errored
	r = probeAndClassify(mk(func(ctx context.Context, id string) (*VideoProbeResult, error) {
		return nil, errors.New("boom")
	}))
	if r.Outcome != OutcomeErrored {
		t.Fatalf("errored: %+v", r)
	}
}

func TestProbeAndClassify_NoHistoryWrites(t *testing.T) {
	// The split exists because ProcessYouTubeVideo has AddToHistory side effects
	// at utils.go:284/:313/:330. probeAndClassify has NO AddToHistory parameter,
	// which the compiler enforces — this test pins the DECAPI side instead:
	// ProcessYouTubeVideo (composed) still writes history on a skipped vod.
	var histCalls int
	res := ProcessYouTubeVideo(ProcessYouTubeVideoParams{
		Ctx: context.Background(), VideoID: "v", Channel: &config.ChannelConfig{Name: "c"},
		ProbeVideo: func(ctx context.Context, id string) (*VideoProbeResult, error) {
			return &VideoProbeResult{StreamStatus: "vod", Title: "T", PlayabilityError: "ok"}, nil
		},
		AddToHistory: func(id string) error { histCalls++; return nil },
		Tracker:      NewMetadataTracker(), Logger: testLogger(t),
	})
	if res.ShouldProcess {
		t.Fatal("skipped vod (IncludeNonLiveContent false) must not process")
	}
	if histCalls != 1 {
		t.Fatalf("DECAPI path must keep its history write, got %d calls", histCalls)
	}
}
```

- [ ] **Step 2: Verify failure** — `go test -run 'TestIsDenied|TestProbeAndClassify' ./internal/monitor/ -v` → FAIL.

- [ ] **Step 3: Implement.** In `utils.go`:

```go
// isDenied is THE denied predicate (spec §9) — stated once; every caller
// defers here. Distrust a probe result only when YouTube said it refused us
// AND the classifier was guessing:
//   denied ⇔ StreamStatus == 'upcoming' AND PlayabilityError ∈ {members_only, login_required}
// Broader rules are rejected in the spec: "trust only ok" kills genuine
// premieres; "any non-ok" refuses downloadable age-restricted VODs; "unknown"
// is not a refusal (it is 'we could not read the answer').
func isDenied(streamStatus, playabilityError string) bool {
	return streamStatus == "upcoming" &&
		(playabilityError == "members_only" || playabilityError == "login_required")
}
```

`probeAndClassify`: extract the middle of today's `ProcessYouTubeVideo` — the cooldown check (`:258-262` → return `OutcomeCooldown`), the probe call + tracker bookkeeping (`:269-294` → error path returns `OutcomeErrored`; success records cooldown at `:299` as today), then classify: `if isDenied(meta.StreamStatus, meta.PlayabilityError) { return ...OutcomeDenied... }` else `OutcomeProbed` with all metadata copied. It performs **no** `AddToHistory`, consults **no** `IsReprobe`, and **panics on nil ProbeVideo** (`panic("probeAndClassify: ProbeVideo not wired")`) — the feed path has no passthrough mode; production always wires it (`monitor_callbacks.go:180`).

Recompose `ProcessYouTubeVideo` on top: keep its `p.ProbeVideo == nil ⇒ ShouldProcess: true` passthrough (`:248-250`) BEFORE delegating; map outcomes back to today's returns byte-identically — `OutcomeCooldown`/`OutcomeErrored` → today's `:261`/`:294` returns (including the give-up `AddToHistory` at `:284`); `OutcomeProbed`/`OutcomeDenied` → today's `:302-355` flow with `nonLiveSkipReason`/`AddToHistory`/`ShouldProcess` untouched. **The composed function's observable behavior for DECAPI must not change** — run the full existing `utils_test.go` suite to prove it.

- [ ] **Step 4: Verify pass** — `go test ./internal/monitor/ -v` → PASS, including every pre-existing test unchanged.
- [ ] **Step 5: Commit** — `git commit -m "feat(monitor): probeAndClassify split with four-outcome contract and canonical denied"`

## Self-check before handoff

- `grep -n "isDenied" internal/monitor/*.go` → exactly one definition; callers do not restate the predicate.
- `grep -c "AddToHistory" internal/monitor/utils.go` → same count as before the split minus zero (all three sites live in the composed function only).
