# Arc 12c — `cookies.acquisition` and the read-only import guard: Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `cookies.acquisition` (`auto` | `profile`) — an explicit cookie acquisition mode that lets a desktop operator with a browser installed refresh credentials by reading their real browser profile read-only (audit G4) — and split the launch-path profile-dir security guard so it stops fast-failing the two read-only import sites when that mode is chosen (audit G3).

**Architecture:** One config field, defaulted and normalised in `internal/config`, reaches `internal/cookies` the same way `cookies.auto_enabled` and `browser_path`/`browser_type` already do: an injected `func() string` predicate on `AutoCookieService`, read live per pass, wired in `cmd/moombox/services.go` to the config store. It changes one refresh decision — `importedFromProfile` in `refreshCookiesDetailed` — and corrects the three sites that INFER that decision from the host (`decideStartupSeed`'s browser short-circuit, `StartPeriodicRefresh`'s browser-free-tick test, and the two read-only consultations of the cached launch-guard verdict, which is renamed to say what it is for and lifted only under the opt-in). Everything else is the settings surface: PUT validation, both UIs, the `R F` pre-flight sentence, and docs.

**Tech Stack:** Go 1.26, `BurntSushi/toml`, chi/v5 REST, vanilla-JS dashboard embedded via `go:embed` (Shoelace v2.16), Bubble Tea TUI, `dop251/goja` for executing the shipped JS modules inside Go tests.

**Spec:** `docs/superpowers/specs/2026-09-02-arc12c-acquisition-mode-design.md` (R1–R5, non-goals, invariants; committed later as `docs/superpowers/specs/2026-09-02-arc12c-acquisition-mode-design.md`). **Ruling 2026-09-02:** TWO values, `"auto"` and `"profile"`. The audit's third value `"browser"` is not added — it was observationally identical to `"auto"` at every site, and a value that behaves like another is a trap; a later semantics can add it additively.

**Anchors** were verified at `d6e0404` and are identical at `main`'s `16afaa6` for every file this plan cites; `internal/cookies/autocookies.go` and `autocookies_firefox.go` are mid-edit on `cookie-a1-linux-reap` (+11 / +42 lines uncommitted), so re-verify `refreshFirefox`/`startFirefoxSetup` line numbers after that arc merges. Every anchor below is also named by symbol.

## Global Constraints

Every task's requirements implicitly include this section.

- **`const livenessRecoveryArmed = false`** (`internal/cookies/refresh.go:748`) stays false. Nothing in this arc arms it.
- **`cmd/moombox/main.go:276-278`** (the `cfg.Cookies.AutoEnabled && len(cfg.Cookies.Platforms) > 0` → `SetExpectedPlatforms` seed) is **no-touch**.
- **The guard never leaves a launch site.** `validateBrowserProfileDirForLaunch`'s verdict stays consulted, unconditionally and in every mode, at all four subprocess-launching sites: `startChromiumSetup`, `refreshChromium`, `startFirefoxSetup`, `refreshFirefox`.
- **`dangerousProfilePathSubstrings` is not edited.** No forward-slash variants, no additions, no removals — audit G3 is explicit that adding them would newly break Linux desktop users already pointing at a real profile.
- **Every AUTOMATIC browser-free import stays behind `automaticImportGuard`.** `"profile"` mode makes a pass browser-free by setting, not by host; the two automatic callers (`decideStartupSeed`, the periodic tick) must recognise that or the timer re-reads a real profile over a live `cookies.txt`.
- **No cookie value in any log or UI string.** Never read, print, or open any cookie file or browser profile while developing (`D:\Moombox\cookies.txt`, `cookies.sqlite`, real browser profiles). Tests use `writeWALCookieProfile(t, youtubeAuthRows())` / `writeWALCookieProfileAt`, which build a synthetic profile in `t.TempDir()`.
- **Every goroutine gets an inline `defer func() { if r := recover(); ... }()`.** No goroutine is added by this arc; if one appears, it carries the recover.
- **The logger is the anonymous interface** (`Debug`/`Info`/`Warn`/`Error`, each `(msg string, args ...any)`), repeated in place. Never extracted to a named interface.
- **LF line endings** on every file this plan touches. Every cited file is LF today EXCEPT `.claude/skills/moombox-settings/SKILL.md` (CRLF); Task 7 edits it in place PRESERVING CRLF — do not convert it.
- **Gates, run at the end of every task, before the commit:**
  ```bash
  go build ./...
  go vet ./...
  GOOS=linux GOARCH=amd64 go build ./...
  gofmt -l internal/ cmd/       # must print nothing
  ```
  Plus `go test -count=1 ./...` **once** per task (the full suite, not just the touched package).
- **Every commit message ends with:**
  ```
  Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
  ```
- **Mutation discipline.** Every assertion this plan adds must be checked by a NAMED mutation: make the stated edit to the implementation, watch the named test fail, revert. The mutations are listed per task under **Mutations to run**. A test that survives its mutation is not finished.

### The two modes, settled

| Mode | Refresh decision (`refreshCookiesDetailed`) | Launch guard, 4 subprocess sites | Launch guard, 2 read-only sites | `decideStartupSeed` with a browser present | Periodic tick with a browser present |
|---|---|---|---|---|---|
| `auto` (default) | browser resolvable → launch; none → import | applies | applies | `autoImportBrowserPresent` (no seed) | browser refresh, no import guard |
| `profile` | **always import, read-only** | applies | **lifted** | falls through to `automaticImportGuard` | **an import, so `automaticImportGuard` applies** |

### The `R F` ladder, per mode

Rungs 1 and 2 ARE the `importedFromProfile` decision; rung 3 is `cookies.IsNoBrowserProfile` on both surfaces. Only the pre-flight sentence needs to know the mode.

| Rung | `auto` | `profile` |
|---|---|---|
| 1 — headless browser | taken when `auto_enabled` permits a launch AND a browser resolves | **never taken** |
| 2 — read-only profile import | taken when no browser, or the launch is gated | **always taken** when the profile dir exists |
| 3 — no profile at all (`ErrProfileNotFound` / `ErrNoBrowserFound`) → fall through to `R C` / a plain recheck | unchanged | unchanged |
| pre-flight line (both surfaces, byte-identical) | `Running browser cookie refresh...` | `Importing cookies from the browser profile...` |
| guard refusal on a real browser's profile dir (rung 2) | `ErrProfileDirNotOptedIn`, 422, message names the setting; not rung 3 | no refusal — the read proceeds |

---

## Task 1: The config field, its default, and its normalisation

**Files:**
- Modify: `internal/config/types.go:229` (end of `CookiesConfig`, after `DpapiFallback`)
- Modify: `internal/config/config.go:80-85` (`Defaults()` → `Cookies`)
- Modify: `internal/config/config.go:685-690` (`validateOrNormalize`, after the `refresh_interval` block)
- Test: `internal/config/config_test.go` (append; it already imports `os`, `path/filepath`, `strings`, `testing`)

**Interfaces:**
- Consumes: nothing.
- Produces: `config.CookiesConfig.Acquisition string` with TOML/JSON key `acquisition`; legal values `"auto"`, `"profile"`; `Defaults().Cookies.Acquisition == "auto"`; `config.Normalize` replaces anything else with `"auto"`; `config.Validate` returns an error mentioning `cookies.acquisition` for an unrecognised non-empty value and stays silent for `""`.

`migrateOldFormat` is **not** touched. `loadFromFile` (`config.go:159`) decodes over `Defaults()`, so a config written before this field keeps `"auto"` — absence is the default, with no migration to write.

- [ ] **Step 1: Write the failing tests**

Append to `internal/config/config_test.go`:

```go
// TestCookiesAcquisitionNormalises is cookies.acquisition's whole contract in
// one table: the two legal values survive, the empty string a hand-edited TOML
// can produce normalises silently, and anything else is BOTH reported by
// Validate and replaced by Normalize.
//
// The two halves are asserted together because they are one rule read twice:
// validateOrNormalize's reportOnly=true arm feeds PUT /api/config's error
// surface and Save's refusal, and its reportOnly=false arm is what stops a
// hand-edited config.toml from handing the cookie service a mode it does not
// understand. A test that only ran one arm would pass on an implementation
// that reports without normalising, which is the config file that boots into
// an undefined acquisition mode.
func TestCookiesAcquisitionNormalises(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      string
		want    string
		wantErr bool
		why     string
	}{
		{"auto", "auto", "auto", false, "the default must survive a round trip"},
		{"profile", "profile", "profile", false, "the browserless-import opt-in is legal"},
		{
			"empty is the absent case", "", "auto", false,
			"a config written before this field decodes to the zero value; there is nothing " +
				"for the operator to fix, so Validate must stay quiet and Normalize must fill it in",
		},
		{
			"unknown", "headless", "auto", true,
			"an unrecognised mode must be reported AND replaced — reporting alone leaves the " +
				"cookie service reading a value no branch handles",
		},
		{
			"browser is not a value", "browser", "auto", true,
			"the audit's third value was ruled out (2026-09-02): it behaved exactly like auto, " +
				"and a value that behaves like another is a trap — it must be reported, not accepted",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reported := Defaults()
			reported.Cookies.Acquisition = tc.in
			errs := Validate(reported)
			var mentioned bool
			for _, err := range errs {
				if strings.Contains(err.Error(), "cookies.acquisition") {
					mentioned = true
				}
			}
			if mentioned != tc.wantErr {
				t.Errorf("Validate mentioned cookies.acquisition = %v, want %v — %s (errs: %v)",
					mentioned, tc.wantErr, tc.why, errs)
			}
			if reported.Cookies.Acquisition != tc.in {
				t.Errorf("Validate mutated the config: acquisition = %q, want %q left alone",
					reported.Cookies.Acquisition, tc.in)
			}

			normalised := Defaults()
			normalised.Cookies.Acquisition = tc.in
			Normalize(normalised)
			if normalised.Cookies.Acquisition != tc.want {
				t.Errorf("Normalize left acquisition = %q, want %q — %s",
					normalised.Cookies.Acquisition, tc.want, tc.why)
			}
		})
	}
}

// TestCookiesAcquisitionNeedsNoMigration pins the reason migrateOldFormat is
// untouched by this field: Load starts from Defaults() and decodes over it, so
// a config.toml written before the key existed comes back as "auto" without a
// migration rule. If this ever fails, the fix is in loadFromFile's construction
// order, NOT a new migration clause.
func TestCookiesAcquisitionNeedsNoMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	const legacy = `
[cookies]
cookie_file = "./cookies.txt"
auto_enabled = true
browser_profile_dir = "./browser-profile"
`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Cookies.Acquisition != "auto" {
		t.Errorf("a config with no acquisition key loaded as %q, want \"auto\"", cfg.Cookies.Acquisition)
	}
	// The neighbours must be untouched — a migration that "helpfully" rewrote
	// them would be the destructive kind this file forbids.
	if !cfg.Cookies.AutoEnabled {
		t.Error("loading a legacy cookies section cleared auto_enabled")
	}
}

// TestCookiesAcquisitionRoundTripsThroughSave proves Save accepts and re-reads
// the non-default mode. Save runs Validate and REFUSES a failing config, so a
// mode the validator does not know would make every subsequent settings save
// fail — losing every other edit in the same save, which is the failure mode
// the TUI's trusted_proxies gate exists to prevent.
func TestCookiesAcquisitionRoundTripsThroughSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	cfg := Defaults()
	cfg.Cookies.Acquisition = "profile"
	if err := Save(cfg, path); err != nil {
		t.Fatalf("Save refused a legal acquisition mode: %v", err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Cookies.Acquisition != "profile" {
		t.Errorf("acquisition round-tripped as %q, want \"profile\"", reloaded.Cookies.Acquisition)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 -run 'TestCookiesAcquisition' ./internal/config/`
Expected: compile failure — `cfg.Cookies.Acquisition undefined (type CookiesConfig has no field or method Acquisition)`.

- [ ] **Step 3: Add the struct field**

In `internal/config/types.go`, immediately after the `DpapiFallback` field and before the closing brace of `CookiesConfig`:

```go
	// Acquisition selects HOW a cookie REFRESH acquires credentials.
	//
	//   "auto"    — the default, and exactly the behaviour that shipped before
	//               this field existed: a resolvable browser takes the headless
	//               launch path, a host with none imports the profile read-only.
	//   "profile" — never launch a browser for a REFRESH. Read
	//               browser_profile_dir read-only even on a desktop that has a
	//               browser installed. The only route to browserless import
	//               from a real signed-in profile on Windows, and the opt-in
	//               that lifts the launch-path profile-dir guard on the two
	//               read-only sites (audit G3).
	//
	// Two values by ruling (2026-09-02). The audit's "browser" was
	// observationally identical to "auto" at every site and was dropped: a
	// value that behaves like another is a trap. A later semantics can add it.
	//
	// Absent or empty means "auto", and there is NO migration: Load decodes the
	// file over Defaults(), so a config written before this key already carries
	// it. An unrecognised value is reported by Validate, replaced by Normalize.
	//
	// COMPOSES with AutoEnabled rather than replacing it — that flag still
	// decides whether a pass may launch at all, and whether the periodic timer
	// exists; under "profile" that timer and the automatic recovery attempt
	// import instead of launching, and the timer's import stays behind
	// automaticImportGuard. Read LIVE through AutoCookieService.AcquisitionMode,
	// so this is NOT restart-required.
	Acquisition string `toml:"acquisition" json:"acquisition"`
```

- [ ] **Step 4: Add the default**

In `internal/config/config.go`, `Defaults()`:

```go
		Cookies: CookiesConfig{
			CookieFile:        "./cookies.txt",
			BrowserProfileDir: "./browser-profile",
			Platforms:         []string{},
			RefreshInterval:   FlexDuration{Value: 360}, // 6 hours in minutes
			Acquisition:       "auto",
		},
```

- [ ] **Step 5: Add the normalisation**

In `internal/config/config.go`, in `validateOrNormalize`, immediately after the `cfg.Cookies.RefreshInterval` block:

```go
	// cookies.acquisition: two values, and anything else is the default.
	// Same shape as network.network_access above — fail() records the issue
	// for Validate's callers (Save refuses to write, PUT /api/config surfaces
	// a field error) and the !reportOnly arm replaces the value so a
	// hand-edited config.toml cannot hand AutoCookieService a mode no branch
	// handles.
	//
	// The empty string gets its own silent arm. It is not an operator mistake:
	// it is what a struct built without Defaults() carries, and what a UI that
	// omits the field sends. Reporting it would make Save refuse a config the
	// operator never typed.
	switch cfg.Cookies.Acquisition {
	case "auto", "profile":
	case "":
		if !reportOnly {
			cfg.Cookies.Acquisition = defaults.Cookies.Acquisition
		}
	default:
		fail("cookies.acquisition %q must be one of auto|profile", cfg.Cookies.Acquisition)
		if !reportOnly {
			cfg.Cookies.Acquisition = defaults.Cookies.Acquisition
		}
	}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test -count=1 -run 'TestCookiesAcquisition' ./internal/config/`
Expected: PASS, all three tests.

- [ ] **Step 7: Run the mutations**

**Mutations to run** (make the edit, confirm the named test FAILS, revert):

