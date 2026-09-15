# Sweep Arc 3 — YouTube Extraction Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make YouTube watch-page extraction correct on pathological descriptions, cheap on the 24/7 poll path, bounded on the 403-recovery path, and free of the dead attestation-challenge plumbing — then re-pin the vendored ejs source and fix the cipher-cache doc drift.

**Architecture:** `internal/youtube/watch_page.go` stops treating the page as a string: `FetchWatchPage` hands the raw `[]byte` to every extractor, `ytInitialPlayerResponse` is located by an assignment-prefix anchor plus the existing `scanBalancedObject` brace walk instead of a lazy `({.+?});`, and the chat continuation is decoded through a typed `json.RawMessage` envelope instead of `map[string]any` over the whole megabyte payload. `internal/cookies/jar.go` gains a `(size, mtime)` short-circuit with a git-style racily-clean settle window. `doRetryRequest`'s fixed 1+2+4 s backoff becomes context-deadline aware. The unused `challenge` parameters and the uncalled `PotTokenProvider.GeneratePlayerPoToken` are deleted per owner ruling R1. Finally the vendored ejs tree is re-pinned to upstream `6f8587b` and every doc that claims a 3-VM cipher LRU is corrected to 10.

**Tech Stack:** Go 1.27 (no CGo), `regexp`, `encoding/json`, `bytes`, `os.Stat`; Node 24 + esbuild for the sidecar's vendored-ejs bundle; `testing.AllocsPerRun` and `testing.B` for the allocation pins.

**Spec:** `docs/superpowers/specs/2026-09-15-sweep-fix-chain-design.md` — **§6 (Arc 3 — YouTube extraction, branch `sweep-3-youtube`)**. §1 (owner ruling R1), §2 (execution model), §3 (global constraints), §11 (concurrency table) and §12 (definition of done) bind this arc. Ledger items: **T1-5, T2-16, T2-23, T3-27, T4-31 (challenge, R1), T4-34 (skill doc LRU), and the ejs pin row** in `reports/sweep-2026-09-15.md`.

## Global Constraints

Copied verbatim from spec §3. Every task's requirements implicitly include this section.

- `go 1.27`, no CGo, pure-Go dependencies; Windows x64 + Linux x64 + Linux arm64 must build.
- LF line endings in every file the chain touches. Commits carry the two trailers
  `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>` and
  `Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq`.
- The anonymous logger interface (`Debug/Info/Warn/Error(msg string, args ...any)`) stays anonymous
  per struct. Every goroutine has an inline `defer func() { if r := recover(); … }()`.
- `internal/tui` never imports `internal/web/routes` or `internal/bgutils` (compile-time embed check).
- Helpers are defined in the task that first uses them (staticcheck U1000 is a hard gate).
- TDD per task: the failing test is written and run red before the change; every new assertion names
  the mutant that fails it (the reviewer verifies at least one).
- Every JS-touching task gates `go test ./internal/web/routes/` (its tests lift app.js/player.js
  bodies into goja) AND `node --test web/tests/*.test.mjs`. Every task that renames, moves or deletes a
  Go symbol, or edits `docs/spec/*.md`/`SPEC.md`, gates `go test ./internal/docs/` (the citation test
  requires the DECLARING file).
- Behaviour that the owner's rulings protect stays: ~60 Hz progress pipeline and DB write cadence
  (make updates cheaper, never rarer); DB layer untouched for perf; `monitors.probe_cooldown` default 0;
  the BotGuard interpreter gate; `/retry` vs `/resume` gates never shared; the Web cookie import stays
  unbounded (a test forbids WithTimeout).
- Reviewers never edit files (they reproduce in `git archive` scratchpad exports); implementers use git
  only for `add`/`commit` on the arc branch; no stashing, no rebasing, no checkout of other branches.
- User-facing strings: match the twin UI's exact output before mirroring it (read the twin's code).

### Arc-specific constraints and environment

- Branch `sweep-3-youtube` in `.worktrees/sweep-3-youtube`, cut from `main`:
  `git worktree add -b sweep-3-youtube .worktrees/sweep-3-youtube main`.
- After creating the worktree, copy the gitignored inputs a fresh worktree lacks:
  `internal/bgutils/embed/{node-windows-amd64.gz,node-linux-amd64.gz,node-linux-arm64.gz,sidecar.tar.gz}`
  and `internal/cipher/testdata/*.js` from `D:/Git/Moombox/`. **Task 6 additionally needs**
  `cd .worktrees/sweep-3-youtube/bgutil-sidecar && npm ci --no-audit --no-fund` (full install — see
  Task 6 step 1 for why `--omit=dev` is the wrong flavour here).
- `references/ejs` is gitignored and exists **only in the main checkout**. Task 6's `cp` sources are
  therefore absolute: `D:/Git/Moombox/references/ejs/...`.
- Every go command is prefixed `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp`.
- Arc 3 may run concurrently with Arc 7 (spec §11) because their `internal/cookies` file sets are
  disjoint: Arc 3 touches **only** `internal/cookies/jar.go` plus a NEW test file
  `internal/cookies/jar_reload_memo_test.go`. Commit with explicit pathspecs, never `git add -A`.
- `docs/spec/data-and-storage.md` belongs to Arc 7 — Arc 3 does not edit it. The jar short-circuit is
  documented in `jar.go`'s own doc comment (Task 3) and reported to the controller for Arc 7 to mirror.

### Known-red baseline (report before starting)

`go test ./internal/cookies/...` is **already red on `main`**:
`TestRefreshWarnsWhenTheExpiredTwitchLoginIsPruned` (`internal/cookies/autocookies_merge_login_prune_test.go:104`)
pins `const twitchTokenExpiry = 1789000000`, which decodes to **2026-09-10** and is now in the past, so
the fixture's auth-token is expired, `platformsToRestoreOnRegression` rolls the write back, and neither
the prune Warn nor the "cookie refresh succeeded" line is produced. Arc 3 does not fix it (wrong file
set) — tell the controller so it is fixed on `main` before this arc's `./internal/cookies/...` gate is
judged. Treat that one test as a pre-existing failure, never as an Arc 3 regression.

### Gate list (spec §2 merge-candidate gates + spec §6 arc gates)

Per task, and again before the merge:

```bash
gofmt -l ./cmd ./internal ./tools ./web                                  # must print nothing
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./...
staticcheck ./...                                                        # pinned 2026.2.1, clean
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./...
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp GOOS=linux GOARCH=amd64 go build ./...
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp GOOS=linux GOARCH=arm64 go build ./...
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/youtube/... ./internal/cipher/... ./internal/bgutils/... ./internal/cookies/... ./internal/docs/
```

ONE controller-run `go test -count=1 ./...` at a time across all worktrees, before the merge.

Live gates (this arc touches extraction clients, the cipher's vendored source and the sidecar payload,
so all four run before the merge):

```bash
MOOMBOX_LIVE_CIPHER_TEST=1 go test -count=1 -timeout 180s -run TestSidecarSolver ./internal/cipher/...
MOOMBOX_LIVE_YT_TEST=1     go test -count=1 -timeout 300s -run 'TestLivePublicExtraction|TestLiveLoginMarkersPresent' ./internal/youtube/...
MOOMBOX_LIVE_BG_TEST=1     go test -count=1 -timeout 180s -run 'TestBotGuardLiveFingerprint|TestSidecarFallsBackOnDeath' ./internal/bgutils/...
MOOMBOX_LIVE_BG_TEST=1     go test -count=1 -timeout 180s -run 'TestSidecarLivePoToken|TestSidecarLiveGvsMint' ./internal/bgutils/sidecar/...
```

Optional, when a signed-in Netscape file is available:
`MOOMBOX_LIVE_YT_COOKIES=<path> go test -count=1 -run TestLiveAuthenticatedAccountProbe ./internal/youtube/...`.
Assert CAPABILITIES, not mechanisms: a live failure is a regression only once the lost capability is
confirmed (memory note `reference_live_tests_catch_client_death.md`).

No JS in `web/` changes in this arc, so the node suite and `./internal/web/routes/` are not gated.

## File Structure

| File | Responsibility | Tasks |
|---|---|---|
| `internal/youtube/watch_page.go` | Watch-page fetch + all four extractors. Gains the anchored player-response scan (T1), goes byte-side end to end and grows the typed chat envelope (T2). | 1, 2 |
| `internal/youtube/watch_page_test.go` | Player-response fixtures, legacy-vs-anchored equivalence table, chat-continuation shapes, the allocation ceiling and benchmark. | 1, 2 |
| `internal/youtube/session_auth_test.go` | Session-auth call sites move to `[]byte`; the string/bytes twin test retires with its twin. | 2 |
| `internal/youtube/liveness_markers_live_test.go` | One `watchPageSessionAuth(string(body))` call site. | 2 |
| `internal/cookies/jar.go` | `Load` gains the stat memo. | 3 |
| `internal/cookies/jar_reload_memo_test.go` (new) | The four memo tests. Kept out of `jar_storage_test.go` so Arc 7 never conflicts with it. | 3 |
| `internal/youtube/player_api_strategy.go` | Deadline-aware retry (T4); `challenge` parameters and arguments deleted (T5). | 4, 5 |
| `internal/youtube/player_api_retry_test.go` (new) | The three `doRetryRequest` deadline tests. | 4 |
| `internal/youtube/player_api.go` | `PotTokenProvider` loses `GeneratePlayerPoToken`. | 5 |
| `internal/bgutils/pot_provider.go` | `PotProvider.GeneratePlayerPoToken` deleted; the two doc comments that name it rewritten. | 5 |
| `internal/bgutils/pot_provider_test.go` | Its three tests retire with their subject. | 5 |
| `bgutil-sidecar/vendor/ejs/**` + `VERSION` | Re-vendored to upstream `6f8587b`. | 6 |
| `.claude/skills/moombox-upstream-porting/SKILL.md` | Two "3 slot / 3 cached VMs" sentences corrected. | 7 |
| `docs/spec/platform-services.md` | Player-response extraction prose (T1), cipher LRU figure ×3 (T7), the dormant-`GeneratePlayerPoToken` sentence (T5). | 1, 5, 7 |
| `docs/spec/architecture.md`, `design-philosophy.md`, `vision-and-purpose.md` | Remaining cipher LRU figures. | 7 |
| `docs/superpowers/plans/2026-09-15-sweep-3-youtube.md` | This plan — deleted in Task 7's commit. | 7 |

---

### Task 1: Player-response brace scan (T1-5)

The three patterns end in a lazy `({.+?});`. A `shortDescription` containing `};` makes all three
produce unbalanced JSON, so `PlayerResponse` comes back nil and the watch page's
`ScheduledStartTime` (the reschedule source read at `player_api_strategy.go:916-923`) and its format
pool are lost silently. `scanBalancedObject` already exists in this file for exactly this hazard.

**Files:**
- Modify: `internal/youtube/watch_page.go:44-50` (the pattern block), `:697-729` (the extraction loop inside `extractYtcfgAndPlayerResponse`)
- Modify: `docs/spec/platform-services.md:214-218`
- Test: `internal/youtube/watch_page_test.go` (append)

**Interfaces:**
- Consumes: `scanBalancedObject(s string) (string, bool)` (`watch_page.go:928`) — unchanged in this task.
- Produces:
  - `var playerResponseAnchors []*regexp.Regexp` — three assignment-PREFIX patterns, each ending on the literal `{`.
  - `func extractPlayerResponse(html string) (map[string]any, bool)` — first anchor whose brace scan AND `json.Unmarshal` both succeed. **Task 2 changes this signature to `extractPlayerResponse(page []byte) (map[string]any, bool)`.**

- [ ] **Step 1: Write the failing tests**

Append to `internal/youtube/watch_page_test.go`. Add `"encoding/json"`, `"reflect"` and `"regexp"` to
its import block if they are not already there.

