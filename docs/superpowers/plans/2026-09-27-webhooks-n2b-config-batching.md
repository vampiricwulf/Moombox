# Arc N2b — config, mentions, dashboard link, batching, both UIs: Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give notification targets the three per-target keys the owner ruled for (`enabled`, `mention`, `mention_events`) plus the install-wide `network.public_url`, make the delivery core act on them (skip disabled targets, attach `content` + `allowed_mentions`, rewrite a job embed's title link to the dashboard), coalesce burst events into one message of up to ten embeds, and expose all of it in both Settings editors and the SPA's `#job=<id>` deep link.

**Architecture:** Four config keys enter through `internal/config` (struct → `Defaults()` → `validateOrNormalize`), reach the manager the way every other notification setting already does (`buildTargets` at `NewManager`/`Reload`), and are edited from both UIs through the existing `PUT /api/config` merge. Two behaviours are added to the delivery core and both are read from the target record the manager already keeps: a mention decision (`mentionFor`) evaluated per send, and a public-URL rewrite (`JobDeepLink`) applied to any send carrying a `JobID`. Batching is a new, self-contained coalescing stage (`internal/notifications/batch.go`) placed in front of Arc N1's per-target FIFO queue: it owns the 5 s window, the ten-embed split and the per-message tier/mention rules, and it touches `manager.go` at **seven** places — stated once here and checked by Task 9: one struct field, one new `enqueue` adapter method, and five one-line call sites (`buildTargets`, `Send`, `Reload`, `Wait`, `BeginShutdown`). The SPA gains a hash consumer in `app.js`; nothing on the server routes it.

**Tech Stack:** Go 1.27, `BurntSushi/toml` (three-way present/empty/absent encoding via `*[]string`), chi/v5 REST, vanilla-JS dashboard embedded via `go:embed` (Shoelace v2.16), Bubble Tea TUI (`charm.land/bubbles/v2`), `node:test` + jsdom for the frontend suites.

**Spec:** `docs/superpowers/specs/2026-09-27-discord-webhooks-design.md` — §0 rulings (Q5 mentions, Q6 dashboard link, Q3 batching, the "empty filter means ALL events / mute with `enabled = false`" ruling), §3 Arc N2b, §5 order. Committed at `1d2df1d4`. Supporting audit (gitignored): `.superpowers/sdd/2026-09-26-webhooks/audit.md` §3 Batching + Content quality, §4 seams.

---

## Arc N1 is not merged yet

This plan was written against `main` at `1d2df1d4` with Arc N1 **unmerged**. It consumes N1 strictly by the names the spec gives it and never by line number:

| N1 name (spec §1) | How N2b uses it |
|---|---|
| `SendOptions.Mention` (string) / `SendOptions.MentionAllowed` (`*AllowedMentions`, the object `struct{ Parse []string; Roles, Users []string }`) | Task 2 fills both. N1 owns the `content` + `allowed_mentions` payload shape; N2b never writes payload JSON. |
| `SendOptions.Tier`, `TierLow` | Task 7 computes a batched message's tier. |
| `SendOptions.JobID` | Tasks 2 and 7 key the deep-link rewrite and the per-job `auth` batching rule on it. |
| `SendOptions.Author` (`*Author` with `Name`, `IconURL`, `URL`) | Task 2 moves the platform link onto it. |
| The per-target FIFO queue, one queue item per message | Task 7's coalescing stage sits **in front of** it and hands it whole messages. |
| `BeginShutdown()`, `Wait()` | Task 7 flushes open windows from both. |
| `effectiveTier(opts)` — N1's Event→Tier derivation applied when `SendOptions.Tier` is unset | Task 7 calls it once, in `batchIsLowTier`. |

**Three adaptation points to re-verify at merge**, each a single expression:

1. **`MentionAllowed`'s concrete type is already pinned.** The controller ruled it (2026-09-27, `progress.md`): `type AllowedMentions struct{ Parse []string; Roles, Users []string }` with `MentionAllowed *AllowedMentions`. If the merged N1 chose a `bool`, that is the N1 defect — fix N1 to the pinned shape rather than narrowing N2b.
2. **`effectiveTier`'s exact name.** One call in `batchIsLowTier` (Task 7).
3. **The FIFO enqueue expression.** Task 7 adds `func (t notificationTarget) enqueue(msg []embedSpec)` to `manager.go`, which builds one queue item from 1..10 embeds. It is the **only** place in N2b that names N1's queue-item type, and the batcher never sees that type at all — so whatever N1's constructor is called, this adaptation is one expression inside one method.

Every **config, route, `settings.js`, `app.js`, `index.html` and TUI line number in this plan was verified at `1d2df1d4`** and none of those files is touched by N1. Re-verify only `internal/notifications/{manager.go,discord.go}` anchors, which this plan names by symbol only.

**N2a ordering note.** `config.defaultMentionEvents` lists `sidecar_down`, a key **N2a** adds. Before N2a merges, `buildTargets` resolves the default list for every target that has a `mention` and no explicit `mention_events`, so each such target logs one `notification target filters on unknown event` Warn for `sidecar_down` at every config load. That is the accepted cost (controller ruling 7, same wave) and it disappears when N2a lands with no edit here — do not special-case the key. Neither UI can offer it in the meantime, because both derive their chips from the live vocabulary. Likewise the deep-link rewrite only fires for sends that carry an `Author`, which N2a's builders give every job send — before N2a it applies to whichever sites already set one.

---

## Global Constraints

Every task's requirements implicitly include this section.

### From the spec (§3 Arc N2b Constraints)

- **No producer changes.** Nothing under `internal/worker/`, `cmd/moombox/monitor_callbacks.go`, `cmd/moombox/main.go`, `cmd/moombox/addvideo.go` or `internal/web/routes/jobs.go` is edited — those are N2a's. If a test needs a producer's behaviour, use a fake target, not a producer.
- **The config-file-only key class is untouched.** `downloader.progress_interval_ms` and `cookies.dpapi_profile_dir` keep their no-UI status; nothing is added to or removed from that class.
- **`PUT /api/config` merges per key as today.** An absent section stays as stored. No key added here becomes mandatory in a payload.
- **The restart-required list stays at 16.** All four new keys hot-reload. `RESTART_REQUIRED_FIELDS` (`web/public/modules/settings.js`) and `restartRequiredKeys` (`internal/tui/settings.go`) are not edited; `TestRestartRequiredListsAgree` must stay green unchanged.
- **No `mode` key.** `mode = "separate" | "edit"` is Arc N3's. Batching is written as if every target is separate-mode, with the one-line gate N3 will add named in Task 7's comments.

### Standing project rules

- **Go 1.27, no CGo.** Pure-Go dependencies only; no new module requirements (`go mod tidy` must leave `go.mod`/`go.sum` unchanged — the gate below checks it).
- **Three builds must pass:** host (windows/amd64), `GOOS=linux GOARCH=amd64`, `GOOS=linux GOARCH=arm64`.
- **The logger is the anonymous interface**, repeated in place:
  ```go
  logger interface {
      Debug(msg string, args ...any)
      Info(msg string, args ...any)
      Warn(msg string, args ...any)
      Error(msg string, args ...any)
  }
  ```
  Never extracted to a named interface. The new `batcher` (Task 7) carries its own copy.
- **Every goroutine gets an inline `defer func() { if r := recover(); r != nil { … } }()`.** This arc adds exactly one goroutine-bearing construct: the `time.AfterFunc` callback in `batch.go`. It carries the recover.
- **The DB layer is untouched.** No schema change, no new column, no change to write cadence or subscriber fan-out.
- **The TUI import fence.** `internal/tui` must not import `internal/web`. It may import `internal/config` and `internal/notifications` (it already does both).
- **LF line endings.** Every file this plan touches is LF at `1d2df1d4` (verified). Keep it that way — do not let an editor introduce CRLF.
- **The citation gate.** `internal/docs` checks that every backticked symbol/path in `docs/spec/*.md`, `SPEC.md`, `CLAUDE.md`, `README.md` and the seven skills resolves to real code. Any symbol a docs task names must exist with that exact spelling, and `TestSpecDocAbsenceClaimsHold` means an "X does not exist" sentence must stay true.
- **No secret ever reaches a log or an error.** Webhook URLs go through `redactURLForLog`. A `mention` is not a secret, but a webhook URL beside it is — keep the existing redaction on every new log line in `buildTargets`.
- **Write/Edit only.** No `sed -i`, no shell heredoc rewriting a tracked file, no `python3 -` (it hangs under the uv shim here).
- **Implementers never run the full suite.** `go test ./...` is forbidden in this arc. Run only the named packages, always with `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp`.
- **Node tests run narrowly**: `timeout 300 node --test web/tests/<file>.test.mjs`. Leave no node process behind.
- **`web/tests/README.md` counts are recounted from live runs**, both with and without jsdom — never arithmetic on the old numbers. At `1d2df1d4` the README claims `tests 291 / pass 131 / skipped 160`; confirm that baseline before changing it.
- **One commit per task, always with an explicit pathspec** (`git commit -m … -- <files>`). A bare `git commit` has swept another task's staged files before.
- **Every commit message ends with these two lines, verbatim:**
  ```
  Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
  ```

### Gates — run at the end of every task, before the commit

```bash
gofmt -l ./cmd ./internal ./tools ./web          # must print nothing
go vet ./...
go mod tidy -diff                                 # must print nothing
go build ./...
GOOS=linux GOARCH=amd64 go build ./...
GOOS=linux GOARCH=arm64 go build ./...
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 <the task's packages>
```

`staticcheck ./...` (pinned `honnef.co/go/tools/cmd/staticcheck@2026.2.1`) runs in Task 9 only — it is slow and nothing earlier can regress it without also failing `go vet`.

### Settings-skill checklist coverage (`.claude/skills/moombox-settings/SKILL.md`)