1. Delete `Acquisition: "auto"` from `Defaults()` → `TestCookiesAcquisitionNeedsNoMigration` fails (`loaded as ""`).
2. In the `default:` arm, delete the `if !reportOnly { ... }` replacement, keeping `fail(...)` → `TestCookiesAcquisitionNormalises/unknown` fails on the Normalize half.
3. Move `case "":` into the `default:` arm (i.e. delete the empty case) → `TestCookiesAcquisitionNormalises/empty_is_the_absent_case` fails on `wantErr`.
4. Change `case "auto", "profile":` to `case "auto":` → `TestCookiesAcquisitionNormalises/profile` fails.
5. Change it to `case "auto", "browser", "profile":` → `TestCookiesAcquisitionNormalises/browser_is_not_a_value` fails (the ruling is pinned, not just the enum).

- [ ] **Step 8: Run the full gates**

```bash
go build ./... && go vet ./... && GOOS=linux GOARCH=amd64 go build ./... && gofmt -l internal/ cmd/
go test -count=1 ./...
```

- [ ] **Step 9: Commit**

```bash
git add internal/config/types.go internal/config/config.go internal/config/config_test.go
git commit -m "$(cat <<'EOF'
feat(config): cookies.acquisition — auto | profile

The field, its default and its normalisation. Absent means "auto" with no
migration: Load decodes over Defaults(), so a config written before the key
existed already carries it. An unrecognised value is reported by Validate and
replaced by Normalize; the empty string is the absent case and stays silent.
Two values by ruling — the audit's "browser" behaved exactly like "auto" and
was dropped.

Audit G4.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

---

## Task 2: The REST surface — validate and apply

**Files:**
- Modify: `internal/web/routes/config_routes.go:329-355` (`validateConfigUpdates`, cookies block — NOT `jobs.go`; the `moombox-settings` skill is stale on that path)
- Modify: `internal/web/routes/config_routes.go:601-603` (`applyConfigUpdates`, cookies block, after `dpapi_fallback`)
- Test: `internal/web/routes/config_routes_test.go` (append; it already imports `config` and `strings`)

**Interfaces:**
- Consumes: `config.CookiesConfig.Acquisition` (Task 1).
- Produces: `validateConfigUpdates` returns the key `"cookies.acquisition"` with the message `acquisition must be auto or profile` for an unrecognised value; `applyConfigUpdates` assigns a trimmed, lower-cased `cookies.acquisition` string to `cfg.Cookies.Acquisition`. The wire key both UIs send is `cookies.acquisition`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/web/routes/config_routes_test.go`:

```go
// TestAcquisitionModeValidation is the API half of cookies.acquisition's
// contract, and it exists because applyConfigUpdates assigns the value
// straight through — validateConfigUpdates is the ONLY thing standing between
// a typo in a PUT body and a config the cookie service has to normalise behind
// the operator's back.
//
// The accept rows are the junction guard. Without them a validator that
// rejected every value would pass the reject rows and lock the setting out of
// both UIs.
func TestAcquisitionModeValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   any
		wantErr bool
	}{
		{"auto", "auto", false},
		{"profile", "profile", false},
		{"empty means leave it at the default", "", false},
		{"unknown word", "headless", true},
		{"a near miss", "profiles", true},
		{"the dropped third value", "browser", true},
		{"wrong type is ignored, not rejected", 3.0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateConfigUpdates(map[string]any{
				"cookies": map[string]any{"acquisition": tc.value},
			})
			got := errs["cookies.acquisition"] != ""
			if got != tc.wantErr {
				t.Errorf("rejected = %v, want %v (errs: %v)", got, tc.wantErr, errs)
			}
		})
	}
}

// TestAcquisitionModeApplied pins the second half. A field that validates and
// never persists is the checklist's first named mistake, and it is invisible
// from the UI: the save reports success and the setting silently reverts on the
// next load.
func TestAcquisitionModeApplied(t *testing.T) {
	cfg := config.Defaults()
	applyConfigUpdates(cfg, map[string]any{
		"cookies": map[string]any{"acquisition": "  PROFILE  "},
	})
	if cfg.Cookies.Acquisition != "profile" {
		t.Errorf("acquisition = %q, want %q — the value is trimmed and lower-cased on the way in "+
			"so a pasted or capitalised mode does not become an unrecognised one",
			cfg.Cookies.Acquisition, "profile")
	}

	// Omitting the key must leave the stored value alone, the same way every
	// other optional cookie field behaves. A save from a UI that has not
	// rendered this control yet must not reset the operator's choice.
	kept := config.Defaults()
	kept.Cookies.Acquisition = "profile"
	applyConfigUpdates(kept, map[string]any{"cookies": map[string]any{}})
	if kept.Cookies.Acquisition != "profile" {
		t.Errorf("an update with no acquisition key reset it to %q", kept.Cookies.Acquisition)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 -run 'TestAcquisitionMode' ./internal/web/routes/`
Expected: FAIL — `rejected = false, want true` on the three reject rows, and `acquisition = "auto", want "profile"`.

- [ ] **Step 3: Add the validation**

In `internal/web/routes/config_routes.go`, inside the `if ck, ok := updates["cookies"].(map[string]any); ok {` block, after the `refresh_interval` check:

```go
		// acquisition: mirrors config.validateOrNormalize's enum exactly, so a
		// value the API accepts is never one Normalize then rewrites behind
		// the operator's back. Empty is the "leave it at the default" case and
		// is not an error — applyConfigUpdates treats it the same way.
		if v, ok := ck["acquisition"].(string); ok {
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "", "auto", "profile":
			default:
				errs["cookies.acquisition"] = "acquisition must be auto or profile"
			}
		}
```

- [ ] **Step 4: Add the application**

In `internal/web/routes/config_routes.go`, inside `applyConfigUpdates`'s cookies block, after the `dpapi_fallback` assignment:

```go
		// Trimmed and lower-cased for the same reason browser_path is trimmed:
		// the TUI's and the dashboard's controls both feed strings, and a
		// value that differs from the enum only in case or whitespace would
		// pass validation above and then be silently replaced by Normalize.
		// An empty string is left to config.Normalize, which fills in the
		// default — assigning "" here and letting it through is what makes a
		// UI that omits the control harmless.
		if v, ok := ck["acquisition"].(string); ok {
			cfg.Cookies.Acquisition = strings.ToLower(strings.TrimSpace(v))
		}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -count=1 -run 'TestAcquisitionMode' ./internal/web/routes/`
Expected: PASS.

- [ ] **Step 6: Run the mutations**

1. Delete the `errs["cookies.acquisition"] = ...` line → `TestAcquisitionModeValidation/unknown_word` fails.
2. Remove `""` from the accept list → `TestAcquisitionModeValidation/empty_means_leave_it_at_the_default` fails.
3. Add `"browser"` to the accept list → `TestAcquisitionModeValidation/the_dropped_third_value` fails.
4. Delete the `applyConfigUpdates` assignment → `TestAcquisitionModeApplied` fails.
5. Drop `strings.ToLower(strings.TrimSpace(...))` from the apply site, assigning `v` raw → `TestAcquisitionModeApplied` fails on `"  PROFILE  "`.

- [ ] **Step 7: Run the full gates**

```bash
go build ./... && go vet ./... && GOOS=linux GOARCH=amd64 go build ./... && gofmt -l internal/ cmd/
go test -count=1 ./...
```

- [ ] **Step 8: Commit**

```bash
git add internal/web/routes/config_routes.go internal/web/routes/config_routes_test.go
git commit -m "$(cat <<'EOF'
feat(api): validate and apply cookies.acquisition on PUT /api/config

The enum mirrors config.validateOrNormalize exactly, so a value the API accepts
is never one Normalize rewrites behind the operator. Empty means "leave the
default"; the applied value is trimmed and lower-cased so a pasted mode does not
become an unrecognised one.

Audit G4.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

---

## Task 3: `AcquisitionMode` on the service, the refresh decision, and the wiring

**Files:**
- Modify: `internal/cookies/autocookies.go:445` (add the field after `ConfiguredBrowserOverride`), `~:135-155` (constants after `dangerousProfilePathSubstrings`), `~:923` (`resolvedAcquisition` after `browserLaunchBlocked`), `:1958` (`importedFromProfile`)
- Modify: `cmd/moombox/services.go:877-884` (wire after `ConfiguredBrowserOverride`)
- Modify: `docs/spec/data-and-storage.md:520-531` (the `[cookies]` field table), `:872` (the four-ways sentence), `:884` (after the `auto_enabled` table), `:890-892` (the `R F` rungs)
- Create: `internal/cookies/autocookies_acquisition_test.go`

**Interfaces:**
- Consumes: `config.CookiesConfig.Acquisition` (Task 1).
- Produces:
  - `cookies.AcquisitionAuto`, `cookies.AcquisitionProfile` — exported `string` constants `"auto"`, `"profile"`.
  - `AutoCookieService.AcquisitionMode func() string` — exported field, nil means `"auto"`.
  - `(*AutoCookieService).resolvedAcquisition() string` — unexported, returns one of the two constants, never anything else. Task 4 consumes this.

Existing seams the tests use, all verified: `writeWALCookieProfile`/`youtubeAuthRows` (`autocookies_profile_test.go:31,51`), `gatedBrowser` (`autocookies_browsergate_test.go:16`), `nopAutoCookieLogger` (`autocookies_periodic_test.go:10`), `s.detectBrowser`, `s.VerifyYouTubeAuth`/`VerifyTwitchAuth`, `StartSetup(platform string) error` (`autocookies.go:973`).

- [ ] **Step 1: Write the failing tests**

Create `internal/cookies/autocookies_acquisition_test.go`:

```go
package cookies

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// TestAcquisitionModeSelectsTheRefreshPath is the whole of cookies.acquisition
// inside this package, asserted as behaviour rather than as a field read.
//
// The discriminator is the one TestBrowserLaunchGateDropsTheBrowserNotTheRefresh
// already uses, and for the same reason: it needs no process. On an EMPTY JAR
// the browser branch gates on `len(refreshPlatforms()) == 0` and declines,
// while the import branch deliberately does not gate and completes. So with a
// profile present, a browser resolvable, and nothing in the jar: "auto" ->
// Ran = false, "profile" -> Ran = true.
//
// The Ran=false rows are the junction guard. Without them "Ran = true" for
// "profile" could equally mean the mode was never consulted and the host simply
// had no browser.
func TestAcquisitionModeSelectsTheRefreshPath(t *testing.T) {
	newService := func(t *testing.T, mode string) (*AutoCookieService, *int) {
		t.Helper()
		s := NewAutoCookieService(
			writeWALCookieProfile(t, youtubeAuthRows()),
			filepath.Join(t.TempDir(), "cookies.txt"),
			NewCookieJar(), nopAutoCookieLogger{})
		// A browser IS resolvable on every row. gatedBrowser's path does not
		// exist, so nothing can execute even if a branch tried.
		s.detectBrowser = func() *DetectedBrowser { return gatedBrowser() }
		s.AcquisitionMode = func() string { return mode }
		verified := 0
		s.VerifyYouTubeAuth = func(context.Context) (bool, error) { verified++; return true, nil }
		s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }
		return s, &verified
	}

	for _, tc := range []struct {
		name     string
		mode     string
		wantRan  bool
		verified bool
		why      string
	}{
		{
			name: "auto keeps the browser path", mode: AcquisitionAuto, wantRan: false,
			why: "auto is today's rule exactly: a resolvable browser takes the launch path, " +
				"which declines on an empty jar",
		},
		{
			name: "profile takes the import path", mode: AcquisitionProfile, wantRan: true, verified: true,
			why: "the whole point of the setting: a desktop with a browser installed reads the " +
				"configured profile read-only instead of launching anything",
		},
		{
			name: "an unrecognised mode falls back to auto", mode: "headless", wantRan: false,
			why: "resolvedAcquisition normalises, so a value that slipped past config validation " +
				"cannot put the service in an undefined state",
		},
		{
			name: "the dropped third value falls back to auto", mode: "browser", wantRan: false,
			why: "\"browser\" is not a mode (ruling 2026-09-02); a stale config carrying it must " +
				"behave as auto rather than as a fourth thing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, verified := newService(t, tc.mode)
			result, err := s.RefreshCookiesDetailed(context.Background())
			if err != nil {
				t.Fatalf("no row here may error: %v", err)
			}
			if result.Ran != tc.wantRan {
				t.Errorf("Ran = %v, want %v — %s", result.Ran, tc.wantRan, tc.why)
			}
			if got := *verified > 0; got != tc.verified {
				t.Errorf("verification ran = %v, want %v — %s", got, tc.verified, tc.why)
			}
		})
	}
}

// TestAcquisitionModeNilDefaultsToAuto pins the nil contract, the same way
// TestBrowserLaunchAllowedDefaultsToPermissive pins its neighbour's. Every
// existing caller and every test that builds the service by struct literal
// must keep today's behaviour without knowing this field exists.
func TestAcquisitionModeNilDefaultsToAuto(t *testing.T) {
	s := NewAutoCookieService("", "", nil, nopAutoCookieLogger{})
	if s.AcquisitionMode != nil {
		t.Fatal("the constructor now sets AcquisitionMode, so the nil default is no longer what callers get")
	}
	if got := s.resolvedAcquisition(); got != AcquisitionAuto {
		t.Errorf("resolvedAcquisition() with a nil callback = %q, want %q", got, AcquisitionAuto)
	}
}

// TestAcquisitionModeIsReadPerPass is the hot-reload assertion. The callback
// exists instead of a string copied in at construction for exactly one reason:
// the operator can change the setting while the process runs, and the next
// press of R F has to see it. A cached read passes every other test in this
// file and fails this one.
func TestAcquisitionModeIsReadPerPass(t *testing.T) {
	mode := AcquisitionAuto
	calls := 0
	s := NewAutoCookieService(
		writeWALCookieProfile(t, youtubeAuthRows()),
		filepath.Join(t.TempDir(), "cookies.txt"),
		NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return gatedBrowser() }
	s.AcquisitionMode = func() string { calls++; return mode }
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }

	first, err := s.RefreshCookiesDetailed(context.Background())
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if first.Ran {
		t.Fatal("under auto with a resolvable browser and an empty jar the pass must decline")
	}

	mode = AcquisitionProfile
	second, err := s.RefreshCookiesDetailed(context.Background())
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if !second.Ran {
		t.Error("the second pass did not see the changed mode — the callback is being cached, " +
			"which makes the setting restart-required without anything saying so")
	}
	if calls < 2 {
		t.Errorf("the callback was consulted %d times across two passes, want at least 2", calls)
	}
}

// TestAcquisitionModeKeepsTheNoSourceOutcomes pins what the mode does NOT
// change: the pre-work missing-directory block runs BEFORE the decision point,
// so "profile mode with nowhere to read from" and "auto with nothing to
// launch" keep the sentinels rung 3 is built on. It is the guard against
// solving G4 by moving that block — a mode that produced a different sentinel
// here would dead-end R F on the install with the least to fall back on.
func TestAcquisitionModeKeepsTheNoSourceOutcomes(t *testing.T) {
	newService := func(t *testing.T, mode string, browser *DetectedBrowser) *AutoCookieService {
		t.Helper()
		s := NewAutoCookieService(
			filepath.Join(t.TempDir(), "no-such-profile"),
			filepath.Join(t.TempDir(), "cookies.txt"),
			NewCookieJar(), nopAutoCookieLogger{})
		s.detectBrowser = func() *DetectedBrowser { return browser }
		s.AcquisitionMode = func() string { return mode }
		return s
	}

	t.Run("profile mode with no profile dir keeps ErrProfileNotFound", func(t *testing.T) {
		s := newService(t, AcquisitionProfile, gatedBrowser())
		_, err := s.RefreshCookiesDetailed(context.Background())
		if !errors.Is(err, ErrProfileNotFound) {
			t.Errorf("err = %v, want ErrProfileNotFound — the operator asked to read a profile "+
				"and there is none, which is 'run setup first', not a browser problem", err)
		}
	})

	t.Run("auto with no browser keeps ErrNoBrowserFound", func(t *testing.T) {
		s := newService(t, AcquisitionAuto, nil)
		_, err := s.RefreshCookiesDetailed(context.Background())
		if !errors.Is(err, ErrNoBrowserFound) {
			t.Errorf("err = %v, want ErrNoBrowserFound", err)
		}
	})
}

