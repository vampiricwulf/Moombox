# Arc 12b — Twitch Tier-2 Entitlement Probe Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give Twitch the tier-2 liveness producer it has never had — a playback-access-token request that answers "is this stored session still signed in" — wired through a `TwitchFallbackLiveness` twin of YouTube's existing `FallbackLiveness` seam, feeding `ObserveLiveness("twitch", …)` under the same disarmed pilot gate.

**Architecture:** Three layers, no new mechanism at any of them. (1) `internal/twitch` gains ONE method, `Service.ProbeSessionLiveness`, over the request `GetHLSMasterPlaylist` already makes (`API.GetStreamAccessToken`), classified by the function Arc 10 already wrote (`PlaybackTokenSession`). (2) `internal/cookies.RefreshService` gains one plain func field beside `FallbackLiveness` and one call site beside YouTube's in `doRefresh`, under the same three conditions. (3) `cmd/moombox` builds the closure that joins them, because `internal/twitch` imports `internal/cookies` (`service.go:7`, `auth.go:13`) so the reverse import is impossible — the same inversion `FallbackLiveness`, `VerifyYouTubeAuth` and `BrowserLaunchAllowed` already use. The probe is an OBSERVATION producer only: `const livenessRecoveryArmed = false` stays false, so what lands is the observation, the dedupe, the freshness accounting and the log line — never a notification.

**Tech Stack:** Go 1.26. `internal/twitch` (GQL over `twitchHTTPClient`), `internal/cookies` (`RefreshService`), `cmd/moombox` (wiring), `internal/config` (`Store.Read`). No new dependency, no config key, no REST route, no database change, no goroutine.

**Spec:** `docs/superpowers/specs/2026-09-02-arc12b-twitch-entitlement-probe-design.md` (R1-R5, non-goals, invariants)

## Global Constraints

Every task's requirements implicitly include this section.

- **`const livenessRecoveryArmed = false` (`internal/cookies/refresh.go:748`) stays false.** The probe inherits the pilot's gate. No task here reads it except to assert it in a test, and none flips it.
- **`internal/cookies` never imports `internal/twitch`.** `internal/twitch/service.go:7` and `internal/twitch/auth.go:13` import `internal/cookies`; the reverse is an import cycle. Every probe call crosses that boundary as an injected closure built in `cmd/moombox`. A task that finds itself adding `"github.com/vampiricwulf/Moombox/internal/twitch"` to a file under `internal/cookies/` has gone wrong.
- **No token value, ever.** The playback access token's `Value` is a signed entitlement document carrying a device id and a user ip; the `auth-token` is a bearer credential. Neither may reach a log line, an error string, a returned value, a test assertion or a notification. Error strings that interpolate a response body are the live hazard here — `gqlRequest` embeds `string(respData)` on 4xx/5xx and an intermediary's error page can echo the `Authorization` header (the exact hazard `validateErrorDetail` names in `internal/twitch/auth.go`). Task 1 sanitises for that reason and Task 1 Step 5 tests it. (Known and OUT OF SCOPE: `gqlRequest`'s retry line `a.logger.Debug("twitch gql retry", …, "prev_err", lastErr)` at `api.go:196` already writes a 5xx/429 body at Debug for EVERY GQL caller — the monitor trips it every 15 s. This arc adds one more caller, not the hazard; do not fix it in passing here — carry it to the worklist.)
- **The two drafts are committed at the paths this plan cites** — `docs/superpowers/specs/2026-09-02-arc12b-twitch-entitlement-probe-design.md` and `docs/superpowers/plans/2026-09-02-arc12b-twitch-entitlement-probe.md` — as the branch's FIRST commit (the A1-Linux precedent: `docs/superpowers/specs/2026-09-02-a1-linux-process-group-reap.md`), so Task 4's bullet points at a file that exists. Neither exists today; both are gitignored drafts under `.superpowers/sdd/`.
- **Never read, print, or assert on a real cookie file or a cookie value.** No task opens `D:\Moombox\cookies.txt`, `cookies.sqlite`, or a browser profile. Offline fixtures use the package's obviously-fake strings (`fixture-auth-token`, `test-token-aaaa`). Task 0 is the ONE live task and it takes its cookie file from an env var, never printing the path's contents — only field NAMES, JSON TYPES and booleans.
- **Logger interface stays anonymous.** Any struct needing a logger repeats the four-method anonymous interface inline (`Debug`/`Info`/`Warn`/`Error`, each `(msg string, args ...any)`). Do not extract a named interface.
- **Every goroutine gets an inline `defer func() { if r := recover(); ... }()`.** No task here starts a goroutine — the probe runs inline on `doRefresh`'s ticker goroutine, which already carries `Start`'s recover. If an implementer reaches for `go func()`, the design has drifted.
- **`cmd/moombox/main.go:276-278` is no-touch** (the `SetExpectedPlatforms` seeding gated on `cookies.auto_enabled`).
- **LF line endings** on every file created or edited.
- **Gates, per task, before the commit:**
  ```bash
  go build ./...
  go vet ./...
  GOOS=linux GOARCH=amd64 go build ./...
  gofmt -l internal/ cmd/          # must print nothing
  go test -count=1 ./...           # once, whole tree
  ```
- **Commit trailers** on every commit in this plan:
  ```
  Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
  ```
- **Docs are edited in the task that makes them true**, never in a trailing docs task. `data-and-storage.md:838` and `platform-services.md` § Twitch flip in Task 3 (the task that creates the producer); the field-gate and remediation-plan bookkeeping is Task 4's own deliverable.
- **Every test assertion names the mutation it closes.** A comment on the test (or the failure message) says what edit to the production code the assertion catches. An assertion that survives every plausible mutation of the line it is about is not a test.

---

## The one thing that decides Task 3: Task 0's finding

R2 says the probe targets a configured Twitch channel whether or not it is live. That rests on an unmeasured premise: that `streamPlaybackAccessToken` answers for an OFFLINE channel. Arc 10's Task 0 measured the LIVE case only (`internal/twitch/playback_token_live_test.go` requires `MOOMBOX_LIVE_TWITCH_CHANNEL` to be "a channel that is LIVE right now", and its own comment says "an offline channel yields no stream playback token at all" — a claim recorded there without a measurement behind it).

Task 0 measures it. Task 3 then takes one of two branches, both written out in full:

- **BRANCH A** — the authenticated reply for an OFFLINE channel decodes and carries a non-null `user_id`. `pickTwitchProbeChannel` returns the first enabled configured Twitch login, and the probe costs ONE GQL request per tick (R4 as written).
- **BRANCH B** — the offline reply errors, or carries no `user_id`. `pickTwitchProbeChannel` first asks `Service.GetStreamInfoBatch` (the same batched request `TwitchMonitor` makes every 15 s — `internal/monitor/twitch.go`, `api.go:493-522`, where a nil info in the returned slice means offline) for the configured logins and returns the first LIVE one, or "" when none is live. That costs TWO GQL requests per tick instead of one, and an install whose channels are all offline gets an inconclusive probe rather than a wrong verdict.

Note for branch B, recorded so it is not re-derived: `TwitchMonitor` keeps **no** exposed liveness state. Its fields are `checking`, `pendingKick`, `warnedSlow`, `timer`, `NextCheckAt` and a `healthTracker` that counts consecutive failures; `OnStreamFound` fires and nothing is retained. `database.Job` has `Platform` and `ChannelName` but no channel login, so the job table cannot be joined back to a configured channel either. Re-issuing the monitor's own batched query is therefore the cheapest correct source, and it adds no state.

**The default, so Task 0 never blocks execution: BRANCH B.** Task 0 is a live measurement the owner runs, and it may not have been run when the implementer reaches Task 3. Branch B is the branch whose every premise is already measured — Arc 10's live probe confirmed `user_id` discriminates on a LIVE channel, and branch B only ever probes a live one. Branch A rests on the unmeasured premise, and the way it fails if that premise is wrong is the one direction this arc must not fail in: an offline channel that mints an anonymous-looking token even for a signed-in session would produce a CONCLUSIVE `loggedIn=false` every tick — filed under `twitch` in the liveness record as evidence for the pilot, and, on the day the pilot is armed, a recovery fired at credentials that were never wrong. Branch B's failure mode is bounded and observation-only: one extra GQL request per tick, and an inconclusive probe while every configured channel is offline. So: no `FINDING:` line → branch B, and the task report says "Task 0 unrun; branch B by default". A later Task 0 run that logs BRANCH A moves the picker to the branch-A form in its own commit (the branch-A code below is complete for that purpose); a run that logs BRANCH B confirms the default and changes nothing.

---

## Task 0: Measure whether an offline channel answers

**Files:**
- Create: `internal/twitch/playback_token_offline_live_test.go`

**Interfaces:**
- Consumes: `NewAuth`, `NewAPI`, `API.GetStreamAccessToken`, `PlaybackTokenSession` (all `internal/twitch`, all existing); `describePlaybackToken`, `jsonKindOf`, `reportKeyDifference`, `nopLogger` (same package, `playback_token_live_test.go` / `auth_test.go`).
- Produces: a FINDING logged by the test, naming **BRANCH A** or **BRANCH B** for Task 3. No production symbol.

**Design notes.**

The probe is gated by THREE env vars, and the arming one is separate from the credential one on purpose: `MOOMBOX_LIVE_TWITCH_PROBE=1` says "run Arc 12b's measurement", `MOOMBOX_LIVE_TWITCH_COOKIES` supplies the credential, `MOOMBOX_LIVE_TWITCH_OFFLINE_CHANNEL` supplies the subject. The existing Arc 10 probe keys on the cookie path alone; adding the explicit arming var here means setting `MOOMBOX_LIVE_TWITCH_COOKIES` for that older test does not silently also fire this one against a channel var it would misread.

The channel var is deliberately NOT `MOOMBOX_LIVE_TWITCH_CHANNEL`: that one is documented as "a channel that is LIVE right now" and this measurement needs the opposite. Two names, two meanings.

The test never Fatals on the interesting outcomes. A request that fails IS the finding for branch B, so it is recorded and the run continues.

- [ ] **Step 1: Write the measurement**

Create `internal/twitch/playback_token_offline_live_test.go`:

```go
package twitch

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// TestLiveOfflineChannelPlaybackToken is Arc 12b Task 0: the ONE live
// measurement that decides which branch Task 3 takes.
//
// THE QUESTION. R2 says the tier-2 probe may target any configured Twitch
// channel, live or not. Arc 10's Task 0 measured only a LIVE channel
// (playback_token_live_test.go requires MOOMBOX_LIVE_TWITCH_CHANNEL to be live,
// and its comment asserts without measuring that "an offline channel yields no
// stream playback token at all"). If an offline channel DOES answer with a
// document carrying user_id, the probe costs one GQL request per tick and needs
// no liveness state; if it does not, Task 3 pays for a GetStreamInfoBatch first.
//
// WHAT THIS PRINTS, absolutely: field NAMES, JSON TYPES, booleans, and the two
// PlaybackTokenSession verdicts. Never a string value, never a number, never
// the Signature, never the auth-token, never a byte of the cookie file. The
// token document is a signed entitlement carrying a device id and a user ip,
// and this output goes to a terminal, a CI log and a pasted report.
//
// Enable with:
//
//	MOOMBOX_LIVE_TWITCH_PROBE=1
//	MOOMBOX_LIVE_TWITCH_COOKIES=<path to a Netscape cookie file for a
//	                             signed-in Twitch session>
//	MOOMBOX_LIVE_TWITCH_OFFLINE_CHANNEL=<login of a channel that is OFFLINE
//	                                     right now>
//
// The arming var is separate from the credential var so running Arc 10's probe
// (which keys on the cookie path alone) does not also fire this one; the
// channel var is separate from MOOMBOX_LIVE_TWITCH_CHANNEL because that one
// means the opposite thing. Always run with -count=1.
func TestLiveOfflineChannelPlaybackToken(t *testing.T) {
	if os.Getenv("MOOMBOX_LIVE_TWITCH_PROBE") != "1" {
		t.Skip("set MOOMBOX_LIVE_TWITCH_PROBE=1 (plus MOOMBOX_LIVE_TWITCH_COOKIES and MOOMBOX_LIVE_TWITCH_OFFLINE_CHANNEL) to run the Arc 12b offline-channel measurement")
	}
	path := os.Getenv("MOOMBOX_LIVE_TWITCH_COOKIES")
	if path == "" {
		t.Skip("set MOOMBOX_LIVE_TWITCH_COOKIES=<path to a signed-in Netscape cookie file> to run the offline-channel measurement")
	}
	channel := os.Getenv("MOOMBOX_LIVE_TWITCH_OFFLINE_CHANNEL")
	if channel == "" {
		t.Skip("set MOOMBOX_LIVE_TWITCH_OFFLINE_CHANNEL=<login of a channel that is OFFLINE right now> to run the offline-channel measurement")
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

	// PREMISE CHECK, and it must run first: the channel has to actually be
	// offline, or this measures the case Arc 10 already measured.
	if info, err := api.GetStreamInfo(ctx, channel, auth.GetAuthToken()); err != nil {
		t.Fatalf("could not confirm the channel is offline (error type %T) — pick another login", err)
	} else if info != nil {
		t.Fatalf("%q is LIVE right now; this measurement needs an OFFLINE channel", channel)
	}
	t.Logf("premise: %q is offline", channel)

	// The authenticated reply. GetAuthToken() is read once and passed straight
	// through; it is never assigned to a variable this test prints.
	authed, authedErr := api.GetStreamAccessToken(ctx, channel, auth.GetAuthToken())
	if authedErr != nil {
		// The error TYPE only. gqlRequest interpolates the response body on
		// 4xx/5xx and an intermediary's error page can echo the Authorization
		// header.
		t.Logf("AUTHENTICATED reply for an offline channel FAILED (error type %T)", authedErr)
		t.Log("FINDING: BRANCH B — an offline channel cannot be probed. " +
			"Task 3 uses the configuredTwitchLogins + firstLiveTwitchLogin form.")
		return
	}

	// The anonymous control. An empty token makes doGQLOnce omit the
	// Authorization header entirely — the same request a cookieless install
	// makes.
	anon, anonErr := api.GetStreamAccessToken(ctx, channel, "")
	if anonErr != nil {
		t.Logf("ANONYMOUS control reply FAILED (error type %T) — the set difference below is unavailable", anonErr)
	}

	authedSignedIn, authedConclusive := PlaybackTokenSession(authed.Value)
	t.Logf("PlaybackTokenSession(authenticated) = signedIn:%v conclusive:%v", authedSignedIn, authedConclusive)

	// The branch, decided and LOGGED before the shapes are described:
	// describePlaybackToken Fatals on a Value that is not a JSON object, and
	// that case is a finding (branch B), not a crash without one. Only the
	// AUTHENTICATED reply decides it: the probe's whole job is to say whether
	// OUR session is still honoured, and the anonymous reply is a control on
	// the discriminator, not an input to the verdict.
	if authedConclusive && authedSignedIn {
		t.Log("FINDING: BRANCH A — an offline channel answers, and user_id identifies the session. " +
			"Task 3 uses pickTwitchProbeChannel's first-configured-login form.")
	} else {
		t.Logf("FINDING: BRANCH B — the offline reply decoded but did not identify the session "+
			"(signedIn:%v conclusive:%v). Task 3 uses the configuredTwitchLogins + firstLiveTwitchLogin form.",
			authedSignedIn, authedConclusive)
	}

	// The shapes, for the record: field names, JSON types and booleans only.
	authedShape := describePlaybackToken(t, "authenticated", authed.Value)
	if anonErr == nil {
		anonSignedIn, anonConclusive := PlaybackTokenSession(anon.Value)
		t.Logf("PlaybackTokenSession(anonymous) = signedIn:%v conclusive:%v", anonSignedIn, anonConclusive)
		anonShape := describePlaybackToken(t, "anonymous", anon.Value)
		reportKeyDifference(t, authedShape, anonShape)
	}
}
```

- [ ] **Step 2: Run it skipped, to prove it compiles and is inert by default**

Run: `go test -count=1 -run TestLiveOfflineChannelPlaybackToken -v ./internal/twitch/`
Expected: PASS, with `--- SKIP` and the reason `set MOOMBOX_LIVE_TWITCH_PROBE=1 …`. Zero network traffic.

- [ ] **Step 3: Run the measurement for real**

Run (PowerShell), with a login you know is offline:

```powershell
$env:MOOMBOX_LIVE_TWITCH_PROBE = "1"
$env:MOOMBOX_LIVE_TWITCH_COOKIES = "<path to a signed-in Netscape cookie file>"
$env:MOOMBOX_LIVE_TWITCH_OFFLINE_CHANNEL = "<an offline channel login>"
go test -count=1 -v -timeout 180s -run TestLiveOfflineChannelPlaybackToken ./internal/twitch/
```

Expected: PASS, with a `FINDING:` line naming BRANCH A or BRANCH B. Record that line verbatim in the task report — Task 3 reads it.

If the premise check Fatals because the channel went live between choosing it and running, pick another login and re-run. That is not a finding.

- [ ] **Step 4: Run the whole-tree gates**

```bash
go build ./...
go vet ./...
GOOS=linux GOARCH=amd64 go build ./...
gofmt -l internal/ cmd/
go test -count=1 ./...
```

- [ ] **Step 5: Commit**

```bash
git add internal/twitch/playback_token_offline_live_test.go
git commit -m "$(cat <<'EOF'
test(twitch): Arc 12b Task 0 — does an offline channel answer the playback-token request

Arc 10 measured only a LIVE channel and asserted the offline case without
looking. R2 rests on that premise, so it gets its own gated live probe:
field NAMES, JSON types and the two PlaybackTokenSession verdicts, never a
value. The FINDING line decides which branch Task 3 takes.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

---

## Task 1: `Service.ProbeSessionLiveness` — the probe itself

**Files:**
- Create: `internal/twitch/liveness_probe.go`
- Create: `internal/twitch/liveness_probe_test.go`

**Interfaces:**
- Consumes: `Service.Auth.GetAuthToken()`, `API.GetStreamAccessToken(ctx, channelLogin, authToken) (*TwitchAccessToken, error)`, `PlaybackTokenSession(value string) (signedIn, conclusive bool)`, `ErrTwitchAuthExpired`, `safeLoginRe` — all existing `internal/twitch`.
- Produces:
  ```go
  var ErrLivenessProbeNotAttempted = errors.New("twitch liveness probe: not attempted")
  func (s *Service) ProbeSessionLiveness(ctx context.Context, channelLogin string) (signedIn, conclusive bool, err error)
  ```
  Task 3's closure calls exactly these two.

**Design notes.**

*Why a method on `Service` and not a free function.* The auth token must be read from the jar the `Service` already holds, ONCE, and used both as the request credential and as the "were credentials actually sent" guard — the discipline `GetHLSMasterPlaylist` documents. A free function taking a token would push the read to the caller in `cmd/moombox`, where the same two-read race is one line away. Note that `TestPlaybackTokenHLSReadsTheAuthTokenOnce`'s technique does NOT transfer: there the second read happens after the network round trip, so the `onGQL` hook can swap the jar between them. Here both uses precede the request, so a two-read mutation has no window a test can drive. Do not write a test that pretends otherwise; the discipline is kept structurally, in one local.

*Why the returned error is sanitised.* `gqlRequest` builds its errors as `fmt.Errorf("gql http %d (%s): %s", statusCode, opLabel(opName), string(respData))` — the RESPONSE BODY, verbatim. On the auth arm it is `fmt.Errorf("gql auth failure (%d) (%s): %s: %w", …, string(respData), ErrTwitchAuthExpired)`. Task 3's closure logs this error at Debug, and Moombox fans the log out over the WebSocket stream to the Web UI and the TUI. An intermediary's error page that echoes the request — including the `Authorization: OAuth …` header — would land on two screens. `internal/twitch/auth.go`'s `validateErrorDetail` already names that exact body as the reason it clamps; the in-tree tests refuse even to PRINT such an error (`service_hls_playback_token_test.go`: "its text is not printed because a transport error embeds the Usher URL, which carries the token"). So this method never passes an upstream error through: it reports the error's TYPE, and re-wraps `ErrTwitchAuthExpired` when that sentinel is present so a caller can still tell the arms apart.

*Why 401/403 is INCONCLUSIVE, not signed-out.* R1 lists exactly three conclusive inputs — `user_id` present, `user_id` null, everything else inconclusive — and a transport error is "everything else". It is tempting to read `ErrTwitchAuthExpired` as a dead token, but `gqlRequest` raises that sentinel on 403 as well as 401, and 403 is an edge block as often as a credential verdict. Whether a genuinely expired `auth-token` surfaces here or as an anonymous token is UNMEASURED (`platform-services.md` § Anonymous Fallback; field-test gate 18). A wrong `loggedIn=false` sends an operator to re-export credentials that were never wrong, which is the one direction this must not fail in. So it is inconclusive, and the error names the arm so a future measurement can find it in a log.

- [ ] **Step 1: Write the failing tests**

Create `internal/twitch/liveness_probe_test.go`:

```go
package twitch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/constants"
)