| Step | `network.public_url` | per-target `enabled` / `mention` / `mention_events` |
|---|---|---|
| 1 Config struct | Task 1 | Task 1 |
| 2 Default | Task 1 (`""` = unset, so no `Defaults()` entry is needed — and because `loadFromFile` decodes over `Defaults()`, that absence is also what makes step 9's "no migration" true) | Task 1 (`enabled` nil ⇒ true via `IsEnabled()`, mirroring `ChannelConfig`) |
| 3 `validateOrNormalize` | Task 1 | Task 1 |
| 4 `validateConfigUpdates` | Task 3 | Task 3 |
| 5 `applyConfigUpdates` | Task 3 | Task 3 |
| 6 Web UI | Task 4 | Task 4 |
| 7 TUI | Task 5 | Task 5 |
| 8 Hot-reload | Task 3 (`OnNotificationsChange` extended to fire on a `public_url` change) | Task 3 (already fires on a `notifications` key) |
| 8b config-file-only | n/a | n/a |
| 9 Migration | none — `loadFromFile` decodes over `Defaults()`, so an older config reads as unset | same |

---

## Task 1: The four config keys, their defaults and their validators

**Files:**
- Modify: `internal/config/types.go:80` (end of `NetworkConfig`, after `TrustedProxies`) and `internal/config/types.go:446-453` (`NotificationConfig`)
- Create: `internal/config/notifications.go`
- Modify: `internal/config/config.go:577` (`validateOrNormalize`, between the `trusted_proxies` block that ends there and the `client_token_ttl_days` block at `:578`) and `internal/config/config.go:966-971` (the channel loop's closing brace at `:971`, before `return errs` at `:973`)
- Modify: `config.example.toml:50` (end of the `[network]` block, before `# ─── Paths ───` at `:52`) and `config.example.toml:302-322` (the `[[notifications]]` comment block, whose last line is the `tags = ["important"]` example at `:322`)
- Modify: `docs/spec/data-and-storage.md:480-490` (`#### [network]` table) and `docs/spec/data-and-storage.md:613-619` (`#### [[notifications]]` table)
- Modify: `.claude/skills/moombox-settings/SKILL.md` (the "Restart-Required Fields" paragraph stays at 16; add the two new hot-reload notes where the checklist mentions notification fields)
- Test: `internal/config/notifications_test.go` (new)

**Interfaces:**

Produces, in `internal/config/types.go`:
```go
// NetworkConfig
	// PublicURL is the externally reachable base URL of this dashboard, e.g.
	// "https://moombox.example.com". Empty means unset. …
	PublicURL string `toml:"public_url,omitempty" json:"public_url,omitempty"`
```
```go
type NotificationConfig struct {
	URL           string    `toml:"url,omitempty" json:"url,omitempty"`
	Enabled       *bool     `toml:"enabled,omitempty" json:"enabled,omitempty"`
	Events        []string  `toml:"events,omitempty" json:"events,omitempty"`
	Mention       string    `toml:"mention,omitempty" json:"mention,omitempty"`
	MentionEvents *[]string `toml:"mention_events,omitempty" json:"mention_events,omitempty"`
}
```

Produces, in the new `internal/config/notifications.go`:
```go
// MentionForm classifies a validated per-target mention token.
type MentionForm int

const (
	MentionNone MentionForm = iota
	MentionRole
	MentionUser
	MentionEveryone
	MentionHere
)

// IsEnabled reports whether this notification target delivers. Absent means
// enabled, exactly as ChannelConfig.IsEnabled treats an absent channel flag.
func (n *NotificationConfig) IsEnabled() bool

// ParseMention validates a per-target mention token and reports its form.
func ParseMention(raw string) (canonical string, form MentionForm, id string, err error)

// DefaultMentionEvents returns a fresh copy of the owner's default mention
// filter (2026-09-27 ruling Q5).
func DefaultMentionEvents() []string

// ResolveMentionEvents returns the effective mention filter for this target.
func (n *NotificationConfig) ResolveMentionEvents() []string

// ValidatePublicURL canonicalises a network.public_url value.
func ValidatePublicURL(raw string) (canonical string, err error)
```

**The three-way `mention_events` encoding — why `*[]string`.** The ruling needs three distinct states: absent (⇒ the default list), explicitly empty (⇒ never mention), and an explicit list. A plain `[]string` with `omitempty` cannot express the middle one: an empty non-nil slice is *omitted* by both the TOML and the JSON encoder, so "never" would silently reload as "the defaults". `*[]string` survives because `BurntSushi/toml`'s `isEmpty` returns `rv.IsNil()` for a pointer, so a pointer to an empty slice is written as `mention_events = []`, and `encoding/json` does the same. This was verified against `github.com/BurntSushi/toml v1.6.0` before this plan was written: a round trip of three targets (absent / `&[]string{}` / `&[]string{"error","auth"}`) came back `nil=true`, `nil=false len=0`, `nil=false len=2` through both encoders.

**Vocabulary validation is deliberately NOT in `internal/config`.** `internal/notifications` imports `internal/config`, so config cannot reach `notifications.KnownEvents` without an import cycle. This is exactly why today's `Events` filter has no vocabulary check in `validateOrNormalize` either. `validateOrNormalize` therefore validates the **shape** of `mention_events` (non-empty trimmed strings, deduped) and the alias-aware vocabulary check stays at the two places that can see the registry: `buildTargets`'s Warn (Task 2) and the web save's strip (Task 3). The TUI cannot produce an unknown key because it derives its rows from `notifications.EventGroups`.

- [ ] **Step 1: Write the failing tests**

Create `internal/config/notifications_test.go`:

```go
package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestValidatePublicURL is network.public_url's whole contract in one table.
// The key is read at SEND time by the notification manager and pasted into
// every job embed's title link, so a value that parses but is not a browsable
// absolute origin produces a dead link in someone's Discord channel rather
// than an error anyone sees. Each rejection below is a link that would have
// been dead: a relative path has no host to resolve against, a query or a
// fragment collides with the "#job=<id>" the manager appends, and embedded
// credentials would be republished to every reader of the channel.
func TestValidatePublicURL(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      string
		want    string
		wantErr bool
		why     string
	}{
		{"unset", "", "", false, "empty is the documented 'no dashboard link' case, not an error"},
		{"whitespace only", "   ", "", false, "a blank field from either editor means unset"},
		{"https host", "https://moombox.example.com", "https://moombox.example.com", false, "the ordinary case"},
		{"http host", "http://192.168.1.10:774", "http://192.168.1.10:774", false, "a LAN install over plain http is legitimate"},
		{"trailing slash trimmed", "https://x.example/", "https://x.example", false,
			"the manager appends \"/#job=\", so a stored trailing slash would produce \"//#job=\""},
		{"sub-path trailing slash trimmed", "https://x.example/moombox/", "https://x.example/moombox", false,
			"a reverse-proxy sub-path is legitimate; only the trailing slash goes"},
		{"surrounding space trimmed", "  https://x.example  ", "https://x.example", false, "a paste carries spaces"},
		{"no scheme", "moombox.example.com", "", true, "with no scheme there is no origin to link to"},
		{"relative", "/moombox", "", true, "a path alone cannot be the base of an absolute embed link"},
		{"ftp scheme", "ftp://x.example", "", true, "only http and https render as links in a Discord embed"},
		{"no host", "https://", "", true, "a scheme with no authority resolves to nothing"},
		{"query", "https://x.example/?a=b", "", true, "a query would sit before the #job fragment and change the request"},
		{"fragment", "https://x.example/#top", "", true, "the fragment slot is what the deep link uses"},
		{"userinfo", "https://user:pw@x.example", "", true,
			"credentials in the base URL would be republished into every embed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidatePublicURL(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidatePublicURL(%q) err = %v, wantErr %v — %s", tc.in, err, tc.wantErr, tc.why)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("ValidatePublicURL(%q) = %q, want %q — %s", tc.in, got, tc.want, tc.why)
			}
		})
	}
}

// TestPublicURLValidateAndNormalizeAgree pins both arms of the one
// implementation (DECISIONS #9): Validate REPORTS without mutating, Normalize
// REPLACES. A bad public_url normalises to "" (unset) rather than to some
// other URL — there is no sensible default host to invent.
func TestPublicURLValidateAndNormalizeAgree(t *testing.T) {
	reported := Defaults()
	reported.Network.PublicURL = "ftp://nope"
	errs := Validate(reported)
	mentioned := false
	for _, err := range errs {
		if strings.Contains(err.Error(), "network.public_url") {
			mentioned = true
		}
	}
	if !mentioned {
		t.Errorf("Validate did not mention network.public_url for %q (errs: %v)", reported.Network.PublicURL, errs)
	}
	if reported.Network.PublicURL != "ftp://nope" {
		t.Errorf("Validate mutated the config: public_url = %q", reported.Network.PublicURL)
	}

	normalised := Defaults()
	normalised.Network.PublicURL = "ftp://nope"
	Normalize(normalised)
	if normalised.Network.PublicURL != "" {
		t.Errorf("Normalize left public_url = %q, want \"\" — an unusable base URL must become unset, "+
			"not stay in the config where the manager would paste it into embeds", normalised.Network.PublicURL)
	}

	// A canonicalisable value is rewritten, not rejected.
	tidy := Defaults()
	tidy.Network.PublicURL = "https://x.example/"
	if errs := Validate(tidy); len(errs) != 0 {
		t.Errorf("Validate reported %v for a merely untidy public_url — a trailing slash is normalisable, not invalid", errs)
	}
	Normalize(tidy)
	if tidy.Network.PublicURL != "https://x.example" {
		t.Errorf("Normalize left public_url = %q, want the trailing slash trimmed", tidy.Network.PublicURL)
	}
}

// TestParseMention pins the four forms the owner ruled for plus the one
// legacy spelling Discord clients still emit. The canonical form is what gets
// stored, so <@!123> and <@123> never round-trip as two different targets.
func TestParseMention(t *testing.T) {
	for _, tc := range []struct {
		name      string
		in        string
		canonical string
		form      MentionForm
		id        string
		wantErr   bool
	}{
		{"unset", "", "", MentionNone, "", false},
		{"role", "<@&123456789012345678>", "<@&123456789012345678>", MentionRole, "123456789012345678", false},
		{"user", "<@123456789012345678>", "<@123456789012345678>", MentionUser, "123456789012345678", false},
		{"legacy nickname user", "<@!123456789012345678>", "<@123456789012345678>", MentionUser, "123456789012345678", false},
		{"everyone", "@everyone", "@everyone", MentionEveryone, "", false},
		{"here", "@here", "@here", MentionHere, "", false},
		{"spaces trimmed", "  @here  ", "@here", MentionHere, "", false},
		{"bare id", "123456789012345678", "", MentionNone, "", true},
		{"plain name", "@ops-team", "", MentionNone, "", true},
		{"non-numeric role", "<@&abc>", "", MentionNone, "", true},
		{"unclosed", "<@&123", "", MentionNone, "", true},
		{"id too long", "<@&123456789012345678901>", "", MentionNone, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			canonical, form, id, err := ParseMention(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ParseMention(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if canonical != tc.canonical || form != tc.form || id != tc.id {
				t.Errorf("ParseMention(%q) = (%q, %v, %q), want (%q, %v, %q)",
					tc.in, canonical, form, id, tc.canonical, tc.form, tc.id)
			}
		})
	}
}

// TestResolveMentionEvents is the three-way rule the *[]string encoding
// exists for. Getting the middle row wrong is the bug that matters: an
// operator who unticks every mention chip would start receiving the DEFAULT
// six pings instead of none.
func TestResolveMentionEvents(t *testing.T) {
	empty := []string{}
	explicit := []string{"finished"}
	for _, tc := range []struct {
		name string
		n    NotificationConfig
		want []string
		why  string
	}{
		{"no mention", NotificationConfig{}, nil, "with nothing to ping, the filter is irrelevant"},
		{"no mention but a list", NotificationConfig{MentionEvents: &explicit}, nil,
			"a filter without a mention still pings nobody"},
		{"mention, absent list", NotificationConfig{Mention: "@here"}, DefaultMentionEvents(),
			"the ruling's default list applies when the key was never written"},
		{"mention, explicit empty", NotificationConfig{Mention: "@here", MentionEvents: &empty}, []string{},
			"an explicit empty list means NEVER — it must not fall back to the defaults"},
		{"mention, explicit list", NotificationConfig{Mention: "@here", MentionEvents: &explicit}, explicit,
			"an explicit list is used as written"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.n.ResolveMentionEvents()
			if !slices.Equal(got, tc.want) {
				t.Errorf("ResolveMentionEvents() = %v, want %v — %s", got, tc.want, tc.why)
			}
		})
	}
}

// TestDefaultMentionEventsIsTheRuling pins the exact list from the owner's
// 2026-09-27 Q5 answer, and that callers get a copy they cannot corrupt for
// every other target in the process.
func TestDefaultMentionEventsIsTheRuling(t *testing.T) {
	want := []string{"error", "auth", "disk_critical", "update_failed", "crash_recovered", "sidecar_down"}
	if got := DefaultMentionEvents(); !slices.Equal(got, want) {
		t.Errorf("DefaultMentionEvents() = %v, want %v (owner ruling Q5, 2026-09-27)", got, want)
	}
	mine := DefaultMentionEvents()
	mine[0] = "clobbered"
	if DefaultMentionEvents()[0] != "error" {
		t.Error("DefaultMentionEvents returned the package slice — one caller's edit would change every target's defaults")
	}
}

// TestNotificationIsEnabled mirrors ChannelConfig.IsEnabled: absent means on,
// so every config written before this key keeps delivering.
func TestNotificationIsEnabled(t *testing.T) {
	var absent NotificationConfig
	if !absent.IsEnabled() {
		t.Error("a target with no enabled key must deliver — otherwise the key's arrival silences every existing install")
	}
	off := NotificationConfig{Enabled: boolPtr(false)}
	if off.IsEnabled() {
		t.Error("enabled = false must not deliver")
	}
	on := NotificationConfig{Enabled: boolPtr(true)}
	if !on.IsEnabled() {
		t.Error("enabled = true must deliver")
	}
}

// TestMentionEventsShapeNormalises covers what validateOrNormalize does to a
// hand-edited list: blanks and duplicates go, the surviving order is kept, and
// an explicitly empty list stays explicitly empty (it means "never"). The
// VOCABULARY is deliberately not checked here — internal/notifications imports
// internal/config, so config cannot see KnownEvents without an import cycle;
// the alias-aware check lives in buildTargets and in the web save path.
func TestMentionEventsShapeNormalises(t *testing.T) {
	messy := []string{"error", "", "  auth  ", "error"}
	cfg := Defaults()
	cfg.Notifications = []NotificationConfig{{
		URL:           "discord://1/abc",
		Mention:       "@here",
		MentionEvents: &messy,
	}}
	Normalize(cfg)
	got := cfg.Notifications[0].MentionEvents
	if got == nil {
		t.Fatal("Normalize dropped the pointer — an explicit list became the default list")
	}
	if !slices.Equal(*got, []string{"error", "auth"}) {
		t.Errorf("mention_events normalised to %v, want [error auth]", *got)
	}

	empty := []string{}
	keep := Defaults()
	keep.Notifications = []NotificationConfig{{URL: "discord://1/abc", Mention: "@here", MentionEvents: &empty}}
	Normalize(keep)
	if keep.Notifications[0].MentionEvents == nil {
		t.Error("Normalize turned an explicit empty mention_events into absent — \"never ping\" became \"ping on the default six\"")
	}
}

// TestNotificationMentionValidateAndNormalize pins the pair for the mention
// token itself: Validate names the field, Normalize clears an unusable value
// (a stored token Discord cannot resolve renders as literal text in the
// message and pings nobody, which is worse than no content line at all).
func TestNotificationMentionValidateAndNormalize(t *testing.T) {
	bad := Defaults()
	bad.Notifications = []NotificationConfig{{URL: "discord://1/abc", Mention: "@ops-team"}}
	errs := Validate(bad)
	mentioned := false
	for _, err := range errs {
		if strings.Contains(err.Error(), "notifications[0].mention") {
			mentioned = true
		}
	}
	if !mentioned {
		t.Errorf("Validate did not name notifications[0].mention for %q (errs: %v)", "@ops-team", errs)
	}
	if bad.Notifications[0].Mention != "@ops-team" {
		t.Error("Validate mutated the config")
	}

	Normalize(bad)
	if bad.Notifications[0].Mention != "" {
		t.Errorf("Normalize left mention = %q, want cleared", bad.Notifications[0].Mention)
	}

	tidy := Defaults()
	tidy.Notifications = []NotificationConfig{{URL: "discord://1/abc", Mention: " <@!123456789012345678> "}}
	Normalize(tidy)
	if tidy.Notifications[0].Mention != "<@123456789012345678>" {
		t.Errorf("Normalize left mention = %q, want the canonical <@id> form", tidy.Notifications[0].Mention)
	}
}

// TestNotificationKeysRoundTripThroughSave is the end-to-end encoding proof:
// Save runs Validate and refuses a failing config, and the three-way
// mention_events state has to survive a real file. The middle target is the
// one that matters — with a plain []string it would come back as the default
// list.
func TestNotificationKeysRoundTripThroughSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	never := []string{}
	some := []string{"finished"}
	cfg := Defaults()
	cfg.Network.PublicURL = "https://moombox.example.com"
	cfg.Notifications = []NotificationConfig{
		{URL: "discord://1/aaa", Mention: "<@&123456789012345678>"},
		{URL: "discord://2/bbb", Mention: "@here", MentionEvents: &never},
		{URL: "discord://3/ccc", Enabled: boolPtr(false), MentionEvents: &some, Mention: "@everyone"},
	}
	if err := Save(cfg, path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "mention_events = []") {
		t.Errorf("the explicitly-empty mention_events was not written; file:\n%s", raw)
	}

	back, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if back.Network.PublicURL != "https://moombox.example.com" {
		t.Errorf("public_url round-tripped as %q", back.Network.PublicURL)
	}
	if back.Notifications[0].MentionEvents != nil {
		t.Error("target 0 gained a mention_events key it never had")
	}
	if e := back.Notifications[1].MentionEvents; e == nil || len(*e) != 0 {
		t.Errorf("target 1's explicit \"never\" came back as %v — the three-way encoding is broken", e)
	}
	if back.Notifications[2].IsEnabled() {
		t.Error("target 2's enabled = false did not survive the round trip")
	}
}
```

Run it — every test fails to **compile**, which is the expected first failure:

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/config/
```

Expected output contains lines of the form:

```
internal/config/notifications_test.go:NN:NN: undefined: ValidatePublicURL
internal/config/notifications_test.go:NN:NN: undefined: MentionNone
internal/config/notifications_test.go:NN:NN: undefined: ParseMention
internal/config/notifications_test.go:NN:NN: undefined: DefaultMentionEvents
internal/config/notifications_test.go:NN:NN: reported.Network.PublicURL undefined (type NetworkConfig has no field or method PublicURL)
internal/config/notifications_test.go:NN:NN: unknown field Mention in struct literal of type NotificationConfig
FAIL	github.com/vampiricwulf/Moombox/internal/config [build failed]
```

- [ ] **Step 2: Implement**

**`internal/config/types.go`** — append to `NetworkConfig` after `TrustedProxies` (`:80`):

```go
	// PublicURL is the externally reachable base URL of this dashboard
	// ("https://moombox.example.com", or "http://192.168.1.10:774" on a LAN).
	// Empty means unset, which is the default and the pre-2.8.9 behaviour.
	//
	// Its only consumer is the notification manager: when set, a job embed's
	// TITLE links to {public_url}/#job=<id> (the dashboard opens that job's
	// details) and the platform page moves to the embed's author line. Moombox
	// never binds to it, never validates that it reaches this process, and
	// never redirects to it — it is a string the operator knows and Moombox
	// does not (a reverse proxy, a tunnel, a port forward).
	//
	// Validated as an absolute http(s) URL with a host, no query, no fragment
	// and no userinfo; a trailing slash is trimmed on the way in because the
	// manager appends "/#job=". Hot-reloadable — read at send time.
	PublicURL string `toml:"public_url,omitempty" json:"public_url,omitempty"`
```

**`internal/config/types.go:446-453`** — replace `NotificationConfig` (keep the existing `tags` comment, extend it):

```go
// NotificationConfig holds notification endpoint configuration.
// (A legacy `tags` field was removed 2026-07: it was persisted and
// round-tripped but read by nothing — leftover keys in existing TOML files
// are ignored harmlessly by the decoder.)
type NotificationConfig struct {
	URL string `toml:"url,omitempty" json:"url,omitempty"`
	// Enabled is a mute switch: false keeps the target and its whole filter
	// in the config but delivers nothing. Absent means enabled, so every
	// config written before this key keeps working. A pointer for exactly
	// that reason, mirroring ChannelConfig.Enabled.
	Enabled *bool    `toml:"enabled,omitempty" json:"enabled,omitempty"`
	Events  []string `toml:"events,omitempty" json:"events,omitempty"`
	// Mention is the content line prepended to this target's messages so a
	// Discord role or user is actually pinged: "<@&ROLE_ID>", "<@USER_ID>",
	// "@everyone" or "@here". Empty means no ping. An embed can never mention
	// anyone on its own, which is why this is a separate key rather than
	// something a producer could put in a description.
	Mention string `toml:"mention,omitempty" json:"mention,omitempty"`
	// MentionEvents is which events the Mention rides along with. THREE
	// states, which is why it is a pointer to a slice and not a slice:
	//   nil              — the key was never written; DefaultMentionEvents()
	//                      applies (the owner's 2026-09-27 ruling).
	//   pointer to empty — written as `mention_events = []`; mention NEVER.
	//   pointer to a list— exactly those events.
	// A plain []string cannot express the middle state: `omitempty` omits an
	// empty slice from both the TOML and the JSON encoding, so "never" would
	// reload as "the default six".
	MentionEvents *[]string `toml:"mention_events,omitempty" json:"mention_events,omitempty"`
}
```

**`internal/config/notifications.go`** (new) — `IsEnabled`, `ParseMention`, `DefaultMentionEvents`, `ResolveMentionEvents`, `ValidatePublicURL`, plus the unexported `defaultMentionEvents` var and the `mentionRe` regexp (`^<@(!?)(\d{1,20})>$` and `^<@&(\d{1,20})>$`). `ValidatePublicURL` uses `net/url.Parse`, checks `u.Scheme` (lowercased) ∈ {http, https}, `u.Host != ""`, `u.RawQuery == "" && !u.ForceQuery`, `u.Fragment == ""`, `u.User == nil`, then `u.Path = strings.TrimSuffix(u.Path, "/")` and returns `u.String()`. `ParseMention` accepts `<@!id>` and canonicalises it to `<@id>`, with the reason in a comment: Discord clients have emitted the nickname form for years and rejecting a working paste is an operator-hostile validator.

**`internal/config/config.go:577`** — insert the `public_url` block between the `trusted_proxies` block (ends `:577`) and the `client_token_ttl_days` comment (`:578`):

```go
	// network.public_url: empty is unset. A value that cannot be an absolute
	// browsable origin is both reported and cleared — there is no sensible
	// default host to substitute, and leaving an unusable value in place
	// would paste a dead link into every job embed.
	if cfg.Network.PublicURL != "" {
		canonical, err := ValidatePublicURL(cfg.Network.PublicURL)
		if err != nil {
			fail("network.public_url %q is not usable: %v", cfg.Network.PublicURL, err)
			if !reportOnly {
				cfg.Network.PublicURL = ""
			}
		} else if !reportOnly {
			cfg.Network.PublicURL = canonical
		}
	}
```

**`internal/config/config.go:971`** — after the channel loop's closing brace, before `return errs`, add the notification loop: per target, run `ParseMention` (report `notifications[%d].mention`, clear on Normalize, store the canonical form otherwise) and the `mention_events` shape pass (trim, drop blanks, dedupe, preserve order, keep the pointer non-nil when it was non-nil).

**`config.example.toml`** — add the commented `public_url` to `[network]` (after the `trusted_proxies` example, `:50`) and rewrite the `[[notifications]]` block (`:302-322`) to document `enabled`, `mention`, `mention_events` and the empty-filter rule. While there, fix the block's two stale lines that predate this arc and now sit directly beside new, accurate text: the "Works with Discord, Slack, ntfy" claim (`parseTarget` is Discord-only by design) and the event list naming a `live` event that does not exist while omitting 17 that do (the block names 10 keys, 9 of them real, out of the vocabulary's 26). Replace the inline list with a pointer to `docs/spec/operations.md`'s table, and drop the `tags = [...]` example at `:322` for a field removed in 2026-07.

**`docs/spec/data-and-storage.md`** — add the `PublicURL` row to `#### [network]` (`:480-490`) and rewrite `#### [[notifications]]` (`:613-619`) with the five real fields. That table currently lists a `Tags []string` field that no longer exists on the struct; it goes in the same edit. That table's header is also malformed today — three columns (`Field | Type | TOML Key`) over four-cell rows — so give it a `Notes` column in the same edit rather than only replacing the rows.

**`.claude/skills/moombox-settings/SKILL.md`** — one sentence under step 8 noting that `OnNotificationsChange` now also fires for a `network.public_url` change, and that the notification keys are all hot-reloading (the restart list stays at 16). Under step 2, one sentence saying `network.public_url` needs no `Defaults()` entry because `""` is both the zero value and the documented "unset", and that `loadFromFile` decodes over `Defaults()` — which is the same fact step 9's "no migration" rests on. The file is LF; keep it LF.

- [ ] **Step 3: Verify**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/config/ ./internal/docs/
```
Expected: `ok  github.com/vampiricwulf/Moombox/internal/config` and `ok  github.com/vampiricwulf/Moombox/internal/docs`. Then the Global gate block.

**Mutations to run** (make the edit, watch the named test fail, revert):
1. Change `MentionEvents` to `[]string` with `omitempty` → `TestNotificationKeysRoundTripThroughSave` fails on the missing `mention_events = []`.
2. Make `IsEnabled` return `n.Enabled != nil && *n.Enabled` → `TestNotificationIsEnabled` fails on the absent case.
3. Drop the `u.Fragment == ""` check → `TestValidatePublicURL/fragment` fails.
4. Make `ResolveMentionEvents` return the defaults for a pointer-to-empty → `TestResolveMentionEvents/mention,_explicit_empty` fails.

- [ ] **Step 4: Commit**

```bash
git add internal/config/types.go internal/config/config.go internal/config/notifications.go \
        internal/config/notifications_test.go config.example.toml \
        docs/spec/data-and-storage.md .claude/skills/moombox-settings/SKILL.md
git commit -m "feat(config): network.public_url and per-target enabled/mention/mention_events

The four keys Arc N2b needs, with their validators. mention_events is a
*[]string because the ruling needs three states — absent (the default six),
explicitly empty (never), explicit — and omitempty cannot encode the middle
one on a plain slice. Vocabulary checking stays out of internal/config: the
notifications package imports it, so KnownEvents is unreachable from here.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" \
        -- internal/config/types.go internal/config/config.go internal/config/notifications.go \
           internal/config/notifications_test.go config.example.toml \
           docs/spec/data-and-storage.md .claude/skills/moombox-settings/SKILL.md
```

---

## Task 2: The manager acts on them — skip, mention, deep link

**Files:**
- Modify: `internal/notifications/manager.go` — `notificationTarget` (symbol, three new fields), `Manager` (symbol, the new `publicURL` field), `buildTargets` (symbol), `NewManager` (symbol), `Reload` (symbol), `Send` (symbol)
- Create: `internal/notifications/mentions.go`
- Test: `internal/notifications/target_options_test.go` (new)

**Anchors are by symbol only** — Arc N1 rewrites this file's line numbering.

- The `Manager` literals in this task's tests are written against `1d2df1d4`. N1 **deletes** the `semaphore` field and `maxInflightNotifications` (spec §1.4). When pasting, drop that line from every literal and use whatever field N1's per-target FIFO needs — or build the Manager through `NewManager` and override `targets` afterwards.

**Interfaces:**

Produces, in `internal/notifications/mentions.go`:
```go
// mentionFor returns the content mention this target attaches to event and the
// matching allowed_mentions object, or ("", nil) when the target has no mention
// configured or the event is not in its mention filter. Alias-aware: a target
// whose mention filter names the older, broader event also pings for the newer
// one it split into.
func (t notificationTarget) mentionFor(event string) (string, *AllowedMentions)

// JobDeepLink returns the dashboard URL that opens a job's details, or "" when
// no public_url is configured.
func JobDeepLink(publicURL, jobID string) string
```

Extends `notificationTarget` (in `manager.go`):
```go
type notificationTarget struct {
	sender         sender
	events         map[string]bool  // nil means all events
	mention        string           // "" = this target never pings
	mentionAllowed *AllowedMentions // built once in buildTargets from ParseMention's (form, id)
	mentionEvents  map[string]bool  // nil when mention == ""
	// (Task 7 adds: batch *batcher)
}
```

Extends `Manager`: `publicURL string`, written under `targetsMu` alongside `targets` in `NewManager` and `Reload`, snapshotted in `Send` with the same RLock that snapshots `targets`.

Consumes from N1: `SendOptions.{Mention, MentionAllowed, JobID, Author}`, the `Author` struct, and the `AllowedMentions` object (`struct{ Parse []string; Roles, Users []string }`, controller ruling 4).

**The Author rule.** The rewrite fires only when `publicURL != "" && opts.JobID != ""`:
- `opts.Author != nil` → **copy** the Author (it is a pointer shared across targets and with the caller; mutating it in place would corrupt the next target's send), set the copy's `URL` to the old `opts.URL`, then set `opts.URL = JobDeepLink(pub, opts.JobID)`.
- `opts.Author == nil` → **no rewrite at all.** With no author line there is nowhere for the platform link to go, and dropping the only link to the video to gain a dashboard link is a net loss. N2a gives every job send an Author, so this arm is the pre-N2a and System-send case.

- [ ] **Step 1: Write the failing tests**

Create `internal/notifications/target_options_test.go`:

```go
package notifications

import (
	"reflect"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

func boolPtr(b bool) *bool { return &b }

// TestBuildTargetsSkipsDisabled pins the mute switch. A disabled target must
// leave the delivery list entirely — not deliver-and-discard — so HasTargets
// reports false when everything is muted and the HasTargets guards in the
// monitor and disk paths stop building embeds nobody will read.
func TestBuildTargetsSkipsDisabled(t *testing.T) {
	cfg := &config.MoomboxConfig{Notifications: []config.NotificationConfig{
		{URL: "discord://1/aaa", Enabled: boolPtr(false)},
		{URL: "discord://2/bbb"},
	}}
	targets := buildTargets(cfg, testLogger{})
	if len(targets) != 1 {
		t.Fatalf("buildTargets returned %d targets, want 1 — the disabled entry was not skipped", len(targets))
	}

	allOff := &config.MoomboxConfig{Notifications: []config.NotificationConfig{
		{URL: "discord://1/aaa", Enabled: boolPtr(false)},
	}}
	m := NewManager(allOff, testLogger{})
	if m.HasTargets() {
		t.Error("HasTargets reported true with every target disabled — all FOUR producer guards " +
			"(cmd/moombox/helpers.go update-available, cmd/moombox/main.go disk, and both " +
			"cmd/moombox/monitor_callbacks.go Stream Found sites) would keep building embeds " +
			"that go nowhere")
	}
}

// TestBuildTargetsDisabledDoesNotShadowItsTwin guards the interaction with
// target dedupe: two spellings of ONE webhook collapse on the resolved URL,
// and the disabled spelling must not take the survivor's slot.
func TestBuildTargetsDisabledDoesNotShadowItsTwin(t *testing.T) {
	// The two entries carry DIFFERENT filters on purpose: with both unfiltered
	// the surviving target's filter is nil either way, and the assertion could
	// not tell a skip-before-dedupe from a skip-after-dedupe.
	cfg := &config.MoomboxConfig{Notifications: []config.NotificationConfig{
		{URL: "discord://1/aaa", Enabled: boolPtr(false), Events: []string{"error"}},
		{URL: "https://discord.com/api/webhooks/1/aaa", Events: []string{"finished"}},
	}}
	targets := buildTargets(cfg, testLogger{})
	if len(targets) != 1 {
		t.Fatalf("buildTargets returned %d targets, want 1", len(targets))
	}
	if !targets[0].events["finished"] || targets[0].events["error"] {
		t.Errorf("the surviving target's filter is %v — it inherited the disabled entry's filter, "+
			"so the skip runs after the dedupe instead of before it", targets[0].events)
	}
}

// TestMentionFor is the whole mention decision: who pings, for what, and the
// three ways to say "nobody". The alias row matters because a target that
// asked to be pinged for disk_warning must be pinged for the MORE urgent
// disk_critical — silently losing the escalation is the worst outcome here.
func TestMentionFor(t *testing.T) {
	mk := func(nc config.NotificationConfig) notificationTarget {
		nc.URL = "discord://1/aaa"
		got := buildTargets(&config.MoomboxConfig{Notifications: []config.NotificationConfig{nc}}, testLogger{})
		if len(got) != 1 {
			t.Fatalf("buildTargets returned %d targets", len(got))
		}
		return got[0]
	}
	never := []string{}
	only := []string{"finished"}
	warnOnly := []string{"disk_warning"}

	for _, tc := range []struct {
		name  string
		nc    config.NotificationConfig
		event string
		want  string
		why   string
	}{
		{"no mention configured", config.NotificationConfig{}, "error", "",
			"a target with no mention key never pings"},
		{"default list, error", config.NotificationConfig{Mention: "@here"}, "error", "@here",
			"error is in the ruling's default six"},
		{"default list, finished", config.NotificationConfig{Mention: "@here"}, "finished", "",
			"finished is not in the default six — a completed download must not ping anyone"},
		{"explicit never", config.NotificationConfig{Mention: "@here", MentionEvents: &never}, "error", "",
			"an explicit empty list means never, even for error"},
		{"explicit list, hit", config.NotificationConfig{Mention: "@here", MentionEvents: &only}, "finished", "@here",
			"an explicit list is honoured as written"},
		{"explicit list, miss", config.NotificationConfig{Mention: "@here", MentionEvents: &only}, "error", "",
			"error is not in this operator's list"},
		{"alias escalation", config.NotificationConfig{Mention: "<@&123456789012345678>", MentionEvents: &warnOnly},
			"disk_critical", "<@&123456789012345678>",
			"disk_critical aliases to disk_warning; a target asking to be pinged for the warning must be " +
				"pinged for the critical"},
		{"empty event never pings", config.NotificationConfig{Mention: "@here"}, "", "",
			"an empty Event bypasses every filter by design; it must not therefore ping everyone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := mk(tc.nc).mentionFor(tc.event); got != tc.want {
				t.Errorf("mentionFor(%q) = %q, want %q — %s", tc.event, got, tc.want, tc.why)
			}
		})
	}
}

// TestMentionAllowedPerForm is spec §3.2's other half. Discord ignores a
// content mention unless allowed_mentions names it, so a target whose role
// ping renders as grey text is indistinguishable from a delivery failure —
// and the webhook default (`{"parse": ["users"]}`) would swallow a role ping
// silently.
func TestMentionAllowedPerForm(t *testing.T) {
	for _, tc := range []struct {
		mention string
		want    AllowedMentions
	}{
		{"<@&123456789012345678>", AllowedMentions{Roles: []string{"123456789012345678"}}},
		{"<@123456789012345678>", AllowedMentions{Users: []string{"123456789012345678"}}},
		{"@everyone", AllowedMentions{Parse: []string{"everyone"}}},
		{"@here", AllowedMentions{Parse: []string{"everyone"}}},
	} {
		tg := buildTargets(&config.MoomboxConfig{Notifications: []config.NotificationConfig{
			{URL: "discord://1/aaa", Mention: tc.mention},
		}}, testLogger{})[0]
		_, got := tg.mentionFor("error")
		if got == nil || !reflect.DeepEqual(*got, tc.want) {
			t.Errorf("mentionFor for %s = %+v, want %+v", tc.mention, got, tc.want)
		}
	}
}

// TestSendAttachesMention proves the decision reaches SendOptions, and that a
// target whose filter excludes the event gets no content line at all.
func TestSendAttachesMention(t *testing.T) {
	var got []SendOptions
	rec := senderFunc(func(_, _ string, _ int, _ []Field, opts SendOptions) error {
		got = append(got, opts)
		return nil
	})
	m := &Manager{
		logger:    testLogger{},
		semaphore: make(chan struct{}, maxInflightNotifications),
		targets: []notificationTarget{{
			sender:         rec,
			mention:        "<@&123456789012345678>",
			mentionAllowed: &AllowedMentions{Roles: []string{"123456789012345678"}},
			mentionEvents:  map[string]bool{"error": true},
		}},
	}
	m.Send("Job Failed", "d", TypeError, nil, SendOptions{Event: "error"})
	m.Send("Download Finished", "d", TypeSuccess, nil, SendOptions{Event: "finished"})
	m.Wait()

	if len(got) != 2 {
		t.Fatalf("recorded %d sends, want 2", len(got))
	}
	if got[0].Mention != "<@&123456789012345678>" {
		t.Errorf("the error send carried Mention = %q, want the configured role", got[0].Mention)
	}
	if got[0].MentionAllowed == nil {
		t.Error("the error send carried no allowed_mentions — Discord would render the role as plain " +
			"text and ping nobody")
	}
	if got[1].Mention != "" {
		t.Errorf("the finished send carried Mention = %q, want none — it is not in the mention filter", got[1].Mention)
	}
	if got[1].MentionAllowed != nil {
		t.Error("the finished send carried an allowed_mentions object with no content mention to match it")
	}
}

// TestJobDeepLink pins the link shape the SPA parses, including the defensive
// trailing-slash trim (the stored value is normalised, but a value that
// reached the manager through a path that skipped Normalize must not produce
// a double slash).
func TestJobDeepLink(t *testing.T) {
	for _, tc := range []struct{ pub, id, want string }{
		{"", "j1", ""},
		{"https://x.example", "", ""},
		{"https://x.example", "j1", "https://x.example/#job=j1"},
		{"https://x.example/", "j1", "https://x.example/#job=j1"},
		{"https://x.example/moombox", "j1", "https://x.example/moombox/#job=j1"},
		{"https://x.example", "a b", "https://x.example/#job=a%20b"},
	} {
		if got := JobDeepLink(tc.pub, tc.id); got != tc.want {
			t.Errorf("JobDeepLink(%q, %q) = %q, want %q", tc.pub, tc.id, got, tc.want)
		}
	}
}

// TestSendRewritesJobURL is ruling Q6: with public_url set, a job embed's
// TITLE goes to the dashboard and the platform page moves to the author line.
// The copy assertion is the one that would bite in production — Author is a
// pointer the caller still owns and every target shares, so an in-place write
// would leak one target's rewrite into the next.
func TestSendRewritesJobURL(t *testing.T) {
	var got []SendOptions
	rec := senderFunc(func(_, _ string, _ int, _ []Field, opts SendOptions) error {
		got = append(got, opts)
		return nil
	})
	author := &Author{Name: "Some Channel", URL: "https://youtube.com/@some", IconURL: "https://i/a.jpg"}
	m := &Manager{
		logger:    testLogger{},
		semaphore: make(chan struct{}, maxInflightNotifications),
		publicURL: "https://moombox.example.com",
		targets:   []notificationTarget{{sender: rec}, {sender: rec}},
	}
	m.Send("Download Finished", "d", TypeSuccess, nil, SendOptions{
		Event:  "finished",
		JobID:  "job-1",
		URL:    "https://youtube.com/watch?v=abc",
		Author: author,
	})
	m.Wait()

	if len(got) != 2 {
		t.Fatalf("recorded %d sends, want 2", len(got))
	}
	for i, o := range got {
		if o.URL != "https://moombox.example.com/#job=job-1" {
			t.Errorf("send %d: title URL = %q, want the dashboard deep link", i, o.URL)
		}
		if o.Author == nil || o.Author.URL != "https://youtube.com/watch?v=abc" {
			t.Errorf("send %d: author URL = %v, want the platform page it displaced", i, o.Author)
		}
		if o.Author != nil && o.Author.Name != "Some Channel" {
			t.Errorf("send %d: the author's name was lost in the rewrite", i)
		}
	}
	if author.URL != "https://youtube.com/@some" {
		t.Errorf("the caller's Author was mutated in place (URL = %q) — it is shared across targets "+
			"and with the producer", author.URL)
	}
}

// TestSendLeavesURLAloneWithoutAuthorOrJobID pins the three no-rewrite arms.
// The no-Author arm is deliberate: with nowhere to put the platform page,
// rewriting the title would DELETE the only link to the video.
func TestSendLeavesURLAloneWithoutAuthorOrJobID(t *testing.T) {
	for _, tc := range []struct {
		name string
		pub  string
		opts SendOptions
	}{
		{"no public_url", "", SendOptions{Event: "finished", JobID: "j1", URL: "https://p/v",
			Author: &Author{Name: "c", URL: "https://p/c"}}},
		{"no job id", "https://x.example", SendOptions{Event: "disk_warning", URL: "https://p/v",
			Author: &Author{Name: "c", URL: "https://p/c"}}},
		{"no author", "https://x.example", SendOptions{Event: "finished", JobID: "j1", URL: "https://p/v"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got SendOptions
			rec := senderFunc(func(_, _ string, _ int, _ []Field, opts SendOptions) error {
				got = opts
				return nil
			})
			m := &Manager{
				logger:    testLogger{},
				semaphore: make(chan struct{}, maxInflightNotifications),
				publicURL: tc.pub,
				targets:   []notificationTarget{{sender: rec}},
			}
			m.Send("t", "d", TypeInfo, nil, tc.opts)
			m.Wait()
			if got.URL != tc.opts.URL {
				t.Errorf("URL = %q, want %q left alone", got.URL, tc.opts.URL)
			}
		})
	}
}

// TestReloadPicksUpPublicURL pins the hot-reload path the web route drives:
// the manager reads the value from the config it was reloaded with, so a
// Settings save changes the next embed's link with no restart.
func TestReloadPicksUpPublicURL(t *testing.T) {
	m := NewManager(&config.MoomboxConfig{}, testLogger{})
	if m.publicURL != "" {
		t.Fatalf("a fresh manager has publicURL = %q", m.publicURL)
	}
	cfg := &config.MoomboxConfig{}
	cfg.Network.PublicURL = "https://later.example"
	m.Reload(cfg)
	if m.publicURL != "https://later.example" {
		t.Errorf("Reload left publicURL = %q — a public_url save would need a restart", m.publicURL)
	}
}
```

Run:

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/notifications/
```

Expected (build failure):

```
internal/notifications/target_options_test.go:NN:NN: unknown field mention in struct literal of type notificationTarget
internal/notifications/target_options_test.go:NN:NN: unknown field publicURL in struct literal of type Manager
internal/notifications/target_options_test.go:NN:NN: undefined: JobDeepLink
internal/notifications/target_options_test.go:NN:NN: got[0].Mention undefined (type SendOptions has no field or method Mention)
FAIL	github.com/vampiricwulf/Moombox/internal/notifications [build failed]
```

> If Arc N1 has already merged, `Mention`/`MentionAllowed`/`Author`/`AllowedMentions` resolve and only the `notificationTarget`/`publicURL`/`JobDeepLink` lines fail. Both are the correct red state.

- [ ] **Step 2: Implement**

1. `notificationTarget` gains `mention`, `mentionAllowed` and `mentionEvents`.
2. `buildTargets`, at the top of its loop body, right after the `url == ""` guard:
   ```go
   		// A disabled target is kept in the config, with its filter and its
   		// mention intact, and delivers nothing. This is the mute an operator
   		// previously had to fake by deleting the webhook (the web UI's
   		// "untick every event" route silently subscribed them to EVERYTHING
   		// instead — an empty filter means all events).
   		if !nc.IsEnabled() {
   			logger.Info("notification target disabled — skipping", "url", redactURLForLog(url))
   			continue
   		}
   ```
   It precedes the `parseTarget` call and the dedupe, so a disabled entry can neither shadow its enabled twin nor warn about a URL nobody will use.
3. `buildTargets`, after the event filter is built: resolve the mention. `nc.ResolveMentionEvents()` into a `map[string]bool`; Warn (with a redacted URL) for any entry outside `KnownEvents`, exactly as the events filter does; call `config.ParseMention(nc.Mention)` once and build `mentionAllowed` from its `(form, id)` — `MentionRole` → `&AllowedMentions{Roles: []string{id}}`, `MentionUser` → `&AllowedMentions{Users: []string{id}}`, `MentionEveryone`/`MentionHere` → `&AllowedMentions{Parse: []string{"everyone"}}` (Discord resolves `@here` through the same `everyone` parse token). Leave `mention`/`mentionAllowed`/`mentionEvents` zero when `nc.Mention == ""`.
4. Dedupe union: when a duplicate resolved URL collapses, the FIRST occurrence's mention wins (same rule as the sender and the slot). Add one sentence to the existing union comment.
5. `Manager` gains `publicURL string`. `NewManager` sets it from `cfg.Network.PublicURL`; `Reload` sets it under the same `targetsMu.Lock()` that swaps `targets`.
6. `Send` snapshots `pub := m.publicURL` inside the existing RLock, applies the Author/URL rewrite **once, before the target loop** (the value is install-wide, not per target), and inside the loop — after the event filter passes — does `perTarget := opts; perTarget.Mention, perTarget.MentionAllowed = target.mentionFor(opts.Event)`. `opts` is a value parameter, so the per-target write needs that local copy; the `*AllowedMentions` it carries is built once in `buildTargets` and is never mutated after, so sharing the pointer across sends is safe.
7. `internal/notifications/mentions.go` holds `mentionFor` and `JobDeepLink`. `JobDeepLink` returns `""` when either argument is empty, trims a trailing `/` from the base, and appends `"/#job=" + url.PathEscape(jobID)`.

- [ ] **Step 3: Verify**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/notifications/
```
Expected: `ok  github.com/vampiricwulf/Moombox/internal/notifications`. Then the Global gate block.

**Mutations to run:**
1. Move the `IsEnabled` skip to *after* the dedupe → `TestBuildTargetsDisabledDoesNotShadowItsTwin` fails.
2. Drop the alias lookup from `mentionFor` → `TestMentionFor/alias_escalation` fails.
3. Mutate the Author in place instead of copying → `TestSendRewritesJobURL` fails on the caller-mutation assertion.
4. Rewrite the title URL when `Author == nil` → `TestSendLeavesURLAloneWithoutAuthorOrJobID/no_author` fails.
5. Read `publicURL` in `NewManager` only → `TestReloadPicksUpPublicURL` fails.
6. Build `&AllowedMentions{Parse: []string{"everyone"}}` for every form → `TestMentionAllowedPerForm` fails on the role and user rows.
7. Return `mentionAllowed` unconditionally, ignoring the filter → `TestSendAttachesMention` fails on the finished send.

- [ ] **Step 4: Commit**

```bash
git commit -m "feat(notifications): skip disabled targets, attach mentions, link the dashboard

buildTargets drops a target with enabled = false before it can shadow its
deduped twin. Send fills SendOptions.Mention from the per-target filter
(alias-aware) and, when network.public_url is set, moves a job embed's
platform link onto the author line so the title can point at the dashboard.
Author is copied, never mutated: it is a pointer shared across targets.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" \
        -- internal/notifications/manager.go internal/notifications/mentions.go \
           internal/notifications/target_options_test.go
```

---

## Task 3: The web API — validate, apply, strip, hot-reload

**Files:**
- Modify: `internal/web/routes/config_routes.go:124-175` (`validateConfigUpdates`, the `network` block) and `:468` (the blank line before `return errs` at `:469`, after the channels block)
- Modify: `internal/web/routes/config_routes.go:477-496` (`applyConfigUpdates`, the network arm) and `:749-768` (the notifications arm)
- Modify: `internal/web/routes/config_routes.go:893-901` (the pre-apply snapshot block), `:920-931` (the post-save new-value block) and `:947-951` (the `OnNotificationsChange` firing arm)
- Test: `internal/web/routes/config_notifications_test.go` (new)
- Test: `internal/web/routes/config_routes_test.go:20-54` (add one field + one callback to `configRoutesFixture`)

**Interfaces:** no new exported symbols. Behaviour contract:

| Input | Outcome |
|---|---|
| `network.public_url` unusable | `400`, `details["network.public_url"]` names it |
| `network.public_url` usable but untidy | `200`, the **canonical** form is stored |
| `network.public_url` changed | `OnNotificationsChange` fires (it is what re-reads the value into the manager) |
| `notifications[i].enabled` bool | stored |
| `notifications[i].mention` unusable | `400`, `details["notifications[i].mention"]` |
| `notifications[i].events` with unknown entries, some known | unknown entries stripped, `200` |
| `notifications[i].events` with **only** unknown entries | stored **as submitted**, `200` |
| `notifications[i].mention_events` absent | stored absent (⇒ the default six) |
| `notifications[i].mention_events` `[]` | stored as an explicit empty list (⇒ never) |
| `notifications[i].mention_events` with unknown entries | unknown entries stripped unconditionally |

**Unknown per-target keys must SURVIVE the save.** `applyConfigUpdates` rebuilds each `NotificationConfig` field by field today (`config_routes.go:749-768`), so any key the route does not know is dropped — and the SPA round-trips the whole object (`_saveNotificationsOnly` sends `this.app.config.notifications` verbatim), so the key *is* in the payload and the route is what discards it. N3's `mode` would vanish on the next unrelated Settings save. The `channels` arm two blocks down already solves exactly this with a `json.Marshal`/`json.Unmarshal` round trip (`config_routes.go:772-777`); this task adopts that idiom.

**Why the two `events` strip rules differ.** Stripping an all-unknown `events` filter to empty would turn "matches nothing" into "matches **everything**" — `buildTargets` treats an absent filter as all events, and today's code deliberately keeps a garbage filter non-nil for exactly that reason. So an all-unknown filter is left alone: the manager's startup Warn tells the operator, and the web UI cannot produce that state anyway (its chips come from the vocabulary). `mention_events` has no such trap: stripping only ever *narrows* who gets pinged, and an emptied list is the meaningful "never".

**The hot-reload gap this closes.** At `1d2df1d4` the PUT handler fires `OnNotificationsChange` only when the payload carries a `notifications` key (`config_routes.go:949`). `public_url` lives in `network`, and the web form's Save sends `network` without `notifications` unless a webhook exists (`settings.js:1040-1042`). So a `public_url` change would never reach `Manager.Reload`. The TUI has no such gap — `cmd/moombox/tui_wiring.go:418` calls `s.notifyMgr.Reload(snap)` unconditionally on every settings save.

- [ ] **Step 1: Write the failing tests**

Add to `configRoutesFixture` (`internal/web/routes/config_routes_test.go:20-54`): a `notifs atomic.Bool` field and `OnNotificationsChange: func() { f.notifs.Store(true) }` in the callback literal.

Create `internal/web/routes/config_notifications_test.go` with, at minimum:

```go
package routes

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/notifications"
)