// TestStartSetupIgnoresAcquisitionMode is R1's last clause, pinned. StartSetup
// is ACQUISITION — an explicit gesture in a visible window, and the thing that
// populates the profile every other path then reads. Gating it on a setting
// that says "do not launch a browser for a REFRESH" would make the setting
// unreachable from a fresh install in profile mode: no profile to import, and
// no way to create one. Asserted by the sentinel it does NOT return;
// gatedBrowser's path does not exist, so nothing executes (the same shape as
// TestStartSetupIgnoresTheBrowserLaunchGate).
func TestStartSetupIgnoresAcquisitionMode(t *testing.T) {
	s := NewAutoCookieService(
		filepath.Join(t.TempDir(), "profile"),
		filepath.Join(t.TempDir(), "cookies.txt"),
		NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return gatedBrowser() }
	s.AcquisitionMode = func() string { return AcquisitionProfile }
	t.Cleanup(func() { _ = s.CancelSetup() })

	err := s.StartSetup("youtube")
	if err == nil {
		t.Fatal("a browser at a path that does not exist must not launch")
	}
	if errors.Is(err, ErrNoBrowserFound) {
		t.Error("StartSetup refused for want of a browser in profile mode — the interactive " +
			"login is acquisition and must never consult cookies.acquisition, or a fresh " +
			"install in profile mode has no way to create the profile it is told to read")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 -run 'TestAcquisitionMode|TestStartSetupIgnoresAcquisition' ./internal/cookies/`
Expected: compile failure — `undefined: AcquisitionAuto`, `s.AcquisitionMode undefined`.

- [ ] **Step 3: Add the constants, the field and the resolver**

In `internal/cookies/autocookies.go`, immediately after `dangerousProfilePathSubstrings`'s var block (before `validateBrowserProfileDir`):

```go
// The two values of cookies.acquisition, which decide how a REFRESH pass
// acquires credentials. Exported because cmd/moombox names them, and because a
// literal repeated across four packages is how an enum drifts.
//
// Two, by ruling (2026-09-02). The audit proposed a third, "browser", meaning
// "launch when a browser resolves" — which is what "auto" already means, so at
// every site the decision was profile-vs-rest and the two values could not be
// told apart. A value that behaves like another is a trap; it was dropped, and
// a later semantics can add it additively.
const (
	AcquisitionAuto    = "auto"
	AcquisitionProfile = "profile"
)
```

After `ConfiguredBrowserOverride` in the `AutoCookieService` struct (`:445`):

```go
	// AcquisitionMode returns cookies.acquisition from the ACTIVE config. It is
	// the injected form of that setting for the same reason BrowserLaunchAllowed
	// is the injected form of cookies.auto_enabled: internal/cookies has no
	// dependency on internal/config, and keeping it that way is why this is a
	// predicate rather than a string copied in at construction — the operator
	// can change the mode while the process runs and the next R F has to see it.
	//
	// Consulted at refreshCookiesDetailed's launch-vs-import decision, and at
	// the three sites that used to INFER that decision from the host: the two
	// READ-ONLY profile sites (which stop consulting the launch guard when this
	// answers "profile" — audit G3), decideStartupSeed's browser short-circuit,
	// and the periodic tick's browser-free test. resolvedBrowser is untouched,
	// and so is StartSetup — the interactive login is acquisition, not a refresh.
	//
	// nil = "auto", so every existing caller and test keeps today's behaviour.
	AcquisitionMode func() string
```

Beside `browserLaunchBlocked` (after it, `~:923`):

```go
// resolvedAcquisition is cookies.acquisition as this package uses it: always
// one of the two constants, never anything else.
//
// The normalisation is not redundant with config.validateOrNormalize. This
// package is reachable from tests and from a service built by struct literal
// that never went through config at all, and an unhandled third value would
// mean an undefined refresh path rather than a loud failure. Same rule as
// browserLaunchBlocked's nil predicate: the safe answer, always.
func (s *AutoCookieService) resolvedAcquisition() string {
	if s.AcquisitionMode == nil {
		return AcquisitionAuto
	}
	if strings.ToLower(strings.TrimSpace(s.AcquisitionMode())) == AcquisitionProfile {
		return AcquisitionProfile
	}
	return AcquisitionAuto
}
```

`autocookies.go` already imports `strings`.

- [ ] **Step 4: Change the decision point**

In `internal/cookies/autocookies.go`, the existing comment (`// importedFromProfile selects the browser-free path: ...` through `// ... launching nothing is precisely what they want.`) stays; append to it and replace the assignment at `:1958`:

```go
	// The disabled case is the same shape and lands here for the same reason:
	// an operator who hand-updates their browser profile presses R F to have it
	// read, and launching nothing is precisely what they want.
	//
	// THE THIRD WAY IN is cookies.acquisition = "profile", and it is the only
	// one that does not depend on the host. A Windows desktop with Firefox
	// installed resolves a browser on every pass, so before this the read-only
	// import was unreachable there — the operator could not ask to have their
	// REAL signed-in profile read instead of a managed one driven headlessly.
	// "auto" leaves the rule exactly as it was.
	importedFromProfile := browser == nil
	if s.resolvedAcquisition() == AcquisitionProfile {
		importedFromProfile = true
	}
```

Nothing downstream needs a change (verified at `d6e0404`): the DPAPI fallback at `:2028` is gated on `err != nil`, which the import branch has already returned on; `:2095`'s empty-profile downgrade is gated on `!importedFromProfile`; `:2153`'s pre-import verification now runs on a desktop with an existing `cookies.txt`, which is the rollback protection working as designed; and `renewed := importedFromProfile || browserActed` at `:2318` is still true for a read that happened. Every other `browser.` dereference after the decision is inside the `else` (browser) branch or a `browser != nil` guard.

- [ ] **Step 5: Wire the callback**

In `cmd/moombox/services.go`, immediately after the `autoCookieSvc.ConfiguredBrowserOverride = ...` assignment (`:877-884`):

```go
	// cookies.acquisition, read LIVE. The callback shape mirrors
	// ConfiguredBrowserOverride above and BrowserLaunchAllowed before it: the
	// cookies package cannot import config, and a value snapshotted here would
	// make the setting restart-required with nothing in either UI saying so.
	//
	// It composes with BrowserLaunchAllowed rather than replacing it. That flag
	// still answers "may this pass launch a browser at all"; this one answers
	// "should it, given one is available". "profile" with auto_enabled = false
	// imports, exactly as "auto" does with the flag off.
	autoCookieSvc.AcquisitionMode = func() string {
		var mode string
		s.configStore.Read(func(c *config.MoomboxConfig) {
			mode = c.Cookies.Acquisition
		})
		return mode
	}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test -count=1 -run 'TestAcquisitionMode|TestStartSetupIgnoresAcquisition' ./internal/cookies/`
Expected: PASS, all five tests.

- [ ] **Step 7: Run the mutations**

1. Delete the `if s.resolvedAcquisition() == AcquisitionProfile { ... }` block → `TestAcquisitionModeSelectsTheRefreshPath/profile_takes_the_import_path` and `TestAcquisitionModeIsReadPerPass` fail.
2. Swap the compared constant to `== AcquisitionAuto` → `.../profile_takes_the_import_path` fails AND `.../auto_keeps_the_browser_path` fails (caught twice — the point of keeping the junction row).
3. In `resolvedAcquisition`, return the normalised input unchanged (`return mode` for any value) → `.../an_unrecognised_mode_falls_back_to_auto` and `.../the_dropped_third_value_falls_back_to_auto` fail.
4. In `resolvedAcquisition`, delete the `if s.AcquisitionMode == nil` guard → `TestAcquisitionModeNilDefaultsToAuto` panics (a nil-func call), which is a fail.
5. Hoist the callback read to a field cached in `NewAutoCookieService` → `TestAcquisitionModeIsReadPerPass` fails.
6. Add `if s.resolvedAcquisition() == AcquisitionProfile { return nil }` at the top of `resolvedBrowser` → `TestStartSetupIgnoresAcquisitionMode` fails with `ErrNoBrowserFound`.

- [ ] **Step 8: Update the docs this task flips**

`docs/spec/data-and-storage.md`, the `[cookies]` field table — add a row after `DpapiFallback` (`:531`):

```markdown
| Acquisition | string | "auto" | `acquisition` | `auto` \| `profile`. Decides how a REFRESH acquires credentials: `auto` (a resolvable browser launches, a host with none imports the profile — the pre-existing rule), `profile` (never launch for a refresh; read `browser_profile_dir` read-only even on a desktop with a browser). Two values by ruling — the audit's `browser` behaved exactly like `auto` and was dropped. **Not** restart-required — `AutoCookieService.AcquisitionMode` reads it live. Composes with `auto_enabled`, which still owns whether a pass may launch at all. `StartSetup` never consults it. Absent or empty means `auto` and needs no migration: `Load` decodes over `Defaults()`. |
```

Same file, § Auto-Cookie Service, the four-ways sentence at `:872` — replace the trailing clause so the read-only import names its opt-in:

```markdown
`AutoCookieService` acquires credentials into `cookies.txt` four ways — an interactive browser login, a headless browser refresh, a browser-free import of a mounted browser profile (which `cookies.acquisition = "profile"` also selects on a desktop that HAS a browser), and `ImportCookies`, the operator-supplied Netscape file that `POST /api/cookies/import` delivers.
```

Same section, add a paragraph immediately after the `auto_enabled` surface table (after the `StartSetup` row at `:884`, before **The periodic timer is `gateExempt`**):

```markdown
**`cookies.acquisition` picks the path; `auto_enabled` picks whether a browser may run.** They compose and neither replaces the other. `acquisition` is read LIVE through `AutoCookieService.AcquisitionMode` (`internal/cookies/autocookies.go`), the same injected-predicate shape as `BrowserLaunchAllowed` and `ConfiguredBrowserOverride`. In a refresh it changes one thing, `importedFromProfile`: `"profile"` forces the browser-free import branch regardless of `resolvedBrowser()`, which is the only route to reading a REAL signed-in profile on a Windows desktop; `"auto"` leaves the rule as it was. It has two values by ruling — the audit's `"browser"` meant "launch when a browser resolves", which is what `"auto"` already means, so the two could not be told apart at any site and the value was dropped. Under `"profile"` the flag's timer and its one automatic recovery attempt import instead of launching, and the timer's import stays behind `automaticImportGuard` (below). `resolvedBrowser` is untouched, and `StartSetup` never consults the mode: the interactive login is acquisition, and gating it would make a fresh install in `"profile"` mode unable to create the profile it is told to read. The no-source outcomes are unchanged, because the missing-directory block runs BEFORE the decision: `"profile"` with no profile directory still returns `ErrProfileNotFound`, and `"auto"` with no browser and no profile still returns `ErrNoBrowserFound` — both still rung 3.
```

Same section, the `R F` ladder — add one line under the three numbered rungs (after `:892`):

```markdown
`cookies.acquisition` moves the gesture down the ladder without changing it: in `"profile"` mode rung 1 is never taken, rung 2 always is, and rung 3 is untouched. The TUI's pre-flight line and the dashboard's toast both name the mechanism that will actually run — `Importing cookies from the browser profile...` instead of `Running browser cookie refresh...` — because the browser sentence is a claim only one of the two modes can support. The two surfaces render the SAME sentence (`cookieRefreshPreflightToast` in `web/public/modules/utils.js`, `cookieRefreshFeedback` in `internal/tui/app_actions.go`), pinned by exact equality in `TestRefreshPreflightSentenceAgreesAcrossSurfaces`; unlike the rung-3 pair they name no per-surface affordance, so they do not diverge.
```

- [ ] **Step 9: Run the full gates**

```bash
go build ./... && go vet ./... && GOOS=linux GOARCH=amd64 go build ./... && gofmt -l internal/ cmd/
go test -count=1 ./...
```

- [ ] **Step 10: Commit**

```bash
git add internal/cookies/autocookies.go internal/cookies/autocookies_acquisition_test.go cmd/moombox/services.go docs/spec/data-and-storage.md
git commit -m "$(cat <<'EOF'
feat(cookies): acquisition mode decides launch vs read-only import

AcquisitionMode joins BrowserLaunchAllowed and ConfiguredBrowserOverride as an
injected, live-read predicate, and changes one refresh decision:
importedFromProfile. "profile" forces the browser-free import branch regardless
of resolvedBrowser(), which is the only route to reading a real signed-in
profile on a Windows desktop. Two values by ruling — the audit's "browser"
behaved exactly like "auto" and was dropped. StartSetup and resolvedBrowser are
untouched, and the no-source outcomes keep their sentinels — the
missing-directory block runs before the decision.

Audit G4.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

---

## Task 4: G3 — the launch guard says what it is for, and the three host-inferring sites follow the mode

**Files:**
- Modify: `internal/cookies/autocookies.go:156-177` (rename `validateBrowserProfileDir` → `validateBrowserProfileDirForLaunch`), `:501` (its one call site + comment), `:447-452` (the `profileDirErr` doc), `~:930` (the `refreshBrowser` doc sentence about `decideStartupSeed`), after `resolvedAcquisition` (add `readOnlyProfileDirErr`), `:2884` (`StartProfileSeed`'s Info line), `:2904-2908` (its `gateApplies` comment), `:2986-3000` (the periodic tick's browser-free test and its comment)
- Modify: `internal/cookies/autocookies_profile.go:503` (`importProfileCookies`), `:861-881` (`decideStartupSeed` doc + body)
- Modify: `internal/cookies/errors.go:78-93` (the block header's count), after `:138` (add `ErrProfileDirNotOptedIn`), `IsNoBrowserProfile`'s exclusion list
- Modify: `internal/cookies/dpapi/dpapi.go:12` (comment names the old guard name)
- Modify: `internal/cookies/autocookies_periodic_test.go:56` (section comment), `:75-76`, `:111-113` (the two calls and their `t.Errorf` strings)
- Modify: `internal/web/routes/cookies.go:476-479` (add the sentinel to the 422 arm)
- Modify: `internal/web/routes/cookies_shiftclick_test.go:303-311` (`TestRungThreeAgreesAcrossBothSurfaces`'s `sentinels` map — it FAILS on any sentinel the handler branches on that the map does not list)
- Modify: `docs/spec/data-and-storage.md:896` (rung-3 exclusion list), `:898` (the automatic-import rule's "two automatic callers" sentence), `:948` (the guard sentence); `docs/spec/security.md:5` (Scope) + new `##` section before **Content Security Policy** (`:450`); `docs/spec/operations.md:108`; `docs/spec/user-interfaces.md:579` (route row), `:640` (422 row)
- Create: `internal/cookies/autocookies_launchguard_test.go`, `internal/web/routes/cookies_acquisition_refusal_test.go`

**Interfaces:**
- Consumes: `resolvedAcquisition()`, `AcquisitionProfile` (Task 3).
- Produces:
  - `validateBrowserProfileDirForLaunch(profileDir string) error` — the renamed guard, message unchanged.
  - `cookies.ErrProfileDirNotOptedIn` — exported sentinel, the read-only refusal.
  - `(*AutoCookieService).readOnlyProfileDirErr() error` — nil in `"profile"` mode or when the dir is fine; otherwise a `%w`-wrapped `ErrProfileDirNotOptedIn`.

**The four launch sites keep the cached verdict unchanged, in every mode** — each reads `s.profileDirErr` on its first line: `startChromiumSetup` (`autocookies_chromium.go:47`), `refreshChromium` (`:207`), `startFirefoxSetup` (`autocookies_firefox.go:49`), `refreshFirefox` (`:180`). **The two read-only sites change:** `importProfileCookies` (`autocookies_profile.go:503`), `decideStartupSeed` (`:873`). **The two host-inferring sites change:** `decideStartupSeed`'s `resolvedBrowser() != nil` short-circuit (`:876`) and the periodic tick's `refreshBrowser(gateExempt) == nil` test (`autocookies.go:3000`) — both encode "a browser is present, so the browser path owns this install", which `"profile"` denies.

- [ ] **Step 1: Write the failing tests**

Create `internal/cookies/autocookies_launchguard_test.go`:

```go
package cookies

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// dangerousProfileDir is a path the launch guard refuses in every mode. Windows
// separators because that is what dangerousProfilePathSubstrings matches, and
// they are deliberately NOT being widened (audit G3: forward-slash variants
// would newly break Linux desktop users already pointing at a real profile).
// filepath.Abs prefixes a working directory on Linux; the substring survives
// either way, which is why the existing guard tests use the same literals.
const dangerousProfileDir = `C:\Users\test\AppData\Roaming\Mozilla\Firefox\Profiles\xxxxx.default-release`

// existingDangerousProfileDir creates a directory the guard refuses, for the
// tests that must get PAST the pre-work missing-directory check and reach the
// read-only site. The element carries the separators as a LITERAL: on Windows
// Join splits it into the nested Mozilla\Firefox\Profiles tree, and on Linux a
// backslash is an ordinary filename byte, so the lowercased absolute path
// contains `\mozilla\firefox\profiles` on both.
func existingDangerousProfileDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), `Mozilla\Firefox\Profiles\xxxxx.default-release`)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestLaunchGuardHoldsEveryLaunchSiteInEveryMode is the invariant G3 must not
// break. The guard stops a hostile config driving the user's REAL browser
// headlessly and exfiltrating the session through cookies.txt; that threat does
// not change with the acquisition mode, so neither does the guard. All four
// subprocess entry points, both modes — and nothing launches, because each one
// fast-fails on the cached verdict before it reaches an exec.
func TestLaunchGuardHoldsEveryLaunchSiteInEveryMode(t *testing.T) {
	for _, mode := range []string{AcquisitionAuto, AcquisitionProfile} {
		t.Run(mode, func(t *testing.T) {
			s := NewAutoCookieService(dangerousProfileDir,
				filepath.Join(t.TempDir(), "cookies.txt"), NewCookieJar(), nopAutoCookieLogger{})
			s.AcquisitionMode = func() string { return mode }
			b := gatedBrowser()

			if err := s.startFirefoxSetup(b, "https://example.invalid"); err == nil {
				t.Error("startFirefoxSetup accepted a real browser profile tree — the guard left a launch site")
			}
			if _, _, err := s.refreshFirefox(context.Background(), b); err == nil {
				t.Error("refreshFirefox accepted a real browser profile tree — the guard left a launch site")
			}
			chromium := &DetectedBrowser{Type: "chrome", Path: "moombox-no-such-browser", Name: "Chrome"}
			if err := s.startChromiumSetup(chromium, "https://example.invalid"); err == nil {
				t.Error("startChromiumSetup accepted a real browser profile tree — the guard left a launch site")
			}
			if _, _, err := s.refreshChromium(context.Background(), chromium); err == nil {
				t.Error("refreshChromium accepted a real browser profile tree — the guard left a launch site")
			}
		})
	}
}

// TestReadOnlyImportIsGatedOnTheOptIn is G3 at the site itself.
//
// Reading a real profile is safe and the codebase already argues it:
// snapshotFirefoxCookieDB copies cookies.sqlite AND its -wal sidecar into a
// 0700 temp dir and opens the COPY mode=ro, so SQLite never writes into the
// user's profile. The residual concern is EXFILTRATION — imported cookies land
// in cookies.txt — which is exactly the surface dpapi_fallback treats as
// opt-in, so the relaxation is tied to the opt-in rather than granted
// unconditionally. The refused row is the junction guard: without it "no
// error under profile" could mean the guard was deleted rather than gated.
func TestReadOnlyImportIsGatedOnTheOptIn(t *testing.T) {
	for _, tc := range []struct {
		mode      string
		wantRefus bool
		why       string
	}{
		{AcquisitionAuto, true, "the default must keep refusing — nobody opted in"},
		{AcquisitionProfile, false, "the opt-in is the whole point: a read-only import of a real profile proceeds"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			s := NewAutoCookieService(dangerousProfileDir,
				filepath.Join(t.TempDir(), "cookies.txt"), NewCookieJar(), nopAutoCookieLogger{})
			s.AcquisitionMode = func() string { return tc.mode }

			_, err := s.importProfileCookies()
			refused := errors.Is(err, ErrProfileDirNotOptedIn)
			if refused != tc.wantRefus {
				t.Errorf("refused = %v, want %v — %s (err: %v)", refused, tc.wantRefus, tc.why, err)
			}
			if !tc.wantRefus && err == nil {
				t.Fatal("the path does not exist, so some error is expected — just not the opt-in refusal")
			}
		})
	}
}

// TestRealProfileTreeReadsOnlyOnTheOptIn is G3 driven through the real entry
// point, on a profile tree that EXISTS and holds a cookie database — the
// desktop shape the setting was built for. The pre-work missing-directory
// block would otherwise answer before the read-only site is reached, which is
// why the sibling test above calls the site directly and this one does not.
//
// The auto row gates the browser (auto_enabled = false), which is the ONLY
// way a desktop reached the import path before this arc — and it is refused,
// with the setting named. The profile row reads it and verifies.
func TestRealProfileTreeReadsOnlyOnTheOptIn(t *testing.T) {
	newService := func(t *testing.T, mode string) (*AutoCookieService, *int) {
		t.Helper()
		dir := existingDangerousProfileDir(t)
		writeWALCookieProfileAt(t, dir, youtubeAuthRows())
		s := NewAutoCookieService(dir, filepath.Join(t.TempDir(), "cookies.txt"),
			NewCookieJar(), nopAutoCookieLogger{})
		s.detectBrowser = func() *DetectedBrowser { return gatedBrowser() }
		s.AcquisitionMode = func() string { return mode }
		verified := 0
		s.VerifyYouTubeAuth = func(context.Context) (bool, error) { verified++; return true, nil }
		s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }
		return s, &verified
	}

	t.Run("auto with the browser gated is refused and names the setting", func(t *testing.T) {
		s, verified := newService(t, AcquisitionAuto)
		s.BrowserLaunchAllowed = func() bool { return false }
		_, err := s.RefreshCookiesDetailed(context.Background())
		if !errors.Is(err, ErrProfileDirNotOptedIn) {
			t.Fatalf("err = %v, want ErrProfileDirNotOptedIn — a real profile tree must not be "+
				"read without the opt-in, even on the browser-free path", err)
		}
		if !strings.Contains(err.Error(), "acquisition") {
			t.Errorf("the refusal does not name the setting that lifts it: %q", err.Error())
		}
		if *verified != 0 {
			t.Error("verification ran on a pass that refused to read")
		}
	})

	t.Run("profile reads it read-only and verifies", func(t *testing.T) {
		s, verified := newService(t, AcquisitionProfile)
		result, err := s.RefreshCookiesDetailed(context.Background())
		if err != nil {
			t.Fatalf("the opt-in did not lift the guard: %v", err)
		}
		if !result.Ran || *verified == 0 {
			t.Errorf("Ran = %v, verified = %d — the import did not happen", result.Ran, *verified)
		}
	})
}

// TestReadOnlyRefusalDoesNotClaimALaunch pins the message split. The launch
// guard says "refusing to launch a headless session against it" — which, on a
// path that launches nothing and reads a copy, describes an event that did not
// happen and sends the operator looking for a browser process. Asserted in both
// directions: the launch sentence must still say it, the read-only one must not.
func TestReadOnlyRefusalDoesNotClaimALaunch(t *testing.T) {
	launchErr := validateBrowserProfileDirForLaunch(dangerousProfileDir)
	if launchErr == nil {
		t.Fatal("the launch guard stopped refusing a real browser profile tree")
	}
	if !strings.Contains(launchErr.Error(), "refusing to launch") {
		t.Errorf("the launch refusal no longer says what it refused: %q", launchErr.Error())
	}

	s := NewAutoCookieService(dangerousProfileDir,
		filepath.Join(t.TempDir(), "cookies.txt"), NewCookieJar(), nopAutoCookieLogger{})
	s.AcquisitionMode = func() string { return AcquisitionAuto }
	_, readErr := s.importProfileCookies()
	if readErr == nil {
		t.Fatal("the read-only site stopped refusing under the default mode")
	}
	if strings.Contains(readErr.Error(), "refusing to launch") {
		t.Errorf("the read-only refusal claims a launch that never happens: %q", readErr.Error())
	}
	if !strings.Contains(readErr.Error(), "acquisition") {
		t.Errorf("the read-only refusal does not name the setting that lifts it, so the operator "+
			"has no next step: %q", readErr.Error())
	}
}

// TestReadOnlyRefusalIsNotTheLadderBottomRung protects the property the six
// profile-import sentinels beside it already have. Rung 3 means there is NO
// profile, and both manual surfaces fall through to the in-process refresh on
// it. This refusal means the profile IS there and the config says do not read
// it — a diagnosable state with a one-line remedy, and folding it into a
// fallback would replace that remedy with a recheck that cannot apply it.
func TestReadOnlyRefusalIsNotTheLadderBottomRung(t *testing.T) {
	s := NewAutoCookieService(dangerousProfileDir,
		filepath.Join(t.TempDir(), "cookies.txt"), NewCookieJar(), nopAutoCookieLogger{})
	s.AcquisitionMode = func() string { return AcquisitionAuto }
	_, err := s.importProfileCookies()
	if IsNoBrowserProfile(err) {
		t.Error("the opt-in refusal landed on rung 3, which drops the only sentence naming the fix")
	}
}

// TestStartupSeedFollowsTheOptIn covers the second read-only site.
//
// decideStartupSeed has TWO short-circuits this arc touches, and both are
// wrong in profile mode for the same reason: they encode "a browser is
// available, so the browser path owns this install". In profile mode the
// operator has said it does not.
func TestStartupSeedFollowsTheOptIn(t *testing.T) {
	t.Run("a browser present no longer stands down in profile mode", func(t *testing.T) {
		s := NewAutoCookieService(
			writeWALCookieProfile(t, youtubeAuthRows()),
			filepath.Join(t.TempDir(), "cookies.txt"),
			NewCookieJar(), nopAutoCookieLogger{})
		s.detectBrowser = func() *DetectedBrowser { return gatedBrowser() }

		s.AcquisitionMode = func() string { return AcquisitionAuto }
		if got := s.decideStartupSeed(); got != autoImportBrowserPresent {
			t.Errorf("auto: verdict = %v, want autoImportBrowserPresent — a desktop's refresh path "+
				"owns the profile and an unsolicited startup import would launch a browser "+
				"nobody asked for", got)
		}

		s.AcquisitionMode = func() string { return AcquisitionProfile }
		if got := s.decideStartupSeed(); got != autoImportOK {
			t.Errorf("profile: verdict = %v, want autoImportOK — the operator asked for the "+
				"profile to be the source, and the boot import has nothing to lose here", got)
		}
	})

	t.Run("the guard still stands down under the default mode", func(t *testing.T) {
		s := NewAutoCookieService(dangerousProfileDir, filepath.Join(t.TempDir(), "cookies.txt"),
			NewCookieJar(), nopAutoCookieLogger{})
		s.detectBrowser = func() *DetectedBrowser { return nil }
		s.AcquisitionMode = func() string { return AcquisitionAuto }
		if got := s.decideStartupSeed(); got != autoImportNotConfigured {
			t.Errorf("verdict = %v, want autoImportNotConfigured", got)
		}
	})
}

// TestPeriodicTickInProfileModeObeysTheImportGuard is the third host-inferring
// site, and the twin of TestPeriodicTickWithABrowserIgnoresTheImportGuard.
//
// The tick decides "is this a browser-free import?" by refreshBrowser() == nil.
// In profile mode a browser resolves, so that answer is "no" — and the pass it
// then runs is an IMPORT, because the mode forces one. Without this arc's
// change the timer would re-read the operator's real profile over a live
// cookies.txt on every tick, which is exactly the automatic import the owner
// ruled out and the ONE rule automaticImportGuard exists to hold. Same fixture
// as the browser test: a populated cookies.txt, a browser present.
func TestPeriodicTickInProfileModeObeysTheImportGuard(t *testing.T) {
	cookiePath := ytAuthCookieFile(t)
	jar := NewCookieJar()
	if err := jar.Load(cookiePath); err != nil {
		t.Fatalf("load the fixture cookie file: %v", err)
	}

	log := &recordingCookieLogger{}
	s := NewAutoCookieService(writeWALCookieProfile(t, youtubeAuthRows()), cookiePath, jar, log)
	s.detectBrowser = func() *DetectedBrowser { return gatedBrowser() }
	s.AcquisitionMode = func() string { return AcquisitionProfile }
	var verified atomic.Int64
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { verified.Add(1); return true, nil }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartPeriodicRefresh(ctx, 20*time.Millisecond)

	if !waitForStandDowns(log, standDownsObserved) {
		t.Fatalf("the periodic timer never stood a profile-mode tick down on a populated cookies.txt "+
			"(imports=%d, stand-downs=%d) — the tick still infers the import from the host, so a "+
			"desktop in profile mode re-reads its real profile over live credentials on a schedule",
			verified.Load(), log.count(guardSkipped))
	}
	if got := verified.Load(); got != 0 {
		t.Errorf("the timer imported %d time(s) over a populated cookies.txt in profile mode — "+
			"an automatic browser-free import may only run when there is nothing to lose", got)
	}
}
```

`ytAuthCookieFile`, `recordingCookieLogger`, `waitForStandDowns`, `standDownsObserved`, `guardSkipped` and `writeWALCookieProfileAt` already exist in the package's test files (`autocookies_autoimport_guard_test.go`, `autocookies_periodic_start_test.go`, `autocookies_profile_test.go`).

Create `internal/web/routes/cookies_acquisition_refusal_test.go`:

```go
package routes

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// TestOptInRefusalAnswers422 pins the route's mapping for the new sentinel
// directly. TestRungThreeAgreesAcrossBothSurfaces reads the handler's cases
// from source and fails on a sentinel it does not know, but it cannot see a
// case that was DELETED — so the status is asserted over the wire here.
//
// Host-independent: the browser is gated rather than detected away, which is
// the only way a desktop reached the import path before this arc, and the
// profile tree exists so the pre-work missing-directory block does not answer
// first. The element carries Windows separators as a literal so the guard
// matches on Linux too (see existingDangerousProfileDir in internal/cookies).
func TestOptInRefusalAnswers422(t *testing.T) {
	dir := filepath.Join(t.TempDir(), `Mozilla\Firefox\Profiles\xxxxx.default-release`)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	svc := cookies.NewAutoCookieService(dir, filepath.Join(t.TempDir(), "cookies.txt"),
		cookies.NewCookieJar(), nopRouteLogger{})
	svc.BrowserLaunchAllowed = func() bool { return false }
	svc.AcquisitionMode = func() string { return cookies.AcquisitionAuto }

	r := chi.NewRouter()
	CookieRoutes(r, nil, svc, nil, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/cookies/auto-refresh", nil))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 — the refusal carries the one sentence naming the setting, "+
			"and any other status either flattens it to \"cookie refresh failed\" or sends the "+
			"dashboard down the rung-3 fallback. Body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "acquisition") {
		t.Errorf("the 422 body does not name cookies.acquisition: %s", rec.Body.String())
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 -run 'TestLaunchGuard|TestReadOnly|TestRealProfileTree|TestStartupSeedFollows|TestPeriodicTickInProfileMode' ./internal/cookies/ && go test -count=1 -run 'TestOptInRefusal' ./internal/web/routes/`
Expected: compile failure — `undefined: validateBrowserProfileDirForLaunch`, `undefined: ErrProfileDirNotOptedIn`.

- [ ] **Step 3: Rename the guard, everywhere the old name appears**

In `internal/cookies/autocookies.go`, rename the function and sharpen its doc. The body and message are unchanged:

```go
// validateBrowserProfileDirForLaunch refuses configured profile directories
// that sit inside any user-installed browser's real profile tree, for the
// purpose of LAUNCHING a browser against them. Empty input is allowed — it just
// leaves the service inert, since every entry point that needs a profile dir
// returns ErrProfileNotFound without one. Otherwise the path is resolved to
// absolute, lowercased, and checked against the dangerous-substring list above.
// Audit reports/cookies.md #26.
//
// The name carries the scope because the scope was the defect (audit G3). The
// cached verdict also fast-failed the two READ-ONLY sites, which launch
// nothing: the import copies cookies.sqlite and its -wal sidecar into a 0700
// temp dir and opens the COPY mode=ro, the line dpapi/dpapi.go already draws.
// Those two now consult readOnlyProfileDirErr instead. This function stays on
// every subprocess site, in every mode, unconditionally.
func validateBrowserProfileDirForLaunch(profileDir string) error {
```

Then every other mention of the old name — Task 7's `grep -rn "validateBrowserProfileDir\b"` must come back empty, and these are all of them:
- `autocookies.go:501` (the constructor call) and its comment (`// Validate ONCE at construction; subprocess-launching entry points ...` — extend: the read-only sites reuse the same verdict through `readOnlyProfileDirErr`).
- `internal/cookies/dpapi/dpapi.go:12` — the package comment's `The validateBrowserProfileDir check` → `The validateBrowserProfileDirForLaunch check`.
- `internal/cookies/autocookies_periodic_test.go:56` (the `// --- validateBrowserProfileDir (audit cookies.md #26) ---` section comment), `:75-76` and `:111-113` (the two calls and their `t.Errorf` strings). The test NAMES (`TestValidateBrowserProfileDirAccepts`/`Refuses`) may stay.

Extend the `profileDirErr` field doc at `:447-452`:

```go
	// profileDirErr captures any validation failure on the configured
	// profile directory (e.g. it points at a real browser's profile
	// tree). Computed once at construction so all subprocess-launching
	// entry points can fast-fail with the same message instead of each
	// re-running the check. Audit reports/cookies.md #26.
	//
	// The four subprocess sites read this field DIRECTLY, in every mode. The
	// two read-only sites go through readOnlyProfileDirErr instead, which
	// consults cookies.acquisition and rewords the refusal — a sentence about
	// refusing to launch is a claim a path that launches nothing cannot make.
	profileDirErr error
```

- [ ] **Step 4: Add the sentinel**

In `internal/cookies/errors.go`, the block header (`:78-93`) says "These five describe" and "when adding a sixth" — it already lists SIX (`ErrProfileDirUnreadable` joined later). Correct it as part of adding the seventh: "These six describe the realistic ways reading a MOUNTED browser profile fails" and "the property to preserve when adding to them — the seventh, ErrProfileDirNotOptedIn, is a config refusal rather than a read failure and keeps it for the same reason". Then, after `ErrNoCookiesInProfile` (`:138`):

```go
	// ErrProfileDirNotOptedIn is returned by the two READ-ONLY profile sites
	// when the configured browser_profile_dir sits inside a real installed
	// browser's profile tree and cookies.acquisition is not "profile".
	//
	// The seventh member of the block above, and it keeps that block's rule:
	// the profile IS there and is wrong in a specific, diagnosable way with a
	// one-line remedy the message names. Deliberately NOT on rung 3 — falling
	// through to a plain recheck would drop the only sentence that tells the
	// operator which setting to change.
	//
	// A CONFIG refusal, not a read failure: nothing was launched and nothing
	// was read. The guard's threat model is exfiltration of the operator's
	// daily-driver session through the cookies.txt export — the same surface
	// dpapi_fallback treats as opt-in — so the relaxation is tied to an explicit
	// setting rather than granted because a read happens to be safe. Audit G3.
	ErrProfileDirNotOptedIn = errors.New("reading this browser profile needs cookies.acquisition = \"profile\"")
```

Extend `IsNoBrowserProfile`'s exclusion paragraph — the list sentence and its closing count:

```go
// Deliberately EXCLUDES every profile-import failure — ErrProfileDirUnreadable,
// ErrProfileNotADirectory, ErrCookieDBNotFound, ErrCookieDBLocked,
// ErrCookieDBUnreadable, ErrNoCookiesInProfile — and ErrProfileDirNotOptedIn,
// the config refusal beside them. Those mean the profile IS there and is wrong
```

and `... the exact collapse the block comment above those six forbids.` → `those seven`.

- [ ] **Step 5: Add the read-only accessor**

In `internal/cookies/autocookies.go`, immediately after `resolvedAcquisition`:

```go
// readOnlyProfileDirErr is the launch guard's verdict as a READ-ONLY caller
// must see it: honoured by default, lifted by the operator's explicit opt-in,
// and worded for a path that launches nothing. Two changes from reading
// profileDirErr directly, and both are the point of audit G3.
//
// First, cookies.acquisition = "profile" is consent. The read copies
// cookies.sqlite and its -wal sidecar into a 0700 temp dir and opens the COPY
// mode=ro, so nothing writes into the user's profile; the residual concern is
// exfiltration through cookies.txt, which is a decision for the operator to
// make once, in a setting, exactly as dpapi_fallback is. Second, the sentence:
// the cached error says "refusing to launch a headless session against it",
// which on this path describes an event that never happens.
//
// Consulted per call rather than cached, so a mode changed at runtime reaches
// the next R F.
func (s *AutoCookieService) readOnlyProfileDirErr() error {
	if s.profileDirErr == nil {
		return nil
	}
	if s.resolvedAcquisition() == AcquisitionProfile {
		return nil
	}
	return fmt.Errorf("browser profile dir %q sits inside a real installed browser's profile tree, "+
		"so nothing was launched and nothing was read — set cookies.acquisition = %q to allow a "+
		"read-only import from it (audit cookies.md #26): %w",
		s.profileDir, AcquisitionProfile, ErrProfileDirNotOptedIn)
}
```

- [ ] **Step 6: Change the two read-only sites and the two host-inferring sites**

`internal/cookies/autocookies_profile.go`, in `importProfileCookies` (`:503`):

```go
	if err := s.readOnlyProfileDirErr(); err != nil {
		return "", err
	}
```

Same file, `decideStartupSeed` (`:861-881`) — the doc comment's "It fires only in the browserless case. On a desktop with a browser installed the normal refresh path already owns the profile, and an unsolicited startup pass there would just launch a browser nobody asked for." becomes "It fires only when the pass it would trigger is an import: a browserless host, or `cookies.acquisition = "profile"` on any host. On a desktop with a browser installed and the default mode the normal refresh path owns the profile, and an unsolicited startup pass there would just launch a browser nobody asked for." Then the body:

```go
func (s *AutoCookieService) decideStartupSeed() autoImportVerdict {
	if err := s.readOnlyProfileDirErr(); err != nil || s.profileDir == "" {
		return autoImportNotConfigured
	}
	// The browser short-circuit is about the HOST, not about a setting — a
	// desktop's normal refresh path already owns the profile, and an
	// unsolicited startup pass there would launch a browser nobody asked for.
	// cookies.acquisition = "profile" is the operator saying that path is not
	// the one they want, so the question stops applying: the refresh this seed
	// would trigger imports rather than launches, and there is no browser to
	// spare from an unrequested run.
	if s.resolvedAcquisition() != AcquisitionProfile && s.resolvedBrowser() != nil {
		return autoImportBrowserPresent
	}
	if !firefoxCookieDBExists(s.profileDir) {
		return autoImportNoProfileDB
	}
	return s.automaticImportGuard()
}
```

`internal/cookies/autocookies.go`, `refreshBrowser`'s doc (`~:930`): "and decideStartupSeed, which uses it to ask 'is this a browserless host?', a question about the machine rather than about a setting." → append "— asked only when cookies.acquisition has not already answered it."

Same file, `StartProfileSeed`'s Info line (`:2884`) — "no browser and no cookies to lose" is false on a desktop in profile mode:

```go
	s.logger.Info("browser-free import path and no cookies to lose — seeding cookies from the configured browser profile",
```

Same function, the `gateApplies` comment (`:2904-2908`), which reasons from the short-circuit:

```go
		// RefreshCookies, i.e. gateApplies: this is not the timer, so it has no
		// claim on the exemption. The two policies remain provably identical
		// here — decideStartupSeed reaches this point only when the pass will
		// take the import branch (no browser resolvable, or acquisition =
		// "profile" forcing it), and the gate's only power is to turn a non-nil
		// browser into nil — so gateExempt would buy nothing and would blur
		// what it means.
```

Same file, the periodic tick (`:2986-3000`). Extend the comment's "Only the browserless pass is an import" paragraph with: "— browserless because no browser resolves, or because cookies.acquisition = "profile" makes the pass an import regardless of the host. The second is the desktop case, where the profile IS the operator's real one and a scheduled re-read over live credentials is precisely what this rule refuses." Then the condition:

```go
				if s.refreshBrowser(gateExempt) == nil || s.resolvedAcquisition() == AcquisitionProfile {
					if v := s.automaticImportGuard(); v != autoImportOK {
```

The `guardSkipped` log line and everything else in the tick are unchanged. `TestAutomaticImportGuardHasExactlyItsTwoAutomaticCallers` reads the call graph and must stay green — this is the same call site, not a new caller.

- [ ] **Step 7: Route the sentinel, and tell the cross-surface test about it**

In `internal/web/routes/cookies.go`, add the sentinel to the existing 422 arm (`:476-479`) and note why:

```go
			// ErrProfileDirNotOptedIn joins them for the same reason: the
			// profile is there, the pass reached a decision about it, and the
			// message names the one setting that changes the answer. Flattening
			// it would leave a Windows desktop operator with "cookie refresh
			// failed" and no way to learn that cookies.acquisition exists.
			case errors.Is(err, cookies.ErrProfileDirUnreadable),
				errors.Is(err, cookies.ErrProfileNotADirectory),
				errors.Is(err, cookies.ErrProfileDirNotOptedIn),
				errors.Is(err, cookies.ErrCookieDBNotFound),
				errors.Is(err, cookies.ErrNoCookiesInProfile):
				jsonError(rw, err.Error(), http.StatusUnprocessableEntity)
```

In `internal/web/routes/cookies_shiftclick_test.go`, `TestRungThreeAgreesAcrossBothSurfaces`'s `sentinels` map (`:303-311`) — add the row, or the test fails with "the auto-refresh handler branches on cookies.ErrProfileDirNotOptedIn, which this test does not know about":

```go
		"ErrProfileDirNotOptedIn": cookies.ErrProfileDirNotOptedIn,
```

- [ ] **Step 8: Run the tests to verify they pass**

Run: `go test -count=1 ./internal/cookies/ ./internal/web/routes/`
Expected: PASS, including the renamed existing guard tests, `TestRungThreeAgreesAcrossBothSurfaces` (422 agrees: not a fallback status, not in `IsNoBrowserProfile`) and `TestAutomaticImportGuardHasExactlyItsTwoAutomaticCallers`.

- [ ] **Step 9: Run the mutations**

1. In `readOnlyProfileDirErr`, delete the `resolvedAcquisition() == AcquisitionProfile` early return → `TestReadOnlyImportIsGatedOnTheOptIn/profile` and `TestRealProfileTreeReadsOnlyOnTheOptIn/profile_reads_it_read-only_and_verifies` fail.
2. Swap that comparison to `== AcquisitionAuto` → `TestReadOnlyImportIsGatedOnTheOptIn` fails on BOTH rows (auto is lifted, profile is refused).
3. Make `readOnlyProfileDirErr` `return s.profileDirErr` unconditionally → `TestReadOnlyImportIsGatedOnTheOptIn/profile` and `TestReadOnlyRefusalDoesNotClaimALaunch` both fail.
4. Change `readOnlyProfileDirErr`'s wrap to `%w` of `ErrProfileNotFound` instead → `TestReadOnlyRefusalIsNotTheLadderBottomRung` fails.
5. Put `s.readOnlyProfileDirErr()` in place of `s.profileDirErr` at `refreshFirefox` → `TestLaunchGuardHoldsEveryLaunchSiteInEveryMode/profile` fails. Repeat for each of the other three launch sites; each must fail the same subtest.
6. Drop the `s.resolvedAcquisition() != AcquisitionProfile &&` clause in `decideStartupSeed` → `TestStartupSeedFollowsTheOptIn/a_browser_present_no_longer_stands_down_in_profile_mode` fails on the `profile` half.
7. Delete the browser short-circuit entirely → the same subtest fails on the `auto` half.
8. Drop the `|| s.resolvedAcquisition() == AcquisitionProfile` clause from the periodic tick → `TestPeriodicTickInProfileModeObeysTheImportGuard` fails (imports > 0); `TestPeriodicTickWithABrowserIgnoresTheImportGuard` must stay green throughout (the `auto` half of the same rule).
9. Delete `errors.Is(err, cookies.ErrProfileDirNotOptedIn)` from the route's 422 arm → `TestOptInRefusalAnswers422` fails (500).
10. Remove the new row from `TestRungThreeAgreesAcrossBothSurfaces`'s map → that test fails on the unmapped name (the existing guard, confirmed rather than assumed).

- [ ] **Step 10: Update the docs this task flips**

`docs/spec/data-and-storage.md:948` — the sentence about `dangerousProfilePathSubstrings`. Replace the paragraph:

```markdown
The anti-automation flags are mirrored on the headless launch deliberately: YouTube raises fraud scores for a browser that advertises itself as automated, which would invalidate the very cookies the pass is refreshing. `dangerousProfilePathSubstrings` (`internal/cookies/autocookies.go`) refuses a configured profile directory that belongs to a real installed browser, so a hostile config cannot launch Chrome against the user's actual signed-in profile and exfiltrate it through the `cookies.txt` export. The check is `validateBrowserProfileDirForLaunch`, computed once at construction, and its name carries its scope: it holds on all four subprocess sites (`startFirefoxSetup`, `refreshFirefox`, `startChromiumSetup`, `refreshChromium`) in every acquisition mode. The two READ-ONLY sites — `importProfileCookies` and `decideStartupSeed` — go through `readOnlyProfileDirErr` instead, which honours it by default and lifts it when `cookies.acquisition = "profile"`, and refuses with `ErrProfileDirNotOptedIn` rather than a sentence about a launch that cannot happen on that path. The list itself is Windows-only by construction and is deliberately NOT widened: forward-slash variants would newly refuse Linux desktop users already pointing at a real profile (audit G3).
```

Same file, `:896` — the rung-3 exclusion sentence lists six sentinels; append the seventh: `..., ErrNoCookiesInProfile — and ErrProfileDirNotOptedIn, the config refusal beside them (the profile IS there; the remedy is one setting).`

Same file, `:898` — "`decideStartupSeed` (the boot import) and `StartPeriodicRefresh`'s tick when that tick would be browser-free" → append: "— browser-free because no browser resolves, or because `cookies.acquisition = "profile"` makes the pass an import regardless of the host. That second case is a desktop reading the operator's REAL profile, and it is exactly the scheduled re-read over live credentials this rule refuses."

`docs/spec/security.md` — in the Scope sentence (`:5`), add "the browser-profile-directory guard and its read-only boundary" to the enumerated coverage. Then a new `##` section after **Rate Limiting**'s `---` (before **Content Security Policy**, `:450`):

```markdown
## Browser Profile Directory Guard

`cookies.browser_profile_dir` is operator-supplied and reaches two very different kinds of code, so it has two verdicts.

**The launch boundary.** `validateBrowserProfileDirForLaunch` (`internal/cookies/autocookies.go`) refuses a directory inside a real installed browser's profile tree. Its threat is a hostile config — or a compromised `PUT /api/config` write — pointing the auto-cookie service at the operator's daily-driver profile and driving a headless browser against it, which would exfiltrate the live session through the `cookies.txt` export. The verdict is computed once, in `NewAutoCookieService`, and is consulted at all four subprocess-launching sites unconditionally and in every acquisition mode. Nothing lifts it. `dangerousProfilePathSubstrings` is Windows-only by construction and is deliberately not widened with forward-slash variants: doing so would newly refuse Linux desktop users already pointing at a real profile, and the launch guard is the only place such variants would ever belong.

**The read boundary.** The browser-free import launches nothing. `snapshotFirefoxCookieDB` copies `cookies.sqlite` together with its `-wal` sidecar into a `0700` temp directory and opens the COPY `mode=ro`, so SQLite never writes into the user's profile — not even the WAL-index recovery a read-write open performs. That is the same line `internal/cookies/dpapi/dpapi.go` already draws. The residual risk on this path is therefore not corruption but **exfiltration**: imported cookies land in `cookies.txt`, so a config write alone would start harvesting the operator's signed-in session. So the relaxation is an explicit opt-in, `cookies.acquisition = "profile"`, exactly as `cookies.dpapi_fallback` is for the DPAPI read — and not a blanket "reads are safe" exemption. With the default mode the two read-only sites (`importProfileCookies`, `decideStartupSeed`) refuse with `ErrProfileDirNotOptedIn`, whose message names the setting rather than claiming a launch was refused.
```

`docs/spec/operations.md:108` (§ Cookies in a container — the guard is not described anywhere in this file today; this is an addition) — append to the paragraph:

```markdown
`cookies.acquisition` needs no entry in a container either: with no browser installed, `"auto"` already takes the import path. It exists for the desktop case — a host that HAS a browser and whose operator wants their real profile read instead — and it is the setting that lifts the launch-path profile-dir guard on the two read-only sites (`validateBrowserProfileDirForLaunch` stays on every subprocess site regardless).
```

`docs/spec/user-interfaces.md:579` — the `POST /api/cookies/auto-refresh` row: "headless browser when the gate allows one, otherwise an immediate browser-profile import" → "headless browser when the gate allows one and `cookies.acquisition` is `auto`, otherwise an immediate browser-profile import". `:640` — the 422 row's sentinel list gains `ErrProfileDirNotOptedIn`.

- [ ] **Step 11: Run the full gates**

```bash
go build ./... && go vet ./... && GOOS=linux GOARCH=amd64 go build ./... && gofmt -l internal/ cmd/
go test -count=1 ./...
```

- [ ] **Step 12: Commit**

```bash
git add internal/cookies/ internal/web/routes/cookies.go internal/web/routes/cookies_shiftclick_test.go internal/web/routes/cookies_acquisition_refusal_test.go docs/spec/data-and-storage.md docs/spec/security.md docs/spec/operations.md docs/spec/user-interfaces.md
git commit -m "$(cat <<'EOF'
fix(cookies): the launch guard says what it is for, and the host-inferring sites follow the mode

validateBrowserProfileDir becomes validateBrowserProfileDirForLaunch and stays on
all four subprocess sites in every mode. The two read-only sites go through
readOnlyProfileDirErr, which honours the guard by default and lifts it on the
explicit opt-in (acquisition = "profile"), refusing with ErrProfileDirNotOptedIn
(422, not rung 3) otherwise. decideStartupSeed's browser short-circuit and the
periodic tick's browser-free test both treat "profile" as an import, so every
automatic import stays behind automaticImportGuard. dangerousProfilePathSubstrings
is unchanged.

Audit G3.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

---

## Task 5: The dashboard — the select, the payload, and the ladder's toast

**Files:**
- Modify: `web/public/index.html:994` (after `<h4>Automatic Cookie Login</h4>`, before `<sl-switch id="cfg-auto-cookies-enabled">`)
- Modify: `web/public/modules/settings.js:693-696` (`populateConfigForm`, beside `dpapi_fallback`), `:848-849` (`saveConfig` gather), `:914-919` (the `cookies` payload)
- Modify: `web/public/modules/utils.js` (new exported `cookieRefreshPreflightToast`, beside `cookieRecheckToast` at `:588`)
- Modify: `web/public/app.js:10` (the utils import list), `:813` (`autoCookieRefresh`'s pre-flight toast)
- Create: `internal/web/routes/cookies_acquisition_panel_test.go`

**Interfaces:**
- Consumes: the `cookies.acquisition` wire key (Task 2).
- Produces: element id `cfg-cookies-acquisition` (an `sl-select` with `sl-option` values `auto`, `profile`); `payload.cookies.acquisition` in `saveConfig`; `cookieRefreshPreflightToast(acquisition)` exported from `utils.js` and called by `autoCookieRefresh` with `this.config?.cookies?.acquisition`. No `SettingsController.acquisitionMode()`.

`acquisition` is **not** added to `RESTART_REQUIRED_FIELDS` (`settings.js:87`) — it is read live. `TestRestartRequiredListsAgree` must stay green untouched.

**The test shape is settled, not conditional.** `appVM`/`jsString` do not exist in `internal/web/routes`, and `app.js` cannot evaluate bare in goja: it ends in a module-level `document.addEventListener("DOMContentLoaded", ...)`. The sentence therefore lives in `utils.js` — the same reason `cookieRecheckToast` and `cookieIndicatorState` live there ("precisely so it can be run here") — and is asserted by RUNNING it through the existing `utilsVM` + `jsCall` (`cookies_setup_utilsvm_test.go:53,62`); the call site inside `autoCookieRefresh` is pinned with the existing `jsBlock` + `jsCode` (`cookies_shiftclick_test.go:29,85`). The bracketed-`Contains` form was rejected: an inverted ternary keeps both sentences present and would survive its mutation.

- [ ] **Step 1: Write the failing tests**

Create `internal/web/routes/cookies_acquisition_panel_test.go`:

```go
package routes

import (
	"strings"
	"testing"
)

// TestAcquisitionSelectIsInTheShippedPanel asserts the control exists in the
// asset the binary actually serves, with both values. A settings field with
// no control is the checklist's dual-UI-parity mistake: the TUI can set it and
// the dashboard silently cannot.
func TestAcquisitionSelectIsInTheShippedPanel(t *testing.T) {
	html := readEmbeddedModule(t, "public/index.html")
	if !strings.Contains(html, `id="cfg-cookies-acquisition"`) {
		t.Fatal("the cookie acquisition select is not in index.html — the dashboard cannot set the mode")
	}
	for _, v := range []string{`value="auto"`, `value="profile"`} {
		if !strings.Contains(html, v) {
			t.Errorf("index.html has no option %s for cfg-cookies-acquisition", v)
		}
	}
	if strings.Contains(html, `value="browser"`) {
		t.Error("index.html offers a \"browser\" acquisition option — that value was ruled out; " +
			"the API rejects it and the select would save a mode the server refuses")
	}
}

// TestSaveConfigSendsAcquisition brackets the assertion to saveConfig, because
// a file-wide Contains passes on a literal that appears in a sibling helper.
// The payload key is what PUT /api/config validates and applies; a control that
// renders and never reaches the body is the "validates but never persists"
// failure, and it is invisible from the UI.
func TestSaveConfigSendsAcquisition(t *testing.T) {
	body := jsMethodBody(t, readEmbeddedModule(t, "public/modules/settings.js"), "saveConfig")
	if !strings.Contains(body, "cfg-cookies-acquisition") {
		t.Error("saveConfig never reads the acquisition select")
	}
	if !strings.Contains(body, "acquisition,") && !strings.Contains(body, "acquisition:") {
		t.Error("saveConfig builds no cookies.acquisition key — the setting would silently never save")
	}
}

// TestPopulateConfigFormReadsAcquisition is the other half. Without it the
// control renders empty on every load and a save writes whatever the browser
// defaulted the select to — quietly resetting the operator's mode.
func TestPopulateConfigFormReadsAcquisition(t *testing.T) {
	body := jsMethodBody(t, readEmbeddedModule(t, "public/modules/settings.js"), "populateConfigForm")
	if !strings.Contains(body, "cfg-cookies-acquisition") {
		t.Error("populateConfigForm never fills the acquisition select — it renders empty and a " +
			"save then overwrites the stored mode with the select's own default")
	}
	if !strings.Contains(body, "cookies?.acquisition") {
		t.Error("populateConfigForm does not read config.cookies.acquisition")
	}
}

// TestRefreshToastNamesTheMechanism RUNS the shipped helper and reads the
// sentence back: what the operator sees is what evaluates.
//
// The rung-1 sentence is a claim only one of the two modes can support. In
// "profile" mode nothing launches, so "Running browser cookie refresh..."
// describes a mechanism the operator switched off — the same class of unearned
// cause as telling a gated operator to install a browser they already have.
// The undefined row is an OLDER BINARY behind a newer dashboard: no
// acquisition key at all must read as auto.
func TestRefreshToastNamesTheMechanism(t *testing.T) {
	vm := utilsVM(t)
	for _, tc := range []struct {
		name string
		mode any
		want string
	}{
		{"auto", "auto", "Running browser cookie refresh..."},
		{"empty", "", "Running browser cookie refresh..."},
		{"absent (older binary)", nil, "Running browser cookie refresh..."},
		{"profile", "profile", "Importing cookies from the browser profile..."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := jsCall(t, vm, "cookieRefreshPreflightToast", tc.mode).(string)
			if got != tc.want {
				t.Errorf("cookieRefreshPreflightToast(%v) = %q, want %q", tc.mode, got, tc.want)
			}
		})
	}
}

// TestAutoCookieRefreshUsesThePreflightHelper pins the call SITE. The helper
// above can be perfect and unused; the bracket says autoCookieRefresh calls it,
// with the live config's mode, and no longer carries its own sentence.
func TestAutoCookieRefreshUsesThePreflightHelper(t *testing.T) {
	src := readEmbeddedModule(t, "public/app.js")
	code := jsCode(jsBlock(t, src, "async autoCookieRefresh() {"))
	if !strings.Contains(code, "cookieRefreshPreflightToast(this.config?.cookies?.acquisition)") {
		t.Error("autoCookieRefresh does not call cookieRefreshPreflightToast with the live " +
			"config's acquisition mode — the toast cannot name the mechanism")
	}
	if strings.Contains(code, "Running browser cookie refresh...") {
		t.Error("autoCookieRefresh still carries the browser sentence inline — two copies of one " +
			"sentence is how the surfaces drift")
	}
	// The import, matched as a parsed statement rather than a file-wide
	// Contains: the name must sit inside the braces of the utils.js import.
	utilsImport := regexp.MustCompile(`import \{[^}]*\bcookieRefreshPreflightToast\b[^}]*\} from "\./modules/utils\.js"`)
	if !utilsImport.MatchString(src) {
		t.Error("app.js does not import cookieRefreshPreflightToast from ./modules/utils.js — the " +
			"call above would throw ReferenceError in the browser")
	}
}
```

The file's import block needs `"regexp"` alongside `"strings"` and `"testing"`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 -run 'TestAcquisitionSelect|TestSaveConfigSendsAcquisition|TestPopulateConfigFormReadsAcquisition|TestRefreshToastNames|TestAutoCookieRefreshUsesThePreflightHelper' ./internal/web/routes/`
Expected: FAIL — the select is absent, the payload key is absent, `jsCall` fails loudly on the missing export, the call site is absent.

- [ ] **Step 3: Add the select**

In `web/public/index.html`, immediately after `<h4>Automatic Cookie Login</h4>` (`:994`) and before `<sl-switch id="cfg-auto-cookies-enabled">`:

```html
                            <sl-select
                                id="cfg-cookies-acquisition"
                                label="How refreshes get cookies"
                                value="auto"
                                style="margin-bottom: 0.75em;"
                            >
                                <sl-option value="auto">Automatic — launch a browser when one is available, otherwise read the profile</sl-option>
                                <sl-option value="profile">Browser profile only — never launch; read the profile directory below</sl-option>
                            </sl-select>
                            <p class="settings-help" style="margin-top: -0.4em;">
                                "Browser profile only" is the setting for a desktop where you want Moombox to
                                read your real signed-in browser profile instead of driving a managed one in the
                                background. It reads a snapshot copy and never writes into your profile — but the
                                cookies it finds are exported to cookies.txt, so it is opt-in. It also allows a
                                profile directory that belongs to an installed browser, which Automatic refuses.
                                Takes effect immediately; no restart.
                            </p>
```

- [ ] **Step 4: Populate and gather it**

In `web/public/modules/settings.js`, in `populateConfigForm`, beside the `dpapi_fallback` block (`:693`):

```js
    // Acquisition mode. setInputValue rather than a direct .value write so an
    // absent key leaves the select's own default rather than blanking it — a
    // blank select would save as "", which config.Normalize then turns back
    // into "auto" behind the operator.
    this.app.setInputValue("cfg-cookies-acquisition", config.cookies?.acquisition || "auto");
```

In `saveConfig`, beside the `dpapiFallback` gather (`:848`):

```js
    const acquisitionEl = document.getElementById("cfg-cookies-acquisition");
    const acquisition = acquisitionEl?.value || "auto";
```

And in the `cookies:` payload object (`:914`):

```js
      cookies: {
        cookie_file: cookieFile,
        active_platforms: activePlatforms,
        auto_enabled: autoEnabled,
        acquisition,
        browser_profile_dir: autoCookiesProfileDir,
        dpapi_fallback: dpapiFallback,
        // refresh_interval / browser_path / browser_type resolved below
      },
```

Dirty tracking needs nothing: `settingsContent` already delegates `sl-change`/`sl-input` to `_markDirty()` (`:706-707`).

- [ ] **Step 5: The sentence, in utils.js, and its call site**

In `web/public/modules/utils.js`, after `cookieRecheckToast`:

```js
/**
 * cookieRefreshPreflightToast is the sentence the dashboard shows BEFORE a
 * manual browser-cookie refresh runs (shift+click on the header, or the
 * Settings page's "Refresh cookies from browser profile" button).
 *
 * The default sentence is a claim only one of the two acquisition modes can
 * support. Under "profile" the server never launches anything — saying
 * "browser" would describe a mechanism the operator switched off, the same
 * unearned cause as telling a gated operator to install a browser they already
 * have. The server still decides; this only names what it will decide.
 *
 * Lives here rather than in app.js so it can be executed in Go tests. The
 * TUI's twin is cookieRefreshFeedback in internal/tui/app_actions.go, and the
 * two are pinned to the same sentences by exact equality
 * (TestRefreshPreflightSentenceAgreesAcrossSurfaces); unlike the rung-3 pair
 * they name no per-surface affordance, so they do not diverge. An absent mode
 * (an older binary that has no cookies.acquisition) reads as auto.
 */
export function cookieRefreshPreflightToast(acquisition) {
  return acquisition === "profile"
    ? "Importing cookies from the browser profile..."
    : "Running browser cookie refresh...";
}
```

In `web/public/app.js:10`, add `cookieRefreshPreflightToast` to the utils import list. Then in `autoCookieRefresh` (`:813`), replace the pre-flight toast:

```js
    this.showToast(cookieRefreshPreflightToast(this.config?.cookies?.acquisition), "primary");
```

- [ ] **Step 6: Rebuild the embedded assets and run the tests**

Run: `go build ./... && go test -count=1 -run 'TestAcquisitionSelect|TestSaveConfigSendsAcquisition|TestPopulateConfigFormReadsAcquisition|TestRefreshToastNames|TestAutoCookieRefreshUsesThePreflightHelper' ./internal/web/routes/`
Expected: PASS. (`go:embed` picks up `web/public/*` at build time; the tests read `webassets.PublicFS`, so the build must come first.)

- [ ] **Step 7: Run the mutations**

1. Delete `acquisition,` from the payload object → `TestSaveConfigSendsAcquisition` fails.
2. Delete the `populateConfigForm` line → `TestPopulateConfigFormReadsAcquisition` fails.
3. Remove `<sl-option value="profile">` → `TestAcquisitionSelectIsInTheShippedPanel` fails.
4. Add `<sl-option value="browser">` → the same test fails (the ruling is pinned on this surface too).
5. Replace `cookieRefreshPreflightToast`'s ternary with the plain browser sentence → `TestRefreshToastNamesTheMechanism/profile` fails.
6. Invert its condition to `!== "profile"` → the `auto`, `empty` and `absent` rows fail.
7. In `autoCookieRefresh`, call `this.showToast("Running browser cookie refresh...", "primary")` inline again → `TestAutoCookieRefreshUsesThePreflightHelper` fails.
8. Add `{ path: "cookies.acquisition", id: "cfg-cookies-acquisition" }` to `RESTART_REQUIRED_FIELDS` → `TestRestartRequiredListsAgree` (`internal/tui`) fails, which is the guard that this setting is NOT restart-required. Revert.

- [ ] **Step 8: Run the full gates**

```bash
go build ./... && go vet ./... && GOOS=linux GOARCH=amd64 go build ./... && gofmt -l internal/ cmd/
go test -count=1 ./...
```

- [ ] **Step 9: Commit**

```bash
git add web/public/index.html web/public/modules/settings.js web/public/modules/utils.js web/public/app.js internal/web/routes/cookies_acquisition_panel_test.go
git commit -m "$(cat <<'EOF'
feat(web): cookie acquisition mode select, and a toast that names the mechanism

The Cookies panel gains cfg-cookies-acquisition with one line of help per value;
populateConfigForm fills it and saveConfig sends cookies.acquisition. Not
restart-required, so it stays out of RESTART_REQUIRED_FIELDS. The pre-flight
toast moves to utils.js as cookieRefreshPreflightToast — executable in Go tests,
like cookieRecheckToast — and says "Importing cookies from the browser
profile..." in profile mode, because the browser sentence is a claim that mode
cannot support.

Audit G4.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

---

## Task 6: The TUI — the settings row, `R F`'s pre-flight line, and the cross-surface pin

**Files:**
- Modify: `internal/tui/settings.go:150` (the Cookies section's `fieldDef` list, after `auto_enabled`), `:511` (`loadValues`, after `dpapi_fallback`), `:781` (`applyValues`, after `DpapiFallback`)
- Modify: `internal/tui/app_actions.go:203-210` (the `R F` case; add two helpers near `recheckCookiesCmd` at `:28`)
- Create: `internal/tui/settings_acquisition_test.go`

**Interfaces:**
- Consumes: `config.CookiesConfig.Acquisition` (Task 1); `cookieRefreshPreflightToast` in `utils.js` (Task 5).
- Produces: the TUI field key `"acquisition"`, a `fieldCycle` row with options `[]string{"auto", "profile"}`; `(*App).cookieAcquisitionMode() string`; `cookieRefreshFeedback(mode string) string`.

`fieldCycle` is the enum row type — the precedent is `network_access` (`settings.go:84`) and `log_level` (`:105`). `fieldDef` has six positional fields (`key, label, ftype, options, help, previewFn`). `browser_type` next door is `fieldText` on purpose; do not copy that shape. `NewSettingsModel`, `Open(cfg)` (which reads `m.configStore`, so set it first), `applyValues`, `m.values`, `m.status`/`saveError`, `m.errorMsg` and `restartRequiredKeys` (`:64`) are the existing surfaces `settings_security_test.go` already drives; `App.configStore` is `app.go:446`.

- [ ] **Step 1: Write the failing tests**

Create `internal/tui/settings_acquisition_test.go`:

```go
package tui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/dop251/goja"

	"github.com/vampiricwulf/Moombox/internal/config"
	webassets "github.com/vampiricwulf/Moombox/web"
)

