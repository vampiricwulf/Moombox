# Arc 10 — Twitch Credential Lifecycle Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

> **EXECUTION STATUS (arc-close, 2026-09-01): IMPLEMENTED.** Every task 0-9 and 7a landed on branch `cookie-arc10-twitch-credential-lifecycle` @ `13a60eb` (29 commits over `main` @ `51832fb`). Read § "Final state" at the END of this file before reading any task: it records what each task delivered, the superseded `OnAnonymousPlayback` lines, the residuals, the field gates and the coordinator-directed deviation. Line numbers cited there are at `13a60eb` (this pointer shifts everything below it by two lines).

**Goal:** Make every Twitch chat auth downgrade mark the platform as needing re-authorization, keep that mark stuck against the `oauth2/validate` 200 that cannot see it, clear it only when the credential pair actually changes, and make a live chat capture pick up repaired credentials the moment they land.

**Architecture:** Four seams, in dependency order. (1) `CookieJar.TwitchIdentity()` — a SHA-256 fingerprint of the `auth-token` + `login` pair, the Twitch counterpart of `YouTubeIdentity`. (2) `RefreshService.NoteTwitchAuthLoss(reason)` — a SECOND writer of `rs.status` under `rs.mu` that sets a sticky mark; `refresh`'s status block consults the mark, so validate cannot un-mark, and clears it when the fingerprint moves. (3) `twitch.ChatDownloader.Reauthenticate()` plus a `twitchChatRegistry` in the worker — the only path from "cookies changed" to a downloader that lives inside one job goroutine's call stack. (4) `OnCredentialsChanged` starts firing for `"twitch"` and drives the registry broadcast. Task 0 is a live probe that decides whether a fifth seam (the HLS playback token) is buildable at all.

**Tech Stack:** Go 1.26, `coder/websocket` (IRC), `modernc/sqlite`, chi/v5, bubbletea (TUI), vanilla JS + Shoelace (Web UI). No CGo.

**Spec:** `docs/superpowers/specs/2026-08-29-arc10-twitch-credential-lifecycle-design.md` — the plan argues from it; executors read both. Owner rulings Q3, Q3a-d, Q6 are recorded in `.superpowers/sdd/2026-08-25-cookie-subsystem-remediation/progress-arc9.md`. The seam inventory this plan verified line-by-line is `.superpowers/sdd/2026-08-25-cookie-subsystem-remediation/arc10-file-map.md`.

## Global Constraints

