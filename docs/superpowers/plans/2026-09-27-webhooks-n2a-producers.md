# Arc N2a — producers (truth, unification, recovery halves) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the job-shaped Discord embeds tell the truth and say it once. Today ten near-duplicate builders scattered across `internal/worker`, `internal/web/routes` and `cmd/moombox` produce four families of embed with different titles and different field sets depending on which entry point fired ("Twitch Video Added" vs "Video Added" vs a CLI add with no channel and no thumbnail; "Download Cancelled" vs "Job Cancelled"; two "Download Finished" builders that disagree about what a finished job is worth saying). A finished job whose tail is incomplete, whose chat capture stopped early, or whose staging still holds set-aside recordings still sends a green "Successfully archived"; a failed job says nothing about whether the operator should press Retry or Resume. And four alerts — sidecar death, disk warning, disk-monitoring failure, channel not responding — have no close, so every incident an operator opens stays open forever. This arc unifies the five families behind pure builders in `internal/notifications`, puts the outcome truth on the `finished`/`error` sends that already exist, and adds the four recovery halves with their vocabulary.

**Architecture:** A new `internal/notifications/builders.go` holds `JobFacts` (a flat, database-free description of a job), `Part`, `TrimFacts` and five pure builders — `JobAdded`, `StreamFound`, `JobCancelled`, `DownloadFinished`, `TrimCreated`. Each returns exactly the five values `Sender.Send` takes, in that order, so a producer writes `notifier.Send(notifications.JobAdded(facts))` and Go's f(g()) call form does the rest. Producers fill `JobFacts` from their own types: `worker.NotifyFacts(*database.Job)` is the one mapper the three producer packages share (`internal/web/routes` and `cmd/moombox` already depend on `internal/worker`). The `finished` and `error` sends gain outcome fields from state the row already carries (`incomplete_tail`, `chat_status`, `ScanAsides`, `HasStagingFiles`, the `mux` error prefix). The recovery halves are three small testable state holders in `cmd/moombox` — `sidecarAlerts` (a second `sidecar.SubscribeHealth` subscriber with a 60 s debounce behind an injected timer), `diskAlerts` (the disk block of the stats ticker, lifted out of the closure so the all-clear can be tested), and a `channelHealthNotifiers` pair fed by a new `onHealthy` hook on `internal/monitor`'s `healthTracker`. Four new event keys join `EventGroups`, the `settings.js` mirror and the operations table; the TUI derives its editor from `EventGroups` and needs no edit.

**Tech Stack:** Go 1.27 (no CGo, pure-Go deps), chi/v5 for routing, `charm.land/bubbletea/v2` + `bubbles/v2` + `lipgloss/v2` for the TUI, vanilla ES modules + Shoelace v2.16 for the dashboard, goja (via `internal/webtest.SettingsVM`) for evaluating the shipped `settings.js` from a Go test, Node 24 `node:test` + jsdom for the frontend suite, Git Bash on Windows for every command below.

**Spec:** `docs/superpowers/specs/2026-09-27-discord-webhooks-design.md` @ `1d2df1d4` — §0 rulings (neutral titles with the platform in the author line; the dead Twitch finished image dropped; recovery events are new keys with aliases to their alert keys; sidecar down debounced at 60 s), §2 "Arc N2a — producers (truth, unification, recovery halves)" in full, §5 order and shared files. Audit (gitignored, read-only): `.superpowers/sdd/2026-09-26-webhooks/audit.md` — §1 inventory rows #1–#41, §2 gaps A1/A2/A3/A5/A6 and M8, §3 findings C1–C5 and the Content-quality table, §4 seams (the web/Go vocabulary mirror, the coverage root cause).

**Depends on Arc N1 (merged first).** This plan consumes N1's interfaces **by the spec's names** and must be re-read against the merged tree before Task 1 starts:

- `notifications.Sender` — `interface { Send(title, description string, ntype NotificationType, fields []Field, opts SendOptions) }`; `*Manager` satisfies it and `Send` on a nil `*Manager` is a no-op.
- `notificationtest.Recorder` (`internal/notifications/notificationtest`) — constructed with `notificationtest.New()` (`recorder.go:54`); implements `Notifier` and therefore `Sender`; records `Call{Title, Description, Type, Fields, Opts}` with a `Call.Field(name) (string, bool)` lookup (`recorder.go:35`); exposes `Calls()`, `ByEvent(e)`, `Reset()`, `HasTargets() == true`, `Reload`, `BeginShutdown`, `Wait`; safe for concurrent use, and `Calls()`/`ByEvent()` deep-copy `Fields`.
- `ClampRunes(s string, limit int) string` (`internal/notifications/limits.go:37`) — the rune-safe cut, exported by N1 **for this arc's Description excerpt**; it marks the cut with `…` and counts CHARACTERS, not bytes.
- `SendOptions{URL, Event, Thumbnail, Image, Author *Author, Platform, JobID, Tier, Mention, MentionAllowed}` with `Author{Name, IconURL, URL string}`.
- `EscapeMarkdown(s string) string`.
- The alias-known rule: `KnownEvents` contains alias values as well as `EventGroups` members.
- N1's payload clamp is the safety net (Discord's 1024-character field limit); this arc's 300-character description **excerpt** is a separate product-level budget that spends the SAME `ClampRunes`, so the two can never disagree about where a multi-byte character ends. This arc writes no truncator of its own.

**Never cite a line number inside `internal/notifications/manager.go` or `internal/notifications/discord.go`** — N1 rewrites both. Every line number in this plan that points at a producer file was verified at `1d2df1d4`; verify each one again after `git merge main`, because N1 edits `orchestrator_mux.go`, `stream_processor.go`, `orchestrator_twitch.go`, `orchestrator.go`, `monitor_callbacks.go` and every consumer's notifier field type.

---

## Global Constraints

From spec §2 Constraints, plus the standing chain rules. Every task's requirements implicitly include this section.

**Spec §2 (Arc N2a):**

- **No manager/discord changes.** `internal/notifications/manager.go` and `internal/notifications/discord.go` are N1's files and are not edited by this arc. `builders.go` is new; `events.go` gains keys and aliases (Task 6) and nothing else.
- **No config keys.** `internal/config/types.go`, defaults, `validateOrNormalize`, `config.example.toml` and `docs/spec/data-and-storage.md` belong to N2b and are not touched.
- **The builders package imports nothing from `internal/worker` or `internal/database`.** `internal/notifications` may import `internal/utils` (for `FormatFileSize` / `FormatDurationHuman`) and nothing else new. This is load-bearing: `internal/tui` imports `internal/notifications`, so a worker import here would breach the TUI import fence through the back door.

**Standing chain rules:**

- `go 1.27`, no CGo, pure-Go dependencies; Windows x64 + Linux x64 + Linux arm64 must build (`GOOS=linux GOARCH=amd64 go vet ./...`, `GOOS=linux GOARCH=arm64 go build ./...` are part of the merge candidate). `go mod tidy -diff` prints nothing.
- LF line endings in every file this arc touches.
- Every commit's LAST TWO LINES are exactly:
  ```
  Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
  ```
  That is the project's rule and it takes precedence over any attribution reminder in an implementer's own context, whatever model name that reminder shows.
- **The citation gate.** `internal/docs/citations_test.go` scans `docs/spec/*.md` (six docs), `SPEC.md`, `CLAUDE.md`, `README.md` and the seven skills. Any task that edits one of those, or renames/moves/deletes a Go symbol, gates `go test ./internal/docs/`. A backticked token starting with `internal/`, `cmd/`, `web/`, `tools/`, `docs/`, `bgutil-sidecar/` or `.github/` is checked as a path; a backticked `Name`, `pkg.Name` or `Type.Method` beside such a path is checked as a symbol.
- **The TUI import fence.** `internal/tui` never imports `internal/web`, `internal/web/routes`, `internal/bgutils` (or `internal/bgutils/sidecar`), `internal/monitor` or `internal/worker`. It imports `internal/notifications` today and must keep being able to — see the builders constraint above.
- **The DB layer is untouched.** No schema change, no new column, no change to write cadence, no change to `UpdateJobFields`. The only reads added are ordinary `GetJob`/`ScanAsides`/`HasStagingFiles` calls the packages already make.
- **The anonymous logger interface** (`Debug/Info/Warn/Error(msg string, args ...any)`) stays anonymous per struct — never extracted to a named interface. Every goroutine (including a `time.AfterFunc` callback) carries an inline `defer func() { if r := recover(); r != nil { … } }()`.
- Helpers are defined in the task that first uses them (staticcheck U1000 is a hard gate — an unused helper is a failure, not a warning).
- TDD per task: the failing test is written and run RED before the change, and the expected failure text is recorded. Every new assertion names the mutant it kills.
- **One commit per task, with the pathspec ON the commit**: `git add <files> && git commit -m "…" -- <same files>`. No stash / checkout / rebase / reset / amend.
- **Implementers never run the full suite.** `go test ./...` is the controller's single serialized gate, in Task 9 only. Every task runs narrow package tests.
- Every `go` command carries `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp` and `-timeout 300s`.
- Never `python3 -` heredocs (the uv shim hangs) — use `perl` or `node -e`. Scratch `node --test` probes carry `--test-timeout`. `ls` is aliased to classify — use `command ls`.
- **Write/Edit only** for file changes; no `sed -i`, no shell redirection into a tracked file.
- Protected behaviour stays: the real-time progress pipeline and DB write cadence (cheaper or more often, never rarer); `monitors.probe_cooldown` default 0; the BotGuard interpreter gate; `/retry` vs `/resume` gates never shared or widened; extraction mirrors yt-dlp; update-path compatibility; never kill processes by image name; never `rm -rf` under `%TEMP%`; no Shoelace bundling.
- User-facing strings: read the twin UI's exact output before mirroring it.

**Arc-N2a-specific rules:**

