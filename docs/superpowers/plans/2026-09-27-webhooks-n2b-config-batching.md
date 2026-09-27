# Arc N2b — config, mentions, dashboard link, batching, both UIs: Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give notification targets the three per-target keys the owner ruled for (`enabled`, `mention`, `mention_events`) plus the install-wide `network.public_url`, make the delivery core act on them (skip disabled targets, attach `content` + `allowed_mentions`, rewrite a job embed's title link to the dashboard), coalesce burst events into one message of up to ten embeds, and expose all of it in both Settings editors and the SPA's `#job=<id>` deep link.

**Architecture:** Four config keys enter through `internal/config` (struct → `Defaults()` → `validateOrNormalize`), reach the manager the way every other notification setting already does (`buildTargets` at `NewManager`/`Reload`), and are edited from both UIs through the existing `PUT /api/config` merge. Two behaviours are added to the delivery core and both are read from the target record the manager already keeps: a mention decision (`mentionFor`) evaluated per send, and a public-URL rewrite (`JobDeepLink`) applied to any send carrying a `JobID`. Delivery needs two changes to N1's core, in order. **Task 7** generalises the single-embed path into a `Message` carrying `[]Embed` — `sender`, `buildPayload`, `queued` and `Manager.Send` all move to it, and a one-embed `Message` produces the byte-identical payload N1 produced. **Task 8** then adds the coalescing stage (`internal/notifications/batch.go`) in front of each target's FIFO: it owns the 5 s window, the split (on both Discord per-message caps — ten embeds and 6000 characters) and the per-message tier/mention rules. Because `Manager.targets` is `[]*targetQueue` and `notificationTarget` is a build-time value `applyTargets` discards, every per-target addition in this arc — the mention fields and the batcher — lives on **`targetQueue`**, created in `newTargetQueue` and retired in `applyTargets`' existing `retired` loop. The per-target surface Tasks 2 and 8 add to `manager.go`/`queue.go` is **seven** places, stated once here and checked by Task 10: (1) `targetQueue`'s four new fields (three for the mention, one for the batcher), (2) `newTargetQueue` copying and creating them, (3) the new `mentionFor`/`setMention` methods, (4) `applyTargets`' survivor arm calling `setMention`, (5) its `retired` loop flushing the batcher beside `stopDiscard`, (6) `Send`'s per-item mention write and batcher entry, (7) `Wait`/`BeginShutdown` flushing before `closeDrain`. `Manager` itself gains exactly one field, `publicURL`. The SPA gains a hash consumer in `app.js`; nothing on the server routes it.

**Tech Stack:** Go 1.27, `BurntSushi/toml` (three-way present/empty/absent encoding via `*[]string`), chi/v5 REST, vanilla-JS dashboard embedded via `go:embed` (Shoelace v2.16), Bubble Tea TUI (`charm.land/bubbles/v2`), `node:test` + jsdom for the frontend suites.

**Spec:** `docs/superpowers/specs/2026-09-27-discord-webhooks-design.md` — §0 rulings (Q5 mentions, Q6 dashboard link, Q3 batching, the "empty filter means ALL events / mute with `enabled = false`" ruling), §3 Arc N2b, §5 order. Committed at `1d2df1d4`. Supporting audit (gitignored): `.superpowers/sdd/2026-09-26-webhooks/audit.md` §3 Batching + Content quality, §4 seams.

---

## What Arc N1 actually built

Arc N1 is finished at `242d921c` (branch `webhooks-n1`; `main` becomes that tree plus a merge commit). This plan was first written against `main` @ `1d2df1d4` with N1 unmerged, and has been rewritten against N1's real code. Export it read-only before starting — `git archive 242d921c | tar -x -C <scratch>/n1tree` from the worktree, never touching the worktree or `main`.

| N1 symbol (verified at `242d921c`) | How N2b uses it |
|---|---|
| `SendOptions.Mention` (string) and `SendOptions.MentionAllowed` (`*AllowedMentions`) | Task 2 fills both, per target per event. |
| `AllowedMentions{Parse, Roles, Users}` (`internal/notifications/manager.go`) — `Parse` is `json:"parse"` **without** omitempty and is non-nil on every sent object | Task 2 never builds one by hand. |
| `MentionParse(mention string) *AllowedMentions` (`internal/notifications/discord.go`) — N1 wrote it for this arc, its doc comment says so, and it handles `<@!id>` itself | Task 2 calls it. Building the object from a form/id pair instead produces `Parse: nil`, which is a different object and a different wire form. |
| `SendOptions.Tier`, `TierUnset`/`TierNormal`/`TierLow`, `effectiveTier(opts)` and `lowTierEvents` (`internal/notifications/manager.go`) | Task 8's `batchIsLowTier` calls `effectiveTier`. `TierUnset` is the zero value — there is no sentinel to invent. |
| `SendOptions.JobID` | Tasks 2 and 8 key the deep-link rewrite and the per-job `auth` batching rule on it. |
| `SendOptions.Author` (`*Author{Name, IconURL, URL}`); `buildPayload` drops an author with an empty `Name` | Task 2 moves the platform link onto it, and only when one exists. |
| `Manager.targets []*targetQueue` + `byKey map[string]*targetQueue`; `notificationTarget{sender, events, key}` is a **build-time value** `applyTargets` consumes and discards | Tasks 2 and 8 put their per-target state on **`*targetQueue`**, which is the only object `Send` sees. |
| `applyTargets(built []notificationTarget)` (`internal/notifications/manager.go`) — the one swap path, shared by `NewManager` and `Reload`: survivors keep their queue via `q.setEvents`, new ones get `newTargetQueue` + `go q.run()`, retired ones get `q.stopDiscard()` outside the lock | Task 2 adds `q.setMention(t)` to the survivor arm; Task 8 retires batchers in the `retired` loop. |
| `targetQueue` + `newTargetQueue(t, logger, shuttingDown)`, `enqueue(it queued)`, `allows`, `setEvents`, `pop`, `deliver`, `closeDrain`, `stopDiscard`, `notificationQueueCap` (`internal/notifications/queue.go`) | Task 8 creates the batcher in `newTargetQueue` and flushes before `closeDrain`. |
| `queued{title, description, color, fields, opts, tier}` and `sender{Send, SendOnce}` — **both strictly one embed**; `buildPayload` ends `Embeds: []discordEmbed{embed}` | **Task 7 replaces this with a `Message`.** See below. |
| `ClampRunes`, `EscapeMarkdown`, `clampEmbed`, `embedRunes`, `limitTotal = 6000` (`internal/notifications/limits.go`) — the 6000 budget is applied per EMBED | Tasks 7 and 8 reuse `embedRunes`/`limitTotal` for the per-MESSAGE budget rather than inventing a second number. |
| `eventAliases` (`internal/notifications/events.go`) — now `disk_critical → disk_warning` **and** `connectivity_resume → connectivity_pause`; `KnownEvents` includes alias values | Task 2's `mentionFor` reuses the same alias rule, so both entries apply. |
| `newTestManager(t, waitTimeout, targets ...notificationTarget)` (`internal/notifications/manager_test.go`) — routes through `applyTargets`, the only path that creates a queue and starts its goroutine | Every Manager in this plan's tests is built through it. A hand-built `&Manager{targets: []notificationTarget{…}}` is a type error. |
| `notificationtest.Recorder` implements `Notifier` (the `Sender`-level surface), not the internal `sender` | Unaffected by Task 7's seam. No edit, and no need to go looking. |

**The one design gap N1 left, and how this plan closes it.** The spec's batching ruling needs one message carrying up to ten embeds. N1's delivery path is strictly single-embed at four places — `queued`, the `sender` interface, `targetQueue.deliver`, and `buildPayload`'s hard-coded `[]discordEmbed{embed}`. That is not an adaptation expression; it is a seam, and it is **Task 7**, placed before batching so Task 8 has something to hand a batch to. Controller ruling B-1 fixes its shape: a `Message` value carrying `[]Embed` plus the message-level `Mention`/`MentionAllowed`, with `sender` and `buildPayload` taking it, `queued` carrying one, `Manager.Send` wrapping a single embed into a one-embed `Message`, and N1's four test fakes adapting — proven byte-identical for the one-embed case.

**Two things to re-verify before starting**, both one expression:

1. **`main`'s merge commit.** This plan's `internal/notifications` anchors are named by symbol and were read at `242d921c`. If the merge resolved anything differently, the symbols are still the contract.
2. **`config.ParseMention` vs `notifications.MentionParse`.** They are different jobs and both exist by the end of Task 1: config **validates and canonicalises** the operator's string (the vocabulary cannot live in `internal/notifications` from config's side — the import runs the other way), and `MentionParse` turns the canonical string into the wire object. Task 2 calls both, in that order, and never reimplements the second.

Every **config, route, `app.js`, `index.html`, `config.example.toml`, `data-and-storage.md`, `user-interfaces.md` and TUI line number in this plan was verified at `1d2df1d4` and re-verified byte-identical at `242d921c`**. The one exception is `web/public/modules/settings.js`, from which N1 removed the `connectivity_pause` mirror entry at old `:41` — **every anchor in that file below line 41 shifts by −1**, and Task 4 already carries the shifted numbers.

**N2a ordering note.** `config.defaultMentionEvents` lists `sidecar_down`, a key **N2a** adds. Before N2a merges, `buildTargets` resolves the default list for every target that has a `mention` and no explicit `mention_events`, so each such target logs one `notification target filters on unknown event` Warn for `sidecar_down` at every config load. That is the accepted cost (controller ruling 7, same wave) and it disappears when N2a lands with no edit here — do not special-case the key. Neither UI can offer it in the meantime, because both derive their chips from the live vocabulary. Likewise the deep-link rewrite only fires for sends that carry an `Author`, which N2a's builders give every job send — before N2a it applies to whichever sites already set one.

---

## Global Constraints

Every task's requirements implicitly include this section.

### From the spec (§3 Arc N2b Constraints)

- **No producer changes.** Nothing under `internal/worker/`, `cmd/moombox/monitor_callbacks.go`, `cmd/moombox/main.go`, `cmd/moombox/addvideo.go` or `internal/web/routes/jobs.go` is edited — those are N2a's. If a test needs a producer's behaviour, use a fake target, not a producer.
- **The config-file-only key class is untouched.** `downloader.progress_interval_ms` and `cookies.dpapi_profile_dir` keep their no-UI status; nothing is added to or removed from that class.
- **`PUT /api/config` merges per key as today.** An absent section stays as stored. No key added here becomes mandatory in a payload.
- **The restart-required list stays at 16.** All four new keys hot-reload. `RESTART_REQUIRED_FIELDS` (`web/public/modules/settings.js`) and `restartRequiredKeys` (`internal/tui/settings.go`) are not edited; `TestRestartRequiredListsAgree` must stay green unchanged.
- **No `mode` key.** `mode = "separate" | "edit"` is Arc N3's. Batching is written as if every target is separate-mode, with the one-line gate N3 will add named in Task 8's comments.

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
  Never extracted to a named interface. The new `batcher` (Task 8) carries its own copy.
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