// TestAcquisitionRowRoundTrips is the dual-UI-parity assertion: the TUI must be
// able to read AND write the mode, or one surface silently owns a setting the
// other can only display.
//
// Both directions, because they fail differently. A missing loadValues entry
// renders an empty row and then writes "" on the next save; a missing
// applyValues entry accepts the edit, reports "Saved", and discards it.
func TestAcquisitionRowRoundTrips(t *testing.T) {
	cfg := config.Defaults()
	cfg.Cookies.Acquisition = "profile"
	m := NewSettingsModel()
	m.configStore = config.NewStore(cfg, "")
	m.Open(cfg)

	if got := m.values["acquisition"]; got != "profile" {
		t.Errorf("loadValues put %q in the acquisition row, want %q — the row renders the stored "+
			"mode or it overwrites it on the next save", got, "profile")
	}

	m.values["acquisition"] = "auto"
	m.applyValues()
	if m.status == saveError {
		t.Fatalf("applyValues rejected a legal mode: %s", m.errorMsg)
	}
	if cfg.Cookies.Acquisition != "auto" {
		t.Errorf("applyValues wrote %q, want %q", cfg.Cookies.Acquisition, "auto")
	}
}

// TestAcquisitionRowIsACycleWithTwoOptions pins the control type. A free-text
// row here would let an operator type a mode the validator then replaces with
// the default behind their back — the setting would appear to accept anything
// and quietly do one thing.
func TestAcquisitionRowIsACycleWithTwoOptions(t *testing.T) {
	var found *fieldDef
	for i := range sections {
		if sections[i].name != "Cookies" {
			continue
		}
		for j := range sections[i].fields {
			if sections[i].fields[j].key == "acquisition" {
				found = &sections[i].fields[j]
			}
		}
	}
	if found == nil {
		t.Fatal("the Cookies section has no acquisition row — the TUI cannot set the mode")
	}
	if found.ftype != fieldCycle {
		t.Errorf("acquisition is %v, want fieldCycle — the enum row type (see network_access, "+
			"log_level); a text row would accept anything and silently normalise", found.ftype)
	}
	want := []string{"auto", "profile"}
	if len(found.options) != len(want) {
		t.Fatalf("acquisition has %d options, want %d: %v", len(found.options), len(want), found.options)
	}
	for i, opt := range want {
		if found.options[i] != opt {
			t.Errorf("option %d = %q, want %q", i, found.options[i], opt)
		}
	}
}