- **SECURITY, verbatim:** No cookie value, token, login, or webhook URL may appear in any log line, notification, payload, error string, test name, or test fixture. Fixtures use obviously fake tokens like `oauth:test-token-aaaa`. Live gates take a cookie-file PATH via an environment variable and never print anything read from it.
- **`const livenessRecoveryArmed = false` at `internal/cookies/refresh.go:632` stays false.** Arc 10's mark is a direct write, not a liveness observation; nothing here arms the pilot.
- **`cmd/moombox/main.go:276-278` is a no-touch zone.** (G5's `SetExpectedPlatforms` gate — analysed and OVERTURNED in Arc 1 execution.)
- **NEVER run two `go test ./...` at once.**
- **Gates, per task:** `go build ./...` · `go vet ./...` · `GOOS=linux go build ./...` · `gofmt -l internal/ cmd/` empty · `go test -count=1 ./...` reporting 27 packages ok / 0 fail from ONE run.
- **Standing test rule:** for EVERY assertion, this plan states the production mutation that breaks it. Bracketing an assertion to one function is no guard when a decoy sits inside it. Name checks and substring checks are no guard.
- **Every goroutine MUST have an inline `defer func() { if r := recover(); ... }()`.** Non-negotiable project rule.
- **The logger is an anonymous interface repeated per struct** (`Debug/Info/Warn/Error(msg string, args ...any)`). Do NOT extract a named interface.
- **Database writes go through `db.UpdateJobFields(id, map[string]any{...})`.** No DB change is expected in this arc; no new column, no schema bump.
- **API routes use the `/api/` prefix, no version.** No new route in this arc.
- **Web UI assets are `go:embed`-ed** — a frontend change would require `go build` to take effect. No frontend change is expected in this arc.
- **No new `CookieStatus` member, no new REST payload key, no job-row indicator** (owner ruling Q3: rejected as a symptom).
- **Do NOT bump the version or tag.** Release timing is the owner's call.
- **Commit messages end with these two trailer lines, exactly:**

```
Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
```

## Task list and model tier

| # | Title | Tier |
|---|-------|------|
| 0 | The HLS playback-token live probe (R6 premise) | sonnet |
| 1 | `CookieJar.TwitchIdentity()` | sonnet |
| 2 | The sticky mark: `NoteTwitchAuthLoss` + the vocabulary | opus |
| 3 | `prevTwitchIdentity` and `OnCredentialsChanged("twitch")` | opus |
| 4 | `ChatDownloader.Reauthenticate()` | opus |
| 5 | `twitchChatRegistry` and the ExecuteTwitch registration | opus |
| 6 | The worker seam: a downgrade marks the platform | sonnet |
| 7 | `cmd/moombox` wiring: mark, both repair edges, adapted sweep, and the two reason renderers | opus |
| 7a | Every credential write reaches the fingerprint comparison | opus |
| 8 | The HLS side, per Task 0's report (branch A builds; branch B records — neither edits a doc) | opus |
| 9 | The doc sentences that change with the code | sonnet |

Tasks 1-7a are strictly ordered (each Consumes the previous). Task 0 runs first and only its REPORT is a dependency (of Task 8). **Task 8 depends on Tasks 0, 2, 6 and 7** — 0 for the branch decision, 2 for the reason vocabulary it extends, 6 for `twitch_auth_loss_vocabulary_test.go`'s `reasons` slice, and 7 for the `dlWorker.SetOnTwitchAuthLoss(...)` block its wiring anchors to. Task 9 depends on everything, Task 0's report included.

Task 5 is opus despite its code being given verbatim: the registry's lock discipline, a `defer` placed inside an 850-line function with its own session loop, and the two-registries pitfall are concurrency judgement calls that a verbatim paste does not remove.

---

### Task 0: The HLS playback-token live probe (R6 premise)

Spec R6 makes the whole HLS half of this arc conditional on one measurement: does Twitch's playback access token say, in the clear, whether it was issued to a signed-in session? Nothing in the tree has ever decoded it — `TwitchAccessToken{Value, Signature}` (`internal/twitch/types.go:21-25`) is used opaquely, only as URL query parameters in `BuildUsherLiveURL` (`internal/twitch/api.go:847-866`). This task answers the question and writes the answer down. It builds nothing.

**Two facts verified in source that the probe depends on, and that the report must confirm:**

1. `Value` is **raw JSON, not URL-encoded**. `BuildUsherLiveURL` puts it into a `url.Values` map and calls `params.Encode()` (`api.go:864`), so the percent-encoding happens at URL-build time, at the very last moment, and never touches the struct field. The probe therefore feeds `token.Value` straight to `json.Unmarshal` with no `url.QueryUnescape` in front of it. If a decode fails, try `url.QueryUnescape` once and say so in the report — do not silently add it to the code.
2. `doGQLOnce` sets `Authorization: OAuth <token>` **only when the token is non-empty** (`api.go:288-291`). An empty token sends the request with no `Authorization` header at all, which is exactly the anonymous arm the probe needs, so the same `GetStreamAccessToken` call produces both replies with no other change.

**Files:**
- Create: `internal/twitch/playback_token_live_test.go`
- Read (do not modify): `internal/twitch/api.go:687-728` (`GetStreamAccessToken`), `internal/twitch/api.go:847-866` (`BuildUsherLiveURL`), `internal/twitch/auth_live_test.go:1-77` (the live-gate precedent this mirrors)

**Interfaces:**
- Consumes: `twitch.NewAPI(logger) *API`, `(*API).GetStreamAccessToken(ctx context.Context, channelLogin, authToken string) (*TwitchAccessToken, error)`, `cookies.NewCookieJar() *CookieJar`, `(*CookieJar).Load(path string) error`, `twitch.NewAuth(jar *cookies.CookieJar, logger …) *Auth`, `(*Auth).GetAuthToken() string`, `nopLogger{}` (already in `internal/twitch`'s test files)
- Produces: **a written report**, not code. The report is pasted into the Task 8 handoff and into `docs/spec/platform-services.md` by Task 9. It must state, for each of the two replies: the top-level field NAMES, each field's JSON type, the values of any BOOLEAN fields, and which names are present in one reply and not the other. It must state NO string value, NO number value, and nothing at all from the `Signature`.

- [ ] **Step 1: Write the probe**

Create `internal/twitch/playback_token_live_test.go`:

```go
package twitch

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// TestLivePlaybackTokenShape is Arc 10 Task 0: the ONE live measurement that
// decides whether the HLS half of this arc is buildable.
//
// The question. GetStreamAccessToken returns TwitchAccessToken{Value,
// Signature} and nothing else, and both halves are used opaquely as Usher
// query parameters — so today a DEAD auth-token produces a perfectly ordinary
// success: an ANONYMOUS playback token, served stitched ads and refused
// subscriber-only content, with nothing above Info in the log to say so. The
// token's Value is a JSON document. If that document states which session it
// was issued to, GetHLSMasterPlaylist can mark the platform the same way the
// chat handshake does, and a job with chat capture OFF stops being blind. If
// it does not, the finding is recorded and nothing is built on that side.
//
// WHAT THIS PRINTS, and why the rule is absolute. Field NAMES, JSON TYPES,
// BOOLEAN values, and the set difference between the two replies. Never a
// string value, never a number, never the Signature. The document is a signed
// entitlement: it carries a device id, a user ip and the token Usher accepts,
// and this file's output goes to a terminal, a CI log, and into a report an
// operator may paste. There is no field here worth reading whose value is
// worth printing — the whole question is answered by names, types and bools.
//
// Enable with:
//
//	MOOMBOX_LIVE_TWITCH_COOKIES=<path to a Netscape cookie file for a
//	                             signed-in Twitch session>
//	MOOMBOX_LIVE_TWITCH_CHANNEL=<the login of a channel that is LIVE right now>
//
// The path alone is the credential opt-in, matching TestLiveAuthenticatedToken-
// Validate in auth_live_test.go. The channel is a second required input rather
// than a hardcoded default: a default would rot, and an offline channel yields
// no stream playback token at all.
func TestLivePlaybackTokenShape(t *testing.T) {
	path := os.Getenv("MOOMBOX_LIVE_TWITCH_COOKIES")
	if path == "" {
		t.Skip("set MOOMBOX_LIVE_TWITCH_COOKIES=<path to a signed-in Netscape cookie file> to run the playback-token shape probe")
	}
	channel := os.Getenv("MOOMBOX_LIVE_TWITCH_CHANNEL")
	if channel == "" {
		t.Skip("set MOOMBOX_LIVE_TWITCH_CHANNEL=<login of a channel that is live right now> to run the playback-token shape probe")
	}

	jar := cookies.NewCookieJar()
	// Load reports only the path on failure, never file contents.
	if err := jar.Load(path); err != nil {
		t.Fatalf("load cookie file: %v", err)
	}
	auth := NewAuth(jar, nopLogger{})
	if !auth.HasAuthToken() {
		t.Fatal("that cookie file carries no Twitch auth-token cookie — check the export, or that the path is the right file")
	}

	api := NewAPI(nopLogger{})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// The authenticated reply. auth.GetAuthToken() is read once and passed
	// straight through; it is never assigned to a variable this test prints.
	authed, err := api.GetStreamAccessToken(ctx, channel, auth.GetAuthToken())
	if err != nil {
		t.Fatalf("authenticated GetStreamAccessToken failed: %.300s", err)
	}
	// The anonymous reply. An empty token makes doGQLOnce omit the
	// Authorization header entirely (api.go), which is the same request an
	// install with no Twitch cookies makes.
	anon, err := api.GetStreamAccessToken(ctx, channel, "")
	if err != nil {
		t.Fatalf("anonymous GetStreamAccessToken failed: %.300s", err)
	}

	authedShape := describePlaybackToken(t, "authenticated", authed.Value)
	anonShape := describePlaybackToken(t, "anonymous", anon.Value)

	// The set difference is the answer. A key present in one reply and not the
	// other, or a boolean that differs, is a statement about the session; two
	// identical shapes mean the reply cannot tell us and branch B applies.
	reportKeyDifference(t, authedShape, anonShape)
}

// playbackTokenField is one top-level key of the playback token document,
// reduced to the three things that may be reported.
type playbackTokenField struct {
	name string
	kind string // "bool" | "number" | "string" | "null" | "object" | "array"
	// boolValue is meaningful only when kind == "bool". Every other kind
	// reports its TYPE and stops: see the file comment.
	boolValue bool
}

// describePlaybackToken decodes one token Value and logs its shape.
//
// The decode is of the RAW field. BuildUsherLiveURL percent-encodes Value when
// it builds the Usher URL (url.Values.Encode), so the struct field itself is
// plain JSON and needs no unescaping. If json.Unmarshal fails here, try
// url.QueryUnescape ONCE by hand, record that in the report, and do not add
// the unescape to production code on the strength of one observation.
func describePlaybackToken(t *testing.T, label, value string) map[string]playbackTokenField {
	t.Helper()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value), &doc); err != nil {
		// The error is printed, the value is NOT: an unmarshal error from
		// encoding/json quotes the offending input.
		t.Fatalf("%s reply: the playback token Value is not a JSON object (error type %T) — "+
			"record this in the report and take branch B", label, err)
	}
	out := make(map[string]playbackTokenField, len(doc))
	names := make([]string, 0, len(doc))
	for name, raw := range doc {
		f := playbackTokenField{name: name, kind: jsonKindOf(raw)}
		if f.kind == "bool" {
			// The one value class that is safe to print, and the one most
			// likely to carry the answer.
			_ = json.Unmarshal(raw, &f.boolValue)
		}
		out[name] = f
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		f := out[name]
		if f.kind == "bool" {
			t.Logf("%s: %s = bool(%v)", label, f.name, f.boolValue)
			continue
		}
		t.Logf("%s: %s = %s", label, f.name, f.kind)
	}
	return out
}

// jsonKindOf names a raw JSON value's type without decoding it.
func jsonKindOf(raw json.RawMessage) string {
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		case '{':
			return "object"
		case '[':
			return "array"
		case '"':
			return "string"
		case 't', 'f':
			return "bool"
		case 'n':
			return "null"
		default:
			return "number"
		}
	}
	return "null"
}

// reportKeyDifference logs which keys and which booleans separate the two
// replies. This is the finding Task 8 branches on.
func reportKeyDifference(t *testing.T, authed, anon map[string]playbackTokenField) {
	t.Helper()
	var onlyAuthed, onlyAnon, differingBools, sameShape []string
	for name, f := range authed {
		other, ok := anon[name]
		if !ok {
			onlyAuthed = append(onlyAuthed, name)
			continue
		}
		switch {
		case f.kind != other.kind:
			differingBools = append(differingBools,
				name+": authenticated="+f.kind+" anonymous="+other.kind)
		case f.kind == "bool" && f.boolValue != other.boolValue:
			differingBools = append(differingBools,
				name+": authenticated=bool("+boolText(f.boolValue)+") anonymous=bool("+boolText(other.boolValue)+")")
		default:
			sameShape = append(sameShape, name)
		}
	}
	for name := range anon {
		if _, ok := authed[name]; !ok {
			onlyAnon = append(onlyAnon, name)
		}
	}
	sort.Strings(onlyAuthed)
	sort.Strings(onlyAnon)
	sort.Strings(differingBools)
	sort.Strings(sameShape)

	t.Logf("KEYS ONLY IN THE AUTHENTICATED REPLY: %v", onlyAuthed)
	t.Logf("KEYS ONLY IN THE ANONYMOUS REPLY:     %v", onlyAnon)
	t.Logf("KEYS WHOSE TYPE OR BOOLEAN DIFFERS:   %v", differingBools)
	t.Logf("KEYS THAT LOOK THE SAME IN BOTH:      %v", sameShape)

	if len(onlyAuthed) == 0 && len(onlyAnon) == 0 && len(differingBools) == 0 {
		t.Log("FINDING: the two replies are indistinguishable by name, type and boolean. " +
			"Arc 10 Task 8 takes BRANCH B — record the finding in the spec docs and build nothing on the HLS side.")
		return
	}
	t.Log("FINDING: the reply distinguishes an authenticated session from an anonymous one. " +
		"Arc 10 Task 8 takes BRANCH A — the discriminating key above is what PlaybackTokenSession reads.")
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
```

- [ ] **Step 2: Confirm the probe is skipped without the gate**

Run: `go test ./internal/twitch/ -run TestLivePlaybackTokenShape -v`
Expected: `--- SKIP: TestLivePlaybackTokenShape` with the "set MOOMBOX_LIVE_TWITCH_COOKIES" message. A FAIL here means the file does not compile or the test reached the network without its gate — either is a blocker.

- [ ] **Step 3: Run the probe against a live channel**

Run (PowerShell):

```powershell
$env:MOOMBOX_LIVE_TWITCH_COOKIES = "C:\path\to\cookies.txt"
$env:MOOMBOX_LIVE_TWITCH_CHANNEL = "somelivechannel"
go test ./internal/twitch/ -run TestLivePlaybackTokenShape -v -count=1
Remove-Item Env:\MOOMBOX_LIVE_TWITCH_COOKIES
Remove-Item Env:\MOOMBOX_LIVE_TWITCH_CHANNEL
```

Expected: PASS, with the four `KEYS …` lines and one `FINDING:` line. If it fails at `HasAuthToken`, the cookie file is wrong. If it fails at `GetStreamAccessToken`, the channel is offline — pick one that is live.

- [ ] **Step 4: Write the report into the task ledger**

Paste the four `KEYS …` lines and the `FINDING:` line verbatim into `.superpowers/sdd/2026-08-25-cookie-subsystem-remediation/progress-arc10.md` under a heading `## Task 0 report — playback token shape (<date>)`. Add one sentence recording whether `json.Unmarshal` accepted `token.Value` directly (it should) or needed `url.QueryUnescape`. **Task 8 reads only this ledger entry.**

- [ ] **Step 5: Run the gates**

```bash
go build ./...
go vet ./...
GOOS=linux go build ./...
gofmt -l internal/ cmd/
go test -count=1 ./...
```
Expected: build and vet clean, `gofmt -l` prints nothing, 27 packages ok / 0 fail.

- [ ] **Step 6: Commit**

```bash
git add internal/twitch/playback_token_live_test.go .superpowers/sdd/2026-08-25-cookie-subsystem-remediation/progress-arc10.md
git commit -m "$(cat <<'EOF'
test(twitch): a live probe reads the playback token's shape, and prints only names, types and bools

Arc 10 Task 0. GetStreamAccessToken returns two opaque fields, so a dead
auth-token yields an ordinary-looking ANONYMOUS playback token and the
capture silently takes stitched ads. The token Value is a JSON document;
this gate decodes it with and without credentials and reports which field
names, types and booleans differ. It reports no string and no number: the
document is a signed entitlement carrying a device id and a user ip.

Its finding decides whether the HLS half of Arc 10 is buildable at all.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

---

### Task 1: `CookieJar.TwitchIdentity()`

Spec R4. The jar has `YouTubeIdentity()` (`internal/cookies/jar.go:773-801`) and **no Twitch counterpart anywhere in the tree**. Every later task compares Twitch credential pairs, so this is the floor.

**The rule deliberately differs from `YouTubeIdentity`, and the difference is load-bearing.** `YouTubeIdentity` returns `""` when EITHER half is missing, because its question is "which Google ACCOUNT is this" and a SAPISID without LOGIN_INFO cannot answer it. Arc 10's question is "is this the same credential PAIR the downgrade was seen under", and a token with no `login` beside it is not an unanswerable state — it IS one of the four downgrade routes (`twitch.AuthDowngradeNoLoginCookie`). Copying YouTube's `||` gate would fingerprint that state as `""`, so the operator's fix (adding the `login` row) would compare `"" != ""` → unchanged, the mark would never clear, and R4 and R5 would both be dead for two of the four routes. So: `""` means "no Twitch credentials at all", and every other state, each half-pair included, gets its own fingerprint.

**Files:**
- Modify: `internal/cookies/jar.go` — insert immediately after `GetTwitchCredentials` (which ends at `:910`), before `IsEmpty` (`:914`)
- Test: `internal/cookies/jar_twitch_identity_test.go` (create)

**Interfaces:**
- Consumes: `j.twitch map[string]cookieEntry` and `j.mu sync.RWMutex` (jar-internal), `crypto/sha256` and `encoding/hex` (already imported by `jar.go` for `YouTubeIdentity`)
- Produces: `func (j *CookieJar) TwitchIdentity() string` — called by Task 2 (`NoteTwitchAuthLoss` samples it when taking the mark) and Task 3 (the status block samples it every pass). Task 7 does NOT call it: it receives the already-computed identity as `OnCredentialsChanged`'s second parameter, and only names this method in a corrected comment.

- [ ] **Step 1: Write the failing tests**

Create `internal/cookies/jar_twitch_identity_test.go`:

```go
package cookies

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every credential in this file is synthetic and none is ever logged.

// twitchJarWith writes a Netscape file holding the given Twitch pair and loads
// it. An empty half is written as an ABSENT ROW, not as an empty value — a
// half-written cookies.txt is what mergeCookieFiles' expiry pruning actually
// leaves behind, and it is the state the whole fingerprint exists to tell
// apart.
func twitchJarWith(t *testing.T, token, login string) *CookieJar {
	t.Helper()
	rows := []string{"# Netscape HTTP Cookie File"}
	if token != "" {
		rows = append(rows, strings.Join([]string{"#HttpOnly_.twitch.tv", "TRUE", "/", "TRUE", "0", "auth-token", token}, "\t"))
	}
	if login != "" {
		rows = append(rows, strings.Join([]string{".twitch.tv", "TRUE", "/", "TRUE", "0", "login", login}, "\t"))
	}
	path := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(path, []byte(strings.Join(rows, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	return jar
}

// TestTwitchIdentityMovesWithEitherHalf is the property Arc 10 R4 rests on.
//
// The mutation it catches is the obvious simplification: fingerprinting the
// auth-token alone. That version passes every "a new token is a change" test
// and silently breaks the two downgrade routes this arc exists for — an
// operator who adds the missing `login` row to a file whose token is fine
// would produce an IDENTICAL fingerprint, the mark would never clear, and the
// live chat session would never re-authenticate.
func TestTwitchIdentityMovesWithEitherHalf(t *testing.T) {
	base := twitchJarWith(t, "test-token-aaaa", "archiveraccount").TwitchIdentity()
	newToken := twitchJarWith(t, "test-token-bbbb", "archiveraccount").TwitchIdentity()
	newLogin := twitchJarWith(t, "test-token-aaaa", "otheraccount").TwitchIdentity()

	if base == "" {
		t.Fatal("a complete Twitch pair fingerprinted to the empty string")
	}
	if base == newToken {
		t.Error("a changed auth-token produced the same fingerprint — a re-exported credential would be invisible")
	}
	if base == newLogin {
		t.Error("a changed login produced the same fingerprint — an account switch would be invisible")
	}
	if newToken == newLogin {
		t.Error("changing the token and changing the login produced the SAME fingerprint — the two halves are not separated in the hash input")
	}
}

// TestTwitchIdentityIsEmptyOnlyWithNoCredentialsAtAll pins the deliberate
// divergence from YouTubeIdentity.
//
// The mutation: copying YouTubeIdentity's `if token == "" || login == ""`
// gate. Under it both half-pairs below fingerprint to "", so the transition
// from "token, no login" to "token, login" — the no-login-cookie repair — reads
// as no change at all.
func TestTwitchIdentityIsEmptyOnlyWithNoCredentialsAtAll(t *testing.T) {
	tokenOnly := twitchJarWith(t, "test-token-aaaa", "").TwitchIdentity()
	loginOnly := twitchJarWith(t, "", "archiveraccount").TwitchIdentity()
	neither := twitchJarWith(t, "", "").TwitchIdentity()
	complete := twitchJarWith(t, "test-token-aaaa", "archiveraccount").TwitchIdentity()

	if tokenOnly == "" {
		t.Error("a jar holding an auth-token with no login fingerprinted to \"\" — that is one of the four downgrade routes, not an absence of credentials")
	}
	if loginOnly == "" {
		t.Error("a jar holding a login with no auth-token fingerprinted to \"\"")
	}
	if neither != "" {
		t.Errorf("a jar with no Twitch credentials fingerprinted to %q, want \"\"", neither)
	}
	if tokenOnly == complete || loginOnly == complete || tokenOnly == loginOnly {
		t.Error("two different Twitch credential states share a fingerprint")
	}
}

// TestTwitchIdentityRevealsNoCredential is the security property. The value is
// compared in code paths near logging and is carried on a RefreshService
// field, while both inputs are secrets: one is a bearer token, the other names
// the signed-in account.
//
// The mutation: returning token+"\x00"+login unhashed, or hex-encoding it
// rather than its digest.
func TestTwitchIdentityRevealsNoCredential(t *testing.T) {
	id := twitchJarWith(t, "test-token-aaaa", "archiveraccount").TwitchIdentity()
	if strings.Contains(id, "test-token-aaaa") || strings.Contains(id, "archiveraccount") {
		t.Error("the fingerprint contains a credential verbatim")
	}
	if len(id) != 64 {
		t.Errorf("fingerprint length = %d, want 64 — a SHA-256 digest in lowercase hex", len(id))
	}
	for _, r := range id {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("fingerprint contains a non-hex character %q — it is not a digest", r)
		}
	}
}

// TestTwitchIdentityIsStable: two reads of one unchanged jar must be equal, or
// every refresh pass would look like a credential change and drop every live
// chat session on a 30-minute timer.
//
// The mutation: mixing time.Now, a counter, or a random salt into the hash.
func TestTwitchIdentityIsStable(t *testing.T) {
	jar := twitchJarWith(t, "test-token-aaaa", "archiveraccount")
	if a, b := jar.TwitchIdentity(), jar.TwitchIdentity(); a != b {
		t.Errorf("two reads of one jar differ: %q vs %q", a, b)
	}
}

// TestTwitchIdentityNilReceiver: RefreshService may hold a nil jar in a
// partially constructed process, and YouTubeIdentity is nil-safe for the same
// reason. The mutation: dropping the nil guard (a panic at boot).
func TestTwitchIdentityNilReceiver(t *testing.T) {
	var jar *CookieJar
	if got := jar.TwitchIdentity(); got != "" {
		t.Errorf("nil jar fingerprint = %q, want \"\"", got)
	}
}

// TestTwitchIdentityReadsOnlyTheTwitchJar: Arc 5 split the jar in two, and a
// name-keyed read against the wrong map is the failure that split exists to
// prevent. A YouTube-only file must fingerprint to "".
//
// The mutation: reading j.jarFor(PlatformYouTube), or a pre-Arc-5 single map.
func TestTwitchIdentityReadsOnlyTheTwitchJar(t *testing.T) {
	jar := jarWithAuth(t) // YouTube SAPISID + LOGIN_INFO, no Twitch rows
	if got := jar.TwitchIdentity(); got != "" {
		t.Errorf("a YouTube-only jar produced a Twitch fingerprint %q", got)
	}
	if jar.YouTubeIdentity() == "" {
		t.Fatal("the fixture is wrong: this jar was supposed to hold YouTube credentials")
	}
}
```

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/cookies/ -run TestTwitchIdentity -v`
Expected: compile failure — `jar.TwitchIdentity undefined (type *CookieJar has no field or method TwitchIdentity)`.

- [ ] **Step 3: Implement `TwitchIdentity`**

In `internal/cookies/jar.go`, insert after `GetTwitchCredentials`'s closing brace (`:910`):

```go
// TwitchIdentity returns a stable, non-reversible fingerprint of WHICH Twitch
// credential pair the jar currently holds — "" only when it holds neither
// half.
//
// The counterpart of YouTubeIdentity, and deliberately NOT its rule.
// YouTubeIdentity requires BOTH halves and returns "" if either is missing,
// because its question is "which Google ACCOUNT is this" and a SAPISID without
// LOGIN_INFO cannot answer it. The question HERE is "is this the same
// credential PAIR the chat downgrade was observed under", and an auth-token
// with no login beside it is not an unanswerable state — it is one of the four
// routes a job with credentials goes anonymous by (twitch.AuthDowngradeNo-
// LoginCookie). Folding it to "" would make the operator's fix, adding the
// login row, compare equal to the broken state it replaced: the auth mark
// would never clear and the live chat session would never re-authenticate. So
// "" means "no Twitch credentials at all" and nothing else.
//
// A changed fingerprint is a HINT, not proof, in the same direction
// YouTubeIdentity chose: Twitch rotates auth-token on its own schedule, so a
// same-account rotation reads as a change. That direction is cheap — one
// re-check and one IRC reconnect, both of which the credentials will pass. The
// opposite error, missing a real credential change, strands a capture in
// anonymous chat for the rest of the job.
//
// Hashed rather than returned raw because this value is compared in code paths
// near logging and is held on a RefreshService field, while both inputs are
// among the highest-value secrets the app holds: one is a bearer token, the
// other names the signed-in account. Callers must treat it as an opaque
// equality token — never as a credential, and never as something to display.
func (j *CookieJar) TwitchIdentity() string {
	if j == nil {
		return ""
	}
	// ONE RLock covering both reads, for the reason GetTwitchCredentials
	// documents at length: Load swaps the whole map under Lock, so two
	// separate locks could pair a token from the pre-Reload jar with a login
	// from the post-Reload one and fingerprint a pair that never existed.
	j.mu.RLock()
	token := j.twitch["auth-token"].value
	login := j.twitch["login"].value
	j.mu.RUnlock()

	if token == "" && login == "" {
		return ""
	}
	// NUL separator: neither cookie may contain one (rowBreakingChars covers
	// tab, CR, LF and NUL), so no pair of distinct (token, login) inputs can
	// concatenate to the same string.
	sum := sha256.Sum256([]byte(token + "\x00" + login))
	return hex.EncodeToString(sum[:])
}
```

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/cookies/ -run TestTwitchIdentity -v`
Expected: PASS for all six tests.

- [ ] **Step 5: Run the gates**

```bash
go build ./...
go vet ./...
GOOS=linux go build ./...
gofmt -l internal/ cmd/
go test -count=1 ./...
```
Expected: build and vet clean, `gofmt -l` prints nothing, 27 packages ok / 0 fail.

- [ ] **Step 6: Commit**

```bash
git add internal/cookies/jar.go internal/cookies/jar_twitch_identity_test.go
git commit -m "$(cat <<'EOF'
feat(cookies): the jar fingerprints the Twitch credential pair

Arc 10 R4. YouTubeIdentity had no Twitch counterpart, so nothing could
answer "are these the same credentials the downgrade was seen under" —
the question every later part of this arc turns on.

The rule differs from YouTubeIdentity on purpose: "" means no Twitch
credentials AT ALL, not "a half-pair". A token with no login beside it is
one of the four routes a job goes anonymous by, and folding it to "" would
make the repair — adding the login row — compare equal to the breakage.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

---

### Task 2: The sticky mark — `NoteTwitchAuthLoss` and the reason vocabulary

Spec R1, R2, R3. Today `AuthStatus.TwitchAuthenticated` is written in exactly one place: `refresh`'s status block (`internal/cookies/refresh.go:1150-1242`), from `checkTwitchAuth` (`:3048-3132`). That check hits `id.twitch.tv/oauth2/validate`, which answers **200 for a valid token even when the `login` cookie is missing** — so `no-login-cookie` and `unusable-login-cookie` stay green forever while every subscriber-only message is dropped. `ObserveLiveness` cannot help: it is disarmed (`livenessRecoveryArmed = false`, `:632`) **and** structurally never touches `rs.status`.

This task adds the second writer and the rule between the two.

**The mark is fingerprint-keyed, not auth-keyed, and that is a deliberate refinement of R4.** The mark carries the `TwitchIdentity()` it was taken under, and `refresh` clears it whenever the jar's fingerprint differs — with **no** `nowAuth` gate. If the mark only cleared on a change that also authenticated, then replacing a broken pair with a DEAD one would leave a stale mark saying `no-login-cookie` while the truth is `401`. Clearing on the fingerprint alone and letting validate write the truth is both simpler and more honest, and it is what R4's sentence says ("the mark clears and validate decides the status again").

**Files:**
- Modify: `internal/cookies/refresh.go` — new constants + `twitchAuthMark` type + `twitchAuthLossMessage` after `verdictFromCheck` (`:312-323`); a `twitchMark` field on `RefreshService` beside `ytEverConcluded`/`twEverConcluded` (`:441-442`); `NoteTwitchAuthLoss` after `CheckTwitchAuth` (`:1003-1005`); the mark consult inside `refresh`'s status block (`:1141-1242`) and the two Twitch decisions below it (`:1279-1290`, `:1292-1305`)
- Test: `internal/cookies/refresh_twitch_mark_test.go` (create)

**Interfaces:**
- Consumes: `(*CookieJar).TwitchIdentity() string` (Task 1); `shouldFireRecovery(everConcluded, prevAuth, nowAuth bool, checkErr error, cookiesPresent bool) bool` (`:1422`); `authStatusChanged(prev, next AuthStatus) bool` (`:361`); `verdictFromCheck(authenticated bool, err error) RefreshVerdict` (`:312`); `(*RefreshService).noteRecoveryDecided(platform string, now time.Time)` (`:975`); `(*CookieJar).HasAnyTwitchAuthCookie() bool` (`jar.go:715`)
- Produces:
  - `func (rs *RefreshService) NoteTwitchAuthLoss(reason string)` — Tasks 6 and 7 call it; Task 8 branch A calls it
  - unexported `const twitchLossLoginRefused = "login-refused"`, `twitchLossLoginUnacknowledged = "login-never-acknowledged"`, `twitchLossNoLoginCookie = "no-login-cookie"`, `twitchLossUnusableLoginCookie = "unusable-login-cookie"` — Task 8 branch A adds a fifth
  - unexported `func twitchAuthLossMessage(reason string) string`
  - unexported `type twitchAuthMark struct { set bool; reason string; identity string }`

- [ ] **Step 1: Write the failing tests**

Create `internal/cookies/refresh_twitch_mark_test.go`:

```go
package cookies

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Arc 10 R1-R3. Every credential in this file is synthetic and none is logged.
//
// The defect these pin: oauth2/validate answers 200 for a valid auth-token
// whether or not a `login` cookie sits beside it, so two of the four ways a
// Twitch capture goes anonymous were invisible to the only thing that writes
// AuthStatus.TwitchAuthenticated. A mark that validate could clear would be no
// mark at all — it would be erased within one 30-minute tick with nothing
// fixed.

// twitchMarkFixture writes a Twitch cookie file, loads it, and returns the
// service pointed at a validate server that answers `code`. The YouTube guide
// seam is pinned too: an unpinned seam is one refactor away from youtube.com.
func twitchMarkFixture(t *testing.T, token, login string, code int) (*RefreshService, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cookies.txt")
	writeTwitchPair(t, path, token, login)
	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	pointTwitchValidateAt(t, statusServer(t, code))
	ytSrv, _ := countingGuide(t, loggedOutGuideBody)
	pointYouTubeGuideAt(t, ytSrv)
	return NewRefreshService(jar, 0, nopLogger{}), path
}

// writeTwitchPair writes exactly the rows the given pair implies. An empty
// half is an ABSENT ROW — the state mergeCookieFiles' expiry pruning leaves.
func writeTwitchPair(t *testing.T, path, token, login string) {
	t.Helper()
	rows := []string{"# Netscape HTTP Cookie File"}
	if token != "" {
		rows = append(rows, strings.Join([]string{"#HttpOnly_.twitch.tv", "TRUE", "/", "TRUE", "0", "auth-token", token}, "\t"))
	}
	if login != "" {
		rows = append(rows, strings.Join([]string{".twitch.tv", "TRUE", "/", "TRUE", "0", "login", login}, "\t"))
	}
	if err := os.WriteFile(path, []byte(strings.Join(rows, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestValidate200DoesNotClearAStandingTwitchMark is R3, and the whole point of
// the arc.
//
// The mutation: deleting the mark consult from refresh's status block, so the
// block writes verdictFromCheck(twAuth=true, nil) = RefreshOK straight over the
// mark. Under it the operator sees green within one tick while the capture is
// still dropping every subscriber-only message.
func TestValidate200DoesNotClearAStandingTwitchMark(t *testing.T) {
	rs, _ := twitchMarkFixture(t, "test-token-aaaa", "", http.StatusOK)

	rs.NoteTwitchAuthLoss(twitchLossNoLoginCookie)
	rs.doRefresh(context.Background())

	got := rs.GetStatus()
	if got.TwitchAuthenticated {
		t.Error("a validate 200 cleared a standing mark — the two routes validate cannot see are green again with nothing fixed")
	}
	if got.TwitchVerification != RefreshFailed {
		t.Errorf("TwitchVerification = %v, want RefreshFailed — the mark is conclusive", got.TwitchVerification)
	}
	if want := twitchAuthLossMessage(twitchLossNoLoginCookie); got.TwitchError != want {
		t.Errorf("TwitchError = %q, want %q — the mark owns the reason while it stands", got.TwitchError, want)
	}
}

// TestATwitchCredentialChangeClearsTheMark is R4's clearing half.
//
// Two mutations. Never clearing: the mark outlives every repair and the
// platform reads dead forever. And re-sampling `rs.twitchMark.identity` on
// each pass instead of holding the value the mark was TAKEN under: it then
// always equals `twIdentity`, so the comparison can never be unequal and the
// mark never clears — the same visible failure, from the opposite mistake.
func TestATwitchCredentialChangeClearsTheMark(t *testing.T) {
	rs, path := twitchMarkFixture(t, "test-token-aaaa", "", http.StatusOK)

	rs.NoteTwitchAuthLoss(twitchLossNoLoginCookie)
	rs.doRefresh(context.Background())
	if rs.GetStatus().TwitchAuthenticated {
		t.Fatal("the fixture is wrong: the mark did not stand through the first pass")
	}

	// The operator's fix: the same token with its login row restored.
	writeTwitchPair(t, path, "test-token-aaaa", "archiveraccount")
	rs.doRefresh(context.Background())

	got := rs.GetStatus()
	if !got.TwitchAuthenticated {
		t.Error("a changed credential pair did not clear the mark — validate never gets to decide again")
	}
	if got.TwitchVerification != RefreshOK {
		t.Errorf("TwitchVerification = %v, want RefreshOK", got.TwitchVerification)
	}
	if got.TwitchError != "" {
		t.Errorf("TwitchError = %q, want \"\" — the mark's reason must not survive the credential that caused it", got.TwitchError)
	}
}

// TestAChangeToDeadCredentialsClearsTheMarkAndReportsTheTruth: clearing is
// keyed on the FINGERPRINT alone, with no authenticated gate.
//
// The mutation: gating the clear on nowAuth. Under it, replacing a
// no-login-cookie pair with a pair whose token is revoked leaves the stale
// sentence about a missing login row in front of an operator whose actual
// problem is a 401.
func TestAChangeToDeadCredentialsClearsTheMarkAndReportsTheTruth(t *testing.T) {
	rs, path := twitchMarkFixture(t, "test-token-aaaa", "", http.StatusUnauthorized)

	rs.NoteTwitchAuthLoss(twitchLossNoLoginCookie)
	writeTwitchPair(t, path, "test-token-bbbb", "archiveraccount")
	rs.doRefresh(context.Background())

	got := rs.GetStatus()
	if got.TwitchAuthenticated {
		t.Error("a 401 reported as authenticated")
	}
	if got.TwitchError == twitchAuthLossMessage(twitchLossNoLoginCookie) {
		t.Error("the mark's stale reason survived a credential change — the operator is told to fix a login row while the real answer is a rejected token")
	}
}

// TestTwitchMarkFiresRecoveryOncePerLoss is R2.
//
// Two mutations: dropping `rs.prevTwitchAuth = false` after the fire (every
// later downgrade on the same dead pair fires again, and with auto_enabled off
// that is a TypeError notification per refusal), and dropping the
// shouldFireRecovery gate entirely (recovery fires for a platform nobody
// configured).
func TestTwitchMarkFiresRecoveryOncePerLoss(t *testing.T) {
	rs, path := twitchMarkFixture(t, "test-token-aaaa", "", http.StatusOK)
	var fired []string
	rs.OnRecoveryNeeded = func(platform string) { fired = append(fired, platform) }

	// A first conclusive check, so the baseline is "authenticated" and the next
	// loss is a WITNESSED transition rather than the startup case.
	rs.doRefresh(context.Background())
	if len(fired) != 0 {
		t.Fatalf("recovery fired %v on a healthy first pass", fired)
	}

	rs.NoteTwitchAuthLoss(twitchLossNoLoginCookie)
	rs.NoteTwitchAuthLoss(twitchLossLoginRefused)
	if len(fired) != 1 || fired[0] != "twitch" {
		t.Fatalf("recovery fired %v, want exactly one [twitch] for one loss", fired)
	}

	// A repair, then a second loss: a NEW loss must be reported.
	writeTwitchPair(t, path, "test-token-bbbb", "archiveraccount")
	rs.doRefresh(context.Background())
	rs.NoteTwitchAuthLoss(twitchLossLoginRefused)
	if len(fired) != 2 {
		t.Errorf("recovery fired %v, want a second fire after the credentials were repaired and lost again", fired)
	}
}

// TestTwitchMarkNeverFiresRecoveryForAnUnconfiguredPlatform: the false-alarm
// guard. A mark that ignored cookiesPresent would send someone to re-export
// credentials they never had — and in a container the remedy it names may not
// even be reachable.
//
// The mutation: passing `true` for cookiesPresent, or dropping the argument.
func TestTwitchMarkNeverFiresRecoveryForAnUnconfiguredPlatform(t *testing.T) {
	pointTwitchValidateAt(t, statusServer(t, http.StatusUnauthorized))
	ytSrv, _ := countingGuide(t, loggedOutGuideBody)
	pointYouTubeGuideAt(t, ytSrv)

	rs := NewRefreshService(NewCookieJar(), 0, nopLogger{})
	var fired []string
	rs.OnRecoveryNeeded = func(platform string) { fired = append(fired, platform) }

	rs.NoteTwitchAuthLoss(twitchLossLoginRefused)

	if len(fired) != 0 {
		t.Errorf("recovery fired %v on an empty jar", fired)
	}
}

// TestTwitchAuthLossReasonIsTheVocabularyOnly is the leak barrier.
//
// AuthStatus.TwitchError reaches two per-request operator surfaces
// (routes.TwitchAuthStatusPayload's `twitchError` and the TUI's R C result
// line). Because every arm of twitchAuthLossMessage returns a string LITERAL,
// the set of strings that field can hold is fixed at compile time and no
// caller can widen it.
//
// The mutation: `return reason`, or any fmt.Sprintf that interpolates it. Both
// pass a "the status says something" assertion, and both put caller-controlled
// text — one upstream change away from a value read off the wire — in front of
// an operator.
func TestTwitchAuthLossReasonIsTheVocabularyOnly(t *testing.T) {
	known := []string{
		twitchLossLoginRefused,
		twitchLossLoginUnacknowledged,
		twitchLossNoLoginCookie,
		twitchLossUnusableLoginCookie,
	}
	seen := map[string]string{}
	generic := twitchAuthLossMessage("a-token-no-arm-was-ever-written-for")
	for _, reason := range known {
		msg := twitchAuthLossMessage(reason)
		if msg == generic {
			t.Errorf("%q renders the fallback sentence — it has no arm of its own", reason)
		}
		if prev, dup := seen[msg]; dup {
			t.Errorf("%q and %q render the same sentence %q", prev, reason, msg)
		}
		seen[msg] = reason
		if strings.Contains(msg, reason) {
			t.Errorf("the sentence for %q contains the raw token: %q", reason, msg)
		}
	}

	// An off-vocabulary input, shaped like the thing that must never reach this
	// function, must render the fallback and carry none of its input.
	leaky := "auth-token=test-token-aaaa; login=archiveraccount"
	if msg := twitchAuthLossMessage(leaky); msg != generic || strings.Contains(msg, "test-token-aaaa") {
		t.Errorf("an off-vocabulary reason rendered %q — the switch is not the barrier it claims to be", msg)
	}

	rs, _ := twitchMarkFixture(t, "test-token-aaaa", "archiveraccount", http.StatusOK)
	rs.NoteTwitchAuthLoss(leaky)
	if got := rs.GetStatus().TwitchError; got != generic {
		t.Errorf("AuthStatus.TwitchError = %q after an off-vocabulary mark, want the fallback sentence", got)
	}
}

// TestTwitchMarkLeavesYouTubeAlone: the mark writes THREE fields, not a whole
// AuthStatus.
//
// The mutation: `rs.status = AuthStatus{TwitchAuthenticated: false, ...}`,
// which silently zeroes YouTube's verdict, its reason and its cookies-present
// flag — so a Twitch chat downgrade repaints the YouTube badge.
func TestTwitchMarkLeavesYouTubeAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.txt")
	content := "# Netscape HTTP Cookie File\n" +
		".youtube.com\tTRUE\t/\tTRUE\t0\tSAPISID\tsapisid-value\n" +
		".youtube.com\tTRUE\t/\tTRUE\t0\tLOGIN_INFO\tlogin-info-value\n" +
		"#HttpOnly_.twitch.tv\tTRUE\t/\tTRUE\t0\tauth-token\ttest-token-aaaa\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	pointTwitchValidateAt(t, statusServer(t, http.StatusOK))
	ytSrv, _ := countingGuide(t, loggedInGuideBody)
	pointYouTubeGuideAt(t, ytSrv)

	rs := NewRefreshService(jar, 0, nopLogger{})
	rs.doRefresh(context.Background())
	before := rs.GetStatus()
	if !before.YouTubeAuthenticated {
		t.Fatal("the fixture is wrong: YouTube did not authenticate")
	}

	rs.NoteTwitchAuthLoss(twitchLossNoLoginCookie)

	after := rs.GetStatus()
	if after.YouTubeAuthenticated != before.YouTubeAuthenticated ||
		after.YouTubeVerification != before.YouTubeVerification ||
		after.YouTubeError != before.YouTubeError ||
		after.HasYouTubeCookies != before.HasYouTubeCookies {
		t.Errorf("a Twitch mark moved the YouTube half of AuthStatus: before %+v, after %+v", before, after)
	}
}

// TestTwitchMarkFiresAuthChangeOnAVerdictTransitionOnly pins the mark to
// authStatusChanged's existing CONTRACT rather than to a second rule.
//
// authStatusChanged deliberately excludes the two reason strings, because no
// OnAuthChange-driven surface may render them. A second mark that changes only
// the REASON must therefore fire no push; the per-request surfaces read the
// string fresh anyway.
//
// The mutation: calling OnAuthChange unconditionally from NoteTwitchAuthLoss.
// Not merely noisy — it repaints two dashboards for an event neither displays.
func TestTwitchMarkFiresAuthChangeOnAVerdictTransitionOnly(t *testing.T) {
	rs, _ := twitchMarkFixture(t, "test-token-aaaa", "archiveraccount", http.StatusOK)
	var pushes int
	rs.OnAuthChange = func(AuthStatus) { pushes++ }

	rs.doRefresh(context.Background()) // twitch reads authenticated
	pushes = 0

	rs.NoteTwitchAuthLoss(twitchLossLoginRefused)
	if pushes != 1 {
		t.Fatalf("OnAuthChange fired %d times for the authenticated -> marked transition, want 1", pushes)
	}
	rs.NoteTwitchAuthLoss(twitchLossNoLoginCookie)
	if pushes != 1 {
		t.Errorf("OnAuthChange fired %d times total; a reason-only change must fire no push (authStatusChanged excludes the strings)", pushes)
	}
}
```

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/cookies/ -run 'TestValidate200|TestATwitchCredential|TestAChangeToDead|TestTwitchMark|TestTwitchAuthLoss' -v`
Expected: compile failure — `undefined: twitchLossNoLoginCookie`, `undefined: twitchAuthLossMessage`, `rs.NoteTwitchAuthLoss undefined`.

- [ ] **Step 3: Add the vocabulary, the mapper and the mark type**

In `internal/cookies/refresh.go`, immediately after `verdictFromCheck`'s closing brace (`:323`):

```go
// The fixed vocabulary of NoteTwitchAuthLoss's reason.
//
// These mirror internal/twitch's AuthDowngrade* constants BY VALUE and cannot
// import them: internal/twitch imports THIS package (twitch/auth.go,
// twitch/service.go), so the dependency only runs one way. The pin against
// drift lives in internal/worker, which imports both — see
// TestTwitchAuthLossVocabularyCoversEveryDowngradeReason.
//
// Opaque tokens, never sentences and never format strings: there is no verb
// here to interpolate a token, a login or a wire line into.
const (
	twitchLossLoginRefused        = "login-refused"
	twitchLossLoginUnacknowledged = "login-never-acknowledged"
	twitchLossNoLoginCookie       = "no-login-cookie"
	twitchLossUnusableLoginCookie = "unusable-login-cookie"
)

// twitchAuthLossMessage renders the operator sentence for one downgrade route.
//
// THE SWITCH IS THE LEAK BARRIER, not a convenience. NoteTwitchAuthLoss's
// caller lives in internal/worker and hands over a token it received from
// internal/twitch; AuthStatus.TwitchError then reaches two per-request
// operator surfaces (routes.TwitchAuthStatusPayload's `twitchError` and the
// TUI's R C result line). Because every arm below returns a string LITERAL,
// the SET of strings that field can hold is fixed at compile time and no
// input — not a future upstream token, not a value read off the wire — can
// widen it. Returning the reason, or interpolating it, would move that
// guarantee from the type system to the caller's discipline.
//
// The default arm exists for a token added upstream without an arm here. It
// must still say a credential is broken: a status line that names no problem
// is worse than the log line it was meant to escape.
func twitchAuthLossMessage(reason string) string {
	switch reason {
	case twitchLossLoginRefused:
		return "Twitch refused the saved login."
	case twitchLossLoginUnacknowledged:
		return "Twitch never acknowledged the saved login."
	case twitchLossNoLoginCookie:
		return "The cookie file has a Twitch auth-token but no login cookie beside it."
	case twitchLossUnusableLoginCookie:
		return "The Twitch login cookie is not a name that can be sent to chat."
	default:
		return "The saved Twitch login could not be used."
	}
}

// twitchAuthMark is a Twitch credential failure observed somewhere OTHER than
// the periodic oauth2/validate check, held until the credential pair changes.
//
// It exists because validate CANNOT SEE two of the four ways a Twitch capture
// goes anonymous. An auth-token with no `login` beside it, and one with a
// `login` that cannot be sent as an IRC nickname, both leave the TOKEN valid —
// so validate answers 200, the platform reads green forever, and every
// subscriber-only message and badge is dropped for the whole job. A mark that
// validate could overwrite would therefore be no mark at all: it would be
// erased within one 30-minute tick with nothing fixed.
//
// The zero value is "no mark". `identity` is CookieJar.TwitchIdentity() as of
// the moment the mark was taken, and it is the ONLY thing that clears the mark
// — see refresh's status block. `reason` is a member of the vocabulary above
// and never anything read from the jar or the wire.
type twitchAuthMark struct {
	set      bool
	reason   string
	identity string
}
```

- [ ] **Step 4: Add the `twitchMark` field**

In `internal/cookies/refresh.go`, in the `RefreshService` struct, immediately after the `ytEverConcluded` / `twEverConcluded` pair (`:441-442`):

```go
	// twitchMark holds a Twitch credential failure that oauth2/validate cannot
	// see, and it is why rs.status has TWO writers rather than one. Written
	// under mu by NoteTwitchAuthLoss; consulted and cleared under mu by
	// refresh's status block. See twitchAuthMark.
	twitchMark twitchAuthMark
```

- [ ] **Step 5: Add `NoteTwitchAuthLoss`**

In `internal/cookies/refresh.go`, immediately after `CheckTwitchAuth` (`:1003-1005`):

```go
// NoteTwitchAuthLoss records that Twitch credentials this install HOLDS were
// refused, or could not be used, by something other than the periodic
// oauth2/validate check — today the IRC chat handshake.
//
// THIS IS THE SECOND WRITER OF rs.status, and the only one that is not
// refresh's status block. Both write under rs.mu, and the rule between them is
// stated once, here, and enforced there: while a mark stands it WINS for
// TwitchAuthenticated, TwitchVerification and TwitchError, and only a changed
// credential fingerprint clears it. The sole-writer property that used to hold
// is gone on purpose; nothing else about the locking discipline changed.
//
// reason must be a member of the fixed vocabulary (twitchLossLoginRefused and
// friends). Nothing derived from a cookie value, a login, or a wire line may
// be passed here — and twitchAuthLossMessage refuses to render anything else
// anyway, which is what keeps AuthStatus.TwitchError's contents a compile-time
// set rather than a caller's promise.
//
// Recovery uses the SAME dedupe a validate-found loss gets. shouldFireRecovery
// is evaluated against this platform's own two pieces of baseline state and
// they are then advanced exactly as refresh advances them, so one loss raises
// one alarm however many times this is called for it, and a loss that follows
// a genuine repair raises a new one.
//
// nowAuth=false and checkErr=nil are not assumptions: a downgrade IS the
// conclusive negative. Something tried to use these credentials against Twitch
// and Twitch would not take them, which is a stronger statement than the
// endpoint check makes.
//
// Callers reach this from ChatDownloader's OnAuthDowngrade, which runs on the
// IRC session goroutine with the read loop parked behind it. This function
// makes no network call and holds no lock across a callback — but the
// callbacks it invokes may block (handleRecoveryNeeded's auto_enabled=false
// arm sends a webhook synchronously), so cmd/moombox's wiring calls it on its
// own goroutine. See the SetOnTwitchAuthLoss wiring in cmd/moombox/services.go.
func (rs *RefreshService) NoteTwitchAuthLoss(reason string) {
	var (
		changed      bool
		fireRecovery bool
		statusCopy   AuthStatus
	)
	// Scoped into a func literal so the unlock is DEFERRED, for the reason
	// refresh's own status block documents: rs.mu is a plain non-reentrant
	// RWMutex, and a panic unwinding with the write lock held would park the
	// goroutine holding it and block every later GetStatus forever.
	func() {
		rs.mu.Lock()
		defer rs.mu.Unlock()

		prev := rs.status
		rs.twitchMark = twitchAuthMark{
			set:    true,
			reason: reason,
			// Sampled under the same lock as the write, so the mark can never
			// be keyed to a pair that was already replaced by the time it
			// landed.
			identity: rs.jar.TwitchIdentity(),
		}
		rs.status.TwitchAuthenticated = false
		rs.status.TwitchVerification = RefreshFailed
		rs.status.TwitchError = twitchAuthLossMessage(reason)
		changed = authStatusChanged(prev, rs.status)
		statusCopy = rs.status

		// The dedupe, decided under the lock and advanced under it, so two
		// concurrent downgrades on one dead pair cannot both witness the
		// transition.
		fireRecovery = rs.OnRecoveryNeeded != nil &&
			shouldFireRecovery(rs.twEverConcluded, rs.prevTwitchAuth, false, nil, rs.jar.HasAnyTwitchAuthCookie())
		rs.prevTwitchAuth = false
		rs.twEverConcluded = true
		// hasCheckedOnce is deliberately NOT touched. It is service-wide and
		// means "a refresh pass has completed"; a chat downgrade is not one,
		// and setting it here would let a Twitch handshake decide whether
		// YouTube's first OnAuthRecovered transition is allowed to fire.
	}()

	// Both callbacks reach out into cmd/moombox and must not run under this
	// service's mutex, following refresh's convention exactly.
	if changed && rs.OnAuthChange != nil {
		rs.OnAuthChange(statusCopy)
	}
	if fireRecovery {
		// Stamp the shared dedupe map for the same reason the tier-1 fire does:
		// a liveness verdict landing in the same window must not fire recovery
		// for a problem this one is already working on.
		rs.noteRecoveryDecided("twitch", time.Now())
		// The SENTENCE, not the caller's `reason`. This function's own doc
		// says the switch is the leak barrier rather than the caller's
		// discipline, and logging the raw argument would quietly make that
		// false — TestTwitchAuthLossReasonIsTheVocabularyOnly deliberately
		// passes a credential-shaped string through this exact call.
		rs.logger.Warn("twitch credentials were refused where they were used, triggering recovery",
			"reason", twitchAuthLossMessage(reason))
		rs.OnRecoveryNeeded("twitch")
	}
}
```

- [ ] **Step 6: Consult the mark inside `refresh`'s status block**

In `refresh`, extend the `var (...)` block above the status func literal (`:1141-1149`) with two names:

```go
		twIdentity                 string
		twEffective                bool
```

Inside the func literal, immediately after the existing `ytIdentity = rs.jar.YouTubeIdentity()` / `prevYTIdentity = rs.prevYouTubeIdentity` pair, add:

```go
		twIdentity = rs.jar.TwitchIdentity()

		// THE MARK, and the rule that makes rs.status's two writers coherent.
		//
		// A downgrade observed outside this check (NoteTwitchAuthLoss) stands
		// until the credential PAIR changes, and while it stands it wins over
		// validate for every Twitch conclusion drawn below. It has to: validate
		// answers 200 for a valid auth-token whether or not a usable `login`
		// sits beside it, so without this a no-login-cookie downgrade would be
		// erased on the next tick with nothing repaired.
		//
		// Clearing is keyed on the FINGERPRINT ALONE, with no authenticated
		// gate. Gating it on nowAuth would leave a stale mark in front of an
		// operator whose broken pair was replaced by a REVOKED one: they would
		// be told to add a login row while the real answer is a 401. Clearing
		// here and letting validate write the truth is both simpler and
		// honest — which is what "the mark clears and validate decides the
		// status again" says.
		twMarked := false
		if rs.twitchMark.set {
			if rs.twitchMark.identity != twIdentity {
				rs.twitchMark = twitchAuthMark{}
			} else {
				twMarked = true
			}
		}
		// twEffective is the Twitch auth answer everything below this line
		// uses — the status, the previous-auth baseline, the recovery gate, the
		// recovered transition and (Task 3) the identity baseline. ONE value
		// rather than a mark check at each site: five sites each deciding for
		// themselves is five chances for one to disagree, and the site that
		// would disagree first is OnAuthRecovered, which would announce a
		// recovery that never happened and resume every parked Twitch job into
		// the same failure.
		twEffective = twAuth && !twMarked
		twVerification := verdictFromCheck(twAuth, twErr)
		twStatusErr := twErrStr
		if twMarked {
			twVerification = RefreshFailed
			twStatusErr = twitchAuthLossMessage(rs.twitchMark.reason)
		}
```

In the `rs.status = AuthStatus{...}` literal, replace the three Twitch fields:

```go
			TwitchAuthenticated: twEffective,
			TwitchVerification:  twVerification,
			TwitchError:         twStatusErr,
```

and replace the Twitch baseline advance:

```go
		if twErr == nil {
			rs.prevTwitchAuth = twEffective
			rs.twEverConcluded = true
		}
```

- [ ] **Step 7: Route the two decisions below the block through `twEffective`**

Still in `refresh`, replace the Twitch arm of the recovery gate (`:1287`):

```go
		if shouldFireRecovery(twConcluded, prevTW, twEffective, twErr, hasTWCookies) {
```

and the Twitch arm of the recovered transition (`:1301`):

```go
		if !prevTW && twEffective && twErr == nil {
```

`prevTW` is the pre-check snapshot and needs no change; `twAuth`, `twErr` and `twErrStr` keep meaning exactly what `checkTwitchAuth` returned, which is what the unmarked path must report.

- [ ] **Step 8: Run the tests and confirm they pass**

Run: `go test ./internal/cookies/ -run 'TestValidate200|TestATwitchCredential|TestAChangeToDead|TestTwitchMark|TestTwitchAuthLoss' -v`
Expected: PASS for all eight tests. (`TestTwitchMark` alone matches four of them: `...FiresRecoveryOncePerLoss`, `...NeverFiresRecoveryForAnUnconfiguredPlatform`, `...LeavesYouTubeAlone`, `...FiresAuthChangeOnAVerdictTransitionOnly`.)

Then the whole package, because this edit is inside the block every cookie test drives:
Run: `go test ./internal/cookies/ -count=1`
Expected: `ok  github.com/vampiricwulf/Moombox/internal/cookies`.

- [ ] **Step 9: Run the gates**

```bash
go build ./...
go vet ./...
GOOS=linux go build ./...
gofmt -l internal/ cmd/
go test -count=1 ./...
```
Expected: build and vet clean, `gofmt -l` prints nothing, 27 packages ok / 0 fail.

- [ ] **Step 10: Commit**

```bash
git add internal/cookies/refresh.go internal/cookies/refresh_twitch_mark_test.go
git commit -m "$(cat <<'EOF'
feat(cookies): a Twitch downgrade marks the platform, and validate cannot un-mark it

Arc 10 R1-R3. oauth2/validate answers 200 for a valid auth-token whether or
not a usable login cookie sits beside it, so two of the four ways a Twitch
capture goes anonymous were invisible to the only writer of
AuthStatus.TwitchAuthenticated. Nothing could inject the fact: ObserveLiveness
is disarmed AND never touches rs.status.

NoteTwitchAuthLoss is now a second writer, under the same mutex, setting a
mark keyed to the credential fingerprint it was taken under. refresh's status
block consults it, so a 200 cannot erase it, and clears it when the pair
changes — on the fingerprint alone, so a swap to a REVOKED token reports the
401 rather than the stale reason.

Recovery rides the existing shouldFireRecovery dedupe: one loss, one alarm.
The reason renders through a switch of string literals, which is what keeps
the set of strings TwitchError can hold fixed at compile time.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

---

### Task 3: `prevTwitchIdentity` and `OnCredentialsChanged("twitch")`

Spec R4's signalling half. `OnCredentialsChanged` exists (`internal/cookies/refresh.go:525-539`) and its doc says, verbatim, *"Fires for 'youtube' only — see prevYouTubeIdentity for why Twitch has no usable identity signal and nothing that would need one."* Both halves of that sentence are now false: Task 1 built the identity signal, and Task 4/5 build the thing that needs one. This task makes it fire, mirroring `prevYouTubeIdentity` / `shouldObserveCredentials` / `advanceIdentityBaseline` (`:1432-1499`) exactly — both helper functions are already pure and platform-agnostic, so this adds call sites, not logic.

**The existing subscriber is ADAPTED, not filtered.** `cmd/moombox/monitor_callbacks.go:678` wires `OnCredentialsChanged` to `resumeCookieParkedJobs(s.db, s.log, platform, identity)`, which delegates to `sweepShouldResume` (`monitor_callbacks.go:66-76`). That function already gates on `job.Platform != platform`, and `ParkReasonMembership` is written only by `parkReasonForError` for `ErrNotAMember` (`internal/worker/worker.go:895-902`), which is YouTube-only — so every Twitch `COOKIES?` job carries `ParkReasonAuth` and is resumable on a Twitch fire. That is the behaviour we want and it needs no code change. What DOES change is the notification wording, which says "after re-checking the signed-in account" — true of YouTube, wrong for a Twitch bearer token. Task 7 adapts that sentence and Task 9 adapts `operations.md`'s row for it.

**Reload sites, enumerated in source — and FOUR OF THEM DO NOT END IN `CheckNow` TODAY.** The comparison lives at ONE junction (`CheckNow` → `refresh(ctx, false)` → `rs.jar.Reload()` at `refresh.go:1104` → the status block), so every gesture that can put new Twitch credentials on disk has to reach it. Three do; four do not, and one of those four — the worker's own auth-failure browser refresh — is a common repair path. **Task 7a closes all four.** Until it does, R4 and R5 are bounded by the 30-minute ticker, which is exactly what "immediately apply the updated cookie" rules out.

| Reload site | Call site (verified) | Reaches `CheckNow`? | How it is pinned |
|---|---|---|---|
| `cookies.txt` edit + Recheck | `internal/web/routes/cookies.go:262` — `refreshSvc.CheckNow(req.Context())` | yes, today | `TestCheckNowObservesATwitchCredentialChange` below drives the same public entry point. The route's delegation itself has no test and cannot get a cheap one: `twitchValidateURL` is an unexported package var, so a routes-package test would hit the real network. Residual, named in the self-review. |
| TUI `R C` | `cmd/moombox/tui_wiring.go:342` — is `CheckNow` | yes, today | Same junction; the wiring is one line with no branch. |
| TUI `R F` | `cmd/moombox/tui_wiring.go:425` → `:435` — `s.cookieRefresh.CheckNow(context.Background())` after `RefreshCookiesDetailed` | yes, today | Task 7a migrates this line to `recheckAfterCookieWrite`, whose three tests are the pin. Output is byte-identical. |
| Web shift+click / Settings button | `internal/web/routes/cookies.go:314` `RefreshCookiesDetailed` → `:399` `refreshSvc.CheckNow(req.Context())` | yes, today | Same junction. The routes package has no operational logger, so this site is a bare call by design (`:390-398` says so). Residual. |
| Automatic recovery, `RefreshOK` arm | `cmd/moombox/monitor_callbacks.go:358` | yes, today | Task 7a hoists it above the `switch` so it covers all three arms; `recheckAfterCookieWrite`'s tests are the pin. |
| Automatic recovery, `RefreshFailed` / `RefreshUnknown` arms | `cmd/moombox/monitor_callbacks.go:363-420` | **no** | Task 7a — the hoist above covers these. A browser pass that wrote a NEW-but-dead pair still moved the fingerprint, so the stale mark's sentence would otherwise stand until the tick. |
| Worker auth-failure refresh (`OnCookieRefreshNeeded`) | `cmd/moombox/services.go:902` `RefreshCookiesDetailed`, returns `report.ok` | **no** | Task 7a. This is the site that contradicts "immediately": a job failing on `ErrTwitchAuthExpired` / `ErrSubscriberOnly` triggers a browser refresh that succeeds, and every OTHER live Twitch job's chat waits up to 30 minutes. |
| Setup-wizard finish (Web) | `internal/web/routes/cookies.go:460` `FinishSetupDetailed` → `autocookies.go:1208` `jar.Load` | **no** | Task 7a adds the bare `refreshSvc.CheckNow(req.Context())` that `:399` already uses in the same file. |
| Setup-wizard finish (TUI twin) | `cmd/moombox/tui_wiring.go:486` `FinishSetupDetailed` | **no** | Task 7a — `recheckAfterCookieWrite`. |
| Auto-cookie periodic timer (`auto_enabled` on) | `internal/cookies/autocookies.go:2898` `refreshCookies` → `jar.Load` at `:2073` / `:2133` / `:2191` | **no**, and it has no caller outside the package | Task 7a adds ONE injected seam, `AutoCookieService.OnPassCompleted`, fired from `notePassCompleted()` at the tick's tail — the only site here with no caller-side opportunity. `TestNotePassCompleted*` in `internal/cookies` is the pin. |
| YouTube request paths (`youtube.Auth.SyncCookies` → `jar.Reload()`) | seven callers | n/a | Reloads the jar so the next IRC session reads fresh rows, but compares nothing and claims nothing. Deliberately untouched: these run per-request and a `CheckNow` on each would be two validate round-trips per YouTube API call. |
| `twitch.Service.ReloadAuth` | `internal/twitch/service.go:112` | n/a | No production callers. |
| Startup | `cmd/moombox/services.go:341` `jar.Load` → `RefreshService.Start`'s initial pass | yes, today | `Start` runs `refresh(ctx, false)` synchronously before the web server binds. |
| Arc 11's import endpoint | not built | — | Arc 11 must end in `recheckAfterCookieWrite`; recorded in the Arc 11 handoff. |

**Every line number below was verified at pristine `main`, and Task 2 inserts roughly 70-90 lines into this same file above most of them.** Re-derive by SYMBOL — `prevYouTubeIdentity`, `OnCredentialsChanged`'s doc comment, the status func literal's `ytIdentity = rs.jar.YouTubeIdentity()` pair, the YouTube `OnCredentialsChanged` fire, `shouldObserveCredentials`, `advanceIdentityBaseline` — not by number. Same hazard, same rule, as Task 7a's banner.

**Files:**
- Modify: `internal/cookies/refresh.go` — `prevTwitchIdentity` field beside `prevYouTubeIdentity` (`:459`); `OnCredentialsChanged`'s doc (`:525-539`); the status block's Twitch identity sample and baseline advance; the fire below the YouTube one (`:1312-1315`)
- Test: `internal/cookies/refresh_twitch_identity_test.go` (create)

**Interfaces:**
- Consumes: `(*CookieJar).TwitchIdentity() string` (Task 1); `twEffective` and `twIdentity` from Task 2's status block; `shouldObserveCredentials(baseline, nowIdentity string, nowAuth bool, checkErr error) bool` (`:1466`); `advanceIdentityBaseline(baseline, nowIdentity string, nowAuth bool, checkErr error) string` (`:1494`)
- Produces: `RefreshService.OnCredentialsChanged` firing with `platform == "twitch"` and the jar's Twitch fingerprint — Task 7 subscribes to it

- [ ] **Step 1: Write the failing tests**

Create `internal/cookies/refresh_twitch_identity_test.go`:

```go
package cookies

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// Arc 10 R4's signalling half. Every credential here is synthetic.
//
// These reuse writeTwitchPair and twitchMarkFixture from
// refresh_twitch_mark_test.go — same package, same fixtures, one definition.

// credentialFires records every OnCredentialsChanged call as "platform" and,
// separately, whether the identity handed over was non-empty. The IDENTITY
// ITSELF is never compared against a literal here and never printed: it is an
// opaque equality token, and a test that hardcoded a digest would also be
// pinning the hash input, which is jar_twitch_identity_test.go's job.
type credentialFires struct {
	platforms  []string
	identities []string
}

func (c *credentialFires) record(platform, identity string) {
	c.platforms = append(c.platforms, platform)
	c.identities = append(c.identities, identity)
}

// TestTwitchCredentialChangeFiresOnCredentialsChanged is the claim.
//
// The mutation: leaving the fire YouTube-only (the shipped state). Under it
// nothing downstream ever learns a Twitch credential changed, so Task 5's
// registry is never broadcast to and a live capture stays anonymous for the
// rest of the job — the exact defect this arc exists to close.
func TestTwitchCredentialChangeFiresOnCredentialsChanged(t *testing.T) {
	rs, path := twitchMarkFixture(t, "test-token-aaaa", "archiveraccount", http.StatusOK)
	var fires credentialFires
	rs.OnCredentialsChanged = fires.record

	rs.doRefresh(context.Background()) // baseline == "" fires once, on purpose
	if len(fires.platforms) != 1 || fires.platforms[0] != "twitch" {
		t.Fatalf("first pass fired %v, want exactly [twitch] — the baseline == \"\" case fires once per process so an offline cookie swap is noticed at all", fires.platforms)
	}
	if fires.identities[0] == "" {
		t.Error("the fire carried an empty identity — the subscriber cannot tell accounts apart")
	}

	writeTwitchPair(t, path, "test-token-bbbb", "archiveraccount")
	rs.doRefresh(context.Background())
	if len(fires.platforms) != 2 {
		t.Fatalf("fires = %v, want a second one after the auth-token changed", fires.platforms)
	}
	if fires.identities[1] == fires.identities[0] {
		t.Error("the second fire carried the SAME identity as the first — the fingerprint is not being re-read from the reloaded jar")
	}
}

// TestTwitchCredentialsUnchangedFireOnce: the edge filter.
//
// The mutation: dropping advanceIdentityBaseline for Twitch, so the baseline
// stays "" and every 30-minute pass fires. Downstream that drops and
// re-establishes every live IRC session twice an hour, forever, on installs
// where nothing changed.
func TestTwitchCredentialsUnchangedFireOnce(t *testing.T) {
	rs, _ := twitchMarkFixture(t, "test-token-aaaa", "archiveraccount", http.StatusOK)
	var fires credentialFires
	rs.OnCredentialsChanged = fires.record

	rs.doRefresh(context.Background())
	rs.doRefresh(context.Background())
	rs.doRefresh(context.Background())

	if len(fires.platforms) != 1 {
		t.Errorf("fires = %v across three unchanged passes, want exactly one", fires.platforms)
	}
}

// TestADeadTwitchTokenDoesNotConsumeTheChangeEdge is the property
// advanceIdentityBaseline exists for, restated for Twitch.
//
// The sequence is routine: an operator drops in an export that is already
// stale, sees it fail, and re-exports properly. If the failed export moved the
// baseline, the working one compares equal to it and NOTHING fires — the live
// chat session never learns the credentials it has been waiting for arrived.
//
// The mutation: `rs.prevTwitchIdentity = twIdentity` unconditionally.
func TestADeadTwitchTokenDoesNotConsumeTheChangeEdge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.txt")
	writeTwitchPair(t, path, "test-token-aaaa", "archiveraccount")
	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	// A validate server whose answer the test controls between passes. The
	// status is atomic because the handler runs on net/http's connection
	// goroutine while the test writes it from its own — a plain int here is a
	// data race that only -race reports, and the twitch and worker packages in
	// this arc are gated under -race.
	var code atomic.Int32
	code.Store(http.StatusUnauthorized)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(code.Load()))
	}))
	t.Cleanup(srv.Close)
	pointTwitchValidateAt(t, srv)
	ytSrv, _ := countingGuide(t, loggedOutGuideBody)
	pointYouTubeGuideAt(t, ytSrv)

	rs := NewRefreshService(jar, 0, nopLogger{})
	var fires credentialFires
	rs.OnCredentialsChanged = fires.record

	// Pass 1: the stale export. Conclusive, NOT authenticated.
	rs.doRefresh(context.Background())
	if len(fires.platforms) != 0 {
		t.Fatalf("fires = %v for credentials that do not authenticate, want none", fires.platforms)
	}

	// Pass 2: the same file re-exported properly. Same fingerprint is not the
	// point — what matters is that the edge survived the failed pass.
	code.Store(http.StatusOK)
	rs.doRefresh(context.Background())
	if len(fires.platforms) != 1 || fires.platforms[0] != "twitch" {
		t.Errorf("fires = %v, want [twitch] — the failed pass consumed the edge", fires.platforms)
	}
}

// TestNoTwitchCredentialsNeverFire: an install with no Twitch cookies has a ""
// fingerprint, and "" compares unequal to every real one.
//
// The mutation: dropping shouldObserveCredentials' `nowIdentity == ""` guard,
// which would fire on every cookieless cycle and broadcast a credential change
// to every live chat session on a YouTube-only install.
func TestNoTwitchCredentialsNeverFire(t *testing.T) {
	pointTwitchValidateAt(t, statusServer(t, http.StatusOK))
	ytSrv, _ := countingGuide(t, loggedOutGuideBody)
	pointYouTubeGuideAt(t, ytSrv)

	rs := NewRefreshService(NewCookieJar(), 0, nopLogger{})
	var fires credentialFires
	rs.OnCredentialsChanged = fires.record

	rs.doRefresh(context.Background())
	rs.doRefresh(context.Background())

	if len(fires.platforms) != 0 {
		t.Errorf("fires = %v on a jar with no Twitch credentials, want none", fires.platforms)
	}
}

// TestCheckNowObservesATwitchCredentialChange pins the comparison at the
// PUBLIC entry point every reload site has to reach.
//
// Those sites are enumerated in this task's table: POST /api/cookies/recheck
// and the dashboard/Settings browser refresh (both internal/web/routes/
// cookies.go), the TUI's R C and R F (cmd/moombox/tui_wiring.go), the
// automatic recovery re-check (cmd/moombox/monitor_callbacks.go), and — once
// Task 7a lands — the worker's auth-failure refresh, both setup-wizard finish
// paths and the auto-cookie periodic timer. All of them end in CheckNow, which
// is refresh with allowFallback=false, so driving CheckNow drives all of them;
// what the two routes-package sites cannot have is a test that they CALL it
// (see the residual in the table).
//
// The mutation: sampling the Twitch fingerprint BEFORE jar.Reload() rather
// than inside the status block, which would make every one of those four
// gestures report the pre-edit file.
func TestCheckNowObservesATwitchCredentialChange(t *testing.T) {
	rs, path := twitchMarkFixture(t, "test-token-aaaa", "archiveraccount", http.StatusOK)
	var fires credentialFires
	rs.OnCredentialsChanged = fires.record

	if !rs.CheckNow(context.Background()) {
		t.Fatal("the first CheckNow reported that no pass ran")
	}
	writeTwitchPair(t, path, "test-token-bbbb", "otheraccount")
	if !rs.CheckNow(context.Background()) {
		t.Fatal("the second CheckNow reported that no pass ran")
	}

	if len(fires.platforms) != 2 {
		t.Errorf("fires = %v, want two — CheckNow must reload the jar and compare the fingerprint within the same pass", fires.platforms)
	}
}
```

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/cookies/ -run 'TestTwitchCredential|TestADeadTwitchToken|TestNoTwitchCredentials|TestCheckNowObserves' -v`
Expected: FAIL — `first pass fired [], want exactly [twitch]` and the same shape from the others. They compile: nothing new is referenced yet.

- [ ] **Step 3: Add the `prevTwitchIdentity` field**

In `internal/cookies/refresh.go`, immediately after the `prevYouTubeIdentity` field (`:459`):

```go
	// prevTwitchIdentity is the jar's TwitchIdentity() as of the last
	// conclusive AND authenticated Twitch check — the baseline
	// shouldObserveCredentials compares against, exactly as
	// prevYouTubeIdentity is for YouTube. See advanceIdentityBaseline for why
	// an unauthenticated check must not move it.
	//
	// The reason the YouTube field's comment above gave for NOT having this —
	// "Twitch's auth-token rotates on Twitch's schedule, so it is not a stable
	// account discriminator, and no Twitch failure produces a membership park"
	// — was correct about ACCOUNTS and is not what this answers. Arc 10 asks
	// "is this the same credential PAIR the chat downgrade was observed
	// under", and a rotation that changes the token is a genuine YES to that:
	// the new pair has not been proven broken, so clearing the mark and
	// reconnecting chat once is the right outcome, not a false positive.
	prevTwitchIdentity string
```

- [ ] **Step 4: Correct `OnCredentialsChanged`'s doc**

In `internal/cookies/refresh.go`, replace the final paragraph of `OnCredentialsChanged`'s doc comment (`:537-538`) — currently *"Fires for 'youtube' only — see prevYouTubeIdentity for why Twitch has no usable identity signal and nothing that would need one."* — with:

```go
	// Fires for BOTH platforms, and the two mean different things to their
	// subscribers. A YouTube fire is "the signed-in ACCOUNT may have changed",
	// which is what unsticks a membership park. A Twitch fire is "the
	// credential PAIR changed", which clears the Twitch auth mark and is the
	// only signal a live IRC chat session has that repaired cookies are on
	// disk — see CookieJar.TwitchIdentity and NoteTwitchAuthLoss.
	//
	// Both are governed by the same two pure functions,
	// shouldObserveCredentials and advanceIdentityBaseline, against
	// per-platform baselines.
```

- [ ] **Step 5: Sample and advance the Twitch baseline**

In `refresh`'s status block, add `prevTWIdentity string` to the `var (...)` declaration beside `prevYTIdentity`, then inside the func literal, next to `prevYTIdentity = rs.prevYouTubeIdentity` (and after Task 2's `twIdentity = rs.jar.TwitchIdentity()` line):

```go
		prevTWIdentity = rs.prevTwitchIdentity
```

and below, beside the YouTube baseline advance:

```go
		// Same rule as YouTube's, and deliberately outside the `twErr == nil`
		// block above for the same reason: the baseline advances only on a
		// check that also AUTHENTICATED, so a stale intermediate export cannot
		// consume the edge the properly re-exported one needs. twEffective, not
		// twAuth — a marked platform has not authenticated, whatever validate
		// says, and moving the baseline under a standing mark would strand the
		// repair that clears it.
		rs.prevTwitchIdentity = advanceIdentityBaseline(rs.prevTwitchIdentity, twIdentity, twEffective, twErr)
```

- [ ] **Step 6: Fire for Twitch**

In `refresh`, immediately after the existing YouTube `OnCredentialsChanged` block (`:1312-1315`):

```go
	// The Twitch counterpart, and the one with a second subscriber: besides
	// the parked-job sweep, cmd/moombox broadcasts this to every live Twitch
	// chat downloader so a repaired cookie file reaches a capture that is
	// already running. See DownloadWorker.ReauthenticateTwitchChats.
	//
	// The identity is an opaque equality token and is handed to the callback,
	// never to the log line.
	if rs.OnCredentialsChanged != nil && shouldObserveCredentials(prevTWIdentity, twIdentity, twEffective, twErr) {
		rs.logger.Info("twitch credential pair observed — re-evaluating parked jobs and live chat sessions")
		rs.OnCredentialsChanged("twitch", twIdentity)
	}
```

- [ ] **Step 7: Run the tests and confirm they pass**

Run: `go test ./internal/cookies/ -run 'TestTwitchCredential|TestADeadTwitchToken|TestNoTwitchCredentials|TestCheckNowObserves' -v`
Expected: PASS for all five tests.

Run: `go test ./internal/cookies/ -count=1`
Expected: `ok  github.com/vampiricwulf/Moombox/internal/cookies`.

- [ ] **Step 8: Run the gates**

```bash
go build ./...
go vet ./...
GOOS=linux go build ./...
gofmt -l internal/ cmd/
go test -count=1 ./...
```
Expected: build and vet clean, `gofmt -l` prints nothing, 27 packages ok / 0 fail.

`cmd/moombox`'s existing `OnCredentialsChanged` subscriber now also receives `"twitch"`. That is intended and needs no change here: `sweepShouldResume` gates on `job.Platform`, and every Twitch `COOKIES?` job carries `ParkReasonAuth` (only `ErrNotAMember` produces `ParkReasonMembership`, and that is YouTube-only). If `go test ./cmd/moombox/` fails on a sweep test, STOP and re-read that premise before changing anything.

- [ ] **Step 9: Commit**

```bash
git add internal/cookies/refresh.go internal/cookies/refresh_twitch_identity_test.go
git commit -m "$(cat <<'EOF'
feat(cookies): OnCredentialsChanged fires for twitch, on the same two pure rules

Arc 10 R4. The callback's doc said it fires for youtube only because "Twitch
has no usable identity signal and nothing that would need one". Task 1 built
the signal and the live-chat reconnect needs it, so both halves are now false.

prevTwitchIdentity mirrors prevYouTubeIdentity and reuses
shouldObserveCredentials and advanceIdentityBaseline unchanged — the baseline
advances only on a check that also authenticated, so a stale intermediate
export cannot consume the edge the good one needs.

The comparison is driven through CheckNow, which is the entry point all four
reload sites (recheck route, R F, the Settings twin, the recovery re-check)
already call.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

---

### Task 4: `ChatDownloader.Reauthenticate()`

Spec R5's downloader half. Credentials ARE re-read per session (`runIRCSession`, `chat_irc.go:171`), but three things stop a repaired cookie file from reaching a running capture: nothing ends a healthy session; `authRefused` latches for the life of the downloader so even a natural reconnect stays anonymous (`sessionCredentials`, `chat.go:258-263`); and `Start`'s reconnect loop charges every reconnect against a ten-attempt budget with exponential backoff (`chat.go:614-667`).

**Three latches are reset, not the two the design named — and that is a gap this plan closes.** Spec R5 says "reset `authRefused` and `downgradeReported`". `warnedNoLogin` must go too, because `noteMissingLogin` (`chat.go:344-366`) returns EARLY on `cd.warnedNoLogin.Swap(true)`, **before** it reaches `reportAuthDowngrade`. Leaving it set means a job whose repaired cookie file is still missing its `login` row reports nothing the second time — which is exactly the silence R5's last sentence ("a second refusal re-marks with the new reason and notifies the job again") forbids.

**One mechanism, not two.** `reauthPending atomic.Bool` is set only when there is a live session to interrupt, and it is consumed by `Start`'s loop with a single `Swap(false)`. Two other places READ it — `runIRCSession`'s read-error branch (to end the session at once rather than burn 20 failed reads against a context we cancelled) and its handshake-outcome defer (so our own cancel is not read as Twitch refusing the login) — but neither decides anything the loop does not.

**Five `chat.go` contract comments become false in this task, and this task fixes them.** Each says "per downloader" or "per job" about a latch that is now per CREDENTIAL PAIR: `warnedNoLogin`'s field doc (`:91-95`), `onAuthDowngrade`'s field doc (`:96-98`), `ChatDownloaderOptions.OnAuthDowngrade` (`:181-183`), `reportAuthDowngrade` (`:271-278`) and `noteHandshakeOutcome`'s "ONE-SHOT per job ... a cookie repaired mid-job does not re-authenticate chat until the next job" (`:407-411`). Leaving them is worse than leaving a stale spec doc: they are the contract a reader checks before touching the latch. Task 6 owns the sixth, on the worker side.

**Files:**
- Modify: `internal/twitch/chat.go` — `errors` import; `reauthPending` field beside `downgradeReported` (`:100-104`); `Reauthenticate` after `interruptSession` (`:726-734`); the reconnect loop in `Start` (`:614-667`); five doc-comment corrections (`:91-95`, `:96-98`, `:181-183`, `:271-278`, `:407-411`)
- Modify: `internal/twitch/chat_irc.go` — the handshake-outcome defer (`:194-204`), the read-error branch (`:270-279`), and the `001` Info line (`:302-307`)
- Test: `internal/twitch/chat_reauth_test.go` (create)

**Interfaces:**
- Consumes: `cd.authRefused`, `cd.downgradeReported`, `cd.warnedNoLogin` (all `atomic.Bool`), `cd.sessionCancel context.CancelFunc` guarded by `cd.mu`, `cd.logger`
- Produces: `func (cd *ChatDownloader) Reauthenticate()` — Task 5's registry calls it through the `twitchChatReauthenticator` interface

- [ ] **Step 1: Write the failing tests**

Create `internal/twitch/chat_reauth_test.go`:

```go
package twitch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/vampiricwulf/Moombox/internal/constants"
)

// Arc 10 R5, downloader half. Every credential here is synthetic and none is
// ever logged.
//
// What the existing fakes cannot do. ircReplier (chat_irc_fallback_test.go)
// closes each connection as soon as its script is written, so the client's
// session ends on its own and a test cannot act on a LIVE session — which is
// the only state Reauthenticate has to work in. The fixture below holds the
// connection open instead.

// pingLine is the one server line a Twitch IRC client answers, which is what
// makes it usable as a round-trip proof.
const pingLine = "PING :tmi.twitch.tv"

// holdingIRCServer answers a handshake, writes a scripted burst, waits for the
// client's PONG, publishes the handshake, and then HOLDS the connection open
// until the client drops it.
//
// Publishing only AFTER the PONG is what makes the wait deterministic:
// receiving from sessions means this session read and handled every scripted
// line. Every script must therefore end in a line the client answers — PING.
//
// The hold matters as much as the script. A server that closed after its
// script would produce a reconnect of its OWN, and a test could not tell it
// from the reconnect Reauthenticate causes.
type holdingIRCServer struct {
	server   *httptest.Server
	sessions chan []string
}

func startHoldingIRCServer(t *testing.T, script ...string) *holdingIRCServer {
	t.Helper()
	h := &holdingIRCServer{sessions: make(chan []string, 32)}
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")

		var lines []string
		for len(lines) < 4 { // PASS, NICK, CAP REQ, JOIN
			_, data, readErr := conn.Read(r.Context())
			if readErr != nil {
				return
			}
			lines = append(lines, string(data))
		}
		for _, line := range script {
			if writeErr := conn.Write(r.Context(), websocket.MessageText, []byte(line)); writeErr != nil {
				return
			}
		}
		if _, _, readErr := conn.Read(r.Context()); readErr != nil { // the PONG
			return
		}
		h.sessions <- lines

		for {
			if _, _, readErr := conn.Read(r.Context()); readErr != nil {
				return
			}
		}
	}))
	t.Cleanup(h.server.Close)

	prev := constants.TwitchURLs.IRCWS
	constants.TwitchURLs.IRCWS = "ws" + strings.TrimPrefix(h.server.URL, "http")
	t.Cleanup(func() { constants.TwitchURLs.IRCWS = prev })
	return h
}

func (h *holdingIRCServer) nextSession(t *testing.T) []string {
	t.Helper()
	select {
	case lines := <-h.sessions:
		return lines
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for an IRC session to reach its scripted state")
		return nil
	}
}

// TestReauthenticateClearsTheRefusalLatchAndRepresentsCredentials is the
// central claim: a job that went anonymous after a refusal comes back
// credentialed when told to.
//
// The assertion is on the SECOND session's WIRE BYTES. "A second handshake
// happened" is a junction the unfixed code satisfies too — it reconnects
// anonymously.
//
// Two mutations: not resetting authRefused (session 2 is the justinfan pair,
// and the whole feature is dead), and not resetting downgradeReported (the
// second refusal is silent, so the mark and the job notice never fire again).
func TestReauthenticateClearsTheRefusalLatchAndRepresentsCredentials(t *testing.T) {
	rep := startIRCReplier(t, []string{loginFailedNotice}, []string{loginFailedNotice})
	var reports downgradeRecorder
	cd := newDowngradeTestChatDownloader(t,
		staticCredentials("test-token-aaaa", "archiveraccount"), &recordingLogger{}, reports.record)

	runLiveIRCSession(t, cd)
	firstPass, firstNick := handshakeLines(t, rep.nextSession(t))
	if firstPass != "PASS oauth:test-token-aaaa" || firstNick != "NICK archiveraccount" {
		t.Fatalf("first handshake = (%q, %q), want the authenticated pair", firstPass, firstNick)
	}
	if !cd.authRefused.Load() {
		t.Fatal("the fixture is wrong: the refusal did not latch the anonymous fallback")
	}
	reports.assertReportedExactly(t, AuthDowngradeLoginRefused)

	cd.Reauthenticate()

	runLiveIRCSession(t, cd)
	secondPass, secondNick := handshakeLines(t, rep.nextSession(t))
	if secondPass != "PASS oauth:test-token-aaaa" || secondNick != "NICK archiveraccount" {
		t.Errorf("second handshake = (%q, %q), want the authenticated pair — the refusal latch survived Reauthenticate", secondPass, secondNick)
	}
	reports.assertReportedExactly(t, AuthDowngradeLoginRefused, AuthDowngradeLoginRefused)
}

// TestReauthenticateReReportsAMissingLoginCookie is the latch the design's
// two-name list missed.
//
// noteMissingLogin returns early on warnedNoLogin.Swap(true) BEFORE it reaches
// reportAuthDowngrade, so leaving that flag set means a repaired cookie file
// that is STILL missing its login row reports nothing the second time — the
// exact silence this arc exists to end.
//
// The mutation: resetting only authRefused and downgradeReported.
func TestReauthenticateReReportsAMissingLoginCookie(t *testing.T) {
	rep := startIRCReplier(t, nil, nil)
	var reports downgradeRecorder
	cd := newDowngradeTestChatDownloader(t,
		staticCredentials("test-token-aaaa", ""), &recordingLogger{}, reports.record)

	runLiveIRCSession(t, cd)
	assertAnonymousHandshake(t, rep.nextSession(t))
	reports.assertReportedExactly(t, AuthDowngradeNoLoginCookie)

	cd.Reauthenticate()

	runLiveIRCSession(t, cd)
	assertAnonymousHandshake(t, rep.nextSession(t))
	reports.assertReportedExactly(t, AuthDowngradeNoLoginCookie, AuthDowngradeNoLoginCookie)
}

// TestReauthenticateOnAnIdleDownloaderDoesNotArmThePendingFlag.
//
// reauthPending is set only when there IS a session to interrupt. Setting it
// unconditionally would leave the flag standing until some LATER session
// ended, and that session's handshake-outcome defer would then read a GENUINE
// refusal as our own cancel: the fallback would never latch and the job would
// spend its whole reconnect budget on a login Twitch will not take.
//
// The mutation: `cd.reauthPending.Store(true)` outside the sessionCancel != nil
// check.
func TestReauthenticateOnAnIdleDownloaderDoesNotArmThePendingFlag(t *testing.T) {
	rep := startIRCReplier(t, []string{loginFailedNotice})
	cd := newAuthTestChatDownloaderWithLogger(t,
		staticCredentials("test-token-aaaa", "archiveraccount"), &recordingLogger{})

	cd.Reauthenticate() // no live session
	if cd.reauthPending.Load() {
		t.Fatal("Reauthenticate armed the pending flag with no session to interrupt")
	}

	runLiveIRCSession(t, cd)
	_ = rep.nextSession(t)
	if !cd.authRefused.Load() {
		t.Error("a genuine refusal did not latch the anonymous fallback — a stale pending flag suppressed it")
	}
}

// TestReauthenticateDoesNotLatchTheFallbackOnItsOwnCancel.
//
// The window is real: Reauthenticate can land between the CAP ACK and the 001,
// and runIRCSession's deferred noteHandshakeOutcome then sees welcomed=false
// with heardFromServer=true — indistinguishable from "Twitch spoke and never
// acknowledged the login" unless the defer knows WE cancelled.
//
// The script is a PING and no welcome, so the client has heard from the server
// and has not been welcomed at the moment the test acts. The PONG the fixture
// waits for is the proof of exactly that state.
//
// The mutation: dropping `cd.reauthPending.Load()` from the defer's guard.
// Under it the reconnect Reauthenticate asked for comes back ANONYMOUS and a
// spurious login-never-acknowledged is reported — the feature inverts itself.
func TestReauthenticateDoesNotLatchTheFallbackOnItsOwnCancel(t *testing.T) {
	h := startHoldingIRCServer(t, pingLine)
	var reports downgradeRecorder
	cd := newDowngradeTestChatDownloader(t,
		staticCredentials("test-token-aaaa", "archiveraccount"), &recordingLogger{}, reports.record)

	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- context.Canceled
			}
		}()
		done <- cd.Start(context.Background())
	}()
	t.Cleanup(func() {
		cd.Stop()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("Start did not return after Stop")
		}
	})

	first := h.nextSession(t)
	if pass, nick := handshakeLines(t, first); pass != "PASS oauth:test-token-aaaa" || nick != "NICK archiveraccount" {
		t.Fatalf("first handshake = (%q, %q), want the authenticated pair", pass, nick)
	}

	cd.Reauthenticate()

	second := h.nextSession(t)
	if pass, nick := handshakeLines(t, second); pass != "PASS oauth:test-token-aaaa" || nick != "NICK archiveraccount" {
		t.Errorf("second handshake = (%q, %q), want the authenticated pair — our own cancel was read as a refusal", pass, nick)
	}
	if cd.authRefused.Load() {
		t.Error("the anonymous fallback latched on a session WE cancelled")
	}
	reports.assertReportedExactly(t)
}

// TestReauthenticateSpendsNoReconnectBudget.
//
// maxReconnects is 10, so eleven credential-driven reconnects exhaust the
// budget if each costs one. The assertion is on session TWELVE arriving and on
// Start not having returned — not on elapsed time, which would be flaky.
//
// Two mutations, both caught. Charging the budget: Start returns "exceeded max
// IRC reconnects" and session twelve never arrives. Keeping the backoff: the
// delays run 2s, 4s, 8s, 16s, 30s, 30s..., so nextSession's 10-second wait
// trips by the fifth cycle.
//
// The script is a welcome followed by a PING, so every session is fully
// established (welcomed=true) before the test acts on it — which keeps this
// test independent of the handshake-defer guard the previous test owns.
func TestReauthenticateSpendsNoReconnectBudget(t *testing.T) {
	h := startHoldingIRCServer(t, welcomeLine, pingLine)
	logger := &acceptedLoginRecorder{}
	cd := newAuthTestChatDownloaderWithLogger(t,
		staticCredentials("test-token-aaaa", "archiveraccount"), logger)

	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- context.Canceled
			}
		}()
		done <- cd.Start(context.Background())
	}()
	t.Cleanup(func() {
		cd.Stop()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("Start did not return after Stop")
		}
	})

	// The initial session plus eleven forced reconnects: one more than the
	// budget, so a budget-charging implementation gives up before the last.
	const forcedReconnects = 11
	for i := 0; i <= forcedReconnects; i++ {
		lines := h.nextSession(t)
		pass, nick := handshakeLines(t, lines)
		if pass != "PASS oauth:test-token-aaaa" || nick != "NICK archiveraccount" {
			t.Fatalf("session %d handshake = (%q, %q), want the authenticated pair", i, pass, nick)
		}
		select {
		case err := <-done:
			t.Fatalf("Start returned after %d sessions (%v) — the reconnect budget was charged for reconnects we asked for", i+1, err)
		default:
		}
		if i < forcedReconnects {
			cd.Reauthenticate()
		}
	}

	// R5's "a credentialed 001 logs at Info". Every session in this test was
	// welcomed, so every session must have said so — that line is the only
	// positive confirmation an operator gets that a repaired credential was
	// accepted, and the field gate asks them to look for it.
	//
	// The mutation: not logging on 001 at all, which leaves the operator with
	// only the ABSENCE of a downgrade report, and absence is not evidence.
	if got := logger.acceptedLogins(); got != forcedReconnects+1 {
		t.Errorf("accepted-login lines = %d across %d welcomed sessions, want one each", got, forcedReconnects+1)
	}
}
```

`acceptedLoginRecorder` is local to this file rather than an extension of
`recordingLogger` (`chat_irc_fallback_test.go:142`), which records Warn only and is
counted exactly by every fallback test in that file — widening it would change
what those counts mean:

```go
// acceptedLoginRecorder counts the Info line an accepted authenticated login
// writes. It records the MESSAGE only, never the args, for the same reason
// recordingLogger does: neither the token nor the login may reach a log line,
// and a recorder that captured args would make a leak invisible here.
type acceptedLoginRecorder struct {
	mu    sync.Mutex
	infos []string
}

func (l *acceptedLoginRecorder) Debug(string, ...any) {}
func (l *acceptedLoginRecorder) Warn(string, ...any)  {}
func (l *acceptedLoginRecorder) Error(string, ...any) {}
func (l *acceptedLoginRecorder) Info(msg string, args ...any) {
	l.mu.Lock()
	l.infos = append(l.infos, msg)
	l.mu.Unlock()
}

// acceptedLogins counts only the accepted-login line, so the several other
// Info lines Start writes per reconnect cannot be mistaken for it.
func (l *acceptedLoginRecorder) acceptedLogins() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, m := range l.infos {
		if strings.Contains(m, "authenticated login accepted") {
			n++
		}
	}
	return n
}
```

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/twitch/ -run TestReauthenticate -v`
Expected: compile failure — `cd.Reauthenticate undefined` and `cd.reauthPending undefined`.

- [ ] **Step 3: Add the `errors` import, the field and `Reauthenticate`**

In `internal/twitch/chat.go`, add `"errors"` to the import block (between `"context"` and `"fmt"`).

Add the field immediately after `downgradeReported` (`:104`):

```go
	// reauthPending marks the window between Reauthenticate() cancelling a
	// session and Start's loop consuming that fact. Three sites read it, one
	// consumes it: runIRCSession's read-error branch ends the session at once
	// instead of burning chatMaxConsecutiveErrs failed reads against a context
	// we cancelled; its handshake-outcome defer refuses to read our own cancel
	// as Twitch refusing the login; and Start's loop Swaps it to false and
	// reconnects immediately, charging nothing to the reconnect budget.
	//
	// It is armed ONLY when there is a live session to interrupt. A flag left
	// standing on an idle downloader would suppress the handshake verdict of
	// some LATER session, turning a genuine refusal into an unbounded retry
	// loop on credentials Twitch will not take.
	reauthPending atomic.Bool
```

Add the sentinel beside the `AuthDowngrade*` block:

```go
// errReauthRequested ends an IRC session that Reauthenticate cancelled.
//
// It exists to END THE READ LOOP on the first failed read rather than spin
// through chatMaxConsecutiveErrs of them against a context we cancelled. It is
// never compared against and never reaches a log line: Start's loop decides on
// reauthPending and `continue`s before the Warn that would print it, and the
// Info line beside that `continue` is what says what happened. Its text is
// therefore for a reader of the code, not for an operator.
var errReauthRequested = errors.New("IRC session cancelled to present refreshed credentials")
```

Add the method immediately after `interruptSession` (`:734`):

```go
// Reauthenticate tells a running downloader to drop its IRC session and open a
// new one with whatever credentials the cookie jar holds NOW.
//
// The problem it solves. Credentials are re-read per session, but nothing ends
// a healthy session, and after a refusal authRefused latches for the life of
// the downloader — so an operator who repairs cookies.txt four hours into a
// twelve-hour capture keeps capturing anonymously until the job ends, losing
// every subscriber-only message and badge in between. This is the only thing
// that can undo that.
//
// IT RESETS THREE LATCHES, not two. authRefused is the behaviour switch that
// makes sessionCredentials return an empty pair. downgradeReported is the
// one-report-per-downloader latch. And warnedNoLogin is not merely a log
// latch: noteMissingLogin returns on its Swap BEFORE it reaches
// reportAuthDowngrade, so leaving it set means a repaired cookie file that is
// still missing its login row reports NOTHING the second time — precisely the
// silence this whole mechanism exists to end. All three are reset before the
// session is dropped, so the next handshake is judged on its own merits.
//
// The drop goes through the existing sessionCancel, so a session parked in a
// six-minute read reacts at once rather than minutes later.
//
// reauthPending is armed inside the same critical section that reads
// sessionCancel, and only when a session exists. Arming it for an idle
// downloader would leave it standing until some later session ended, and that
// session's handshake-outcome defer would then read a genuine refusal as our
// own cancel.
//
// Safe on a downloader that is not running: the latches are still cleared, so
// the next Start — the orchestrator relaunches chat after a connectivity gap —
// presents credentials. Safe from any goroutine, and it does not block.
func (cd *ChatDownloader) Reauthenticate() {
	cd.authRefused.Store(false)
	cd.downgradeReported.Store(false)
	cd.warnedNoLogin.Store(false)

	// interruptSession's body is inlined rather than called, because the
	// decision and the read must be ONE critical section: between a separate
	// read and a separate arm, a session could start or end.
	cd.mu.Lock()
	cancel := cd.sessionCancel
	if cancel != nil {
		cd.reauthPending.Store(true)
	}
	cd.mu.Unlock()

	// Names no credential, and there is nothing here to name one with.
	cd.logger.Info("twitch chat: re-authenticating with the current credentials",
		"channel", cd.channelLogin, "hadLiveSession", cancel != nil)
	if cancel != nil {
		cancel()
	}
}
```

- [ ] **Step 3a: Correct the five contract comments this task falsifies**

Each of these says "per downloader" or "per job" about a latch `Reauthenticate` now resets. They are the contract a reader checks before touching the latch, so a stale one is worse than a stale spec sentence. Open each and make the named edit; change nothing else in the comment.

`chat.go:91-95`, `warnedNoLogin`'s field doc — replace "latches noteMissingLogin's single Warn for the life of this downloader" with:

```go
	// warnedNoLogin latches noteMissingLogin's single Warn for the life of this
	// downloader's CURRENT CREDENTIAL PAIR — Reauthenticate resets it, because
	// it also gates this site's reportAuthDowngrade and a repaired file that is
	// still missing its login row has to be reported again.
```

`chat.go:96-98`, `onAuthDowngrade`'s field doc — replace "fired at most once per downloader" with "fired at most once per credential pair (Reauthenticate resets the latch)".

`chat.go:181-183`, `ChatDownloaderOptions.OnAuthDowngrade` — replace "called AT MOST ONCE per downloader" with:

```go
	// OnAuthDowngrade is called AT MOST ONCE per CREDENTIAL PAIR — once per
	// downloader until Arc 10, and still once per downloader for any job whose
	// cookies never change. Reauthenticate resets the latch, so a repaired
	// credential that fails again reports again, by design.
```

`chat.go:271-278`, `reportAuthDowngrade` — replace the "ONE report per downloader across every trigger site" opening with "ONE report per CREDENTIAL PAIR across every trigger site (Reauthenticate resets the latch — see there for why all three latches move together)", leaving the rest of that comment, including the three-flags derivation, unchanged.

`chat.go:407-411`, `noteHandshakeOutcome` — the whole "ONE-SHOT per job" paragraph is now wrong in its last sentence. Replace:

```go
// ONE-SHOT per job: once anonymous, the job stays anonymous. Flapping between
// the two would re-pay the rejected handshake on every reconnect, which is the
// cost this exists to bound. Cost if wrong: a cookie repaired mid-job does not
// re-authenticate chat until the next job — exactly the behaviour that shipped
// before the nickname existed, so a floor rather than a regression.
```

with:

```go
// ONE-SHOT per CREDENTIAL PAIR: once anonymous, the job stays anonymous for as
// long as the cookie file holds the pair Twitch refused. Flapping would re-pay
// the rejected handshake on every reconnect, which is the cost this exists to
// bound — so the latch is cleared by exactly one thing, Reauthenticate, which
// fires when the credential pair on disk actually changes. A cookie repaired
// mid-job therefore DOES re-authenticate chat, in place, without waiting for
// the next job; a second refusal on the new pair latches again here.
```

- [ ] **Step 4: Teach `Start`'s reconnect loop about the immediate reconnect**

In `internal/twitch/chat.go`, in `Start`, replace the loop preamble and the post-session block:

```go
	reconnectAttempts := 0
	// immediate suppresses the backoff for a reconnect WE asked for. Separate
	// from reconnectAttempts because the two answer different questions: how
	// many times the network has failed us, and whether to wait before the
	// next attempt. A credential the operator has just repaired must reach the
	// wire now, not after thirty seconds.
	immediate := false

	for reconnectAttempts <= maxReconnects {
		if ctx.Err() != nil || !cd.IsRunning() {
			return nil
		}

		if reconnectAttempts > 0 && !immediate {
			// Exponential backoff: 1000 * 2^attempts, capped at 30s (matches TypeScript)
			shift := min(reconnectAttempts, 15) // cap shift to prevent overflow
			delayMs := min(1000*(1<<shift), 30000)
			delay := time.Duration(delayMs) * time.Millisecond
			cd.logger.Info("reconnecting to twitch IRC",
				"channel", cd.channelLogin, "attempt", reconnectAttempts, "max", maxReconnects, "delay", delay)
			cd.flush() // Save state before reconnect
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(delay):
			}
		}
		immediate = false

		sessionStart := time.Now()
		err := cd.runIRCSession(ctx)
		sessionUptime := time.Since(sessionStart)

		// A Reauthenticate() cancelled this session on purpose. Swap rather
		// than Load, and on EVERY exit path rather than only the expected one:
		// the flag's job is done once the session it interrupted has unwound,
		// and a cancel that raced a real socket failure must not leave it
		// standing into a later session where it would suppress a genuine
		// refusal.
		//
		// Straight back in: no backoff, and nothing charged to the reconnect
		// budget. That budget bounds retries against a network that will not
		// stay up; this drop was ours, and eleven credential repairs during one
		// marathon stream must not be able to exhaust it and abandon chat for
		// the rest of the job.
		if cd.reauthPending.Swap(false) && ctx.Err() == nil && cd.IsRunning() {
			// Flush first, exactly as the backoff path above does. The owner
			// priced this reconnect as "one flush plus one reconnect per
			// session per credential change", and that is the cost the docs
			// quote; skipping it would leave the tail of this session's chat in
			// memory until the next session's flusher tick, which is a second,
			// undocumented behaviour for no saving.
			cd.flush()
			cd.logger.Info("twitch chat: reconnecting with the refreshed credentials", "channel", cd.channelLogin)
			immediate = true
			continue
		}

		if err == nil || ctx.Err() != nil || !cd.IsRunning() {
			return nil
		}
```

(The rest of the loop body — the uptime reset, `reconnectAttempts++` and the Warn — is unchanged.)

- [ ] **Step 5: Guard the handshake verdict, log an accepted login, and short-circuit the read loop**

In `internal/twitch/chat_irc.go`, in the `if authenticated { defer func() { ... } }` block (`:196-204`), replace the guard condition:

```go
	if authenticated {
		defer func() {
			// reauthPending: WE cancelled this session to present new
			// credentials, so its missing 001 is not Twitch's verdict on the
			// login. Without it, a Reauthenticate landing between the CAP ACK
			// and the 001 would latch the anonymous fallback and demote the
			// very session it was trying to upgrade.
			if ctx.Err() != nil || !cd.IsRunning() || cd.reauthPending.Load() {
				return
			}
			cd.noteHandshakeOutcome(welcomed, heardFromServer, sawLoginFailure)
		}()
	}
```

And in the read loop's error branch (`:270-279`), after the existing `ctx.Err()` check:

```go
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// Our own cancel. End the session now rather than spinning
			// chatMaxConsecutiveErrs failed reads against a context we
			// cancelled; Start's loop reads reauthPending and reconnects at
			// once. The error value itself is never compared against — see
			// errReauthRequested.
			if cd.reauthPending.Load() {
				return errReauthRequested
			}
			consecutiveErrors++
```

Finally, give an ACCEPTED login its own line. Spec R5 says "a credentialed `001` logs at Info and needs no further signal", and today nothing logs it: `welcomed = true` is set silently, and the only Info on this path is "joined twitch IRC", written before any reply arrives and for anonymous sessions too. Without this line the field gate ("confirm a credentialed `001` in the log") can never be met, and an operator who has just repaired their cookies has no positive confirmation at all — only the absence of a downgrade, which is not evidence.

In `internal/twitch/chat_irc.go`, in the read loop's handshake-outcome branch (`:302-307`):

```go
			if authenticated && !welcomed {
				if ircIsWelcome(line) {
					welcomed = true
					// The one positive signal on this path. Twitch has accepted
					// the account login, so this session captures subscriber-only
					// messages and badges — the thing every downgrade in this
					// file is about NOT having.
					//
					// Info, and once per session rather than once per
					// downloader: a reconnect that re-authenticates after a
					// credential repair is exactly the event an operator is
					// looking for, and a per-downloader latch would hide it.
					// Names the channel and nothing else — the account name is
					// in the 001 line's parameters and must not be echoed.
					cd.logger.Info("twitch chat: authenticated login accepted", "channel", cd.channelLogin)
				} else if ircIsLoginFailureNotice(line) {
					sawLoginFailure = true
				}
			}
```

- [ ] **Step 6: Run the tests and confirm they pass**

Run: `go test ./internal/twitch/ -run TestReauthenticate -v -count=1`
Expected: PASS for all five tests. `TestReauthenticateSpendsNoReconnectBudget` should finish in well under a second of wall time — if it takes tens of seconds, the backoff is still being applied.

Then the whole package, because `Start`'s loop and the handshake defer are under every chat test:
Run: `go test ./internal/twitch/ -count=1`
Expected: `ok  github.com/vampiricwulf/Moombox/internal/twitch`.

- [ ] **Step 7: Run the race detector on the new tests**

Run: `go test ./internal/twitch/ -run 'TestReauthenticate|TestIRC' -race -count=1`
Expected: PASS with no race report. `Reauthenticate` writes three atomics and reads `sessionCancel` under `cd.mu` from a goroutine other than the session's; a mutation that read `sessionCancel` without the lock is caught here and nowhere else.

- [ ] **Step 8: Run the gates**

```bash
go build ./...
go vet ./...
GOOS=linux go build ./...
gofmt -l internal/ cmd/
go test -count=1 ./...
```
Expected: build and vet clean, `gofmt -l` prints nothing, 27 packages ok / 0 fail.

- [ ] **Step 9: Commit**

```bash
git add internal/twitch/chat.go internal/twitch/chat_irc.go internal/twitch/chat_reauth_test.go
git commit -m "$(cat <<'EOF'
feat(twitch): a live chat session can be told to re-authenticate

Arc 10 R5. Credentials are read per IRC session, but nothing ended a healthy
session and authRefused latched for the life of the downloader — so an
operator who repaired cookies.txt mid-capture kept capturing anonymously
until the job ended.

Reauthenticate resets THREE latches, not the two the design named:
warnedNoLogin gates noteMissingLogin's own call to reportAuthDowngrade, so
leaving it set would silence the second report on a file that is still
missing its login row.

The drop rides the existing sessionCancel and is charged nothing against the
reconnect budget, which bounds retries against a network that will not stay
up rather than repairs the operator makes. One flag, reauthPending, armed
only when a session exists so it cannot suppress a later genuine refusal.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

---

### Task 5: `twitchChatRegistry` and the `ExecuteTwitch` registration

Spec R5's worker half. **No registry of live `*twitch.ChatDownloader` instances exists.** A downloader is built once per job in `processTwitchLive` (`stream_processor_twitch.go:487-504`), returned on `StreamProcessResult.TwitchChatDownloader` (`stream_processor.go:53`), read once in `processJob` (`worker.go:633-639`) and passed into `ExecuteTwitch` — after which it is reachable only through that one job goroutine's call stack. `queue.go:42`'s `activeDownloads` is a concurrency counter, not a registry.

**Where it lives, and why not `StreamProcessor`.** `StreamProcessor` already owns `activeChats []*chat.ChatDownloader` with `trackChat`/`untrackChat` (`stream_processor.go:96, :164-179`), so it looks like the home. It is the wrong one: the YouTube downloader is created AND stopped by the stream processor, whereas the Twitch downloader outlives `processTwitchLive` and is driven by `DownloadOrchestrator.ExecuteTwitch`. Registering where it is built would leave unregistration to be remembered at each of the four `Stop`/`MarkStreamEnded` sites (`orchestrator_twitch.go:795`, `:814`, `:833`, `:840`), none of which covers the error and panic exits. A dedicated type owned by `DownloadWorker` and registered from `ExecuteTwitch` with a `defer` covers every exit path with nothing to remember — the same shape `ExecuteTwitch` already uses for `o.db.OnJobUpdate` (`orchestrator_twitch.go:83-92`) and `o.conn.OnStateChange` (`:95-102`).

**Files:**
- Create: `internal/worker/twitch_chat_registry.go`
- Modify: `internal/worker/orchestrator.go:56-87` (one field, one line in the constructor's caller — the signature does not change)
- Modify: `internal/worker/orchestrator_twitch.go` — the registration, immediately after `irc, _ := twitchChatDl.(*twitch.ChatDownloader)` (`:320`)
- Modify: `internal/worker/worker.go` — one field on `DownloadWorker` (`:161-192`), the wiring in `NewDownloadWorker` (`:268-281`), and the exported accessor
- Test: `internal/worker/twitch_chat_registry_test.go` (create)

**Interfaces:**
- Consumes: `(*twitch.ChatDownloader).Reauthenticate()` (Task 4)
- Produces:
  - `type twitchChatReauthenticator interface { Reauthenticate() }`
  - `func newTwitchChatRegistry() *twitchChatRegistry`
  - `func (r *twitchChatRegistry) add(cd twitchChatReauthenticator) (remove func())`
  - `func (r *twitchChatRegistry) reauthenticateAll() int`
  - `func (w *DownloadWorker) ReauthenticateTwitchChats() int` — Task 7 calls this from `cmd/moombox`

- [ ] **Step 1: Write the failing tests**

Create `internal/worker/twitch_chat_registry_test.go`:

```go
package worker

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Arc 10 R5, worker half. Nothing here touches a credential: the registry's
// whole surface is "which downloaders are live" and "tell all of them".

// fakeChat counts Reauthenticate calls. The registry takes an interface rather
// than *twitch.ChatDownloader precisely so its own behaviour — add, remove,
// broadcast — is testable without a websocket.
//
// reentrant, when set, runs inside Reauthenticate. It is how
// TestBroadcastDoesNotHoldTheRegistryLock reaches back into the registry from
// the middle of a broadcast, which is the only way to observe a lock held
// across the calls.
type fakeChat struct {
	calls     atomic.Int64
	reentrant func()
}

func (f *fakeChat) Reauthenticate() {
	f.calls.Add(1)
	if f.reentrant != nil {
		f.reentrant()
	}
}

// TestRegistryBroadcastsToEveryLiveDownloader.
//
// The mutation: returning after the first entry, or breaking out of the loop.
// A single-entry broadcast passes any "something was told" assertion and
// leaves every concurrent capture but one anonymous — and concurrent Twitch
// captures are the normal case for this application.
func TestRegistryBroadcastsToEveryLiveDownloader(t *testing.T) {
	reg := newTwitchChatRegistry()
	a, b, c := &fakeChat{}, &fakeChat{}, &fakeChat{}
	reg.add(a)
	reg.add(b)
	reg.add(c)

	if got := reg.reauthenticateAll(); got != 3 {
		t.Errorf("reauthenticateAll() = %d, want 3", got)
	}
	for i, f := range []*fakeChat{a, b, c} {
		if n := f.calls.Load(); n != 1 {
			t.Errorf("downloader %d was told %d times, want exactly 1", i, n)
		}
	}
}

// TestRemovedDownloaderIsNotReauthenticated.
//
// The mutation: a remove closure that deletes the wrong key. A finished job's
// downloader would keep being told to reconnect, and Reauthenticate on a
// stopped downloader clears latches a resumed job then inherits.
func TestRemovedDownloaderIsNotReauthenticated(t *testing.T) {
	reg := newTwitchChatRegistry()
	stays, goes := &fakeChat{}, &fakeChat{}
	reg.add(stays)
	removeGoes := reg.add(goes)

	removeGoes()

	if got := reg.reauthenticateAll(); got != 1 {
		t.Errorf("reauthenticateAll() = %d after one removal, want 1", got)
	}
	if n := goes.calls.Load(); n != 0 {
		t.Errorf("the removed downloader was told %d times, want 0", n)
	}
	if n := stays.calls.Load(); n != 1 {
		t.Errorf("the remaining downloader was told %d times, want 1", n)
	}
}

// TestRemoveIsIdempotent. ExecuteTwitch defers the remove; a future edit that
// also removes on an explicit path must not corrupt the registry or panic.
//
// The mutation: a slice-based registry whose second removal panics on, or
// re-slices around, an index it no longer holds — taking the surviving entry
// with it.
func TestRemoveIsIdempotent(t *testing.T) {
	reg := newTwitchChatRegistry()
	stays := &fakeChat{}
	reg.add(stays)
	remove := reg.add(&fakeChat{})

	remove()
	remove()

	if got := reg.reauthenticateAll(); got != 1 {
		t.Errorf("reauthenticateAll() = %d after a double removal, want 1", got)
	}
}

// TestRegistryIsSafeUnderConcurrentJobs. Jobs start and finish while a
// credential change broadcasts; that is the ordinary case, not an edge one.
//
// Two mutations. Dropping the mutex: caught only under -race, which the step
// below runs explicitly — so this test's value is largely in that run, and it
// is listed separately so nobody deletes the -race step believing it is
// redundant. And a slice-index remove that SHIFTS under a concurrent add: the
// final assertion catches that one, because a shifted index removes the wrong
// entry and leaves a live one behind. Neither is reachable from a sequential
// test, which is why neither belongs on TestRemovedDownloaderIsNotReauthenticated.
func TestRegistryIsSafeUnderConcurrentJobs(t *testing.T) {
	reg := newTwitchChatRegistry()
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f := &fakeChat{}
			remove := reg.add(f)
			reg.reauthenticateAll()
			remove()
		}()
	}
	wg.Wait()

	if got := reg.reauthenticateAll(); got != 0 {
		t.Errorf("reauthenticateAll() = %d after every job removed itself, want 0", got)
	}
}

// TestBroadcastDoesNotHoldTheRegistryLock is the claim reauthenticateAll's
// comment makes and that no other test here can make.
//
// A fakeChat that returns immediately is invisible to the mutation that
// matters: holding the registry mutex across the Reauthenticate calls passes
// every other test in this file, with or without -race, and only shows up in
// production as a Twitch job that cannot START while a credential change is
// being broadcast to the jobs already running.
//
// So the fake RE-ENTERS the registry, which is exactly what a starting job
// does. The re-entrant call runs on its own goroutine and is waited for with a
// bounded timeout: under the mutation it blocks on a mutex the broadcast still
// holds, and the wait below reports that rather than hanging until the test
// binary's own timeout.
func TestBroadcastDoesNotHoldTheRegistryLock(t *testing.T) {
	reg := newTwitchChatRegistry()
	registered := make(chan struct{})

	reg.add(&fakeChat{reentrant: func() {
		go func() {
			remove := reg.add(&fakeChat{})
			remove()
			close(registered)
		}()
		select {
		case <-registered:
		case <-time.After(2 * time.Second):
			t.Error("a job registering while a broadcast was in flight blocked — reauthenticateAll is holding the registry lock across Reauthenticate")
		}
	}})

	if got := reg.reauthenticateAll(); got != 1 {
		t.Errorf("reauthenticateAll() = %d, want 1", got)
	}
}

// TestNilRegistryIsInert. DownloadWorker may be constructed in a test or a
// partially initialised process without one; a nil deref at the moment an
// operator repairs their cookies is the worst possible time for one.
//
// The mutation: dropping either nil guard.
func TestNilRegistryIsInert(t *testing.T) {
	var reg *twitchChatRegistry
	reg.add(&fakeChat{})() // add returns a no-op remove; calling it must not panic
	if got := reg.reauthenticateAll(); got != 0 {
		t.Errorf("a nil registry broadcast to %d downloaders", got)
	}
}

// TestReauthenticateTwitchChatsReachesTheRegistry pins the worker's exported
// accessor to the registry rather than to a second mechanism.
//
// The mutation: an accessor that returns 0 unconditionally, or one wired to a
// registry the orchestrator does not share — either leaves cmd/moombox's
// broadcast reaching nothing while every test above still passes.
func TestReauthenticateTwitchChatsReachesTheRegistry(t *testing.T) {
	reg := newTwitchChatRegistry()
	f := &fakeChat{}
	reg.add(f)

	w := &DownloadWorker{twitchChats: reg}
	if got := w.ReauthenticateTwitchChats(); got != 1 {
		t.Errorf("ReauthenticateTwitchChats() = %d, want 1", got)
	}
	if n := f.calls.Load(); n != 1 {
		t.Errorf("the registered downloader was told %d times, want 1", n)
	}

	var nilWorker *DownloadWorker
	if got := nilWorker.ReauthenticateTwitchChats(); got != 0 {
		t.Errorf("a nil worker broadcast to %d downloaders", got)
	}
	if got := (&DownloadWorker{}).ReauthenticateTwitchChats(); got != 0 {
		t.Errorf("a worker with no registry broadcast to %d downloaders", got)
	}
}

// TestOrchestratorAndWorkerShareOneRegistry is the wiring pin.
//
// ExecuteTwitch registers into the ORCHESTRATOR's registry and cmd/moombox
// broadcasts through the WORKER's. If NewDownloadWorker built two, every test
// above would still pass and the feature would be dead in production — the
// broadcast would reach an always-empty map.
//
// The mutation: `orchestrator: NewDownloadOrchestrator(...)` left with its own
// freshly constructed registry, or a second newTwitchChatRegistry() call.
func TestOrchestratorAndWorkerShareOneRegistry(t *testing.T) {
	reg := newTwitchChatRegistry()
	orch := &DownloadOrchestrator{twitchChats: reg}
	w := &DownloadWorker{orchestrator: orch, twitchChats: reg}

	f := &fakeChat{}
	remove := orch.twitchChats.add(f)
	defer remove()

	if got := w.ReauthenticateTwitchChats(); got != 1 {
		t.Fatalf("a downloader registered through the orchestrator was not reached through the worker (%d told)", got)
	}
	if w.twitchChats != orch.twitchChats {
		t.Error("the worker and the orchestrator hold different registries")
	}
}
```

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/worker/ -run 'TestRegistry|TestRemove|TestNilRegistry|TestBroadcastDoesNotHold|TestReauthenticateTwitchChats|TestOrchestratorAndWorker' -v`
Expected: compile failure — `undefined: newTwitchChatRegistry`, `unknown field twitchChats`.

- [ ] **Step 3: Create the registry**

Create `internal/worker/twitch_chat_registry.go`:

```go
package worker

import "sync"

// twitchChatReauthenticator is the slice of *twitch.ChatDownloader the
// registry needs.
//
// An interface rather than the concrete type, so the registry's own
// behaviour — add, remove, broadcast under concurrency — is testable without a
// websocket server. It is deliberately one method: a registry that could also
// Stop() or MessageCount() would invite callers to reach a running job's
// downloader for things that belong to the job goroutine.
type twitchChatReauthenticator interface {
	Reauthenticate()
}

// twitchChatRegistry holds every live Twitch IRC chat downloader so a
// credential change can reach all of them at once.
//
// It exists because nothing else can reach them. A downloader is built per job
// in processTwitchLive, handed to ExecuteTwitch, and from there lives only
// inside one job goroutine's call stack — so an operator repairing cookies.txt
// during three concurrent Twitch captures had no path to any of them, and each
// job stayed anonymous until it ended.
//
// Keyed by an opaque counter rather than by job ID. The only question this type
// answers is "which downloaders are live"; the entry is removed by the closure
// add returns, so no caller needs a key, and a job ID would invite a second
// question this type has no business answering.
//
// VOD chat is deliberately absent. VodChatDownloader polls GQL and re-reads its
// bearer token per page, so a repaired credential reaches it on its own.
type twitchChatRegistry struct {
	mu      sync.Mutex
	next    uint64
	entries map[uint64]twitchChatReauthenticator
}

func newTwitchChatRegistry() *twitchChatRegistry {
	return &twitchChatRegistry{entries: make(map[uint64]twitchChatReauthenticator)}
}

// add registers one downloader and returns the function that removes it.
//
// The remove-closure shape, rather than a Remove(id) method, is the same one
// database.OnJobUpdate uses and is chosen for the same reason: the caller can
// `defer` it beside the registration and cannot hold the wrong key.
// ExecuteTwitch defers it, so every job exit path — finish, error, user cancel,
// shutdown, connectivity finalize, panic — unregisters, and no exit path has
// to remember to.
//
// Calling the returned function more than once is safe.
func (r *twitchChatRegistry) add(cd twitchChatReauthenticator) (remove func()) {
	if r == nil || cd == nil {
		return func() {}
	}
	r.mu.Lock()
	id := r.next
	r.next++
	r.entries[id] = cd
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		delete(r.entries, id)
		r.mu.Unlock()
	}
}

// reauthenticateAll tells every live downloader to re-read its credentials and
// reconnect, and returns how many were told.
//
// The count is a NUMBER and is the only thing that leaves this function.
// Nothing here may surface which channel, which job or which account — the
// caller logs the count and stops.
//
// The snapshot is taken under the lock and the calls are made outside it,
// following StreamProcessor.Stop's convention: Reauthenticate takes the
// downloader's own mutex and cancels a context, and holding the registry lock
// across N of those would put an unrelated job's registration behind them.
func (r *twitchChatRegistry) reauthenticateAll() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	live := make([]twitchChatReauthenticator, 0, len(r.entries))
	for _, cd := range r.entries {
		live = append(live, cd)
	}
	r.mu.Unlock()

	for _, cd := range live {
		cd.Reauthenticate()
	}
	return len(live)
}
```

- [ ] **Step 4: Give the orchestrator and the worker the SAME registry**

In `internal/worker/orchestrator.go`, add one field to `DownloadOrchestrator` (`:56-67`), after `conn`:

```go
	// twitchChats is the live Twitch IRC chat downloaders, shared with the
	// DownloadWorker that owns this orchestrator (NewDownloadWorker assigns
	// ONE registry to both). ExecuteTwitch registers into it; cmd/moombox
	// broadcasts through the worker's accessor. nil is inert.
	twitchChats *twitchChatRegistry
```

The constructor signature is deliberately unchanged — nine parameters is already too many, and the field is assigned by the only caller.

In `internal/worker/worker.go`, add one field to `DownloadWorker` (after `orchestrator`, `:169`):

```go
	// twitchChats is the same registry the orchestrator holds. Kept here so
	// cmd/moombox has an exported path to it without reaching through the
	// orchestrator, which is otherwise entirely internal to this package.
	twitchChats *twitchChatRegistry
```

and in `NewDownloadWorker`, replace the `return &DownloadWorker{...}` construction's orchestrator line with an explicit two-step so both hold ONE registry:

```go
	// ONE registry, two holders. ExecuteTwitch registers into the
	// orchestrator's; cmd/moombox broadcasts through the worker's. Two
	// registries would compile, pass every registry unit test, and leave the
	// broadcast reaching an always-empty map.
	twitchChats := newTwitchChatRegistry()
	orchestrator := NewDownloadOrchestrator(db, queue, cfg.Paths.FfmpegPath, logger, cs, routedCs, pp, nm, conn)
	orchestrator.twitchChats = twitchChats

	return &DownloadWorker{
		db:           db,
		yt:           yt,
		tw:           tw,
		cfg:          cfg,
		queue:        queue,
		scheduler:    sched,
		orchestrator: orchestrator,
		twitchChats:  twitchChats,
		streamProc:   sp,
		notifier:     nm,
		logger:       logger,
		notifyJob:    make(chan struct{}, 1),
	}
```

Add the accessor beside the other `DownloadWorker` setters (after `SetParallelDownloads`, `worker.go:1166`):

```go
// ReauthenticateTwitchChats tells every live Twitch IRC chat downloader to
// re-read its credentials and reconnect, and returns how many were told.
//
// Called by cmd/moombox from RefreshService.OnCredentialsChanged("twitch") —
// the only signal a capture that is already running has that repaired cookies
// are on disk. Returns a COUNT and nothing else: no channel, no job, no
// account.
//
// Nil-safe on both the receiver and the registry, so a partially constructed
// worker degrades to "nothing to tell" rather than panicking at the moment an
// operator fixes their credentials.
func (w *DownloadWorker) ReauthenticateTwitchChats() int {
	if w == nil {
		return 0
	}
	return w.twitchChats.reauthenticateAll()
}
```

- [ ] **Step 5: Register from `ExecuteTwitch`**

In `internal/worker/orchestrator_twitch.go`, immediately after `irc, _ := twitchChatDl.(*twitch.ChatDownloader)` (`:320`):

```go
	// Register the live IRC downloader so a Twitch credential change can reach
	// it MID-JOB (Arc 10 R5). Until now this object was reachable only through
	// this goroutine's call stack.
	//
	// Deferred here rather than at any of the Stop / MarkStreamEnded sites
	// below: this defer covers EVERY exit — finish, error, user cancel,
	// shutdown, connectivity finalize and panic — so there is no exit path
	// that has to remember to unregister, which is exactly how a registry
	// keyed to long-running jobs leaks. Same shape as the OnJobUpdate and
	// OnStateChange unsubscribes above.
	//
	// Only the IRC downloader. The VOD chat downloader re-reads its bearer
	// token per GQL page, so it needs no signal.
	if irc != nil {
		unregisterChat := o.twitchChats.add(irc)
		defer unregisterChat()
	}
```

- [ ] **Step 6: Run the tests and confirm they pass**

Run: `go test ./internal/worker/ -run 'TestRegistry|TestRemove|TestNilRegistry|TestBroadcastDoesNotHold|TestReauthenticateTwitchChats|TestOrchestratorAndWorker' -v`
Expected: PASS for all eight tests. `TestBroadcastDoesNotHoldTheRegistryLock` must finish immediately — if it takes two seconds and then fails, the broadcast is holding the lock.

Run: `go test ./internal/worker/ -count=1`
Expected: `ok  github.com/vampiricwulf/Moombox/internal/worker`.

- [ ] **Step 7: Run the race detector**

Run: `go test ./internal/worker/ -run 'TestRegistry|TestRemove|TestNilRegistry|TestBroadcastDoesNotHold' -race -count=1`
Expected: PASS with no race report.

- [ ] **Step 8: Run the gates**

```bash
go build ./...
go vet ./...
GOOS=linux go build ./...
gofmt -l internal/ cmd/
go test -count=1 ./...
```
Expected: build and vet clean, `gofmt -l` prints nothing, 27 packages ok / 0 fail.

**Named residual, carried to the field-gate list:** the registration site inside `ExecuteTwitch` is covered by compilation and by the registry's own tests, not by an end-to-end test. Reaching it needs a real Twitch download (a network, a live broadcast and FFmpeg), which this suite does not have. `TestOrchestratorAndWorkerShareOneRegistry` catches the mistake most likely to make it dead (two registries); the remaining risk — the `defer` being deleted or the `irc != nil` branch being inverted — is caught by the first real capture.

- [ ] **Step 9: Commit**

```bash
git add internal/worker/twitch_chat_registry.go internal/worker/twitch_chat_registry_test.go internal/worker/orchestrator.go internal/worker/orchestrator_twitch.go internal/worker/worker.go
git commit -m "$(cat <<'EOF'
feat(worker): a registry of live Twitch chat downloaders

Arc 10 R5. A Twitch chat downloader was built per job and then reachable
only through one job goroutine's call stack, so nothing outside that stack
could tell it credentials had changed.

One registry, held by both the worker and the orchestrator, registered from
ExecuteTwitch with a defer — which covers finish, error, user cancel,
shutdown, connectivity finalize and panic, so no exit path has to remember
to unregister. The broadcast returns a count and nothing else.

Not on StreamProcessor beside activeChats: the YouTube downloader is created
and stopped there, while the Twitch one outlives processTwitchLive and is
driven by the orchestrator, whose four Stop sites do not cover the error
exits.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

---

### Task 6: The worker seam — a downgrade marks the platform

Spec R1's wiring end. `twitchChatDowngradeCallback` (`stream_processor_twitch.go:217-221`) is `OnAuthDowngrade`'s production value and today does exactly one thing: send the per-job notification. R1 adds a second consumer of the same report — the platform mark — and the split is the point: **the notification names the JOB, the mark names the PLATFORM.** Both survive; neither replaces the other.

**A func seam, not an import.** `internal/worker` must not import `internal/cookies` (it does not today; only one worker TEST file does). The seam mirrors `DownloadWorker.CurrentCredentialIdentity` (`worker.go:185-191`), which exists for the same reason.

**Resolved at fire time, not captured** — the same rule `notifierSend` (`:195-200`) already states, and for a sharper reason here: the report can arrive hours into a capture, and the resolver must yield whatever is wired now.

**Files:**
- Modify: `internal/worker/stream_processor.go` — one field on `StreamProcessor` (`:76-97`) and a setter beside `SetWakeScheduler` (`:139-141`)
- Modify: `internal/worker/stream_processor_twitch.go` — a resolver beside `notifierSend` (`:195-200`), `twitchChatDowngradeCallback`'s signature and body (`:217-221`), and its call site (`:502`)
- Modify: `internal/worker/worker.go` — `SetOnTwitchAuthLoss` forwarding to the stream processor
- Test: `internal/worker/stream_processor_twitch_credentials_test.go` (append, AND fix the three pre-existing calls to `twitchChatDowngradeCallback` at `:327`, `:356` and `:386` — Step 4 changes its arity, so all three must gain the new argument or the package does not compile) and `internal/worker/twitch_auth_loss_vocabulary_test.go` (create)

**Interfaces:**
- Consumes: `notifySend` and `sendTwitchChatDowngrade` (`stream_processor_twitch.go:88-90`, `:178-184`); `twitch.AuthDowngrade*` constants (`chat.go:38-54`); `cookies.RefreshService.NoteTwitchAuthLoss(reason string)` (Task 2) — only through the injected func
- Produces:
  - `func (sp *StreamProcessor) SetOnTwitchAuthLoss(fn func(reason string))`
  - `func (w *DownloadWorker) SetOnTwitchAuthLoss(fn func(reason string))` — Task 7 calls this
  - `twitchChatDowngradeCallback(resolveSend func() notifySend, resolveMark func() func(reason string), job *database.Job, channel string) func(reason string)`

- [ ] **Step 1: Write the failing tests**

Append to `internal/worker/stream_processor_twitch_credentials_test.go`:

```go
// TestTwitchChatDowngradeCallbackMarksThePlatformAndNotifiesTheJob is Arc 10
// R1's wiring claim: ONE report, TWO consumers.
//
// The notification names the job ("Twitch chat is anonymous for X") and the
// mark names the platform. A change that replaced one with the other would
// pass any "something happened" assertion and lose half the behaviour: without
// the notice the operator is not told which capture is degraded; without the
// mark the platform stays green and no recovery is attempted.
//
// The mutation: dropping either call from the callback.
func TestTwitchChatDowngradeCallbackMarksThePlatformAndNotifiesTheJob(t *testing.T) {
	rec := &recordingNotifier{}
	var marked []string

	cb := twitchChatDowngradeCallback(
		func() notifySend { return rec.send },
		func() func(string) { return func(reason string) { marked = append(marked, reason) } },
		downgradeJob(), "TestChan")
	cb(twitch.AuthDowngradeNoLoginCookie)

	if len(marked) != 1 || marked[0] != twitch.AuthDowngradeNoLoginCookie {
		t.Errorf("the platform mark received %v, want exactly [%s]", marked, twitch.AuthDowngradeNoLoginCookie)
	}
	if len(rec.sent) != 1 {
		t.Fatalf("the notifier received %d notices, want exactly 1", len(rec.sent))
	}
	if rec.sent[0].title != "Twitch chat is anonymous for TestChan" {
		t.Errorf("the job notice no longer names the channel: %q", rec.sent[0].title)
	}
	// The mark carries the vocabulary token and nothing else — the same
	// no-credential property assertNoCredentialInReports pins on the notice
	// side, restated for the new consumer.
	if strings.ContainsAny(marked[0], " ;=") {
		t.Errorf("the mark received %q, which is not a bare vocabulary token", marked[0])
	}
}

// TestTwitchChatDowngradeCallbackResolvesTheMarkPerFire.
//
// The resolver is called at FIRE time, not captured at construction. A report
// can arrive hours into a capture, and the seam is wired during startup — a
// captured nil would silence the mark for the life of the process on any
// ordering where the downloader is built first.
//
// The mutation: `mark := resolveMark()` hoisted out of the returned closure.
func TestTwitchChatDowngradeCallbackResolvesTheMarkPerFire(t *testing.T) {
	rec := &recordingNotifier{}
	var marked []string
	var live func(string)

	cb := twitchChatDowngradeCallback(
		func() notifySend { return rec.send },
		func() func(string) { return live },
		downgradeJob(), "TestChan")

	cb(twitch.AuthDowngradeLoginRefused) // nothing wired yet: must not panic
	if len(marked) != 0 {
		t.Fatalf("marks = %v before anything was wired", marked)
	}

	live = func(reason string) { marked = append(marked, reason) }
	cb(twitch.AuthDowngradeLoginRefused)

	if len(marked) != 1 {
		t.Errorf("marks = %v, want one after the seam was wired — the resolver was captured, not called per fire", marked)
	}
}

// TestTwitchChatDowngradeCallbackWithNoMarkStillNotifies. An install with no
// refresh service wired is not an error, and the per-job notice is exactly the
// signal it still needs.
//
// The mutation: `if mark == nil { return }` placed before the send.
func TestTwitchChatDowngradeCallbackWithNoMarkStillNotifies(t *testing.T) {
	rec := &recordingNotifier{}

	cb := twitchChatDowngradeCallback(
		func() notifySend { return rec.send },
		func() func(string) { return nil },
		downgradeJob(), "TestChan")
	cb(twitch.AuthDowngradeUnusableLoginCookie)

	if len(rec.sent) != 1 {
		t.Errorf("the notifier received %d notices with no mark wired, want 1", len(rec.sent))
	}
}

// TestStreamProcessorTwitchAuthLossResolverIsNilSafeAndLive pins the
// production resolver, which is what the callback above is handed.
//
// The mutation: returning a non-nil zero func, or capturing sp.onTwitchAuthLoss
// at construction.
func TestStreamProcessorTwitchAuthLossResolverIsNilSafeAndLive(t *testing.T) {
	sp := &StreamProcessor{}
	if fn := sp.twitchAuthLossReporter(); fn != nil {
		t.Error("an unwired stream processor returned a non-nil auth-loss reporter")
	}

	var got []string
	sp.SetOnTwitchAuthLoss(func(reason string) { got = append(got, reason) })
	fn := sp.twitchAuthLossReporter()
	if fn == nil {
		t.Fatal("a wired stream processor returned a nil auth-loss reporter")
	}
	fn(twitch.AuthDowngradeLoginRefused)
	if len(got) != 1 || got[0] != twitch.AuthDowngradeLoginRefused {
		t.Errorf("the reporter delivered %v, want [%s]", got, twitch.AuthDowngradeLoginRefused)
	}
}
```

Create `internal/worker/twitch_auth_loss_vocabulary_test.go`:

```go
package worker

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// TestTwitchAuthLossVocabularyCoversEveryDowngradeReason is the drift pin
// internal/cookies asks for and cannot write itself.
//
// internal/cookies mirrors internal/twitch's AuthDowngrade* tokens BY VALUE,
// because internal/twitch imports internal/cookies and the dependency only
// runs one way. This package imports BOTH, so it is the only place the two
// vocabularies can be compared.
//
// The mutation this catches is the realistic one: a fifth AuthDowngrade* route
// added upstream with no arm in twitchAuthLossMessage, or a token whose value
// is edited on one side. Either lands the operator on the generic
// "the saved Twitch login could not be used", which names no remedy.
//
// It asserts through the SERVICE rather than against a copy of the sentences:
// a table of expected strings here would be a second source of truth and would
// pass while the mapping was wrong in both places.
func TestTwitchAuthLossVocabularyCoversEveryDowngradeReason(t *testing.T) {
	reasons := []string{
		twitch.AuthDowngradeLoginRefused,
		twitch.AuthDowngradeLoginUnacknowledged,
		twitch.AuthDowngradeNoLoginCookie,
		twitch.AuthDowngradeUnusableLoginCookie,
	}

	// The sentence an unrecognised token renders, discovered rather than
	// hardcoded, so this test cannot drift from the default arm's wording.
	generic := twitchAuthLossSentence(t, "a-token-no-arm-was-ever-written-for")

	seen := map[string]string{}
	for _, reason := range reasons {
		got := twitchAuthLossSentence(t, reason)
		if got == generic {
			t.Errorf("%q renders the generic sentence — internal/cookies has no arm for it", reason)
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("%q and %q render the same sentence %q", prev, reason, got)
		}
		seen[got] = reason
	}
}

// twitchAuthLossSentence drives one reason through the real RefreshService and
// returns the sentence it published. No network: NoteTwitchAuthLoss makes no
// request, and with no callbacks wired it invokes nothing.
// It uses an EMPTY jar on purpose: with no Twitch cookies configured,
// shouldFireRecovery declines, so no callback fires and nothing here can reach
// a network or a notifier.
//
// nopWorkerLogger is the package's existing discard logger
// (stream_processor_twitch_credentials_test.go:403) and already satisfies the
// anonymous logger interface cookies.NewRefreshService takes.
func twitchAuthLossSentence(t *testing.T, reason string) string {
	t.Helper()
	rs := cookies.NewRefreshService(cookies.NewCookieJar(), 0, nopWorkerLogger{})
	rs.NoteTwitchAuthLoss(reason)
	got := rs.GetStatus().TwitchError
	if got == "" {
		t.Fatalf("NoteTwitchAuthLoss(%q) published no reason at all", reason)
	}
	return got
}
```

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/worker/ -run 'TestTwitchChatDowngradeCallbackMarks|TestTwitchChatDowngradeCallbackResolvesTheMark|TestTwitchChatDowngradeCallbackWithNoMark|TestStreamProcessorTwitchAuthLoss|TestTwitchAuthLossVocabulary' -v`
Expected: compile failure — `too many arguments in call to twitchChatDowngradeCallback`, `sp.SetOnTwitchAuthLoss undefined`, `sp.twitchAuthLossReporter undefined`.

Note the new vocabulary test file imports `internal/cookies` into the `worker` package's TESTS. That is legal and already precedented (`stream_processor_twitch_credentials_test.go:9` imports it); production `internal/worker` code still must not.

- [ ] **Step 3: Add the field, the setter and the resolver**

In `internal/worker/stream_processor.go`, add to the `StreamProcessor` struct after `wakeScheduler` (`:91`):

```go
	// onTwitchAuthLoss reports a Twitch chat downgrade to whatever owns the
	// PLATFORM's credential status — cookies.RefreshService.NoteTwitchAuthLoss
	// in production, wired by cmd/moombox. nil is a valid install and the
	// per-job notification still goes out.
	//
	// A func rather than the service, so internal/worker does not import
	// internal/cookies. Same inversion, and the same reason, as
	// DownloadWorker.CurrentCredentialIdentity.
	onTwitchAuthLoss func(reason string)
```

and the setter beside `SetWakeScheduler` (`:139-141`):

```go
// SetOnTwitchAuthLoss wires the platform-mark seam. Called during startup,
// long before any job goroutine exists.
func (sp *StreamProcessor) SetOnTwitchAuthLoss(fn func(reason string)) {
	sp.onTwitchAuthLoss = fn
}
```

In `internal/worker/stream_processor_twitch.go`, add the resolver beside `notifierSend` (`:200`):

```go
// twitchAuthLossReporter resolves the platform-mark seam, or nil when none is
// wired.
//
// Read at CALL time rather than captured, for the reason notifierSend states
// and one sharper: a downgrade report can arrive hours into a capture, so a
// value captured when the downloader was built could be a nil from before
// startup finished wiring.
func (sp *StreamProcessor) twitchAuthLossReporter() func(reason string) {
	return sp.onTwitchAuthLoss
}
```

In `internal/worker/worker.go`, add the forwarding setter beside the other `DownloadWorker` setters:

```go
// SetOnTwitchAuthLoss wires the Twitch platform-mark seam through to the
// stream processor, which is where the chat downgrade is observed.
//
// cmd/moombox holds both the refresh service and the worker; the worker holds
// the stream processor. This is the same one-hop forwarding SetConfigStore
// does, and it exists so cmd/moombox never has to know that the stream
// processor is where the callback lands.
func (w *DownloadWorker) SetOnTwitchAuthLoss(fn func(reason string)) {
	if w.streamProc != nil {
		w.streamProc.SetOnTwitchAuthLoss(fn)
	}
}
```

- [ ] **Step 4: Fold the mark into the downgrade callback**

In `internal/worker/stream_processor_twitch.go`, replace `twitchChatDowngradeCallback` (`:217-221`) and its doc:

```go
// twitchChatDowngradeCallback builds the OnAuthDowngrade callback for one live
// chat downloader. It does TWO things with ONE report, and the split is the
// point: the notification names the JOB, the mark names the PLATFORM.
//
// Neither replaces the other. Without the notice an operator is not told which
// capture is degraded, or which channel's subscriber-only messages are being
// lost. Without the mark the platform stays green — validate answers 200 for
// two of the four routes — so nothing attempts recovery and the next capture
// starts anonymous too.
//
// BOTH resolvers are called at FIRE time and both may yield nil. Injecting the
// LOOKUP rather than the sender is what makes this closure the same object in
// production and under test, and it is the one place a reason could be
// dropped, rewritten, or replaced by a constant on its way from the downloader
// to either consumer — a defect no test that calls sendTwitchChatDowngrade
// directly can see. The production lookups are StreamProcessor.notifierSend
// and StreamProcessor.twitchAuthLossReporter.
//
// Order: the mark first. Both are cheap and neither can fail, but the mark is
// what a UI reads on the next request and what fires the recovery attempt,
// and sendTwitchChatDowngrade's target list can be long.
//
// The downloader guarantees at most one call per CREDENTIAL PAIR (Arc 10:
// Reauthenticate resets its latches when the cookie file changes), so there is
// no dedup here — and dedup ACROSS jobs is deliberately absent: a second job on
// the same channel an hour later with the same dead cookies must notify again,
// because by then the operator may believe they fixed it.
func twitchChatDowngradeCallback(
	resolveSend func() notifySend,
	resolveMark func() func(reason string),
	job *database.Job,
	channel string,
) func(reason string) {
	return func(reason string) {
		if mark := resolveMark(); mark != nil {
			mark(reason)
		}
		sendTwitchChatDowngrade(resolveSend(), job, channel, reason)
	}
}
```

and update the construction site (`:502`):

```go
				OnAuthDowngrade: twitchChatDowngradeCallback(
					sp.notifierSend, sp.twitchAuthLossReporter, job, chatChannel),
```

- [ ] **Step 5: Run the tests and confirm they pass**

**THREE pre-existing call sites must gain the new second argument or the package does not compile.** Step 4 changed `twitchChatDowngradeCallback` from three parameters to four; `stream_processor_twitch_credentials_test.go` calls it at `:327`, `:356` and `:386`. Pass `func() func(string) { return nil }` as the second argument at each, and change nothing else about these tests — they pin the notification half and must keep doing so unchanged:

- `:327`, inside `TestTwitchChatDowngradeWithoutANotifierIsSilent` (`:321`) — `twitchChatDowngradeCallback(sp.notifierSend, downgradeJob(), "TestChan")`. This one is easy to miss: it is not named "…Callback…", it drives the production `sp.notifierSend` resolver rather than a hand-written one, and it is the only test covering the no-notifier install.
- `:356`, inside `TestTwitchChatDowngradeCallbackDeliversWhatTheDownloaderReports` (`:347`).
- `:386`, inside `TestTwitchChatDowngradeCallbackResolvesTheSenderPerFire` (`:383`).

Run: `go test ./internal/worker/ -run 'TestTwitchChatDowngrade|TestStreamProcessorTwitchAuthLoss|TestTwitchAuthLossVocabulary' -v`
Expected: PASS. The regex matches the six pre-existing `TestTwitchChatDowngrade*` tests (`:183`, `:245`, `:292`, `:321`, `:347`, `:383`) plus the five added in Step 1 — eleven in all.

Run: `go test ./internal/worker/ -count=1`
Expected: `ok  github.com/vampiricwulf/Moombox/internal/worker`.

- [ ] **Step 6: Run the gates**

```bash
go build ./...
go vet ./...
GOOS=linux go build ./...
gofmt -l internal/ cmd/
go test -count=1 ./...
```
Expected: build and vet clean, `gofmt -l` prints nothing, 27 packages ok / 0 fail.

- [ ] **Step 7: Commit**

```bash
git add internal/worker/stream_processor.go internal/worker/stream_processor_twitch.go internal/worker/worker.go internal/worker/stream_processor_twitch_credentials_test.go internal/worker/twitch_auth_loss_vocabulary_test.go
git commit -m "$(cat <<'EOF'
feat(worker): a Twitch chat downgrade marks the platform as well as the job

Arc 10 R1's wiring end. One report, two consumers: the notification names
the job, the mark names the platform. Neither replaces the other — without
the notice nobody knows WHICH capture is degraded; without the mark the
platform stays green, because oauth2/validate answers 200 for two of the
four routes.

A func seam, not an import: internal/worker still does not import
internal/cookies, mirroring CurrentCredentialIdentity. Both resolvers are
called at fire time, because a report can arrive hours into a capture.

The vocabulary pin lives here because this is the only package that imports
both internal/twitch and internal/cookies, and it drives the real service
rather than restating the sentences.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

---

### Task 7: `cmd/moombox` wiring — the mark, the broadcast, and the adapted sweep

`cmd/moombox` is the only package that holds the refresh service, the worker and the jar at once, so both ends of the arc close here. Four edits — the fourth was found by the Task 2 review and belongs to nobody else.

**0. THE MARK'S REASON REACHES NO OPERATOR TODAY.** Spec R1 says "every existing surface reuses this ... the per-request reason rendering (`R C`, `POST /api/cookies/recheck`, `/api/status`)". It does not, and the reason is a gate neither Task 2 nor Task 3 touches: **both** per-request renderers append the reason string only on an INCONCLUSIVE verdict —

- TUI, `internal/tui/app_update.go:834` in `cookieRecheckFeedback`: `if verdict == cookies.RefreshUnknown && reason != ""`
- Web, `web/public/modules/utils.js:533` in `cookieIndicatorState`: `if (status?.verification === "unknown")`, with the reason appended inside that arm only

The mark writes `RefreshFailed` **with** a non-empty reason. It is the first producer in the codebase to do that, so under Tasks 2-3 alone an operator pressing `R C` sees `Cookies: Twitch not authenticated` and nothing else — no hint which of the four routes broke, which is the whole point of having four. The gates were right when they were written (every earlier producer really did leave the field empty on a conclusive verdict, and both comments say so); they are wrong now. Steps 4a and 4b widen both to "render it when it is there", which is also the simpler rule.

**`RefreshOK` is unaffected in practice and needs no special case**: `verdictFromCheck` returns `RefreshOK` only when `err == nil`, and the error string is that same error — so an OK verdict's reason is always `""` and the widened gate never fires for it. That is a property of the producer, not a promise from the caller, which is why the new gate tests the STRING rather than trusting the verdict.

**The push-driven bars are deliberately NOT touched.** `authStatusChanged` still excludes the two strings, so no `OnAuthChange`-driven surface may render them — the TUI status bar and the header badge's push path stay exactly as they are. That exclusion was re-ruled during this arc as DOCUMENT, not widen: the reason rides the per-request surfaces only. Task 9 states it.

**1. The mark seam runs on its own goroutine.** `NoteTwitchAuthLoss`'s caller chain starts on `internal/twitch`'s IRC session goroutine, with the read loop parked behind it (`ChatDownloaderOptions.OnAuthDowngrade`'s contract, `chat.go:181-189`: *"must not block"*). The mark can reach `handleRecoveryNeeded`'s `auto_enabled=false` arm (`monitor_callbacks.go:503-544`), which sends the "Cookie Re-Authentication Required" webhook **synchronously**. Spec §4 requires the mark be delivered asynchronously by its consumer; this is that consumer. Fire-and-forget is correct: the mark is idempotent, and the downloader already latches its report once per job, so there is nothing to sequence and nothing to wait for.

**2. The credential-change subscriber gains the broadcast and is ADAPTED, not filtered.** `resumeCookieParkedJobs` already gates on `job.Platform`, and every Twitch `COOKIES?` job carries `ParkReasonAuth` (`parkReasonForError`, `worker.go:895-902`, returns `ParkReasonMembership` only for `ErrNotAMember`, which is YouTube-only), so a Twitch fire resumes exactly the Twitch jobs it should. What is wrong for Twitch is the notification's sentence — *"after re-checking the signed-in account"* — which describes a Google account, not a bearer token. Task 7 owns that sentence and Step 4 replaces it; Task 9 adapts `operations.md`'s row to match.

**2a. THERE ARE TWO REPAIR EDGES, and the broadcast must ride both.** Found by the Task 3 review. `refresh` fires `OnCredentialsChanged("twitch", …)` when the credential FINGERPRINT moves, and `OnAuthRecovered("twitch")` when validate goes not-authenticated → authenticated. Those are different events and one does not imply the other:

- A transient Twitch-side refusal (a 401 during an outage, a token that starts validating again) recovers auth with the fingerprint **unchanged** — `shouldObserveCredentials` returns false, so `OnCredentialsChanged` never fires. Same for an operator who restores the exact previous pair after a bad edit.
- A same-account rotation, or a swap to an account that does not validate, moves the fingerprint with no auth transition.

A chat session that went anonymous on a refusal and is repaired by the first kind would, with `OnCredentialsChanged` alone, stay anonymous **until the job ends** — R5's "immediately" not covered. So both closures call the same `reauthenticateTwitchChats`, which already filters on platform and is nil-safe.

**Covering both costs nothing, and the double fire is bounded.** The broadcast is idempotent in the way that matters: `Reauthenticate` resets three already-reset latches and cancels a session. A repair that fires BOTH edges in one pass (a swap that also restores auth) runs the broadcast twice, microseconds apart on the same goroutine, and the worst case is that the second cancel lands on the session the first one just started — one extra reconnect, which by Task 4's design spends no reconnect budget and no backoff. That is a smaller cost than a dedupe mechanism to prevent it, and the plan does not build one.

**The broadcast must never park its caller**, and does not. Both closures can run on an HTTP handler goroutine — `POST /api/cookies/recheck` (`routes/cookies.go:262`) and the dashboard browser refresh (`:399`) both call `CheckNow` synchronously, and `refresh` invokes these callbacks inline. `reauthenticateAll` snapshots under the registry lock and calls outside it (Task 5), and `Reauthenticate` writes three atomics, takes `cd.mu` briefly and cancels a context (Task 4). No I/O, no wait.

**3. Task 8 branch A, if taken, wires `twService.OnAnonymousPlayback` here too.** That line is written in Task 8, not here.

**Files:**
- Modify: `cmd/moombox/services.go` — the mark seam, beside `dlWorker.CurrentCredentialIdentity` (`:866-877`)
- Modify: `cmd/moombox/monitor_callbacks.go` — a small helper near `sweepShouldResume` (`:66-76`), and BOTH credential-repair closures, extracted out of `wireMonitorCallbacks` into one testable method: `OnAuthRecovered` (`:648-665`) and `OnCredentialsChanged` (`:678-706`)
- Modify: `internal/tui/app_update.go:834` — one condition in `cookieRecheckFeedback`
- Modify: `web/public/modules/utils.js:527-546` — one condition in `cookieIndicatorState`, plus the doc comment above it (`:515-521`) that states the old rule
- Test: `cmd/moombox/monitor_callbacks_twitch_reauth_test.go` (create)
- Test: `internal/tui/cookie_recheck_reason_test.go` (amend the subtest that pins the OLD rule — see Step 4a)
- Test: `internal/web/routes/cookies_indicator_test.go` (amend the subtest that pins the OLD rule — see Step 4b)

**Interfaces:**
- Consumes: `RefreshService.OnAuthRecovered func(platform string)` (pre-existing, `refresh.go:520-523`; fires on a not-authenticated → authenticated transition, which for Twitch since Task 2 means `twEffective`, so a standing mark cannot produce a phantom recovery) — the SECOND repair edge, and the only one a transient refusal produces. Also `RefreshService.OnCredentialsChanged func(platform, identity string)` **firing for `"twitch"` (Task 3)** — the whole reason Step 4 exists; without Task 3 the closure Step 4 rewrites is never entered for Twitch and the broadcast is dead code. Also `(*cookies.RefreshService).NoteTwitchAuthLoss(reason string)` (Task 2); `(*worker.DownloadWorker).SetOnTwitchAuthLoss(fn func(reason string))` (Task 6); `(*worker.DownloadWorker).ReauthenticateTwitchChats() int` (Task 5); `resumeCookieParkedJobs(db, log, platform, currentIdentity string) int` (`monitor_callbacks.go:81-106`)
- Consumes (Steps 4a/4b): `cookieRecheckResultMsg{YouTube, Twitch cookies.RefreshVerdict; YouTubeReason, TwitchReason, LastError string}` (`internal/tui/app_actions.go:28-48` produces it from `OnRecheckCookies`, unchanged by this task) and `routes.TwitchAuthStatusPayload`'s `twitchError` / `CookieStatusPayload`'s `youtubeError` keys (`internal/web/routes/cookies.go`, unchanged by this task)
- Produces: `func reauthenticateTwitchChats(platform string, broadcast func() int) int` — no later task consumes it; it exists so the platform gate is drivable. Steps 4a/4b produce no new symbol: both are one-condition widenings of existing functions.

- [ ] **Step 1: Write the failing tests**

Create `cmd/moombox/monitor_callbacks_twitch_reauth_test.go`:

```go
package main

import "testing"

// Arc 10 R5's last hop. OnCredentialsChanged now fires for both platforms, and
// only one of them has live chat sessions to reach.

// TestOnlyATwitchCredentialChangeBroadcasts is the platform gate.
//
// The mutation: dropping the gate, so a YouTube cookie rotation drops and
// re-establishes every live Twitch IRC session. That is not merely wasteful —
// each reconnect re-runs the handshake and, on a marathon stream, a YouTube
// refresh cadence would keep tearing chat down for no reason at all.
func TestOnlyATwitchCredentialChangeBroadcasts(t *testing.T) {
	calls := 0
	broadcast := func() int { calls++; return 3 }

	if got := reauthenticateTwitchChats("youtube", broadcast); got != 0 {
		t.Errorf("a youtube credential change broadcast to %d sessions, want 0", got)
	}
	if calls != 0 {
		t.Errorf("the broadcaster was called %d times for youtube, want 0", calls)
	}

	if got := reauthenticateTwitchChats("twitch", broadcast); got != 3 {
		t.Errorf("a twitch credential change broadcast to %d sessions, want 3", got)
	}
	if calls != 1 {
		t.Errorf("the broadcaster was called %d times for twitch, want 1", calls)
	}
}

// TestBroadcastWithNoWorkerIsSafe. wireMonitorCallbacks runs during startup and
// a test harness may build a runState without a worker; a nil deref at the
// moment an operator repairs their credentials is the worst time for one.
//
// The mutation: dropping the nil guard.
func TestBroadcastWithNoWorkerIsSafe(t *testing.T) {
	if got := reauthenticateTwitchChats("twitch", nil); got != 0 {
		t.Errorf("a broadcast with no worker returned %d, want 0", got)
	}
}

// TestUnknownPlatformDoesNotBroadcast. The callback's platform argument comes
// from RefreshService and is "youtube" or "twitch" today; an equality test
// rather than a not-youtube test is what keeps a third platform from silently
// inheriting Twitch behaviour.
//
// The mutation: `if platform != "youtube"` instead of `if platform != "twitch"`.
func TestUnknownPlatformDoesNotBroadcast(t *testing.T) {
	calls := 0
	if got := reauthenticateTwitchChats("kick", func() int { calls++; return 2 }); got != 0 || calls != 0 {
		t.Errorf("an unknown platform broadcast to %d sessions with %d calls, want 0 and 0", got, calls)
	}
}

// repairCallbackState builds the minimum runState wireCredentialRepairCallbacks
// needs, and returns it with a counter the broadcast increments.
//
// A real RefreshService over an empty jar (no network is reached — nothing
// calls a check) and a real empty database, following
// monitor_callbacks_recovery_test.go's fixture. The empty DB is load-bearing:
// resumeCookieParkedJobs finds no jobs, so `resumed` stays 0, so notifyMgr is
// never touched and may stay nil.
func repairCallbackState(t *testing.T) (*runState, *int) {
	t.Helper()
	log, err := logger.New(filepath.Join(t.TempDir(), "repair.log"), "error", 4096, 1)
	if err != nil {
		t.Fatalf("logger.New: %v", err)
	}
	log.SuppressStdout()
	t.Cleanup(log.Close)

	db, err := database.Open(filepath.Join(t.TempDir(), "repair.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	s := &runState{
		log:           log,
		db:            db,
		cookieRefresh: cookies.NewRefreshService(cookies.NewCookieJar(), time.Hour, log),
	}
	calls := 0
	s.wireCredentialRepairCallbacks(func() int { calls++; return 1 })
	return s, &calls
}

// TestBothRepairEdgesBroadcastForTwitch is the Task 3 review's finding 1.
//
// OnAuthRecovered is a repair edge OnCredentialsChanged does not cover: a
// transient Twitch refusal, or an operator restoring the exact pair they had
// before, brings validate back to authenticated with the credential
// fingerprint UNCHANGED, so shouldObserveCredentials returns false and
// OnCredentialsChanged never fires. Wired to that callback alone, a chat
// session that went anonymous on the refusal stays anonymous until the job
// ends — R5's "immediately" simply not covered for that path.
//
// THE MUTATION: dropping `reauth(platform)` from the OnAuthRecovered closure
// (which is how the plan's first draft had it). The first subtest then reports
// 0 broadcasts. Dropping it from OnCredentialsChanged fails the second.
func TestBothRepairEdgesBroadcastForTwitch(t *testing.T) {
	t.Run("auth recovered", func(t *testing.T) {
		s, calls := repairCallbackState(t)
		s.cookieRefresh.OnAuthRecovered("twitch")
		if *calls != 1 {
			t.Errorf("OnAuthRecovered(\"twitch\") broadcast %d times, want 1 — a transient refusal that heals produces no OnCredentialsChanged, so this is the only edge covering it", *calls)
		}
	})

	t.Run("credentials changed", func(t *testing.T) {
		s, calls := repairCallbackState(t)
		s.cookieRefresh.OnCredentialsChanged("twitch", "an-opaque-identity-token")
		if *calls != 1 {
			t.Errorf("OnCredentialsChanged(\"twitch\") broadcast %d times, want 1", *calls)
		}
	})
}

// TestAYouTubeRepairDoesNotBroadcast: the platform gate, driven through the
// REGISTERED callbacks rather than through the helper.
//
// A YouTube cookie rotation is routine and fires both edges on its own
// cadence. Broadcasting there would drop and re-establish every live Twitch
// IRC session for a credential that has nothing to do with them — on a
// marathon stream, repeatedly.
//
// THE MUTATION: calling `broadcast()` from `reauth` without the platform
// filter, or filtering on `platform != "youtube"`.
func TestAYouTubeRepairDoesNotBroadcast(t *testing.T) {
	s, calls := repairCallbackState(t)

	s.cookieRefresh.OnAuthRecovered("youtube")
	s.cookieRefresh.OnCredentialsChanged("youtube", "an-opaque-identity-token")

	if *calls != 0 {
		t.Errorf("a YouTube repair broadcast to Twitch chat sessions %d times, want 0", *calls)
	}
}
```

That file's imports are `"context"` and `"testing"` plus, for the fixture, `"path/filepath"`, `"time"`, `"github.com/vampiricwulf/Moombox/internal/cookies"`, `"github.com/vampiricwulf/Moombox/internal/database"` and `"github.com/vampiricwulf/Moombox/internal/logger"`.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./cmd/moombox/ -run 'TestOnlyATwitchCredentialChange|TestBroadcastWithNoWorker|TestUnknownPlatformDoesNot|TestBothRepairEdges|TestAYouTubeRepair' -v`
Expected: compile failure — `undefined: reauthenticateTwitchChats`, `s.wireCredentialRepairCallbacks undefined`.

- [ ] **Step 3: Add the helper**

In `cmd/moombox/monitor_callbacks.go`, immediately after `resumeCookieParkedJobs` (`:106`):

```go
// reauthenticateTwitchChats broadcasts a credential change to every live
// Twitch chat session and reports how many were told.
//
// Split out of the OnCredentialsChanged closure for the same reason
// sweepShouldResume was split out of it: the closure needs a whole runState to
// build, and the decision inside it — WHICH platform gets a broadcast — is
// worth driving directly.
//
// The gate is an equality test, not "anything but youtube". A third platform
// added later must not silently inherit Twitch's reconnect behaviour, and
// dropping the gate entirely would let a YouTube cookie rotation tear down
// every live Twitch chat session on its own cadence.
//
// broadcast is DownloadWorker.ReauthenticateTwitchChats in production, taken
// as a func so a nil worker degrades to "nothing to tell". It returns a COUNT
// and nothing else: the caller logs a number, never a channel or an account.
func reauthenticateTwitchChats(platform string, broadcast func() int) int {
	if platform != "twitch" || broadcast == nil {
		return 0
	}
	return broadcast()
}
```

- [ ] **Step 4: Wire the broadcast into BOTH repair edges, and adapt the notice**

In `cmd/moombox/monitor_callbacks.go`, lift the two credential-repair closures out of `wireMonitorCallbacks` (`OnAuthRecovered` at `:648-665`, `OnCredentialsChanged` at `:678-706`) into one method beside them. The lift is what makes the wiring testable: `wireMonitorCallbacks` takes no arguments and touches the monitors, the DB, the notifier and half of `runState`, so nothing can drive it in a test — and "the hook is registered" is exactly the property this task must not get wrong twice.

Replace both closures with a single call in `wireMonitorCallbacks`, where the first of them stood:

```go
	// Both credential-repair edges, wired together because they mean the same
	// thing to a live chat session and different things to everything else.
	// The broadcast is injected rather than read off s.dlWorker inside, so a
	// test can count it; the method value is safe on a nil worker
	// (ReauthenticateTwitchChats is nil-receiver-guarded - Task 5).
	s.wireCredentialRepairCallbacks(s.dlWorker.ReauthenticateTwitchChats)
```

and add the method after `resumeCookieParkedJobs`:

```go
// wireCredentialRepairCallbacks installs the two RefreshService callbacks that
// fire when a platform's credentials become usable again.
//
// TWO EDGES, and neither implies the other:
//
//   - OnAuthRecovered - validate went not-authenticated to authenticated. A
//     transient Twitch-side refusal, or an operator restoring the EXACT pair
//     they had before, recovers auth with the credential fingerprint
//     unchanged, so shouldObserveCredentials returns false and the other
//     callback never fires.
//   - OnCredentialsChanged - the fingerprint moved. A same-account rotation,
//     or a swap to an account that does not validate, moves it with no auth
//     transition at all.
//
// A live Twitch chat session that went anonymous on a refusal has to hear
// about BOTH, or a transient failure strands it in anonymous capture for the
// rest of the job (Arc 10 R5, "immediately"). Everything else about the two
// stays as it was: the recovery sweep passes no identity and so holds back
// membership parks, the credential sweep passes one and so can move them.
//
// broadcast is DownloadWorker.ReauthenticateTwitchChats in production, taken
// as a func so this method can be driven directly - wireMonitorCallbacks
// cannot be.
func (s *runState) wireCredentialRepairCallbacks(broadcast func() int) {
	// reauth is the half both edges share. reauthenticateTwitchChats filters
	// the platform and is nil-safe, so this is safe to call from either.
	//
	// LIVE SESSIONS FIRST, before the sweep below. A job the sweep resumes
	// starts a fresh downloader that reads the new credentials anyway; a job
	// already CAPTURING has no other way to learn about them, and that capture
	// is the one the operator is watching right now.
	//
	// Only the COUNT is logged. On the OnCredentialsChanged edge an identity is
	// in scope, and it is an opaque equality token (see
	// CookieJar.TwitchIdentity) that must never reach a log line.
	reauth := func(platform string) {
		if n := reauthenticateTwitchChats(platform, broadcast); n > 0 {
			s.log.Info("twitch credentials usable again — re-authenticating live chat sessions",
				"platform", platform, "sessions", n)
		}
	}

	// When a platform transitions from not-authenticated to authenticated,
	// sweep the jobs parked in StatusCookies on that platform back to Upcoming
	// so they get re-probed without manual intervention. Closes audit
	// decision #23 (worker.md Q3).
	//
	// "the jobs", not "every job": sweepShouldResume holds back the
	// membership-parked ones, whose session was already authenticated when
	// they failed and which this transition therefore cannot fix.
	s.cookieRefresh.OnAuthRecovered = func(platform string) {
		reauth(platform)
		resumed := resumeCookieParkedJobs(s.db, s.log, platform, "")
		if resumed > 0 {
			s.log.Info("auth recovered — resumed COOKIES? jobs", "platform", platform, "count", resumed)
			// Event "auth" pairs with the worker's "Authentication Required"
			// emit — an empty Event would bypass every target's allowlist
			// (unfilterable) since the filter only applies when Event != "".
			s.notifyMgr.Send("Authentication Recovered",
				fmt.Sprintf("Resumed %d job(s) waiting on %s cookies", resumed, platform),
				notifications.TypeInfo,
				[]notifications.Field{
					{Name: "Platform", Value: platform, Inline: true},
					{Name: "Jobs", Value: fmt.Sprintf("%d", resumed), Inline: true},
				},
				notifications.SendOptions{Event: "auth"},
			)
		}
	}

	// Whenever the signed-in account is (re-)observed, re-evaluate the parked
	// jobs against it. For a membership park this is the only thing that can
	// help — such a job parked while auth was perfectly healthy, so it is
	// invisible to OnAuthRecovered above — and it resumes only if the account
	// is genuinely a different one from the one that refused it.
	//
	// Dead-cookie parks are eligible here as well. In the common case
	// OnAuthRecovered already took them (a swap that also restores auth fires
	// both), and resumeCookieParkedJobs is idempotent, so whichever runs
	// second simply finds nothing left. Being permissive costs nothing and
	// covers the swap-while-healthy case for them too.
	s.cookieRefresh.OnCredentialsChanged = func(platform, identity string) {
		reauth(platform)
		resumed := resumeCookieParkedJobs(s.db, s.log, platform, identity)
		if resumed > 0 {
			s.log.Info("account identity observed — resumed COOKIES? jobs", "platform", platform, "count", resumed)
			// States no cause, for the same reason the "Cookie Auto-Refresh
			// Ineffective" notification above states none. This fires on the
			// first authenticated observation of EVERY process, not only on a
			// real account change: an operator who fixed their cookies while
			// Moombox was stopped gets their jobs resumed here, and telling
			// them "a different account was supplied" would be flatly false.
			// A notification is more visible than a log line, so it should
			// assert less than the log line, not more — report what happened
			// (jobs resumed) and leave the cause to the log.
			//
			// "the saved credentials", not "the signed-in account": since Arc
			// 10 this fires for Twitch too, whose credential is a bearer token
			// and a login name rather than a Google account, and the old
			// wording would have been simply wrong there.
			//
			// Same "auth" event as the recovery notification above, for the
			// same reason: an empty Event bypasses every target's allowlist.
			s.notifyMgr.Send("Parked Jobs Re-evaluated",
				fmt.Sprintf("Resumed %d job(s) parked on %s credentials after re-checking the saved credentials", resumed, platform),
				notifications.TypeInfo,
				[]notifications.Field{
					{Name: "Platform", Value: platform, Inline: true},
					{Name: "Jobs", Value: fmt.Sprintf("%d", resumed), Inline: true},
				},
				notifications.SendOptions{Event: "auth"},
			)
		}
	}
}
```

Both closure bodies below `reauth(platform)` are the EXISTING code, moved verbatim except for the one notification sentence the comment marks. Do not rewrite them while relocating: a diff showing anything else changed inside those two blocks is a mistake.

- [ ] **Step 4a: TUI — the `R C` result line renders a reason whatever the verdict**

`internal/tui/app_update.go`, in `cookieRecheckFeedback`'s `consider` closure (`:834`). Current condition:

```go
		if verdict == cookies.RefreshUnknown && reason != "" {
			reasons = append(reasons, label+": "+reason)
		}
```

Replace with:

```go
		// Gated on the STRING, not on the verdict, since Arc 10.
		//
		// It was gated on RefreshUnknown because every producer at the time
		// left the reason empty on a conclusive verdict — a cause attached to
		// "OK" or "not authenticated" reads as an explanation for a conclusion
		// that has none. NoteTwitchAuthLoss is the first producer that writes
		// RefreshFailed WITH a reason, and it is precisely the reason the
		// operator needs: which of the four chat-downgrade routes broke. Under
		// the old gate they pressed R C and read "Twitch not authenticated"
		// with no way to tell a missing login cookie from a refused one.
		//
		// RefreshOK still carries nothing, and by construction rather than by
		// trust: verdictFromCheck returns OK only when the error is nil, and
		// the reason string IS that error. Testing the string rather than the
		// verdict is what makes that a property of the data instead of a
		// promise from the caller.
		if reason != "" {
			reasons = append(reasons, label+": "+reason)
		}
```

The producer needs no change: `recheckCookiesCmd` (`internal/tui/app_actions.go:28-48`) already carries both reason strings off `OnRecheckCookies` into `cookieRecheckResultMsg`, and `tui_wiring.go` already fills them from `GetStatus()`. Only the render gate was narrow.

**One existing subtest pins the OLD rule and must be rewritten, not deleted.** `internal/tui/cookie_recheck_reason_test.go`, subtest `"a conclusive verdict renders the shared sentence and nothing else"` (`:78-95`), loops `RefreshOK` and `RefreshFailed` with a reason supplied and asserts the sentence alone. Split it:

```go
	t.Run("an OK verdict renders the shared sentence and nothing else", func(t *testing.T) {
		// Unchanged intent. An authenticated check has no cause to give, and
		// verdictFromCheck cannot produce OK beside a non-empty reason — so
		// this row is a pin on the PRODUCER's invariant, exercised through the
		// renderer, and it must keep passing after the gate widened.
		got := recheckFeedback(t, 200, true, false, cookieRecheckResultMsg{
			YouTube:       cookies.RefreshOK,
			YouTubeReason: "",
		})
		want := cookies.RecheckReport(
			cookies.RecheckedPlatform{Label: "YouTube", Verdict: cookies.RefreshOK},
		)
		if got != want {
			t.Errorf("feedback = %q, want the shared sentence alone %q", got, want)
		}
	})

	t.Run("a conclusive REFUSAL names its reason", func(t *testing.T) {
		// Arc 10 reversed this row. The mark writes RefreshFailed with one of
		// four fixed sentences, and it is the only thing that says WHICH route
		// broke — "the cookie file has a Twitch auth-token but no login cookie
		// beside it" versus "Twitch refused the saved login" are different
		// remedies. Withholding it left the operator with "not authenticated"
		// and no next step.
		//
		// THE MUTATION: narrowing the gate back to
		// `verdict == cookies.RefreshUnknown && reason != ""` in
		// cookieRecheckFeedback (app_update.go). This subtest then fails on the
		// Contains check below — the sentence comes back as the bare
		// RecheckReport with no parenthetical.
		const twReasonMark = "The cookie file has a Twitch auth-token but no login cookie beside it."
		got := recheckFeedback(t, 200, false, true, cookieRecheckResultMsg{
			Twitch:       cookies.RefreshFailed,
			TwitchReason: twReasonMark,
		})
		if !strings.Contains(got, twReasonMark) {
			t.Errorf("feedback = %q, want it to name %q — a conclusive refusal that knows WHY must say so", got, twReasonMark)
		}
		if !strings.Contains(got, "Twitch") {
			t.Errorf("feedback = %q, want the platform label kept beside the reason", got)
		}
	})
```

(Add `"strings"` to that file's imports if it is not already there.) Every other subtest in the file is untouched: the inconclusive rows still pass under the widened gate, and the `"no reason supplied leaves the sentence byte-identical"` row is exactly the guard that keeps the widening additive.

- [ ] **Step 4b: Web — the cookie indicator renders a reason whatever the verdict**

`web/public/modules/utils.js`, in `cookieIndicatorState` (`:527-546`). The reason is currently appended inside the `unknown` arm only, and the final arm returns a bare sentence:

```js
  if (status?.verification === "unknown") {
    const reason = status?.[meta.errorKey];
    return {
      className: "indicator-warn",
      title: `${meta.name}: Cookies saved — Moombox could not establish whether they work`
        + (reason ? ` (${reason})` : ""),
    };
  }
  return { className: "indicator-error", title: `${meta.name}: Not authenticated` };
```

Replace both arms with:

```js
  const reason = status?.[meta.errorKey];
  const cause = reason ? ` (${reason})` : "";
  if (status?.verification === "unknown") {
    return {
      className: "indicator-warn",
      title: `${meta.name}: Cookies saved — Moombox could not establish whether they work${cause}`,
    };
  }
  return { className: "indicator-error", title: `${meta.name}: Not authenticated${cause}` };
```

and correct the doc comment above the function (`:515-521`), which states the rule being changed:

```js
 * The reason line is `youtubeError` / `twitchError` off the same payload, and
 * it is appended to whichever arm has one. Until Arc 10 that was the
 * inconclusive arm alone, because every producer left the field empty on a
 * conclusive verdict — but `NoteTwitchAuthLoss` writes `failed` WITH a fixed
 * sentence naming which of the four Twitch chat-downgrade routes broke, and
 * that sentence is the only thing distinguishing "no login cookie" from
 * "Twitch refused the login". An `ok` verdict still shows nothing, by
 * construction: the server derives the verdict from the same error the string
 * carries, so `ok` and a non-empty reason cannot co-occur. An older binary
 * sends no such key at all and both titles degrade to exactly today's
 * sentence.
```

**`go build` is required for this to reach a browser** — `web/public/` is `go:embed`-ed (`web/embed.go`), so an un-rebuilt binary serves the old module and the change is invisible.

**There IS a JS test harness and this is testable — no field check needed.** `internal/web/routes/cookies_indicator_test.go` runs the shipped `utils.js` in goja (`utilsVM`, `cookies_setup_utilsvm_test.go:48`) and calls `cookieIndicatorState` through `indicatorState(t, vm, platform, status, relogin, parked...)`. One existing subtest pins the OLD rule: `TestIndicatorTitleNamesWhyACheckCouldNotConclude`'s `"a conclusive verdict carries no cause"` (`:423-433`). Replace that subtest with:

```go
	t.Run("a conclusive REFUSAL names its cause", func(t *testing.T) {
		// Arc 10 reversed this row, and the paragraph it replaces explained
		// why the old rule was right at the time: no producer wrote a reason
		// beside a conclusive verdict. NoteTwitchAuthLoss does, and its four
		// sentences are the only thing that says which chat-downgrade route
		// broke.
		//
		// THE MUTATION: restoring the reason to the `unknown` arm only in
		// cookieIndicatorState (utils.js). This subtest then fails on the
		// Contains check — the title comes back as the bare "Not
		// authenticated".
		const markReason = "The cookie file has a Twitch auth-token but no login cookie beside it."
		_, title := indicatorState(t, vm, "twitch", map[string]any{
			"found": true, "authenticated": false, "verification": "failed",
			"twitchError": markReason,
		}, false)
		if !strings.Contains(title, "Not authenticated") {
			t.Errorf("title = %q, want the conclusive sentence kept intact — the cause is appended to it, never woven into it", title)
		}
		if !strings.Contains(title, markReason) {
			t.Errorf("title = %q, want it to name %q. Without it every dead-credential state renders identically and none says what to fix", title, markReason)
		}
	})

	t.Run("an OK verdict carries no cause", func(t *testing.T) {
		// The invariant the widened gate now leans on, pinned at the renderer:
		// an authenticated badge must never sprout a parenthetical, and the
		// server cannot produce one (verdictFromCheck returns ok only for a
		// nil error, and the reason string is that error).
		_, title := indicatorState(t, vm, "youtube", map[string]any{
			"found": true, "authenticated": true, "verification": "ok",
			"youtubeError": "",
		}, false)
		if strings.Contains(title, "(") {
			t.Errorf("title = %q — an authenticated badge must carry no parenthetical", title)
		}
	})
```

The `"an older binary sends no reason"` subtest below it is the additive guard and stays exactly as written: with no key the `cause` string is empty and both titles are byte-identical to today's.

- [ ] **Step 5: Wire the mark seam**

In `cmd/moombox/services.go`, immediately after the `dlWorker.CurrentCredentialIdentity` block (`:877`):

```go
	// Arc 10 R1: a Twitch chat auth downgrade marks the PLATFORM, beside the
	// per-job notification the worker already sends.
	//
	// ON ITS OWN GOROUTINE, with the inline recover every goroutine in this
	// project carries. The caller is internal/twitch's IRC session goroutine
	// with its read loop parked behind this call (OnAuthDowngrade's contract
	// says it must not block), and NoteTwitchAuthLoss can reach
	// handleRecoveryNeeded's auto_enabled=false arm, which sends the
	// "Cookie Re-Authentication Required" webhook SYNCHRONOUSLY. A chat read
	// loop must not be held open behind an HTTP POST to Discord.
	//
	// Fire-and-forget is correct rather than convenient: the mark is
	// idempotent — writing the same reason twice is the same status — and the
	// downloader latches its report once per job anyway, so there is nothing
	// to sequence and nothing to wait for. The reason is a fixed vocabulary
	// token; nothing read from the jar or the wire passes through here.
	dlWorker.SetOnTwitchAuthLoss(func(reason string) {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Error("panic marking twitch auth loss", "panic", fmt.Sprint(r))
				}
			}()
			s.cookieRefresh.NoteTwitchAuthLoss(reason)
		}()
	})
```

Placement verified: `s.cookieRefresh = cookieRefresh` is at `services.go:713` and the `CurrentCredentialIdentity` block at `:868`, so this lands well after the assignment.

While here, correct the neighbouring comment that Task 1 falsified. `CurrentCredentialIdentity`'s YouTube gate (`services.go:869-874`) currently reasons *"Only YouTube produces a membership park, and only YouTube has a stable account fingerprint — see cookies.RefreshService's prevYouTubeIdentity."* The second clause is now misleading. Replace that comment body with:

```go
		if platform != "youtube" {
			// Only YouTube produces a membership park, which is the only thing
			// this fingerprint is recorded FOR. Twitch has a fingerprint since
			// Arc 10 (CookieJar.TwitchIdentity), but it identifies a credential
			// PAIR rather than an account, and no Twitch failure parks a job on
			// an account question — so there is nothing here for it to record.
			return ""
		}
```

- [ ] **Step 6: Run the tests and confirm they pass**

Run: `go test ./cmd/moombox/ -run 'TestOnlyATwitchCredentialChange|TestBroadcastWithNoWorker|TestUnknownPlatformDoesNot|TestBothRepairEdges|TestAYouTubeRepair' -v`
Expected: PASS for all five (`TestBothRepairEdgesBroadcastForTwitch` has two subtests).

Run: `go test ./cmd/moombox/ -count=1`
Expected: `ok  github.com/vampiricwulf/Moombox/cmd/moombox`. If a test asserts the old "after re-checking the signed-in account" wording, update it to the new sentence — that is the intended change, not a regression.

Then the two renderer packages, which Steps 4a and 4b changed:

Run: `go test ./internal/tui/ -run TestRecheckFeedback -v -count=1`
Expected: PASS, including the two subtests that replaced `"a conclusive verdict renders the shared sentence and nothing else"`.

Run: `go test ./internal/web/routes/ -run TestIndicatorTitle -v -count=1`
Expected: PASS, including the two subtests that replaced `"a conclusive verdict carries no cause"`.

Run: `go test ./internal/tui/ ./internal/web/routes/ -count=1`
Expected: both `ok`. A failure in `internal/web/routes` that is NOT one of the two named subtests means another test pins the old rule too — find it, and change it only if it is asserting the gate rather than the wording around it.

- [ ] **Step 7: Run the gates**

```bash
go build ./...
go vet ./...
GOOS=linux go build ./...
gofmt -l internal/ cmd/
go test -count=1 ./...
```
Expected: build and vet clean, `gofmt -l` prints nothing, 27 packages ok / 0 fail.

- [ ] **Step 8: Commit**

```bash
git add cmd/moombox/services.go cmd/moombox/monitor_callbacks.go cmd/moombox/monitor_callbacks_twitch_reauth_test.go internal/tui/app_update.go internal/tui/cookie_recheck_reason_test.go web/public/modules/utils.js internal/web/routes/cookies_indicator_test.go
git commit -m "$(cat <<'EOF'
feat(cmd): a Twitch downgrade reaches the platform status, the operator, and live chat

Arc 10's two ends close here, in the only package that holds the refresh
service, the worker and the jar at once.

The mark runs on its own goroutine with the inline recover: its caller is the
IRC session goroutine with the read loop parked behind it, and the mark can
reach the auto_enabled=false arm, which posts a webhook synchronously.

BOTH repair edges broadcast to every live chat session, before the parked-job
sweep — a resumed job builds a fresh downloader anyway, while a running
capture has no other way to learn. Two edges because neither implies the
other: OnCredentialsChanged fires when the credential FINGERPRINT moves, and
OnAuthRecovered when validate goes not-authenticated to authenticated, which a
transient refusal does with the fingerprint unchanged. Wired to the first
alone, a chat session that went anonymous on a transient refusal stayed
anonymous until the job ended.

The two closures moved into wireCredentialRepairCallbacks so the registration
itself is testable — wireMonitorCallbacks takes no arguments and touches half
of runState, so nothing could drive it. Their bodies are unchanged apart from
the one notification sentence. The gate is an equality test so a third
platform cannot inherit it; only the count is logged, never the identity.

The existing sweep is adapted, not filtered: sweepShouldResume already gates
on job.Platform and every Twitch COOKIES? job carries ParkReasonAuth, so the
only wrong thing was the notice saying "the signed-in account" about a bearer
token.

And both per-request reason renderers now show a reason whenever there is
one. They gated on RefreshUnknown because no producer had ever written a
reason beside a conclusive verdict; the mark writes RefreshFailed with one of
four fixed sentences, and that sentence is the only thing distinguishing "no
login cookie" from "Twitch refused the login". Without this the operator
pressed R C and read "not authenticated" with no next step. An OK verdict
still shows nothing — verdictFromCheck derives it from the same error the
string carries, so the two cannot co-occur — which is why the new gate tests
the string rather than trusting the verdict.

The push-driven bars are untouched: authStatusChanged still excludes the two
strings, so no OnAuthChange-driven surface renders them.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

---

### Task 7a: Every credential write reaches the fingerprint comparison

Task 3's table enumerates every gesture that can put new credentials on disk. **Four do not reach `CheckNow`**, so the fingerprint is never compared for them, the Twitch auth mark is never cleared, and no live chat session is told — until the 30-minute ticker. R4 lists these as sources of change and R5 says "immediately"; a 30-minute floor is neither. This task closes all four.

**Why `CheckNow` and not a lighter method.** A reload-only-and-compare method was considered and rejected. Every one of the four callers has just waited on a headless browser (up to two minutes at three of them, a full setup wizard at the fourth), so two 15-second validate round-trips are not the cost that matters there — and a second entry point into `refresh`'s status block would be a second mechanism containing the first, with its own single-flight interaction to reason about. `CheckNow` is what the three working sites already call; a fourth spelling would make the invariant harder to state, not easier.

**Why one helper.** Five of the seven `cmd/moombox`-side sites want the same five lines: call `CheckNow`, and if it reports that another pass was already in flight, say so at Info because the caller has just rewritten the file the in-flight pass already read. Writing that five times is five chances to drop the Info line. The two `internal/web/routes` sites keep bare calls — that package has no operational logger and says so at `routes/cookies.go:390-398`.

**Every line number below was verified at `main` before Task 7 ran, and Task 7 edits three of these files first.** Re-derive by SYMBOL — `resumeCookieParkedJobs`, `runCookieRecovery`'s `switch result.Verdict(platform)`, `dlWorker.OnCookieRefreshNeeded`, `app.OnForceRefreshCookies`, the setup-finish closure, the `auto-setup/finish` handler, `StartPeriodicRefresh`'s tick tail — not by number.

**Files:**
- Modify: `cmd/moombox/monitor_callbacks.go` — the new helper after `resumeCookieParkedJobs` (`:81-106`), and the `CheckNow` hoist in `runCookieRecovery` (`:336-361`)
- Modify: `cmd/moombox/services.go` — `OnCookieRefreshNeeded` (`:879-913`), and the `OnPassCompleted` wiring beside the other `autoCookieSvc` injections (`:848-862`)
- Modify: `cmd/moombox/tui_wiring.go` — `R F`'s re-check (`:435`) and the setup-wizard finish twin (`:483-491`)
- Modify: `internal/web/routes/cookies.go` — the setup-finish handler's success path (`:504`)
- Modify: `internal/cookies/autocookies.go` — `OnPassCompleted` field, `notePassCompleted`, and the periodic tick's tail (`:2896-2910`)
- Test: `cmd/moombox/monitor_callbacks_recheck_test.go` (create), `internal/cookies/autocookies_pass_completed_test.go` (create)

**Interfaces:**
- Consumes: `(*cookies.RefreshService).CheckNow(ctx context.Context) bool` (`refresh.go:805-807`); `cookies.RefreshResult.Ran`
- Produces:
  - `func recheckAfterCookieWrite(ctx context.Context, checkNow func(context.Context) bool, log interface{...}, gesture string, args ...any) bool` (`cmd/moombox`)
  - `AutoCookieService.OnPassCompleted func()` and `func (s *AutoCookieService) notePassCompleted()` (`internal/cookies`)

- [ ] **Step 1: Write the failing tests for the helper**

Create `cmd/moombox/monitor_callbacks_recheck_test.go`:

```go
package main

import (
	"context"
	"strings"
	"testing"
)

// Arc 10 Task 7a. The in-process re-check that must follow every pass which
// may have rewritten cookies.txt, because refresh's status block is the only
// place the Twitch credential fingerprint is compared and the auth mark
// cleared.

// recheckLogger records Info messages and their args, so the skipped line can
// be asserted on. It records nothing else: the other three levels are
// discarded, and no cookie value can reach any of them anyway.
type recheckLogger struct {
	infos []string
	args  [][]any
}

func (l *recheckLogger) Debug(string, ...any) {}
func (l *recheckLogger) Warn(string, ...any)  {}
func (l *recheckLogger) Error(string, ...any) {}
func (l *recheckLogger) Info(msg string, args ...any) {
	l.infos = append(l.infos, msg)
	l.args = append(l.args, args)
}

// TestRecheckAfterCookieWriteRunsThePassAndStaysQuiet.
//
// The mutation: swapping the return polarity, so a pass that RAN logs the
// "status may lag" line. That line tells an operator their badge is stale when
// it is not, which is the one thing worse than not logging at all.
func TestRecheckAfterCookieWriteRunsThePassAndStaysQuiet(t *testing.T) {
	var gotCtx context.Context
	calls := 0
	log := &recheckLogger{}
	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "carried")

	ran := recheckAfterCookieWrite(ctx, func(c context.Context) bool {
		calls++
		gotCtx = c
		return true
	}, log, "the browser refresh")

	if !ran {
		t.Error("recheckAfterCookieWrite reported no pass although CheckNow said one ran")
	}
	if calls != 1 {
		t.Errorf("CheckNow was called %d times, want exactly 1", calls)
	}
	if gotCtx == nil || gotCtx.Value(ctxKey{}) != "carried" {
		t.Error("the caller's context did not reach CheckNow — a cancelled request would not cancel the pass")
	}
	if len(log.infos) != 0 {
		t.Errorf("logged %q for a pass that ran, want silence", log.infos)
	}
}

// TestRecheckAfterCookieWriteSaysSoWhenSkipped.
//
// The mutation: dropping the Info line. The caller has just rewritten
// cookies.txt and the in-flight pass read the OLD file, so the badge stays
// stale until the next tick — and this line is the only evidence of why. It
// has been at Info rather than the service's own Debug since Arc 6, for
// exactly that reason.
func TestRecheckAfterCookieWriteSaysSoWhenSkipped(t *testing.T) {
	log := &recheckLogger{}

	ran := recheckAfterCookieWrite(context.Background(), func(context.Context) bool { return false },
		log, "recovery", "platform", "twitch")

	if ran {
		t.Error("recheckAfterCookieWrite reported a pass although CheckNow said none ran")
	}
	if len(log.infos) != 1 {
		t.Fatalf("logged %q, want exactly one line", log.infos)
	}
	if !strings.Contains(log.infos[0], "recovery") {
		t.Errorf("the skipped line does not name the gesture: %q", log.infos[0])
	}
	if !strings.Contains(log.infos[0], "status may lag") {
		t.Errorf("the skipped line does not say what it costs: %q", log.infos[0])
	}
	if len(log.args[0]) != 2 || log.args[0][0] != "platform" || log.args[0][1] != "twitch" {
		t.Errorf("the caller's structured args were dropped: %v", log.args[0])
	}
}

// TestRecheckAfterCookieWriteWithNoServiceIsSafe. wireMonitorCallbacks and
// initServices both run during startup, and a harness may build a runState
// without a refresh service. A nil deref at the moment an operator's
// credentials are repaired is the worst time for one.
//
// The mutation: dropping the nil guard.
func TestRecheckAfterCookieWriteWithNoServiceIsSafe(t *testing.T) {
	log := &recheckLogger{}
	if ran := recheckAfterCookieWrite(context.Background(), nil, log, "recovery"); ran {
		t.Error("reported a pass with no refresh service wired")
	}
	if len(log.infos) != 0 {
		t.Errorf("logged %q with no refresh service wired — there was no stale badge to explain", log.infos)
	}
}
```

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test ./cmd/moombox/ -run TestRecheckAfterCookieWrite -v`
Expected: compile failure — `undefined: recheckAfterCookieWrite`.

- [ ] **Step 3: Write the helper**

In `cmd/moombox/monitor_callbacks.go`, immediately after `resumeCookieParkedJobs` (`:106`):

```go
// recheckAfterCookieWrite runs the in-process auth re-check that MUST follow
// any pass which may have rewritten cookies.txt, and reports whether a pass
// actually ran.
//
// Why every such gesture has to end here: refresh's status block is the only
// place the Twitch credential fingerprint is compared, the auth mark cleared
// and OnCredentialsChanged fired (Arc 10 R4), and that block runs only inside
// a refresh pass. A repaired cookie file that reaches no pass is invisible
// until the 30-minute ticker — which is precisely what "immediately apply the
// updated cookie" rules out. The full enumeration of sites, and which of them
// were missing this call before Arc 10, is in the plan's Task 3 table.
//
// CheckNow rather than something lighter: every caller has just waited on a
// headless browser or a whole setup wizard, so two validate round-trips are
// not the cost that matters, and a second entry point into the status block
// would be a second mechanism containing the first.
//
// The skipped case is Info, not the service's own Debug, and that split
// predates Arc 10: the caller has just rewritten the file, the in-flight pass
// read the OLD one, so the badge stays stale until the next tick and this is
// the only line that explains it. Nothing here retries or waits — the guard's
// contract is that a second caller does nothing.
//
// checkNow is RefreshService.CheckNow as a method value, taken as a func so a
// process with no refresh service degrades to "nothing to re-check". gesture
// names what just wrote, and args are the caller's own structured fields.
func recheckAfterCookieWrite(ctx context.Context, checkNow func(context.Context) bool, log interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}, gesture string, args ...any) bool {
	if checkNow == nil {
		return false
	}
	if checkNow(ctx) {
		return true
	}
	log.Info("auth re-check after "+gesture+" was skipped, a cookie refresh was already in flight — status may lag until the next refresh", args...)
	return false
}
```

- [ ] **Step 4: Run them and confirm they pass**

Run: `go test ./cmd/moombox/ -run TestRecheckAfterCookieWrite -v`
Expected: PASS for all three.

- [ ] **Step 5: Hoist the recovery re-check so all three verdict arms get it**

In `cmd/moombox/monitor_callbacks.go`, in `runCookieRecovery`: DELETE the `CheckNow` call and its Info line from the `case cookies.RefreshOK:` arm (`:358-361`), keep that arm's `s.log.Info("auto-cookie recovery succeeded", ...)`, and insert the hoisted call immediately BEFORE `switch result.Verdict(platform) {` (`:339`):

```go
	// The re-check, hoisted out of the RefreshOK arm so it covers every verdict
	// a pass that RAN can reach.
	//
	// It used to sit only under RefreshOK, on the reasoning that a successful
	// refresh is the one whose result the UI needs. That was half the truth.
	// A pass that ran and FAILED still rewrote cookies.txt — a browser that
	// produced a new-but-dead pair moves the credential fingerprint just as a
	// working one does — so without this the Twitch auth mark taken under the
	// OLD pair stands until the ticker, telling the operator to fix a login row
	// on a file that no longer has that problem.
	//
	// Gated on Ran, not on the verdict: a DECLINED pass (setup in progress, a
	// refresh already in flight, nothing configured, the service stopped) wrote
	// nothing at all, so there is nothing to re-read and the Info line below
	// would describe a staleness that does not exist.
	//
	// Placed after the err != nil block above, which returns: an errored pass
	// includes the S9 abort, which deliberately did not write.
	if result.Ran {
		recheckAfterCookieWrite(context.Background(), s.cookieRefresh.CheckNow, s.log, "recovery", "platform", platform)
	}

	switch result.Verdict(platform) {
```

The `RefreshOK` arm then reads:

```go
	case cookies.RefreshOK:
		s.log.Info("auto-cookie recovery succeeded", "platform", platform)
```

- [ ] **Step 6: Add the re-check to the worker's auth-failure refresh**

In `cmd/moombox/services.go`, in `dlWorker.OnCookieRefreshNeeded`, after `report := cookieRefreshReportFor(platform, result)` and its Warn, before `return report.ok`:

```go
		// Arc 10 R4/R5. This is the browser refresh a FAILING JOB triggers, and
		// it was the one credential-writing gesture with no re-check: the job
		// that asked gets its answer from `result`, but every OTHER live Twitch
		// job's chat session learned nothing until the 30-minute ticker
		// compared the fingerprint. That is the case the owner's "immediately
		// apply the updated cookie" is about.
		//
		// Background, not refreshCtx: that context is cancelled by the defer
		// two lines down as soon as this closure returns, and the re-check must
		// not be racing its own caller's teardown.
		if result.Ran {
			recheckAfterCookieWrite(context.Background(), s.cookieRefresh.CheckNow, log, "the job-triggered cookie refresh", "platform", platform)
		}
		return report.ok
```

- [ ] **Step 7: Migrate `R F` and add the TUI setup-finish re-check**

In `cmd/moombox/tui_wiring.go`, replace `R F`'s re-check (`:435-437`) with the helper. The emitted line is byte-identical — the point is one shape, not two:

```go
		recheckAfterCookieWrite(context.Background(), s.cookieRefresh.CheckNow, s.log, "browser refresh")
		return result, nil
```

and in the setup-wizard finish closure (`:483-491`), after a successful `FinishSetupDetailed`:

```go
			result, err := s.autoCookieSvc.FinishSetupDetailed(finishCtx)
			if err != nil {
				s.log.Error("Failed to finish auto-cookie setup", slog.String("error", err.Error()))
				return result, err
			}
			// A completed wizard has just written cookies.txt from the browser
			// the operator signed in to — the most deliberate credential change
			// there is, and until Arc 10 the one that told the running process
			// nothing. Same context as the finish itself is deliberately NOT
			// used: finishCtx is cancelled by the defer above.
			recheckAfterCookieWrite(context.Background(), s.cookieRefresh.CheckNow, s.log, "the setup wizard")
			return result, nil
```

- [ ] **Step 8: Add the re-check to the Web setup-finish handler**

In `internal/web/routes/cookies.go`, in the `POST /api/cookies/auto-setup/finish` handler, immediately before `jsonResponse(rw, cookieSetupOutcome(result))` (`:504`):

```go
		// The dashboard twin of the TUI wizard's re-check, and the same shape
		// as the auto-refresh handler's at the top of this file: a completed
		// wizard has just rewritten cookies.txt, and refresh's status block is
		// the only place the credential fingerprint is compared and the Twitch
		// auth mark cleared. Bare, with no log line, for the reason stated
		// there — the routes package has no operational logger.
		if refreshSvc != nil {
			refreshSvc.CheckNow(req.Context())
		}
		jsonResponse(rw, cookieSetupOutcome(result))
```

(`refreshSvc` is `CookieRoutes`' first parameter and is captured by this closure; the nil guard matches the `autoCookieSvc == nil` guard at the top of the same handler.)

- [ ] **Step 9: Write the failing test for the periodic-timer seam**

Create `internal/cookies/autocookies_pass_completed_test.go`:

```go
package cookies

import "testing"

// Arc 10 Task 7a. The periodic auto-cookie timer is the one credential-writing
// path whose caller lives INSIDE this package, so it is the one that needs an
// injected seam rather than a call at the caller.

// TestNotePassCompletedFiresTheHook.
//
// The mutation: notePassCompleted not invoking the hook — an inverted nil
// guard, or a body that falls through. Firing once per PROCESS rather than
// once per pass is the second, which is why two calls are made and counted.
//
// What this does NOT catch, and structurally cannot: the TICK failing to call
// notePassCompleted at all. That branch needs a browser profile, a browser and
// a network — the reason the seam is a named method in the first place — so it
// is a field residual, listed as field gate 6(d).
func TestNotePassCompletedFiresTheHook(t *testing.T) {
	calls := 0
	s := &AutoCookieService{OnPassCompleted: func() { calls++ }}

	s.notePassCompleted()
	s.notePassCompleted()

	if calls != 2 {
		t.Errorf("the hook fired %d times for two completed passes, want 2", calls)
	}
}

// TestNotePassCompletedWithNoHookIsSafe. The hook is injected by cmd/moombox;
// every test in this package, and any embedding that does not wire it, has
// none.
//
// The mutation: dropping the nil guard — a panic inside the periodic
// goroutine, which is recovered but kills that timer for the life of the
// process.
func TestNotePassCompletedWithNoHookIsSafe(t *testing.T) {
	s := &AutoCookieService{}
	s.notePassCompleted() // must not panic
}
```

- [ ] **Step 10: Run it and confirm it fails**

Run: `go test ./internal/cookies/ -run TestNotePassCompleted -v`
Expected: compile failure — `unknown field OnPassCompleted`, `s.notePassCompleted undefined`.

- [ ] **Step 11: Add the seam**

In `internal/cookies/autocookies.go`, add the field to `AutoCookieService` beside the other injected funcs (`HasActiveJobs` and friends):

```go
	// OnPassCompleted is called after a PERIODIC refresh tick that actually ran
	// a pass, so whoever owns the in-process auth check can re-read the file
	// this pass may have rewritten.
	//
	// Injected rather than called directly for the reason FallbackLiveness and
	// HasActiveJobs are: this package must not reach into RefreshService's
	// lifecycle, and cmd/moombox holds both. It exists ONLY for the periodic
	// timer — every other credential-writing gesture has a caller outside this
	// package that calls CheckNow itself (see the plan's Task 3 table), and
	// firing this from RefreshCookiesDetailed as well would double every one of
	// those.
	//
	// Called on the periodic goroutine with no lock held, and it MAY run a full
	// in-process re-check (RefreshService.CheckNow — two validate round-trips,
	// up to their timeouts). That is deliberate rather than tolerated: the
	// ticker coalesces missed ticks, so a slow hook costs cadence, never
	// correctness, and the alternative — spawning a goroutine here — would put
	// an unbounded number of re-checks behind a browser pass that is already
	// single-flighted. What the hook must NOT do is block forever.
	OnPassCompleted func()
```

and the method beside the other small helpers:

```go
// notePassCompleted fires OnPassCompleted if one is wired.
//
// A named method rather than an inline nil check so the decision has a seam a
// test can drive: the tick that calls it needs a browser profile, a browser
// and a network, so the branch is otherwise unreachable offline.
func (s *AutoCookieService) notePassCompleted() {
	if s.OnPassCompleted != nil {
		s.OnPassCompleted()
	}
}
```

and call it at the periodic tick's tail, gated on `Ran` (`autocookies.go:2897-2910`).

**The tick must switch from `refreshCookies` to `refreshCookiesDetailed` to do that.** `refreshCookies` (`autocookies.go:1749-1752`) is a two-line wrapper that returns `(result.AnyVerified(), err)` and DISCARDS `Ran` — and `refreshCookiesDetailed` has seven `refreshDeclined()` exits (`:1796`, `:1810`, `:1815`, `:1850`, `:1860`, `:1863`, `:1916`) that reach this tail having written nothing at all. Firing the hook on those would contradict this task's own rule at Step 5, where the recovery re-check is gated on `Ran` precisely because "a DECLINED pass wrote nothing at all". The cost of getting it wrong is bounded — one wasted validate pass per declined tick — but a rule that holds at one of two sites is not a rule.

`ok` keeps its exact meaning: `AnyVerified()` is what the wrapper returned and what the Debug line below reads.

```go
				refreshCtx, cancel := context.WithTimeout(ctx, refreshOverallBudget)
				// Detailed, not the bool wrapper: only the full result carries
				// Ran, and Ran is what decides whether anything was written.
				result, err := s.refreshCookiesDetailed(refreshCtx, gateExempt)
				cancel()
				ok := result.AnyVerified()
				// Gated on Ran, NOT on success. A pass that ran and failed still
				// rewrote cookies.txt — a browser refresh that produced a
				// new-but-dead pair moves the credential fingerprint exactly as a
				// working one does — so firing on success only would leave the
				// Twitch auth mark keyed to a pair that is no longer on disk. A
				// DECLINED pass (seven refreshDeclined() exits) wrote nothing, so
				// there is nothing to re-read.
				if result.Ran {
					s.notePassCompleted()
				}
				if err != nil {
					s.logger.Warn("periodic auto-cookie refresh failed", "err", err)
				} else if ok {
```

- [ ] **Step 12: Wire the hook**

In `cmd/moombox/services.go`, beside the other `autoCookieSvc` injections (after `autoCookieSvc.HasActiveJobs`, `:848-854`):

```go
	// The periodic timer is the only credential-writing path with no caller
	// outside internal/cookies, so it gets the only injected re-check seam.
	// Everything else calls recheckAfterCookieWrite directly.
	//
	// Reads s.cookieRefresh at FIRE time by convention with the other injected
	// funcs, not out of necessity: it is already assigned by the time this
	// closure is built. §15 constructs and assigns cookieRefresh at
	// services.go:711-713, autoCookieSvc is built at :751, and this wiring runs
	// after both. The nil guard is therefore belt and braces against a future
	// reordering rather than a live hazard — CheckNow on a nil *RefreshService
	// would panic inside the periodic goroutine, which is recovered but kills
	// that timer for the life of the process.
	autoCookieSvc.OnPassCompleted = func() {
		if s.cookieRefresh == nil {
			return
		}
		recheckAfterCookieWrite(context.Background(), s.cookieRefresh.CheckNow, log, "the periodic cookie refresh")
	}
```

That is the whole of Step 12 — one block, guarded. Do not drop the nil check as "belt and braces": belt and braces is why it is cheap, not why it is optional.

- [ ] **Step 13: Run the tests**

Run: `go test ./internal/cookies/ -run TestNotePassCompleted -v`
Expected: PASS for both.

Run: `go test ./cmd/moombox/ ./internal/cookies/ ./internal/web/routes/ -count=1`
Expected: all three `ok`. `cmd/moombox`'s existing `monitor_callbacks_recovery_test.go` drives `handleRecoveryNeeded`, not `runCookieRecovery`, so the hoist changes nothing it asserts — if a test there does fail, the hoist has moved a call it pins and the fix is to update the test, not to revert the hoist.

- [ ] **Step 14: Run the gates**

```bash
go build ./...
go vet ./...
GOOS=linux go build ./...
gofmt -l internal/ cmd/
go test -count=1 ./...
```
Expected: build and vet clean, `gofmt -l` prints nothing, 27 packages ok / 0 fail.

- [ ] **Step 15: Commit**

```bash
git add cmd/moombox/monitor_callbacks.go cmd/moombox/monitor_callbacks_recheck_test.go cmd/moombox/services.go cmd/moombox/tui_wiring.go internal/web/routes/cookies.go internal/cookies/autocookies.go internal/cookies/autocookies_pass_completed_test.go
git commit -m "$(cat <<'EOF'
fix(cookies): every gesture that writes credentials now re-checks them

Arc 10 R4/R5. refresh's status block is the only place the credential
fingerprint is compared and the Twitch auth mark cleared, and four gestures
that rewrite cookies.txt never reached it: the worker's auth-failure browser
refresh, the recovery path's two non-OK verdict arms, both setup-wizard
finish paths, and the auto_enabled periodic timer. Each was bounded by the
30-minute ticker, which is what "immediately apply the updated cookie" rules
out — the first most sharply, since a job's own refresh left every OTHER live
Twitch capture anonymous for up to half an hour.

The recovery re-check is hoisted above the verdict switch and gated on Ran
rather than on success: a browser pass that wrote a new-but-DEAD pair moves
the fingerprint exactly as a working one does, so the mark taken under the old
pair would otherwise stand and name a problem the file no longer has.

One helper for the five cmd/moombox sites, because the Info line that explains
a stale badge is the part five copies would drop. The two routes sites stay
bare — that package has no operational logger. The periodic timer gets the
only injected seam: it is the one path whose caller lives inside
internal/cookies.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

---

### Task 8: The HLS side — branch A or branch B, decided by Task 0's report

Spec R6. **Read `.superpowers/sdd/2026-08-25-cookie-subsystem-remediation/progress-arc10.md` § "Task 0 report — playback token shape" before writing anything.** Its `FINDING:` line names the branch. Take exactly one.

**Take BRANCH A when** the report's `KEYS ONLY IN THE AUTHENTICATED REPLY` or `KEYS WHOSE TYPE OR BOOLEAN DIFFERS` line names a field that identifies the session — `user_id` in Twitch's published shape, present-and-numeric when authenticated and `null` when not. The code below is written against `user_id`; if the report names a different key for the same fact, substitute that key string in the two places it appears and change nothing else. If the report names only fields that differ for reasons unrelated to identity (an expiry, a nonce, a device id), that is NOT a session statement — take branch B.

**Take BRANCH B when** the `FINDING:` line says the replies are indistinguishable, or when the only differing keys are not about the session.

Do not take both. Do not take neither.

**This task runs after Tasks 0, 2, 6 and 7** (branch A extends Task 2's vocabulary and Task 6's `reasons` slice, and anchors to Task 7's `SetOnTwitchAuthLoss` block), so **every line number below was verified at pristine `main` and most of these files have since been edited.** Re-derive by SYMBOL — the `AuthDowngrade*` const block, `Service`'s struct, `GetHLSMasterPlaylist`, the `twitchLoss*` const block, `twitchAuthLossMessage`'s `default` arm, `TestTwitchAuthLossReasonIsTheVocabularyOnly`'s `known` slice, `twitch_auth_loss_vocabulary_test.go`'s `reasons` slice, `dlWorker.SetOnTwitchAuthLoss` — not by number. Same rule as Tasks 3 and 7a.

**Neither branch edits a doc.** Task 9 is the sole doc-editing task in this arc and it carries both variants of every HLS-side sentence, selecting by the same Task 0 report this task reads. Branch B's whole deliverable is the ledger entry that tells Task 9 which variant to take.

---

#### Branch A — the playback token states whether it was honoured

**Files:**
- Modify: `internal/twitch/chat.go:38-54` — one more `AuthDowngrade*` constant
- Modify: `internal/twitch/service.go:11-21` — one field on `Service`; `:62-71` — `GetHLSMasterPlaylist`
- Create: `internal/twitch/playback_token.go` — `PlaybackTokenSession`
- Modify: `internal/cookies/refresh.go` — a fifth vocabulary constant and a fifth arm in `twitchAuthLossMessage`
- Modify: `internal/cookies/refresh_twitch_mark_test.go` (Task 2) — extend `TestTwitchAuthLossReasonIsTheVocabularyOnly`'s `known` slice
- Modify: `internal/worker/twitch_auth_loss_vocabulary_test.go` (Task 6) — extend the `reasons` slice
- Modify: `cmd/moombox/services.go` — wire `OnAnonymousPlayback` beside `SetOnTwitchAuthLoss`
- Test: `internal/twitch/playback_token_test.go` (create)

**Interfaces:**
- Consumes: `(*API).GetStreamAccessToken` (`api.go:687`); `(*Auth).GetAuthToken() string` (`auth.go:38`); `(*cookies.RefreshService).NoteTwitchAuthLoss` (Task 2)
- Produces: `func PlaybackTokenSession(value string) (signedIn, conclusive bool)`; `func playbackTokenReportsAnonymous(authToken, tokenValue string) bool`; `Service.OnAnonymousPlayback func()`; `const AuthDowngradePlaybackTokenAnonymous = "playback-token-anonymous"`

- [ ] **A Step 1: Write the failing tests**

Create `internal/twitch/playback_token_test.go`:

```go
package twitch

import "testing"

// Arc 10 R6, branch A. These are the exact shapes the Task 0 live probe
// observed, reduced to the one key that answers the question. No fixture here
// carries a real token: the Signature is absent entirely and every value is
// obviously synthetic.

// TestPlaybackTokenSessionReadsTheAuthenticatedAndAnonymousShapes.
//
// The mutation: reading truthiness of the whole document, or treating any
// non-empty Value as authenticated — both of which pass on the authenticated
// fixture and report an anonymous token as signed in, which is the exact
// silence this branch exists to end.
func TestPlaybackTokenSessionReadsTheAuthenticatedAndAnonymousShapes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		value      string
		signedIn   bool
		conclusive bool
	}{
		{"authenticated", `{"user_id":12345678,"expires":1900000000,"channel":"somechannel"}`, true, true},
		{"anonymous null", `{"user_id":null,"expires":1900000000,"channel":"somechannel"}`, false, true},
		{"authenticated with a string id", `{"user_id":"12345678","expires":1900000000}`, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			signedIn, conclusive := PlaybackTokenSession(tc.value)
			if signedIn != tc.signedIn || conclusive != tc.conclusive {
				t.Errorf("PlaybackTokenSession = (%v, %v), want (%v, %v)", signedIn, conclusive, tc.signedIn, tc.conclusive)
			}
		})
	}
}

// TestPlaybackTokenSessionRefusesToGuess is the over-claim guard, and it is
// the assertion that keeps this from becoming a false-alarm generator.
//
// A key Twitch RENAMES, a document that stops being JSON, an empty value — none
// of those is a statement about the session, and reporting any of them as
// anonymous would mark the platform dead on every capture, for every user, on
// the day of the change.
//
// The mutation: `if _, ok := doc["user_id"]; !ok { return false, true }` —
// folding "the key is gone" into "the token belongs to nobody". That mutation
// passes both cases above and turns a Twitch field rename into a global
// credential alarm.
func TestPlaybackTokenSessionRefusesToGuess(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{"the key was renamed", `{"userId":12345678,"expires":1900000000}`},
		{"not an object", `"a signed opaque blob"`},
		{"not JSON at all", `%7B%22user_id%22%3A1%7D`},
		{"empty", ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			signedIn, conclusive := PlaybackTokenSession(tc.value)
			if conclusive {
				t.Errorf("PlaybackTokenSession reported a CONCLUSIVE %v for a document it cannot read", signedIn)
			}
		})
	}
}

// TestPlaybackTokenReportsAnonymousIsTheWholeDecision drives the PRODUCTION
// predicate, not a copy of it.
//
// The decision has to live in a named function for that reason. Written inline
// in GetHLSMasterPlaylist it is unreachable offline — that method makes a GQL
// call and then fetches a real Usher playlist, neither of which has a seam — so
// a test could only restate the condition and would then pass under every
// mutation of the real one. playbackTokenReportsAnonymous is the whole
// decision, and GetHLSMasterPlaylist's only job is to call it.
//
// Two halves, both load-bearing. A cookieless install gets an anonymous
// playback token BY DESIGN and must never be told its credentials failed —
// the same rule noteMissingLogin's token check enforces on the chat side. And
// an install that DID send a token and got an anonymous document back is the
// case this branch exists for: it is visible even with chat capture off.
//
// The mutations: dropping the `authToken != ""` guard (every cookieless
// install marks Twitch dead on its first capture), reporting on `!conclusive`
// (every Twitch response-shape change does the same, for every user, on the
// day of the change), and inverting `signedIn` (a healthy credential marks
// itself dead). All three are caught here because all three are in this
// function.
func TestPlaybackTokenReportsAnonymousIsTheWholeDecision(t *testing.T) {
	for _, tc := range []struct {
		name       string
		authToken  string
		value      string
		wantReport bool
	}{
		{"cookieless install, anonymous token", "", `{"user_id":null}`, false},
		{"cookieless install, unreadable document", "", `{"userId":1}`, false},
		{"credentials sent, anonymous token", "test-token-aaaa", `{"user_id":null}`, true},
		{"credentials sent, honoured", "test-token-aaaa", `{"user_id":12345678}`, false},
		{"credentials sent, unreadable document", "test-token-aaaa", `{"userId":1}`, false},
		{"credentials sent, not JSON", "test-token-aaaa", `not json`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := playbackTokenReportsAnonymous(tc.authToken, tc.value); got != tc.wantReport {
				t.Errorf("playbackTokenReportsAnonymous = %v, want %v", got, tc.wantReport)
			}
		})
	}
}
```

- [ ] **A Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/twitch/ -run TestPlaybackToken -v`
Expected: compile failure — `undefined: PlaybackTokenSession`, `undefined: playbackTokenReportsAnonymous`.

- [ ] **A Step 3: Write `PlaybackTokenSession`**

Create `internal/twitch/playback_token.go`:

```go
package twitch

import "encoding/json"

// PlaybackTokenSession reports what a playback access token says about the
// session it was issued to: (signedIn, conclusive).
//
// TwitchAccessToken.Value is a JSON document Twitch signs and Usher verifies.
// Nothing in Moombox has ever looked inside it — it is passed through verbatim
// as a URL parameter by BuildUsherLiveURL, which percent-encodes it at that
// moment, so the field itself is plain JSON and needs no unescaping here.
//
// What is inside is the entitlement the stream will actually be served under,
// and that is the fact a dead auth-token hides: GetStreamAccessToken succeeds
// either way and returns the same two-field shape, so an expired token
// silently yields an ANONYMOUS playback token. The capture then takes stitched
// ads (skipped, correctly, leaving a timestamp jump) and is refused outright on
// subscriber-only content — with nothing above Info in the log to explain it.
//
// user_id is the discriminator, confirmed by the Arc 10 Task 0 live probe: an
// authenticated reply carries it, an anonymous one carries null. No other
// field is read, and NOTHING read here leaves the function: the return is two
// booleans.
//
// conclusive == false means this learned nothing, and the caller must treat it
// as silence. THE ABSENT-KEY CASE IS DELIBERATELY INCONCLUSIVE, not anonymous:
// a key Twitch renames is a response-shape change, and folding it into "the
// token belongs to nobody" would mark the platform dead on every capture for
// every user on the day of that change. An explicit null is the only anonymous
// verdict this will give.
func PlaybackTokenSession(value string) (signedIn, conclusive bool) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value), &doc); err != nil {
		return false, false
	}
	raw, ok := doc["user_id"]
	if !ok {
		return false, false
	}
	if string(raw) == "null" {
		return false, true
	}
	return true, true
}

// playbackTokenReportsAnonymous is the WHOLE decision behind the HLS-side
// credential mark: should this playback token be reported as proof that the
// saved Twitch credentials are dead?
//
// It is a named function rather than three conditions inline in
// GetHLSMasterPlaylist because inline it would be untestable — that method
// makes a GQL call and then fetches a real Usher playlist, so a test could
// only restate the condition, and a restated condition passes under every
// mutation of the real one.
//
// Three conditions, each doing its own work:
//
//   - authToken != "" — credentials were actually SENT. A cookieless install
//     gets an anonymous token by design and must never be told its credentials
//     failed; this is the same guard noteMissingLogin's token check applies on
//     the chat side, and dropping it marks Twitch dead on every install that
//     never configured it.
//   - conclusive — the document was readable. A renamed key is a response
//     shape change, not a verdict; see PlaybackTokenSession.
//   - !signedIn — Twitch says this token belongs to nobody.
func playbackTokenReportsAnonymous(authToken, tokenValue string) bool {
	if authToken == "" {
		return false
	}
	signedIn, conclusive := PlaybackTokenSession(tokenValue)
	return conclusive && !signedIn
}
```

- [ ] **A Step 4: Add the reason token and the hook, and read the token**

In `internal/twitch/chat.go`, add to the `AuthDowngrade*` const block:

```go
	// AuthDowngradePlaybackTokenAnonymous: the jar held credentials and Twitch
	// nonetheless issued an ANONYMOUS playback access token, so this capture
	// is served stitched ads and would be refused subscriber-only content.
	// Reported by Service.GetHLSMasterPlaylist, NOT by the chat downloader —
	// it lives in this block because it is a member of the same vocabulary and
	// splitting the vocabulary across two files is how two of them drift.
	AuthDowngradePlaybackTokenAnonymous = "playback-token-anonymous"
```

In `internal/twitch/service.go`, add the hook to `Service`:

```go
	// OnAnonymousPlayback is called when the jar HELD Twitch credentials and
	// Twitch nonetheless issued an anonymous playback access token.
	//
	// This is the ONE site that can see a dead Twitch credential on a job with
	// chat capture switched off, which is why it exists at all — every other
	// detector is on the IRC path. nil is the ordinary case (tests, and any
	// caller with nowhere to route it).
	//
	// Called on the goroutine that asked for the playlist, which is a job
	// goroutine mid-probe, so it must not block. cmd/moombox's wiring spawns
	// its own goroutine for exactly that reason.
	OnAnonymousPlayback func()
```

and rewrite `GetHLSMasterPlaylist`:

```go
// GetHLSMasterPlaylist fetches and parses the HLS master playlist for a live channel.
func (s *Service) GetHLSMasterPlaylist(ctx context.Context, channelLogin string) ([]TwitchHLSVariant, error) {
	// Read ONCE and reuse: the same value must decide the request and the
	// verdict below, or a jar reload between two reads could report a
	// cookieless install as one whose credentials failed.
	authToken := s.Auth.GetAuthToken()
	token, err := s.API.GetStreamAccessToken(ctx, channelLogin, authToken)
	if err != nil {
		return nil, fmt.Errorf("get access token: %w", err)
	}

	// The dead-credential check, and the only one a chat-disabled job gets.
	// The decision itself is playbackTokenReportsAnonymous, which is where the
	// three conditions and their reasons live — and where they are testable.
	if s.OnAnonymousPlayback != nil && playbackTokenReportsAnonymous(authToken, token.Value) {
		// Names the channel and the consequence. Nothing from the token
		// document reaches this line.
		s.logger.Warn("twitch issued an ANONYMOUS playback token although credentials were sent; "+
			"this capture will be served stitched ads and cannot fetch subscriber-only content",
			"channel", channelLogin)
		s.OnAnonymousPlayback()
	}

	url := BuildUsherLiveURL(channelLogin, token)
	return FetchHLSMasterPlaylist(ctx, url)
}
```

- [ ] **A Step 5: Add the fifth vocabulary member**

In `internal/cookies/refresh.go`, add to the vocabulary const block:

```go
	twitchLossPlaybackTokenAnonymous = "playback-token-anonymous"
```

and the arm to `twitchAuthLossMessage`, before the `default`:

```go
	case twitchLossPlaybackTokenAnonymous:
		return "Twitch issued an anonymous playback token although saved credentials were sent."
```

Then extend `internal/worker/twitch_auth_loss_vocabulary_test.go`'s `reasons` slice with `twitch.AuthDowngradePlaybackTokenAnonymous`, so the drift pin covers all five. Extend `internal/cookies/refresh_twitch_mark_test.go`'s `known` slice in `TestTwitchAuthLossReasonIsTheVocabularyOnly` likewise.

- [ ] **A Step 6: Wire it in `cmd/moombox`**

In `cmd/moombox/services.go`, immediately after the `dlWorker.SetOnTwitchAuthLoss(...)` block from Task 7:

```go
	// The HLS half of Arc 10 R1: a dead Twitch credential is visible even on a
	// job with chat capture off, because the playback access token says whose
	// session it was issued to. Routed to the SAME mark as the chat downgrade,
	// through the same vocabulary.
	//
	// Its own goroutine with the inline recover, for the reason the chat seam
	// above states: the caller is a job goroutine mid-probe, and
	// NoteTwitchAuthLoss can reach a synchronous webhook.
	//
	// Set once here, before any job goroutine exists; twService is shared and
	// this field is never reassigned.
	twService.OnAnonymousPlayback = func() {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Error("panic marking twitch playback-token anonymity", "panic", fmt.Sprint(r))
				}
			}()
			s.cookieRefresh.NoteTwitchAuthLoss(twitch.AuthDowngradePlaybackTokenAnonymous)
		}()
	}
```

Confirm `cmd/moombox/services.go` already imports `internal/twitch` (it does — `twitch.NewService` is called at `:412`).

- [ ] **A Step 7: Run the tests**

Run: `go test ./internal/twitch/ -run TestPlaybackToken -v -count=1`
Expected: PASS.

Run: `go test ./internal/twitch/ ./internal/cookies/ ./internal/worker/ ./cmd/moombox/ -count=1`
Expected: all four `ok`.

- [ ] **A Step 8: Run the gates**

```bash
go build ./...
go vet ./...
GOOS=linux go build ./...
gofmt -l internal/ cmd/
go test -count=1 ./...
```
Expected: build and vet clean, `gofmt -l` prints nothing, 27 packages ok / 0 fail.

**Named field gate:** nothing offline can prove the mark fires on a real dead token, because the decision needs a live GQL reply. The first capture on an expired `auth-token` is the gate; the Warn line above is what makes it readable.

- [ ] **A Step 9: Commit**

```bash
git add internal/twitch/playback_token.go internal/twitch/playback_token_test.go internal/twitch/chat.go internal/twitch/service.go internal/cookies/refresh.go internal/cookies/refresh_twitch_mark_test.go internal/worker/twitch_auth_loss_vocabulary_test.go cmd/moombox/services.go
git commit -m "$(cat <<'EOF'
feat(twitch): an anonymous playback token marks the platform, even with chat off

Arc 10 R6, branch A — the Task 0 live probe found the playback access token
states which session it was issued to.

GetStreamAccessToken succeeds whether or not the auth-token is alive and
returns the same two opaque fields, so an expired credential silently
produced an ANONYMOUS playback token: stitched ads, a timestamp jump in the
archive, and outright refusal on subscriber-only content, with nothing above
Info to explain it. This is the only detector a chat-disabled job gets.

PlaybackTokenSession refuses to guess. An absent key is INCONCLUSIVE, not
anonymous: folding a Twitch field rename into "the token belongs to nobody"
would mark the platform dead for every user on the day of that change. Only
an explicit null is an anonymous verdict, and a cookieless install is never
reported at all.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

---

#### Branch B — the reply cannot tell, so record the finding and stop

**No code changes, and NO DOC CHANGES.** An earlier draft had this branch edit three spec files; it does not, because Task 9 edits the same three sentences and the two would collide — once as a duplicated paragraph in `data-and-storage.md`, once as two near-identical "since Arc 10 it is a check plus a MARK" sentences in `SPEC.md`. **Task 9 is the sole doc-editing task in this arc** and carries both variants of every HLS-side sentence. Branch B's entire deliverable is the ledger entry that tells Task 9 which variant to take, and that is not a demotion: the measurement IS the deliverable, and a finding recorded in one place cannot drift from itself.

**Files:**
- Modify: `.superpowers/sdd/2026-08-25-cookie-subsystem-remediation/progress-arc10.md` (append)

- [ ] **B Step 1: Write the ruling into the ledger**

Under the existing `## Task 0 report — playback token shape` heading created by Task 0 Step 4, append:

```markdown
### Arc 10 Task 8 ruling: BRANCH B — no HLS-side detector

The playback access token cannot report whether it was honoured. Measured live
on <DATE> (see the KEYS lines above): <one sentence naming what was actually
seen — "the two replies are indistinguishable by name, type and boolean", or
the differing keys and why none of them is a statement about the session>.

Consequences, and the ONLY place they are recorded:

- No `PlaybackTokenSession`, no `playbackTokenReportsAnonymous`, no
  `Service.OnAnonymousPlayback`, no fifth reason token. The chat handshake's
  four routes stay the whole detector.
- A job with chat recording OFF has no credential detector at all. The only
  observable signal on that side is downstream and probabilistic: stitched-ad
  `#EXT-X-DATERANGE` markers on the MEDIA playlist during the capture
  (`collectAdDateRanges`, `internal/engine/manifest.go`).
- **Task 9 takes the "branch B" variant** of its three HLS-side sentences
  (Step 1(c), Step 5, and the `AuthStatus` paragraph's closing clause). Do not
  edit any spec file from this task.

Do not re-open without a NEW measurement; this one is dated above.
```

Substitute the date and the one-sentence finding. Nothing read from the token document goes in — the KEYS lines Task 0 already wrote are the record, and they carry names, types and booleans only.

- [ ] **B Step 2: Confirm nothing else changed**

```bash
git status --short
```
Expected: exactly one modified file, the ledger. If a `docs/spec/` or `SPEC.md` path appears, an earlier draft's instructions were followed — revert them and let Task 9 do it.

- [ ] **B Step 3: Run the gates**

```bash
go build ./...
go vet ./...
GOOS=linux go build ./...
gofmt -l internal/ cmd/
go test -count=1 ./...
```
Expected: byte-identical to Task 7a's run — build and vet clean, `gofmt -l` prints nothing, 27 packages ok / 0 fail. Nothing in this branch touches Go.

- [ ] **B Step 4: Commit**

```bash
git add .superpowers/sdd/2026-08-25-cookie-subsystem-remediation/progress-arc10.md
git commit -m "$(cat <<'EOF'
docs(sdd): the Twitch playback token cannot report whether it was honoured

Arc 10 R6, branch B. The live probe compared the playback access token's
Value document with and without an auth-token and found nothing in it that
states which session it was issued to, so the HLS-side detector R6 made
conditional is not buildable.

The finding lands in the ledger and nowhere else. Task 9 owns every spec
sentence in this arc and carries both variants of the three HLS-side ones;
writing them here as well would have duplicated a paragraph in
data-and-storage.md and left two near-identical claims in SPEC.md.

The only observable signal on that side remains downstream and
probabilistic: stitched-ad DATERANGE markers on the media playlist during the
capture.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

---

### Task 9: The doc sentences that change with the code

Spec §5 names five documents. Every edit below quotes the CURRENT sentence and gives the replacement; open each file and confirm the quoted text before editing, because a sentence that has already drifted means the surrounding paragraph needs re-reading, not a blind replace.

**This is the SOLE doc-editing task in the arc.** (Task 7 Steps 4a/4b edit one JS doc COMMENT beside the code they change, which is a code comment, not a spec file.) No other task edits a file under `docs/spec/` or `SPEC.md` — Task 8 branch B was rewritten to record its finding in the ledger precisely so these sentences have one author. That matters most for the three HLS-side ones, which have TWO variants each: what is true if Task 8 took branch A, and what is true if it took branch B. Both are written out below.

**Before editing anything, read `.superpowers/sdd/2026-08-25-cookie-subsystem-remediation/progress-arc10.md`** — Task 0's `FINDING:` line and, if branch B was taken, Task 8's `### Arc 10 Task 8 ruling` entry. That decides which variant of Step 1(c), Step 2(d)'s closing clause and Step 5 you write. If the ledger has no Task 8 ruling entry and `internal/twitch/playback_token.go` exists, branch A was taken; if neither, Task 8 has not run and this task is premature.

**Files:**
- Modify: `docs/spec/platform-services.md` (three sentences, `:514`, `:527`, `:529`)
- Modify: `docs/spec/data-and-storage.md` (five sites, `:733`, `:736`, `:748`, `:835`, `:857`)
- Modify: `docs/spec/user-interfaces.md` (four sites, `:395`, `:672`, `:693`, `:851`)
- Modify: `docs/spec/operations.md` (three table rows and one sentence, `:471`, `:472`, `:473`, `:475`)
- Modify: `SPEC.md` (two clauses in one paragraph, `:668-670`)

Three of these edits have two variants — `platform-services.md` Step 1(c), `SPEC.md` Step 5's caller sentence, and (for context only) the `AuthStatus` paragraph, which is branch-neutral. The file COUNT and the file LIST are the same on both branches.

**Interfaces:**
- Consumes: everything Tasks 1-8 built, plus `.superpowers/sdd/2026-08-25-cookie-subsystem-remediation/progress-arc10.md` — Task 0's `FINDING:` line and Task 8 branch B's ruling entry, which select the variants
- Produces: nothing — this is the last task

- [ ] **Step 1: `docs/spec/platform-services.md`**

**(a)** In the **"One-shot anonymous fallback."** paragraph (`:514`), this clause is now false:

> `noteHandshakeOutcome` (`chat.go`) latches `authRefused`, `sessionCredentials` then returns an empty pair, and every later reconnect in this job uses the anonymous handshake rather than spending the reconnect budget on a login that cannot succeed.

Replace with:

> `noteHandshakeOutcome` (`chat.go`) latches `authRefused`, `sessionCredentials` then returns an empty pair, and every later reconnect in this job uses the anonymous handshake rather than spending the reconnect budget on a login that cannot succeed — **unless the credentials change**. Since Arc 10 the latch is one-shot per CREDENTIAL PAIR rather than per job: `Reauthenticate()` clears `authRefused`, `downgradeReported` and `warnedNoLogin` together and drops the live session, so a repaired `cookies.txt` gets one fresh authenticated handshake and a second refusal latches and reports again. Its own drop is charged nothing against the reconnect budget below and skips the backoff, and `runIRCSession`'s handshake-outcome defer refuses to read it as a refusal (`reauthPending`).

**(b)** In the **"One notification per job."** paragraph (`:527`), two things change. The heading claim narrows and the "next capture" sentence gains its remedy. Replace:

> `reportAuthDowngrade` latches across every trigger site (a THIRD flag, not a reuse of the per-site `warnedNoLogin` or the behaviour-changing `authRefused`), and the worker turns it into "Twitch chat is anonymous for {channel}"

with:

> `reportAuthDowngrade` latches across every trigger site (a THIRD flag, not a reuse of the per-site `warnedNoLogin` or the behaviour-changing `authRefused`), and the worker does TWO things with the one report: it turns it into "Twitch chat is anonymous for {channel}", **and** it marks the PLATFORM through `cookies.RefreshService.NoteTwitchAuthLoss` (the injected `StreamProcessor.onTwitchAuthLoss` seam, so `internal/worker` still does not import `internal/cookies`). The notice names the job; the mark names the platform, fires `OnRecoveryNeeded("twitch")` through the ordinary dedupe, and sticks against a validate 200 until the credential pair changes — see `data-and-storage.md` § Cookies. Neither replaces the other: without the notice nobody knows WHICH capture is degraded, and without the mark nothing attempts recovery. The latch is per credential pair, not per job, because `Reauthenticate` resets it.

**(c)** Replace the whole **"A job with chat recording off gets no signal at all."** paragraph (`:529`). Its current text is:

> **A job with chat recording off gets no signal at all.** `OnAuthDowngrade` is wired only when `liveDownloadChat` is true (`internal/worker/stream_processor_twitch.go`), so the degradation is detected only where chat is being captured. The site that would cover the rest is `GetHLSMasterPlaylist`, which already holds the token — a recorded follow-up, not built.

**On Task 8 branch A**, replace with:

> **A job with chat recording off is covered by the playback token.** `OnAuthDowngrade` is wired only when `liveDownloadChat` is true (`internal/worker/stream_processor_twitch.go`), so the CHAT detector sees only jobs that capture chat. The other detector is `Service.GetHLSMasterPlaylist`, which decodes the playback access token's `Value` (a JSON document — `PlaybackTokenSession`, `internal/twitch/playback_token.go`) and marks the platform with `playback-token-anonymous` when credentials were sent and Twitch answered with a token issued to nobody. It refuses to guess in both directions: a cookieless install is never reported (it is anonymous by design), and an UNREADABLE document — a renamed key, a non-JSON body — is inconclusive rather than anonymous, because folding a Twitch field rename into "the credential is dead" would alarm every install on the day of the change.

**On Task 8 branch B**, replace the same paragraph with:

> **A job with chat recording off gets no signal at all, and that is now a measured fact.** `OnAuthDowngrade` is wired only when `liveDownloadChat` is true (`internal/worker/stream_processor_twitch.go`), so the degradation is detected only where chat is being captured. The site that would cover the rest is `GetHLSMasterPlaylist`, which already holds the token — and it CANNOT: measured live on {DATE}, the playback access token's `Value` document says nothing about which session it was issued to ({one clause naming what the probe saw}), so an anonymous token and an honoured one are indistinguishable to the caller. The only observable signal there remains downstream and probabilistic — stitched-ad `#EXT-X-DATERANGE` markers on the MEDIA playlist during the capture (`collectAdDateRanges`, `internal/engine/manifest.go`). Chat-disabled jobs stay blind by measurement, not by omission; do not re-open this without a new measurement.

Take the date and the parenthetical clause verbatim from the ledger's Task 8 ruling — the record must say what was seen, not "nothing useful".

- [ ] **Step 2: `docs/spec/data-and-storage.md`**

**(a)** After the `YouTubeIdentity()` bullet (`:733`), add a sibling bullet:

> - `TwitchIdentity()`: SHA-256 over `auth-token + NUL + login`, both read under ONE `RLock` for the reason `GetTwitchCredentials` documents. `""` means **no Twitch credentials at all** — deliberately NOT `YouTubeIdentity`'s "either half missing" rule. The question here is "is this the same credential PAIR a downgrade was observed under", and a token with no `login` beside it is one of the four downgrade routes rather than an unanswerable state: folding it to `""` would make the operator's fix, adding the `login` row, compare equal to the breakage it replaced. A token rotation therefore reads as a change, which is the cheap direction — one re-check and one IRC reconnect that the credentials pass.

**(b)** In the **Thread safety** line (`:736`), extend the nil-safe list:

> Nil-receiver-safe where a caller may legitimately hold none (`HasAnyYouTubeAuthCookie`, `HasAnyTwitchAuthCookie`, `ExpiredAuthCookiesFor`, `AuthCookieHorizonFor`, `YouTubeIdentity`, `TwitchIdentity`).

**(c)** In the `SetExpectedPlatforms` paragraph (`:835`), this clause is now wrong:

> `OnCredentialsChanged` is separate and not a weaker `OnAuthRecovered`: a job parked because the signed-in account lacks a membership parked while auth was HEALTHY, so swapping accounts produces no auth transition to ride. `shouldObserveCredentials` and `advanceIdentityBaseline` (both pure) govern it

Replace with:

> `OnCredentialsChanged` is separate and not a weaker `OnAuthRecovered`: a job parked because the signed-in account lacks a membership parked while auth was HEALTHY, so swapping accounts produces no auth transition to ride. Since Arc 10 it fires for BOTH platforms, against per-platform baselines (`prevYouTubeIdentity` / `prevTwitchIdentity`), and the two mean different things: a YouTube fire is "the signed-in ACCOUNT may have changed", a Twitch fire is "the credential PAIR changed". The Twitch fire has a second subscriber — `cmd/moombox` broadcasts it to every live Twitch IRC chat downloader through `DownloadWorker.ReauthenticateTwitchChats`, which is the only way a capture already in flight learns that repaired cookies are on disk. `shouldObserveCredentials` and `advanceIdentityBaseline` (both pure, both platform-agnostic) govern both

**(d)** After the `AuthStatus` paragraph (`:857`), add a new paragraph — **this is the sole-writer replacement spec §4 requires.** It is added HERE and nowhere else: Task 8 branch B no longer touches this file, so there is no second author and no risk of adding it twice.

The paragraph is branch-neutral and is written the same way whichever branch Task 8 took: it describes the WRITE PATH, and that path is identical whether one producer calls it or two. Which producers exist is stated in Step 5 and Step 1(c), where the two variants live.

> **`rs.status` has TWO writers, both under `rs.mu`, and the mark wins.** `refresh`'s status block was the only one until Arc 10; `RefreshService.NoteTwitchAuthLoss(reason)` is the second. It writes the Twitch triple — `TwitchAuthenticated=false`, `TwitchVerification=RefreshFailed`, `TwitchError` = a sentence from a fixed `switch` of string literals — and records a `twitchAuthMark` carrying the reason and the `CookieJar.TwitchIdentity()` it was taken under. `refresh`'s block then CONSULTS that mark: while it stands, one `twEffective` value drives the status, the previous-auth baseline, `shouldFireRecovery`, the `OnAuthRecovered` transition and the identity baseline, so validate's 200 cannot flip any of them. It has to work this way — `oauth2/validate` answers 200 for a valid `auth-token` whether or not a usable `login` sits beside it, so two of the four chat-downgrade routes would otherwise be erased within one tick with nothing repaired. The mark clears on a **changed fingerprint alone**, with no authenticated gate: a swap to a REVOKED token must report the 401, not the stale reason from the pair it replaced. Recovery rides the existing `shouldFireRecovery` dedupe and advances the same two baselines, so one loss raises one alarm. The reason never leaves the vocabulary: `twitchAuthLossMessage`'s arms are all string literals, so the set of strings `TwitchError` can hold is fixed at compile time, which is what makes it safe on the two per-request surfaces that render it.

- [ ] **Step 3: `docs/spec/user-interfaces.md`**

In **"The reason strings are deliberately absent from this bar."** (`:395`), this parenthetical is now incomplete:

> `cookies.AuthStatus` carries `YouTubeError` / `TwitchError` — why a check reached `Unknown` — and they have readers on the two **per-request** paths only

Replace with:

> `cookies.AuthStatus` carries `YouTubeError` / `TwitchError` — why a check reached `Unknown`, or, for Twitch since Arc 10, which of the four chat-downgrade routes marked the platform (`NoteTwitchAuthLoss`, a fixed vocabulary of sentences) — and they have readers on the two **per-request** paths only

The rest of that paragraph stands unchanged and is now load-bearing for a second reason: a Twitch mark that changes only the REASON fires no `OnAuthChange`, exactly as the contract requires, so the operator gets it on the next `R C` or the next status fetch.

**Three more sentences in this file state the `RefreshUnknown` gating that Task 7 Steps 4a/4b removed.** All three were true when Arc 9 wrote them and all three are false the moment the mark exists.

**(b)** The Web badge's arm 5 (`:672`) currently reads:

> 5. `verification === "unknown"` → warning, `Cookies saved — Moombox could not establish whether they work`, with `(reason)` appended from `youtubeError` / `twitchError` when present. The reason is appended to **this arm only** — a conclusive `"ok"` or `"failed"` has no cause to give, and the producers leave the field empty there.

Replace with:

> 5. `verification === "unknown"` → warning, `Cookies saved — Moombox could not establish whether they work`, with `(reason)` appended from `youtubeError` / `twitchError` when present. Since Arc 10 the reason is appended to the **conclusive-refusal** arm too (`Not authenticated (…)`), because `NoteTwitchAuthLoss` writes `failed` beside one of four fixed sentences naming which Twitch chat-downgrade route broke — the only thing distinguishing a missing `login` cookie from a login Twitch refused. `"ok"` still shows nothing, by construction rather than by convention: `verdictFromCheck` returns OK only for a nil error and the reason string IS that error, so the two cannot co-occur. The gate is therefore on the STRING, not the verdict.

**(c)** The `R C` line's description (`:693`) currently reads:

> A reason is appended only for an *active* platform whose verdict is `RefreshUnknown`.

Replace with:

> A reason is appended for any *active* platform that has one, whatever its verdict — `RefreshUnknown` until Arc 10, and since then `RefreshFailed` as well, which is how the mark's four sentences reach the operator. An `RefreshOK` platform never has one to append.

**(d)** The divergence table's "Inconclusive-check reason" row (`:851`) currently reads:

> | Inconclusive-check reason | **Divergent, by contract.** The Web badge appends `youtubeError` / `twitchError` to its tooltip; the TUI status bar never does. The web has no cookie-status WebSocket event, so status and reason always arrive together in one `/api/status` fetch; the TUI bar is push-driven off `OnAuthChange`, whose gate excludes the two strings. The TUI surfaces the reason on the next `R C` instead. |

Replace with:

> | Check-reason rendering | **Divergent, by contract, and the divergence is PUSH vs PER-REQUEST rather than web vs TUI.** Both per-request surfaces render the reason whenever there is one — the Web badge's tooltip off `/api/status`, and the TUI's `R C` result line — for inconclusive AND conclusively-refused verdicts alike (Arc 10: the Twitch mark writes `failed` with a reason). Neither push-driven surface renders it: the TUI status bar is fed by `OnAuthChange`, whose `authStatusChanged` gate excludes the two strings, and the web has no cookie-status WebSocket event at all, so its badge is only ever painted from a fetch it asked for. The TUI bar surfaces the reason on the next `R C` instead. |

Note the row's LABEL changes with it — "Inconclusive-check reason" is no longer what the row is about.

- [ ] **Step 4: `docs/spec/operations.md`**

**(a0)** Replace the **Authentication Recovered** row (`:471`), whose "What it asserts" column currently reads:

> N jobs parked on this platform's cookies were resumed

with:

> N jobs parked on this platform's cookies were resumed. Since Arc 10 this edge does one more thing on Twitch and says nothing about it: `OnAuthRecovered` also re-authenticates every live Twitch chat session, because a transient refusal heals with the credential fingerprint UNCHANGED and so fires no `OnCredentialsChanged`. The notification still reports only the jobs — the chat reconnect is a log line (`twitch credentials usable again`), not an operator decision

**(a)** Replace the **Parked Jobs Re-evaluated** row (`:472`), whose "What it asserts" column currently reads:

> N parked jobs were resumed after the signed-in account was re-observed. States no cause on purpose: this fires on the first authenticated observation of EVERY process, not only on a real account change, so "a different account was supplied" would often be false

with:

> N parked jobs were resumed after the platform's saved credentials were re-observed. States no cause on purpose: this fires on the first authenticated observation of EVERY process, not only on a real change, so "a different account was supplied" would often be false. Since Arc 10 it fires for Twitch as well, whose credential is a bearer token and a login name rather than an account — which is why the wording says "saved credentials"

**(b)** Replace the **Twitch chat is anonymous for {channel}** row (`:473`)'s "What it asserts" column:

> A job that HAD Twitch credentials is capturing chat anonymously. Warning rather than Error because nothing failed — this capture is fine and the NEXT one starts anonymous. Fields: Channel, Job, Reason (one of the four `AuthDowngrade*` tokens, never a credential). See [platform-services.md](platform-services.md) § IRC Chat (Live)

with:

> A job that HAD Twitch credentials is capturing chat anonymously. Warning rather than Error because nothing failed — this capture is fine and the NEXT one starts anonymous. Fields: Channel, Job, Reason (one of the `AuthDowngrade*` tokens, never a credential). Since Arc 10 the SAME report also marks the platform (`NoteTwitchAuthLoss`), which is what fires "Cookie Re-Authentication Required" or the one automatic recovery attempt above — so an operator with `auto_enabled` off can receive both, one naming the job and one naming the platform. See [platform-services.md](platform-services.md) § IRC Chat (Live)

**(c)** In the cooldown sentence (`:475`), this clause needs a qualifier:

> The Twitch chat notice is latched once per downloader instead, and is deliberately NOT deduped across jobs — a later job with the same dead cookies must notify again.

Replace with:

> The Twitch chat notice is latched once per downloader instead, and is deliberately NOT deduped across jobs — a later job with the same dead cookies must notify again. Since Arc 10 that latch is reset by `Reauthenticate()`, so a repaired credential that fails AGAIN on the same job notifies again too; the platform mark it fires beside is deduped separately, by `shouldFireRecovery`, and so raises one alarm per loss however many jobs report it.

- [ ] **Step 4a: `docs/spec/data-and-storage.md` again — the single-flight paragraph's caller list**

Task 7a takes the "just rewrote `cookies.txt`" callers from two to five plus a helper, so this clause at `:748` is now an undercount:

> so the two callers in that position log the skip at **Info**: the post-recovery re-check (`handleRecoveryNeeded`, `cmd/moombox/monitor_callbacks.go`) and the post-`R F` re-check (`cmd/moombox/tui_wiring.go`), both saying "status may lag until the next refresh".

Replace with:

> so every caller in that position logs the skip at **Info**, through one helper — `recheckAfterCookieWrite` (`cmd/moombox/monitor_callbacks.go`), which says "status may lag until the next refresh" and names the gesture. Its callers are the post-recovery re-check (hoisted above `runCookieRecovery`'s verdict switch and gated on `RefreshResult.Ran`, so a pass that ran and FAILED is re-read too — it moved the credential fingerprint just as a working one would), the post-`R F` re-check, the worker's job-triggered refresh (`OnCookieRefreshNeeded`), the TUI setup wizard's finish, and the `auto_enabled` periodic timer through the one injected seam that path needs (`AutoCookieService.OnPassCompleted`). The two `internal/web/routes` callers — the dashboard/Settings browser refresh and the setup wizard's finish — call `CheckNow` bare, because that package has no operational logger. **Every gesture that can write `cookies.txt` ends in one of these**, and that is a requirement rather than an observation: `refresh`'s status block is the only place the Twitch credential fingerprint is compared and the auth mark cleared, so a writer that reaches no pass is invisible until the ticker.

- [ ] **Step 5: `SPEC.md`**

First, in the same paragraph, the on-demand list is now short. Replace:

> Always runs — on its own 30-minute timer, and on demand from either UI (`R C` / `POST /api/cookies/recheck`, plus the re-check each side runs after a browser refresh or an automatic recovery) — and is never gated on any config flag.

with:

> Always runs — on its own 30-minute timer, and on demand from either UI (`R C` / `POST /api/cookies/recheck`, plus the re-check that follows EVERY gesture which may have rewritten `cookies.txt`: both browser-refresh buttons, both setup-wizard finishes, the automatic recovery whatever its verdict, the worker's job-triggered refresh, and the `auto_enabled` periodic timer) — and is never gated on any config flag.

Then, in § Cookies, the `RefreshService` paragraph (`:670`) ends:

> **It rotates YouTube cookies only.** Google's `Set-Cookie` responses are admitted back into `cookies.txt` under `admitSetCookie`'s rules; there is no Twitch refresh anywhere in the process, and none appears to be possible in-process — reading yt-dlp's Twitch extractor and chatterino7 turned up no client that renews an `auth-token`, only ones that read it and detect its expiry, so a browser sign-in is the only thing observed to issue a new one. The Twitch side is a check with no rotation.

Replace the final sentence, **"The Twitch side is a check with no rotation."**, with:

> The Twitch side is a check with no rotation — plus, since Arc 10, a MARK. Anything that finds Twitch credentials refused where they are actually used calls `NoteTwitchAuthLoss(reason)`, which writes the Twitch triple under the same mutex, fires `OnRecoveryNeeded("twitch")` through the ordinary dedupe, and sticks against a `oauth2/validate` 200 until the credential pair's fingerprint changes (`CookieJar.TwitchIdentity`). Today's callers are the IRC chat handshake's four downgrade routes. A credential change also fires `OnCredentialsChanged("twitch")`, which re-authenticates every live chat session in place rather than waiting for the next job.

That replacement carries a placeholder sentence — **"Today's callers are the IRC chat handshake's four downgrade routes."** — which is the one clause with two variants. Write it per the branch Task 8 took, and write the rest of the paragraph exactly as given above either way.

**On Task 8 branch A**, that sentence reads:

> Today's callers are the IRC chat handshake's four downgrade routes and `GetHLSMasterPlaylist`, which reads whether the playback access token was issued to a signed-in session — the one detector a job with chat capture off still gets.

**On Task 8 branch B**, it reads:

> Today's only caller is the IRC chat handshake, through its four downgrade routes, so a job with chat recording off has no credential detector: the playback access token was measured and says nothing about whether it was honoured (see `platform-services.md` § Anonymous Fallback and the Downgrade Report).

Either way this is the ONLY edit to that sentence. Task 8 branch B no longer appends anything to this paragraph, so there is no second "since Arc 10 it is a check plus a MARK" claim to reconcile.

- [ ] **Step 6: Verify no citation went stale**

Every line number this task quotes was verified against the tree at `1114555`. Re-derive any that has moved, and check the two absence claims this arc falsified are gone:

```bash
grep -rn "fires for .youtube. only\|Fires for \"youtube\" only" internal/ docs/spec/ SPEC.md
grep -rn "a recorded follow-up, not built" docs/spec/
grep -rn "TwitchIdentity" docs/spec/data-and-storage.md
```
Expected: the first two print nothing; the third prints FOUR lines — the new `TwitchIdentity()` bullet, the thread-safety line, Step 2(c)'s `prevTwitchIdentity` clause and Step 2(d)'s `CookieJar.TwitchIdentity()` reference.

The first two greps are scoped to `docs/spec/` rather than `docs/`, and that is not tidiness: this plan file lives under `docs/superpowers/plans/` and QUOTES both falsified sentences several times (it has to — Task 3 and Task 9 name the text they replace), so an unscoped grep matches the plan itself and can never print nothing. If the greps are ever widened, add `--exclude-dir=superpowers`.

- [ ] **Step 7: Run the gates**

```bash
go build ./...
go vet ./...
GOOS=linux go build ./...
gofmt -l internal/ cmd/
go test -count=1 ./...
```
Expected: unchanged — build and vet clean, `gofmt -l` prints nothing, 27 packages ok / 0 fail. (Docs-only, but the arc closes on a green run.)

- [ ] **Step 8: Commit**

```bash
git add docs/spec/platform-services.md docs/spec/data-and-storage.md docs/spec/user-interfaces.md docs/spec/operations.md SPEC.md
git commit -m "$(cat <<'EOF'
docs(spec): the Twitch credential lifecycle, as the code now works

Arc 10's doc half. Five documents carried sentences the code has made false:
that rs.status has one writer, that OnCredentialsChanged fires for youtube
only, that the anonymous fallback lasts the whole job, that TwitchIdentity
does not exist, and that a chat downgrade only produces a per-job notice.

The sole-writer property is replaced rather than deleted: two writers, both
under rs.mu, mark wins until the credential fingerprint moves — with the
reason validate cannot see the two missing-login routes stated beside it, so
the next reader does not have to rediscover why the rule exists.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

---

## Field gates (open after this arc, for the owner's field-test plan)

These are not tasks. They are the claims this suite structurally cannot make, listed so the arc closes honestly.

1. **The registration site.** `ExecuteTwitch`'s `o.twitchChats.add(irc)` + `defer` is covered by compilation and by the registry's own tests. Reaching it needs a real Twitch download. The first live capture is the gate; the `Reauthenticate` Info line names the channel.
2. **The end-to-end repair.** Start a Twitch capture with an `auth-token` and no `login` row; confirm the platform badge goes not-authenticated AND that the missing-login sentence itself renders on BOTH per-request surfaces — the TUI's `R C` result line and the dashboard badge's tooltip (Task 7 Steps 4a/4b; before them a marked platform showed the verdict with no reason on either); add the `login` row; press `R C`; confirm one "re-authenticating live chat sessions" Info line with `sessions=1`, followed by one "twitch chat: authenticated login accepted" line for that channel (Task 4 Step 5 adds it; before Arc 10 nothing logged an accepted `001`).
2a. **The transient-refusal edge (Task 7's second hook).** With a capture running and chat authenticated, make validate fail transiently WITHOUT changing `cookies.txt` — block `id.twitch.tv` at the firewall for one 30-minute cycle, or point `twitchValidateURL` at a 401 — then restore it. Confirm the log carries `twitch credentials usable again — re-authenticating live chat sessions` with `platform=twitch` on the recovering pass, and an accepted-login line after it. This is the path `OnCredentialsChanged` cannot see: the fingerprint never moved.
3. **Validate stickiness over a real tick.** The same setup left running for one 30-minute cycle: the badge must NOT return to green on its own.
4. **Task 8 branch A only:** a capture on a genuinely expired `auth-token` with chat capture OFF, confirming the `playback-token-anonymous` mark.
5. **Recovery interaction.** With `auto_enabled = true`, confirm a chat downgrade produces exactly one browser recovery attempt, not one per refusal.
6. **The four newly-wired reload sites (Task 7a).** Each needs one observation, and none can be made offline: (a) let a Twitch job fail on an expired token with `auto_enabled` on and confirm the job-triggered refresh is followed by an auth re-check in the log; (b) let a recovery attempt run and FAIL, and confirm the re-check still runs (it is gated on `Ran`, not on success); (c) finish the setup wizard from each UI and confirm the badge updates without waiting for a tick; (d) leave the `auto_enabled` periodic timer to fire once and confirm the same — and this one is a NAMED RESIDUAL, not just a gate: `TestNotePassCompletedFiresTheHook` pins the seam, but nothing offline can pin the TICK's call to it, because that branch needs a browser profile, a browser and a network. A tick that ran a pass and did not fire the hook looks exactly like a tick that declined.

---

## Self-Review

Run against the spec with fresh eyes after the plan was written. Issues found were fixed inline; this section records what they were, so an executor knows which parts of the plan are corrections to the spec rather than transcriptions of it.

### 1. Spec coverage

| Spec requirement | Task |
|---|---|
| **R1** — every downgrade marks Twitch (`TwitchAuthenticated=false`, `TwitchVerification=RefreshFailed`, `TwitchError` = the route's operator sentence, no new payload key, no schema change) **and every existing surface reuses it, including the per-request reason rendering** | Task 2 (the write + the vocabulary), Task 6 (the four chat routes reach it), Task 8 branch A (the fifth route), Task 7 (the wiring, **and Steps 4a/4b — the two per-request renderers, which gated the reason on `RefreshUnknown` and so showed nothing for a marked platform; found by the Task 2 review, previously unowned**) |
| **R2** — the mark feeds automatic recovery through `shouldFireRecovery`'s dedupe; the per-job notice survives | Task 2 (`fireRecovery` + `noteRecoveryDecided`), Task 6 (both consumers of one report), Task 7 (the async delivery) |
| **R3** — the mark is sticky against a validate 200 | Task 2 (`twMarked` / `twEffective` in the status block; `TestValidate200DoesNotClearAStandingTwitchMark`) |
| **R4** — a credential-pair change clears it; `TwitchIdentity`; `OnCredentialsChanged("twitch")`; every reload site pinned | Task 1 (the fingerprint), Task 2 (the clear), Task 3 (the fire + the reload-site table + `TestCheckNowObservesATwitchCredentialChange`), **Task 7a** (the four sites that reached no pass at all) |
| **R5** — active chat sessions reconnect immediately; latches reset; no budget spent; a second refusal re-marks; a credentialed `001` logs at Info | Task 4 (`Reauthenticate`, the three latches, the accepted-login Info line), Task 5 (the registry), Task 7 (the broadcast, on **both** repair edges — `OnCredentialsChanged` AND `OnAuthRecovered`, since a transient refusal heals with the fingerprint unchanged and fires only the second; found by the Task 3 review), **Task 7a** ("immediately" is only true once every writer reaches a pass) |
| **R6** — one live probe first; build the HLS mark only if the reply states honoured-vs-anonymous, otherwise record and stop | Task 0 (the probe), Task 8 (branch A builds it; branch B records the finding in the ledger and stops), Task 9 (both variants of the three sentences either branch makes true) |
| **§3 non-goals** — no job-row indicator, no new UI state or REST key, no pilot change, no keepalive, no entitlement probe, no YouTube chat change | Global Constraints; no task touches `livenessRecoveryArmed`, `CookieStatus`, `TwitchAuthStatusPayload`'s keys, or `internal/chat` |
| **§4 invariants** — no credential in any log/notification/payload/error; `AuthStatus` writes under `rs.mu`; the sole-writer doc sentence changes with the code; `OnAuthDowngrade` still non-blocking; inline recover on every goroutine; anonymous logger stays anonymous; `UpdateJobFields` | Global Constraints; Task 2 (locked writes, literal-only sentences); Task 7 (the goroutine + recover that keeps `OnAuthDowngrade` non-blocking); Task 9 Step 2(d) (the sole-writer replacement) |
| **§5 docs** — `platform-services.md`, `data-and-storage.md`, `user-interfaces.md`, `operations.md`, `SPEC.md` | Task 9 ALONE, with the current sentence quoted for each — including the two sentences Task 7a falsifies and both variants of the three the Task 8 branch decides. No other task edits a spec file; Task 8 branch B records its finding in the ledger instead, so no sentence has two authors |

No spec requirement is unassigned.

### 2. Gaps found in the spec and fixed in the plan

Nine. Each is a place where following the spec literally would have shipped something broken or dishonest. Gaps 7-9 were found by the Fable review of the first draft (`.superpowers/sdd/2026-08-25-cookie-subsystem-remediation/arc10-plan-review.md`) and are recorded here rather than only there, because an executor reading this plan should not have to hold the review beside it.

1. **`TwitchIdentity` must NOT copy `YouTubeIdentity`'s rule.** R4 says "mirroring `YouTubeIdentity`", and `YouTubeIdentity` returns `""` when either half is missing. Copying that makes the fingerprint `""` for an `auth-token` with no `login` — which is the `no-login-cookie` downgrade state — so the operator's repair (adding the `login` row) would compare `"" != ""` and the mark would never clear. **R4 and R5 would both be dead for two of the four routes.** Task 1 diverges deliberately: `""` means "no Twitch credentials at all", and the divergence is pinned by `TestTwitchIdentityIsEmptyOnlyWithNoCredentialsAtAll`.

2. **`Reauthenticate` must reset THREE latches, not two.** R5 names `authRefused` and `downgradeReported`. But `noteMissingLogin` (`chat.go:344-366`) returns on `cd.warnedNoLogin.Swap(true)` **before** it reaches `reportAuthDowngrade`, so a still-broken cookie file would report nothing the second time — contradicting R5's own last sentence. Task 4 resets all three; `TestReauthenticateReReportsAMissingLoginCookie` is the pin.

3. **The mark must clear on the fingerprint ALONE, with no `nowAuth` gate.** R4's sources-of-change list reads naturally as "a change that works". Gating on authentication leaves a stale `no-login-cookie` sentence in front of an operator whose replacement token is revoked. Task 2 clears on the fingerprint and lets validate write the truth, which is what R4's own sentence ("validate decides the status again") says. Pinned by `TestAChangeToDeadCredentialsClearsTheMarkAndReportsTheTruth`.

4. **`refresh` needs ONE `twEffective`, not a mark check at five sites.** The spec says the mark wins for `TwitchAuthenticated`/`TwitchError`. It is silent on `prevTwitchAuth`, `shouldFireRecovery`, `OnAuthRecovered` and the identity baseline — and the one that bites is `OnAuthRecovered`: with the mark standing and `prevTwitchAuth` false, a validate 200 would announce "twitch auth recovered" and resume every parked Twitch job into the same failure, once per mark. Task 2 routes all five through one value.

5. **`Reauthenticate`'s own cancel must not be read as a refusal.** Nothing in the spec mentions it. `runIRCSession`'s deferred `noteHandshakeOutcome` sees `welcomed=false, heardFromServer=true` for a session cancelled between the CAP ACK and the `001` — indistinguishable from `login-never-acknowledged`. Without the `reauthPending` guard the feature INVERTS: the reconnect it asked for comes back anonymous and reports a spurious downgrade. Task 4 adds the guard; `TestReauthenticateDoesNotLatchTheFallbackOnItsOwnCancel` is the pin.

6. **`SPEC.md` § Authentication is the wrong section.** Spec §5 names it, but that section is about DASHBOARD auth and says so in its first line ("It is unrelated to the platform credentials in § Cookies"). Task 9 Step 5 edits **§ Cookies** (`SPEC.md:666-680`) instead.

7. **R4's "each reload site is followed by a `CheckNow`" is not true of the code.** Spec R4 says so as a statement of fact and the first draft repeated it. Four sites reach no pass at all: the worker's job-triggered browser refresh (`services.go:902`), recovery's `RefreshFailed`/`RefreshUnknown` arms, both setup-wizard finishes, and the `auto_enabled` periodic timer. The first is the sharp one — a job's own auth-failure refresh succeeds and every OTHER live Twitch capture stays anonymous for up to thirty minutes, which is precisely what R5's "immediately" rules out. **Task 7a** closes all four; Task 3's table now marks each site with whether it reaches a pass today.

8. **R5's "a credentialed `001` logs at Info" describes code that does not exist.** `chat_irc.go` sets `welcomed = true` silently; the only Info on that path is "joined twitch IRC", written before any reply and for anonymous sessions too. Without it the operator's confirmation that a repair worked is the ABSENCE of a downgrade report, which is not evidence. Task 4 Step 5 adds the line and `TestReauthenticateSpendsNoReconnectBudget` counts it.

9. **The `warnedNoLogin` reset (gap 2) has a mirror in the code's own contract comments.** Five `chat.go` doc comments say "per downloader" or "per job" about latches that are now per credential pair, including `noteHandshakeOutcome`'s "a cookie repaired mid-job does not re-authenticate chat until the next job" — the exact sentence this arc exists to falsify. A stale contract comment is worse than a stale spec sentence, because it is what a reader checks before touching the latch. Task 4 Step 3a corrects all five; Task 6 corrects the sixth, on the worker side.

### 3. Where the file map disagreed with the code

Three, all minor; the plan uses the code.

- **The "sole writer of `rs.status`" sentence does not exist in any document.** The file map attributes it to `refresh.go:1150-1242`, and spec §4 asks for "the doc sentence that states sole-writer" to change. Neither `refresh.go` nor `data-and-storage.md` contains such a sentence — `data-and-storage.md:792`'s "the sole writer" is about the COOKIE FILE (`checkAndRefreshYouTube` / `processYouTubeSetCookies`) and must not be touched. Task 9 Step 2(d) therefore ADDS the two-writers paragraph after the `AuthStatus` paragraph (`:857`) rather than editing a sentence that is not there.
- **`GetHLSMasterPlaylist`'s call site is `stream_processor_twitch.go:443`, and the file map says both `:443` and (in §1) "`:443`" against a design note that said `~:410`.** Verified: `variants, err := sp.tw.GetHLSMasterPlaylist(ctx, login)` is at `:443`. No plan change; noted because §1 and §5 of the map cite it twice.
- **The dashboard shift+click and the Settings button were cited at the wrong file.** The first draft put them beside `R F` at `cmd/moombox/tui_wiring.go:434`. They actually go through `POST /api/cookies/auto-refresh` — `internal/web/routes/cookies.go:314` `RefreshCookiesDetailed` → `:399` `refreshSvc.CheckNow(req.Context())`. The conclusion (a `CheckNow` follows) held; the file did not. Task 3's table now lists them as separate rows, and `R F` is at `:435`, not `:434`.
- **`docs/spec/platform-services.md`'s "A job with chat recording off" paragraph is at `:529`, not `:530`** (`:530` is blank). Corrected in Task 8 branch B's Files list and Task 9 Step 1(c) — both of which are by-number edits, so the drift mattered.
- **`Start`'s reconnect loop returns on `err == nil`, so `chat_irc.go`'s `RECONNECT` handler does NOT do what its comment claims.** The comment at `chat_irc.go:322-327` says *"Return nil so the outer loop does a clean reconnect without incrementing the error counter"*, but `chat.go`'s loop reads `if err == nil || ctx.Err() != nil || !cd.IsRunning() { return nil }` — a `nil` return ends `Start` entirely, so a Twitch-initiated `RECONNECT` silently ends chat capture for the job unless a connectivity outage later relaunches it. **This is a pre-existing latent bug, out of Arc 10's scope, and the plan deliberately does not fix it** (a `continue` there would need its own budget decision and its own test). It is recorded here because Task 4 edits that exact loop and an executor will read the stale comment: do not "fix" it in passing, and do not model `Reauthenticate` on it — that is precisely why Task 4 uses `reauthPending` rather than a `nil` return. **Carry this to the Arc 12 planning list.**

### 3a. What the pre-flight conflict scan changed

A pairwise Produces/Consumes and per-task self-consistency scan
(`.superpowers/sdd/2026-08-25-cookie-subsystem-remediation/arc10-preflight-scan.md`) found ten
items. Two were compile- or artifact-breaking and are worth carrying here rather than only in
the scan:

- **Task 6 would not have compiled.** `stream_processor_twitch_credentials_test.go` calls
  `twitchChatDowngradeCallback` at THREE sites (`:327`, `:356`, `:386`), not the two the step
  named. The missed one, inside `TestTwitchChatDowngradeWithoutANotifierIsSilent`, is the only
  test covering the no-notifier install and does not carry "Callback" in its name. Step 5 now
  lists all three with their line numbers and says why that one is easy to miss.
- **Task 8 branch B and Task 9 would have edited the same three sentences.** Branch B pulled
  Task 9's `data-and-storage.md` paragraph forward into its own commit, and appended to a
  `SPEC.md` sentence Task 9 replaces — a duplicated paragraph and two near-identical "since Arc
  10" claims. Resolved by ruling Task 9 the SOLE doc-editing task: branch B now records its
  finding in the ledger, and Task 9 carries both variants of every HLS-side sentence, selecting
  by the same Task 0 report.

The other eight were declaration defects rather than behaviour defects — an undercounted test
count, an undercounted Files block, an incomplete dependency line, two inaccurate Interfaces
manifests, a duplicated code block with no strike-through, and the "re-derive by symbol" caution
being attached to one task when three need it. All are fixed in place.

### 4. Placeholder scan

No "TBD", no "implement later", no "add appropriate error handling", no "similar to Task N", no "write tests for the above". Every code step carries the actual code. Task 8's two branches are both written in full, with the branch condition stated as a check against a named artefact (Task 0's ledger entry) rather than as a decision deferred to the executor's judgement.

Re-run after the review edits: the same scan over the changed sections (Task 3's table, Task 4 Steps 3a/4/5, Task 5's registry tests, Task 7a in full, Task 8A, Task 9 Steps 4a/5) finds nothing. Task 7a's "re-derive by symbol, not by number" instruction is a warning about drift, not a deferred decision — every symbol it names is listed.

The one place the plan cannot be fully concrete is Task 8 branch A's key name (`user_id`), which depends on a measurement that has not been taken. It is handled by naming the expected key, stating the substitution rule in one sentence, and giving the branch-B exit for the case where no such key exists — so the executor is never asked to invent anything.

### 5. Type consistency

Checked across tasks:

- `TwitchIdentity() string` — defined Task 1, called in Task 2 (`rs.jar.TwitchIdentity()`), Task 3 (status block), cited in Task 7's comment. One name throughout.
- `NoteTwitchAuthLoss(reason string)` — defined Task 2, called in Task 6's test helper, Task 7's seam, Task 8 branch A's wiring. No `NoteTwitchAuthLost` / `MarkTwitchAuthLoss` variant anywhere.
- `twitchLoss*` constants (cookies, unexported) vs `twitch.AuthDowngrade*` (exported) — two vocabularies with identical VALUES, kept apart because the import graph forbids one. The pin is `TestTwitchAuthLossVocabularyCoversEveryDowngradeReason` in `internal/worker`, the only package importing both. Task 8 branch A adds a member to both and extends both enumerating tests.
- `Reauthenticate()` — defined Task 4 on `*twitch.ChatDownloader`, consumed Task 5 through `twitchChatReauthenticator`, reached Task 7 through `ReauthenticateTwitchChats() int`. Three names, three layers, no overlap.
- `twitchChatRegistry.add(cd) (remove func())` / `reauthenticateAll() int` — Task 5 only; `DownloadWorker.twitchChats` and `DownloadOrchestrator.twitchChats` are the same field name for the same shared pointer, pinned by `TestOrchestratorAndWorkerShareOneRegistry`.
- `SetOnTwitchAuthLoss(fn func(reason string))` — the same signature on `StreamProcessor` (Task 6) and `DownloadWorker` (Task 6, forwarding); the resolver is `twitchAuthLossReporter() func(reason string)`, matching `twitchChatDowngradeCallback`'s `resolveMark func() func(reason string)` parameter exactly.
- `reauthenticateTwitchChats(platform string, broadcast func() int) int` — Task 7 only. Not to be confused with `twitchChatRegistry.reauthenticateAll`; different packages, different arities.
- `PlaybackTokenSession(value string) (signedIn, conclusive bool)` — Task 8 branch A only, and its `(bool, bool)` return matches the `FallbackLiveness` convention already in the codebase (`loggedIn, conclusive`), deliberately. `playbackTokenReportsAnonymous(authToken, tokenValue string) bool` wraps it and is the whole call-site decision, so the test drives production code rather than a copy of the condition.
- `recheckAfterCookieWrite(ctx, checkNow func(context.Context) bool, log <anonymous 4-method>, gesture string, args ...any) bool` (Task 7a, `cmd/moombox`) — five call sites, all passing `s.cookieRefresh.CheckNow` as a method value. Not to be confused with `AutoCookieService.OnPassCompleted func()` / `notePassCompleted()` (Task 7a, `internal/cookies`), which is the injected seam for the one caller that lives inside that package; different packages, different arities, and the seam's only job is to reach the helper.
- `acceptedLoginRecorder` (Task 4's test file) is deliberately NOT an extension of `recordingLogger` (`chat_irc_fallback_test.go:142`): that recorder captures Warn only and every fallback test in that file counts its entries exactly, so widening it would change what those counts mean.
- `fakeChat.reentrant func()` (Task 5's test file) exists only for `TestBroadcastDoesNotHoldTheRegistryLock`; the other five registry tests leave it nil and are unaffected.

### 6. Ordering and parallelism

Tasks 1 → 2 → 3 are strictly sequential and all edit `internal/cookies/refresh.go`. Tasks 4 → 5 → 6 → 7 → 7a are strictly sequential. Task 0 is independent and should run FIRST (its result gates Task 8, and it needs an operator with a live channel, which may take a day). **Task 8 needs Tasks 0, 2, 6 and 7** — not just 0 and 2, as an earlier draft said: branch A's Step 5 extends a test file Task 6 creates, and its Step 6 anchors to a block Task 7 lands. Task 9 needs everything, Task 7a and Task 0's report included — two of its sentences describe Task 7a's behaviour, and three of them have two variants selected by the report.

Task 7a comes after Task 7 rather than before it because both edit `cmd/moombox/services.go` within twenty lines of each other, and because 7a's helper is only worth reviewing once the mark and the broadcast it feeds exist. Its line-number citations were taken before Task 7 ran and it says so: re-derive by symbol.

The two chains (1-3 and 4-6) touch disjoint packages and could be executed in parallel by two agents, but Task 7 joins them and Task 9 rewrites sentences about both, so the review overhead is unlikely to pay. Execute in the listed order unless the executor has a reason not to.

---

## Final state (arc-close review, 2026-09-01 — the tree at `13a60eb`)

This section is the record of what was BUILT, written at the arc-close review after every task closed. It does not rewrite the tasks above; where a task's text and this section disagree, this section is the tree. Plan line numbers below are at `13a60eb` (the pointer under the title shifts everything after it by two).

### What each task delivered

| Task | Landed as | Delivered |
|------|-----------|-----------|
| plan | `2de37fc`; amended `86a867a` (pre-flight scan: Task 6's third `twitchChatDowngradeCallback` caller, counts, deps), `97f721b` (Task 7 gains the two per-request reason renderers, Steps 4a/4b), `d5c4a1c` (Task 7 broadcasts on BOTH repair edges via `wireCredentialRepairCallbacks`) | — |
| 0 | `4692284` (+ controller `9351922`: `differingFields`, the `-count=1` note) | `TestLivePlaybackTokenShape` (`internal/twitch/playback_token_live_test.go`), run once: `user_id` is a JSON number when authenticated and `null` when anonymous, all 32 top-level keys present in both replies, `Value` is raw JSON. **Branch A.** |
| 1 | `b4b9a11` | `CookieJar.TwitchIdentity()` — SHA-256 over `auth-token + NUL + login`; `""` only when the jar holds neither half (deliberately NOT `YouTubeIdentity`'s rule). 7 tests, incl. the implementer's `TestTwitchIdentityIgnoresCoexistingYouTubeCookies` (the brief's wrong-map mutation was blind to its own test). |
| 2 | `b12f055`; fix1 `9e9b578`; controller `f0aac07` | `RefreshService.NoteTwitchAuthLoss(reason)` — the SECOND writer of `rs.status` (under `rs.mu`; the mark wins for the Twitch triple until the fingerprint changes); the five-member `twitchLoss*` vocabulary and `twitchAuthLossMessage` (every arm a string literal — the leak barrier); `OnAuthChange` through `authStatusChanged`; `OnRecoveryNeeded("twitch")` through `shouldFireRecovery`'s dedupe with `prevTwitchAuth`/`twEverConcluded` advanced and `hasCheckedOnce` untouched; the Warn carries the mapped sentence only. 10 tests. |
| 3 | `cb8a7a9` | `prevTwitchIdentity`; `twEffective = twAuth && !twMarked` feeding all five sites (status, `prevTwitchAuth`, `shouldFireRecovery`, `OnAuthRecovered`, `advanceIdentityBaseline`); `OnCredentialsChanged("twitch", identity)` on the same two pure rules as YouTube. `cmd/moombox` needed no change (`sweepShouldResume` gates on `job.Platform`). 6 tests, incl. `TestAStandingTwitchMarkFiresNoCredentialChange`. |
| 4 | `254b8e9`; fix1 `602c031` | `ChatDownloader.Reauthenticate()` — three latches (`authRefused`, `downgradeReported`, `warnedNoLogin`) reset INSIDE `cd.mu`, `reauthPending` armed only when a session exists, `sessionCancel` fired after the unlock; Start's reauth branch: `cd.flush()`, the three latches re-cleared (a verdict from the dying session cannot re-latch), the stability reset, an `immediate` reconnect with nothing charged to the budget; `runIRCSession` ends on the first failed read and its handshake defer refuses to read our own cancel as a refusal; a credentialed `001` logs "twitch chat: authenticated login accepted" once per session. 7 tests. |
| 5 | `0778ded`; fix1 `e6fc403` | `twitchChatRegistry` (`add` returns the remove closure; `reauthenticateAll` snapshots under the lock and calls outside it; the count is downloaders TOLD); ONE registry shared by `DownloadOrchestrator` and `DownloadWorker` (`NewDownloadWorker`); `ExecuteTwitch` registers the live IRC downloader for its whole run via a function-scoped `defer`; `DownloadWorker.ReauthenticateTwitchChats() int` (nil-safe). 9 tests, incl. the reviewer's `TestExecuteTwitchRegistersTheLiveChatDownloaderForItsWholeRun` harness. |
| 6 | `e000118` | `StreamProcessor.onTwitchAuthLoss` + `SetOnTwitchAuthLoss` (forwarded by `DownloadWorker.SetOnTwitchAuthLoss`); `twitchAuthLossReporter()` resolved at FIRE time; `twitchChatDowngradeCallback(resolveSend, resolveMark, job, channel)` marks first, then notifies; `internal/worker` still imports `internal/cookies` only in tests, which is where the two vocabularies are pinned (`TestTwitchAuthLossVocabularyCoversEveryDowngradeReason`). 11 tests. |
| 7 | `9f02558`; fix1 `445b6b7` | `cmd/moombox`: `twitchAuthLossHook` (the recover-guarded goroutine in front of `NoteTwitchAuthLoss` — the ONLY asynchronous hop between the IRC read loop and the mark); `wireCredentialRepairCallbacks(broadcast)` installing `OnAuthRecovered` AND `OnCredentialsChanged`, both calling `reauthenticateTwitchChats(platform, broadcast)` (equality-gated on `"twitch"`) inline before the parked-job sweep; the notification sentence "after re-checking the saved credentials"; the two per-request reason renderers widened from `RefreshUnknown` to "any non-empty reason" (`cookieRecheckFeedback`, `cookieIndicatorState`) — which also surfaces the pre-existing unsignable-jar sentinel; the push-driven bars UNCHANGED. 8 new + 4 amended tests. |
| 7a | `615500d`; fix1 `7b52939`; fix2 `d32e467`; controller `2f8a8d4` | `recheckAfterCookieWrite(ctx, checkNow, log, gesture, args...)` + `runState.checkNowFn()` (nil-safe accessor); `Ran`-gated `defer`s at the recovery (all three verdicts), the worker's `OnCookieRefreshNeeded` and TUI `R F`; `SetupResult.Wrote` gating both wizard finishes (TUI, and the Web finish on a DETACHED 45 s context with an `http.Flusher` flush first); `AutoCookieService.OnPassCompleted` + `notePassCompleted()`, fired `Ran`-gated by the periodic tick AND `StartProfileSeed` (the row the plan missed), wrapped in `postRefreshRecheckHook`'s own recover; two AST structural pins. The 16-row reload-site table is in `task-7a-review.md`. |
| 8 | `04197f2`; fix1 `b2f4f58`; controller `33e10ea` | Branch A. `PlaybackTokenSession(value) (signedIn, conclusive)` — `user_id` JSON `null` → anonymous, absent key or undecodable → inconclusive, any other type → signed-in; `playbackTokenReportsAnonymous(authToken, value)` (the whole decision); `GetHLSMasterPlaylist` returns `(variants, anonymousPlayback, err)` reading the auth token ONCE; `StreamProcessor.noteAnonymousPlayback` marks `playback-token-anonymous` through the SAME seam as the chat route, BEFORE the error check, with no notification; `FetchVariantsFn` discards the verdict on every downloader (re)start (one verdict per capture START); the fifth member in both vocabularies; `twitchChatDowngradeNotice` carries a SCOPE note (its payload is wrong for the fifth route in three clauses — do not widen it). 11 tests, incl. the RoundTripper stub over `twitchHTTPClient`. |
| 9 | `d10f41a`; fix1 `527aab6`; controller `f5898f9`, `13a60eb` | The six docs: `SPEC.md` § Cookies; `architecture.md`'s `OnCredentialsChanged` sentence; `data-and-storage.md` (`TwitchIdentity`, the single-flight paragraph's reload-site enumeration, `OnCredentialsChanged` for both platforms, the new "two writers, mark wins" paragraph); `operations.md`'s three notification rows and the cooldown paragraph; `platform-services.md` § Anonymous Fallback and the Downgrade Report, rewritten around the credential pair and the playback-token detector; `user-interfaces.md`'s four former `RefreshUnknown` sites. Go comments that counted "four" sentences now say five. |

### Coordinator-directed deviation from the plan (Task 8) — SUPERSEDED LINES

`Service.OnAnonymousPlayback` was **NOT built and must not be added later.** The Task 8 dispatch directed the implementer to route the HLS mark through the SAME fire-time resolver seam as the chat downgrade, with nothing in `cmd/moombox` changing; the Task 8 review (`task-8-review.md`, F5) confirmed this was coordinator-directed, not drift, and accepted it on merits (one seam / one recover; per-site control; a testable named method; fire-time resolution; the Warn fires even unwired). What the tree has instead: `GetHLSMasterPlaylist` RETURNS `anonymousPlayback`; `StreamProcessor.noteAnonymousPlayback` calls `sp.twitchAuthLossReporter()`; `FetchVariantsFn` discards the verdict.

Plan lines superseded by that (at `13a60eb`): `:3297` (Task 7 §3, "Task 8 branch A … wires `twService.OnAnonymousPlayback` here too"); `:4470` (Task 8 Files, "wire `OnAnonymousPlayback` beside `SetOnTwitchAuthLoss`"); `:4475` (Task 8 Interfaces, "Produces … `Service.OnAnonymousPlayback func()`"); A Step 4 `:4677-4738` (the `OnAnonymousPlayback` field at `:4694-4705` and the `GetHLSMasterPlaylist` rewrite that calls it at `:4725-4731`); A Step 6 `:4756-4785` (the `cmd/moombox` wiring at `:4772`). The branch-B section `:4849-4920` was NOT taken (Task 0 decided branch A) and its ledger-only ruling was never written.

Two further post-plan facts, both already in the tasks above by amendment: Task 7a exists because the plan review found four of eight reload sites unpinned (its `StartProfileSeed` row 9 was found only in execution, by the Task 7a review); Task 3's `twEffective` baseline rationale in the brief was wrong and the implementer's coherent choice stands (`TestAStandingTwitchMarkFiresNoCredentialChange` encodes it).

### Residuals — named, not hidden

Each is accepted by a recorded ruling in the ledger (`.superpowers/sdd/2026-08-29-arc10-twitch-credential-lifecycle/progress.md`, gitignored); none is presented as done anywhere.

1. **Task 4 F1a** — the lock-ordering half of the reauth reset (the three `Store`s INSIDE `cd.mu`, closing the review's Sequence B) has no deterministic test; a test holding `cd.mu` blocks the event it stages. Only the re-clear in Start's reauth branch (Sequence A) is pinned (`TestReauthenticateSurvivesAVerdictThatLandsAfterTheArm`); reverting BOTH halves is caught.
2. **Task 4 F6** — the stability reset on the reauth `continue` is uncovered both ways (a deletion mutant needs a session of at least five minutes). Cost if wrong: a healthy job carries a stale `reconnectAttempts` across a repair, bounded by `reconnectResetUptime`.
3. **Task 4 M9** — deleting the read-loop `reauthPending` short-circuit in `runIRCSession` is uncaught: no observable consequence (twenty instant failed reads, the same decision).
4. **The `cd.flush()` before the reauth reconnect** is pinned only as the barrier of one test (it is what parks Start's branch in `TestReauthenticateSurvivesAVerdictThatLandsAfterTheArm`); deleting it is not caught. Arc-close observation.
5. **Task 2 F10** — a mark taken on a jar holding NO Twitch credentials writes an inert failed verdict (every surface takes its not-configured arm first); stated in `twitchAuthMark`'s doc comment.
6. **The mark is process-local** — a restart drops it, and validate reports Twitch green for the two missing-`login` routes until the next handshake re-takes it; stated in `data-and-storage.md` § Refresh Service ("two writers").
7. **`noteRecoveryDecided("twitch", …)` in the mark's fire path is unasserted** while the liveness pilot is disarmed (`livenessRecoveryArmed = false`); arming pins it — the ARMING checklist in `progress-arc8.md` carries the line.
8. **The periodic tick's `Ran` gate has no browser-free test in the FIRE direction** — field gate 6(d) above; the AST tests (`TestNotePassCompletedHasExactlyItsTwoWritingCallers`, `TestNotePassCompletedIsGatedOnRanAtEverySite`) pin the call and its gate structurally, and `TestNotePassCompletedFiresTheHook` pins the seam.
9. **Task 7a R5** — four of the five deferred re-check sites (the worker's `OnCookieRefreshNeeded`, TUI `R F`, both wizard finishes) are pinned by reading only; the recovery site is pinned (`TestRecoveryRechecksExactlyWhenThePassRan`). The cheap later fix is a `cmd/moombox` AST test asserting each `recheckAfterCookieWrite` call sits in a `*ast.DeferStmt`.
10. **Task 7a R6** — `SetupResult{Wrote: true}` on the post-write `jar.Load` error exit is read-verified (`autocookies.go:1273` at `13a60eb`); the success exit and two no-write exits are pinned (`TestSetupResultWroteReportsWhetherTheFileWasReplaced`).
11. **Task 7a R7** — the recovery re-check also fires on the S9 unreadable-file path (one wasted validate pass on a rare error path); accepted as the price of the `Ran` over-approximation.
12. **The Web wizard's detached re-check still runs on the request goroutine** — the `http.Flusher` flush commits the response first, so the browser's 60 s abort no longer cancels a setup that succeeded; the handler itself returns only after the pass (bounded by 45 s). Field gate 6(c).
13. **Three `initServices` / `wireMonitorCallbacks` joins are compile-verified only** — `dlWorker.SetOnTwitchAuthLoss(twitchAuthLossHook(...))`, `autoCookieSvc.OnPassCompleted = postRefreshRecheckHook(...)`, `s.wireCredentialRepairCallbacks(s.dlWorker.ReauthenticateTwitchChats)`; each extracted function is tested. Likewise `DownloadWorker.SetOnTwitchAuthLoss`'s one-hop forward, `processTwitchLive`'s two call sites (`OnAuthDowngrade: twitchChatDowngradeCallback(sp.notifierSend, sp.twitchAuthLossReporter, …)`; `sp.noteAnonymousPlayback(anonymousPlayback)` before the error check) and `FetchVariantsFn`'s discard — `processTwitchLive` is unreachable offline.
14. **The notification sentence** "after re-checking the saved credentials" is untested (`notifications.sender` is unexported).
15. **Task 7 F6** — the broadcast-before-sweep order inside both repair closures is not load-bearing and deliberately untested (a test would pin an accident).
16. **Registration spans the whole `ExecuteTwitch` run** — a credential change during muxing calls `Reauthenticate` on a stopped downloader, which clears latches and requests nothing (harmless; narrowing rejected).
17. **The dead-token arm of the HLS detector is INFERRED, not observed** — a real `auth-token` expiry may fail earlier inside the GQL call as `ErrTwitchAuthExpired` rather than yield an anonymous token; the Task 0 probe observed only the anonymous-by-design reply. Field gate 4. The signal is anonymous-vs-signed-in, never entitlement: a signed-in token that cannot fetch subscriber-only content still reads signed-in.

### Field gates

§ "Field gates" above stands as written (1, 2, 2a, 3, 4, 5, 6a-d); the arc-close adds none and verifies none. Nothing in the six docs describes any of them as verified.

### Arc-close review (2026-09-01, `arc10-arc-close-review.md`, gitignored beside the ledger)

Verdict **MERGE AFTER NAMED EDITS**. Whole-chain trace: 30 arrows, 20 pinned by test, 10 by reading (all ten are in residuals 1, 2, 4 and 13 above). `-race` clean on the six arc packages in a `%TEMP%` worktree of `13a60eb`. Six findings, all docs/comments, no code behaviour: (F1) `SPEC.md` § Twitch IRC still says a refused session "falls back to anonymous once, for the rest of the job" — false since R5; (F2) `user-interfaces.md` § Cookies still says the downgrade is "a state neither badge can show", "one notification per job" and a "four-value vocabulary"; (F3) `platform-services.md`'s playback-token paragraph omits the never-entitlement / inferred-dead-arm limits (residual 17); (F4) `data-and-storage.md` cites `refresh.go:632` for `livenessRecoveryArmed`, which this arc moved to `:748`; (F5) `operations.md`'s "sole producer of a recovery is `shouldFireRecovery`" (and `handleRecoveryNeeded`'s "exactly one producer" comment) should name `NoteTwitchAuthLoss` as the second caller of that gate; (F6) two `refresh.go` comments still speak in the future tense about Tasks 7 and 7a. F1 and F2 are the named edits; F3-F6 ride in the same commit.