// probeRoundTripper routes every request to one canned reply.
type probeRoundTripper func(*http.Request) (*http.Response, error)

func (f probeRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// installProbeStub swaps the package-level twitchHTTPClient for one that
// answers the GQL endpoint with (status, body) and refuses every other host,
// and restores it in t.Cleanup. It returns the request counter so a test can
// assert that a guard REFUSED to send rather than sent and ignored.
//
// The swap is why no test in this file may call t.Parallel — the var is shared
// with every other test in the package, exactly as installHLSStub documents.
func installProbeStub(t *testing.T, status int, body string) *atomic.Int64 {
	t.Helper()
	var calls atomic.Int64
	prev := twitchHTTPClient
	t.Cleanup(func() { twitchHTTPClient = prev })

	twitchHTTPClient = &http.Client{Transport: probeRoundTripper(func(req *http.Request) (*http.Response, error) {
		if !strings.HasPrefix(req.URL.String(), constants.TwitchURLs.GQL) {
			// A request anywhere else is a defect in this stub, not a pass.
			return nil, fmt.Errorf("stub received an unexpected request host")
		}
		calls.Add(1)
		h := make(http.Header)
		h.Set("Content-Type", "application/json")
		return &http.Response{
			StatusCode: status,
			Header:     h,
			Body:       io.NopCloser(bytes.NewReader([]byte(body))),
			Request:    req,
		}, nil
	})}
	return &calls
}

// tokenReply renders the real GQL success shape around one token document.
// The signature is always empty: nothing in this file is a signed anything.
func tokenReply(t *testing.T, tokenValue string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"data": map[string]any{
			"streamPlaybackAccessToken": map[string]any{
				"value":     tokenValue,
				"signature": "",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// probeService builds a Service over a jar written from `rows` (the package's
// row helper, auth_test.go), so a cookieless install is `probeService(t)`.
func probeService(t *testing.T, rows ...string) *Service {
	t.Helper()
	jar := hlsJar(t, filepath.Join(t.TempDir(), "cookies.txt"), rows...)
	return NewService(jar, nopLogger{})
}

const probeFixtureToken = "test-token-aaaa"

// TestProbeSessionLivenessReadsTheSessionFromTheToken is the whole verdict
// rule, end to end through the real method.
//
// MUTATION CLOSED: swapping the returned pair for PlaybackTokenSession's
// arguments, or collapsing the absent-key arm into "anonymous". The last is
// the expensive one — a key Twitch renames would then mark every install's
// session dead on the day of the change.
func TestProbeSessionLivenessReadsTheSessionFromTheToken(t *testing.T) {
	for _, tc := range []struct {
		name           string
		tokenValue     string
		wantSignedIn   bool
		wantConclusive bool
		wantErr        bool
	}{
		{"signed in", `{"user_id":12345678,"channel":"somechannel"}`, true, true, false},
		{"anonymous", `{"user_id":null,"channel":"somechannel"}`, false, true, false},
		{"renamed key", `{"userId":12345678}`, false, false, true},
		{"not a json object", `not-json`, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := installProbeStub(t, http.StatusOK, tokenReply(t, tc.tokenValue))
			svc := probeService(t, row(".twitch.tv", "auth-token", probeFixtureToken))

			signedIn, conclusive, err := svc.ProbeSessionLiveness(context.Background(), "somechannel")

			if calls.Load() != 1 {
				t.Fatalf("GQL requests = %d, want 1 — the assertions below say nothing about a probe that never ran", calls.Load())
			}
			if signedIn != tc.wantSignedIn || conclusive != tc.wantConclusive {
				t.Errorf("got (signedIn=%v, conclusive=%v), want (%v, %v)", signedIn, conclusive, tc.wantSignedIn, tc.wantConclusive)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("err != nil = %v, want %v", err != nil, tc.wantErr)
			}
		})
	}
}

// TestProbeSessionLivenessRefusesWithoutInputs: both guards must refuse BEFORE
// the request, and the counter is what proves "refused" rather than "sent and
// ignored".
//
// MUTATION CLOSED (cookieless): dropping the auth-token guard. Twitch answers
// an unauthenticated playback-token request with an ANONYMOUS token by design,
// so the probe would report signedIn=false conclusively — a permanent false
// "your Twitch session is dead" on every cookieless install.
// MUTATION CLOSED (no channel): dropping the login guard. safeLoginRe turns
// both "" and "!!!" into an empty channelName — a request that cannot answer,
// sent every tick.
func TestProbeSessionLivenessRefusesWithoutInputs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rows    []string
		channel string
	}{
		{"cookieless install", nil, "somechannel"},
		{"no channel configured", []string{row(".twitch.tv", "auth-token", probeFixtureToken)}, ""},
		{"login with no usable characters", []string{row(".twitch.tv", "auth-token", probeFixtureToken)}, "!!!"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := installProbeStub(t, http.StatusOK, tokenReply(t, `{"user_id":null}`))
			svc := probeService(t, tc.rows...)

			signedIn, conclusive, err := svc.ProbeSessionLiveness(context.Background(), tc.channel)

			if got := calls.Load(); got != 0 {
				t.Errorf("GQL requests = %d, want 0 — the probe asked anyway", got)
			}
			if signedIn || conclusive {
				t.Errorf("got (signedIn=%v, conclusive=%v), want (false, false)", signedIn, conclusive)
			}
			if !errors.Is(err, ErrLivenessProbeNotAttempted) {
				t.Errorf("err = %v, want ErrLivenessProbeNotAttempted", err)
			}
		})
	}
}