// TestAcquisitionIsNotRestartRequired guards the hot-reload claim. Both UIs
// label a restart-required setting and the dashboard shows a banner; labelling
// this one would tell the operator to restart for a change the very next R F
// already sees, and NOT labelling a setting that needs one is the worse half of
// the same mistake. It is read live through AutoCookieService.AcquisitionMode,
// so it belongs in neither list.
func TestAcquisitionIsNotRestartRequired(t *testing.T) {
	if restartRequiredKeys["acquisition"] {
		t.Error("acquisition is marked restart-required, but AcquisitionMode is consulted per " +
			"refresh pass — the label would be false")
	}
}

// TestForceRefreshFeedbackNamesTheMechanism is the TUI half of the ladder's
// pre-flight sentence. Under "profile" nothing is launched, so the browser
// sentence names a mechanism the operator switched off.
func TestForceRefreshFeedbackNamesTheMechanism(t *testing.T) {
	for _, tc := range []struct{ mode, want string }{
		{"auto", "Running browser cookie refresh..."},
		{"", "Running browser cookie refresh..."},
		{"profile", "Importing cookies from the browser profile..."},
	} {
		if got := cookieRefreshFeedback(tc.mode); got != tc.want {
			t.Errorf("cookieRefreshFeedback(%q) = %q, want %q", tc.mode, got, tc.want)
		}
	}
}

