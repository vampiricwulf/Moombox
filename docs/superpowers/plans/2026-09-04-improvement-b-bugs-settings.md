# Arc B — Bug Fixes and Settings Gaps — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The TUI channel editor can no longer lose data; both channel editors expose every per-channel field; the TUI import request encodes its headers; the Web resume gate is one rule; the probe goroutines recover; the settings API validates what it applies and both UIs expose `probe_targets`; three read-once settings become live and two sidecar knobs are honestly marked restart-required.

**Architecture:** Small, independently testable edits at existing seams. TUI channel code gains an `existing` parameter and four fields; the Web dialog gains four inputs and a pure payload helper in `modules/utils.js`; the Web resume rule moves to `modules/utils.js` as one exported function; the config route gains validation blocks and three hot-reload callbacks wired in `cmd/moombox`; `TrimService` gains a setter that rebuilds its muxer.

**Tech Stack:** Go 1.27 (`internal/tui`, `internal/web/routes`, `internal/worker`, `internal/connectivity`, `cmd/moombox`), vanilla JS ES modules (`web/public/modules/utils.js`, `settings.js`, `app.js`, `index.html`), `node --test` for pure JS, goja-driven TUI/JS parity tests.

**Spec:** `docs/superpowers/specs/2026-09-04-improvement-chain-design.md` §5 (Arc B), §2 (execution model).

## Global Constraints

- Every change is TDD: failing test first, minimal code, green, then a named mutant that must fail. Paste literal compiler/test output in reports — never paraphrase.
- Behaviour that must NOT change: existing channels edited in either UI keep every field the editor does not show; `config.Validate` semantics; the Web single-job resume rule (`status ∈ {Cancelled, Error, COOKIES?} or Finished+incompleteTail`, platform `youtube`, `hasStaging`) — the batch paths adopt it.
- Blank numeric override fields mean "use the global/default" and clear the pointer (`nil`), on both UIs.
- Restart-required parity: `RESTART_REQUIRED_FIELDS` (`web/public/modules/settings.js`) and `restartRequiredKeys` (`internal/tui/settings.go`) stay bijective; every new key also enters `categoryOf` in `internal/tui/settings_restart_parity_test.go`, and its category word appears in BOTH the TUI restart overlay sentence (`internal/tui/settings_view.go`, `renderRestartOverlay`) and the Web confirm string (`settings.js`, the `"Some settings require a restart to take effect ("` prompt).
- New hot-reload callbacks fire only when the value changed (mirror the existing `oldLogLevel`/`newLogLevel` pattern); nothing fires under the config lock.
- Worktree: `D:/Git/Moombox/.worktrees/improvement-b-bugs-settings` (branch `improvement-b-bugs-settings`). Fresh worktrees need: `internal/bgutils/embed/{node-windows-amd64.gz,node-linux-amd64.gz,node-linux-arm64.gz,sidecar.tar.gz}`, `internal/cipher/testdata/*.js`, and `cd web/tests && npm ci --no-audit --no-fund --ignore-scripts`. Every `go` command carries `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp`. ONE `go test -count=1 ./...` at a time on the machine (Task 13 only); tasks run package tests.
- Gate recipe: `gofmt -l ./cmd ./internal ./tools ./web` empty; `go vet ./...`; `go build ./...`; the touched packages' tests; `node --test web/tests/*.test.mjs` when JS changed.
- Commit trailer on every commit:
  ```
  Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01Jw2Ne9cGsn3R9bJBNuXudp
  ```
- Chain ledger: `D:/Git/Moombox/.superpowers/sdd/2026-09-04-improvement-chain/progress.md` — one dated line per task under "## Arc B".

---

### Task 1: TUI channel editor preserves the fields it does not show

