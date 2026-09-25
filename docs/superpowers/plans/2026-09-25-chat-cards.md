# Arc K — player sidebar chat cards — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The player's sidebar chat stops rendering every paid, membership and Twitch event as a flat line of text. A YouTube Super Chat or Super Sticker becomes a two-part card in its own tier colours (K1); a membership event becomes a green card carrying the renderer's own header line (K2); a Twitch sub, resub, gift or raid becomes a purple-accented notice block with its system line (K3); a bits message gains a cheer chip coloured by Twitch's amount scale (K3). The video overlay is untouched (K4). Two of those shapes need archive data the Go chat parser throws away today, so two bounded parser tasks run first.

**Architecture:** One worktree, eight tasks, strictly sequential. Tasks 1–2 are Go (`internal/chat`); tasks 3–6 are the four UI shapes and all three of them touch the same three files (`web/public/modules/player.js`, `web/public/moombox.css`, `web/tests/player.test.mjs`) — see the shared-file table below for how each diff stays separable; task 7 is docs plus the one stylesheet-text pin and the README recount; task 8 is the gates and this plan's deletion.

Task 3 does the only structural work: `_buildChatMessageEl` becomes a four-line shape dispatch over four `_fill*` methods, with the existing flat row extracted **verbatim** into `_fillPlainRow` and the shared pieces (`_timeSpan`, `_authorSpan`, `_cardParts`) factored out. Tasks 4–6 then each add exactly one `_fill*` method, one dispatch line, one CSS block and one test block. That ordering is deliberate: it puts every merge-prone edit in one task and leaves the following three additive.

**Tech Stack:** Go 1.27 (`toolchain go1.27.1`), no CGo. Vanilla ES modules under `web/public/` embedded by `go:embed` (`web/embed.go`), so an asset change needs a `go build`. Node 24 + `node:test` for `web/tests`; jsdom 30.0.1 (verified installed in this worktree) for `player.test.mjs` through `web/tests/helpers/player-dom.mjs`'s `makePlayer(opts)`. Git Bash on Windows for every command below.

**Spec:** `docs/superpowers/specs/2026-09-25-tui-120fps-and-chat-cards-design.md` — §0 K1–K4 (the owner's rulings verbatim), §2 (this arc's scope), §2.1 (the capture gaps, resolved below), §3 (order; branch `chat-cards`, cut from main `721ada06`).

---

## §2.1 findings — both gaps are real

Checked by reading `internal/chat/api.go` (`parseAction` at `:474`, `selectRenderer` at `:583`, `parseMessageRenderer` at `:620`) and by searching for every membership/sponsorship fixture in the package (`internal/chat` has no `testdata/` directory — every chat fixture is an inline `map[string]any` literal in a `_test.go` file; the only membership mention is one `selectRenderer` table row at `internal/chat/api_test.go:285`).

**(a) Membership header text is NOT captured.** `parseAction:513` sets `msg.IsMembership = true` and nothing else; `parseMessageRenderer` reads only `renderer["message"]`. A `liveChatMembershipItemRenderer` keeps its human-readable line in `headerPrimaryText` (a milestone: "Member for 6 months") or, when there is no primary text, in `headerSubtext` (a new member: "Welcome to Member!"), and carries `message` only for a milestone the member typed into. So a **new-member event archives as a message with an empty `message` array** and no other content at all, and a milestone loses its "Member for N months" line. Field shape confirmed against `references/bgutil-ytdlp-pot-provider/server/node_modules/.deno/youtubei.js@16.0.1/node_modules/youtubei.js/bundle/browser.js:22885-22910` (`LiveChatMembershipItem`: `header_primary_text` optional, `header_subtext` always, `message` optional). → **Task 1.**

**(b) Neither sponsorship renderer is parsed.** `selectRenderer`'s roster is exactly four keys — `liveChatTextMessageRenderer`, `liveChatPaidMessageRenderer`, `liveChatPaidStickerRenderer`, `liveChatMembershipItemRenderer` — so `liveChatSponsorshipsGiftPurchaseAnnouncementRenderer` and `liveChatSponsorshipsGiftRedemptionAnnouncementRenderer` return `nil` and are **dropped silently**; gifted memberships are absent from every archive Moombox has written. (`internal/chat/helpers_test.go:104-109` already claims the roster has five entries including the gift-purchase one — a comment written against an intention, not the code. Task 2 makes it true and correct, at six.) Shapes confirmed at the same reference, `:23125-23200`: the purchase renderer nests `authorName`/`authorBadges`/`primaryText` one level down in `header.liveChatSponsorshipsHeaderRenderer` and carries no `timestampText`, while the redemption renderer is flat and carries its line in `message`. → **Task 2.**

---

## Global Constraints

Every task's requirements implicitly include this section.

- **Sidebar only (K4), pinned.** `_buildNicoEl` (`web/public/modules/player.js:1711-1724`) must be **byte-identical** at the end of the arc, and the overlay's rendered output must not move either. The gate in task 8 counts only **code** lines the diff added or removed —

  ```bash
  git diff main -- web/public/modules/player.js | grep -cE '^[-+][^*]*(_buildNicoEl\(|"nico-message")'
  ```

  expected `0` — and the function body is re-read there. A bare `grep -c '_buildNicoEl'` over the diff would be a false-positive machine: a prose mention of the overlay in any comment this arc adds would trip it. **Corollary, and a rule for every task: no comment this arc writes may name `_buildNicoEl` or the string `"nico-message"`** — say "the overlay builder" instead. This constraint is also why task 1 adds a NEW field rather than folding the membership header line into `message`: the overlay renders `msg.message` and returns `null` when it is empty, so a membership event that renders as nothing today would start scrolling across the video.
- **The row is still a row.** Every shape produces exactly ONE element, appended as one direct child of `#player-sidebar-messages`, carrying the `chat-msg` class. The sidebar's promotion (`updateSidebarActiveState:1284`), reset (`resetSidebarToTime:1337`), post-end promotion (`_markPostEnd:1247`), divider reconciliation (`_applyDividers:1209`), search filter (`filterChat:554`) and scroll maths (`syncSidebarToTime:1302`) all address rows by `container.children[i]` and never by a selector — but every `.chat-msg.<state>` rule in `web/public/moombox.css` does key on the class, and so does the jsdom harness's `measure()` (`web/tests/helpers/player-dom.mjs:325`), which is what gives a row a non-zero `offsetTop`. **Decision pinned: cards and notices keep `chat-msg` and add a modifier class; no timeline code learns a new class.** Task 3's "a card is still a timeline row" test is the pin.
- No Shoelace bundling; no new CDN dependency; **no new fetches** — every field these shapes read is already in the chat file the player loads from `GET /api/jobs/{id}/chat` (`internal/web/routes/jobs.go:540`, which serves the archive verbatim through `serveChatJSON` — there is no normaliser anywhere between the file and `_buildChatMessageEl`, so the JS sees the Go JSON tags exactly).
- **The CSP is untouched.** Colours reach the DOM as CSS custom properties written through the CSSOM (`el.style.setProperty("--card-header", …)`). A CSSOM write is not an inline `<style>` element and is not governed by `style-src`; and `internal/web/middleware.go:98` grants `style-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net` regardless, a grant `internal/web/middleware_csp_test.go:71` pins for Shoelace's shadow DOM. No task edits `middleware.go`.
- DB layer untouched; no config key; no API route change; no schema change.
- The anonymous logger interface stays anonymous per struct; every goroutine keeps its inline `defer func() { if r := recover(); … }()`. (Only tasks 1–2 touch Go, and neither starts a goroutine.)
- TDD per task: the failing test is written and run **red** before the change; every new assertion names the mutant that fails it, and each task verifies at least one mutant **by execution**, by hand-editing the file and reverting it by hand (no `git checkout`, no `git stash`).
- One commit per task **with a pathspec** (`git commit -F <msgfile> -- <paths>`); never a bare `git commit`. No stash/checkout/rebase/reset/amend.
- Every commit's LAST TWO LINES are exactly `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>` and `Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq` — the project's rule, which takes precedence over any attribution reminder in an implementer's own context whatever model name it shows.
- **LF only** on every file written. Verified per task with `perl -0777 -ne 'print tr/\r//'` (`grep -c $'\r'` and `awk '/\r/'` both lie about CRLF under Git Bash).
- `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp` on every go command (export it once per shell). **Package-scoped go test runs only** inside a task — `go test ./...` is the controller's, once, in task 8. Never `python3 -` heredocs (the uv shim hangs); `perl` or `node -e` instead. Never kill processes by image name; never `rm -rf` under `%TEMP%`. Leave no stray `node` process behind: every `node --test` invocation below is wrapped in `timeout 300`.
- Line numbers quoted below are from the worktree at `721ada06`. Locate every target by the quoted text or symbol as well, never by the number alone.

---

## Branch and worktree