// utilsModuleVM loads the SHIPPED utils.js into a goja runtime, the way
// settingsVM loads settings.js: strip the `export` keyword and nothing else.
func utilsModuleVM(t *testing.T) *goja.Runtime {
	t.Helper()
	raw, err := webassets.PublicFS.ReadFile("public/modules/utils.js")
	if err != nil {
		t.Fatalf("read the embedded utils.js: %v", err)
	}
	src := strings.ReplaceAll(string(raw), "\r\n", "\n")
	src = regexp.MustCompile(`(?m)^export `).ReplaceAllString(src, "")
	vm := goja.New()
	if _, err := vm.RunString(src); err != nil {
		t.Fatalf("utils.js does not evaluate — the browser would fail the same way: %v", err)
	}
	return vm
}

// TestRefreshPreflightSentenceAgreesAcrossSurfaces is the cross-UI pin, built
// the way TestRecheckToastRendersTheSameSentenceAsTheTUI is: the JS is RUN and
// its answer compared to the Go renderer by exact equality, for every mode.
// Two independent literal tables would both stay green through a reword on
// either side; this fails on one. The two sentences are meant to be identical
// (neither names a per-surface affordance), which is the opposite of the
// rung-3 pair TestRungThreeSentencesDivergeByDesign holds apart.
func TestRefreshPreflightSentenceAgreesAcrossSurfaces(t *testing.T) {
	vm := utilsModuleVM(t)
	fn, ok := goja.AssertFunction(vm.Get("cookieRefreshPreflightToast"))
	if !ok {
		t.Fatal("utils.js no longer exports a callable cookieRefreshPreflightToast")
	}
	for _, mode := range []string{"auto", "profile", ""} {
		v, err := fn(goja.Undefined(), vm.ToValue(mode))
		if err != nil {
			t.Fatalf("mode %q: %v", mode, err)
		}
		if web, tui := v.String(), cookieRefreshFeedback(mode); web != tui {
			t.Errorf("mode %q: the dashboard says %q and the TUI says %q — one gesture, two "+
				"sentences", mode, web, tui)
		}
	}
}