`staticcheck ./...` (pinned `honnef.co/go/tools/cmd/staticcheck@2026.2.1`) runs in Task 10 only — it is slow and nothing earlier can regress it without also failing `go vet`.

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

**`config.example.toml`** — add the commented `public_url` to `[network]` (after the `trusted_proxies` example, `:50`) and rewrite the `[[notifications]]` block (`:302-322`) to document `enabled`, `mention`, `mention_events` and the empty-filter rule. While there, fix the block's two stale lines that predate this arc and now sit directly beside new, accurate text: the "Works with Discord, Slack, ntfy" claim (`parseTarget` is Discord-only by design) and the event list naming a `live` event that does not exist while omitting 16 that do (the block names 10 keys, 9 of them real, out of the vocabulary's **25** — N1 retired `connectivity_pause` from `EventGroups`, keeping it alive only through `eventAliases`). Replace the inline list with a pointer to `docs/spec/operations.md`'s table, and drop the `tags = [...]` example at `:322` for a field removed in 2026-07.

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
- Modify: `internal/notifications/manager.go` — `notificationTarget` (symbol, three new fields), `Manager` (symbol, the new `publicURL` field), `buildTargets` (symbol), `applyTargets` (symbol, the survivor arm), `NewManager` (symbol), `Reload` (symbol), `Send` (symbol)
- Modify: `internal/notifications/queue.go` — `targetQueue` (symbol, three new fields), `newTargetQueue` (symbol), and the new `mentionFor` / `setMention` methods beside `allows` / `setEvents`
- Create: `internal/notifications/mentions.go`
- Test: `internal/notifications/target_options_test.go` (new)

**Anchors are by symbol only** — every line number in `internal/notifications` moved with Arc N1.

- **Never hand-build a `Manager`.** `Manager.targets` is `[]*targetQueue` and `notificationTarget` is a build-time value `applyTargets` consumes and discards, so `&Manager{targets: []notificationTarget{…}}` is a type error, not a stale field. Build every Manager in this file through `newTestManager(t, time.Second, notificationTarget{sender: rec, key: "k1"})` (`internal/notifications/manager_test.go`) — it routes through `applyTargets`, the only path that creates a queue and starts its goroutine — and set `m.publicURL` directly afterwards. `senderFunc` (`internal/notifications/manager_dispatch_test.go`) and `testLogger` (`internal/notifications/manager_test.go`) are unchanged; note that `senderFunc` implements **both** `Send` and `SendOnce`.
- N1 deleted the `semaphore` field and `maxInflightNotifications`; neither appears in any test here.
- A test that asserts on what a queue delivered must let the goroutine run. `newTestManager`'s cleanup calls `stopDiscard`, so drain deliberately: `m.Wait()` after the sends (it calls `closeDrain` and blocks until each goroutine exits).

**Interfaces:**

Produces, in `internal/notifications/mentions.go`:
```go
// JobDeepLink returns the dashboard URL that opens a job's details, or "" when
// no public_url is configured.
func JobDeepLink(publicURL, jobID string) string
```

Produces, in `internal/notifications/queue.go` beside `allows` and `setEvents` — **on `*targetQueue`, not on `notificationTarget`**, because `Send` iterates `[]*targetQueue` and never sees a `notificationTarget`:
```go
// mentionFor returns the content mention this target attaches to event and the
// matching allowed_mentions object, or ("", nil) when the target has no mention
// configured or the event is not in its mention filter. Alias-aware, by the
// same rule and the same eventAliases table as allows.
func (q *targetQueue) mentionFor(event string) (string, *AllowedMentions)

// setMention swaps the mention on a target that survived a Reload — the twin
// of setEvents, and required for the same reason: applyTargets keeps a
// survivor's queue and discards the freshly built notificationTarget, so
// without this a save that changes only `mention` or `mention_events` is
// silently ignored for every webhook that survived the diff.
func (q *targetQueue) setMention(t notificationTarget)
```

Extends `notificationTarget` (build-time, in `manager.go`) — these are what `buildTargets` produces and `newTargetQueue`/`setMention` copy onto the queue:
```go
type notificationTarget struct {
	sender         sender
	events         map[string]bool  // nil means all events
	key            string           // the RESOLVED webhook URL
	mention        string           // "" = this target never pings
	mentionAllowed *AllowedMentions // MentionParse's object, resolved once
	mentionEvents  map[string]bool  // nil when mention == ""
}
```
`targetQueue` gains the same last three fields, guarded by `q.mu` like `events`; `newTargetQueue` copies them.

Extends `Manager`: `publicURL string`. It is **not** written by `applyTargets` — `NewManager` and `Reload` take `targetsMu.Lock()` for it themselves, *before* calling `applyTargets`, so a `Send` that already sees the new targets can never still see the old base URL. `Send` snapshots it inside the same `RLock` that snapshots `targets`.

Consumes from N1: `SendOptions.{Mention, MentionAllowed, JobID, Author}`, `Author`, `AllowedMentions`, `MentionParse`, `eventAliases`, `newTestManager`.