**Files:**
- Modify: `internal/tui/settings_channels.go:33-57` (`valuesToChannel`), `:206-221` (`saveCurrentChannel`)
- Modify: `internal/tui/setup_wizard.go:925-930` (the wizard's save)
- Create: `internal/tui/settings_channels_test.go`

**Interfaces:**
- Produces: `func valuesToChannel(vals map[string]string, existing *config.ChannelConfig) config.ChannelConfig` — `existing == nil` for a new channel; non-nil copies every field first and overwrites only the edited ones. Task 2 extends both this function and `channelToValues`.

- [ ] **Step 1: Write the failing tests**

Create `internal/tui/settings_channels_test.go`:

```go
package tui

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

func intPtr(v int) *int { return &v }

// fullChannel is a channel carrying every field the TUI editor does NOT
// show, plus named (map) terms. Any of these vanishing after an edit is
// the data-loss bug this file exists to pin.
func fullChannel() config.ChannelConfig {
	enabled := true
	return config.ChannelConfig{
		ID:                    "UC123",
		Name:                  "Old name",
		Platform:              "youtube",
		Enabled:               &enabled,
		Terms:                 config.ChannelTerms{IsMap: true, Named: map[string]string{"stream": "(?i)karaoke", "vod": "(?i)vod"}},
		NumDescLookbehind:     intPtr(5),
		OutputDirectory:       "D:/special",
		IncludeNonLiveContent: true,
		ArchiveWindowDays:     intPtr(7),
		ArchiveSlots:          intPtr(2),
		QualityPreference:     "720p",
	}
}

// TestValuesToChannelPreservesUnshownFields: editing only the display name
// keeps the four hidden fields and the named terms verbatim.
func TestValuesToChannelPreservesUnshownFields(t *testing.T) {
	existing := fullChannel()
	vals := channelToValues(existing)
	vals["name"] = "New name"

	got := valuesToChannel(vals, &existing)

	if got.Name != "New name" {
		t.Errorf("Name = %q, want New name", got.Name)
	}
	if got.NumDescLookbehind == nil || *got.NumDescLookbehind != 5 {
		t.Errorf("NumDescLookbehind = %v, want 5", got.NumDescLookbehind)
	}
	if got.OutputDirectory != "D:/special" {
		t.Errorf("OutputDirectory = %q, want D:/special", got.OutputDirectory)
	}
	if got.ArchiveWindowDays == nil || *got.ArchiveWindowDays != 7 {
		t.Errorf("ArchiveWindowDays = %v, want 7", got.ArchiveWindowDays)
	}
	if got.ArchiveSlots == nil || *got.ArchiveSlots != 2 {
		t.Errorf("ArchiveSlots = %v, want 2", got.ArchiveSlots)
	}
	if !got.Terms.IsMap || got.Terms.Named["vod"] != "(?i)vod" {
		t.Errorf("Terms = %+v, want the named map preserved", got.Terms)
	}
	if !got.IncludeNonLiveContent || got.QualityPreference != "720p" {
		t.Errorf("shown fields drifted: include=%v quality=%q", got.IncludeNonLiveContent, got.QualityPreference)
	}
}

// TestValuesToChannelEditedTermsReplaceNamedMap: typing a new pattern
// replaces the whole terms value with the simple form (the editor shows one
// string, so that is what the operator meant).
func TestValuesToChannelEditedTermsReplaceNamedMap(t *testing.T) {
	existing := fullChannel()
	vals := channelToValues(existing)
	vals["terms"] = "(?i)new"

	got := valuesToChannel(vals, &existing)
	if got.Terms.IsMap || got.Terms.Simple != "(?i)new" {
		t.Errorf("Terms = %+v, want Simple (?i)new", got.Terms)
	}
}

// TestValuesToChannelClearedFieldsClear: clearing terms, switching the
// uploads toggle off and quality back to best must clear, not keep, the
// existing values (a copied-then-conditionally-set field would keep them).
func TestValuesToChannelClearedFieldsClear(t *testing.T) {
	existing := fullChannel()
	existing.Terms = config.ChannelTerms{Simple: "(?i)old"}
	vals := channelToValues(existing)
	vals["terms"] = ""
	vals["include_non_live"] = "No"
	vals["quality_preference"] = "best"
	vals["enabled"] = "No"

	got := valuesToChannel(vals, &existing)
	if got.Terms.Simple != "" || got.Terms.IsMap {
		t.Errorf("Terms = %+v, want empty", got.Terms)
	}
	if got.IncludeNonLiveContent {
		t.Error("IncludeNonLiveContent still true after toggling No")
	}
	if got.QualityPreference != "" {
		t.Errorf("QualityPreference = %q, want empty for best", got.QualityPreference)
	}
	if got.Enabled == nil || *got.Enabled {
		t.Errorf("Enabled = %v, want false", got.Enabled)
	}
}

// TestValuesToChannelNewChannel: nil existing behaves exactly like before —
// a fresh ChannelConfig from the form values only.
func TestValuesToChannelNewChannel(t *testing.T) {
	vals := map[string]string{
		"id": " UC999 ", "name": "N", "platform": "youtube", "enabled": "Yes",
		"terms": "(?i)x", "include_non_live": "Yes", "quality_preference": "best",
	}
	got := valuesToChannel(vals, nil)
	if got.ID != "UC999" || got.Name != "N" || got.Platform != "youtube" {
		t.Errorf("identity fields = %q %q %q", got.ID, got.Name, got.Platform)
	}
	if got.Enabled == nil || !*got.Enabled || !got.IncludeNonLiveContent || got.Terms.Simple != "(?i)x" || got.QualityPreference != "" {
		t.Errorf("form fields drifted: %+v", got)
	}
	if got.NumDescLookbehind != nil || got.OutputDirectory != "" || got.ArchiveWindowDays != nil || got.ArchiveSlots != nil {
		t.Errorf("hidden fields must be zero for a new channel: %+v", got)
	}
}

// TestSaveCurrentChannelPassesExisting: the settings overlay's save path hands
// the channel being edited to valuesToChannel (an edit) but nil for an add.
func TestSaveCurrentChannelPassesExisting(t *testing.T) {
	m := NewSettingsModel()
	m.channels = []config.ChannelConfig{fullChannel()}
	m.channelIndex = 0
	m.channelEditValues = channelToValues(m.channels[0])
	m.channelEditValues["name"] = "Renamed"
	m.saveCurrentChannel()
	if got := m.channels[0]; got.Name != "Renamed" || got.ArchiveSlots == nil || *got.ArchiveSlots != 2 {
		t.Errorf("edit lost fields: %+v", got)
	}

	m.channelIndex = len(m.channels) // add sentinel
	m.channelEditValues = map[string]string{"id": "UC2", "name": "", "platform": "youtube", "enabled": "Yes", "terms": "", "include_non_live": "No", "quality_preference": "best"}
	m.saveCurrentChannel()
	if len(m.channels) != 2 || m.channels[1].ID != "UC2" || m.channels[1].ArchiveSlots != nil {
		t.Errorf("add produced %+v", m.channels)
	}
}
```

- [ ] **Step 2: Run to confirm the compile failure**

```bash
cd D:/Git/Moombox/.worktrees/improvement-b-bugs-settings
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestValuesToChannel|TestSaveCurrentChannel' ./internal/tui/ 2>&1 | head -5
```
Expected: `too many arguments in call to valuesToChannel` (build failed).

- [ ] **Step 3: Implement**

Replace `valuesToChannel` in `internal/tui/settings_channels.go` with:

```go
// valuesToChannel turns the editor's form values into a ChannelConfig. For an
// edit, existing is the channel being edited: every field is copied from it
// first, so the fields this editor does not show (num_desc_lookbehind,
// output_directory, archive_window_days, archive_slots, named terms) survive
// the round trip — the bug this parameter exists to close. nil means a new
// channel. Shown fields are assigned unconditionally so clearing one clears
// it in the result too.
func valuesToChannel(vals map[string]string, existing *config.ChannelConfig) config.ChannelConfig {
	var ch config.ChannelConfig
	if existing != nil {
		ch = *existing
	}
	ch.ID = strings.TrimSpace(vals["id"])
	ch.Name = strings.TrimSpace(vals["name"])
	ch.Platform = vals["platform"]
	switch vals["enabled"] {
	case "No":
		boolFalse := false
		ch.Enabled = &boolFalse
	case "Yes":
		boolTrue := true
		ch.Enabled = &boolTrue
	}
	// The editor shows one pattern string. Unchanged text keeps whatever
	// shape the config had (a named map shows as its Simple, ""); changed
	// text becomes the simple form, and "" clears the terms.
	if existing == nil || vals["terms"] != channelToValues(*existing)["terms"] {
		ch.Terms = config.ChannelTerms{}
		if vals["terms"] != "" {
			ch.Terms = config.ChannelTerms{Simple: vals["terms"]}
		}
	}
	ch.IncludeNonLiveContent = vals["platform"] == "youtube" && vals["include_non_live"] == "Yes"
	ch.QualityPreference = ""
	if q := vals["quality_preference"]; q != "" && q != "best" {
		ch.QualityPreference = q
	}
	return ch
}
```

In `saveCurrentChannel` replace `ch := valuesToChannel(m.channelEditValues)` with:
```go
	var existing *config.ChannelConfig
	if m.channelIndex < len(m.channels) {
		existing = &m.channels[m.channelIndex]
	}
	ch := valuesToChannel(m.channelEditValues, existing)
```

In `internal/tui/setup_wizard.go` (the `keyEnter` case of `handleChannelEditKey`, currently `:925`) replace `ch := valuesToChannel(m.channelEditValues)` with the same five lines (the wizard has the same `m.channels`/`m.channelIndex` fields).

- [ ] **Step 4: Run the tests**

```bash
cd D:/Git/Moombox/.worktrees/improvement-b-bugs-settings
gofmt -l internal/tui
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./... && GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestValuesToChannel|TestSaveCurrentChannel' -v ./internal/tui/ 2>&1 | grep -E '^(--- |ok|FAIL)'
```
Expected: five `--- PASS`, `ok`.

- [ ] **Step 5: Mutants (apply, run, confirm FAIL, revert each)**

1. Delete the two lines `if existing != nil { ch = *existing }` (keep `var ch`) → `TestValuesToChannelPreservesUnshownFields` FAILS on NumDescLookbehind.
2. Change `ch.IncludeNonLiveContent = …` back to `if … { ch.IncludeNonLiveContent = true }` → `TestValuesToChannelClearedFieldsClear` FAILS.
3. In `saveCurrentChannel`, pass `nil` instead of `existing` → `TestSaveCurrentChannelPassesExisting` FAILS.

- [ ] **Step 6: Full TUI package tests and commit**

```bash
cd D:/Git/Moombox/.worktrees/improvement-b-bugs-settings
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/tui/ 2>&1 | tail -1
git add internal/tui/settings_channels.go internal/tui/setup_wizard.go internal/tui/settings_channels_test.go
git commit -F - <<'MSG'
fix(tui): channel editor keeps the fields it does not show

valuesToChannel copies the channel being edited before applying the form,
so num_desc_lookbehind, output_directory, archive_window_days, archive_slots
and named terms survive a TUI edit (they were dropped). Shown fields are
assigned unconditionally so clearing one clears it. The setup wizard's save
uses the same path.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Jw2Ne9cGsn3R9bJBNuXudp
MSG
```
Ledger line under "## Arc B": `- <date> Task 1: TUI channel edit preserves hidden fields (<sha>); tui package ok.`

---

### Task 2: The four per-channel fields in the TUI channel form

**Files:**
- Modify: `internal/tui/settings.go:256-264` (`channelFields`)
- Modify: `internal/tui/settings_channels.go` (`channelToValues`, `valuesToChannel`, the add-seed map at `:100-104`, a new `validateChannelValues`, its call in `handleChannelKey`'s `keyEnter`)
- Modify: `internal/tui/setup_wizard.go:861-865` (seed map) and `:912-926` (call `validateChannelValues` before saving)
- Test: `internal/tui/settings_channels_test.go`

**Interfaces:**
- Consumes: Task 1's `valuesToChannel(vals, existing)`.
- Produces: form keys `num_desc_lookbehind`, `output_directory`, `archive_window_days`, `archive_slots` (strings; blank = unset); `func validateChannelValues(vals map[string]string) string` returning an error message or "".

- [ ] **Step 1: Write the failing tests** (append to `settings_channels_test.go`)

```go
// TestChannelValuesRoundTripOverrides: the four override fields render as
// text ("" when unset) and parse back to pointers (nil when blank).
func TestChannelValuesRoundTripOverrides(t *testing.T) {
	existing := fullChannel()
	vals := channelToValues(existing)
	for k, want := range map[string]string{
		"num_desc_lookbehind": "5", "output_directory": "D:/special",
		"archive_window_days": "7", "archive_slots": "2",
	} {
		if vals[k] != want {
			t.Errorf("channelToValues[%q] = %q, want %q", k, vals[k], want)
		}
	}

	vals["num_desc_lookbehind"] = ""
	vals["output_directory"] = ""
	vals["archive_window_days"] = "14"
	vals["archive_slots"] = " 4 "
	got := valuesToChannel(vals, &existing)
	if got.NumDescLookbehind != nil {
		t.Errorf("blank lookbehind must clear, got %v", *got.NumDescLookbehind)
	}
	if got.OutputDirectory != "" {
		t.Errorf("blank output dir must clear, got %q", got.OutputDirectory)
	}
	if got.ArchiveWindowDays == nil || *got.ArchiveWindowDays != 14 {
		t.Errorf("ArchiveWindowDays = %v, want 14", got.ArchiveWindowDays)
	}
	if got.ArchiveSlots == nil || *got.ArchiveSlots != 4 {
		t.Errorf("ArchiveSlots = %v, want 4 (trimmed)", got.ArchiveSlots)
	}

	unset := config.ChannelConfig{ID: "UC1"}
	if v := channelToValues(unset); v["num_desc_lookbehind"] != "" || v["archive_window_days"] != "" || v["archive_slots"] != "" || v["output_directory"] != "" {
		t.Errorf("unset overrides must render blank: %v", v)
	}
}

// TestValidateChannelValues: ranges mirror config.Validate's global bounds
// (window 1-3650, slots 1-100) and lookbehind must be >= 0; blanks pass.
func TestValidateChannelValues(t *testing.T) {
	base := func() map[string]string {
		return map[string]string{"id": "UC1", "num_desc_lookbehind": "", "archive_window_days": "", "archive_slots": ""}
	}
	if msg := validateChannelValues(base()); msg != "" {
		t.Errorf("blanks rejected: %s", msg)
	}
	cases := map[string][2]string{
		"lookbehind negative": {"num_desc_lookbehind", "-1"},
		"lookbehind text":     {"num_desc_lookbehind", "three"},
		"window zero":         {"archive_window_days", "0"},
		"window too big":      {"archive_window_days", "3651"},
		"slots zero":          {"archive_slots", "0"},
		"slots too big":       {"archive_slots", "101"},
	}
	for name, c := range cases {
		vals := base()
		vals[c[0]] = c[1]
		if msg := validateChannelValues(vals); msg == "" {
			t.Errorf("%s: %s=%q accepted", name, c[0], c[1])
		}
	}
	ok := base()
	ok["num_desc_lookbehind"], ok["archive_window_days"], ok["archive_slots"] = "0", "3650", "100"
	if msg := validateChannelValues(ok); msg != "" {
		t.Errorf("boundary values rejected: %s", msg)
	}
}

// TestChannelFieldsIncludeOverrides: the form shows the four fields for both
// platforms (they are platform-neutral).
func TestChannelFieldsIncludeOverrides(t *testing.T) {
	keys := map[string]bool{}
	for _, f := range channelFields {
		keys[f.key] = true
		if f.key == "num_desc_lookbehind" || f.key == "output_directory" || f.key == "archive_window_days" || f.key == "archive_slots" {
			if f.platformFilter != "" {
				t.Errorf("%s must not be platform-filtered", f.key)
			}
		}
	}
	for _, k := range []string{"num_desc_lookbehind", "output_directory", "archive_window_days", "archive_slots"} {
		if !keys[k] {
			t.Errorf("channelFields lacks %s", k)
		}
	}
}
```

- [ ] **Step 2: Run to confirm failure**

```bash
cd D:/Git/Moombox/.worktrees/improvement-b-bugs-settings
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestChannelValuesRoundTrip|TestValidateChannelValues|TestChannelFieldsIncludeOverrides' ./internal/tui/ 2>&1 | head -5
```
Expected: `undefined: validateChannelValues` (build failed).

- [ ] **Step 3: Implement**

`internal/tui/settings.go` — insert after the `quality_preference` entry of `channelFields`:
```go
	{"num_desc_lookbehind", "Description lookbehind", fieldNumber, nil, "compare descriptions with N older feed items; blank = default", ""},
	{"output_directory", "Output directory", fieldText, nil, "per-channel override; blank = the global output directory", ""},
	{"archive_window_days", "Archive window (days)", fieldNumber, nil, "per-channel override, 1-3650; blank = global", ""},
	{"archive_slots", "Archive slots", fieldNumber, nil, "per-channel override, 1-100; blank = global", ""},
```

`internal/tui/settings_channels.go`:
- `channelToValues` gains four entries in its returned map:
  ```go
		"num_desc_lookbehind": optIntString(ch.NumDescLookbehind),
		"output_directory":    ch.OutputDirectory,
		"archive_window_days": optIntString(ch.ArchiveWindowDays),
		"archive_slots":       optIntString(ch.ArchiveSlots),
  ```
  with the helpers (add `"strconv"` to the imports):
  ```go
	// optIntString renders an optional override for the form: "" when unset.
	func optIntString(p *int) string {
		if p == nil {
			return ""
		}
		return strconv.Itoa(*p)
	}

	// optIntFromString parses a form value back to an optional override; blank
	// or unparseable text clears it (validateChannelValues rejects the latter
	// before the save reaches here).
	func optIntFromString(s string) *int {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil
		}
		return &n
	}
  ```
- `valuesToChannel`: after the quality block add
  ```go
	ch.NumDescLookbehind = optIntFromString(vals["num_desc_lookbehind"])
	ch.OutputDirectory = strings.TrimSpace(vals["output_directory"])
	ch.ArchiveWindowDays = optIntFromString(vals["archive_window_days"])
	ch.ArchiveSlots = optIntFromString(vals["archive_slots"])
  ```
- New function:
  ```go
	// validateChannelValues checks the numeric overrides before a save so a
	// typo produces a field error instead of a silently-cleared override.
	// Bounds match config.Validate's global monitors bounds.
	func validateChannelValues(vals map[string]string) string {
		check := func(key, label string, min, max int) string {
			s := strings.TrimSpace(vals[key])
			if s == "" {
				return ""
			}
			n, err := strconv.Atoi(s)
			if err != nil || n < min || n > max {
				return fmt.Sprintf("%s must be a whole number %d-%d (blank = default)", label, min, max)
			}
			return ""
		}
		if msg := check("num_desc_lookbehind", "Description lookbehind", 0, 1000); msg != "" {
			return msg
		}
		if msg := check("archive_window_days", "Archive window", 1, 3650); msg != "" {
			return msg
		}
		return check("archive_slots", "Archive slots", 1, 100)
	}
  ```
  (add `"fmt"` to the imports.)
- The add-seed map at `:100-104` gains `"num_desc_lookbehind": "", "output_directory": "", "archive_window_days": "", "archive_slots": ""`.
- In `handleChannelKey`'s edit-mode `keyEnter` (the branch that calls `saveCurrentChannel` / resolve), before saving: `if msg := validateChannelValues(m.channelEditValues); msg != "" { m.errorMsg = msg; m.status = saveError; return "" }` — read the function to place it right after the existing "Channel ID is required" check.

`internal/tui/setup_wizard.go`: the seed map at `:861-865` gains the same four keys; in `handleChannelEditKey`'s `keyEnter`, after the duplicate-ID check and before `valuesToChannel`, add `if msg := validateChannelValues(m.channelEditValues); msg != "" { m.errorMsg = msg; return "" }`.

- [ ] **Step 4: Run, mutants, commit**

```bash
cd D:/Git/Moombox/.worktrees/improvement-b-bugs-settings
gofmt -l internal/tui
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestChannel|TestValidateChannel|TestValuesToChannel|TestSaveCurrentChannel' -v ./internal/tui/ 2>&1 | grep -E '^(--- |ok|FAIL)'
```
Mutants: (1) in `optIntFromString` return `&n` even for `""` (i.e. drop the blank check) → RoundTrip FAILS; (2) change the slots upper bound to 1000 → `TestValidateChannelValues` FAILS ("slots too big").
Then `go test -count=1 ./internal/tui/` → `ok`, and commit:
```
feat(tui): channel form exposes num_desc_lookbehind, output_directory, archive_window_days, archive_slots

Blank means "use the global"; numeric overrides are validated (0+, 1-3650,
1-100) before the save so a typo is a field error, not a cleared override.
Setup wizard shares the form.
```
(with trailers). Ledger line.

---

### Task 3: The four per-channel fields in the Web channel dialog

**Files:**
- Modify: `web/public/index.html:1548-1627` (`add-channel-dialog`)
- Modify: `web/public/modules/settings.js:1431-1478` (`showAddChannelDialog`), `:1504-1601` (`saveChannel`)
- Modify: `web/public/modules/utils.js` (new export)
- Test: `web/tests/utils.test.mjs`

**Interfaces:**
- Produces: `export function applyChannelOverrides(channel, overrides)` in `modules/utils.js` — `overrides = {numDescLookbehind, outputDirectory, archiveWindowDays, archiveSlots}` where numbers are `undefined` for blank and `outputDirectory` is a trimmed string; returns `{ channel, error }` — `channel` with the four keys set or deleted, `error` a message string or `null`.

- [ ] **Step 1: Write the failing node tests** (append to `web/tests/utils.test.mjs`, adding `applyChannelOverrides` to its import list)

```js
test("applyChannelOverrides: values set, blanks clear, existing preserved", () => {
  const existing = { id: "UC1", name: "N", num_desc_lookbehind: 5, output_directory: "D:/old", archive_window_days: 7, archive_slots: 2 };
  const r = applyChannelOverrides({ ...existing }, {
    numDescLookbehind: undefined, outputDirectory: "", archiveWindowDays: 14, archiveSlots: 4,
  });
  assert.equal(r.error, null);
  assert.equal("num_desc_lookbehind" in r.channel, false, "blank clears the key");
  assert.equal("output_directory" in r.channel, false, "blank clears the key");
  assert.equal(r.channel.archive_window_days, 14);
  assert.equal(r.channel.archive_slots, 4);
  assert.equal(r.channel.name, "N", "unrelated keys untouched");
});

test("applyChannelOverrides: rejects out-of-range and non-integer values", () => {
  const cases = [
    [{ numDescLookbehind: -1 }, /lookbehind/i],
    [{ numDescLookbehind: 1.5 }, /lookbehind/i],
    [{ archiveWindowDays: 0 }, /window/i],
    [{ archiveWindowDays: 3651 }, /window/i],
    [{ archiveSlots: 0 }, /slots/i],
    [{ archiveSlots: 101 }, /slots/i],
  ];
  for (const [ov, re] of cases) {
    const r = applyChannelOverrides({ id: "UC1" }, ov);
    assert.match(r.error ?? "", re, JSON.stringify(ov));
  }
  const ok = applyChannelOverrides({ id: "UC1" }, { numDescLookbehind: 0, archiveWindowDays: 3650, archiveSlots: 100, outputDirectory: " D:/x " });
  assert.equal(ok.error, null);
  assert.equal(ok.channel.output_directory, "D:/x", "trimmed");
});
```

- [ ] **Step 2: Run to confirm failure**

```bash
cd D:/Git/Moombox/.worktrees/improvement-b-bugs-settings/web/tests && node --test utils.test.mjs 2>&1 | grep -E 'applyChannelOverrides|SyntaxError|does not provide' | head -3
```
Expected: `SyntaxError: The requested module '../public/modules/utils.js' does not provide an export named 'applyChannelOverrides'`.

- [ ] **Step 3: Implement the helper** (append to `web/public/modules/utils.js`)

```js
/**
 * Apply the per-channel override inputs to a channel payload. A blank input
 * (undefined number / empty string) clears the key so the server falls back
 * to the global value; a present value is validated against the same bounds
 * config.Validate uses for the globals. Returns { channel, error }.
 */
export function applyChannelOverrides(channel, { numDescLookbehind, outputDirectory, archiveWindowDays, archiveSlots }) {
  const out = { ...channel };
  const setInt = (key, value, label, min, max) => {
    if (value === undefined || value === null || value === "") {
      delete out[key];
      return null;
    }
    if (!Number.isInteger(value) || value < min || value > max) {
      return `${label} must be a whole number ${min}-${max} (blank = default)`;
    }
    out[key] = value;
    return null;
  };
  const err =
    setInt("num_desc_lookbehind", numDescLookbehind, "Description lookbehind", 0, 1000) ||
    setInt("archive_window_days", archiveWindowDays, "Archive window", 1, 3650) ||
    setInt("archive_slots", archiveSlots, "Archive slots", 1, 100);
  if (err) return { channel, error: err };
  const dir = (outputDirectory || "").trim();
  if (dir) out.output_directory = dir; else delete out.output_directory;
  return { channel: out, error: null };
}
```

- [ ] **Step 4: Dialog markup** — in `web/public/index.html`, inside `#add-channel-dialog` after the terms input block (`channel-terms-input`, ~`:1572`) and before the include-VODs row, add:

```html
                <sl-input id="channel-lookbehind-input" type="number" min="0" label="Description lookbehind"
                    help-text="Compare descriptions with N older feed items. Blank = default."></sl-input>
                <sl-input id="channel-output-dir-input" label="Output directory"
                    help-text="Per-channel override. Blank = the global output directory."></sl-input>
                <sl-input id="channel-archive-window-input" type="number" min="1" max="3650" label="Archive window (days)"
                    help-text="Per-channel override, 1-3650. Blank = global."></sl-input>
                <sl-input id="channel-archive-slots-input" type="number" min="1" max="100" label="Archive slots"
                    help-text="Per-channel override, 1-100. Blank = global."></sl-input>
```
Match the surrounding indentation and attribute style (read the neighbouring `sl-input`s first).

- [ ] **Step 5: Dialog logic** — in `web/public/modules/settings.js`:
  - `showAddChannelDialog(channel)`: after the existing field fills add
    ```js
    this.app.setInputValue("channel-lookbehind-input", channel?.num_desc_lookbehind ?? "");
    this.app.setInputValue("channel-output-dir-input", channel?.output_directory ?? "");
    this.app.setInputValue("channel-archive-window-input", channel?.archive_window_days ?? "");
    this.app.setInputValue("channel-archive-slots-input", channel?.archive_slots ?? "");
    ```
  - `saveChannel()`: after the `channel` object is built and its terms/quality blocks run (after `:1601`), add
    ```js
    const overrides = applyChannelOverrides(channel, {
      numDescLookbehind: this.app.getInputNumber("channel-lookbehind-input"),
      outputDirectory: document.getElementById("channel-output-dir-input")?.value ?? "",
      archiveWindowDays: this.app.getInputNumber("channel-archive-window-input"),
      archiveSlots: this.app.getInputNumber("channel-archive-slots-input"),
    });
    if (overrides.error) {
      this.app.showToast(overrides.error, "danger");
      return;
    }
    const channelPayload = overrides.channel;
    ```
    and POST `channelPayload` instead of `channel` in the `fetch` body. Add `applyChannelOverrides` to settings.js's import from `./utils.js` (read the file's existing import line and extend it). Remove the now-stale comment "preserve fields the UI doesn't expose" wording: the spread still preserves anything else, so reword to "start from the existing channel so keys this dialog doesn't manage survive".

- [ ] **Step 6: Tests, goja parity, commit**

```bash
cd D:/Git/Moombox/.worktrees/improvement-b-bugs-settings
(cd web/tests && node --test ./*.test.mjs 2>&1 | grep -E '(tests|pass|fail|skipped) [0-9]+')
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'Restart|Settings' ./internal/tui/ 2>&1 | tail -1
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./web/...
```
Expected: node `tests 94 / pass 94 / fail 0`; the goja-driven settings.js tests still `ok` (settings.js gained an import — `settingsVM` strips `import` lines, so the new symbol must not be CALLED at module top level; it is only called inside `saveChannel`, which those tests never invoke); `go build` (embed) fine.
Mutant: in `applyChannelOverrides` replace `delete out[key]` with `out[key] = undefined` → the first test's `"num_desc_lookbehind" in r.channel` assertion FAILS. Revert.
Commit: `feat(web): channel dialog exposes the four per-channel overrides` (+ trailers). Ledger line.

---

### Task 4: TUI import request percent-encodes its metadata headers

**Files:**
- Modify: `internal/tui/app_commands.go:232-250` (extract `newImportRequest`; add `"net/url"` import)
- Create: `internal/tui/app_commands_import_test.go`

**Interfaces:**
- Produces: `func newImportRequest(baseURL string, body io.Reader, title, channel string) (*http.Request, error)` — POST `baseURL + "/api/import"`, `Content-Type: application/octet-stream`, `X-Import-Title`/`X-Import-Channel` set to `url.PathEscape(strings.TrimSpace(v))` when non-empty.

- [ ] **Step 1: Failing test**

```go
package tui

import (
	"net/url"
	"strings"
	"testing"
)

// TestNewImportRequestEncodesHeaders: the server PathUnescapes these headers
// (import_routes.go decodeImportHeader) and the Web UI encodeURIComponent()s
// them; the TUI sent them raw, so a '%' was mangled and non-Latin-1 titles
// went out as raw UTF-8 in an HTTP header.
func TestNewImportRequestEncodesHeaders(t *testing.T) {
	title := " 50% off / 春のライブ+1 "
	channel := "Some Channel"
	req, err := newImportRequest("http://127.0.0.1:774", strings.NewReader("zip"), title, channel)
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != "POST" || req.URL.String() != "http://127.0.0.1:774/api/import" {
		t.Errorf("request = %s %s", req.Method, req.URL)
	}
	if got := req.Header.Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type = %q", got)
	}
	h := req.Header.Get("X-Import-Title")
	if strings.ContainsAny(h, " %春") && !strings.Contains(h, "%25") {
		t.Errorf("title header not percent-encoded: %q", h)
	}
	for _, r := range h {
		if r > 0x7e {
			t.Fatalf("non-ASCII byte in header: %q", h)
		}
	}
	back, err := url.PathUnescape(h)
	if err != nil || back != strings.TrimSpace(title) {
		t.Errorf("round trip = %q (%v), want %q", back, err, strings.TrimSpace(title))
	}
	if got := req.Header.Get("X-Import-Channel"); got != url.PathEscape(channel) {
		t.Errorf("channel header = %q, want %q", got, url.PathEscape(channel))
	}
}

func TestNewImportRequestOmitsEmptyHeaders(t *testing.T) {
	req, err := newImportRequest("http://127.0.0.1:774", strings.NewReader(""), "  ", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := req.Header["X-Import-Title"]; ok {
		t.Error("blank title must not set X-Import-Title")
	}
	if _, ok := req.Header["X-Import-Channel"]; ok {
		t.Error("empty channel must not set X-Import-Channel")
	}
}
```

- [ ] **Step 2: Run → `undefined: newImportRequest`.**

- [ ] **Step 3: Implement** — in `app_commands.go`, replace the inline block (`url := fmt.Sprintf("%s/api/import", baseURL)` through the two header `if`s) with `req, err := newImportRequest(baseURL, f, title, channel)` (keep the existing error handling that follows), and add:

```go
// newImportRequest builds the archive-import POST. The metadata headers are
// percent-encoded (url.PathEscape) because HTTP headers are Latin-1 and the
// server PathUnescapes them — the same contract the Web UI follows with
// encodeURIComponent. Blank values set no header.
func newImportRequest(baseURL string, body io.Reader, title, channel string) (*http.Request, error) {
	req, err := http.NewRequest("POST", baseURL+"/api/import", body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if t := strings.TrimSpace(title); t != "" {
		req.Header.Set("X-Import-Title", url.PathEscape(t))
	}
	if c := strings.TrimSpace(channel); c != "" {
		req.Header.Set("X-Import-Channel", url.PathEscape(c))
	}
	return req, nil
}
```
Rename the local variable `url` in the old code if it shadows the package (it did: `url := fmt.Sprintf(...)` — it is removed by the extraction). Add `"net/url"` to the imports.

- [ ] **Step 4: Run, mutant, commit** — both tests PASS; mutant: drop `url.PathEscape` on the title → the non-ASCII assertion FAILS. `go test -count=1 ./internal/tui/` ok. Commit `fix(tui): import request percent-encodes X-Import-Title/-Channel like the Web UI` (+ trailers). Ledger line.

---

### Task 5: One Web resume rule

**Files:**
- Modify: `web/public/modules/utils.js` (new export), `web/public/app.js:18-27`, `:2524`, `:4805`, `:4844`, and the utils import line `:10`
- Test: `web/tests/utils.test.mjs`

**Interfaces:**
- Produces: `export function canResumeJob(job)` — true when (`job.status ∈ {"Cancelled","Error","COOKIES?"}` or `job.status === "Finished" && job.incompleteTail`) AND `job.platform === "youtube"` AND `!!job.hasStaging`.

- [ ] **Step 1: Failing tests** (append to `utils.test.mjs`; import `canResumeJob`)

```js
test("canResumeJob: the single-job gate, applied everywhere", () => {
  const yt = (status, extra = {}) => ({ status, platform: "youtube", hasStaging: true, ...extra });
  assert.equal(canResumeJob(yt("Error")), true);
  assert.equal(canResumeJob(yt("Cancelled")), true);
  assert.equal(canResumeJob(yt("COOKIES?")), true);
  assert.equal(canResumeJob(yt("Finished", { incompleteTail: true })), true);
  assert.equal(canResumeJob(yt("Finished")), false, "a complete Finished job is not resumable");
  assert.equal(canResumeJob(yt("Downloading")), false);
  assert.equal(canResumeJob({ ...yt("Error"), platform: "twitch" }), false, "resume is YouTube-only");
  assert.equal(canResumeJob({ ...yt("Error"), hasStaging: false }), false, "no staging, nothing to resume");
  assert.equal(canResumeJob({ ...yt("Error"), hasStaging: undefined }), false);
});
```

- [ ] **Step 2: Run → export missing error.**

- [ ] **Step 3: Implement** — `utils.js`:
```js
const RESUMABLE_STATUSES = new Set(["Cancelled", "Error", "COOKIES?"]);

/**
 * Whether a job can be resumed (staging preserved, continue where it stopped).
 * One rule for the details button, the batch bar, and the batch action: the
 * status must be resumable — or Finished with an incomplete tail — AND the job
 * must be a YouTube job with staging on disk. The server enforces the same
 * three conditions (jobs.go resume route); this keeps the UI from offering
 * what the server will refuse.
 */
export function canResumeJob(job) {
  const statusOk = RESUMABLE_STATUSES.has(job.status) || (job.status === "Finished" && !!job.incompleteTail);
  return statusOk && job.platform === "youtube" && !!job.hasStaging;
}
```
`app.js`: delete `RESUME_STATUSES` and `canResumeStatus` (lines `:19` and `:21-24` incl. the comment); add `canResumeJob` to the `./modules/utils.js` import list; `:2524` becomes `const canResume = canResumeJob(job);`; `:4805` `selectedJobs.some(j => canResumeJob(j))`; `:4844` `eligibleJobs.filter(j => canResumeJob(j))`. Then `grep -n 'canResumeStatus\|RESUME_STATUSES' web/public/app.js` must print nothing.

- [ ] **Step 4: Tests, mutant, commit** — node suite `tests 95 / pass 95`; `node --check web/public/app.js` OK; mutant: drop `&& job.platform === "youtube"` → the twitch assertion FAILS. Commit `fix(web): batch resume uses the same gate as the details button` (+ trailers). Ledger line.

---

### Task 6: Probe goroutines recover

**Files:**
- Modify: `internal/connectivity/probe.go:36-46`
- Test: `internal/connectivity/probe_test.go`

**Interfaces:**
- Produces: package var `probeDial func(ctx context.Context, d *net.Dialer, addr string) (net.Conn, error)` (default: `d.DialContext(ctx, "tcp", addr)`), the seam the test uses.

- [ ] **Step 1: Failing test** (append)
```go
// TestReachabilityProbe_RecoversFromDialPanic: a panicking dial goroutine must
// still deliver its result, or the receive loop waits forever (project rule:
// every goroutine recovers inline). No logger is in scope in this free
// function, so the recover reports "unreachable" silently.
func TestReachabilityProbe_RecoversFromDialPanic(t *testing.T) {
	orig := probeDial
	t.Cleanup(func() { probeDial = orig })
	probeDial = func(context.Context, *net.Dialer, string) (net.Conn, error) { panic("boom") }

	done := make(chan bool, 1)
	go func() { done <- reachabilityProbe(context.Background(), []string{"127.0.0.1:1", "127.0.0.1:2"}) }()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("a panicking dial must not count as reachable")
		}
	case <-time.After(probeRaceTimeout + 2*time.Second):
		t.Fatal("reachabilityProbe hung after a dial panic")
	}
}
```
- [ ] **Step 2: Run → `undefined: probeDial`.**
- [ ] **Step 3: Implement**
```go
// probeDial performs one TCP dial; a variable so tests can inject failure
// modes (including a panic) without a real network.
var probeDial = func(ctx context.Context, d *net.Dialer, addr string) (net.Conn, error) {
	return d.DialContext(ctx, "tcp", addr)
}
```
and the goroutine body:
```go
		go func(addr string) {
			// Inline recovery (project rule). A panic before the send would
			// leave the receive loop waiting for a result that never comes;
			// report "unreachable" instead. No logger is available in this
			// free function — the Monitor's own recover logs at its level.
			defer func() {
				if r := recover(); r != nil {
					resultCh <- false
				}
			}()
			conn, err := probeDial(ctx, &d, addr)
			if err != nil {
				resultCh <- false
				return
			}
			_ = conn.Close()
			resultCh <- true
		}(t)
```
- [ ] **Step 4: Run, mutant, commit** — all four probe tests PASS; mutant: delete the `defer` block → the new test FAILS with "hung" (after ~5 s). `go test -count=1 ./internal/connectivity/` ok. Commit `fix(connectivity): probe dial goroutines recover and still report` (+ trailers). Ledger line — note the ruling: no Debug log (no logger in scope), deviating from spec §5 item 5's "log at Debug".

---

### Task 7: Config API validates client_token_ttl_days and connectivity.probe_targets; applies connectivity

**Files:**
- Modify: `internal/web/routes/config_routes.go` — `validateConfigUpdates` (network block, new connectivity block), `applyConfigUpdates` (new connectivity block after Bgutils, `:658-662`)
- Test: `internal/web/routes/config_routes_test.go`

- [ ] **Step 1: Failing tests** (append; model on `TestConfigUpdatesTrustedProxies`)
```go
func TestConfigUpdatesClientTokenTTL(t *testing.T) {
	for _, bad := range []float64{0, -1, 3651} {
		u := map[string]any{"network": map[string]any{"client_token_ttl_days": bad}}
		if errs := validateConfigUpdates(u); errs["network.client_token_ttl_days"] == "" {
			t.Errorf("ttl %v accepted: %v", bad, errs)
		}
	}
	good := map[string]any{"network": map[string]any{"client_token_ttl_days": float64(30)}}
	if errs := validateConfigUpdates(good); len(errs) != 0 {
		t.Errorf("ttl 30 rejected: %v", errs)
	}
	cfg := config.Defaults()
	applyConfigUpdates(cfg, good)
	if cfg.Network.ClientTokenTTLDays != 30 {
		t.Errorf("apply: %d, want 30", cfg.Network.ClientTokenTTLDays)
	}
}

func TestConfigUpdatesProbeTargets(t *testing.T) {
	bad := map[string]any{"connectivity": map[string]any{"probe_targets": []any{"1.1.1.1:443", "8.8.8.8"}}}
	if errs := validateConfigUpdates(bad); errs["connectivity.probe_targets"] == "" {
		t.Errorf("host without port accepted: %v", errs)
	}
	empty := map[string]any{"connectivity": map[string]any{"probe_targets": []any{}}}
	if errs := validateConfigUpdates(empty); errs["connectivity.probe_targets"] == "" {
		t.Errorf("empty list accepted (config.Validate would reject it): %v", errs)
	}
	notStrings := map[string]any{"connectivity": map[string]any{"probe_targets": []any{443}}}
	if errs := validateConfigUpdates(notStrings); errs["connectivity.probe_targets"] == "" {
		t.Errorf("non-string entry accepted: %v", errs)
	}
	good := map[string]any{"connectivity": map[string]any{"probe_targets": []any{" 1.1.1.1:443 ", "[2606:4700::1111]:443"}}}
	if errs := validateConfigUpdates(good); len(errs) != 0 {
		t.Errorf("valid targets rejected: %v", errs)
	}
	cfg := config.Defaults()
	applyConfigUpdates(cfg, good)
	want := []string{"1.1.1.1:443", "[2606:4700::1111]:443"}
	if len(cfg.Connectivity.ProbeTargets) != 2 || cfg.Connectivity.ProbeTargets[0] != want[0] || cfg.Connectivity.ProbeTargets[1] != want[1] {
		t.Errorf("apply: %v, want %v (trimmed)", cfg.Connectivity.ProbeTargets, want)
	}
}
```
- [ ] **Step 2: Run → both FAIL** (ttl accepted; probe_targets errors empty; apply leaves defaults).
- [ ] **Step 3: Implement** — in `validateConfigUpdates`'s network block, after the `network_access` switch:
```go
		if v, ok := net["client_token_ttl_days"].(float64); ok {
			if v < 1 || v > 3650 {
				errs["network.client_token_ttl_days"] = "client_token_ttl_days must be between 1 and 3650"
			}
		}
```
After the memory block (before `// Cookies` or wherever the section order puts it — keep config order: after `memory`), add:
```go
	// Connectivity sub-fields
	if conn, ok := updates["connectivity"].(map[string]any); ok {
		if v, ok := conn["probe_targets"].([]any); ok {
			valid := 0
			for _, item := range v {
				s, ok := item.(string)
				if !ok {
					errs["connectivity.probe_targets"] = "probe_targets must be an array of host:port strings"
					break
				}
				s = strings.TrimSpace(s)
				if s == "" {
					continue
				}
				if _, _, err := net2.SplitHostPort(s); err != nil {
					errs["connectivity.probe_targets"] = fmt.Sprintf("%q is not a valid host:port", s)
					break
				}
				valid++
			}
			if _, bad := errs["connectivity.probe_targets"]; !bad && valid == 0 {
				errs["connectivity.probe_targets"] = "at least one probe target is required"
			}
		}
	}
```
In `applyConfigUpdates`, after the Bgutils block (`:662`):
```go
	// Connectivity
	if conn, ok := updates["connectivity"].(map[string]any); ok {
		if v, ok := conn["probe_targets"].([]any); ok {
			targets := make([]string, 0, len(v))
			for _, item := range v {
				if s, ok := item.(string); ok {
					if s = strings.TrimSpace(s); s != "" {
						targets = append(targets, s)
					}
				}
			}
			if len(targets) > 0 {
				cfg.Connectivity.ProbeTargets = targets
			}
		}
	}
```
- [ ] **Step 4: Run, mutants, commit** — both PASS; `go test -count=1 ./internal/web/routes/` ok (16 s). Mutants: (1) change the ttl upper bound to 36500 → `TestConfigUpdatesClientTokenTTL` FAILS; (2) drop the `valid == 0` clause → the `empty` case FAILS. Commit `fix(config-api): validate client_token_ttl_days; validate and apply connectivity.probe_targets` (+ trailers). Ledger line.

---

### Task 8: `probe_targets` in the TUI Network section

**Files:**
- Modify: `internal/tui/settings.go` — Network `fields` (after `trusted_proxies`, `:91`), the load (`:456` area), the pre-save validation (`:555-578` area), the parse (`:699-705` area)
- Test: `internal/tui/tui_test.go` (append)

- [ ] **Step 1: Failing test** (model on `TestApplyValuesRejectsBadTrustedProxy`, `tui_test.go:365-402`)
```go
// TestApplyValuesProbeTargets: comma-separated host:port list; one bad entry
// blocks the save with a field error (config.Validate would refuse the whole
// file), blank falls back to the defaults, valid entries land trimmed.
func TestApplyValuesProbeTargets(t *testing.T) {
	cfg := config.Defaults()
	m := NewSettingsModel()
	m.configStore = config.NewStore(cfg, "")
	m.Open(cfg)

	if got := m.values["probe_targets"]; got != "1.1.1.1:443, 8.8.8.8:443, 9.9.9.9:443" {
		t.Fatalf("loaded probe_targets = %q", got)
	}

	m.values["probe_targets"] = "1.1.1.1:443, 8.8.8.8"
	m.applyValues()
	if m.status != saveError {
		t.Fatalf("status = %v, want saveError for a host without a port", m.status)
	}

	m.status, m.errorMsg = saveIdle, ""
	m.values["probe_targets"] = " 1.0.0.1:443 ,[2606:4700::1111]:443"
	m.applyValues()
	if m.status == saveError {
		t.Fatalf("valid entries rejected: %s", m.errorMsg)
	}
	if got := cfg.Connectivity.ProbeTargets; len(got) != 2 || got[0] != "1.0.0.1:443" || got[1] != "[2606:4700::1111]:443" {
		t.Errorf("ProbeTargets = %v", got)
	}

	m.values["probe_targets"] = "  "
	m.applyValues()
	if got := cfg.Connectivity.ProbeTargets; len(got) != 3 || got[0] != "1.1.1.1:443" {
		t.Errorf("blank must restore the defaults, got %v", got)
	}
}
```
- [ ] **Step 2: Run → FAILS at the loaded-value assertion (empty).**
- [ ] **Step 3: Implement** — field entry after `trusted_proxies`:
```go
			{"probe_targets", "Connectivity probe targets", fieldText, nil, "comma-separated host:port TCP targets raced to detect internet reachability; blank = defaults (requires restart)", nil},
```
Load: `m.values["probe_targets"] = strings.Join(cfg.Connectivity.ProbeTargets, ", ")`. Validation (next to the trusted_proxies loop):
```go
	for _, p := range strings.Split(m.values["probe_targets"], ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(p); err != nil {
			m.errorMsg = fmt.Sprintf("Probe targets: %q is not a valid host:port", p)
			m.status = saveError
			return
		}
	}
```
Parse (next to the proxies parse):
```go
	targets := []string(nil)
	for _, p := range strings.Split(m.values["probe_targets"], ",") {
		if p = strings.TrimSpace(p); p != "" {
			targets = append(targets, p)
		}
	}
	if len(targets) == 0 {
		targets = append([]string(nil), config.DefaultProbeTargets...)
	}
	m.cfg.Connectivity.ProbeTargets = targets
```
(`config.DefaultProbeTargets` exists at `internal/config/config.go:20`; confirm the exported name.)
- [ ] **Step 4: Run, mutant, commit** — PASS; mutant: drop the `len(targets) == 0` fallback → the blank case FAILS. `go test -count=1 ./internal/tui/` ok. Commit `feat(tui): Network section exposes connectivity.probe_targets` (+ trailers). Ledger line.

---

### Task 9: `probe_targets` in the Web Network section

**Files:**
- Modify: `web/public/index.html:~650` (after `cfg-trusted-proxies`), `web/public/modules/settings.js` (populate near `:693`, read near `:848-852`, payload near `:888-950`)
- Test: the goja-driven TUI parity tests (`go test -run Restart ./internal/tui/`) and `node --check`

- [ ] **Step 1: Markup** — after the `cfg-trusted-proxies` `sl-input` (`index.html:644-649`):
```html
                            <sl-input
                                id="cfg-probe-targets"
                                label="Connectivity Probe Targets"
                                placeholder="1.1.1.1:443, 8.8.8.8:443, 9.9.9.9:443"
                                help-text="Comma-separated host:port TCP targets raced to detect internet reachability. Leave empty for the defaults. Requires restart."
                            ></sl-input>
```
- [ ] **Step 2: Logic** — populate: `this.app.setInputValue("cfg-probe-targets", (config.connectivity?.probe_targets || []).join(", "));` beside the trusted-proxies line. Read: beside the trusted-proxies read,
```js
    const probeTargetsEl = document.getElementById("cfg-probe-targets");
    const probeTargets = (probeTargetsEl?.value || "")
      .split(",")
      .map((s) => s.trim())
      .filter(Boolean);
```
Payload: add a section `connectivity: { probe_targets: probeTargets }` — but ONLY when non-empty, so blank means "leave the stored defaults" (the API rejects an empty list): `...(probeTargets.length ? { connectivity: { probe_targets: probeTargets } } : {}),` inside the `payload` object literal.
- [ ] **Step 3: Checks and commit** — `node --check web/public/modules/settings.js`; `go test -count=1 -run 'Restart|Settings' ./internal/tui/` ok (the goja VM still evaluates settings.js); `node --test web/tests/*.test.mjs` unchanged count. Commit `feat(web): Network section exposes connectivity.probe_targets` (+ trailers). Ledger line.

---

### Task 10: Restart-required lists tell the truth

**Files:**
- Modify: `web/public/modules/settings.js:88-101` (three entries) and `:1118` (confirm sentence)
- Modify: `internal/tui/settings.go:64-77` (three keys), `:175` and `:183` help texts (append "(requires restart)")
- Modify: `internal/tui/settings_view.go:820` (overlay sentence)
- Modify: `internal/tui/settings_restart_parity_test.go:185-191` (`categoryOf`)
- Modify: `web/public/index.html` help-text of `cfg-memory-sidecar-hard-limit-mb` and `cfg-bgutils-use-sidecar` (append "Requires restart.")

- [ ] **Step 1: Failing test** — add the three keys to `categoryOf` first:
```go
		"probe_targets": "connectivity",
		"sidecar_hard_limit_mb": "sidecar", "use_sidecar": "sidecar",
```
Run `go test -count=1 -run 'Restart' ./internal/tui/` → `TestRestartOverlayNamesEveryCategoryItCovers` and `TestRestartRequiredListsAgree` FAIL (keys not in the lists; words missing from the sentences).
- [ ] **Step 2: Implement** — `restartRequiredKeys` gains `"probe_targets": true, "sidecar_hard_limit_mb": true, "use_sidecar": true`. `RESTART_REQUIRED_FIELDS` gains
```js
  { path: "connectivity.probe_targets", id: "cfg-probe-targets" },
  { path: "memory.sidecar_hard_limit_mb", id: "cfg-memory-sidecar-hard-limit-mb" },
  { path: "bgutils.use_sidecar", id: "cfg-bgutils-use-sidecar" },
```
Both sentences become `port, network access, connectivity probe targets, database path, log settings, cookie settings, sidecar settings` (TUI `settings_view.go:820` DimStyle string; Web `settings.js:1118` inside the parentheses). Help texts: TUI `:175` `use_sidecar` and `:183` `sidecar_hard_limit_mb` gain ` (requires restart)`; Web `index.html` the two controls' `help-text` gain ` Requires restart.`.
- [ ] **Step 3: Run the parity suite** — `go test -count=1 -run 'Restart' -v ./internal/tui/ | grep -E '^(--- |ok|FAIL)'` → all PASS (`TestRestartRequiredWebIdsExist`, `TestRestartBadgeIdsAreTheControlsTheirPathsDrive` included — read `settings_restart_badge_test.go` if it needs an entry). Mutant: remove `"sidecar settings"` from the Web sentence → `TestRestartOverlayNamesEveryCategoryItCovers` FAILS. Commit `fix(settings): probe_targets, sidecar_hard_limit_mb, use_sidecar are restart-required in both UIs` (+ trailers). Ledger line.

---

### Task 11: Hot-reload for go_soft_limit_mb, trust_forwarded_proto, ffmpeg_path

**Files:**
- Modify: `internal/web/routes/config_routes.go:48-64` (callbacks struct), `:808-853` (snapshot/fire)
- Modify: `cmd/moombox/routes_wiring.go:68-101` (wire three callbacks)
- Modify: `internal/worker/trim.go:22-49` (+ `SetFfmpegPath`, `mux()` accessor; use sites `:137,148,374,384,418,443,447`)
- Modify: `internal/web/auth.go:330-332` (comment)
- Test: `internal/web/routes/config_routes_test.go` (fixture + 3 tests + extend the unchanged test); Create: `internal/worker/trim_hotreload_test.go`

**Interfaces:**
- Produces: `ConfigRoutesCallbacks.OnGoSoftLimitChange func(mb int)`, `OnTrustForwardedProtoChange func(trust bool)`, `OnFfmpegPathChange func(path string)`; `func (ts *TrimService) SetFfmpegPath(path string)`; `func (ts *TrimService) mux() *engine.Muxer`.

- [ ] **Step 1: Failing tests**

`config_routes_test.go`: the fixture struct gains `goSoft atomic.Int32`, `trustProto atomic.Pointer[bool]`, `ffmpeg atomic.Pointer[string]`; `newConfigRoutesFixture` wires `OnGoSoftLimitChange: func(mb int) { f.goSoft.Store(int32(mb)) }`, `OnTrustForwardedProtoChange: func(b bool) { f.trustProto.Store(&b) }`, `OnFfmpegPathChange: func(p string) { f.ffmpeg.Store(&p) }`. Tests (each PUTs one field and asserts the holder; model on `TestConfigPutFiresLogLevelCallback`):
```go
// putConfig PUTs one JSON body and fails the test on a non-200.
func putConfig(t *testing.T, f *configRoutesFixture, body map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("PUT", "/api/config", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT %v: want 200, got %d (body: %s)", body, rec.Code, rec.Body.String())
	}
}

func TestConfigPutFiresGoSoftLimitCallback(t *testing.T) {
	f := newConfigRoutesFixture(t)
	putConfig(t, f, map[string]any{"memory": map[string]any{"go_soft_limit_mb": 512}})
	if got := f.goSoft.Load(); got != 512 {
		t.Errorf("OnGoSoftLimitChange: want 512, got %d", got)
	}
}

func TestConfigPutFiresTrustForwardedProtoCallback(t *testing.T) {
	f := newConfigRoutesFixture(t)
	putConfig(t, f, map[string]any{"network": map[string]any{"trust_forwarded_proto": true}})
	if got := f.trustProto.Load(); got == nil || !*got {
		t.Errorf("OnTrustForwardedProtoChange: want true, got %v", got)
	}
}

func TestConfigPutFiresFfmpegPathCallback(t *testing.T) {
	f := newConfigRoutesFixture(t)
	// An absolute path that pathFieldError accepts (read pathFieldError: it
	// rejects traversal and, for required fields, blanks; ffmpeg_path is optional).
	putConfig(t, f, map[string]any{"paths": map[string]any{"ffmpeg_path": "C:/tools/ffmpeg.exe"}})
	if got := f.ffmpeg.Load(); got == nil || *got != "C:/tools/ffmpeg.exe" {
		t.Errorf("OnFfmpegPathChange: want C:/tools/ffmpeg.exe, got %v", got)
	}
}
```
Extend `TestConfigPutDoesNotFireUnchangedCallbacks`: after the port-only PUT, `f.goSoft.Load() == 0`, `f.trustProto.Load() == nil`, `f.ffmpeg.Load() == nil`.

`internal/worker/trim_hotreload_test.go`:
```go
package worker

import "testing"

func TestTrimServiceSetFfmpegPathRebuildsMuxer(t *testing.T) {
	ts := NewTrimService(nil, "", testLogger{})
	if got := ts.mux().FFprobePath(); got != "ffprobe" {
		t.Fatalf("default ffprobe = %q", got)
	}
	ts.SetFfmpegPath("C:/tools/ffmpeg.exe")
	if got := ts.mux().FFprobePath(); got != "C:/tools/ffprobe.exe" && got != `C:\tools\ffprobe.exe` {
		t.Errorf("after SetFfmpegPath, ffprobe = %q", got)
	}
	ts.SetFfmpegPath("")
	if got := ts.mux().FFprobePath(); got != "ffprobe" {
		t.Errorf("blank path must restore PATH lookup, got %q", got)
	}
}
```
(`testLogger` — check the worker package's existing test logger type name and use it; if none, define a minimal one in this file.)

- [ ] **Step 2: Run → compile failures** (unknown fields/methods).

- [ ] **Step 3: Implement**

`config_routes.go` struct additions:
```go
	// OnGoSoftLimitChange is called when memory.go_soft_limit_mb changes so the
	// runtime soft limit follows without a restart (0 = disable).
	OnGoSoftLimitChange func(mb int)
	// OnTrustForwardedProtoChange is called when network.trust_forwarded_proto
	// changes; the web package's atomic flag follows.
	OnTrustForwardedProtoChange func(trust bool)
	// OnFfmpegPathChange is called when paths.ffmpeg_path changes so services
	// that captured the path at construction (TrimService) rebuild.
	OnFfmpegPathChange func(path string)
```
Snapshot before apply: `oldGoSoft := cfg.Memory.GoSoftLimitMB; oldTrust := cfg.Network.TrustForwardedProto; oldFfmpeg := cfg.Paths.FfmpegPath`; after `*cfg = cfgCopy` read the new three; after unlock fire each when changed and non-nil.

`routes_wiring.go` additions inside the literal:
```go
		OnGoSoftLimitChange: func(mb int) {
			if mb > 0 {
				debug.SetMemoryLimit(int64(mb) << 20)
			} else {
				debug.SetMemoryLimit(math.MaxInt64) // Go's "no limit"
			}
			s.log.Info("Go soft memory limit re-applied", slog.Int("mb", mb))
		},
		OnTrustForwardedProtoChange: web.SetTrustForwardedProto,
		OnFfmpegPathChange: func(path string) {
			s.trimSvc.SetFfmpegPath(path)
			s.log.Info("trim service ffmpeg path re-applied", slog.String("path", path))
		},
```
(imports `math`, `runtime/debug`, `log/slog` as needed — check what the file already imports.)

`trim.go`: add `muxerMu sync.RWMutex` beside `muxer`; add
```go
// SetFfmpegPath rebuilds the muxer for a new ffmpeg path (config hot-reload).
// In-flight trims keep the muxer they captured; new trims see the new path.
func (ts *TrimService) SetFfmpegPath(path string) {
	m := engine.NewMuxer(path, ts.logger)
	ts.muxerMu.Lock()
	ts.muxer = m
	ts.muxerMu.Unlock()
}

// mux returns the current muxer under the read lock.
func (ts *TrimService) mux() *engine.Muxer {
	ts.muxerMu.RLock()
	defer ts.muxerMu.RUnlock()
	return ts.muxer
}
```
and replace every `ts.muxer.` use site (`:137,148,374,384,418,443,447`) with `ts.mux().` — capture once per operation where a function uses it more than once (`m := ts.mux()`).

`auth.go:330-332` comment: "Set at startup from cmd/moombox/services.go and re-applied by the config hot-reload callback (routes_wiring.go); the atomic makes both safe."

- [ ] **Step 4: Run, mutants, commit** — `go test -count=1 ./internal/web/routes/ ./internal/worker/` ok; `go build ./...`; `go vet ./...`. Mutants: (1) fire `OnGoSoftLimitChange` unconditionally (drop the `!=` compare) → the unchanged test FAILS; (2) make `SetFfmpegPath` a no-op → the trim test FAILS. Commit `feat(config): go_soft_limit_mb, trust_forwarded_proto and ffmpeg_path apply on save` (+ trailers). Ledger line.

---

### Task 12: Docs

**Files:**
- Modify: `docs/spec/data-and-storage.md` rows `:469` (trust_forwarded_proto → "Hot-reloadable — no restart"), `:556` (GoSoftLimitMB → "Hot-reloadable (re-applied on save; 0 clears the limit)"), `:558` (SidecarHardLimitMB → "Requires restart"), the `use_sidecar` row (→ "Requires restart"), the `ffmpeg_path` row (→ "Hot-reloadable; TrimService rebuilds its muxer"), the `probe_targets` row (→ "Editable in both UIs' Network section; requires restart; API validates host:port"), the `client_token_ttl_days` row (→ "API validates 1-3650"), channel rows `:569-573` (→ "Editable in both channel editors; blank = global")
- Modify: `docs/spec/user-interfaces.md` — the Settings/Network rows (add Connectivity probe targets), the channel dialog/form description (the four fields), the batch-bar resume gate sentence if one exists (grep `resume`), the restart-required sentence (three new categories)
- Modify: `SPEC.md:668` if it lists which fields the UIs expose (grep "channel editor")

- [ ] **Step 1: grep each target before editing** (`grep -n 'go_soft_limit_mb\|use_sidecar\|ffmpeg_path\|probe_targets\|client_token_ttl_days\|trust_forwarded_proto' docs/spec/data-and-storage.md`; `grep -n -i 'trusted proxies\|channel dialog\|Add channel\|restart' docs/spec/user-interfaces.md`) and make the edits in place, one sentence each, no new sections.
- [ ] **Step 2: Citation checker + commit** — `go test -count=1 ./internal/docs/` ok. Commit `docs(spec): live vs restart-required settings, per-channel fields in both editors, probe targets` (+ trailers). Ledger line.

---

### Task 13: Arc close — full gates, plan removal

- [ ] **Step 1: Gates in the worktree**
```bash
cd D:/Git/Moombox/.worktrees/improvement-b-bugs-settings
gofmt -l ./cmd ./internal ./tools ./web
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./... && GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./...
GOOS=linux GOARCH=amd64 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build -o /dev/null ./cmd/moombox
GOOS=linux GOARCH=arm64 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build -o /dev/null ./cmd/moombox
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./... > D:/Git/Moombox/.superpowers/sdd/2026-09-04-improvement-b-bugs-settings/arc-b-gate.txt 2>&1; echo "exit $?"; grep -cE '^ok' D:/Git/Moombox/.superpowers/sdd/2026-09-04-improvement-b-bugs-settings/arc-b-gate.txt; grep -E '^(FAIL|---)' D:/Git/Moombox/.superpowers/sdd/2026-09-04-improvement-b-bugs-settings/arc-b-gate.txt | head
(cd web/tests && node --test ./*.test.mjs 2>&1 | grep -E '(tests|pass|fail|skipped) [0-9]+')
```
Expected: gofmt empty; builds silent; `exit 0`; `28`; no FAIL; node `tests 95 / pass 95 / fail 0 / skipped 0`. Paste LITERAL output.
- [ ] **Step 2: Delete the plan, commit**
```bash
git rm docs/superpowers/plans/2026-09-04-improvement-b-bugs-settings.md
git commit -F - <<'MSG'
chore: Arc B implemented — remove its plan

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Jw2Ne9cGsn3R9bJBNuXudp
MSG
git log --oneline main..HEAD
```
Expected: thirteen commits. Ledger line: `READY FOR ARC-CLOSE REVIEW`.

---

## After the plan (controller)

Fable arc-close review of `main..improvement-b-bugs-settings`; fix wave; scoped re-review; full gates on the merge candidate; `git merge --no-ff improvement-b-bugs-settings -F <msg-file>` (standing owner ruling: merge locally, no question); delete worktree + branch; post-merge gates on main; rebuild `moombox.exe`. Arc C next.