- **The builders' return tuple IS `Sender.Send`'s parameter list**, in order: `(title, description string, ntype NotificationType, fields []Field, opts SendOptions)`. Producers call `notifier.Send(notifications.JobAdded(f))`. If a builder's signature drifts from `Send`'s, every producer breaks at compile time — which is the point. Do not "improve" the tuple into a struct.
- **Neutral titles, platform in the author line** (spec §0): `"Job Added"`, `"Stream Found"`, `"Job Cancelled"`, `"Download Finished"`, `"Trim Created"`. No platform word in any of the five titles. The System titles (disk, sidecar, channel, update, crash) keep their descriptive names.
- **Every job send sets `Platform`, `JobID`, `URL` (the video's platform page) and `Author`** (channel name + avatar + channel page) when the fact is known. The builders never read config; N2b's manager rewrites `URL`/`Author.URL` when `public_url` is set.
- **`Image` is dropped for Twitch finished sends** (ruling: the preview URL 404s once the stream ends). No multipart upload in this arc.
- **No new sends in the job families.** `JobAdded`/`StreamFound`/`JobCancelled`/`DownloadFinished`/`TrimCreated` replace existing sends one for one. The only NEW sends this arc adds are the four recovery halves.
- **`worker.NotifyFacts` is the one job→facts mapper.** Three producer packages, one function. A site that needs more than it provides assigns to the returned struct; it does not write a second mapper.

---

## Branch and worktree

Cut from `main` **after N1 merges**, in parallel with N2b.

```bash
cd /d/Git/Moombox
git worktree add -b webhooks-n2a-producers .worktrees/webhooks-n2a-producers main
cp internal/bgutils/embed/node-windows-amd64.gz \
   internal/bgutils/embed/node-linux-amd64.gz \
   internal/bgutils/embed/node-linux-arm64.gz \
   internal/bgutils/embed/sidecar.tar.gz \
   .worktrees/webhooks-n2a-producers/internal/bgutils/embed/
mkdir -p .worktrees/webhooks-n2a-producers/internal/cipher/testdata
cp internal/cipher/testdata/*.js .worktrees/webhooks-n2a-producers/internal/cipher/testdata/
cd .worktrees/webhooks-n2a-producers/web/tests && npm ci --no-audit --no-fund
```

Every path below is relative to `D:/Git/Moombox/.worktrees/webhooks-n2a-producers`.

**Before Task 1**, read the merged tree and confirm the six N1 interfaces listed above exist with those names. If `notifications.Sender` does not exist, or `notificationtest.Recorder` lacks `ByEvent`, STOP and report — every task below depends on them.

---

## Shared files with N2b (spec §5)

| File | N2a's hunk | N2b's hunk |
|---|---|---|
| `web/public/modules/settings.js` | `NOTIFICATION_EVENT_GROUPS` at the top (Task 6) | the target-editor body |
| `docs/spec/operations.md` | the § Event Types table rows (Task 6) | mentions / batching / `enabled` / `public_url` |
| `internal/notifications/` | `builders.go` (new), `events.go` | `manager.go` |
| `SPEC.md` | the Notifications-Detail event list (Task 6) | — |

Whichever arc merges second merges `main` first (Task 9 Step 1). Conflicts should be adjacent-hunk only.

---

## File Structure

| File | Task | Responsibility after this arc |
|---|---|---|
| `internal/notifications/builders.go` — **new** | 1, 4, 5 | `JobFacts`, `Part`, `TrimFacts`, the five builders, and the private helpers `jobOpts`, `authorFor`, `displayName`, `resolutionLabel`, `addIDField`. No truncator of its own — the Description excerpt spends N1's `ClampRunes`. |
| `internal/notifications/builders_test.go` — **new** | 1, 4, 5 | Table tests per builder per platform; the escape rule; the excerpt's rune boundary; the truth fields. |
| `internal/notifications/events.go` | 6 | `EventGroups` System group gains `sidecar_down`, `sidecar_restored`, `disk_ok`, `channel_healthy`; `eventAliases` **gains three entries in place** (`disk_ok → disk_warning`, `channel_healthy → channel_unhealthy`, `sidecar_restored → sidecar_down`) beside the two N1 already carries; new exported reader `AliasOf`. |
| `internal/worker/notify_facts.go` — **new** | 2 | `NotifyFacts(*database.Job) notifications.JobFacts` — the one mapper, including the YouTube watch-URL fallback and the YouTube channel-page derivation. |
| `internal/worker/notify_facts_test.go` — **new** | 2 | The mapper's per-platform output and both fallbacks. |
| `internal/web/routes/jobs.go` | 2, 3 | The Twitch add (`:772`), YouTube add (`:901`) and cancel (`:959`) sends become builder calls; the `notifier` parameter is `notifications.Sender` (N1). |
| `internal/web/routes/jobs_test.go` | 2, 3 | `jobsFixture` gains a `notify *notificationtest.Recorder` wired into `JobRoutes`. |
| `internal/web/routes/job_notifications_test.go` — **new** | 2, 3 | HTTP-level Recorder assertions for the two adds and the cancel. |
| `cmd/moombox/addvideo.go` | 2 | Both CLI adds call `notifications.JobAdded(cliAddedFacts(...))`; the YouTube add gains a thumbnail, the Twitch live add gains channel + channel page. |
| `cmd/moombox/job_notifications.go` — **new** | 2, 3 | `cliAddedFacts` and `notifyStreamFound` — the two seams that let `cmd/moombox`'s job sends be tested. |
| `cmd/moombox/job_notifications_test.go` — **new** | 2, 3 | Recorder tests for both. |
| `cmd/moombox/monitor_callbacks.go` | 3, 8 | The two `found` sends become `notifyStreamFound`; `unhealthyNotify` becomes `channelHealthNotifiers`, wired with `SetOnChannelHealthy`. |
| `internal/worker/worker.go` | 3, 5 | `handleCancellation`'s send becomes `notifications.JobCancelled`; `setJobError`'s "Job Failed" gains Stage / Staging / Set-aside recordings; new `errorStage` helper. |
| `internal/worker/orchestrator_mux.go` | 4 | One `sendDownloadFinished` + `finishedFacts` replaces the two inline finished builders; new `formatSelectionLabel`, `trimmedRangeLabel`; N1's `finishedImage` and the `descMaxLen` clamp line are deleted with the sends that used them. |
| `internal/worker/orchestrator_mux_test.go` | 4 | N1's `TestFinishedImageIsDroppedForTwitch` and `TestDescriptionExcerptCutsOnARuneBoundary` deleted with the code they pin. |
| `internal/worker/trim.go` | 5 | Both "Trim Created" sends become `notifications.TrimCreated`. |
| `internal/worker/job_notifications_test.go` — **new** | 3, 4, 5 | Recorder tests for cancel, both finished shapes, the truth fields, both trims and Job Failed. |
| `internal/monitor/health.go` | 8 | `healthTracker` gains `onHealthy`; `recordSuccess` fires it once per notified streak. |
| `internal/monitor/feed.go`, `decapi.go`, `twitch.go` | 8 | `SetOnChannelHealthy`, beside each `SetOnChannelUnhealthy`. |
| `internal/monitor/health_test.go` | 8 | Gains the tracker's healthy edge beside the three streak/prune tests it already holds. **Append — the file exists (94 lines).** |
| `cmd/moombox/sidecar_alerts.go` — **new** | 7 | `sidecarAlerts`, the 60 s debounce behind an injected timer, `wireSidecarAlerts`. |
| `cmd/moombox/sidecar_alerts_test.go` — **new** | 7 | The debounce, the flap, the restore. |
| `cmd/moombox/disk_alerts.go` — **new** | 8 | `diskAlerts` — the disk notification decision lifted out of the stats-ticker closure. |
| `cmd/moombox/disk_alerts_test.go` — **new** | 8 | Warning → all-clear, read failure → monitoring recovered, no orphan closes. |
| `cmd/moombox/channel_health_test.go` — **new** | 8 | The notifier pair, including the sibling-suppressed case. |
| `cmd/moombox/main.go` | 7, 8 | The stats ticker's disk block delegates to `diskAlerts`; `wireSidecarAlerts` is called and its unsubscribe deferred. |
| `web/public/modules/settings.js` | 6 | `NOTIFICATION_EVENT_GROUPS` System group gains the four labelled keys. |
| `internal/tui/settings_notification_vocab_parity_test.go` — **new** | 6 | Pins `notifications.EventGroups`, the TUI's derived `allNotifEvents`, and the shipped `settings.js` mirror against each other. |
| `docs/spec/operations.md` | 6 | Four new event rows; the `finished` and `error` rows say what the truth fields add. |
| `SPEC.md` | 6 | The Notifications-Detail event list gains the four keys. |
| `docs/superpowers/plans/2026-09-27-webhooks-n2a-producers.md` | 9 | Deleted — git history is the archive. |

---

### Task 1: `internal/notifications/builders.go` — the five pure builders

The five families exist today as ten hand-rolled `Send` calls that disagree with each other (audit §3 C1–C5). This task writes the single source for all five, as pure functions with no database, no worker, no filesystem and no config — so every later task is a one-line substitution plus a Recorder assertion.

**Files:**
- Create: `internal/notifications/builders.go`
- Create: `internal/notifications/builders_test.go`

**Interfaces:**
- Consumes (from N1, in this same package): `NotificationType` with `TypeInfo`/`TypeSuccess`/`TypeWarning`/`TypeCancelled`, `Field`, `FieldBuilder` + `NewFieldBuilder`/`Add`/`AddInline`/`AddIf`/`AddInlineIf`/`Build`, `SendOptions`, `Author`, `EscapeMarkdown(s string) string` (`limits.go:89`), `ClampRunes(s string, limit int) string` (`limits.go:37`), `IDLabel(platform string) string`.
- Consumes: `utils.FormatFileSize(bytes int64) string` and `utils.FormatDurationHuman(d time.Duration) string` (`internal/utils/format.go:12`, `internal/utils/format.go:72`).
- Produces, in package `notifications` — **these exact names and signatures are used unchanged by Tasks 2–5:**
  - `type JobFacts struct { … }` (every field documented in Step 3)
  - `type Part struct { File, Quality string; Size int64; Duration time.Duration; Width, Height, Fps int }`
  - `type TrimFacts struct { TimeRange string; Duration time.Duration; Size *int64; Parts int }`
  - `func JobAdded(f JobFacts) (string, string, NotificationType, []Field, SendOptions)`
  - `func StreamFound(f JobFacts) (string, string, NotificationType, []Field, SendOptions)`
  - `func JobCancelled(f JobFacts) (string, string, NotificationType, []Field, SendOptions)`
  - `func DownloadFinished(f JobFacts, parts []Part) (string, string, NotificationType, []Field, SendOptions)`
  - `func TrimCreated(f JobFacts, t TrimFacts) (string, string, NotificationType, []Field, SendOptions)`
  - unexported: `displayName`, `authorFor`, `jobOpts`, `addIDField`, `resolutionLabel`, and the constant `descriptionExcerptLen`

Every return tuple is `Sender.Send`'s parameter list in order, so a producer writes `n.Send(notifications.StreamFound(f))`.

- [ ] **Step 1: Write the failing test**

Create `internal/notifications/builders_test.go`:

```go
package notifications

import (
	"strings"
	"testing"
	"time"
)

// ytFacts and twFacts are the two platform shapes every builder is exercised
// against. They carry a markdown-hostile title on purpose: the escape rule is
// the one thing a reader of a Discord channel notices when it is missing.
func ytFacts() JobFacts {
	return JobFacts{
		ID:               "dQw4w9WgXcQ",
		VideoID:          "dQw4w9WgXcQ",
		Platform:         "youtube",
		Title:            "【歌枠】 *live* _test_",
		Channel:          "Some Channel",
		ChannelURL:       "https://www.youtube.com/channel/UC123",
		ChannelAvatarURL: "https://yt3.example/avatar.jpg",
		URL:              "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		ThumbnailURL:     "https://i.ytimg.com/vi/dQw4w9WgXcQ/maxresdefault.jpg",
	}
}

func twFacts() JobFacts {
	return JobFacts{
		ID:               "tw_12345",
		VideoID:          "12345",
		Platform:         "twitch",
		Title:            "Streamer — playing something",
		Channel:          "Streamer",
		ChannelURL:       "https://twitch.tv/streamer",
		ChannelAvatarURL: "https://static.example/profile.png",
		URL:              "https://twitch.tv/streamer",
		ThumbnailURL:     "https://static.example/preview.jpg",
		Category:         "Just Chatting",
	}
}

// fieldValue returns the value of the named field, and whether it was present.
func fieldValue(fields []Field, name string) (string, bool) {
	for _, f := range fields {
		if f.Name == name {
			return f.Value, true
		}
	}
	return "", false
}

func mustField(t *testing.T, fields []Field, name string) string {
	t.Helper()
	v, ok := fieldValue(fields, name)
	if !ok {
		var names []string
		for _, f := range fields {
			names = append(names, f.Name)
		}
		t.Fatalf("field %q missing; got %v", name, names)
	}
	return v
}

func mustNotHaveField(t *testing.T, fields []Field, name string) {
	t.Helper()
	if v, ok := fieldValue(fields, name); ok {
		t.Errorf("field %q present with value %q — it must be omitted when unknown", name, v)
	}
}

// TestJobBuildersShareOneOptsShape pins the four rules every job embed obeys,
// for both platforms and all five families at once.
//
// Mutants this kills:
//   - forgetting Platform or JobID on one builder (the footer reads both, and
//     Arc N3 keys edit-in-place on JobID).
//   - building an author line from a blank channel.
//   - putting the platform back in the title (the §0 ruling).
func TestJobBuildersShareOneOptsShape(t *testing.T) {
	for _, f := range []JobFacts{ytFacts(), twFacts()} {
		cases := map[string]func() (string, string, NotificationType, []Field, SendOptions){
			"JobAdded":     func() (string, string, NotificationType, []Field, SendOptions) { return JobAdded(f) },
			"StreamFound":  func() (string, string, NotificationType, []Field, SendOptions) { return StreamFound(f) },
			"JobCancelled": func() (string, string, NotificationType, []Field, SendOptions) { return JobCancelled(f) },
			"DownloadFinished": func() (string, string, NotificationType, []Field, SendOptions) {
				return DownloadFinished(f, []Part{{File: "a.mp4"}})
			},
			"TrimCreated": func() (string, string, NotificationType, []Field, SendOptions) {
				return TrimCreated(f, TrimFacts{TimeRange: "0:00 - 1:00", Duration: time.Minute})
			},
		}
		for name, build := range cases {
			t.Run(f.Platform+"/"+name, func(t *testing.T) {
				title, _, _, _, opts := build()
				if opts.Platform != f.Platform {
					t.Errorf("opts.Platform = %q, want %q", opts.Platform, f.Platform)
				}
				if opts.JobID != f.ID {
					t.Errorf("opts.JobID = %q, want %q", opts.JobID, f.ID)
				}
				if opts.URL != f.URL {
					t.Errorf("opts.URL = %q, want the video's platform page %q", opts.URL, f.URL)
				}
				if opts.Author == nil {
					t.Fatal("opts.Author is nil — every job embed names its channel")
				}
				if opts.Author.Name != f.Channel || opts.Author.IconURL != f.ChannelAvatarURL || opts.Author.URL != f.ChannelURL {
					t.Errorf("opts.Author = %+v, want {%q %q %q}", *opts.Author, f.Channel, f.ChannelAvatarURL, f.ChannelURL)
				}
				lower := strings.ToLower(title)
				if strings.Contains(lower, "twitch") || strings.Contains(lower, "youtube") {
					t.Errorf("title %q names a platform — the platform belongs in the author line", title)
				}
			})
		}
	}
}

// TestBuildersOmitTheAuthorWhenTheChannelIsUnknown covers the CLI add, which
// knows no channel for YouTube.
//
// Mutant: building an Author unconditionally — Discord draws an empty author
// bar with a broken avatar.
func TestBuildersOmitTheAuthorWhenTheChannelIsUnknown(t *testing.T) {
	f := ytFacts()
	f.Channel = ""
	if _, _, _, _, opts := JobAdded(f); opts.Author != nil {
		t.Errorf("opts.Author = %+v with no channel, want nil", *opts.Author)
	}
}

// TestJobAddedPerPlatformAndEntryPoint is the C3 unification: four call sites,
// one shape.
//
// Mutants this kills:
//   - a hardcoded "Video ID" for Twitch (IDLabel exists for exactly this).
//   - dropping the optional format/range fields the web add supplies.
//   - describing a CLI add with an empty title as "Manually added: ".
func TestJobAddedPerPlatformAndEntryPoint(t *testing.T) {
	t.Run("youtube web add with format and range", func(t *testing.T) {
		f := ytFacts()
		f.VideoFormat = "itag 299"
		f.AudioFormat = "itag 251"
		f.TimeRange = "0:30 - 1:45 (1:15)"
		title, desc, ntype, fields, opts := JobAdded(f)
		if title != "Job Added" {
			t.Errorf("title = %q, want %q", title, "Job Added")
		}
		if ntype != TypeInfo {
			t.Errorf("ntype = %v, want TypeInfo", ntype)
		}
		if want := "Manually added: " + EscapeMarkdown(f.Title); desc != want {
			t.Errorf("desc = %q, want %q", desc, want)
		}
		if got := mustField(t, fields, "Video ID"); got != "dQw4w9WgXcQ" {
			t.Errorf("Video ID = %q", got)
		}
		if got := mustField(t, fields, "Video Format"); got != "itag 299" {
			t.Errorf("Video Format = %q", got)
		}
		mustField(t, fields, "Audio Format")
		mustField(t, fields, "Time Range")
		if opts.Event != "added" {
			t.Errorf("opts.Event = %q, want %q", opts.Event, "added")
		}
		if opts.Thumbnail != f.ThumbnailURL {
			t.Errorf("opts.Thumbnail = %q, want the job thumbnail", opts.Thumbnail)
		}
	})

	t.Run("twitch add uses the Stream ID label", func(t *testing.T) {
		_, _, _, fields, _ := JobAdded(twFacts())
		mustField(t, fields, "Stream ID")
		mustNotHaveField(t, fields, "Video ID")
	})

	t.Run("cli add with no title falls back to the id", func(t *testing.T) {
		f := JobFacts{ID: "abc123", VideoID: "abc123", Platform: "youtube", URL: "https://www.youtube.com/watch?v=abc123"}
		_, desc, _, fields, _ := JobAdded(f)
		if desc != "Manually added: abc123" {
			t.Errorf("desc = %q, want %q", desc, "Manually added: abc123")
		}
		mustNotHaveField(t, fields, "Channel")
		mustNotHaveField(t, fields, "Video Format")
	})
}

// TestStreamFoundDescriptionIsPerPlatform keeps the two discovery shapes the
// audit recorded (rows #26/#27) while collapsing the two titles into one.
//
// Mutants this kills:
//   - one description for both platforms (a Twitch find would read "Found
//     matching stream" for a stream that is live right now).
//   - emitting Category for YouTube, where it is always empty.
func TestStreamFoundDescriptionIsPerPlatform(t *testing.T) {
	t.Run("youtube", func(t *testing.T) {
		f := ytFacts()
		title, desc, ntype, fields, opts := StreamFound(f)
		if title != "Stream Found" {
			t.Errorf("title = %q", title)
		}
		if ntype != TypeInfo {
			t.Errorf("ntype = %v, want TypeInfo", ntype)
		}
		if want := "Found matching stream: " + EscapeMarkdown(f.Title); desc != want {
			t.Errorf("desc = %q, want %q", desc, want)
		}
		mustNotHaveField(t, fields, "Category")
		if opts.Event != "found" {
			t.Errorf("opts.Event = %q", opts.Event)
		}
	})

	t.Run("twitch", func(t *testing.T) {
		f := twFacts()
		_, desc, _, fields, _ := StreamFound(f)
		if want := "Live: " + EscapeMarkdown(f.Title); desc != want {
			t.Errorf("desc = %q, want %q", desc, want)
		}
		if got := mustField(t, fields, "Category"); got != "Just Chatting" {
			t.Errorf("Category = %q", got)
		}
		mustField(t, fields, "Stream ID")
	})
}

// TestJobCancelledIsOneTitle collapses row #18 "Download Cancelled" and row
// #25 "Job Cancelled" (audit C5): which code path noticed the cancel is not
// something an operator can act on.
//
// Mutant: keeping TypeInfo — the embed loses the orange sidebar and reads like
// an ordinary lifecycle note.
func TestJobCancelledIsOneTitle(t *testing.T) {
	f := twFacts()
	title, desc, ntype, fields, opts := JobCancelled(f)
	if title != "Job Cancelled" {
		t.Errorf("title = %q, want %q", title, "Job Cancelled")
	}
	if ntype != TypeCancelled {
		t.Errorf("ntype = %v, want TypeCancelled", ntype)
	}
	if want := "Cancelled: " + EscapeMarkdown(f.Title); desc != want {
		t.Errorf("desc = %q, want %q", desc, want)
	}
	mustField(t, fields, "Channel")
	mustField(t, fields, "Stream ID")
	if opts.Event != "cancelled" {
		t.Errorf("opts.Event = %q, want %q", opts.Event, "cancelled")
	}
}

// TestEscapeReachesEveryJobSuppliedString is the rule a reader notices when it
// is missing. Two values are deliberately raw and documented where they are
// built: the author NAME (Discord renders no markdown in the author bar, so
// escaping would show the backslashes) and the platform ID (addIDField — it is
// the field a reader copies out, and a YouTube id is full of "_" and "-").
//
// Mutant: escaping the whole embed — the generated "Time Range" separator
// would pick up backslashes.
func TestEscapeReachesEveryJobSuppliedString(t *testing.T) {
	f := ytFacts()
	f.Channel = "a_b*c"
	_, _, _, fields, opts := StreamFound(f)
	if got := mustField(t, fields, "Channel"); got != EscapeMarkdown("a_b*c") {
		t.Errorf("Channel = %q, want the escaped form %q", got, EscapeMarkdown("a_b*c"))
	}
	if opts.Author == nil || opts.Author.Name != "a_b*c" {
		t.Error("Author.Name must carry the RAW channel — Discord does not render markdown there")
	}

	// The " -> " between qualities is the BUILDER's own text, not the job's.
	// EscapeMarkdown escapes ">", so escaping the JOINED string puts a
	// backslash in front of every separator.
	_, _, _, ff, _ := DownloadFinished(ytFacts(), []Part{{Quality: "1080p60"}, {Quality: "720p60"}})
	if got := mustField(t, ff, "Qualities"); got != "1080p60 -> 720p60" {
		t.Errorf("Qualities = %q — the generated separator was escaped", got)
	}
}

// TestDescriptionExcerptIsRuneSafeAndBounded pins the COMPOSITION the builder
// uses, at the product budget. ClampRunes' own boundary behaviour is pinned
// exhaustively by N1 in limits_test.go; what this adds is that
// DownloadFinished spends the 300 on the DESCRIPTION and escapes afterwards.
//
// Mutants this kill:
//   - a byte-based cut in place of ClampRunes: 300 bytes of Japanese is 100
//     characters, and the rune count below catches it.
//   - escaping before clamping: the clamp would then spend budget on
//     backslashes and could cut one away from the character it protects.
func TestDescriptionExcerptIsRuneSafeAndBounded(t *testing.T) {
	f := ytFacts()
	f.Description = strings.Repeat("あ", 500) // 3 bytes each
	_, _, _, fields, _ := DownloadFinished(f, []Part{{File: "a.mp4"}})
	got := mustField(t, fields, "Description")
	if !utf8.ValidString(got) {
		t.Error("the excerpt is not valid UTF-8 — the cut landed mid-rune")
	}
	if n := utf8.RuneCountInString(got); n != descriptionExcerptLen {
		t.Errorf("excerpt = %d runes, want %d", n, descriptionExcerptLen)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("excerpt does not carry the clamp marker: %q", got)
	}
	short := "already short"
	f.Description = short
	_, _, _, fields, _ = DownloadFinished(f, []Part{{File: "a.mp4"}})
	if got := mustField(t, fields, "Description"); got != short {
		t.Errorf("Description = %q — a string already under the budget was altered", got)
	}
}
```

Add `"unicode/utf8"` to the test's import block.

- [ ] **Step 2: Run the test to verify it fails**

```bash
cd /d/Git/Moombox/.worktrees/webhooks-n2a-producers
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -timeout 300s -run 'TestJobBuilders|TestBuildersOmit|TestJobAdded|TestStreamFound|TestJobCancelled|TestEscapeReaches|TestDescriptionExcerpt' ./internal/notifications/
```

Expected: FAIL to build — `undefined: JobFacts`, `undefined: Part`, `undefined: TrimFacts`, `undefined: JobAdded`, `undefined: StreamFound`, `undefined: JobCancelled`, `undefined: DownloadFinished`, `undefined: TrimCreated`, `undefined: descriptionExcerptLen`. (`ClampRunes`, `EscapeMarkdown`, `Field`, `FieldBuilder`, `SendOptions` and `Author` all exist — N1 shipped them.)

- [ ] **Step 3: Create `internal/notifications/builders.go`**

```go
package notifications

import (
	"fmt"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// JobFacts is everything a job-shaped embed can say, in a form this package
// can read without knowing what a database.Job is.
//
// It is a flat carrier on purpose. The alternative — an interface, or an
// import of internal/database — would put the row type inside the package
// internal/tui imports, which is the back door the TUI import fence exists to
// keep shut. Producers fill what they know and leave the rest zero; every
// builder treats a zero field as "unknown" and OMITS its field rather than
// rendering an empty one.
//
// worker.NotifyFacts is the ONE mapper from a database row into this struct. A
// producer that knows more than the mapper assigns to the result; it does not
// write a second mapper.
type JobFacts struct {
	// --- Identity. Every builder reads these. ---

	// ID is the job row's id: the footer's job id and, from Arc N3, the
	// edit-in-place key. VideoID is what the operator sees in the embed — the
	// two differ for Twitch, whose job id carries a prefix.
	ID      string
	VideoID string
	// Platform is "youtube" or "twitch". It drives IDLabel and the author
	// line, never the title (§0 ruling: neutral titles).
	Platform string
	// Title is the stream/video title as the platform supplied it, RAW. Every
	// builder escapes it before it reaches a description or a field.
	Title string
	// Channel, ChannelURL and ChannelAvatarURL become the embed's author line.
	// An empty Channel means no author line at all.
	Channel          string
	ChannelURL       string
	ChannelAvatarURL string
	// URL is the video's page on its platform; ThumbnailURL its preview image.
	URL          string
	ThumbnailURL string

	// --- Discovery and manual add ---

	// Category is the Twitch game category; always empty for YouTube.
	Category string
	// VideoFormat, AudioFormat and TimeRange are the advanced options a web
	// add can carry, pre-rendered by the route that owns their wording.
	VideoFormat string
	AudioFormat string
	TimeRange   string

	// --- Finalize (DownloadFinished only) ---

	// TotalTime is wall clock from download start to finalize.
	TotalTime time.Duration
	// SegmentCounter is the job-level sequence pair, e.g. "V: 1234 A: 1230".
	SegmentCounter string
	// ChatMessages is nil when the job captured no chat at all.
	ChatMessages *int
	// FormatSelection and TrimmedRange are pre-rendered by the orchestrator,
	// which owns the itag and timestamp vocabularies.
	FormatSelection string
	TrimmedRange    string
	// Description is the video description, RAW and unbounded; the builder
	// excerpts it.
	Description string

	// --- Outcome truth (DownloadFinished only; the error send builds its own
	// fields at its site, where the staging question is answerable) ---

	// IncompleteTail turns the embed Warning-coloured and adds the Tail field:
	// the archive is knowingly short and Resume appends the rest.
	IncompleteTail bool
	// ChatIncomplete reports chat_status == "incomplete".
	ChatIncomplete bool
	// AsideCount is how many set-aside recordings the job's staging still held
	// at finalize.
	AsideCount int
}

// Part is one muxed output of a job: the whole recording for a job that never
// split, one part for a quality/gap-split job. DownloadFinished SUMS their
// sizes and durations, so a caller must not also pass a job-level total.
type Part struct {
	// File is the output's base name; "" when the caller has no name to give
	// (the multi-part shape names the count, not the files).
	File string
	// Quality is the part's label ("1080p60"); "" when unknown.
	Quality string
	// Size in bytes; 0 means unknown, not empty.
	Size int64
	// Duration; 0 means unknown.
	Duration time.Duration
	// Width/Height/Fps describe the part's video; 0 means unknown. Only the
	// FIRST part's resolution is rendered — a split job's parts differ by
	// definition, and listing every one would bury the useful fields.
	Width, Height, Fps int
}

// TrimFacts is the trim-specific half of a "Trim Created" embed.
type TrimFacts struct {
	// TimeRange is pre-rendered ("1:02:03 - 1:05:00"), so this package needs
	// no timestamp formatter of its own.
	TimeRange string
	Duration  time.Duration
	// Size is nil when the trim file could not be stat'd.
	Size *int64
	// Parts is how many source parts the trim spans; the field is rendered
	// only when it is more than one.
	Parts int
}

// descriptionExcerptLen bounds the Description field at the PRODUCT level: a
// finished embed is a notification, not a copy of the video page.
//
// The cut itself is ClampRunes (limits.go) — the same rune-safe clamp the
// payload safety net uses, which Arc N1 exported for precisely this producer
// excerpt, so the two can never disagree about where a multi-byte character
// ends. Nothing in this file truncates on its own.
const descriptionExcerptLen = 300

// displayName is what an embed calls the job: its title, or its id when no
// title was ever fetched. `moombox add` runs no metadata fetch and writes the
// placeholder "Manual Add" to the row, so the CLI sites pass an EMPTY Title
// and this falls back — "Manually added: Manual Add" is not a sentence.
func displayName(f JobFacts) string {
	if f.Title != "" {
		return f.Title
	}
	if f.VideoID != "" {
		return f.VideoID
	}
	return f.ID
}

// authorFor builds the embed's author line, or nil when the channel is
// unknown. The name is RAW: Discord renders no markdown in the author bar, so
// escaping it would show the backslashes.
func authorFor(f JobFacts) *Author {
	if f.Channel == "" {
		return nil
	}
	return &Author{Name: f.Channel, IconURL: f.ChannelAvatarURL, URL: f.ChannelURL}
}

// jobOpts is the SendOptions every job embed shares. Image (the full-width
// picture) is set only by DownloadFinished, and only for a platform whose
// preview survives the stream.
func jobOpts(f JobFacts, event string) SendOptions {
	return SendOptions{
		URL:       f.URL,
		Event:     event,
		Thumbnail: f.ThumbnailURL,
		Author:    authorFor(f),
		Platform:  f.Platform,
		JobID:     f.ID,
	}
}

// addIDField appends the platform-correct id field. Twitch broadcasts carry
// stream ids and everything else video ids; IDLabel is the one place that
// decision lives, and three of the five builders need it.
//
// The id is the one job-supplied string this package does NOT escape, and
// that is deliberate: a YouTube id routinely contains "_" and "-", so
// escaping turns "a_b-c" into "a\_b\-c" in the one field a reader copies out
// of the embed to paste somewhere else. Discord's italic needs a MATCHED pair
// of underscores, so a single id renders verbatim; the cosmetic risk is an
// id that happens to hold two, against a real cost to every id.
func addIDField(b *FieldBuilder, f JobFacts) *FieldBuilder {
	return b.AddInline(IDLabel(f.Platform), f.VideoID)
}

// resolutionLabel renders "1920x1080 @60fps", or "" when nothing was probed.
func resolutionLabel(p Part) string {
	if p.Width <= 0 || p.Height <= 0 {
		return ""
	}
	res := fmt.Sprintf("%dx%d", p.Width, p.Height)
	if p.Fps > 0 {
		res += fmt.Sprintf(" @%dfps", p.Fps)
	}
	return res
}

// JobAdded is the embed for a job created by hand — both web add routes and
// both `moombox add` paths, which previously sent three different titles and
// two different field sets (audit C3).
func JobAdded(f JobFacts) (string, string, NotificationType, []Field, SendOptions) {
	fb := NewFieldBuilder().
		AddInlineIf(f.Channel != "", "Channel", EscapeMarkdown(f.Channel))
	addIDField(fb, f).
		AddInlineIf(f.VideoFormat != "", "Video Format", f.VideoFormat).
		AddInlineIf(f.AudioFormat != "", "Audio Format", f.AudioFormat).
		AddIf(f.TimeRange != "", "Time Range", f.TimeRange)

	return "Job Added",
		"Manually added: " + EscapeMarkdown(displayName(f)),
		TypeInfo,
		fb.Build(),
		jobOpts(f, "added")
}

// StreamFound is the discovery embed for both monitors (audit C4). The TITLE
// is shared; the DESCRIPTION stays per platform because the two moments are
// genuinely different — a YouTube find is usually an upcoming stream, a Twitch
// find is live right now.
func StreamFound(f JobFacts) (string, string, NotificationType, []Field, SendOptions) {
	desc := "Found matching stream: " + EscapeMarkdown(displayName(f))
	if f.Platform == "twitch" {
		desc = "Live: " + EscapeMarkdown(displayName(f))
	}
	fb := NewFieldBuilder().
		AddInlineIf(f.Channel != "", "Channel", EscapeMarkdown(f.Channel))
	addIDField(fb, f).
		AddInlineIf(f.Category != "", "Category", EscapeMarkdown(f.Category))

	return "Stream Found", desc, TypeInfo, fb.Build(), jobOpts(f, "found")
}

// JobCancelled is the one cancel embed. The worker's mid-job cancel and the
// route's never-started cancel described the same event under two titles
// (audit C5); which code path noticed is not something an operator can act on.
func JobCancelled(f JobFacts) (string, string, NotificationType, []Field, SendOptions) {
	fb := NewFieldBuilder().
		AddInlineIf(f.Channel != "", "Channel", EscapeMarkdown(f.Channel))
	addIDField(fb, f)

	return "Job Cancelled",
		"Cancelled: " + EscapeMarkdown(displayName(f)),
		TypeCancelled,
		fb.Build(),
		jobOpts(f, "cancelled")
}

// DownloadFinished is the one finished embed. len(parts) == 1 is the shape a
// job that never split produces (File / File Size / Resolution); more than one
// is the split shape (Parts / Qualities / Total Size). Everything job-level —
// the format selection, the trimmed range, the description excerpt, the chat
// count — is now carried by BOTH: the multi-part builder dropped them for no
// reason anyone recorded (audit C1).
//
// The colour is the OUTCOME, not the code path. A job whose recording is
// knowingly short is Warning, because a green "Successfully archived" over a
// truncated archive says the opposite of the truth (audit A5).
func DownloadFinished(f JobFacts, parts []Part) (string, string, NotificationType, []Field, SendOptions) {
	var totalSize int64
	var totalDuration time.Duration
	var qualities []string
	for _, p := range parts {
		totalSize += p.Size
		totalDuration += p.Duration
		if p.Quality != "" {
			qualities = append(qualities, p.Quality)
		}
	}

	fb := NewFieldBuilder()
	switch {
	case len(parts) == 1:
		fb.AddIf(parts[0].File != "", "File", EscapeMarkdown(parts[0].File))
	case len(parts) > 1:
		// "Parts", not "Segments": Segments is the job-level video/audio
		// sequence counter below, and the two meant different things under one
		// name before this builder existed.
		fb.AddInline("Parts", fmt.Sprintf("%d", len(parts)))
		// Escape each LABEL and join with the RAW separator. Escaping the
		// joined string escapes the " -> " this builder generated — the
		// EscapeMarkdown set includes ">" — and Discord then renders
		// "1080p60 -\> 720p60".
		escaped := make([]string, 0, len(qualities))
		for _, q := range qualities {
			escaped = append(escaped, EscapeMarkdown(q))
		}
		fb.AddInlineIf(len(escaped) > 0, "Qualities", strings.Join(escaped, " -> "))
	}
	if len(parts) > 0 {
		fb.AddInlineIf(resolutionLabel(parts[0]) != "", "Resolution", resolutionLabel(parts[0]))
	}
	if totalSize > 0 {
		name := "File Size"
		if len(parts) > 1 {
			name = "Total Size"
		}
		fb.AddInline(name, utils.FormatFileSize(totalSize))
	}
	fb.AddInlineIf(totalDuration > 0, "Duration", utils.FormatDurationHuman(totalDuration)).
		AddInlineIf(f.TotalTime > 0, "Total Time", utils.FormatDurationHuman(f.TotalTime)).
		AddInlineIf(f.SegmentCounter != "", "Segments", f.SegmentCounter)
	if f.ChatMessages != nil && *f.ChatMessages > 0 {
		fb.AddInline("Chat Messages", fmt.Sprintf("%d", *f.ChatMessages))
	}
	fb.AddIf(f.FormatSelection != "", "Format Selection", f.FormatSelection).
		AddInlineIf(f.TrimmedRange != "", "Trimmed Range", f.TrimmedRange)
	if f.Description != "" {
		// CLAMP FIRST, escape second — the same order N1's site used: escaping
		// inserts backslashes, and a cut applied afterwards could slice one
		// away from the character it protects.
		fb.Add("Description", EscapeMarkdown(ClampRunes(f.Description, descriptionExcerptLen)))
	}

	// The truth fields last, so they read as the postscript they are.
	ntype := TypeSuccess
	if f.IncompleteTail {
		ntype = TypeWarning
		fb.Add("Tail", "incomplete — Resume appends the rest")
	}
	fb.AddIf(f.ChatIncomplete, "Chat", "incomplete")
	if f.AsideCount > 0 {
		fb.Add("Set-aside recordings", fmt.Sprintf("%d — Recover to mux them", f.AsideCount))
	}

	opts := jobOpts(f, "finished")
	// The finished embed has always used the FULL-WIDTH image rather than the
	// corner thumbnail, so the shared Thumbnail is cleared here rather than
	// shown twice. For Twitch it is cleared and nothing replaces it: a Twitch
	// preview URL 404s the moment the stream ends, so both pictures are broken
	// by the time anybody reads the embed (§0 ruling — the dead Twitch
	// finished image is dropped; multipart upload of the saved thumbnail file
	// is a later option).
	opts.Thumbnail = ""
	if f.Platform != "twitch" {
		opts.Image = f.ThumbnailURL
	}

	return "Download Finished",
		"Successfully archived: " + EscapeMarkdown(displayName(f)),
		ntype,
		fb.Build(),
		opts
}

// TrimCreated is the one trim embed; the single-file and multi-segment paths
// differed only by a Segments field (audit C2).
func TrimCreated(f JobFacts, t TrimFacts) (string, string, NotificationType, []Field, SendOptions) {
	dur := utils.FormatDurationHuman(t.Duration)
	name := EscapeMarkdown(displayName(f))
	fb := NewFieldBuilder().
		Add("Source Video", name).
		AddInlineIf(t.TimeRange != "", "Time Range", t.TimeRange).
		AddInline("Duration", dur).
		AddInlineIf(t.Parts > 1, "Segments", fmt.Sprintf("%d segments", t.Parts))
	if t.Size != nil {
		fb.AddInline("File Size", utils.FormatFileSize(*t.Size))
	}

	return "Trim Created",
		fmt.Sprintf("Created %s trim from \"%s\"", dur, name),
		TypeInfo,
		fb.Build(),
		jobOpts(f, "trim_created")
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
cd /d/Git/Moombox/.worktrees/webhooks-n2a-producers
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go test -count=1 -timeout 300s -run 'TestJobBuilders|TestBuildersOmit|TestJobAdded|TestStreamFound|TestJobCancelled|TestEscapeReaches|TestDescriptionExcerpt' -v ./internal/notifications/
go test -count=1 -timeout 300s ./internal/notifications/
go vet ./internal/notifications/
gofmt -l ./internal/notifications
```

Expected: the seven new tests PASS, the package is `ok`, vet silent, `gofmt -l` prints nothing.

- [ ] **Step 5: Confirm the package's dependency set did not widen**

```bash
cd /d/Git/Moombox/.worktrees/webhooks-n2a-producers
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go list -deps ./internal/notifications | grep 'Moombox/internal' | sort
go list -deps ./internal/tui | grep -E 'Moombox/internal/(worker|monitor|web|web/routes|bgutils)$'
```

Expected: the first prints exactly `config`, `connectivity`, `constants`, `httpx`, `notifications`, `utils` — **no `database`, no `worker`**. The second prints nothing (grep exits 1, which is the pass).

- [ ] **Step 6: Commit**

```bash
cd /d/Git/Moombox/.worktrees/webhooks-n2a-producers
git add internal/notifications/builders.go internal/notifications/builders_test.go
git commit -m "feat(notifications): one builder per job embed family

Ten hand-rolled Send calls produced four families of embed whose title and
field set depended on which entry point fired: one route said Twitch Video
Added and the other said Video Added, a CLI add carried no channel and no
thumbnail, a cancel was Download Cancelled or Job Cancelled depending on
whether the worker happened to be mid-job, and two finished builders disagreed
about what a finished job is worth saying.

JobAdded, StreamFound, JobCancelled, DownloadFinished and TrimCreated are pure
functions over JobFacts, a flat database-free description a producer fills from
whatever it has. Each returns exactly Sender.Send's parameter list in order, so
a site reads n.Send(notifications.StreamFound(f)) and a signature drift breaks
every caller at compile time. JobFacts carries no database or worker type on
purpose: internal/tui imports this package, and a row type here would be the
back door the TUI import fence exists to keep shut.

Titles are neutral and the platform moves to the author line (channel name,
avatar, channel page); every job embed carries Platform, JobID and the video's
platform URL. Job-supplied strings are markdown-escaped and the author name is
not, because Discord renders no markdown there. A finished job with an
incomplete tail is Warning-coloured and says so, and the description excerpt
spends Arc N1's ClampRunes rather than a second truncator — N1 exported it for
this producer excerpt, and 300 bytes of a Japanese description is 100
characters.

No producer is adopted yet; the sites move in the tasks that follow.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/notifications/builders.go internal/notifications/builders_test.go
```

---

### Task 2: Adopt `JobAdded` at all four manual-add sites

Four sites create a job by hand and four embeds describe it differently (audit rows #21–#24): the Twitch route says "Twitch Video Added", the YouTube route says "Video Added" with three extra optional fields, and both CLI paths say "Video Added" with a bare id, no channel, no thumbnail and no author. This task introduces the one job→facts mapper and turns all four into `JobAdded` calls.

**Files:**
- Create: `internal/worker/notify_facts.go`
- Create: `internal/worker/notify_facts_test.go`
- Create: `cmd/moombox/job_notifications.go`
- Create: `cmd/moombox/job_notifications_test.go`
- Create: `internal/web/routes/job_notifications_test.go`
- Modify: `internal/web/routes/jobs.go` (the Twitch add send at `:771-786`; the YouTube add field block + send at `:859-911`)
- Modify: `internal/web/routes/jobs_test.go` (`jobsFixture` at `:51-59` and `newJobsFixture` at `:65-106` gain a recorder; a `post` helper is added) — N1 does not touch this file
- Modify: `cmd/moombox/addvideo.go` (`:105-109`, `:141-145`)

**Interfaces:**
- Consumes: `notifications.JobFacts`, `notifications.JobAdded` (Task 1); `notifications.Sender`, `notificationtest.Recorder` (N1); `database.Job` fields `ID`, `VideoID`, `Platform`, `Title`, `ChannelName`, `ChannelAvatarURL`, `ChannelID`, `URL`, `ThumbnailURL` (`internal/database/types.go:85-160`); `youtubeThumbnailURL(videoID string) string` (`cmd/moombox/helpers.go:71`); `utils.TwitchVOD` (`internal/utils/twitch.go:14`).
- Produces:
  - `func NotifyFacts(j *database.Job) notifications.JobFacts` in package `worker` (`internal/worker/notify_facts.go`)
  - `func cliAddedFacts(platform, jobID, jobURL, channelLogin string) notifications.JobFacts` in package `main` (`cmd/moombox/job_notifications.go`)
  - `jobsFixture.notify *notificationtest.Recorder` in `internal/web/routes/jobs_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/worker/notify_facts_test.go`:

```go
package worker

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestNotifyFactsCarriesTheRowStraightThrough is the base case: the mapper is
// the ONE place a database row becomes a JobFacts, so every field a builder
// reads must arrive.
//
// Mutant: dropping ChannelAvatarURL — every job embed loses its author icon
// and nothing else fails.
func TestNotifyFactsCarriesTheRowStraightThrough(t *testing.T) {
	chID := "UC_abc"
	j := &database.Job{
		ID:               "vid1",
		VideoID:          "vid1",
		Platform:         "youtube",
		Title:            "A Title",
		ChannelName:      "A Channel",
		ChannelAvatarURL: "https://yt3.example/a.jpg",
		ChannelID:        &chID,
		URL:              "https://www.youtube.com/watch?v=vid1",
		ThumbnailURL:     "https://i.ytimg.com/vi/vid1/maxresdefault.jpg",
	}
	f := NotifyFacts(j)
	if f.ID != "vid1" || f.VideoID != "vid1" || f.Platform != "youtube" {
		t.Errorf("identity fields = %q/%q/%q", f.ID, f.VideoID, f.Platform)
	}
	if f.Title != "A Title" || f.Channel != "A Channel" {
		t.Errorf("Title/Channel = %q/%q", f.Title, f.Channel)
	}
	if f.ChannelAvatarURL != j.ChannelAvatarURL {
		t.Errorf("ChannelAvatarURL = %q, want %q", f.ChannelAvatarURL, j.ChannelAvatarURL)
	}
	if f.ThumbnailURL != j.ThumbnailURL {
		t.Errorf("ThumbnailURL = %q", f.ThumbnailURL)
	}
	if f.ChannelURL != "https://www.youtube.com/channel/UC_abc" {
		t.Errorf("ChannelURL = %q, want the channel page derived from channel_id", f.ChannelURL)
	}
}

// TestNotifyFactsFallsBackToTheWatchURL folds the identical fallback that the
// cancel route and setJobError each carried separately.
//
// Mutants this kill:
//   - dropping the fallback: a row with no url produces an embed whose title
//     links nowhere.
//   - applying it to Twitch: twitch.tv rows would get a youtube.com link.
func TestNotifyFactsFallsBackToTheWatchURL(t *testing.T) {
	yt := NotifyFacts(&database.Job{ID: "v", VideoID: "v", Platform: "youtube"})
	if yt.URL != "https://www.youtube.com/watch?v=v" {
		t.Errorf("URL = %q, want the watch-URL fallback", yt.URL)
	}
	tw := NotifyFacts(&database.Job{ID: "tw_1", VideoID: "1", Platform: "twitch"})
	if tw.URL != "" {
		t.Errorf("URL = %q for a twitch row with no url, want empty — there is no watch-URL to guess", tw.URL)
	}
}

// TestNotifyFactsDerivesNoChannelURLWithoutAChannelID covers manual and Twitch
// rows, whose channel_id is NULL.
//
// Mutant: dereferencing ChannelID unconditionally — a nil pointer panic on
// every manually added job.
func TestNotifyFactsDerivesNoChannelURLWithoutAChannelID(t *testing.T) {
	empty := ""
	for name, j := range map[string]*database.Job{
		"nil channel id":   {ID: "v", VideoID: "v", Platform: "youtube"},
		"empty channel id": {ID: "v", VideoID: "v", Platform: "youtube", ChannelID: &empty},
		"twitch":           {ID: "tw_1", VideoID: "1", Platform: "twitch"},
	} {
		if got := NotifyFacts(j).ChannelURL; got != "" {
			t.Errorf("%s: ChannelURL = %q, want empty", name, got)
		}
	}
}

// TestNotifyFactsIsNilSafe: the mapper is called from error paths.
func TestNotifyFactsIsNilSafe(t *testing.T) {
	if got := NotifyFacts(nil); got.ID != "" {
		t.Errorf("NotifyFacts(nil) = %+v, want the zero JobFacts", got)
	}
}
```

Create `cmd/moombox/job_notifications_test.go`:

```go
package main

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
)

// TestCLIAddedFactsGainsAThumbnailAndChannel is audit row #23/#24: the CLI add
// sent a bare id with no channel, no thumbnail and no author, while the web
// add sent all three for the same action.
//
// Mutants this kill:
//   - leaving the YouTube thumbnail empty: the embed loses the image the web
//     add has always had, and nothing else changes.
//   - setting Title from the row's "Manual Add" placeholder: the description
//     reads "Manually added: Manual Add".
//   - naming a Twitch VOD's channel: a VOD add knows a video id, not a login.
func TestCLIAddedFactsGainsAThumbnailAndChannel(t *testing.T) {
	t.Run("youtube", func(t *testing.T) {
		f := cliAddedFacts("youtube", "abc123", "https://www.youtube.com/watch?v=abc123", "")
		if f.ThumbnailURL != youtubeThumbnailURL("abc123") {
			t.Errorf("ThumbnailURL = %q, want the maxres thumbnail", f.ThumbnailURL)
		}
		if f.Title != "" {
			t.Errorf("Title = %q, want empty so the builder falls back to the id", f.Title)
		}
		if f.Channel != "" || f.ChannelURL != "" {
			t.Errorf("channel = %q/%q — a CLI YouTube add knows no channel", f.Channel, f.ChannelURL)
		}
	})

	t.Run("twitch live add names the channel", func(t *testing.T) {
		f := cliAddedFacts("twitch", "streamer", "https://www.twitch.tv/streamer", "streamer")
		if f.Channel != "streamer" {
			t.Errorf("Channel = %q, want the login", f.Channel)
		}
		if f.ChannelURL != "https://www.twitch.tv/streamer" {
			t.Errorf("ChannelURL = %q", f.ChannelURL)
		}
	})

	t.Run("twitch vod add names no channel", func(t *testing.T) {
		f := cliAddedFacts("twitch", "tw_v123", "https://www.twitch.tv/videos/123", "")
		if f.Channel != "" || f.ChannelURL != "" {
			t.Errorf("channel = %q/%q — a VOD add knows a video id, not a login", f.Channel, f.ChannelURL)
		}
	})
}

// TestCLIAddedFactsProduceAJobAddedEmbed runs the composed path a recorder can
// observe, which the addVideo function itself cannot offer (it loads config,
// opens the database and calls os.Exit).
func TestCLIAddedFactsProduceAJobAddedEmbed(t *testing.T) {
	rec := notificationtest.New()
	rec.Send(notifications.JobAdded(cliAddedFacts("youtube", "abc123", "https://www.youtube.com/watch?v=abc123", "")))

	calls := rec.ByEvent("added")
	if len(calls) != 1 {
		t.Fatalf("recorded %d added calls, want 1", len(calls))
	}
	c := calls[0]
	if c.Title != "Job Added" {
		t.Errorf("title = %q, want %q", c.Title, "Job Added")
	}
	if c.Description != "Manually added: abc123" {
		t.Errorf("description = %q", c.Description)
	}
	if c.Opts.Thumbnail == "" {
		t.Error("the CLI add still sends no thumbnail")
	}
	if c.Opts.JobID != "abc123" || c.Opts.Platform != "youtube" {
		t.Errorf("opts = %+v, want the job id and platform", c.Opts)
	}
}
```

Create `internal/web/routes/job_notifications_test.go`:

```go
package routes

import (
	"net/http"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestAddRoutesSendOneJobAddedEmbed is the route half of audit C3: the two add
// routes sent two different titles for the same action.
//
// Mutants this kill:
//   - leaving "Twitch Video Added" in place on the Twitch route.
//   - dropping the advanced-option fields from the YouTube route.
func TestAddRoutesSendOneJobAddedEmbed(t *testing.T) {
	t.Run("youtube", func(t *testing.T) {
		f := newJobsFixture(t)
		f.yt.meta = &YouTubeJobMetadata{Title: "A Title", ChannelName: "A Channel", ThumbnailURL: "https://i.ytimg.example/t.jpg"}

		rec := f.post(t, "/api/jobs", `{"videoId":"dQw4w9WgXcQ","selectedVideoItag":299}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("POST /api/jobs = %d, want 201", rec.Code)
		}
		calls := f.notify.ByEvent("added")
		if len(calls) != 1 {
			t.Fatalf("recorded %d added calls, want 1", len(calls))
		}
		if calls[0].Title != "Job Added" {
			t.Errorf("title = %q, want %q", calls[0].Title, "Job Added")
		}
		if calls[0].Opts.Author == nil || calls[0].Opts.Author.Name != "A Channel" {
			t.Error("the add embed carries no author line")
		}
		if _, ok := calls[0].Field("Video Format"); !ok {
			t.Error("the selected video itag is missing from the embed")
		}
	})

	t.Run("twitch", func(t *testing.T) {
		f := newJobsFixture(t)
		// IsLive is load-bearing: internal/web/routes/jobs.go:712 gates the
		// live-add branch on it, and without it the route falls to the
		// tw_manual_<login>_<unixnano> fallback at :728 — every assertion
		// below would still pass while testing a path this subtest does not
		// name.
		f.tw.streamMeta = &TwitchJobMetadata{
			Title: "Live", ChannelName: "Streamer", StreamID: "12345",
			IsLive: true, AvatarURL: "https://static.example/p.png",
		}

		rec := f.post(t, "/api/jobs", `{"url":"https://www.twitch.tv/streamer"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("POST /api/jobs = %d, want 201", rec.Code)
		}
		calls := f.notify.ByEvent("added")
		if len(calls) != 1 {
			t.Fatalf("recorded %d added calls, want 1", len(calls))
		}
		if calls[0].Title != "Job Added" {
			t.Errorf("title = %q — the Twitch route still names the platform in its title", calls[0].Title)
		}
		if v, ok := calls[0].Field("Stream ID"); !ok || v != "tw_12345" {
			t.Errorf("Stream ID = %q (present=%v), want the job id the old send carried", v, ok)
		}
		if calls[0].Opts.Author == nil || calls[0].Opts.Author.IconURL == "" {
			t.Error("the Twitch add embed lost the avatar the row carries")
		}
	})
}

```

**No field-lookup helper is written in this package.** N1's `Call.Field(name) (string, bool)` (`internal/notifications/notificationtest/recorder.go:35`) is exactly that lookup — write `calls[0].Field("Video Format")` and delete the `fieldNamed` shape entirely. (`builders_test.go`'s own `mustField`/`fieldValue` stay: that file is `package notifications` and cannot import `notificationtest` without an import cycle.)

This file imports only `net/http`, `testing` and `internal/database` — the recorded calls are read through `Call.Field` and `Call.Opts`, so no `internal/notifications` import is needed here. `f.post` does not exist yet — Step 2 writes it, because this is the first task that uses it. Read `TwitchJobMetadata`'s and `YouTubeJobMetadata`'s real field names in `internal/web/routes/jobs.go` before running and correct the literals if they differ.

- [ ] **Step 2: Wire the recorder into `jobsFixture`, and give it the `post` helper it lacks**

In `internal/web/routes/jobs_test.go`, add a `notify *notificationtest.Recorder` field to `jobsFixture` (`:51-59`), build one in `newJobsFixture` and pass it as `JobRoutes`' last argument in place of the current `nil` (`:95`). Every existing test keeps working — a recorder with nothing asserted against it is inert.

```go
	notify := notificationtest.New()

	r := chi.NewRouter()
	JobRoutes(r, db, store, nil, apiRL, tw, yt, notify)

	return &jobsFixture{
		router:     r,
		db:         db,
		store:      store,
		outputDir:  outputDir,
		stagingDir: stagingDir,
		tw:         tw,
		yt:         yt,
		notify:     notify,
	}
```

Also add the `post` helper `jobsFixture` does **not** have. The package's only fixture helpers are `addJob` (`internal/web/routes/jobs_test.go:110`) and `getJSON` (`internal/web/routes/asides_test.go:15`); every existing POST test builds its request inline, and three of this arc's tests need one:

```go
// post issues a JSON POST through the fixture's router.
func (f *jobsFixture) post(t *testing.T, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}
```

`net/http/httptest` and `strings` are already in `jobs_test.go`'s import block.

- [ ] **Step 3: Run the tests to verify they fail**

```bash
cd /d/Git/Moombox/.worktrees/webhooks-n2a-producers
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go test -count=1 -timeout 300s -run 'TestNotifyFacts' ./internal/worker/
go test -count=1 -timeout 300s -run 'TestCLIAddedFacts' ./cmd/moombox/
go test -count=1 -timeout 300s -run 'TestAddRoutesSendOneJobAddedEmbed' ./internal/web/routes/
```

Expected: `internal/worker` FAILS to build with `undefined: NotifyFacts`; `cmd/moombox` FAILS to build with `undefined: cliAddedFacts`; `internal/web/routes` compiles (the fixture change landed) but the two subtests FAIL with `recorded 0 added calls, want 1` — the routes still call the recorder through the old inline `Send`, so the event lands but the title assertion fails with `title = "Video Added", want "Job Added"` / `title = "Twitch Video Added"`. Record whichever of the two shapes appears; both are red.

- [ ] **Step 4: Create `internal/worker/notify_facts.go`**

```go
package worker

import (
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications"
)

// NotifyFacts turns a job row into the notifications.JobFacts every job embed
// is built from.
//
// It lives in internal/worker rather than in internal/notifications because
// notifications must not import internal/database — internal/tui imports
// notifications, and a row type there would breach the TUI import fence
// through the back door. internal/worker is the package the other two
// producers (internal/web/routes and cmd/moombox) already depend on, so one
// mapper serves all three; a site that knows more than the row assigns to the
// returned struct instead of writing a second mapper.
//
// Two fallbacks are folded in here because both were duplicated at their call
// sites before:
//
//   - The YouTube watch URL, which the cancel route and setJobError each
//     reconstructed when the row's url was empty. It is YouTube-only on
//     purpose: there is no URL to guess for a Twitch row.
//   - The channel page, derived from channel_id. Only the feed and DECAPI
//     creators set channel_id, so manual and Twitch rows get no author link
//     (the author line still carries the name and the avatar).
func NotifyFacts(j *database.Job) notifications.JobFacts {
	if j == nil {
		return notifications.JobFacts{}
	}
	f := notifications.JobFacts{
		ID:               j.ID,
		VideoID:          j.VideoID,
		Platform:         j.Platform,
		Title:            j.Title,
		Channel:          j.ChannelName,
		ChannelAvatarURL: j.ChannelAvatarURL,
		URL:              j.URL,
		ThumbnailURL:     j.ThumbnailURL,
	}
	if f.URL == "" && j.Platform != "twitch" && j.VideoID != "" {
		f.URL = "https://www.youtube.com/watch?v=" + j.VideoID
	}
	if j.Platform != "twitch" && j.ChannelID != nil && *j.ChannelID != "" {
		f.ChannelURL = "https://www.youtube.com/channel/" + *j.ChannelID
	}
	return f
}
```

- [ ] **Step 5: Create `cmd/moombox/job_notifications.go`**

```go
package main

import (
	"github.com/vampiricwulf/Moombox/internal/notifications"
)

// cliAddedFacts describes a job added from the command line.
//
// `moombox add` runs no metadata fetch, so the row it writes carries the
// placeholders "Manual Add" and "Manual"; passing those through would make the
// embed read "Manually added: Manual Add" with a Channel field that names
// nothing. The facts are therefore built from what the COMMAND knows rather
// than from the row: an empty Title (the builder falls back to the id), the
// derived YouTube thumbnail, and — for a Twitch live add, where the target IS
// a channel login — the channel name and its page.
//
// channelLogin is empty for YouTube and for a Twitch VOD add, whose target is
// a video id.
func cliAddedFacts(platform, jobID, jobURL, channelLogin string) notifications.JobFacts {
	f := notifications.JobFacts{
		ID:       jobID,
		VideoID:  jobID,
		Platform: platform,
		URL:      jobURL,
	}
	switch {
	case platform == "twitch":
		if channelLogin != "" {
			f.Channel = channelLogin
			f.ChannelURL = "https://www.twitch.tv/" + channelLogin
		}
	default:
		f.ThumbnailURL = youtubeThumbnailURL(jobID)
	}
	return f
}
```

- [ ] **Step 6: Adopt the builder at the four sites**

`cmd/moombox/addvideo.go` — replace the Twitch send (`:105-109`):

```go
			login := ""
			if tw.Type != utils.TwitchVOD {
				login = tw.Value
			}
			notifyMgr.Send(notifications.JobAdded(cliAddedFacts("twitch", jobID, jobURL, login)))
```

and the YouTube send (`:141-145`):

```go
			notifyMgr.Send(notifications.JobAdded(cliAddedFacts("youtube", videoID, videoURL, "")))
```

`internal/web/routes/jobs.go` — replace the Twitch add send (`:771-786`):

```go
			if notifier != nil {
				notifier.Send(notifications.JobAdded(worker.NotifyFacts(job)))
			}
```

and the YouTube add block (`:859-911`) — keep every existing label computation, but assign into the facts instead of appending fields:

```go
		if notifier != nil {
			facts := worker.NotifyFacts(job)
			if body.SelectedVideoItag != nil {
				facts.VideoFormat = fmt.Sprintf("itag %d", *body.SelectedVideoItag)
				if *body.SelectedVideoItag == -1 {
					facts.VideoFormat = "None (audio only)"
				}
			}
			if body.SelectedAudioItag != nil {
				facts.AudioFormat = fmt.Sprintf("itag %d", *body.SelectedAudioItag)
				if *body.SelectedAudioItag == -1 {
					facts.AudioFormat = "None (video only)"
				}
			}
			if body.StartTime != nil || body.EndTime != nil {
				// … the existing startStr/endStr/rangeValue computation, unchanged …
				facts.TimeRange = rangeValue
			}
			notifier.Send(notifications.JobAdded(facts))
		}
```

The Twitch route's `Stream ID` field previously carried `job.ID` and the YouTube route's `Video ID` carried `videoID`; `NotifyFacts` sends `job.VideoID`, which equals both (`internal/web/routes/jobs.go:740` sets `VideoID: jobID` for Twitch, `:826` `VideoID: videoID` for YouTube — verify after the merge).

If `internal/web/routes/jobs.go` does not yet import `internal/worker`, it does after this task; it already calls `worker.ScanAsides` at `:49`, so the import is present.

- [ ] **Step 7: Run the tests to verify they pass**

```bash
cd /d/Git/Moombox/.worktrees/webhooks-n2a-producers
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go test -count=1 -timeout 300s -run 'TestNotifyFacts' -v ./internal/worker/
go test -count=1 -timeout 300s -run 'TestCLIAddedFacts' -v ./cmd/moombox/
go test -count=1 -timeout 300s -run 'TestAddRoutesSendOneJobAddedEmbed' -v ./internal/web/routes/
go test -count=1 -timeout 300s ./internal/worker/ ./internal/web/routes/ ./cmd/moombox/ ./internal/notifications/
go vet ./internal/worker/ ./internal/web/routes/ ./cmd/moombox/
gofmt -l ./internal/worker ./internal/web/routes ./cmd/moombox
```

Expected: all new tests PASS; all four packages `ok`; vet silent; `gofmt -l` prints nothing.

- [ ] **Step 8: Commit**

```bash
cd /d/Git/Moombox/.worktrees/webhooks-n2a-producers
git add internal/worker/notify_facts.go internal/worker/notify_facts_test.go \
        cmd/moombox/job_notifications.go cmd/moombox/job_notifications_test.go \
        cmd/moombox/addvideo.go internal/web/routes/jobs.go \
        internal/web/routes/jobs_test.go internal/web/routes/job_notifications_test.go
git commit -m "feat(notifications): one Job Added embed for all four add paths

Four sites create a job by hand and four embeds described it differently: the
Twitch route said Twitch Video Added, the YouTube route said Video Added with
three extra optional fields, and both moombox add paths said Video Added with a
bare id, no channel, no thumbnail and no author line at all.

All four now call notifications.JobAdded. worker.NotifyFacts is the one mapper
from a job row into JobFacts, shared by the three producer packages because
internal/web/routes and cmd/moombox already depend on internal/worker and
internal/notifications must not import internal/database. It folds in the two
fallbacks that were duplicated at their call sites: the YouTube watch URL for a
row with no url, and the channel page derived from channel_id.

The CLI adds gain what the web adds always had. A YouTube add carries the
maxres thumbnail; a Twitch live add names the channel and links its page. Their
facts are built from what the command knows rather than from the row, because
the row holds the placeholders Manual Add and Manual, and an embed reading
Manually added: Manual Add is worse than one that names the id.

The routes test fixture now wires a notificationtest.Recorder, so the add and
cancel paths are asserted through real HTTP requests instead of not at all.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/worker/notify_facts.go internal/worker/notify_facts_test.go cmd/moombox/job_notifications.go cmd/moombox/job_notifications_test.go cmd/moombox/addvideo.go internal/web/routes/jobs.go internal/web/routes/jobs_test.go internal/web/routes/job_notifications_test.go
```

---

### Task 3: Adopt `StreamFound` and `JobCancelled`

Two discovery sites and two cancel sites, four embeds, three titles (audit C4, C5). Both discovery sites live inside closures in `wireMonitorCallbacks` and have never been testable; this task gives them a package-level seam so they are.

**Files:**
- Modify: `cmd/moombox/job_notifications.go` (add `notifyStreamFound`)
- Modify: `cmd/moombox/job_notifications_test.go`
- Modify: `cmd/moombox/monitor_callbacks.go` (the YouTube found send at `:1412-1425`, guard at `:1412`; the Twitch found send at `:1503-1526`, guard at `:1503`) — N1 inserted ~45 lines into this file, so these are the post-merge numbers
- Modify: `internal/worker/worker.go` (`handleCancellation`'s send at `:1075-1087`)
- Create: `internal/worker/job_notifications_test.go`
- Modify: `internal/web/routes/jobs.go` (the cancel send at `:953-973`)
- Modify: `internal/web/routes/job_notifications_test.go`

**Interfaces:**
- Consumes: `notifications.StreamFound`, `notifications.JobCancelled`, `worker.NotifyFacts` (Tasks 1–2); `twitch.TwitchStreamInfo` fields `ChannelLogin`, `GameCategory` (held by the `OnStreamFound` closure in `cmd/moombox/monitor_callbacks.go`, not by `internal/monitor`).
- Produces: `func notifyStreamFound(n notifications.Sender, job *database.Job, channelURL, category string)` in package `main`.

- [ ] **Step 1: Write the failing tests**

Append to `cmd/moombox/job_notifications_test.go`:

```go
// TestNotifyStreamFoundIsOneEmbedForBothMonitors is audit C4. Both discovery
// sites lived inside closures in wireMonitorCallbacks and had no test at all;
// this seam is what makes them assertable.
//
// Mutants this kill:
//   - keeping "Twitch Stream Found" as a second title.
//   - dropping the Twitch channel page, which is the only channel link a
//     Twitch row can produce (channel_id is NULL for every Twitch job).
//   - sending the category for YouTube, where it is always empty.
func TestNotifyStreamFoundIsOneEmbedForBothMonitors(t *testing.T) {
	t.Run("youtube", func(t *testing.T) {
		rec := notificationtest.New()
		chID := "UC_abc"
		notifyStreamFound(rec, &database.Job{
			ID: "vid1", VideoID: "vid1", Platform: "youtube", Title: "A Stream",
			ChannelName: "A Channel", ChannelID: &chID,
			URL: "https://www.youtube.com/watch?v=vid1", ThumbnailURL: "https://i.ytimg.example/t.jpg",
		}, "", "")

		calls := rec.ByEvent("found")
		if len(calls) != 1 {
			t.Fatalf("recorded %d found calls, want 1", len(calls))
		}
		if calls[0].Title != "Stream Found" {
			t.Errorf("title = %q", calls[0].Title)
		}
		if calls[0].Description != "Found matching stream: A Stream" {
			t.Errorf("description = %q", calls[0].Description)
		}
		if calls[0].Opts.Author == nil || calls[0].Opts.Author.URL != "https://www.youtube.com/channel/UC_abc" {
			t.Error("the YouTube find lost its channel-page link")
		}
	})

	t.Run("twitch", func(t *testing.T) {
		rec := notificationtest.New()
		notifyStreamFound(rec, &database.Job{
			ID: "tw_9", VideoID: "9", Platform: "twitch", Title: "Streamer — live",
			ChannelName: "Streamer", ChannelAvatarURL: "https://static.example/p.png",
			URL: "https://twitch.tv/streamer", ThumbnailURL: "https://static.example/prev.jpg",
		}, "https://twitch.tv/streamer", "Just Chatting")

		calls := rec.ByEvent("found")
		if len(calls) != 1 {
			t.Fatalf("recorded %d found calls, want 1", len(calls))
		}
		if calls[0].Title != "Stream Found" {
			t.Errorf("title = %q — the Twitch find still names the platform", calls[0].Title)
		}
		if calls[0].Description != "Live: Streamer — live" {
			t.Errorf("description = %q", calls[0].Description)
		}
		if calls[0].Opts.Author == nil || calls[0].Opts.Author.URL != "https://twitch.tv/streamer" {
			t.Error("the Twitch find has no channel-page link")
		}
	})
}
```

Create `internal/worker/job_notifications_test.go`:

```go
package worker

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
)

// notifyField is the fatal-on-missing wrapper around N1's Call.Field
// (notificationtest/recorder.go:35), which already answers
// `value, ok := call.Field(name)`. This adds only the t.Fatalf, so a missing
// field names the embed it was missing from instead of failing three
// assertions later on an empty string.
func notifyField(t *testing.T, c notificationtest.Call, name string) string {
	t.Helper()
	v, ok := c.Field(name)
	if !ok {
		t.Fatalf("field %q missing from %q; got %+v", name, c.Title, c.Fields)
	}
	return v
}

// TestUserCancelSendsTheOneCancelEmbed is audit C5's worker half: the
// mid-job cancel said "Download Cancelled" while the route's cancel of the
// same job a second earlier would have said "Job Cancelled".
//
// Mutant: leaving the old title in place — a subscriber filtering on the
// title (not the event) sees two different cancels for one action.
func TestUserCancelSendsTheOneCancelEmbed(t *testing.T) {
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)
	rec := notificationtest.New()
	w.notifier = rec

	job := &database.Job{
		ID: "tw_c1", VideoID: "c1", Platform: "twitch", Title: "Cancel Me",
		ChannelName: "Streamer", URL: "https://twitch.tv/streamer",
		Status: database.StatusDownloading,
	}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	// Cancel flags ONLY a job the queue is already PROCESSING
	// (internal/worker/queue.go:387 — "Only flag jobs that are actually
	// processing"), so the row has to be enqueued and dequeued first.
	// Without that, nothing is flagged, handleCancellation takes the SHUTDOWN
	// branch and sends nothing — the test would be red before the change and
	// red after it.
	w.queue.Enqueue(job.ID, database.StatusDownloading)
	if _, _, ok := w.queue.Dequeue(context.Background()); !ok {
		t.Fatal("Dequeue returned no job — the cancel path needs a processing run")
	}
	if !w.queue.Cancel(job.ID) {
		t.Fatal("Cancel did not flag a user cancel")
	}
	w.handleCancellation(job)

	calls := rec.ByEvent("cancelled")
	if len(calls) != 1 {
		t.Fatalf("recorded %d cancelled calls, want 1", len(calls))
	}
	if calls[0].Title != "Job Cancelled" {
		t.Errorf("title = %q, want %q", calls[0].Title, "Job Cancelled")
	}
	if got := notifyField(t, calls[0], "Stream ID"); got != "c1" {
		t.Errorf("Stream ID = %q", got)
	}
}
```

Add `"context"` to this file's import block. `w.queue` and `w.notifier` are package-internal fields; the three queue methods are `Enqueue(jobID string, status database.JobStatus)` (`internal/worker/queue.go:104`), `Dequeue(ctx context.Context) (string, context.Context, bool)` (`:157`) and `Cancel(jobID string) bool` (`:387`).

Append to `internal/web/routes/job_notifications_test.go`:

```go
// TestCancelRouteSendsTheOneCancelEmbed is C5's route half. The route only
// sends when the worker did not (jobs.go dedupes on CancelJob's bool), so this
// fixture's nil worker is exactly the shape that reaches the send.
func TestCancelRouteSendsTheOneCancelEmbed(t *testing.T) {
	f := newJobsFixture(t)
	f.addJob(t, "vid9", func(j *database.Job) {
		j.Status = database.StatusUpcoming
		j.Title = "Cancel Me"
		j.URL = ""
	})

	rec := f.post(t, "/api/jobs/vid9/cancel", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST cancel = %d, want 200", rec.Code)
	}
	calls := f.notify.ByEvent("cancelled")
	if len(calls) != 1 {
		t.Fatalf("recorded %d cancelled calls, want 1", len(calls))
	}
	if calls[0].Title != "Job Cancelled" {
		t.Errorf("title = %q", calls[0].Title)
	}
	if calls[0].Opts.URL != "https://www.youtube.com/watch?v=vid9" {
		t.Errorf("opts.URL = %q, want the watch-URL fallback for a row with no url", calls[0].Opts.URL)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
cd /d/Git/Moombox/.worktrees/webhooks-n2a-producers
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go test -count=1 -timeout 300s -run 'TestNotifyStreamFound' ./cmd/moombox/
go test -count=1 -timeout 300s -run 'TestUserCancelSendsTheOneCancelEmbed' ./internal/worker/
go test -count=1 -timeout 300s -run 'TestCancelRouteSendsTheOneCancelEmbed' ./internal/web/routes/
```

Expected: `cmd/moombox` FAILS to build with `undefined: notifyStreamFound`; the worker test FAILS with `title = "Download Cancelled", want "Job Cancelled"`; the routes test FAILS the same way, `title = "Job Cancelled"` already matching but the fields/URL coming from the old inline block.

- [ ] **Step 3: Add `notifyStreamFound` to `cmd/moombox/job_notifications.go`**

```go
// notifyStreamFound is the one discovery embed, for both monitors.
//
// A package-level function rather than two inline sends inside
// wireMonitorCallbacks' closures: those closures need a runState, three live
// monitors and a database to reach, which is why neither discovery embed had a
// test before (audit §4, coverage root cause). Everything the two sites differ
// by is a parameter.
//
// channelURL is the channel's page when the caller knows one the job row
// cannot derive — a Twitch row's channel_id is always NULL, and its login is
// only on the monitor's stream info. category is the Twitch game, empty for
// YouTube.
func notifyStreamFound(n notifications.Sender, job *database.Job, channelURL, category string) {
	f := worker.NotifyFacts(job)
	if channelURL != "" {
		f.ChannelURL = channelURL
	}
	f.Category = category
	n.Send(notifications.StreamFound(f))
}
```

Add the `internal/database` and `internal/worker` imports.

- [ ] **Step 4: Adopt at the four sites**

`cmd/moombox/monitor_callbacks.go`, the YouTube found send (`:1412-1425`) becomes:

```go
		if s.notifyMgr.HasTargets() {
			notifyStreamFound(s.notifyMgr, job, "", "")
		}
```

and the Twitch found send (`:1503-1526`):

```go
		if s.notifyMgr.HasTargets() {
			notifyStreamFound(s.notifyMgr, job, "https://twitch.tv/"+info.ChannelLogin, info.GameCategory)
		}
```

**Both `HasTargets()` guards stay exactly as they are.** N1 typed `runState.notifyMgr` as `notifications.Notifier` (`cmd/moombox/runstate.go:68`), a superset of `Sender` that carries `HasTargets`/`Reload`/`BeginShutdown`/`Wait`, so they compile unchanged. N2a neither adds nor removes a `HasTargets` guard anywhere — with the one exception of the disk send, Task 8 Step 6, which is called out there and reported.

`internal/worker/worker.go`, `handleCancellation` (`:1074-1088`):

```go
		if w.notifier != nil {
			w.notifier.Send(notifications.JobCancelled(NotifyFacts(job)))
		}
```

`internal/web/routes/jobs.go`, the cancel send (`:953-973`): the `idLabel` local and the `notifyURL` fallback both move into `NotifyFacts`, so the block collapses to

```go
		if notifier != nil && !workerWillNotify {
			notifier.Send(notifications.JobCancelled(worker.NotifyFacts(job)))
		}
```

Delete the now-unused `idLabel` and `notifyURL` locals (staticcheck is a hard gate).

- [ ] **Step 5: Run the tests to verify they pass**

```bash
cd /d/Git/Moombox/.worktrees/webhooks-n2a-producers
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go test -count=1 -timeout 300s -run 'TestNotifyStreamFound|TestCLIAddedFacts' -v ./cmd/moombox/
go test -count=1 -timeout 300s -run 'TestUserCancelSendsTheOneCancelEmbed' -v ./internal/worker/
go test -count=1 -timeout 300s -run 'TestCancelRouteSendsTheOneCancelEmbed|TestAddRoutesSendOneJobAddedEmbed' -v ./internal/web/routes/
go test -count=1 -timeout 300s ./cmd/moombox/ ./internal/worker/ ./internal/web/routes/
go vet ./cmd/moombox/ ./internal/worker/ ./internal/web/routes/
gofmt -l ./cmd/moombox ./internal/worker ./internal/web/routes
```

Expected: every new test PASSES, three packages `ok`, vet silent, `gofmt -l` prints nothing.

- [ ] **Step 6: Commit**

```bash
cd /d/Git/Moombox/.worktrees/webhooks-n2a-producers
git add cmd/moombox/job_notifications.go cmd/moombox/job_notifications_test.go \
        cmd/moombox/monitor_callbacks.go internal/worker/worker.go \
        internal/worker/job_notifications_test.go internal/web/routes/jobs.go \
        internal/web/routes/job_notifications_test.go
git commit -m "feat(notifications): one Stream Found and one Job Cancelled embed

Two discovery sites sent two titles for the same event, and two cancel sites
sent two more depending on whether the worker happened to be mid-job when the
operator pressed the button. Neither difference is something an operator can
act on, and neither discovery embed had a test — both live inside closures in
wireMonitorCallbacks that need a runState, three monitors and a database to
reach.

notifyStreamFound is the package-level seam both monitors now call, so the
discovery embed is asserted for the first time. The per-platform DESCRIPTION
stays (a YouTube find is usually an upcoming stream; a Twitch find is live
right now) and the title no longer names the platform, which the author line
does. Twitch finds pass their channel page explicitly because a Twitch row's
channel_id is always NULL and only the monitor holds the login.

The cancel route's IDLabel local and its watch-URL fallback are gone: both now
come from worker.NotifyFacts, which is where the same fallback already lives
for setJobError.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- cmd/moombox/job_notifications.go cmd/moombox/job_notifications_test.go cmd/moombox/monitor_callbacks.go internal/worker/worker.go internal/worker/job_notifications_test.go internal/web/routes/jobs.go internal/web/routes/job_notifications_test.go
```

---

### Task 4: One `DownloadFinished` send, and the finished embed's outcome truth

Two builders describe the same moment with different field sets (audit C1): the multi-part one at `orchestrator_mux.go:1028-1077` reports Segments / Qualities / Total Size and drops the format selection, the trimmed range and the description; the single-part one at `:1242-1337` reports File / File Size / Format Selection / Trimmed Range / Description and knows nothing about parts. Both send a green "Successfully archived" for a job whose tail is incomplete, whose chat capture stopped early, or whose staging still holds recordings nobody has muxed (audit A5, A6) — the embed says the opposite of the truth at exactly the moment the operator could act on it.

**Every line number below is against `main` at `bbe0e674` (N1 merged).** N1 inserted `sendMuxingStarting` (~60 lines) and `finishedImage` (~16) into this file, so the numbers moved from the pre-N1 tree; re-verify each before editing.

**Files:**
- Modify: `internal/worker/orchestrator_mux.go` (the multi-part block at `:1028-1077`; `finishedImage` at `:1225-1240` — **deleted**; `sendFinishedNotification` at `:1242-1337`)
- Modify: `internal/worker/orchestrator_mux_test.go` (N1's `TestFinishedImageIsDroppedForTwitch` at `:217-243` and `TestDescriptionExcerptCutsOnARuneBoundary` at `:262` — **both deleted**, see Steps 4 and 6)
- Modify: `internal/worker/job_notifications_test.go`

**Interfaces:**
- Consumes: `notifications.DownloadFinished`, `notifications.Part`, `notifications.JobFacts`, `NotifyFacts` (Tasks 1–2); `asideReport(stagingDir string) AsideReport` (`internal/worker/orchestrator_mux.go:365`); `chatStatusIncomplete` (`internal/worker/orchestrator_chat.go:288`); `database.Segment` fields `Quality`, `Filename`, `FileSize`, `DurationSeconds`, `VideoWidth/Height/Fps` (`internal/database/types.go:198-216`); `database.Job` fields `IncompleteTail` (`:154`), `ChatStatus` (`:96`), `TotalChatMessages`, `LastVideoSeq`, `LastAudioSeq`, `DownloadStartedAt`, `LengthSeconds`, `SelectedVideoItag`, `SelectedAudioItag`, `StartTime`, `EndTime`, `Description`; `FormatSecondsToTimestamp`.
- Produces, in package `worker`:
  - `func (o *DownloadOrchestrator) sendDownloadFinished(jobCtx *JobContext, finishedJob *database.Job, parts []notifications.Part)`
  - `func (o *DownloadOrchestrator) finishedFacts(jobCtx *JobContext, j *database.Job) notifications.JobFacts`
  - `func formatSelectionLabel(j *database.Job) string`
  - `func trimmedRangeLabel(j *database.Job) string`

- [ ] **Step 1: Write the failing tests**

Append to `internal/worker/job_notifications_test.go`:

```go
// finishedJobRow is the smallest row the finished embed reads.
func finishedJobRow() *database.Job {
	msgs := 4210
	vSeq, aSeq := 1234, 1230
	length := 3600
	return &database.Job{
		ID: "vidF", VideoID: "vidF", Platform: "youtube", Title: "Archived Stream",
		ChannelName: "A Channel", URL: "https://www.youtube.com/watch?v=vidF",
		ThumbnailURL:      "https://i.ytimg.example/t.jpg",
		TotalChatMessages: &msgs, LastVideoSeq: &vSeq, LastAudioSeq: &aSeq,
		LengthSeconds: &length,
	}
}

// TestFinishedEmbedIsWarningWhenTheTailIsIncomplete is audit A5. The archive is
// short, the fix is one Resume click, and the embed said "Successfully
// archived" in green.
//
// Mutants this kill:
//   - keeping TypeSuccess: the colour is the only thing most readers see.
//   - adding the Tail field without the colour, or vice versa.
func TestFinishedEmbedIsWarningWhenTheTailIsIncomplete(t *testing.T) {
	rec := notificationtest.New()
	o := &DownloadOrchestrator{logger: discardLogger{}, notifier: rec}
	j := finishedJobRow()
	j.IncompleteTail = true

	o.sendDownloadFinished(&JobContext{Job: j}, j, []notifications.Part{{File: "out.mp4"}})

	calls := rec.ByEvent("finished")
	if len(calls) != 1 {
		t.Fatalf("recorded %d finished calls, want 1", len(calls))
	}
	if calls[0].Type != notifications.TypeWarning {
		t.Errorf("type = %v, want TypeWarning for an incomplete tail", calls[0].Type)
	}
	if got := notifyField(t, calls[0], "Tail"); got != "incomplete — Resume appends the rest" {
		t.Errorf("Tail = %q", got)
	}
}

// TestFinishedEmbedReportsAnIncompleteChat: chatStatusForOutcome already wrote
// the verdict to the row and both UIs render it; the embed did not.
//
// Mutant: reading TotalChatMessages instead of ChatStatus — a truncated
// capture with thousands of messages reads as complete, which is the exact
// ranking bug chatStatusForOutcome exists to prevent.
func TestFinishedEmbedReportsAnIncompleteChat(t *testing.T) {
	rec := notificationtest.New()
	o := &DownloadOrchestrator{logger: discardLogger{}, notifier: rec}
	j := finishedJobRow()
	j.ChatStatus = chatStatusIncomplete

	o.sendDownloadFinished(&JobContext{Job: j}, j, []notifications.Part{{File: "out.mp4"}})

	if got := notifyField(t, rec.ByEvent("finished")[0], "Chat"); got != "incomplete" {
		t.Errorf("Chat = %q, want %q", got, "incomplete")
	}
	if rec.ByEvent("finished")[0].Type != notifications.TypeSuccess {
		t.Error("an incomplete chat alone must not recolour the embed — the recording is whole")
	}
}

// TestFinishedEmbedCountsSetAsideRecordings is audit A6: Arc A built the
// Recover verb, and nothing ever told an operator there was something to
// recover.
//
// Mutant: scanning the OUTPUT dir instead of the staging dir — the count is
// always zero and the field never appears.
func TestFinishedEmbedCountsSetAsideRecordings(t *testing.T) {
	rec := notificationtest.New()
	o := &DownloadOrchestrator{logger: discardLogger{}, notifier: rec}
	j := finishedJobRow()

	staging := t.TempDir()
	writeAsidePair(t, staging, "1700000000", 8, false, false)

	o.sendDownloadFinished(&JobContext{Job: j, StagingDir: staging}, j, []notifications.Part{{File: "out.mp4"}})

	if got := notifyField(t, rec.ByEvent("finished")[0], "Set-aside recordings"); got != "1 — Recover to mux them" {
		t.Errorf("Set-aside recordings = %q", got)
	}
}

// TestFinishedEmbedShapesPerPartCount is audit C1: the two builders' field
// sets are now one, and the job-level facts the multi-part path used to drop
// arrive on both.
//
// Mutants this kill:
//   - emitting "Parts" for a single-part job (noise on the common case).
//   - keeping the multi-part path's blindness to Format Selection.
//   - labelling the summed size "File Size" for a split job.
func TestFinishedEmbedShapesPerPartCount(t *testing.T) {
	vItag, aItag := 299, 251
	t.Run("single part", func(t *testing.T) {
		rec := notificationtest.New()
		o := &DownloadOrchestrator{logger: discardLogger{}, notifier: rec}
		j := finishedJobRow()
		j.SelectedVideoItag, j.SelectedAudioItag = &vItag, &aItag

		o.sendDownloadFinished(&JobContext{Job: j}, j, []notifications.Part{
			{File: "out.mp4", Size: 1 << 30, Width: 1920, Height: 1080, Fps: 60},
		})

		c := rec.ByEvent("finished")[0]
		if notifyField(t, c, "File") != "out.mp4" {
			t.Error("the single-part shape lost its File field")
		}
		notifyField(t, c, "File Size")
		notifyField(t, c, "Resolution")
		notifyField(t, c, "Format Selection")
		for _, f := range c.Fields {
			if f.Name == "Parts" {
				t.Error("a single-part job must not report a part count")
			}
		}
	})

	t.Run("three parts", func(t *testing.T) {
		rec := notificationtest.New()
		o := &DownloadOrchestrator{logger: discardLogger{}, notifier: rec}
		j := finishedJobRow()
		j.SelectedVideoItag, j.SelectedAudioItag = &vItag, &aItag

		o.sendDownloadFinished(&JobContext{Job: j}, j, []notifications.Part{
			{Quality: "1080p60", Size: 1 << 29, Width: 1920, Height: 1080},
			{Quality: "720p60", Size: 1 << 28},
			{Quality: "720p60", Size: 1 << 28},
		})

		c := rec.ByEvent("finished")[0]
		if got := notifyField(t, c, "Parts"); got != "3" {
			t.Errorf("Parts = %q", got)
		}
		if got := notifyField(t, c, "Qualities"); got != "1080p60 -> 720p60 -> 720p60" {
			t.Errorf("Qualities = %q", got)
		}
		notifyField(t, c, "Total Size")
		// The job-level facts the multi-part builder used to drop.
		notifyField(t, c, "Format Selection")
		notifyField(t, c, "Segments")
		notifyField(t, c, "Chat Messages")
	})

	// The single-part path's own Part assembly. Every other subtest drives
	// sendDownloadFinished directly, which leaves the probeData / os.FileInfo /
	// LengthSeconds -> Part mapping unasserted — and a Part with a zero
	// Duration silently produces an embed with NO Duration field at all.
	//
	// Mutants this kills: dropping the LengthSeconds -> Part.Duration line, or
	// the probeData -> Width/Height/Fps lines, or filepath.Base.
	t.Run("the single-part producer fills the Part", func(t *testing.T) {
		rec := notificationtest.New()
		o := &DownloadOrchestrator{logger: discardLogger{}, notifier: rec}
		j := finishedJobRow() // LengthSeconds = 3600

		o.sendFinishedNotification(&JobContext{Job: j}, j,
			filepath.Join("C:", "out", "out.mp4"),
			&ffprobeData{Width: 1920, Height: 1080, Fps: 60}, nil)

		c := rec.ByEvent("finished")[0]
		if got := notifyField(t, c, "File"); got != "out.mp4" {
			t.Errorf("File = %q, want the base name", got)
		}
		if got := notifyField(t, c, "Resolution"); got != "1920x1080 @60fps" {
			t.Errorf("Resolution = %q", got)
		}
		notifyField(t, c, "Duration") // from LengthSeconds; absent if the mapping is dropped
	})
}

// TestFinishedEmbedDropsTheDeadTwitchImage is the §0 ruling: a Twitch preview
// URL 404s once the stream ends, so the image on a finished Twitch embed is
// broken by the time anybody reads it.
//
// Mutant: setting Image unconditionally — the Twitch embed renders a broken
// picture and nothing in the suite notices.
func TestFinishedEmbedDropsTheDeadTwitchImage(t *testing.T) {
	rec := notificationtest.New()
	o := &DownloadOrchestrator{logger: discardLogger{}, notifier: rec}
	j := finishedJobRow()
	j.Platform = "twitch"
	j.ID, j.VideoID = "tw_F", "F"

	o.sendDownloadFinished(&JobContext{Job: j}, j, []notifications.Part{{File: "out.mp4"}})

	c := rec.ByEvent("finished")[0]
	if c.Opts.Image != "" || c.Opts.Thumbnail != "" {
		t.Errorf("twitch finished carries Image=%q Thumbnail=%q, want neither", c.Opts.Image, c.Opts.Thumbnail)
	}

	rec.Reset()
	j.Platform, j.ID, j.VideoID = "youtube", "vidF", "vidF"
	o.sendDownloadFinished(&JobContext{Job: j}, j, []notifications.Part{{File: "out.mp4"}})
	c = rec.ByEvent("finished")[0]
	if c.Opts.Image == "" {
		t.Error("the YouTube finished embed lost the full-width image it has always had")
	}
	if c.Opts.Thumbnail != "" {
		t.Error("the finished embed shows the same picture twice — it has always used the full-width image alone")
	}
}
```

`writeAsidePair(t, dir, stamp string, size int, withSidecar, withAudio bool)` is the existing helper in `internal/worker/asides_test.go:25`; confirm its signature before use. Add `path/filepath` and `github.com/vampiricwulf/Moombox/internal/notifications` to `job_notifications_test.go`'s import block.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
cd /d/Git/Moombox/.worktrees/webhooks-n2a-producers
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -timeout 300s -run 'TestFinishedEmbed' ./internal/worker/
```

Expected: FAIL to build — `o.sendDownloadFinished undefined (type *DownloadOrchestrator has no field or method sendDownloadFinished)`.

- [ ] **Step 3: Add the shared send and its facts to `internal/worker/orchestrator_mux.go`**

Insert immediately BEFORE `finishedImage` (`:1225`), which Step 4 deletes:

```go
// sendDownloadFinished is the ONE "Download Finished" send. Both finalize
// paths reach it: the single-part path with one Part, the multi-segment path
// with one per part. Before this there were two builders for the same moment
// with different field sets, and the split job's embed silently dropped the
// format selection, the trimmed range and the description for no recorded
// reason (audit C1).
//
// It is called BEFORE cleanupStagingAfterMux runs (processJob), which is what
// makes the set-aside count answerable at all.
func (o *DownloadOrchestrator) sendDownloadFinished(jobCtx *JobContext, finishedJob *database.Job, parts []notifications.Part) {
	if o.notifier == nil {
		return
	}
	if finishedJob == nil {
		finishedJob = jobCtx.Job
	}
	o.notifier.Send(notifications.DownloadFinished(o.finishedFacts(jobCtx, finishedJob), parts))
}

// finishedFacts describes a finished job to the embed builder, including the
// three outcome truths the embed never carried:
//
//   - incomplete_tail: the recording is knowingly short and Resume appends the
//     rest. finalizeIncompleteTail writes the flag and logs it; the embed said
//     "Successfully archived" in green (audit A5).
//   - chat_status == incomplete: the capture STOPPED, which is not the same as
//     running out of chat. Read off the row rather than off the message count,
//     which is the ranking chatStatusForOutcome exists to reject.
//   - set-aside recordings still in staging: Arc A built the Recover verb and
//     nothing ever said there was something to recover (audit A6).
//
// The row is the fresh one UpdateJobFields returned, so all three are current.
func (o *DownloadOrchestrator) finishedFacts(jobCtx *JobContext, j *database.Job) notifications.JobFacts {
	f := NotifyFacts(j)
	if j.DownloadStartedAt != "" {
		if startedAt, err := time.Parse(time.RFC3339, j.DownloadStartedAt); err == nil {
			f.TotalTime = time.Since(startedAt)
		}
	}
	if j.LastVideoSeq != nil {
		f.SegmentCounter = fmt.Sprintf("V: %d", *j.LastVideoSeq)
		if j.LastAudioSeq != nil {
			f.SegmentCounter += fmt.Sprintf(" A: %d", *j.LastAudioSeq)
		}
	}
	f.ChatMessages = j.TotalChatMessages
	f.FormatSelection = formatSelectionLabel(j)
	f.TrimmedRange = trimmedRangeLabel(j)
	f.Description = j.Description
	f.IncompleteTail = j.IncompleteTail
	f.ChatIncomplete = j.ChatStatus == chatStatusIncomplete
	if jobCtx != nil && jobCtx.StagingDir != "" {
		f.AsideCount = len(asideReport(jobCtx.StagingDir).Groups)
	}
	return f
}

// formatSelectionLabel renders the itags the operator chose, or "" when they
// took the defaults. -1 is the sentinel for "none of this stream".
func formatSelectionLabel(j *database.Job) string {
	if j.SelectedVideoItag == nil && j.SelectedAudioItag == nil {
		return ""
	}
	var out string
	if j.SelectedVideoItag != nil {
		out = fmt.Sprintf("Video: itag %d", *j.SelectedVideoItag)
		if *j.SelectedVideoItag == -1 {
			out = "Video: None"
		}
	}
	if j.SelectedAudioItag != nil {
		if out != "" {
			out += ", "
		}
		if *j.SelectedAudioItag == -1 {
			out += "Audio: None"
		} else {
			out += fmt.Sprintf("Audio: itag %d", *j.SelectedAudioItag)
		}
	}
	return out
}

// trimmedRangeLabel renders the post-download trim bounds, or "" when the job
// had none. An absent start is 0:00 and an absent end is the end of the file.
func trimmedRangeLabel(j *database.Job) string {
	if j.StartTime == nil && j.EndTime == nil {
		return ""
	}
	startStr, endStr := "0:00", "end"
	if j.StartTime != nil {
		startStr = FormatSecondsToTimestamp(*j.StartTime)
	}
	if j.EndTime != nil {
		endStr = FormatSecondsToTimestamp(*j.EndTime)
	}
	return fmt.Sprintf("%s - %s", startStr, endStr)
}
```

- [ ] **Step 4: Replace `sendFinishedNotification`'s body (`:1242-1337`) and delete `finishedImage` (`:1225-1240`)**

The whole function becomes the single-part Part assembly:

```go
// sendFinishedNotification is the single-part finalize path's call into the
// shared finished send. It exists as its own function only because its caller
// (:870) holds the probe result and the FileInfo, which nothing else does.
func (o *DownloadOrchestrator) sendFinishedNotification(jobCtx *JobContext, finishedJob *database.Job, outputFile string, probeData *ffprobeData, info os.FileInfo) {
	if finishedJob == nil {
		finishedJob = jobCtx.Job
	}
	p := notifications.Part{File: filepath.Base(outputFile)}
	if probeData != nil {
		p.Width, p.Height, p.Fps = probeData.Width, probeData.Height, probeData.Fps
	}
	if info != nil {
		p.Size = info.Size()
	}
	if finishedJob.LengthSeconds != nil && *finishedJob.LengthSeconds > 0 {
		p.Duration = time.Duration(*finishedJob.LengthSeconds) * time.Second
	}
	o.sendDownloadFinished(jobCtx, finishedJob, []notifications.Part{p})
}
```

Everything it used to build — the FieldBuilder, the resolution string, the itag label, the trimmed range, the 300-rune description clamp — is gone; `formatSelectionLabel`, `trimmedRangeLabel` and the builder's own `EscapeMarkdown(ClampRunes(…, descriptionExcerptLen))` own those now. **The `descMaxLen` constant at `:1325` and the `EscapeMarkdown(ClampRunes(...))` line at `:1327` go with it** — N1 already made that cut rune-safe here, and this task moves the SAME composition into the builder, where every caller gets it. Do not leave a second truncator behind.

**Delete `finishedImage` (`:1225-1240`) too, in this commit.** N1 added it for the §0 Twitch ruling and calls it at exactly the two sends this task replaces (`:1074` and `:1335`); `DownloadFinished` now carries the rule. Left in place it is unreachable production code kept alive only by its own test, and the rule would live in two files.

Two N1 tests in `internal/worker/orchestrator_mux_test.go` go with the code they pin:

- `TestFinishedImageIsDroppedForTwitch` (`:217-243`) — `TestFinishedEmbedDropsTheDeadTwitchImage` (Step 1) asserts the same youtube/twitch cases through a recorded send, and the nil-job case is covered by `sendDownloadFinished`'s own `finishedJob == nil` fallback.
- `TestDescriptionExcerptCutsOnARuneBoundary` (`:262`) — it builds its own closure so it still COMPILES, but after this step it pins a composition no worker code performs. Task 1's `TestDescriptionExcerptIsRuneSafeAndBounded` pins the same composition at the only place that still performs it.

- [ ] **Step 5: Replace the multi-part block (`:1028-1077`)**

```go
	// Send notification
	if o.notifier != nil {
		finishedJob, _ := o.db.GetJob(jobCtx.Job.ID)
		if finishedJob == nil {
			finishedJob = jobCtx.Job
		}
		parts := make([]notifications.Part, 0, len(segments))
		for _, seg := range segments {
			p := notifications.Part{
				File:     seg.Filename,
				Quality:  seg.Quality,
				Duration: time.Duration(seg.DurationSeconds * float64(time.Second)),
			}
			if seg.FileSize != nil {
				p.Size = *seg.FileSize
			}
			if seg.VideoWidth != nil {
				p.Width = *seg.VideoWidth
			}
			if seg.VideoHeight != nil {
				p.Height = *seg.VideoHeight
			}
			if seg.VideoFps != nil {
				p.Fps = *seg.VideoFps
			}
			parts = append(parts, p)
		}
		o.sendDownloadFinished(jobCtx, finishedJob, parts)
	}
```

The `qualityLabels`/`finFields` locals and the inline resolution, total-time and chat-message blocks are deleted, along with this site's `finishedImage(jobCtx.Job)` call (`:1074`). `totalSize` and `totalDuration` (`:953-960`) stay — the DB update above still writes them — but the embed no longer reads them: the builder sums the parts, so the two can never drift.

- [ ] **Step 6: Run the tests to verify they pass**

```bash
cd /d/Git/Moombox/.worktrees/webhooks-n2a-producers
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go test -count=1 -timeout 300s -run 'TestFinishedEmbed' -v ./internal/worker/
go test -count=1 -timeout 300s ./internal/worker/
go vet ./internal/worker/
gofmt -l ./internal/worker
```

Expected: the five new tests PASS; the package is `ok` (the FFmpeg-backed mux tests must report PASS, not SKIP — a SKIP here means the finalize paths were never executed end to end); vet silent; `gofmt -l` prints nothing.

- [ ] **Step 7: Commit**

```bash
cd /d/Git/Moombox/.worktrees/webhooks-n2a-producers
git add internal/worker/orchestrator_mux.go internal/worker/orchestrator_mux_test.go internal/worker/job_notifications_test.go
git commit -m "feat(worker): one finished embed, and it tells the truth

Two builders described the same moment with different field sets. The
multi-segment one reported parts, qualities and a total size and silently
dropped the format selection, the trimmed range and the description; the
single-part one reported all three and knew nothing about parts. Whichever a
job got depended on whether its stream happened to change quality.

sendDownloadFinished is the one send both finalize paths reach, with one
notifications.Part per output. The builder sums their sizes and durations, so
the embed's totals can no longer drift from the row's, and the job-level facts
now arrive on both shapes.

The embed also stops lying about the outcome. A job flagged incomplete_tail is
Warning-coloured and says Resume appends the rest; a chat capture that STOPPED
is reported as incomplete from chat_status rather than guessed from the message
count; and set-aside recordings still sitting in staging are counted, which is
the first time anything has told an operator that the Recover verb has work to
do. The finished embed's image is dropped for Twitch, whose preview URL 404s
the moment the stream ends.

finishedImage and its test go with the two call sites: the Twitch ruling now
lives in DownloadFinished, pinned by TestFinishedEmbedDropsTheDeadTwitchImage,
which asserts the same cases through a recorded send. The description clamp
goes the same way — the builder spends ClampRunes, which Arc N1 exported for
this producer excerpt, so there is one truncator and not two.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/worker/orchestrator_mux.go internal/worker/orchestrator_mux_test.go internal/worker/job_notifications_test.go
```

---

### Task 5: One `TrimCreated` send, and the Job Failed embed's stage and staging

Two trim builders differ only by a Segments field (audit C2). And "Job Failed" (audit row #20) reports an error string that gives no hint which button fixes it: a mux failure arrives as `"mux: …"` and leaves staging intact so Resume works, while a download failure may have left nothing — the exact distinction `reference_retry_vs_resume_semantics` warns about (audit M8).

**Files:**
- Modify: `internal/worker/trim.go` (the single-file send at `:199-223`; the multi-segment send at `:502-531`)
- Modify: `internal/worker/worker.go` (`setJobError`'s "Job Failed" block at `:1324-1349`)
- Modify: `internal/worker/job_notifications_test.go`

**Interfaces:**
- Consumes: `notifications.TrimCreated`, `notifications.TrimFacts`, `NotifyFacts` (Tasks 1–2); `HasStagingFiles(stagingBase, jobID string) bool` (`internal/worker/staging.go:10`); `ScanAsides(stagingBase, jobID string) AsideReport` (`internal/worker/orchestrator_mux.go:359`); `(*DownloadWorker).readConfig` (`internal/worker/worker.go:257`); `config.PathsConfig.EffectiveStagingDir()`.
- Produces: `func errorStage(errMsg string) string` in package `worker`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/worker/job_notifications_test.go`:

```go
// TestErrorStageClassifiesTheFinalizeErrors pins the one signal there is: the
// prefixes the orchestrator itself writes.
//
// Mutants this kill:
//   - matching "mux" anywhere in the string: an ffmpeg stderr tail from a
//     DOWNLOAD failure that happens to mention muxing flips the answer.
//   - dropping the two non-"mux" finalize prefixes: "no media files to mux"
//     and "create output dir" are mux-stage failures that do not start with
//     the word.
func TestErrorStageClassifiesTheFinalizeErrors(t *testing.T) {
	for msg, want := range map[string]string{
		"mux: ffmpeg: exit status 1 (stderr: …)":                           "mux",
		"mux produced 12s from a 3600s input (3588s missing) — the copy …": "mux",
		"mux segment 3: ffmpeg: exit status 1":                             "mux",
		"no media files to mux":                                            "mux",
		"no media files to mux for segment 2":                              "mux",
		"create output dir: mkdir E:\\out: access is denied":               "mux",
		"twitch channel is offline":                                        "download",
		"failed to fetch segment 1234: context deadline exceeded":          "download",
		"ffmpeg stderr mentions muxing somewhere in the tail":              "download",
		// The two finalize returns the prefixes still miss — pinned so the
		// residual is a fact in the suite, not only in a report.
		"create segment output dir: mkdir: denied":    "download",
		"no segment files found in staging directory": "download",
	} {
		if got := errorStage(msg); got != want {
			t.Errorf("errorStage(%q) = %q, want %q", msg, got, want)
		}
	}
}

// TestJobFailedNamesTheStageAndTheStaging is audit M8: the embed reported an
// error string and left the operator to guess whether Retry (which DELETES
// staging) or Resume (which preserves it) is the right button.
//
// Mutants this kill:
//   - claiming "Resume available" for a Twitch job: Resume is YouTube-only in
//     both UIs and the route answers 400.
//   - reading the staging flag before the error is committed, or from the
//     output directory.
func TestJobFailedNamesTheStageAndTheStaging(t *testing.T) {
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)
	rec := notificationtest.New()
	w.notifier = rec

	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })

	t.Run("mux failure with staging preserved", func(t *testing.T) {
		rec.Reset()
		job := &database.Job{ID: "vidE1", VideoID: "vidE1", Platform: "youtube", Title: "Failed", Status: database.StatusDownloading}
		if _, err := db.AddJob(job); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(stagingBase, job.ID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "video.mp4"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}

		w.setJobError(job, errors.New("mux: ffmpeg: exit status 1"))

		calls := rec.ByEvent("error")
		if len(calls) != 1 {
			t.Fatalf("recorded %d error calls, want 1", len(calls))
		}
		if got := notifyField(t, calls[0], "Stage"); got != "mux" {
			t.Errorf("Stage = %q", got)
		}
		if got := notifyField(t, calls[0], "Staging"); got != "preserved — Resume available" {
			t.Errorf("Staging = %q", got)
		}
	})

	t.Run("download failure with nothing staged", func(t *testing.T) {
		rec.Reset()
		job := &database.Job{ID: "vidE2", VideoID: "vidE2", Platform: "youtube", Title: "Failed", Status: database.StatusDownloading}
		if _, err := db.AddJob(job); err != nil {
			t.Fatal(err)
		}

		w.setJobError(job, errors.New("twitch channel is offline"))

		c := rec.ByEvent("error")[0]
		if got := notifyField(t, c, "Stage"); got != "download" {
			t.Errorf("Stage = %q", got)
		}
		if got := notifyField(t, c, "Staging"); got != "removed" {
			t.Errorf("Staging = %q", got)
		}
	})

	t.Run("twitch never promises Resume", func(t *testing.T) {
		rec.Reset()
		job := &database.Job{ID: "tw_E3", VideoID: "E3", Platform: "twitch", Title: "Failed", Status: database.StatusDownloading}
		if _, err := db.AddJob(job); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(stagingBase, job.ID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "video.ts"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}

		w.setJobError(job, errors.New("mux: ffmpeg: exit status 1"))

		if got := notifyField(t, rec.ByEvent("error")[0], "Staging"); got != "preserved" {
			t.Errorf("Staging = %q — Resume is YouTube-only in both UIs and the route answers 400", got)
		}
	})
}

// TestTrimCreatedIsOneEmbed is audit C2.
//
// Mutant: emitting Segments for a single-file trim — the field claims a split
// that never happened.
func TestTrimCreatedIsOneEmbed(t *testing.T) {
	rec := notificationtest.New()
	size := int64(1 << 20)
	f := NotifyFacts(&database.Job{
		ID: "vidT", VideoID: "vidT", Platform: "youtube", Title: "Source",
		ChannelName: "A Channel", URL: "https://www.youtube.com/watch?v=vidT",
	})

	rec.Send(notifications.TrimCreated(f, notifications.TrimFacts{
		TimeRange: "0:10 - 0:40", Duration: 30 * time.Second, Size: &size,
	}))
	c := rec.ByEvent("trim_created")[0]
	if c.Title != "Trim Created" {
		t.Errorf("title = %q", c.Title)
	}
	notifyField(t, c, "Source Video")
	notifyField(t, c, "File Size")
	for _, fl := range c.Fields {
		if fl.Name == "Segments" {
			t.Error("a single-file trim must not report Segments")
		}
	}

	rec.Reset()
	rec.Send(notifications.TrimCreated(f, notifications.TrimFacts{
		TimeRange: "0:10 - 0:40", Duration: 30 * time.Second, Parts: 3,
	}))
	if got := notifyField(t, rec.ByEvent("trim_created")[0], "Segments"); got != "3 segments" {
		t.Errorf("Segments = %q", got)
	}
}
```

Add `errors`, `os`, `path/filepath`, `time` and `internal/config` to the test's imports as needed.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
cd /d/Git/Moombox/.worktrees/webhooks-n2a-producers
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -timeout 300s -run 'TestErrorStage|TestJobFailedNames|TestTrimCreatedIsOneEmbed' ./internal/worker/
```

Expected: FAIL to build — `undefined: errorStage`. After that compiles, `TestJobFailedNames` FAILS with `field "Stage" missing from "Job Failed"`.

- [ ] **Step 3: Add `errorStage` and the two fields to `setJobError`**

In `internal/worker/worker.go`, above `setJobError` (`:1245`):

```go
// errorStage answers "which button fixes this" from the only signal the error
// carries: the prefix the orchestrator writes.
//
// Four finalize shapes exist and all four are the mux stage: muxAndFinalize's
// fmt.Errorf("mux: %w", err) (orchestrator_mux.go:790), verifyMuxedDuration's
// short-output refusal "mux produced …" (:160), "no media files to mux…"
// (:786, :1407) and "create output dir: %w" (:605, :751, :936). The "mux"
// prefix also keeps "mux segment N:" (:1412, :1427) on the mux side. All of
// them reach setJobError unwrapped — ExecuteWithChat returns the finalize
// error straight through — so the prefix survives. Everything else is the
// download stage.
//
// The prefixes are anchored deliberately: an ffmpeg stderr tail from a
// DOWNLOAD failure can mention muxing anywhere in its 500 characters, and a
// substring match would flip the answer for the case that matters most.
//
// Known limit: two finalize returns still read as "download" — "create segment
// output dir: …" (orchestrator_mux.go:1384) and "no segment files found in
// staging directory" (:1524). Naming them would mean teaching every producer a
// stage argument; the prefixes below are what exists today.
func errorStage(errMsg string) string {
	// "mux" alone covers "mux: …", "mux produced …" and "mux segment N: …".
	for _, p := range []string{"mux", "no media files to mux", "create output dir"} {
		if strings.HasPrefix(errMsg, p) {
			return "mux"
		}
	}
	return "download"
}
```

Inside `setJobError`'s non-cookie branch (`:1324-1349`), extend the FieldBuilder:

```go
			var stagingBase string
			w.readConfig(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })
			// "preserved" is the same predicate the resume route gates on
			// (HasStagingFiles), and Resume is YouTube-only in both UIs — a
			// Twitch job told "Resume available" gets a 400.
			staging := "removed"
			if HasStagingFiles(stagingBase, job.ID) {
				staging = "preserved"
				if job.Platform != "twitch" {
					staging = "preserved — Resume available"
				}
			}
			asides := len(ScanAsides(stagingBase, job.ID).Groups)

			fields := notifications.NewFieldBuilder().
				AddInline("Channel", notifications.EscapeMarkdown(job.ChannelName)).
				AddInline(notifications.IDLabel(job.Platform), job.VideoID).
				// Error is ALREADY wrapped by N1 — worker.go:1332 is one of the
				// four sites N1 escapes. Carry N1's line through unchanged; a
				// second wrap renders every \* as \\*. ChannelName is NOT one
				// of N1's four, so the wrap above is new and single.
				Add("Error", notifications.EscapeMarkdown(errMsg)).
				AddInline("Stage", errorStage(errMsg)).
				AddInline("Staging", staging).
				AddIf(job.AutoRetryCount > 0, "Automatic Retries",
					fmt.Sprintf("gave up after %d/%d", job.AutoRetryCount, MaxTwitchAutoRetries)).
				AddIf(asides > 0, "Set-aside recordings",
					fmt.Sprintf("%d — Recover to mux them", asides)).
				Build()
```

Leave the `notifURL` fallback exactly as N1 left it — if N1 has not already replaced it, swap the whole `SendOptions` literal for `worker.NotifyFacts`-derived opts is **out of scope here**: this send is not one of the five builders (spec §2.2 lists five, and "Job Failed" is not among them), so only its FIELDS change.

**Before writing this block, open `internal/worker/worker.go:1332` on the merged tree and copy N1's exact `Error` line into it. Do not add a second `EscapeMarkdown`.**

`worker.go:1332` is the **only** N1↔N2a escape overlap. The other three N1 escape sites resolve without a collision: `internal/worker/orchestrator.go:657` ("Trim Failed") is not touched by this arc; `cmd/moombox/monitor_callbacks.go:1651` (`Last Error`) is rewritten wholesale by Task 8, whose paste carries exactly one `EscapeMarkdown(lastErr)`; and `internal/worker/orchestrator_mux.go:1327` (the `EscapeMarkdown(ClampRunes(...))` Description excerpt) is **deleted** by Task 4, after which `DownloadFinished` performs the same composition once. Every other builder output is escaped once per site — `Title` through `displayName`, `Channel`, `Category`, `Part.File`, `Description` — with two deliberate exemptions, `Author.Name` (raw, pinned by a test) and `VideoID` (raw, documented on `addIDField`).

- [ ] **Step 4: Adopt `TrimCreated` at both trim sites**

`internal/worker/trim.go`, the single-file send (`:199-223`):

```go
	// Send "Trim Created" notification
	if ts.notifier != nil {
		ts.notifier.Send(notifications.TrimCreated(NotifyFacts(job), notifications.TrimFacts{
			TimeRange: fmt.Sprintf("%s - %s", FormatSecondsToTimestamp(startTime), FormatSecondsToTimestamp(endTime)),
			Duration:  time.Duration(duration) * time.Second,
			Size:      fileSize,
		}))
	}
```

and the multi-segment send (`:502-531`):

```go
	// Send notification
	if ts.notifier != nil {
		ts.notifier.Send(notifications.TrimCreated(NotifyFacts(job), notifications.TrimFacts{
			TimeRange: fmt.Sprintf("%s - %s", FormatSecondsToTimestamp(startTime), FormatSecondsToTimestamp(endTime)),
			Duration:  time.Duration(trimDuration) * time.Second,
			Size:      fileSize,
			Parts:     len(involved),
		}))
	}
```

The `timeRange`/`durStr`/`fields` locals go; delete any that become unused. **"Trim Deleted" (`:265`) is untouched** — one site, one shape, nothing to unify.

- [ ] **Step 5: Run the tests to verify they pass**

```bash
cd /d/Git/Moombox/.worktrees/webhooks-n2a-producers
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go test -count=1 -timeout 300s -run 'TestErrorStage|TestJobFailedNames|TestTrimCreatedIsOneEmbed' -v ./internal/worker/
go test -count=1 -timeout 300s ./internal/worker/
go vet ./internal/worker/
gofmt -l ./internal/worker
```

Expected: the three new tests PASS (six subtests), the package is `ok`, vet silent, `gofmt -l` prints nothing.

- [ ] **Step 6: Commit**

```bash
cd /d/Git/Moombox/.worktrees/webhooks-n2a-producers
git add internal/worker/trim.go internal/worker/worker.go internal/worker/job_notifications_test.go
git commit -m "feat(worker): one Trim Created embed, and Job Failed names the button

The two trim builders differed by a single Segments field; both now call
notifications.TrimCreated, which adds that field only when the trim actually
spans more than one part.

Job Failed reported an error string and left the operator to guess. It now says
which stage failed — read off the prefix the orchestrator already writes, mux:
from the copy and mux produced from the short-output refusal — and whether
staging survived, using the same HasStagingFiles predicate the resume route
gates on. Resume is promised only for YouTube, because that is the only
platform either UI will resume and the route answers 400 for anything else.
A failure that left set-aside recordings behind says how many.

That is the Retry-versus-Resume distinction the two verbs' semantics turn on:
Retry deletes staging, Resume keeps it, and the embed was the one surface that
said neither.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/worker/trim.go internal/worker/worker.go internal/worker/job_notifications_test.go
```

---

### Task 6: The four recovery keys — vocabulary, both UI registries, and the docs

Four alerts have no close (audit A1, A2, A3 and the "Symmetric pairs with a missing half" list): the sidecar can die and come back, the disk can fill and be freed, disk monitoring can fail and recover, and a channel can stop responding and start again — and in every case the operator who acted at 03:00 gets no confirmation. Per the §0 ruling the closes are NEW keys, aliased to their alert keys so a target that filters the alert keeps receiving the close without touching its config, and a target can subscribe to the close alone.

This task lands the vocabulary FIRST so Tasks 7 and 8 send keys that are already canonical, already offered by both UIs and already documented. It also pins the Go and JavaScript registries against each other, which nothing has ever done (audit §4, "Web/Go vocabulary mirror").

**Files:**
- Modify: `internal/notifications/events.go` (`EventGroups` System group at `:40`; the `eventAliases` doc comment at `:65-77` and the map at `:78-86`) — N1 added a 12-line Connectivity comment above these, so they sit lower than in the pre-N1 tree
- Modify: `web/public/modules/settings.js` (`NOTIFICATION_EVENT_GROUPS` System group at `:54-65` — N1 removed the `connectivity_pause` entry)
- Create: `internal/tui/settings_notification_vocab_parity_test.go`
- Modify: `docs/spec/operations.md` (the Event Types table, `:461-488`; the `finished` row `:469`; the `error` row `:470`)
- Modify: `SPEC.md` (the Notifications-Detail event list, `:885`)

**Interfaces:**
- Consumes: `notifications.EventGroups`, `notifications.KnownEvents` (`internal/notifications/events.go:21`, `:52`); `notifEventGroups` / `allNotifEvents` (`internal/tui/settings.go:270`, `:279`); `settingsVM(t)` (`internal/tui/settings_js_vm_test.go:23`) over `webtest.SettingsVM` (`internal/webtest/settings_vm.go:54`).
- Produces: the four event ids `sidecar_down`, `sidecar_restored`, `disk_ok`, `channel_healthy` in both registries, three new alias entries beside N1's two, and the exported reader `AliasOf`.

- [ ] **Step 1: Write the failing test**

Create `internal/tui/settings_notification_vocab_parity_test.go`:

```go
package tui

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/notifications"
)

// notificationIDsFromJS runs the SHIPPED settings.js and reads the ids out of
// NOTIFICATION_EVENT_GROUPS, group by group.
//
// Executed rather than pattern-matched, for the reason settings_restart_parity
// _test.go gives: a source match on a JS list passes on a list that is
// malformed, commented out, or shadowed three lines later. What the browser
// gets is what evaluates.
//
// NOTIFICATION_EVENT_GROUPS is a module-level `const`, which lives in the
// global lexical environment rather than on the global object, so it is read
// as the completion value of an expression rather than via vm.Get.
func notificationIDsFromJS(t *testing.T) ([]string, map[string][]string) {
	t.Helper()
	vm := settingsVM(t)
	v, err := vm.RunString("NOTIFICATION_EVENT_GROUPS")
	if err != nil {
		t.Fatalf("settings.js no longer defines NOTIFICATION_EVENT_GROUPS: %v", err)
	}
	groups, ok := v.Export().([]any)
	if !ok {
		t.Fatalf("NOTIFICATION_EVENT_GROUPS is %T, want an array", v.Export())
	}
	var flat []string
	byGroup := map[string][]string{}
	for i, g := range groups {
		entry, ok := g.(map[string]any)
		if !ok {
			t.Fatalf("NOTIFICATION_EVENT_GROUPS[%d] is %T, want an object", i, g)
		}
		name, _ := entry["name"].(string)
		events, ok := entry["events"].([]any)
		if !ok {
			t.Fatalf("group %q has no events array", name)
		}
		for j, e := range events {
			ev, ok := e.(map[string]any)
			if !ok {
				t.Fatalf("group %q event %d is %T, want an object", name, j, e)
			}
			id, _ := ev["id"].(string)
			label, _ := ev["label"].(string)
			if id == "" {
				t.Fatalf("group %q event %d has no id", name, j)
			}
			if label == "" {
				t.Errorf("event %q has no label — the web editor renders the raw id", id)
			}
			flat = append(flat, id)
			byGroup[name] = append(byGroup[name], id)
		}
	}
	if len(flat) == 0 {
		t.Fatal("NOTIFICATION_EVENT_GROUPS is empty — nothing below can be concluded")
	}
	return flat, byGroup
}

// TestNotificationVocabulariesAgree pins the one fact written in three places:
// the Go registry the manager filters on, the flat list the TUI editor derives,
// and the labelled mirror the web editor renders.
//
// Whichever is edited alone keeps working perfectly while the other UI silently
// stops offering the key — the operator ticks every box they can see and still
// misses the event. Compared as SETS in both directions, and group by group, so
// neither list can grow, shrink, or move a key to the wrong section alone.
//
// internal/tui is the only package that can see all three: it imports
// internal/notifications, owns allNotifEvents, and has the goja harness for the
// shipped settings.js.
func TestNotificationVocabulariesAgree(t *testing.T) {
	jsFlat, jsByGroup := notificationIDsFromJS(t)

	goByGroup := map[string][]string{}
	goFlat := map[string]bool{}
	for _, g := range notifications.EventGroups {
		goByGroup[g.Name] = append(goByGroup[g.Name], g.Events...)
		for _, e := range g.Events {
			goFlat[e] = true
		}
	}

	jsSet := map[string]bool{}
	for _, id := range jsFlat {
		jsSet[id] = true
		if !goFlat[id] {
			t.Errorf("settings.js offers %q, which is not in notifications.EventGroups — the manager will warn it is unknown and never match it", id)
		}
	}
	for e := range goFlat {
		if !jsSet[e] {
			t.Errorf("notifications.EventGroups has %q and settings.js does not — the web editor cannot subscribe to it", e)
		}
	}

	for name, want := range goByGroup {
		got := jsByGroup[name]
		if len(got) != len(want) {
			t.Errorf("group %q: settings.js has %d events, Go has %d (%v vs %v)", name, len(got), len(want), got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("group %q position %d: settings.js has %q, Go has %q — the two editors list the events in different orders", name, i, got[i], want[i])
			}
		}
	}

	// The TUI derives from EventGroups, so this catches a hand-written list
	// creeping back in.
	if len(allNotifEvents) != len(goFlat) {
		t.Errorf("allNotifEvents has %d entries, EventGroups has %d — the TUI list is no longer derived", len(allNotifEvents), len(goFlat))
	}
}

// TestRecoveryEventsAreKnownAndAliased pins the §0 ruling: each close is its
// own key so a target can subscribe to it alone, AND it is aliased to its
// alert so a target that already filters the alert receives the close without
// touching its config.
//
// Mutants this kill:
//   - adding the keys without the aliases: every existing filtered target
//     silently never sees a close.
//   - aliasing the wrong way round (alert -> close): the alias map maps the
//     NEWER key to the OLDER one it split from, and reversing it would make a
//     target that filters only disk_ok start receiving disk warnings.
//   - retyping the map instead of editing it in place, which drops N1's
//     connectivity_resume retirement entry.
func TestRecoveryEventsAreKnownAndAliased(t *testing.T) {
	for _, e := range []string{"sidecar_down", "sidecar_restored", "disk_ok", "channel_healthy"} {
		if !notifications.KnownEvents[e] {
			t.Errorf("%q is not a known event — NewManager warns on a config that lists it", e)
		}
	}
	// All FOUR entries, not just this arc's three: the map is edited in place
	// and the one thing that must never happen to it is an entry being lost.
	// N1's connectivity_resume retirement is the entry a careless retype
	// drops, and dropping it silences a legacy connectivity_pause filter
	// through every outage.
	//
	// closeEv, not close: the builtin is shadowed otherwise, which vet lets
	// pass and staticcheck's predeclared check does not.
	for closeEv, alert := range map[string]string{
		"disk_ok":             "disk_warning",
		"channel_healthy":     "channel_unhealthy",
		"sidecar_restored":    "sidecar_down",
		"disk_critical":       "disk_warning",
		"connectivity_resume": "connectivity_pause",
	} {
		if got := notifications.AliasOf(closeEv); got != alert {
			t.Errorf("alias of %q = %q, want %q", closeEv, got, alert)
		}
	}
}
```

`eventAliases` is unexported and N1 exports **no** reader for it — its own code indexes the map directly — so `TestRecoveryEventsAreKnownAndAliased`'s second half needs one. Step 3 adds `AliasOf`, and Step 2's expected `undefined: notifications.AliasOf` is therefore correct as written.

- [ ] **Step 2: Run the test to verify it fails**

```bash
cd /d/Git/Moombox/.worktrees/webhooks-n2a-producers
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -timeout 300s -run 'TestNotificationVocabulariesAgree|TestRecoveryEventsAreKnownAndAliased' ./internal/tui/
```

Expected: `TestNotificationVocabulariesAgree` PASSES (the two registries agree today — that is the regression guard earning its place before the change), and `TestRecoveryEventsAreKnownAndAliased` FAILS to build with `undefined: notifications.AliasOf`, or, once that exists, with four `"sidecar_down" is not a known event` errors.

- [ ] **Step 3: Add the keys and aliases to `internal/notifications/events.go`**

The System group (`:40`) gains all four — the sidecar, the disk and the channel are all process-level facts, and `disk_warning` / `channel_unhealthy` already live there:

```go
	{"System", []string{"disk_warning", "disk_critical", "disk_ok", "update_available", "update_applied", "update_failed", "crash_recovered", "channel_unhealthy", "channel_healthy", "sidecar_down", "sidecar_restored"}},
```

Each close sits immediately after its alert so both editors read as pairs.

`eventAliases` (`:78-86`) **GAINS three entries** — the map already carries two, `disk_critical` and N1's `connectivity_resume ← connectivity_pause` retirement. **Do not retype the map from this plan; edit it in place.** Deleting N1's entry turns three of its tests red (`TestRetiredConnectivityPauseIsAliasedNotForgotten`, `TestALegacyPauseFilterStillReceivesTheResume`, `TestKnownEventsCoversEveryAliasValue`) and makes a legacy `connectivity_pause` filter go silent through every outage — the exact migration N1 shipped to prevent. The result, keeping the existing NEWER→OLDER direction:

```go
var eventAliases = map[string]string{
	"disk_critical": "disk_warning",
	// C8 (N1): the retired connectivity_pause key. DELETE one release after
	// the retirement ships — it is a migration, not a permanent mapping.
	// (N1's full comment stays; it is abbreviated here.)
	"connectivity_resume": "connectivity_pause",
	// The recovery halves. Each close is its own key, so a target can
	// subscribe to the all-clear alone; the alias means a target that already
	// filters the ALERT receives its close without a config edit, which is
	// the whole point of an incident having an end (§0 ruling).
	"disk_ok":          "disk_warning",
	"channel_healthy":  "channel_unhealthy",
	"sidecar_restored": "sidecar_down",
}

// AliasOf returns the older, broader event a newer key splits from, or "" when
// the key stands alone. Exported for the vocabulary parity test, which is the
// only thing outside this package that needs to see the mapping — the filter
// itself applies it internally.
func AliasOf(event string) string { return eventAliases[event] }
```

The doc comment above `eventAliases` (`:65-77`) already names two idioms — the SPLIT (`disk_critical`) and the RETIREMENT (`connectivity_resume`). Add a third short paragraph for the CLOSE: a recovery key aliased to the alert it ends, permanent like a split rather than expiring like a retirement.

- [ ] **Step 4: Mirror the four keys in `web/public/modules/settings.js`**

The System group (`:54-65`) becomes:

```js
  {
    name: "System",
    events: [
      { id: "disk_warning", label: "Disk Warning" },
      { id: "disk_critical", label: "Disk Critical" },
      { id: "disk_ok", label: "Disk Recovered" },
      { id: "update_available", label: "Update Available" },
      { id: "update_applied", label: "Update Applied" },
      { id: "update_failed", label: "Update Failed" },
      { id: "crash_recovered", label: "Crash Recovered" },
      { id: "channel_unhealthy", label: "Channel Unhealthy" },
      { id: "channel_healthy", label: "Channel Recovered" },
      { id: "sidecar_down", label: "Sidecar Down" },
      { id: "sidecar_restored", label: "Sidecar Restored" },
    ],
  },
```

Order and grouping must match Step 3 exactly — the parity test compares position by position.

The TUI needs no edit: `notifEventGroups` derives from `notifications.EventGroups` (`internal/tui/settings.go:270-276`) and `allNotifEvents` flattens it (`:279-285`).

- [ ] **Step 5: Document the four keys and the two changed rows**

`docs/spec/operations.md`, in the Event Types table (`:461-488`). Insert `disk_ok` after `disk_critical` (`:482`) and `channel_healthy` after `channel_unhealthy` (`:487`), then the two sidecar rows:

```
| `disk_ok` | Disk usage fell back under the warning threshold after a warning or critical alert was sent ("Disk Space Recovered"), or disk monitoring recovered after a read-failure alert ("Disk Monitoring Recovered"). Success-coloured; the close of the `disk_warning`/`disk_critical` family. Targets filtering on `disk_warning` also receive it, via the manager's event alias, so an incident that was reported always gets an end. A reading that closes both incidents at once sends both embeds — two alerts, two closes |
| `channel_healthy` | A channel that fired `channel_unhealthy` answered a check again. Fires only when the alert was actually SENT — a streak suppressed by the cross-monitor confirmation has no alert to close. Aliased to `channel_unhealthy` |
| `sidecar_down` | The BotGuard sidecar has been unhealthy for a continuous 60 seconds. Error-coloured and mention-eligible. The supervisor's restart ladder handles everything shorter, so this is the failure it could not fix: PO tokens fall back to the slower in-process goja solver until it returns. Carries the reason the child died and how many successful restarts this process has made |
| `sidecar_restored` | The sidecar is healthy again after a `sidecar_down` was sent. Success-coloured. Aliased to `sidecar_down`, so a target that filters the outage also receives its close |
```

Amend the `finished` row (`:469`) and the `error` row (`:470`):

```
| `finished` | Job completed successfully. Warning-coloured rather than Success when the row carries `incomplete_tail` — the recording is knowingly short and Resume appends the rest — and the embed also reports an incomplete chat capture and any set-aside recordings still waiting in staging |
| `error` | Job failed. The embed names the stage (`mux` or `download`, read off the error prefixes the orchestrator writes — `mux…`, `no media files to mux`, `create output dir`) and whether staging survived, which is the Retry-versus-Resume distinction: Retry deletes staging, Resume preserves it |
```

`SPEC.md:885` — N1 rewrites this sentence to the full key list; append the four:

```
**Notification events:** … `channel_unhealthy`, `channel_healthy`, `sidecar_down`, `sidecar_restored`, `disk_ok` — see `docs/spec/operations.md` for the per-event table; new events must be registered in both UI filter registries.
```

Read the merged sentence first and extend it; do not replace N1's list with a stale one.

- [ ] **Step 6: Confirm `docs/spec/user-interfaces.md` names no embed titles**

```bash
cd /d/Git/Moombox/.worktrees/webhooks-n2a-producers
grep -n 'Stream Found\|Video Added\|Job Added\|Download Finished\|Job Cancelled\|Download Cancelled\|Muxing Starting\|Trim Created' docs/spec/user-interfaces.md README.md
```

Expected: **no output.** Neither doc names an embed title, so the unified titles from Tasks 2–5 need no doc edit there. If either grep hits after the merge (N1 edits `README.md`), amend the hit in this task rather than leaving it stale.

- [ ] **Step 7: Run the gates**

```bash
cd /d/Git/Moombox/.worktrees/webhooks-n2a-producers
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go test -count=1 -timeout 300s -run 'TestNotificationVocabulariesAgree|TestRecoveryEventsAreKnownAndAliased' -v ./internal/tui/
go test -count=1 -timeout 300s ./internal/tui/ ./internal/notifications/ ./internal/web/routes/
go test -count=1 -timeout 300s ./internal/docs/
node --test --test-timeout=120000 web/tests/*.test.mjs
go vet ./internal/tui/ ./internal/notifications/
gofmt -l ./internal/tui ./internal/notifications
```

Expected: both parity tests PASS; three packages `ok`; `internal/docs` `ok` (the citation gate — `operations.md` and `SPEC.md` were edited); the node suite reports `fail 0` with the counts `web/tests/README.md` already claims (no JS test reads `NOTIFICATION_EVENT_GROUPS`, so the counts do not move — if they DO, regenerate `web/tests/README.md` from the live run and add it to this commit); vet silent; `gofmt -l` prints nothing.

- [ ] **Step 8: Commit**

```bash
cd /d/Git/Moombox/.worktrees/webhooks-n2a-producers
git add internal/notifications/events.go web/public/modules/settings.js \
        internal/tui/settings_notification_vocab_parity_test.go \
        docs/spec/operations.md SPEC.md
git commit -m "feat(notifications): vocabulary for the four recovery halves

Four alerts had no close. The sidecar could die and come back, the disk could
fill and be freed, disk monitoring could fail and recover, and a channel could
stop answering and start again — and in every case the operator who acted at
03:00 got no confirmation, while the 30-minute repeat on the disk warning made
the missing resolved line more visible, not less.

disk_ok, channel_healthy, sidecar_down and sidecar_restored join the System
group of the canonical vocabulary and the web UI's labelled mirror, each close
beside the alert it ends. disk_ok, channel_healthy and sidecar_restored are
aliased to the alerts they close, so a target that already filters the alert
receives the close with no config edit while a target that wants only the
all-clear can subscribe to it alone.

A parity test now pins the three registries against each other — the Go
vocabulary the manager filters on, the flat list the TUI derives, and the
labelled mirror the browser evaluates — group by group and position by
position. Nothing connected them before: whichever was edited alone kept
working perfectly while the other UI silently stopped offering the key.

The producers land in the two tasks that follow.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/notifications/events.go web/public/modules/settings.js internal/tui/settings_notification_vocab_parity_test.go docs/spec/operations.md SPEC.md
```

---

### Task 7: `sidecar_down` / `sidecar_restored` — the second health subscriber

`sidecar.SubscribeHealth` has exactly one subscriber today: the TUI status bar (`cmd/moombox/tui_wiring.go:1105-1107`, drawing "SIDECAR DOWN" via `tui.SidecarStatusMsg`). `/api/status` exposes the snapshot but nothing pushes it, so a headless or Docker install learns the sidecar died only from the log — and a dead sidecar means PO tokens fall back to the in-process goja solver, which is exactly the "unattended recorder hiding a problem" the crash notice exists for (audit A1). Per the §0 ruling the alert is debounced at 60 s: the supervisor's restart ladder handles everything shorter, and the webhook is for the failure it could not fix.

**Files:**
- Create: `cmd/moombox/sidecar_alerts.go`
- Create: `cmd/moombox/sidecar_alerts_test.go`
- Modify: `cmd/moombox/main.go` (beside `s.wireMonitorCallbacks()` at `:267` — N1 does not touch this file, so every `main.go` citation in Tasks 7 and 8 is still the pre-N1 number)

**Interfaces:**
- Consumes: `sidecar.Health{Healthy bool; Reason string; Restarts uint64; Since time.Time}` and `sidecar.SubscribeHealth(fn func(Health)) (unsubscribe func())` (`internal/bgutils/sidecar/health.go:18-29`, `:71-91`); `notifications.Sender`, `notificationtest.Recorder` (N1); `runState.notifyMgr`, `runState.log`.
- Produces, in package `main`:
  - `const sidecarDownDebounce = 60 * time.Second`
  - `type stoppableTimer interface{ Stop() bool }`
  - `type sidecarAlerts struct { … }` with `newSidecarAlerts`, `(*sidecarAlerts).onHealth(sidecar.Health)`, `(*sidecarAlerts).stop()`
  - `func (s *runState) wireSidecarAlerts() func()`

- [ ] **Step 1: Write the failing test**

Create `cmd/moombox/sidecar_alerts_test.go`:

```go
package main

import (
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/bgutils/sidecar"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
)

// fakeTimer is the injected clock. It records the delay it was asked for and
// fires only when the test says so, which is what makes "59 s" testable
// without waiting 59 seconds: the assertion is that nothing fired, and the
// delay the alerter ASKED for is asserted separately.
type fakeTimer struct {
	d       time.Duration
	f       func()
	stopped bool
}

func (t *fakeTimer) Stop() bool {
	was := t.stopped
	t.stopped = true
	return !was
}

func (t *fakeTimer) fire() {
	if !t.stopped {
		t.f()
	}
}

type fakeClock struct{ timers []*fakeTimer }

func (c *fakeClock) after(d time.Duration, f func()) stoppableTimer {
	t := &fakeTimer{d: d, f: f}
	c.timers = append(c.timers, t)
	return t
}

func (c *fakeClock) last(t *testing.T) *fakeTimer {
	t.Helper()
	if len(c.timers) == 0 {
		t.Fatal("no timer was scheduled — the alerter fired (or did nothing) instead of debouncing")
	}
	return c.timers[len(c.timers)-1]
}

func newTestSidecarAlerts(t *testing.T) (*sidecarAlerts, *notificationtest.Recorder, *fakeClock) {
	t.Helper()
	rec := notificationtest.New()
	clk := &fakeClock{}
	a := newSidecarAlerts(rec, &nopLogger{}, clk.after)
	return a, rec, clk
}

// TestSidecarDownIsDebounced is the §0 ruling: the supervisor's restart ladder
// handles the quick cases, so the webhook waits for the one it cannot fix.
//
// Mutants this kill:
//   - sending on the transition instead of after the window: every supervisor
//     restart pages the operator.
//   - scheduling the wrong delay (the assertion on d).
func TestSidecarDownIsDebounced(t *testing.T) {
	a, rec, clk := newTestSidecarAlerts(t)

	a.onHealth(sidecar.Health{Healthy: false, Reason: "stdout EOF", Restarts: 3})
	if got := len(rec.Calls()); got != 0 {
		t.Fatalf("recorded %d calls the instant the sidecar went down, want 0", got)
	}
	if d := clk.last(t).d; d != sidecarDownDebounce {
		t.Errorf("debounce = %v, want %v", d, sidecarDownDebounce)
	}

	clk.last(t).fire()
	calls := rec.ByEvent("sidecar_down")
	if len(calls) != 1 {
		t.Fatalf("recorded %d sidecar_down calls after the window, want 1", len(calls))
	}
	if calls[0].Type != notifications.TypeError {
		t.Errorf("type = %v, want TypeError", calls[0].Type)
	}
	var sawReason, sawRestarts bool
	for _, f := range calls[0].Fields {
		switch f.Name {
		case "Reason":
			sawReason = f.Value == "stdout EOF"
		case "Restarts":
			sawRestarts = f.Value == "3"
		}
	}
	if !sawReason || !sawRestarts {
		t.Errorf("fields = %+v, want the reason the child died and the restart count", calls[0].Fields)
	}
}

// TestSidecarFlapUnderTheWindowIsSilent is the case the debounce exists for:
// the supervisor restarted the child and nobody needed to know.
//
// Mutant: not stopping the timer on the healthy edge — the alert fires for a
// sidecar that is already back.
func TestSidecarFlapUnderTheWindowIsSilent(t *testing.T) {
	a, rec, clk := newTestSidecarAlerts(t)

	a.onHealth(sidecar.Health{Healthy: false, Reason: "stdout EOF"})
	a.onHealth(sidecar.Health{Healthy: true, Restarts: 1})
	clk.last(t).fire() // the supervisor's own timer, now cancelled

	if got := len(rec.Calls()); got != 0 {
		t.Fatalf("recorded %d calls for a flap inside the window, want 0: %+v", got, rec.Calls())
	}
}

// TestSidecarRestoredOnlyClosesAnAlertThatWasSent.
//
// Mutants this kill:
//   - sending the all-clear on every healthy edge: a healthy boot, and every
//     supervisor restart, would announce a recovery from nothing.
//   - failing to clear the sent flag: the second outage never alerts.
func TestSidecarRestoredOnlyClosesAnAlertThatWasSent(t *testing.T) {
	a, rec, clk := newTestSidecarAlerts(t)

	// A healthy first snapshot says nothing.
	a.onHealth(sidecar.Health{Healthy: true})
	if got := len(rec.Calls()); got != 0 {
		t.Fatalf("a healthy snapshot recorded %d calls, want 0", got)
	}

	a.onHealth(sidecar.Health{Healthy: false, Reason: "readPump panic"})
	clk.last(t).fire()
	rec.Reset()

	a.onHealth(sidecar.Health{Healthy: true, Restarts: 4})
	calls := rec.ByEvent("sidecar_restored")
	if len(calls) != 1 {
		t.Fatalf("recorded %d sidecar_restored calls, want 1", len(calls))
	}
	if calls[0].Type != notifications.TypeSuccess {
		t.Errorf("type = %v, want TypeSuccess", calls[0].Type)
	}

	// A second healthy snapshot is not a second recovery.
	rec.Reset()
	a.onHealth(sidecar.Health{Healthy: true, Restarts: 4})
	if got := len(rec.Calls()); got != 0 {
		t.Fatalf("a repeated healthy snapshot recorded %d calls, want 0", got)
	}

	// And a second outage still alerts.
	a.onHealth(sidecar.Health{Healthy: false, Reason: "failed initial start"})
	clk.last(t).fire()
	if got := len(rec.ByEvent("sidecar_down")); got != 1 {
		t.Fatalf("the second outage recorded %d sidecar_down calls, want 1", got)
	}
}
```

Add `"github.com/vampiricwulf/Moombox/internal/notifications"` to the imports. The logger is the package's existing `nopLogger` (`cmd/moombox/helpers.go:89-95`) — and it must be `&nopLogger{}`, not `nopLogger{}`: its four methods have POINTER receivers, so the value type does not satisfy the logger interface.

- [ ] **Step 2: Run the test to verify it fails**

```bash
cd /d/Git/Moombox/.worktrees/webhooks-n2a-producers
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -timeout 300s -run 'TestSidecar' ./cmd/moombox/
```

Expected: FAIL to build — `undefined: sidecarAlerts`, `undefined: newSidecarAlerts`, `undefined: stoppableTimer`, `undefined: sidecarDownDebounce`.

- [ ] **Step 3: Create `cmd/moombox/sidecar_alerts.go`**

```go
package main

import (
	"fmt"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/bgutils/sidecar"
	"github.com/vampiricwulf/Moombox/internal/notifications"
)

// sidecarDownDebounce is how long the BotGuard sidecar must stay unhealthy
// before the operator hears about it.
//
// The supervisor's restart ladder handles everything shorter — a child that
// dies and is replaced inside a minute cost nothing anyone needs to act on —
// so the webhook is for the failure the supervisor could NOT fix (§0 ruling).
// The cost is a 60-second-later alert, which is nothing next to the alternative
// of one page per restart.
const sidecarDownDebounce = 60 * time.Second

// stoppableTimer is the slice of *time.Timer this file uses. It is an
// interface so a test can supply a clock it controls: the 59-second case
// cannot be asserted by waiting.
type stoppableTimer interface{ Stop() bool }

// sidecarAlerts turns sidecar health transitions into the sidecar_down /
// sidecar_restored pair.
//
// It is the SECOND subscriber on sidecar.SubscribeHealth; the first is the TUI
// status bar (tui_wiring.go), which is why a headless or Docker install learnt
// about a dead sidecar only from the log (audit A1). Unlike the TUI's, this
// one must run whether or not a TUI exists.
type sidecarAlerts struct {
	notify notifications.Sender
	log    interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
	after func(time.Duration, func()) stoppableTimer

	mu sync.Mutex
	// timer is the armed debounce, nil when none is pending.
	timer stoppableTimer
	// epoch invalidates a timer that already escaped Stop. time.AfterFunc can
	// have entered its callback before Stop is called, and the callback then
	// cannot tell "I am the current outage" from "I am a cancelled one" — the
	// epoch it captured can.
	epoch uint64
	// downSent gates the all-clear: a recovery is only news if the outage was
	// reported. Without it every healthy boot and every supervisor restart
	// would announce a recovery from nothing.
	downSent bool
	// last is the most recent snapshot, read by the debounce when it fires so
	// the embed names the CURRENT reason and restart count.
	last sidecar.Health
}

func newSidecarAlerts(notify notifications.Sender, log interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}, after func(time.Duration, func()) stoppableTimer) *sidecarAlerts {
	return &sidecarAlerts{notify: notify, log: log, after: after}
}

// onHealth is the subscriber. It runs on the PUBLISHER's goroutine (the
// supervisor loop, or startup) and must not block, which is why the only work
// it does is arm or cancel a timer and hand a send to the notification
// manager, whose Send queues and returns.
func (a *sidecarAlerts) onHealth(h sidecar.Health) {
	a.mu.Lock()
	a.last = h

	if !h.Healthy {
		if a.timer == nil && !a.downSent {
			a.epoch++
			ep := a.epoch
			a.timer = a.after(sidecarDownDebounce, func() { a.fireDown(ep) })
		}
		a.mu.Unlock()
		return
	}

	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
		a.epoch++
	}
	if !a.downSent {
		a.mu.Unlock()
		return
	}
	a.downSent = false
	restarts := h.Restarts
	a.mu.Unlock()

	a.log.Info("BotGuard sidecar healthy again", "restarts", restarts)
	a.notify.Send("BotGuard Sidecar Restored",
		"The BotGuard sidecar is answering again — PO token minting is back on the V8 child",
		notifications.TypeSuccess,
		[]notifications.Field{
			{Name: "Restarts", Value: fmt.Sprintf("%d", restarts), Inline: true},
		},
		notifications.SendOptions{Event: "sidecar_restored"},
	)
}

// fireDown runs on the timer's own goroutine, so it carries its own recover.
func (a *sidecarAlerts) fireDown(epoch uint64) {
	defer func() {
		if r := recover(); r != nil {
			a.log.Error("panic in the sidecar down alert", "panic", fmt.Sprint(r))
		}
	}()

	a.mu.Lock()
	if epoch != a.epoch || a.timer == nil {
		// The sidecar came back while this callback was in flight.
		a.mu.Unlock()
		return
	}
	a.timer = nil
	a.downSent = true
	h := a.last
	a.mu.Unlock()

	reason := h.Reason
	if reason == "" {
		reason = "unknown"
	}
	a.notify.Send("BotGuard Sidecar Down",
		fmt.Sprintf("The BotGuard sidecar has been unhealthy for over %s — PO tokens are falling back to the slower in-process solver until it returns",
			sidecarDownDebounce),
		notifications.TypeError,
		[]notifications.Field{
			{Name: "Reason", Value: notifications.EscapeMarkdown(reason)},
			{Name: "Restarts", Value: fmt.Sprintf("%d", h.Restarts), Inline: true},
		},
		notifications.SendOptions{Event: "sidecar_down"},
	)
}

// stop cancels a pending debounce. Called from the unsubscribe wireSidecarAlerts
// returns, so a shutdown cannot fire an alert on the way out.
func (a *sidecarAlerts) stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
		a.epoch++
	}
}

// wireSidecarAlerts subscribes the alerter and returns its teardown.
//
// SubscribeHealth calls back IMMEDIATELY with the current snapshot when one
// exists, which is deliberate here: initServices publishes a failed first start
// long before this runs, and without the immediate call the dominant failure —
// a sidecar that cannot start at all — would arm no debounce, because the only
// later publish is the Healthy:true of a restart that never comes.
//
// A process with `[bgutils] use_sidecar = false` published nothing, so nothing
// is delivered and no alert is ever armed, which is right.
func (s *runState) wireSidecarAlerts() func() {
	a := newSidecarAlerts(s.notifyMgr, s.log, func(d time.Duration, f func()) stoppableTimer {
		return time.AfterFunc(d, f)
	})
	unsub := sidecar.SubscribeHealth(a.onHealth)
	return func() {
		unsub()
		a.stop()
	}
}
```

- [ ] **Step 4: Wire it in `cmd/moombox/main.go`**

Immediately after `s.wireMonitorCallbacks()` (`:267`):

```go
	// BotGuard sidecar liveness -> webhooks. The TUI's own subscriber
	// (tui_wiring.go) draws the status bar; this one is the headless half,
	// and it runs whether or not a TUI does.
	unsubSidecarAlerts := s.wireSidecarAlerts()
	defer unsubSidecarAlerts()
```

`s.notifyMgr` must satisfy `notifications.Sender` here. If N1 left it as `*notifications.Manager`, that is already true. If N1 narrowed it to `Sender`, that is also true. No change either way.

- [ ] **Step 5: Run the tests to verify they pass**

```bash
cd /d/Git/Moombox/.worktrees/webhooks-n2a-producers
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go test -count=1 -timeout 300s -run 'TestSidecar' -v ./cmd/moombox/
go test -count=1 -timeout 300s ./cmd/moombox/
go test -count=1 -timeout 300s -race -run 'TestSidecar' ./cmd/moombox/
go vet ./cmd/moombox/
gofmt -l ./cmd/moombox
```

Expected: the three new tests PASS; the package is `ok`; the race run is clean (the alerter is read from the publisher's goroutine and written from a timer's); vet silent; `gofmt -l` prints nothing.

- [ ] **Step 6: Commit**

```bash
cd /d/Git/Moombox/.worktrees/webhooks-n2a-producers
git add cmd/moombox/sidecar_alerts.go cmd/moombox/sidecar_alerts_test.go cmd/moombox/main.go
git commit -m "feat(cmd): alert when the BotGuard sidecar stays down, and when it returns

sidecar.SubscribeHealth had exactly one subscriber, the TUI status bar, so a
headless or Docker install learnt that the sidecar had died only from the log —
and a dead sidecar means PO tokens fall back to the slower in-process solver
for as long as it stays dead. /api/status exposed the snapshot and nothing
pushed it anywhere.

sidecarAlerts is the second subscriber. It waits a continuous 60 seconds before
alerting, because the supervisor's restart ladder handles everything shorter and
a page per restart is worse than an alert a minute late; the embed names the
reason the child died and how many successful restarts this process has made.
The all-clear fires only when an alert was actually sent, so a healthy boot and
an ordinary restart stay silent.

The debounce sits behind an injected timer, which is what makes the case the
feature exists for — 59 seconds of unhealthy, then recovery — assertable
without waiting a minute for it.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- cmd/moombox/sidecar_alerts.go cmd/moombox/sidecar_alerts_test.go cmd/moombox/main.go
```

---

### Task 8: `disk_ok`, "Disk Monitoring Recovered", and `channel_healthy`

Three more incidents with no end. `main.go:818` resets `lastDiskLevel` to ok silently after a warning or critical was sent; `main.go:761` clears `diskReadFailing` silently after a monitoring-failure alert; and `healthTracker.recordSuccess` (`internal/monitor/health.go:57-65`) resets `notified` with no callback at all, so "Channel Not Responding" tells an operator to go check a rename or a ban and nothing ever tells them to stop (audit A2, A3).

The disk decision lives inside a 190-line closure in the stats ticker and is untestable where it is; this task lifts it out unchanged and then adds the two closes.

**Files:**
- Create: `cmd/moombox/disk_alerts.go`
- Create: `cmd/moombox/disk_alerts_test.go`
- Create: `cmd/moombox/channel_health_test.go`
- Modify: `cmd/moombox/main.go` (the stats-ticker state at `:666-675`; the disk block at `:752-846`)
- Modify: `cmd/moombox/monitor_callbacks.go` (`unhealthyNotify` at `:1631-1656`; the three `SetOnChannelUnhealthy` wirings at `:1660-1662`) — post-N1 numbers
- Modify: `internal/monitor/health.go` (`healthTracker` at `:35-41`; `recordSuccess` at `:57-65`) — `internal/monitor` is untouched by N1
- Modify: `internal/monitor/feed.go` (`:278-282`), `internal/monitor/decapi.go` (`:185-189`), `internal/monitor/twitch.go` (`:79-83`)
- Modify: `internal/monitor/health_test.go` — **APPEND to the existing 94-line file**; it already holds `TestHealthTrackerStreakAndReset`, `TestHealthTrackerFiresOnceAtThreshold`, `TestHealthTrackerPrune` and `findHealth`, and its import block already has `errors` and `testing`. Writing it fresh destroys all four.

**Interfaces:**
- Consumes: `routes.DiskStatus{Free, Total uint64; UsedPct float64; WarnLevel string}` (`internal/web/routes/stats.go:18-23`); `channelHealthReporter` and `siblingReachable(siblings []channelHealthReporter, channelID string, now time.Time) bool` (`cmd/moombox/monitor_callbacks.go:492-494`, `:506`); `notifications.Sender`, `notificationtest.Recorder`.
- Produces:
  - `internal/monitor`: `healthTracker.onHealthy func(channelID string)`; `(*FeedMonitor).SetOnChannelHealthy`, `(*DecapiMonitor).SetOnChannelHealthy`, `(*TwitchMonitor).SetOnChannelHealthy`, each `func(fn func(channelID string))`
  - `cmd/moombox`: `type diskAlerts struct{…}` with `newDiskAlerts`, `onReading(ds *routes.DiskStatus, outputDir string, now time.Time)`, `onReadFailure(outputDir string)`; `func channelHealthNotifiers(n notifications.Sender, log …, platform string, siblings ...channelHealthReporter) (func(string, int, string), func(string))`

- [ ] **Step 1: Write the failing tests**

Append to `internal/monitor/health_test.go` (do NOT overwrite it — the file exists and holds three tests plus `findHealth`; both imports below are already in its block, so add the two functions alone):

```go
// TestRecordSuccessFiresOnHealthyOnlyAfterAStreakWasNotified is audit A3's
// tracker half: recordSuccess cleared `notified` with no callback, so the
// unhealthy alert had no pair.
//
// Mutants this kill:
//   - firing on every success: a healthy channel would publish a recovery on
//     every poll, forever.
//   - firing without clearing `notified`: the next success fires a second
//     recovery for the same streak.
func TestRecordSuccessFiresOnHealthyOnlyAfterAStreakWasNotified(t *testing.T) {
	h := newHealthTracker()
	var healthy []string
	h.onHealthy = func(id string) { healthy = append(healthy, id) }

	// A success with no streak behind it says nothing.
	h.recordSuccess("ch1")
	if len(healthy) != 0 {
		t.Fatalf("onHealthy fired %d times with no prior streak, want 0", len(healthy))
	}

	for i := 0; i < unhealthyThreshold; i++ {
		h.recordError("ch1", errors.New("boom"))
	}
	h.recordSuccess("ch1")
	if len(healthy) != 1 || healthy[0] != "ch1" {
		t.Fatalf("onHealthy = %v, want exactly one call for ch1", healthy)
	}

	h.recordSuccess("ch1")
	if len(healthy) != 1 {
		t.Fatalf("onHealthy fired %d times, want 1 — the streak was already closed", len(healthy))
	}
}

// TestRecordSuccessBelowTheThresholdIsSilent: a short failure run never
// alerted, so it has nothing to close.
func TestRecordSuccessBelowTheThresholdIsSilent(t *testing.T) {
	h := newHealthTracker()
	fired := 0
	h.onHealthy = func(string) { fired++ }

	for i := 0; i < unhealthyThreshold-1; i++ {
		h.recordError("ch1", errors.New("blip"))
	}
	h.recordSuccess("ch1")
	if fired != 0 {
		t.Errorf("onHealthy fired %d times for a streak that never alerted, want 0", fired)
	}
}
```

Create `cmd/moombox/disk_alerts_test.go`:

```go
package main

import (
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
	"github.com/vampiricwulf/Moombox/internal/web/routes"
)

func diskReading(level string, usedPct float64) *routes.DiskStatus {
	return &routes.DiskStatus{Free: 40 << 30, Total: 1000 << 30, UsedPct: usedPct, WarnLevel: level}
}

// TestDiskAllClearClosesAWarningThatWasSent is audit A2. Every warning was an
// open incident with no end, and the 30-minute repeat made the missing
// resolved line more visible, not less.
//
// Mutants this kill:
//   - sending the all-clear unconditionally: a healthy install announces a
//     recovery on its first reading, and again after every ok reading.
//   - not clearing lastLevel: the second warning episode never closes.
func TestDiskAllClearClosesAWarningThatWasSent(t *testing.T) {
	rec := notificationtest.New()
	d := newDiskAlerts(rec, nopLoggerForTest())
	now := time.Now()

	// An ok reading with no warning behind it says nothing.
	d.onReading(diskReading("ok", 40), "./output", now)
	if got := len(rec.Calls()); got != 0 {
		t.Fatalf("an ok reading recorded %d calls, want 0", got)
	}

	d.onReading(diskReading("warn", 91), "./output", now)
	if got := len(rec.ByEvent("disk_warning")); got != 1 {
		t.Fatalf("recorded %d disk_warning calls, want 1", got)
	}
	rec.Reset()

	d.onReading(diskReading("ok", 40), "./output", now.Add(time.Minute))
	calls := rec.ByEvent("disk_ok")
	if len(calls) != 1 {
		t.Fatalf("recorded %d disk_ok calls, want 1", len(calls))
	}
	if calls[0].Type != notifications.TypeSuccess {
		t.Errorf("type = %v, want TypeSuccess", calls[0].Type)
	}

	rec.Reset()
	d.onReading(diskReading("ok", 40), "./output", now.Add(2*time.Minute))
	if got := len(rec.Calls()); got != 0 {
		t.Fatalf("a second ok reading recorded %d calls, want 0", got)
	}
}

// TestDiskWarningCooldownIsUnchanged guards the behaviour this task LIFTS but
// must not alter: a level change sends immediately, the same level waits 30
// minutes.
func TestDiskWarningCooldownIsUnchanged(t *testing.T) {
	rec := notificationtest.New()
	d := newDiskAlerts(rec, nopLoggerForTest())
	now := time.Now()

	d.onReading(diskReading("warn", 91), "./output", now)
	d.onReading(diskReading("warn", 92), "./output", now.Add(5*time.Minute))
	if got := len(rec.Calls()); got != 1 {
		t.Fatalf("recorded %d calls inside the cooldown, want 1", got)
	}
	d.onReading(diskReading("critical", 97), "./output", now.Add(6*time.Minute))
	if got := len(rec.ByEvent("disk_critical")); got != 1 {
		t.Fatalf("a level change recorded %d disk_critical calls, want 1 — it must not wait out the cooldown", got)
	}
	d.onReading(diskReading("critical", 98), "./output", now.Add(40*time.Minute))
	if got := len(rec.ByEvent("disk_critical")); got != 2 {
		t.Fatalf("recorded %d disk_critical calls after the cooldown expired, want 2", got)
	}
}

// TestDiskMonitoringRecoveredClosesAReadFailure.
//
// Mutants this kill:
//   - announcing a recovery after the FIRST failed read, which never alerted
//     (the alert is on the second, ~12 minutes in).
//   - not resetting the counter: the next failure streak never alerts.
func TestDiskMonitoringRecoveredClosesAReadFailure(t *testing.T) {
	rec := notificationtest.New()
	d := newDiskAlerts(rec, nopLoggerForTest())
	now := time.Now()

	d.onReadFailure("./output")
	d.onReading(diskReading("ok", 40), "./output", now)
	if got := len(rec.Calls()); got != 0 {
		t.Fatalf("one failed read then a recovery recorded %d calls, want 0 — nothing had been reported", got)
	}

	d.onReadFailure("./output")
	d.onReadFailure("./output")
	if got := len(rec.Calls()); got != 1 {
		t.Fatalf("recorded %d calls for a two-read failure streak, want 1", got)
	}
	rec.Reset()

	d.onReading(diskReading("ok", 40), "./output", now.Add(time.Minute))
	calls := rec.ByEvent("disk_ok")
	if len(calls) != 1 {
		t.Fatalf("recorded %d disk_ok calls after monitoring recovered, want 1", len(calls))
	}
	if calls[0].Title != "Disk Monitoring Recovered" {
		t.Errorf("title = %q", calls[0].Title)
	}
}
```

`nopLoggerForTest()` returns the existing `&nopLogger{}` (`cmd/moombox/helpers.go:89-95`) — the POINTER, because `nopLogger`'s four methods have pointer receivers; inline `&nopLogger{}` rather than adding a helper if that reads better.

Create `cmd/moombox/channel_health_test.go`:

```go
package main

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
)

// TestChannelHealthNotifiersPairAnAlertWithItsClose is audit A3's cmd half.
//
// Mutants this kill:
//   - closing a streak whose alert was SUPPRESSED by the cross-monitor
//     confirmation: the operator gets "Channel Recovered" for a channel they
//     were never told about. The tracker's own `notified` flag cannot see the
//     suppression — it is applied here, one layer up — which is why the sent
//     set lives in this closure and not in internal/monitor.
//   - firing the close for a channel that never alerted at all.
func TestChannelHealthNotifiersPairAnAlertWithItsClose(t *testing.T) {
	t.Run("alert then close", func(t *testing.T) {
		rec := notificationtest.New()
		unhealthy, healthy := channelHealthNotifiers(rec, &nopLogger{}, "youtube")

		unhealthy("UC_dead", 20, "404")
		if got := len(rec.ByEvent("channel_unhealthy")); got != 1 {
			t.Fatalf("recorded %d channel_unhealthy calls, want 1", got)
		}
		rec.Reset()

		healthy("UC_dead")
		calls := rec.ByEvent("channel_healthy")
		if len(calls) != 1 {
			t.Fatalf("recorded %d channel_healthy calls, want 1", len(calls))
		}
		if calls[0].Type != notifications.TypeSuccess {
			t.Errorf("type = %v, want TypeSuccess", calls[0].Type)
		}

		rec.Reset()
		healthy("UC_dead")
		if got := len(rec.Calls()); got != 0 {
			t.Fatalf("a second recovery recorded %d calls, want 0", got)
		}
	})

	t.Run("a suppressed alert has no close", func(t *testing.T) {
		rec := notificationtest.New()
		// A sibling that vouches for the channel suppresses the alert.
		unhealthy, healthy := channelHealthNotifiers(rec, &nopLogger{}, "youtube", freshSibling("UC_ok"))

		unhealthy("UC_ok", 20, "404")
		if got := len(rec.Calls()); got != 0 {
			t.Fatalf("a suppressed streak recorded %d calls, want 0", got)
		}
		healthy("UC_ok")
		if got := len(rec.Calls()); got != 0 {
			t.Fatalf("the close of a suppressed streak recorded %d calls, want 0", got)
		}
	})

	t.Run("a recovery with no alert says nothing", func(t *testing.T) {
		rec := notificationtest.New()
		_, healthy := channelHealthNotifiers(rec, &nopLogger{}, "twitch")
		healthy("streamer")
		if got := len(rec.Calls()); got != 0 {
			t.Fatalf("recorded %d calls, want 0", got)
		}
	})
}
```

There is no `freshSibling` today — `cmd/moombox/monitor_callbacks_test.go:11-13` has the `fakeHealthReporter` type and `TestSiblingReachable` builds its reporters with a closure local to the test (`:31`). Add the constructor to `channel_health_test.go`, and give it a **fresh** timestamp: `siblingReachable` vouches only on `ConsecutiveErrors == 0 && LastCheckedAt != 0 && now - LastCheckedAt <= crossMonitorVouchWindow` (20 min, `cmd/moombox/monitor_callbacks.go:29`, condition at `:514-517`), so a zero or stale one makes the "suppressed alert has no close" subtest pass for the wrong reason.

```go
func freshSibling(channelID string) channelHealthReporter {
	return fakeHealthReporter{health: []monitor.ChannelHealth{{
		ChannelID:         channelID,
		LastCheckedAt:     time.Now().UnixMilli(),
		ConsecutiveErrors: 0,
	}}}
}
```

Add `time` and `internal/monitor` to the test's imports, and match `fakeHealthReporter`'s real field name before writing it.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
cd /d/Git/Moombox/.worktrees/webhooks-n2a-producers
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go test -count=1 -timeout 300s -run 'TestRecordSuccess' ./internal/monitor/
go test -count=1 -timeout 300s -run 'TestDisk|TestChannelHealthNotifiers' ./cmd/moombox/
```

Expected: `internal/monitor` FAILS with `h.onHealthy undefined (type *healthTracker has no field or method onHealthy)`; `cmd/moombox` FAILS to build with `undefined: newDiskAlerts` and `undefined: channelHealthNotifiers`.

- [ ] **Step 3: Add `onHealthy` to `internal/monitor/health.go`**

On `healthTracker` (`:35-41`), beside `onUnhealthy`:

```go
	// onHealthy fires ONCE when a channel that crossed the threshold answers
	// a check again. It is the pair of onUnhealthy: the unhealthy alert tells
	// an operator to go and check a rename, a ban or a typo, and without this
	// nothing ever tells them to stop. Set by the wiring layer; nil = track
	// only.
	//
	// It fires on the tracker's `notified` flag, which says an alert was
	// RAISED here. Whether that alert was actually delivered is a question one
	// layer up — cmd/moombox suppresses an alert a sibling monitor
	// contradicts — so the wiring, not this tracker, decides whether the close
	// is worth sending.
	onHealthy func(channelID string)
```

`recordSuccess` (`:56-65`) gains the edge. It cannot keep `defer h.mu.Unlock()`, because the callback must run OFF the lock — `recordError` already fires its callback off the lock for the same reason:

```go
// recordSuccess clears a channel's failure streak, and fires onHealthy once
// when the streak it clears had crossed the threshold.
func (h *healthTracker) recordSuccess(id string) {
	h.mu.Lock()
	s := h.state(id)
	s.lastCheckedAt = time.Now()
	s.lastError = ""
	s.consecutiveErrors = 0
	fire := s.notified
	s.notified = false
	cb := h.onHealthy
	h.mu.Unlock()

	if fire && cb != nil {
		cb(id)
	}
}
```

Add the setter to all three monitors, immediately after each `SetOnChannelUnhealthy` (`feed.go:278-282`, `decapi.go:185-189`, `twitch.go:79-83`):

```go
// SetOnChannelHealthy installs the callback fired once when a channel that
// crossed the threshold answers a check again.
func (fm *FeedMonitor) SetOnChannelHealthy(fn func(channelID string)) {
	fm.health.onHealthy = fn
}
```

(and the `dm`/`tm` equivalents).

- [ ] **Step 4: Replace `unhealthyNotify` with `channelHealthNotifiers` in `cmd/moombox/monitor_callbacks.go`**

The closure at `:1631-1656` becomes a package-level function — a test cannot reach a closure inside `wireMonitorCallbacks`, which is why the send has never been asserted (only `siblingReachable` has):

```go
// channelHealthNotifiers returns the unhealthy/healthy callback pair for one
// monitor: the alert and its close.
//
// Package-level, like withAuthFailureCooldown, because the decision it makes
// needs a test and wireMonitorCallbacks' closure cannot be reached from one.
//
// The `sent` set is the whole reason the close is decided HERE rather than in
// internal/monitor's tracker. The tracker knows a streak crossed the threshold;
// it does not know that the alert was suppressed because a sibling monitor
// still reaches the channel. Closing a suppressed streak would announce a
// recovery from an incident the operator was never told about.
func channelHealthNotifiers(
	n notifications.Sender,
	log interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	},
	platform string,
	siblings ...channelHealthReporter,
) (func(channelID string, consecutive int, lastErr string), func(channelID string)) {
	var mu sync.Mutex
	sent := map[string]bool{}

	unhealthy := func(channelID string, consecutive int, lastErr string) {
		// Cross-monitor confirmation: a channel is only "not responding" if
		// EVERY monitor covering it has lost it. YouTube serves RSS 404/5xx
		// during peak hours while the independent DECAPI monitor keeps
		// working, so a lone feed-monitor failure is a false positive — its
		// streams are still being seen. Suppress unless no sibling vouches.
		if siblingReachable(siblings, channelID, time.Now()) {
			log.Info("channel unhealthy on one monitor but still reachable via another — suppressing alert",
				"platform", platform, "channel", channelID, "consecutive", consecutive, "err", lastErr)
			return
		}
		log.Warn("channel failing monitor checks — verify it still exists",
			"platform", platform, "channel", channelID, "consecutive", consecutive, "err", lastErr)
		mu.Lock()
		sent[channelID] = true
		mu.Unlock()
		n.Send("Channel Not Responding",
			fmt.Sprintf("A %s channel has failed %d consecutive monitor checks — it may be renamed, banned, or misconfigured, and its streams are being missed", platform, consecutive),
			notifications.TypeWarning,
			[]notifications.Field{
				{Name: "Channel", Value: channelID, Inline: true},
				{Name: "Platform", Value: platform, Inline: true},
				{Name: "Last Error", Value: notifications.EscapeMarkdown(lastErr)},
			},
			notifications.SendOptions{Event: "channel_unhealthy"},
		)
	}

	healthy := func(channelID string) {
		mu.Lock()
		fire := sent[channelID]
		delete(sent, channelID)
		mu.Unlock()
		if !fire {
			return
		}
		log.Info("channel responding again", "platform", platform, "channel", channelID)
		n.Send("Channel Responding Again",
			fmt.Sprintf("A %s channel that stopped answering monitor checks is being reached again", platform),
			notifications.TypeSuccess,
			[]notifications.Field{
				{Name: "Channel", Value: channelID, Inline: true},
				{Name: "Platform", Value: platform, Inline: true},
			},
			notifications.SendOptions{Event: "channel_healthy"},
		)
	}

	return unhealthy, healthy
}
```

The wiring at `:1660-1662` becomes:

```go
	// YouTube channels are covered by both the RSS feed and DECAPI monitors, so
	// each cross-confirms against the other before alerting. Twitch has a single
	// (reliable GQL) monitor with no sibling to confirm against.
	feedUnhealthy, feedHealthy := channelHealthNotifiers(s.notifyMgr, s.log, "youtube", s.decapiMon)
	s.feedMon.SetOnChannelUnhealthy(feedUnhealthy)
	s.feedMon.SetOnChannelHealthy(feedHealthy)

	decapiUnhealthy, decapiHealthy := channelHealthNotifiers(s.notifyMgr, s.log, "youtube", s.feedMon)
	s.decapiMon.SetOnChannelUnhealthy(decapiUnhealthy)
	s.decapiMon.SetOnChannelHealthy(decapiHealthy)

	twitchUnhealthy, twitchHealthy := channelHealthNotifiers(s.notifyMgr, s.log, "twitch")
	s.twitchMon.SetOnChannelUnhealthy(twitchUnhealthy)
	s.twitchMon.SetOnChannelHealthy(twitchHealthy)
```

Each monitor gets its OWN pair, so each keeps its own `sent` set — which is right: the feed monitor losing a channel and DECAPI losing it are separate incidents with separate closes, exactly as the two alerts are separate today.

- [ ] **Step 5: Create `cmd/moombox/disk_alerts.go`**

```go
package main

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/web/routes"
)

// diskNotifyCooldown is how long the same warning LEVEL waits before repeating.
// A level change (warn -> critical) never waits it out.
const diskNotifyCooldown = 30 * time.Minute

// diskReadFailuresBeforeAlert is how many consecutive failed readings the
// low-disk safety net must miss before the operator hears about it. The second
// failure is roughly twelve minutes in, which rides out a transient SMB blip or
// a flapping network volume without hiding a volume that has actually gone.
const diskReadFailuresBeforeAlert = 2

// diskAlerts owns the disk notification decision.
//
// It is a struct rather than four locals inside the stats ticker's closure so
// the decision can be tested: the all-clear this type adds is unreachable from
// any test while it lives inside a 190-line goroutine that also reads runtime
// memory stats and talks to the sidecar. Everything about the WARNING path is
// the behaviour that closure already had, moved.
//
// Not goroutine-safe: the stats ticker is its only caller and it is a single
// goroutine.
type diskAlerts struct {
	notify notifications.Sender
	log    interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}

	// lastNotify and lastLevel implement the repeat cooldown. lastLevel is
	// non-empty exactly when a warning or critical alert HAS been sent and not
	// yet closed, which is what makes the all-clear conditional.
	lastNotify time.Time
	lastLevel  string

	// readFailing/readFailCount track the monitoring-failure streak, and
	// readFailNotified says whether that streak was reported — the first
	// failure never is.
	readFailing      bool
	readFailCount    int
	readFailNotified bool
}

func newDiskAlerts(notify notifications.Sender, log interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *diskAlerts {
	return &diskAlerts{notify: notify, log: log}
}

// absOutputDir names the directory an operator can act on. An operator with
// several machines (or several volumes) cannot act on "output drive" alone,
// and the config default is the relative "./output".
func absOutputDir(outputDir string) string {
	if abs, err := filepath.Abs(outputDir); err == nil {
		return abs
	}
	return outputDir
}

// onReading feeds one successful disk reading in.
//
// A reading can close TWO incidents at once: monitoring that had been reported
// as failing, and a space warning that was open before it failed. Both closes
// are sent, in that order — they are separate incidents with separate alerts,
// and collapsing them would leave one of the two alerts hanging.
func (d *diskAlerts) onReading(ds *routes.DiskStatus, outputDir string, now time.Time) {
	if d.readFailing {
		d.readFailing = false
		d.readFailCount = 0
		if d.readFailNotified {
			d.readFailNotified = false
			d.notify.Send("Disk Monitoring Recovered",
				"Disk space checks are answering again — the gauge and the low-disk alerts are live",
				notifications.TypeSuccess,
				[]notifications.Field{{Name: "Output Directory", Value: absOutputDir(outputDir)}},
				notifications.SendOptions{Event: "disk_ok"},
			)
		}
	}

	if ds.WarnLevel != "ok" {
		if d.lastLevel == ds.WarnLevel && now.Sub(d.lastNotify) < diskNotifyCooldown {
			return
		}
		freeGB := float64(ds.Free) / (1024 * 1024 * 1024)
		level, ntype, event := "Warning", notifications.TypeWarning, "disk_warning"
		if ds.WarnLevel == "critical" {
			// Critical gets its own event so targets can route it separately
			// (e.g. a high-priority channel); eventAliases keeps plain
			// "disk_warning" filters receiving it too.
			level, ntype, event = "Critical", notifications.TypeError, "disk_critical"
		}
		d.notify.Send(
			fmt.Sprintf("Disk Space %s", level),
			fmt.Sprintf("%.1f%% used — %.1f GB free on output drive", ds.UsedPct, freeGB),
			ntype,
			[]notifications.Field{{Name: "Output Directory", Value: absOutputDir(outputDir)}},
			notifications.SendOptions{Event: event},
		)
		d.lastNotify = now
		d.lastLevel = ds.WarnLevel
		return
	}

	// Back to ok. Every warning and critical was an open incident with no end,
	// and the 30-minute repeat made the missing resolved line more visible,
	// not less (audit A2). lastLevel is non-empty only when an alert was
	// actually sent, so a healthy install never announces a recovery from
	// nothing.
	if d.lastLevel == "" {
		return
	}
	d.lastLevel = ""
	freeGB := float64(ds.Free) / (1024 * 1024 * 1024)
	d.notify.Send("Disk Space Recovered",
		fmt.Sprintf("%.1f%% used — %.1f GB free on output drive", ds.UsedPct, freeGB),
		notifications.TypeSuccess,
		[]notifications.Field{{Name: "Output Directory", Value: absOutputDir(outputDir)}},
		notifications.SendOptions{Event: "disk_ok"},
	)
}

// onReadFailure feeds one failed disk reading in (volume offline, I/O error).
func (d *diskAlerts) onReadFailure(outputDir string) {
	d.readFailCount++
	if !d.readFailing {
		d.log.Warn("[Disk] disk space check failed; gauge and low-disk alerts frozen until it recovers",
			"outputDir", outputDir)
		d.readFailing = true
	}
	// The safety net dying is itself alert-worthy: with monitoring frozen, the
	// drive can fill unnoticed. Once per streak, on the second consecutive
	// failure, to ride out a transient blip.
	if d.readFailCount == diskReadFailuresBeforeAlert {
		d.readFailNotified = true
		d.notify.Send("Disk Monitoring Failed",
			"Disk space checks are failing (volume offline or I/O error) — low-disk alerts are suspended until monitoring recovers",
			notifications.TypeError,
			[]notifications.Field{{Name: "Output Directory", Value: outputDir}},
			notifications.SendOptions{Event: "disk_warning"},
		)
	}
}
```

The "Disk Monitoring Failed" field keeps the RAW `outputDir` it has today, while the three others absolutise; that asymmetry is pre-existing and is left alone rather than silently changed.

- [ ] **Step 6: Delegate from the stats ticker in `cmd/moombox/main.go`**

Delete `lastDiskNotify`, `lastDiskLevel`, `diskReadFailing` and `diskReadFailCount` from the goroutine's locals (`:667-675`) and put one line in their place:

```go
		diskCheckCounter := 0
		diskAlerter := newDiskAlerts(notifyMgr, log)
```

Then in the disk block (`:752-846`) the whole notification decision collapses:

```go
					if ds := routes.UpdateDiskStatus(diskOutputDir, s.configStore); ds != nil {
						// Broadcast to web clients
						wsHub.Broadcast("disk_status", map[string]any{ /* unchanged */ })
						// Push to TUI
						select { /* unchanged */ }
						diskAlerter.onReading(ds, diskOutputDir, time.Now())
					} else {
						diskAlerter.onReadFailure(diskOutputDir)
					}