**The Author rule.** The rewrite fires only when `publicURL != "" && opts.JobID != ""`:
- `opts.Author != nil` → **copy** the Author (it is a pointer shared across targets and with the caller; mutating it in place would corrupt the next target's send), set the copy's `URL` to the old `opts.URL`, then set `opts.URL = JobDeepLink(pub, opts.JobID)`.
- `opts.Author == nil` → **no rewrite at all.** With no author line there is nowhere for the platform link to go, and dropping the only link to the video to gain a dashboard link is a net loss. N2a gives every job send an Author, so this arm is the pre-N2a and System-send case.

- [ ] **Step 1: Write the failing tests**

Create `internal/notifications/target_options_test.go`:

```go
package notifications

import (
	"reflect"
	"sync"
	"testing"
	"time"

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
	// Through newTargetQueue, not the bare notificationTarget: mentionFor is a
	// *targetQueue method because that is the only object Send iterates, and a
	// test that called it on the build-time value would not prove the fields
	// survive newTargetQueue's copy.
	mk := func(nc config.NotificationConfig) *targetQueue {
		nc.URL = "discord://1/aaa"
		got := buildTargets(&config.MoomboxConfig{Notifications: []config.NotificationConfig{nc}}, testLogger{})
		if len(got) != 1 {
			t.Fatalf("buildTargets returned %d targets", len(got))
		}
		return newTargetQueue(got[0], testLogger{}, nil)
	}
	never := []string{}
	only := []string{"finished"}
	warnOnly := []string{"disk_warning"}
	pauseOnly := []string{"connectivity_pause"}

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
		{"retired-key alias", config.NotificationConfig{Mention: "@here", MentionEvents: &pauseOnly},
			"connectivity_resume", "@here",
			"N1 retired connectivity_pause and mapped connectivity_resume onto it; a config written " +
				"before that retirement must keep pinging on the embed that now carries the pause"},
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
//
// The EMPTY `Parse` on the role and user rows is load-bearing, not
// incidental. AllowedMentions.Parse is `json:"parse"` WITHOUT omitempty
// (internal/notifications/manager.go) precisely so the list is always on the
// wire: an omitted or null parse re-widens a role ping into the webhook
// default. MentionParse (internal/notifications/discord.go) returns
// []string{}; a hand-built literal would return nil, which marshals to
// `"parse": null` — a different wire form, and reflect.DeepEqual separates
// them. That is why this test builds nothing itself.
func TestMentionAllowedPerForm(t *testing.T) {
	for _, tc := range []struct {
		mention string
		want    AllowedMentions
	}{
		{"<@&123456789012345678>", AllowedMentions{Parse: []string{}, Roles: []string{"123456789012345678"}}},
		{"<@123456789012345678>", AllowedMentions{Parse: []string{}, Users: []string{"123456789012345678"}}},
		{"@everyone", AllowedMentions{Parse: []string{"everyone"}}},
		{"@here", AllowedMentions{Parse: []string{"everyone"}}},
	} {
		built := buildTargets(&config.MoomboxConfig{Notifications: []config.NotificationConfig{
			{URL: "discord://1/aaa", Mention: tc.mention},
		}}, testLogger{})[0]
		_, got := newTargetQueue(built, testLogger{}, nil).mentionFor("error")
		if got == nil || !reflect.DeepEqual(*got, tc.want) {
			t.Errorf("mentionFor for %s = %+v, want %+v", tc.mention, got, tc.want)
		}
	}
}

// recordOpts is the shared recorder for the three Send tests below: a
// senderFunc that appends every delivered SendOptions under a mutex (the
// queue delivers from its own goroutine) and the slice it fills.
func recordOpts() (*[]SendOptions, *sync.Mutex, senderFunc) {
	var mu sync.Mutex
	got := &[]SendOptions{}
	return got, &mu, senderFunc(func(_, _ string, _ int, _ []Field, opts SendOptions) error {
		mu.Lock()
		defer mu.Unlock()
		*got = append(*got, opts)
		return nil
	})
}

// mentioningTarget is the built target the mention tests deliver through.
func mentioningTarget(t *testing.T, s sender, nc config.NotificationConfig) notificationTarget {
	t.Helper()
	nc.URL = "discord://1/aaa"
	built := buildTargets(&config.MoomboxConfig{Notifications: []config.NotificationConfig{nc}}, testLogger{})
	if len(built) != 1 {
		t.Fatalf("buildTargets returned %d targets", len(built))
	}
	built[0].sender = s // keep the resolved key and the mention, swap the transport
	return built[0]
}

// TestSendAttachesMention proves the decision reaches the delivered send, and
// that a target whose filter excludes the event gets no content line at all.
func TestSendAttachesMention(t *testing.T) {
	only := []string{"error"}
	got, mu, rec := recordOpts()
	m := newTestManager(t, time.Second, mentioningTarget(t, rec, config.NotificationConfig{
		Mention:       "<@&123456789012345678>",
		MentionEvents: &only,
	}))

	m.Send("Job Failed", "d", TypeError, nil, SendOptions{Event: "error"})
	m.Send("Download Finished", "d", TypeSuccess, nil, SendOptions{Event: "finished"})
	m.Wait() // closeDrain + block until the queue goroutine has delivered both

	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 2 {
		t.Fatalf("recorded %d sends, want 2", len(*got))
	}
	if (*got)[0].Mention != "<@&123456789012345678>" {
		t.Errorf("the error send carried Mention = %q, want the configured role", (*got)[0].Mention)
	}
	if (*got)[0].MentionAllowed == nil {
		t.Error("the error send carried no allowed_mentions — Discord would render the role as plain " +
			"text and ping nobody")
	}
	if (*got)[1].Mention != "" {
		t.Errorf("the finished send carried Mention = %q, want none — it is not in the mention filter", (*got)[1].Mention)
	}
	if (*got)[1].MentionAllowed != nil {
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
	got, mu, rec := recordOpts()
	author := &Author{Name: "Some Channel", URL: "https://youtube.com/@some", IconURL: "https://i/a.jpg"}
	// TWO targets, distinct keys: applyTargets dedupes on key, and the whole
	// point of the copy assertion is that target 1's rewrite must not have
	// mutated the Author target 2 then sees.
	m := newTestManager(t, time.Second,
		notificationTarget{sender: rec, key: "k1"},
		notificationTarget{sender: rec, key: "k2"},
	)
	m.publicURL = "https://moombox.example.com"

	m.Send("Download Finished", "d", TypeSuccess, nil, SendOptions{
		Event:  "finished",
		JobID:  "job-1",
		URL:    "https://youtube.com/watch?v=abc",
		Author: author,
	})
	m.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 2 {
		t.Fatalf("recorded %d sends, want 2", len(*got))
	}
	for i, o := range *got {
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
			got, mu, rec := recordOpts()
			m := newTestManager(t, time.Second, notificationTarget{sender: rec, key: "k1"})
			m.publicURL = tc.pub
			m.Send("t", "d", TypeInfo, nil, tc.opts)
			m.Wait()
			mu.Lock()
			defer mu.Unlock()
			if len(*got) != 1 {
				t.Fatalf("recorded %d sends, want 1", len(*got))
			}
			if (*got)[0].URL != tc.opts.URL {
				t.Errorf("URL = %q, want %q left alone", (*got)[0].URL, tc.opts.URL)
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

// TestReloadCarriesTheMentionToASurvivingTarget is applyTargets' survivor arm.
// A target whose URL did not change KEEPS its queue — that is deliberate, so a
// save touching an unrelated section does not cost every webhook its backlog
// and its learned rate bucket — and the freshly built notificationTarget is
// discarded. setEvents exists so the FILTER still follows the save; without
// the matching setMention, changing only `mention` or `mention_events` is
// accepted by both UIs, written to the file, and then ignored until restart.
func TestReloadCarriesTheMentionToASurvivingTarget(t *testing.T) {
	const url = "discord://1/aaa"
	before := &config.MoomboxConfig{Notifications: []config.NotificationConfig{{URL: url}}}
	m := NewManager(before, testLogger{})
	t.Cleanup(func() { m.Wait() })

	m.targetsMu.RLock()
	q := m.targets[0]
	m.targetsMu.RUnlock()
	if got, _ := q.mentionFor("error"); got != "" {
		t.Fatalf("a target with no mention resolved %q", got)
	}

	after := &config.MoomboxConfig{Notifications: []config.NotificationConfig{{URL: url, Mention: "@here"}}}
	m.Reload(after)

	m.targetsMu.RLock()
	same := m.targets[0] == q
	q2 := m.targets[0]
	m.targetsMu.RUnlock()
	if !same {
		t.Fatal("the surviving target did not keep its queue — applyTargets' diff is broken, and this " +
			"test is no longer testing what it says")
	}
	if got, allowed := q2.mentionFor("error"); got != "@here" || allowed == nil {
		t.Errorf("after Reload the surviving target resolves (%q, %v) for error, want (\"@here\", non-nil) — "+
			"applyTargets kept the queue but not the new mention", got, allowed)
	}
}
```

Run:

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/notifications/
```

Expected (build failure — the four names this task introduces):

```
internal/notifications/target_options_test.go:NN:NN: undefined: JobDeepLink
internal/notifications/target_options_test.go:NN:NN: built[0].mentionFor undefined (type notificationTarget has no field or method mentionFor)
internal/notifications/target_options_test.go:NN:NN: unknown field mention in struct literal of type notificationTarget
internal/notifications/target_options_test.go:NN:NN: m.publicURL undefined (type *Manager has no field or method publicURL)
FAIL	github.com/vampiricwulf/Moombox/internal/notifications [build failed]
```

Task 1's four config keys must be in the tree first, or the first error is instead `unknown field Enabled in struct literal of type config.NotificationConfig` — which is the same red state one task earlier.

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
3. `buildTargets`, after the event filter is built: resolve the mention. `nc.ResolveMentionEvents()` into a `map[string]bool`; Warn (with a redacted URL) for any entry outside `KnownEvents`, exactly as the events filter does. Then:
   ```go
   		// TWO parsers, two jobs. config.ParseMention validates the operator's
   		// string and hands back its canonical form — it lives in
   		// internal/config because that is where validateOrNormalize and both
   		// Settings editors need it, and internal/config cannot import this
   		// package (the import runs the other way). MentionParse
   		// (internal/notifications/discord.go) turns that canonical string into
   		// the wire object; N1 wrote it for this call and its doc comment says
   		// so. Do NOT rebuild the object from (form, id): MentionParse returns
   		// Parse: []string{} for the role and user forms, and Parse is
   		// json:"parse" WITHOUT omitempty precisely so an empty list is on the
   		// wire — a nil there marshals to "parse": null and re-widens the ping
   		// to the webhook default.
   		if canonical, _, _, err := config.ParseMention(nc.Mention); err == nil && canonical != "" {
   			mention = canonical
   			mentionAllowed = MentionParse(canonical)
   		}
   ```
   Leave `mention`/`mentionAllowed`/`mentionEvents` zero when `nc.Mention == ""`. `MentionParse` returning nil for a form it does not recognise is the second gate: no object, no ping.
4. Dedupe union: when a duplicate resolved URL collapses, the FIRST occurrence's mention wins (same rule as the sender and the slot). Add one sentence to the existing union comment.
   **4b.** `targetQueue` gains `mention`, `mentionAllowed` and `mentionEvents` under `q.mu`; `newTargetQueue` copies them from the `notificationTarget` it is given. Add `setMention(t notificationTarget)` beside `setEvents`, and call it in `applyTargets`' **survivor** arm right after `q.setEvents(t.events)`. Without it, `applyTargets` keeps the survivor's queue and discards the freshly built target, so a save that changes only `mention` or `mention_events` is silently ignored for every webhook that survived the diff — the same defect `setEvents` exists to prevent for the filter.
5. `Manager` gains `publicURL string`. `NewManager` and `Reload` each take `m.targetsMu.Lock()` for it **before** calling `applyTargets` — `applyTargets` is the swap path and takes the same lock itself, so the write cannot ride inside it, and doing it first means a `Send` that already sees the new targets can never still see the old base URL.
6. `Send`: snapshot `pub := m.publicURL` inside the existing `RLock` that snapshots `targets`; apply the Author/URL rewrite to `opts` **once, before** `it := queued{…}` is built (the value is install-wide, not per target, and the rewrite must be inside the item every queue receives). Then, in the existing `for _, q := range targets` loop, after `q.allows(opts.Event)` passes:
   ```go
   		perItem := it
   		perItem.opts.Mention, perItem.opts.MentionAllowed = q.mentionFor(opts.Event)
   		q.enqueue(perItem)
   ```
   `queued` is a value, so `perItem := it` is the per-target copy; `fields` is shared across the copies, which is safe because N1 already copied it once in `Send` and nothing downstream mutates it. The `*AllowedMentions` is built once in `buildTargets` and never mutated after, so sharing the pointer across targets and sends is safe.
7. `internal/notifications/mentions.go` holds `JobDeepLink` only; `mentionFor` and `setMention` go in `queue.go` beside `allows` and `setEvents`, which they mirror. `JobDeepLink` returns `""` when either argument is empty, trims a trailing `/` from the base, and appends `"/#job=" + url.PathEscape(jobID)`.

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
6. Rebuild the `allowed_mentions` object from `config.ParseMention`'s `(form, id)` instead of calling `MentionParse` → `TestMentionAllowedPerForm` fails on the role and user rows, on the `Parse` slice (`[]` vs `nil`).
7. Return `mentionAllowed` unconditionally, ignoring the filter → `TestSendAttachesMention` fails on the finished send.
8. Drop the `q.setMention(t)` call from `applyTargets`' survivor arm → `TestReloadCarriesTheMentionToASurvivingTarget` fails. This is the defect that would otherwise ship silently: both UIs accept the edit and the file records it.
9. Drop the `connectivity_pause` row's alias arm → `TestMentionFor/retired-key_alias` fails.

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

**The hot-reload gap this closes.** At `1d2df1d4` the PUT handler fires `OnNotificationsChange` only when the payload carries a `notifications` key (`config_routes.go:949`). `public_url` lives in `network`, and the web form's Save sends `network` without `notifications` unless a webhook exists (`settings.js:1039-1041` at `242d921c`). So a `public_url` change would never reach `Manager.Reload`. The TUI has no such gap — `cmd/moombox/tui_wiring.go:418` calls `s.notifyMgr.Reload(snap)` unconditionally on every settings save.

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
- Modify: `web/public/modules/settings.js` — **every line number below is N1's (`242d921c`), one lower than the pre-N1 plan carried, because N1 removed the `connectivity_pause` mirror entry at old `:41`**: `:20-68` (the constants block; add `DEFAULT_MENTION_EVENTS` beside `ALL_EVENT_IDS` at `:68`), `:744-745` (populate), `:917-925` (gather), `:940-947` (payload), `:1770-1842` (`renderNotificationsList` + the `_notifDelegated` guard at `:1829-1841`), `:1939-1993` (the three filter handlers: `toggleNotificationEvent` `:1939`, `enableNotificationFilter` `:1967`, `clearNotificationFilter` `:1981`) and `:2000` (`_saveNotificationsOnly`, unchanged but read by the new handlers)
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

`settings.js` — the constant beside `ALL_EVENT_IDS` (`:68`), the `setInputValue("cfg-public-url", config.network?.public_url || "")` beside the `cfg-trusted-proxies` populate (`:744`), the gather (`:917`) and `public_url: publicUrl` in the `network` payload literal (`:946`).

Dirty tracking needs nothing **for `cfg-public-url`** — `.settings-content` (`index.html:554`, read at `settings.js:762`) already delegates `sl-change`/`sl-input` to `_markDirty()` (`:764-765`). It needs a **stop** for the two new *card* controls: `#notifications-list` (`index.html:1395`) is inside `.settings-content` too, the card controls auto-save through `_saveNotificationsOnly`, and `_markDirty` (`:1269-1274`) unconditionally sets `_dirty` and shows `#settings-unsaved-banner`. Today the card uses only `<sl-tag>` clicks so nothing fires; a switch and a text input there would raise a false "unsaved changes" banner on every auto-saved edit. Both new listeners therefore call `e.stopPropagation()` on the `sl-change` they handle.

`renderNotificationsList` — extend the card template and **split the delegate by event type** inside the existing `_notifDelegated` guard (`:1829-1841`). The **click** listener keeps its five existing actions and gains only `toggle-mention-event`; it must `return` early for `toggle-enabled` and `mention-input`, which a click on those controls also reaches (`<sl-switch>` and `<sl-input>` both bubble `click`, and `closest("[data-notif-action]")` matches them). A second `container.addEventListener("sl-change", …)` handles exactly those two. Without the split the switch PUTs twice per toggle and the mention field PUTs on every focus click.

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

**Why a mention COLUMN and not a second 25-row block.** The event list is already the tallest thing in the overlay and already needs a focus-following scroll window (`settings_view.go:628-644`). A second block of the same 25 rows (N1 retired `connectivity_pause` from `EventGroups`, so `len(allNotifEvents)` is 25, not 26) would double a list an operator navigates with ↑/↓ only, and would need its own scroll mapping in `handleMouseNotifClick`. A second column on the row that already names the event keeps one navigation list, puts an event's two flags side by side where they are read together, and costs the mouse map one extra hit region instead of 25. `Space` toggles the filter checkbox (unchanged); `m` toggles the mention flag on the focused row.

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

**The empty filter is accepted.** `handleNotifEditKey`'s Enter arm currently refuses a zero-event selection (`settings_notifications.go:163-169`) with "Select at least one event". That refusal is removed: an empty selection now stores `Events = nil`, which is "all events" — the same thing the web UI has always meant by it and what `operations.md` documents. The list view's `(N/M events)` suffix — `M` is `len(allNotifEvents)`, which the line renders itself (`settings_view.go:539`), so it needs no edit for N1's retirement of `connectivity_pause` — becomes `All events` when the filter is empty, matching the web card's `<sl-tag variant="success">All events</sl-tag>`. An operator who wanted silence uses the Enabled toggle.

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
- Modify: `web/public/app.js:148` (immediately after the `resize` listener in `initializeApp`, which runs `:141-147` and closes with `}, { passive: true });` at `:147`) and `:1144` (before `break;` in the `initial_state` case)
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

`app.js:148`, immediately after the `resize` listener (which closes at `:147`):

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

## Task 7: The multi-embed seam — one `Message`, many `Embed`s

**Files:**
- Create: `internal/notifications/message.go`
- Modify: `internal/notifications/discord.go` — `buildPayload`, `Send`, `SendOnce` (all three take a `Message`)
- Modify: `internal/notifications/manager.go` — the `sender` interface, `SendTest`, `Manager.Send`'s item construction
- Modify: `internal/notifications/queue.go` — `queued`, `enqueue`'s two Warn lines, `deliver`
- Modify: N1's four test fakes — `senderFunc` (`internal/notifications/manager_dispatch_test.go`), `recordingSender` (`internal/notifications/manager_test.go`), `gateSender` and `fieldSender` (`internal/notifications/queue_test.go`)
- Modify: `internal/notifications/target_options_test.go` (Task 2's three `senderFunc` closures and their assertions move from `SendOptions` to `Message`)
- Modify: `internal/notifications/payload_test.go` (its seven `buildPayload(...)` calls take a `Message`)
- Test: `internal/notifications/message_test.go` (new)

**Why this task exists.** Spec §3.4 needs one message carrying up to ten embeds. N1's delivery path is strictly one embed at four places, verified at `242d921c`: `queued{title, description, color, fields, opts, tier}` (`internal/notifications/queue.go`), `sender.Send`/`SendOnce` taking those five scalars (`internal/notifications/manager.go`), `targetQueue.deliver` calling them positionally, and `buildPayload` ending `payload := discordPayload{Embeds: []discordEmbed{embed}}` (`internal/notifications/discord.go`). No adaptation expression bridges that; a value type does. Doing it as its own task, before batching, keeps the risky part — changing a path all ~36 producers already use — provable on its own, with a byte-identity test as the proof.

**Interfaces:**

Produces, in `internal/notifications/message.go`:
```go
// Embed is one embed's worth of a message: exactly what Manager.Send used to
// hand a sender as five positional arguments. Opts is per embed because
// Discord's title URL, author line, thumbnail, image and footer are all
// per-embed fields — a batch of ten `found`s is ten different jobs.
type Embed struct {
	Title       string
	Description string
	Color       int
	Fields      []Field
	Opts        SendOptions
}

// Message is one Discord webhook POST: its embeds, and the single content
// mention that applies to the whole message. The mention is message-level
// because Discord's `content` and `allowed_mentions` are message-level — an
// embed can never ping anyone — so a batch pings once however many embeds it
// carries.
type Message struct {
	Embeds         []Embed
	Mention        string
	MentionAllowed *AllowedMentions
}

// One is the single-embed Message every non-batched send is.
func One(title, description string, color int, fields []Field, opts SendOptions) Message

// logEvent and logTitle name a message for a log line: the first embed's,
// which for a single-embed message is the only one.
func (m Message) logEvent() string
func (m Message) logTitle() string
```

Changes in `internal/notifications/manager.go`:
```go
type sender interface {
	Send(msg Message) error
	SendOnce(msg Message) error
}
```

Changes in `internal/notifications/queue.go`:
```go
// queued is one MESSAGE waiting for one target. The tier is resolved once at
// enqueue so the overflow policy never has to re-derive it.
type queued struct {
	msg  Message
	tier Tier
}
```

Changes in `internal/notifications/discord.go`: `buildPayload(msg Message) ([]byte, error)` builds one `discordEmbed` per `Embed` through a new `toDiscordEmbed(e Embed) discordEmbed` (which does everything `buildPayload` does per embed today, `clampEmbed` included), and reads `Content`/`AllowedMentions` from `msg` rather than from `opts`. `toDiscordEmbed` is exported to the package so Task 8 can size an embed with the *same* clamp the payload will apply — one definition of "how big is this embed", no drift.

**What does NOT change.** `SendOptions.Mention`/`MentionAllowed` stay exactly where N1 put them. They are still what `q.mentionFor` answers with; the only difference is that `Send`'s per-target copy now writes them onto the `Message` rather than onto the embed's `Opts`, because Discord's `content` and `allowed_mentions` are message-level and a batch must ping once. Producers are untouched — `Manager.Send`'s exported signature is byte-identical, which is why none of the ~36 call sites, and none of `internal/worker`, `cmd/moombox` or `internal/web/routes`, appears in the Files list. `notificationtest.Recorder` implements `Notifier` (the `Sender`-level surface), not the internal `sender`, so it needs no edit and no one should go looking.

- [ ] **Step 1: Write the failing test**

Create `internal/notifications/message_test.go`:

```go
package notifications

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestOneEmbedMessageIsByteIdenticalToN1 is this task's whole safety
// argument. Every one of the ~36 producer sites goes through the single-embed
// path, and the seam rewrites that path for the sake of a feature none of
// them use. The payload they produce must not move by one byte.
//
// The expected JSON is N1's, captured from buildPayload at 242d921c with the
// timestamp elided (it is time.Now at build time). Field ORDER matters: it is
// struct order in discordPayload/discordEmbed, and a reordering would be a
// silent diff in every operator's channel.
func TestOneEmbedMessageIsByteIdenticalToN1(t *testing.T) {
	opts := SendOptions{
		URL:       "https://youtube.com/watch?v=abc",
		Event:     "finished",
		Thumbnail: "https://i.example/t.jpg",
		Platform:  "YouTube",
		JobID:     "job-1",
		Author:    &Author{Name: "Some Channel", URL: "https://youtube.com/@some", IconURL: "https://i.example/a.jpg"},
	}
	fields := []Field{{Name: "File", Value: "out.mp4"}, {Name: "Duration", Value: "1:02:03", Inline: true}}

	body, err := buildPayload(One("Download Finished", "Successfully archived: X", 0x2ecc71, fields, opts))
	if err != nil {
		t.Fatal(err)
	}
	got := elideTimestamp(t, body)

	const want = `{"embeds":[{"title":"Download Finished","description":"Successfully archived: X",` +
		`"color":3066993,"url":"https://youtube.com/watch?v=abc",` +
		`"author":{"name":"Some Channel","url":"https://youtube.com/@some","icon_url":"https://i.example/a.jpg"},` +
		`"fields":[{"name":"File","value":"out.mp4"},{"name":"Duration","value":"1:02:03","inline":true}],` +
		`"thumbnail":{"url":"https://i.example/t.jpg"},` +
		`"footer":{"text":"Moombox · YouTube · job-1"},"timestamp":"ELIDED"}]}`
	if got != want {
		t.Errorf("a one-embed Message no longer produces N1's payload.\n got: %s\nwant: %s", got, want)
	}
}

// elideTimestamp replaces every embed timestamp with a constant so the
// comparison is about shape, not about the clock.
func elideTimestamp(t *testing.T, body []byte) string {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	for _, e := range v["embeds"].([]any) {
		e.(map[string]any)["timestamp"] = "ELIDED"
	}
	// Re-marshalling through a map would reorder keys alphabetically and lose
	// the point of the test, so edit the raw bytes instead.
	out := string(body)
	for {
		i := strings.Index(out, `"timestamp":"`)
		if i < 0 {
			return out
		}
		j := strings.Index(out[i+13:], `"`)
		if j < 0 {
			return out
		}
		out = out[:i+13] + "ELIDED" + out[i+13+j:]
	}
}