// putConfigExpect PUTs a body and returns the recorder so a test can assert
// on a 400's details map as well as on a 200.
func putConfigExpect(t *testing.T, f *configRoutesFixture, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("PUT", "/api/config", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

// TestConfigPutPublicURLValidation mirrors config.ValidatePublicURL at the API
// edge so a value the route accepts is never one Normalize then rewrites
// behind the operator's back.
func TestConfigPutPublicURLValidation(t *testing.T) {
	f := newConfigRoutesFixture(t)
	rec := putConfigExpect(t, f, map[string]any{"network": map[string]any{"public_url": "moombox.example.com"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a scheme-less public_url returned %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Details map[string]string `json:"details"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if _, ok := resp.Details["network.public_url"]; !ok {
		t.Errorf("the 400 did not name network.public_url: %v", resp.Details)
	}
}

// TestConfigPutPublicURLStoredCanonical: the route stores the canonical form,
// so the value the manager pastes into embeds is the same whichever editor
// typed it.
func TestConfigPutPublicURLStoredCanonical(t *testing.T) {
	f := newConfigRoutesFixture(t)
	putConfig(t, f, map[string]any{"network": map[string]any{"public_url": "  https://x.example/  "}})
	var got string
	f.store.Read(func(c *config.MoomboxConfig) { got = c.Network.PublicURL })
	if got != "https://x.example" {
		t.Errorf("stored public_url = %q, want the canonical form", got)
	}
}

// TestConfigPutPublicURLFiresNotificationsCallback is the hot-reload gap.
// public_url lives in [network] and the web form's Save sends `network`
// without `notifications` when no webhook is configured, so without this arm
// a public_url change would sit in the file until the next restart.
func TestConfigPutPublicURLFiresNotificationsCallback(t *testing.T) {
	f := newConfigRoutesFixture(t)
	putConfig(t, f, map[string]any{"network": map[string]any{"public_url": "https://x.example"}})
	if !f.notifs.Load() {
		t.Error("OnNotificationsChange did not fire for a public_url change — the manager would keep " +
			"the old base URL until a restart")
	}
}

// TestConfigPutPublicURLUnchangedDoesNotFire keeps the callback honest:
// re-saving the same value must not rebuild the target list, matching every
// other change-gated callback on this route.
func TestConfigPutPublicURLUnchangedDoesNotFire(t *testing.T) {
	f := newConfigRoutesFixture(t)
	putConfig(t, f, map[string]any{"network": map[string]any{"public_url": "https://x.example"}})
	f.notifs.Store(false)
	putConfig(t, f, map[string]any{"network": map[string]any{"public_url": "https://x.example"}})
	if f.notifs.Load() {
		t.Error("OnNotificationsChange fired for an unchanged public_url")
	}
}

// TestConfigPutNotificationKeysPersist covers enabled, mention and the
// three-way mention_events through the real merge path.
func TestConfigPutNotificationKeysPersist(t *testing.T) {
	f := newConfigRoutesFixture(t)
	putConfig(t, f, map[string]any{"notifications": []any{
		map[string]any{"url": "discord://1/aaa", "enabled": false},
		map[string]any{"url": "discord://2/bbb", "mention": "<@&123456789012345678>"},
		map[string]any{"url": "discord://3/ccc", "mention": "@here", "mention_events": []any{}},
		map[string]any{"url": "discord://4/ddd", "mention": "@here", "mention_events": []any{"finished"}},
	}})
	var n []config.NotificationConfig
	f.store.Read(func(c *config.MoomboxConfig) { n = append(n, c.Notifications...) })
	if len(n) != 4 {
		t.Fatalf("stored %d targets, want 4", len(n))
	}
	if n[0].IsEnabled() {
		t.Error("target 0: enabled = false was not stored")
	}
	if n[1].Mention != "<@&123456789012345678>" {
		t.Errorf("target 1: mention = %q", n[1].Mention)
	}
	if n[1].MentionEvents != nil {
		t.Error("target 1: an absent mention_events must stay absent so the default list applies")
	}
	if e := n[2].MentionEvents; e == nil || len(*e) != 0 {
		t.Errorf("target 2: explicit empty mention_events came back as %v — \"never ping\" was lost", e)
	}
	if e := n[3].MentionEvents; e == nil || len(*e) != 1 || (*e)[0] != "finished" {
		t.Errorf("target 3: mention_events = %v", e)
	}
}

// TestConfigPutRejectsBadMention: a token Discord cannot resolve renders as
// literal text and pings nobody, which looks like a delivery failure.
func TestConfigPutRejectsBadMention(t *testing.T) {
	f := newConfigRoutesFixture(t)
	rec := putConfigExpect(t, f, map[string]any{"notifications": []any{
		map[string]any{"url": "discord://1/aaa", "mention": "@ops-team"},
	}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "notifications[0].mention") {
		t.Errorf("the 400 did not name the field: %s", rec.Body.String())
	}
}

// TestConfigPutStripsUnknownEvents is audit §4's web/TUI divergence:
// toggleNotificationEvent splices the stored array and applyConfigUpdates
// stored any string, so an unknown key survived a web save forever. The
// all-unknown case is the exception and it is load-bearing — stripping it to
// empty would turn "matches nothing" into "matches EVERYTHING".
func TestConfigPutStripsUnknownEvents(t *testing.T) {
	f := newConfigRoutesFixture(t)
	putConfig(t, f, map[string]any{"notifications": []any{
		map[string]any{"url": "discord://1/aaa", "events": []any{"finished", "bogus_event", "error"}},
		map[string]any{"url": "discord://2/bbb", "events": []any{"bogus_only"}},
		map[string]any{"url": "discord://3/ccc", "mention": "@here", "mention_events": []any{"error", "nope"}},
	}})
	var n []config.NotificationConfig
	f.store.Read(func(c *config.MoomboxConfig) { n = append(n, c.Notifications...) })

	if strings.Join(n[0].Events, ",") != "finished,error" {
		t.Errorf("target 0 events = %v, want the unknown entry stripped and the order kept", n[0].Events)
	}
	if strings.Join(n[1].Events, ",") != "bogus_only" {
		t.Errorf("target 1 events = %v — an ALL-unknown filter must survive as written; stripping it to "+
			"empty makes the manager treat the target as unfiltered and deliver every event", n[1].Events)
	}
	if e := n[2].MentionEvents; e == nil || strings.Join(*e, ",") != "error" {
		t.Errorf("target 2 mention_events = %v, want the unknown entry stripped", e)
	}
}

// TestNotificationsApplyUsesTheSharedDecode is the forward-compat pin. The
// notifications arm rebuilt each target from the keys it had an explicit arm
// for, so every field added later needed a route edit too — and the first one
// anybody forgets (N3's `mode`) is silently erased by an unrelated Settings
// save, because the SPA sends the whole stored object back and the route is
// what drops it. The `channels` arm has always decoded instead.
//
// This is a source-level assertion on purpose: no BEHAVIOURAL test can tell
// the two apart until a field exists that the route has no arm for, and the
// whole point of the change is that such a field must never need one. When N3
// adds `Mode`, replace this with a `mode` round trip through putConfig.
func TestNotificationsApplyUsesTheSharedDecode(t *testing.T) {
	src, err := os.ReadFile("config_routes.go")
	if err != nil {
		t.Fatal(err)
	}
	arm := regexp.MustCompile(`(?s)// Notifications\n.*?\n\t}\n`).Find(src)
	if arm == nil {
		t.Fatal("the // Notifications arm of applyConfigUpdates was not found")
	}
	if !strings.Contains(string(arm), "json.Unmarshal") {
		t.Error("the notifications arm still builds config.NotificationConfig field by field; " +
			"every field a later arc adds would then need a route edit, and N3's `mode` would be " +
			"erased by any unrelated Settings save. Decode the array the way the channels arm does.")
	}
}

// TestDefaultMentionEventsMirroredInSettingsJS is the cheap insurance the
// audit asked for, applied to the one list this arc adds on both sides. The
// web editor preselects these chips when a mention is first typed; a drift
// would show one set in the browser and ping on another.
func TestDefaultMentionEventsMirroredInSettingsJS(t *testing.T) {
	raw, err := os.ReadFile("../../../web/public/modules/settings.js")
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile(`(?s)const DEFAULT_MENTION_EVENTS = \[(.*?)\];`).FindSubmatch(raw)
	if block == nil {
		t.Fatal("DEFAULT_MENTION_EVENTS not found in settings.js")
	}
	var js []string
	for _, m := range regexp.MustCompile(`"([a-z_]+)"`).FindAllSubmatch(block[1], -1) {
		js = append(js, string(m[1]))
	}
	if strings.Join(js, ",") != strings.Join(config.DefaultMentionEvents(), ",") {
		t.Errorf("settings.js DEFAULT_MENTION_EVENTS = %v, Go = %v", js, config.DefaultMentionEvents())
	}
	// The vocabulary the chips are drawn from must know every default that is
	// already a real key. sidecar_down arrives with Arc N2a; until then it is
	// inert by design, so it is exempt rather than asserted.
	for _, e := range config.DefaultMentionEvents() {
		if e == "sidecar_down" {
			continue
		}
		if !notifications.KnownEvents[e] {
			t.Errorf("default mention event %q is not in the vocabulary", e)
		}
	}
}
```

Run:

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/web/routes/ -run 'ConfigPut|DefaultMentionEvents'
```

Expected: `TestConfigPutPublicURLValidation` fails with `a scheme-less public_url returned 200, want 400`; `TestConfigPutNotificationKeysPersist` fails with `target 0: enabled = false was not stored`; `TestConfigPutPublicURLFiresNotificationsCallback` fails with `OnNotificationsChange did not fire…`; `TestNotificationsApplyUsesTheSharedDecode` fails with `the notifications arm still builds config.NotificationConfig field by field…`; `TestDefaultMentionEventsMirroredInSettingsJS` fails with `DEFAULT_MENTION_EVENTS not found in settings.js` (Task 4 adds it).

Two of these are red for a weaker reason than they will be later, and the plan says so rather than letting an implementer over-read them. `TestConfigPutPublicURLUnchangedDoesNotFire` **passes vacuously at this point** — the callback never fires at all — and only becomes meaningful once step 5 lands; it is what mutation 2 kills. `TestConfigPutPublicURLStoredCanonical` cannot distinguish the apply arm's canonicalisation from the `Normalize` that `config.Save` runs on the copy before `*cfg = cfgCopy`; it is a genuine round-trip pin (nothing reads the key yet, so it is red now) but it is not a test of step 3 specifically.

- [ ] **Step 2: Implement**

1. `validateConfigUpdates`, inside the `network` block (`:124-175`, after the `trusted_proxies` arm that ends at `:166`): call `config.ValidatePublicURL` on a present `public_url` string and record `errs["network.public_url"]` on failure.
2. `validateConfigUpdates`, at `:468` before `return errs`: a `notifications` block that walks the array and records `errs[fmt.Sprintf("notifications[%d].mention", i)]` for an unparseable mention. Unknown event names are **not** errors here — they are stripped in step 3.
3. `applyConfigUpdates`, network arm (`:477-496`): store the canonical `public_url` (`config.ValidatePublicURL`'s first return; an error here is unreachable because validation already ran, but fall back to leaving the stored value untouched rather than writing garbage).
4. `applyConfigUpdates`, notifications arm (`:749-768`): **stop rebuilding the struct field-by-field.** Decode the array the way the `channels` arm at `:772-777` already does — `data, _ := json.Marshal(notifs); var ncs []config.NotificationConfig; if json.Unmarshal(data, &ncs) != nil { return }` — so every tagged field carries through and a field a later arc adds (N3's `Mode`) needs no route edit and survives a save written before it existed. The decode also gives the three-way `mention_events` for free: an absent key leaves the pointer `nil`, `[]` produces a non-nil pointer to an empty slice. Then post-process the decoded slice: canonicalise `Mention` via `config.ParseMention`, strip unknown `Events` entries **unless** the strip would empty a non-empty list, and strip unknown `mention_events` entries unconditionally. Finally `cfg.Notifications = ncs`.
5. The PUT handler: snapshot `oldPublicURL := cfg.Network.PublicURL` beside the other pre-apply snapshots (`:893-901`), read `newPublicURL` beside the others (`:920-931`), and extend the firing condition at `:949`:
   ```go
   			// public_url lives in [network], not [notifications], but the
   			// notification manager is its only consumer — it reads the base
   			// URL at send time from the config Reload hands it. The web
   			// form's Save sends `network` WITHOUT `notifications` when no
   			// webhook is configured, so without this second trigger a
   			// public_url change would sit unread until the next restart.
   			// (The TUI has no such gap: its save path calls Reload
   			// unconditionally — cmd/moombox/tui_wiring.go.)
   			_, hasNotifs := updates["notifications"]
   			if (hasNotifs || newPublicURL != oldPublicURL) && callbacks.OnNotificationsChange != nil {
   				callbacks.OnNotificationsChange()
   			}
   ```

- [ ] **Step 3: Verify**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/web/routes/ \
  -skip TestDefaultMentionEventsMirroredInSettingsJS
```
Expected: `ok  github.com/vampiricwulf/Moombox/internal/web/routes`. `TestDefaultMentionEventsMirroredInSettingsJS` is skipped here because it asserts against `settings.js`, which Task 4 has not written yet — note the deferral in the commit body; Task 4's verify step runs the package without `-skip` and turns it green. Then the Global gate block.

**Mutations to run:**
1. Change the firing condition back to `hasNotifs` alone → `TestConfigPutPublicURLFiresNotificationsCallback` fails.
2. Drop the change-gate (`newPublicURL != oldPublicURL`) and fire always → `TestConfigPutPublicURLUnchangedDoesNotFire` fails.
3. Strip an all-unknown `events` list to empty → `TestConfigPutStripsUnknownEvents` fails on target 1.
4. Treat an absent `mention_events` the same as `[]` → `TestConfigPutNotificationKeysPersist` fails on target 1.
5. Rebuild the notifications arm field by field instead of decoding → `TestNotificationsApplyUsesTheSharedDecode` fails.

- [ ] **Step 4: Commit**

Pathspec: `internal/web/routes/config_routes.go internal/web/routes/config_routes_test.go internal/web/routes/config_notifications_test.go`.
Message: `feat(web): accept the notification keys, strip unknown events, reload on public_url` plus the two trailers from Global Constraints, verbatim.

---

## Task 4: The web editor — the three per-target controls and the Network row

**Files:**
- Modify: `web/public/index.html:630` (after `cfg-probe-targets`, still inside `#settings-network`)
- Modify: `web/public/modules/settings.js:20-69` (the constants block; add `DEFAULT_MENTION_EVENTS` beside `ALL_EVENT_IDS` at `:69`), `:745-746` (populate), `:918-926` (gather), `:941-948` (payload), `:1771-1843` (`renderNotificationsList` + the `_notifDelegated` guard at `:1830-1842`), `:1940-1994` (the three filter handlers: `toggleNotificationEvent` `:1940`, `enableNotificationFilter` `:1968`, `clearNotificationFilter` `:1982`) and `:2001` (`_saveNotificationsOnly`, unchanged but read by the new handlers)
- Test: `web/tests/settings-notifications.test.mjs` (new)

**Interfaces** (new/changed methods on `SettingsController`):
```js
const DEFAULT_MENTION_EVENTS = ["error", "auth", "disk_critical", "update_failed", "crash_recovered", "sidecar_down"];

/** Effective mention filter for a card: [] when no mention, the defaults when the key is absent. */
_resolveMentionEvents(notif)          // → string[]
async toggleNotificationEnabled(index)
async setNotificationMention(index, value)
async toggleMentionEvent(index, eventId)
```

**Controls per card:**
- An `<sl-switch data-notif-action="toggle-enabled">` in the card header, checked when `notif.enabled !== false`. A disabled card gets a `notification-card--disabled` class (dimmed) and a `<sl-badge variant="neutral">Muted</sl-badge>`.
- An `<sl-input data-notif-action="mention-input" placeholder="<@&ROLE_ID>, @here, @everyone">` under the URL row, value `notif.mention || ""`, committed on `sl-change` (blur/Enter) — not `sl-input`, which would PUT on every keystroke.
- A "Mentions:" chip row, rendered only when `notif.mention` is non-empty, over the same `NOTIFICATION_EVENT_GROUPS` vocabulary, each chip `variant="primary"` when the event is in `_resolveMentionEvents(notif)`. The first chip click writes an explicit array (`notif.mention_events = resolved.filter(...)`), so the defaults stop applying the moment the operator disagrees with them.
- The existing "All events" affordance is unchanged in behaviour and gains one word of help text: with no filter the card already shows `<sl-tag variant="success">All events</sl-tag>`, which is now correct rather than a trap, because muting has its own switch.

**Deliberately unchanged:** `toggleNotificationEvent`'s `delete notif.events` when the last chip is unticked. That is the documented "empty filter = all events" semantics the owner reaffirmed, and the mute is now the switch.

- [ ] **Step 1: Write the failing tests**

Create `web/tests/settings-notifications.test.mjs`, following `settings-reorder-budget.test.mjs`'s jsdom probe verbatim (lines 12-25 of that file). The harness wiring and the first test are pasted in full so nothing below has to be re-derived; the rest are described against them.

```js
import { test, after } from "node:test";
import assert from "node:assert/strict";

let jsdomMissing = null;
try {
  await import("jsdom");
} catch (e) {
  if (e.code !== "ERR_MODULE_NOT_FOUND") throw e;
  jsdomMissing = `jsdom not installed — run \`npm ci\` in web/tests (${e.code})`;
}
const harness = jsdomMissing ? null : await import("./helpers/app-dom.mjs");
const skip = jsdomMissing || false;

after(() => harness?.teardownAll());

const CONFIG = {
  network: { port: 774, public_url: "https://x.example" },
  notifications: [{ url: "discord://1/aaa" }],
};

/** An app with the Settings form populated and PUT /api/config answered. */
async function openSettings(overrides = {}) {
  const cfg = { ...structuredClone(CONFIG), ...overrides };
  const h = await harness.makeApp({
    initialState: { config: structuredClone(cfg) },
    routes: { "PUT /api/config": () => ({ success: true }) },
  });
  h.app.config = structuredClone(cfg);
  h.app.settings.populateConfigForm();
  h.app.settings.renderNotificationsList();
  return h;
}

// MUTANT: drop the setInputValue line from populateConfigForm — the field
// renders empty, and the operator's next save then erases their public_url
// (the key IS sent, as ""), silently reverting every embed to a platform link.
test("the Network section has a public_url field populated from the config", { skip }, async () => {
  const h = await openSettings();
  assert.equal(h.el("cfg-public-url").value, "https://x.example");
});
```

The remaining tests:

2. `"saveConfig sends network.public_url"` — set the field, run the save, then `const body = JSON.parse(h.http.matching("/api/config", "PUT").at(-1).body)` and assert `body.network.public_url === "https://x.example"`. (`matching(needle, method)` filters on the **pathname**, so the two-argument form is required — `settings-reorder-budget.test.mjs:72` is the pattern.)
3. `"an enabled:false target renders muted and the switch is off"`.
4. `"toggling the enabled switch PUTs enabled:false and reverts on failure"` — the `PUT /api/config` route answers `500` on the second call; assert the switch returns to on and `this.app.config.notifications[0].enabled` is restored.
5. `"a card with no mention shows no mention chips"`.
6. `"typing a mention preselects the ruling's six chips without writing them"` — after `sl-change`, assert the six chips are `primary`, and assert the PUT body's target has `mention` set and **no** `mention_events` key ("the default list must stay implicit until the operator disagrees with it — writing it out would freeze today's defaults into their config").
7. `"unticking one default chip writes the remaining five explicitly"`.
8. `"unticking every mention chip writes an explicit empty list"` — assert `Array.isArray(body.notifications[0].mention_events) && body.notifications[0].mention_events.length === 0`, with the comment that an omitted key would mean "the default six" and silently re-enable the pings the operator just switched off.
9. `"a target with no event filter renders the All events label"` — regression pin on the existing affordance.
10. `"toggling the enabled switch does not raise the unsaved-settings banner"` — after the toggle, assert `h.el("settings-unsaved-banner").style.display === "none"`. The edit is already saved; telling the operator otherwise trains them to hit Save and re-PUT a form they never touched.
11. `"the card keeps its Test and Delete buttons"` — spec §3.5 ends "Test-send unchanged", and the card rewrite is the one thing that could drop them. Assert both `[data-notif-action="test"]` and `[data-notif-action="delete"]` exist on card 0 and still carry `data-notif-index="0"`.

Run:

```bash
timeout 300 node --test web/tests/settings-notifications.test.mjs
```

Expected: every test fails, the first with `AssertionError: Cannot read properties of null (reading 'value')` (the `cfg-public-url` element does not exist yet) and the mention ones with `assert.equal(chips.length, 0)` style mismatches. Test 11 is the exception: it **passes from the start** — it is a regression pin on markup that already exists, and its job is to fail if the rewrite drops it.

- [ ] **Step 2: Implement**

`web/public/index.html:630` — after the `cfg-probe-targets` input, before `<sl-divider>`:

```html
                            <sl-input
                                id="cfg-public-url"
                                label="Public Dashboard URL"
                                placeholder="https://moombox.example.com"
                                help-text="The address you reach this dashboard on from outside. Used only in webhook notifications: a job embed's title links here and opens that job. Leave empty to keep linking to YouTube/Twitch. Applies immediately."
                            ></sl-input>
```

`settings.js` — the constant beside `ALL_EVENT_IDS` (`:69`), the `setInputValue("cfg-public-url", config.network?.public_url || "")` beside the `cfg-trusted-proxies` populate (`:745`), the gather (`:918`) and `public_url: publicUrl` in the `network` payload literal (`:947`).

Dirty tracking needs nothing **for `cfg-public-url`** — `.settings-content` (`index.html:554`, read at `settings.js:763`) already delegates `sl-change`/`sl-input` to `_markDirty()` (`:765-766`). It needs a **stop** for the two new *card* controls: `#notifications-list` (`index.html:1395`) is inside `.settings-content` too, the card controls auto-save through `_saveNotificationsOnly`, and `_markDirty` (`:1270-1275`) unconditionally sets `_dirty` and shows `#settings-unsaved-banner`. Today the card uses only `<sl-tag>` clicks so nothing fires; a switch and a text input there would raise a false "unsaved changes" banner on every auto-saved edit. Both new listeners therefore call `e.stopPropagation()` on the `sl-change` they handle.

`renderNotificationsList` — extend the card template and **split the delegate by event type** inside the existing `_notifDelegated` guard (`:1830-1842`). The **click** listener keeps its five existing actions and gains only `toggle-mention-event`; it must `return` early for `toggle-enabled` and `mention-input`, which a click on those controls also reaches (`<sl-switch>` and `<sl-input>` both bubble `click`, and `closest("[data-notif-action]")` matches them). A second `container.addEventListener("sl-change", …)` handles exactly those two. Without the split the switch PUTs twice per toggle and the mention field PUTs on every focus click.

The three handlers follow `toggleNotificationEvent`'s established shape exactly: snapshot the previous value, mutate, `await this._saveNotificationsOnly()`, restore on throw, `this.renderNotificationsList()`.

- [ ] **Step 3: Verify**

```bash
timeout 300 node --test web/tests/settings-notifications.test.mjs
timeout 300 node --test web/tests/settings-reorder-budget.test.mjs web/tests/app.test.mjs web/tests/resolution-picker.test.mjs
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/web/routes/ -run DefaultMentionEvents
go build ./...   # re-embeds web/public via go:embed
```
Expected: all `pass`, `fail 0`; `TestDefaultMentionEventsMirroredInSettingsJS` now `ok` (the package is run without `-skip` here). `app.test.mjs` is a rendering **pin** — if it fails, the new markup changed a snapshotted panel and the fixture must be regenerated deliberately per `web/tests/README.md`; a `#settings-network` addition should not touch it. Then the Global gate block.

**Mutations to run:**
1. Let the click listener fall through for `toggle-enabled` → the switch PUTs twice; test 4's failure-revert assertion and the fetch count both break.
2. Drop the `e.stopPropagation()` from the card listeners → test 10 fails on the raised banner.
3. Write `mention_events` when the mention is first typed → test 6 fails.
4. Omit the key instead of writing `[]` when the last mention chip is unticked → test 8 fails.
5. Drop the Test icon button from the card template → test 11 fails.

- [ ] **Step 4: Commit**

Pathspec: `web/public/index.html web/public/modules/settings.js web/tests/settings-notifications.test.mjs`.
Message: `feat(web): per-target mute, mention and mention chips, plus the public URL row` plus the two trailers from Global Constraints, verbatim.

---

## Task 5: The TUI editor — the same controls, and the empty filter accepted

**Files:**
- Modify: `internal/tui/settings.go:117-131` (the Network section's `fields`), `:382-386` (the notification sub-editor state), `:501-509` (`loadValues`), `:600-660` (the save-time validation block), `:801-824` (`applyValues`)
- Modify: `internal/tui/settings_notifications.go:128-208` (`handleNotifEditKey`) and `:46-64` / `:65-74` (the Enter/`a` seeds)
- Modify: `internal/tui/settings_view.go:547-648` (`renderNotifEdit`) and `:496-545` (`renderNotifications`, the list line)
- Modify: `internal/tui/settings_components.go:65-72` and `:171-175` (the text-input arms)
- Modify: `internal/tui/settings_mouse.go:94-109` (the wheel bounds), `:377-394` (`handleMouseNotifClick`'s line map), `:419-449` (`clickNotifEvent`)
- Modify: `internal/tui/settings_notif_click_test.go:14-52` (both tests hard-code `contentY`/`scrollStart` against the pre-N2b row layout; their numbers move with the +2 shift, their contract does not)
- Test: `internal/tui/settings_notifications_test.go` (new)

**The edit-form layout** (focus index → row):

| Focus | Row |
|---|---|
| `0` | Webhook URL (text) |
| `1` | Enabled (toggle, `Space`) |
| `2` | Mention (text) |
| `3 .. 2+len(allNotifEvents)` | one row per event: `[x] Event Name   @` |

`notifEditFocus` keeps meaning "0 = the first text field", so the event index is `notifEditFocus - notifEditEventBase` with `const notifEditEventBase = 3`. Every `flatIdx + 1` in `settings_mouse.go` and `settings_view.go` becomes `flatIdx + notifEditEventBase`, and `total := 1 + len(allNotifEvents)` becomes `notifEditEventBase + len(allNotifEvents)`.

**Why a mention COLUMN and not a second 26-row block.** The event list is already the tallest thing in the overlay and already needs a focus-following scroll window (`settings_view.go:628-644`). A second block of the same 26 rows would double a list an operator navigates with ↑/↓ only, and would need its own scroll mapping in `handleMouseNotifClick`. A second column on the row that already names the event keeps one navigation list, puts an event's two flags side by side where they are read together, and costs the mouse map one extra hit region instead of 26. `Space` toggles the filter checkbox (unchanged); `m` toggles the mention flag on the focused row.

**State:**
```go
	notifEditEnabled       bool
	notifEditMention       string
	notifEditMentionEvents map[string]bool
	// notifEditMentionTouched records whether the operator changed any mention
	// toggle in this editing session. False keeps mention_events ABSENT on
	// save, so the target keeps following the shipped default list instead of
	// freezing today's defaults into their config file.
	notifEditMentionTouched bool
```

**The empty filter is accepted.** `handleNotifEditKey`'s Enter arm currently refuses a zero-event selection (`settings_notifications.go:163-169`) with "Select at least one event". That refusal is removed: an empty selection now stores `Events = nil`, which is "all events" — the same thing the web UI has always meant by it and what `operations.md` documents. The list view's `(N/26 events)` suffix becomes `All events` when the filter is empty, matching the web card's `<sl-tag variant="success">All events</sl-tag>`. An operator who wanted silence uses the Enabled toggle.

- [ ] **Step 1: Write the failing tests**

Create `internal/tui/settings_notifications_test.go`. The package's established pattern is a direct `&SettingsModel{...}` literal driven through its handlers (see `settings_notif_click_test.go`) — no `tea.Program` is needed and none of the existing settings tests uses one.

```go
package tui

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/textinput"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// sectionIndexByName resolves a settings section to its index. internal/tui
// has no such helper today (settings_channels_test.go:242 inlines the loop);
// this is that loop, named once.
func sectionIndexByName(t *testing.T, name string) int {
	t.Helper()
	for i, s := range sections {
		if s.name == name {
			return i
		}
	}
	t.Fatalf("no settings section named %q", name)
	return -1
}

func newNotifEditModel(t *testing.T, existing []config.NotificationConfig) *SettingsModel {
	t.Helper()
	m := &SettingsModel{
		values:        map[string]string{},
		textInput:     textinput.New(),
		notifications: existing,
		notifMode:     "list",
	}
	m.sectionIndex = sectionIndexByName(t, "Integrations")
	return m
}

// newSettingsModelForSave builds a model the pre-save gate can actually run
// against. internal/tui has no such helper either; this mirrors
// settings_save_error_test.go's inline four lines. The configStore is not
// optional: saveAndClose reads PasswordHash through it (settings.go:606)
// before it reaches any field validation, and a nil store panics there.
func newSettingsModelForSave(t *testing.T) *SettingsModel {
	t.Helper()
	cfg := config.Defaults()
	m := NewSettingsModel()
	m.configStore = config.NewStore(cfg, "")
	m.cfg = cfg
	m.OnSave = func(*config.MoomboxConfig) error { return nil }
	m.Open(cfg)
	return m
}

// TestNotifEditAcceptsEmptyFilter removes the last place the two UIs
// disagreed about what an empty event list means. The manager has always read
// a nil/empty filter as "all events" and operations.md has always documented
// that; the TUI's refusal was the outlier, and it left an operator who wanted
// silence deleting the webhook. The mute is the Enabled toggle now.
func TestNotifEditAcceptsEmptyFilter(t *testing.T) {
	m := newNotifEditModel(t, nil)
	m.handleNotifKey("a")
	m.notifEditURL = "discord://1/aaa"
	m.notifEditEvents = map[string]bool{} // every box unticked
	m.handleNotifEditKey(keyEnter)

	if m.status == saveError {
		t.Fatalf("saving an empty filter reported %q — an empty filter means ALL events, not an error", m.errorMsg)
	}
	if len(m.notifications) != 1 {
		t.Fatalf("the target was not saved (%d stored)", len(m.notifications))
	}
	if m.notifications[0].Events != nil {
		t.Errorf("Events = %v, want nil so the manager reads it as all events", m.notifications[0].Events)
	}
}

// TestNotifListShowsAllEventsLabel pins the wording parity with the web card.
func TestNotifListShowsAllEventsLabel(t *testing.T) {
	m := newNotifEditModel(t, []config.NotificationConfig{{URL: "discord://1/aaa"}})
	out := m.renderNotifications(80, 10)
	if !strings.Contains(out, "All events") {
		t.Errorf("the list line for an unfiltered target did not say \"All events\":\n%s", out)
	}
}

// TestNotifEditEnabledToggleRoundTrips: the mute must survive edit → save →
// re-edit, or an operator who opens a muted target and presses Enter silently
// un-mutes it.
func TestNotifEditEnabledToggleRoundTrips(t *testing.T) {
	m := newNotifEditModel(t, []config.NotificationConfig{{URL: "discord://1/aaa"}})
	m.handleNotifKey(keyEnter) // open the editor
	if !m.notifEditEnabled {
		t.Fatal("an existing target with no enabled key opened as disabled")
	}
	m.notifEditFocus = 1
	m.handleNotifEditKey(" ")
	if m.notifEditEnabled {
		t.Fatal("Space on the Enabled row did not toggle it")
	}
	m.handleNotifEditKey(keyEnter)
	if m.notifications[0].IsEnabled() {
		t.Fatal("enabled = false was not saved")
	}

	m.handleNotifKey(keyEnter)
	if m.notifEditEnabled {
		t.Error("re-opening a muted target showed it as enabled — pressing Enter would silently un-mute it")
	}
}

// TestNotifEditMentionDefaultsStayImplicit is the three-way rule at the TUI
// edge. Typing a mention must NOT write mention_events: the target then
// follows the shipped default list, and a later release that adds a key to
// that list reaches them. Writing it out would freeze today's six.
func TestNotifEditMentionDefaultsStayImplicit(t *testing.T) {
	m := newNotifEditModel(t, nil)
	m.handleNotifKey("a")
	m.notifEditURL = "discord://1/aaa"
	m.notifEditMention = "@here"
	m.handleNotifEditKey(keyEnter)

	if m.notifications[0].Mention != "@here" {
		t.Fatalf("mention = %q", m.notifications[0].Mention)
	}
	if m.notifications[0].MentionEvents != nil {
		t.Errorf("mention_events = %v, want absent so the default list keeps applying",
			m.notifications[0].MentionEvents)
	}
}

// TestNotifEditMentionToggleWritesExplicitList: the moment the operator
// disagrees with a default, the whole list becomes explicit — including the
// all-off case, which must be an explicit EMPTY list ("never"), not an absent
// key ("the default six").
func TestNotifEditMentionToggleWritesExplicitList(t *testing.T) {
	m := newNotifEditModel(t, nil)
	m.handleNotifKey("a")
	m.notifEditURL = "discord://1/aaa"
	m.notifEditMention = "@here"
	m.seedMentionDefaults() // what the editor does when a mention is first entered

	// Untick every default.
	for _, e := range config.DefaultMentionEvents() {
		m.notifEditMentionEvents[e] = false
	}
	m.notifEditMentionTouched = true
	m.handleNotifEditKey(keyEnter)

	got := m.notifications[0].MentionEvents
	if got == nil {
		t.Fatal("unticking every mention toggle left mention_events absent — the operator switched the " +
			"pings off and would keep receiving the default six")
	}
	if len(*got) != 0 {
		t.Errorf("mention_events = %v, want an explicit empty list", *got)
	}
}

// TestNotifEditMKeyTogglesTheFocusedEventsMention pins the column's key.
func TestNotifEditMKeyTogglesTheFocusedEventsMention(t *testing.T) {
	first := allNotifEvents[0]
	m := newNotifEditModel(t, nil)
	m.handleNotifKey("a")
	m.notifEditMention = "@here"
	m.seedMentionDefaults()
	m.notifEditFocus = notifEditEventBase // the first event row
	before := m.notifEditMentionEvents[first]
	m.handleNotifEditKey("m")
	if m.notifEditMentionEvents[first] == before {
		t.Errorf("m on the first event row did not toggle its mention flag")
	}
	if !m.notifEditMentionTouched {
		t.Error("toggling a mention flag did not mark the list explicit")
	}
}

// TestHandleMouseNotifClickMapsThroughTheNewRows guards the index shift: the
// edit form gained two rows above the event list, so every hard-coded offset
// in the mouse map had to move with it.
func TestHandleMouseNotifClickMapsThroughTheNewRows(t *testing.T) {
	firstEvent := notifEventGroups[0].events[0]
	m := newNotifEditModel(t, nil)
	m.notifMode = "edit"
	m.notifEditEvents = map[string]bool{}
	m.notifEditMentionEvents = map[string]bool{}
	m.notifEditScrollStart = 0
	// Unscrolled layout: 0 title, 1 URL, 2 Enabled, 3 Mention, 4 blank,
	// 5 "Events", 6 group0 blank, 7 group0 header, 8 group0 event0.
	m.handleMouseNotifClick(8)
	if !m.notifEditEvents[firstEvent] {
		t.Errorf("a click on the first event row (contentY=8) did not toggle %q — the mouse map still "+
			"assumes the pre-N2b row offsets", firstEvent)
	}
}

// TestNotifEditPreservesFieldsTheEditorDoesNotShow: the Enter arm built a
// fresh NotificationConfig from the rows on screen, so editing a webhook's
// URL from the TUI erased every key the form has no row for. N3's `mode` is
// the next one, and it would go the first time anyone fixes a typo.
//
// The assertion below uses Events, the one field the form rebuilds from its
// own checkboxes: the editor is opened and Enter pressed WITHOUT touching the
// event rows, so a copy-then-overwrite keeps the stored filter while a
// rebuild-from-scratch would widen it to "all events". When N3 lands, add a
// `mode` assertion here rather than replacing this one.
func TestNotifEditPreservesFieldsTheEditorDoesNotShow(t *testing.T) {
	m := newNotifEditModel(t, []config.NotificationConfig{
		{URL: "discord://1/aaa", Events: []string{"finished"}},
	})
	m.handleNotifKey(keyEnter)         // open — seeds the checkboxes from Events
	m.notifEditURL = "discord://1/bbb" // the operator only fixed the URL
	m.handleNotifEditKey(keyEnter)

	got := m.notifications[0]
	if got.URL != "discord://1/bbb" {
		t.Fatalf("URL = %q", got.URL)
	}
	if len(got.Events) != 1 || got.Events[0] != "finished" {
		t.Errorf("events = %v — a URL edit widened the filter; the Enter arm rebuilt the target "+
			"instead of copying it", got.Events)
	}
}

// TestNetworkSectionHasPublicURL and its save path.
func TestPublicURLFieldRoundTrips(t *testing.T) {
	found := false
	for _, s := range sections {
		if s.name != "Network" {
			continue
		}
		for _, f := range s.fields {
			if f.key == "public_url" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("the Network section has no public_url row — the web UI offers one and the TUI must too")
	}

	cfg := config.Defaults()
	cfg.Network.PublicURL = "https://x.example"
	m := &SettingsModel{values: map[string]string{}, textInput: textinput.New()}
	m.loadValues(cfg)
	if m.values["public_url"] != "https://x.example" {
		t.Errorf("loadValues did not populate public_url (%q)", m.values["public_url"])
	}
}

// TestSaveRejectsUnusablePublicURL mirrors the trusted_proxies gate: config.Save
// runs Validate and REFUSES a failing config, so without a pre-save check one
// typo makes the whole save fail while saveAndClose still reports "Saved" and
// every other change in that save is lost.
func TestSaveRejectsUnusablePublicURL(t *testing.T) {
	m := newSettingsModelForSave(t) // the package's existing save-test helper
	m.values["public_url"] = "moombox.example.com"
	m.saveAndClose()
	if m.status != saveError {
		t.Fatal("an unusable public_url was accepted; config.Save would then refuse the whole config " +
			"and every other edit in this save would be lost")
	}
	if !strings.Contains(m.errorMsg, "public_url") && !strings.Contains(m.errorMsg, "Public dashboard URL") {
		t.Errorf("the error did not name the field: %q", m.errorMsg)
	}
}
```

> Both helpers are pasted above because `internal/tui` has **neither** today: `settings_save_error_test.go` uses the inline `NewSettingsModel()` + `configStore` + `cfg` + `Open(cfg)` pattern and `settings_channels_test.go:242` inlines the section loop. Before pasting, re-check those two files — if the arc they were written for has since named either helper, reuse it rather than adding a twin. `NewSettingsModel`'s exact constructor signature and the `OnSave` field name must be read from `internal/tui/settings.go` at paste time.

Run:

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/tui/ -run Notif
```

Expected: a build failure naming `m.notifEditEnabled undefined`, `m.seedMentionDefaults undefined`, `undefined: notifEditEventBase`, plus (once those compile) `TestNotifEditAcceptsEmptyFilter` failing with `saving an empty filter reported "Select at least one event (Space to toggle)"`.

- [ ] **Step 2: Implement**

1. `settings.go:117-131` — add `{"public_url", "Public dashboard URL", fieldText, nil, "external address of this dashboard, used only in webhook embeds (e.g. https://moombox.example.com); blank = link to YouTube/Twitch", nil}` after `trusted_proxies`. Not added to `restartRequiredKeys`.
2. `settings.go:509` / `:824` — `loadValues`/`applyValues` for `public_url`; `applyValues` stores `config.ValidatePublicURL`'s canonical form.
3. `settings.go:638` — a pre-save gate beside the `probe_targets` one, calling `config.ValidatePublicURL` and setting `m.errorMsg`/`m.status = saveError`.
4. `settings.go:382-386` — the four new sub-editor fields plus `const notifEditEventBase = 3`.
5. `settings_notifications.go` — seed all four fields in the Enter (`:46-64`) and `a` (`:65-74`) arms; add `seedMentionDefaults()`; rewrite `handleNotifEditKey` for the new focus map, the `Space` arms (Enabled row, event rows) and the `m` arm.

   The Enter arm: drop the zero-event refusal, and **copy the existing target instead of constructing a new one**. Today `:170` does `n := config.NotificationConfig{URL: strings.TrimSpace(m.notifEditURL)}`, so every per-target field the form has no row for is silently dropped by an unrelated TUI save — N3's `mode` would be erased the first time anyone fixes a typo in a webhook URL:
   ```go
   var n config.NotificationConfig
   if m.notifIndex < len(m.notifications) {
       n = m.notifications[m.notifIndex] // keep every field this editor does not show
   }
   n.URL = strings.TrimSpace(m.notifEditURL)
   n.Events = events // nil when the filter is empty
   n.Enabled = …; n.Mention = …; n.MentionEvents = … // the three-way rule
   ```
   The editor overwrites only the five fields it owns.
6. `settings_components.go:65-72` / `:171-175` — the text-input arms become `notifEditFocus == 0` (URL) and `notifEditFocus == 2` (Mention).
7. `settings_view.go` — two new rows in `renderNotifEdit`, the `@` column on each event row, the `flatIdx + notifEditEventBase` shift, and the `All events` list label in `renderNotifications`.
8. `settings_mouse.go` — the wheel bound (`:97`), the `origLine` map (`:385-393`: rows 1/2/3 are URL/Enabled/Mention, events start at `origLine >= 6`), and `clickNotifEvent`'s `flatIdx + notifEditEventBase`.

- [ ] **Step 3: Verify**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/tui/
```
Expected `ok`, with `TestRestartRequiredListsAgree` and `TestHandleMouseNotifClickAccountsForScroll` still passing (the latter's hard-coded `contentY`/`scrollStart` pair moves with the new layout — update its comment and numbers in the same edit, and say in the commit body that its *contract* is unchanged). Then the Global gate block.

**Mutations to run:**
1. Restore the zero-event refusal → `TestNotifEditAcceptsEmptyFilter` fails.
2. Write `MentionEvents` unconditionally when a mention is set → `TestNotifEditMentionDefaultsStayImplicit` fails.
3. Store `nil` when every mention toggle is off → `TestNotifEditMentionToggleWritesExplicitList` fails.
4. Leave one `flatIdx + 1` in `clickNotifEvent` → `TestHandleMouseNotifClickMapsThroughTheNewRows` fails.
5. Restore the fresh `config.NotificationConfig{URL: …}` construction in the Enter arm → `TestNotifEditPreservesFieldsTheEditorDoesNotShow` fails.

- [ ] **Step 4: Commit**

Pathspec: `internal/tui/settings.go internal/tui/settings_notifications.go internal/tui/settings_view.go internal/tui/settings_components.go internal/tui/settings_mouse.go internal/tui/settings_notif_click_test.go internal/tui/settings_notifications_test.go`.
Message: `feat(tui): the three per-target controls, the public URL row, and an empty filter means all events` plus the two trailers from Global Constraints, verbatim.

---

## Task 6: The SPA deep link — `#job=<id>`

**Files:**
- Modify: `web/public/app.js:148` (immediately after the `resize` listener in `initializeApp`, which runs `:141-148` and closes with `}, { passive: true });`) and `:1144` (before `break;` in the `initial_state` case)
- Test: `web/tests/job-deeplink.test.mjs` (new)

**Interfaces:**
```js
/**
 * Open the job named by a `#job=<id>` hash, then clear the hash.
 * Called after initial_state (the jobs list is the thing it searches) and on
 * hashchange. Idempotent: the hash is cleared before the lookup, so a
 * reconnect's second initial_state is a no-op.
 *
 * The clear is `history.replaceState(null, "", location.pathname + location.search)`
 * — jsdom implements it, and unlike assigning location.hash it leaves no
 * history entry for the operator to Back into and re-trigger.
 */
async _consumeJobHash()
```

**Resolution order:** `this.jobs` → `this.archivedJobs` → one `GET /api/jobs/<id>` → toast `"Job not found"`. The API fallback is not in the spec sentence but the feature does not work without it: the embed that carries a deep link is most often "Download Finished", and a finished job older than `hide_finished_age_days` lives in `archivedJobs`, which is fetched lazily (`app.js:256`, `:1301`) and is empty on a cold load. Without the fetch the commonest link in the feature would toast "Job not found". The fallback reuses `_verifyJobExists`'s exact response handling — `resp.ok` → cache into `archivedJobs` and show; anything else → the toast.

- [ ] **Step 1: Write the failing tests**

Create `web/tests/job-deeplink.test.mjs` (jsdom probe copied from `settings-reorder-budget.test.mjs:12-25`). The first test is pasted in full so the harness wiring is not re-derived; the rest are described against it.

```js
const JOB = { id: "j1", title: "A Stream", status: "Finished", platform: "youtube", channel: "c" };

// MUTANT: drop the _consumeJobHash() call from the initial_state case — the
// link in every job embed lands on a dashboard that shows nothing in
// particular, which reads to the operator as a broken link.
test("#job=<id> on load opens that job's details", { skip }, async () => {
  const h = await harness.makeApp({
    url: "http://localhost/#job=j1",
    routes: { "GET /api/jobs/j1": () => JOB },
  });
  h.app.handleMessage({ type: "initial_state", payload: { jobs: [JOB] } });
  await h.flush();
  assert.equal(h.app.selectedJobId, "j1");
  assert.equal(h.el("details-dialog").open, true);
});
```

2. `"the hash is cleared so a reconnect does not reopen the dialog"` — after the above, assert `h.window.location.hash === ""` (the clear is `history.replaceState`, so `location.pathname` is unchanged and no history entry is added), close the dialog, re-send `initial_state`, assert the dialog stayed closed.
3. `"hashchange opens a job without a reconnect"` — dispatch a `hashchange` after setting `location.hash`.
4. `"an archived job reached only through the API still opens"` — `routes: { "GET /api/jobs/j9": () => ARCHIVED_JOB }`, empty `jobs`, assert the dialog opened. Comment: this is the commonest deep link of all — a finished job past the archive boundary.
5. `"an unknown id toasts instead of opening"` — the route answers 404; assert no dialog and that a `sl-alert` carrying `Job not found` was toasted.
6. `"a non-job hash is left alone"` — `#settings` must not be consumed or cleared.

Run:

```bash
timeout 300 node --test web/tests/job-deeplink.test.mjs
```

Expected: `AssertionError [ERR_ASSERTION]: Expected values to be strictly equal: undefined !== 'j1'` on the first test.

- [ ] **Step 2: Implement**

`app.js:146`, immediately after the `resize` listener:

```js
    // Deep link from a notification embed: {public_url}/#job=<id>. The SPA has
    // no job route — details live in a modal — so the hash is the whole
    // mechanism and needs no server change. Handled on hashchange here and
    // once per initial_state below (the jobs list is what it searches, and on
    // a cold load that list arrives with the socket, not with the document).
    window.addEventListener("hashchange", () => this._consumeJobHash());
```

`app.js:1144`, the last statement of the `initial_state` case before `break;`:

```js
        // Safe to re-run on every reconnect: _consumeJobHash clears the hash
        // before it looks anything up, so the second call finds nothing.
        this._consumeJobHash();
```

`_consumeJobHash` itself goes beside `_verifyJobExists` (`app.js:1436`), whose structure it mirrors.

- [ ] **Step 3: Verify**

```bash
timeout 300 node --test web/tests/job-deeplink.test.mjs
timeout 300 node --test web/tests/app.test.mjs web/tests/app-resync.test.mjs web/tests/archive-boundary.test.mjs
go build ./...
```
Expected: all `pass`, `fail 0`. Then the Global gate block.

**Mutations to run:**
1. Clear the hash *after* the lookup → test 2 fails (a reconnect reopens the dialog).
2. Drop the `GET /api/jobs/<id>` fallback → test 4 fails.
3. Match any hash rather than `^#job=` → test 6 fails.

- [ ] **Step 4: Commit**

Pathspec: `web/public/app.js web/tests/job-deeplink.test.mjs`.
Message: `feat(web): open a job's details from a #job=<id> deep link` plus the two trailers from Global Constraints, verbatim.

---

## Task 7: Batching — the coalescing stage in front of the per-target FIFO

**Files:**
- Create: `internal/notifications/batch.go`
- Create: `internal/notifications/batch_test.go`
- Modify: `internal/notifications/manager.go` — the seven touch points the Architecture names: `notificationTarget` (one field), the new `enqueue` adapter method, `buildTargets` (one loop at the end), `Send` (one call site), `Reload` / `Wait` / `BeginShutdown` (one call each)
- Modify: `internal/notifications/target_options_test.go` (Task 2's hand-built `notificationTarget` literals gain a batcher — see step 2b)

**Interfaces** (all in `batch.go`, all unexported except where noted):

```go
// batchWindow is how long a target's coalescing window stays open, measured
// from the FIRST item in it — not a sliding window, so a steady trickle of
// finds cannot hold a message open indefinitely.
const batchWindow = 5 * time.Second

// maxEmbedsPerMessage is Discord's documented per-message embed cap.
const maxEmbedsPerMessage = 10

// embedSpec is one embed's worth of a message: exactly the arguments Send was
// given for a single notification, after filtering and after the mention and
// deep-link decisions.
type embedSpec struct {
	Title       string
	Description string
	Type        NotificationType
	Fields      []Field
	Opts        SendOptions
}

// emitFunc hands ONE message (1..maxEmbedsPerMessage embeds) to a target's
// FIFO queue as a single queue item, so ordering and the drop policy are
// unchanged by batching.
type emitFunc func(msg []embedSpec)

type batchTimer interface{ Stop() bool }

// batchClock is the time source, injected so tests do not sleep.
type batchClock interface {
	AfterFunc(d time.Duration, f func()) batchTimer
}

type realBatchClock struct{}

func (realBatchClock) AfterFunc(d time.Duration, f func()) batchTimer

// batcher coalesces batchable embeds for ONE target.
type batcher struct { /* window, clock, emit, logger, mu, pending, timer */ }

func newBatcher(window time.Duration, clock batchClock, emit emitFunc, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *batcher

// Add routes one send: a non-batchable event is emitted immediately as a
// one-embed message; a batchable one joins the open window, arming it if it
// was closed.
func (b *batcher) Add(spec embedSpec)

// Flush emits every pending embed now. Called from Wait and BeginShutdown.
func (b *batcher) Flush()

// Stop flushes and disarms. Called for every old target on Reload.
func (b *batcher) Stop()

// isBatchable reports whether a send coalesces.
func isBatchable(opts SendOptions) bool

// splitBatch chops pending embeds into messages of at most
// maxEmbedsPerMessage, preserving arrival order.
func splitBatch(pending []embedSpec) [][]embedSpec

// batchIsLowTier reports whether EVERY member of a message is low-tier.
func batchIsLowTier(msg []embedSpec) bool

// batchMentionIndex returns the index of the first mention-eligible member, or
// -1. The message carries that member's Mention/MentionAllowed, so a batch
// pings once if any member would have pinged.
func batchMentionIndex(msg []embedSpec) int
```

Added to `manager.go` (not `batch.go` — it is the one place in N2b that touches N1's queue type):

```go
// enqueue adapts a coalesced message to N1's per-target FIFO: one queue item
// carrying 1..maxEmbedsPerMessage embeds, its tier from batchIsLowTier and its
// mention/allowed_mentions copied from the member batchMentionIndex names.
func (t notificationTarget) enqueue(msg []embedSpec)
```

**The rules, and why each one is where it is:**

| Rule | Where |
|---|---|
| `found`, `added` batch; `auth` batches only with a `JobID` (the per-job "Authentication Required"; the platform-level cookie family carries none and must not wait) | `isBatchable` |
| `error`, `cancelled`, `trim_*`, System and everything else never batch | `isBatchable`'s default |
| Filters apply before coalescing | `Send` — the batcher sits after the event-filter check, so a target never accumulates an embed it would not have sent |
| The window is 5 s from the first item | `Add` arms the timer only when `pending` was empty |
| Overflow rolls into further messages in order | `splitBatch` |
| The batch is ONE queue item, so the FIFO's drop policy and the ordering *between messages* are unchanged | `emitFunc`'s contract; `buildTargets` builds the closure |
| A non-batchable send emitted while a window is open goes out **ahead of** the embeds already coalesced | `Add` — deliberate: an error must not wait 5 s behind a backfill sweep. This is the one ordering change batching makes, so operations.md names it rather than claiming ordering is untouched. For a single job it means an `error` can land before that job's own `found`. |
| `TierLow` only if every member is | `batchIsLowTier`, applied by the emit closure |
| The mention rides once if any member is eligible | `batchMentionIndex`, applied by the emit closure |
| `Wait` / `BeginShutdown` flush open windows | `Manager.Wait` / `Manager.BeginShutdown` call `Flush` on every target before draining |
| Separate-mode targets only | Arc N3 adds `if target.mode == modeEdit { emit directly }` to `Add`'s caller; a comment in `Add` names it |

`Flush` copies `pending` and releases `b.mu` **before** calling `emit` — `emit` enqueues into the FIFO, which takes another lock, and holding both would invert the lock order against `Add`. The `AfterFunc` callback carries the inline `defer recover()`.

- [ ] **Step 1: Write the failing tests**

Create `internal/notifications/batch_test.go` with a fake clock and one test per rule:

```go
package notifications

import (
	"sync"
	"testing"
	"time"
)

// fakeBatchClock arms timers without running them; fire() runs everything
// currently armed. No test in this file sleeps.
type fakeBatchClock struct {
	mu    sync.Mutex
	armed []func()
}

func (c *fakeBatchClock) AfterFunc(_ time.Duration, f func()) batchTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{c: c, idx: len(c.armed)}
	c.armed = append(c.armed, f)
	return t
}

func (c *fakeBatchClock) fire() {
	c.mu.Lock()
	due := c.armed
	c.armed = nil
	c.mu.Unlock()
	for _, f := range due {
		if f != nil {
			f()
		}
	}
}

type fakeTimer struct {
	c   *fakeBatchClock
	idx int
}

func (t *fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	if t.idx < len(t.c.armed) && t.c.armed[t.idx] != nil {
		t.c.armed[t.idx] = nil
		return true
	}
	return false
}

func collector() (*[][]embedSpec, emitFunc) {
	var mu sync.Mutex
	out := &[][]embedSpec{}
	return out, func(msg []embedSpec) {
		mu.Lock()
		defer mu.Unlock()
		*out = append(*out, msg)
	}
}

func found(id string) embedSpec {
	return embedSpec{Title: "Stream Found", Opts: SendOptions{Event: "found", JobID: id, Tier: TierLow}}
}

// TestIsBatchable is the closed set. The auth rows are the subtle ones: the
// per-JOB "Authentication Required" is the burst (one dead cookie parks N
// jobs), while the platform-level cookie family carries no JobID and is
// already cooldown-deduped — making it wait 5 s would delay the only alert
// that tells an operator their credentials are gone.
func TestIsBatchable(t *testing.T) {
	for _, tc := range []struct {
		opts SendOptions
		want bool
		why  string
	}{
		{SendOptions{Event: "found"}, true, "a backfill sweep is the burst this exists for"},
		{SendOptions{Event: "added"}, true, "a bulk add is the same shape"},
		{SendOptions{Event: "auth", JobID: "j1"}, true, "one dead cookie parks N jobs"},
		{SendOptions{Event: "auth"}, false, "the platform-level cookie alerts must not be delayed"},
		{SendOptions{Event: "error", JobID: "j1"}, false, "a failure goes out at once"},
		{SendOptions{Event: "cancelled"}, false, ""},
		{SendOptions{Event: "finished"}, false, ""},
		{SendOptions{Event: "disk_critical"}, false, ""},
		{SendOptions{Event: "trim_created"}, false, ""},
		{SendOptions{Event: ""}, false, "an unfiltered send (SendTest) is never batched"},
	} {
		if got := isBatchable(tc.opts); got != tc.want {
			t.Errorf("isBatchable(%+v) = %v, want %v — %s", tc.opts, got, tc.want, tc.why)
		}
	}
}

// TestBatcherCoalescesOneWindow: three finds inside one window leave the
// batcher as ONE message, and nothing is emitted before the window closes.
func TestBatcherCoalescesOneWindow(t *testing.T) {
	got, emit := collector()
	clk := &fakeBatchClock{}
	b := newBatcher(batchWindow, clk, emit, testLogger{})
	b.Add(found("a"))
	b.Add(found("b"))
	b.Add(found("c"))
	if len(*got) != 0 {
		t.Fatalf("emitted %d messages before the window closed", len(*got))
	}
	clk.fire()
	if len(*got) != 1 {
		t.Fatalf("emitted %d messages, want 1", len(*got))
	}
	if len((*got)[0]) != 3 {
		t.Errorf("the message carried %d embeds, want 3", len((*got)[0]))
	}
}

// TestBatcherSplitsAtTen is Discord's documented per-message embed cap:
// overflow rolls into further messages, in order, and nothing is dropped —
// the whole point of batching a backfill sweep is that the sweep stops LOSING
// notifications.
func TestBatcherSplitsAtTen(t *testing.T) {
	got, emit := collector()
	clk := &fakeBatchClock{}
	b := newBatcher(batchWindow, clk, emit, testLogger{})
	for i := 0; i < 23; i++ {
		b.Add(found(string(rune('a' + i))))
	}
	clk.fire()
	if len(*got) != 3 {
		t.Fatalf("emitted %d messages for 23 embeds, want 3", len(*got))
	}
	if n := []int{len((*got)[0]), len((*got)[1]), len((*got)[2])}; n[0] != 10 || n[1] != 10 || n[2] != 3 {
		t.Errorf("message sizes %v, want [10 10 3]", n)
	}
	if (*got)[0][0].Opts.JobID != "a" || (*got)[2][2].Opts.JobID != string(rune('a'+22)) {
		t.Error("arrival order was not preserved across the split")
	}
}

// TestBatcherPassesNonBatchableThrough: an error never waits behind an open
// find window.
func TestBatcherPassesNonBatchableThrough(t *testing.T) {
	got, emit := collector()
	clk := &fakeBatchClock{}
	b := newBatcher(batchWindow, clk, emit, testLogger{})
	b.Add(found("a"))
	b.Add(embedSpec{Title: "Job Failed", Opts: SendOptions{Event: "error", JobID: "j9"}})
	if len(*got) != 1 || len((*got)[0]) != 1 || (*got)[0][0].Title != "Job Failed" {
		t.Fatalf("the error did not go out immediately: %v", *got)
	}
	clk.fire()
	if len(*got) != 2 {
		t.Fatalf("the find window did not close afterwards: %d messages", len(*got))
	}
}

// TestBatchIsLowTier: one important embed protects the whole message from the
// queue's drop-oldest-low-tier policy.
func TestBatchIsLowTier(t *testing.T) {
	low := []embedSpec{found("a"), found("b")}
	if !batchIsLowTier(low) {
		t.Error("an all-low batch is not low-tier")
	}
	mixed := []embedSpec{found("a"), {Opts: SendOptions{Event: "auth", JobID: "j1"}}}
	if batchIsLowTier(mixed) {
		t.Error("a batch containing a normal-tier member was marked low-tier — the queue would drop it " +
			"under pressure ahead of a lone Stream Found")
	}
	if batchIsLowTier(nil) {
		t.Error("an empty message must not be reported low-tier")
	}
}

// TestBatchMentionIndex: the message pings once if any member would have.
func TestBatchMentionIndex(t *testing.T) {
	none := []embedSpec{found("a"), found("b")}
	if got := batchMentionIndex(none); got != -1 {
		t.Errorf("batchMentionIndex = %d, want -1", got)
	}
	withOne := []embedSpec{
		found("a"),
		{Opts: SendOptions{Event: "auth", JobID: "j1", Mention: "@here"}},
		{Opts: SendOptions{Event: "auth", JobID: "j2", Mention: "@here"}},
	}
	if got := batchMentionIndex(withOne); got != 1 {
		t.Errorf("batchMentionIndex = %d, want 1 (the FIRST eligible member, so the message pings once)", got)
	}
}

// TestBatcherFlushOnShutdown: a window open when shutdown begins must go out,
// not evaporate.
func TestBatcherFlushOnShutdown(t *testing.T) {
	got, emit := collector()
	clk := &fakeBatchClock{}
	b := newBatcher(batchWindow, clk, emit, testLogger{})
	b.Add(found("a"))
	b.Flush()
	if len(*got) != 1 {
		t.Fatalf("Flush emitted %d messages, want 1", len(*got))
	}
	clk.fire() // the disarmed timer must not double-emit
	if len(*got) != 1 {
		t.Errorf("the timer fired after Flush and emitted the batch twice")
	}
}

// TestBatcherWindowIsNotSliding: a steady trickle must not hold the window
// open forever. The timer is armed by the FIRST item only.
func TestBatcherWindowIsNotSliding(t *testing.T) {
	clk := &fakeBatchClock{}
	_, emit := collector()
	b := newBatcher(batchWindow, clk, emit, testLogger{})
	b.Add(found("a"))
	b.Add(found("b"))
	clk.mu.Lock()
	armed := len(clk.armed)
	clk.mu.Unlock()
	if armed != 1 {
		t.Errorf("%d timers armed for two items in one window, want 1 — a re-armed timer is a sliding "+
			"window and a trickle would never flush", armed)
	}
}
```

Plus the two manager-level tests that exercise the `Send` → batcher seam, and the helper Task 2's literals need:

```go
// immediateBatchClock never delays: AfterFunc runs its callback on the spot,
// so a batcher built with it behaves like no batcher at all. It is what
// target_options_test.go's hand-built targets use (see step 2b) — those tests
// are about mentions and deep links, not windows.
type immediateBatchClock struct{}

func (immediateBatchClock) AfterFunc(_ time.Duration, f func()) batchTimer {
	f()
	return noopTimer{}
}

type noopTimer struct{}

func (noopTimer) Stop() bool { return false }

// TestSendBatchesThroughTheTarget is the Send→batcher seam: three finds
// become ONE queue item and an error jumps the open window.
func TestSendBatchesThroughTheTarget(t *testing.T) {
	got, emit := collector()
	clk := &fakeBatchClock{}
	tg := notificationTarget{sender: senderFunc(func(string, string, int, []Field, SendOptions) error { return nil })}
	tg.batch = newBatcher(batchWindow, clk, emit, testLogger{})
	m := &Manager{logger: testLogger{}, targets: []notificationTarget{tg}}

	m.Send("Stream Found", "d", TypeInfo, nil, SendOptions{Event: "found", JobID: "a"})
	m.Send("Stream Found", "d", TypeInfo, nil, SendOptions{Event: "found", JobID: "b"})
	m.Send("Job Failed", "d", TypeError, nil, SendOptions{Event: "error", JobID: "c"})
	if len(*got) != 1 || (*got)[0][0].Title != "Job Failed" {
		t.Fatalf("the error did not jump the open find window: %v", *got)
	}
	clk.fire()
	if len(*got) != 2 || len((*got)[1]) != 2 {
		t.Errorf("the two finds did not close as one 2-embed message: %v", *got)
	}
}

// TestFilteredEventNeverEntersTheBatch: filters run BEFORE coalescing, so a
// target that does not subscribe to `found` never accumulates one — a batch
// must never deliver an embed the allowlist excluded.
func TestFilteredEventNeverEntersTheBatch(t *testing.T) {
	got, emit := collector()
	clk := &fakeBatchClock{}
	tg := notificationTarget{
		sender: senderFunc(func(string, string, int, []Field, SendOptions) error { return nil }),
		events: map[string]bool{"error": true},
	}
	tg.batch = newBatcher(batchWindow, clk, emit, testLogger{})
	m := &Manager{logger: testLogger{}, targets: []notificationTarget{tg}}

	m.Send("Stream Found", "d", TypeInfo, nil, SendOptions{Event: "found", JobID: "a"})
	clk.fire()
	if len(*got) != 0 {
		t.Errorf("a filtered-out event reached the batch: %v", *got)
	}
}
```

Run:

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/notifications/ -run Batch
```
Expected: `internal/notifications/batch_test.go:NN:NN: undefined: newBatcher` … `FAIL … [build failed]`.

- [ ] **Step 2: Implement**

`batch.go` per the interfaces above. Then, in `manager.go`:

1. `notificationTarget` gains `batch *batcher`.
2. `buildTargets`, as its **last** statement before `return targets`:
   ```go
   	// The coalescing stage sits in front of each target's FIFO queue: a burst
   	// of finds becomes ONE queue item carrying up to ten embeds, so the
   	// queue's ordering and its drop policy are unchanged by batching.
   	for i := range targets {
   		targets[i].batch = newBatcher(batchWindow, realBatchClock{}, targets[i].enqueue, logger)
   	}
   ```
   (`enqueue` is **new in this arc**, added to `manager.go` beside `notificationTarget`: it adapts `[]embedSpec` — a type this arc creates, which N1 knows nothing about — to whatever queue-item constructor N1 provides, taking the item's tier from `batchIsLowTier(msg)` and its mention/`allowed_mentions` from the member `batchMentionIndex(msg)` names. It is the only place in N2b that touches N1's queue type, which is what keeps adaptation point 3 to one expression.)

   **2b.** Task 2's `target_options_test.go` builds `notificationTarget` literals by hand, so `batch` is nil there and `Send`/`Wait` would panic the moment this task lands — Task 7's own `-race` verify step is what would discover it. Add a helper in that file and wrap every literal with it:
   ```go
   func withBatch(t notificationTarget) notificationTarget {
       t.batch = newBatcher(batchWindow, immediateBatchClock{}, t.enqueue, testLogger{})
       return t
   }
   ```
   Do **not** nil-guard `target.batch` in `Send`: a target without a batcher is a construction bug, and a guard would hide it in production.
3. `Send`'s per-target dispatch becomes `target.batch.Add(spec)` where `spec` is the `embedSpec` built from the (already rewritten, already mention-decided) arguments. **This is the one merge-conflict surface with Arc N1.**
4. `Reload`: read the outgoing slice under the **same** `targetsMu.Lock()` that swaps it (`old := m.targets` before the assignment), then call `Stop()` on each **after** `Unlock()` — `Stop` flushes, a flush enqueues, and holding `targetsMu` across a queue lock is the inversion this task already guards against inside `Flush`. A surviving target's flush lands in the queue it keeps; a removed target's flush lands in the queue N1 then discards with its own Warn, so nothing new is lost and the code needs no special case.
5. `Wait` and `BeginShutdown`: snapshot the target slice under `targetsMu.RLock()`, release, then `Flush` each batcher before draining — same lock-order rule as item 4 — so an open window is delivered rather than dropped.

- [ ] **Step 3: Verify**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -race ./internal/notifications/
```
`-race` here specifically: the batcher is the only new concurrency in the arc. Expected `ok`. Then the Global gate block.

**Mutations to run:**
1. Re-arm the timer on every `Add` → `TestBatcherWindowIsNotSliding` fails.
2. Make `splitBatch` return one message regardless of size → `TestBatcherSplitsAtTen` fails.
3. Make `batchIsLowTier` return true when *any* member is low → `TestBatchIsLowTier` fails.
4. Batch `auth` regardless of `JobID` → `TestIsBatchable` fails.
5. Skip the `timer.Stop()` in `Flush` → `TestBatcherFlushOnShutdown` fails on the double emit.
6. Route batchable sends past the filter check → `TestFilteredEventNeverEntersTheBatch` fails.
7. Hold `targetsMu` across the `Stop()` loop in `Reload` → the `-race` run deadlocks or reports the inverted acquisition.

- [ ] **Step 4: Commit**

Pathspec: `internal/notifications/batch.go internal/notifications/batch_test.go internal/notifications/manager.go internal/notifications/target_options_test.go`.
Message: `feat(notifications): coalesce found/added/per-job auth into one message` plus the two trailers from Global Constraints.

---

## Task 8: Docs and the node README recount

**Files:**
- Modify: `docs/spec/operations.md:442-527` (the Notifications section: Configuration, the event table's preamble, Dispatch Behavior)
- Modify: `docs/spec/user-interfaces.md:229` (the `settings_notifications.go` row) and the Settings rows near `:221` and `:376`; add the deep link beside the `/api/jobs` surface notes
- Modify: `README.md:673-683` (the Webhook Notifications section)
- Modify: `web/tests/README.md` — the "Sixteen suites" prose (`:4`) and the per-suite table (`:35`), the inline per-suite DOM breakdown (`:52-58`), the fenced without-jsdom count block (`:65-70`) and the with-jsdom prose sentence (`:72-73`)
- No code changes.

**What goes where:**

`operations.md` — a new `### Target Options` subsection after `### URL Formats` covering `enabled` (mute, kept in config, `NewManager` logs the skip, and `HasTargets` reports false when **every** target is muted, which correctly short-circuits all four producer guards: `cmd/moombox/helpers.go` update-available, `cmd/moombox/main.go` disk, and both `cmd/moombox/monitor_callbacks.go` Stream Found sites), `mention` + `mention_events` (the four accepted forms, the `allowed_mentions` each produces, the alias rule, the three-way default/never/explicit semantics with the exact default list), and `network.public_url` (what the title link becomes, where the platform link goes, that it is read at send time so a save applies without a restart, and that it does nothing when a send carries no author). A new `### Batching` subsection: which events coalesce, the 5 s non-sliding window, ten embeds per message with overflow rolling forward, that a batch is one queue item **so the FIFO's drop policy and the ordering between messages are unchanged — with the one exception that a non-batchable send (an error, a cancel) arriving while a window is open is delivered immediately, ahead of the embeds still coalescing**, that a batch is low-tier only if every member is, that it pings once, and that `Wait`/`BeginShutdown` flush. Amend the "empty filter = all events" sentence in the event-table preamble to say both UIs now agree and name the mute.

`user-interfaces.md` — the `settings_notifications.go` row gains the three controls and the `m` key; the Settings paragraph gains the `public_url` row; a short paragraph on the `#job=<id>` deep link (what produces it, that the SPA clears the hash, that an unknown id toasts).

`README.md` — replace the `[[notifications]]` example and the wrong inline event list with a current example showing `enabled`/`mention`/`mention_events` and a pointer to the operations table, plus a short "Mentions and dashboard links" paragraph. (`README.md:67`'s "Discord, Slack, ntfy" line is **Arc N1's** fix — leave it alone to avoid a merge collision.)

**Citation gate.** Every backticked symbol added here must resolve: `PublicURL`, `NotificationConfig`, `IsEnabled`, `ParseMention`, `DefaultMentionEvents`, `ResolveMentionEvents`, `ValidatePublicURL`, `JobDeepLink`, `mentionFor`, `AllowedMentions`, `isBatchable`, `batchWindow`, `internal/notifications/batch.go`, `web/public/modules/settings.js`, `internal/tui/settings_notifications.go`. Each symbol must appear next to its **declaring** file path through one of the connectors the gate accepts (``Foo`` (`internal/…/x.go`), ``Foo`` in `internal/…/x.go`, …) — a citation pointing at a *caller* rather than the declaration fails exactly the way a missing symbol does (`identRe` + `connectorRe` + `fileFacts.declared`, `internal/docs/citations_test.go`). Unexported names (`mentionFor`, `isBatchable`, `batchWindow`) are fine under that rule; a bare backticked `isBatchable` with no nearby `internal/notifications/batch.go` is not. Prefer symbol names over line numbers; `TestSpecDocAbsenceClaimsHold` means any "there is no X" sentence must still be true after this arc.

- [ ] **Step 1: Recount the node suite (no arithmetic)**

```bash
cd D:/Git/Moombox/web/tests && command ls node_modules/jsdom >/dev/null 2>&1 && echo "jsdom present" || echo "jsdom absent"
cd D:/Git/Moombox && timeout 300 node --test web/tests/*.test.mjs 2>&1 | tail -20
```
Record the `tests` / `pass` / `fail` / `skipped` line **with** jsdom. Then move `web/tests/node_modules` aside, re-run, record the **without**-jsdom numbers, and move it back. Baseline at `1d2df1d4`: `tests 291 / pass 131 / skipped 160` without jsdom, `291 / 291 / 0` with. Confirm that baseline reproduces before trusting the new numbers. Also record the per-suite counts for the two new files (`settings-notifications.test.mjs`, `job-deeplink.test.mjs`) from `--test-reporter=spec`, since the README's breakdown lists them individually.

- [ ] **Step 2: Write the docs**

Both new suites need jsdom, so they join the "yes" row of `web/tests/README.md`'s table (`:35`), the prose count at `:4` ("Sixteen suites" → the live number; the directory holds 24 test files today and 26 after this arc, 18 of them jsdom suites), and the inline DOM-test breakdown at `:52-58`. The fenced block at `:65-70` and the prose sentence at `:72-73` take the two recounted totals.

- [ ] **Step 3: Verify**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/docs/
```
Expected `ok  github.com/vampiricwulf/Moombox/internal/docs`. A failure names the exact unresolvable citation — fix the doc, never the allowlist.

- [ ] **Step 4: Commit**

Pathspec: `docs/spec/operations.md docs/spec/user-interfaces.md README.md web/tests/README.md`.
Message: `docs: target options, batching, the deep link, and a recounted node suite` plus the two trailers from Global Constraints, verbatim.

---

## Task 9: Full gates, self-review, and plan deletion

**Files:**
- Delete: `docs/superpowers/plans/2026-09-27-webhooks-n2b-config-batching.md` (this file)

- [ ] **Step 1: Run every gate**

```bash
gofmt -l ./cmd ./internal ./tools ./web
go vet ./...
go mod tidy -diff
go install honnef.co/go/tools/cmd/staticcheck@2026.2.1 && staticcheck ./...
go build ./...
GOOS=linux GOARCH=amd64 go build ./...
GOOS=linux GOARCH=arm64 go build ./...
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 \
  ./internal/config/ ./internal/notifications/ ./internal/web/routes/ ./internal/tui/ ./internal/docs/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -race ./internal/notifications/
timeout 300 node --test web/tests/*.test.mjs
```

Expected: `gofmt`, `go mod tidy -diff` and `staticcheck` print nothing; all three builds succeed; five `ok` lines; the node run's totals match what Task 8 wrote into `web/tests/README.md`.

Then the same node run **without** jsdom, confirming the second set of README numbers.

- [ ] **Step 2: Self-review against the spec**

Walk `docs/superpowers/specs/2026-09-27-discord-webhooks-design.md` §3 items 1-7 and tick each against the diff:

1. Four keys, defaults, `validateOrNormalize`, `config.example.toml`, `data-and-storage.md`, the settings skill — Task 1. `mode` deliberately absent (N3).
2. Mentions in the payload per form; a batch pings once — Tasks 2 and 7.
3. Dashboard link rewritten at send time from the live config; the SPA opens and clears — Tasks 2, 3, 6.
4. Batching: separate-mode only, the five excluded classes, filters first, one queue item, tier rule, shutdown flush — Task 7.
5. Both editors: `enabled`, `mention`, `mention_events`, the "All events" label, the `public_url` row, test-send unchanged — Tasks 4 and 5.
6. Tests: validators, mention payload, default-list rule, batching, deep link, both round-trips, README recount — Tasks 1-8.
7. Docs — Task 8.

Two deliberate deviations to record rather than re-litigate: spec §3.6 asks for "TUI a `tea` test" and Task 5 uses direct `&SettingsModel{}` literals instead (no existing settings test in `internal/tui` drives a `tea.Program`, and the handlers under test are pure model methods); and Task 6's `GET /api/jobs/<id>` fallback exceeds the spec sentence for the reason Task 6 states.

Then the four mechanical checks:

- **No placeholders** — `grep -rn "TODO\|FIXME\|XXX\|<placeholder>"` over the diff must return nothing new.
- **Every test named in this plan exists in the tree** — the grep above cannot see a *missing* test. Walk each task's Step 1 and confirm the named function is present and running (not skipped): `TestMentionAllowedPerForm`, `TestNotificationsApplyUsesTheSharedDecode`, `TestNotifEditPreservesFieldsTheEditorDoesNotShow`, `TestSendBatchesThroughTheTarget`, `TestFilteredEventNeverEntersTheBatch`, and the eleven jsdom tests of Task 4 plus the six of Task 6.
- **Type consistency** — `MentionEvents` is `*[]string` at every layer (struct, route decode, both editors, both round-trip tests); `Enabled` is `*bool` with `IsEnabled()` the only reader; `MentionAllowed` is `*AllowedMentions` at every layer, built once in `buildTargets` and never mutated after.
- **The N1 adaptation points** — confirm all three named at the top of this plan were resolved against the merged tree, and that `internal/notifications/manager.go` carries exactly the **seven** touch points the Architecture names: the `notificationTarget.batch` field, the `enqueue` adapter method, and the five call sites in `buildTargets`, `Send`, `Reload`, `Wait` and `BeginShutdown`. More than seven means batching leaked into the delivery core; fewer means a flush path was missed.

- [ ] **Step 3: Delete this plan and commit**

Per the standing rule that an implemented plan is deleted (git history is the archive):

```bash
git rm docs/superpowers/plans/2026-09-27-webhooks-n2b-config-batching.md
git commit -m "chore(plans): remove the implemented Arc N2b plan

Arc N2b is complete: config keys, mentions, the dashboard deep link,
batching and both editors. git history is the archive.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" \
        -- docs/superpowers/plans/2026-09-27-webhooks-n2b-config-batching.md
```