// TestCookieAcquisitionModeReadsTheLiveStore pins WHERE the chord reads from.
// A snapshot taken at App construction would make the sentence stale for the
// whole process, which is the same defect the service's own callback exists to
// avoid — and a nil store must not panic on a chord an operator can press.
func TestCookieAcquisitionModeReadsTheLiveStore(t *testing.T) {
	a := &App{}
	if got := a.cookieAcquisitionMode(); got != "auto" {
		t.Errorf("with no config store the mode is %q, want \"auto\"", got)
	}

	cfg := config.Defaults()
	store := config.NewStore(cfg, "")
	a.configStore = store
	if got := a.cookieAcquisitionMode(); got != "auto" {
		t.Errorf("mode = %q, want \"auto\"", got)
	}
	cfg.Cookies.Acquisition = "profile"
	if got := a.cookieAcquisitionMode(); got != "profile" {
		t.Errorf("after a live edit the mode is %q, want \"profile\" — the read is cached", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 -run 'TestAcquisitionRow|TestAcquisitionIsNot|TestForceRefreshFeedback|TestRefreshPreflightSentence|TestCookieAcquisitionMode' ./internal/tui/`
Expected: compile failure — `undefined: cookieRefreshFeedback`, `a.cookieAcquisitionMode undefined`.

- [ ] **Step 3: Add the settings row**

In `internal/tui/settings.go`, in the `Cookies` section, immediately after the `auto_enabled` row (`:150`):

```go
			{"acquisition", "Cookie source", fieldCycle, []string{"auto", "profile"}, "how a refresh gets cookies: auto = launch a browser when one is available, else read the profile; profile = never launch, read browser_profile_dir read-only (also allows a real browser's profile dir, which auto refuses). Takes effect immediately.", nil},
```

In `loadValues`, after `dpapi_fallback` (`:511`):

```go
	m.values["acquisition"] = cfg.Cookies.Acquisition
```

In `applyValues`, after `DpapiFallback` (`:781`):

```go
	m.cfg.Cookies.Acquisition = m.values["acquisition"]
```

No validation clause is needed: `fieldCycle` can only ever hold one of its options, and `config.Normalize` behind `OnSave` is the backstop for a value that arrives any other way. (Contrast `trusted_proxies` and `browser_path`, which are free text and need the pre-save gate at `:557`.)

- [ ] **Step 4: Make `R F`'s pre-flight line name the mechanism**

In `internal/tui/app_actions.go`, replace the `case "R F":` body's first line (`:205`) and add the two helpers near `recheckCookiesCmd`:

```go
	case "R F":
		if a.OnForceRefreshCookies != nil {
			a.setFeedback(cookieRefreshFeedback(a.cookieAcquisitionMode()))
			refreshFn := a.OnForceRefreshCookies
			return a, safeCmd(func() tea.Msg {
				result, err := refreshFn()
				return cookieForceRefreshResultMsg{Result: result, Err: err}
			})
		}
```

```go
// cookieAcquisitionMode reads cookies.acquisition from the LIVE store, the same
// way AutoCookieService.AcquisitionMode does in cmd/moombox. A value snapshotted
// at construction would leave R F's sentence describing a mode the operator
// changed twenty minutes ago. Empty (no store, or a config built without
// Defaults) is "auto", which is what the service resolves it to anyway.
func (a *App) cookieAcquisitionMode() string {
	if a.configStore == nil {
		return "auto"
	}
	var mode string
	a.configStore.Read(func(c *config.MoomboxConfig) {
		mode = c.Cookies.Acquisition
	})
	return mode
}

// cookieRefreshFeedback is R F's pre-flight line, and it exists as a function
// so the two sentences can be tested without an event loop.
//
// The default sentence is a claim only one of the two acquisition modes can
// support. Under "profile" the pass launches nothing, so naming the browser
// sends the operator looking for a process that will never start — the same
// unearned cause as telling a gated operator to install a browser they already
// have. The dashboard's twin is cookieRefreshPreflightToast in
// web/public/modules/utils.js; TestRefreshPreflightSentenceAgreesAcrossSurfaces
// pins the two by exact equality. Unlike the ladder's rung-3 pair these do NOT
// diverge by surface, because neither names an affordance.
func cookieRefreshFeedback(mode string) string {
	if mode == "profile" {
		return "Importing cookies from the browser profile..."
	}
	return "Running browser cookie refresh..."
}
```

`app_actions.go` already imports `internal/config`.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -count=1 -run 'TestAcquisitionRow|TestAcquisitionIsNot|TestForceRefreshFeedback|TestRefreshPreflightSentence|TestCookieAcquisitionMode' ./internal/tui/`
Expected: PASS.

- [ ] **Step 6: Run the mutations**

1. Delete the `loadValues` line → `TestAcquisitionRowRoundTrips` fails on the read half.
2. Delete the `applyValues` line → `TestAcquisitionRowRoundTrips` fails on the write half.
3. Change the row's `ftype` to `fieldText` (and drop the options) → `TestAcquisitionRowIsACycleWithTwoOptions` fails.
4. Drop `"profile"` from the options slice → the same test fails on the count; swap the two → it fails on the order.
5. Add `"browser"` to the options slice → the same test fails (the ruling is pinned on this surface too).
6. Add `"acquisition": true` to `restartRequiredKeys` → `TestAcquisitionIsNotRestartRequired` fails.
7. Invert `cookieRefreshFeedback`'s condition → `TestForceRefreshFeedbackNamesTheMechanism` fails on three rows AND `TestRefreshPreflightSentenceAgreesAcrossSurfaces` fails on every mode.
8. Reword ONE surface's profile sentence (e.g. drop the ellipsis in `cookieRefreshFeedback` only) → `TestRefreshPreflightSentenceAgreesAcrossSurfaces/` fails on `profile` while both single-surface tests may still pass — which is why the parity test exists.
9. Cache the mode in an `App` field set once → `TestCookieAcquisitionModeReadsTheLiveStore` fails on the live-edit row.

- [ ] **Step 7: Run the full gates**

```bash
go build ./... && go vet ./... && GOOS=linux GOARCH=amd64 go build ./... && gofmt -l internal/ cmd/
go test -count=1 ./...
```

- [ ] **Step 8: Commit**

```bash
git add internal/tui/settings.go internal/tui/app_actions.go internal/tui/settings_acquisition_test.go
git commit -m "$(cat <<'EOF'
feat(tui): cookie acquisition row, and R F's line names the mechanism

A fieldCycle row in the Cookies section with the two modes, loaded and applied
like every other cookie field, and deliberately absent from restartRequiredKeys —
the service reads the mode per pass. R F's pre-flight feedback now says
"Importing cookies from the browser profile..." in profile mode; the dashboard's
cookieRefreshPreflightToast is pinned to the same sentences by exact equality.

Audit G4.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

---

## Task 7: The narrative docs and the deferred-claim close-out

**Files:**
- Modify: `SPEC.md:672` (§ Cookies, the `AutoCookieService` paragraph's last sentence), after `:674` (the `auto_enabled` paragraph)
- Modify: `README.md:536` (the option count), after `:550` (the `auto_enabled` paragraph under **Automatic**)
- Modify: `docs/superpowers/plans/2026-08-25-cookie-subsystem-remediation.md:2009` (the G3/G4 deferred bullet)
- Modify: `.claude/skills/moombox-settings/SKILL.md:30,33,58` (three stale `internal/web/routes/jobs.go` paths → `config_routes.go`; the file is CRLF — keep it CRLF)

**Interfaces:**
- Consumes: everything Tasks 1-6 landed.
- Produces: no code. Its own task because each sentence is only true once BOTH G3 and G4 are in the tree, and a reviewer can reject the prose without rejecting the code.

- [ ] **Step 1: Verify every claim before writing it**

```bash
grep -n "acquisition" internal/config/types.go internal/cookies/autocookies.go cmd/moombox/services.go internal/tui/settings.go web/public/modules/settings.js web/public/modules/utils.js
grep -rn "validateBrowserProfileDir\b" internal/ cmd/ docs/    # must return NOTHING (Task 4 renamed dpapi.go:12 and the periodic test's comment too)
grep -rn "validateBrowserProfileDirForLaunch" internal/ docs/
grep -rn "AcquisitionBrowser\|\"browser\" *[,)]" internal/config internal/cookies/autocookies*.go internal/tui/settings.go   # no third value anywhere
```

- [ ] **Step 2: Update `SPEC.md` § Cookies**

Replace the last sentence of the `AutoCookieService` paragraph at `:672` ("It manages a dedicated profile directory and refuses one that points inside a real installed browser's profile tree."):

```markdown
It manages a dedicated profile directory and refuses one that points inside a real installed browser's profile tree — for LAUNCHING. `cookies.acquisition` (`auto` | `profile`) selects how a refresh acquires credentials; `"profile"` never launches, reads the configured profile directory read-only, and is the explicit opt-in that lets that read proceed against a real browser's profile. The launch guard itself is never lifted, in any mode.
```

Add a paragraph after `:674`'s `auto_enabled` paragraph:

```markdown
**What `cookies.acquisition` does.** It picks the PATH a refresh takes; `auto_enabled` picks whether a browser may run at all. The two compose and neither replaces the other. `"auto"` is the default and is the behaviour that shipped before the setting existed: a resolvable browser launches, a host with none imports. `"profile"` forces the browser-free import even on a desktop with a browser installed, which is the only route to reading a real signed-in profile on Windows; under it the flag's timer and its one automatic recovery attempt import instead of launching, and the timer's import stays behind `automaticImportGuard` like every automatic import. Two values by ruling — the audit's `"browser"` behaved exactly like `"auto"` and was dropped. It is read live (`AutoCookieService.AcquisitionMode`) and is not restart-required. `StartSetup` never consults it — the interactive login is acquisition, and gating it would leave a fresh install in `"profile"` mode unable to create the profile it is told to read.
```

- [ ] **Step 3: Update `README.md`**

`:536` says "Three options:" over FOUR `###` sections (Automatic, Browser profile import, Paste or upload, Manual) — the count is already stale. Change it to "Four options:". The recipe below is a variant of **Automatic** (a `####` under it), not a fifth option. After the `auto_enabled` paragraph at `:550`, add:

````markdown
#### Reading your real browser profile instead

If you would rather Moombox read the browser profile you actually sign in with
than drive a managed one of its own in the background, set:

```toml
[cookies]
acquisition = "profile"
browser_profile_dir = "C:/Users/you/AppData/Roaming/Mozilla/Firefox/Profiles/xxxxx.default-release"
```

`acquisition` has two values — `auto` (the default: launch a browser when one is
available, otherwise read the profile directory) and `profile` (never launch;
read the profile directory read-only). `profile` is the only mode that will
point at a real browser's profile directory: `auto` refuses those paths, because
a config that could aim a headless browser at your signed-in profile could also
export it. The read itself launches nothing and never writes into your profile
— it copies `cookies.sqlite` and its `-wal` sidecar to a temporary directory
and reads the copy — but the cookies it finds are written to `cookies.txt`,
which is why it is opt-in. If a refresh reports that `cookies.sqlite` is locked,
close the browser and press the button again. The setting takes effect
immediately; no restart.
````

- [ ] **Step 4: Close the deferred claim**

In `docs/superpowers/plans/2026-08-25-cookie-subsystem-remediation.md`, replace the G3/G4 bullet at `:2009`:

```markdown
- **G4** (explicit `acquisition` mode) and **G3** (splitting the launch guard from the read-only import). **BUILT — Arc 12c.** `cookies.acquisition` (`auto` | `profile`, default `auto`, no migration; the audit's `browser` dropped by ruling as observationally identical to `auto`) reaches `AutoCookieService` as the live-read `AcquisitionMode` predicate and changes one refresh decision, `importedFromProfile`; the three sites that inferred that decision from the host follow it — `decideStartupSeed`'s browser short-circuit, the periodic tick's browser-free test (so `automaticImportGuard` still governs every automatic import), and the two read-only consultations of the launch guard. `validateBrowserProfileDir` is now `validateBrowserProfileDirForLaunch` and holds on all four subprocess sites in every mode; the read-only sites go through `readOnlyProfileDirErr`, which lifts it only under `"profile"` and refuses with the new `ErrProfileDirNotOptedIn` (422, excluded from rung 3) otherwise. `dangerousProfilePathSubstrings` unchanged, per G3's own warning about Linux desktop users. Both UIs carry the control; neither labels it restart-required; both render the same pre-flight sentence, pinned by exact equality. Docs: `data-and-storage.md` (field table, § Auto-Cookie Service, the `R F` ladder, the automatic-import rule, the guard sentence), `security.md` § Browser Profile Directory Guard (new), `operations.md` § Cookies in a container, `user-interfaces.md` (route + 422 rows), `SPEC.md` § Cookies, README.
```

- [ ] **Step 5: Correct the settings skill's stale paths**

In `.claude/skills/moombox-settings/SKILL.md`, lines 30, 33 and 58 name `internal/web/routes/jobs.go` for `validateConfigUpdates`, `applyConfigUpdates` and `ConfigRoutesCallbacks`; all three live in `internal/web/routes/config_routes.go`. Replace the path in those three lines only. The file is CRLF: edit in place with an editor that preserves line endings, and confirm with `git diff --stat` that only three lines changed.

- [ ] **Step 6: Run the full gates**

```bash
go build ./... && go vet ./... && GOOS=linux GOARCH=amd64 go build ./... && gofmt -l internal/ cmd/
go test -count=1 ./...
```

(No code changed; the gates run anyway, because a doc-only task that skips them is how a broken tree gets committed under a green banner.)

- [ ] **Step 7: Commit**

```bash
git add SPEC.md README.md docs/superpowers/plans/2026-08-25-cookie-subsystem-remediation.md .claude/skills/moombox-settings/SKILL.md
git commit -m "$(cat <<'EOF'
docs: acquisition mode and the split guard, in the narrative docs

SPEC.md § Cookies gains the setting and the guard's new boundary; README gains
the profile-mode recipe with the honest opt-in reasoning (and its option count
catches up with its four headings); the remediation plan's G3/G4 bullet goes
from deferred to built with what actually shipped; the settings skill stops
pointing at jobs.go for the config-route functions.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

---

## Self-Review

### 1. Spec coverage

| Spec requirement | Task |
|---|---|
| R1 — one setting, two values, default `auto`, empty/absent → `auto`, validation replaces (`fail()` + `!reportOnly`, as `network_access`), API rejects with a field error | Tasks 1, 2 (`TestCookiesAcquisitionNormalises`, `TestAcquisitionModeValidation`) |
| R1 — `StartSetup` unaffected | Task 3 (`TestStartSetupIgnoresAcquisitionMode`) |
| R2 — `AcquisitionMode func() string` beside `ConfiguredBrowserOverride`, wired to the live config, consulted per pass, `importedFromProfile` decision, `resolvedBrowser` untouched, nil = `auto` | Task 3 (`TestAcquisitionModeSelectsTheRefreshPath`, `TestAcquisitionModeIsReadPerPass`, `TestAcquisitionModeNilDefaultsToAuto`) |
| R3 — rename, kept on every subprocess site, read-only sites gated on the opt-in, own refusal message, `decideStartupSeed`'s second short-circuit, the `gateApplies` comment, `dangerousProfilePathSubstrings` unchanged; plus the periodic tick (same class, added by review) | Task 4 (`TestLaunchGuardHoldsEveryLaunchSiteInEveryMode`, `TestReadOnlyImportIsGatedOnTheOptIn`, `TestRealProfileTreeReadsOnlyOnTheOptIn`, `TestReadOnlyRefusalDoesNotClaimALaunch`, `TestStartupSeedFollowsTheOptIn`, `TestPeriodicTickInProfileModeObeysTheImportGuard`, `TestOptInRefusalAnswers422`) |
| R4 — web select, TUI row, PUT parse+validate, `R F` ladder and the Settings-page button respect the mode (the button calls `autoCookieRefresh`, so the toast covers it) | Tasks 2, 5, 6 |
| R5 — `data-and-storage.md`, `operations.md`, `security.md`, `SPEC.md`, the remediation bullet, README; `user-interfaces.md` added by review (its 422 table enumerates sentinels) | Tasks 3, 4, 7 |
| Non-goals — no `auto_enabled` change, no wizard change, no `dpapi_fallback` change | nothing in any task touches them |
| Invariant — `migrateOldFormat` untouched | Task 1 (`TestCookiesAcquisitionNeedsNoMigration`) |
| Invariant — guard never leaves a launch site | Task 4 (`TestLaunchGuardHoldsEveryLaunchSiteInEveryMode`, both modes) |
| Invariant — every automatic import behind `automaticImportGuard` | Task 4 (`TestStartupSeedFollowsTheOptIn`, `TestPeriodicTickInProfileModeObeysTheImportGuard`) |
| Invariant — every assertion mutation-checked | each task's **Mutations to run** |

### 2. Placeholder scan

No `TBD`, no "similar to Task N", no conditional left to the executor. The two conditionals the previous draft carried are settled: the web test runs `utils.js` (Task 5, with the reason), and README's count is "Four options:" (Task 7, with the reason).

### 3. Type consistency

`AcquisitionAuto`/`AcquisitionProfile` (Task 3) are used verbatim in Task 4. `resolvedAcquisition()` is defined in Task 3 and consumed in Task 4 at four sites. `readOnlyProfileDirErr()` and `ErrProfileDirNotOptedIn` are defined, consumed and routed within Task 4. `cookieRefreshPreflightToast` (Task 5, JS) is consumed by Task 6's parity test; `cookieRefreshFeedback` and `cookieAcquisitionMode` are defined and consumed within Task 6. The wire key is `cookies.acquisition` in Tasks 2, 5, 6; the element id `cfg-cookies-acquisition` in Task 5 only; the TUI field key `acquisition` in Task 6 only. The two pre-flight sentences are byte-identical in Tasks 5 and 6 and pinned by `TestRefreshPreflightSentenceAgreesAcrossSurfaces`.

### 4. Where the code shaped the plan

1. **`validateOrNormalize` has no logger** (`config.go:427`): `fail()` records only when `reportOnly`; Load normalises silently. The plan mirrors `network.network_access` (`:442-447`) exactly. No "log once".
2. **`validateConfigUpdates`/`applyConfigUpdates` live in `config_routes.go` (`:106`, `:399`)**, not `jobs.go`; Task 7 corrects the skill.
3. **`decideStartupSeed` and the periodic tick both infer the import path from the host** (`autocookies_profile.go:876`, `autocookies.go:3000`). The audit's G4 names the first; the second is the same defect at the other automatic caller, and without it `"profile"` + `auto_enabled = true` would re-read a real profile over a live `cookies.txt` on a schedule. Task 4 corrects both and pins both.
4. **`TestRungThreeAgreesAcrossBothSurfaces` fails on any sentinel the handler branches on that its map does not list** — Task 4 adds the row and a direct 422 test, since that test cannot see a deleted case.
5. **`app.js` cannot run in goja** (module-level `document.addEventListener`); the sentence lives in `utils.js`, the codebase's established seam for exactly this.