```

**The `notifyMgr.HasTargets()` guard at `:783` goes.** `Manager.Send` returns immediately when no targets are configured, so the guard changed nothing observable — and keeping it would put half the decision outside the type that owns the other half: a reading skipped by the guard would leave `lastLevel` stale, and the all-clear depends on that field being accurate. N1's plan expects this guard to survive its own change (it is one of the four `HasTargets` callers its `Notifier` interface exists for), so call the removal out explicitly in the task report rather than letting the reviewer find it. The other three callers are untouched.

- [ ] **Step 7: Run the tests to verify they pass**

```bash
cd /d/Git/Moombox/.worktrees/webhooks-n2a-producers
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go test -count=1 -timeout 300s -run 'TestRecordSuccess' -v ./internal/monitor/
go test -count=1 -timeout 300s -run 'TestDisk|TestChannelHealthNotifiers' -v ./cmd/moombox/
go test -count=1 -timeout 300s ./internal/monitor/ ./cmd/moombox/
go test -count=1 -timeout 300s -race ./internal/monitor/ ./cmd/moombox/
go vet ./internal/monitor/ ./cmd/moombox/
gofmt -l ./internal/monitor ./cmd/moombox
```

Expected: every new test PASSES; both packages `ok`; the race run clean (`recordSuccess` now fires a callback off its lock, the shape `recordError` already had); vet silent; `gofmt -l` prints nothing.

- [ ] **Step 8: Commit**

```bash
cd /d/Git/Moombox/.worktrees/webhooks-n2a-producers
git add internal/monitor/health.go internal/monitor/feed.go internal/monitor/decapi.go \
        internal/monitor/twitch.go internal/monitor/health_test.go \
        cmd/moombox/disk_alerts.go cmd/moombox/disk_alerts_test.go \
        cmd/moombox/channel_health_test.go cmd/moombox/monitor_callbacks.go cmd/moombox/main.go