```go
// synthPlayerPage renders a watch page whose ytInitialPlayerResponse carries
// desc as its shortDescription, JSON-encoded exactly as YouTube encodes it.
// The trailing `var meta=1;` matters: it is a second `;` for the old lazy
// `({.+?});` pattern to reach, so a fixture without it would not reproduce
// the truncation this task fixes.
func synthPlayerPage(t *testing.T, desc string) string {
	t.Helper()
	encoded, err := json.Marshal(desc)
	if err != nil {
		t.Fatalf("marshal description: %v", err)
	}
	return `<!DOCTYPE html><html><head><script nonce="q">var ytInitialPlayerResponse = ` +
		`{"videoDetails":{"videoId":"abc12345678","title":"T","author":"A","channelId":"UC1",` +
		`"shortDescription":` + string(encoded) + `}};var meta=1;</script></head><body></body></html>`
}

// TestPlayerResponseSurvivesBraceSemicolonInDescription is ledger item T1-5.
//
// Mutant named: the old lazy `({.+?});` patterns. They stop at the FIRST `};`
// in the page, which a description containing a code sample supplies, so the
// captured text is unbalanced, every pattern fails identically, and
// PlayerResponse is nil with nothing in the log.
func TestPlayerResponseSurvivesBraceSemicolonInDescription(t *testing.T) {
	const desc = "code sample: if (x) {y();};  thanks for watching"
	page := synthPlayerPage(t, desc)

	ytcfg, pr := extractYtcfgAndPlayerResponse(page)
	if pr == nil {
		t.Fatal("playerResponse is nil — a `};` inside shortDescription truncated the object")
	}
	vd, _ := pr["videoDetails"].(map[string]any)
	if got, _ := vd["shortDescription"].(string); got != desc {
		t.Errorf("shortDescription = %q, want %q", got, desc)
	}
	if ytcfg.Description != desc {
		t.Errorf("ytcfg.Description = %q, want %q", ytcfg.Description, desc)
	}
}

// TestPlayerResponseScannerHonoursEscapedQuotes pins the OTHER half of the
// fix: the balanced scan must track backslash escapes.
//
// Mutant named: a scanner that does not track escapes. On this fixture the
// description's `"` arrives as `\"`; an escape-blind scanner leaves the
// string there, reads the following `}` as a closing brace, and returns the
// object one level short — which then fails to parse, or worse, parses into
// a truncated videoDetails.
func TestPlayerResponseScannerHonoursEscapedQuotes(t *testing.T) {
	const desc = `a " quote then } brace`
	page := synthPlayerPage(t, desc)

	_, pr := extractYtcfgAndPlayerResponse(page)
	if pr == nil {
		t.Fatal("playerResponse is nil — the scan mis-handled the escaped quote")
	}
	vd, _ := pr["videoDetails"].(map[string]any)
	if got, _ := vd["shortDescription"].(string); got != desc {
		t.Errorf("shortDescription = %q, want %q", got, desc)
	}
}

// legacyPlayerResponsePatterns are the three lazy regexes this task replaces.
// They live HERE, in the test, purely as the before-image for the equivalence
// check below: on every page shape the lazy form parsed, the anchored brace
// scan must return the same decoded object. The repository carries no
// watch-page fixture corpus, so this table IS the fixture set.
var legacyPlayerResponsePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?s)var ytInitialPlayerResponse\s*=\s*({.+?});`),
	regexp.MustCompile(`(?s)window\["ytInitialPlayerResponse"\]\s*=\s*({.+?});`),
	regexp.MustCompile(`(?s)ytInitialPlayerResponse\s*=\s*({.+?});`),
}

// legacyExtractPlayerResponse is the pre-change control flow, verbatim:
// first pattern whose capture unmarshals wins.
func legacyExtractPlayerResponse(html string) (map[string]any, bool) {
	for _, re := range legacyPlayerResponsePatterns {
		m := re.FindStringSubmatch(html)
		if m == nil {
			continue
		}
		var pr map[string]any
		if json.Unmarshal([]byte(m[1]), &pr) == nil {
			return pr, true
		}
	}
	return nil, false
}

// TestPlayerResponseExtractionMatchesTheLegacyPatterns is the before/after
// equivalence check. Each fixture must ALSO parse under the legacy patterns —
// a fixture that does not is not a before-image and the subtest says so.
//
// Mutant named: an anchor that matches at the wrong offset (e.g. one that
// forgets `loc[1]-1` and starts the scan one byte past the `{`) returns a
// different object, or none, on every row here.
func TestPlayerResponseExtractionMatchesTheLegacyPatterns(t *testing.T) {
	fixtures := map[string]string{
		"var form":          synthPlayerPage(t, "a benign description"),
		"window form":       `<script>window["ytInitialPlayerResponse"] = {"videoDetails":{"shortDescription":"plain"}};</script>`,
		"bare form":         `<script>ytInitialPlayerResponse = {"videoDetails":{"shortDescription":"plain"}};var x=1;</script>`,
		"spaced assignment": `<script>var ytInitialPlayerResponse   =   {"videoDetails":{"shortDescription":"plain"}};</script>`,
		"newlines inside":   "<script>var ytInitialPlayerResponse = {\n\"videoDetails\":{\"shortDescription\":\"plain\"}\n};</script>",
		"unicode escapes":   `<script>var ytInitialPlayerResponse = {"videoDetails":{"shortDescription":"\u007d\u003b end"}};</script>`,
	}
	for name, page := range fixtures {
		t.Run(name, func(t *testing.T) {
			want, ok := legacyExtractPlayerResponse(page)
			if !ok {
				t.Fatalf("fixture is not a before-image: the legacy patterns did not parse it")
			}
			got, ok := extractPlayerResponse(page)
			if !ok {
				t.Fatalf("the anchored scan found no object where the legacy patterns did")
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("anchored extraction differs from the legacy extraction\n got: %#v\nwant: %#v", got, want)
			}
		})
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestPlayerResponse' ./internal/youtube/`

Expected: FAIL to build with `undefined: extractPlayerResponse`. Comment out the two
`extractPlayerResponse` call lines in the equivalence test and re-run to see the other two red:
`TestPlayerResponseSurvivesBraceSemicolonInDescription` fails with
`playerResponse is nil — a };  inside shortDescription truncated the object`;
`TestPlayerResponseScannerHonoursEscapedQuotes` PASSES on the old code (the lazy regex happens to
survive that shape — it is the scanner-correctness pin, not a second reproduction). Restore the
commented lines before Step 3.

- [ ] **Step 3: Replace the patterns with anchors**

In `internal/youtube/watch_page.go`, replace lines 44-50:

```go
	// Assignment-PREFIX anchors for ytInitialPlayerResponse. Each pattern
	// ends ON the opening brace; the object's extent then comes from
	// scanBalancedObject, never from the regex.
	//
	// These used to end in a lazy `({.+?});`, which stops at the first `};`
	// anywhere in the page. A shortDescription carrying `};` (a code sample,
	// an emoticon) therefore yielded unbalanced JSON — and because all three
	// patterns shared the flaw, all three failed identically, leaving
	// PlayerResponse nil. That silently costs the watch page's
	// ScheduledStartTime (the reschedule source) and its format pool, with
	// nothing in the log to distinguish it from "the page had no player
	// response". Same failure and same fix as ytAtNOpenRe below.
	//
	// Order is load-bearing and unchanged: the `var` form, then the
	// window-property form (whose `"]` means the bare anchor cannot match
	// it), then the bare form.
	playerResponseAnchors = []*regexp.Regexp{
		regexp.MustCompile(`var ytInitialPlayerResponse\s*=\s*\{`),
		regexp.MustCompile(`window\["ytInitialPlayerResponse"\]\s*=\s*\{`),
		regexp.MustCompile(`ytInitialPlayerResponse\s*=\s*\{`),
	}
```

- [ ] **Step 4: Add the extractor and rewrite the loop**

Add above `extractYtcfgAndPlayerResponse` (i.e. before `watch_page.go:656`):

```go
// extractPlayerResponse returns the decoded ytInitialPlayerResponse object.
//
// Per anchor: match the assignment prefix, brace-scan the literal from the
// `{` the match ends on, unmarshal. The first anchor that yields BOTH a
// balanced literal and parseable JSON wins; anything short of that falls
// through to the next anchor, which is the control flow the lazy patterns
// had. Only the first occurrence of each anchor is considered — also as
// before — because on a real watch page ytInitialPlayerResponse is assigned
// before any page text that could spell it, and taking later candidates
// would open a door page-authored metadata does not have today.
func extractPlayerResponse(html string) (map[string]any, bool) {
	for _, re := range playerResponseAnchors {
		loc := re.FindStringIndex(html)
		if loc == nil {
			continue
		}
		// The match ends ON the opening brace, so rescan from it.
		obj, ok := scanBalancedObject(html[loc[1]-1:])
		if !ok {
			continue
		}
		var pr map[string]any
		if json.Unmarshal([]byte(obj), &pr) != nil {
			continue
		}
		return pr, true
	}
	return nil, false
}
```

Then replace the loop at `watch_page.go:697-729` (`// Extract ytInitialPlayerResponse (try multiple
patterns)` through its closing braces) with:

```go
	// Extract ytInitialPlayerResponse (anchor + balanced scan, see above)
	playerResponse, _ := extractPlayerResponse(html)
	if vd, ok := playerResponse["videoDetails"].(map[string]any); ok {
		if title, ok := vd["title"].(string); ok {
			ytcfg.Title = title
		}
		if author, ok := vd["author"].(string); ok {
			ytcfg.Author = author
		}
		if channelID, ok := vd["channelId"].(string); ok {
			ytcfg.ChannelID = channelID
		}
		if desc, ok := vd["shortDescription"].(string); ok {
			ytcfg.Description = desc
		}
		if thumb, ok := vd["thumbnail"].(map[string]any); ok {
			if thumbs, ok := thumb["thumbnails"].([]any); ok && len(thumbs) > 0 {
				if last, ok := thumbs[len(thumbs)-1].(map[string]any); ok {
					if url, ok := last["url"].(string); ok {
						ytcfg.ThumbnailURL = url
					}
				}
			}
		}
	}
```

(Indexing a nil map is legal and yields the zero value, so the `playerResponse == nil` case needs no
guard. The `var playerResponse map[string]any` declaration that headed the old loop goes away —
`extractYtcfgAndPlayerResponse` still ends in `return ytcfg, playerResponse`.)

- [ ] **Step 5: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestPlayerResponse' ./internal/youtube/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/youtube/
```

Expected: PASS for all three new tests and for the existing suite (notably
`TestExtractAttestationChallengeRejectsHostileOrigin`, whose fixture embeds a hostile payload inside a
`ytInitialPlayerResponse` shortDescription and must keep being rejected by the host gate).

- [ ] **Step 6: Update the spec prose**

In `docs/spec/platform-services.md`, replace lines 214-218:

```markdown
The `ytInitialPlayerResponse` is located by an assignment-PREFIX anchor and then brace-scanned
(`extractPlayerResponse`, `internal/youtube/watch_page.go`), never captured by a non-greedy regex.
The anchors are tried in order:
1. `var ytInitialPlayerResponse\s*=\s*\{`
2. `window["ytInitialPlayerResponse"]\s*=\s*\{`
3. `ytInitialPlayerResponse\s*=\s*\{`

Each match ends on the opening brace; `scanBalancedObject` then walks the literal tracking JS string
state, so a `};` inside `shortDescription` cannot truncate it. The lazy `({.+?});` form this replaces
stopped at the first `};` in the page and failed identically on all three patterns, losing the watch
page's `ScheduledStartTime` and format pool with nothing in the log.
```

Then run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/docs/` — expected PASS
(the citation `extractPlayerResponse` is paired with `internal/youtube/watch_page.go`, which is now
its declaring file).

- [ ] **Step 7: Format, vet, commit**

```bash
gofmt -l ./cmd ./internal ./tools ./web
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/youtube/
staticcheck ./internal/youtube/
git add internal/youtube/watch_page.go internal/youtube/watch_page_test.go docs/spec/platform-services.md
git commit -m "$(cat <<'EOF'
fix(youtube): brace-scan ytInitialPlayerResponse instead of a lazy regex

The three player-response patterns ended in `({.+?});`, which stops at the
first `};` in the page. A shortDescription containing one (a code sample, an
emoticon) produced unbalanced JSON; because all three patterns shared the
flaw they failed identically and PlayerResponse came back nil — silently
losing the watch page's ScheduledStartTime, the reschedule source, and its
format pool. Each pattern now matches only the assignment prefix up to the
opening brace and scanBalancedObject walks the literal, which is the fix the
ytAtN extractor in this same file already carries.

Ledger item T1-5.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
EOF
)"
```

---

### Task 2: Watch-page allocations (T2-16)

Measured on a synthetic 727 KB watch page (see the numbers in Step 1's comment): the current
`extractChatContinuation` costs **164,193 allocs/op, 9.65 MB/op, 22.3 ms/op**, because it decodes the
entire `ytInitialData` blob into `map[string]any` to read one continuation token. The typed
`json.RawMessage` envelope — the shape `channel_membership.go:131-151` already uses — costs **6
allocs/op** (`AllocsPerRun`), 4.5 KB/op, 1.44 ms/op, and is flat in page size. Separately,
`html := string(body)` (`watch_page.go:225`) copies the whole page once more; removing it means every
extractor takes `[]byte`.

**Files:**
- Modify: `internal/youtube/watch_page.go` — `FetchWatchPage:225-230`, `sessionAuthBody:278-282`, `sessionAuthWordAt:289`, `sessionAuthSkipSpace:320`, `sessionAuthMarkerAt:336`, `sessionAuthMarkerInString:362-376` (deleted), `sessionAuthMarkerInBytes:384-398` (renamed), `sessionAuthValue:446`, `sessionAuthClosedBy:475`, `watchPageSessionAuth:506-520`, the retired-twin comment block `:521-537`, `livenessVerdict:571-579`, `extractChatContinuation:581-636`, `extractYtcfgAndPlayerResponse:656-732`, `extractPlayerResponse` (Task 1), `extractAttestationChallenge:785-822`, `scanBalancedObject:925-960`
- Modify: `internal/youtube/watch_page_test.go`, `internal/youtube/session_auth_test.go:338-389` (delete `TestMarkerLookupTwinsAgree`) and its other `watchPageSessionAuth` call sites, `internal/youtube/liveness_markers_live_test.go:61`
- Reuse (no change): `ytInitialDataStartRe` and `extractYtInitialData` (`internal/youtube/channel_membership.go:29`, `:323`)

**Interfaces:**
- Consumes: `extractPlayerResponse(html string) (map[string]any, bool)` from Task 1; `extractYtInitialData(data []byte) ([]byte, bool)`; `sessionAuthMarkerInBytes(b []byte, key string) (SessionAuthState, bool)`.
- Produces (every signature Tasks 4-7 and the tests use):
  - `func scanBalancedObject(s []byte) ([]byte, bool)`
  - `func watchPageSessionAuth(page []byte) SessionAuthState`
  - `func sessionAuthMarkerIn(b []byte, key string) (SessionAuthState, bool)` (renamed from `…InBytes`; `sessionAuthMarkerInString` deleted, `sessionAuthBody` deleted, the five helpers take `[]byte`)
  - `func extractChatContinuation(page []byte) (string, bool, error)`
  - `func extractYtcfgAndPlayerResponse(page []byte) (*YtcfgData, map[string]any)`
  - `func extractPlayerResponse(page []byte) (map[string]any, bool)`
  - `func extractAttestationChallenge(page []byte) (challenge, reason string)`
  - `type watchNextChatEnvelope`, `type liveChatRendererEnvelope`, `type chatContinuationData`

**Planner ruling (recorded for the reviewer):** `sessionAuthMarkerInString` and its pin
`TestMarkerLookupTwinsAgree` are deleted rather than kept. Once `FetchWatchPage` holds bytes, the
string twin has no production caller, and a twin kept alive only by the test that compares it to the
live one is the pointless duplication the owner's standing feedback forbids. `watch_page.go:521-537`
already records one such retirement (`sessionAuthFromBytes`); this task rewrites that block to record
the new state — ONE byte-side marker reader, with `watchPageSessionAuth` and `livenessVerdict`
differing only in the ytcfg fallback, each pinned directly. Cost if wrong: re-introducing a string
reader is a ten-line function.

- [ ] **Step 1: Write the failing benchmark and allocation ceiling**

Append to `internal/youtube/watch_page_test.go`:

```go
// synthChatPage renders a watch page whose ytInitialData carries `filler`
// video renderers ahead of the conversationBar, so the cost of walking the
// document is realistic rather than notional. 500 items is ~180 KB.
func synthChatPage(filler int) []byte {
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html><head><script nonce="q">var ytInitialData = {"filler":[`)
	for i := range filler {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"videoRenderer":{"videoId":"abcdefghij%d","title":{"runs":[{"text":"Some fairly long video title number %d that pads the payload"}]},"thumbnail":{"thumbnails":[{"url":"https://i.ytimg.com/vi/abcdefghij%d/hqdefault.jpg","width":480,"height":360}]},"ownerText":{"runs":[{"text":"Channel Name %d"}]}}}`, i, i, i, i)
	}
	b.WriteString(`],"contents":{"twoColumnWatchNextResults":{"conversationBar":{"liveChatRenderer":` +
		`{"isReplay":false,"continuations":[{"reloadContinuationData":{"continuation":"CONTINUATION_TOKEN_XYZ"}}]}}}}};</script></head><body></body></html>`)
	return []byte(b.String())
}

func BenchmarkExtractChatContinuation(b *testing.B) {
	page := synthChatPage(500)
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := extractChatContinuation(page); err != nil {
			b.Fatal(err)
		}
	}
}

// chatContinuationAllocCeiling bounds extractChatContinuation's allocations.
//
// Measured 2026-09-15 on this exact fixture: the map[string]any decode of the
// whole ytInitialData blob costs 41,093 allocs/op at 500 filler items (and
// 410,554 at 5,000 — it is linear in page size, and a real watch page is
// several times this one). The typed json.RawMessage envelope costs 6,
// independent of page size. The ceiling sits at 16 so ordinary
// encoding/json or Go-version drift does not flap it, while still being
// three orders of magnitude below the shape it replaces.
//
// Mutant named: any re-introduction of a whole-document map[string]any (or a
// json.RawMessage captured at `contents` rather than at liveChatRenderer,
// which copies the document) blows straight through 16.
const chatContinuationAllocCeiling = 16

func TestExtractChatContinuationAllocationCeiling(t *testing.T) {
	page := synthChatPage(500)
	if got := testing.AllocsPerRun(20, func() {
		if _, _, err := extractChatContinuation(page); err != nil {
			t.Fatal(err)
		}
	}); got > chatContinuationAllocCeiling {
		t.Errorf("extractChatContinuation allocated %.0f per call, ceiling %d", got, chatContinuationAllocCeiling)
	}
}

// TestExtractChatContinuationShapes pins the behaviour the envelope must
// preserve, including the two shapes the old `var ytInitialData = (…);</script>`
// regex could not read.
//
// Mutant named: an envelope that keys the continuation list off only
// reloadContinuationData loses the replay row; a locator that keeps the
// `;</script>` terminator fails the "trailing script" row; a locator that
// keeps the `var ` prefix fails the "window property" row.
func TestExtractChatContinuationShapes(t *testing.T) {
	const head = `<script>var ytInitialData = `
	body := func(inner string) string {
		return `{"contents":{"twoColumnWatchNextResults":{"conversationBar":{"liveChatRenderer":` + inner + `}}}}`
	}
	for _, tc := range []struct {
		name       string
		page       string
		wantToken  string
		wantReplay bool
		wantErr    string
	}{
		{
			name:      "reload continuation",
			page:      head + body(`{"isReplay":false,"continuations":[{"reloadContinuationData":{"continuation":"TOK"}}]}`) + `;</script>`,
			wantToken: "TOK",
		},
		{
			name:       "replay continuation",
			page:       head + body(`{"isReplay":true,"continuations":[{"liveChatReplayContinuationData":{"continuation":"RTOK"}}]}`) + `;</script>`,
			wantToken:  "RTOK",
			wantReplay: true,
		},
		{
			name:      "trailing script content, no terminator",
			page:      head + body(`{"continuations":[{"invalidationContinuationData":{"continuation":"ITOK"}}]}`) + `;var other = 1;</script>`,
			wantToken: "ITOK",
		},
		{
			name:      "window property form",
			page:      `<script>window["ytInitialData"] = ` + body(`{"continuations":[{"timedContinuationData":{"continuation":"TTOK"}}]}`) + `;</script>`,
			wantToken: "TTOK",
		},
		{
			name:    "no ytInitialData at all",
			page:    `<html><body>nothing here</body></html>`,
			wantErr: "ytInitialData not found",
		},
		{
			name:    "no liveChatRenderer",
			page:    head + `{"contents":{"twoColumnWatchNextResults":{"conversationBar":{}}}}` + `;</script>`,
			wantErr: "no liveChatRenderer found",
		},
		{
			name:    "renderer with an empty continuation list",
			page:    head + body(`{"isReplay":false,"continuations":[]}`) + `;</script>`,
			wantErr: "no continuations found",
		},
		{
			name:    "continuations carrying no token",
			page:    head + body(`{"continuations":[{"someOtherData":{"x":1}}]}`) + `;</script>`,
			wantErr: "no continuation token found",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tok, replay, err := extractChatContinuation([]byte(tc.page))
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tok != tc.wantToken || replay != tc.wantReplay {
				t.Errorf("got (%q, %v), want (%q, %v)", tok, replay, tc.wantToken, tc.wantReplay)
			}
		})
	}
}
```

Add `"fmt"` and `"strings"` to the test file's imports if absent.

- [ ] **Step 2: Run to verify it fails**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestExtractChatContinuation' ./internal/youtube/`

Expected: FAIL to build —
`cannot use page (variable of type []byte) as string value in argument to extractChatContinuation`.
That compile error IS the red for this signature-changing refactor; the allocation ceiling becomes
observable as soon as the signature lands, and a deliberate detour that keeps the map decode behind
the new signature reports `extractChatContinuation allocated 41093 per call, ceiling 16`.

- [ ] **Step 3: Convert `scanBalancedObject` and `extractAttestationChallenge` to bytes**

In `watch_page.go`, change the scanner (keep its doc comment, appending the second paragraph):

```go
// scanBalancedObject returns the complete `{...}` literal starting at s[0],
// tracking JS string state so braces inside quoted payloads never affect the
// depth count. Returns ok=false when the literal never closes.
//
// []byte rather than string because every caller now holds the raw response
// body: FetchWatchPage stopped copying the ~1-5 MB page into a string for
// the sake of four extractors that only read it.
func scanBalancedObject(s []byte) ([]byte, bool) {
```

The body is unchanged — every operation in it (`len`, index, slice) works identically on `[]byte`.

Then `extractAttestationChallenge` (`:785`):

```go
func extractAttestationChallenge(page []byte) (challenge, reason string) {
	loc := ytAtNOpenRe.FindIndex(page)
	if loc == nil {
		return "", atnNoCall
	}
	// The regex ends on the literal '{'; rescan from there so the object's
	// true extent comes from brace balancing, not from the first `})`.
	obj, ok := scanBalancedObject(page[loc[1]-1:])
	if !ok {
		return "", atnUnbalanced
	}
	// One small copy: the ytAtN literal is kilobytes, and utils.JSToJSON
	// parses a string.
	jsonStr, err := utils.JSToJSON(string(obj), nil, false)
```

The rest of the function is unchanged. Update its five test call sites in `watch_page_test.go`
(`:113`, `:135`, `:153`, `:267`, `:279`, `:294`) to pass `[]byte(page)` / `[]byte(html)`.

- [ ] **Step 4: Collapse the session-auth readers onto bytes**

Delete `type sessionAuthBody` (`:278-282`) and drop the type parameter from the five helpers, each
taking `b []byte`:

```go
func sessionAuthWordAt(b []byte, i int, word string) bool
func sessionAuthSkipSpace(b []byte, i int) int
func sessionAuthMarkerAt(b []byte, i int) (SessionAuthState, bool)
func sessionAuthValue(b []byte, start int) SessionAuthState
func sessionAuthClosedBy(b []byte, i int, q byte) bool
```

Their bodies are unchanged. Delete `sessionAuthMarkerInString` (`:362-376`) and rename
`sessionAuthMarkerInBytes` to `sessionAuthMarkerIn`, keeping its body and updating its doc comment:

```go
// sessionAuthMarkerIn finds the first occurrence of `key` in b that is
// actually a marker and returns its verdict. ok=false means the page carries
// no such marker, whatever else it carries.
//
// The scan continues past an occurrence that is not a marker — a bare
// `"LOGGED_IN"` in some unrelated list, say — rather than giving up on it,
// because the previous colon-bearing key would have skipped it silently and
// found the real marker further on. Answering Unknown there instead would be a
// new way to lose a LoggedIn read, and a lost LoggedIn costs an authenticated
// download its datasyncID binding.
//
// Total work stays linear in the page: each iteration resumes past the
// previous match, so the Index scans partition the body.
//
// The []byte(key) conversion is hoisted out of the loop and never escapes
// (bytes.Index does not retain it), which is what keeps the zero-allocation
// pins in session_auth_test.go holding.
func sessionAuthMarkerIn(b []byte, key string) (SessionAuthState, bool) {
```

Rewrite `watchPageSessionAuth` (`:506-520`), keeping its whole doc comment and replacing only the
signature, the two lookups and the fallback:

```go
func watchPageSessionAuth(page []byte) SessionAuthState {
	if st, ok := sessionAuthMarkerIn(page, sessionAuthKey); ok {
		return st
	}
	if st, ok := sessionAuthMarkerIn(page, sessionAuthCamelKey); ok {
		return st
	}
	// No login key, but a real watch-page shell: YouTube answered as a page
	// it would have stamped the key onto, so an anonymous session is the
	// sound reading.
	if bytes.Contains(page, []byte(sessionAuthYtcfgMark)) {
		return SessionAuthLoggedOut
	}
	return SessionAuthUnknown
}
```

Replace the retired-twin comment block at `:521-537` with:

```go
// There used to be a string twin of the marker scan here
// (sessionAuthMarkerInString), plus a TestMarkerLookupTwinsAgree that pinned
// it against the byte-side one. Both are gone: FetchWatchPage now hands the
// raw response bytes to every extractor rather than copying the ~1-5 MB page
// into a string, so the string side had no production caller left, and a twin
// kept alive only by the test that compares it to the live one is duplication
// pretending to be coverage.
//
// What remains is ONE reader. watchPageSessionAuth and livenessVerdict below
// differ in exactly one thing — the ytcfg-bootstrap fallback, which is sound
// for a watch page and unsafe for the liveness probe — and each is pinned
// directly rather than inferred from the other.

// livenessVerdict is watchPageSessionAuth over raw bytes with the ytcfg
```

(`livenessVerdict`'s own two `sessionAuthMarkerInBytes` calls become `sessionAuthMarkerIn`.)

- [ ] **Step 5: Convert the ytcfg extractor to bytes**

`extractYtcfgAndPlayerResponse` (`:656`) and `extractPlayerResponse` (Task 1) take `page []byte`;
every `FindStringSubmatch` becomes `FindSubmatch`, `MatchString` becomes `Match`, and
`FindStringIndex` becomes `FindIndex`:

```go
func extractYtcfgAndPlayerResponse(page []byte) (*YtcfgData, map[string]any) {
	ytcfg := &YtcfgData{}

	// Extract player URL
	if m := jsURLRegex.FindSubmatch(page); m != nil {
		ytcfg.PlayerURL = normalizePlayerJSURL(string(m[1]))
	}

	// Extract visitor data. string(m[1]) is the copy that breaks the
	// substring→backing-array alias: regexp submatches over `page` are
	// slices of the full ~5 MB watch-page response, and storing one
	// anywhere persistent (Service.visitorData, sessionCache keys, …)
	// would pin the entire page in memory. Per-poll leak observed at
	// ~5 MB/min in pprof. On the string side this needed an explicit
	// strings.Clone; converting a []byte submatch always copies.
	if m := visitorDataRegex.FindSubmatch(page); m != nil {
		ytcfg.VisitorData = string(m[1])
	}

	// Extract session index
	if m := sessionIndexRegex.FindSubmatch(page); m != nil {
		if idx, err := strconv.Atoi(string(m[1])); err == nil {
			ytcfg.SessionIndex = &idx
		}
	}

	// Extract delegated session ID (copied — see VisitorData comment).
	if m := delegatedSessionRegex.FindSubmatch(page); m != nil {
		ytcfg.DelegatedSessionID = string(m[1])
	}

	// Extract datasync ID (copied — see VisitorData comment).
	if m := dataSyncIDRegex.FindSubmatch(page); m != nil {
		ytcfg.DataSyncID = string(m[1])
	}
```

`ytcfg.GvsBindToVideoID = gvsBindVideoIDRegex.Match(page)`, and the player-response call becomes
`extractPlayerResponse(page)`. In `extractPlayerResponse`, `re.FindStringIndex(html)` becomes
`re.FindIndex(page)` and the unmarshal becomes `json.Unmarshal(obj, &pr)` (no `[]byte(...)`).
`normalizePlayerJSURL` keeps its `string` parameter; its final `strings.Clone` is now redundant but
harmless — leave it and its comment alone, the embed path still passes a substring.

`extractEncryptedHostFlags` and `FetchEmbedPage` are **out of scope**: the embed page is small and the
ledger does not cite it. They keep their `string(body)`.

- [ ] **Step 6: Replace the chat-continuation decode**

Add the envelope types immediately above `extractChatContinuation` and rewrite the function:

```go
// watchNextChatEnvelope decodes ONLY the path from ytInitialData down to the
// live-chat renderer. Everything else in the document — the megabytes of
// video renderers, the sidebar, the engagement panels — is skipped by
// encoding/json without being materialised.
//
// The renderer itself stays a json.RawMessage so its two load-bearing fields
// are decoded from a kilobyte-sized slice in a second pass. Capturing higher
// up (at `contents`, say) would defeat the point: RawMessage.UnmarshalJSON
// COPIES the bytes it captures, and at `contents` that is most of the page.
// Same shape, same reasoning, as membershipTabHeader in channel_membership.go.
type watchNextChatEnvelope struct {
	Contents struct {
		TwoColumnWatchNextResults struct {
			ConversationBar struct {
				LiveChatRenderer json.RawMessage `json:"liveChatRenderer"`
			} `json:"conversationBar"`
		} `json:"twoColumnWatchNextResults"`
	} `json:"contents"`
}

// liveChatRendererEnvelope is the renderer's two load-bearing fields. The
// four continuation shapes are tried per element in the order the live chat
// API emits them; a renderer carrying several keeps today's winner.
type liveChatRendererEnvelope struct {
	IsReplay      bool `json:"isReplay"`
	Continuations []struct {
		Reload       *chatContinuationData `json:"reloadContinuationData"`
		Invalidation *chatContinuationData `json:"invalidationContinuationData"`
		Timed        *chatContinuationData `json:"timedContinuationData"`
		Replay       *chatContinuationData `json:"liveChatReplayContinuationData"`
	} `json:"continuations"`
}

type chatContinuationData struct {
	Continuation string `json:"continuation"`
}

// extractChatContinuation pulls the live-chat continuation token (and its
// isReplay flag) out of a watch page's ytInitialData blob. Returns an empty
// token + descriptive error when chat isn't available (most non-live videos,
// streams with chat disabled, etc.) — callers treat that as "no chat" rather
// than a hard failure. Shape mirrors chat.ExtractChatContinuation; duplicated
// here so the youtube package owns its own extraction and watch_page.go can
// drop the raw page before returning. json.Unmarshal allocates fresh strings,
// so the returned token does not alias the page's backing array.
//
// The blob is located by extractYtInitialData (channel_membership.go), the
// same brace-depth scan the membership path uses — which also drops the old
// regex's requirement that the assignment be spelled `var ytInitialData = `
// and be terminated by `;</script>`.
//
// A decode error is reported ONLY when the renderer did not come out of it:
// encoding/json records the first type error and keeps decoding, so a
// mismatch in an unrelated subtree must not lose a token we did read. That is
// what the map[string]any walk this replaces did implicitly, and the point of
// replacing it is cost, not behaviour: at 500 filler renderers the map decode
// cost 41,093 allocations to read one string, and it is linear in page size.
func extractChatContinuation(page []byte) (string, bool, error) {
	raw, ok := extractYtInitialData(page)
	if !ok {
		return "", false, fmt.Errorf("ytInitialData not found")
	}

	var env watchNextChatEnvelope
	err := json.Unmarshal(raw, &env)
	rendererRaw := env.Contents.TwoColumnWatchNextResults.ConversationBar.LiveChatRenderer
	if len(rendererRaw) == 0 || string(rendererRaw) == "null" {
		if err != nil {
			return "", false, fmt.Errorf("parse ytInitialData: %w", err)
		}
		return "", false, fmt.Errorf("no liveChatRenderer found")
	}

	var renderer liveChatRendererEnvelope
	if err := json.Unmarshal(rendererRaw, &renderer); err != nil {
		return "", false, fmt.Errorf("parse ytInitialData: %w", err)
	}
	if len(renderer.Continuations) == 0 {
		return "", false, fmt.Errorf("no continuations found")
	}

	for _, cont := range renderer.Continuations {
		for _, data := range [...]*chatContinuationData{cont.Reload, cont.Invalidation, cont.Timed, cont.Replay} {
			if data != nil && data.Continuation != "" {
				return data.Continuation, renderer.IsReplay, nil
			}
		}
	}

	return "", false, fmt.Errorf("no continuation token found")
}
```

Delete `ytInitialDataRegex` (`watch_page.go:51-54`) — `extractYtInitialData` replaces it and nothing
else in the package uses it. (`internal/chat/api.go` keeps its own copy; that twin is Arc 7's file
set and is deliberately untouched here.)

- [ ] **Step 7: Drop the `string(body)` copy in `FetchWatchPage`**

Replace `watch_page.go:225-230`:

```go
	// No string(body) here: every extractor below reads the page as bytes,
	// so the ~1-5 MB copy this used to make on every watch-page fetch —
	// monitor polls and quality probes included — is gone.
	sessionAuth := watchPageSessionAuth(body)

	ytcfg, playerResponse := extractYtcfgAndPlayerResponse(body)
	chatContinuation, chatIsReplay, chatErr := extractChatContinuation(body)
	attestationChallenge, attestationReason := extractAttestationChallenge(body)
```

Add `"bytes"` to the import block if Step 4 needs it (it is already imported — `sessionAuthMarkerInBytes`
uses it). Remove `"strings"` from the imports only if nothing else in the file uses it — `isConsentRedirect`
and `normalizePlayerJSURL` still do, so it stays.

- [ ] **Step 8: Update the remaining test call sites**

- `session_auth_test.go`: `watchPageSessionAuth(tt.html)` (`:66`), `watchPageSessionAuth(tt.body)`
  (`:147`, `:191`), `watchPageSessionAuth(page)` (`:220`), `watchPageSessionAuth(body)` (`:251`),
  `watchPageSessionAuth(string(shell))` (`:421`) — wrap each argument in `[]byte(...)` (and drop the
  now-pointless `string(...)` at `:421`). Delete `TestMarkerLookupTwinsAgree` (`:338-389`, doc comment
  included).
- `liveness_markers_live_test.go:61`: `watchPageSessionAuth(string(body))` → `watchPageSessionAuth(body)`.
- `watch_page_test.go`: the `extractAttestationChallenge` and `extractYtcfgAndPlayerResponse` call
  sites take `[]byte(...)`; Task 1's `extractPlayerResponse(page)` in the equivalence test becomes
  `extractPlayerResponse([]byte(page))`.

- [ ] **Step 9: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/youtube/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run XXX -bench BenchmarkExtractChatContinuation -benchtime 50x ./internal/youtube/
```

Expected: PASS, including `TestLivenessVerdictDoesNotAllocate` (unchanged — `livenessVerdict` still
never converts bytes to a string) and `TestExtractChatContinuationAllocationCeiling`. The benchmark
should report on the order of `~1.4 ms/op  ~4.5 kB/op  ~20 allocs/op` on the 500-item fixture; a
number in the tens of thousands means the map decode survived somewhere.

- [ ] **Step 10: Format, vet, staticcheck, commit**

```bash
gofmt -l ./cmd ./internal ./tools ./web
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/youtube/
staticcheck ./internal/youtube/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/docs/
git add internal/youtube/watch_page.go internal/youtube/watch_page_test.go internal/youtube/session_auth_test.go internal/youtube/liveness_markers_live_test.go
git commit -m "$(cat <<'EOF'
perf(youtube): read the watch page as bytes and decode chat lazily

extractChatContinuation decoded the entire ytInitialData blob into a
map[string]any to read one continuation token: 41,093 allocations on a 180 KB
fixture, 410,554 on a 1.8 MB one — linear in page size, on a monitor cadence.
A typed envelope that keeps only liveChatRenderer as a json.RawMessage (the
shape channel_membership.go already uses) costs 6, flat. FetchWatchPage also
stopped copying the whole page into a string: every extractor now takes the
response bytes, which retires the string twin of the session-auth marker scan
and the test that pinned it against the live one.

Ledger item T2-16. Behaviour pinned by a new shapes table, an AllocsPerRun
ceiling of 16 and BenchmarkExtractChatContinuation.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
EOF
)"
```

---

### Task 3: Cookie jar reload short-circuit (T2-23)

`GetVideoInfo` calls `SyncCookies` (`internal/youtube/service.go:192`) and then
`GetVideoInfoAuthenticated` calls it again (`player_api_strategy.go:106`), so `cookies.txt` is read and
parsed twice per extraction. `CookieJar.Load` has no short-circuit.

**Files:**
- Modify: `internal/cookies/jar.go:68-78` (struct), `:192-210` (Load)
- Create: `internal/cookies/jar_reload_memo_test.go`

**Interfaces:**
- Consumes: `cookieRow(domain, expiry, name, value string) string` and
  `loadRowsInto(t *testing.T, path string, rows []string) *CookieJar` (`internal/cookies/jar_storage_test.go:15`, `:21`);
  `(*CookieJar).loadFrom(data []byte, filePath string)` (`jar.go:223`);
  `(*CookieJar).GetCookieFor(p Platform, name string) string` (`jar.go:424`).
- Produces: `const cookieJarStatSettle = 2 * time.Second`; three unexported `CookieJar` fields
  `loadedSize int64`, `loadedMod time.Time`, `loadedMemo bool`. No exported surface changes; callers
  are untouched.

**Planner ruling (recorded for the reviewer):** the memo carries a git-style "racily clean" settle
window. A size+mtime pair alone is unsafe here because `cookies.txt` has two writers that can write
twice inside one filesystem timestamp tick — the 2.8.7 verify-and-roll-back path writes the new set,
verifies, then restores the previous one, and a restore differing from the new set only in expiry
digits has the SAME byte length. Trusting the pair in that window would leave the jar holding
credentials the file no longer contains. Cost if wrong: one extra read of a ~10 KB file per pass in
the first two seconds after a write, which is exactly what happens today on every pass.

- [ ] **Step 1: Write the failing tests**

Create `internal/cookies/jar_reload_memo_test.go`:

```go
package cookies

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// agedCookieFile writes rows to path and back-dates its mtime by age, so the
// stat memo is outside the racily-clean settle window and may be trusted.
func agedCookieFile(t *testing.T, path string, rows []string, age time.Duration) {
	t.Helper()
	content := "# Netscape HTTP Cookie File\n"
	for _, r := range rows {
		content += r + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

func futureExpiry() string {
	return strconv.FormatInt(time.Now().Add(24*time.Hour).Unix(), 10)
}

// TestLoadShortCircuitsOnAnUnchangedFile is ledger item T2-23: GetVideoInfo
// and GetVideoInfoAuthenticated both SyncCookies, so cookies.txt was read and
// parsed twice per extraction.
//
// The seam is the jar itself: loadFrom writes state the FILE does not carry
// and leaves the memo cleared-then-unrecorded, so if the second Load re-reads
// the file it will overwrite that state with the file's.
//
// Mutant named: deleting the short-circuit (or comparing only the path) makes
// the second Load re-parse and SID goes back to "from-file".
func TestLoadShortCircuitsOnAnUnchangedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.txt")
	agedCookieFile(t, path, []string{cookieRow(".youtube.com", futureExpiry(), "SID", "from-file")}, time.Hour)

	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	jar.loadFrom([]byte("# Netscape HTTP Cookie File\n"+
		cookieRow(".youtube.com", futureExpiry(), "SID", "from-memory")+"\n"), path)

	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	if got := jar.GetCookieFor(PlatformYouTube, "SID"); got != "from-memory" {
		t.Errorf("SID = %q, want %q — the second Load re-parsed an unchanged file", got, "from-memory")
	}
}

// TestLoadReparsesAfterAWrite pins the invalidation half.
//
// Mutant named: a memo that keys on the path alone, or that is recorded
// before the read rather than after it, never notices the rewrite.
func TestLoadReparsesAfterAWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.txt")
	agedCookieFile(t, path, []string{cookieRow(".youtube.com", futureExpiry(), "SID", "first")}, time.Hour)

	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	agedCookieFile(t, path, []string{cookieRow(".youtube.com", futureExpiry(), "SID", "second-value")}, 30*time.Minute)
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	if got := jar.GetCookieFor(PlatformYouTube, "SID"); got != "second-value" {
		t.Errorf("SID = %q, want %q — a rewritten file was served from the memo", got, "second-value")
	}
}