// TestProbeSessionLivenessAuthRefusalIsInconclusive: a 401/403 is NOT a
// signed-out verdict.
//
// gqlRequest raises ErrTwitchAuthExpired for 403 as well as 401, and 403 is an
// edge block as often as a credential verdict; whether a genuinely expired
// auth-token even surfaces here is unmeasured (field-test gate 18). Reporting
// it as conclusive would send an operator to re-export working credentials.
//
// MUTATION CLOSED: mapping the auth arm to (false, true). The sentinel is
// asserted alongside so the arm stays identifiable in a future measurement.
func TestProbeSessionLivenessAuthRefusalIsInconclusive(t *testing.T) {
	calls := installProbeStub(t, http.StatusUnauthorized, `{"error":"Unauthorized"}`)
	svc := probeService(t, row(".twitch.tv", "auth-token", probeFixtureToken))

	signedIn, conclusive, err := svc.ProbeSessionLiveness(context.Background(), "somechannel")

	if calls.Load() != 1 {
		t.Fatalf("GQL requests = %d, want 1", calls.Load())
	}
	if signedIn || conclusive {
		t.Errorf("got (signedIn=%v, conclusive=%v), want (false, false) — a refusal is not a verdict", signedIn, conclusive)
	}
	if !errors.Is(err, ErrTwitchAuthExpired) {
		t.Errorf("err = %v, want it to wrap ErrTwitchAuthExpired so the arm stays identifiable", err)
	}
}

// TestProbeSessionLivenessErrorsCarryNoResponseBody is the leak barrier, and
// the reason this method does not pass upstream errors through: gqlRequest
// interpolates string(respData) into its 4xx and auth errors, Task 3's closure
// logs the result at Debug, and Moombox fans the log out over the WebSocket
// stream to both UIs — so an intermediary's page echoing the Authorization
// header lands on two screens. Same hazard validateErrorDetail clamps for.
//
// MUTATION CLOSED: `return false, false, err` on either error arm.
func TestProbeSessionLivenessErrorsCarryNoResponseBody(t *testing.T) {
	// A body shaped like the things that must never escape: a bearer header,
	// a token document, and the fixture credential itself.
	const leakyBody = `{"echo":"Authorization: OAuth ` + probeFixtureToken + `","user_id":12345678}`

	for _, status := range []int{http.StatusUnauthorized, http.StatusBadRequest} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			installProbeStub(t, status, leakyBody)
			svc := probeService(t, row(".twitch.tv", "auth-token", probeFixtureToken))

			_, _, err := svc.ProbeSessionLiveness(context.Background(), "somechannel")
			if err == nil {
				t.Fatal("want an error from a refused request")
			}
			for _, secret := range []string{probeFixtureToken, "Authorization", "OAuth", "user_id", "12345678"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("the returned error carried %q: %q", secret, err.Error())
				}
			}
		})
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 -run TestProbeSessionLiveness ./internal/twitch/`
Expected: FAIL to COMPILE — `svc.ProbeSessionLiveness undefined` and `undefined: ErrLivenessProbeNotAttempted`.

- [ ] **Step 3: Write the implementation**

Create `internal/twitch/liveness_probe.go`:

```go
package twitch

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrLivenessProbeNotAttempted means ProbeSessionLiveness declined to make a
// request at all: the jar holds no Twitch auth-token, or the channel login is
// not something GetStreamAccessToken can ask about.
//
// It is a sentinel rather than a plain error because the caller's log line
// distinguishes "the probe could not be attempted" (an install that is simply
// not set up for it) from "the probe ran and learned nothing" (a signal that
// may be broken). Same distinction internal/cookies draws with
// ErrAuthCheckNotAttempted.
var ErrLivenessProbeNotAttempted = errors.New("twitch liveness probe: not attempted")