git commit -m "feat: close the disk and channel incidents that had no end

Three alerts were raised and never resolved. The disk warning reset its level
to ok in silence, the monitoring-failure flag cleared in silence, and
healthTracker.recordSuccess cleared its notified flag with no callback at all —
so Channel Not Responding told an operator to go and check a rename or a ban and
nothing ever told them to stop. The 30-minute repeat on the disk warning made
the missing resolved line more visible, not less.

healthTracker gains onHealthy, fired once per notified streak and off the lock
the way onUnhealthy already is, with a setter on each of the three monitors. The
decision to SEND the close stays in cmd/moombox, because the tracker knows a
streak crossed the threshold but not that its alert was suppressed by the
cross-monitor confirmation — closing a suppressed streak would announce a
recovery from an incident nobody was told about. channelHealthNotifiers returns
the pair and is package-level, so the send is asserted for the first time;
before this only siblingReachable had a test.

The disk decision moves out of the stats ticker's closure into diskAlerts. The
warning path is the behaviour that closure already had, moved unchanged —
cooldown, level escalation, second-failure alert — and the two closes are new.
Both are unreachable from any test while they live inside a 190-line goroutine
that also reads memory stats and talks to the sidecar, which is why they move.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/monitor/health.go internal/monitor/feed.go internal/monitor/decapi.go internal/monitor/twitch.go internal/monitor/health_test.go cmd/moombox/disk_alerts.go cmd/moombox/disk_alerts_test.go cmd/moombox/channel_health_test.go cmd/moombox/monitor_callbacks.go cmd/moombox/main.go
```

---

### Task 9: Arc gates, then delete this plan

Nothing here is optional, and nothing here is a task's own gate re-run for comfort: this is the merge-candidate set from the Global Constraints plus the two arc-specific rules, run once against a branch that has `main` merged into it.

**Files:**
- Delete: `docs/superpowers/plans/2026-09-27-webhooks-n2a-producers.md`

**Interfaces:**
- Consumes: everything Tasks 1–8 produced.
- Produces: a merge-ready branch.

- [ ] **Step 1: Merge `main` into the branch**

N2b runs in parallel and may have merged first. The shared files are listed in "Shared files with N2b" above; every overlap is an adjacent hunk, so this should be clean.

```bash
cd /d/Git/Moombox/.worktrees/webhooks-n2a-producers
git fetch --all --quiet || true
git merge --no-edit main
```

If a conflict appears in `internal/notifications/manager.go`, `internal/config/types.go` or the `settings.js` editor body, **stop and report** — those are N2b's files and this arc declared it does not touch them, so a conflict there means an assumption is wrong. A conflict in `web/public/modules/settings.js`'s `NOTIFICATION_EVENT_GROUPS`, `docs/spec/operations.md` or `README.md` is expected and resolved by keeping BOTH arcs' additions.

- [ ] **Step 2: Re-verify the line numbers this plan cited**

Every producer line number in this plan was read at `1d2df1d4`, before N1. N1 edits `orchestrator_mux.go`, `stream_processor.go`, `orchestrator_twitch.go`, `orchestrator.go` and `monitor_callbacks.go`. Confirm each adopted site is where the tasks left it and that N1 did not re-introduce an inline builder:

```bash
cd /d/Git/Moombox/.worktrees/webhooks-n2a-producers
grep -rnE '\.Send\(\s*"' --include='*.go' internal/worker internal/web/routes cmd/moombox | grep -v _test.go
grep -rn -A1 --include='*.go' '\.Send($' internal/worker internal/web/routes cmd/moombox | grep -v _test.go
```

**Both forms, and the second is load-bearing.** `internal/web/routes/jobs.go` writes every one of its three sends as `notifier.Send(` then the title on the NEXT line, so the single-line pattern returns nothing at all from the package that owns two of the five unified families. The receiver is left out of both patterns on purpose: this arc's new sends use `d.notify` and `a.notify`, which a `notifier.Send(` / `notifyMgr.Send(` pattern would miss. Baseline at N1's merge (`bbe0e674`): **26 single-line + 3 multi-line = 29** literal-titled sends.

Expected — the load-bearing half only: **none of `"Video Added"`, `"Twitch Video Added"`, `"Stream Found"`, `"Twitch Stream Found"`, `"Download Cancelled"`, `"Job Cancelled"`, `"Download Finished"` or `"Trim Created"` may remain** as a literal at a call site, in either form. Those five families come from `internal/notifications/builders.go` now.

Everything else the two greps return is a send this arc deliberately did not unify: "YouTube Download Starting", "Twitch Download Starting", "Muxing Starting", "Trim Deleted", "Trim Failed", "Authentication Required", "Job Failed", "YouTube Start Time Confirmed", "YouTube Schedule Changed", "Authentication Recovered", "Parked Jobs Re-evaluated", "Channel Not Responding", "Update Available", "Update Applied", "Previous Update Failed", "Recovered From Crash", "Disk Monitoring Failed" — plus this arc's five new ones: "Channel Responding Again", "BotGuard Sidecar Down", "BotGuard Sidecar Restored", "Disk Space Recovered", "Disk Monitoring Recovered". The split/connectivity builders and "Outage Alert" appear in neither grep: their titles are computed, not literal.

- [ ] **Step 3: The merge-candidate gates**

```bash
cd /d/Git/Moombox/.worktrees/webhooks-n2a-producers
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp

gofmt -l ./cmd ./internal ./tools
go vet ./...
GOOS=linux GOARCH=amd64 go vet ./...
staticcheck ./...
go mod tidy -diff
go build ./...
GOOS=linux GOARCH=amd64 go build ./...
GOOS=linux GOARCH=arm64 go build ./...
node --test --test-timeout=120000 web/tests/*.test.mjs
go test -count=1 -timeout 300s ./internal/docs/
```

Expected: `gofmt -l` prints nothing; both vets silent; staticcheck silent (a hard gate — an unused local left behind by a collapsed send is a failure, not a warning); `go mod tidy -diff` prints nothing; all three builds succeed; the node suite reports `fail 0` with the counts `web/tests/README.md` claims; `internal/docs` `ok`.

- [ ] **Step 4: The ONE full test run (controller)**

Only one of these runs across the whole chain at a time.

```bash
cd /d/Git/Moombox/.worktrees/webhooks-n2a-producers
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./...
```

Expected: every package `ok` or `no test files`. (`TestBotGuardLiveFingerprint` is a long-standing known failure and not a gate — nothing in this arc touches goja.)

- [ ] **Step 5: Unfiltered `-race` over the arc's packages**

```bash
cd /d/Git/Moombox/.worktrees/webhooks-n2a-producers
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -race -timeout 300s \
  ./internal/notifications/ ./internal/worker/ ./internal/web/routes/ ./internal/monitor/ ./internal/tui/ ./cmd/moombox/
```

Expected: all six `ok`, no race report. This arc adds one mutex-guarded debounce driven from a timer goroutine (`sidecarAlerts`), one callback fired off a tracker lock (`recordSuccess`), and one mutex-guarded map per channel-health pair — `cmd/moombox` and `internal/monitor` are where a finding would land.

- [ ] **Step 6: The import fences**

```bash
cd /d/Git/Moombox/.worktrees/webhooks-n2a-producers
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go list -deps ./internal/tui | grep -E 'Moombox/internal/(web|web/routes|bgutils|bgutils/sidecar|worker|monitor)$'
go list -deps ./internal/notifications | grep -E 'Moombox/internal/(database|worker|web|web/routes|monitor|engine)$'
```

Expected: **no output from either** (grep exits 1, which is the pass). The second is this arc's own temptation: `JobFacts` exists in the shape it does precisely so `internal/notifications` never needs a row type, and `internal/tui` imports that package.

- [ ] **Step 7: Confirm the FFmpeg-backed finalize tests ran**

Task 4 rewired both finalize paths. The worker tests that actually drive a mux only mean anything with FFmpeg on PATH; confirm they reported **PASS, not SKIP**, in Step 4:

```bash
cd /d/Git/Moombox/.worktrees/webhooks-n2a-producers
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -timeout 300s -v \
  -run 'Mux|Finalize|Segment' ./internal/worker/ | grep -E '^--- (PASS|SKIP|FAIL)' | sort | uniq -c
```

Expected: no `--- SKIP` among them. A SKIP means FFmpeg is not on this host's PATH and the two finalize paths were never executed end to end — fix the host, do not merge.

**Residuals to carry into the owner report** (none of them blocks the merge):

1. **Field gate — the first real recovery embed.** `sidecar_down` needs a sidecar the supervisor genuinely cannot restart for a minute; `disk_ok` needs a real disk filling and being freed; `channel_healthy` needs a channel to fail twenty consecutive checks and come back. All three are pinned by fixtures and none has been seen in the field.
2. **`errorStage` classifies by prefix, and two finalize returns still escape it.** `"create segment output dir: …"` (`internal/worker/orchestrator_mux.go:1384`) and `"no segment files found in staging directory"` (`:1524`) read as "download". Naming them properly means threading a stage argument through `setJobError`, which touches every failure path — a follow-up, not this arc.
3. **The CLI Twitch add's description no longer names the media type.** Today it reads `"Manually added Twitch vod: tw_v123"`; the unified builder says `"Manually added: tw_v123"`. The id still distinguishes the two shapes (a VOD add's id is `tw_v<digits>`, a live add's is the channel login) and the platform is in the author line and footer, so the loss is the word, not the fact.
4. **The multi-part `Duration` figure can move by up to a second.** The old builder truncated the summed float to whole seconds once; the builder sums per-part `time.Duration` values instead, so a three-part job's rendered duration may differ from the row's `length_seconds` by sub-second rounding. More accurate, not less, but it is a visible change.
5. **"Chat Messages" is now omitted at zero on the single-part path.** It used to render `0` whenever the pointer was non-nil; the unified builder requires `> 0`, matching what the multi-part path always did.
6. **The `HasTargets()` guard around the disk sends is gone** (Task 8 Step 6). `Manager.Send` returns immediately with no targets, so nothing observable changed, but it is a deliberate deletion rather than an oversight.
7. **A one-segment multi-part job now gets the SINGLE-part embed shape.** `finalizeMultiSegmentJob` reaches `renameSinglePartToPlain` (`internal/worker/orchestrator_mux.go:948-950`) for a job whose split produced exactly one part; its embed used to say `Segments: 1 segments` with a one-entry `Qualities`, and now says `File` / `File Size`. That matches what the file on disk is actually called, so it is an improvement — but it is a visible change to an embed shape.
8. **"Muxing Starting" is left without `Platform`, `JobID` or `Author`.** After this arc it is the one job-lifecycle embed with no footer job id and no author line while its five neighbours have both. Out of scope — spec §2 names five families and `muxing` is not one — and deliberately NOT widened here; worth a follow-up.
9. **`M1` (recover-asides outcome), `M2` (members-only marker on Stream Found), `M3` (backlog find at creation vs admission), `M5` (respawn counter), `M7` (Twitch chat re-authenticated) are NOT in this arc.** The spec scoped N2a to A1/A2/A3/A5/A6/M8 and C1–C5; the remaining MAYBEs are still open owner calls.

- [ ] **Step 8: Delete this plan and commit**

The plan is implemented and verified; git history is the archive.

```bash
cd /d/Git/Moombox/.worktrees/webhooks-n2a-producers
git rm docs/superpowers/plans/2026-09-27-webhooks-n2a-producers.md
git commit -m "chore(plans): delete the Arc N2a plan — implemented and verified

Every task is merged into the branch and the merge-candidate gates are green:
gofmt, both vets, staticcheck, go mod tidy -diff, all three platform builds,
the node suite, the docs citation test, one full go test ./..., -race over the
six packages this arc touches, and both import fences — internal/tui reaches
neither web nor worker nor the sidecar, and internal/notifications reaches
neither database nor worker, which is what lets the builders live in the
package the TUI imports.

Residuals recorded in the owner report: the three recovery embeds have no field
sighting yet; two finalize returns still classify as the download stage; the CLI
Twitch add no longer names the media type in its sentence; the multi-part
duration can round differently by under a second; Chat Messages is omitted at
zero on the single-part path now as it always was on the multi-part one; the
HasTargets guard around the disk sends is gone because Send is already a no-op
without targets, which the N1 owner is told about.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- docs/superpowers/plans/2026-09-27-webhooks-n2a-producers.md
```

- [ ] **Step 9: Report**

Hand the controller: every gate's result, the seven residuals from Step 7, and the decisions this arc made that the spec did not dictate —

- **`worker.NotifyFacts` as the shared mapper**, rather than one per producer package or a new package: `internal/web/routes` and `cmd/moombox` already depend on `internal/worker`, and `internal/notifications` must not see a row type.
- **The builders' return tuple is `Sender.Send`'s parameter list**, so adoption is `n.Send(notifications.X(f))` and a drift is a compile error.
- **`JobFacts` carries no scheduled time.** The prompt's field sketch suggested one, but none of the five builders has a consumer for it — `scheduled`/`rescheduled` are not unified in this arc — so it was left out rather than added dead.
- **The per-platform description in `StreamFound`** (audit C4 permitted either); the titles are unified, the sentences are not, because a Twitch find is live now and a YouTube find usually is not.
- **"Parts" rather than "Segments" for the part count**, because "Segments" is already the job-level video/audio sequence counter and the multi-part embed now carries both.
- **"Staging: preserved" without "Resume available" for non-YouTube jobs** — Resume is YouTube-only in both UIs and the route answers 400 — a narrowing of the spec's literal wording.
- **"Disk Monitoring Recovered" sends under `disk_ok`**, the same key as the space all-clear: one close for the whole disk family, reachable by a `disk_warning` filter through the alias. A reading that closes BOTH a reported read-failure and an open warning therefore emits two `disk_ok` embeds — two incidents, two closes, documented in `onReading` and in the operations table.
- **Tell the N1 owner** that `cmd/moombox/main.go:783`'s `HasTargets()` call is gone (Task 8 Step 6): N1's `Notifier` interface is justified by four call sites and there are now three.
- **Each monitor gets its own channel-health pair** (its own `sent` set), matching the fact that the feed monitor and DECAPI already raise separate alerts for the same channel.
- **The cmd-side `sent` set rather than the tracker's `notified` flag** decides whether a channel close is sent, because the tracker cannot see the cross-monitor suppression.
- Anything Step 2's grep turned up that this plan did not anticipate.