// TestLoadNeverTrustsAFreshlyWrittenFile pins the racily-clean rule: a file
// whose mtime is inside cookieJarStatSettle of the load is never memoised.
//
// Mutant named: dropping the settle window. cookies.txt has writers that
// write twice inside one timestamp tick — the verify-and-roll-back pass
// writes the new set and then restores the previous one, and a restore that
// differs only in expiry digits has the same byte length — so a size+mtime
// pair alone would leave the jar holding credentials the file no longer has.
func TestLoadNeverTrustsAFreshlyWrittenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.txt")
	agedCookieFile(t, path, []string{cookieRow(".youtube.com", futureExpiry(), "SID", "from-file")}, 0)

	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	jar.loadFrom([]byte("# Netscape HTTP Cookie File\n"+
		cookieRow(".youtube.com", futureExpiry(), "SID", "from-memory")+"\n"), path)

	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	if got := jar.GetCookieFor(PlatformYouTube, "SID"); got != "from-file" {
		t.Errorf("SID = %q, want %q — a file written this instant was memoised", got, "from-file")
	}
}

// TestLoadOfADeletedFileStillClearsTheJar pins that the memo is consulted
// only after a successful stat.
//
// Mutant named: a short-circuit placed ahead of the stat (or one that ignores
// the stat error) reports success and leaves a deleted credential file's
// cookies live in memory forever.
func TestLoadOfADeletedFileStillClearsTheJar(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.txt")
	agedCookieFile(t, path, []string{cookieRow(".youtube.com", futureExpiry(), "SID", "from-file")}, time.Hour)

	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := jar.Load(path); err != nil {
		t.Fatalf("Load of a missing file must stay a nil-error no-op: %v", err)
	}
	if got := jar.GetCookieFor(PlatformYouTube, "SID"); got != "" {
		t.Errorf("SID = %q, want empty — a deleted cookie file was served from the memo", got)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestLoad(ShortCircuits|Reparses|NeverTrusts|OfADeleted)' ./internal/cookies/`

Expected: FAIL to build with `undefined: cookieJarStatSettle`. Remove that one reference from the
comment (it is only mentioned in prose) — the real red is
`TestLoadShortCircuitsOnAnUnchangedFile: SID = "from-file", want "from-memory" — the second Load
re-parsed an unchanged file`. The other three PASS on the current code, which is correct: they are the
guard rails the new short-circuit must not break.

- [ ] **Step 3: Add the memo fields**

In `internal/cookies/jar.go`, extend the struct (`:68-78`) — add `"time"` to the imports if absent:

```go
	youtube  map[string]cookieEntry // name -> entry
	twitch   map[string]cookieEntry // name -> entry
	filePath string
	// loadedSize/loadedMod describe the file Load last parsed; loadedMemo
	// says whether that pair may be trusted. All three are written only by
	// Load, under j.mu, and cleared by loadFrom (a buffer a caller handed
	// us says nothing about what is on disk).
	loadedSize int64
	loadedMod  time.Time
	loadedMemo bool
	logger     cookieJarLogger // optional; set via SetLogger
```

And the constant, next to the other package constants:

```go
// cookieJarStatSettle is how long a file must have been untouched before its
// (size, mtime) pair may be memoised — git's "racily clean" rule, for the
// same reason. cookies.txt has writers that write it twice inside one
// filesystem timestamp tick: the verify-and-roll-back pass writes the new set,
// verifies it, and restores the previous one, and a restore that differs only
// in expiry digits has the SAME byte length. Trusting the pair in that window
// would leave the jar holding credentials the file no longer contains. The
// cost of the rule is one extra read of a ~10 KB file during the two seconds
// after a write — which is what every pass does today.
const cookieJarStatSettle = 2 * time.Second
```

- [ ] **Step 4: Short-circuit `Load`**

Replace the body of `Load` (`jar.go:192-210`), appending a paragraph to its existing doc comment:

```go
// Load re-reads the file only when it has actually changed. A stat whose
// (size, mtime) match the pair recorded by the last successful parse — and
// only when that pair was outside cookieJarStatSettle — returns immediately;
// everything else reads and parses as before. Both YouTube extraction entry
// points SyncCookies (internal/youtube/service.go and player_api_strategy.go),
// so the file was read and parsed twice per extraction, forever, for an
// answer that had not changed.
func (j *CookieJar) Load(filePath string) error {
	// Stat FIRST: a file that has been deleted must fall through to the
	// read below and clear the jar, not be served from a memo.
	if st, statErr := os.Stat(filePath); statErr == nil && st.Mode().IsRegular() {
		j.mu.RLock()
		unchanged := j.loadedMemo &&
			j.filePath == filePath &&
			j.loadedSize == st.Size() &&
			j.loadedMod.Equal(st.ModTime())
		j.mu.RUnlock()
		if unchanged {
			return nil
		}
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			// No cookies file is OK; clear state so callers see an empty jar.
			j.mu.Lock()
			j.filePath = filePath
			j.youtube = make(map[string]cookieEntry)
			j.twitch = make(map[string]cookieEntry)
			j.loadedMemo = false
			j.mu.Unlock()
			return nil
		}
		return fmt.Errorf("failed to read cookie file: %w", err)
	}

	j.loadFrom(data, filePath)

	// Stat AFTER the read, not before: a write that landed while we were
	// reading leaves an mtime newer than the bytes we hold, and the settle
	// check then refuses to memoise it.
	if st, statErr := os.Stat(filePath); statErr == nil && st.Mode().IsRegular() {
		j.mu.Lock()
		if j.filePath == filePath {
			j.loadedSize = st.Size()
			j.loadedMod = st.ModTime()
			j.loadedMemo = time.Since(st.ModTime()) >= cookieJarStatSettle
		}
		j.mu.Unlock()
	}
	return nil
}
```

And in `loadFrom`, inside its existing final `j.mu.Lock()` block (`jar.go:339-343`), add
`j.loadedMemo = false` beside the other assignments, with the comment:

```go
	j.mu.Lock()
	j.filePath = filePath
	j.youtube = youtube
	j.twitch = twitch
	// A caller-supplied buffer says nothing about the file on disk, so the
	// stat memo cannot survive it. Load re-records it after its own read.
	j.loadedMemo = false
	j.mu.Unlock()
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestLoad' ./internal/cookies/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/cookies/... ./internal/youtube/...
```

Expected: PASS everywhere except the known-red
`TestRefreshWarnsWhenTheExpiredTwitchLoginIsPruned` documented in Global Constraints. Pay particular
attention to `TestGetTwitchCredentialsIsAtomicAcrossReload` (`jar_storage_test.go:648`): its writes are
instantaneous, so the settle window keeps every one of its reloads honest.

- [ ] **Step 6: Format, vet, staticcheck, commit**

```bash
gofmt -l ./cmd ./internal ./tools ./web
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/cookies/
staticcheck ./internal/cookies/
git add internal/cookies/jar.go internal/cookies/jar_reload_memo_test.go
git commit -m "$(cat <<'EOF'
perf(cookies): skip the jar re-parse when cookies.txt has not changed

Both YouTube extraction entry points SyncCookies — service.go's GetVideoInfo
and player_api_strategy.go's GetVideoInfoAuthenticated — so the cookie file
was read and parsed twice per extraction for an answer that had not changed.
Load now records the (size, mtime) pair of the file it parsed and returns
early when a fresh stat matches it.

The pair is memoised only when the file has been untouched for
cookieJarStatSettle (2 s), git's racily-clean rule: cookies.txt has writers
that write it twice inside one timestamp tick — the verify-and-roll-back pass
restores a previous set that can differ from the new one only in expiry
digits, i.e. at the same byte length — and a pair trusted there would leave
the jar holding credentials the file no longer contains.

Ledger item T2-23.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
EOF
)"
```

---

### Task 4: Deadline-aware player retries (T3-27)

`doRetryRequest` sleeps 1 s, 2 s, 4 s between attempts regardless of the caller's deadline. Mid-download
403 credential recovery runs under `min(45 s, MaxTimeout/3)` — as little as 10 s at the configured floor
(the comment at `player_api_strategy.go:740-742`) — so the backoff can consume the whole budget and the
caller learns only `context.DeadlineExceeded` instead of the HTTP status that caused the retries.

**Spec ruling carried forward (§6.4):** no concurrent client cascade in this chain. Cost if wrong:
recovery stays sequential, but now bounded.

**Files:**
- Modify: `internal/youtube/player_api_strategy.go:826-895` (`doRetryRequest`)
- Create: `internal/youtube/player_api_retry_test.go`

**Interfaces:**
- Consumes: `utils.Sleep(ctx context.Context, d time.Duration) error` (`internal/utils/async.go:11`);
  `noopLogger{}` and `NewPlayerAPI(auth *Auth, logger …) *PlayerAPI` (`internal/youtube/player_api.go:69`).
- Produces: `var playerRetryBackoffBase = time.Second` — a package-level var, not a const, so the tests
  scale the schedule without a real multi-second sleep. Production never writes it. (`internal/youtube`
  has no `t.Parallel()` test, so the swap is race-free; the tests restore it via `t.Cleanup`.)

- [ ] **Step 1: Write the failing tests**

Create `internal/youtube/player_api_retry_test.go`:

```go
package youtube

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// scaleRetryBackoff shrinks the 1/2/4 s schedule so a deadline test costs
// milliseconds. internal/youtube runs no parallel tests, so the swap is safe.
func scaleRetryBackoff(t *testing.T, base time.Duration) {
	t.Helper()
	previous := playerRetryBackoffBase
	playerRetryBackoffBase = base
	t.Cleanup(func() { playerRetryBackoffBase = previous })
}

func newRetryTestAPI() *PlayerAPI {
	return NewPlayerAPI(NewAuth(cookies.NewCookieJar(), noopLogger{}), noopLogger{})
}

// TestDoRetryRequestStopsWhenTheBackoffWouldOutlastTheDeadline is ledger item
// T3-27. The recovery budget is min(45 s, MaxTimeout/3) — 10 s at the floor —
// and the retry ladder alone is 7 s.
//
// Mutant named: an unconditional utils.Sleep. It burns the rest of the budget
// inside the sleep and returns context.DeadlineExceeded, discarding the 503
// that is the actual reason the caller is being told no.
func TestDoRetryRequestStopsWhenTheBackoffWouldOutlastTheDeadline(t *testing.T) {
	scaleRetryBackoff(t, 40*time.Millisecond)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	_, err := newRetryTestAPI().doRetryRequest(ctx, srv.URL, []byte(`{}`), nil, nil, "Innertube")
	if err == nil {
		t.Fatal("a 503-forever server must produce an error")
	}
	if !strings.Contains(err.Error(), "HTTP 503") {
		t.Errorf("err = %v, want the last HTTP error (HTTP 503), not the deadline", err)
	}
	if n := hits.Load(); n != 2 {
		t.Errorf("server saw %d requests, want 2 (the immediate attempt plus one that fit its backoff)", n)
	}
}

// TestDoRetryRequestKeepsRetryingInsideABudget pins that the new check fires
// only when the sleep genuinely would not fit.
//
// Mutant named: a check that returns early whenever ctx HAS a deadline. It
// gives up after the first attempt and never sees the 200.
func TestDoRetryRequestKeepsRetryingInsideABudget(t *testing.T) {
	scaleRetryBackoff(t, 5*time.Millisecond)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"videoDetails":{"videoId":"abc12345678","title":"T"}}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	info, err := newRetryTestAPI().doRetryRequest(ctx, srv.URL, []byte(`{}`), nil, nil, "Innertube")
	if err != nil {
		t.Fatalf("doRetryRequest: %v", err)
	}
	if info == nil {
		t.Fatal("info is nil after a 200")
	}
	if n := hits.Load(); n != 3 {
		t.Errorf("server saw %d requests, want 3", n)
	}
}

// TestDoRetryRequestWithoutADeadlineUsesEveryAttempt pins the `ok` half of
// ctx.Deadline().
//
// Mutant named: `dl, _ := ctx.Deadline()` with the bool dropped. A
// deadline-less context yields the zero Time, time.Until(zero) is hugely
// negative, and the guard fires on the FIRST retry — collapsing four attempts
// to one for every caller that passes context.Background().
func TestDoRetryRequestWithoutADeadlineUsesEveryAttempt(t *testing.T) {
	scaleRetryBackoff(t, time.Millisecond)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	_, err := newRetryTestAPI().doRetryRequest(context.Background(), srv.URL, []byte(`{}`), nil, nil, "Innertube")
	if err == nil {
		t.Fatal("a 503-forever server must produce an error")
	}
	if n := hits.Load(); n != 4 {
		t.Errorf("server saw %d requests, want 4 — every attempt must run when there is no deadline", n)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestDoRetryRequest' ./internal/youtube/`

Expected: FAIL to build with `undefined: playerRetryBackoffBase`. Add only the var (Step 3's first
hunk) and re-run to see the real red:
`TestDoRetryRequestStopsWhenTheBackoffWouldOutlastTheDeadline: err = context deadline exceeded, want
the last HTTP error (HTTP 503), not the deadline`.

- [ ] **Step 3: Make the backoff deadline-aware**

In `internal/youtube/player_api_strategy.go`, add above `doRetryRequest`:

```go
// playerRetryBackoffBase is the first retry delay; attempt n waits
// base<<(n-1), i.e. 1 s, 2 s, 4 s. A var rather than a const purely so tests
// can scale the ladder down instead of sleeping for real seconds; production
// never writes it.
var playerRetryBackoffBase = time.Second
```

Extend `doRetryRequest`'s doc comment and replace its backoff block (`:836-842`):

```go
// doRetryRequest performs an HTTP POST with retry logic (up to 4 attempts with
// exponential backoff). Retries on transport errors, partial body reads,
// 5xx/429 responses, and JSON unmarshal failures — all of which have been
// observed as transient CDN issues that would otherwise unnecessarily push
// callers through their full fallback chain.
//
// The backoff is bounded by the CALLER's deadline. Mid-download 403 credential
// recovery runs under min(45 s, MaxTimeout/3) — as little as 10 s at the
// configured floor — and the ladder alone is 7 s, so an unconditional sleep
// could spend the whole budget and then report context.DeadlineExceeded,
// throwing away the HTTP status that is the actual reason the caller is being
// told no. A sleep that would not leave the deadline room for the attempt it
// precedes is not taken at all; the last real error is returned instead.
func (p *PlayerAPI) doRetryRequest(ctx context.Context, apiURL string, body []byte, headers map[string]string, ytcfg *YtcfgData, clientLabel string) (*VideoInfo, error) {
	var playerURL string
	if ytcfg != nil {
		playerURL = ytcfg.PlayerURL
	}

	var lastErr error
	for attempt := range 4 {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if attempt > 0 {
			// Exponential backoff: 1s, 2s, 4s (matching p-retry default factor=2, minTimeout=1000)
			delay := playerRetryBackoffBase << (attempt - 1)
			if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= delay {
				// Every `continue` above sets lastErr, so attempt > 0
				// always has one to return.
				p.logger.Debug("[PlayerApi] retry budget exhausted, returning the last error",
					slog.String("client", clientLabel),
					slog.Int("attempt", attempt+1),
					slog.Duration("wouldSleep", delay))
				return nil, lastErr
			}
			if err := utils.Sleep(ctx, delay); err != nil {
				return nil, err
			}
		}
```

The remainder of the function is unchanged.

- [ ] **Step 4: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestDoRetryRequest' ./internal/youtube/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/youtube/
```

Expected: PASS.

- [ ] **Step 5: Format, vet, staticcheck, commit**

```bash
gofmt -l ./cmd ./internal ./tools ./web
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/youtube/
staticcheck ./internal/youtube/
git add internal/youtube/player_api_strategy.go internal/youtube/player_api_retry_test.go
git commit -m "$(cat <<'EOF'
fix(youtube): bound the player-API retry backoff by the caller's deadline

doRetryRequest slept 1+2+4 s between attempts regardless of the context it
was handed. Mid-download 403 credential recovery runs under
min(45 s, MaxTimeout/3) — 10 s at the configured floor — so the ladder alone
could spend the whole budget inside a sleep and hand the caller
context.DeadlineExceeded instead of the HTTP status that caused the retries.
A sleep that would not leave the deadline room for the attempt it precedes is
now skipped and the last real error returned.

Ledger item T3-27. The spec's concurrent client cascade stays unbuilt by
ruling (§6.4); recovery remains sequential but is now bounded.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
EOF
)"
```

---

### Task 5: Delete the dead attestation-challenge path (T4-31, ruling R1)

Owner ruling R1: delete. `fetchWithClient` (`:541`) and `fetchWithEmbedded` (`:751`) never read their
`challenge` parameter — both mint through `GeneratePoTokenString(ctx, videoID, false)` — and
`PotTokenProvider.GeneratePlayerPoToken` (`player_api.go:34`) has no caller anywhere;
`bgutils.PotProvider.GeneratePlayerPoToken` (`pot_provider.go:196`) is reached only from its own three
tests. The sidecar protocol (`Sidecar.GeneratePlayerPoToken`, `Sidecar.GenerateGvsPoToken`),
`generateAndMint` and `generatePoTokenChallenge` are explicitly left alone.

**Files:**
- Modify: `internal/youtube/player_api_strategy.go` — call sites `:73`, `:89`, `:155`, `:163`, `:185`, `:191`, `:297`, `:323`, `:425`, `:443`; signatures `:541`, `:751`; the `ProbeVideoDate` comment `:70-72`
- Modify: `internal/youtube/player_api.go:23-35` (interface)
- Modify: `internal/bgutils/pot_provider.go:181-205` (delete the method), `:206-214` (its doc reference)
- Modify: `internal/bgutils/pot_provider_test.go:1142-1221` (delete three tests)
- Modify: `docs/spec/platform-services.md:863`

**Interfaces:**
- Produces:
  - `func (p *PlayerAPI) fetchWithClient(ctx context.Context, videoID string, client constants.YouTubeClientConfig, ytcfg *YtcfgData, sts int) (*VideoInfo, error)`
  - `func (p *PlayerAPI) fetchWithEmbedded(ctx context.Context, videoID string, ytcfg *YtcfgData, sts int, fetchEmbedPage bool) (*VideoInfo, error)`
  - `type PotTokenProvider interface { GeneratePoTokenString(ctx context.Context, contentBinding string, bypassCache bool) (string, error) }`
- Unchanged and deliberately out of scope: `WatchPageResult.AttestationChallenge`,
  `VideoInfo.AttestationChallenge`, `extractAttestationChallenge`, the two
  `"no attestation challenge from watch page"` Debug lines (`:125`, `:403`), and every bgutils
  challenge parameter below `generatePoTokenChallenge`.

- [ ] **Step 1: Write the failing test**

The deletion's observable contract is the interface shape, so the pin is a compile-time one. Append to
`internal/youtube/player_api_retry_test.go`:

```go
// minimalPotProvider implements PotTokenProvider with exactly the method the
// player API calls. It is the compile-time pin for ruling R1: while the
// interface still declared GeneratePlayerPoToken, this type did not satisfy
// it, and the assignment below did not build.
//
// Mutant named: re-adding a method to PotTokenProvider that no call site in
// this package uses breaks this file rather than quietly widening what every
// provider must implement.
type minimalPotProvider struct{ binding string }

func (m *minimalPotProvider) GeneratePoTokenString(ctx context.Context, contentBinding string, bypassCache bool) (string, error) {
	m.binding = contentBinding
	return "pot-" + contentBinding, nil
}

func TestPotTokenProviderNeedsOnlyGeneratePoTokenString(t *testing.T) {
	var provider PotTokenProvider = &minimalPotProvider{}
	got, err := provider.GeneratePoTokenString(context.Background(), "dQw4w9WgXcQ", false)
	if err != nil {
		t.Fatalf("GeneratePoTokenString: %v", err)
	}
	if got != "pot-dQw4w9WgXcQ" {
		t.Errorf("token = %q, want %q", got, "pot-dQw4w9WgXcQ")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestPotTokenProviderNeedsOnly ./internal/youtube/`

Expected: FAIL to build with
`cannot use &minimalPotProvider{} (value of type *minimalPotProvider) as PotTokenProvider value: *minimalPotProvider does not implement PotTokenProvider (missing method GeneratePlayerPoToken)`.

- [ ] **Step 3: Narrow the interface**

In `internal/youtube/player_api.go`, replace `:23-35`:

```go
// PotTokenProvider generates PO tokens for Innertube player requests.
// Defined here to avoid an import cycle with the bgutils package;
// *bgutils.PotProvider satisfies this interface.
//
// One method, because one is what the player API uses: both fetch paths mint
// with the video ID as the content binding (yt-dlp's PoTokenContext.PLAYER ->
// (video_id, VIDEO_ID) rule) through the provider's ordinary session cache. A
// challenge-sourced variant used to be declared here; it never had a caller,
// and it was deleted rather than left as an obligation on every implementer
// (owner ruling R1, 2026-09-15). The sidecar protocol that would carry a
// challenge is untouched.
type PotTokenProvider interface {
	GeneratePoTokenString(ctx context.Context, contentBinding string, bypassCache bool) (string, error)
}
```

- [ ] **Step 4: Drop the parameters and arguments**

In `internal/youtube/player_api_strategy.go`:

- `:541` → `func (p *PlayerAPI) fetchWithClient(ctx context.Context, videoID string, client constants.YouTubeClientConfig, ytcfg *YtcfgData, sts int) (*VideoInfo, error) {`
- `:751` → `func (p *PlayerAPI) fetchWithEmbedded(ctx context.Context, videoID string, ytcfg *YtcfgData, sts int, fetchEmbedPage bool) (*VideoInfo, error) {`
- `:73` → `info, err := p.fetchWithClient(ctx, videoID, constants.WebSafariClient, ytcfg, 0)`
- `:89` → `return p.fetchWithClient(ctx, videoID, constants.TVDowngradedClient, ytcfg, 0)`
- `:155` → `authEmb, authEmbErr := p.fetchWithEmbedded(ctx, videoID, ytcfg, sts, false)`
- `:163` → `result, err := p.fetchWithClient(ctx, videoID, constants.TVDowngradedClient, ytcfg, sts)`
- `:185` → `webResult, webErr := p.fetchWithClient(ctx, videoID, constants.WebSafariClient, ytcfg, sts)`
- `:191` → `webResult, webErr = p.fetchWithClient(ctx, videoID, constants.WebClient, ytcfg, sts)`
- `:297` → `embResult, embErr = p.fetchWithEmbedded(ctx, videoID, ytcfg, sts, true)`
- `:323` → `wcResult, wcErr := p.fetchWithClient(ctx, videoID, constants.WebCreatorClient, ytcfg, sts)`
- `:425` → `result, err := p.fetchWithClient(ctx, videoID, constants.TVDowngradedClient, wp.Ytcfg, stsPublic)`
- `:443` → `embResult, embErr := p.fetchWithEmbedded(ctx, videoID, wp.Ytcfg, stsPublic, true)`

Replace the `ProbeVideoDate` comment at `:70-72` with:

```go
	// No watch page fetched on this probe-only path, so no ytcfg beyond the
	// visitor data the caller cached.
```

- [ ] **Step 5: Delete the provider method and its tests**

In `internal/bgutils/pot_provider.go`, delete `GeneratePlayerPoToken` and its doc comment
(`:181-205`, from `// GeneratePlayerPoToken generates a PO token for the Innertube PLAYER` through the
method's closing brace). Rewrite `generatePoTokenChallenge`'s doc comment (`:206-214`) so it names no
deleted symbol:

```go
// generatePoTokenChallenge is the core behind GeneratePoToken. challenge is
// threaded through to the sidecar mint call (generateAndMint) so that IF a
// fresh minter must be built — no live session or process-wide cached minter
// satisfies this call — it can be built from a supplied BotGuard attestation
// challenge instead of the sidecar's /att/get fallback. No caller supplies one
// today: the watch-page-sourced variant was deleted by owner ruling R1
// (2026-09-15) because it never had a caller either. The parameter and the
// sidecar protocol behind it are kept deliberately, unchanged, so restoring
// that path is a one-line call rather than a re-derivation.
```

In `internal/bgutils/pot_provider_test.go`, delete `TestGeneratePlayerPoToken_BindsToVideoID`,
`TestGeneratePlayerPoToken_EmptyVideoIDErrors` and
`TestGeneratePlayerPoToken_ChallengeOnlyMattersOnFreshMint` (`:1142-1221`, section banner included).
Their subject is gone; the content binding they pinned is still pinned by
`TestPotProvider_GeneratePoTokenString` (`:997`), and the sidecar's own wire contract stays pinned by
`internal/bgutils/sidecar/sidecar_player_pot_test.go`.

- [ ] **Step 6: Update the spec prose**

In `docs/spec/platform-services.md:863`, replace the final sentence
("The challenge-sourced variant (`GeneratePlayerPoToken`, watch-page ytAtN attestation) exists but is
dormant: it exceeds what yt-dlp does, and stays parked unless premieres 403 despite the yt-dlp-parity
bindings.") with:

```markdown
A challenge-sourced variant — the watch page's own ytAtN attestation, threaded into a fresh mint — is
NOT wired: it exceeded what yt-dlp does and had no caller, so the Go-side entry point was deleted
(owner ruling R1, 2026-09-15). The sidecar protocol that would carry it is untouched, and
`generatePoTokenChallenge` still takes the challenge through to `generateAndMint`, so restoring the
path is one call. `extractAttestationChallenge` keeps running and keeps logging its reason string,
which is the diagnostic premiere 403s would be read from.
```

- [ ] **Step 7: Run the gates**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./...
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./...
staticcheck ./...
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/youtube/... ./internal/bgutils/... ./internal/docs/
```

Expected: PASS, staticcheck clean (in particular no U1000 for anything the deletion orphaned).

- [ ] **Step 8: Commit**

```bash
git add internal/youtube/player_api.go internal/youtube/player_api_strategy.go internal/youtube/player_api_retry_test.go internal/bgutils/pot_provider.go internal/bgutils/pot_provider_test.go docs/spec/platform-services.md
git commit -m "$(cat <<'EOF'
refactor(youtube): delete the dead attestation-challenge plumbing

fetchWithClient and fetchWithEmbedded took a `challenge` neither ever read —
both mint through GeneratePoTokenString bound to the video ID — and
PotTokenProvider.GeneratePlayerPoToken had no caller in the tree, its bgutils
implementation being reached only from its own three tests. Owner ruling R1
(2026-09-15) is to delete rather than restore the one-line call.

The sidecar protocol is untouched, and generatePoTokenChallenge still carries
a challenge down to generateAndMint, so wiring the path back is one call. The
watch-page extractor and its reason string stay — they are the diagnostic
premiere 403s would be read from.

Ledger item T4-31 (challenge half).

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
EOF
)"
```

---

### Task 6: Re-vendor ejs to upstream `6f8587b`

`bgutil-sidecar/vendor/ejs/VERSION` pins `2231f1fd6e13aa88ee9b3af7a3193a81e07e6a27`; upstream HEAD is
`6f8587bb7009a1fc81038538071d2cb66b8b8ed0`, eight commits of CI/workflow and formatter churn
(`d60b824` replaced prettier+eslint with oxfmt+oxlint). Verified 2026-09-15 by the planner: of the
seven vendored files, `setup.ts`, `types.ts` and `LICENSE` are byte-identical modulo CRLF, and
`solvers.ts`, `nsig.ts`, `main.ts` and `utils.ts` differ **only** in import order and blank lines.
No solver logic changes. This is the pin bump the ledger's Upstream table marks optional.

**Files:**
- Modify: `bgutil-sidecar/vendor/ejs/src/yt/solver/{solvers,nsig,setup,main}.ts`,
  `bgutil-sidecar/vendor/ejs/src/{types,utils}.ts`, `bgutil-sidecar/vendor/ejs/LICENSE`,
  `bgutil-sidecar/vendor/ejs/VERSION`
- Rebuilt, gitignored: `bgutil-sidecar/vendor/ejs.bundle.js`, `bgutil-sidecar/dist/sidecar.tar.gz`,
  `internal/bgutils/embed/sidecar.tar.gz`

**Interfaces:** none — no Go signature changes. The recipe is
`.claude/skills/moombox-upstream-porting/SKILL.md` § "Updating vendored ejs".

- [ ] **Step 1: Prepare the worktree's sidecar deps**

```bash
cd D:/Git/Moombox/.worktrees/sweep-3-youtube/bgutil-sidecar
npm ci --no-audit --no-fund
```

A FULL install, not `--omit=dev`: esbuild, meriyah and astring are devDependencies, and when esbuild is
missing `build.mjs` self-heals by running `npm install`, which can rewrite the tracked
`package-lock.json`. Installing everything up front keeps the self-heal from firing.

- [ ] **Step 2: Confirm the upstream SHA and the diff shape**

```bash
git -C D:/Git/Moombox/references/ejs rev-parse HEAD
git -C D:/Git/Moombox/references/ejs log --oneline 2231f1fd6e13aa88ee9b3af7a3193a81e07e6a27..HEAD
```

Expected: `6f8587bb7009a1fc81038538071d2cb66b8b8ed0` and eight commits, none of which touches solver
logic. If HEAD has moved past `6f8587b`, STOP and report to the controller — this arc bumps to the SHA
the ledger reviewed, not to a moving target.

- [ ] **Step 3: Copy the vendored files**

From the worktree root (`D:/Git/Moombox/.worktrees/sweep-3-youtube`):

```bash
cp D:/Git/Moombox/references/ejs/src/yt/solver/solvers.ts bgutil-sidecar/vendor/ejs/src/yt/solver/
cp D:/Git/Moombox/references/ejs/src/yt/solver/nsig.ts    bgutil-sidecar/vendor/ejs/src/yt/solver/
cp D:/Git/Moombox/references/ejs/src/yt/solver/setup.ts   bgutil-sidecar/vendor/ejs/src/yt/solver/
cp D:/Git/Moombox/references/ejs/src/yt/solver/main.ts    bgutil-sidecar/vendor/ejs/src/yt/solver/
cp D:/Git/Moombox/references/ejs/src/types.ts             bgutil-sidecar/vendor/ejs/src/
cp D:/Git/Moombox/references/ejs/src/utils.ts             bgutil-sidecar/vendor/ejs/src/
cp D:/Git/Moombox/references/ejs/LICENSE                  bgutil-sidecar/vendor/ejs/
```

(`.gitattributes` pins `*.ts text eol=lf`, and the upstream files are LF, so this also normalises the
CRLF the working tree currently carries.)

- [ ] **Step 4: Write the SHA pin**

```bash
(cd D:/Git/Moombox/references/ejs && git rev-parse HEAD) > bgutil-sidecar/vendor/ejs/VERSION
cat bgutil-sidecar/vendor/ejs/VERSION
```

Expected: `6f8587bb7009a1fc81038538071d2cb66b8b8ed0`.

- [ ] **Step 5: Check the meriyah/astring lockstep**

```bash
diff <(cat D:/Git/Moombox/references/ejs/package.json) <(cat bgutil-sidecar/package.json)
```

Expected at `6f8587b` (verified 2026-09-15): upstream `dependencies` are `astring 1.9.0` and
`meriyah 6.1.4`; `bgutil-sidecar/package.json` pins the same two exact versions in `devDependencies`
(they are devDeps here because esbuild inlines them into `vendor/ejs.bundle.js` at build time, so the
shipped tarball needs neither). **No change.** If either version differs, edit
`bgutil-sidecar/package.json` to match exactly and run `npm install` in `bgutil-sidecar` before Step 6.

- [ ] **Step 6: Rebuild the sidecar payload**

```bash
cd D:/Git/Moombox/.worktrees/sweep-3-youtube/bgutil-sidecar && node build.mjs
```

Expected: an `ejs bundle    NNN.N KB  vendor/ejs.bundle.js` line, then the tar and the copy into
`internal/bgutils/embed/sidecar.tar.gz`.

- [ ] **Step 7: Confirm the build mutated nothing tracked but the vendored tree**

```bash
cd D:/Git/Moombox/.worktrees/sweep-3-youtube && git status --porcelain
```

Expected: only `bgutil-sidecar/vendor/ejs/**` entries. `build.mjs` runs `npm prune --omit=dev` and then
`npm install` to restore devDeps; if `bgutil-sidecar/package-lock.json` shows up as modified, STOP and
report to the controller rather than reverting it (implementers use git only for `add`/`commit`).

- [ ] **Step 8: Run the cipher gates**

```bash
cd D:/Git/Moombox/.worktrees/sweep-3-youtube
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/cipher/...
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp MOOMBOX_LIVE_CIPHER_TEST=1 go test -count=1 -timeout 180s -run TestSidecarSolver ./internal/cipher/...
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp MOOMBOX_LIVE_BG_TEST=1 go test -count=1 -timeout 180s -run 'TestSidecarLivePoToken|TestSidecarLiveGvsMint' ./internal/bgutils/sidecar/...
```

Expected: PASS. `TestSidecarSolverLive` and `TestSidecarSolverGojaParity` are the ones that matter —
the parity test catches any output divergence between the rebuilt ejs bundle and the goja fallback on
every fixture goja can handle statically. A goja-side BotGuard live failure is NOT a regression signal
here (memory note `reference_goja_botguard_live_test_fails.md`); the cipher and sidecar gates are.

No `internal/cipher/` source change is expected: the eight upstream commits are formatter and CI churn,
so the goja fallback in `internal/cipher/extractor*.go` has nothing to track.

- [ ] **Step 9: Commit**

```bash
git add bgutil-sidecar/vendor/ejs
git commit -m "$(cat <<'EOF'
chore(sidecar): re-pin vendored ejs to 6f8587b

Eight upstream commits since 2231f1f, all CI/workflow and formatter churn
(d60b824 swapped prettier+eslint for oxfmt+oxlint). The four files that differ
— solvers.ts, nsig.ts, main.ts, utils.ts — differ only in import order and
blank lines; setup.ts, types.ts and LICENSE are byte-identical modulo CRLF. No
solver logic changed, so internal/cipher's goja fallback needs no parallel
port.

Gates: go test ./internal/cipher/..., the live cipher solver and goja-parity
tests, and the live sidecar PO-token mints.

Ledger: the Upstream table's optional ejs pin bump.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
EOF
)"
```

- [ ] **Step 10: Post-merge note for the controller (not an implementer step)**

`internal/bgutils/embed/sidecar.tar.gz` is gitignored (`internal/bgutils/embed/.gitignore`), so the
rebuilt payload does NOT travel with the merge. After merging `sweep-3-youtube` into `main`, the
controller must:

```bash
cp D:/Git/Moombox/.worktrees/sweep-3-youtube/internal/bgutils/embed/sidecar.tar.gz \
   D:/Git/Moombox/internal/bgutils/embed/sidecar.tar.gz
cd D:/Git/Moombox && GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build -o moombox.exe ./cmd/moombox
```

before deleting the worktree — otherwise `main` keeps building the OLD ejs bundle into the binary while
its `VERSION` file claims the new pin. (CI rebuilds it from source either way; this is for local
builds.)

---

### Task 7: Cipher-cache doc drift and plan deletion (T4-34)

`solverCacheSize = 10` (`internal/cipher/solver.go:19`, raised from 3 because multi-channel monitoring
routinely holds 4+ active player URLs). Twelve places across five documents still claim 3, which
understates the worst-case VM residency by a factor of three (~500 MB, per the constant's own comment,
not ~150 MB). Spec §6.7 names the skill doc and `platform-services.md`; the planner found four more
files and has widened the task to all of them.

**Files:**
- Modify: `.claude/skills/moombox-upstream-porting/SKILL.md:55`, `:107` (the "Cipher capped at 3 cached VMs" bullet under Porting Considerations)
- Modify: `docs/spec/platform-services.md:15`, `:981`, `:1180`
- Modify: `docs/spec/architecture.md:108`, `:194`, `:602-606`
- Modify: `docs/spec/design-philosophy.md:60`, `:144`, `:155`, `:238`
- Modify: `docs/spec/vision-and-purpose.md:88`
- Delete: `docs/superpowers/plans/2026-09-15-sweep-3-youtube.md`

**Interfaces:** none. Prose only.

- [ ] **Step 1: Write the failing test**

Append to `internal/cipher/solver_test.go`:

```go
// TestSolverCacheSizeMatchesTheDocs is the anti-drift pin for ledger item
// T4-34. The cap was raised from 3 to 10 in 2026-04; five documents and the
// upstream-porting skill went on saying 3, which understates the worst-case
// VM residency by 3x (~500 MB against the ~150 MB a reader would price).
//
// Mutant named: changing solverCacheSize without touching the docs fails
// here with both numbers on screen, which is the only moment anyone is
// holding the context needed to fix the prose.
func TestSolverCacheSizeMatchesTheDocs(t *testing.T) {
	const documented = 10
	if solverCacheSize != documented {
		t.Fatalf("solverCacheSize = %d, but docs/spec/{architecture,platform-services,design-philosophy,vision-and-purpose}.md "+
			"and .claude/skills/moombox-upstream-porting/SKILL.md document %d — update the prose in the same commit",
			solverCacheSize, documented)
	}
}
```

- [ ] **Step 2: Run to verify it passes and to prove it bites**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestSolverCacheSizeMatchesTheDocs ./internal/cipher/`

Expected: PASS. This test is a drift TRAP, not a red-first test — the code is already correct and the
documents are wrong. Prove it bites by temporarily editing `solverCacheSize` to 3, re-running (expected
FAIL: `solverCacheSize = 3, but docs/spec/… document 10`), then restoring 10.

- [ ] **Step 3: Fix the skill doc**

In `.claude/skills/moombox-upstream-porting/SKILL.md`, line 55 (the ejs → Moombox mapping table row):

```markdown
| N-challenge | `bgutil-sidecar/vendor/ejs/` (bundled via esbuild) | `cipher/solver.go` | Compiles full player.js in Goja VM, 10-slot LRU cache (`solverCacheSize`) |
```

And the Porting Considerations bullet:

```markdown
- **VM memory**: BotGuard and cipher VMs hold multi-MB JavaScript runtimes. Auto-evict when idle. Cipher caps the LRU at 10 VMs (`solverCacheSize`, `internal/cipher/solver.go`) — ~30-50 MB each, so ~500 MB worst case. It was 3 until 2026-04; multi-channel monitoring routinely holds 4+ active player URLs and the smaller cap caused constant re-compiles.
```

- [ ] **Step 4: Fix `platform-services.md`**

- `:15` → `- **Cipher has a 10-VM LRU with AST + regex fallback.** Memory cache holds at most 10 compiled solver VMs keyed by SHA256 of the player URL (`solverCacheSize`). Disk cache (`~/.cache/yt-cipher/player_cache/`) has a 14-day TTL. Compilation is mutex-serialized to prevent thundering herd. The Goja VM inside each Solvers struct is mutex-protected because Goja is not thread-safe.`
- `:981` → `- **Max size**: 10 entries (`solverCacheSize`).`
- `:1180` (cache table row) → `| Cipher (Memory) | Memory LRU | Unbounded (no expiry) | 10 solvers | Oldest by insertion order | SHA256(playerURL) |`

- [ ] **Step 5: Fix `architecture.md`**

- `:108` → `…Cache directory is `%TEMP%/yt-cipher`. Manages a 10-VM LRU cache keyed by `player.js` URL…`
- `:194` → `internal/cipher     (9 files, ~1,500)  -- YouTube signature cipher: AST + regex, 10-VM LRU`
- `:602-606` → heading `### Cipher 10-VM LRU` and the first bullet
  `- Maximum 10 VMs cached simultaneously (`solverCacheSize`; ~30-50 MB each, so ~500 MB worst case)`

- [ ] **Step 6: Fix `design-philosophy.md` and `vision-and-purpose.md`**

Replace every "3-VM LRU" / "at most three player.js runtimes" / "a fourth unique player.js" phrase with
the 10-VM reading:

- `design-philosophy.md:60` → `…Cipher VMs use a 10-VM LRU cache, so at most ten player.js runtimes exist simultaneously…`
- `design-philosophy.md:144` → `…and a 10-VM LRU memory cache with mutex-serialized compilation to prevent thundering herd.`
- `design-philosophy.md:155` → `…manages a 10-VM LRU cache with mutex serialization…`
- `design-philosophy.md:238` → `- **Cipher** uses a 10-VM Goja LRU cache keyed by player.js URL. When an eleventh unique player.js is encountered, the least-recently-used VM is evicted…`
- `vision-and-purpose.md:88` → `…maintaining a 10-VM LRU cache keyed by player.js URL.`

- [ ] **Step 7: Run the doc gates**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/docs/ ./internal/cipher/
grep -rn "3-VM LRU\|3-slot\|3 cached VMs\|at most 3 compiled\|3 solvers\|three player.js" docs/ .claude/ SPEC.md
```

Expected: both test packages PASS and the grep prints nothing.

- [ ] **Step 8: Delete this plan and commit**

Per spec §2, the plan is deleted in the arc's last commit (git history is the archive).

```bash
rm docs/superpowers/plans/2026-09-15-sweep-3-youtube.md
git add .claude/skills/moombox-upstream-porting/SKILL.md docs/spec/platform-services.md docs/spec/architecture.md docs/spec/design-philosophy.md docs/spec/vision-and-purpose.md internal/cipher/solver_test.go
git add -u docs/superpowers/plans/2026-09-15-sweep-3-youtube.md
git commit -m "$(cat <<'EOF'
docs: the cipher LRU holds 10 VMs, not 3

solverCacheSize was raised from 3 to 10 in 2026-04 — multi-channel monitoring
routinely holds 4+ active player URLs and the smaller cap caused constant
re-compiles — but twelve places across five documents and the upstream-porting
skill still said 3, understating worst-case VM residency threefold (~500 MB,
not ~150 MB). A new TestSolverCacheSizeMatchesTheDocs fails the next time the
constant moves without the prose.

Also deletes the Arc 3 plan, implemented.

Ledger item T4-34 (skill doc LRU half).

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
EOF
)"
```

---

## Arc close

1. Run the full gate list from Global Constraints, including all four live gates.
2. Dispatch the Fable arc-completion review over the whole branch diff (standing feedback rule).
3. Merge `--no-ff` into `main` without asking (standing ruling 2026-09-04); delete the worktree AND the
   branch.
4. Copy the rebuilt `sidecar.tar.gz` to `main` and rebuild `moombox.exe` (Task 6 Step 10).
5. Report to the controller for the chain ledger:
   - the pre-existing `TestRefreshWarnsWhenTheExpiredTwitchLoginIsPruned` time bomb (fix on `main`);
   - the wider dead surface R1 deliberately left standing — `PotProvider.GenerateGvsPoToken` has no
     caller outside `internal/bgutils` either, and `VideoInfo.AttestationChallenge` is written at
     `player_api_strategy.go:33` and read nowhere;
   - `internal/chat/api.go:281`'s `ExtractChatContinuation` is the untouched twin of the function Task 2
     rewrote, and carries the same whole-document `map[string]any` cost (Arc 7's file set);
   - spec §12's memory-note update: the cipher-cache figure is 10 slots.

## Self-review

**1. Spec coverage** (§6's seven design items plus §1's R1):

| Spec §6 item | Ledger | Task |
|---|---|---|
| 1. Player-response parse — anchor + `scanBalancedObject` | T1-5 | Task 1 |
| 2. Watch-page allocations — `[]byte` + typed RawMessage envelope + benchmark + AllocsPerRun | T2-16 | Task 2 |
| 3. Cookie reload — `(size, mtime)` short-circuit under the jar lock, callers untouched | T2-23 | Task 3 |
| 4. Retry budget — context-deadline-aware backoff; no concurrent cascade | T3-27 | Task 4 |
| 5. Dead challenge path — params, args, interface method, implementation | T4-31 / R1 | Task 5 |
| 6. ejs re-vendor to `6f8587b` — cp list, VERSION, lockstep, `node build.mjs`, gates, post-merge copy | Upstream row | Task 6 |
| 7. Doc drift — skill doc + `platform-services.md` (+ four more files the planner found) | T4-34 | Task 7 |
| §2 plan deletion in the arc's last commit | — | Task 7 Step 8 |

No gaps. Three places where the plan departs from the spec's letter, each justified in-line and
reported to the controller: (a) §6.1's "the existing fixtures still parse byte-identically" — no
watch-page fixture corpus exists, so Task 1 builds the before-image table and compares decoded objects;
(b) §6.2's "behaviour pinned by the existing continuation tests" — none existed, so Task 2 writes the
shapes table first; (c) §6.7 named two documents, the drift is in six.

**2. Placeholder scan:** no "TBD", "implement later", "add error handling", "similar to Task N" or
bare "write tests" anywhere. Every code step carries the actual code; every test step carries the
actual test with its mutant named; every run step carries the command and the expected output. The
three prose-only steps (Task 1 Step 6, Task 5 Step 6, Task 7 Steps 3-6) carry the replacement text
verbatim. Task 6's shell steps are the skill recipe's exact commands with absolute `references/` paths
substituted, because `references/` does not exist inside a worktree.

**3. Type consistency:** `extractPlayerResponse` is introduced in Task 1 as
`(html string) (map[string]any, bool)` and Task 2's Interfaces block states the `(page []byte)` change
explicitly, with Task 2 Step 8 fixing Task 1's test call site — the one cross-task signature move, and
it is named in both directions. `scanBalancedObject` is `(s []byte) ([]byte, bool)` from Task 2 onward,
used only by `extractPlayerResponse` and `extractAttestationChallenge`, both converted in the same
task. `sessionAuthMarkerIn` is the post-rename name and is used consistently in `watchPageSessionAuth`
and `livenessVerdict`. `chatContinuationData` is declared once (Task 2) and referenced only there.
`playerRetryBackoffBase` is declared in Task 4 and used only by Task 4's `scaleRetryBackoff`.
`cookieJarStatSettle`, `loadedSize`, `loadedMod` and `loadedMemo` are declared and used only in Task 3.
`fetchWithClient`/`fetchWithEmbedded`'s post-Task-5 signatures match every one of the twelve call sites
listed. `minimalPotProvider` implements exactly the one method Task 5's narrowed `PotTokenProvider`
declares.