// ProbeSessionLiveness asks whether the stored Twitch session is still signed
// in, using the playback access token as the evidence.
//
// The Twitch twin of YouTube's channel-independent liveness probe, and it
// exists because oauth2/validate cannot answer the question: validate returns
// 200 for a token that is valid but no longer entitled to authenticated
// playback, so RefreshService.checkTwitchAuth reads a dead session as healthy.
// The playback access token DOES say, because Twitch mints it for a session —
// user_id present is signed in, JSON null is nobody (PlaybackTokenSession, and
// Arc 10 Task 0's live measurement behind it). The request is exactly the one
// GetHLSMasterPlaylist already makes; nothing here fetches a playlist, so the
// Usher URL is never built.
//
// THE RETURN CARRIES NO TOKEN. Two booleans and an error, and the error is
// synthesised rather than passed through: gqlRequest interpolates the response
// body into its 4xx and auth errors, an intermediary's error page can echo the
// Authorization header, and the caller logs this into a stream that reaches the
// Web UI and the TUI.
//
// conclusive == false is SILENCE — never a signed-out session. That includes
// the 401/403 arm: gqlRequest raises ErrTwitchAuthExpired for both statuses,
// 403 is an edge block as often as a credential verdict, and whether a
// genuinely expired auth-token even surfaces this way is unmeasured
// (platform-services.md § Anonymous Fallback; field-test gate 18). The sentinel
// is wrapped so that measurement can find the arm.
//
// The auth token is read ONCE, into one local used for both the guard and the
// request — the discipline GetHLSMasterPlaylist documents.
func (s *Service) ProbeSessionLiveness(ctx context.Context, channelLogin string) (signedIn, conclusive bool, err error) {
	authToken := s.Auth.GetAuthToken()
	if authToken == "" {
		// A cookieless install gets an ANONYMOUS token by design. Sending the
		// request anyway would produce a conclusive "signed out" about a
		// session that does not exist — the same guard
		// playbackTokenReportsAnonymous applies first, for the same reason.
		return false, false, fmt.Errorf("%w: no twitch auth-token in the jar", ErrLivenessProbeNotAttempted)
	}
	// GetStreamAccessToken runs safeLoginRe over the login, so an empty or
	// punctuation-only value becomes an empty channelName in the query — a
	// request that cannot answer. Refuse it here instead, using the same rule
	// so the two cannot drift.
	if safeLoginRe.ReplaceAllString(strings.ToLower(channelLogin), "") == "" {
		return false, false, fmt.Errorf("%w: no usable twitch channel login to probe", ErrLivenessProbeNotAttempted)
	}

	token, err := s.API.GetStreamAccessToken(ctx, channelLogin, authToken)
	if err != nil {
		if errors.Is(err, ErrTwitchAuthExpired) {
			// Named, not classified. See the doc comment: this is inconclusive.
			return false, false, fmt.Errorf("twitch liveness probe: twitch refused the credentials on the playback-token request: %w", ErrTwitchAuthExpired)
		}
		// The error TYPE only — never the upstream text, which may carry the
		// response body.
		return false, false, fmt.Errorf("twitch liveness probe: the playback-token request failed (error type %T)", err)
	}

	signedIn, conclusive = PlaybackTokenSession(token.Value)
	if !conclusive {
		// The document was unreadable or carried no user_id. A renamed key is
		// a response-shape change, not a verdict; the message says so without
		// quoting a single byte of the document.
		return false, false, errors.New("twitch liveness probe: the playback token did not say which session it was issued to")
	}
	return signedIn, true, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -count=1 -v -run TestProbeSessionLiveness ./internal/twitch/`
Expected: PASS, all four test functions.

- [ ] **Step 5: Verify the leak barrier by reverting it once**

Temporarily change the non-sentinel arm to `return false, false, err` and run:

Run: `go test -count=1 -run TestProbeSessionLivenessErrorsCarryNoResponseBody ./internal/twitch/`
Expected: FAIL, naming `Authorization` / `OAuth` / the fixture token in the error. Then restore the sanitised line and re-run: PASS.

- [ ] **Step 6: Run the whole-tree gates**

```bash
go build ./...
go vet ./...
GOOS=linux GOARCH=amd64 go build ./...
gofmt -l internal/ cmd/
go test -count=1 ./...
```

- [ ] **Step 7: Commit**

```bash
git add internal/twitch/liveness_probe.go internal/twitch/liveness_probe_test.go
git commit -m "$(cat <<'EOF'
feat(twitch): ProbeSessionLiveness — the playback token as a session signal

oauth2/validate answers 200 for a token that is no longer entitled to
authenticated playback, so it cannot tell a live Twitch session from a dead
one. The playback access token can: Twitch mints it for a session and
PlaybackTokenSession already reads that. One method over the request
GetHLSMasterPlaylist already makes; no playlist is fetched.

Two booleans and a SYNTHESISED error. gqlRequest interpolates the response
body into its 4xx and auth errors and an intermediary's page can echo the
Authorization header, so nothing upstream is passed through. 401/403 is
inconclusive, not signed-out — 403 is an edge block as often as a verdict.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

---

## Task 2: `RefreshService.TwitchFallbackLiveness` — the seam

**Files:**
- Modify: `internal/cookies/refresh.go:673-674` (the `FallbackLiveness` field, add the twin after it)
- Modify: `internal/cookies/refresh.go:1399-1402` (the locked snapshot block, add `hasTwitchToken`)
- Modify: `internal/cookies/refresh.go:1359-1369` (the `var (…)` declaration above it)
- Modify: `internal/cookies/refresh.go:1670` (insert the Twitch block after the YouTube one's closing brace)
- Create: `internal/cookies/refresh_twitch_liveness_test.go`

**Interfaces:**
- Consumes: nothing from Task 1 (`internal/cookies` cannot import `internal/twitch` — the closure in Task 3 bridges them).
- Produces:
  ```go
  // field on RefreshService
  TwitchFallbackLiveness func(ctx context.Context) (loggedIn, conclusive bool)
  ```
  Task 3 assigns exactly this field.

**Design notes.**

*The gate is the NARROW predicate.* The YouTube block gates its inconclusive log on `hasYTCookies` (`HasAnyYouTubeAuthCookie` — "was this platform ever configured"), and runs the probe unconditionally because `ProbeAccountLiveness` has its own first gate. Twitch is the other way round: the probe REQUIRES the bearer token, because a request without it gets an anonymous token by design and Task 1 refuses to send. So the gate is `jar.HasTwitchAuthCookies()` — "is the `auth-token` present right now" (`jar.go:878`) — and it covers the whole block, verdict arm and inconclusive arm alike. `HasAnyTwitchAuthCookie` (the broad one, which also counts `twilight-user`) would let a session whose `auth-token` was pruned on expiry pay for a request the probe would decline anyway.

*Snapshot it under the lock.* `hasTwitchToken` is read in the same locked block as `hasTWCookies`, for the reason that block already states: every snapshot this pass reasons about is taken under one lock after `jar.Reload()`, so a concurrent reload cannot make the gate and the rest of the pass describe different files.

*The dedupe for "no channel configured" is `recordInconclusiveLiveness`.* R2 wants ONE Info line, not one per tick, when the install has a token and nothing to probe. That is exactly what the existing inconclusive arm does: `recordInconclusiveLiveness(platform)` returns notable only on a CHANGE of what is known about the platform, so a permanently-unprobeable install says so once per process at Info and Debug thereafter. Reusing it — same message string, `"platform", "twitch"` — is the "no new mechanism" shape and keeps the line greppable across both platforms. `cmd/moombox`'s closure logs the REASON at Debug, where it holds it, exactly as the YouTube closure does.

*Nothing here writes `AuthStatus`.* `ObserveLiveness` is the only exit, and while `livenessRecoveryArmed` is false it records, dedupes and logs and stops. The capture-time mark (`NoteTwitchAuthLoss`, Arc 10) remains the sole direct writer of `rs.status.TwitchAuthenticated` outside `doRefresh`'s own block.

- [ ] **Step 1: Write the failing tests**

Create `internal/cookies/refresh_twitch_liveness_test.go`:

```go
package cookies

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// jarWithTwilightUserOnly is the jar the NARROW gate exists for: Twitch was
// configured (twilight-user survives), the auth-token was pruned on expiry,
// HasAnyTwitchAuthCookie says true and HasTwitchAuthCookies says false. See
// twitchAuthCookieNames for how the state arises.
func jarWithTwilightUserOnly(t *testing.T) *CookieJar {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cookies.txt")
	content := "# Netscape HTTP Cookie File\n" +
		".twitch.tv\tTRUE\t/\tTRUE\t0\ttwilight-user\tfixture-twilight-user\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	return jar
}

// TestTwitchFallbackRunsAndOnlyForAConfiguredToken pairs the two halves that
// make each other mean something.
//
// MUTATION CLOSED (the 1): deleting the whole Twitch block — every "want 0"
// assertion in this file passes without it.
// MUTATION CLOSED (the 0): dropping the jar gate, or widening it to
// HasAnyTwitchAuthCookie. A jar holding only twilight-user has no bearer token,
// so the probe would be sent and declined every tick forever.
func TestTwitchFallbackRunsAndOnlyForAConfiguredToken(t *testing.T) {
	healthyRefreshSeams(t)

	withToken := 0
	rs := NewRefreshService(jarWithTwitchAuth(t), 0, nopLogger{})
	rs.TwitchFallbackLiveness = func(context.Context) (bool, bool) { withToken++; return true, true }
	rs.doRefresh(context.Background())
	if withToken != 1 {
		t.Errorf("probe fired %d times for a jar holding an auth-token, want 1", withToken)
	}

	bare := 0
	rsBare := NewRefreshService(NewCookieJar(), 0, nopLogger{})
	rsBare.TwitchFallbackLiveness = func(context.Context) (bool, bool) { bare++; return true, true }
	rsBare.doRefresh(context.Background())
	if bare != 0 {
		t.Errorf("probe fired %d times for a jar with no Twitch cookies at all, want 0", bare)
	}

	// The arm that actually closes the widening mutation: an empty jar fails
	// HasAnyTwitchAuthCookie too, so only a twilight-user-only jar can tell
	// the narrow gate from the broad one.
	pruned := 0
	rsPruned := NewRefreshService(jarWithTwilightUserOnly(t), 0, nopLogger{})
	rsPruned.TwitchFallbackLiveness = func(context.Context) (bool, bool) { pruned++; return true, true }
	rsPruned.doRefresh(context.Background())
	if pruned != 0 {
		t.Errorf("probe fired %d times for a jar holding twilight-user but no auth-token, want 0 — the gate is the broad HasAnyTwitchAuthCookie, and this install would be asked and declined every tick", pruned)
	}
}

// TestTwitchFallbackObservesOnlyConclusiveAnswers: a conclusive verdict reaches
// ObserveLiveness; an inconclusive one moves nothing — neither the freshness
// stamp (which would suppress the next cycle's probe) nor the recovery dedupe
// (which would swallow the next real signed-out verdict).
//
// MUTATION CLOSED: routing the inconclusive arm through ObserveLiveness, or
// dropping the `if conclusive` entirely.
func TestTwitchFallbackObservesOnlyConclusiveAnswers(t *testing.T) {
	healthyRefreshSeams(t)

	// Conclusive: the observation lands and the freshness stamp is written.
	rs := NewRefreshService(jarWithTwitchAuth(t), 0, nopLogger{})
	rs.TwitchFallbackLiveness = func(context.Context) (bool, bool) { return true, true }
	rs.doRefresh(context.Background())
	if !rs.livenessObservedRecently("twitch", time.Now()) {
		t.Error("a conclusive verdict recorded no observation — the probe's answer never reached ObserveLiveness")
	}

	// Inconclusive: nothing moves.
	called := 0
	rsInc := NewRefreshService(jarWithTwitchAuth(t), 0, nopLogger{})
	rsInc.TwitchFallbackLiveness = func(context.Context) (bool, bool) { called++; return false, false }
	rsInc.doRefresh(context.Background())
	if called != 1 {
		t.Fatalf("probe fired %d times, want 1 — the assertions below say nothing about a probe that never ran", called)
	}
	if rsInc.livenessObservedRecently("twitch", time.Now()) {
		t.Error("an inconclusive probe recorded an observation — it would suppress the next cycle's probe")
	}
	if due, _ := rsInc.recordLiveness("twitch", false, time.Now()); !due {
		t.Error("an inconclusive probe consumed the recovery dedupe — a real signed-out verdict would be swallowed")
	}
}

// TestTwitchFallbackUsesTheTwitchPlatformKey.
//
// MUTATION CLOSED: typing "youtube" into either the freshness gate or the
// ObserveLiveness call — a copy-paste from the block directly above, and the
// single most likely defect in this change. Either mistake makes a YouTube
// observation suppress the Twitch probe, or files a Twitch verdict under
// YouTube's key where an armed pilot would page about the wrong platform.
func TestTwitchFallbackUsesTheTwitchPlatformKey(t *testing.T) {
	healthyRefreshSeams(t)

	called := 0
	rs := NewRefreshService(jarWithBothPlatformsAuth(t), 0, nopLogger{})
	rs.TwitchFallbackLiveness = func(context.Context) (bool, bool) { called++; return false, true }

	// A fresh YouTube observation must not suppress the Twitch probe.
	rs.ObserveLiveness("youtube", true)
	rs.doRefresh(context.Background())
	if called != 1 {
		t.Fatalf("probe fired %d times after a YouTube observation, want 1 — the gate is reading the wrong platform key", called)
	}

	// And the verdict landed under "twitch": a signed-out verdict consumes the
	// twitch dedupe and leaves youtube's alone.
	if due, _ := rs.recordLiveness("twitch", false, time.Now()); due {
		t.Error("the signed-out verdict did not consume the twitch dedupe — it was filed under another platform")
	}
	if due, _ := rs.recordLiveness("youtube", false, time.Now()); !due {
		t.Error("the Twitch verdict consumed YouTube's dedupe — the platform string is wrong")
	}
}

// TestTwitchFallbackSkippedWhenObservedRecently: a fresh Twitch observation
// must not be paid for twice. The second half is what makes the first mean
// anything — `called == 0` is satisfied just as well by the block not existing,
// so the observation is aged past the window on the SAME service and the SAME
// call must now pay.
//
// MUTATION CLOSED: dropping !livenessObservedRecently from the condition.
func TestTwitchFallbackSkippedWhenObservedRecently(t *testing.T) {
	healthyRefreshSeams(t)

	called := 0
	rs := NewRefreshService(jarWithTwitchAuth(t), 0, nopLogger{})
	rs.TwitchFallbackLiveness = func(context.Context) (bool, bool) { called++; return true, true }

	rs.ObserveLiveness("twitch", true)
	rs.doRefresh(context.Background())
	if called != 0 {
		t.Errorf("probe fired %d times despite a fresh observation, want 0", called)
	}

	rs.mu.Lock()
	rs.lastLivenessObserved["twitch"] = time.Now().Add(-livenessFreshWindow - time.Minute)
	rs.mu.Unlock()

	rs.doRefresh(context.Background())
	if called != 1 {
		t.Errorf("probe fired %d times once the observation aged out, want 1 — the zero above proves nothing if the probe never runs at all", called)
	}
}

// TestTwitchInconclusiveFallbackIsReportedOncePerRun is R2's "says so once at
// Info", pinned as a dedupe rather than a promise. An install with a token and
// no configured channel is inconclusive on EVERY tick, forever: a line per tick
// is noise fanned out over the WebSocket stream to both UIs, and no line makes
// "the signal is dead" indistinguishable from "healthy, nothing to say" — the
// judgement the disarmed pilot exists to inform.
//
// MUTATION CLOSED: logging unconditionally at Info (3 lines), dropping the line
// (0), or recording through recordLiveness instead of
// recordInconclusiveLiveness (which also suppresses the next probe — the
// `called != 3` assertion catches that one).
func TestTwitchInconclusiveFallbackIsReportedOncePerRun(t *testing.T) {
	healthyRefreshSeams(t)

	const line = "liveness fallback probe learned nothing"

	log := &capturingLogger{}
	called := 0
	rs := NewRefreshService(jarWithTwitchAuth(t), 0, log)
	rs.TwitchFallbackLiveness = func(context.Context) (bool, bool) { called++; return false, false }

	for range 3 {
		rs.doRefresh(context.Background())
	}

	if called != 3 {
		t.Fatalf("probe ran %d times over 3 refreshes, want 3 — the log counts below say nothing otherwise", called)
	}
	if got := countContaining(log.infos, line); got != 1 {
		t.Errorf("%d operator-visible lines about an inconclusive probe over 3 cycles, want exactly 1: %q", got, log.infos)
	}
	if got := countContaining(log.debugs, line); got != 2 {
		t.Errorf("%d debug-level repeats, want 2 — the repeats must still be recorded, just not at Info: %q", got, log.debugs)
	}
}

// TestTwitchFallbackIsPeriodicOnly: CheckNow runs synchronously on an HTTP
// handler and Start's initial check runs before the web server binds; neither
// may buy a GQL round trip. The doRefresh half is not decoration — without it
// `called == 0` is equally well explained by a block that was never written.
//
// MUTATION CLOSED: dropping allowFallback from the condition.
func TestTwitchFallbackIsPeriodicOnly(t *testing.T) {
	healthyRefreshSeams(t)

	called := 0
	rs := NewRefreshService(jarWithTwitchAuth(t), 0, nopLogger{})
	rs.TwitchFallbackLiveness = func(context.Context) (bool, bool) { called++; return true, true }

	rs.CheckNow(context.Background())
	if called != 0 {
		t.Errorf("probe fired %d times on the CheckNow path, want 0", called)
	}

	rs.doRefresh(context.Background())
	if called != 1 {
		t.Errorf("probe fired %d times on the periodic path, want 1 — the CheckNow zero above proves nothing if the probe never runs at all", called)
	}
}

// TestTwitchFallbackSkipsTheStartupCheck: Start runs its initial check
// SYNCHRONOUSLY on the caller's goroutine, and cmd/moombox's run() blocks on
// it before the web server binds. A GQL round trip — up to the closure's 20 s
// timeout — in front of the dashboard on every start is the trade the YouTube
// twin refused (TestStartupRefreshSkipsFallbackProbe), and it was missed there
// first, which is why the mirror carries it separately from the CheckNow arm.
//
// MUTATION CLOSED: Start's initial check passing allowFallback=true.
func TestTwitchFallbackSkipsTheStartupCheck(t *testing.T) {
	healthyRefreshSeams(t)

	// Atomic because Start spawns the ticker goroutine.
	var called atomic.Int64
	rs := NewRefreshService(jarWithTwitchAuth(t), 0, nopLogger{})
	rs.TwitchFallbackLiveness = func(context.Context) (bool, bool) { called.Add(1); return true, true }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rs.Start(ctx)
	rs.Stop()

	if got := called.Load(); got != 0 {
		t.Errorf("probe fired %d times on the synchronous startup check, want 0", got)
	}

	rs.doRefresh(context.Background())
	if got := called.Load(); got != 1 {
		t.Errorf("probe fired %d times on the ticker path, want 1 — the startup zero proves nothing if the probe never runs at all", got)
	}
}

// TestTwitchFallbackWritesNoAuthStatus: the probe is an OBSERVATION producer.
// Arc 10's capture-time mark stays the only writer of
// rs.status.TwitchAuthenticated outside doRefresh's own status block.
//
// MUTATION CLOSED: adding `rs.status.TwitchAuthenticated = loggedIn` (or a
// NoteTwitchAuthLoss call) to the new block. With the tier-1 seam answering a
// healthy 200, a signed-out tier-2 verdict must leave the status green; the
// recorded observation is asserted alongside so a probe that never ran cannot
// pass this.
func TestTwitchFallbackWritesNoAuthStatus(t *testing.T) {
	healthyRefreshSeams(t)

	rs := NewRefreshService(jarWithTwitchAuth(t), 0, nopLogger{})
	rs.TwitchFallbackLiveness = func(context.Context) (bool, bool) { return false, true }
	rs.doRefresh(context.Background())

	if !rs.livenessObservedRecently("twitch", time.Now()) {
		t.Fatal("the signed-out verdict never reached ObserveLiveness — the assertion below would pass vacuously")
	}
	if got := rs.GetStatus(); !got.TwitchAuthenticated {
		t.Error("a tier-2 signed-out verdict flipped AuthStatus.TwitchAuthenticated while the pilot is disarmed")
	}
	if livenessRecoveryArmed {
		t.Error("livenessRecoveryArmed is true — this arc does not arm the pilot")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 -run TestTwitch ./internal/cookies/`
Expected: FAIL to COMPILE — `rs.TwitchFallbackLiveness undefined`.

- [ ] **Step 3: Add the field**

In `internal/cookies/refresh.go`, immediately after the `FallbackLiveness` field (line 673) and before the closing `}` of the `RefreshService` struct:

```go
	// TwitchFallbackLiveness is the Twitch twin of FallbackLiveness, injected
	// for the same structural reason and one more: internal/twitch imports
	// THIS package (service.go, auth.go), so the direct call is an import cycle
	// in the other direction. cmd/moombox builds the closure.
	//
	// It exists because checkTwitchAuth cannot answer the question it looks
	// like it answers: oauth2/validate returns 200 for a token that is valid
	// but no longer entitled to authenticated playback, so an install with no
	// capture running reads a dead session as healthy until a stream goes live.
	// The playback access token DOES say which session it was minted for — see
	// internal/twitch.Service.ProbeSessionLiveness and PlaybackTokenSession.
	//
	// Called at the tail of a PERIODIC refresh under the same three conditions
	// the YouTube twin uses, plus one: the jar must hold a Twitch auth-token
	// RIGHT NOW (HasTwitchAuthCookies, not the broad HasAnyTwitchAuthCookie).
	// Without the bearer token the request gets an anonymous playback token by
	// design, so the probe would decline anyway.
	//
	// conclusive == false means the probe learned nothing — no configured
	// channel, a rate limit, a transport failure, a 401/403 that may be an edge
	// block — and MUST NOT move any state.
	TwitchFallbackLiveness func(ctx context.Context) (loggedIn, conclusive bool)
```

- [ ] **Step 4: Snapshot the gate under the lock**

In the `var (…)` block at `refresh.go:1359-1369`, change the cookie-presence line to declare the new value too:

```go
		hasYTCookies, hasTWCookies bool
		hasTwitchToken             bool
```

And in the locked block, immediately after `hasTWCookies = rs.jar.HasAnyTwitchAuthCookie()`:

```go
		// The NARROW predicate, and deliberately not hasTWCookies. The tier-2
		// Twitch probe sends the bearer token as the credential; without it
		// Twitch mints an anonymous playback token by design and the probe
		// declines. Sampled here so the gate and every other snapshot this
		// pass reasons about describe the same reload.
		hasTwitchToken = rs.jar.HasTwitchAuthCookies()
```

- [ ] **Step 5: Add the call site**

In `doRefresh`, immediately after the closing brace of the YouTube fallback block (`refresh.go:1670` — `:1669` closes its `else if`, `:1670` closes the `if`) and before `rs.logger.Debug("cookie refresh done", …)`:

```go
	// Tier 2, Twitch. The same shape as the block above, and the same pilot
	// gate withholds the same last step. What differs is the jar condition:
	// this probe SENDS the auth-token, so an install without one is not
	// "unreported", it is unprobeable — and asking anyway would get an
	// anonymous playback token by design and read as a dead session.
	//
	// Runs inline on the ticker goroutine, which carries Start's inline
	// recover. Nothing is spawned.
	if allowFallback && rs.TwitchFallbackLiveness != nil && hasTwitchToken &&
		!rs.livenessObservedRecently("twitch", time.Now()) {
		// Only a conclusive answer moves anything. `false, false` is no
		// configured channel, a rate limit, a transport failure, or a 401/403
		// that may be an edge block — never a dead session.
		if loggedIn, conclusive := rs.TwitchFallbackLiveness(ctx); conclusive {
			rs.ObserveLiveness("twitch", loggedIn)
		} else {
			// Deduped through the same record a verdict uses, so an install
			// that can never answer — no Twitch channel configured, a
			// permanently refused request — says so once per process instead
			// of once per cycle. No second configured-platform gate is needed
			// the way the YouTube arm needs one: hasTwitchToken above already
			// established that there is a session to report on.
			//
			// The reason is not here because the (loggedIn, conclusive) pair
			// cannot carry one; cmd/moombox's closure logs the probe's own
			// error at Debug, where it has it.
			logAt := rs.logger.Debug
			if rs.recordInconclusiveLiveness("twitch") {
				logAt = rs.logger.Info
			}
			logAt("liveness fallback probe learned nothing about this session", "platform", "twitch")
		}
	}
```

- [ ] **Step 5b: Retire the two comments that count producers**

`ObserveLiveness`'s doc comment (`refresh.go:926-937`) says reaching it means "YouTube told us" and that "Two producers exist today, both YouTube"; its Warn branch (`:972-977`) says "ObserveLiveness has two producers". All three are false once Step 5 lands. In the doc comment, replace

> `// means "YouTube told us", not "we asked". A consent wall, a rate limit, an`

with

> `// means "the platform told us", not "we asked". A consent wall, a rate limit, an`

and replace the four lines

> `// Two producers exist today, both YouTube: the per-channel membership probe`
> `// (which runs once per configured channel per feed cycle) and the`
> `// channel-independent FallbackLiveness probe. The first is why the dedupe is`
> `// not optional — one dead session must raise one alarm, not one per channel.`

with

> `// Three producers exist today: YouTube's per-channel membership probe (which`
> `// runs once per configured channel per feed cycle), YouTube's`
> `// channel-independent FallbackLiveness probe, and Twitch's`
> `// TwitchFallbackLiveness probe. The first is why the dedupe is not optional —`
> `// one dead session must raise one alarm, not one per channel.`

and in the Warn branch replace

> `// has two producers — the per-channel membership probe and the`
> `// channel-independent fallback — and cannot tell which sent this`

with

> `// has three producers — the per-channel membership probe and the two`
> `// channel-independent fallbacks — and cannot tell which sent this`

Gate: `grep -n "both YouTube\|two producers\|YouTube told us" internal/cookies/refresh.go` prints nothing.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test -count=1 -v -run TestTwitchFallback ./internal/cookies/` and `go test -count=1 -v -run TestTwitchInconclusive ./internal/cookies/`
Expected: PASS, all eight test functions.

- [ ] **Step 7: Verify the platform-key test by mutating once**

Temporarily change `rs.ObserveLiveness("twitch", loggedIn)` to `rs.ObserveLiveness("youtube", loggedIn)` and run:

Run: `go test -count=1 -run TestTwitchFallbackUsesTheTwitchPlatformKey ./internal/cookies/`
Expected: FAIL. Restore the line and re-run: PASS.

- [ ] **Step 8: Run the whole-tree gates**

```bash
go build ./...
go vet ./...
GOOS=linux GOARCH=amd64 go build ./...
gofmt -l internal/ cmd/
go test -count=1 ./...
```

- [ ] **Step 9: Commit**

```bash
git add internal/cookies/refresh.go internal/cookies/refresh_twitch_liveness_test.go
git commit -m "$(cat <<'EOF'
feat(cookies): TwitchFallbackLiveness — the tier-2 seam Twitch never had

A twin of FallbackLiveness, injected for the same reason and one more:
internal/twitch imports this package, so the direct call is an import cycle in
the other direction. Same three conditions as the YouTube call site, plus the
NARROW jar gate — the probe sends the bearer token, so an install without one
is unprobeable rather than unreported.

Feeds ObserveLiveness("twitch", …) and stops there. livenessRecoveryArmed stays
false; nothing here writes AuthStatus. The inconclusive arm reuses
recordInconclusiveLiveness so an install with a token and no configured Twitch
channel says so once per process, not once per tick.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

---

## Task 3: The `cmd/moombox` closure, and the two spec sentences it makes false

**Files:**
- Modify: `cmd/moombox/services.go:838` (append the Twitch closure directly after the YouTube one)
- Create: `cmd/moombox/services_twitch_liveness_test.go`
- Modify: `docs/spec/data-and-storage.md:838`
- Modify: `docs/spec/platform-services.md:483-493` (§ Twitch Authentication)

**Interfaces:**
- Consumes: `twitch.Service.ProbeSessionLiveness(ctx, login) (signedIn, conclusive bool, err error)` and `twitch.ErrLivenessProbeNotAttempted` (Task 1); `cookies.RefreshService.TwitchFallbackLiveness` (Task 2); `config.Store.Read(func(*config.MoomboxConfig))`; `config.ChannelConfig{ID, Platform, Enabled}`.
- Produces:
  ```go
  const twitchLivenessProbeTimeout = 20 * time.Second
  func pickTwitchProbeChannel(cfg *config.MoomboxConfig) string   // BRANCH A
  // or, BRANCH B:
  const twitchLivenessProbeBatch = 30
  func configuredTwitchLogins(cfg *config.MoomboxConfig) []string
  func firstLiveTwitchLogin(ctx context.Context, tw *twitch.Service, logins []string) string
  ```
  Nothing later in this plan consumes them; Task 4 records the field gate they open.

**Design notes.**

*Which channel.* The configured Twitch channels are `cfg.Channels` entries whose `Platform` is exactly `"twitch"` and which are enabled. That predicate is copied from `TwitchMonitor.getTwitchChannels` (`internal/monitor/twitch.go:423-437`) verbatim — including its use of the RAW `ch.Platform` field rather than `ch.GetPlatform()`, whose default is `"youtube"` — so the probe can only ever target a channel the monitor would actually poll. `ChannelConfig.ID` IS the Twitch login.

*Hot-reload safety.* The config is read inside `s.configStore.Read` on EVERY call, never captured at wiring time. An operator who adds their first Twitch channel gets a working probe on the next tick without a restart; one who removes the last gets an inconclusive probe and one Info line, not a request against a stale login.

*The timeout, and why the closure owns it.* `doRefresh` hands the ticker's context straight through, unbounded. `gqlRequest` retries transient failures three times with a 1s/2s/4s backoff on top of `twitchHTTPClient`'s 30 s per-attempt timeout — up to roughly 127 s of ticker goroutine for one probe that is allowed to learn nothing. 20 s bounds it to about one attempt plus the first retry's backoff, and matches the number the YouTube twin's tests use for a tier-2 probe (`TestCheckNowSkipsFallbackProbe`: "Adding a 20s page fetch to a button press is a bad trade"). An answer this probe does not get in 20 s is an answer the next tick can have.

*Placement.* Directly after the YouTube closure at `services.go:831-838`, inside `initServices`, where `twService` (`:508`), `s.configStore` and `log` are all already in scope. The two closures sit together so a reader sees one tier-2 wiring block, not two unrelated ones.

- [ ] **Step 1: Write the failing tests**

Create `cmd/moombox/services_twitch_liveness_test.go`. This is the BRANCH A file; under branch B — the default — write the branch-B file given at the end of Step 3-ALT instead, and Step 2's expected compile failure reads `undefined: configuredTwitchLogins`:

```go
package main

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// ptrBool is a local helper: ChannelConfig.Enabled is a *bool whose nil means
// enabled.
func ptrBool(b bool) *bool { return &b }

// TestPickTwitchProbeChannelMirrorsTheMonitor pins the one decision the closure
// makes on its own: WHICH login the tier-2 probe targets. The predicate is
// copied from TwitchMonitor.getTwitchChannels (internal/monitor/twitch.go) so
// the probe can only ever ask about a channel the monitor would actually poll.
//
// MUTATION CLOSED (platform): using GetPlatform(), which defaults an empty
// platform to "youtube" and would send a YouTube channel's ID to Twitch GQL.
// MUTATION CLOSED (enabled): dropping the disabled skip — a channel the
// operator turned off would still generate traffic every tick.
// MUTATION CLOSED (order): returning the last match, or any match.
func TestPickTwitchProbeChannelMirrorsTheMonitor(t *testing.T) {
	for _, tc := range []struct {
		name     string
		channels []config.ChannelConfig
		want     string
	}{
		{"no channels at all", nil, ""},
		{
			"youtube only",
			[]config.ChannelConfig{{ID: "UC-something", Platform: "youtube"}},
			"",
		},
		{
			"platform absent is not twitch",
			[]config.ChannelConfig{{ID: "UC-something"}},
			"",
		},
		{
			"first twitch entry wins",
			[]config.ChannelConfig{
				{ID: "UC-something", Platform: "youtube"},
				{ID: "first_login", Platform: "twitch"},
				{ID: "second_login", Platform: "twitch"},
			},
			"first_login",
		},
		{
			"a disabled twitch channel is skipped",
			[]config.ChannelConfig{
				{ID: "disabled_login", Platform: "twitch", Enabled: ptrBool(false)},
				{ID: "enabled_login", Platform: "twitch"},
			},
			"enabled_login",
		},
		{
			"every twitch channel disabled",
			[]config.ChannelConfig{
				{ID: "disabled_login", Platform: "twitch", Enabled: ptrBool(false)},
			},
			"",
		},
		{
			"an explicit enabled=true is honoured",
			[]config.ChannelConfig{
				{ID: "explicit_login", Platform: "twitch", Enabled: ptrBool(true)},
			},
			"explicit_login",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := pickTwitchProbeChannel(&config.MoomboxConfig{Channels: tc.channels})
			if got != tc.want {
				t.Errorf("pickTwitchProbeChannel = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPickTwitchProbeChannelIsStable: repeated calls on one config must return
// the same login. The verdict is session-level so the choice does not change
// the answer, but a target that wanders between ticks reads as a defect in the
// log — and a future refactor that indexed channels by ID would make it wander
// silently, because Go map iteration is random.
//
// MUTATION CLOSED: any implementation that does not walk cfg.Channels in order.
func TestPickTwitchProbeChannelIsStable(t *testing.T) {
	cfg := &config.MoomboxConfig{Channels: []config.ChannelConfig{
		{ID: "aaa_login", Platform: "twitch"},
		{ID: "bbb_login", Platform: "twitch"},
		{ID: "ccc_login", Platform: "twitch"},
	}}
	for range 20 {
		if got := pickTwitchProbeChannel(cfg); got != "aaa_login" {
			t.Fatalf("pickTwitchProbeChannel = %q, want the first configured login %q on every call", got, "aaa_login")
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 -run TestPickTwitchProbeChannel ./cmd/moombox/`
Expected: FAIL to COMPILE — `undefined: pickTwitchProbeChannel`.

- [ ] **Step 3: Write the picker (BRANCH A) — ONLY if Task 0 logged BRANCH A; with no `FINDING:` line, skip to Step 3-ALT (branch B is the default)**

Add to `cmd/moombox/services.go`, immediately after `livenessFromProbe` (`:242`):

```go
// twitchLivenessProbeTimeout bounds the tier-2 Twitch probe.
//
// doRefresh hands the ticker's context straight through, unbounded, and
// internal/twitch's gqlRequest retries transient failures three times with a
// 1s/2s/4s backoff on top of a 30-second per-attempt client timeout — up to
// roughly two minutes of ticker goroutine for a probe that is ALLOWED to learn
// nothing. 20 seconds bounds it to about one attempt plus the first retry's
// wait, and matches the number the YouTube twin's tests use for a tier-2
// probe. An answer this does not get in 20 seconds is one the next tick can
// have.
const twitchLivenessProbeTimeout = 20 * time.Second

// pickTwitchProbeChannel returns the login the tier-2 Twitch probe should ask
// about, or "" when this install has no Twitch channel to ask about.
//
// The predicate is TwitchMonitor.getTwitchChannels' (internal/monitor/
// twitch.go), copied rather than shared because that method is unexported and
// hangs off the monitor: an enabled channel whose `Platform` field is exactly
// "twitch". The RAW field, deliberately — ChannelConfig.GetPlatform() defaults
// an empty platform to "youtube", so reading through it would let a YouTube
// channel with no explicit platform be sent to Twitch GQL.
//
// FIRST match, in slice order, so the target is stable across ticks: the
// verdict is session-level and does not depend on which channel is used, but a
// wandering target in the log reads as a defect.
func pickTwitchProbeChannel(cfg *config.MoomboxConfig) string {
	for _, ch := range cfg.Channels {
		if ch.Enabled != nil && !*ch.Enabled {
			continue
		}
		if ch.Platform != "twitch" {
			continue
		}
		return ch.ID
	}
	return ""
}
```

- [ ] **Step 3-ALT: Write the picker (BRANCH B) — the DEFAULT: take it if Task 0 logged BRANCH B or was not run**

Write `twitchLivenessProbeTimeout` exactly as in Step 3, and instead of `pickTwitchProbeChannel` add the constant and the two functions below. Step 1's test file is replaced WHOLE by the branch-B file that follows the code (the same seven cases, compared as `[]string`; the same stability pin). Both branches were compile-checked against Task 1's signature at `16afaa6`.

```go
// twitchLivenessProbeBatch caps how many configured logins ride in the
// probe's one stream-info request. It is TwitchMonitor's twitchBatchChunk
// (internal/monitor/twitch.go — unexported, so the number is repeated here):
// the monitor never sends more than 30 logins in one request, and this probe
// needs ONE live channel, not a survey, so the first chunk of the configured
// list keeps the request the shape the monitor has field-proven.
const twitchLivenessProbeBatch = 30

// configuredTwitchLogins returns every enabled Twitch channel login in config
// order. The predicate is TwitchMonitor.getTwitchChannels' (internal/monitor/
// twitch.go), copied because that method is unexported: an enabled channel
// whose RAW `Platform` field is exactly "twitch" — not GetPlatform(), which
// defaults an empty platform to "youtube" and would let a YouTube channel
// with no explicit platform be sent to Twitch GQL.
func configuredTwitchLogins(cfg *config.MoomboxConfig) []string {
	var logins []string
	for _, ch := range cfg.Channels {
		if ch.Enabled != nil && !*ch.Enabled {
			continue
		}
		if ch.Platform != "twitch" {
			continue
		}
		logins = append(logins, ch.ID)
	}
	return logins
}

// firstLiveTwitchLogin returns the first configured login that is LIVE right
// now, or "" when none is.
//
// Branch B: the default, and the branch Task 0's measurement selects when an
// offline channel's playback access token does not identify the session.
// Every premise here is measured — Arc 10's live probe confirmed user_id on a
// LIVE channel — where branch A's is not. It costs a second GQL request per
// tick — but it is the SAME batched request TwitchMonitor makes every 15
// seconds (API.GetStreamInfoBatch), so it adds no endpoint, no query and no
// state. TwitchMonitor keeps no exposed liveness set of its own — its fields
// are the cycle latch, the health tracker and the next-check timestamp — and
// database.Job carries a display name rather than a login, so re-issuing the
// query is the cheapest correct source.
//
// A nil info in the returned slice means the channel is offline (see
// API.parseStreamInfo). wholeErr makes the per-channel slices unusable, so it
// returns "" and the probe reports inconclusive.
func firstLiveTwitchLogin(ctx context.Context, tw *twitch.Service, logins []string) string {
	if len(logins) == 0 {
		return ""
	}
	if len(logins) > twitchLivenessProbeBatch {
		logins = logins[:twitchLivenessProbeBatch]
	}
	infos, _, wholeErr := tw.GetStreamInfoBatch(ctx, logins)
	if wholeErr != nil || len(infos) != len(logins) {
		return ""
	}
	for i, info := range infos {
		if info != nil {
			return logins[i]
		}
	}
	return ""
}
```

Branch B's `cmd/moombox/services_twitch_liveness_test.go`, replacing Step 1's file whole:

```go
package main

import (
	"slices"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// ptrBool is a local helper: ChannelConfig.Enabled is a *bool whose nil means
// enabled.
func ptrBool(b bool) *bool { return &b }

// TestConfiguredTwitchLoginsMirrorTheMonitor pins the one decision the closure
// makes on its own: WHICH logins the tier-2 probe may ask about. The predicate
// is copied from TwitchMonitor.getTwitchChannels (internal/monitor/twitch.go)
// so the probe can only ever ask about a channel the monitor would actually
// poll.
//
// MUTATION CLOSED (platform): using GetPlatform(), which defaults an empty
// platform to "youtube" and would send a YouTube channel's ID to Twitch GQL.
// MUTATION CLOSED (enabled): dropping the disabled skip — a channel the
// operator turned off would still generate traffic every tick.
// MUTATION CLOSED (order): any order but config order.
func TestConfiguredTwitchLoginsMirrorTheMonitor(t *testing.T) {
	for _, tc := range []struct {
		name     string
		channels []config.ChannelConfig
		want     []string
	}{
		{"no channels at all", nil, nil},
		{
			"youtube only",
			[]config.ChannelConfig{{ID: "UC-something", Platform: "youtube"}},
			nil,
		},
		{
			"platform absent is not twitch",
			[]config.ChannelConfig{{ID: "UC-something"}},
			nil,
		},
		{
			"twitch entries in config order",
			[]config.ChannelConfig{
				{ID: "UC-something", Platform: "youtube"},
				{ID: "first_login", Platform: "twitch"},
				{ID: "second_login", Platform: "twitch"},
			},
			[]string{"first_login", "second_login"},
		},
		{
			"a disabled twitch channel is skipped",
			[]config.ChannelConfig{
				{ID: "disabled_login", Platform: "twitch", Enabled: ptrBool(false)},
				{ID: "enabled_login", Platform: "twitch"},
			},
			[]string{"enabled_login"},
		},
		{
			"every twitch channel disabled",
			[]config.ChannelConfig{
				{ID: "disabled_login", Platform: "twitch", Enabled: ptrBool(false)},
			},
			nil,
		},
		{
			"an explicit enabled=true is honoured",
			[]config.ChannelConfig{
				{ID: "explicit_login", Platform: "twitch", Enabled: ptrBool(true)},
			},
			[]string{"explicit_login"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := configuredTwitchLogins(&config.MoomboxConfig{Channels: tc.channels})
			if !slices.Equal(got, tc.want) {
				t.Errorf("configuredTwitchLogins = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestConfiguredTwitchLoginsAreStable: repeated calls on one config must
// return the same order, so the live-channel pick that follows them targets
// the same login tick after tick. A future refactor that indexed channels by
// ID would make it wander silently, because Go map iteration is random.
//
// MUTATION CLOSED: any implementation that does not walk cfg.Channels in order.
func TestConfiguredTwitchLoginsAreStable(t *testing.T) {
	cfg := &config.MoomboxConfig{Channels: []config.ChannelConfig{
		{ID: "aaa_login", Platform: "twitch"},
		{ID: "bbb_login", Platform: "twitch"},
		{ID: "ccc_login", Platform: "twitch"},
	}}
	want := []string{"aaa_login", "bbb_login", "ccc_login"}
	for range 20 {
		if got := configuredTwitchLogins(cfg); !slices.Equal(got, want) {
			t.Fatalf("configuredTwitchLogins = %q, want %q on every call", got, want)
		}
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -count=1 -v -run TestPickTwitchProbeChannel ./cmd/moombox/` (branch A) or `go test -count=1 -v -run TestConfiguredTwitchLogins ./cmd/moombox/` (branch B).
Expected: PASS.

- [ ] **Step 5: Wire the closure**

In `cmd/moombox/services.go`, immediately after the closing `}` of the `cookieRefresh.FallbackLiveness` assignment (`:838`):

**BRANCH A:**

```go
	// The Twitch twin, injected for the same reason plus one more:
	// internal/twitch imports internal/cookies (service.go, auth.go), so the
	// call cannot be made from the refresh service in either direction. This
	// file imports both. It exists because oauth2/validate returns 200 for a
	// token that is valid but no longer entitled to authenticated playback,
	// while the playback access token says which session it was minted for.
	//
	// The config is read on EVERY call, never captured here: an operator who
	// adds their first Twitch channel gets a working probe on the next tick
	// with no restart, and one who removes the last gets an inconclusive probe
	// rather than a request against a stale login.
	//
	// The three answers collapse to two, and the collapse is the point: no
	// configured channel, a transport failure, a rate limit and a 401/403 that
	// may be an edge block are all "we learned nothing" and must report
	// conclusive=false so the refresh service moves no state. The reason is
	// logged here, at the one place that still holds it, at Debug because it
	// recurs every cycle for as long as the obstruction lasts — the
	// once-per-change Info line belongs to the refresh service, which owns the
	// dedupe. ProbeSessionLiveness synthesises its error, so nothing written
	// here can carry a response body, an Authorization header or a token.
	cookieRefresh.TwitchFallbackLiveness = func(ctx context.Context) (bool, bool) {
		var login string
		s.configStore.Read(func(c *config.MoomboxConfig) {
			login = pickTwitchProbeChannel(c)
		})

		probeCtx, cancel := context.WithTimeout(ctx, twitchLivenessProbeTimeout)
		defer cancel()

		signedIn, conclusive, err := twService.ProbeSessionLiveness(probeCtx, login)
		if !conclusive {
			log.Debug("tier-2 twitch liveness probe did not answer", "channel", login, "err", err)
		}
		return signedIn, conclusive
	}
```

**BRANCH B** — identical except the picker step, which becomes:

```go
		var logins []string
		s.configStore.Read(func(c *config.MoomboxConfig) {
			logins = configuredTwitchLogins(c)
		})

		probeCtx, cancel := context.WithTimeout(ctx, twitchLivenessProbeTimeout)
		defer cancel()

		// The probe asks a LIVE channel — the only case Arc 10's measurement
		// covers; see firstLiveTwitchLogin. Same batched request the monitor
		// makes; no new state.
		login := firstLiveTwitchLogin(probeCtx, twService, logins)
```

and the log line gains the reason for an empty login:

```go
		if login == "" {
			log.Debug("tier-2 twitch liveness probe has no live configured channel to ask about", "configured", len(logins))
			return false, false
		}
```

- [ ] **Step 6: Verify the build and the wiring compiles**

Run: `go build ./... && go vet ./...`
Expected: clean. If `context` or `time` is reported unused/undefined in `services.go`, add the import — both are already imported at HEAD, so no import change is expected.

- [ ] **Step 7: Flip `docs/spec/data-and-storage.md:838`**

Not only the last sentence: the paragraph's "fed by two YouTube producers" and "reaching the method means \"YouTube told us\"" become false at the same moment. Replace the WHOLE paragraph at `:838` (it begins `**Two-tier liveness, and its pilot is DISARMED.**` and ends `Twitch has no tier-2 producer.`) with the following, keeping exactly one of the two bracketed clauses — the branch taken — and dropping the brackets:

> **Two-tier liveness, and its pilot is DISARMED.** Tier 1 is the auth check above. Tier 2 is `ObserveLiveness(platform, loggedIn)`, fed by three producers — two YouTube: the per-channel membership probe (once per configured channel per feed cycle) and the channel-independent `FallbackLiveness` probe injected by `cmd/moombox` (this package cannot import `internal/youtube`) — and one Twitch, below. Callers must filter their own inconclusive results out; reaching the method means "the platform told us", not "we asked". Twitch's producer is the channel-independent `TwitchFallbackLiveness` probe, injected by `cmd/moombox` for the same reason and one more — `internal/twitch` imports `internal/cookies`, so the call is an import cycle in either direction. It asks `internal/twitch.Service.ProbeSessionLiveness` for a playback access token on one enabled configured Twitch channel [BRANCH A: the first in config order] [BRANCH B: the first LIVE one, found with the monitor's own batched stream-info query over the first 30 configured logins — two GQL requests per tick, and inconclusive while every configured channel is offline] and reads `user_id` out of the token document (`PlaybackTokenSession`) — the question `checkTwitchAuth` cannot answer, because `oauth2/validate` returns 200 for a token that is valid but no longer entitled to authenticated playback. It runs under the YouTube twin's three conditions plus one: the jar must hold an `auth-token` right now (`HasTwitchAuthCookies`, the narrow predicate), because the probe SENDS that token and an install without it would get an anonymous playback token by design. A 401/403 is INCONCLUSIVE, not signed-out — `gqlRequest` raises `ErrTwitchAuthExpired` for both statuses and 403 is an edge block as often as a credential verdict. Nothing on this path writes `AuthStatus`; Arc 10's capture-time mark (`NoteTwitchAuthLoss`) remains the only direct writer outside `doRefresh`'s own status block.

Gate: `grep -n "two YouTube producers\|YouTube told us\|no tier-2 producer" docs/spec/data-and-storage.md` prints nothing.

- [ ] **Step 8: Add the probe row to `docs/spec/platform-services.md` § Twitch Authentication**

Insert a new paragraph after the `RefreshService.checkTwitchAuth` paragraph (currently at `:491`) and before the "There is no in-process Twitch keepalive" paragraph:

> **The tier-2 entitlement probe.** `Service.ProbeSessionLiveness` (`internal/twitch/liveness_probe.go`) answers the question validate cannot: it requests the stream playback access token for one channel — the same `API.GetStreamAccessToken` call `GetHLSMasterPlaylist` makes, with no playlist fetched afterwards — and classifies the token document with `PlaybackTokenSession`. `user_id` present is signed in, JSON null is anonymous, anything else is inconclusive. It returns `(signedIn, conclusive bool, err error)` and NEVER the token: the error is synthesised rather than passed through, because `gqlRequest` interpolates the response body into its 4xx and auth errors and an intermediary's error page can echo the `Authorization` header — the same body `validateErrorDetail` clamps for above. Two guards refuse before any request goes out: no `auth-token` in the jar (a cookieless install gets an anonymous token by design and must never be told its credentials failed) and no usable channel login (`safeLoginRe`, the same rule the query builder applies). The token is read ONCE and used for both the request and the guard, the discipline `GetHLSMasterPlaylist` documents. `cmd/moombox` wires it to `cookies.RefreshService.TwitchFallbackLiveness` under a 20-second timeout, reading the configured channel list live on every call; see `data-and-storage.md` § Refresh Service for the conditions and the disarmed pilot gate.

The unmeasured sentence at `:531` — "which of the two a real token expiry produces has not been measured" — stays as it is. Task 0 measured whether an OFFLINE channel answers, not what an expired token produces; field-test gate 18 still owns that.

- [ ] **Step 9: Run the whole-tree gates**

```bash
go build ./...
go vet ./...
GOOS=linux GOARCH=amd64 go build ./...
gofmt -l internal/ cmd/
go test -count=1 ./...
```

- [ ] **Step 10: Commit**

```bash
git add cmd/moombox/services.go cmd/moombox/services_twitch_liveness_test.go docs/spec/data-and-storage.md docs/spec/platform-services.md
git commit -m "$(cat <<'EOF'
feat(cmd): wire the Twitch tier-2 probe, and Twitch stops having no producer

The closure that joins internal/twitch's ProbeSessionLiveness to the refresh
service's TwitchFallbackLiveness seam — built here because internal/twitch
imports internal/cookies, so neither package can call the other. The channel is
the first ENABLED configured Twitch login, read live on every call so a config
change lands on the next tick; the predicate is TwitchMonitor's, raw
`ch.Platform` and all, so the probe can only target a channel the monitor polls.

20-second timeout: doRefresh's context is unbounded and gqlRequest's retry
ladder can spend two minutes on the ticker goroutine for an answer the probe is
allowed not to get.

data-and-storage.md's "Twitch has no tier-2 producer" is now false and says so;
platform-services.md gains the probe. The ":531" unmeasured sentence stays —
Task 0 measured the offline channel, not an expired token.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

Under branch B, the first paragraph's "The channel is the first ENABLED configured Twitch login" reads instead: "The channel is the first LIVE one among the enabled configured Twitch logins, found with the monitor's own GetStreamInfoBatch — two GQL requests per tick — branch B, by Task 0's measurement or by default because Task 0 was not run".

---

## Task 4: Record the field gate this arc opened

**Files:**
- Modify: `docs/superpowers/plans/2026-08-29-cookie-remediation-field-test-plan.md` (Part 4 table, append after row 21)
- Modify: `docs/superpowers/plans/2026-08-25-cookie-subsystem-remediation.md:2013` (the **Twitch liveness** bullet)

**Interfaces:**
- Consumes: the behaviour Tasks 1-3 shipped and Task 0's recorded FINDING.
- Produces: nothing code reads.

**Design notes.**

This is not a docs-tidying task and it does not belong folded into Task 3. Everything in Tasks 1-3 is offline evidence — unit tests, mutations and a gated measurement — and the arc's only real-world claim is one nobody has watched: that the probe returns a CONCLUSIVE verdict against a real Twitch session on a real install, on the periodic path, once per 30-minute cycle. Writing that gate's Setup / Closes-when / Failed-looks-like columns is its own design work, and a reviewer can reject the gate's wording while approving the code.

Read the Part 4 table before editing: the rows are numbered and the last one present is 21 (Arc 11 re-auth ingest, `:180` at `16afaa6`). Append row 22 after it, matching the SIX-column shape exactly (`# | Gate (opened by) | What it proves | Setup | Closes when you observe | Failed looks like`). Part 5's smoke-test table has its own numbering and already carries rows 21-27; the new row belongs to Part 4 only.

- [ ] **Step 1: Read the table's tail so the new row matches**

Run: `grep -n "^| 21 | Arc 11" docs/superpowers/plans/2026-08-29-cookie-remediation-field-test-plan.md`
Expected: one line (`:180`). A bare `^| 21 ` also hits Part 5's row 21 (`:213`, the TUI wizard countdown) — not the table being edited. The new row goes immediately after `:180`, before the "Not listed:" paragraph.

- [ ] **Step 2: Append row 22**

```markdown
| 22 | Twitch tier-2 entitlement probe answering on a real session (Arc 12b, this plan's Tasks 1-3) | That `TwitchFallbackLiveness` returns a CONCLUSIVE verdict against a real Twitch session on the periodic path — the arc's only claim no offline test can reach. Everything else is unit-tested and mutated; nobody has watched this probe answer | Real install with a Twitch `auth-token` in `cookies.txt` and at least one enabled `platform = "twitch"` channel configured. Leave it running through at least two 30-minute `RefreshService` cycles at `DEBUG` (Twitch has no cheaper producer to make the probe stand down and its own stamp ages out inside one cadence, so EVERY periodic tick pays for one probe; under branch B at least one configured channel must be LIVE during the window, or every tick is inconclusive by design). Do NOT press Recheck Cookies or `R C` — `CheckNow` deliberately skips the probe | An Info line `liveness observation` with `platform=twitch`, `loggedIn=true`, `wouldFireRecovery=false`, `armed=false`, once per process for a healthy session (repeats drop to Debug). No `tier-2 twitch liveness probe did not answer` line, and no `liveness fallback probe learned nothing about this session platform=twitch` | `loggedIn=false` while Twitch downloads and authenticated chat still work — a false verdict, and the reason the pilot is disarmed; report it rather than arming. Or `tier-2 twitch liveness probe did not answer` on every cycle: read its `err` — `not attempted` means the jar or channel gate refused (check the `auth-token` row and the `platform = "twitch"` spelling), `twitch refused the credentials` means a 401/403 the arc deliberately treats as inconclusive (that is field gate 18's question, not this one), `did not say which session` means Twitch renamed `user_id` and `PlaybackTokenSession` needs re-measuring |
```

- [ ] **Step 3: Rewrite the remediation plan's Twitch-liveness bullet**

Replace the whole of `docs/superpowers/plans/2026-08-25-cookie-subsystem-remediation.md:2013` — the bullet beginning `- **Twitch liveness** — ` — with:

```markdown
- **Twitch liveness** — `oauth2/validate` returns 200 for an unentitled token. The equivalent probe is a playback-access-token request. **BUILT — Arc 12b** (`docs/superpowers/plans/2026-09-02-arc12b-twitch-entitlement-probe.md`): `internal/twitch.Service.ProbeSessionLiveness` over the existing `GetStreamAccessToken`, classified by Arc 10's `PlaybackTokenSession`; `RefreshService.TwitchFallbackLiveness`, a twin of the YouTube seam called from `doRefresh` under the same conditions plus the narrow `HasTwitchAuthCookies` jar gate; the closure in `cmd/moombox/services.go` beside the YouTube one, because `internal/twitch` imports `internal/cookies` and the reverse import is impossible. `checkTwitchAuth` is unchanged and remains the tier-1 signal; Arc 10's capture-time mark is unchanged and remains the only direct `AuthStatus` writer outside `doRefresh`. The pilot stays DISARMED — the probe produces observations and inherits `livenessRecoveryArmed = false`. `data-and-storage.md` §Refresh Service's "Twitch has no tier-2 producer" is flipped; `platform-services.md` §Twitch Authentication gains the probe; field-test gate 22 is the open field claim.
```

- [ ] **Step 4: Verify no other doc still says Twitch has no tier-2 producer**

Run: `grep -rn "no tier-2 producer\|no tier 2 producer" docs/ SPEC.md`
Expected: no matches. If `SPEC.md` carries the claim in its Cookies section, fix it here in the same commit — an absence claim that survives its refutation is the specific failure this project's absence-claim rule exists to prevent.

- [ ] **Step 5: Run the whole-tree gates**

```bash
go build ./...
go vet ./...
GOOS=linux GOARCH=amd64 go build ./...
gofmt -l internal/ cmd/
go test -count=1 ./...
```

- [ ] **Step 6: Commit**

```bash
git add docs/superpowers/plans/2026-08-29-cookie-remediation-field-test-plan.md docs/superpowers/plans/2026-08-25-cookie-subsystem-remediation.md
git commit -m "$(cat <<'EOF'
docs(plans): Arc 12b's one open field claim, and the deferred bullet it closes

Everything Tasks 1-3 shipped is offline evidence. The claim nobody has watched
is that the probe returns a conclusive verdict against a real Twitch session on
the periodic path — field-test gate 22, with the three inconclusive reasons
spelled out so a failed run is diagnosable from one log line.

The remediation plan's Twitch-liveness bullet moves from SCHEDULED to BUILT and
names what did and did not change.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

---

## Self-Review

**1. Spec coverage.**

| Spec item | Task |
|---|---|
| R1 — one `internal/twitch` function over `GetStreamAccessToken`, answering from `PlaybackTokenSession`, never logging or returning the token | Task 1 |
| R2 — first configured Twitch channel; no channel → inconclusive, said once at Info; Task 0 confirms the offline premise; branch B is the default when Task 0 is unrun | Task 3 (`pickTwitchProbeChannel` / `configuredTwitchLogins` + `firstLiveTwitchLogin`, live config read), Task 2 (`recordInconclusiveLiveness` dedupe), Task 0, "The one thing that decides Task 3" |
| R3 — `TwitchFallbackLiveness` twin, same call site conditions, `ObserveLiveness("twitch", …)`, no `AuthStatus` write, no new mechanism | Task 2 |
| R4 — one GQL request per tick, only with a token, existing tick is the throttle, no new timer | Task 2 (the `hasTwitchToken` gate, no timer) + Task 3 (branch B costs two, and says so) |
| R5 — `data-and-storage.md:838` flips; `platform-services.md` § Twitch gains the probe row; the field-test plan gains a row | Task 3 (spec docs), Task 4 (field-test row + remediation bullet) |
| Non-goals — no arming, no `checkTwitchAuth` change, no Arc 10 mark change, no unconfigured-channel probing, no per-channel entitlement check | Global Constraints + Task 2 Step 9 test `TestTwitchFallbackWritesNoAuthStatus`; nothing in any task touches `checkTwitchAuth` or `NoteTwitchAuthLoss` |
| Invariants — no reverse import, no token in a log/error/payload, inline recover, anonymous logger, mutation-checked assertions | Global Constraints; Task 1 Steps 1/5, Task 2 Step 7 |

No gap found.

**2. Placeholder scan.** Every code step carries the actual code. The only conditional content is Task 3's branch A / branch B, and BOTH are written out in full — production code AND the test file for each — rather than described; the branch is a finding from Task 0 (or the ruled default, B) and not a decision deferred to the implementer.

**3. Type consistency.** `ProbeSessionLiveness(ctx, channelLogin) (signedIn, conclusive bool, err error)` is declared in Task 1's Interfaces, implemented in Task 1 Step 3, and called in Task 3 Step 5 with the same three results. `ErrLivenessProbeNotAttempted` is declared in Task 1 and asserted in Task 1's tests and named in Task 4's gate-22 failure column. `TwitchFallbackLiveness func(ctx context.Context) (loggedIn, conclusive bool)` is declared in Task 2 Step 3 and assigned in Task 3 Step 5 with a matching signature. `pickTwitchProbeChannel(*config.MoomboxConfig) string` is used identically in Task 3's test and implementation. `twitchLivenessProbeTimeout` is defined once (Task 3 Step 3) and used once (Step 5), and branch B keeps it unchanged.

**Where the code contradicted the design draft — followed the code, flagged here:**

- **The draft's `refresh.go:1630-1669` / `:673` / `:938` / `:748` anchors all hold** at `8c9f9da`; `internal/twitch/api.go:689 GetStreamAccessToken` and `playback_token.go:41 PlaybackTokenSession` hold; `cmd/moombox/services.go:831-838` and `:508` hold. `checkTwitchAuth` is at `:3349`, matching the file map, not the remediation plan's stale `:3048`.
- **R3 says "only when the jar holds a Twitch `auth-token`" and the draft cites `doRefresh`'s existing gate.** `doRefresh` snapshots `HasAnyTwitchAuthCookie` (auth-token **or** `twilight-user`), which is NOT "holds an auth-token" — a session whose token was pruned on expiry passes it. The plan therefore adds a second snapshot, `hasTwitchToken = rs.jar.HasTwitchAuthCookies()` (`jar.go:878`), and gates on that. R3's words are honoured; the existing variable is not the one it describes.
- **R2's "says so once at Info" has no home in `internal/cookies` as written.** The refresh service cannot know whether a channel is configured — the closure can. The plan routes it through the EXISTING inconclusive arm: the closure returns `(false, false)` and logs the reason at Debug (mirroring the YouTube closure), and `recordInconclusiveLiveness("twitch")` provides the once-per-process Info line. No new dedupe was built.
- **R1's "transport error → inconclusive" collides with a plausible reading of `ErrTwitchAuthExpired`.** A 401/403 on a request that CARRIED our token looks like a dead-token verdict, but `gqlRequest` raises that sentinel for 403 too, and `platform-services.md:531` records that which failure a real expiry produces is unmeasured. The plan follows R1 literally — inconclusive — and wraps the sentinel so field gate 18's measurement can still find the arm. This is a deliberate conservatism, not an oversight.
- **R1's "never returns the token value" needed more than a return-type argument.** `gqlRequest` interpolates `string(respData)` into its 4xx and auth errors, and the closure logs that error into a stream that reaches the Web UI and TUI. The draft does not mention it. Task 1 synthesises every returned error and Task 1 Step 5 proves it by reverting.
- **"The existing monitor state" (the task brief's branch-B suggestion) does not exist.** `TwitchMonitor` retains no live-channel set, and `database.Job` carries `ChannelName` (a display name) rather than a login, so there is nothing to join a running job back to a configured channel. Branch B therefore re-issues `GetStreamInfoBatch` — the monitor's own batched query, adding no state — and the plan records why, in the "The one thing that decides Task 3" section and in `firstLiveTwitchLogin`'s comment.
- **Arc 10 already answered half of Task 0.** `playback_token.go`'s comment records that the Arc 10 live probe confirmed `user_id` discriminates, with `subscriber=false` beside it. Task 0 is narrowed to the part that is genuinely unmeasured — whether an OFFLINE channel answers at all — rather than re-running a measurement the tree already carries.

---

## Final state (arc close, 2026-09-03)

Branch `cookie-arc12b-twitch-entitlement-probe`, cut from `main` @ `ef5a918`; the spec and this plan landed as `fb75fbf`, Task 0 as `779fff2`, Task 1 as `836b185` + fix round 1 `de82e2c`, Task 2 as `5211ce2`, Task 3 as `1f9b58b`, Task 4 as `5faa479`. The Fable arc-close review of `ef5a918..5faa479` is `.superpowers/sdd/2026-09-02-arc12b-twitch-entitlement-probe/arc12b-arc-close-review.md` (gitignored beside the ledger `progress.md`); it traced R1-R4 as ONE chain across `internal/cookies` → `cmd/moombox` → `internal/twitch` by opening the code, traced every error the probe can return back to the wire, checked every doc sentence the tasks changed against HEAD, and ran eight mutations in a `%TEMP%` worktree (six caught; two survived — one an equivalent mutant whose test comment overclaims, one a test-precision gap; both carried to `arc12b-close-fix-items.md`, the gap with a test shape proven against its mutant). The commit that adds this section also fixes the two doc sentences its body names.

### The branch Task 0 decided

Task 0 was RUN — once, on 2026-09-02, against one signed-in session and one offline configured channel — and the authenticated playback token for an OFFLINE channel decoded and carried a numeric `user_id`; the anonymous control carried `null`. FINDING: **BRANCH A.** Task 3 therefore built `pickTwitchProbeChannel` — the first enabled `platform = "twitch"` channel in config order — and the probe costs ONE GQL request per tick, R4 as written. The plan review's branch-B default (F6) never applied; no branch-B code exists in the tree (`configuredTwitchLogins`, `firstLiveTwitchLogin`, `twitchLivenessProbeBatch`: zero hits outside Task 0's own FINDING strings). Task 0's three env vars stayed unset in every later run, and the test skips.

### What each task delivered

| Task | Commit | Delivered | Review |
|---|---|---|---|
| 0 | `779fff2` | `TestLiveOfflineChannelPlaybackToken` (`internal/twitch/playback_token_offline_live_test.go`): three env vars, the premise check first (`GetStreamInfo` nil = offline), the FINDING logged BEFORE `describePlaybackToken`'s shape Fatal (F8); prints field names, JSON kinds and booleans, `%T` on every error, never a value | `task-0-review.md`: PASS — the skip live-verified with no vars set; leak trace clean |
| 1 | `836b185` + `de82e2c` | `Service.ProbeSessionLiveness(ctx, login) (signedIn, conclusive bool, err error)` and `ErrLivenessProbeNotAttempted` (`internal/twitch/liveness_probe.go`): the token read ONCE; two guards before any request (no `auth-token`; no usable login, by `safeLoginRe`); the same `API.GetStreamAccessToken` call `GetHLSMasterPlaylist` makes, no playlist; the 401/403 arm re-wraps ONLY `ErrTwitchAuthExpired`, every other error is `%T`; the verdict from `PlaybackTokenSession`; four tests — the verdict rule, both guards with a request counter, the auth refusal, the leak barrier | `task-1-review.md`: PASS / PASS WITH REQUIRED EDITS — the generic error arm's booleans were discarded (`_, _, err :=`) so `conclusive=true` there survived; fix round 1 (test-only) asserts the pair; re-review ADDRESSED, both arms caught |
| 2 | `5211ce2` | `RefreshService.TwitchFallbackLiveness` beside `FallbackLiveness`; `hasTwitchToken = rs.jar.HasTwitchAuthCookies()` in the locked snapshot (the NARROW predicate); the Twitch block after the YouTube one under `allowFallback && != nil && hasTwitchToken && !livenessObservedRecently("twitch")` — `ObserveLiveness("twitch", …)` on conclusive, `recordInconclusiveLiveness("twitch")` otherwise; the three producer-counting comments in `ObserveLiveness` rewritten (F3); eight tests incl. `jarWithTwilightUserOnly`, the only jar that tells the narrow gate from the broad one (F1) | `task-2-review.md` (sonnet, after the opus tier answered 529 three times): PASS / PASS — lock table (a)-(d) holds, `-race` 8/8, 6/6 mutants; the "delete the block" mutant does not compile (`hasTwitchToken` unused) and was run as `if false && …` |
| 3 | `1f9b58b` | `pickTwitchProbeChannel` (the monitor's predicate, RAW `Platform`, first match), `twitchLivenessProbeTimeout = 20 s`, the closure in `initServices` directly after the YouTube one — config read live on every call, the Debug line carries the configured login and the synthesised error; two picker tests; `data-and-storage.md:838` replaced whole (F4); `platform-services.md:493` gains the probe paragraph; `:533` ("has not been measured") untouched | `task-3-review.md`: PASS / PASS WITH REQUIRED EDITS — the `MUTATION CLOSED (platform)` comment names a mutant no fixture can catch (equivalent for `!= "twitch"`); 3/3 catchable mutants caught |
| 4 | `5faa479` | field-test plan Part 4 row 22 (six columns, after row 21); the remediation plan's **Twitch liveness** bullet SCHEDULED → BUILT | `task-4-review.md`: PASS |

### Deviations from the plan as written

1. **Task 3 took BRANCH A, not the ruled default B.** Task 0 measured the premise the day the plan was committed; the ledger's ruling (2026-09-02 23:31) records why the measurement supersedes F6's default and what it costs if Twitch changes the offline token's shape (the probe reads inconclusive — an absent key — never a false negative).
2. **Task 2's "delete the whole Twitch block" mutant cannot compile** (`hasTwitchToken` becomes `declared and not used`); it was run as `if false && …`, which is behaviourally the same deletion. The review reproduced the compile failure and judged the substitute sound.
3. **Task 2 ran on sonnet, not opus.** The opus tier answered 529 Overloaded twice for the implementer and once for the reviewer; the sonnet review carried an explicit lock-discipline table and a `-race` run in its place, and this arc close re-read the seam.
4. **Task 1 needed a fix round** — the leak test discarded `signedIn`/`conclusive` on the generic error arm; `de82e2c` asserts them (test-only).
5. **Task 3's `MUTATION CLOSED (platform)` comment overclaims, and `pickTwitchProbeChannel`'s own doc comment gives the same false mechanism.** `GetPlatform()` maps `""` to `"youtube"`, which fails `!= "twitch"` exactly as `""` does, so reading through it could NOT "let a YouTube channel with no explicit platform be sent to Twitch GQL"; the raw field is right for parity with `getTwitchChannels`, and that is the only true reason. Both comments are carried to the fix items (comment-only; the code is correct).
6. **Task 4's row 22 carried a branch-B clause for a branch that was never built** ("under branch B at least one configured channel must be LIVE …") and cited "this plan's Tasks 1-3" from a document that has no Tasks 1-3. Both fixed by the commit that adds this section: the row now says the probe asks about the first enabled configured channel whether or not it is live, and names this plan.
7. **The remediation plan's `:1739` still said "Twitch has no tier-2 signal at all (`ObserveLiveness` has two producers, both YouTube)".** Task 4's absence-claim grep was `no tier-2 producer`, which does not match "no tier-2 signal". Rewritten by the same commit as history ("had … until Arc 12b").
8. **Arc close: one mutant survives every test** — `recordInconclusiveLiveness("youtube")` in the Twitch inconclusive arm. `TestTwitchFallbackUsesTheTwitchPlatformKey` closes the freshness gate and the `ObserveLiveness` call (both caught) but not the third platform-string site; the dedupe works under either key, so the Info/Debug counts are identical. Test shape proven against the mutant, carried to the fix items.

### Residuals, each with a home

| Residual | Behaviour | Home |
|---|---|---|
| the offline-channel premise is measured ONCE | one session, one channel, one day; if Twitch stops carrying `user_id` on an offline channel's token the probe reads inconclusive (absent key), never signed-out | row 22's "did not say which session" arm; `PlaybackTokenSession`'s comment; Task 0's test, re-runnable |
| branch B's second GQL request per tick | not applicable — branch A built; ONE request per periodic tick, none on `CheckNow` or the startup check | Task 3 Step 3-ALT stays in this plan as the fallback form; R4 |
| the probe is inert for RECOVERY until arming | `livenessRecoveryArmed = false`: the observation, the dedupe, the freshness stamp and the log line land; `OnRecoveryNeeded` never fires | `data-and-storage.md` § Refresh Service; `TestTwitchFallbackWritesNoAuthStatus`; the remediation plan's ARMING items |
| every periodic tick pays for one probe | `livenessFreshWindow` (25 min) is shorter than the default cadence (30 min) and Twitch has no per-channel producer to make the fallback stand down | row 22's Setup; the `livenessObservedRecently` comment |
| the closure body has no unit test | `pickTwitchProbeChannel` is pinned; the config read, the 20 s timeout and the Debug line are exercised only in the field, as the YouTube closure's are | row 22 — the arc's only field claim |
| which failure a real token expiry produces is unmeasured | 401/403 (`ErrTwitchAuthExpired`) or an anonymous token; the probe treats the former as inconclusive and the latter as signed-out | `platform-services.md:533`; field gate 18 |
| `api.go:196` logs a 5xx/429 body at Debug for every GQL caller | pre-existing; the arc added a caller, not the hazard | worklist (plan review F12) |
| the seam's `hasTwitchToken` and the probe's own `GetAuthToken()` are two reads | a jar reload between them makes the probe's guard the authority: `ErrLivenessProbeNotAttempted` at Debug, `(false, false)` | `ProbeSessionLiveness`'s first guard; the field comment on `TwitchFallbackLiveness` |
| the configured login appears in the closure's Debug line | a public channel login the operator typed; never a token, body or header | the closure's comment; the leak table in the arc-close review |
| the full-suite timing flake | `TestTightenCookieDirOncePermanentFailureCostIsOnePerWrite` hit the 10-min timeout once under whole-tree load (Task 1), passed alone and on retry; the whole `internal/cookies` package passed in 32 s at arc close | the ledger (2026-09-03 07:23); the post-merge gate re-runs the suite |
| three comment/test-precision items | the M1b assertion; the two `GetPlatform()` comments; Task 0's FINDING strings naming `configuredTwitchLogins`/`firstLiveTwitchLogin`, which were never built | `arc12b-close-fix-items.md` |

### The field gate

Row 22 of `2026-08-29-cookie-remediation-field-test-plan.md` Part 4: a conclusive `liveness observation platform=twitch loggedIn=true wouldFireRecovery=false armed=false` on a real session, on the periodic path, through two 30-minute cycles at `DEBUG`. Arming stays the owner's.