Already created and prepared (verified 2026-09-25): `D:/Git/Moombox/.worktrees/chat-cards` on branch `chat-cards` at `721ada06` (main's head, which carries the spec), with the four embed blobs in `internal/bgutils/embed/`, `internal/cipher/testdata/*.js` and `web/tests/node_modules` (jsdom 30.0.1) all present, and a clean `git status`. The recipe is recorded only so it can be rebuilt if the worktree is lost:

```bash
cd /d/Git/Moombox
git worktree add -b chat-cards .worktrees/chat-cards main
cp internal/bgutils/embed/node-windows-amd64.gz \
   internal/bgutils/embed/node-linux-amd64.gz \
   internal/bgutils/embed/node-linux-arm64.gz \
   internal/bgutils/embed/sidecar.tar.gz \
   .worktrees/chat-cards/internal/bgutils/embed/
mkdir -p .worktrees/chat-cards/internal/cipher/testdata
cp internal/cipher/testdata/*.js .worktrees/chat-cards/internal/cipher/testdata/
cd .worktrees/chat-cards/web/tests && npm ci --no-audit --no-fund
```

Every path below is relative to the worktree root `D:/Git/Moombox/.worktrees/chat-cards`. The plan commit sits on top of `721ada06`, so every code line number above still resolves.

---

## File Structure

| File | Task | Responsibility after this arc |
|---|---|---|
| `internal/chat/types.go` | 1 | `ChatMessage` gains `MembershipText string \`json:"membershipText,omitempty"\`` — the membership renderer's own header line, empty for every other kind of message. |
| `internal/chat/api.go` | 1, 2 | `renderedText(any) string` (new, task 1): a YouTube text field as a plain string, `simpleText` or joined `runs`. `parseAction`'s membership branch sets `MembershipText` (task 1) and gains the two sponsorship branches (task 2). `selectRenderer`'s roster grows to six (task 2). `giftPurchaseFields(map[string]any) (map[string]any, string)` (new, task 2) hoists the purchase renderer's nested header. |
| `internal/chat/api_test.go` | 1, 2 | Gains the membership-text table (task 1) and the two sponsorship tests (task 2); `TestSelectRendererPicksFirstMatch`'s table grows by two rows (task 2). |
| `internal/chat/helpers_test.go` | 2 | `TestSelectRendererSuperChatPaidMessageBranch`'s doc comment stops claiming a roster the code does not have. Comment only. |
| `web/public/modules/player.js` | 3, 4, 5, 6 | `_buildChatMessageEl` becomes a shape dispatch. New module-scope exports: `SUPERCHAT_TIER_COLORS`, `relativeLuminance`, `readableInk` (3), `MEMBER_CARD_COLORS` (4), `TWITCH_NOTICE_TYPES`, `twitchNoticeLine` (5), `CHEER_SCALE`, `cheerColor` (6). New methods: `_timeSpan`, `_authorSpan`, `_cardParts`, `_fillPlainRow`, `_fillSuperchatCard` (3), `_fillMemberCard` (4), `_fillTwitchNotice` (5), `_cheerChip` (6). `_buildNicoEl` **byte-identical**. |
| `web/public/moombox.css` | 3, 4, 5, 6 | `.chat-msg.superchat`'s lone `border-left` is replaced by the card block; `.chat-msg.divider-before.future > span` becomes `> *`. New: the card shell + ink classes (3), `.chat-card-note` (4), the Twitch notice block (5), `.cheer-chip` (6). |
| `web/tests/player.test.mjs` | 3, 4, 5, 6 | One new section per task, all under one "Sidebar chat cards (Arc K)" banner with the shared `ytMsg`/`showChat` fixtures task 3 adds. |
| `web/tests/a11y-controls.test.mjs` | 7 | One more stylesheet-as-text test (no `skip`, no jsdom), beside the existing one: the divider-dim rule reaches a card's block children. |
| `docs/spec/user-interfaces.md` | 7 | The `web/public/modules/player.js` row records the card/notice shapes, the palette fallback, the luminance rule and "sidebar only". |
| `web/tests/README.md` | 7 | Counts and the per-suite breakdown recounted from live runs, with and without jsdom. |
| `web/public/modules/chat-timeline.js` | — | **Untouched.** The timeline maths does not see DOM. |
| `internal/twitch/*` | — | **Untouched.** Every field K3 needs is already archived (`TwitchChatMessage.MessageType/Bits/SystemMsg/SubPlan/GiftRecipient/ViewerCount/AuthorBadges`, `internal/twitch/types.go:62-86`). |
| `docs/superpowers/plans/2026-09-25-chat-cards.md` | 8 | Deleted — git history is the archive. |

### Shared-file note (checked — this is why the tasks are strictly sequential)

Tasks 3, 4, 5 and 6 all write **the same three files**. They are ordered, never parallel, and each one's diff is kept separable by construction:

| File | Task 3 | Task 4 | Task 5 | Task 6 |
|---|---|---|---|---|
| `player.js` | restructures `_buildChatMessageEl`; adds 3 shared helpers + `_fillSuperchatCard`; adds 1 dispatch line | adds `MEMBER_CARD_COLORS` + `_fillMemberCard` + **1** dispatch line after task 3's | adds `TWITCH_NOTICE_TYPES`/`twitchNoticeLine` + `_fillTwitchNotice` + **1** dispatch line after task 4's | adds `CHEER_SCALE`/`cheerColor` + `_cheerChip` + **6** lines inside `_fillPlainRow` |
| `moombox.css` | deletes `.chat-msg.superchat`; edits one selector; appends the card block at the end of the chat section | appends `.chat-card-note` | appends the notice block | appends `.cheer-chip` |
| `player.test.mjs` | appends the section banner, the shared fixtures and **9** tests (7 jsdom + 2 pure) | appends **3** tests (3 jsdom) | extends the `player.js` import line, then appends **4** tests (3 jsdom + 1 pure) | extends the `player.js` import line, then appends **3** tests (2 jsdom + 1 pure) |

Task 3 is the only one that modifies existing lines in `player.js` or `moombox.css`; tasks 4, 5 and 6 delete nothing in either (verify with `git diff <previous task's commit> -- web/public/modules/player.js web/public/moombox.css | grep -c '^-[^-]'` → `0`). In `player.test.mjs` tasks 5 and 6 each have exactly **one** non-append hunk, the shared `player.js` import line their step 1 tells the implementer to extend. Tasks 1 and 2 share `internal/chat/api.go` and `internal/chat/api_test.go` and are likewise ordered — task 2 consumes task 1's `renderedText` and `MembershipText`, and anchors on the membership branch task 1 writes. Task 5 anchors on task 4's `MEMBER_CARD_COLORS` and `_fillMemberCard`, so it needs task 4 in place, not merely task 3. Task 7 touches three files no earlier task does. No task may be started before the previous one's commit exists.

---

### Task 1: the membership renderer's header line reaches the archive

Spec §2.1, first bullet. `parseAction:513` currently reads:

```go
	if _, ok := item["liveChatMembershipItemRenderer"]; ok {
		msg.IsMembership = true
	}
```

— it throws the renderer away after testing for its presence. Everything a reader would want to see on a membership event is in the two fields it discards. The result today: a new member archives as a message with an empty `message` array, no amount and no text, which the sidebar renders as a bare timestamp and a name.

**Files:**
- Modify: `internal/chat/types.go`
- Modify: `internal/chat/api.go`
- Modify: `internal/chat/api_test.go`

**Interfaces:**
- Produces: `ChatMessage.MembershipText string` (JSON `membershipText,omitempty`) and `renderedText(field any) string` (unexported, `api.go`).
- Consumes: nothing new.
- Unchanged: `ChatMessage.IsMembership`, `parseMessageRenderer`, `parseMessageRuns`, `selectRenderer`, the `SuperchatInfo` path.

**Ruling — a new `membershipText` field, not a prepended `message` part.** Spec §2.1 allows either. A new field wins on three counts. (i) K2 asks for "the author, **the milestone text** and any message" — three distinct things — and a card that had them concatenated into one runs array could not put the milestone line beside the name and the member's own words in the body. (ii) `_buildNicoEl` renders `msg.message` and returns `null` when it produces no nodes, so folding text into `message` would start scrolling new-member notices across the video — exactly what K4 forbids. (iii) The JS reads one new string; `message` keeps its shape, so `appendChatContent`, `filterChat`'s `textParts` walk and `correctLegacyTwitchEmotes` all stay as they are. **Cost if wrong:** the overlay never shows membership header text (it shows nothing for those messages today either), and a later task can append it.

**Ruling — `headerPrimaryText` when present, else `headerSubtext`; never both.** A milestone carries both, and its subtext is the bare tier name ("Member") beside a primary text that already says "Member for 6 months" — a second line that restates a field, which the project rules out. A new member carries only the subtext, which is the whole message ("Welcome to Member!"). One line, chosen by presence. **Cost if wrong:** a milestone card omits the tier name.

- [ ] **Step 1: Write the failing tests**

Append to `internal/chat/api_test.go` (its imports already cover `testing`; nothing new is needed):

```go
// membershipItem builds a liveChatMembershipItemRenderer action in the shape
// YouTube ships. A NEW member carries headerSubtext and no message at all; a
// MILESTONE carries headerPrimaryText ("Member for 6 months"), a headerSubtext
// that is only the tier name, and the member's own message. Confirmed against
// youtubei.js' LiveChatMembershipItem (header_primary_text optional,
// header_subtext always, message optional).
func membershipItem(primary, subtext, message string) map[string]any {
	r := map[string]any{
		"id":            "memb-1",
		"timestampUsec": "1700000000000000",
		"authorName":    map[string]any{"simpleText": "newfan"},
	}
	if primary != "" {
		r["headerPrimaryText"] = map[string]any{"runs": []any{
			map[string]any{"text": primary},
		}}
	}
	if subtext != "" {
		r["headerSubtext"] = map[string]any{"simpleText": subtext}
	}
	if message != "" {
		r["message"] = map[string]any{"runs": []any{map[string]any{"text": message}}}
	}
	return map[string]any{"addChatItemAction": map[string]any{
		"item": map[string]any{"liveChatMembershipItemRenderer": r},
	}}
}

// TestMembershipHeaderTextIsArchived: until this change parseAction tested for
// the membership renderer's PRESENCE and threw it away, so a new-member event
// archived as a message with an empty message array and no text anywhere — the
// sidebar had a timestamp and a name to render and nothing else. The header
// line is the event.
//
// Mutants this kills: dropping the MembershipText assignment (every want below
// comes back ""); reading headerSubtext first (the milestone row reports
// "Member" instead of "Member for 6 months"); folding the line into Message
// instead (the message assertions fail, and with them the overlay-silence
// guarantee K4 rests on).
func TestMembershipHeaderTextIsArchived(t *testing.T) {
	api := NewChatAPI("k", "", nil)
	cases := []struct {
		name        string
		primary     string
		subtext     string
		message     string
		wantText    string
		wantMessage string
	}{
		{"new member", "", "Welcome to Member!", "", "Welcome to Member!", ""},
		{"milestone", "Member for 6 months", "Member", "thanks!", "Member for 6 months", "thanks!"},
		{"milestone with no message", "Member for 2 months", "Member", "", "Member for 2 months", ""},
		{"neither header", "", "", "just talking", "", "just talking"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := api.parseAction(membershipItem(c.primary, c.subtext, c.message))
			if msg == nil {
				t.Fatal("parseAction returned nil for a membership item")
			}
			if !msg.IsMembership {
				t.Error("IsMembership must stay set")
			}
			if msg.MembershipText != c.wantText {
				t.Errorf("MembershipText = %q, want %q", msg.MembershipText, c.wantText)
			}
			var gotMessage string
			for _, p := range msg.Message {
				gotMessage += p.Text
			}
			if gotMessage != c.wantMessage {
				t.Errorf("Message = %q, want %q — the header line must NOT be folded into it", gotMessage, c.wantMessage)
			}
		})
	}
}

// TestRenderedTextReadsBothTextShapes: YouTube writes a text field either as
// {"simpleText": …} or as {"runs": [{"text": …}, …]} and uses both within one
// membership renderer, so the reader has to take either. Emoji runs (which
// carry no "text") contribute nothing rather than an "undefined".
//
// Mutants this kills: handling only simpleText (the runs rows come back "");
// handling only runs (the simpleText row comes back ""); concatenating a run's
// whole map rather than its text.
func TestRenderedTextReadsBothTextShapes(t *testing.T) {
	cases := []struct {
		name  string
		field any
		want  string
	}{
		{"simple", map[string]any{"simpleText": "Welcome!"}, "Welcome!"},
		{"runs", map[string]any{"runs": []any{
			map[string]any{"text": "Member for "},
			map[string]any{"text": "6"},
			map[string]any{"text": " months"},
		}}, "Member for 6 months"},
		{"simple wins over empty runs", map[string]any{
			"simpleText": "Welcome!", "runs": []any{},
		}, "Welcome!"},
		{"a run with no text contributes nothing", map[string]any{"runs": []any{
			map[string]any{"text": "a"},
			map[string]any{"emoji": map[string]any{"emojiId": "x"}},
			map[string]any{"text": "b"},
		}}, "ab"},
		{"absent", nil, ""},
		{"wrong type", "Welcome!", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := renderedText(c.field); got != c.want {
				t.Errorf("renderedText = %q, want %q", got, c.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run the tests and see them RED**

```bash
cd /d/Git/Moombox/.worktrees/chat-cards
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go test -count=1 -timeout 300s -run 'TestMembershipHeaderTextIsArchived|TestRenderedTextReadsBothTextShapes' -v ./internal/chat/
```

Expected: **build failure**, not a test failure — `undefined: renderedText` and `msg.MembershipText undefined (type *ChatMessage has no field or method MembershipText)`. That is the honest red for a test that names symbols this task creates. Record the exact compiler lines.

- [ ] **Step 3: Add the field**

In `internal/chat/types.go`, inside `ChatMessage` (currently `:14-26`), add the field immediately after `IsMembership`:

```go
	IsMembership    bool           `json:"isMembership,omitempty"`

	// MembershipText is a liveChatMembershipItemRenderer's own header line —
	// "Member for 6 months" (headerPrimaryText) for a milestone, "Welcome to
	// Member!" (headerSubtext) for a new member — and the gift-purchase
	// announcement's primaryText ("Gifted 5 memberships"). It is the EVENT;
	// Message is only what the member typed, which a new member or a gift
	// purchase does not have.
	//
	// A separate field rather than a run prepended to Message: the sidebar's
	// member card shows the line beside the author and the message in its
	// body, and the video overlay renders Message alone, so folding it in
	// would start scrolling membership notices across the video (2026-09-25
	// ruling K4, "sidebar only"). Empty on every other kind of message and on
	// every file written before this field existed.
	MembershipText string `json:"membershipText,omitempty"`
```

**The field is written unpadded, deliberately.** A full-line comment inside a struct ends gofmt's tabwriter alignment run, so `MembershipText` starts a run of one and must NOT be padded out to the `IsMembership` column. Padding it makes `gofmt -l ./internal/chat` name `types.go` at step 7 — confirmed by execution (2026-09-25), which produced the `gofmt -d` hunk `-\tMembershipText  string         …` / `+\tMembershipText string …`.

- [ ] **Step 4: Add the reader and wire the branch**

In `internal/chat/api.go`, add `renderedText` immediately **after** `parseMessageRuns`, which ends at `:874` — i.e. directly above `extractNavURL` (`:876-894`):

```go
// renderedText flattens one of YouTube's text fields to a plain string.
// YouTube writes them two ways — {"simpleText": "…"} and {"runs": [{"text":
// "…"}, …]} — and uses both inside a single liveChatMembershipItemRenderer
// (headerSubtext is usually simple, headerPrimaryText is always runs), so a
// reader has to take either. Runs that carry no "text" (an emoji run)
// contribute nothing. Returns "" for an absent or wrongly-typed field.
//
// parseMessageRuns is the richer sibling and stays the reader for message
// bodies: it keeps emoji, links and bold/italic as MessagePart values. This
// one exists for the fields that are displayed as one flat line.
func renderedText(field any) string {
	m, ok := field.(map[string]any)
	if !ok {
		return ""
	}
	if s, ok := m["simpleText"].(string); ok && s != "" {
		return s
	}
	runs, _ := m["runs"].([]any)
	var b strings.Builder
	for _, run := range runs {
		runMap, _ := run.(map[string]any)
		if runMap == nil {
			continue
		}
		if t, ok := runMap["text"].(string); ok {
			b.WriteString(t)
		}
	}
	return b.String()
}
```

`strings` is already imported by `api.go` (used by `parseNegativeTimestampText` and `extractCurrency`); confirm by build rather than by eye.

Then replace the membership branch in `parseAction` (`:513-515`):

```go
	if _, ok := item["liveChatMembershipItemRenderer"]; ok {
		msg.IsMembership = true
	}
```

with:

```go
	if memb, ok := item["liveChatMembershipItemRenderer"].(map[string]any); ok {
		msg.IsMembership = true
		// The event itself: "Member for 6 months" when YouTube sends a primary
		// text, else the subtext ("Welcome to Member!"), which is all a
		// new-member renderer has. Never both — a milestone's subtext is just
		// the tier name the primary text already names.
		msg.MembershipText = renderedText(memb["headerPrimaryText"])
		if msg.MembershipText == "" {
			msg.MembershipText = renderedText(memb["headerSubtext"])
		}
	}
```

- [ ] **Step 5: Run the tests and see them GREEN**

```bash
cd /d/Git/Moombox/.worktrees/chat-cards
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go test -count=1 -timeout 300s -run 'TestMembershipHeaderTextIsArchived|TestRenderedTextReadsBothTextShapes|TestSelectRenderer|TestParseAction|TestParseMessageRuns' -v ./internal/chat/
```

Expected: `ok github.com/vampiricwulf/Moombox/internal/chat`, all four `TestMembershipHeaderTextIsArchived` subtests and all six `TestRenderedTextReadsBothTextShapes` subtests passing, and every pre-existing `TestSelectRenderer*` / `TestParseAction*` / `TestParseMessageRuns*` test still green.

- [ ] **Step 6: Verify a mutant by execution**

By hand (no `git checkout`, no `git stash`), swap the two `renderedText` calls in the branch so `headerSubtext` is read first:

```go
		msg.MembershipText = renderedText(memb["headerSubtext"])
		if msg.MembershipText == "" {
			msg.MembershipText = renderedText(memb["headerPrimaryText"])
		}
```

```bash
cd /d/Git/Moombox/.worktrees/chat-cards
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go test -count=1 -timeout 300s -run 'TestMembershipHeaderTextIsArchived' ./internal/chat/   # expect FAIL
```

Expected: `milestone` and `milestone with no message` both fail with `MembershipText = "Member", want "Member for 6 months"`. Restore the order by hand, re-run the same command, expect `ok`. Record the observed failure lines.

- [ ] **Step 7: Package gates**

```bash
cd /d/Git/Moombox/.worktrees/chat-cards
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
gofmt -l ./internal/chat
go vet ./internal/chat/
staticcheck ./internal/chat/
go test -count=1 -timeout 600s ./internal/chat/
for f in internal/chat/types.go internal/chat/api.go internal/chat/api_test.go; do
  printf '%s ' "$f"; perl -0777 -ne 'print tr/\r//' "$f"; echo
done
```

Expected: `gofmt -l` and `staticcheck` silent; `ok` for the package; `0` beside each of the three paths.

- [ ] **Step 8: Commit**

```bash
cd /d/Git/Moombox/.worktrees/chat-cards
cat > /tmp/k-t1.msg <<'MSG'
feat(chat): archive the membership renderer's own header line

parseAction tested liveChatMembershipItemRenderer for its PRESENCE and threw
the renderer away, so everything a reader wants to see on a membership event
was discarded: a new member archived as a message with an empty message array,
a name and nothing else, and a milestone lost its "Member for 6 months" line.
Both live in headerPrimaryText / headerSubtext, which nothing read.

ChatMessage gains MembershipText, filled from headerPrimaryText when YouTube
sends one and from headerSubtext otherwise (a milestone's subtext is only the
tier name its primary text already spells). A separate field rather than a run
prepended to Message: the sidebar's member card wants the line beside the
author and the message in its body, and the video overlay renders Message
alone, so folding it in would start scrolling membership notices across the
video.

renderedText is the flat reader for YouTube's two text shapes — simpleText and
runs — both of which appear inside one membership renderer. parseMessageRuns
stays the reader for message bodies, where emoji, links and bold survive.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
MSG
git add internal/chat/types.go internal/chat/api.go internal/chat/api_test.go
git commit -F /tmp/k-t1.msg -- internal/chat/types.go internal/chat/api.go internal/chat/api_test.go
```

---

### Task 2: gifted memberships stop being dropped

Spec §2.1, second bullet. `selectRenderer` (`internal/chat/api.go:583-595`) knows four renderer keys and returns `nil` for everything else, and `parseAction:499` treats `nil` as "not a chat message we recognise". `liveChatSponsorshipsGiftPurchaseAnnouncementRenderer` (someone bought N memberships) and `liveChatSponsorshipsGiftRedemptionAnnouncementRenderer` (someone received one) are therefore **absent from every archive Moombox has ever written**.

The redemption renderer is flat and needs nothing but a roster entry: `authorName`, `authorBadges`, `timestampText` and a `message` runs array ("was gifted a membership by X") are all where `parseMessageRenderer` already looks. The purchase renderer is not: its `authorName`, `authorBadges` and `primaryText` sit one level down in `header.liveChatSponsorshipsHeaderRenderer`, and it carries no `timestampText` at all, so reading it raw yields author `"Unknown"` and no text.

**Files:**
- Modify: `internal/chat/api.go`
- Modify: `internal/chat/api_test.go`
- Modify: `internal/chat/helpers_test.go`

**Interfaces:**
- Consumes: `renderedText` (task 1), `ChatMessage.MembershipText` (task 1).
- Produces: `giftPurchaseFields(r map[string]any) (map[string]any, string)` (unexported, `api.go`).
- Unchanged: `parseMessageRenderer`'s signature and body, `parseSuperChatInfo`, `extractBadges`.

**Ruling — flatten the purchase renderer in `parseAction`, don't teach `parseMessageRenderer` a second field layout.** `parseMessageRenderer` is the one reader four renderer kinds already share, and an `if authorName is missing, look in header.liveChatSponsorshipsHeaderRenderer` fallback inside it would make every caller pay for one caller's shape. A nine-line hoist at the single call site keeps the shared reader shape-blind. **Cost if wrong:** if YouTube ever nests another renderer the same way, the hoist is copied rather than reused.

- [ ] **Step 1: Write the failing tests**

Append to `internal/chat/api_test.go`:

```go
// giftPurchaseItem builds a liveChatSponsorshipsGiftPurchaseAnnouncementRenderer
// in the shape YouTube ships: the id, timestampUsec and
// authorExternalChannelId at the top, and the author, badges and primaryText
// ("Gifted 5 memberships") one level down in
// header.liveChatSponsorshipsHeaderRenderer. There is no timestampText.
func giftPurchaseItem() map[string]any {
	return map[string]any{"addChatItemAction": map[string]any{
		"item": map[string]any{
			"liveChatSponsorshipsGiftPurchaseAnnouncementRenderer": map[string]any{
				"id":                      "gift-1",
				"timestampUsec":           "1700000000000000",
				"authorExternalChannelId": "UCgifter",
				"header": map[string]any{
					"liveChatSponsorshipsHeaderRenderer": map[string]any{
						"authorName": map[string]any{"simpleText": "gifter"},
						"authorBadges": []any{map[string]any{
							"liveChatAuthorBadgeRenderer": map[string]any{
								"tooltip": "Moderator",
								"icon":    map[string]any{"iconType": "MODERATOR"},
							},
						}},
						"primaryText": map[string]any{"runs": []any{
							map[string]any{"text": "Gifted "},
							map[string]any{"text": "5"},
							map[string]any{"text": " memberships"},
						}},
					},
				},
			},
		},
	}}
}

// giftRedemptionItem builds the recipient's side: flat, with the line in
// `message` and the author where parseMessageRenderer already looks.
func giftRedemptionItem() map[string]any {
	return map[string]any{"addChatItemAction": map[string]any{
		"item": map[string]any{
			"liveChatSponsorshipsGiftRedemptionAnnouncementRenderer": map[string]any{
				"id":            "redeem-1",
				"timestampUsec": "1700000000000000",
				"timestampText": map[string]any{"simpleText": "1:02:03"},
				"authorName":    map[string]any{"simpleText": "lucky"},
				"message": map[string]any{"runs": []any{
					map[string]any{"text": "was gifted a membership by gifter"},
				}},
			},
		},
	}}
}

// TestGiftPurchaseIsArchivedWithItsHeaderFields: the purchase renderer was not
// in selectRenderer's roster at all, so every gifted membership Moombox has
// ever archived is missing. Adding the key alone is not enough — the author,
// the badges and the line all live one level down, so a raw read yields
// author "Unknown" and no text.
//
// Mutants this kills: adding the roster key without the hoist (AuthorName
// comes back "Unknown" and MembershipText ""); hoisting authorName but not
// authorBadges (the moderator badge is gone); losing id or timestampUsec in
// the hoist (the ID falls back to a random one and the offset arithmetic loses
// its clock).
func TestGiftPurchaseIsArchivedWithItsHeaderFields(t *testing.T) {
	api := NewChatAPI("k", "", nil)
	msg := api.parseAction(giftPurchaseItem())
	if msg == nil {
		t.Fatal("a gift purchase announcement was dropped")
	}
	if !msg.IsMembership {
		t.Error("a gift purchase is a membership event")
	}
	if msg.MembershipText != "Gifted 5 memberships" {
		t.Errorf("MembershipText = %q, want %q", msg.MembershipText, "Gifted 5 memberships")
	}
	if msg.AuthorName != "gifter" {
		t.Errorf("AuthorName = %q, want %q — the name is in the nested header", msg.AuthorName, "gifter")
	}
	if msg.ID != "gift-1" {
		t.Errorf("ID = %q, want gift-1 — the hoist must keep the outer id", msg.ID)
	}
	if msg.TimestampUsec != "1700000000000000" {
		t.Errorf("TimestampUsec = %q, want the outer one", msg.TimestampUsec)
	}
	if msg.AuthorChannelID != "UCgifter" {
		t.Errorf("AuthorChannelID = %q, want UCgifter", msg.AuthorChannelID)
	}
	var hasMod bool
	for _, b := range msg.AuthorBadges {
		if b == "moderator" {
			hasMod = true
		}
	}
	if !hasMod {
		t.Errorf("AuthorBadges = %v, want the nested header's moderator badge", msg.AuthorBadges)
	}
	if len(msg.Message) != 0 {
		t.Errorf("Message = %v, want empty — a purchase has no typed message", msg.Message)
	}
}

// TestGiftRedemptionIsArchived: the recipient's side is flat, so the roster
// entry is the whole fix — its line is in `message`, which parseMessageRenderer
// already reads, and it has no header text of its own.
//
// Mutant this kills: dropping the roster key (parseAction returns nil and the
// event is invisible); routing it through the purchase hoist (author and
// message both come back empty).
func TestGiftRedemptionIsArchived(t *testing.T) {
	api := NewChatAPI("k", "", nil)
	msg := api.parseAction(giftRedemptionItem())
	if msg == nil {
		t.Fatal("a gift redemption announcement was dropped")
	}
	if !msg.IsMembership {
		t.Error("a gift redemption is a membership event")
	}
	if msg.AuthorName != "lucky" {
		t.Errorf("AuthorName = %q, want lucky", msg.AuthorName)
	}
	if msg.MembershipText != "" {
		t.Errorf("MembershipText = %q, want empty — the redemption's line is its message", msg.MembershipText)
	}
	var got string
	for _, p := range msg.Message {
		got += p.Text
	}
	if got != "was gifted a membership by gifter" {
		t.Errorf("Message = %q, want the redemption line", got)
	}
}
```

And extend the existing `TestSelectRendererPicksFirstMatch` table (`internal/chat/api_test.go:277-287`) with two rows, after `{"membership", "liveChatMembershipItemRenderer"}`:

```go
		{"gift purchase", "liveChatSponsorshipsGiftPurchaseAnnouncementRenderer"},
		{"gift redemption", "liveChatSponsorshipsGiftRedemptionAnnouncementRenderer"},
```

- [ ] **Step 2: Run the tests and see them RED**

```bash
cd /d/Git/Moombox/.worktrees/chat-cards
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go test -count=1 -timeout 300s -run 'TestGiftPurchaseIsArchivedWithItsHeaderFields|TestGiftRedemptionIsArchived|TestSelectRendererPicksFirstMatch' -v ./internal/chat/
```

Expected: **FAIL**, three failures —
- `TestGiftPurchaseIsArchivedWithItsHeaderFields` → `a gift purchase announcement was dropped`
- `TestGiftRedemptionIsArchived` → `a gift redemption announcement was dropped`
- `TestSelectRendererPicksFirstMatch/gift_purchase` and `/gift_redemption` → `expected non-nil renderer for liveChatSponsorships…`

- [ ] **Step 3: Grow the roster and add the hoist**

In `internal/chat/api.go`, extend `selectRenderer`'s key list (`:584-589`) to six, in the order YouTube's own action items most often take:

```go
	for _, key := range []string{
		"liveChatTextMessageRenderer",
		"liveChatPaidMessageRenderer",
		"liveChatPaidStickerRenderer",
		"liveChatMembershipItemRenderer",
		"liveChatSponsorshipsGiftPurchaseAnnouncementRenderer",
		"liveChatSponsorshipsGiftRedemptionAnnouncementRenderer",
	} {
```

Add `giftPurchaseFields` immediately after `selectRenderer`:

```go
// giftPurchaseFields hoists a liveChatSponsorshipsGiftPurchaseAnnouncementRenderer
// into the field layout parseMessageRenderer reads, and returns its header
// line alongside.
//
// The purchase renderer is the one membership shape that is not flat: the id,
// timestampUsec and authorExternalChannelId are at the top, but authorName,
// authorBadges and the primaryText ("Gifted 5 memberships") are one level down
// in header.liveChatSponsorshipsHeaderRenderer, and there is no timestampText
// at all (parseMessageRenderer's formatTimestamp fallback covers that). Read
// raw it produces author "Unknown" and no text.
//
// Flattening here rather than teaching parseMessageRenderer a second layout:
// that reader is shared by every renderer kind, and a nested-author fallback
// inside it would make five callers pay for one. Returns (nil, "") when the
// header is missing, which the caller treats as "leave the renderer alone".
func giftPurchaseFields(r map[string]any) (map[string]any, string) {
	header, _ := r["header"].(map[string]any)
	sponsor, _ := header["liveChatSponsorshipsHeaderRenderer"].(map[string]any)
	if sponsor == nil {
		return nil, ""
	}
	flat := map[string]any{
		"id":                      r["id"],
		"timestampUsec":           r["timestampUsec"],
		"authorExternalChannelId": r["authorExternalChannelId"],
		"authorName":              sponsor["authorName"],
		"authorBadges":            sponsor["authorBadges"],
	}
	return flat, renderedText(sponsor["primaryText"])
}
```

Note `header["…"]` on a nil map is a legal read in Go and yields the zero value, so the two type assertions need no separate nil check between them.

Then make two separate edits inside `parseAction`. **The first is the hoist**, and it does NOT touch the membership branch: it is inserted between `selectRenderer`'s nil guard (`:499-501`) and `msg := api.parseMessageRenderer(renderer)` (`:503`), so the shared reader receives the flattened map. The `msg := …` lines are shown only to mark the join point — do not duplicate them:

```go
	var giftLine string
	if gift, ok := item["liveChatSponsorshipsGiftPurchaseAnnouncementRenderer"].(map[string]any); ok {
		if flat, line := giftPurchaseFields(gift); flat != nil {
			renderer, giftLine = flat, line
		}
	}

	msg := api.parseMessageRenderer(renderer)
	if msg == nil {
		return nil
	}
```

**The second edit replaces the membership branch task 1 wrote** (further down, after the paid/sticker branch, which is unchanged) with the three-way form:

```go
	if memb, ok := item["liveChatMembershipItemRenderer"].(map[string]any); ok {
		msg.IsMembership = true
		// The event itself: "Member for 6 months" when YouTube sends a primary
		// text, else the subtext ("Welcome to Member!"), which is all a
		// new-member renderer has. Never both — a milestone's subtext is just
		// the tier name the primary text already names.
		msg.MembershipText = renderedText(memb["headerPrimaryText"])
		if msg.MembershipText == "" {
			msg.MembershipText = renderedText(memb["headerSubtext"])
		}
	}
	// A gifted membership is a membership event too: the purchase carries its
	// line in the nested header (hoisted above), and the redemption carries it
	// in `message`, which parseMessageRenderer has already read.
	if _, ok := item["liveChatSponsorshipsGiftPurchaseAnnouncementRenderer"]; ok {
		msg.IsMembership = true
		msg.MembershipText = giftLine
	}
	if _, ok := item["liveChatSponsorshipsGiftRedemptionAnnouncementRenderer"]; ok {
		msg.IsMembership = true
	}
```

Finally, correct the stale roster claim in `internal/chat/helpers_test.go:104-109`. Replace

```go
// TestSelectRendererSuperChatPaidMessageBranch — selectRenderer's
// switch handles 5 distinct renderer types (chatTextMessageRenderer,
// chatPaidMessageRenderer, chatPaidStickerRenderer,
// chatMembershipItemRenderer, chatSponsorshipsGiftPurchaseAnnouncement
// Renderer). Existing tests cover the happy path; this test locks the
// paid-message branch which feeds parseSuperChatInfo.
```

with

```go
// TestSelectRendererSuperChatPaidMessageBranch — selectRenderer's roster is
// six renderer types: liveChatTextMessageRenderer, liveChatPaidMessageRenderer,
// liveChatPaidStickerRenderer, liveChatMembershipItemRenderer,
// liveChatSponsorshipsGiftPurchaseAnnouncementRenderer and
// liveChatSponsorshipsGiftRedemptionAnnouncementRenderer.
// TestSelectRendererPicksFirstMatch walks all six; this test locks the
// paid-message branch specifically, which is the one that feeds
// parseSuperChatInfo.
//
// (Until 2026-09-25 this comment claimed five types including the gift
// purchase, which selectRenderer did not know — the roster had four and every
// gifted membership was dropped.)
```

- [ ] **Step 4: Run the tests and see them GREEN**

```bash
cd /d/Git/Moombox/.worktrees/chat-cards
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go test -count=1 -timeout 300s -run 'TestGift|TestSelectRenderer|TestMembershipHeaderTextIsArchived|TestParseAction|TestParseMessageRoutes' -v ./internal/chat/
```

Expected: `ok`, with all six `TestSelectRendererPicksFirstMatch` subtests, both gift tests, task 1's four membership subtests and the two superchat routing pins (`TestParseMessageRoutesStickersToTheStickerParser`, `TestParseActionRoutesPaidMessagesToTheMessageParser`) green — the last two are the witnesses that the hoist inserted between `selectRenderer` and `parseMessageRenderer` did not disturb the paid path.

- [ ] **Step 5: Verify a mutant by execution**

By hand, make `giftPurchaseFields` drop `authorBadges` from the flattened map (delete the `"authorBadges": sponsor["authorBadges"],` line):

```bash
cd /d/Git/Moombox/.worktrees/chat-cards
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go test -count=1 -timeout 300s -run 'TestGiftPurchaseIsArchivedWithItsHeaderFields' ./internal/chat/   # expect FAIL
```

Expected: `AuthorBadges = [], want the nested header's moderator badge`. Restore the line by hand, re-run, expect `ok`. Record the observed failure line.

- [ ] **Step 6: Package gates**

```bash
cd /d/Git/Moombox/.worktrees/chat-cards
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
gofmt -l ./internal/chat
go vet ./internal/chat/
staticcheck ./internal/chat/
go test -count=1 -timeout 600s ./internal/chat/
go test -count=1 -timeout 600s -race ./internal/chat/
for f in internal/chat/api.go internal/chat/api_test.go internal/chat/helpers_test.go; do
  printf '%s ' "$f"; perl -0777 -ne 'print tr/\r//' "$f"; echo
done
```

Expected: `gofmt -l` and `staticcheck` silent; both test runs `ok` with no `WARNING: DATA RACE`; `0` beside each path.

- [ ] **Step 7: Commit**

```bash
cd /d/Git/Moombox/.worktrees/chat-cards
cat > /tmp/k-t2.msg <<'MSG'
feat(chat): archive gifted memberships instead of dropping them

selectRenderer knew four renderer keys and parseAction treats an unknown one
as "not a chat message", so liveChatSponsorshipsGiftPurchaseAnnouncementRenderer
and liveChatSponsorshipsGiftRedemptionAnnouncementRenderer were dropped
silently: no gifted membership appears in any archive Moombox has written.
Both are membership events and both now land.

The redemption side is flat — its line is in `message`, its author where
parseMessageRenderer already looks — so the roster entry is the whole fix. The
purchase side is not: id, timestampUsec and authorExternalChannelId sit at the
top while authorName, authorBadges and the primaryText ("Gifted 5
memberships") are one level down in header.liveChatSponsorshipsHeaderRenderer,
and there is no timestampText at all. giftPurchaseFields hoists those at the
single call site rather than teaching the shared parseMessageRenderer a second
field layout for one caller's sake.

helpers_test.go's comment claimed a five-type roster including the gift
purchase, written against an intention the code never had. It now names all
six and records what was actually there.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
MSG
git add internal/chat/api.go internal/chat/api_test.go internal/chat/helpers_test.go
git commit -F /tmp/k-t2.msg -- internal/chat/api.go internal/chat/api_test.go internal/chat/helpers_test.go
```

---

### Task 3 (T-A): the YouTube Super Chat / Super Sticker card

Spec §0 K1, §2 item 1. Today a Super Chat is a flat row that gains one amber left border and a bold amount span (`web/public/modules/player.js:1120-1141`, `web/public/moombox.css:1743-1745` and `:1777-1781`); `tier`, `kind`, `headerColor` and `bodyColor` — all four archived by `internal/chat`'s `SuperchatInfo` — are never read.

This task also does the arc's only restructuring, so tasks 4–6 can be additive.

**Files:**
- Modify: `web/public/modules/player.js`
- Modify: `web/public/moombox.css`
- Modify: `web/tests/player.test.mjs`

**Interfaces:**
- Produces (module scope, exported): `SUPERCHAT_TIER_COLORS`, `relativeLuminance(hex)`, `readableInk(hex)`. Produces (module scope, private): `HEX_RE`, `isHexColor(s)`, `resolvedColor(archived, fallback)`.
- Produces (methods): `_timeSpan(msg)`, `_authorSpan(msg, withColon)`, `_cardParts(div, headerColor, bodyColor)`, `_fillPlainRow(div, msg)`, `_fillSuperchatCard(div, msg)`.
- Consumes: `formatMsToTime`, `dividerLabelFor`, `announcementColorClass`, `this.appendChatContent` — all already in the file.
- Unchanged: `_buildNicoEl`, `appendChatContent`, `_appendTwitchMessage`, `_createEmoteImg`, every timeline method.

**Ruling — the tier palette lives in JS, not in a CSS `[data-tier]` block.** Spec §2 item 1 suggested CSS. It cannot work: the card's ink is derived from the **effective** body colour by the luminance rule, so if the fallback colour only existed in the stylesheet, JS could not compute the ink for a pre-2026-09-05 archive and a second, divergent per-tier ink table would have to live in CSS beside it. One table, one resolver, and every case becomes assertable in jsdom (which applies no stylesheets at all, so a CSS-only palette would be untestable). `data-tier` is still stamped on the element as a styling hook and a test handle. **Cost if wrong:** a future re-skin edits JS rather than CSS.

**Ruling — the amount is shown verbatim; `currency` is not appended.** `SuperchatInfo.Amount` is YouTube's rendered `purchaseAmountText` ("$5.00") and `SuperchatInfo.Currency` is derived from it by `extractCurrency` (`internal/chat/api.go:961`). "$5.00 USD" would be a display string restating a machine field — the thing the project's "no redundant label fields" rule forbids. **Cost if wrong:** an archive whose amount somehow lacks a symbol shows a bare number.

**Ruling — the outer div keeps the `superchat` class, but its CSS rule goes.** The class is what a reader greps for and what a future "paid only" filter would use, so it stays on the element. Its one declaration — the amber `border-left` — has nothing to become: no element carries `.superchat` without `.chat-card` any more, and the card supplies the whole look. The rule is deleted rather than given a placeholder value.

**Ruling — one ink per card, derived from the BODY colour, applied to both halves.** Spec §2 item 1 says the implementation "derives it from the body colour's luminance", and YouTube paints a tier's header and body text the same colour. Deriving each half from its own background instead would split four of the eight tiers' cards in two: only tier 4's header is light enough for dark ink, so a tier-3 card would read white-on-header over dark-on-body and a tier-4 card the reverse. **Cost if wrong:** a hypothetical archived pair whose header is much lighter than its body gets one low-contrast half.

**Measured, recorded here so the implementer does not have to re-derive it** (all executed against this task's own `relativeLuminance`, 2026-09-25). Body colours, tiers 0-7: 0.178, 0.235, 0.633, 0.619, 0.637, 0.338, 0.192, 0.180 — so a 0.5 threshold reproduces YouTube's own choice exactly, dark ink on 2, 3 and 4 and white on 0, 1, 5, 6 and 7, with no second table. Header colours, tiers 0-7: 0.117, 0.133, 0.390, 0.400, 0.535, 0.227, 0.129, 0.134 — recorded only to show why they are NOT the input: on seven of eight tiers a header-derived ink would disagree with its own body. Tier 0's gray pair is #606060 / #757575. Sanity anchors: #FFFFFF = 1.0000, #000000 = 0.0000, #808080 = 0.2159.

- [ ] **Step 1: Write the failing tests**

Append to `web/tests/player.test.mjs`. First add the two colour-helper imports to the file's header — they go **beside** the existing imports at the very top, before the jsdom probe, because importing `player.js` needs no DOM (the harness itself imports it at module scope before any jsdom exists):

```js
import { relativeLuminance, readableInk, SUPERCHAT_TIER_COLORS } from "../public/modules/player.js";
```

Then append the section:

```js
// ── Sidebar chat cards (Arc K) ──────────────────────────────────────────────

/**
 * A YouTube-shaped chat file. No `platform`, so correctLegacyTwitchEmotes is
 * skipped, and no `streamStartTime`, so deriveMissingOffsets returns at once —
 * the offsets below are used exactly as written.
 */
const ytChat = (messages) => ({ messages });

/** One YouTube message: `message` is the MessagePart[] internal/chat writes. */
const ytMsg = (extra = {}) => ({
  offsetMs: 1000, authorName: "Viewer",
  message: [{ type: "text", text: "hello" }], ...extra,
});

/** Build a player showing exactly these messages, overlay off, sidebar on. */
async function showChat(messages) {
  const h = harness.makePlayer({
    jobs: [finished("j1", { chatFilename: "chat.json" })],
    watchState: {},
    chat: ytChat(messages),
    storage: { "player-nico-toggle": "false", "player-sidebar-toggle": "true" },
  });
  await h.selectJob("j1");
  return h;
}

/** A Super Chat message: `superchat` carries internal/chat's SuperchatInfo. */
const superchatMsg = (superchat, extra = {}) => ytMsg({ superchat, ...extra });

// These two need no DOM: player.js imports nothing that touches `document` at
// module scope (helpers/player-dom.mjs imports it before any jsdom exists).
// They carry no `skip` for that reason — the pattern a11y-controls.test.mjs's
// stylesheet test already sets in a jsdom suite.
//
// MUTANT: move the threshold to 0.49 or 0.51 and exactly one of the two
// boundary colours below flips. MUTANT: drop the sRGB linearisation and use the
// raw channel average — #BBBBBB reads 0.733 and turns dark.
test("relativeLuminance and readableInk flip at 0.5, on the WCAG curve", () => {
  assert.equal(relativeLuminance("#000000"), 0);
  assert.equal(relativeLuminance("#FFFFFF"), 1);
  assert.equal(relativeLuminance("not a colour"), null);
  assert.equal(relativeLuminance("#FFF"), null, "only the six-digit form the archive writes");
  assert.equal(relativeLuminance(undefined), null);

  // The boundary pair: #BBBBBB is 0.4969 and #BCBCBC is 0.5029.
  assert.ok(relativeLuminance("#BBBBBB") < 0.5);
  assert.ok(relativeLuminance("#BCBCBC") >= 0.5);
  assert.equal(readableInk("#BBBBBB"), "light");
  assert.equal(readableInk("#BCBCBC"), "dark");
  assert.equal(readableInk("garbage"), "light", "an unreadable colour defaults to the safe ink");
});

// MUTANT: swap any tier's header and body, or copy a neighbour's hex, and the
// ink this asserts moves — these are the four YouTube paints dark and the four
// it paints white, derived rather than tabulated.
test("every Super Chat tier's palette lands on YouTube's own ink", () => {
  const ink = (t) => readableInk(SUPERCHAT_TIER_COLORS[t].body);
  assert.deepEqual([0, 1, 2, 3, 4, 5, 6, 7].map(ink),
    ["light", "light", "dark", "dark", "dark", "light", "light", "light"]);
  assert.equal(SUPERCHAT_TIER_COLORS[3].header, "#00BFA5");
  assert.equal(SUPERCHAT_TIER_COLORS[7].body, "#E62117");
});

test("a Super Chat is a two-part card in the colours the archive recorded", { skip }, async () => {
  const h = await showChat([superchatMsg(
    { amount: "$5.00", currency: "USD", color: "green", tier: 3, kind: "message",
      headerColor: "#00BFA5", bodyColor: "#1DE9B6" },
    { authorName: "Payer", message: [{ type: "text", text: "thank you" }] },
  )]);
  const row = h.sidebar().children[0];
  assert.ok(row.classList.contains("chat-msg"), "a card is still a timeline row");
  assert.ok(row.classList.contains("chat-card"));
  assert.ok(row.classList.contains("superchat"));
  assert.equal(row.dataset.tier, "3");
  assert.equal(row.style.getPropertyValue("--card-header"), "#00BFA5");
  assert.equal(row.style.getPropertyValue("--card-body"), "#1DE9B6");

  const header = row.querySelector(".chat-card-header");
  const body = row.querySelector(".chat-card-body");
  // Tier 3's body green (#1DE9B6, luminance 0.619) takes dark ink, and the
  // header takes the same one — one ink per card, derived from the body.
  // MUTANT: derive each half from its own colour and the header flips to
  // chat-ink-light, because #00BFA5 is only 0.400.
  assert.ok(header.classList.contains("chat-ink-dark"), header.className);
  assert.ok(body.classList.contains("chat-ink-dark"), body.className);
  assert.equal(header.querySelector(".chat-msg-author").textContent, "Payer",
    "a card header shows the name, not the flat row's 'Name: ' prefix");
  assert.equal(header.querySelector(".chat-msg-superchat").textContent, "$5.00",
    "the amount is shown as archived; `currency` restates it and is not appended");
  assert.equal(header.lastChild.className, "chat-msg-time",
    "the time sits at the card's top-right");
  assert.equal(body.textContent, "thank you");
});

// MUTANT: prefer the palette over the archived pair (the test above fails);
// MUTANT: ignore the palette when the pair is absent (this one fails — the
// custom properties come back empty and the ink defaults to light).
test("a Super Chat with no archived colours falls back to the tier palette", { skip }, async () => {
  const h = await showChat([superchatMsg({ amount: "£100.00", tier: 7, kind: "message" },
    { message: [{ type: "text", text: "big one" }] })]);
  const row = h.sidebar().children[0];
  assert.equal(row.style.getPropertyValue("--card-header"), "#D00000");
  assert.equal(row.style.getPropertyValue("--card-body"), "#E62117");
  assert.ok(row.querySelector(".chat-card-body").classList.contains("chat-ink-light"));
});

// MUTANT: treat a malformed archived colour as usable — the card paints
// `--card-header: rgb(0,191,165)` (a form no CSS var consumer of ours writes)
// and the ink is computed from nothing.
test("an unparseable archived colour is treated as absent", { skip }, async () => {
  const h = await showChat([superchatMsg(
    { amount: "$2.00", tier: 2, kind: "message", headerColor: "rgb(0,191,165)", bodyColor: "" })]);
  const row = h.sidebar().children[0];
  assert.equal(row.style.getPropertyValue("--card-header"), "#00B8D4");
  assert.equal(row.style.getPropertyValue("--card-body"), "#00E5FF");
});

// MUTANT: render the (unarchived) sticker image, or leave the body empty — a
// sticker becomes an unexplained blank card.
test("a Super Sticker says so in place of the image it does not archive", { skip }, async () => {
  const h = await showChat([
    superchatMsg({ amount: "$2.00", tier: 2, kind: "sticker",
                   headerColor: "#00B8D4", bodyColor: "#00E5FF" }, { message: [] }),
    // An archive written before `kind` existed (it arrived 2026-09-05): a paid
    // message with no parts at all is a sticker in everything but the label.
    superchatMsg({ amount: "$2.00", tier: 2 }, { offsetMs: 2000, message: [] }),
  ]);
  assert.equal(h.sidebar().children[0].querySelector(".chat-card-body").textContent, "Super Sticker");
  assert.equal(h.sidebar().children[1].querySelector(".chat-card-body").textContent, "Super Sticker");
});

// MUTANT: drop the tier clamp — an archive with tier 9 (or a string) indexes
// SUPERCHAT_TIER_COLORS to undefined and the builder throws mid-chunk, taking
// the whole sidebar build with it.
test("an unresolved tier gets the neutral gray card", { skip }, async () => {
  const h = await showChat([
    superchatMsg({ amount: "¥500", color: "gray", tier: 0, kind: "message" }),
    superchatMsg({ amount: "¥500", tier: 9 }, { offsetMs: 2000 }),
  ]);
  for (const i of [0, 1]) {
    const row = h.sidebar().children[i];
    assert.equal(row.dataset.tier, "0");
    assert.equal(row.style.getPropertyValue("--card-body"), "#757575");
  }
});

// The pin behind the "cards keep `chat-msg`" decision. The sidebar promotes,
// dims, divides and measures rows by index and by that class; a card that
// dropped it would still be promoted (the index walk checks no class) but would
// lose every .chat-msg rule in the stylesheet and, here, its measured box.
//
// MUTANT: build the card as a bare <div class="chat-card"> — offsetTop comes
// back 0 for every row (helpers/player-dom.mjs's measure() keys on `chat-msg`)
// and the divider/future assertions fail.
test("a card is still a timeline row: future, active, divider, measured", { skip }, async () => {
  const h = await showChat([
    ytMsg({ offsetMs: -5000, message: [{ type: "text", text: "waiting room" }] }),
    superchatMsg({ amount: "$5.00", tier: 3 }, { offsetMs: 1000 }),
  ]);
  const rows = h.sidebar().children;
  assert.ok(rows[1].classList.contains("divider-before"),
    "the card is the first in-video row, so it carries the region divider");
  assert.equal(rows[1].dataset.divider, "Waiting room — 1 messages before the stream");
  assert.ok(rows[1].classList.contains("future"));
  h.tick(2000);
  assert.ok(rows[1].classList.contains("active"), "a card is promoted like any row");
  assert.ok(rows[1].offsetTop > 0, "a card is measured like any row");
});

// The regression pin for the extraction: a message with no superchat must come
// out byte-for-byte as before. MUTANT: drop the "Name: " colon, reorder the
// spans, or lose the announcement classes.
test("an ordinary message is unchanged by the card dispatch", { skip }, async () => {
  const h = await showChat([
    ytMsg({ authorName: "Plain", authorBadges: ["moderator"],
            message: [{ type: "text", text: "hi" }] }),
    { offsetMs: 2000, authorName: "Ann", message: "announced",
      messageType: "announcement", announcementColor: "blue" },
  ]);
  const plain = h.sidebar().children[0];
  assert.equal(plain.className, "chat-msg future");
  assert.deepEqual([...plain.children].map((c) => c.className),
    ["chat-msg-time", "chat-msg-author moderator", ""]);
  assert.equal(plain.children[1].textContent, "Plain: ");
  assert.equal(plain.children[2].textContent, "hi");
  const ann = h.sidebar().children[1];
  assert.ok(ann.classList.contains("announcement"));
  assert.ok(ann.classList.contains("announcement-blue"));
});
```

- [ ] **Step 2: Run the tests and see them RED**

```bash
cd /d/Git/Moombox/.worktrees/chat-cards/web/tests
timeout 300 node --test player.test.mjs 2>&1 | tail -30
```

Expected: the file fails to load — `SyntaxError: The requested module '../public/modules/player.js' does not provide an export named 'SUPERCHAT_TIER_COLORS'` — which is the honest red for a test naming symbols this task creates. Node names the **last** unresolved binding in the import list, not the first, so the symbol quoted is whichever this plan's import line ends with. Record the exact message. (If Node reports the whole file as one failure rather than per-test, that is expected for an import-time error.)

- [ ] **Step 3: Add the colour model to `player.js`**

Insert after `announcementColorClass` (`web/public/modules/player.js:23-25`), before `focusPlayerSurface`:

```js
/**
 * YouTube's Super Chat palette: each tier's header and body colour, 1 (blue,
 * $1) through 7 (red, $100+), plus a neutral gray pair for tier 0 — the
 * archive's marker for a colour pair internal/chat's table did not know
 * (SuperchatInfo.Tier).
 *
 * The table lives here rather than in a CSS [data-tier] block because a card's
 * INK is derived from the colour actually painted: a stylesheet-only fallback
 * would leave this file unable to compute the ink for an archive written
 * before headerColor/bodyColor were recorded, and a second per-tier ink table
 * in CSS would then have to agree with this one forever. `data-tier` is still
 * stamped on the element as a styling hook.
 */
export const SUPERCHAT_TIER_COLORS = {
  0: { header: "#606060", body: "#757575" },
  1: { header: "#1565C0", body: "#1E88E5" },
  2: { header: "#00B8D4", body: "#00E5FF" },
  3: { header: "#00BFA5", body: "#1DE9B6" },
  4: { header: "#FFB300", body: "#FFCA28" },
  5: { header: "#E65100", body: "#F57C00" },
  6: { header: "#C2185B", body: "#E91E63" },
  7: { header: "#D00000", body: "#E62117" },
};

/** internal/chat's argbHex writes exactly #RRGGBB; nothing else is a colour. */
const HEX_RE = /^#[0-9a-fA-F]{6}$/;

function isHexColor(s) {
  return typeof s === "string" && HEX_RE.test(s);
}

/**
 * WCAG 2.x relative luminance of an #RRGGBB colour, or null when the string is
 * not one (an old or malformed archive), which every caller reads as "no
 * colour recorded".
 * @param {string} hex
 * @returns {number|null}
 */
export function relativeLuminance(hex) {
  if (!isHexColor(hex)) return null;
  const chan = (i) => {
    const c = parseInt(hex.slice(1 + i * 2, 3 + i * 2), 16) / 255;
    return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
  };
  return 0.2126 * chan(0) + 0.7152 * chan(1) + 0.0722 * chan(2);
}

/**
 * Which ink reads on `hex`: "dark" at or above a relative luminance of 0.5,
 * "light" below it and for anything unparseable (the safe default on a
 * saturated card).
 *
 * Measured: the seven tier body colours come out at 0.235, 0.633, 0.619,
 * 0.637, 0.338, 0.192 and 0.180, so this one threshold reproduces YouTube's
 * own choice — dark text on tiers 2, 3 and 4, white on 1, 5, 6 and 7 — without
 * a second hard-coded table, and an archived colour YouTube has never shipped
 * still reads.
 * @param {string} hex
 * @returns {"light"|"dark"}
 */
export function readableInk(hex) {
  const l = relativeLuminance(hex);
  return l !== null && l >= 0.5 ? "dark" : "light";
}

/** The colour actually painted: the archived one when usable, else the fallback. */
function resolvedColor(archived, fallback) {
  return isHexColor(archived) ? archived : fallback;
}
```

- [ ] **Step 4: Restructure `_buildChatMessageEl` and add the card**

Replace the whole body of `_buildChatMessageEl` (`web/public/modules/player.js:1105-1163`, from `const div = document.createElement("div");` to the closing `return div;`) with:

```js
    const div = document.createElement("div");
    div.className = index < this.playerActiveChatIndex ? "chat-msg active" : "chat-msg future";
    div.dataset.offset = msg.offsetMs;

    // Region boundary: the divider is ::before pseudo-content on the first row
    // of the region, so the children[i] === playerChatMessages[i] alignment
    // that filterChat / resetSidebarToTime / updateSidebarActiveState rely on
    // survives (a real divider element would shift every index after it).
    const dividerLabel = dividerLabelFor(this._chatParts, index);
    if (dividerLabel) {
      div.classList.add("divider-before");
      div.dataset.divider = dividerLabel;
    }

    // Shape dispatch. Every branch fills the SAME element: one direct child of
    // #player-sidebar-messages per message, still carrying `chat-msg`. The
    // sidebar's promotion, reset, post-end marking, divider reconciliation,
    // search filter and scroll maths all address rows by container.children[i]
    // and would not notice the class going — but every `.chat-msg.<state>`
    // rule in moombox.css would, and so would the jsdom harness's measured box.
    if (msg.superchat) {
      this._fillSuperchatCard(div, msg);
      return div;
    }
    this._fillPlainRow(div, msg);
    return div;
```

Then add, immediately after `_buildChatMessageEl`:

```js
  /**
   * The ordinary flat row: time, author, content. Extracted verbatim from
   * _buildChatMessageEl; the Super Chat class and amount span it used to carry
   * moved into _fillSuperchatCard, which is now the only shape that reaches
   * them.
   */
  _fillPlainRow(div, msg) {
    if (msg.messageType === "announcement") {
      div.classList.add("announcement");
      div.classList.add(`announcement-${announcementColorClass(msg.announcementColor)}`);
    }
    div.appendChild(this._timeSpan(msg));
    div.appendChild(this._authorSpan(msg, true));
    const contentSpan = document.createElement("span");
    this.appendChatContent(contentSpan, msg.message || [], msg.emotes);
    div.appendChild(contentSpan);
  }

  /** The row's offset timestamp. */
  _timeSpan(msg) {
    const span = document.createElement("span");
    span.className = "chat-msg-time";
    span.textContent = formatMsToTime(msg.offsetMs);
    return span;
  }

  /**
   * The author span with its badge class. `withColon` is the flat row's
   * "Name: " prefix; a card header puts the name on its own line and drops it.
   */
  _authorSpan(msg, withColon) {
    const authorSpan = document.createElement("span");
    authorSpan.className = "chat-msg-author";
    if (msg.authorBadges && Array.isArray(msg.authorBadges)) {
      // Twitch badges use "type/tier" format (e.g. "subscriber/12"), so check prefix
      const hasBadge = (name) => msg.authorBadges.some((b) => b === name || b.startsWith(name + "/"));
      if (hasBadge("owner") || hasBadge("broadcaster")) authorSpan.classList.add("owner");
      else if (hasBadge("moderator")) authorSpan.classList.add("moderator");
      else if (hasBadge("member") || hasBadge("subscriber")) authorSpan.classList.add("member");
      else if (hasBadge("vip")) authorSpan.classList.add("member");
    }
    authorSpan.textContent = withColon ? msg.authorName + ": " : msg.authorName;
    return authorSpan;
  }

  /**
   * Turn `div` into a two-part card and hand back its header and body.
   *
   * The colours ride as CSS custom properties written through the CSSOM. A
   * setProperty write is not an inline <style> element and is not governed by
   * style-src (which internal/web/middleware.go grants 'unsafe-inline' anyway,
   * for Shoelace's shadow DOM), so nothing about the CSP moves.
   *
   * ONE ink for the whole card, derived from the BODY colour — the half that
   * carries the message, and the one YouTube picks its text colour from. Both
   * halves take it, as they do on YouTube: a header strip is always the darker
   * partner of its body, so deriving each half separately would put dark text
   * on a tier-4 header (#FFB300, luminance 0.535) above white text on its own
   * body, and light text on a tier-3 header (#00BFA5, 0.400) above dark text
   * on its body — a card that changes ink halfway down. Measured header
   * luminances, tiers 0-7: 0.117, 0.133, 0.390, 0.400, 0.535, 0.227, 0.129,
   * 0.134 — only tier 4's would disagree with its body.
   */
  _cardParts(div, headerColor, bodyColor) {
    div.classList.add("chat-card");
    div.style.setProperty("--card-header", headerColor);
    div.style.setProperty("--card-body", bodyColor);
    const ink = `chat-ink-${readableInk(bodyColor)}`;
    const header = document.createElement("div");
    header.className = `chat-card-header ${ink}`;
    const body = document.createElement("div");
    body.className = `chat-card-body ${ink}`;
    div.appendChild(header);
    div.appendChild(body);
    return { header, body };
  }

  /**
   * K1: a Super Chat or Super Sticker as YouTube draws it — a header strip in
   * the tier's header colour carrying the author, the amount and the time, and
   * the message in the body colour beneath it.
   *
   * The archived headerColor/bodyColor win; SUPERCHAT_TIER_COLORS is the
   * fallback for a file written before internal/chat recorded them (or with a
   * pair its table did not know, which arrives as tier 0). The amount is shown
   * exactly as archived — SuperchatInfo.Currency is derived from that same
   * string, so appending it would restate it.
   */
  _fillSuperchatCard(div, msg) {
    const sc = msg.superchat || {};
    const tier = SUPERCHAT_TIER_COLORS[sc.tier] ? sc.tier : 0;
    const palette = SUPERCHAT_TIER_COLORS[tier];
    div.classList.add("superchat");
    div.dataset.tier = String(tier);
    const { header, body } = this._cardParts(
      div,
      resolvedColor(sc.headerColor, palette.header),
      resolvedColor(sc.bodyColor, palette.body),
    );

    header.appendChild(this._authorSpan(msg, false));
    const amount = document.createElement("span");
    amount.className = "chat-msg-superchat";
    amount.textContent = sc.amount || "";
    header.appendChild(amount);
    header.appendChild(this._timeSpan(msg));

    // A Super Sticker's image is not archived, so the body says what it was.
    // `kind` arrived with the tier fix (2026-09-05); an older file has none,
    // and a paid message with no parts at all is a sticker in all but name.
    const parts = Array.isArray(msg.message) ? msg.message : [];
    if (sc.kind === "sticker" || (!sc.kind && parts.length === 0)) {
      body.textContent = "Super Sticker";
      return;
    }
    this.appendChatContent(body, msg.message || [], msg.emotes);
  }
```

`SUPERCHAT_TIER_COLORS[sc.tier] ? sc.tier : 0` is the clamp: an absent, out-of-range, string or non-integer tier all resolve to the gray pair instead of indexing to `undefined` and throwing inside a build chunk.

- [ ] **Step 5: Add the CSS**

Two edits plus one append in `web/public/moombox.css`.

**(a)** Delete the `.chat-msg.superchat` rule (`:1743-1745`) outright, along with the blank line that follows it:

```css
.chat-msg.superchat {
    border-left: 3px solid var(--sl-color-warning-500);
}
```

The class stays on the element (`_fillSuperchatCard` adds it) as the marker a reader greps for and a future "paid only" filter would use, but it has no styling of its own any more: nothing carries `.superchat` without `.chat-card`, and the card block below supplies the whole look. A rule left behind with a placeholder declaration would be worse than none.

**(b)** Widen the divider-dim rule (`:1715-1717`) and correct its comment. Replace

```css
/* A divider row is `.future` until playback reaches it — and .future's opacity
   fades the pseudo-element with the row, dropping the label to ~1.9:1 in the
   state it spends most of its life in. Dim the row's CONTENT instead: every
   direct child built by _buildChatMessageEl is a span (time, superchat,
   author, content), so the label keeps its full contrast either way. */
.chat-msg.divider-before.future {
    opacity: 1;
}

.chat-msg.divider-before.future > span {
    opacity: 0.35;
}
```

with

```css
/* A divider row is `.future` until playback reaches it — and .future's opacity
   fades the pseudo-element with the row, dropping the label to ~1.9:1 in the
   state it spends most of its life in. Dim the row's CONTENT instead, so the
   label keeps its full contrast either way. `> *` and not `> span`: a flat
   row's children are all spans, but a card's are its header and body divs and
   a notice's include its system line, and a rule that reached only spans would
   leave a Super Chat card at full strength while every row around it dimmed. */
.chat-msg.divider-before.future {
    opacity: 1;
}

.chat-msg.divider-before.future > * {
    opacity: 0.35;
}
```

**(c)** Append the card block immediately after the `.chat-msg-superchat` rule (which ends at `:1781`) and before `.chat-emoji`:

```css
/* ── Sidebar chat cards (2026-09-25 rulings K1/K2) ───────────────────────────
   A Super Chat, Super Sticker or membership event is a self-contained two-part
   card rather than a flat row with a border. The ROW keeps `.chat-msg` — the
   sidebar promotes, dims, hides, divides and measures rows by that class and
   by their index — and stops striping; the card inside supplies its colour.
   --card-header / --card-body are written from JS through the CSSOM
   (player.js _cardParts), which is not an inline style attribute and asks
   nothing of style-src. */
.chat-msg.chat-card:nth-child(odd),
.chat-msg.chat-card:nth-child(even) {
    background-color: transparent;
}

.chat-card-header,
.chat-card-body {
    padding: 3px var(--sl-spacing-x-small);
}

.chat-card-header {
    display: flex;
    align-items: baseline;
    gap: var(--sl-spacing-2x-small);
    background: var(--card-header);
    border-radius: 4px 4px 0 0;
}

/* A card whose body would be empty drops the body element, so the header is
   the whole card and rounds on all four corners. */
.chat-card-header:last-child {
    border-radius: 4px;
}

.chat-card-body {
    background: var(--card-body);
    border-radius: 0 0 4px 4px;
}

/* The card's ink, chosen from its BODY colour by relative luminance in
   player.js _cardParts and worn by both halves. Set on the header and body
   elements themselves rather than inherited from the row, so
   `.chat-msg.post`'s neutral-700 — which reaches a flat row's spans by
   inheritance — cannot repaint a card. */
.chat-ink-light { color: #ffffff; }
.chat-ink-dark  { color: rgba(0, 0, 0, 0.87); }

/* Inside a header the author, amount and time take the card's ink instead of
   their own row colours, and the time is pushed to the top-right. The extra
   .chat-card qualifier outranks `.chat-msg-author.owner` and friends rather
   than relying on source order. */
.chat-card .chat-card-header .chat-msg-author,
.chat-card .chat-card-header .chat-msg-superchat,
.chat-card .chat-card-header .chat-msg-time {
    color: inherit;
    margin-right: 0;
}

.chat-card .chat-card-header .chat-msg-author {
    flex: 1 1 auto;
    min-width: 0;
}

.chat-card .chat-card-header .chat-msg-time {
    opacity: 0.75;
}
```

- [ ] **Step 6: Run the tests and see them GREEN**

```bash
cd /d/Git/Moombox/.worktrees/chat-cards/web/tests
timeout 300 node --test player.test.mjs 2>&1 | tail -20
```

Expected: `pass 46` / `fail 0` / `skipped 0` (37 before + 9 added: 2 without jsdom, 7 with). Record the actual numbers — task 7's recount uses them.

- [ ] **Step 7: Verify a mutant by execution**

By hand, change `readableInk`'s threshold from `>= 0.5` to `>= 0.51`:

```bash
cd /d/Git/Moombox/.worktrees/chat-cards/web/tests
timeout 300 node --test --test-name-pattern="flip at 0.5" player.test.mjs 2>&1 | tail -20   # expect FAIL
```

Expected: `AssertionError: 'light' !== 'dark'` on `readableInk("#BCBCBC")`. Restore `>= 0.5` by hand, re-run, expect pass.

Then the second mutant, which must strip the `chat-msg` marker **and nothing else** — replace `_cardParts`'s first line with:

```js
    div.classList.add("chat-card");
    div.classList.remove("chat-msg");
```

```bash
cd /d/Git/Moombox/.worktrees/chat-cards/web/tests
timeout 300 node --test --test-name-pattern="still a timeline row" player.test.mjs 2>&1 | tail -20   # expect FAIL
```

Expected: `AssertionError [ERR_ASSERTION]: a card is measured like any row` — the `offsetTop > 0` claim, and only that one. (`div.className = "chat-card";` would be the wrong mutant: it also wipes `future`, so the run dies three assertions earlier and never reaches the one the mutant exists to prove.) Restore the single `div.classList.add("chat-card");` line by hand, re-run, expect pass. Record both observed failures.

- [ ] **Step 8: Gates**

```bash
cd /d/Git/Moombox/.worktrees/chat-cards
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go build ./...                       # web/embed.go re-embeds the changed assets
cd web/tests && timeout 300 node --test *.test.mjs 2>&1 | tail -10
cd /d/Git/Moombox/.worktrees/chat-cards
for f in web/public/modules/player.js web/public/moombox.css web/tests/player.test.mjs; do
  printf '%s ' "$f"; perl -0777 -ne 'print tr/\r//' "$f"; echo
done
git diff --stat -- web/public/modules/player.js
grep -n '_buildNicoEl' web/public/modules/player.js
```

Expected: the build is silent; the whole node suite `fail 0`; `0` beside each of the three paths; and `_buildNicoEl` still declared exactly once, its body untouched (read the ten lines after the declaration and confirm against the quoted original in this plan's constraints).

- [ ] **Step 9: Commit**

```bash
cd /d/Git/Moombox/.worktrees/chat-cards
cat > /tmp/k-t3.msg <<'MSG'
feat(player): YouTube Super Chats become two-part cards (K1)

A Super Chat was a flat row with one amber left border and a bold amount. The
four fields internal/chat archives for it — tier, kind, headerColor, bodyColor
— were never read, so a $1 and a $500 looked identical and a Super Sticker
rendered as a name and a timestamp with no content at all.

Now: a header strip in the tier's header colour with the author, the amount
and the time, and the message in the body colour beneath. The archived colours
win; SUPERCHAT_TIER_COLORS is the fallback for a file written before they were
recorded, with a neutral gray pair for the tier-0 "unresolved" marker. The ink
is derived from the colour actually painted by WCAG relative luminance at a
0.5 threshold, which reproduces YouTube's own choice on all seven tiers (dark
on 2, 3, 4; white on 1, 5, 6, 7) without a second table and still reads on a
colour YouTube has never shipped. A Super Sticker's body says "Super Sticker"
because the image is not archived.

The colours ride as CSS custom properties written through the CSSOM, which is
not an inline style and asks nothing of the CSP. The card element keeps the
`chat-msg` class: the sidebar addresses rows by index, but every
`.chat-msg.<state>` rule and the jsdom harness's measured box key on the class,
and a card must dim, divide, hide and scroll like any other row.

_buildChatMessageEl becomes a shape dispatch; the flat row is extracted
verbatim into _fillPlainRow and the time, author and card shell into shared
helpers. The overlay (_buildNicoEl) is untouched — sidebar only, per K4.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
MSG
git add web/public/modules/player.js web/public/moombox.css web/tests/player.test.mjs
git commit -F /tmp/k-t3.msg -- web/public/modules/player.js web/public/moombox.css web/tests/player.test.mjs
```

---

### Task 4 (T-B): the YouTube member card

Spec §0 K2, §2 item 2. `isMembership` is in every archive and is read by nothing. With tasks 1 and 2 in, a membership event now also carries `membershipText`, and gifted memberships exist at all.

**Files:**
- Modify: `web/public/modules/player.js`
- Modify: `web/public/moombox.css`
- Modify: `web/tests/player.test.mjs`

**Interfaces:**
- Produces: `MEMBER_CARD_COLORS` (exported, module scope), `_fillMemberCard(div, msg)`.
- Consumes: `_cardParts`, `_authorSpan`, `_timeSpan`, `readableInk` (task 3), `msg.membershipText` (tasks 1–2).

**Ruling — the body green is `#4BB682`, `#0F9D58` mixed 25% toward white.** K2 asks for "YouTube's member green" with "a lighter tint body" and names no second hex. A computed tint keeps the hue and is reproducible from the header rather than picked by eye; at a relative luminance of 0.366 it lands on the same white ink as the header (0.249), so the card reads as one block instead of switching ink halfway down. **Cost if wrong:** one hex changes.

**Ruling — an empty card body is removed from the DOM, not hidden.** A new member and a gift purchase have `membershipText` and no `message` at all; leaving an empty coloured strip under the header would look like a rendering fault. `[hidden] { display: none }` is not in `moombox.css` (it is part of the artifact skeleton, not this app), so `el.hidden` would do nothing here. `.chat-card-header:last-child` (task 3) rounds the header's bottom corners when it is the only child. **Cost if wrong:** a card with an empty body reappears; the CSS rule is already there to catch it.

- [ ] **Step 1: Write the failing tests**

Append to `web/tests/player.test.mjs`:

```js
// MUTANT: read `message` and ignore membershipText — a new member renders as a
// name and a timestamp, which is exactly what the archive used to hold.
test("a new member gets a green card carrying the renderer's own line", { skip }, async () => {
  const h = await showChat([ytMsg({
    authorName: "newfan", isMembership: true,
    membershipText: "Welcome to Member!", message: [],
  })]);
  const row = h.sidebar().children[0];
  assert.ok(row.classList.contains("chat-msg"));
  assert.ok(row.classList.contains("chat-card"));
  assert.ok(row.classList.contains("member"));
  assert.equal(row.style.getPropertyValue("--card-header"), "#0F9D58");
  assert.equal(row.style.getPropertyValue("--card-body"), "#4BB682");
  const header = row.querySelector(".chat-card-header");
  assert.ok(header.classList.contains("chat-ink-light"),
    "the member green and its tint both take white ink");
  assert.equal(header.querySelector(".chat-msg-author").textContent, "newfan");
  assert.equal(header.querySelector(".chat-card-note").textContent, "Welcome to Member!");
  assert.equal(row.querySelector(".chat-card-body"), null,
    "a member who typed nothing gets no empty body strip");
});

// MUTANT: put the milestone line in the body — the member's own words and the
// renderer's line become one paragraph and the card stops having two parts.
test("a milestone card keeps the line and the message apart", { skip }, async () => {
  const h = await showChat([ytMsg({
    authorName: "oldfan", isMembership: true,
    membershipText: "Member for 6 months",
    message: [{ type: "text", text: "thanks!" }],
  })]);
  const row = h.sidebar().children[0];
  assert.equal(row.querySelector(".chat-card-note").textContent, "Member for 6 months");
  assert.equal(row.querySelector(".chat-card-body").textContent, "thanks!");
  assert.equal(row.querySelector(".chat-card-header").lastChild.className, "chat-msg-time");
});

// The two shapes internal/chat only started archiving in this arc. MUTANT:
// gate the card on membershipText instead of isMembership — the redemption
// (which has none, its line is its message) falls back to a flat row.
test("both gifted-membership shapes render as member cards", { skip }, async () => {
  const h = await showChat([
    ytMsg({ authorName: "gifter", isMembership: true,
            membershipText: "Gifted 5 memberships", message: [] }),
    ytMsg({ offsetMs: 2000, authorName: "lucky", isMembership: true,
            message: [{ type: "text", text: "was gifted a membership by gifter" }] }),
  ]);
  const [purchase, redemption] = h.sidebar().children;
  assert.equal(purchase.querySelector(".chat-card-note").textContent, "Gifted 5 memberships");
  assert.equal(purchase.querySelector(".chat-card-body"), null);
  assert.ok(redemption.classList.contains("member"));
  assert.equal(redemption.querySelector(".chat-card-note"), null);
  assert.equal(redemption.querySelector(".chat-card-body").textContent,
    "was gifted a membership by gifter");
});
```

- [ ] **Step 2: Run the tests and see them RED**

```bash
cd /d/Git/Moombox/.worktrees/chat-cards/web/tests
timeout 300 node --test --test-name-pattern="green card|milestone card|gifted-membership" player.test.mjs 2>&1 | tail -30
```

Expected: `tests 3 / pass 0 / fail 3`, each on the first card assertion — `AssertionError [ERR_ASSERTION]: The expression evaluated to a falsy value:` for `assert.ok(row.classList.contains("chat-card"))`, because a membership message still takes the flat-row branch. The pattern must name all three tests: "milestone card" is the middle one's only distinctive phrase, and a pattern of `member card|green card|gifted-membership` silently selects two of three. Record one failure verbatim and the `tests 3` line.

- [ ] **Step 3: Add the member card**

In `web/public/modules/player.js`, add the palette constant immediately after `SUPERCHAT_TIER_COLORS`:

```js
/**
 * YouTube's member green. The body is that green mixed 25% toward white
 * (#0F9D58 → #4BB682): a lighter tint of the same hue, computed from the
 * header rather than picked, and at a relative luminance of 0.366 it takes the
 * same white ink as the header (0.249), so the card reads as one block.
 */
export const MEMBER_CARD_COLORS = { header: "#0F9D58", body: "#4BB682" };
```

Add the dispatch line in `_buildChatMessageEl`, immediately after the `msg.superchat` branch:

```js
    if (msg.isMembership) {
      this._fillMemberCard(div, msg);
      return div;
    }
```

And add the method immediately after `_fillSuperchatCard`:

```js
  /**
   * K2: a membership event — a new member, a milestone, a gift purchase or a
   * gift redemption — as a green card.
   *
   * `membershipText` is the renderer's own header line ("Welcome to Member!",
   * "Member for 6 months", "Gifted 5 memberships"), captured by
   * internal/chat/api.go; `message` is whatever the member typed, which a new
   * member and a gift purchase do not have. The two are kept apart — the line
   * beside the name, the words in the body — which is the reason the archive
   * carries them as separate fields.
   *
   * A card with nothing to put in its body drops the body element rather than
   * leaving an empty coloured strip; .chat-card-header:last-child rounds the
   * header on all four corners when that happens.
   */
  _fillMemberCard(div, msg) {
    div.classList.add("member");
    const { header, body } = this._cardParts(div, MEMBER_CARD_COLORS.header, MEMBER_CARD_COLORS.body);
    header.appendChild(this._authorSpan(msg, false));
    if (msg.membershipText) {
      const note = document.createElement("span");
      note.className = "chat-card-note";
      note.textContent = msg.membershipText;
      header.appendChild(note);
    }
    header.appendChild(this._timeSpan(msg));
    this.appendChatContent(body, msg.message || [], msg.emotes);
    if (!body.hasChildNodes()) body.remove();
  }
```

- [ ] **Step 4: Add the CSS**

Append to `web/public/moombox.css`, at the end of the card block task 3 added:

```css
/* K2: the renderer's own line ("Member for 6 months"), set beside the name
   rather than under it so a member card stays at most two rows tall. */
.chat-card-note {
    font-size: var(--sl-font-size-x-small);
    opacity: 0.85;
}
```

- [ ] **Step 5: Run the tests and see them GREEN**

```bash
cd /d/Git/Moombox/.worktrees/chat-cards/web/tests
timeout 300 node --test player.test.mjs 2>&1 | tail -12
```

Expected: `pass 49` / `fail 0` (task 3's count + 3). Record the numbers.

- [ ] **Step 6: Verify a mutant by execution**

By hand, change `if (!body.hasChildNodes()) body.remove();` to `if (false) body.remove();`:

```bash
cd /d/Git/Moombox/.worktrees/chat-cards/web/tests
timeout 300 node --test --test-name-pattern="green card" player.test.mjs 2>&1 | tail -20   # expect FAIL
```

Expected: `a member who typed nothing gets no empty body strip` — the `querySelector(".chat-card-body")` comes back as an element instead of `null`. Restore by hand, re-run, expect pass.

- [ ] **Step 7: Gates**

```bash
cd /d/Git/Moombox/.worktrees/chat-cards
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go build ./...
cd web/tests && timeout 300 node --test *.test.mjs 2>&1 | tail -10
cd /d/Git/Moombox/.worktrees/chat-cards
for f in web/public/modules/player.js web/public/moombox.css web/tests/player.test.mjs; do
  printf '%s ' "$f"; perl -0777 -ne 'print tr/\r//' "$f"; echo
done
```

Expected: silent build; whole suite `fail 0`; `0` beside each path.

- [ ] **Step 8: Commit**

```bash
cd /d/Git/Moombox/.worktrees/chat-cards
cat > /tmp/k-t4.msg <<'MSG'
feat(player): membership events become green member cards (K2)

isMembership has been in every archive since chat downloading landed and was
read by nothing, so a new member rendered as a name, a timestamp and — because
the renderer's "Welcome to Member!" was thrown away upstream — no content at
all. With that line now archived as membershipText, a membership event gets a
card in YouTube's member green: the author and the renderer's own line in the
header, the member's own words in the body.

The body green is the header mixed 25% toward white (#0F9D58 → #4BB682), so it
is a computed tint of the same hue rather than a second colour picked by eye,
and at a luminance of 0.366 it takes the same white ink as the header. A card
with nothing to put in its body drops the body element instead of leaving an
empty coloured strip, and the header rounds on all four corners when it does.

Covers all four shapes: new member, milestone, gift purchase and gift
redemption — the last two of which the parser only started archiving in this
arc.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
MSG
git add web/public/modules/player.js web/public/moombox.css web/tests/player.test.mjs
git commit -F /tmp/k-t4.msg -- web/public/modules/player.js web/public/moombox.css web/tests/player.test.mjs
```

---

### Task 5 (T-C): Twitch sub / resub / gift / raid notices

Spec §0 K3 (first half), §2 item 3. `internal/twitch/types.go:62-86` archives `MessageType`, `SystemMsg`, `SubPlan`, `GiftRecipient` and `ViewerCount`, and `player.js` reads only `messageType === "announcement"`. A sub, resub, gift or raid therefore renders as a plain row whose message is often empty — the event itself is invisible.

**Files:**
- Modify: `web/public/modules/player.js`
- Modify: `web/public/moombox.css`
- Modify: `web/tests/player.test.mjs`

**Interfaces:**
- Produces: `TWITCH_NOTICE_TYPES` (exported `Set`), `twitchNoticeLine(msg)` (exported, pure), `TWITCH_PLAN_NAMES` (private), `_fillTwitchNotice(div, msg)`.
- Consumes: `_timeSpan`, `this.appendChatContent` (which takes the Twitch string form and its native emotes).
- Unchanged: `announcement` and `system` keep today's rendering — the `announcement` classes stay in `_fillPlainRow`.

**Ruling — `.chat-notice.twitch`, exactly as the spec writes it.** A bare `.twitch` class is a collision risk in a global stylesheet, but every rule that uses it is compounded (`.chat-msg.chat-notice.twitch { … }`) and `grep -rn '\.twitch\b' web/public/moombox.css` finds nothing else. Following the spec costs nothing here. **Cost if wrong:** a future bare `.twitch` rule needs scoping.

**Ruling — the raid line is pluralised and the unknown-plan line is shortened.** K3's example is "Y is raiding with 120 viewers"; a literal template would also write "1 viewers", and an unrecognised `subPlan` would write "subscribed with undefined". One ternary and one guard, both in the test table. **Cost if wrong:** two strings read slightly differently from the ruling's example, which used a plural case.

- [ ] **Step 1: Write the failing tests**

Add `twitchNoticeLine` to the existing `player.js` import line at the top of `web/tests/player.test.mjs`, then append:

```js
/** A Twitch-shaped chat file: `message` is a plain string, not MessagePart[]. */
const twNotice = (extra = {}) => ({
  offsetMs: 1000, authorName: "streamer_fan", message: "", ...extra,
});

/** Build a player over a Twitch chat file (marked, so no legacy correction). */
async function showTwitchChat(messages) {
  const h = harness.makePlayer({
    jobs: [finished("j1", { chatFilename: "chat.json" })],
    watchState: {},
    chat: { platform: "twitch", emoteOffsets: "utf16", messages },
    storage: { "player-nico-toggle": "false", "player-sidebar-toggle": "true" },
  });
  await h.selectJob("j1");
  return h;
}

// Pure: no DOM. MUTANT: build the line even when the wire sent one (a resub's
// real systemMsg carries the month count and the streak, which no rebuild has).
// MUTANT: drop the plural guard and a one-viewer raid reads "1 viewers".
test("twitchNoticeLine prefers the wire's own line and rebuilds a sane one", () => {
  assert.equal(twitchNoticeLine({ messageType: "resub", systemMsg: "fan subscribed for 12 months!" }),
    "fan subscribed for 12 months!");
  assert.equal(twitchNoticeLine({ messageType: "sub", authorName: "fan", subPlan: "1000" }),
    "fan subscribed with Tier 1");
  assert.equal(twitchNoticeLine({ messageType: "resub", authorName: "fan", subPlan: "Prime" }),
    "fan subscribed with Prime");
  assert.equal(twitchNoticeLine({ messageType: "sub", authorName: "fan", subPlan: "9999" }),
    "fan subscribed", "an unknown plan is omitted, never printed");
  assert.equal(twitchNoticeLine({ messageType: "subgift", authorName: "fan", giftRecipient: "pal" }),
    "fan gifted a sub to pal");
  assert.equal(twitchNoticeLine({ messageType: "subgift", authorName: "fan" }),
    "fan gifted a sub");
  assert.equal(twitchNoticeLine({ messageType: "raid", authorName: "other", viewerCount: 120 }),
    "other is raiding with 120 viewers");
  assert.equal(twitchNoticeLine({ messageType: "raid", authorName: "other", viewerCount: 1 }),
    "other is raiding with 1 viewer");
  assert.equal(twitchNoticeLine({ messageType: "raid", authorName: "other" }),
    "other is raiding");
  assert.equal(twitchNoticeLine({ messageType: "chat", authorName: "fan" }), "");
});

// MUTANT: drop the .chat-msg marker from the notice and the row stops dimming,
// dividing and measuring with the rest of the sidebar.
test("a Twitch sub, gift and raid each render as a purple notice block", { skip }, async () => {
  const h = await showTwitchChat([
    twNotice({ messageType: "sub", subPlan: "Prime", systemMsg: "fan subscribed with Prime" }),
    twNotice({ offsetMs: 2000, messageType: "subgift", authorName: "fan", giftRecipient: "pal" }),
    twNotice({ offsetMs: 3000, messageType: "raid", authorName: "other", viewerCount: 120 }),
  ]);
  const lines = [...h.sidebar().children].map((row) => {
    assert.ok(row.classList.contains("chat-msg"), row.className);
    assert.ok(row.classList.contains("chat-notice"), row.className);
    assert.ok(row.classList.contains("twitch"), row.className);
    return row.querySelector(".chat-notice-line").textContent;
  });
  assert.deepEqual(lines, [
    "fan subscribed with Prime",
    "fan gifted a sub to pal",
    "other is raiding with 120 viewers",
  ]);
});

// MUTANT: always append the content span — a notice with no message gains an
// empty trailing span, which the divider-dim rule then dims as a child.
test("a resub's own words sit under its system line, and silence adds nothing", { skip }, async () => {
  const h = await showTwitchChat([
    twNotice({ messageType: "resub", systemMsg: "fan subscribed for 12 months!",
               message: "still here!" }),
    twNotice({ offsetMs: 2000, messageType: "sub", systemMsg: "quiet subscribed" }),
  ]);
  const [spoken, silent] = h.sidebar().children;
  assert.deepEqual([...spoken.children].map((c) => c.className),
    ["chat-msg-time", "chat-notice-line", ""]);
  assert.equal(spoken.lastChild.textContent, "still here!");
  assert.deepEqual([...silent.children].map((c) => c.className),
    ["chat-msg-time", "chat-notice-line"]);
});

// The scope pin for K3: only the four kinds named become blocks.
// MUTANT: add "announcement" or "system" to TWITCH_NOTICE_TYPES and the
// announcement loses its colour classes to a notice block.
test("announcements and system messages keep today's flat rendering", { skip }, async () => {
  const h = await showTwitchChat([
    twNotice({ messageType: "announcement", announcementColor: "green", message: "hello all" }),
    twNotice({ offsetMs: 2000, messageType: "system", message: "stream is starting" }),
  ]);
  const [ann, sys] = h.sidebar().children;
  assert.ok(ann.classList.contains("announcement"));
  assert.ok(ann.classList.contains("announcement-green"));
  assert.equal(ann.querySelector(".chat-notice-line"), null);
  assert.equal(sys.className, "chat-msg future");
  assert.equal(sys.lastChild.textContent, "stream is starting");
});
```

- [ ] **Step 2: Run the tests and see them RED**

```bash
cd /d/Git/Moombox/.worktrees/chat-cards/web/tests
timeout 300 node --test player.test.mjs 2>&1 | tail -30
```

Expected: an import-time `SyntaxError: … does not provide an export named 'twitchNoticeLine'`. Record it, then add the export skeleton in step 3 and re-run to see the four tests fail on their own terms before the bodies exist.

- [ ] **Step 3: Add the notice**

In `web/public/modules/player.js`, after `MEMBER_CARD_COLORS`:

```js
/**
 * The Twitch event kinds that become a notice block. `announcement` and
 * `system` deliberately keep the flat row they have today (2026-09-25 ruling
 * K3) — the announcement's colour classes are its whole styling.
 */
export const TWITCH_NOTICE_TYPES = new Set(["sub", "resub", "subgift", "raid"]);

/** msg-param-sub-plan (internal/twitch: SubPlan) → the name Twitch shows. */
const TWITCH_PLAN_NAMES = { 1000: "Tier 1", 2000: "Tier 2", 3000: "Tier 3", Prime: "Prime" };

/**
 * The bold first line of a Twitch notice: the wire's own `systemMsg` when the
 * archive has one — it is richer than anything reconstructable, carrying month
 * counts and streaks — else rebuilt from the fields the IRC parser records
 * (internal/twitch/types.go: SubPlan, GiftRecipient, ViewerCount). Returns ""
 * when nothing can be said, and the caller omits the line rather than printing
 * a half-sentence.
 * @param {object} msg
 * @returns {string}
 */
export function twitchNoticeLine(msg) {
  if (msg.systemMsg) return msg.systemMsg;
  const who = msg.authorName || "Someone";
  switch (msg.messageType) {
    case "sub":
    case "resub": {
      const plan = TWITCH_PLAN_NAMES[msg.subPlan];
      return plan ? `${who} subscribed with ${plan}` : `${who} subscribed`;
    }
    case "subgift":
      return msg.giftRecipient ? `${who} gifted a sub to ${msg.giftRecipient}` : `${who} gifted a sub`;
    case "raid": {
      const n = Number(msg.viewerCount);
      if (!Number.isFinite(n) || n <= 0) return `${who} is raiding`;
      return `${who} is raiding with ${n} ${n === 1 ? "viewer" : "viewers"}`;
    }
    default:
      return "";
  }
}
```

Add the dispatch line in `_buildChatMessageEl`, after the `isMembership` branch:

```js
    if (TWITCH_NOTICE_TYPES.has(msg.messageType)) {
      this._fillTwitchNotice(div, msg);
      return div;
    }
```

And the method after `_fillMemberCard`:

```js
  /**
   * K3: a Twitch sub, resub, gift or raid as a highlighted block — Twitch's
   * purple down the left edge, the same purple at 10% behind it, the system
   * line first and the sender's own words, if any, underneath.
   *
   * Not a card: these carry no colour of their own and no amount, so the
   * two-part shell would be two strips of the same purple. The content span is
   * appended only when it produced nodes, the same hasChildNodes idiom the
   * overlay builder uses, so a silent notice does not end in an empty span
   * that the divider-dim rule would then dim as a child.
   *
   * ("the overlay builder", not its name: task 8's K4 gate greps the diff for
   * the overlay symbol, and a prose mention would read as an overlay edit.)
   */
  _fillTwitchNotice(div, msg) {
    div.classList.add("chat-notice", "twitch");
    div.appendChild(this._timeSpan(msg));
    const line = twitchNoticeLine(msg);
    if (line) {
      const lineEl = document.createElement("div");
      lineEl.className = "chat-notice-line";
      lineEl.textContent = line;
      div.appendChild(lineEl);
    }
    const content = document.createElement("span");
    this.appendChatContent(content, msg.message || [], msg.emotes);
    if (content.hasChildNodes()) div.appendChild(content);
  }
```

- [ ] **Step 4: Add the CSS**

Append to `web/public/moombox.css`, after the `.chat-card-note` rule:

```css
/* ── Twitch notices (2026-09-25 ruling K3) ───────────────────────────────────
   A sub, resub, gift or raid is a highlighted block rather than a flat line:
   Twitch's purple down the left edge and the same purple at 10% behind it. The
   striping override matches .chat-msg.announcement's — nth-child would
   otherwise repaint every second notice. The accent is written twice rather
   than hoisted into a custom property: unlike --announcement-accent, which
   five colour classes set, there is one value and one setter. */
.chat-msg.chat-notice.twitch {
    border-left: 4px solid #9147ff;
}

.chat-msg.chat-notice.twitch:nth-child(odd),
.chat-msg.chat-notice.twitch:nth-child(even) {
    background-color: color-mix(in srgb, #9147ff 10%, transparent);
}

.chat-notice-line {
    font-weight: var(--sl-font-weight-semibold);
}
```

- [ ] **Step 5: Run the tests and see them GREEN**

```bash
cd /d/Git/Moombox/.worktrees/chat-cards/web/tests
timeout 300 node --test player.test.mjs 2>&1 | tail -12
```

Expected: `pass 53` / `fail 0` (task 4's count + 4: one pure, three jsdom). Record the numbers.

- [ ] **Step 6: Verify a mutant by execution**

By hand, add `"announcement"` to `TWITCH_NOTICE_TYPES`:

```bash
cd /d/Git/Moombox/.worktrees/chat-cards/web/tests
timeout 300 node --test --test-name-pattern="keep today's flat rendering" player.test.mjs 2>&1 | tail -20   # expect FAIL
```

Expected: `ann.classList.contains("announcement")` comes back false. Restore by hand, re-run, expect pass.

- [ ] **Step 7: Gates**

Same block as task 4 step 7 (`go build ./...`, the whole node suite, the three LF checks).

- [ ] **Step 8: Commit**

```bash
cd /d/Git/Moombox/.worktrees/chat-cards
cat > /tmp/k-t5.msg <<'MSG'
feat(player): Twitch sub, gift and raid notices get their own block (K3)

internal/twitch has archived messageType, systemMsg, subPlan, giftRecipient
and viewerCount since the IRC producer landed, and the player read exactly one
of them (the announcement colour). A sub, resub, gift or raid therefore
rendered as a plain row whose message is usually empty — the event itself was
invisible in the replay.

Those four kinds now render as a highlighted block: Twitch's purple down the
left edge, the same purple at 10% behind it, the system line first and the
sender's own words underneath. The wire's systemMsg wins when the archive has
one — it carries month counts and streaks no rebuild can reconstruct — and
otherwise the line is built from the fields, with the plan names Twitch shows
(1000/2000/3000/Prime → Tier 1/2/3/Prime), a singular for a one-viewer raid,
and no half-sentence when a field is missing.

announcement and system keep today's flat rendering, per the ruling's scope.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
MSG
git add web/public/modules/player.js web/public/moombox.css web/tests/player.test.mjs
git commit -F /tmp/k-t5.msg -- web/public/modules/player.js web/public/moombox.css web/tests/player.test.mjs
```

---

### Task 6 (T-D): the Twitch cheer chip

Spec §0 K3 (second half), §2 item 4. `TwitchChatMessage.Bits` is archived and unread; a cheer renders as ordinary text.

**Files:**
- Modify: `web/public/modules/player.js`
- Modify: `web/public/moombox.css`
- Modify: `web/tests/player.test.mjs`

**Interfaces:**
- Produces: `CHEER_SCALE` (exported), `cheerColor(bits)` (exported, pure), `_cheerChip(bits)`.
- Consumes: `readableInk` (task 3), `_fillPlainRow` (task 3) — the chip is inserted there, between the author and the content.

**Ruling — the chip is built on `bits > 0`, not on `messageType === "bits"`.** Spec §2 item 4 offers either. A `bits` message with no count has nothing to put in a chip, and `0 bits` in a coloured pill is a wart; a cheer that somehow arrived with `messageType: "chat"` and a real count still gets its chip. **Cost if wrong:** a hypothetical zero-bit cheer renders as an ordinary row, which is what it is.

**Stated up front so nobody over-claims it:** all five cheer colours have a relative luminance below 0.5 (gray 0.309, purple 0.163, green 0.348, blue 0.299, red 0.215), so every chip takes light ink and the chip tests **cannot** distinguish `readableInk` from a hard-coded `"light"`. The mutant that hard-codes the ink is killed by task 3's tier-2/3/4 card tests and by the boundary-pair test, not here. The chip uses the shared helper anyway, so a future scale colour that is pale reads without a second thought.

- [ ] **Step 1: Write the failing tests**

Add `cheerColor` to the `player.js` import line at the top of `web/tests/player.test.mjs`, then append:

```js
// Pure: no DOM. The eight boundaries the ruling names, each side of each step.
// MUTANT: write `>` instead of `>=` anywhere in the ladder and 100, 1000, 5000
// or 10000 drops a band. MUTANT: order the ladder ascending and every cheer
// comes back gray.
test("cheerColor follows Twitch's amount scale at every boundary", () => {
  assert.deepEqual([0, 99, 100, 999, 1000, 4999, 5000, 9999, 10000, 250000].map(cheerColor), [
    "#979797", "#979797", "#9c3ee8", "#9c3ee8", "#1db2a5",
    "#1db2a5", "#0099fe", "#0099fe", "#f43021", "#f43021",
  ]);
  assert.equal(cheerColor("1500"), "#1db2a5", "the archive writes a number, but a string still lands");
  assert.equal(cheerColor(undefined), "#979797");
});

// MUTANT: place the chip after the content and the cheer reads as a trailing
// afterthought instead of a prefix. MUTANT: build it for every Twitch message
// and every ordinary line grows a "0 bits" pill.
test("a cheer gets a scaled chip before its content; a plain line does not", { skip }, async () => {
  const h = await showTwitchChat([
    twNotice({ messageType: "bits", bits: 5000, authorName: "cheerer", message: "take my bits" }),
    twNotice({ offsetMs: 2000, messageType: "chat", message: "no bits here" }),
    twNotice({ offsetMs: 3000, messageType: "bits", bits: 0, message: "nothing to show" }),
  ]);
  const [cheer, plain, empty] = h.sidebar().children;
  assert.deepEqual([...cheer.children].map((c) => c.className),
    ["chat-msg-time", "chat-msg-author", "cheer-chip chat-ink-light", ""]);
  const chip = cheer.querySelector(".cheer-chip");
  assert.equal(chip.textContent, "5000 bits");
  assert.equal(chip.style.getPropertyValue("--cheer-bg"), "#0099fe");
  assert.equal(cheer.lastChild.textContent, "take my bits");
  assert.equal(plain.querySelector(".cheer-chip"), null);
  assert.equal(empty.querySelector(".cheer-chip"), null,
    "a bits message with no count has nothing to put in a chip");
});

// The chip lives on the FLAT row, so it must survive beside everything else
// that row can carry. MUTANT: insert it before the time span, or build it in a
// branch that an announcement takes first — a cheered announcement is a real
// Twitch shape and would lose either its colour or its amount.
test("a cheer chip coexists with a badge and an announcement", { skip }, async () => {
  const h = await showTwitchChat([
    twNotice({ messageType: "announcement", announcementColor: "blue", bits: 100,
               authorBadges: ["subscriber/12"], message: "cheers" }),
  ]);
  const row = h.sidebar().children[0];
  assert.ok(row.classList.contains("announcement-blue"), row.className);
  assert.deepEqual([...row.children].map((c) => c.className),
    ["chat-msg-time", "chat-msg-author member", "cheer-chip chat-ink-light", ""]);
  assert.equal(row.querySelector(".cheer-chip").style.getPropertyValue("--cheer-bg"), "#9c3ee8");
});
```

- [ ] **Step 2: Run the tests and see them RED**

```bash
cd /d/Git/Moombox/.worktrees/chat-cards/web/tests
timeout 300 node --test player.test.mjs 2>&1 | tail -30
```

Expected: an import-time `SyntaxError: … does not provide an export named 'cheerColor'`. Record it.

- [ ] **Step 3: Add the chip**

In `web/public/modules/player.js`, after `twitchNoticeLine`:

```js
/**
 * Twitch's cheer colour scale, richest first so the first match wins:
 * gray under 100 bits, purple from 100, green from 1,000, blue from 5,000 and
 * red from 10,000.
 */
export const CHEER_SCALE = [
  { min: 10000, color: "#f43021" },
  { min: 5000, color: "#0099fe" },
  { min: 1000, color: "#1db2a5" },
  { min: 100, color: "#9c3ee8" },
  { min: 0, color: "#979797" },
];

/**
 * The colour for a cheer of `bits`. Anything unparseable reads as 0, i.e. the
 * bottom band — never an exception inside a sidebar build chunk.
 * @param {number|string} bits
 * @returns {string}
 */
export function cheerColor(bits) {
  const n = Number(bits) || 0;
  for (const step of CHEER_SCALE) {
    if (n >= step.min) return step.color;
  }
  return CHEER_SCALE[CHEER_SCALE.length - 1].color;
}
```

Add the chip to `_fillPlainRow`, between the author and the content:

```js
    div.appendChild(this._authorSpan(msg, true));
    // K3: a cheer's amount, coloured by Twitch's scale. Gated on the count
    // rather than on messageType === "bits": a bits message with no count has
    // nothing to put in a chip, and a cheer that arrived typed as ordinary
    // chat still has its amount.
    const bits = Number(msg.bits) || 0;
    if (bits > 0) div.appendChild(this._cheerChip(bits));
    const contentSpan = document.createElement("span");
```

And add the method after `_fillTwitchNotice`:

```js
  /**
   * The cheer chip: "<n> bits" in a pill of the scale's colour, with the ink
   * the shared luminance rule asks for. Every colour on today's scale happens
   * to be dark enough for white ink, so the rule is invisible here — it is
   * used anyway so a future pale band reads without a second thought.
   */
  _cheerChip(bits) {
    const chip = document.createElement("span");
    const color = cheerColor(bits);
    chip.className = `cheer-chip chat-ink-${readableInk(color)}`;
    chip.style.setProperty("--cheer-bg", color);
    chip.textContent = `${bits} bits`;
    return chip;
  }
```

- [ ] **Step 4: Add the CSS**

Append to `web/public/moombox.css`, after the Twitch notice block:

```css
/* The cheer chip: a pill of the amount's scale colour, inline before the
   message it paid for. --cheer-bg is set from JS through the CSSOM. */
.cheer-chip {
    display: inline-block;
    padding: 0 6px;
    margin-right: var(--sl-spacing-2x-small);
    border-radius: 999px;
    background: var(--cheer-bg);
    font-size: var(--sl-font-size-x-small);
    font-weight: var(--sl-font-weight-bold);
}
```

- [ ] **Step 5: Run the tests and see them GREEN**

```bash
cd /d/Git/Moombox/.worktrees/chat-cards/web/tests
timeout 300 node --test player.test.mjs 2>&1 | tail -12
```

Expected: `pass 56` / `fail 0` (task 5's count + 3: one pure, two jsdom). Record the numbers.

- [ ] **Step 6: Verify a mutant by execution**

By hand, change `if (n >= step.min)` to `if (n > step.min)`:

```bash
cd /d/Git/Moombox/.worktrees/chat-cards/web/tests
timeout 300 node --test --test-name-pattern="amount scale at every boundary" player.test.mjs 2>&1 | tail -25   # expect FAIL
```

Expected: the deepEqual reports `100 → "#979797"` where `"#9c3ee8"` was wanted (and the same slip at 1000, 5000 and 10000). Restore by hand, re-run, expect pass.

- [ ] **Step 7: Gates**

Same block as task 4 step 7.

- [ ] **Step 8: Commit**

```bash
cd /d/Git/Moombox/.worktrees/chat-cards
cat > /tmp/k-t6.msg <<'MSG'
feat(player): cheers get a chip coloured by Twitch's bits scale (K3)

TwitchChatMessage.Bits has been archived since the IRC producer landed and was
read by nothing, so a 10,000-bit cheer and an ordinary line looked the same in
the replay. A cheer now carries a pill with its amount, in Twitch's own colour
ladder: gray under 100, purple from 100, green from 1,000, blue from 5,000 and
red from 10,000, each boundary pinned from both sides.

The chip is built on the COUNT, not on messageType: a bits message with no
count has nothing to put in a pill, and a cheer typed as ordinary chat still
has its amount. Its ink comes from the same luminance rule the cards use —
every colour on today's scale is dark enough for white, so the rule changes
nothing here, but a pale band added later reads without a second thought.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
MSG
git add web/public/modules/player.js web/public/moombox.css web/tests/player.test.mjs
git commit -F /tmp/k-t6.msg -- web/public/modules/player.js web/public/moombox.css web/tests/player.test.mjs
```

---

### Task 7 (T-E): docs, the stylesheet pin, and the README recount

Spec §2 item 7, plus the divider-dim rule task 3 widened — the one behaviour in this arc that lives entirely in CSS and that jsdom cannot see (the harness builds a document with no stylesheet at all, so no computed style exists).

**Files:**
- Modify: `web/tests/a11y-controls.test.mjs`
- Modify: `docs/spec/user-interfaces.md`
- Modify: `web/tests/README.md`

**Interfaces:**
- Consumes: `web/public/moombox.css` as text (the pattern the file's existing focus-ring test already sets), the live `node --test` output for the recount.
- Produces: no code.

**Ruling — the stylesheet assertion goes in `a11y-controls.test.mjs`, not a new suite.** That file already owns the one "read `moombox.css` and assert on its text" test, for the same reason (a legibility rule with no runtime witness), and it is registered without `skip`. A dedicated one-test suite would cost a README table row and a suite for a single assertion. **Cost if wrong:** the arc's CSS pin lives one file away from its CSS.

- [ ] **Step 1: Write the failing test**

Append to `web/tests/a11y-controls.test.mjs`, immediately after the existing focus-ring test (the file already imports `fs`, `path` and `fileURLToPath` for it):

```js
// Reads the stylesheet as text, so it needs no jsdom and carries no `skip` —
// the same shape as the focus-ring test above, and for the same reason: the
// rule has no runtime witness. The player harness builds a document with no
// stylesheet, so nothing in player.test.mjs can see a computed opacity.
//
// MUTANT: narrow the selector back to `> span`. A region-divider row that is
// still `.future` keeps its label at full contrast by holding the ROW at
// opacity 1 and dimming its children instead; a flat row's children are all
// spans, but a Super Chat card's are its header and body divs and a Twitch
// notice's include its system line — so `> span` would leave a card at full
// strength in the middle of a dimmed pre-show region, reading as though
// playback had already reached it.
test("the divider row dims a card's block children, not only its spans", () => {
  const css = fs.readFileSync(
    path.join(path.dirname(fileURLToPath(import.meta.url)), "..", "public", "moombox.css"),
    "utf8",
  );
  assert.ok(css.includes(".chat-msg.divider-before.future > * {"),
    "the divider-dim rule must reach every direct child, not only spans");
  assert.ok(!css.includes(".chat-msg.divider-before.future > span"),
    "the span-only form must be gone, not merely joined by a wider one");
});
```

- [ ] **Step 2: Run it and see it GREEN immediately — then prove it can fail**

```bash
cd /d/Git/Moombox/.worktrees/chat-cards/web/tests
timeout 300 node --test --test-name-pattern="dims a card's block children" a11y-controls.test.mjs 2>&1 | tail -12
```

Expected: **pass**. This is a regression pin, not a red-first test — task 3 already made the change it guards. So prove its teeth by execution: by hand, revert the selector in `web/public/moombox.css` to `.chat-msg.divider-before.future > span {`, re-run the same command and confirm the failure — `AssertionError: the divider-dim rule must reach every direct child, not only spans`, the **first** assertion's message and the only one printed, because `node:assert` throws there and the second never runs. Then restore `> *` by hand and re-run to green. Record both outputs.

- [ ] **Step 3: Update `docs/spec/user-interfaces.md`**

In the `web/public/modules/player.js` row of the file-layout table (line 48), append one sentence immediately before the closing ` |`, after "…(`segments.js` `SegmentPlayer`).":

```
 Sidebar chat renders four shapes beyond the flat row (2026-09-25 rulings K1-K4, sidebar only — the overlay keeps plain scrolling text): a YouTube Super Chat or Super Sticker as a two-part card in the tier's header/body colours, taken from the archive when `internal/chat` recorded them and from the seven-tier palette in `web/public/modules/player.js` when it did not (tier 0 = a neutral gray "unresolved" pair), with the text colour derived from the painted colour by WCAG relative luminance at a 0.5 threshold rather than a second table; a membership event (new member, milestone, gift purchase, gift redemption) as a green card carrying the renderer's own header line from `internal/chat/api.go`; a Twitch sub/resub/subgift/raid as a purple notice block whose first line is the wire's `systemMsg` or a rebuild from the archived fields; and a cheer chip coloured by Twitch's bits scale. Every shape is one direct child of the message list carrying the `chat-msg` class, so the sidebar's active/future/post promotion, region dividers, search filter and scroll maths are unchanged.
```

Note: the two backticked paths in that sentence (`web/public/modules/player.js`, `internal/chat/api.go`) are real files, and no backticked symbol sits adjacent to a `.go` path citation, so the citation gate has nothing new to resolve. Verified by `go test ./internal/docs/` in step 5.

- [ ] **Step 4: Recount `web/tests/README.md` from live runs**

Both figure sets must come from an actual run, not from this plan's predictions (258 + 20 added = **278** tests, **148** DOM, **130** without jsdom, player 56 of which 4 are pure — a sanity check, never a target; if the live run disagrees, the live run is right and the discrepancy goes in the task report).

```bash
cd /d/Git/Moombox/.worktrees/chat-cards
timeout 600 node --test web/tests/*.test.mjs 2>&1 | tail -10          # WITH jsdom
mv web/tests/node_modules web/tests/node_modules.off
timeout 600 node --test web/tests/*.test.mjs 2>&1 | tail -10          # WITHOUT jsdom
mv web/tests/node_modules.off web/tests/node_modules
timeout 600 node --test web/tests/player.test.mjs 2>&1 | tail -8      # the per-suite number
timeout 600 node --test web/tests/a11y-controls.test.mjs 2>&1 | tail -8
```

`mv` and not a delete — `npm ci` is reproducible but the install is slow and the directory is the one thing here that is not in git. Move it back in the same step, and confirm with `ls web/tests/node_modules/jsdom` before continuing.

Then update `web/tests/README.md`:
- the `ℹ tests / pass / fail / skipped` block and the "With jsdom installed the same command reports …" line, from the two runs;
- the per-suite breakdown in "the 133 DOM tests (player 37, …)" — `player` and the total both move;
- the "leaving 125 tests that need no DOM" sentence and its list of exceptions, which gains **the colour-helper tests in `player.test.mjs`** (task 3's two, task 5's one and task 6's one) beside the existing a11y and resolution-picker exceptions, and the a11y entry becomes "the two stylesheet-text tests";
- the `| Suite | Needs jsdom |` table keeps `player.test.mjs` in the **yes** row and `a11y-controls.test.mjs` in the **yes** row: the established convention is that a mostly-jsdom suite with a pure test stays in "yes" and the prose names the exception (only `resolution-picker.test.mjs`, which is half and half, is "partly").

- [ ] **Step 5: Gates**

```bash
cd /d/Git/Moombox/.worktrees/chat-cards
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go test -count=1 -timeout 300s ./internal/docs/
timeout 600 node --test web/tests/*.test.mjs 2>&1 | tail -10
for f in web/tests/a11y-controls.test.mjs docs/spec/user-interfaces.md web/tests/README.md; do
  printf '%s ' "$f"; perl -0777 -ne 'print tr/\r//' "$f"; echo
done
```

Expected: `ok` for `internal/docs` (the citation gate over the edited doc); the node suite `fail 0` with the numbers the README now states; `0` beside each path.

- [ ] **Step 6: Commit**

```bash
cd /d/Git/Moombox/.worktrees/chat-cards
cat > /tmp/k-t7.msg <<'MSG'
docs(player,tests): the sidebar's four chat shapes, and the counts

user-interfaces.md's player row records what the sidebar now draws: the
two-part Super Chat card with its archived-then-palette colours and the
luminance-derived ink, the green membership card and the line it carries, the
Twitch notice block, the cheer chip — and that every one of them is still one
`chat-msg` row, so the promotion, dividers, search and scroll maths are
unchanged. The overlay's "sidebar only" scope is stated where a reader looking
for the overlay would find it.

The divider-dim rule is the one behaviour in this arc with no runtime witness —
the player harness builds a document with no stylesheet, so nothing can read a
computed opacity — so it gets the same treatment as the focus ring beside it: a
stylesheet-as-text assertion that the rule reaches a card's block children and
that the span-only form is gone rather than merely joined.

web/tests/README.md recounted from live runs, with and without jsdom.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
MSG
git add web/tests/a11y-controls.test.mjs docs/spec/user-interfaces.md web/tests/README.md
git commit -F /tmp/k-t7.msg -- web/tests/a11y-controls.test.mjs docs/spec/user-interfaces.md web/tests/README.md
```

---

### Task 8: Arc gates, then delete this plan

The merge-candidate gates over a branch that has absorbed `main`. The sibling arc (`tui-120fps`) may have merged first, so the merge is expected to do something — run it before anything else.

The chain's authoritative gate list is `.superpowers/sdd/2026-09-25-chat-cards/gates.sh`, **which the controller runs** — it adds the libc pin, the TUI import rule, the `-race` set, the `FrameCost` pins and the four `MOOMBOX_LIVE_*` gates on top of what follows. Spec §3 is *Order* and names no gates; step 2 below is the implementer's subset, chosen for what this arc actually touches.

**Files:**
- Delete: `docs/superpowers/plans/2026-09-25-chat-cards.md`

**Interfaces:**
- Consumes: tasks 1–7's commits.
- Produces: a green merge candidate for the controller to merge `--no-ff` into `main`.

- [ ] **Step 1: Merge main**

```bash
cd /d/Git/Moombox/.worktrees/chat-cards
git log --oneline -3 main
git merge main
```

Expected: either `Already up to date.` (Arc F has not merged yet) or a clean merge of Arc F. Arc F's file set is `internal/tui/`, `internal/worker/progress.go`, `internal/config/`, `config.example.toml`, `SPEC.md` and `docs/spec/data-and-storage.md` + `docs/spec/user-interfaces.md`; this arc's is `internal/chat/`, `web/public/modules/player.js`, `web/public/moombox.css`, `web/tests/` and `docs/spec/user-interfaces.md`. **`docs/spec/user-interfaces.md` is the one shared file** — Arc F edits the TUI/renderer sections, this arc edits the `player.js` row of the Web UI file table, so a conflict there is a textual collision in different sections and is resolvable by keeping both edits. A conflict anywhere else means something landed on `main` inside this arc's set: stop and report it to the controller rather than resolving it here.

- [ ] **Step 2: The merge-candidate gates**

```bash
cd /d/Git/Moombox/.worktrees/chat-cards
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
gofmt -l ./cmd ./internal ./tools
go vet ./...
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go vet ./...
staticcheck ./...
go build ./...
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /dev/null ./cmd/moombox
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o /dev/null ./cmd/moombox
go mod verify
go mod tidy -diff && echo TIDY-CLEAN
go test -count=1 -timeout 600s ./internal/chat/
go test -count=1 -timeout 600s ./internal/web/routes/
go test -count=1 -timeout 300s ./internal/docs/
go test -count=1 -timeout 300s ./internal/web/
timeout 600 node --test web/tests/*.test.mjs 2>&1 | tail -10
```

Expected: `gofmt -l` prints nothing; vet/staticcheck/builds silent; `go mod verify` prints `all modules verified`; `TIDY-CLEAN` printed; four `ok`s; the node suite `fail 0` with the figures task 7's README now states. `go build ./...` matters here specifically — `web/embed.go` embeds `web/public/`, so an asset change that never went through a build is an unexported change. `./internal/web/routes/` is the archive's server side (the chat route this arc's data travels through) and `./internal/web/` carries the CSP pins this arc must not have moved.

Then the no-jsdom half of the node gate, and the LF rule on every file the arc touched:

```bash
cd /d/Git/Moombox/.worktrees/chat-cards
mv web/tests/node_modules web/tests/node_modules.off
timeout 600 node --test web/tests/*.test.mjs 2>&1 | tail -10
mv web/tests/node_modules.off web/tests/node_modules
ls web/tests/node_modules/jsdom > /dev/null && echo JSDOM-RESTORED

for f in internal/chat/types.go internal/chat/api.go internal/chat/api_test.go \
         internal/chat/helpers_test.go \
         web/public/modules/player.js web/public/moombox.css \
         web/tests/player.test.mjs web/tests/a11y-controls.test.mjs web/tests/README.md \
         docs/spec/user-interfaces.md; do
  printf '%s ' "$f"; perl -0777 -ne 'print tr/\r//' "$f"; echo
done
```

Expected: the no-jsdom run reports `fail 0` with the skipped count the README states; `JSDOM-RESTORED`; `0` beside every path. (`grep -c $'\r'` and `awk '/\r/'` both lie about CRLF under Git Bash — only `perl -0777 -ne 'print tr/\r//'` is trustworthy here.)

- [ ] **Step 3: The K4 pin — the overlay did not move**

```bash
cd /d/Git/Moombox/.worktrees/chat-cards
git diff main -- web/public/modules/player.js | grep -nE '^[-+][^*]*(_buildNicoEl\(|"nico-message")' || echo "NO OVERLAY LINES IN THE DIFF"
git diff main -- web/public/modules/player.js | grep -cE '^[-+][^*]*(_buildNicoEl\(|"nico-message")'
sed -n '/_buildNicoEl(msg) {/,/^  }$/p' web/public/modules/player.js
```

Expected: `NO OVERLAY LINES IN THE DIFF`, then `0`, then a printed `_buildNicoEl` body identical to the one quoted in this plan's constraints — the `announcement` branch, the `appendChatContent` call, the `hasChildNodes` null return and the eager-loading sweep, unchanged. If the diff shows an overlay line, stop: K4 is the arc's hard scope boundary.

The pattern matches only added or removed lines that **call** the builder or name the overlay's class, and `[^*]*` excludes JSDoc continuation lines (` * …`), so a comment mentioning the overlay in prose cannot trip it. A bare `grep -n '_buildNicoEl'` over the diff would: it was tried, and task 5's own doc comment matched it, halting the arc on a false positive. The comment rule in the Global Constraints (no arc-written comment may name `_buildNicoEl` or `"nico-message"`) is the second half of the same fix.

- [ ] **Step 4: The ONE full suite — controller-run**

Implementers never run the full suite; the controller's gate runner does, one at a time across the chain. Hand off to the controller. Do not run this yourself under any circumstances — the command block below is the controller's.

```bash
cd /d/Git/Moombox/.worktrees/chat-cards
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go test -count=1 ./...
```

Expected: 32 `ok` / 0 `fail` (four packages have no test files: `cmd/sign`, `internal/bgutils/embed`, `tools/sidecar-sig-probe`, `web`).

- [ ] **Step 5: Delete this plan and commit**

The plan is implemented and verified; git history is the archive (standing project rule).

```bash
cd /d/Git/Moombox/.worktrees/chat-cards
cat > /tmp/k-t8.msg <<'MSG'
chore(plans): delete the Arc K plan — implemented and verified

The chat parser archives the membership renderer's header line and both
gifted-membership shapes; the player's sidebar draws Super Chat and Super
Sticker cards, green membership cards, Twitch notice blocks and cheer chips;
the overlay is untouched. Git history is the archive.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
MSG
git rm docs/superpowers/plans/2026-09-25-chat-cards.md
git commit -F /tmp/k-t8.msg -- docs/superpowers/plans/2026-09-25-chat-cards.md
```

- [ ] **Step 6: Hand the candidate to the controller**

Report: task 1's RED compiler lines and the header-order mutant's failure; task 2's three RED lines and the badge-hoist mutant's failure; task 3's import-time RED, the two mutants verified by execution (the 0.51 threshold and the dropped `chat-msg` marker) and the per-suite count after it; tasks 4, 5 and 6's RED lines, their mutants and their counts; task 7's both-directions proof of the stylesheet pin and the two live figure sets; the K4 overlay-diff output verbatim; the LF zero-counts; and the complete gate transcript from step 2. The controller merges `--no-ff` into `main` without asking (standing ruling), runs the post-merge gates, then deletes the worktree **and** the `chat-cards` branch.
