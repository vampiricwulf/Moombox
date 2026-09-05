# Arc H — TUI Structured Filter Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The TUI's `/` box understands the Web dashboard's filter language — `status:` / `channel:` / `platform:` tokens with `-` negation, `a|b` OR groups, quoted values and free text — with one token list driving both the typed query and the `F` status cycle.

**Architecture:** A neutral package `internal/jobfilter` is a line-for-line Go port of `web/public/modules/filter-parser.js` and `filter-engine.js` (`Parse`, `Match`, `Serialize`), tested against the same 32 input→expected pairs the node suites pin. `TaskListModel` replaces its two filter states (`filter Filter` enum + `searchQuery string`) with `tokens []jobfilter.Token`: `F` cycles the single `status:` token (none → active → issues → finished → none) and `/` edits the whole query; both existing gate sites (`rebuildVirtualList`, `archiveBucketsDirty`) call one `passes(job)` = `jobfilter.Match`. Text terms match title, channel and video ID by case-insensitive substring (the Web's semantics plus the video-ID field the TUI already searched). The `/` box keeps its exact lifecycle (Enter applies and closes, Esc clears then closes, focus loss closes keeping the query).

**Tech Stack:** Go 1.27.1, Bubble Tea v2 (`bubbles/textinput`), `internal/database` (`Job`).

**Spec:** `docs/superpowers/specs/2026-09-04-improvement-chain-design.md` §9 (Arc H — "own brainstorm"); survey + rulings H1–H8 in `.superpowers/sdd/2026-09-05-improvement-h-tui-filter/design-notes.md`. Anchors were read at main `5fa0ab5`; `internal/tui/task_list.go` is untouched since, `app_keys.go` grew (Arcs F/G) — anchor by text.

## Global Constraints

- Worktree `D:/Git/Moombox/.worktrees/improvement-h-tui-filter`, branch `improvement-h-tui-filter`, cut from `main` after this plan's commit (after Arc G merges). Never push. Never `cd` to the main checkout. Git is read-only beyond `git add`/`git commit`; never edit a file you are not tasked with.
- Every `go` command carries `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp`. Implementers run only the packages their task names; the controller runs the one full `go test -count=1 ./...`.
- `gofmt -l ./cmd ./internal ./tools ./web` prints nothing; `go vet ./...` silent; `staticcheck ./...` (2026.2.1) prints nothing (hard CI gate). LF only. Helpers are defined in the task that first uses them.
- **Semantics are the Web's, value for value:** `internal/jobfilter`'s tests mirror `web/tests/filter-parser.test.mjs` (15) and `web/tests/filter-engine.test.mjs` (17) — same inputs, same expected outputs — with one deliberate TUI superset: a text term also matches `Job.VideoID` (ruling H2). `web/public/*` is NOT modified by this arc.
- **One filter state** in `TaskListModel`: `tokens []jobfilter.Token` (+ the raw `queryText string` the box shows). The `Filter` enum survives only as the derived position of the status token for the `F` cycle. No `searchQuery` field remains.
- **The `/` lifecycle is preserved**: `StartSearch`, live re-filter on typing, Enter applies and closes, Esc clears (if a query is applied) then closes, losing panel focus closes the box but keeps the applied query, `ClearSearch()` from the App's Esc path. The five tests in `internal/tui/task_search_test.go` stay green except where H2 (substring, not fuzzy) changes an input — each such change is listed in the report.
- `internal/tui` must not import `internal/web/routes` (`go list -deps ./internal/tui/ | grep -c 'internal/web/routes\|internal/bgutils'` → 0); `internal/jobfilter` imports only `internal/database` + stdlib.
- Docs cited by `internal/docs/citations_test.go` must resolve. Both trailers on every commit:
  ```
  Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01Jw2Ne9cGsn3R9bJBNuXudp
  ```
  Write the message to a file and `git commit -F <file>` (never `-F -`).

---

### Task 1: `internal/jobfilter` — the language, ported and pinned

**Files:**
- Create: `internal/jobfilter/jobfilter.go`, `internal/jobfilter/jobfilter_test.go`

**Interfaces:**
```go
package jobfilter

// Kind is a token's namespace.
type Kind string
const (
	KindText     Kind = "text"
	KindStatus   Kind = "status"
	KindChannel  Kind = "channel"
	KindPlatform Kind = "platform"
	KindOr       Kind = "or"
)

// Token is one parsed unit: a term (Kind text/status/channel/platform with
// Value and Negate) or an OR group (Kind "or" with Terms). Value is kept as
// typed (for Serialize); lower is its lower-cased form, computed once.
type Token struct {
	Kind   Kind
	Value  string
	Negate bool
	Terms  []Token // Kind == KindOr only
	lower  string
}

// Parse tokenises a query the way web/public/modules/filter-parser.js does.
func Parse(query string) []Token
// Serialize renders tokens back to query text (filter-parser.js serializeToken).
func Serialize(tokens []Token) string
// Match reports whether job passes every token (AND across tokens, OR within
// a group, negation per term) — web/public/modules/filter-engine.js, plus
// the TUI's VideoID field in the text match.
func Match(tokens []Token, job *database.Job) bool
// StatusBucket names the bucket a status: value selects ("active", "issues",
// "finished") or "" for a raw status name; "errors" is an alias of "issues".
func StatusBucket(value string) string
// BucketStatuses is the Web's STATUS_FILTER_MAP.
var BucketStatuses = map[string][]database.JobStatus{...}
```

- [ ] **Step 1: Write the failing tests** — `internal/jobfilter/jobfilter_test.go`. Read `web/tests/filter-parser.test.mjs` and `web/tests/filter-engine.test.mjs` in full FIRST and transcribe every test into Go, keeping the node test's name in a comment and its exact input and expected output. The engine tests use the same five-row `jobs` fixture (ids "1"–"5": Hololive concert / Shachi Too / Live / youtube; Random clip / Shachi Too / Finished; Stream VOD / Another Ch / Error / twitch; Upcoming debut / Debut Channel / Upcoming; Muxing now / Debut Channel / Muxing) and the four-row `bucketJobs` fixture (e/c/k/f: Error/Cancelled/COOKIES?/Finished). Build them as `[]*database.Job` with `ID`, `Title`, `ChannelName`, `Status`, `Platform`. Helper `filterWith(query) []string` returns the ids of jobs that pass, in fixture order. Then add the TUI-only cases:

```go
// TUI superset (ruling H2): a text term also matches the video ID by
// case-insensitive substring; fuzzy subsequence matching is gone.
func TestTextMatchesVideoIDBySubstring(t *testing.T) {
	j := &database.Job{ID: "j", Title: "Something", ChannelName: "Chan", VideoID: "dQw4w9WgXcQ", Status: database.StatusFinished, Platform: "youtube"}
	if !Match(Parse("w9wg"), j) {
		t.Fatal("substring of the video ID must match")
	}
	if Match(Parse("dQwWgX"), j) { // subsequence, not substring
		t.Fatal("fuzzy subsequence must NOT match — semantics are the Web's substring match")
	}
}

func TestStatusBucket(t *testing.T) {
	for in, want := range map[string]string{"active": "active", "ISSUES": "issues", "errors": "issues", "finished": "finished", "live": "", "": ""} {
		if got := StatusBucket(in); got != want {
			t.Errorf("StatusBucket(%q) = %q, want %q", in, got, want)
		}
	}
}

func BenchmarkMatch1000(b *testing.B) {
	jobs := make([]*database.Job, 1000)
	for i := range jobs {
		jobs[i] = &database.Job{ID: fmt.Sprint(i), Title: fmt.Sprintf("Stream %d karaoke night", i), ChannelName: "Channel " + fmt.Sprint(i%20), VideoID: fmt.Sprintf("vid%08d", i), Status: database.StatusFinished, Platform: "youtube"}
	}
	tokens := Parse(`karaoke status:finished -platform:twitch`)
	b.ResetTimer()
	for b.Loop() {
		for _, j := range jobs {
			Match(tokens, j)
		}
	}
}
```
(`b.Loop()` is Go 1.24+; the repo is on 1.27.)

- [ ] **Step 2: Run** — `go test -count=1 ./internal/jobfilter/` → FAIL to compile.

- [ ] **Step 3: Implement `jobfilter.go`** as a faithful port. Structure to mirror:
  - `tokenize(query) []string`: walk runes; whitespace splits unless inside quotes; a quote (`"` or `'`) OPENS quoting only when at token start, right after a leading `-`, or right after a namespace colon (`key:`); the matching closing quote ends quoting; other quote characters are literal (an apostrophe in `mori's` does not quote). Mirror `filter-parser.js:59-85` branch for branch.
  - `parseTerm(raw) Token`: strip one leading `-` → `Negate`; split at the first `:`; if the lowercased prefix is a namespace, `Kind` = that namespace and `Value` = `stripQuotePair(rest)`; else `Kind = KindText`, `Value = stripQuotePair(raw-without-minus)`. `lower = strings.ToLower(Value)`.
  - `Parse`: for each raw token, if it contains `|` and no quote characters → `Token{Kind: KindOr, Terms: parseTerm each part}` (empty parts skipped); else `parseTerm`. Empty/whitespace query → `nil` (tests expect zero tokens; compare lengths, not nil-ness, in tests).
  - `Serialize`: per token, the JS `serializeToken` rules (negation prefix, `key:` prefix for namespaces, re-quote values containing spaces with `"…"`, OR groups joined with `|`), tokens joined by single spaces.
  - `matchTerm(t, job)`: text → `strings.Contains(strings.ToLower(job.Title), t.lower) || strings.Contains(strings.ToLower(job.ChannelName), t.lower) || strings.Contains(strings.ToLower(job.VideoID), t.lower)`; status → if `BucketStatuses[StatusBucket(t.lower)]` exists, `slices.Contains(bucket, job.Status)`, else `strings.EqualFold(string(job.Status), t.Value)`; channel → `strings.EqualFold(job.ChannelName, t.Value)`; platform → `strings.EqualFold(job.Platform, t.Value)`; apply `Negate`.
  - `Match`: `for each token: if Kind == KindOr { if !slices.ContainsFunc(Terms, matchTerm) return false } else if !matchTerm → false`; return true. Precompute nothing per job beyond the ToLower calls.
  - `BucketStatuses`: `active: [Downloading, Live, Upcoming, Muxing, Queued]`, `issues: [Error, Cancelled, COOKIES?]`, `finished: [Finished]` using the `database.Status*` constants; `StatusBucket` maps `errors` → `issues`.
  Package doc: "Package jobfilter is the Go twin of web/public/modules/filter-parser.js and filter-engine.js — the dashboard's filter language, used by the TUI's / box. Changes to the language land in both places; the tests here mirror the node suites value for value."

- [ ] **Step 4: Run** — all tests PASS; `go test -count=1 -run xxx -bench Match1000 -benchmem ./internal/jobfilter/` — record ns/op and allocs/op in the report. `go list -deps ./internal/jobfilter/ | grep -c 'internal/web\|internal/tui'` → 0. gofmt/vet/staticcheck silent.

- [ ] **Step 5: Commit**
```
feat(jobfilter): the dashboard's filter language, ported to Go and pinned

Parse/Match/Serialize mirror filter-parser.js and filter-engine.js; the
tests are the node suites' 32 cases value for value, plus the TUI's
video-ID substring match.
```

---

### Task 2: `TaskListModel` runs on one token list; `F` cycles the status token

**Files:**
- Modify: `internal/tui/task_list.go` — fields `:136` area (`filter`, `searchQuery`, `searchInput`), `Filter` enum `:37-63`, `StartSearch` `:175-182`, `HandleSearchKey` `:192-218`, `refilterSelectTop` `:224-228`, `UpdateSearchInput` `:234-246`, `ClearSearch` `:250-262`, `SetFocused` `:495-502`, `CycleFilter` `:601-608`, `archiveBucketsDirty` `:676-692`, `rebuildVirtualList` `:707-801` (gate `:717-722`, archive rule `:785`), `passesSearch` `:803-816`, `passesFilter` `:818-829`, empty-state `:840-841`, `View` `:852-856`, `renderHeader` `:866-889`
- Modify: `internal/tui/task_search_test.go` (revise per H2; add token tests)
- Create: `internal/tui/task_filter_tokens_test.go`
- `internal/tui/app_keys.go`: no change expected (`handleFilter` → `CycleFilter`; `/` → `StartSearch`; Esc → `ClearSearch`) — confirm by reading.

**Interfaces:**
- Consumes `jobfilter.Parse/Match/Serialize/StatusBucket`.
- Produces on `TaskListModel`: `tokens []jobfilter.Token`, `queryText string`; `Query() string` (serialized, for the header/tests); `CycleFilter()` operating on the status token; `filterPosition() Filter` (derived); `passes(j *database.Job) bool`.

- [ ] **Step 1: Write the failing tests** — `internal/tui/task_filter_tokens_test.go`:

```go
package tui

import (
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

func tokenFixtureJobs() []*database.Job {
	return []*database.Job{
		{ID: "1", Title: "Hololive concert", ChannelName: "Shachi Too", Status: database.StatusLive, Platform: "youtube", VideoID: "aaaaaaaaaaa"},
		{ID: "2", Title: "Random clip", ChannelName: "Shachi Too", Status: database.StatusFinished, Platform: "youtube", VideoID: "bbbbbbbbbbb"},
		{ID: "3", Title: "Stream VOD", ChannelName: "Another Ch", Status: database.StatusError, Platform: "twitch", VideoID: "tw_v123"},
		{ID: "4", Title: "Upcoming debut", ChannelName: "Debut Channel", Status: database.StatusUpcoming, Platform: "youtube", VideoID: "ccccccccccc"},
	}
}

func visibleIDs(m *TaskListModel) []string {
	var ids []string
	for _, it := range m.list.Items() { // use the real accessor the model exposes for its visible rows
		if ti, ok := it.(taskItem); ok && ti.job != nil {
			ids = append(ids, ti.job.ID)
		}
	}
	return ids
}

// TestFCyclesTheStatusToken: F walks none → active → issues → finished → none,
// replacing any status token the operator typed, and the header shows the
// serialized query.
func TestFCyclesTheStatusToken(t *testing.T) {
	m := NewTaskListModel()
	m.SetSize(100, 30)
	m.SetJobs(tokenFixtureJobs())
	if got := m.Query(); got != "" {
		t.Fatalf("initial query %q", got)
	}
	m.CycleFilter()
	if m.Query() != "status:active" || !equalIDs(visibleIDs(m), []string{"1", "4"}) {
		t.Fatalf("after F: query %q ids %v", m.Query(), visibleIDs(m))
	}
	m.CycleFilter()
	if m.Query() != "status:issues" || !equalIDs(visibleIDs(m), []string{"3"}) {
		t.Fatalf("after F F: %q %v", m.Query(), visibleIDs(m))
	}
	m.CycleFilter()
	if m.Query() != "status:finished" || !equalIDs(visibleIDs(m), []string{"2"}) {
		t.Fatalf("after F F F: %q %v", m.Query(), visibleIDs(m))
	}
	m.CycleFilter()
	if m.Query() != "" || len(visibleIDs(m)) != 4 {
		t.Fatalf("fourth F must clear: %q %v", m.Query(), visibleIDs(m))
	}
	// A typed status token is the same token: F replaces it and keeps the rest.
	m.applyQuery(`-platform:twitch status:issues`)
	m.CycleFilter() // issues → finished
	if m.Query() != "-platform:twitch status:finished" {
		t.Fatalf("F must replace the typed status token in place: %q", m.Query())
	}
	if !strings.Contains(stripANSI(m.renderHeader()), "[-platform:twitch status:finished]") {
		t.Fatalf("header must show the serialized query: %q", stripANSI(m.renderHeader()))
	}
}

// TestTypedTokensFilterTheList: the / box understands the whole language.
func TestTypedTokensFilterTheList(t *testing.T) {
	m := NewTaskListModel()
	m.SetSize(100, 30)
	m.SetJobs(tokenFixtureJobs())
	for query, want := range map[string][]string{
		`shachi`:                     {"1", "2"},
		`channel:"shachi too" -clip`: {"1"},
		`platform:twitch|status:upcoming`: {"3", "4"},
		`tw_v`:                       {"3"}, // video-ID substring
		`status:live`:                {"1"}, // raw status fallback
		`nothing-matches`:            nil,
	} {
		m.applyQuery(query)
		if got := visibleIDs(m); !equalIDs(got, want) {
			t.Errorf("%q → %v, want %v", query, got, want)
		}
	}
}

// TestArchiveVisibilityFollowsTheStatusToken: the archive section is hidden
// while a status token excludes Finished; a negated token does not hide it.
func TestArchiveVisibilityFollowsTheStatusToken(t *testing.T) {
	m := NewTaskListModel()
	m.applyQuery("status:active")
	if m.showArchive() {
		t.Error("status:active must hide the archive")
	}
	m.applyQuery("status:finished")
	if !m.showArchive() {
		t.Error("status:finished must show the archive")
	}
	m.applyQuery("-status:active")
	if !m.showArchive() {
		t.Error("a negated status token must not hide the archive")
	}
	m.applyQuery("channel:x")
	if !m.showArchive() {
		t.Error("no status token → archive shown")
	}
}

func equalIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
```
Use the model's real names: the visible-rows accessor (read how `TestSearchFiltersVisibleList` in `task_search_test.go:71-98` reads the rows and copy that), `renderHeader`'s signature, `SetJobs`. `applyQuery(q string)` is a new small method (sets `queryText`, parses, rebuilds — the same thing Enter in the box does) and `showArchive() bool` extracts the rule at `:785`.

- [ ] **Step 2: Run** — FAIL to compile.

- [ ] **Step 3: Migrate `task_list.go`**
1. Fields: delete `filter Filter` and `searchQuery string`; add `tokens []jobfilter.Token` and `queryText string`. Keep `searching bool` and `searchInput`.
2. `Filter` enum stays (`FilterAll/FilterActive/FilterErrors/FilterFinished`, `String()`, `Next()`); add
   ```go
   // filterPosition derives the F-cycle position from the status token.
   func (m *TaskListModel) filterPosition() Filter {
   	for _, t := range m.tokens {
   		if t.Kind == jobfilter.KindStatus && !t.Negate {
   			switch jobfilter.StatusBucket(t.Value) {
   			case "active":
   				return FilterActive
   			case "issues":
   				return FilterErrors
   			case "finished":
   				return FilterFinished
   			}
   		}
   	}
   	return FilterAll
   }
   ```
3. `CycleFilter()`: `next := m.filterPosition().Next()`; remove every non-negated `KindStatus` token whose bucket is one of the three; if `next != FilterAll`, insert `jobfilter.Token{Kind: KindStatus, Value: <"active"|"issues"|"finished">}` at the position of the removed token (or append); then `m.queryText = jobfilter.Serialize(m.tokens)`, `m.rebuildVirtualList()`, `m.list.Select(0)`, `m.resetMarquee()` (as today). Expose a small constructor in `jobfilter` if `lower` must be set (e.g. `jobfilter.Term(kind, value, negate) Token`) — add it to Task 1's package now if needed and note it.
4. `applyQuery(q string)`: `m.queryText = strings.TrimSpace(q); m.tokens = jobfilter.Parse(m.queryText); m.rebuildVirtualList()` (+ select top as `refilterSelectTop` does). `Query() string` returns `jobfilter.Serialize(m.tokens)` (empty when no tokens).
5. The `/` box: `StartSearch` seeds `searchInput` with `m.queryText` (so re-opening shows the applied query — check what it does today and keep it); `UpdateSearchInput` → `applyQuery(m.searchInput.Value())` live; `HandleSearchKey` Enter → apply + close; Esc → if `len(m.tokens) > 0` clear (`applyQuery("")`) then close — exactly the current lifecycle; `SetFocused(false)` closes the box keeping `tokens`; `ClearSearch()` → `applyQuery("")`, returns whether anything was cleared. Placeholder: `text  status:active  channel:"name"  -platform:twitch`.
6. Gates: replace `passesFilter` + `passesSearch` with `func (m *TaskListModel) passes(j *database.Job) bool { return jobfilter.Match(m.tokens, j) }` at BOTH sites (`rebuildVirtualList` and `archiveBucketsDirty`). Delete the old two functions and the `fuzzy` import if unused (check other uses first).
7. Archive rule: `func (m *TaskListModel) showArchive() bool` — true unless some non-negated `KindStatus` token has a bucket that does not contain `database.StatusFinished` (a raw status value like `status:live` also hides it; `status:finished` and no-status show it); use it at the former `:785` alongside the other conditions that line had (`hideFinished`… — keep them).
8. Header (`renderHeader`): replace the `[Active]`-style suffix and the `[/query]` suffix with ONE yellow ` [<Query()>]` when `Query() != ""`. Empty state: `"No tasks match " + Query()`.

- [ ] **Step 4: Revise `task_search_test.go`** for H2: `TestPassesSearch` becomes a table over `m.passes` after `applyQuery`; each former fuzzy input that is NOT a substring of `Title + ChannelName + VideoID` is rewritten to a substring (list every changed case in the report with the old and new input); the video-ID case (`"aaaaaaaaaaa"`) stays; add one negative case proving subsequence-only inputs no longer match. `TestSearchFiltersVisibleList`, `TestSearchLifecycle`, `TestSearchClosesOnFocusLoss`, `TestSearchEscClearsQuery`: keep their scenarios; replace `m.searchQuery` reads with `m.Query()`/`m.queryText` and assert the same outcomes.

- [ ] **Step 5: Run** — all tui tests PASS: `go test -count=1 ./internal/tui/` (`TestHelpCoversEveryChord` unaffected). Mutant: make `showArchive()` return true unconditionally → `TestArchiveVisibilityFollowsTheStatusToken` FAILS; revert. Record.

- [ ] **Step 6: Gates** — gofmt/vet/staticcheck silent; `go list -deps ./internal/tui/ | grep -c 'internal/web/routes\|internal/bgutils'` → 0; `grep -n 'searchQuery\|passesFilter\|passesSearch\|fuzzy\.' internal/tui/*.go` → nothing outside comments (or nothing at all).

- [ ] **Step 7: Commit**
```
feat(tui): the task list filters on the dashboard's language; F cycles the status token

One token list replaces the F enum and the / query string: typed
status:/channel:/platform: tokens, negation, OR groups and free text (title,
channel, video ID by substring) all pass through jobfilter.Match at both
gate sites; F inserts or cycles the status token in place; the header shows
the serialized query; the archive hides only while a status token excludes
Finished. The / box keeps its lifecycle.
```

---

### Task 3: Docs, help and the README's missing `/`

**Files:**
- Modify: `internal/tui/help.go` (`quickKeys` `F` line; `navigationKeys` `/` line) if their text no longer matches (`F` → "Cycle status filter (Tasks) · description (Details) · log level (Logs)"; `/` → "Filter jobs — text, status:, channel:, platform:, -negate, a|b (Tasks) · find text (Logs)")
- Modify: `CLAUDE.md:109` (the `/` clause: today it says "Search logs — log panel only" → `**/** (Filter: on the Tasks panel the dashboard's filter language — text, status:/channel:/platform:, -negation, a|b; on the log panel a text search with n/N)`), `README.md` Quick Keys (`| / | Filter jobs (Tasks) / search (Logs) — status:active channel:"name" -platform:twitch |` and the `F` row reads "Cycle status filter (All/Active/Issues/Finished)" — already true), `docs/spec/user-interfaces.md`: the `F` row (`:266`) and `/` row (`:270`) rewritten to the truth; the module table rows for `filter-parser.js`/`filter-engine.js` (`:52-53`) gain "Go twin: `internal/jobfilter`"; the "Unified filter state" paragraph (`:69`) gains a sentence: `The TUI's \`/\` box speaks the same language through \`internal/jobfilter\` (\`Parse\`, \`Match\`), and its \`F\` key cycles the \`status:\` token of that query.`; Module/Overlay Feature Mapping: `| Filter language | \`filter-parser.js\` / \`filter-engine.js\` | \`internal/jobfilter\` behind the Tasks panel's \`/\` box |`; `docs/spec/architecture.md` package graph: `internal/jobfilter (1 file, ~150) -- the dashboard's filter language (Parse/Match/Serialize), the TUI's / box`.

- [ ] **Step 1: Edit.** **Step 2:** `go test -count=1 ./internal/docs/ ./internal/tui/` ok. **Step 3: Commit**
```
docs: the TUI / box speaks the dashboard's filter language

user-interfaces.md, README, CLAUDE.md and the help overlay say what F and /
do on each panel; internal/jobfilter joins the package graph.
```

---

## Self-Review

**Spec coverage (§9 H + H1–H8):** the Web language with negation and OR and quoting ✔ Task 1 (mirrored 32 cases); video ID in the text match ✔ (H2, Task 1 test + Task 2); reuse of `filter-parser.js` semantics in Go ✔ (port); interaction with `F` (one token list; F cycles the status token) ✔ Task 2; interaction with `/` (it IS the entry point; lifecycle preserved) ✔ Task 2; docs ✔ Task 3. **Placeholders:** the Task 1 table is described by reference to the node files with the fixture spelled out and the transcription rule stated; every other step has code or exact text; model-specific accessor names are flagged with where to read them. **Type consistency:** `jobfilter.Token{Kind, Value, Negate, Terms}`, `Parse/Match/Serialize/StatusBucket/BucketStatuses` used identically in Tasks 1–2; `TaskListModel.tokens/queryText/Query()/applyQuery()/showArchive()/filterPosition()/passes()` consistent across Task 2's steps and tests.