// TestMessageCarriesTheMention pins that the content mention moved from the
// embed's opts to the MESSAGE, which is what lets a ten-embed batch ping once.
func TestMessageCarriesTheMention(t *testing.T) {
	msg := One("T", "D", 0x1, nil, SendOptions{})
	msg.Mention = "<@&123456789012345678>"
	msg.MentionAllowed = MentionParse(msg.Mention)

	body, err := buildPayload(msg)
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	if !strings.Contains(s, `"content":"<@\u0026123456789012345678>"`) &&
		!strings.Contains(s, `"content":"<@&123456789012345678>"`) {
		t.Errorf("the message content did not carry the mention: %s", s)
	}
	if !strings.Contains(s, `"allowed_mentions":{"parse":[],"roles":["123456789012345678"]}`) {
		t.Errorf("allowed_mentions is not the object MentionParse built: %s", s)
	}
}

// TestMultiEmbedMessageEmitsEveryEmbed is the capability the seam exists for.
func TestMultiEmbedMessageEmitsEveryEmbed(t *testing.T) {
	msg := Message{}
	for i := range 3 {
		msg.Embeds = append(msg.Embeds, Embed{
			Title: "Stream Found " + itoa(i),
			Opts:  SendOptions{Event: "found", JobID: "j" + itoa(i)},
		})
	}
	body, err := buildPayload(msg)
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Embeds []struct {
			Title  string `json:"title"`
			Footer struct {
				Text string `json:"text"`
			} `json:"footer"`
		} `json:"embeds"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Embeds) != 3 {
		t.Fatalf("payload carried %d embeds, want 3", len(v.Embeds))
	}
	for i, e := range v.Embeds {
		if e.Title != "Stream Found "+itoa(i) {
			t.Errorf("embed %d out of order: %q", i, e.Title)
		}
		if !strings.HasSuffix(e.Footer.Text, "j"+itoa(i)) {
			t.Errorf("embed %d's footer is not its own (%q) — Opts must be PER EMBED, or a batch of "+
				"ten jobs would show the same job id ten times", i, e.Footer.Text)
		}
	}
}
```

Run:

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/notifications/
```

Expected (build failure):

```
internal/notifications/message_test.go:NN:NN: undefined: One
internal/notifications/message_test.go:NN:NN: not enough arguments in call to buildPayload
	have (Message)
	want (string, string, int, []Field, SendOptions)
internal/notifications/message_test.go:NN:NN: undefined: Message
FAIL	github.com/vampiricwulf/Moombox/internal/notifications [build failed]
```

- [ ] **Step 2: Implement**

1. `internal/notifications/message.go` — `Embed`, `Message`, `One`, `logEvent`, `logTitle`.
2. `internal/notifications/discord.go` — split today's `buildPayload` body at the point where it has finished the one embed: everything up to and including `clampEmbed(&embed)` becomes `toDiscordEmbed(e Embed) discordEmbed` (reading `e.Title`, `e.Description`, `e.Color`, `e.Fields`, `e.Opts`), and `buildPayload(msg Message)` maps it over `msg.Embeds` and then reads `msg.Mention`/`msg.MentionAllowed` for `Content`/`AllowedMentions`. Keep the existing comments with the code they explain. `DiscordWebhook.Send(msg Message)` and `SendOnce(msg Message)` change only their first line.
3. `internal/notifications/manager.go` — the `sender` interface's two methods take a `Message`; `SendTest` becomes `s.SendOnce(One("Test Notification", "Moombox notifications are configured correctly", TypeSuccess.Color(), []Field{{Name: "Status", Value: "Working", Inline: true}}, SendOptions{}))`; `Manager.Send` builds `msg := One(title, description, ntype.Color(), append([]Field(nil), fields...), opts)` — **keeping N1's field copy exactly where it is, once per send** — then `it := queued{msg: msg, tier: effectiveTier(opts)}`.
4. `internal/notifications/queue.go` — `queued` becomes `{msg Message; tier Tier}`; `enqueue`'s three Warn lines read `it.msg.logEvent()` / `it.msg.logTitle()` (and `dropped.msg.…` in the oldest-victim arm); `deliver` calls `q.sender.SendOnce(it.msg)` / `q.sender.Send(it.msg)`.
5. **Task 2's per-target mention write moves onto the message.** `Send`'s loop body becomes:
   ```go
   		perItem := it
   		perItem.msg.Mention, perItem.msg.MentionAllowed = q.mentionFor(opts.Event)
   		q.enqueue(perItem)
   ```
   `Message` is a value and `Embeds` is shared between the copies, which is safe: nothing mutates an `Embed` after `One` builds it.
6. N1's four test fakes each gain a two-line adaptation — they keep their existing positional bodies and add a `Send(msg Message)`/`SendOnce(msg Message)` that unpacks `msg.Embeds[0]`. Where a fake asserts on `opts`, it reads `msg.Embeds[0].Opts`; `gateSender`'s `titles`/`once` read `msg.logTitle()`. `payload_test.go`'s seven `buildPayload("T", "D", 0x1, nil, opts)` calls become `buildPayload(One("T", "D", 0x1, nil, opts))`, and the two mention cases at `payload_test.go:194` and `:249` set the fields on the `Message` instead of on `SendOptions`.
7. `internal/notifications/target_options_test.go` (Task 2's file) — the three `senderFunc` closures take a `Message`; `recordOpts` appends `msg.Embeds[0].Opts`; `TestSendAttachesMention`'s two mention assertions read `msg.Mention`/`msg.MentionAllowed`, which is where they now live. Update `recordOpts`' returned closure signature in one place and the two assertions in another.

- [ ] **Step 3: Verify**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -race ./internal/notifications/
```
Expected: `ok  github.com/vampiricwulf/Moombox/internal/notifications` with **N1's entire suite still green** — `payload_test.go`, `limits_test.go`, `queue_test.go`, `ratelimit_test.go`, `discord_test.go`, `events_test.go`, `manager_test.go`, `manager_dispatch_test.go`. That is the real gate for this task: the seam is only safe if nothing N1 pinned moved. Then:
```bash
go build ./...
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/worker/ ./cmd/moombox/
```
— `Manager.Send`'s exported signature is unchanged, so these must pass without a single producer edit. If either fails, the seam leaked past the package and the design is wrong, not the call site. Then the Global gate block.

**Mutations to run:**
1. Reorder `discordPayload`'s or `discordEmbed`'s fields → `TestOneEmbedMessageIsByteIdenticalToN1` fails.
2. Hoist `Opts` from `Embed` to `Message` (one footer for the whole message) → `TestMultiEmbedMessageEmitsEveryEmbed` fails on the footer assertion.
3. Read `Content`/`AllowedMentions` from `msg.Embeds[0].Opts` instead of from `msg` → `TestMessageCarriesTheMention` fails.
4. Drop `clampEmbed` from `toDiscordEmbed` → N1's `limits_test.go` fails.
5. Move N1's per-send `fields` copy into `toDiscordEmbed` → N1's `queue_test.go` field-aliasing test (`fieldSender`) fails.

- [ ] **Step 4: Commit**

Pathspec: `internal/notifications/message.go internal/notifications/message_test.go internal/notifications/discord.go internal/notifications/manager.go internal/notifications/queue.go internal/notifications/manager_test.go internal/notifications/manager_dispatch_test.go internal/notifications/queue_test.go internal/notifications/payload_test.go internal/notifications/target_options_test.go`.
Message: `refactor(notifications): one Message, many Embeds — the multi-embed delivery seam` plus the two trailers from Global Constraints, verbatim.

---

## Task 8: Batching — the coalescing stage in front of the per-target FIFO

**Files:**
- Create: `internal/notifications/batch.go`
- Create: `internal/notifications/batch_test.go`
- Modify: `internal/notifications/queue.go` — `targetQueue` (one field), `newTargetQueue` (creates it, and gains the injectable clock), the new `enqueueBatch` method, `closeDrain` (flush first)
- Modify: `internal/notifications/manager.go` — `Send` (route through the batcher), `applyTargets` (retire batchers in the existing `retired` loop), `Wait` and `BeginShutdown` (flush before `closeDrain`)
- Modify: `internal/notifications/manager_test.go` — `newTestManager` passes the clock through to `newTargetQueue`

**Where the batcher lives, and why not where the first draft put it.** `Manager.targets` is `[]*targetQueue`; `notificationTarget` is a build-time value `applyTargets` consumes and discards. A batcher created at the end of `buildTargets` and hung on a `notificationTarget` would be thrown away on every `Reload` — `applyTargets` keeps a **surviving** target's queue and drops the freshly built value, so an open `found` window would vanish, with no log line, every time an operator saved an unrelated setting. So: the batcher is created in `newTargetQueue`, lives on `targetQueue`, and is retired in `applyTargets`' existing `retired` loop beside `q.stopDiscard()` — which already runs outside `targetsMu` and is therefore already the right place for something that flushes into a queue.

**Interfaces** (all in `batch.go` unless noted):

```go
// batchWindow is how long a target's coalescing window stays open, measured
// from the FIRST item in it — not a sliding window, so a steady trickle of
// finds cannot hold a message open indefinitely.
const batchWindow = 5 * time.Second

// maxEmbedsPerMessage is Discord's documented per-message embed cap.
const maxEmbedsPerMessage = 10

// emitFunc hands ONE Message to a target's FIFO queue as a single queue item,
// so the queue's drop policy and the ordering between messages are unchanged
// by batching.
type emitFunc func(msg Message)

type batchTimer interface{ Stop() bool }

// batchClock is the time source, injected so tests do not sleep.
type batchClock interface {
	AfterFunc(d time.Duration, f func()) batchTimer
}

type realBatchClock struct{}

func (realBatchClock) AfterFunc(d time.Duration, f func()) batchTimer

// batcher coalesces batchable embeds for ONE target.
type batcher struct { /* window, clock, emit, logger, mu, pending []Embed, timer */ }

func newBatcher(window time.Duration, clock batchClock, emit emitFunc, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *batcher

// Add routes one embed: a non-batchable one is emitted immediately as a
// one-embed Message; a batchable one joins the open window, arming it if it
// was closed. mention/mentionAllowed travel with the embed so a flush can
// decide the message's single ping.
func (b *batcher) Add(e Embed, mention string, allowed *AllowedMentions)

// Flush emits every pending embed now. Called before closeDrain, and from
// applyTargets when a target is retired.
func (b *batcher) Flush()

// Stop flushes and disarms.
func (b *batcher) Stop()

// isBatchable reports whether a send coalesces.
func isBatchable(opts SendOptions) bool

// splitMessages chops pending embeds into messages that satisfy BOTH of
// Discord's per-message caps — at most maxEmbedsPerMessage embeds, and at
// most limitTotal characters summed across them — preserving arrival order.
func splitMessages(pending []Embed) [][]Embed

// messageRunes is the character count Discord applies its per-MESSAGE 6000
// against: the sum of embedRunes over the message's CLAMPED embeds. It goes
// through toDiscordEmbed (discord.go), the same conversion buildPayload will
// use, so the size the splitter measures is the size the payload will have.
func messageRunes(embeds []Embed) int

// batchIsLowTier reports whether EVERY member is low-tier.
func batchIsLowTier(embeds []Embed) bool
```

Added to `queue.go`:

```go
// enqueueBatch is the batcher's exit: it wraps one coalesced Message in a
// queue item — tier from batchIsLowTier, the message keeping whatever single
// mention the flush chose — and hands it to the ordinary FIFO. Named apart
// from enqueue, which takes an already-built queued item and is what this
// calls.
func (q *targetQueue) enqueueBatch(msg Message)
```

**The rules, and why each one is where it is:**

| Rule | Where |
|---|---|
| `found`, `added` batch; `auth` batches only with a `JobID` (the per-job "Authentication Required"; the platform-level cookie family carries none and must not wait) | `isBatchable` |
| `error`, `cancelled`, `trim_*`, System and everything else never batch | `isBatchable`'s default |
| Filters apply before coalescing | `Send` — the batcher sits after `q.allows(opts.Event)`, so a target never accumulates an embed it would not have sent |
| The window is 5 s from the first item | `Add` arms the timer only when `pending` was empty |
| Overflow rolls into further messages in order, on **either** cap | `splitMessages` starts a new message when adding the next embed would take the count past `maxEmbedsPerMessage` **or** the summed clamped size past `limitTotal` |
| The 6000-character budget is per MESSAGE, not per embed | `messageRunes`. `clampEmbed` applies `limitTotal` to ONE embed (`internal/notifications/limits.go`), so ten clamped embeds can total 60,000 — and `discord.go`'s ladder treats a non-429 4xx as permanent, so an over-budget batch would be one Error line and ten lost notifications. Splitting rather than shedding is what keeps "nothing dropped" true. |
| The batch is ONE queue item, so the FIFO's drop policy and the ordering *between messages* are unchanged | `emitFunc`'s contract; the closure is built in `newTargetQueue` over `q.enqueueBatch` |
| A non-batchable send emitted while a window is open goes out **ahead of** the embeds already coalesced | `Add` — deliberate: an error must not wait 5 s behind a backfill sweep. This is the one ordering change batching makes, so operations.md names it rather than claiming ordering is untouched. For a single job it means an `error` can land before that job's own `found`. |
| `TierLow` only if every member is | `batchIsLowTier`, applied by `enqueueBatch` |
| The mention rides once if any member is eligible | `Add` keeps the FIRST non-empty mention it is handed for the open window and puts it on the flushed `Message`; a batch therefore pings once however many members were eligible |
| `Wait` / `BeginShutdown` flush open windows | `Manager.Wait` calls `q.batch.Flush()` on every target **before** `q.closeDrain()`, and `BeginShutdown` flushes too. The order is not cosmetic: `enqueue` drops with a Warn once `q.closing` is set (`internal/notifications/queue.go`), so a flush after `closeDrain` would emit the batch straight into the drop path. |
| A retired target's window is flushed, not lost | `applyTargets`' `retired` loop calls `q.batch.Stop()` beside `q.stopDiscard()`. The queue then discards it, with N1's own "target removed — discarding its queued notifications" Warn naming the count, which is the honest outcome for a webhook the operator has just deleted. |
| Separate-mode targets only | Arc N3 adds `if q.mode == modeEdit { b.emit(One(…)) ; return }` at the top of `Add`; a comment in `Add` names it |

`Flush` copies `pending` and releases `b.mu` **before** calling `emit` — `emit` reaches `q.enqueue`, which takes `q.mu`, and holding both would invert the lock order against `Add`. The `AfterFunc` callback carries the inline `defer recover()`.

- [ ] **Step 1: Write the failing tests**

Create `internal/notifications/batch_test.go` with a fake clock and one test per rule:

```go
package notifications

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
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

func collector() (*[]Message, *sync.Mutex, emitFunc) {
	var mu sync.Mutex
	out := &[]Message{}
	return out, &mu, func(msg Message) {
		mu.Lock()
		defer mu.Unlock()
		*out = append(*out, msg)
	}
}

func found(id string) Embed {
	return Embed{Title: "Stream Found", Opts: SendOptions{Event: "found", JobID: id, Tier: TierLow}}
}

// addFound is the no-mention Add the window tests use.
func addFound(b *batcher, id string) { b.Add(found(id), "", nil) }

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
	got, mu, emit := collector()
	clk := &fakeBatchClock{}
	b := newBatcher(batchWindow, clk, emit, testLogger{})
	addFound(b, "a")
	addFound(b, "b")
	addFound(b, "c")
	mu.Lock()
	n := len(*got)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("emitted %d messages before the window closed", n)
	}
	clk.fire()
	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 1 {
		t.Fatalf("emitted %d messages, want 1", len(*got))
	}
	if len((*got)[0].Embeds) != 3 {
		t.Errorf("the message carried %d embeds, want 3", len((*got)[0].Embeds))
	}
}

// TestBatcherSplitsAtTen is Discord's documented per-message embed cap:
// overflow rolls into further messages, in order, and nothing is dropped —
// the whole point of batching a backfill sweep is that the sweep stops LOSING
// notifications.
func TestBatcherSplitsAtTen(t *testing.T) {
	got, mu, emit := collector()
	clk := &fakeBatchClock{}
	b := newBatcher(batchWindow, clk, emit, testLogger{})
	for i := range 23 {
		addFound(b, itoa(i))
	}
	clk.fire()
	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 3 {
		t.Fatalf("emitted %d messages for 23 embeds, want 3", len(*got))
	}
	n := []int{len((*got)[0].Embeds), len((*got)[1].Embeds), len((*got)[2].Embeds)}
	if n[0] != 10 || n[1] != 10 || n[2] != 3 {
		t.Errorf("message sizes %v, want [10 10 3]", n)
	}
	if (*got)[0].Embeds[0].Opts.JobID != "0" || (*got)[2].Embeds[2].Opts.JobID != "22" {
		t.Error("arrival order was not preserved across the split")
	}
}

// TestBatcherSplitsOnTheMessageCharacterBudget is the OTHER Discord cap, and
// the one the first draft of this plan missed. clampEmbed applies its 6000 to
// ONE embed (internal/notifications/limits.go), so ten legally-clamped embeds
// can total 60,000 characters — and discord.go's ladder treats a non-429 4xx
// as permanent, so the whole batch would be one Error line and ten lost
// notifications. Five near-maximal embeds must therefore come out as more
// than one message even though five is under the ten-embed cap.
func TestBatcherSplitsOnTheMessageCharacterBudget(t *testing.T) {
	got, mu, emit := collector()
	clk := &fakeBatchClock{}
	b := newBatcher(batchWindow, clk, emit, testLogger{})
	big := strings.Repeat("あ", 2000) // 2000 runes each; three of them exceed 6000
	for i := range 5 {
		b.Add(Embed{
			Title:       "Stream Found",
			Description: big,
			Opts:        SendOptions{Event: "found", JobID: itoa(i), Tier: TierLow},
		}, "", nil)
	}
	clk.fire()

	mu.Lock()
	defer mu.Unlock()
	if len(*got) < 2 {
		t.Fatalf("five 2000-rune embeds came out as %d message(s) — the per-MESSAGE 6000 budget is "+
			"not being applied, and Discord would reject the POST as a permanent 400", len(*got))
	}
	seen := 0
	for i, m := range *got {
		if n := messageRunes(m.Embeds); n > limitTotal {
			t.Errorf("message %d is %d runes, over the %d Discord allows per message", i, n, limitTotal)
		}
		for _, e := range m.Embeds {
			if e.Opts.JobID != itoa(seen) {
				t.Errorf("embed out of order: got %q, want %q", e.Opts.JobID, itoa(seen))
			}
			seen++
		}
	}
	if seen != 5 {
		t.Errorf("%d embeds were delivered, want all 5 — splitting must never drop one", seen)
	}
}

// TestBatcherPassesNonBatchableThrough: an error never waits behind an open
// find window.
func TestBatcherPassesNonBatchableThrough(t *testing.T) {
	got, mu, emit := collector()
	clk := &fakeBatchClock{}
	b := newBatcher(batchWindow, clk, emit, testLogger{})
	addFound(b, "a")
	b.Add(Embed{Title: "Job Failed", Opts: SendOptions{Event: "error", JobID: "j9"}}, "", nil)
	mu.Lock()
	ok := len(*got) == 1 && len((*got)[0].Embeds) == 1 && (*got)[0].Embeds[0].Title == "Job Failed"
	mu.Unlock()
	if !ok {
		t.Fatalf("the error did not go out immediately: %v", *got)
	}
	clk.fire()
	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 2 {
		t.Fatalf("the find window did not close afterwards: %d messages", len(*got))
	}
}

// TestBatchIsLowTier: one important embed protects the whole message from the
// queue's drop-oldest-low-tier policy. effectiveTier is N1's derivation, and
// TierUnset is its zero value — there is no sentinel to invent here.
func TestBatchIsLowTier(t *testing.T) {
	low := []Embed{found("a"), found("b")}
	if !batchIsLowTier(low) {
		t.Error("an all-low batch is not low-tier")
	}
	mixed := []Embed{found("a"), {Opts: SendOptions{Event: "auth", JobID: "j1"}}}
	if batchIsLowTier(mixed) {
		t.Error("a batch containing a normal-tier member was marked low-tier — the queue would drop it " +
			"under pressure ahead of a lone Stream Found")
	}
	if batchIsLowTier(nil) {
		t.Error("an empty message must not be reported low-tier")
	}
}

// TestBatchPingsOnce: the message carries ONE mention however many members
// were eligible — Discord's content and allowed_mentions are message-level,
// so there is nowhere to put a second, and ten pings for one backfill sweep
// is the noise batching exists to remove.
func TestBatchPingsOnce(t *testing.T) {
	got, mu, emit := collector()
	clk := &fakeBatchClock{}
	b := newBatcher(batchWindow, clk, emit, testLogger{})
	allowed := MentionParse("@here")
	addFound(b, "a")                       // not mention-eligible
	b.Add(found("b"), "@here", allowed)    // eligible
	b.Add(found("c"), "@here", allowed)    // eligible too
	clk.fire()

	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 1 {
		t.Fatalf("emitted %d messages, want 1", len(*got))
	}
	if (*got)[0].Mention != "@here" || (*got)[0].MentionAllowed == nil {
		t.Errorf("the batch carried (%q, %v), want the one eligible member's ping",
			(*got)[0].Mention, (*got)[0].MentionAllowed)
	}

	// And a window with no eligible member pings nobody.
	got2, mu2, emit2 := collector()
	clk2 := &fakeBatchClock{}
	b2 := newBatcher(batchWindow, clk2, emit2, testLogger{})
	addFound(b2, "a")
	clk2.fire()
	mu2.Lock()
	defer mu2.Unlock()
	if (*got2)[0].Mention != "" || (*got2)[0].MentionAllowed != nil {
		t.Error("a batch with no mention-eligible member still carried a ping")
	}
}

// TestBatcherFlushOnShutdown: a window open when shutdown begins must go out,
// not evaporate.
func TestBatcherFlushOnShutdown(t *testing.T) {
	got, mu, emit := collector()
	clk := &fakeBatchClock{}
	b := newBatcher(batchWindow, clk, emit, testLogger{})
	addFound(b, "a")
	b.Flush()
	mu.Lock()
	n := len(*got)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("Flush emitted %d messages, want 1", n)
	}
	clk.fire() // the disarmed timer must not double-emit
	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 1 {
		t.Errorf("the timer fired after Flush and emitted the batch twice")
	}
}

// TestBatcherWindowIsNotSliding: a steady trickle must not hold the window
// open forever. The timer is armed by the FIRST item only.
func TestBatcherWindowIsNotSliding(t *testing.T) {
	clk := &fakeBatchClock{}
	_, _, emit := collector()
	b := newBatcher(batchWindow, clk, emit, testLogger{})
	addFound(b, "a")
	addFound(b, "b")
	clk.mu.Lock()
	armed := len(clk.armed)
	clk.mu.Unlock()
	if armed != 1 {
		t.Errorf("%d timers armed for two items in one window, want 1 — a re-armed timer is a sliding "+
			"window and a trickle would never flush", armed)
	}
}
```

Plus the two tests that exercise the `Send` → batcher → queue → sender seam. They observe what reaches the **sender**, not an `emitFunc` the test wires in: that crosses Task 7's seam as well and proves the whole path, and after this task there is no hand-wired emit to observe anyway — the closure is built inside `newTargetQueue`.

```go
// batchSender records the Messages a queue actually delivered.
type batchSender struct {
	mu   sync.Mutex
	msgs []Message
}

func (s *batchSender) Send(msg Message) error     { return s.record(msg) }
func (s *batchSender) SendOnce(msg Message) error { return s.record(msg) }
func (s *batchSender) record(msg Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, msg)
	return nil
}
func (s *batchSender) delivered() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message(nil), s.msgs...)
}

// TestSendBatchesThroughTheTarget is the whole seam: three finds become ONE
// delivered message and an error jumps the open window.
func TestSendBatchesThroughTheTarget(t *testing.T) {
	rec := &batchSender{}
	clk := &fakeBatchClock{}
	m := newTestManagerWithClock(t, clk, time.Second, notificationTarget{sender: rec, key: "k1"})

	m.Send("Stream Found", "d", TypeInfo, nil, SendOptions{Event: "found", JobID: "a"})
	m.Send("Stream Found", "d", TypeInfo, nil, SendOptions{Event: "found", JobID: "b"})
	m.Send("Job Failed", "d", TypeError, nil, SendOptions{Event: "error", JobID: "c"})
	clk.fire() // close the find window
	m.Wait()   // flush, closeDrain, and block until the queue goroutine exits

	got := rec.delivered()
	if len(got) != 2 {
		t.Fatalf("delivered %d messages, want 2 (the error, then the coalesced finds)", len(got))
	}
	if got[0].Embeds[0].Title != "Job Failed" {
		t.Errorf("the error did not jump the open find window; first message was %q", got[0].logTitle())
	}
	if len(got[1].Embeds) != 2 {
		t.Errorf("the two finds arrived as %d embeds, want one 2-embed message", len(got[1].Embeds))
	}
}

// TestFilteredEventNeverEntersTheBatch: filters run BEFORE coalescing, so a
// target that does not subscribe to `found` never accumulates one — a batch
// must never deliver an embed the allowlist excluded.
func TestFilteredEventNeverEntersTheBatch(t *testing.T) {
	rec := &batchSender{}
	clk := &fakeBatchClock{}
	m := newTestManagerWithClock(t, clk, time.Second, notificationTarget{
		sender: rec,
		key:    "k1",
		events: map[string]bool{"error": true},
	})

	m.Send("Stream Found", "d", TypeInfo, nil, SendOptions{Event: "found", JobID: "a"})
	clk.fire()
	m.Wait()

	if got := rec.delivered(); len(got) != 0 {
		t.Errorf("a filtered-out event reached the batch: %v", got)
	}
}

// TestReloadFlushesARetiredTargetsWindow is the defect that put the batcher
// on targetQueue rather than on notificationTarget. applyTargets keeps a
// SURVIVING target's queue and discards the freshly built notificationTarget;
// a batcher hung on the latter would take the open window with it, on every
// unrelated config save, with no log line. Here the survivor's window must
// still be there after the Reload and still flush.
func TestReloadFlushesARetiredTargetsWindow(t *testing.T) {
	const url = "discord://1/aaa"
	clk := &fakeBatchClock{}
	m := newTestManagerWithClockFromConfig(t, clk,
		&config.MoomboxConfig{Notifications: []config.NotificationConfig{{URL: url}}})
	t.Cleanup(func() { m.Wait() })

	m.Send("Stream Found", "d", TypeInfo, nil, SendOptions{Event: "found", JobID: "a"})

	// A save that touches nothing about this target.
	m.Reload(&config.MoomboxConfig{Notifications: []config.NotificationConfig{{URL: url}}})

	m.targetsMu.RLock()
	q := m.targets[0]
	m.targetsMu.RUnlock()
	q.batch.Flush()
	if q.pending() == 0 {
		t.Error("the open find window did not survive an unrelated Reload — the batcher is on the " +
			"build-time notificationTarget, which applyTargets throws away")
	}
}
```

`newTestManagerWithClockFromConfig` is the same helper built from a config rather than from hand-made targets (it calls `buildTargets` first), so the Reload diff has real resolved keys to match on.

`newTestManagerWithClock` is `newTestManager`'s third form, added beside it in `internal/notifications/manager_test.go`: same body, with the `batchClock` threaded through `applyTargets` → `newTargetQueue`. Production keeps `realBatchClock{}`.

Run:

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/notifications/ -run Batch
```
Expected: `internal/notifications/batch_test.go:NN:NN: undefined: newBatcher` … `FAIL … [build failed]`.

- [ ] **Step 2: Implement**

`batch.go` per the interfaces above. `messageRunes` sums `embedRunes(&e)` over `toDiscordEmbed(e)` (Task 7's conversion, which clamps) so the splitter and the payload agree on size by construction. Then:

1. **`queue.go` — `targetQueue` gains `batch *batcher`, created in `newTargetQueue`:**
   ```go
   	q := &targetQueue{ … }
   	// The coalescing stage sits in front of THIS queue. It lives on the
   	// queue, not on the notificationTarget that built it, because
   	// applyTargets keeps a surviving target's queue and throws the freshly
   	// built target away — a batcher hung on the latter would take every open
   	// window with it on each unrelated config save, silently.
   	q.batch = newBatcher(batchWindow, clock, q.enqueueBatch, logger)
   	return q
   ```
   `newTargetQueue` takes the `batchClock` as a parameter; `applyTargets` passes `realBatchClock{}` and `newTestManagerWithClock` passes the fake. `enqueueBatch` builds `queued{msg: msg, tier: …}` — `TierLow` only when `batchIsLowTier(msg.Embeds)` — and calls the existing `q.enqueue`.
2. **`manager.go` — `Send`'s loop body** (replacing the `perItem` block Task 7 left there):
   ```go
   		mention, allowed := q.mentionFor(opts.Event)
   		q.batch.Add(msg.Embeds[0], mention, allowed)
   ```
   `msg` is the one-embed `Message` Task 7's `One` built; the batcher decides immediately-or-coalesce from `isBatchable(opts)` and re-wraps on the way out. `effectiveTier` is no longer computed in `Send` — `enqueueBatch` derives the item's tier from the embeds it actually carries.
3. **`manager.go` — `applyTargets`' `retired` loop**, which already runs outside `targetsMu` and already takes each queue's own lock:
   ```go
   	for _, q := range retired {
   		q.batch.Stop() // flush the open window before the queue stops accepting
   		q.stopDiscard()
   	}
   ```
   The flushed items land in a queue `stopDiscard` then drops, with N1's own "target removed — discarding its queued notifications" Warn naming the count. That is the honest outcome for a webhook the operator just deleted, and it is one line rather than a special case.
4. **`manager.go` — `Wait`**: after snapshotting `targets` under `RLock` and before the `q.closeDrain()` loop, `for _, q := range targets { q.batch.Flush() }`. The order is load-bearing: `enqueue` drops with a Warn once `q.closing` is set (`internal/notifications/queue.go`), so a flush after `closeDrain` would emit the batch straight into the drop path. **`BeginShutdown`** flushes the same way, under its own `RLock`, before setting the flag — a window open when shutdown begins is delivered single-attempt rather than evaporating.
5. **`manager_test.go`** — `newTestManagerWithClock(t, clk, waitTimeout, targets…)` beside the two existing constructors; `newTestManager` delegates to it with `realBatchClock{}`.

- [ ] **Step 3: Verify**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -race ./internal/notifications/
```
`-race` here specifically: the batcher is the only new concurrency in the arc, and it holds two locks in sequence (`b.mu`, then `q.mu` via `emit`). Expected `ok`, with N1's `queue_test.go` and `ratelimit_test.go` still green — the batcher sits in front of the FIFO and must not have changed how the FIFO behaves. Then the Global gate block.

**Mutations to run:**
1. Re-arm the timer on every `Add` → `TestBatcherWindowIsNotSliding` fails.
2. Make `splitMessages` return one message regardless of count → `TestBatcherSplitsAtTen` fails.
3. Make `splitMessages` split on the embed count only, ignoring `messageRunes` → `TestBatcherSplitsOnTheMessageCharacterBudget` fails.
4. Make `messageRunes` sum the UNCLAMPED embeds → the same test fails on the per-message assertion once an embed exceeds a single limit.
5. Make `batchIsLowTier` return true when *any* member is low → `TestBatchIsLowTier` fails.
6. Batch `auth` regardless of `JobID` → `TestIsBatchable` fails.
7. Skip the `timer.Stop()` in `Flush` → `TestBatcherFlushOnShutdown` fails on the double emit.
8. Route batchable sends past `q.allows` → `TestFilteredEventNeverEntersTheBatch` fails.
9. Carry every eligible member's mention instead of the first → `TestBatchPingsOnce` fails.
10. Move the `Flush` in `Wait` to *after* the `closeDrain` loop → `TestSendBatchesThroughTheTarget` fails (the coalesced finds never arrive; the queue Warns "the target is shutting down" instead).
11. Put the batcher back on `notificationTarget`, created in `buildTargets` → `TestReloadFlushesARetiredTargetsWindow` fails.
12. Drop `q.batch.Stop()` from `applyTargets`' `retired` loop → extend `TestReloadFlushesARetiredTargetsWindow` with a second target that the Reload REMOVES, and assert its `stopDiscard` Warn counted the flushed embed; without the `Stop()` the count is one lower.

- [ ] **Step 4: Commit**

Pathspec: `internal/notifications/batch.go internal/notifications/batch_test.go internal/notifications/queue.go internal/notifications/manager.go internal/notifications/manager_test.go`.
Message: `feat(notifications): coalesce found/added/per-job auth into one message` plus the two trailers from Global Constraints, verbatim.

---

## Task 9: Docs and the node README recount

**Files:**
- Modify: `docs/spec/operations.md` — the Notifications section, `:442-551` at `242d921c` (`### Configuration` `:444`, `### URL Formats` `:448`, `### Event Types` `:457`, `### Credential Notifications` `:504`, `### Dispatch Behavior` `:523`, `### Notification Type Colors` `:538`). N1 **rewrote** Dispatch Behavior into eleven bullets that already document the per-target FIFO, the drop policy, the rate bucket, the embed limits and the shutdown pair — this task **extends** that list, it does not replace the old five.
- Modify: `docs/spec/user-interfaces.md:229` (the `settings_notifications.go` row) and the Settings rows near `:221` and `:376`; add the deep link beside the `/api/jobs` surface notes. Unchanged by N1, so these are the verified numbers.
- Modify: `README.md:676-680` (the `[[notifications]]` example inside the Webhook Notifications section, `:673-687` at `242d921c`)
- Modify: `web/tests/README.md` — the "Sixteen suites" prose (`:4`) and the per-suite table (`:37`), the inline per-suite DOM breakdown (`:52-58`), the fenced without-jsdom count block (`:66`) and the with-jsdom prose sentence (`:72`). N1 touched no file under `web/tests/`, so the baseline below is unchanged.
- No code changes.

**What goes where:**

`operations.md` — a new `### Target Options` subsection after `### URL Formats` covering `enabled` (mute, kept in config, `NewManager` logs the skip, and `HasTargets` reports false when **every** target is muted, which correctly short-circuits all four producer guards: `cmd/moombox/helpers.go` update-available, `cmd/moombox/main.go` disk, and both `cmd/moombox/monitor_callbacks.go` Stream Found sites), `mention` + `mention_events` (the four accepted forms, the `allowed_mentions` each produces, the alias rule, the three-way default/never/explicit semantics with the exact default list), and `network.public_url` (what the title link becomes, where the platform link goes, that it is read at send time so a save applies without a restart, and that it does nothing when a send carries no author). A new `### Batching` subsection: which events coalesce, the 5 s non-sliding window, the split on **both** Discord per-message caps (ten embeds and 6000 characters) with overflow rolling forward and nothing dropped, that a batch is one queue item **so the FIFO's drop policy and the ordering between messages are unchanged — with the one exception that a non-batchable send (an error, a cancel) arriving while a window is open is delivered immediately, ahead of the embeds still coalescing**, that a batch is low-tier only if every member is, that it pings once, that a retired target's window is flushed into the queue its removal then discards, and that `Wait`/`BeginShutdown` flush before the drain closes. Two bullets are added to N1's **existing** Dispatch Behavior list rather than written as a replacement: one naming the `Message` (a delivery is now one message of one-to-ten embeds, and the 6000 budget is per message), one naming the batching window. Amend the "empty filter = all events" sentence in the event-table preamble to say both UIs now agree and name the mute.

`user-interfaces.md` — the `settings_notifications.go` row gains the three controls and the `m` key; the Settings paragraph gains the `public_url` row; a short paragraph on the `#job=<id>` deep link (what produces it, that the SPA clears the hash, that an unknown id toasts).

`README.md` — add `enabled`, `mention` and `mention_events` to the `[[notifications]]` example (`:676-680`) and a short "Mentions and dashboard links" paragraph after the existing `docs/spec/operations.md` pointer. **Arc N1 already landed the three fixes the pre-N1 draft of this plan claimed here** — `:67` now reads "Discord webhook notifications — Rich embeds …", the phantom `live` event is gone from the example (`events = ["found", "finished", "error"]`), and the inline event list has been replaced by that pointer. Do not re-edit any of them; re-editing would undo N1's work and collide on merge.

**Citation gate.** Every backticked symbol added here must resolve: `PublicURL`, `NotificationConfig`, `IsEnabled`, `ParseMention`, `DefaultMentionEvents`, `ResolveMentionEvents`, `ValidatePublicURL`, `JobDeepLink`, `mentionFor`, `setMention`, `AllowedMentions`, `MentionParse`, `Message`, `Embed`, `isBatchable`, `batchWindow`, `messageRunes`, `internal/notifications/message.go`, `internal/notifications/batch.go`, `web/public/modules/settings.js`, `internal/tui/settings_notifications.go`. Each symbol must appear next to its **declaring** file path through one of the connectors the gate accepts (``Foo`` (`internal/…/x.go`), ``Foo`` in `internal/…/x.go`, …) — a citation pointing at a *caller* rather than the declaration fails exactly the way a missing symbol does (`identRe` + `connectorRe` + `fileFacts.declared`, `internal/docs/citations_test.go`). Unexported names (`mentionFor`, `isBatchable`, `batchWindow`) are fine under that rule; a bare backticked `isBatchable` with no nearby `internal/notifications/batch.go` is not. Prefer symbol names over line numbers; `TestSpecDocAbsenceClaimsHold` means any "there is no X" sentence must still be true after this arc.

- [ ] **Step 1: Recount the node suite (no arithmetic)**

```bash
cd D:/Git/Moombox/web/tests && command ls node_modules/jsdom >/dev/null 2>&1 && echo "jsdom present" || echo "jsdom absent"
cd D:/Git/Moombox && timeout 300 node --test web/tests/*.test.mjs 2>&1 | tail -20
```
Record the `tests` / `pass` / `fail` / `skipped` line **with** jsdom. Then move `web/tests/node_modules` aside, re-run, record the **without**-jsdom numbers, and move it back. Baseline at `1d2df1d4` and unchanged at `242d921c` (N1 touched no file under `web/tests/`): `tests 291 / pass 131 / skipped 160` without jsdom, `291 / 291 / 0` with. Confirm that baseline reproduces before trusting the new numbers. Also record the per-suite counts for the two new files (`settings-notifications.test.mjs`, `job-deeplink.test.mjs`) from `--test-reporter=spec`, since the README's breakdown lists them individually.

- [ ] **Step 2: Write the docs**

Both new suites need jsdom, so they join the "yes" row of `web/tests/README.md`'s table (`:37`), the prose count at `:4` ("Sixteen suites" → the live number; the directory holds 24 test files today and 26 after this arc, 18 of them jsdom suites), and the inline DOM-test breakdown at `:52-58`. The fenced block at `:66` and the prose sentence at `:72` take the two recounted totals.

- [ ] **Step 3: Verify**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/docs/
```
Expected `ok  github.com/vampiricwulf/Moombox/internal/docs`. A failure names the exact unresolvable citation — fix the doc, never the allowlist.

- [ ] **Step 4: Commit**

Pathspec: `docs/spec/operations.md docs/spec/user-interfaces.md README.md web/tests/README.md`.
Message: `docs: target options, batching, the deep link, and a recounted node suite` plus the two trailers from Global Constraints, verbatim.

---

## Task 10: Full gates, self-review, and plan deletion

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

Also, once only, the producer-side proof that Task 7's seam stayed inside its package:

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/worker/ ./cmd/moombox/
```

Expected: `gofmt`, `go mod tidy -diff` and `staticcheck` print nothing; all three builds succeed; five `ok` lines plus the two producer packages; the node run's totals match what Task 9 wrote into `web/tests/README.md`.

Then the same node run **without** jsdom, confirming the second set of README numbers.

- [ ] **Step 2: Self-review against the spec**

Walk `docs/superpowers/specs/2026-09-27-discord-webhooks-design.md` §3 items 1-7 and tick each against the diff:

1. Four keys, defaults, `validateOrNormalize`, `config.example.toml`, `data-and-storage.md`, the settings skill — Task 1. `mode` deliberately absent (N3).
2. Mentions in the payload per form; a batch pings once — Tasks 2, 7 and 8. The `allowed_mentions` object comes from N1's `MentionParse` at every layer; nothing in this arc builds one.
3. Dashboard link rewritten at send time from the live config; the SPA opens and clears — Tasks 2, 3, 6.
4. Batching: separate-mode only, the five excluded classes, filters first, one queue item, both per-message caps, tier rule, shutdown flush — Tasks 7 and 8.
5. Both editors: `enabled`, `mention`, `mention_events`, the "All events" label, the `public_url` row, test-send unchanged — Tasks 4 and 5.
6. Tests: validators, mention payload, default-list rule, batching, deep link, both round-trips, README recount — Tasks 1-9.
7. Docs — Task 9.

Two deliberate deviations to record rather than re-litigate: spec §3.6 asks for "TUI a `tea` test" and Task 5 uses direct `&SettingsModel{}` literals instead (no existing settings test in `internal/tui` drives a `tea.Program`, and the handlers under test are pure model methods); and Task 6's `GET /api/jobs/<id>` fallback exceeds the spec sentence for the reason Task 6 states.

Then the four mechanical checks:

- **No placeholders** — `grep -rn "TODO\|FIXME\|XXX\|<placeholder>"` over the diff must return nothing new.
- **Every test named in this plan exists in the tree** — the grep above cannot see a *missing* test. Walk each task's Step 1 and confirm the named function is present and running (not skipped): `TestMentionAllowedPerForm`, `TestReloadCarriesTheMentionToASurvivingTarget`, `TestNotificationsApplyUsesTheSharedDecode`, `TestNotifEditPreservesFieldsTheEditorDoesNotShow`, `TestOneEmbedMessageIsByteIdenticalToN1`, `TestMultiEmbedMessageEmitsEveryEmbed`, `TestBatcherSplitsOnTheMessageCharacterBudget`, `TestBatchPingsOnce`, `TestReloadFlushesARetiredTargetsWindow`, `TestSendBatchesThroughTheTarget`, `TestFilteredEventNeverEntersTheBatch`, and the eleven jsdom tests of Task 4 plus the six of Task 6.
- **Type consistency** — `MentionEvents` is `*[]string` at every layer (struct, route decode, both editors, both round-trip tests); `Enabled` is `*bool` with `IsEnabled()` the only reader; `MentionAllowed` is `*AllowedMentions` at every layer, produced only by `MentionParse` and never mutated after.
- **The per-target surface** — confirm `internal/notifications/{manager.go,queue.go}` carry exactly the **seven** places the Architecture names: `targetQueue`'s four new fields, `newTargetQueue` copying and creating them, `mentionFor`/`setMention`, `applyTargets`' survivor arm, its `retired` loop, `Send`'s per-item write plus batcher entry, and the `Wait`/`BeginShutdown` flush. More than seven means batching or mentions leaked into the delivery core; fewer means a flush or a reload path was missed. `Manager` must carry exactly one new field, `publicURL`.
- **N1's suite is untouched** — `payload_test.go`, `limits_test.go`, `queue_test.go`, `ratelimit_test.go`, `discord_test.go`, `events_test.go`, `manager_test.go` and `manager_dispatch_test.go` may gain *adaptations* (Task 7's four fakes, `newTestManagerWithClock`) but no assertion of N1's may have been weakened or deleted. Read the diff of those files specifically; it is the one place where making this arc's tests pass could quietly cost N1 a guarantee.

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
